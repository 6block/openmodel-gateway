package worker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPollerFetchGPUState(t *testing.T) {
	// Mock go-scheduler /ready endpoint
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ready" {
			fmt.Fprintln(w, "ready, gpu_state=GPU_STATE_AVAILABLE")
		}
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	registry := NewRegistry(logger, "")
	poller := NewPoller(registry, time.Second, 3, logger)

	state, err := poller.fetchGPUState(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if state != "GPU_STATE_AVAILABLE" {
		t.Errorf("expected GPU_STATE_AVAILABLE, got %q", state)
	}
}

func TestPollerFetchInferenceHealth(t *testing.T) {
	// Mock py-inference /health endpoint
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"status":"ok","engine_state":"running","active_requests":3,"loaded_model":"Qwen2.5-3B"}`)
		}
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	registry := NewRegistry(logger, "")
	poller := NewPoller(registry, time.Second, 3, logger)

	engineState, activeReqs, model, _, err := poller.fetchInferenceHealth(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if engineState != "running" {
		t.Errorf("expected running, got %q", engineState)
	}
	if activeReqs != 3 {
		t.Errorf("expected 3, got %d", activeReqs)
	}
	if model != "Qwen2.5-3B" {
		t.Errorf("expected Qwen2.5-3B, got %q", model)
	}
}

func TestPollerStateChange(t *testing.T) {
	// Mock both endpoints
	var gpuState atomic.Value
	gpuState.Store("GPU_STATE_AVAILABLE")

	scheduler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ready, gpu_state=%s\n", gpuState.Load().(string))
	}))
	defer scheduler.Close()

	inference := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"status":"ok","engine_state":"running","active_requests":0,"loaded_model":"test"}`)
	}))
	defer inference.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	registry := NewRegistry(logger, "")
	registry.Register(WorkerRegistration{
		ID:           "w1",
		Endpoint:     inference.URL,
		SchedulerURL: scheduler.URL,
	})

	type stateChange struct{ old, new_ WorkerState }
	var mu sync.Mutex
	var stateChanges []stateChange

	poller := NewPoller(registry, 100*time.Millisecond, 3, logger)
	poller.SetOnChange(func(id string, old, new_ WorkerState) {
		mu.Lock()
		stateChanges = append(stateChanges, stateChange{old, new_})
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	go poller.Run(ctx)

	// Wait for initial poll (offline -> idle)
	time.Sleep(200 * time.Millisecond)

	// Switch to mining
	gpuState.Store("GPU_STATE_WINDOW_POST")
	time.Sleep(200 * time.Millisecond)

	cancel()

	mu.Lock()
	changes := make([]stateChange, len(stateChanges))
	copy(changes, stateChanges)
	mu.Unlock()

	if len(changes) < 2 {
		t.Fatalf("expected at least 2 state changes, got %d", len(changes))
	}

	// First change: offline -> idle
	if changes[0].old != StateOffline || changes[0].new_ != StateIdle {
		t.Errorf("first change: expected offline->idle, got %s->%s",
			changes[0].old, changes[0].new_)
	}

	// Second change: idle -> mining
	if changes[1].old != StateIdle || changes[1].new_ != StateMining {
		t.Errorf("second change: expected idle->mining, got %s->%s",
			changes[1].old, changes[1].new_)
	}
}

func TestPollerWorkerUnreachable(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	registry := NewRegistry(logger, "")
	registry.Register(WorkerRegistration{
		ID:           "w1",
		Endpoint:     "http://127.0.0.1:19999", // unreachable
		SchedulerURL: "http://127.0.0.1:19998",
	})

	// Set to idle first
	registry.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "model", 1)

	var gotOffline atomic.Bool
	poller := NewPoller(registry, 100*time.Millisecond, 3, logger)
	poller.SetOnChange(func(id string, old, new WorkerState) {
		if new == StateOffline {
			gotOffline.Store(true)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	go poller.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	cancel()

	if !gotOffline.Load() {
		t.Error("expected worker to be marked offline when unreachable")
	}
}
