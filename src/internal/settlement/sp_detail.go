package settlement

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"sort"
	"strings"
	"time"
)

// sp_detail.go implements the SP per-request earnings view: it lets a Storage
// Provider see, for each individual inference request it served, how much it earned
// and whether that request has been settled on-chain (and in which tx).
//
// The earning amount is computed from the request log via the SAME pricing path
// settlement uses (Aggregator.RecordEarningUSD), so the per-request detail can never
// drift from how billing actually works. Settlement status comes from the
// settlement-items.jsonl ledger written when a batch confirms on-chain.

// SPRequestEarning is one inference request attributed to an SP.
type SPRequestEarning struct {
	RequestID     string    `json:"request_id"`
	Timestamp     time.Time `json:"timestamp"`
	Model         string    `json:"model"`
	TotalTokens   int       `json:"total_tokens"`
	PromptTokens  int       `json:"prompt_tokens"`
	CachedTokens  int       `json:"cached_tokens"`
	EarningUSD    string    `json:"earning_usd"`
	Settled       bool      `json:"settled"`
	TxHash        string    `json:"tx_hash,omitempty"`
	DetailsHash   string    `json:"details_hash,omitempty"`
	BlockNumber   uint64    `json:"block_number,omitempty"`
}

// SPEarningsDetailResult is the full response for one SP's per-request earnings.
//
// B9: the totals/counts cover the RETURNED items (newest-first page of `limit`),
// NOT all history — computing all-history from raw logs made every call read the
// full request log + the whole items ledger (>90s at soak scale). All-time settled
// totals come from GET /api/v1/revenue/<sp> (dedicated cumulative store, instant).
type SPEarningsDetailResult struct {
	SP              string             `json:"sp"`
	PlatformFeeBps  int64              `json:"platform_fee_bps"`
	Scope           string             `json:"scope"` // always "returned_items" (self-documenting)
	TotalEarningUSD string             `json:"total_earning_usd"`
	SettledEarningUSD string           `json:"settled_earning_usd"`
	PendingEarningUSD string           `json:"pending_earning_usd"`
	SettledCount    int                `json:"settled_count"`
	PendingCount    int                `json:"pending_count"`
	Items           []SPRequestEarning `json:"items"`
}

// SPEarningsDetail returns the per-request earnings for one SP (by EVM address).
//   - sinceUnix: only requests at/after this unix time (0 = all).
//   - limit: max items returned (newest first); <=0 means a sane default cap.
//   - feeBps: on-chain platform fee in basis points (caller reads it from the contract).
//
// B9 data path (2026-07-07): the old implementation loaded the ENTIRE items ledger
// into memory and read every request-log file fully on each call — >90s once the
// ledgers reached soak size. Now:
//   - request logs are walked NEWEST→OLDEST (live, .1, .2 …; rotation makes .1 the
//     newest backup) and the walk STOPS once `limit` matches are collected — every
//     record in an older file predates everything in a newer file;
//   - settled status is resolved by POINT LOOKUPS for just the returned page via the
//     F6 merkle rid→offset index ("settled" here = merkle-committed on-chain, the
//     same boundary the receipt-proof endpoint serves);
//   - totals cover the returned page only (see SPEarningsDetailResult).
func (s *Settler) SPEarningsDetail(spEVM string, sinceUnix int64, limit int, feeBps int64) (SPEarningsDetailResult, error) {
	if limit <= 0 {
		limit = 200
	}
	want := strings.ToLower(spEVM)

	// Refresh worker→SP from the live registry so attribution matches settlement.
	if s.resolver != nil {
		s.aggregator.UpdateWorkerSPMap(s.resolver.GetWorkerSPMap())
	}

	res := SPEarningsDetailResult{SP: spEVM, PlatformFeeBps: feeBps, Scope: "returned_items"}

	// Newest→oldest walk with early stop.
	paths := []string{s.requestLogPath}
	for i := 1; ; i++ {
		bp := fmt.Sprintf("%s.%d", s.requestLogPath, i)
		if _, err := os.Stat(bp); err != nil {
			break
		}
		paths = append(paths, bp)
	}
	var recs []RequestRecord
	for _, p := range paths {
		fileRecs, maxTS, err := s.scanSPRecords(p, want, sinceUnix)
		if err != nil {
			return res, err
		}
		recs = append(recs, fileRecs...)
		if len(recs) >= limit {
			break // older files cannot displace anything in the newest-first page
		}
		// Everything in the remaining files predates this file's newest record; if
		// that is already before `since`, older files cannot match either.
		if sinceUnix > 0 && !maxTS.IsZero() && maxTS.Unix() < sinceUnix {
			break
		}
	}

	// Newest first, cap to the page.
	sort.Slice(recs, func(i, j int) bool { return recs[i].Timestamp.After(recs[j].Timestamp) })
	if len(recs) > limit {
		recs = recs[:limit]
	}

	// Settled lookups for just this page.
	rids := make([]string, len(recs))
	for i, r := range recs {
		rids[i] = r.RequestID
	}
	settled := s.settledInfoForRIDs(rids)

	totalUSD := new(big.Float)
	settledUSD := new(big.Float)
	pendingUSD := new(big.Float)
	items := make([]SPRequestEarning, 0, len(recs))
	for _, rec := range recs {
		earn := s.aggregator.RecordEarningUSD(rec, feeBps)
		totalUSD.Add(totalUSD, earn)
		item := SPRequestEarning{
			RequestID:    rec.RequestID,
			Timestamp:    rec.Timestamp,
			Model:        rec.Model,
			TotalTokens:  rec.TotalTokens,
			PromptTokens: rec.PromptTokens,
			CachedTokens: rec.CachedTokens,
			EarningUSD:   earn.Text('f', 8),
		}
		if led, ok := settled[rec.RequestID]; ok {
			item.Settled = true
			item.TxHash = led.TxHash
			item.DetailsHash = led.DetailsHash
			item.BlockNumber = led.BlockNumber
			settledUSD.Add(settledUSD, earn)
			res.SettledCount++
		} else {
			pendingUSD.Add(pendingUSD, earn)
			res.PendingCount++
		}
		items = append(items, item)
	}
	res.Items = items
	res.TotalEarningUSD = totalUSD.Text('f', 8)
	res.SettledEarningUSD = settledUSD.Text('f', 8)
	res.PendingEarningUSD = pendingUSD.Text('f', 8)
	return res, nil
}

// settledInfoForRIDs resolves on-chain settlement info for a page of request ids
// WITHOUT reading the items ledger: the F6 merkle index maps every merkle-committed
// rid to its batch line offset; page rids are grouped by offset so each referenced
// line is seek-read and parsed ONCE, with a minimal shape that skips the (large)
// embedded records. A rid absent from the index is reported unsettled — either truly
// pending, or settled before Merkle commitments existed (pre-2026-07-03 history; the
// receipt-proof endpoint draws the exact same boundary).
func (s *Settler) settledInfoForRIDs(rids []string) map[string]settlementItemRecord {
	out := make(map[string]settlementItemRecord, len(rids))
	if len(rids) == 0 {
		return out
	}
	s.merkleIdxMu.Lock()
	if !s.merkleWarm {
		s.loadOrWarmIndexesLocked()
	}
	byOff := make(map[int64][]string)
	for _, rid := range rids {
		if off, ok := s.merkleIdx[rid]; ok {
			byOff[off] = append(byOff[off], rid)
		}
	}
	s.merkleIdxMu.Unlock()
	if len(byOff) == 0 {
		return out
	}

	f, err := os.Open(s.merkleLedgerPath())
	if err != nil {
		return out
	}
	defer f.Close()
	for off, group := range byOff {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			continue
		}
		line, rerr := bufio.NewReaderSize(f, 1<<20).ReadBytes('\n')
		if len(line) == 0 && rerr != nil {
			continue
		}
		// Minimal shape — only identity + rids; embedded leaf records are not decoded.
		var rec struct {
			DetailsHash string `json:"details_hash"`
			TxHash      string `json:"tx_hash"`
			BlockNumber uint64 `json:"block_number"`
			Leaves      []struct {
				Rid string `json:"rid"`
			} `json:"leaves"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &rec) != nil {
			continue
		}
		in := make(map[string]bool, len(rec.Leaves))
		for _, l := range rec.Leaves {
			in[l.Rid] = true
		}
		for _, rid := range group {
			if in[rid] {
				out[rid] = settlementItemRecord{
					RequestID: rid, TxHash: rec.TxHash,
					DetailsHash: rec.DetailsHash, BlockNumber: rec.BlockNumber,
				}
			}
		}
	}
	return out
}

// scanSPRecords reads billable records from one log file that belong to spEVM
// (worker→SP resolved) and are at/after sinceUnix. maxTS is the newest timestamp of
// ANY parsed record in the file (matching or not) — the caller uses it to stop
// descending into older rotated files once everything left predates `since`.
func (s *Settler) scanSPRecords(path, wantSPLower string, sinceUnix int64) (out []RequestRecord, maxTS time.Time, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, time.Time{}, nil
		}
		return nil, time.Time{}, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec RequestRecord
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec.Timestamp.After(maxTS) {
			maxTS = rec.Timestamp
		}
		// Same billable predicate as the scanner.
		if rec.Status != 200 || rec.TotalTokens <= 0 || rec.Wallet == "" || rec.ErrorReason != "" {
			continue
		}
		if sinceUnix > 0 && rec.Timestamp.Unix() < sinceUnix {
			continue
		}
		spEVM := s.aggregator.ResolveWorkerToEVM(rec.WorkerID)
		if spEVM == "" || strings.ToLower(spEVM) != wantSPLower {
			continue
		}
		out = append(out, rec)
	}
	return out, maxTS, sc.Err()
}

// loadItemsLedgerIndex reads settlement-items.jsonl into request_id → record. If a
// request appears more than once (shouldn't, but be safe), the last wins.
func (s *Settler) loadItemsLedgerIndex() map[string]settlementItemRecord {
	idx := make(map[string]settlementItemRecord)
	f, err := os.Open(s.itemsLedgerPath)
	if err != nil {
		return idx
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var r settlementItemRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.RequestID != "" {
			idx[r.RequestID] = r
		}
	}
	return idx
}
