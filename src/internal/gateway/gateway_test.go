package gateway

import "testing"

// The gateway's default max_tokens must actually REACH the worker. It used to be
// applied only to the balance reservation, so the worker fell back to its own
// 256 default and truncated the user's answer mid-sentence — while the catalog
// advertised 4096 and the config said 512. Three numbers, and the user
// experienced the one nobody had configured.
func TestDefaultMaxTokens_ReachesTheWorker(t *testing.T) {
	base := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	got := withDefaultMaxTokens(base, 2048)
	if v := extractInt(got, "max_tokens"); v != 2048 {
		t.Fatalf("default must be written into the upstream body, got %d", v)
	}
	// The rest of the request must survive the rewrite untouched.
	if extractModel(got) != "m" || !hasTopLevelKey(got, "messages") {
		t.Fatalf("rewrite damaged the request: %s", got)
	}

	// An explicit client value always wins — never silently overridden.
	explicit := []byte(`{"model":"m","max_tokens":16,"messages":[]}`)
	if v := extractInt(withDefaultMaxTokens(explicit, 2048), "max_tokens"); v != 16 {
		t.Fatalf("explicit client max_tokens must be preserved, got %d", v)
	}

	// A zero/absent default leaves the body alone rather than writing nonsense.
	if string(withDefaultMaxTokens(base, 0)) != string(base) {
		t.Fatal("a zero default must not modify the body")
	}
	// Malformed JSON must pass through rather than being dropped.
	bad := []byte(`{not json`)
	if string(withDefaultMaxTokens(bad, 2048)) != string(bad) {
		t.Fatal("unparseable bodies must pass through unchanged")
	}
}
