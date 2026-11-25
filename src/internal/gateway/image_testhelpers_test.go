package gateway

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// mockPNG returns a tiny VALID PNG whose bytes differ per seed.
//
// A real decodable image, not a placeholder string: the gateway decodes and hashes the
// bytes it forwards, so a fixture that is not a real PNG would exercise a path that never
// runs in production.
func mockPNG(seed int) []byte {
	const size = 2
	raw := make([]byte, 0, size*(1+size*3))
	for y := 0; y < size; y++ {
		raw = append(raw, 0) // per-scanline filter byte
		for x := 0; x < size; x++ {
			raw = append(raw, byte((seed*60+x*20+y*5)%256), 90, 160)
		}
	}

	var buf []byte
	buf = append(buf, 0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n')
	chunk := func(tag string, data []byte) {
		n := uint32(len(data))
		buf = append(buf, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
		buf = append(buf, tag...)
		buf = append(buf, data...)
		c := crc32.ChecksumIEEE(append([]byte(tag), data...))
		buf = append(buf, byte(c>>24), byte(c>>16), byte(c>>8), byte(c))
	}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	zw.Write(raw)
	zw.Close()
	chunk("IHDR", []byte{0, 0, 0, size, 0, 0, 0, size, 8, 2, 0, 0, 0})
	chunk("IDAT", compressed.Bytes())
	chunk("IEND", nil)
	return buf
}

// hashImagesForTest is the digest of `n` mock images concatenated, i.e. exactly what the
// gateway computes from a well-formed response. Used to build a receipt whose asset_hash
// agrees with the delivered bytes.
func hashImagesForTest(t *testing.T, n int) string {
	t.Helper()
	h := sha256.New()
	for i := 0; i < n; i++ {
		h.Write(mockPNG(i))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// newCapturingLogger returns a RequestLogger writing to a temp file.
//
// The records are NOT returned immediately: the request under test has not run yet. The
// test calls readLogged itself after the request — see the ledger test. A cleanup closes
// the file so the last line is flushed even if a test fails early.
func newCapturingLogger(t *testing.T, out *[]RequestRecord) *RequestLogger {
	t.Helper()
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	rl := NewRequestLogger(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		rl.Close()
		if out != nil && len(*out) == 0 {
			*out = readLogged(t, path)
		}
	})
	// Expose the path so a test can read it mid-flight.
	testLogPath = path
	return rl
}

// testLogPath is set by newCapturingLogger so a test can read the log after the request
// but before cleanup. Single-threaded test use only.
var testLogPath string

func readLoggedNow(t *testing.T, out *[]RequestRecord) {
	t.Helper()
	if testLogPath == "" {
		t.Fatal("newCapturingLogger was not called")
	}
	*out = readLogged(t, testLogPath)
}

func readLogged(t *testing.T, path string) []RequestRecord {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []RequestRecord
	for _, line := range splitJSONLines(raw) {
		var rec RequestRecord
		if json.Unmarshal(line, &rec) == nil && rec.RequestID != "" {
			out = append(out, rec)
		}
	}
	return out
}

func splitJSONLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
