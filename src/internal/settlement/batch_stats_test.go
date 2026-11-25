package settlement

import (
	"encoding/hex"
	"fmt"
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

// TestGetSettlementUnpackV4 mirrors the v3 test for the v1.4 tuple: the per-unit volume
// ARRAY must round-trip (ConvertType has to map a uint256[UNIT_COUNT] onto a [UnitCount]*big.Int field —
// a shape mismatch here would leave every on-chain read silently empty), and TokenCount
// must stay nil so a caller cannot read a v3 field that this contract does not have.
func TestGetSettlementUnpackV4(t *testing.T) {
	parsed, err := abi.JSON(strings.NewReader(contractABIV4JSON))
	if err != nil {
		t.Fatal(err)
	}
	method, ok := parsed.Methods["getSettlement"]
	if !ok {
		t.Fatal("getSettlement not in v4 ABI")
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
		VolumeByUnit [UnitCount]*big.Int
	}
	packed, err := method.Outputs.Pack(recIn{
		BatchId:      big.NewInt(9),
		Timestamp:    big.NewInt(777),
		TotalAmount:  big.NewInt(4200),
		SettledCount: big.NewInt(2),
		FailedCount:  big.NewInt(0),
		DetailsHash:  dh,
		RequestCount: big.NewInt(15),
		VolumeByUnit: vol(1200, 4),
	})
	if err != nil {
		t.Fatalf("pack outputs: %v", err)
	}
	out, err := unpackSettlement(parsed, schemaV4, packed)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if out.BatchId.Int64() != 9 || out.RequestCount == nil {
		t.Fatalf("v4 fields missing: %+v", out)
	}
	if out.RequestCount.Int64() != 15 {
		t.Errorf("request count wrong: %v", out.RequestCount)
	}
	if out.VolumeByUnit[UnitToken] == nil || out.VolumeByUnit[UnitImage] == nil {
		t.Fatalf("per-unit volume did not decode: %v", out.VolumeByUnit)
	}
	if out.VolumeByUnit[UnitToken].Int64() != 1200 || out.VolumeByUnit[UnitImage].Int64() != 4 ||
		out.VolumeByUnit[UnitAudioSecond].Sign() != 0 {
		t.Errorf("per-unit volume misdecoded: %v", out.VolumeByUnit)
	}
	if out.TokenCount != nil {
		t.Errorf("a v4 read must not populate the schema-3 TokenCount, got %v", out.TokenCount)
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

// TestV4ABIEncodesSubmitSettlement is the guard against the worst failure mode of this
// whole change: a v4 client whose ABI silently does not match the deployed contract. The
// embedded ABI must actually encode a full submitSettlement call — a mismatch would
// otherwise surface only at settle time, on the operator's key, mid-batch.
func TestV4ABIEncodesSubmitSettlement(t *testing.T) {
	v4, err := abi.JSON(strings.NewReader(contractABIV4JSON))
	if err != nil {
		t.Fatalf("embedded abi_v4.json does not parse: %v", err)
	}
	m, ok := v4.Methods["submitSettlement"]
	if !ok {
		t.Fatal("abi_v4.json has no submitSettlement")
	}
	// Seven inputs: the v1.3 stats column is replaced by ONE per-item vector argument,
	// not two parallel arrays.
	want := []string{"users", "sps", "amounts", "tokens", "requestCounts", "itemVolume", "detailsHash"}
	if len(m.Inputs) != len(want) {
		t.Fatalf("submitSettlement has %d inputs, want %d: %v", len(m.Inputs), len(want), m.Inputs)
	}
	for i, name := range want {
		if m.Inputs[i].Name != name {
			t.Errorf("input %d: want %q, got %q", i, name, m.Inputs[i].Name)
		}
	}
	if got, want := m.Inputs[5].Type.String(), fmt.Sprintf("uint256[%d][]", UnitCount); got != want {
		t.Errorf("itemVolume type: want %s, got %s", want, got)
	}

	// Encode a two-item batch and confirm the call actually goes through the ABI — the
	// step at which a schema/ABI mismatch becomes an error instead of a wrong tx.
	batch := SettlementBatch{
		Users:         []common.Address{common.HexToAddress(walletU), common.HexToAddress(walletU)},
		SPs:           []common.Address{common.HexToAddress(sp1Addr), common.HexToAddress(sp2Addr)},
		Amounts:       []*big.Int{big.NewInt(1), big.NewInt(2)},
		Tokens:        []common.Address{common.HexToAddress(filAddr), common.HexToAddress(filAddr)},
		RequestCounts: []*big.Int{big.NewInt(3), big.NewInt(4)},
		ItemVolume:    [][UnitCount]*big.Int{vol(1200, 4), vol(75)},
	}
	data, err := v4.Pack("submitSettlement", batch.Users, batch.SPs, batch.Amounts, batch.Tokens,
		batch.RequestCounts, itemVolumeArg(batch), batch.DetailsHash)
	if err != nil {
		t.Fatalf("abi_v4 cannot pack a submitSettlement call: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("packed calldata is empty")
	}
	if got := hex.EncodeToString(data[:4]); got != hex.EncodeToString(m.ID) {
		t.Errorf("calldata selector %s does not match the method id %s", got, hex.EncodeToString(m.ID))
	}
}

// TestV4SettlementTopicsDifferFromV3: the SettlementExecuted signature changed again in
// v1.4 (the trailing scalar became an array), so a v3 client must not mistake a v4 log
// for its own — and vice versa.
func TestV4SettlementTopicsDifferFromV3(t *testing.T) {
	v4, _ := abi.JSON(strings.NewReader(contractABIV4JSON))
	v3, _ := abi.JSON(strings.NewReader(contractABIV3JSON))
	v2, _ := abi.JSON(strings.NewReader(contractABIJSON))
	id4 := v4.Events["SettlementExecuted"].ID
	if id4 == v3.Events["SettlementExecuted"].ID {
		t.Fatal("SettlementExecuted topic did not change between schema 3 and 4")
	}
	if id4 == v2.Events["SettlementExecuted"].ID {
		t.Fatal("SettlementExecuted topic did not change between schema 2 and 4")
	}
}

// TestV4SchemaVersionMatchesConstant: the ABI and the Go schema number must agree, or
// verifySchema would reject a correct contract (or, worse, accept a wrong one).
func TestV4SchemaVersionMatchesConstant(t *testing.T) {
	if schemaV4 != 4 {
		t.Fatalf("schemaV4 constant drifted from the contract's SCHEMA_VERSION: %d", schemaV4)
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

// TestBuildPendingItemStats: token volume comes from the records (prompt+completion), a
// debt rid without a record counts 1 request / 0 volume.
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
	if got := it.Volume[UnitToken].Int64(); got != 130 {
		t.Errorf("token volume: want 130, got %d", got)
	}
	if it.TokenCount != 130 {
		t.Errorf("schema-3 token column must carry the same token volume, got %d", it.TokenCount)
	}
	// A token-only batch puts nothing in the other unit columns.
	if it.Volume[UnitImage].Sign() != 0 || it.Volume[UnitAudioSecond].Sign() != 0 {
		t.Errorf("token-only batch wrote a non-token column: %v", it.Volume)
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
	if its[0].RequestCount != 1 || its[0].Volume[UnitToken].Int64() != 60 {
		t.Errorf("item0: want 1/60, got %d/%d", its[0].RequestCount, its[0].Volume[UnitToken])
	}
	if its[1].RequestCount != 1 || its[1].Volume[UnitToken].Int64() != 10 {
		t.Errorf("item1: want 1/10 (split rid excluded), got %d/%d", its[1].RequestCount, its[1].Volume[UnitToken])
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

// TestToBatchCarriesVolume: the WAL carries the per-unit volume into the submit call.
// A WAL written before the volume columns existed has none, and must submit a full
// zero vector per item rather than a nil the ABI cannot encode.
func TestToBatchCarriesVolume(t *testing.T) {
	pb := pendingBatch{
		DetailsHash: strings.Repeat("ab", 32),
		Items: []pendingItem{
			{User: walletU, SP: sp1Addr, Amount: "100", Token: filAddr, RequestCount: 4,
				Volume: []*big.Int{big.NewInt(1200), big.NewInt(3), big.NewInt(0)}},
			{User: walletU, SP: sp2Addr, Amount: "200", Token: filAddr}, // old WAL entry
		},
	}
	var dh [32]byte
	b, err := pb.toBatch(dh)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.ItemVolume) != 2 {
		t.Fatalf("volume table must match item count, got %d", len(b.ItemVolume))
	}
	if b.ItemVolume[0][UnitToken].Int64() != 1200 || b.ItemVolume[0][UnitImage].Int64() != 3 {
		t.Errorf("item0 volume lost: %v", b.ItemVolume[0])
	}
	// The old entry must be a full zero vector — a nil column would be unencodable.
	for u := 0; u < UnitCount; u++ {
		if b.ItemVolume[1][u] == nil || b.ItemVolume[1][u].Sign() != 0 {
			t.Errorf("old WAL entry column %d must be a zero big.Int, got %v", u, b.ItemVolume[1][u])
		}
	}
}

// TestBuildPendingMixedUnitsInOneItem is the case the per-item vector exists for: one
// settlement item (grouped by wallet+SP) whose requests span a token model and an image
// model. Both volumes must land in their own column of the SAME item — a single unit tag
// could not express this, and filing the image into the token column would silently
// inflate the public token total.
func TestBuildPendingMixedUnitsInOneItem(t *testing.T) {
	s, _ := newTestSettler(t, newMockContract(), discardLogger())
	s.cfg.ModelCatalog = map[string]ModelInfo{
		"text-model":   {Modality: ModalityText, PriceUnit: PriceUnitToken},
		"flux-schnell": {Modality: ModalityImage, PriceUnit: PriceUnitImage, UnitUSD: "0.01"},
	}
	records := map[string]RequestRecord{
		"req-text":  {RequestID: "req-text", Wallet: walletU, Model: "text-model", PromptTokens: 900, CompletionTokens: 100},
		"req-image": {RequestID: "req-image", Wallet: walletU, Model: "flux-schnell", Receipt: &RecordReceipt{Quantity: 4}},
	}
	items := []SettlementItem{statsItem(sp1Addr, 5, "req-text", "req-image")}
	p := s.buildPending(items, Cursor{}, nil, nil, records)
	it := p.Batches[0].Items[0]
	if got := it.Volume[UnitToken].Int64(); got != 1000 {
		t.Errorf("token column: want 1000, got %d", got)
	}
	if got := it.Volume[UnitImage].Int64(); got != 4 {
		t.Errorf("image column: want 4, got %d", got)
	}
	// Schema-3 column: tokens only. The 4 images must not leak into it, and the token
	// volume must still be there for deployments that have not moved to schema 4.
	if it.TokenCount != 1000 {
		t.Errorf("schema-3 token column: want 1000 (images excluded), got %d", it.TokenCount)
	}
	if it.Volume[UnitAudioSecond].Sign() != 0 {
		t.Errorf("audio column must stay 0, got %v", it.Volume[UnitAudioSecond])
	}
}

// TestBuildPendingUnknownUnitCountsNoColumn: a model whose catalog entry names no
// recognised unit is a configuration error. Its volume must be omitted from every
// column rather than being silently filed under tokens (the one column a reader would
// never question).
func TestBuildPendingUnknownUnitCountsNoColumn(t *testing.T) {
	s, _ := newTestSettler(t, newMockContract(), discardLogger())
	s.cfg.ModelCatalog = map[string]ModelInfo{
		"odd-model": {Modality: "video", PriceUnit: "per_second"},
	}
	records := map[string]RequestRecord{
		"req-odd": {RequestID: "req-odd", Wallet: walletU, Model: "odd-model",
			PromptTokens: 500, CompletionTokens: 500},
	}
	items := []SettlementItem{statsItem(sp1Addr, 5, "req-odd")}
	p := s.buildPending(items, Cursor{}, nil, nil, records)
	it := p.Batches[0].Items[0]
	// The request is still COUNTED (it settled and cost money); only its volume is
	// omitted, because no column truthfully describes it.
	if it.RequestCount != 1 {
		t.Errorf("request count: want 1, got %d", it.RequestCount)
	}
	for u := 0; u < UnitCount; u++ {
		if it.Volume[u].Sign() != 0 {
			t.Errorf("column %d must stay 0 for an unrecognised unit, got %v", u, it.Volume[u])
		}
	}
}

// TestItemVolumeArgShapes: the ABI argument is one UNIT_COUNT-long vector per item, and
// a short or absent volume slice zero-fills rather than emitting a wrong-length row.
func TestItemVolumeArgShapes(t *testing.T) {
	batch := SettlementBatch{
		Users: make([]common.Address, 3),
		ItemVolume: [][UnitCount]*big.Int{
			func() [UnitCount]*big.Int { v := vol(5, 0, 7); v[UnitImage] = nil; return v }(), // mid entry with a nil column
		},
	}
	got := itemVolumeArg(batch)
	if len(got) != 3 {
		t.Fatalf("one vector per item, got %d", len(got))
	}
	if got[0][UnitToken].Int64() != 5 || got[0][UnitAudioSecond].Int64() != 7 {
		t.Errorf("present columns must survive: %v", got[0])
	}
	if got[0][UnitImage] == nil || got[0][UnitImage].Sign() != 0 {
		t.Errorf("nil column must zero-fill, got %v", got[0][UnitImage])
	}
	for i := 1; i < 3; i++ {
		for u := 0; u < UnitCount; u++ {
			if got[i][u] == nil || got[i][u].Sign() != 0 {
				t.Errorf("item %d col %d must zero-fill, got %v", i, u, got[i][u])
			}
		}
	}
}

// TestUnitIndexMatchesContractConstants pins the Go unit indices to the contract's
// UNIT_* constants. This is the ABI: if the two ever disagree, every on-chain volume
// number is filed under the wrong unit, silently and only in the public stats.
func TestUnitIndexMatchesContractConstants(t *testing.T) {
	if UnitToken != 0 || UnitImage != 1 || UnitAudioSecond != 2 || UnitCharacter != 3 || UnitCount != 4 {
		t.Fatalf("Go unit indices drifted from the contract's UNIT_* constants: %d/%d/%d/%d/%d",
			UnitToken, UnitImage, UnitAudioSecond, UnitCharacter, UnitCount)
	}
	if UnitIndex(PriceUnitToken) != UnitToken || UnitIndex(PriceUnitImage) != UnitImage ||
		UnitIndex(PriceUnitAudioSecond) != UnitAudioSecond || UnitIndex(PriceUnitCharacter) != UnitCharacter {
		t.Fatal("UnitIndex does not map the price units onto the contract's constants")
	}
	// The vector width is also baked into the embedded ABI. Read it from there, so a
	// contract recompiled with a different UNIT_COUNT fails here rather than at the
	// first settlement.
	v4, err := abi.JSON(strings.NewReader(contractABIV4JSON))
	if err != nil {
		t.Fatal(err)
	}
	if got := v4.Methods["submitSettlement"].Inputs[5].Type.String(); got != fmt.Sprintf("uint256[%d][]", UnitCount) {
		t.Fatalf("abi_v4 itemVolume is %s, Go UnitCount is %d", got, UnitCount)
	}
	if UnitIndex("token") != UnitToken {
		t.Fatal("an unset/empty unit must map to the token column, not to -1")
	}
	if UnitIndex("tokens") != -1 {
		t.Fatal("an unrecognised unit must map to -1 so it is counted in no column")
	}
}

// vol builds a full-width per-unit volume vector: the given values fill the leading
// columns in UNIT_* order and every remaining column is zero. Tests build vectors
// through this so a change in UnitCount does not leave nil columns behind.
func vol(vals ...int64) [UnitCount]*big.Int {
	var v [UnitCount]*big.Int
	for i := range v {
		v[i] = big.NewInt(0)
		if i < len(vals) {
			v[i] = big.NewInt(vals[i])
		}
	}
	return v
}
