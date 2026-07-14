// Package gateway provides an OpenAI-compatible reverse proxy that routes
// inference requests to available SP Workers with mining-aware load balancing.
//
// Unlike the previous new-api architecture, routing decisions and state
// monitoring happen in the same process — no sync delay, no channel management.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	math_big "math/big"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/metrics"
	"openmodel/sp-state-agent/internal/settlement"
	"openmodel/sp-state-agent/internal/worker"
)

// maxRequestBody caps the request body we'll read to rewrite stream flag (10 MiB).
const maxRequestBody = 10 << 20

const (
	queuePollInterval = 1 * time.Second // How often to re-check for available workers
)

// apiKeyEntry is a resolved API key for fast lookup.
type apiKeyEntry struct {
	Name   string
	Wallet string
}

// Gateway is the OpenAI-compatible reverse proxy.
type Gateway struct {
	registry              *worker.Registry
	apiKeys               map[string]apiKeyEntry // key string → metadata (empty map = auth disabled)
	keysMu                sync.RWMutex           // guards apiKeys (self-registration adds keys at runtime)
	registrationsPath     string                 // JSON store of self-registered keys; empty = no persistence
	seenSigs              map[string]time.Time   // recently-used registration signatures → expiry (replay guard)
	regMu                 sync.Mutex             // guards seenSigs and registration writes
	authEnabled           bool
	requestTimeout        time.Duration
	queueTimeout          time.Duration // Max time to wait in queue for an available worker
	maxQueueSize          int           // Max queued requests; 0 = unlimited
	modelSwitchLoadFactor float64       // Trigger model switch when loaded worker overloaded; 0 = disabled
	queuedCount           atomic.Int32  // Current number of queued requests
	requestLogger         *RequestLogger
	balanceChecker        *settlement.BalanceCache
	settlementCfg         *settlement.Config
	modelPrices           map[string]*math_big.Float // precomputed USD-per-token output/base price
	catalogInput          map[string]*math_big.Float // per-token input price (catalog split), for adjustBalance
	catalogCacheRead      map[string]*math_big.Float // per-token cache-read price (catalog split)
	httpClient            *http.Client               // shared client for non-streaming forwards (connection pooling)
	streamTransport       *http.Transport            // shared transport for streaming reverse proxy
	sessions              *sessionAffinity           // session→worker stickiness for prefix-cache reuse
	rateLimiter           *RateLimiter               // per-key rate + concurrency abuse controls (B5); nil = disabled
	maxRequestBytes       int64                      // max request body size; 0 falls back to maxRequestBody
	draining              atomic.Bool                // true once graceful shutdown begins; new requests get 503 (B7)
	inFlight              atomic.Int64               // proxied requests currently being served (for drain accounting, B7)
	sampler               *verificationSampler       // retains a fraction of req/resp pairs for SP fraud detection (A7); nil = off
	streamResume          bool                       // B2: transparently continue server-interrupted streams on another worker
	streamMaxResumes      int                        // B2: per-request continuation budget
	logger                *slog.Logger
}

// SetVerificationSampler enables sampling+retention of request/response pairs for offline
// SP fraud detection (model substitution / token misreporting). No-op when the sampler is
// nil (feature disabled), so the request path pays nothing when off.
func (g *Gateway) SetVerificationSampler(s *verificationSampler) {
	g.sampler = s
}

// KnownWallets returns the distinct non-empty wallets across every API key the gateway
// currently knows — static config keys AND persisted self-registrations loaded at
// startup. main.go seeds the BalanceCache refresh list from this so a self-registered
// wallet's on-chain balance is refreshed across restarts; otherwise it is never polled
// and the user is wrongly 402'd forever after a restart.
func (g *Gateway) KnownWallets() []string {
	g.keysMu.RLock()
	defer g.keysMu.RUnlock()
	seen := make(map[string]bool)
	var out []string
	for _, e := range g.apiKeys {
		if e.Wallet == "" {
			continue
		}
		lower := strings.ToLower(e.Wallet)
		if seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, e.Wallet)
	}
	return out
}

// SetBalanceChecker injects the settlement balance checker for pre-request balance
// validation and precomputes the per-token model prices once (avoids reparsing
// on every request). Catalog (input/cache-read) prices are parsed too: the deferred
// pendingSpend adjustment must price completed requests with the IDENTICAL catalog
// split settlement clears them with (settlement.CostBreakdownUSD) — adjusting at the
// flat rate left an un-drainable residue per request (audit finding #47).
func (g *Gateway) SetBalanceChecker(bc *settlement.BalanceCache, cfg *settlement.Config) {
	g.balanceChecker = bc
	g.settlementCfg = cfg
	g.modelPrices = make(map[string]*math_big.Float)
	if cfg != nil {
		for model, priceStr := range cfg.ModelPricesUSD {
			price, _, err := math_big.ParseFloat(priceStr, 10, 128, math_big.ToNearestEven)
			if err != nil {
				g.logger.Warn("invalid model price in settlement config", "model", model, "price", priceStr)
				continue
			}
			price.Quo(price, math_big.NewFloat(1_000_000)) // USD per 1M tokens → per token
			g.modelPrices[model] = price
		}
		g.catalogInput, g.catalogCacheRead = settlement.ParseCatalogPrices(cfg.ModelCatalog)
	}
}

// New creates a new Gateway.
func New(registry *worker.Registry, cfg config.GatewayConfig, logger *slog.Logger) *Gateway {
	timeout := time.Duration(cfg.RequestTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	queueTimeout := time.Duration(cfg.QueueTimeoutSec) * time.Second
	if queueTimeout <= 0 {
		queueTimeout = 60 * time.Second
	}

	// Build API key lookup table
	keys := make(map[string]apiKeyEntry)
	if len(cfg.APIKeys) > 0 {
		for _, k := range cfg.APIKeys {
			if k.Key != "" {
				keys[k.Key] = apiKeyEntry{Name: k.Name, Wallet: k.Wallet}
			}
		}
	} else if cfg.ClientToken != "" {
		// Backward compat: single token → key named "default"
		keys[cfg.ClientToken] = apiKeyEntry{Name: "default"}
	}

	// Self-registered keys (POST /v1/register) persist to a JSON file next to the
	// request log; load them at startup so they survive restarts and auth (and the
	// authEnabled gate below) recognizes them alongside the static config keys.
	registrationsPath := registrationsPathFor(cfg.RequestLogPath)
	if registrationsPath != "" {
		for _, rec := range loadRegistrationsFile(registrationsPath, logger) {
			if rec.Key != "" {
				keys[rec.Key] = apiKeyEntry{Name: rec.Name, Wallet: rec.Wallet}
			}
		}
	}

	// Shared transports so connections are pooled and reused across requests instead
	// of building a fresh transport (and TCP/TLS handshake) per request (audit fix).
	nonStreamTransport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
	streamTransport := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: timeout, // bound time-to-first-byte; the stream body itself is unbounded
	}

	// Request log: size/backup retention must exceed the settlement interval × peak
	// rate or settlement falls behind rotation and loses un-billed records.
	reqLog := NewRequestLogger(cfg.RequestLogPath, logger)
	if reqLog != nil {
		if cfg.RequestLogMaxMB > 0 {
			reqLog.maxSize = int64(cfg.RequestLogMaxMB) << 20
		}
		if cfg.RequestLogBackups > 0 {
			reqLog.maxBackups = cfg.RequestLogBackups
		}
	}

	maxResumes := cfg.StreamMaxResumes
	if maxResumes <= 0 {
		maxResumes = 2 // default continuation budget when stream_resume is enabled
	}

	return &Gateway{
		registry:              registry,
		apiKeys:               keys,
		registrationsPath:     registrationsPath,
		seenSigs:              make(map[string]time.Time),
		authEnabled:           len(keys) > 0,
		requestTimeout:        timeout,
		queueTimeout:          queueTimeout,
		maxQueueSize:          cfg.MaxQueueSize,
		modelSwitchLoadFactor: cfg.ModelSwitchLoadFactor,
		requestLogger:         reqLog,
		httpClient:            &http.Client{Timeout: timeout, Transport: nonStreamTransport},
		streamTransport:       streamTransport,
		sessions:              newSessionAffinity(0),
		rateLimiter: NewRateLimiter(RateLimitConfig{
			RatePerSec:  cfg.RatePerSecPerKey,
			Burst:       cfg.RateBurstPerKey,
			MaxInFlight: cfg.MaxConcurrentPerKey,
		}),
		maxRequestBytes:  cfg.MaxRequestBytes,
		streamResume:     cfg.StreamResume,
		streamMaxResumes: maxResumes,
		logger:           logger,
	}
}

// Close releases resources (request log file, verification sample log).
func (g *Gateway) Close() error {
	g.sampler.close()
	return g.requestLogger.Close()
}

// BeginDrain starts graceful shutdown (B7): new proxy requests are immediately
// rejected with 503 + Retry-After, while requests already in flight are given up to
// `timeout` to complete. It returns once in-flight reaches zero or the timeout
// elapses, reporting how many requests were still in flight at return (0 = clean).
//
// Sequencing in main: call BeginDrain BEFORE http.Server.Shutdown so clients get a
// reliable 503 during the window instead of a dropped connection, and so settlement
// can stop at a WAL boundary without new billable traffic arriving.
func (g *Gateway) BeginDrain(timeout time.Duration) int64 {
	g.draining.Store(true)
	metrics.GatewayDraining.Set(1)
	g.logger.Info("gateway draining: rejecting new requests, waiting for in-flight to finish",
		"in_flight", g.inFlight.Load(), "timeout", timeout.String())

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if g.inFlight.Load() == 0 {
			g.logger.Info("gateway drain complete, no in-flight requests remain")
			return 0
		}
		time.Sleep(50 * time.Millisecond)
	}
	remaining := g.inFlight.Load()
	if remaining > 0 {
		g.logger.Warn("gateway drain timed out with requests still in flight", "in_flight", remaining)
	}
	return remaining
}

// IsDraining reports whether graceful shutdown has begun.
func (g *Gateway) IsDraining() bool { return g.draining.Load() }

// InFlight returns the number of proxy requests currently being served.
func (g *Gateway) InFlight() int64 { return g.inFlight.Load() }

// Handler returns an http.Handler for the gateway.
// Mount this on the gateway HTTP server (port 3000).
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", g.handleProxy)
	mux.HandleFunc("/v1/completions", g.handleProxy)
	mux.HandleFunc("/v1/models", g.handleModels)
	mux.HandleFunc("/v1/catalog", g.handleCatalog)
	mux.HandleFunc("/v1/register", g.handleRegister)
	// Catch-all for unsupported /v1/* endpoints
	mux.HandleFunc("/v1/", g.handleUnsupported)
	return mux
}

// selectWithAffinity prefers the worker this session last used (so vLLM's prefix
// cache is reused) when that worker is still routable AND has the requested model
// loaded; otherwise it selects normally and records the mapping. Affinity is a
// preference — a gone / busy-elsewhere / model-switched sticky worker transparently
// falls back to normal weighted routing, so stickiness never blocks a request.
func (g *Gateway) selectWithAffinity(sessKey, requestModel string) (*worker.Worker, error) {
	if sessKey != "" {
		if wid, ok := g.sessions.get(sessKey); ok {
			if w, found := g.registry.Get(wid); found &&
				(w.State == worker.StateIdle || w.State == worker.StateBusy) &&
				(requestModel == "" || requestModel == "default" || modelMatches(w.LoadedModel, requestModel)) {
				return w, nil
			}
		}
	}
	target, err := selectWorkerForModel(g.registry, requestModel, nil, g.modelSwitchLoadFactor)
	if err != nil {
		return nil, err
	}
	g.sessions.put(sessKey, target.ID) // no-op when sessKey == ""
	return target, nil
}

// handleProxy is the core reverse proxy handler for inference requests.
func (g *Gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// Graceful drain (B7): once shutdown has begun, refuse new work with a clean
	// 503 + Retry-After so the client retries elsewhere, instead of having its
	// connection dropped mid-flight by the server shutdown.
	if g.draining.Load() {
		w.Header().Set("Retry-After", "5")
		jsonError(w, "server is shutting down, please retry", http.StatusServiceUnavailable)
		return
	}
	g.inFlight.Add(1)
	defer g.inFlight.Add(-1)

	// Auth check — returns key metadata if valid
	keyInfo, ok := g.authenticate(r)
	if !ok {
		g.logger.Warn("auth failed", "path", r.URL.Path, "remote", r.RemoteAddr)
		jsonError(w, "invalid or missing Authorization header", http.StatusUnauthorized)
		return
	}

	// Generate unique request ID
	requestID := generateRequestID()
	w.Header().Set("X-Request-ID", requestID)

	// Per-key abuse controls (B5). Rate limit first (cheap, no slot held), then a
	// concurrency slot held for the whole request via defer. A noisy key is bounded
	// without affecting other keys; unset limits are no-ops.
	if g.rateLimiter != nil {
		if !g.rateLimiter.AllowRate(keyInfo.Name) {
			metrics.RateLimitRejectedTotal.WithLabelValues("rate").Inc()
			g.logger.Warn("rate limit exceeded", "api_key", keyInfo.Name, "request_id", requestID)
			w.Header().Set("Retry-After", "1")
			jsonError(w, "rate limit exceeded for this API key", http.StatusTooManyRequests)
			return
		}
		release, ok := g.rateLimiter.Acquire(keyInfo.Name)
		if !ok {
			metrics.RateLimitRejectedTotal.WithLabelValues("concurrency").Inc()
			g.logger.Warn("per-key concurrency limit reached", "api_key", keyInfo.Name, "request_id", requestID)
			w.Header().Set("Retry-After", "1")
			jsonError(w, "too many concurrent requests for this API key", http.StatusTooManyRequests)
			return
		}
		defer release()
	}

	// Read request body first (need model name for routing). Enforce the configured
	// max body size: a body at or over the cap is rejected with 413 rather than
	// silently truncated (which would corrupt JSON and zero out billing).
	bodyCap := g.maxRequestBytes
	if bodyCap <= 0 {
		bodyCap = maxRequestBody
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, bodyCap+1))
	if err != nil {
		jsonError(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > bodyCap {
		metrics.RateLimitRejectedTotal.WithLabelValues("body_too_large").Inc()
		g.logger.Warn("request body exceeds limit", "api_key", keyInfo.Name,
			"request_id", requestID, "limit_bytes", bodyCap)
		jsonError(w, fmt.Sprintf("request body exceeds limit of %d bytes", bodyCap), http.StatusRequestEntityTooLarge)
		return
	}

	// Validate the body IS JSON before doing anything model-based with it (F1, soak v2
	// finding): previously a malformed body fell through extractModel→"unknown"→
	// "no worker supports model" 404, which misleads clients into thinking the endpoint
	// doesn't exist. Correct HTTP semantics for a syntactically bad body is 400.
	if !json.Valid(body) {
		metrics.GatewayRequestsTotal.WithLabelValues("400", "none").Inc()
		jsonError(w, "request body is not valid JSON", http.StatusBadRequest)
		return
	}

	requestModel := extractModel(body)
	isStreaming := extractBool(body, "stream")

	// om_continuation is a GATEWAY-INTERNAL field (B2 stream resume). A client setting
	// it directly would smuggle unbilled prompt text past the token estimate, so it is
	// rejected outright — only the gateway itself attaches it on a resumed segment.
	if hasTopLevelKey(body, "om_continuation") {
		metrics.GatewayRequestsTotal.WithLabelValues("400", "none").Inc()
		jsonError(w, "om_continuation is a reserved internal field", http.StatusBadRequest)
		return
	}
	sessKey := sessionKeyOf(keyInfo.Name, r.Header.Get("X-Session-Id"), body)
	// Expose an opaque fingerprint of the routing session key so isolation is
	// observable: same (key, session id) → same value; different api keys with the
	// same X-Session-Id → different values.
	if h := shortSessionHash(sessKey); h != "" {
		w.Header().Set("X-Session-Key", h)
	}

	// Balance check (only when settlement is enabled and wallet is set).
	// We reserve the estimated cost up-front, then use `settled` to record the
	// actual cost once known. A deferred adjustment reverses the reservation on
	// EVERY exit path (402/404/queue-timeout/success/failure), preventing the
	// pendingSpend leak (fixes C5).
	var estimatedCostUSD *math_big.Float
	var settledUsage tokenUsage
	settledModel := ""
	if g.balanceChecker != nil && keyInfo.Wallet != "" {
		// Debt-based service suspension (D3): a wallet whose outstanding carried debt
		// crossed the suspension threshold is refused with 402 account_suspended —
		// distinct from "insufficient balance" — until settlement collects the debt
		// after a top-up. This forces debt repayment rather than letting a wallet keep
		// running right at the edge of its balance.
		if suspended, debt := g.balanceChecker.IsSuspended(keyInfo.Wallet); suspended {
			metrics.GatewayRequestsTotal.WithLabelValues("402", "none").Inc()
			g.logger.Warn("request refused: wallet suspended for unpaid debt",
				"api_key", keyInfo.Name, "wallet", keyInfo.Wallet,
				"debt_usd", debt.Text('f', 6), "request_id", requestID)
			jsonError(w, "account suspended: outstanding debt must be settled (top up your balance and wait for the next settlement cycle)", http.StatusPaymentRequired)
			return
		}

		maxTokens := extractInt(body, "max_tokens")
		if maxTokens <= 0 && g.settlementCfg != nil {
			maxTokens = g.settlementCfg.DefaultMaxTokens
		}
		// Include a conservative estimate of the prompt (input) tokens. Without it the
		// reservation only covers max_tokens (output), so a large prompt with a tiny
		// max_tokens would slip past the balance gate under-reserved. Over-reserving is
		// safe here: adjustBalance() reconciles to the actual usage on completion, so the
		// only effect is erring toward rejecting an under-funded request.
		estimatedTokens := maxTokens + estimatePromptTokens(body)
		estimatedCostUSD = settlement.EstimateCostUSD(requestModel, estimatedTokens, g.modelPrices)
		if !g.balanceChecker.Reserve(keyInfo.Wallet, estimatedCostUSD) {
			metrics.GatewayRequestsTotal.WithLabelValues("402", "none").Inc()
			jsonError(w, "insufficient balance", http.StatusPaymentRequired)
			return
		}
		defer func() {
			// settledUsage/settledModel are set by the handlers on a billable
			// outcome; they stay zero on any failure → full reservation reversal.
			g.adjustBalance(keyInfo.Wallet, estimatedCostUSD, settledModel, settledUsage)
		}()
	}

	// Select a worker with model-aware routing, preferring this session's sticky
	// worker (prefix-cache reuse) when it is still routable for the model.
	// Priority: loaded model > supported model > any (for "default")
	target, err := g.selectWithAffinity(sessKey, requestModel)
	if err != nil {
		// Only queue if workers exist but are mining/offline.
		// If no worker supports the model at all, reject immediately.
		if !anyWorkerSupportsModel(g.registry, requestModel) {
			metrics.GatewayRequestsTotal.WithLabelValues("404", "none").Inc()
			jsonError(w, fmt.Sprintf("no worker supports model %q", requestModel), http.StatusNotFound)
			return
		}
		target, err = g.blockForWorker(r.Context(), requestModel, time.Now().Add(g.requestTimeout))
		if err != nil {
			msg := "no available worker — all workers are mining or offline"
			reason := "queue_timeout"
			if errors.Is(err, ErrQueueFull) {
				msg = fmt.Sprintf("request rejected — queue full (%d/%d)", g.queuedCount.Load(), g.maxQueueSize)
				reason = "queue_full"
			}
			metrics.GatewayRequestsTotal.WithLabelValues("503", "none").Inc()
			g.logRequest(RequestRecord{
				RequestID: requestID, Timestamp: time.Now(), APIKeyName: keyInfo.Name,
				Wallet: keyInfo.Wallet, WorkerID: "none", Path: r.URL.Path,
				Status: 503, ErrorReason: reason,
			})
			w.Header().Set("Retry-After", g.retryAfterEstimate())
			jsonError(w, msg, http.StatusServiceUnavailable)
			return
		}
	}

	// Rewrite model name in request body to match what the worker has loaded.
	// This prevents M1's _ensure_model_loaded from triggering unnecessary model
	// switches due to path format differences (e.g., "Qwen/X" vs "/models/Qwen--X").
	// If the model is NOT loaded (intentional switch to a supported model),
	// keep the original name so M1 performs the switch.
	if target.LoadedModel != "" && modelMatches(target.LoadedModel, requestModel) {
		body = rewriteModelField(body, target.LoadedModel)
	}

	// Log if this routing will trigger a model switch on the worker
	if requestModel != "" && requestModel != "default" &&
		!modelMatches(target.LoadedModel, requestModel) {
		reason := "no_worker_has_model_loaded"
		if g.modelSwitchLoadFactor > 0 {
			// Check if this was triggered by overload
			for _, w := range g.registry.List() {
				if modelMatches(w.LoadedModel, requestModel) &&
					(w.State == worker.StateIdle || w.State == worker.StateBusy) {
					reason = fmt.Sprintf("overload (active=%d, threshold=%d)",
						w.ActiveRequests, int(float64(w.GPUCount)*g.modelSwitchLoadFactor))
					break
				}
			}
		}
		g.logger.Info("routing will trigger model switch",
			"request_id", requestID,
			"worker", target.ID,
			"requested_model", requestModel,
			"current_model", target.LoadedModel,
			"reason", reason,
		)
		// Exclude this worker from routing OTHER requests for the duration of the
		// switch. The triggering request below still goes to it (it asked for the
		// new model); subsequent requests skip it until it reports the target model
		// loaded. Without this, requests routed to the reloading engine hang until
		// they time out (the worker keeps reporting "running" during the switch, so
		// polling alone never excludes it).
		g.registry.MarkSwitching(target.ID, requestModel)
	}

	t0 := time.Now()

	if !isStreaming {
		settledModel, settledUsage = g.handleNonStreaming(w, r, body, target, requestID, requestModel, keyInfo, t0)
		return
	}
	settledModel, settledUsage = g.handleStreaming(w, r, body, target, requestID, requestModel, keyInfo, t0)
}

// handleNonStreaming forwards a non-streaming request to a worker.
// If the worker returns 503 (mining mid-generation), retries on a different worker.
// handleNonStreaming returns (settledModel, settledUsage) describing the billable
// outcome: the actual model + token breakdown on success, or zero usage on any
// failure so the caller's deferred balance adjustment fully reverses the reservation.
// The full breakdown (not just the total) is returned because the adjustment prices
// it with the catalog split — identical to settlement (see adjustBalance).
func (g *Gateway) handleNonStreaming(w http.ResponseWriter, r *http.Request, body []byte,
	firstTarget *worker.Worker, requestID, requestModel string, keyInfo apiKeyEntry, t0 time.Time) (string, tokenUsage) {

	tried := make(map[string]bool)
	target := firstTarget
	// Keep trying until the overall request deadline: forward, and on a transient
	// failure (worker mining/offline/transport error) move to another servable
	// worker — or, when none is servable right now, wait in the queue for one to
	// recover and retry with a fresh slate. This makes brief overlapping outages
	// (both machines momentarily mining/offline) transparent instead of a 503.
	deadline := time.Now().Add(g.requestTimeout)

	for attempt := 0; ; attempt++ {
		// Inc/Dec the worker's in-flight count around the forward, guarded by
		// defer so a panic in forwardRequest can't leak the count and depress
		// that worker's routing weight forever (the http server recovers the
		// panic per-request, so without defer the Dec would simply be skipped).
		var respBody []byte
		var status int
		var upstreamHeader http.Header
		var workerID string
		var err error
		func() {
			g.registry.IncInflight(target.ID)
			defer g.registry.DecInflight(target.ID)
			respBody, status, upstreamHeader, workerID, err = g.forwardRequest(r.Context(), r.URL.Path, body, target, requestID)
		}()

		if err != nil {
			// Transport error (connection refused, timeout)
			g.logger.Error("gateway proxy error",
				"request_id", requestID, "worker", workerID,
				"attempt", attempt+1, "error", err)
			tried[workerID] = true
		} else if status == 503 {
			// Worker returned 503 (mining mid-generation) — retry on another worker
			g.logger.Warn("worker returned 503, retrying on another",
				"request_id", requestID, "worker", workerID, "attempt", attempt+1)
			tried[workerID] = true
		} else {
			// Success or non-retryable error — send to client
			elapsed := time.Since(t0)
			usage := g.clampUsage(extractUsage(respBody), requestModel, requestID, workerID)
			// A1: capture + verify the worker-signed receipt (nil when absent). The
			// receipt is compared against the UNclamped usage the worker reported —
			// it attests the worker's own claim; clamping only bounds billing.
			var receipt *ReceiptInfo
			if status == http.StatusOK && target.HasFeature(worker.FeatureReceipt) {
				receipt = g.captureReceipt(upstreamHeader.Get("X-Om-Receipt"), workerID,
					target.ReceiptPubkey, body, extractUsage(respBody), true, requestID)
			}
			statusStr := fmt.Sprintf("%d", status)
			metrics.GatewayRequestsTotal.WithLabelValues(statusStr, workerID).Inc()
			metrics.GatewayRequestDuration.WithLabelValues(workerID).Observe(elapsed.Seconds())

			g.logger.Info("gateway request",
				"request_id", requestID, "api_key", keyInfo.Name,
				"worker", workerID, "path", r.URL.Path, "model", requestModel,
				"status", status, "duration_ms", elapsed.Milliseconds(),
				"prompt_tokens", usage.PromptTokens, "completion_tokens", usage.CompletionTokens,
				"attempts", attempt+1)

			g.logRequest(RequestRecord{
				RequestID: requestID, Timestamp: t0, APIKeyName: keyInfo.Name,
				Wallet: keyInfo.Wallet, WorkerID: workerID, Path: r.URL.Path,
				Model: requestModel, Status: status, DurationMs: elapsed.Milliseconds(),
				PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
				CachedTokens: usage.CachedTokens, TotalTokens: usage.TotalTokens,
				Receipt: receipt,
			})

			// Pass through the worker's response headers (minus hop-by-hop ones),
			// then force our own Content-Type/X-Request-ID (audit fix: previously the
			// upstream headers were dropped entirely).
			copyResponseHeaders(w.Header(), upstreamHeader)
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", "application/json")
			}
			w.Header().Set("X-Request-ID", requestID)
			w.WriteHeader(status)
			w.Write(respBody)
			// Retain a sampled fraction (prompt + response + claimed model + tokens) for
			// offline SP fraud detection. After the client write so it never adds latency.
			g.retainSample(g.sampler.shouldSample(), requestID, t0, keyInfo, workerID,
				requestModel, false, status, body, string(respBody), usage)
			// Bill the actual token usage (caller's defer adjusts the reservation) —
			// but ONLY for a 200, matching settlement's billable predicate. A non-200
			// response is never settled, so keeping usage reserved for it would wedge
			// pendingSpend forever (and let a byzantine worker inflate a wallet's
			// pending with crafted 4xx+usage bodies until the credit cap 402s it).
			if status == http.StatusOK {
				return requestModel, usage
			}
			return requestModel, tokenUsage{}
		}

		// Try another worker that is servable right now (excluding ones we tried).
		if next, nerr := selectWorkerForModel(g.registry, requestModel, tried, g.modelSwitchLoadFactor); nerr == nil {
			target = next
			continue
		}
		// Nothing servable at this instant. Rather than fail, wait in the queue for
		// a worker to come back (mining is brief; offline windows are short), then
		// retry with a fresh slate — bounded by the overall deadline.
		w2, werr := g.blockForWorker(r.Context(), requestModel, deadline)
		if werr != nil {
			break // deadline reached, queue full, or client disconnected
		}
		tried = make(map[string]bool) // a recovered worker is a fresh candidate
		target = w2
	}

	// Deadline reached with no servable worker
	elapsed := time.Since(t0)
	metrics.GatewayRequestsTotal.WithLabelValues("503", "none").Inc()
	metrics.GatewayRequestDuration.WithLabelValues("none").Observe(elapsed.Seconds())

	g.logger.Warn("all retries exhausted, returning 503",
		"request_id", requestID, "api_key", keyInfo.Name,
		"path", r.URL.Path, "model", requestModel,
		"duration_ms", elapsed.Milliseconds(),
		"workers_tried", len(tried))

	g.logRequest(RequestRecord{
		RequestID: requestID, Timestamp: t0, APIKeyName: keyInfo.Name,
		Wallet: keyInfo.Wallet, WorkerID: "none", Path: r.URL.Path,
		Model: requestModel, Status: 503, ErrorReason: "all_retries_failed",
		DurationMs: elapsed.Milliseconds(),
	})

	w.Header().Set("Retry-After", g.retryAfterEstimate())
	jsonError(w, "all workers returned errors or are mining", http.StatusServiceUnavailable)
	// All retries failed → not billed (caller's defer reverses the reservation).
	return "", tokenUsage{}
}

// forwardRequest sends the request body to a worker and returns the full response.
// Does NOT write to the client — caller decides what to do with the response.
// The request is bound to ctx so a client disconnect (or handler timeout) cancels the
// upstream call instead of leaking the worker connection (audit fix). The shared
// g.httpClient pools connections across requests.
func (g *Gateway) forwardRequest(ctx context.Context, path string, body []byte, target *worker.Worker, requestID string) (
	respBody []byte, status int, header http.Header, workerID string, err error) {

	workerID = target.ID
	reqURL := strings.TrimRight(target.Endpoint, "/") + path

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, workerID, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", requestID)
	if target.HasFeature(worker.FeatureReceipt) {
		req.Header.Set("X-OM-Receipt-Req", "1") // A1: ask the worker to sign a receipt
	}
	// Authenticate to the worker with its per-worker token (required when the worker
	// is on a different public IP, so its GPU can't be used bypassing the gateway).
	if target.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+target.AuthToken)
	}

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, 0, nil, workerID, err
	}
	defer resp.Body.Close()

	respBody, err = io.ReadAll(io.LimitReader(resp.Body, maxRequestBody))
	if err != nil {
		return nil, resp.StatusCode, resp.Header, workerID, err
	}
	return respBody, resp.StatusCode, resp.Header, workerID, nil
}

// errWorkerMining is the sentinel ModifyResponse returns when a worker answers a
// streaming request with 503 (yielded to mining mid-flight). It routes through
// ErrorHandler so the stream can be retried on another worker BEFORE any bytes reach
// the client.
var errWorkerMining = errors.New("worker returned 503 (mining)")

// handleStreaming forwards a streaming request via reverse proxy with SSE passthrough.
// Returns (settledModel, settledTokens): the actual usage on a clean completion, or
// ("", 0) when the stream was interrupted (not billed) so the reservation reverses.
//
// If a worker returns 503 or fails at the transport layer BEFORE any bytes have been
// sent to the client, the request is retried on a different worker (audit fix:
// previously the streaming path had no 503 retry, unlike the non-streaming path).
// Once bytes have streamed to the client a failure can no longer be recovered.
func (g *Gateway) handleStreaming(w http.ResponseWriter, r *http.Request, body []byte,
	target *worker.Worker, requestID, requestModel string, keyInfo apiKeyEntry, t0 time.Time) (string, tokenUsage) {

	tried := make(map[string]bool)
	// Same deadline-bounded retry policy as non-streaming, but only recoverable
	// BEFORE any bytes reach the client (once streaming starts we cannot retry) —
	// UNLESS stream resume (B2) is enabled: a SERVER-side interruption after bytes
	// have flowed is then transparently continued on another capable worker.
	deadline := time.Now().Add(g.requestTimeout)

	// Decide once whether to retain this request for offline SP fraud detection; when
	// sampled, each sseCap accumulates the raw SSE delivered to the client (bounded).
	sampled := g.sampler.shouldSample()

	// B2 stream-resume state, spanning segments of one client stream.
	var resumeContent bytes.Buffer // decoded text delivered so far (continuation prefix)
	resumes := 0                   // continuation segments dispatched
	totalDelivered := 0            // delivered completion tokens across all segments
	clientStarted := false         // any bytes sent to the client by ANY segment
	curBody := body                // current upstream body (original, or continuation)
	origMaxTokens := extractInt(body, "max_tokens")
	if origMaxTokens <= 0 && g.settlementCfg != nil {
		origMaxTokens = g.settlementCfg.DefaultMaxTokens
	}
	if origMaxTokens <= 0 {
		origMaxTokens = 256 // matches the worker-side default
	}

	var sseCap *sseCaptureWriter
	for attempt := 0; ; attempt++ {
		targetURL, err := url.Parse(target.Endpoint)
		if err != nil {
			jsonError(w, "internal routing error", http.StatusBadGateway)
			return "", tokenUsage{}
		}

		sseCap = &sseCaptureWriter{ResponseWriter: w}
		// F2 (soak v2 finding): streaming clients previously had NO way to obtain a
		// verifiable receipt (the gateway always stripped the worker's om_receipt event,
		// while non-streaming clients get the X-Om-Receipt header). If the CLIENT opted in
		// with X-OM-Receipt-Req, forward the receipt event (capture-for-verification
		// unchanged). Off by default so strict SSE parsers never see an unknown event.
		sseCap.forwardReceipt = r.Header.Get("X-OM-Receipt-Req") != ""
		if sampled {
			sseCap.captureBuf = &bytes.Buffer{}
			sseCap.captureCap = sampleMaxBodyBytes
		}
		if g.streamResume {
			sseCap.filter = true
			sseCap.headerSent = clientStarted
			sseCap.contentBuf = &resumeContent
			sseCap.contentCap = resumeContentCap
		}
		var proxyErr error
		proxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = targetURL.Scheme
				req.URL.Host = targetURL.Host
				req.Host = targetURL.Host
				req.Body = io.NopCloser(bytes.NewReader(curBody))
				req.ContentLength = int64(len(curBody))
				req.Header.Del("Authorization") // never leak the client's gateway key to the worker
				if target.AuthToken != "" {
					req.Header.Set("Authorization", "Bearer "+target.AuthToken) // authenticate to the worker
				}
				req.Header.Set("X-Request-ID", requestID)
				if g.streamResume && target.HasFeature(worker.FeatureReceipt) {
					// A1: request a stream receipt only when the filter is active to
					// capture+strip it (otherwise the event would leak to the client).
					req.Header.Set("X-OM-Receipt-Req", "1")
				}
			},
			Transport: g.streamTransport, // shared, pooled transport (audit fix)
			ModifyResponse: func(resp *http.Response) error {
				if resp.StatusCode == http.StatusServiceUnavailable {
					return errWorkerMining // retry on another worker (nothing sent yet)
				}
				resp.Header.Set("X-Request-ID", requestID)
				resp.Header.Del("Content-Length")
				return nil
			},
			FlushInterval: -1,
			ErrorHandler: func(rw http.ResponseWriter, req *http.Request, perr error) {
				// Record the error only. The post-ServeHTTP logic decides whether to
				// retry (nothing streamed yet) or surface it (already streaming / out
				// of retries) — keeping all client writes in one place.
				proxyErr = perr
			},
		}

		func() {
			g.registry.IncInflight(target.ID)
			defer g.registry.DecInflight(target.ID) // panic-safe (see handleNonStreaming)
			// httputil.ReverseProxy panics http.ErrAbortHandler when the client disconnects
			// mid-stream. Recover it and record it as an interruption so the post-ServeHTTP
			// billing logic still runs (and charges for delivered tokens on a client abort,
			// rather than being skipped by the panic → free inference). Any OTHER panic is
			// genuinely unexpected and is re-raised.
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler {
						if proxyErr == nil {
							proxyErr = http.ErrAbortHandler
						}
					} else {
						panic(rec)
					}
				}
			}()
			proxy.ServeHTTP(sseCap, r)
		}()

		clientStarted = clientStarted || sseCap.wroteHeader
		totalDelivered += sseCap.deliveredCompletion

		// The worker's own [DONE] is the authoritative end-of-stream signal. A read
		// error AFTER it (e.g. a tunnel/socat hop turning the final FIN into RST, seen
		// on the real fs3 path) is connection-teardown noise, not an interruption —
		// treating it as one mis-billed a COMPLETE stream as stream_interrupted ($0).
		if proxyErr != nil && sseCap.heldDone {
			proxyErr = nil
		}

		if proxyErr == nil {
			// Worker stream ended. An SSE error event (mining yield / engine crash mid-
			// generation) is a SERVER-side interruption: with resume enabled the event is
			// being held back — try to continue on another capable worker so the client
			// never sees the break.
			if sseCap.streamError != "" {
				if next, nb, ok := g.tryResume(r, requestID, body, requestModel, tried, target,
					&resumeContent, sseCap, origMaxTokens-totalDelivered, resumes, deadline); ok {
					target, curBody = next, nb
					resumes++
					continue
				}
				sseCap.flushHeld() // resume impossible → restore legacy error+[DONE] wire behavior
			}
			break // fall through to post-processing
		}

		// An error occurred. If nothing has been sent to the client yet, we can retry
		// on a different worker; otherwise the stream is already partially delivered.
		if !clientStarted {
			tried[target.ID] = true
			next, nerr := selectWorkerForModel(g.registry, requestModel, tried, g.modelSwitchLoadFactor)
			if nerr == nil {
				g.logger.Warn("streaming worker failed before any output, retrying on another",
					"request_id", requestID, "worker", target.ID,
					"attempt", attempt+1, "error", proxyErr)
				target = next
				continue
			}
			// None servable right now — wait in the queue for recovery (still before
			// any bytes have been sent), then retry with a fresh slate, bounded by the
			// overall deadline. Only if nothing recovers do we surface a terminal error.
			if w2, werr := g.blockForWorker(r.Context(), requestModel, deadline); werr == nil {
				g.logger.Warn("streaming: no worker now, waited in queue, retrying",
					"request_id", requestID, "attempt", attempt+1)
				tried = make(map[string]bool)
				target = w2
				continue
			}
			// No worker recovered before the deadline — surface a terminal error.
			elapsed := time.Since(t0)
			status := http.StatusBadGateway
			reason := "upstream_error"
			if errors.Is(proxyErr, errWorkerMining) {
				status = http.StatusServiceUnavailable
				reason = "all_retries_failed"
			}
			metrics.GatewayRequestsTotal.WithLabelValues(fmt.Sprintf("%d", status), target.ID).Inc()
			metrics.GatewayRequestDuration.WithLabelValues(target.ID).Observe(elapsed.Seconds())
			g.logger.Warn("streaming request failed, no worker available",
				"request_id", requestID, "worker", target.ID, "reason", reason, "error", proxyErr)
			g.logRequest(RequestRecord{
				RequestID: requestID, Timestamp: t0, APIKeyName: keyInfo.Name,
				Wallet: keyInfo.Wallet, WorkerID: target.ID, Path: r.URL.Path,
				Model: requestModel, Status: status, ErrorReason: reason,
				DurationMs: elapsed.Milliseconds(),
			})
			if status == http.StatusServiceUnavailable {
				w.Header().Set("Retry-After", g.retryAfterEstimate())
			}
			jsonError(w, fmt.Sprintf("upstream error: %v", proxyErr), status)
			return "", tokenUsage{}
		}

		// Bytes already streamed to the client — cannot recover or retry. Distinguish the
		// cause by whether the CLIENT went away: if the inbound request context is
		// canceled, the client chose to disconnect, so bill it for the tokens actually
		// delivered (gateway-metered) — this closes the "abort mid-stream, right before
		// the usage chunk, to get free tokens" hole. If instead an UPSTREAM/worker error
		// interrupted us while the client was still connected (e.g. a mining yield), that
		// is the server's fault → do not bill.
		elapsed := time.Since(t0)
		if r.Context().Err() != nil && totalDelivered > 0 {
			metrics.GatewayRequestsTotal.WithLabelValues("200", target.ID).Inc()
			metrics.GatewayRequestDuration.WithLabelValues(target.ID).Observe(elapsed.Seconds())
			prompt := estimatePromptTokens(body)
			total := prompt + totalDelivered
			g.logger.Warn("client disconnected mid-stream — billing delivered tokens",
				"request_id", requestID, "worker", target.ID,
				"delivered_completion", totalDelivered, "duration_ms", elapsed.Milliseconds())
			metered := tokenUsage{PromptTokens: prompt, CompletionTokens: totalDelivered, TotalTokens: total}
			g.logRequest(RequestRecord{
				RequestID: requestID, Timestamp: t0, APIKeyName: keyInfo.Name,
				Wallet: keyInfo.Wallet, WorkerID: target.ID, Path: r.URL.Path,
				Model: requestModel, Status: 200, ErrorReason: "",
				DurationMs:   elapsed.Milliseconds(),
				PromptTokens: prompt, CompletionTokens: totalDelivered,
				CachedTokens: 0, TotalTokens: total, Resumes: resumes,
			})
			g.retainSample(sampled, requestID, t0, keyInfo, target.ID, requestModel,
				true, 200, body, sseCapBody(sseCap), metered)
			return requestModel, metered
		}
		// SERVER-side abort mid-stream (upstream connection died — hard worker crash,
		// network drop) with the client still connected: same interruption class as an
		// SSE error event, so try a transparent continuation first.
		if next, nb, ok := g.tryResume(r, requestID, body, requestModel, tried, target,
			&resumeContent, sseCap, origMaxTokens-totalDelivered, resumes, deadline); ok {
			target, curBody = next, nb
			resumes++
			continue
		}
		metrics.GatewayRequestsTotal.WithLabelValues("200", target.ID).Inc()
		metrics.GatewayRequestDuration.WithLabelValues(target.ID).Observe(elapsed.Seconds())
		sseCap.flushHeld() // terminate the client stream cleanly in filter mode
		g.logger.Warn("stream interrupted after partial delivery",
			"request_id", requestID, "worker", target.ID,
			"error", proxyErr, "duration_ms", elapsed.Milliseconds())
		g.logRequest(RequestRecord{
			RequestID: requestID, Timestamp: t0, APIKeyName: keyInfo.Name,
			Wallet: keyInfo.Wallet, WorkerID: target.ID, Path: r.URL.Path,
			Model: requestModel, Status: 200, ErrorReason: "stream_interrupted",
			DurationMs:   elapsed.Milliseconds(),
			PromptTokens: sseCap.usage.PromptTokens, CompletionTokens: sseCap.usage.CompletionTokens,
			CachedTokens: sseCap.usage.CachedTokens, TotalTokens: sseCap.usage.TotalTokens,
			Resumes: resumes,
		})
		return "", tokenUsage{}
	}

	elapsed := time.Since(t0)
	status := sseCap.statusCode
	if status == 0 {
		status = 200
	}
	statusStr := fmt.Sprintf("%d", status)
	metrics.GatewayRequestsTotal.WithLabelValues(statusStr, target.ID).Inc()
	metrics.GatewayRequestDuration.WithLabelValues(target.ID).Observe(elapsed.Seconds())

	// Filter mode held back the worker's [DONE] — terminate the client stream now
	// (only when the stream ended without an unresolved interruption; the flushHeld
	// paths above already terminated it otherwise).
	if sseCap.streamError == "" {
		sseCap.finishClean()
	}

	// Bound worker-reported usage to the model's physical limits (byzantine defense).
	sseCap.usage = g.clampUsage(sseCap.usage, requestModel, requestID, target.ID)

	// Tokens to bill. Prefer the worker's final usage chunk (accurate). If it never
	// arrived — client disconnected before it, or usage wasn't requested — but content
	// WAS delivered, fall back to GATEWAY-METERED delivered tokens (prompt estimate +
	// counted content deltas). This bills what was actually delivered and closes the
	// "abort the stream right before the usage chunk to get free tokens" hole. (A
	// server-side interruption — mining/worker error event — is handled below via
	// errorReason and is NOT billed regardless.)
	//
	// A RESUMED stream (B2) is always billed gateway-metered: the final segment's
	// usage chunk counts the re-fed continuation prefix as fresh prompt tokens, which
	// the client must not pay for (the interruption was the server's fault).
	billed := sseCap.usage
	if resumes > 0 {
		billed = tokenUsage{
			PromptTokens:     estimatePromptTokens(body),
			CompletionTokens: totalDelivered,
		}
		billed.TotalTokens = billed.PromptTokens + billed.CompletionTokens
		metrics.GatewayStreamResumes.WithLabelValues("completed").Inc()
	} else if billed.TotalTokens == 0 && totalDelivered > 0 {
		billed.PromptTokens = estimatePromptTokens(body)
		billed.CompletionTokens = totalDelivered
		billed.CachedTokens = 0
		billed.TotalTokens = billed.PromptTokens + billed.CompletionTokens
	}

	// If the stream was interrupted by an error (e.g., mining mid-generation),
	// log it as a warning with the error message.
	if sseCap.streamError != "" {
		g.logger.Warn("stream interrupted",
			"request_id", requestID, "worker", target.ID,
			"error", sseCap.streamError,
			"duration_ms", elapsed.Milliseconds())
	}

	g.logger.Info("gateway request",
		"request_id", requestID, "api_key", keyInfo.Name,
		"worker", target.ID, "path", r.URL.Path, "model", requestModel,
		"status", status, "duration_ms", elapsed.Milliseconds(),
		"stream", true,
		"stream_error", sseCap.streamError != "",
		"resumes", resumes,
		"prompt_tokens", billed.PromptTokens,
		"completion_tokens", billed.CompletionTokens)

	errorReason := ""
	if sseCap.streamError != "" {
		errorReason = "stream_interrupted"
	}

	// A1: verify + attach the stream receipt (captured from the om_receipt event).
	// On a resumed stream the final segment's receipt attests the CONTINUATION
	// request, so its request-hash is checked against curBody (what that worker got).
	var receipt *ReceiptInfo
	if sseCap.receiptJSON != nil {
		var rc ReceiptInfo
		if json.Unmarshal(sseCap.receiptJSON, &rc) == nil {
			verifyReceipt(&rc, target.ReceiptPubkey, curBody, sseCap.usage, resumes == 0)
			if !rc.Verified {
				g.logger.Error("inference receipt FAILED verification (byzantine evidence)",
					"request_id", requestID, "worker", target.ID, "reason", rc.VerifyError)
			}
			receipt = &rc
		}
	} else if g.streamResume && target.HasFeature(worker.FeatureReceipt) {
		metrics.ReceiptsTotal.WithLabelValues("missing").Inc()
	}

	g.logRequest(RequestRecord{
		RequestID: requestID, Timestamp: t0, APIKeyName: keyInfo.Name,
		Wallet: keyInfo.Wallet, WorkerID: target.ID, Path: r.URL.Path,
		Model: requestModel, Status: status, ErrorReason: errorReason,
		DurationMs:   elapsed.Milliseconds(),
		PromptTokens: billed.PromptTokens, CompletionTokens: billed.CompletionTokens,
		CachedTokens: billed.CachedTokens, TotalTokens: billed.TotalTokens,
		Resumes: resumes,
		Receipt: receipt,
	})

	// Retain a sampled fraction (prompt + delivered SSE + claimed model + tokens) for
	// offline SP fraud detection, covering both the clean and server-interrupted cases
	// (content was delivered either way, so both are usable evidence).
	g.retainSample(sampled, requestID, t0, keyInfo, target.ID, requestModel,
		true, status, body, sseCapBody(sseCap), billed)

	// Server-side interruption (mining/worker error event) → NOT billed; release the
	// reservation fully via the caller's defer. A plain client disconnect (no error
	// event) bills the metered `billed` tokens above for what was actually delivered.
	if errorReason != "" {
		return "", tokenUsage{}
	}
	return requestModel, billed
}

// tryResume decides whether a SERVER-side mid-stream interruption can be transparently
// continued on another worker (B2), and if so returns that worker plus the continuation
// body (original request + om_continuation = delivered text + remaining max_tokens).
// ok=false → the caller degrades to the legacy behavior (error event + [DONE]).
func (g *Gateway) tryResume(r *http.Request, requestID string, origBody []byte,
	requestModel string, tried map[string]bool, cur *worker.Worker,
	content *bytes.Buffer, sseCap *sseCaptureWriter, remainingTokens, resumes int,
	deadline time.Time) (*worker.Worker, []byte, bool) {

	if !g.streamResume || resumes >= g.streamMaxResumes {
		if g.streamResume {
			metrics.GatewayStreamResumes.WithLabelValues("gave_up").Inc()
		}
		return nil, nil, false
	}
	// Client already gone / out of time / nothing delivered yet (the pre-first-byte
	// retry path owns that case) / prefix too large to replay faithfully.
	if r.Context().Err() != nil || !time.Now().Before(deadline) ||
		content.Len() == 0 || sseCap.contentOverflow || remainingTokens < 1 {
		metrics.GatewayStreamResumes.WithLabelValues("gave_up").Inc()
		return nil, nil, false
	}

	tried[cur.ID] = true
	var next *worker.Worker
	for {
		cand, err := selectWorkerForModel(g.registry, requestModel, tried, g.modelSwitchLoadFactor)
		if err != nil {
			// Nothing servable this instant — wait in the queue (mining is brief),
			// bounded by the request deadline. A recovered worker without the
			// continuation feature ends the search (old M1 would re-generate from
			// scratch and the client would see duplicated text).
			w2, werr := g.blockForWorker(r.Context(), requestModel, deadline)
			if werr != nil || !w2.HasFeature(worker.FeatureContinuation) {
				metrics.GatewayStreamResumes.WithLabelValues("gave_up").Inc()
				return nil, nil, false
			}
			next = w2
			break
		}
		if !cand.HasFeature(worker.FeatureContinuation) {
			tried[cand.ID] = true
			continue
		}
		next = cand
		break
	}

	nb := withContinuation(origBody, content.String(), remainingTokens)
	if nb == nil {
		metrics.GatewayStreamResumes.WithLabelValues("gave_up").Inc()
		return nil, nil, false
	}
	metrics.GatewayStreamResumes.WithLabelValues("attempt").Inc()
	g.logger.Info("resuming interrupted stream on another worker",
		"request_id", requestID, "from", cur.ID, "to", next.ID,
		"delivered_chars", content.Len(), "remaining_tokens", remainingTokens,
		"resume_no", resumes+1)
	return next, nb, true
}

// retryAfterEstimate returns an HONEST Retry-After for "all workers mining/offline":
// the smallest scheduler-estimated resume time across mining workers (B1 — while
// mining, seconds_until_change is the estimated time to resume), decayed by estimate
// age and clamped to [5, 120]s. Falls back to the legacy fixed 30s when no worker
// exposes an estimate (older schedulers).
func (g *Gateway) retryAfterEstimate() string {
	best := int64(-1)
	now := time.Now()
	for _, w := range g.registry.List() {
		if w.State != worker.StateMining || w.SecondsUntilChange < 0 || w.UntilChangeAt.IsZero() {
			continue
		}
		rem := w.SecondsUntilChange - int64(now.Sub(w.UntilChangeAt).Seconds())
		if rem < 0 {
			rem = 0
		}
		if best < 0 || rem < best {
			best = rem
		}
	}
	if best < 0 {
		return "30"
	}
	if best < 5 {
		best = 5
	}
	if best > 120 {
		best = 120
	}
	return strconv.FormatInt(best, 10)
}

// ErrQueueFull is returned when the request queue has reached max_queue_size.
var ErrQueueFull = errors.New("queue full")

// anyWorkerSupportsModel checks if any registered worker (regardless of state)
// supports or has loaded the given model. Used to fast-reject unsupported models.
func anyWorkerSupportsModel(registry *worker.Registry, model string) bool {
	if model == "" || model == "default" {
		return true
	}
	for _, w := range registry.List() {
		if modelMatches(w.LoadedModel, model) || workerSupportsModel(&w, model) {
			return true
		}
	}
	return false
}

// waitForWorkerForModel blocks until a worker supporting the model becomes available.
func (g *Gateway) waitForWorkerForModel(ctx context.Context, model string) (*worker.Worker, error) {
	return g.waitForWorkerInternal(ctx, model)
}

// blockForWorker waits for a servable worker to appear, re-queuing across
// successive queueTimeout windows until the overall deadline. This lets a request
// ride out an outage that lasts longer than a single queue window (e.g. a worker
// offline for 75s while the other is briefly mining) instead of failing. Returns
// ErrNoWorkerAvailable once the deadline passes, or ErrQueueFull / ctx error.
func (g *Gateway) blockForWorker(ctx context.Context, model string, deadline time.Time) (*worker.Worker, error) {
	for time.Now().Before(deadline) {
		wkr, err := g.waitForWorkerForModel(ctx, model)
		if err == nil {
			return wkr, nil
		}
		if errors.Is(err, ErrNoWorkerAvailable) {
			continue // this queue window elapsed but the deadline hasn't — wait again
		}
		return nil, err // queue full or client disconnected
	}
	return nil, ErrNoWorkerAvailable
}

// waitForWorker blocks until any available worker appears (for backward compat).
func (g *Gateway) waitForWorker(ctx context.Context) (*worker.Worker, error) {
	return g.waitForWorkerInternal(ctx, "default")
}

func (g *Gateway) waitForWorkerInternal(ctx context.Context, model string) (*worker.Worker, error) {
	// Reserve a queue slot atomically: Add first, then check. The previous
	// check-then-Add could let concurrent callers both pass the size check and
	// overshoot max_queue_size (audit fix). If we overshot, give the slot back.
	queued := g.queuedCount.Add(1)
	if g.maxQueueSize > 0 && int(queued) > g.maxQueueSize {
		g.queuedCount.Add(-1)
		g.logger.Warn("request rejected, queue full",
			"queued", queued-1, "max", g.maxQueueSize)
		return nil, ErrQueueFull
	}

	g.logger.Info("all workers busy/mining, request queued",
		"timeout", g.queueTimeout.String(), "queued", queued)
	metrics.GatewayQueuedRequests.Inc()
	defer func() {
		g.queuedCount.Add(-1)
		metrics.GatewayQueuedRequests.Dec()
	}()

	deadline := time.After(g.queueTimeout)
	ticker := time.NewTicker(queuePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, ErrNoWorkerAvailable
		case <-ticker.C:
			if target, err := selectWorkerForModel(g.registry, model, nil, g.modelSwitchLoadFactor); err == nil {
				g.logger.Info("queued request released, worker available", "worker", target.ID)
				return target, nil
			}
		}
	}
}

// handleModels returns the list of models available across all active workers.
func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := g.authenticate(r); !ok {
		jsonError(w, "invalid or missing Authorization header", http.StatusUnauthorized)
		return
	}

	seen := make(map[string]bool)
	var models []map[string]interface{}

	for _, w := range g.registry.List() {
		if w.State != worker.StateIdle && w.State != worker.StateBusy {
			continue
		}
		if w.LoadedModel != "" && !seen[w.LoadedModel] {
			seen[w.LoadedModel] = true
			models = append(models, map[string]interface{}{
				"id":       w.LoadedModel,
				"object":   "model",
				"owned_by": "openmodel",
			})
		}
	}

	if !seen["default"] {
		models = append([]map[string]interface{}{{
			"id":       "default",
			"object":   "model",
			"owned_by": "openmodel",
		}}, models...)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
}

// handleCatalog returns a rich model catalog: per-model pricing (input / output /
// cache-read, USD per 1M tokens) and limits (context window, max output) — the
// data backing a model-listing UI. Prices come from the settlement config
// (model_prices_usd = output/base, model_catalog = input/cache-read + limits);
// availability comes from the live worker registry.
func (g *Gateway) handleCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := g.authenticate(r); !ok {
		jsonError(w, "invalid or missing Authorization header", http.StatusUnauthorized)
		return
	}

	ids := map[string]bool{}
	var prices map[string]string
	var catalog map[string]settlement.ModelInfo
	if g.settlementCfg != nil {
		prices = g.settlementCfg.ModelPricesUSD
		catalog = g.settlementCfg.ModelCatalog
		for m := range prices {
			ids[m] = true
		}
		for m := range catalog {
			ids[m] = true
		}
	}
	// Availability: models loaded/supported by a currently routable worker.
	avail := map[string]bool{}
	for _, wk := range g.registry.List() {
		routable := wk.State == worker.StateIdle || wk.State == worker.StateBusy
		if wk.LoadedModel != "" {
			ids[wk.LoadedModel] = true
			if routable {
				avail[wk.LoadedModel] = true
			}
		}
		for _, sm := range wk.SupportedModels {
			ids[sm] = true
			if routable {
				avail[sm] = true
			}
		}
	}

	priceOf := func(m map[string]string, id string) string {
		if m == nil {
			return ""
		}
		if v, ok := m[id]; ok {
			return v
		}
		return m["default"]
	}
	catOf := func(id string) settlement.ModelInfo {
		if catalog == nil {
			return settlement.ModelInfo{}
		}
		if v, ok := catalog[id]; ok {
			return v
		}
		return catalog["default"]
	}

	out := []map[string]interface{}{}
	for id := range ids {
		if id == "default" { // pricing fallback key, not a real model
			continue
		}
		info := catOf(id)
		input := info.InputUSD
		if input == "" {
			input = priceOf(prices, id) // no explicit input price → fall back to output/base
		}
		out = append(out, map[string]interface{}{
			"id":                          id,
			"input_price_usd_per_1m":      input,
			"output_price_usd_per_1m":     priceOf(prices, id),
			"cache_read_price_usd_per_1m": info.CacheReadUSD,
			"context_window":              info.ContextWindow,
			"max_output":                  info.MaxOutput,
			"available":                   avail[id],
		})
	}
	resp := map[string]interface{}{"object": "list", "data": out}
	// The FIL/USD rate billing currently uses — the same number settlement converts
	// with — so a client can translate the USD prices above into FIL. Omitted when
	// settlement (and with it pricing) is disabled.
	if p := g.balanceChecker.FILPriceUSD(); p != nil && p.Sign() > 0 {
		resp["fil_price_usd"] = p.Text('f', 4)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleUnsupported returns 404 for unsupported /v1/* endpoints.
func (g *Gateway) handleUnsupported(w http.ResponseWriter, r *http.Request) {
	jsonError(w, fmt.Sprintf("endpoint %s is not supported", r.URL.Path), http.StatusNotFound)
}

// authenticate checks the Bearer token and returns the associated key metadata.
func (g *Gateway) authenticate(r *http.Request) (apiKeyEntry, bool) {
	if !g.authEnabled {
		return apiKeyEntry{Name: "anonymous"}, true
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return apiKeyEntry{}, false
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	g.keysMu.RLock()
	entry, ok := g.apiKeys[token]
	g.keysMu.RUnlock()
	return entry, ok
}

// logRequest writes to the persistent request log (if enabled).
func (g *Gateway) logRequest(rec RequestRecord) {
	g.requestLogger.Log(rec)
}

// retainSample records a served request/response pair for offline SP fraud detection
// (A7 phase 1). No-op unless this request was picked for sampling. respRaw is the worker's
// full response body (non-stream) or the captured raw SSE (stream); the claimed served
// model is scanned out of it for offline comparison against requestModel.
func (g *Gateway) retainSample(sampled bool, requestID string, t0 time.Time, keyInfo apiKeyEntry,
	workerID, requestModel string, stream bool, status int, reqBody []byte, respRaw string, u tokenUsage) {
	if !sampled || g.sampler == nil {
		return
	}
	g.sampler.write(VerificationSample{
		RequestID:        requestID,
		Timestamp:        t0.UTC().Format(time.RFC3339),
		WorkerID:         workerID,
		APIKeyName:       keyInfo.Name,
		ModelReq:         requestModel,
		ModelResp:        scanModelField([]byte(respRaw)),
		Stream:           stream,
		Status:           status,
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		CachedTokens:     u.CachedTokens,
		TotalTokens:      u.TotalTokens,
		Request:          string(reqBody),
		Response:         respRaw,
	})
}

// adjustBalance corrects pendingSpend after request completion. Zero usage (failure
// or stream interruption) fully reverses the reservation. The actual cost is priced
// with settlement.CostBreakdownUSD — the IDENTICAL formula (catalog input/output/
// cache-read split) that settlement later clears this pending with via SettleSpend.
// Any other formula here leaves a per-request residue that can never be drained:
// with the flat total×output price, prompt×(output−input) accumulated per request
// until the wallet hit its unsettled-spend cap and got 402'd (audit finding #47).
func (g *Gateway) adjustBalance(wallet string, estimatedCostUSD *math_big.Float, model string, u tokenUsage) {
	if g.balanceChecker == nil || estimatedCostUSD == nil || wallet == "" {
		return
	}
	actualCostUSD := new(math_big.Float)
	if u.TotalTokens > 0 {
		actualCostUSD = settlement.CostBreakdownUSD(model,
			u.PromptTokens, u.CompletionTokens, u.CachedTokens, u.TotalTokens,
			g.modelPrices, g.catalogInput, g.catalogCacheRead)
	}
	g.balanceChecker.Adjust(wallet, estimatedCostUSD, actualCostUSD)
}

// --- Helper functions ---

// generateRequestID creates a short unique ID for request tracing.
func generateRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "req-" + hex.EncodeToString(b)
}

// extractModel extracts the "model" field from a JSON request body.
func extractModel(body []byte) string {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "unknown"
	}
	if m, ok := parsed["model"].(string); ok {
		return m
	}
	return "unknown"
}

// hasTopLevelKey reports whether the JSON object body contains the given top-level key.
// Proper JSON parse (not a substring scan), so mentioning the key inside message TEXT
// does not false-positive.
func hasTopLevelKey(body []byte, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// withContinuation builds the upstream body for a mid-stream continuation (B2): the
// ORIGINAL client body plus om_continuation = the text already delivered, and
// max_tokens reduced to what is still owed. Returns nil if the body cannot be rebuilt.
func withContinuation(orig []byte, prefix string, maxTokens int) []byte {
	var m map[string]interface{}
	if json.Unmarshal(orig, &m) != nil {
		return nil
	}
	m["om_continuation"] = prefix
	m["max_tokens"] = maxTokens
	out, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return out
}

// scanModelField finds the first `"model":"<value>"` in raw bytes and returns the value.
// Unlike extractModel it does not require a single well-formed JSON object, so it works on
// a non-stream response body AND on a concatenated raw SSE stream (each chunk carries the
// served model). Used by verification sampling to record what model the worker CLAIMS it
// served, for offline comparison against the requested model. Returns "" if not found.
func scanModelField(b []byte) string {
	marker := []byte(`"model"`)
	i := bytes.Index(b, marker)
	if i < 0 {
		return ""
	}
	j := i + len(marker)
	for j < len(b) && (b[j] == ' ' || b[j] == ':' || b[j] == '\t') {
		j++
	}
	if j >= len(b) || b[j] != '"' {
		return ""
	}
	j++
	start := j
	for j < len(b) && b[j] != '"' {
		if b[j] == '\\' { // skip escaped char
			j++
		}
		j++
	}
	if j > len(b) {
		return ""
	}
	return string(b[start:j])
}

// tokenUsage holds parsed token counts from the response.
type tokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int // prompt tokens served from the prefix cache (usage.prompt_tokens_details.cached_tokens)
	TotalTokens      int
}

// extractUsage parses usage fields from a JSON response body.
func extractUsage(body []byte) tokenUsage {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return tokenUsage{}
	}
	usage, ok := parsed["usage"].(map[string]interface{})
	if !ok {
		return tokenUsage{}
	}
	cached := 0
	if d, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
		cached = intFromJSON(d["cached_tokens"])
	}
	return tokenUsage{
		PromptTokens:     intFromJSON(usage["prompt_tokens"]),
		CompletionTokens: intFromJSON(usage["completion_tokens"]),
		CachedTokens:     cached,
		TotalTokens:      intFromJSON(usage["total_tokens"]),
	}
}

// clampUsage bounds worker-reported token counts to the model's physical limits from
// the catalog (context_window for input, max_output for output). A legitimate request
// can never exceed these — the model rejects an over-length prompt and stops at
// max_output — so clamping never under-bills real usage; it only caps a
// misreporting/byzantine worker's ability to over-bill the client (and over-earn as
// SP), and logs it for detection. Models with no catalog entry (and no "default") are
// returned unchanged.
func (g *Gateway) clampUsage(u tokenUsage, model, requestID, workerID string) tokenUsage {
	if g.settlementCfg == nil || g.settlementCfg.ModelCatalog == nil {
		return u
	}
	info, ok := g.settlementCfg.ModelCatalog[model]
	if !ok {
		info, ok = g.settlementCfg.ModelCatalog["default"]
	}
	if !ok || info.ContextWindow <= 0 {
		return u
	}
	ctxWin := info.ContextWindow
	maxOut := info.MaxOutput
	if maxOut <= 0 {
		maxOut = ctxWin
	}
	clamped := false
	if u.PromptTokens > ctxWin {
		u.PromptTokens = ctxWin
		clamped = true
	}
	if u.CompletionTokens > maxOut {
		u.CompletionTokens = maxOut
		clamped = true
	}
	if u.CachedTokens > u.PromptTokens {
		u.CachedTokens = u.PromptTokens
		clamped = true
	}
	if maxTotal := ctxWin + maxOut; u.TotalTokens > maxTotal {
		u.TotalTokens = maxTotal
		clamped = true
	}
	if clamped {
		g.logger.Warn("worker-reported token usage exceeded model physical limits — clamped (possible misreporting worker)",
			"request_id", requestID, "worker", workerID, "model", model,
			"context_window", ctxWin, "max_output", maxOut)
	}
	return u
}

func intFromJSON(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

// rewriteModelField replaces the "model" field in a JSON body with the given value.
// Used to normalize the model name to match what M1 currently has loaded,
// avoiding unnecessary model switches due to path format differences.
func rewriteModelField(body []byte, model string) []byte {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}
	parsed["model"] = model
	rewritten, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return rewritten
}

// extractBool extracts a boolean field from a JSON body. Returns false if missing or not JSON.
func extractBool(body []byte, field string) bool {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	v, ok := parsed[field].(bool)
	return ok && v
}

// extractInt extracts an integer field from a JSON body. Returns 0 if missing.
func extractInt(body []byte, field string) int {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0
	}
	if v, ok := parsed[field].(float64); ok {
		return int(v)
	}
	return 0
}

// estimatePromptTokens makes a rough, conservative estimate of the input (prompt) token
// count from the request body, used only to size the up-front balance reservation so a
// large prompt with a tiny max_tokens can't slip past the gate under-reserved. It uses a
// ~4-chars-per-token heuristic over the chat `messages[].content` (or `prompt`) text.
// Over-counting is safe: the reservation is reconciled to the real usage after the
// request completes. Returns 0 on unparseable bodies (the max_tokens estimate still applies).
func estimatePromptTokens(body []byte) int {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0
	}
	chars := 0
	if msgs, ok := parsed["messages"].([]interface{}); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]interface{})
			if !ok {
				continue
			}
			if c, ok := mm["content"].(string); ok {
				chars += len(c)
			}
		}
	}
	switch p := parsed["prompt"].(type) {
	case string:
		chars += len(p)
	case []interface{}:
		for _, s := range p {
			if ss, ok := s.(string); ok {
				chars += len(ss)
			}
		}
	}
	return chars / 4
}

// sseCaptureWriter wraps http.ResponseWriter to pass through SSE chunks
// while scanning each chunk for usage data and error events.
type sseCaptureWriter struct {
	http.ResponseWriter
	statusCode          int
	wroteHeader         bool // true once any header/body has gone to the client (can't retry after)
	usage               tokenUsage
	streamError         string // Non-empty if an error event was detected in the SSE stream
	deliveredCompletion int    // gateway-metered count of non-empty content deltas actually sent to the client
	// Verification sampling (A7): when captureBuf is non-nil this request was picked for
	// retention, so we accumulate the raw SSE bytes delivered to the client (bounded by
	// captureCap) as fraud-detection evidence. nil on the vast majority of requests.
	captureBuf *bytes.Buffer
	captureCap int

	// --- B2 stream-resume filter mode (all zero-valued when stream_resume is off,
	// in which case Write is byte-for-byte passthrough as before) ---
	filter          bool          // hold back error events + [DONE]; accumulate decoded content
	headerSent      bool          // a previous segment already sent the client header → swallow WriteHeader
	pend            []byte        // partial trailing SSE line, forwarded once completed
	heldError       []byte        // suppressed error-event line (flushed only if resume proves impossible)
	heldDone        bool          // saw the worker's data: [DONE] (suppressed; gateway emits its own)
	lastSuppressed  bool          // last processed line was suppressed → also drop its blank terminator
	contentBuf      *bytes.Buffer // decoded content text delivered to the client (shared across segments)
	contentCap      int
	contentOverflow bool // delivered text exceeded contentCap → resume disabled for this request

	// receiptJSON is the raw om_receipt event payload captured from the stream (A1).
	// By default the event is stripped before it reaches the client; when the CLIENT
	// opted in via X-OM-Receipt-Req, forwardReceipt makes the gateway pass the event
	// through as well (F2: streaming parity with the non-streaming X-Om-Receipt header,
	// so streaming spend is independently auditable too). nil when the worker sent none.
	receiptJSON    []byte
	forwardReceipt bool
}

// resumeContentCap bounds the delivered-text buffer kept for continuation. A stream
// longer than this simply loses resumability (contentOverflow) — never truncated,
// because continuing from a truncated prefix would corrupt the visible output.
const resumeContentCap = 256 << 10 // 256 KiB

// sseDelta is the minimal shape needed to decode a streamed content delta.
type sseDelta struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

// processLine handles one complete SSE line in filter mode: suppress [DONE] and error
// events (holding them for a possible flush), forward everything else, and account
// delivered content for metering + continuation.
func (w *sseCaptureWriter) processLine(line []byte, fwd *bytes.Buffer) {
	trim := bytes.TrimSpace(line)
	if len(trim) == 0 {
		// Blank line: SSE event terminator. Drop the one belonging to a suppressed
		// event; forward the rest untouched.
		if w.lastSuppressed {
			w.lastSuppressed = false
			return
		}
		fwd.Write(line)
		return
	}
	if bytes.Equal(trim, []byte("data: [DONE]")) {
		w.heldDone = true
		w.lastSuppressed = true
		return
	}
	if bytes.HasPrefix(trim, []byte("data: ")) {
		payload := trim[6:]
		// Usage chunks pass through (and are captured for billing preference).
		if u := extractUsage(payload); u.TotalTokens > 0 {
			w.usage = u
		}
		// A1 stream receipt event: always capture for gateway-side verification. Strip by
		// default; FORWARD when the client explicitly asked for receipts (F2) so streaming
		// clients can audit their spend like non-streaming ones (X-Om-Receipt header).
		if bytes.Contains(payload, []byte(`"om_receipt"`)) {
			var env struct {
				OmReceipt json.RawMessage `json:"om_receipt"`
			}
			if json.Unmarshal(payload, &env) == nil && env.OmReceipt != nil {
				w.receiptJSON = append([]byte{}, env.OmReceipt...)
				if w.forwardReceipt {
					w.lastSuppressed = false // its blank terminator must pass too
					fwd.Write(line)
					return
				}
				w.lastSuppressed = true
				return
			}
		}
		// Error events are HELD BACK: if a resume succeeds the client never sees the
		// interruption; if it fails, flushHeld restores the legacy wire behavior.
		if bytes.Contains(payload, []byte(`"error"`)) {
			var parsed map[string]interface{}
			if json.Unmarshal(payload, &parsed) == nil {
				if errObj, ok := parsed["error"].(map[string]interface{}); ok {
					if msg, ok := errObj["message"].(string); ok {
						if w.streamError == "" {
							w.streamError = msg
						}
						w.heldError = append([]byte{}, line...)
						w.lastSuppressed = true
						return
					}
				}
			}
		}
		// Content delta: meter it and accumulate the decoded text for continuation.
		// Decoded via real JSON (not a byte marker) so both Go-style compact and
		// Python-style spaced encodings are handled identically.
		var d sseDelta
		if json.Unmarshal(payload, &d) == nil {
			for _, c := range d.Choices {
				if c.Delta.Content == "" {
					continue
				}
				w.deliveredCompletion++
				if w.contentBuf != nil && !w.contentOverflow {
					if w.contentBuf.Len()+len(c.Delta.Content) > w.contentCap {
						w.contentOverflow = true
					} else {
						w.contentBuf.WriteString(c.Delta.Content)
					}
				}
			}
		}
	}
	w.lastSuppressed = false
	fwd.Write(line)
}

// flushHeld restores the legacy wire behavior when an interruption could NOT be
// resumed: emit the held error event (if any) and terminate the stream with [DONE].
func (w *sseCaptureWriter) flushHeld() {
	if !w.filter {
		return
	}
	var out bytes.Buffer
	if len(w.pend) > 0 {
		out.Write(w.pend)
		out.WriteByte('\n')
		w.pend = nil
	}
	if len(w.heldError) > 0 {
		out.Write(w.heldError)
		out.WriteString("\n")
		w.heldError = nil
	}
	out.WriteString("data: [DONE]\n\n")
	w.ResponseWriter.Write(out.Bytes())
	w.Flush()
}

// finishClean terminates a filtered stream that ended without interruption: forward
// any pending tail and emit the gateway's own [DONE] (the worker's was suppressed).
func (w *sseCaptureWriter) finishClean() {
	if !w.filter {
		return
	}
	var out bytes.Buffer
	if len(w.pend) > 0 {
		out.Write(w.pend)
		out.WriteByte('\n')
		w.pend = nil
	}
	out.WriteString("data: [DONE]\n\n")
	w.ResponseWriter.Write(out.Bytes())
	w.Flush()
}

// content delta markers for gateway-side delivered-token metering (cheap byte-count).
// BOTH spacing variants must be counted: Go's json.Marshal emits compact `"content":"…"`,
// but Python's json.dumps (py-inference) emits `"content": "…"` with a space — a
// real-machine verification caught the compact-only marker never matching production
// workers (metering silently read 0 delivered tokens).
var sseContentMarker = []byte(`"content":"`)
var sseEmptyContentMarker = []byte(`"content":""`)
var sseContentMarkerSp = []byte(`"content": "`)
var sseEmptyContentMarkerSp = []byte(`"content": ""`)

// countContentDeltas counts non-empty content deltas in raw SSE bytes, tolerant of
// both JSON spacing styles.
func countContentDeltas(b []byte) int {
	return bytes.Count(b, sseContentMarker) - bytes.Count(b, sseEmptyContentMarker) +
		bytes.Count(b, sseContentMarkerSp) - bytes.Count(b, sseEmptyContentMarkerSp)
}

func (w *sseCaptureWriter) WriteHeader(code int) {
	w.statusCode = code
	w.wroteHeader = true
	// On a RESUMED segment the client already has response headers from the first
	// segment — swallow the duplicate (it would log "superfluous WriteHeader").
	if w.filter && w.headerSent {
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *sseCaptureWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true // an implicit 200 is sent on first Write if WriteHeader wasn't called
	// Verification sampling: retain the raw SSE bytes (bounded) for offline fraud checks.
	if w.captureBuf != nil && w.captureBuf.Len() < w.captureCap {
		if room := w.captureCap - w.captureBuf.Len(); room >= len(b) {
			w.captureBuf.Write(b)
		} else {
			w.captureBuf.Write(b[:room])
		}
	}

	// B2 stream-resume filter mode: line-oriented processing so error events and
	// [DONE] can be held back (making a mid-stream continuation invisible to the
	// client), delivered content is decoded for the continuation prefix, and metering
	// counts only what is actually forwarded. A partial trailing line is buffered
	// until completed — per-event upstream writes make this the rare case.
	if w.filter {
		data := append(w.pend, b...)
		w.pend = nil
		var fwd bytes.Buffer
		for {
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				w.pend = append([]byte{}, data...)
				break
			}
			w.processLine(data[:i+1], &fwd)
			data = data[i+1:]
		}
		if fwd.Len() > 0 {
			if _, err := w.ResponseWriter.Write(fwd.Bytes()); err != nil {
				return len(b), err
			}
		}
		return len(b), nil
	}

	// Gateway-side metering: count non-empty content deltas actually delivered to the
	// client (each ≈ one completion token). This lets us bill what was delivered even if
	// the client disconnects before the final usage chunk — closing the "abort the stream
	// right before usage to get free tokens" hole. Cheap byte-count, no per-token parse.
	if n := countContentDeltas(b); n > 0 {
		w.deliveredCompletion += n
	}
	// Scan SSE data lines for usage and error events
	if bytes.Contains(b, []byte(`"usage"`)) || bytes.Contains(b, []byte(`"error"`)) {
		for _, line := range bytes.Split(b, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data: ")) || bytes.Equal(line, []byte("data: [DONE]")) {
				continue
			}
			payload := line[6:]
			// Check for usage
			u := extractUsage(payload)
			if u.TotalTokens > 0 {
				w.usage = u
			}
			// Check for error event (e.g., "Engine paused during generation")
			if w.streamError == "" && bytes.Contains(payload, []byte(`"error"`)) {
				var parsed map[string]interface{}
				if json.Unmarshal(payload, &parsed) == nil {
					if errObj, ok := parsed["error"].(map[string]interface{}); ok {
						if msg, ok := errObj["message"].(string); ok {
							w.streamError = msg
						}
					}
				}
			}
		}
	}
	// Always pass through to client
	return w.ResponseWriter.Write(b)
}

func (w *sseCaptureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// sseCapBody returns the raw SSE bytes retained for verification sampling, or "" when this
// request wasn't sampled (captureBuf nil).
func sseCapBody(w *sseCaptureWriter) string {
	if w == nil || w.captureBuf == nil {
		return ""
	}
	return w.captureBuf.String()
}

// hopByHopHeaders are connection-scoped headers that must NOT be forwarded from the
// upstream worker response to the client (per RFC 7230 §6.1).
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	// Content-Length is recomputed by the client writer from the body we send.
	"Content-Length": true,
}

// copyResponseHeaders copies non-hop-by-hop headers from the upstream worker response
// into the client response (audit fix: the non-streaming path previously dropped them).
func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopByHopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func jsonError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"message": message,
			"type":    "gateway_error",
		},
	})
}
