package settlement

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"
)

type RequestRecord struct {
	RequestID        string    `json:"request_id"`
	Timestamp        time.Time `json:"timestamp"`
	APIKeyName       string    `json:"api_key_name"`
	Wallet           string    `json:"wallet,omitempty"`
	WorkerID         string    `json:"worker_id"`
	Path             string    `json:"path"`
	Model            string    `json:"model"`
	Status           int       `json:"status"`
	ErrorReason      string    `json:"error_reason,omitempty"`
	DurationMs       int64     `json:"duration_ms"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	CachedTokens     int       `json:"cached_tokens,omitempty"` // prompt tokens served from prefix cache
	TotalTokens      int       `json:"total_tokens"`
	// Receipt is the worker-signed attestation captured by the gateway (A1); only the
	// fields the Merkle leaf needs are decoded here (extra JSON fields are ignored).
	Receipt *RecordReceipt `json:"receipt,omitempty"`
}

// RecordReceipt mirrors the gateway's ReceiptInfo in FULL: the public receipt-proof
// endpoint serves this to external verifiers, who must be able to re-verify the
// worker's ed25519 signature over the canonical payload — every signed field must
// therefore survive the round-trip through this ledger projection.
type RecordReceipt struct {
	V                int    `json:"v"`
	RequestID        string `json:"request_id"`
	Model            string `json:"model"`
	RequestSHA256    string `json:"request_sha256"`
	ResponseSHA256   string `json:"response_sha256"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	CachedTokens     int    `json:"cached_tokens"`
	TS               int64  `json:"ts"`
	Pubkey           string `json:"pubkey"`
	Sig              string `json:"sig"`
	Verified         bool   `json:"verified"`
	VerifyError      string `json:"verify_error,omitempty"`
}

type Cursor struct {
	Offset        int64     `json:"offset"`
	LastTimestamp time.Time `json:"last_timestamp"`
	FileSize      int64     `json:"file_size"`
	// Inode/Dev identify the physical file the Offset belongs to. Tracking file
	// identity lets us detect log rotation (rename to .1 + fresh file) reliably —
	// even when the new file has already grown past the old size, which the old
	// "size decreased" heuristic missed and which caused billable records in the
	// rotated-away .1 to be skipped (audit CRITICAL). Zero = unknown (legacy
	// cursor / first run).
	Inode uint64 `json:"inode,omitempty"`
	Dev   uint64 `json:"dev,omitempty"`
}

type Scanner struct {
	logPath    string
	cursorPath string
	logger     *slog.Logger
}

func NewScanner(logPath, cursorPath string, logger *slog.Logger) *Scanner {
	return &Scanner{
		logPath:    logPath,
		cursorPath: cursorPath,
		logger:     logger,
	}
}

// fileIdentity returns the (dev, inode) of a stat result, or ok=false if the
// platform doesn't expose it. Used to detect rotation by file identity.
func fileIdentity(info os.FileInfo) (dev, ino uint64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}

// Peek reads new billable records starting from the saved cursor WITHOUT
// advancing it. The caller must call CommitCursor(newCursor) only after the
// records have been durably settled on-chain. This decouples reading from
// committing so a settlement failure never loses records.
//
// Rotation handling (audit CRITICAL fix): the gateway rotates requests.jsonl by
// renaming it to requests.jsonl.1 and creating a fresh file. We detect this by
// comparing the current main file's inode against the cursor's recorded inode
// (and, as a fallback, by the file shrinking below the cursor offset). On
// rotation we FIRST drain the unsettled tail of the rotated-away .1 (from the
// old offset to its end) and THEN read the new main from the beginning, so no
// billable record written before the rotation is ever skipped.
func (s *Scanner) Peek() (records []RequestRecord, oldCursor, newCursor Cursor, err error) {
	oldCursor, loadErr := s.loadCursor()
	if loadErr != nil {
		s.logger.Warn("failed to load cursor, starting from beginning", "error", loadErr)
		oldCursor = Cursor{}
	}

	mainInfo, statErr := os.Stat(s.logPath)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			// Main file was just rotated away and not yet recreated. Read the .1
			// tail from the old offset, but do NOT advance the cursor — the main
			// file reappears momentarily and the rotation branch handles it then.
			backupPath := s.logPath + ".1"
			if _, bErr := os.Stat(backupPath); bErr == nil {
				tail, _, _, scanErr := s.scanFrom(backupPath, oldCursor.Offset)
				if scanErr == nil && len(tail) > 0 {
					return tail, oldCursor, oldCursor, nil
				}
			}
			return nil, oldCursor, oldCursor, nil
		}
		return nil, oldCursor, oldCursor, fmt.Errorf("stat log file: %w", statErr)
	}

	mainDev, mainIno, idOK := fileIdentity(mainInfo)

	// Rotation detected if (a) the main file's identity changed vs the cursor, or
	// (b) the file is now smaller than where we last read (shrink/truncation,
	// covers inode reuse and platforms without inode support).
	rotated := (idOK && oldCursor.Inode != 0 && mainIno != oldCursor.Inode) ||
		(oldCursor.Offset > 0 && mainInfo.Size() < oldCursor.Offset)

	var lastTS time.Time
	if rotated {
		// Drain EVERY un-settled record still on disk: start at the backup that holds
		// the cursor's file (matched by inode), read its tail from the cursor offset,
		// then read each newer backup (.i-1 … .1) and finally the new main, all in
		// chronological order. Keeping maxBackups copies means multiple rotations
		// within one settlement cycle no longer lose records — the old code only ever
		// read .1, so a second rotation silently dropped the un-billed tail.
		drained := false
		if oldCursor.Inode != 0 {
			for i := 1; ; i++ {
				bp := fmt.Sprintf("%s.%d", s.logPath, i)
				bInfo, bErr := os.Stat(bp)
				if bErr != nil {
					break // no more numbered backups
				}
				_, bIno, bOK := fileIdentity(bInfo)
				if !bOK || bIno != oldCursor.Inode {
					continue
				}
				// .i is the file the cursor was reading.
				tail, _, tailTS, scanErr := s.scanFrom(bp, oldCursor.Offset)
				if scanErr != nil {
					return nil, oldCursor, oldCursor, fmt.Errorf("scan rotated tail %s: %w", bp, scanErr)
				}
				records = append(records, tail...)
				if len(tail) > 0 {
					lastTS = tailTS
				}
				for j := i - 1; j >= 1; j-- { // newer backups, fully un-read
					np := fmt.Sprintf("%s.%d", s.logPath, j)
					recs, _, ts, scanErr := s.scanFrom(np, 0)
					if scanErr != nil {
						return nil, oldCursor, oldCursor, fmt.Errorf("scan rotated backup %s: %w", np, scanErr)
					}
					records = append(records, recs...)
					if len(recs) > 0 {
						lastTS = ts
					}
				}
				s.logger.Info("log rotation detected, drained unsettled records from backups",
					"cursor_backup", bp, "newer_backups", i-1, "tail_and_backup_records", len(records))
				drained = true
				break
			}
		}
		if !drained {
			// Legacy cursor (no inode) or the cursor's file has rotated beyond the
			// retained backups. Best effort: drain .1 from the old offset (covers the
			// common legacy/first-run case); if the cursor file is truly gone, warn.
			backupPath := s.logPath + ".1"
			if bInfo, bErr := os.Stat(backupPath); bErr == nil {
				_, bIno, bOK := fileIdentity(bInfo)
				startAt := int64(0)
				if oldCursor.Inode == 0 || (bOK && bIno == oldCursor.Inode) {
					startAt = oldCursor.Offset
				} else {
					s.logger.Warn("log rotated beyond retained backups — un-billed records may be lost; "+
						"increase request_log_backups or settle more often",
						"cursor_inode", oldCursor.Inode)
				}
				tail, _, tailTS, scanErr := s.scanFrom(backupPath, startAt)
				if scanErr == nil {
					records = append(records, tail...)
					if len(tail) > 0 {
						lastTS = tailTS
					}
				}
			} else {
				s.logger.Warn("log rotation detected but no .1 backup present — tail records may be lost")
			}
		}

		newRecs, end, newTS, scanErr := s.scanFrom(s.logPath, 0)
		if scanErr != nil {
			return nil, oldCursor, oldCursor, fmt.Errorf("scan rotated main: %w", scanErr)
		}
		records = append(records, newRecs...)
		if len(newRecs) > 0 {
			lastTS = newTS
		}
		if lastTS.IsZero() {
			lastTS = oldCursor.LastTimestamp
		}
		newCursor = Cursor{Offset: end, LastTimestamp: lastTS, FileSize: mainInfo.Size(), Inode: mainIno, Dev: mainDev}
		return records, oldCursor, newCursor, nil
	}

	// Normal path: no rotation, read the main file from the saved offset.
	recs, end, ts, scanErr := s.scanFrom(s.logPath, oldCursor.Offset)
	if scanErr != nil {
		return nil, oldCursor, oldCursor, fmt.Errorf("scan log file: %w", scanErr)
	}
	if ts.IsZero() {
		ts = oldCursor.LastTimestamp
	}
	newCursor = Cursor{Offset: end, LastTimestamp: ts, FileSize: mainInfo.Size(), Inode: mainIno, Dev: mainDev}
	return recs, oldCursor, newCursor, nil
}

// CommitCursor durably advances the cursor. Call only after on-chain confirmation.
func (s *Scanner) CommitCursor(c Cursor) error {
	return s.saveCursor(c)
}

// scanFrom reads billable records from path starting at startOffset, returning
// the records, the resulting end offset, and the last record's timestamp.
func (s *Scanner) scanFrom(path string, startOffset int64) (records []RequestRecord, endOffset int64, lastTS time.Time, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, startOffset, time.Time{}, err
	}
	defer f.Close()

	if startOffset > 0 {
		if _, err := f.Seek(startOffset, 0); err != nil {
			return nil, startOffset, time.Time{}, fmt.Errorf("seek to offset %d: %w", startOffset, err)
		}
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec RequestRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			s.logger.Warn("skipping malformed JSONL line", "error", err)
			continue
		}
		if !s.isBillable(rec) {
			continue
		}
		records = append(records, rec)
		lastTS = rec.Timestamp
	}
	if err := scanner.Err(); err != nil {
		return records, startOffset, lastTS, fmt.Errorf("scan error: %w", err)
	}

	endOffset, _ = f.Seek(0, 1)
	return records, endOffset, lastTS, nil
}

func (s *Scanner) isBillable(rec RequestRecord) bool {
	return rec.Status == 200 &&
		rec.TotalTokens > 0 &&
		rec.Wallet != "" &&
		rec.ErrorReason == ""
}

func (s *Scanner) loadCursor() (Cursor, error) {
	data, err := os.ReadFile(s.cursorPath)
	if err != nil {
		return Cursor{}, err
	}
	var c Cursor
	if err := json.Unmarshal(data, &c); err != nil {
		return Cursor{}, err
	}
	return c, nil
}

func (s *Scanner) saveCursor(c Cursor) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmpPath := s.cursorPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.cursorPath)
}
