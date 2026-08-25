package gateway

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"sync/atomic"
	"time"
)

// certIssuer turns a successful registration into a worker certificate:
// "admitted by miner signature" and "holds a certificate" become the same fact.
//
// Trust model: the SP proves miner control (challenge signature) and passes
// admission; only then is its CSR signed. The CSR keeps the worker's private
// key on the worker (proof-of-possession is the CSR's own signature); the CN
// must equal the registered worker_id, which the miner signature already binds
// — so one SP cannot obtain a certificate for another's identity on either
// layer. Issued certs are SHORT-LIVED (default 7 days) and renewal is refused
// for banned/deregistered workers, which quietly upgrades a routing ban into
// network-layer removal with no revocation machinery at all.
//
// Key custody (phase 1): the issuing CA key lives on the gateway host with the
// same hot-key treatment as the settlement operator key. The documented
// production shape is a cold root signing a short-lived intermediate for the
// gateway; the code only ever sees "the CA it was given", so that upgrade is
// config-only.
type certIssuer struct {
	caCert   *x509.Certificate
	caKey    any // crypto.Signer (ECDSA or RSA)
	caPEM    []byte
	validity time.Duration
	serial   atomic.Int64
}

// newCertIssuer loads the issuing CA. Empty paths → nil issuer (registration
// keeps working, responses just carry no certificate — plaintext/manual-cert
// workers are untouched).
func newCertIssuer(caCertFile, caKeyFile string, validity time.Duration) (*certIssuer, error) {
	if caCertFile == "" && caKeyFile == "" {
		return nil, nil
	}
	if caCertFile == "" || caKeyFile == "" {
		return nil, fmt.Errorf("cert issuer: issuer_ca_cert and issuer_ca_key must both be set")
	}
	certPEM, err := os.ReadFile(caCertFile)
	if err != nil {
		return nil, fmt.Errorf("cert issuer: read CA cert: %w", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("cert issuer: no PEM block in %s", caCertFile)
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("cert issuer: parse CA cert: %w", err)
	}
	keyPEM, err := os.ReadFile(caKeyFile)
	if err != nil {
		return nil, fmt.Errorf("cert issuer: read CA key: %w", err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, fmt.Errorf("cert issuer: no PEM block in %s", caKeyFile)
	}
	var caKey any
	switch kb.Type {
	case "EC PRIVATE KEY":
		caKey, err = x509.ParseECPrivateKey(kb.Bytes)
	case "RSA PRIVATE KEY":
		caKey, err = x509.ParsePKCS1PrivateKey(kb.Bytes)
	default:
		caKey, err = x509.ParsePKCS8PrivateKey(kb.Bytes)
	}
	if err != nil {
		return nil, fmt.Errorf("cert issuer: parse CA key: %w", err)
	}
	if validity <= 0 {
		validity = 7 * 24 * time.Hour
	}
	iss := &certIssuer{caCert: caCert, caKey: caKey, caPEM: certPEM, validity: validity}
	iss.serial.Store(time.Now().UnixNano())
	return iss, nil
}

// issueFromCSR validates the CSR and signs a server certificate for workerID.
//
// Only the PUBLIC KEY is taken from the CSR. Subject, SANs, extensions and any
// requested usages in the CSR are ignored wholesale — the certificate's
// identity comes from the REGISTRATION (workerID), so a crafted CSR cannot
// smuggle extra names or capabilities past the miner-signature binding. The
// CSR's CN is still required to match, as an explicit statement of intent.
func (ci *certIssuer) issueFromCSR(csrPEM, workerID string) (certPEM string, err error) {
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return "", fmt.Errorf("csr: not a PEM CERTIFICATE REQUEST")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("csr: parse: %w", err)
	}
	// Proof of possession: the CSR is signed by the key it carries.
	if err := csr.CheckSignature(); err != nil {
		return "", fmt.Errorf("csr: signature check failed (key not held?): %w", err)
	}
	if csr.Subject.CommonName != workerID {
		return "", fmt.Errorf("csr: CN %q must equal the registered worker_id %q", csr.Subject.CommonName, workerID)
	}
	switch pub := csr.PublicKey.(type) {
	case *ecdsa.PublicKey:
		_ = pub
	case *rsa.PublicKey:
		if pub.N.BitLen() < 2048 {
			return "", fmt.Errorf("csr: RSA key too small (%d bits)", pub.N.BitLen())
		}
	default:
		return "", fmt.Errorf("csr: unsupported key type %T", csr.PublicKey)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(ci.serial.Add(1)),
		Subject:      pkix.Name{CommonName: workerID},
		DNSNames:     []string{workerID}, // the identity the gateway pins (ServerName)
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(ci.validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ci.caCert, csr.PublicKey, ci.caKey)
	if err != nil {
		return "", fmt.Errorf("csr: sign: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), nil
}

// caCertPEM returns the issuing CA certificate — workers install it so their
// TLS front can verify the gateway's client certificate (same CA).
func (ci *certIssuer) caCertPEM() string { return string(ci.caPEM) }

// serverTLSCert issues a server certificate for the GATEWAY ITSELF, signed by
// the same CA that signs worker certificates.
//
// This closes the last plaintext leg: gateway→worker was already mTLS (the
// worker's TLS front), but worker→gateway (registration, token issuance, the
// admission self-view) ran bare HTTP and survived only because it was tunnelled.
// Exposing it to the public internet needs the gateway to present a certificate
// workers can verify — and they already trust this CA, so no public PKI or
// domain is required.
//
// The SAN is the gateway_id, not an address: workers dial any IP/port the
// operator maps but verify tls.Config.ServerName == gateway_id — the same
// address-decoupling the worker certificates use, so remappings never require
// reissuing. The certificate lives in memory only; every restart mints a fresh
// one (validity is generous because nothing needs to rotate it independently
// of the CA).
func (ci *certIssuer) serverTLSCert(gatewayID string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("gateway server cert: keygen: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(ci.serial.Add(1)),
		Subject:      pkix.Name{CommonName: gatewayID},
		DNSNames:     []string{gatewayID},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ci.caCert, &key.PublicKey, ci.caKey.(crypto.Signer))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("gateway server cert: sign: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
