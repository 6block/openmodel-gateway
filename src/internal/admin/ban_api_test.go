package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"openmodel/sp-state-agent/internal/worker"
)

func banTestServer(t *testing.T) (*Server, *worker.Registry) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := worker.NewRegistry(logger, "")
	if _, err := reg.Register(worker.WorkerRegistration{
		ID: "sp-1", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", GPUCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(0, "", reg, nil, logger)
	return s, reg
}

func TestWorkerBanEndpoint(t *testing.T) {
	s, reg := banTestServer(t)

	// Ban with explicit duration + reason.
	body, _ := json.Marshal(map[string]any{"duration_sec": 3600, "reason": "substandard output"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/sp-1/ban", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleWorkerByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("ban: status %d body %s", rr.Code, rr.Body.String())
	}
	w, _ := reg.Get("sp-1")
	if !w.IsBanned() || w.BanReason != "substandard output" {
		t.Fatalf("ban not applied: %+v", w)
	}
	remain := time.Until(w.BannedUntil)
	if remain < 59*time.Minute || remain > 61*time.Minute {
		t.Fatalf("ban duration off: %v", remain)
	}

	// Lift.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/workers/sp-1/ban", nil)
	rr = httptest.NewRecorder()
	s.handleWorkerByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unban: status %d", rr.Code)
	}
	if w, _ := reg.Get("sp-1"); w.IsBanned() {
		t.Fatal("ban not lifted")
	}
}

func TestWorkerBanDefaultDurationAndErrors(t *testing.T) {
	s, reg := banTestServer(t)
	s.SetBanDefault(48 * time.Hour)

	// Empty body → default duration.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/sp-1/ban", nil)
	rr := httptest.NewRecorder()
	s.handleWorkerByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("default ban: status %d body %s", rr.Code, rr.Body.String())
	}
	w, _ := reg.Get("sp-1")
	remain := time.Until(w.BannedUntil)
	if remain < 47*time.Hour || remain > 49*time.Hour {
		t.Fatalf("default duration off: %v", remain)
	}

	// Unknown worker → 404.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/workers/nope/ban", nil)
	rr = httptest.NewRecorder()
	s.handleWorkerByID(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown worker: status %d", rr.Code)
	}

	// Wrong method → 405.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/workers/sp-1/ban", nil)
	rr = httptest.NewRecorder()
	s.handleWorkerByID(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET ban: status %d", rr.Code)
	}
}
