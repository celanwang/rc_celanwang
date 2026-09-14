package notification

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/celanwang/rc_celanwang/internal/config"
	"github.com/celanwang/rc_celanwang/internal/domain"
	"github.com/celanwang/rc_celanwang/internal/store/mysqlstore"
)

var (
	ErrInvalidRequest   = errors.New("invalid notification request")
	ErrTargetNotAllowed = errors.New("target not allowed")
	ErrBacklogFull      = errors.New("notification backlog limit reached")
)

type Store interface {
	Create(context.Context, domain.Notification) (domain.Notification, bool, error)
	CountActive(context.Context) (int64, error)
	Get(context.Context, string, string) (domain.Notification, error)
	List(context.Context, string, mysqlstore.ListFilter) ([]domain.Notification, error)
	ListAttempts(context.Context, string, string, int, int) ([]domain.Attempt, error)
	Replay(context.Context, string, string, string, string, int, time.Time) (domain.Notification, error)
	GetByIdempotency(context.Context, string, string) (domain.Notification, error)
	Metrics(context.Context) (mysqlstore.Metrics, error)
	Now(context.Context) (time.Time, error)
}

type Service struct {
	store        Store
	targets      map[string]config.Target
	maxBodyBytes int64
	maxAttempts  int
	validity     time.Duration
	backlogLimit int64
}

func NewService(store Store, cfg config.Config) *Service {
	return &Service{store: store, targets: cfg.Targets, maxBodyBytes: cfg.MaxBodyBytes, maxAttempts: cfg.MaxAttempts, validity: cfg.Validity, backlogLimit: cfg.BacklogLimit}
}

func (s *Service) Get(ctx context.Context, callerID, id string) (domain.Notification, error) {
	return s.store.Get(ctx, callerID, id)
}

func (s *Service) List(ctx context.Context, callerID string, filter mysqlstore.ListFilter) ([]domain.Notification, error) {
	return s.store.List(ctx, callerID, filter)
}

func (s *Service) ListAttempts(ctx context.Context, callerID, id string, limit, offset int) ([]domain.Attempt, error) {
	return s.store.ListAttempts(ctx, callerID, id, limit, offset)
}

func (s *Service) Replay(ctx context.Context, callerID, operator, id, reason string, expectedRun int, expiresAt time.Time) (domain.Notification, error) {
	return s.store.Replay(ctx, callerID, operator, id, reason, expectedRun, expiresAt)
}

func (s *Service) Metrics(ctx context.Context) (mysqlstore.Metrics, error) {
	return s.store.Metrics(ctx)
}

func (s *Service) Create(ctx context.Context, callerID, key string, req domain.CreateRequest) (domain.Notification, bool, error) {
	if len(key) < 1 || len(key) > 200 || len(callerID) > 128 {
		return domain.Notification{}, false, fmt.Errorf("%w: invalid idempotency key", ErrInvalidRequest)
	}
	normalized, err := s.validate(callerID, req)
	if err != nil {
		return domain.Notification{}, false, err
	}
	hash := requestHash(normalized)
	count, err := s.store.CountActive(ctx)
	if err != nil {
		return domain.Notification{}, false, err
	}
	if count >= s.backlogLimit {
		existing, findErr := s.store.GetByIdempotency(ctx, callerID, key)
		if findErr == nil {
			if subtle.ConstantTimeCompare(existing.RequestHash, hash) == 1 {
				return existing, false, nil
			}
			return domain.Notification{}, false, mysqlstore.ErrIdempotencyConflict
		}
		if !IsNotFound(findErr) {
			return domain.Notification{}, false, findErr
		}
		return domain.Notification{}, false, ErrBacklogFull
	}
	now, err := s.store.Now(ctx)
	if err != nil {
		return domain.Notification{}, false, err
	}
	n := domain.Notification{
		ID:             newUUID(),
		CallerID:       callerID,
		IdempotencyKey: key,
		RequestHash:    hash,
		TargetID:       normalized.TargetID,
		URL:            normalized.URL,
		Method:         normalized.Method,
		Headers:        normalized.Headers,
		Body:           []byte(normalized.Body),
		Status:         domain.StatusPending,
		RunNo:          1,
		MaxAttempts:    s.maxAttempts,
		NextAttemptAt:  now,
		ExpiresAt:      now.Add(s.validity),
		ReplayHistory:  []domain.Replay{},
	}
	return s.store.Create(ctx, n)
}

func (s *Service) validate(callerID string, req domain.CreateRequest) (domain.CreateRequest, error) {
	target, ok := s.targets[req.TargetID]
	if !ok || !contains(target.Callers, callerID) {
		return domain.CreateRequest{}, ErrTargetNotAllowed
	}
	req.Method = strings.ToUpper(strings.TrimSpace(req.Method))
	if !contains([]string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}, req.Method) {
		return domain.CreateRequest{}, fmt.Errorf("%w: unsupported method", ErrInvalidRequest)
	}
	if int64(len(req.Body)) > s.maxBodyBytes || !utf8.ValidString(req.Body) {
		return domain.CreateRequest{}, fmt.Errorf("%w: body must be bounded UTF-8", ErrInvalidRequest)
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return domain.CreateRequest{}, fmt.Errorf("%w: invalid URL", ErrInvalidRequest)
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return domain.CreateRequest{}, fmt.Errorf("%w: invalid port", ErrInvalidRequest)
		}
	}
	if !containsFold(target.Hosts, u.Hostname()) || !containsInt(target.Ports, port) {
		return domain.CreateRequest{}, ErrTargetNotAllowed
	}
	normalized := make(domain.Header, len(req.Headers))
	headerBytes := 0
	for name, values := range req.Headers {
		canonical := http.CanonicalHeaderKey(strings.TrimSpace(name))
		if canonical == "" || !validHeaderName(canonical) || forbiddenHeader(canonical) {
			return domain.CreateRequest{}, fmt.Errorf("%w: invalid request header", ErrInvalidRequest)
		}
		for _, value := range values {
			if strings.ContainsAny(value, "\r\n") {
				return domain.CreateRequest{}, fmt.Errorf("%w: invalid request header value", ErrInvalidRequest)
			}
			headerBytes += len(canonical) + len(value)
			normalized[canonical] = append(normalized[canonical], value)
		}
	}
	if headerBytes > 16<<10 {
		return domain.CreateRequest{}, fmt.Errorf("%w: headers too large", ErrInvalidRequest)
	}
	req.Headers = normalized
	req.URL = u.String()
	return req, nil
}

func requestHash(req domain.CreateRequest) []byte {
	h := sha256.New()
	writePart(h, req.TargetID)
	writePart(h, req.URL)
	writePart(h, req.Method)
	keys := make([]string, 0, len(req.Headers))
	for key := range req.Headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		writePart(h, key)
		for _, value := range req.Headers[key] {
			writePart(h, value)
		}
	}
	writePart(h, req.Body)
	return h.Sum(nil)
}

type stringWriter interface{ Write([]byte) (int, error) }

func writePart(w stringWriter, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = w.Write(length[:])
	_, _ = w.Write([]byte(value))
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(b[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsFold(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(value, wanted) {
			return true
		}
	}
	return false
}

func containsInt(values []int, wanted int) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func validHeaderName(name string) bool {
	for _, c := range []byte(name) {
		if c <= 32 || c >= 127 || strings.ContainsRune("()<>@,;:\\\"/[]?={}\t", rune(c)) {
			return false
		}
	}
	return true
}

func forbiddenHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "host", "content-length", "connection", "transfer-encoding", "upgrade", "trailer":
		return true
	default:
		return false
	}
}

func IsNotFound(err error) bool { return errors.Is(err, mysqlstore.ErrNotFound) }

func IsIdempotencyConflict(err error) bool { return errors.Is(err, mysqlstore.ErrIdempotencyConflict) }

func HostIsPublic(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast())
}
