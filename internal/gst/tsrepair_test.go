package gst

import (
	"bytes"
	"testing"
)

// tsPacket builds one transport packet. afc is adaptation_field_control; when
// it has the adaptation bit, aflen is the adaptation_field_length and the field
// is stuffing (0xff); whatever room is left is payload filled with fill.
func tsPacket(pid int, pusi bool, afc byte, cc byte, aflen int, fill byte) []byte {
	p := make([]byte, 188)
	p[0] = 0x47
	p[1] = byte(pid >> 8 & 0x1f)
	if pusi {
		p[1] |= 0x40
	}
	p[2] = byte(pid)
	p[3] = afc<<4 | cc&0x0f
	i := 4
	if afc&0x02 != 0 {
		p[4] = byte(aflen)
		for j := 0; j < aflen; j++ {
			p[5+j] = 0xff
		}
		if aflen > 0 {
			p[5] = 0x00 // adaptation flags: none
		}
		i = 5 + aflen
	}
	for ; i < 188; i++ {
		p[i] = fill
	}
	return p
}

func TestRepairZeroPayloadPacketsRewritesExactlyTheMuxersPacket(t *testing.T) {
	// The shape measured on COMM-01: data packet (cc 9), the zero-payload
	// packet claiming a payload (cc 10, afc 0b11, length 183), then the next
	// picture's PUSI (cc 11). Only the middle one changes, and only two bytes.
	before := tsPacket(0x41, false, 0x01, 9, 0, 0xaa)
	broken := tsPacket(0x41, false, 0x03, 10, 183, 0xbb)
	after := tsPacket(0x41, true, 0x01, 11, 0, 0xcc)
	buf := bytes.Join([][]byte{before, broken, after}, nil)

	if n := RepairZeroPayloadPackets(buf); n != 1 {
		t.Fatalf("repaired %d packets, want 1", n)
	}
	if !bytes.Equal(buf[:188], before) || !bytes.Equal(buf[376:], after) {
		t.Fatal("a packet other than the zero-payload one was changed")
	}
	got := buf[188:376]
	if got[4] != 182 {
		t.Fatalf("adaptation_field_length = %d, want 182", got[4])
	}
	if got[187] != 0x00 {
		t.Fatalf("the one payload byte = %#x, want 0x00 (trailing_zero_8bits)", got[187])
	}
	if got[3] != broken[3] {
		t.Fatalf("header byte 3 changed: %#x -> %#x; the continuity counter and afc must stay", broken[3], got[3])
	}
	want := append([]byte(nil), broken...)
	want[4], want[187] = 182, 0x00
	if !bytes.Equal(got, want) {
		t.Fatal("bytes other than the length and the payload byte changed")
	}
	// Idempotent: a repaired packet is compliant and is not touched again.
	if n := RepairZeroPayloadPackets(buf); n != 0 {
		t.Fatalf("second pass repaired %d, want 0", n)
	}
}

func TestRepairZeroPayloadPacketsLeavesCompliantPacketsAlone(t *testing.T) {
	cases := map[string][]byte{
		"payload only":                         tsPacket(0x41, false, 0x01, 3, 0, 0x11),
		"adaptation only, full (legal 183)":    tsPacket(0x41, false, 0x02, 3, 183, 0),
		"adaptation only, short":               tsPacket(0x41, false, 0x02, 3, 7, 0),
		"adaptation + one payload byte (182)":  tsPacket(0x41, false, 0x03, 3, 182, 0x22),
		"adaptation + payload, ordinary":       tsPacket(0x41, true, 0x03, 3, 7, 0x33),
		"null packet":                          tsPacket(0x1fff, false, 0x01, 0, 0, 0xff),
		"audio PID with the muxer shape":       tsPacket(0x42, false, 0x03, 5, 183, 0), // is repaired: see below
		"reserved afc 0b00 (would be dropped)": tsPacket(0x41, false, 0x00, 3, 0, 0),
	}
	for name, p := range cases {
		orig := append([]byte(nil), p...)
		n := RepairZeroPayloadPackets(p)
		switch name {
		case "audio PID with the muxer shape":
			// The repair is by shape, not by PID: it has no PMT. Documented in
			// tsrepair.go; never observed on the audio PID.
			if n != 1 {
				t.Errorf("%s: repaired %d, want 1 (the repair is PID-agnostic)", name, n)
			}
		default:
			if n != 0 || !bytes.Equal(p, orig) {
				t.Errorf("%s: repaired %d and/or bytes changed; must be left alone", name, n)
			}
		}
	}
}

func TestRepairZeroPayloadPacketsSkipsWhatIsNotAWholeAlignedPacket(t *testing.T) {
	broken := tsPacket(0x41, false, 0x03, 10, 183, 0)

	// A trailing partial packet is never touched, even if it looks like one.
	partial := append(append([]byte(nil), tsPacket(0x41, false, 0x01, 1, 0, 0)...), broken[:100]...)
	orig := append([]byte(nil), partial...)
	if n := RepairZeroPayloadPackets(partial); n != 0 || !bytes.Equal(partial, orig) {
		t.Fatalf("a partial packet was repaired (%d)", n)
	}

	// A step of zeros at the front (no sync byte anywhere under 188) leaves the
	// grid at 0: the packet after it is still examined on the 188 grid.
	junk := make([]byte, 188)
	buf := bytes.Join([][]byte{junk, broken}, nil)
	if n := RepairZeroPayloadPackets(buf); n != 1 {
		t.Fatalf("repaired %d, want 1 (the aligned packet after the junk)", n)
	}
	if !bytes.Equal(buf[:188], junk) {
		t.Fatal("the junk step was modified")
	}

	// Empty and short inputs.
	if n := RepairZeroPayloadPackets(nil); n != 0 {
		t.Fatalf("nil: %d", n)
	}
	if n := RepairZeroPayloadPackets(broken[:187]); n != 0 {
		t.Fatalf("187 bytes: %d", n)
	}
}

func TestRepairZeroPayloadPacketsAcrossAnSRTBuffer(t *testing.T) {
	// srtsrc delivers 7 packets per buffer. Two of the muxer's packets in one
	// buffer, at either end, are both repaired and counted.
	pk := [][]byte{
		tsPacket(0x41, false, 0x03, 0, 183, 0),
		tsPacket(0x41, true, 0x01, 1, 0, 1),
		tsPacket(0x41, false, 0x01, 2, 0, 2),
		tsPacket(0x42, false, 0x01, 7, 0, 3),
		tsPacket(0x41, false, 0x01, 3, 0, 4),
		tsPacket(0x1fff, false, 0x01, 0, 0, 0xff),
		tsPacket(0x41, false, 0x03, 4, 183, 0),
	}
	buf := bytes.Join(pk, nil)
	if len(buf) != 1316 {
		t.Fatalf("buffer is %d bytes, want 1316", len(buf))
	}
	if n := RepairZeroPayloadPackets(buf); n != 2 {
		t.Fatalf("repaired %d, want 2", n)
	}
	for _, off := range []int{0, 6 * 188} {
		if buf[off+4] != 182 || buf[off+187] != 0 {
			t.Errorf("packet at %d not repaired", off)
		}
	}
	for i := 1; i <= 5; i++ {
		if !bytes.Equal(buf[i*188:(i+1)*188], pk[i]) {
			t.Errorf("packet %d changed", i)
		}
	}
}

func TestRepairZeroPayloadPacketsFindsTheGridInAShiftedBuffer(t *testing.T) {
	// A buffer that begins mid-packet — the tail of one packet, then whole
	// ones. The grid is found from the sync bytes and the muxer's packet on it
	// is repaired; the tail is left alone.
	tail := tsPacket(0x41, false, 0x01, 8, 0, 0x55)[100:]
	broken := tsPacket(0x41, false, 0x03, 10, 183, 0)
	buf := bytes.Join([][]byte{tail, tsPacket(0x41, false, 0x01, 9, 0, 1), broken, tsPacket(0x41, true, 0x01, 11, 0, 2)}, nil)
	if got := tsRepairGridStart(buf); got != len(tail) {
		t.Fatalf("grid start = %d, want %d", got, len(tail))
	}
	if n := RepairZeroPayloadPackets(buf); n != 1 {
		t.Fatalf("repaired %d, want 1", n)
	}
	at := len(tail) + 188
	if buf[at+4] != 182 || buf[at+187] != 0 {
		t.Fatal("the packet on the shifted grid was not repaired")
	}
	if !bytes.Equal(buf[:len(tail)], tsPacket(0x41, false, 0x01, 8, 0, 0x55)[100:]) {
		t.Fatal("the leading partial packet was modified")
	}
}
