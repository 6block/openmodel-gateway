package settlement

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// restartSettler builds a fresh Settler over an EXISTING data dir — i.e. it models a
// process restart: cold in-memory indexes (merkleWarm=false), everything else on disk.
func restartSettler(t *testing.T, dir string, mock *mockContract) *Settler {
	t.Helper()
	reqLog := filepath.Join(dir, "requests.jsonl")
	cfg := coverageCfg()
	cfg.MaxBatchSize = 50
	cfg.OperatorMinBalance = "0.1"
	cfg.FILPriceUSD = "2.0"
	cfg.FILPriceSource = "manual"
	cfg.IntervalMinutes = 60
	pricer := NewPricer(cfg, discardLogger())
	bc := NewBalanceCache(nil, cfg.SupportedTokens, pricer, 30, discardLogger())
	return NewSettler(cfg, mock, pricer, bc, nil, reqLog, dir, discardLogger())
}

// settleRID appends one billable record (with a worker receipt) and runs one Settle, so a
// single merkle batch containing rid is committed and persisted.
func settleRID(t *testing.T, s *Settler, dir, rid string) {
	t.Helper()
	rec := billableRecord(rid, "w1", 5)
	rec.Receipt = &RecordReceipt{Pubkey: "pk", Sig: "sig-" + rid}
	writeRequestLog(t, filepath.Join(dir, "requests.jsonl"), []RequestRecord{rec})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(6))
	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("settle %s: %v", rid, err)
	}
}

// R4/P1: the merkle batch line must be THIN — the request record (and its receipt sig)
// lives only in the external receipt-records.jsonl, not embedded in the ledger line.
func TestR4_LedgerLineThin_RecordsExternalized(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	settleRID(t, s, dir, "r1")

	ledger, err := os.ReadFile(filepath.Join(dir, "merkle-batches.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ledger, []byte(`"record"`)) || bytes.Contains(ledger, []byte("sig-r1")) {
		t.Fatalf("R4: batch line must not embed the record; got %s", ledger)
	}
	recStore, err := os.ReadFile(filepath.Join(dir, "receipt-records.jsonl"))
	if err != nil {
		t.Fatalf("R4: receipt-records.jsonl must exist: %v", err)
	}
	if !bytes.Contains(recStore, []byte("sig-r1")) {
		t.Fatal("R4: the record (with receipt sig) must live in receipt-records.jsonl")
	}
	// The proof still resolves the full record via the external store.
	proof, err := s.BuildReceiptProof("r1")
	if err != nil {
		t.Fatal(err)
	}
	if proof.Record == nil {
		t.Fatal("proof must carry the record resolved from the external store")
	}
}

// R4/P0: with a warm index an unknown request_id is an authoritative "not settled" — it
// must NOT fall back to scanning the whole ledger (the removed ~56s DoS path).
func TestR4_UnknownRid_NotFoundNoScan(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	settleRID(t, s, dir, "r1")

	if _, err := s.BuildReceiptProof("r1"); err != nil { // warms the index
		t.Fatalf("known rid must resolve: %v", err)
	}
	if _, err := s.BuildReceiptProof("nope"); err == nil {
		t.Fatal("unknown rid must return an error, not a proof")
	}
	// Prove the miss did not consult the ledger file: delete it, and an unknown rid still
	// short-circuits on the warm index (a scan would have to open the now-missing file).
	if err := os.Remove(filepath.Join(dir, "merkle-batches.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildReceiptProof("still-nope"); err == nil {
		t.Fatal("unknown rid must stay not-found via the index after the ledger is gone")
	}
}

// R4/P2: a restart replays the sidecar (fast path) — the proof resolves and the sidecar is
// NOT rewritten (a rebuild would rewrite it).
func TestR4_Restart_ReplaysSidecar(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	settleRID(t, s, dir, "r1")

	sidecar := filepath.Join(dir, "receipt-index.jsonl")
	before, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("sidecar must exist after settle: %v", err)
	}

	s2 := restartSettler(t, dir, mock)
	proof, err := s2.BuildReceiptProof("r1")
	if err != nil {
		t.Fatalf("restart proof must resolve from the sidecar: %v", err)
	}
	if proof.Record == nil {
		t.Fatal("restart proof must carry the record")
	}
	after, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("sidecar replay (fast path) must not rewrite the sidecar")
	}
}

// R4/P2: if the sidecar is missing, the index is rebuilt from the data files and the
// sidecar is rewritten for the next start.
func TestR4_Restart_RebuildsWhenSidecarMissing(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	settleRID(t, s, dir, "r1")

	sidecar := filepath.Join(dir, "receipt-index.jsonl")
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}
	s2 := restartSettler(t, dir, mock)
	if _, err := s2.BuildReceiptProof("r1"); err != nil {
		t.Fatalf("must rebuild from data files when the sidecar is gone: %v", err)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("rebuild must rewrite the sidecar: %v", err)
	}
}

// R4/P2: an integrity guard — if the data files grew past what the sidecar accounts for (a
// crash between the ledger append and the sidecar append), the stale sidecar is rejected
// and the index rebuilt, so nothing is silently lost.
func TestR4_Restart_RebuildsWhenSidecarStale(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	settleRID(t, s, dir, "r1")

	// Grow the ledger without updating the sidecar → size mismatch on replay.
	f, err := os.OpenFile(filepath.Join(dir, "merkle-batches.jsonl"), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("\n"))
	_ = f.Close()

	s2 := restartSettler(t, dir, mock)
	if _, err := s2.BuildReceiptProof("r1"); err != nil {
		t.Fatalf("stale sidecar must trigger a rebuild; r1 still resolvable: %v", err)
	}
}

// R4 backward-compat: a pre-R4 "fat" batch line (record embedded, no external store, no
// sidecar) — the exact on-disk shape of the pre-migration ledger — must still resolve, with
// the record served from the embedded leaf.
func TestR4_BackwardCompat_FatLineEmbeddedRecord(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())

	rec := billableRecord("old1", "w1", 5)
	rec.Receipt = &RecordReceipt{Pubkey: "pk", Sig: "sig-old1"}
	leafHash := ReceiptLeaf("old1", walletU, common.HexToAddress(sp1Addr).Hex(),
		"default", rec.PromptTokens, rec.CompletionTokens, rec.CachedTokens, "sig-old1")
	rc := rec
	batch := merkleBatchRecord{
		DetailsHash: "dh", LegacyHash: "lh",
		MerkleRoot: hex.EncodeToString(leafHash[:]), // single leaf → root == leaf
		TxHash:     "0xtx", BlockNumber: 1,
		Leaves: []merkleLeaf{{
			Rid: "old1", Leaf: hex.EncodeToString(leafHash[:]),
			SP: common.HexToAddress(sp1Addr).Hex(), Record: &rc,
		}},
	}
	line, _ := json.Marshal(batch)
	if err := os.WriteFile(filepath.Join(dir, "merkle-batches.jsonl"), append(line, '\n'), 0644); err != nil {
		t.Fatal(err)
	}

	proof, err := s.BuildReceiptProof("old1")
	if err != nil {
		t.Fatalf("pre-R4 fat line must still resolve: %v", err)
	}
	got, ok := proof.Record.(RequestRecord)
	if !ok || got.Receipt == nil || got.Receipt.Sig != "sig-old1" {
		t.Fatalf("pre-R4 record must come from the embedded leaf; got %#v", proof.Record)
	}
}

// writeFatBatch appends one pre-R4 "fat" batch line (records embedded) to the ledger.
func writeFatBatch(t *testing.T, dir string, rids ...string) {
	t.Helper()
	var leaves []merkleLeaf
	for _, rid := range rids {
		rec := billableRecord(rid, "w1", 5)
		rec.Receipt = &RecordReceipt{Pubkey: "pk", Sig: "sig-" + rid}
		lh := ReceiptLeaf(rid, walletU, common.HexToAddress(sp1Addr).Hex(),
			"default", rec.PromptTokens, rec.CompletionTokens, rec.CachedTokens, "sig-"+rid)
		rc := rec
		leaves = append(leaves, merkleLeaf{
			Rid: rid, Leaf: hex.EncodeToString(lh[:]),
			SP: common.HexToAddress(sp1Addr).Hex(), Record: &rc,
		})
	}
	root := MerkleRoot(func() [][32]byte {
		out := make([][32]byte, len(leaves))
		for i, l := range leaves {
			out[i], _ = hexToHash32(l.Leaf)
		}
		return out
	}())
	line, _ := json.Marshal(merkleBatchRecord{
		DetailsHash: "dh-" + rids[0], LegacyHash: "lh", MerkleRoot: hex.EncodeToString(root[:]),
		TxHash: "0xtx", BlockNumber: 1, Leaves: leaves,
	})
	f, err := os.OpenFile(filepath.Join(dir, "merkle-batches.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
}

// R4 migration: a fat ledger is converted to a thin ledger + external record store +
// sidecar; the original is preserved; a fresh settler then serves proofs via the sidecar
// fast path (no rebuild); and re-running the migration is refused.
func TestR4_MigrateFatLedger(t *testing.T) {
	mock := newMockContract()
	_, dir := newTestSettler(t, mock, discardLogger())
	writeFatBatch(t, dir, "a1", "a2") // batch 1: two leaves
	writeFatBatch(t, dir, "b1")       // batch 2: one leaf

	stats, err := MigrateFatLedger(dir)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if stats.Batches != 2 || stats.Leaves != 3 || stats.RecordsMoved != 3 {
		t.Fatalf("stats: %+v (want 2 batches / 3 leaves / 3 records)", stats)
	}
	if stats.NewLedgerSize >= stats.OldLedgerSize {
		t.Fatalf("thin ledger (%d) must be smaller than fat (%d)", stats.NewLedgerSize, stats.OldLedgerSize)
	}

	ledger, _ := os.ReadFile(filepath.Join(dir, "merkle-batches.jsonl"))
	if bytes.Contains(ledger, []byte(`"record"`)) || bytes.Contains(ledger, []byte("sig-a1")) {
		t.Fatal("migrated ledger must be thin (no embedded records)")
	}
	recStore, _ := os.ReadFile(filepath.Join(dir, "receipt-records.jsonl"))
	for _, rid := range []string{"a1", "a2", "b1"} {
		if !bytes.Contains(recStore, []byte("sig-"+rid)) {
			t.Fatalf("record store must contain %s", rid)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "merkle-batches.jsonl.pre-r4")); err != nil {
		t.Fatalf("original fat ledger must be preserved: %v", err)
	}

	// Fresh settler resolves every rid, and the sidecar is the fast path (untouched).
	sidecar := filepath.Join(dir, "receipt-index.jsonl")
	before, _ := os.ReadFile(sidecar)
	s := restartSettler(t, dir, mock)
	for _, rid := range []string{"a1", "a2", "b1"} {
		proof, perr := s.BuildReceiptProof(rid)
		if perr != nil {
			t.Fatalf("post-migration proof %s: %v", rid, perr)
		}
		got, ok := proof.Record.(RequestRecord)
		if !ok || got.Receipt == nil || got.Receipt.Sig != "sig-"+rid {
			t.Fatalf("post-migration record %s must carry the receipt; got %#v", rid, proof.Record)
		}
	}
	if after, _ := os.ReadFile(sidecar); !bytes.Equal(before, after) {
		t.Fatal("post-migration proofs must use the sidecar fast path (no rebuild/rewrite)")
	}

	// Re-running the migration must be refused (records store now present).
	if _, err := MigrateFatLedger(dir); err == nil {
		t.Fatal("re-running migration on an already-migrated dir must error")
	}
}

// R4: an ancient leaf that carries neither an sp field nor an embedded record (the oldest
// pre-A1-sp history) must return the proof FAST with an empty SP — it must NOT fall back to
// scanning the multi-GB items ledger. We prove the fallback is gone by seeding the items
// ledger with an SP for this rid and asserting the proof does NOT pick it up.
func TestR4_AncientLeaf_NoItemsLedgerScanForSP(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())

	// A thin batch line whose single leaf has only {rid, leaf} — no sp, no record.
	lh := ReceiptLeaf("anc1", walletU, "", "default", 5, 0, 0, "")
	line, _ := json.Marshal(merkleBatchRecord{
		DetailsHash: "dh", LegacyHash: "lh", MerkleRoot: hex.EncodeToString(lh[:]),
		TxHash: "0xtx", BlockNumber: 1,
		Leaves: []merkleLeaf{{Rid: "anc1", Leaf: hex.EncodeToString(lh[:])}},
	})
	if err := os.WriteFile(filepath.Join(dir, "merkle-batches.jsonl"), append(line, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	// Items ledger DOES have an SP for anc1 — the old code would have returned it.
	item, _ := json.Marshal(settlementItemRecord{RequestID: "anc1", SPEVM: "0xSHOULD_NOT_APPEAR"})
	if err := os.WriteFile(filepath.Join(dir, "settlement-items.jsonl"), append(item, '\n'), 0644); err != nil {
		t.Fatal(err)
	}

	proof, err := s.BuildReceiptProof("anc1")
	if err != nil {
		t.Fatalf("ancient leaf must still yield a proof: %v", err)
	}
	if proof.SP != "" {
		t.Fatalf("SP must stay empty (no items-ledger scan); got %q", proof.SP)
	}
	if proof.Record != nil {
		t.Fatalf("ancient leaf has no record; got %#v", proof.Record)
	}
	if proof.Leaf != hex.EncodeToString(lh[:]) {
		t.Fatal("proof leaf must match the ledger leaf")
	}
}
