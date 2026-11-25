package settlement

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// assetRecord is a billable image-generation record: status 200, a wallet, and a
// worker receipt carrying the quantity + asset digest. Token counts are all zero —
// that is the point of the asset unit, and the pipeline must not read them as "no
// usage".
func assetRecord(id, worker, model string, quantity int, assetHash string) RequestRecord {
	return RequestRecord{
		RequestID: id, Timestamp: time.Now(), Wallet: walletU, WorkerID: worker,
		Model: model, Status: 200, Path: "/v1/images/generations",
		Receipt: &RecordReceipt{
			V: 1, RequestID: id, Model: model, Quantity: quantity, AssetHash: assetHash,
			TS: time.Now().Unix(), Pubkey: "pk", Sig: "sig-" + id,
		},
	}
}

// imageCfg is a settlement config where one model is billed per image and the rest per
// token.
func imageCfg() *Config {
	cfg := coverageCfg()
	cfg.ModelPricesUSD = map[string]string{"default": "1000000"} // $1/token
	cfg.ModelCatalog = map[string]ModelInfo{
		"flux-schnell": {
			UnitUSD:                "2", // $2 per image (per UNIT, not per 1M)
			Modality:               ModalityImage,
			PriceUnit:              PriceUnitImage,
			MaxImagesPerGeneration: 4,
		},
	}
	return cfg
}

// newImageSettlerOver builds a fresh settler over an EXISTING data dir (a process
// restart), again with the image catalog in place from construction.
func newImageSettlerOver(t *testing.T, dir string, mock *mockContract) *Settler {
	t.Helper()
	s, _ := newImageSettlerAt(t, dir, mock)
	return s
}

func newImageSettlerAt(t *testing.T, dir string, mock *mockContract) (*Settler, string) {
	t.Helper()
	reqLog := filepath.Join(dir, "requests.jsonl")
	cfg := imageCfg()
	cfg.MaxBatchSize = 50
	cfg.OperatorMinBalance = "0.1"
	cfg.FILPriceUSD = "2.0"
	cfg.FILPriceSource = "manual"
	cfg.IntervalMinutes = 60
	pricer := NewPricer(cfg, discardLogger())
	bc := NewBalanceCache(nil, cfg.SupportedTokens, pricer, 30, discardLogger())
	return NewSettler(cfg, mock, pricer, bc, nil, reqLog, dir, discardLogger()), dir
}

// newImageSettler builds a settler whose config ALREADY declares the image model, so the
// aggregator's price tables include its per-unit price. (Production reads the config once
// at start-up; a config swapped in after NewSettler would leave the tables stale, which is
// why the tests must build with it in place rather than assigning it afterwards.)
func newImageSettler(t *testing.T, mock *mockContract) (*Settler, string) {
	t.Helper()
	return newImageSettlerAt(t, t.TempDir(), mock)
}

// settleAsset appends one asset-billed record and runs one Settle pass, so a merkle
// batch containing it is committed and persisted — the same shape as settleRID, but
// through the image-billing config.
func settleAsset(t *testing.T, s *Settler, dir string, rec RequestRecord) {
	t.Helper()
	writeRequestLog(t, filepath.Join(dir, "requests.jsonl"), []RequestRecord{rec})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(6))
	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("settle %s: %v", rec.RequestID, err)
	}
}

// The leaf for a non-token record is the asset shape, carrying the unit and quantity,
// and it is reconstructible from what the proof serves.
func TestAssetBilling_LeafIsAssetShapeAndProofServesUnit(t *testing.T) {
	mock := newMockContract()
	s, dir := newImageSettler(t, mock)

	settleAsset(t, s, dir, assetRecord("img1", "w1", "flux-schnell", 1, "deadbeef"))

	proof, err := s.BuildReceiptProof("img1")
	if err != nil {
		t.Fatalf("proof: %v", err)
	}
	if proof.PriceUnit != string(PriceUnitImage) {
		t.Errorf("proof must serve the leaf's price_unit, got %q", proof.PriceUnit)
	}

	// The served leaf must be exactly the asset shape, recomputable from the record.
	wantLeaf := AssetLeaf("img1", walletU, proof.SP, "flux-schnell",
		PriceUnitImage, 1, "deadbeef", "sig-img1")
	if proof.Leaf != hex.EncodeToString(wantLeaf[:]) {
		t.Errorf("leaf is not the asset shape\n want %s\n got  %s",
			hex.EncodeToString(wantLeaf[:]), proof.Leaf)
	}

	// And the token shape must NOT reproduce it — the byte formats are distinct.
	wrong := ReceiptLeaf("img1", walletU, proof.SP, "flux-schnell", 0, 0, 0, "sig-img1")
	if proof.Leaf == hex.EncodeToString(wrong[:]) {
		t.Error("an asset record must not commit the token leaf shape")
	}
}

// The ledger entry must persist price_unit so a proof can be served after a restart,
// from the ledger alone.
func TestAssetBilling_PriceUnitPersistedInLedger(t *testing.T) {
	mock := newMockContract()
	s, dir := newImageSettler(t, mock)

	settleAsset(t, s, dir, assetRecord("img1", "w1", "flux-schnell", 2, "cafe"))

	// Reopen from disk the way a fresh process would: cold indexes, ledger only.
	s2 := newImageSettlerOver(t, dir, mock)
	proof, err := s2.BuildReceiptProof("img1")
	if err != nil {
		t.Fatalf("proof after restart: %v", err)
	}
	if proof.PriceUnit != string(PriceUnitImage) {
		t.Errorf("price_unit must survive a restart (persisted in the ledger), got %q", proof.PriceUnit)
	}
	if proof.Record == nil {
		t.Fatal("the record must resolve after a restart")
	}
}

// A token record must NOT gain a price_unit: its commitment has to stay byte-identical
// to what every pre-existing proof records.
func TestAssetBilling_TokenRecordLedgerHasNoUnit(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())

	rec := billableRecord("r1", "w1", 5)
	rec.Receipt = &RecordReceipt{Pubkey: "pk", Sig: "sig-r1"}
	writeRequestLog(t, filepath.Join(dir, "requests.jsonl"), []RequestRecord{rec})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(6))
	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("settle: %v", err)
	}

	proof, err := s.BuildReceiptProof("r1")
	if err != nil {
		t.Fatal(err)
	}
	if proof.PriceUnit != "" {
		t.Errorf("a token proof must not carry a price_unit, got %q", proof.PriceUnit)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "merkle-batches.jsonl"))
	for _, line := range splitLines(raw) {
		var b merkleBatchRecord
		if json.Unmarshal([]byte(line), &b) != nil {
			continue
		}
		for _, l := range b.Leaves {
			if l.PriceUnit != "" {
				t.Errorf("a token leaf must not persist a price_unit, got %q", l.PriceUnit)
			}
		}
	}
}

// The billing unit comes from CONFIG, never from the record: a worker that labels its
// own receipt for a model the catalog prices per token must not change how it is billed.
func TestAssetBilling_UnitComesFromConfigNotRecord(t *testing.T) {
	cfg := imageCfg()
	// A model the catalog does not declare as an image model is a token model, whatever
	// a receipt for it claims.
	r := assetRecord("weird1", "w1", "some-text-model", 1, "h")
	if got := cfg.UnitFor(r.Model); !got.IsToken() {
		t.Fatalf("a model with no catalog image unit must be billed per token, got %q", got)
	}
	if got := cfg.UnitFor("flux-schnell"); got != PriceUnitImage {
		t.Fatalf("flux-schnell must be billed per image, got %q", got)
	}
}

func splitLines(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, string(b[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

// TestReceiptlessImageStillSettles: a worker that returns images but no receipt. The
// gateway reserved and adjusted the balance by the DELIVERED count, so settlement must
// bill the same count — reading the quantity from the (absent) receipt made it settle
// as zero: the user's pendingSpend was never drained and the SP was never paid.
func TestReceiptlessImageStillSettles(t *testing.T) {
	cfg := imageCfg()
	rec := RequestRecord{
		RequestID: "img-norcpt", Timestamp: time.Now(), Wallet: walletU, WorkerID: "w1",
		Model: "flux-schnell", Status: 200, Path: "/v1/images/generations",
		Quantity: 2, AssetHash: "ab12",
	}
	s := &Scanner{}
	if !s.isBillable(rec) {
		t.Fatal("a delivered image without a receipt must still be billable")
	}
	agg := NewAggregator(cfg, map[string]string{"w1": "miner1"}, discardLogger())
	got := agg.recordCostUSD(rec)
	if f, _ := got.Float64(); f != 4 { // 2 images × $2
		t.Fatalf("receipt-less image priced at %v, want $4 (2 × $2)", got)
	}
	// The Merkle leaf commits the gateway's own count and digest.
	byID := map[string]RequestRecord{rec.RequestID: rec}
	item := SettlementItem{UserWallet: walletU, SPEVM: common.HexToAddress(sp1Addr), RequestIDs: []string{rec.RequestID}}
	_, leaves := batchLeaves([]SettlementItem{item}, byID, cfg.UnitFor)
	want := AssetLeaf(rec.RequestID, walletU, common.HexToAddress(sp1Addr).Hex(), rec.Model,
		PriceUnitImage, 2, "ab12", "")
	if len(leaves) != 1 || leaves[0] != want {
		t.Fatal("receipt-less asset record must commit an asset leaf built from the gateway's count and digest")
	}
}

// TestSignedReceiptStillDecidesTheLeaf: with a signed asset receipt, the leaf is built
// from the receipt exactly as attested — that is what an external verifier can rebuild.
func TestSignedReceiptStillDecidesTheLeaf(t *testing.T) {
	cfg := imageCfg()
	rec := assetRecord("img-signed", "w1", "flux-schnell", 1, "c0ffee")
	rec.Quantity, rec.AssetHash = 1, "c0ffee"
	byID := map[string]RequestRecord{rec.RequestID: rec}
	item := SettlementItem{UserWallet: walletU, SPEVM: common.HexToAddress(sp1Addr), RequestIDs: []string{rec.RequestID}}
	_, leaves := batchLeaves([]SettlementItem{item}, byID, cfg.UnitFor)
	want := AssetLeaf(rec.RequestID, walletU, common.HexToAddress(sp1Addr).Hex(), rec.Model,
		PriceUnitImage, 1, "c0ffee", rec.Receipt.Sig)
	if leaves[0] != want {
		t.Fatal("signed receipt must still determine the asset leaf")
	}
}
