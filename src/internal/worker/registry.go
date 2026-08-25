package worker

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

// validWorkerID matches: letters, digits, hyphens, underscores. 1-64 chars.
var validWorkerID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// defaultLoadingTimeout bounds how long a worker may stay in the "loading" state
// before it is treated as offline. The slowest real model switch (8-GPU
// multi-instance) is ~2.5 min, so 5 min leaves headroom while still catching a hung
// switch instead of leaving the worker forever "about to be available".
const defaultLoadingTimeout = 5 * time.Minute

// Registry manages the set of registered SP workers.
type Registry struct {
	mu       sync.RWMutex
	workers  map[string]*Worker
	inflight map[string]int // gateway-tracked in-flight request count per worker ID
	logger   *slog.Logger
	savePath string // JSON file for persistence

	loadingTimeout time.Duration // stuck-loading → offline threshold
	admissionGate  atomic.Bool   // probe admission gate (routing trusts verified_models)
}

// NewRegistry creates a new worker registry.
// If savePath is non-empty, worker registrations are persisted to that file.
func NewRegistry(logger *slog.Logger, savePath string) *Registry {
	r := &Registry{
		workers:        make(map[string]*Worker),
		inflight:       make(map[string]int),
		logger:         logger,
		savePath:       savePath,
		loadingTimeout: defaultLoadingTimeout,
	}
	if savePath != "" {
		r.loadFromDisk()
	}
	return r
}

// SetLoadingTimeout overrides the stuck-loading → offline threshold (used by tests).
func (r *Registry) SetLoadingTimeout(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadingTimeout = d
}

// IncInflight records that the gateway dispatched a request to this worker.
func (r *Registry) IncInflight(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inflight[id]++
}

// DecInflight records that a dispatched request to this worker completed.
func (r *Registry) DecInflight(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight[id] > 0 {
		r.inflight[id]--
	}
	if r.inflight[id] == 0 {
		delete(r.inflight, id)
	}
}

// RegisterResult describes what happened during Register.
type RegisterResult struct {
	Worker          *Worker
	Created         bool   // true if this is a new registration
	EndpointChanged bool   // true if existing worker's endpoint was updated
	OldEndpoint     string // previous endpoint value, if EndpointChanged
}

// Register adds or updates a worker in the registry.
// Returns RegisterResult so the caller can propagate endpoint changes to new-api.
func (r *Registry) Register(reg WorkerRegistration) (*RegisterResult, error) {
	if reg.ID == "" {
		return nil, fmt.Errorf("worker ID is required")
	}
	if !validWorkerID.MatchString(reg.ID) {
		return nil, fmt.Errorf("worker ID must be 1-64 alphanumeric characters, hyphens, underscores, or dots")
	}
	if reg.Endpoint == "" {
		return nil, fmt.Errorf("endpoint is required")
	}
	if u, err := url.Parse(reg.Endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("endpoint must be a valid URL (e.g. http://host:port)")
	}
	if reg.SchedulerURL == "" {
		return nil, fmt.Errorf("scheduler_url is required")
	}
	if u, err := url.Parse(reg.SchedulerURL); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("scheduler_url must be a valid URL (e.g. http://host:port)")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	result := &RegisterResult{}
	w, exists := r.workers[reg.ID]
	if exists {
		result.OldEndpoint = w.Endpoint
		result.EndpointChanged = w.Endpoint != reg.Endpoint

		// Update existing worker
		w.Endpoint = reg.Endpoint
		w.SchedulerURL = reg.SchedulerURL
		w.GPUCount = reg.GPUCount
		w.MinerAddress = reg.MinerAddress
		w.AuthToken = reg.AuthToken
		if len(reg.SupportedModels) > 0 {
			w.SupportedModels = reg.SupportedModels
		}
		// Payout is only ever set through the miner-signed self-registration path;
		// an admin re-register without it must not wipe the signed value (settlement
		// attribution would silently fall back to static config).
		if reg.PayoutAddress != "" {
			w.PayoutAddress = reg.PayoutAddress
		}
		if reg.SelfRegistered {
			w.SelfRegistered = true
		}
		// VerifiedModels/VerifiedAt are deliberately NOT touched here: a routine
		// restart re-registers, and forgetting the proven list would take the
		// worker out of rotation for a full re-verification pass.

		if result.EndpointChanged {
			r.logger.Info("worker endpoint changed",
				"id", reg.ID,
				"old_endpoint", result.OldEndpoint,
				"new_endpoint", reg.Endpoint,
			)
		} else {
			r.logger.Info("worker updated", "id", reg.ID)
		}
	} else {
		result.Created = true
		w = &Worker{
			ID:              reg.ID,
			Endpoint:        reg.Endpoint,
			SchedulerURL:    reg.SchedulerURL,
			State:           StateOffline,
			GPUCount:        reg.GPUCount,
			MinerAddress:    reg.MinerAddress,
			AuthToken:       reg.AuthToken,
			SupportedModels: reg.SupportedModels,
			PayoutAddress:   reg.PayoutAddress,
			SelfRegistered:  reg.SelfRegistered,
			RegisteredAt:    time.Now(),
		}
		r.workers[reg.ID] = w
		r.logger.Info("worker registered", "id", reg.ID, "endpoint", reg.Endpoint)
	}

	result.Worker = w
	r.saveToDiskLocked()
	return result, nil
}

// Deregister removes a worker from the registry.
func (r *Registry) Deregister(id string) (*Worker, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.workers[id]
	if !ok {
		return nil, false
	}
	delete(r.workers, id)
	r.logger.Info("worker deregistered", "id", id)
	r.saveToDiskLocked()
	return w, true
}

// Get returns a worker by ID.
func (r *Registry) Get(id string) (*Worker, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	w, ok := r.workers[id]
	if !ok {
		return nil, false
	}
	copy := *w
	copy.InFlight = r.inflight[id]
	return &copy, true
}

// List returns a copy of all registered workers.
func (r *Registry) List() []Worker {
	r.mu.RLock()
	defer r.mu.RUnlock()

	list := make([]Worker, 0, len(r.workers))
	for _, w := range r.workers {
		copy := *w
		copy.InFlight = r.inflight[w.ID]
		list = append(list, copy)
	}
	return list
}

// ListWorkerSPMap returns a mapping of worker ID → MinerAddress for settlement.
func (r *Registry) ListWorkerSPMap() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	m := make(map[string]string, len(r.workers))
	for id, w := range r.workers {
		if w.MinerAddress != "" {
			m[id] = w.MinerAddress
		}
	}
	return m
}

// ListMinerPayoutMap returns miner address → miner-signed EVM payout address for
// every worker that self-registered one. Settlement overlays this on top of the
// static sp_address_map (the signed value wins for its miner).
func (r *Registry) ListMinerPayoutMap() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	m := make(map[string]string)
	for _, w := range r.workers {
		if w.MinerAddress != "" && w.PayoutAddress != "" {
			m[w.MinerAddress] = w.PayoutAddress
		}
	}
	return m
}

// FindByToken returns the worker whose per-worker auth token equals token.
// Used by certificate renewal, where the Bearer token IS the worker identity.
// Constant-time compare per candidate: the token is a credential.
func (r *Registry) FindByToken(token string) (*Worker, bool) {
	if token == "" {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, w := range r.workers {
		if w.AuthToken != "" && subtle.ConstantTimeCompare([]byte(w.AuthToken), []byte(token)) == 1 {
			cp := *w
			return &cp, true
		}
	}
	return nil, false
}

// FindByMiner returns the worker bound to the given miner address, if any.
func (r *Registry) FindByMiner(miner string) (*Worker, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, w := range r.workers {
		if w.MinerAddress == miner {
			copy := *w
			copy.InFlight = r.inflight[w.ID]
			return &copy, true
		}
	}
	return nil, false
}

// Count returns the number of registered workers.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.workers)
}

// SetBan sets (or, with a zero time, clears) a worker's routing ban. Banned
// workers keep being polled — their state stays observable — but the router
// dispatches no inference tasks to them until the ban expires. Persisted so a
// gateway restart cannot cut a punishment short.
func (r *Registry) SetBan(id string, until time.Time, reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workers[id]
	if !ok {
		return false
	}
	w.BannedUntil = until
	w.BanReason = reason
	if until.IsZero() {
		r.logger.Info("worker ban cleared", "id", id)
	} else {
		r.logger.Warn("worker banned from routing", "id", id, "until", until, "reason", reason)
	}
	r.saveToDiskLocked()
	return true
}

// SetVerified replaces a worker's evidence-backed model list wholesale and
// stamps VerifiedAt. Persisted: a gateway restart must not forget which claims
// were already proven (re-verification would take the worker out of rotation
// for the whole probe pass).
func (r *Registry) SetVerified(id string, models []string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workers[id]
	if !ok {
		return false
	}
	w.VerifiedModels = append([]string(nil), models...)
	w.VerifiedAt = time.Now()
	r.logger.Info("worker verified models updated", "id", id, "models", models)
	r.saveToDiskLocked()
	return true
}

// AdmissionGate switches routing (for self-registered workers) to trust ONLY
// verified_models. Set once at start-up from probe config.
func (r *Registry) SetAdmissionGate(on bool) { r.admissionGate.Store(on) }

// AdmissionGateEnabled reports whether the probe admission gate is active.
func (r *Registry) AdmissionGateEnabled() bool { return r.admissionGate.Load() }

// SetFeatures records the capability flags a worker's /health advertises (e.g.
// "continuation" for B2 stream resume). Called by the poller each cycle; transient
// runtime state, never persisted. The slice is replaced wholesale (never mutated in
// place), so snapshot readers (List/Get shallow copies) are race-free.
func (r *Registry) SetFeatures(id string, features []string, receiptPubkey string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workers[id]
	if !ok {
		return
	}
	w.Features = features
	if receiptPubkey != "" && w.ReceiptPubkey != "" && w.ReceiptPubkey != receiptPubkey {
		// Key rotation is legitimate but rare; impersonation looks identical — say it loudly.
		r.logger.Warn("worker receipt pubkey CHANGED",
			"worker", id, "old", w.ReceiptPubkey[:16]+"…", "new", receiptPubkey[:16]+"…")
	}
	if receiptPubkey != "" {
		w.ReceiptPubkey = receiptPubkey
	}
}

// SetUntilChange records the scheduler's state-duration estimate (B1 predictive
// routing). Transient runtime state, refreshed each poll; never persisted.
func (r *Registry) SetUntilChange(id string, seconds int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.workers[id]; ok {
		w.SecondsUntilChange = seconds
		w.UntilChangeAt = time.Now()
	}
}

// UpdateState updates the runtime state of a worker from a successful poll.
// Resets ConsecutiveFailures. Updates GPUCount from detected engine count
// so weight calculation reflects actual inference capacity, not physical GPU count.
func (r *Registry) UpdateState(id string, gpuState, engineState string, activeRequests int, loadedModel string, engineCount int) (oldState, newState WorkerState, changed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.workers[id]
	if !ok {
		return "", "", false
	}

	oldState = w.State
	w.GPUState = gpuState
	w.EngineState = engineState
	w.ActiveRequests = activeRequests
	w.LastPollTime = time.Now()
	w.ConsecutiveFailures = 0
	derived := DeriveState(gpuState, engineState, activeRequests)
	now := time.Now()

	// Gateway-initiated model switch: py-inference keeps reporting
	// engine_state="running" all through a switch, so this poll would otherwise
	// derive idle/busy and the router would route requests onto the reloading
	// engine (they hang until timeout). While SwitchingTo is set, force "loading"
	// so the router excludes the worker — until it reports the target model
	// loaded (switch done), or the switch exceeds loadingTimeout (hung/failed).
	if w.SwitchingTo != "" {
		switch {
		case sameModelBasename(loadedModel, w.SwitchingTo):
			w.SwitchingTo = ""
			w.SwitchingSince = time.Time{}
		case r.loadingTimeout > 0 && !w.SwitchingSince.IsZero() && now.Sub(w.SwitchingSince) > r.loadingTimeout:
			r.logger.Warn("worker model switch exceeded timeout, treating as offline",
				"worker", id, "switching_to", w.SwitchingTo, "elapsed", now.Sub(w.SwitchingSince).String())
			w.SwitchingTo = ""
			w.SwitchingSince = time.Time{}
			derived = StateOffline
		default:
			derived = StateLoading
		}
	}

	// Stuck-loading guard: a worker that stays in "loading" longer than any real
	// model switch is treated as offline, so the router stops counting it as "about
	// to become available" (audit MEDIUM fix). LoadingSince marks when it first
	// entered loading; it is cleared whenever the worker leaves the loading state.
	if derived == StateLoading {
		if w.LoadingSince.IsZero() {
			w.LoadingSince = now
		} else if r.loadingTimeout > 0 && now.Sub(w.LoadingSince) > r.loadingTimeout {
			r.logger.Warn("worker stuck loading too long, treating as offline",
				"worker", id, "loading_for", now.Sub(w.LoadingSince).String())
			derived = StateOffline
		}
	} else {
		w.LoadingSince = time.Time{}
	}

	w.State = derived
	newState = w.State

	// Log model changes
	if loadedModel != "" && w.LoadedModel != loadedModel {
		r.logger.Info("worker loaded model changed",
			"worker", id,
			"old_model", w.LoadedModel,
			"new_model", loadedModel,
		)
	}
	w.LoadedModel = loadedModel

	// Auto-update GPUCount from engine count (reflects actual capacity)
	if engineCount > 0 && w.GPUCount != engineCount {
		r.logger.Info("auto-updated gpu_count from engine_count",
			"worker", id, "old", w.GPUCount, "new", engineCount)
		w.GPUCount = engineCount
	}

	return oldState, newState, oldState != newState
}

// MarkSwitching records that the gateway just dispatched a request that will
// trigger a model switch to targetModel on this worker, and forces the worker to
// "loading" immediately so the router stops routing OTHER requests onto it during
// the switch. Without this, the router keeps routing to the worker until a poll
// notices — but py-inference reports engine_state="running" throughout a switch,
// so the poll never marks it loading, and requests pile onto the reloading engine
// and hang until they time out. No-op if the worker already has the target model.
func (r *Registry) MarkSwitching(id, targetModel string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workers[id]
	if !ok || targetModel == "" {
		return
	}
	if sameModelBasename(w.LoadedModel, targetModel) {
		return // already loaded — not actually a switch
	}
	if w.SwitchingTo != targetModel {
		w.SwitchingTo = targetModel
		w.SwitchingSince = time.Now()
	}
	w.State = StateLoading
	r.logger.Info("worker marked switching — excluded from routing during model switch",
		"worker", id, "switching_to", targetModel)
}

// RecordPollFailure increments the consecutive failure counter.
// Returns (shouldMarkOffline, currentFailures).
// Caller should only mark offline when shouldMarkOffline is true.
func (r *Registry) RecordPollFailure(id string, threshold int) (shouldMarkOffline bool, failures int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.workers[id]
	if !ok {
		return false, 0
	}

	w.ConsecutiveFailures++
	failures = w.ConsecutiveFailures

	// Only recommend marking offline if threshold reached AND not already offline
	return failures >= threshold && w.State != StateOffline, failures
}

// MarkOffline marks a worker as offline (unreachable).
// Typically called after RecordPollFailure returns shouldMarkOffline=true.
func (r *Registry) MarkOffline(id string) (oldState WorkerState, changed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.workers[id]
	if !ok {
		return "", false
	}

	oldState = w.State
	if oldState == StateOffline {
		return oldState, false
	}
	w.State = StateOffline
	w.GPUState = ""
	w.EngineState = ""
	w.ActiveRequests = 0
	return oldState, true
}

// FleetStats holds aggregate statistics about the worker fleet.
type FleetStats struct {
	TotalWorkers   int `json:"total_workers"`
	IdleWorkers    int `json:"idle_workers"`
	BusyWorkers    int `json:"busy_workers"`
	MiningWorkers  int `json:"mining_workers"`
	LoadingWorkers int `json:"loading_workers"`
	OfflineWorkers int `json:"offline_workers"`
	TotalGPUs      int `json:"total_gpus"`
	AvailableGPUs  int `json:"available_gpus"`
}

// Stats returns aggregate statistics about the worker fleet.
func (r *Registry) Stats() FleetStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	stateCounts := map[WorkerState]int{
		StateIdle:    0,
		StateBusy:    0,
		StateMining:  0,
		StateLoading: 0,
		StateOffline: 0,
	}
	totalGPUs := 0
	availableGPUs := 0

	for _, w := range r.workers {
		stateCounts[w.State]++
		totalGPUs += w.GPUCount
		if w.State == StateIdle || w.State == StateBusy {
			availableGPUs += w.GPUCount
		}
	}

	return FleetStats{
		TotalWorkers:   len(r.workers),
		IdleWorkers:    stateCounts[StateIdle],
		BusyWorkers:    stateCounts[StateBusy],
		MiningWorkers:  stateCounts[StateMining],
		LoadingWorkers: stateCounts[StateLoading],
		OfflineWorkers: stateCounts[StateOffline],
		TotalGPUs:      totalGPUs,
		AvailableGPUs:  availableGPUs,
	}
}

// persistedWorker is the subset of Worker fields we persist to disk.
type persistedWorker struct {
	ID              string    `json:"id"`
	Endpoint        string    `json:"endpoint"`
	SchedulerURL    string    `json:"scheduler_url"`
	GPUCount        int       `json:"gpu_count"`
	MinerAddress    string    `json:"miner_address"`
	SupportedModels []string  `json:"supported_models,omitempty"`
	AuthToken       string    `json:"auth_token,omitempty"` // per-worker secret; must survive restarts or polls 401 → offline
	PayoutAddress   string    `json:"payout_address,omitempty"`
	SelfRegistered  bool      `json:"self_registered,omitempty"`
	BannedUntil     time.Time `json:"banned_until,omitempty"` // a restart must not cut a punishment short
	BanReason       string    `json:"ban_reason,omitempty"`
	RegisteredAt    time.Time `json:"registered_at"`
	// The evidence-backed model list MUST persist: without it, every gateway
	// restart drops the whole fleet out of the admission-gated routing pool
	// until each worker is re-probed model-by-model (minutes of dead air).
	VerifiedModels []string  `json:"verified_models,omitempty"`
	VerifiedAt     time.Time `json:"verified_at,omitempty"`
}

func (r *Registry) saveToDiskLocked() {
	if r.savePath == "" {
		return
	}

	list := make([]persistedWorker, 0, len(r.workers))
	for _, w := range r.workers {
		list = append(list, persistedWorker{
			ID:              w.ID,
			Endpoint:        w.Endpoint,
			SchedulerURL:    w.SchedulerURL,
			GPUCount:        w.GPUCount,
			MinerAddress:    w.MinerAddress,
			SupportedModels: w.SupportedModels,
			AuthToken:       w.AuthToken,
			PayoutAddress:   w.PayoutAddress,
			SelfRegistered:  w.SelfRegistered,
			BannedUntil:     w.BannedUntil,
			BanReason:       w.BanReason,
			RegisteredAt:    w.RegisteredAt,
			VerifiedModels:  w.VerifiedModels,
			VerifiedAt:      w.VerifiedAt,
		})
	}

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		r.logger.Error("failed to marshal workers", "error", err)
		return
	}

	// Atomic write: write to a temp file in the same directory then rename.
	// This prevents data corruption if the process crashes mid-write.
	tmpPath := r.savePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		r.logger.Error("failed to write temp file", "error", err)
		return
	}
	if err := os.Rename(tmpPath, r.savePath); err != nil {
		r.logger.Error("failed to rename temp file", "error", err)
	}
}

func (r *Registry) loadFromDisk() {
	data, err := os.ReadFile(r.savePath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		r.logger.Error("failed to load workers", "error", err)
		return
	}

	var list []persistedWorker
	if err := json.Unmarshal(data, &list); err != nil {
		r.logger.Error("failed to parse workers file", "error", err)
		return
	}

	for _, pw := range list {
		registeredAt := pw.RegisteredAt
		if registeredAt.IsZero() {
			registeredAt = time.Now() // Fallback for old files without RegisteredAt
		}
		r.workers[pw.ID] = &Worker{
			ID:              pw.ID,
			Endpoint:        pw.Endpoint,
			SchedulerURL:    pw.SchedulerURL,
			State:           StateOffline, // Start as offline until first poll
			GPUCount:        pw.GPUCount,
			MinerAddress:    pw.MinerAddress,
			SupportedModels: pw.SupportedModels,
			VerifiedModels:  pw.VerifiedModels,
			VerifiedAt:      pw.VerifiedAt,
			AuthToken:       pw.AuthToken,
			PayoutAddress:   pw.PayoutAddress,
			SelfRegistered:  pw.SelfRegistered,
			BannedUntil:     pw.BannedUntil,
			BanReason:       pw.BanReason,
			RegisteredAt:    registeredAt,
		}
	}
	r.logger.Info("loaded workers from disk", "count", len(list))
}
