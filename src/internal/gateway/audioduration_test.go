package gateway

import (
	"bytes"
	"encoding/binary"
	"math"
	"path/filepath"
	"testing"

	"openmodel/sp-state-agent/internal/settlement"
)

// wavBytes builds a PCM WAV of `seconds` at the given rate / channels / bit depth.
func wavBytes(seconds float64, rate, channels, bits int) []byte {
	frames := int(math.Round(seconds * float64(rate)))
	blockAlign := channels * bits / 8
	data := make([]byte, frames*blockAlign)
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + len(data)))
	b.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1))
	w(uint16(channels))
	w(uint32(rate))
	w(uint32(rate * blockAlign))
	w(uint16(blockAlign))
	w(uint16(bits))
	b.WriteString("data")
	w(uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

// flacBytes builds a minimal FLAC header with a STREAMINFO block declaring the length.
func flacBytes(sampleRate uint32, totalSamples uint64) []byte {
	si := make([]byte, 34)
	si[10] = byte(sampleRate >> 12)
	si[11] = byte(sampleRate >> 4)
	si[12] = byte(sampleRate<<4) | 0x02 // channels/bps bits, irrelevant here
	si[13] = byte(totalSamples>>32) & 0x0F
	binary.BigEndian.PutUint32(si[14:18], uint32(totalSamples))
	out := []byte("fLaC")
	out = append(out, 0x80, 0, 0, 34) // last-metadata-block flag + type 0 + length
	return append(out, si...)
}

func TestAudioDuration_WAVAcrossFormats(t *testing.T) {
	for _, c := range []struct {
		rate, ch, bits int
	}{{16000, 1, 16}, {44100, 2, 16}, {48000, 1, 24}, {24000, 1, 32}, {8000, 1, 8}} {
		d, kind, err := audioDuration(wavBytes(3.4, c.rate, c.ch, c.bits))
		if err != nil || kind != "wav" {
			t.Fatalf("%+v: %v %s", c, err, kind)
		}
		if math.Abs(d-3.4) > 1e-3 {
			t.Errorf("%+v: duration %.4f, want 3.4", c, d)
		}
		if billableSeconds(d) != 4 {
			t.Errorf("%+v: 3.4s must bill 4 seconds, got %d", c, billableSeconds(d))
		}
	}
}

func TestAudioDuration_SkipsUnknownChunks(t *testing.T) {
	wav := wavBytes(2.0, 16000, 1, 16)
	// Insert a LIST chunk (odd size, so the word-alignment pad matters) before "data".
	list := append([]byte("LIST"), 3, 0, 0, 0, 'a', 'b', 'c', 0)
	at := 12 + 8 + 16
	patched := append(append(append([]byte{}, wav[:at]...), list...), wav[at:]...)
	d, _, err := audioDuration(patched)
	if err != nil || math.Abs(d-2.0) > 1e-3 {
		t.Fatalf("duration %.4f err %v, want 2.0", d, err)
	}
}

func TestAudioDuration_FLAC(t *testing.T) {
	d, kind, err := audioDuration(flacBytes(44100, 44100*7+100))
	if err != nil || kind != "flac" {
		t.Fatal(err)
	}
	if billableSeconds(d) != 8 {
		t.Fatalf("7.0023s must bill 8 seconds, got %d (%.4f)", billableSeconds(d), d)
	}
	if _, _, err := audioDuration(flacBytes(44100, 0)); err == nil {
		t.Fatal("a FLAC that does not declare its length must be refused, not guessed")
	}
}

func TestAudioDuration_RefusesWhatItCannotMeasure(t *testing.T) {
	cases := map[string][]byte{
		"mp3":      append([]byte("ID3\x04\x00"), make([]byte, 100)...),
		"ogg":      append([]byte("OggS"), make([]byte, 100)...),
		"empty":    {},
		"text":     []byte("hello"),
		"adpcmWAV": func() []byte { b := wavBytes(1, 16000, 1, 16); b[20] = 2; return b }(),
	}
	for name, b := range cases {
		if _, _, err := audioDuration(b); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

// TestBillableSecondsMatchesWorker: the gateway and the worker both round a measured
// duration; the shared fixture pins that they round identically at every boundary.
func TestBillableSecondsMatchesWorker(t *testing.T) {
	v, err := settlement.LoadCanonicalVectors(filepath.Join("..", "settlement", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	if len(v.BillableSeconds) == 0 {
		t.Fatal("fixture has no billable_seconds cases")
	}
	for _, c := range v.BillableSeconds {
		if got := billableSeconds(c.Duration); got != c.Seconds {
			t.Errorf("billableSeconds(%v) = %d, worker rule gives %d", c.Duration, got, c.Seconds)
		}
	}
}
