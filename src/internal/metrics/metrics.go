// Package metrics holds Prometheus metrics that can be incremented from any
// package without creating import cycles. The health package provides the
// aggregation layer that reads the worker registry; this package provides
// low-level counters/histograms.
package metrics

import "github.com/prometheus/client_golang/prometheus"

// Version labels the openmodel_agent_info metric. Overridden at build time:
//   go build -ldflags "-X openmodel/sp-state-agent/internal/metrics.Version=v2.1.0"
var Version = "dev"

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

	// --- Settlement / fund-safety metrics (M3 production hardening, B2) ---

	// SettlementTxTotal counts on-chain settlement submissions by outcome:
	// "confirmed" (mined OK), "reverted" (mined but status=failed),
	// "stuck" (timed out unmined → RBF replace), "error" (submit/RPC error).
	SettlementTxTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "openmodel_settlement_tx_total",
		Help: "Settlement transactions by outcome (confirmed/reverted/stuck/error)",
	}, []string{"result"})

	// SettlementCyclesTotal counts settlement cycles by outcome
	// ("complete", "deferred_price", "failed", "panic").
	SettlementCyclesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "openmodel_settlement_cycles_total",
		Help: "Settlement cycles by outcome",
	}, []string{"result"})

	// SettlementCycleDuration is the wall-clock duration of a full Settle() cycle.
	SettlementCycleDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "openmodel_settlement_cycle_duration_seconds",
		Help:    "Duration of a full settlement cycle",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
	})

	// SettlementItemsTotal counts per-item settlement outcomes
	// ("settled", "insufficient", "unresolved", "deadlettered").
	SettlementItemsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "openmodel_settlement_items_total",
		Help: "Settlement line items by outcome",
	}, []string{"result"})

	// DeadLetterEntries is the current number of records parked in the
	// settlement dead-letter queue (worker→SP unresolved). A non-zero, growing
	// value means SP payout mapping is missing — page on it.
	DeadLetterEntries = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "openmodel_settlement_deadletter_entries",
		Help: "Records currently parked in the settlement dead-letter queue",
	})

	// RequestLogWriteErrors counts failures to persist a billing/request record
	// (disk full, IO error, marshal). Each increment means a served request was NOT
	// recorded for billing (revenue loss) — alert on any positive rate.
	RequestLogWriteErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "openmodel_request_log_write_errors_total",
		Help: "Failures to persist a request/billing record (dropped from billing)",
	})

	// DebtEntries / DebtUSD track carried under-funded debt (the user consumed
	// service its balance could not cover; collected on the next top-up).
	DebtEntries = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "openmodel_settlement_debt_entries",
		Help: "Number of (wallet,SP) carried-debt entries outstanding",
	})
	DebtUSD = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "openmodel_settlement_debt_usd",
		Help: "Total carried under-funded debt outstanding, in USD",
	})

	// PendingSettlementWAL is 1 while an uncommitted settlement WAL exists on
	// disk (a cycle was interrupted mid-submit and will be replayed), else 0.
	PendingSettlementWAL = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "openmodel_settlement_pending_wal",
		Help: "1 if an uncommitted settlement write-ahead-log is present, else 0",
	})

	// PendingSpendUSD is the total reserved-but-unsettled spend across all
	// wallets (the balance-gate accumulator). Diverging far from the request-log
	// total is a billing-correctness alarm.
	PendingSpendUSD = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "openmodel_settlement_pending_spend_usd",
		Help: "Total reserved-but-unsettled spend across all wallets, in USD",
	})

	// OperatorBalanceFIL is the settlement operator's on-chain FIL balance (gas).
	// Alert when it approaches operator_min_balance_fil — a drained operator
	// halts all settlement.
	OperatorBalanceFIL = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "openmodel_settlement_operator_balance_fil",
		Help: "Settlement operator on-chain FIL (gas) balance",
	})

	// OperatorBalanceLow is 1 while the operator gas balance is below the
	// configured minimum, else 0 — a simple alert target.
	OperatorBalanceLow = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "openmodel_settlement_operator_balance_low",
		Help: "1 if operator gas balance is below operator_min_balance_fil, else 0",
	})

	// FILPriceUSD is the FIL price currently used for billing/conversion.
	FILPriceUSD = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "openmodel_settlement_fil_price_usd",
		Help: "FIL price in USD currently used for billing",
	})

	// FILPriceStale is 1 while the FIL price is stale (all sources failing in
	// auto mode → settlement is deferred), else 0.
	FILPriceStale = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "openmodel_settlement_fil_price_stale",
		Help: "1 if FIL price is stale (settlement deferred), else 0",
	})

	// --- Reconciliation metrics (B4) ---

	// ReconcileRunsTotal counts reconciliation runs by result ("ok", "drift", "error").
	ReconcileRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "openmodel_reconcile_runs_total",
		Help: "Three-way reconciliation runs by result",
	}, []string{"result"})

	// ReconcileDriftUSD is the absolute USD discrepancy found by the most recent
	// reconciliation between billed (request log) and settled (on-chain) totals.
	ReconcileDriftUSD = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "openmodel_reconcile_drift_usd",
		Help: "Absolute USD drift between billed and settled totals at last reconcile",
	})

	// ReconcileLastUnixTime is the unix timestamp of the last successful reconcile.
	ReconcileLastUnixTime = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "openmodel_reconcile_last_timestamp_seconds",
		Help: "Unix time of the last completed reconciliation",
	})

	// --- Rate limiting / abuse metrics (B5) ---

	// RateLimitRejectedTotal counts requests rejected by the gateway's abuse
	// controls, by reason ("rate", "concurrency", "body_too_large").
	RateLimitRejectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "openmodel_gateway_ratelimit_rejected_total",
		Help: "Requests rejected by per-key rate/concurrency/body-size limits",
	}, []string{"reason"})

	// --- Graceful shutdown metrics (B7) ---

	// GatewayDraining is 1 while the gateway is draining in-flight requests
	// during graceful shutdown (rejecting new requests with 503), else 0.
	GatewayDraining = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "openmodel_gateway_draining",
		Help: "1 while the gateway is draining for graceful shutdown, else 0",
	})

	// --- Service-suspension metrics (D3) ---

	// SuspendedWallets is the number of wallets currently suspended for unpaid
	// debt (served 402 until they top up and the debt is collected).
	SuspendedWallets = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "openmodel_settlement_suspended_wallets",
		Help: "Wallets currently suspended for outstanding debt",
	})

	// --- Stream resume metrics (B2) ---

	// GatewayStreamResumes counts mid-stream continuation events by result:
	// "attempt" (a continuation was dispatched), "completed" (a resumed stream
	// finished cleanly), "gave_up" (interruption could not be resumed — no capable
	// worker / budget exhausted → degraded to the legacy error-event behavior).
	GatewayStreamResumes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "openmodel_gateway_stream_resumes_total",
		Help: "Mid-stream continuations by result (attempt/completed/gave_up)",
	}, []string{"result"})

	// --- Stablecoin depeg protection (C3) ---

	// StablecoinPriceUSD is the monitored stablecoin's fetched USD price (labeled by
	// symbol, e.g. USDC). Pinned at 1.0 when no price source is configured.
	StablecoinPriceUSD = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "openmodel_settlement_stablecoin_price_usd",
		Help: "Monitored stablecoin USD price used for billing/collection",
	}, []string{"symbol"})

	// StablecoinDepegged is 1 while a stablecoin's price is outside its peg band
	// (settlement stops accepting it and it stops counting toward spendable credit),
	// else 0. Alert on this.
	StablecoinDepegged = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "openmodel_settlement_stablecoin_depegged",
		Help: "1 while a stablecoin is depegged (excluded from settlement + credit), else 0",
	}, []string{"symbol"})

	// --- Signed inference receipts (A1) ---

	// ReceiptsTotal counts per-request receipt outcomes: "verified" (signature +
	// request hash + usage all check out), "invalid" (byzantine evidence — alert),
	// "missing" (worker presented none — old worker or feature off).
	ReceiptsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "openmodel_receipts_total",
		Help: "Signed inference receipts by verification outcome",
	}, []string{"result"})

	// --- SP verification / fraud-detection sampling metrics (A7 phase 1) ---

	// VerificationSamplesRetained counts request/response pairs retained for offline
	// SP fraud detection (model substitution / token misreporting).
	VerificationSamplesRetained = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "openmodel_verification_samples_retained_total",
		Help: "Request/response pairs retained for offline SP fraud detection",
	})

	// VerificationSampleWriteErrors counts failures to persist a verification sample
	// (marshal/IO). A retained sample was lost — informational, not a billing error.
	VerificationSampleWriteErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "openmodel_verification_sample_write_errors_total",
		Help: "Failures to persist a verification sample",
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
		// Settlement / fund-safety (B2)
		SettlementTxTotal,
		SettlementCyclesTotal,
		SettlementCycleDuration,
		SettlementItemsTotal,
		DeadLetterEntries,
		RequestLogWriteErrors,
		DebtEntries,
		DebtUSD,
		PendingSettlementWAL,
		PendingSpendUSD,
		OperatorBalanceFIL,
		OperatorBalanceLow,
		FILPriceUSD,
		FILPriceStale,
		// Reconciliation (B4)
		ReconcileRunsTotal,
		ReconcileDriftUSD,
		ReconcileLastUnixTime,
		// Rate limiting (B5)
		RateLimitRejectedTotal,
		// Graceful shutdown (B7)
		GatewayDraining,
		// Service suspension (D3)
		SuspendedWallets,
		// Stream resume (B2)
		GatewayStreamResumes,
		// Stablecoin depeg protection (C3)
		StablecoinPriceUSD,
		StablecoinDepegged,
		// Signed receipts (A1)
		ReceiptsTotal,
		// SP verification sampling (A7 phase 1)
		VerificationSamplesRetained,
		VerificationSampleWriteErrors,
	)
	AgentInfo.WithLabelValues(Version).Set(1)
}
