package settlement

import (
	"math/big"
	"testing"
)

// The image unit must satisfy the SAME no-residue invariant the token unit does, for the
// same reason: the gateway's pendingSpend adjustment and settlement's clearing price must
// be the same number, or every request leaves a residue that never drains (#47). An image
// request has zero tokens, so the token paths cannot express it — this pins that the unit
// path is priced identically on both sides.
func TestCostCaliber_ImagePendingDrainsToZero(t *testing.T) {
	cfg := imageCfg()
	agg := NewAggregator(cfg, map[string]string{"w1": "miner1"}, discardLogger())
	bc := NewBalanceCache(nil, cfg.SupportedTokens,
		NewPricer(&Config{FILPriceUSD: "2.0", FILPriceSource: "manual"}, discardLogger()), 30, discardLogger())

	wallet := walletU
	rec := assetRecord("img1", "w1", "flux-schnell", 1, "deadbeef")

	// Gateway path: reserve an estimate, then adjust to the actual price of one image.
	estimate := big.NewFloat(10) // a deliberate over-reserve
	bc.chainBalances[wallet] = map[string]*big.Int{filAddr: fil(1000)}
	if !bc.Reserve(wallet, estimate) {
		t.Fatal("reserve failed")
	}
	actual := agg.RecordCostUSD(rec) // $2 — one image at the catalog's per-unit price
	if actual.Cmp(big.NewFloat(2)) != 0 {
		t.Fatalf("one image must cost $2 (catalog unit price), got %s", actual.Text('f', 6))
	}
	bc.Adjust(wallet, estimate, actual)

	// Settlement path: the same record, priced by the same aggregator.
	_, unresolved, settledPerWallet, debts := agg.AggregateWithDebts(
		[]RequestRecord{rec}, nil, big.NewFloat(2.0), bc.GetAllBalances())
	if len(unresolved) != 0 || len(debts) != 0 {
		t.Fatalf("expected clean settlement, unresolved=%d debts=%d", len(unresolved), len(debts))
	}
	bc.SettleSpend(wallet, settledPerWallet[wallet])

	if ps := bc.GetPendingSpend(wallet); ps.Sign() != 0 {
		t.Errorf("pending must drain to ZERO for an image record: got %s", ps.Text('f', 6))
	}
}

// Price scales with the number of images, and a batch of them settles as one item.
func TestCostCaliber_ImagePriceScalesWithQuantity(t *testing.T) {
	cfg := imageCfg()
	agg := NewAggregator(cfg, map[string]string{"w1": "miner1"}, discardLogger())

	for _, n := range []int{1, 2, 4} {
		rec := assetRecord("img", "w1", "flux-schnell", n, "h")
		got := agg.RecordCostUSD(rec)
		want := big.NewFloat(float64(n) * 2.0)
		if got.Cmp(want) != 0 {
			t.Errorf("%d images: want $%v, got %s", n, want, got.Text('f', 6))
		}
	}
}

// A token record and an image record must be priced by the same function without either
// shape leaking into the other: the token model's cost is unchanged by the presence of an
// image catalog entry, and vice versa.
func TestCostCaliber_UnitsDoNotLeakAcrossModels(t *testing.T) {
	cfg := imageCfg()
	agg := NewAggregator(cfg, map[string]string{"w1": "miner1"}, discardLogger())

	// Token model: 100 prompt (30 cached) + 20 completion is priced purely by tokens,
	// even though the config also declares an image model.
	tok := RequestRecord{Model: "default", PromptTokens: 100, CompletionTokens: 20,
		CachedTokens: 30, TotalTokens: 120, Receipt: &RecordReceipt{Quantity: 0}}
	tokCost := agg.RecordCostUSD(tok)

	// An image record with the same token counts must NOT be priced by tokens: its
	// quantity drives the cost.
	img := assetRecord("i", "w1", "flux-schnell", 1, "h")
	img.PromptTokens, img.CompletionTokens, img.TotalTokens = 100, 20, 120
	imgCost := agg.RecordCostUSD(img)

	if imgCost.Cmp(big.NewFloat(2)) != 0 {
		t.Errorf("image model must be priced per image regardless of token fields, got %s", imgCost.Text('f', 6))
	}
	if tokCost.Cmp(big.NewFloat(2)) == 0 {
		t.Error("token model must not be priced at the image unit price")
	}
}

// A misconfigured catalog (no unit price for an image model) must bill ZERO, not a
// guessed number — and Validate refuses such a catalog at start-up, so this only
// documents the runtime behaviour if the check is ever bypassed.
func TestCostCaliber_MissingUnitPriceBillsZeroNotGuess(t *testing.T) {
	got := CostBreakdownUSD("flux-schnell", 0, 0, 0, 0, nil, nil, nil, nil, 3)
	if got.Sign() != 0 {
		t.Errorf("a missing unit price must bill 0 rather than guess, got %s", got.Text('f', 6))
	}
}
