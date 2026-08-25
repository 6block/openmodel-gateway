package filecoin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	bls12381 "github.com/drand/kyber-bls12381"
	"github.com/drand/kyber/sign/bls"
	"github.com/drand/kyber/util/random"
	"github.com/ethereum/go-ethereum/crypto"
	faddr "github.com/filecoin-project/go-address"
	"golang.org/x/crypto/blake2b"
)

// ---- signature verification ----

// secpSignLikeLotus reproduces Lotus's secp256k1 WalletSign for arbitrary bytes:
// ECDSA-recoverable signature over blake2b-256(msg), V ∈ {0,1}.
func secpSignLikeLotus(t *testing.T, msg []byte) (sig []byte, addr string) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	digest := blake2b.Sum256(msg)
	sig, err = crypto.Sign(digest[:], key)
	if err != nil {
		t.Fatal(err)
	}
	a, err := faddr.NewSecp256k1Address(crypto.FromECDSAPub(&key.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	return sig, a.String()
}

func TestVerifySecp256k1(t *testing.T) {
	msg := []byte("OpenModel SP registration\nminer: t01000\nchallenge: abc")
	sig, addr := secpSignLikeLotus(t, msg)

	if err := VerifySignature(SigTypeSecp256k1, sig, addr, msg); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}

	// Ethereum-style V offset (27/28) must be normalized, not rejected.
	off := make([]byte, len(sig))
	copy(off, sig)
	off[64] += 27
	if err := VerifySignature(SigTypeSecp256k1, off, addr, msg); err != nil {
		t.Fatalf("V+27 signature rejected: %v", err)
	}

	if err := VerifySignature(SigTypeSecp256k1, sig, addr, []byte("tampered")); err == nil {
		t.Fatal("tampered message accepted")
	}
	_, otherAddr := secpSignLikeLotus(t, msg)
	if err := VerifySignature(SigTypeSecp256k1, sig, otherAddr, msg); err == nil {
		t.Fatal("signature accepted for a different signer")
	}
	if err := VerifySignature(SigTypeSecp256k1, sig[:64], addr, msg); err == nil {
		t.Fatal("truncated signature accepted")
	}
}

func TestVerifyBLS(t *testing.T) {
	msg := []byte("OpenModel SP registration\nminer: t01001\nchallenge: def")

	suite := bls12381.NewBLS12381Suite()
	scheme := bls.NewSchemeOnG2(suite)
	priv, pub := scheme.NewKeyPair(random.New())
	_ = priv
	sig, err := scheme.Sign(priv, msg)
	if err != nil {
		t.Fatal(err)
	}
	pubBytes, err := pub.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	a, err := faddr.NewBLSAddress(pubBytes)
	if err != nil {
		t.Fatal(err)
	}

	if err := VerifySignature(SigTypeBLS, sig, a.String(), msg); err != nil {
		t.Fatalf("valid BLS signature rejected: %v", err)
	}
	if err := VerifySignature(SigTypeBLS, sig, a.String(), []byte("tampered")); err == nil {
		t.Fatal("tampered message accepted")
	}
	_, pub2 := scheme.NewKeyPair(random.New())
	pub2Bytes, _ := pub2.MarshalBinary()
	a2, _ := faddr.NewBLSAddress(pub2Bytes)
	if err := VerifySignature(SigTypeBLS, sig, a2.String(), msg); err == nil {
		t.Fatal("signature accepted for a different signer")
	}
}

func TestVerifyTypeAddressMismatch(t *testing.T) {
	msg := []byte("m")
	sig, secpAddr := secpSignLikeLotus(t, msg)
	// Claiming BLS type with an f1 address must fail before any crypto runs.
	if err := VerifySignature(SigTypeBLS, sig, secpAddr, msg); err == nil {
		t.Fatal("bls type with secp address accepted")
	}
	if err := VerifySignature(99, sig, secpAddr, msg); err == nil {
		t.Fatal("unknown signature type accepted")
	}
}

func TestSameKey(t *testing.T) {
	msg := []byte("x")
	_, addr := secpSignLikeLotus(t, msg)
	// t/f prefix must not matter.
	other := "f" + addr[1:]
	if !SameKey(addr, other) {
		t.Fatalf("SameKey(%s, %s) = false, want true", addr, other)
	}
	_, addr2 := secpSignLikeLotus(t, msg)
	if SameKey(addr, addr2) {
		t.Fatal("distinct keys reported equal")
	}
	if SameKey(addr, "not-an-address") {
		t.Fatal("garbage address reported equal")
	}
}

// TestRealLotusFixtures verifies byte-compatibility against signatures produced by a
// REAL Lotus node (`Filecoin.WalletSign`). The fixture file is generated on the
// miner hosts during deployment validation; the test is skipped when absent so CI
// stays green before that step.
//
// Fixture format (testdata/lotus-sig-fixtures.json):
//
//	[{"signer": "t3...", "sig_type": 2, "sig_base64": "...", "message": "..."}]
func TestRealLotusFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/lotus-sig-fixtures.json")
	if err != nil {
		t.Skip("no real-Lotus fixtures present (generated during deployment validation)")
	}
	var fixtures []struct {
		Signer    string `json:"signer"`
		SigType   uint64 `json:"sig_type"`
		SigBase64 string `json:"sig_base64"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("bad fixture file: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("fixture file present but empty")
	}
	for i, f := range fixtures {
		sig, err := base64.StdEncoding.DecodeString(f.SigBase64)
		if err != nil {
			t.Fatalf("fixture %d: bad base64: %v", i, err)
		}
		if err := VerifySignature(f.SigType, sig, f.Signer, []byte(f.Message)); err != nil {
			t.Errorf("fixture %d (%s type %d): real Lotus signature rejected: %v", i, f.Signer, f.SigType, err)
		}
		if err := VerifySignature(f.SigType, sig, f.Signer, []byte(f.Message+"x")); err == nil {
			t.Errorf("fixture %d: tampered message accepted", i)
		}
	}
}

// ---- rpc client ----

// fakeLotus builds a JSON-RPC test server answering the given method with result
// (or an HTTP failure when result is nil).
func fakeLotus(t *testing.T, handler func(method string, params []json.RawMessage) (any, bool)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
			ID     int               `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		result, ok := handler(req.Method, req.Params)
		if !ok {
			http.Error(w, "boom", 500)
			return
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestClientMinerInfoAndFailover(t *testing.T) {
	dead := fakeLotus(t, func(string, []json.RawMessage) (any, bool) { return nil, false })
	defer dead.Close()
	good := fakeLotus(t, func(method string, _ []json.RawMessage) (any, bool) {
		switch method {
		case "Filecoin.StateMinerInfo":
			return map[string]any{"Owner": "t0100", "Worker": "t0101"}, true
		case "Filecoin.StateAccountKey":
			return "t3wxyz", true
		}
		return nil, false
	})
	defer good.Close()

	c := NewClient([]string{dead.URL, good.URL}, testLogger())
	owner, worker, err := c.MinerInfo(context.Background(), "t01234")
	if err != nil {
		t.Fatalf("MinerInfo with failover: %v", err)
	}
	if owner != "t0100" || worker != "t0101" {
		t.Fatalf("got owner=%s worker=%s", owner, worker)
	}
	key, err := c.AccountKey(context.Background(), "t0100")
	if err != nil || key != "t3wxyz" {
		t.Fatalf("AccountKey = %q, %v", key, err)
	}
}

func TestClientCrossChecksFakeZero(t *testing.T) {
	zero := fakeLotus(t, func(method string, _ []json.RawMessage) (any, bool) {
		return map[string]any{"MinerPower": map[string]any{"RawBytePower": "0"}}, true
	})
	defer zero.Close()
	real := fakeLotus(t, func(method string, _ []json.RawMessage) (any, bool) {
		return map[string]any{"MinerPower": map[string]any{"RawBytePower": "34359738368"}}, true
	})
	defer real.Close()

	// A fake zero from the first endpoint must not shadow the real value.
	c := NewClient([]string{zero.URL, real.URL}, testLogger())
	p, err := c.MinerRawPower(context.Background(), "t01234")
	if err != nil {
		t.Fatal(err)
	}
	if want := big.NewInt(34359738368); p.Cmp(want) != 0 {
		t.Fatalf("power = %s, want %s (fake zero shadowed the real value)", p, want)
	}

	// All endpoints agreeing on zero IS zero.
	c2 := NewClient([]string{zero.URL}, testLogger())
	p2, err := c2.MinerRawPower(context.Background(), "t01234")
	if err != nil {
		t.Fatal(err)
	}
	if p2.Sign() != 0 {
		t.Fatalf("power = %s, want 0", p2)
	}
}

func TestClientActorBalance(t *testing.T) {
	srv := fakeLotus(t, func(method string, _ []json.RawMessage) (any, bool) {
		if method != "Filecoin.StateGetActor" {
			return nil, false
		}
		return map[string]any{"Balance": "2500000000000000000000"}, true
	})
	defer srv.Close()
	c := NewClient([]string{srv.URL}, testLogger())
	b, err := c.ActorBalance(context.Background(), "t01234")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := new(big.Int).SetString("2500000000000000000000", 10)
	if b.Cmp(want) != 0 {
		t.Fatalf("balance = %s, want %s", b, want)
	}
}

func TestClientAllEndpointsDown(t *testing.T) {
	dead := fakeLotus(t, func(string, []json.RawMessage) (any, bool) { return nil, false })
	dead.Close() // actually unreachable
	c := NewClient([]string{dead.URL}, testLogger())
	if _, _, err := c.MinerInfo(context.Background(), "t01234"); err == nil {
		t.Fatal("expected error when every endpoint is down")
	}
	if _, err := c.MinerRawPower(context.Background(), "t01234"); err == nil {
		t.Fatal("expected error when every endpoint is down")
	}
}
