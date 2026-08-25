package admin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"openmodel/sp-state-agent/internal/settlement"
)

// ContractReader is the subset of *settlement.ContractClient the admin API needs.
// An interface so settlement endpoints can be tested with a mock chain.
type ContractReader interface {
	GetUserBalance(ctx context.Context, user, token common.Address) (*big.Int, error)
	GetSPEarnings(ctx context.Context, sp, token common.Address) (*big.Int, error)
	SettlementNonce(ctx context.Context) (uint64, error)
	GetSettlement(ctx context.Context, batchID uint64) (settlement.OnChainSettlement, error)
	IsProcessedBatch(ctx context.Context, detailsHash [32]byte) (bool, error)
	OperatorBalance(ctx context.Context) (*big.Int, error)
	OperatorAddress() common.Address
	PlatformFeeBps(ctx context.Context) (int64, error)
}

// frozenEarningsReader is the OPTIONAL v1.1 extension of ContractReader (earnings
// freeze views). Kept separate so existing mocks/fakes keep compiling; the real
// ContractClient implements it.
type frozenEarningsReader interface {
	GetTotalEarnings(ctx context.Context, sp, token common.Address) (*big.Int, error)
	GetFrozenEarnings(ctx context.Context, sp, token common.Address) (*big.Int, error)
	GetWithdrawableEarnings(ctx context.Context, sp, token common.Address) (*big.Int, error)
}

// readEarningsSplit reads an SP's per-token earnings. Against a v1.1 contract it
// returns (total, frozen, withdrawable); against v1.0 — or when the v1.1 views
// error (e.g. talking to the old deployed contract) — frozen/withdrawable are
// empty and earnings carries getSPEarnings as before.
func (sa *SettlementAPI) readEarningsSplit(ctx context.Context, evmAddr string) (earnings, frozen, withdrawable map[string]string) {
	earnings = make(map[string]string)
	frozen = make(map[string]string)
	withdrawable = make(map[string]string)
	fr, hasV11 := sa.contract.(frozenEarningsReader)
	for _, token := range sa.tokens {
		sp, tok := common.HexToAddress(evmAddr), common.HexToAddress(token.Address)
		if hasV11 {
			total, err := fr.GetTotalEarnings(ctx, sp, tok)
			if err == nil {
				if total.Sign() > 0 {
					earnings[token.Symbol] = formatTokenAmount(total, token.Decimals)
				}
				if fz, err := fr.GetFrozenEarnings(ctx, sp, tok); err == nil && fz.Sign() > 0 {
					frozen[token.Symbol] = formatTokenAmount(fz, token.Decimals)
				}
				if wd, err := fr.GetWithdrawableEarnings(ctx, sp, tok); err == nil && wd.Sign() > 0 {
					withdrawable[token.Symbol] = formatTokenAmount(wd, token.Decimals)
				}
				continue
			}
			// Fall through: v1.0 contract on chain — the method reverts.
		}
		bal, err := sa.contract.GetSPEarnings(ctx, sp, tok)
		if err != nil {
			sa.logger.Warn("failed to query SP earnings", "sp", evmAddr, "token", token.Symbol, "error", err)
			continue
		}
		if bal.Sign() > 0 {
			earnings[token.Symbol] = formatTokenAmount(bal, token.Decimals)
		}
	}
	return earnings, frozen, withdrawable
}

// SettlementAPI handles settlement-related admin endpoints.
type SettlementAPI struct {
	contract      ContractReader
	balance       *settlement.BalanceCache
	pricer        *settlement.Pricer
	settler       *settlement.Settler
	reconciler    *settlement.Reconciler
	tokens        []settlement.TokenConfig
	spMap         map[string]string        // miner address → EVM address (static config)
	minerPayoutFn func() map[string]string // live self-registered payout overlay; nil = static only
	logger        *slog.Logger
	// scanSem bounds how many request-log-scanning endpoints (receipt-proof,
	// sp-earnings-detail) run concurrently (F4): each scan allocates a multi-MB reader +
	// unmarshal garbage over a large log, so a burst of audit calls could stack memory
	// and (with a tight container limit) OOM the gateway. A small semaphore serializes
	// the heavy scans without failing callers — they briefly queue instead.
	scanSem chan struct{}
	// statsDeps supplies the catalog/developer counts for the public network-stats
	// endpoint; unset fields are omitted from the response rather than sent as 0.
	statsDeps NetworkStatsDeps
}

// NewSettlementAPI creates the settlement admin API handler.
func NewSettlementAPI(
	contract ContractReader,
	balance *settlement.BalanceCache,
	pricer *settlement.Pricer,
	settler *settlement.Settler,
	reconciler *settlement.Reconciler,
	tokens []settlement.TokenConfig,
	spMap map[string]string,
	logger *slog.Logger,
) *SettlementAPI {
	return &SettlementAPI{
		contract:   contract,
		balance:    balance,
		pricer:     pricer,
		settler:    settler,
		reconciler: reconciler,
		tokens:     tokens,
		spMap:      spMap,
		logger:     logger,
		scanSem:    make(chan struct{}, 2), // ≤2 concurrent heavy log scans
	}
}

// withScanLimit runs fn while holding one of the limited scan slots (F4), bounding the
// memory of concurrent full-log-scan endpoints. ctx cancellation (client gone / timeout)
// aborts the wait so a stampede can't pile up unboundedly.
func (sa *SettlementAPI) withScanLimit(ctx context.Context, fn func()) error {
	if sa.scanSem == nil {
		fn()
		return nil
	}
	select {
	case sa.scanSem <- struct{}{}:
		defer func() { <-sa.scanSem }()
		fn()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RegisterRoutes adds settlement endpoints to the given mux.
func (sa *SettlementAPI) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/revenue", sa.handleRevenue)
	mux.HandleFunc("/api/v1/revenue/", sa.handleRevenueBySP)
	mux.HandleFunc("/api/v1/balances", sa.handleBalances)
	mux.HandleFunc("/api/v1/balances/", sa.handleBalanceByAddr)
	mux.HandleFunc("/api/v1/settlements", sa.handleSettlements)
	mux.HandleFunc("/api/v1/settlements/", sa.handleSettlementByID)
	mux.HandleFunc("/api/v1/settle-now", sa.handleSettleNow)
	mux.HandleFunc("/api/v1/operator-balance", sa.handleOperatorBalance)
	mux.HandleFunc("/api/v1/fil-price", sa.handleFILPrice)
	mux.HandleFunc("/api/v1/reconcile", sa.handleReconcile)
	mux.HandleFunc("/api/v1/state-check", sa.handleStateCheck)
	mux.HandleFunc("/api/v1/sp-earnings-detail/", sa.handleSPEarningsDetail)
}

// maxDetailLimit caps the page size of the earnings detail so a caller (especially on
// the public, unauthenticated port) can't force an unbounded response. Use ?since= to
// page further back rather than raising this.
const maxDetailLimit = 1000

// RegisterPublicRoutes adds ONLY the endpoints that are safe to expose WITHOUT the
// admin token: the read-only, client-identity-free SP earnings detail. The separate
// public query server (public.go) mounts these. Keeping this list next to the handler
// makes "what is public" an explicit, auditable decision — never add a mutating or
// client-identifying route here.
func (sa *SettlementAPI) RegisterPublicRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/sp-earnings-detail/", withPublicCORS(sa.handleSPEarningsDetail))
	mux.HandleFunc("/api/v1/receipt-proof/", withPublicCORS(sa.handleReceiptProof))
	// Aggregate-only network metrics (counts, no identities) — published so the
	// grant metrics can be read by anyone without operator access.
	mux.HandleFunc("/api/v1/network-stats", withPublicCORS(sa.handleNetworkStats))
}

// withPublicCORS opens these READ-ONLY public routes to browser fetch() from any
// origin. The web UI's receipt viewer lives on the API origin (:18019) and reads
// the public-query origin (:18020): same host, different port — a cross-origin
// request the browser blocks without these headers. The old receipt link escaped
// this only because a full-page navigation is exempt from CORS; an in-page fetch
// is not. "*" widens nothing: the routes are public by design (anyone holding a
// request_id may audit it), GET-only, and credential-free.
func withPublicCORS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Expose-Headers", "Retry-After")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

// GET /api/v1/receipt-proof/:request_id — the verifiable-billing projection (A1).
// Returns the billing-ledger row (with the worker-signed receipt), the Merkle
// inclusion proof, and the on-chain batch identity, so ANYONE holding a request_id
// can verify offline that the settled charge matches what the worker attested.
// Read-only; exposes a single request's own data keyed by its unguessable id.
func (sa *SettlementAPI) handleReceiptProof(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if sa.settler == nil {
		jsonError(w, "settlement engine not running", http.StatusServiceUnavailable)
		return
	}
	rid := strings.TrimRight(strings.TrimPrefix(r.URL.Path, "/api/v1/receipt-proof/"), "/")
	if rid == "" {
		jsonError(w, "usage: /api/v1/receipt-proof/<request_id>", http.StatusBadRequest)
		return
	}
	var proof *settlement.ReceiptProof
	var pending *settlement.UnsettledStatus
	var err error
	if werr := sa.withScanLimit(r.Context(), func() {
		proof, err = sa.settler.BuildReceiptProof(rid)
		if err != nil {
			// Not in a committed batch — is it billed and merely awaiting one?
			// "Not settled yet" and "no such request" are different answers, and
			// the moment a user is most likely to open their receipt link is
			// right after the reply, i.e. before the batch. Serving them an
			// "error" there reads as lost money; it is just a queue.
			pending, _ = sa.settler.FindUnsettled(rid)
		}
	}); werr != nil {
		jsonError(w, "server busy, retry", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case err == nil:
		_ = json.NewEncoder(w).Encode(proof)
	case pending != nil:
		eta := sa.settler.SettleETA()
		w.Header().Set("Retry-After", strconv.FormatInt(eta, 10))
		w.WriteHeader(http.StatusAccepted) // 202: recorded, completion pending
		rec := pending.Record
		_ = json.NewEncoder(w).Encode(map[string]any{
			"request_id":              rid,
			"status":                  "pending_settlement",
			"settled":                 false,
			"message":                 "This request is recorded and billed; its settlement batch has not been committed on-chain yet. The full Merkle inclusion proof becomes available right after the next settlement pass.",
			"next_settlement_eta_sec": eta,
			"request_time":            rec.Timestamp,
			"model":                   rec.Model,
			"total_tokens":            rec.TotalTokens,
			"worker_receipt":          rec.Receipt, // ed25519-signed; verifiable immediately
			"verify_now":              "The worker_receipt signature can be checked immediately (verify-receipt.py steps 1-2); re-fetch this URL after the ETA for the on-chain steps.",
		})
	default:
		jsonError(w, "unknown request id — no billing record found (check the id; records also rotate out after ~2×50MB of traffic)", http.StatusNotFound)
	}
}

// GET /api/v1/sp-earnings-detail/:sp — per-request earnings for one SP. Lets an SP
// see, for each inference request it served, how much it earned and whether that
// request has been settled on-chain (and in which tx). Query params:
//   - since: unix seconds; only requests at/after this time (optional).
//   - limit: max items, newest first (default 200).
//
// :sp may be a miner address (resolved via sp_address_map) or an EVM address.
func (sa *SettlementAPI) handleSPEarningsDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if sa.settler == nil {
		jsonError(w, "settlement engine not running", http.StatusServiceUnavailable)
		return
	}
	sp := strings.TrimPrefix(r.URL.Path, "/api/v1/sp-earnings-detail/")
	sp = strings.TrimRight(sp, "/")
	if sp == "" {
		jsonError(w, "SP address required", http.StatusBadRequest)
		return
	}
	evm := sa.resolveToEVM(sp) // miner address → EVM, or passthrough if already EVM

	var sinceUnix int64
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			sinceUnix = n
		}
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxDetailLimit {
		limit = maxDetailLimit
	}

	// Read the on-chain platform fee so per-request earnings use the exact fee the
	// contract applies. Fall back to 0 (no fee) only if the read fails.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	feeBps, err := sa.contract.PlatformFeeBps(ctx)
	if err != nil {
		sa.logger.Warn("could not read platform fee, assuming 0 for detail view", "error", err)
		feeBps = 0
	}

	var res any
	var derr error
	if werr := sa.withScanLimit(r.Context(), func() {
		res, derr = sa.settler.SPEarningsDetail(evm, sinceUnix, limit, feeBps)
	}); werr != nil {
		jsonError(w, "server busy, retry", http.StatusServiceUnavailable)
		return
	}
	if derr != nil {
		jsonError(w, "failed to compute SP earnings detail: "+derr.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, res)
}

// GET /api/v1/state-check — verify the integrity of the persisted settlement state
// (B3). Returns 200 when the state is consistent, 409 when problems are found (so a
// restore script / CI gate can fail on a bad restore before starting the gateway).
func (sa *SettlementAPI) handleStateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if sa.settler == nil {
		jsonError(w, "state check not available (settlement disabled)", http.StatusServiceUnavailable)
		return
	}
	res := sa.settler.VerifyState()
	status := http.StatusOK
	if !res.OK {
		status = http.StatusConflict
	}
	jsonResponse(w, status, res)
}

// GET /api/v1/reconcile — run the three-way billing reconciliation on demand and
// return the report (B4). Also runs periodically in the background; this endpoint
// lets an operator or the deploy pipeline check drift immediately.
func (sa *SettlementAPI) handleReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
		return
	}
	if sa.reconciler == nil {
		jsonError(w, "reconciliation not available (settlement disabled)", http.StatusServiceUnavailable)
		return
	}
	report, err := sa.reconciler.Run(r.Context())
	if err != nil {
		jsonError(w, "reconciliation failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	status := http.StatusOK
	if !report.WithinTolerance {
		status = http.StatusConflict // 409 signals drift so a CI gate can fail on it
	}
	jsonResponse(w, status, report)
}

// GET /api/v1/revenue — all SP earnings summary
func (sa *SettlementAPI) handleRevenue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	type spRevenue struct {
		MinerAddress string            `json:"miner_address"`
		EVMAddress   string            `json:"evm_address"`
		Earnings     map[string]string `json:"earnings"` // total credited (withdrawable + frozen)
		// Present only against a v1.1 contract with an earnings freeze:
		Frozen       map[string]string `json:"frozen_earnings,omitempty"`
		Withdrawable map[string]string `json:"withdrawable_earnings,omitempty"`
	}

	var results []spRevenue
	for minerAddr, evmAddr := range sa.effectiveSPMap() {
		earnings, frozen, withdrawable := sa.readEarningsSplit(ctx, evmAddr)
		results = append(results, spRevenue{
			MinerAddress: minerAddr,
			EVMAddress:   evmAddr,
			Earnings:     earnings,
			Frozen:       frozen,
			Withdrawable: withdrawable,
		})
	}

	filPrice := sa.pricer.GetFILPriceUSD()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"providers":     results,
		"fil_price_usd": filPrice.Text('f', 4),
	})
}

// GET /api/v1/revenue/:sp — single SP earnings
func (sa *SettlementAPI) handleRevenueBySP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	spAddr := strings.TrimPrefix(r.URL.Path, "/api/v1/revenue/")
	spAddr = strings.TrimRight(spAddr, "/")
	if spAddr == "" {
		jsonError(w, "SP address required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	evmAddr := sa.resolveToEVM(spAddr)
	earnings, frozen, withdrawable := sa.readEarningsSplit(ctx, evmAddr)
	if len(frozen) > 0 || len(withdrawable) > 0 {
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"address":               evmAddr,
			"earnings":              earnings,
			"frozen_earnings":       frozen,
			"withdrawable_earnings": withdrawable,
		})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"address":  evmAddr,
		"earnings": earnings,
	})
}

// GET /api/v1/balances — all user balances
func (sa *SettlementAPI) handleBalances(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	allBalances := sa.balance.GetAllBalances()

	type userBalance struct {
		Wallet       string            `json:"wallet"`
		Balances     map[string]string `json:"balances"`
		PendingSpend string            `json:"pending_spend_usd"`
	}

	var results []userBalance
	for wallet, tokenBals := range allBalances {
		bals := make(map[string]string)
		for _, token := range sa.tokens {
			if bal, ok := tokenBals[token.Address]; ok && bal.Sign() > 0 {
				bals[token.Symbol] = formatTokenAmount(bal, token.Decimals)
			}
		}
		ps := sa.balance.GetPendingSpend(wallet)
		results = append(results, userBalance{
			Wallet:       wallet,
			Balances:     bals,
			PendingSpend: ps.Text('f', 6),
		})
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"users": results,
		"total": len(results),
	})
}

// GET /api/v1/balances/:addr — single user balance
func (sa *SettlementAPI) handleBalanceByAddr(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	addr := strings.TrimPrefix(r.URL.Path, "/api/v1/balances/")
	addr = strings.TrimRight(addr, "/")
	if addr == "" {
		jsonError(w, "address required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	userAddr := common.HexToAddress(addr)
	balances := make(map[string]string)
	for _, token := range sa.tokens {
		bal, err := sa.contract.GetUserBalance(ctx, userAddr, common.HexToAddress(token.Address))
		if err != nil {
			continue
		}
		if bal.Sign() > 0 {
			balances[token.Symbol] = formatTokenAmount(bal, token.Decimals)
		}
	}

	ps := sa.balance.GetPendingSpend(addr)

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"address":           addr,
		"balances":          balances,
		"pending_spend_usd": ps.Text('f', 6),
	})
}

// GET /api/v1/settlements — list settlement batches (from local JSONL)
func (sa *SettlementAPI) handleSettlements(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	nonce, err := sa.contract.SettlementNonce(ctx)
	if err != nil {
		jsonError(w, "failed to query settlement nonce: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"total_batches": nonce,
	})
}

// GET /api/v1/settlements/:id — on-chain settlement record for a batch, plus a
// cross-reference against the local audit log (settlements.jsonl). Used by
// `settlement-cli verify` to reconcile chain vs local data.
func (sa *SettlementAPI) handleSettlementByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	idStr := strings.TrimPrefix(r.URL.Path, "/api/v1/settlements/")
	idStr = strings.TrimRight(idStr, "/")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil || id == 0 {
		jsonError(w, "invalid batch ID (must be a positive integer)", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	nonce, err := sa.contract.SettlementNonce(ctx)
	if err != nil {
		jsonError(w, "failed to query settlement nonce: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if id > nonce {
		jsonError(w, "batch "+idStr+" not found (latest batch is "+strconv.FormatUint(nonce, 10)+")", http.StatusNotFound)
		return
	}

	rec, err := sa.contract.GetSettlement(ctx, id)
	if err != nil {
		jsonError(w, "failed to query settlement: "+err.Error(), http.StatusInternalServerError)
		return
	}

	hashHex := "0x" + hex.EncodeToString(rec.DetailsHash[:])
	processed, err := sa.contract.IsProcessedBatch(ctx, rec.DetailsHash)
	if err != nil {
		sa.logger.Warn("failed to query processedBatches", "batch", id, "error", err)
	}

	onChain := map[string]interface{}{
		"details_hash":  hashHex,
		"total_amount":  bigIntString(rec.TotalAmount),
		"settled_count": uint64OrZero(rec.SettledCount),
		"failed_count":  uint64OrZero(rec.FailedCount),
		"timestamp":     uint64OrZero(rec.Timestamp),
		"processed":     processed,
	}
	// Batch inference stats exist on schema-3 (v1.3) contracts only; omit the keys
	// entirely against older contracts instead of reporting a misleading 0.
	if rec.RequestCount != nil {
		onChain["request_count"] = uint64OrZero(rec.RequestCount)
	}
	if rec.TokenCount != nil {
		onChain["token_count"] = uint64OrZero(rec.TokenCount)
	}
	resp := map[string]interface{}{
		"batch_id": id,
		"on_chain": onChain,
	}

	// Cross-reference the local audit log.
	if sa.settler != nil {
		if audit, ok := sa.settler.FindSettlementByHash(hashHex); ok {
			resp["local_audit"] = map[string]interface{}{
				"found":        true,
				"tx_hash":      audit.TxHash,
				"block_number": audit.BlockNumber,
				"gas_used":     audit.GasUsed,
				"item_count":   audit.ItemCount,
			}
		} else {
			resp["local_audit"] = map[string]interface{}{"found": false}
		}
	}

	jsonResponse(w, http.StatusOK, resp)
}

func bigIntString(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}

func uint64OrZero(v *big.Int) uint64 {
	if v == nil {
		return 0
	}
	return v.Uint64()
}

// POST /api/v1/settle-now — trigger immediate settlement
func (sa *SettlementAPI) handleSettleNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	if sa.settler == nil {
		jsonError(w, "settlement engine not running", http.StatusServiceUnavailable)
		return
	}

	sa.settler.TriggerNow()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"triggered": true,
		"message":   "settlement cycle queued",
	})
}

// GET /api/v1/operator-balance — operator wallet FIL balance
func (sa *SettlementAPI) handleOperatorBalance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	bal, err := sa.contract.OperatorBalance(ctx)
	if err != nil {
		jsonError(w, "failed to query operator balance: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := map[string]interface{}{
		"address": sa.contract.OperatorAddress().Hex(),
		"balance": settlement.FormatFIL(bal),
	}
	// Surface the RPC endpoint currently in use so an operator can see which endpoint a
	// C2 failover has landed on (optional: only the real ContractClient reports this).
	if ep, ok := sa.contract.(interface{ ActiveEndpoint() string }); ok {
		resp["active_rpc_endpoint"] = ep.ActiveEndpoint()
	}
	jsonResponse(w, http.StatusOK, resp)
}

// GET/PUT /api/v1/fil-price — query or update FIL price
func (sa *SettlementAPI) handleFILPrice(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		price := sa.pricer.GetFILPriceUSD()
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"fil_price_usd": price.Text('f', 4),
		})

	case http.MethodPut:
		var body struct {
			Price string `json:"fil_price_usd"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		price, _, err := big.ParseFloat(body.Price, 10, 128, big.ToNearestEven)
		if err != nil || price.Sign() <= 0 {
			jsonError(w, "invalid price value", http.StatusBadRequest)
			return
		}
		sa.pricer.SetFILPriceUSD(price)
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"fil_price_usd": price.Text('f', 4),
			"updated":       true,
		})

	default:
		http.Error(w, "GET or PUT only", http.StatusMethodNotAllowed)
	}
}

// --- helpers ---

// SetMinerPayoutProvider wires the live miner → miner-signed payout overlay
// (worker.Registry.ListMinerPayoutMap) so revenue endpoints cover SELF-registered
// SPs, whose payout addresses never appear in the static sp_address_map. Found
// live: with an empty static map, /api/v1/revenue listed nothing even though two
// self-registered SPs had frozen earnings on chain.
func (sa *SettlementAPI) SetMinerPayoutProvider(f func() map[string]string) {
	sa.minerPayoutFn = f
}

// effectiveSPMap merges the static sp_address_map with the live self-registered
// payout overlay; the miner-signed value wins for its miner (same precedence as
// settlement attribution).
func (sa *SettlementAPI) effectiveSPMap() map[string]string {
	if sa.minerPayoutFn == nil {
		return sa.spMap
	}
	dyn := sa.minerPayoutFn()
	if len(dyn) == 0 {
		return sa.spMap
	}
	m := make(map[string]string, len(sa.spMap)+len(dyn))
	for k, v := range sa.spMap {
		m[k] = v
	}
	for k, v := range dyn {
		m[k] = v
	}
	return m
}

func (sa *SettlementAPI) resolveToEVM(addr string) string {
	if evmAddr, ok := sa.effectiveSPMap()[addr]; ok {
		return evmAddr
	}
	return addr
}

func formatTokenAmount(wei *big.Int, decimals int) string {
	if wei == nil || wei.Sign() == 0 {
		return "0"
	}
	scale := new(big.Float).SetInt(pow10(decimals))
	f := new(big.Float).Quo(new(big.Float).SetInt(wei), scale)
	return f.Text('f', 6)
}

func pow10(n int) *big.Int {
	result := big.NewInt(1)
	ten := big.NewInt(10)
	for i := 0; i < n; i++ {
		result.Mul(result, ten)
	}
	return result
}
