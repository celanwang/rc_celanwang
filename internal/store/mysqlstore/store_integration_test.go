package mysqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/celanwang/rc_celanwang/internal/domain"
	_ "github.com/go-sql-driver/mysql"
)

func TestCreateIdempotencyAndConflict(t *testing.T) {
	store, db := integrationStore(t)
	resetTables(t, db)
	base := integrationNotification("00000000-0000-4000-8000-000000000001", "same-key")
	var created atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := base
			n.ID = uuidFor(i + 1)
			_, wasCreated, err := store.Create(context.Background(), n)
			if wasCreated {
				created.Add(1)
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if created.Load() != 1 {
		t.Fatalf("created %d notifications, want 1", created.Load())
	}
	conflict := base
	conflict.ID = uuidFor(99)
	differentHash := sha256.Sum256([]byte("different"))
	conflict.RequestHash = differentHash[:]
	if _, _, err := store.Create(context.Background(), conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("got %v, want idempotency conflict", err)
	}
}

func TestLeaseRecoveryRejectsStaleCompletion(t *testing.T) {
	store, db := integrationStore(t)
	resetTables(t, db)
	n := integrationNotification(uuidFor(1), "lease-key")
	if _, _, err := store.Create(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.Claim(context.Background(), n.TargetID, 30*time.Second)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if _, err := db.Exec("UPDATE notification SET lease_until=DATE_SUB(NOW(6), INTERVAL 1 SECOND) WHERE id=?", n.ID); err != nil {
		t.Fatal(err)
	}
	if count, err := store.Recover(context.Background(), 10); err != nil || count != 1 {
		t.Fatalf("recover: count=%d err=%v", count, err)
	}
	newClaim, ok, err := store.Claim(context.Background(), n.TargetID, 30*time.Second)
	if err != nil || !ok {
		t.Fatalf("replacement claim: ok=%v err=%v", ok, err)
	}
	err = store.Complete(context.Background(), claimed, DeliveryResult{Result: "http_success"}, Completion{Status: domain.StatusSucceeded})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("got %v, want lost lease", err)
	}
	if err := store.Complete(context.Background(), newClaim, DeliveryResult{Result: "http_success", DeliveryEvidence: "http_response"}, Completion{Status: domain.StatusSucceeded}); err != nil {
		t.Fatalf("replacement completion: %v", err)
	}
	got, err := store.Get(context.Background(), n.CallerID, n.ID)
	if err != nil || got.Status != domain.StatusSucceeded {
		t.Fatalf("recovered task: %+v err=%v", got, err)
	}
	attempts, err := store.ListAttempts(context.Background(), n.CallerID, n.ID, 10, 0)
	if err != nil || len(attempts) != 2 || attempts[0].Result != "http_success" || attempts[1].Result != "recovered_unknown" {
		t.Fatalf("attempts: %+v err=%v", attempts, err)
	}
}

func TestCompetingClaimsAreDistinct(t *testing.T) {
	store, db := integrationStore(t)
	resetTables(t, db)
	for i := 1; i <= 2; i++ {
		if _, _, err := store.Create(context.Background(), integrationNotification(uuidFor(i), "key-"+uuidFor(i))); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	ids := make(chan string, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			n, ok, err := store.Claim(context.Background(), "target", 30*time.Second)
			if err == nil && !ok {
				err = errors.New("nothing claimed")
			}
			ids <- n.ID
			errs <- err
		}()
	}
	close(start)
	first, second := <-ids, <-ids
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("both workers claimed %s", first)
	}
}

func integrationStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("NOTIFIER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("NOTIFIER_TEST_MYSQL_DSN is not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return New(db), db
}

func resetTables(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("DELETE FROM notification_attempt"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM notification"); err != nil {
		t.Fatal(err)
	}
}

func integrationNotification(id, key string) domain.Notification {
	now := time.Now().UTC()
	hash := sha256.Sum256([]byte("stable request"))
	return domain.Notification{ID: id, CallerID: "caller", IdempotencyKey: key, RequestHash: hash[:], TargetID: "target", URL: "http://example.com", Method: "POST", Headers: domain.Header{}, Body: []byte("{}"), Status: domain.StatusPending, RunNo: 1, MaxAttempts: 3, NextAttemptAt: now, ExpiresAt: now.Add(time.Hour), ReplayHistory: []domain.Replay{}}
}

func uuidFor(value int) string {
	return "00000000-0000-4000-8000-" + leftPad(value)
}

func leftPad(value int) string {
	text := strconv.Itoa(value)
	return strings.Repeat("0", 12-len(text)) + text
}
