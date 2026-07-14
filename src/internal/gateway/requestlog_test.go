package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRequestLoggerWritesJSONL(t *testing.T) {
	p := filepath.Join(t.TempDir(), "requests.jsonl")
	rl := NewRequestLogger(p, discardLog())
	rl.Log(RequestRecord{RequestID: "r1", Wallet: "0xabc", Model: "default", Status: 200, TotalTokens: 7})
	rl.Close()

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var rec RequestRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatalf("line is not valid JSON: %v", err)
	}
	if rec.RequestID != "r1" || rec.Wallet != "0xabc" || rec.TotalTokens != 7 {
		t.Errorf("round-trip mismatch: %+v", rec)
	}
}

func TestRequestLoggerNilIsNoop(t *testing.T) {
	rl := NewRequestLogger("", discardLog()) // empty path → nil
	if rl != nil {
		t.Fatal("empty path should yield a nil logger")
	}
	rl.Log(RequestRecord{RequestID: "x"}) // must not panic
	if err := rl.Close(); err != nil {
		t.Errorf("Close on nil should be nil, got %v", err)
	}
}

func TestRequestLoggerAutoRotates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.jsonl")
	rl := NewRequestLogger(p, discardLog())

	rl.Log(RequestRecord{RequestID: "r1"})
	// Size the threshold off one real record so exactly the NEXT write trips rotation.
	rl.maxSize = rl.currentSize + 10
	rl.Log(RequestRecord{RequestID: "r2"}) // crosses threshold → rotate (r1+r2 → .1)
	rl.Log(RequestRecord{RequestID: "r3"}) // lands in the fresh file
	rl.Close()

	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatalf(".1 backup should exist after auto-rotation: %v", err)
	}
	backup, _ := os.ReadFile(p + ".1")
	if !strings.Contains(string(backup), `"r1"`) {
		t.Error("backup should hold the pre-rotation records")
	}
	cur, _ := os.ReadFile(p)
	if strings.Contains(string(cur), `"r1"`) {
		t.Error("rotated content leaked into the new file")
	}
	if !strings.Contains(string(cur), `"r3"`) {
		t.Error("new file missing the post-rotation record")
	}
}

func TestRequestLoggerRotateResetsSize(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.jsonl")
	rl := NewRequestLogger(p, discardLog())
	rl.Log(RequestRecord{RequestID: "before"})
	rl.rotate()
	if rl.currentSize != 0 {
		t.Errorf("currentSize after rotate = %d, want 0", rl.currentSize)
	}
	rl.Close()
}

func TestRequestLoggerConcurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.jsonl")
	rl := NewRequestLogger(p, discardLog())
	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			rl.Log(RequestRecord{RequestID: fmt.Sprintf("r%d", i)})
		}(i)
	}
	wg.Wait()
	rl.Close()

	data, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != n {
		t.Errorf("expected %d JSONL lines from concurrent writes, got %d", n, len(lines))
	}
	for _, ln := range lines { // every line must be valid JSON (no interleaving)
		var rec RequestRecord
		if json.Unmarshal([]byte(ln), &rec) != nil {
			t.Fatalf("corrupted (interleaved) line: %q", ln)
		}
	}
}

// TestRequestLog_KeepsConfiguredBackups: rotation keeps exactly maxBackups numbered
// backups and discards the oldest — the retention that prevents settlement loss.
func TestRequestLog_KeepsConfiguredBackups(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "req.jsonl")
	rl := NewRequestLogger(p, discardLog())
	rl.maxSize = 300 // tiny → rotate frequently
	rl.maxBackups = 3
	for i := 0; i < 800; i++ {
		rl.Log(RequestRecord{RequestID: fmt.Sprintf("r%d", i), Status: 200, TotalTokens: 1})
	}
	rl.Close()
	for _, i := range []int{1, 2, 3} {
		if _, err := os.Stat(fmt.Sprintf("%s.%d", p, i)); err != nil {
			t.Errorf("backup .%d should exist: %v", i, err)
		}
	}
	if _, err := os.Stat(p + ".4"); !os.IsNotExist(err) {
		t.Errorf("backup .4 must NOT exist (only 3 kept)")
	}
}

// TestShortSessionHash: the X-Session-Key fingerprint isolates by API key — same
// (key, session id) is stable, different keys with the same id differ.
func TestShortSessionHash(t *testing.T) {
	a := shortSessionHash(sessionKeyOf("userA", "iso", nil))
	if a == "" || a != shortSessionHash(sessionKeyOf("userA", "iso", nil)) {
		t.Fatal("same inputs must yield the same fingerprint")
	}
	if b := shortSessionHash(sessionKeyOf("userB", "iso", nil)); a == b {
		t.Fatal("different api keys with same session id must isolate")
	}
	if shortSessionHash("") != "" {
		t.Fatal("empty key → empty fingerprint")
	}
}
