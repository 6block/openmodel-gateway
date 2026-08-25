package gateway

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"openmodel/sp-state-agent/internal/worker"
)

// Routing-ban behavior: a banned worker is polled but never selected — including
// through session affinity — and recovers automatically when the ban expires.

func banTestRegistry(t *testing.T, ids ...string) *worker.Registry {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := worker.NewRegistry(logger, "")
	for _, id := range ids {
		if _, err := reg.Register(worker.WorkerRegistration{
			ID: id, Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", GPUCount: 1,
		}); err != nil {
			t.Fatal(err)
		}
		reg.UpdateState(id, "GPU_STATE_AVAILABLE", "running", 0, "m", 1)
	}
	return reg
}

func TestBan_RouterSkipsBannedWorker(t *testing.T) {
	reg := banTestRegistry(t, "good", "bad")
	if !reg.SetBan("bad", time.Now().Add(time.Hour), "substandard output") {
		t.Fatal("SetBan failed")
	}

	for i := 0; i < 50; i++ {
		w, err := selectWorkerForModel(reg, "default", nil)
		if err != nil {
			t.Fatalf("expected the non-banned worker, got error: %v", err)
		}
		if w.ID != "good" {
			t.Fatalf("banned worker %q was selected", w.ID)
		}
	}
}

func TestBan_AllBannedMeansNoWorker(t *testing.T) {
	reg := banTestRegistry(t, "w1")
	reg.SetBan("w1", time.Now().Add(time.Hour), "x")
	if _, err := selectWorkerForModel(reg, "default", nil); err == nil {
		t.Fatal("banned-only pool must yield no worker")
	}
}

func TestBan_ExpiryRestoresRouting(t *testing.T) {
	reg := banTestRegistry(t, "w1")
	reg.SetBan("w1", time.Now().Add(50*time.Millisecond), "short ban")
	if _, err := selectWorkerForModel(reg, "default", nil); err == nil {
		t.Fatal("should be unroutable while banned")
	}
	time.Sleep(60 * time.Millisecond)
	w, err := selectWorkerForModel(reg, "default", nil)
	if err != nil || w.ID != "w1" {
		t.Fatalf("expired ban should restore routing, got %v / %v", w, err)
	}
}

func TestBan_ClearLiftsImmediately(t *testing.T) {
	reg := banTestRegistry(t, "w1")
	reg.SetBan("w1", time.Now().Add(time.Hour), "x")
	reg.SetBan("w1", time.Time{}, "") // lift
	if w, err := selectWorkerForModel(reg, "default", nil); err != nil || w.ID != "w1" {
		t.Fatalf("cleared ban should restore routing, got %v / %v", w, err)
	}
}

func TestBan_AffinityDoesNotTunnel(t *testing.T) {
	reg := banTestRegistry(t, "sticky", "other")
	g := &Gateway{
		registry: reg,
		sessions: newSessionAffinity(0),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// The session previously stuck to "sticky"; then "sticky" got banned.
	g.sessions.put("sess-1", "sticky")
	reg.SetBan("sticky", time.Now().Add(time.Hour), "x")

	for i := 0; i < 20; i++ {
		w, err := g.selectWithAffinity("sess-1", "default")
		if err != nil {
			t.Fatalf("fallback selection failed: %v", err)
		}
		if w.ID == "sticky" {
			t.Fatal("session affinity tunneled around the routing ban")
		}
	}
}
