package worker

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"sync"
	"time"
)

// validWorkerID matches: letters, digits, hyphens, underscores. 1-64 chars.
var validWorkerID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// Registry manages the set of registered SP workers.
type Registry struct {
	mu       sync.RWMutex
	workers  map[string]*Worker
	logger   *slog.Logger
	savePath string // JSON file for persistence
}

// NewRegistry creates a new worker registry.
// If savePath is non-empty, worker registrations are persisted to that file.
func NewRegistry(logger *slog.Logger, savePath string) *Registry {
	r := &Registry{
		workers:  make(map[string]*Worker),
		logger:   logger,
		savePath: savePath,
	}
	if savePath != "" {
		r.loadFromDisk()
	}
	return r
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
		if len(reg.SupportedModels) > 0 {
			w.SupportedModels = reg.SupportedModels
		}

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
			SupportedModels: reg.SupportedModels,
			RegisteredAt: time.Now(),
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
	return &copy, true
}

// List returns a copy of all registered workers.
func (r *Registry) List() []Worker {
	r.mu.RLock()
	defer r.mu.RUnlock()

	list := make([]Worker, 0, len(r.workers))
	for _, w := range r.workers {
		list = append(list, *w)
	}
	return list
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
	w.State = DeriveState(gpuState, engineState, activeRequests)
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
	RegisteredAt    time.Time `json:"registered_at"`
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
			RegisteredAt:    w.RegisteredAt,
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
			ID:           pw.ID,
			Endpoint:     pw.Endpoint,
			SchedulerURL: pw.SchedulerURL,
			State:        StateOffline, // Start as offline until first poll
			GPUCount:        pw.GPUCount,
			MinerAddress:    pw.MinerAddress,
			SupportedModels: pw.SupportedModels,
			RegisteredAt:    registeredAt,
		}
	}
	r.logger.Info("loaded workers from disk", "count", len(list))
}
