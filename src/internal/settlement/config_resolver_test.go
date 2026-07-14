package settlement

import "testing"

// TestApplyDefaultsFillsAll verifies every default-fill branch on an empty config.
func TestApplyDefaultsFillsAll(t *testing.T) {
	c := &Config{}
	c.ApplyDefaults()

	checks := []struct {
		name string
		got  interface{}
		want interface{}
	}{
		{"IntervalMinutes", c.IntervalMinutes, 15},
		{"MaxBatchSize", c.MaxBatchSize, 50},
		{"BalanceRefreshSec", c.BalanceRefreshSec, 30},
		{"FILPriceRefreshSec", c.FILPriceRefreshSec, 300},
		{"DefaultMaxTokens", c.DefaultMaxTokens, 4096},
		{"FILPriceSource", c.FILPriceSource, "manual"},
		{"OperatorMinBalance", c.OperatorMinBalance, "0.1"},
		{"MinBalanceFIL", c.MinBalanceFIL, "0.001"},
		{"FILPriceUSD", c.FILPriceUSD, "3.50"},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s = %v, want %v", ck.name, ck.got, ck.want)
		}
	}
	if len(c.FILPriceSources) != 2 || c.FILPriceSources[0] != "coingecko" || c.FILPriceSources[1] != "binance" {
		t.Errorf("FILPriceSources = %v, want [coingecko binance]", c.FILPriceSources)
	}
	if len(c.DeductionPriority) != 2 || c.DeductionPriority[0] != "USDC" || c.DeductionPriority[1] != "FIL" {
		t.Errorf("DeductionPriority = %v, want [USDC FIL]", c.DeductionPriority)
	}
	if c.ModelPricesUSD == nil || c.ModelPricesUSD["default"] != "0.20" {
		t.Errorf("ModelPricesUSD = %v, want a map with default=0.20", c.ModelPricesUSD)
	}
}

// TestApplyDefaultsPreservesExplicitValues: defaults must not clobber set values.
func TestApplyDefaultsPreservesExplicitValues(t *testing.T) {
	c := &Config{
		IntervalMinutes:   5,
		MaxBatchSize:      10,
		FILPriceSource:    "auto",
		FILPriceUSD:       "9.99",
		DeductionPriority: []string{"FIL"},
		ModelPricesUSD:    map[string]string{"premium": "0.50"}, // missing "default"
	}
	c.ApplyDefaults()

	if c.IntervalMinutes != 5 || c.MaxBatchSize != 10 || c.FILPriceSource != "auto" || c.FILPriceUSD != "9.99" {
		t.Error("ApplyDefaults clobbered an explicitly-set value")
	}
	if len(c.DeductionPriority) != 1 || c.DeductionPriority[0] != "FIL" {
		t.Errorf("DeductionPriority was overwritten: %v", c.DeductionPriority)
	}
	// "default" injected, "premium" preserved
	if c.ModelPricesUSD["default"] != "0.20" {
		t.Errorf("expected default price injected, got %v", c.ModelPricesUSD)
	}
	if c.ModelPricesUSD["premium"] != "0.50" {
		t.Errorf("expected premium price preserved, got %v", c.ModelPricesUSD)
	}
}

// TestConfirmationDepthDefaultVsExplicitZero: an ABSENT confirmation_depth defaults
// to 5, but an EXPLICIT 0 must be kept (meaning "no finality wait"). The earlier bug
// treated 0 as unset and overrode it to 5, stalling settlement on chains that only
// produce blocks on tx (e.g. a local dev Hardhat node).
func TestConfirmationDepthDefaultVsExplicitZero(t *testing.T) {
	c1 := &Config{} // absent
	c1.ApplyDefaults()
	if c1.ConfirmationDepth == nil || *c1.ConfirmationDepth != 5 {
		t.Errorf("absent confirmation_depth should default to 5, got %v", c1.ConfirmationDepth)
	}
	z := 0
	c2 := &Config{ConfirmationDepth: &z} // explicit 0
	c2.ApplyDefaults()
	if c2.ConfirmationDepth == nil || *c2.ConfirmationDepth != 0 {
		t.Errorf("explicit confirmation_depth: 0 must be kept as 0 (no finality wait), got %v", c2.ConfirmationDepth)
	}
	n := 7
	c3 := &Config{ConfirmationDepth: &n} // explicit non-zero
	c3.ApplyDefaults()
	if c3.ConfirmationDepth == nil || *c3.ConfirmationDepth != 7 {
		t.Errorf("explicit confirmation_depth: 7 must be preserved, got %v", c3.ConfirmationDepth)
	}
}

// TestValidateDisabledSkips: a disabled settlement config validates regardless.
func TestValidateDisabledSkips(t *testing.T) {
	c := &Config{Enabled: false} // all required fields empty
	if err := c.Validate(); err != nil {
		t.Errorf("disabled config should validate, got %v", err)
	}
}

// TestValidateRequiredFields: each missing required field is reported in turn.
func TestValidateRequiredFields(t *testing.T) {
	c := &Config{Enabled: true}
	mustErrContain := func(sub string) {
		t.Helper()
		err := c.Validate()
		if err == nil {
			t.Fatalf("expected error mentioning %q, got nil", sub)
		}
		if !contains(err.Error(), sub) {
			t.Fatalf("expected error mentioning %q, got %q", sub, err.Error())
		}
	}
	mustErrContain("rpc_url")
	c.RPCURL = "http://localhost:8545"
	mustErrContain("contract_address")
	c.ContractAddress = "0xabc"
	mustErrContain("operator_private_key")
	c.OperatorPrivateKey = "0xkey"
	mustErrContain("chain_id")
	c.ChainID = 31337
	if err := c.Validate(); err != nil {
		t.Errorf("fully-populated enabled config should validate, got %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// --- resolver ---

type fakeLister struct{ m map[string]string }

func (f fakeLister) ListWorkerSPMap() map[string]string { return f.m }

func TestRegistryResolver(t *testing.T) {
	m := map[string]string{"w1": "miner1"}
	r := NewRegistryResolver(fakeLister{m: m})
	got := r.GetWorkerSPMap()
	if len(got) != 1 || got["w1"] != "miner1" {
		t.Errorf("GetWorkerSPMap = %v, want %v", got, m)
	}
}

// TestRegistryResolverNilLister covers the nil-lister guard.
func TestRegistryResolverNilLister(t *testing.T) {
	r := NewRegistryResolver(nil)
	if got := r.GetWorkerSPMap(); got != nil {
		t.Errorf("nil lister should yield nil map, got %v", got)
	}
}
