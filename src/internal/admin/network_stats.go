package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"openmodel/sp-state-agent/internal/settlement"
)

// cumulativeStatsReader is the OPTIONAL schema-3 extension of ContractReader (the
// contract's all-time inference counters). Kept separate so existing mocks keep
// compiling and so a schema-2 deployment degrades to omitting the fields.
type cumulativeStatsReader interface {
	CumulativeStats(ctx context.Context) (requests, tokens uint64, err error)
}

// unitVolumeReader is the OPTIONAL schema-4 addition: the per-unit volume breakdown.
// A separate interface (not a method folded into cumulativeStatsReader) so a mock or a
// schema-3 deployment that cannot answer it still reports the counters it does have,
// instead of the whole block silently vanishing.
type unitVolumeReader interface {
	CumulativeVolumeByUnit(ctx context.Context) ([]uint64, error)
}

// NetworkStatsDeps supplies the two figures the settlement layer cannot derive on
// its own. Both are optional: a nil function omits its field rather than
// reporting zero, so "not wired up" never reads as "none".
type NetworkStatsDeps struct {
	// ProductionModels returns how many models are servable right now (a live
	// worker can be routed to for each), not how many the catalog lists.
	ProductionModels func() int
	// ActiveDevelopers returns the count of wallets meeting the activity floor and
	// when it was computed; a zero time means no successful computation yet.
	ActiveDevelopers func() (int, time.Time)
}

// SetNetworkStatsDeps wires the catalog and developer counters into the public
// stats endpoint.
func (sa *SettlementAPI) SetNetworkStatsDeps(d NetworkStatsDeps) { sa.statsDeps = d }

// handleNetworkStats serves the public, aggregate-only network metrics.
//
// Every field here is a COUNT. Nothing identifies a client, a request, or a key —
// the same boundary the other public routes hold: provider-side and network-wide
// transparency must not become client-activity transparency.
//
// The two on-chain figures are mirrored from the settlement contract for
// convenience; the contract remains the authority and is named in the response so
// a reader can verify them directly rather than trust this endpoint.
func (sa *SettlementAPI) handleNetworkStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErrorAdmin(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	out := map[string]interface{}{
		"as_of": time.Now().UTC().Format(time.RFC3339),
	}

	// --- Providers receiving revenue: non-zero settled earnings in any token ---
	providers := 0
	counted := false
	for _, evmAddr := range sa.effectiveSPMap() {
		earnings, _, _ := sa.readEarningsSplit(ctx, evmAddr)
		for _, v := range earnings {
			if v != "" && v != "0" && !isZeroDecimal(v) {
				providers++
				break
			}
		}
		counted = true
	}
	if counted {
		out["providers_with_revenue"] = providers
	}

	// --- Production models ---
	if sa.statsDeps.ProductionModels != nil {
		out["models_available"] = sa.statsDeps.ProductionModels()
	}

	// --- Active developers ---
	if sa.statsDeps.ActiveDevelopers != nil {
		n, at := sa.statsDeps.ActiveDevelopers()
		if !at.IsZero() {
			out["active_developers"] = n
			out["active_developers_as_of"] = at.UTC().Format(time.RFC3339)
			// Publish the counting rule alongside the number: without both the
			// window and the floor, the figure cannot be interpreted or reproduced.
			out["active_developers_window_days"] = int(settlement.ActiveWalletWindow.Hours() / 24)
			out["active_developers_min_requests"] = settlement.ActiveWalletMinRequests
		}
	}

	// --- On-chain volume counters (mirrored; contract is authoritative) ---
	onchain := map[string]interface{}{}
	if csr, ok := sa.contract.(cumulativeStatsReader); ok {
		if reqs, toks, err := csr.CumulativeStats(ctx); err == nil {
			onchain["cumulative_requests"] = reqs
			// On a v1.4 contract `toks` is the sum across every unit, so it is NOT a
			// token count; publishing it under the token name would mislabel it. Ask
			// for the per-unit breakdown when the deployment has one.
			if uvr, ok := sa.contract.(unitVolumeReader); ok {
				if byUnit, verr := uvr.CumulativeVolumeByUnit(ctx); verr == nil && len(byUnit) >= settlement.UnitCount {
					onchain["cumulative_volume"] = map[string]interface{}{
						"token":        byUnit[settlement.UnitToken],
						"image":        byUnit[settlement.UnitImage],
						"audio_second": byUnit[settlement.UnitAudioSecond],
						"character":    byUnit[settlement.UnitCharacter],
					}
				} else {
					onchain["cumulative_tokens"] = toks
				}
			} else {
				// Pre-v1.4: a single token-shaped column, correctly named.
				onchain["cumulative_tokens"] = toks
			}
		} else {
			sa.logger.Warn("cumulative stats unavailable", "error", err)
		}
	}
	if len(onchain) > 0 {
		onchain["note"] = "mirrored from the settlement contract; read it directly to verify"
		out["onchain"] = onchain
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(out)
}

// isZeroDecimal reports whether a decimal-string amount is zero ("0", "0.0",
// "0.000000000000000000"), so a provider credited nothing is not counted.
func isZeroDecimal(s string) bool {
	for _, c := range s {
		if c >= '1' && c <= '9' {
			return false
		}
	}
	return true
}

// jsonErrorAdmin mirrors the gateway's error shape for these public routes.
func jsonErrorAdmin(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{"message": msg, "type": "gateway_error"},
	})
}
