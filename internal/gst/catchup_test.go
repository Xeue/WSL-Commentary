package gst

import "testing"

// au builds an Annex B access unit of the given NAL types, each with a
// two-byte header and a little payload.
func au(types ...uint8) []byte {
	var b []byte
	for _, t := range types {
		b = append(b, 0x00, 0x00, 0x01, t<<1, 0x01, 0xaa, 0xbb, 0xcc)
	}
	return b
}

func TestHasIRAPRecognisesEveryRandomAccessPointAndNothingElse(t *testing.T) {
	for _, typ := range []uint8{16, 17, 18, 19, 20, 21} {
		if !hasIRAP(au(35, 39, typ)) {
			t.Errorf("NAL type %d is an IRAP and must be found behind an AUD and an SEI", typ)
		}
	}
	for _, typ := range []uint8{0, 1, 2, 9, 15, 22, 32, 33, 34, 35, 39} {
		if hasIRAP(au(typ)) {
			t.Errorf("NAL type %d is not an IRAP", typ)
		}
	}
	if hasIRAP(nil) || hasIRAP([]byte{0, 0, 1}) {
		t.Error("nothing in nothing")
	}
}

func TestCatchUpDoesNothingWhileTheDecoderKeepsUp(t *testing.T) {
	c := newCatchUp(15, 3)
	scanned := 0
	irap := func() bool { scanned++; return true }
	for i := 0; i < 1000; i++ {
		v := c.decide(i%14, irap)
		if v.Drop || v.Began || v.Resumed {
			t.Fatalf("access unit %d at fill %d: %+v; nothing may happen below the high-water mark", i, i%14, v)
		}
	}
	if scanned != 0 {
		t.Fatalf("the keyframe question was asked %d times while not dropping; it costs a scan and must not be", scanned)
	}
	if d, e := c.Totals(); d != 0 || e != 0 {
		t.Fatalf("totals %d/%d, want 0/0", d, e)
	}
}

func TestCatchUpDropsToTheNextKeyframeOnceTheFillIsLow(t *testing.T) {
	c := newCatchUp(15, 3)

	// The queue has filled to the mark: this access unit begins an episode
	// and is dropped, keyframe or not.
	v := c.decide(15, func() bool { return true })
	if !v.Began || !v.Drop || v.Resumed {
		t.Fatalf("at the high-water mark: %+v, want Began and Drop", v)
	}

	// The queue drains as we drop. A keyframe while the fill is still high is
	// NOT the resume point: resuming there would leave the backlog in place.
	v = c.decide(9, func() bool { return true })
	if !v.Drop || v.Resumed {
		t.Fatalf("keyframe at fill 9 (low is 3): %+v, want Drop", v)
	}
	// Low enough, but not a keyframe: keep dropping.
	for i := 0; i < 5; i++ {
		v = c.decide(2, func() bool { return false })
		if !v.Drop || v.Resumed {
			t.Fatalf("non-keyframe at fill 2: %+v, want Drop", v)
		}
	}
	// Low and a keyframe: resume HERE, and say how many were skipped.
	v = c.decide(1, func() bool { return true })
	if v.Drop || !v.Resumed {
		t.Fatalf("keyframe at fill 1: %+v, want Resumed and not Drop", v)
	}
	if v.Skipped != 7 {
		t.Fatalf("skipped = %d, want 7 (1 + 1 + 5)", v.Skipped)
	}
	if d, e := c.Totals(); d != 7 || e != 1 {
		t.Fatalf("totals %d/%d, want 7/1", d, e)
	}

	// Back to normal: the next access unit at a low fill passes untouched.
	v = c.decide(0, func() bool { t.Fatal("no scan while not dropping"); return false })
	if v.Drop || v.Began || v.Resumed {
		t.Fatalf("after resuming: %+v", v)
	}

	// A second episode counts as such.
	c.decide(20, func() bool { return false })
	c.decide(0, func() bool { return true })
	if d, e := c.Totals(); d != 8 || e != 2 {
		t.Fatalf("totals %d/%d after a second episode, want 8/2", d, e)
	}
}

func TestCatchUpThresholdsAreSane(t *testing.T) {
	c := newCatchUp(0, 0)
	if c.high != catchUpHighWater || c.low != catchUpLowWater {
		t.Fatalf("zero thresholds must become the defaults, got %d/%d", c.high, c.low)
	}
	c = newCatchUp(2, 5)
	if c.low >= c.high {
		t.Fatalf("low %d must be below high %d", c.low, c.high)
	}
	c = newCatchUp(1, 0)
	if c.low != 0 || c.high != 1 {
		t.Fatalf("high 1 low 0 is the tightest legal pair, got %d/%d", c.high, c.low)
	}
}
