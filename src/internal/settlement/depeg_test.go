package settlement

import (
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// C3 stablecoin depeg-protection tests.

// pricerWithStable builds a Pricer with a valid FIL price and white-box stablecoin state
// (same package), so the aggregator / balance tests can drive depeg deterministically
// without a live feed.
func pricerWithStable(t *testing.T, filUSD, stablePrice float64, depegged bool) *Pricer {
	t.Helper()
	cfg := &Config{FILPriceUSD: fmt.Sprintf("%.6f", filUSD), FILPriceSource: "manual", StablecoinSymbol: "USDC"}
	cfg.ApplyDefaults()
	p := NewPricer(cfg, discardLogger())
	p.stableSymbol = "USDC"
	p.stablePriceUSD = big.NewFloat(stablePrice)
	p.stableDepegged = depegged
	return p
}

// The pricer's real fetch → plausibility → depeg-latch path, driven by an httptest
// coingecko-shaped feed whose price we flip between polls.
func TestDepeg_PricerDetectsRejectsAndRepegs(t *testing.T) {
	var served atomic.Value // string body
	served.Store(`{"usd-coin":{"usd":1.0}}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, served.Load().(string))
	}))
	defer srv.Close()

	cfg := &Config{FILPriceUSD: "3.0", FILPriceSource: "manual", StablecoinSymbol: "USDC",
		StablecoinPriceSources: []string{"coingecko"}, StablecoinDepegBps: 200}
	cfg.ApplyDefaults()
	p := NewPricer(cfg, discardLogger())
	p.stableCoinGeckoURL = srv.URL

	// At peg: not depegged.
	p.refreshStable()
	if p.IsStablecoinDepegged("USDC") {
		t.Fatal("$1.00 must not be depegged")
	}

	// Within the 2% band ($0.99): accepted, repriced, NOT depegged.
	served.Store(`{"usd-coin":{"usd":0.99}}`)
	p.refreshStable()
	if p.IsStablecoinDepegged("USDC") {
		t.Error("$0.99 (1% off, inside 2% band) must not be depegged")
	}
	if got, _ := p.StablecoinPriceUSD("USDC").Float64(); got < 0.988 || got > 0.992 {
		t.Errorf("in-band price must update to ~0.99, got %v", got)
	}

	// Beyond the band ($0.85): depegged.
	served.Store(`{"usd-coin":{"usd":0.85}}`)
	p.refreshStable()
	if !p.IsStablecoinDepegged("USDC") {
		t.Error("$0.85 (15% off) must be depegged")
	}

	// Implausible feed value ($0.20 < 0.5 floor): rejected, last good ($0.85) kept, and
	// depeg latch unchanged (still depegged from the last real value).
	served.Store(`{"usd-coin":{"usd":0.20}}`)
	p.refreshStable()
	if got, _ := p.StablecoinPriceUSD("USDC").Float64(); got < 0.84 || got > 0.86 {
		t.Errorf("implausible value must be rejected, keeping last good ~0.85, got %v", got)
	}

	// Re-peg ($1.00): depeg latch clears.
	served.Store(`{"usd-coin":{"usd":1.0}}`)
	p.refreshStable()
	if p.IsStablecoinDepegged("USDC") {
		t.Error("re-pegged to $1.00 must clear the depeg latch")
	}

	// A non-monitored symbol is never depegged and prices at $1.
	if p.IsStablecoinDepegged("DAI") {
		t.Error("non-monitored token must never be depegged")
	}
	if got, _ := p.StablecoinPriceUSD("DAI").Float64(); got != 1.0 {
		t.Errorf("non-monitored token must price at $1, got %v", got)
	}
}

// A depegged stablecoin is skipped in settlement deduction — the cost falls through to
// the next priority token (FIL), rather than being collected in a token we distrust.
func TestDepeg_DeductionSkipsDepeggedStablecoin(t *testing.T) {
	agg := testAggregator(t)
	agg.SetPricer(pricerWithStable(t, 2.0, 0.85, true)) // USDC depegged

	// Wallet has plenty of USDC AND 10 FIL. Cost = $10 (10 tokens @ $1). FIL @ $2.
	balances := map[string]map[string]*big.Int{
		walletU: {usdcAddr: usdc(100), filAddr: fil(10)},
	}
	records := []RequestRecord{
		{Wallet: walletU, WorkerID: "w1", Model: "default", TotalTokens: 10, Status: 200},
	}
	items, unresolved := agg.Aggregate(records, big.NewFloat(2.0), balances)
	if len(unresolved) != 0 {
		t.Fatalf("expected 0 unresolved, got %d", len(unresolved))
	}
	for _, it := range items {
		if it.TokenAddr == common.HexToAddress(usdcAddr) {
			t.Fatalf("depegged USDC must NOT be collected, but an item used it: %s", it.Amount)
		}
	}
	// The whole $10 must come from FIL: 10/2 = 5 FIL.
	if len(items) != 1 || items[0].TokenAddr != common.HexToAddress(filAddr) {
		t.Fatalf("expected the cost to fall through to a single FIL item, got %+v", items)
	}
	if items[0].Amount.Cmp(fil(5)) != 0 {
		t.Errorf("expected 5 FIL ($10 at $2), got %s", items[0].Amount)
	}
}

// Within the peg band, an off-peg stablecoin is REPRICED: to collect $X the SP is paid
// X/price USDC (more when USDC < $1), so it is never under-paid.
func TestDepeg_DeductionRepricesInBand(t *testing.T) {
	agg := testAggregator(t)
	agg.SetPricer(pricerWithStable(t, 2.0, 0.99, false)) // USDC at $0.99, in band

	balances := map[string]map[string]*big.Int{walletU: {usdcAddr: usdc(100)}}
	records := []RequestRecord{
		{Wallet: walletU, WorkerID: "w1", Model: "default", TotalTokens: 10, Status: 200},
	}
	items, _ := agg.Aggregate(records, big.NewFloat(2.0), balances)
	if len(items) != 1 || items[0].TokenAddr != common.HexToAddress(usdcAddr) {
		t.Fatalf("expected a single USDC item, got %+v", items)
	}
	// $10 / $0.99 ≈ 10.101 USDC — strictly MORE than the naive $1-peg 10 USDC.
	if items[0].Amount.Cmp(usdc(10)) <= 0 {
		t.Errorf("off-peg USDC must be repriced UP (>10 USDC for $10), got %s", items[0].Amount)
	}
	if items[0].Amount.Cmp(usdc(11)) >= 0 {
		t.Errorf("repriced amount should be ~10.1 USDC, got %s", items[0].Amount)
	}
	// USD value still ~$10.
	if v, _ := items[0].AmountUSD.Float64(); v < 9.99 || v > 10.01 {
		t.Errorf("item USD value must stay ~$10, got %v", v)
	}
}

// Credit gate is symmetric with deduction: a depegged stablecoin is excluded from
// spendable credit (so a wallet can't keep spending against value we won't settle),
// while an in-band off-peg stablecoin is valued at its real price.
func TestDepeg_CreditGateSymmetric(t *testing.T) {
	cfg := &Config{SupportedTokens: []TokenConfig{
		{Symbol: "USDC", Address: usdcAddr, Decimals: 6},
		{Symbol: "FIL", Address: filAddr, Decimals: 18},
	}}

	seed := func(bc *BalanceCache) {
		bc.chainBalances[walletU] = map[string]*big.Int{usdcAddr: usdc(5), filAddr: fil(10)}
	}

	// Depegged: USDC ($5) excluded → only 10 FIL @ $2 = $20 counts.
	bcDe := NewBalanceCache(nil, cfg.SupportedTokens, pricerWithStable(t, 2.0, 0.85, true), 30, discardLogger())
	seed(bcDe)
	if got, _ := bcDe.availableUSD(walletU).Float64(); got < 19.99 || got > 20.01 {
		t.Errorf("depegged USDC must be excluded (expect ~$20 from FIL only), got %v", got)
	}

	// In-band off-peg ($0.98): USDC counts at 5*0.98=$4.90 → total $24.90.
	bcIn := NewBalanceCache(nil, cfg.SupportedTokens, pricerWithStable(t, 2.0, 0.98, false), 30, discardLogger())
	seed(bcIn)
	if got, _ := bcIn.availableUSD(walletU).Float64(); got < 24.89 || got > 24.91 {
		t.Errorf("in-band USDC must count at real price (expect ~$24.90), got %v", got)
	}
}

// Backward compatibility: with NO stablecoin price source, USDC stays pinned at $1 and is
// never depegged — deduction and credit behave exactly as pre-C3.
func TestDepeg_BackwardCompatNoSources(t *testing.T) {
	cfg := &Config{FILPriceUSD: "2.0", FILPriceSource: "manual"}
	cfg.ApplyDefaults() // sets StablecoinSymbol=USDC but NO sources → monitor never runs
	p := NewPricer(cfg, discardLogger())

	if p.IsStablecoinDepegged("USDC") {
		t.Fatal("no feed → USDC must never be depegged")
	}
	if got, _ := p.StablecoinPriceUSD("USDC").Float64(); got != 1.0 {
		t.Fatalf("no feed → USDC pinned at $1, got %v", got)
	}

	agg := testAggregator(t)
	agg.SetPricer(p)
	balances := map[string]map[string]*big.Int{walletU: {usdcAddr: usdc(100)}}
	records := []RequestRecord{{Wallet: walletU, WorkerID: "w1", Model: "default", TotalTokens: 10, Status: 200}}
	items, _ := agg.Aggregate(records, big.NewFloat(2.0), balances)
	if len(items) != 1 || items[0].Amount.Cmp(usdc(10)) != 0 {
		t.Fatalf("pinned USDC must bill exactly $10 = 10 USDC, got %+v", items)
	}
}
