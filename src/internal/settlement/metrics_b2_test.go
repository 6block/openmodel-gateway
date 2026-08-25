package settlement

import (
	"context"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"openmodel/sp-state-agent/internal/metrics"
)

// TestMetricsEmittedOnSuccessfulCycle verifies B2: a clean settlement cycle moves
// the confirmed-tx counter, the complete-cycle counter, records the settled item
// count, and clears the WAL gauge. These are the dashboards/alerts the operator
// relies on, so a regression that silently stops emitting them is a real risk.
func TestMetricsEmittedOnSuccessfulCycle(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")

	writeRequestLog(t, reqLog, []RequestRecord{
		billableRecord("m1", "w1", 5),
		billableRecord("m2", "w1", 5),
	})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(10))

	txBefore := testutil.ToFloat64(metrics.SettlementTxTotal.WithLabelValues("confirmed"))
	cycBefore := testutil.ToFloat64(metrics.SettlementCyclesTotal.WithLabelValues("complete"))

	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	if got := testutil.ToFloat64(metrics.SettlementTxTotal.WithLabelValues("confirmed")) - txBefore; got != 1 {
		t.Errorf("confirmed-tx counter: want +1, got +%v", got)
	}
	if got := testutil.ToFloat64(metrics.SettlementCyclesTotal.WithLabelValues("complete")) - cycBefore; got != 1 {
		t.Errorf("complete-cycle counter: want +1, got +%v", got)
	}
	// WAL gauge cleared after a clean finish.
	if got := testutil.ToFloat64(metrics.PendingSettlementWAL); got != 0 {
		t.Errorf("WAL gauge: want 0 after clean cycle, got %v", got)
	}
}

// TestMetricsStuckTxAndPriceStale verifies the failure-path metrics: a stuck
// (timed-out) settlement tx increments the "stuck" outcome and leaves the WAL gauge
// set, and a stale FIL price increments the deferred-price cycle counter + sets the
// stale gauge without submitting anything.
func TestMetricsStuckTxAndPriceStale(t *testing.T) {
	// --- stuck tx ---
	mock := newMockContract()
	mock.receiptErr = ErrTxTimeout
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")
	writeRequestLog(t, reqLog, []RequestRecord{billableRecord("s1", "w1", 5)})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(5))

	stuckBefore := testutil.ToFloat64(metrics.SettlementTxTotal.WithLabelValues("stuck"))
	if err := s.Settle(context.Background()); err == nil {
		t.Fatal("expected Settle to fail on stuck tx")
	}
	if got := testutil.ToFloat64(metrics.SettlementTxTotal.WithLabelValues("stuck")) - stuckBefore; got != 1 {
		t.Errorf("stuck-tx counter: want +1, got +%v", got)
	}
	// WAL must remain (set to 1) so the stuck batch is replayed/RBF'd next cycle.
	if got := testutil.ToFloat64(metrics.PendingSettlementWAL); got != 1 {
		t.Errorf("WAL gauge: want 1 while a stuck settlement is pending, got %v", got)
	}

	// --- stale price defers the cycle ---
	mock2 := newMockContract()
	s2, dir2 := newTestSettler(t, mock2, discardLogger())
	s2.cfg.FILPriceSource = "auto"                 // auto mode can be stale
	s2.pricer = NewPricer(s2.cfg, discardLogger()) // never refreshed → lastUpdateTime zero → stale
	reqLog2 := filepath.Join(dir2, "requests.jsonl")
	writeRequestLog(t, reqLog2, []RequestRecord{billableRecord("p1", "w1", 5)})
	s2.resolver = staticResolver{"w1": "miner1"}

	deferBefore := testutil.ToFloat64(metrics.SettlementCyclesTotal.WithLabelValues("deferred_price"))
	if err := s2.Settle(context.Background()); err != nil {
		t.Fatalf("stale-price cycle should defer cleanly, got error: %v", err)
	}
	if got := testutil.ToFloat64(metrics.SettlementCyclesTotal.WithLabelValues("deferred_price")) - deferBefore; got != 1 {
		t.Errorf("deferred-price counter: want +1, got +%v", got)
	}
	if got := testutil.ToFloat64(metrics.FILPriceStale); got != 1 {
		t.Errorf("FIL-price-stale gauge: want 1, got %v", got)
	}
	if mock2.submitCount != 0 {
		t.Errorf("stale price must not submit anything, got %d submits", mock2.submitCount)
	}
}

// TestPublishFundMetrics verifies the periodic gauge publisher reflects on-disk
// dead-letter and debt state (what a dashboard reads between settlement cycles).
func TestPublishFundMetrics(t *testing.T) {
	mock := newMockContract()
	s, _ := newTestSettler(t, mock, discardLogger())

	// Park two dead-letter records and one debt entry on disk.
	s.writeDeadLetter([]RequestRecord{
		billableRecord("d1", "wX", 3),
		billableRecord("d2", "wX", 4),
	})
	if err := s.saveDebts([]debtRecord{{Wallet: walletU, SPEVM: sp1Addr, USD: "2.5"}}); err != nil {
		t.Fatal(err)
	}
	s.balance.AddPendingSpend(walletU, big.NewFloat(7.25))

	s.PublishFundMetrics()

	if got := testutil.ToFloat64(metrics.DeadLetterEntries); got != 2 {
		t.Errorf("dead-letter gauge: want 2, got %v", got)
	}
	if got := testutil.ToFloat64(metrics.DebtEntries); got != 1 {
		t.Errorf("debt-entries gauge: want 1, got %v", got)
	}
	if got := testutil.ToFloat64(metrics.DebtUSD); got != 2.5 {
		t.Errorf("debt-usd gauge: want 2.5, got %v", got)
	}
	if got := testutil.ToFloat64(metrics.PendingSpendUSD); got != 7.25 {
		t.Errorf("pending-spend gauge: want 7.25, got %v", got)
	}
}
