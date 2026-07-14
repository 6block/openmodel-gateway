package settlement

import (
	"context"
	"math/big"
	"path/filepath"
	"testing"
)

// Medium-severity audit-finding regression: the contract SKIPS (not reverts) items whose user balance was
// drained between plan and execution. The settler must decode the receipt's outcome
// events and reverse each skipped item — otherwise the revenue is silently counted as
// settled while the chain never moved it.

// One item fails on-chain → its USD must NOT reduce pendingSpend, must NOT enter
// settled-total, and must be carried as debt; the next (clean) cycle collects it.
func TestOnChainItemFailureReversedToDebtAndRecollected(t *testing.T) {
	mock := newMockContract()
	mock.outcome = &SettlementOutcome{Found: true, SettledCount: 0, FailedCount: 1, FailedIndexes: []uint64{0}}
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")

	writeRequestLog(t, reqLog, []RequestRecord{billableRecord("r1", "w1", 5)}) // $5 at $1/token
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(5))

	// Cycle 1: tx confirms, but the contract reports the item SKIPPED.
	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if ps := s.balance.GetPendingSpend(walletU); ps.Cmp(big.NewFloat(5)) != 0 {
		t.Errorf("failed item must keep its $5 reserved (chain moved nothing), pending=%s", ps.Text('f', 6))
	}
	if st := s.SettledUSDTotal(); st.Sign() != 0 {
		t.Errorf("failed item must not enter settled-total, got %s", st.Text('f', 6))
	}
	if d := s.DebtUSDTotal(); d.Cmp(big.NewFloat(5)) != 0 {
		t.Errorf("failed item must be carried as $5 debt, got %s", d.Text('f', 6))
	}

	// Cycle 2: outcome now clean → the carried debt is collected and the books close.
	mock.outcome = nil
	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("settle 2: %v", err)
	}
	if ps := s.balance.GetPendingSpend(walletU); ps.Sign() != 0 {
		t.Errorf("after debt collection pending must be zero, got %s", ps.Text('f', 6))
	}
	if d := s.DebtUSDTotal(); d.Sign() != 0 {
		t.Errorf("debt must be cleared after collection, got %s", d.Text('f', 6))
	}
	if st := s.SettledUSDTotal(); st.Cmp(big.NewFloat(5)) != 0 {
		t.Errorf("settled-total must now hold the collected $5, got %s", st.Text('f', 6))
	}
}

// A clean outcome (the overwhelmingly common case) must keep the fast path intact.
func TestOnChainOutcomeCleanKeepsBooks(t *testing.T) {
	mock := newMockContract() // default outcome: Found, zero failures
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")

	writeRequestLog(t, reqLog, []RequestRecord{billableRecord("r1", "w1", 5)})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(5))

	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if ps := s.balance.GetPendingSpend(walletU); ps.Sign() != 0 {
		t.Errorf("clean settle must clear pending, got %s", ps.Text('f', 6))
	}
	if d := s.DebtUSDTotal(); d.Sign() != 0 {
		t.Errorf("clean settle must carry no debt, got %s", d.Text('f', 6))
	}
}
