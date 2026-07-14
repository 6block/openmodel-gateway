package settlement

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// migrate.go — one-shot offline migration of a pre-R4 "fat" merkle ledger (records embedded
// in each batch line) into the R4 layout: a THIN merkle-batches.jsonl ({rid,leaf,sp} only),
// an external receipt-records.jsonl, and a receipt-index.jsonl sidecar. Run it with the
// gateway STOPPED, before starting the R4 build, so the first proof is instant and the
// ledger shrinks (~5x). It is safe to skip — the R4 read path resolves fat lines directly
// and rebuilds the index on first start — but skipping keeps the old bloat and pays a
// one-time full rebuild on that first start.

// MigrationStats reports what a migration did.
type MigrationStats struct {
	Batches       int
	Leaves        int
	RecordsMoved  int   // leaves whose embedded record was externalized
	OldLedgerSize int64 // bytes, before
	NewLedgerSize int64 // bytes, after (thin)
	RecordSize    int64 // bytes, external record store
}

// MigrateFatLedger converts the merkle ledger in dir to the R4 layout. It streams line by
// line (bounded memory, safe on a multi-GB ledger), writes .new files, then renames them
// into place with the original ledger preserved as merkle-batches.jsonl.pre-r4.
//
// It refuses to run if receipt-records.jsonl already exists and is non-empty — that means
// the ledger was already migrated (or the gateway is producing thin lines), and re-running
// would strip records that are no longer embedded. Returns (nil stats, nil) if there is no
// ledger to migrate.
func MigrateFatLedger(dir string) (*MigrationStats, error) {
	ledgerPath := filepath.Join(dir, "merkle-batches.jsonl")
	recordsPath := filepath.Join(dir, "receipt-records.jsonl")
	indexPath := filepath.Join(dir, "receipt-index.jsonl")

	oldSize := fileSize(ledgerPath)
	if oldSize == 0 {
		return nil, nil // nothing to migrate
	}
	if fileSize(recordsPath) > 0 {
		return nil, fmt.Errorf("receipt-records.jsonl already exists (%d bytes) — ledger looks already migrated; refusing to run", fileSize(recordsPath))
	}

	in, err := os.Open(ledgerPath)
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	defer in.Close()

	ledgerNew := ledgerPath + ".new"
	recordsNew := recordsPath + ".new"
	indexNew := indexPath + ".new"
	lf, err := os.OpenFile(ledgerNew, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("create thin ledger: %w", err)
	}
	rf, err := os.OpenFile(recordsNew, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		lf.Close()
		return nil, fmt.Errorf("create record store: %w", err)
	}
	xf, err := os.OpenFile(indexNew, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		lf.Close()
		rf.Close()
		return nil, fmt.Errorf("create sidecar: %w", err)
	}

	var stats MigrationStats
	var ledgerOff, recordOff int64
	r := bufio.NewReaderSize(in, 1<<20)
	fail := func(e error) (*MigrationStats, error) {
		lf.Close()
		rf.Close()
		xf.Close()
		_ = os.Remove(ledgerNew)
		_ = os.Remove(recordsNew)
		_ = os.Remove(indexNew)
		return nil, e
	}
	for {
		line, rerr := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var batch merkleBatchRecord
			if json.Unmarshal(bytes.TrimSpace(line), &batch) != nil {
				if rerr != nil {
					break
				}
				continue // skip an unparseable line rather than abort the whole migration
			}
			stats.Batches++
			rids := make([]string, 0, len(batch.Leaves))
			recs := make(map[string]recordLoc)
			for i := range batch.Leaves {
				l := &batch.Leaves[i]
				stats.Leaves++
				rids = append(rids, l.Rid)
				if l.Record != nil {
					rline, _ := json.Marshal(struct {
						Rid    string        `json:"rid"`
						Record RequestRecord `json:"record"`
					}{l.Rid, *l.Record})
					rline = append(rline, '\n')
					if _, werr := rf.Write(rline); werr != nil {
						return fail(fmt.Errorf("write record store: %w", werr))
					}
					recs[l.Rid] = recordLoc{Off: recordOff, Len: int64(len(rline))}
					recordOff += int64(len(rline))
					stats.RecordsMoved++
					l.Record = nil // strip → thin
				}
			}
			thin, _ := json.Marshal(batch)
			thin = append(thin, '\n')
			if _, werr := lf.Write(thin); werr != nil {
				return fail(fmt.Errorf("write thin ledger: %w", werr))
			}
			if _, werr := xf.Write(append(mustJSON(receiptIdxLine{BatchOff: ledgerOff, Rids: rids, Recs: recs}), '\n')); werr != nil {
				return fail(fmt.Errorf("write sidecar: %w", werr))
			}
			ledgerOff += int64(len(thin))
		}
		if rerr != nil {
			break
		}
	}
	// Trailing marker with the final sizes (matches rebuildFromDataLocked's format).
	if _, werr := xf.Write(append(mustJSON(receiptIdxLine{BatchOff: -1, MerkleSize: ledgerOff, RecordSize: recordOff}), '\n')); werr != nil {
		return fail(fmt.Errorf("write sidecar marker: %w", werr))
	}
	for _, f := range []*os.File{lf, rf, xf} {
		if serr := f.Sync(); serr != nil {
			return fail(fmt.Errorf("sync: %w", serr))
		}
		if cerr := f.Close(); cerr != nil {
			return fail(fmt.Errorf("close: %w", cerr))
		}
	}

	// Preserve the original, then swap the new files in. Order: records + sidecar first,
	// thin ledger last — so if this is interrupted, the old fat ledger is still the file the
	// gateway would read, and the next start rebuilds cleanly.
	if err := os.Rename(recordsNew, recordsPath); err != nil {
		return nil, fmt.Errorf("install record store: %w", err)
	}
	if err := os.Rename(indexNew, indexPath); err != nil {
		return nil, fmt.Errorf("install sidecar: %w", err)
	}
	if err := os.Rename(ledgerPath, ledgerPath+".pre-r4"); err != nil {
		return nil, fmt.Errorf("back up fat ledger: %w", err)
	}
	if err := os.Rename(ledgerNew, ledgerPath); err != nil {
		return nil, fmt.Errorf("install thin ledger: %w", err)
	}
	stats.NewLedgerSize = ledgerOff
	stats.RecordSize = recordOff
	stats.OldLedgerSize = oldSize
	return &stats, nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
