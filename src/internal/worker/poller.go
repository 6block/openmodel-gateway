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
	"openmodel/sp-state-agent/internal/workermtls"
)

// unreachableLogEvery paces the "still unreachable" WARN for a worker that
// stays offline: at the default 5s poll interval, one line every ~10 minutes.
const unreachableLogEvery = 120

// Poller periodically polls each registered SP worker's M1 endpoints
// to collect their state (GPU state from go-scheduler, engine state from py-inference).
type Poller struct {
	registry         *Registry
	client           *http.Client
	mtls             *workermtls.Factory // nil/disabled → p.client (plaintext) unchanged
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

// SetWorkerMTLS arms polling with the gateway's client certificate. Only https
// worker endpoints engage it; http endpoints keep using the shared plain client.
func (p *Poller) SetWorkerMTLS(f *workermtls.Factory) { p.mtls = f }

// clientFor returns the poll client for one worker: identity-pinned mTLS when
// material is loaded, the shared plaintext client otherwise.
func (p *Poller) clientFor(workerID string) *http.Client {
	if p.mtls.Enabled() {
		return p.mtls.ClientFor(workerID, p.client.Timeout, nil)
	}
	return p.client
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
	gpuState, untilChange, err := p.fetchGPUState(ctx, w.ID, w.SchedulerURL, w.AuthToken)
	metrics.PollDuration.WithLabelValues("go-scheduler").Observe(time.Since(t0).Seconds())
	if err != nil {
		metrics.PollTotal.WithLabelValues("error").Inc()
		p.handlePollFailure(w.ID, "go-scheduler", err)
		return
	}

	t1 := time.Now()
	engineState, activeRequests, loadedModel, engineCount, features, receiptPubkey, loadedModels, mgpu, err := p.fetchInferenceHealth(ctx, w.ID, w.Endpoint, w.AuthToken)
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
	// After UpdateState so the primary model it just recorded is folded in.
	p.registry.SetLoadedModels(w.ID, loadedModels)
	capacity, active, notReady := poolMaps(mgpu)
	p.registry.SetPools(w.ID, capacity, active, notReady)
	p.registry.SetPoolGroups(w.ID, poolGroups(mgpu))
	p.registry.SetModelLimits(w.ID, modelLimits(mgpu, loadedModel))
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

	if shouldOffline {
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
			return
		}
	}
	// No state transition to announce — which is exactly the silent case: a
	// worker that has never been reachable STARTS offline (RecordPollFailure
	// then never recommends the transition), so its failures used to be
	// visible only at Debug level. Surface the first threshold crossing and
	// then roughly every ten minutes, with the actual error.
	if failures == p.failureThreshold || (failures > p.failureThreshold && failures%unreachableLogEvery == 0) {
		p.logger.Warn("worker still unreachable",
			"worker", workerID,
			"source", source,
			"consecutive_failures", failures,
			"error", err.Error(),
		)
	}
}

// fetchGPUState calls the go-scheduler /ready endpoint and parses the gpu_state.
// Response format: "ready, gpu_state=GPU_STATE_AVAILABLE\n"
func (p *Poller) fetchGPUState(ctx context.Context, workerID, schedulerURL, token string) (string, int64, error) {
	url := strings.TrimRight(schedulerURL, "/") + "/ready"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", -1, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := p.clientFor(workerID).Do(req)
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
	Status         string `json:"status"`
	EngineState    string `json:"engine_state"`
	ActiveRequests int    `json:"active_requests"`
	LoadedModel    string `json:"loaded_model"`
	// LoadedModels: every resident model on a multi-pool worker. Absent on older
	// workers, which then count as having exactly LoadedModel.
	LoadedModels []string      `json:"loaded_models,omitempty"`
	MultiGPU     *multiGPUInfo `json:"multi_gpu,omitempty"`
	// Limits: request limits of a single-engine worker's model (a multi-pool worker
	// reports them per pool). Absent on older workers → no limit known.
	Limits ModelLimit `json:"limits"`
	// Features are capability flags the worker advertises (e.g. "continuation" =
	// understands om_continuation for B2 stream resume). Absent on older workers →
	// the gateway simply never uses the newer behaviors against them.
	Features []string `json:"features,omitempty"`
	// ReceiptPubkey is the worker's hex ed25519 receipt-signing pubkey (A1).
	ReceiptPubkey string `json:"receipt_pubkey,omitempty"`
}

type multiGPUInfo struct {
	// single carries a single-engine worker's top-level limits (set after decoding).
	single      ModelLimit
	Mode        string     `json:"mode"`
	EngineCount int        `json:"engine_count"`
	Pools       []poolInfo `json:"pools,omitempty"`
}

type poolInfo struct {
	SupportedModels []string  `json:"supported_models"`
	EngineState    string     `json:"engine_state"`
	Limits         ModelLimit `json:"limits"`
	Models         []string `json:"models"`
	Devices        []int    `json:"devices"`
	ActiveRequests int      `json:"active_requests"`
}

// poolMaps turns a multi-pool /health into per-model capacity (GPUs of the serving
// pool) and active requests. nil for a single-engine worker.
func poolMaps(m *multiGPUInfo) (capacity, active map[string]int, notReady map[string]bool) {
	if m == nil || len(m.Pools) == 0 {
		return nil, nil, nil
	}
	capacity, active, notReady = map[string]int{}, map[string]int{}, map[string]bool{}
	for _, p := range m.Pools {
		if p.EngineState != "" && p.EngineState != "running" {
			for _, model := range p.Models {
				notReady[model] = true
			}
		}
		n := len(p.Devices)
		if n < 1 {
			n = 1
		}
		for _, model := range p.Models {
			capacity[model] = n
			active[model] = p.ActiveRequests
		}
	}
	return capacity, active, notReady
}

// fetchInferenceHealth calls the py-inference /health endpoint.
func (p *Poller) fetchInferenceHealth(ctx context.Context, workerID, endpoint, token string) (engineState string, activeRequests int, loadedModel string, engineCount int, features []string, receiptPubkey string, loadedModels []string, pools *multiGPUInfo, err error) {
	url := strings.TrimRight(endpoint, "/") + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, "", 0, nil, "", nil, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := p.clientFor(workerID).Do(req)
	if err != nil {
		return "", 0, "", 0, nil, "", nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", 0, "", 0, nil, "", nil, nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var health inferenceHealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return "", 0, "", 0, nil, "", nil, nil, err
	}

	engineCount = 1 // default: single engine
	if health.MultiGPU != nil && health.MultiGPU.EngineCount > 0 {
		engineCount = health.MultiGPU.EngineCount
	}

	if health.MultiGPU == nil {
		health.MultiGPU = &multiGPUInfo{}
	}
	health.MultiGPU.single = health.Limits
	return health.EngineState, health.ActiveRequests, health.LoadedModel, engineCount, health.Features, health.ReceiptPubkey, health.LoadedModels, health.MultiGPU, nil
}

// modelLimits maps each resident model to its reported request limits: per pool on a
// multi-pool worker, the top-level limits for a single-engine worker's model. nil when
// nothing was reported (older workers) — the gateway then assumes no limit.
func modelLimits(m *multiGPUInfo, loadedModel string) map[string]ModelLimit {
	if m == nil {
		return nil
	}
	out := map[string]ModelLimit{}
	for _, p := range m.Pools {
		if p.Limits == (ModelLimit{}) {
			continue
		}
		for _, model := range p.Models {
			out[model] = p.Limits
		}
	}
	if len(m.Pools) == 0 && m.single != (ModelLimit{}) && loadedModel != "" {
		out[loadedModel] = m.single
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// poolGroups lists, per pool of a multi-pool worker, every model that pool holds or
// may switch to. A switch in one pool takes exactly that group offline on the worker;
// nil for a single-engine worker (a switch there takes the whole worker).
func poolGroups(m *multiGPUInfo) [][]string {
	if m == nil || len(m.Pools) < 2 {
		return nil
	}
	var out [][]string
	for _, p := range m.Pools {
		g := append(append([]string{}, p.Models...), p.SupportedModels...)
		if len(g) > 0 {
			out = append(out, g)
		}
	}
	return out
}
