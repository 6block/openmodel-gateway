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
	"openmodel/sp-state-agent/internal/worker"
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

// The chat UI decides BEFORE login whether it can accept an image upload, and how
// large a prompt it may build, from this public payload alone: it has no key yet,
// so /v1/catalog is out of reach. Losing `modality` would silently hide the attach
// button on a vision model; losing context_window/max_output would let the UI stage
// a prompt the worker must reject. Both halves are asserted here for that reason.
func TestWebConfigCarriesVisionCapabilities(t *testing.T) {
	g := webuiTestGateway(t)
	g.settlementCfg.ModelPricesUSD = map[string]string{"default": "0.60"}
	g.settlementCfg.ModelCatalog = map[string]settlement.ModelInfo{
		"default": {InputUSD: "0.20", ContextWindow: 32768, MaxOutput: 4096},
		"/models/Qwen--Qwen3-VL-8B-Instruct": {
			InputUSD: "0.30", ContextWindow: 8192, MaxOutput: 4096,
			Modality: settlement.ModalityVision, MaxImagesPerRequest: 1,
		},
	}
	// The catalog only lists models the registry actually serves; register the VLM
	// so resolveModels returns it (same as production, where a worker claims it).
	g.registry.Register(worker.WorkerRegistration{
		ID: "w-vlm", Endpoint: "http://127.0.0.1:8000", SchedulerURL: "http://127.0.0.1:9090",
		GPUCount: 1, SelfRegistered: false,
		SupportedModels: []string{"/models/Qwen--Qwen3-VL-8B-Instruct"},
	})
	g.registry.UpdateState("w-vlm", "GPU_STATE_AVAILABLE", "running", 0,
		"/models/Qwen--Qwen3-VL-8B-Instruct", 1)

	var m map[string]interface{}
	_ = json.Unmarshal(getPath(g, "/v1/webconfig").Body.Bytes(), &m)
	list, ok := m["models_pricing"].([]interface{})
	if !ok {
		t.Fatalf("models_pricing = %v", m["models_pricing"])
	}
	var vlm map[string]interface{}
	for _, e := range list {
		row := e.(map[string]interface{})
		if row["id"] == "/models/Qwen--Qwen3-VL-8B-Instruct" {
			vlm = row
		}
	}
	if vlm == nil {
		t.Fatalf("vision model missing from webconfig: %v", list)
	}
	if vlm["modality"] != settlement.ModalityVision {
		t.Fatalf("modality = %v, want %q — the attach UI keys off this field",
			vlm["modality"], settlement.ModalityVision)
	}
	if vlm["context_window"] != float64(8192) || vlm["max_output"] != float64(4096) {
		t.Fatalf("budget fields wrong: context_window=%v max_output=%v",
			vlm["context_window"], vlm["max_output"])
	}
	// The image cap is part of the same contract: absent means "no count limit",
	// which is what the multi-image path relies on.
	if vlm["max_images_per_request"] != float64(1) {
		t.Fatalf("max_images_per_request = %v, want 1", vlm["max_images_per_request"])
	}
}

// No configured image cap must mean the field is ABSENT, not zero. The UI reads
// absence as "no count limit — bound by the context window", which is the shipped
// multi-image behaviour; a literal 0 would read as "zero images allowed".
func TestWebConfigOmitsImageCapWhenUnset(t *testing.T) {
	g := webuiTestGateway(t)
	g.settlementCfg.ModelPricesUSD = map[string]string{"default": "0.60"}
	g.settlementCfg.ModelCatalog = map[string]settlement.ModelInfo{
		"default": {InputUSD: "0.20", ContextWindow: 8192, MaxOutput: 4096},
		"/models/Qwen--Qwen3-VL-8B-Instruct": {
			InputUSD: "0.30", ContextWindow: 8192, MaxOutput: 4096,
			Modality: settlement.ModalityVision, // MaxImagesPerRequest deliberately 0
		},
	}
	g.registry.Register(worker.WorkerRegistration{
		ID: "w-vlm2", Endpoint: "http://127.0.0.1:8000", SchedulerURL: "http://127.0.0.1:9090",
		GPUCount: 1, SelfRegistered: false,
		SupportedModels: []string{"/models/Qwen--Qwen3-VL-8B-Instruct"},
	})
	g.registry.UpdateState("w-vlm2", "GPU_STATE_AVAILABLE", "running", 0,
		"/models/Qwen--Qwen3-VL-8B-Instruct", 1)

	var m map[string]interface{}
	_ = json.Unmarshal(getPath(g, "/v1/webconfig").Body.Bytes(), &m)
	list := m["models_pricing"].([]interface{})
	for _, e := range list {
		row := e.(map[string]interface{})
		if row["id"] != "/models/Qwen--Qwen3-VL-8B-Instruct" {
			continue
		}
		if _, has := row["max_images_per_request"]; has {
			t.Fatalf("unset cap must be omitted, got %v", row["max_images_per_request"])
		}
		if row["modality"] != settlement.ModalityVision {
			t.Fatalf("modality = %v", row["modality"])
		}
		return
	}
	t.Fatal("vision model missing from webconfig")
}
