package settlement

import (
	"bufio"
	"bytes"
	"io"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"openmodel/sp-state-agent/internal/metrics"
)

// settlementContract is the subset of *ContractClient that the Settler depends on.
// Declaring it as an interface lets the crash-safe settlement lifecycle (WAL replay,
// on-chain dedup, cursor commit) be unit-tested with a mock. *ContractClient
// satisfies it, so production wiring is unchanged.
type settlementContract interface {
	IsProcessedBatch(ctx context.Context, detailsHash [32]byte) (bool, error)
	SubmitSettlement(ctx context.Context, batch SettlementBatch) (*types.Transaction, error)
	WaitForReceipt(ctx context.Context, tx *types.Transaction, timeout time.Duration) (*types.Receipt, error)
	WaitForFinality(ctx context.Context, txHash common.Hash, confirmations uint64, timeout time.Duration) (*types.Receipt, error)
	OperatorBalance(ctx context.Context) (*big.Int, error)
	ParseSettlementOutcome(receipt *types.Receipt) SettlementOutcome
}

// Settler orchestrates the settlement lifecycle with crash-safe semantics:
//
//	Peek (no cursor advance) → aggregate → write pending WAL → submit batches →
//	confirm on-chain → commit cursor → reduce pendingSpend → delete WAL.
//
// The cursor only advances after on-chain confirmation, so a failure at any
// step never loses revenue (records are re-scanned next cycle). A write-ahead
// pending file freezes the planned batches so a crash-retry reproduces identical
// batch hashes for on-chain dedup, preventing double-charging.
type Settler struct {
	cfg         *Config
	contract    settlementContract
	scanner     *Scanner
	aggregator  *Aggregator
	pricer      *Pricer
	balance     *BalanceCache
	resolver    WorkerSPResolver
	modelPrices map[string]*big.Float
	logger      *slog.Logger

	requestLogPath    string
	settlementLogPath string
	deadLetterPath    string
	pendingPath       string
	debtPath          string
	settledTotalPath  string
	itemsLedgerPath   string
	triggerCh         chan struct{}
	mu                sync.Mutex

	// F6/R4: receipt-proof indexing. merkleIdx maps rid → byte offset of its batch line in
	// merkle-batches.jsonl; recordIdx maps rid → (offset,len) of its RequestRecord in the
	// SEPARATE receipt-records.jsonl store (R4: records are no longer embedded in the batch
	// line, which had bloated it to ~10 MB and made every read fat). merkleWarm is true once
	// the index provably reflects the whole ledger — a miss on a warm index means "not
	// settled", so no full-ledger fallback scan is ever needed (the R4 DoS/timeout fix).
	// Both indexes persist to a sidecar (receipt-index.jsonl); on restart it is replayed if
	// its recorded sizes still match the data files, else rebuilt once — so a restart no
	// longer re-parses the whole ledger before the first proof.
	merkleIdxMu sync.Mutex
	merkleIdx   map[string]int64
	recordIdx   map[string]recordLoc
	merkleWarm  bool

	// pendingRestoredCh closes once Start has finished restorePendingSpend (round-3 soak
	// finding: the reconciler's immediate first pass raced this restore after a gateway
	// restart — pendingSpend read 0 → one false DRIFT alarm per restart; see
	// Reconciler.SetReadySignal).
	pendingRestoredCh   chan struct{}
	pendingRestoredOnce sync.Once
}

// WorkerSPResolver provides worker_id → MinerAddress mapping.
type WorkerSPResolver interface {
	GetWorkerSPMap() map[string]string
}

func NewSettler(
	cfg *Config,
	contract settlementContract,
	pricer *Pricer,
	balance *BalanceCache,
	resolver WorkerSPResolver,
	requestLogPath string,
	dataDir string,
	logger *slog.Logger,
) *Settler {
	workerSPMap := make(map[string]string)
	if resolver != nil {
		workerSPMap = resolver.GetWorkerSPMap()
	}

	scanner := NewScanner(requestLogPath, dataDir+"/settlement-cursor.json", logger)
	aggregator := NewAggregator(cfg, workerSPMap, logger)
	aggregator.SetPricer(pricer) // C3: stablecoin depeg-aware deduction

	return &Settler{
		cfg:               cfg,
		contract:          contract,
		scanner:           scanner,
		aggregator:        aggregator,
		pricer:            pricer,
		balance:           balance,
		resolver:          resolver,
		modelPrices:       parseModelPrices(cfg, logger),
		pendingRestoredCh: make(chan struct{}),
		logger:            logger,
		requestLogPath:    requestLogPath,
		settlementLogPath: dataDir + "/settlements.jsonl",
		deadLetterPath:    dataDir + "/settlement-deadletter.jsonl",
		pendingPath:       dataDir + "/pending-settlement.json",
		debtPath:          dataDir + "/settlement-debt.json",
		settledTotalPath:  dataDir + "/settled-total.json",
		itemsLedgerPath:   dataDir + "/settlement-items.jsonl",
		triggerCh:         make(chan struct{}, 1),
	}
}

// Start runs the settlement loop. On startup it first resumes any interrupted
// settlement (WAL) and restores pendingSpend from unsettled records.
func (s *Settler) Start(ctx context.Context) {
	s.logger.Info("settlement engine starting",
		"interval_minutes", s.cfg.IntervalMinutes,
		"max_batch_size", s.cfg.MaxBatchSize,
	)

	// Crash recovery: finish any interrupted settlement before anything else.
	s.mu.Lock()
	if err := s.resumePending(ctx); err != nil {
		s.logger.Error("failed to resume pending settlement on startup", "error", err)
	}
	s.restorePendingSpend()
	s.mu.Unlock()
	s.markPendingRestored() // unblock the reconciler's first pass (see PendingRestored)

	ticker := time.NewTicker(time.Duration(s.cfg.IntervalMinutes) * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("settlement engine stopped")
			return
		case <-ticker.C:
			s.settleWithRecover(ctx)
		case <-s.triggerCh:
			s.logger.Info("settlement triggered manually")
			s.settleWithRecover(ctx)
		}
	}
}

// TriggerNow signals the settler to run a settlement cycle immediately.
func (s *Settler) TriggerNow() {
	select {
	case s.triggerCh <- struct{}{}:
	default:
	}
}

// PublishFundMetrics refreshes the slow-moving fund-safety gauges (pending spend,
// dead-letter depth, carried debt, WAL presence). Cheap and lock-light; call it on
// the periodic metrics tick so a dashboard stays current between settlement cycles.
func (s *Settler) PublishFundMetrics() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if total, _ := s.balance.TotalPendingSpendUSD().Float64(); total >= 0 {
		metrics.PendingSpendUSD.Set(total)
	}
	metrics.DeadLetterEntries.Set(float64(len(s.loadDeadLetter())))
	s.updateDebtMetrics(carriedDebtsToRecords(s.loadDebts()))

	if _, ok, _ := s.readPending(); ok {
		metrics.PendingSettlementWAL.Set(1)
	} else {
		metrics.PendingSettlementWAL.Set(0)
	}
}

func (s *Settler) settleWithRecover(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			metrics.SettlementCyclesTotal.WithLabelValues("panic").Inc()
			s.logger.Error("settlement panicked, will retry next cycle", "panic", r)
		}
	}()
	if err := s.Settle(ctx); err != nil {
		metrics.SettlementCyclesTotal.WithLabelValues("failed").Inc()
		s.logger.Error("settlement cycle failed", "error", err)
	}
}

// Settle runs one full settlement cycle.
func (s *Settler) Settle(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Always finish an interrupted settlement first.
	if err := s.resumePending(ctx); err != nil {
		return fmt.Errorf("resume pending: %w", err)
	}

	t0 := time.Now()

	// 1. Refresh worker→SP map from the live registry (fixes stale/empty map).
	if s.resolver != nil {
		s.aggregator.UpdateWorkerSPMap(s.resolver.GetWorkerSPMap())
	}

	// 2. Peek new billable records WITHOUT advancing the cursor.
	records, _, newCursor, err := s.scanner.Peek()
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}

	// Load any previously dead-lettered records (their worker→SP mapping may now be
	// configured) and carried under-funded debt. A cycle with NO new traffic but with
	// outstanding dead-letters or debt must still run — otherwise a balance top-up
	// during a quiet period would never be collected (audit MEDIUM fix).
	deadLettered := s.loadDeadLetter()
	carried := s.loadDebts()
	if len(records) == 0 && len(deadLettered) == 0 && len(carried) == 0 {
		s.logger.Debug("no new billable records, dead-letters, or carried debt")
		return nil
	}
	if len(records) > 0 {
		s.logger.Info("scanned billable records", "count", len(records))
	}

	// 3. Snapshot pricing/balances for a consistent aggregation. If the FIL price is
	//    stale (all sources failing in auto mode), defer the whole cycle WITHOUT
	//    advancing the cursor — billing at an arbitrary old rate would mis-charge
	//    users (audit MEDIUM fix). Records are retried next cycle once price recovers.
	if s.pricer.IsStale() {
		metrics.FILPriceStale.Set(1)
		metrics.SettlementCyclesTotal.WithLabelValues("deferred_price").Inc()
		s.logger.Error("FIL price is stale (all sources failing); deferring settlement this cycle")
		return nil
	}
	metrics.FILPriceStale.Set(0)
	balances := s.balance.GetAllBalances()
	filPrice := s.pricer.GetFILPriceUSD()
	if fp, _ := filPrice.Float64(); fp > 0 {
		metrics.FILPriceUSD.Set(fp)
	}

	// 4. Aggregate new records together with re-included dead-lettered records and
	//    carried debt against current balances.
	if len(deadLettered) > 0 {
		records = append(deadLettered, records...)
		s.logger.Info("retrying previously dead-lettered records", "count", len(deadLettered))
	}
	items, unresolved, settledPerWallet, remainingDebts := s.aggregator.AggregateWithDebts(records, carried, filPrice, balances)
	// Persist the STILL-unresolvable set (atomic replace; clears the file when empty),
	// so resolvable records are eventually collected once the mapping exists and don't
	// silently lose SP revenue (audit MEDIUM fix).
	s.writeDeadLetter(unresolved)
	if len(unresolved) > 0 {
		s.logger.Warn("records still unresolvable, kept in dead-letter", "count", len(unresolved))
	}

	// 5. pendingSpend drops only by what was ACTUALLY settled on-chain; any
	//    under-funded shortfall stays reserved and is carried as debt to next cycle
	//    (audit HIGH fix C: a shortfall must never be silently forgotten).
	settledUSD := floatMapToStrings(settledPerWallet)
	debtRecords := carriedDebtsToRecords(remainingDebts)
	if len(remainingDebts) > 0 {
		s.logger.Warn("under-funded usage carried as debt for next cycle", "debt_entries", len(remainingDebts))
	}

	// 6. Freeze the plan (batches + cursor + ACTUAL settled + remaining debt) into
	//    the WAL — even with zero items, so the cursor advance, pendingSpend
	//    reduction and debt-ledger update all happen atomically on confirmation.
	recordsByID := make(map[string]RequestRecord, len(records))
	for _, rec := range records {
		if rec.RequestID != "" {
			recordsByID[rec.RequestID] = rec
		}
	}
	pending := s.buildPending(items, newCursor, settledUSD, debtRecords, recordsByID)
	if err := s.writePending(pending); err != nil {
		return fmt.Errorf("write pending WAL: %w", err)
	}
	s.logger.Info("settlement plan written",
		"items", len(items), "batches", len(pending.Batches), "debt_entries", len(debtRecords))

	// 7. Submit + confirm; commit cursor; reduce pendingSpend; persist debt; delete WAL.
	if err := s.resumePending(ctx); err != nil {
		return fmt.Errorf("execute settlement: %w", err)
	}

	s.logger.Info("settlement cycle complete",
		"records", len(records),
		"items", len(items),
		"duration_ms", time.Since(t0).Milliseconds(),
	)
	metrics.SettlementCycleDuration.Observe(time.Since(t0).Seconds())
	metrics.SettlementCyclesTotal.WithLabelValues("complete").Inc()
	metrics.SettlementItemsTotal.WithLabelValues("settled").Add(float64(len(items)))
	if len(unresolved) > 0 {
		metrics.SettlementItemsTotal.WithLabelValues("unresolved").Add(float64(len(unresolved)))
	}
	return nil
}

// resumePending executes (or finishes) the settlement described by the WAL file.
// Idempotent: safe to call when no WAL exists.
func (s *Settler) resumePending(ctx context.Context) error {
	pending, ok, err := s.readPending()
	if err != nil {
		return fmt.Errorf("read pending WAL: %w", err)
	}
	if !ok {
		return nil
	}

	s.logger.Info("resuming pending settlement",
		"batches", len(pending.Batches),
		"created_at", pending.CreatedAt,
	)

	// Check operator gas balance once before submitting.
	s.checkOperatorGas(ctx)

	for i := range pending.Batches {
		b := &pending.Batches[i]
		if b.Confirmed {
			continue
		}

		hashBytes, err := hexToHash32(b.DetailsHash)
		if err != nil {
			return fmt.Errorf("bad batch hash %q: %w", b.DetailsHash, err)
		}

		// Dedup: if already processed on-chain (crash after submit), skip. No receipt
		// is at hand here, so the per-item outcome goes unverified for this batch —
		// acceptable for the rare crash-replay window, but say so.
		if processed, err := s.contract.IsProcessedBatch(ctx, hashBytes); err == nil && processed {
			s.logger.Info("batch already processed on-chain, marking confirmed (per-item outcome unverified — crash-replay path)",
				"hash", b.DetailsHash)
			b.Confirmed = true
			if err := s.writePending(*pending); err != nil {
				return fmt.Errorf("persist confirm progress: %w", err)
			}
			continue
		}

		batch, err := b.toBatch(hashBytes)
		if err != nil {
			return fmt.Errorf("decode batch: %w", err)
		}

		tx, err := s.contract.SubmitSettlement(ctx, batch)
		if err != nil {
			metrics.SettlementTxTotal.WithLabelValues("error").Inc()
			return fmt.Errorf("submit batch %d: %w", i, err)
		}
		receipt, err := s.contract.WaitForReceipt(ctx, tx, 5*time.Minute)
		if err != nil {
			// A timeout means the tx is stuck unmined and will be RBF-replaced on
			// the next attempt; other errors include an on-chain revert.
			if errors.Is(err, ErrTxTimeout) {
				metrics.SettlementTxTotal.WithLabelValues("stuck").Inc()
			} else {
				metrics.SettlementTxTotal.WithLabelValues("reverted").Inc()
			}
			return fmt.Errorf("confirm batch %d: %w", i, err)
		}

		// Reorg safety (C2): wait until the tx's block is buried under
		// confirmation_depth blocks before treating the batch as final. If the
		// receipt disappears (reorged away), the batch is left UNCONFIRMED in the WAL
		// and re-submitted — on-chain dedup (processedBatches) makes that safe, so a
		// reorg can never lose or double a settlement.
		depth := 0
		if s.cfg.ConfirmationDepth != nil {
			depth = *s.cfg.ConfirmationDepth
		}
		if depth > 0 {
			finalReceipt, ferr := s.contract.WaitForFinality(ctx, tx.Hash(), uint64(depth), 10*time.Minute)
			if errors.Is(ferr, ErrReorged) {
				// The tx-HASH receipt vanished before reaching finality depth. On Filecoin
				// this is usually NOT a lost settlement: the message can move tipsets so its
				// eth tx-hash receipt reads NotFound even though its on-chain EFFECT
				// persisted. The contract's processedBatches[detailsHash] is the source of
				// truth, so consult it before re-submitting. If the batch IS recorded on
				// chain, the settlement is durably final — treat it as confirmed. Blindly
				// re-submitting here is what a 24h soak caught: it spuriously marked ~90% of
				// cycles "reorged" and, by deferring each confirmation a cycle, halved
				// settlement throughput until pending spend hit the cap and 402'd traffic.
				if processed, perr := s.contract.IsProcessedBatch(ctx, hashBytes); perr == nil && processed {
					s.logger.Info("finality receipt vanished but batch is processed on-chain — treating as final",
						"tx_hash", tx.Hash().Hex(), "batch", i, "hash", b.DetailsHash)
					// fall through to the confirmed path below
				} else {
					// Genuinely absent on-chain → a real reorg dropped it; re-submit (the
					// dedup guard above makes replay safe).
					metrics.SettlementTxTotal.WithLabelValues("reorged").Inc()
					s.logger.Warn("settlement tx reorged away before finality; re-submitting batch",
						"tx_hash", tx.Hash().Hex(), "batch", i, "hash", b.DetailsHash)
					return fmt.Errorf("batch %d reorged, will re-submit: %w", i, ferr)
				}
			} else if ferr != nil {
				return fmt.Errorf("await finality batch %d: %w", i, ferr)
			} else if finalReceipt != nil {
				receipt = finalReceipt
			}
		}
		metrics.SettlementTxTotal.WithLabelValues("confirmed").Inc()

		// Verify the PER-ITEM on-chain outcome (audit finding, medium severity): the contract
		// skips — does not revert on — items whose user balance was drained between
		// plan and execution (e.g. a refund claimed mid-window). Trusting the plan
		// would count that revenue as settled while the chain never moved it. Reverse
		// each failed item out of SettledUSD at its exact planned USD and carry it as
		// debt, BEFORE marking the batch confirmed, so a crash-replay keeps the books.
		s.reverseFailedItems(pending, b, s.contract.ParseSettlementOutcome(receipt))

		b.Confirmed = true
		if err := s.writePending(*pending); err != nil {
			return fmt.Errorf("persist confirm progress: %w", err)
		}

		s.logSettlement(settlementLog{
			TxHash:      tx.Hash().Hex(),
			BlockNumber: receipt.BlockNumber.Uint64(),
			GasUsed:     receipt.GasUsed,
			ItemCount:   len(batch.Users),
			DetailsHash: b.DetailsHash,
			Timestamp:   time.Now(),
		})

		// Per-request settlement ledger (SP per-request earnings): record which
		// request IDs this confirmed batch settled, with the on-chain tx/block, so an
		// SP can later see exactly which inference requests a given on-chain payment
		// covered. Keyed off the batch's items' RequestIDs (persisted in the WAL).
		s.appendItemsLedger(b, tx.Hash().Hex(), receipt.BlockNumber.Uint64())
		// A1: persist the batch's Merkle leaf set so inclusion proofs can be served
		// (public receipt endpoint) long after the WAL is gone.
		s.appendMerkleLedger(b, tx.Hash().Hex(), receipt.BlockNumber.Uint64())
	}

	// All batches confirmed → commit cursor, reduce pendingSpend, delete WAL.
	if err := s.scanner.CommitCursor(pending.CursorToCommit); err != nil {
		return fmt.Errorf("commit cursor: %w", err)
	}
	s.reducePendingSpend(pending.SettledUSD)
	// Maintain a durable running total of USD actually settled on-chain, so the
	// reconciler can compare billed vs settled without re-deriving from token amounts
	// (B4). Keyed by the WAL's CreatedAt so a crash-replay of the SAME WAL does not
	// double-count (idempotent), while a genuinely new cycle always adds.
	s.addSettledTotal(pending.SettledUSD, pending.CreatedAt)
	// Persist the new outstanding-debt ledger atomically with the cursor commit:
	// the WAL still exists until the very end, so a crash here replays both.
	if err := s.saveDebts(pending.RemainingDebts); err != nil {
		s.logger.Error("failed to persist settlement debt ledger", "error", err)
	}
	if err := os.Remove(s.pendingPath); err != nil && !os.IsNotExist(err) {
		s.logger.Warn("failed to delete pending WAL", "error", err)
	}
	metrics.PendingSettlementWAL.Set(0)
	s.balance.ForceRefresh(ctx)
	return nil
}

// reverseFailedItems reconciles a confirmed batch against its decoded on-chain outcome.
// For every item the contract SKIPPED (SettlementItemFailed), the item's exact planned
// USD is subtracted from the WAL's SettledUSD for that wallet (so pendingSpend is not
// reduced for money that never moved) and re-added to RemainingDebts (so it is collected
// next cycle once the balance allows). Mutates `pending`; caller persists it.
func (s *Settler) reverseFailedItems(pending *pendingSettlement, b *pendingBatch, outcome SettlementOutcome) {
	if !outcome.Found {
		// No SettlementExecuted event decodable — cannot verify. Keep the plan's
		// accounting (previous behavior) but say so loudly.
		s.logger.Warn("settlement receipt carried no decodable outcome event; per-item results unverified",
			"hash", b.DetailsHash)
		return
	}
	if outcome.FailedCount == 0 {
		return
	}
	metrics.SettlementItemsTotal.WithLabelValues("onchain_failed").Add(float64(outcome.FailedCount))
	s.logger.Error("on-chain settlement skipped items (user balance drained mid-window); reversing to debt",
		"hash", b.DetailsHash, "failed", outcome.FailedCount, "settled", outcome.SettledCount)

	for _, idx := range outcome.FailedIndexes {
		if idx >= uint64(len(b.Items)) {
			s.logger.Error("on-chain failed index out of range", "index", idx, "items", len(b.Items))
			continue
		}
		it := b.Items[idx]
		usd, _, perr := big.ParseFloat(it.AmountUSD, 10, 128, big.ToNearestEven)
		if it.AmountUSD == "" || perr != nil || usd.Sign() <= 0 {
			// Older WAL without AmountUSD — cannot reverse exactly. The money stays
			// counted as settled (pre-fix behavior); flag it for manual reconcile.
			s.logger.Error("failed item has no planned USD (old WAL?); cannot auto-reverse — reconcile manually",
				"index", idx, "user", it.User, "amount", it.Amount, "token", it.Token)
			continue
		}
		// Find the wallet key in SettledUSD (request-log form) matching the item's
		// EIP-55 address; subtract the failed USD, floored at zero.
		for wallet, sStr := range pending.SettledUSD {
			if !strings.EqualFold(wallet, it.User) {
				continue
			}
			cur, _, e := big.ParseFloat(sStr, 10, 128, big.ToNearestEven)
			if e != nil {
				cur = new(big.Float)
			}
			cur.Sub(cur, usd)
			if cur.Sign() < 0 {
				cur = new(big.Float)
			}
			pending.SettledUSD[wallet] = cur.Text('f', 18)
			pending.RemainingDebts = append(pending.RemainingDebts, debtRecord{
				Wallet: wallet, SPEVM: it.SP, USD: usd.Text('f', 18), RequestIDs: it.RequestIDs,
			})
			break
		}
	}
}

func (s *Settler) checkOperatorGas(ctx context.Context) {
	opBal, err := s.contract.OperatorBalance(ctx)
	if err != nil {
		return
	}
	opBalF, _ := weiToFloat(opBal, 18).Float64()
	metrics.OperatorBalanceFIL.Set(opBalF)
	minBal, _, perr := big.ParseFloat(s.cfg.OperatorMinBalance, 10, 128, big.ToNearestEven)
	if perr != nil {
		return
	}
	if opBal.Cmp(floatToWei(minBal, 18)) < 0 {
		metrics.OperatorBalanceLow.Set(1)
		s.logger.Warn("operator gas balance low",
			"balance", FormatFIL(opBal),
			"threshold", s.cfg.OperatorMinBalance+" FIL",
		)
	} else {
		metrics.OperatorBalanceLow.Set(0)
	}
}

// buildPending splits items into batches and freezes them into a WAL structure.
// recordsByID supplies per-request data for the Merkle leaves (A1).
func (s *Settler) buildPending(items []SettlementItem, cursor Cursor, settledUSD map[string]string, debts []debtRecord, recordsByID map[string]RequestRecord) pendingSettlement {
	batches := splitBatches(items, s.cfg.MaxBatchSize)
	p := pendingSettlement{
		CursorToCommit: cursor,
		SettledUSD:     settledUSD,
		RemainingDebts: debts,
		CreatedAt:      time.Now(),
	}
	for _, batch := range batches {
		// The legacy hash derives purely from the batch's economic content + request
		// IDs (no cursor salt), so a re-scan with a different cursor still dedups.
		// The on-chain detailsHash additionally commits to the receipt Merkle root:
		// sha256(legacy ‖ root) — still fully content-derived (all dedup/crash-replay
		// properties preserved) while binding every request and its worker-signed
		// receipt to the on-chain batch (A1).
		legacy := BatchHash(batch)
		rids, leaves := batchLeaves(batch, recordsByID)
		root := MerkleRoot(leaves)
		combined := CombinedDetailsHash(legacy, root)
		pb := pendingBatch{
			DetailsHash: hex.EncodeToString(combined[:]),
			LegacyHash:  hex.EncodeToString(legacy[:]),
			MerkleRoot:  hex.EncodeToString(root[:]),
		}
		// rid → SP for this batch, built from the in-memory items (no ledger scan).
		ridSP := make(map[string]string)
		for _, it := range batch {
			for _, rid := range it.RequestIDs {
				if _, ok := ridSP[rid]; !ok {
					ridSP[rid] = it.SPEVM.Hex()
				}
			}
		}
		for i, rid := range rids {
			leaf := merkleLeaf{Rid: rid, Leaf: hex.EncodeToString(leaves[i][:]), SP: ridSP[rid]}
			if rec, ok := recordsByID[rid]; ok {
				r := rec // copy — snapshot the billing row (with receipt) into the ledger
				leaf.Record = &r
			}
			pb.Leaves = append(pb.Leaves, leaf)
		}
		for _, it := range batch {
			amountUSD := ""
			if it.AmountUSD != nil {
				amountUSD = it.AmountUSD.Text('f', 18)
			}
			pb.Items = append(pb.Items, pendingItem{
				User:       it.UserEVM.Hex(),
				SP:         it.SPEVM.Hex(),
				Amount:     it.Amount.String(),
				Token:      it.TokenAddr.Hex(),
				AmountUSD:  amountUSD,
				RequestIDs: it.RequestIDs,
			})
		}
		p.Batches = append(p.Batches, pb)
	}
	return p
}

// reducePendingSpend subtracts settled USD from each wallet's pendingSpend.
func (s *Settler) reducePendingSpend(settledUSD map[string]string) {
	for wallet, amtStr := range settledUSD {
		amt, _, err := big.ParseFloat(amtStr, 10, 128, big.ToNearestEven)
		if err != nil {
			continue
		}
		s.balance.SettleSpend(wallet, amt)
	}
}

// restorePendingSpend rebuilds pendingSpend from unsettled records AND outstanding
// carried debt after restart, so the balance gate accounts for both.
// PendingRestored closes once startup crash-recovery has re-reserved pendingSpend from
// unsettled records. The reconciler gates its FIRST pass on this — running earlier reads
// pendingSpend=0 and reports the pre-restart pending as false positive drift.
func (s *Settler) PendingRestored() <-chan struct{} { return s.pendingRestoredCh }

func (s *Settler) markPendingRestored() {
	s.pendingRestoredOnce.Do(func() { close(s.pendingRestoredCh) })
}

func (s *Settler) restorePendingSpend() {
	// Re-reserve outstanding carried debt (under-funded usage already past the cursor).
	carried := s.loadDebts()
	debtByWallet := make(map[string]*big.Float)
	for _, d := range carried {
		s.balance.AddPendingSpend(d.Wallet, d.USD)
		if d.USD != nil && d.USD.Sign() > 0 {
			if cur := debtByWallet[d.Wallet]; cur != nil {
				cur.Add(cur, d.USD)
			} else {
				debtByWallet[d.Wallet] = new(big.Float).Copy(d.USD)
			}
		}
	}
	// Restore debt-based suspension so a restart does not silently un-suspend a
	// wallet that still owes (D3).
	n := s.balance.UpdateDebts(debtByWallet)
	metrics.SuspendedWallets.Set(float64(n))
	// Re-reserve dead-lettered (still-unresolvable) records too — the user already
	// consumed the service, so the balance gate must keep accounting for it until
	// the record resolves and settles (audit MEDIUM fix). Priced via the aggregator's
	// RecordCostUSD — the SAME formula settlement clears pending with — so a restart
	// cannot reintroduce a flat-vs-split residue (see CostBreakdownUSD).
	if dl := s.loadDeadLetter(); len(dl) > 0 {
		s.balance.RestorePendingSpend(dl, s.aggregator.RecordCostUSD)
	}
	records, _, _, err := s.scanner.Peek()
	if err != nil {
		s.logger.Warn("failed to scan for pendingSpend restore", "error", err)
		return
	}
	if len(records) == 0 {
		return
	}
	s.balance.RestorePendingSpend(records, s.aggregator.RecordCostUSD)
}

// --- Settled-total persistence (running cumulative on-chain settled USD, B4) ---

// settledTotalFile is the persisted running total of USD settled on-chain. LastWAL
// records the CreatedAt of the most recently applied WAL so a crash-replay of that
// same WAL is not double-counted.
type settledTotalFile struct {
	TotalUSD string    `json:"total_usd"`
	LastWAL  time.Time `json:"last_wal"`
}

// addSettledTotal adds the per-cycle settled USD to the running total, idempotently
// keyed by the WAL's creation time (a replay of the same WAL is a no-op).
func (s *Settler) addSettledTotal(settledUSD map[string]string, walCreatedAt time.Time) {
	cur := s.readSettledTotal()
	if !walCreatedAt.IsZero() && cur.LastWAL.Equal(walCreatedAt) {
		return // already counted this WAL (crash-replay) — idempotent
	}
	total, _, _ := big.ParseFloat(cur.TotalUSD, 10, 128, big.ToNearestEven)
	if total == nil {
		total = new(big.Float)
	}
	for _, amtStr := range settledUSD {
		if amt, _, err := big.ParseFloat(amtStr, 10, 128, big.ToNearestEven); err == nil {
			total.Add(total, amt)
		}
	}
	out := settledTotalFile{TotalUSD: total.Text('f', 18), LastWAL: walCreatedAt}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	tmp := s.settledTotalPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		s.logger.Warn("failed to write settled-total file", "error", err)
		return
	}
	if err := os.Rename(tmp, s.settledTotalPath); err != nil {
		s.logger.Warn("failed to replace settled-total file", "error", err)
	}
}

func (s *Settler) readSettledTotal() settledTotalFile {
	data, err := os.ReadFile(s.settledTotalPath)
	if err != nil {
		return settledTotalFile{TotalUSD: "0"}
	}
	var f settledTotalFile
	if json.Unmarshal(data, &f) != nil || f.TotalUSD == "" {
		return settledTotalFile{TotalUSD: "0"}
	}
	return f
}

// SettledUSDTotal returns the cumulative USD settled on-chain (for reconciliation, B4).
func (s *Settler) SettledUSDTotal() *big.Float {
	v, _, err := big.ParseFloat(s.readSettledTotal().TotalUSD, 10, 128, big.ToNearestEven)
	if err != nil || v == nil {
		return new(big.Float)
	}
	return v
}

// DebtUSDTotal returns the total outstanding carried debt in USD (for reconciliation, B4).
func (s *Settler) DebtUSDTotal() *big.Float {
	total := new(big.Float)
	for _, d := range s.loadDebts() {
		if d.USD != nil {
			total.Add(total, d.USD)
		}
	}
	return total
}

// NewReconciler builds a three-way billing reconciler wired to this settler's
// request log, dead-letter file, pricing path, balance cache, and settled/debt
// totals (B4). toleranceUSD nil → 1-cent default. The billed side is priced with
// the aggregator's RecordCostUSD — identical to what settlement actually clears —
// so the drift alert can see a reserve-vs-settle mismatch instead of absorbing it.
func (s *Settler) NewReconciler(toleranceUSD *big.Float, logger *slog.Logger) *Reconciler {
	return NewReconciler(
		s.requestLogPath,
		s.deadLetterPath,
		filepath.Dir(s.deadLetterPath), // dataDir: reconciler's own cursor + state live here
		s.aggregator.RecordCostUSD,
		s.balance,
		s, // *Settler implements settledTotaler
		toleranceUSD,
		logger,
	)
}

// --- Debt ledger persistence (carried under-funded shortfall) ---

func (s *Settler) loadDebts() []CarriedDebt {
	data, err := os.ReadFile(s.debtPath)
	if err != nil {
		return nil // not-exist or unreadable → no carried debt
	}
	var recs []debtRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		s.logger.Warn("failed to parse debt ledger, ignoring", "error", err)
		return nil
	}
	out := make([]CarriedDebt, 0, len(recs))
	for _, r := range recs {
		usd, _, perr := big.ParseFloat(r.USD, 10, 128, big.ToNearestEven)
		if perr != nil || usd.Sign() <= 0 {
			continue
		}
		out = append(out, CarriedDebt{Wallet: r.Wallet, SPEVM: r.SPEVM, USD: usd, RequestIDs: r.RequestIDs})
	}
	return out
}

func (s *Settler) saveDebts(debts []debtRecord) error {
	s.updateDebtMetrics(debts)
	// Refresh the gateway's suspension set from the current debt ledger (D3): a
	// wallet over the debt threshold is suspended; one whose debt was just collected
	// is automatically un-suspended. Aggregated per wallet across all its SPs.
	debtByWallet := make(map[string]*big.Float)
	for _, d := range debts {
		v, _, err := big.ParseFloat(d.USD, 10, 128, big.ToNearestEven)
		if err != nil || v.Sign() <= 0 {
			continue
		}
		if cur := debtByWallet[d.Wallet]; cur != nil {
			cur.Add(cur, v)
		} else {
			debtByWallet[d.Wallet] = v
		}
	}
	n := s.balance.UpdateDebts(debtByWallet)
	metrics.SuspendedWallets.Set(float64(n))

	if len(debts) == 0 {
		if err := os.Remove(s.debtPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(debts, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.debtPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.debtPath)
}

// updateDebtMetrics sets the carried-debt gauges from the persisted debt set.
func (s *Settler) updateDebtMetrics(debts []debtRecord) {
	total := new(big.Float)
	for _, d := range debts {
		if v, _, err := big.ParseFloat(d.USD, 10, 128, big.ToNearestEven); err == nil {
			total.Add(total, v)
		}
	}
	usd, _ := total.Float64()
	metrics.DebtEntries.Set(float64(len(debts)))
	metrics.DebtUSD.Set(usd)
}

func carriedDebtsToRecords(ds []CarriedDebt) []debtRecord {
	if len(ds) == 0 {
		return nil
	}
	out := make([]debtRecord, 0, len(ds))
	for _, d := range ds {
		usd := "0"
		if d.USD != nil {
			usd = d.USD.Text('f', 18)
		}
		out = append(out, debtRecord{Wallet: d.Wallet, SPEVM: d.SPEVM, USD: usd, RequestIDs: d.RequestIDs})
	}
	return out
}

func floatMapToStrings(m map[string]*big.Float) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if v == nil {
			continue
		}
		out[k] = v.Text('f', 18)
	}
	return out
}

// loadDeadLetter reads all records currently parked in the dead-letter file.
func (s *Settler) loadDeadLetter() []RequestRecord {
	f, err := os.Open(s.deadLetterPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []RequestRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec RequestRecord
		if json.Unmarshal(line, &rec) == nil {
			out = append(out, rec)
		}
	}
	return out
}

// writeDeadLetter atomically replaces the dead-letter file with the given records
// (removing it when empty), so records that became resolvable drop out on reprocess.
func (s *Settler) writeDeadLetter(records []RequestRecord) {
	metrics.DeadLetterEntries.Set(float64(len(records)))
	if len(records) == 0 {
		if err := os.Remove(s.deadLetterPath); err != nil && !os.IsNotExist(err) {
			s.logger.Warn("failed to clear dead-letter file", "error", err)
		}
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, rec := range records {
		_ = enc.Encode(rec)
	}
	tmp := s.deadLetterPath + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0644); err != nil {
		s.logger.Error("failed to write dead-letter file", "error", err)
		return
	}
	if err := os.Rename(tmp, s.deadLetterPath); err != nil {
		s.logger.Error("failed to replace dead-letter file", "error", err)
	}
}

func splitBatches(items []SettlementItem, maxSize int) [][]SettlementItem {
	if maxSize <= 0 {
		maxSize = 50
	}
	// No items → NO batches. Returning a single empty batch here would submit an
	// empty settlement on-chain, which the contract rejects with require(len > 0),
	// reverting the transaction and leaving the WAL to retry forever. A zero-item
	// cycle must still advance the cursor / reduce pendingSpend, but it does that
	// through the batch-less WAL, not by submitting anything (audit fix).
	if len(items) == 0 {
		return nil
	}
	if len(items) <= maxSize {
		return [][]SettlementItem{items}
	}
	var batches [][]SettlementItem
	for i := 0; i < len(items); i += maxSize {
		end := i + maxSize
		if end > len(items) {
			end = len(items)
		}
		batches = append(batches, items[i:end])
	}
	return batches
}

// --- Pending WAL persistence ---

type pendingSettlement struct {
	CursorToCommit Cursor            `json:"cursor_to_commit"`
	SettledUSD     map[string]string `json:"settled_usd"`     // wallet → ACTUAL on-chain settled USD
	RemainingDebts []debtRecord      `json:"remaining_debts"` // under-funded shortfall to persist on confirm
	Batches        []pendingBatch    `json:"batches"`
	CreatedAt      time.Time         `json:"created_at"`
}

// debtRecord is the persisted form of a CarriedDebt (USD as decimal string).
type debtRecord struct {
	Wallet     string   `json:"wallet"`
	SPEVM      string   `json:"sp_evm"`
	USD        string   `json:"usd"`
	RequestIDs []string `json:"request_ids,omitempty"`
}

type pendingBatch struct {
	DetailsHash string        `json:"details_hash"` // hex (no 0x) — sha256(legacy ‖ merkle_root), the on-chain value
	Confirmed   bool          `json:"confirmed"`
	Items       []pendingItem `json:"items"`
	// A1 Merkle commitment components (omitempty: an older WAL lacks them and its
	// stored DetailsHash is still used verbatim on replay — fully backward safe).
	LegacyHash string       `json:"legacy_hash,omitempty"`
	MerkleRoot string       `json:"merkle_root,omitempty"`
	Leaves     []merkleLeaf `json:"leaves,omitempty"`
}

// merkleLeaf pins one request's leaf inside a batch's Merkle tree (leaves are sorted
// by request_id; index = position in this slice).
type merkleLeaf struct {
	Rid  string `json:"rid"`
	Leaf string `json:"leaf"` // hex sha256
	// Record + SP make the merkle ledger SELF-SUFFICIENT for serving a receipt-proof (F6):
	// snapshotted at settlement time so BuildReceiptProof never has to scan the (rotating,
	// 500MB+) request log or the 1GB items ledger. Optional/omitempty — a pre-F6 ledger
	// entry lacks them and BuildReceiptProof falls back to the legacy scans for those rids.
	Record *RequestRecord `json:"record,omitempty"`
	SP     string         `json:"sp,omitempty"`
}

type pendingItem struct {
	User   string `json:"user"`
	SP     string `json:"sp"`
	Amount string `json:"amount"`
	Token  string `json:"token"`
	// AmountUSD is the exact planned USD of this item (plan-time FIL price), used to
	// reverse a per-item ON-CHAIN failure out of SettledUSD and carry it as debt.
	// Optional/omitempty: an older WAL lacks it → the reversal degrades to a warning.
	AmountUSD string `json:"amount_usd,omitempty"`
	// RequestIDs are the billable request IDs aggregated into this item. Persisted so
	// that, on confirmation, each settled request can be written to the per-request
	// settlement ledger (settlement-items.jsonl) for SP per-request earnings queries.
	// Optional/omitempty: an older WAL (pre-feature) lacks it and degrades gracefully.
	RequestIDs []string `json:"request_ids,omitempty"`
}

func (pb pendingBatch) toBatch(hash [32]byte) (SettlementBatch, error) {
	b := SettlementBatch{
		Users:       make([]common.Address, len(pb.Items)),
		SPs:         make([]common.Address, len(pb.Items)),
		Amounts:     make([]*big.Int, len(pb.Items)),
		Tokens:      make([]common.Address, len(pb.Items)),
		DetailsHash: hash,
	}
	for i, it := range pb.Items {
		amt, ok := new(big.Int).SetString(it.Amount, 10)
		if !ok {
			return b, fmt.Errorf("invalid amount %q", it.Amount)
		}
		b.Users[i] = common.HexToAddress(it.User)
		b.SPs[i] = common.HexToAddress(it.SP)
		b.Amounts[i] = amt
		b.Tokens[i] = common.HexToAddress(it.Token)
	}
	return b, nil
}

func (s *Settler) writePending(p pendingSettlement) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.pendingPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.pendingPath); err != nil {
		return err
	}
	metrics.PendingSettlementWAL.Set(1)
	return nil
}

func (s *Settler) readPending() (*pendingSettlement, bool, error) {
	data, err := os.ReadFile(s.pendingPath)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var p pendingSettlement
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, false, err
	}
	return &p, true, nil
}

func hexToHash32(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, err
	}
	if len(b) != 32 {
		return out, fmt.Errorf("expected 32 bytes, got %d", len(b))
	}
	copy(out[:], b)
	return out, nil
}

// --- Settlement audit log ---

type settlementLog struct {
	TxHash      string    `json:"tx_hash"`
	BlockNumber uint64    `json:"block_number"`
	GasUsed     uint64    `json:"gas_used"`
	ItemCount   int       `json:"item_count"`
	DetailsHash string    `json:"details_hash"`
	Timestamp   time.Time `json:"timestamp"`
}

// SettlementAudit is one entry from the local settlement audit log
// (settlements.jsonl). Shares JSON tags with settlementLog.
type SettlementAudit struct {
	TxHash      string    `json:"tx_hash"`
	BlockNumber uint64    `json:"block_number"`
	GasUsed     uint64    `json:"gas_used"`
	ItemCount   int       `json:"item_count"`
	DetailsHash string    `json:"details_hash"`
	Timestamp   time.Time `json:"timestamp"`
}

// FindSettlementByHash scans the local audit log for the entry whose detailsHash
// matches (case-insensitive, 0x-prefix tolerant). Used to reconcile an on-chain
// batch against the local record.
func (s *Settler) FindSettlementByHash(detailsHash string) (SettlementAudit, bool) {
	want := strings.ToLower(strings.TrimPrefix(detailsHash, "0x"))
	f, err := os.Open(s.settlementLogPath)
	if err != nil {
		return SettlementAudit{}, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var entry SettlementAudit
		if json.Unmarshal(sc.Bytes(), &entry) != nil {
			continue
		}
		if strings.ToLower(strings.TrimPrefix(entry.DetailsHash, "0x")) == want {
			return entry, true
		}
	}
	return SettlementAudit{}, false
}

func (s *Settler) logSettlement(entry settlementLog) {
	data, err := json.Marshal(entry)
	if err != nil {
		s.logger.Error("failed to marshal settlement log", "error", err)
		return
	}
	f, err := os.OpenFile(s.settlementLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		s.logger.Error("failed to open settlement log", "error", err)
		return
	}
	defer f.Close()
	f.Write(append(data, '\n'))
}

// settlementItemRecord is one line in settlement-items.jsonl: a single billable
// request that was settled on-chain, with the SP it paid and the on-chain tx. The
// per-request USD earning is NOT stored here — it is computed at query time from the
// request log via the SAME pricing path settlement uses (RecordEarningUSD), so the
// detail view can never drift from how billing actually works.
type settlementItemRecord struct {
	RequestID   string    `json:"request_id"`
	SPEVM       string    `json:"sp_evm"`
	User        string    `json:"user"`
	Token       string    `json:"token"`
	TxHash      string    `json:"tx_hash"`
	DetailsHash string    `json:"details_hash"`
	BlockNumber uint64    `json:"block_number"`
	SettledAt   time.Time `json:"settled_at"`
}

// appendItemsLedger writes one settlement-items.jsonl line per request_id settled by
// this confirmed batch. Idempotent: if the batch's details_hash is already present
// (crash-replay of the same WAL), it does nothing — so a re-run never double-records.
func (s *Settler) appendItemsLedger(b *pendingBatch, txHash string, block uint64) {
	if b == nil {
		return
	}
	// Idempotency: skip if this details_hash was already ledgered.
	if s.itemsLedgerHasHash(b.DetailsHash) {
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	now := time.Now()
	for _, it := range b.Items {
		for _, rid := range it.RequestIDs {
			_ = enc.Encode(settlementItemRecord{
				RequestID:   rid,
				SPEVM:       it.SP,
				User:        it.User,
				Token:       it.Token,
				TxHash:      txHash,
				DetailsHash: b.DetailsHash,
				BlockNumber: block,
				SettledAt:   now,
			})
		}
	}
	if buf.Len() == 0 {
		return // older WAL without RequestIDs, or empty batch — nothing to record
	}
	f, err := os.OpenFile(s.itemsLedgerPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		s.logger.Error("failed to open settlement items ledger", "error", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(buf.Bytes()); err != nil {
		s.logger.Error("failed to write settlement items ledger", "error", err)
	}
}

// merkleBatchRecord is one line of merkle-batches.jsonl: everything needed to serve
// an inclusion proof for any request in a confirmed batch, plus the components an
// external verifier needs to recompute the on-chain detailsHash.
type merkleBatchRecord struct {
	DetailsHash string       `json:"details_hash"`
	LegacyHash  string       `json:"legacy_hash"`
	MerkleRoot  string       `json:"merkle_root"`
	TxHash      string       `json:"tx_hash"`
	BlockNumber uint64       `json:"block_number"`
	Leaves      []merkleLeaf `json:"leaves"`
}

// appendMerkleLedger persists a confirmed batch's Merkle data. Idempotent by
// details_hash (crash-replay safe); a batch without leaves (old WAL) is skipped.
func (s *Settler) appendMerkleLedger(b *pendingBatch, txHash string, block uint64) {
	if b == nil || len(b.Leaves) == 0 {
		return
	}
	path := s.merkleLedgerPath()
	if ledgerHasHash(path, b.DetailsHash) {
		return
	}
	// R4: externalize each leaf's RequestRecord to receipt-records.jsonl, remember its
	// (offset,len), and persist only the THIN leaf {rid, leaf, sp} in the batch line.
	recLocs := make(map[string]recordLoc, len(b.Leaves))
	rids := make([]string, len(b.Leaves))
	thin := make([]merkleLeaf, len(b.Leaves))
	for i, l := range b.Leaves {
		thin[i] = merkleLeaf{Rid: l.Rid, Leaf: l.Leaf, SP: l.SP} // no embedded Record
		rids[i] = l.Rid
		if l.Record != nil {
			if loc, ok := s.appendReceiptRecord(l.Rid, l.Record); ok {
				recLocs[l.Rid] = loc
			}
		}
	}

	data, err := json.Marshal(merkleBatchRecord{
		DetailsHash: b.DetailsHash, LegacyHash: b.LegacyHash, MerkleRoot: b.MerkleRoot,
		TxHash: txHash, BlockNumber: block, Leaves: thin,
	})
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		s.logger.Error("failed to open merkle ledger", "error", err)
		return
	}
	defer f.Close()
	// Offset where this line lands (single-writer settler; O_APPEND write == current end).
	off, _ := f.Seek(0, io.SeekEnd)
	if _, werr := f.Write(append(data, '\n')); werr != nil {
		return
	}
	// Keep the warm indexes consistent, and persist the sidecar in lockstep.
	s.merkleIdxMu.Lock()
	if s.merkleIdx != nil {
		for _, rid := range rids {
			s.merkleIdx[rid] = off
		}
	}
	if s.recordIdx != nil {
		for rid, loc := range recLocs {
			s.recordIdx[rid] = loc
		}
	}
	s.appendReceiptIdxLine(off, rids, recLocs)
	s.merkleIdxMu.Unlock()
}

func (s *Settler) merkleLedgerPath() string {
	// Lives next to the other ledgers (same dataDir as settlement-items.jsonl).
	return strings.TrimSuffix(s.itemsLedgerPath, "settlement-items.jsonl") + "merkle-batches.jsonl"
}

// findMerkleBatch returns the merkle record containing rid. The index is warmed with EVERY
// settled rid (sidecar replay or one-time rebuild), so an index miss is authoritative: the
// rid was never settled. It therefore never falls back to scanning the whole growing ledger
// — that scan was a ~56s DoS surface on the public query port (R4). The only retry is a
// single re-warm if an indexed offset is somehow unreadable (should not happen).
func (s *Settler) findMerkleBatch(rid string) (merkleBatchRecord, bool) {
	s.merkleIdxMu.Lock()
	if !s.merkleWarm {
		s.loadOrWarmIndexesLocked()
	}
	off, ok := s.merkleIdx[rid]
	s.merkleIdxMu.Unlock()
	if !ok {
		return merkleBatchRecord{}, false // warm index ⇒ miss means "not settled"
	}
	if rec, ok := s.readMerkleBatchAt(off, rid); ok {
		return rec, true
	}
	// Indexed but unreadable at that offset: rebuild once from the data files and retry.
	s.merkleIdxMu.Lock()
	s.rebuildFromDataLocked()
	off, ok = s.merkleIdx[rid]
	s.merkleIdxMu.Unlock()
	if ok {
		if rec, ok := s.readMerkleBatchAt(off, rid); ok {
			return rec, true
		}
	}
	return merkleBatchRecord{}, false
}

// readMerkleBatchAt reads the single batch line at offset and returns it if it holds rid.
func (s *Settler) readMerkleBatchAt(off int64, rid string) (merkleBatchRecord, bool) {
	f, err := os.Open(s.merkleLedgerPath())
	if err != nil {
		return merkleBatchRecord{}, false
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return merkleBatchRecord{}, false
	}
	line, err := bufio.NewReaderSize(f, 1<<20).ReadBytes('\n')
	if len(line) == 0 && err != nil {
		return merkleBatchRecord{}, false
	}
	var rec merkleBatchRecord
	if json.Unmarshal(bytes.TrimSpace(line), &rec) != nil {
		return merkleBatchRecord{}, false
	}
	for _, l := range rec.Leaves {
		if l.Rid == rid {
			return rec, true
		}
	}
	return merkleBatchRecord{}, false
}

// ledgerHasHash reports whether a JSONL ledger already contains a details_hash line.
func ledgerHasHash(path, detailsHash string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	needle := []byte(`"details_hash":"` + detailsHash + `"`)
	for sc.Scan() {
		if bytes.Contains(sc.Bytes(), needle) {
			return true
		}
	}
	return false
}

// itemsLedgerHasHash reports whether the ledger already contains any line for the
// given batch details_hash (used for idempotent appends).
func (s *Settler) itemsLedgerHasHash(detailsHash string) bool {
	f, err := os.Open(s.itemsLedgerPath)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	needle := []byte(`"details_hash":"` + detailsHash + `"`)
	for sc.Scan() {
		if bytes.Contains(sc.Bytes(), needle) {
			return true
		}
	}
	return false
}

// parseModelPrices converts the config's USD-per-1M-token prices to USD-per-token.
func parseModelPrices(cfg *Config, logger *slog.Logger) map[string]*big.Float {
	prices := make(map[string]*big.Float)
	for model, priceStr := range cfg.ModelPricesUSD {
		price, _, err := big.ParseFloat(priceStr, 10, 128, big.ToNearestEven)
		if err != nil {
			logger.Warn("invalid model price, skipping", "model", model, "price", priceStr)
			continue
		}
		price.Quo(price, big.NewFloat(1_000_000))
		prices[model] = price
	}
	return prices
}
