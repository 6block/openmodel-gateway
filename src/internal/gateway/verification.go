package gateway

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"sync"

	"openmodel/sp-state-agent/internal/metrics"
)

// VerificationSample is one retained request/response pair, kept for offline SP fraud
// detection. It captures what the SP *claimed* (model returned, tokens reported) alongside
// the actual prompt and delivered response, so a later offline verifier can re-check the
// claim: re-run the prompt against a reference of the same model (output divergence →
// model substitution), probe capability, or flag token/composition anomalies. This layer
// only *retains* the evidence; it renders no verdict.
type VerificationSample struct {
	RequestID  string `json:"request_id"`
	Timestamp  string `json:"timestamp"` // RFC3339
	WorkerID   string `json:"worker_id"`
	APIKeyName string `json:"api_key_name,omitempty"`
	ModelReq   string `json:"model_req"`            // model the client asked for
	ModelResp  string `json:"model_resp,omitempty"` // model the worker claims it served ("model" field in response)
	Stream     bool   `json:"stream"`
	Status     int    `json:"status"`
	// Token counts as reported by the worker (post-clamp, the billed figures).
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CachedTokens     int `json:"cached_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// Evidence. Request is the raw client body. Response is the worker's raw body for
	// non-stream, or the concatenated delivered content for stream. Both may be truncated
	// to sampleMaxBodyBytes to bound disk use — Truncated records that.
	Request   string `json:"request"`
	Response  string `json:"response"`
	Truncated bool   `json:"truncated,omitempty"`
}

// sampleMaxBodyBytes caps each retained request/response body so a pathological megabyte
// prompt can't blow up the sample log. Enough to fingerprint the prompt + judge the reply.
const sampleMaxBodyBytes = 16 << 10 // 16 KiB

// verificationSampler retains a random fraction of served requests to a rotating JSONL
// file. It is opt-in (nil when disabled) and mirrors RequestLogger's numbered-backup
// rotation. A miss (rand ≥ rate) short-circuits before any capture cost is paid.
type verificationSampler struct {
	rate       float64
	mu         sync.Mutex
	file       *os.File
	path       string
	current    int64
	maxSize    int64
	maxBackups int
	logger     *slog.Logger
}

const (
	defaultSampleMaxSize = 50 << 20 // 50 MB per file
	defaultSampleBackups = 5
)

// NewVerificationSampler opens (or creates) the sample log. Returns nil — sampling
// disabled — when rate <= 0 or path is empty, so callers can guard with a nil check
// and pay nothing when the feature is off.
func NewVerificationSampler(path string, rate float64, maxMB, backups int, logger *slog.Logger) *verificationSampler {
	if rate <= 0 || path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		logger.Error("failed to open verification sample log — sampling disabled", "path", path, "error", err)
		return nil
	}
	maxSize := int64(defaultSampleMaxSize)
	if maxMB > 0 {
		maxSize = int64(maxMB) << 20
	}
	if backups <= 0 {
		backups = defaultSampleBackups
	}
	if rate > 1 {
		rate = 1
	}
	info, _ := f.Stat()
	var size int64
	if info != nil {
		size = info.Size()
	}
	logger.Info("verification sampling enabled", "path", path, "sample_rate", rate, "max_mb", maxSize>>20, "backups", backups)
	return &verificationSampler{
		rate:       rate,
		file:       f,
		path:       path,
		current:    size,
		maxSize:    maxSize,
		maxBackups: backups,
		logger:     logger,
	}
}

// shouldSample reports whether this request should be retained. Safe on a nil sampler
// (returns false), so the hot path is a single nil+bool check when disabled.
func (vs *verificationSampler) shouldSample() bool {
	if vs == nil {
		return false
	}
	return rand.Float64() < vs.rate
}

// write appends one sample as a JSON line, truncating oversized bodies first. Safe on a
// nil sampler (no-op). Never blocks the request path beyond the marshal + append.
func (vs *verificationSampler) write(s VerificationSample) {
	if vs == nil {
		return
	}
	if len(s.Request) > sampleMaxBodyBytes {
		s.Request = s.Request[:sampleMaxBodyBytes]
		s.Truncated = true
	}
	if len(s.Response) > sampleMaxBodyBytes {
		s.Response = s.Response[:sampleMaxBodyBytes]
		s.Truncated = true
	}
	data, err := json.Marshal(s)
	if err != nil {
		metrics.VerificationSampleWriteErrors.Inc()
		vs.logger.Error("failed to marshal verification sample", "error", err)
		return
	}
	data = append(data, '\n')

	vs.mu.Lock()
	defer vs.mu.Unlock()
	n, err := vs.file.Write(data)
	if err != nil {
		metrics.VerificationSampleWriteErrors.Inc()
		vs.logger.Error("failed to write verification sample", "error", err)
		return
	}
	vs.current += int64(n)
	metrics.VerificationSamplesRetained.Inc()
	if vs.current >= vs.maxSize {
		vs.rotate()
	}
}

// rotate shifts numbered backups (.maxBackups discarded, .i→.i+1, current→.1) and opens
// a fresh file. Mirrors RequestLogger.rotate. Caller holds vs.mu.
func (vs *verificationSampler) rotate() {
	vs.file.Close()
	backups := vs.maxBackups
	if backups < 1 {
		backups = 1
	}
	os.Remove(fmt.Sprintf("%s.%d", vs.path, backups))
	for i := backups - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", vs.path, i), fmt.Sprintf("%s.%d", vs.path, i+1))
	}
	os.Rename(vs.path, vs.path+".1")

	f, err := os.OpenFile(vs.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		vs.logger.Error("failed to open new verification sample file after rotation", "error", err)
		return
	}
	vs.file = f
	vs.current = 0
}

// close flushes and closes the underlying file. Safe on nil.
func (vs *verificationSampler) close() {
	if vs == nil {
		return
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if vs.file != nil {
		vs.file.Close()
	}
}
