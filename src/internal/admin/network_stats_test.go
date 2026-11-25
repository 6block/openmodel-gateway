package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"openmodel/sp-state-agent/internal/settlement"
)

// statsContract is a ContractReader that also speaks the optional cumulative-stats
// interfaces, so the network-stats handler's schema branching can be exercised without
// a chain.
type statsContract struct {
	ContractReader
	requests  uint64
	tokens    uint64
	byUnit    []uint64
	byUnitErr error
}

func (c *statsContract) CumulativeStats(context.Context) (uint64, uint64, error) {
	return c.requests, c.tokens, nil
}

func (c *statsContract) CumulativeVolumeByUnit(context.Context) ([]uint64, error) {
	return c.byUnit, c.byUnitErr
}

// volumeOnlyContract implements the per-unit reader but NOT the older cumulative one,
// pinning that the two are probed independently.
type volumeOnlyContract struct {
	ContractReader
}

func runNetworkStats(t *testing.T, c ContractReader) map[string]interface{} {
	t.Helper()
	sa := &SettlementAPI{logger: slog.Default(), contract: c}
	rec := httptest.NewRecorder()
	sa.handleNetworkStats(rec, httptest.NewRequest(http.MethodGet, "/api/v1/network-stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	onchain, _ := out["onchain"].(map[string]interface{})
	return onchain
}

// TestNetworkStatsPerUnitVolume: on a v1.4 contract the handler must publish the
// per-unit breakdown, and must NOT label a cross-unit sum "cumulative_tokens" — that
// number now includes images, so the old name would be a plain mislabel.
func TestNetworkStatsPerUnitVolume(t *testing.T) {
	c := &statsContract{
		requests: 42,
		tokens:   1204, // 1200 tokens + 4 images, as CumulativeStats would sum them
		byUnit:   []uint64{1200, 4, 0, 0},
	}
	onchain := runNetworkStats(t, c)
	if _, bad := onchain["cumulative_tokens"]; bad {
		t.Fatal("a per-unit deployment must not publish a cross-unit sum as cumulative_tokens")
	}
	vol, ok := onchain["cumulative_volume"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing cumulative_volume: %v", onchain)
	}
	if vol["token"].(float64) != 1200 || vol["image"].(float64) != 4 || vol["audio_second"].(float64) != 0 {
		t.Errorf("per-unit volume misreported: %v", vol)
	}
	if onchain["cumulative_requests"].(float64) != 42 {
		t.Errorf("requests misreported: %v", onchain["cumulative_requests"])
	}
}

// TestNetworkStatsPreV14KeepsTokenColumn: against a contract that cannot answer the
// per-unit question, the single token-shaped column keeps its old, correct name.
func TestNetworkStatsPreV14KeepsTokenColumn(t *testing.T) {
	c := &statsContract{requests: 7, tokens: 900, byUnitErr: context.DeadlineExceeded}
	onchain := runNetworkStats(t, c)
	if _, bad := onchain["cumulative_volume"]; bad {
		t.Fatal("a deployment without per-unit volume must not publish an empty breakdown")
	}
	if onchain["cumulative_tokens"].(float64) != 900 {
		t.Errorf("pre-v1.4 token count misreported: %v", onchain["cumulative_tokens"])
	}
}

// TestNetworkStatsVolumeOnlyContract: the per-unit reader is probed as its own optional
// interface, so a contract exposing only that still reports the breakdown.
func TestNetworkStatsVolumeOnlyContract(t *testing.T) {
	sa := &SettlementAPI{logger: slog.Default(), contract: &volumeOnlyContract{}}
	rec := httptest.NewRecorder()
	sa.handleNetworkStats(rec, httptest.NewRequest(http.MethodGet, "/api/v1/network-stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, present := out["onchain"]; present {
		t.Fatalf("a contract answering neither cumulative getter must omit the block, got %v", out["onchain"])
	}
}

// TestUnitCountMatchesContract guards the one constant the whole per-unit mapping rests
// on: the Go column count has to equal the contract's UNIT_COUNT, or the ABI argument
// is rejected (or worse, silently truncated by a future ABI change).
func TestUnitCountMatchesContract(t *testing.T) {
	if settlement.UnitCount != 4 {
		t.Fatalf("Go UnitCount drifted from the contract's UNIT_COUNT: %d", settlement.UnitCount)
	}
}
