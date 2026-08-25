package worker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A healthy worker must not report a zero date for optional timestamps. Before the
// custom MarshalJSON, `omitempty` on time.Time (a struct) silently did nothing and
// every response carried banned_until="0001-01-01T00:00:00Z" — which reads as
// "banned until year 1" to any consumer.
func TestWorkerJSON_OmitsZeroTimestamps(t *testing.T) {
	// A registered, healthy worker: the always-present timestamps are set (they carry
	// no omitempty by design), the optional ones are zero.
	now := time.Date(2026, 7, 27, 3, 38, 29, 0, time.UTC)
	w := Worker{ID: "sp-x", State: StateIdle, LoadedModel: "m", RegisteredAt: now, LastPollTime: now}
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if strings.Contains(body, "0001-01-01") {
		t.Fatalf("zero timestamp leaked into JSON: %s", body)
	}
	for _, k := range []string{"banned_until", "loading_since", "switching_since", "until_change_at"} {
		if strings.Contains(body, k) {
			t.Errorf("%q should be absent when zero: %s", k, body)
		}
	}
	// Non-optional fields must still be present.
	for _, k := range []string{`"id"`, `"state"`, `"loaded_model"`, `"registered_at"`} {
		if !strings.Contains(body, k) {
			t.Errorf("%q missing from JSON: %s", k, body)
		}
	}
}

func TestWorkerJSON_KeepsSetTimestamps(t *testing.T) {
	// Relative future time, truncated to whole seconds so the RFC3339 round-trip
	// is exact. (The first version hardcoded "today at 10:00" — which became a
	// PAST time the next day and IsBanned() correctly turned false: a date bomb.)
	until := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)
	w := Worker{ID: "sp-y", BannedUntil: until, BanReason: "probe score too low"}
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["banned_until"] != until.Format(time.RFC3339) {
		t.Fatalf("banned_until = %v, want %s", got["banned_until"], until.Format(time.RFC3339))
	}
	if got["ban_reason"] != "probe score too low" {
		t.Fatalf("ban_reason = %v", got["ban_reason"])
	}
	// A real ban must round-trip so consumers (and our own admin tooling) agree.
	var back Worker
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !back.BannedUntil.Equal(until) {
		t.Fatalf("round-trip lost the ban time: %v", back.BannedUntil)
	}
	if !back.IsBanned() {
		t.Fatal("round-tripped worker should still read as banned")
	}
}
