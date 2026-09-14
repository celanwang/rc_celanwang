package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/celanwang/rc_celanwang/internal/config"
	"github.com/celanwang/rc_celanwang/internal/domain"
	"github.com/celanwang/rc_celanwang/internal/notification"
	"github.com/celanwang/rc_celanwang/internal/store/mysqlstore"
)

type Service interface {
	Create(context.Context, string, string, domain.CreateRequest) (domain.Notification, bool, error)
	Get(context.Context, string, string) (domain.Notification, error)
	List(context.Context, string, mysqlstore.ListFilter) ([]domain.Notification, error)
	ListAttempts(context.Context, string, string, int, int) ([]domain.Attempt, error)
	Replay(context.Context, string, string, string, string, int, time.Time) (domain.Notification, error)
}

type Server struct {
	handler  http.Handler
	service  Service
	apiKeys  map[string]config.Principal
	maxBytes int64
	ready    func(context.Context) error
	logger   *slog.Logger
}

type principalKey struct{}
type requestIDKey struct{}

func New(service Service, cfg config.Config, ready func(context.Context) error, logger *slog.Logger) *Server {
	s := &Server{service: service, apiKeys: cfg.APIKeys, maxBytes: cfg.MaxEnvelopeBytes, ready: ready, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", s.live)
	mux.HandleFunc("GET /health/ready", s.readiness)
	mux.Handle("POST /v1/notifications", s.auth(http.HandlerFunc(s.create)))
	mux.Handle("GET /v1/notifications", s.auth(http.HandlerFunc(s.list)))
	mux.Handle("GET /v1/notifications/{id}", s.auth(http.HandlerFunc(s.get)))
	mux.Handle("GET /v1/notifications/{id}/attempts", s.auth(http.HandlerFunc(s.attempts)))
	mux.Handle("POST /v1/notifications/{id}/replays", s.auth(http.HandlerFunc(s.replay)))
	s.handler = s.requestContext(mux)
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) requestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID()
		w.Header().Set("X-Request-Id", requestID)
		ctx := context.WithValue(r.Context(), requestIDKey{}, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			s.writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "valid bearer token required")
			return
		}
		principal, ok := s.apiKeys[strings.TrimPrefix(auth, "Bearer ")]
		if !ok {
			s.writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "valid bearer token required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal)))
	})
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		s.writeError(w, r, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var req domain.CreateRequest
	if err := decoder.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, r, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "request exceeds size limit")
			return
		}
		s.writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON request")
		return
	}
	if err := ensureEOF(decoder); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "request must contain one JSON object")
		return
	}
	principal := principalFrom(r)
	n, created, err := s.service.Create(r.Context(), principal.CallerID, r.Header.Get("Idempotency-Key"), req)
	if err != nil {
		s.mapServiceError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	s.writeJSON(w, status, notificationView(n))
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	n, err := s.service.Get(r.Context(), principalFrom(r).CallerID, r.PathValue("id"))
	if err != nil {
		s.mapServiceError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, notificationView(n))
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	filter := mysqlstore.ListFilter{Limit: queryInt(r, "limit", 50, 1, 100), Offset: queryInt(r, "offset", 0, 0, 1000000)}
	if raw := r.URL.Query().Get("status"); raw != "" {
		filter.Status = domain.Status(raw)
		if !filter.Status.Valid() {
			s.writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid status filter")
			return
		}
	}
	filter.TargetID = r.URL.Query().Get("target_id")
	items, err := s.service.List(r.Context(), principalFrom(r).CallerID, filter)
	if err != nil {
		s.mapServiceError(w, r, err)
		return
	}
	views := make([]any, 0, len(items))
	for _, item := range items {
		views = append(views, notificationView(item))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": views, "limit": filter.Limit, "offset": filter.Offset})
}

func (s *Server) attempts(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50, 1, 100)
	offset := queryInt(r, "offset", 0, 0, 1000000)
	items, err := s.service.ListAttempts(r.Context(), principalFrom(r).CallerID, r.PathValue("id"), limit, offset)
	if err != nil {
		s.mapServiceError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items, "limit": limit, "offset": offset})
}

func (s *Server) replay(w http.ResponseWriter, r *http.Request) {
	principal := principalFrom(r)
	if !principal.Admin {
		s.writeError(w, r, http.StatusForbidden, "FORBIDDEN", "administrator permission required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var req struct {
		ExpectedRun int       `json:"expected_run"`
		Reason      string    `json:"reason"`
		ExpiresAt   time.Time `json:"expires_at"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || ensureEOF(decoder) != nil {
		s.writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid replay request")
		return
	}
	n, err := s.service.Replay(r.Context(), principal.CallerID, principal.CallerID, r.PathValue("id"), strings.TrimSpace(req.Reason), req.ExpectedRun, req.ExpiresAt)
	if err != nil {
		if errors.Is(err, mysqlstore.ErrStateConflict) {
			s.writeError(w, r, http.StatusConflict, "STATE_CONFLICT", "notification cannot be replayed from the expected run")
			return
		}
		s.mapServiceError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, notificationView(n))
}

func (s *Server) live(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.ready(ctx); err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "database unavailable")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) mapServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, notification.ErrInvalidRequest):
		s.writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	case errors.Is(err, notification.ErrTargetNotAllowed):
		s.writeError(w, r, http.StatusForbidden, "TARGET_NOT_ALLOWED", "target is not allowed")
	case notification.IsIdempotencyConflict(err):
		s.writeError(w, r, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "idempotency key has different content")
	case notification.IsNotFound(err):
		s.writeError(w, r, http.StatusNotFound, "NOTIFICATION_NOT_FOUND", "notification not found")
	case errors.Is(err, notification.ErrBacklogFull):
		s.writeError(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "notification backlog is full")
	default:
		s.logger.Error("request failed", "request_id", requestIDFrom(r), "error_type", "internal")
		s.writeError(w, r, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "service cannot reliably accept the request")
	}
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	s.writeJSON(w, status, map[string]string{"request_id": requestIDFrom(r), "code": code, "message": message})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func principalFrom(r *http.Request) config.Principal {
	return r.Context().Value(principalKey{}).(config.Principal)
}

func requestIDFrom(r *http.Request) string {
	value, _ := r.Context().Value(requestIDKey{}).(string)
	return value
}

func newRequestID() string {
	var value [12]byte
	_, _ = rand.Read(value[:])
	return hex.EncodeToString(value[:])
}

func queryInt(r *http.Request, name string, fallback, minValue, maxValue int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minValue || value > maxValue {
		return fallback
	}
	return value
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return errors.New("trailing JSON content")
}

func notificationView(n domain.Notification) map[string]any {
	return map[string]any{
		"id": n.ID, "target_id": n.TargetID, "method": n.Method, "status": n.Status,
		"run_no": n.RunNo, "run_attempt_count": n.RunAttemptCount, "last_attempt_no": n.LastAttemptNo,
		"max_attempts": n.MaxAttempts, "next_attempt_at": n.NextAttemptAt, "expires_at": n.ExpiresAt,
		"last_http_status": n.LastHTTPStatus, "last_error_code": n.LastErrorCode, "last_error": n.LastError,
		"stop_reason": n.StopReason, "replay_history": n.ReplayHistory, "created_at": n.CreatedAt,
		"updated_at": n.UpdatedAt, "finished_at": n.FinishedAt,
	}
}
