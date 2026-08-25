package gateway

import (
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/crypto"
)

// signKeysAction produces the wallet signature for a management action the same
// way a browser wallet would (EIP-191 over the server-composed message).
func signKeysAction(t *testing.T, priv *ecdsa.PrivateKey, action, wallet string, issuedAt int64, name, keyID string) string {
	t.Helper()
	sig, err := crypto.Sign(accounts.TextHash([]byte(keyManagementMessage(action, wallet, issuedAt, name, keyID))), priv)
	if err != nil {
		t.Fatal(err)
	}
	sig[64] += 27
	return "0x" + hexEncode(sig)
}

func postKeys(g *Gateway, body map[string]any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/keys", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	g.Handler().ServeHTTP(rec, req)
	return rec
}

func doKeysAction(t *testing.T, g *Gateway, priv *ecdsa.PrivateKey, wallet, action, name, keyID string) *httptest.ResponseRecorder {
	t.Helper()
	issued := time.Now().Unix()
	return postKeys(g, map[string]any{
		"wallet": wallet, "action": action, "issued_at": issued, "name": name, "key_id": keyID,
		"signature": signKeysAction(t, priv, action, wallet, issued, name, keyID),
	})
}

// persistentKeysGateway gives the gateway a real registrations file so
// hashed-persistence assertions can inspect the bytes on disk.
func persistentKeysGateway(t *testing.T) *Gateway {
	t.Helper()
	g := newRegTestGateway(t)
	g.registrationsPath = filepath.Join(t.TempDir(), "registrations.json")
	return g
}

func TestKeysCreateListDeleteLifecycle(t *testing.T) {
	g := persistentKeysGateway(t)
	priv, wallet := genWallet(t)

	// create two keys
	var keys []string
	for i, name := range []string{"laptop", "server"} {
		rec := doKeysAction(t, g, priv, wallet, "create", name, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("create %d = %d body=%s", i, rec.Code, rec.Body.String())
		}
		var resp struct {
			APIKey string  `json:"api_key"`
			Key    keyInfo `json:"key"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if !strings.HasPrefix(resp.APIKey, "sk-om-") || resp.Key.ID == "" || resp.Key.Display == "" {
			t.Fatalf("create resp = %+v", resp)
		}
		keys = append(keys, resp.APIKey)
	}

	// both authenticate (auth path hashes the bearer)
	for _, k := range keys {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+k)
		if _, ok := g.authenticate(req); !ok {
			t.Fatalf("key does not authenticate: %s", keyDisplay(k))
		}
	}

	// the store file must never contain a full key — only hashes and masks
	raw, err := os.ReadFile(g.registrationsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if strings.Contains(string(raw), k) {
			t.Fatal("plaintext key persisted to disk")
		}
	}
	if !strings.Contains(string(raw), `"key_hash"`) {
		t.Fatalf("store not in v2 form: %s", raw)
	}

	// list returns metadata only (no hash, no full key)
	rec := doKeysAction(t, g, priv, wallet, "list", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	var list struct {
		Keys []keyInfo `json:"keys"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Keys) != 2 {
		t.Fatalf("list = %+v", list)
	}
	if strings.Contains(rec.Body.String(), "key_hash") || strings.Contains(rec.Body.String(), keys[0]) {
		t.Fatal("list leaks hash or full key")
	}

	// delete the first key: revocation is immediate
	rec = doKeysAction(t, g, priv, wallet, "delete", "", list.Keys[0].ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d body=%s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+keys[0])
	if _, ok := g.authenticate(req); ok {
		t.Fatal("deleted key still authenticates")
	}
	req.Header.Set("Authorization", "Bearer "+keys[1])
	if _, ok := g.authenticate(req); !ok {
		t.Fatal("surviving key lost")
	}
}

func TestKeysDeleteForeignWalletRejected(t *testing.T) {
	g := persistentKeysGateway(t)
	privA, walletA := genWallet(t)
	privB, walletB := genWallet(t)

	rec := doKeysAction(t, g, privA, walletA, "create", "mine", "")
	var created struct {
		Key keyInfo `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// B signs a perfectly valid delete for A's key id → 404 (ownership is part of
	// the lookup; foreign ids are indistinguishable from nonexistent ones).
	rec = doKeysAction(t, g, privB, walletB, "delete", "", created.Key.ID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign delete = %d, want 404", rec.Code)
	}
	// A's key still lives
	rec = doKeysAction(t, g, privA, walletA, "list", "", "")
	if !strings.Contains(rec.Body.String(), created.Key.ID) {
		t.Fatal("victim key disappeared")
	}
}

func TestKeysPerWalletCap(t *testing.T) {
	g := persistentKeysGateway(t)
	g.maxKeysPerWallet = 2
	priv, wallet := genWallet(t)
	// Distinct names keep the signed messages distinct: identical create requests
	// in the same second would collide with the one-time-signature replay guard
	// (deterministic ECDSA), which is its own tested behavior.
	for i, name := range []string{"a", "b"} {
		if rec := doKeysAction(t, g, priv, wallet, "create", name, ""); rec.Code != http.StatusOK {
			t.Fatalf("create %d = %d", i, rec.Code)
		}
	}
	rec := doKeysAction(t, g, priv, wallet, "create", "c", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "key limit") {
		t.Fatalf("over-cap create = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestKeysReplayRejected(t *testing.T) {
	g := persistentKeysGateway(t)
	priv, wallet := genWallet(t)
	issued := time.Now().Unix()
	sig := signKeysAction(t, priv, "create", wallet, issued, "", "")
	body := map[string]any{"wallet": wallet, "action": "create", "issued_at": issued, "signature": sig}
	if rec := postKeys(g, body); rec.Code != http.StatusOK {
		t.Fatalf("first = %d", rec.Code)
	}
	if rec := postKeys(g, body); rec.Code != http.StatusConflict {
		t.Fatalf("replay = %d, want 409", rec.Code)
	}
}

func TestKeysWrongActionSignatureRejected(t *testing.T) {
	g := persistentKeysGateway(t)
	priv, wallet := genWallet(t)
	issued := time.Now().Unix()
	// signature over "list" cannot authorize "create" (action is inside the message)
	sig := signKeysAction(t, priv, "list", wallet, issued, "", "")
	rec := postKeys(g, map[string]any{"wallet": wallet, "action": "create", "issued_at": issued, "signature": sig})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("cross-action signature = %d, want 401", rec.Code)
	}
}

// Legacy v1 stores carried plaintext keys; loading must migrate them to hashes,
// scrub the plaintext from disk, and keep the old key authenticating.
func TestLegacyPlaintextStoreMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registrations.json")
	legacy := `[{"key":"sk-om-legacy-secret","name":"old","wallet":"0x9875c8D91fE91199D7B9207d78f5A592EFCc6f88","created_at":"2026-07-01T00:00:00Z"}]`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	recs := loadRegistrationsFile(path, testLogger())
	if len(recs) != 1 || recs[0].KeyHash != hashKey("sk-om-legacy-secret") || recs[0].Key != "" || recs[0].ID == "" {
		t.Fatalf("migrated rec = %+v", recs[0])
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "sk-om-legacy-secret") {
		t.Fatal("plaintext survived migration on disk")
	}

	// A gateway loading this store must accept the ORIGINAL key via hash lookup.
	g := newRegTestGateway(t)
	g.registrationsPath = path
	for _, rec := range loadRegistrationsFile(path, testLogger()) {
		g.apiKeys[rec.KeyHash] = apiKeyEntry{Name: rec.Name, Wallet: rec.Wallet, ID: rec.ID}
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer sk-om-legacy-secret")
	if e, ok := g.authenticate(req); !ok || e.Name != "old" {
		t.Fatalf("legacy key lookup failed: ok=%v e=%+v", ok, e)
	}
}

func TestKeysMessageEndpointDriftProof(t *testing.T) {
	g := webuiTestGateway(t)
	_, wallet := genWallet(t)
	rec := getPath(g, "/v1/keys/message?wallet="+strings.ToLower(wallet)+"&action=delete&key_id=k-abc123")
	if rec.Code != http.StatusOK {
		t.Fatalf("keys/message = %d body=%s", rec.Code, rec.Body.String())
	}
	var m struct {
		Wallet   string `json:"wallet"`
		Action   string `json:"action"`
		IssuedAt int64  `json:"issued_at"`
		KeyID    string `json:"key_id"`
		Message  string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m.Wallet != wallet {
		t.Fatalf("not canonicalized: %s", m.Wallet)
	}
	if m.Message != keyManagementMessage("delete", wallet, m.IssuedAt, "", "k-abc123") {
		t.Fatalf("message drifted: %q", m.Message)
	}
	if rec := getPath(g, "/v1/keys/message?wallet="+wallet+"&action=nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad action = %d", rec.Code)
	}
	if rec := getPath(g, "/v1/keys/message?wallet="+wallet+"&action=delete"); rec.Code != http.StatusBadRequest {
		t.Fatalf("delete sans key_id = %d", rec.Code)
	}
}
