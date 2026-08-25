package workermtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testPKI generates a CA, a worker server cert (SAN = workerID) and a gateway
// client cert, entirely in-process.
type testPKI struct {
	caPEM, caKey    []byte
	caCert          *x509.Certificate
	caPriv          *ecdsa.PrivateKey
	gatewayCertFile string
	gatewayKeyFile  string
	caFile          string
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	dir := t.TempDir()

	caPriv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caPriv.PublicKey, caPriv)
	caCert, _ := x509.ParseCertificate(caDER)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	p := &testPKI{caPEM: caPEM, caCert: caCert, caPriv: caPriv}
	p.caFile = filepath.Join(dir, "ca.crt")
	os.WriteFile(p.caFile, caPEM, 0600)

	// gateway client cert
	certPEM, keyPEM := p.issue(t, "test-gateway", nil, x509.ExtKeyUsageClientAuth)
	p.gatewayCertFile = filepath.Join(dir, "gw.crt")
	p.gatewayKeyFile = filepath.Join(dir, "gw.key")
	os.WriteFile(p.gatewayCertFile, certPEM, 0600)
	os.WriteFile(p.gatewayKeyFile, keyPEM, 0600)
	return p
}

// issue creates a leaf cert signed by the test CA. dnsNames go into SAN.
func (p *testPKI) issue(t *testing.T, cn string, dnsNames []string, eku x509.ExtKeyUsage) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{eku},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &priv.PublicKey, p.caPriv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(priv)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// workerServer starts an httptest TLS server that REQUIRES a client cert from
// our CA and presents a server cert whose SAN is workerID.
func (p *testPKI) workerServer(t *testing.T, workerID string) *httptest.Server {
	t.Helper()
	certPEM, keyPEM := p.issue(t, workerID, []string{workerID}, x509.ExtKeyUsageServerAuth)
	cert, _ := tls.X509KeyPair(certPEM, keyPEM)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(p.caPEM)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok:"+workerID)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert, // the mTLS in mTLS
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestDisabledFactoryIsPlaintextPassthrough(t *testing.T) {
	f, err := New("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Enabled() {
		t.Fatal("empty config must mean disabled")
	}
	base := &http.Transport{}
	if got := f.TransportFor("w1", base); got != http.RoundTripper(base) {
		t.Fatal("disabled factory must return the base transport unchanged")
	}
	// nil factory too (defensive callers)
	var nilF *Factory
	if nilF.Enabled() {
		t.Fatal("nil factory must read disabled")
	}
}

func TestPartialConfigRejected(t *testing.T) {
	if _, err := New("ca.crt", "", ""); err == nil {
		t.Fatal("partial mTLS config must be an error, not silent plaintext")
	}
}

// The full handshake: gateway client cert accepted, worker identity pinned to
// the worker ID regardless of the dial address (127.0.0.1 here — the tunnel
// scenario).
func TestMutualHandshakeAndIdentityPinning(t *testing.T) {
	pki := newTestPKI(t)
	srv := pki.workerServer(t, "sp-worker-a")

	f, err := New(pki.caFile, pki.gatewayCertFile, pki.gatewayKeyFile)
	if err != nil {
		t.Fatal(err)
	}

	// Correct identity: succeeds even though we dial 127.0.0.1, not "sp-worker-a".
	resp, err := f.ClientFor("sp-worker-a", 5*time.Second, nil).Get(srv.URL)
	if err != nil {
		t.Fatalf("mTLS request should succeed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok:sp-worker-a" {
		t.Fatalf("unexpected body %q", body)
	}

	// Wrong expected identity: the certificate says sp-worker-a, we insist on
	// sp-worker-b → handshake must fail. This is the impersonation check.
	if _, err := f.ClientFor("sp-worker-b", 5*time.Second, nil).Get(srv.URL); err == nil {
		t.Fatal("connecting to worker-a while expecting worker-b must fail")
	}
}

// A client WITHOUT our client certificate must be refused by the worker side —
// holding the URL (or sitting on the path) is no longer enough.
func TestServerRejectsCertlessClient(t *testing.T) {
	pki := newTestPKI(t)
	srv := pki.workerServer(t, "sp-worker-a")

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pki.caPEM)
	bare := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "sp-worker-a"},
	}}
	if _, err := bare.Get(srv.URL); err == nil {
		t.Fatal("a client without the gateway certificate must be rejected")
	}
}

// Same-worker transports are cached and reused (connection pooling stays intact).
func TestTransportCachedPerWorker(t *testing.T) {
	pki := newTestPKI(t)
	f, err := New(pki.caFile, pki.gatewayCertFile, pki.gatewayKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	a1 := f.TransportFor("w1", nil)
	a2 := f.TransportFor("w1", nil)
	b := f.TransportFor("w2", nil)
	if a1 != a2 {
		t.Fatal("same worker must reuse the cached transport")
	}
	if a1 == b {
		t.Fatal("different workers must not share a transport (different pinned identities)")
	}
}
