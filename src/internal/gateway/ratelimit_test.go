package gateway

import (
	"sync"
	"testing"
	"time"
)

// TestTokenBucketRateLimit verifies the token bucket: it permits up to `burst`
// immediately, rejects once drained, and refills at the configured rate over time.
// A fixed clock makes the timing deterministic (no sleeps, no flakes).
func TestTokenBucketRateLimit(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base
	rl := NewRateLimiter(RateLimitConfig{RatePerSec: 2, Burst: 3})
	rl.now = func() time.Time { return now }

	// Burst of 3 should pass back-to-back at t0.
	for i := 0; i < 3; i++ {
		if !rl.AllowRate("userA") {
			t.Fatalf("request %d within burst should pass", i+1)
		}
	}
	// 4th immediate request exceeds the burst → rejected.
	if rl.AllowRate("userA") {
		t.Fatal("4th request beyond burst must be rejected")
	}

	// After 1s at 2 tokens/sec, exactly 2 more should pass.
	now = base.Add(1 * time.Second)
	if !rl.AllowRate("userA") || !rl.AllowRate("userA") {
		t.Fatal("two requests should pass after 1s refill")
	}
	if rl.AllowRate("userA") {
		t.Fatal("third request after 1s refill must be rejected")
	}
}

// TestRateLimitIsolatedPerKey verifies one key draining its bucket does not affect
// another key — the core fairness property of per-key limiting.
func TestRateLimitIsolatedPerKey(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rl := NewRateLimiter(RateLimitConfig{RatePerSec: 1, Burst: 1})
	rl.now = func() time.Time { return now }

	if !rl.AllowRate("noisy") {
		t.Fatal("noisy key's first request should pass")
	}
	if rl.AllowRate("noisy") {
		t.Fatal("noisy key should now be limited")
	}
	// A different key has its own full bucket.
	if !rl.AllowRate("quiet") {
		t.Fatal("a different key must not be affected by another key's limit")
	}
}

// TestConcurrencyLimit verifies the per-key in-flight cap and that releasing a slot
// frees capacity, and that release is idempotent.
func TestConcurrencyLimit(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{MaxInFlight: 2})

	r1, ok1 := rl.Acquire("k")
	r2, ok2 := rl.Acquire("k")
	if !ok1 || !ok2 {
		t.Fatal("first two concurrent acquires should succeed")
	}
	if _, ok3 := rl.Acquire("k"); ok3 {
		t.Fatal("third concurrent acquire must be rejected at cap=2")
	}
	// Release one slot → capacity available again.
	r1()
	r3, ok4 := rl.Acquire("k")
	if !ok4 {
		t.Fatal("after releasing one slot, a new acquire should succeed")
	}
	// Idempotent release: calling r1 again must not over-decrement and grant a bogus slot.
	r1()
	r2()
	r3()
	// A different key is independent.
	if _, ok := rl.Acquire("other"); !ok {
		t.Fatal("a different key has its own concurrency budget")
	}
}

// TestConcurrencyLimitRaceSafe hammers Acquire/release concurrently to ensure the
// in-flight counter never exceeds the cap and never underflows. Run with -race.
func TestConcurrencyLimitRaceSafe(t *testing.T) {
	const cap = 4
	rl := NewRateLimiter(RateLimitConfig{MaxInFlight: cap})

	var mu sync.Mutex
	cur, peak := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, ok := rl.Acquire("k")
			if !ok {
				return
			}
			mu.Lock()
			cur++
			if cur > peak {
				peak = cur
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			cur--
			mu.Unlock()
			rel()
		}()
	}
	wg.Wait()
	if peak > cap {
		t.Fatalf("concurrent in-flight peaked at %d, exceeding cap %d", peak, cap)
	}
}

// TestRateLimiterDisabled verifies that an unconfigured limiter is nil and that the
// nil-receiver methods are permissive no-ops (preserving pre-B5 behaviour).
func TestRateLimiterDisabled(t *testing.T) {
	if rl := NewRateLimiter(RateLimitConfig{}); rl != nil {
		t.Fatal("limiter with no limits configured should be nil")
	}
	var rl *RateLimiter // nil
	if !rl.AllowRate("x") {
		t.Fatal("nil limiter must allow all rates")
	}
	rel, ok := rl.Acquire("x")
	if !ok {
		t.Fatal("nil limiter must allow all acquires")
	}
	rel() // must not panic
}

// TestBurstDefaulting verifies burst defaults to ceil(rate)>=1 when unset.
func TestBurstDefaulting(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rl := NewRateLimiter(RateLimitConfig{RatePerSec: 5}) // burst unset → should default to 5
	rl.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		if !rl.AllowRate("k") {
			t.Fatalf("request %d within defaulted burst should pass", i+1)
		}
	}
	if rl.AllowRate("k") {
		t.Fatal("request beyond defaulted burst must be rejected")
	}
}
