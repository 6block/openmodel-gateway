package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"

	"openmodel/sp-state-agent/internal/worker"
)

// probe_speech.go — admission probes for speech models.
//
// ASR. The gateway carries a small bank of spoken digits (two voices × 0–9, synthesized
// once and embedded below). Each probe item draws a fresh random 4-digit sequence and a
// voice, concatenates the clips with short pauses into one WAV, and asks the worker to
// transcribe it. The score is whether the transcript reads back those exact four digits,
// in order. The answer cannot be rehearsed: there are 2×10⁴ possible clips per item, the
// digit order is drawn per item, and the worker cannot tell the probe from a paying
// upload (same route, same headers, same mTLS client).
//
// Scoring is deliberately lenient about FORM and strict about CONTENT: Whisper writes
// "5, 2, 6, 0.", "eight, one, five, nine." or "1-1-4-7" for the same audio, so digits are
// read whether written as numerals or words, and everything else is ignored.
//
// TTS. Each item sends a fresh random sentence and checks the returned audio itself:
//
//	decode     a real PCM WAV, not text or an error page
//	duration   plausible for the character count — a canned clip, or one that ignores
//	           the input, does not scale with it
//	not silent the energy is above the noise floor and varies over time (a tone or a
//	           constant hum has flat energy)
//	diversity  different inputs give different audio (the same canned-replay catch the
//	           image probe uses)
//
//	content    when another admitted ASR worker is available, the audio is transcribed
//	           there and must contain the words that were asked for
//
// The first four are shape checks, and measured impersonators showed they are not
// enough on their own: noise modulated at a syllable rate, or one recording stretched to
// the expected length, passes every one of them. The content check is what refuses
// those. It needs an ASR the gateway already trusts, so it is used when the fleet has
// one and skipped (and logged as skipped) when it does not.
//
// What neither probe establishes is the exact model. A smaller recogniser
// (whisper-tiny for whisper-small) or a cheaper synthesizer (espeak-ng for Kokoro)
// passes, as a smaller diffusion model passes the image probe: the probes refuse
// workers that do not do the work, not workers that do it with a lesser model. That is
// left to receipts and reputation.

//go:embed probeaudio/*.wav
var probeAudioFS embed.FS

var asrProbeVoices = []string{"af", "am"}

// asrModelMarkers / ttsModelMarkers identify speech model families by name, matched on
// the normalised key like the other families. Repo names normalise without their short
// aliases, so the repo-name families are listed explicitly.
var (
	asrModelMarkers = []string{"whisper", "fasterwhisper", "distilwhisper", "paraformer", "sensevoice"}
	ttsModelMarkers = []string{"kokoro", "cosyvoice", "melotts", "piper", "xtts", "chattts"}
)

func isASRModel(model string) bool { return containsAnyMarker(model, asrModelMarkers) }
func isTTSModel(model string) bool { return containsAnyMarker(model, ttsModelMarkers) }

func containsAnyMarker(model string, markers []string) bool {
	norm := normalizeModelKey(model)
	for _, m := range markers {
		if strings.Contains(norm, normalizeModelKey(m)) {
			return true
		}
	}
	return false
}

// ---- ASR ---------------------------------------------------------------------------

type asrProbeItem struct {
	Digits []int
	Voice  string
}

func (a *auditor) generateASR(n int) []asrProbeItem {
	if n <= 0 {
		n = 16
	}
	a.rngMu.Lock()
	defer a.rngMu.Unlock()
	items := make([]asrProbeItem, n)
	for i := range items {
		d := make([]int, 4)
		for j := range d {
			d[j] = a.rng.Intn(10)
		}
		items[i] = asrProbeItem{Digits: d, Voice: asrProbeVoices[a.rng.Intn(len(asrProbeVoices))]}
	}
	return items
}

// buildDigitWAV concatenates the embedded clips with 0.35 s of silence after each, as
// one 16 kHz mono 16-bit WAV.
func buildDigitWAV(item asrProbeItem) ([]byte, error) {
	var pcm []byte
	gap := make([]byte, int(0.35*16000)*2)
	for _, d := range item.Digits {
		clip, err := probeAudioFS.ReadFile(fmt.Sprintf("probeaudio/%s_%d.wav", item.Voice, d))
		if err != nil {
			return nil, err
		}
		data, err := wavPCM16Mono16k(clip)
		if err != nil {
			return nil, err
		}
		pcm = append(pcm, data...)
		pcm = append(pcm, gap...)
	}
	return encodeWAV16k(pcm), nil
}

// wavPCM16Mono16k returns the data chunk of one of the embedded clips, checking the
// format it was generated in.
func wavPCM16Mono16k(b []byte) ([]byte, error) {
	off := 12
	var fmtOK bool
	for off+8 <= len(b) {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := off + 8
		switch id {
		case "fmt ":
			ch := binary.LittleEndian.Uint16(b[body+2 : body+4])
			rate := binary.LittleEndian.Uint32(b[body+4 : body+8])
			bits := binary.LittleEndian.Uint16(b[body+14 : body+16])
			fmtOK = ch == 1 && rate == 16000 && bits == 16
		case "data":
			if !fmtOK {
				return nil, fmt.Errorf("probe clip is not 16 kHz mono 16-bit")
			}
			return b[body : body+size], nil
		}
		off = body + size + size%2
	}
	return nil, fmt.Errorf("probe clip has no data chunk")
}

func encodeWAV16k(pcm []byte) []byte {
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + len(pcm)))
	b.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1))
	w(uint16(1))
	w(uint32(16000))
	w(uint32(32000))
	w(uint16(2))
	w(uint16(16))
	b.WriteString("data")
	w(uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

var spokenDigits = map[string]string{
	"zero": "0", "oh": "0", "o": "0", "one": "1", "two": "2", "to": "2", "too": "2",
	"three": "3", "four": "4", "for": "4", "five": "5", "six": "6", "seven": "7",
	"eight": "8", "nine": "9",
}

var tokenRE = regexp.MustCompile(`[0-9]|[a-zA-Z]+`)

// transcriptDigits reads the digits out of a transcript, accepting numerals and words.
func transcriptDigits(text string) string {
	var out strings.Builder
	for _, tok := range tokenRE.FindAllString(text, -1) {
		if len(tok) == 1 && tok[0] >= '0' && tok[0] <= '9' {
			out.WriteString(tok)
			continue
		}
		if d, ok := spokenDigits[strings.ToLower(tok)]; ok {
			out.WriteString(d)
		}
	}
	return out.String()
}

func scoreASRItem(transcript string, item asrProbeItem) bool {
	want := ""
	for _, d := range item.Digits {
		want += fmt.Sprint(d)
	}
	return transcriptDigits(transcript) == want
}

func (a *auditor) runASRProbe(ctx context.Context, w worker.Worker, model string) (correct, asked int, err error) {
	for _, item := range a.generateASR(a.cfg.NumQuestions) {
		wav, berr := buildDigitWAV(item)
		if berr != nil {
			return 0, 0, berr
		}
		asked++
		text, serr := a.sendASRProbe(ctx, w, model, wav)
		if serr != nil {
			return 0, 0, serr
		}
		if scoreASRItem(text, item) {
			correct++
		} else {
			a.logger.Info("asr probe item failed", "worker", w.ID, "model", model,
				"want", item.Digits, "got", text)
		}
	}
	return correct, asked, nil
}

func (a *auditor) sendASRProbe(ctx context.Context, w worker.Worker, model string, wav []byte) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", model)
	fw, _ := mw.CreateFormFile("file", "audio.wav")
	fw.Write(wav)
	mw.Close()
	raw, status, err := a.postProbe(ctx, w, "/v1/audio/transcriptions", mw.FormDataContentType(), body.Bytes())
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("worker returned %d", status)
	}
	var parsed struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return "", nil // an unparseable reply is a wrong answer, not a transport abort
	}
	return parsed.Text, nil
}

// ---- TTS ---------------------------------------------------------------------------

var ttsProbeWords = []string{
	"river", "garden", "window", "silver", "morning", "basket", "candle", "harbor",
	"orange", "pencil", "thunder", "meadow", "lantern", "puzzle", "violin", "compass",
	"blanket", "rocket", "forest", "mirror", "tunnel", "island", "marble", "feather",
}

type ttsProbeItem struct{ Text string }

func (a *auditor) generateTTS(n int) []ttsProbeItem {
	if n <= 0 {
		n = 16
	}
	a.rngMu.Lock()
	defer a.rngMu.Unlock()
	items := make([]ttsProbeItem, n)
	for i := range items {
		k := 5 + a.rng.Intn(5)
		ws := make([]string, k)
		for j := range ws {
			ws[j] = ttsProbeWords[a.rng.Intn(len(ttsProbeWords))]
		}
		items[i] = ttsProbeItem{Text: "The " + strings.Join(ws, " ") + "."}
	}
	return items
}

// TTS duration bounds per input character. English speech runs ~55–85 ms per character
// (measured: Kokoro af_heart, 75 ms); a Han character is a whole syllable and runs far
// longer (measured: zf_xiaoxiao, 241 ms). The window covers both with margin so neither
// script nor a slow voice is refused, while a fixed clip — whose length ignores the input
// — still falls outside it for most items.
const (
	ttsMinMsPerChar = 25
	ttsMaxMsPerChar = 400
)

func scoreTTSItem(wav []byte, item ttsProbeItem) (bool, string) {
	d, kind, err := audioDuration(wav)
	if err != nil || kind != "wav" {
		return false, "reply is not a measurable WAV"
	}
	chars := len([]rune(item.Text))
	ms := d * 1000 / float64(chars)
	if ms < ttsMinMsPerChar || ms > ttsMaxMsPerChar {
		return false, fmt.Sprintf("duration %.2fs implausible for %d characters", d, chars)
	}
	samples, err := wavSamples(wav)
	if err != nil {
		return false, err.Error()
	}
	if !hasSpeechEnergy(samples) {
		return false, "audio is silent or has flat energy (no speech)"
	}
	return true, ""
}

// wavSamples decodes 16-bit PCM (mono or interleaved) into the first channel's samples
// in [-1, 1].
func wavSamples(b []byte) ([]float64, error) {
	off := 12
	var channels, bits int
	for off+8 <= len(b) {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := off + 8
		switch id {
		case "fmt ":
			channels = int(binary.LittleEndian.Uint16(b[body+2 : body+4]))
			bits = int(binary.LittleEndian.Uint16(b[body+14 : body+16]))
		case "data":
			if bits != 16 || channels < 1 {
				return nil, fmt.Errorf("expected 16-bit PCM")
			}
			end := body + size
			if end > len(b) {
				end = len(b)
			}
			stride := 2 * channels
			out := make([]float64, 0, (end-body)/stride)
			for i := body; i+1 < end; i += stride {
				out = append(out, float64(int16(binary.LittleEndian.Uint16(b[i:i+2])))/32768)
			}
			return out, nil
		}
		off = body + size + size%2
	}
	return nil, fmt.Errorf("no data chunk")
}

// hasSpeechEnergy: speech is loud enough overall and its short-time energy rises and
// falls (syllables, pauses). Silence fails the first test; a tone, a hum or stationary
// noise fails the second.
func hasSpeechEnergy(s []float64) bool {
	const win = 400 // 400 samples at 16–48 kHz: 8–25 ms frames
	if len(s) < win*8 {
		return false
	}
	var frames []float64
	for i := 0; i+win <= len(s); i += win {
		var e float64
		for _, v := range s[i : i+win] {
			e += v * v
		}
		frames = append(frames, math.Sqrt(e/win))
	}
	var mean, max float64
	for _, f := range frames {
		mean += f
		if f > max {
			max = f
		}
	}
	mean /= float64(len(frames))
	if max < 0.02 { // ≈ −34 dBFS peak frame: nothing audible was produced
		return false
	}
	var v float64
	for _, f := range frames {
		v += (f - mean) * (f - mean)
	}
	cv := math.Sqrt(v/float64(len(frames))) / (mean + 1e-9)
	return cv > 0.35 // speech energy varies strongly; tones and hum are nearly flat
}

// ttsContentRecall is the share of an item's words that must appear in the verifier's
// transcript. Measured: espeak-ng output scored 0.60–1.00 on whisper-small, noise
// modulated at a syllable rate 0.00; an unrelated recording names words from the
// item's 24-word vocabulary only by accident.
const ttsContentRecall = 0.5

// asrVerifier picks the worker that transcribes a TTS probe's audio: an admitted ASR
// worker other than the one under test, preferring one run by a different miner. A
// worker under test could otherwise answer its own check — its ASR knows what its TTS
// was just asked to say. If the only ASR in the fleet is on the worker itself, that one
// is used and the check is reported as self-verified: it still refuses audio that does
// not say the words, but not a worker that fakes both sides.
func (a *auditor) asrVerifier(subject worker.Worker) (v worker.Worker, model string, self, ok bool) {
	if a.gw == nil || a.gw.registry == nil {
		return worker.Worker{}, "", false, false
	}
	var same, otherMiner, sameMiner *worker.Worker
	var sameM, otherM, sameMinerM string
	for _, w := range a.gw.registry.List() {
		w := w
		if w.IsBanned() || (w.State != worker.StateIdle && w.State != worker.StateBusy) {
			continue
		}
		for _, m := range loadedModels(&w) {
			if !isASRModel(m) || (w.SelfRegistered && !workerVerifiedFor(&w, m)) {
				continue
			}
			switch {
			case w.ID == subject.ID:
				same, sameM = &w, m
			case w.MinerAddress == "" || w.MinerAddress != subject.MinerAddress:
				otherMiner, otherM = &w, m
			default:
				sameMiner, sameMinerM = &w, m
			}
			break
		}
	}
	switch {
	case otherMiner != nil:
		return *otherMiner, otherM, false, true
	case sameMiner != nil:
		return *sameMiner, sameMinerM, false, true
	case same != nil:
		return *same, sameM, true, true
	}
	return worker.Worker{}, "", false, false
}

// wordRecall is the share of the item's content words (all but the leading "The") that
// appear in the transcript, case-insensitively.
func wordRecall(item ttsProbeItem, transcript string) float64 {
	heard := map[string]bool{}
	for _, tok := range tokenRE.FindAllString(strings.ToLower(transcript), -1) {
		heard[tok] = true
	}
	words := tokenRE.FindAllString(strings.ToLower(item.Text), -1)
	if len(words) > 0 && words[0] == "the" {
		words = words[1:]
	}
	if len(words) == 0 {
		return 0
	}
	hit := 0
	for _, wd := range words {
		if heard[wd] {
			hit++
		}
	}
	return float64(hit) / float64(len(words))
}

func (a *auditor) runTTSProbe(ctx context.Context, w worker.Worker, model string) (correct, asked int, err error) {
	digests := map[[32]byte]bool{}
	verifier, asrModel, self, haveASR := a.asrVerifier(w)
	if haveASR {
		a.logger.Info("tts probe: content checked by transcription", "worker", w.ID, "model", model,
			"verifier", verifier.ID, "asr_model", asrModel, "self_verified", self)
	} else {
		a.logger.Warn("tts probe: no admitted ASR worker — content NOT verified, shape checks only",
			"worker", w.ID, "model", model)
	}
	for _, item := range a.generateTTS(a.cfg.NumQuestions) {
		body, _ := json.Marshal(map[string]string{"model": model, "input": item.Text, "response_format": "wav"})
		asked++
		raw, status, perr := a.postProbe(ctx, w, "/v1/audio/speech", "application/json", body)
		if perr != nil {
			return 0, 0, perr
		}
		if status != http.StatusOK {
			return 0, 0, fmt.Errorf("worker returned %d", status)
		}
		ok, why := scoreTTSItem(raw, item)
		if ok && haveASR {
			text, terr := a.sendASRProbe(ctx, verifier, asrModel, raw)
			if terr != nil {
				// The verifier failing is not the subject's fault: drop the content check
				// for the rest of the round rather than score the subject on it.
				a.logger.Warn("tts probe: verifier transcription failed; content check off for this round",
					"worker", w.ID, "verifier", verifier.ID, "error", terr)
				haveASR = false
			} else if r := wordRecall(item, text); r < ttsContentRecall {
				ok, why = false, fmt.Sprintf("audio does not say the requested words (recall %.2f, heard %q)", r, text)
			}
		}
		if !ok {
			a.logger.Info("tts probe item failed", "worker", w.ID, "model", model, "reason", why)
			continue
		}
		digests[sha256.Sum256(raw)] = true
		correct++
	}
	// Diversity: distinct inputs must give distinct audio (canned replay).
	if correct > 1 && len(digests) < correct {
		a.logger.Info("tts probe: repeated identical audio across different inputs",
			"worker", w.ID, "model", model, "distinct", len(digests), "scored", correct)
		correct = len(digests)
	}
	return correct, asked, nil
}

// postProbe sends one probe request over the same path, headers and mTLS client a
// paying request uses, so the probe cannot be told apart from real traffic.
func (a *auditor) postProbe(ctx context.Context, w worker.Worker, path, contentType string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(w.Endpoint, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Request-ID", generateRequestID())
	if w.HasFeature(worker.FeatureReceipt) {
		req.Header.Set("X-OM-Receipt-Req", "1")
	}
	if w.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+w.AuthToken)
	}
	client := a.client
	if a.gw != nil && a.gw.workerMTLS.Enabled() {
		client = a.gw.workerMTLS.ClientFor(w.ID, a.client.Timeout, nil)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return raw, resp.StatusCode, err
}
