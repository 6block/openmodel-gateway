package settlement

import (
	"io"
	"log/slog"
	"math/big"
	"testing"
)

func testBalanceCache(t *testing.T) *BalanceCache {
	t.Helper()
	cfg := &Config{
		FILPriceUSD:    "2.0",
		FILPriceSource: "manual",
		SupportedTokens: []TokenConfig{
			{Symbol: "USDC", Address: usdcAddr, Decimals: 6},
			{Symbol: "FIL", Address: filAddr, Decimals: 18},
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pricer := NewPricer(cfg, logger)
	bc := NewBalanceCache(nil, cfg.SupportedTokens, pricer, 30, logger)
	// Seed chain balances directly (white-box) — $5 USDC + 10 FIL ($20).
	bc.chainBalances[walletU] = map[string]*big.Int{
		usdcAddr: usdc(5),
		filAddr:  fil(10),
	}
	return bc
}

func TestReserveRespectsAvailableBalance(t *testing.T) {
	bc := testBalanceCache(t)

	// Total available = $5 USDC + $20 FIL = $25.
	if !bc.HasSufficientBalance(walletU, big.NewFloat(20)) {
		t.Error("should have sufficient balance for $20")
	}
	if bc.HasSufficientBalance(walletU, big.NewFloat(26)) {
		t.Error("should NOT have sufficient balance for $26")
	}

	// Reserve $20 → available drops to $5.
	if !bc.Reserve(walletU, big.NewFloat(20)) {
		t.Fatal("first reserve of $20 should succeed")
	}
	if bc.Reserve(walletU, big.NewFloat(10)) {
		t.Error("second reserve of $10 should fail (only $5 left)")
	}
	if !bc.Reserve(walletU, big.NewFloat(5)) {
		t.Error("reserve of remaining $5 should succeed")
	}
}

func TestAdjustReversesOverestimate(t *testing.T) {
	bc := testBalanceCache(t)

	est := big.NewFloat(10)
	if !bc.Reserve(walletU, est) {
		t.Fatal("reserve failed")
	}
	// Actual cost was only $3 → pendingSpend should drop from $10 to $3.
	bc.Adjust(walletU, est, big.NewFloat(3))

	ps := bc.GetPendingSpend(walletU)
	if ps.Cmp(big.NewFloat(3)) != 0 {
		t.Errorf("expected pendingSpend $3 after adjust, got %s", ps.Text('f', 6))
	}
}

func TestAdjustFullReversalOnFailure(t *testing.T) {
	bc := testBalanceCache(t)
	est := big.NewFloat(10)
	bc.Reserve(walletU, est)
	// Failed request → actual cost 0 → full reversal.
	bc.Adjust(walletU, est, new(big.Float))

	ps := bc.GetPendingSpend(walletU)
	if ps.Sign() != 0 {
		t.Errorf("expected pendingSpend 0 after failed-request reversal, got %s", ps.Text('f', 6))
	}
}

// TestSettleSpendPreservesInflight is the regression test for bug H1: settling
// must subtract only the settled amount, preserving in-flight reservations.
func TestSettleSpendPreservesInflight(t *testing.T) {
	bc := testBalanceCache(t)

	// $3 of completed-unsettled consumption + $2 in-flight reservation = $5 pending.
	bc.Reserve(walletU, big.NewFloat(3))
	bc.Reserve(walletU, big.NewFloat(2))

	// Settlement covers only the $3 completed portion.
	bc.SettleSpend(walletU, big.NewFloat(3))

	ps := bc.GetPendingSpend(walletU)
	if ps.Cmp(big.NewFloat(2)) != 0 {
		t.Errorf("expected $2 in-flight reservation preserved, got %s (bug H1 regression)",
			ps.Text('f', 6))
	}
}

func TestSettleSpendDeletesWhenZero(t *testing.T) {
	bc := testBalanceCache(t)
	bc.Reserve(walletU, big.NewFloat(5))
	bc.SettleSpend(walletU, big.NewFloat(5))

	ps := bc.GetPendingSpend(walletU)
	if ps.Sign() != 0 {
		t.Errorf("expected pendingSpend cleared, got %s", ps.Text('f', 6))
	}
}

// flatCostFn adapts a flat per-token price map to RestorePendingSpend's costFn shape
// (the production caller passes the aggregator's RecordCostUSD instead).
func flatCostFn(prices map[string]*big.Float) func(RequestRecord) *big.Float {
	return func(rec RequestRecord) *big.Float {
		return CostBreakdownUSD(rec.Model,
			rec.PromptTokens, rec.CompletionTokens, rec.CachedTokens, rec.TotalTokens,
			prices, nil, nil)
	}
}

func TestRestorePendingSpend(t *testing.T) {
	bc := testBalanceCache(t)
	prices := map[string]*big.Float{"default": big.NewFloat(1)} // $1/token (per-token already)

	records := []RequestRecord{
		{Wallet: walletU, Model: "default", TotalTokens: 3, Status: 200},
		{Wallet: walletU, Model: "default", TotalTokens: 2, Status: 200},
	}
	bc.RestorePendingSpend(records, flatCostFn(prices))

	ps := bc.GetPendingSpend(walletU)
	if ps.Cmp(big.NewFloat(5)) != 0 {
		t.Errorf("expected restored pendingSpend $5, got %s", ps.Text('f', 6))
	}
}

// TestReserveCreditLimitCap covers the audit MEDIUM fix: max_pending_spend_fil caps a
// single wallet's unsettled spend even when its balance could cover more. With FIL at
// $2.0 and a 4 FIL cap (= $8), a wallet with $25 of balance is still refused once its
// pending tally would exceed $8.
func TestReserveCreditLimitCap(t *testing.T) {
	bc := testBalanceCache(t)
	bc.SetCreditLimits(nil, big.NewFloat(4)) // 4 FIL cap → $8 at price 2.0

	if !bc.Reserve(walletU, big.NewFloat(5)) {
		t.Fatal("first reserve of $5 should succeed (pending 5 <= cap 8)")
	}
	if bc.Reserve(walletU, big.NewFloat(5)) {
		t.Error("second reserve of $5 should be refused by the cap (pending 10 > cap 8) despite a $25 balance")
	}
	// A smaller reserve that stays under the cap still succeeds.
	if !bc.Reserve(walletU, big.NewFloat(2)) {
		t.Error("reserve of $2 should succeed (pending 7 <= cap 8)")
	}
}

// TestReserveCreditLimitDisabled: a nil/zero cap leaves Reserve gated only by balance.
func TestReserveCreditLimitDisabled(t *testing.T) {
	bc := testBalanceCache(t)
	bc.SetCreditLimits(nil, nil) // explicitly disabled
	// Full balance ($25) is reservable with no cap interfering.
	if !bc.Reserve(walletU, big.NewFloat(25)) {
		t.Error("with no cap, reserving the full $25 balance should succeed")
	}
}

// TestMinBalanceReserveBuffer covers the audit MEDIUM fix: min_balance_fil keeps a
// reserve buffer un-spendable. With FIL at $2.0 and a 1 FIL buffer (= $2), a $25
// balance only exposes $23 of spendable headroom.
func TestMinBalanceReserveBuffer(t *testing.T) {
	bc := testBalanceCache(t)
	bc.SetCreditLimits(big.NewFloat(1), nil) // 1 FIL buffer → $2 at price 2.0

	if !bc.HasSufficientBalance(walletU, big.NewFloat(23)) {
		t.Error("$23 should be spendable ($25 balance minus $2 buffer)")
	}
	if bc.HasSufficientBalance(walletU, big.NewFloat(24)) {
		t.Error("$24 should NOT be spendable: the $2 reserve buffer must stay untouched")
	}
}
