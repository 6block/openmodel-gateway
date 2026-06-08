// Package metrics holds Prometheus metrics that can be incremented from any
// package without creating import cycles. The health package provides the
// aggregation layer that reads the worker registry; this package provides
// low-level counters/histograms.
package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	WorkersGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "openmodel_workers_total",
		Help: "Number of registered workers by state",
	}, []string{"state"})

	PollTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "openmodel_polls_total",
		Help: "Total worker poll attempts by result",
	}, []string{"result"})

	PollDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "openmodel_poll_duration_seconds",
		Help:    "Duration of a single worker poll",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	}, []string{"source"})

	StateTransitionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "openmodel_worker_state_transitions_total",
		Help: "Worker state transitions",
	}, []string{"from", "to"})

	WorkerConsecutiveFailures = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "openmodel_worker_consecutive_failures",
		Help: "Consecutive poll failures per worker (0 = healthy)",
	}, []string{"worker_id"})

	AgentInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "openmodel_agent_info",
		Help: "sp-state-agent build info (value always 1)",
	}, []string{"version"})

	// Gateway metrics
	GatewayRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "openmodel_gateway_requests_total",
		Help: "Total gateway proxy requests by HTTP status and worker",
	}, []string{"status", "worker_id"})

	GatewayRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "openmodel_gateway_request_duration_seconds",
		Help:    "Duration of proxied inference requests",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120},
	}, []string{"worker_id"})

	GatewayQueuedRequests = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "openmodel_gateway_queued_requests",
		Help: "Number of requests currently waiting for an available worker",
	})
)

func init() {
	prometheus.MustRegister(
		WorkersGauge,
		PollTotal,
		PollDuration,
		StateTransitionsTotal,
		WorkerConsecutiveFailures,
		AgentInfo,
		GatewayRequestsTotal,
		GatewayRequestDuration,
		GatewayQueuedRequests,
	)
	AgentInfo.WithLabelValues("v0.2.0").Set(1)
}
