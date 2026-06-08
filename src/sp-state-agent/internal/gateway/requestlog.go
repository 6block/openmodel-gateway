package gateway

import (
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"
)

// RequestRecord is a structured log entry for each proxied request.
// Designed as the billing data source for M3's on-chain settlement.
type RequestRecord struct {
	RequestID        string    `json:"request_id"`
	Timestamp        time.Time `json:"timestamp"`
	APIKeyName       string    `json:"api_key_name"`        // Which API key was used
	Wallet           string    `json:"wallet,omitempty"`    // Associated wallet (M3)
	WorkerID         string    `json:"worker_id"`
	Path             string    `json:"path"`
	Model            string    `json:"model"`
	Status           int       `json:"status"`
	ErrorReason      string    `json:"error_reason,omitempty"` // "queue_full", "queue_timeout", "all_retries_failed", "stream_interrupted"
	DurationMs       int64     `json:"duration_ms"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
}

// RequestLogger writes structured request records to a JSONL file.
// Each line is one JSON object — easy to parse, tail, and ingest into billing systems.
// Automatically rotates when file exceeds maxFileSize (keeps one .1 backup).
type RequestLogger struct {
	mu          sync.Mutex
	file        *os.File
	path        string
	currentSize int64
	logger      *slog.Logger
}

// maxFileSize is the rotation threshold (50 MB).
const maxFileSize = 50 << 20

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
		rl.logger.Error("failed to marshal request record", "error", err)
		return
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	data = append(data, '\n')
	n, err := rl.file.Write(data)
	if err != nil {
		rl.logger.Error("failed to write request record", "error", err)
		return
	}
	rl.currentSize += int64(n)

	// Rotate if file exceeds threshold
	if rl.currentSize >= maxFileSize {
		rl.rotate()
	}
}

// rotate closes the current file, renames it to .1, and opens a new one.
func (rl *RequestLogger) rotate() {
	rl.file.Close()

	backup := rl.path + ".1"
	os.Remove(backup)                // Remove old backup
	os.Rename(rl.path, backup)       // Current → .1

	f, err := os.OpenFile(rl.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		rl.logger.Error("failed to open new log file after rotation", "error", err)
		return
	}
	rl.file = f
	rl.currentSize = 0
	rl.logger.Info("request log rotated", "backup", backup)
}

// Close closes the log file.
func (rl *RequestLogger) Close() error {
	if rl == nil {
		return nil
	}
	return rl.file.Close()
}
