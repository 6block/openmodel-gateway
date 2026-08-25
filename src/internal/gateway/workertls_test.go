package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The worker→gateway direction (registration, token issuance, self-view) ran
// bare HTTP and survived only inside tunnels. These tests pin the TLS shape
// that lets it face the public internet: the gateway presents a CA-signed
// certificate whose identity is the GATEWAY ID — never the dialed address — so
// operators can remap public ports without reissuing anything.
func TestWorkerTLS_GatewayIdentityIsTheID(t *testing.T) {
	ci, _ := issuerFixture(t, time.Hour) // reuses the certissuer test CA fixture
	cert, err := ci.serverTLSCert("worker-under-test")
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello worker")
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(ci.caCertPEM())) {
		t.Fatal("CA PEM did not load")
	}

	// Correct ServerName (the gateway id) verifies — regardless of the dialed
	// address (here: 127.0.0.1:random).
	ok := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, ServerName: "worker-under-test"}}}
	resp, err := ok.Get(srv.URL)
	if err != nil {
		t.Fatalf("CA-trusting client with the right ServerName must connect: %v", err)
	}
	resp.Body.Close()

	// Wrong expected identity must fail even with the right CA.
	bad := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, ServerName: "some-other-gateway"}}}
	if _, err := bad.Get(srv.URL); err == nil {
		t.Fatal("a client expecting a different gateway id must refuse the handshake")
	}

	// No CA → fail closed: the public internet cannot MITM its way in.
	stranger := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: x509.NewCertPool(), ServerName: "worker-under-test"}}}
	if _, err := stranger.Get(srv.URL); err == nil {
		t.Fatal("a client without the registration CA must refuse the certificate")
	}
}
