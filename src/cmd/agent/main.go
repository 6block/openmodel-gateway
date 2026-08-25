package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	faddress "github.com/filecoin-project/go-address"

	"openmodel/sp-state-agent/internal/admin"
	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/filecoin"
	"openmodel/sp-state-agent/internal/gateway"
	"openmodel/sp-state-agent/internal/health"
	"openmodel/sp-state-agent/internal/settlement"
	"openmodel/sp-state-agent/internal/worker"
	"openmodel/sp-state-agent/internal/workermtls"
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

	// Worker-direction mTLS (gateway's client certificate + private CA). Loaded
	// once, shared by the poller, the request forwarders and the probe. Fails
	// closed on half-configuration; empty config = plaintext, exactly as before.
	if cfg.WorkerMTLS.CAFile == "" && cfg.WorkerMTLS.CertFile == "" && cfg.WorkerMTLS.KeyFile == "" {
		// Loud, not silent: with no material the factory quietly degrades to
		// plaintext defaults, and any https worker endpoint then fails its
		// handshake with a misleading client-side alert (took the first external
		// SP offline for hours before anyone looked). Deliberate for all-http
		// fleets, so WARN rather than fail.
		logger.Warn("worker_mtls not configured — polling/forwarding to https:// worker endpoints WILL fail the TLS handshake; only plain-http workers are servable")
	}
	workerTLS, err := workermtls.New(cfg.WorkerMTLS.CAFile, cfg.WorkerMTLS.CertFile, cfg.WorkerMTLS.KeyFile)
	if err != nil {
		logger.Error("worker mTLS configuration invalid", "error", err)
		os.Exit(1)
	}
	if workerTLS.Enabled() {
		logger.Info("worker mTLS enabled — https worker endpoints require the private-CA handshake",
			"ca", cfg.WorkerMTLS.CAFile)
	}

	// Poller with state-change callback for logging and metrics.
	pollInterval := time.Duration(cfg.Workers.PollIntervalSec) * time.Second
	poller := worker.NewPoller(registry, pollInterval, cfg.Workers.OfflineFailThreshold, logger)
	poller.SetWorkerMTLS(workerTLS)
	if cfg.Workers.PollTimeoutSec > 0 {
		poller.SetPollTimeout(time.Duration(cfg.Workers.PollTimeoutSec) * time.Second)
	}
	poller.SetOnChange(func(workerID string, oldState, newState worker.WorkerState) {
		health.RecordStateTransition(logger, workerID, oldState, newState)
		health.UpdateMetrics(registry)
	})

	// Admin API (port 9091) — worker management + metrics + health
	if cfg.Admin.Token == "" {
		logger.Warn("admin API authentication is DISABLED — set admin.token in config for production")
	}
	adminServer := admin.NewServer(cfg.Admin.Port, cfg.Admin.Token, registry, poller, logger)
	adminServer.SetBanDefault(time.Duration(cfg.Ban.DefaultDurationSec) * time.Second)

	// Gateway (port 3000) — OpenAI-compatible reverse proxy
	if cfg.Gateway.ClientToken == "" && len(cfg.Gateway.APIKeys) == 0 {
		logger.Warn("gateway client authentication is DISABLED — set gateway.client_token or gateway.api_keys for production")
	}
	gw := gateway.New(registry, cfg.Gateway, logger)
	gw.SetWorkerMTLS(workerTLS)
	if len(cfg.Gateway.TrustedProxies) > 0 {
		if err := gateway.SetTrustedProxies(cfg.Gateway.TrustedProxies); err != nil {
			logger.Error("gateway.trusted_proxies invalid", "error", err)
			os.Exit(1) // a typo here silently collapses per-IP limits to one bucket — refuse to run
		}
		logger.Info("trusted reverse proxies configured — X-Forwarded-For honored from them", "proxies", cfg.Gateway.TrustedProxies)
	}
	// Verification sampling (A7 phase 1): retain a random fraction of served requests
	// (prompt + response + claimed model + tokens) for offline SP fraud detection.
	// Opt-in — nil (no cost) unless sample_rate > 0 and a sample_log_path is set.
	if sampler := gateway.NewVerificationSampler(
		cfg.Verification.SampleLogPath, cfg.Verification.SampleRate,
		cfg.Verification.SampleMaxMB, cfg.Verification.SampleBackups, logger,
	); sampler != nil {
		gw.SetVerificationSampler(sampler)
	}
	// SP self-registration (miner-signed admission). Must be enabled before
	// gw.Handler() builds the mux.
	if cfg.SPRegistration.Enabled {
		minPower := new(big.Int)
		if s := strings.TrimSpace(cfg.SPRegistration.MinRawPowerBytes); s != "" {
			v, ok := new(big.Int).SetString(s, 10)
			if !ok || v.Sign() < 0 {
				logger.Error("invalid sp_registration.min_raw_power_bytes", "value", s)
				os.Exit(1)
			}
			minPower = v
		}
		minBalance, err := gateway.ParseFILToAtto(cfg.SPRegistration.MinMinerBalanceFIL)
		if err != nil {
			logger.Error("invalid sp_registration.min_miner_balance_fil", "error", err)
			os.Exit(1)
		}
		gw.EnableSPRegistration(gateway.SPRegistrationOptions{
			GatewayID:          cfg.SPRegistration.GatewayID,
			MinRawPowerBytes:   minPower,
			MinMinerBalance:    minBalance,
			ChallengeTTL:       time.Duration(cfg.SPRegistration.ChallengeTTLSec) * time.Second,
			RegisterRatePerMin: cfg.SPRegistration.RegisterRatePerMin,
			MaxRegisteredSPs:   cfg.SPRegistration.MaxRegisteredSPs,
		}, filecoin.NewClient(cfg.SPRegistration.FilecoinRPCURLs, logger))
		if err := gw.EnableCertIssuer(cfg.SPRegistration.IssuerCACert, cfg.SPRegistration.IssuerCAKey,
			time.Duration(cfg.SPRegistration.CertValiditySec)*time.Second); err != nil {
			logger.Error("certificate issuer configuration invalid", "error", err)
			os.Exit(1)
		}
		logger.Info("sp self-registration enabled",
			"gateway_id", cfg.SPRegistration.GatewayID,
			"rpc_endpoints", len(cfg.SPRegistration.FilecoinRPCURLs),
			"min_raw_power_bytes", minPower.String(),
			"min_miner_balance_atto", minBalance.String(),
		)
	}
	gatewayServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Gateway.Port),
		Handler: gw.Handler(),
	}
	if cfg.SPRegistration.Enabled && cfg.SPRegistration.HTTPSPort > 0 {
		go func() {
			if err := gw.StartWorkerTLS(fmt.Sprintf(":%d", cfg.SPRegistration.HTTPSPort),
				cfg.SPRegistration.GatewayID, gw.Handler()); err != nil {
				logger.Error("worker-direction HTTPS failed", "error", err)
				os.Exit(1) // an operator who configured it wants it up, not silently absent
			}
		}()
	}

	// Settlement engine (conditional on config)
	var settler *settlement.Settler
	var balanceCache *settlement.BalanceCache
	var pricer *settlement.Pricer
	var reconciler *settlement.Reconciler
	var contractClient *settlement.ContractClient
	// Public read-only query port (SP earnings transparency); only created when both
	// settlement and public_query are enabled.
	var publicServer *admin.PublicServer
	if cfg.Settlement.Enabled {
		// Filecoin address rendering/parsing (miner ids, f410 funding helper) must
		// match the chain: mainnet uses the f prefix, everything else t. go-address
		// keys this off a package-global; set it once before any address work.
		if cfg.Settlement.ChainID == 314 {
			faddress.CurrentNetwork = faddress.Mainnet
		} else {
			faddress.CurrentNetwork = faddress.Testnet
		}

		var err error
		contractClient, err = settlement.NewContractClient(&cfg.Settlement, logger)
		if err != nil {
			logger.Error("failed to initialize settlement contract client", "error", err)
			os.Exit(1)
		}

		pricer = settlement.NewPricer(&cfg.Settlement, logger)
		balanceCache = settlement.NewBalanceCache(
			contractClient, cfg.Settlement.SupportedTokens, pricer,
			cfg.Settlement.BalanceRefreshSec, logger,
		)

		// Seed the refresh list from EVERY wallet the gateway knows: static config keys
		// AND self-registrations persisted across restarts. Using gw.KnownWallets() (not
		// just cfg.Gateway.APIKeys) is what makes a registered wallet's balance keep
		// refreshing after a restart — otherwise it is never polled and the user is
		// wrongly 402'd forever.
		balanceCache.SetWallets(gw.KnownWallets())

		// Activate the credit limits (reserve buffer + per-wallet unsettled-spend cap).
		// Both are FIL-denominated; an unset/zero value disables that limit.
		minBuf := parseFILOrNil(cfg.Settlement.MinBalanceFIL)
		maxPending := parseFILOrNil(cfg.Settlement.MaxPendingSpend)
		balanceCache.SetCreditLimits(minBuf, maxPending)

		// Debt-based service suspension (D3): empty = disabled, "0" = suspend on any
		// positive debt, ">0" = suspend at that USD threshold.
		balanceCache.SetDebtSuspension(parseSuspendThreshold(cfg.Settlement.DebtSuspendUSD))

		resolver := settlement.NewRegistryResolver(registry)
		settler = settlement.NewSettler(&cfg.Settlement, contractClient, pricer, balanceCache, resolver, cfg.Gateway.RequestLogPath, dataDir, logger)

		// Inject balance checker into gateway
		gw.SetBalanceChecker(balanceCache, &cfg.Settlement)

		// Three-way billing reconciler (B4). Its first pass waits for the settler's
		// pendingSpend restore — running earlier reads pending=0 after a restart and
		// reports the pre-restart pending as a false DRIFT alarm (round-3 soak finding).
		reconciler = settler.NewReconciler(parseFILOrNil(cfg.Settlement.ReconcileToleranceUSD), logger)
		reconciler.SetReadySignal(settler.PendingRestored())

		// Register settlement endpoints on admin API
		settlementAPI := admin.NewSettlementAPI(
			contractClient, balanceCache, pricer, settler, reconciler,
			cfg.Settlement.SupportedTokens, cfg.Settlement.SPAddressMap, logger,
		)
		// Revenue endpoints must also cover SELF-registered SPs (their payout
		// addresses live in the registry, not the static sp_address_map).
		settlementAPI.SetMinerPayoutProvider(registry.ListMinerPayoutMap)
		// Public network metrics: catalog size from the gateway's own resolution,
		// active developers counted from the request log (cached; see
		// settlement.ActiveWalletCounter). Both are counts only.
		devCounter := settlement.NewActiveWalletCounter(cfg.Gateway.RequestLogPath)
		settlementAPI.SetNetworkStatsDeps(admin.NetworkStatsDeps{
			ProductionModels: gw.AvailableModelCount,
			ActiveDevelopers: devCounter.Count,
		})
		adminServer.RegisterSettlementAPI(settlementAPI)

		// Optional public read-only query port (SP earnings transparency). Kept on a
		// SEPARATE port from the admin API so exposing it never exposes the admin
		// token's powers (register/deregister/settle/pause). Read-only, no client
		// identity, rate-limited — see internal/admin/public.go.
		if cfg.PublicQuery.Enabled {
			publicServer = admin.NewPublicServer(
				cfg.PublicQuery.Port, cfg.PublicQuery.RatePerSec, cfg.PublicQuery.RateBurst,
				settlementAPI, logger,
			)
			logger.Info("public query API enabled (read-only, no auth)", "port", cfg.PublicQuery.Port)
			// Also mount the same read-only routes on the gateway mux itself, so a
			// browser on the HTTPS web UI reads receipts SAME-ORIGIN. Without this
			// the viewer on https://…:<https_port> must call the plaintext public
			// port and the browser blocks it as mixed content. The standalone
			// public port stays for third-party auditors and scripts.
			gw.MountPublicQuery(settlementAPI.RegisterPublicRoutes)
		}

		logger.Info("settlement engine enabled",
			"interval_minutes", cfg.Settlement.IntervalMinutes,
			"contract", cfg.Settlement.ContractAddress,
		)
	}
	if cfg.PublicQuery.Enabled && !cfg.Settlement.Enabled {
		logger.Warn("public_query.enabled but settlement is disabled — public query port NOT started (it needs the settlement engine's earnings data)")
	}

	// Background context for poller and metrics goroutines.
	bgCtx, cancelBg := context.WithCancel(context.Background())

	// Admission gate needs the prober: a gate with nothing to verify claims would
	// leave every self-registered worker permanently unverified → zero traffic.
	// Fail loudly at start-up rather than silently starve the fleet.
	if cfg.Probe.AdmissionGate && !cfg.Probe.Enabled {
		logger.Error("probe.admission_gate requires probe.enabled: true (a gate with no prober starves every worker)")
		os.Exit(1)
	}
	registry.SetAdmissionGate(cfg.Probe.AdmissionGate)
	if cfg.Probe.AdmissionGate {
		logger.Info("probe admission gate ON — self-registered workers route only on verified_models")
	}

	// Automated capability auditor: admission verification (gate) + TTL re-checks +
	// periodic spot-checks. No-op unless probe.enabled; only self-registered workers.
	gw.StartAuditor(bgCtx, gateway.ProbeConfig{
		Enabled:         cfg.Probe.Enabled,
		IntervalSec:     cfg.Probe.IntervalSec,
		NumQuestions:    cfg.Probe.NumQuestions,
		MinScore:        cfg.Probe.MinScore,
		BanSeconds:      cfg.Probe.BanSeconds,
		AdmissionGate:   cfg.Probe.AdmissionGate,
		VerifyTTL:       time.Duration(cfg.Probe.VerifyTTLSec) * time.Second,
		ModelFloors:     cfg.Probe.ModelFloors,
		SpotMinInterval: time.Duration(cfg.Probe.SpotMinIntervalSec) * time.Second,
	})

	// Track background goroutines for graceful shutdown.
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := adminServer.Start(); err != nil && err != http.ErrServerClosed {
			logger.Error("admin server error", "error", err)
		}
	}()

	if publicServer != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := publicServer.Start(); err != nil && err != http.ErrServerClosed {
				logger.Error("public query server error", "error", err)
			}
		}()
	}

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
				if settler != nil {
					settler.PublishFundMetrics()
				}
			}
		}
	}()

	// Settlement goroutines (only if enabled)
	if settler != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			settler.Start(bgCtx)
		}()

		// Warm the receipt-proof index in the background so the first public query after a
		// restart doesn't pay the one-time index-load cost on the request path (R4).
		go settler.WarmReceiptIndex()

		wg.Add(1)
		go func() {
			defer wg.Done()
			balanceCache.Start(bgCtx)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			pricer.Start(bgCtx)
		}()

		// Stablecoin depeg monitor (C3): independent of the FIL price mode. No-op unless
		// stablecoin_price_sources is configured.
		wg.Add(1)
		go func() {
			defer wg.Done()
			pricer.StartStablecoinMonitor(bgCtx)
		}()

		if reconciler != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				interval := time.Duration(cfg.Settlement.ReconcileIntervalMinutes) * time.Minute
				reconciler.Start(bgCtx, interval)
			}()
		}

		// FEVM RPC endpoint health monitor (C2): probes the active endpoint and rotates
		// to a healthy one on failure. No-op when only one endpoint is configured.
		if contractClient != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				contractClient.MonitorEndpoints(bgCtx, 30*time.Second)
			}()
		}
	}

	logger.Info("sp-gateway started")

	// Wait for shutdown signal (SIGINT / SIGTERM)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	logger.Info("shutdown signal received", "signal", sig.String())

	// Graceful shutdown sequence.
	shutdownTimeout := 10 * time.Second

	// 1. Stop accepting NEW gateway work and drain in-flight requests first (B7).
	//    Clients in flight get to finish (bounded); new ones get a clean 503+Retry-After
	//    instead of a dropped connection. This also quiesces billable traffic before
	//    settlement stops, so the settler halts at a clean WAL boundary.
	drainTimeout := 8 * time.Second
	if remaining := gw.BeginDrain(drainTimeout); remaining > 0 {
		logger.Warn("proceeding with shutdown despite in-flight requests", "in_flight", remaining)
	}

	if err := adminServer.Stop(shutdownTimeout); err != nil {
		logger.Warn("admin server shutdown error", "error", err)
	}

	if publicServer != nil {
		if err := publicServer.Stop(shutdownTimeout); err != nil {
			logger.Warn("public query server shutdown error", "error", err)
		}
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

// parseFILOrNil parses a FIL-denominated config value (e.g. "0.001", "10") into a
// *big.Float. An empty string or a non-positive/invalid value returns nil, which
// the BalanceCache treats as "limit disabled".
func parseFILOrNil(s string) *big.Float {
	if s == "" {
		return nil
	}
	v, _, err := big.ParseFloat(s, 10, 128, big.ToNearestEven)
	if err != nil || v.Sign() <= 0 {
		return nil
	}
	return v
}

// parseSuspendThreshold parses the D3 debt-suspension threshold. Unlike the credit
// limits, "0" is meaningful here (suspend on ANY positive debt), so an empty string
// disables suspension (nil) while "0" returns a zero threshold.
func parseSuspendThreshold(s string) *big.Float {
	if s == "" {
		return nil // suspension disabled
	}
	v, _, err := big.ParseFloat(s, 10, 128, big.ToNearestEven)
	if err != nil || v.Sign() < 0 {
		return nil
	}
	return v
}
