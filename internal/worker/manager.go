package worker

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/celanwang/rc_celanwang/internal/config"
	"github.com/celanwang/rc_celanwang/internal/delivery"
	"github.com/celanwang/rc_celanwang/internal/domain"
	"github.com/celanwang/rc_celanwang/internal/retry"
	"github.com/celanwang/rc_celanwang/internal/store/mysqlstore"
)

type Store interface {
	ReadyTargets(context.Context, int) ([]string, error)
	Claim(context.Context, string, time.Duration) (domain.Notification, bool, error)
	Complete(context.Context, domain.Notification, mysqlstore.DeliveryResult, mysqlstore.Completion) error
	Recover(context.Context, int) (int, error)
	ExpirePending(context.Context, int) (int64, error)
	Cleanup(context.Context, time.Duration, int) (int64, error)
	Now(context.Context) (time.Time, error)
}

type Deliverer interface {
	Deliver(context.Context, domain.Notification) delivery.Result
}

type Manager struct {
	store        Store
	deliverer    Deliverer
	policy       retry.Policy
	logger       *slog.Logger
	lease        time.Duration
	scanInterval time.Duration
	global       chan struct{}
	targets      map[string]chan struct{}
	wg           sync.WaitGroup
	mu           sync.Mutex
	nextTarget   int
	lastCleanup  time.Time
	retention    time.Duration
	cleanupEvery time.Duration
	stopped      chan struct{}
}

func New(store Store, deliverer Deliverer, cfg config.Config, logger *slog.Logger) *Manager {
	targets := make(map[string]chan struct{}, len(cfg.Targets))
	for id, target := range cfg.Targets {
		targets[id] = make(chan struct{}, target.MaxConcurrency)
	}
	return &Manager{
		store: store, deliverer: deliverer, logger: logger, lease: cfg.LeaseDuration,
		scanInterval: cfg.ScanInterval, global: make(chan struct{}, cfg.Workers), targets: targets,
		policy:    retry.Policy{Base: cfg.RetryBase, Max: cfg.RetryMax, Min: cfg.RetryMin, Rand: rand.Float64},
		retention: cfg.TerminalRetention, cleanupEvery: cfg.CleanupInterval,
		stopped: make(chan struct{}),
	}
}

func (m *Manager) Run(ctx context.Context) {
	defer close(m.stopped)
	ticker := time.NewTicker(m.scanInterval)
	defer ticker.Stop()
	m.scan(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.scan(ctx)
		}
	}
}

func (m *Manager) Wait(ctx context.Context) error {
	select {
	case <-m.stopped:
	case <-ctx.Done():
		return ctx.Err()
	}
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) scan(ctx context.Context) {
	if m.lastCleanup.IsZero() || time.Since(m.lastCleanup) >= m.cleanupEvery {
		if count, err := m.store.Cleanup(ctx, m.retention, 500); err != nil {
			m.logger.Error("terminal cleanup failed", "error_type", "database")
		} else {
			m.lastCleanup = time.Now()
			if count > 0 {
				m.logger.Info("terminal notifications cleaned", "count", count)
			}
		}
	}
	if recovered, err := m.store.Recover(ctx, cap(m.global)); err != nil {
		m.logger.Error("lease recovery failed", "error_type", "database")
	} else if recovered > 0 {
		m.logger.Warn("expired leases recovered", "count", recovered)
	}
	if _, err := m.store.ExpirePending(ctx, cap(m.global)); err != nil {
		m.logger.Error("pending expiry failed", "error_type", "database")
	}
	available := cap(m.global) - len(m.global)
	if available <= 0 {
		return
	}
	targetIDs, err := m.store.ReadyTargets(ctx, len(m.targets))
	if err != nil {
		m.logger.Error("ready target scan failed", "error_type", "database")
		return
	}
	targetIDs = m.rotate(targetIDs)
	for available > 0 && len(targetIDs) > 0 {
		claimedAny := false
		for _, targetID := range targetIDs {
			if available == 0 {
				break
			}
			targetSlots, ok := m.targets[targetID]
			if !ok || !tryAcquire(targetSlots) {
				continue
			}
			if !tryAcquire(m.global) {
				release(targetSlots)
				return
			}
			n, ok, err := m.store.Claim(ctx, targetID, m.lease)
			if err != nil || !ok {
				release(m.global)
				release(targetSlots)
				if err != nil {
					m.logger.Error("notification claim failed", "target_id", targetID, "error_type", "database")
				}
				continue
			}
			available--
			claimedAny = true
			m.wg.Add(1)
			go m.execute(n, targetSlots)
		}
		if !claimedAny {
			break
		}
	}
}

func (m *Manager) execute(n domain.Notification, targetSlots chan struct{}) {
	defer m.wg.Done()
	defer release(m.global)
	defer release(targetSlots)
	result := m.deliverer.Deliver(context.Background(), n)
	now, err := m.store.Now(context.Background())
	if err != nil {
		m.logger.Error("database clock unavailable after delivery", "notification_id", n.ID, "attempt_no", n.LastAttemptNo, "error_type", "database")
		return
	}
	decision := m.policy.Decide(n, result, now)
	completion := mysqlstore.Completion{NextAttemptAt: decision.NextAttemptAt, StopReason: decision.StopReason, RetryAfter: decision.RetryAfter}
	switch decision.Action {
	case retry.ActionSucceed:
		completion.Status = domain.StatusSucceeded
	case retry.ActionRetry:
		completion.Status = domain.StatusPending
	case retry.ActionStop:
		completion.Status = domain.StatusFailed
	}
	storeResult := mysqlstore.DeliveryResult{
		Result: result.Result, HTTPStatus: result.HTTPStatus, ErrorCode: result.ErrorCode, Phase: result.Phase,
		DeliveryEvidence: result.DeliveryEvidence, Duration: result.Duration, ResponseSummary: result.ResponseSummary,
	}
	if err := m.store.Complete(context.Background(), n, storeResult, completion); err != nil {
		if errors.Is(err, mysqlstore.ErrLeaseLost) {
			m.logger.Warn("stale delivery result rejected", "notification_id", n.ID, "attempt_no", n.LastAttemptNo, "target_id", n.TargetID)
		} else {
			m.logger.Error("delivery result write failed", "notification_id", n.ID, "attempt_no", n.LastAttemptNo, "target_id", n.TargetID, "error_type", "database")
		}
		return
	}
	m.logger.Info("delivery completed", "notification_id", n.ID, "attempt_no", n.LastAttemptNo, "run_no", n.RunNo, "target_id", n.TargetID, "status", completion.Status, "error_code", result.ErrorCode)
}

func (m *Manager) rotate(values []string) []string {
	if len(values) < 2 {
		return values
	}
	m.mu.Lock()
	start := m.nextTarget % len(values)
	m.nextTarget = (m.nextTarget + 1) % len(values)
	m.mu.Unlock()
	rotated := append([]string(nil), values[start:]...)
	return append(rotated, values[:start]...)
}

func tryAcquire(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func release(slots chan struct{}) { <-slots }
