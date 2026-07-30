// A from-scratch MPEG-TS/PES parser, just deep enough to answer the three
// questions the SRT listener exists to answer about what a caller is
// actually sending: what bitrate, do the PIDs look like H.264 + AAC, and —
// the one that matters most — does DTS ever go backwards.
//
// The backwards-DTS bug is the measured failure that motivates this whole
// file: audio DTS jumping backwards by exactly the previous run's uptime,
// 1,523 non-monotonic errors downstream, commentary never returning while
// every indicator read healthy (spec section 6.1). It is a pipeline-restart
// bug, not an SRT bug, so nothing here can reproduce it — but everything
// here can DETECT it, automatically, the moment WP-3b or WP-8 reintroduces
// it, rather than an hour into a soak test.
//
// This is not a general-purpose demuxer. It tracks only PAT -> PMT -> the
// video and audio elementary streams' PES headers, and only far enough into
// each PES header to read PTS/DTS. It does not reassemble PES payloads, does
// not track continuity counters, and does not verify CRCs — none of that is
// needed to answer the three questions above, and a mock earns its keep by
// being exactly as complicated as its job requires.
package main

import (
	"fmt"
	"sync"
	"time"
)

// MPEG-TS constants (ISO/IEC 13818-1).
const (
	tsPacketSize = 188
	tsSyncByte   = 0x47
	tsPATPID     = 0x0000
)

// Stream types this mock cares about, from ISO/IEC 13818-1 Table 2-34 and the
// registration-descriptor values commonly used for AC-3 in the wild. Only
// these six are inspected; anything else is invisible to the analyzer, which
// is correct for this mock's job — it exists to tell H.264+AAC apart from
// everything else, not to catalogue every stream type there is.
const (
	streamTypeH264       = 0x1B // ISO/IEC 14496-10 (H.264/AVC) video
	streamTypeAACADTS    = 0x0F // ISO/IEC 13818-7 AAC, ADTS framing
	streamTypeAACLATM    = 0x11 // ISO/IEC 14496-3 AAC, LATM/LOAS framing
	streamTypeMPEG1Audio = 0x03 // MPEG-1 audio (MP2) — M2L-X silently drops this
	streamTypeMPEG2Audio = 0x04 // MPEG-2 audio (MP2) — M2L-X silently drops this
	streamTypeAC3        = 0x81 // ATSC A/52 (Dolby AC-3) — M2L-X silently drops this
)

// StreamKind coarsely classifies an elementary stream for logging.
type StreamKind int

const (
	kindVideo StreamKind = iota
	kindAudio
)

func (k StreamKind) String() string {
	if k == kindVideo {
		return "video"
	}
	return "audio"
}

// AnalyzerSnapshot is a point-in-time read of everything the analyzer has
// learned about one SRT session's payload.
type AnalyzerSnapshot struct {
	// BytesTotal is every payload byte handed to Write, including TS
	// stuffing and packets on PIDs the analyzer does not otherwise care
	// about — it is meant to match "what the wire carried", not just the
	// elementary streams.
	BytesTotal uint64

	// BitrateBps is BytesTotal*8 divided by the wall-clock span between the
	// first and last byte written, or 0 before enough data has arrived to
	// measure a span. Deliberately NOT modelled on M2L-X's own
	// statistics.bitrate, which freezes at its last value and so lies about
	// a dead input forever (spec section 8) — this is recomputed from
	// scratch on every read and reads 0 the instant nothing is arriving.
	BitrateBps float64

	// HaveVideoPID and HaveAudioPID report whether a PMT has been seen
	// naming a video / audio elementary stream at all.
	HaveVideoPID bool
	HaveAudioPID bool

	// VideoIsH264 is true once the PMT has identified the video PID with
	// stream_type 0x1B.
	VideoIsH264 bool

	// AudioIsAAC is true when the PMT has identified at least one audio PID
	// and every audio PID it found is AAC (ADTS or LATM). False, with
	// HaveAudioPID true, is the signature of the MP2/AC-3 silent-drop bug
	// (spec section 5, "Audio codec is pinned"): the PID is there, but it is
	// not AAC.
	AudioIsAAC bool

	// DTSBackwards is true once any PES packet on the video or audio PID has
	// carried a DTS lower than the previous one seen on the SAME pid. It
	// latches for the life of the session: a single regression is exactly as
	// bad as a hundred, because downstream decoders don't recover either
	// (spec section 6.1: "1,523 non-monotonic errors downstream, commentary
	// never returning").
	DTSBackwards bool

	// DTSBackwardsDetail is a human-readable description of the first
	// regression seen, empty when DTSBackwards is false.
	DTSBackwardsDetail string
}

// tsAnalyzer incrementally parses an MPEG-TS byte stream fed to it via
// Write, in whatever chunk sizes the caller happens to read from the
// network. It is safe for concurrent use: Write is called from the SRT
// connection's reader goroutine, Snapshot from the status broadcaster and
// the /control HTTP handlers, on different goroutines, at the same time.
type tsAnalyzer struct {
	mu  sync.Mutex
	now func() time.Time // overridden in tests for deterministic bitrate math

	buf []byte // bytes received but not yet consumed as a whole 188-byte packet

	pmtPID          int // -1 until the PAT has named one
	videoPID        int // -1 until the PMT has named one
	videoStreamType int
	audioPIDs       map[int]int // elementary PID -> stream_type, from the PMT

	lastDTS            map[int]uint64 // elementary PID -> last DTS seen, 90 kHz units
	dtsBackwards       bool
	dtsBackwardsDetail string
	loggedDTSBackwards bool // consumed once by FirstDTSBackwards, see below

	totalBytes uint64
	firstByte  time.Time
	lastByte   time.Time
}

// newTSAnalyzer returns an analyzer ready to Write to.
func newTSAnalyzer() *tsAnalyzer {
	return &tsAnalyzer{
		now:       time.Now,
		pmtPID:    -1,
		videoPID:  -1,
		audioPIDs: make(map[int]int),
		lastDTS:   make(map[int]uint64),
	}
}

// Write implements io.Writer: it feeds p into the analyzer as the next slice
// of the byte stream. It never returns a non-nil error — a malformed or
// resyncing stream is exactly what this analyzer exists to tolerate and keep
// reporting on, not something to fail out of.
func (an *tsAnalyzer) Write(p []byte) (int, error) {
	an.mu.Lock()
	defer an.mu.Unlock()

	if len(p) == 0 {
		return 0, nil
	}

	now := an.now()
	if an.firstByte.IsZero() {
		an.firstByte = now
	}
	an.lastByte = now
	an.totalBytes += uint64(len(p))

	an.buf = append(an.buf, p...)

	for {
		// Resynchronise: drop bytes until the next 0x47. GStreamer's
		// alignment=7 muxing (spec section 5) means a well-behaved sender
		// never requires this, but a fault-injection tool that lets a test
		// throw arbitrary bytes at the listener needs to survive it rather
		// than wedge.
		for len(an.buf) > 0 && an.buf[0] != tsSyncByte {
			an.buf = an.buf[1:]
		}
		if len(an.buf) < tsPacketSize {
			break
		}
		pkt := an.buf[:tsPacketSize]
		an.buf = an.buf[tsPacketSize:]
		an.processPacket(pkt)
	}

	return len(p), nil
}

// processPacket parses one 188-byte TS packet, pkt[0] already known to be
// the 0x47 sync byte.
func (an *tsAnalyzer) processPacket(pkt []byte) {
	if pkt[1]&0x80 != 0 { // transport_error_indicator
		return
	}

	pusi := pkt[1]&0x40 != 0
	pid := int(pkt[1]&0x1F)<<8 | int(pkt[2])
	afc := (pkt[3] >> 4) & 0x03 // adaptation_field_control

	i := 4
	if afc == 2 || afc == 3 { // adaptation field present
		if i >= len(pkt) {
			return
		}
		adaptLen := int(pkt[i])
		i += 1 + adaptLen
		if i > len(pkt) {
			return
		}
	}
	if afc == 0 || afc == 2 { // no payload (reserved, or adaptation-field-only)
		return
	}
	if i >= len(pkt) {
		return
	}
	payload := pkt[i:]

	switch {
	case pid == tsPATPID:
		an.parsePAT(payload, pusi)
	case an.pmtPID >= 0 && pid == an.pmtPID:
		an.parsePMT(payload, pusi)
	case pid == an.videoPID:
		an.parsePES(pid, payload, pusi, kindVideo)
	default:
		if _, ok := an.audioPIDs[pid]; ok {
			an.parsePES(pid, payload, pusi, kindAudio)
		}
	}
}

// parsePAT extracts the first non-zero program's PMT PID.
func (an *tsAnalyzer) parsePAT(payload []byte, pusi bool) {
	if !pusi || len(payload) < 1 {
		return
	}
	pointer := int(payload[0])
	i := 1 + pointer
	if i+8 > len(payload) {
		return
	}

	sectionLength := int(payload[i+1]&0x0F)<<8 | int(payload[i+2])
	end := i + 3 + sectionLength
	if end > len(payload) {
		end = len(payload)
	}

	j := i + 3 + 5 // past table_id/section_length, transport_stream_id, version/current, section_number, last_section_number
	for j+4 <= end-4 {
		programNumber := int(payload[j])<<8 | int(payload[j+1])
		mapPID := int(payload[j+2]&0x1F)<<8 | int(payload[j+3])
		if programNumber != 0 {
			an.pmtPID = mapPID
		}
		j += 4
	}
}

// parsePMT extracts the video and audio elementary stream PIDs and their
// stream types.
func (an *tsAnalyzer) parsePMT(payload []byte, pusi bool) {
	if !pusi || len(payload) < 1 {
		return
	}
	pointer := int(payload[0])
	i := 1 + pointer
	if i+12 > len(payload) {
		return
	}

	sectionLength := int(payload[i+1]&0x0F)<<8 | int(payload[i+2])
	end := i + 3 + sectionLength
	if end > len(payload) {
		end = len(payload)
	}

	programInfoLenOff := i + 3 + 7 // past program_number, version/current, section_number, last_section_number, PCR_PID
	if programInfoLenOff+2 > len(payload) {
		return
	}
	programInfoLength := int(payload[programInfoLenOff]&0x0F)<<8 | int(payload[programInfoLenOff+1])
	j := programInfoLenOff + 2 + programInfoLength

	newVideoPID := -1
	newVideoType := 0
	newAudio := make(map[int]int)

	for j+5 <= end-4 {
		streamType := int(payload[j])
		esPID := int(payload[j+1]&0x1F)<<8 | int(payload[j+2])
		esInfoLength := int(payload[j+3]&0x0F)<<8 | int(payload[j+4])
		j += 5 + esInfoLength
		if j > end {
			break
		}

		switch streamType {
		case streamTypeH264:
			newVideoPID = esPID
			newVideoType = streamType
		case streamTypeAACADTS, streamTypeAACLATM, streamTypeMPEG1Audio, streamTypeMPEG2Audio, streamTypeAC3:
			newAudio[esPID] = streamType
		}
	}

	if newVideoPID >= 0 {
		an.videoPID = newVideoPID
		an.videoStreamType = newVideoType
	}
	if len(newAudio) > 0 {
		an.audioPIDs = newAudio
	}
}

// parsePES reads a PES packet's optional header, if any, and records its DTS
// (falling back to PTS when only PTS is present — a stream with PTS-only
// framing has no DTS to go backwards, but tracking PTS on that path costs
// nothing and catches the same class of bug if one ever appears there).
func (an *tsAnalyzer) parsePES(pid int, payload []byte, pusi bool, kind StreamKind) {
	if !pusi || len(payload) < 9 {
		return
	}
	if payload[0] != 0x00 || payload[1] != 0x00 || payload[2] != 0x01 {
		return // not a PES start code
	}

	flags1 := payload[6]
	if flags1&0xC0 != 0x80 {
		// Not the standard PES optional header marker ('10'); some stream
		// types have no optional header at all. Nothing to read.
		return
	}
	ptsDTSFlags := (payload[7] >> 6) & 0x03
	headerDataLength := int(payload[8])
	hdr := payload[9:]
	if len(hdr) < headerDataLength {
		return
	}

	var ts uint64
	var have bool

	switch ptsDTSFlags {
	case 0x2: // PTS only
		if len(hdr) < 5 {
			return
		}
		ts, have = decodeTimestamp(hdr[0:5]), true
	case 0x3: // PTS then DTS — DTS is what we want
		if len(hdr) < 10 {
			return
		}
		ts, have = decodeTimestamp(hdr[5:10]), true
	default:
		return // no timestamp present
	}

	if !have {
		return
	}

	prev, seen := an.lastDTS[pid]
	if seen && ts < prev {
		if !an.dtsBackwards {
			an.dtsBackwards = true
			an.dtsBackwardsDetail = fmt.Sprintf(
				"pid 0x%04x (%s): DTS went backwards: %d -> %d (Δ -%d @ 90kHz, -%.3fs)",
				pid, kind, prev, ts, prev-ts, float64(prev-ts)/90000.0,
			)
		}
	}
	an.lastDTS[pid] = ts
}

// decodeTimestamp reconstructs a 33-bit 90 kHz PTS or DTS value from its
// 5-byte, marker-bit-interleaved wire encoding (ISO/IEC 13818-1 section
// 2.4.3.6). Marker bits and the 4-bit prefix are not validated: a
// permissive parser is the right trade-off for a mock whose job is to
// tolerate whatever a test throws at the listener, not to be a conformance
// checker.
func decodeTimestamp(b []byte) uint64 {
	_ = b[4] // bounds check hint, mirrors the standard library idiom
	return uint64(b[0]>>1&0x07)<<30 |
		uint64(b[1])<<22 |
		uint64(b[2]>>1&0x7F)<<15 |
		uint64(b[3])<<7 |
		uint64(b[4]>>1&0x7F)
}

// FirstDTSBackwards reports the detail string of a DTS regression exactly
// once — the first call made after one is detected — and ("", false) every
// other time, including every call before one occurs. It exists so a reader
// loop that calls it after every Read logs the event exactly once per
// session instead of once per packet.
func (an *tsAnalyzer) FirstDTSBackwards() (detail string, first bool) {
	an.mu.Lock()
	defer an.mu.Unlock()
	if an.dtsBackwards && !an.loggedDTSBackwards {
		an.loggedDTSBackwards = true
		return an.dtsBackwardsDetail, true
	}
	return "", false
}

// Snapshot returns the current state of everything the analyzer has learned.
func (an *tsAnalyzer) Snapshot() AnalyzerSnapshot {
	an.mu.Lock()
	defer an.mu.Unlock()

	var bitrate float64
	if !an.firstByte.IsZero() && an.lastByte.After(an.firstByte) {
		elapsed := an.lastByte.Sub(an.firstByte).Seconds()
		if elapsed > 0 {
			bitrate = float64(an.totalBytes) * 8 / elapsed
		}
	}

	audioIsAAC := len(an.audioPIDs) > 0
	for _, st := range an.audioPIDs {
		if st != streamTypeAACADTS && st != streamTypeAACLATM {
			audioIsAAC = false
			break
		}
	}

	return AnalyzerSnapshot{
		BytesTotal:         an.totalBytes,
		BitrateBps:         bitrate,
		HaveVideoPID:       an.videoPID >= 0,
		HaveAudioPID:       len(an.audioPIDs) > 0,
		VideoIsH264:        an.videoPID >= 0 && an.videoStreamType == streamTypeH264,
		AudioIsAAC:         audioIsAAC,
		DTSBackwards:       an.dtsBackwards,
		DTSBackwardsDetail: an.dtsBackwardsDetail,
	}
}
