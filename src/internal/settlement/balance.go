package settlement

import (
	"context"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// balanceContract is the subset of *ContractClient that BalanceCache depends on.
// Declaring it as an interface lets balance refresh be tested with a mock that
// returns canned on-chain balances. *ContractClient satisfies it, so production
// wiring is unchanged.
type balanceContract interface {
	GetUserBalance(ctx context.Context, user, token common.Address) (*big.Int, error)
}

// BalanceCache caches on-chain balances and tracks pending (unsettled) spending.
type BalanceCache struct {
	mu sync.RWMutex

	// on-chain balances: wallet → token_addr → balance
	chainBalances map[string]map[string]*big.Int

	// unsettled spending in USD per wallet (accumulated since last settlement)
	pendingSpend map[string]*big.Float

	contract   balanceContract
	tokens     []TokenConfig
	pricer     *Pricer
	refreshSec int
	wallets    []string // known wallets to refresh
	logger     *slog.Logger

	// Credit limits (both FIL-denominated; converted to USD at check time via the
	// current FIL price). Previously these config knobs were parsed but never
	// enforced (audit MEDIUM fix).
	//   - minBalanceFIL: a reserve buffer kept un-spendable, so a request can't draw
	//     the balance to absolute zero. This headroom absorbs the small gap between
	//     the off-chain cost estimate and the eventual on-chain settlement amount,
	//     which would otherwise leave the user under-funded and create carried debt.
	//   - maxPendingSpendFIL: a hard cap on a single wallet's unsettled spend. Once
	//     reached, new requests are refused until the next settlement cycle clears the
	//     pending tally, bounding the platform's exposure to any one wallet. nil = off.
	minBalanceFIL      *big.Float
	maxPendingSpendFIL *big.Float

	// Service-suspension policy (D3). suspendThreshold is the outstanding carried-debt
	// USD amount at or above which a wallet is suspended (served 402 until the debt is
	// collected). nil = suspension disabled (rely on the balance gate alone). suspended
	// holds the current debt USD per suspended wallet, refreshed from the debt ledger.
	suspendThreshold *big.Float
	suspended        map[string]*big.Float
}

// SetDebtSuspension configures the debt-based suspension policy (D3). A nil threshold
// disables suspension; a zero threshold suspends a wallet on ANY positive outstanding
// debt; a positive threshold suspends once debt reaches that USD amount.
func (bc *BalanceCache) SetDebtSuspension(thresholdUSD *big.Float) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.suspendThreshold = thresholdUSD
}

// UpdateDebts refreshes the suspended-wallet set from the current carried-debt ledger
// (wallet → outstanding debt USD). Called by the settler whenever the debt ledger is
// persisted, so suspension is lifted automatically once a wallet's debt is collected.
// Returns the number of wallets currently suspended (for the metric).
func (bc *BalanceCache) UpdateDebts(debtByWallet map[string]*big.Float) int {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	bc.suspended = make(map[string]*big.Float)
	if bc.suspendThreshold == nil {
		return 0 // suspension disabled
	}
	for wallet, debt := range debtByWallet {
		if debt == nil || debt.Sign() <= 0 {
			continue
		}
		// Suspend when debt >= threshold (a zero threshold → any positive debt).
		if debt.Cmp(bc.suspendThreshold) >= 0 {
			bc.suspended[wallet] = new(big.Float).Copy(debt)
		}
	}
	return len(bc.suspended)
}

// IsSuspended reports whether a wallet is currently suspended for unpaid debt, and
// the outstanding debt USD if so. Cheap read-path check used by the gateway per request.
func (bc *BalanceCache) IsSuspended(wallet string) (bool, *big.Float) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if d, ok := bc.suspended[wallet]; ok {
		return true, new(big.Float).Copy(d)
	}
	return false, nil
}

// SuspendedCount returns the number of wallets currently suspended (for metrics).
func (bc *BalanceCache) SuspendedCount() int {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return len(bc.suspended)
}

// SetCreditLimits configures the reserve buffer and the per-wallet unsettled-spend
// cap. A nil value leaves that limit disabled. Both are denominated in FIL.
func (bc *BalanceCache) SetCreditLimits(minBalanceFIL, maxPendingSpendFIL *big.Float) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.minBalanceFIL = minBalanceFIL
	bc.maxPendingSpendFIL = maxPendingSpendFIL
}

func NewBalanceCache(
	contract balanceContract,
	tokens []TokenConfig,
	pricer *Pricer,
	refreshSec int,
	logger *slog.Logger,
) *BalanceCache {
	return &BalanceCache{
		chainBalances: make(map[string]map[string]*big.Int),
		pendingSpend:  make(map[string]*big.Float),
		contract:      contract,
		tokens:        tokens,
		pricer:        pricer,
		refreshSec:    refreshSec,
		logger:        logger,
	}
}

// FILPriceUSD exposes the FIL/USD rate billing currently uses — the same number
// settlement converts with. Nil-safe (nil receiver or no pricer returns nil) so
// read-only surfaces like the model catalog can include it without caring whether
// settlement is enabled.
func (bc *BalanceCache) FILPriceUSD() *big.Float {
	if bc == nil || bc.pricer == nil {
		return nil
	}
	return bc.pricer.GetFILPriceUSD()
}

// SetWallets sets the list of wallets to refresh. Called when config is loaded.
func (bc *BalanceCache) SetWallets(wallets []string) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.wallets = wallets
}

// AddWallet registers a wallet for balance refresh at runtime — used by self-service key
// registration, where a wallet appears AFTER startup. Idempotent: a wallet already
// tracked (case-insensitive) is a no-op. On a genuine add it kicks an immediate,
// bounded, background refresh so a freshly-registered user who has already deposited
// on-chain can spend on the very next request; without it availableUSD reads 0 until the
// next periodic tick and the user is wrongly 402'd (or, across a restart, forever — the
// bug this fixes). Safe for concurrent use.
func (bc *BalanceCache) AddWallet(wallet string) {
	if wallet == "" {
		return
	}
	bc.mu.Lock()
	for _, w := range bc.wallets {
		if strings.EqualFold(w, wallet) {
			bc.mu.Unlock()
			return // already tracked
		}
	}
	bc.wallets = append(bc.wallets, wallet)
	bc.mu.Unlock()

	// Immediate refresh, independent of any request context (registration returns right
	// away; the refresh must outlive that handler). No-op when no chain client is wired.
	if bc.contract == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		bc.refreshWallet(ctx, wallet)
	}()
}

// WalletCount returns how many distinct wallets are currently tracked for refresh.
func (bc *BalanceCache) WalletCount() int {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return len(bc.wallets)
}

// Start begins periodic balance refreshing.
func (bc *BalanceCache) Start(ctx context.Context) {
	bc.refreshAll(ctx)

	ticker := time.NewTicker(time.Duration(bc.refreshSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bc.refreshAll(ctx)
		}
	}
}

// HasSufficientBalance checks if the wallet has enough balance (across all tokens)
// minus pending spend to cover the estimated cost in USD.
func (bc *BalanceCache) HasSufficientBalance(wallet string, estimatedCostUSD *big.Float) bool {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	available := bc.availableUSD(wallet)
	return available.Cmp(estimatedCostUSD) >= 0
}

// Reserve pre-deducts estimated cost from available balance.
// Returns false if insufficient balance.
func (bc *BalanceCache) Reserve(wallet string, estimatedCostUSD *big.Float) bool {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	available := bc.availableUSD(wallet)
	if available.Cmp(estimatedCostUSD) < 0 {
		return false
	}

	pending := bc.pendingSpend[wallet]
	if pending == nil {
		pending = new(big.Float)
	}
	newPending := new(big.Float).Add(pending, estimatedCostUSD)

	// Enforce the per-wallet unsettled-spend cap (credit limit). Once a wallet's
	// pending tally would exceed max_pending_spend_fil (converted to USD), refuse the
	// request until the next settlement cycle drains it (audit MEDIUM fix).
	if bc.maxPendingSpendFIL != nil && bc.maxPendingSpendFIL.Sign() > 0 {
		capUSD := new(big.Float).Mul(bc.maxPendingSpendFIL, bc.pricer.GetFILPriceUSD())
		if newPending.Cmp(capUSD) > 0 {
			bc.logger.Warn("wallet hit unsettled-spend cap, refusing request",
				"wallet", wallet,
				"pending_usd", newPending.Text('f', 6),
				"cap_usd", capUSD.Text('f', 6))
			return false
		}
	}

	bc.pendingSpend[wallet] = newPending
	return true
}

// Adjust corrects the pending spend after a request completes.
// estimated is the pre-reserved amount, actual is the real cost (0 for failed requests).
func (bc *BalanceCache) Adjust(wallet string, estimated, actual *big.Float) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	diff := new(big.Float).Sub(estimated, actual)
	pending := bc.pendingSpend[wallet]
	if pending == nil {
		return
	}
	bc.pendingSpend[wallet] = new(big.Float).Sub(pending, diff)
	if bc.pendingSpend[wallet].Sign() < 0 {
		bc.pendingSpend[wallet] = new(big.Float)
	}
}

// SettleSpend subtracts an exact settled USD amount from a wallet's pendingSpend
// after on-chain confirmation. Unlike a full reset, this preserves reservations
// for in-flight requests and consumption logged after the settlement cutoff (H1).
func (bc *BalanceCache) SettleSpend(wallet string, settledUSD *big.Float) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	pending, ok := bc.pendingSpend[wallet]
	if !ok {
		return
	}
	remaining := new(big.Float).Sub(pending, settledUSD)
	// Floor at zero; delete if effectively zero to keep the map small.
	if remaining.Sign() <= 0 || remaining.Cmp(big.NewFloat(1e-12)) < 0 {
		delete(bc.pendingSpend, wallet)
		return
	}
	bc.pendingSpend[wallet] = remaining
}

// AddPendingSpend adds a USD amount to a wallet's pendingSpend. Used on restart to
// re-reserve outstanding carried debt (under-funded usage that already advanced past
// the settlement cursor) so the balance gate keeps accounting for what the wallet
// still owes (audit HIGH fix C).
func (bc *BalanceCache) AddPendingSpend(wallet string, usd *big.Float) {
	if usd == nil || usd.Sign() <= 0 {
		return
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	pending := bc.pendingSpend[wallet]
	if pending == nil {
		pending = new(big.Float)
	}
	bc.pendingSpend[wallet] = new(big.Float).Add(pending, usd)
}

// ForceRefresh triggers an immediate balance refresh for all wallets.
func (bc *BalanceCache) ForceRefresh(ctx context.Context) {
	bc.refreshAll(ctx)
}

// GetAllBalances returns a snapshot of all cached balances (for settlement aggregation).
func (bc *BalanceCache) GetAllBalances() map[string]map[string]*big.Int {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	result := make(map[string]map[string]*big.Int)
	for wallet, tokens := range bc.chainBalances {
		tokenMap := make(map[string]*big.Int)
		for token, balance := range tokens {
			tokenMap[token] = new(big.Int).Set(balance)
		}
		result[wallet] = tokenMap
	}
	return result
}

// GetPendingSpend returns the current pending spend in USD for a wallet.
func (bc *BalanceCache) GetPendingSpend(wallet string) *big.Float {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if ps, ok := bc.pendingSpend[wallet]; ok {
		return new(big.Float).Copy(ps)
	}
	return new(big.Float)
}

// TotalPendingSpendUSD sums reserved-but-unsettled spend across all wallets.
// Used to publish the openmodel_settlement_pending_spend_usd gauge and to
// cross-check against the request-log total during reconciliation (B4).
func (bc *BalanceCache) TotalPendingSpendUSD() *big.Float {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	total := new(big.Float)
	for _, ps := range bc.pendingSpend {
		if ps != nil {
			total.Add(total, ps)
		}
	}
	return total
}

// RestorePendingSpend initializes pending spend from unsettled records (after restart).
// costFn must be the SAME pricing path settlement clears pending with (the aggregator's
// RecordCostUSD, i.e. the catalog split) — restoring at a different rate would recreate
// the flat-vs-split residue that could never be drained (see CostBreakdownUSD).
func (bc *BalanceCache) RestorePendingSpend(records []RequestRecord, costFn func(RequestRecord) *big.Float) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	for _, rec := range records {
		cost := costFn(rec)
		if cost == nil || cost.Sign() <= 0 {
			continue
		}
		pending := bc.pendingSpend[rec.Wallet]
		if pending == nil {
			pending = new(big.Float)
		}
		bc.pendingSpend[rec.Wallet] = new(big.Float).Add(pending, cost)
	}

	if len(bc.pendingSpend) > 0 {
		bc.logger.Info("restored pending spend from unsettled records",
			"wallets", len(bc.pendingSpend))
	}
}

// --- internal ---

// availableUSD calculates total available balance in USD minus pending spend.
// Must be called with mu held (at least RLock).
// AvailableUSDView is the PUBLIC read of the same valuation the 402 gate uses —
// multi-token, depeg-aware, pending- and buffer-adjusted. The /v1/me dashboard
// must call THIS rather than recompute: a hand-rolled FIL-only view shipped once
// and showed $0 to a wallet whose USDFC the gate was happily accepting.
func (bc *BalanceCache) AvailableUSDView(wallet string) *big.Float {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.availableUSD(wallet)
}

// TokenBalancesView returns the wallet's on-chain holdings per token symbol
// (human units), for balance-detail display.
func (bc *BalanceCache) TokenBalancesView(wallet string) map[string]*big.Float {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	out := map[string]*big.Float{}
	if tokenBalances, ok := bc.chainBalances[wallet]; ok {
		for _, tokenCfg := range bc.tokens {
			if b, ok := tokenBalances[tokenCfg.Address]; ok && b.Sign() > 0 {
				out[tokenCfg.Symbol] = weiToFloat(b, tokenCfg.Decimals)
			}
		}
	}
	return out
}

func (bc *BalanceCache) availableUSD(wallet string) *big.Float {
	filPrice := bc.pricer.GetFILPriceUSD()
	total := new(big.Float)

	if tokenBalances, ok := bc.chainBalances[wallet]; ok {
		for _, tokenCfg := range bc.tokens {
			balance, ok := tokenBalances[tokenCfg.Address]
			if !ok || balance.Sign() <= 0 {
				continue
			}
			balFloat := weiToFloat(balance, tokenCfg.Decimals)
			if tokenCfg.Symbol == "FIL" {
				usd := new(big.Float).Mul(balFloat, filPrice)
				total.Add(total, usd)
			} else if bc.pricer.IsStablecoinDepegged(tokenCfg.Symbol) {
				// C3: a depegged stablecoin is excluded from spendable credit — settlement
				// won't collect it (aggregator skips it), so counting it here would let a
				// wallet keep spending against value we can't settle → debt. Symmetric skip.
				continue
			} else {
				// Value the token at its USD price (a monitored stablecoin uses its real,
				// in-band price; everything else is pinned at $1). Keeps the credit gate and
				// settlement deduction using the identical valuation.
				usd := new(big.Float).Mul(balFloat, bc.pricer.StablecoinPriceUSD(tokenCfg.Symbol))
				total.Add(total, usd)
			}
		}
	}

	if pending, ok := bc.pendingSpend[wallet]; ok {
		total.Sub(total, pending)
	}

	// Keep a reserve buffer un-spendable so a request can't draw the balance to
	// absolute zero; this headroom absorbs estimate-vs-settlement drift (audit fix).
	if bc.minBalanceFIL != nil && bc.minBalanceFIL.Sign() > 0 {
		bufferUSD := new(big.Float).Mul(bc.minBalanceFIL, filPrice)
		total.Sub(total, bufferUSD)
	}

	return total
}

func (bc *BalanceCache) refreshAll(ctx context.Context) {
	bc.mu.RLock()
	wallets := make([]string, len(bc.wallets))
	copy(wallets, bc.wallets)
	bc.mu.RUnlock()

	for _, wallet := range wallets {
		bc.refreshWallet(ctx, wallet)
	}
}

func (bc *BalanceCache) refreshWallet(ctx context.Context, wallet string) {
	userAddr := common.HexToAddress(wallet)

	// Start from the LAST KNOWN balances, not an empty map. The gate refuses a
	// paying user only on positive evidence of an empty balance — "the RPC did
	// not answer" is not that evidence. The original code skipped failed reads
	// and then overwrote the whole entry, so one transient RPC failure erased a
	// perfectly valid cached balance and the next request got a wrong 402
	// (24h-soak finding #3). Overspend against a stale value stays bounded by
	// the per-wallet unsettled-spend cap, which is exactly what it exists for.
	bc.mu.RLock()
	tokenBalances := make(map[string]*big.Int, len(bc.tokens))
	for addr, bal := range bc.chainBalances[wallet] {
		tokenBalances[addr] = bal
	}
	bc.mu.RUnlock()

	for _, tokenCfg := range bc.tokens {
		tokenAddr := common.HexToAddress(tokenCfg.Address)
		balance, err := bc.contract.GetUserBalance(ctx, userAddr, tokenAddr)
		if err != nil {
			bc.logger.Warn("failed to refresh balance; keeping last known value",
				"wallet", wallet, "token", tokenCfg.Symbol, "error", err)
			continue // keep the previous value for this token
		}
		// A sudden positive→zero drop is how a broken RPC read looks (an empty
		// eth_call body decodes as 0 — the "fake zero" glif lesson from M3). A
		// real drain is rare and permanent; a fake zero is common and transient.
		// Re-read once before believing it: a fake zero repeating twice in a row
		// is far less likely, and a real zero costs one extra call, exactly once,
		// on the transition.
		if prev, ok := tokenBalances[tokenCfg.Address]; ok && prev.Sign() > 0 && balance.Sign() == 0 {
			confirm, err2 := bc.contract.GetUserBalance(ctx, userAddr, tokenAddr)
			if err2 != nil {
				bc.logger.Warn("suspicious zero balance and the confirm read failed; keeping last known value",
					"wallet", wallet, "token", tokenCfg.Symbol, "error", err2)
				continue
			}
			if confirm.Sign() != 0 {
				bc.logger.Warn("fake zero balance caught by confirm read",
					"wallet", wallet, "token", tokenCfg.Symbol)
				balance = confirm
			} else {
				bc.logger.Info("zero balance confirmed by second read",
					"wallet", wallet, "token", tokenCfg.Symbol)
			}
		}
		tokenBalances[tokenCfg.Address] = balance
	}

	bc.mu.Lock()
	bc.chainBalances[wallet] = tokenBalances
	bc.mu.Unlock()
}
