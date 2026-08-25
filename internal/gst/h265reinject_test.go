// Gate A tests for the parameter-set re-injection in h265reinject.go.
//
// Untagged, like the file they test, so the byte-stream surgery that decides
// which HEVC slices get their VPS/SPS/PPS spliced back in is exercised on every
// build and under -race, not only on the one field machine that reproduces the
// fault. No GStreamer, decoder or rig is involved: the input is hand-built
// Annex-B and every expectation is spelled out byte for byte.
//
// The NAL headers below are the real two-byte HEVC headers. First byte =
// (forbidden_zero_bit<<7) | (nal_unit_type<<1) | (nuh_layer_id high bit); with
// layer 0 that is simply nal_unit_type<<1. So SPS(33)=0x42, PPS(34)=0x44,
// VPS(32)=0x40, PREFIX_SEI(39)=0x4E, and a TRAIL_R slice(1)=0x02. The third NAL
// byte carries first_slice_segment_in_pic_flag in its top bit: 0x80 means "new
// picture", 0x00 means a dependent/continuation slice.

package gst

import (
	"bytes"
	"testing"
)

// Annex-B building blocks. sc3/sc4 are the two start-code lengths; the NAL
// bodies are deliberately padded past three bytes so isFirstSlice always has a
// third byte to read.
var (
	sc3 = []byte{0x00, 0x00, 0x01}
	sc4 = []byte{0x00, 0x00, 0x00, 0x01}
)

// nal builds one Annex-B NAL: start code, the two header bytes for typ at layer
// 0, then body. body[0] is the first RBSP byte, whose top bit is
// first_slice_segment_in_pic_flag for a slice.
func nal(sc []byte, typ uint8, body ...byte) []byte {
	out := append([]byte{}, sc...)
	out = append(out, typ<<1, 0x01) // second header byte: temporal_id_plus1 = 1
	return append(out, body...)
}

func vps(sc []byte) []byte { return nal(sc, h265NalVPS, 0x0C, 0x01, 0xFF) }
func sps(sc []byte) []byte { return nal(sc, h265NalSPS, 0x01, 0x02, 0x03) }
func pps(sc []byte) []byte { return nal(sc, h265NalPPS, 0xC1, 0x02) }
func sei(sc []byte) []byte { return nal(sc, 39, 0x4E, 0x08, 0x00) }

// firstSlice / laterSlice build TRAIL_R (type 1) VCL NALs; the third NAL byte
// (body[0]) sets or clears first_slice_segment_in_pic_flag.
func firstSlice(sc []byte, payload ...byte) []byte {
	return nal(sc, 1, append([]byte{0x80}, payload...)...)
}
func laterSlice(sc []byte, payload ...byte) []byte {
	return nal(sc, 1, append([]byte{0x00}, payload...)...)
}

func cat(chunks ...[]byte) []byte { return bytes.Join(chunks, nil) }

// TestRewriteTable is the whole contract, one row per scenario the design has to
// get right. Each row primes a FRESH cache with a sequence of buffers (so
// cross-buffer state — "params seen at the IDR, gone by the P-frame" — is under
// test too), then asserts the rewrite of the final buffer.
func TestRewriteTable(t *testing.T) {
	// A canonical set of parameters used across rows, always with 3-byte start
	// codes so the expected output is easy to state.
	V, S, P := vps(sc3), sps(sc3), pps(sc3)
	block := cat(V, S, P) // what an injection splices in, in this exact order

	tests := []struct {
		name string
		// feed is the buffers seen BEFORE the one under test; they prime the
		// cache but their output is not asserted.
		feed [][]byte
		in   []byte
		want []byte // expected rewrite of in
		// changed is the expected second return value for `in`.
		changed bool
	}{
		{
			// The field case. Params arrive only at the IDR; the P-frames that
			// follow, in later buffers, carry none — and are exactly the slices
			// h265parse was dropping. Each must come out with the sets ahead of it.
			name:    "params at IDR then a bare P-frame",
			feed:    [][]byte{cat(V, S, P, firstSlice(sc3, 0x11))},
			in:      firstSlice(sc3, 0x22),
			want:    cat(block, firstSlice(sc3, 0x22)),
			changed: true,
		},
		{
			// Nothing has ever supplied parameters, so there is nothing to inject
			// and the slice must pass through untouched. This is the guard against
			// corrupting a stream we joined mid-GOP before the first IDR.
			name:    "params never seen: pass through",
			feed:    nil,
			in:      firstSlice(sc3, 0x22),
			want:    firstSlice(sc3, 0x22),
			changed: false,
		},
		{
			// The IDR buffer itself already carries the sets directly before the
			// slice: refresh the cache, inject NOTHING, change nothing. A second
			// copy of VPS/SPS/PPS here would be the corruption this row exists to
			// forbid.
			name:    "IDR already preceded by SPS/PPS: no double-inject",
			feed:    nil,
			in:      cat(V, S, P, firstSlice(sc3, 0x11)),
			want:    cat(V, S, P, firstSlice(sc3, 0x11)),
			changed: false,
		},
		{
			// Two bare pictures in ONE buffer: each new-picture slice is its own
			// splice point, so both get the sets.
			name:    "multi-NAL buffer, two bare pictures",
			feed:    [][]byte{cat(V, S, P, firstSlice(sc3, 0x11))},
			in:      cat(firstSlice(sc3, 0x22), firstSlice(sc3, 0x33)),
			want:    cat(block, firstSlice(sc3, 0x22), block, firstSlice(sc3, 0x33)),
			changed: true,
		},
		{
			// A single-NAL buffer holding just the bare slice — the common
			// packetisation on this path — gets exactly one block.
			name:    "single-NAL bare slice",
			feed:    [][]byte{cat(V, S, P, firstSlice(sc3, 0x11))},
			in:      firstSlice(sc3, 0x44),
			want:    cat(block, firstSlice(sc3, 0x44)),
			changed: true,
		},
		{
			// Parameters primed with 4-byte start codes, slice arriving with a
			// 4-byte start code: caching keys off nal_unit_type, not start-code
			// length, and the injected block is whatever was cached (here 4-byte).
			name:    "4-byte start codes throughout",
			feed:    [][]byte{cat(vps(sc4), sps(sc4), pps(sc4), firstSlice(sc4, 0x11))},
			in:      firstSlice(sc4, 0x22),
			want:    cat(vps(sc4), sps(sc4), pps(sc4), firstSlice(sc4, 0x22)),
			changed: true,
		},
		{
			// SEI sits between the (present) parameters and the slice. sawParams is
			// set by the parameter NALs and a prefix SEI does not clear it, so the
			// slice is recognised as already carrying its sets: no injection.
			name:    "SEI after params before slice: no inject",
			feed:    nil,
			in:      cat(V, S, P, sei(sc3), firstSlice(sc3, 0x11)),
			want:    cat(V, S, P, sei(sc3), firstSlice(sc3, 0x11)),
			changed: false,
		},
		{
			// SEI before a BARE slice, no parameters in this buffer. The block goes
			// immediately before the slice's start code — after the SEI — which is
			// where the parser needs it; a prefix SEI ahead of the sets is legal.
			name:    "SEI before a bare slice: inject before the slice",
			feed:    [][]byte{cat(V, S, P, firstSlice(sc3, 0x11))},
			in:      cat(sei(sc3), firstSlice(sc3, 0x22)),
			want:    cat(sei(sc3), block, firstSlice(sc3, 0x22)),
			changed: true,
		},
		{
			// A dependent slice (first_slice_segment_in_pic_flag = 0) in the middle
			// of a picture is NOT a new-picture boundary and must never be a splice
			// point, even with parameters cached and absent from this buffer.
			name:    "mid-picture dependent slice: no inject",
			feed:    [][]byte{cat(V, S, P, firstSlice(sc3, 0x11))},
			in:      laterSlice(sc3, 0x22),
			want:    laterSlice(sc3, 0x22),
			changed: false,
		},
		{
			// First slice of a multi-slice picture gets the block; the dependent
			// slices that complete the same picture do not.
			name:    "multi-slice picture: only the first slice",
			feed:    [][]byte{cat(V, S, P, firstSlice(sc3, 0x11))},
			in:      cat(firstSlice(sc3, 0x22), laterSlice(sc3, 0x23), laterSlice(sc3, 0x24)),
			want:    cat(block, firstSlice(sc3, 0x22), laterSlice(sc3, 0x23), laterSlice(sc3, 0x24)),
			changed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c h265ParamCache
			for _, f := range tt.feed {
				c.rewrite(f) // prime the cache; output not asserted
			}
			got, changed := c.rewrite(tt.in)
			if changed != tt.changed {
				t.Errorf("changed = %v, want %v", changed, tt.changed)
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("rewrite mismatch\n got: % x\nwant: % x", got, tt.want)
			}
		})
	}
}

// TestRewriteIdempotent asserts the property the whole path leans on: rewriting
// rewrite's own output changes nothing. Without it, a buffer that was already
// fixed upstream, or a second probe firing, would keep growing the stream.
func TestRewriteIdempotent(t *testing.T) {
	var c h265ParamCache
	// Prime the cache, then rewrite a bare two-picture buffer.
	c.rewrite(cat(vps(sc3), sps(sc3), pps(sc3), firstSlice(sc3, 0x11)))
	once, changed := c.rewrite(cat(firstSlice(sc3, 0x22), firstSlice(sc3, 0x33)))
	if !changed {
		t.Fatal("first rewrite should have injected")
	}
	twice, changedAgain := c.rewrite(once)
	if changedAgain {
		t.Error("second rewrite changed an already-rewritten buffer")
	}
	if !bytes.Equal(once, twice) {
		t.Errorf("not idempotent\n once: % x\ntwice: % x", once, twice)
	}
}

// TestCacheCopiesInput guards the one memory rule that a test with its own
// backing arrays would otherwise never catch: the cache must COPY parameter
// bytes, because in production the input is GStreamer memory unmapped the instant
// the probe returns. Mutating the input buffer after caching must not disturb a
// later injection.
func TestCacheCopiesInput(t *testing.T) {
	var c h265ParamCache
	idr := cat(vps(sc3), sps(sc3), pps(sc3), firstSlice(sc3, 0x11))
	feed := append([]byte(nil), idr...)
	c.rewrite(feed)

	// Scribble over the buffer the cache read from, as unmapping-and-reusing
	// would.
	for i := range feed {
		feed[i] = 0xEE
	}

	want := cat(vps(sc3), sps(sc3), pps(sc3), firstSlice(sc3, 0x22))
	got, changed := c.rewrite(firstSlice(sc3, 0x22))
	if !changed || !bytes.Equal(got, want) {
		t.Errorf("cache aliased its input\n got: % x\nwant: % x", got, want)
	}
}
