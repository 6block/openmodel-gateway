package gateway

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/worker"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestSessionAffinity_GetPutExpiry(t *testing.T) {
	s := newSessionAffinity(50 * time.Millisecond)
	if _, ok := s.get("k"); ok {
		t.Fatal("empty map should miss")
	}
	s.put("k", "w1")
	if wid, ok := s.get("k"); !ok || wid != "w1" {
		t.Fatalf("get after put = %q,%v; want w1,true", wid, ok)
	}
	time.Sleep(70 * time.Millisecond)
	if _, ok := s.get("k"); ok {
		t.Fatal("entry must expire after TTL")
	}
	// empty key / worker are no-ops.
	s.put("", "w")
	s.put("k2", "")
	if _, ok := s.get(""); ok {
		t.Fatal("empty key never maps")
	}
	if _, ok := s.get("k2"); ok {
		t.Fatal("empty worker never maps")
	}
}

func TestSessionKeyOf(t *testing.T) {
	// Explicit X-Session-Id header wins, but is namespaced by API key so the same
	// session id under two different keys does NOT share routing.
	hk := sessionKeyOf("api", "sess-123", []byte(`{}`))
	if len(hk) < 2 || hk[:2] != "h:" {
		t.Fatalf("header key = %q, want h:...", hk)
	}
	if sessionKeyOf("api", "sess-123", nil) != hk {
		t.Fatal("same api key + header must be stable")
	}
	if sessionKeyOf("other", "sess-123", nil) == hk {
		t.Fatal("different api key must change the header session key")
	}
	body := []byte(`{"messages":[{"role":"system","content":"You are X"},{"role":"user","content":"hi"}]}`)
	k1 := sessionKeyOf("api", "", body)
	if len(k1) < 3 || k1[:2] != "m:" {
		t.Fatalf("message key = %q, want m:...", k1)
	}
	// A later turn with the SAME first two messages maps to the SAME key (stickiness).
	body2 := []byte(`{"messages":[{"role":"system","content":"You are X"},{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"again"}]}`)
	if k2 := sessionKeyOf("api", "", body2); k2 != k1 {
		t.Fatalf("same conversation prefix → same key; got %q vs %q", k2, k1)
	}
	// Different API key → different session key.
	if k := sessionKeyOf("other", "", body); k == k1 {
		t.Fatal("api key must affect the session key")
	}
	// No messages → no session key (routing not made sticky).
	if k := sessionKeyOf("api", "", []byte(`{}`)); k != "" {
		t.Fatalf("empty body → empty key, got %q", k)
	}
}

func TestExtractUsageCachedTokens(t *testing.T) {
	u := extractUsage([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":64}}}`))
	if u.PromptTokens != 100 || u.CompletionTokens != 20 || u.TotalTokens != 120 || u.CachedTokens != 64 {
		t.Fatalf("usage = %+v", u)
	}
	// Missing prompt_tokens_details → cached 0 (backward compatible).
	if u2 := extractUsage([]byte(`{"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`)); u2.CachedTokens != 0 {
		t.Fatalf("cached without details = %d, want 0", u2.CachedTokens)
	}
}

func TestSelectWithAffinity(t *testing.T) {
	reg := worker.NewRegistry(quietLog(), "")
	reg.Register(worker.WorkerRegistration{ID: "w1", Endpoint: "http://x:1", SchedulerURL: "http://x:2", GPUCount: 1, SupportedModels: []string{"m"}})
	reg.Register(worker.WorkerRegistration{ID: "w2", Endpoint: "http://y:1", SchedulerURL: "http://y:2", GPUCount: 1, SupportedModels: []string{"m"}})
	reg.UpdateState("w1", "GPU_STATE_AVAILABLE", "running", 0, "m", 1)
	reg.UpdateState("w2", "GPU_STATE_AVAILABLE", "running", 0, "m", 1)
	g := New(reg, config.GatewayConfig{}, quietLog())

	first, err := g.selectWithAffinity("s1", "m")
	if err != nil {
		t.Fatal(err)
	}
	// Sticky: repeated selections for the same session hit the same worker.
	for i := 0; i < 10; i++ {
		w, err := g.selectWithAffinity("s1", "m")
		if err != nil || w.ID != first.ID {
			t.Fatalf("affinity not sticky: got %v err %v, want %s", w, err, first.ID)
		}
	}
	// Sticky worker goes mining → transparently falls back to the other worker.
	reg.UpdateState(first.ID, "GPU_STATE_WINDOW_POST", "paused", 0, "m", 1)
	w, err := g.selectWithAffinity("s1", "m")
	if err != nil {
		t.Fatal(err)
	}
	if w.ID == first.ID {
		t.Fatalf("must not stick to a mining worker")
	}
	// No session key → normal routing, no panic / no stickiness.
	if _, err := g.selectWithAffinity("", "m"); err != nil {
		t.Fatal(err)
	}
}
