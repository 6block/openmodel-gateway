package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// C5 — streaming SSE parsing boundary tests. The sseCaptureWriter is the heart of
// streaming billing correctness: it must extract usage from the final SSE chunk,
// detect mid-stream error events (so an interrupted stream is NOT billed), and track
// whether any bytes reached the client (which decides retry-ability). These run
// without a socket via httptest.NewRecorder.

// feedSSE writes each chunk through the capture writer (as a worker's stream would)
// and returns the writer for assertions.
func feedSSE(chunks ...string) *sseCaptureWriter {
	rec := httptest.NewRecorder()
	w := &sseCaptureWriter{ResponseWriter: rec}
	for _, c := range chunks {
		_, _ = w.Write([]byte(c))
	}
	return w
}

// TestSSEUsageExtractedFromFinalChunk verifies usage is captured from the terminal
// usage-bearing SSE event (OpenAI streams usage in the last data frame).
func TestSSEUsageExtractedFromFinalChunk(t *testing.T) {
	w := feedSSE(
		`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20}}` + "\n\n",
		"data: [DONE]\n\n",
	)
	if w.usage.TotalTokens != 20 || w.usage.PromptTokens != 12 || w.usage.CompletionTokens != 8 {
		t.Errorf("usage not parsed from final chunk: %+v", w.usage)
	}
	if w.streamError != "" {
		t.Errorf("clean stream should have no error, got %q", w.streamError)
	}
}

// TestSSECachedTokensParsed verifies prefix-cache hit tokens are parsed (cache-read
// billing depends on this).
func TestSSECachedTokensParsed(t *testing.T) {
	w := feedSSE(
		`data: {"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"prompt_tokens_details":{"cached_tokens":80}}}` + "\n\n",
	)
	if w.usage.CachedTokens != 80 {
		t.Errorf("cached_tokens: want 80, got %d", w.usage.CachedTokens)
	}
}

// TestSSEErrorEventDetected verifies a mid-stream error event (e.g. engine paused for
// mining) is captured into streamError, which the handler uses to mark the request
// stream_interrupted and NOT bill it.
func TestSSEErrorEventDetected(t *testing.T) {
	w := feedSSE(
		`data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n",
		`data: {"error":{"message":"Engine paused during generation","type":"server_error"}}` + "\n\n",
	)
	if w.streamError == "" {
		t.Fatal("mid-stream error event should be detected")
	}
	if !strings.Contains(w.streamError, "Engine paused") {
		t.Errorf("streamError should carry the message, got %q", w.streamError)
	}
}

// TestSSEWroteHeaderTracksDelivery verifies wroteHeader flips once any byte is
// written — the signal the handler uses to decide a stream can no longer be retried
// on another worker (bytes already left for the client).
func TestSSEWroteHeaderTracksDelivery(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &sseCaptureWriter{ResponseWriter: rec}
	if w.wroteHeader {
		t.Fatal("wroteHeader should be false before any write")
	}
	_, _ = w.Write([]byte("data: {}\n\n"))
	if !w.wroteHeader {
		t.Fatal("wroteHeader should be true after the first write (retry no longer safe)")
	}
}

// TestSSEMalformedJSONIgnored verifies malformed/garbage SSE data does not panic and
// does not fabricate usage — robustness against a misbehaving worker stream.
func TestSSEMalformedJSONIgnored(t *testing.T) {
	w := feedSSE(
		"data: {not json at all\n\n",
		": this is a comment line\n\n",
		"\n\n",
		`data: {"usage":{"total_tokens":}}` + "\n\n", // broken JSON with usage marker
	)
	if w.usage.TotalTokens != 0 {
		t.Errorf("malformed usage must not be counted, got %d", w.usage.TotalTokens)
	}
	if w.streamError != "" {
		t.Errorf("malformed data must not fabricate an error, got %q", w.streamError)
	}
}

// TestSSEMultiLineChunkScanned verifies a single Write carrying several data lines
// (workers may batch frames) is fully scanned for usage.
func TestSSEMultiLineChunkScanned(t *testing.T) {
	w := feedSSE(
		`data: {"choices":[{"delta":{"content":"a"}}]}` + "\n" +
			`data: {"choices":[{"delta":{"content":"b"}}]}` + "\n" +
			`data: {"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n",
	)
	if w.usage.TotalTokens != 5 {
		t.Errorf("usage in a multi-line chunk should be parsed, got %d", w.usage.TotalTokens)
	}
}
