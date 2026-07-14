package gateway

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/worker"
)

// B1 predictive routing tests: workers about to yield to mining are de-prioritized,
// and 503 Retry-After reflects the scheduler's real resume estimate.

func TestPredictive_YieldingSoonPenalizesWeight(t *testing.T) {
	now := time.Now()
	base := &worker.Worker{GPUCount: 4, State: worker.StateIdle}

	soon := *base
	soon.SecondsUntilChange = 10 // graceful yield begins in 10s
	soon.UntilChangeAt = now

	far := *base
	far.SecondsUntilChange = 3600
	far.UntilChangeAt = now

	unknown := *base // older scheduler: no estimate → no penalty
	unknown.SecondsUntilChange = 0
	unknown.UntilChangeAt = time.Time{}

	wSoon, wFar, wUnknown := computeWeight(&soon), computeWeight(&far), computeWeight(&unknown)
	if wSoon >= wFar*yieldSoonPenalty*2 {
		t.Errorf("about-to-yield worker must be heavily de-prioritized: soon=%v far=%v", wSoon, wFar)
	}
	if wUnknown != wFar {
		t.Errorf("unknown estimate must carry NO penalty: unknown=%v far=%v", wUnknown, wFar)
	}
	// Decay: an estimate observed 55s ago with 60s remaining is now inside the window.
	decayed := *base
	decayed.SecondsUntilChange = 100
	decayed.UntilChangeAt = now.Add(-55 * time.Second) // 45s effective remaining < 60s window
	if computeWeight(&decayed) >= wFar {
		t.Error("estimate must decay with age (45s effective remaining → penalized)")
	}
}

// End-to-end steering: with one worker yielding in 10s and one clear, ~all requests
// go to the clear worker (soft preference, not hard exclusion).
func TestPredictive_RoutingSteersAwayFromYieldingWorker(t *testing.T) {
	logger := testLogger()
	registry := worker.NewRegistry(logger, "")
	for _, id := range []string{"w-soon", "w-clear"} {
		registry.Register(worker.WorkerRegistration{ID: id, Endpoint: "http://x", SchedulerURL: "http://x", GPUCount: 1})
		registry.UpdateState(id, "GPU_STATE_AVAILABLE", "running", 0, "default", 1)
	}
	registry.SetUntilChange("w-soon", 10)
	registry.SetUntilChange("w-clear", 3600)

	picked := map[string]int{}
	for i := 0; i < 400; i++ {
		w, err := selectWorkerForModel(registry, "default", nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		picked[w.ID]++
	}
	if picked["w-clear"] < 350 {
		t.Errorf("clear worker must dominate routing: got %v", picked)
	}
	if picked["w-soon"] == 0 {
		t.Log("note: soft preference may still occasionally pick w-soon; 0 hits is fine too")
	}
}

// When EVERY worker is about to yield, routing still works (no starvation).
func TestPredictive_AllYieldingSoonStillRoutes(t *testing.T) {
	logger := testLogger()
	registry := worker.NewRegistry(logger, "")
	registry.Register(worker.WorkerRegistration{ID: "w0", Endpoint: "http://x", SchedulerURL: "http://x", GPUCount: 1})
	registry.UpdateState("w0", "GPU_STATE_AVAILABLE", "running", 0, "default", 1)
	registry.SetUntilChange("w0", 5)
	if _, err := selectWorkerForModel(registry, "default", nil, 0); err != nil {
		t.Fatalf("all-yielding-soon must still route (B2 resume beats a 503): %v", err)
	}
}

// Honest Retry-After: the smallest mining worker's resume estimate, clamped.
func TestPredictive_RetryAfterUsesResumeEstimate(t *testing.T) {
	logger := testLogger()
	registry := worker.NewRegistry(logger, "")
	for _, id := range []string{"m1", "m2"} {
		registry.Register(worker.WorkerRegistration{ID: id, Endpoint: "http://x", SchedulerURL: "http://x", GPUCount: 1})
		registry.UpdateState(id, "GPU_STATE_WINDOW_POST", "paused", 0, "default", 1)
	}
	registry.SetUntilChange("m1", 42)
	registry.SetUntilChange("m2", 300) // clamps to 120 if it were the min

	gw := New(registry, config.GatewayConfig{RequestTimeoutSec: 1,
		APIKeys: []config.APIKey{{Key: "test", Name: "u"}}}, logger)
	defer gw.Close()

	if got := gw.retryAfterEstimate(); got != "42" {
		t.Errorf("Retry-After must be the smallest resume estimate: want 42, got %s", got)
	}

	// No estimates at all → legacy fixed 30.
	registry2 := worker.NewRegistry(logger, "")
	registry2.Register(worker.WorkerRegistration{ID: "m", Endpoint: "http://x", SchedulerURL: "http://x", GPUCount: 1})
	registry2.UpdateState("m", "GPU_STATE_WINDOW_POST", "paused", 0, "default", 1)
	gw2 := New(registry2, config.GatewayConfig{RequestTimeoutSec: 1,
		APIKeys: []config.APIKey{{Key: "test", Name: "u"}}}, logger)
	defer gw2.Close()
	if got := gw2.retryAfterEstimate(); got != "30" {
		t.Errorf("no estimate → legacy 30, got %s", got)
	}
}

// The 503 surface actually carries the estimate (integration through the queue path).
func TestPredictive_503CarriesHonestRetryAfter(t *testing.T) {
	logger := testLogger()
	registry := worker.NewRegistry(logger, "")
	registry.Register(worker.WorkerRegistration{ID: "m1", Endpoint: "http://127.0.0.1:1", SchedulerURL: "http://127.0.0.1:1", GPUCount: 1})
	registry.UpdateState("m1", "GPU_STATE_WINNING_POST", "paused", 0, "default", 1)
	registry.SetUntilChange("m1", 17)

	gw := New(registry, config.GatewayConfig{RequestTimeoutSec: 1, QueueTimeoutSec: 1,
		APIKeys: []config.APIKey{{Key: "test", Name: "u"}}}, logger)
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	defer gw.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"default","messages":[{"role":"user","content":"x"}],"max_tokens":4}`))
	req.Header.Set("Authorization", "Bearer test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
	ra, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After not numeric: %q", resp.Header.Get("Retry-After"))
	}
	// 17s estimate decays during the ~1-2s queue wait; must be ~estimate, clamped ≥5.
	if ra < 5 || ra > 17 {
		t.Errorf("Retry-After must reflect the mining worker's resume estimate (≈17s, ≥5 clamp), got %d", ra)
	}
}
