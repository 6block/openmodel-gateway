package settlement

import (
	"context"
	"math/big"
	"path/filepath"
	"testing"
	"time"
)

// TestSPEarningsDetailAttributionAndPricing verifies the per-request earnings view:
// each request is attributed to the right SP (worker→miner→EVM) and priced with the
// SAME path settlement uses, minus the platform fee.
func TestSPEarningsDetailAttributionAndPricing(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")
	// w1→miner1→sp1Addr. default = $1/token.
	writeRequestLog(t, reqLog, []RequestRecord{
		billableRecord("r1", "w1", 10), // $10 cost
		billableRecord("r2", "w1", 5),  // $5 cost
		// a non-billable record must be excluded:
		{RequestID: "bad", Wallet: walletU, WorkerID: "w1", Model: "default", Status: 503, TotalTokens: 0},
	})
	s.resolver = staticResolver{"w1": "miner1"}

	// feeBps=300 (3%) → SP keeps 97%.
	res, err := s.SPEarningsDetail(sp1Addr, 0, 200, 300)
	if err != nil {
		t.Fatalf("SPEarningsDetail: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("want 2 billable items, got %d", len(res.Items))
	}
	// Total = (10+5) × $1 × 0.97 = 14.55
	if res.TotalEarningUSD != "14.55000000" {
		t.Errorf("total earning: want 14.55000000, got %s", res.TotalEarningUSD)
	}
	// All pending (nothing settled yet).
	if res.SettledCount != 0 || res.PendingCount != 2 {
		t.Errorf("want 0 settled / 2 pending, got %d / %d", res.SettledCount, res.PendingCount)
	}
	for _, it := range res.Items {
		if it.Settled {
			t.Errorf("item %s should be pending before settlement", it.RequestID)
		}
	}
}

// TestSPEarningsDetailMarksSettled verifies that after a full settlement cycle, the
// requests show up as settled with the on-chain tx hash, and the settled earning sum
// matches the manually-computed per-request total (pricing-basis consistency).
func TestSPEarningsDetailMarksSettled(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")
	writeRequestLog(t, reqLog, []RequestRecord{
		billableRecord("r1", "w1", 10),
		billableRecord("r2", "w1", 5),
	})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(15))

	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	res, err := s.SPEarningsDetail(sp1Addr, 0, 200, 300)
	if err != nil {
		t.Fatalf("SPEarningsDetail: %v", err)
	}
	if res.SettledCount != 2 || res.PendingCount != 0 {
		t.Fatalf("after settlement want 2 settled / 0 pending, got %d / %d", res.SettledCount, res.PendingCount)
	}
	for _, it := range res.Items {
		if !it.Settled {
			t.Errorf("item %s should be settled", it.RequestID)
		}
		if it.TxHash == "" {
			t.Errorf("settled item %s must carry a tx_hash", it.RequestID)
		}
	}
	// Settled earning sum == total (all settled). 14.55 with 3% fee.
	if res.SettledEarningUSD != "14.55000000" {
		t.Errorf("settled earning: want 14.55000000, got %s", res.SettledEarningUSD)
	}
}

// TestSPEarningsDetailLedgerIdempotent verifies the items ledger is not double-written
// when a confirmed batch is replayed (crash-safety): re-running resumePending on the
// same WAL must not duplicate ledger lines.
func TestSPEarningsDetailLedgerIdempotent(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")
	writeRequestLog(t, reqLog, []RequestRecord{billableRecord("r1", "w1", 10)})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(10))

	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	// Re-derive index; one request → exactly one ledger entry.
	idx := s.loadItemsLedgerIndex()
	if len(idx) != 1 {
		t.Fatalf("want exactly 1 ledgered request, got %d", len(idx))
	}

	// Simulate a replay of the SAME details_hash via appendItemsLedger directly.
	for _, r := range []settlementItemRecord{idx["r1"]} {
		b := &pendingBatch{DetailsHash: r.DetailsHash, Items: []pendingItem{{
			User: walletU, SP: sp1Addr, Token: filAddr, RequestIDs: []string{"r1"},
		}}}
		s.appendItemsLedger(b, r.TxHash, r.BlockNumber)
	}
	idx2 := s.loadItemsLedgerIndex()
	if len(idx2) != 1 {
		t.Errorf("ledger must stay idempotent on replay, got %d entries", len(idx2))
	}
}

// TestSPEarningsDetailSinceAndLimit verifies the since filter and limit cap.
func TestSPEarningsDetailSinceAndLimit(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")
	recs := make([]RequestRecord, 0, 5)
	for i := 0; i < 5; i++ {
		recs = append(recs, billableRecord("r"+string(rune('1'+i)), "w1", 2))
	}
	writeRequestLog(t, reqLog, recs)
	s.resolver = staticResolver{"w1": "miner1"}

	res, err := s.SPEarningsDetail(sp1Addr, 0, 3, 0) // feeBps=0, limit 3
	if err != nil {
		t.Fatalf("SPEarningsDetail: %v", err)
	}
	if len(res.Items) != 3 {
		t.Errorf("limit=3 should cap items at 3, got %d", len(res.Items))
	}
	// B9 semantics: totals cover the RETURNED page, not all history (all-time settled
	// totals come from the revenue store). feeBps=0 → 3 returned × 2 tokens × $1 = $6.
	if res.TotalEarningUSD != "6.00000000" {
		t.Errorf("page total (fee=0): want 6.00000000, got %s", res.TotalEarningUSD)
	}
	if res.Scope != "returned_items" {
		t.Errorf("scope must self-document page semantics, got %q", res.Scope)
	}
}

// TestSPEarningsDetailEarlyStopAcrossRotation is the B9 regression: with rotated log
// files (live newest, .1 older, .2 oldest), a page that the newer files already fill
// must (a) contain ONLY the newest records and (b) never require reading the oldest
// file's content into the result — the old implementation read every file fully and
// loaded the whole items ledger per call (>90s at soak scale).
func TestSPEarningsDetailEarlyStopAcrossRotation(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")
	old := billableRecord("r-old", "w1", 2)
	old.Timestamp = old.Timestamp.Add(-2 * time.Hour)
	mid := billableRecord("r-mid", "w1", 2)
	mid.Timestamp = mid.Timestamp.Add(-1 * time.Hour)
	newest := billableRecord("r-new", "w1", 2)
	writeRequestLog(t, reqLog+".2", []RequestRecord{old}) // oldest backup
	writeRequestLog(t, reqLog+".1", []RequestRecord{mid})
	writeRequestLog(t, reqLog, []RequestRecord{newest}) // live = newest
	s.resolver = staticResolver{"w1": "miner1"}

	res, err := s.SPEarningsDetail(sp1Addr, 0, 2, 0)
	if err != nil {
		t.Fatalf("SPEarningsDetail: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("want page of 2, got %d", len(res.Items))
	}
	if res.Items[0].RequestID != "r-new" || res.Items[1].RequestID != "r-mid" {
		t.Fatalf("page must be the NEWEST records in order, got %s,%s",
			res.Items[0].RequestID, res.Items[1].RequestID)
	}
	// Page totals: 2 × 2 tokens × $1, fee 0.
	if res.TotalEarningUSD != "4.00000000" {
		t.Errorf("page total: want 4.00000000, got %s", res.TotalEarningUSD)
	}
}

// TestSPEarningsDetailSettledViaMerkleIndex verifies the B9 settled lookup goes through
// the F6 merkle index (point seek-reads), including after a cold start (index nil), and
// that a rid absent from the merkle ledger reports pending.
func TestSPEarningsDetailSettledViaMerkleIndex(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")
	writeRequestLog(t, reqLog, []RequestRecord{
		billableRecord("r1", "w1", 10),
		billableRecord("r2", "w1", 5),
	})
	s.resolver = staticResolver{"w1": "miner1"}
	s.balance.chainBalances[walletU] = map[string]*big.Int{filAddr: fil(1000)}
	s.balance.AddPendingSpend(walletU, big.NewFloat(15))
	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	// A later request that never settled (writeRequestLog appends).
	writeRequestLog(t, reqLog, []RequestRecord{billableRecord("r3", "w1", 2)})

	// Cold start: force the index to re-warm from disk inside the detail call.
	s.merkleIdxMu.Lock()
	s.merkleIdx = nil
	s.merkleIdxMu.Unlock()

	res, err := s.SPEarningsDetail(sp1Addr, 0, 200, 0)
	if err != nil {
		t.Fatalf("SPEarningsDetail: %v", err)
	}
	if res.SettledCount != 2 || res.PendingCount != 1 {
		t.Fatalf("want 2 settled / 1 pending, got %d / %d", res.SettledCount, res.PendingCount)
	}
	for _, it := range res.Items {
		switch it.RequestID {
		case "r1", "r2":
			if !it.Settled || it.TxHash == "" || it.DetailsHash == "" {
				t.Errorf("%s must be settled with tx/details from the merkle ledger: %+v", it.RequestID, it)
			}
		case "r3":
			if it.Settled {
				t.Errorf("r3 never settled, must be pending")
			}
		}
	}
}

// TestSPEarningsDetailWrongSPEmpty verifies a SP with no matching requests gets an
// empty result, not another SP's data.
func TestSPEarningsDetailWrongSPEmpty(t *testing.T) {
	mock := newMockContract()
	s, dir := newTestSettler(t, mock, discardLogger())
	reqLog := filepath.Join(dir, "requests.jsonl")
	writeRequestLog(t, reqLog, []RequestRecord{billableRecord("r1", "w1", 10)})
	s.resolver = staticResolver{"w1": "miner1"}

	res, err := s.SPEarningsDetail(sp2Addr, 0, 200, 300) // sp2 has no requests
	if err != nil {
		t.Fatalf("SPEarningsDetail: %v", err)
	}
	if len(res.Items) != 0 {
		t.Errorf("sp2 should have no items, got %d", len(res.Items))
	}
}
