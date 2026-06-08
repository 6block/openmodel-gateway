package admin

import (
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

		token := extractBearerToken(r)
		if token == "" {
			writeAuthError(w, "missing Authorization header", http.StatusUnauthorized)
			return
		}

		// Constant-time compare to avoid timing attacks
		if subtle.ConstantTimeCompare([]byte(token), []byte(adminToken)) != 1 {
			writeAuthError(w, "invalid token", http.StatusUnauthorized)
			return
		}

		handler.ServeHTTP(w, r)
	})
}

// isPublicPath reports whether a URL path bypasses authentication.
// /health: simple liveness probe (Docker HEALTHCHECK, k8s liveness).
// /ready:  deeper readiness probe (new-api reachable + workers available).
// /metrics: Prometheus scraper (rely on network-level auth, e.g. firewall).
func isPublicPath(path string) bool {
	return path == "/health" || path == "/ready" || path == "/metrics"
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	// Accept both "Bearer TOKEN" and plain "TOKEN" formats
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
}

func writeAuthError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
