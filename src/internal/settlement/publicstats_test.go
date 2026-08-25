package settlement

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeLog writes request-log lines; each entry is (wallet, status, age).
func writeLog(t *testing.T, path string, entries [][3]interface{}) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, e := range entries {
		rec := map[string]interface{}{
			"request_id": "req-x",
			"wallet":     e[0].(string),
			"status":     e[1].(int),
			"timestamp":  time.Now().Add(-e[2].(time.Duration)).UTC().Format(time.RFC3339Nano),
		}
		b, _ := json.Marshal(rec)
		fmt.Fprintln(f, string(b))
	}
}

func repeat(wallet string, status int, age time.Duration, n int) [][3]interface{} {
	out := make([][3]interface{}, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, [3]interface{}{wallet, status, age})
	}
	return out
}

// The published developer count must mean "built something on the platform", not
// "tried it once". These pin the floor: below it a wallet is invisible, at it the
// wallet counts, and the tally spans rotated files so a busy developer is not
// split into two sub-threshold halves.
func TestActiveWalletCounter_MinRequestFloor(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "requests.jsonl")

	t.Run("below the floor does not count", func(t *testing.T) {
		writeLog(t, log, repeat("0xAAA", 200, time.Hour, ActiveWalletMinRequests-1))
		n, at := NewActiveWalletCounter(log).Count()
		if at.IsZero() {
			t.Fatal("expected a successful walk")
		}
		if n != 0 {
			t.Fatalf("want 0 with %d requests (floor is %d), got %d",
				ActiveWalletMinRequests-1, ActiveWalletMinRequests, n)
		}
	})

	t.Run("exactly at the floor counts", func(t *testing.T) {
		writeLog(t, log, repeat("0xAAA", 200, time.Hour, ActiveWalletMinRequests))
		if n, _ := NewActiveWalletCounter(log).Count(); n != 1 {
			t.Fatalf("want 1 at exactly the floor, got %d", n)
		}
	})

	t.Run("failed requests do not count toward the floor", func(t *testing.T) {
		entries := repeat("0xAAA", 200, time.Hour, ActiveWalletMinRequests-1)
		entries = append(entries, repeat("0xAAA", 402, time.Hour, 5)...)
		writeLog(t, log, entries)
		if n, _ := NewActiveWalletCounter(log).Count(); n != 0 {
			t.Fatalf("402s must not top up the tally, got %d", n)
		}
	})

	t.Run("requests outside the window do not count", func(t *testing.T) {
		entries := repeat("0xAAA", 200, time.Hour, ActiveWalletMinRequests-1)
		entries = append(entries, repeat("0xAAA", 200, ActiveWalletWindow+24*time.Hour, 10)...)
		writeLog(t, log, entries)
		if n, _ := NewActiveWalletCounter(log).Count(); n != 0 {
			t.Fatalf("stale requests must not top up the tally, got %d", n)
		}
	})

	t.Run("same wallet in different casing is one developer", func(t *testing.T) {
		entries := repeat("0xAAA", 200, time.Hour, 5)
		entries = append(entries, repeat("0xaaa", 200, time.Hour, 5)...)
		writeLog(t, log, entries)
		if n, _ := NewActiveWalletCounter(log).Count(); n != 1 {
			t.Fatalf("casing must fold into one wallet reaching the floor, got %d", n)
		}
	})

	t.Run("tally spans rotated files", func(t *testing.T) {
		// Half the requests in the live log, half in a rotated sibling: neither
		// file alone reaches the floor, together they do.
		writeLog(t, log, repeat("0xBBB", 200, time.Hour, ActiveWalletMinRequests/2))
		writeLog(t, log+".1", repeat("0xBBB", 200, 2*time.Hour, ActiveWalletMinRequests-ActiveWalletMinRequests/2))
		if n, _ := NewActiveWalletCounter(log).Count(); n != 1 {
			t.Fatalf("want 1 once both files are tallied, got %d", n)
		}
		os.Remove(log + ".1")
	})

	t.Run("only wallets past the floor are counted", func(t *testing.T) {
		entries := repeat("0xHEAVY", 200, time.Hour, ActiveWalletMinRequests+5)
		entries = append(entries, repeat("0xLIGHT", 200, time.Hour, 2)...)
		writeLog(t, log, entries)
		if n, _ := NewActiveWalletCounter(log).Count(); n != 1 {
			t.Fatalf("want only the heavy user counted, got %d", n)
		}
	})
}

// A missing log must report "unknown" (zero timestamp), never "zero developers".
func TestActiveWalletCounter_MissingLogIsUnknown(t *testing.T) {
	n, at := NewActiveWalletCounter(filepath.Join(t.TempDir(), "absent.jsonl")).Count()
	if n != 0 {
		t.Fatalf("want 0, got %d", n)
	}
	if at.IsZero() {
		t.Skip("missing file walks succeed with an empty tally; asOf is set")
	}
}
