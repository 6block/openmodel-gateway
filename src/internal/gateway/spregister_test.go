package gateway

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	faddr "github.com/filecoin-project/go-address"
	"golang.org/x/crypto/blake2b"

	"openmodel/sp-state-agent/internal/filecoin"
	"openmodel/sp-state-agent/internal/worker"
)

// ---- fakes & helpers ----

// fakeChain implements ChainReader with fixed answers.
type fakeChain struct {
	ownerID, workerID   string // ID addresses
	ownerKey, workerKey string // f1/f3 key addresses
	power               *big.Int
	balance             *big.Int
	minerInfoErr        error
}

func (f *fakeChain) MinerInfo(_ context.Context, miner string) (string, string, error) {
	if f.minerInfoErr != nil {
		return "", "", f.minerInfoErr
	}
	return f.ownerID, f.workerID, nil
}

func (f *fakeChain) AccountKey(_ context.Context, idAddr string) (string, error) {
	switch idAddr {
	case f.ownerID:
		return f.ownerKey, nil
	case f.workerID:
		return f.workerKey, nil
	}
	return "", fmt.Errorf("unknown id address %s", idAddr)
}

func (f *fakeChain) MinerRawPower(_ context.Context, miner string) (*big.Int, error) {
	if f.power == nil {
		return new(big.Int), nil
	}
	return f.power, nil
}

func (f *fakeChain) ActorBalance(_ context.Context, addr string) (*big.Int, error) {
	if f.balance == nil {
		return new(big.Int), nil
	}
	return f.balance, nil
}

// minerKey is a secp256k1 key playing the role of a miner's worker/owner key,
// signing exactly like Lotus WalletSign does for arbitrary bytes.
type minerKey struct {
	priv *ecdsa.PrivateKey
	addr string // f1 address string
}

func newMinerKey(t *testing.T) *minerKey {
	t.Helper()
	priv, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	a, err := faddr.NewSecp256k1Address(crypto.FromECDSAPub(&priv.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	return &minerKey{priv: priv, addr: a.String()}
}

func (k *minerKey) sign(t *testing.T, msg string) spSignature {
	t.Helper()
	digest := blake2b.Sum256([]byte(msg))
	sig, err := crypto.Sign(digest[:], k.priv)
	if err != nil {
		t.Fatal(err)
	}
	return spSignature{Type: filecoin.SigTypeSecp256k1, Data: base64.StdEncoding.EncodeToString(sig)}
}

const testGatewayID = "openmodel-test-gw"

// newSPTestGateway builds a gateway with SP self-registration enabled against the
// given fake chain. Returns the gateway and its (persisted) worker registry.
func newSPTestGateway(t *testing.T, chain ChainReader, opts SPRegistrationOptions) *Gateway {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := worker.NewRegistry(logger, filepath.Join(t.TempDir(), "workers.json"))
	g := &Gateway{
		registry: reg,
		sessions: newSessionAffinity(0),
		logger:   logger,
	}
	if opts.GatewayID == "" {
		opts.GatewayID = testGatewayID
	}
	if opts.RegisterRatePerMin == 0 {
		opts.RegisterRatePerMin = 10000 // most tests hammer one IP; the limiter has its own test
	}
	g.EnableSPRegistration(opts, chain)
	return g
}

func getChallenge(t *testing.T, g *Gateway, miner string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"miner_id": miner})
	req := httptest.NewRequest(http.MethodPost, "/v1/sp/challenge", bytes.NewReader(b))
	req.RemoteAddr = "10.1.2.3:5555"
	rr := httptest.NewRecorder()
	g.handleSPChallenge(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("challenge: status %d body %s", rr.Code, rr.Body.String())
	}
	var resp spChallengeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Challenge
}

// spReq is a mutable register request builder with valid defaults.
type spReq struct {
	miner, workerID, payout, endpoint, schedURL, challenge, signer string
	csr                                                            string
	sig                                                            spSignature
}

func defaultSPReq(miner, challenge string) *spReq {
	return &spReq{
		miner:     miner,
		workerID:  "sp-" + miner,
		payout:    "0x9965507D1a55bcC2695C58ba16FB37d819B0A4dc",
		endpoint:  "http://198.51.100.7:8000",
		schedURL:  "http://198.51.100.7:8090",
		challenge: challenge,
	}
}

func (r *spReq) message(gatewayID string) string {
	return buildSPRegistrationMessage(r.miner, gatewayID, r.workerID, r.payout, r.endpoint, r.schedURL, r.challenge)
}

func postSPRegister(t *testing.T, g *Gateway, r *spReq) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(spRegisterRequest{
		MinerID:       r.miner,
		WorkerID:      r.workerID,
		PayoutAddress: r.payout,
		Endpoint:      r.endpoint,
		SchedulerURL:  r.schedURL,
		Challenge:     r.challenge,
		Signer:        r.signer,
		Signature:     r.sig,
		CSR:           r.csr,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/sp/register", bytes.NewReader(body))
	req.RemoteAddr = "10.1.2.3:5555"
	rr := httptest.NewRecorder()
	g.handleSPRegister(rr, req)
	return rr
}

// registerHappy drives the full challenge→sign→register flow with the worker key.
func registerHappy(t *testing.T, g *Gateway, chain *fakeChain, miner string, key *minerKey) spRegisterResponse {
	t.Helper()
	r := defaultSPReq(miner, getChallenge(t, g, miner))
	r.signer = key.addr
	r.sig = key.sign(t, r.message(testGatewayID))
	rr := postSPRegister(t, g, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("register: status %d body %s", rr.Code, rr.Body.String())
	}
	var resp spRegisterResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// ---- tests ----

// TestSPMessageGolden pins the exact signed text. The M1 side
// (go-scheduler internal/spregister buildRegistrationMessage) carries the SAME
// golden test — if either side drifts, its golden test fails first.
func TestSPMessageGolden(t *testing.T) {
	got := buildSPRegistrationMessage(
		"t01000", "openmodel-test-gw", "sp-t01000",
		"0x9965507D1a55bcC2695C58ba16FB37d819B0A4dc",
		"http://198.51.100.7:8000", "http://198.51.100.7:8090", "abc123")
	want := "OpenModel SP registration\n" +
		"miner: t01000\n" +
		"gateway: openmodel-test-gw\n" +
		"worker_id: sp-t01000\n" +
		"payout: 0x9965507D1a55bcC2695C58ba16FB37d819B0A4dc\n" +
		"endpoint: http://198.51.100.7:8000\n" +
		"scheduler_url: http://198.51.100.7:8090\n" +
		"challenge: abc123"
	if got != want {
		t.Fatalf("message drifted from the M1 contract:\n got: %q\nwant: %q", got, want)
	}
}

func TestSPRegisterHappyPathWorkerKey(t *testing.T) {
	workerKey := newMinerKey(t)
	ownerKey := newMinerKey(t)
	chain := &fakeChain{
		ownerID: "t0900", workerID: "t0901",
		ownerKey: ownerKey.addr, workerKey: workerKey.addr,
		power:   big.NewInt(64 << 30), // 64 GiB
		balance: big.NewInt(1e18),
	}
	minPower := big.NewInt(32 << 30)
	g := newSPTestGateway(t, chain, SPRegistrationOptions{
		MinRawPowerBytes: minPower,
		MinMinerBalance:  big.NewInt(1e17),
	})

	resp := registerHappy(t, g, chain, "t01000", workerKey)
	if !strings.HasPrefix(resp.AuthToken, "wk-") || len(resp.AuthToken) < 20 {
		t.Fatalf("bad auth token %q", resp.AuthToken)
	}
	if resp.Rotated {
		t.Fatal("fresh registration reported rotated")
	}

	w, ok := g.registry.Get("sp-t01000")
	if !ok {
		t.Fatal("worker not in registry")
	}
	if w.MinerAddress != "t01000" || !w.SelfRegistered || w.AuthToken != resp.AuthToken {
		t.Fatalf("bad worker record: %+v", w)
	}
	// Payout stored in EIP-55 canonical form and surfaced to settlement.
	if w.PayoutAddress != "0x9965507D1a55bcC2695C58ba16FB37d819B0A4dc" {
		t.Fatalf("payout = %s", w.PayoutAddress)
	}
	m := g.registry.ListMinerPayoutMap()
	if m["t01000"] != w.PayoutAddress {
		t.Fatalf("payout map = %v", m)
	}
}

func TestSPRegisterOwnerKeyAlsoAccepted(t *testing.T) {
	workerKey := newMinerKey(t)
	ownerKey := newMinerKey(t)
	chain := &fakeChain{
		ownerID: "t0900", workerID: "t0901",
		ownerKey: ownerKey.addr, workerKey: workerKey.addr,
		power: big.NewInt(1), balance: big.NewInt(1),
	}
	g := newSPTestGateway(t, chain, SPRegistrationOptions{})
	registerHappy(t, g, chain, "t01000", ownerKey) // owner key instead of worker key
}

func TestSPRegisterRejectsForeignSigner(t *testing.T) {
	workerKey := newMinerKey(t)
	ownerKey := newMinerKey(t)
	stranger := newMinerKey(t)
	chain := &fakeChain{
		ownerID: "t0900", workerID: "t0901",
		ownerKey: ownerKey.addr, workerKey: workerKey.addr,
	}
	g := newSPTestGateway(t, chain, SPRegistrationOptions{})

	// Valid signature, but by a key that is neither owner nor worker of the miner.
	r := defaultSPReq("t01000", getChallenge(t, g, "t01000"))
	r.signer = stranger.addr
	r.sig = stranger.sign(t, r.message(testGatewayID))
	if rr := postSPRegister(t, g, r); rr.Code != http.StatusForbidden {
		t.Fatalf("foreign signer: status %d body %s", rr.Code, rr.Body.String())
	}
}

func TestSPRegisterRejectsBadSignature(t *testing.T) {
	workerKey := newMinerKey(t)
	chain := &fakeChain{ownerID: "t0900", workerID: "t0901", ownerKey: workerKey.addr, workerKey: workerKey.addr}
	g := newSPTestGateway(t, chain, SPRegistrationOptions{})

	// Signature over a DIFFERENT payout than the request carries (tamper).
	r := defaultSPReq("t01000", getChallenge(t, g, "t01000"))
	r.signer = workerKey.addr
	tampered := *r
	tampered.payout = "0x0000000000000000000000000000000000000001"
	r.sig = workerKey.sign(t, tampered.message(testGatewayID))
	if rr := postSPRegister(t, g, r); rr.Code != http.StatusUnauthorized {
		t.Fatalf("tampered payout: status %d body %s", rr.Code, rr.Body.String())
	}
}

func TestSPRegisterAdmissionThresholds(t *testing.T) {
	workerKey := newMinerKey(t)
	chain := &fakeChain{
		ownerID: "t0900", workerID: "t0901",
		ownerKey: workerKey.addr, workerKey: workerKey.addr,
		power:   big.NewInt(16 << 30), // 16 GiB — below the 32 GiB floor
		balance: big.NewInt(5e17),     // 0.5 FIL — below the 1 FIL floor
	}

	// Power floor rejects.
	g := newSPTestGateway(t, chain, SPRegistrationOptions{MinRawPowerBytes: big.NewInt(32 << 30)})
	r := defaultSPReq("t01000", getChallenge(t, g, "t01000"))
	r.signer = workerKey.addr
	r.sig = workerKey.sign(t, r.message(testGatewayID))
	if rr := postSPRegister(t, g, r); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "power") {
		t.Fatalf("power floor: status %d body %s", rr.Code, rr.Body.String())
	}

	// Balance floor rejects.
	g2 := newSPTestGateway(t, chain, SPRegistrationOptions{MinMinerBalance: big.NewInt(1e18)})
	r2 := defaultSPReq("t01000", getChallenge(t, g2, "t01000"))
	r2.signer = workerKey.addr
	r2.sig = workerKey.sign(t, r2.message(testGatewayID))
	if rr := postSPRegister(t, g2, r2); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "balance") {
		t.Fatalf("balance floor: status %d body %s", rr.Code, rr.Body.String())
	}

	// Zero thresholds disable both checks.
	g3 := newSPTestGateway(t, chain, SPRegistrationOptions{})
	registerHappy(t, g3, chain, "t01000", workerKey)
}

func TestSPRegisterChallengeLifecycle(t *testing.T) {
	workerKey := newMinerKey(t)
	chain := &fakeChain{ownerID: "t0900", workerID: "t0901", ownerKey: workerKey.addr, workerKey: workerKey.addr}
	g := newSPTestGateway(t, chain, SPRegistrationOptions{})

	// Unknown challenge.
	r := defaultSPReq("t01000", "deadbeef")
	r.signer = workerKey.addr
	r.sig = workerKey.sign(t, r.message(testGatewayID))
	if rr := postSPRegister(t, g, r); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown challenge: status %d", rr.Code)
	}

	// Challenge issued for another miner.
	ch := getChallenge(t, g, "t09999")
	r2 := defaultSPReq("t01000", ch)
	r2.signer = workerKey.addr
	r2.sig = workerKey.sign(t, r2.message(testGatewayID))
	if rr := postSPRegister(t, g, r2); rr.Code != http.StatusBadRequest {
		t.Fatalf("cross-miner challenge: status %d", rr.Code)
	}

	// Expired challenge.
	ch3 := getChallenge(t, g, "t01000")
	g.spReg.mu.Lock()
	g.spReg.challenges[ch3] = spChallenge{miner: "t01000", expires: time.Now().Add(-time.Second)}
	g.spReg.mu.Unlock()
	r3 := defaultSPReq("t01000", ch3)
	r3.signer = workerKey.addr
	r3.sig = workerKey.sign(t, r3.message(testGatewayID))
	if rr := postSPRegister(t, g, r3); rr.Code != http.StatusBadRequest {
		t.Fatalf("expired challenge: status %d", rr.Code)
	}

	// A failed attempt burns the challenge: bad signature first, then a correct
	// retry with the SAME challenge must fail (one-time use).
	ch4 := getChallenge(t, g, "t01000")
	r4 := defaultSPReq("t01000", ch4)
	r4.signer = workerKey.addr
	r4.sig = spSignature{Type: filecoin.SigTypeSecp256k1, Data: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 65))}
	if rr := postSPRegister(t, g, r4); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad sig: status %d", rr.Code)
	}
	r4.sig = workerKey.sign(t, r4.message(testGatewayID))
	if rr := postSPRegister(t, g, r4); rr.Code != http.StatusBadRequest {
		t.Fatalf("burned challenge reuse: status %d", rr.Code)
	}
}

func TestSPRegisterUniquenessAndRotation(t *testing.T) {
	workerKey := newMinerKey(t)
	chain := &fakeChain{ownerID: "t0900", workerID: "t0901", ownerKey: workerKey.addr, workerKey: workerKey.addr}
	g := newSPTestGateway(t, chain, SPRegistrationOptions{})

	first := registerHappy(t, g, chain, "t01000", workerKey)

	// Same miner, DIFFERENT worker_id → 409.
	r := defaultSPReq("t01000", getChallenge(t, g, "t01000"))
	r.workerID = "sp-other-name"
	r.signer = workerKey.addr
	r.sig = workerKey.sign(t, r.message(testGatewayID))
	if rr := postSPRegister(t, g, r); rr.Code != http.StatusConflict {
		t.Fatalf("same miner new worker_id: status %d body %s", rr.Code, rr.Body.String())
	}

	// worker_id already used by ANOTHER miner → 409.
	r2 := defaultSPReq("t01001", getChallenge(t, g, "t01001"))
	r2.workerID = "sp-t01000" // taken
	r2.signer = workerKey.addr
	r2.sig = workerKey.sign(t, r2.message(testGatewayID))
	if rr := postSPRegister(t, g, r2); rr.Code != http.StatusConflict {
		t.Fatalf("stolen worker_id: status %d body %s", rr.Code, rr.Body.String())
	}

	// Same miner + same worker_id: fresh proof of key control → update + token rotation.
	newPayout := "0x976EA74026E726554dB657fA54763abd0C3a0aa9"
	r3 := defaultSPReq("t01000", getChallenge(t, g, "t01000"))
	r3.payout = newPayout
	r3.signer = workerKey.addr
	r3.sig = workerKey.sign(t, r3.message(testGatewayID))
	rr := postSPRegister(t, g, r3)
	if rr.Code != http.StatusOK {
		t.Fatalf("rotation: status %d body %s", rr.Code, rr.Body.String())
	}
	var resp spRegisterResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if !resp.Rotated {
		t.Fatal("re-registration not reported as rotated")
	}
	if resp.AuthToken == first.AuthToken {
		t.Fatal("token was not rotated")
	}
	w, _ := g.registry.Get("sp-t01000")
	if w.AuthToken != resp.AuthToken || w.PayoutAddress != newPayout {
		t.Fatalf("rotation not applied: %+v", w)
	}
}

func TestSPRegisterDoesNotClearBan(t *testing.T) {
	workerKey := newMinerKey(t)
	chain := &fakeChain{ownerID: "t0900", workerID: "t0901", ownerKey: workerKey.addr, workerKey: workerKey.addr}
	g := newSPTestGateway(t, chain, SPRegistrationOptions{})

	registerHappy(t, g, chain, "t01000", workerKey)
	until := time.Now().Add(time.Hour)
	if !g.registry.SetBan("sp-t01000", until, "substandard output") {
		t.Fatal("SetBan failed")
	}

	// Re-registering (token rotation) must NOT lift the punishment.
	r := defaultSPReq("t01000", getChallenge(t, g, "t01000"))
	r.signer = workerKey.addr
	r.sig = workerKey.sign(t, r.message(testGatewayID))
	if rr := postSPRegister(t, g, r); rr.Code != http.StatusOK {
		t.Fatalf("rotation while banned: status %d", rr.Code)
	}
	w, _ := g.registry.Get("sp-t01000")
	if !w.IsBanned() {
		t.Fatal("re-registration cleared the ban")
	}
}

func TestSPRegisterFieldValidation(t *testing.T) {
	workerKey := newMinerKey(t)
	chain := &fakeChain{ownerID: "t0900", workerID: "t0901", ownerKey: workerKey.addr, workerKey: workerKey.addr}
	g := newSPTestGateway(t, chain, SPRegistrationOptions{})

	cases := []struct {
		name   string
		mutate func(*spReq)
	}{
		{"bad miner id", func(r *spReq) { r.miner = "miner-1" }},
		{"bad worker id", func(r *spReq) { r.workerID = "has spaces!" }},
		{"bad payout", func(r *spReq) { r.payout = "not-an-address" }},
		{"bad endpoint", func(r *spReq) { r.endpoint = "ftp://x" }},
		{"bad scheduler url", func(r *spReq) { r.schedURL = "" }},
	}
	for _, tc := range cases {
		r := defaultSPReq("t01000", getChallenge(t, g, "t01000"))
		tc.mutate(r)
		r.signer = workerKey.addr
		r.sig = workerKey.sign(t, r.message(testGatewayID))
		if rr := postSPRegister(t, g, r); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d body %s", tc.name, rr.Code, rr.Body.String())
		}
	}
}

func TestSPRegisterDisabledIs404(t *testing.T) {
	g := &Gateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := httptest.NewRequest(http.MethodPost, "/v1/sp/register", strings.NewReader("{}"))
	rr := httptest.NewRecorder()
	g.handleSPRegister(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled: status %d", rr.Code)
	}
}

func TestSPRegisterRateLimitByIP(t *testing.T) {
	workerKey := newMinerKey(t)
	chain := &fakeChain{ownerID: "t0900", workerID: "t0901", ownerKey: workerKey.addr, workerKey: workerKey.addr}
	g := newSPTestGateway(t, chain, SPRegistrationOptions{RegisterRatePerMin: 2})

	post := func(ip string) int {
		b, _ := json.Marshal(map[string]string{"miner_id": "t01000"})
		req := httptest.NewRequest(http.MethodPost, "/v1/sp/challenge", bytes.NewReader(b))
		req.RemoteAddr = ip + ":9"
		rr := httptest.NewRecorder()
		g.handleSPChallenge(rr, req)
		return rr.Code
	}
	if post("10.0.0.1") != 200 || post("10.0.0.1") != 200 {
		t.Fatal("first two requests should pass")
	}
	if post("10.0.0.1") != http.StatusTooManyRequests {
		t.Fatal("third request should be rate limited")
	}
	if post("10.0.0.2") != 200 {
		t.Fatal("another IP must not be affected")
	}
}

func TestSPRegisterCapacity(t *testing.T) {
	workerKey := newMinerKey(t)
	chain := &fakeChain{ownerID: "t0900", workerID: "t0901", ownerKey: workerKey.addr, workerKey: workerKey.addr}
	g := newSPTestGateway(t, chain, SPRegistrationOptions{MaxRegisteredSPs: 1})

	registerHappy(t, g, chain, "t01000", workerKey)

	r := defaultSPReq("t01001", getChallenge(t, g, "t01001"))
	r.signer = workerKey.addr
	r.sig = workerKey.sign(t, r.message(testGatewayID))
	if rr := postSPRegister(t, g, r); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("over capacity: status %d body %s", rr.Code, rr.Body.String())
	}
}

func TestSPRegisterChainUnavailable(t *testing.T) {
	workerKey := newMinerKey(t)
	chain := &fakeChain{minerInfoErr: fmt.Errorf("all endpoints down")}
	g := newSPTestGateway(t, chain, SPRegistrationOptions{})

	r := defaultSPReq("t01000", getChallenge(t, g, "t01000"))
	r.signer = workerKey.addr
	r.sig = workerKey.sign(t, r.message(testGatewayID))
	if rr := postSPRegister(t, g, r); rr.Code != http.StatusBadGateway {
		t.Fatalf("chain down: status %d body %s", rr.Code, rr.Body.String())
	}
}

func TestParseFILToAtto(t *testing.T) {
	cases := map[string]string{
		"":                     "0",
		"0":                    "0",
		"1":                    "1000000000000000000",
		"10.5":                 "10500000000000000000",
		"0.000000000000000001": "1",
	}
	for in, want := range cases {
		got, err := ParseFILToAtto(in)
		if err != nil {
			t.Fatalf("ParseFILToAtto(%q): %v", in, err)
		}
		if got.String() != want {
			t.Fatalf("ParseFILToAtto(%q) = %s, want %s", in, got, want)
		}
	}
	for _, bad := range []string{"-1", "abc", "1.2.3"} {
		if _, err := ParseFILToAtto(bad); err == nil {
			t.Fatalf("ParseFILToAtto(%q) should fail", bad)
		}
	}
}
