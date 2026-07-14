package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/settlement"
	"openmodel/sp-state-agent/internal/worker"
)

// The catalog carries the FIL/USD rate billing currently uses (top-level
// fil_price_usd), so a client can translate the USD prices into FIL — this was
// previously visible only on the operator's admin port.
func TestCatalogIncludesFILPrice(t *testing.T) {
	scfg := &settlement.Config{
		ModelPricesUSD: map[string]string{"default": "0.20", "m1": "0.10"},
		FILPriceUSD:    "2.5",
		FILPriceSource: "manual",
	}
	pricer := settlement.NewPricer(scfg, discardLog())
	bc := settlement.NewBalanceCache(nil, nil, pricer, 30, discardLog())
	gw := New(worker.NewRegistry(discardLog(), ""),
		config.GatewayConfig{APIKeys: []config.APIKey{{Key: "k", Name: "n"}}}, discardLog())
	gw.SetBalanceChecker(bc, scfg)

	req := httptest.NewRequest(http.MethodGet, "/v1/catalog", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	gw.handleCatalog(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("catalog: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		FILPriceUSD string                   `json:"fil_price_usd"`
		Data        []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.FILPriceUSD != "2.5000" {
		t.Fatalf("fil_price_usd: want 2.5000 (the pricer's live value), got %q", body.FILPriceUSD)
	}
	if len(body.Data) == 0 {
		t.Fatal("catalog data must still list the priced models")
	}
}

// Without settlement wired in there is no price to report — the field must be
// omitted entirely, not sent as zero (a client must not mistake "no billing"
// for "FIL is worthless").
func TestCatalogOmitsFILPriceWhenSettlementDisabled(t *testing.T) {
	gw := New(worker.NewRegistry(discardLog(), ""),
		config.GatewayConfig{APIKeys: []config.APIKey{{Key: "k", Name: "n"}}}, discardLog())

	req := httptest.NewRequest(http.MethodGet, "/v1/catalog", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	gw.handleCatalog(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("catalog: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := raw["fil_price_usd"]; present {
		t.Fatalf("fil_price_usd must be omitted when settlement is disabled; body=%s", rec.Body.String())
	}
}
