package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/worker"
)

// newSamplingGateway builds a gateway with one idle worker per endpoint and a
// verification sampler at rate=1 (retain everything) writing to samplePath. No settlement
// — sampling is independent of billing.
func newSamplingGateway(t *testing.T, samplePath string, endpoints ...string) (*httptest.Server, func()) {
	t.Helper()
	logger := testLogger()
	registry := worker.NewRegistry(logger, "")
	for i, ep := range endpoints {
		id := fmt.Sprintf("w%d", i)
		registry.Register(worker.WorkerRegistration{ID: id, Endpoint: ep, SchedulerURL: ep, GPUCount: 1})
		// Worker CLAIMS it has loaded "big-model" (what the client asks for) — but the
		// stub upstream returns "small-model", the model-substitution fraud we detect.
		registry.UpdateState(id, "GPU_STATE_AVAILABLE", "running", 0, "big-model", 1)
	}
	gw := New(registry, config.GatewayConfig{
		RequestTimeoutSec: 5,
		APIKeys:           []config.APIKey{{Key: "test", Name: "user1"}},
	}, logger)
	gw.SetVerificationSampler(NewVerificationSampler(samplePath, 1.0, 0, 0, logger))
	srv := httptest.NewServer(gw.Handler())
	return srv, func() { srv.Close(); _ = gw.Close() }
}

// A sampled non-streaming request retains the prompt, the response, and — critically for
// model-substitution detection — the model the worker CLAIMS vs the model requested.
func TestSampling_NonStreamingCapturesModelMismatch(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Client asked for "big-model"; worker claims it served "small-model".
		fmt.Fprint(w, `{"model":"small-model","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
	}))
	defer up.Close()
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	gw, cleanup := newSamplingGateway(t, path, up.URL)
	defer cleanup()

	body := `{"model":"big-model","messages":[{"role":"user","content":"hello"}],"max_tokens":10}`
	req, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// The sample is written on the handler goroutine after the client body is delivered;
	// poll briefly for it to land.
	var got []VerificationSample
	for i := 0; i < 40; i++ {
		if got = readSamples(t, path); len(got) >= 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(got))
	}
	s := got[0]
	if s.ModelReq != "big-model" || s.ModelResp != "small-model" {
		t.Errorf("model capture wrong: req=%q resp=%q (want big-model/small-model)", s.ModelReq, s.ModelResp)
	}
	if s.Stream {
		t.Error("non-stream request recorded as stream")
	}
	if s.TotalTokens != 5 {
		t.Errorf("tokens: got %d want 5", s.TotalTokens)
	}
	if !strings.Contains(s.Request, "big-model") || !strings.Contains(s.Response, "small-model") {
		t.Errorf("request/response bodies not retained: req=%q resp=%q", s.Request, s.Response)
	}
}

// A sampled streaming request retains the delivered raw SSE and the claimed served model.
func TestSampling_StreamingCapturesDeliveredSSE(t *testing.T) {
	up := sseServer(
		"data: {\"model\":\"small-model\",\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n",
		"data: {\"model\":\"small-model\",\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2,\"total_tokens\":4}}\n\n",
		"data: [DONE]\n\n",
	)
	defer up.Close()
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	gw, cleanup := newSamplingGateway(t, path, up.URL)
	defer cleanup()

	body := `{"model":"big-model","messages":[{"role":"user","content":"hello"}],"stream":true,"max_tokens":10}`
	req, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	// Drain the stream fully so the handler reaches its terminal billing/sampling path.
	sc := bufio.NewReader(resp.Body)
	for {
		if _, rerr := sc.ReadString('\n'); rerr != nil {
			break
		}
	}
	resp.Body.Close()

	// The sample is written on the handler goroutine right before it returns; give it a
	// beat after the client-side read loop ends.
	var got []VerificationSample
	for i := 0; i < 40; i++ {
		got = readSamples(t, path)
		if len(got) >= 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(got))
	}
	s := got[0]
	if !s.Stream {
		t.Error("stream request not recorded as stream")
	}
	if s.ModelReq != "big-model" || s.ModelResp != "small-model" {
		t.Errorf("model capture wrong: req=%q resp=%q", s.ModelReq, s.ModelResp)
	}
	if !strings.Contains(s.Response, "hello") && !(strings.Contains(s.Response, "he") && strings.Contains(s.Response, "llo")) {
		t.Errorf("delivered SSE content not retained: %q", s.Response)
	}
	if s.TotalTokens != 4 {
		t.Errorf("tokens: got %d want 4", s.TotalTokens)
	}
}
