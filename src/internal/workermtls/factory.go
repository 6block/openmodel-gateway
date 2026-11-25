// Package workermtls hands out the HTTP clients the gateway uses to talk TO
// workers (polling, inference forwarding, probing), optionally armed with
// mutual TLS.
//
// Identity model: every worker's server certificate carries its WORKER ID as
// the SAN (issued by our private CA at registration or by the operator), and
// the gateway verifies exactly that — tls.Config.ServerName is set to the
// worker ID, never to a hostname. The network address is irrelevant to
// authentication, so SSH tunnels, jump-host port maps and NAT stay fully
// transparent; conversely the gateway presents its own client certificate,
// which the worker-side terminator requires. A transit node can no longer read
// tokens or impersonate either side — it forwards ciphertext or nothing.
//
// Migration model: material loaded ≠ mTLS forced. The TLS config only engages
// for https:// worker endpoints; http:// endpoints keep using the plain shared
// transport. A fleet can therefore migrate worker by worker (the mainnet
// gateway keeps plaintext workers while the test gateway's workers move to
// https), and rollback is "flip the endpoint back to http".
package workermtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// Factory builds and caches one *http.Transport per worker ID. A nil *Factory
// (or one built with no material) is valid and means "plaintext only" — every
// method degrades to the caller's base transport, preserving pre-mTLS behavior.
type Factory struct {
	base *tls.Config // nil = no material loaded

	mu    sync.Mutex
	cache map[string]*expiryCheckingTransport // worker ID → armed transport

	now func() time.Time // injectable for tests; nil = time.Now
}

// expiryCheckingTransport refuses to keep using a connection whose peer
// certificate has expired. TLS checks validity only at the handshake, and the
// gateway polls every worker every few seconds, so a pooled connection never
// idles out: without this check a worker whose renewal was refused kept serving
// on its expired certificate indefinitely (seen on the test gateway — the cert
// expired at 03:41:42 and the worker still polled "idle" afterwards), and expiry
// is the only revocation mechanism there is.
type expiryCheckingTransport struct {
	*http.Transport
	now func() time.Time
}

func (t *expiryCheckingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.Transport.RoundTrip(req)
	if err != nil || resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		return resp, err
	}
	if leaf := resp.TLS.PeerCertificates[0]; t.now().After(leaf.NotAfter) {
		resp.Body.Close()
		// Drop the pooled connections so the next attempt re-handshakes — and
		// fails the handshake, as an expired certificate should.
		t.Transport.CloseIdleConnections()
		return nil, fmt.Errorf("worker certificate for %q expired at %s",
			leaf.Subject.CommonName, leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	return resp, nil
}

// New loads the CA bundle plus the gateway's client certificate. All three
// paths empty → disabled factory (still usable, plaintext passthrough).
// Anything half-configured is an error: silently proceeding without client
// auth is exactly the failure mTLS exists to prevent.
func New(caFile, certFile, keyFile string) (*Factory, error) {
	if caFile == "" && certFile == "" && keyFile == "" {
		return &Factory{}, nil
	}
	if caFile == "" || certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("worker mTLS: ca_file, cert_file and key_file must all be set (got partial config)")
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("worker mTLS: read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("worker mTLS: no certificates parsed from %s", caFile)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("worker mTLS: load client keypair: %w", err)
	}
	return &Factory{
		base: &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}, nil
}

// Enabled reports whether TLS material is loaded.
func (f *Factory) Enabled() bool { return f != nil && f.base != nil }

// TransportFor returns a transport whose TLS handshake pins the peer identity
// to workerID (certificate SAN check against ServerName). base carries the
// caller's pooling/timeout tuning and is cloned, never mutated. Plaintext
// (disabled factory) returns base unchanged.
func (f *Factory) TransportFor(workerID string, base *http.Transport) http.RoundTripper {
	if !f.Enabled() {
		if base != nil {
			return base
		}
		return http.DefaultTransport
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.cache[workerID]; ok {
		return t
	}
	var t *http.Transport
	if base != nil {
		t = base.Clone()
	} else {
		t = http.DefaultTransport.(*http.Transport).Clone()
	}
	cfg := f.base.Clone()
	// The whole point: verify "this is worker <ID>", not "this is host X".
	cfg.ServerName = workerID
	t.TLSClientConfig = cfg
	if f.cache == nil {
		f.cache = make(map[string]*expiryCheckingTransport)
	}
	now := f.now
	if now == nil {
		now = time.Now
	}
	ct := &expiryCheckingTransport{Transport: t, now: now}
	f.cache[workerID] = ct
	return ct
}

// ClientFor wraps TransportFor in an *http.Client with the given timeout.
func (f *Factory) ClientFor(workerID string, timeout time.Duration, base *http.Transport) *http.Client {
	return &http.Client{Timeout: timeout, Transport: f.TransportFor(workerID, base)}
}
