package gateway

import (
	"net/http/httptest"
	"testing"
)

// The gateway meters delivered completion tokens itself (non-empty content deltas),
// so a client that disconnects before the final usage chunk is still billed for what
// was delivered.
func TestSSEDeliveredCompletionMetering(t *testing.T) {
	w := &sseCaptureWriter{ResponseWriter: httptest.NewRecorder()}
	chunks := []string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n", // role only, no content → 0
		`data: {"choices":[{"delta":{"content":"Hello"}}]}` + "\n\n",  // +1
		`data: {"choices":[{"delta":{"content":" world"}}]}` + "\n\n", // +1
		`data: {"choices":[{"delta":{"content":""}}]}` + "\n\n",       // empty content → 0
		`data: {"choices":[{"delta":{"content":"!"}}]}` + "\n\n",      // +1
		`data: [DONE]` + "\n\n", // sentinel → 0
	}
	for _, c := range chunks {
		if _, err := w.Write([]byte(c)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if w.deliveredCompletion != 3 {
		t.Fatalf("expected 3 delivered content deltas, got %d", w.deliveredCompletion)
	}
}

// The final usage chunk (when it arrives) is still captured and preferred; the usage
// chunk itself has no content delta so it doesn't inflate the delivered count.
func TestSSEUsageAndErrorCapture(t *testing.T) {
	w := &sseCaptureWriter{ResponseWriter: httptest.NewRecorder()}
	w.Write([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n"))
	w.Write([]byte(`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}` + "\n\n"))
	if w.usage.TotalTokens != 6 {
		t.Fatalf("usage not captured, got %+v", w.usage)
	}
	if w.deliveredCompletion != 1 {
		t.Fatalf("usage chunk must not inflate delivered count, got %d", w.deliveredCompletion)
	}

	// A worker error event (mining interruption) is captured → caller marks the record
	// ErrorReason and does NOT bill it.
	w2 := &sseCaptureWriter{ResponseWriter: httptest.NewRecorder()}
	w2.Write([]byte(`data: {"error":{"message":"Engine paused during generation"}}` + "\n\n"))
	if w2.streamError == "" {
		t.Fatal("streamError should be captured from a worker error event")
	}
}

// Production py-inference emits SPACED JSON (`"content": "…"`, json.dumps default) —
// a compact-only marker read 0 delivered tokens on real workers (found by real-machine
// B2 verification). Both spacings must meter identically, in legacy passthrough mode.
func TestMetering_SpacedJSONFormatCounts(t *testing.T) {
	w := &sseCaptureWriter{ResponseWriter: httptest.NewRecorder()}
	w.Write([]byte("data: {\"choices\": [{\"delta\": {\"content\": \"He\"}}]}\n\n"))
	w.Write([]byte("data: {\"choices\": [{\"delta\": {\"content\": \"llo\"}}]}\n\n"))
	w.Write([]byte("data: {\"choices\": [{\"delta\": {\"content\": \"\"}}]}\n\n")) // empty → not counted
	if w.deliveredCompletion != 2 {
		t.Errorf("spaced-format deltas must be metered: want 2, got %d", w.deliveredCompletion)
	}
	// Compact format unchanged.
	w2 := &sseCaptureWriter{ResponseWriter: httptest.NewRecorder()}
	w2.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"))
	if w2.deliveredCompletion != 1 {
		t.Errorf("compact-format deltas must still meter: want 1, got %d", w2.deliveredCompletion)
	}
}
