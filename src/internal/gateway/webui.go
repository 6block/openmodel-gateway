package gateway

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// webui.go serves the embedded chat + wallet-registration single-page app (M4.1)
// and its two supporting read-only endpoints. Everything is same-origin with the
// inference API, so the browser app needs no CORS and no separate deployment: the
// gateway binary IS the website.
//
//   GET /                      → index.html (when web_ui.enabled)
//   GET /app.js, /style.css    → embedded static assets
//   GET /v1/webconfig          → chain/contract parameters the app needs (public)
//   GET /v1/register/message   → the EXACT registration text to sign (public)
//
// /v1/register/message exists so the message format has ONE source of truth: the
// server builds the string with the same registrationMessage() used to verify it,
// and the browser signs those bytes verbatim. A hand-rolled client copy would have
// to reimplement EIP-55 checksumming and track any future format change.

//go:embed webui
var webuiFS embed.FS

// chainPresets provides wallet_addEthereumChain parameters for the chains the
// gateway can settle on. Display/wallet-bootstrap data only — the gateway never
// calls these RPCs itself (its own chain access is settlement.rpc_url/rpc_urls).
var chainPresets = map[int64]map[string]interface{}{
	314: {
		"chainName": "Filecoin Mainnet",
		"rpcUrls":   []string{"https://api.node.glif.io/rpc/v1", "https://rpc.ankr.com/filecoin"},
		"nativeCurrency": map[string]interface{}{
			"name": "Filecoin", "symbol": "FIL", "decimals": 18,
		},
		"blockExplorerUrls": []string{"https://filfox.info/en"},
	},
	314159: {
		"chainName": "Filecoin Calibration Testnet",
		"rpcUrls":   []string{"https://api.calibration.node.glif.io/rpc/v1"},
		"nativeCurrency": map[string]interface{}{
			"name": "Test Filecoin", "symbol": "tFIL", "decimals": 18,
		},
		"blockExplorerUrls": []string{"https://calibration.filfox.info/en"},
	},
}

// faucetURLs: testnet funding helpers surfaced in the deposit card. Mainnet has
// none — the UI shows exchange/f410 guidance instead.
var faucetURLs = map[int64]string{
	314159: "https://faucet.calibnet.chainsafe-fil.io",
}

// RegisterWebUI mounts the app routes onto the gateway mux. Called from Handler()
// only when web_ui.enabled — with the section absent the mux behaves exactly as
// before (no "/" route, /v1/webconfig falls into handleUnsupported).
func (g *Gateway) registerWebUI(mux *http.ServeMux) {
	sub, err := fs.Sub(webuiFS, "webui")
	if err != nil { // embed layout is fixed at compile time; impossible in practice
		g.logger.Error("webui assets unavailable", "error", err)
		return
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			jsonError(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		// Exact known assets only; anything else on "/" is a 404, not index.html —
		// wrong API paths must keep failing loudly rather than returning HTML.
		// ServeFileFS (not FileServer) because the file name is decoupled from
		// r.URL.Path: FileServer would 301-canonicalize "/index.html" to "/".
		var name string
		switch r.URL.Path {
		case "/", "/index.html":
			name = "index.html"
		case "/app.js", "/style.css", "/favicon.svg":
			name = strings.TrimPrefix(r.URL.Path, "/")
		default:
			jsonError(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Cache-Control", "no-cache") // tiny assets; always fresh after redeploys
		http.ServeFileFS(w, r, sub, name)
	})
	mux.HandleFunc("/v1/webconfig", g.handleWebConfig)
}

// handleWebConfig exposes the non-secret parameters the browser app needs to talk
// to the wallet and the settlement contract. Public by design: chain id, contract
// address and price are all on-chain public data anyway.
func (g *Gateway) handleWebConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	resp := map[string]interface{}{
		"registration_enabled": true,
		"public_query_url":     strings.TrimRight(g.webUI.PublicQueryURL, "/"),
		"tokens":               g.webTokens(),
	}
	if g.settlementCfg != nil {
		resp["settlement_enabled"] = true
		resp["chain_id"] = g.settlementCfg.ChainID
		resp["contract_address"] = g.settlementCfg.ContractAddress
		if preset, ok := chainPresets[g.settlementCfg.ChainID]; ok {
			resp["chain"] = preset
		}
		if faucet, ok := faucetURLs[g.settlementCfg.ChainID]; ok {
			resp["faucet_url"] = faucet
		}
		// Pricing transparency for the pre-login UI (deposit sizing, price hints).
		// USD per 1M tokens, same catalog the biller uses; the live FIL/USD rate
		// lets the client translate. Post-login /v1/catalog remains the full view.
		prices := map[string]interface{}{}
		if p, ok := g.settlementCfg.ModelPricesUSD["default"]; ok {
			prices["default_output_per_mtok"] = p
		}
		if info, ok := g.settlementCfg.ModelCatalog["default"]; ok {
			prices["default_input_per_mtok"] = info.InputUSD
			prices["default_cache_read_per_mtok"] = info.CacheReadUSD
		}
		if len(prices) > 0 {
			resp["prices_usd"] = prices
		}
		// Per-model pricing (input = cache-miss prompt, cache_read = cache-hit
		// prompt, output = completion; USD per 1M tokens). Public so the UI can show
		// a pricing table and the current model's rate WITHOUT a key. Same resolver
		// the authed catalog + biller use, so displayed prices match what is charged.
		models := g.resolveModels()
		if len(models) > 0 {
			mp := make([]map[string]interface{}, 0, len(models))
			for _, m := range models {
				mp = append(mp, map[string]interface{}{
					"id": m.ID, "input_per_mtok": m.Input, "output_per_mtok": m.Output,
					"cache_read_per_mtok": m.CacheRead, "available": m.Available,
				})
			}
			resp["models_pricing"] = mp
		}
		if g.balanceChecker != nil {
			if p := g.balanceChecker.FILPriceUSD(); p != nil && p.Sign() > 0 {
				resp["fil_price_usd"] = p.Text('f', 4)
			}
		}
	} else {
		resp["settlement_enabled"] = false
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleRegisterMessage returns the exact registration text for a wallet at the
// current server time. Stateless and side-effect-free: signing happens in the
// user's wallet, and the subsequent POST /v1/register re-derives this same string.
func (g *Gateway) handleRegisterMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	wallet := r.URL.Query().Get("wallet")
	if !common.IsHexAddress(wallet) {
		jsonError(w, "wallet must be a 0x EVM address", http.StatusBadRequest)
		return
	}
	canonical := common.HexToAddress(wallet).Hex()
	issuedAt := time.Now().Unix()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"wallet":    canonical,
		"issued_at": issuedAt,
		"message":   registrationMessage(canonical, issuedAt),
	})
}

// webTokens exposes the settlement token list (symbol/address/decimals) so the
// web UI can render deposit/withdraw currency choices. FIL (the native zero
// address) is always present even without settlement config.
func (g *Gateway) webTokens() []map[string]any {
	out := []map[string]any{{"symbol": "FIL", "address": "0x0000000000000000000000000000000000000000", "decimals": 18}}
	if g.settlementCfg != nil {
		for _, t := range g.settlementCfg.SupportedTokens {
			if t.Symbol == "FIL" {
				continue
			}
			out = append(out, map[string]any{"symbol": t.Symbol, "address": t.Address, "decimals": t.Decimals})
		}
	}
	return out
}
