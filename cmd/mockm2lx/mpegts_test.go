package main

// A hand-rolled MPEG-TS/PES generator, built only for these tests, so the
// analyzer in mpegts.go can be tested against a stream this package fully
// controls rather than requiring a real encoder or another process. It
// writes exactly the fields tsAnalyzer reads: PAT -> PMT -> PES headers with
// PTS/DTS. Everything else (CRC, most reserved bits) is left at whatever
// zero value falls out, because the analyzer under test does not check it
// either — a permissive parser deserves a permissive generator, so the tests
// exercise the same tolerances the production path relies on.

import (
	"fmt"
	"testing"
	"time"
)

// tsWriter builds synthetic, standards-shaped MPEG-TS packets, tracking a
// per-PID continuity counter the way a real muxer would (unused by the
// analyzer today, kept for realism and so a future continuity check has
// something correct to build on).
type tsWriter struct {
	cc map[int]byte
}

func newTSWriter() *tsWriter {
	return &tsWriter{cc: make(map[int]byte)}
}

func (w *tsWriter) nextCC(pid int) byte {
	c := w.cc[pid]
	w.cc[pid] = (c + 1) & 0x0F
	return c
}

// sectionPacket assembles one PSI section (a PAT or a PMT, in these tests
// always small enough to fit a single TS packet) into a full 188-byte
// packet: pointer_field, table_id, section_length, body, the concatenated
// loop entries, a 4-byte CRC placeholder (left zero — the analyzer does not
// validate it), and 0xFF stuffing to pad to 188 bytes.
func (w *tsWriter) sectionPacket(pid int, tableID byte, body []byte, loopEntries [][]byte) []byte {
	var loop []byte
	for _, e := range loopEntries {
		loop = append(loop, e...)
	}
	sectionLength := len(body) + len(loop) + 4 // +4 for the CRC

	pkt := make([]byte, tsPacketSize)
	pkt[0] = tsSyncByte
	pkt[1] = 0x40 | byte(pid>>8)&0x1F // payload_unit_start_indicator + PID hi
	pkt[2] = byte(pid & 0xFF)
	pkt[3] = 0x10 | w.nextCC(pid) // adaptation_field_control='01' (payload only)

	i := 4
	pkt[i] = 0x00 // pointer_field
	i++
	pkt[i] = tableID
	i++
	pkt[i] = 0xB0 | byte((sectionLength>>8)&0x0F)
	pkt[i+1] = byte(sectionLength & 0xFF)
	i += 2
	i += copy(pkt[i:], body)
	i += copy(pkt[i:], loop)
	i += 4 // CRC32 placeholder, left zero

	for ; i < tsPacketSize; i++ {
		pkt[i] = 0xFF
	}
	return pkt
}

// pat builds a PAT naming one program whose PMT lives at pmtPID.
func (w *tsWriter) pat(pmtPID int) []byte {
	body := []byte{0x00, 0x01, 0xC1, 0x00, 0x00} // transport_stream_id=1, version/current, section_number, last_section_number
	entry := []byte{0x00, 0x01, byte(0xE0 | (pmtPID>>8)&0x1F), byte(pmtPID & 0xFF)}
	return w.sectionPacket(0x0000, 0x00, body, [][]byte{entry})
}

// audioEntry describes one audio elementary stream for pmt.
type audioEntry struct {
	PID        int
	StreamType byte
}

// pmt builds a PMT naming an H.264 video PID and zero or more audio PIDs.
func (w *tsWriter) pmt(pmtPID, videoPID int, audio []audioEntry) []byte {
	body := []byte{
		0x00, 0x01, // program_number = 1
		0xC1,       // version/current_next
		0x00, 0x00, // section_number, last_section_number
		byte(0xE0 | (videoPID>>8)&0x1F), byte(videoPID & 0xFF), // PCR_PID (piggybacks on the video PID)
		0xF0, 0x00, // program_info_length = 0
	}
	entries := [][]byte{
		{streamTypeH264, byte(0xE0 | (videoPID>>8)&0x1F), byte(videoPID & 0xFF), 0xF0, 0x00},
	}
	for _, a := range audio {
		entries = append(entries, []byte{a.StreamType, byte(0xE0 | (a.PID>>8)&0x1F), byte(a.PID & 0xFF), 0xF0, 0x00})
	}
	return w.sectionPacket(pmtPID, 0x02, body, entries)
}

// encodeTimestamp is the inverse of decodeTimestamp in mpegts.go: it packs a
// 33-bit 90kHz timestamp into the 5-byte, marker-bit-interleaved wire
// encoding, with the given 4-bit prefix (0x2 = PTS only, 0x3 = PTS of a
// PTS+DTS pair, 0x1 = DTS of a PTS+DTS pair).
func encodeTimestamp(prefix byte, ts uint64) [5]byte {
	var b [5]byte
	b[0] = prefix<<4 | byte((ts>>30)&0x07)<<1 | 0x01
	b[1] = byte((ts >> 22) & 0xFF)
	b[2] = byte((ts>>15)&0x7F)<<1 | 0x01
	b[3] = byte((ts >> 7) & 0xFF)
	b[4] = byte(ts&0x7F)<<1 | 0x01
	return b
}

// pes builds one TS packet carrying a PES header (with a PTS+DTS pair when
// haveDTS is set) starting at the payload, followed by n bytes of filler
// elementary-stream payload. The analyzer only reads the header, so the
// filler content is never inspected.
func (w *tsWriter) pes(pid int, streamID byte, dts uint64, haveDTS bool, payloadLen int) []byte {
	var hdr []byte
	var flags2 byte
	if haveDTS {
		flags2 = 0xC0 // '11': PTS and DTS both present
		pts := encodeTimestamp(0x3, dts)
		dtsB := encodeTimestamp(0x1, dts)
		hdr = append(hdr, pts[:]...)
		hdr = append(hdr, dtsB[:]...)
	}
	pesHeader := []byte{0x00, 0x00, 0x01, streamID, 0x00, 0x00, 0x80, flags2, byte(len(hdr))}

	full := append(append([]byte{}, pesHeader...), hdr...)
	full = append(full, make([]byte, payloadLen)...)

	pkt := make([]byte, tsPacketSize)
	pkt[0] = tsSyncByte
	pkt[1] = 0x40 | byte(pid>>8)&0x1F
	pkt[2] = byte(pid & 0xFF)
	pkt[3] = 0x10 | w.nextCC(pid)

	n := copy(pkt[4:], full)
	for i := 4 + n; i < tsPacketSize; i++ {
		pkt[i] = 0xFF
	}
	return pkt
}

// --- tests -----------------------------------------------------------------

const (
	testPMTPID   = 0x1000
	testVideoPID = 0x0100
	testAudioPID = 0x0101
)

// baseStream returns a PAT + PMT (H.264 video, AAC ADTS audio) with no PES
// packets yet, ready for a test to append PES packets of its own.
func baseStream(w *tsWriter) []byte {
	var s []byte
	s = append(s, w.pat(testPMTPID)...)
	s = append(s, w.pmt(testPMTPID, testVideoPID, []audioEntry{{testAudioPID, streamTypeAACADTS}})...)
	return s
}

func TestAnalyzer_IdentifiesH264AndAAC(t *testing.T) {
	w := newTSWriter()
	stream := baseStream(w)
	stream = append(stream, w.pes(testVideoPID, 0xE0, 90000, true, 100)...)
	stream = append(stream, w.pes(testAudioPID, 0xC0, 90000, true, 50)...)

	an := newTSAnalyzer()
	if _, err := an.Write(stream); err != nil {
		t.Fatalf("Write: %v", err)
	}

	snap := an.Snapshot()
	if !snap.HaveVideoPID || !snap.VideoIsH264 {
		t.Errorf("expected H.264 video PID detected, got HaveVideoPID=%v VideoIsH264=%v", snap.HaveVideoPID, snap.VideoIsH264)
	}
	if !snap.HaveAudioPID || !snap.AudioIsAAC {
		t.Errorf("expected AAC audio PID detected, got HaveAudioPID=%v AudioIsAAC=%v", snap.HaveAudioPID, snap.AudioIsAAC)
	}
	if snap.DTSBackwards {
		t.Errorf("expected no DTS regression, got %q", snap.DTSBackwardsDetail)
	}
	if snap.BytesTotal != uint64(len(stream)) {
		t.Errorf("BytesTotal = %d, want %d", snap.BytesTotal, len(stream))
	}
}

func TestAnalyzer_NonAACAudioIsNotReportedAsAAC(t *testing.T) {
	tests := []struct {
		name       string
		streamType byte
	}{
		{"MPEG1 audio (MP2)", streamTypeMPEG1Audio},
		{"MPEG2 audio (MP2)", streamTypeMPEG2Audio},
		{"AC-3", streamTypeAC3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newTSWriter()
			var stream []byte
			stream = append(stream, w.pat(testPMTPID)...)
			stream = append(stream, w.pmt(testPMTPID, testVideoPID, []audioEntry{{testAudioPID, tc.streamType}})...)
			stream = append(stream, w.pes(testVideoPID, 0xE0, 90000, true, 10)...)
			stream = append(stream, w.pes(testAudioPID, 0xC0, 90000, true, 10)...)

			an := newTSAnalyzer()
			an.Write(stream)
			snap := an.Snapshot()

			if !snap.HaveAudioPID {
				t.Fatalf("expected the audio PID to be detected at all")
			}
			if snap.AudioIsAAC {
				t.Errorf("stream_type 0x%02X must not be reported as AAC — this is the MP2/AC-3 silent-drop signature the AUDIO OK lamp exists to catch", tc.streamType)
			}
		})
	}
}

func TestAnalyzer_MixedAACAndNonAACIsNotAllAAC(t *testing.T) {
	const secondAudioPID = 0x0102
	w := newTSWriter()
	var stream []byte
	stream = append(stream, w.pat(testPMTPID)...)
	stream = append(stream, w.pmt(testPMTPID, testVideoPID, []audioEntry{
		{testAudioPID, streamTypeAACADTS},
		{secondAudioPID, streamTypeMPEG1Audio},
	})...)

	an := newTSAnalyzer()
	an.Write(stream)
	snap := an.Snapshot()

	if !snap.HaveAudioPID {
		t.Fatalf("expected audio PIDs to be detected")
	}
	if snap.AudioIsAAC {
		t.Errorf("AudioIsAAC must be false when any audio PID is not AAC, even if another one is")
	}
}

func TestAnalyzer_DetectsDTSRegression(t *testing.T) {
	w := newTSWriter()
	stream := baseStream(w)
	// A normal, monotonically increasing run first.
	stream = append(stream, w.pes(testVideoPID, 0xE0, 90000, true, 10)...)
	stream = append(stream, w.pes(testVideoPID, 0xE0, 93600, true, 10)...) // +40ms @ 90kHz
	stream = append(stream, w.pes(testVideoPID, 0xE0, 97200, true, 10)...)

	an := newTSAnalyzer()
	an.Write(stream)
	if snap := an.Snapshot(); snap.DTSBackwards {
		t.Fatalf("false positive: monotonic DTS reported as backwards (%s)", snap.DTSBackwardsDetail)
	}

	// This is the measured bug (spec section 6.1): a pipeline restart resets
	// running time to zero, so the very next DTS on the wire is far lower
	// than the last one the peer saw. 1000 (90kHz units) is arbitrary; what
	// matters is that it is less than 97200.
	an.Write(w.pes(testVideoPID, 0xE0, 1000, true, 10))

	snap := an.Snapshot()
	if !snap.DTSBackwards {
		t.Fatalf("expected DTS regression to be detected")
	}
	if snap.DTSBackwardsDetail == "" {
		t.Errorf("expected a non-empty DTSBackwardsDetail")
	}
	t.Logf("detected: %s", snap.DTSBackwardsDetail)
}

func TestAnalyzer_DTSRegressionLatchesForTheSession(t *testing.T) {
	w := newTSWriter()
	an := newTSAnalyzer()
	an.Write(baseStream(w))
	an.Write(w.pes(testVideoPID, 0xE0, 50000, true, 10))
	an.Write(w.pes(testVideoPID, 0xE0, 10000, true, 10)) // regression #1
	firstDetail := an.Snapshot().DTSBackwardsDetail

	// A second, later regression must not overwrite the first — the first
	// one is what a human debugging a soak test needs to see.
	an.Write(w.pes(testVideoPID, 0xE0, 60000, true, 10))
	an.Write(w.pes(testVideoPID, 0xE0, 20000, true, 10)) // regression #2

	snap := an.Snapshot()
	if !snap.DTSBackwards {
		t.Fatalf("expected DTSBackwards to remain latched true")
	}
	if snap.DTSBackwardsDetail != firstDetail {
		t.Errorf("detail changed after a second regression: got %q, want the first detail %q", snap.DTSBackwardsDetail, firstDetail)
	}
}

func TestAnalyzer_FirstDTSBackwardsFiresOnce(t *testing.T) {
	w := newTSWriter()
	an := newTSAnalyzer()
	an.Write(baseStream(w))

	if _, first := an.FirstDTSBackwards(); first {
		t.Fatalf("FirstDTSBackwards must be false before any regression")
	}

	an.Write(w.pes(testVideoPID, 0xE0, 50000, true, 10))
	an.Write(w.pes(testVideoPID, 0xE0, 10000, true, 10)) // regression

	detail, first := an.FirstDTSBackwards()
	if !first || detail == "" {
		t.Fatalf("expected FirstDTSBackwards to fire once with a detail, got first=%v detail=%q", first, detail)
	}

	if _, first := an.FirstDTSBackwards(); first {
		t.Errorf("FirstDTSBackwards must not fire a second time for the same regression")
	}
}

func TestAnalyzer_TracksDTSPerPIDIndependently(t *testing.T) {
	// A regression on the audio PID must not be masked, or triggered, by
	// unrelated video PID timestamps.
	w := newTSWriter()
	an := newTSAnalyzer()
	an.Write(baseStream(w))

	an.Write(w.pes(testVideoPID, 0xE0, 100000, true, 10))
	an.Write(w.pes(testAudioPID, 0xC0, 50000, true, 10))
	an.Write(w.pes(testVideoPID, 0xE0, 200000, true, 10)) // video keeps increasing
	if an.Snapshot().DTSBackwards {
		t.Fatalf("video-only increase must not trip the detector")
	}

	an.Write(w.pes(testAudioPID, 0xC0, 10000, true, 10)) // audio regresses
	snap := an.Snapshot()
	if !snap.DTSBackwards {
		t.Fatalf("expected the audio-PID regression to be detected")
	}
}

func TestAnalyzer_ResyncsAfterGarbagePrefix(t *testing.T) {
	w := newTSWriter()
	stream := baseStream(w)
	stream = append(stream, w.pes(testVideoPID, 0xE0, 90000, true, 10)...)
	stream = append(stream, w.pes(testAudioPID, 0xC0, 90000, true, 10)...)

	garbage := []byte{0x00, 0x01, 0x02, 0x03, 0x04}
	an := newTSAnalyzer()
	an.Write(append(garbage, stream...))

	snap := an.Snapshot()
	if !snap.VideoIsH264 || !snap.AudioIsAAC {
		t.Errorf("expected the analyzer to resync past a garbage prefix and still identify the streams, got VideoIsH264=%v AudioIsAAC=%v", snap.VideoIsH264, snap.AudioIsAAC)
	}
}

func TestAnalyzer_SurvivesArbitraryChunkBoundaries(t *testing.T) {
	// The SRT connection hands the analyzer whatever byte count Read
	// returns, which need not align with 188-byte packet boundaries. Feed
	// the same stream one byte at a time and confirm the result is
	// identical to feeding it whole.
	w := newTSWriter()
	stream := baseStream(w)
	stream = append(stream, w.pes(testVideoPID, 0xE0, 90000, true, 10)...)
	stream = append(stream, w.pes(testAudioPID, 0xC0, 90000, true, 10)...)
	stream = append(stream, w.pes(testVideoPID, 0xE0, 1000, true, 10)...) // regression

	an := newTSAnalyzer()
	for _, b := range stream {
		an.Write([]byte{b})
	}

	snap := an.Snapshot()
	if !snap.VideoIsH264 || !snap.AudioIsAAC {
		t.Errorf("byte-at-a-time feed: expected streams identified, got VideoIsH264=%v AudioIsAAC=%v", snap.VideoIsH264, snap.AudioIsAAC)
	}
	if !snap.DTSBackwards {
		t.Errorf("byte-at-a-time feed: expected the DTS regression to still be detected")
	}
	if snap.BytesTotal != uint64(len(stream)) {
		t.Errorf("BytesTotal = %d, want %d", snap.BytesTotal, len(stream))
	}
}

func TestAnalyzer_Bitrate(t *testing.T) {
	an := newTSAnalyzer()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	an.now = func() time.Time { return now }

	chunk := make([]byte, 1316) // one SRT payload, spec section 5
	an.Write(chunk)

	if snap := an.Snapshot(); snap.BitrateBps != 0 {
		t.Errorf("bitrate before any elapsed time must be 0, got %v", snap.BitrateBps)
	}

	now = now.Add(1 * time.Second)
	an.Write(chunk)

	snap := an.Snapshot()
	wantBitrate := float64(len(chunk)*2) * 8 // two chunks over one second
	if snap.BitrateBps != wantBitrate {
		t.Errorf("BitrateBps = %v, want %v", snap.BitrateBps, wantBitrate)
	}
}

func TestAnalyzer_EmptyWriteIsANoOp(t *testing.T) {
	an := newTSAnalyzer()
	n, err := an.Write(nil)
	if n != 0 || err != nil {
		t.Fatalf("Write(nil) = (%d, %v), want (0, nil)", n, err)
	}
	if snap := an.Snapshot(); snap.BytesTotal != 0 {
		t.Errorf("BytesTotal after an empty write = %d, want 0", snap.BytesTotal)
	}
}

// TestDecodeEncodeTimestampRoundTrip is a property check over the bit-packing
// math shared between mpegts.go's decodeTimestamp and this file's
// encodeTimestamp: every 33-bit value must survive the round trip.
func TestDecodeEncodeTimestampRoundTrip(t *testing.T) {
	values := []uint64{0, 1, 90000, 1 << 32, (1 << 33) - 1, 0x1FFFFFFFF, 12345678901}
	for _, v := range values {
		v := v & 0x1FFFFFFFF // clamp to 33 bits, as the real field is
		t.Run(fmt.Sprintf("%d", v), func(t *testing.T) {
			enc := encodeTimestamp(0x3, v)
			got := decodeTimestamp(enc[:])
			if got != v {
				t.Errorf("round trip: encode(%d) -> decode = %d", v, got)
			}
		})
	}
}
