package gateway

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
)

// audioduration_compressed.go — duration of compressed audio, read from frame headers or
// container metadata (see audioduration.go for the rules). Each parser has a byte-for-byte
// counterpart in the worker (py-inference speech_engine.audio_duration); the two must
// return the same number for the same file, since both bill from it.

// ---- MP3 ----

var (
	mp3BitrateV1 = [3][16]int{
		{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0}, // layer I
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0},    // layer II
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},     // layer III
	}
	mp3BitrateV2 = [3][16]int{
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0}, // layer I
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},      // layer II
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},      // layer III
	}
	mp3SampleRates = [4][3]int{ // by version bits: 0 = MPEG 2.5, 2 = MPEG 2, 3 = MPEG 1
		{11025, 12000, 8000}, {}, {22050, 24000, 16000}, {44100, 48000, 32000},
	}
)

// mp3Frame decodes one MPEG audio frame header: frame length in bytes, samples it
// carries and its sample rate. ok is false for anything that is not a valid header.
func mp3Frame(h []byte) (length, samples, rate int, ok bool) {
	if len(h) < 4 || h[0] != 0xFF || h[1]&0xE0 != 0xE0 {
		return 0, 0, 0, false
	}
	version := int(h[1]>>3) & 3
	layer := int(h[1]>>1) & 3 // 3 = I, 2 = II, 1 = III
	brIdx := int(h[2] >> 4)
	srIdx := int(h[2]>>2) & 3
	pad := int(h[2]>>1) & 1
	if version == 1 || layer == 0 || brIdx == 0 || brIdx == 15 || srIdx == 3 {
		return 0, 0, 0, false
	}
	rate = mp3SampleRates[version][srIdx]
	li := 3 - layer // 0 = I, 1 = II, 2 = III
	var kbps int
	if version == 3 {
		kbps = mp3BitrateV1[li][brIdx]
	} else {
		kbps = mp3BitrateV2[li][brIdx]
	}
	br := kbps * 1000
	switch li {
	case 0:
		return (12*br/rate + pad) * 4, 384, rate, true
	case 1:
		return 144*br/rate + pad, 1152, rate, true
	default:
		if version == 3 {
			return 144*br/rate + pad, 1152, rate, true
		}
		return 72*br/rate + pad, 576, rate, true
	}
}

func isMP3Header(h []byte) bool {
	_, _, _, ok := mp3Frame(h)
	return ok
}

func mp3Duration(b []byte) (float64, error) {
	off := 0
	if len(b) >= 10 && bytes.Equal(b[0:3], []byte("ID3")) {
		size := int(b[6]&0x7F)<<21 | int(b[7]&0x7F)<<14 | int(b[8]&0x7F)<<7 | int(b[9]&0x7F)
		off = 10 + size
		if b[5]&0x10 != 0 {
			off += 10
		}
	}
	dur := 0.0
	frames := 0
	trim, sampleRate := 0, 0
	for off+4 <= len(b) {
		length, samples, rate, ok := mp3Frame(b[off : off+4])
		if !ok || length < 4 {
			// Trailing tags end the audio; anything else is skipped up to the next
			// header that a second valid header confirms.
			if bytes.HasPrefix(b[off:], []byte("TAG")) || bytes.HasPrefix(b[off:], []byte("APETAGEX")) ||
				bytes.HasPrefix(b[off:], []byte("LYRICS")) {
				break
			}
			next := mp3Resync(b, off+1)
			if next < 0 {
				break
			}
			off = next
			continue
		}
		if frames == 0 && mp3IsInfoFrame(b[off:min(off+length, len(b))]) {
			trim = mp3GaplessTrim(b[off:min(off+length, len(b))])
			sampleRate = rate
			off += length
			frames = -1 // the header frame is not audio; count from the next one
			continue
		}
		if frames < 0 {
			frames = 0
		}
		dur += float64(samples) / float64(rate)
		frames++
		off += length
	}
	if frames <= 0 {
		return 0, errors.New("MP3 has no audio frames")
	}
	// Gapless trim from the LAME tag: the encoder's leading delay and trailing padding
	// are silence it added, not audio the client sent; decoders drop them too.
	if trim > 0 && sampleRate > 0 {
		if t := float64(trim) / float64(sampleRate); t < dur {
			dur -= t
		}
	}
	return dur, nil
}

// mp3GaplessTrim reads encoder delay + padding (in samples) from the LAME extension of a
// Xing/Info frame; 0 when the frame carries none.
func mp3GaplessTrim(f []byte) int {
	i := bytes.Index(f, []byte("Xing"))
	if i < 0 {
		i = bytes.Index(f, []byte("Info"))
	}
	if i < 0 || i+8 > len(f) {
		return 0
	}
	flags := binary.BigEndian.Uint32(f[i+4 : i+8])
	p := i + 8
	if flags&1 != 0 {
		p += 4
	}
	if flags&2 != 0 {
		p += 4
	}
	if flags&4 != 0 {
		p += 100
	}
	if flags&8 != 0 {
		p += 4
	}
	// LAME tag: 9-byte encoder string, then delay/padding at offset 21 (12 + 12 bits).
	if p+24 > len(f) {
		return 0
	}
	enc := string(f[p : p+4])
	if enc != "LAME" && enc != "Lavf" && enc != "Lavc" {
		return 0
	}
	d := f[p+21 : p+24]
	delay := int(d[0])<<4 | int(d[1])>>4
	padding := int(d[1]&0x0F)<<8 | int(d[2])
	return delay + padding
}

// mp3Resync finds the next offset holding a frame header that is followed by another one.
func mp3Resync(b []byte, from int) int {
	for i := from; i+4 <= len(b); i++ {
		length, _, _, ok := mp3Frame(b[i : i+4])
		if !ok || length < 4 {
			continue
		}
		if j := i + length; j+4 <= len(b) {
			if _, _, _, ok2 := mp3Frame(b[j : j+4]); !ok2 {
				continue
			}
		}
		return i
	}
	return -1
}

func mp3IsInfoFrame(f []byte) bool {
	n := min(len(f), 64)
	head := f[:n]
	return bytes.Contains(head, []byte("Xing")) || bytes.Contains(head, []byte("Info")) ||
		bytes.Contains(head, []byte("VBRI"))
}

// ---- Ogg ----

func oggDuration(b []byte) (float64, error) {
	var serial uint32
	codec := ""
	rate := 0.0
	preSkip := int64(0)
	last := int64(-1)
	off := 0
	for off+27 <= len(b) {
		if !bytes.Equal(b[off:off+4], []byte("OggS")) {
			return 0, errors.New("malformed Ogg page")
		}
		granule := int64(binary.LittleEndian.Uint64(b[off+6 : off+14]))
		ser := binary.LittleEndian.Uint32(b[off+14 : off+18])
		nseg := int(b[off+26])
		if off+27+nseg > len(b) {
			return 0, errors.New("truncated Ogg page")
		}
		body := 0
		for _, l := range b[off+27 : off+27+nseg] {
			body += int(l)
		}
		start := off + 27 + nseg
		end := start + body
		if end > len(b) {
			end = len(b)
		}
		payload := b[start:end]
		if codec == "" {
			switch {
			case len(payload) >= 19 && bytes.Equal(payload[0:8], []byte("OpusHead")):
				codec, serial, rate = "opus", ser, 48000
				preSkip = int64(binary.LittleEndian.Uint16(payload[10:12]))
			case len(payload) >= 16 && bytes.Equal(payload[0:7], []byte("\x01vorbis")):
				codec, serial = "vorbis", ser
				rate = float64(binary.LittleEndian.Uint32(payload[12:16]))
			}
		} else if ser == serial && granule >= 0 {
			last = granule
		}
		off = start + body
	}
	if codec == "" {
		return 0, errors.New("Ogg file has no Opus or Vorbis stream")
	}
	if last < 0 || rate <= 0 {
		return 0, errors.New("Ogg stream declares no length")
	}
	samples := last - preSkip
	if samples <= 0 {
		return 0, errors.New("Ogg stream is empty")
	}
	return float64(samples) / rate, nil
}

// ---- WebM / Matroska ----

const ebmlUnknownSize = -1

// ebmlID reads an element ID (marker bits kept), returning it and its length.
func ebmlID(b []byte) (uint32, int, bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	n := 1
	for m := byte(0x80); n <= 4 && b[0]&m == 0; m >>= 1 {
		n++
	}
	if n > 4 || n > len(b) {
		return 0, 0, false
	}
	var id uint32
	for i := 0; i < n; i++ {
		id = id<<8 | uint32(b[i])
	}
	return id, n, true
}

// ebmlSize reads a data size (marker removed); all value bits set means unknown.
func ebmlSize(b []byte) (int64, int, bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	n := 1
	for m := byte(0x80); n <= 8 && b[0]&m == 0; m >>= 1 {
		n++
	}
	if n > len(b) {
		return 0, 0, false
	}
	v := int64(b[0] & (0xFF >> n))
	allOnes := v == int64(0xFF>>n)
	for i := 1; i < n; i++ {
		v = v<<8 | int64(b[i])
		if b[i] != 0xFF {
			allOnes = false
		}
	}
	if allOnes {
		return ebmlUnknownSize, n, true
	}
	return v, n, true
}

func ebmlUint(p []byte) uint64 {
	var v uint64
	for _, c := range p {
		v = v<<8 | uint64(c)
	}
	return v
}

// opusPacketDuration is the length of one Opus packet in 48 kHz samples, from its TOC.
func opusPacketDuration(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	cfg := int(p[0] >> 3)
	var frame int // in 48 kHz samples
	switch {
	case cfg < 12:
		frame = []int{480, 960, 1920, 2880}[cfg%4]
	case cfg < 16:
		frame = []int{480, 960}[cfg%2]
	default:
		frame = []int{120, 240, 480, 960}[cfg%4]
	}
	switch p[0] & 3 {
	case 0:
		return frame
	case 1, 2:
		return 2 * frame
	default:
		if len(p) < 2 {
			return 0
		}
		return int(p[1]&0x3F) * frame
	}
}

func matroskaDuration(b []byte) (float64, error) {
	const (
		idSegment     = 0x18538067
		idInfo        = 0x1549A966
		idTimecodeSc  = 0x2AD7B1
		idDuration    = 0x4489
		idTracks      = 0x1654AE6B
		idTrackEntry  = 0xAE
		idTrackNumber = 0xD7
		idTrackType   = 0x83
		idCodecID     = 0x86
		idCluster     = 0x1F43B675
		idTimecode    = 0xE7
		idSimpleBlock = 0xA3
		idBlockGroup  = 0xA0
		idBlock       = 0xA1
		idEBML        = 0x1A45DFA3
		idCodecDelay  = 0x56AA
		idDiscardPad  = 0x75A2
	)
	// Master elements whose children are read in place (a flat scan also copes with
	// the unknown-size Segment and Clusters a live recorder writes).
	enter := map[uint32]bool{idSegment: true, idInfo: true, idTracks: true, idTrackEntry: true,
		idCluster: true, idBlockGroup: true}

	scale := uint64(1000000)
	infoDur := 0.0
	type track struct {
		num   uint64
		typ   uint64
		codec string
	}
	var tracks []track
	audioTrack := uint64(0)
	clusterTC := uint64(0)
	lastTick := int64(-1)
	lastFrame := 0
	codecDelay := uint64(0) // ns, of the audio track
	lastDiscard := int64(0) // ns, DiscardPadding of the last audio block
	inGroupDiscard := int64(0)

	off := 0
	for off < len(b) {
		id, il, ok := ebmlID(b[off:])
		if !ok {
			break
		}
		size, sl, ok := ebmlSize(b[off+il:])
		if !ok {
			break
		}
		start := off + il + sl
		if enter[id] {
			if id == idTrackEntry {
				tracks = append(tracks, track{})
			}
			off = start
			continue
		}
		if size == ebmlUnknownSize || start+int(size) > len(b) || size < 0 {
			if id == idEBML {
				return 0, errors.New("malformed WebM header")
			}
			break // truncated tail: keep what was read
		}
		p := b[start : start+int(size)]
		switch id {
		case idTimecodeSc:
			scale = ebmlUint(p)
		case idDuration:
			if len(p) == 4 {
				infoDur = float64(math.Float32frombits(binary.BigEndian.Uint32(p)))
			} else if len(p) == 8 {
				infoDur = math.Float64frombits(binary.BigEndian.Uint64(p))
			}
		case idTrackNumber:
			if n := len(tracks); n > 0 {
				tracks[n-1].num = ebmlUint(p)
			}
		case idTrackType:
			if n := len(tracks); n > 0 {
				tracks[n-1].typ = ebmlUint(p)
			}
		case idCodecID:
			if n := len(tracks); n > 0 {
				tracks[n-1].codec = string(p)
			}
		case idCodecDelay:
			if n := len(tracks); n > 0 && tracks[n-1].typ != 1 {
				codecDelay = ebmlUint(p)
			}
		case idDiscardPad:
			v := int64(ebmlUint(p))
			if len(p) > 0 && len(p) < 8 && p[0]&0x80 != 0 { // signed
				v -= int64(1) << (8 * len(p))
			}
			inGroupDiscard = v
			lastDiscard = v
		case idTimecode:
			clusterTC = ebmlUint(p)
		case idSimpleBlock, idBlock:
			if audioTrack == 0 {
				for _, t := range tracks {
					if t.typ == 2 {
						audioTrack = t.num
						break
					}
				}
			}
			tn, tl, ok := ebmlSize(p)
			if !ok || tn < 0 || len(p) < tl+3 || uint64(tn) != audioTrack || audioTrack == 0 {
				break
			}
			rel := int64(int16(binary.BigEndian.Uint16(p[tl : tl+2])))
			tick := int64(clusterTC) + rel
			if tick >= lastTick {
				lastTick = tick
				lastFrame = 0
				lastDiscard = inGroupDiscard // DiscardPadding precedes nothing in SimpleBlock
				inGroupDiscard = 0
				if p[tl+2]&0x06 == 0 { // unlaced: the payload is one codec packet
					lastFrame = opusPacketDuration(p[tl+3:])
				}
			}
		}
		off = start + int(size)
	}
	isOpus := false
	for _, t := range tracks {
		if t.num == audioTrack && t.codec == "A_OPUS" {
			isOpus = true
		}
	}
	if lastTick >= 0 {
		d := float64(lastTick) * float64(scale) / 1e9
		if isOpus {
			// Last packet length, less the padding the encoder flagged for discard and
			// the codec's built-in delay (Opus pre-skip) — the same quantities an Ogg
			// granule position accounts for.
			d += float64(lastFrame)/48000 - float64(lastDiscard)/1e9 - float64(codecDelay)/1e9
		}
		if d <= 0 {
			return 0, errors.New("WebM audio is empty")
		}
		return d, nil
	}
	if infoDur > 0 {
		return infoDur * float64(scale) / 1e9, nil
	}
	return 0, errors.New("WebM file has no audio blocks")
}

// ---- MP4 / M4A ----

func mp4Duration(b []byte) (float64, error) {
	type box struct {
		typ        string
		start, end int
	}
	children := func(from, to int) []box {
		var out []box
		for off := from; off+8 <= to; {
			size := int64(binary.BigEndian.Uint32(b[off : off+4]))
			typ := string(b[off+4 : off+8])
			hdr := 8
			if size == 1 {
				if off+16 > to {
					break
				}
				size = int64(binary.BigEndian.Uint64(b[off+8 : off+16]))
				hdr = 16
			} else if size == 0 {
				size = int64(to - off)
			}
			if size < int64(hdr) || int64(off)+size > int64(to) {
				break
			}
			out = append(out, box{typ, off + hdr, off + int(size)})
			off += int(size)
		}
		return out
	}
	find := func(bs []box, typ string) *box {
		for i := range bs {
			if bs[i].typ == typ {
				return &bs[i]
			}
		}
		return nil
	}
	moov := find(children(0, len(b)), "moov")
	if moov == nil {
		return 0, errors.New("MP4 has no moov box (upload a complete file)")
	}
	for _, trak := range children(moov.start, moov.end) {
		if trak.typ != "trak" {
			continue
		}
		mdia := find(children(trak.start, trak.end), "mdia")
		if mdia == nil {
			continue
		}
		mb := children(mdia.start, mdia.end)
		hdlr, mdhd := find(mb, "hdlr"), find(mb, "mdhd")
		if hdlr == nil || mdhd == nil || hdlr.end-hdlr.start < 12 || string(b[hdlr.start+8:hdlr.start+12]) != "soun" {
			continue
		}
		p := b[mdhd.start:mdhd.end]
		var scale, dur uint64
		switch {
		case len(p) >= 20 && p[0] == 0:
			scale, dur = uint64(binary.BigEndian.Uint32(p[12:16])), uint64(binary.BigEndian.Uint32(p[16:20]))
			if dur == 0xFFFFFFFF {
				dur = 0
			}
		case len(p) >= 32 && p[0] == 1:
			scale, dur = uint64(binary.BigEndian.Uint32(p[20:24])), binary.BigEndian.Uint64(p[24:32])
			if dur == math.MaxUint64 {
				dur = 0
			}
		default:
			return 0, errors.New("malformed MP4 mdhd box")
		}
		if scale == 0 || dur == 0 {
			return 0, errors.New("MP4 sound track declares no length (fragmented MP4 is not supported)")
		}
		return float64(dur) / float64(scale), nil
	}
	return 0, errors.New("MP4 has no sound track")
}
