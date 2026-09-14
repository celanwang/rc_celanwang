package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/celanwang/rc_celanwang/internal/config"
	"github.com/celanwang/rc_celanwang/internal/domain"
	"github.com/celanwang/rc_celanwang/internal/store/mysqlstore"
)

type stubService struct {
	created bool
	err     error
}

func (s stubService) Create(context.Context, string, string, domain.CreateRequest) (domain.Notification, bool, error) {
	return domain.Notification{ID: "task", Status: domain.StatusPending}, s.created, s.err
}
func (stubService) Get(context.Context, string, string) (domain.Notification, error) {
	return domain.Notification{ID: "task", Status: domain.StatusPending}, nil
}
func (stubService) List(context.Context, string, mysqlstore.ListFilter) ([]domain.Notification, error) {
	return nil, nil
}
func (stubService) ListAttempts(context.Context, string, string, int, int) ([]domain.Attempt, error) {
	return nil, nil
}
func (stubService) Replay(context.Context, string, string, string, string, int, time.Time) (domain.Notification, error) {
	return domain.Notification{}, nil
}
func (stubService) Metrics(context.Context) (mysqlstore.Metrics, error) {
	return mysqlstore.Metrics{}, nil
}

func TestCreateAuthenticationAndPersistenceResponse(t *testing.T) {
	tests := []struct {
		name       string
		auth       string
		service    stubService
		wantStatus int
	}{
		{"missing auth", "", stubService{}, http.StatusUnauthorized},
		{"database unavailable", "Bearer secret", stubService{err: errors.New("database down")}, http.StatusServiceUnavailable},
		{"new notification", "Bearer secret", stubService{created: true}, http.StatusAccepted},
		{"idempotent notification", "Bearer secret", stubService{created: false}, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{APIKeys: map[string]config.Principal{"secret": {CallerID: "caller"}}, MaxEnvelopeBytes: 1024}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			server := New(tc.service, cfg, func(context.Context) error { return nil }, logger)
			req := httptest.NewRequest(http.MethodPost, "/v1/notifications", strings.NewReader(`{"target_id":"t","url":"http://example.com","method":"POST"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", tc.auth)
			req.Header.Set("Idempotency-Key", "key")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, req)
			if response.Code != tc.wantStatus {
				t.Fatalf("got %d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestEnvelopeLimit(t *testing.T) {
	cfg := config.Config{APIKeys: map[string]config.Principal{"secret": {CallerID: "caller"}}, MaxEnvelopeBytes: 32}
	server := New(stubService{}, cfg, func(context.Context) error { return nil }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/v1/notifications", strings.NewReader(`{"target_id":"target","url":"http://example.com","method":"POST","body":"large"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d body=%s", response.Code, response.Body.String())
	}
}
