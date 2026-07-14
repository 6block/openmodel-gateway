package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"openmodel/sp-state-agent/internal/worker"
)

// TestForwardRequestSendsWorkerToken verifies the gateway authenticates to the worker
// with the worker's per-worker token (so a worker on a public IP can require it and
// reject anyone trying to use its GPU directly, bypassing the gateway).
func TestForwardRequestSendsWorkerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	g := &Gateway{
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	target := &worker.Worker{ID: "w1", Endpoint: srv.URL, AuthToken: "wtok"}

	_, status, _, _, err := g.forwardRequest(context.Background(), "/v1/chat/completions", []byte(`{}`), target, "req-1")
	if err != nil || status != http.StatusOK {
		t.Fatalf("forward failed: status=%d err=%v", status, err)
	}
	if gotAuth != "Bearer wtok" {
		t.Errorf("worker did not receive the per-worker token: got %q", gotAuth)
	}
}

// TestForwardRequestNoTokenOmitsAuth: with no token configured (trusted-LAN mode),
// the gateway must not send an Authorization header to the worker.
func TestForwardRequestNoTokenOmitsAuth(t *testing.T) {
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	g := &Gateway{httpClient: &http.Client{Timeout: 5 * time.Second}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	target := &worker.Worker{ID: "w1", Endpoint: srv.URL} // no AuthToken
	if _, _, _, _, err := g.forwardRequest(context.Background(), "/v1/chat/completions", []byte(`{}`), target, "req-1"); err != nil {
		t.Fatal(err)
	}
	if hadAuth {
		t.Error("gateway sent an Authorization header to the worker despite no token configured")
	}
}
