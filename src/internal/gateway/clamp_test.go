package gateway

import (
	"log/slog"
	"testing"

	"openmodel/sp-state-agent/internal/settlement"
)

func TestClampUsage(t *testing.T) {
	g := &Gateway{
		logger: slog.Default(),
		settlementCfg: &settlement.Config{
			ModelCatalog: map[string]settlement.ModelInfo{
				"default": {ContextWindow: 1000, MaxOutput: 100},
			},
		},
	}

	// Legit usage within the model's physical limits → unchanged.
	legit := tokenUsage{PromptTokens: 500, CompletionTokens: 50, CachedTokens: 100, TotalTokens: 550}
	if got := g.clampUsage(legit, "default", "r1", "w1"); got != legit {
		t.Fatalf("legit usage must pass through unchanged, got %+v", got)
	}

	// A misreporting worker inflating every field → clamped to physical bounds.
	liar := tokenUsage{PromptTokens: 999999, CompletionTokens: 999999, CachedTokens: 999999, TotalTokens: 9999999}
	c := g.clampUsage(liar, "default", "r2", "w2")
	if c.PromptTokens != 1000 {
		t.Fatalf("prompt must clamp to context_window 1000, got %d", c.PromptTokens)
	}
	if c.CompletionTokens != 100 {
		t.Fatalf("completion must clamp to max_output 100, got %d", c.CompletionTokens)
	}
	if c.CachedTokens != 1000 {
		t.Fatalf("cached must clamp to (clamped) prompt 1000, got %d", c.CachedTokens)
	}
	if c.TotalTokens != 1100 {
		t.Fatalf("total must clamp to context_window+max_output 1100, got %d", c.TotalTokens)
	}

	// Unknown model falls back to the "default" catalog entry.
	if c2 := g.clampUsage(liar, "some-other-model", "r3", "w3"); c2.PromptTokens != 1000 {
		t.Fatalf("unknown model should use default catalog bound, got prompt %d", c2.PromptTokens)
	}

	// No catalog configured → pass through unchanged (fail-open, don't break billing).
	g2 := &Gateway{logger: slog.Default(), settlementCfg: &settlement.Config{}}
	if got := g2.clampUsage(liar, "x", "r", "w"); got != liar {
		t.Fatalf("no catalog must pass through unchanged, got %+v", got)
	}
}
