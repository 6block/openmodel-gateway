package worker

import (
	"strings"
	"time"
)

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
	ID           string `json:"id"`
	Endpoint     string `json:"endpoint"`      // py-inference URL, e.g. "http://10.0.1.5:8000"
	SchedulerURL string `json:"scheduler_url"` // go-scheduler URL, e.g. "http://10.0.1.5:9090"
	// AuthToken is the per-worker shared secret the gateway sends as
	// `Authorization: Bearer <token>` on every call to this worker (inference
	// forwards + /health + /ready polls). The SP sets the same value on its
	// py-inference (INFERENCE_API_TOKEN) and go-scheduler (http_auth_token). Empty =
	// no auth (only safe on a trusted LAN); required once worker and gateway are on
	// different public IPs so the worker's GPU cannot be used bypassing the gateway.
	AuthToken       string      `json:"auth_token,omitempty"`
	State           WorkerState `json:"state"`
	GPUState        string      `json:"gpu_state"`    // Raw GPU state from go-scheduler
	EngineState     string      `json:"engine_state"` // Raw engine state from py-inference
	ActiveRequests  int         `json:"active_requests"`
	LoadedModel     string      `json:"loaded_model"`
	SupportedModels []string    `json:"supported_models,omitempty"` // Models this worker can load
	GPUCount        int         `json:"gpu_count"`
	MinerAddress    string      `json:"miner_address"`
	LastPollTime    time.Time   `json:"last_poll_time"`
	RegisteredAt    time.Time   `json:"registered_at"`

	// InFlight is the gateway's own real-time count of requests dispatched to this
	// worker that haven't completed yet. It is a transient runtime value filled in
	// by the registry on snapshot (List/Get) — never persisted — and gives the load
	// balancer a fresher signal than the polled ActiveRequests (which lags the poll
	// interval). See computeWeight.
	InFlight int `json:"in_flight"`

	// Features are the capability flags this worker's py-inference advertises on
	// /health (refreshed every poll, never persisted). E.g. "continuation" = supports
	// om_continuation, required before the gateway resumes an interrupted stream onto
	// it (B2) — an old worker without it would re-generate from scratch and the client
	// would see duplicated text.
	Features []string `json:"features,omitempty"`

	// SecondsUntilChange is the scheduler's estimate (B1) of how long the CURRENT
	// gpu state lasts: while servable it is "seconds until the graceful mining yield
	// begins" (WindowPoSt deadlines are deterministic on-chain); while mining it is
	// the estimated seconds until resume. -1 = unknown (older scheduler).
	// UntilChangeAt is when that estimate was observed, so readers can decay it.
	SecondsUntilChange int64     `json:"seconds_until_change,omitempty"`
	UntilChangeAt      time.Time `json:"until_change_at,omitempty"`

	// ReceiptPubkey is the hex ed25519 public key this worker signs inference
	// receipts with (A1), advertised on /health. Refreshed every poll; a change is
	// logged loudly (possible impersonation / key rotation).
	ReceiptPubkey string `json:"receipt_pubkey,omitempty"`

	// LoadingSince marks when the worker most recently entered the "loading" state.
	// A worker stuck loading far longer than any real model switch is treated as
	// offline so it stops being counted as "about to become available".
	LoadingSince time.Time `json:"loading_since,omitempty"`

	// SwitchingTo is set by the gateway the instant it dispatches a request that
	// will trigger a model switch on this worker. While set, the worker is forced
	// to "loading" so the router excludes it from routing OTHER requests during
	// the switch — necessary because py-inference keeps reporting
	// engine_state="running" through a switch, so a naive poll would flip the
	// worker back to idle/busy and the router would route requests onto the
	// reloading engine (where they hang until timeout). Cleared by the poll once
	// the worker reports the target model loaded, or after loadingTimeout (a
	// hung/failed switch). Transient runtime state — never persisted to disk.
	SwitchingTo    string    `json:"switching_to,omitempty"`
	SwitchingSince time.Time `json:"switching_since,omitempty"`

	// ConsecutiveFailures tracks poll failures in a row.
	// Worker is marked offline only after >= OfflineFailureThreshold.
	ConsecutiveFailures int `json:"consecutive_failures"`
}

// FeatureContinuation is the /health capability flag meaning the worker understands
// om_continuation (B2 stream resume).
const FeatureContinuation = "continuation"

// FeatureReceipt is the /health capability flag meaning the worker signs inference
// receipts (A1) and understands the X-OM-Receipt-Req request header.
const FeatureReceipt = "receipt"

// HasFeature reports whether the worker advertises the given capability flag.
func (w *Worker) HasFeature(name string) bool {
	for _, f := range w.Features {
		if f == name {
			return true
		}
	}
	return false
}

// WorkerRegistration is the input for registering a new worker.
type WorkerRegistration struct {
	ID              string   `json:"id"`
	Endpoint        string   `json:"endpoint"`
	SchedulerURL    string   `json:"scheduler_url"`
	GPUCount        int      `json:"gpu_count"`
	MinerAddress    string   `json:"miner_address"`
	SupportedModels []string `json:"supported_models,omitempty"` // Models this worker can load
	AuthToken       string   `json:"auth_token,omitempty"`       // per-worker shared secret (see Worker.AuthToken)
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

// sameModelBasename reports whether two model identifiers refer to the same
// model despite path/format differences, e.g.
// "/models/Qwen--Qwen2.5-3B-Instruct" vs "Qwen/Qwen2.5-3B-Instruct". Used to
// detect when a switching worker has finished loading its target model.
func sameModelBasename(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return modelBasename(a) == modelBasename(b)
}

// modelBasename strips path and org prefixes to the bare model name:
// "/models/Qwen--Qwen2.5-3B-Instruct" → "Qwen2.5-3B-Instruct",
// "Qwen/Qwen2.5-3B-Instruct" → "Qwen2.5-3B-Instruct".
func modelBasename(m string) string {
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.LastIndex(m, "--"); i >= 0 {
		m = m[i+2:]
	}
	return m
}
