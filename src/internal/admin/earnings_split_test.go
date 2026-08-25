package admin

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"openmodel/sp-state-agent/internal/settlement"
)

// v1.0-only reader: implements ContractReader but NOT frozenEarningsReader.
type v10Reader struct {
	mockReader
	earnings *big.Int
}

func (m *v10Reader) GetSPEarnings(ctx context.Context, sp, token common.Address) (*big.Int, error) {
	return m.earnings, nil
}

// v1.1 reader: adds the freeze views.
type v11Reader struct {
	v10Reader
	total, frozen, withdrawable *big.Int
}

func (m *v11Reader) GetTotalEarnings(ctx context.Context, sp, token common.Address) (*big.Int, error) {
	return m.total, nil
}
func (m *v11Reader) GetFrozenEarnings(ctx context.Context, sp, token common.Address) (*big.Int, error) {
	return m.frozen, nil
}
func (m *v11Reader) GetWithdrawableEarnings(ctx context.Context, sp, token common.Address) (*big.Int, error) {
	return m.withdrawable, nil
}

func splitTestAPI(t *testing.T, contract ContractReader) *SettlementAPI {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tokens := []settlement.TokenConfig{{Symbol: "FIL", Address: "0x0000000000000000000000000000000000000000", Decimals: 18}}
	return NewSettlementAPI(contract, nil, nil, nil, nil, tokens, map[string]string{}, logger)
}

func TestReadEarningsSplitV11(t *testing.T) {
	fil := func(n int64) *big.Int {
		return new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	}
	m := &v11Reader{
		total:        fil(10),
		frozen:       fil(7),
		withdrawable: fil(3),
	}
	sa := splitTestAPI(t, m)
	earnings, frozen, withdrawable := sa.readEarningsSplit(context.Background(), "0x0000000000000000000000000000000000000009")
	if earnings["FIL"] != "10.000000" {
		t.Fatalf("earnings = %v", earnings)
	}
	if frozen["FIL"] != "7.000000" || withdrawable["FIL"] != "3.000000" {
		t.Fatalf("split = %v / %v", frozen, withdrawable)
	}
}

// Regression for the live finding: with an empty static sp_address_map, the
// revenue listing must still cover self-registered SPs via the payout provider,
// and the miner-signed value must win over a static entry for the same miner.
func TestEffectiveSPMapMergesSelfRegistered(t *testing.T) {
	sa := splitTestAPI(t, &v10Reader{earnings: big.NewInt(0)})
	sa.spMap = map[string]string{"t0100": "0xStaticA", "t0200": "0xStaticB"}

	// No provider → static only.
	if got := sa.effectiveSPMap(); len(got) != 2 || got["t0100"] != "0xStaticA" {
		t.Fatalf("static-only map = %v", got)
	}

	sa.SetMinerPayoutProvider(func() map[string]string {
		return map[string]string{
			"t0200": "0xSigned200", // overrides static for the same miner
			"t0300": "0xSigned300", // purely self-registered
		}
	})
	got := sa.effectiveSPMap()
	if len(got) != 3 {
		t.Fatalf("merged map = %v", got)
	}
	if got["t0100"] != "0xStaticA" || got["t0200"] != "0xSigned200" || got["t0300"] != "0xSigned300" {
		t.Fatalf("merge precedence wrong: %v", got)
	}
	if sa.resolveToEVM("t0300") != "0xSigned300" {
		t.Fatal("resolveToEVM missed the dynamic overlay")
	}
}

func TestReadEarningsSplitFallsBackToV10(t *testing.T) {
	m := &v10Reader{earnings: new(big.Int).Mul(big.NewInt(5), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))}
	sa := splitTestAPI(t, m)
	earnings, frozen, withdrawable := sa.readEarningsSplit(context.Background(), "0x0000000000000000000000000000000000000009")
	if earnings["FIL"] != "5.000000" {
		t.Fatalf("earnings = %v", earnings)
	}
	if len(frozen) != 0 || len(withdrawable) != 0 {
		t.Fatalf("v1.0 reader must not produce a split: %v / %v", frozen, withdrawable)
	}
}
