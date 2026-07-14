package admin

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTokenBucket(t *testing.T) {
	b := newTokenBucket(10, 3) // 10 tokens/sec, burst 3

	// A burst of 3 should pass immediately.
	for i := 0; i < 3; i++ {
		if !b.allow() {
			t.Fatalf("burst token %d should be allowed", i)
		}
	}
	// The 4th, with effectively no time elapsed, is over burst → blocked.
	if b.allow() {
		t.Fatal("token past burst should be blocked")
	}
	// After ~1.5 tokens' worth of refill, one call succeeds again.
	time.Sleep(150 * time.Millisecond)
	if !b.allow() {
		t.Fatal("token should be allowed after refill")
	}
}

func TestPublicServer_NoAuth(t *testing.T) {
	// A SettlementAPI with a nil settler makes the earnings handler return 503 (engine
	// not running) — crucially WITHOUT checking any token. A 503 (not 401) proves the
	// public port has no auth gate: the request reached the handler unauthenticated.
	sa := &SettlementAPI{logger: slog.Default()}
	ps := NewPublicServer(0, 1000, 2, sa, slog.Default())
	srv := httptest.NewServer(ps.httpServer.Handler)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/sp-earnings-detail/0xabc", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("public port must NOT require auth, got 401")
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (nil settler reached handler unauthenticated), got %d", resp.StatusCode)
	}

	// /health is always 200.
	hr, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("health failed: %v", err)
	}
	hr.Body.Close()
	if hr.StatusCode != http.StatusOK {
		t.Fatalf("health should be 200, got %d", hr.StatusCode)
	}
}

func TestRateLimitMiddleware_429AndHealthBypass(t *testing.T) {
	// burst 2, negligible refill → the 3rd rapid call is 429.
	b := newTokenBucket(0.001, 2)
	h := rateLimitMiddleware(b, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	codes := make([]int, 3)
	for i := range codes {
		resp, err := http.Get(srv.URL + "/api/v1/sp-earnings-detail/0xabc")
		if err != nil {
			t.Fatalf("call %d failed: %v", i, err)
		}
		codes[i] = resp.StatusCode
		resp.Body.Close()
	}
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("first two calls should pass (burst 2), got %v", codes)
	}
	if codes[2] != http.StatusTooManyRequests {
		t.Fatalf("third call should be 429, got %d", codes[2])
	}

	// /health bypasses the limiter even when the bucket is exhausted.
	hr, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("health failed: %v", err)
	}
	code := hr.StatusCode
	hr.Body.Close()
	if code != http.StatusOK {
		t.Fatalf("/health must bypass rate limiting, got %d", code)
	}
}
