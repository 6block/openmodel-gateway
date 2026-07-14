package gateway

import (
	"sync"
	"time"
)

// ratelimit.go implements per-API-key abuse controls for the gateway (B5):
//   - a token-bucket request-rate limit (requests/second with a burst), and
//   - a concurrent in-flight cap.
//
// Both are keyed by API-key name so one noisy client cannot starve others, and
// both are self-contained (no external dependency) so the build stays offline-safe.
// A zero limit means "unlimited" for that dimension, preserving today's behaviour
// when the operator has not configured limits.

// tokenBucket is a classic token-bucket rate limiter: it refills at `ratePerSec`
// tokens/second up to `burst`, and `allow` consumes one token if available.
type tokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	burst      float64
	ratePerSec float64
	last       time.Time
}

func newTokenBucket(ratePerSec float64, burst int, now time.Time) *tokenBucket {
	b := float64(burst)
	if b < 1 {
		b = 1
	}
	return &tokenBucket{tokens: b, burst: b, ratePerSec: ratePerSec, last: now}
}

// allowAt reports whether a request is permitted at time `now`, consuming a token
// if so. Taking `now` as a parameter keeps it deterministically testable.
func (b *tokenBucket) allowAt(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.ratePerSec
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// keyLimiter holds the per-key rate bucket and concurrency counter.
type keyLimiter struct {
	bucket  *tokenBucket
	inFlight int
}

// RateLimiter enforces per-key request-rate and concurrency limits. It is safe for
// concurrent use. Limits of 0 disable that dimension entirely.
type RateLimiter struct {
	mu          sync.Mutex
	keys        map[string]*keyLimiter
	ratePerSec  float64
	burst       int
	maxInFlight int
	now         func() time.Time // injectable clock for tests
}

// RateLimitConfig configures the limiter. Zero values disable a dimension.
type RateLimitConfig struct {
	RatePerSec  float64 // sustained requests/sec per key; 0 = unlimited
	Burst       int     // bucket size (max instantaneous burst); defaults to ceil(RatePerSec) or 1
	MaxInFlight int     // max concurrent requests per key; 0 = unlimited
}

// NewRateLimiter builds a limiter from config. Returns nil if no limit is set, so
// callers can cheaply skip all limiter work when the operator configured nothing.
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	if cfg.RatePerSec <= 0 && cfg.MaxInFlight <= 0 {
		return nil
	}
	burst := cfg.Burst
	if burst <= 0 {
		burst = int(cfg.RatePerSec)
		if burst < 1 {
			burst = 1
		}
	}
	return &RateLimiter{
		keys:        make(map[string]*keyLimiter),
		ratePerSec:  cfg.RatePerSec,
		burst:       burst,
		maxInFlight: cfg.MaxInFlight,
		now:         time.Now,
	}
}

func (rl *RateLimiter) limiterFor(key string) *keyLimiter {
	kl, ok := rl.keys[key]
	if !ok {
		kl = &keyLimiter{}
		if rl.ratePerSec > 0 {
			kl.bucket = newTokenBucket(rl.ratePerSec, rl.burst, rl.now())
		}
		rl.keys[key] = kl
	}
	return kl
}

// AllowRate reports whether a new request for `key` is within the rate limit,
// consuming a token if so. Always true when the limiter is nil or rate is disabled.
func (rl *RateLimiter) AllowRate(key string) bool {
	if rl == nil || rl.ratePerSec <= 0 {
		return true
	}
	rl.mu.Lock()
	kl := rl.limiterFor(key)
	bucket := kl.bucket
	rl.mu.Unlock()
	return bucket.allowAt(rl.now())
}

// Acquire tries to reserve a concurrency slot for `key`. It returns ok=false if the
// per-key in-flight cap is already reached; otherwise it increments the counter and
// returns a release func the caller MUST defer to free the slot. When concurrency is
// disabled it returns a no-op release.
func (rl *RateLimiter) Acquire(key string) (release func(), ok bool) {
	if rl == nil || rl.maxInFlight <= 0 {
		return func() {}, true
	}
	rl.mu.Lock()
	kl := rl.limiterFor(key)
	if kl.inFlight >= rl.maxInFlight {
		rl.mu.Unlock()
		return func() {}, false
	}
	kl.inFlight++
	rl.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			rl.mu.Lock()
			if kl.inFlight > 0 {
				kl.inFlight--
			}
			rl.mu.Unlock()
		})
	}, true
}
