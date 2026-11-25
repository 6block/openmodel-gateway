package settlement

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
)

// merkle.go — A1's Merkle batch commitment.
//
// Every settlement batch commits a Merkle root over PER-REQUEST leaves into the
// on-chain detailsHash, WITHOUT any contract change:
//
//	detailsHash(on-chain) = sha256( legacyBatchHash ‖ merkleRoot )
//
// legacyBatchHash is the proven dedup identity (BatchHash: items + request IDs,
// cursor-independent); the combined hash keeps every dedup/crash-replay property
// (fully deterministic from batch content) while binding each individual request —
// including its worker-signed receipt — to the on-chain record. A user holding a
// receipt can verify offline:
//
//	1. leaf     == sha256(canonical leaf JSON of their request)
//	2. VerifyMerkleProof(leaf, idx, proof, root)
//	3. sha256(legacy ‖ root) == detailsHash stored on-chain (getSettlement)
//
// so the operator cannot settle a batch that misrepresents their usage without it
// being detectable.

// ReceiptLeaf is the canonical leaf for a token-billed record. Fixed-template JSON
// (field order hardcoded) — documented for external verifiers; sig is the worker's
// receipt signature (empty when the worker presented none: the leaf then only binds
// the gateway-asserted values).
//
// Byte format is frozen: external verifiers rebuild this exactly, so this shape must
// never change. Non-token models get their own leaf (AssetLeaf) rather than extra
// fields here, so a token model's commitment is identical before and after unit
// generalisation.
func ReceiptLeaf(rid, wallet, spEVM, model string, promptTokens, completionTokens, cachedTokens int, sig string) [32]byte {
	js := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	payload := fmt.Sprintf(
		`{"cached_tokens":%d,"completion_tokens":%d,"model":%s,"prompt_tokens":%d,"request_id":%s,"sig":%s,"sp":%s,"wallet":%s}`,
		cachedTokens, completionTokens, js(model), promptTokens, js(rid), js(sig), js(spEVM), js(wallet))
	return sha256.Sum256([]byte(payload))
}

// AssetLeaf is the canonical leaf for a record billed in a NON-token unit (image,
// audio_second). Fixed-template JSON, keys in ASCII order — a separate shape, not
// ReceiptLeaf plus appended fields.
//
// Why a separate shape: a non-token record has no token counts, and writing zeros for
// them would commit a fictitious measurement to the on-chain record. The leaf instead
// binds what the unit is and how much of it was produced, plus asset_hash — the digest
// of the produced bytes, which is what a verifier can recompute from the asset itself.
//
//	{"asset_hash":…,"model":…,"price_unit":…,"quantity":N,"request_id":…,"sig":…,"sp":…,"wallet":…}
func AssetLeaf(rid, wallet, spEVM, model string, unit PriceUnit, quantity int, assetHash, sig string) [32]byte {
	js := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	payload := fmt.Sprintf(
		`{"asset_hash":%s,"model":%s,"price_unit":%s,"quantity":%d,"request_id":%s,"sig":%s,"sp":%s,"wallet":%s}`,
		js(assetHash), js(model), js(string(EffectiveUnit(unit))), quantity, js(rid), js(sig), js(spEVM), js(wallet))
	return sha256.Sum256([]byte(payload))
}

// DebtLeaf is the leaf for a carried-debt request ID whose original record is no
// longer at hand (only identity is known).
func DebtLeaf(rid, wallet, spEVM string) [32]byte {
	js := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	payload := fmt.Sprintf(`{"debt":true,"request_id":%s,"sp":%s,"wallet":%s}`,
		js(rid), js(spEVM), js(wallet))
	return sha256.Sum256([]byte(payload))
}

// MerkleRoot computes the root of a binary sha256 tree. An odd node at any level is
// paired with itself. Empty input → zero hash.
func MerkleRoot(leaves [][32]byte) [32]byte {
	if len(leaves) == 0 {
		return [32]byte{}
	}
	level := make([][32]byte, len(leaves))
	copy(level, leaves)
	for len(level) > 1 {
		var next [][32]byte
		for i := 0; i < len(level); i += 2 {
			j := i + 1
			if j == len(level) {
				j = i // odd → pair with itself
			}
			next = append(next, hashPair(level[i], level[j]))
		}
		level = next
	}
	return level[0]
}

// MerkleProofFor returns the sibling path for the leaf at idx.
func MerkleProofFor(leaves [][32]byte, idx int) [][32]byte {
	if idx < 0 || idx >= len(leaves) {
		return nil
	}
	var proof [][32]byte
	level := make([][32]byte, len(leaves))
	copy(level, leaves)
	for len(level) > 1 {
		sib := idx ^ 1
		if sib >= len(level) {
			sib = idx // odd node pairs with itself
		}
		proof = append(proof, level[sib])
		var next [][32]byte
		for i := 0; i < len(level); i += 2 {
			j := i + 1
			if j == len(level) {
				j = i
			}
			next = append(next, hashPair(level[i], level[j]))
		}
		level = next
		idx /= 2
	}
	return proof
}

// VerifyMerkleProof re-derives the root from a leaf + sibling path.
func VerifyMerkleProof(leaf [32]byte, idx int, proof [][32]byte, root [32]byte) bool {
	h := leaf
	for _, sib := range proof {
		if idx%2 == 0 {
			h = hashPair(h, sib)
		} else {
			h = hashPair(sib, h)
		}
		idx /= 2
	}
	return h == root
}

// CombinedDetailsHash binds the legacy dedup hash and the receipt Merkle root into
// the single bytes32 the contract stores.
func CombinedDetailsHash(legacy, root [32]byte) [32]byte {
	buf := make([]byte, 64)
	copy(buf, legacy[:])
	copy(buf[32:], root[:])
	return sha256.Sum256(buf)
}

func hashPair(a, b [32]byte) [32]byte {
	buf := make([]byte, 64)
	copy(buf, a[:])
	copy(buf[32:], b[:])
	return sha256.Sum256(buf)
}

// batchLeaves builds the deterministic (request-id-sorted, deduplicated) leaf set for
// one batch of settlement items. Records supply full leaf data; a rid without a record
// (carried debt from an earlier cycle) degrades to an identity-only DebtLeaf.
// unitOf reports the billing unit a model is priced in, from the settlement config.
// It is passed in rather than looked up inside batchLeaves so the leaf builder stays a
// pure function of its inputs — the unit comes from CONFIGURATION, never from the
// record or the receipt.
type unitOf func(model string) PriceUnit

// batchLeaves builds the deterministic (request-id-sorted, deduplicated) leaf set for
// a batch. Each record's leaf shape follows its model's catalog unit: a token model
// commits the frozen ReceiptLeaf, an asset-billed model (image, audio_second) commits
// an AssetLeaf carrying the unit, the quantity and the asset digest.
func batchLeaves(batch []SettlementItem, recordsByID map[string]RequestRecord, unitFor unitOf) (rids []string, leaves [][32]byte) {
	seen := make(map[string]bool)
	type ent struct {
		rid  string
		leaf [32]byte
	}
	var ents []ent
	for _, it := range batch {
		for _, rid := range it.RequestIDs {
			if seen[rid] {
				continue // a rid may appear in two items when split across tokens
			}
			seen[rid] = true
			var leaf [32]byte
			if rec, ok := recordsByID[rid]; ok {
				sig := ""
				if rec.Receipt != nil {
					sig = rec.Receipt.Sig
				}
				// A signed asset receipt is committed exactly as attested, so an
				// external verifier rebuilds the leaf from the receipt alone. Without
				// one, the gateway's own count and digest are committed — a delivered
				// image must not drop out of the tree because the worker sent no
				// receipt.
				quantity := quantityOf(rec)
				assetHash := assetHashOf(rec)
				if rec.Receipt != nil && rec.Receipt.Quantity > 0 {
					quantity = rec.Receipt.Quantity
					assetHash = rec.Receipt.AssetHash
				}
				unit := PriceUnitToken
				if unitFor != nil {
					unit = EffectiveUnit(unitFor(rec.Model))
				}
				if unit.IsToken() {
					leaf = ReceiptLeaf(rid, rec.Wallet, it.SPEVM.Hex(), rec.Model,
						rec.PromptTokens, rec.CompletionTokens, rec.CachedTokens, sig)
				} else {
					leaf = AssetLeaf(rid, rec.Wallet, it.SPEVM.Hex(), rec.Model,
						unit, quantity, assetHash, sig)
				}
			} else {
				leaf = DebtLeaf(rid, it.UserWallet, it.SPEVM.Hex())
			}
			ents = append(ents, ent{rid, leaf})
		}
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].rid < ents[j].rid })
	for _, e := range ents {
		rids = append(rids, e.rid)
		leaves = append(leaves, e.leaf)
	}
	return rids, leaves
}

// ReceiptProof is everything an external verifier needs to check that one request was
// settled inside an on-chain batch (served by the public receipt endpoint).
type ReceiptProof struct {
	RequestID string `json:"request_id"`
	SP        string `json:"sp"`               // SP EVM payout address (leaf component)
	Record    any    `json:"record,omitempty"` // the billing-ledger row (incl. worker receipt)
	Leaf      string `json:"leaf"`             // hex leaf hash
	// PriceUnit is set for a non-token record (image, audio_second): the leaf commits
	// the unit, so a verifier needs it to rebuild the leaf bytes. Absent for a token
	// record, whose shape implies the unit.
	PriceUnit   string   `json:"price_unit,omitempty"`
	LeafIndex   int      `json:"leaf_index"`
	Proof       []string `json:"proof"` // hex sibling path
	MerkleRoot  string   `json:"merkle_root"`
	LegacyHash  string   `json:"legacy_hash"`  // sha256(legacy ‖ root) must equal DetailsHash
	DetailsHash string   `json:"details_hash"` // the value stored on-chain (getSettlement)
	TxHash      string   `json:"tx_hash"`
	BlockNumber uint64   `json:"block_number"`
	// Verify describes the offline procedure (kept in the payload so the endpoint is
	// self-documenting for third parties).
	Verify string `json:"verify"`
}

// BuildReceiptProof assembles the inclusion proof for one settled request, or an
// error naming what is missing (not yet settled / pre-Merkle batch / unknown rid).
func (s *Settler) BuildReceiptProof(rid string) (*ReceiptProof, error) {
	batch, ok := s.findMerkleBatch(rid)
	if !ok {
		return nil, fmt.Errorf("request %s has no merkle-committed settlement (not yet settled, or settled before merkle commitments)", rid)
	}
	idx := -1
	leaves := make([][32]byte, len(batch.Leaves))
	for i, l := range batch.Leaves {
		b, err := hexToHash32(l.Leaf)
		if err != nil {
			return nil, fmt.Errorf("corrupt leaf in ledger: %w", err)
		}
		leaves[i] = b
		if l.Rid == rid {
			idx = i
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("request %s not in its batch leaf set", rid)
	}
	proof := MerkleProofFor(leaves, idx)
	hexProof := make([]string, len(proof))
	for i, p := range proof {
		hexProof[i] = fmt.Sprintf("%x", p)
	}
	// Attach the billing-ledger row (with the worker receipt) and SP. Resolution order:
	// (1) a record embedded in the leaf — pre-R4 "fat" batch lines (O(1), rotation-proof);
	// (2) the external receipt-records.jsonl store via the record index (R4 thin lines,
	//     O(1) point-read). A rid that is merkle-committed but has NO stored record — pre-F6
	//     settlements, whose request logs have since rotated away — returns the proof with a
	//     null record rather than scanning the (multi-hundred-MB) request log. That scan was
	//     the same public-port DoS as the ledger scan R4 removed; the proof itself is complete
	//     without it, only the human-readable billing row is missing for that old history.
	leaf := batch.Leaves[idx]
	var record any
	if leaf.Record != nil {
		record = *leaf.Record
	} else if rec, ok := s.loadRecordForRID(rid); ok {
		record = rec
	}
	// SP is embedded in the leaf (the sp field has been written at settlement time since A1).
	// A leaf without it is ancient history predating that field; return it empty rather than
	// scanning the (multi-GB) items ledger — that scan was the same public-port DoS as the
	// ledger and request-log scans R4 removed. SP is supplementary to the proof, not required.
	sp := leaf.SP
	return &ReceiptProof{
		RequestID:   rid,
		SP:          sp,
		Record:      record,
		Leaf:        batch.Leaves[idx].Leaf,
		PriceUnit:   leaf.PriceUnit,
		LeafIndex:   idx,
		Proof:       hexProof,
		MerkleRoot:  batch.MerkleRoot,
		LegacyHash:  batch.LegacyHash,
		DetailsHash: batch.DetailsHash,
		TxHash:      batch.TxHash,
		BlockNumber: batch.BlockNumber,
		Verify: "1) leaf == sha256 of the canonical leaf JSON (byte-level formats: the contracts repo, docs/verification.md); " +
			"2) fold leaf up the proof (sha256 pairs, index parity picks side) == merkle_root; " +
			"3) sha256(legacy_hash || merkle_root) == details_hash; " +
			"4) details_hash matches getSettlement(batchId).detailsHash on-chain (tx_hash/block above); " +
			"5) record.receipt.sig is the worker's ed25519 signature over its canonical receipt payload.",
	}, nil
}
