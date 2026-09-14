package retry

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/celanwang/rc_celanwang/internal/delivery"
	"github.com/celanwang/rc_celanwang/internal/domain"
)

const (
	ActionSucceed = "succeed"
	ActionRetry   = "retry"
	ActionStop    = "stop"
)

type Decision struct {
	Action        string
	NextAttemptAt time.Time
	StopReason    string
	RetryAfter    *time.Time
}

type Policy struct {
	Base time.Duration
	Max  time.Duration
	Min  time.Duration
	Rand func() float64
}

func (p Policy) Decide(n domain.Notification, result delivery.Result, now time.Time) Decision {
	if result.Result == "http_success" {
		return Decision{Action: ActionSucceed}
	}
	if !result.Retryable {
		return Decision{Action: ActionStop, StopReason: "PERMANENT_ERROR"}
	}
	if n.RunAttemptCount >= n.MaxAttempts {
		return Decision{Action: ActionStop, StopReason: "RETRY_EXHAUSTED"}
	}
	if !now.Before(n.ExpiresAt) {
		return Decision{Action: ActionStop, StopReason: "EXPIRED"}
	}
	capDuration := p.Base
	for i := 1; i < n.RunAttemptCount && capDuration < p.Max; i++ {
		if capDuration > p.Max/2 {
			capDuration = p.Max
		} else {
			capDuration *= 2
		}
	}
	capDuration = min(capDuration, p.Max)
	random := 0.5
	if p.Rand != nil {
		random = math.Max(0, math.Min(1, p.Rand()))
	}
	wait := time.Duration(float64(capDuration) * random)
	wait = max(wait, p.Min)
	next := now.Add(wait)
	var retryAfter *time.Time
	if parsed, ok := ParseRetryAfter(result.RetryAfter, now); ok {
		retryAfter = &parsed
		if parsed.After(next) {
			next = parsed
		}
	}
	if next.After(n.ExpiresAt) {
		return Decision{Action: ActionStop, StopReason: "EXPIRED", RetryAfter: retryAfter}
	}
	return Decision{Action: ActionRetry, NextAttemptAt: next, RetryAfter: retryAfter}
}

func ParseRetryAfter(value string, now time.Time) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds >= 0 {
		return now.Add(time.Duration(seconds) * time.Second), true
	}
	parsed, err := http.ParseTime(value)
	if err != nil || parsed.Before(now) {
		return time.Time{}, false
	}
	return parsed, true
}
