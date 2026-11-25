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

func TestStreaming502RetrySwitchesWorker(t *testing.T) {
	// A worker front answering 502 before any byte (its engine died) is retried too.
	logger := discardGwLogger()
	registry := worker.NewRegistry(logger, "")

	backend503 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
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
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
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

// A worker refusing a streaming request (4xx, e.g. image input to a text model)
// must reach the client as the same JSON error the non-streaming path returns —
// {"error":{"message":…}} — with no SSE [DONE] appended. Raw, it arrived as the
// worker's {"detail":…} followed by "data: [DONE]", which no client could parse
// (the web page showed "400: {}").
func TestStreaming4xxIsAPlainJSONError(t *testing.T) {
	for _, resume := range []bool{false, true} {
		logger := discardGwLogger()
		registry := worker.NewRegistry(logger, "")
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"detail":"model 'm' does not accept image input"}`)
		}))
		registry.Register(worker.WorkerRegistration{ID: "w1", Endpoint: backend.URL, SchedulerURL: "http://x:1", GPUCount: 1})
		registry.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "test-model", 1)
		gw := New(registry, config.GatewayConfig{RequestTimeoutSec: 10, StreamResume: resume}, logger)
		srv := httptest.NewServer(gw.Handler())

		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		srv.Close()
		backend.Close()
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if resp.StatusCode != 400 || json.Unmarshal(raw, &e) != nil || !strings.Contains(e.Error.Message, "does not accept image input") {
			t.Fatalf("resume=%v: want 400 {\"error\":{\"message\":…}}, got %d %q", resume, resp.StatusCode, raw)
		}
	}
}

// A retry that lands on a worker which only SUPPORTS the model makes that worker switch
// models — it must be marked switching exactly as a first attempt would be, or the next
// requests for its pool are routed onto the reloading engine. Covered for the three
// chat retry paths: non-streaming, streaming before any byte, and stream resume.
func TestChatRetryOntoASwitchingWorkerMarksIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
		resume bool
	}{{"non-streaming", false, false}, {"streaming", true, false}, {"stream resume", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			logger := discardGwLogger()
			registry := worker.NewRegistry(logger, "")
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !tc.resume {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"part one\"}}]}\n\n")
				io.WriteString(w, "data: {\"error\":{\"message\":\"Engine paused during generation — request aborted\",\"type\":\"server_error\"}}\n\n")
				io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer first.Close()
			var duringTo string
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if w2, ok := registry.Get("w2"); ok { // observed during the switch
					duringTo = w2.SwitchingTo
				}
				if !tc.stream {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\" two\"}}]}\n\n")
				io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n")
				io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer second.Close()
			registry.Register(worker.WorkerRegistration{ID: "w1", Endpoint: first.URL, SchedulerURL: "http://x:1", GPUCount: 1})
			registry.Register(worker.WorkerRegistration{ID: "w2", Endpoint: second.URL, SchedulerURL: "http://x:2", GPUCount: 1,
				SupportedModels: []string{"test-model", "other-model"}})
			registry.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "test-model", 1)
			registry.UpdateState("w2", "GPU_STATE_AVAILABLE", "running", 0, "other-model", 1)
			registry.SetFeatures("w1", []string{worker.FeatureContinuation}, "")
			registry.SetFeatures("w2", []string{worker.FeatureContinuation}, "")

			gw := New(registry, config.GatewayConfig{RequestTimeoutSec: 10, StreamResume: true}, logger)
			srv := httptest.NewServer(gw.Handler())
			defer srv.Close()
			body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"max_tokens":30`
			if tc.stream {
				body += `,"stream":true`
			}
			resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body+"}"))
			if err != nil {
				t.Fatal(err)
			}
			out, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode, out)
			}
			if tc.resume && !strings.Contains(string(out), "two") {
				t.Fatalf("the stream was not resumed on w2: %s", out)
			}
			if duringTo == "" {
				t.Fatalf("w2 served test-model by switching to it but was not marked switching (body=%s)", out)
			}
			if w2, _ := registry.Get("w2"); w2.SwitchingTo != "" {
				t.Fatalf("the mark must be cleared once the request completed, got %q", w2.SwitchingTo)
			}
		})
	}
}

// The first attempt of a chat request, and a retry that had to wait in the queue for a
// worker to come back, mark a switching worker just like an immediate retry does.
func TestChatSwitchMarkOnFirstAttemptAndAfterQueueWait(t *testing.T) {
	var duringTo string
	var reg *worker.Registry
	ok := func(stream bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if w2, found := reg.Get("w2"); found { // observed during the switch
				duringTo = w2.SwitchingTo
			}
			if !stream {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
			io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
		}
	}
	for _, tc := range []struct {
		name         string
		stream, wait bool
	}{{"first attempt", false, false}, {"non-streaming after queue wait", false, true}, {"streaming after queue wait", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			logger := discardGwLogger()
			registry := worker.NewRegistry(logger, "")
			reg, duringTo = registry, ""
			busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer busy.Close()
			second := httptest.NewServer(ok(tc.stream))
			defer second.Close()
			registry.Register(worker.WorkerRegistration{ID: "w2", Endpoint: second.URL, SchedulerURL: "http://x:2", GPUCount: 1,
				SupportedModels: []string{"test-model", "other-model"}})
			if tc.wait {
				// w1 holds the model and yields; w2 is mining when w1 fails and comes back
				// shortly after, so the retry reaches it only through the queue.
				registry.Register(worker.WorkerRegistration{ID: "w1", Endpoint: busy.URL, SchedulerURL: "http://x:1", GPUCount: 1})
				registry.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "test-model", 1)
				registry.UpdateState("w2", "GPU_STATE_WINNING_POST", "paused", 0, "other-model", 1)
				go func() {
					time.Sleep(300 * time.Millisecond)
					registry.UpdateState("w2", "GPU_STATE_AVAILABLE", "running", 0, "other-model", 1)
				}()
			} else {
				registry.UpdateState("w2", "GPU_STATE_AVAILABLE", "running", 0, "other-model", 1)
			}
			gw := New(registry, config.GatewayConfig{RequestTimeoutSec: 10, QueueTimeoutSec: 5}, logger)
			srv := httptest.NewServer(gw.Handler())
			defer srv.Close()
			body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]`
			if tc.stream {
				body += `,"stream":true`
			}
			resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body+"}"))
			if err != nil {
				t.Fatal(err)
			}
			out, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode, out)
			}
			if duringTo == "" {
				t.Fatalf("w2 served test-model by switching to it but was not marked switching (body=%s)", out)
			}
		})
	}
}
