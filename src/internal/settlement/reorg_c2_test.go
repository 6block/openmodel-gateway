package settlement

import (
	"context"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"openmodel/sp-state-agent/internal/metrics"
)

// TestReorgKeepsBatchUnconfirmedAndReplays verifies C2's core safety property: if a
// settlement tx is reorged away before reaching confirmation depth, the cursor does
// NOT advance and the batch is re-submitted on the next cycle. On-chain dedup makes
// the re-submit safe (no double charge).
func iptr(n int) *int { return &n }

func TestReorgKeepsBatchUnconfirmedAndReplays(t *testing.T) {
	mock := newMockContract()
	mock.finalityErr = ErrReorged     // first finality wait sees the tx reorged away
	mock.submitMarksProcessed = false // a REAL reorg: the effect is gone on-chain too
	s, dir := newTestSettler(t, mock, discardLogger())
	s.cfg.ConfirmationDepth = iptr(5)
	reqLog := filepath.Join(dir, "requests.jsonl")

	writeRequestLog(t, reqLog, []RequestRecord{billableRecord("r1", "w1", 5)})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(5))

	reorgBefore := testutil.ToFloat64(metrics.SettlementTxTotal.WithLabelValues("reorged"))

	// First cycle: tx mines, then is reorged away → Settle returns an error and the
	// cursor must NOT advance.
	if err := s.Settle(context.Background()); err == nil {
		t.Fatal("expected Settle to fail when the tx is reorged before finality")
	}
	if got := testutil.ToFloat64(metrics.SettlementTxTotal.WithLabelValues("reorged")) - reorgBefore; got != 1 {
		t.Errorf("reorged-tx counter: want +1, got +%v", got)
	}
	// WAL must still exist (batch left unconfirmed for replay).
	if !walExists(dir) {
		t.Fatal("WAL must survive a reorg so the batch is replayed")
	}

	// Second cycle: the reorg has cleared (finality succeeds now). The batch replays
	// and settles cleanly; the cursor advances and the WAL is removed.
	mock.finalityErr = nil
	mock.submitMarksProcessed = true // the replay lands durably this time
	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("replay after reorg should succeed: %v", err)
	}
	if walExists(dir) {
		t.Error("WAL must be deleted after the replayed settlement confirms")
	}
	// Dedup: the batch hash was submitted, and re-submission is guarded by
	// processedBatches so the user is charged exactly once.
	if mock.submitCount < 1 {
		t.Errorf("expected the batch to be submitted, got %d submits", mock.submitCount)
	}
}

// TestFinalityFalsePositiveTreatedAsFinal is the fix for the 24h-soak finding: on
// Filecoin the tx-HASH receipt can read NotFound after a tipset shift even though the
// settlement's on-chain EFFECT persisted (processedBatches[hash]==true). WaitForFinality
// surfaces that as ErrReorged, but the settler must consult processedBatches and treat the
// batch as FINAL — not blindly re-submit (which spuriously marked ~90% of cycles "reorged"
// and halved settlement throughput). So: one submit, no "reorged" metric, cursor advances.
func TestFinalityFalsePositiveTreatedAsFinal(t *testing.T) {
	mock := newMockContract()
	mock.finalityErr = ErrReorged    // tx-hash receipt vanished before finality...
	mock.submitMarksProcessed = true // ...but the batch DID land on-chain (the effect persisted)
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")

	writeRequestLog(t, reqLog, []RequestRecord{billableRecord("r1", "w1", 5)})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(5))

	reorgBefore := testutil.ToFloat64(metrics.SettlementTxTotal.WithLabelValues("reorged"))

	// The false-positive must NOT fail the cycle: the batch is processed on-chain → final.
	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("a vanished receipt with the batch processed on-chain must settle cleanly, got: %v", err)
	}
	if got := testutil.ToFloat64(metrics.SettlementTxTotal.WithLabelValues("reorged")) - reorgBefore; got != 0 {
		t.Errorf("false-positive reorg must NOT increment the reorged counter, got +%v", got)
	}
	if mock.submitCount != 1 {
		t.Errorf("must submit exactly once (no spurious re-submit), got %d", mock.submitCount)
	}
	if walExists(dir) {
		t.Error("WAL must be cleared once the batch is treated as final")
	}
}

// TestConfirmationDepthWaited verifies that with a positive confirmation_depth the
// settler calls WaitForFinality (i.e. it does not treat a 1-block receipt as final).
func TestConfirmationDepthWaited(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	s.cfg.ConfirmationDepth = iptr(3)
	reqLog := filepath.Join(dir, "requests.jsonl")
	writeRequestLog(t, reqLog, []RequestRecord{billableRecord("r1", "w1", 5)})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(5))

	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if mock.finalityCalls != 1 {
		t.Errorf("expected exactly 1 finality wait with depth=3, got %d", mock.finalityCalls)
	}
}

// TestZeroConfirmationDepthSkipsFinality verifies that depth=0 keeps the legacy
// behaviour (mined == final, no finality wait), so operators can opt out.
func TestZeroConfirmationDepthSkipsFinality(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	s.cfg.ConfirmationDepth = iptr(0) // explicit opt-out
	reqLog := filepath.Join(dir, "requests.jsonl")
	writeRequestLog(t, reqLog, []RequestRecord{billableRecord("r1", "w1", 5)})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(5))

	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if mock.finalityCalls != 0 {
		t.Errorf("depth=0 must not wait for finality, but WaitForFinality was called %d times", mock.finalityCalls)
	}
}
