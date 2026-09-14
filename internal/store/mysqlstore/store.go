package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/celanwang/rc_celanwang/internal/domain"
	"github.com/go-sql-driver/mysql"
)

var (
	ErrNotFound            = errors.New("notification not found")
	ErrIdempotencyConflict = errors.New("idempotency key content conflict")
	ErrStateConflict       = errors.New("notification state conflict")
)

type Store struct {
	db *sql.DB
}

func New(db *sql.DB) *Store { return &Store{db: db} }

func Open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetConnMaxLifetime(3 * time.Minute)
	db.SetMaxOpenConns(40)
	db.SetMaxIdleConns(20)
	return db, nil
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) CountActive(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notification WHERE status IN ('pending','processing')").Scan(&count)
	return count, err
}

func (s *Store) Create(ctx context.Context, n domain.Notification) (domain.Notification, bool, error) {
	headers, err := json.Marshal(n.Headers)
	if err != nil {
		return domain.Notification{}, false, err
	}
	history, err := json.Marshal(n.ReplayHistory)
	if err != nil {
		return domain.Notification{}, false, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO notification
        (id, caller_id, idempotency_key, request_hash, target_id, request_url, request_method,
         request_headers, request_body, status, run_no, max_attempts, next_attempt_at, expires_at, replay_history)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', 1, ?, ?, ?, ?)`,
		n.ID, n.CallerID, n.IdempotencyKey, n.RequestHash, n.TargetID, n.URL, n.Method,
		headers, n.Body, n.MaxAttempts, n.NextAttemptAt.UTC(), n.ExpiresAt.UTC(), history)
	if err == nil {
		created, getErr := s.Get(ctx, n.CallerID, n.ID)
		return created, true, getErr
	}
	var mysqlErr *mysql.MySQLError
	if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1062 {
		return domain.Notification{}, false, fmt.Errorf("insert notification: %w", err)
	}
	existing, getErr := s.getByIdempotency(ctx, n.CallerID, n.IdempotencyKey)
	if getErr != nil {
		return domain.Notification{}, false, fmt.Errorf("read concurrent notification: %w", getErr)
	}
	if !equalBytes(existing.RequestHash, n.RequestHash) {
		return domain.Notification{}, false, ErrIdempotencyConflict
	}
	return existing, false, nil
}

func (s *Store) Get(ctx context.Context, callerID, id string) (domain.Notification, error) {
	return scanNotification(s.db.QueryRowContext(ctx, notificationSelect+" WHERE caller_id = ? AND id = ?", callerID, id))
}

func (s *Store) GetAny(ctx context.Context, id string) (domain.Notification, error) {
	return scanNotification(s.db.QueryRowContext(ctx, notificationSelect+" WHERE id = ?", id))
}

func (s *Store) getByIdempotency(ctx context.Context, callerID, key string) (domain.Notification, error) {
	return scanNotification(s.db.QueryRowContext(ctx, notificationSelect+" WHERE caller_id = ? AND idempotency_key = ?", callerID, key))
}

func (s *Store) GetByIdempotency(ctx context.Context, callerID, key string) (domain.Notification, error) {
	return s.getByIdempotency(ctx, callerID, key)
}

type ListFilter struct {
	Status   domain.Status
	TargetID string
	Limit    int
	Offset   int
}

func (s *Store) List(ctx context.Context, callerID string, f ListFilter) ([]domain.Notification, error) {
	query := notificationSelect + " WHERE caller_id = ?"
	args := []any{callerID}
	if f.Status != "" {
		query += " AND status = ?"
		args = append(args, f.Status)
	}
	if f.TargetID != "" {
		query += " AND target_id = ?"
		args = append(args, f.TargetID)
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, f.Limit, f.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) ListAttempts(ctx context.Context, callerID, id string, limit, offset int) ([]domain.Attempt, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, "SELECT 1 FROM notification WHERE caller_id = ? AND id = ?", callerID, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, notification_id, run_no, attempt_no, started_at, finished_at,
        result, http_status, error_code, phase, delivery_evidence, retry_after, duration_ms, response_summary
        FROM notification_attempt WHERE notification_id = ? ORDER BY attempt_no DESC LIMIT ? OFFSET ?`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

const notificationSelect = `SELECT id, caller_id, idempotency_key, request_hash, target_id, request_url,
    request_method, request_headers, request_body, status, run_no, run_attempt_count, last_attempt_no,
    max_attempts, next_attempt_at, expires_at, lease_token, lease_until, last_http_status,
    last_error_code, last_error, stop_reason, replay_history, created_at, updated_at, finished_at
    FROM notification`

type scanner interface{ Scan(...any) error }

func scanNotification(row scanner) (domain.Notification, error) {
	var n domain.Notification
	var headers, history []byte
	var leaseToken sql.NullString
	var lastErrorCode, lastError, stopReason string
	var leaseUntil, finishedAt sql.NullTime
	var httpStatus sql.NullInt64
	err := row.Scan(&n.ID, &n.CallerID, &n.IdempotencyKey, &n.RequestHash, &n.TargetID, &n.URL,
		&n.Method, &headers, &n.Body, &n.Status, &n.RunNo, &n.RunAttemptCount, &n.LastAttemptNo,
		&n.MaxAttempts, &n.NextAttemptAt, &n.ExpiresAt, &leaseToken, &leaseUntil, &httpStatus,
		&lastErrorCode, &lastError, &stopReason, &history, &n.CreatedAt, &n.UpdatedAt, &finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Notification{}, ErrNotFound
	}
	if err != nil {
		return domain.Notification{}, err
	}
	if err := json.Unmarshal(headers, &n.Headers); err != nil {
		return domain.Notification{}, err
	}
	if err := json.Unmarshal(history, &n.ReplayHistory); err != nil {
		return domain.Notification{}, err
	}
	n.LeaseToken, n.LastErrorCode, n.LastError, n.StopReason = leaseToken.String, lastErrorCode, lastError, stopReason
	if leaseUntil.Valid {
		n.LeaseUntil = &leaseUntil.Time
	}
	if finishedAt.Valid {
		n.FinishedAt = &finishedAt.Time
	}
	if httpStatus.Valid {
		v := int(httpStatus.Int64)
		n.LastHTTPStatus = &v
	}
	return n, nil
}

func scanAttempt(row scanner) (domain.Attempt, error) {
	var a domain.Attempt
	var finishedAt, retryAfter sql.NullTime
	var status, duration sql.NullInt64
	if err := row.Scan(&a.ID, &a.NotificationID, &a.RunNo, &a.AttemptNo, &a.StartedAt, &finishedAt,
		&a.Result, &status, &a.ErrorCode, &a.Phase, &a.DeliveryEvidence, &retryAfter, &duration, &a.ResponseSummary); err != nil {
		return domain.Attempt{}, err
	}
	if finishedAt.Valid {
		a.FinishedAt = &finishedAt.Time
	}
	if retryAfter.Valid {
		a.RetryAfter = &retryAfter.Time
	}
	if status.Valid {
		v := int(status.Int64)
		a.HTTPStatus = &v
	}
	if duration.Valid {
		v := duration.Int64
		a.DurationMS = &v
	}
	return a, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
