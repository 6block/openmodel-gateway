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
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/metrics"
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
	authEnabled           bool
	requestTimeout        time.Duration
	queueTimeout          time.Duration // Max time to wait in queue for an available worker
	maxQueueSize          int           // Max queued requests; 0 = unlimited
	modelSwitchLoadFactor float64       // Trigger model switch when loaded worker overloaded; 0 = disabled
	queuedCount           atomic.Int32  // Current number of queued requests
	requestLogger         *RequestLogger
	logger                *slog.Logger
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

	return &Gateway{
		registry:       registry,
		apiKeys:        keys,
		authEnabled:    len(keys) > 0,
		requestTimeout:        timeout,
		queueTimeout:          queueTimeout,
		maxQueueSize:          cfg.MaxQueueSize,
		modelSwitchLoadFactor: cfg.ModelSwitchLoadFactor,
		requestLogger:         NewRequestLogger(cfg.RequestLogPath, logger),
		logger:                logger,
	}
}

// Close releases resources (request log file).
func (g *Gateway) Close() error {
	return g.requestLogger.Close()
}

// Handler returns an http.Handler for the gateway.
// Mount this on the gateway HTTP server (port 3000).
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", g.handleProxy)
	mux.HandleFunc("/v1/completions", g.handleProxy)
	mux.HandleFunc("/v1/models", g.handleModels)
	// Catch-all for unsupported /v1/* endpoints
	mux.HandleFunc("/v1/", g.handleUnsupported)
	return mux
}

// handleProxy is the core reverse proxy handler for inference requests.
func (g *Gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

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

	// Read request body first (need model name for routing)
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		jsonError(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	requestModel := extractModel(body)
	isStreaming := extractBool(body, "stream")

	// Select a worker with model-aware routing.
	// Priority: loaded model > supported model > any (for "default")
	target, err := selectWorkerForModel(g.registry, requestModel, nil, g.modelSwitchLoadFactor)
	if err != nil {
		// Only queue if workers exist but are mining/offline.
		// If no worker supports the model at all, reject immediately.
		if !anyWorkerSupportsModel(g.registry, requestModel) {
			metrics.GatewayRequestsTotal.WithLabelValues("404", "none").Inc()
			jsonError(w, fmt.Sprintf("no worker supports model %q", requestModel), http.StatusNotFound)
			return
		}
		target, err = g.waitForWorkerForModel(r.Context(), requestModel)
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
			w.Header().Set("Retry-After", "30")
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
	}

	t0 := time.Now()

	if !isStreaming {
		g.handleNonStreaming(w, r, body, target, requestID, requestModel, keyInfo, t0)
		return
	}
	g.handleStreaming(w, r, body, target, requestID, requestModel, keyInfo, t0)
}

// handleNonStreaming forwards a non-streaming request to a worker.
// If the worker returns 503 (mining mid-generation), retries on a different worker.
func (g *Gateway) handleNonStreaming(w http.ResponseWriter, r *http.Request, body []byte,
	firstTarget *worker.Worker, requestID, requestModel string, keyInfo apiKeyEntry, t0 time.Time) {

	tried := make(map[string]bool)
	target := firstTarget
	maxRetries := 3

	for attempt := 0; attempt < maxRetries; attempt++ {
		respBody, status, workerID, err := g.forwardRequest(r.URL.Path, body, target, requestID)

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
			usage := extractUsage(respBody)
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
				TotalTokens: usage.TotalTokens,
			})

			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Request-ID", requestID)
			w.WriteHeader(status)
			w.Write(respBody)
			return
		}

		// Pick a different worker for retry
		next, err := selectWorkerForModel(g.registry, requestModel, tried, g.modelSwitchLoadFactor)
		if err != nil {
			break // No other workers available
		}
		target = next
	}

	// All retries exhausted
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

	w.Header().Set("Retry-After", "30")
	jsonError(w, "all workers returned errors or are mining", http.StatusServiceUnavailable)
}

// forwardRequest sends the request body to a worker and returns the full response.
// Does NOT write to the client — caller decides what to do with the response.
func (g *Gateway) forwardRequest(path string, body []byte, target *worker.Worker, requestID string) (
	respBody []byte, status int, workerID string, err error) {

	workerID = target.ID
	reqURL := strings.TrimRight(target.Endpoint, "/") + path

	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, workerID, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", requestID)

	client := &http.Client{Timeout: g.requestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, workerID, err
	}
	defer resp.Body.Close()

	respBody, err = io.ReadAll(io.LimitReader(resp.Body, maxRequestBody))
	if err != nil {
		return nil, resp.StatusCode, workerID, err
	}
	return respBody, resp.StatusCode, workerID, nil
}

// handleStreaming forwards a streaming request via reverse proxy with SSE passthrough.
func (g *Gateway) handleStreaming(w http.ResponseWriter, r *http.Request, body []byte,
	target *worker.Worker, requestID, requestModel string, keyInfo apiKeyEntry, t0 time.Time) {

	targetURL, err := url.Parse(target.Endpoint)
	if err != nil {
		jsonError(w, "internal routing error", http.StatusBadGateway)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.Host = targetURL.Host
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			req.Header.Del("Authorization")
			req.Header.Set("X-Request-ID", requestID)
		},
		Transport: &http.Transport{
			ResponseHeaderTimeout: g.requestTimeout,
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Set("X-Request-ID", requestID)
			resp.Header.Del("Content-Length")
			return nil
		},
		FlushInterval: -1,
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
			elapsed := time.Since(t0)
			metrics.GatewayRequestsTotal.WithLabelValues("502", target.ID).Inc()
			metrics.GatewayRequestDuration.WithLabelValues(target.ID).Observe(elapsed.Seconds())
			g.logger.Error("gateway stream proxy error",
				"request_id", requestID, "worker", target.ID, "error", proxyErr)
			g.logRequest(RequestRecord{
				RequestID: requestID, Timestamp: t0, APIKeyName: keyInfo.Name,
				Wallet: keyInfo.Wallet, WorkerID: target.ID, Path: r.URL.Path,
				Model: requestModel, Status: 502, DurationMs: elapsed.Milliseconds(),
			})
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(rw).Encode(map[string]interface{}{
				"error": map[string]string{
					"message": fmt.Sprintf("upstream error: %v", proxyErr),
					"type":    "upstream_error",
				},
			})
		},
	}

	sseCap := &sseCaptureWriter{ResponseWriter: w}
	proxy.ServeHTTP(sseCap, r)

	elapsed := time.Since(t0)
	status := sseCap.statusCode
	if status == 0 {
		status = 200
	}
	statusStr := fmt.Sprintf("%d", status)
	metrics.GatewayRequestsTotal.WithLabelValues(statusStr, target.ID).Inc()
	metrics.GatewayRequestDuration.WithLabelValues(target.ID).Observe(elapsed.Seconds())

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
		"prompt_tokens", sseCap.usage.PromptTokens,
		"completion_tokens", sseCap.usage.CompletionTokens)

	errorReason := ""
	if sseCap.streamError != "" {
		errorReason = "stream_interrupted"
	}

	g.logRequest(RequestRecord{
		RequestID: requestID, Timestamp: t0, APIKeyName: keyInfo.Name,
		Wallet: keyInfo.Wallet, WorkerID: target.ID, Path: r.URL.Path,
		Model: requestModel, Status: status, ErrorReason: errorReason,
		DurationMs: elapsed.Milliseconds(),
		PromptTokens: sseCap.usage.PromptTokens, CompletionTokens: sseCap.usage.CompletionTokens,
		TotalTokens: sseCap.usage.TotalTokens,
	})
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

// waitForWorker blocks until any available worker appears (for backward compat).
func (g *Gateway) waitForWorker(ctx context.Context) (*worker.Worker, error) {
	return g.waitForWorkerInternal(ctx, "default")
}

func (g *Gateway) waitForWorkerInternal(ctx context.Context, model string) (*worker.Worker, error) {
	// Check queue size limit before entering
	if g.maxQueueSize > 0 {
		current := int(g.queuedCount.Load())
		if current >= g.maxQueueSize {
			g.logger.Warn("request rejected, queue full",
				"queued", current, "max", g.maxQueueSize)
			return nil, ErrQueueFull
		}
	}

	queued := g.queuedCount.Add(1)
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
	entry, ok := g.apiKeys[token]
	return entry, ok
}

// logRequest writes to the persistent request log (if enabled).
func (g *Gateway) logRequest(rec RequestRecord) {
	g.requestLogger.Log(rec)
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

// tokenUsage holds parsed token counts from the response.
type tokenUsage struct {
	PromptTokens     int
	CompletionTokens int
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
	return tokenUsage{
		PromptTokens:     intFromJSON(usage["prompt_tokens"]),
		CompletionTokens: intFromJSON(usage["completion_tokens"]),
		TotalTokens:      intFromJSON(usage["total_tokens"]),
	}
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

// sseCaptureWriter wraps http.ResponseWriter to pass through SSE chunks
// while scanning each chunk for usage data and error events.
type sseCaptureWriter struct {
	http.ResponseWriter
	statusCode   int
	usage        tokenUsage
	streamError  string // Non-empty if an error event was detected in the SSE stream
}

func (w *sseCaptureWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *sseCaptureWriter) Write(b []byte) (int, error) {
	// Scan SSE data lines for usage and error events
	if bytes.Contains(b, []byte(`"usage"`) ) || bytes.Contains(b, []byte(`"error"`)) {
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

// responseCaptureWriter wraps http.ResponseWriter to capture the response body
// for post-processing (token usage extraction). Passes through all writes.
type responseCaptureWriter struct {
	http.ResponseWriter
	statusCode int
	body       []byte
}

func (w *responseCaptureWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseCaptureWriter) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return w.ResponseWriter.Write(b)
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
