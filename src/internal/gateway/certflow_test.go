package gateway

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// certFlowGateway: SP registration + a live issuer wired together.
func certFlowGateway(t *testing.T) (*Gateway, *fakeChain, *minerKey) {
	t.Helper()
	workerKey := newMinerKey(t)
	ownerKey := newMinerKey(t)
	chain := &fakeChain{
		ownerID: "t0900", workerID: "t0901",
		ownerKey: ownerKey.addr, workerKey: workerKey.addr,
		power:   big.NewInt(64 << 30),
		balance: big.NewInt(1e18),
	}
	g := newSPTestGateway(t, chain, SPRegistrationOptions{})

	ci, _ := issuerFixture(t, 7*24*time.Hour)
	g.certIssuer = ci
	return g, chain, workerKey
}

// The whole feature in one flow: register WITH a CSR → the response carries a
// verifiable short-lived certificate for exactly this worker, plus the CA cert
// the worker's TLS front needs.
func TestCertAtRegistration(t *testing.T) {
	g, _, key := certFlowGateway(t)

	r := defaultSPReq("t01000", getChallenge(t, g, "t01000"))
	r.signer = key.addr
	r.sig = key.sign(t, r.message(testGatewayID))
	r.csr = makeCSR(t, "sp-t01000")
	rr := postSPRegister(t, g, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rr.Code, rr.Body.String())
	}
	var resp spRegisterResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)

	if resp.WorkerCert == "" || resp.CACert == "" || resp.CertNotAfterUnix == 0 {
		t.Fatalf("registration response missing certificate fields: %+v", resp)
	}
	block, _ := pem.Decode([]byte(resp.WorkerCert))
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if crt.Subject.CommonName != "sp-t01000" {
		t.Fatalf("cert issued for %q, want the registered worker", crt.Subject.CommonName)
	}
	caBlock, _ := pem.Decode([]byte(resp.CACert))
	caCrt, _ := x509.ParseCertificate(caBlock.Bytes)
	pool := x509.NewCertPool()
	pool.AddCert(caCrt)
	if _, err := crt.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("worker cert must chain to the returned CA: %v", err)
	}
}

// A bad CSR must not fail the registration — the worker still gets its token
// (plaintext service works); it just gets no certificate.
func TestCertAtRegistration_BadCSRDoesNotFailRegistration(t *testing.T) {
	g, _, key := certFlowGateway(t)

	r := defaultSPReq("t01000", getChallenge(t, g, "t01000"))
	r.signer = key.addr
	r.sig = key.sign(t, r.message(testGatewayID))
	r.csr = makeCSR(t, "sp-SOMEONE-ELSE") // CN mismatch
	rr := postSPRegister(t, g, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("registration must survive a bad CSR: %d", rr.Code)
	}
	var resp spRegisterResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.AuthToken == "" {
		t.Fatal("token must still be issued")
	}
	if resp.WorkerCert != "" {
		t.Fatal("mismatched CSR must not yield a certificate")
	}
}

func postRenew(t *testing.T, g *Gateway, token, csr string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"csr": csr})
	req := httptest.NewRequest(http.MethodPost, "/v1/sp/renew-cert", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	g.handleSPRenewCert(rr, req)
	return rr
}

// Renewal is the revocation mechanism: valid token renews; a banned worker is
// refused (it ages out of mTLS within one validity window); a bogus token is 401.
func TestRenew_TokenBannedAndBogus(t *testing.T) {
	g, _, key := certFlowGateway(t)

	r := defaultSPReq("t01000", getChallenge(t, g, "t01000"))
	r.signer = key.addr
	r.sig = key.sign(t, r.message(testGatewayID))
	r.csr = makeCSR(t, "sp-t01000")
	var resp spRegisterResponse
	json.Unmarshal(postSPRegister(t, g, r).Body.Bytes(), &resp)

	// happy renew
	rr := postRenew(t, g, resp.AuthToken, makeCSR(t, "sp-t01000"))
	if rr.Code != http.StatusOK {
		t.Fatalf("renew with a valid token must succeed: %d %s", rr.Code, rr.Body.String())
	}
	var renew struct {
		WorkerCert string `json:"worker_cert"`
	}
	json.Unmarshal(rr.Body.Bytes(), &renew)
	if renew.WorkerCert == "" {
		t.Fatal("renew must return a certificate")
	}

	// banned → refused
	g.registry.SetBan("sp-t01000", time.Now().Add(time.Hour), "probe fail")
	if rr := postRenew(t, g, resp.AuthToken, makeCSR(t, "sp-t01000")); rr.Code != http.StatusForbidden {
		t.Fatalf("banned worker renew must be 403, got %d", rr.Code)
	}
	g.registry.SetBan("sp-t01000", time.Time{}, "")

	// bogus token → 401
	if rr := postRenew(t, g, "wk-bogus000000000000", makeCSR(t, "sp-t01000")); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token must be 401, got %d", rr.Code)
	}
	// no issuer → 404
	g2 := newSPTestGateway(t, &fakeChain{}, SPRegistrationOptions{})
	if rr := postRenew(t, g2, "wk-x", "csr"); rr.Code != http.StatusNotFound {
		t.Fatalf("no issuer must be 404, got %d", rr.Code)
	}
}
