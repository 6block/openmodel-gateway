package gateway

import (
	"testing"

	"openmodel/sp-state-agent/internal/worker"
)

func TestComputeWeight(t *testing.T) {
	cases := []struct {
		name   string
		gpus   int
		active int
		want   float64
	}{
		{"idle 8 GPU", 8, 0, 8},
		{"idle clamps gpus<=0 to 1", 0, 0, 1},
		{"busy 4gpu/4active", 4, 4, 2}, // 4/(1+4/4)=2
		{"heavy load clamps to 1", 1, 100, 1},
	}
	for _, c := range cases {
		got := computeWeight(&worker.Worker{GPUCount: c.gpus, ActiveRequests: c.active})
		if got != c.want {
			t.Errorf("%s: computeWeight = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestModelMatches(t *testing.T) {
	cases := []struct {
		loaded, requested string
		want              bool
	}{
		{"", "x", false},
		{"x", "", false},
		{"Qwen/Qwen2.5-3B", "Qwen/Qwen2.5-3B", true},                              // exact
		{"/models/Qwen--Qwen2.5-3B-Instruct", "Qwen/Qwen2.5-3B-Instruct", true},   // local path ↔ HF id
		{"Qwen--Qwen2.5-3B-Instruct", "Qwen/Qwen2.5-3B-Instruct", true},           // -- ↔ /
		{"/models/Qwen2.5-3B", "Qwen2.5-3B", true},                                // suffix
		{"Qwen/Qwen2.5-3B-Instruct", "meta-llama/Llama-3-70B", false},             // unrelated
	}
	for _, c := range cases {
		if got := modelMatches(c.loaded, c.requested); got != c.want {
			t.Errorf("modelMatches(%q,%q) = %v, want %v", c.loaded, c.requested, got, c.want)
		}
	}
}

func TestWorkerSupportsModel(t *testing.T) {
	w := &worker.Worker{SupportedModels: []string{"Qwen/Qwen2.5-3B-Instruct", "Qwen/Qwen2.5-14B-Instruct"}}
	if !workerSupportsModel(w, "Qwen/Qwen2.5-14B-Instruct") {
		t.Error("exact model in supported list should match")
	}
	if !workerSupportsModel(w, "Qwen--Qwen2.5-3B-Instruct") {
		t.Error("path-variant of a supported model should match")
	}
	if workerSupportsModel(w, "meta-llama/Llama-3-70B") {
		t.Error("unsupported model should not match")
	}
}

func TestWeightedPick(t *testing.T) {
	if _, err := weightedPick(nil); err == nil {
		t.Error("empty candidates should return an error")
	}
	// single-candidate fast path
	w, err := weightedPick([]candidate{{worker: worker.Worker{ID: "solo"}, weight: 3}})
	if err != nil || w.ID != "solo" {
		t.Fatalf("single pick = %v, %v", w, err)
	}
	// weighted distribution: the heavy candidate dominates
	cands := []candidate{
		{worker: worker.Worker{ID: "heavy"}, weight: 99},
		{worker: worker.Worker{ID: "light"}, weight: 1},
	}
	heavy := 0
	for i := 0; i < 1000; i++ {
		w, _ := weightedPick(cands)
		if w.ID == "heavy" {
			heavy++
		}
	}
	if heavy < 900 {
		t.Errorf("heavy (weight 99/100) picked %d/1000, expected ~990", heavy)
	}
}

func routerReg(t *testing.T) *worker.Registry {
	return routerTestRegistry(t) // defined in router_select_test.go
}

func TestSelectWorkerForModel_Priority1Loaded(t *testing.T) {
	reg := routerReg(t)
	reg.Register(worker.WorkerRegistration{ID: "w1", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", GPUCount: 1})
	reg.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "Qwen/Qwen2.5-3B-Instruct", 1)

	w, err := selectWorkerForModel(reg, "Qwen/Qwen2.5-3B-Instruct", nil)
	if err != nil || w.ID != "w1" {
		t.Fatalf("priority-1 (loaded model) should pick w1, got %v err %v", w, err)
	}
}

func TestSelectWorkerForModel_Priority2SupportedIdle(t *testing.T) {
	reg := routerReg(t)
	// w2 has a DIFFERENT model loaded but supports the requested one and is idle → switch
	reg.Register(worker.WorkerRegistration{ID: "w2", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090",
		GPUCount: 1, SupportedModels: []string{"Qwen/Qwen2.5-14B-Instruct"}})
	reg.UpdateState("w2", "GPU_STATE_AVAILABLE", "running", 0, "Qwen/Qwen2.5-3B-Instruct", 1)

	w, err := selectWorkerForModel(reg, "Qwen/Qwen2.5-14B-Instruct", nil)
	if err != nil || w.ID != "w2" {
		t.Fatalf("priority-2 (supported+idle) should pick w2, got %v err %v", w, err)
	}
}

func TestSelectWorkerForModel_OverloadTriggersSwitch(t *testing.T) {
	reg := routerReg(t)
	// loaded worker is busy + overloaded; idle worker supports the model
	reg.Register(worker.WorkerRegistration{ID: "loaded", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", GPUCount: 2})
	reg.UpdateState("loaded", "GPU_STATE_AVAILABLE", "running", 10, "Qwen/Qwen2.5-3B-Instruct", 2) // busy, 10 active
	reg.Register(worker.WorkerRegistration{ID: "idle", Endpoint: "http://y:8000", SchedulerURL: "http://y:9090",
		GPUCount: 1, SupportedModels: []string{"Qwen/Qwen2.5-3B-Instruct"}})
	reg.UpdateState("idle", "GPU_STATE_AVAILABLE", "running", 0, "other-model", 1)

	// factor 2.0: threshold = 2*2 = 4; loaded has 10 active > 4 → overloaded → switch to idle
	w, err := selectWorkerForModel(reg, "Qwen/Qwen2.5-3B-Instruct", nil, 2.0)
	if err != nil || w.ID != "idle" {
		t.Fatalf("overload should switch to idle worker, got %v err %v", w, err)
	}

	// with a high factor (no overload), it stays on the loaded worker
	w2, err := selectWorkerForModel(reg, "Qwen/Qwen2.5-3B-Instruct", nil, 100.0)
	if err != nil || w2.ID != "loaded" {
		t.Fatalf("no overload should stay on loaded worker, got %v err %v", w2, err)
	}
}

func TestSelectWorkerForModel_ExcludeSet(t *testing.T) {
	reg := routerReg(t)
	for _, id := range []string{"a", "b"} {
		reg.Register(worker.WorkerRegistration{ID: id, Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", GPUCount: 1})
		reg.UpdateState(id, "GPU_STATE_AVAILABLE", "running", 0, "default", 1)
	}
	for i := 0; i < 20; i++ {
		w, err := selectWorkerForModel(reg, "default", map[string]bool{"a": true})
		if err != nil || w.ID != "b" {
			t.Fatalf("excluded 'a' must always pick 'b', got %v err %v", w, err)
		}
	}
}
