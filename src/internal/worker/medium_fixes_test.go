package worker

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func quietRegistry(t *testing.T) *Registry {
	t.Helper()
	return NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)), "")
}

// TestInflightCounting covers the audit MEDIUM fix: the registry tracks the gateway's
// own in-flight request count per worker, exposed on List()/Get() snapshots, so the
// load balancer has a real-time signal between polls.
func TestInflightCounting(t *testing.T) {
	r := quietRegistry(t)
	r.Register(WorkerRegistration{ID: "w1", Endpoint: "http://x:1", SchedulerURL: "http://x:2", GPUCount: 4})

	r.IncInflight("w1")
	r.IncInflight("w1")
	r.IncInflight("w1")

	if g, _ := r.Get("w1"); g.InFlight != 3 {
		t.Errorf("Get: expected InFlight=3, got %d", g.InFlight)
	}
	if list := r.List(); len(list) != 1 || list[0].InFlight != 3 {
		t.Errorf("List: expected InFlight=3, got %+v", list)
	}

	r.DecInflight("w1")
	if g, _ := r.Get("w1"); g.InFlight != 2 {
		t.Errorf("after one Dec: expected InFlight=2, got %d", g.InFlight)
	}

	// Decrement past zero must floor at zero, not go negative.
	r.DecInflight("w1")
	r.DecInflight("w1")
	r.DecInflight("w1")
	if g, _ := r.Get("w1"); g.InFlight != 0 {
		t.Errorf("after over-Dec: expected InFlight floored at 0, got %d", g.InFlight)
	}
}

// TestLoadingTimeoutMarksOffline covers the audit MEDIUM fix: a worker stuck in the
// "loading" state past loadingTimeout is treated as offline, and the timer resets
// once the worker leaves the loading state.
func TestLoadingTimeoutMarksOffline(t *testing.T) {
	r := quietRegistry(t)
	r.Register(WorkerRegistration{ID: "w1", Endpoint: "http://x:1", SchedulerURL: "http://x:2", GPUCount: 1})
	r.SetLoadingTimeout(50 * time.Millisecond)

	// First loading poll → state "loading" (timer starts), not offline yet.
	_, ns, _ := r.UpdateState("w1", "GPU_STATE_AVAILABLE", "loading", 0, "m", 1)
	if ns != StateLoading {
		t.Fatalf("first loading poll: expected loading, got %s", ns)
	}

	// After the timeout, a still-loading poll flips the worker to offline.
	time.Sleep(70 * time.Millisecond)
	_, ns, _ = r.UpdateState("w1", "GPU_STATE_AVAILABLE", "loading", 0, "m", 1)
	if ns != StateOffline {
		t.Fatalf("stuck-loading poll past timeout: expected offline, got %s", ns)
	}

	// Recovering to running clears the timer; a fresh loading episode is not
	// immediately offline.
	r.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "m", 1)
	_, ns, _ = r.UpdateState("w1", "GPU_STATE_AVAILABLE", "loading", 0, "m", 1)
	if ns != StateLoading {
		t.Fatalf("loading after recovery: expected fresh loading, got %s", ns)
	}
}
