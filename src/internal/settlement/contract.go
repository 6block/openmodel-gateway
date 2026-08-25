package settlement

import (
	"context"
	"crypto/ecdsa"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// ErrTxTimeout means a submitted tx was not mined before the wait timeout. It may
// be stuck in the mempool (gas too low); the next submit of the same batch reuses
// its nonce with a higher gas price (RBF) instead of getting stranded.
var ErrTxTimeout = errors.New("settlement tx not mined before timeout (will be replaced with higher gas)")

const (
	// gasHeadroomPercent is added over the node's suggested gas price on a first
	// submit, giving headroom against FEVM gas-price volatility.
	gasHeadroomPercent = 25
	// gasReplacePercent is the minimum bump (>=12.5%) required for a replacement
	// (same-nonce) tx to be accepted instead of "replacement transaction underpriced".
	gasReplacePercent = 13
)

// bumpGasPrice returns p increased by percent (integer percent), rounded down.
func bumpGasPrice(p *big.Int, percent int64) *big.Int {
	if p == nil || p.Sign() <= 0 {
		return p
	}
	out := new(big.Int).Mul(p, big.NewInt(100+percent))
	return out.Div(out, big.NewInt(100))
}

// decideNonceGas picks the nonce and gas price for the next submit. Normally it uses
// the network's pending nonce with a gas-headroom bump. But if a prior submit is
// still pending (the network nonce has not advanced past it), it REPLACES that tx:
// same nonce, gas price >= prior + gasReplacePercent (RBF), so a stuck settlement
// is bumped through instead of deadlocking. Pure function for unit-testing.
func decideNonceGas(pendingNonce uint64, suggested *big.Int, lastNonce *uint64, lastGasPrice *big.Int) (uint64, *big.Int) {
	gasPrice := bumpGasPrice(suggested, gasHeadroomPercent)
	nonce := pendingNonce
	if lastNonce != nil && pendingNonce <= *lastNonce {
		nonce = *lastNonce
		if lastGasPrice != nil {
			minReplace := bumpGasPrice(lastGasPrice, gasReplacePercent)
			if gasPrice == nil || gasPrice.Cmp(minReplace) < 0 {
				gasPrice = minReplace
			}
		}
	}
	return nonce, gasPrice
}

//go:embed abi.json
var contractABIJSON string

// abi_v3.json is the v1.3 contract ABI (schema 3): submitSettlement gains
// requestCounts/tokenCounts arrays, SettlementRecord/SettlementExecuted gain the
// two stats fields, and SCHEMA_VERSION()/cumulativeRequests()/cumulativeTokens()
// exist. Which ABI a client speaks is fixed by config (settlement.contract_schema);
// schema 3 is verified against the live contract at startup so a schema/address
// mismatch dies loudly instead of failing at settle time.
//
//go:embed abi_v3.json
var contractABIV3JSON string

// buildEndpoints returns the failover-ordered, de-duplicated RPC endpoint list:
// rpc_url first (backward compat, always tried first), then rpc_urls in order.
// Blank entries are dropped; duplicates collapse so a URL listed in both places is
// only probed once.
func buildEndpoints(primary string, extra []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	add(primary)
	for _, u := range extra {
		add(u)
	}
	return out
}

// dialFirstHealthy dials endpoints starting at `start` and wrapping around, returning
// the first that both dials AND answers a cheap eth_chainId probe (a dial alone does not
// prove the node is actually serving — it can connect to a dead/ syncing endpoint). Used
// at startup and by the monitor when rotating away from a failed endpoint. Because it
// wraps, passing start=from+1 prefers a DIFFERENT endpoint over reconnecting to the
// current one, but still falls back to the current one if it is the only healthy node.
func dialFirstHealthy(endpoints []string, start int) (*ethclient.Client, int, error) {
	n := len(endpoints)
	if n == 0 {
		return nil, 0, fmt.Errorf("no endpoints")
	}
	var lastErr error
	for i := 0; i < n; i++ {
		idx := ((start % n) + n + i) % n
		client, err := ethclient.Dial(endpoints[idx])
		if err != nil {
			lastErr = err
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		_, err = client.ChainID(ctx)
		cancel()
		if err != nil {
			client.Close()
			lastErr = err
			continue
		}
		return client, idx, nil
	}
	return nil, 0, lastErr
}

type ContractClient struct {
	// endpoints is the failover-ordered RPC list; clientMu guards `client`, which is
	// swapped to the next healthy endpoint by the background monitor (C2).
	endpoints    []string
	clientMu     sync.RWMutex
	activeIdx    int
	rpcURL       string // active endpoint (for logging)
	client       *ethclient.Client
	contractAddr common.Address
	abi          abi.ABI
	// schema is the contract ABI generation this client speaks: 2 = v1.2 and
	// earlier (5-arg submitSettlement), 3 = v1.3 (7-arg with batch stats). It picks
	// which embedded ABI `abi` was parsed from and gates the stats fields end to end.
	schema       int
	operatorKey  *ecdsa.PrivateKey
	operatorAddr common.Address
	chainID      *big.Int
	logger       *slog.Logger

	// RBF tracking: the nonce/gas of the last UNCONFIRMED submit, so a retry of a
	// stuck tx replaces it (same nonce, higher gas) instead of getting "replacement
	// underpriced" or stranded behind a nonce gap. Cleared on confirmation.
	mu           sync.Mutex
	lastNonce    *uint64
	lastGasPrice *big.Int
}

func NewContractClient(cfg *Config, logger *slog.Logger) (*ContractClient, error) {
	endpoints := buildEndpoints(cfg.RPCURL, cfg.RPCURLs)
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("no FEVM RPC endpoint configured")
	}
	client, idx, err := dialFirstHealthy(endpoints, 0)
	if err != nil {
		return nil, fmt.Errorf("dial FEVM RPC (all %d endpoints): %w", len(endpoints), err)
	}

	schema := cfg.ContractSchema
	if schema == 0 {
		schema = 2 // default: the v1.2 ABI every existing deployment (incl. mainnet) runs
	}
	if schema != 2 && schema != 3 {
		return nil, fmt.Errorf("settlement.contract_schema must be 2 or 3, got %d", schema)
	}
	abiSrc := contractABIJSON
	if schema == 3 {
		abiSrc = contractABIV3JSON
	}
	parsed, err := abi.JSON(strings.NewReader(abiSrc))
	if err != nil {
		return nil, fmt.Errorf("parse contract ABI (schema %d): %w", schema, err)
	}

	key, err := crypto.HexToECDSA(strings.TrimPrefix(cfg.OperatorPrivateKey, "0x"))
	if err != nil {
		return nil, fmt.Errorf("parse operator private key: %w", err)
	}
	addr := crypto.PubkeyToAddress(key.PublicKey)

	logger.Info("contract client initialized",
		"contract", cfg.ContractAddress,
		"operator", addr.Hex(),
		"chain_id", cfg.ChainID,
		"schema", schema,
	)

	logger.Info("FEVM RPC endpoints configured", "count", len(endpoints), "active", endpoints[idx])
	c := &ContractClient{
		endpoints:    endpoints,
		activeIdx:    idx,
		rpcURL:       endpoints[idx],
		client:       client,
		contractAddr: common.HexToAddress(cfg.ContractAddress),
		abi:          parsed,
		schema:       schema,
		operatorKey:  key,
		operatorAddr: addr,
		chainID:      big.NewInt(cfg.ChainID),
		logger:       logger,
	}
	if schema == 3 {
		// Schema 3 against a v1.2 contract would fail at settle time with an opaque
		// revert (7-arg selector unknown there). Verify SCHEMA_VERSION() up front so a
		// schema/address mismatch is a loud startup error naming the fix. Retried a
		// few times so one flaky RPC response cannot block an otherwise-valid start.
		if err := c.verifySchemaV3(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// verifySchemaV3 confirms the configured contract really speaks schema 3.
func (c *ContractClient) verifySchemaV3() error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		data, err := c.abi.Pack("SCHEMA_VERSION")
		if err != nil {
			cancel()
			return fmt.Errorf("pack SCHEMA_VERSION: %w", err)
		}
		result, err := c.callContract(ctx, data)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		out := new(big.Int)
		if err := c.unpackInto(&out, "SCHEMA_VERSION", result); err != nil {
			lastErr = err
			continue
		}
		if out.Int64() != 3 {
			return fmt.Errorf("contract %s reports SCHEMA_VERSION=%d, config says contract_schema: 3 — wrong contract address or wrong schema", c.contractAddr.Hex(), out.Int64())
		}
		return nil
	}
	return fmt.Errorf("contract %s does not answer SCHEMA_VERSION() (last error: %w) — if this is a v1.2 or older contract, set settlement.contract_schema: 2 (or remove the field); if it is v1.3, check the RPC endpoints", c.contractAddr.Hex(), lastErr)
}

// --- View Functions ---

func (c *ContractClient) GetUserBalance(ctx context.Context, user, token common.Address) (*big.Int, error) {
	data, err := c.abi.Pack("getUserBalance", user, token)
	if err != nil {
		return nil, fmt.Errorf("pack getUserBalance: %w", err)
	}
	result, err := c.callContract(ctx, data)
	if err != nil {
		return nil, err
	}
	out := new(big.Int)
	if err := c.unpackInto(&out, "getUserBalance", result); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *ContractClient) GetSPEarnings(ctx context.Context, sp, token common.Address) (*big.Int, error) {
	return c.getBigIntView(ctx, "getSPEarnings", sp, token)
}

// v1.1 earnings-freeze views. On a v1.0 contract these methods don't exist and
// the call errors — callers treat that as "no freeze support" and fall back to
// GetSPEarnings (which is the total there).

// GetTotalEarnings returns withdrawable + frozen earnings.
func (c *ContractClient) GetTotalEarnings(ctx context.Context, sp, token common.Address) (*big.Int, error) {
	return c.getBigIntView(ctx, "getTotalEarnings", sp, token)
}

// GetFrozenEarnings returns earnings still inside the freeze window (the
// confiscable dispute stake).
func (c *ContractClient) GetFrozenEarnings(ctx context.Context, sp, token common.Address) (*big.Int, error) {
	return c.getBigIntView(ctx, "getFrozenEarnings", sp, token)
}

// GetWithdrawableEarnings returns what withdrawEarnings would pay right now.
func (c *ContractClient) GetWithdrawableEarnings(ctx context.Context, sp, token common.Address) (*big.Int, error) {
	return c.getBigIntView(ctx, "getWithdrawableEarnings", sp, token)
}

// getBigIntView calls a view(address,address)→uint256 contract method.
func (c *ContractClient) getBigIntView(ctx context.Context, method string, sp, token common.Address) (*big.Int, error) {
	data, err := c.abi.Pack(method, sp, token)
	if err != nil {
		return nil, fmt.Errorf("pack %s: %w", method, err)
	}
	result, err := c.callContract(ctx, data)
	if err != nil {
		return nil, err
	}
	out := new(big.Int)
	if err := c.unpackInto(&out, method, result); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *ContractClient) GetPlatformEarnings(ctx context.Context, token common.Address) (*big.Int, error) {
	data, err := c.abi.Pack("platformEarnings", token)
	if err != nil {
		return nil, fmt.Errorf("pack platformEarnings: %w", err)
	}
	result, err := c.callContract(ctx, data)
	if err != nil {
		return nil, err
	}
	out := new(big.Int)
	if err := c.unpackInto(&out, "platformEarnings", result); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *ContractClient) IsProcessedBatch(ctx context.Context, detailsHash [32]byte) (bool, error) {
	data, err := c.abi.Pack("processedBatches", detailsHash)
	if err != nil {
		return false, fmt.Errorf("pack processedBatches: %w", err)
	}
	result, err := c.callContract(ctx, data)
	if err != nil {
		return false, err
	}
	var out bool
	if err := c.unpackInto(&out, "processedBatches", result); err != nil {
		return false, err
	}
	return out, nil
}

func (c *ContractClient) SettlementNonce(ctx context.Context) (uint64, error) {
	data, err := c.abi.Pack("settlementNonce")
	if err != nil {
		return 0, fmt.Errorf("pack settlementNonce: %w", err)
	}
	result, err := c.callContract(ctx, data)
	if err != nil {
		return 0, err
	}
	out := new(big.Int)
	if err := c.unpackInto(&out, "settlementNonce", result); err != nil {
		return 0, err
	}
	return out.Uint64(), nil
}

// CumulativeStats reads the contract's all-time inference counters (schema 3+).
// These are the public volume metrics: one call each, no log indexing. Against a
// schema-2 contract the getters do not exist and the call reverts — callers must
// treat the error as "not available on this deployment", never as zero.
func (c *ContractClient) CumulativeStats(ctx context.Context) (requests, tokens uint64, err error) {
	read := func(name string) (uint64, error) {
		data, err := c.abi.Pack(name)
		if err != nil {
			return 0, fmt.Errorf("pack %s: %w", name, err)
		}
		result, err := c.callContract(ctx, data)
		if err != nil {
			return 0, err
		}
		out := new(big.Int)
		if err := c.unpackInto(&out, name, result); err != nil {
			return 0, err
		}
		return out.Uint64(), nil
	}
	if c.schema < 3 {
		return 0, 0, fmt.Errorf("cumulative counters need contract schema 3 (this client speaks %d)", c.schema)
	}
	if requests, err = read("cumulativeRequests"); err != nil {
		return 0, 0, err
	}
	if tokens, err = read("cumulativeTokens"); err != nil {
		return 0, 0, err
	}
	return requests, tokens, nil
}

// PlatformFeeBps reads the on-chain platform fee in basis points. Used by the SP
// per-request earnings view to compute each request's SP earning with the SAME fee
// the contract actually applies (no config drift).
func (c *ContractClient) PlatformFeeBps(ctx context.Context) (int64, error) {
	data, err := c.abi.Pack("platformFeeBps")
	if err != nil {
		return 0, fmt.Errorf("pack platformFeeBps: %w", err)
	}
	result, err := c.callContract(ctx, data)
	if err != nil {
		return 0, err
	}
	out := new(big.Int)
	if err := c.unpackInto(&out, "platformFeeBps", result); err != nil {
		return 0, err
	}
	return out.Int64(), nil
}

// OnChainSettlement mirrors the contract's SettlementRecord struct, returned by
// getSettlement(batchId). Field names map to the ABI tuple components. The two
// stats fields exist on schema-3 contracts only; against schema 2 they stay nil.
type OnChainSettlement struct {
	BatchId      *big.Int
	Timestamp    *big.Int
	TotalAmount  *big.Int
	SettledCount *big.Int
	FailedCount  *big.Int
	DetailsHash  [32]byte
	RequestCount *big.Int
	TokenCount   *big.Int
}

// onChainSettlementV2 is the schema-2 tuple shape (no stats fields); ConvertType
// needs the struct's field count to match the ABI tuple exactly.
type onChainSettlementV2 struct {
	BatchId      *big.Int
	Timestamp    *big.Int
	TotalAmount  *big.Int
	SettledCount *big.Int
	FailedCount  *big.Int
	DetailsHash  [32]byte
}

func zeroBigInts(n int) []*big.Int {
	out := make([]*big.Int, n)
	for i := range out {
		out[i] = big.NewInt(0)
	}
	return out
}

// GetSettlement reads an on-chain settlement record by batch ID.
func (c *ContractClient) GetSettlement(ctx context.Context, batchID uint64) (OnChainSettlement, error) {
	var out OnChainSettlement
	data, err := c.abi.Pack("getSettlement", new(big.Int).SetUint64(batchID))
	if err != nil {
		return out, fmt.Errorf("pack getSettlement: %w", err)
	}
	result, err := c.callContract(ctx, data)
	if err != nil {
		return out, err
	}
	return unpackSettlement(c.abi, c.schema, result)
}

// unpackSettlement decodes a getSettlement return (a single tuple) into
// OnChainSettlement using the standard abigen pattern (Unpack + ConvertType), which
// — unlike UnpackIntoInterface — correctly handles a sole tuple output. The tuple
// shape follows the schema: 6 fields on schema 2, 8 on schema 3.
func unpackSettlement(parsed abi.ABI, schema int, result []byte) (OnChainSettlement, error) {
	var out OnChainSettlement
	unpacked, err := parsed.Unpack("getSettlement", result)
	if err != nil {
		return out, fmt.Errorf("unpack getSettlement: %w", err)
	}
	if len(unpacked) == 0 {
		return out, fmt.Errorf("empty getSettlement result")
	}
	if schema >= 3 {
		out = *abi.ConvertType(unpacked[0], new(OnChainSettlement)).(*OnChainSettlement)
		return out, nil
	}
	v2 := *abi.ConvertType(unpacked[0], new(onChainSettlementV2)).(*onChainSettlementV2)
	out = OnChainSettlement{
		BatchId:      v2.BatchId,
		Timestamp:    v2.Timestamp,
		TotalAmount:  v2.TotalAmount,
		SettledCount: v2.SettledCount,
		FailedCount:  v2.FailedCount,
		DetailsHash:  v2.DetailsHash,
	}
	return out, nil
}

// --- Write Functions ---

type SettlementBatch struct {
	Users   []common.Address
	SPs     []common.Address
	Amounts []*big.Int
	Tokens  []common.Address
	// Per-item inference stats (schema 3): request count and prompt+completion token
	// sum of the requests aggregated into each item. Ignored when the client speaks
	// schema 2 — the arrays never reach the wire there.
	RequestCounts []*big.Int
	TokenCounts   []*big.Int
	DetailsHash   [32]byte
}

func (c *ContractClient) SubmitSettlement(ctx context.Context, batch SettlementBatch) (*types.Transaction, error) {
	auth, err := c.transactor(ctx)
	if err != nil {
		return nil, err
	}

	args := []interface{}{batch.Users, batch.SPs, batch.Amounts, batch.Tokens, batch.DetailsHash}
	if c.schema >= 3 {
		reqCounts, tokCounts := batch.RequestCounts, batch.TokenCounts
		// A WAL written by an older build has no stats arrays; zero-fill rather than
		// refuse — the money math is untouched and the stats honestly report 0.
		if len(reqCounts) != len(batch.Users) {
			reqCounts = zeroBigInts(len(batch.Users))
		}
		if len(tokCounts) != len(batch.Users) {
			tokCounts = zeroBigInts(len(batch.Users))
		}
		args = []interface{}{batch.Users, batch.SPs, batch.Amounts, batch.Tokens, reqCounts, tokCounts, batch.DetailsHash}
	}

	cli := c.curClient()
	bc := bind.NewBoundContract(c.contractAddr, c.abi, cli, cli, cli)
	tx, err := bc.Transact(auth, "submitSettlement", args...)
	if err != nil {
		return nil, fmt.Errorf("send submitSettlement: %w", err)
	}

	c.logger.Info("settlement tx submitted",
		"tx_hash", tx.Hash().Hex(),
		"batch_size", len(batch.Users),
	)
	return tx, nil
}

func (c *ContractClient) WaitForReceipt(ctx context.Context, tx *types.Transaction, timeout time.Duration) (*types.Receipt, error) {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	receipt, err := bind.WaitMined(wctx, c.curClient(), tx)
	if err != nil {
		// Distinguish a stuck (under-priced) tx from other wait errors. On timeout,
		// keep the nonce tracked so the next submit of the same batch REPLACES it
		// with a higher gas price (RBF) instead of deadlocking.
		if errors.Is(wctx.Err(), context.DeadlineExceeded) {
			c.logger.Warn("settlement tx not mined before timeout; will replace with higher gas (RBF)",
				"tx_hash", tx.Hash().Hex(), "timeout", timeout.String())
			return nil, ErrTxTimeout
		}
		return nil, fmt.Errorf("wait for receipt: %w", err)
	}
	// Mined (success OR revert): the nonce is consumed, stop tracking it so the next
	// submit uses a fresh nonce.
	c.clearPendingTx()
	if receipt.Status == types.ReceiptStatusFailed {
		return receipt, fmt.Errorf("transaction reverted: %s", tx.Hash().Hex())
	}
	c.logger.Info("settlement tx confirmed",
		"tx_hash", tx.Hash().Hex(),
		"block", receipt.BlockNumber.Uint64(),
		"gas_used", receipt.GasUsed,
	)
	return receipt, nil
}

// --- Utility ---

// curClient returns the currently-active RPC client under a read lock. EVERY RPC call
// goes through this so the background monitor (C2) can atomically swap in a healthy
// endpoint without racing in-flight callers. Callers snapshot the pointer once and use
// it for the whole logical operation, so a mid-operation swap does not mix two clients.
func (c *ContractClient) curClient() *ethclient.Client {
	c.clientMu.RLock()
	defer c.clientMu.RUnlock()
	return c.client
}

// ActiveEndpoint reports the URL of the RPC endpoint currently in use (for /health and
// diagnostics — shows which endpoint a rotation has landed on).
func (c *ContractClient) ActiveEndpoint() string {
	c.clientMu.RLock()
	defer c.clientMu.RUnlock()
	return c.rpcURL
}

// MonitorEndpoints runs until ctx is cancelled, probing the active RPC endpoint every
// `interval` with a cheap eth_blockNumber call. On a failed probe it rotates to the next
// healthy endpoint, atomically swapping the client. With fewer than two endpoints there
// is nothing to fail over to, so it returns immediately. This is C2's self-healing: a
// provider going down (the GLIF blip the 24h soak hit) is routed around within one
// interval instead of stalling every settlement / balance-refresh call until a human
// swaps the config and restarts.
func (c *ContractClient) MonitorEndpoints(ctx context.Context, interval time.Duration) {
	if len(c.endpoints) < 2 {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	c.logger.Info("FEVM RPC endpoint monitor started", "endpoints", len(c.endpoints), "interval", interval.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			_, err := c.curClient().BlockNumber(pctx)
			cancel()
			if err == nil {
				continue // active endpoint healthy
			}
			c.rotateEndpoint(err)
		}
	}
}

// rotateEndpoint dials the next healthy endpoint (preferring one DIFFERENT from the
// current, but reconnecting to the current if it is the only healthy node) and swaps it
// in atomically, closing the old client. If NO endpoint is healthy it keeps the existing
// client so calls still have something to attempt — a total RPC outage is transient and
// self-heals on the next probe once any endpoint recovers; dropping the client would only
// turn every call into an immediate nil-panic instead of a normal transport error.
func (c *ContractClient) rotateEndpoint(cause error) {
	c.clientMu.RLock()
	from := c.activeIdx
	old := c.client
	c.clientMu.RUnlock()

	newClient, idx, err := dialFirstHealthy(c.endpoints, from+1)
	if err != nil {
		c.logger.Warn("all FEVM RPC endpoints unhealthy; keeping current client",
			"active", c.endpoints[from], "probe_err", cause.Error(), "rotate_err", err.Error())
		return
	}

	c.clientMu.Lock()
	c.client = newClient
	c.activeIdx = idx
	c.rpcURL = c.endpoints[idx]
	c.clientMu.Unlock()
	if old != nil {
		old.Close()
	}

	if idx == from {
		c.logger.Info("reconnected FEVM RPC endpoint after probe failure",
			"endpoint", c.endpoints[idx], "cause", cause.Error())
	} else {
		c.logger.Warn("rotated FEVM RPC endpoint after probe failure",
			"from", c.endpoints[from], "to", c.endpoints[idx], "cause", cause.Error())
	}
}

// ErrReorged means a tx that was previously mined is no longer found on-chain (its
// block was reorged away). The caller must re-submit the batch; on-chain dedup
// (processedBatches[detailsHash]) makes re-submission safe.
var ErrReorged = errors.New("settlement tx receipt disappeared (chain reorg); batch must be re-submitted")

// WaitForFinality blocks until the tx's block is buried under `confirmations`
// additional blocks (reorg safety, C2), polling the head and re-fetching the receipt.
// If the receipt disappears mid-wait (its block was reorged away) it returns
// ErrReorged so the caller re-submits. Returns the final receipt once buried deep
// enough. A confirmations value <= 0 means "no extra depth required" (mined == final).
func (c *ContractClient) WaitForFinality(ctx context.Context, txHash common.Hash, confirmations uint64, timeout time.Duration) (*types.Receipt, error) {
	return waitForFinality(ctx, txHash, confirmations, timeout, 3*time.Second, c.receiptAndHead)
}

// maxNotFoundStreak is how many consecutive NotFound polls WaitForFinality tolerates
// before declaring a reorg. On Filecoin/FEVM, eth_getTransactionReceipt can transiently
// return NotFound for a still-mined tx while the head tipset re-settles (normal
// Expected-Consensus head wobble / null rounds near the chain head). A SINGLE NotFound is
// therefore NOT proof of a reorg — only a receipt that stays absent across several
// consecutive polls is. Tolerating transient misses avoids false-positive "reorged"
// re-submits, which on Calibration fired on ~90% of settlements and, by leaving each
// batch unconfirmed for an extra cycle, roughly halved settlement throughput (a 24h soak
// found this: pending spend outran settlement until it hit the per-wallet cap and 402'd
// real traffic). A genuinely dropped tx stays absent, so the streak still catches it.
const maxNotFoundStreak = 5 // ~15s of continuous absence at the 3s poll interval

// waitForFinality is the pollable core of WaitForFinality, split out so the reorg /
// transient-NotFound-tolerance logic is unit-testable without a live RPC. fetch returns
// the tx receipt and current head; it reports a missing receipt as ethereum.NotFound.
func waitForFinality(
	ctx context.Context, txHash common.Hash, confirmations uint64, timeout, pollInterval time.Duration,
	fetch func(context.Context, common.Hash) (*types.Receipt, uint64, error),
) (*types.Receipt, error) {
	if confirmations == 0 {
		r, _, err := fetch(ctx, txHash)
		return r, err
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	notFoundStreak := 0
	for {
		receipt, head, err := fetch(wctx, txHash)
		switch {
		case errors.Is(err, ethereum.NotFound):
			notFoundStreak++
			if notFoundStreak >= maxNotFoundStreak {
				// Durably absent across the whole streak → genuinely reorged away; the
				// caller re-submits (on-chain processedBatches dedup makes that safe).
				return nil, ErrReorged
			}
		case err == nil && receipt != nil:
			notFoundStreak = 0 // present (possibly re-mined at a new block) → not reorged
			txBlock := receipt.BlockNumber.Uint64()
			if head >= txBlock && head-txBlock >= confirmations {
				return receipt, nil // buried deep enough → final
			}
		}
		select {
		case <-wctx.Done():
			return nil, fmt.Errorf("wait for finality (%d confirmations): %w", confirmations, wctx.Err())
		case <-ticker.C:
		}
	}
}

// SettlementOutcome is the per-item result of a mined settlement tx, decoded from the
// receipt's SettlementExecuted / SettlementItemFailed events. The contract SKIPS (not
// reverts) items whose user balance was drained between plan and execution, so a
// successful tx can still contain unpaid items — the settler must reverse those out of
// its "settled" accounting or the revenue silently disappears (audit finding, medium severity).
type SettlementOutcome struct {
	SettledCount  uint64
	FailedCount   uint64
	FailedIndexes []uint64 // indexes into the submitted batch's item arrays
	Found         bool     // false if the receipt carries no SettlementExecuted event
}

// ParseSettlementOutcome decodes the settlement events from a mined receipt.
func (c *ContractClient) ParseSettlementOutcome(receipt *types.Receipt) SettlementOutcome {
	var out SettlementOutcome
	if receipt == nil {
		return out
	}
	execID := c.abi.Events["SettlementExecuted"].ID
	failID := c.abi.Events["SettlementItemFailed"].ID
	for _, lg := range receipt.Logs {
		if lg == nil || len(lg.Topics) == 0 || lg.Address != c.contractAddr {
			continue
		}
		switch lg.Topics[0] {
		case execID:
			vals, err := c.abi.Events["SettlementExecuted"].Inputs.NonIndexed().Unpack(lg.Data)
			// non-indexed: totalAmount, platformFee, settledCount, failedCount, detailsHash
			if err != nil || len(vals) < 4 {
				continue
			}
			if sc, ok := vals[2].(*big.Int); ok {
				out.SettledCount = sc.Uint64()
			}
			if fc, ok := vals[3].(*big.Int); ok {
				out.FailedCount = fc.Uint64()
			}
			out.Found = true
		case failID:
			vals, err := c.abi.Events["SettlementItemFailed"].Inputs.NonIndexed().Unpack(lg.Data)
			// non-indexed: index, user, reason
			if err != nil || len(vals) < 1 {
				continue
			}
			if idx, ok := vals[0].(*big.Int); ok {
				out.FailedIndexes = append(out.FailedIndexes, idx.Uint64())
			}
		}
	}
	return out
}

// receiptAndHead fetches the tx receipt and the current head block number.
func (c *ContractClient) receiptAndHead(ctx context.Context, txHash common.Hash) (*types.Receipt, uint64, error) {
	cli := c.curClient()
	receipt, err := cli.TransactionReceipt(ctx, txHash)
	if err != nil {
		return nil, 0, err
	}
	head, err := cli.BlockNumber(ctx)
	if err != nil {
		return receipt, 0, err
	}
	return receipt, head, nil
}

func (c *ContractClient) OperatorBalance(ctx context.Context) (*big.Int, error) {
	return c.curClient().BalanceAt(ctx, c.operatorAddr, nil)
}

func (c *ContractClient) OperatorAddress() common.Address {
	return c.operatorAddr
}

func (c *ContractClient) Close() {
	c.clientMu.Lock()
	defer c.clientMu.Unlock()
	if c.client != nil {
		c.client.Close()
	}
}

// --- internal ---

func (c *ContractClient) callContract(ctx context.Context, data []byte) ([]byte, error) {
	msg := ethereum.CallMsg{
		To:   &c.contractAddr,
		Data: data,
	}
	return c.curClient().CallContract(ctx, msg, nil)
}

func (c *ContractClient) unpackInto(out interface{}, method string, data []byte) error {
	results, err := c.abi.Unpack(method, data)
	if err != nil {
		return fmt.Errorf("unpack %s: %w", method, err)
	}
	if len(results) == 0 {
		return fmt.Errorf("empty result from %s", method)
	}
	switch v := out.(type) {
	case **big.Int:
		if val, ok := results[0].(*big.Int); ok {
			*v = val
		} else {
			*v = new(big.Int)
		}
	case *bool:
		if val, ok := results[0].(bool); ok {
			*v = val
		}
	}
	return nil
}

func (c *ContractClient) transactor(ctx context.Context) (*bind.TransactOpts, error) {
	auth, err := bind.NewKeyedTransactorWithChainID(c.operatorKey, c.chainID)
	if err != nil {
		return nil, fmt.Errorf("create transactor: %w", err)
	}
	auth.Context = ctx

	cli := c.curClient()
	pendingNonce, err := cli.PendingNonceAt(ctx, c.operatorAddr)
	if err != nil {
		return nil, fmt.Errorf("pending nonce: %w", err)
	}
	suggested, err := cli.SuggestGasPrice(ctx)
	if err != nil {
		return nil, fmt.Errorf("suggest gas price: %w", err)
	}

	c.mu.Lock()
	nonce, gasPrice := decideNonceGas(pendingNonce, suggested, c.lastNonce, c.lastGasPrice)
	n := nonce
	c.lastNonce = &n
	c.lastGasPrice = new(big.Int).Set(gasPrice)
	c.mu.Unlock()

	auth.Nonce = new(big.Int).SetUint64(nonce)
	auth.GasPrice = gasPrice
	// GasLimit left 0 → bind auto-estimates per call (avoids out-of-gas from a
	// too-low fixed cap while still bounding via the node's estimate).
	return auth, nil
}

// clearPendingTx forgets the tracked unconfirmed tx after it mines, so the next
// submit uses a fresh nonce rather than trying to replace an already-mined tx.
func (c *ContractClient) clearPendingTx() {
	c.mu.Lock()
	c.lastNonce = nil
	c.lastGasPrice = nil
	c.mu.Unlock()
}
