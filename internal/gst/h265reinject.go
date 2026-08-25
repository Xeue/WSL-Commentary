// h265reinject.go re-injects HEVC parameter sets so h265parse always holds a
// valid SPS+PPS by the time a slice reaches it.
//
// See picture_cgo.go for WHY. On a machine that loses the active parameter sets
// between an IDR and the P-frames that follow — the field symptom on COMM-01 —
// h265parse drops every slice whose picture headers it cannot resolve
// ("broken/invalid nal ... will be dropped") and the decoder tears for a GOP
// until the next IDR. gst_h265_parse_process_nal returns FALSE for a slice
// whenever !GST_H265_PARSE_STATE_VALID(...VALID_PICTURE_HEADERS), i.e. whenever
// the parser is not currently holding an SPS+PPS. The cure is to make the
// parameter sets travel with every picture: cache the in-band VPS/SPS/PPS the
// first time they appear and splice them back in ahead of every new-picture
// slice that does not already carry them.
//
// THIS FILE IS PURE GO ON PURPOSE. It is the whole of the logic, and it is
// exercised by h265reinject_test.go on every build and under -race. The cgo in
// picture_cgo.go does nothing but map a buffer to a []byte, call rewrite, and
// hand the result back to the pad; all the byte-twiddling that decides which
// slices get parameters is here, testable without a GStreamer, a decoder or a
// rig. Nothing in this file imports C.
package gst

// HEVC nal_unit_type values this path acts on. Everything else — SEI (39/40),
// AUD (35), EOS/EOB, and the VCL slice types themselves — passes through
// untouched; only the parameter sets are cached and only new-picture slices are
// a splice point.
const (
	h265NalVPS = 32 // video parameter set
	h265NalSPS = 33 // sequence parameter set
	h265NalPPS = 34 // picture parameter set

	// nal_unit_type 0..31 are VCL (slice) NAL units. A VCL NAL begins a new
	// picture when first_slice_segment_in_pic_flag is set; that is the one bit
	// this file reads out of a slice.
	h265NalVCLMax = 31
)

// h265ParamCache holds the most recent VPS/SPS/PPS seen on the wire, each as its
// raw Annex-B bytes INCLUDING a leading start code, ready to splice back in
// verbatim.
//
// The bytes are always COPIED in, never aliased into the input buffer: the input
// is GStreamer memory mapped only for the duration of one probe callback, and a
// cached slice that pointed into it would dangle the moment the buffer is
// unmapped.
//
// It is confined to ONE streaming thread — the queue src pad's — for its whole
// life: the probe that owns it runs only there, and teardown removes that probe
// (which blocks for any in-flight callback) before the cache is dropped. So it
// carries no lock. If it is ever shared across pads or goroutines, that
// confinement is the assumption to revisit.
type h265ParamCache struct {
	vps []byte
	sps []byte
	pps []byte
}

// haveParams reports whether the load-bearing pair is cached. VPS is NOT
// required for h265parse to hold valid picture headers — SPS and PPS are, and
// GST_H265_PARSE_STATE_VALID_PICTURE_HEADERS is exactly "has an active SPS+PPS"
// — so injection is gated on those two, and VPS rides along only when we have it.
func (c *h265ParamCache) haveParams() bool {
	return c.sps != nil && c.pps != nil
}

// nalMark is one Annex-B NAL unit located in a byte-stream buffer.
type nalMark struct {
	sc  int   // index of the first byte of the start code (the 00 of 00 00 01)
	hdr int   // index of the NAL header's first byte, just past the start code
	typ uint8 // nal_unit_type = (hdr byte >> 1) & 0x3F
}

// scanNALs locates every NAL unit in an Annex-B buffer.
//
// A start code is the literal sequence 00 00 01. A four-byte 00 00 00 01 is seen
// as a three-byte start code with a leading zero left attached to whatever
// precedes it — which is harmless here because rewrite only ever SPLICES around
// whole start codes and never rewrites the bytes between them, so an extra
// leading zero simply becomes a legal leading_zero_8bits ahead of the next start
// code. Emulation prevention (00 00 03) can never spell 00 00 01, so this literal
// scan is exact rather than a heuristic.
func scanNALs(b []byte) []nalMark {
	var marks []nalMark
	n := len(b)
	for i := 0; i+2 < n; {
		if b[i] == 0x00 && b[i+1] == 0x00 && b[i+2] == 0x01 {
			hdr := i + 3
			if hdr < n {
				marks = append(marks, nalMark{sc: i, hdr: hdr, typ: (b[hdr] >> 1) & 0x3F})
			}
			i = hdr
			continue
		}
		i++
	}
	return marks
}

// isFirstSlice reports first_slice_segment_in_pic_flag for the slice whose NAL
// header begins at hdr. That flag is the very first bit of the slice segment
// header, which is the top bit of the first RBSP byte — two bytes past the NAL
// header. Emulation prevention cannot touch a buffer's third NAL byte (it needs
// two preceding zero bytes), so the bit is read straight.
func isFirstSlice(b []byte, hdr int) bool {
	third := hdr + 2
	return third < len(b) && b[third]&0x80 != 0
}

// paramBlock concatenates the cached VPS+SPS+PPS in that order, each carrying its
// own start code. VPS is included only when cached; SPS and PPS are guaranteed by
// the haveParams gate the one caller applies first.
func (c *h265ParamCache) paramBlock() []byte {
	block := make([]byte, 0, len(c.vps)+len(c.sps)+len(c.pps))
	block = append(block, c.vps...)
	block = append(block, c.sps...)
	block = append(block, c.pps...)
	return block
}

// rewrite returns b with cached VPS+SPS+PPS spliced in ahead of every
// new-picture slice that is not already directly preceded by parameter sets,
// after refreshing the cache from any parameter sets found in b. It also reports
// whether it changed anything, so the caller can pass an untouched buffer
// straight through rather than copy it into a fresh GstBuffer.
//
// It is a PURE SPLICE: the returned slice is b with parameter blocks inserted at
// chosen offsets and not one byte of b altered or dropped. Two consequences the
// caller relies on:
//
//   - changed==false means the bytes are exactly b, so the original buffer can
//     flow on as-is.
//   - Running rewrite on its own output is a no-op: the parameters it injected
//     are seen as in-band parameter sets on the next pass and suppress a second
//     injection. So it is safe to leave in the path unconditionally and safe
//     against a buffer that was already rewritten upstream.
//
// The scan is single-pass and order-preserving: the cache is refreshed as each
// parameter NAL is passed, so a slice is measured against the parameters that
// actually precede it, and sawParams — set by a parameter NAL, cleared once a new
// picture has been passed — is what distinguishes a slice that already carries
// its sets from one that needs them.
func (c *h265ParamCache) rewrite(b []byte) (out []byte, changed bool) {
	marks := scanNALs(b)
	if len(marks) == 0 {
		return b, false
	}

	var insertAt []int // byte offsets in b at which to splice a parameter block
	sawParams := false // parameter sets already seen ahead of the next new picture
	for i, m := range marks {
		switch {
		case m.typ == h265NalVPS, m.typ == h265NalSPS, m.typ == h265NalPPS:
			// A parameter NAL runs from its start code to the next NAL's start
			// code, or to the end of the buffer. Copy it — b is transient — so the
			// cache outlives this buffer's mapping.
			end := len(b)
			if i+1 < len(marks) {
				end = marks[i+1].sc
			}
			raw := append([]byte(nil), b[m.sc:end]...)
			switch m.typ {
			case h265NalVPS:
				c.vps = raw
			case h265NalSPS:
				c.sps = raw
			case h265NalPPS:
				c.pps = raw
			}
			sawParams = true

		case m.typ <= h265NalVCLMax && isFirstSlice(b, m.hdr):
			// A new-picture boundary. If the parameter sets are not already ahead
			// of it and we have some cached, this is where they go — immediately
			// before the slice's start code.
			if c.haveParams() && !sawParams {
				insertAt = append(insertAt, m.sc)
			}
			sawParams = false // the next picture starts without them until proven otherwise
		}
	}

	if len(insertAt) == 0 {
		return b, false
	}

	block := c.paramBlock()
	out = make([]byte, 0, len(b)+len(insertAt)*len(block))
	prev := 0
	for _, at := range insertAt {
		out = append(out, b[prev:at]...)
		out = append(out, block...)
		prev = at
	}
	out = append(out, b[prev:]...)
	return out, true
}
