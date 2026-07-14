package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/worker"
)

// newLimitGateway builds a gateway with the given abuse-control config and one
// API key "k1". No worker is registered, so requests that pass the abuse gate fall
// through to routing — the tests only assert the abuse-gate status codes, which are
// returned before routing.
func newLimitGateway(t *testing.T, cfg config.GatewayConfig) *Gateway {
	t.Helper()
	cfg.APIKeys = []config.APIKey{{Key: "secret", Name: "k1"}}
	return New(worker.NewRegistry(discardLog(), ""), cfg, discardLog())
}

func proxyReq(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Content-Type", "application/json")
	return r
}

// unsupportedModelBody routes to a model no worker supports, so handleProxy
// fast-rejects with 404 BEFORE entering the worker queue. This lets the abuse-gate
// tests assert "not 413 / not 429" without the request blocking on blockForWorker
// (which would wait the full request timeout since the tests register no worker).
const unsupportedModelBody = `{"model":"no-such-model-xyz"}`

// TestHandleProxyBodyTooLarge verifies B5: a body over max_request_bytes is
// rejected with 413 (not silently truncated, which would corrupt JSON + zero
// billing). Uses httptest.NewRecorder so it needs no network socket.
func TestHandleProxyBodyTooLarge(t *testing.T) {
	gw := newLimitGateway(t, config.GatewayConfig{MaxRequestBytes: 64})
	big := `{"model":"default","prompt":"` + strings.Repeat("x", 200) + `"}`

	rec := httptest.NewRecorder()
	gw.handleProxy(rec, proxyReq(big))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: want 413, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleProxyBodyWithinLimit verifies a body within the cap is NOT rejected by
// the size gate (it proceeds past the gate; with no worker it then 404/503s, which
// is fine — we only assert it is not a 413).
func TestHandleProxyBodyWithinLimit(t *testing.T) {
	gw := newLimitGateway(t, config.GatewayConfig{MaxRequestBytes: 4096})
	rec := httptest.NewRecorder()
	gw.handleProxy(rec, proxyReq(unsupportedModelBody))

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("within-limit body must not be rejected as too large; got 413")
	}
}

// TestHandleProxyRateLimited verifies that once a key's bucket is drained, the
// gateway returns 429 with a Retry-After header.
func TestHandleProxyRateLimited(t *testing.T) {
	gw := newLimitGateway(t, config.GatewayConfig{RatePerSecPerKey: 1, RateBurstPerKey: 1})

	// First request consumes the only token (it won't 429 — likely 404/503 with no worker).
	rec1 := httptest.NewRecorder()
	gw.handleProxy(rec1, proxyReq(unsupportedModelBody))
	if rec1.Code == http.StatusTooManyRequests {
		t.Fatal("first request within burst must not be rate-limited")
	}

	// Second immediate request from the same key is over the limit → 429.
	rec2 := httptest.NewRecorder()
	gw.handleProxy(rec2, proxyReq(unsupportedModelBody))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request should be rate-limited: want 429, got %d", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Error("429 response should carry a Retry-After header")
	}
}

// TestHandleProxyNoLimitsConfigured verifies that with no abuse config, repeated
// requests are never rejected with 429/413 (preserves pre-B5 behaviour).
func TestHandleProxyNoLimitsConfigured(t *testing.T) {
	gw := newLimitGateway(t, config.GatewayConfig{})
	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		gw.handleProxy(rec, proxyReq(unsupportedModelBody))
		if rec.Code == http.StatusTooManyRequests || rec.Code == http.StatusRequestEntityTooLarge {
			t.Fatalf("request %d unexpectedly rejected by abuse gate (%d) when no limits set", i+1, rec.Code)
		}
	}
}
