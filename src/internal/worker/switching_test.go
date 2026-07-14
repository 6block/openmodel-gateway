package worker

import (
	"testing"
	"time"
)

// switchReg registers an 8-GPU worker idle on 1.5B, supporting both models.
func switchReg(t *testing.T) *Registry {
	t.Helper()
	r := quietRegistry(t)
	r.Register(WorkerRegistration{
		ID: "w1", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", GPUCount: 8,
		SupportedModels: []string{"Qwen/Qwen2.5-3B-Instruct", "Qwen/Qwen2.5-1.5B-Instruct"},
	})
	r.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "/models/Qwen--Qwen2.5-1.5B-Instruct", 8)
	return r
}

// TestMarkSwitchingExcludesUntilTargetLoaded is the core fix: a gateway-triggered
// switch forces the worker to "loading" so the router excludes it, and crucially a
// mid-switch poll that still reports engine_state="running" on the OLD model does
// NOT flip it back to routable. It returns to idle only once the poll reports the
// target model loaded.
func TestMarkSwitchingExcludesUntilTargetLoaded(t *testing.T) {
	r := switchReg(t)

	r.MarkSwitching("w1", "Qwen/Qwen2.5-3B-Instruct")
	if g, _ := r.Get("w1"); g.State != StateLoading {
		t.Fatalf("after MarkSwitching state=%q, want loading", g.State)
	}

	// Mid-switch poll: py-inference still reports running on the old model.
	// Before the fix this derived idle/busy → worker became routable → hangs.
	if _, ns, _ := r.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "/models/Qwen--Qwen2.5-1.5B-Instruct", 8); ns != StateLoading {
		t.Fatalf("mid-switch poll state=%q, want loading (must stay excluded)", ns)
	}

	// Switch completes: poll reports the target model → routable again, flag cleared.
	if _, ns, _ := r.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "/models/Qwen--Qwen2.5-3B-Instruct", 8); ns != StateIdle {
		t.Fatalf("switch-complete poll state=%q, want idle", ns)
	}
	if g, _ := r.Get("w1"); g.SwitchingTo != "" {
		t.Fatalf("SwitchingTo=%q not cleared after completion", g.SwitchingTo)
	}
}

// TestMarkSwitchingTimesOutToOffline: a switch that never completes within
// loadingTimeout is treated as offline so the worker stops being "about to serve".
func TestMarkSwitchingTimesOutToOffline(t *testing.T) {
	r := switchReg(t)
	r.SetLoadingTimeout(50 * time.Millisecond)
	r.MarkSwitching("w1", "Qwen/Qwen2.5-3B-Instruct")
	if g, _ := r.Get("w1"); g.State != StateLoading {
		t.Fatalf("after MarkSwitching state=%q, want loading", g.State)
	}
	// Backdate the switch start beyond the timeout, then poll (still old model).
	time.Sleep(70 * time.Millisecond)
	if _, ns, _ := r.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "/models/Qwen--Qwen2.5-1.5B-Instruct", 8); ns != StateOffline {
		t.Fatalf("stuck-switch poll state=%q, want offline", ns)
	}
	if g, _ := r.Get("w1"); g.SwitchingTo != "" {
		t.Fatalf("SwitchingTo not cleared after timeout")
	}
}

// TestMarkSwitchingNoopIfAlreadyLoaded: requesting a model the worker already has
// loaded is not a switch — it must not force loading.
func TestMarkSwitchingNoopIfAlreadyLoaded(t *testing.T) {
	r := switchReg(t)
	r.MarkSwitching("w1", "Qwen/Qwen2.5-1.5B-Instruct") // already loaded
	g, _ := r.Get("w1")
	if g.SwitchingTo != "" {
		t.Fatalf("SwitchingTo set for already-loaded model")
	}
	if g.State == StateLoading {
		t.Fatalf("state forced loading for already-loaded model")
	}
}

// TestMarkSwitchingUnknownWorker is a no-op and must not panic.
func TestMarkSwitchingUnknownWorker(t *testing.T) {
	r := switchReg(t)
	r.MarkSwitching("does-not-exist", "Qwen/Qwen2.5-3B-Instruct")
}
