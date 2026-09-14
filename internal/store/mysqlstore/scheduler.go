package mysqlstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/celanwang/rc_celanwang/internal/domain"
)

var ErrLeaseLost = errors.New("notification lease lost")

func (s *Store) ReadyTargets(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT target_id FROM notification
        WHERE status = 'pending' AND next_attempt_at <= NOW(6) AND expires_at > NOW(6)
        GROUP BY target_id ORDER BY MIN(next_attempt_at), target_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func (s *Store) Claim(ctx context.Context, targetID string, lease time.Duration) (domain.Notification, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return domain.Notification{}, false, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM notification
        WHERE target_id = ? AND status = 'pending' AND next_attempt_at <= NOW(6) AND expires_at > NOW(6)
        ORDER BY next_attempt_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, targetID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Notification{}, false, nil
	}
	if err != nil {
		return domain.Notification{}, false, err
	}
	token := newToken()
	micros := lease.Microseconds()
	result, err := tx.ExecContext(ctx, `UPDATE notification SET status='processing', lease_token=?,
        lease_until=DATE_ADD(NOW(6), INTERVAL ? MICROSECOND), run_attempt_count=run_attempt_count+1,
        last_attempt_no=last_attempt_no+1 WHERE id=? AND status='pending'`, token, micros, id)
	if err != nil {
		return domain.Notification{}, false, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return domain.Notification{}, false, ErrStateConflict
	}
	n, err := scanNotification(tx.QueryRowContext(ctx, notificationSelect+" WHERE id = ?", id))
	if err != nil {
		return domain.Notification{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO notification_attempt
        (notification_id, run_no, attempt_no, started_at, result)
        VALUES (?, ?, ?, NOW(6), 'in_progress')`, n.ID, n.RunNo, n.LastAttemptNo)
	if err != nil {
		return domain.Notification{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Notification{}, false, err
	}
	return n, true, nil
}

type DeliveryResult struct {
	Result           string
	HTTPStatus       *int
	ErrorCode        string
	Phase            string
	DeliveryEvidence string
	Duration         time.Duration
	ResponseSummary  string
}

type Completion struct {
	Status        domain.Status
	NextAttemptAt time.Time
	StopReason    string
	RetryAfter    *time.Time
}

func (s *Store) Complete(ctx context.Context, n domain.Notification, result DeliveryResult, completion Completion) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	status := completion.Status
	var next any = completion.NextAttemptAt.UTC()
	stopReason := completion.StopReason
	terminal := false
	switch status {
	case domain.StatusSucceeded:
		next = n.NextAttemptAt.UTC()
		terminal = true
	case domain.StatusFailed:
		next = n.NextAttemptAt.UTC()
		terminal = true
	case domain.StatusPending:
	default:
		return errors.New("invalid completion status")
	}
	updated, err := tx.ExecContext(ctx, `UPDATE notification SET status=?, next_attempt_at=?, lease_token=NULL,
		lease_until=NULL, last_http_status=?, last_error_code=?, last_error=?, stop_reason=?,
		finished_at=CASE WHEN ? THEN NOW(6) ELSE NULL END
		WHERE id=? AND status='processing' AND lease_token=?`, status, next, nullableInt(result.HTTPStatus),
		result.ErrorCode, safeError(result), stopReason, terminal, n.ID, n.LeaseToken)
	if err != nil {
		return err
	}
	if rows, _ := updated.RowsAffected(); rows != 1 {
		return ErrLeaseLost
	}
	attemptUpdated, err := tx.ExecContext(ctx, `UPDATE notification_attempt SET finished_at=NOW(6), result=?, http_status=?,
        error_code=?, phase=?, delivery_evidence=?, retry_after=?, duration_ms=?, response_summary=?
        WHERE notification_id=? AND attempt_no=? AND result='in_progress'`, result.Result, nullableInt(result.HTTPStatus),
		result.ErrorCode, result.Phase, result.DeliveryEvidence, completion.RetryAfter, result.Duration.Milliseconds(),
		result.ResponseSummary, n.ID, n.LastAttemptNo)
	if err != nil {
		return err
	}
	if rows, _ := attemptUpdated.RowsAffected(); rows != 1 {
		return ErrLeaseLost
	}
	return tx.Commit()
}

func (s *Store) Recover(ctx context.Context, limit int) (int, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, last_attempt_no, run_attempt_count, max_attempts, expires_at <= NOW(6)
        FROM notification WHERE status='processing' AND lease_until <= NOW(6)
        ORDER BY lease_until, id LIMIT ? FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return 0, err
	}
	type expired struct {
		id                          string
		attempt, count, maxAttempts int
		expired                     bool
	}
	var items []expired
	for rows.Next() {
		var item expired
		if err := rows.Scan(&item.id, &item.attempt, &item.count, &item.maxAttempts, &item.expired); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, item := range items {
		status, stop := domain.StatusPending, ""
		terminal := false
		if item.count >= item.maxAttempts {
			status, stop, terminal = domain.StatusFailed, "RETRY_EXHAUSTED", true
		} else if item.expired {
			status, stop, terminal = domain.StatusFailed, "EXPIRED", true
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notification_attempt SET finished_at=NOW(6),
            result='recovered_unknown', error_code='lease_expired', phase='recovery', delivery_evidence='unknown'
            WHERE notification_id=? AND attempt_no=? AND result='in_progress'`, item.id, item.attempt); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notification SET status=?, next_attempt_at=NOW(6),
            lease_token=NULL, lease_until=NULL, last_error_code='lease_expired', last_error='execution lease expired',
			stop_reason=?, finished_at=CASE WHEN ? THEN NOW(6) ELSE NULL END WHERE id=? AND status='processing'`, status, stop, terminal, item.id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(items), nil
}

func (s *Store) ExpirePending(ctx context.Context, limit int) (int64, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE notification SET status='failed', stop_reason='EXPIRED',
        finished_at=NOW(6), last_error_code='expired', last_error='notification validity expired'
        WHERE status='pending' AND expires_at <= NOW(6) LIMIT ?`, limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) Cleanup(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM notification
        WHERE status IN ('succeeded','failed') AND finished_at < DATE_SUB(NOW(6), INTERVAL ? MICROSECOND)
        ORDER BY finished_at, id LIMIT ?`, retention.Microseconds(), limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) Replay(ctx context.Context, callerID, operator, id, reason string, expectedRun int, expiresAt time.Time) (domain.Notification, error) {
	if reason == "" || len(reason) > 500 || !expiresAt.After(time.Now().UTC()) {
		return domain.Notification{}, ErrStateConflict
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return domain.Notification{}, err
	}
	defer tx.Rollback()
	n, err := scanNotification(tx.QueryRowContext(ctx, notificationSelect+" WHERE caller_id=? AND id=? FOR UPDATE", callerID, id))
	if err != nil {
		return domain.Notification{}, err
	}
	if n.Status != domain.StatusFailed || n.RunNo != expectedRun {
		return domain.Notification{}, ErrStateConflict
	}
	n.RunNo++
	n.ReplayHistory = append(n.ReplayHistory, domain.Replay{Operator: operator, Reason: reason, RunNo: n.RunNo, ExpiresAt: expiresAt.UTC(), At: time.Now().UTC()})
	history, err := json.Marshal(n.ReplayHistory)
	if err != nil {
		return domain.Notification{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE notification SET status='pending', run_no=?, run_attempt_count=0,
        next_attempt_at=NOW(6), expires_at=?, stop_reason='', last_error_code='', last_error='', last_http_status=NULL,
        finished_at=NULL, replay_history=? WHERE id=? AND caller_id=? AND status='failed' AND run_no=?`,
		n.RunNo, expiresAt.UTC(), history, id, callerID, expectedRun)
	if err != nil {
		return domain.Notification{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return domain.Notification{}, ErrStateConflict
	}
	if err := tx.Commit(); err != nil {
		return domain.Notification{}, err
	}
	return s.Get(ctx, callerID, id)
}

func newToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	v := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", v[:8], v[8:12], v[12:16], v[16:20], v[20:])
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func safeError(result DeliveryResult) string {
	if result.ErrorCode == "" {
		return ""
	}
	return result.ErrorCode
}
