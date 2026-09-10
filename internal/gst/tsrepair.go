package gst

// tsrepair.go repairs one non-compliant transport packet the M2L-X muxer emits,
// before tsdemux sees it. It is the fix for the persistent software-HEVC
// picture tear, and every claim in this file was measured on 2026-09-10 on
// COMM-01's own bytes (docs/picture-diagnostics-runbook.md section 0; the rig).
//
// # What the muxer sends
//
// Forty times a minute on the video PID — at the END of a PES packet, in the
// packet immediately before the next payload_unit_start — the muxer emits a
// packet whose adaptation_field_control says "adaptation field AND payload"
// (0b11) with adaptation_field_length 183, which fills the packet and leaves
// ZERO payload bytes. ISO/IEC 13818-1 2.4.3.4 permits length 183 only when the
// packet carries NO payload (0b10); with 0b11 the length must be 0..182 so that
// at least one payload byte exists. The muxer nevertheless counts the packet in
// the continuity counter, as a payload packet.
//
// # What tsdemux does with it
//
// mpegtspacketizer sets packet->payload to the byte after the adaptation field
// — the end of the packet — and tsdemux's push path takes a zero-length payload
// as nothing to handle, so the packet is skipped WITHOUT the stream's continuity
// counter advancing. The next packet, the payload_unit_start of the following
// picture, then reads as a skip of one ("CONTINUITY: Mismatch packet 11, stream
// 9"), and tsdemux's reaction (tsdemux.c, gst_ts_demux_handle_packet) is:
// free the PES it was collecting — the completed previous picture, waiting for
// exactly this PUSI to push it — and NOT process this packet, leaving the
// stream "waiting for packet start" until the PUSI after that. Two pictures
// lost per event. Every picture until the next IDR then references a missing
// one — libav's "Could not find ref with POC" — and decodes as grey/green
// garbage. That is the tear: on and off, forty times a minute, on every
// machine, with SRT reporting zero loss, because nothing was lost.
//
// Measured on COMM-01's 59 s capture: 40 such packets; tsdemux 40 mismatches on
// the video PID; libav 252 reference errors; 2820 of 2955 pictures decoded.
// With the 40 packets repaired: 0 mismatches, 0 reference errors, 2896 decoded
// (the remainder is the join, before the first parameter sets).
//
// # The repair
//
// Make the packet what it claims to be: adaptation_field_length 182 and a
// single payload byte of 0x00. The counter the muxer already advanced now
// matches a packet the demuxer counts, and the extra byte is appended to the
// end of the PES payload — in an Annex B byte stream a trailing zero byte is
// trailing_zero_8bits, which h265parse's start-code scan steps over. It is
// stateless, in place, and touches nothing else.
//
// The only place it has been seen is the video PID. An AAC (ADTS) PID has never
// carried one in any capture; if it did, a 0x00 between ADTS frames would cost
// aacparse one resync. The picture path discards the audio PID anyway.

const tsRepairPacketSize = 188

// RepairZeroPayloadPackets rewrites, in place, every whole 188-byte transport
// packet in b that claims a payload but has none (adaptation_field_control 0b11
// with adaptation_field_length 183), giving it a one-byte 0x00 payload. It
// returns how many packets it changed.
//
// Packets are taken on a 188-byte grid from the first sync byte: offset 0 when
// the buffer starts on one, as srtsrc's 7-packet buffers and an aligned
// filesrc block do; otherwise the first offset under 188 at which three
// consecutive grid positions carry 0x47 (or as many as the buffer holds), so
// a buffer that begins mid-packet is repaired from its first whole packet.
// A step that does not begin with 0x47 and a trailing partial packet are left
// alone.
func RepairZeroPayloadPackets(b []byte) int {
	n := 0
	for off := tsRepairGridStart(b); off+tsRepairPacketSize <= len(b); off += tsRepairPacketSize {
		p := b[off : off+tsRepairPacketSize]
		if p[0] != 0x47 {
			continue
		}
		afc := (p[3] >> 4) & 0x03
		if afc != 0x03 || p[4] != 183 {
			continue
		}
		p[4] = 182
		p[tsRepairPacketSize-1] = 0x00
		n++
	}
	return n
}

// tsRepairGridStart is the offset of the packet grid in b: 0 when b starts on
// a sync byte, else the first offset under 188 whose next three grid positions
// (as far as b reaches) are all sync bytes, else 0.
func tsRepairGridStart(b []byte) int {
	if len(b) == 0 || b[0] == 0x47 {
		return 0
	}
	for k := 1; k < tsRepairPacketSize && k < len(b); k++ {
		ok := true
		for j := 0; j < 3; j++ {
			at := k + j*tsRepairPacketSize
			if at >= len(b) {
				break
			}
			if b[at] != 0x47 {
				ok = false
				break
			}
		}
		if ok {
			return k
		}
	}
	return 0
}
