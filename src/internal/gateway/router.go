package gateway

import (
	"encoding/json"
	"errors"
	"math/rand"
	"path/filepath"
	"strings"
	"time"

	"openmodel/sp-state-agent/internal/worker"
)

// ErrNoWorkerAvailable is returned when no Worker is idle or busy.
var ErrNoWorkerAvailable = errors.New("no available worker")

type candidate struct {
	worker worker.Worker
	weight float64
}

// selectWorkerForModel picks a Worker using model-aware weighted random selection.
//
// Priority:
//  1. Worker already has the requested model loaded (no switch needed)
//  2. Idle worker that lists the model in supported_models (will trigger model switch)
//  3. If model is "default" or empty, any available worker
//
// overloadFactor: when > 0, if ALL loaded workers have active_requests > gpu_count * overloadFactor,
// fall through to Priority 2 (trigger model switch on an idle worker) instead of piling onto
// the overloaded worker. Set to 0 to disable.
//
// Returns ErrNoWorkerAvailable if no suitable Worker is found.
// workerVerifiedFor reports whether model appears in w's verified list
// (normalized matching, same rules as loaded-model matching).
func workerVerifiedFor(w *worker.Worker, model string) bool {
	if model == "" {
		return false
	}
	for _, vm := range w.VerifiedModels {
		if modelMatches(vm, model) || modelMatches(model, vm) {
			return true
		}
	}
	return false
}

// requestNeeds is what one request demands of a worker's per-model limits.
type requestNeeds struct {
	images int // image parts in the messages (exact)
	tokens int // estimated prompt tokens + requested output tokens
}

// needsOf reads a chat/completions body. Zero value for anything else.
func needsOf(body []byte) requestNeeds {
	var p struct {
		Messages []struct {
			Content interface{} `json:"content"`
		} `json:"messages"`
		MaxTokens int `json:"max_tokens"`
	}
	if json.Unmarshal(body, &p) != nil {
		return requestNeeds{}
	}
	n := requestNeeds{tokens: estimatePromptTokens(body) + p.MaxTokens}
	for _, m := range p.Messages {
		_, i := messageContentChars(m.Content)
		n.images += i
	}
	return n
}

// fits reports whether w's reported limits for model can take the request. Images
// are counted exactly; the token estimate is rough (~4 chars/token), so a worker is
// ruled out only when the estimate exceeds its context by a clear margin.
func fits(w *worker.Worker, model string, n requestNeeds) bool {
	if len(w.ModelLimits) == 0 {
		return true
	}
	m, ok := loadedMatch(w, model)
	if !ok {
		return true
	}
	lim, ok := w.ModelLimits[m]
	if !ok {
		return true
	}
	if lim.MaxImages > 0 && n.images > lim.MaxImages {
		return false
	}
	if lim.ContextLimit > 0 && n.tokens > lim.ContextLimit*5/4 {
		return false
	}
	return true
}

// selectWorkerFor is selectWorkerForModel restricted to workers whose limits fit the
// request — when at least one such worker is servable. Otherwise it routes exactly as
// before and the chosen worker answers with its own precise 400 (never a worse
// outcome than ignoring the limits).
func selectWorkerFor(registry *worker.Registry, model string, n requestNeeds, exclude map[string]bool, overloadFactor ...float64) (*worker.Worker, error) {
	if n.images > 0 || n.tokens > 0 {
		ex := map[string]bool{}
		for k, v := range exclude {
			ex[k] = v
		}
		ruledOut := false
		for _, w := range registry.List() {
			if !fits(&w, model, n) {
				ex[w.ID] = true
				ruledOut = true
			}
		}
		if ruledOut {
			if w, err := selectWorkerForModel(registry, model, ex, overloadFactor...); err == nil {
				return w, nil
			}
		}
	}
	return selectWorkerForModel(registry, model, exclude, overloadFactor...)
}

func selectWorkerForModel(registry *worker.Registry, model string, exclude map[string]bool, overloadFactor ...float64) (*worker.Worker, error) {
	workers := registry.List()

	var loaded []candidate    // Priority 1: model already loaded
	var supported []candidate // Priority 2: model in supported_models, worker idle
	var fallback []candidate  // Priority 3: any available worker (for "default")

	isDefault := model == "" || model == "default"

	for _, w := range workers {
		// Only idle/busy workers are routable. mining/loading/offline workers are
		// excluded here. NOTE: registry.List() is a snapshot, so a worker can still
		// transition to mining between selection and request forwarding (a TOCTOU
		// window bounded by the poll interval). That residual race is handled by
		// the non-streaming 503 retry in handleNonStreaming, which re-selects a
		// different worker — so a transient mid-flight yield stays transparent.
		if w.State != worker.StateIdle && w.State != worker.StateBusy {
			continue
		}
		// Banned workers stay polled (state visible) but get no inference tasks
		// until the ban expires — the routing half of the misbehavior punishment.
		if w.IsBanned() {
			continue
		}
		// Probe admission gate: for self-registered workers, routing trusts only
		// the EVIDENCE-BACKED verified_models list — supported_models is the
		// claim, verified_models is the proof. A freshly-registered worker with
		// nothing verified yet receives no traffic until its first admission
		// probe passes (minutes). Operator-registered workers are exempt.
		if registry.AdmissionGateEnabled() && w.SelfRegistered {
			if isDefault {
				if !workerVerifiedFor(&w, w.LoadedModel) {
					continue
				}
			} else if !workerVerifiedFor(&w, model) {
				continue
			}
		}
		if exclude != nil && exclude[w.ID] {
			continue
		}
		// The model's own pool is still reloading: not servable here yet, even
		// though other pools on the same worker are.
		if !isDefault && !poolReadyFor(&w, model) {
			continue
		}
		// One pool of this multi-pool worker is switching models: requests for that
		// pool's models wait; the worker's other models are routed as usual.
		if !isDefault && switchingCovers(&w, model) {
			continue
		}

		wt := computeWeightFor(&w, model)

		if isDefault {
			fallback = append(fallback, candidate{worker: w, weight: wt})
			continue
		}

		// Check if this worker already has the model loaded (any of its pools)
		if workerHasLoaded(&w, model) {
			loaded = append(loaded, candidate{worker: w, weight: wt})
			continue
		}

		// Check if this worker supports the model (can load it)
		if workerSupportsModel(&w, model) {
			// Prefer idle workers for model switch (busy workers would interrupt in-flight requests)
			if w.State == worker.StateIdle || poolIdleFor(&w, model) {
				supported = append(supported, candidate{worker: w, weight: wt})
			}
			continue
		}

		// Worker doesn't know this model — skip
	}

	// Check if loaded workers are overloaded — if so, prefer model switch on idle worker
	factor := 0.0
	if len(overloadFactor) > 0 {
		factor = overloadFactor[0]
	}
	if factor > 0 && len(loaded) > 0 && len(supported) > 0 {
		allOverloaded := true
		for _, c := range loaded {
			gpus := c.worker.GPUCount
			if gpus <= 0 {
				gpus = 1
			}
			threshold := int(float64(gpus) * factor)
			if effectiveLoad(&c.worker) <= threshold {
				allOverloaded = false
				break
			}
		}
		if allOverloaded {
			// All loaded workers are overloaded — use idle worker with model switch
			return weightedPick(supported)
		}
	}

	// Pick from highest priority non-empty group
	if len(loaded) > 0 {
		return weightedPick(loaded)
	}
	if len(supported) > 0 {
		return weightedPick(supported)
	}
	if isDefault && len(fallback) > 0 {
		return weightedPick(fallback)
	}

	return nil, ErrNoWorkerAvailable
}

// selectWorker picks any available Worker (model-agnostic, for backward compat).
func selectWorker(registry *worker.Registry) (*worker.Worker, error) {
	return selectWorkerForModel(registry, "default", nil)
}

// selectWorkerExcluding picks a Worker excluding given IDs (for retry).
func selectWorkerExcluding(registry *worker.Registry, exclude map[string]bool) (*worker.Worker, error) {
	return selectWorkerForModel(registry, "default", exclude)
}

// modelMatches checks if a worker's loaded model matches the requested model.
// Handles path variations: "/models/Qwen--Qwen2.5-1.5B" matches "Qwen/Qwen2.5-1.5B".
func modelMatches(loadedModel, requestedModel string) bool {
	if loadedModel == "" || requestedModel == "" {
		return false
	}
	if loadedModel == requestedModel {
		return true
	}
	// Compare basenames: "Qwen--Qwen2.5-1.5B-Instruct" vs "Qwen/Qwen2.5-1.5B-Instruct"
	loadedBase := filepath.Base(loadedModel)
	requestedBase := filepath.Base(requestedModel)
	if loadedBase == requestedBase {
		return true
	}
	// Normalize both to HuggingFace ID format for comparison
	// "Qwen--Qwen2.5-1.5B-Instruct" → "Qwen/Qwen2.5-1.5B-Instruct"
	loadedNorm := strings.Replace(loadedBase, "--", "/", 1)
	requestedNorm := strings.Replace(requestedBase, "--", "/", 1)
	if loadedNorm == requestedNorm {
		return true
	}
	// Also compare normalized against full paths
	if loadedNorm == requestedModel || requestedNorm == loadedModel {
		return true
	}
	// Suffix match (either direction)
	if strings.HasSuffix(loadedModel, requestedModel) || strings.HasSuffix(requestedModel, loadedModel) {
		return true
	}
	return false
}

// canonicalModelID collapses the two ways the same model reaches the gateway —
// the worker's local weight path ("/models/Qwen--Qwen2.5-3B-Instruct") and the
// HuggingFace id it also reports ("Qwen/Qwen2.5-3B-Instruct") — onto the
// HuggingFace form. Routing already treats them as one (modelMatches); without
// the same collapse in the listing, /v1/models returned both and the model
// picker showed the same model twice.
func canonicalModelID(m string) string {
	base := filepath.Base(m)
	if strings.Contains(base, "--") {
		return strings.Replace(base, "--", "/", 1)
	}
	// Already an id like "Qwen/Qwen2.5-3B-Instruct": filepath.Base would drop the
	// org prefix, so keep the string as it came.
	return m
}

// workerSupportsModel checks if the requested model is in the worker's supported_models list.
func workerSupportsModel(w *worker.Worker, model string) bool {
	for _, m := range w.SupportedModels {
		if m == model || modelMatches(m, model) {
			return true
		}
	}
	return false
}

// weightedPick selects one candidate using weighted random.
func weightedPick(candidates []candidate) (*worker.Worker, error) {
	if len(candidates) == 0 {
		return nil, ErrNoWorkerAvailable
	}
	if len(candidates) == 1 {
		w := candidates[0].worker
		return &w, nil
	}

	var totalWeight float64
	for _, c := range candidates {
		totalWeight += c.weight
	}

	r := rand.Float64() * totalWeight
	for _, c := range candidates {
		r -= c.weight
		if r <= 0 {
			w := c.worker
			return &w, nil
		}
	}

	w := candidates[len(candidates)-1].worker
	return &w, nil
}

// effectiveLoad returns the worker's request load for routing decisions. It takes
// the larger of the polled ActiveRequests (accurate but up to one poll interval
// stale) and the gateway's own in-flight count (real-time, but only counts requests
// THIS gateway dispatched). Taking the max avoids double-counting while still
// reacting immediately to a burst of dispatches between polls (audit MEDIUM fix).
func effectiveLoad(w *worker.Worker) int {
	if w.InFlight > w.ActiveRequests {
		return w.InFlight
	}
	return w.ActiveRequests
}

// yieldSoonWindow / yieldSoonPenalty implement B1 predictive routing: a worker whose
// scheduler says the graceful mining yield begins within the window is heavily
// DE-PRIORITIZED (not excluded — if every worker is about to yield, serving with a
// possible B2 resume still beats a 503). WindowPoSt deadlines are deterministic
// on-chain, so this steers new work — especially long streams — away from a KNOWN
// upcoming interruption instead of discovering it mid-generation.
const (
	yieldSoonWindow  = 60 * time.Second
	yieldSoonPenalty = 0.05
)

// yieldingSoon reports whether the worker's servable window is about to close,
// decaying the poll-time estimate by the time elapsed since it was observed.
func yieldingSoon(w *worker.Worker, now time.Time) bool {
	if w.SecondsUntilChange < 0 || w.UntilChangeAt.IsZero() {
		return false // unknown (older scheduler) → no penalty
	}
	remaining := time.Duration(w.SecondsUntilChange)*time.Second - now.Sub(w.UntilChangeAt)
	return remaining < yieldSoonWindow
}

// computeWeight returns a load-balancing weight for a worker.
//
//	idle (0 requests):  weight = GPUCount
//	busy (N requests):  weight = max(1, GPUCount / (1 + N/GPUCount))
//	about to yield:     weight ×= yieldSoonPenalty (B1)
func computeWeight(w *worker.Worker) float64 {
	return computeWeightFor(w, "")
}

// computeWeightFor weighs w for a request for model. On a multi-pool worker the GPU
// count and load are those of the pool that serves the model (if resident); otherwise
// the worker as a whole.
func computeWeightFor(w *worker.Worker, model string) float64 {
	gpus := w.GPUCount
	load := effectiveLoad(w)
	if model != "" && len(w.PoolCapacity) > 0 {
		if m, ok := loadedMatch(w, model); ok {
			if c, ok := w.PoolCapacity[m]; ok {
				gpus = c
				// The pool's polled load, plus whatever the gateway has dispatched to this
				// worker since the last poll (InFlight beyond ActiveRequests) — the same
				// fresher-than-the-poll signal effectiveLoad uses for a whole worker.
				load = w.PoolActive[m]
				if extra := w.InFlight - w.ActiveRequests; extra > 0 {
					load += extra
				}
			}
		}
	}
	if gpus <= 0 {
		gpus = 1
	}
	weight := float64(gpus)
	if load > 0 {
		loadFactor := float64(load) / float64(gpus)
		weight = float64(gpus) / (1.0 + loadFactor)
		if weight < 1 {
			weight = 1
		}
	}
	if yieldingSoon(w, time.Now()) {
		weight *= yieldSoonPenalty
	}
	return weight
}

// loadedModels is every model the worker has resident right now. A multi-pool worker
// reports several; an older worker reports one, carried in LoadedModel. Both are
// folded together so callers never have to remember which field to read.
func loadedModels(w *worker.Worker) []string {
	if len(w.LoadedModels) > 0 {
		return w.LoadedModels
	}
	if w.LoadedModel != "" {
		return []string{w.LoadedModel}
	}
	return nil
}

// loadedMatch returns the resident model on w that serves the requested model
// (same model under either spelling), and whether there is one. The returned
// spelling is the worker's own, which is what a request should be rewritten to so
// the worker does not mistake a spelling difference for a model switch.
func loadedMatch(w *worker.Worker, model string) (string, bool) {
	for _, m := range loadedModels(w) {
		if modelMatches(m, model) {
			return m, true
		}
	}
	return "", false
}

// poolIdleFor: on a multi-pool worker, "busy" usually means some OTHER pool has work.
// Under mixed load the test worker was busy almost all the time (image/speech/4B traffic), so a
// request needing its text pool to switch (32B ↔ VL) never found it "idle" and timed out
// in the queue after 60 s — 34 VL and 17 32B requests in one 10-minute run. The pool
// that would switch is idle when none of its models has requests in flight; the worker
// itself then still waits for that pool's in-flight work before switching.
func poolIdleFor(w *worker.Worker, model string) bool {
	if len(w.PoolGroups) == 0 || len(w.SwitchingModels) > 0 {
		return false
	}
	for _, g := range w.PoolGroups {
		in := false
		for _, m := range g {
			if modelMatches(m, model) || modelMatches(model, m) {
				in = true
				break
			}
		}
		if !in {
			continue
		}
		for _, m := range g {
			if w.PoolActive[m] > 0 {
				return false
			}
		}
		return true
	}
	return false
}

func switchingCovers(w *worker.Worker, model string) bool {
	for _, m := range w.SwitchingModels {
		if modelMatches(m, model) || modelMatches(model, m) {
			return true
		}
	}
	return false
}

// poolReadyFor reports whether the pool serving model on w is running. A multi-pool
// worker is "running" as soon as any pool is; the model's own pool may still be
// reloading after a yield. Single-engine workers (no pool info) are always ready here
// — their worker-level state already says it.
func poolReadyFor(w *worker.Worker, model string) bool {
	if len(w.PoolNotReady) == 0 {
		return true
	}
	if m, ok := loadedMatch(w, model); ok {
		return !w.PoolNotReady[m]
	}
	return true
}

// workerHasLoaded reports whether any of the worker's resident models serves model.
func workerHasLoaded(w *worker.Worker, model string) bool {
	_, ok := loadedMatch(w, model)
	return ok
}

// routableModels is the set of models THIS worker can actually serve a request
// for right now: loaded ∪ supported, filtered by the admission gate's verified
// list for self-registered workers when the gate is on. This is the single
// caliber the model listing and the catalog must share with the router — a
// model shown to users but not routable (or vice versa) is a lie in one
// direction or the other.
func routableModels(w *worker.Worker, gateOn bool) []string {
	cand := append([]string{}, loadedModels(w)...)
	cand = append(cand, w.SupportedModels...)
	if !gateOn || !w.SelfRegistered {
		return cand
	}
	out := cand[:0]
	for _, m := range cand {
		if workerVerifiedFor(w, m) {
			out = append(out, m)
		}
	}
	return out
}
