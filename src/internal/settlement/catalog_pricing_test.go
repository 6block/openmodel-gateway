package settlement

import (
	"io"
	"log/slog"
	"math"
	"testing"
)

func quietAggLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRecordCostSplitWithCatalog: a model with a catalog entry bills prompt
// (split into cached/non-cached) and completion at the configured input /
// cache-read / output rates.
func TestRecordCostSplitWithCatalog(t *testing.T) {
	cfg := &Config{
		ModelPricesUSD: map[string]string{"default": "0.60"}, // output: $0.60 per 1M
		ModelCatalog: map[string]ModelInfo{
			"default": {InputUSD: "0.20", CacheReadUSD: "0.05", ContextWindow: 32768, MaxOutput: 4096},
		},
		SupportedTokens: []TokenConfig{{Symbol: "FIL", Address: "0x0000000000000000000000000000000000000000", Decimals: 18}},
	}
	cfg.ApplyDefaults()
	a := NewAggregator(cfg, map[string]string{}, quietAggLogger())

	rec := RequestRecord{Model: "default", PromptTokens: 1000, CachedTokens: 400, CompletionTokens: 500, TotalTokens: 1500}
	got, _ := a.recordCostUSD(rec).Float64()
	// (1000-400)*0.20 + 400*0.05 + 500*0.60, per 1M tokens
	// = (120 + 20 + 300) / 1e6 = 0.00044
	if math.Abs(got-0.00044) > 1e-12 {
		t.Fatalf("split cost = %v, want 0.00044", got)
	}
}

// TestRecordCostFlatWithoutCatalog: a model with no catalog entry keeps the old
// flat total*output billing — cached tokens are not discounted (backward compat).
func TestRecordCostFlatWithoutCatalog(t *testing.T) {
	cfg := &Config{
		ModelPricesUSD:  map[string]string{"default": "1000000"}, // $1 per token
		SupportedTokens: []TokenConfig{{Symbol: "FIL", Address: "0x0000000000000000000000000000000000000000", Decimals: 18}},
	}
	cfg.ApplyDefaults() // ModelCatalog stays empty → flat pricing
	a := NewAggregator(cfg, map[string]string{}, quietAggLogger())

	rec := RequestRecord{Model: "default", PromptTokens: 5, CompletionTokens: 5, CachedTokens: 2, TotalTokens: 10}
	got, _ := a.recordCostUSD(rec).Float64()
	if math.Abs(got-10.0) > 1e-9 { // 10 total tokens * $1, cached ignored in flat mode
		t.Fatalf("flat cost = %v, want 10", got)
	}
}

// TestRecordCostNoCacheReadRate: catalog has input but no cache_read → cached
// tokens fall back to the input rate (no free cache, no crash).
func TestRecordCostNoCacheReadRate(t *testing.T) {
	cfg := &Config{
		ModelPricesUSD:  map[string]string{"default": "0.60"},
		ModelCatalog:    map[string]ModelInfo{"default": {InputUSD: "0.20"}},
		SupportedTokens: []TokenConfig{{Symbol: "FIL", Address: "0x0000000000000000000000000000000000000000", Decimals: 18}},
	}
	cfg.ApplyDefaults()
	a := NewAggregator(cfg, map[string]string{}, quietAggLogger())

	rec := RequestRecord{Model: "default", PromptTokens: 1000, CachedTokens: 400, CompletionTokens: 0, TotalTokens: 1000}
	got, _ := a.recordCostUSD(rec).Float64()
	// all 1000 prompt at input 0.20 (cached billed at input rate too) = 200/1e6 = 0.0002
	if math.Abs(got-0.0002) > 1e-12 {
		t.Fatalf("no-cache-read cost = %v, want 0.0002", got)
	}
}
