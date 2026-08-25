package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/settlement"
	"openmodel/sp-state-agent/internal/worker"
)

// B2 stream resume integration tests. Trick for deterministic routing: ONE upstream
// server registered as TWO workers (w0, w1 → same URL). Segment 1 lands on either;
// after the interruption tryResume excludes it and picks the other — same server —
// where an atomic call counter switches the behavior to "verify continuation & finish".

// newResumeGateway builds a gateway with stream_resume configured and two workers
// pointing at upstream. features are advertised (or not) on both.
func newResumeGateway(t *testing.T, upstream string, resume bool, features []string) (*httptest.Server, *worker.Registry, func()) {
	t.Helper()
	logger := testLogger()
	registry := worker.NewRegistry(logger, "")
	for _, id := range []string{"w0", "w1"} {
		registry.Register(worker.WorkerRegistration{ID: id, Endpoint: upstream, SchedulerURL: upstream, GPUCount: 1})
		registry.UpdateState(id, "GPU_STATE_AVAILABLE", "running", 0, "test-model", 1)
		registry.SetFeatures(id, features, "")
	}
	gw := New(registry, config.GatewayConfig{
		RequestTimeoutSec: 5,
		APIKeys:           []config.APIKey{{Key: "test", Name: "user1"}},
		StreamResume:      resume,
	}, logger)
	srv := httptest.NewServer(gw.Handler())
	return srv, registry, func() { srv.Close(); _ = gw.Close() }
}

// interruptThenContinueServer: call 1 streams deltas "A","B","C" then a mining error
// event + [DONE]; call 2 records the continuation request and streams "D","E" + usage +
// finish + [DONE]. abortInsteadOfError=true makes call 1 hard-abort the connection
// (upstream crash) instead of sending a polite error event.
func interruptThenContinueServer(t *testing.T, calls *atomic.Int32, gotCont *atomic.Value, gotMax *atomic.Int64, abortInsteadOfError bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		flush := func() {
			if fl != nil {
				fl.Flush()
			}
		}
		if n == 1 {
			// Python-style spaced JSON — matches real py-inference output (a compact-
			// only byte marker regression is exactly what this format catches).
			for _, c := range []string{"A", "B", "C"} {
				fmt.Fprintf(w, "data: {\"id\": \"seg1\", \"choices\": [{\"delta\": {\"content\": %q}}]}\n\n", c)
				flush()
			}
			if abortInsteadOfError {
				panic(http.ErrAbortHandler) // hard upstream crash mid-stream
			}
			fmt.Fprint(w, "data: {\"error\":{\"message\":\"Engine paused during generation — request aborted\",\"type\":\"server_error\"}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			flush()
			return
		}
		// Continuation segment: record what the gateway sent.
		var body struct {
			OmContinuation string `json:"om_continuation"`
			MaxTokens      int64  `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotCont.Store(body.OmContinuation)
		gotMax.Store(body.MaxTokens)
		for _, c := range []string{"D", "E"} {
			fmt.Fprintf(w, "data: {\"id\": \"seg2\", \"choices\": [{\"delta\": {\"content\": %q}}]}\n\n", c)
			flush()
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2,\"total_tokens\":11}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
	}))
}

// readStream collects content deltas, error frames and [DONE] count from a client view.
func readStream(t *testing.T, url, body string) (contents []string, errorFrames, doneCount int) {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := line[6:]
		if payload == "[DONE]" {
			doneCount++
			continue
		}
		var d struct {
			Error   *struct{ Message string } `json:"error"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &d) != nil {
			continue
		}
		if d.Error != nil {
			errorFrames++
		}
		for _, c := range d.Choices {
			if c.Delta.Content != "" {
				contents = append(contents, c.Delta.Content)
			}
		}
	}
	return contents, errorFrames, doneCount
}

const resumeBody = `{"model":"test-model","messages":[{"role":"user","content":"tell a story"}],"max_tokens":30,"stream":true}`

// The headline behavior: a mining interruption mid-stream is invisible to the client —
// content continues seamlessly on another worker, no error frame, exactly one [DONE],
// and the continuation request carries the delivered prefix + reduced budget.
func TestStreamResume_MiningInterruptSeamless(t *testing.T) {
	var calls atomic.Int32
	var gotCont atomic.Value
	var gotMax atomic.Int64
	up := interruptThenContinueServer(t, &calls, &gotCont, &gotMax, false)
	defer up.Close()
	gw, _, cleanup := newResumeGateway(t, up.URL, true, []string{worker.FeatureContinuation})
	defer cleanup()

	contents, errFrames, done := readStream(t, gw.URL, resumeBody)

	if got := strings.Join(contents, ""); got != "ABCDE" {
		t.Errorf("client must see the spliced stream ABCDE, got %q", got)
	}
	if errFrames != 0 {
		t.Errorf("interruption must be invisible: got %d error frames", errFrames)
	}
	if done != 1 {
		t.Errorf("exactly one [DONE] must terminate the stream, got %d", done)
	}
	if c, _ := gotCont.Load().(string); c != "ABC" {
		t.Errorf("continuation must carry the delivered prefix ABC, got %q", c)
	}
	if m := gotMax.Load(); m != 27 { // 30 requested − 3 delivered
		t.Errorf("continuation max_tokens must be reduced to 27, got %d", m)
	}
}

// A hard upstream abort (worker crash / network drop) resumes the same way.
func TestStreamResume_UpstreamAbortMidStream(t *testing.T) {
	var calls atomic.Int32
	var gotCont atomic.Value
	var gotMax atomic.Int64
	up := interruptThenContinueServer(t, &calls, &gotCont, &gotMax, true)
	defer up.Close()
	gw, _, cleanup := newResumeGateway(t, up.URL, true, []string{worker.FeatureContinuation})
	defer cleanup()

	contents, errFrames, done := readStream(t, gw.URL, resumeBody)

	if got := strings.Join(contents, ""); got != "ABCDE" {
		t.Errorf("client must see the spliced stream ABCDE after a hard abort, got %q", got)
	}
	if errFrames != 0 || done != 1 {
		t.Errorf("want 0 error frames and 1 [DONE], got %d/%d", errFrames, done)
	}
	if c, _ := gotCont.Load().(string); c != "ABC" {
		t.Errorf("continuation prefix: want ABC, got %q", c)
	}
}

// stream_resume disabled → byte-for-byte legacy behavior (error frame + [DONE] pass
// through, no continuation request is made).
func TestStreamResume_DisabledKeepsLegacyWire(t *testing.T) {
	var calls atomic.Int32
	var gotCont atomic.Value
	var gotMax atomic.Int64
	up := interruptThenContinueServer(t, &calls, &gotCont, &gotMax, false)
	defer up.Close()
	gw, _, cleanup := newResumeGateway(t, up.URL, false, []string{worker.FeatureContinuation})
	defer cleanup()

	contents, errFrames, done := readStream(t, gw.URL, resumeBody)

	if got := strings.Join(contents, ""); got != "ABC" {
		t.Errorf("legacy: only the first segment's content, got %q", got)
	}
	if errFrames != 1 || done != 1 {
		t.Errorf("legacy: want 1 error frame + 1 [DONE], got %d/%d", errFrames, done)
	}
	if calls.Load() != 1 {
		t.Errorf("no continuation request may be made when disabled, upstream saw %d calls", calls.Load())
	}
}

// No worker advertises the continuation feature (old M1 fleet) → resume must NOT be
// attempted (an old worker would re-generate from scratch → duplicated text); the held
// error + [DONE] are flushed so the client still gets a well-formed stream.
func TestStreamResume_NoCapableWorkerDegradesGracefully(t *testing.T) {
	var calls atomic.Int32
	var gotCont atomic.Value
	var gotMax atomic.Int64
	up := interruptThenContinueServer(t, &calls, &gotCont, &gotMax, false)
	defer up.Close()
	gw, _, cleanup := newResumeGateway(t, up.URL, true, nil) // resume ON, no features
	defer cleanup()

	contents, errFrames, done := readStream(t, gw.URL, resumeBody)

	if got := strings.Join(contents, ""); got != "ABC" {
		t.Errorf("degraded: only first segment content, got %q", got)
	}
	if errFrames != 1 || done != 1 {
		t.Errorf("degraded: held error + [DONE] must be flushed, got %d/%d", errFrames, done)
	}
	if calls.Load() != 1 {
		t.Errorf("must not resume onto a feature-less worker, upstream saw %d calls", calls.Load())
	}
}

// Client-supplied om_continuation is rejected outright (reserved internal field).
func TestStreamResume_ClientSuppliedContinuationRejected(t *testing.T) {
	up := sseServer("data: [DONE]\n\n")
	defer up.Close()
	gw, _, cleanup := newResumeGateway(t, up.URL, true, []string{worker.FeatureContinuation})
	defer cleanup()

	req, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"x"}],"om_continuation":"sneaky","stream":true}`))
	req.Header.Set("Authorization", "Bearer test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("client om_continuation must be rejected with 400, got %d", resp.StatusCode)
	}
}

// A resumed stream is billed GATEWAY-METERED (prompt estimate + total delivered) — the
// client must not pay for the continuation prefix the final worker re-processed.
func TestStreamResume_ResumedBilledMetered(t *testing.T) {
	var calls atomic.Int32
	var gotCont atomic.Value
	var gotMax atomic.Int64
	up := interruptThenContinueServer(t, &calls, &gotCont, &gotMax, false)
	defer up.Close()

	logger := testLogger()
	registry := worker.NewRegistry(logger, "")
	for _, id := range []string{"w0", "w1"} {
		registry.Register(worker.WorkerRegistration{ID: id, Endpoint: up.URL, SchedulerURL: up.URL, GPUCount: 1})
		registry.UpdateState(id, "GPU_STATE_AVAILABLE", "running", 0, "test-model", 1)
		registry.SetFeatures(id, []string{worker.FeatureContinuation}, "")
	}
	scfg := &settlement.Config{
		ModelPricesUSD: map[string]string{"default": "1000000"}, // $1/token
		FILPriceUSD:    "2.0", FILPriceSource: "manual",
		SupportedTokens: []settlement.TokenConfig{
			{Symbol: "USDC", Address: billUSDC, Decimals: 6},
			{Symbol: "FIL", Address: billFIL, Decimals: 18},
		},
		DeductionPriority: []string{"USDC", "FIL"},
		DefaultMaxTokens:  100,
	}
	pricer := settlement.NewPricer(scfg, logger)
	bc := settlement.NewBalanceCache(&fakeBalanceContract{usdc: usdcWei(100000)}, scfg.SupportedTokens, pricer, 30, logger)
	bc.SetWallets([]string{billWallet})
	bc.ForceRefresh(t.Context())
	gw := New(registry, config.GatewayConfig{
		RequestTimeoutSec: 5,
		APIKeys:           []config.APIKey{{Key: "test", Name: "user1", Wallet: billWallet}},
		StreamResume:      true,
	}, logger)
	gw.SetBalanceChecker(bc, scfg)
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	defer gw.Close()

	contents, errFrames, done := readStream(t, srv.URL, resumeBody)
	if got := strings.Join(contents, ""); got != "ABCDE" || errFrames != 0 || done != 1 {
		t.Fatalf("splice failed: content=%q err=%d done=%d", got, errFrames, done)
	}

	// Billed = prompt estimate + 5 delivered tokens, at $1/token. NOT the final
	// segment's usage (prompt 9 = original + re-fed prefix, completion 2).
	wantTokens := estimatePromptTokens([]byte(resumeBody)) + 5
	want := big.NewFloat(float64(wantTokens))
	if ps := bc.GetPendingSpend(billWallet); ps.Cmp(want) != 0 {
		t.Errorf("resumed stream must bill metered $%d (prompt est + 5 delivered), got %s",
			wantTokens, ps.Text('f', 4))
	}
}

// A teardown error AFTER the worker's [DONE] (tunnel turning FIN into RST — observed on
// a real tunneled worker hop) must NOT be treated as an interruption: the stream is complete and
// must be billed, with a clean client-side termination.
func TestStreamResume_TeardownErrorAfterDoneIsClean(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\": [{\"delta\": {\"content\": \"Hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\": [], \"usage\": {\"prompt_tokens\": 3, \"completion_tokens\": 1, \"total_tokens\": 4}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
		panic(http.ErrAbortHandler) // teardown noise AFTER a complete stream
	}))
	defer up.Close()
	gw, _, cleanup := newResumeGateway(t, up.URL, true, []string{worker.FeatureContinuation})
	defer cleanup()

	contents, errFrames, done := readStream(t, gw.URL, resumeBody)
	if got := strings.Join(contents, ""); got != "Hi" {
		t.Errorf("content: want Hi, got %q", got)
	}
	if errFrames != 0 || done != 1 {
		t.Errorf("teardown-after-DONE must end cleanly: err=%d done=%d", errFrames, done)
	}
}
