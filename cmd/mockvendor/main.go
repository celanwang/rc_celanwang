package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := os.Getenv("MOCK_VENDOR_ADDR")
	if addr == "" {
		addr = ":18081"
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Info("mock request", "method", r.Method, "path", r.URL.Path, "has_idempotency", r.Header.Get("X-Supplier-Idempotency") != "")
		switch r.URL.Path {
		case "/success":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "note": "HTTP status remains authoritative"})
		case "/accepted":
			w.WriteHeader(http.StatusAccepted)
		case "/retry":
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/slow":
			time.Sleep(15 * time.Second)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 3 * time.Second}
	logger.Info("mock vendor started", "addr", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("mock vendor stopped", "error_type", "server")
		os.Exit(1)
	}
}
