package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/crypto"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/settlement"
)

func webuiTestGateway(t *testing.T) *Gateway {
	t.Helper()
	g := newRegTestGateway(t)
	g.webUI = config.WebUIConfig{Enabled: true, PublicQueryURL: "http://example.com:18020/"}
	g.settlementCfg = &settlement.Config{ChainID: 314159, ContractAddress: "0x2e291E8C7eB5B053aeAC32F2A6Fa74b71B0E2e57"}
	return g
}

func getPath(g *Gateway, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestWebUIDisabledByDefault(t *testing.T) {
	g := newRegTestGateway(t) // zero-value webUI
	if rec := getPath(g, "/"); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled: / = %d, want 404", rec.Code)
	}
	// /v1/webconfig must fall into the /v1/ catch-all, not exist implicitly.
	if rec := getPath(g, "/v1/webconfig"); rec.Code == http.StatusOK {
		t.Fatal("disabled: /v1/webconfig should not be served")
	}
}

func TestWebUIServesApp(t *testing.T) {
	g := webuiTestGateway(t)

	rec := getPath(g, "/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "OpenModel") {
		t.Fatalf("/ = %d, body %.60q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("/ content-type = %q", ct)
	}
	for _, p := range []string{"/app.js", "/style.css", "/favicon.svg"} {
		if rec := getPath(g, p); rec.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", p, rec.Code)
		}
	}
	// Unknown roots are 404 JSON, and unknown /v1/* keeps its API-shaped error —
	// the SPA must never shadow API 404s with HTML.
	if rec := getPath(g, "/not-a-page"); rec.Code != http.StatusNotFound {
		t.Fatalf("/not-a-page = %d, want 404", rec.Code)
	}
	if rec := getPath(g, "/v1/nope"); strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("/v1/nope returned HTML: %.60q", rec.Body.String())
	}
}

func TestWebConfigPayload(t *testing.T) {
	g := webuiTestGateway(t)
	rec := getPath(g, "/v1/webconfig")
	if rec.Code != http.StatusOK {
		t.Fatalf("webconfig = %d", rec.Code)
	}
	var cfg struct {
		SettlementEnabled bool   `json:"settlement_enabled"`
		ChainID           int64  `json:"chain_id"`
		Contract          string `json:"contract_address"`
		PublicQueryURL    string `json:"public_query_url"`
		Chain             struct {
			ChainName string   `json:"chainName"`
			RPCURLs   []string `json:"rpcUrls"`
		} `json:"chain"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.SettlementEnabled || cfg.ChainID != 314159 || cfg.Contract == "" {
		t.Fatalf("webconfig = %+v", cfg)
	}
	if cfg.PublicQueryURL != "http://example.com:18020" { // trailing slash trimmed
		t.Fatalf("public_query_url = %q", cfg.PublicQueryURL)
	}
	if !strings.Contains(cfg.Chain.ChainName, "Calibration") || len(cfg.Chain.RPCURLs) == 0 {
		t.Fatalf("chain preset = %+v", cfg.Chain)
	}

	// Settlement disabled → explicit flag, no chain params.
	g.settlementCfg = nil
	var m map[string]any
	_ = json.Unmarshal(getPath(g, "/v1/webconfig").Body.Bytes(), &m)
	if m["settlement_enabled"] != false {
		t.Fatalf("settlement_enabled = %v", m["settlement_enabled"])
	}
	if _, ok := m["chain_id"]; ok {
		t.Fatal("chain_id should be absent when settlement is disabled")
	}
}

// The endpoint must return the byte-exact string handleRegister will verify, with
// the wallet canonicalized (EIP-55) server-side — that is its whole reason to exist.
func TestRegisterMessageEndpoint(t *testing.T) {
	g := webuiTestGateway(t)
	_, wallet := genWallet(t)

	rec := getPath(g, "/v1/register/message?wallet="+strings.ToLower(wallet))
	if rec.Code != http.StatusOK {
		t.Fatalf("register/message = %d body=%s", rec.Code, rec.Body.String())
	}
	var m struct {
		Wallet   string `json:"wallet"`
		IssuedAt int64  `json:"issued_at"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Wallet != wallet { // genWallet returns the checksummed form
		t.Fatalf("wallet not canonicalized: %q vs %q", m.Wallet, wallet)
	}
	if m.Message != registrationMessage(m.Wallet, m.IssuedAt) {
		t.Fatalf("message drifted from registrationMessage: %q", m.Message)
	}

	if rec := getPath(g, "/v1/register/message?wallet=zzz"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad wallet = %d, want 400", rec.Code)
	}
}

// Full browser flow against the real handlers: fetch the message, sign it the way
// a wallet does (EIP-191 over the raw string), register, get a key.
func TestWebRegistrationFlowEndToEnd(t *testing.T) {
	g := webuiTestGateway(t)
	priv, wallet := genWallet(t)

	var m struct {
		Wallet   string `json:"wallet"`
		IssuedAt int64  `json:"issued_at"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(getPath(g, "/v1/register/message?wallet="+wallet).Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	sig, err := crypto.Sign(accounts.TextHash([]byte(m.Message)), priv)
	if err != nil {
		t.Fatal(err)
	}
	sig[64] += 27 // wallet-style v

	rec := postRegister(g, map[string]any{
		"wallet": m.Wallet, "issued_at": m.IssuedAt, "signature": "0x" + hexEncode(sig),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("register via web flow = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp registerResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.HasPrefix(resp.APIKey, "sk-om-") {
		t.Fatalf("api key = %q", resp.APIKey)
	}
}

func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0xf])
	}
	return string(out)
}
