package gateway

import (
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/settlement"
	"openmodel/sp-state-agent/internal/worker"
)

// #47 regression: with a catalog-priced model, the gateway's deferred pendingSpend
// adjustment must use the SPLIT cost (input/output/cache-read) — identical to what
// settlement will later clear via SettleSpend — not the flat total×output price.
// With the old flat adjustment this test leaves pending at $8; split leaves $5.
func TestBilling_CatalogAdjustUsesSplitCost(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// prompt 6 (2 cached), completion 2, total 8
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}],`+
			`"usage":{"prompt_tokens":6,"completion_tokens":2,"total_tokens":8,`+
			`"prompt_tokens_details":{"cached_tokens":2}}}`)
	}))
	defer up.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := worker.NewRegistry(logger, "")
	registry.Register(worker.WorkerRegistration{ID: "w0", Endpoint: up.URL, SchedulerURL: up.URL, GPUCount: 1})
	registry.UpdateState("w0", "GPU_STATE_AVAILABLE", "running", 0, "default", 1)

	scfg := &settlement.Config{
		ModelPricesUSD: map[string]string{"default": "1000000"}, // $1/token output
		ModelCatalog: map[string]settlement.ModelInfo{
			// input $0.50/token, cache-read $0.25/token
			"default": {InputUSD: "500000", CacheReadUSD: "250000", ContextWindow: 32768, MaxOutput: 4096},
		},
		FILPriceUSD:    "2.0",
		FILPriceSource: "manual",
		SupportedTokens: []settlement.TokenConfig{
			{Symbol: "USDC", Address: billUSDC, Decimals: 6},
			{Symbol: "FIL", Address: billFIL, Decimals: 18},
		},
		DeductionPriority: []string{"USDC", "FIL"},
		DefaultMaxTokens:  100,
	}
	pricer := settlement.NewPricer(scfg, logger)
	bc := settlement.NewBalanceCache(&fakeBalanceContract{usdc: usdcWei(1000)}, scfg.SupportedTokens, pricer, 30, logger)
	bc.SetWallets([]string{billWallet})
	bc.ForceRefresh(t.Context())

	gw := New(registry, config.GatewayConfig{
		RequestTimeoutSec: 5,
		APIKeys:           []config.APIKey{{Key: "test", Name: "user1", Wallet: billWallet}},
	}, logger)
	gw.SetBalanceChecker(bc, scfg)
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	defer gw.Close()

	if st := doChat(t, srv.URL, chatBody); st != 200 {
		t.Fatalf("expected 200, got %d", st)
	}

	// Split: 4 non-cached×$0.50 + 2 cached×$0.25 + 2 completion×$1 = 2 + 0.5 + 2 = $4.50.
	// Flat (the bug) would be 8×$1 = $8 — a $3.50 residue SettleSpend could never drain.
	want := big.NewFloat(4.5)
	if ps := bc.GetPendingSpend(billWallet); ps.Cmp(want) != 0 {
		t.Errorf("pendingSpend must equal the SPLIT cost $4.50 (settlement caliber), got %s", ps.Text('f', 6))
	}
}
