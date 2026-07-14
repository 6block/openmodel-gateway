package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// readSamples parses every JSON line in the sample log at path.
func readSamples(t *testing.T, path string) []VerificationSample {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open sample log: %v", err)
	}
	defer f.Close()
	var out []VerificationSample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var s VerificationSample
		if err := json.Unmarshal(line, &s); err != nil {
			t.Fatalf("bad sample line %q: %v", line, err)
		}
		out = append(out, s)
	}
	return out
}

// A rate of 0 (or empty path) must yield a nil sampler so the request path pays nothing.
func TestVerificationSampler_DisabledReturnsNil(t *testing.T) {
	if s := NewVerificationSampler("", 1.0, 0, 0, testLogger()); s != nil {
		t.Error("empty path must disable sampling (nil sampler)")
	}
	dir := t.TempDir()
	if s := NewVerificationSampler(filepath.Join(dir, "s.jsonl"), 0, 0, 0, testLogger()); s != nil {
		t.Error("rate 0 must disable sampling (nil sampler)")
	}
}

// A nil sampler's methods are safe no-ops (the disabled-feature hot path).
func TestVerificationSampler_NilSafe(t *testing.T) {
	var s *verificationSampler
	if s.shouldSample() {
		t.Error("nil sampler must not sample")
	}
	s.write(VerificationSample{RequestID: "x"}) // must not panic
	s.close()                                   // must not panic
}

// rate=1 samples everything; each write lands as one parseable JSON line with the fields.
func TestVerificationSampler_WriteRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	s := NewVerificationSampler(path, 1.0, 0, 0, testLogger())
	if s == nil {
		t.Fatal("expected a sampler")
	}
	if !s.shouldSample() {
		t.Error("rate 1.0 must always sample")
	}
	s.write(VerificationSample{
		RequestID: "req-1", WorkerID: "w1", ModelReq: "big-model", ModelResp: "small-model",
		Stream: false, Status: 200, PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		Request: `{"model":"big-model"}`, Response: `{"model":"small-model"}`,
	})
	s.close()

	got := readSamples(t, path)
	if len(got) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(got))
	}
	if got[0].ModelReq != "big-model" || got[0].ModelResp != "small-model" {
		t.Errorf("model mismatch recorded wrong: req=%q resp=%q", got[0].ModelReq, got[0].ModelResp)
	}
	if got[0].TotalTokens != 15 {
		t.Errorf("tokens: got %d want 15", got[0].TotalTokens)
	}
}

// Oversized request/response bodies are truncated to the cap and flagged.
func TestVerificationSampler_TruncatesLargeBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	s := NewVerificationSampler(path, 1.0, 0, 0, testLogger())
	big := strings.Repeat("A", sampleMaxBodyBytes+5000)
	s.write(VerificationSample{RequestID: "r", Request: big, Response: big})
	s.close()

	got := readSamples(t, path)
	if len(got) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(got))
	}
	if len(got[0].Request) != sampleMaxBodyBytes || len(got[0].Response) != sampleMaxBodyBytes {
		t.Errorf("bodies not truncated to cap: req=%d resp=%d cap=%d",
			len(got[0].Request), len(got[0].Response), sampleMaxBodyBytes)
	}
	if !got[0].Truncated {
		t.Error("truncated flag not set")
	}
}

// The sample log rotates once it exceeds the size threshold, preserving a .1 backup.
func TestVerificationSampler_Rotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	s := NewVerificationSampler(path, 1.0, 0, 2, testLogger())
	// Force a tiny threshold so a couple of writes trigger rotation.
	s.maxSize = 200
	line := strings.Repeat("z", 150)
	for i := 0; i < 5; i++ {
		s.write(VerificationSample{RequestID: "r", Request: line})
	}
	s.close()
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("expected rotated backup %s.1: %v", path, err)
	}
}

// scanModelField pulls the claimed model out of both a JSON object and a raw SSE stream.
func TestScanModelField(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"model":"qwen2.5-3b","usage":{}}`, "qwen2.5-3b"},
		{`{"model": "spaced-model"}`, "spaced-model"},
		{"data: {\"model\":\"stream-model\",\"choices\":[]}\n\ndata: {\"model\":\"stream-model\"}\n\n", "stream-model"},
		{`{"nomodel":true}`, ""},
		{``, ""},
	}
	for _, c := range cases {
		if got := scanModelField([]byte(c.in)); got != c.want {
			t.Errorf("scanModelField(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
