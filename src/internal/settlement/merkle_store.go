package settlement

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// merkle_store.go — R4: receipt-proof storage & indexing.
//
// F6 made receipt-proofs O(1) for settled requests by embedding each request's record
// into its Merkle leaf, but at soak scale that bloated the batch line to ~10 MB and left
// two holes the fourth soak exposed:
//   - a miss (unknown / not-yet-settled request_id) fell back to a full scan of the whole
//     (2 GB+) ledger — ~56 s, a DoS surface on the public :3001 query port;
//   - every restart re-parsed the whole ledger to warm the index.
//
// This file fixes both without changing the append-only, crash-safe money path:
//   - records live in a SEPARATE receipt-records.jsonl (point-read by (offset,len)); the
//     batch line keeps only {rid, leaf, sp} (~2 MB), enough for the Merkle proof math;
//   - a warm index of ALL settled rids means a miss is authoritative ("not settled") — no
//     fallback scan ever (see findMerkleBatch);
//   - both indexes persist to a sidecar (receipt-index.jsonl) written in lockstep with each
//     batch; on restart it is replayed if its recorded sizes still match the data files,
//     else the index is rebuilt from the data files (and the sidecar rewritten). The sidecar
//     is pure derived cache — any inconsistency degrades to a one-time rebuild, never to
//     wrong data.
//
// Backward compatible: pre-R4 batch lines still carry embedded records; the read path
// prefers an embedded record, then the external store, then the pre-F6 request-log scan.

// recordLoc points at a RequestRecord's bytes in receipt-records.jsonl.
type recordLoc struct {
	Off int64 `json:"o"`
	Len int64 `json:"n"`
}

// receiptIdxLine is one sidecar line. Normal appends and the rebuild's trailing marker
// both carry the true post-write sizes (lc/rc); the rebuild's per-batch lines carry 0 and
// are always followed by the marker, so the LAST line's sizes are always authoritative.
type receiptIdxLine struct {
	BatchOff   int64                `json:"bo"`           // offset of the batch line in the merkle ledger
	MerkleSize int64                `json:"lc"`           // merkle ledger size this line accounts up to
	RecordSize int64                `json:"rc"`           // record store size this line accounts up to
	Rids       []string             `json:"r,omitempty"`  // all rids in the batch (their merkle offset = BatchOff)
	Recs       map[string]recordLoc `json:"rl,omitempty"` // external record locs (subset of Rids; absent = embedded/legacy)
}

func (s *Settler) dataPrefix() string {
	return strings.TrimSuffix(s.itemsLedgerPath, "settlement-items.jsonl")
}
func (s *Settler) receiptRecordsPath() string { return s.dataPrefix() + "receipt-records.jsonl" }
func (s *Settler) receiptIndexPath() string   { return s.dataPrefix() + "receipt-index.jsonl" }

func fileSize(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return 0
}

// appendReceiptRecord appends one RequestRecord to receipt-records.jsonl and returns its
// (offset,len). Single-writer settler, so the O_APPEND write lands at the current end.
func (s *Settler) appendReceiptRecord(rid string, rec *RequestRecord) (recordLoc, bool) {
	f, err := os.OpenFile(s.receiptRecordsPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		s.logger.Error("failed to open receipt-records store", "error", err)
		return recordLoc{}, false
	}
	defer f.Close()
	line, err := json.Marshal(struct {
		Rid    string        `json:"rid"`
		Record RequestRecord `json:"record"`
	}{rid, *rec})
	if err != nil {
		return recordLoc{}, false
	}
	line = append(line, '\n')
	off, _ := f.Seek(0, io.SeekEnd)
	if _, werr := f.Write(line); werr != nil {
		return recordLoc{}, false
	}
	return recordLoc{Off: off, Len: int64(len(line))}, true
}

// loadRecordForRID point-reads a RequestRecord from receipt-records.jsonl via recordIdx.
func (s *Settler) loadRecordForRID(rid string) (RequestRecord, bool) {
	s.merkleIdxMu.Lock()
	if !s.merkleWarm {
		s.loadOrWarmIndexesLocked()
	}
	loc, ok := s.recordIdx[rid]
	s.merkleIdxMu.Unlock()
	if !ok {
		return RequestRecord{}, false
	}
	f, err := os.Open(s.receiptRecordsPath())
	if err != nil {
		return RequestRecord{}, false
	}
	defer f.Close()
	if _, err := f.Seek(loc.Off, io.SeekStart); err != nil {
		return RequestRecord{}, false
	}
	buf := make([]byte, loc.Len)
	if _, err := io.ReadFull(f, buf); err != nil {
		return RequestRecord{}, false
	}
	var e struct {
		Record RequestRecord `json:"record"`
	}
	if json.Unmarshal(bytes.TrimSpace(buf), &e) != nil {
		return RequestRecord{}, false
	}
	return e.Record, true
}

// appendReceiptIdxLine persists one batch's index entry (called under merkleIdxMu, after
// the record store and merkle ledger writes, so fileSize reflects them).
func (s *Settler) appendReceiptIdxLine(batchOff int64, rids []string, recs map[string]recordLoc) {
	b, err := json.Marshal(receiptIdxLine{
		BatchOff: batchOff, Rids: rids, Recs: recs,
		MerkleSize: fileSize(s.merkleLedgerPath()),
		RecordSize: fileSize(s.receiptRecordsPath()),
	})
	if err != nil {
		return
	}
	f, err := os.OpenFile(s.receiptIndexPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// WarmReceiptIndex eagerly warms the receipt indexes so the first receipt-proof query after
// a restart does not pay the one-time sidecar-replay (or rebuild) cost on the request path.
// Safe to call concurrently and repeatedly; a no-op once warm.
func (s *Settler) WarmReceiptIndex() {
	s.merkleIdxMu.Lock()
	defer s.merkleIdxMu.Unlock()
	if !s.merkleWarm {
		s.loadOrWarmIndexesLocked()
	}
}

// loadOrWarmIndexesLocked populates merkleIdx + recordIdx and sets merkleWarm. Caller holds
// merkleIdxMu. Fast path replays the sidecar; otherwise rebuilds from the data files.
func (s *Settler) loadOrWarmIndexesLocked() {
	if s.replaySidecarLocked() {
		s.merkleWarm = true
		return
	}
	s.rebuildFromDataLocked()
	s.merkleWarm = true
}

// replaySidecarLocked loads both indexes from receipt-index.jsonl and accepts them only if
// the sizes it accounts for still match the current data files (i.e. nothing was appended
// since it was last written — no crash gap). Returns false to force a rebuild otherwise.
func (s *Settler) replaySidecarLocked() bool {
	f, err := os.Open(s.receiptIndexPath())
	if err != nil {
		// No sidecar. Only trust an empty state if the data files are also empty.
		if fileSize(s.merkleLedgerPath()) == 0 && fileSize(s.receiptRecordsPath()) == 0 {
			s.merkleIdx = make(map[string]int64)
			s.recordIdx = make(map[string]recordLoc)
			return true
		}
		return false
	}
	defer f.Close()
	m := make(map[string]int64)
	rr := make(map[string]recordLoc)
	var lastMerkle, lastRecord int64
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			var e receiptIdxLine
			if json.Unmarshal(bytes.TrimSpace(line), &e) != nil {
				return false // corrupt sidecar → rebuild
			}
			for _, rid := range e.Rids {
				m[rid] = e.BatchOff
			}
			for rid, loc := range e.Recs {
				rr[rid] = loc
			}
			lastMerkle, lastRecord = e.MerkleSize, e.RecordSize
		}
		if rerr != nil {
			break
		}
	}
	if lastMerkle != fileSize(s.merkleLedgerPath()) || lastRecord != fileSize(s.receiptRecordsPath()) {
		return false
	}
	s.merkleIdx = m
	s.recordIdx = rr
	return true
}

// rebuildFromDataLocked rebuilds both indexes by scanning the data files once each, then
// rewrites the sidecar atomically so the next start is fast. Caller holds merkleIdxMu.
func (s *Settler) rebuildFromDataLocked() {
	s.recordIdx = s.scanRecordStoreLocked()
	s.merkleIdx = make(map[string]int64)

	tmp := s.receiptIndexPath() + ".tmp"
	out, oerr := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)

	if f, err := os.Open(s.merkleLedgerPath()); err == nil {
		defer f.Close()
		r := bufio.NewReaderSize(f, 1<<20)
		var off int64
		for {
			line, rerr := r.ReadBytes('\n')
			if len(line) > 0 {
				var rec struct {
					Leaves []struct {
						Rid string `json:"rid"`
					} `json:"leaves"`
				}
				if json.Unmarshal(bytes.TrimSpace(line), &rec) == nil {
					rids := make([]string, 0, len(rec.Leaves))
					recs := make(map[string]recordLoc)
					for _, l := range rec.Leaves {
						s.merkleIdx[l.Rid] = off
						rids = append(rids, l.Rid)
						if loc, ok := s.recordIdx[l.Rid]; ok {
							recs[l.Rid] = loc
						}
					}
					if oerr == nil {
						if b, e := json.Marshal(receiptIdxLine{BatchOff: off, Rids: rids, Recs: recs}); e == nil {
							_, _ = out.Write(append(b, '\n'))
						}
					}
				}
				off += int64(len(line))
			}
			if rerr != nil {
				break
			}
		}
	}
	if oerr == nil {
		// Trailing marker carries the authoritative post-rebuild sizes.
		if b, e := json.Marshal(receiptIdxLine{
			BatchOff:   -1,
			MerkleSize: fileSize(s.merkleLedgerPath()),
			RecordSize: fileSize(s.receiptRecordsPath()),
		}); e == nil {
			_, _ = out.Write(append(b, '\n'))
		}
		_ = out.Close()
		_ = os.Rename(tmp, s.receiptIndexPath())
	}
}

// scanRecordStoreLocked builds rid → (offset,len) over receipt-records.jsonl.
func (s *Settler) scanRecordStoreLocked() map[string]recordLoc {
	idx := make(map[string]recordLoc)
	f, err := os.Open(s.receiptRecordsPath())
	if err != nil {
		return idx
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var off int64
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			var e struct {
				Rid string `json:"rid"`
			}
			if json.Unmarshal(bytes.TrimSpace(line), &e) == nil && e.Rid != "" {
				idx[e.Rid] = recordLoc{Off: off, Len: int64(len(line))}
			}
			off += int64(len(line))
		}
		if rerr != nil {
			break
		}
	}
	return idx
}
