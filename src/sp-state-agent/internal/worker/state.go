package worker

import "time"

// WorkerState represents the derived network-level state of an SP worker.
type WorkerState string

const (
	StateIdle    WorkerState = "idle"    // GPU available, no active requests
	StateBusy    WorkerState = "busy"    // GPU available, has active requests
	StateMining  WorkerState = "mining"  // GPU yielded to mining
	StateLoading WorkerState = "loading" // GPU available but engine loading model (switch in progress)
	StateOffline WorkerState = "offline" // Unreachable
)

// Worker represents a registered SP worker node.
type Worker struct {
	ID              string      `json:"id"`
	Endpoint        string      `json:"endpoint"`         // py-inference URL, e.g. "http://10.0.1.5:8000"
	SchedulerURL    string      `json:"scheduler_url"`    // go-scheduler URL, e.g. "http://10.0.1.5:9090"
	State           WorkerState `json:"state"`
	GPUState        string      `json:"gpu_state"`        // Raw GPU state from go-scheduler
	EngineState     string      `json:"engine_state"`     // Raw engine state from py-inference
	ActiveRequests  int         `json:"active_requests"`
	LoadedModel     string      `json:"loaded_model"`
	SupportedModels []string   `json:"supported_models,omitempty"` // Models this worker can load
	GPUCount     int       `json:"gpu_count"`
	MinerAddress string    `json:"miner_address"`
	LastPollTime time.Time `json:"last_poll_time"`
	RegisteredAt    time.Time   `json:"registered_at"`

	// ConsecutiveFailures tracks poll failures in a row.
	// Worker is marked offline only after >= OfflineFailureThreshold.
	ConsecutiveFailures int `json:"consecutive_failures"`
}

// WorkerRegistration is the input for registering a new worker.
type WorkerRegistration struct {
	ID              string   `json:"id"`
	Endpoint        string   `json:"endpoint"`
	SchedulerURL    string   `json:"scheduler_url"`
	GPUCount        int      `json:"gpu_count"`
	MinerAddress    string   `json:"miner_address"`
	SupportedModels []string `json:"supported_models,omitempty"` // Models this worker can load
}

// DeriveState maps raw M1 states to network-level WorkerState.
// gpu_state comes from go-scheduler /ready endpoint.
// engineState comes from py-inference /health endpoint.
// activeRequests comes from py-inference /health endpoint.
//
// A Worker is "idle"/"busy" only if BOTH:
//   - go-scheduler says GPU is available (not mining)
//   - py-inference engine is actually running (not loading/unloading/paused)
//
// If the GPU is available but the engine is still loading the model, we treat
// the worker as "mining" (offline-for-routing) to prevent requests hitting an
// engine that will reject them with 503.
func DeriveState(gpuState, engineState string, activeRequests int) WorkerState {
	switch gpuState {
	case "GPU_STATE_AVAILABLE":
		// GPU is free for inference. Check engine actually ready.
		if !isEngineReady(engineState) {
			// Distinguish model loading (switch) from other unavailable states.
			// Loading = model switch in progress, will be ready soon.
			// Paused/unloading = typically post-mining recovery.
			if engineState == "loading" || engineState == "starting" {
				return StateLoading
			}
			return StateMining
		}
		if activeRequests > 0 {
			return StateBusy
		}
		return StateIdle
	case "GPU_STATE_YIELDING", "GPU_STATE_WINDOW_POST", "GPU_STATE_WINNING_POST":
		return StateMining
	default:
		return StateOffline
	}
}

// isEngineReady returns true if the py-inference engine can serve requests.
// M1 engine states: stopped, starting, running, unloading, paused, loading, stopping.
func isEngineReady(engineState string) bool {
	// Accept empty (backward compat with older M1 versions that don't report engine_state)
	return engineState == "" || engineState == "running"
}
