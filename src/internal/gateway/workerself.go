package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// workerself.go — the SP's view of its own admission progress, and the lever to
// retry it.
//
// Verification state used to live only behind the operator admin port: an SP
// whose claimed model was failing its first verification could not see that it
// was failing, why, or how close to a ban it was — and after fixing the cause
// (wrong weights, bad quantisation, missing file) it could only wait for the
// auditor to wander back. Both halves are worker-token authenticated: the same
// bearer token the gateway issued at registration names exactly one worker, so
// an SP can only ever see and reset itself.

// modelAdmissionView is one claimed model's verification status as shown to the SP.
type modelAdmissionView struct {
	Model  string `json:"model"`
	Status string `json:"status"` // verified | pending | failing
	// Floor is the pooled score this model must reach (SPs deserve to know the
	// bar they are being held to; it is public in the docs anyway).
	Floor            float64 `json:"floor"`
	ConsecutiveFails int     `json:"consecutive_fails,omitempty"` // 3 ⇒ ban (after an independent confirmation)
	EvidenceRuns     int     `json:"evidence_runs,omitempty"`     // pooled runs collected so far (window: 3)
	VerifiedAt       string  `json:"verified_at,omitempty"`
}

// handleWorkerSelf: GET /v1/worker/self — worker-token authenticated self-view.
func (g *Gateway) handleWorkerSelf(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	wk, ok := g.workerFromToken(r)
	if !ok {
		jsonError(w, "invalid or missing worker token", http.StatusUnauthorized)
		return
	}

	var adm map[string]AdmissionState
	if g.auditor != nil {
		adm = g.auditor.admissionSnapshot(wk.ID)
	}
	verified := map[string]bool{}
	for _, m := range wk.VerifiedModels {
		verified[m] = true
	}

	// One row per claimed model (loaded ∪ supported), deduped, claim order kept.
	seen := map[string]bool{}
	claims := []string{}
	if wk.LoadedModel != "" {
		claims = append(claims, wk.LoadedModel)
		seen[wk.LoadedModel] = true
	}
	for _, m := range wk.SupportedModels {
		if !seen[m] {
			claims = append(claims, m)
			seen[m] = true
		}
	}

	models := []modelAdmissionView{}
	for _, m := range claims {
		v := modelAdmissionView{Model: m}
		if g.auditor != nil {
			v.Floor = g.auditor.floorFor(m)
		}
		st := adm[m]
		v.ConsecutiveFails = st.ConsecutiveFails
		v.EvidenceRuns = st.EvidenceRuns
		switch {
		case verified[m]:
			v.Status = "verified"
			if !wk.VerifiedAt.IsZero() {
				v.VerifiedAt = wk.VerifiedAt.UTC().Format(time.RFC3339)
			}
		case st.ConsecutiveFails > 0:
			v.Status = "failing"
		default:
			v.Status = "pending"
		}
		models = append(models, v)
	}

	resp := map[string]any{
		"worker_id":      wk.ID,
		"admission_gate": g.registry.AdmissionGateEnabled(),
		"models":         models,
	}
	if !wk.BannedUntil.IsZero() && wk.BannedUntil.After(time.Now()) {
		resp["banned_until"] = wk.BannedUntil.UTC().Format(time.RFC3339)
		resp["ban_reason"] = wk.BanReason
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// reverify rate limit: one request per (worker, model) per this interval. The
// auditor re-tries failing claims on its own; this lever exists for "I fixed
// the weights, examine me NOW", not for hammering the probe into a lucky pass —
// a reset also wipes any accumulated evidence, so retrying never stacks luck.
const reverifyMinInterval = 10 * time.Minute

var (
	reverifyMu   sync.Mutex
	reverifyLast = map[string]time.Time{}
)

// handleWorkerReverify: POST /v1/worker/reverify {"model": "..."} — clears the
// fail counter and pooled evidence for one claimed model so the auditor's next
// tick re-examines it immediately.
func (g *Gateway) handleWorkerReverify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	wk, ok := g.workerFromToken(r)
	if !ok {
		jsonError(w, "invalid or missing worker token", http.StatusUnauthorized)
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Model) == "" {
		jsonError(w, `body must be {"model":"<claimed model id>"}`, http.StatusBadRequest)
		return
	}
	model := strings.TrimSpace(req.Model)

	claimed := wk.LoadedModel == model
	for _, m := range wk.SupportedModels {
		claimed = claimed || m == model
	}
	if !claimed {
		jsonError(w, "model is not in this worker's claimed list", http.StatusNotFound)
		return
	}
	if !wk.BannedUntil.IsZero() && wk.BannedUntil.After(time.Now()) {
		jsonError(w, "worker is banned until "+wk.BannedUntil.UTC().Format(time.RFC3339)+
			"; re-verification resumes automatically after the ban lifts", http.StatusConflict)
		return
	}
	if g.auditor == nil {
		jsonError(w, "probe auditor is not running on this gateway", http.StatusServiceUnavailable)
		return
	}

	key := wk.ID + "|" + model
	reverifyMu.Lock()
	if last, ok := reverifyLast[key]; ok && time.Since(last) < reverifyMinInterval {
		wait := (reverifyMinInterval - time.Since(last)).Round(time.Second)
		reverifyMu.Unlock()
		w.Header().Set("Retry-After", wait.String())
		jsonError(w, "reverify was already requested recently; retry in "+wait.String(), http.StatusTooManyRequests)
		return
	}
	reverifyLast[key] = time.Now()
	reverifyMu.Unlock()

	g.auditor.resetAdmission(key)
	g.logger.Info("SP requested re-verification",
		"worker", wk.ID, "model", model, "miner", wk.MinerAddress)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"worker_id": wk.ID,
		"model":     model,
		"status":    "scheduled",
		"note":      "fail counter and pooled evidence cleared; the auditor examines unverified claims first, typically within one probe interval",
	})
}

// workerFromToken resolves the calling worker from its bearer token.
func (g *Gateway) workerFromToken(r *http.Request) (*workerView, bool) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" || tok == r.Header.Get("Authorization") {
		return nil, false
	}
	wk, ok := g.registry.FindByToken(tok)
	if !ok {
		return nil, false
	}
	return &workerView{
		ID: wk.ID, MinerAddress: wk.MinerAddress, LoadedModel: wk.LoadedModel,
		SupportedModels: wk.SupportedModels, VerifiedModels: wk.VerifiedModels,
		VerifiedAt: wk.VerifiedAt, BannedUntil: wk.BannedUntil, BanReason: wk.BanReason,
	}, true
}

// workerView is the subset of worker fields the self-view needs (a plain copy,
// safe to read after the registry lock is released).
type workerView struct {
	ID              string
	MinerAddress    string
	LoadedModel     string
	SupportedModels []string
	VerifiedModels  []string
	VerifiedAt      time.Time
	BannedUntil     time.Time
	BanReason       string
}
