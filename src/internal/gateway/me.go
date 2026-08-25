package gateway

import (
	"encoding/json"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/filecoin-project/go-address"

	"openmodel/sp-state-agent/internal/settlement"
)

// me.go — the account self-view (M4.1 user dashboard backend) and the f410
// funding helper.
//
//   GET /v1/me      (API-key auth)  balances as the GATEWAY sees them + recent usage
//   GET /v1/f4addr  (public)        0x → f410 form, for funding from exchanges/Lotus
//
// /v1/me answers the questions the chat UI could not: "does the gateway consider
// me funded yet?" (chain balance is polled every ~15s — right after a deposit the
// chain says yes while the 402 gate still says no), "what have I spent that is not
// settled yet?", and "what did my recent requests cost?".

// usageRing keeps the last N request records in memory for per-key recent-usage
// display. Deliberately not persisted: the request LOG is the billing record; this
// is a dashboard convenience and "since gateway start" semantics are acceptable.
type usageRing struct {
	mu   sync.Mutex
	buf  []usageEntry
	next int
	full bool
}

type usageEntry struct {
	KeyName    string    `json:"-"`
	Timestamp  time.Time `json:"ts"`
	Model      string    `json:"model"`
	Status     int       `json:"status"`
	Prompt     int       `json:"prompt_tokens"`
	Cached     int       `json:"cached_tokens,omitempty"`
	Completion int       `json:"completion_tokens"`
	CostUSD    string    `json:"cost_usd,omitempty"`
	RequestID  string    `json:"request_id"`
}

func newUsageRing(n int) *usageRing { return &usageRing{buf: make([]usageEntry, n)} }

func (u *usageRing) push(e usageEntry) {
	u.mu.Lock()
	u.buf[u.next] = e
	u.next = (u.next + 1) % len(u.buf)
	if u.next == 0 {
		u.full = true
	}
	u.mu.Unlock()
}

// lastByKey returns up to max most-recent entries for keyName, newest first.
func (u *usageRing) lastByKey(keyName string, max int) []usageEntry {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := u.next
	total := len(u.buf)
	if !u.full {
		total = u.next
	}
	out := []usageEntry{}
	for i := 0; i < total && len(out) < max; i++ {
		e := u.buf[((n-1-i)+len(u.buf))%len(u.buf)]
		if e.KeyName == keyName {
			out = append(out, e)
		}
	}
	return out
}

// recordUsage feeds the ring from the request tail (same call site as the billing
// log). Cost uses the SAME formula as billing (#47 single-caliber rule) so the
// dashboard never shows a number settlement would disagree with.
func (g *Gateway) recordUsage(rec RequestRecord) {
	if g.usage == nil {
		return
	}
	e := usageEntry{
		KeyName: rec.APIKeyName, Timestamp: rec.Timestamp, Model: rec.Model,
		Status: rec.Status, Prompt: rec.PromptTokens, Cached: rec.CachedTokens,
		Completion: rec.CompletionTokens, RequestID: rec.RequestID,
	}
	if rec.Status == http.StatusOK && g.modelPrices != nil {
		cost := settlement.CostBreakdownUSD(rec.Model, rec.PromptTokens, rec.CompletionTokens,
			rec.CachedTokens, rec.TotalTokens, g.modelPrices, g.catalogInput, g.catalogCacheRead)
		if cost != nil && cost.Sign() > 0 {
			e.CostUSD = cost.Text('f', 8)
		}
	}
	g.usage.push(e)
}

// handleMe: GET /v1/me — key-authenticated account view.
func (g *Gateway) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	entry, ok := g.authenticate(r)
	if !ok {
		jsonError(w, "invalid or missing Authorization header", http.StatusUnauthorized)
		return
	}
	resp := map[string]interface{}{
		"key":    map[string]interface{}{"name": entry.Name, "id": entry.ID, "static": entry.Static},
		"wallet": entry.Wallet,
	}
	if g.balanceChecker != nil && entry.Wallet != "" {
		bal := map[string]interface{}{}
		chainUSD := new(big.Float)
		const nativeToken = "0x0000000000000000000000000000000000000000"
		if price := g.balanceChecker.FILPriceUSD(); price != nil && price.Sign() > 0 {
			bal["fil_price_usd"] = price.Text('f', 4)
			// Balance-cache keys are the canonical checksummed wallet — the same
			// form apiKeyEntry.Wallet carries (both derive via HexToAddress().Hex()).
			if tokens, ok := g.balanceChecker.GetAllBalances()[entry.Wallet]; ok {
				fil := new(big.Float)
				if wei, ok := tokens[nativeToken]; ok {
					fil.Quo(new(big.Float).SetInt(wei), big.NewFloat(1e18))
				}
				bal["chain_fil"] = fil.Text('f', 6)
				chainUSD.Mul(fil, price)
			}
			// Per-token detail (human units) so multi-currency wallets can see
			// what backs the number below.
			toks := map[string]string{}
			for sym, v := range g.balanceChecker.TokenBalancesView(entry.Wallet) {
				toks[sym] = v.Text('f', 6)
			}
			if len(toks) > 0 {
				bal["tokens"] = toks
			}
			pending := g.balanceChecker.GetPendingSpend(entry.Wallet)
			bal["pending_usd"] = pending.Text('f', 6)
			// One caliber: the SAME multi-token, depeg-aware, buffer-adjusted
			// valuation the 402 gate uses. The previous hand-rolled FIL-only sum
			// showed $0 to a wallet whose USDFC the gate was accepting.
			available := g.balanceChecker.AvailableUSDView(entry.Wallet)
			if available.Sign() < 0 {
				available = new(big.Float)
			}
			bal["available_usd"] = available.Text('f', 6)
		}
		resp["balance"] = bal
	}
	if g.usage != nil {
		resp["recent_usage"] = g.usage.lastByKey(entry.Name, 20)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleF4Addr: GET /v1/f4addr?wallet=0x… — the f410 (delegated) form of an EVM
// address, which is what exchanges/Lotus wallets need as a send target. Public:
// pure derivation, no state. The f/t prefix follows address.CurrentNetwork, set at
// startup from the settlement chain id.
func (g *Gateway) handleF4Addr(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	wallet := r.URL.Query().Get("wallet")
	if !common.IsHexAddress(wallet) {
		jsonError(w, "wallet must be a 0x EVM address", http.StatusBadRequest)
		return
	}
	ethAddr := common.HexToAddress(wallet)
	f4, err := address.NewDelegatedAddress(10, ethAddr.Bytes()) // namespace 10 = EAM
	if err != nil {
		jsonError(w, "derivation failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"wallet": ethAddr.Hex(),
		"f4":     f4.String(),
	})
}
