package settlement

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"time"

	"openmodel/sp-state-agent/internal/metrics"
)

// reconcile.go implements the automated three-way billing reconciliation (B4).
//
// It cross-checks three independently-derived totals and alerts on drift:
//
//   billed   = Σ over the request log of billable records × model price (USD)
//   settled  = Σ pendingSpend reductions confirmed on-chain (USD), read from the
//              local settlement audit log's running total
//   pending  = current reserved-but-unsettled spend + carried debt (USD)
//
// The fund-correctness invariant is:
//
//   billed  ==  settled + pending          (within a small epsilon)
//
// i.e. every dollar of billable usage is either already settled on-chain or still
// accounted for as pending/debt. A drift beyond epsilon means revenue was lost
// (under-settled) or double-counted (over-settled) and must be investigated — it is
// the end-to-end check that the whole scan → aggregate → submit → confirm pipeline
// is conserving money. The previous process ran this by hand; B4 makes it periodic
// with a metric + alert and an on-demand admin endpoint.

// ReconcileReport is the result of one reconciliation pass. All monetary fields are
// USD decimal strings so the JSON is exact (no float rounding in transport).
type ReconcileReport struct {
	Timestamp       time.Time `json:"timestamp"`
	BilledUSD       string    `json:"billed_usd"`    // Σ billable request-log records × price
	SettledUSD      string    `json:"settled_usd"`   // Σ on-chain settled (from audit log)
	PendingUSD      string    `json:"pending_usd"`   // current reserved-but-unsettled spend
	DebtUSD         string    `json:"debt_usd"`      // carried under-funded debt
	DriftUSD        string    `json:"drift_usd"`     // billed - (settled + pending + debt); ~0 is healthy
	DriftAbsUSD     string    `json:"drift_abs_usd"` // |drift|, the alert quantity
	WithinTolerance bool      `json:"within_tolerance"`
	ToleranceUSD    string    `json:"tolerance_usd"`
	BillableCount   int       `json:"billable_count"` // billable records counted
	DeadLetters     int       `json:"dead_letters"`   // records still unresolved (not yet billed/settled)
}

// settledTotaler exposes the running on-chain-settled USD total and the outstanding
// carried-debt USD total. The Settler implements both from its audit log and debt
// ledger; declaring an interface keeps the Reconciler unit-testable with a stub.
type settledTotaler interface {
	SettledUSDTotal() *big.Float
	DebtUSDTotal() *big.Float
}

// Reconciler runs the three-way check. It reuses the Scanner's billable definition
// and the SAME per-record pricing path settlement uses (costFn, the aggregator's
// RecordCostUSD) for the billed side, the Settler's audit total for the settled side,
// and the BalanceCache for the pending/debt side. Pricing the billed side with any
// OTHER formula (e.g. the old flat total×output) silently re-defines "pending" as
// "whatever makes the equation balance" and blinds the drift alert to real leaks.
type Reconciler struct {
	requestLogPath string
	deadLetterPath string
	costFn         func(RequestRecord) *big.Float
	balance        *BalanceCache
	settled        settledTotaler
	toleranceUSD   *big.Float
	logger         *slog.Logger

	// F3 (soak v2): rotation-immune reconciliation. Instead of re-summing the retained
	// request log every pass (which under-counts once rotation deletes old backups, and
	// can't see settled amounts predating the current log window → false growing drift),
	// the reconciler owns a cursor and a PERSISTED cumulative billed total that only ever
	// advances over NEW records — never re-reading (or needing) deleted history. A
	// first-run baseline of settled/pending/debt is subtracted so pre-existing on-chain
	// settlement (from before this reconciler started) is not mistaken for drift.
	scanner   *Scanner
	statePath string
	st        reconcileState

	// ready (optional, see SetReadySignal) gates the FIRST periodic pass until the
	// settler has restored pendingSpend after a restart — running earlier reads
	// pending=0 and reports the pre-restart pending as a false DRIFT alarm (round-3
	// soak finding, gateway_drain_restart at 2026-07-09T00:23Z).
	ready <-chan struct{}
}

// SetReadySignal wires the settler's pendingSpend-restore signal into the reconciler.
// Optional: nil (tests, settlement-less setups) keeps the old immediate first pass.
func (rc *Reconciler) SetReadySignal(ch <-chan struct{}) { rc.ready = ch }

// reconcileState is the persisted rotation-immune state (atomic tmp+rename).
type reconcileState struct {
	Initialized     bool   `json:"initialized"`
	BilledCumUSD    string `json:"billed_cum_usd"`      // Σ cost of every billable record ever seen
	BillableCount   int    `json:"billable_count"`      // cumulative billable records
	SettledBaseline string `json:"settled_baseline_usd"` // settled/pending/debt at first run
	PendingBaseline string `json:"pending_baseline_usd"`
	DebtBaseline    string `json:"debt_baseline_usd"`
}

func NewReconciler(
	requestLogPath, deadLetterPath, dataDir string,
	costFn func(RequestRecord) *big.Float,
	balance *BalanceCache,
	settled settledTotaler,
	toleranceUSD *big.Float,
	logger *slog.Logger,
) *Reconciler {
	if toleranceUSD == nil {
		toleranceUSD = big.NewFloat(0.01) // 1 cent default
	}
	rc := &Reconciler{
		requestLogPath: requestLogPath,
		deadLetterPath: deadLetterPath,
		costFn:         costFn,
		balance:        balance,
		settled:        settled,
		toleranceUSD:   toleranceUSD,
		logger:         logger,
		scanner:        NewScanner(requestLogPath, dataDir+"/reconcile-cursor.json", logger),
		statePath:      dataDir + "/reconcile-state.json",
	}
	rc.loadState()
	return rc
}

func (rc *Reconciler) loadState() {
	data, err := os.ReadFile(rc.statePath)
	if err != nil {
		return // first run
	}
	if err := json.Unmarshal(data, &rc.st); err != nil {
		rc.logger.Warn("failed to parse reconcile state, re-baselining", "error", err)
		rc.st = reconcileState{}
	}
}

func (rc *Reconciler) saveState() {
	data, err := json.Marshal(rc.st)
	if err != nil {
		return
	}
	tmp := rc.statePath + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, rc.statePath)
	}
}

func parseBigOrZero(s string) *big.Float {
	if s == "" {
		return new(big.Float)
	}
	f, _, err := big.ParseFloat(s, 10, 128, big.ToNearestEven)
	if err != nil {
		return new(big.Float)
	}
	return f
}

// Run performs one reconciliation pass and updates the reconcile metrics. It never
// returns an error for a drift (drift is a data finding, surfaced in the report and
// metric); it only errors if the inputs cannot be read at all.
func (rc *Reconciler) Run(_ context.Context) (ReconcileReport, error) {
	// 1. Advance the cumulative billed total over NEW billable records only (rotation-
	//    immune: never re-reads deleted history). Peek returns records past our cursor;
	//    Scanner already handles rotation of the live file safely.
	records, _, newCur, err := rc.scanner.Peek()
	if err != nil {
		metrics.ReconcileRunsTotal.WithLabelValues("error").Inc()
		return ReconcileReport{}, fmt.Errorf("scan billed delta: %w", err)
	}
	settled := new(big.Float)
	debt := new(big.Float)
	if rc.settled != nil {
		settled = rc.settled.SettledUSDTotal()
		debt = rc.settled.DebtUSDTotal()
	}
	pending := new(big.Float)
	if rc.balance != nil {
		pending = rc.balance.TotalPendingSpendUSD()
	}

	billedCum := parseBigOrZero(rc.st.BilledCumUSD)
	if !rc.st.Initialized {
		// 2. First run: snapshot settled/pending/debt as the baseline and SKIP the records
		//    already in the log (they predate us and are already reflected in that baseline
		//    — settled, or pending via WAL restore). Counting them would double them. From
		//    here billedCum only ever advances over NEW records, so pre-existing on-chain
		//    settlement and deleted-by-rotation history never masquerade as drift.
		rc.st.SettledBaseline = settled.Text('f', 12)
		rc.st.PendingBaseline = pending.Text('f', 12)
		rc.st.DebtBaseline = debt.Text('f', 12)
		rc.st.Initialized = true
	} else {
		for _, rec := range records {
			if cost := rc.costFn(rec); cost != nil && cost.Sign() > 0 {
				billedCum.Add(billedCum, cost)
				rc.st.BillableCount++
			}
		}
		rc.st.BilledCumUSD = billedCum.Text('f', 12)
	}
	// Commit cursor first, then persist the total: a crash in-between at worst re-adds one
	// window of records next run (a small visible over-count an operator can re-baseline
	// by deleting reconcile-state.json), never silently loses money accounting.
	_ = rc.scanner.CommitCursor(newCur)
	rc.saveState()

	// 3. Invariant over DELTAS since baseline: every dollar billed since we started is
	//    either settled-since, still-pending-vs-baseline, or debt-since.
	//        billedCum == (settled-base) + (pending-base) + (debt-base)
	settledDelta := new(big.Float).Sub(settled, parseBigOrZero(rc.st.SettledBaseline))
	pendingDelta := new(big.Float).Sub(pending, parseBigOrZero(rc.st.PendingBaseline))
	debtDelta := new(big.Float).Sub(debt, parseBigOrZero(rc.st.DebtBaseline))
	accounted := new(big.Float).Add(settledDelta, pendingDelta)
	accounted.Add(accounted, debtDelta)
	drift := new(big.Float).Sub(billedCum, accounted)
	driftAbs := new(big.Float).Abs(drift)
	within := driftAbs.Cmp(rc.toleranceUSD) <= 0

	report := ReconcileReport{
		Timestamp:       time.Now(),
		BilledUSD:       billedCum.Text('f', 6),
		SettledUSD:      settledDelta.Text('f', 6),
		PendingUSD:      pendingDelta.Text('f', 6),
		DebtUSD:         debtDelta.Text('f', 6),
		DriftUSD:        drift.Text('f', 6),
		DriftAbsUSD:     driftAbs.Text('f', 6),
		WithinTolerance: within,
		ToleranceUSD:    rc.toleranceUSD.Text('f', 6),
		BillableCount:   rc.st.BillableCount,
		DeadLetters:     rc.deadLetterCount(),
	}

	driftF, _ := driftAbs.Float64()
	metrics.ReconcileDriftUSD.Set(driftF)
	metrics.ReconcileLastUnixTime.Set(float64(time.Now().Unix()))
	if within {
		metrics.ReconcileRunsTotal.WithLabelValues("ok").Inc()
		rc.logger.Info("reconciliation ok",
			"billed_usd", report.BilledUSD, "settled_usd", report.SettledUSD,
			"pending_usd", report.PendingUSD, "debt_usd", report.DebtUSD,
			"drift_usd", report.DriftUSD)
	} else {
		metrics.ReconcileRunsTotal.WithLabelValues("drift").Inc()
		rc.logger.Error("reconciliation DRIFT detected — investigate billing correctness",
			"billed_usd", report.BilledUSD, "settled_usd", report.SettledUSD,
			"pending_usd", report.PendingUSD, "debt_usd", report.DebtUSD,
			"drift_usd", report.DriftUSD, "tolerance_usd", report.ToleranceUSD)
	}
	return report, nil
}

// Start runs reconciliation periodically until ctx is cancelled.
func (rc *Reconciler) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	rc.logger.Info("reconciliation loop started", "interval", interval.String())
	// Gate the first pass on the settler's pendingSpend restore (WAL resume can spend
	// minutes waiting on chain confirmations). Capped so a wedged settler cannot
	// silence reconciliation forever.
	if rc.ready != nil {
		select {
		case <-rc.ready:
		case <-time.After(5 * time.Minute):
			rc.logger.Warn("pendingSpend-restore signal not seen within 5m; running first reconcile anyway")
		case <-ctx.Done():
			return
		}
	}
	// One pass shortly after start so a fresh deploy gets a baseline.
	_, _ = rc.Run(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := rc.Run(ctx); err != nil {
				rc.logger.Warn("reconciliation pass failed", "error", err)
			}
		}
	}
}

func (rc *Reconciler) deadLetterCount() int {
	f, err := os.Open(rc.deadLetterPath)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			n++
		}
	}
	return n
}
