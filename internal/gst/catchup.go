package gst

// catchup.go keeps the picture CURRENT: when the decoder falls behind the
// stream, access units are dropped up to the next keyframe rather than queued.
//
// # Why
//
// The picture sink runs sync=false, so a decoded frame is shown the moment it
// exists and the display never holds anything back. What CAN hold things back
// is the decoder itself: software HEVC at 1080p50 on a laptop that is also
// rendering a WebView, decoding the mosaic and being screen-shared has, at
// times, less than a frame's worth of CPU per frame. Every frame it is late
// by then waits in the queue in front of it, and the queue in front of it
// (picq, a plain GstQueue) held up to 200 access units — four seconds of
// picture — before it would refuse any more, at which point the demuxer, then
// srtsrc, then libsrt's receive buffer filled in turn. The commentator saw a
// picture drifting steadily behind the match, seconds behind after minutes,
// and nothing ever caught up. The operator's instruction: "We cannot fall
// behind at all, I would rather drop frames."
//
// # How
//
// A probe on picq's src pad — the pad every access unit leaves the queue by
// on its way to h265parse — reads the queue's fill (current-level-buffers,
// one access unit per buffer as tsdemux emits them) on every buffer:
//
//   - below the high-water mark nothing happens and nothing is even mapped;
//   - at or above it, dropping begins: every access unit is discarded (the
//     queue drains in milliseconds, since dropping costs nothing) UNTIL a
//     keyframe arrives with the fill at or below the low-water mark. Decoding
//     resumes AT that keyframe, which needs no reference the decoder does not
//     have, so the picture simply jumps forward with no garbage. The first
//     buffer through is flagged DISCONT, which is the truth.
//
// The stream's keyframes are 0.2 s apart (an IDR every ten frames, measured),
// so a catch-up costs at most the backlog plus 0.2 s of picture, and a machine
// that keeps up pays nothing: the fill never reaches the mark.
//
// Thresholds: 15 access units high (0.3 s at 50 fps — a laptop that is that
// far behind is not going to recover by itself), 3 low.

// catchUpHighWater and catchUpLowWater are the queue fills, in access units,
// at which dropping starts and at which a keyframe ends it.
const (
	catchUpHighWater = 15
	catchUpLowWater  = 3
)

// h265NalIRAPMin and h265NalIRAPMax bound the intra random access point NAL
// types: BLA_W_LP (16) .. CRA_NUT (21), IDR_W_RADL (19) and IDR_N_LP (20)
// among them. A picture beginning with one needs no earlier picture.
const (
	h265NalIRAPMin = 16
	h265NalIRAPMax = 21
)

// hasIRAP reports whether an access unit (Annex B byte stream) contains an
// IRAP slice: a keyframe the decoder can start from.
func hasIRAP(b []byte) bool {
	for _, m := range scanNALs(b) {
		if m.typ >= h265NalIRAPMin && m.typ <= h265NalIRAPMax {
			return true
		}
	}
	return false
}

// catchUp is the decision. It is driven from one streaming thread and read
// (Dropped, Episodes) from the stats logger, hence the plain fields plus a
// mutex-free design: the counters are only ever written by decide.
type catchUp struct {
	high, low int

	dropping bool
	// episodeDropped counts the current episode's discards, for the log line
	// when it ends.
	episodeDropped uint64

	// Totals, for the stats line. Written by decide only.
	dropped  uint64
	episodes uint64
}

func newCatchUp(high, low int) *catchUp {
	if high < 1 {
		high = catchUpHighWater
	}
	if low <= 0 || low >= high {
		low = catchUpLowWater
		if low >= high {
			low = high - 1
		}
	}
	return &catchUp{high: high, low: low}
}

// catchUpVerdict is what decide says about one access unit.
type catchUpVerdict struct {
	// Drop: discard this access unit.
	Drop bool
	// Resumed: this access unit is the keyframe decoding resumes at, after
	// an episode that discarded Skipped access units; flag it DISCONT.
	Resumed bool
	Skipped uint64
	// Began: this access unit is the first of an episode (it is dropped).
	Began bool
	// Backlog is the fill decide was given, for the log.
	Backlog int
}

// decide takes the queue's fill AFTER this access unit left it and whether the
// access unit begins with a keyframe (only consulted while dropping, so the
// caller need not scan otherwise — see irapKnown), and says what to do with it.
func (c *catchUp) decide(backlog int, irap func() bool) catchUpVerdict {
	v := catchUpVerdict{Backlog: backlog}
	if !c.dropping {
		if backlog < c.high {
			return v
		}
		c.dropping = true
		c.episodes++
		c.episodeDropped = 0
		v.Began = true
	}
	if backlog <= c.low && irap() {
		c.dropping = false
		v.Resumed = true
		v.Skipped = c.episodeDropped
		return v
	}
	c.dropping = true
	c.episodeDropped++
	c.dropped++
	v.Drop = true
	return v
}

// Totals returns what has been dropped so far and in how many episodes.
func (c *catchUp) Totals() (dropped, episodes uint64) {
	return c.dropped, c.episodes
}
