package gateway

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/crypto"
)

func newRegTestGateway(t *testing.T) *Gateway {
	t.Helper()
	return &Gateway{
		apiKeys:           map[string]apiKeyEntry{},
		seenSigs:          map[string]time.Time{},
		registrationsPath: filepath.Join(t.TempDir(), "registrations.json"),
		authEnabled:       true,
		logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func genWallet(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	priv, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return priv, crypto.PubkeyToAddress(priv.PublicKey).Hex()
}

// signFor produces an EIP-191 personal_sign signature over the registration message,
// with v ∈ {27,28} as a real eth client (MetaMask/ethers) would.
func signFor(t *testing.T, priv *ecdsa.PrivateKey, wallet string, issuedAt int64) string {
	t.Helper()
	sig, err := crypto.Sign(accounts.TextHash([]byte(registrationMessage(wallet, issuedAt))), priv)
	if err != nil {
		t.Fatal(err)
	}
	sig[64] += 27
	return "0x" + hex.EncodeToString(sig)
}

func postRegister(g *Gateway, body map[string]any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/register", bytes.NewReader(b))
	rr := httptest.NewRecorder()
	g.handleRegister(rr, req)
	return rr
}

func TestRegisterHappyPath(t *testing.T) {
	g := newRegTestGateway(t)
	priv, wallet := genWallet(t)
	issued := time.Now().Unix()
	rr := postRegister(g, map[string]any{"wallet": wallet, "issued_at": issued, "signature": signFor(t, priv, wallet, issued)})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp registerResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.APIKey == "" || resp.Wallet != wallet {
		t.Fatalf("bad response: %+v", resp)
	}
	// the issued key authenticates and resolves to the wallet (for billing)
	g.keysMu.RLock()
	e, ok := g.apiKeys[hashKey(resp.APIKey)]
	g.keysMu.RUnlock()
	if !ok || e.Wallet != wallet {
		t.Fatalf("key not registered/bound: ok=%v entry=%+v", ok, e)
	}
	// persisted to disk
	if recs := loadRegistrationsFile(g.registrationsPath, g.logger); len(recs) != 1 || recs[0].Wallet != wallet {
		t.Fatalf("registration not persisted: %+v", recs)
	}
}

func TestRegisterWrongSignatureRejected(t *testing.T) {
	g := newRegTestGateway(t)
	_, wallet := genWallet(t)
	otherPriv, _ := genWallet(t) // signature from a DIFFERENT key
	issued := time.Now().Unix()
	rr := postRegister(g, map[string]any{"wallet": wallet, "issued_at": issued, "signature": signFor(t, otherPriv, wallet, issued)})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong signature, got %d", rr.Code)
	}
}

func TestRegisterReplayRejected(t *testing.T) {
	g := newRegTestGateway(t)
	priv, wallet := genWallet(t)
	issued := time.Now().Unix()
	sig := signFor(t, priv, wallet, issued)
	if rr := postRegister(g, map[string]any{"wallet": wallet, "issued_at": issued, "signature": sig}); rr.Code != http.StatusOK {
		t.Fatalf("first register should succeed, got %d", rr.Code)
	}
	rr := postRegister(g, map[string]any{"wallet": wallet, "issued_at": issued, "signature": sig})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for replayed signature, got %d", rr.Code)
	}
}

func TestRegisterWalletAlreadyRegistered(t *testing.T) {
	g := newRegTestGateway(t)
	priv, wallet := genWallet(t)
	issued := time.Now().Unix()
	if rr := postRegister(g, map[string]any{"wallet": wallet, "issued_at": issued, "signature": signFor(t, priv, wallet, issued)}); rr.Code != http.StatusOK {
		t.Fatalf("first register should succeed, got %d", rr.Code)
	}
	// fresh, valid signature for the SAME wallet (different timestamp) must still be refused
	issued2 := issued + 1
	rr := postRegister(g, map[string]any{"wallet": wallet, "issued_at": issued2, "signature": signFor(t, priv, wallet, issued2)})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for already-registered wallet, got %d", rr.Code)
	}
}

func TestRegisterCannotHijackStaticConfigWallet(t *testing.T) {
	g := newRegTestGateway(t)
	priv, wallet := genWallet(t)
	// wallet is already bound to an operator-configured key
	g.apiKeys[hashKey("sk-static")] = apiKeyEntry{Name: "client", Wallet: wallet, Static: true}
	issued := time.Now().Unix()
	rr := postRegister(g, map[string]any{"wallet": wallet, "issued_at": issued, "signature": signFor(t, priv, wallet, issued)})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 (cannot claim a wallet already in config), got %d", rr.Code)
	}
}

func TestRegisterStaleTimestampRejected(t *testing.T) {
	g := newRegTestGateway(t)
	priv, wallet := genWallet(t)
	issued := time.Now().Add(-30 * time.Minute).Unix() // well outside the ±5min window
	rr := postRegister(g, map[string]any{"wallet": wallet, "issued_at": issued, "signature": signFor(t, priv, wallet, issued)})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for stale timestamp, got %d", rr.Code)
	}
}

func TestRegisterBadWalletRejected(t *testing.T) {
	g := newRegTestGateway(t)
	issued := time.Now().Unix()
	rr := postRegister(g, map[string]any{"wallet": "not-an-address", "issued_at": issued, "signature": "0xdeadbeef"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad wallet, got %d", rr.Code)
	}
}

func TestRegisterMethodNotAllowed(t *testing.T) {
	g := newRegTestGateway(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/register", nil)
	rr := httptest.NewRecorder()
	g.handleRegister(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d", rr.Code)
	}
}
