package settlement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestMerkle_RootAndProofsAllIndexes(t *testing.T) {
	// 1..7 leaves (odd/even shapes) — every index's proof must verify, and a wrong
	// leaf must not.
	for n := 1; n <= 7; n++ {
		var leaves [][32]byte
		for i := 0; i < n; i++ {
			leaves = append(leaves, sha256.Sum256([]byte{byte(i)}))
		}
		root := MerkleRoot(leaves)
		for i := 0; i < n; i++ {
			proof := MerkleProofFor(leaves, i)
			if !VerifyMerkleProof(leaves[i], i, proof, root) {
				t.Fatalf("n=%d idx=%d: valid proof must verify", n, i)
			}
			bad := sha256.Sum256([]byte("bogus"))
			if VerifyMerkleProof(bad, i, proof, root) {
				t.Fatalf("n=%d idx=%d: wrong leaf must NOT verify", n, i)
			}
		}
	}
}

func TestMerkle_LeafCanonicalStability(t *testing.T) {
	// The leaf hash is a published verification interface — pin it against
	// accidental format drift.
	leaf := ReceiptLeaf("req-1", "0xW", "0xSP", "m", 10, 5, 2, "sighex")
	want := sha256.Sum256([]byte(`{"cached_tokens":2,"completion_tokens":5,"model":"m","prompt_tokens":10,"request_id":"req-1","sig":"sighex","sp":"0xSP","wallet":"0xW"}`))
	if leaf != want {
		t.Fatal("ReceiptLeaf canonical format drifted — this breaks all external verifiers")
	}
	dl := DebtLeaf("req-2", "0xW", "0xSP")
	wantD := sha256.Sum256([]byte(`{"debt":true,"request_id":"req-2","sp":"0xSP","wallet":"0xW"}`))
	if dl != wantD {
		t.Fatal("DebtLeaf canonical format drifted")
	}
}

// Full cycle with records → merkle ledger persisted → BuildReceiptProof returns a
// proof that verifies end-to-end, including sha256(legacy‖root)==details_hash and
// details_hash == what was submitted on-chain (mock).
func TestMerkle_FullCycleProofVerifies(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")

	recs := []RequestRecord{billableRecord("r1", "w1", 5), billableRecord("r2", "w1", 7)}
	recs[0].Receipt = &RecordReceipt{Pubkey: "pk", Sig: "sig-r1"}
	writeRequestLog(t, reqLog, recs)
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(12))

	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("settle: %v", err)
	}

	proof, err := s.BuildReceiptProof("r1")
	if err != nil {
		t.Fatalf("proof: %v", err)
	}
	// 1) leaf folds up to root
	leaf, _ := hexToHash32(proof.Leaf)
	root, _ := hexToHash32(proof.MerkleRoot)
	sibs := make([][32]byte, len(proof.Proof))
	for i, p := range proof.Proof {
		sibs[i], _ = hexToHash32(p)
	}
	if !VerifyMerkleProof(leaf, proof.LeafIndex, sibs, root) {
		t.Fatal("inclusion proof must verify")
	}
	// 2) sha256(legacy ‖ root) == details_hash
	legacy, _ := hexToHash32(proof.LegacyHash)
	combined := CombinedDetailsHash(legacy, root)
	if hex.EncodeToString(combined[:]) != proof.DetailsHash {
		t.Fatal("combined hash must equal details_hash")
	}
	// 3) details_hash is exactly what went on-chain
	want, _ := hexToHash32(proof.DetailsHash)
	found := false
	for _, h := range mock.submitted {
		if h == want {
			found = true
		}
	}
	if !found {
		t.Fatal("details_hash must match the on-chain submitted batch hash")
	}
	// 4) the ledger row (with the worker receipt sig) rides along, and the leaf binds it
	if proof.Record == nil {
		t.Fatal("proof must carry the billing-ledger record")
	}
	// The leaf's sp is the EIP-55 form of the config's sp_address_map value —
	// reconstruct it exactly as buildPending does (common.HexToAddress(...).Hex()).
	wantLeaf := ReceiptLeaf("r1", walletU, common.HexToAddress(sp1Addr).Hex(),
		"default", recs[0].PromptTokens, recs[0].CompletionTokens, recs[0].CachedTokens, "sig-r1")
	if hex.EncodeToString(wantLeaf[:]) != proof.Leaf {
		t.Fatal("leaf must be reconstructible from the record + receipt sig (external verifier path)")
	}
}

// Determinism: identical content (fresh settler, re-scan from zero cursor) reproduces
// the identical combined details_hash — the dedup property #45/#47 depend on.
func TestMerkle_CombinedHashDeterministicAcrossRescan(t *testing.T) {
	build := func() string {
		mock := newMockContract()
		s, dir := newTestSettler(t, mock, discardLogger())
		reqLog := filepath.Join(dir, "requests.jsonl")
		recs := []RequestRecord{billableRecord("r1", "w1", 5), billableRecord("r2", "w1", 7)}
		recs[1].Receipt = &RecordReceipt{Pubkey: "pk", Sig: "sig-r2"}
		writeRequestLog(t, reqLog, recs)
		s.resolver = staticResolver{"w1": "miner1"}
		s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
		if err := s.Settle(context.Background()); err != nil {
			t.Fatalf("settle: %v", err)
		}
		if len(mock.submitted) != 1 {
			t.Fatalf("want 1 batch, got %d", len(mock.submitted))
		}
		return hex.EncodeToString(mock.submitted[0][:])
	}
	if a, b := build(), build(); a != b {
		t.Fatalf("combined details_hash must be content-deterministic: %s != %s", a, b)
	}
}

// F6/R4: after settlement the receipt store is self-sufficient — BuildReceiptProof serves
// the record (from the external receipt-records.jsonl store, R4) and SP (from the leaf) via
// the warmed offset index, so it must still succeed with the request log AND items ledger
// deleted (the scans it used to depend on). This is the regression for the multi-second
// full-scan receipt-proof under a large log.
func TestMerkle_ReceiptProofSelfSufficient_NoLogScan(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")

	recs := []RequestRecord{billableRecord("r1", "w1", 5), billableRecord("r2", "w1", 7)}
	recs[0].Receipt = &RecordReceipt{Pubkey: "pk", Sig: "sig-r1"}
	writeRequestLog(t, reqLog, recs)
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("settle: %v", err)
	}

	// Simulate a restart (cold index) AND remove every legacy scan source. If the proof
	// still resolves, it can ONLY have come from the self-sufficient merkle ledger.
	s.merkleIdxMu.Lock()
	s.merkleIdx = nil
	s.merkleIdxMu.Unlock()
	if err := os.Remove(reqLog); err != nil {
		t.Fatalf("remove request log: %v", err)
	}
	_ = os.Remove(s.itemsLedgerPath) // items ledger: the SP fallback source

	proof, err := s.BuildReceiptProof("r1")
	if err != nil {
		t.Fatalf("proof must resolve with no logs on disk (F6): %v", err)
	}
	if proof.Record == nil {
		t.Fatal("record must resolve from the external receipt store, not the request log")
	}
	rec, ok := proof.Record.(RequestRecord)
	if !ok || rec.Receipt == nil || rec.Receipt.Sig != "sig-r1" {
		t.Fatalf("resolved record must carry the worker receipt; got %#v", proof.Record)
	}
	if proof.SP != common.HexToAddress(sp1Addr).Hex() {
		t.Fatalf("SP must come from the leaf, got %q", proof.SP)
	}
	// The warm scan must have populated the offset index (the O(1) seek path for the next call).
	s.merkleIdxMu.Lock()
	_, indexed := s.merkleIdx["r1"]
	s.merkleIdxMu.Unlock()
	if !indexed {
		t.Fatal("first proof must warm the rid→offset index")
	}

	// Leaf still reconstructs from the resolved record (external-verifier path intact).
	wantLeaf := ReceiptLeaf("r1", walletU, common.HexToAddress(sp1Addr).Hex(),
		"default", rec.PromptTokens, rec.CompletionTokens, rec.CachedTokens, "sig-r1")
	if hex.EncodeToString(wantLeaf[:]) != proof.Leaf {
		t.Fatal("leaf must be reconstructible from the resolved record")
	}
}
