package admin

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// authMiddleware validates the Bearer token for protected endpoints.
// Returns a new handler that wraps the given handler with auth check.
//
// Unauthenticated endpoints (can be called without token): /health, /metrics.
// All /api/v1/* endpoints require a valid Bearer token.
//
// If adminToken is empty, authentication is DISABLED (for dev/test mode).
// A warning should be logged by the caller in that case.
func authMiddleware(adminToken string, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public endpoints bypass auth
		if isPublicPath(r.URL.Path) {
			handler.ServeHTTP(w, r)
			return
		}

		// Auth disabled (dev mode)
		if adminToken == "" {
			handler.ServeHTTP(w, r)
			return
		}

		token, ok := extractBearerToken(r)
		if !ok {
			writeAuthError(w, "missing or malformed Authorization header (expected 'Bearer <token>')", http.StatusUnauthorized)
			return
		}

		if !tokensEqual(token, adminToken) {
			writeAuthError(w, "invalid token", http.StatusUnauthorized)
			return
		}

		handler.ServeHTTP(w, r)
	})
}

// tokensEqual compares two tokens in constant time without leaking their
// lengths. Both sides are SHA-256 hashed to a fixed 32 bytes first, so the
// subtle.ConstantTimeCompare never short-circuits on a length mismatch.
func tokensEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// isPublicPath reports whether a URL path bypasses authentication.
// /health: simple liveness probe (Docker HEALTHCHECK, k8s liveness).
// /ready:  deeper readiness probe (new-api reachable + workers available).
// /metrics: Prometheus scraper (rely on network-level auth, e.g. firewall).
func isPublicPath(path string) bool {
	return path == "/health" || path == "/ready" || path == "/metrics"
}

// extractBearerToken requires the standard "Bearer <token>" form and returns
// (token, true) on success. A raw token without the scheme is rejected
// (returns "", false) — accepting bare tokens was a minor auth-laxity issue.
func extractBearerToken(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return "", false
	}
	token := strings.TrimSpace(auth[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

func writeAuthError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
