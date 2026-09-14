package mysqlstore

import (
	"context"
	"database/sql"
)

type AttemptMetric struct {
	TargetID string
	Result   string
	Count    int64
}

type Metrics struct {
	StatusCounts         map[string]int64
	OldestPendingSeconds float64
	Attempts             []AttemptMetric
}

func (s *Store) Metrics(ctx context.Context) (Metrics, error) {
	out := Metrics{StatusCounts: map[string]int64{"pending": 0, "processing": 0, "succeeded": 0, "failed": 0}}
	rows, err := s.db.QueryContext(ctx, "SELECT status, COUNT(*) FROM notification GROUP BY status")
	if err != nil {
		return Metrics{}, err
	}
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			rows.Close()
			return Metrics{}, err
		}
		out.StatusCounts[status] = count
	}
	if err := rows.Close(); err != nil {
		return Metrics{}, err
	}
	var oldest sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `SELECT TIMESTAMPDIFF(MICROSECOND, MIN(next_attempt_at), NOW(6)) / 1000000
        FROM notification WHERE status='pending' AND next_attempt_at <= NOW(6)`).Scan(&oldest); err != nil {
		return Metrics{}, err
	}
	if oldest.Valid && oldest.Float64 > 0 {
		out.OldestPendingSeconds = oldest.Float64
	}
	rows, err = s.db.QueryContext(ctx, `SELECT n.target_id, a.result, COUNT(*)
        FROM notification_attempt a JOIN notification n ON n.id=a.notification_id
        GROUP BY n.target_id, a.result ORDER BY n.target_id, a.result`)
	if err != nil {
		return Metrics{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var metric AttemptMetric
		if err := rows.Scan(&metric.TargetID, &metric.Result, &metric.Count); err != nil {
			return Metrics{}, err
		}
		out.Attempts = append(out.Attempts, metric)
	}
	return out, rows.Err()
}
