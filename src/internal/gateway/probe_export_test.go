package gateway

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"testing"
)

// TestExportProbeQuestions dumps the gateway's OWN generated items in the format
// probe/run_probe.py consumes, so a model can be scored against exactly the
// questions the admission gate asks. Skipped unless EXPORT_PROBE_N is set.
//
// This exists because the floors are per-model thresholds on THIS generator's
// output: measuring a model on the separate offline item bank produces numbers
// that do not transfer (different questions, different scale), and setting a
// floor from the wrong instrument is what made an honest 1.5B look like a fraud.
//
//	EXPORT_PROBE_N=60 go test ./internal/gateway -run TestExportProbeQuestions > items.jsonl
func TestExportProbeQuestions(t *testing.T) {
	nStr := os.Getenv("EXPORT_PROBE_N")
	if nStr == "" {
		t.Skip("set EXPORT_PROBE_N to export")
	}
	n, err := strconv.Atoi(nStr)
	if err != nil || n <= 0 {
		t.Fatalf("EXPORT_PROBE_N must be a positive integer, got %q", nStr)
	}

	au := newAuditor(ProbeConfig{Enabled: true, IntervalSec: 1, NumQuestions: n}, nil,
		slog.New(slog.NewTextHandler(discardWriter{}, nil)))

	f, err := os.Create(os.Getenv("EXPORT_PROBE_FILE"))
	if err != nil {
		t.Fatalf("set EXPORT_PROBE_FILE to a writable path: %v", err)
	}
	defer f.Close()

	for i, q := range au.generate(n) {
		typ := "string"
		if q.numeric {
			typ = "numeric"
		}
		line, _ := json.Marshal(map[string]any{
			"id":     fmt.Sprintf("g%02d", i+1),
			"type":   typ,
			"answer": q.answer,
			"prompt": q.prompt,
		})
		fmt.Fprintln(f, string(line))
	}
	t.Logf("exported %d items", n)
}
