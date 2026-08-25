package settlement

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// ---- Schema-3 ABI plumbing ----

// TestGetSettlementUnpackV3 mirrors TestGetSettlementUnpack for the v1.3 tuple:
// the two stats fields must round-trip and land in OnChainSettlement.
func TestGetSettlementUnpackV3(t *testing.T) {
	parsed, err := abi.JSON(strings.NewReader(contractABIV3JSON))
	if err != nil {
		t.Fatal(err)
	}
	method, ok := parsed.Methods["getSettlement"]
	if !ok {
		t.Fatal("getSettlement not in v3 ABI")
	}
	var dh [32]byte
	for i := range dh {
		dh[i] = 0xCD
	}
	type recIn struct {
		BatchId      *big.Int
		Timestamp    *big.Int
		TotalAmount  *big.Int
		SettledCount *big.Int
		FailedCount  *big.Int
		DetailsHash  [32]byte
		RequestCount *big.Int
		TokenCount   *big.Int
	}
	packed, err := method.Outputs.Pack(recIn{
		BatchId:      big.NewInt(9),
		Timestamp:    big.NewInt(777),
		TotalAmount:  big.NewInt(4200),
		SettledCount: big.NewInt(2),
		FailedCount:  big.NewInt(0),
		DetailsHash:  dh,
		RequestCount: big.NewInt(15),
		TokenCount:   big.NewInt(98765),
	})
	if err != nil {
		t.Fatalf("pack outputs: %v", err)
	}
	out, err := unpackSettlement(parsed, 3, packed)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if out.BatchId.Int64() != 9 || out.RequestCount == nil || out.TokenCount == nil {
		t.Fatalf("v3 fields missing: %+v", out)
	}
	if out.RequestCount.Int64() != 15 || out.TokenCount.Int64() != 98765 {
		t.Errorf("stats mapping wrong: req=%v tok=%v", out.RequestCount, out.TokenCount)
	}
}

// TestSubmitSettlementPackV3 pins the 7-arg calldata shape: the v3 ABI must accept
// the stats arrays and reject the 5-arg legacy shape (selector discipline — this is
// exactly what protects a mis-configured schema from silently half-working).
func TestSubmitSettlementPackV3(t *testing.T) {
	v3, err := abi.JSON(strings.NewReader(contractABIV3JSON))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := abi.JSON(strings.NewReader(contractABIJSON))
	if err != nil {
		t.Fatal(err)
	}
	users := []common.Address{common.HexToAddress(walletU)}
	sps := []common.Address{common.HexToAddress(sp1Addr)}
	amounts := []*big.Int{big.NewInt(100)}
	tokens := []common.Address{common.HexToAddress(filAddr)}
	reqs := []*big.Int{big.NewInt(3)}
	toks := []*big.Int{big.NewInt(450)}
	var dh [32]byte

	if _, err := v3.Pack("submitSettlement", users, sps, amounts, tokens, reqs, toks, dh); err != nil {
		t.Fatalf("v3 ABI must pack 7 args: %v", err)
	}
	if _, err := v3.Pack("submitSettlement", users, sps, amounts, tokens, dh); err == nil {
		t.Fatal("v3 ABI accepted the 5-arg shape — schema separation broken")
	}
	if _, err := v2.Pack("submitSettlement", users, sps, amounts, tokens, dh); err != nil {
		t.Fatalf("v2 ABI must keep packing 5 args: %v", err)
	}
	// The two generations must not share a selector: a schema-2 client calling a
	// v1.3 contract has to revert loudly, not alias onto some other function.
	m3, m2 := v3.Methods["submitSettlement"], v2.Methods["submitSettlement"]
	if string(m3.ID) == string(m2.ID) {
		t.Fatal("v2 and v3 submitSettlement selectors are identical")
	}
}

// TestSettlementExecutedTopicsDiffer pins that the event signature changed in v3, so
// each client generation only recognizes its own contract's logs.
func TestSettlementExecutedTopicsDiffer(t *testing.T) {
	v3, _ := abi.JSON(strings.NewReader(contractABIV3JSON))
	v2, _ := abi.JSON(strings.NewReader(contractABIJSON))
	if v3.Events["SettlementExecuted"].ID == v2.Events["SettlementExecuted"].ID {
		t.Fatal("SettlementExecuted topic did not change between schemas")
	}
}

// ---- Per-item stats computation (buildPending) ----

func statsItem(sp string, amount int64, rids ...string) SettlementItem {
	return SettlementItem{
		UserWallet: walletU,
		UserEVM:    common.HexToAddress(walletU),
		SPEVM:      common.HexToAddress(sp),
		Amount:     big.NewInt(amount),
		TokenAddr:  common.HexToAddress(filAddr),
		RequestIDs: rids,
	}
}

// TestBuildPendingItemStats: tokens come from the records (prompt+completion), a
// debt rid without a record counts 1 request / 0 tokens.
func TestBuildPendingItemStats(t *testing.T) {
	s, _ := newTestSettler(t, newMockContract(), discardLogger())
	records := map[string]RequestRecord{
		"req-a": {RequestID: "req-a", Wallet: walletU, PromptTokens: 100, CompletionTokens: 20, CachedTokens: 90},
		"req-b": {RequestID: "req-b", Wallet: walletU, PromptTokens: 7, CompletionTokens: 3},
	}
	items := []SettlementItem{statsItem(sp1Addr, 5, "req-a", "req-b", "req-debt")}
	p := s.buildPending(items, Cursor{}, nil, nil, records)
	if len(p.Batches) != 1 || len(p.Batches[0].Items) != 1 {
		t.Fatalf("unexpected batch shape: %+v", p.Batches)
	}
	it := p.Batches[0].Items[0]
	if it.RequestCount != 3 {
		t.Errorf("request count: want 3 (incl. recordless debt rid), got %d", it.RequestCount)
	}
	// 100+20 + 7+3 + 0 — cached tokens are a subset of prompt tokens, never added.
	if it.TokenCount != 130 {
		t.Errorf("token count: want 130, got %d", it.TokenCount)
	}
}

// TestBuildPendingSplitRidCountedOnce: a rid split across two items (token
// spillover) is attributed to the FIRST item only — otherwise the same request
// would inflate the on-chain stats twice.
func TestBuildPendingSplitRidCountedOnce(t *testing.T) {
	s, _ := newTestSettler(t, newMockContract(), discardLogger())
	records := map[string]RequestRecord{
		"req-split": {RequestID: "req-split", Wallet: walletU, PromptTokens: 50, CompletionTokens: 10},
		"req-own":   {RequestID: "req-own", Wallet: walletU, PromptTokens: 5, CompletionTokens: 5},
	}
	items := []SettlementItem{
		statsItem(sp1Addr, 5, "req-split"),
		statsItem(sp2Addr, 5, "req-split", "req-own"),
	}
	p := s.buildPending(items, Cursor{}, nil, nil, records)
	its := p.Batches[0].Items
	if its[0].RequestCount != 1 || its[0].TokenCount != 60 {
		t.Errorf("item0: want 1/60, got %d/%d", its[0].RequestCount, its[0].TokenCount)
	}
	if its[1].RequestCount != 1 || its[1].TokenCount != 10 {
		t.Errorf("item1: want 1/10 (split rid excluded), got %d/%d", its[1].RequestCount, its[1].TokenCount)
	}
	// Invariant: total attributed requests == number of merkle leaves.
	total := its[0].RequestCount + its[1].RequestCount
	if total != len(p.Batches[0].Leaves) {
		t.Errorf("stats/leaves divergence: %d attributed vs %d leaves", total, len(p.Batches[0].Leaves))
	}
}

// TestToBatchCarriesStats: the WAL round-trip must hand the stats to the contract
// call as equal-length big.Int arrays, zero-filled for pre-v1.3 WAL entries.
func TestToBatchCarriesStats(t *testing.T) {
	pb := pendingBatch{
		DetailsHash: strings.Repeat("ab", 32),
		Items: []pendingItem{
			{User: walletU, SP: sp1Addr, Amount: "100", Token: filAddr, RequestCount: 4, TokenCount: 999},
			{User: walletU, SP: sp2Addr, Amount: "200", Token: filAddr}, // old WAL entry: no stats
		},
	}
	var dh [32]byte
	b, err := pb.toBatch(dh)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.RequestCounts) != 2 || len(b.TokenCounts) != 2 {
		t.Fatalf("stats arrays must match item count: %d/%d", len(b.RequestCounts), len(b.TokenCounts))
	}
	if b.RequestCounts[0].Int64() != 4 || b.TokenCounts[0].Int64() != 999 {
		t.Errorf("item0 stats lost: %v/%v", b.RequestCounts[0], b.TokenCounts[0])
	}
	if b.RequestCounts[1].Int64() != 0 || b.TokenCounts[1].Int64() != 0 {
		t.Errorf("old WAL entry must submit zeros, got %v/%v", b.RequestCounts[1], b.TokenCounts[1])
	}
}
