package gateway

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// audioduration.go — the gateway measures the duration of uploaded audio itself.
//
// WHY THE GATEWAY MEASURES. Transcription is billed per input second. A worker attests a
// quantity in its signed receipt, but the signature only proves the worker SAID it; the
// gateway has the uploaded bytes in hand, so it derives the billable seconds first-hand
// and refuses a receipt that disagrees. Trusting the worker's number would let a worker
// bill any duration it likes.
//
// HOW EACH FORMAT IS MEASURED. Every rule reads lengths the file itself carries, without
// decoding, so it is exact, cheap and needs no native dependency:
//
//	WAV         PCM data-chunk size over the byte rate
//	FLAC        STREAMINFO total samples over the sample rate
//	MP3         sum over the MPEG audio frames of samples-per-frame / sample rate
//	            (an initial Xing/Info/VBRI header frame carries no audio and is skipped)
//	Ogg         last granule position of the first Opus or Vorbis stream (Opus: minus
//	            the pre-skip, at 48 kHz)
//	WebM / MKV  timestamp of the last audio block plus that block's Opus frame length;
//	            the Info Duration only when there are no audio blocks to read
//	MP4 / M4A   media-header duration of the first sound track
//
// The worker applies the same rules (speech_engine.audio_duration) and bills the same
// number. It also decodes the audio and refuses a file that plays noticeably longer than
// its container declares, so a forged header cannot buy a cheaper transcription.

// errUnsupportedAudio is returned for any format the gateway cannot measure exactly.
var errUnsupportedAudio = errors.New("unsupported audio format: upload WAV, FLAC, MP3, OGG (Opus/Vorbis), WebM or M4A/MP4")

// audioDuration returns the playback duration in seconds and the detected format.
func audioDuration(b []byte) (float64, string, error) {
	switch {
	case len(b) >= 12 && bytes.Equal(b[0:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WAVE")):
		d, err := wavDuration(b)
		return d, "wav", err
	case len(b) >= 4 && bytes.Equal(b[0:4], []byte("fLaC")):
		d, err := flacDuration(b)
		return d, "flac", err
	case len(b) >= 4 && bytes.Equal(b[0:4], []byte("OggS")):
		d, err := oggDuration(b)
		return d, "ogg", err
	case len(b) >= 4 && bytes.Equal(b[0:4], []byte{0x1A, 0x45, 0xDF, 0xA3}):
		d, err := matroskaDuration(b)
		return d, "webm", err
	case len(b) >= 8 && bytes.Equal(b[4:8], []byte("ftyp")):
		d, err := mp4Duration(b)
		return d, "mp4", err
	case len(b) >= 3 && bytes.Equal(b[0:3], []byte("ID3")), len(b) >= 4 && isMP3Header(b[0:4]):
		d, err := mp3Duration(b)
		return d, "mp3", err
	}
	return 0, "", errUnsupportedAudio
}

// wavDuration walks the RIFF chunks for "fmt " and "data". Only uncompressed PCM
// (format 1) and IEEE float (3), plus WAVE_FORMAT_EXTENSIBLE carrying either, are
// accepted: for those, data bytes / byte rate is the exact duration.
func wavDuration(b []byte) (float64, error) {
	var byteRate uint32
	var haveFmt bool
	off := 12
	for off+8 <= len(b) {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := off + 8
		if size < 0 || body+size < body {
			return 0, errors.New("malformed WAV chunk size")
		}
		switch id {
		case "fmt ":
			if size < 16 || body+16 > len(b) {
				return 0, errors.New("malformed WAV fmt chunk")
			}
			format := binary.LittleEndian.Uint16(b[body : body+2])
			if format == 0xFFFE && size >= 26 && body+26 <= len(b) {
				format = binary.LittleEndian.Uint16(b[body+24 : body+26]) // sub-format GUID head
			}
			if format != 1 && format != 3 {
				return 0, fmt.Errorf("WAV encoding %d is compressed; upload PCM WAV or FLAC", format)
			}
			byteRate = binary.LittleEndian.Uint32(b[body+8 : body+12])
			haveFmt = true
		case "data":
			if !haveFmt || byteRate == 0 {
				return 0, errors.New("WAV data chunk before a valid fmt chunk")
			}
			// A streaming writer may leave the size as 0 or 0xFFFFFFFF; use what is
			// actually present in that case, never a declared size beyond the file.
			n := size
			if n == 0 || body+n > len(b) {
				n = len(b) - body
			}
			return float64(n) / float64(byteRate), nil
		}
		off = body + size + size%2 // chunks are word-aligned
	}
	return 0, errors.New("WAV has no data chunk")
}

// flacDuration reads STREAMINFO (always the first metadata block): sample rate (20 bits)
// and total samples (36 bits).
func flacDuration(b []byte) (float64, error) {
	if len(b) < 4+4+34 {
		return 0, errors.New("FLAC too short")
	}
	hdr := b[4]
	if hdr&0x7F != 0 { // block type 0 = STREAMINFO
		return 0, errors.New("FLAC STREAMINFO block missing")
	}
	si := b[8 : 8+34]
	sampleRate := uint32(si[10])<<12 | uint32(si[11])<<4 | uint32(si[12])>>4
	total := uint64(si[13]&0x0F)<<32 | uint64(binary.BigEndian.Uint32(si[14:18]))
	if sampleRate == 0 {
		return 0, errors.New("FLAC sample rate is zero")
	}
	if total == 0 {
		// "Unknown" in the spec. Measuring would need decoding; refuse rather than guess.
		return 0, errors.New("FLAC does not declare its length; re-encode with a known sample count")
	}
	return float64(total) / float64(sampleRate), nil
}

// billableSeconds is the transcription billing quantity: duration rounded UP to whole
// seconds, minimum 1 — the same rule the worker applies (speech_engine.billable_seconds)
// and the same as OpenAI whisper-1 / Google STT V2.
func billableSeconds(d float64) int {
	if d <= 0 {
		return 1
	}
	s := int(math.Ceil(d - 1e-9))
	if s < 1 {
		s = 1
	}
	return s
}
