package settlement

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"openmodel/sp-state-agent/internal/metrics"
)

// TestPricerIsStale covers the audit MEDIUM fix: an auto-mode price that hasn't been
// refreshed within maxAge (or ever) is reported stale so the settler defers.
func TestPricerIsStale(t *testing.T) {
	manual := NewPricer(&Config{FILPriceUSD: "2.0", FILPriceSource: "manual"}, discardLogger())
	if manual.IsStale() {
		t.Error("manual price must never be stale")
	}

	auto := NewPricer(&Config{FILPriceUSD: "2.0", FILPriceSource: "auto", FILPriceRefreshSec: 300}, discardLogger())
	if !auto.IsStale() {
		t.Error("auto price with no successful fetch must be stale")
	}
	auto.mu.Lock()
	auto.lastUpdateTime = time.Now()
	auto.mu.Unlock()
	if auto.IsStale() {
		t.Error("auto price just updated must not be stale")
	}
	auto.mu.Lock()
	auto.lastUpdateTime = time.Now().Add(-time.Hour)
	auto.mu.Unlock()
	if !auto.IsStale() {
		t.Error("auto price an hour old (maxAge ~20min) must be stale")
	}
}

// TestPricerFILURLOverrideAndStaleDrill covers the price-staleness drill mechanism:
// NewPricer must honor the config FIL URL overrides (so the feed can be aimed at a stub),
// and once that stub starts FAILING every refresh, IsStale eventually fires — which is what
// the soak injection exercises (the old ipblock-a-CDN approach never made refresh fail).
func TestPricerFILURLOverrideAndStaleDrill(t *testing.T) {
	fail := false
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"filecoin":{"usd":5.5}}`))
	}))
	defer stub.Close()

	p := NewPricer(&Config{
		FILPriceUSD: "3.50", FILPriceSource: "auto", FILPriceRefreshSec: 300,
		FILPriceSources:      []string{"coingecko"},
		FILPriceCoinGeckoURL: stub.URL, // the override must flow through NewPricer
	}, discardLogger())
	if p.coinGeckoURL != stub.URL {
		t.Fatalf("config FIL URL override not applied: got %q", p.coinGeckoURL)
	}

	p.refresh() // healthy stub → price updates, not stale
	if p.GetFILPriceUSD().Cmp(big.NewFloat(5.5)) != 0 {
		t.Fatalf("expected 5.5 from stub, got %s", p.GetFILPriceUSD().Text('f', 4))
	}
	if p.IsStale() {
		t.Fatal("fresh successful fetch must not be stale")
	}
	// The live-price gauge must track the refresh (regression: it was defined+registered
	// but never Set, so openmodel_settlement_fil_price_usd read a dead 0 on dashboards).
	if got := testutil.ToFloat64(metrics.FILPriceUSD); got != 5.5 {
		t.Fatalf("fil_price_usd gauge must reflect the refreshed price, got %v", got)
	}

	// Stub now fails every refresh (the injected outage): lastUpdateTime stops advancing.
	fail = true
	p.refresh()
	// Simulate maxAge elapsing with only failing refreshes — IsStale must fire (the drill's
	// PASS condition). Reproduce by aging the last successful update past maxAge.
	p.mu.Lock()
	p.lastUpdateTime = time.Now().Add(-p.maxAge - time.Minute)
	p.mu.Unlock()
	if !p.IsStale() {
		t.Fatal("failing feed past maxAge must be reported stale")
	}
	// Round-3 regression: the GAUGE must flip on the next refresh, not wait for a
	// settle cycle (a stale window shorter than one settle interval was invisible).
	p.refresh() // still failing
	if got := testutil.ToFloat64(metrics.FILPriceStale); got != 1 {
		t.Fatalf("fil_price_stale gauge must go 1 on a refresh while stale, got %v", got)
	}

	// Restore → next successful refresh clears staleness (post-outage recovery).
	fail = false
	p.refresh()
	if p.IsStale() {
		t.Fatal("stale flag must clear after the feed recovers")
	}
	if got := testutil.ToFloat64(metrics.FILPriceStale); got != 0 {
		t.Fatalf("fil_price_stale gauge must clear on recovery, got %v", got)
	}
}

func newTestPricer(sources []string) *Pricer {
	cfg := &Config{
		FILPriceUSD:     "3.50",
		FILPriceSource:  "auto",
		FILPriceSources: sources,
	}
	return NewPricer(cfg, discardLogger())
}

// TestPricerInvalidInitialPrice: a bad fil_price_usd falls back to 3.50.
func TestPricerInvalidInitialPrice(t *testing.T) {
	for _, bad := range []string{"not-a-number", "-1", "0"} {
		p := NewPricer(&Config{FILPriceUSD: bad}, discardLogger())
		if p.GetFILPriceUSD().Cmp(big.NewFloat(3.50)) != 0 {
			t.Errorf("FILPriceUSD=%q: expected default 3.50, got %s", bad, p.GetFILPriceUSD().Text('f', 4))
		}
	}
}

// TestPricerManualModeNoFetch: in manual mode Start returns immediately and the
// configured price is used as-is (no network call).
func TestPricerManualModeNoFetch(t *testing.T) {
	p := NewPricer(&Config{FILPriceUSD: "4.25", FILPriceSource: "manual"}, discardLogger())
	// point sources at a server that would fail the test if hit
	p.coinGeckoURL = "http://127.0.0.1:0/should-not-be-called"
	p.Start(context.Background()) // must return immediately, not block or fetch
	if p.GetFILPriceUSD().Cmp(big.NewFloat(4.25)) != 0 {
		t.Errorf("manual mode: expected 4.25, got %s", p.GetFILPriceUSD().Text('f', 4))
	}
}

// TestPricerCoinGeckoSuccess: a healthy CoinGecko response updates the price.
func TestPricerCoinGeckoSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"filecoin":{"usd":4.2}}`))
	}))
	defer srv.Close()

	p := newTestPricer([]string{"coingecko"})
	p.coinGeckoURL = srv.URL
	p.refresh()

	if p.GetFILPriceUSD().Cmp(big.NewFloat(4.2)) != 0 {
		t.Errorf("expected 4.2 from CoinGecko, got %s", p.GetFILPriceUSD().Text('f', 4))
	}
}

// TestPricerFallbackToBinance: CoinGecko fails (500), Binance succeeds.
func TestPricerFallbackToBinance(t *testing.T) {
	cg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer cg.Close()
	bn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"price":"3.75"}`))
	}))
	defer bn.Close()

	p := newTestPricer([]string{"coingecko", "binance"})
	p.coinGeckoURL = cg.URL
	p.binanceURL = bn.URL
	p.refresh()

	if p.GetFILPriceUSD().Cmp(big.NewFloat(3.75)) != 0 {
		t.Errorf("expected 3.75 from Binance fallback, got %s", p.GetFILPriceUSD().Text('f', 4))
	}
}

// TestPricerAllSourcesFailKeepsLast: when every source fails, the LAST KNOWN price
// is retained — NOT reset to the config initial value. This is the explicit design
// decision ("don't fall back to manual config").
func TestPricerAllSourcesFailKeepsLast(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer down.Close()

	// config initial = 9.99, but operator dynamically set last-known to 12.0
	p := NewPricer(&Config{FILPriceUSD: "9.99", FILPriceSource: "auto",
		FILPriceSources: []string{"coingecko", "binance"}}, discardLogger())
	p.coinGeckoURL = down.URL
	p.binanceURL = down.URL
	p.SetFILPriceUSD(big.NewFloat(12.0))

	p.refresh() // both fail

	got := p.GetFILPriceUSD()
	if got.Cmp(big.NewFloat(12.0)) != 0 {
		t.Errorf("all-fail must keep last-known 12.0 (not config 9.99 or default 3.50), got %s",
			got.Text('f', 4))
	}
}

// TestPricerSetFILPriceUSD: dynamic update (the Admin PUT path) takes effect.
func TestPricerSetFILPriceUSD(t *testing.T) {
	p := NewPricer(&Config{FILPriceUSD: "3.50"}, discardLogger())
	p.SetFILPriceUSD(big.NewFloat(7.0))
	if p.GetFILPriceUSD().Cmp(big.NewFloat(7.0)) != 0 {
		t.Errorf("expected 7.0 after SetFILPriceUSD, got %s", p.GetFILPriceUSD().Text('f', 4))
	}
}

// TestPricerUnknownSource: an unknown source name is an error (covers the default
// branch of fetchFromSource); refresh with only unknown sources keeps last price.
func TestPricerUnknownSource(t *testing.T) {
	p := newTestPricer([]string{"nasdaq"})
	if _, err := p.fetchFromSource("nasdaq"); err == nil {
		t.Error("expected error for unknown source 'nasdaq'")
	}
	before := p.GetFILPriceUSD()
	p.refresh() // only unknown source -> all fail -> keep last
	if before.Cmp(p.GetFILPriceUSD()) != 0 {
		t.Error("price changed despite all sources failing")
	}
}

// TestPricerCoinGeckoBadResponses: status!=200, malformed JSON, and non-positive
// price are all treated as fetch failures.
func TestPricerCoinGeckoBadResponses(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"non-200": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) },
		"bad-json": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{not json`)) },
		"zero-price": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"filecoin":{"usd":0}}`)) },
	}
	for name, h := range cases {
		srv := httptest.NewServer(h)
		p := newTestPricer([]string{"coingecko"})
		p.coinGeckoURL = srv.URL
		if _, err := p.fetchCoinGecko(); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
		srv.Close()
	}
}
