package worker

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// A worker that has NEVER been reachable registers as offline, so the
// state-change WARN can never fire for it. These pin the fallback: the failure
// must surface at WARN when the threshold is crossed, repeat on the pacing
// interval, and stay quiet in between (5s polls must not mean 5s warnings).
func TestPollFailureOfNeverOnlineWorkerIsWarned(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	registry := NewRegistry(logger, "")
	registry.Register(WorkerRegistration{
		ID: "w-never", Endpoint: "https://203.0.113.9:38443",
		SchedulerURL: "https://203.0.113.9:39443", GPUCount: 1,
	})

	threshold := 5
	poller := NewPoller(registry, time.Second, threshold, logger)

	warns := func() int { return strings.Count(buf.String(), "worker still unreachable") }

	for i := 0; i < threshold-1; i++ {
		poller.handlePollFailure("w-never", "scheduler", errors.New("remote error: tls: bad certificate"))
	}
	if warns() != 0 {
		t.Fatalf("below the threshold should stay quiet, got %d WARNs", warns())
	}

	poller.handlePollFailure("w-never", "scheduler", errors.New("remote error: tls: bad certificate"))
	if warns() != 1 {
		t.Fatalf("crossing the threshold must WARN once, got %d\nlog: %s", warns(), buf.String())
	}
	if !strings.Contains(buf.String(), "bad certificate") {
		t.Fatalf("the WARN must carry the underlying error, log: %s", buf.String())
	}

	// Keep failing up to just before the pacing point: no extra noise.
	for i := threshold + 1; i < unreachableLogEvery; i++ {
		poller.handlePollFailure("w-never", "scheduler", errors.New("dial timeout"))
	}
	if warns() != 1 {
		t.Fatalf("between threshold and pacing interval should stay quiet, got %d WARNs", warns())
	}

	// The pacing point itself speaks again.
	poller.handlePollFailure("w-never", "scheduler", errors.New("dial timeout"))
	if warns() != 2 {
		t.Fatalf("want a paced repeat at %d consecutive failures, got %d WARNs", unreachableLogEvery, warns())
	}
}

// The online→offline transition keeps its original single WARN ("marked
// offline"), not the unreachable fallback — the two paths must not double-log.
func TestPollFailureOfOnlineWorkerWarnsOnceViaStateChange(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	registry := NewRegistry(logger, "")
	registry.Register(WorkerRegistration{
		ID: "w-live", Endpoint: "http://w:8000", SchedulerURL: "http://w:9090", GPUCount: 1,
	})
	registry.UpdateState("w-live", "GPU_STATE_AVAILABLE", "running", 0, "m", 1)

	threshold := 5
	poller := NewPoller(registry, time.Second, threshold, logger)
	for i := 0; i < threshold; i++ {
		poller.handlePollFailure("w-live", "scheduler", errors.New("dial timeout"))
	}
	if n := strings.Count(buf.String(), "worker marked offline"); n != 1 {
		t.Fatalf("want exactly one marked-offline WARN, got %d\nlog: %s", n, buf.String())
	}
	if n := strings.Count(buf.String(), "worker still unreachable"); n != 0 {
		t.Fatalf("the transition itself must not also log unreachable, got %d", n)
	}
}
