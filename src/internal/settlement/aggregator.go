package settlement

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"
)

// SettlementItem is one (user, SP, amount, token) tuple ready for on-chain submission.
type SettlementItem struct {
	UserWallet string
	UserEVM    common.Address
	SPEVM      common.Address
	Amount     *big.Int
	TokenAddr  common.Address
	TokenInfo  TokenConfig
	// AmountUSD is the exact USD this item allocates (at the plan-time FIL price).
	// Persisted in the WAL so an item the CONTRACT rejects (per-item failure inside an
	// otherwise-successful batch) can be reversed out of settledPerWallet and carried
	// as debt at the exact planned value — no re-conversion at a later price.
	AmountUSD       *big.Float
	TotalTokenCount int
	// RequestIDs are the underlying billable request IDs this item settles. They
	// give the batch a STABLE economic identity for on-chain dedup that does not
	// depend on the file cursor (audit HIGH fix: a lost/reset cursor must not
	// change the detailsHash and cause a double-charge).
	RequestIDs []string
}

// CarriedDebt is an amount a wallet still owes a specific SP (in USD), recorded
// when the wallet's on-chain balance could not cover its usage at settle time. It
// is fed back into the NEXT aggregation so the debt is collected once the balance
// is topped up (audit HIGH fix: an under-funded shortfall must not be silently
// forgotten). RequestIDs carry the originating records' identity so the eventual
// on-chain batch hash stays content-derived and dedup-safe.
type CarriedDebt struct {
	Wallet     string
	SPEVM      string
	USD        *big.Float
	RequestIDs []string
}

// aggregationKey groups records by (wallet, SP EVM address).
type aggregationKey struct {
	Wallet string
	SPEVM  string
}

type Aggregator struct {
	modelPrices      map[string]*big.Float // model → USD per token (OUTPUT / base price, from ModelPricesUSD)
	catalogInput     map[string]*big.Float // model → USD per input token (from ModelCatalog; presence enables the split)
	catalogCacheRead map[string]*big.Float // model → USD per cached (prefix-hit) prompt token
	spAddressMap     map[string]string     // miner address → EVM address (static config)
	minerPayoutMap   map[string]string     // miner address → EVM address (miner-SIGNED at self-registration; wins over static)
	workerSPMap      map[string]string     // worker_id → miner address (from registry)
	tokens           []TokenConfig
	deductPriority   []string
	logger           *slog.Logger
	// pricer supplies stablecoin USD price + depeg status (C3). Optional/nil-safe: when
	// nil, non-FIL tokens are valued 1:1 with USD and never skipped (pre-C3 behavior),
	// so the aggregator's unit tests keep constructing it without a pricer.
	pricer *Pricer
}

// SetPricer wires the price oracle so deduction can value a stablecoin at its real
// (possibly off-peg) price and skip it entirely while depegged (C3). Called once by the
// settler after construction; nil-safe if never called.
func (a *Aggregator) SetPricer(p *Pricer) { a.pricer = p }

// parsePerToken parses a "USD per 1M tokens" string into USD-per-token.
func parsePerToken(s string) (*big.Float, bool) {
	if s == "" {
		return nil, false
	}
	p, _, err := big.ParseFloat(s, 10, 128, big.ToNearestEven)
	if err != nil {
		return nil, false
	}
	p.Quo(p, big.NewFloat(1_000_000))
	return p, true
}

func NewAggregator(cfg *Config, workerSPMap map[string]string, logger *slog.Logger) *Aggregator {
	prices := make(map[string]*big.Float)
	for model, priceStr := range cfg.ModelPricesUSD {
		price, ok := parsePerToken(priceStr)
		if !ok {
			logger.Warn("invalid model price, skipping", "model", model, "price", priceStr)
			continue
		}
		prices[model] = price
	}
	// Optional per-model catalog: input + cache-read rates. A model's presence in
	// catalogInput switches its billing to the input/output/cache-read split.
	catInput, catCacheRead := ParseCatalogPrices(cfg.ModelCatalog)

	return &Aggregator{
		modelPrices:      prices,
		catalogInput:     catInput,
		catalogCacheRead: catCacheRead,
		spAddressMap:     cfg.SPAddressMap,
		workerSPMap:      workerSPMap,
		tokens:           cfg.SupportedTokens,
		deductPriority:   cfg.DeductionPriority,
		logger:           logger,
	}
}

// Aggregate is the debt-free entry point, kept for backward compatibility.
func (a *Aggregator) Aggregate(
	records []RequestRecord,
	filPriceUSD *big.Float,
	balances map[string]map[string]*big.Int,
) (items []SettlementItem, unresolved []RequestRecord) {
	items, unresolved, _, _ = a.AggregateWithDebts(records, nil, filPriceUSD, balances)
	return items, unresolved
}

// AggregateWithDebts groups records (plus any carried debts) into settlement items.
//
// Returns, in addition to the items and the worker-unresolvable records:
//   - settledPerWallet: the USD actually allocated on-chain per wallet. The settler
//     reduces pendingSpend by THIS (not the full requested cost), so a shortfall is
//     never silently dropped (audit HIGH fix C).
//   - remainingDebts: per (wallet, SP) USD that could NOT be covered by the wallet's
//     balance this cycle. The settler persists these and feeds them back next cycle
//     so the revenue is collected once the wallet is topped up.
//
// A per-wallet running balance is decremented as allocations are made across that
// wallet's SPs (and across carried debts), so the same balance is never allocated
// twice.
func (a *Aggregator) AggregateWithDebts(
	records []RequestRecord,
	carriedDebts []CarriedDebt,
	filPriceUSD *big.Float,
	balances map[string]map[string]*big.Int, // wallet → token_addr → balance
) (items []SettlementItem, unresolved []RequestRecord, settledPerWallet map[string]*big.Float, remainingDebts []CarriedDebt) {

	settledPerWallet = make(map[string]*big.Float)

	if filPriceUSD == nil || filPriceUSD.Sign() <= 0 {
		a.logger.Error("invalid FIL price (<=0), deferring all records and keeping debts",
			"records", len(records), "debts", len(carriedDebts))
		return nil, records, settledPerWallet, carriedDebts
	}

	type costEntry struct {
		totalUSD    *big.Float
		totalTokens int
		requestIDs  []string
	}
	costs := make(map[aggregationKey]*costEntry)

	for _, rec := range records {
		spEVM := a.resolveWorkerToEVM(rec.WorkerID)
		if spEVM == "" {
			unresolved = append(unresolved, rec)
			continue
		}
		cost := a.recordCostUSD(rec)
		key := aggregationKey{Wallet: rec.Wallet, SPEVM: spEVM}
		entry, ok := costs[key]
		if !ok {
			entry = &costEntry{totalUSD: new(big.Float)}
			costs[key] = entry
		}
		entry.totalUSD.Add(entry.totalUSD, cost)
		entry.totalTokens += rec.TotalTokens
		if rec.RequestID != "" {
			entry.requestIDs = append(entry.requestIDs, rec.RequestID)
		}
	}

	// Fold carried debts into the same cost map so they compete for the same
	// balance as new usage (no double-spend of balance).
	for _, d := range carriedDebts {
		if d.USD == nil || d.USD.Sign() <= 0 {
			continue
		}
		key := aggregationKey{Wallet: d.Wallet, SPEVM: d.SPEVM}
		entry, ok := costs[key]
		if !ok {
			entry = &costEntry{totalUSD: new(big.Float)}
			costs[key] = entry
		}
		entry.totalUSD.Add(entry.totalUSD, d.USD)
		entry.requestIDs = append(entry.requestIDs, d.RequestIDs...)
	}

	walletKeys := make(map[string][]aggregationKey)
	for key := range costs {
		walletKeys[key.Wallet] = append(walletKeys[key.Wallet], key)
	}
	wallets := make([]string, 0, len(walletKeys))
	for w := range walletKeys {
		wallets = append(wallets, w)
	}
	sort.Strings(wallets)

	for _, wallet := range wallets {
		keys := walletKeys[wallet]
		sort.Slice(keys, func(i, j int) bool { return keys[i].SPEVM < keys[j].SPEVM })
		working := cloneTokenBalances(balances[wallet])

		for _, key := range keys {
			entry := costs[key]
			userItems, shortfall := a.allocate(key, entry.totalUSD, entry.totalTokens, entry.requestIDs, filPriceUSD, working)
			items = append(items, userItems...)

			settled := new(big.Float).Sub(entry.totalUSD, shortfall)
			if settled.Sign() > 0 {
				if settledPerWallet[wallet] == nil {
					settledPerWallet[wallet] = new(big.Float)
				}
				settledPerWallet[wallet].Add(settledPerWallet[wallet], settled)
			}
			if shortfall.Sign() > 0 {
				remainingDebts = append(remainingDebts, CarriedDebt{
					Wallet:     wallet,
					SPEVM:      key.SPEVM,
					USD:        shortfall,
					RequestIDs: entry.requestIDs,
				})
			}
		}
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].UserWallet != items[j].UserWallet {
			return items[i].UserWallet < items[j].UserWallet
		}
		if items[i].SPEVM.Hex() != items[j].SPEVM.Hex() {
			return items[i].SPEVM.Hex() < items[j].SPEVM.Hex()
		}
		return items[i].TokenAddr.Hex() < items[j].TokenAddr.Hex()
	})

	return items, unresolved, settledPerWallet, remainingDebts
}

// allocate converts a single (wallet, SP) USD cost into token settlement items,
// deducting from the shared per-wallet `working` balance map (which it mutates).
func (a *Aggregator) allocate(
	key aggregationKey,
	totalUSD *big.Float,
	totalTokens int,
	requestIDs []string,
	filPriceUSD *big.Float,
	working map[string]*big.Int,
) ([]SettlementItem, *big.Float) {
	remainingUSD := new(big.Float).Copy(totalUSD)
	var items []SettlementItem

	for _, symbol := range a.deductPriority {
		if remainingUSD.Sign() <= 0 {
			break
		}

		tokenCfg := a.findToken(symbol)
		if tokenCfg == nil {
			continue
		}

		balance, ok := working[tokenCfg.Address]
		if !ok || balance.Sign() <= 0 {
			continue
		}

		// C3: a depegged stablecoin is not collected — skip it so the cost falls through
		// to the next priority token (e.g. FIL). Its balance is likewise excluded from
		// spendable credit (BalanceCache.availableUSD), so the two stay symmetric and a
		// depeg can't open a debt hole (bill against it here while the gate still counted
		// it as full USD). Only the monitored stablecoin with a live feed can be depegged.
		if symbol != "FIL" && a.pricer != nil && a.pricer.IsStablecoinDepegged(symbol) {
			a.logger.Warn("skipping depegged stablecoin in settlement deduction",
				"symbol", symbol, "wallet", key.Wallet)
			continue
		}

		// USD price of one whole token. FIL uses the cycle price snapshot; a stablecoin
		// uses its real (in-band, possibly slightly off-peg) price so the SP is paid full
		// USD value; any other token stays pinned at $1 (unchanged).
		priceUSD := big.NewFloat(1)
		if symbol == "FIL" {
			priceUSD = filPriceUSD
		} else if a.pricer != nil {
			priceUSD = a.pricer.StablecoinPriceUSD(symbol)
		}

		// Convert remaining USD to this token's smallest unit: tokens = USD / price.
		amountFloat := new(big.Float).Quo(remainingUSD, priceUSD)
		amount := floatToWei(amountFloat, tokenCfg.Decimals)

		var itemUSD *big.Float
		if amount.Cmp(balance) > 0 {
			// Not enough in this token: take the full balance, carry the rest over.
			amount = new(big.Int).Set(balance)
			usedUSD := new(big.Float).Mul(weiToFloat(amount, tokenCfg.Decimals), priceUSD)
			remainingUSD.Sub(remainingUSD, usedUSD)
			itemUSD = usedUSD
		} else {
			itemUSD = new(big.Float).Copy(remainingUSD) // this item covers the rest
			remainingUSD = new(big.Float)
		}

		if amount.Sign() > 0 {
			// Decrement the shared running balance so other SPs for this wallet
			// see the reduced amount (fixes cross-SP double allocation).
			working[tokenCfg.Address] = new(big.Int).Sub(balance, amount)
			items = append(items, SettlementItem{
				UserWallet:      key.Wallet,
				UserEVM:         common.HexToAddress(key.Wallet),
				SPEVM:           common.HexToAddress(key.SPEVM),
				Amount:          amount,
				TokenAddr:       common.HexToAddress(tokenCfg.Address),
				TokenInfo:       *tokenCfg,
				AmountUSD:       itemUSD,
				TotalTokenCount: totalTokens,
				RequestIDs:      requestIDs,
			})
		}
	}

	// remainingUSD > 0 means the wallet's balance could not cover the full cost.
	return items, remainingUSD
}

// cloneTokenBalances makes a deep copy of a wallet's token balance map.
func cloneTokenBalances(src map[string]*big.Int) map[string]*big.Int {
	dst := make(map[string]*big.Int, len(src))
	for k, v := range src {
		dst[k] = new(big.Int).Set(v)
	}
	return dst
}

func (a *Aggregator) resolveWorkerToEVM(workerID string) string {
	minerAddr, ok := a.workerSPMap[workerID]
	if !ok {
		return ""
	}
	// A payout address the miner itself signed at self-registration outranks the
	// operator-maintained static map for that miner.
	if evmAddr, ok := a.minerPayoutMap[minerAddr]; ok && evmAddr != "" {
		return evmAddr
	}
	evmAddr, ok := a.spAddressMap[minerAddr]
	if !ok {
		return ""
	}
	return evmAddr
}

// recordCostUSD computes a record's USD cost via the shared billing formula.
func (a *Aggregator) recordCostUSD(rec RequestRecord) *big.Float {
	return CostBreakdownUSD(rec.Model,
		rec.PromptTokens, rec.CompletionTokens, rec.CachedTokens, rec.TotalTokens,
		a.modelPrices, a.catalogInput, a.catalogCacheRead)
}

// CostBreakdownUSD is THE billing formula, shared by every component that prices a
// completed request: the settlement aggregator (what actually settles on-chain), the
// gateway's pendingSpend adjustment, the reconciler's billed total, and the restart
// restore path. When the model has catalog input pricing, it splits prompt tokens into
// cached (cache-read rate) and non-cached (input rate) and bills completion at the
// output rate; otherwise it falls back to the flat total×output price.
//
// It MUST stay the single source of truth: an audit found the gateway adjusting
// pendingSpend with the flat price while settlement cleared it with this split — since
// input < output, every settled request left a residue of
// prompt×(output−input)+cached×(output−cache_read) that SettleSpend could never drain,
// so pending grew monotonically until it hit the per-wallet cap and 402'd real traffic.
// Worse, the reconciler also billed flat, so the residue was absorbed into "pending"
// and billed==settled+pending held — the leak was invisible to the drift alert.
func CostBreakdownUSD(model string, promptTokens, completionTokens, cachedTokens, totalTokens int,
	outPrices, catalogInput, catalogCacheRead map[string]*big.Float) *big.Float {
	out := lookupPrice(outPrices, model) // output / base per-token price
	if out == nil {
		out = new(big.Float)
	}
	inp := lookupPrice(catalogInput, model)
	if inp == nil {
		return new(big.Float).Mul(out, big.NewFloat(float64(totalTokens)))
	}
	cached := cachedTokens
	if cached < 0 {
		cached = 0
	}
	if cached > promptTokens {
		cached = promptTokens
	}
	nonCached := promptTokens - cached
	cost := new(big.Float).Mul(inp, big.NewFloat(float64(nonCached)))
	cr := lookupPrice(catalogCacheRead, model)
	if cr == nil {
		cr = inp // no cache-read rate configured → bill cached tokens at the input rate
	}
	cost.Add(cost, new(big.Float).Mul(cr, big.NewFloat(float64(cached))))
	cost.Add(cost, new(big.Float).Mul(out, big.NewFloat(float64(completionTokens))))
	return cost
}

// lookupPrice returns m[model], falling back to m["default"], or nil.
func lookupPrice(m map[string]*big.Float, model string) *big.Float {
	if m == nil {
		return nil
	}
	if p, ok := m[model]; ok {
		return p
	}
	if p, ok := m["default"]; ok {
		return p
	}
	return nil
}

// ParseCatalogPrices converts a config ModelCatalog into per-token USD price maps
// (input, cache-read), for callers outside this package (the gateway) that must price
// with the identical catalog the aggregator settles with.
func ParseCatalogPrices(catalog map[string]ModelInfo) (catalogInput, catalogCacheRead map[string]*big.Float) {
	catalogInput = make(map[string]*big.Float)
	catalogCacheRead = make(map[string]*big.Float)
	for model, info := range catalog {
		if p, ok := parsePerToken(info.InputUSD); ok {
			catalogInput[model] = p
		}
		if p, ok := parsePerToken(info.CacheReadUSD); ok {
			catalogCacheRead[model] = p
		}
	}
	return catalogInput, catalogCacheRead
}

func (a *Aggregator) findToken(symbol string) *TokenConfig {
	for i := range a.tokens {
		if a.tokens[i].Symbol == symbol {
			return &a.tokens[i]
		}
	}
	return nil
}

// RecordCostUSD exposes the per-record USD cost (the same function settlement uses
// to bill a request), so the SP per-request earnings view computes amounts with an
// IDENTICAL pricing path to what actually gets settled (no pricing-basis drift).
func (a *Aggregator) RecordCostUSD(rec RequestRecord) *big.Float {
	return a.recordCostUSD(rec)
}

// RecordEarningUSD is a single request's SP earning in USD = cost × (1 − platformFee).
// platformFeeBps is the on-chain platform fee in basis points (e.g. 300 = 3%). The SP
// receives the cost minus the platform's cut, matching the contract's
// spAmount = amount − amount×feeBps/10000.
func (a *Aggregator) RecordEarningUSD(rec RequestRecord, platformFeeBps int64) *big.Float {
	cost := a.recordCostUSD(rec)
	if platformFeeBps <= 0 {
		return cost
	}
	keep := new(big.Float).Sub(big.NewFloat(1), new(big.Float).Quo(big.NewFloat(float64(platformFeeBps)), big.NewFloat(10000)))
	return new(big.Float).Mul(cost, keep)
}

// ResolveWorkerToEVM exposes worker_id → SP EVM payout address (worker→miner→EVM),
// so the per-request earnings view can attribute each request to an SP exactly as
// settlement does.
func (a *Aggregator) ResolveWorkerToEVM(workerID string) string {
	return a.resolveWorkerToEVM(workerID)
}

// BatchHash computes a deterministic hash for a batch of settlement items derived
// PURELY from the batch's economic content: the (user, sp, amount, token) tuples
// plus the set of underlying request IDs. It does NOT depend on the file cursor, so
// a crash-retry — or a lost/reset cursor that re-scans the same records — reproduces
// the SAME hash, letting the contract's processedBatches dedup prevent a
// double-charge (audit HIGH fix: previously the hash was salted with the cursor
// offset, so a cursor reset produced a different hash and bypassed dedup). Two
// genuinely different batches (different request IDs) hash differently even if their
// amounts coincide.
func BatchHash(items []SettlementItem) [32]byte {
	type hashItem struct {
		User   string `json:"u"`
		SP     string `json:"s"`
		Amount string `json:"a"`
		Token  string `json:"t"`
	}
	hItems := make([]hashItem, 0, len(items))
	idSet := make(map[string]struct{})
	for _, item := range items {
		hItems = append(hItems, hashItem{
			User:   item.UserEVM.Hex(),
			SP:     item.SPEVM.Hex(),
			Amount: item.Amount.String(),
			Token:  item.TokenAddr.Hex(),
		})
		for _, id := range item.RequestIDs {
			idSet[id] = struct{}{}
		}
	}
	sort.Slice(hItems, func(i, j int) bool {
		if hItems[i].User != hItems[j].User {
			return hItems[i].User < hItems[j].User
		}
		if hItems[i].SP != hItems[j].SP {
			return hItems[i].SP < hItems[j].SP
		}
		if hItems[i].Token != hItems[j].Token {
			return hItems[i].Token < hItems[j].Token
		}
		return hItems[i].Amount < hItems[j].Amount
	})
	reqIDs := make([]string, 0, len(idSet))
	for id := range idSet {
		reqIDs = append(reqIDs, id)
	}
	sort.Strings(reqIDs)

	payload := struct {
		Items      []hashItem `json:"items"`
		RequestIDs []string   `json:"request_ids"`
	}{Items: hItems, RequestIDs: reqIDs}
	data, _ := json.Marshal(payload)
	return sha256.Sum256(data)
}

func floatToWei(f *big.Float, decimals int) *big.Int {
	scale := new(big.Float).SetInt(pow10(decimals))
	result := new(big.Float).Mul(f, scale)
	wei, _ := result.Int(nil)
	return wei
}

func weiToFloat(wei *big.Int, decimals int) *big.Float {
	scale := new(big.Float).SetInt(pow10(decimals))
	return new(big.Float).Quo(new(big.Float).SetInt(wei), scale)
}

func pow10(n int) *big.Int {
	result := big.NewInt(1)
	ten := big.NewInt(10)
	for i := 0; i < n; i++ {
		result.Mul(result, ten)
	}
	return result
}

// UpdateWorkerSPMap refreshes the worker→miner mapping from registry.
func (a *Aggregator) UpdateWorkerSPMap(m map[string]string) {
	a.workerSPMap = m
}

// UpdateMinerPayoutMap refreshes the miner → miner-signed EVM payout overlay
// (from self-registered workers). Same call discipline as UpdateWorkerSPMap:
// invoked from the settlement cycle before aggregation.
func (a *Aggregator) UpdateMinerPayoutMap(m map[string]string) {
	a.minerPayoutMap = m
}

// EstimateCostUSD estimates the USD cost for a request based on model and max_tokens.
func EstimateCostUSD(model string, maxTokens int, modelPrices map[string]*big.Float) *big.Float {
	var pricePerToken *big.Float
	if p, ok := modelPrices[model]; ok {
		pricePerToken = p
	} else if p, ok := modelPrices["default"]; ok {
		pricePerToken = p
	} else {
		return new(big.Float)
	}
	return new(big.Float).Mul(pricePerToken, big.NewFloat(float64(maxTokens)))
}

// FormatFIL formats wei to human-readable FIL string.
func FormatFIL(wei *big.Int) string {
	if wei == nil {
		return "0"
	}
	f := weiToFloat(wei, 18)
	return fmt.Sprintf("%s FIL", f.Text('f', 6))
}
