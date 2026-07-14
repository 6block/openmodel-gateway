package settlement

import (
	"io"
	"log/slog"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

const (
	usdcAddr = "0x0000000000000000000000000000000000000001"
	filAddr  = "0x0000000000000000000000000000000000000000"
	sp1Addr  = "0x00000000000000000000000000000000000000A1"
	sp2Addr  = "0x00000000000000000000000000000000000000A2"
	walletU  = "0x00000000000000000000000000000000000000B1"
)

func testAggregator(t *testing.T) *Aggregator {
	t.Helper()
	cfg := &Config{
		ModelPricesUSD: map[string]string{"default": "1000000"}, // $1 per token
		SupportedTokens: []TokenConfig{
			{Symbol: "USDC", Address: usdcAddr, Decimals: 6},
			{Symbol: "FIL", Address: filAddr, Decimals: 18},
		},
		DeductionPriority: []string{"USDC", "FIL"},
		SPAddressMap: map[string]string{
			"miner1": sp1Addr,
			"miner2": sp2Addr,
		},
	}
	workerSPMap := map[string]string{"w1": "miner1", "w2": "miner2"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewAggregator(cfg, workerSPMap, logger)
}

func usdc(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1_000_000)) }
func fil(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
}

// TestCrossSPNoDoubleSpend is the regression test for bug C4: a user who spent
// with two SPs must not have the same token balance allocated twice.
func TestCrossSPNoDoubleSpend(t *testing.T) {
	agg := testAggregator(t)

	// User has $5 USDC and 10 FIL ($20 at $2/FIL).
	balances := map[string]map[string]*big.Int{
		walletU: {
			usdcAddr: usdc(5),
			filAddr:  fil(10),
		},
	}
	// Spent $3 with SP1 and $3 with SP2 (3 tokens each at $1/token).
	records := []RequestRecord{
		{Wallet: walletU, WorkerID: "w1", Model: "default", TotalTokens: 3, Status: 200},
		{Wallet: walletU, WorkerID: "w2", Model: "default", TotalTokens: 3, Status: 200},
	}
	filPrice := big.NewFloat(2.0)

	items, unresolved := agg.Aggregate(records, filPrice, balances)
	if len(unresolved) != 0 {
		t.Fatalf("expected 0 unresolved, got %d", len(unresolved))
	}

	// Sum allocations per token across ALL items.
	totalUSDC := big.NewInt(0)
	totalFIL := big.NewInt(0)
	for _, it := range items {
		switch it.TokenAddr {
		case common.HexToAddress(usdcAddr):
			totalUSDC.Add(totalUSDC, it.Amount)
		case common.HexToAddress(filAddr):
			totalFIL.Add(totalFIL, it.Amount)
		}
	}

	// USDC allocated must NOT exceed the user's $5 USDC balance.
	if totalUSDC.Cmp(usdc(5)) > 0 {
		t.Errorf("USDC over-allocated: got %s, balance is %s (bug C4 regression)",
			totalUSDC, usdc(5))
	}
	// Total value must equal $6: $5 USDC + $1 worth of FIL (0.5 FIL at $2).
	if totalUSDC.Cmp(usdc(5)) != 0 {
		t.Errorf("expected exactly $5 USDC allocated, got %s", totalUSDC)
	}
	expectedFIL := new(big.Int).Div(fil(1), big.NewInt(2)) // 0.5 FIL
	if totalFIL.Cmp(expectedFIL) != 0 {
		t.Errorf("expected 0.5 FIL allocated, got %s", totalFIL)
	}
}

// TestSingleSPStablecoinOnly verifies the simple case: enough USDC, no FIL needed.
func TestSingleSPStablecoinOnly(t *testing.T) {
	agg := testAggregator(t)
	balances := map[string]map[string]*big.Int{
		walletU: {usdcAddr: usdc(100), filAddr: fil(10)},
	}
	records := []RequestRecord{
		{Wallet: walletU, WorkerID: "w1", Model: "default", TotalTokens: 10, Status: 200},
	}
	items, _ := agg.Aggregate(records, big.NewFloat(2.0), balances)

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].TokenAddr != common.HexToAddress(usdcAddr) {
		t.Errorf("expected USDC deduction, got token %s", items[0].TokenAddr.Hex())
	}
	if items[0].Amount.Cmp(usdc(10)) != 0 {
		t.Errorf("expected $10 USDC, got %s", items[0].Amount)
	}
}

// TestUnresolvedWorker verifies records for unmapped workers are returned as
// unresolved (dead-letter), not silently dropped.
func TestUnresolvedWorker(t *testing.T) {
	agg := testAggregator(t)
	balances := map[string]map[string]*big.Int{
		walletU: {usdcAddr: usdc(100)},
	}
	records := []RequestRecord{
		{Wallet: walletU, WorkerID: "unknown-worker", Model: "default", TotalTokens: 5, Status: 200},
	}
	items, unresolved := agg.Aggregate(records, big.NewFloat(2.0), balances)

	if len(items) != 0 {
		t.Errorf("expected 0 items for unresolved worker, got %d", len(items))
	}
	if len(unresolved) != 1 {
		t.Errorf("expected 1 unresolved record, got %d", len(unresolved))
	}
}

// TestZeroFILPriceDefersAll verifies bug M2 guard: a non-positive FIL price
// defers all records instead of dividing by zero.
func TestZeroFILPriceDefersAll(t *testing.T) {
	agg := testAggregator(t)
	balances := map[string]map[string]*big.Int{walletU: {filAddr: fil(10)}}
	records := []RequestRecord{
		{Wallet: walletU, WorkerID: "w1", Model: "default", TotalTokens: 5, Status: 200},
	}
	items, unresolved := agg.Aggregate(records, big.NewFloat(0), balances)
	if len(items) != 0 {
		t.Errorf("expected 0 items with zero FIL price, got %d", len(items))
	}
	if len(unresolved) != 1 {
		t.Errorf("expected all records deferred, got %d unresolved", len(unresolved))
	}
}

// TestBatchHashContentBased verifies the audit HIGH fix: the batch hash derives
// purely from economic content + request IDs (NO cursor salt), so a crash-retry or
// a lost/reset cursor that re-scans the SAME records reproduces the SAME hash (the
// contract's processedBatches dedup works → no double-charge), while a genuinely
// different batch (different request IDs) hashes differently even with identical
// amounts.
func TestBatchHashContentBased(t *testing.T) {
	mk := func(reqIDs ...string) []SettlementItem {
		return []SettlementItem{{
			UserEVM:    common.HexToAddress(walletU),
			SPEVM:      common.HexToAddress(sp1Addr),
			Amount:     usdc(5),
			TokenAddr:  common.HexToAddress(usdcAddr),
			RequestIDs: reqIDs,
		}}
	}

	h1 := BatchHash(mk("r1", "r2"))
	h1again := BatchHash(mk("r1", "r2")) // same records (crash-retry / cursor reset)
	h2 := BatchHash(mk("r3", "r4"))      // different records, identical amount

	if h1 != h1again {
		t.Error("same records must hash identically (crash-retry / cursor-reset dedup)")
	}
	if h1 == h2 {
		t.Error("different request IDs with identical amounts must hash differently")
	}
	if BatchHash(mk("r2", "r1")) != h1 {
		t.Error("request-ID order must not affect the hash")
	}
}

// TestAggregateUnderfundedCarriesDebt is the regression for audit HIGH fix C: when a
// wallet's balance cannot cover its usage, the shortfall is reported as a carried
// debt (not silently dropped) and settledPerWallet reflects only what was actually
// allocated. Once the balance is topped up, replaying the debt collects it in full.
func TestAggregateUnderfundedCarriesDebt(t *testing.T) {
	agg := testAggregator(t)
	filPrice := big.NewFloat(2.0)

	// User spent $10 with SP1 (10 tokens @ $1) but holds only $4 USDC.
	balances := map[string]map[string]*big.Int{walletU: {usdcAddr: usdc(4)}}
	records := []RequestRecord{
		{RequestID: "r1", Wallet: walletU, WorkerID: "w1", Model: "default", TotalTokens: 10, Status: 200},
	}

	items, unresolved, settled, debts := agg.AggregateWithDebts(records, nil, filPrice, balances)
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %d", len(unresolved))
	}
	if got := settled[walletU]; got == nil || got.Cmp(big.NewFloat(4)) != 0 {
		t.Fatalf("settledPerWallet = %v, want 4 (only the balance was settled)", got)
	}
	if len(debts) != 1 || debts[0].USD.Cmp(big.NewFloat(6)) != 0 {
		t.Fatalf("expected one $6 carried debt, got %+v", debts)
	}
	var totalSettled big.Int
	for _, it := range items {
		totalSettled.Add(&totalSettled, it.Amount)
	}
	if totalSettled.Cmp(usdc(4)) != 0 {
		t.Errorf("settled items total = %s, want %s", totalSettled.String(), usdc(4).String())
	}

	// Top up the balance and replay the carried debt with NO new records.
	balances2 := map[string]map[string]*big.Int{walletU: {usdcAddr: usdc(100)}}
	items2, _, settled2, debts2 := agg.AggregateWithDebts(nil, debts, filPrice, balances2)
	if len(debts2) != 0 {
		t.Fatalf("debt not fully collected after top-up: %d remain", len(debts2))
	}
	if got := settled2[walletU]; got == nil || got.Cmp(big.NewFloat(6)) != 0 {
		t.Fatalf("collected settledPerWallet = %v, want 6", got)
	}
	var collected big.Int
	for _, it := range items2 {
		collected.Add(&collected, it.Amount)
	}
	if collected.Cmp(usdc(6)) != 0 {
		t.Errorf("collected items total = %s, want %s", collected.String(), usdc(6).String())
	}
}

func TestSplitBatches(t *testing.T) {
	mk := func(n int) []SettlementItem {
		out := make([]SettlementItem, n)
		return out
	}
	if got := len(splitBatches(mk(10), 50)); got != 1 {
		t.Errorf("10 items / 50 = 1 batch, got %d", got)
	}
	if got := len(splitBatches(mk(120), 50)); got != 3 {
		t.Errorf("120 items / 50 = 3 batches, got %d", got)
	}
	if got := len(splitBatches(mk(100), 50)); got != 2 {
		t.Errorf("100 items / 50 = 2 batches, got %d", got)
	}
}
