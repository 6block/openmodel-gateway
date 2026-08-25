package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/worker"
)

func TestHandleModels(t *testing.T) {
	reg := worker.NewRegistry(discardLog(), "")
	reg.Register(worker.WorkerRegistration{ID: "w1", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", GPUCount: 1})
	reg.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "Qwen/Qwen2.5-3B-Instruct", 1)
	gw := New(reg, config.GatewayConfig{}, discardLog())
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range body.Data {
		ids[m.ID] = true
	}
	if !ids["Qwen/Qwen2.5-3B-Instruct"] {
		t.Error("expected the worker's loaded model listed in /v1/models")
	}
	if ids["default"] {
		t.Error("'default' must NOT be advertised — clients request exact model ids now")
	}
}

func TestHandleUnsupportedEndpoint(t *testing.T) {
	gw := New(worker.NewRegistry(discardLog(), ""), config.GatewayConfig{}, discardLog())
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/embeddings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unsupported endpoint = %d, want 404", resp.StatusCode)
	}
}
