package delivery

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/celanwang/rc_celanwang/internal/config"
	"github.com/celanwang/rc_celanwang/internal/domain"
)

func TestHTTPStatusDeterminesSuccess(t *testing.T) {
	tests := []struct {
		status int
		body   string
	}{
		{http.StatusOK, `{"success":false}`},
		{http.StatusAccepted, "queued"},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := New(testConfig(t, server.URL))
			if err != nil {
				t.Fatal(err)
			}
			result := client.Deliver(context.Background(), testNotification(server.URL))
			if result.Result != "http_success" {
				t.Fatalf("got %+v", result)
			}
		})
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	redirected := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			redirected = true
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer server.Close()
	client, err := New(testConfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	result := client.Deliver(context.Background(), testNotification(server.URL))
	if redirected || result.HTTPStatus == nil || *result.HTTPStatus != http.StatusFound || result.Retryable {
		t.Fatalf("unexpected redirect result: redirected=%v result=%+v", redirected, result)
	}
}

func TestTLSCertificateIsVerified(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	client, err := New(testConfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	result := client.Deliver(context.Background(), testNotification(server.URL))
	if result.ErrorCode != "tls_certificate" || result.Retryable {
		t.Fatalf("unexpected TLS result: %+v", result)
	}
}

func TestHTTPSWithConfiguredCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	defer server.Close()
	caFile := t.TempDir() + "/ca.pem"
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, server.URL)
	target := cfg.Targets["target"]
	target.CAFile = caFile
	cfg.Targets["target"] = target
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result := client.Deliver(context.Background(), testNotification(server.URL))
	if result.Result != "http_success" || result.HTTPStatus == nil || *result.HTTPStatus != http.StatusAccepted {
		t.Fatalf("unexpected HTTPS result: %+v", result)
	}
}

func TestStatusAndTimeoutClassification(t *testing.T) {
	tests := []struct {
		status    int
		retryable bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusTooManyRequests, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusNotImplemented, false},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "5")
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			client, err := New(testConfig(t, server.URL))
			if err != nil {
				t.Fatal(err)
			}
			result := client.Deliver(context.Background(), testNotification(server.URL))
			if result.Result != "http_failure" || result.Retryable != tc.retryable {
				t.Fatalf("got %+v", result)
			}
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	cfg := testConfig(t, server.URL)
	cfg.RequestTimeout = 10 * time.Millisecond
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result := client.Deliver(context.Background(), testNotification(server.URL))
	if result.ErrorCode != "timeout" || !result.Retryable {
		t.Fatalf("unexpected timeout result: %+v", result)
	}
}

func testConfig(t *testing.T, rawURL string) config.Config {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil {
			t.Fatal(err)
		}
	}
	target := config.Target{ID: "target", Callers: []string{"caller"}, Hosts: []string{u.Hostname()}, Ports: []int{port}, AllowPrivate: true, MaxConcurrency: 1}
	return config.Config{Targets: map[string]config.Target{"target": target}, Workers: 1, ConnectTimeout: time.Second, TLSTimeout: time.Second, ResponseHeaderTimeout: time.Second, RequestTimeout: time.Second, MaxResponseBodyBytes: 64}
}

func testNotification(rawURL string) domain.Notification {
	return domain.Notification{TargetID: "target", CallerID: "caller", URL: rawURL, Method: http.MethodPost, Headers: domain.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"x":1}`)}
}
