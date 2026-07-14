package admin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// PublicServer serves the read-only, client-identity-free SP earnings endpoint on a
// SEPARATE port from the admin API (:9091). It exists so a Storage Provider — or
// anyone — can look up per-request earnings WITHOUT being handed the admin token
// (which can register/deregister workers, force settlement, and pause the contract).
//
// Why this is safe to expose publicly:
//   - Read-only: nothing here mutates state or moves money. Money can only be withdrawn
//     by the SP's own key, and that is enforced on-chain, not here.
//   - No client identity: the earnings-detail projection carries request_id, model,
//     tokens, earning and tx — never the paying client's wallet or api-key. SP-earnings
//     transparency must not turn into client-activity transparency.
//   - Rate-limited: SPEarningsDetail scans the whole request log on every call, so a
//     global token bucket caps how fast the backend can be forced to do that work.
//
// TLS is intentionally NOT terminated here. For the M3 invite-only, small-amount trial
// this runs as plain HTTP (the same call we made for the rest of the trial). Before
// exposing it to untrusted networks at scale, front it with a TLS reverse proxy: over
// plain HTTP a network-path attacker can tamper with or impersonate this money-adjacent
// response. The on-chain record is still the backstop — getSPEarnings can't be forged.
type PublicServer struct {
	httpServer *http.Server
	logger     *slog.Logger
}

// NewPublicServer builds the public read-only query server. sa provides the earnings
// handler; ratePerSec/burst bound the global request rate (0 => sane defaults).
func NewPublicServer(port int, ratePerSec float64, burst int, sa *SettlementAPI, logger *slog.Logger) *PublicServer {
	if ratePerSec <= 0 {
		ratePerSec = 20
	}
	if burst <= 0 {
		burst = int(ratePerSec * 2)
		if burst < 1 {
			burst = 1
		}
	}

	mux := http.NewServeMux()
	sa.RegisterPublicRoutes(mux)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"ok"}`)
	})

	limiter := newTokenBucket(ratePerSec, burst)
	return &PublicServer{
		httpServer: &http.Server{
			Addr:              fmt.Sprintf(":%d", port),
			Handler:           rateLimitMiddleware(limiter, mux),
			ReadHeaderTimeout: 10 * time.Second,
		},
		logger: logger,
	}
}

// Start begins serving. Blocks until the server shuts down.
func (s *PublicServer) Start() error {
	s.logger.Info("public query API starting (read-only, no auth)", "addr", s.httpServer.Addr)
	return s.httpServer.ListenAndServe()
}

// Stop gracefully shuts down the server.
func (s *PublicServer) Stop(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.httpServer.Shutdown(ctx)
}

// rateLimitMiddleware rejects with 429 when the global token bucket is exhausted.
// /health always passes so liveness probes are never rate-limited.
func rateLimitMiddleware(b *tokenBucket, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || b.allow() {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintln(w, `{"error":"rate limit exceeded, retry shortly"}`)
	})
}

// tokenBucket is a minimal global token-bucket rate limiter (no external deps). It
// protects the backend as a whole (each call scans the request log), so a single
// global bucket — not per-IP — is the right granularity here.
type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	max    float64
	rate   float64 // tokens refilled per second
	last   time.Time
}

func newTokenBucket(ratePerSec float64, burst int) *tokenBucket {
	return &tokenBucket{
		tokens: float64(burst),
		max:    float64(burst),
		rate:   ratePerSec,
		last:   time.Now(),
	}
}

// allow reports whether one request may proceed, consuming a token if so.
func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * b.rate
	b.last = now
	if b.tokens > b.max {
		b.tokens = b.max
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}
