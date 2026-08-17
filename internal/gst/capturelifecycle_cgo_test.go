//go:build cgo && !gststub

// capturelifecycle_cgo_test.go is the guard over the capture layer's LIFECYCLE:
// the two watchdog deadlocks, the fault channel that outlives an aborted build,
// the latched fault that must not outlive it, and the two refusals that stop a
// send pipeline being bound to a capture that cannot feed it.
//
// None of these needs GStreamer. Every type under test is a plain Go struct whose
// zero value is usable, so the tests below construct one directly rather than
// building a pipeline — which is what lets them assert the failure modes that only
// happen on the paths a live test never takes: a build that aborted, a state
// change that failed, a teardown that ran twice.
//
// They live behind the cgo tag only because the types do.
package gst

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The muxer watchdog's two deadlocks
// ---------------------------------------------------------------------------

// newTestLiveWatch is attachLiveWatch's product without the pipeline: the same
// channels, no probes, and as many named pads as the caller wants counters for.
func newTestLiveWatch(pads ...string) *liveWatch {
	w := &liveWatch{
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	for _, name := range pads {
		w.pads = append(w.pads, &livePad{name: name})
	}
	return w
}

// TestTheWatchdogStopReturnsWhenThePollerWasNeverStarted covers the whole window
// between attachLiveWatch and PLAYING.
//
// The probes go on BEFORE the pipeline is started — that is what makes the 2 s
// liveness gate mean anything — and the poller goes on AFTER. Everything that can
// fail in between (BlockSetState refusing, a bus error latching first, the
// liveness gate refusing the START) reaches teardown and calls Stop. A Stop that
// joins a goroutine nobody spawned never returns: START never returns either, the
// send pipeline cannot be torn down, and shutdown never completes with the session
// lock held.
func TestTheWatchdogStopReturnsWhenThePollerWasNeverStarted(t *testing.T) {
	w := newTestLiveWatch(nameMuxOutput)

	returned := make(chan struct{})
	go func() {
		w.Stop()
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("liveWatch.Stop() did not return when run() had never been called. Every failure " +
			"between attachLiveWatch and PLAYING takes this path, and a Stop that hangs there " +
			"wedges START, the send teardown and shutdown, permanently")
	}
}

// TestTheWatchdogStopIsCallableFromInsideItsOwnFatalHandler is the other half of
// the same rule.
//
// The fatal handler's job is to take a dead feed off air, which means tearing the
// send pipeline down, which means Stop. If the poller publishes its completion
// with a deferred close AFTER calling fatal, Stop waits for the handler and the
// handler waits for Stop — and the process wedges while on air, with the feed
// already dead. Nothing in the type's doc forbids the re-entry, and nothing should:
// it is the natural implementation.
func TestTheWatchdogStopIsCallableFromInsideItsOwnFatalHandler(t *testing.T) {
	w := newTestLiveWatch(nameMuxOutput)

	handled := make(chan error, 1)
	// PLAYING ten seconds ago and not one buffer since, so the first 250 ms tick
	// produces the silence verdict.
	w.run(time.Now().Add(-10*time.Second), func(err error) {
		w.Stop()
		handled <- err
	})

	select {
	case err := <-handled:
		if !errors.Is(err, ErrPipelineFatal) {
			t.Errorf("the fatal handler was given %v, which does not wrap ErrPipelineFatal", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("liveWatch.Stop() called from inside the fatal callback never returned. The " +
			"handler that fires here is the one that takes a dead feed off air, so it is the " +
			"handler that tears the send pipeline down, so it is the handler that calls Stop")
	}
}

// TestTheWatchdogStopJoinsAPollerThatIsRunning keeps the fix above from being made
// by simply not joining: the poller reads the pads and the probes write them, and
// a Stop that removed the probes while the poller was still reading is the race
// the join exists to prevent.
func TestTheWatchdogStopJoinsAPollerThatIsRunning(t *testing.T) {
	w := newTestLiveWatch(nameMuxVideoQueue, nameMuxAudioQueue, nameMuxOutput)
	// A pad seen just now, so the verdict stays healthy and the poller only ever
	// leaves through Stop.
	for _, p := range w.pads {
		p.mu.Lock()
		p.buffers, p.last = 1, time.Now()
		p.mu.Unlock()
	}
	w.run(time.Now(), func(error) { t.Error("a healthy feed was indicted") })

	returned := make(chan struct{})
	go func() {
		w.Stop()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("liveWatch.Stop() did not return with the poller running")
	}
	select {
	case <-w.done:
	default:
		t.Error("Stop returned without joining the poller. The poller reads the pads the probes " +
			"write, and detaching underneath it is the race the join exists to prevent")
	}
	w.Stop() // idempotent: the teardown that calls it can itself be run twice
}

// ---------------------------------------------------------------------------
// The fault channel across an aborted build
// ---------------------------------------------------------------------------

// newTestCapture is what NewCapture returns, minus the width publisher and the
// validation: enough of a cgoCapture for the teardown paths, which touch no
// GStreamer at all when nothing was built.
//
// logWarnings IS started, because it is not an optional decoration on the object:
// it is the only thing that moves a line from deliverWarning to Warnings(), and
// it is what closes Warnings() at the end. A fixture without it would test a
// warning path that cannot deliver and a Stop that never ends the channel.
func newTestCapture(legs CaptureLegs) *cgoCapture {
	c := &cgoCapture{
		legs:     legs,
		claims:   newSeamClaims(legs),
		errs:     make(chan error, errorChannelBuffer),
		warns:    make(chan string, warningChannelBuffer),
		warnsOut: make(chan string, warningChannelBuffer),
		widths:   make(chan capturedWidth, warningChannelBuffer),
	}
	go c.logWarnings()
	return c
}

// TestAnAbortedBuildLeavesTheFaultChannelOpen is the regression for the capture
// object that goes deaf on its first failure.
//
// buildLocked's abort() runs teardownLocked, and Start's documented
// retry-without-preview then builds again on the SAME object. A teardown that
// closed the fault channels would hand the application a closed Faults() over a
// pipeline that is live: a `for range` exits at launch, a bare select spins, and
// the card being unplugged twenty minutes later is invisible.
func TestAnAbortedBuildLeavesTheFaultChannelOpen(t *testing.T) {
	c := newTestCapture(CaptureLegs{Commentary: CommentaryNative})

	// Exactly what abort() does.
	if err := c.teardownLocked(); err != nil {
		t.Fatalf("teardownLocked on a pipeline that was never built failed: %v", err)
	}

	c.markFatal(errors.New("the card was unplugged twenty minutes into the match"))
	c.deliver(c.fatalError())

	select {
	case err, ok := <-c.Faults():
		if !ok {
			t.Fatal("Faults() is a CLOSED channel after an aborted build. The retry that Start " +
				"performs would produce a LIVE capture pipeline whose every fault is dropped")
		}
		if err == nil {
			t.Fatal("Faults() delivered a nil error, which is what a closed channel reads as")
		}
	default:
		t.Fatal("the fault was not delivered after an aborted build")
	}

	// The warning takes a hop through logWarnings, which writes it to the field
	// log and forwards it here, so this waits rather than polling once.
	c.deliverWarning("gst: a warning after an aborted build")
	select {
	case line, ok := <-c.Warnings():
		if !ok || line == "" {
			t.Fatal("Warnings() is closed or empty after an aborted build")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the warning was not delivered after an aborted build")
	}
}

// TestTheRetryDoesNotInheritTheFirstAttemptsFatal is the other half of the same
// bug, and it is the half that decides whether a seat comes up at all.
//
// The retry-without-preview exists for a confidence monitor that will not start.
// If teardownLocked leaves the first attempt's latched fault standing, the second
// build aborts on it — and the seat comes up with NO capture at all, no meters and
// no commentary, instead of coming up without a preview.
func TestTheRetryDoesNotInheritTheFirstAttemptsFatal(t *testing.T) {
	c := newTestCapture(CaptureLegs{Picture: PictureCard, Preview: true})

	c.markFatal(errors.New("glimagesink: could not create a GL context"))
	if c.fatalError() == nil {
		t.Fatal("markFatal did not latch")
	}

	if err := c.teardownLocked(); err != nil {
		t.Fatalf("teardownLocked failed: %v", err)
	}
	if err := c.fatalError(); err != nil {
		t.Fatalf("the second build inherits the first attempt's fault (%v), so a seat whose "+
			"preview will not start comes up with no capture at all rather than without a "+
			"confidence monitor", err)
	}
	if got := c.publishedWidth.Load(); got != 0 {
		t.Errorf("the published width survived teardown as %d; a rebuild at the same width would "+
			"take the probe's de-duplication early return and never republish it", got)
	}
}

// TestStopClosesTheFaultChannels — the goroutines parked on them are ended by Stop
// and by nothing else, so a capture that is never stopped leaks two of them.
func TestStopClosesTheFaultChannels(t *testing.T) {
	c := newTestCapture(CaptureLegs{Commentary: CommentaryNative})
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop on an unstarted pipeline failed: %v", err)
	}
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop is not idempotent: %v", err)
	}
	for _, ch := range []struct {
		name string
		open func() bool
	}{
		{"Faults()", func() bool { _, ok := <-c.Faults(); return ok }},
		{"Warnings()", func() bool { _, ok := <-c.Warnings(); return ok }},
	} {
		if ch.open() {
			t.Errorf("%s is still open after Stop, so the goroutine draining it never ends", ch.name)
		}
	}
}

// TestEveryWarningReachesBothTheLogAndTheApplication is the regression for a
// defect that had no symptom: Warnings() used to BE the channel logWarnings
// ranges over, so every line went to one consumer or the other and which one
// depended on the scheduler.
//
// The failure it produced is the invisible kind. A confidence monitor dying is a
// warning the operator is meant to see on screen AND a line the field log is
// meant to carry afterwards, and half of each is indistinguishable from a
// mechanism that works — until the night somebody has to explain why the log says
// nothing about the fault that was on screen.
func TestEveryWarningReachesBothTheLogAndTheApplication(t *testing.T) {
	c := newTestCapture(CaptureLegs{Picture: PictureCard, Preview: true})
	t.Cleanup(func() { c.Stop() })

	const lines = 6
	for i := 0; i < lines; i++ {
		c.deliverWarning("gst: the confidence monitor failed")
	}
	for i := 0; i < lines; i++ {
		select {
		case line, ok := <-c.Warnings():
			if !ok || line == "" {
				t.Fatalf("warning %d of %d: Warnings() closed or empty", i+1, lines)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d warnings reached the application; the rest were taken by the "+
				"logger, which reads the same channel", i, lines)
		}
	}
}

// ---------------------------------------------------------------------------
// The refusals that keep a send pipeline off a capture that cannot feed it
// ---------------------------------------------------------------------------

// TestStopRefusesWhileASendPipelineHoldsTheSeam is the ordering rule, enforced.
//
// Taking the device to NULL under a bound proxysrc is the measured
// completely-silent failure: 0 buffers, no EOS, no ERROR and no WARNING on either
// bus, the send pipeline still PLAYING and SRT still connected. It was previously
// enforced by four call sites remembering to stop the sender first.
func TestStopRefusesWhileASendPipelineHoldsTheSeam(t *testing.T) {
	c := newTestCapture(CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard})
	if err := c.ClaimForSend(); err != nil {
		t.Fatalf("the setup claim failed: %v", err)
	}

	err := c.Stop()
	if err == nil {
		t.Fatal("Stop took the capture down with a consumer still attached. Nothing crosses the " +
			"seam to say the producer has gone — proxysink returns GST_FLOW_OK unconditionally — " +
			"so the switcher receives silence behind a connected socket")
	}
	if !errors.Is(err, ErrSeamBusy) {
		t.Errorf("the refusal does not wrap ErrSeamBusy: %v", err)
	}
	if !strings.Contains(err.Error(), nameVideoProxySink) {
		t.Errorf("the refusal does not name the proxysink that is held: %v", err)
	}

	// And the way out is the way the caller was supposed to go: release, then stop.
	c.ReleaseFromSend()
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop failed after the seam was released: %v", err)
	}
}

// TestArmForSendRefusesAfterTheCaptureHasDied is the guard on a green lamp over
// silence.
//
// A latched fault clears neither started nor stopped, so every method on the
// object still works. On a dead device the arming's IDLE probe fires IMMEDIATELY —
// the pad is idle because it is dead — and reports success, after which only the
// 2 s liveness gate stands between the operator and a connected feed carrying
// nothing. That gate is PLAN.md step 6's backstop for a MISSED arming, not for an
// arming that succeeded over a corpse.
func TestArmForSendRefusesAfterTheCaptureHasDied(t *testing.T) {
	c := newTestCapture(CaptureLegs{Commentary: CommentaryNative})
	c.started = true
	c.markFatal(errors.New("asrc: Could not read from resource (the device went away)"))

	if c.Health() == nil {
		t.Fatal("Health() is nil on a capture whose bus handler latched a fatal")
	}
	if err := c.ArmForSend(); err == nil {
		t.Fatal("ArmForSend reported success on a capture pipeline that has already died")
	}

	if _, _, err := c.seamSinks(); err == nil {
		t.Fatal("seamSinks handed out proxysinks belonging to a capture pipeline that has died; " +
			"a send pipeline bound to them reaches PLAYING and carries zero bytes")
	}

	// The same accessor refuses an unstarted pipeline, which is the other way a
	// send pipeline can be bound to a producer that will never push.
	fresh := newTestCapture(CaptureLegs{Commentary: CommentaryNative})
	if _, _, err := fresh.seamSinks(); err == nil {
		t.Fatal("seamSinks handed out proxysinks from a capture pipeline that was never started")
	}
}

// TestInputChannelsDoesNotQueueBehindADeviceOpen is the UI-facing half.
//
// mu is held across the whole NULL->PLAYING transition — captureStartTimeout, and
// twice that on the retry-without-preview path — and InputChannels is polled many
// times a second while the routing panel is open. CommentaryMuted was given an
// atomic for exactly this argument; the width read is the same argument about the
// same panel.
func TestInputChannelsDoesNotQueueBehindADeviceOpen(t *testing.T) {
	c := newTestCapture(CaptureLegs{Commentary: CommentaryNative})

	c.mu.Lock()
	defer c.mu.Unlock()

	done := make(chan int, 1)
	go func() { done <- c.InputChannels() }()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("InputChannels() blocked behind a held mu. mu is held for the length of a device " +
			"open, so a routing panel polling this through a commentary device change would " +
			"freeze for seconds")
	}
}
