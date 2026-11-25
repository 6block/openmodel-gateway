package gateway

import (
	"os"
	"strconv"
	"testing"

	"openmodel/sp-state-agent/internal/worker"
)

// Live speech-probe check against a REAL endpoint. Skipped unless SPEECH_LIVE_ENDPOINT
// is set:
//
//	SPEECH_LIVE_ENDPOINT=http://127.0.0.1:8000 \
//	SPEECH_LIVE_MODEL=/models/hexgrad--Kokoro-82M \
//	SPEECH_LIVE_ROUNDS=3 \
//	go test ./internal/gateway/ -run TestSpeechLive -count=1 -v
//
// SPEECH_LIVE_TOKEN supplies the inference API bearer token when the worker needs one;
// read it from the worker's sidecar config rather than typing it into a command line.
//
// The floor of each speech family is derived from BOTH sides, as the vision and
// image-generation floors are: an honest model must clear it and a fabricating
// responder must not. Set SPEECH_LIVE_EXPECT_IMPERSONATION=1 when the endpoint is an
// impersonator (scripts/speech_impersonator_stub.py); the run then asserts the pooled
// score stays below the floor. Each round is one 16-item probe run, scored by the same
// code the auditor uses — so the reported pooled score is exactly what admission sees.
func TestSpeechLive(t *testing.T) {
	endpoint := os.Getenv("SPEECH_LIVE_ENDPOINT")
	if endpoint == "" {
		t.Skip("SPEECH_LIVE_ENDPOINT not set — live speech check skipped")
	}
	model := os.Getenv("SPEECH_LIVE_MODEL")
	if model == "" {
		model = "/models/hexgrad--Kokoro-82M"
	}
	rounds, _ := strconv.Atoi(os.Getenv("SPEECH_LIVE_ROUNDS"))
	if rounds <= 0 {
		rounds = 3
	}
	impersonator := os.Getenv("SPEECH_LIVE_EXPECT_IMPERSONATION") != ""

	reg := worker.NewRegistry(testLogger(), "")
	reg.Register(worker.WorkerRegistration{
		ID: "sp-live-speech", Endpoint: endpoint, SchedulerURL: "http://127.0.0.1:9090",
		GPUCount: 1, SupportedModels: []string{model}, AuthToken: os.Getenv("SPEECH_LIVE_TOKEN"),
	})
	reg.UpdateState("sp-live-speech", "GPU_STATE_AVAILABLE", "running", 0, model, 1)
	// SPEECH_LIVE_ASR_VERIFIER: an ASR worker the TTS content check transcribes on (with
	// SPEECH_LIVE_ASR_TOKEN). Without it the TTS probe runs its shape checks only.
	if v := os.Getenv("SPEECH_LIVE_ASR_VERIFIER"); v != "" {
		reg.Register(worker.WorkerRegistration{
			ID: "sp-live-asr", Endpoint: v, SchedulerURL: "http://127.0.0.1:9090", GPUCount: 1,
			MinerAddress: "t0verifier", AuthToken: os.Getenv("SPEECH_LIVE_ASR_TOKEN"),
		})
		reg.UpdateState("sp-live-asr", "GPU_STATE_AVAILABLE", "running", 0, "/models/Systran--faster-whisper-small", 1)
	}
	a := newAuditor(ProbeConfig{Enabled: true, NumQuestions: 16, MinScore: 0.5, RequestTimeout: 120e9},
		&Gateway{registry: reg, logger: testLogger()}, testLogger())

	w := mustGet(t, reg, "sp-live-speech")
	total, asked, aborts := 0, 0, 0
	for r := 0; r < rounds; r++ {
		var correct, n int
		var err error
		switch {
		case isASRModel(model):
			correct, n, err = a.runASRProbe(t.Context(), w, model)
		case isTTSModel(model):
			correct, n, err = a.runTTSProbe(t.Context(), w, model)
		default:
			t.Fatalf("%s is not a speech model", model)
		}
		if err != nil {
			// A transport error scores nothing and is never a pass.
			aborts++
			t.Logf("round %d: ABORT (%v)", r+1, err)
			continue
		}
		total += correct
		asked += n
		t.Logf("round %d: %d/%d", r+1, correct, n)
	}
	floor := a.floorFor(model)
	pooled := 0.0
	if asked > 0 {
		pooled = float64(total) / float64(asked)
	}
	t.Logf("live speech probe %s: %d/%d over %d round(s), %d abort(s); pooled %.3f (%.1f/48), floor %.3f (%.1f/48)",
		model, total, asked, rounds, aborts, pooled, pooled*48, floor, floor*48)

	if impersonator {
		if asked > 0 && pooled >= floor {
			t.Fatalf("an impersonator must not clear the floor: pooled %.3f >= %.3f", pooled, floor)
		}
		return
	}
	if aborts > 0 {
		t.Errorf("%d round(s) aborted — an honest worker must answer every item", aborts)
	}
	if pooled < floor {
		t.Fatalf("a real %s must clear its floor: pooled %.3f < %.3f", model, pooled, floor)
	}
}
