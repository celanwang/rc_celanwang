package delivery

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/celanwang/rc_celanwang/internal/config"
	"github.com/celanwang/rc_celanwang/internal/domain"
	"github.com/celanwang/rc_celanwang/internal/notification"
)

type Result struct {
	Result           string
	HTTPStatus       *int
	ErrorCode        string
	Phase            string
	DeliveryEvidence string
	RetryAfter       string
	Duration         time.Duration
	ResponseSummary  string
	Retryable        bool
}

type Client struct {
	clients         map[string]*http.Client
	targets         map[string]config.Target
	maxResponseBody int64
}

type targetError struct{ message string }

func (e *targetError) Error() string { return e.message }

func New(cfg config.Config) (*Client, error) {
	clients := make(map[string]*http.Client, len(cfg.Targets))
	for id, target := range cfg.Targets {
		target := target
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		if target.CAFile != "" {
			pem, err := os.ReadFile(target.CAFile)
			if err != nil {
				return nil, fmt.Errorf("read CA file for target %s: %w", id, err)
			}
			pool, err := x509.SystemCertPool()
			if err != nil {
				return nil, fmt.Errorf("load system CA pool: %w", err)
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("CA file for target %s contains no certificate", id)
			}
			tlsConfig.RootCAs = pool
		}
		dialer := &net.Dialer{Timeout: cfg.ConnectTimeout, KeepAlive: 30 * time.Second}
		transport := &http.Transport{
			Proxy:                  nil,
			DialContext:            guardedDialer(dialer, target),
			ForceAttemptHTTP2:      true,
			MaxIdleConns:           cfg.Workers * 2,
			MaxIdleConnsPerHost:    target.MaxConcurrency,
			IdleConnTimeout:        90 * time.Second,
			TLSHandshakeTimeout:    cfg.TLSTimeout,
			ResponseHeaderTimeout:  cfg.ResponseHeaderTimeout,
			ExpectContinueTimeout:  time.Second,
			MaxResponseHeaderBytes: 32 << 10,
			TLSClientConfig:        tlsConfig,
		}
		clients[id] = &http.Client{
			Transport: transport,
			Timeout:   cfg.RequestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &Client{clients: clients, targets: cfg.Targets, maxResponseBody: cfg.MaxResponseBodyBytes}, nil
}

func (c *Client) CloseIdleConnections() {
	for _, client := range c.clients {
		client.CloseIdleConnections()
	}
}

func (c *Client) Deliver(ctx context.Context, n domain.Notification) Result {
	started := time.Now()
	client, ok := c.clients[n.TargetID]
	target := c.targets[n.TargetID]
	if !ok || !contains(target.Callers, n.CallerID) {
		return failed(started, "target_not_allowed", "validation", "not_sent", false)
	}
	req, err := http.NewRequestWithContext(ctx, n.Method, n.URL, bytes.NewReader(n.Body))
	if err != nil {
		return failed(started, "invalid_request", "validation", "not_sent", false)
	}
	for name, values := range n.Headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	for name, value := range target.Credential {
		if req.Header.Get(name) != "" {
			return failed(started, "credential_conflict", "validation", "not_sent", false)
		}
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		code, phase, retryable := classifyError(err)
		return failed(started, code, phase, "unknown", retryable)
	}
	defer resp.Body.Close()
	read, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, c.maxResponseBody+1))
	summary := fmt.Sprintf("diagnostic_body_bytes=%d", min(read, c.maxResponseBody))
	if read > c.maxResponseBody {
		summary += " truncated=true"
	}
	if readErr != nil {
		summary += " read_error=true"
	}
	status := resp.StatusCode
	result := Result{HTTPStatus: &status, Duration: time.Since(started), ResponseSummary: summary, DeliveryEvidence: "http_response"}
	if status >= 200 && status <= 299 {
		result.Result = "http_success"
		return result
	}
	result.Result = "http_failure"
	result.ErrorCode = "http_" + strconv.Itoa(status)
	result.Phase = "response"
	result.RetryAfter = resp.Header.Get("Retry-After")
	result.Retryable = status == 408 || status == 429 || (status >= 500 && status != 501 && status != 505)
	return result
}

func guardedDialer(dialer *net.Dialer, target config.Target) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, portText, err := net.SplitHostPort(address)
		if err != nil || !containsFold(target.Hosts, host) {
			return nil, &targetError{message: "target address is not allowed"}
		}
		port, err := strconv.Atoi(portText)
		if err != nil || !containsInt(target.Ports, port) {
			return nil, &targetError{message: "target port is not allowed"}
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(addresses) == 0 {
			return nil, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
		}
		for _, address := range addresses {
			if !target.AllowPrivate && !notification.HostIsPublic(address.IP) {
				return nil, &targetError{message: "resolved address is not public"}
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), portText))
	}
}

func classifyError(err error) (string, string, bool) {
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameError x509.HostnameError
	var dnsError *net.DNSError
	var urlError *url.Error
	var denied *targetError
	if errors.As(err, &denied) {
		return "target_not_allowed", "connect", false
	}
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostnameError) {
		return "tls_certificate", "tls", false
	}
	if errors.As(err, &dnsError) {
		return "dns_error", "connect", !dnsError.IsNotFound
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", "request", true
	}
	if errors.As(err, &urlError) && strings.Contains(strings.ToLower(urlError.Err.Error()), "certificate") {
		return "tls_certificate", "tls", false
	}
	return "transport_error", "request", true
}

func failed(start time.Time, code, phase, evidence string, retryable bool) Result {
	return Result{Result: "transport_error", ErrorCode: code, Phase: phase, DeliveryEvidence: evidence, Duration: time.Since(start), Retryable: retryable}
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
