package worker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// TestPoller_FailureThreshold verifies that a worker is NOT marked offline
// on first failure, only after reaching the configured threshold.
func TestPoller_FailureThreshold(t *testing.T) {
	// Mock server that returns 503 (simulating a temporary failure)
	var failCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	registry := NewRegistry(logger, "")
	registry.Register(WorkerRegistration{
		ID:           "w1",
		Endpoint:     server.URL,
		SchedulerURL: server.URL,
	})

	// Set to idle first so we can observe the transition
	registry.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "model", 1)

	threshold := 3
	var offlineEvents atomic.Int32
	poller := NewPoller(registry, 50*time.Millisecond, threshold, logger)
	poller.SetOnChange(func(id string, old, new WorkerState) {
		if new == StateOffline {
			offlineEvents.Add(1)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go poller.Run(ctx)

	// Let it poll a few times
	<-ctx.Done()

	// Should have seen exactly one offline transition (happens once when threshold is first reached)
	events := offlineEvents.Load()
	if events != 1 {
		t.Errorf("expected exactly 1 offline event, got %d (failures=%d)",
			events, failCount.Load())
	}

	// Verify the worker record shows consecutive failures
	w, _ := registry.Get("w1")
	if w.ConsecutiveFailures < threshold {
		t.Errorf("expected consecutive_failures >= %d, got %d", threshold, w.ConsecutiveFailures)
	}
}

// TestPoller_TransientFailureDoesNotMarkOffline verifies that a single
// failed poll followed by a successful poll does NOT mark the worker offline.
func TestPoller_TransientFailureDoesNotMarkOffline(t *testing.T) {
	var shouldFail atomic.Bool
	shouldFail.Store(true)

	// go-scheduler endpoint: fails once, then succeeds
	sched := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if shouldFail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ready, gpu_state=GPU_STATE_AVAILABLE")
	}))
	defer sched.Close()

	// py-inference endpoint: always succeeds
	infer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"status":"ok","engine_state":"running","active_requests":0,"loaded_model":"m"}`)
	}))
	defer infer.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	registry := NewRegistry(logger, "")
	registry.Register(WorkerRegistration{
		ID:           "w1",
		Endpoint:     infer.URL,
		SchedulerURL: sched.URL,
	})
	registry.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "m", 1)

	threshold := 3
	var offlineEvents atomic.Int32
	poller := NewPoller(registry, 50*time.Millisecond, threshold, logger)
	poller.SetOnChange(func(id string, old, new WorkerState) {
		if new == StateOffline {
			offlineEvents.Add(1)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poller.Run(ctx)

	// Wait for one failed poll
	time.Sleep(75 * time.Millisecond)

	// Check failure is recorded but worker is NOT offline
	w, _ := registry.Get("w1")
	if w.ConsecutiveFailures == 0 {
		t.Error("expected failure to be recorded")
	}
	if w.State == StateOffline {
		t.Error("worker should not be offline after single failure")
	}

	// Recover
	shouldFail.Store(false)
	time.Sleep(150 * time.Millisecond)

	// Failures should be reset, no offline event
	w, _ = registry.Get("w1")
	if w.ConsecutiveFailures != 0 {
		t.Errorf("expected consecutive_failures=0 after recovery, got %d", w.ConsecutiveFailures)
	}
	if offlineEvents.Load() != 0 {
		t.Errorf("expected 0 offline events (transient failure recovered), got %d", offlineEvents.Load())
	}
}
