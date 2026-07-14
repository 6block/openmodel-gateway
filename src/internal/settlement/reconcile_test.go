package settlement

import (
	"time"
	"context"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"openmodel/sp-state-agent/internal/metrics"
)

// mutableSettled implements settledTotaler with settable totals so a test can model the
// state CHANGING between reconcile passes (F3 incremental model).
type mutableSettled struct {
	settled *big.Float
	debt    *big.Float
}

func (s *mutableSettled) SettledUSDTotal() *big.Float {
	if s.settled == nil {
		return new(big.Float)
	}
	return s.settled
}
func (s *mutableSettled) DebtUSDTotal() *big.Float {
	if s.debt == nil {
		return new(big.Float)
	}
	return s.debt
}

// f3Harness builds a reconciler whose settled/pending/debt can change between Runs, so
// tests exercise the rotation-immune incremental model: Run #1 baselines + skips any
// pre-existing log; later Runs count only newly-appended records and compare DELTAS.
type f3Harness struct {
	t      *testing.T
	rc     *Reconciler
	reqLog string
	bc     *BalanceCache
	st     *mutableSettled
}

func newF3(t *testing.T) *f3Harness {
	t.Helper()
	dir := t.TempDir()
	reqLog := filepath.Join(dir, "requests.jsonl")
	prices := map[string]*big.Float{"default": big.NewFloat(1)} // $1/token
	bc := NewBalanceCache(nil, nil, NewPricer(coverageCfg(), discardLogger()), 30, discardLogger())
	st := &mutableSettled{settled: new(big.Float), debt: new(big.Float)}
	rc := NewReconciler(reqLog, filepath.Join(dir, "settlement-deadletter.jsonl"),
		dir, flatCostFn(prices), bc, st, big.NewFloat(0.01), discardLogger())
	return &f3Harness{t: t, rc: rc, reqLog: reqLog, bc: bc, st: st}
}

func (h *f3Harness) append(recs ...RequestRecord) {
	writeRequestLog(h.t, h.reqLog, recs) // O_APPEND
}

// writeFileTrunc empties the file in place (simulates the live request log being rotated
// away / pruned — the Scanner detects the shrink and continues without the old bytes).
func writeFileTrunc(path string) error {
	return os.WriteFile(path, []byte{}, 0644)
}
func (h *f3Harness) run() ReconcileReport {
	rep, err := h.rc.Run(context.Background())
	if err != nil {
		h.t.Fatalf("Run: %v", err)
	}
	return rep
}

// TestReconcileFirstPassWaitsForReadySignal is the round-3 regression: after a gateway
// restart the reconciler's immediate first pass raced the settler's pendingSpend restore
// (pending read 0 → the pre-restart pending surfaced as a false DRIFT). With a ready
// signal wired, Start must NOT run its first pass until the signal closes.
func TestReconcileFirstPassWaitsForReadySignal(t *testing.T) {
	h := newF3(t)
	ready := make(chan struct{})
	h.rc.SetReadySignal(ready)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.rc.Start(ctx, time.Hour)

	statePath := h.rc.statePath
	time.Sleep(250 * time.Millisecond)
	if _, err := os.Stat(statePath); err == nil {
		t.Fatal("first pass ran BEFORE the ready signal (race regression)")
	}
	close(ready)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(statePath); err == nil {
			return // first pass ran after the signal ✓
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("first pass never ran after the ready signal closed")
}

// TestReconcileBalancedIncremental: after a baseline pass, new billed usage that is fully
// settled + pending stays within tolerance — and the pre-existing settled at baseline
// does NOT show as drift (the soak F3 bug).
func TestReconcileBalancedIncremental(t *testing.T) {
	h := newF3(t)
	// Pre-existing state at deploy: $100 already settled on-chain from before + one old
	// log record. Run #1 must baseline these away (drift 0), NOT flag $100 as lost.
	h.st.settled = big.NewFloat(100)
	h.append(billableRecord("old", "w1", 10))
	base := h.run()
	if !base.WithinTolerance || base.BilledUSD != "0.000000" {
		t.Fatalf("baseline run must be drift-free and count nothing: billed=%s drift=%s", base.BilledUSD, base.DriftUSD)
	}

	// Now $30 of NEW billable usage: $20 settled (settled 100→120), $10 pending.
	h.append(billableRecord("a", "w1", 10), billableRecord("b", "w1", 10), billableRecord("c", "w1", 10))
	h.st.settled = big.NewFloat(120)
	h.bc.AddPendingSpend(walletU, big.NewFloat(10))

	okBefore := testutil.ToFloat64(metrics.ReconcileRunsTotal.WithLabelValues("ok"))
	rep := h.run()
	if !rep.WithinTolerance {
		t.Fatalf("balanced increment must be within tolerance: billed=%s settled=%s pending=%s drift=%s",
			rep.BilledUSD, rep.SettledUSD, rep.PendingUSD, rep.DriftUSD)
	}
	if rep.BilledUSD != "30.000000" || rep.BillableCount != 3 {
		t.Errorf("billed delta: want $30/3, got %s/%d", rep.BilledUSD, rep.BillableCount)
	}
	if got := testutil.ToFloat64(metrics.ReconcileRunsTotal.WithLabelValues("ok")) - okBefore; got != 1 {
		t.Errorf("ok counter: want +1, got +%v", got)
	}
}

// TestReconcileDriftDetectedIncremental: a real shortfall (billed increment not covered
// by settled+pending increment) is still flagged.
func TestReconcileDriftDetectedIncremental(t *testing.T) {
	h := newF3(t)
	h.run() // baseline (empty)
	h.append(billableRecord("a", "w1", 10), billableRecord("b", "w1", 10)) // $20 new
	h.st.settled = big.NewFloat(5)                                         // only $5 settled, nothing pending

	driftBefore := testutil.ToFloat64(metrics.ReconcileRunsTotal.WithLabelValues("drift"))
	rep := h.run()
	if rep.WithinTolerance {
		t.Fatal("a $15 shortfall must be flagged as drift")
	}
	if rep.DriftAbsUSD != "15.000000" {
		t.Errorf("drift: want 15.000000, got %s", rep.DriftAbsUSD)
	}
	if got := testutil.ToFloat64(metrics.ReconcileRunsTotal.WithLabelValues("drift")) - driftBefore; got != 1 {
		t.Errorf("drift counter: want +1, got +%v", got)
	}
}

// TestReconcileRotationImmune: the incremental billed total survives request-log rotation
// (old backups deleted). The old full-rescan reconciler under-counted here → false drift.
func TestReconcileRotationImmune(t *testing.T) {
	h := newF3(t)
	h.run() // baseline empty

	// $50 of usage across two passes; between them, ROTATE the log away (delete it) to
	// simulate retention pruning. Incremental billedCum must retain the earlier $ despite
	// the file being gone.
	h.append(billableRecord("a", "w1", 10), billableRecord("b", "w1", 10), billableRecord("c", "w1", 10)) // $30
	h.st.settled = big.NewFloat(30)
	if r1 := h.run(); r1.BilledUSD != "30.000000" {
		t.Fatalf("pass1 billed want 30, got %s", r1.BilledUSD)
	}
	// Simulate rotation: the live log is truncated/replaced (old records gone from disk).
	if err := writeFileTrunc(h.reqLog); err != nil {
		t.Fatal(err)
	}
	h.append(billableRecord("d", "w1", 10), billableRecord("e", "w1", 10)) // $20 more
	h.st.settled = big.NewFloat(50)
	r2 := h.run()
	if r2.BilledUSD != "50.000000" {
		t.Errorf("cumulative billed must survive rotation: want 50, got %s", r2.BilledUSD)
	}
	if !r2.WithinTolerance {
		t.Errorf("balanced after rotation must be drift-free: drift=%s", r2.DriftUSD)
	}
}

// TestReconcileStatePersistsAcrossRestart: a fresh Reconciler over the same dataDir
// resumes the cumulative total + baseline (survives the gateway OOM-restarts of F4).
func TestReconcileStatePersistsAcrossRestart(t *testing.T) {
	h := newF3(t)
	h.run() // baseline
	h.append(billableRecord("a", "w1", 10))
	h.st.settled = big.NewFloat(10)
	h.run() // billedCum = 10

	// "Restart": new Reconciler, same files.
	dir := filepath.Dir(h.reqLog)
	prices := map[string]*big.Float{"default": big.NewFloat(1)}
	rc2 := NewReconciler(h.reqLog, filepath.Join(dir, "settlement-deadletter.jsonl"), dir,
		flatCostFn(prices), h.bc, h.st, big.NewFloat(0.01), discardLogger())
	h.append(billableRecord("b", "w1", 10))
	h.st.settled = big.NewFloat(20)
	rep, err := rc2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.BilledUSD != "20.000000" {
		t.Errorf("cumulative billed must persist across restart: want 20, got %s", rep.BilledUSD)
	}
	if !rep.WithinTolerance {
		t.Errorf("balanced across restart: drift=%s", rep.DriftUSD)
	}
}
