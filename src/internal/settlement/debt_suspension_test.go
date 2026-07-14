package settlement

import (
	"math/big"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"openmodel/sp-state-agent/internal/metrics"
)

// TestDebtSuspensionThreshold verifies the D3 suspension policy in BalanceCache:
// disabled by default, suspends at/above the threshold, and a zero threshold
// suspends on any positive debt.
func TestDebtSuspensionThreshold(t *testing.T) {
	bc := NewBalanceCache(nil, nil, nil, 30, discardLogger())

	debts := map[string]*big.Float{
		"walletSmall": big.NewFloat(2),
		"walletBig":   big.NewFloat(20),
	}

	// Disabled (nil threshold): nobody suspended.
	if n := bc.UpdateDebts(debts); n != 0 {
		t.Fatalf("suspension disabled should suspend 0, got %d", n)
	}
	if s, _ := bc.IsSuspended("walletBig"); s {
		t.Fatal("no wallet should be suspended while policy is disabled")
	}

	// Threshold $10: only walletBig ($20) is suspended.
	bc.SetDebtSuspension(big.NewFloat(10))
	if n := bc.UpdateDebts(debts); n != 1 {
		t.Fatalf("threshold $10 should suspend exactly 1 wallet, got %d", n)
	}
	if s, d := bc.IsSuspended("walletBig"); !s || d.Cmp(big.NewFloat(20)) != 0 {
		t.Fatalf("walletBig should be suspended with debt 20, got suspended=%v debt=%v", s, d)
	}
	if s, _ := bc.IsSuspended("walletSmall"); s {
		t.Fatal("walletSmall ($2) is below the $10 threshold and must not be suspended")
	}

	// Zero threshold: any positive debt suspends.
	bc.SetDebtSuspension(big.NewFloat(0))
	if n := bc.UpdateDebts(debts); n != 2 {
		t.Fatalf("zero threshold should suspend all wallets with debt, got %d", n)
	}
}

// TestDebtSuspensionLiftedOnRepayment verifies that when a wallet's debt is cleared
// from the ledger, the next UpdateDebts un-suspends it automatically.
func TestDebtSuspensionLiftedOnRepayment(t *testing.T) {
	bc := NewBalanceCache(nil, nil, nil, 30, discardLogger())
	bc.SetDebtSuspension(big.NewFloat(5))

	bc.UpdateDebts(map[string]*big.Float{"w": big.NewFloat(8)})
	if s, _ := bc.IsSuspended("w"); !s {
		t.Fatal("wallet should be suspended at debt 8 > threshold 5")
	}

	// Debt collected → ledger no longer lists the wallet.
	bc.UpdateDebts(map[string]*big.Float{})
	if s, _ := bc.IsSuspended("w"); s {
		t.Fatal("wallet should be un-suspended once its debt is cleared")
	}
}

// TestSettlerWiresSuspensionFromDebtLedger verifies the end-to-end D3 wiring through
// the settler: persisting a debt ledger over threshold suspends the wallet, and the
// suspended-wallets metric reflects it.
func TestSettlerWiresSuspensionFromDebtLedger(t *testing.T) {
	mock := newMockContract()
	s, _ := newTestSettler(t, mock, discardLogger())
	s.balance.SetDebtSuspension(big.NewFloat(1)) // suspend at $1

	// Persist a debt ledger: walletU owes $3 (over threshold).
	if err := s.saveDebts([]debtRecord{
		{Wallet: walletU, SPEVM: sp1Addr, USD: "3.0"},
	}); err != nil {
		t.Fatal(err)
	}

	if s, _ := s.balance.IsSuspended(walletU); !s {
		t.Fatal("walletU should be suspended after a $3 debt over the $1 threshold")
	}
	if got := testutil.ToFloat64(metrics.SuspendedWallets); got != 1 {
		t.Errorf("suspended-wallets metric: want 1, got %v", got)
	}

	// Collect the debt (empty ledger) → suspension lifted.
	if err := s.saveDebts(nil); err != nil {
		t.Fatal(err)
	}
	if s, _ := s.balance.IsSuspended(walletU); s {
		t.Fatal("walletU should be un-suspended after debt cleared")
	}
}

// TestSuspensionRestoredOnRestart verifies that restorePendingSpend re-applies
// suspension from the on-disk debt ledger, so a restart does not silently un-suspend
// a wallet that still owes.
func TestSuspensionRestoredOnRestart(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	s.balance.SetDebtSuspension(big.NewFloat(1))

	// Write a debt ledger directly to disk (as a prior run would have left it).
	if err := s.saveDebts([]debtRecord{{Wallet: walletU, SPEVM: sp1Addr, USD: "5.0"}}); err != nil {
		t.Fatal(err)
	}
	_ = filepath.Join(dir, "settlement-debt.json")

	// Simulate a fresh process: new BalanceCache, same data dir, re-run restore.
	mock2 := newMockContract()
	s2 := NewSettler(s.cfg, mock2, s.pricer, NewBalanceCache(nil, s.cfg.SupportedTokens, s.pricer, 30, discardLogger()), nil, s.requestLogPath, dir, discardLogger())
	s2.balance.SetDebtSuspension(big.NewFloat(1))
	s2.restorePendingSpend()

	if susp, _ := s2.balance.IsSuspended(walletU); !susp {
		t.Fatal("suspension should be restored from the debt ledger after restart")
	}
}
