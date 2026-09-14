package retry

import (
	"net/http"
	"testing"
	"time"

	"github.com/celanwang/rc_celanwang/internal/delivery"
	"github.com/celanwang/rc_celanwang/internal/domain"
)

func TestPolicy(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	policy := Policy{Base: 5 * time.Second, Max: time.Minute, Min: time.Second, Rand: func() float64 { return 0.5 }}
	tests := []struct {
		name   string
		n      domain.Notification
		result delivery.Result
		action string
		stop   string
		next   time.Time
	}{
		{"success after expiry", domain.Notification{ExpiresAt: now.Add(-time.Second)}, delivery.Result{Result: "http_success"}, ActionSucceed, "", time.Time{}},
		{"permanent 400", task(now, 1, 3), delivery.Result{Result: "http_failure", HTTPStatus: intPtr(http.StatusBadRequest)}, ActionStop, "PERMANENT_ERROR", time.Time{}},
		{"retry 503", task(now, 1, 3), delivery.Result{Result: "http_failure", HTTPStatus: intPtr(http.StatusServiceUnavailable), Retryable: true}, ActionRetry, "", now.Add(2500 * time.Millisecond)},
		{"attempt budget", task(now, 3, 3), delivery.Result{Result: "transport_error", Retryable: true}, ActionStop, "RETRY_EXHAUSTED", time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := policy.Decide(tc.n, tc.result, now)
			if got.Action != tc.action || got.StopReason != tc.stop || !got.NextAttemptAt.Equal(tc.next) {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestRetryAfterControlsScheduleAndExpiry(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	policy := Policy{Base: time.Second, Max: time.Minute, Min: time.Second, Rand: func() float64 { return 0 }}
	result := delivery.Result{Result: "http_failure", Retryable: true, RetryAfter: "30"}
	got := policy.Decide(task(now, 1, 3), result, now)
	if got.Action != ActionRetry || !got.NextAttemptAt.Equal(now.Add(30*time.Second)) {
		t.Fatalf("got %+v", got)
	}
	n := task(now, 1, 3)
	n.ExpiresAt = now.Add(20 * time.Second)
	got = policy.Decide(n, result, now)
	if got.Action != ActionStop || got.StopReason != "EXPIRED" {
		t.Fatalf("got %+v", got)
	}
}

func task(now time.Time, count, maxAttempts int) domain.Notification {
	return domain.Notification{RunAttemptCount: count, MaxAttempts: maxAttempts, ExpiresAt: now.Add(time.Hour)}
}

func intPtr(v int) *int { return &v }
