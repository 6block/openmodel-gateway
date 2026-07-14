package settlement

import (
	"bytes"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

// TestFindSettlementByHash covers the local audit-log lookup used by the
// settlements/:id endpoint and `verify` CLI command.
func TestFindSettlementByHash(t *testing.T) {
	s, _ := newTestSettler(t, newMockContract(), discardLogger())
	h1 := strings.Repeat("ab", 32)
	h2 := strings.Repeat("cd", 32)
	s.logSettlement(settlementLog{TxHash: "0xtx1", BlockNumber: 8, GasUsed: 100, ItemCount: 3, DetailsHash: h1})
	s.logSettlement(settlementLog{TxHash: "0xtx2", BlockNumber: 9, GasUsed: 200, ItemCount: 1, DetailsHash: h2})

	got, ok := s.FindSettlementByHash(h1)
	if !ok || got.TxHash != "0xtx1" || got.BlockNumber != 8 || got.ItemCount != 3 {
		t.Fatalf("exact lookup failed: ok=%v got=%+v", ok, got)
	}
	// 0x-prefix tolerance + case-insensitive
	got, ok = s.FindSettlementByHash("0x" + strings.ToUpper(h2))
	if !ok || got.TxHash != "0xtx2" {
		t.Fatalf("0x/case-insensitive lookup failed: ok=%v got=%+v", ok, got)
	}
	if _, ok := s.FindSettlementByHash(strings.Repeat("ef", 32)); ok {
		t.Fatal("unexpected match for unknown hash")
	}
}

// TestFindSettlementByHashNoFile: missing audit log → not found, no panic.
func TestFindSettlementByHashNoFile(t *testing.T) {
	s, _ := newTestSettler(t, newMockContract(), discardLogger())
	if _, ok := s.FindSettlementByHash(strings.Repeat("ab", 32)); ok {
		t.Error("expected not-found when audit log does not exist")
	}
}

// TestGetSettlementUnpack validates that OnChainSettlement's fields map correctly
// onto the getSettlement ABI tuple (the risky part of ContractClient.GetSettlement),
// without needing a live chain.
func TestGetSettlementUnpack(t *testing.T) {
	parsed, err := abi.JSON(strings.NewReader(contractABIJSON))
	if err != nil {
		t.Fatal(err)
	}
	method, ok := parsed.Methods["getSettlement"]
	if !ok {
		t.Fatal("getSettlement not in ABI")
	}

	var dh [32]byte
	copy(dh[:], bytes.Repeat([]byte{0xAB}, 32))

	// Pack a tuple output matching the ABI components, then unpack into our struct.
	type recIn struct {
		BatchId      *big.Int
		Timestamp    *big.Int
		TotalAmount  *big.Int
		SettledCount *big.Int
		FailedCount  *big.Int
		DetailsHash  [32]byte
	}
	packed, err := method.Outputs.Pack(recIn{
		BatchId:      big.NewInt(7),
		Timestamp:    big.NewInt(1234),
		TotalAmount:  big.NewInt(5000),
		SettledCount: big.NewInt(3),
		FailedCount:  big.NewInt(1),
		DetailsHash:  dh,
	})
	if err != nil {
		t.Fatalf("pack outputs: %v", err)
	}

	// Exercise the SAME helper ContractClient.GetSettlement uses in production.
	out, err := unpackSettlement(parsed, packed)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if out.BatchId.Int64() != 7 || out.Timestamp.Int64() != 1234 ||
		out.TotalAmount.Int64() != 5000 || out.SettledCount.Int64() != 3 ||
		out.FailedCount.Int64() != 1 || out.DetailsHash != dh {
		t.Errorf("field mapping wrong: %+v", out)
	}
}
