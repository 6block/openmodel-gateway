package settlement

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// scriptedChain returns canned GetUserBalance results in order; the last entry
// repeats. Lets a test act out "worked, then failed, then answered zero".
type scriptedChain struct {
	script []func() (*big.Int, error)
	calls  int
}

func (s *scriptedChain) GetUserBalance(context.Context, common.Address, common.Address) (*big.Int, error) {
	i := s.calls
	if i >= len(s.script) {
		i = len(s.script) - 1
	}
	s.calls++
	return s.script[i]()
}

func balFIL(fil int64) func() (*big.Int, error) {
	wei := new(big.Int).Mul(big.NewInt(fil), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	return func() (*big.Int, error) { return wei, nil }
}
func rpcDown() (*big.Int, error) { return nil, errors.New("connection reset by peer") }

// refreshCache builds a single-token (FIL @ $2) cache around a scripted chain.
func refreshCache(t *testing.T, script ...func() (*big.Int, error)) (*BalanceCache, *scriptedChain) {
	t.Helper()
	cfg := &Config{
		FILPriceUSD:    "2.0",
		FILPriceSource: "manual",
		SupportedTokens: []TokenConfig{
			{Symbol: "FIL", Address: filAddr, Decimals: 18},
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	chain := &scriptedChain{script: script}
	bc := NewBalanceCache(chain, cfg.SupportedTokens, NewPricer(cfg, logger), 30, logger)
	return bc, chain
}

// Soak finding #3: one transient RPC failure must NOT erase a valid cached
// balance. Refusing a paying user needs positive evidence of an empty balance;
// "the RPC did not answer" is not that evidence.
func TestRefresh_RPCFailureKeepsLastKnownBalance(t *testing.T) {
	bc, _ := refreshCache(t, balFIL(10), rpcDown)

	bc.refreshWallet(context.Background(), walletU) // succeeds: 10 FIL = $20
	if !bc.HasSufficientBalance(walletU, big.NewFloat(15)) {
		t.Fatal("setup: $20 should cover $15")
	}

	bc.refreshWallet(context.Background(), walletU) // RPC down — must keep $20
	if !bc.HasSufficientBalance(walletU, big.NewFloat(15)) {
		t.Fatal("a transient RPC failure erased the cached balance (the soak's wrong 402)")
	}
}

// A positive→zero drop is how a broken read looks (empty eth_call body decodes
// as 0 — the M3 glif "fake zero"). One confirm read must catch it.
func TestRefresh_FakeZeroCaughtByConfirmRead(t *testing.T) {
	bc, chain := refreshCache(t, balFIL(10), func() (*big.Int, error) { return big.NewInt(0), nil }, balFIL(10))

	bc.refreshWallet(context.Background(), walletU) // 10 FIL
	bc.refreshWallet(context.Background(), walletU) // reads 0 → confirm reads 10 → keep 10
	if !bc.HasSufficientBalance(walletU, big.NewFloat(15)) {
		t.Fatal("fake zero slipped through; balance should still be $20")
	}
	if chain.calls != 3 {
		t.Fatalf("expected exactly one confirm read (3 calls total), got %d", chain.calls)
	}
}

// A zero confirmed by a second read is a REAL zero and must be believed —
// fail-open must never shield a genuinely drained wallet.
func TestRefresh_RealZeroConfirmedAndEnforced(t *testing.T) {
	bc, _ := refreshCache(t, balFIL(10), func() (*big.Int, error) { return big.NewInt(0), nil })

	bc.refreshWallet(context.Background(), walletU) // 10 FIL
	bc.refreshWallet(context.Background(), walletU) // 0, confirm 0 → accept
	if bc.HasSufficientBalance(walletU, big.NewFloat(1)) {
		t.Fatal("confirmed-empty wallet must be refused")
	}
}

// A brand-new wallet with a failing RPC has no last-known value to fall back
// on: it stays unfunded (correct — nothing was ever proven about it).
func TestRefresh_UnknownWalletWithFailingRPCStaysUnfunded(t *testing.T) {
	bc, _ := refreshCache(t, rpcDown)

	bc.refreshWallet(context.Background(), walletU)
	if bc.HasSufficientBalance(walletU, big.NewFloat(1)) {
		t.Fatal("never-seen wallet must not gain balance from a failed read")
	}
}
