package worker

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistence_SaveAndReload(t *testing.T) {
	dir := t.TempDir()
	savePath := filepath.Join(dir, "workers.json")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create registry and register workers
	r1 := NewRegistry(logger, savePath)
	r1.Register(WorkerRegistration{
		ID: "w1", Endpoint: "http://a:8000", SchedulerURL: "http://a:9090", GPUCount: 4, MinerAddress: "t01",
	})
	r1.Register(WorkerRegistration{
		ID: "w2", Endpoint: "http://b:8000", SchedulerURL: "http://b:9090", GPUCount: 8, MinerAddress: "t02",
	})
	// Verify file exists
	if _, err := os.Stat(savePath); err != nil {
		t.Fatalf("save file should exist: %v", err)
	}

	// Load into new registry
	r2 := NewRegistry(logger, savePath)

	workers := r2.List()
	if len(workers) != 2 {
		t.Fatalf("expected 2 workers after reload, got %d", len(workers))
	}

	w1, ok := r2.Get("w1")
	if !ok {
		t.Fatal("w1 not found after reload")
	}
	if w1.Endpoint != "http://a:8000" {
		t.Errorf("endpoint: want http://a:8000, got %s", w1.Endpoint)
	}
	if w1.GPUCount != 4 {
		t.Errorf("gpu_count: want 4, got %d", w1.GPUCount)
	}
	if w1.State != StateOffline {
		t.Errorf("state after reload should be offline, got %s", w1.State)
	}
}

func TestPersistence_RegisteredAtPreserved(t *testing.T) {
	dir := t.TempDir()
	savePath := filepath.Join(dir, "workers.json")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Register at a known time
	r1 := NewRegistry(logger, savePath)
	r1.Register(WorkerRegistration{
		ID: "w1", Endpoint: "http://a:8000", SchedulerURL: "http://a:9090",
	})

	w1, _ := r1.Get("w1")
	originalTime := w1.RegisteredAt

	// Small delay to prove time difference
	time.Sleep(10 * time.Millisecond)

	// Reload
	r2 := NewRegistry(logger, savePath)
	w1Reloaded, _ := r2.Get("w1")

	// RegisteredAt should be the original time, NOT time.Now()
	if w1Reloaded.RegisteredAt.Sub(originalTime) > time.Millisecond {
		t.Errorf("RegisteredAt should be preserved. Original: %v, Reloaded: %v",
			originalTime, w1Reloaded.RegisteredAt)
	}
}

func TestPersistence_AtomicWrite_NoCorruption(t *testing.T) {
	dir := t.TempDir()
	savePath := filepath.Join(dir, "workers.json")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Write valid data
	r1 := NewRegistry(logger, savePath)
	r1.Register(WorkerRegistration{
		ID: "w1", Endpoint: "http://a:8000", SchedulerURL: "http://a:9090",
	})

	// Simulate a "crashed" temp file left behind (should not affect loading)
	tmpPath := savePath + ".tmp"
	os.WriteFile(tmpPath, []byte("garbage"), 0644)

	// Loading should still work from the main file
	r2 := NewRegistry(logger, savePath)
	if _, ok := r2.Get("w1"); !ok {
		t.Error("worker should be loadable even with stale .tmp file")
	}
}

func TestPersistence_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	savePath := filepath.Join(dir, "workers.json")

	// Write empty JSON array
	os.WriteFile(savePath, []byte("[]"), 0644)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r := NewRegistry(logger, savePath)

	if len(r.List()) != 0 {
		t.Errorf("expected 0 workers from empty file, got %d", len(r.List()))
	}
}

func TestPersistence_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	savePath := filepath.Join(dir, "workers.json")

	// Write invalid JSON
	os.WriteFile(savePath, []byte("not json at all"), 0644)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r := NewRegistry(logger, savePath)

	// Should not crash, just log error and start empty
	if len(r.List()) != 0 {
		t.Errorf("expected 0 workers from corrupt file, got %d", len(r.List()))
	}
}

func TestPersistence_DeregisterRemovesFromDisk(t *testing.T) {
	dir := t.TempDir()
	savePath := filepath.Join(dir, "workers.json")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	r1 := NewRegistry(logger, savePath)
	r1.Register(WorkerRegistration{
		ID: "w1", Endpoint: "http://a:8000", SchedulerURL: "http://a:9090",
	})
	r1.Register(WorkerRegistration{
		ID: "w2", Endpoint: "http://b:8000", SchedulerURL: "http://b:9090",
	})
	r1.Deregister("w1")

	// Reload — only w2 should exist
	r2 := NewRegistry(logger, savePath)
	if _, ok := r2.Get("w1"); ok {
		t.Error("w1 should NOT exist after deregister + reload")
	}
	if _, ok := r2.Get("w2"); !ok {
		t.Error("w2 should exist after reload")
	}
}

func TestPersistence_AuthTokenSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	savePath := filepath.Join(dir, "workers.json")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	r1 := NewRegistry(logger, savePath)
	r1.Register(WorkerRegistration{
		ID: "w1", Endpoint: "http://a:8000", SchedulerURL: "http://a:9090", AuthToken: "per-worker-secret",
	})

	// Reload into a fresh registry. The per-worker token MUST survive — otherwise a
	// gateway restart loads the worker without it, the poller sends no token, and the
	// worker's /ready 401s → it flaps offline (regression guard for that bug).
	r2 := NewRegistry(logger, savePath)
	w, ok := r2.Get("w1")
	if !ok {
		t.Fatal("w1 not found after reload")
	}
	if w.AuthToken != "per-worker-secret" {
		t.Errorf("auth_token did not survive reload: got %q", w.AuthToken)
	}
}
