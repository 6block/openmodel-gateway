package settlement

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"sync"
	"time"

	"openmodel/sp-state-agent/internal/metrics"
)

type Pricer struct {
	mu             sync.RWMutex
	filPriceUSD    *big.Float
	sources        []string
	refreshSec     int
	mode           string
	httpClient     *http.Client
	logger         *slog.Logger
	lastUpdateTime time.Time
	maxAge         time.Duration // in auto mode, price older than this is "stale"
	// Source endpoints. Defaulted to the public APIs in NewPricer; overridable
	// in tests to point at a local httptest server.
	coinGeckoURL string
	binanceURL   string

	// Stablecoin depeg protection (C3). Guarded by mu. stableSymbol is the monitored
	// USD-pegged token; stablePriceUSD is its latest USD price (default 1.0 = pinned).
	// stableDepegged latches true while |price-1| exceeds stableDepegBps. When
	// stableSources is empty the price stays pinned at 1.0 and depeg is never triggered
	// (pre-C3 behavior). URLs overridable in tests.
	stableSymbol       string
	stableSources      []string
	stableDepegBps     int
	stableRefreshSec   int
	stablePriceUSD     *big.Float
	stableDepegged     bool
	stableLastUpdate   time.Time
	stableCoinGeckoURL string
	stableBinanceURL   string
}

func NewPricer(cfg *Config, logger *slog.Logger) *Pricer {
	initialPrice, _, err := big.ParseFloat(cfg.FILPriceUSD, 10, 128, big.ToNearestEven)
	if err != nil || initialPrice.Sign() <= 0 {
		initialPrice = big.NewFloat(3.50)
		logger.Warn("invalid or non-positive fil_price_usd, using default 3.50", "value", cfg.FILPriceUSD)
	}

	// In auto mode, treat a price older than ~4 refresh intervals (floor 15 min) as
	// stale: if every source has been failing for that long, settling at an arbitrary
	// old FIL price would mis-charge users (audit MEDIUM fix). The settler defers a
	// cycle while the price is stale instead of billing on a bad rate.
	maxAge := time.Duration(cfg.FILPriceRefreshSec) * 4 * time.Second
	if maxAge < 15*time.Minute {
		maxAge = 15 * time.Minute
	}

	return &Pricer{
		filPriceUSD:  initialPrice,
		sources:      cfg.FILPriceSources,
		refreshSec:   cfg.FILPriceRefreshSec,
		mode:         cfg.FILPriceSource,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		logger:       logger,
		maxAge:       maxAge,
		coinGeckoURL: firstNonEmpty(cfg.FILPriceCoinGeckoURL, "https://api.coingecko.com/api/v3/simple/price?ids=filecoin&vs_currencies=usd"),
		binanceURL:   firstNonEmpty(cfg.FILPriceBinanceURL, "https://api.binance.com/api/v3/ticker/price?symbol=FILUSDT"),

		stableSymbol:       cfg.StablecoinSymbol,
		stableSources:      cfg.StablecoinPriceSources,
		stableDepegBps:     cfg.StablecoinDepegBps,
		stableRefreshSec:   cfg.StablecoinPriceRefreshSec,
		stablePriceUSD:     big.NewFloat(1.0), // pinned until a source updates it
		stableCoinGeckoURL: firstNonEmpty(cfg.StablecoinCoinGeckoURL, "https://api.coingecko.com/api/v3/simple/price?ids=usd-coin&vs_currencies=usd"),
		stableBinanceURL:   firstNonEmpty(cfg.StablecoinBinanceURL, "https://api.binance.com/api/v3/ticker/price?symbol=USDCUSDT"),
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// IsStale reports whether the cached FIL price is too old to bill on. Manual mode is
// never stale (the operator sets the price intentionally). In auto mode it is stale
// if no source has succeeded within maxAge (or ever).
func (p *Pricer) IsStale() bool {
	if p.mode != "auto" {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.lastUpdateTime.IsZero() {
		return true
	}
	return time.Since(p.lastUpdateTime) > p.maxAge
}

func (p *Pricer) Start(ctx context.Context) {
	if p.mode != "auto" {
		p.logger.Info("FIL price source: manual", "price_usd", p.filPriceUSD.Text('f', 4))
		return
	}

	p.logger.Info("FIL price source: auto", "sources", p.sources, "refresh_sec", p.refreshSec)
	p.refresh()

	ticker := time.NewTicker(time.Duration(p.refreshSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.refresh()
		}
	}
}

func (p *Pricer) GetFILPriceUSD() *big.Float {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return new(big.Float).Copy(p.filPriceUSD)
}

func (p *Pricer) SetFILPriceUSD(price *big.Float) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.filPriceUSD = price
	p.lastUpdateTime = time.Now()
	p.logger.Info("FIL price updated manually", "price_usd", price.Text('f', 4))
}

func (p *Pricer) refresh() {
	// Keep the staleness gauge LIVE: it used to be set only inside the 20-min settle
	// cycle, so a stale window shorter than one settle interval was invisible on
	// /metrics (round-3 soak: 4 failed refreshes → IsStale true for ~5min → gauge
	// never flipped, drill read 0 throughout). Refresh cadence (fil_price_refresh_sec)
	// now bounds the gauge's lag; the settler still sets it at settle time too.
	defer func() {
		if p.IsStale() {
			metrics.FILPriceStale.Set(1)
		} else {
			metrics.FILPriceStale.Set(0)
		}
	}()
	for _, source := range p.sources {
		price, err := p.fetchFromSource(source)
		if err != nil {
			p.logger.Warn("price source failed", "source", source, "error", err)
			continue
		}
		p.mu.Lock()
		if p.isImplausiblePrice(price) {
			lastGood := p.filPriceUSD.Text('f', 4)
			p.mu.Unlock()
			// Rejecting it means no fresh price from this source. If EVERY source is
			// rejected, lastUpdateTime stops advancing → IsStale eventually fires →
			// settlement defers the cycle (never bills on a bad value). This guards the
			// case IsStale can't: a source that returns a plausible-looking but wrong
			// value (flash spike / upstream bug) rather than failing outright.
			p.logger.Error("price source returned an implausible value; rejecting it and keeping the last good price",
				"source", source, "fetched_usd", price.Text('f', 4), "last_good_usd", lastGood)
			continue
		}
		p.filPriceUSD = price
		p.lastUpdateTime = time.Now()
		p.mu.Unlock()
		// Export the live price gauge (mirrors the stablecoin path); previously defined +
		// registered but never Set, so openmodel_settlement_fil_price_usd read a dead 0.
		ff, _ := price.Float64()
		metrics.FILPriceUSD.Set(ff)
		p.logger.Info("FIL price updated", "source", source, "price_usd", price.Text('f', 4))
		return
	}
	p.logger.Error("all price sources failed, using last known price",
		"price_usd", p.filPriceUSD.Text('f', 4),
		"last_update", p.lastUpdateTime,
	)
}

// Plausibility bounds for an auto-fetched FIL price (USD). The deviation band catches
// flash anomalies / upstream bugs relative to the last good price; the absolute band
// guards the bootstrap case (no prior price). FIL's real daily volatility is well under
// 50%, so 0.5 rejects gross errors without tripping on normal moves.
const (
	maxPriceDeviationRatio = 0.5
	minPlausiblePriceUSD   = 0.01
	maxPlausiblePriceUSD   = 10000.0
)

// isImplausiblePrice reports whether a freshly fetched price should be rejected as a
// likely source error. MUST be called with p.mu held (reads filPriceUSD/lastUpdateTime).
func (p *Pricer) isImplausiblePrice(price *big.Float) bool {
	f, _ := price.Float64()
	if f < minPlausiblePriceUSD || f > maxPlausiblePriceUSD {
		return true
	}
	if p.lastUpdateTime.IsZero() {
		return false // no reference yet; absolute band above already applied
	}
	last, _ := p.filPriceUSD.Float64()
	if last <= 0 {
		return false
	}
	dev := (f - last) / last
	if dev < 0 {
		dev = -dev
	}
	return dev > maxPriceDeviationRatio
}

func (p *Pricer) fetchFromSource(source string) (*big.Float, error) {
	switch source {
	case "coingecko":
		return p.fetchCoinGecko()
	case "binance":
		return p.fetchBinance()
	default:
		return nil, fmt.Errorf("unknown price source: %s", source)
	}
}

func (p *Pricer) fetchCoinGecko() (*big.Float, error) {
	url := p.coinGeckoURL
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("coingecko request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("coingecko status %d", resp.StatusCode)
	}

	var result struct {
		Filecoin struct {
			USD float64 `json:"usd"`
		} `json:"filecoin"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("coingecko decode: %w", err)
	}
	if result.Filecoin.USD <= 0 {
		return nil, fmt.Errorf("coingecko returned invalid price: %f", result.Filecoin.USD)
	}
	return big.NewFloat(result.Filecoin.USD), nil
}

func (p *Pricer) fetchBinance() (*big.Float, error) {
	url := p.binanceURL
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("binance request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("binance status %d", resp.StatusCode)
	}

	var result struct {
		Price string `json:"price"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("binance decode: %w", err)
	}

	price, _, err := big.ParseFloat(result.Price, 10, 128, big.ToNearestEven)
	if err != nil || price.Sign() <= 0 {
		return nil, fmt.Errorf("binance invalid price: %s", result.Price)
	}
	return price, nil
}

// --- Stablecoin depeg protection (C3) ---

// stablecoin plausibility band: reject a fetched stablecoin price outside [0.5, 2.0] as
// a feed error (a real stablecoin, even severely depegged, stays within this; values
// beyond it mean a broken/misrouted feed, not a depeg). The DEPEG threshold
// (stableDepegBps) is a tighter, accepted-but-flagged band applied on top.
const (
	minPlausibleStableUSD = 0.5
	maxPlausibleStableUSD = 2.0
)

// StablecoinPriceUSD returns the USD price to value `symbol` at. For the monitored
// stablecoin it is the latest fetched price (default 1.0 when no source is configured);
// for any other non-FIL token it is 1.0 (pinned). FIL is NOT handled here (it uses the
// cycle FIL price snapshot). Safe for concurrent use.
func (p *Pricer) StablecoinPriceUSD(symbol string) *big.Float {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.stableSymbol != "" && symbol == p.stableSymbol {
		return new(big.Float).Copy(p.stablePriceUSD)
	}
	return big.NewFloat(1.0)
}

// IsStablecoinDepegged reports whether `symbol` is the monitored stablecoin AND it is
// currently outside its peg band. Non-monitored tokens are never depegged. Safe for
// concurrent use.
func (p *Pricer) IsStablecoinDepegged(symbol string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.stableSymbol != "" && symbol == p.stableSymbol && p.stableDepegged
}

// StartStablecoinMonitor polls the stablecoin price sources every stableRefreshSec and
// updates the price + depeg latch. It is independent of the FIL price mode (a manual FIL
// price does not disable stablecoin monitoring). With no sources configured it returns
// immediately, leaving the token pinned at $1 (pre-C3 behavior).
func (p *Pricer) StartStablecoinMonitor(ctx context.Context) {
	if len(p.stableSources) == 0 || p.stableSymbol == "" {
		return
	}
	refresh := p.stableRefreshSec
	if refresh <= 0 {
		refresh = 300
	}
	p.logger.Info("stablecoin depeg monitor started",
		"symbol", p.stableSymbol, "sources", p.stableSources, "depeg_bps", p.stableDepegBps, "refresh_sec", refresh)
	p.refreshStable()

	ticker := time.NewTicker(time.Duration(refresh) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.refreshStable()
		}
	}
}

// refreshStable fetches the monitored stablecoin's USD price from the first working
// source, rejects implausible feed values (keeps last good), and recomputes the depeg
// latch, publishing both to Prometheus. On a state change (peg↔depeg) it logs at WARN.
func (p *Pricer) refreshStable() {
	var fetched *big.Float
	for _, source := range p.stableSources {
		price, err := p.fetchStableFromSource(source)
		if err != nil {
			p.logger.Warn("stablecoin price source failed", "symbol", p.stableSymbol, "source", source, "error", err)
			continue
		}
		f, _ := price.Float64()
		if f < minPlausibleStableUSD || f > maxPlausibleStableUSD {
			p.logger.Error("stablecoin source returned an implausible value; rejecting it, keeping last good",
				"symbol", p.stableSymbol, "source", source, "fetched_usd", price.Text('f', 4))
			continue
		}
		fetched = price
		p.logger.Info("stablecoin price updated", "symbol", p.stableSymbol, "source", source, "price_usd", price.Text('f', 4))
		break
	}
	if fetched == nil {
		p.logger.Error("all stablecoin price sources failed, keeping last known price",
			"symbol", p.stableSymbol, "price_usd", p.stablePriceUSD.Text('f', 4))
		return
	}

	f, _ := fetched.Float64()
	dev := f - 1.0
	if dev < 0 {
		dev = -dev
	}
	depegged := dev*10000 > float64(p.stableDepegBps)

	p.mu.Lock()
	wasDepegged := p.stableDepegged
	p.stablePriceUSD = fetched
	p.stableDepegged = depegged
	p.stableLastUpdate = time.Now()
	p.mu.Unlock()

	metrics.StablecoinPriceUSD.WithLabelValues(p.stableSymbol).Set(f)
	if depegged {
		metrics.StablecoinDepegged.WithLabelValues(p.stableSymbol).Set(1)
	} else {
		metrics.StablecoinDepegged.WithLabelValues(p.stableSymbol).Set(0)
	}
	if depegged != wasDepegged {
		if depegged {
			p.logger.Warn("STABLECOIN DEPEGGED — excluding from settlement and spendable credit until it re-pegs",
				"symbol", p.stableSymbol, "price_usd", fetched.Text('f', 4), "depeg_bps", p.stableDepegBps)
		} else {
			p.logger.Warn("stablecoin re-pegged — resuming acceptance",
				"symbol", p.stableSymbol, "price_usd", fetched.Text('f', 4))
		}
	}
}

func (p *Pricer) fetchStableFromSource(source string) (*big.Float, error) {
	switch source {
	case "coingecko":
		return p.fetchStableCoinGecko()
	case "binance":
		return p.fetchStableBinance()
	default:
		return nil, fmt.Errorf("unknown stablecoin price source: %s", source)
	}
}

func (p *Pricer) fetchStableCoinGecko() (*big.Float, error) {
	req, err := http.NewRequest("GET", p.stableCoinGeckoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("coingecko request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("coingecko status %d", resp.StatusCode)
	}
	var result struct {
		USDCoin struct {
			USD float64 `json:"usd"`
		} `json:"usd-coin"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("coingecko decode: %w", err)
	}
	if result.USDCoin.USD <= 0 {
		return nil, fmt.Errorf("coingecko returned invalid stablecoin price: %f", result.USDCoin.USD)
	}
	return big.NewFloat(result.USDCoin.USD), nil
}

func (p *Pricer) fetchStableBinance() (*big.Float, error) {
	// USDCUSDT prices USDC in USDT (both USD-pegged); a good proxy for USDC/USD and, in
	// practice, the pair that actually moves when USDC breaks peg. Secondary to coingecko.
	req, err := http.NewRequest("GET", p.stableBinanceURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("binance request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("binance status %d", resp.StatusCode)
	}
	var result struct {
		Price string `json:"price"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("binance decode: %w", err)
	}
	price, _, err := big.ParseFloat(result.Price, 10, 128, big.ToNearestEven)
	if err != nil || price.Sign() <= 0 {
		return nil, fmt.Errorf("binance invalid stablecoin price: %s", result.Price)
	}
	return price, nil
}
