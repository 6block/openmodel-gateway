package gateway

import (
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/settlement"
	"openmodel/sp-state-agent/internal/worker"
)

const suspWallet = "0x00000000000000000000000000000000000000D3"

// TestSuspendedWalletGets402 verifies the D3 gateway path: a wallet suspended for
// unpaid debt is refused with 402 (account suspended) BEFORE routing, regardless of
// its current on-chain balance. Uses httptest.NewRecorder (no socket).
func TestSuspendedWalletGets402(t *testing.T) {
	scfg := &settlement.Config{
		ModelPricesUSD:    map[string]string{"default": "1000000"},
		FILPriceUSD:       "2.0",
		FILPriceSource:    "manual",
		SupportedTokens:   []settlement.TokenConfig{{Symbol: "FIL", Address: "0x0000000000000000000000000000000000000000", Decimals: 18}},
		DeductionPriority: []string{"FIL"},
		DefaultMaxTokens:  100,
	}
	pricer := settlement.NewPricer(scfg, discardLog())
	bc := settlement.NewBalanceCache(nil, scfg.SupportedTokens, pricer, 30, discardLog())
	bc.SetDebtSuspension(big.NewFloat(1))
	// Wallet owes $5 (over the $1 threshold) → suspended.
	bc.UpdateDebts(map[string]*big.Float{suspWallet: big.NewFloat(5)})

	gw := New(worker.NewRegistry(discardLog(), ""),
		config.GatewayConfig{APIKeys: []config.APIKey{{Key: "secret", Name: "k1", Wallet: suspWallet}}},
		discardLog())
	gw.SetBalanceChecker(bc, scfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"default"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	gw.handleProxy(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("suspended wallet must get 402, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "suspended") {
		t.Errorf("402 body should explain suspension, got %s", rec.Body.String())
	}
}

// TestUnsuspendedWalletNot402 verifies a wallet with debt below the threshold is NOT
// suspended (it proceeds past the suspension gate; with sufficient balance it then
// routes — here no worker, so it won't be a 402-suspension).
func TestUnsuspendedWalletNot402(t *testing.T) {
	scfg := &settlement.Config{
		ModelPricesUSD:    map[string]string{"default": "1000000"},
		FILPriceUSD:       "2.0",
		FILPriceSource:    "manual",
		SupportedTokens:   []settlement.TokenConfig{{Symbol: "FIL", Address: "0x0000000000000000000000000000000000000000", Decimals: 18}},
		DeductionPriority: []string{"FIL"},
		DefaultMaxTokens:  100,
	}
	pricer := settlement.NewPricer(scfg, discardLog())
	bc := settlement.NewBalanceCache(nil, scfg.SupportedTokens, pricer, 30, discardLog())
	bc.SetDebtSuspension(big.NewFloat(10))
	bc.UpdateDebts(map[string]*big.Float{suspWallet: big.NewFloat(2)}) // below threshold

	gw := New(worker.NewRegistry(discardLog(), ""),
		config.GatewayConfig{APIKeys: []config.APIKey{{Key: "secret", Name: "k1", Wallet: suspWallet}}},
		discardLog())
	gw.SetBalanceChecker(bc, scfg)

	// Use an unsupported model so it fast-404s rather than blocking; the point is it
	// must NOT be refused by the suspension gate. With no balance it could be 402
	// insufficient-balance though, so we only assert the body isn't the suspension msg.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"no-such-model-xyz"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	gw.handleProxy(rec, req)

	if strings.Contains(rec.Body.String(), "suspended") {
		t.Fatalf("wallet below debt threshold must not be suspended, got %d (%s)", rec.Code, rec.Body.String())
	}
}
