package gateway

import (
	"testing"

	"openmodel/sp-state-agent/internal/settlement"
	"openmodel/sp-state-agent/internal/worker"
)

// The published metric must mean "a client can be served this model right now",
// not "the catalog mentions it". These tests pin that distinction: a catalog
// entry no live worker serves must never be counted, or the grant metric would
// report capacity nobody can reach.
func TestAvailableModelCount(t *testing.T) {
	cfg := &settlement.Config{
		ModelPricesUSD: map[string]string{
			"default":     "0.60",
			"model-live":  "0.60",
			"model-ghost": "0.60", // priced in the catalog, served by nobody
		},
	}

	newGW := func(t *testing.T, workers []worker.Worker, gateOn bool) *Gateway {
		t.Helper()
		reg := worker.NewRegistry(testLogger(), "")
		reg.SetAdmissionGate(gateOn)
		for i := range workers {
			w := workers[i]
			reg.Register(worker.WorkerRegistration{
				ID: w.ID, Endpoint: "http://w:8000", SchedulerURL: "http://w:9090",
				GPUCount: 1, SupportedModels: w.SupportedModels,
			})
			reg.UpdateState(w.ID, "GPU_STATE_AVAILABLE", "running", 0, w.LoadedModel, 1)
			if len(w.VerifiedModels) > 0 {
				reg.SetVerified(w.ID, w.VerifiedModels)
			}
		}
		return &Gateway{registry: reg, settlementCfg: cfg}
	}

	t.Run("catalog entry with no worker is not counted", func(t *testing.T) {
		g := newGW(t, []worker.Worker{
			{ID: "w1", LoadedModel: "model-live", SupportedModels: []string{"model-live"}},
		}, false)
		if got := g.AvailableModelCount(); got != 1 {
			t.Fatalf("want 1 (only model-live is served), got %d — model-ghost leaked in", got)
		}
	})

	t.Run("no workers at all means zero, not catalog size", func(t *testing.T) {
		g := newGW(t, nil, false)
		if got := g.AvailableModelCount(); got != 0 {
			t.Fatalf("want 0 with no live workers, got %d", got)
		}
	})

	t.Run("two workers serving the same model count once", func(t *testing.T) {
		g := newGW(t, []worker.Worker{
			{ID: "w1", LoadedModel: "model-live", SupportedModels: []string{"model-live"}},
			{ID: "w2", LoadedModel: "model-live", SupportedModels: []string{"model-live"}},
		}, false)
		if got := g.AvailableModelCount(); got != 1 {
			t.Fatalf("want 1 distinct model, got %d", got)
		}
	})

	t.Run("offline worker's models do not count", func(t *testing.T) {
		reg := worker.NewRegistry(testLogger(), "")
		reg.Register(worker.WorkerRegistration{
			ID: "w1", Endpoint: "http://w:8000", SchedulerURL: "http://w:9090",
			GPUCount: 1, SupportedModels: []string{"model-live"},
		})
		// Never transitioned to idle/busy: stays offline, so nothing is routable.
		g := &Gateway{registry: reg, settlementCfg: cfg}
		if got := g.AvailableModelCount(); got != 0 {
			t.Fatalf("want 0 while the only worker is offline, got %d", got)
		}
	})
}
