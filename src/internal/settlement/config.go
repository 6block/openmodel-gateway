package settlement

import (
	"fmt"
	"math/big"
)

type Config struct {
	Enabled            bool              `yaml:"enabled"`
	RPCURL             string            `yaml:"rpc_url"`
	// RPCURLs are additional FEVM RPC endpoints for failover (C2). rpc_url is tried
	// first (backward compat); on a persistent transport failure the contract client
	// rotates to the next healthy endpoint, so one flaky provider (a GLIF blip — the
	// 24h soak was bitten by exactly this) no longer stalls settlement/balance refresh.
	RPCURLs            []string          `yaml:"rpc_urls"`
	ChainID            int64             `yaml:"chain_id"`
	ContractAddress    string            `yaml:"contract_address"`
	OperatorPrivateKey string            `yaml:"operator_private_key"`
	IntervalMinutes    int               `yaml:"interval_minutes"`
	MaxBatchSize       int               `yaml:"max_batch_size"`
	ModelPricesUSD     map[string]string `yaml:"model_prices_usd"`
	FILPriceUSD        string            `yaml:"fil_price_usd"`
	FILPriceSource     string            `yaml:"fil_price_source"`
	FILPriceSources    []string          `yaml:"fil_price_sources"`
	FILPriceRefreshSec int               `yaml:"fil_price_refresh_sec"`
	// Optional overrides for the FIL price-feed URLs (symmetric with the stablecoin ones
	// below). Empty → the public CoinGecko / Binance endpoints. Set these to point the
	// "coingecko"/"binance" source at a self-hosted oracle — also how a controlled
	// price-staleness drill is run on a live node (aim them at a stub that can be made to
	// fail, so every refresh errors and IsStale eventually fires). Response shape must match
	// the provider (CoinGecko: {"filecoin":{"usd":N}}; Binance: {"price":"N"}).
	FILPriceCoinGeckoURL string `yaml:"fil_price_coingecko_url"`
	FILPriceBinanceURL   string `yaml:"fil_price_binance_url"`
	// Stablecoin depeg protection (C3). StablecoinSymbol names the USD-pegged token to
	// monitor (default "USDC"). When StablecoinPriceSources is non-empty its USD price is
	// fetched like FIL; if it drifts beyond StablecoinDepegBps from $1 the token is
	// treated as DEPEGGED — settlement stops collecting it and it stops counting toward
	// spendable credit (symmetrically), falling through to the next deduction_priority
	// token (e.g. FIL). WITHIN the band the real (slightly off-peg) price is used to value
	// it, so the SP is neither over- nor under-paid. Empty StablecoinPriceSources = the
	// token stays pinned at exactly $1 (the pre-C3 behavior; fully backward compatible).
	StablecoinSymbol          string   `yaml:"stablecoin_symbol"`
	StablecoinPriceSources    []string `yaml:"stablecoin_price_sources"`
	StablecoinDepegBps        int      `yaml:"stablecoin_depeg_bps"`
	StablecoinPriceRefreshSec int      `yaml:"stablecoin_price_refresh_sec"`
	// Optional overrides for the stablecoin price-feed URLs. Empty → the public
	// CoinGecko / Binance endpoints. Set these to point the "coingecko"/"binance" source
	// at a self-hosted price oracle or an alternate provider (also how a controlled depeg
	// drill is run on a live node). The response shape must match the provider being
	// overridden (CoinGecko: {"usd-coin":{"usd":N}}; Binance: {"price":"N"}).
	StablecoinCoinGeckoURL string `yaml:"stablecoin_coingecko_url"`
	StablecoinBinanceURL   string `yaml:"stablecoin_binance_url"`
	SupportedTokens    []TokenConfig     `yaml:"supported_tokens"`
	BalanceRefreshSec  int               `yaml:"balance_refresh_sec"`
	MinBalanceFIL      string            `yaml:"min_balance_fil"`
	MaxPendingSpend    string            `yaml:"max_pending_spend_fil"`
	DefaultMaxTokens   int               `yaml:"default_max_tokens"`
	OperatorMinBalance string            `yaml:"operator_min_balance_fil"`
	// ConfirmationDepth is how many blocks must build on top of a settlement tx's
	// block before it is treated as final (reorg safety, C2). The cursor only
	// advances after this depth, so a tx dropped by a reorg is re-submitted (on-chain
	// dedup makes that safe) rather than silently lost.
	// Pointer so we can tell "absent" (nil → default 5) from an explicit 0: depth 0
	// means "mined == final, no extra wait", required on chains that don't produce
	// blocks on their own (e.g. a local dev Hardhat node mining only on tx). A plain
	// int could not express this — an explicit 0 was wrongly overridden to the default.
	ConfirmationDepth *int `yaml:"confirmation_depth"`
	// ReconcileIntervalMinutes is how often the automated three-way billing
	// reconciliation runs (B4). 0 = default 30 min. ReconcileToleranceUSD is the
	// drift (USD) tolerated before a run is flagged; empty = 1 cent.
	ReconcileIntervalMinutes int    `yaml:"reconcile_interval_minutes"`
	ReconcileToleranceUSD    string `yaml:"reconcile_tolerance_usd"`
	SPAddressMap       map[string]string `yaml:"sp_address_map"`
	DeductionPriority  []string          `yaml:"deduction_priority"`
	// DebtSuspendUSD is the outstanding carried-debt threshold (USD) at which a wallet
	// is suspended from service (served 402 until the debt is collected). Empty/unset =
	// suspension disabled; "0" = suspend on any positive debt (D3).
	DebtSuspendUSD string `yaml:"debt_suspend_usd"`
	// ModelCatalog adds per-model input / cache-read pricing and display metadata
	// (context window, max output) on top of ModelPricesUSD. ModelPricesUSD stays
	// the OUTPUT (and base) price per 1M tokens; when a model also appears here,
	// billing splits cost into input / output / cache-read instead of a flat
	// total*price. Optional and fully backward compatible: models without a
	// catalog entry bill exactly as before. Also feeds the model-catalog API.
	ModelCatalog map[string]ModelInfo `yaml:"model_catalog"`
}

type TokenConfig struct {
	Symbol   string `yaml:"symbol"`
	Address  string `yaml:"address"`
	Decimals int    `yaml:"decimals"`
}

// ModelInfo is the per-model pricing detail and catalog metadata. Prices are USD
// per 1M tokens. InputUSD / CacheReadUSD supplement ModelPricesUSD (the output
// price); CacheReadUSD is the discounted rate for prompt tokens served from the
// prefix cache. ContextWindow / MaxOutput are display-only for the catalog.
type ModelInfo struct {
	InputUSD      string `yaml:"input"`
	CacheReadUSD  string `yaml:"cache_read"`
	ContextWindow int    `yaml:"context_window"`
	MaxOutput     int    `yaml:"max_output"`
}

func (c *Config) ApplyDefaults() {
	if c.IntervalMinutes == 0 {
		c.IntervalMinutes = 15
	}
	if c.MaxBatchSize == 0 {
		c.MaxBatchSize = 50
	}
	if c.BalanceRefreshSec == 0 {
		c.BalanceRefreshSec = 30
	}
	if c.FILPriceRefreshSec == 0 {
		c.FILPriceRefreshSec = 300
	}
	if c.DefaultMaxTokens == 0 {
		c.DefaultMaxTokens = 4096
	}
	if c.FILPriceSource == "" {
		c.FILPriceSource = "manual"
	}
	if len(c.FILPriceSources) == 0 {
		c.FILPriceSources = []string{"coingecko", "binance"}
	}
	if c.StablecoinSymbol == "" {
		c.StablecoinSymbol = "USDC"
	}
	if c.StablecoinDepegBps == 0 {
		// 200 bps = 2%. Beyond this a stablecoin is treated as depegged. USDC's worst
		// real depeg (Mar 2023 SVB) hit ~12%; normal noise is a few bps, so 2% flags a
		// genuine break without tripping on routine micro-fluctuation.
		c.StablecoinDepegBps = 200
	}
	if c.StablecoinPriceRefreshSec == 0 {
		c.StablecoinPriceRefreshSec = 300
	}
	if len(c.DeductionPriority) == 0 {
		c.DeductionPriority = []string{"USDC", "FIL"}
	}
	if c.OperatorMinBalance == "" {
		c.OperatorMinBalance = "0.1"
	}
	if c.ConfirmationDepth == nil {
		// FEVM finality: Filecoin reaches finality at ~900 epochs, but practical
		// reorgs are short. A small depth (a handful of tipsets) catches the common
		// case cheaply; operators on mainnet handling large sums may raise this.
		// Only applied when the key is ABSENT — an explicit `confirmation_depth: 0`
		// is honored as "no finality wait" (for non-self-mining dev chains).
		d := 5
		c.ConfirmationDepth = &d
	}
	if c.MinBalanceFIL == "" {
		c.MinBalanceFIL = "0.001"
	}
	if c.FILPriceUSD == "" {
		c.FILPriceUSD = "3.50"
	}
	if c.ModelPricesUSD == nil {
		c.ModelPricesUSD = map[string]string{"default": "0.20"}
	}
	if _, ok := c.ModelPricesUSD["default"]; !ok {
		c.ModelPricesUSD["default"] = "0.20"
	}
	if c.ModelCatalog == nil {
		c.ModelCatalog = map[string]ModelInfo{}
	}
	// No auto "default" entry: catalog pricing is opt-in per model. Models without
	// a catalog entry keep the existing flat total*price billing (backward compat);
	// only explicitly-configured models get the input/output/cache-read split.
}

func (c *Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.RPCURL == "" {
		return fmt.Errorf("settlement.rpc_url is required")
	}
	if c.ContractAddress == "" {
		return fmt.Errorf("settlement.contract_address is required")
	}
	if c.OperatorPrivateKey == "" {
		return fmt.Errorf("settlement.operator_private_key is required")
	}
	if c.ChainID == 0 {
		return fmt.Errorf("settlement.chain_id is required")
	}
	// Every deduction_priority symbol must name a supported_tokens entry,
	// otherwise a typo is silently ignored and that token never participates in
	// the deduction order (gap-hunt finding: knob parsed but not validated).
	supported := make(map[string]bool, len(c.SupportedTokens))
	for _, t := range c.SupportedTokens {
		supported[t.Symbol] = true
	}
	for _, sym := range c.DeductionPriority {
		if !supported[sym] {
			return fmt.Errorf("settlement.deduction_priority lists %q which is not in supported_tokens", sym)
		}
	}
	// stablecoin depeg band must be a sane basis-points value; a typo like 20000
	// (200%) would make depeg unreachable, silently disabling the protection.
	if c.StablecoinDepegBps < 0 || c.StablecoinDepegBps >= 10000 {
		return fmt.Errorf("settlement.stablecoin_depeg_bps = %d must be in [0, 10000)", c.StablecoinDepegBps)
	}
	// model_catalog prices, when set, must parse — otherwise a typo silently
	// disables the input/cache-read split for that model.
	for model, info := range c.ModelCatalog {
		for field, v := range map[string]string{"input": info.InputUSD, "cache_read": info.CacheReadUSD} {
			if v == "" {
				continue
			}
			if _, _, err := big.ParseFloat(v, 10, 128, big.ToNearestEven); err != nil {
				return fmt.Errorf("settlement.model_catalog[%q].%s = %q is not a valid number", model, field, v)
			}
		}
	}
	return nil
}
