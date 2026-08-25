package gateway

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// The spot-check pool is self-registered workers only, so with few external
// workers a bare tick cadence concentrates every probe on the same machines.
// These pin the cooldown: one completed probe removes the worker from the
// candidate pool for SpotMinInterval, and the pool readmits it afterwards.
func TestProbe_SpotCheckCooldown(t *testing.T) {
	tok := "wtok-cool"
	srv := fakeWorkerServer(t, true, tok)
	defer srv.Close()
	g := newProbeTestGateway(t)
	registerFakeSP(t, g, "sp-cool", "big-model", srv.URL, tok)

	a := newAuditor(ProbeConfig{NumQuestions: 4, MinScore: 0.5, BanSeconds: 60},
		g, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if a.cfg.SpotMinInterval != 3*24*time.Hour {
		t.Fatalf("default cooldown must be 3 days, got %v", a.cfg.SpotMinInterval)
	}
	clock := time.Now()
	a.now = func() time.Time { return clock }

	if got := len(a.candidates()); got != 1 {
		t.Fatalf("fresh worker must be a candidate, got %d", got)
	}

	w, _ := g.registry.Get("sp-cool")
	a.probeWorker(context.Background(), *w)

	if got := len(a.candidates()); got != 0 {
		t.Fatalf("a just-probed worker must cool down, still %d candidate(s)", got)
	}

	clock = clock.Add(3*24*time.Hour - time.Minute)
	if got := len(a.candidates()); got != 0 {
		t.Fatalf("cooldown must hold until the full interval, got %d candidate(s)", got)
	}

	clock = clock.Add(2 * time.Minute)
	if got := len(a.candidates()); got != 1 {
		t.Fatalf("after the interval the worker must be eligible again, got %d", got)
	}
}

// An admission run is itself a full check: it must start the same cooldown so
// the worker is not spot-checked again minutes after being verified.
func TestProbe_AdmissionRunStartsCooldown(t *testing.T) {
	tok := "wtok-adm"
	srv := fakeWorkerServer(t, true, tok)
	defer srv.Close()
	g := newProbeTestGateway(t)
	registerFakeSP(t, g, "sp-adm", "big-model", srv.URL, tok)

	a := newAuditor(ProbeConfig{NumQuestions: 4, MinScore: 0.5, BanSeconds: 60, AdmissionGate: true},
		g, slog.New(slog.NewTextHandler(io.Discard, nil)))

	w, _ := g.registry.Get("sp-adm")
	a.verifyModel(context.Background(), *w, "big-model")

	if got, _ := g.registry.Get("sp-adm"); len(got.VerifiedModels) != 1 {
		t.Fatalf("admission should have verified the model, got %v", got.VerifiedModels)
	}
	if got := len(a.candidates()); got != 0 {
		t.Fatalf("a just-admitted worker must not be an immediate spot-check candidate, got %d", got)
	}
}

// A transport-failed probe is not a completed check and must NOT consume the
// cooldown — otherwise an unreachable worker escapes scrutiny for days.
func TestProbe_TransportErrorDoesNotStartCooldown(t *testing.T) {
	tok := "wtok-dead"
	srv := fakeWorkerServer(t, true, tok)
	g := newProbeTestGateway(t)
	registerFakeSP(t, g, "sp-dead", "big-model", srv.URL, tok)
	srv.Close() // endpoint now refuses connections

	a := newAuditor(ProbeConfig{NumQuestions: 4, MinScore: 0.5, BanSeconds: 60},
		g, slog.New(slog.NewTextHandler(io.Discard, nil)))

	w, _ := g.registry.Get("sp-dead")
	a.probeWorker(context.Background(), *w)

	if got := len(a.candidates()); got != 1 {
		t.Fatalf("an aborted probe must leave the worker eligible, got %d candidate(s)", got)
	}
}
