package config

import "testing"

func TestValidateRejectsNegativeWorkerValues(t *testing.T) {
	base := func() *Config {
		return &Config{Mode: "dev", Workers: WorkerConfig{PollIntervalSec: 5, OfflineFailThreshold: 3}}
	}
	if err := validate(base()); err != nil {
		t.Fatalf("baseline config should validate, got %v", err)
	}

	neg := base()
	neg.Workers.PollIntervalSec = -1
	if validate(neg) == nil {
		t.Error("expected error for negative poll_interval_sec")
	}

	zero := base()
	zero.Workers.OfflineFailThreshold = 0
	if validate(zero) == nil {
		t.Error("expected error for offline_fail_threshold < 1")
	}
}
