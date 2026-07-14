package gateway

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
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
	return &RequestLogger{
		file:        f,
		path:        path,
		currentSize: size,
		maxSize:     maxFileSize,
		maxBackups:  defaultMaxBackups,
		logger:      logger,
	}
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
	n, err := rl.file.Write(data)
	if err != nil {
		metrics.RequestLogWriteErrors.Inc()
		rl.logger.Error("failed to write request record — request dropped from billing", "error", err)
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
