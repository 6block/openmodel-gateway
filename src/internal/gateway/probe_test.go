package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/worker"
)

// fakeWorker serves /v1/chat/completions. If honest, it actually solves the probe
// question (extracts the number the prompt asks for by re-deriving is out of scope,
// so we cheat: an honest fake echoes the correct answer we precompute per prompt).
// A dishonest fake always answers a fixed wrong value.
func fakeWorkerServer(t *testing.T, honest bool, token string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		prompt := ""
		if len(req.Messages) > 0 {
			prompt = req.Messages[0].Content
		}
		reply := "ANSWER: 999999" // dishonest: always wrong
		if honest {
			reply = "ANSWER: " + solveForTest(prompt)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": reply}}},
		})
	}))
}

// fakeWorkerServerToggle is fakeWorkerServer whose honesty can flip mid-test, so
// a run of good probes can be followed by a bad one.
func fakeWorkerServerToggle(t *testing.T, honest *atomic.Bool, token string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		prompt := ""
		if len(req.Messages) > 0 {
			prompt = req.Messages[0].Content
		}
		reply := "ANSWER: 999999"
		if honest.Load() {
			reply = "ANSWER: " + solveForTest(prompt)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": reply}}},
		})
	}))
}

// solveForTest recomputes the numeric answer from the generated prompt text so the
// honest fake can pass. Mirrors the templates' arithmetic by parsing the numbers.
func solveForTest(prompt string) string {
	// The probe templates are deterministic given their prompt; rather than re-parse
	// each, the honest fake in these tests answers using a tiny lookup by keyword.
	// We instead reuse the real generator by having the auditor expose the answer,
	// which it does via probeQuestion — but the fake only sees the prompt. So we solve
	// the few arithmetic forms we need for the test by regex.
	return solveArithmetic(prompt)
}

// solveArithmetic re-derives a generated probe question's answer from its prompt
// text (test-only). Keyword dispatch over the 10 templates in probe_gen.go.
func solveArithmetic(p string) string {
	nums := numberRe.FindAllString(p, -1)
	n := make([]int, len(nums))
	for i, s := range nums {
		n[i], _ = strconv.Atoi(s)
	}
	get := func(i int) int {
		if i < len(n) {
			return n[i]
		}
		return 0
	}
	isqrtT := func(x int) int {
		r := 0
		for (r+1)*(r+1) <= x {
			r++
		}
		return r
	}
	var v int
	switch {
	// ---- discriminating templates first: several share keywords with the
	// baseline ones (an inverse handshake item also contains "handshakes"), so
	// the more specific pattern has to win.
	case strings.Contains(p, "handshakes in total. How many people"):
		for k := 2; k <= 200; k++ {
			if k*(k-1)/2 == get(0) {
				v = k
				break
			}
		}
	case strings.Contains(p, "is removed from the set"):
		v = (get(0)*get(1) - get(2)) / (get(0) - 1)
	case strings.Contains(p, "side equal to the sum of the side lengths"):
		s1, s2 := isqrtT(get(0)), isqrtT(get(1))
		v = (s1 + s2) * (s1 + s2)
	case strings.Contains(p, "days are in February in the year"):
		y := get(0)
		v = 28
		if y%4 == 0 && (y%100 != 0 || y%400 == 0) {
			v = 29
		}
	case strings.Contains(p, "half full"):
		v = get(0) - 1
	case strings.Contains(p, "cats are needed"):
		v = get(0)
	case strings.Contains(p, "distinct books be arranged"):
		v = 1
		for k := 2; k <= get(0); k++ {
			v *= k
		}
	case strings.Contains(p, "units digit of"):
		v = 1
		for k := 0; k < get(1); k++ {
			v = v * get(0) % 10
		}
	case strings.Contains(p, "days from now"):
		days := []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
		for i, d := range days {
			if strings.Contains(p, "today is "+d) {
				return days[(i+get(0))%7]
			}
		}
	case strings.Contains(p, "Working together"):
		v = get(0) * get(1) / (get(0) + get(1))

	// ---- baseline templates ----
	case strings.Contains(p, "returns"): // buy N at $P, return R
		v = (get(0) - get(2)) * get(1)
	case strings.Contains(p, "% of"): // PC% of NB
		v = get(0) * get(1) / 100
	case strings.Contains(p, "discount"): // D% off → cost PRICE, want original
		v = get(1) * 100 / (100 - get(0))
	case strings.Contains(p, "remainder"): // X mod B
		v = get(0) % get(1)
	case strings.Contains(p, "sum of all"): // "from 1 to N" — N is the SECOND number
		v = get(1) * (get(1) + 1) / 2
	case strings.Contains(p, "handshakes"): // N(N-1)/2
		v = get(0) * (get(0) - 1) / 2
	case strings.Contains(p, "next number"): // a,b,c,d → next
		v = get(3) + (get(3) - get(2))
	case strings.Contains(p, "multiplied by"): // M*x+B=C → x
		v = (get(2) - get(1)) / get(0)
	case strings.Contains(p, "books at"): // N1*P1 + N2*P2
		v = get(0)*get(1) + get(2)*get(3)
	case strings.Contains(p, "change do you receive"): // PAID - PRICE
		v = get(1) - get(0)
	}
	return strconv.Itoa(v)
}

// The honest fake must be able to answer EVERY template, otherwise "honest" in
// these tests silently means "answers half the questions" and the thresholds
// under test are measured against the wrong thing.
func TestFakeWorker_SolvesEveryTemplate(t *testing.T) {
	au := newAuditor(ProbeConfig{Enabled: true, IntervalSec: 1, NumQuestions: 16}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	bad := 0
	for i := 0; i < 400; i++ {
		for _, q := range au.generate(16) {
			if !scoreProbeAnswer(q, "ANSWER: "+solveArithmetic(q.prompt)) {
				bad++
				if bad < 4 {
					t.Errorf("fake cannot solve: %s (want %s, got %s)", q.prompt, q.answer, solveArithmetic(q.prompt))
				}
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d generated items unsolved by the honest fake", bad)
	}
}

func newProbeTestGateway(t *testing.T) *Gateway {
	t.Helper()
	reg := worker.NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	return New(reg, config.GatewayConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func registerFakeSP(t *testing.T, g *Gateway, id, model, endpoint, token string) {
	t.Helper()
	g.registry.Register(worker.WorkerRegistration{ID: id, Endpoint: endpoint, SchedulerURL: endpoint, GPUCount: 1, MinerAddress: "t0" + id, AuthToken: token, SelfRegistered: true})
	g.registry.UpdateState(id, "GPU_STATE_AVAILABLE", "running", 0, model, 1)
}

func TestProbe_HonestWorkerPasses(t *testing.T) {
	tok := "wtok-x"
	srv := fakeWorkerServer(t, true, tok)
	defer srv.Close()
	g := newProbeTestGateway(t)
	registerFakeSP(t, g, "sp-honest", "big-model", srv.URL, tok)

	a := newAuditor(ProbeConfig{NumQuestions: 12, MinScore: 0.5, BanSeconds: 60}, g, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w, _ := g.registry.Get("sp-honest")
	a.probeWorker(context.Background(), *w)

	if got, _ := g.registry.Get("sp-honest"); got.IsBanned() {
		t.Fatalf("honest worker was banned: %s", got.BanReason)
	}
}

func TestProbe_DishonestWorkerBanned(t *testing.T) {
	tok := "wtok-y"
	srv := fakeWorkerServer(t, false, tok) // always wrong answers
	defer srv.Close()
	g := newProbeTestGateway(t)
	registerFakeSP(t, g, "sp-cheat", "big-model", srv.URL, tok)
	// Bans are withheld for the last servable worker, so give the fleet a
	// healthy second member — otherwise this would test the fail-safe, not the ban.
	srvOK := fakeWorkerServer(t, true, "wtok-ok")
	defer srvOK.Close()
	registerFakeSP(t, g, "sp-backup", "big-model", srvOK.URL, "wtok-ok")

	a := newAuditor(ProbeConfig{NumQuestions: 12, MinScore: 0.5, BanSeconds: 60}, g, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w, _ := g.registry.Get("sp-cheat")
	// A verdict needs a full evidence window, so probe until one exists. A
	// single run is deliberately not enough to ban: at real capabilities one
	// unlucky run crosses a floor often enough to punish honest workers.
	for i := 0; i < evidenceWindow; i++ {
		a.probeWorker(context.Background(), *w)
	}

	got, _ := g.registry.Get("sp-cheat")
	if !got.IsBanned() {
		t.Fatal("dishonest worker (all-wrong answers) was NOT banned")
	}
	if !strings.Contains(got.BanReason, "pooled") {
		t.Fatalf("ban reason = %q", got.BanReason)
	}
}

// The complement: pooling means a verdict weighs the WINDOW, not the worst run.
// Two clean runs plus one bad one leaves pooled evidence at ~0.67, which clears
// a lenient floor — so no ban. (Against a 3B's 0.833 floor the same window WOULD
// convict, and should: a run at zero is not noise. What pooling buys is that the
// decision is made on 48 items instead of 16.)
func TestProbe_PooledWindowDecidesNotWorstRun(t *testing.T) {
	tok := "wtok-z"
	var honest atomic.Bool
	honest.Store(true)
	srv := fakeWorkerServerToggle(t, &honest, tok)
	defer srv.Close()
	g := newProbeTestGateway(t)
	// Model with no entry in defaultModelFloors → falls back to MinScore 0.5.
	registerFakeSP(t, g, "sp-blip", "some-lenient-model", srv.URL, tok)

	a := newAuditor(ProbeConfig{NumQuestions: 12, MinScore: 0.5, BanSeconds: 60}, g, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w, _ := g.registry.Get("sp-blip")
	a.probeWorker(context.Background(), *w) // good
	a.probeWorker(context.Background(), *w) // good
	honest.Store(false)
	a.probeWorker(context.Background(), *w) // one bad run

	if got, _ := g.registry.Get("sp-blip"); got.IsBanned() {
		t.Fatalf("a single bad run inside a healthy window must not ban: %s", got.BanReason)
	}
}

func TestProbe_TransportErrorDoesNotBan(t *testing.T) {
	g := newProbeTestGateway(t)
	// endpoint points nowhere → every ask errors → run aborts, no ban
	registerFakeSP(t, g, "sp-down", "big-model", "http://127.0.0.1:1", "")
	a := newAuditor(ProbeConfig{NumQuestions: 4, MinScore: 0.5, BanSeconds: 60}, g, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.client.Timeout = 500 * time.Millisecond
	w, _ := g.registry.Get("sp-down")
	a.probeWorker(context.Background(), *w)
	if got, _ := g.registry.Get("sp-down"); got.IsBanned() {
		t.Fatal("a worker unreachable due to transport error must NOT be banned (network blip ≠ fraud)")
	}
}

func TestProbe_OnlySelfRegisteredProbed(t *testing.T) {
	g := newProbeTestGateway(t)
	// operator-configured (not self-registered) worker
	g.registry.Register(worker.WorkerRegistration{ID: "sp-static", Endpoint: "http://x", GPUCount: 1})
	g.registry.UpdateState("sp-static", "GPU_STATE_AVAILABLE", "running", 0, "m", 1)
	a := newAuditor(ProbeConfig{NumQuestions: 4}, g, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if len(a.candidates()) != 0 {
		t.Fatal("operator-configured worker must not be a probe candidate")
	}
}

func TestScoreProbeAnswer(t *testing.T) {
	// numeric: parse ANSWER line
	if !scoreProbeAnswer(probeQuestion{answer: "42", numeric: true}, "reasoning...\nANSWER: 42") {
		t.Fatal("numeric exact should pass")
	}
	if scoreProbeAnswer(probeQuestion{answer: "42", numeric: true}, "ANSWER: 43") {
		t.Fatal("numeric wrong should fail")
	}
	// numeric with $ and words
	if !scoreProbeAnswer(probeQuestion{answer: "50", numeric: true}, "ANSWER: $50 dollars") {
		t.Fatal("numeric with decoration should pass")
	}
	// string word-boundary (no false positive)
	if scoreProbeAnswer(probeQuestion{answer: "no"}, "ANSWER: cannot be determined") {
		t.Fatal("'no' must not match 'cannot'")
	}
}

// An automated rule must never be able to take the whole service down. A wrong
// floor or a bad item can flag every worker at once — it did here, banning both
// workers within seconds — so the last servable worker is kept in rotation and
// the verdict is escalated to a human instead.
func TestProbe_NeverBansTheLastServableWorker(t *testing.T) {
	tok := "wtok-last"
	srv := fakeWorkerServer(t, false, tok) // always wrong
	defer srv.Close()
	g := newProbeTestGateway(t)
	registerFakeSP(t, g, "sp-only", "big-model", srv.URL, tok)

	a := newAuditor(ProbeConfig{NumQuestions: 12, MinScore: 0.5, BanSeconds: 60}, g, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w, _ := g.registry.Get("sp-only")
	for i := 0; i < evidenceWindow+2; i++ {
		a.probeWorker(context.Background(), *w)
	}
	if got, _ := g.registry.Get("sp-only"); got.IsBanned() {
		t.Fatal("the only servable worker must not be banned — that ends all service")
	}

	// With a second healthy worker present, the cheat IS banned.
	tok2 := "wtok-other"
	srv2 := fakeWorkerServer(t, true, tok2)
	defer srv2.Close()
	registerFakeSP(t, g, "sp-good", "big-model", srv2.URL, tok2)
	for i := 0; i < evidenceWindow+2; i++ {
		a.probeWorker(context.Background(), *w)
	}
	if got, _ := g.registry.Get("sp-only"); !got.IsBanned() {
		t.Fatal("with another worker available, a persistently failing worker must be banned")
	}
	if got, _ := g.registry.Get("sp-good"); got.IsBanned() {
		t.Fatal("the healthy worker must not be collateral damage")
	}
}

// A worker that fails the pooled window but PASSES the independent confirmation
// run must not be banned: the pooled verdict was bad luck, not fraud. This is
// the last guard before an irreversible-looking action, and it is what keeps a
// sub-1% per-window false-ban rate from ever becoming a real ban.
func TestProbe_ConfirmationRunSpares(t *testing.T) {
	var honest atomic.Bool
	honest.Store(false) // failing at first, so the pooled window fills with misses
	srv := fakeWorkerServerToggle(t, &honest, "")
	defer srv.Close()

	g := newProbeTestGateway(t)
	registerFakeSP(t, g, "sp-unlucky", "big-model", srv.URL, "")
	// A second healthy worker: bans are withheld for the last servable one.
	srvOK := fakeWorkerServer(t, true, "wtok-ok")
	defer srvOK.Close()
	registerFakeSP(t, g, "sp-backup", "big-model", srvOK.URL, "wtok-ok")

	a := newAuditor(ProbeConfig{NumQuestions: 8, MinScore: 0.5, BanSeconds: 60}, g,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	w, _ := g.registry.Get("sp-unlucky")
	for i := 0; i < evidenceWindow-1; i++ {
		a.probeWorker(t.Context(), *w)
	}
	// Recover before the run that would convict: the confirmation set sees a
	// healthy worker, so the pooled verdict must not be acted on.
	honest.Store(true)
	a.probeWorker(t.Context(), *w)

	if got, _ := g.registry.Get("sp-unlucky"); got.IsBanned() {
		t.Fatal("a worker that passes the independent confirmation run must NOT be banned")
	}
}

// The probe must be indistinguishable from forwarded user traffic at the header
// level too: a worker that can recognise an audit can serve it a better model
// than it serves to paying customers. Rotating prompt wording is not enough
// while the header set still says "this one is the probe".
func TestProbe_RequestLooksLikeUserTraffic(t *testing.T) {
	seen := make(chan http.Header, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case seen <- r.Header.Clone():
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ANSWER: 1"}}},
		})
	}))
	defer srv.Close()

	g := newProbeTestGateway(t)
	registerFakeSP(t, g, "sp-x", "big-model", srv.URL, "wtok")
	a := newAuditor(ProbeConfig{NumQuestions: 1, MinScore: 0.5}, g,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	w, _ := g.registry.Get("sp-x")
	a.askModel(t.Context(), *w, "big-model", a.generate(1)[0])

	h := <-seen
	if rid := h.Get("X-Request-ID"); !strings.HasPrefix(rid, "req-") {
		t.Fatalf("probe must carry a normal-looking X-Request-ID, got %q", rid)
	}
	if h.Get("Authorization") != "Bearer wtok" {
		t.Fatalf("probe must authenticate like a forwarded request, got %q", h.Get("Authorization"))
	}
	if h.Get("Content-Type") != "application/json" {
		t.Fatal("probe must send the same Content-Type as forwarded traffic")
	}
}
