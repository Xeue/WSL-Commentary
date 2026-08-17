// sendseam.go is the SEND SIDE'S CLAIM on the capture layer: the object a send
// pipeline holds for as long as it is attached, and the refusal that stops a
// second one attaching.
//
// # Why there is an object at all rather than a rule
//
// A second proxysrc attaching to a live proxysink does not fail. It SILENTLY
// STEALS THE STREAM AND KILLS THE FIRST — measured, consumer A stopped dead at
// 5.994 s the instant consumer B attached at 6.007 s, with nothing on either bus,
// both pipelines still reporting PLAYING. There is no refusal inside the element
// to lean on and there is no symptom to notice, so the rule has to be enforced in
// Go, by a flag, and not by discipline.
//
// The object also carries the ARMING, because the two belong to the same moment:
// a send pipeline that claimed the seam and did not arm it is the measured
// zero-byte second session, and one that armed without claiming is a session that
// can be stolen. Doing both through one type is what stops the second half being
// forgotten in a later refactor of the first.
package gst

import (
	"errors"
	"fmt"
	"log"
	"sync"
)

// SendSeam is one send session's hold on a capture set.
//
// It is created at START, before the send pipeline is built, and released at
// STOP. Its lifetime is the send pipeline's lifetime exactly; nothing else may
// hold one.
type SendSeam struct {
	// mu guards this object's own fields. The per-proxysink claims underneath are
	// atomic and safe on their own; the object that owns them was not, and
	// "currently only ever called under the send pipeline's lock" is a property of
	// code that does not exist yet.
	mu sync.Mutex

	set CaptureSet

	// claimed is the pipelines whose claims this seam took, in the order it took
	// them. It is the release list, and it is the CLAIMED list rather than the
	// set's list so that a partially successful claim releases exactly what it
	// took.
	claimed []CapturePipeline

	// sendAtNull answers "has the send pipeline this seam was bound to actually
	// reached NULL", set by Bind. Nil until then.
	//
	// It exists because the ARMING'S CORRECTNESS DEPENDS ON IT and nothing else
	// checks. gst_proxy_src_dispose clears only the src's weak ref on the sink;
	// the SINK's ref on the old src survives until the old src is finalised, which
	// Go may not do promptly. Between the arming and the next Bind the capture
	// pushes a buffer or two, and gst_proxy_sink_sink_chain forwards the sticky
	// events to whatever that ref still points at. If the old proxysrc is at NULL
	// its internal_srcpad is inactive, gst_pad_store_sticky_event FAILS, and
	// copy_sticky_events resets sent_stream_start/sent_caps to FALSE — which is the
	// only reason the measured cycles work. If it is still PAUSED or PLAYING the
	// store SUCCEEDS, the flags go TRUE before the new proxysrc binds, and the new
	// session is the zero-byte feed this whole file exists to prevent.
	sendAtNull func() bool

	released bool
}

// NewSend takes the single-consumer claim on every proxysink in a capture set
// and arms them all.
//
// It fails — with an error wrapping ErrSeamBusy, naming the proxysink — if either
// half of the seam already has a consumer, and it fails without having claimed
// anything.
//
// # The arming happens HERE, at START, and not at STOP
//
// STOP has abnormal paths through which it can be skipped: a teardown that failed
// after taking one leg to NULL, a crash between the two halves, an aborted send
// build. START has none, because it cannot proceed without it. Arming here also
// makes the invariant self-healing — it runs on the first START as a no-op, after
// a STOP that failed halfway, and after an aborted send build — and it can never
// race a reconnect, because no send pipeline exists at this instant.
//
// # What the caller does next
//
// Build the send pipeline from sendDescription, look up vproxsrc and aproxsrc by
// name, and hand each the pointer with Bind. Then, and only then, take it to
// PLAYING. Binding before the arming would attach a consumer to a proxysink whose
// sticky events have not been reset, which is the whole hazard.
func NewSend(set CaptureSet) (*SendSeam, error) {
	pipelines := set.Pipelines()
	if len(pipelines) == 0 {
		return nil, errors.New("gst: NewSend was given an empty capture set. A send pipeline is " +
			"minted only by capture, so that a send pipeline with no device behind it is " +
			"unconstructible rather than merely unlikely")
	}

	s := &SendSeam{set: set}
	for _, p := range pipelines {
		if err := p.ClaimForSend(); err != nil {
			s.Stop()
			return nil, err
		}
		s.claimed = append(s.claimed, p)
	}
	for _, p := range s.claimed {
		if err := p.ArmForSend(); err != nil {
			s.Stop()
			return nil, fmt.Errorf("gst: the capture seam could not be armed, so this send "+
				"session would have carried zero bytes with SRT connected and every lamp "+
				"green: %w", err)
		}
	}
	return s, nil
}

// Set is the capture set this seam is attached to.
func (s *SendSeam) Set() CaptureSet { return s.set }

// Stop releases every claim this seam took. It is idempotent, because the path
// that calls it is a teardown and a teardown that can fail twice must be callable
// twice.
//
// It does NOT stop, arm or otherwise touch the capture pipelines. They are
// always-live and outlive every send session; that is the whole of R1.
//
// # CALL IT AFTER THE SEND PIPELINE HAS REACHED NULL, NOT BEFORE
//
// The next session's arming is only correct if this session's proxysrcs are at
// NULL first — see the sendAtNull field for the mechanism, read out of
// gstproxysink.c/gstproxysrc.c 1.26.10. Releasing the claim while the old send
// pipeline is still PAUSED or PLAYING opens the seam to a consumer whose sticky
// events will never be reset, which is the measured zero-byte session with SRT
// connected and every lamp green. The claim cannot protect against it, because
// releasing the claim IS the thing being done too early, so the violation is
// named in the log instead.
func (s *SendSeam) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return
	}
	if s.sendAtNull != nil && !s.sendAtNull() {
		log.Print("gst: the capture seam is being released while its send pipeline has not " +
			"reached NULL. The next session's arming cannot repair a proxysink whose old " +
			"consumer is still alive — gst_proxy_sink_sink_chain will re-store the sticky events " +
			"on the old proxysrc's still-active pad and the new session carries zero bytes with " +
			"SRT connected. Take the send pipeline to NULL first")
	}
	s.released = true
	for _, p := range s.claimed {
		p.ReleaseFromSend()
	}
	s.claimed = nil
}
