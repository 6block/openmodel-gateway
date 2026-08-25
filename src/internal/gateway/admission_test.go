package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openmodel/sp-state-agent/internal/worker"
)

// admissionRegistry builds a registry with the gate on and one self-registered,
// servable worker that supports two models but has none verified yet.
func admissionRegistry(t *testing.T, gateOn bool) *worker.Registry {
	t.Helper()
	reg := worker.NewRegistry(testLogger(), "")
	reg.SetAdmissionGate(gateOn)
	reg.Register(worker.WorkerRegistration{
		ID: "sp-a", Endpoint: "http://w:8000", SchedulerURL: "http://w:9090",
		GPUCount: 1, SelfRegistered: true,
		SupportedModels: []string{"Qwen/Qwen2.5-3B-Instruct", "qwen3-32b"},
	})
	reg.UpdateState("sp-a", "GPU_STATE_AVAILABLE", "running", 0, "Qwen/Qwen2.5-3B-Instruct", 1)
	return reg
}

// With the gate ON, a worker that has verified nothing yet must receive NO
// traffic — for any model, and for "default".
func TestAdmission_UnverifiedGetsNoTraffic(t *testing.T) {
	reg := admissionRegistry(t, true)

	if _, err := selectWorkerForModel(reg, "Qwen/Qwen2.5-3B-Instruct", nil); err == nil {
		t.Fatal("unverified worker must not be routable for a specific model")
	}
	if _, err := selectWorkerForModel(reg, "default", nil); err == nil {
		t.Fatal("unverified worker must not be routable for default either")
	}
}

// After a model is verified, routing for THAT model succeeds — but a different
// claimed-but-unverified model still gets nothing.
func TestAdmission_RoutesOnlyVerifiedModels(t *testing.T) {
	reg := admissionRegistry(t, true)
	reg.SetVerified("sp-a", []string{"Qwen/Qwen2.5-3B-Instruct"})

	if _, err := selectWorkerForModel(reg, "Qwen/Qwen2.5-3B-Instruct", nil); err != nil {
		t.Fatalf("verified model must be routable: %v", err)
	}
	if _, err := selectWorkerForModel(reg, "qwen3-32b", nil); err == nil {
		t.Fatal("a claimed-but-unverified model must stay unroutable")
	}
}

// The gate applies ONLY to self-registered workers; an operator-registered
// worker is trusted and routes without any verification.
func TestAdmission_OperatorWorkerExempt(t *testing.T) {
	reg := worker.NewRegistry(testLogger(), "")
	reg.SetAdmissionGate(true)
	reg.Register(worker.WorkerRegistration{
		ID: "op-1", Endpoint: "http://w:8000", SchedulerURL: "http://w:9090",
		GPUCount: 1, SelfRegistered: false,
		SupportedModels: []string{"Qwen/Qwen2.5-3B-Instruct"},
	})
	reg.UpdateState("op-1", "GPU_STATE_AVAILABLE", "running", 0, "Qwen/Qwen2.5-3B-Instruct", 1)

	if _, err := selectWorkerForModel(reg, "Qwen/Qwen2.5-3B-Instruct", nil); err != nil {
		t.Fatalf("operator worker must route without verification: %v", err)
	}
}

// Gate OFF = current behavior: a self-registered worker routes on its claimed
// models with no verification (full backward compatibility).
func TestAdmission_GateOffIsBackwardCompatible(t *testing.T) {
	reg := admissionRegistry(t, false)
	if _, err := selectWorkerForModel(reg, "Qwen/Qwen2.5-3B-Instruct", nil); err != nil {
		t.Fatalf("gate off must route as before: %v", err)
	}
}

// SetVerified persists across re-registration (a routine restart must not drop
// the proven list — otherwise the worker leaves rotation for a full re-probe).
func TestAdmission_VerifiedSurvivesReRegister(t *testing.T) {
	reg := admissionRegistry(t, true)
	reg.SetVerified("sp-a", []string{"Qwen/Qwen2.5-3B-Instruct"})

	// Re-register (new token, same worker) — the path a restart takes.
	reg.Register(worker.WorkerRegistration{
		ID: "sp-a", Endpoint: "http://w:8000", SchedulerURL: "http://w:9090",
		GPUCount: 1, SelfRegistered: true, AuthToken: "wk-new",
		SupportedModels: []string{"Qwen/Qwen2.5-3B-Instruct", "qwen3-32b"},
	})
	w, _ := reg.Get("sp-a")
	if len(w.VerifiedModels) != 1 || w.VerifiedModels[0] != "Qwen/Qwen2.5-3B-Instruct" {
		t.Fatalf("verified models lost on re-register: %+v", w.VerifiedModels)
	}
}

// The per-model floor resolves from config, then defaults, then MinScore.
func TestAdmission_FloorResolution(t *testing.T) {
	a := &auditor{cfg: ProbeConfig{
		MinScore:    0.5,
		ModelFloors: map[string]float64{"custom-model": 0.9},
	}}
	if f := a.floorFor("custom-model"); f != 0.9 {
		t.Fatalf("config override floor = %v, want 0.9", f)
	}
	if f := a.floorFor("qwen3-32b"); f != 46.0/48.0 {
		t.Fatalf("default floor for qwen3-32b = %v, want 46/48", f)
	}
	if f := a.floorFor("totally-unknown"); f != 0.5 {
		t.Fatalf("unknown model must fall back to min_score, got %v", f)
	}
}

// The point of per-model floors: a LOW SCORE IS NOT FRAUD, it is a small model.
// An honest 1.5B scoring like a 1.5B must pass, while the same score claiming a
// big model must fail. A single global floor cannot express this — and did
// mis-ban an honest 1.5B on the test fleet before these floors existed.
func TestAdmission_HonestSmallModelIsNotFraud(t *testing.T) {
	a := &auditor{cfg: ProbeConfig{MinScore: 0.5}}
	const measured15B = 0.881 // 320-item template-path measurement for Qwen2.5-1.5B

	if measured15B < a.floorFor("Qwen/Qwen2.5-1.5B-Instruct") {
		t.Fatal("an honest 1.5B scoring its own normal score must PASS its own floor")
	}
	if measured15B >= a.floorFor("qwen3-32b") {
		t.Fatal("a 1.5B-level score while claiming a 32B must FAIL that model's floor")
	}
	// Bigger claim, never a lower bar. Not STRICTLY increasing: 3B through 32B
	// measure identically on this instrument (0.982-0.983), so they share a
	// floor — inventing a higher one for the larger models would ban honest
	// 3B-class workers to catch a substitution this probe cannot see anyway.
	if !(a.floorFor("Qwen/Qwen2.5-1.5B-Instruct") < a.floorFor("Qwen/Qwen2.5-3B-Instruct") &&
		a.floorFor("Qwen/Qwen2.5-3B-Instruct") <= a.floorFor("qwen3-32b")) {
		t.Fatal("floors must not decrease as the claimed model gets bigger")
	}
}

// verifyModel's outcome logic, driven against a mock worker inference endpoint.
// A passing probe adds the model to verified_models; a failing one removes it
// and, on the third consecutive failure, bans the worker.
func TestAdmission_VerifyOutcomeAddsBansAndSpares(t *testing.T) {
	// Mock worker: answers correctly when `good` is set, gibberish otherwise.
	var good atomic.Bool
	good.Store(true)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		ans := "ANSWER: banana" // deliberately wrong for arithmetic
		if good.Load() {
			// echo back a correct answer by recomputing is overkill; instead the
			// probe generator is deterministic per-call, so we can't know it here.
			// Use the reflected-instruction trick: the worker that ACTUALLY runs the
			// model would compute it. We approximate a "good" worker by returning the
			// last integer present in the prompt's arithmetic — good enough because
			// generate() builds "a OP b" and the mock can't solve it. So instead we
			// make "good" mean: a real-model-like high score is faked via a sentinel.
			ans = "ANSWER: __CORRECT__"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": ans}}},
		})
	}))
	defer backend.Close()

	reg := worker.NewRegistry(testLogger(), "")
	reg.SetAdmissionGate(true)
	reg.Register(worker.WorkerRegistration{
		ID: "sp-a", Endpoint: backend.URL, SchedulerURL: "http://w:9090",
		GPUCount: 1, SelfRegistered: true,
		SupportedModels: []string{"Qwen/Qwen2.5-3B-Instruct"},
	})
	reg.UpdateState("sp-a", "GPU_STATE_AVAILABLE", "running", 0, "Qwen/Qwen2.5-3B-Instruct", 1)

	// A second, healthy worker: bans are withheld when the target is the last
	// servable worker (a probe must not be able to end all service), so a
	// ban test needs someone else able to serve.
	reg.Register(worker.WorkerRegistration{
		ID: "sp-standby", Endpoint: backend.URL, SchedulerURL: "http://w:9090",
		GPUCount: 1, SelfRegistered: true,
		SupportedModels: []string{"Qwen/Qwen2.5-3B-Instruct"},
	})
	reg.UpdateState("sp-standby", "GPU_STATE_AVAILABLE", "running", 0, "Qwen/Qwen2.5-3B-Instruct", 1)

	gw := &Gateway{registry: reg, logger: testLogger()}
	a := newAuditor(ProbeConfig{Enabled: true, IntervalSec: 1, NumQuestions: 4,
		MinScore: 0.5, BanSeconds: 60, AdmissionGate: true}, gw, testLogger())
	// Force every probe question to accept the sentinel as correct.
	a.scoreOverride = func(_ probeQuestion, resp string) bool {
		return strings.Contains(resp, "__CORRECT__")
	}

	// Good worker → model verified.
	a.verifyModel(t.Context(), mustGet(t, reg, "sp-a"), "Qwen/Qwen2.5-3B-Instruct")
	if w, _ := reg.Get("sp-a"); !workerVerifiedFor(w, "Qwen/Qwen2.5-3B-Instruct") {
		t.Fatal("a passing worker must have the model verified")
	}

	// Now it goes bad. Conviction needs POOLED evidence: the window must first
	// fill with bad runs (the earlier good run is still in it), and only full
	// windows count toward the three strikes. That deliberately costs several
	// more probes than the old one-run-one-strike rule — a worker's living
	// depends on this verdict, and one unlucky run must not decide it.
	good.Store(false)
	for i := 0; i < 6; i++ {
		a.verifyModel(t.Context(), mustGet(t, reg, "sp-a"), "Qwen/Qwen2.5-3B-Instruct")
	}
	w, _ := reg.Get("sp-a")
	if workerVerifiedFor(w, "Qwen/Qwen2.5-3B-Instruct") {
		t.Fatal("a failing model must be removed from verified_models")
	}
	if !w.IsBanned() {
		t.Fatal("sustained failure across full evidence windows must ban the worker")
	}
}

// The flip side of pooling: a single bad run must NOT convict. This is the
// property that a global single-run floor lacked, which mis-banned an honest
// 1.5B whose score momentarily dipped.
func TestAdmission_OneBadRunDoesNotConvict(t *testing.T) {
	a := &auditor{cfg: ProbeConfig{MinScore: 0.5}, evidence: map[string][]probeRun{},
		admFails: map[string]int{}}
	// An ordinary dip under template-path scoring: a 3B measures 0.991, i.e.
	// about one dropped item per 48 — so 16,16 then 15 of 16 is a normal run,
	// and pooling must keep it above the 46/48 floor. (13/16 is NOT a normal
	// dip anymore: at p=0.991 three misses in one run is a ~4e-4 event, and a
	// worker producing it repeatedly SHOULD fall below the floor.)
	a.record("w|m", 16, 16)
	a.record("w|m", 16, 16)
	pooled, full := a.record("w|m", 15, 16)
	if !full {
		t.Fatal("three runs should fill the window")
	}
	if pooled < a.floorFor("Qwen/Qwen2.5-3B-Instruct") {
		t.Fatalf("a normal dip must not drag pooled evidence below the floor: %.3f", pooled)
	}
	// And a partial window is never a verdict.
	b := &auditor{cfg: ProbeConfig{MinScore: 0.5}, evidence: map[string][]probeRun{}}
	if _, full := b.record("x|y", 0, 16); full {
		t.Fatal("a single run must not report a full window")
	}
}

// A transport error during verification must NOT touch verified_models or the
// fail counter — a mining yield or network blip is not fraud.
func TestAdmission_TransportErrorDoesNotPunish(t *testing.T) {
	reg := admissionRegistry(t, true)
	reg.SetVerified("sp-a", []string{"Qwen/Qwen2.5-3B-Instruct"})
	// Endpoint points at a dead port → every ask errors.
	reg.Register(worker.WorkerRegistration{
		ID: "sp-a", Endpoint: "http://127.0.0.1:1", SchedulerURL: "http://w:9090",
		GPUCount: 1, SelfRegistered: true,
		SupportedModels: []string{"Qwen/Qwen2.5-3B-Instruct"},
	})

	gw := &Gateway{registry: reg, logger: testLogger()}
	a := newAuditor(ProbeConfig{Enabled: true, IntervalSec: 1, NumQuestions: 2,
		MinScore: 0.5, BanSeconds: 60, AdmissionGate: true, RequestTimeout: 2 * time.Second}, gw, testLogger())

	a.verifyModel(t.Context(), mustGet(t, reg, "sp-a"), "Qwen/Qwen2.5-3B-Instruct")
	w, _ := reg.Get("sp-a")
	if !workerVerifiedFor(w, "Qwen/Qwen2.5-3B-Instruct") {
		t.Fatal("a transport error must not remove an already-verified model")
	}
	if w.IsBanned() {
		t.Fatal("a transport error must never ban")
	}
}

func mustGet(t *testing.T, reg *worker.Registry, id string) worker.Worker {
	t.Helper()
	w, ok := reg.Get(id)
	if !ok {
		t.Fatalf("worker %s not found", id)
	}
	return *w
}

// Self-registration must carry the worker's claimed model list into the
// registry: model-aware routing can only switch toward a model it knows about,
// and this was the one registration path that dropped the list — so switching
// silently never applied to self-registered SPs.
func TestSelfRegister_SupportedModelsReachRoutingAndAdmission(t *testing.T) {
	reg := worker.NewRegistry(testLogger(), "")
	reg.SetAdmissionGate(true)
	reg.Register(worker.WorkerRegistration{
		ID: "sp-sw", Endpoint: "http://w:8000", SchedulerURL: "http://w:9090",
		GPUCount: 1, SelfRegistered: true,
		SupportedModels: []string{"model-a", "model-b"},
	})
	reg.UpdateState("sp-sw", "GPU_STATE_AVAILABLE", "running", 0, "model-a", 1)
	reg.SetVerified("sp-sw", []string{"model-a", "model-b"})

	// Verified + supported (not loaded) → routable, which is what triggers the
	// gateway-side model switch toward model-b.
	if _, err := selectWorkerForModel(reg, "model-b", nil); err != nil {
		t.Fatalf("a verified supported model must be routable (switch trigger): %v", err)
	}
	// And the auditor's first-verification queue must see the claim.
	w, _ := reg.Get("sp-sw")
	if len(w.SupportedModels) != 2 {
		t.Fatalf("claimed models lost on registration: %v", w.SupportedModels)
	}
}

// The model listing must share the router's verdict exactly. With the gate on,
// a verified-but-not-loaded switch target is routable and MUST be listed (the
// user cannot switch to a model they cannot see), while a loaded-but-unverified
// model is unroutable and must NOT be (selecting it can only fail). This is the
// live deployment shape: parked on a freshly loaded model mid-first-verification.
func TestRoutableModels_PartialVerification(t *testing.T) {
	w := &worker.Worker{
		ID: "sp-a", SelfRegistered: true, LoadedModel: "model-new",
		SupportedModels: []string{"model-old", "model-new"},
		VerifiedModels:  []string{"model-old"},
	}
	got := routableModels(w, true)
	if len(got) != 1 || got[0] != "model-old" {
		t.Fatalf("gate on: want only the verified switch target, got %v", got)
	}
	// Gate off = claims are trusted: everything shows.
	if n := len(routableModels(&worker.Worker{ID: "b", SelfRegistered: true,
		LoadedModel: "x", SupportedModels: []string{"x", "y"}}, false)); n != 3 {
		t.Fatalf("gate off must list loaded+supported unfiltered, got %d entries", n)
	}
}
