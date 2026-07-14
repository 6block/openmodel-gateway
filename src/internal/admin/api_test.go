package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"openmodel/sp-state-agent/internal/worker"
)

type testEnv struct {
	registry *worker.Registry
	mux      http.Handler
	token    string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	registry := worker.NewRegistry(logger, "")
	poller := worker.NewPoller(registry, 0, 3, logger)

	token := "test-admin-token"
	server := NewServer(9999, token, registry, poller, logger)

	return &testEnv{
		registry: registry,
		mux:      server.httpServer.Handler,
		token:    token,
	}
}

func (e *testEnv) request(method, path string, body interface{}) *httptest.ResponseRecorder {
	var reqBody *bytes.Buffer
	if body != nil {
		data, _ := json.Marshal(body)
		reqBody = bytes.NewBuffer(data)
	} else {
		reqBody = &bytes.Buffer{}
	}

	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.token)
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func TestRegisterWorker(t *testing.T) {
	env := newTestEnv(t)

	rec := env.request(http.MethodPost, "/api/v1/workers/register", map[string]interface{}{
		"id":            "sp-test-1",
		"endpoint":      "http://10.0.1.5:8000",
		"scheduler_url": "http://10.0.1.5:9090",
		"gpu_count":     8,
		"miner_address": "t0182063",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp["worker_id"] != "sp-test-1" {
		t.Errorf("worker_id: want sp-test-1, got %v", resp["worker_id"])
	}
	if resp["registered"] != true {
		t.Error("expected registered=true")
	}
	if resp["created"] != true {
		t.Error("expected created=true")
	}
}

func TestRegisterWorker_InvalidBody(t *testing.T) {
	env := newTestEnv(t)

	rec := env.request(http.MethodPost, "/api/v1/workers/register", map[string]interface{}{
		"id": "sp-test-1",
	})

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing endpoint, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterWorker_MethodNotAllowed(t *testing.T) {
	env := newTestEnv(t)

	rec := env.request(http.MethodGet, "/api/v1/workers/register", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestListWorkers(t *testing.T) {
	env := newTestEnv(t)

	env.registry.Register(worker.WorkerRegistration{
		ID: "w1", Endpoint: "http://a:8000", SchedulerURL: "http://a:9090", GPUCount: 4,
	})
	env.registry.Register(worker.WorkerRegistration{
		ID: "w2", Endpoint: "http://b:8000", SchedulerURL: "http://b:9090", GPUCount: 8,
	})

	rec := env.request(http.MethodGet, "/api/v1/workers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp["total"] != float64(2) {
		t.Errorf("total: want 2, got %v", resp["total"])
	}
}

func TestGetWorkerByID(t *testing.T) {
	env := newTestEnv(t)

	env.registry.Register(worker.WorkerRegistration{
		ID: "w1", Endpoint: "http://a:8000", SchedulerURL: "http://a:9090", GPUCount: 4,
	})

	rec := env.request(http.MethodGet, "/api/v1/workers/w1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGetWorkerByID_NotFound(t *testing.T) {
	env := newTestEnv(t)

	rec := env.request(http.MethodGet, "/api/v1/workers/nonexistent", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestDeleteWorker(t *testing.T) {
	env := newTestEnv(t)

	env.registry.Register(worker.WorkerRegistration{
		ID: "w1", Endpoint: "http://a:8000", SchedulerURL: "http://a:9090",
	})

	rec := env.request(http.MethodDelete, "/api/v1/workers/w1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if _, ok := env.registry.Get("w1"); ok {
		t.Error("worker should be removed after DELETE")
	}
}

func TestStats(t *testing.T) {
	env := newTestEnv(t)

	env.registry.Register(worker.WorkerRegistration{
		ID: "w1", Endpoint: "http://a:8000", SchedulerURL: "http://a:9090", GPUCount: 4,
	})
	env.registry.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "model", 1)

	rec := env.request(http.MethodGet, "/api/v1/stats", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHealthEndpoint(t *testing.T) {
	env := newTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestReadyEndpoint(t *testing.T) {
	env := newTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthRequired(t *testing.T) {
	env := newTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", env.token))
	rec = httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 with correct token, got %d", rec.Code)
	}
}
