package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Principal struct {
	CallerID string
	Admin    bool
}

type Target struct {
	ID             string            `json:"id"`
	Callers        []string          `json:"callers"`
	Hosts          []string          `json:"hosts"`
	Ports          []int             `json:"ports"`
	AllowPrivate   bool              `json:"allow_private"`
	MaxConcurrency int               `json:"max_concurrency"`
	Credential     map[string]string `json:"credential_headers,omitempty"`
	CAFile         string            `json:"ca_file,omitempty"`
}

type Config struct {
	Addr                  string
	MySQLDSN              string
	APIKeys               map[string]Principal
	Targets               map[string]Target
	Workers               int
	ScanInterval          time.Duration
	LeaseDuration         time.Duration
	ShutdownTimeout       time.Duration
	RequestTimeout        time.Duration
	ConnectTimeout        time.Duration
	TLSTimeout            time.Duration
	ResponseHeaderTimeout time.Duration
	MaxBodyBytes          int64
	MaxEnvelopeBytes      int64
	MaxResponseBodyBytes  int64
	MaxAttempts           int
	Validity              time.Duration
	RetryBase             time.Duration
	RetryMax              time.Duration
	RetryMin              time.Duration
	BacklogLimit          int64
	TerminalRetention     time.Duration
	CleanupInterval       time.Duration
}

func Load() (Config, error) {
	c := Config{
		Addr:                  env("NOTIFIER_ADDR", ":8080"),
		MySQLDSN:              os.Getenv("NOTIFIER_MYSQL_DSN"),
		Workers:               envInt("NOTIFIER_WORKERS", 20),
		ScanInterval:          envDuration("NOTIFIER_SCAN_INTERVAL", time.Second),
		LeaseDuration:         envDuration("NOTIFIER_LEASE_DURATION", 30*time.Second),
		ShutdownTimeout:       envDuration("NOTIFIER_SHUTDOWN_TIMEOUT", 12*time.Second),
		RequestTimeout:        envDuration("NOTIFIER_REQUEST_TIMEOUT", 10*time.Second),
		ConnectTimeout:        envDuration("NOTIFIER_CONNECT_TIMEOUT", 3*time.Second),
		TLSTimeout:            envDuration("NOTIFIER_TLS_TIMEOUT", 3*time.Second),
		ResponseHeaderTimeout: envDuration("NOTIFIER_RESPONSE_HEADER_TIMEOUT", 5*time.Second),
		MaxBodyBytes:          envInt64("NOTIFIER_MAX_BODY_BYTES", 1<<20),
		MaxEnvelopeBytes:      envInt64("NOTIFIER_MAX_ENVELOPE_BYTES", (1<<20)+(64<<10)),
		MaxResponseBodyBytes:  envInt64("NOTIFIER_MAX_RESPONSE_BODY_BYTES", 16<<10),
		MaxAttempts:           envInt("NOTIFIER_MAX_ATTEMPTS", 20),
		Validity:              envDuration("NOTIFIER_VALIDITY", 24*time.Hour),
		RetryBase:             envDuration("NOTIFIER_RETRY_BASE", 5*time.Second),
		RetryMax:              envDuration("NOTIFIER_RETRY_MAX", time.Hour),
		RetryMin:              envDuration("NOTIFIER_RETRY_MIN", time.Second),
		BacklogLimit:          envInt64("NOTIFIER_BACKLOG_LIMIT", 100000),
		TerminalRetention:     envDuration("NOTIFIER_TERMINAL_RETENTION", 30*24*time.Hour),
		CleanupInterval:       envDuration("NOTIFIER_CLEANUP_INTERVAL", time.Hour),
	}
	var err error
	c.APIKeys, err = parseAPIKeys(os.Getenv("NOTIFIER_API_KEYS"))
	if err != nil {
		return Config{}, err
	}
	c.Targets, err = parseTargets(os.Getenv("NOTIFIER_TARGETS_JSON"))
	if err != nil {
		return Config{}, err
	}
	if c.MySQLDSN == "" {
		return Config{}, errors.New("NOTIFIER_MYSQL_DSN is required")
	}
	if len(c.APIKeys) == 0 || len(c.Targets) == 0 {
		return Config{}, errors.New("at least one API key and target are required")
	}
	if c.Workers < 1 || c.MaxAttempts < 1 || c.MaxBodyBytes < 1 || c.MaxEnvelopeBytes < c.MaxBodyBytes || c.BacklogLimit < 1 {
		return Config{}, errors.New("worker, attempt, backlog and size limits must be positive")
	}
	if c.ScanInterval <= 0 || c.LeaseDuration <= c.RequestTimeout || c.RequestTimeout <= 0 || c.RetryMin <= 0 || c.RetryBase < c.RetryMin || c.RetryMax < c.RetryBase || c.Validity <= 0 || c.TerminalRetention <= 0 || c.CleanupInterval <= 0 {
		return Config{}, errors.New("invalid timeout, lease, validity or retry settings")
	}
	return c, nil
}

func parseAPIKeys(raw string) (map[string]Principal, error) {
	out := make(map[string]Principal)
	for _, item := range strings.Split(raw, ",") {
		if strings.TrimSpace(item) == "" {
			continue
		}
		parts := strings.Split(item, ":")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || (parts[2] != "user" && parts[2] != "admin") {
			return nil, fmt.Errorf("invalid NOTIFIER_API_KEYS entry")
		}
		if _, exists := out[parts[1]]; exists {
			return nil, errors.New("duplicate API key")
		}
		out[parts[1]] = Principal{CallerID: parts[0], Admin: parts[2] == "admin"}
	}
	return out, nil
}

func parseTargets(raw string) (map[string]Target, error) {
	var list []Target
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, fmt.Errorf("parse NOTIFIER_TARGETS_JSON: %w", err)
	}
	out := make(map[string]Target, len(list))
	for _, target := range list {
		if target.ID == "" || len(target.Callers) == 0 || len(target.Hosts) == 0 || len(target.Ports) == 0 || target.MaxConcurrency < 1 {
			return nil, errors.New("each target requires id, callers, hosts, ports and positive max_concurrency")
		}
		if _, exists := out[target.ID]; exists {
			return nil, fmt.Errorf("duplicate target %q", target.ID)
		}
		out[target.ID] = target
	}
	return out, nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(env(name, fallback.String()))
	if err != nil {
		return -1
	}
	return value
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(env(name, strconv.Itoa(fallback)))
	if err != nil {
		return -1
	}
	return value
}

func envInt64(name string, fallback int64) int64 {
	value, err := strconv.ParseInt(env(name, strconv.FormatInt(fallback, 10)), 10, 64)
	if err != nil {
		return -1
	}
	return value
}
