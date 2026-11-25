package gateway

import (
	"math"
	"math/rand"
	"testing"
)

func TestSpeechModelFamilies(t *testing.T) {
	for m, want := range map[string]string{
		"/models/Systran--faster-whisper-small":    "asr",
		"openai/whisper-large-v3-turbo":            "asr",
		"/models/hexgrad--Kokoro-82M":              "tts",
		"FunAudioLLM/CosyVoice2-0.5B":              "tts",
		"/models/Qwen--Qwen3-32B-AWQ":              "text",
		"stabilityai/stable-diffusion-xl-base-1.0": "image_gen",
	} {
		if got := probeKind(m); got != want {
			t.Errorf("probeKind(%q) = %s, want %s", m, got, want)
		}
	}
}

func TestSpeechFloorsResolveForRealPaths(t *testing.T) {
	a := &auditor{cfg: ProbeConfig{MinScore: 0.99}}
	if f := a.floorFor("/models/Systran--faster-whisper-small"); f != 40.0/48.0 {
		t.Errorf("whisper floor = %v, want 40/48 (fell through to min_score?)", f)
	}
	if f := a.floorFor("/models/hexgrad--Kokoro-82M"); f != 24.0/48.0 {
		t.Errorf("kokoro floor = %v, want 24/48", f)
	}
}

// TestTranscriptDigits: the forms Whisper actually produced for the probe's audio on
// real hardware (numerals with commas, words, hyphens, full stops) all read back.
func TestTranscriptDigits(t *testing.T) {
	for in, want := range map[string]string{
		"5, 2, 6, 0.":             "5260",
		"eight, one, five, nine.": "8159",
		"1-1-4-7":                 "1147",
		"2. 9. 1. 7.":             "2917",
		"seven, six, zero, one.":  "7601",
		"Six, one, three, one.":   "6131",
		"I don't know.":           "",
	} {
		if got := transcriptDigits(in); got != want {
			t.Errorf("transcriptDigits(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDigitWAVIsMeasurableAndSized(t *testing.T) {
	item := asrProbeItem{Digits: []int{3, 1, 4, 1}, Voice: "af"}
	wav, err := buildDigitWAV(item)
	if err != nil {
		t.Fatal(err)
	}
	d, kind, err := audioDuration(wav)
	if err != nil || kind != "wav" {
		t.Fatal(err)
	}
	if d < 2 || d > 8 {
		t.Fatalf("a 4-digit probe clip should be a few seconds, got %.2fs", d)
	}
}

// ASR impersonators: a worker that returns a fixed or random transcript. A random
// 4-digit guess passes with probability 1e-4 per item.
func TestASRScoringRejectsGuessers(t *testing.T) {
	a := &auditor{rng: rand.New(rand.NewSource(1))}
	items := a.generateASR(48)
	fixed, random := 0, 0
	r := rand.New(rand.NewSource(2))
	for _, it := range items {
		if scoreASRItem("1, 2, 3, 4.", it) {
			fixed++
		}
		guess := ""
		for i := 0; i < 4; i++ {
			guess += string(rune('0' + r.Intn(10)))
		}
		if scoreASRItem(guess, it) {
			random++
		}
	}
	if fixed > 1 || random > 1 {
		t.Fatalf("guessers scored fixed=%d random=%d of 48; the floor would be meaningless", fixed, random)
	}
}

// TTS impersonators measured offline: silence, a constant tone, white noise, and the
// wrong length must all fail; a speech-shaped signal of the right length passes.
func TestTTSScoringBothSides(t *testing.T) {
	item := ttsProbeItem{Text: "The river garden window silver morning."} // 39 chars
	const rate = 24000
	mk := func(seconds float64, f func(i int) float64) []byte {
		n := int(seconds * rate)
		pcm := make([]byte, 2*n)
		for i := 0; i < n; i++ {
			v := int16(math.Max(-1, math.Min(1, f(i))) * 32000)
			pcm[2*i], pcm[2*i+1] = byte(v), byte(uint16(v)>>8)
		}
		return wavFromPCM(rate, pcm)
	}
	r := rand.New(rand.NewSource(3))
	// syllable-like bursts: a carrier gated on/off ~4 times a second
	speechy := func(i int) float64 {
		t := float64(i) / rate
		gate := 0.0
		if math.Sin(2*math.Pi*4*t) > 0 {
			gate = 1
		}
		return gate * 0.4 * math.Sin(2*math.Pi*180*t)
	}
	cases := []struct {
		name string
		wav  []byte
		pass bool
	}{
		{"speech-like, right length", mk(2.6, speechy), true},
		{"silence", mk(2.6, func(int) float64 { return 0 }), false},
		{"steady tone", mk(2.6, func(i int) float64 { return 0.4 * math.Sin(2*math.Pi*440*float64(i)/rate) }), false},
		{"white noise", mk(2.6, func(int) float64 { return r.Float64()*0.8 - 0.4 }), false},
		{"canned 0.4s clip", mk(0.4, speechy), false},
		{"canned 30s clip", mk(30, speechy), false},
	}
	for _, c := range cases {
		ok, why := scoreTTSItem(c.wav, item)
		if ok != c.pass {
			t.Errorf("%s: pass=%v (%s), want %v", c.name, ok, why, c.pass)
		}
	}
}

func wavFromPCM(rate int, pcm []byte) []byte {
	b := encodeWAV16k(pcm)
	// patch the rate fields for a non-16k signal
	put := func(off int, v uint32) {
		b[off], b[off+1], b[off+2], b[off+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
	}
	put(24, uint32(rate))
	put(28, uint32(rate*2))
	return b
}

// TestTTSDurationWindowFitsMeasuredSpeech: the per-character window must admit the rates
// measured from real Kokoro output in both scripts the probe or users exercise.
func TestTTSDurationWindowFitsMeasuredSpeech(t *testing.T) {
	for name, msPerChar := range map[string]float64{"english af_heart": 75, "mandarin zf_xiaoxiao": 241} {
		if msPerChar < ttsMinMsPerChar || msPerChar > ttsMaxMsPerChar {
			t.Errorf("%s at %.0f ms/char falls outside [%d, %d]", name, msPerChar, ttsMinMsPerChar, ttsMaxMsPerChar)
		}
	}
}
