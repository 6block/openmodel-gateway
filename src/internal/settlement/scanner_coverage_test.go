package settlement

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestScannerRotationSizeDecrease covers the rotation reset (info.Size() <
// cursor.FileSize -> rescan from offset 0). Without it, the committed offset would
// point past the end of the freshly-rotated (smaller) file and the new records
// would be silently lost.
func TestScannerRotationSizeDecrease(t *testing.T) {
	sc, logPath := testScanner(t)

	// 5 records, commit cursor (FileSize/Offset now reflect a "large" file).
	recs := make([]RequestRecord, 5)
	for i := range recs {
		recs[i] = RequestRecord{RequestID: fmt.Sprintf("r%d", i), Wallet: walletU, Status: 200, TotalTokens: 10, Timestamp: time.Now()}
	}
	writeRecords(t, logPath, recs)
	_, _, c1, err := sc.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.CommitCursor(c1); err != nil {
		t.Fatal(err)
	}

	// Rotate: the old log moved aside, a fresh, smaller requests.jsonl appears.
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	writeRecords(t, logPath, []RequestRecord{
		{RequestID: "new", Wallet: walletU, Status: 200, TotalTokens: 99, Timestamp: time.Now()},
	})

	records, _, _, err := sc.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].TotalTokens != 99 {
		t.Fatalf("rotation reset failed: expected the new 99-token record, got %d records %+v", len(records), records)
	}
}

// TestScannerBackupFallback covers the `.1` rotated-backup scan: when the primary
// log is missing (just rotated away), Peek falls back to logPath+".1".
func TestScannerBackupFallback(t *testing.T) {
	sc, logPath := testScanner(t)
	// Do NOT create the primary log; only the rotated backup exists.
	writeRecords(t, logPath+".1", []RequestRecord{
		{Wallet: walletU, Status: 200, TotalTokens: 42, Timestamp: time.Now()},
	})

	records, _, _, err := sc.Peek()
	if err != nil {
		t.Fatalf("expected backup fallback to succeed, got error: %v", err)
	}
	if len(records) != 1 || records[0].TotalTokens != 42 {
		t.Fatalf("expected 1 record from .1 backup, got %d %+v", len(records), records)
	}
}

// TestScannerSkipsMalformedLine covers the malformed-JSONL skip: a poison line is
// skipped, valid records around it are still parsed.
func TestScannerSkipsMalformedLine(t *testing.T) {
	sc, logPath := testScanner(t)
	good := RequestRecord{Wallet: walletU, Status: 200, TotalTokens: 10, Timestamp: time.Now()}
	b, _ := json.Marshal(good)
	content := string(b) + "\n{ this is not valid json\n" + string(b) + "\n"
	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	records, _, _, err := sc.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 valid records (malformed line skipped), got %d", len(records))
	}
}

// TestScannerRecoversFromCorruptCursor covers loadCursor's unmarshal-error path:
// a corrupt cursor file must not crash; the scanner restarts from the beginning.
func TestScannerRecoversFromCorruptCursor(t *testing.T) {
	sc, logPath := testScanner(t)
	writeRecords(t, logPath, []RequestRecord{
		{Wallet: walletU, Status: 200, TotalTokens: 10, Timestamp: time.Now()},
	})
	if err := os.WriteFile(sc.cursorPath, []byte("{garbage not json"), 0644); err != nil {
		t.Fatal(err)
	}

	records, _, _, err := sc.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected recovery to scan from start (1 record), got %d", len(records))
	}
}

func recIDs(recs []RequestRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.RequestID
	}
	return out
}

// TestScannerRotationPreservesUnsettledTail is the regression for the audit
// CRITICAL: records written to the main log AFTER the last commit but BEFORE a
// rotation live in the rotated-away .1 and must NOT be skipped. Before the fix
// the scanner opened the recreated main file successfully, never read .1, and
// lost the tail permanently → silent billing-data loss.
func TestScannerRotationPreservesUnsettledTail(t *testing.T) {
	sc, logPath := testScanner(t)

	// Settle the first 3 records.
	writeRecords(t, logPath, []RequestRecord{
		{RequestID: "a0", Wallet: walletU, Status: 200, TotalTokens: 10, Timestamp: time.Now()},
		{RequestID: "a1", Wallet: walletU, Status: 200, TotalTokens: 10, Timestamp: time.Now()},
		{RequestID: "a2", Wallet: walletU, Status: 200, TotalTokens: 10, Timestamp: time.Now()},
	})
	_, _, c1, err := sc.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.CommitCursor(c1); err != nil {
		t.Fatal(err)
	}

	// Two MORE records written to main but NOT yet settled.
	writeRecords(t, logPath, []RequestRecord{
		{RequestID: "a3", Wallet: walletU, Status: 200, TotalTokens: 11, Timestamp: time.Now()},
		{RequestID: "a4", Wallet: walletU, Status: 200, TotalTokens: 12, Timestamp: time.Now()},
	})

	// Rotation: main -> .1, fresh main with 4 new records.
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		t.Fatal(err)
	}
	writeRecords(t, logPath, []RequestRecord{
		{RequestID: "b0", Wallet: walletU, Status: 200, TotalTokens: 20, Timestamp: time.Now()},
		{RequestID: "b1", Wallet: walletU, Status: 200, TotalTokens: 20, Timestamp: time.Now()},
		{RequestID: "b2", Wallet: walletU, Status: 200, TotalTokens: 20, Timestamp: time.Now()},
		{RequestID: "b3", Wallet: walletU, Status: 200, TotalTokens: 20, Timestamp: time.Now()},
	})

	records, _, c2, err := sc.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 6 {
		t.Fatalf("rotation lost records: expected 6 (2 tail + 4 new), got %d: %v", len(records), recIDs(records))
	}
	got := map[string]bool{}
	for _, r := range records {
		got[r.RequestID] = true
	}
	for _, want := range []string{"a3", "a4", "b0", "b1", "b2", "b3"} {
		if !got[want] {
			t.Errorf("missing record %s after rotation", want)
		}
	}

	// Idempotency: after commit, only a brand-new record is returned.
	if err := sc.CommitCursor(c2); err != nil {
		t.Fatal(err)
	}
	writeRecords(t, logPath, []RequestRecord{
		{RequestID: "b4", Wallet: walletU, Status: 200, TotalTokens: 30, Timestamp: time.Now()},
	})
	records2, _, _, err := sc.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if len(records2) != 1 || records2[0].RequestID != "b4" {
		t.Fatalf("post-rotation cursor wrong: expected only b4, got %v", recIDs(records2))
	}
}

// TestScannerRotationNewFileLargerThanOldSize defeats the old "size < FileSize"
// heuristic: after rotation the new main file is already LARGER than the
// pre-rotation file, so size-based detection fails. Inode tracking must still
// detect rotation and neither lose the .1 tail nor skip the new file's head.
func TestScannerRotationNewFileLargerThanOldSize(t *testing.T) {
	sc, logPath := testScanner(t)

	writeRecords(t, logPath, []RequestRecord{
		{RequestID: "s0", Wallet: walletU, Status: 200, TotalTokens: 10, Timestamp: time.Now()},
		{RequestID: "s1", Wallet: walletU, Status: 200, TotalTokens: 10, Timestamp: time.Now()},
	})
	_, _, c1, err := sc.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.CommitCursor(c1); err != nil {
		t.Fatal(err)
	}

	// 1 unsettled tail record on the old main.
	writeRecords(t, logPath, []RequestRecord{
		{RequestID: "s2", Wallet: walletU, Status: 200, TotalTokens: 13, Timestamp: time.Now()},
	})

	// Rotate, then write MANY records to the new main so its size exceeds the old
	// file's size, breaking the size-decrease heuristic.
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		t.Fatal(err)
	}
	var big []RequestRecord
	for i := 0; i < 20; i++ {
		big = append(big, RequestRecord{RequestID: fmt.Sprintf("n%d", i), Wallet: walletU, Status: 200, TotalTokens: 5, Timestamp: time.Now()})
	}
	writeRecords(t, logPath, big)

	records, _, _, err := sc.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 21 {
		t.Fatalf("inode rotation detection failed: expected 21 (1 tail + 20 new), got %d", len(records))
	}
	got := map[string]bool{}
	for _, r := range records {
		got[r.RequestID] = true
	}
	if !got["s2"] {
		t.Error("lost unsettled tail record s2 (the audit CRITICAL)")
	}
	if !got["n0"] {
		t.Error("skipped new file's first record n0 (size-heuristic bug)")
	}
}
