//go:build cgo && !gststub

// proxy_cgo.go is the two GStreamer operations the seam is made of: ARMING a
// proxysink so the next consumer receives its sticky events, and BINDING a
// proxysrc to a proxysink through an object-valued property.
//
// Both are short, both were expensive to get right, and both are here rather
// than inline at their call sites so that the measurement that settles them has
// one home. seam.go carries the argument; this file carries the code.
package gst

import (
	"errors"
	"fmt"
	"sync"
	"time"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// armTimeout bounds the wait for the IDLE probe to fire.
//
// MEASURED: 108-511 microseconds for BOTH branches together, on a capture
// pipeline carrying 1080p50 NV12 and 48 kHz stereo, on two rigs. Five seconds is
// "the streaming thread is wedged", not "this machine is slow", and the error it
// produces says so.
const armTimeout = 5 * time.Second

// armProxySink runs PLAN.md section 3.1's READY cycle over one proxysink and the
// leaky queue in front of it, so that the next proxysrc to attach receives
// STREAM_START, CAPS and SEGMENT.
//
// gstproxysink.c:134-139 resets sent_stream_start / sent_caps only on
// READY->PAUSED. A proxysink in an always-live pipeline never makes that
// transition again, so every consumer after the first gets nothing — measured,
// 1,133,076 bytes on the first send session and 0 on the second and third, with
// SRT connected and every lamp green throughout. Taking the two elements down to
// READY and back is what makes the transition happen again.
//
// # The probe goes on the UPSTREAM SRC PAD, not on the queue's own sink pad
//
// The peer of `queue:sink` is the tee's request pad, or alevel's src pad, or the
// conform capsfilter's. A probe on a SINK pad runs with that pad's stream lock
// held, and the SetState(READY) inside the callback would deadlock stopping the
// queue's own task. The measured rig put the probe on the src pad and this does
// too.
//
// # It is READY, never NULL
//
// set_state(NULL) on a branch inside a blocking pad probe was measured taking the
// on-air leg from 50 fps to 0 PERMANENTLY, with the pipeline still reporting
// PLAYING. READY stops the elements without destroying their pads or their
// allocation.
//
// # The order inside the callback may not be changed
//
// The queue is stopped FIRST so that the proxysink is not asked to change state
// with its own chain function running, and each is brought back with
// SyncStateWithParent rather than SetState(PLAYING) so that a capture pipeline
// which is itself mid-transition is not overtaken.
//
// # A ZERO RETURN FROM gst_pad_add_probe IS NOT A FAILURE HERE
//
// It is documented, in the Returns clause of gst_pad_add_probe itself:
//
//	"an id or 0 if no probe is pending. [...] When using GST_PAD_PROBE_TYPE_IDLE
//	 it can happen that the probe can be run immediately and if the probe returns
//	 GST_PAD_PROBE_REMOVE this functions returns 0."
//
// Both halves are true of this call site by construction: the pad IS idle (the
// probe is what makes it so) and the callback DOES return REMOVE. MEASURED: every
// arming on both rigs returned 0, and every one of them had already performed the
// READY cycle before returning. The codebase's existing idiom at
// gst_cgo.go:1907-1930 — `if id == 0 { fail }` — is CORRECT THERE and would abort
// a successful arming here. The completion channel below is the only correct
// success test, and a zero id is evidence of failure only when the callback has
// not run.
//
// # Element references need nothing special across the cycle
//
// No ref, no unref, no runtime.KeepAlive, no unbinding between cycles. Taking the
// old send pipeline to NULL and dropping the Go reference is sufficient; measured
// over three armed cycles on test sources and three on the real card.
func armProxySink(queue, sink gogst.Element) (time.Duration, error) {
	if queue == nil || sink == nil {
		return 0, errors.New("gst: armProxySink was given a nil element; the capture pipeline " +
			"did not build the seam it says it owns")
	}

	sinkPad := queue.GetStaticPad("sink")
	if sinkPad == nil {
		return 0, fmt.Errorf("gst: %s has no sink pad", queue.GetName())
	}
	peer := sinkPad.GetPeer()
	if peer == nil {
		return 0, fmt.Errorf("gst: %s:sink has no peer, so there is no upstream src pad to put "+
			"the IDLE probe on. The seam cannot be armed and every send session after the first "+
			"would carry zero bytes", queue.GetName())
	}

	var (
		mu       sync.Mutex
		problems []string
		once     sync.Once
	)
	note := func(format string, args ...any) {
		mu.Lock()
		problems = append(problems, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	done := make(chan struct{})
	started := time.Now()

	id := peer.AddProbe(gogst.PadProbeTypeIdle,
		func(_ gogst.Pad, _ *gogst.PadProbeInfo) gogst.PadProbeReturn {
			if ret := queue.SetState(gogst.StateReady); !stateChangeOK(ret) {
				note("%s would not go to READY (%s)", queue.GetName(), ret)
			}
			if ret := sink.SetState(gogst.StateReady); !stateChangeOK(ret) {
				note("%s would not go to READY (%s)", sink.GetName(), ret)
			}
			if !sink.SyncStateWithParent() {
				note("%s would not sync its state with its parent", sink.GetName())
			}
			if !queue.SyncStateWithParent() {
				note("%s would not sync its state with its parent", queue.GetName())
			}
			once.Do(func() { close(done) })
			return gogst.PadProbeRemove
		})

	armed := false
	select {
	case <-done:
		armed = true
	case <-time.After(armTimeout):
		// THE PROBE COMES OFF BEFORE THIS RETURNS, and that is not tidiness.
		// Timing out means the streaming thread is not idle NOW; it becoming idle
		// LATER is the likely outcome, and the callback would then run the READY
		// cycle over a live capture leg's queue and proxysink minutes after this
		// START was refused, out of any lock, under whatever consumer had since
		// attached. Taking a proxysink to READY under a live proxysrc is the same
		// class of event as the unarmed seam: no error, no EOS, the feed goes
		// quiet. The next START would also install a SECOND probe on the same pad.
		//
		// gst_pad_remove_probe blocks until a callback that is already running
		// returns, so the completion channel is re-read afterwards and is the final
		// answer. If the cycle did run in the window between the timer firing and
		// the probe coming off, the arming HAPPENED, and reporting failure here
		// would refuse a START that is healthy.
		if id != 0 {
			peer.RemoveProbe(id)
			select {
			case <-done:
				armed = true
			default:
			}
		}
	}

	if !armed {
		if id == 0 {
			return 0, fmt.Errorf("gst: gst_pad_add_probe returned 0 for the IDLE probe on %s:src "+
				"AND the callback never ran, so the probe was not installed at all and %s is "+
				"unarmed", peer.GetName(), sink.GetName())
		}
		return 0, fmt.Errorf("gst: the IDLE probe on %s:src (upstream of %s) did not fire within "+
			"%s; the streaming thread is not idle and never becomes so. The probe has been removed, "+
			"so nothing will run the READY cycle over this leg later",
			peer.GetName(), queue.GetName(), armTimeout)
	}

	took := time.Since(started)
	mu.Lock()
	got := append([]string(nil), problems...)
	mu.Unlock()
	if len(got) > 0 {
		return took, fmt.Errorf("gst: arming %s: %v. An unarmed proxysink does not fail loudly — "+
			"the next send session connects, the lamp goes green and the switcher receives "+
			"silence", sink.GetName(), got)
	}
	return took, nil
}

// bindProxySrc points one proxysrc at one proxysink and READS THE POINTER BACK.
//
// # go-gst DOES bind this write, and no hand-written cgo is needed
//
// The property is object-typed, so it cannot go in the parse string — measured,
// `proxysrc proxysink=ps` fails with `could not set property "proxysink" in
// element "proxysrc" to "ps"`. But SetObjectProperty takes it: measured, the
// write lands and ObjectProperty reads back a gogst.Element whose GetName matches
// the sink's. An earlier version of the plan said go-gst did not bind it and that
// a cgo file was needed; it does, and there is none.
//
// # The read-back is the point
//
// A write that silently did nothing would leave the proxysrc attached to NOTHING,
// which at the pipeline level is INDISTINGUISHABLE from the unarmed-seam hazard:
// no error, no EOS, no bus message, a send pipeline that reaches PLAYING and a
// feed that carries zero bytes. Reading the name back is the only thing that says
// the write landed.
func bindProxySrc(src, sink gogst.Element) error {
	if src == nil || sink == nil {
		return errors.New("gst: bindProxySrc was given a nil element")
	}

	src.SetObjectProperty(propProxySink, sink)

	got := src.ObjectProperty(propProxySink)
	bound, ok := got.(gogst.Element)
	if !ok {
		return fmt.Errorf("gst: after setting %s on %s the property reads back as %T (%v) rather "+
			"than a GstElement, so the write did not land and this proxysrc is attached to "+
			"nothing at all — which reaches the operator as a connected feed carrying zero bytes",
			propProxySink, src.GetName(), got, got)
	}
	if bound.GetName() != sink.GetName() {
		return fmt.Errorf("gst: %s.%s reads back as %q, not %q",
			src.GetName(), propProxySink, bound.GetName(), sink.GetName())
	}
	return nil
}

// propProxySink is proxysrc's object-valued handle on its producer. It is named
// here rather than written as a literal because it appears in a setter, in a
// getter and in the two errors above, and those four must agree.
const propProxySink = "proxysink"
