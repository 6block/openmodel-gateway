package worker

import "testing"

func TestRegisterRejectsInvalidURL(t *testing.T) {
	reg := NewRegistry(wLog(), "")
	// endpoint with no scheme/host
	if _, err := reg.Register(WorkerRegistration{ID: "w1", Endpoint: "not-a-url", SchedulerURL: "http://x:9090", GPUCount: 1}); err == nil {
		t.Error("expected error for endpoint with no scheme/host")
	}
	// scheduler_url with no host
	if _, err := reg.Register(WorkerRegistration{ID: "w2", Endpoint: "http://x:8000", SchedulerURL: "http://", GPUCount: 1}); err == nil {
		t.Error("expected error for scheduler_url with no host")
	}
	// a fully valid one still succeeds
	if _, err := reg.Register(WorkerRegistration{ID: "w3", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", GPUCount: 1}); err != nil {
		t.Errorf("valid registration should succeed, got %v", err)
	}
}

func TestListWorkerSPMap(t *testing.T) {
	reg := NewRegistry(wLog(), "")
	reg.Register(WorkerRegistration{ID: "w1", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", GPUCount: 1, MinerAddress: "t0182063"})
	reg.Register(WorkerRegistration{ID: "w2", Endpoint: "http://y:8000", SchedulerURL: "http://y:9090", GPUCount: 1}) // no MinerAddress

	m := reg.ListWorkerSPMap()
	if m["w1"] != "t0182063" {
		t.Errorf("w1 → %q, want t0182063", m["w1"])
	}
	if _, ok := m["w2"]; ok {
		t.Error("a worker with no MinerAddress must be filtered out of the SP map")
	}
}
