package settlement

import (
	"bytes"
	"context"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// mockContract implements settlementContract for unit-testing the Settler lifecycle.
type mockContract struct {
	processed   map[[32]byte]bool // hashes considered already on-chain
	submitCount int
	submitted   [][32]byte
	opBalance   *big.Int

	// Failure injection (all default to "no failure"):
	submitErr     error // SubmitSettlement returns this without marking the batch processed
	receiptErr    error // WaitForReceipt returns this (transaction "stuck"/timeout)
	receiptFail   bool  // WaitForReceipt returns a reverted (status=failed) receipt
	finalityErr   error // WaitForFinality returns this (e.g. ErrReorged)
	finalityCalls int   // how many times WaitForFinality was invoked
	// submitMarksProcessed models whether a submit's on-chain EFFECT durably lands: true
	// (default) = normal; false = simulate a real reorg that drops the tx AND its effect
	// (processedBatches stays false), distinct from a Filecoin false-positive where the
	// tx-hash receipt vanishes but the batch IS processed on-chain.
	submitMarksProcessed bool
	// outcome is what ParseSettlementOutcome reports. Default (zero FailedCount,
	// Found=true) = every item settled. Set FailedCount/FailedIndexes to simulate the
	// contract SKIPPING items (user balance drained mid-window).
	outcome *SettlementOutcome
}

func newMockContract() *mockContract {
	return &mockContract{processed: make(map[[32]byte]bool), submitMarksProcessed: true}
}

func (m *mockContract) IsProcessedBatch(ctx context.Context, h [32]byte) (bool, error) {
	return m.processed[h], nil
}

func (m *mockContract) SubmitSettlement(ctx context.Context, batch SettlementBatch) (*types.Transaction, error) {
	if m.submitErr != nil {
		return nil, m.submitErr // simulate a submit failure: NOT marked processed
	}
	m.submitCount++
	m.submitted = append(m.submitted, batch.DetailsHash)
	if m.submitMarksProcessed {
		m.processed[batch.DetailsHash] = true // simulate the settlement's on-chain effect landing
	}
	tx := types.NewTx(&types.LegacyTx{Nonce: uint64(m.submitCount), Gas: 21000, GasPrice: big.NewInt(1), Value: big.NewInt(0)})
	return tx, nil
}

func (m *mockContract) WaitForReceipt(ctx context.Context, tx *types.Transaction, timeout time.Duration) (*types.Receipt, error) {
	if m.receiptErr != nil {
		return nil, m.receiptErr
	}
	status := types.ReceiptStatusSuccessful
	if m.receiptFail {
		status = types.ReceiptStatusFailed
	}
	return &types.Receipt{Status: status, BlockNumber: big.NewInt(1), GasUsed: 21000}, nil
}

func (m *mockContract) OperatorBalance(ctx context.Context) (*big.Int, error) {
	if m.opBalance != nil {
		return m.opBalance, nil
	}
	return fil(100), nil
}

func (m *mockContract) WaitForFinality(ctx context.Context, txHash common.Hash, confirmations uint64, timeout time.Duration) (*types.Receipt, error) {
	m.finalityCalls++
	if m.finalityErr != nil {
		return nil, m.finalityErr
	}
	return &types.Receipt{Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(1), GasUsed: 21000}, nil
}

func (m *mockContract) ParseSettlementOutcome(receipt *types.Receipt) SettlementOutcome {
	if m.outcome != nil {
		return *m.outcome
	}
	return SettlementOutcome{Found: true} // default: all items settled, none failed
}

func newTestSettler(t *testing.T, mock *mockContract, logger *slog.Logger) (*Settler, string) {
	t.Helper()
	dir := t.TempDir()
	reqLog := filepath.Join(dir, "requests.jsonl")
	cfg := coverageCfg()
	cfg.MaxBatchSize = 50
	cfg.OperatorMinBalance = "0.1"
	cfg.FILPriceUSD = "2.0"
	cfg.FILPriceSource = "manual"
	cfg.IntervalMinutes = 60
	pricer := NewPricer(cfg, logger)
	bc := NewBalanceCache(nil, cfg.SupportedTokens, pricer, 30, logger)
	s := NewSettler(cfg, mock, pricer, bc, nil, reqLog, dir, logger)
	return s, dir
}

func mkItem(amount *big.Int, token string) SettlementItem {
	return SettlementItem{
		UserWallet: walletU,
		UserEVM:    common.HexToAddress(walletU),
		SPEVM:      common.HexToAddress(sp1Addr),
		Amount:     amount,
		TokenAddr:  common.HexToAddress(token),
	}
}

func walExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "pending-settlement.json"))
	return err == nil
}

// TestResumePendingNoWAL: with no WAL file, resume is a no-op.
func TestResumePendingNoWAL(t *testing.T) {
	mock := newMockContract()
	s, _ := newTestSettler(t, mock, discardLogger())
	if err := s.resumePending(context.Background()); err != nil {
		t.Fatalf("resume with no WAL should be nil, got %v", err)
	}
	if mock.submitCount != 0 {
		t.Errorf("expected 0 submits, got %d", mock.submitCount)
	}
}

// TestResumePendingSubmitsAndCleansUp: a fresh WAL is submitted, confirmed, cursor
// committed, and the WAL deleted.
func TestResumePendingSubmitsAndCleansUp(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())

	items := []SettlementItem{mkItem(usdc(5), usdcAddr)}
	p := s.buildPending(items, Cursor{Offset: 100, FileSize: 100}, map[string]string{walletU: "5"}, nil, nil)
	if err := s.writePending(p); err != nil {
		t.Fatal(err)
	}

	if err := s.resumePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.submitCount != 1 {
		t.Errorf("expected exactly 1 submit, got %d", mock.submitCount)
	}
	if walExists(dir) {
		t.Error("WAL should be deleted after successful settlement")
	}
	if _, err := os.Stat(filepath.Join(dir, "settlement-cursor.json")); err != nil {
		t.Errorf("cursor should be committed, stat err: %v", err)
	}
}

// TestResumePendingIdempotentAfterCrash is THE double-charge regression: if a batch
// was already submitted on-chain before a crash, resume must detect it via
// IsProcessedBatch and NOT submit again.
func TestResumePendingIdempotentAfterCrash(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())

	items := []SettlementItem{mkItem(usdc(5), usdcAddr)}
	p := s.buildPending(items, Cursor{Offset: 100, FileSize: 100}, map[string]string{walletU: "5"}, nil, nil)
	if err := s.writePending(p); err != nil {
		t.Fatal(err)
	}
	// Simulate: this batch was submitted on-chain, then we crashed before marking it.
	h, err := hexToHash32(p.Batches[0].DetailsHash)
	if err != nil {
		t.Fatal(err)
	}
	mock.processed[h] = true

	if err := s.resumePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.submitCount != 0 {
		t.Errorf("DOUBLE-CHARGE: expected 0 submits (already processed), got %d", mock.submitCount)
	}
	if walExists(dir) {
		t.Error("WAL should be deleted after dedup-confirm")
	}
}

// TestResumePendingSkipsConfirmedBatch: a batch already marked Confirmed in the WAL
// (persisted before a crash) is skipped; only the unconfirmed batch is submitted.
func TestResumePendingSkipsConfirmedBatch(t *testing.T) {
	mock := newMockContract()
	s, _ := newTestSettler(t, mock, discardLogger())
	s.cfg.MaxBatchSize = 1 // force 2 items -> 2 batches

	items := []SettlementItem{mkItem(usdc(5), usdcAddr), mkItem(usdc(7), usdcAddr)}
	p := s.buildPending(items, Cursor{Offset: 100, FileSize: 100}, map[string]string{walletU: "12"}, nil, nil)
	if len(p.Batches) != 2 {
		t.Fatalf("expected 2 batches, got %d", len(p.Batches))
	}
	p.Batches[0].Confirmed = true // already done before crash
	if err := s.writePending(p); err != nil {
		t.Fatal(err)
	}

	if err := s.resumePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.submitCount != 1 {
		t.Fatalf("expected only the unconfirmed batch submitted (1), got %d", mock.submitCount)
	}
	want, _ := hexToHash32(p.Batches[1].DetailsHash)
	if mock.submitted[0] != want {
		t.Error("the wrong batch was submitted (should be batch[1], the unconfirmed one)")
	}
}

// TestResumePendingReducesPendingSpend: after on-chain confirmation, pendingSpend
// drops by the settled amount.
func TestResumePendingReducesPendingSpend(t *testing.T) {
	mock := newMockContract()
	s, _ := newTestSettler(t, mock, discardLogger())
	// seed a balance so Reserve succeeds, then reserve $5 of pending spend
	s.balance.chainBalances[walletU] = map[string]*big.Int{usdcAddr: usdc(100)}
	if !s.balance.Reserve(walletU, big.NewFloat(5)) {
		t.Fatal("reserve failed")
	}

	items := []SettlementItem{mkItem(usdc(5), usdcAddr)}
	p := s.buildPending(items, Cursor{Offset: 100, FileSize: 100}, map[string]string{walletU: "5"}, nil, nil)
	if err := s.writePending(p); err != nil {
		t.Fatal(err)
	}
	if err := s.resumePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ps := s.balance.GetPendingSpend(walletU); ps.Sign() != 0 {
		t.Errorf("expected pendingSpend reduced to 0 after settle, got %s", ps.Text('f', 6))
	}
}

// TestReadPendingNotExist covers the not-exist branch of readPending.
func TestReadPendingNotExist(t *testing.T) {
	s, _ := newTestSettler(t, newMockContract(), discardLogger())
	p, ok, err := s.readPending()
	if err != nil || ok || p != nil {
		t.Errorf("expected (nil,false,nil), got (%v,%v,%v)", p, ok, err)
	}
}

// TestWALRoundTrip covers writePending/readPending fidelity.
func TestWALRoundTrip(t *testing.T) {
	s, _ := newTestSettler(t, newMockContract(), discardLogger())
	items := []SettlementItem{mkItem(usdc(5), usdcAddr)}
	p := s.buildPending(items, Cursor{Offset: 42, FileSize: 99}, map[string]string{walletU: "5"}, nil, nil)
	if err := s.writePending(p); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.readPending()
	if err != nil || !ok {
		t.Fatalf("readPending failed: ok=%v err=%v", ok, err)
	}
	if got.CursorToCommit.Offset != 42 || got.CursorToCommit.FileSize != 99 {
		t.Errorf("cursor not round-tripped: %+v", got.CursorToCommit)
	}
	if len(got.Batches) != 1 || got.Batches[0].DetailsHash != p.Batches[0].DetailsHash {
		t.Error("batch not round-tripped")
	}
}

func TestSplitBatchesDefaultsMaxSize(t *testing.T) {
	if got := len(splitBatches(make([]SettlementItem, 3), 0)); got != 1 {
		t.Errorf("maxSize<=0 should default to 50 => 1 batch, got %d", got)
	}
	if got := len(splitBatches(make([]SettlementItem, 3), -5)); got != 1 {
		t.Errorf("negative maxSize should default => 1 batch, got %d", got)
	}
}

func TestDeadLetterWritesRecords(t *testing.T) {
	s, dir := newTestSettler(t, newMockContract(), discardLogger())
	s.writeDeadLetter([]RequestRecord{
		{RequestID: "x", Wallet: walletU, WorkerID: "ghost", Status: 200, TotalTokens: 5},
	})
	data, err := os.ReadFile(filepath.Join(dir, "settlement-deadletter.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"request_id":"x"`) || !strings.Contains(string(data), "ghost") {
		t.Errorf("dead-letter file missing record: %s", data)
	}
}

// TestDeadLetterRoundTripAndClear covers the audit MEDIUM fix: dead-lettered records
// can be re-loaded for a retry, and re-writing an empty set atomically clears the file
// (so records that became resolvable on reprocess don't linger forever).
func TestDeadLetterRoundTripAndClear(t *testing.T) {
	s, dir := newTestSettler(t, newMockContract(), discardLogger())
	path := filepath.Join(dir, "settlement-deadletter.jsonl")

	in := []RequestRecord{
		{RequestID: "a", Wallet: walletU, WorkerID: "ghost1", Status: 200, TotalTokens: 5},
		{RequestID: "b", Wallet: walletU, WorkerID: "ghost2", Status: 200, TotalTokens: 7},
	}
	s.writeDeadLetter(in)

	got := s.loadDeadLetter()
	if len(got) != 2 {
		t.Fatalf("expected 2 dead-lettered records back, got %d", len(got))
	}
	if got[0].RequestID != "a" || got[1].RequestID != "b" || got[1].TotalTokens != 7 {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Writing an empty set must remove the file (all records became resolvable).
	s.writeDeadLetter(nil)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("dead-letter file should have been removed when written empty, stat err=%v", err)
	}
	if leftover := s.loadDeadLetter(); len(leftover) != 0 {
		t.Errorf("expected no records after clear, got %d", len(leftover))
	}
}

// TestDeadLetterReprocessResolvesWhenMappingAppears: a record dead-lettered because its
// worker had no SP mapping is collected on a later cycle once the mapping exists, and
// drops out of the dead-letter file (audit MEDIUM fix — no silently lost SP revenue).
func TestDeadLetterReprocessResolvesWhenMappingAppears(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	path := filepath.Join(dir, "settlement-deadletter.jsonl")

	// Seed a dead-lettered record. coverageCfg prices "default" at $1/token, so 5
	// tokens = $5; SPAddressMap maps miner1→sp1Addr.
	rec := RequestRecord{
		RequestID: "late", Wallet: walletU, WorkerID: "w1", Model: "default",
		Status: 200, TotalTokens: 5,
	}
	s.writeDeadLetter([]RequestRecord{rec})

	// Give the settler a resolver that now knows w1→miner1, plus a funded FIL balance.
	// FIL price is $2.0 (manual), so $5 of cost needs 2.5 FIL — 100 FIL is plenty.
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(100)}

	// No new requests.jsonl traffic — the cycle must still run because a dead-letter
	// record is outstanding (the quiet-period reprocess fix).
	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("Settle returned error: %v", err)
	}

	// The dead-letter file must now be gone (record resolved + settled).
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("dead-letter file should be cleared after the record resolved, stat err=%v", err)
	}
	if len(mock.submitted) == 0 {
		t.Error("expected the reprocessed dead-letter record to be submitted on-chain")
	}
}

// staticResolver is a fixed worker_id → MinerAddress map for tests.
type staticResolver map[string]string

func (r staticResolver) GetWorkerSPMap() map[string]string { return r }

func TestHexToHash32Errors(t *testing.T) {
	if _, err := hexToHash32("zzzz"); err == nil {
		t.Error("expected error for non-hex input")
	}
	if _, err := hexToHash32("abcd"); err == nil {
		t.Error("expected error for wrong-length hash (2 bytes)")
	}
	valid := strings.Repeat("ab", 32) // 32 bytes
	if _, err := hexToHash32(valid); err != nil {
		t.Errorf("valid 32-byte hash should decode, got %v", err)
	}
}

// TestCheckOperatorGasWarnsWhenLow asserts the low-gas warning fires (and does not
// fire when the balance is healthy).
func TestCheckOperatorGasWarnsWhenLow(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	mock := newMockContract()
	mock.opBalance = big.NewInt(0) // below the 0.1 FIL threshold
	s, _ := newTestSettler(t, mock, logger)

	s.checkOperatorGas(context.Background())
	if !strings.Contains(buf.String(), "operator gas balance low") {
		t.Errorf("expected low-gas warning, log was: %s", buf.String())
	}

	buf.Reset()
	mock.opBalance = fil(5) // healthy
	s.checkOperatorGas(context.Background())
	if strings.Contains(buf.String(), "operator gas balance low") {
		t.Errorf("did not expect warning for healthy balance, log was: %s", buf.String())
	}
}
