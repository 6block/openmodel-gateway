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

// An image model has no context_window, so the token limits do not apply to it — but the
// image cap does. Before this branch existed the whole function returned early for such a
// model, leaving the billed image count completely unbounded.
func TestClampUsage_ImageModelClampsImageCount(t *testing.T) {
	g := &Gateway{
		logger: slog.Default(),
		settlementCfg: &settlement.Config{
			ModelCatalog: map[string]settlement.ModelInfo{
				// Note: no ContextWindow — an image model has none.
				"flux-schnell": {
					Modality: settlement.ModalityImage, PriceUnit: settlement.PriceUnitImage,
					UnitUSD: "2", MaxImagesPerGeneration: 4,
				},
			},
		},
	}

	// Within the cap → unchanged.
	ok := tokenUsage{AssetQty: 3}
	if got := g.clampUsage(ok, "flux-schnell", "r1", "w1"); got.AssetQty != 3 {
		t.Fatalf("a count within the cap must pass through, got %d", got.AssetQty)
	}

	// Above the cap → clamped, and the token fields stay untouched (they are meaningless
	// for this model and must not be invented).
	over := tokenUsage{AssetQty: 999}
	c := g.clampUsage(over, "flux-schnell", "r2", "w2")
	if c.AssetQty != 4 {
		t.Fatalf("image count must clamp to max_images_per_generation 4, got %d", c.AssetQty)
	}
	if c.PromptTokens != 0 || c.CompletionTokens != 0 {
		t.Errorf("clamping an image count must not touch token fields, got %+v", c)
	}

	// No cap declared (0) → unbounded by design, not clamped to zero.
	noCap := &Gateway{
		logger: slog.Default(),
		settlementCfg: &settlement.Config{ModelCatalog: map[string]settlement.ModelInfo{
			"flux-schnell": {Modality: settlement.ModalityImage, PriceUnit: settlement.PriceUnitImage, UnitUSD: "2"},
		}},
	}
	if got := noCap.clampUsage(tokenUsage{AssetQty: 100}, "flux-schnell", "r3", "w3"); got.AssetQty != 100 {
		t.Errorf("an uncapped image model must not clamp, got %d", got.AssetQty)
	}
}

// TestClampUsage_SpeechUnitsUseTheirOwnCaps: seconds are clamped by max_audio_sec and
// characters by max_chars. The image cap must never bound a speech quantity — with one
// shared cap a 4-image limit would have clamped every transcription to 4 seconds.
func TestClampUsage_SpeechUnitsUseTheirOwnCaps(t *testing.T) {
	g := &Gateway{
		logger: slog.Default(),
		settlementCfg: &settlement.Config{
			ModelCatalog: map[string]settlement.ModelInfo{
				"whisper": {Modality: settlement.ModalityASR, PriceUnit: settlement.PriceUnitAudioSecond,
					UnitUSD: "0.0001", MaxAudioSec: 600, MaxImagesPerGeneration: 4},
				"kokoro": {Modality: settlement.ModalityTTS, PriceUnit: settlement.PriceUnitCharacter,
					UnitUSD: "0.000015", MaxChars: 4000, MaxImagesPerGeneration: 4},
			},
		},
	}
	if got := g.clampUsage(tokenUsage{AssetQty: 120}, "whisper", "r", "w").AssetQty; got != 120 {
		t.Fatalf("120s is within max_audio_sec 600 and must not be clamped by the image cap, got %d", got)
	}
	if got := g.clampUsage(tokenUsage{AssetQty: 9999}, "whisper", "r", "w").AssetQty; got != 600 {
		t.Fatalf("seconds must clamp to max_audio_sec 600, got %d", got)
	}
	if got := g.clampUsage(tokenUsage{AssetQty: 3000}, "kokoro", "r", "w").AssetQty; got != 3000 {
		t.Fatalf("3000 chars is within max_chars, got %d", got)
	}
	if got := g.clampUsage(tokenUsage{AssetQty: 99999}, "kokoro", "r", "w").AssetQty; got != 4000 {
		t.Fatalf("characters must clamp to max_chars 4000, got %d", got)
	}
}
