package gateway

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// registration.go implements a minimal self-service API-key registration (M3 v1).
//
// A user proves control of an EVM wallet by signing a fixed, domain-bound message
// with that wallet's key (EIP-191 personal_sign), and in return gets an API key
// bound to the wallet — so usage on that key is billed to that wallet at settlement.
//
// Single request (no server-issued nonce, to keep client interaction minimal). Replay
// is bounded by three things: (1) the signed message carries an issued_at timestamp
// the server checks against a ±window; (2) a signature already accepted within the
// window is rejected; (3) one wallet maps to at most one key. M4 will add key
// rotation, multiple keys per wallet, and (optionally) a challenge-response flow.

// registrationWindow bounds how far the signed issued_at may be from server time.
const registrationWindow = 5 * time.Minute

// registrationRecord is one persisted self-registered key.
type registrationRecord struct {
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	Wallet    string    `json:"wallet"`
	CreatedAt time.Time `json:"created_at"`
}

type registerRequest struct {
	Wallet    string `json:"wallet"`
	IssuedAt  int64  `json:"issued_at"`
	Signature string `json:"signature"`
	Name      string `json:"name,omitempty"`
}

type registerResponse struct {
	APIKey string `json:"api_key"`
	Wallet string `json:"wallet"`
	Name   string `json:"name"`
}

// registrationMessage is the exact text a registrant signs. It is reconstructed
// verbatim on the server from the request's wallet + issued_at, so any change here
// must stay in lockstep with the documented client signing format.
func registrationMessage(wallet string, issuedAt int64) string {
	return fmt.Sprintf("OpenModel API key registration\nwallet: %s\nissued_at: %d", wallet, issuedAt)
}

// registrationsPathFor places the key store next to the request log (the data dir).
func registrationsPathFor(requestLogPath string) string {
	if requestLogPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(requestLogPath), "registrations.json")
}

// loadRegistrationsFile reads persisted registrations; a missing file means none yet.
func loadRegistrationsFile(path string, logger *slog.Logger) []registrationRecord {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var recs []registrationRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		if logger != nil {
			logger.Error("failed to parse registrations file; ignoring", "path", path, "error", err)
		}
		return nil
	}
	return recs
}

// verifyEthSignature reports whether sigHex is an EIP-191 personal_sign signature of
// message produced by the private key for wantAddr.
func verifyEthSignature(message, sigHex, wantAddr string) bool {
	sig, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(sigHex), "0x"))
	if err != nil || len(sig) != 65 {
		return false
	}
	// eth clients (MetaMask/ethers) set v ∈ {27,28}; SigToPub wants the recovery id {0,1}.
	if sig[64] >= 27 {
		sig[64] -= 27
	}
	if sig[64] != 0 && sig[64] != 1 {
		return false
	}
	pub, err := crypto.SigToPub(accounts.TextHash([]byte(message)), sig)
	if err != nil {
		return false
	}
	return strings.EqualFold(crypto.PubkeyToAddress(*pub).Hex(), wantAddr)
}

// generateAPIKey returns a fresh random key. "sk-om-" marks it as an OpenModel
// self-registered key (distinct from operator-configured keys).
func generateAPIKey() (string, error) {
	b := make([]byte, 24)
	if _, err := cryptorand.Read(b); err != nil {
		return "", err
	}
	return "sk-om-" + hex.EncodeToString(b), nil
}

// handleRegister: POST /v1/register {wallet, issued_at, signature, name?} → {api_key,...}.
func (g *Gateway) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed; use POST", http.StatusMethodNotAllowed)
		return
	}
	var req registerRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if !common.IsHexAddress(req.Wallet) {
		jsonError(w, "wallet must be a 0x EVM address", http.StatusBadRequest)
		return
	}
	canonical := common.HexToAddress(req.Wallet).Hex()

	// Freshness: signed timestamp must be within ±window of now.
	now := time.Now()
	skew := now.Sub(time.Unix(req.IssuedAt, 0))
	if skew < 0 {
		skew = -skew
	}
	if req.IssuedAt == 0 || skew > registrationWindow {
		jsonError(w, "issued_at missing or outside the ±5min window; use a current unix timestamp", http.StatusBadRequest)
		return
	}

	// Signature must recover to the claimed wallet (proves key ownership).
	if !verifyEthSignature(registrationMessage(canonical, req.IssuedAt), req.Signature, canonical) {
		jsonError(w, "signature does not match wallet (sign the exact registration message with that wallet's key)", http.StatusUnauthorized)
		return
	}

	// Serialize all registrations so the replay check and the wallet-uniqueness check
	// are atomic with the insert (no TOCTOU between concurrent registrations).
	g.regMu.Lock()
	defer g.regMu.Unlock()

	g.pruneSeenSigsLocked(now)
	sigKey := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(req.Signature), "0x"))
	if _, seen := g.seenSigs[sigKey]; seen {
		jsonError(w, "signature already used", http.StatusConflict)
		return
	}

	// One wallet → one key (also blocks claiming a wallet already in static config).
	if g.walletAlreadyRegistered(canonical) {
		jsonError(w, "wallet already registered", http.StatusConflict)
		return
	}

	key, err := generateAPIKey()
	if err != nil {
		jsonError(w, "failed to generate key", http.StatusInternalServerError)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "user-" + strings.ToLower(canonical[2:8])
	}

	if err := g.appendRegistration(registrationRecord{Key: key, Name: name, Wallet: canonical, CreatedAt: now}); err != nil {
		g.logger.Error("failed to persist registration", "wallet", canonical, "error", err)
		jsonError(w, "failed to persist registration", http.StatusInternalServerError)
		return
	}
	g.keysMu.Lock()
	g.apiKeys[key] = apiKeyEntry{Name: name, Wallet: canonical}
	g.keysMu.Unlock()
	g.seenSigs[sigKey] = now.Add(registrationWindow)

	// Register the wallet for on-chain balance refresh — otherwise a freshly-registered
	// user who has already deposited reads availableUSD=0 and is wrongly 402'd (and, until
	// this call existed, forever). No-op when settlement is disabled (nil checker).
	if g.balanceChecker != nil {
		g.balanceChecker.AddWallet(canonical)
	}

	g.logger.Info("api key registered", "wallet", canonical, "name", name)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(registerResponse{APIKey: key, Wallet: canonical, Name: name})
}

// pruneSeenSigsLocked drops expired replay-guard entries. Caller holds g.regMu.
func (g *Gateway) pruneSeenSigsLocked(now time.Time) {
	for sig, exp := range g.seenSigs {
		if now.After(exp) {
			delete(g.seenSigs, sig)
		}
	}
}

// walletAlreadyRegistered reports whether any existing key (static or registered) is
// bound to canonical. Caller holds g.regMu (serializing registrations).
func (g *Gateway) walletAlreadyRegistered(canonical string) bool {
	g.keysMu.RLock()
	defer g.keysMu.RUnlock()
	for _, e := range g.apiKeys {
		if strings.EqualFold(e.Wallet, canonical) {
			return true
		}
	}
	return false
}

// appendRegistration atomically rewrites the key store with the new record appended.
// Caller holds g.regMu. A no-op (in-memory only) when persistence is disabled.
func (g *Gateway) appendRegistration(rec registrationRecord) error {
	if g.registrationsPath == "" {
		return nil
	}
	recs := append(loadRegistrationsFile(g.registrationsPath, g.logger), rec)
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	tmp := g.registrationsPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, g.registrationsPath)
}
