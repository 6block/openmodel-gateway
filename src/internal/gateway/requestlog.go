package gateway

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"openmodel/sp-state-agent/internal/metrics"
)

// RequestRecord is a structured log entry for each proxied request.
// Designed as the billing data source for M3's on-chain settlement.
type RequestRecord struct {
	RequestID        string    `json:"request_id"`
	Timestamp        time.Time `json:"timestamp"`
	APIKeyName       string    `json:"api_key_name"`     // Which API key was used
	Wallet           string    `json:"wallet,omitempty"` // Associated wallet (M3)
	WorkerID         string    `json:"worker_id"`
	Path             string    `json:"path"`
	Model            string    `json:"model"`
	Status           int       `json:"status"`
	ErrorReason      string    `json:"error_reason,omitempty"` // "queue_full", "queue_timeout", "all_retries_failed", "stream_interrupted"
	DurationMs       int64     `json:"duration_ms"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	CachedTokens     int       `json:"cached_tokens,omitempty"` // prompt tokens served from prefix cache (for cache-read billing)
	TotalTokens      int       `json:"total_tokens"`
	// Resumes counts mid-stream continuations (B2): how many times this stream was
	// interrupted server-side and seamlessly resumed on another worker. 0 for the
	// overwhelmingly common uninterrupted case (omitted from JSON).
	Resumes int `json:"resumes,omitempty"`
	// Receipt is the worker-signed inference receipt (A1), verified by the gateway.
	// Settlement commits a Merkle root over receipts into the on-chain batch hash,
	// so its presence here makes the ledger row user-verifiable end-to-end.
	Receipt *ReceiptInfo `json:"receipt,omitempty"`
	// Quantity / AssetHash are what the GATEWAY counted and hashed in the response it
	// delivered, for an asset-billed model (image, audio_second). They are first-hand
	// and present whether or not the worker sent a receipt — the receipt only ever
	// attests them. Settlement bills from these, never from the receipt alone, so a
	// worker that omits its receipt cannot make a delivered image settle as zero.
	Quantity  int    `json:"quantity,omitempty"`
	AssetHash string `json:"asset_hash,omitempty"`
}

// RequestLogger writes structured request records to a JSONL file.
// Each line is one JSON object — easy to parse, tail, and ingest into billing systems.
// Automatically rotates when the file exceeds maxSize, keeping maxBackups numbered
// backups (.1 newest … .N oldest). Retention (maxSize × maxBackups) must exceed the
// settlement interval × peak request rate, or settlement can fall behind rotation
// and lose un-billed records — so both are configurable and default generously.
type RequestLogger struct {
	mu          sync.Mutex
	file        *os.File
	path        string
	currentSize int64
	maxSize     int64 // rotation threshold; defaults to maxFileSize (overridable in tests)
	maxBackups  int   // numbered backups kept (.1 … .maxBackups)
	logger      *slog.Logger

	// Ledger-space gate: a request whose record cannot be written is served but
	// never billed. With the data disk full every request was answered 200 and
	// dropped from billing — free service for as long as the disk stayed full.
	// Writable() lets the entry points refuse billable work up front instead.
	spaceCheckedAt atomic.Int64                            // unix nanos of the last free-space probe
	spaceOK        atomic.Bool                             // last probe found room and no write has failed since
	freeBytes      func(dir string) (uint64, error)        // injectable for tests
	write          func(f *os.File, b []byte) (int, error) // nil = f.Write; tests inject short writes
}

// minLedgerFreeBytes is the free space below which billable requests are
// refused: comfortably more than any burst of records written between probes.
const minLedgerFreeBytes = 8 << 20

// spaceProbeInterval bounds how often free space is queried (one statfs).
const spaceProbeInterval = time.Second

// Writable reports whether request records can currently be persisted: enough
// free space on the log's filesystem and no write failure since space was last
// confirmed. A nil logger (logging disabled) is always writable.
func (rl *RequestLogger) Writable() bool {
	if rl == nil {
		return true
	}
	now := time.Now().UnixNano()
	if last := rl.spaceCheckedAt.Load(); now-last >= int64(spaceProbeInterval) &&
		rl.spaceCheckedAt.CompareAndSwap(last, now) {
		free, err := rl.freeBytes(filepath.Dir(rl.path))
		rl.spaceOK.Store(err != nil || free >= minLedgerFreeBytes) // unknown → do not block
	}
	return rl.spaceOK.Load()
}

// statfsFree returns the bytes available to unprivileged writers under dir.
func statfsFree(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// maxFileSize is the default rotation threshold (50 MB).
const maxFileSize = 50 << 20

// defaultMaxBackups keeps ~500 MB of history by default (50 MB × 10), enough that
// the hourly settlement cycle stays well within retention even at high throughput.
const defaultMaxBackups = 10

// NewRequestLogger creates a logger that appends to the given file path.
// Returns nil if path is empty (logging disabled).
func NewRequestLogger(path string, logger *slog.Logger) *RequestLogger {
	if path == "" {
		return nil
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		logger.Error("failed to open request log file", "path", path, "error", err)
		return nil
	}

	// Get current file size for rotation tracking
	info, _ := f.Stat()
	var size int64
	if info != nil {
		size = info.Size()
	}

	logger.Info("request logging enabled", "path", path, "current_size_mb", size>>20)
	rl := &RequestLogger{
		file:        f,
		path:        path,
		currentSize: size,
		maxSize:     maxFileSize,
		maxBackups:  defaultMaxBackups,
		logger:      logger,
		freeBytes:   statfsFree,
	}
	rl.spaceOK.Store(true)
	return rl
}

// Log writes a request record as a single JSON line.
func (rl *RequestLogger) Log(rec RequestRecord) {
	if rl == nil {
		return
	}

	data, err := json.Marshal(rec)
	if err != nil {
		metrics.RequestLogWriteErrors.Inc()
		rl.logger.Error("failed to marshal request record — request dropped from billing", "error", err)
		return
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	data = append(data, '\n')
	write := rl.write
	if write == nil {
		write = (*os.File).Write
	}
	n, err := write(rl.file, data)
	if err != nil {
		// A short write (disk full mid-record) leaves half a line with no newline;
		// the next record that does get written is appended onto it and the whole
		// line fails to parse — that later, successfully served request was lost
		// from billing too (seen in the disk drill). Cut the fragment off.
		if n > 0 {
			if terr := rl.file.Truncate(rl.currentSize); terr != nil {
				rl.logger.Error("failed to truncate a partial request record — the next record may be lost",
					"error", terr)
			}
		}
		metrics.RequestLogWriteErrors.Inc()
		rl.logger.Error("failed to write request record — request dropped from billing", "error", err)
		rl.spaceOK.Store(false) // closed until a probe finds room again
		return
	}
	rl.currentSize += int64(n)

	// Rotate if file exceeds threshold
	if rl.currentSize >= rl.maxSize {
		rl.rotate()
	}
}

// rotate closes the current file and shifts the numbered backups — the oldest
// (.maxBackups) is discarded, each .i becomes .i+1, and the current file becomes
// .1 — then opens a fresh file. Keeping maxBackups copies lets settlement fall
// behind by up to maxBackups rotations without losing any un-billed record.
func (rl *RequestLogger) rotate() {
	rl.file.Close()

	backups := rl.maxBackups
	if backups < 1 {
		backups = 1
	}
	os.Remove(fmt.Sprintf("%s.%d", rl.path, backups)) // discard the oldest
	for i := backups - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", rl.path, i), fmt.Sprintf("%s.%d", rl.path, i+1))
	}
	os.Rename(rl.path, rl.path+".1") // current → .1

	f, err := os.OpenFile(rl.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		rl.logger.Error("failed to open new log file after rotation", "error", err)
		return
	}
	rl.file = f
	rl.currentSize = 0
	rl.logger.Info("request log rotated", "backups_kept", backups)
}

// Close closes the log file.
func (rl *RequestLogger) Close() error {
	if rl == nil {
		return nil
	}
	return rl.file.Close()
}
