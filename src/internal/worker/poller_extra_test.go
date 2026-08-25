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
	if _, _, _, _, _, _, err := p.fetchInferenceHealth(context.Background(), "w-test", hlt.URL, "wtok"); err != nil {
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
	if _, _, _, _, _, _, err := newPoller(t).fetchInferenceHealth(context.Background(), "w-test", srv.URL, ""); err == nil {
		t.Error("expected error on non-200 /health")
	}
}

func TestFetchInferenceHealth_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "{ not valid json")
	}))
	defer srv.Close()
	if _, _, _, _, _, _, err := newPoller(t).fetchInferenceHealth(context.Background(), "w-test", srv.URL, ""); err == nil {
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
	es, ar, lm, ec, _, _, err := newPoller(t).fetchInferenceHealth(context.Background(), "w-test", srv.URL, "")
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
