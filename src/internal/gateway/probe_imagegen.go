package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/png" // registers the PNG decoder used to inspect the returned bytes
	"io"
	"math/rand"
	"net/http"
	"strings"

	"openmodel/sp-state-agent/internal/worker"
)

// probe_imagegen.go — the admission probe for image-generation models.
//
// WHY THIS IS NOT THE VISION PROBE. The vision probe asks a model to READ a rendered
// image and scores its ANSWER line. An image generation model produces pixels and answers
// nothing, so the same request shape would score every honest generator at zero. The
// probe must therefore (a) speak the images endpoint rather than chat/completions, and
// (b) score the BYTES returned, not any text.
//
// WHAT IT VERIFIES, and why each check is deterministic rather than a model judgement:
//
//	decode      — the payload is a real image, not a text blob or an error dressed up
//	尺寸        — the returned size is the one requested, so a canned thumbnail fails
//	not-blank   — a solid single-colour image is what a stub returns; a real render is not
//	seeded      — the SAME prompt with different seeds must give DIFFERENT bytes. This is
//	              the check that makes the probe un-rehearsable: a worker that caches one
//	              image, or replays a fixture, is caught by it while an honest renderer
//	              passes without effort.
//
// No LLM judge and no aesthetic scoring: every check is a byte-level assertion that can
// be reproduced from the response alone.

// imageGenModelMarkers identify image-generation models by family name, matched the same
// normalised way the vision markers are. Extend as new generators enter the catalog.
var imageGenModelMarkers = []string{
	"flux", "sdxl", "sd3", "stablediffusion", "stable-diffusion",
	"sd15", "sd21", "sd-turbo", "sdturbo", "playground",
}

// isImageGenModel reports whether a claimed model id names an image-generation model.
//
// Checked BEFORE isVisionModel, and the two marker sets are disjoint by construction:
// "flux" and "sdxl" contain no vision marker, and the vision markers (vl8b, qwen3vl…)
// contain none of these. If that ever stops being true, the order here decides — so the
// generator set is consulted first, being the more specific claim.
func isImageGenModel(model string) bool {
	norm := normalizeModelKey(model)
	for _, m := range imageGenModelMarkers {
		if strings.Contains(norm, m) {
			return true
		}
	}
	return false
}

// imageGenPrompt is the fixed instruction every image-generation probe sends. Kept
// mundane and unambiguous: the probe scores bytes, not aesthetics, so the prompt only
// has to elicit a real render.
const imageGenPrompt = "a single red circle centered on a plain white background"

// imageGenItem is one probe item: a seed to render with, and the size to ask for.
type imageGenItem struct {
	Seed   int64
	Size   string
	Width  int
	Height int
	Steps  int
}

// Probe render parameters. They must match how the model was TRAINED, not be as cheap
// as possible: the first real measurement (SDXL on a 3080, 2026-10-19) scored the
// honest model 0/16 at 256x256 with the engine's 4-step default — SDXL is trained at
// 1024 with ~20-30 steps, and at 256/4 it renders noise-coloured blobs, so the probe
// was measuring the parameters rather than the model. 1024x1024 at 24 steps costs
// ~10 s per item (~170 s per 16-item round on that card), which is affordable for an
// admission check that then holds for a week.
const (
	imageGenProbeSize  = "1024x1024"
	imageGenProbeWidth = 1024
	imageGenProbeSteps = 24
)

// generateImageGen builds NumQuestions fresh items, each with a distinct random seed so
// the render cannot be precomputed, and a small size to keep a probe round affordable.
func generateImageGen(n int) []imageGenItem {
	if n <= 0 {
		n = 16
	}
	items := make([]imageGenItem, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, imageGenItem{
			Seed:   rand.Int63(),
			Size:   imageGenProbeSize,
			Width:  imageGenProbeWidth,
			Height: imageGenProbeWidth,
			Steps:  imageGenProbeSteps,
		})
	}
	return items
}

// imageGenProbeBody renders the request the probe sends: the OpenAI images shape, so the
// probe travels the same path a paying request does.
func imageGenProbeBody(model string, item imageGenItem) ([]byte, error) {
	return json.Marshal(map[string]any{
		"model":           model,
		"prompt":          imageGenPrompt,
		"n":               1,
		"size":            item.Size,
		"steps":           item.Steps,
		"seed":            item.Seed,
		"response_format": "b64_json",
	})
}

// sendImageGenProbe posts an image-generation request to a worker and returns the decoded
// images. It deliberately mirrors sendProbe's transport discipline — same endpoint the
// forwarders use, same header set, same mTLS client — because a probe that is
// distinguishable from paying traffic is a probe that can be gamed.
func (a *auditor) sendImageGenProbe(ctx context.Context, w worker.Worker, body []byte) ([][]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(w.Endpoint, "/")+"/v1/images/generations", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", generateRequestID())
	if w.HasFeature(worker.FeatureReceipt) {
		req.Header.Set("X-OM-Receipt-Req", "1")
	}
	if w.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+w.AuthToken)
	}
	probeClient := a.client
	if a.gw != nil && a.gw.workerMTLS.Enabled() {
		probeClient = a.gw.workerMTLS.ClientFor(w.ID, a.client.Timeout, nil)
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("worker returned %d", resp.StatusCode)
	}
	// A generous cap: a 256x256 PNG is small, but a worker returning something huge is
	// exactly what this should refuse rather than buffer.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	// A body that does not parse, or carries no images, is a NEGATIVE RESULT for this item
	// (the worker produced nothing) rather than a transport abort: aborting would leave
	// the round uncounted and a worker that always returns empty could never be judged.
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, nil
	}
	out := make([][]byte, 0, len(parsed.Data))
	for _, item := range parsed.Data {
		b, derr := base64Decode(item.B64JSON)
		if derr != nil {
			// An undecodable payload is also a negative result, not an abort — see above.
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// scoreImageGenItem decides whether one probe item passed, from the bytes alone.
//
// Returns (ok, reason). The reason is for the audit log — a failure that says WHY is the
// difference between a reproducible finding and a mystery.
//
// The checks are ordered cheapest-first and each closes a distinct fraud:
//
//	decode + size   a text blob, an error page, or a fixed thumbnail
//	not flat        a stub that returns a blank canvas
//	prompt shape    NOISE. Correctly-sized random bytes decode, are the right size and are
//	                not flat, so they would pass everything above while costing a worker
//	                nothing. The prompt asks for a red circle on a white field, and a
//	                rendered answer has red pixels in the middle and white ones at the
//	                corners; noise has neither. Tolerances are generous — this is a
//	                coarse structural check, not an aesthetic one, so a model that draws
//	                the circle slightly off-centre or with a soft edge still passes.
func scoreImageGenItem(images [][]byte, item imageGenItem) (bool, string) {
	if len(images) == 0 {
		return false, "no images returned"
	}
	img, format, err := image.Decode(bytes.NewReader(images[0]))
	if err != nil {
		return false, fmt.Sprintf("undecodable image (%v)", err)
	}
	if format != "png" {
		return false, "expected a PNG payload, got " + format
	}
	b := img.Bounds()
	if b.Dx() != item.Width || b.Dy() != item.Height {
		return false, fmt.Sprintf("wrong size: asked %dx%d, got %dx%d",
			item.Width, item.Height, b.Dx(), b.Dy())
	}
	if isUniform(img) {
		return false, "image is a single flat colour (nothing was rendered)"
	}
	if !hasPromptShape(img) {
		return false, "image does not match the requested subject (no red centre / white surround)"
	}
	return true, ""
}

// hasPromptShape checks the coarse structure the prompt asks for: a light background with
// a substantial RED REGION that is CONNECTED.
//
// The connectivity test is what makes this discriminative. An earlier version only asked
// "is some pixel in the middle reddish, and some pixel at a corner light", and random
// noise passes that essentially always: across a 256×256 field there is near-certainly
// one reddish pixel near the centre and one light pixel in a corner. A rendered circle,
// by contrast, is ONE LARGE CONNECTED red region on a light field, while noise scatters
// its reddish pixels into thousands of one- and two-pixel specks. So the shape test is:
//
//	background — a large fraction of the image is light (the field the subject sits on)
//	subject    — the LARGEST CONNECTED reddish region is a meaningful share of the frame
//
// Both thresholds are deliberately loose, because the cost of refusing an honest render
// is worse than the cost of admitting an unusual one: a circle drawn off-centre, soft-
// edged, pale, or partly cropped still passes comfortably.
func hasPromptShape(img image.Image) bool {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 8 || h < 8 {
		return false
	}

	// Sample on a coarse grid: a real structure spans thousands of pixels, so a 4-pixel
	// stride cannot hide it, and this keeps the probe cheap on a large render.
	const stride = 4
	gw, gh := w/stride, h/stride
	red := make([]bool, gw*gh)
	light := 0
	redCount := 0
	for gy := 0; gy < gh; gy++ {
		for gx := 0; gx < gw; gx++ {
			c := color.RGBAModel.Convert(img.At(b.Min.X+gx*stride, b.Min.Y+gy*stride)).(color.RGBA)
			if isReddish(c) {
				red[gy*gw+gx] = true
				redCount++
			}
			if isNearWhite(c) {
				light++
			}
		}
	}
	total := gw * gh
	if total == 0 {
		return false
	}
	// A plain light field is what makes this discriminative against noise: uniform
	// random colour is light over only a few percent of the frame, so it fails here
	// regardless of what else it accidentally satisfies.
	if light < total/2 { // < 50% light: not "on a plain background"
		return false
	}
	// The subject must be present at all.
	if redCount < total/200 { // < 0.5% red: nothing was drawn
		return false
	}
	// The largest connected red region must be a real region, not the largest speck of
	// scattered noise. 4-neighbour flood fill over the sampled grid. The bar is low
	// because an honest render may draw the subject as a thin OUTLINE: measured on
	// real SDXL output, a pale 1px-thick ring is ~1-2% of the frame, while random
	// specks never exceed a fraction of a percent.
	largest := largestConnectedRegion(red, gw, gh)
	return largest >= total/200 // ≥ 0.5% of the frame in one connected piece
}

// largestConnectedRegion returns the size (in samples) of the biggest 4-connected run of
// true values. Iterative, so a large region cannot overflow the stack.
func largestConnectedRegion(grid []bool, w, h int) int {
	seen := make([]bool, len(grid))
	best := 0
	queue := make([]int, 0, 64)
	for start := range grid {
		if !grid[start] || seen[start] {
			continue
		}
		seen[start] = true
		queue = queue[:0]
		queue = append(queue, start)
		size := 0
		for len(queue) > 0 {
			idx := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			size++
			x, y := idx%w, idx/w
			// 4-neighbours inside the grid.
			if x > 0 && grid[idx-1] && !seen[idx-1] {
				seen[idx-1] = true
				queue = append(queue, idx-1)
			}
			if x < w-1 && grid[idx+1] && !seen[idx+1] {
				seen[idx+1] = true
				queue = append(queue, idx+1)
			}
			if y > 0 && grid[idx-w] && !seen[idx-w] {
				seen[idx-w] = true
				queue = append(queue, idx-w)
			}
			if y < h-1 && grid[idx+w] && !seen[idx+w] {
				seen[idx+w] = true
				queue = append(queue, idx+w)
			}
		}
		if size > best {
			best = size
		}
	}
	return best
}

// isReddish accepts a broad red range (a real render's circle has soft edges and shading,
// so requiring a saturated pure red would refuse honest output).
func isReddish(c color.RGBA) bool {
	// A broad, hue-based test rather than "near-pure red". Honest renders draw the
	// subject in many reds — pale pink outlines, shaded spheres, deep crimson fills —
	// and measuring them showed the stricter margin rejecting well over half of them
	// while adding nothing against noise, which the light-background check already
	// removes.
	return int(c.R) > 120 && int(c.R) > int(c.G)+15 && int(c.R) > int(c.B)+15
}

// isNearWhite accepts the light background under mild encoder noise or a faint tint.
func isNearWhite(c color.RGBA) bool {
	return int(c.R) > 170 && int(c.G) > 170 && int(c.B) > 170
}

// isUniform reports whether every pixel is the same colour (within a small tolerance for
// encoder noise). Sampling rather than scanning every pixel keeps this cheap on a large
// render; a real image varies far more than the stride can hide.
func isUniform(img image.Image) bool {
	b := img.Bounds()
	first := color.RGBAModel.Convert(img.At(b.Min.X, b.Min.Y)).(color.RGBA)
	step := 1
	if b.Dx() > 64 {
		step = b.Dx() / 64
	}
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			c := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
			if !closeColour(c, first) {
				return false
			}
		}
	}
	return true
}

// closeColour treats two colours as equal when each channel is within a small tolerance,
// so lossy encoding of a genuinely flat region still reads as flat.
func closeColour(a, b color.RGBA) bool {
	const tol = 6
	diff := func(x, y uint8) bool {
		d := int(x) - int(y)
		return d <= tol && d >= -tol
	}
	return diff(a.R, b.R) && diff(a.G, b.G) && diff(a.B, b.B)
}

// runImageGenProbe runs one full round against a claimed image-generation model.
//
// Two things are checked, and they catch different frauds:
//
//	per item      — the returned bytes decode, are the requested size, and are not a flat
//	                colour. Scored pass/fail, so the floor maths is the same binomial
//	                arithmetic the text and vision probes use.
//	across items  — the images must DIFFER from one another. Each item uses a fresh
//	                random seed, so an honest renderer returns different pixels every
//	                time. This is what catches the fraud the per-item checks cannot: a
//	                worker replaying one canned image (possibly a real render, so it
//	                decodes, is the right size, and is not blank) passes every per-item
//	                check while producing nothing.
func (a *auditor) runImageGenProbe(ctx context.Context, w worker.Worker, model string) (correct, asked int, err error) {
	items := generateImageGen(a.cfg.NumQuestions)
	digests := make(map[string]bool, len(items))
	for _, item := range items {
		body, berr := imageGenProbeBody(model, item)
		if berr != nil {
			return 0, 0, berr
		}
		asked++
		images, serr := a.sendImageGenProbe(ctx, w, body)
		if serr != nil {
			// A transport failure aborts the round uncounted, exactly as the text and
			// vision probes do — the caller decides whether it is retryable.
			return 0, 0, serr
		}
		if ok, reason := scoreImageGenItem(images, item); !ok {
			a.logger.Info("image-gen probe item failed", "worker", w.ID, "model", model,
				"seed", item.Seed, "reason", reason)
			continue
		}
		digests[imageGenProbeDigest(images)] = true
		correct++
	}
	// The diversity gate: enough DISTINCT images across the round. Applied to the whole
	// round rather than per item, because distinctness is a property of the set. A round
	// that produced one image every time scores zero however well each copy scored.
	if correct > 1 && len(digests) < correct {
		a.logger.Info("image-gen probe: repeated identical images across seeds",
			"worker", w.ID, "model", model,
			"distinct", len(digests), "scored", correct)
		// Scale the score down to the number of genuinely distinct renders: the worker
		// has shown it can produce this many different images, and no more.
		correct = len(digests)
	}
	return correct, asked, nil
}

// imageGenProbeDigest returns the sha256 of an item's images, used to detect a worker
// that returns one fixed image regardless of the request.
func imageGenProbeDigest(images [][]byte) string {
	h := sha256.New()
	for _, b := range images {
		h.Write(b)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func sameBytes(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

var _ = base64.StdEncoding
