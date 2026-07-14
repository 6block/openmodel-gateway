package settlement

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeRequestLog appends the given records to the settler's request log as JSONL.
func writeRequestLog(t *testing.T, path string, recs []RequestRecord) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
}

func billableRecord(id, worker string, tokens int) RequestRecord {
	return RequestRecord{
		RequestID: id, Timestamp: time.Now(), Wallet: walletU, WorkerID: worker,
		Model: "default", Status: 200, TotalTokens: tokens, Path: "/v1/chat/completions",
	}
}

// TestSettleFullCycle exercises the entire settlement pipeline end-to-end through the
// public Settle entry point: scan requests.jsonl → aggregate → submit batch → confirm
// receipt → commit cursor → reduce pendingSpend → write audit log → delete WAL.
func TestSettleFullCycle(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")

	// Two billable records for the same wallet+worker (default = $1/token at coverageCfg).
	writeRequestLog(t, reqLog, []RequestRecord{
		billableRecord("r1", "w1", 5),
		billableRecord("r2", "w1", 5),
	})
	s.resolver = staticResolver{"w1": "miner1"} // w1 → miner1 → sp1Addr
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	// Mirror what the gateway would have reserved during the requests: $10 total.
	s.balance.AddPendingSpend(walletU, big.NewFloat(10))

	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("Settle returned error: %v", err)
	}

	// One batch submitted on-chain.
	if mock.submitCount != 1 {
		t.Errorf("expected exactly 1 batch submitted, got %d", mock.submitCount)
	}
	// WAL deleted (clean finish), audit log written.
	if walExists(dir) {
		t.Error("WAL must be deleted after a clean settlement")
	}
	if _, err := os.Stat(filepath.Join(dir, "settlements.jsonl")); err != nil {
		t.Errorf("expected settlements audit log to be written: %v", err)
	}
	// pendingSpend fully reduced by the settled amount.
	if ps := s.balance.GetPendingSpend(walletU); ps.Sign() != 0 {
		t.Errorf("expected pendingSpend reduced to 0 after settlement, got %s", ps.Text('f', 6))
	}
	// Cursor advanced — a second cycle finds nothing new to bill.
	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("second Settle returned error: %v", err)
	}
	if mock.submitCount != 1 {
		t.Errorf("cursor did not advance: second cycle re-billed (submitCount=%d)", mock.submitCount)
	}
}

// TestSettleAllUnresolvedCommitsCursor: when EVERY record is unresolvable (worker has
// no SP mapping), nothing is submitted, the records are parked in the dead-letter file,
// and the cursor STILL advances so they are not re-scanned from the request log each
// cycle (they are retried only via the dead-letter reprocess path).
func TestSettleAllUnresolvedCommitsCursor(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")

	writeRequestLog(t, reqLog, []RequestRecord{
		billableRecord("g1", "ghost", 5),
		billableRecord("g2", "ghost", 5),
	})
	// No resolver → "ghost" maps to nothing.
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}

	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("Settle returned error: %v", err)
	}

	if mock.submitCount != 0 {
		t.Errorf("expected nothing submitted for unresolvable records, got %d", mock.submitCount)
	}
	// Dead-letter file holds the unresolvable records.
	dl := s.loadDeadLetter()
	if len(dl) != 2 {
		t.Errorf("expected 2 dead-lettered records, got %d", len(dl))
	}
	// Cursor advanced — the raw request log is not re-scanned next cycle.
	recs, _, _, err := s.scanner.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Errorf("cursor did not advance past unresolvable records; Peek still returns %d", len(recs))
	}
}

// TestSettleSubmitFailureRetainsWAL: a submit failure mid-cycle must leave the WAL in
// place and NOT advance the cursor, so the next cycle replays the exact same batch
// (crash/failure recovery). After the fault clears, the replay settles it once.
func TestSettleSubmitFailureRetainsWAL(t *testing.T) {
	mock := newMockContract()
	mock.submitErr = errors.New("rpc down")
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")

	writeRequestLog(t, reqLog, []RequestRecord{billableRecord("r1", "w1", 5)})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}

	// First cycle: submit fails → Settle errors, WAL retained, cursor NOT advanced.
	if err := s.Settle(context.Background()); err == nil {
		t.Fatal("expected Settle to error when submit fails")
	}
	if !walExists(dir) {
		t.Error("WAL must be retained after a submit failure")
	}
	recs, _, _, err := s.scanner.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Errorf("cursor must NOT advance on submit failure; expected the record still pending, got %d", len(recs))
	}

	// Recover: next cycle replays the WAL and settles exactly once.
	mock.submitErr = nil
	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("recovery Settle returned error: %v", err)
	}
	if mock.submitCount != 1 {
		t.Errorf("expected exactly 1 submission after recovery, got %d", mock.submitCount)
	}
	if walExists(dir) {
		t.Error("WAL must be deleted after the recovered settlement")
	}
}

// TestSettleReceiptFailureRetainsWAL: a stuck/timed-out transaction (WaitForReceipt
// error) must also retain the WAL so the batch is retried, never silently dropped.
func TestSettleReceiptFailureRetainsWAL(t *testing.T) {
	mock := newMockContract()
	mock.receiptErr = errors.New("receipt timeout")
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")

	writeRequestLog(t, reqLog, []RequestRecord{billableRecord("r1", "w1", 5)})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}

	if err := s.Settle(context.Background()); err == nil {
		t.Fatal("expected Settle to error when the receipt never confirms")
	}
	if !walExists(dir) {
		t.Error("WAL must be retained when the transaction does not confirm")
	}
}
