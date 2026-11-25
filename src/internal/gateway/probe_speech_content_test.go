package gateway

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"openmodel/sp-state-agent/internal/worker"
)

// The TTS probe's shape checks alone let two measured impersonators through: noise
// gated at a syllable rate, and a recording stretched to the expected length. The
// content check — transcribing the audio on an admitted ASR worker — is what refuses
// them. These tests drive the real runTTSProbe against fake workers.

// syllableNoise is the measured impersonator: a carrier gated ~4 times a second, sized
// to the input. It passes every shape check.
func syllableNoise(text string) []byte {
	const rate = 24000
	n := int(float64(len([]rune(text))) * 0.075 * rate)
	pcm := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		t := float64(i) / rate
		g := 0.0
		if math.Sin(2*math.Pi*4*t) > 0 {
			g = 1
		}
		v := int16(g * 0.4 * math.Sin(2*math.Pi*180*t) * 32000)
		pcm[2*i], pcm[2*i+1] = byte(v), byte(uint16(v)>>8)
	}
	return wavFromPCM(rate, pcm)
}

type contentFleet struct {
	tts, asr *httptest.Server
	mu       sync.Mutex
	said     map[string]string // audio body → the text it was synthesized from (honest mode)
	honest   bool
	asrCalls int
}

func newContentFleet(t *testing.T, honest bool) *contentFleet {
	f := &contentFleet{said: map[string]string{}, honest: honest}
	f.tts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		wav := syllableNoise(req.Input)
		// Make each reply unique so the diversity gate is not what refuses it.
		wav = append(wav, []byte(req.Input)...)
		f.mu.Lock()
		f.said[string(wav)] = req.Input
		f.mu.Unlock()
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav)
	}))
	// The verifier "hears" the original text only for audio from an honest synthesizer;
	// for the impersonator it hears nothing intelligible.
	f.asr = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, audio, _ := parseTranscriptionForm(mustRead(r), r.Header.Get("Content-Type"))
		f.mu.Lock()
		f.asrCalls++
		text := ""
		if f.honest {
			text = f.said[string(audio)]
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"text": text})
	}))
	t.Cleanup(func() { f.tts.Close(); f.asr.Close() })
	return f
}

func mustRead(r *http.Request) []byte { b, _ := io.ReadAll(r.Body); return b }

func contentAuditor(t *testing.T, f *contentFleet, withASR bool, asrMiner string) (*auditor, worker.Worker) {
	reg := worker.NewRegistry(testLogger(), "")
	reg.Register(worker.WorkerRegistration{ID: "subject", Endpoint: f.tts.URL, SchedulerURL: f.tts.URL,
		GPUCount: 1, MinerAddress: "t01000"})
	reg.UpdateState("subject", "GPU_STATE_AVAILABLE", "running", 0, "/models/hexgrad--Kokoro-82M", 1)
	if withASR {
		reg.Register(worker.WorkerRegistration{ID: "verifier", Endpoint: f.asr.URL, SchedulerURL: f.asr.URL,
			GPUCount: 1, MinerAddress: asrMiner})
		reg.UpdateState("verifier", "GPU_STATE_AVAILABLE", "running", 0, "/models/Systran--faster-whisper-small", 1)
	}
	a := newAuditor(ProbeConfig{Enabled: true, NumQuestions: 8}, &Gateway{registry: reg, logger: testLogger()}, testLogger())
	return a, mustGet(t, reg, "subject")
}

func TestTTSContentCheckRefusesSyllableNoise(t *testing.T) {
	f := newContentFleet(t, false)
	a, w := contentAuditor(t, f, true, "t02000")
	correct, n, err := a.runTTSProbe(t.Context(), w, "/models/hexgrad--Kokoro-82M")
	if err != nil {
		t.Fatal(err)
	}
	if correct != 0 || n != 8 || f.asrCalls != 8 {
		t.Fatalf("noise that says nothing must score 0 once transcribed: %d/%d, %d transcriptions", correct, n, f.asrCalls)
	}
}

func TestTTSContentCheckPassesAudioThatSaysTheWords(t *testing.T) {
	f := newContentFleet(t, true)
	a, w := contentAuditor(t, f, true, "t02000")
	correct, n, _ := a.runTTSProbe(t.Context(), w, "/models/hexgrad--Kokoro-82M")
	if correct != n {
		t.Fatalf("audio the verifier hears as the requested words must pass: %d/%d", correct, n)
	}
}

// Without any admitted ASR in the fleet the probe falls back to the shape checks —
// which this impersonator passes. That is the documented limit, pinned here so a change
// in it is deliberate.
func TestTTSWithoutVerifierFallsBackToShapeChecks(t *testing.T) {
	f := newContentFleet(t, false)
	a, w := contentAuditor(t, f, false, "")
	correct, n, _ := a.runTTSProbe(t.Context(), w, "/models/hexgrad--Kokoro-82M")
	if correct != n || f.asrCalls != 0 {
		t.Fatalf("no verifier: shape checks only, got %d/%d with %d transcriptions", correct, n, f.asrCalls)
	}
}

func TestASRVerifierPrefersAnotherMiner(t *testing.T) {
	reg := worker.NewRegistry(testLogger(), "")
	for _, w := range []struct{ id, miner, model string }{
		{"subject", "t01000", "/models/Systran--faster-whisper-small"},
		{"same-miner", "t01000", "/models/Systran--faster-whisper-small"},
		{"other-miner", "t03000", "/models/Systran--faster-whisper-small"},
		{"text-only", "t04000", "/models/Qwen--Qwen3-32B-AWQ"},
	} {
		reg.Register(worker.WorkerRegistration{ID: w.id, Endpoint: "http://x", SchedulerURL: "http://x", GPUCount: 1, MinerAddress: w.miner})
		reg.UpdateState(w.id, "GPU_STATE_AVAILABLE", "running", 0, w.model, 1)
	}
	a := newAuditor(ProbeConfig{Enabled: true}, &Gateway{registry: reg, logger: testLogger()}, testLogger())
	subject := mustGet(t, reg, "subject")
	if v, _, self, ok := a.asrVerifier(subject); !ok || v.ID != "other-miner" || self {
		t.Fatalf("verifier = %s self=%v ok=%v, want other-miner", v.ID, self, ok)
	}
	reg.Deregister("other-miner")
	if v, _, self, _ := a.asrVerifier(subject); v.ID != "same-miner" || self {
		t.Fatalf("without another miner: verifier = %s self=%v, want same-miner", v.ID, self)
	}
	reg.Deregister("same-miner")
	if v, _, self, _ := a.asrVerifier(subject); v.ID != "subject" || !self {
		t.Fatalf("last resort: verifier = %s self=%v, want subject itself", v.ID, self)
	}
}

func TestWordRecall(t *testing.T) {
	item := ttsProbeItem{Text: "The river garden window silver."}
	for heard, want := range map[string]float64{
		"The river, garden window silver.": 1,
		"river garden":                     0.5,
		"":                                 0,
		"THE RIVER GARDEN WINDOW":          0.75,
	} {
		if got := wordRecall(item, heard); math.Abs(got-want) > 1e-9 {
			t.Errorf("wordRecall(%q) = %v, want %v", heard, got, want)
		}
	}
}
