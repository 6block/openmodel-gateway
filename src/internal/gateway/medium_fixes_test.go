package gateway

import (
	"context"
	"encoding/json"
	"errors"
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

func discardGwLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestStreaming503RetrySwitchesWorker covers the audit MEDIUM fix: when a worker
// answers a streaming request with 503 (yielded to mining) BEFORE any bytes are sent,
// the gateway retries on another worker instead of forwarding the 503 to the client.
//
// Determinism: w1 has the model LOADED (routing Priority 1, always chosen first) and
// always returns 503; w2 only SUPPORTS the model (Priority 2) and streams a real
// answer. So w1 is tried first, fails, and the retry lands on w2.
func TestStreaming503RetrySwitchesWorker(t *testing.T) {
	logger := discardGwLogger()
	registry := worker.NewRegistry(logger, "")

	backend503 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer backend503.Close()

	backendOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		w.WriteHeader(200)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"from-w2\"}}]}\n\n")
		fl.Flush()
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n")
		fl.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer backendOK.Close()

	registry.Register(worker.WorkerRegistration{ID: "w1", Endpoint: backend503.URL, SchedulerURL: "http://x:1", GPUCount: 1})
	registry.Register(worker.WorkerRegistration{ID: "w2", Endpoint: backendOK.URL, SchedulerURL: "http://x:2", GPUCount: 1, SupportedModels: []string{"test-model"}})
	// w1 has test-model loaded (Priority 1); w2 has a different model loaded but supports test-model (Priority 2).
	registry.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "test-model", 1)
	registry.UpdateState("w2", "GPU_STATE_AVAILABLE", "running", 0, "other-model", 1)

	gw := New(registry, config.GatewayConfig{RequestTimeoutSec: 10}, logger)
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	body := string(bodyBytes)

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 after streaming retry, got %d (body=%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "from-w2") {
		t.Errorf("expected the stream to be served by w2 after w1's 503, body=%s", body)
	}
}

// TestQueueCapIsAtomic covers the audit MEDIUM fix: max_queue_size is enforced with an
// atomic Add-then-check, so once the queue is at capacity an additional caller is
// rejected immediately rather than slipping past a check-then-Add race.
func TestQueueCapIsAtomic(t *testing.T) {
	gw, registry, _ := newTestGateway(t)
	gw.maxQueueSize = 2
	gw.queueTimeout = 10 * time.Second
	// Worker mining → no one is ever released, so the queue stays full.
	registry.UpdateState("w1", "GPU_STATE_WINDOW_POST", "paused", 0, "model", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // releases the two blockers at test end

	for i := 0; i < 2; i++ {
		go gw.waitForWorkerInternal(ctx, "default")
	}

	// Wait until both blockers have occupied their queue slots.
	deadline := time.Now().Add(2 * time.Second)
	for gw.queuedCount.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("blockers never occupied the queue")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A third caller must be refused immediately, and the count must not overshoot.
	_, err := gw.waitForWorkerInternal(ctx, "default")
	if !errors.Is(err, ErrQueueFull) {
		t.Errorf("expected ErrQueueFull at capacity, got %v", err)
	}
	if c := gw.queuedCount.Load(); c != 2 {
		t.Errorf("queuedCount must stay at the cap of 2, got %d", c)
	}
}

// TestForwardRequestRespectsContext covers the audit fix: forwardRequest binds the
// upstream call to the request context, so a cancelled context aborts it promptly
// instead of blocking for the full request timeout.
func TestForwardRequestRespectsContext(t *testing.T) {
	logger := discardGwLogger()
	registry := worker.NewRegistry(logger, "")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
			w.WriteHeader(200)
		}
	}))
	defer backend.Close()

	gw := New(registry, config.GatewayConfig{RequestTimeoutSec: 30}, logger)
	target := &worker.Worker{ID: "w1", Endpoint: backend.URL}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	t0 := time.Now()
	_, _, _, _, err := gw.forwardRequest(ctx, "/v1/chat/completions", []byte(`{}`), target, "req-x")
	if err == nil {
		t.Error("expected a context-cancellation error from forwardRequest")
	}
	if elapsed := time.Since(t0); elapsed > 1*time.Second {
		t.Errorf("forwardRequest ignored ctx cancel; took %v (expected < 1s)", elapsed)
	}
}

// TestResponseHeaderPassthrough covers the audit fix: the non-streaming path forwards
// the worker's response headers (minus hop-by-hop ones) to the client.
func TestResponseHeaderPassthrough(t *testing.T) {
	logger := discardGwLogger()
	registry := worker.NewRegistry(logger, "")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom-Header", "custom-value")
		w.Header().Set("Keep-Alive", "timeout=5") // hop-by-hop → must NOT be forwarded
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{{"message": map[string]string{"role": "assistant", "content": "ok"}}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer backend.Close()

	registry.Register(worker.WorkerRegistration{ID: "w1", Endpoint: backend.URL, SchedulerURL: "http://x:1", GPUCount: 1})
	registry.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "model", 1)

	gw := New(registry, config.GatewayConfig{RequestTimeoutSec: 10}, logger)
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"default","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Custom-Header"); got != "custom-value" {
		t.Errorf("expected upstream X-Custom-Header to pass through, got %q", got)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("expected gateway to set X-Request-ID")
	}
	if got := resp.Header.Get("Keep-Alive"); got != "" {
		t.Errorf("hop-by-hop Keep-Alive must not be forwarded, got %q", got)
	}
}

// TestComputeWeightUsesInflight covers the audit fix: the routing weight reacts to the
// gateway's real-time in-flight count, not only the polled ActiveRequests.
func TestComputeWeightUsesInflight(t *testing.T) {
	idle := worker.Worker{GPUCount: 4}
	if w := computeWeight(&idle); w != 4 {
		t.Errorf("idle worker weight expected 4, got %v", w)
	}
	loadedByInflight := worker.Worker{GPUCount: 4, InFlight: 8}
	if computeWeight(&loadedByInflight) >= computeWeight(&idle) {
		t.Error("a high in-flight count must lower the weight below idle")
	}

	if got := effectiveLoad(&worker.Worker{ActiveRequests: 2, InFlight: 5}); got != 5 {
		t.Errorf("effectiveLoad should take in-flight when larger: expected 5, got %d", got)
	}
	if got := effectiveLoad(&worker.Worker{ActiveRequests: 7, InFlight: 1}); got != 7 {
		t.Errorf("effectiveLoad should take polled active when larger: expected 7, got %d", got)
	}
}

// TestCopyResponseHeadersFiltersHopByHop is a focused unit test for the header filter.
func TestCopyResponseHeadersFiltersHopByHop(t *testing.T) {
	src := http.Header{}
	src.Set("X-Keep", "yes")
	src.Set("Connection", "keep-alive")
	src.Set("Transfer-Encoding", "chunked")
	src.Set("Content-Length", "123")

	dst := http.Header{}
	copyResponseHeaders(dst, src)

	if dst.Get("X-Keep") != "yes" {
		t.Error("non-hop-by-hop header X-Keep should be copied")
	}
	for _, h := range []string{"Connection", "Transfer-Encoding", "Content-Length"} {
		if dst.Get(h) != "" {
			t.Errorf("hop-by-hop header %s must not be copied", h)
		}
	}
}

// TestSharedClientsInitialized asserts the shared HTTP client and stream transport are
// built once in New (audit fix: previously each request built its own transport).
func TestSharedClientsInitialized(t *testing.T) {
	gw := New(worker.NewRegistry(discardGwLogger(), ""), config.GatewayConfig{RequestTimeoutSec: 10}, discardGwLogger())
	if gw.httpClient == nil || gw.httpClient.Transport == nil {
		t.Error("shared httpClient and its transport must be initialized")
	}
	if gw.streamTransport == nil {
		t.Error("shared streamTransport must be initialized")
	}
}
