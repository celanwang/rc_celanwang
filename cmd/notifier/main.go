package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/celanwang/rc_celanwang/internal/api"
	"github.com/celanwang/rc_celanwang/internal/config"
	"github.com/celanwang/rc_celanwang/internal/delivery"
	"github.com/celanwang/rc_celanwang/internal/notification"
	"github.com/celanwang/rc_celanwang/internal/store/mysqlstore"
	"github.com/celanwang/rc_celanwang/internal/worker"
)

func main() {
	migrateOnly := flag.Bool("migrate-only", false, "apply database migrations and exit")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration rejected", "error", err)
		os.Exit(1)
	}
	db, err := mysqlstore.Open(cfg.MySQLDSN)
	if err != nil {
		logger.Error("database initialization failed", "error_type", "database")
		os.Exit(1)
	}
	defer db.Close()
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startupCancel()
	if err := db.PingContext(startupCtx); err != nil {
		logger.Error("database connection failed", "error_type", "database")
		os.Exit(1)
	}
	if err := mysqlstore.Migrate(startupCtx, db); err != nil {
		logger.Error("database migration failed", "error_type", "database")
		os.Exit(1)
	}
	if *migrateOnly {
		logger.Info("database migrations applied")
		return
	}
	store := mysqlstore.New(db)
	deliverer, err := delivery.New(cfg)
	if err != nil {
		logger.Error("delivery client initialization failed", "error_type", "configuration")
		os.Exit(1)
	}
	defer deliverer.CloseIdleConnections()
	service := notification.NewService(store, cfg)
	apiServer := api.New(service, cfg, store.Ping, logger)
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	manager := worker.New(store, deliverer, cfg, logger)
	go manager.Run(workerCtx)
	errCh := make(chan error, 1)
	go func() {
		logger.Info("notification service started", "addr", cfg.Addr, "workers", cfg.Workers)
		errCh <- httpServer.ListenAndServe()
	}()
	signalCtx, stopSignal := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignal()
	select {
	case <-signalCtx.Done():
		logger.Info("shutdown requested")
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server stopped", "error_type", "server")
		}
	}
	stopWorkers()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("HTTP shutdown incomplete", "error_type", "timeout")
	}
	if err := manager.Wait(shutdownCtx); err != nil {
		logger.Warn("in-flight delivery shutdown incomplete", "error_type", "timeout")
	}
}
