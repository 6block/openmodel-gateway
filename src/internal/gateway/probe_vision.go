package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
)

// probe_vision.go — admission probes for image-understanding (VLM) models.
//
// A worker that claims a vision model must prove it can actually SEE: each probe
// renders a fresh synthetic image (random colours, shape and count), embeds it as
// a data URI in an OpenAI multimodal message, and asks a question whose answer is
// fixed by the pixels. The scorer is deterministic (string / numeric match on the
// ANSWER line), so no LLM judge is involved and the item cannot be memorised —
// the image is different every run.
//
// A text-only model receiving one of these requests either rejects the content
// array or answers without the image; both score 0. That is the fraud this gate
// exists to catch: claiming a VLM while serving a text model behind it.

// vlmModelMarkers identify image-understanding models by family name, the same
// normalised way floorFor matches floors. Extend when a new VLM enters the catalog.
var vlmModelMarkers = []string{"qwen3vl", "qwen25vl", "qwen2vl", "vl8b", "vl7b", "vl3b"}

// isVisionModel reports whether a claimed model id names an image-understanding
// model (and therefore needs the vision probe rather than the text one).
func isVisionModel(model string) bool {
	norm := normalizeModelKey(model)
	for _, m := range vlmModelMarkers {
		if strings.Contains(norm, m) {
			return true
		}
	}
	return false
}

// visionPalette: colours a model can name unambiguously. Pairs are kept far apart
// in hue so a correct model has no legitimate reason to confuse them; the
// accepted answer set covers the common synonyms a model might use.
var visionPalette = []struct {
	name   string
	accept []string
	rgb    color.RGBA
}{
	{"red", []string{"red"}, color.RGBA{220, 20, 20, 255}},
	{"green", []string{"green"}, color.RGBA{20, 170, 40, 255}},
	{"blue", []string{"blue"}, color.RGBA{30, 60, 220, 255}},
	{"yellow", []string{"yellow"}, color.RGBA{240, 220, 30, 255}},
	{"white", []string{"white"}, color.RGBA{250, 250, 250, 255}},
	{"black", []string{"black"}, color.RGBA{10, 10, 10, 255}},
}

// visionProbeImage is one rendered probe image plus the ground truth it encodes.
type visionProbeImage struct {
	dataURI  string
	bgName   string
	bgAccept []string
	count    int // number of foreground shapes (1..4)
}

// renderVisionProbe draws a solid background with `count` filled circles of a
// contrasting colour, spaced so they never overlap, and returns it as a PNG data
// URI. Small (192×192) on purpose: the probe must cost a handful of visual tokens,
// not hundreds, and be quick to encode on every run.
func (a *auditor) renderVisionProbe() visionProbeImage {
	const side = 192
	bgIdx := a.rng.Intn(len(visionPalette))
	fgIdx := a.rng.Intn(len(visionPalette) - 1)
	if fgIdx >= bgIdx {
		fgIdx++ // any colour but the background's
	}
	// Avoid the two hardest foreground/background pairs for a small image: dark
	// on dark and light on light give a correct model a fair excuse to miscount.
	bad := func(x, y int) bool {
		n1, n2 := visionPalette[x].name, visionPalette[y].name
		return (n1 == "black" && n2 == "blue") || (n1 == "blue" && n2 == "black") ||
			(n1 == "white" && n2 == "yellow") || (n1 == "yellow" && n2 == "white")
	}
	for bad(bgIdx, fgIdx) {
		fgIdx = (fgIdx + 1) % len(visionPalette)
		if fgIdx == bgIdx {
			fgIdx = (fgIdx + 1) % len(visionPalette)
		}
	}
	count := a.rng.Intn(4) + 1

	img := image.NewRGBA(image.Rect(0, 0, side, side))
	bg := visionPalette[bgIdx].rgb
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			img.SetRGBA(x, y, bg)
		}
	}
	// Fixed slot grid (2×2) so up to four circles never touch; radius leaves a
	// clear margin inside each slot.
	slots := [][2]int{{48, 48}, {144, 48}, {48, 144}, {144, 144}}
	a.rng.Shuffle(len(slots), func(i, j int) { slots[i], slots[j] = slots[j], slots[i] })
	fg := visionPalette[fgIdx].rgb
	const r = 26
	for _, c := range slots[:count] {
		for y := c[1] - r; y <= c[1]+r; y++ {
			for x := c[0] - r; x <= c[0]+r; x++ {
				dx, dy := x-c[0], y-c[1]
				if dx*dx+dy*dy <= r*r {
					img.SetRGBA(x, y, fg)
				}
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img) // encoding an in-memory RGBA cannot fail
	return visionProbeImage{
		dataURI:  "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
		bgName:   visionPalette[bgIdx].name,
		bgAccept: visionPalette[bgIdx].accept,
		count:    count,
	}
}

// visionQuestion is a probe item whose answer depends on an image.
type visionQuestion struct {
	probeQuestion
	image visionProbeImage
}

// generateVision draws n fresh vision items, alternating the two question kinds
// (background colour / shape count) so a run always exercises both colour
// perception and counting — a model that only gets one of them is not a VLM
// operating correctly.
func (a *auditor) generateVision(n int) []visionQuestion {
	a.rngMu.Lock()
	defer a.rngMu.Unlock()
	out := make([]visionQuestion, 0, n)
	for i := 0; i < n; i++ {
		img := a.renderVisionProbe()
		var q probeQuestion
		if i%2 == 0 {
			q = probeQuestion{
				prompt:  "Look at the image. What is the background colour? Reply with the single colour word only.",
				answer:  img.bgName,
				numeric: false,
			}
		} else {
			q = numQ("Look at the image. How many circles are there? Reply with the number only.", img.count)
		}
		out = append(out, visionQuestion{probeQuestion: q, image: img})
	}
	return out
}

// visionProbeBody builds the OpenAI multimodal request body for one vision item:
// exactly the shape a paying user's image request takes, so the probe is
// indistinguishable from real traffic at the content layer. `/no_think` mirrors
// the text probe (Qwen3-VL has a thinking mode too).
func visionProbeBody(model string, q visionQuestion) []byte {
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": q.prompt + " Put the answer on the last line as ANSWER: <value>. /no_think"},
				{"type": "image_url", "image_url": map[string]string{"url": q.image.dataURI}},
			},
		}},
		"temperature": 0,
		"max_tokens":  256,
	})
	return body
}

// scoreVisionAnswer scores a vision reply. Colour items accept any of the
// palette synonyms (word-boundary match on the ANSWER line, same rule as the
// text scorer); count items use the numeric scorer.
func scoreVisionAnswer(q visionQuestion, response string) bool {
	if q.numeric {
		return scoreProbeAnswer(q.probeQuestion, response)
	}
	for _, acc := range q.image.bgAccept {
		if scoreProbeAnswer(probeQuestion{answer: acc, numeric: false}, response) {
			return true
		}
	}
	return false
}

// describeVisionItem renders a compact log form of one item (never the image).
func describeVisionItem(q visionQuestion) string {
	if q.numeric {
		return fmt.Sprintf("count=%d bg=%s", q.image.count, q.image.bgName)
	}
	return fmt.Sprintf("colour bg=%s count=%d", q.image.bgName, q.image.count)
}
