package settlement

import (
	"fmt"
	"math/big"
)

type Config struct {
	Enabled bool   `yaml:"enabled"`
	RPCURL  string `yaml:"rpc_url"`
	// RPCURLs are additional FEVM RPC endpoints for failover (C2). rpc_url is tried
	// first (backward compat); on a persistent transport failure the contract client
	// rotates to the next healthy endpoint, so one flaky provider (a GLIF blip — the
	// 24h soak was bitten by exactly this) no longer stalls settlement/balance refresh.
	RPCURLs         []string `yaml:"rpc_urls"`
	ChainID         int64    `yaml:"chain_id"`
	ContractAddress string   `yaml:"contract_address"`
	// ContractSchema selects the contract ABI generation: 2 = v1.2 and earlier
	// (the default when unset — every existing deployment, mainnet included), 3 =
	// v1.3 (submitSettlement carries per-item request/token stats), 4 = v1.4
	// (per-item per-unit volume, so a network serving tokens and images reports each
	// in its own counter). Schema 3 and 4 are verified against the live contract at
	// startup (SCHEMA_VERSION()) so a mismatch dies loudly instead of at settle time.
	ContractSchema     int               `yaml:"contract_schema"`
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
	StablecoinCoinGeckoURL string        `yaml:"stablecoin_coingecko_url"`
	StablecoinBinanceURL   string        `yaml:"stablecoin_binance_url"`
	SupportedTokens        []TokenConfig `yaml:"supported_tokens"`
	BalanceRefreshSec      int           `yaml:"balance_refresh_sec"`
	MinBalanceFIL          string        `yaml:"min_balance_fil"`
	MaxPendingSpend        string        `yaml:"max_pending_spend_fil"`
	DefaultMaxTokens       int           `yaml:"default_max_tokens"`
	OperatorMinBalance     string        `yaml:"operator_min_balance_fil"`
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
	ReconcileIntervalMinutes int               `yaml:"reconcile_interval_minutes"`
	ReconcileToleranceUSD    string            `yaml:"reconcile_tolerance_usd"`
	SPAddressMap             map[string]string `yaml:"sp_address_map"`
	DeductionPriority        []string          `yaml:"deduction_priority"`
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
//
// Modality and PriceUnit describe what kind of model this is and what the price
// is per. Both default to the text-model values ("text" / "token") so every
// existing catalog entry keeps its meaning without edits. Image-understanding
// models (Modality "vision") still bill per token: their image input is
// tokenized into visual prompt tokens by the worker and reported in the same
// prompt_tokens field, so the whole token pipeline applies unchanged.
type ModelInfo struct {
	InputUSD      string `yaml:"input"`
	CacheReadUSD  string `yaml:"cache_read"`
	ContextWindow int    `yaml:"context_window"`
	MaxOutput     int    `yaml:"max_output"`
	// UnitUSD is the price of ONE unit for a model billed in a non-token unit: one
	// generated image, one second of audio. It is a separate field from InputUSD on
	// purpose. InputUSD is "USD per 1M tokens" and is divided by 1e6 when parsed;
	// reusing it here would mean an image price had to be written as "USD per 1,000,000
	// images", and an operator writing the intuitive "2" for $2/image would underprice
	// a thousand-fold. Required for a non-token unit, ignored for a token one.
	UnitUSD string `yaml:"unit_usd"`
	// Modality: "text" (default), "vision" (image understanding — chat with image
	// parts), "image" (image generation), "asr" (speech-to-text), "tts"
	// (text-to-speech). Grouping/display only; routing is by model id.
	Modality string `yaml:"modality"`
	// PriceUnit: "token" (default; prices are per 1M tokens), "image" (per
	// generated image) or "audio_second" (per second of audio). A model is billed in
	// exactly one unit, and this field — configuration — is what decides it.
	PriceUnit PriceUnit `yaml:"price_unit"`
	// MaxImagesPerRequest caps how many image parts a single chat request to a
	// vision model may carry (0 = worker default). Display-only in the catalog
	// today; a per-request clamp is the natural next consumer.
	MaxImagesPerRequest int `yaml:"max_images_per_request"`
	// MaxImagesPerGeneration caps how many images one image-generation request may
	// ask for (0 = no declared cap). Named apart from MaxImagesPerRequest, which
	// bounds image INPUT to a vision model; this bounds image OUTPUT from a
	// generative one. Both are display-and-clamp values, not routing keys.
	MaxImagesPerGeneration int `yaml:"max_images_per_generation"`
	// MaxAudioSec caps the input audio one transcription request may carry, in whole
	// seconds as billed (0 = unset). MaxChars caps the input text of one speech
	// request, in Unicode code points (0 = unset). Each clamps its own unit only.
	MaxAudioSec int `yaml:"max_audio_sec"`
	MaxChars    int `yaml:"max_chars"`
}

// Recognised ModelInfo.Modality values.
const (
	ModalityText   = "text"
	ModalityVision = "vision"
	ModalityImage  = "image"
	ModalityASR    = "asr"
	ModalityTTS    = "tts"
)

// Recognised ModelInfo.PriceUnit values (see unit.go for the PriceUnit type).
const (
	PriceUnitToken       PriceUnit = "token"
	PriceUnitImage       PriceUnit = "image"
	PriceUnitAudioSecond PriceUnit = "audio_second"
	// PriceUnitCharacter bills a text-to-speech request by the characters it was asked
	// to speak (Unicode code points of the input), as OpenAI tts-1, ElevenLabs and
	// Polly do. The gateway counts them itself from the request, so the quantity is
	// first-hand and the client can know the price before sending.
	PriceUnitCharacter PriceUnit = "character"
)

// Volume unit indices, matching the contract's UNIT_* constants (schema 4). These are
// the ABI: they are what a caller indexes CumulativeVolumeByUnit by, and what goes on
// the wire in submitSettlement's units column. Duplicated from the contract rather than
// derived, because a mismatch must break loudly in a test rather than silently agree.
const (
	UnitToken = iota
	UnitImage
	UnitAudioSecond
	UnitCharacter
	UnitCount
)

// UnitIndex maps a PriceUnit to the contract's volume-unit index. An unrecognised unit
// maps to -1 so a caller cannot silently file it under tokens.
func UnitIndex(u PriceUnit) int {
	switch EffectiveUnit(u) {
	case PriceUnitToken:
		return UnitToken
	case PriceUnitImage:
		return UnitImage
	case PriceUnitAudioSecond:
		return UnitAudioSecond
	case PriceUnitCharacter:
		return UnitCharacter
	}
	return -1
}

// EffectiveModality returns the modality with the text default applied.
func (m ModelInfo) EffectiveModality() string {
	if m.Modality == "" {
		return ModalityText
	}
	return m.Modality
}

// EffectivePriceUnit returns the price unit with the token default applied.
func (m ModelInfo) EffectivePriceUnit() PriceUnit {
	return EffectiveUnit(m.PriceUnit)
}

// UnitFor reports the billing unit configured for a model, defaulting to token for a
// model with no catalog entry or an entry that declares no unit. This is the single
// place the rest of the code asks "what is this model priced in" — the answer always
// comes from configuration, never from a request or a worker's attestation.
func (c *Config) UnitFor(model string) PriceUnit {
	if c == nil {
		return PriceUnitToken
	}
	info, ok := c.ModelCatalog[model]
	if !ok {
		return PriceUnitToken
	}
	return info.EffectivePriceUnit()
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
		// modality / price_unit are closed vocabularies: a typo ("vison") would
		// silently fall back to text/token and mis-describe the model to every
		// client reading the catalog.
		switch info.Modality {
		case "", ModalityText, ModalityVision, ModalityImage, ModalityASR, ModalityTTS:
		default:
			return fmt.Errorf("settlement.model_catalog[%q].modality = %q is not one of text|vision|image|asr|tts", model, info.Modality)
		}
		switch info.PriceUnit {
		case "", PriceUnitToken, PriceUnitImage, PriceUnitAudioSecond, PriceUnitCharacter:
		default:
			return fmt.Errorf("settlement.model_catalog[%q].price_unit = %q is not one of token|image|audio_second|character", model, info.PriceUnit)
		}
		// A non-token unit is billable now, so instead of refusing it we require the
		// pieces it needs. A per-image model with no per-unit price would bill
		// quantity × nil = $0, and the reconciler would agree — an under-charge that
		// never surfaces as drift. Refuse it at start-up instead.
		if u := info.EffectivePriceUnit(); !u.IsToken() {
			if _, ok := parsePerUnit(info.UnitUSD); !ok {
				return fmt.Errorf("settlement.model_catalog[%q].unit_usd is required for price_unit %q (got %q)", model, u, info.UnitUSD)
			}
			// Unit and modality must describe the same kind of model. A mismatch
			// (e.g. modality "image" priced per token) is a configuration mistake that
			// would otherwise bill the model in a unit nobody intended.
			wantModality := map[PriceUnit]string{
				PriceUnitImage:       ModalityImage,
				PriceUnitAudioSecond: "", // asr or tts — either is valid for per-second
				PriceUnitCharacter:   ModalityTTS,
			}
			if want, ok := wantModality[u]; ok && want != "" && info.EffectiveModality() != want {
				return fmt.Errorf("settlement.model_catalog[%q]: price_unit %q requires modality %q, got %q",
					model, u, want, info.EffectiveModality())
			}
			if u == PriceUnitAudioSecond {
				m := info.EffectiveModality()
				if m != ModalityASR && m != ModalityTTS {
					return fmt.Errorf("settlement.model_catalog[%q]: price_unit %q requires modality asr or tts, got %q",
						model, u, m)
				}
			}
		} else if info.UnitUSD != "" {
			// The reverse mistake: a per-unit price on a token model is money the
			// operator thinks they are charging but never will.
			return fmt.Errorf("settlement.model_catalog[%q].unit_usd is set but price_unit is token; set price_unit to image, audio_second or character", model)
		}
		if info.MaxImagesPerRequest < 0 {
			return fmt.Errorf("settlement.model_catalog[%q].max_images_per_request must be >= 0", model)
		}
		if info.MaxAudioSec < 0 {
			return fmt.Errorf("settlement.model_catalog[%q].max_audio_sec must be >= 0", model)
		}
		if info.MaxChars < 0 {
			return fmt.Errorf("settlement.model_catalog[%q].max_chars must be >= 0", model)
		}
		if info.MaxImagesPerGeneration < 0 {
			return fmt.Errorf("settlement.model_catalog[%q].max_images_per_generation must be >= 0", model)
		}
	}
	return nil
}
