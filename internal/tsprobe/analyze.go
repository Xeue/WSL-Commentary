// The transport-stream dissector. It answers, from the bytes actually received
// over SRT, the one question a day of guessing could not settle: is the video
// the field machine is handed already corrupt on arrival, or does it arrive
// intact and break later in decode?
//
// It parses PAT -> PMT -> per-PID continuity, and reassembles the HEVC
// elementary stream far enough to census its NAL units. The load-bearing
// measurement is per-PID CONTINUITY-COUNTER analysis, split three ways:
//
//   - a REAL error: the 4-bit continuity_counter skipped, with no
//     discontinuity_indicator set to say it was meant to. That is a genuinely
//     lost/corrupt run of transport packets -- a hole in the elementary stream
//     that WILL tear the picture, and it is invisible to SRT's own "0 lost"
//     because SRT counts its packets, not the transport-stream CC inside them.
//   - a FLAGGED discontinuity: the CC jumped but the adaptation field's
//     discontinuity_indicator said so. That is legal and intentional (a splice,
//     a PCR reset); tsdemux still logs "CONTINUITY: Mismatch" for it, so it is
//     exactly the noise that makes that warning untrustworthy on its own.
//   - a duplicate: the one repeated packet the standard permits. Benign.
//
// Run this on both machines and diff. If COMM-01 shows REAL errors on the video
// PID and the dev box shows none, the stream is arriving broken and the search
// moves to the link/receiver. If both show only FLAGGED ones (or none), the
// received bytes are the same and the fault is downstream in decode. Either way
// the answer is a number in this report, not a theory.
//
// Pure Go, no GStreamer: it reads the same SRT the app's srtsrc reads, so what
// it sees is what the pipeline was handed.
package tsprobe

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// MPEG-TS constants (ISO/IEC 13818-1).
const (
	tsPacketSize = 188
	tsSyncByte   = 0x47
	tsPATPID     = 0x0000
)

// Elementary stream types this tool names (ISO/IEC 13818-1 Table 2-34). The one
// that matters here is HEVC (0x24): the M2L-X return is 1080p50 H.265, and the
// whole point is to find its PID and watch its continuity.
const (
	streamTypeH264     = 0x1B
	streamTypeHEVC     = 0x24
	streamTypeAACADTS  = 0x0F
	streamTypeAACLATM  = 0x11
	streamTypeMP1Audio = 0x03
	streamTypeMP2Audio = 0x04
	streamTypeAC3      = 0x81
)

func streamTypeName(st int) string {
	switch st {
	case streamTypeH264:
		return "H.264 video"
	case streamTypeHEVC:
		return "HEVC video"
	case streamTypeAACADTS:
		return "AAC audio (ADTS)"
	case streamTypeAACLATM:
		return "AAC audio (LATM)"
	case streamTypeMP1Audio:
		return "MPEG-1 audio"
	case streamTypeMP2Audio:
		return "MPEG-2 audio"
	case streamTypeAC3:
		return "AC-3 audio"
	default:
		return fmt.Sprintf("stream_type 0x%02x", st)
	}
}

func isVideoType(st int) bool { return st == streamTypeHEVC || st == streamTypeH264 }

// hevcNALName names the NAL unit types the census reports on. Coarse on purpose:
// the report needs "how many parameter sets, how many IDRs, how many slices",
// not a line per temporal sub-layer.
func hevcNALName(t uint8) string {
	switch {
	case t == 32:
		return "VPS"
	case t == 33:
		return "SPS"
	case t == 34:
		return "PPS"
	case t == 35:
		return "AUD"
	case t == 39 || t == 40:
		return "SEI"
	case t == 19 || t == 20:
		return "IDR"
	case t == 21:
		return "CRA"
	case t <= 21:
		return "slice (non-IRAP)"
	default:
		return fmt.Sprintf("nal type %d", t)
	}
}

// pidStat is the continuity picture for one PID.
type pidStat struct {
	pid        int
	streamType int // -1 until a PMT names it; PAT/PMT PIDs stay -1
	packets    uint64
	payload    uint64 // packets that carried payload (the ones CC counts)
	lastCC     int    // -1 until the first payload packet on this PID
	ccDup      uint64
	ccReal     uint64 // unflagged CC discontinuities: real holes
	ccFlagged  uint64 // discontinuity_indicator was set: intentional
	tei        uint64 // transport_error_indicator packets
	firstReal  time.Duration
	realBySec  map[int]int // second -> real CC errors, for the timeline
	gapHist    map[int]int // apparent packets skipped -> count

	// PES timestamp monotonicity. A DTS that goes backwards is the measured
	// M2L-X pipeline-restart failure (cmd/mockm2lx exists to detect it): a
	// non-monotonic decode timestamp jams the decoder and the picture with it.
	lastDTS    uint64
	haveDTS    bool
	dtsBack    uint64 // times DTS went backwards on this PID
	dtsBackMax uint64 // largest backward jump seen, 90 kHz units
	pesStarts  uint64 // PES packets seen (PUSI with a start code)
}

func newPIDStat(pid int) *pidStat {
	return &pidStat{
		pid:        pid,
		streamType: -1,
		lastCC:     -1,
		realBySec:  make(map[int]int),
		gapHist:    make(map[int]int),
	}
}

// analyzer incrementally parses the TS stream fed to it via Write, in whatever
// chunk sizes the SRT reader returns. now() is the receive clock, injected so
// tests are deterministic.
type Analyzer struct {
	mu    sync.Mutex
	now   func() time.Duration // elapsed since the first byte
	start bool

	buf  []byte // bytes not yet consumed as whole 188-byte packets
	sync uint64 // packets dropped resynchronising to 0x47

	pmtPID   int
	videoPID int
	pids     map[int]*pidStat

	video *videoStream // NAL census on the HEVC elementary stream

	bytesTotal uint64
	firstAt    time.Duration
	lastAt     time.Duration
}

func NewAnalyzer(now func() time.Duration) *Analyzer {
	return &Analyzer{
		now:      now,
		pmtPID:   -1,
		videoPID: -1,
		pids:     make(map[int]*pidStat),
		video:    newVideoStream(),
	}
}

func (a *Analyzer) stat(pid int) *pidStat {
	ps := a.pids[pid]
	if ps == nil {
		ps = newPIDStat(pid)
		a.pids[pid] = ps
	}
	return ps
}

// Write feeds the next slice of the SRT byte stream. Like the mock's analyzer it
// never errors: a resyncing or holed stream is precisely what it exists to
// measure, not to fail on.
func (a *Analyzer) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(p) == 0 {
		return 0, nil
	}
	now := a.now()
	if !a.start {
		a.start = true
		a.firstAt = now
	}
	a.lastAt = now
	a.bytesTotal += uint64(len(p))
	a.buf = append(a.buf, p...)

	for {
		for len(a.buf) > 0 && a.buf[0] != tsSyncByte {
			a.buf = a.buf[1:]
			a.sync++
		}
		if len(a.buf) < tsPacketSize {
			break
		}
		pkt := a.buf[:tsPacketSize]
		a.buf = a.buf[tsPacketSize:]
		a.processPacket(pkt, now)
	}
	return len(p), nil
}

// processPacket parses one 188-byte packet (pkt[0] == 0x47) and updates the
// per-PID continuity and, for the video PID, the NAL census.
func (a *Analyzer) processPacket(pkt []byte, now time.Duration) {
	pid := int(pkt[1]&0x1F)<<8 | int(pkt[2])
	ps := a.stat(pid)
	ps.packets++

	if pkt[1]&0x80 != 0 { // transport_error_indicator
		ps.tei++
	}
	pusi := pkt[1]&0x40 != 0
	afc := (pkt[3] >> 4) & 0x03
	cc := int(pkt[3] & 0x0F)

	hasAdapt := afc == 2 || afc == 3
	hasPayload := afc == 1 || afc == 3

	discont := false
	i := 4
	if hasAdapt {
		if i >= len(pkt) {
			return
		}
		adaptLen := int(pkt[i])
		if adaptLen > 0 && i+1 < len(pkt) {
			discont = pkt[i+1]&0x80 != 0 // discontinuity_indicator
		}
		i += 1 + adaptLen
		if i > len(pkt) {
			return
		}
	}

	// Continuity counter: it increments by one per PAYLOAD-bearing packet on a
	// PID (ISO/IEC 13818-1 2.4.3.3); packets with no payload do not advance it.
	if hasPayload {
		if ps.lastCC >= 0 {
			expected := (ps.lastCC + 1) & 0x0F
			switch {
			case cc == expected:
				// in sequence
			case cc == ps.lastCC:
				ps.ccDup++ // the single duplicate the standard permits
			case discont:
				ps.ccFlagged++ // a signalled, legal discontinuity
			default:
				ps.ccReal++
				if ps.ccReal == 1 {
					ps.firstReal = now
				}
				ps.realBySec[int(now.Seconds())]++
				ps.gapHist[(cc-expected+16)&0x0F]++
			}
		}
		ps.lastCC = cc
		ps.payload++
	}

	if !hasPayload || i >= len(pkt) {
		return
	}
	payload := pkt[i:]
	switch {
	case pid == tsPATPID:
		a.parsePAT(payload, pusi)
	case a.pmtPID >= 0 && pid == a.pmtPID:
		a.parsePMT(payload, pusi)
	default:
		if ps.streamType >= 0 { // a PMT-named elementary stream
			a.parsePES(ps, payload, pusi)
			if pid == a.videoPID {
				a.video.feed(payload, now)
			}
		}
	}
}

// parsePES reads a PES packet's optional header at PUSI and tracks decode-
// timestamp monotonicity for one elementary stream. DTS is preferred (it is the
// monotonic one; PTS reorders around B-frames); a stream carrying only PTS is
// tracked on PTS, where the bug being hunted -- a pipeline-restart DTS jumping
// back by seconds -- still dwarfs any legitimate reorder.
func (a *Analyzer) parsePES(ps *pidStat, payload []byte, pusi bool) {
	if !pusi || len(payload) < 9 {
		return
	}
	if payload[0] != 0x00 || payload[1] != 0x00 || payload[2] != 0x01 {
		return
	}
	ps.pesStarts++
	if payload[6]&0xC0 != 0x80 {
		return // not the standard '10' PES optional-header marker
	}
	ptsDTSFlags := (payload[7] >> 6) & 0x03
	headerDataLength := int(payload[8])
	hdr := payload[9:]
	if len(hdr) < headerDataLength {
		return
	}
	var ts uint64
	switch ptsDTSFlags {
	case 0x2: // PTS only
		if len(hdr) < 5 {
			return
		}
		ts = decodeTimestamp(hdr[0:5])
	case 0x3: // PTS then DTS -- DTS is what decode order uses
		if len(hdr) < 10 {
			return
		}
		ts = decodeTimestamp(hdr[5:10])
	default:
		return
	}
	if ps.haveDTS && ts < ps.lastDTS {
		ps.dtsBack++
		if d := ps.lastDTS - ts; d > ps.dtsBackMax {
			ps.dtsBackMax = d
		}
	}
	ps.lastDTS = ts
	ps.haveDTS = true
}

// decodeTimestamp reconstructs a 33-bit 90 kHz PTS/DTS from its 5-byte
// marker-interleaved wire form (ISO/IEC 13818-1 2.4.3.6).
func decodeTimestamp(b []byte) uint64 {
	_ = b[4]
	return uint64(b[0]>>1&0x07)<<30 |
		uint64(b[1])<<22 |
		uint64(b[2]>>1&0x7F)<<15 |
		uint64(b[3])<<7 |
		uint64(b[4]>>1&0x7F)
}

// parsePAT records the first program's PMT PID.
func (a *Analyzer) parsePAT(payload []byte, pusi bool) {
	if !pusi || len(payload) < 1 {
		return
	}
	i := 1 + int(payload[0]) // pointer_field
	if i+8 > len(payload) {
		return
	}
	sectionLen := int(payload[i+1]&0x0F)<<8 | int(payload[i+2])
	end := i + 3 + sectionLen
	if end > len(payload) {
		end = len(payload)
	}
	for j := i + 3 + 5; j+4 <= end-4; j += 4 {
		program := int(payload[j])<<8 | int(payload[j+1])
		mapPID := int(payload[j+2]&0x1F)<<8 | int(payload[j+3])
		if program != 0 {
			a.pmtPID = mapPID
		}
	}
}

// parsePMT records every elementary stream's PID and stream_type, and marks the
// HEVC (or H.264) one as the video PID.
func (a *Analyzer) parsePMT(payload []byte, pusi bool) {
	if !pusi || len(payload) < 1 {
		return
	}
	i := 1 + int(payload[0])
	if i+12 > len(payload) {
		return
	}
	sectionLen := int(payload[i+1]&0x0F)<<8 | int(payload[i+2])
	end := i + 3 + sectionLen
	if end > len(payload) {
		end = len(payload)
	}
	piLenOff := i + 3 + 7
	if piLenOff+2 > len(payload) {
		return
	}
	programInfoLen := int(payload[piLenOff]&0x0F)<<8 | int(payload[piLenOff+1])
	j := piLenOff + 2 + programInfoLen

	for j+5 <= end-4 {
		st := int(payload[j])
		esPID := int(payload[j+1]&0x1F)<<8 | int(payload[j+2])
		esInfoLen := int(payload[j+3]&0x0F)<<8 | int(payload[j+4])
		j += 5 + esInfoLen
		if j > end {
			break
		}
		a.stat(esPID).streamType = st
		if isVideoType(st) && a.videoPID < 0 {
			a.videoPID = esPID
		}
	}
}

// videoStream counts HEVC NAL units on the reassembled elementary stream. It
// does not decode: it locates Annex-B start codes and reads nal_unit_type, which
// is enough to say how often parameter sets and IDRs actually arrive and whether
// the slice count collapses when the CC holes above appear.
type videoStream struct {
	buf       []byte // accumulated ES payload bytes, compacted as scanned
	scanned   int
	nalCounts map[uint8]uint64
	nalTotal  uint64
	idrCount  uint64
	lastIDR   time.Duration
	idrGaps   []time.Duration
}

func newVideoStream() *videoStream {
	return &videoStream{nalCounts: make(map[uint8]uint64)}
}

// feed appends one video-PID TS payload and scans for NAL start codes. A PES
// packet header also begins 00 00 01, but its next byte is a stream_id >= 0xB0
// (top bit set), whereas an HEVC NAL header has forbidden_zero_bit == 0 (top bit
// clear) -- so the top-bit test cleanly separates NAL start codes from PES ones
// without reassembling PES headers. Emulation prevention (00 00 03) can never
// spell 00 00 01, so the scan is exact.
func (v *videoStream) feed(payload []byte, now time.Duration) {
	v.buf = append(v.buf, payload...)
	i := v.scanned
	for i <= len(v.buf)-4 {
		if v.buf[i] == 0 && v.buf[i+1] == 0 && v.buf[i+2] == 1 {
			hdr := v.buf[i+3]
			if hdr&0x80 == 0 { // NAL, not a PES start code
				t := (hdr >> 1) & 0x3F
				v.nalCounts[t]++
				v.nalTotal++
				if t == 19 || t == 20 { // IDR_W_RADL / IDR_N_LP
					v.idrCount++
					if v.lastIDR > 0 {
						v.idrGaps = append(v.idrGaps, now-v.lastIDR)
					}
					v.lastIDR = now
				}
			}
			i += 3
			continue
		}
		i++
	}
	v.scanned = i
	// Compact: keep only the trailing bytes a start code could still span.
	if v.scanned > 1<<16 {
		drop := v.scanned - 3
		v.buf = append(v.buf[:0], v.buf[drop:]...)
		v.scanned = len(v.buf)
	}
}

// report renders everything the analyzer has learned as a stable, diffable text
// block, ending in a verdict that reads the numbers so a human does not have to.
func (a *Analyzer) Report(target string, dur time.Duration) string {
	a.mu.Lock()
	defer a.mu.Unlock()

	var b strings.Builder
	span := a.lastAt - a.firstAt
	mbps := 0.0
	if span > 0 {
		mbps = float64(a.bytesTotal) * 8 / span.Seconds() / 1e6
	}

	fmt.Fprintf(&b, "=== WSLComms SRT/TS probe ===\n")
	fmt.Fprintf(&b, "target      : %s\n", target)
	fmt.Fprintf(&b, "ran         : %s (data span %.1fs)\n", dur.Round(time.Millisecond), span.Seconds())
	fmt.Fprintf(&b, "bytes       : %d (%.2f Mbps)\n", a.bytesTotal, mbps)
	fmt.Fprintf(&b, "TS packets  : %d\n", a.bytesTotal/tsPacketSize)
	fmt.Fprintf(&b, "resync drops: %d\n", a.sync)
	fmt.Fprintf(&b, "video PID   : %s\n\n", pidHex(a.videoPID))

	// PID table, sorted, so two machines' reports line up for a diff.
	pids := make([]int, 0, len(a.pids))
	for pid := range a.pids {
		pids = append(pids, pid)
	}
	sort.Ints(pids)

	fmt.Fprintf(&b, "%-8s %-16s %10s %10s %9s %9s %8s %6s\n",
		"PID", "type", "packets", "payload", "cc-REAL", "cc-flag", "cc-dup", "tei")
	for _, pid := range pids {
		ps := a.pids[pid]
		typ := "-"
		switch {
		case pid == tsPATPID:
			typ = "PAT"
		case pid == a.pmtPID:
			typ = "PMT"
		case ps.streamType >= 0:
			typ = streamTypeName(ps.streamType)
		}
		fmt.Fprintf(&b, "0x%04x   %-16s %10d %10d %9d %9d %8d %6d\n",
			pid, typ, ps.packets, ps.payload, ps.ccReal, ps.ccFlagged, ps.ccDup, ps.tei)
	}

	// The video PID gets the detail, because it is the one the verdict turns on.
	if vps := a.pids[a.videoPID]; a.videoPID >= 0 && vps != nil {
		fmt.Fprintf(&b, "\n--- video PID %s continuity ---\n", pidHex(a.videoPID))
		if vps.ccReal > 0 {
			fmt.Fprintf(&b, "first REAL cc error at: %.2fs\n", vps.firstReal.Seconds())
			fmt.Fprintf(&b, "apparent packets skipped per error: %s\n", histLine(vps.gapHist))
			fmt.Fprintf(&b, "real cc errors by 10s window: %s\n", timelineLine(vps.realBySec))
		} else {
			fmt.Fprintf(&b, "no unflagged continuity errors on the video PID\n")
		}
	}

	// PES decode-timestamp monotonicity per elementary stream. A backward DTS is
	// the measured pipeline-restart failure and jams the decoder.
	fmt.Fprintf(&b, "\n--- PES decode-timestamp monotonicity ---\n")
	anyPES := false
	for _, pid := range pids {
		ps := a.pids[pid]
		if ps.pesStarts == 0 {
			continue
		}
		anyPES = true
		note := "monotonic"
		if ps.dtsBack > 0 {
			note = fmt.Sprintf("*** %d BACKWARD step(s), max -%d (%.3fs) ***",
				ps.dtsBack, ps.dtsBackMax, float64(ps.dtsBackMax)/90000.0)
		}
		typ := "-"
		if ps.streamType >= 0 {
			typ = streamTypeName(ps.streamType)
		}
		fmt.Fprintf(&b, "  0x%04x %-16s PES-packets %d  DTS %s\n", pid, typ, ps.pesStarts, note)
	}
	if !anyPES {
		fmt.Fprintf(&b, "  (no PES headers parsed)\n")
	}

	// NAL census.
	fmt.Fprintf(&b, "\n--- HEVC NAL census (video PID) ---\n")
	fmt.Fprintf(&b, "NAL units   : %d\n", a.video.nalTotal)
	types := make([]int, 0, len(a.video.nalCounts))
	for t := range a.video.nalCounts {
		types = append(types, int(t))
	}
	sort.Ints(types)
	for _, t := range types {
		fmt.Fprintf(&b, "  %-18s (%2d): %d\n", hevcNALName(uint8(t)), t, a.video.nalCounts[uint8(t)])
	}
	fmt.Fprintf(&b, "IDR frames  : %d%s\n", a.video.idrCount, idrGapSummary(a.video.idrGaps))

	b.WriteString("\n--- verdict ---\n")
	b.WriteString(a.verdict())
	return b.String()
}

// verdict reads the numbers into the one sentence the whole exercise is for.
func (a *Analyzer) verdict() string {
	vps := a.pids[a.videoPID]
	switch {
	case a.videoPID < 0 || vps == nil:
		return "No HEVC/H.264 video PID was seen. The PMT never named one, or no data arrived.\n"
	case vps.dtsBack > 0:
		return fmt.Sprintf(
			"The video PID's DECODE TIMESTAMP went BACKWARDS %d time(s) (largest -%.3fs).\n"+
				"That is the measured pipeline-restart failure -- a non-monotonic DTS jams the\n"+
				"decoder and the picture with it -- and it is a sender-side problem, not decode.\n"+
				"(This coexists with the continuity numbers above; both are worth reporting.)\n",
			vps.dtsBack, float64(vps.dtsBackMax)/90000.0)
	case vps.ccReal > 0:
		return fmt.Sprintf(
			"REAL continuity errors on the video PID: %d unflagged CC holes in %.0fs.\n"+
				"The transport stream is arriving with genuine gaps in the video -- packets the\n"+
				"muxer's continuity counter says are missing and that no discontinuity_indicator\n"+
				"excuses. Each is a shredded run of the elementary stream: THIS is a tear source,\n"+
				"and it is upstream of the decoder. Run the identical probe on a machine that does\n"+
				"NOT tear: if it shows 0 here, the stream is arriving broken only on this box and\n"+
				"the fault is the link/receiver, not decode.\n", vps.ccReal, (a.lastAt - a.firstAt).Seconds())
	case vps.ccFlagged > 0:
		return fmt.Sprintf(
			"The video PID has %d FLAGGED discontinuities and 0 real ones.\n"+
				"Every continuity jump carried a discontinuity_indicator -- they are intentional and\n"+
				"legal, and the elementary stream is intact. tsdemux's \"CONTINUITY: Mismatch\"\n"+
				"warnings are this, and on their own they are NOT the tear. The received bytes are\n"+
				"sound; the fault is downstream, in decode.\n", vps.ccFlagged)
	default:
		return "The video PID has NO continuity errors of any kind -- the received transport\n" +
			"stream is clean. Whatever tears the picture is downstream of the bytes on the\n" +
			"wire: decode, or presentation. Not the stream.\n"
	}
}

func pidHex(pid int) string {
	if pid < 0 {
		return "(none)"
	}
	return fmt.Sprintf("0x%04x", pid)
}

func histLine(h map[int]int) string {
	keys := make([]int, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d:%d", k, h[k]))
	}
	return strings.Join(parts, " ")
}

// timelineLine buckets the per-second error counts into 10s windows so a long
// run stays one line and a burst is still visible against a steady trickle.
func timelineLine(bySec map[int]int) string {
	buckets := make(map[int]int)
	maxB := 0
	for s, n := range bySec {
		bkt := s / 10
		buckets[bkt] += n
		if bkt > maxB {
			maxB = bkt
		}
	}
	parts := make([]string, 0, maxB+1)
	for bkt := 0; bkt <= maxB; bkt++ {
		parts = append(parts, fmt.Sprintf("%d", buckets[bkt]))
	}
	return "[" + strings.Join(parts, " ") + "] (per 10s)"
}

func idrGapSummary(gaps []time.Duration) string {
	if len(gaps) == 0 {
		return ""
	}
	var min, max, sum time.Duration
	min = gaps[0]
	for _, g := range gaps {
		if g < min {
			min = g
		}
		if g > max {
			max = g
		}
		sum += g
	}
	avg := sum / time.Duration(len(gaps))
	return fmt.Sprintf(" (interval min %.2fs avg %.2fs max %.2fs)", min.Seconds(), avg.Seconds(), max.Seconds())
}
