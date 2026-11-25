package gateway

import (
	"testing"

	"openmodel/sp-state-agent/internal/worker"
)

// A multi-pool worker (one inference process, disjoint GPU sets per modality) has
// several models resident at once. These tests pin that routing, the model listing,
// the self-view and the audits treat EVERY resident model as loaded — not only the
// primary LoadedModel the single-engine code was written around.

const (
	mpText  = "/models/Qwen--Qwen3-32B-AWQ"
	mpImage = "/models/stabilityai--stable-diffusion-xl-base-1.0"
	mpASR   = "/models/Systran--faster-whisper-small"
	mpTTS   = "/models/hexgrad--Kokoro-82M"
)

func multiPoolRegistry(t *testing.T, gateOn bool) *worker.Registry {
	t.Helper()
	reg := worker.NewRegistry(testLogger(), "")
	reg.SetAdmissionGate(gateOn)
	reg.Register(worker.WorkerRegistration{
		ID: "sp-multi", Endpoint: "http://w:8000", SchedulerURL: "http://w:9090",
		GPUCount: 8, SelfRegistered: gateOn,
		SupportedModels: []string{mpText, mpImage, mpASR, mpTTS},
	})
	reg.UpdateState("sp-multi", "GPU_STATE_AVAILABLE", "running", 0, mpText, 6)
	reg.SetLoadedModels("sp-multi", []string{mpText, mpImage, mpASR, mpTTS})
	return reg
}

// TestMultiPool_EveryResidentModelRoutesAsLoaded: a request for the image or speech
// model must count as priority-1 "already loaded", not "supported → trigger a switch".
// A switch would tell the worker to unload its text pool to serve an image request.
func TestMultiPool_EveryResidentModelRoutesAsLoaded(t *testing.T) {
	reg := multiPoolRegistry(t, false)
	for _, m := range []string{mpText, mpImage, mpASR, mpTTS, "stabilityai/stable-diffusion-xl-base-1.0"} {
		w, err := selectWorkerForModel(reg, m, nil)
		if err != nil {
			t.Fatalf("%s: not routable: %v", m, err)
		}
		if !workerHasLoaded(w, m) {
			t.Fatalf("%s: routed, but not recognised as resident on %s (would trigger a switch)", m, w.ID)
		}
	}
}

// TestMultiPool_LoadedMatchReturnsWorkerSpelling: the request body is rewritten to
// the worker's own spelling of the model, so a spelling difference is not mistaken
// for a switch. With several resident models the right one must be picked.
func TestMultiPool_LoadedMatchReturnsWorkerSpelling(t *testing.T) {
	reg := multiPoolRegistry(t, false)
	w, _ := reg.Get("sp-multi")
	got, ok := loadedMatch(w, "hexgrad/Kokoro-82M")
	if !ok || got != mpTTS {
		t.Fatalf("loadedMatch = %q,%v; want the worker's spelling %q", got, ok, mpTTS)
	}
	if _, ok := loadedMatch(w, "Qwen/Qwen3-VL-8B-Instruct"); ok {
		t.Fatal("a model that is not resident must not match")
	}
}

// TestMultiPool_OldWorkerStillHasItsOneModel: a worker that only reports
// loaded_model (no loaded_models list) keeps working exactly as before.
func TestMultiPool_OldWorkerStillHasItsOneModel(t *testing.T) {
	reg := worker.NewRegistry(testLogger(), "")
	reg.Register(worker.WorkerRegistration{ID: "old", Endpoint: "http://w:8000", SchedulerURL: "http://w:9090", GPUCount: 1})
	reg.UpdateState("old", "GPU_STATE_AVAILABLE", "running", 0, mpText, 1)
	reg.SetLoadedModels("old", nil)
	w, _ := reg.Get("old")
	if got := loadedModels(w); len(got) != 1 || got[0] != mpText {
		t.Fatalf("loadedModels = %v, want just the primary", got)
	}
}

// TestMultiPool_RoutableModelsListsEveryPool: /v1/models and the catalog derive from
// routableModels. A resident model missing there is a model users cannot select.
func TestMultiPool_RoutableModelsListsEveryPool(t *testing.T) {
	reg := multiPoolRegistry(t, false)
	w, _ := reg.Get("sp-multi")
	got := map[string]bool{}
	for _, m := range routableModels(w, false) {
		got[m] = true
	}
	for _, m := range []string{mpText, mpImage, mpASR, mpTTS} {
		if !got[m] {
			t.Errorf("routableModels is missing resident model %s", m)
		}
	}
}

// TestMultiPool_SpotChecksRotateAcrossPools: periodic audits must eventually cover
// every pool. Auditing only the primary model would let a worker serve its other
// pools with anything at all.
func TestMultiPool_SpotChecksRotateAcrossPools(t *testing.T) {
	reg := multiPoolRegistry(t, true)
	w, _ := reg.Get("sp-multi")
	a := newAuditor(ProbeConfig{Enabled: true, IntervalSec: 1, NumQuestions: 2}, &Gateway{registry: reg}, testLogger())
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		seen[a.nextSpotModel(*w)] = true
	}
	for _, m := range []string{mpText, mpImage, mpASR, mpTTS} {
		if !seen[m] {
			t.Errorf("four consecutive spot checks never audited %s", m)
		}
	}
}

// TestMultiPool_UnverifiedResidentModelGetsAdmitted: with the gate on, a worker that
// declared no supported list still gets EVERY resident model admission-probed, not
// just the primary.
func TestMultiPool_UnverifiedResidentModelGetsAdmitted(t *testing.T) {
	reg := worker.NewRegistry(testLogger(), "")
	reg.SetAdmissionGate(true)
	reg.Register(worker.WorkerRegistration{ID: "sp-nolist", Endpoint: "http://w:8000",
		SchedulerURL: "http://w:9090", GPUCount: 2, SelfRegistered: true})
	reg.UpdateState("sp-nolist", "GPU_STATE_AVAILABLE", "running", 0, mpText, 2)
	reg.SetLoadedModels("sp-nolist", []string{mpText, mpTTS})
	reg.SetVerified("sp-nolist", []string{mpText})
	a := newAuditor(ProbeConfig{Enabled: true, IntervalSec: 1, NumQuestions: 2}, &Gateway{registry: reg}, testLogger())
	_, model, ok := a.nextUnverified()
	if !ok || model != mpTTS {
		t.Fatalf("nextUnverified = %q,%v; want the unverified resident model %q", model, ok, mpTTS)
	}
}
