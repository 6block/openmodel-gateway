package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	faddress "github.com/filecoin-project/go-address"

	"openmodel/sp-state-agent/internal/settlement"
)

func TestMeRequiresAuthAndReturnsUsage(t *testing.T) {
	g := webuiTestGateway(t)
	g.apiKeys[hashKey("sk-me-test")] = apiKeyEntry{Name: "me-user", Wallet: "0x9875c8D91fE91199D7B9207d78f5A592EFCc6f88", ID: "k-abc"}
	g.usage = newUsageRing(8)
	for i, name := range []string{"me-user", "someone-else", "me-user"} {
		g.recordUsage(RequestRecord{
			RequestID: "req-" + name + string(rune('0'+i)), APIKeyName: name,
			Timestamp: time.Now(), Model: "default", Status: 200,
			PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30,
		})
	}

	rec := getPath(g, "/v1/me")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v1/me = %d, want 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer sk-me-test")
	rr := httptest.NewRecorder()
	g.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/v1/me = %d body=%s", rr.Code, rr.Body.String())
	}
	var me struct {
		Key    struct{ Name, ID string }
		Wallet string
		Recent []usageEntry `json:"recent_usage"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.Key.Name != "me-user" || me.Key.ID != "k-abc" || me.Wallet == "" {
		t.Fatalf("me = %+v", me)
	}
	// Only own usage, newest first.
	if len(me.Recent) != 2 {
		t.Fatalf("recent = %+v", me.Recent)
	}
	for _, u := range me.Recent {
		if !strings.Contains(u.RequestID, "me-user") {
			t.Fatalf("foreign usage leaked: %+v", u)
		}
	}
}

func TestUsageRingWraps(t *testing.T) {
	u := newUsageRing(3)
	for i := 0; i < 5; i++ {
		u.push(usageEntry{KeyName: "k", RequestID: string(rune('a' + i))})
	}
	got := u.lastByKey("k", 10)
	if len(got) != 3 || got[0].RequestID != "e" || got[2].RequestID != "c" {
		t.Fatalf("ring = %+v", got)
	}
}

func TestF4AddrDerivation(t *testing.T) {
	prev := faddress.CurrentNetwork
	faddress.CurrentNetwork = faddress.Testnet
	defer func() { faddress.CurrentNetwork = prev }()

	g := webuiTestGateway(t)
	rec := getPath(g, "/v1/f4addr?wallet=0x9875c8D91fE91199D7B9207d78f5A592EFCc6f88")
	if rec.Code != http.StatusOK {
		t.Fatalf("f4addr = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct{ Wallet, F4 string }
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	// t-prefix (testnet), f410 namespace, deterministic derivation.
	if !strings.HasPrefix(resp.F4, "t410f") {
		t.Fatalf("f4 = %q", resp.F4)
	}
	// Same input → same output (pure function).
	rec2 := getPath(g, "/v1/f4addr?wallet=0x9875c8d91fe91199d7b9207d78f5a592efcc6f88") // lowercase in
	var resp2 struct{ Wallet, F4 string }
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if resp2.F4 != resp.F4 {
		t.Fatalf("derivation not canonical: %q vs %q", resp2.F4, resp.F4)
	}
	if rec := getPath(g, "/v1/f4addr?wallet=nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad wallet = %d", rec.Code)
	}
}

func TestWebConfigCarriesPricesAndFaucet(t *testing.T) {
	g := webuiTestGateway(t)
	g.settlementCfg.ModelPricesUSD = map[string]string{"default": "0.60"}
	var m map[string]interface{}
	_ = json.Unmarshal(getPath(g, "/v1/webconfig").Body.Bytes(), &m)
	prices, ok := m["prices_usd"].(map[string]interface{})
	if !ok || prices["default_output_per_mtok"] != "0.60" {
		t.Fatalf("prices_usd = %v", m["prices_usd"])
	}
	if m["faucet_url"] != "https://faucet.calibnet.chainsafe-fil.io" {
		t.Fatalf("faucet_url = %v (calibration preset expected)", m["faucet_url"])
	}
}

// Per-model pricing must be public (in webconfig) so the UI's pricing table and
// current-model rate render without a key, with cache-hit (cache_read) distinct
// from cache-miss (input). A model with no explicit entry inherits default rates.
func TestWebConfigCarriesPerModelPricing(t *testing.T) {
	g := webuiTestGateway(t)
	g.settlementCfg.ModelPricesUSD = map[string]string{"default": "0.60", "premium-model": "1.20"}
	g.settlementCfg.ModelCatalog = map[string]settlement.ModelInfo{
		"default":       {InputUSD: "0.20", CacheReadUSD: "0.05", ContextWindow: 32768, MaxOutput: 4096},
		"premium-model": {InputUSD: "0.40", CacheReadUSD: "0.10", ContextWindow: 131072, MaxOutput: 8192},
	}
	var m map[string]interface{}
	_ = json.Unmarshal(getPath(g, "/v1/webconfig").Body.Bytes(), &m)
	list, ok := m["models_pricing"].([]interface{})
	if !ok {
		t.Fatalf("models_pricing = %v", m["models_pricing"])
	}
	byID := map[string]map[string]interface{}{}
	for _, e := range list {
		row := e.(map[string]interface{})
		byID[row["id"].(string)] = row
	}
	if _, hasDefault := byID["default"]; hasDefault {
		t.Fatal("'default' is a fallback key, must not appear as a model")
	}
	prem := byID["premium-model"]
	if prem == nil {
		t.Fatalf("premium-model missing: %v", byID)
	}
	if prem["input_per_mtok"] != "0.40" || prem["output_per_mtok"] != "1.20" || prem["cache_read_per_mtok"] != "0.10" {
		t.Fatalf("premium pricing wrong: %v", prem)
	}
}
