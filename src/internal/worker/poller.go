package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"openmodel/sp-state-agent/internal/metrics"
)

// Poller periodically polls each registered SP worker's M1 endpoints
// to collect their state (GPU state from go-scheduler, engine state from py-inference).
type Poller struct {
	registry         *Registry
	client           *http.Client
	logger           *slog.Logger
	interval         time.Duration
	failureThreshold int // Consecutive poll failures before marking offline
	onChange         func(workerID string, oldState, newState WorkerState)
}

// NewPoller creates a new worker state poller.
// failureThreshold: mark worker offline only after this many consecutive poll failures.
// Recommended value: 3 (filters out transient network blips).
func NewPoller(registry *Registry, interval time.Duration, failureThreshold int, logger *slog.Logger) *Poller {
	if failureThreshold <= 0 {
		failureThreshold = 3
	}
	return &Poller{
		registry: registry,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		logger:           logger,
		interval:         interval,
		failureThreshold: failureThreshold,
	}
}

// SetOnChange registers a callback for state transitions.
func (p *Poller) SetOnChange(fn func(workerID string, oldState, newState WorkerState)) {
	p.onChange = fn
}

// SetPollTimeout overrides the per-poll HTTP timeout (default 5s). Used to raise the
// timeout for workers reached over the public internet (higher RTT/jitter). A value
// <= 0 is ignored (keeps the current timeout).
func (p *Poller) SetPollTimeout(d time.Duration) {
	if d > 0 {
		p.client.Timeout = d
	}
}

// PollNow polls a single worker immediately, bypassing the periodic schedule.
// Used by the webhook handler to react to push notifications from M1.
// No-op if the worker ID is not registered.
func (p *Poller) PollNow(ctx context.Context, workerID string) {
	w, ok := p.registry.Get(workerID)
	if !ok {
		p.logger.Debug("PollNow: unknown worker", "id", workerID)
		return
	}
	p.pollWorker(ctx, *w)
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	// Poll immediately on start
	p.pollAll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollAll(ctx)
		}
	}
}

// pollAll polls all registered workers concurrently.
func (p *Poller) pollAll(ctx context.Context) {
	workers := p.registry.List()
	if len(workers) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w Worker) {
			defer wg.Done()
			p.pollWorker(ctx, w)
		}(w)
	}
	wg.Wait()
}

// pollWorker polls a single worker's go-scheduler and py-inference endpoints.
// On failure, increments consecutive failure counter; marks offline only after threshold.
func (p *Poller) pollWorker(ctx context.Context, w Worker) {
	t0 := time.Now()
	gpuState, untilChange, err := p.fetchGPUState(ctx, w.SchedulerURL, w.AuthToken)
	metrics.PollDuration.WithLabelValues("go-scheduler").Observe(time.Since(t0).Seconds())
	if err != nil {
		metrics.PollTotal.WithLabelValues("error").Inc()
		p.handlePollFailure(w.ID, "go-scheduler", err)
		return
	}

	t1 := time.Now()
	engineState, activeRequests, loadedModel, engineCount, features, receiptPubkey, err := p.fetchInferenceHealth(ctx, w.Endpoint, w.AuthToken)
	metrics.PollDuration.WithLabelValues("py-inference").Observe(time.Since(t1).Seconds())
	if err != nil {
		metrics.PollTotal.WithLabelValues("error").Inc()
		p.handlePollFailure(w.ID, "py-inference", err)
		return
	}
	metrics.PollTotal.WithLabelValues("success").Inc()

	// B1 predictive routing: record how long the scheduler expects the current gpu
	// state to last (yield-in for servable workers / resume-in for mining ones).
	p.registry.SetUntilChange(w.ID, untilChange)

	// Record advertised capabilities (transient runtime state, refreshed every poll).
	p.registry.SetFeatures(w.ID, features, receiptPubkey)

	oldState, newState, changed := p.registry.UpdateState(
		w.ID, gpuState, engineState, activeRequests, loadedModel, engineCount,
	)
	if changed {
		p.logger.Info("worker state changed",
			"worker", w.ID,
			"old_state", oldState,
			"new_state", newState,
			"gpu_state", gpuState,
			"engine_state", engineState,
		)
		if p.onChange != nil {
			p.onChange(w.ID, oldState, newState)
		}
	}
}

// handlePollFailure records a poll failure and marks the worker offline
// only after failureThreshold consecutive failures.
func (p *Poller) handlePollFailure(workerID, source string, err error) {
	shouldOffline, failures := p.registry.RecordPollFailure(workerID, p.failureThreshold)

	p.logger.Debug("poll failure recorded",
		"worker", workerID,
		"source", source,
		"consecutive_failures", failures,
		"threshold", p.failureThreshold,
		"error", err,
	)

	if !shouldOffline {
		return
	}

	oldState, changed := p.registry.MarkOffline(workerID)
	if changed {
		p.logger.Warn("worker marked offline after consecutive failures",
			"worker", workerID,
			"failures", failures,
			"last_error", err.Error(),
		)
		if p.onChange != nil {
			p.onChange(workerID, oldState, StateOffline)
		}
	}
}

// fetchGPUState calls the go-scheduler /ready endpoint and parses the gpu_state.
// Response format: "ready, gpu_state=GPU_STATE_AVAILABLE\n"
func (p *Poller) fetchGPUState(ctx context.Context, schedulerURL, token string) (string, int64, error) {
	url := strings.TrimRight(schedulerURL, "/") + "/ready"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", -1, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", -1, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", -1, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", -1, err
	}

	// Parse "ready[, seconds_until_change=N], gpu_state=GPU_STATE_AVAILABLE".
	// seconds_until_change (B1) precedes gpu_state so this parser — which takes
	// everything after "gpu_state=" — works against both old and new schedulers.
	line := strings.TrimSpace(string(body))
	untilChange := int64(-1)
	if i := strings.Index(line, "seconds_until_change="); i >= 0 {
		rest := line[i+len("seconds_until_change="):]
		if j := strings.IndexAny(rest, ", \n"); j >= 0 {
			rest = rest[:j]
		}
		if v, perr := strconv.ParseInt(rest, 10, 64); perr == nil {
			untilChange = v
		}
	}
	parts := strings.SplitN(line, "gpu_state=", 2)
	if len(parts) != 2 {
		return "", -1, fmt.Errorf("unexpected /ready format: %q", line)
	}
	return strings.TrimSpace(parts[1]), untilChange, nil
}

// inferenceHealthResponse is the JSON response from py-inference /health.
type inferenceHealthResponse struct {
	Status         string        `json:"status"`
	EngineState    string        `json:"engine_state"`
	ActiveRequests int           `json:"active_requests"`
	LoadedModel    string        `json:"loaded_model"`
	MultiGPU       *multiGPUInfo `json:"multi_gpu,omitempty"`
	// Features are capability flags the worker advertises (e.g. "continuation" =
	// understands om_continuation for B2 stream resume). Absent on older workers →
	// the gateway simply never uses the newer behaviors against them.
	Features []string `json:"features,omitempty"`
	// ReceiptPubkey is the worker's hex ed25519 receipt-signing pubkey (A1).
	ReceiptPubkey string `json:"receipt_pubkey,omitempty"`
}

type multiGPUInfo struct {
	Mode        string `json:"mode"`
	EngineCount int    `json:"engine_count"`
}

// fetchInferenceHealth calls the py-inference /health endpoint.
func (p *Poller) fetchInferenceHealth(ctx context.Context, endpoint, token string) (engineState string, activeRequests int, loadedModel string, engineCount int, features []string, receiptPubkey string, err error) {
	url := strings.TrimRight(endpoint, "/") + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, "", 0, nil, "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", 0, "", 0, nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", 0, "", 0, nil, "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var health inferenceHealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return "", 0, "", 0, nil, "", err
	}

	engineCount = 1 // default: single engine
	if health.MultiGPU != nil && health.MultiGPU.EngineCount > 0 {
		engineCount = health.MultiGPU.EngineCount
	}

	return health.EngineState, health.ActiveRequests, health.LoadedModel, engineCount, health.Features, health.ReceiptPubkey, nil
}
