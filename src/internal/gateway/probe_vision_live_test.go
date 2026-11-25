package gateway

import (
	"os"
	"testing"

	"openmodel/sp-state-agent/internal/worker"
)

// Live vision-probe check against a REAL image-understanding worker. Skipped
// unless VLM_LIVE_ENDPOINT is set (it needs a GPU, the model weights, and a
// worker serving the OpenAI API on that address):
//
//	VLM_LIVE_ENDPOINT=http://127.0.0.1:8000 \
//	VLM_LIVE_MODEL=/models/Qwen--Qwen3-VL-8B-Instruct \
//	go test ./internal/gateway/ -run TestVisionLive -count=1 -v
//
// VLM_LIVE_TOKEN supplies the inference API bearer token when the worker needs
// one; read it from the worker's sidecar config rather than typing it into a
// command line.
//
// The mock-based tests prove the wiring; this one proves the ITEMS are answerable
// by a real VLM — a probe whose images a real model cannot read would fail every
// honest worker, which no mock can detect.
//
// It also measures the number this floor was DERIVED from rather than guessed
// at: the item family's items are only worth anything if a real model clears
// them, so the pass rate it reports is the input to the floor arithmetic in
// probe.go.
//
// Set VLM_LIVE_EXPECT_IMPERSONATION=1 to invert the assertion for the other side
// of the floor: the model behind the endpoint is then a TEXT engine served under
// a VLM claim, and the run must show it CANNOT clear the floor (a blind answer,
// or a non-200 that scores nothing — a transport error is never a pass). Both
// sides are measured because the floor is only meaningful if some model clears
// it and some claimed model cannot.
func TestVisionLive(t *testing.T) {
	endpoint := os.Getenv("VLM_LIVE_ENDPOINT")
	if endpoint == "" {
		t.Skip("VLM_LIVE_ENDPOINT not set — live VLM check skipped")
	}
	model := os.Getenv("VLM_LIVE_MODEL")
	if model == "" {
		model = "/models/Qwen--Qwen3-VL-8B-Instruct"
	}
	impersonator := os.Getenv("VLM_LIVE_EXPECT_IMPERSONATION") != ""

	reg := worker.NewRegistry(testLogger(), "")
	reg.Register(worker.WorkerRegistration{
		ID: "sp-live-vlm", Endpoint: endpoint, SchedulerURL: "http://127.0.0.1:9090",
		GPUCount: 8, SelfRegistered: false, SupportedModels: []string{model},
		AuthToken: os.Getenv("VLM_LIVE_TOKEN"),
	})
	reg.UpdateState("sp-live-vlm", "GPU_STATE_AVAILABLE", "running", 0, model, 8)

	a := newAuditor(ProbeConfig{
		Enabled: true, NumQuestions: 16, MinScore: 0.5,
		// A tp=8 VLM needs tens of seconds per item on this hardware.
		RequestTimeout: 180e9,
	}, &Gateway{registry: reg, logger: testLogger()}, testLogger())

	qs := a.generateVision(16)
	colourN, countN, okN, aborts := 0, 0, 0, 0
	colourOK, countOK := 0, 0
	for i, q := range qs {
		ok, err := a.askVision(t.Context(), mustGet(t, reg, "sp-live-vlm"), model, q)
		if err != nil {
			// An abort scores nothing. On the honest side that is a failure to
			// answer; on the impersonation side it is the expected outcome for a
			// text engine that cannot parse the multimodal array.
			aborts++
			t.Logf("item %2d %-28s -> ABORT (%v)", i, describeVisionItem(q), err)
		} else {
			if q.numeric {
				countN++
				if ok {
					countOK++
				}
			} else {
				colourN++
				if ok {
					colourOK++
				}
			}
			if ok {
				okN++
			}
			t.Logf("item %2d %-28s -> %s", i, describeVisionItem(q), map[bool]string{true: "PASS", false: "FAIL"}[ok])
		}
	}
	floor := a.floorFor(model)
	// The floor is a pooled 48-item threshold; one 16-item run is not comparable
	// to it, so report the pooled-equivalent gap instead of pretending it is.
	t.Logf("live vision probe: %d/%d correct (colour %d/%d, count %d/%d, %d aborts); floor %.4f = %.1f/48 (%+0.1f items vs this run)",
		okN, len(qs), colourOK, colourN, countOK, countN, aborts, floor, floor*48, float64(okN*3)-floor*48)

	if impersonator {
		// Neither responder may clear the floor. Blind guesses land near 1/6 and
		// 1/4 per kind; aborts score zero. A single 16-item run that reaches the
		// pooled threshold only TIES it here — the pooled decision needs two
		// independent windows — so equality is reported, not failed.
		if float64(okN*3) > floor*48 {
			t.Fatalf("a text engine claiming a VLM must not clear the floor: %d/16 → %.1f/48 equivalent, floor %.1f/48",
				okN, float64(okN*3), floor*48)
		}
		if float64(okN*3) == floor*48 {
			t.Logf("this run TIES the pooled floor at %d/16; the pooled decision needs the confirmation run", okN)
		}
		// The leak check belongs on the COUNT half alone. A colour-only responder
		// legitimately wins the whole background half — colour 8/8 in every run
		// above — so a total-score threshold would flag it as a leak it is not.
		// Nothing that fails to read the image can count: 1 of 4 blind, so 6 or
		// more of 8 is p≈0.004 and means the count answers are reaching the
		// responder without a reading of the picture.
		if countOK >= 6 {
			t.Errorf("count half scored %d/%d — far above blind chance (1/4); "+
				"the count items may be leaking their answer without the image", countOK, countN)
		}
		return
	}
	if aborts > 0 {
		t.Errorf("%d/%d items aborted — an honest VLM must answer every item", aborts, len(qs))
	}
	if okN < 15 {
		t.Fatalf("a real VLM must pass the vision probe: %d/16 (floor %.3f)", okN, floor)
	}
}
