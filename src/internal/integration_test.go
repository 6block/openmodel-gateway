package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/gateway"
	"openmodel/sp-state-agent/internal/worker"
)

// === Mock SP Worker (simulates go-scheduler + py-inference on one machine) ===

type mockSPWorker struct {
	gpuState       atomic.Value // string
	engineState    atomic.Value // string
	activeRequests atomic.Int32
	loadedModel    atomic.Value // string
	scheduler      *httptest.Server
	inference      *httptest.Server
}

func newMockSPWorker() *mockSPWorker {
	w := &mockSPWorker{}
	w.gpuState.Store("GPU_STATE_AVAILABLE")
	w.engineState.Store("running")
	w.loadedModel.Store("Qwen/Qwen2.5-3B-Instruct")

	// Mock go-scheduler /ready
	w.scheduler = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ready" {
			fmt.Fprintf(rw, "ready, gpu_state=%s\n", w.gpuState.Load().(string))
		}
	}))

	// Mock py-inference /health and /v1/chat/completions
	w.inference = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			rw.Header().Set("Content-Type", "application/json")
			json.NewEncoder(rw).Encode(map[string]interface{}{
				"status":          "ok",
				"engine_state":    w.engineState.Load().(string),
				"active_requests": w.activeRequests.Load(),
				"loaded_model":    w.loadedModel.Load().(string),
			})
		case "/v1/chat/completions":
			rw.Header().Set("Content-Type", "application/json")
			json.NewEncoder(rw).Encode(map[string]interface{}{
				"id":      "cmpl-test",
				"object":  "chat.completion",
				"model":   "default",
				"choices": []map[string]interface{}{{"index": 0, "message": map[string]string{"role": "assistant", "content": "Hello from " + w.inference.URL}, "finish_reason": "stop"}},
				"usage":   map[string]int{"prompt_tokens": 5, "completion_tokens": 10, "total_tokens": 15},
			})
		}
	}))

	return w
}

func (w *mockSPWorker) close() {
	w.scheduler.Close()
	w.inference.Close()
}

func (w *mockSPWorker) startMining() {
	w.gpuState.Store("GPU_STATE_WINDOW_POST")
	w.engineState.Store("paused")
	w.activeRequests.Store(0)
}

func (w *mockSPWorker) stopMining() {
	w.gpuState.Store("GPU_STATE_AVAILABLE")
	w.engineState.Store("running")
}

// === Integration Test ===

func TestIntegration_GatewayFailover(t *testing.T) {
	sp1 := newMockSPWorker()
	defer sp1.close()

	sp2 := newMockSPWorker()
	defer sp2.close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Registry + register two workers
	registry := worker.NewRegistry(logger, "")

	registry.Register(worker.WorkerRegistration{
		ID: "sp-worker-1", Endpoint: sp1.inference.URL,
		SchedulerURL: sp1.scheduler.URL, GPUCount: 4,
	})
	registry.Register(worker.WorkerRegistration{
		ID: "sp-worker-2", Endpoint: sp2.inference.URL,
		SchedulerURL: sp2.scheduler.URL, GPUCount: 8,
	})

	// Poller
	poller := worker.NewPoller(registry, 100*time.Millisecond, 1, logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poller.Run(ctx)

	// Wait for initial poll
	time.Sleep(300 * time.Millisecond)

	// Both should be idle
	w1, _ := registry.Get("sp-worker-1")
	w2, _ := registry.Get("sp-worker-2")
	if w1.State != worker.StateIdle || w2.State != worker.StateIdle {
		t.Fatalf("expected both idle, got %s and %s", w1.State, w2.State)
	}

	// Create gateway
	gw := gateway.New(registry, config.GatewayConfig{RequestTimeoutSec: 10}, logger)
	gwServer := httptest.NewServer(gw.Handler())
	defer gwServer.Close()

	// Test 1: Normal request should succeed
	resp := postChat(t, gwServer.URL, "Hello")
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Test 2: SP Worker 1 starts mining
	t.Log("--- SP Worker 1 starts mining ---")
	sp1.startMining()
	time.Sleep(300 * time.Millisecond)

	w1, _ = registry.Get("sp-worker-1")
	if w1.State != worker.StateMining {
		t.Errorf("worker 1 should be mining, got %s", w1.State)
	}

	// Request should still succeed — routed to worker 2
	resp = postChat(t, gwServer.URL, "Hello during mining")
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 (failover to worker 2), got %d", resp.StatusCode)
	}
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()

	choices := body["choices"].([]interface{})
	msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})["content"].(string)
	if msg == "" {
		t.Error("expected non-empty response from failover worker")
	}
	t.Logf("failover response: %s", msg)

	// Test 3: Both workers mining — should get 503
	t.Log("--- Both workers mining ---")
	sp2.startMining()
	time.Sleep(300 * time.Millisecond)

	resp = postChat(t, gwServer.URL, "All mining")
	if resp.StatusCode != 503 {
		t.Errorf("expected 503 when all mining, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Test 4: Recovery
	t.Log("--- Workers recover ---")
	sp1.stopMining()
	sp2.stopMining()
	time.Sleep(300 * time.Millisecond)

	resp = postChat(t, gwServer.URL, "After recovery")
	if resp.StatusCode != 200 {
		t.Errorf("expected 200 after recovery, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Test 5: Deregister worker 2
	registry.Deregister("sp-worker-2")
	stats := registry.Stats()
	if stats.TotalWorkers != 1 {
		t.Errorf("expected 1 worker after deregister, got %d", stats.TotalWorkers)
	}

	t.Log("Integration test passed: register → poll → mine → failover → recover → deregister")
}

func TestIntegration_StreamRewrite(t *testing.T) {
	sp := newMockSPWorker()
	defer sp.close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	registry := worker.NewRegistry(logger, "")

	registry.Register(worker.WorkerRegistration{
		ID: "sp-1", Endpoint: sp.inference.URL,
		SchedulerURL: sp.scheduler.URL, GPUCount: 1,
	})
	registry.UpdateState("sp-1", "GPU_STATE_AVAILABLE", "running", 0, "model", 1)

	gw := gateway.New(registry, config.GatewayConfig{RequestTimeoutSec: 10}, logger)
	gwServer := httptest.NewServer(gw.Handler())
	defer gwServer.Close()

	// Send request with stream: true — gateway should rewrite to false
	resp, err := http.Post(gwServer.URL+"/v1/chat/completions", "application/json",
		bytes.NewBufferString(`{"model":"default","messages":[{"role":"user","content":"test"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200 even with stream:true, got %d", resp.StatusCode)
	}

	// Response should be a normal JSON object, not SSE
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("expected JSON response (not SSE): %v", err)
	}
	if body["choices"] == nil {
		t.Error("expected choices in response")
	}
}

func postChat(t *testing.T, baseURL, content string) *http.Response {
	t.Helper()
	body := fmt.Sprintf(`{"model":"default","messages":[{"role":"user","content":"%s"}],"max_tokens":10}`, content)
	resp, err := http.Post(baseURL+"/v1/chat/completions", "application/json",
		bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
