package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"openmodel/sp-state-agent/internal/worker"
)

// probe.go — automated capability spot-checks that close the "register → audit →
// punish" loop. A background auditor periodically picks a servable self-registered
// worker, asks it a handful of FRESH deterministically-scored questions through its
// own inference endpoint, and scores the answers. A worker that scores far below the
// capability its claimed model should have is auto-banned from routing (the ban
// lever); on-chain confiscation stays a human arbiter action (real money).
//
// Fresh generated questions (not a fixed set) mean an SP cannot memorize the probe,
// and the traffic is ordinary short chat completions — indistinguishable from a user
// asking a math question. This is the active-probe path; post-hoc audit of real
// sampled traffic (A7) is the complementary undetectable path (future).

// ProbeConfig configures the background auditor.
type ProbeConfig struct {
	Enabled        bool
	IntervalSec    int     // how often to probe one worker; 0 disables
	NumQuestions   int     // questions per probe run
	MinScore       float64 // hard floor: below this → suspect regardless of model
	BanSeconds     int     // ban duration on a suspect verdict
	RequestTimeout time.Duration
	// AdmissionGate turns registration into evidence: every model a
	// self-registered worker claims is probed BEFORE routing trusts it
	// (verified_models). Requires Enabled.
	AdmissionGate bool
	// VerifyTTL is how long a verification stays fresh before the auditor
	// re-confirms it (spread over the regular cadence, service uninterrupted).
	VerifyTTL time.Duration // default 7 days
	// ModelFloors overrides the per-model score floors (claimed model → floor).
	// Unlisted models fall back to MinScore.
	ModelFloors map[string]float64
	// SpotMinInterval is the per-worker cooldown between routine spot checks
	// (default 3 days). The spot tick picks from the self-registered pool only,
	// so with few external workers a bare IntervalSec cadence would concentrate
	// every probe on the same machines. Admission and TTL re-verification are
	// exempt: a new worker must still be verified immediately.
	SpotMinInterval time.Duration
}

// defaultModelFloors — the pooled score a worker must reach FOR THE MODEL IT
// CLAIMS (pooled over evidenceWindow runs, then confirmed by an independent
// re-test before any punishment).
//
// Per-model on purpose: a low score is not evidence of fraud, it is evidence of
// a small model. An honest 1.5B scoring like a 1.5B must pass; the same score
// while CLAIMING a 32B is the fraud this gate exists to catch.
//
// Measured on 320 freshly generated items per model, THROUGH THE CHAT-TEMPLATE
// PATH (the maintenance-window fleet run of 2026-07-30; run_probe with
// error-exclusion). Every model on the official list is measured — no guessed
// entries. Template rendering moved every score up, but unevenly, and that
// asymmetry is what the floors encode:
//
//	1.5B 0.881      3B 0.991      4b 1.000    8b 0.997
//	20b  1.000      32b ~1.0 (64/64 spot run)
//
// Floors: lowest threshold an honest model of that tier falls below <1% of the
// time over a 48-item pooled window (binomial). Exact fractions, not rounded
// decimals — a floor of "0.958" would sit above 46/48 itself.
//
//	        measured   floor        per-window false-ban
//	1.5B      0.881     37/48                0.91%
//	3B+       0.991     46/48                0.93%
//
// Why the floors moved (history worth keeping): under bare-joined prompts the
// tiers measured 0.856 vs 0.950 and the best achievable floor let a 1.5B
// impersonate a big model ~45% of the time per window. Templates lifted the big
// tier to ~0.99 while the 1.5B only reached 0.881, so the SAME 1% false-ban
// constraint now yields 46/48 — impersonation escape drops to ~6% per window,
// ~0.4% with the confirmation run. Small samples flatter models; re-measure on
// 320 freshly generated items whenever the serving path changes the prompt.
//
// STILL NOT SEPARABLE: 3B through 32B measure 0.991-1.000 — statistically one
// tier. A 3B claiming a 32B is invisible to this instrument; raising the big
// floors past 46/48 would only ban honest 3B-class workers.
var defaultModelFloors = map[string]float64{
	"Qwen/Qwen2.5-1.5B-Instruct": 37.0 / 48.0,
	"Qwen/Qwen2.5-3B-Instruct":   46.0 / 48.0,
	"qwen3-4b":                   46.0 / 48.0,
	"qwen3-8b":                   46.0 / 48.0,
	"qwen3-32b":                  46.0 / 48.0,
	"gpt-oss-20b":                46.0 / 48.0,
	// Qwen3.8-27B sits in the same statistically-inseparable big tier (its
	// admission runs measured 16/16); floor inherited from that tier pending a
	// dedicated 320-item fleet measurement.
	"qwen3.8-27b": 46.0 / 48.0,
}

// floorFor resolves the score floor for a claimed model.
//
// Matching is by MODEL FAMILY, not by the exact string: a worker reports what it
// loaded ("/models/Qwen--Qwen3-32B-AWQ") while the table is keyed by family
// ("qwen3-32b"), and the path prefix, the "--" separator, the quantisation
// suffix and the casing all differ. modelMatches() only reconciles path-format
// differences, so it misses this — and a miss silently falls through to the
// permissive global MinScore, i.e. a 32B judged against a 0.5 bar. That is the
// gap this normalisation closes.
//
// Longest key wins, so a more specific family ("qwen3-32b") is preferred over a
// broader one ("qwen3") if both are configured.
func (a *auditor) floorFor(model string) float64 {
	norm := normalizeModelKey(model)
	best, bestLen := 0.0, -1
	consider := func(k string, f float64) {
		nk := normalizeModelKey(k)
		if nk == "" || !strings.Contains(norm, nk) {
			return
		}
		if len(nk) > bestLen {
			best, bestLen = f, len(nk)
		}
	}
	for m, f := range a.cfg.ModelFloors { // config overrides win at equal specificity
		consider(m, f)
	}
	if bestLen >= 0 {
		return best
	}
	for m, f := range defaultModelFloors {
		consider(m, f)
	}
	if bestLen >= 0 {
		return best
	}
	return a.cfg.MinScore
}

// normalizeModelKey strips everything that varies between how a model is named
// in config and how a worker reports it: directories, separators, case.
func normalizeModelKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// banWouldEmptyFleet reports whether banning id leaves no servable worker at
// all. Losing every worker takes the whole gateway down, which is a worse
// outcome than briefly serving traffic from a suspect one: a wrong floor, a bad
// item, or a model-wide regression can flag every worker at once, and an
// automated rule must not be able to switch the service off. It happened here —
// a floor set from a mismeasured baseline banned both workers within seconds.
// The suspicion is still logged loudly for an operator to act on.
func (a *auditor) banWouldEmptyFleet(id string) bool {
	for _, w := range a.gw.registry.List() {
		if w.ID == id || w.IsBanned() {
			continue
		}
		if w.State == worker.StateIdle || w.State == worker.StateBusy {
			return false // someone else can serve
		}
	}
	return true
}

// banOrWarn applies a ban unless it would leave the fleet empty.
func (a *auditor) banOrWarn(id, reason string, until time.Time, kv ...any) {
	if a.banWouldEmptyFleet(id) {
		args := append([]any{"worker", id, "reason", reason,
			"action", "NOT banned: it is the last servable worker — service stays up, investigate manually"}, kv...)
		a.logger.Error("probe verdict withheld to keep the fleet serving", args...)
		return
	}
	a.gw.registry.SetBan(id, until, reason)
	args := append([]any{"worker", id, "until", until.Format(time.RFC3339), "reason", reason}, kv...)
	a.logger.Warn("worker auto-banned by probe", args...)
}

// probeRun is one completed probe: how many of n items were correct.
type probeRun struct{ correct, n int }

// evidenceWindow is how many runs are pooled before a verdict is taken. Three
// 16-item runs = 48 items, the point where a 1.5B's chance of passing the
// big-model floor by luck falls from 45% to ~2%.
const evidenceWindow = 3

// confirmBelowFloor re-tests the worker on a COMPLETELY FRESH item set and
// reports whether it is still below the floor.
//
// A verdict is a random variable: an honest worker lands below its floor by bad
// luck a fraction of the time, and pooling shrinks that fraction without ever
// reaching zero. Demanding that a second, independent set agree squares the
// residual — the bad luck has to strike twice — while costing a genuine cheat
// only the extra minute before it is caught. A transport error returns an error
// rather than a verdict, so a network blip is never read as confirmed guilt.
func (a *auditor) confirmBelowFloor(ctx context.Context, w worker.Worker, model string, floor float64) (stillBelow bool, score float64, err error) {
	qs := a.generate(a.cfg.NumQuestions)
	correct := 0
	for _, q := range qs {
		ok, aerr := a.askModel(ctx, w, model, q)
		if aerr != nil {
			return false, 0, aerr
		}
		if ok {
			correct++
		}
	}
	score = float64(correct) / float64(len(qs))
	return score < floor, score, nil
}

// record appends a run and returns the pooled score plus whether the window is
// full. A verdict on a partial window would be exactly the coarse judgement the
// pooling exists to avoid.
func (a *auditor) record(key string, correct, n int) (pooled float64, full bool) {
	a.admMu.Lock()
	defer a.admMu.Unlock()
	runs := append(a.evidence[key], probeRun{correct, n})
	if len(runs) > evidenceWindow {
		runs = runs[len(runs)-evidenceWindow:]
	}
	a.evidence[key] = runs
	c, tot := 0, 0
	for _, r := range runs {
		c += r.correct
		tot += r.n
	}
	if tot == 0 {
		return 0, false
	}
	return float64(c) / float64(tot), len(runs) >= evidenceWindow
}

func (a *auditor) resetFails(key string) { a.admMu.Lock(); delete(a.admFails, key); a.admMu.Unlock() }
func (a *auditor) bumpFails(key string) int {
	a.admMu.Lock()
	defer a.admMu.Unlock()
	a.admFails[key]++
	return a.admFails[key]
}
func (a *auditor) evidenceLen(key string) int {
	a.admMu.Lock()
	defer a.admMu.Unlock()
	return len(a.evidence[key])
}
func (a *auditor) clearEvidence(key string) { a.admMu.Lock(); a.evidence[key] = nil; a.admMu.Unlock() }

// resetAdmission wipes both the fail counter and the pooled evidence for one
// (worker, model) — the "start over" the SP-facing reverify trigger relies on.
func (a *auditor) resetAdmission(key string) {
	a.admMu.Lock()
	delete(a.admFails, key)
	a.evidence[key] = nil
	a.admMu.Unlock()
}

// AdmissionState is the SP-visible verification progress of one claimed model.
type AdmissionState struct {
	ConsecutiveFails int `json:"consecutive_fails,omitempty"`
	EvidenceRuns     int `json:"evidence_runs"`
}

// admissionSnapshot returns the in-flight verification state for a worker's
// claims, keyed by model. Read by the /v1/worker/self endpoint.
func (a *auditor) admissionSnapshot(workerID string) map[string]AdmissionState {
	a.admMu.Lock()
	defer a.admMu.Unlock()
	out := map[string]AdmissionState{}
	prefix := workerID + "|"
	for k, f := range a.admFails {
		if strings.HasPrefix(k, prefix) {
			st := out[strings.TrimPrefix(k, prefix)]
			st.ConsecutiveFails = f
			out[strings.TrimPrefix(k, prefix)] = st
		}
	}
	for k, runs := range a.evidence {
		if strings.HasPrefix(k, prefix) {
			st := out[strings.TrimPrefix(k, prefix)]
			st.EvidenceRuns = len(runs)
			out[strings.TrimPrefix(k, prefix)] = st
		}
	}
	return out
}

// probeQuestion is one generated question with a computed answer.
type probeQuestion struct {
	prompt  string
	answer  string // canonical string form; numeric compared loosely
	numeric bool
}

// StartAuditor launches the background probe loop when enabled. Safe to call with
// probing disabled (no-op). Runs until ctx is cancelled.
func (g *Gateway) StartAuditor(ctx context.Context, cfg ProbeConfig) {
	if !cfg.Enabled || cfg.IntervalSec <= 0 {
		// Loud, not silent: without the auditor, self-registered workers are
		// never capability-checked and (with the gate off) route on claims
		// alone. The mainnet deployment ran like this for two weeks without
		// anyone noticing — deliberate opt-outs exist, so WARN rather than fail.
		g.logger.Warn("capability auditor DISABLED (probe.enabled false or interval_sec 0) — " +
			"self-registered workers are not probed and admission gating is off")
		return
	}
	a := newAuditor(cfg, g, g.logger)
	g.auditor = a
	go a.Run(ctx)
}

// auditor runs the periodic probe loop.
type auditor struct {
	cfg    ProbeConfig
	gw     *Gateway
	logger *slog.Logger
	client *http.Client
	rng    *rand.Rand
	rngMu  sync.Mutex
	// admMu guards admFails/evidence: they were touched only by the single Run
	// goroutine until the worker self-view endpoint (GET /v1/worker/self and the
	// reverify trigger) started reading and resetting them from HTTP handlers.
	admMu sync.Mutex
	// Admission bookkeeping (in-memory: a gateway restart simply retries).
	admFails map[string]int // "workerID|model" → consecutive failed verifications
	// evidence accumulates the last few runs per "workerID|model". A single
	// 16-item run is too coarse to convict on: at the measured capabilities a
	// 1.5B clears the big-model floor 45% of the time by luck alone. Pooling
	// three runs (48 items) drops that to ~2% without making any single probe
	// more expensive.
	evidence map[string][]probeRun
	// scoreOverride, when set, replaces the answer scorer (tests only — a mock
	// worker cannot actually solve the generated arithmetic).
	scoreOverride func(probeQuestion, string) bool
	// lastChecked records when a worker last completed a full probe (spot check
	// or admission), driving the SpotMinInterval cooldown. Touched only by the
	// single Run goroutine, so unlocked. In-memory: a restart forgets it, which
	// at worst means one extra spot check per worker after a restart.
	lastChecked map[string]time.Time
	now         func() time.Time // injectable for tests
}

func newAuditor(cfg ProbeConfig, gw *Gateway, logger *slog.Logger) *auditor {
	if cfg.NumQuestions <= 0 {
		// 16, not 12: the score is compared against a floor, so its run-to-run
		// spread has to be small enough that noise alone cannot cross the floor.
		// Each item is worth 1/n, so fewer items means a coarser, jumpier score.
		cfg.NumQuestions = 16
	}
	if cfg.MinScore <= 0 {
		cfg.MinScore = 0.5
	}
	if cfg.BanSeconds <= 0 {
		cfg.BanSeconds = 3600
	}
	if cfg.RequestTimeout <= 0 {
		// 120s, not 30s. A 32B running tensor-parallel with CUDA graphs disabled
		// takes ~39s for one item on this hardware, so a 30s budget timed out every
		// probe — the model could never be admitted at all. The timeout has to fit
		// the slowest model the fleet is expected to serve, not the fastest.
		cfg.RequestTimeout = 120 * time.Second
	}
	if cfg.VerifyTTL <= 0 {
		cfg.VerifyTTL = 7 * 24 * time.Hour
	}
	if cfg.SpotMinInterval <= 0 {
		cfg.SpotMinInterval = 3 * 24 * time.Hour
	}
	return &auditor{
		cfg:         cfg,
		gw:          gw,
		logger:      logger,
		client:      &http.Client{Timeout: cfg.RequestTimeout},
		rng:         rand.New(rand.NewSource(time.Now().UnixNano())),
		admFails:    make(map[string]int),
		evidence:    make(map[string][]probeRun),
		lastChecked: make(map[string]time.Time),
		now:         time.Now,
	}
}

// Run drives the probe loop until ctx is cancelled. Picks one servable, non-banned,
// self-registered worker per tick (round-robin by rotating the candidate list).
func (a *auditor) Run(ctx context.Context) {
	interval := time.Duration(a.cfg.IntervalSec) * time.Second
	if interval <= 0 {
		return
	}
	a.logger.Info("sp auditor started", "interval_sec", a.cfg.IntervalSec,
		"questions", a.cfg.NumQuestions, "min_score", a.cfg.MinScore, "ban_sec", a.cfg.BanSeconds,
		"spot_cooldown", a.cfg.SpotMinInterval.String())
	t := time.NewTicker(interval)
	defer t.Stop()
	var cursor int
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Priority 1: admission — a claimed model with no evidence yet. One
			// (worker, model) per tick keeps the cadence and the retry counters sane.
			if a.cfg.AdmissionGate {
				if w, model, ok := a.nextUnverified(); ok {
					a.verifyModel(ctx, w, model)
					continue
				}
				// Priority 2: evidence past its TTL — re-confirm in place (the
				// worker keeps serving; only a failed re-check removes the model).
				if w, model, ok := a.nextExpired(); ok {
					a.verifyModel(ctx, w, model)
					continue
				}
			}
			// Priority 3: the regular spot check on whatever is loaded.
			cands := a.candidates()
			if len(cands) == 0 {
				continue
			}
			w := cands[cursor%len(cands)]
			cursor++
			a.probeWorker(ctx, w)
		}
	}
}

// candidates are servable (idle/busy), non-banned, self-registered workers with a
// loaded model. Only self-registered ones are probed — operator-configured workers
// are trusted. A worker with no loaded model (mining/loading/offline) is skipped.
func (a *auditor) candidates() []worker.Worker {
	var out []worker.Worker
	for _, w := range a.gw.registry.List() {
		if !w.SelfRegistered || w.IsBanned() || w.LoadedModel == "" {
			continue
		}
		if !a.spotDue(w.ID) {
			continue
		}
		if w.State == worker.StateIdle || w.State == worker.StateBusy {
			out = append(out, w)
		}
	}
	return out
}

// spotDue reports whether the per-worker spot-check cooldown has elapsed.
func (a *auditor) spotDue(workerID string) bool {
	return a.now().Sub(a.lastChecked[workerID]) >= a.cfg.SpotMinInterval
}

// markChecked stamps a completed full probe (spot or admission) for the cooldown.
func (a *auditor) markChecked(workerID string) { a.lastChecked[workerID] = a.now() }

// nextUnverified finds one (self-registered, servable) worker + a claimed model
// it has NOT yet proven. Skips models already failing repeatedly (handled by
// verifyModel's ban). Returns ok=false when everything claimed is verified.
func (a *auditor) nextUnverified() (worker.Worker, string, bool) {
	for _, w := range a.gw.registry.List() {
		if !w.SelfRegistered || w.IsBanned() {
			continue
		}
		if w.State != worker.StateIdle && w.State != worker.StateBusy {
			continue // needs to be servable to accept a switch
		}
		for _, m := range w.SupportedModels {
			if workerVerifiedFor(&w, m) {
				continue
			}
			return w, m, true
		}
		// A worker that advertised no supported list still gets its loaded model
		// verified — otherwise the gate would starve it forever.
		if len(w.SupportedModels) == 0 && w.LoadedModel != "" && !workerVerifiedFor(&w, w.LoadedModel) {
			return w, w.LoadedModel, true
		}
	}
	return worker.Worker{}, "", false
}

// nextExpired finds one verified (worker, model) whose confirmation is older
// than VerifyTTL. Re-verification is in place: the model stays in verified_models
// (worker keeps serving it) unless the re-check FAILS.
func (a *auditor) nextExpired() (worker.Worker, string, bool) {
	for _, w := range a.gw.registry.List() {
		if !w.SelfRegistered || w.IsBanned() || len(w.VerifiedModels) == 0 {
			continue
		}
		if w.State != worker.StateIdle && w.State != worker.StateBusy {
			continue
		}
		if time.Since(w.VerifiedAt) < a.cfg.VerifyTTL {
			continue
		}
		// Re-confirm the loaded model if it is one of the verified ones (no switch
		// cost); otherwise the first verified model.
		if w.LoadedModel != "" && workerVerifiedFor(&w, w.LoadedModel) {
			return w, w.LoadedModel, true
		}
		return w, w.VerifiedModels[0], true
	}
	return worker.Worker{}, "", false
}

// verifyModel switches the worker to `model` (if needed), probes it against the
// model's score floor, and updates verified_models. Passing adds the model;
// failing removes it and, after 3 consecutive fails, bans the worker (claiming a
// model it cannot serve is the fraud the gate exists to catch). Transport errors
// never count — a mining yield or blip just retries next tick.
func (a *auditor) verifyModel(ctx context.Context, w worker.Worker, model string) {
	key := w.ID + "|" + model
	qs := a.generate(a.cfg.NumQuestions)
	correct := 0
	for _, q := range qs {
		ok, err := a.askModel(ctx, w, model, q)
		if err != nil {
			a.logger.Warn("admission probe aborted (transport error, not counted)",
				"worker", w.ID, "model", model, "error", err)
			return
		}
		if ok {
			correct++
		}
	}
	score := float64(correct) / float64(len(qs))
	floor := a.floorFor(model)
	pooled, full := a.record(key, correct, len(qs))
	// Verdict on POOLED evidence, never on one run — see evidenceWindow.
	// A first pass admits optimistically (the worker starts earning while
	// evidence accumulates); only a full window can convict.
	pass := pooled >= floor
	a.logger.Info("admission probe complete", "worker", w.ID, "model", model,
		"score", fmt.Sprintf("%.3f", score), "pooled", fmt.Sprintf("%.3f", pooled),
		"floor", fmt.Sprintf("%.3f", floor), "correct", correct, "n", len(qs),
		"window_full", full, "pass", pass)

	// Recompute the verified set from the CURRENT registry snapshot (this probe
	// may have run for minutes; a concurrent re-register could have changed it).
	cur, ok := a.gw.registry.Get(w.ID)
	if !ok {
		return
	}
	set := map[string]bool{}
	for _, m := range cur.VerifiedModels {
		set[m] = true
	}
	if pass {
		a.resetFails(key)
		set[model] = true
	} else if !full {
		// Below the floor but the window is not full yet: not enough evidence to
		// remove a model or punish. Keep whatever is already verified and wait.
		a.logger.Info("admission probe below floor, gathering more evidence",
			"worker", w.ID, "model", model, "pooled", fmt.Sprintf("%.3f", pooled),
			"runs", a.evidenceLen(key), "need", evidenceWindow)
		return
	} else {
		delete(set, model)
		fails := a.bumpFails(key)
		a.logger.Warn("admission probe failed on pooled evidence",
			"worker", w.ID, "model", model, "pooled", fmt.Sprintf("%.3f", pooled),
			"consecutive_fails", fails,
			"note", "claimed a model it does not serve at its capability band")
		if fails >= 3 {
			// Independent confirmation before any punishment (see confirmBelowFloor).
			stillBad, confScore, cerr := a.confirmBelowFloor(ctx, w, model, floor)
			if cerr != nil {
				a.logger.Warn("confirmation run failed transport-wise; no punishment this cycle",
					"worker", w.ID, "model", model, "error", cerr)
				return
			}
			if !stillBad {
				a.logger.Info("confirmation run PASSED — pooled verdict not upheld, no ban",
					"worker", w.ID, "model", model,
					"confirm_score", fmt.Sprintf("%.3f", confScore), "floor", fmt.Sprintf("%.3f", floor))
				a.resetAdmission(key)
				return
			}
			until := time.Now().Add(time.Duration(a.cfg.BanSeconds) * time.Second)
			a.banOrWarn(w.ID,
				fmt.Sprintf("admission: model %s pooled %.3f below floor %.3f over %d windows",
					model, pooled, floor, fails),
				until, "model", model)
			a.resetFails(key)
		}
	}
	models := make([]string, 0, len(set))
	for m := range set {
		models = append(models, m)
	}
	a.gw.registry.SetVerified(w.ID, models)
	// An admission run IS a full check — no point spot-checking the same worker
	// again within the cooldown.
	a.markChecked(w.ID)
}

// probeWorker runs one probe against a single worker and bans it on a suspect verdict.
func (a *auditor) probeWorker(ctx context.Context, w worker.Worker) {
	qs := a.generate(a.cfg.NumQuestions)
	correct := 0
	for _, q := range qs {
		ok, err := a.ask(ctx, w, q)
		if err != nil {
			// A transport error is not evidence of fraud — skip the run rather than
			// punish for a network blip or a mid-probe mining yield.
			a.logger.Warn("probe aborted (transport error, not counted)", "worker", w.ID, "error", err)
			return
		}
		if ok {
			correct++
		}
	}
	score := float64(correct) / float64(len(qs))
	a.markChecked(w.ID) // a full run completed — start the spot-check cooldown
	pooled, full := a.record(w.ID+"|"+w.LoadedModel, correct, len(qs))
	// Same rule as admission: convict on pooled evidence only. One unlucky run
	// must never ban a worker that is serving the model it claims.
	floor := a.floorFor(w.LoadedModel)
	suspect := full && pooled < floor
	a.logger.Info("probe complete", "worker", w.ID, "miner", w.MinerAddress,
		"model", w.LoadedModel, "score", fmt.Sprintf("%.3f", score),
		"pooled", fmt.Sprintf("%.3f", pooled), "floor", fmt.Sprintf("%.3f", floor),
		"correct", correct, "n", len(qs), "window_full", full, "suspect", suspect)
	if suspect {
		// Independent confirmation before punishment (see confirmBelowFloor).
		stillBad, confScore, cerr := a.confirmBelowFloor(ctx, w, w.LoadedModel, floor)
		if cerr != nil {
			a.logger.Warn("confirmation run failed transport-wise; no punishment this cycle",
				"worker", w.ID, "error", cerr)
			return
		}
		if !stillBad {
			a.logger.Info("confirmation run PASSED — pooled verdict not upheld, no ban",
				"worker", w.ID, "model", w.LoadedModel,
				"confirm_score", fmt.Sprintf("%.3f", confScore), "floor", fmt.Sprintf("%.3f", floor))
			a.clearEvidence(w.ID + "|" + w.LoadedModel) // stale evidence: restart the window
			return
		}
		until := time.Now().Add(time.Duration(a.cfg.BanSeconds) * time.Second)
		reason := fmt.Sprintf("probe pooled score %.3f below floor %.3f over %d runs (model %s)",
			pooled, floor, evidenceWindow, w.LoadedModel)
		a.banOrWarn(w.ID, reason, until, "miner", w.MinerAddress,
			"pooled", fmt.Sprintf("%.3f", pooled),
			"note", "on-chain confiscation of frozen earnings requires manual arbiter review")
	}
}

// ask sends one probe question against the worker's LOADED model.
func (a *auditor) ask(ctx context.Context, w worker.Worker, q probeQuestion) (bool, error) {
	return a.askModel(ctx, w, w.LoadedModel, q)
}

// askModel sends one probe question with an EXPLICIT model name (admission probes
// a claimed model, triggering the worker's own switch-and-load). Scores it.
func (a *auditor) askModel(ctx context.Context, w worker.Worker, model string, q probeQuestion) (bool, error) {
	body, _ := json.Marshal(map[string]any{
		"model": model,
		// "/no_think" mirrors the offline baseline runner's --no-think flag. Qwen3
		// models otherwise spend the whole token budget inside a reasoning block and
		// never emit the ANSWER: line — measured here as 800/800 completion tokens
		// and a reply cut off mid-sentence, which scores 0 no matter how capable the
		// model is. Models without a thinking mode ignore the token, so every model
		// is measured in the same direct-answer mode the floors were derived in.
		"messages":    []map[string]string{{"role": "user", "content": q.prompt + pickInstruction(q.prompt) + " /no_think"}},
		"temperature": 0,
		// 2048. This budget has now truncated two different models: a 1.5B at 512,
		// and a 32B at 800 — the latter needed 845 tokens and stopped 45 short of
		// its ANSWER: line, scoring 0 on items it had actually solved. Reasoning
		// models write long even when asked not to, and a truncated reply is
		// indistinguishable from a wrong one to the scorer. Budget for the most
		// verbose model in the fleet; unused tokens cost nothing because
		// generation stops at the model's own stop token.
		"max_tokens": 2048,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(w.Endpoint, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Wear the same headers a forwarded user request wears. A probe that is
	// distinguishable is a probe that can be gamed: a worker seeing no
	// X-Request-ID could serve the good model to audits and a cheap one to
	// paying traffic. Rotating the instruction wording (content layer) was not
	// enough while the header set still gave it away.
	req.Header.Set("X-Request-ID", generateRequestID())
	if w.HasFeature(worker.FeatureReceipt) {
		req.Header.Set("X-OM-Receipt-Req", "1")
	}
	if w.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+w.AuthToken)
	}
	probeClient := a.client
	if a.gw != nil && a.gw.workerMTLS.Enabled() {
		// The probe hits the worker's inference port directly — the same surface
		// the forwarders use, so it must present the same client certificate.
		probeClient = a.gw.workerMTLS.ClientFor(w.ID, a.client.Timeout, nil)
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("worker returned %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, err
	}
	var parsed struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Choices) == 0 {
		return false, fmt.Errorf("unparseable worker response")
	}
	if a.scoreOverride != nil {
		return a.scoreOverride(q, parsed.Choices[0].Message.Content), nil
	}
	return scoreProbeAnswer(q, parsed.Choices[0].Message.Content), nil
}

// probeInstructions rotate by content hash so there is no single fixed suffix to
// fingerprint as "this request is a probe" — a worker that can recognise a probe
// can serve it with a better model than it serves real traffic. Mirrors the pool
// in probe/run_probe.py; every variant still ends in a parseable ANSWER: line.
var probeInstructions = []string{
	"\n\nSolve it. Think briefly if needed, then on the final line write the word ANSWER: followed by only the final result.",
	"\n\nWork it out, then finish with a line beginning ANSWER: and just the answer.",
	"\n\nGive your reasoning if you like, but end your reply with 'ANSWER: <result>'.",
	"\n\nPlease answer. Put your final answer on the last line as ANSWER: <value>.",
	"\n\nAfter any working, state the result on a final line: ANSWER: ...",
}

// pickInstruction is stable per prompt (same rule as the Python runner), so the
// baseline and the live probe ask an identical question.
func pickInstruction(prompt string) string {
	h := 0
	for _, c := range prompt {
		h += int(c)
	}
	return probeInstructions[h%len(probeInstructions)]
}

var answerLineRe = regexp.MustCompile(`(?i)ANSWER:\s*(.*)`)
var numberRe = regexp.MustCompile(`-?\d+(?:\.\d+)?`)

// scoreProbeAnswer mirrors run_probe.py: parse the ANSWER line, then numeric
// compare or whole-token string match (word boundary, answer line only).
func scoreProbeAnswer(q probeQuestion, response string) bool {
	ans := response
	if m := answerLineRe.FindAllStringSubmatch(response, -1); len(m) > 0 {
		ans = m[len(m)-1][1]
	}
	if q.numeric {
		nums := numberRe.FindAllString(strings.ReplaceAll(ans, ",", ""), -1)
		if len(nums) == 0 {
			nums = numberRe.FindAllString(strings.ReplaceAll(response, ",", ""), -1)
		}
		if len(nums) == 0 {
			return false
		}
		got, err := strconv.ParseFloat(nums[len(nums)-1], 64)
		if err != nil {
			return false
		}
		want, _ := strconv.ParseFloat(q.answer, 64)
		return diffAbs(got, want) < 1e-6
	}
	exp := strings.ToLower(q.answer)
	re := regexp.MustCompile(`(?i)(?:^|[^a-z0-9])` + regexp.QuoteMeta(exp) + `(?:[^a-z0-9]|$)`)
	return re.MatchString(strings.ToLower(ans))
}

func diffAbs(a, b float64) float64 {
	if a < b {
		return b - a
	}
	return a - b
}
