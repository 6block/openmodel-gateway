package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func testHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
}

func TestAuthMiddleware_PublicPaths(t *testing.T) {
	// /health and /metrics should always pass, even without token
	handler := authMiddleware("required-token", testHandler())

	for _, path := range []string{"/health", "/ready", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", path, rec.Code)
		}
	}
}

func TestAuthMiddleware_MissingToken(t *testing.T) {
	handler := authMiddleware("required-token", testHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	handler := authMiddleware("required-token", testHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddleware_ValidTokenBearer(t *testing.T) {
	handler := authMiddleware("secret-token", testHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestAuthMiddleware_ValidTokenPlain(t *testing.T) {
	handler := authMiddleware("secret-token", testHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	req.Header.Set("Authorization", "secret-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 (plain token accepted), got %d", rec.Code)
	}
}

func TestAuthMiddleware_EmptyTokenDisablesAuth(t *testing.T) {
	// When adminToken is empty, auth is disabled (dev mode)
	handler := authMiddleware("", testHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 (auth disabled), got %d", rec.Code)
	}
}
