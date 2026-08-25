package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func reqFrom(remote string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func withProxies(t *testing.T, entries []string) {
	t.Helper()
	old := trustedProxies
	if err := SetTrustedProxies(entries); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { trustedProxies = old })
}

// A direct client's forwarding headers are attacker-controlled input: ignored.
func TestClientIP_DirectPeerHeadersIgnored(t *testing.T) {
	withProxies(t, []string{"47.108.207.38"})
	got := clientIP(reqFrom("6.7.8.9:1234", map[string]string{"X-Forwarded-For": "1.2.3.4"}))
	if got != "6.7.8.9" {
		t.Fatalf("spoofed XFF from a direct peer was believed: %s", got)
	}
}

// From the trusted proxy, the rightmost non-proxy XFF entry is the client —
// entries left of it are client-supplied and must not win.
func TestClientIP_TrustedProxyRightmostWins(t *testing.T) {
	withProxies(t, []string{"47.108.207.38"})
	got := clientIP(reqFrom("47.108.207.38:5555", map[string]string{
		"X-Forwarded-For": "9.9.9.9, 5.6.7.8"})) // 9.9.9.9 = forged by client, 5.6.7.8 = appended by nginx
	if got != "5.6.7.8" {
		t.Fatalf("want rightmost untrusted entry 5.6.7.8, got %s", got)
	}
}

// Chained trusted hops are skipped right-to-left.
func TestClientIP_SkipsTrustedChain(t *testing.T) {
	withProxies(t, []string{"47.108.207.38", "10.0.0.0/8"})
	got := clientIP(reqFrom("47.108.207.38:5555", map[string]string{
		"X-Forwarded-For": "5.6.7.8, 10.1.2.3"}))
	if got != "5.6.7.8" {
		t.Fatalf("want 5.6.7.8 past the trusted 10.x hop, got %s", got)
	}
}

// Trusted proxy without XFF falls back to X-Real-IP, then to the peer itself.
func TestClientIP_TrustedProxyRealIPFallback(t *testing.T) {
	withProxies(t, []string{"47.108.207.38"})
	if got := clientIP(reqFrom("47.108.207.38:5555", map[string]string{"X-Real-IP": "5.6.7.8"})); got != "5.6.7.8" {
		t.Fatalf("X-Real-IP fallback: got %s", got)
	}
	if got := clientIP(reqFrom("47.108.207.38:5555", nil)); got != "47.108.207.38" {
		t.Fatalf("bare trusted peer: got %s", got)
	}
}

// No proxies configured (the default): behavior identical to before.
func TestClientIP_NoProxiesConfigured(t *testing.T) {
	withProxies(t, nil)
	got := clientIP(reqFrom("47.108.207.38:5555", map[string]string{"X-Forwarded-For": "1.2.3.4"}))
	if got != "47.108.207.38" {
		t.Fatalf("with no allowlist the peer itself must be the client: %s", got)
	}
}

func TestSetTrustedProxies_RejectsGarbage(t *testing.T) {
	if err := SetTrustedProxies([]string{"not-an-ip"}); err == nil {
		t.Fatal("garbage entry accepted")
	}
}
