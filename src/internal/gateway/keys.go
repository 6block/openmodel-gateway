package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// keys.go implements self-service API-key MANAGEMENT (key-store v2): multiple keys
// per wallet, revocation, and show-once listing — the register endpoint stays the
// unchanged one-shot "first key" flow (its 409 contract is documented API surface).
//
// Every management action is authorized by an EIP-191 wallet signature over a
// server-composed message (same trust model as registration: the wallet IS the
// account; API keys themselves deliberately cannot manage keys, so a leaked
// inference key can neither mint siblings nor revoke its peers).
//
//   GET  /v1/keys/message?wallet=…&action=…[&name=…][&key_id=…]  → exact text to sign
//   POST /v1/keys {wallet, action, issued_at, signature, name?, key_id?}
//        action=create → 200 {api_key: "sk-om-…", key: {…}}     // full key, shown ONCE
//        action=list   → 200 {keys: [{id,name,display,created_at,static}]}
//        action=delete → 200 {deleted: "k-…"}
//
// Replay/freshness rules are registration's: ±5min issued_at window, one-time
// signatures (shared seenSigs cache), regMu serializing all key-store mutations.

const maxKeysPerWalletDefault = 10

type keysRequest struct {
	Wallet    string `json:"wallet"`
	Action    string `json:"action"`
	IssuedAt  int64  `json:"issued_at"`
	Signature string `json:"signature"`
	Name      string `json:"name,omitempty"`
	KeyID     string `json:"key_id,omitempty"`
}

type keyInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Display   string `json:"display"`
	CreatedAt string `json:"created_at,omitempty"`
	Static    bool   `json:"static,omitempty"`
}

// keyManagementMessage is the exact text signed for a management action. Field
// order is fixed; optional fields are appended only when present so the message
// stays minimal and unambiguous (the server rebuilds it verbatim on POST).
func keyManagementMessage(action, wallet string, issuedAt int64, name, keyID string) string {
	msg := fmt.Sprintf("OpenModel key management\naction: %s\nwallet: %s\nissued_at: %d", action, wallet, issuedAt)
	if name != "" {
		msg += "\nname: " + name
	}
	if keyID != "" {
		msg += "\nkey_id: " + keyID
	}
	return msg
}

func validKeysAction(a string) bool { return a == "create" || a == "delete" || a == "list" }

// handleKeysMessage mirrors /v1/register/message for management actions: one
// source of truth for the signable text, zero crypto in the browser.
func (g *Gateway) handleKeysMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	action := q.Get("action")
	if !validKeysAction(action) {
		jsonError(w, "action must be create, delete or list", http.StatusBadRequest)
		return
	}
	if !common.IsHexAddress(q.Get("wallet")) {
		jsonError(w, "wallet must be a 0x EVM address", http.StatusBadRequest)
		return
	}
	canonical := common.HexToAddress(q.Get("wallet")).Hex()
	name := strings.TrimSpace(q.Get("name"))
	keyID := strings.TrimSpace(q.Get("key_id"))
	if action == "delete" && keyID == "" {
		jsonError(w, "delete requires key_id", http.StatusBadRequest)
		return
	}
	issuedAt := time.Now().Unix()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"wallet": canonical, "action": action, "issued_at": issuedAt,
		"name": name, "key_id": keyID,
		"message": keyManagementMessage(action, canonical, issuedAt, name, keyID),
	})
}

// handleKeys is the signed management dispatcher.
func (g *Gateway) handleKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed; use POST", http.StatusMethodNotAllowed)
		return
	}
	if g.regIPLimiter != nil && !g.regIPLimiter.allow(clientIP(r)) {
		jsonError(w, "too many key-management requests from this address; retry later", http.StatusTooManyRequests)
		return
	}
	var req keysRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if !validKeysAction(req.Action) {
		jsonError(w, "action must be create, delete or list", http.StatusBadRequest)
		return
	}
	if !common.IsHexAddress(req.Wallet) {
		jsonError(w, "wallet must be a 0x EVM address", http.StatusBadRequest)
		return
	}
	canonical := common.HexToAddress(req.Wallet).Hex()
	req.Name = strings.TrimSpace(req.Name)
	req.KeyID = strings.TrimSpace(req.KeyID)
	if req.Action == "delete" && req.KeyID == "" {
		jsonError(w, "delete requires key_id", http.StatusBadRequest)
		return
	}

	now := time.Now()
	skew := now.Sub(time.Unix(req.IssuedAt, 0))
	if skew < 0 {
		skew = -skew
	}
	if req.IssuedAt == 0 || skew > registrationWindow {
		jsonError(w, "issued_at missing or outside the ±5min window; fetch a fresh message", http.StatusBadRequest)
		return
	}
	msg := keyManagementMessage(req.Action, canonical, req.IssuedAt, req.Name, req.KeyID)
	if !verifyEthSignature(msg, req.Signature, canonical) {
		jsonError(w, "signature does not match wallet (sign the exact management message)", http.StatusUnauthorized)
		return
	}

	g.regMu.Lock()
	defer g.regMu.Unlock()
	g.pruneSeenSigsLocked(now)
	sigKey := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(req.Signature), "0x"))
	if _, seen := g.seenSigs[sigKey]; seen {
		jsonError(w, "signature already used", http.StatusConflict)
		return
	}
	g.seenSigs[sigKey] = now.Add(registrationWindow)

	switch req.Action {
	case "create":
		g.keysCreateLocked(w, canonical, req.Name, now)
	case "list":
		g.keysListLocked(w, canonical)
	case "delete":
		g.keysDeleteLocked(w, canonical, req.KeyID)
	}
}

func (g *Gateway) keysCreateLocked(w http.ResponseWriter, wallet, name string, now time.Time) {
	max := g.maxKeysPerWallet
	if max <= 0 {
		max = maxKeysPerWalletDefault
	}
	owned := 0
	for _, rec := range loadRegistrationsFile(g.registrationsPath, g.logger) {
		if strings.EqualFold(rec.Wallet, wallet) {
			owned++
		}
	}
	if owned >= max {
		jsonError(w, fmt.Sprintf("key limit reached (%d per wallet); delete one first", max), http.StatusConflict)
		return
	}
	if name == "" {
		name = fmt.Sprintf("key-%d", owned+1)
	}
	key, rec, err := g.createKeyLocked(wallet, name, now)
	if err != nil {
		g.logger.Error("failed to create key", "wallet", wallet, "error", err)
		jsonError(w, "failed to create key", http.StatusInternalServerError)
		return
	}
	// The wallet may be new to the balance refresher (first key minted via /v1/keys
	// rather than /v1/register).
	if g.balanceChecker != nil {
		g.balanceChecker.AddWallet(wallet)
	}
	g.logger.Info("api key created", "wallet", wallet, "key_id", rec.ID, "name", name)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"api_key": key, // shown once, never retrievable again
		"key": keyInfo{ID: rec.ID, Name: rec.Name, Display: rec.Display,
			CreatedAt: rec.CreatedAt.UTC().Format(time.RFC3339)},
	})
}

func (g *Gateway) keysListLocked(w http.ResponseWriter, wallet string) {
	keys := []keyInfo{}
	for _, rec := range loadRegistrationsFile(g.registrationsPath, g.logger) {
		if strings.EqualFold(rec.Wallet, wallet) {
			keys = append(keys, keyInfo{ID: rec.ID, Name: rec.Name, Display: rec.Display,
				CreatedAt: rec.CreatedAt.UTC().Format(time.RFC3339)})
		}
	}
	// Operator-configured keys bound to this wallet are shown (so the dashboard is
	// complete) but marked static: they live in gateway config, not the store, and
	// cannot be deleted here.
	g.keysMu.RLock()
	for _, e := range g.apiKeys {
		if e.Static && strings.EqualFold(e.Wallet, wallet) {
			keys = append(keys, keyInfo{Name: e.Name, Display: "(operator-configured)", Static: true})
		}
	}
	g.keysMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"wallet": wallet, "keys": keys})
}

func (g *Gateway) keysDeleteLocked(w http.ResponseWriter, wallet, keyID string) {
	recs := loadRegistrationsFile(g.registrationsPath, g.logger)
	idx := -1
	for i, rec := range recs {
		// Ownership is part of the match: another wallet's valid signature cannot
		// address this row, and the not-found answer leaks nothing about foreign ids.
		if rec.ID == keyID && strings.EqualFold(rec.Wallet, wallet) {
			idx = i
			break
		}
	}
	if idx < 0 {
		jsonError(w, "key not found for this wallet", http.StatusNotFound)
		return
	}
	removed := recs[idx]
	recs = append(recs[:idx], recs[idx+1:]...)
	if g.registrationsPath != "" {
		if err := writeRegistrationsFile(g.registrationsPath, recs); err != nil {
			g.logger.Error("failed to persist key deletion", "key_id", keyID, "error", err)
			jsonError(w, "failed to persist deletion", http.StatusInternalServerError)
			return
		}
	}
	g.keysMu.Lock()
	delete(g.apiKeys, removed.KeyHash) // revocation is immediate: next auth lookup misses
	g.keysMu.Unlock()
	g.logger.Info("api key deleted", "wallet", wallet, "key_id", keyID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"deleted": keyID})
}
