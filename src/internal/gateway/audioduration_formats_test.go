package gateway

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// The fixtures in testdata/audiofmt were encoded by FFmpeg (through PyAV) at several
// sample rates and lengths; truth.json records the duration a full decode produced.
// The gateway's header-only measurement must land on the same billable second.
//
// AAC in MP4 is the one codec whose decoded length exceeds the content: the encoder's
// priming and padding frames (≈1024 + up to 1023 samples) are part of the declared
// track duration. Both sides measure the declared duration, so they agree; the
// tolerance below allows for that padding only for AAC. For Opus (Ogg and WebM) the
// measurement is the length the container declares (RFC 7845: last granule position
// minus pre-skip; Matroska: last block end minus DiscardPadding and CodecDelay). An
// FFmpeg-encoded Opus file declares 312 samples (6.5 ms) more than FFmpeg then decodes,
// so Opus gets 10 ms; the measurement is never shorter than the audio.
func TestAudioDuration_CompressedFormatsMatchADecode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "audiofmt", "truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var truth map[string]struct {
		Decoded  float64 `json:"decoded"`
		Intended float64 `json:"intended"`
	}
	if err := json.Unmarshal(raw, &truth); err != nil {
		t.Fatal(err)
	}
	if len(truth) < 21 {
		t.Fatalf("expected 21 fixtures, got %d", len(truth))
	}
	wantKind := map[string]string{".mp3": "mp3", ".ogg": "ogg", ".webm": "webm", ".m4a": "mp4", ".mp4": "mp4", ".flac": "flac"}
	for name, tr := range truth {
		b, err := os.ReadFile(filepath.Join("testdata", "audiofmt", name))
		if err != nil {
			t.Fatal(err)
		}
		d, kind, err := audioDuration(b)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if kind != wantKind[filepath.Ext(name)] {
			t.Errorf("%s: detected %q", name, kind)
		}
		tol := 0.002
		if kind == "ogg" || kind == "webm" {
			tol = 0.01
		}
		if kind == "mp4" {
			tol = 0.1
		}
		if math.Abs(d-tr.Decoded) > tol {
			t.Errorf("%s: measured %.4f s, decode gives %.4f s", name, d, tr.Decoded)
		}
		// Never below the real content: a header cannot be used to under-bill.
		if d < tr.Intended-0.03 {
			t.Errorf("%s: measured %.4f s is shorter than the %.2f s of audio", name, d, tr.Intended)
		}
	}
}

func TestAudioDuration_RefusesGarbageAndTruncatedCompressed(t *testing.T) {
	for name, b := range map[string][]byte{
		"text":            []byte("hello, not audio"),
		"ogg header only": []byte("OggS\x00\x02"),
		"ftyp no moov":    append([]byte{0, 0, 0, 16}, []byte("ftypM4A \x00\x00\x00\x00")...),
		"ebml only":       {0x1A, 0x45, 0xDF, 0xA3, 0x80},
		"id3 no frames":   append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), make([]byte, 50)...),
	} {
		if d, _, err := audioDuration(b); err == nil {
			t.Errorf("%s: accepted with duration %.3f", name, d)
		}
	}
}
