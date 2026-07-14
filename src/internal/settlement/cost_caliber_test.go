package settlement

import (
	"math/big"
	"testing"
)

// #47 regression: every component that prices a completed request must use ONE formula.
// These tests pin the invariant that made the flat-vs-split mismatch possible.

func calCfg() *Config {
	cfg := &Config{
		ModelPricesUSD: map[string]string{"default": "1000000"}, // $1/token output
		ModelCatalog: map[string]ModelInfo{
			"default": {InputUSD: "500000", CacheReadUSD: "250000", ContextWindow: 32768, MaxOutput: 4096},
		},
		SPAddressMap: map[string]string{"miner1": "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"},
		SupportedTokens: []TokenConfig{
			{Symbol: "FIL", Address: "0x0000000000000000000000000000000000000000", Decimals: 18},
		},
		DeductionPriority: []string{"FIL"},
	}
	return cfg
}

// The standalone CostBreakdownUSD and the aggregator's RecordCostUSD must agree
// exactly — for the split path, the cached-token path, and the flat fallback.
func TestCostCaliber_SharedFormulaMatchesAggregator(t *testing.T) {
	cfg := calCfg()
	agg := NewAggregator(cfg, map[string]string{"w1": "miner1"}, discardLogger())
	out := map[string]*big.Float{"default": big.NewFloat(1)}
	inp := map[string]*big.Float{"default": big.NewFloat(0.5)}
	cr := map[string]*big.Float{"default": big.NewFloat(0.25)}

	cases := []RequestRecord{
		{Model: "default", PromptTokens: 80, CompletionTokens: 20, CachedTokens: 0, TotalTokens: 100},
		{Model: "default", PromptTokens: 80, CompletionTokens: 20, CachedTokens: 30, TotalTokens: 100},
		{Model: "default", PromptTokens: 0, CompletionTokens: 0, CachedTokens: 0, TotalTokens: 50},   // metered/legacy: only total known
		{Model: "default", PromptTokens: 10, CompletionTokens: 5, CachedTokens: 99, TotalTokens: 15}, // cached > prompt → clamped
	}
	for i, rec := range cases {
		want := agg.RecordCostUSD(rec)
		got := CostBreakdownUSD(rec.Model, rec.PromptTokens, rec.CompletionTokens, rec.CachedTokens, rec.TotalTokens, out, inp, cr)
		if want.Cmp(got) != 0 {
			t.Errorf("case %d: aggregator=%s shared=%s — formulas diverged", i, want.Text('f', 6), got.Text('f', 6))
		}
	}

	// Split sanity: 80 prompt (30 cached) + 20 completion
	// = 50×0.5 + 30×0.25 + 20×1 = 25 + 7.5 + 20 = 52.5, NOT flat 100×1 = 100.
	got := CostBreakdownUSD("default", 80, 20, 30, 100, out, inp, cr)
	if got.Cmp(big.NewFloat(52.5)) != 0 {
		t.Errorf("split cost: want 52.5, got %s", got.Text('f', 6))
	}
}

// End-to-end pending drain: reserve a flat estimate (what the gateway reserves),
// adjust to the SPLIT actual (what the fixed gateway now does), settle the same
// record through the aggregator, and reduce pending by settledPerWallet. Pending
// must land at exactly zero — with the old flat adjustment it retained
// prompt×(output−input)+cached×(output−cache_read) per request forever.
func TestCostCaliber_PendingDrainsToZeroWithCatalog(t *testing.T) {
	cfg := calCfg()
	agg := NewAggregator(cfg, map[string]string{"w1": "miner1"}, discardLogger())
	bc := NewBalanceCache(nil, cfg.SupportedTokens, NewPricer(&Config{FILPriceUSD: "2.0", FILPriceSource: "manual"}, discardLogger()), 30, discardLogger())

	wallet := "0x9875c8D91fE91199D7B9207d78f5A592EFCc6f88"
	rec := RequestRecord{
		RequestID: "r1", Wallet: wallet, WorkerID: "w1", Model: "default", Status: 200,
		PromptTokens: 80, CompletionTokens: 20, CachedTokens: 30, TotalTokens: 100,
	}

	// Gateway path: reserve flat estimate, then adjust to split actual.
	estimate := big.NewFloat(120) // flat over-reserve (max_tokens + prompt estimate)
	bc.chainBalances[wallet] = map[string]*big.Int{
		"0x0000000000000000000000000000000000000000": new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18)),
	}
	if !bc.Reserve(wallet, estimate) {
		t.Fatal("reserve failed")
	}
	actual := agg.RecordCostUSD(rec) // 52.5 — the SAME formula the fixed gateway uses
	bc.Adjust(wallet, estimate, actual)

	// Settlement path: aggregate the identical record and clear pending by what settled.
	filPrice := big.NewFloat(2.0)
	_, unresolved, settledPerWallet, debts := agg.AggregateWithDebts([]RequestRecord{rec}, nil, filPrice, bc.GetAllBalances())
	if len(unresolved) != 0 || len(debts) != 0 {
		t.Fatalf("expected clean settlement, unresolved=%d debts=%d", len(unresolved), len(debts))
	}
	bc.SettleSpend(wallet, settledPerWallet[wallet])

	if ps := bc.GetPendingSpend(wallet); ps.Sign() != 0 {
		t.Errorf("pending must drain to ZERO after settle (was the un-drainable residue): got %s", ps.Text('f', 6))
	}
}

// Reconciler caliber: with a catalog model, billedTotal must price records with the
// split (matching settled), not the flat total — otherwise the drift alert is blind.
func TestCostCaliber_ReconcilerBillsSplit(t *testing.T) {
	cfg := calCfg()
	agg := NewAggregator(cfg, map[string]string{"w1": "miner1"}, discardLogger())
	recs := []RequestRecord{
		{RequestID: "r1", Wallet: walletU, WorkerID: "w1", Model: "default", Status: 200,
			PromptTokens: 80, CompletionTokens: 20, CachedTokens: 30, TotalTokens: 100},
	}
	dir := t.TempDir()
	reqLog := dir + "/requests.jsonl"
	bc := NewBalanceCache(nil, nil, NewPricer(coverageCfg(), discardLogger()), 30, discardLogger())
	st := &mutableSettled{settled: new(big.Float), debt: new(big.Float)}
	rc := NewReconciler(reqLog, dir+"/dl.jsonl", dir, agg.RecordCostUSD, bc, st, big.NewFloat(0.01), discardLogger())
	// baseline pass (empty), then the split-priced record settles to $52.5.
	if _, err := rc.Run(t.Context()); err != nil {
		t.Fatalf("baseline run: %v", err)
	}
	writeRequestLog(t, reqLog, recs)
	st.settled = big.NewFloat(52.5)
	rep, err := rc.Run(t.Context())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.BilledUSD != "52.500000" {
		t.Errorf("reconciler must bill the split cost 52.5 (settled caliber), got %s", rep.BilledUSD)
	}
	if !rep.WithinTolerance {
		t.Errorf("billed(split)==settled(split) must reconcile clean, drift=%s", rep.DriftUSD)
	}
}
