//go:build cgo && !gststub

// sendseam_cgo.go is the GStreamer half of the send side's claim: pointing the
// parsed send pipeline's two proxysrcs at the capture pipelines' proxysinks.
package gst

import (
	"errors"
	"fmt"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// Bind points a parsed send pipeline's vproxsrc and aproxsrc at the proxysinks
// this seam has claimed, and reads each pointer back.
//
// It must be called AFTER NewSend (which arms the sinks) and BEFORE the send
// pipeline leaves NULL. Both orderings matter and neither is recoverable
// afterwards: binding before the arming attaches a consumer to a proxysink whose
// sticky events have not been reset, and binding after PLAYING attaches it to a
// pipeline that has already decided it has no upstream.
//
// A capture pipeline that is not a *cgoCapture — the Gate A twin — is refused by
// name rather than skipped. A Bind that silently did nothing would leave the send
// pipeline attached to NOTHING, which at the pipeline level is indistinguishable
// from the hazard this whole seam exists for: PLAYING, connected, zero bytes.
// # The proxysinks are read UNDER THE CAPTURE PIPELINE'S OWN LOCK
//
// They are fields a capture teardown nils while holding that lock. Reading them
// without it is a data race — two reported by -race against
// teardownLocked — and the semantic half is worse than the race: an unlocked read
// can return a still-non-nil element belonging to a pipeline already on its way to
// NULL, and the send pipeline then reaches PLAYING bound to a dead proxysink. SRT
// connects, the lamps go green, and the feed carries zero bytes, which is the exact
// hazard this seam exists to prevent. seamSinks also refuses a capture that is
// stopped, unstarted or already failed, which the field read could not.
func (s *SendSeam) Bind(pipeline gogst.Pipeline) error {
	if s == nil {
		return errors.New("gst: SendSeam.Bind was called on a nil seam")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.released {
		return errors.New("gst: this send seam has been released; the capture pipelines it " +
			"claimed are free for another consumer and binding to them now would steal the " +
			"stream from whatever took them")
	}
	if pipeline == nil {
		return errors.New("gst: SendSeam.Bind was given a nil send pipeline")
	}

	var video, audio gogst.Element
	for _, p := range s.claimed {
		c, ok := p.(*cgoCapture)
		if !ok {
			return fmt.Errorf("gst: a capture pipeline in this set is a %T rather than the real "+
				"one; a send pipeline built against it would reach PLAYING attached to nothing "+
				"and carry zero bytes", p)
		}
		v, a, err := c.seamSinks()
		if err != nil {
			return err
		}
		if video == nil {
			video = v
		}
		if audio == nil {
			audio = a
		}
	}

	for _, pair := range []struct {
		srcName string
		sink    gogst.Element
	}{
		{nameVideoProxySrc, video},
		{nameAudioProxySrc, audio},
	} {
		src := pipeline.GetByName(pair.srcName)
		if src == nil {
			return errNoElement(pair.srcName, "there is nothing to attach to the capture seam")
		}
		if pair.sink == nil {
			return fmt.Errorf("gst: no capture pipeline in this set owns the proxysink %s needs. "+
				"The send description is invariant — it always has both legs — so a capture set "+
				"missing one is a planning error and not a seat with a shorter feed",
				pair.srcName)
		}
		if err := bindProxySrc(src, pair.sink); err != nil {
			return err
		}
	}

	// The seam now knows which pipeline it is feeding, so Stop can say whether it
	// is being released too early. See SendSeam.sendAtNull.
	s.sendAtNull = func() bool {
		state, _, _ := pipeline.GetState(0)
		return state == gogst.StateNull
	}
	return nil
}
