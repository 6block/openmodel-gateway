package settlement

import (
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
)

// TestAdjustActualExceedsEstimate covers the negative-diff branch: when the real
// cost is higher than the reservation, pendingSpend is corrected UPWARD to actual.
func TestAdjustActualExceedsEstimate(t *testing.T) {
	bc := testBalanceCache(t)
	est := big.NewFloat(5)
	if !bc.Reserve(walletU, est) {
		t.Fatal("reserve failed")
	}
	// actual ($8) > estimated ($5): pending should rise from 5 to 8.
	bc.Adjust(walletU, est, big.NewFloat(8))
	if ps := bc.GetPendingSpend(walletU); ps.Cmp(big.NewFloat(8)) != 0 {
		t.Errorf("expected pendingSpend corrected up to $8, got %s", ps.Text('f', 6))
	}
}

// TestAdjustFloorsAtZero covers the safety floor: an over-reversal cannot drive
// pendingSpend negative.
func TestAdjustFloorsAtZero(t *testing.T) {
	bc := testBalanceCache(t)
	bc.Reserve(walletU, big.NewFloat(3))
	// estimated ($10) far exceeds the $3 actually pending → would go to -7, must floor to 0.
	bc.Adjust(walletU, big.NewFloat(10), new(big.Float))
	if ps := bc.GetPendingSpend(walletU); ps.Sign() != 0 {
		t.Errorf("expected pendingSpend floored to 0, got %s", ps.Text('f', 6))
	}
}

// TestAdjustNoPendingIsNoop covers the nil-pending early return: adjusting a wallet
// with no reservation must not panic or create a (negative) entry.
func TestAdjustNoPendingIsNoop(t *testing.T) {
	bc := testBalanceCache(t)
	bc.Adjust(walletU, big.NewFloat(5), big.NewFloat(2)) // never reserved
	if ps := bc.GetPendingSpend(walletU); ps.Sign() != 0 {
		t.Errorf("expected no pendingSpend, got %s", ps.Text('f', 6))
	}
}

// TestSettleSpendAbsentWalletIsNoop covers the wallet-absent early return.
func TestSettleSpendAbsentWalletIsNoop(t *testing.T) {
	bc := testBalanceCache(t)
	bc.SettleSpend(walletU, big.NewFloat(5)) // nothing pending
	if ps := bc.GetPendingSpend(walletU); ps.Sign() != 0 {
		t.Errorf("expected no pendingSpend, got %s", ps.Text('f', 6))
	}
}

// TestRestorePendingSpendDefaultAndSkip covers (a) a non-default model falling back
// to the "default" price, and (b) a model with neither a price nor a default being
// skipped (contributing nothing).
func TestRestorePendingSpendDefaultAndSkip(t *testing.T) {
	// (a) default fallback
	bc := testBalanceCache(t)
	bc.RestorePendingSpend(
		[]RequestRecord{{Wallet: walletU, Model: "unconfigured", TotalTokens: 4, Status: 200}},
		flatCostFn(map[string]*big.Float{"default": big.NewFloat(1)}), // $1/token
	)
	if ps := bc.GetPendingSpend(walletU); ps.Cmp(big.NewFloat(4)) != 0 {
		t.Errorf("unconfigured model should use default => $4, got %s", ps.Text('f', 6))
	}

	// (b) no price and no default → record skipped
	bc2 := testBalanceCache(t)
	bc2.RestorePendingSpend(
		[]RequestRecord{{Wallet: walletU, Model: "unconfigured", TotalTokens: 4, Status: 200}},
		flatCostFn(map[string]*big.Float{"premium": big.NewFloat(5)}), // no "default"
	)
	if ps := bc2.GetPendingSpend(walletU); ps.Sign() != 0 {
		t.Errorf("record with no price and no default must be skipped, got %s", ps.Text('f', 6))
	}
}

// TestConcurrentReserveNeverOverspends is the concurrency/overspend-prevention test
// (run under -race). Available balance is exactly $25; 100 goroutines each try to
// reserve $1. Reserve must be atomic, so EXACTLY 25 succeed and pendingSpend lands
// at exactly $25 — never more (which would mean the user overspent).
func TestConcurrentReserveNeverOverspends(t *testing.T) {
	bc := testBalanceCache(t) // $5 USDC + 10 FIL @ $2 = $25 available

	const goroutines = 100
	var successes atomic.Int32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if bc.Reserve(walletU, big.NewFloat(1)) {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := successes.Load(); got != 25 {
		t.Errorf("expected exactly 25 successful $1 reserves out of $25, got %d (overspend or lost update)", got)
	}
	if ps := bc.GetPendingSpend(walletU); ps.Cmp(big.NewFloat(25)) != 0 {
		t.Errorf("expected pendingSpend exactly $25, got %s", ps.Text('f', 6))
	}
	// And available must not have gone negative.
	if !bc.HasSufficientBalance(walletU, new(big.Float)) {
		t.Error("available balance went negative")
	}
}
