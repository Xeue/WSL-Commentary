package main

import (
	"testing"
	"time"
)

// zeroClock is a fixed receive clock: counts are what the tests assert, not
// timing, so a constant keeps the fixtures simple.
func zeroClock() time.Duration { return 0 }

// tsPacket builds one 188-byte payload-only TS packet (afc=01), padded with
// 0xFF after the payload the way a real muxer stuffs a short packet.
func tsPacket(pid, cc int, pusi bool, payload []byte) []byte {
	pkt := make([]byte, tsPacketSize)
	pkt[0] = tsSyncByte
	pkt[1] = byte(pid>>8) & 0x1F
	if pusi {
		pkt[1] |= 0x40
	}
	pkt[2] = byte(pid & 0xFF)
	pkt[3] = 0x10 | byte(cc&0x0F) // adaptation_field_control=01, payload only
	for i := 4; i < tsPacketSize; i++ {
		pkt[i] = 0xFF
	}
	copy(pkt[4:], payload)
	return pkt
}

// tsPacketDiscont builds a packet whose adaptation field sets
// discontinuity_indicator, i.e. a signalled (legal) continuity break.
func tsPacketDiscont(pid, cc int, pusi bool, payload []byte) []byte {
	pkt := make([]byte, tsPacketSize)
	pkt[0] = tsSyncByte
	pkt[1] = byte(pid>>8) & 0x1F
	if pusi {
		pkt[1] |= 0x40
	}
	pkt[2] = byte(pid & 0xFF)
	pkt[3] = 0x30 | byte(cc&0x0F) // adaptation_field_control=11, adaptation + payload
	pkt[4] = 1                    // adaptation_field_length
	pkt[5] = 0x80                 // discontinuity_indicator
	for i := 6; i < tsPacketSize; i++ {
		pkt[i] = 0xFF
	}
	copy(pkt[6:], payload)
	return pkt
}

func patPayload(pmtPID int) []byte {
	return []byte{
		0x00,       // pointer_field
		0x00,       // table_id = PAT
		0xB0, 0x0D, // section_syntax=1, section_length=13
		0x00, 0x01, // transport_stream_id
		0xC1,       // version/current_next
		0x00, 0x00, // section_number, last_section_number
		0x00, 0x01, // program_number = 1
		byte(0xE0 | (pmtPID>>8)&0x1F), byte(pmtPID & 0xFF), // reserved + PMT PID
		0x00, 0x00, 0x00, 0x00, // CRC (not validated)
	}
}

func pmtPayload(videoPID, streamType int) []byte {
	return []byte{
		0x00,       // pointer_field
		0x02,       // table_id = PMT
		0xB0, 0x12, // section_syntax=1, section_length=18
		0x00, 0x01, // program_number
		0xC1,       // version/current
		0x00, 0x00, // section_number, last_section_number
		byte(0xE0 | (videoPID>>8)&0x1F), byte(videoPID & 0xFF), // reserved + PCR_PID
		0xF0, 0x00, // reserved + program_info_length = 0
		byte(streamType),                                       // stream_type
		byte(0xE0 | (videoPID>>8)&0x1F), byte(videoPID & 0xFF), // reserved + ES_PID
		0xF0, 0x00, // reserved + ES_info_length = 0
		0x00, 0x00, 0x00, 0x00, // CRC
	}
}

// nal builds one Annex-B HEVC NAL: 00 00 01, then the two-byte header for typ at
// layer 0 (first byte = typ<<1, top bit clear so the scan reads it as a NAL), a
// temporal_id byte, then body.
func nal(typ uint8, body ...byte) []byte {
	return append([]byte{0x00, 0x00, 0x01, typ << 1, 0x01}, body...)
}

func cat(chunks ...[]byte) []byte {
	var out []byte
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

func TestPMTNamesTheHEVCVideoPID(t *testing.T) {
	a := newAnalyzer(zeroClock)
	a.Write(tsPacket(tsPATPID, 0, true, patPayload(0x0100)))
	a.Write(tsPacket(0x0100, 0, true, pmtPayload(0x0041, streamTypeHEVC)))

	if a.videoPID != 0x0041 {
		t.Fatalf("videoPID = 0x%04x, want 0x0041", a.videoPID)
	}
	if st := a.pids[0x0041].streamType; st != streamTypeHEVC {
		t.Fatalf("stream_type = 0x%02x, want HEVC 0x%02x", st, streamTypeHEVC)
	}
}

func TestContinuityRealErrorWhenCCSkips(t *testing.T) {
	a := newAnalyzer(zeroClock)
	// CC 0,1,2 in sequence, then 4 -- 3 is missing, no discontinuity_indicator.
	for _, cc := range []int{0, 1, 2, 4} {
		a.Write(tsPacket(0x0041, cc, false, []byte{0x01, 0x02}))
	}
	ps := a.pids[0x0041]
	if ps.ccReal != 1 {
		t.Errorf("ccReal = %d, want 1", ps.ccReal)
	}
	if ps.ccFlagged != 0 {
		t.Errorf("ccFlagged = %d, want 0", ps.ccFlagged)
	}
}

func TestContinuityFlaggedDiscontinuityIsNotAnError(t *testing.T) {
	a := newAnalyzer(zeroClock)
	a.Write(tsPacket(0x0041, 0, false, []byte{0x01}))
	a.Write(tsPacket(0x0041, 1, false, []byte{0x01}))
	// CC jumps 1 -> 6 but the packet flags the discontinuity: legal.
	a.Write(tsPacketDiscont(0x0041, 6, false, []byte{0x01}))
	ps := a.pids[0x0041]
	if ps.ccReal != 0 {
		t.Errorf("ccReal = %d, want 0 (the jump was flagged)", ps.ccReal)
	}
	if ps.ccFlagged != 1 {
		t.Errorf("ccFlagged = %d, want 1", ps.ccFlagged)
	}
}

func TestContinuityDuplicateIsTolerated(t *testing.T) {
	a := newAnalyzer(zeroClock)
	// CC 0,1,1,2 -- the one repeat the standard permits.
	for _, cc := range []int{0, 1, 1, 2} {
		a.Write(tsPacket(0x0041, cc, false, []byte{0x01}))
	}
	ps := a.pids[0x0041]
	if ps.ccReal != 0 {
		t.Errorf("ccReal = %d, want 0", ps.ccReal)
	}
	if ps.ccDup != 1 {
		t.Errorf("ccDup = %d, want 1", ps.ccDup)
	}
}

func TestNALCensusCountsTypesAndIDRs(t *testing.T) {
	a := newAnalyzer(zeroClock)
	a.Write(tsPacket(tsPATPID, 0, true, patPayload(0x0100)))
	a.Write(tsPacket(0x0100, 0, true, pmtPayload(0x0041, streamTypeHEVC)))

	// One access unit: VPS, SPS, PPS, an IDR slice, then a trailing slice.
	es := cat(
		nal(32, 0x00, 0x11),       // VPS
		nal(33, 0x01, 0x22),       // SPS
		nal(34, 0x33),             // PPS
		nal(19, 0x80, 0x44, 0x55), // IDR_W_RADL (first_slice bit set in body)
		nal(1, 0x80, 0x66),        // TRAIL_R
	)
	a.Write(tsPacket(0x0041, 0, true, es))

	v := a.video
	if got := v.nalCounts[32]; got != 1 {
		t.Errorf("VPS count = %d, want 1", got)
	}
	if got := v.nalCounts[33]; got != 1 {
		t.Errorf("SPS count = %d, want 1", got)
	}
	if got := v.nalCounts[34]; got != 1 {
		t.Errorf("PPS count = %d, want 1", got)
	}
	if got := v.nalCounts[19]; got != 1 {
		t.Errorf("IDR count = %d, want 1", got)
	}
	if got := v.nalCounts[1]; got != 1 {
		t.Errorf("TRAIL_R count = %d, want 1", got)
	}
	if v.idrCount != 1 {
		t.Errorf("idrCount = %d, want 1", v.idrCount)
	}
	if v.nalTotal != 5 {
		t.Errorf("nalTotal = %d, want 5", v.nalTotal)
	}
}

// TestPESStartCodeNotCountedAsNAL guards the top-bit test that separates a PES
// packet-start (00 00 01 then stream_id 0xE0, top bit set) from a real NAL.
func TestNALCensusIgnoresPESStartCodes(t *testing.T) {
	a := newAnalyzer(zeroClock)
	a.Write(tsPacket(tsPATPID, 0, true, patPayload(0x0100)))
	a.Write(tsPacket(0x0100, 0, true, pmtPayload(0x0041, streamTypeHEVC)))

	// A PES start (00 00 01 E0 ...) followed by one real NAL.
	es := cat(
		[]byte{0x00, 0x00, 0x01, 0xE0, 0x00, 0x00}, // PES start code + video stream_id
		nal(1, 0x80, 0x00),                         // one real slice NAL
	)
	a.Write(tsPacket(0x0041, 0, true, es))

	if a.video.nalTotal != 1 {
		t.Errorf("nalTotal = %d, want 1 (the PES start code must not count)", a.video.nalTotal)
	}
}

// TestNALCensusFindsStartCodeSpanningPackets checks a start code split across
// two packets is still found. The first packet is FULL (184 bytes) and ends in
// 00 00, so no stuffing separates the halves -- exactly how a real muxer splits
// a NAL across TS packets; the 01 that completes the start code leads the next.
func TestNALCensusFindsStartCodeSpanningPackets(t *testing.T) {
	a := newAnalyzer(zeroClock)
	a.Write(tsPacket(tsPATPID, 0, true, patPayload(0x0100)))
	a.Write(tsPacket(0x0100, 0, true, pmtPayload(0x0041, streamTypeHEVC)))

	p1 := make([]byte, 184)
	for i := range p1 {
		p1[i] = 0xEE
	}
	p1[182], p1[183] = 0x00, 0x00              // packet ends mid start code
	p2 := []byte{0x01, 0x02, 0x01, 0x80, 0x00} // 01 completes 00 00 01; 0x02 = TRAIL_R header

	a.Write(tsPacket(0x0041, 0, true, p1))
	a.Write(tsPacket(0x0041, 1, false, p2))

	if a.video.nalTotal != 1 {
		t.Errorf("nalTotal = %d, want 1 across the packet boundary", a.video.nalTotal)
	}
}

// encodeTS is decodeTimestamp's inverse: a 33-bit value into its 5-byte,
// marker-interleaved wire form, with prefix bits the decoder ignores.
func encodeTS(prefix byte, ts uint64) []byte {
	return []byte{
		(prefix << 4) | byte(((ts>>30)&0x07)<<1) | 1,
		byte(ts >> 22),
		byte(((ts>>15)&0x7F)<<1) | 1,
		byte(ts >> 7),
		byte((ts&0x7F)<<1) | 1,
	}
}

// pesWithDTS builds a video PES packet start carrying PTS and DTS.
func pesWithDTS(pts, dts uint64) []byte {
	pes := []byte{0x00, 0x00, 0x01, 0xE0, 0x00, 0x00, 0x80, 0xC0, 10}
	pes = append(pes, encodeTS(0x3, pts)...)
	pes = append(pes, encodeTS(0x1, dts)...)
	return append(pes, 0x00, 0x00)
}

func TestDTSBackwardsDetected(t *testing.T) {
	a := newAnalyzer(zeroClock)
	a.Write(tsPacket(tsPATPID, 0, true, patPayload(0x0100)))
	a.Write(tsPacket(0x0100, 0, true, pmtPayload(0x0041, streamTypeHEVC)))
	// Two PES on the video PID with DESCENDING DTS: one backward step of 50000.
	a.Write(tsPacket(0x0041, 0, true, pesWithDTS(100000, 100000)))
	a.Write(tsPacket(0x0041, 1, true, pesWithDTS(50000, 50000)))
	ps := a.pids[0x0041]
	if ps.dtsBack != 1 {
		t.Errorf("dtsBack = %d, want 1", ps.dtsBack)
	}
	if ps.dtsBackMax != 50000 {
		t.Errorf("dtsBackMax = %d, want 50000", ps.dtsBackMax)
	}
}

func TestDTSForwardIsMonotonic(t *testing.T) {
	a := newAnalyzer(zeroClock)
	a.Write(tsPacket(tsPATPID, 0, true, patPayload(0x0100)))
	a.Write(tsPacket(0x0100, 0, true, pmtPayload(0x0041, streamTypeHEVC)))
	a.Write(tsPacket(0x0041, 0, true, pesWithDTS(50000, 50000)))
	a.Write(tsPacket(0x0041, 1, true, pesWithDTS(100000, 100000)))
	if got := a.pids[0x0041].dtsBack; got != 0 {
		t.Errorf("dtsBack = %d, want 0", got)
	}
}
