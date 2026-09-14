package domain

import "time"

type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusSucceeded  Status = "succeeded"
	StatusFailed     Status = "failed"
)

func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusProcessing, StatusSucceeded, StatusFailed:
		return true
	default:
		return false
	}
}

type Header map[string][]string

type CreateRequest struct {
	TargetID string `json:"target_id"`
	URL      string `json:"url"`
	Method   string `json:"method"`
	Headers  Header `json:"headers,omitempty"`
	Body     string `json:"body,omitempty"`
}

type Notification struct {
	ID              string     `json:"id"`
	CallerID        string     `json:"caller_id,omitempty"`
	IdempotencyKey  string     `json:"-"`
	RequestHash     []byte     `json:"-"`
	TargetID        string     `json:"target_id"`
	URL             string     `json:"url"`
	Method          string     `json:"method"`
	Headers         Header     `json:"headers,omitempty"`
	Body            []byte     `json:"-"`
	Status          Status     `json:"status"`
	RunNo           int        `json:"run_no"`
	RunAttemptCount int        `json:"run_attempt_count"`
	LastAttemptNo   int        `json:"last_attempt_no"`
	MaxAttempts     int        `json:"max_attempts"`
	NextAttemptAt   time.Time  `json:"next_attempt_at"`
	ExpiresAt       time.Time  `json:"expires_at"`
	LeaseToken      string     `json:"-"`
	LeaseUntil      *time.Time `json:"lease_until,omitempty"`
	LastHTTPStatus  *int       `json:"last_http_status,omitempty"`
	LastErrorCode   string     `json:"last_error_code,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	StopReason      string     `json:"stop_reason,omitempty"`
	ReplayHistory   []Replay   `json:"replay_history,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

type Replay struct {
	Operator  string    `json:"operator"`
	Reason    string    `json:"reason"`
	RunNo     int       `json:"run_no"`
	ExpiresAt time.Time `json:"expires_at"`
	At        time.Time `json:"at"`
}

type Attempt struct {
	ID               int64      `json:"id"`
	NotificationID   string     `json:"notification_id"`
	RunNo            int        `json:"run_no"`
	AttemptNo        int        `json:"attempt_no"`
	StartedAt        time.Time  `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	Result           string     `json:"result"`
	HTTPStatus       *int       `json:"http_status,omitempty"`
	ErrorCode        string     `json:"error_code,omitempty"`
	Phase            string     `json:"phase,omitempty"`
	DeliveryEvidence string     `json:"delivery_evidence,omitempty"`
	RetryAfter       *time.Time `json:"retry_after,omitempty"`
	DurationMS       *int64     `json:"duration_ms,omitempty"`
	ResponseSummary  string     `json:"response_summary,omitempty"`
}
