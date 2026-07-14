package gateway

import (
	"log/slog"
	"os"
	"testing"

	"openmodel/sp-state-agent/internal/worker"
)

func routerTestRegistry(t *testing.T) *worker.Registry {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return worker.NewRegistry(logger, "")
}

// G3: the router must never select a worker that is mining, loading, or offline.
func TestSelect_ExcludesNonRoutableWorkers(t *testing.T) {
	reg := routerTestRegistry(t)
	for _, id := range []string{"idle", "mining", "loading", "offline"} {
		reg.Register(worker.WorkerRegistration{
			ID: id, Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", GPUCount: 1,
		})
	}
	reg.UpdateState("idle", "GPU_STATE_AVAILABLE", "running", 0, "m", 1)
	reg.UpdateState("mining", "GPU_STATE_WINDOW_POST", "paused", 0, "m", 1)
	reg.UpdateState("loading", "GPU_STATE_AVAILABLE", "loading", 0, "m", 1)
	// "offline" left without a successful poll → offline state.

	// Repeatedly select; must always be the idle worker.
	for i := 0; i < 50; i++ {
		w, err := selectWorkerForModel(reg, "default", nil)
		if err != nil {
			t.Fatalf("expected a routable worker, got error: %v", err)
		}
		if w.ID != "idle" {
			t.Fatalf("G3 regression: selected non-routable worker %q (state=%v)", w.ID, w.State)
		}
	}
}

func TestSelect_NoWorkersWhenAllMining(t *testing.T) {
	reg := routerTestRegistry(t)
	reg.Register(worker.WorkerRegistration{
		ID: "w1", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", GPUCount: 1,
	})
	reg.UpdateState("w1", "GPU_STATE_WINNING_POST", "paused", 0, "m", 1)

	if _, err := selectWorkerForModel(reg, "default", nil); err == nil {
		t.Fatal("expected ErrNoWorkerAvailable when the only worker is mining")
	}
}
