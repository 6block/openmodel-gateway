package health

import (
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"openmodel/sp-state-agent/internal/metrics"
	"openmodel/sp-state-agent/internal/worker"
)

func hLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestUpdateMetrics_GaugesAndStaleLabelCleanup(t *testing.T) {
	reg := worker.NewRegistry(hLog(), "")
	reg.Register(worker.WorkerRegistration{ID: "hw1", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", GPUCount: 1})
	reg.UpdateState("hw1", "GPU_STATE_AVAILABLE", "running", 0, "m", 1) // idle

	UpdateMetrics(reg)
	if v := testutil.ToFloat64(metrics.WorkersGauge.WithLabelValues("idle")); v != 1 {
		t.Errorf("idle gauge = %v, want 1", v)
	}

	// deregister → the next refresh must zero the idle gauge and drop the stale
	// per-worker failure label (cardinality-leak fix).
	reg.Deregister("hw1")
	UpdateMetrics(reg)
	if v := testutil.ToFloat64(metrics.WorkersGauge.WithLabelValues("idle")); v != 0 {
		t.Errorf("idle gauge after deregister = %v, want 0", v)
	}
}

func TestRecordStateTransition(t *testing.T) {
	before := testutil.ToFloat64(metrics.StateTransitionsTotal.WithLabelValues(
		string(worker.StateIdle), string(worker.StateBusy)))
	RecordStateTransition(hLog(), "hw1", worker.StateIdle, worker.StateBusy)
	after := testutil.ToFloat64(metrics.StateTransitionsTotal.WithLabelValues(
		string(worker.StateIdle), string(worker.StateBusy)))
	if after != before+1 {
		t.Errorf("transition counter: before=%v after=%v (want +1)", before, after)
	}
}
