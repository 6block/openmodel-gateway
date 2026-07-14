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

// SettlementAPI handles settlement-related admin endpoints.
type SettlementAPI struct {
	contract   ContractReader
	balance    *settlement.BalanceCache
	pricer     *settlement.Pricer
	settler    *settlement.Settler
	reconciler *settlement.Reconciler
	tokens     []settlement.TokenConfig
	spMap      map[string]string // miner address → EVM address
	logger     *slog.Logger
	// scanSem bounds how many request-log-scanning endpoints (receipt-proof,
	// sp-earnings-detail) run concurrently (F4): each scan allocates a multi-MB reader +
	// unmarshal garbage over a large log, so a burst of audit calls could stack memory
	// and (with a tight container limit) OOM the gateway. A small semaphore serializes
	// the heavy scans without failing callers — they briefly queue instead.
	scanSem chan struct{}
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
	mux.HandleFunc("/api/v1/sp-earnings-detail/", sa.handleSPEarningsDetail)
	mux.HandleFunc("/api/v1/receipt-proof/", sa.handleReceiptProof)
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
	var err error
	if werr := sa.withScanLimit(r.Context(), func() {
		proof, err = sa.settler.BuildReceiptProof(rid)
	}); werr != nil {
		jsonError(w, "server busy, retry", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(proof)
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
		Earnings     map[string]string `json:"earnings"`
	}

	var results []spRevenue
	for minerAddr, evmAddr := range sa.spMap {
		earnings := make(map[string]string)
		for _, token := range sa.tokens {
			bal, err := sa.contract.GetSPEarnings(ctx, common.HexToAddress(evmAddr), common.HexToAddress(token.Address))
			if err != nil {
				sa.logger.Warn("failed to query SP earnings", "sp", evmAddr, "token", token.Symbol, "error", err)
				continue
			}
			if bal.Sign() > 0 {
				earnings[token.Symbol] = formatTokenAmount(bal, token.Decimals)
			}
		}
		results = append(results, spRevenue{
			MinerAddress: minerAddr,
			EVMAddress:   evmAddr,
			Earnings:     earnings,
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
	earnings := make(map[string]string)
	for _, token := range sa.tokens {
		bal, err := sa.contract.GetSPEarnings(ctx, common.HexToAddress(evmAddr), common.HexToAddress(token.Address))
		if err != nil {
			continue
		}
		if bal.Sign() > 0 {
			earnings[token.Symbol] = formatTokenAmount(bal, token.Decimals)
		}
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

func (sa *SettlementAPI) resolveToEVM(addr string) string {
	if evmAddr, ok := sa.spMap[addr]; ok {
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
