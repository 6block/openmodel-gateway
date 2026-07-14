package gateway

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/worker"
)

// drainGateway builds a gateway with one key and no workers (so a passed-through
// request fast-404s on an unsupported model rather than blocking).
func drainGateway(t *testing.T) *Gateway {
	t.Helper()
	return New(worker.NewRegistry(discardLog(), ""),
		config.GatewayConfig{APIKeys: []config.APIKey{{Key: "secret", Name: "k1"}}},
		discardLog())
}

// TestDrainRejectsNewRequests verifies B7: once draining, new proxy requests get a
// clean 503 + Retry-After instead of being routed/queued or having the connection
// dropped.
func TestDrainRejectsNewRequests(t *testing.T) {
	gw := drainGateway(t)

	// Before drain: not a 503-from-drain (no worker → 404, which is fine; just not the
	// drain rejection).
	rec := httptest.NewRecorder()
	gw.handleProxy(rec, proxyReq(unsupportedModelBody))
	if rec.Code == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") == "5" {
		t.Fatal("should not be drain-rejected before BeginDrain")
	}

	// Begin drain (nothing in flight → returns immediately).
	if remaining := gw.BeginDrain(time.Second); remaining != 0 {
		t.Fatalf("clean drain should return 0 in-flight, got %d", remaining)
	}
	if !gw.IsDraining() {
		t.Fatal("gateway should report draining after BeginDrain")
	}

	// After drain: every new request is rejected with 503 + Retry-After.
	rec2 := httptest.NewRecorder()
	gw.handleProxy(rec2, proxyReq(unsupportedModelBody))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("draining gateway must reject new requests with 503, got %d", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Error("drain 503 should carry a Retry-After header")
	}
}

// TestDrainWaitsForInFlight verifies BeginDrain blocks until an in-flight request
// finishes, and that the in-flight counter is accurate. We simulate an in-flight
// request by bumping the counter directly (a real request holds it via defer).
func TestDrainWaitsForInFlight(t *testing.T) {
	gw := drainGateway(t)

	// Simulate one in-flight request.
	gw.inFlight.Add(1)
	if gw.InFlight() != 1 {
		t.Fatalf("in-flight should be 1, got %d", gw.InFlight())
	}

	// Release it after 150ms from another goroutine.
	go func() {
		time.Sleep(150 * time.Millisecond)
		gw.inFlight.Add(-1)
	}()

	start := time.Now()
	remaining := gw.BeginDrain(2 * time.Second)
	elapsed := time.Since(start)

	if remaining != 0 {
		t.Errorf("drain should complete cleanly once in-flight releases, got %d remaining", remaining)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("drain returned too early (%v); it should have waited for the in-flight request", elapsed)
	}
}

// TestDrainTimesOut verifies BeginDrain returns the still-in-flight count when the
// timeout elapses before requests finish (so shutdown can proceed, logging the leak).
func TestDrainTimesOut(t *testing.T) {
	gw := drainGateway(t)
	gw.inFlight.Add(2) // never released

	remaining := gw.BeginDrain(200 * time.Millisecond)
	if remaining != 2 {
		t.Fatalf("drain timeout should report 2 in-flight, got %d", remaining)
	}
}

// TestDrainConcurrentRequestsBlocked verifies that under concurrency, every request
// arriving after BeginDrain is rejected, and none slip through to routing.
func TestDrainConcurrentRequestsBlocked(t *testing.T) {
	gw := drainGateway(t)
	gw.BeginDrain(time.Second)

	var wg sync.WaitGroup
	var nonDrainCodes sync.Map
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			gw.handleProxy(rec, proxyReq(unsupportedModelBody))
			if rec.Code != http.StatusServiceUnavailable {
				nonDrainCodes.Store(i, rec.Code)
			}
		}(i)
	}
	wg.Wait()

	nonDrainCodes.Range(func(k, v any) bool {
		t.Errorf("request %v slipped through drain with code %v", k, v)
		return true
	})
}
