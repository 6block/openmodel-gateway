package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openmodel/sp-state-agent/internal/worker"
)

// The SP's own view of its admission progress. Verification state used to live
// only behind the operator admin port: a worker whose claim was failing could
// not see that, why, or how close to a ban it was — and after fixing the cause
// it could only wait for the auditor to wander back.
func selfTestGateway(t *testing.T) (*Gateway, *auditor) {
	t.Helper()
	reg := worker.NewRegistry(testLogger(), "")
	reg.SetAdmissionGate(true)
	reg.Register(worker.WorkerRegistration{
		ID: "sp-a", Endpoint: "http://w:8000", SchedulerURL: "http://w:9090",
		GPUCount: 1, SelfRegistered: true, AuthToken: "wtok-self",
		SupportedModels: []string{"m-ok", "m-bad"},
	})
	reg.UpdateState("sp-a", "GPU_STATE_AVAILABLE", "running", 0, "m-ok", 1)
	reg.SetVerified("sp-a", []string{"m-ok"})

	g := &Gateway{registry: reg, logger: testLogger()}
	a := newAuditor(ProbeConfig{Enabled: true, IntervalSec: 300, MinScore: 0.5,
		AdmissionGate: true}, g, testLogger())
	g.auditor = a
	return g, a
}

func TestWorkerSelf_ShowsPerModelStatus(t *testing.T) {
	g, a := selfTestGateway(t)
	a.bumpFails("sp-a|m-bad") // one failed verification window so far

	req := httptest.NewRequest(http.MethodGet, "/v1/worker/self", nil)
	req.Header.Set("Authorization", "Bearer wtok-self")
	rec := httptest.NewRecorder()
	g.handleWorkerSelf(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("self view must answer the worker's own token: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		Models []modelAdmissionView `json:"models"`
	}
	json.NewDecoder(rec.Body).Decode(&resp)
	got := map[string]modelAdmissionView{}
	for _, m := range resp.Models {
		got[m.Model] = m
	}
	if got["m-ok"].Status != "verified" {
		t.Fatalf("verified claim must show verified, got %+v", got["m-ok"])
	}
	if b := got["m-bad"]; b.Status != "failing" || b.ConsecutiveFails != 1 {
		t.Fatalf("failing claim must show failing with the count, got %+v", b)
	}
	if got["m-bad"].Floor <= 0 {
		t.Fatal("the SP deserves to see the bar it is held to (floor missing)")
	}

	// A stranger's token sees nothing.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/worker/self", nil)
	req2.Header.Set("Authorization", "Bearer someone-elses-token")
	rec2 := httptest.NewRecorder()
	g.handleWorkerSelf(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token must be rejected, got %d", rec2.Code)
	}
}

func TestWorkerReverify_ClearsAndRateLimits(t *testing.T) {
	g, a := selfTestGateway(t)
	key := "sp-a|m-bad"
	a.bumpFails(key)
	a.bumpFails(key)
	a.record(key, 4, 16)

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/worker/reverify",
			strings.NewReader(`{"model":"m-bad"}`))
		req.Header.Set("Authorization", "Bearer wtok-self")
		rec := httptest.NewRecorder()
		g.handleWorkerReverify(rec, req)
		return rec
	}

	if rec := post(); rec.Code != http.StatusOK {
		t.Fatalf("reverify must succeed for an own claimed model: %d %s", rec.Code, rec.Body)
	}
	snap := a.admissionSnapshot("sp-a")
	if st := snap["m-bad"]; st.ConsecutiveFails != 0 || st.EvidenceRuns != 0 {
		t.Fatalf("reverify must wipe fails AND pooled evidence, got %+v", st)
	}

	// Second call inside the rate window is refused with a Retry-After — the
	// lever is "examine me now", not a way to hammer the probe into luck.
	if rec := post(); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("immediate second reverify must rate-limit, got %d", rec.Code)
	}

	// A model the worker never claimed is 404 — no probing other people's names.
	req := httptest.NewRequest(http.MethodPost, "/v1/worker/reverify",
		strings.NewReader(`{"model":"not-mine"}`))
	req.Header.Set("Authorization", "Bearer wtok-self")
	rec := httptest.NewRecorder()
	g.handleWorkerReverify(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unclaimed model must 404, got %d", rec.Code)
	}
}
