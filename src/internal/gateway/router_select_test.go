package gateway

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"openmodel/sp-state-agent/internal/worker"
)

func routerTestRegistry(t *testing.T) *worker.Registry {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return worker.NewRegistry(logger, "")
}

// G3: the router must never select a worker that is mining, loading, or offline.
func TestSelect_ExcludesNonRoutableWorkers(t *testing.T) {
	reg := routerTestRegistry(t)
	for _, id := range []string{"idle", "mining", "loading", "offline"} {
		reg.Register(worker.WorkerRegistration{
			ID: id, Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", GPUCount: 1,
		})
	}
	reg.UpdateState("idle", "GPU_STATE_AVAILABLE", "running", 0, "m", 1)
	reg.UpdateState("mining", "GPU_STATE_WINDOW_POST", "paused", 0, "m", 1)
	reg.UpdateState("loading", "GPU_STATE_AVAILABLE", "loading", 0, "m", 1)
	// "offline" left without a successful poll → offline state.

	// Repeatedly select; must always be the idle worker.
	for i := 0; i < 50; i++ {
		w, err := selectWorkerForModel(reg, "default", nil)
		if err != nil {
			t.Fatalf("expected a routable worker, got error: %v", err)
		}
		if w.ID != "idle" {
			t.Fatalf("G3 regression: selected non-routable worker %q (state=%v)", w.ID, w.State)
		}
	}
}

func TestSelect_NoWorkersWhenAllMining(t *testing.T) {
	reg := routerTestRegistry(t)
	reg.Register(worker.WorkerRegistration{
		ID: "w1", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", GPUCount: 1,
	})
	reg.UpdateState("w1", "GPU_STATE_WINNING_POST", "paused", 0, "m", 1)

	if _, err := selectWorkerForModel(reg, "default", nil); err == nil {
		t.Fatal("expected ErrNoWorkerAvailable when the only worker is mining")
	}
}

// TestMultiPoolWorkerWeighedByServingPool: found on real hardware — a multi-pool worker
// with 5 engines (4 cards on a 32B, 1 on a 4B) took 25 of 40 and then every 4B request
// against a 1-card worker serving the same 4B, because the whole worker's engine count
// was its weight. For 4B traffic both are 1-card workers and must share evenly.
func TestMultiPoolWorkerWeighedByServingPool(t *testing.T) {
	reg := worker.NewRegistry(discardGwLogger(), "")
	const m4b = "/models/Qwen--Qwen3-4B-Instruct-2507"
	reg.Register(worker.WorkerRegistration{ID: "multi", Endpoint: "http://a", SchedulerURL: "http://a", GPUCount: 1})
	reg.Register(worker.WorkerRegistration{ID: "single", Endpoint: "http://b", SchedulerURL: "http://b", GPUCount: 1})
	reg.UpdateState("multi", "GPU_STATE_AVAILABLE", "running", 0, "/models/Qwen--Qwen3-32B-AWQ", 5)
	reg.SetLoadedModels("multi", []string{m4b})
	reg.SetPools("multi", map[string]int{"/models/Qwen--Qwen3-32B-AWQ": 4, m4b: 1}, map[string]int{}, nil)
	reg.UpdateState("single", "GPU_STATE_AVAILABLE", "running", 0, m4b, 1)
	n := map[string]int{}
	for i := 0; i < 4000; i++ {
		w, err := selectWorkerForModel(reg, m4b, nil)
		if err != nil {
			t.Fatal(err)
		}
		n[w.ID]++
	}
	if r := float64(n["multi"]) / float64(n["single"]); r < 0.8 || r > 1.25 {
		t.Fatalf("4B traffic split %v; two 1-card pools must share about evenly", n)
	}
	// For the 32B the multi-pool worker is the only one — still a 4-card pool.
	w, _ := reg.Get("multi")
	if got := computeWeightFor(w, "/models/Qwen--Qwen3-32B-AWQ"); got != 4 {
		t.Fatalf("32B pool weight = %v, want 4", got)
	}
}

// TestModelWithReloadingPoolIsNotRouted: after a yield a multi-pool worker reports
// "running" once its fast pools are back, while its vLLM pool reloads for ~40 s more.
// Found on real hardware: a VL stream interrupted by the yield was resumed onto that
// worker as soon as its image/speech pools came back, got 503 from the still-reloading
// VL pool twice in four seconds, and was cut off. A model whose own pool is not running
// must not be routed there; the worker's other models still are.
func TestModelWithReloadingPoolIsNotRouted(t *testing.T) {
	reg := worker.NewRegistry(discardGwLogger(), "")
	const vl, img = "/models/Qwen--Qwen3-VL-8B-Instruct", "/models/stabilityai--stable-diffusion-xl-base-1.0"
	reg.Register(worker.WorkerRegistration{ID: "multi", Endpoint: "http://a", SchedulerURL: "http://a", GPUCount: 1})
	reg.UpdateState("multi", "GPU_STATE_AVAILABLE", "running", 0, vl, 5)
	reg.SetLoadedModels("multi", []string{vl, img})
	reg.SetPools("multi", map[string]int{vl: 4, img: 1}, map[string]int{}, map[string]bool{vl: true})
	if _, err := selectWorkerForModel(reg, vl, nil); err == nil {
		t.Fatal("the VL pool is still reloading: no worker may be selected for VL")
	}
	if w, err := selectWorkerForModel(reg, img, nil); err != nil || w.ID != "multi" {
		t.Fatalf("the image pool is running: image requests must still route there (%v)", err)
	}
	reg.SetPools("multi", map[string]int{vl: 4, img: 1}, map[string]int{}, nil) // next poll: VL up
	if w, err := selectWorkerForModel(reg, vl, nil); err != nil || w.ID != "multi" {
		t.Fatalf("once the VL pool is running it must be selected (%v)", err)
	}
}

// TestRequestRoutedOnlyWhereItFits: found on real hardware — a 10 GB worker serves VL-8B with 1
// image / 4k tokens, a 4-card worker with 32 images / 8k. A 3-image or
// ~5k-token request landed on the small one at random and was refused (400) although the large one could
// serve it. Requests must go to a worker whose reported limits fit; when none fits,
// routing is unchanged (that worker answers with its own precise 400).
func TestRequestRoutedOnlyWhereItFits(t *testing.T) {
	reg := worker.NewRegistry(discardGwLogger(), "")
	const vl = "/models/Qwen--Qwen3-VL-8B-Instruct"
	for _, id := range []string{"small", "big"} {
		reg.Register(worker.WorkerRegistration{ID: id, Endpoint: "http://" + id, SchedulerURL: "http://" + id, GPUCount: 1})
		reg.UpdateState(id, "GPU_STATE_AVAILABLE", "running", 0, vl, 1)
	}
	reg.SetModelLimits("small", map[string]worker.ModelLimit{vl: {MaxImages: 1, ContextLimit: 4096}})
	reg.SetModelLimits("big", map[string]worker.ModelLimit{vl: {MaxImages: 32, ContextLimit: 8192}})

	img := `{"type":"image_url","image_url":{"url":"data:x"}}`
	body := func(images int, text string, maxTok int) []byte {
		parts := `{"type":"text","text":"` + text + `"}`
		for i := 0; i < images; i++ {
			parts += "," + img
		}
		return []byte(fmt.Sprintf(`{"model":"vl","max_tokens":%d,"messages":[{"role":"user","content":[%s]}]}`, maxTok, parts))
	}
	count := func(b []byte) map[string]int {
		n := map[string]int{}
		for i := 0; i < 400; i++ {
			w, err := selectWorkerFor(reg, vl, needsOf(b), nil)
			if err != nil {
				t.Fatal(err)
			}
			n[w.ID]++
		}
		return n
	}
	if n := count(body(1, "hi", 8)); n["small"] == 0 || n["big"] == 0 {
		t.Fatalf("a request both can serve must still spread: %v", n)
	}
	if n := count(body(3, "hi", 8)); n["small"] != 0 {
		t.Fatalf("3 images must never go to the 1-image worker: %v", n)
	}
	long := strings.Repeat("word ", 5200) // ~6.5k tokens by the 4-chars estimate
	if n := count(body(1, long, 8)); n["small"] != 0 {
		t.Fatalf("a ~6k-token prompt must never go to the 4k worker: %v", n)
	}
	// Nothing fits (40 images): routing falls back unchanged rather than failing.
	if _, err := selectWorkerFor(reg, vl, needsOf(body(40, "hi", 8)), nil); err != nil {
		t.Fatalf("when no worker fits, routing must fall back, got %v", err)
	}
	// Workers that report no limits (older) are never ruled out.
	reg.SetModelLimits("small", nil)
	if n := count(body(3, "hi", 8)); n["small"] == 0 {
		t.Fatalf("a worker with unknown limits must stay eligible: %v", n)
	}
}

// TestPoolSwitchKeepsOtherPoolsRoutable: a model switch inside one pool of a
// multi-pool worker (32B ↔ VL in the test worker's text pool) used to mark the WHOLE worker
// "loading". Under mixed load that happened 17 times in 10 minutes, ~45 s each, and
// image/speech/4B requests queued for minutes behind switches they had nothing to do
// with. Only the switching pool's models may be excluded.
func TestPoolSwitchKeepsOtherPoolsRoutable(t *testing.T) {
	reg := worker.NewRegistry(discardGwLogger(), "")
	const b32, vl, img = "/models/Qwen--Qwen3-32B-AWQ", "/models/Qwen--Qwen3-VL-8B-Instruct", "/models/stabilityai--stable-diffusion-xl-base-1.0"
	reg.Register(worker.WorkerRegistration{ID: "fs", Endpoint: "http://a", SchedulerURL: "http://a", GPUCount: 5,
		SupportedModels: []string{b32, vl, img}})
	reg.UpdateState("fs", "GPU_STATE_AVAILABLE", "running", 0, b32, 5)
	reg.SetLoadedModels("fs", []string{b32, img})
	reg.SetPoolGroups("fs", [][]string{{b32, b32, vl}, {img}})

	reg.MarkSwitching("fs", vl)
	if w, err := selectWorkerForModel(reg, img, nil); err != nil || w.ID != "fs" {
		t.Fatalf("image requests must still route to the worker during its text-pool switch (%v)", err)
	}
	if _, err := selectWorkerForModel(reg, vl, nil); err == nil {
		t.Fatal("VL must wait for the switch, not trigger a second one")
	}
	if _, err := selectWorkerForModel(reg, b32, nil); err == nil {
		t.Fatal("the 32B is being unloaded by the switch: it must not be routed there")
	}
	// The next poll still shows the old model: the mark holds.
	reg.UpdateState("fs", "GPU_STATE_AVAILABLE", "running", 0, b32, 5)
	reg.SetLoadedModels("fs", []string{b32, img})
	if _, err := selectWorkerForModel(reg, vl, nil); err == nil {
		t.Fatal("still switching: VL must not be routed yet")
	}
	// The worker already names VL while its text pool is still loading: not done.
	reg.UpdateState("fs", "GPU_STATE_AVAILABLE", "running", 0, vl, 5)
	reg.SetLoadedModels("fs", []string{vl, img})
	reg.SetPools("fs", map[string]int{vl: 4, img: 1}, map[string]int{}, map[string]bool{vl: true})
	if _, err := selectWorkerForModel(reg, b32, nil); err == nil {
		t.Fatal("a switch back to the 32B must not start while the VL switch is still loading")
	}
	// Switch done: VL resident and its pool running.
	reg.SetPools("fs", map[string]int{vl: 4, img: 1}, map[string]int{}, nil)
	if w, err := selectWorkerForModel(reg, vl, nil); err != nil || w.ID != "fs" {
		t.Fatalf("after the switch VL routes to the worker (%v)", err)
	}
	// A single-engine worker (no pool groups) is still excluded as a whole.
	reg.Register(worker.WorkerRegistration{ID: "one", Endpoint: "http://b", SchedulerURL: "http://b", GPUCount: 1, SupportedModels: []string{vl, b32}})
	reg.UpdateState("one", "GPU_STATE_AVAILABLE", "running", 0, b32, 1)
	reg.MarkSwitching("one", vl)
	if w, _ := reg.Get("one"); w.State != worker.StateLoading {
		t.Fatalf("single-engine switch must exclude the worker, state %s", w.State)
	}
}

// TestBusyMultiPoolWorkerStillSwitchesAnIdlePool: a multi-pool worker is "busy" whenever any pool has
// work. A VL request must still be able to switch its text pool when that pool is idle.
func TestBusyMultiPoolWorkerStillSwitchesAnIdlePool(t *testing.T) {
	reg := worker.NewRegistry(discardGwLogger(), "")
	const b32, vl, img = "/models/Qwen--Qwen3-32B-AWQ", "/models/Qwen--Qwen3-VL-8B-Instruct", "/models/stabilityai--stable-diffusion-xl-base-1.0"
	reg.Register(worker.WorkerRegistration{ID: "fs", Endpoint: "http://a", SchedulerURL: "http://a", GPUCount: 5, SupportedModels: []string{b32, vl, img}})
	reg.UpdateState("fs", "GPU_STATE_AVAILABLE", "running", 2, b32, 5) // busy: 2 image requests
	reg.SetLoadedModels("fs", []string{b32, img})
	reg.SetPoolGroups("fs", [][]string{{b32, vl}, {img}})
	reg.SetPools("fs", map[string]int{b32: 4, img: 1}, map[string]int{b32: 0, img: 2}, nil)
	if w, err := selectWorkerForModel(reg, vl, nil); err != nil || w.ID != "fs" {
		t.Fatalf("text pool idle: VL must be able to switch it although the worker is busy (%v)", err)
	}
	reg.SetPools("fs", map[string]int{b32: 4, img: 1}, map[string]int{b32: 1, img: 2}, nil)
	if _, err := selectWorkerForModel(reg, vl, nil); err == nil {
		t.Fatal("text pool serving the 32B: a VL switch must wait")
	}
}
