package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"openmodel/sp-state-agent/internal/admin"
	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/gateway"
	"openmodel/sp-state-agent/internal/health"
	"openmodel/sp-state-agent/internal/worker"
)

func main() {
	configPath := flag.String("config", "/etc/openmodel/sp-state-agent.yaml", "config file path")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	logger := setupLogger(cfg.Logging)

	logger.Info("sp-gateway starting",
		"mode", cfg.Mode,
		"gateway_port", cfg.Gateway.Port,
		"admin_port", cfg.Admin.Port,
		"poll_interval_sec", cfg.Workers.PollIntervalSec,
		"offline_fail_threshold", cfg.Workers.OfflineFailThreshold,
	)

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/data"
	}
	savePath := dataDir + "/workers.json"

	registry := worker.NewRegistry(logger, savePath)

	// Poller with state-change callback for logging and metrics.
	pollInterval := time.Duration(cfg.Workers.PollIntervalSec) * time.Second
	poller := worker.NewPoller(registry, pollInterval, cfg.Workers.OfflineFailThreshold, logger)
	poller.SetOnChange(func(workerID string, oldState, newState worker.WorkerState) {
		health.RecordStateTransition(logger, workerID, oldState, newState)
		health.UpdateMetrics(registry)
	})

	// Admin API (port 9091) — worker management + metrics + health
	if cfg.Admin.Token == "" {
		logger.Warn("admin API authentication is DISABLED — set admin.token in config for production")
	}
	adminServer := admin.NewServer(cfg.Admin.Port, cfg.Admin.Token, registry, poller, logger)

	// Gateway (port 3000) — OpenAI-compatible reverse proxy
	if cfg.Gateway.ClientToken == "" && len(cfg.Gateway.APIKeys) == 0 {
		logger.Warn("gateway client authentication is DISABLED — set gateway.client_token or gateway.api_keys for production")
	}
	gw := gateway.New(registry, cfg.Gateway, logger)
	gatewayServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Gateway.Port),
		Handler: gw.Handler(),
	}

	// Background context for poller and metrics goroutines.
	bgCtx, cancelBg := context.WithCancel(context.Background())

	// Track background goroutines for graceful shutdown.
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := adminServer.Start(); err != nil && err != http.ErrServerClosed {
			logger.Error("admin server error", "error", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("gateway starting", "addr", gatewayServer.Addr)
		if err := gatewayServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("gateway server error", "error", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		poller.Run(bgCtx)
	}()

	// Periodic Prometheus metrics refresh
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-ticker.C:
				health.UpdateMetrics(registry)
			}
		}
	}()

	logger.Info("sp-gateway started")

	// Wait for shutdown signal (SIGINT / SIGTERM)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	logger.Info("shutdown signal received", "signal", sig.String())

	// Graceful shutdown sequence
	shutdownTimeout := 10 * time.Second
	if err := adminServer.Stop(shutdownTimeout); err != nil {
		logger.Warn("admin server shutdown error", "error", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()
	if err := gatewayServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("gateway server shutdown error", "error", err)
	}

	cancelBg()
	gw.Close() // Close request log file

	// Wait for goroutines with a bounded timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		logger.Info("all goroutines exited cleanly")
	case <-time.After(5 * time.Second):
		logger.Warn("shutdown timeout, forcing exit")
	}

	logger.Info("shutdown complete")
}

func setupLogger(cfg config.LoggingConfig) *slog.Logger {
	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: parseLogLevel(cfg.Level)}
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}

func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
