package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openmodel/sp-state-agent/internal/worker"
)

// --- unit: item generation and scoring -----------------------------------------

func TestVision_IsVisionModel(t *testing.T) {
	yes := []string{"/models/Qwen--Qwen3-VL-8B-Instruct", "Qwen/Qwen2.5-VL-7B-Instruct-AWQ", "qwen3-vl-8b"}
	no := []string{"/models/Qwen--Qwen3-32B-AWQ", "Qwen/Qwen2.5-3B-Instruct", "gpt-oss-20b", "qwen3.8-27b"}
	for _, m := range yes {
		if !isVisionModel(m) {
			t.Errorf("%q should be a vision model", m)
		}
	}
	for _, m := range no {
		if isVisionModel(m) {
			t.Errorf("%q should NOT be a vision model", m)
		}
	}
}

func TestVision_FloorResolvesToVisionTier(t *testing.T) {
	a := newAuditor(ProbeConfig{Enabled: true, MinScore: 0.5}, &Gateway{logger: testLogger()}, testLogger())
	if f := a.floorFor("/models/Qwen--Qwen3-VL-8B-Instruct"); f != 33.0/48.0 {
		t.Fatalf("Qwen3-VL floor: want 33/48, got %.4f (fell through to MinScore?)", f)
	}
}

// binomPMF is the probability mass function of Binomial(n, p) as a slice 0..n.
func binomPMF(n int, p float64) []float64 {
	pmf := make([]float64, n+1)
	c := 1.0
	for k := 0; k <= n; k++ {
		pmf[k] = c * math.Pow(p, float64(k)) * math.Pow(1-p, float64(n-k))
		c = c * float64(n-k) / float64(k+1)
	}
	return pmf
}

// pooledPMF convolves the two item kinds a vision window contains: the window
// alternates colour and count items (generateVision), so 24 of each over
// evidenceWindow=3 runs of 16.
func pooledPMF(colourP, countP float64) []float64 {
	const perKind = 16 * evidenceWindow / 2
	cw, nw := binomPMF(perKind, colourP), binomPMF(perKind, countP)
	out := make([]float64, 2*perKind+1)
	for i, ci := range cw {
		for j, nj := range nw {
			out[i+j] += ci * nj
		}
	}
	return out
}

func tailAtLeast(pmf []float64, floor float64) float64 {
	sum, start := 0.0, int(math.Ceil(floor*float64(len(pmf)-1)))
	for k := start; k < len(pmf); k++ {
		sum += pmf[k]
	}
	return sum
}

func tailBelow(pmf []float64, floor float64) float64 {
	sum, end := 0.0, int(math.Ceil(floor*float64(len(pmf)-1)))
	for k := 0; k < end && k < len(pmf); k++ {
		sum += pmf[k]
	}
	return sum
}

// TestVision_FloorSeparatesFraudFromCapability is the derivation of the vision
// floor, pinned so a later edit has to re-argue it rather than nudge the number.
//
// It asserts the three properties the floor comment claims: a blind text model
// cannot escape, an honest VLM is not falsely refused, and — the deliberate
// trade-off — a worker that sees colour but cannot count is more often caught
// than not. The last one is the guard against lowering the floor further.
func TestVision_FloorSeparatesFraudFromCapability(t *testing.T) {
	floor := defaultModelFloors["qwen3-vl-8b"]
	if floor != 33.0/48.0 {
		t.Fatalf("vision floor moved to %.4f — re-derive the numbers in this test", floor)
	}

	// Impersonation side: 1/6 on colour items, 1/4 on count items (blind guess).
	blind := pooledPMF(1.0/6.0, 1.0/4.0)
	if esc := tailAtLeast(blind, floor); esc > 1e-9 {
		t.Errorf("blind text model escapes at %.2e per window (want <1e-9)", esc)
	}

	// Honest side: a VLM that scores 0.85 pooled must be refused <1% of the time.
	// (0.85 is deliberately below the real model's expected score; the floor is
	// not entitled to assume an unmeasured ≥0.99.)
	if fp := tailBelow(pooledPMF(0.85, 0.85), floor); fp > 0.01 {
		t.Errorf("honest VLM at 0.85 falsely refused %.2f%% of the time (want <1%%)", fp*100)
	}

	// The trade-off this floor actually makes: colour-only perception (1.0 on
	// colour, chance 0.25 on count → pooled 0.625) must be caught more often than
	// not. Lowering the floor past 30/48 would silently flip this.
	colourOnly := pooledPMF(1.0, 0.25)
	if caught := tailBelow(colourOnly, floor); caught <= 0.5 {
		t.Errorf("colour-only worker caught only %.1f%% of the time at the current floor — "+
			"the floor is too loose to still mean anything on the vision family", caught*100)
	}
}

// decodeProbeImage decodes the data URI a probe item carries back into pixels —
// the mock worker below uses it to answer from the image, like a real VLM would.
func decodeProbeImage(t *testing.T, dataURI string) image.Image {
	t.Helper()
	const pfx = "data:image/png;base64,"
	if !strings.HasPrefix(dataURI, pfx) {
		t.Fatalf("probe image is not a PNG data URI: %.40s", dataURI)
	}
	raw, err := base64.StdEncoding.DecodeString(dataURI[len(pfx):])
	if err != nil {
		t.Fatalf("probe image base64: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("probe image png: %v", err)
	}
	return img
}

// nearest maps a pixel to the palette colour name it is closest to.
func nearest(r, g, b uint32) string {
	best, bestD := "", uint32(1<<32-1)
	for _, p := range visionPalette {
		pr, pg, pb := uint32(p.rgb.R), uint32(p.rgb.G), uint32(p.rgb.B)
		d := sq(pr, r) + sq(pg, g) + sq(pb, b)
		if d < bestD {
			best, bestD = p.name, d
		}
	}
	return best
}

func sq(a, b uint32) uint32 {
	if a > b {
		return (a - b) * (a - b)
	}
	return (b - a) * (b - a)
}

// countCircles counts foreground blobs by sampling the four fixed slot centres
// (the generator only ever draws at those positions).
func countCircles(img image.Image, bg string) int {
	n := 0
	for _, c := range [][2]int{{48, 48}, {144, 48}, {48, 144}, {144, 144}} {
		r, g, b, _ := img.At(c[0], c[1]).RGBA()
		if nearest(r>>8, g>>8, b>>8) != bg {
			n++
		}
	}
	return n
}

func TestVision_GeneratedItemsAreSelfConsistent(t *testing.T) {
	a := newAuditor(ProbeConfig{Enabled: true}, &Gateway{logger: testLogger()}, testLogger())
	items := a.generateVision(16)
	if len(items) != 16 {
		t.Fatalf("want 16 items, got %d", len(items))
	}
	colour, count := 0, 0
	for _, q := range items {
		img := decodeProbeImage(t, q.image.dataURI)
		if img.Bounds().Dx() != 192 || img.Bounds().Dy() != 192 {
			t.Fatalf("unexpected image size %v", img.Bounds())
		}
		// Corner pixel is always background.
		r, g, b, _ := img.At(2, 2).RGBA()
		if got := nearest(r>>8, g>>8, b>>8); got != q.image.bgName {
			t.Fatalf("background pixel reads %q, item says %q", got, q.image.bgName)
		}
		if got := countCircles(img, q.image.bgName); got != q.image.count {
			t.Fatalf("image has %d circles, item says %d", got, q.image.count)
		}
		if q.image.count < 1 || q.image.count > 4 {
			t.Fatalf("count out of range: %d", q.image.count)
		}
		if q.numeric {
			count++
			if !scoreVisionAnswer(q, "ANSWER: "+q.answer) {
				t.Fatal("count item rejects its own answer")
			}
		} else {
			colour++
			if !scoreVisionAnswer(q, "The background is "+q.image.bgName+".\nANSWER: "+q.image.bgName) {
				t.Fatal("colour item rejects its own answer")
			}
			if scoreVisionAnswer(q, "ANSWER: purple") {
				t.Fatal("colour item accepts a wrong colour")
			}
		}
	}
	if colour != 8 || count != 8 {
		t.Fatalf("want 8 colour + 8 count items, got %d + %d", colour, count)
	}
	// Body shape: OpenAI multimodal array, image as data URI, model echoed.
	var body map[string]any
	if err := json.Unmarshal(visionProbeBody("m", items[0]), &body); err != nil {
		t.Fatal(err)
	}
	parts := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 || parts[0].(map[string]any)["type"] != "text" || parts[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("unexpected content parts: %v", parts)
	}
}

// Two consecutive runs must not repeat the same images (unmemorisable).
func TestVision_ItemsAreFresh(t *testing.T) {
	a := newAuditor(ProbeConfig{Enabled: true}, &Gateway{logger: testLogger()}, testLogger())
	seen := map[string]bool{}
	dup := 0
	for _, q := range append(a.generateVision(8), a.generateVision(8)...) {
		if seen[q.image.dataURI] {
			dup++
		}
		seen[q.image.dataURI] = true
	}
	// 6 colours × 5 fg × 4 counts × slot permutations — collisions are rare, not zero.
	if dup > 3 {
		t.Fatalf("too many repeated probe images: %d/16", dup)
	}
}

// --- integration: a VLM claim is verified with image items -----------------------

// visionMockWorker is a fake VLM: it decodes the image in the request and answers
// from the pixels (correct), or — when blind — ignores the image and guesses a
// fixed colour / number (what a text model behind a fake VLM claim would do).
func visionMockWorker(t *testing.T, blind *bool, sawImage *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) == 0 {
			http.Error(w, "text-only content shape", 422) // a text model rejecting the array
			return
		}
		var text, uri string
		for _, p := range req.Messages[0].Content {
			switch p.Type {
			case "text":
				text = p.Text
			case "image_url":
				uri = p.ImageURL.URL
			}
		}
		ans := "ANSWER: blue"
		if strings.Contains(text, "How many") {
			ans = "ANSWER: 2"
		}
		if uri != "" {
			*sawImage++
		}
		if !*blind && uri != "" {
			img := decodeProbeImage(t, uri)
			r0, g0, b0, _ := img.At(2, 2).RGBA()
			bg := nearest(r0>>8, g0>>8, b0>>8)
			if strings.Contains(text, "How many") {
				ans = "ANSWER: " + string(rune('0'+countCircles(img, bg)))
			} else {
				ans = "ANSWER: " + bg
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": ans}}},
		})
	}))
}

func TestVision_AdmissionVerifiesRealVLMAndRejectsBlindOne(t *testing.T) {
	const vlm = "/models/Qwen--Qwen3-VL-8B-Instruct"
	blind := false
	saw := 0
	backend := visionMockWorker(t, &blind, &saw)
	defer backend.Close()

	reg := worker.NewRegistry(testLogger(), "")
	reg.SetAdmissionGate(true)
	for _, id := range []string{"sp-vlm", "sp-standby"} {
		reg.Register(worker.WorkerRegistration{
			ID: id, Endpoint: backend.URL, SchedulerURL: "http://w:9090",
			GPUCount: 1, SelfRegistered: true, SupportedModels: []string{vlm},
		})
		reg.UpdateState(id, "GPU_STATE_AVAILABLE", "running", 0, vlm, 1)
	}
	gw := &Gateway{registry: reg, logger: testLogger()}
	a := newAuditor(ProbeConfig{Enabled: true, IntervalSec: 1, NumQuestions: 8,
		MinScore: 0.5, BanSeconds: 60, AdmissionGate: true}, gw, testLogger())

	// A real VLM (answers from pixels) passes at 8/8 and gets verified.
	a.verifyModel(t.Context(), mustGet(t, reg, "sp-vlm"), vlm)
	if w, _ := reg.Get("sp-vlm"); !workerVerifiedFor(w, vlm) {
		t.Fatal("a worker answering from the image must have the VLM verified")
	}
	if saw != 8 {
		t.Fatalf("every probe item must carry an image; worker saw %d/8", saw)
	}

	// The same endpoint now answers blind (fixed guesses): pooled evidence fills
	// with ~1/4–1/6 scores, far under the 46/48 floor → removed and banned.
	blind = true
	for i := 0; i < 6; i++ {
		a.verifyModel(t.Context(), mustGet(t, reg, "sp-vlm"), vlm)
	}
	w, _ := reg.Get("sp-vlm")
	if workerVerifiedFor(w, vlm) {
		t.Fatal("a blind worker must lose the VLM from verified_models")
	}
	if !w.IsBanned() {
		t.Fatal("sustained blind answering across full windows must ban the worker")
	}
}

// A text-only worker that cannot even parse the multimodal content array returns
// a non-200: that is a transport-level abort (not counted), never a pass.
func TestVision_TextOnlyWorkerRejectingArrayIsNotAdmitted(t *testing.T) {
	const vlm = "/models/Qwen--Qwen3-VL-8B-Instruct"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"content must be a string"}`, http.StatusUnprocessableEntity)
	}))
	defer backend.Close()
	reg := worker.NewRegistry(testLogger(), "")
	reg.SetAdmissionGate(true)
	reg.Register(worker.WorkerRegistration{
		ID: "sp-text", Endpoint: backend.URL, SchedulerURL: "http://w:9090",
		GPUCount: 1, SelfRegistered: true, SupportedModels: []string{vlm},
	})
	reg.UpdateState("sp-text", "GPU_STATE_AVAILABLE", "running", 0, vlm, 1)
	a := newAuditor(ProbeConfig{Enabled: true, NumQuestions: 4, MinScore: 0.5, AdmissionGate: true},
		&Gateway{registry: reg, logger: testLogger()}, testLogger())
	a.verifyModel(t.Context(), mustGet(t, reg, "sp-text"), vlm)
	if w, _ := reg.Get("sp-text"); workerVerifiedFor(w, vlm) {
		t.Fatal("a worker that cannot accept image content must never be verified for a VLM")
	}
}
