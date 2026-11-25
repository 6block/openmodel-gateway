package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/worker"
)

// streamBackend answers a streaming chat request with a short SSE stream.
func streamBackend(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"hello"}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(s.Close)
	return s
}

func keepaliveGateway(t *testing.T, backendURL string, queue time.Duration) (*Gateway, *worker.Registry, *httptest.Server) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := worker.NewRegistry(logger, "")
	reg.Register(worker.WorkerRegistration{ID: "w1", Endpoint: backendURL, SchedulerURL: "http://unused:9090", GPUCount: 1})
	gw := New(reg, config.GatewayConfig{RequestTimeoutSec: 10}, logger)
	gw.queueTimeout = queue
	gw.keepaliveAfter = 200 * time.Millisecond
	gw.keepaliveEvery = 200 * time.Millisecond
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return gw, reg, srv
}

const streamReq = `{"model":"model","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// TestKeepalive_LongWaitSendsCommentsThenTheStream: a streaming request that waits
// (here: the only worker is mining) gets SSE comment lines while it waits, so no
// proxy between client and gateway sees an idle connection, and then the real stream.
func TestKeepalive_LongWaitSendsCommentsThenTheStream(t *testing.T) {
	_, reg, srv := keepaliveGateway(t, streamBackend(t).URL, 5*time.Second)
	reg.UpdateState("w1", "GPU_STATE_WINDOW_POST", "paused", 0, "model", 1)
	go func() {
		time.Sleep(1200 * time.Millisecond)
		reg.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "model", 1)
	}()
	t0 := time.Now()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(streamReq))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d ct %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if time.Since(t0) > time.Second {
		t.Errorf("headers must arrive with the first keepalive, took %s", time.Since(t0))
	}
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	if n := strings.Count(body, ": waiting for a worker"); n < 2 {
		t.Errorf("want ≥2 keepalive comments during the 1.2 s wait, got %d:\n%s", n, body)
	}
	if !strings.Contains(body, `"content":"hello"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("the worker's stream must follow the comments intact:\n%s", body)
	}
	if strings.Index(body, ": waiting") > strings.Index(body, `"content"`) {
		t.Error("no keepalive may be written once the worker stream has started")
	}
}

// TestKeepalive_FastRequestIsUntouched: a stream that starts before the first keepalive
// is byte-for-byte what the worker sent (no comment, headers from the worker).
func TestKeepalive_FastRequestIsUntouched(t *testing.T) {
	_, reg, srv := keepaliveGateway(t, streamBackend(t).URL, 5*time.Second)
	reg.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "model", 1)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(streamReq))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(raw), ": waiting") {
		t.Errorf("fast stream must carry no keepalive:\n%s", raw)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("gateway headers must still be present")
	}
}

// TestKeepalive_ErrorAfterCommitBecomesAnSSEErrorEvent: when the wait ends in a 503
// (queue window over), the status line is already a 200, so the error arrives as an
// SSE error event carrying the status — never as a dropped connection.
func TestKeepalive_ErrorAfterCommitBecomesAnSSEErrorEvent(t *testing.T) {
	_, reg, srv := keepaliveGateway(t, streamBackend(t).URL, 900*time.Millisecond)
	reg.UpdateState("w1", "GPU_STATE_WINDOW_POST", "paused", 0, "model", 1)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(streamReq))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("committed stream must be 200, got %d", resp.StatusCode)
	}
	var ev map[string]map[string]any
	sc := bufio.NewScanner(resp.Body)
	sawDone := false
	for sc.Scan() {
		line := sc.Text()
		if line == "data: [DONE]" {
			sawDone = true
		} else if strings.HasPrefix(line, "data: ") {
			if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
				t.Fatalf("bad event %q: %v", line, err)
			}
		}
	}
	if ev["error"] == nil || ev["error"]["status"] != float64(503) || ev["error"]["type"] != "service_unavailable" {
		t.Fatalf("want an error event with status 503, got %v", ev)
	}
	if msg, _ := ev["error"]["message"].(string); !strings.Contains(msg, "mining") {
		t.Errorf("the gateway's message must be kept, got %q", msg)
	}
	if !sawDone {
		t.Error("the stream must end with [DONE]")
	}
}

// TestKeepalive_QuickRefusalKeepsItsStatus: an error before the first keepalive is an
// ordinary HTTP error, as before.
func TestKeepalive_QuickRefusalKeepsItsStatus(t *testing.T) {
	_, reg, srv := keepaliveGateway(t, streamBackend(t).URL, 50*time.Millisecond)
	reg.UpdateState("w1", "GPU_STATE_WINDOW_POST", "paused", 0, "model", 1)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(streamReq))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Errorf("want a plain 503 with Retry-After, got %d %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

// TestKeepalive_NonStreamingIsNotWrapped: non-streaming requests keep their exact
// status and JSON body semantics.
func TestKeepalive_NonStreamingIsNotWrapped(t *testing.T) {
	_, reg, srv := keepaliveGateway(t, streamBackend(t).URL, 900*time.Millisecond)
	reg.UpdateState("w1", "GPU_STATE_WINDOW_POST", "paused", 0, "model", 1)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("non-streaming wait must end in a plain 503, got %d", resp.StatusCode)
	}
}
