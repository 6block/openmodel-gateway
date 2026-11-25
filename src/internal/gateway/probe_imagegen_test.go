package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"openmodel/sp-state-agent/internal/settlement"
	"openmodel/sp-state-agent/internal/worker"
)

// probe_imagegen_test.go — the image-generation admission probe.
//
// The point of these tests is not that a good image passes; it is that each IMPERSONATION
// fails. The floor's whole justification is "a responder that does not really render
// cannot reach it", so every refusal path is exercised against a stand-in that fabricates
// output in a different way.

// renderRedCircle draws the subject the probe prompt asks for: a red disc centred on a
// white field. This is the honest side.
func renderRedCircle(size int, seed int64) []byte {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	white := color.RGBA{255, 255, 255, 255}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.Set(x, y, white)
		}
	}
	r := rand.New(rand.NewSource(seed))
	// A slightly jittered centre and radius, so "same seed → same bytes" holds while two
	// different seeds genuinely differ (the diversity gate depends on that).
	//nolint:gosec // test-only determinism, not security
	cx := size/2 + r.Intn(size/16+1) - size/32
	cy := size/2 + r.Intn(size/16+1) - size/32
	rad := size/4 + r.Intn(size/16+1)
	red := color.RGBA{220, 30, 30, 255}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= rad*rad {
				img.Set(x, y, red)
			}
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func encodeImages(imgs ...[]byte) string {
	data := make([]map[string]string, 0, len(imgs))
	for _, b := range imgs {
		data = append(data, map[string]string{"b64_json": base64.StdEncoding.EncodeToString(b)})
	}
	out, _ := json.Marshal(map[string]any{"data": data, "usage": map[string]int{"images": len(imgs)}})
	return string(out)
}

func probeItem(size int, seed int64) imageGenItem {
	return imageGenItem{Seed: seed, Size: fmt.Sprintf("%dx%d", size, size), Width: size, Height: size}
}

// --- the honest side ---------------------------------------------------------------

func TestScoreImageGen_AcceptsAHonestRender(t *testing.T) {
	item := probeItem(256, 1)
	ok, reason := scoreImageGenItem([][]byte{renderRedCircle(256, 1)}, item)
	if !ok {
		t.Fatalf("a red circle on white must score: %s", reason)
	}
}

func TestScoreImageGen_AcceptsAnOffCentreOrSoftSubject(t *testing.T) {
	// A model that draws the circle a bit off-centre, or with a paler red, is still
	// honest. The check must be coarse enough not to refuse it.
	size := 256
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.Set(x, y, color.RGBA{245, 245, 240, 255}) // faintly tinted background
		}
	}
	// Off-centre disc, softer red.
	cx, cy, rad := size*5/8, size*5/8, size/5
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= rad*rad {
				img.Set(x, y, color.RGBA{180, 70, 70, 255})
			}
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	ok, reason := scoreImageGenItem([][]byte{buf.Bytes()}, probeItem(size, 1))
	if !ok {
		t.Fatalf("an off-centre, softly-coloured render must still pass: %s", reason)
	}
}

// --- the impersonation side: every refusal path -------------------------------------

func TestScoreImageGen_RefusesEachFabrication(t *testing.T) {
	size := 256
	item := probeItem(size, 7)

	// 1) Not an image at all (a text blob or an error page).
	if ok, _ := scoreImageGenItem([][]byte{[]byte("sorry, I cannot do that")}, item); ok {
		t.Error("a non-image payload must be refused")
	}

	// 2) A valid image of the WRONG size (a canned thumbnail).
	small := renderRedCircle(64, 1)
	if ok, reason := scoreImageGenItem([][]byte{small}, item); ok {
		t.Errorf("a wrong-sized image must be refused (was %s)", reason)
	}

	// 3) A blank canvas — the classic stub.
	blank := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			blank.Set(x, y, color.RGBA{255, 255, 255, 255})
		}
	}
	var bb bytes.Buffer
	png.Encode(&bb, blank)
	if ok, reason := scoreImageGenItem([][]byte{bb.Bytes()}, item); ok {
		t.Errorf("a flat canvas must be refused (was %s)", reason)
	}

	// 4) Correctly-sized NOISE: decodes, right size, not flat — the case that would slip
	//    through a weaker check. Random pixels have no red centre on a white surround.
	rng := rand.New(rand.NewSource(99))
	noise := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			noise.Set(x, y, color.RGBA{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255})
		}
	}
	var nb bytes.Buffer
	png.Encode(&nb, noise)
	if ok, reason := scoreImageGenItem([][]byte{nb.Bytes()}, item); ok {
		t.Errorf("correctly-sized noise must be refused (was %s)", reason)
	}

	// 5) No images at all.
	if ok, _ := scoreImageGenItem(nil, item); ok {
		t.Error("an empty response must be refused")
	}
}

// A WRONG SUBJECT: a real render, right size, not flat — but of something else. This is
// the strongest fabrication a stub could manage with a general-purpose image, and the
// prompt-shape check is what refuses it.
func TestScoreImageGen_RefusesARenderOfTheWrongSubject(t *testing.T) {
	size := 256
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			// A green field with a blue band: plausible-looking, but not the prompt.
			c := color.RGBA{40, 160, 60, 255}
			if y > size/2 {
				c = color.RGBA{40, 60, 200, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	if ok, reason := scoreImageGenItem([][]byte{buf.Bytes()}, probeItem(size, 1)); ok {
		t.Errorf("a render of the wrong subject must be refused (was %s)", reason)
	}
}

// --- probe round against a real HTTP stand-in ---------------------------------------

func imageGenProbeServer(t *testing.T, render func(item imageGenItem) string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Size string `json:"size"`
			Seed int64  `json:"seed"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		var w2, h2 int
		fmt.Sscanf(req.Size, "%dx%d", &w2, &h2)
		io.WriteString(w, render(imageGenItem{Seed: req.Seed, Size: req.Size, Width: w2, Height: h2}))
	}))
}

func newImageProbeAuditor(t *testing.T, endpoint string) *auditor {
	t.Helper()
	w := worker.Worker{ID: "w-img", Endpoint: endpoint, AuthToken: "tok"}
	return newAuditorForTest(t, w)
}

func TestRunImageGenProbe_HonestWorkerScoresFullMarks(t *testing.T) {
	srv := imageGenProbeServer(t, func(item imageGenItem) string {
		return encodeImages(renderRedCircle(item.Width, item.Seed))
	})
	defer srv.Close()

	a := newImageProbeAuditor(t, srv.URL)
	correct, asked, err := a.runImageGenProbe(context.Background(),
		worker.Worker{ID: "w-img", Endpoint: srv.URL, AuthToken: "tok"}, "flux-schnell")
	if err != nil {
		t.Fatalf("round errored: %v", err)
	}
	if asked == 0 {
		t.Fatal("no items were asked")
	}
	if correct != asked {
		t.Errorf("an honest renderer must score every item: %d/%d", correct, asked)
	}
}

// The fraud the per-item checks cannot catch on their own: a worker that returns ONE
// canned image for every request. It decodes, is the right size, is not flat, and happens
// to be a real render of the right subject — so every per-item check passes, and only the
// diversity gate refuses it.
func TestRunImageGenProbe_CatchesACannedImage(t *testing.T) {
	canned := renderRedCircle(256, 4242) // a REAL render, reused for every request
	srv := imageGenProbeServer(t, func(imageGenItem) string {
		return encodeImages(canned)
	})
	defer srv.Close()

	a := newImageProbeAuditor(t, srv.URL)
	correct, asked, err := a.runImageGenProbe(context.Background(),
		worker.Worker{ID: "w-img", Endpoint: srv.URL, AuthToken: "tok"}, "flux-schnell")
	if err != nil {
		t.Fatalf("round errored: %v", err)
	}
	if correct > 1 {
		t.Errorf("a worker replaying one image must not score %d/%d — the diversity gate "+
			"must collapse it to the number of distinct renders", correct, asked)
	}
}

func TestRunImageGenProbe_StubScoresZero(t *testing.T) {
	srv := imageGenProbeServer(t, func(imageGenItem) string {
		return encodeImages() // an empty data list: produced nothing
	})
	defer srv.Close()

	a := newImageProbeAuditor(t, srv.URL)
	correct, asked, err := a.runImageGenProbe(context.Background(),
		worker.Worker{ID: "w-img", Endpoint: srv.URL, AuthToken: "tok"}, "flux-schnell")
	if err != nil {
		t.Fatalf("round errored: %v", err)
	}
	if correct != 0 {
		t.Errorf("a worker that returns no images must score 0, got %d/%d", correct, asked)
	}
}

func TestRunImageGenProbe_NoiseScoresNearZero(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	srv := imageGenProbeServer(t, func(item imageGenItem) string {
		img := image.NewRGBA(image.Rect(0, 0, item.Width, item.Height))
		for y := 0; y < item.Height; y++ {
			for x := 0; x < item.Width; x++ {
				img.Set(x, y, color.RGBA{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255})
			}
		}
		var buf bytes.Buffer
		png.Encode(&buf, img)
		return encodeImages(buf.Bytes())
	})
	defer srv.Close()

	a := newImageProbeAuditor(t, srv.URL)
	correct, asked, err := a.runImageGenProbe(context.Background(),
		worker.Worker{ID: "w-img", Endpoint: srv.URL, AuthToken: "tok"}, "flux-schnell")
	if err != nil {
		t.Fatalf("round errored: %v", err)
	}
	// Each call re-uses the same RNG stream, so the images differ — the diversity gate
	// does not fire. What must hold is that the per-item shape check refuses them.
	if correct != 0 {
		t.Errorf("correctly-sized noise must score 0, got %d/%d", correct, asked)
	}
}

func TestRunImageGenProbe_TransportErrorAbortsUncounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "worker is mining", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	a := newImageProbeAuditor(t, srv.URL)
	correct, asked, err := a.runImageGenProbe(context.Background(),
		worker.Worker{ID: "w-img", Endpoint: srv.URL, AuthToken: "tok"}, "flux-schnell")
	if err == nil {
		t.Fatal("a 503 must abort the round")
	}
	if correct != 0 || asked != 0 {
		t.Errorf("an aborted round is uncounted, got correct=%d asked=%d", correct, asked)
	}
}

// --- family dispatch -----------------------------------------------------------------

func TestProbeFamilyDispatch(t *testing.T) {
	cases := []struct {
		model string
		kind  string
		gen   bool
		vis   bool
	}{
		{"/models/flux-schnell", "image_gen", true, false},
		{"FLUX.1-schnell", "image_gen", true, false},
		{"/models/Qwen--Qwen3-VL-8B-Instruct", "vision", false, true},
		{"qwen3-8b", "text", false, false},
		{"Qwen/Qwen2.5-3B-Instruct", "text", false, false},
	}
	for _, c := range cases {
		if got := probeKind(c.model); got != c.kind {
			t.Errorf("probeKind(%q) = %q, want %q", c.model, got, c.kind)
		}
		if got := isImageGenModel(c.model); got != c.gen {
			t.Errorf("isImageGenModel(%q) = %v, want %v", c.model, got, c.gen)
		}
		if got := isVisionModel(c.model); got != c.vis {
			t.Errorf("isVisionModel(%q) = %v, want %v", c.model, got, c.vis)
		}
	}
}

// The two marker sets must stay disjoint: an overlap would make dispatch order decide the
// family, which is exactly the silent coupling the table-driven dispatch exists to avoid.
func TestProbeMarkerSetsAreDisjoint(t *testing.T) {
	for _, vm := range vlmModelMarkers {
		for _, gm := range imageGenModelMarkers {
			if vm == gm {
				t.Fatalf("marker %q is in BOTH the vision and image-gen sets", vm)
			}
		}
	}
}

// The image-gen floor must exist for the families we ship, and be reachable by an honest
// worker (i.e. not 1.0, which no real renderer could guarantee on every item).
func TestImageGenFloorIsSane(t *testing.T) {
	a := newAuditorForTest(t, worker.Worker{})
	for _, model := range []string{"flux-schnell", "sdxl"} {
		f := a.floorFor(model)
		if f <= 0 || f >= 1 {
			t.Errorf("floor for %q must be in (0,1), got %v", model, f)
		}
	}
	// A model with no entry must fall back to the configured default, not to the
	// image-gen floor by accident.
	if got := a.floorFor("some-unknown-model"); got != 0.5 {
		t.Errorf("an unknown model must use the configured default, got %v", got)
	}
}

// --- test scaffolding ----------------------------------------------------------------

func newAuditorForTest(t *testing.T, w worker.Worker) *auditor {
	t.Helper()
	return &auditor{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		client: &http.Client{Timeout: 10 * time.Second},
		cfg:    ProbeConfig{NumQuestions: 8, MinScore: 0.5},
	}
}

// TestImageGenFloorMatchesRealModelPath: the SDXL weights directory is named after the
// upstream repo, which normalises to "...stablediffusionxl..." — no "sdxl" in it. The
// floor must still resolve to the measured image-gen value, not to probe.min_score.
func TestImageGenFloorMatchesRealModelPath(t *testing.T) {
	a := &auditor{cfg: ProbeConfig{MinScore: 0.9}}
	if f := a.floorFor("/models/stabilityai--stable-diffusion-xl-base-1.0"); f != 24.0/48.0 {
		t.Fatalf("SDXL path floor = %v, want the measured 24/48 (fell through to min_score?)", f)
	}
}

// TestImageGenProbeUsesTrainingResolution: the first real run scored an honest SDXL
// 0/16 at 256x256 with 4 steps — the probe measured its own parameters, not the model.
// Pin the parameters the floor was derived at.
func TestImageGenProbeUsesTrainingResolution(t *testing.T) {
	items := generateImageGen(4)
	for _, it := range items {
		if it.Width != 1024 || it.Height != 1024 || it.Size != "1024x1024" || it.Steps < 20 {
			t.Fatalf("probe item %+v: want 1024x1024 and >=20 steps (the floor was measured there)", it)
		}
	}
	body, err := imageGenProbeBody("m", items[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"steps":24`)) {
		t.Fatalf("probe request must carry its step count, got %s", body)
	}
}

// TestPromptShapeAcceptsOutlineRing: an honest render often draws the circle as a thin
// pale-red OUTLINE on white. The old thresholds (≥5% red, ≥3.3% connected) refused most
// of those; the measured fix must accept one while still refusing scattered red specks.
func TestPromptShapeAcceptsOutlineRing(t *testing.T) {
	const n = 512
	ring := image.NewRGBA(image.Rect(0, 0, n, n))
	specks := image.NewRGBA(image.Rect(0, 0, n, n))
	r := rand.New(rand.NewSource(7))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			white := color.RGBA{240, 240, 240, 255}
			ring.Set(x, y, white)
			specks.Set(x, y, white)
			dx, dy := float64(x-n/2), float64(y-n/2)
			d := math.Sqrt(dx*dx + dy*dy)
			if d > 150 && d < 158 { // an 8px pale-red ring
				ring.Set(x, y, color.RGBA{215, 120, 125, 255})
			}
			if r.Intn(60) == 0 { // ~1.7% isolated red pixels
				specks.Set(x, y, color.RGBA{220, 30, 30, 255})
			}
		}
	}
	if !hasPromptShape(ring) {
		t.Fatal("a pale-red outline ring on white must pass the prompt-shape check")
	}
	if hasPromptShape(specks) {
		t.Fatal("scattered red specks on white must not pass: no connected subject")
	}
}

// A model whose name carries no family marker is audited by its catalog modality, and
// held to that family's floor: "segmind/SSD-1B" is an image generator, and the name
// alone sent it the chat probe (which it cannot answer) at the global min_score.
func TestProbeFamilyFollowsTheCatalogModality(t *testing.T) {
	const ssd = "/models/segmind--SSD-1B"
	cfg := imageCfg()
	cfg.ModelCatalog[ssd] = cfg.ModelCatalog[imageModel]
	cfg.ModelCatalog["/models/acme--ears-v2"] = settlement.ModelInfo{UnitUSD: "1",
		Modality: settlement.ModalityASR, PriceUnit: settlement.PriceUnitAudioSecond}
	up := imageUpstream(t, 1, "")
	defer up.Close()
	gw, _ := newImageGateway(t, up, cfg)
	defer gw.Close()
	a := newAuditor(ProbeConfig{NumQuestions: 4, MinScore: 0.5}, gw, slog.New(slog.NewTextHandler(io.Discard, nil)))

	for model, want := range map[string]string{
		ssd:                                     "image_gen",
		"segmind/SSD-1B":                        "image_gen", // listed spelling of the same catalog entry
		"/models/acme--ears-v2":                 "asr",
		"/models/Qwen--Qwen3-32B-AWQ":           "text",
		"/models/Systran--faster-whisper-small": "asr", // no catalog entry: name markers
	} {
		if got := a.kindOf(model); got != want {
			t.Errorf("kindOf(%q) = %q, want %q", model, got, want)
		}
	}
	if f := a.floorFor(ssd); f != 24.0/48.0 {
		t.Errorf("floorFor(SSD-1B) = %v, want the image-generation floor 24/48", f)
	}
	if f := a.floorFor("/models/acme--ears-v2"); f != 40.0/48.0 {
		t.Errorf("floorFor(unmarked ASR) = %v, want the ASR floor 40/48", f)
	}
	if f := a.floorFor("totally-unknown"); f != 0.5 {
		t.Errorf("an unknown text model keeps min_score, got %v", f)
	}
}
