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
			strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"test"}]}`),
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
	// The gateway now re-queues across queue windows until the overall request
	// deadline, so the terminal 503 is bounded by requestTimeout (not a single
	// queueTimeout). Keep both short for this test.
	gw.queueTimeout = 1 * time.Second
	gw.requestTimeout = 2 * time.Second

	// All workers mining
	registry.UpdateState("w1", "GPU_STATE_WINDOW_POST", "paused", 0, "model", 1)

	gwServer := httptest.NewServer(gw.Handler())
	defer gwServer.Close()

	t0 := time.Now()
	resp, err := http.Post(
		gwServer.URL+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"test"}]}`),
	)
	elapsed := time.Since(t0)

	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 503 {
		t.Errorf("expected 503 after queue timeout, got %d", resp.StatusCode)
	}

	// Should have waited approximately requestTimeout (the overall deadline)
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
				strings.NewReader(fmt.Sprintf(`{"model":"test-model","messages":[{"role":"user","content":"req %d"}]}`, n)),
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
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"test"}]}`),
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

// TestQueue_RecoversBeforeDeadlineSucceeds: a request arriving while the only
// worker is mining must ride through a brief yield transparently — the worker
// recovers INSIDE the queue window and the request succeeds. This is the
// WinningPoSt case: ~35s of mining against the default 60s window needs exactly
// one window, never a re-queue. (Recovery LONGER than the window is the
// WindowPoSt case and must fail fast instead — see
// TestQueue_LongYieldFailsFastAfterQueueTimeout; the old expectation of
// re-queuing across windows is the hang the 24h soak flagged as finding #1.)
func TestQueue_RecoversBeforeDeadlineSucceeds(t *testing.T) {
	gw, registry, _ := newTestGateway(t)
	gw.queueTimeout = 2 * time.Second // > 1s poll interval
	gw.requestTimeout = 10 * time.Second

	registry.UpdateState("w1", "GPU_STATE_WINDOW_POST", "paused", 0, "model", 1)
	go func() {
		// Recover inside the queue window, and clear of the window's edge: the
		// queue re-checks every 1s, so recovery at 0.8s is seen by the t=1s tick.
		// Recovery at 1.2s would race the t=2s tick against the t=2s deadline in
		// the same select (a coin flip → flaky).
		time.Sleep(800 * time.Millisecond)
		registry.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "model", 1)
	}()

	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	t0 := time.Now()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 after worker recovered in-window, got %d", resp.StatusCode)
	}
	if d := time.Since(t0); d < 1*time.Second {
		t.Errorf("expected to wait for recovery (~1.2s), returned in %v", d)
	}
}

// B1 short-circuit: when the only mining candidate advertises a recovery
// estimate far beyond the queue window, the 503 must be immediate — burning the
// whole window before saying "come back in 18 minutes" helps nobody.
func TestQueue_MiningEstimateBeyondBudgetFailsImmediately(t *testing.T) {
	gw, registry, _ := newTestGateway(t) // queueTimeout=5s

	registry.UpdateState("w1", "GPU_STATE_WINDOW_POST", "paused", 0, "model", 1)
	registry.SetUntilChange("w1", 1100) // ~18 min to resume — a WindowPoSt

	gwServer := httptest.NewServer(gw.Handler())
	defer gwServer.Close()

	t0 := time.Now()
	resp, err := http.Post(
		gwServer.URL+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"x"}]}`),
	)
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("known-long yield must 503 immediately, took %v", elapsed)
	}
	// Retry-After must reflect the actual estimate (clamped to 120), not the 30s fallback.
	if ra := resp.Header.Get("Retry-After"); ra != "120" {
		t.Fatalf("Retry-After should carry the clamped honest estimate 120, got %q", ra)
	}
}

// TestRetry_WorkerReturns503ThenRecovers: the worker stays "available" in the
// registry but answers 503 (mining mid-flight) for a while, then 200. The gateway
// must keep retrying/queuing until it gets a real answer rather than give up with
// 503 — the exact pattern behind the soak's "all retries failed" failures.
func TestRetry_WorkerReturns503ThenRecovers(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	registry := worker.NewRegistry(logger, "")
	var mining atomic.Bool
	mining.Store(true)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mining.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"mining"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]string{"role": "assistant", "content": "ok"}, "finish_reason": "stop"},
			},
		})
	}))
	defer backend.Close()
	registry.Register(worker.WorkerRegistration{ID: "w1", Endpoint: backend.URL, SchedulerURL: "http://unused:9090", GPUCount: 1})
	registry.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "model", 1)
	gw := New(registry, config.GatewayConfig{RequestTimeoutSec: 10}, logger)
	gw.queueTimeout = 2 * time.Second // > 1s poll interval

	go func() {
		time.Sleep(1500 * time.Millisecond)
		mining.Store(false) // worker finishes mining, starts answering 200
	}()

	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 after worker stopped returning 503, got %d", resp.StatusCode)
	}
}

// Soak finding #1: with every worker mining, a request must fail fast after ONE
// queueTimeout window (503 + Retry-After) — not silently re-queue window after
// window until requestTimeout. During a 19-minute WindowPoSt the old behavior
// hung every request for 5 minutes; clients gave up at 90s having received
// nothing at all.
func TestQueue_LongYieldFailsFastAfterQueueTimeout(t *testing.T) {
	gw, registry, _ := newTestGateway(t) // queueTimeout=5s, requestTimeout=10s

	registry.UpdateState("w1", "GPU_STATE_WINDOW_POST", "paused", 0, "model", 1)

	gwServer := httptest.NewServer(gw.Handler())
	defer gwServer.Close()

	t0 := time.Now()
	resp, err := http.Post(
		gwServer.URL+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"x"}]}`),
	)
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
	// One queue window (5s) plus scheduling slack — NOT the 10s request timeout.
	if elapsed < 4*time.Second || elapsed > 8*time.Second {
		t.Fatalf("503 should arrive right after the queue window (~5s), took %v", elapsed)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("503 must carry a Retry-After header")
	}
}
