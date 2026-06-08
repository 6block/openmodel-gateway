package worker

import (
	"log/slog"
	"os"
	"testing"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewRegistry(logger, "")
}

func TestRegisterAndGet(t *testing.T) {
	r := newTestRegistry(t)

	reg := WorkerRegistration{
		ID:           "sp-test",
		Endpoint:     "http://10.0.1.5:8000",
		SchedulerURL: "http://10.0.1.5:9090",
		GPUCount:     8,
		MinerAddress: "t0182063",
	}

	result, err := r.Register(reg)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created {
		t.Error("expected Created=true for new registration")
	}
	if result.Worker.ID != "sp-test" {
		t.Errorf("expected ID sp-test, got %s", result.Worker.ID)
	}
	if result.Worker.State != StateOffline {
		t.Errorf("expected initial state offline, got %s", result.Worker.State)
	}

	got, ok := r.Get("sp-test")
	if !ok {
		t.Fatal("worker not found")
	}
	if got.Endpoint != "http://10.0.1.5:8000" {
		t.Errorf("expected endpoint, got %s", got.Endpoint)
	}
}

func TestRegisterEndpointChanged(t *testing.T) {
	r := newTestRegistry(t)

	first, err := r.Register(WorkerRegistration{
		ID: "sp-test", Endpoint: "http://10.0.1.5:8000", SchedulerURL: "http://10.0.1.5:9090",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created {
		t.Error("first register should be Created=true")
	}
	if first.EndpointChanged {
		t.Error("first register should NOT be EndpointChanged")
	}

	// Re-register with same endpoint
	same, _ := r.Register(WorkerRegistration{
		ID: "sp-test", Endpoint: "http://10.0.1.5:8000", SchedulerURL: "http://10.0.1.5:9090",
	})
	if same.Created {
		t.Error("re-register with same endpoint should NOT be Created")
	}
	if same.EndpointChanged {
		t.Error("re-register with same endpoint should NOT be EndpointChanged")
	}

	// Re-register with changed endpoint
	changed, _ := r.Register(WorkerRegistration{
		ID: "sp-test", Endpoint: "http://10.0.2.5:8000", SchedulerURL: "http://10.0.2.5:9090",
	})
	if changed.Created {
		t.Error("update should NOT be Created")
	}
	if !changed.EndpointChanged {
		t.Error("endpoint change should set EndpointChanged=true")
	}
	if changed.OldEndpoint != "http://10.0.1.5:8000" {
		t.Errorf("OldEndpoint mismatch: got %s", changed.OldEndpoint)
	}
}

func TestRegisterValidation(t *testing.T) {
	r := newTestRegistry(t)

	_, err := r.Register(WorkerRegistration{})
	if err == nil {
		t.Error("expected error for empty ID")
	}

	_, err = r.Register(WorkerRegistration{ID: "test"})
	if err == nil {
		t.Error("expected error for empty endpoint")
	}

	_, err = r.Register(WorkerRegistration{ID: "test", Endpoint: "http://x"})
	if err == nil {
		t.Error("expected error for empty scheduler_url")
	}
}

func TestDeregister(t *testing.T) {
	r := newTestRegistry(t)

	r.Register(WorkerRegistration{
		ID: "sp-test", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090",
	})

	w, ok := r.Deregister("sp-test")
	if !ok {
		t.Fatal("deregister failed")
	}
	if w.ID != "sp-test" {
		t.Error("wrong worker returned")
	}

	_, ok = r.Get("sp-test")
	if ok {
		t.Error("worker should not exist after deregister")
	}

	_, ok = r.Deregister("nonexistent")
	if ok {
		t.Error("should not find nonexistent worker")
	}
}

func TestUpdateState(t *testing.T) {
	r := newTestRegistry(t)

	r.Register(WorkerRegistration{
		ID: "sp-test", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090",
	})

	// offline -> idle
	old, new, changed := r.UpdateState("sp-test", "GPU_STATE_AVAILABLE", "running", 0, "Qwen2.5", 1)
	if !changed {
		t.Error("expected state change")
	}
	if old != StateOffline || new != StateIdle {
		t.Errorf("expected offline->idle, got %s->%s", old, new)
	}

	// idle -> busy
	old, new, changed = r.UpdateState("sp-test", "GPU_STATE_AVAILABLE", "running", 3, "Qwen2.5", 1)
	if !changed {
		t.Error("expected state change")
	}
	if old != StateIdle || new != StateBusy {
		t.Errorf("expected idle->busy, got %s->%s", old, new)
	}

	// busy -> mining
	old, new, changed = r.UpdateState("sp-test", "GPU_STATE_WINDOW_POST", "paused", 0, "", 1)
	if !changed {
		t.Error("expected state change")
	}
	if old != StateBusy || new != StateMining {
		t.Errorf("expected busy->mining, got %s->%s", old, new)
	}

	// mining -> mining (no change)
	_, _, changed = r.UpdateState("sp-test", "GPU_STATE_WINNING_POST", "paused", 0, "", 1)
	if changed {
		t.Error("expected no state change (mining->mining)")
	}
}

func TestMarkOffline(t *testing.T) {
	r := newTestRegistry(t)

	r.Register(WorkerRegistration{
		ID: "sp-test", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090",
	})

	// Set to idle first
	r.UpdateState("sp-test", "GPU_STATE_AVAILABLE", "running", 0, "model", 1)

	old, changed := r.MarkOffline("sp-test")
	if !changed {
		t.Error("expected change")
	}
	if old != StateIdle {
		t.Errorf("expected old state idle, got %s", old)
	}

	// Already offline
	_, changed = r.MarkOffline("sp-test")
	if changed {
		t.Error("expected no change (already offline)")
	}
}

func TestStats(t *testing.T) {
	r := newTestRegistry(t)

	r.Register(WorkerRegistration{
		ID: "w1", Endpoint: "http://a:8000", SchedulerURL: "http://a:9090", GPUCount: 4,
	})
	r.Register(WorkerRegistration{
		ID: "w2", Endpoint: "http://b:8000", SchedulerURL: "http://b:9090", GPUCount: 8,
	})

	r.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "", 1)
	r.UpdateState("w2", "GPU_STATE_WINDOW_POST", "paused", 0, "", 1)

	stats := r.Stats()
	if stats.TotalWorkers != 2 {
		t.Errorf("expected 2 total workers, got %d", stats.TotalWorkers)
	}
	if stats.IdleWorkers != 1 {
		t.Errorf("expected 1 idle, got %d", stats.IdleWorkers)
	}
	if stats.MiningWorkers != 1 {
		t.Errorf("expected 1 mining, got %d", stats.MiningWorkers)
	}
	// After UpdateState, GPUCount is auto-updated from engineCount (both=1)
	if stats.TotalGPUs != 2 {
		t.Errorf("expected 2 total GPUs (auto-updated from engine_count), got %d", stats.TotalGPUs)
	}
	if stats.AvailableGPUs != 1 {
		t.Errorf("expected 1 available GPUs, got %d", stats.AvailableGPUs)
	}
}

func TestList(t *testing.T) {
	r := newTestRegistry(t)

	r.Register(WorkerRegistration{
		ID: "w1", Endpoint: "http://a:8000", SchedulerURL: "http://a:9090",
	})
	r.Register(WorkerRegistration{
		ID: "w2", Endpoint: "http://b:8000", SchedulerURL: "http://b:9090",
	})

	workers := r.List()
	if len(workers) != 2 {
		t.Errorf("expected 2 workers, got %d", len(workers))
	}
}
