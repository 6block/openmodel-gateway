package settlement

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// ParseSettlementOutcome decodes the events of a mined settlement tx by INDEX into the
// non-indexed argument list. That arithmetic is invisible to the compiler, and the list
// has grown once already (v1.3 appended requestCount; v1.4 appended volumeByUnit). These
// tests pin the decode against the real embedded ABIs, so a future field inserted in the
// middle — which would shift settledCount/failedCount and silently mis-report how many
// items settled, reversing money into debt or leaving it credited — fails here instead
// of on chain.

func packEventLog(t *testing.T, parsed abi.ABI, name string, contract common.Address, args ...interface{}) *types.Log {
	t.Helper()
	ev, ok := parsed.Events[name]
	if !ok {
		t.Fatalf("%s not in ABI", name)
	}
	// Only the non-indexed values go in Data; the indexed ones go in Topics.
	var nonIndexed abi.Arguments
	for _, in := range ev.Inputs {
		if !in.Indexed {
			nonIndexed = append(nonIndexed, in)
		}
	}
	data, err := nonIndexed.Pack(args...)
	if err != nil {
		t.Fatalf("pack %s: %v", name, err)
	}
	var topics []common.Hash
	topics = append(topics, ev.ID)
	// batchId is the sole indexed arg on both events; placeholder value is fine for the
	// decode under test.
	for _, in := range ev.Inputs {
		if in.Indexed {
			topics = append(topics, common.BigToHash(big.NewInt(1)))
		}
	}
	return &types.Log{Address: contract, Topics: topics, Data: data}
}

// TestParseSettlementOutcomeV3: the v1.3 event's non-indexed list is
// [totalAmount, platformFee, settledCount, failedCount, detailsHash, requestCount,
// tokenCount]. settledCount/failedCount sit at indexes 2 and 3 — appending
// requestCount/tokenCount did not move them, and that has to stay true.
func TestParseSettlementOutcomeV3(t *testing.T) {
	parsed, err := abi.JSON(strings.NewReader(contractABIV3JSON))
	if err != nil {
		t.Fatal(err)
	}
	addr := common.HexToAddress("0x00000000000000000000000000000000000000AA")
	var dh [32]byte
	c := &ContractClient{abi: parsed, contractAddr: addr}

	lg := packEventLog(t, parsed, "SettlementExecuted", addr,
		big.NewInt(5000), big.NewInt(25), big.NewInt(7), big.NewInt(2), dh,
		big.NewInt(99), big.NewInt(123456))
	rec := &types.Receipt{Logs: []*types.Log{lg}}

	out := c.ParseSettlementOutcome(rec)
	if !out.Found {
		t.Fatal("SettlementExecuted event was not recognized")
	}
	if out.SettledCount != 7 || out.FailedCount != 2 {
		t.Fatalf("v3 counts misdecoded: settled=%d failed=%d (want 7/2)", out.SettledCount, out.FailedCount)
	}
}

// TestParseSettlementOutcomeV4: v1.4 replaced the trailing scalar with a
// uint256[UNIT_COUNT], keeping the field count at 8. The two counts must still decode,
// and the new array must not shift them.
func TestParseSettlementOutcomeV4(t *testing.T) {
	parsed, err := abi.JSON(strings.NewReader(contractABIV4JSON))
	if err != nil {
		t.Fatal(err)
	}
	addr := common.HexToAddress("0x00000000000000000000000000000000000000BB")
	var dh [32]byte
	c := &ContractClient{abi: parsed, contractAddr: addr}

	lg := packEventLog(t, parsed, "SettlementExecuted", addr,
		big.NewInt(5000), big.NewInt(25), big.NewInt(9), big.NewInt(3), dh,
		big.NewInt(99), vol(1200, 4))
	rec := &types.Receipt{Logs: []*types.Log{lg}}

	out := c.ParseSettlementOutcome(rec)
	if !out.Found {
		t.Fatal("SettlementExecuted event was not recognized")
	}
	if out.SettledCount != 9 || out.FailedCount != 3 {
		t.Fatalf("v4 counts misdecoded: settled=%d failed=%d (want 9/3)", out.SettledCount, out.FailedCount)
	}
}

// TestParseSettlementOutcomeIgnoresForeignAddress: a log from another contract that
// happens to share the topic must not be read as ours — otherwise a look-alike event
// could rewrite the settled/failed accounting.
func TestParseSettlementOutcomeIgnoresForeignAddress(t *testing.T) {
	parsed, err := abi.JSON(strings.NewReader(contractABIV4JSON))
	if err != nil {
		t.Fatal(err)
	}
	var dh [32]byte
	c := &ContractClient{abi: parsed, contractAddr: common.HexToAddress("0x00000000000000000000000000000000000000BB")}

	lg := packEventLog(t, parsed, "SettlementExecuted",
		common.HexToAddress("0x00000000000000000000000000000000000000CC"),
		big.NewInt(5000), big.NewInt(25), big.NewInt(9), big.NewInt(3), dh,
		big.NewInt(99), vol(1))
	out := c.ParseSettlementOutcome(&types.Receipt{Logs: []*types.Log{lg}})
	if out.Found {
		t.Fatal("a SettlementExecuted log from a different contract must be ignored")
	}
}

// TestParseSettlementOutcomeFailedIndexes: SettlementItemFailed carries (index, user,
// reason) non-indexed. The index decides WHICH item's revenue is reversed into debt, so
// a wrong one reverses the wrong user's settlement.
func TestParseSettlementOutcomeFailedIndexes(t *testing.T) {
	parsed, err := abi.JSON(strings.NewReader(contractABIV4JSON))
	if err != nil {
		t.Fatal(err)
	}
	addr := common.HexToAddress("0x00000000000000000000000000000000000000BB")
	c := &ContractClient{abi: parsed, contractAddr: addr}
	var dh [32]byte

	logs := []*types.Log{
		packEventLog(t, parsed, "SettlementExecuted", addr,
			big.NewInt(5000), big.NewInt(25), big.NewInt(1), big.NewInt(2), dh,
			big.NewInt(2), vol(10)),
		packEventLog(t, parsed, "SettlementItemFailed", addr, big.NewInt(1),
			common.HexToAddress("0x00000000000000000000000000000000000000DD"), "insufficient balance"),
		packEventLog(t, parsed, "SettlementItemFailed", addr, big.NewInt(4),
			common.HexToAddress("0x00000000000000000000000000000000000000EE"), "zero sp address"),
	}
	out := c.ParseSettlementOutcome(&types.Receipt{Logs: logs})
	if !out.Found || out.SettledCount != 1 || out.FailedCount != 2 {
		t.Fatalf("counts misdecoded: %+v", out)
	}
	if len(out.FailedIndexes) != 2 || out.FailedIndexes[0] != 1 || out.FailedIndexes[1] != 4 {
		t.Fatalf("failed indexes misdecoded: %v (want [1 4])", out.FailedIndexes)
	}
}
