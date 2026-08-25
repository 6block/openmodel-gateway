package gateway

import (
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
)

// The "default" alias (and an empty/missing model) is rejected at the external
// entry with 400 — a client must always request an exact model id. A real loaded
// model still succeeds. The rejection lands AFTER auth (a bad key is still 401)
// and returns a message pointing to /v1/models.
func TestModelRequired_RejectsDefaultAndEmpty(t *testing.T) {
	// Worker loads "test-model"; balance is ample so only the model gate can 4xx.
	upSSE := sseServer(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n")
	defer upSSE.Close()
	srv, _, cleanup := newBillingGateway(t, big.NewInt(1_000_000_000_000), upSSE.URL)
	defer cleanup()

	chat := func(body string) (int, string) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	for _, tc := range []struct{ name, body string }{
		{"literal default", `{"model":"default","messages":[{"role":"user","content":"x"}]}`},
		{"empty model", `{"model":"","messages":[{"role":"user","content":"x"}]}`},
		{"missing model", `{"messages":[{"role":"user","content":"x"}]}`},
	} {
		code, body := chat(tc.body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400 (body %s)", tc.name, code, body)
			continue
		}
		if !strings.Contains(body, "/v1/models") {
			t.Errorf("%s: error should point to /v1/models, got %s", tc.name, body)
		}
	}

	// A real model still works.
	if code, body := chat(`{"model":"test-model","messages":[{"role":"user","content":"x"}],"max_tokens":4,"stream":true}`); code != http.StatusOK {
		t.Fatalf("exact model: got %d, want 200 (body %s)", code, body)
	}

	// Auth still precedes the model gate: a bad key with model "default" is 401.
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"default"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	resp, _ := http.DefaultClient.Do(req)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad key + default model: got %d, want 401", resp.StatusCode)
	}
}

// /v1/models must not advertise a synthetic "default"; the pricing/routing default
// fallback still exists internally but is never a requestable id.
func TestModelsListingHasNoDefault(t *testing.T) {
	upSSE := sseServer()
	defer upSSE.Close()
	srv, _, cleanup := newBillingGateway(t, big.NewInt(1_000_000_000_000), upSSE.URL)
	defer cleanup()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	sawReal := false
	for _, m := range out.Data {
		if m.ID == "default" {
			t.Error("/v1/models must not list 'default'")
		}
		if m.ID == "test-model" {
			sawReal = true
		}
	}
	if !sawReal {
		t.Errorf("expected the loaded 'test-model' in the listing: %+v", out.Data)
	}
}
