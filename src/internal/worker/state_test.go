package worker

import "testing"

func TestDeriveState(t *testing.T) {
	tests := []struct {
		name           string
		gpuState       string
		engineState    string
		activeRequests int
		want           WorkerState
	}{
		{"available + running + no requests", "GPU_STATE_AVAILABLE", "running", 0, StateIdle},
		{"available + running + requests", "GPU_STATE_AVAILABLE", "running", 5, StateBusy},
		{"available + loading → loading (model switch)", "GPU_STATE_AVAILABLE", "loading", 0, StateLoading},
		{"available + starting → loading", "GPU_STATE_AVAILABLE", "starting", 0, StateLoading},
		// Unloading with the GPU still AVAILABLE is the first half of a model
		// switch (a mining yield flips gpu_state first) — it must read as
		// loading, or every switch surfaces as "busy mining" to clients.
		{"available + unloading → loading", "GPU_STATE_AVAILABLE", "unloading", 0, StateLoading},
		{"available + paused → mining", "GPU_STATE_AVAILABLE", "paused", 0, StateMining},
		{"available + empty engine_state (backward compat) → idle", "GPU_STATE_AVAILABLE", "", 0, StateIdle},
		{"yielding", "GPU_STATE_YIELDING", "running", 0, StateMining},
		{"window post", "GPU_STATE_WINDOW_POST", "paused", 3, StateMining},
		{"winning post", "GPU_STATE_WINNING_POST", "paused", 0, StateMining},
		{"unknown state", "GPU_STATE_UNKNOWN", "running", 0, StateOffline},
		{"empty gpu state", "", "", 0, StateOffline},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeriveState(tt.gpuState, tt.engineState, tt.activeRequests)
			if got != tt.want {
				t.Errorf("DeriveState(%q, %q, %d) = %q, want %q",
					tt.gpuState, tt.engineState, tt.activeRequests, got, tt.want)
			}
		})
	}
}
