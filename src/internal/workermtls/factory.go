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
	cache map[string]*http.Transport // worker ID → armed transport
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
		f.cache = make(map[string]*http.Transport)
	}
	f.cache[workerID] = t
	return t
}

// ClientFor wraps TransportFor in an *http.Client with the given timeout.
func (f *Factory) ClientFor(workerID string, timeout time.Duration, base *http.Transport) *http.Client {
	return &http.Client{Timeout: timeout, Transport: f.TransportFor(workerID, base)}
}
