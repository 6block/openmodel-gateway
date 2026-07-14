package admin

import (
	"testing"

	"openmodel/sp-state-agent/internal/worker"
)

// TestHandleReady503WhenAllMining covers the readiness 503 branch — the signal an
// upstream LB/k8s uses to stop routing when no worker can serve.
func TestHandleReady503WhenAllMining(t *testing.T) {
	e := newTestEnv(t)
	e.registry.Register(worker.WorkerRegistration{ID: "w1", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", GPUCount: 1})
	e.registry.UpdateState("w1", "GPU_STATE_WINDOW_POST", "paused", 0, "m", 1) // mining → not available

	if rec := e.request("GET", "/ready", nil); rec.Code != 503 {
		t.Errorf("ready with all workers mining = %d, want 503", rec.Code)
	}
}

func TestWorkerByID_EmptyID(t *testing.T) {
	e := newTestEnv(t)
	if rec := e.request("GET", "/api/v1/workers/", nil); rec.Code != 400 {
		t.Errorf("empty worker id = %d, want 400", rec.Code)
	}
}

func TestWorkerByID_DeleteNotFound(t *testing.T) {
	e := newTestEnv(t)
	if rec := e.request("DELETE", "/api/v1/workers/ghost", nil); rec.Code != 404 {
		t.Errorf("delete nonexistent worker = %d, want 404", rec.Code)
	}
}

func TestWorkerByID_MethodNotAllowed(t *testing.T) {
	e := newTestEnv(t)
	e.registry.Register(worker.WorkerRegistration{ID: "w1", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", GPUCount: 1})
	if rec := e.request("PUT", "/api/v1/workers/w1", nil); rec.Code != 405 {
		t.Errorf("PUT on worker-by-id = %d, want 405", rec.Code)
	}
}
