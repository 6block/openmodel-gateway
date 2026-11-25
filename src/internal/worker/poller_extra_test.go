package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func wLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newPoller(t *testing.T) *Poller {
	t.Helper()
	return NewPoller(NewRegistry(wLog(), ""), time.Second, 3, wLog())
}

func TestSetPollTimeout(t *testing.T) {
	p := newPoller(t)
	p.SetPollTimeout(12 * time.Second)
	if p.client.Timeout != 12*time.Second {
		t.Errorf("SetPollTimeout did not apply: got %v", p.client.Timeout)
	}
	p.SetPollTimeout(0) // ignored — keeps the current value
	if p.client.Timeout != 12*time.Second {
		t.Errorf("SetPollTimeout(0) must be a no-op, got %v", p.client.Timeout)
	}
}

func TestPollSendsWorkerToken(t *testing.T) {
	// Both the scheduler /ready and the inference /health polls must carry the
	// per-worker Bearer token so the worker can require it on a public IP.
	var readyAuth, healthAuth string
	rdy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		readyAuth = r.Header.Get("Authorization")
		fmt.Fprintln(w, "ready, gpu_state=GPU_STATE_AVAILABLE")
	}))
	defer rdy.Close()
	hlt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthAuth = r.Header.Get("Authorization")
		fmt.Fprintln(w, `{"status":"ok","engine_state":"running","active_requests":0,"loaded_model":"m"}`)
	}))
	defer hlt.Close()
	p := newPoller(t)
	if _, _, err := p.fetchGPUState(context.Background(), "w-test", rdy.URL, "wtok"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, _, _, _, err := p.fetchInferenceHealth(context.Background(), "w-test", hlt.URL, "wtok"); err != nil {
		t.Fatal(err)
	}
	if readyAuth != "Bearer wtok" {
		t.Errorf("/ready poll missing worker token: got %q", readyAuth)
	}
	if healthAuth != "Bearer wtok" {
		t.Errorf("/health poll missing worker token: got %q", healthAuth)
	}
}

func TestFetchGPUState_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	if _, _, err := newPoller(t).fetchGPUState(context.Background(), "w-test", srv.URL, ""); err == nil {
		t.Error("expected error on non-200 /ready")
	}
}

func TestFetchGPUState_Malformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ready but no gpu state field")
	}))
	defer srv.Close()
	if _, _, err := newPoller(t).fetchGPUState(context.Background(), "w-test", srv.URL, ""); err == nil {
		t.Error("expected error on /ready missing gpu_state=")
	}
}

func TestFetchInferenceHealth_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	if _, _, _, _, _, _, _, _, err := newPoller(t).fetchInferenceHealth(context.Background(), "w-test", srv.URL, ""); err == nil {
		t.Error("expected error on non-200 /health")
	}
}

func TestFetchInferenceHealth_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "{ not valid json")
	}))
	defer srv.Close()
	if _, _, _, _, _, _, _, _, err := newPoller(t).fetchInferenceHealth(context.Background(), "w-test", srv.URL, ""); err == nil {
		t.Error("expected JSON decode error")
	}
}

// TestFetchInferenceHealth_MultiGPU covers the engine_count auto-detect from a
// multi_gpu payload (the weight-from-engine-count path).
func TestFetchInferenceHealth_MultiGPU(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"status":"ok","engine_state":"running","active_requests":2,"loaded_model":"m","multi_gpu":{"engine_count":8}}`)
	}))
	defer srv.Close()
	es, ar, lm, ec, _, _, _, _, err := newPoller(t).fetchInferenceHealth(context.Background(), "w-test", srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if ec != 8 {
		t.Errorf("engineCount = %d, want 8 (auto-detected from multi_gpu)", ec)
	}
	if es != "running" || ar != 2 || lm != "m" {
		t.Errorf("got engineState=%q active=%d model=%q", es, ar, lm)
	}
}

func TestPollNow_UnknownWorker(t *testing.T) {
	newPoller(t).PollNow(context.Background(), "ghost") // must not panic
}

func TestPoolMapsFromMultiPoolHealth(t *testing.T) {
	cap, act, notReady := poolMaps(&multiGPUInfo{Pools: []poolInfo{
		{EngineState: "loading", Models: []string{"/models/a"}, Devices: []int{0, 1, 2, 3}, ActiveRequests: 2},
		{EngineState: "running", Models: []string{"/models/b"}, Devices: []int{7}, ActiveRequests: 0},
		{Models: []string{"/models/c", "/models/d"}, Devices: []int{5}},
	}})
	if cap["/models/a"] != 4 || act["/models/a"] != 2 || cap["/models/b"] != 1 || cap["/models/d"] != 1 {
		t.Fatalf("capacity %v active %v", cap, act)
	}
	// A pool still reloading (a) is not ready; running (b) or unreported (c, d) is.
	if !notReady["/models/a"] || notReady["/models/b"] || notReady["/models/c"] {
		t.Fatalf("notReady %v", notReady)
	}
	if c, a, n := poolMaps(nil); c != nil || a != nil || n != nil {
		t.Fatal("a single-engine worker has no pool maps")
	}
}

func TestModelLimitsFromHealth(t *testing.T) {
	multi := &multiGPUInfo{Pools: []poolInfo{
		{Models: []string{"/models/vl"}, Limits: ModelLimit{MaxImages: 32, ContextLimit: 8192}},
		{Models: []string{"/models/sdxl"}},
	}}
	got := modelLimits(multi, "/models/vl")
	if got["/models/vl"] != (ModelLimit{MaxImages: 32, ContextLimit: 8192}) || len(got) != 1 {
		t.Fatalf("pool limits %v", got)
	}
	single := &multiGPUInfo{single: ModelLimit{MaxImages: 1, ContextLimit: 4096}}
	if got := modelLimits(single, "/models/vl"); got["/models/vl"].MaxImages != 1 {
		t.Fatalf("single-engine limits %v", got)
	}
	if modelLimits(&multiGPUInfo{}, "/models/x") != nil || modelLimits(nil, "x") != nil {
		t.Fatal("no reported limits → nil (older workers impose nothing)")
	}
}
