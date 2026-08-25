package worker

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

// The admission gate's evidence MUST survive a gateway restart. Persistence uses
// persistedWorker (a separate struct from Worker.MarshalJSON), so a field added to
// Worker but not to persistedWorker silently vanishes on disk — the whole fleet
// then falls out of the gated routing pool on every restart until re-probed. A
// memory-only re-register test cannot catch this; the disk round-trip does.
func TestVerifiedSurvivesDiskRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workers.json")
	reg := NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)), path)
	reg.Register(WorkerRegistration{ID: "sp-a", Endpoint: "http://w:8000",
		SchedulerURL: "http://w:9090", GPUCount: 1, SelfRegistered: true})
	reg.SetVerified("sp-a", []string{"model-x", "model-y"})

	reg2 := NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)), path) // constructor should loadFromDisk
	w, ok := reg2.Get("sp-a")
	if !ok {
		t.Fatal("worker not loaded from disk")
	}
	if len(w.VerifiedModels) != 2 {
		t.Fatalf("verified models lost across disk round-trip: %+v", w.VerifiedModels)
	}
}
