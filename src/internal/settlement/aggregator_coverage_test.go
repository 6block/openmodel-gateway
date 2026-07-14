package settlement

import (
	"io"
	"log/slog"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

const walletV = "0x00000000000000000000000000000000000000B2" // > walletU

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func coverageCfg() *Config {
	return &Config{
		ModelPricesUSD: map[string]string{"default": "1000000"}, // $1/token
		SupportedTokens: []TokenConfig{
			{Symbol: "USDC", Address: usdcAddr, Decimals: 6},
			{Symbol: "FIL", Address: filAddr, Decimals: 18},
		},
		DeductionPriority: []string{"USDC", "FIL"},
		SPAddressMap:      map[string]string{"miner1": sp1Addr, "miner2": sp2Addr},
	}
}

// TestPerModelPricingNonDefault exercises getModelPrice's per-model branch:
// a model with its own configured price must be charged at that price, NOT default.
func TestPerModelPricingNonDefault(t *testing.T) {
	cfg := coverageCfg()
	cfg.ModelPricesUSD = map[string]string{"default": "1000000", "premium": "5000000"} // $1 and $5/token
	agg := NewAggregator(cfg, map[string]string{"w1": "miner1"}, discardLogger())

	balances := map[string]map[string]*big.Int{walletU: {usdcAddr: usdc(1000)}}
	// premium model, 2 tokens => 2 * $5 = $10
	records := []RequestRecord{{Wallet: walletU, WorkerID: "w1", Model: "premium", TotalTokens: 2, Status: 200}}
	items, _ := agg.Aggregate(records, big.NewFloat(2.0), balances)

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Amount.Cmp(usdc(10)) != 0 {
		t.Errorf("premium model: expected $10 USDC (2 tokens x $5), got %s", items[0].Amount)
	}
}

// TestUnknownModelFallsBackToDefault: a model name not in the price map uses "default".
func TestUnknownModelFallsBackToDefault(t *testing.T) {
	cfg := coverageCfg()
	cfg.ModelPricesUSD = map[string]string{"default": "1000000", "premium": "5000000"}
	agg := NewAggregator(cfg, map[string]string{"w1": "miner1"}, discardLogger())

	balances := map[string]map[string]*big.Int{walletU: {usdcAddr: usdc(1000)}}
	records := []RequestRecord{{Wallet: walletU, WorkerID: "w1", Model: "some-unconfigured-model", TotalTokens: 3, Status: 200}}
	items, _ := agg.Aggregate(records, big.NewFloat(2.0), balances)

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Amount.Cmp(usdc(3)) != 0 { // 3 tokens x $1 default
		t.Errorf("unknown model should fall back to default $1/token => $3, got %s", items[0].Amount)
	}
}

// TestNoPriceNoDefaultSettlesZero documents the zero-price fallback: if a model
// is unpriced AND there is no "default", getModelPrice returns 0 and the request
// is settled for NOTHING (a silent revenue leak). ApplyDefaults normally injects
// "default", but a hand-built config without it exposes this branch.
func TestNoPriceNoDefaultSettlesZero(t *testing.T) {
	cfg := coverageCfg()
	cfg.ModelPricesUSD = map[string]string{"premium": "5000000"} // NO default
	agg := NewAggregator(cfg, map[string]string{"w1": "miner1"}, discardLogger())

	balances := map[string]map[string]*big.Int{walletU: {usdcAddr: usdc(1000)}}
	records := []RequestRecord{{Wallet: walletU, WorkerID: "w1", Model: "basic", TotalTokens: 5, Status: 200}}
	items, unresolved := agg.Aggregate(records, big.NewFloat(2.0), balances)

	if len(unresolved) != 0 {
		t.Fatalf("worker resolves fine; expected 0 unresolved, got %d", len(unresolved))
	}
	if len(items) != 0 {
		t.Errorf("zero-price model yields no settlement item (free usage), got %d items", len(items))
	}
}

// TestMinerNotInAddressMap covers the SECOND resolution-failure branch:
// worker -> miner succeeds, but miner has no sp_address_map entry -> unresolved.
func TestMinerNotInAddressMap(t *testing.T) {
	cfg := coverageCfg()
	agg := NewAggregator(cfg, map[string]string{"w1": "orphan-miner"}, discardLogger()) // "orphan-miner" not in SPAddressMap

	balances := map[string]map[string]*big.Int{walletU: {usdcAddr: usdc(100)}}
	records := []RequestRecord{{Wallet: walletU, WorkerID: "w1", Model: "default", TotalTokens: 5, Status: 200}}
	items, unresolved := agg.Aggregate(records, big.NewFloat(2.0), balances)

	if len(items) != 0 {
		t.Errorf("expected 0 items when miner unmapped, got %d", len(items))
	}
	if len(unresolved) != 1 {
		t.Errorf("expected 1 unresolved (miner->EVM missing), got %d", len(unresolved))
	}
}

// TestDeterministicMultiWalletSort verifies items are ordered by (wallet, SP, token).
func TestDeterministicMultiWalletSort(t *testing.T) {
	cfg := coverageCfg()
	agg := NewAggregator(cfg, map[string]string{"w1": "miner1", "w2": "miner2"}, discardLogger())

	balances := map[string]map[string]*big.Int{
		walletU: {usdcAddr: usdc(100)},
		walletV: {usdcAddr: usdc(100)},
	}
	// walletU spends at sp1 and sp2; walletV spends at sp1. Submitted out of order.
	records := []RequestRecord{
		{Wallet: walletV, WorkerID: "w1", Model: "default", TotalTokens: 1, Status: 200},
		{Wallet: walletU, WorkerID: "w2", Model: "default", TotalTokens: 1, Status: 200},
		{Wallet: walletU, WorkerID: "w1", Model: "default", TotalTokens: 1, Status: 200},
	}
	items, _ := agg.Aggregate(records, big.NewFloat(2.0), balances)

	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	// Expected order: U/sp1, U/sp2, V/sp1  (walletU "..B1" < walletV "..B2"; sp1 "..A1" < sp2 "..A2")
	want := []struct {
		wallet string
		sp     string
	}{
		{walletU, sp1Addr},
		{walletU, sp2Addr},
		{walletV, sp1Addr},
	}
	for i, w := range want {
		if items[i].UserWallet != w.wallet {
			t.Errorf("item %d: wallet = %s, want %s", i, items[i].UserWallet, w.wallet)
		}
		if items[i].SPEVM != common.HexToAddress(w.sp) {
			t.Errorf("item %d: sp = %s, want %s", i, items[i].SPEVM.Hex(), w.sp)
		}
	}
}

// TestDeductionPriorityFILFirst verifies a non-default deduction order is honored:
// with ["FIL","USDC"], FIL is spent before USDC.
func TestDeductionPriorityFILFirst(t *testing.T) {
	cfg := coverageCfg()
	cfg.DeductionPriority = []string{"FIL", "USDC"}
	agg := NewAggregator(cfg, map[string]string{"w1": "miner1"}, discardLogger())

	balances := map[string]map[string]*big.Int{walletU: {usdcAddr: usdc(100), filAddr: fil(10)}}
	// 4 tokens x $1 = $4; at $2/FIL that's 2 FIL, all from FIL (USDC untouched).
	records := []RequestRecord{{Wallet: walletU, WorkerID: "w1", Model: "default", TotalTokens: 4, Status: 200}}
	items, _ := agg.Aggregate(records, big.NewFloat(2.0), balances)

	if len(items) != 1 {
		t.Fatalf("expected 1 item (FIL only), got %d", len(items))
	}
	if items[0].TokenAddr != common.HexToAddress(filAddr) {
		t.Errorf("expected FIL deducted first, got token %s", items[0].TokenAddr.Hex())
	}
	if items[0].Amount.Cmp(fil(2)) != 0 {
		t.Errorf("expected 2 FIL ($4 / $2), got %s", items[0].Amount)
	}
}

// TestFindTokenMissingSymbolSkipped: a deduction_priority symbol with no matching
// supported token is skipped gracefully (findToken -> nil), falling through to the
// next token.
func TestFindTokenMissingSymbolSkipped(t *testing.T) {
	cfg := coverageCfg()
	cfg.DeductionPriority = []string{"DAI", "USDC"} // DAI is not a supported token
	agg := NewAggregator(cfg, map[string]string{"w1": "miner1"}, discardLogger())

	balances := map[string]map[string]*big.Int{walletU: {usdcAddr: usdc(100)}}
	records := []RequestRecord{{Wallet: walletU, WorkerID: "w1", Model: "default", TotalTokens: 5, Status: 200}}
	items, _ := agg.Aggregate(records, big.NewFloat(2.0), balances)

	if len(items) != 1 {
		t.Fatalf("expected 1 item (USDC, DAI skipped), got %d", len(items))
	}
	if items[0].TokenAddr != common.HexToAddress(usdcAddr) || items[0].Amount.Cmp(usdc(5)) != 0 {
		t.Errorf("expected $5 USDC after skipping DAI, got token %s amount %s",
			items[0].TokenAddr.Hex(), items[0].Amount)
	}
}

// TestEstimateCostUSD covers all three branches: model match, default fallback, none.
func TestEstimateCostUSD(t *testing.T) {
	prices := map[string]*big.Float{
		"default": big.NewFloat(2), // $2/token (already per-token)
		"premium": big.NewFloat(5), // $5/token
	}
	if got := EstimateCostUSD("premium", 10, prices); got.Cmp(big.NewFloat(50)) != 0 {
		t.Errorf("premium 10 tokens: got %s, want 50", got.Text('f', 2))
	}
	if got := EstimateCostUSD("unknown", 10, prices); got.Cmp(big.NewFloat(20)) != 0 {
		t.Errorf("unknown model should use default: got %s, want 20", got.Text('f', 2))
	}
	if got := EstimateCostUSD("x", 10, map[string]*big.Float{}); got.Sign() != 0 {
		t.Errorf("no price and no default: expected 0, got %s", got.Text('f', 2))
	}
}

// TestFormatFIL covers the value path and the nil path.
func TestFormatFIL(t *testing.T) {
	if got := FormatFIL(nil); got != "0" {
		t.Errorf("FormatFIL(nil) = %q, want %q", got, "0")
	}
	if got := FormatFIL(fil(1)); got != "1.000000 FIL" {
		t.Errorf("FormatFIL(1e18) = %q, want %q", got, "1.000000 FIL")
	}
	half := new(big.Int).Div(fil(1), big.NewInt(2))
	if got := FormatFIL(half); got != "0.500000 FIL" {
		t.Errorf("FormatFIL(0.5 FIL) = %q, want %q", got, "0.500000 FIL")
	}
}
