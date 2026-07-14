package settlement

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"testing"
)

// TestVerifyStateCleanAfterCycle verifies that the post-restore integrity check
// reports OK on the state left by a clean settlement cycle, with the expected
// cursor/settled/dead-letter values (the "restore landed good state" path of B3).
func TestVerifyStateCleanAfterCycle(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")
	writeRequestLog(t, reqLog, []RequestRecord{
		billableRecord("a", "w1", 5),
		billableRecord("b", "w1", 5),
	})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(10))

	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	res := s.VerifyState()
	if !res.OK {
		t.Fatalf("clean state should verify OK, problems: %v", res.Problems)
	}
	if res.WALPresent {
		t.Error("no WAL should be present after a clean cycle")
	}
	if res.CursorOffset <= 0 {
		t.Errorf("cursor offset should have advanced past 0, got %d", res.CursorOffset)
	}
	if res.SettledUSD == "" || res.SettledUSD == "0" {
		t.Errorf("settled-total should be non-zero after settling, got %q", res.SettledUSD)
	}
}

// TestVerifyStateDetectsCursorBeyondLog verifies the check FAILS when the cursor
// points past the end of the request log with no rotation backup — the signature of
// a log that was replaced/truncated without resetting the cursor, which would cause
// records to be skipped (lost billing).
func TestVerifyStateDetectsCursorBeyondLog(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")

	// Small log, but cursor claims a huge offset.
	writeRequestLog(t, reqLog, []RequestRecord{billableRecord("a", "w1", 5)})
	if err := s.scanner.CommitCursor(Cursor{Offset: 1 << 30}); err != nil {
		t.Fatal(err)
	}

	res := s.VerifyState()
	if res.OK {
		t.Fatal("cursor far beyond the log size (no backup) must be flagged as a problem")
	}
	if len(res.Problems) == 0 {
		t.Error("expected a problem describing the cursor/log mismatch")
	}
}

// TestVerifyStateDetectsCorruptWAL verifies the check FAILS on an unparseable WAL —
// a corrupt WAL must not be silently started on (it drives on-chain submission).
func TestVerifyStateDetectsCorruptWAL(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())

	if err := os.WriteFile(filepath.Join(dir, "pending-settlement.json"), []byte("{not valid json"), 0644); err != nil {
		t.Fatal(err)
	}

	res := s.VerifyState()
	if res.OK {
		t.Fatal("a corrupt WAL must be flagged")
	}
}

// TestRestoreRoundTripPreservesState simulates the backup→restore round trip at the
// file level: state written by one settler is readable and consistent when a fresh
// settler is pointed at the same data dir (what restore-state.sh achieves by copying
// files into DATA_DIR). The reborn settler must see the same cursor and settled total.
func TestRestoreRoundTripPreservesState(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")
	writeRequestLog(t, reqLog, []RequestRecord{billableRecord("a", "w1", 7)})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(7))
	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	wantCursor := s.VerifyState().CursorOffset
	wantSettled := s.SettledUSDTotal().Text('f', 6)

	// "Restore": a brand-new settler over the SAME dir (files persisted on disk).
	mock2 := newMockContract()
	bc2 := NewBalanceCache(nil, s.cfg.SupportedTokens, s.pricer, 30, discardLogger())
	s2 := NewSettler(s.cfg, mock2, s.pricer, bc2, nil, reqLog, dir, discardLogger())

	res := s2.VerifyState()
	if !res.OK {
		t.Fatalf("restored state should verify OK, problems: %v", res.Problems)
	}
	if res.CursorOffset != wantCursor {
		t.Errorf("restored cursor offset: want %d, got %d", wantCursor, res.CursorOffset)
	}
	if got := s2.SettledUSDTotal().Text('f', 6); got != wantSettled {
		t.Errorf("restored settled total: want %s, got %s", wantSettled, got)
	}
}
