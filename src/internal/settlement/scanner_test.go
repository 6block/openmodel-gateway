package settlement

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// rotateN simulates the gateway logger's N-backup rotation: .i -> .i+1, main -> .1.
func rotateN(path string, backups int) {
	os.Remove(fmt.Sprintf("%s.%d", path, backups))
	for i := backups - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", path, i), fmt.Sprintf("%s.%d", path, i+1))
	}
	os.Rename(path, path+".1")
}

func billRec(id string) RequestRecord {
	return RequestRecord{RequestID: id, Timestamp: time.Now(), Wallet: "0xabc",
		Status: 200, PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}
}

// TestScannerMultiRotationNoLoss: when the log rotates more than once between
// settlement scans (the high-throughput case that lost ~76% of billing), the
// scanner must drain every un-settled backup, not just .1 — zero records lost and
// none double-billed.
func TestScannerMultiRotationNoLoss(t *testing.T) {
	s, logPath := testScanner(t)
	billed := map[string]int{}
	consume := func() {
		recs, _, nc, err := s.Peek()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			billed[r.RequestID]++
		}
		if err := s.CommitCursor(nc); err != nil {
			t.Fatal(err)
		}
	}

	writeRecords(t, logPath, []RequestRecord{billRec("1"), billRec("2"), billRec("3")})
	consume() // bills 1,2,3; cursor at end of main

	writeRecords(t, logPath, []RequestRecord{billRec("4"), billRec("5")}) // appended, un-scanned
	rotateN(logPath, 10)                                                  // main([1..5]) -> .1
	writeRecords(t, logPath, []RequestRecord{billRec("6"), billRec("7")})
	rotateN(logPath, 10) // .1 -> .2, main([6,7]) -> .1
	writeRecords(t, logPath, []RequestRecord{billRec("8"), billRec("9")})
	// Two rotations with no scan between: 4,5 sit in .2 (tail past cursor), 6,7 in .1.

	consume() // must drain .2 tail (4,5) + .1 (6,7) + new main (8,9)

	for id := 1; id <= 9; id++ {
		k := fmt.Sprintf("%d", id)
		if billed[k] == 0 {
			t.Errorf("record %s lost (not billed)", k)
		}
		if billed[k] > 1 {
			t.Errorf("record %s double-billed (%d times)", k, billed[k])
		}
	}
	if len(billed) != 9 {
		t.Errorf("expected 9 distinct billed records, got %d", len(billed))
	}
}

func writeRecords(t *testing.T, path string, recs []RequestRecord) {
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

func testScanner(t *testing.T) (*Scanner, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "requests.jsonl")
	cursorPath := filepath.Join(dir, "cursor.json")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewScanner(logPath, cursorPath, logger), logPath
}

func TestScannerBillableFilter(t *testing.T) {
	sc, logPath := testScanner(t)
	now := time.Now()
	writeRecords(t, logPath, []RequestRecord{
		{Wallet: walletU, Status: 200, TotalTokens: 10, Timestamp: now},                                   // billable
		{Wallet: walletU, Status: 503, TotalTokens: 0, ErrorReason: "all_retries", Timestamp: now},        // not billable (status)
		{Wallet: walletU, Status: 200, TotalTokens: 5, ErrorReason: "stream_interrupted", Timestamp: now}, // not billable (interrupted)
		{Wallet: "", Status: 200, TotalTokens: 7, Timestamp: now},                                         // not billable (no wallet)
		{Wallet: walletU, Status: 200, TotalTokens: 0, Timestamp: now},                                    // not billable (0 tokens)
	})

	records, _, _, err := sc.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 billable record, got %d", len(records))
	}
	if records[0].TotalTokens != 10 {
		t.Errorf("expected the 10-token record, got %d tokens", records[0].TotalTokens)
	}
}

// TestPeekDoesNotAdvanceCursor is the regression test for bug C1: Peek must not
// persist the cursor; only CommitCursor does.
func TestPeekDoesNotAdvanceCursor(t *testing.T) {
	sc, logPath := testScanner(t)
	writeRecords(t, logPath, []RequestRecord{
		{Wallet: walletU, Status: 200, TotalTokens: 10, Timestamp: time.Now()},
	})

	// First Peek returns the record.
	records, _, newCursor, err := sc.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}

	// Without committing, a second Peek must return the SAME record (cursor not advanced).
	records2, _, _, err := sc.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if len(records2) != 1 {
		t.Fatalf("cursor advanced without commit (bug C1 regression): got %d records", len(records2))
	}

	// After committing, a third Peek returns nothing new.
	if err := sc.CommitCursor(newCursor); err != nil {
		t.Fatal(err)
	}
	records3, _, _, err := sc.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if len(records3) != 0 {
		t.Fatalf("expected 0 records after commit, got %d", len(records3))
	}
}

func TestScannerResumesFromCursor(t *testing.T) {
	sc, logPath := testScanner(t)
	writeRecords(t, logPath, []RequestRecord{
		{Wallet: walletU, Status: 200, TotalTokens: 10, Timestamp: time.Now()},
	})

	_, _, c1, _ := sc.Peek()
	if err := sc.CommitCursor(c1); err != nil {
		t.Fatal(err)
	}

	// Append a new record after committing.
	writeRecords(t, logPath, []RequestRecord{
		{Wallet: walletU, Status: 200, TotalTokens: 20, Timestamp: time.Now()},
	})

	records, _, _, err := sc.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].TotalTokens != 20 {
		t.Fatalf("expected only the new 20-token record, got %+v", records)
	}
}
