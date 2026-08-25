package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// issuerFixture writes a self-signed CA to disk and returns a live issuer.
func issuerFixture(t *testing.T, validity time.Duration) (*certIssuer, *x509.Certificate) {
	t.Helper()
	dir := t.TempDir()
	caPriv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "issuer-test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caPriv.PublicKey, caPriv)
	caCert, _ := x509.ParseCertificate(der)
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
	kb, _ := x509.MarshalECPrivateKey(caPriv)
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0600)

	ci, err := newCertIssuer(certPath, keyPath, validity)
	if err != nil {
		t.Fatal(err)
	}
	return ci, caCert
}

func makeCSR(t *testing.T, cn string) string {
	t.Helper()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cn},
	}, priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func TestIssuer_DisabledWhenUnconfigured(t *testing.T) {
	ci, err := newCertIssuer("", "", 0)
	if err != nil || ci != nil {
		t.Fatalf("empty config must mean nil issuer, got %v/%v", ci, err)
	}
	if _, err := newCertIssuer("ca.crt", "", 0); err == nil {
		t.Fatal("half-configured issuer must be an error")
	}
}

func TestIssuer_SignsValidCSR(t *testing.T) {
	ci, caCert := issuerFixture(t, 7*24*time.Hour)
	certPEM, err := ci.issueFromCSR(makeCSR(t, "sp-worker-a"), "sp-worker-a")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(certPEM))
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if crt.Subject.CommonName != "sp-worker-a" || len(crt.DNSNames) != 1 || crt.DNSNames[0] != "sp-worker-a" {
		t.Fatalf("identity fields wrong: CN=%q SAN=%v", crt.Subject.CommonName, crt.DNSNames)
	}
	// Chain verifies against the CA, and the validity is short-lived as configured.
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := crt.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("issued cert must verify against the CA: %v", err)
	}
	if d := time.Until(crt.NotAfter); d > 7*24*time.Hour+time.Hour {
		t.Fatalf("validity too long: %v", d)
	}
}

// One SP must not be able to obtain a certificate for another's identity: the
// CSR's CN has to equal the REGISTERED worker_id.
func TestIssuer_RejectsMismatchedIdentity(t *testing.T) {
	ci, _ := issuerFixture(t, 0)
	if _, err := ci.issueFromCSR(makeCSR(t, "sp-worker-b"), "sp-worker-a"); err == nil {
		t.Fatal("CN != registered worker_id must be refused")
	}
}

// A CSR that smuggles SANs must not see them in the issued certificate — the
// identity comes from the registration, never from the request.
func TestIssuer_IgnoresSmuggledSANs(t *testing.T) {
	ci, _ := issuerFixture(t, 0)
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: "sp-worker-a"},
		DNSNames: []string{"sp-worker-a", "api.openmodel.example", "sp-worker-b"},
	}, priv)
	csr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))

	certPEM, err := ci.issueFromCSR(csr, "sp-worker-a")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(certPEM))
	crt, _ := x509.ParseCertificate(block.Bytes)
	if len(crt.DNSNames) != 1 || crt.DNSNames[0] != "sp-worker-a" {
		t.Fatalf("smuggled SANs leaked into the certificate: %v", crt.DNSNames)
	}
}

func TestIssuer_RejectsGarbageCSR(t *testing.T) {
	ci, _ := issuerFixture(t, 0)
	for _, bad := range []string{"", "not pem", "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----"} {
		if _, err := ci.issueFromCSR(bad, "sp-worker-a"); err == nil {
			t.Fatalf("garbage CSR %q must be refused", bad)
		}
	}
}

func TestIssuer_CACertExposedForWorkers(t *testing.T) {
	ci, _ := issuerFixture(t, 0)
	if !strings.Contains(ci.caCertPEM(), "BEGIN CERTIFICATE") {
		t.Fatal("caCertPEM must return the CA certificate PEM")
	}
}
