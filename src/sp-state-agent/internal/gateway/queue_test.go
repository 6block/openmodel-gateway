package gateway

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/worker"
)

func newTestGateway(t *testing.T) (*Gateway, *worker.Registry, *httptest.Server) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	registry := worker.NewRegistry(logger, "")

	// Mock backend
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]string{"role": "assistant", "content": "hello"}, "finish_reason": "stop"},
			},
		})
	}))
	t.Cleanup(backend.Close)

	registry.Register(worker.WorkerRegistration{
		ID: "w1", Endpoint: backend.URL, SchedulerURL: "http://unused:9090", GPUCount: 1,
	})

	gw := New(registry, config.GatewayConfig{RequestTimeoutSec: 10}, logger)
	gw.queueTimeout = 5 * time.Second // Short timeout for tests

	return gw, registry, backend
}

func TestQueue_WorkerRecoversDuringWait(t *testing.T) {
	gw, registry, _ := newTestGateway(t)

	// Set worker to mining — no workers available
	registry.UpdateState("w1", "GPU_STATE_WINDOW_POST", "paused", 0, "model", 1)

	gwServer := httptest.NewServer(gw.Handler())
	defer gwServer.Close()

	// Send request in background — should queue
	var resp *http.Response
	var reqErr error
	done := make(chan struct{})
	go func() {
		resp, reqErr = http.Post(
			gwServer.URL+"/v1/chat/completions",
			"application/json",
			strings.NewReader(`{"model":"default","messages":[{"role":"user","content":"test"}]}`),
		)
		close(done)
	}()

	// After 2s, recover the worker
	time.Sleep(2 * time.Second)
	registry.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "model", 1)

	// Wait for response
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("request timed out")
	}

	if reqErr != nil {
		t.Fatal(reqErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200 after queue release, got %d", resp.StatusCode)
	}
}

func TestQueue_TimeoutReturns503(t *testing.T) {
	gw, registry, _ := newTestGateway(t)
	gw.queueTimeout = 2 * time.Second // Very short for this test

	// All workers mining
	registry.UpdateState("w1", "GPU_STATE_WINDOW_POST", "paused", 0, "model", 1)

	gwServer := httptest.NewServer(gw.Handler())
	defer gwServer.Close()

	t0 := time.Now()
	resp, err := http.Post(
		gwServer.URL+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"default","messages":[{"role":"user","content":"test"}]}`),
	)
	elapsed := time.Since(t0)

	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 503 {
		t.Errorf("expected 503 after queue timeout, got %d", resp.StatusCode)
	}

	// Should have waited approximately queueTimeout
	if elapsed < 1*time.Second || elapsed > 4*time.Second {
		t.Errorf("expected ~2s wait, got %v", elapsed)
	}
}

func TestQueue_MultipleRequestsQueued(t *testing.T) {
	gw, registry, _ := newTestGateway(t)

	// All workers mining
	registry.UpdateState("w1", "GPU_STATE_WINDOW_POST", "paused", 0, "model", 1)

	gwServer := httptest.NewServer(gw.Handler())
	defer gwServer.Close()

	// Send 3 requests concurrently — all should queue
	var wg sync.WaitGroup
	var successCount atomic.Int32

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			resp, err := http.Post(
				gwServer.URL+"/v1/chat/completions",
				"application/json",
				strings.NewReader(fmt.Sprintf(`{"model":"default","messages":[{"role":"user","content":"req %d"}]}`, n)),
			)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				successCount.Add(1)
			}
		}(i)
	}

	// Recover worker after 1.5s — all 3 queued requests should succeed
	time.Sleep(1500 * time.Millisecond)
	registry.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "model", 1)

	wg.Wait()

	if successCount.Load() != 3 {
		t.Errorf("expected all 3 queued requests to succeed, got %d", successCount.Load())
	}
}

func TestQueue_NoQueueWhenWorkerAvailable(t *testing.T) {
	gw, registry, _ := newTestGateway(t)

	// Worker is idle — should NOT queue
	registry.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "model", 1)

	gwServer := httptest.NewServer(gw.Handler())
	defer gwServer.Close()

	t0 := time.Now()
	resp, err := http.Post(
		gwServer.URL+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"default","messages":[{"role":"user","content":"test"}]}`),
	)
	elapsed := time.Since(t0)

	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	// Should be fast — no queue wait
	if elapsed > 2*time.Second {
		t.Errorf("expected fast response (no queue), took %v", elapsed)
	}
}
