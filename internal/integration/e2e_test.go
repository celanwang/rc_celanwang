package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/celanwang/rc_celanwang/internal/api"
	"github.com/celanwang/rc_celanwang/internal/config"
	"github.com/celanwang/rc_celanwang/internal/delivery"
	"github.com/celanwang/rc_celanwang/internal/notification"
	"github.com/celanwang/rc_celanwang/internal/store/mysqlstore"
	"github.com/celanwang/rc_celanwang/internal/worker"
	_ "github.com/go-sql-driver/mysql"
)

func TestNotificationLifecycle(t *testing.T) {
	dsn := os.Getenv("NOTIFIER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("NOTIFIER_TEST_MYSQL_DSN is not set")
	}
	var repaired atomic.Bool
	var observedAuth, observedBody atomic.Value
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedAuth.Store(r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		observedBody.Store(string(body))
		switch r.URL.Path {
		case "/business-error":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"success":false}`))
		case "/accepted":
			w.WriteHeader(http.StatusAccepted)
		case "/bad":
			if repaired.Load() {
				w.WriteHeader(http.StatusNoContent)
			} else {
				w.WriteHeader(http.StatusBadRequest)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer vendor.Close()
	cfg := integrationConfig(t, dsn, vendor.URL)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := mysqlstore.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM notification_attempt"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM notification"); err != nil {
		t.Fatal(err)
	}
	store := mysqlstore.New(db)
	deliverer, err := delivery.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer deliverer.CloseIdleConnections()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := notification.NewService(store, cfg)
	apiServer := httptest.NewServer(api.New(service, cfg, store.Ping, logger).Handler())
	defer apiServer.Close()
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	manager := worker.New(store, deliverer, cfg, logger)
	go manager.Run(workerCtx)
	defer func() {
		cancelWorkers()
		waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Wait(waitCtx)
	}()

	businessID := submit(t, apiServer.URL, "business", vendor.URL+"/business-error", http.StatusAccepted)
	acceptedID := submit(t, apiServer.URL, "accepted", vendor.URL+"/accepted", http.StatusAccepted)
	failedID := submit(t, apiServer.URL, "bad", vendor.URL+"/bad", http.StatusAccepted)
	submit(t, apiServer.URL, "business", vendor.URL+"/business-error", http.StatusOK)
	submit(t, apiServer.URL, "business", vendor.URL+"/accepted", http.StatusConflict)
	submitWithTarget(t, apiServer.URL, "forbidden", "unknown", vendor.URL+"/accepted", http.StatusForbidden)

	waitStatus(t, apiServer.URL, businessID, "succeeded", 1)
	waitStatus(t, apiServer.URL, acceptedID, "succeeded", 1)
	waitStatus(t, apiServer.URL, failedID, "failed", 1)
	if observedAuth.Load() != "Bearer supplier-secret" || observedBody.Load() != `{"event":"registered"}` {
		t.Fatalf("supplier request mismatch: auth=%v body=%v", observedAuth.Load(), observedBody.Load())
	}
	attempts := getJSON(t, apiServer.URL+"/v1/notifications/"+businessID+"/attempts", http.StatusOK)
	if len(attempts["items"].([]any)) != 1 {
		t.Fatalf("unexpected attempts: %+v", attempts)
	}

	repaired.Store(true)
	replayBody := fmt.Sprintf(`{"expected_run":1,"reason":"supplier configuration repaired","expires_at":%q}`, time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano))
	req, _ := http.NewRequest(http.MethodPost, apiServer.URL+"/v1/notifications/"+failedID+"/replays", strings.NewReader(replayBody))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("replay status=%d", resp.StatusCode)
	}
	waitStatus(t, apiServer.URL, failedID, "succeeded", 2)
	afterReplay := getJSON(t, apiServer.URL+"/v1/notifications/"+failedID+"/attempts", http.StatusOK)
	if len(afterReplay["items"].([]any)) != 2 {
		t.Fatalf("replay did not preserve attempts: %+v", afterReplay)
	}
}

func integrationConfig(t *testing.T, dsn, vendorURL string) config.Config {
	t.Helper()
	u, err := url.Parse(vendorURL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	target := config.Target{ID: "vendor", Callers: []string{"caller"}, Hosts: []string{u.Hostname()}, Ports: []int{port}, AllowPrivate: true, MaxConcurrency: 2, Credential: map[string]string{"Authorization": "Bearer supplier-secret"}}
	return config.Config{
		MySQLDSN: dsn, APIKeys: map[string]config.Principal{"user-secret": {CallerID: "caller"}, "admin-secret": {CallerID: "caller", Admin: true}},
		Targets: map[string]config.Target{"vendor": target}, Workers: 2, ScanInterval: 10 * time.Millisecond,
		LeaseDuration: time.Second, RequestTimeout: 200 * time.Millisecond, ConnectTimeout: 100 * time.Millisecond,
		TLSTimeout: 100 * time.Millisecond, ResponseHeaderTimeout: 150 * time.Millisecond, MaxBodyBytes: 1024,
		MaxEnvelopeBytes: 2048, MaxResponseBodyBytes: 128, MaxAttempts: 2, Validity: time.Minute,
		RetryBase: 10 * time.Millisecond, RetryMax: 20 * time.Millisecond, RetryMin: time.Millisecond, BacklogLimit: 100,
		TerminalRetention: time.Hour, CleanupInterval: time.Hour,
	}
}

func submit(t *testing.T, serverURL, key, targetURL string, want int) string {
	t.Helper()
	return submitWithTarget(t, serverURL, key, "vendor", targetURL, want)
}

func submitWithTarget(t *testing.T, serverURL, key, target, targetURL string, want int) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"target_id": target, "url": targetURL, "method": "POST", "headers": map[string][]string{"Content-Type": {"application/json"}}, "body": `{"event":"registered"}`})
	req, _ := http.NewRequest(http.MethodPost, serverURL+"/v1/notifications", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer user-secret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("submit %s got %d want %d body=%s", key, resp.StatusCode, want, data)
	}
	if want != http.StatusAccepted && want != http.StatusOK {
		return ""
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value["id"].(string)
}

func waitStatus(t *testing.T, serverURL, id, status string, run int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		value := getJSON(t, serverURL+"/v1/notifications/"+id, http.StatusOK)
		if value["status"] == status && int(value["run_no"].(float64)) == run {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("notification %s did not reach %s run %d", id, status, run)
}

func getJSON(t *testing.T, endpoint string, want int) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, endpoint, nil)
	req.Header.Set("Authorization", "Bearer user-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s got %d body=%s", endpoint, resp.StatusCode, data)
	}
	var value map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
