// Package health aggregates worker state into Prometheus gauges.
// Low-level counters live in the metrics package (which has no worker dependency).
package health

import (
	"log/slog"
	"sync"

	"openmodel/sp-state-agent/internal/metrics"
	"openmodel/sp-state-agent/internal/worker"
)

// knownWorkerIDs tracks which worker IDs have been seen by UpdateMetrics.
// When a worker is deregistered, its metric label is removed to prevent cardinality leak.
var (
	knownWorkersMu sync.Mutex
	knownWorkers   = make(map[string]struct{})
)

// UpdateMetrics refreshes gauges from the registry snapshot.
func UpdateMetrics(registry *worker.Registry) {
	stats := registry.Stats()
	metrics.WorkersGauge.WithLabelValues("idle").Set(float64(stats.IdleWorkers))
	metrics.WorkersGauge.WithLabelValues("busy").Set(float64(stats.BusyWorkers))
	metrics.WorkersGauge.WithLabelValues("mining").Set(float64(stats.MiningWorkers))
	metrics.WorkersGauge.WithLabelValues("loading").Set(float64(stats.LoadingWorkers))
	metrics.WorkersGauge.WithLabelValues("offline").Set(float64(stats.OfflineWorkers))

	currentIDs := make(map[string]struct{})
	for _, w := range registry.List() {
		metrics.WorkerConsecutiveFailures.WithLabelValues(w.ID).Set(float64(w.ConsecutiveFailures))
		currentIDs[w.ID] = struct{}{}
	}

	// Remove stale labels for deregistered workers
	knownWorkersMu.Lock()
	for id := range knownWorkers {
		if _, exists := currentIDs[id]; !exists {
			metrics.WorkerConsecutiveFailures.DeleteLabelValues(id)
		}
	}
	knownWorkers = currentIDs
	knownWorkersMu.Unlock()
}

// RecordStateTransition increments the state transition counter.
// Note: the poller already logs the transition at Info level, so we don't log again here.
func RecordStateTransition(_ *slog.Logger, _ string, oldState, newState worker.WorkerState) {
	metrics.StateTransitionsTotal.WithLabelValues(string(oldState), string(newState)).Inc()
}
