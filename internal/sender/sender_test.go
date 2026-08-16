package sender

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"wslcomms/internal/gst"
)

// ---------------------------------------------------------------------------
// the backoff ladder in isolation
// ---------------------------------------------------------------------------

// TestBackoffDelay pins the ladder from specification section 6.2 rung by rung.
// The first rung is the one that matters most: it must stay at or above six
// seconds to clear M2L-X's re-accept refusal window, and if somebody ever
// "optimises" it to one second this is the test that stops them.
func TestBackoffDelay(t *testing.T) {
	tests := []struct {
		name    string
		attempt int
		want    time.Duration
	}{
		{"first attempt clears the re-accept window", 0, 7 * time.Second},
		{"second", 1, 7 * time.Second},
		{"third", 2, 10 * time.Second},
		{"fourth", 3, 15 * time.Second},
		{"fifth", 4, 20 * time.Second},
		{"sixth is the cap", 5, 30 * time.Second},
		{"seventh stays at the cap", 6, 30 * time.Second},
		{"hundredth stays at the cap", 100, 30 * time.Second},
		{"a whole match of failures stays at the cap", 10000, 30 * time.Second},
		{"negative is clamped rather than panicking", -1, 7 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backoffDelay(tt.attempt); got != tt.want {
				t.Fatalf("backoffDelay(%d) = %v, want %v", tt.attempt, got, tt.want)
			}
		})
	}
}

// TestBackoffLadderFirstDelayClearsRefusalWindow guards the declared ladder
// itself, not the function that walks it. Six seconds is the measured floor.
func TestBackoffLadderFirstDelayClearsRefusalWindow(t *testing.T) {
	if len(BackoffLadder) == 0 {
		t.Fatal("BackoffLadder is empty; the sender would go straight to the cap")
	}
	if BackoffLadder[0] < 6*time.Second {
		t.Fatalf("BackoffLadder[0] = %v, must be >= 6s to clear the re-accept refusal window", BackoffLadder[0])
	}
	if BackoffCap < BackoffLadder[len(BackoffLadder)-1] {
		t.Fatalf("BackoffCap %v is shorter than the last rung %v", BackoffCap, BackoffLadder[len(BackoffLadder)-1])
	}
}

// ---------------------------------------------------------------------------
// happy path
// ---------------------------------------------------------------------------

// TestHappyPath is the sequence a match should see once: connect, stay
// connected, stop when the operator says so.
func TestHappyPath(t *testing.T) {
	p := newFakePipeline()
	s := newSender(p, newFakeClock())

	opts := testOpts()
	if err := s.Start(opts); err != nil {
		t.Fatalf("Start: %v", err)
	}

	expectStates(t, s.States(), StateConnecting, StateConnected)

	// ForceKeyUnit is issued before CONNECTED is announced, so observing
	// CONNECTED is sufficient proof that it happened.
	if got := p.snapshot(); got.forceKeyUnits != 1 {
		t.Fatalf("forceKeyUnits = %d, want 1", got.forceKeyUnits)
	}

	sinks := p.sinks()
	if len(sinks) != 1 {
		t.Fatalf("ReplaceSink called %d times, want 1", len(sinks))
	}
	if sinks[0] != opts.Sink {
		t.Fatalf("sink opts = %+v, want %+v", sinks[0], opts.Sink)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	expectState(t, s.States(), StateStopped)
	expectClosed(t, s.States())

	if got := p.snapshot(); got.starts != 1 || got.stops != 1 {
		t.Fatalf("counts = %+v, want one Start and one Stop", got)
	}
}

// TestStartPipelineFailureIsAnError distinguishes the one failure that is a
// Start error — a pipeline that will not play — from a failure to connect,
// which is not. After it, the Sender is still startable.
func TestStartPipelineFailureIsAnError(t *testing.T) {
	p := newFakePipeline()
	wantErr := errors.New("gst: no such audio device")
	p.startErr = wantErr

	s := newSender(p, newFakeClock())

	err := s.Start(testOpts())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Start error = %v, want it to wrap %v", err, wantErr)
	}
	if got := s.Stop(); !errors.Is(got, ErrNotStarted) {
		t.Fatalf("Stop after a failed Start = %v, want ErrNotStarted", got)
	}

	// The operator fixes the device and tries again.
	p.mu.Lock()
	p.startErr = nil
	p.mu.Unlock()

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestStopReturnsPipelineStopError checks that a teardown failure reaches the
// caller rather than being swallowed, and that repeated Stops report the same
// thing.
func TestStopReturnsPipelineStopError(t *testing.T) {
	p := newFakePipeline()
	wantErr := errors.New("gst: pipeline would not go to NULL")
	p.stopErr = wantErr

	s := newSender(p, newFakeClock())
	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)

	first := s.Stop()
	if !errors.Is(first, wantErr) {
		t.Fatalf("Stop = %v, want it to wrap %v", first, wantErr)
	}
	if second := s.Stop(); second != first {
		t.Fatalf("second Stop = %v, want the same error as the first (%v)", second, first)
	}
}

// TestForceKeyUnitFailureDoesNotDropTheConnection: an IDR that will not come
// costs at most the two second GOP. Tearing down a working connection over it
// would cost the match.
func TestForceKeyUnitFailureDoesNotDropTheConnection(t *testing.T) {
	p := newFakePipeline()
	p.forceKeyUnitErr = errors.New("gst: no encoder to force")

	s := newSender(p, newFakeClock())
	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// ---------------------------------------------------------------------------
// the reconnect ladder end to end
// ---------------------------------------------------------------------------

// TestConnectFailsThenSucceedsWalksTheLadder is the central test of this
// package. Twelve consecutive connection failures must produce exactly the
// delays 7, 7, 10, 15, 20, 30, 30, 30, 30, 30, 30, 30 seconds, must never give
// up, and the thirteenth attempt must connect normally.
func TestConnectFailsThenSucceedsWalksTheLadder(t *testing.T) {
	const failures = 12

	want := []time.Duration{
		7 * time.Second, 7 * time.Second, 10 * time.Second, 15 * time.Second,
		20 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second,
		30 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	if len(want) != failures {
		t.Fatalf("test is inconsistent: %d delays for %d failures", len(want), failures)
	}

	p := newFakePipeline()
	p.failSinks(failures, errConnectRefused)
	clk := newFakeClock()
	s := newSender(p, clk)

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectState(t, s.States(), StateConnecting)

	for i := 0; i < failures; i++ {
		expectState(t, s.States(), StateBackoff)

		tm := clk.next(t)
		if tm.d != want[i] {
			t.Fatalf("attempt %d waited %v, want %v", i, tm.d, want[i])
		}
		tm.fire()

		expectState(t, s.States(), StateConnecting)
		if !tm.wasStopped() {
			t.Fatalf("attempt %d: the backoff timer was not released", i)
		}
	}

	expectState(t, s.States(), StateConnected)

	if got := p.snapshot(); got.replaceSinks != failures+1 {
		t.Fatalf("replaceSinks = %d, want %d", got.replaceSinks, failures+1)
	}
	if got := p.snapshot(); got.forceKeyUnits != 1 {
		t.Fatalf("forceKeyUnits = %d, want 1 (only the successful attempt forces an IDR)", got.forceKeyUnits)
	}
	if got := clk.delays(); len(got) != failures {
		t.Fatalf("armed %d timers, want %d", len(got), failures)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	expectState(t, s.States(), StateStopped)
	expectClosed(t, s.States())
}

// TestNoAttemptLimit runs the ladder far past any plausible retry cap. On total
// network loss libsrt declares the peer dead at about 5.27 s and M2L-X never
// recovers by itself, so a sender that gave up would silence the commentary for
// the rest of the match.
func TestNoAttemptLimit(t *testing.T) {
	const failures = 500

	p := newFakePipeline()
	p.failSinks(failures, errConnectRefused)
	clk := newFakeClock()
	s := newSender(p, clk)

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for i := 0; i < failures; i++ {
		tm := clk.next(t)
		if i >= len(BackoffLadder) && tm.d != BackoffCap {
			t.Fatalf("attempt %d waited %v, want the cap %v", i, tm.d, BackoffCap)
		}
		tm.fire()
	}

	// The 501st attempt is allowed to succeed; the machine must still be alive
	// to make it.
	deadline := time.After(testTimeout)
	for {
		if p.snapshot().replaceSinks >= failures+1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("after %d failures the sender stopped trying at %d attempts",
				failures, p.snapshot().replaceSinks)
		default:
			runtime.Gosched()
		}
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestReconnectAfterPeerLoss covers the CONNECTED -> DRAINING -> BACKOFF ->
// CONNECTING -> CONNECTED cycle, which is what a mid-match SRT drop looks like,
// and checks that the ladder restarts from its first rung rather than resuming
// wherever an earlier outage left it.
func TestReconnectAfterPeerLoss(t *testing.T) {
	p := newFakePipeline()
	clk := newFakeClock()
	s := newSender(p, clk)

	opts := testOpts()
	if err := s.Start(opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)

	for cycle := 0; cycle < 3; cycle++ {
		p.mustInjectError(t, errPeerGone)
		expectStates(t, s.States(), StateDraining, StateBackoff)

		tm := clk.next(t)
		if tm.d != BackoffLadder[0] {
			t.Fatalf("cycle %d waited %v after a drop, want the first rung %v",
				cycle, tm.d, BackoffLadder[0])
		}
		tm.fire()

		expectStates(t, s.States(), StateConnecting, StateConnected)
	}

	// Specification section 6.2: every reconnect creates a fresh srtsink with
	// *identical* properties. A sender that quietly altered the latency or
	// dropped the passphrase between attempts would connect unencrypted, or not
	// at all, and only on the retry path — the worst possible thing to discover
	// during a match.
	sinks := p.sinks()
	if len(sinks) != 4 {
		t.Fatalf("ReplaceSink called %d times, want 4", len(sinks))
	}
	for i, got := range sinks {
		if got != opts.Sink {
			t.Fatalf("attempt %d used sink opts %+v, want %+v", i, got, opts.Sink)
		}
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestForceKeyUnitAfterEveryConnect: the picture must recover immediately on
// every reconnection, not only the first. Without this the far end waits up to
// two seconds for the next scheduled IDR each time.
func TestForceKeyUnitAfterEveryConnect(t *testing.T) {
	const cycles = 4

	p := newFakePipeline()
	clk := newFakeClock()
	s := newSender(p, clk)

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)

	for cycle := 1; cycle <= cycles; cycle++ {
		if got := p.snapshot().forceKeyUnits; got != cycle {
			t.Fatalf("after %d connects forceKeyUnits = %d, want %d", cycle, got, cycle)
		}

		p.mustInjectError(t, errPeerGone)
		expectStates(t, s.States(), StateDraining, StateBackoff)
		clk.next(t).fire()
		expectStates(t, s.States(), StateConnecting, StateConnected)
	}

	if got := p.snapshot().forceKeyUnits; got != cycles+1 {
		t.Fatalf("forceKeyUnits = %d, want %d", got, cycles+1)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// ---------------------------------------------------------------------------
// DRAINING actually drains
// ---------------------------------------------------------------------------

// TestDrainingRemovesTheSinkBeforeTheBackoffWait is the test that the reconnect
// costs seven seconds rather than fourteen.
//
// Specification section 6.2 orders the cycle DRAINING (unlink, srtout to NULL,
// remove) -> BACKOFF -> CONNECTING, and the order is the entire value of the
// ladder. An M2L-X SRT listener accepts exactly one peer, never displaces the
// incumbent, and refuses re-accept for roughly five seconds; the >= 6 s first
// rung is sized to outlast that window, and it only outlasts anything if our own
// socket is already gone when the wait begins. Leaving the teardown to the next
// ReplaceSink puts milliseconds between our socket closing and the new handshake,
// so the retry lands inside the refusal window, fails, and the connection does
// not come back until the second rung has elapsed too.
//
// The assertion is on the ORDER, not on a count, because a count is satisfied by
// the broken version as well: ReplaceSink tears the old sink out too, just far
// too late. Pipeline calls and clock waits share one log for exactly this.
func TestDrainingRemovesTheSinkBeforeTheBackoffWait(t *testing.T) {
	p, clk, callLog := newRecorded()
	s := newSender(p, clk)

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)

	p.mustInjectError(t, errPeerGone)
	expectStates(t, s.States(), StateDraining, StateBackoff)

	tm := clk.next(t)
	if tm.d != BackoffLadder[0] {
		t.Fatalf("waited %v, want the first rung %v", tm.d, BackoffLadder[0])
	}

	// The sink is gone and only then does the clock start. Firing the timer is
	// deliberately left until after this assertion so that nothing the next
	// attempt does can be mistaken for the teardown.
	expectEvents(t, callLog,
		"start",
		"replaceSink", "forceKeyUnit",
		"removeSink",
		"wait 7s",
	)

	tm.fire()
	expectStates(t, s.States(), StateConnecting, StateConnected)

	expectEvents(t, callLog,
		"start",
		"replaceSink", "forceKeyUnit",
		"removeSink",
		"wait 7s",
		"replaceSink", "forceKeyUnit",
	)

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestDrainingRemovesTheSinkOnEveryCycle: the teardown is not a first-outage
// special case. Every reconnect for the length of a match must vacate the
// listener before it starts waiting.
func TestDrainingRemovesTheSinkOnEveryCycle(t *testing.T) {
	const cycles = 5

	p := newFakePipeline()
	clk := newFakeClock()
	s := newSender(p, clk)

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)

	for cycle := 1; cycle <= cycles; cycle++ {
		p.mustInjectError(t, errPeerGone)
		expectStates(t, s.States(), StateDraining, StateBackoff)

		tm := clk.next(t)
		if got := p.snapshot().removeSinks; got != cycle {
			t.Fatalf("cycle %d: RemoveSink called %d times by the time the wait was armed, want %d",
				cycle, got, cycle)
		}
		tm.fire()
		expectStates(t, s.States(), StateConnecting, StateConnected)
	}

	// A successful connect must not remove anything of its own: the only
	// teardown is the one DRAINING performs.
	if got := p.snapshot().removeSinks; got != cycles {
		t.Fatalf("RemoveSink called %d times over %d outages, want %d", got, cycles, cycles)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestDrainingSucceedsWithoutRemovingAnything: a connect that never succeeded
// leaves no sink to remove, so the CONNECTING -> BACKOFF edge must not call
// RemoveSink at all. gst.Pipeline.RemoveSink is idempotent, so calling it would
// be harmless — but it would also mean the machine could not tell the two edges
// apart, and the log would say a sink was torn down when none existed.
func TestDrainingSucceedsWithoutRemovingAnything(t *testing.T) {
	p := newFakePipeline()
	p.failSinks(2, errConnectRefused)
	clk := newFakeClock()
	s := newSender(p, clk)

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectState(t, s.States(), StateConnecting)

	for i := 0; i < 2; i++ {
		expectState(t, s.States(), StateBackoff)
		clk.next(t).fire()
		expectState(t, s.States(), StateConnecting)
	}
	expectState(t, s.States(), StateConnected)

	if got := p.snapshot().removeSinks; got != 0 {
		t.Fatalf("RemoveSink called %d times on the failed-connect path, want 0", got)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestRemoveSinkFailureStillReachesBackoff: a teardown that will not complete
// must not wedge the machine. The next ReplaceSink performs the same removal
// itself, so a machine that carries on can still recover; one that stopped would
// be off air for the rest of the match, which is the outcome this whole package
// exists to prevent.
func TestRemoveSinkFailureStillReachesBackoff(t *testing.T) {
	logs := captureLog(t)

	errRemove := errors.New("gst: could not remove srtout-1 from the pipeline")

	p := newFakePipeline()
	p.failRemovals(1, errRemove)
	clk := newFakeClock()
	s := newSender(p, clk)

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)

	p.mustInjectError(t, errPeerGone)
	expectStates(t, s.States(), StateDraining, StateBackoff)

	tm := clk.next(t)
	if tm.d != BackoffLadder[0] {
		t.Fatalf("waited %v, want the first rung %v; a failed teardown must not disturb the ladder", tm.d, BackoffLadder[0])
	}
	tm.fire()

	// And it reconnects.
	expectStates(t, s.States(), StateConnecting, StateConnected)

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	expectState(t, s.States(), StateStopped)
	expectClosed(t, s.States())

	// Carrying on is right; carrying on silently is not. A sink that will not
	// leave the pipeline is exactly the thing an engineer needs to see after a
	// match that half worked.
	if text := logs.text(); !strings.Contains(text, errRemove.Error()) {
		t.Fatalf("the teardown failure was swallowed; the log says:\n%s", text)
	}
}

// ---------------------------------------------------------------------------
// errors arriving at awkward moments
// ---------------------------------------------------------------------------

// The two tests below are one property seen from both sides, and the thing that
// separates them is WHEN the error is queued relative to ReplaceSink. Before the
// call there is no sink installed — DRAINING removed it before the backoff wait
// — so a queued message is stale and must be discarded. After the call returns
// nil the gate is open and buffers are on the wire, so a queued message belongs
// to the new sink and must be obeyed. A drain on the wrong side of that call
// fails exactly one of these two, which is the point of having both.

// TestStaleErrorQueuedBeforeTheSinkSwapIsDiscarded is the test that stops a
// recovered network from flapping every seven seconds.
//
// The stale message is real rather than theoretical. DRAINING drives the old
// srtsink to NULL and removes it, and srtq's own flow error follows it onto the
// asynchronous Errors channel; the backoff wait absorbs the bulk of that burst,
// but the wait ends at a moment nobody chooses and anything arriving between
// sleep returning and the swap is still queued when the reconnect succeeds.
// Acting on it tears down a session that has just come up: green lamp, amber
// lamp, seven second wait — and since attempt is zeroed by every successful
// connect the ladder restarts at its first rung each time, so the flap sustains
// itself for the rest of the match.
//
// The error is placed on the machine's own queue from the hook that runs
// immediately before the second CONNECTING is published — on the state-machine
// goroutine, after the backoff wait has already returned and before the swap. No
// timing, no race, no grace period, and no way for sleep to absorb it and make
// the test pass for the wrong reason.
func TestStaleErrorQueuedBeforeTheSinkSwapIsDiscarded(t *testing.T) {
	p := newFakePipeline()
	clk := newFakeClock()
	s := newSender(p, clk)

	var (
		mu         sync.Mutex
		connecting int
		drainings  int
		pending    = -1
	)
	s.hook = func(st State) {
		mu.Lock()
		defer mu.Unlock()
		switch st {
		case StateConnecting:
			connecting++
			if connecting == 2 {
				s.errs <- errPeerGone
			}
		case StateConnected:
			if connecting >= 2 && pending < 0 {
				pending = len(s.errs)
			}
		case StateDraining:
			drainings++
		}
	}

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)

	// A genuine outage first, so that the second connect is a real reconnect
	// with a real removed sink behind it and the injected message is stale in
	// the way the production one is.
	p.mustInjectError(t, errPeerGone)
	expectStates(t, s.States(), StateDraining, StateBackoff)
	clk.next(t).fire()

	expectStates(t, s.States(), StateConnecting, StateConnected)

	mu.Lock()
	gotPending := pending
	mu.Unlock()
	if gotPending != 0 {
		t.Fatalf("%d stale error(s) were still queued when the reconnect was announced; "+
			"a message about the sink DRAINING already removed is about to tear down its replacement", gotPending)
	}

	// And from the outside: CONNECTED is not just announced, it stays.
	expectNoBackoff(t, clk, 100*time.Millisecond)

	mu.Lock()
	gotDrainings := drainings
	mu.Unlock()
	if gotDrainings != 1 {
		t.Fatalf("the machine entered DRAINING %d times, want 1; the stale error was acted on", gotDrainings)
	}
	if got := p.snapshot().removeSinks; got != 1 {
		t.Fatalf("RemoveSink called %d times, want 1: only the genuine outage tears a sink down", got)
	}

	// And the drain has not deafened the machine: a failure of the sink that is
	// now installed still works normally.
	p.mustInjectError(t, errPeerGone)
	expectStates(t, s.States(), StateDraining, StateBackoff)
	if tm := clk.next(t); tm.d != BackoffLadder[0] {
		t.Fatalf("waited %v, want the first rung %v", tm.d, BackoffLadder[0])
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestNewSinkFailureAroundTheSwapIsNotDiscardedIntoAFalseGreen is the regression
// test for a permanent false green: green lamp, nothing on the wire, no
// reconnect, for the rest of the match.
//
// It is what a drain placed AFTER ReplaceSink produces. gst_cgo.go opens the
// data gate as the last thing it does, so from that instant buffers are hitting
// the new srtsink, and it has already cleared the private route that diverted
// that sink's errors into the call — any error from here on goes to Errors(),
// reaches the sender's queue, and is the only notification there will ever be.
// A drain on this side of the call throws it away, and nothing replaces it:
// onBusMessage closes the gate before delivering, and the gate probe DROPS,
// which returns GST_FLOW_OK, so srtq never takes a bad flow return and no
// further bus error is posted.
//
// The failure is not exotic. An M2L-X listener still holding its one permitted
// peer accepts the socket and fails the first write, which lands in the window
// between the gate opening and CONNECTED being announced — a window that spans
// gst_cgo.go's success log under the process-global log mutex, its unlock, a
// quit check and a full ForceKeyUnit round trip into GStreamer. Both cases below
// are inside it: one with ReplaceSink still in flight, one after it has returned
// nil.
func TestNewSinkFailureAroundTheSwapIsNotDiscardedIntoAFalseGreen(t *testing.T) {
	tests := []struct {
		name string
		// arm prepares the failure before Start and returns the step, if any,
		// that has to run once the machine is in CONNECTING. landed is called
		// with whether the failure genuinely reached the state machine's queue
		// before CONNECTED was announced.
		arm func(p *fakePipeline, s *senderImpl, landed func(bool)) (drive func(t *testing.T))
	}{
		{
			// The old sink is gone by now, so a message arriving mid-handshake
			// can only be the new one refusing the socket, or srtq beneath it.
			name: "posted while ReplaceSink is still in flight",
			arm: func(p *fakePipeline, s *senderImpl, landed func(bool)) func(*testing.T) {
				release := p.gateSinks()
				return func(t *testing.T) {
					t.Helper()
					waitReplaceSinks(t, p, 1)
					p.mustInjectError(t, errPeerGone)
					waitErrsLen(t, s, 1, "the injected error never reached the state machine's queue")
					landed(true)
					release()
				}
			},
		},
		{
			// The exact production shape: ReplaceSink has returned nil, the gate
			// is open, and the sink fails its first write while the sender is
			// away forcing an IDR.
			name: "posted after ReplaceSink returned nil, during the ForceKeyUnit round trip",
			arm: func(p *fakePipeline, s *senderImpl, landed func(bool)) func(*testing.T) {
				var once sync.Once
				p.onForceKeyUnit = func() {
					once.Do(func() {
						// Only the first connect fails this way; the reconnect
						// at the end of the test has to be allowed to succeed.
						landed(p.injectError(errPeerGone) && awaitErrsLen(s, 1))
					})
				}
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newFakePipeline()
			clk := newFakeClock()
			s := newSender(p, clk)

			var (
				mu     sync.Mutex
				queued bool
			)
			landed := func(v bool) {
				mu.Lock()
				defer mu.Unlock()
				queued = v
			}

			drive := tt.arm(p, s, landed)

			if err := s.Start(testOpts()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			expectState(t, s.States(), StateConnecting)
			if drive != nil {
				drive(t)
			}

			// ReplaceSink returned nil, so CONNECTED is announced — and then
			// immediately withdrawn, because the sink behind it has already
			// failed. A machine that stalls here is the false green: this
			// assertion times out rather than passing.
			expectStates(t, s.States(), StateConnected, StateDraining, StateBackoff)

			mu.Lock()
			gotQueued := queued
			mu.Unlock()
			if !gotQueued {
				t.Fatal("the failure never reached the state machine's queue, so this run proved nothing")
			}

			if got := p.snapshot().removeSinks; got != 1 {
				t.Fatalf("RemoveSink called %d times, want 1: the failed sink must be torn out", got)
			}

			// And it reconnects, on the first rung, exactly as a real drop does.
			tm := clk.next(t)
			if tm.d != BackoffLadder[0] {
				t.Fatalf("waited %v, want the first rung %v", tm.d, BackoffLadder[0])
			}
			tm.fire()
			expectStates(t, s.States(), StateConnecting, StateConnected)

			if err := s.Stop(); err != nil {
				t.Fatalf("Stop: %v", err)
			}
		})
	}
}

// TestExtraErrorsDuringOneOutageDoNotAdvanceTheLadder: a second error arriving
// while the machine is already on its way out of CONNECTED must be absorbed. It
// is deposited on the sender's internal queue from inside the DRAINING hook,
// which runs on the state-machine goroutine immediately before DRAINING is
// published — so this is deterministic, not a race the test hopes to win.
//
// The mechanism that absorbs it is the backoff wait, not a drain: sleep selects
// on s.errs alongside the timer, and every path out of DRAINING reaches sleep.
// This test is named and commented for that after the adversarial review found
// its predecessor claiming to cover drainErrors, which it did not — the call it
// was named for could be deleted with the whole suite still green.
func TestExtraErrorsDuringOneOutageDoNotAdvanceTheLadder(t *testing.T) {
	p := newFakePipeline()
	clk := newFakeClock()
	s := newSender(p, clk)

	var once sync.Once
	s.hook = func(st State) {
		if st != StateDraining {
			return
		}
		once.Do(func() {
			// Two more errors from the same failure, exactly as a real bus
			// posts them.
			s.errs <- fmt.Errorf("gst: srtout: write failed")
			s.errs <- fmt.Errorf("gst: srtout: connection lost")
		})
	}

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)

	p.mustInjectError(t, errPeerGone)

	// One lost peer, one DRAINING, one BACKOFF, and the ladder starts at its
	// first rung rather than counting the extra errors as extra attempts.
	expectStates(t, s.States(), StateDraining, StateBackoff)

	tm := clk.next(t)
	if tm.d != BackoffLadder[0] {
		t.Fatalf("waited %v, want %v; the extra errors advanced the ladder", tm.d, BackoffLadder[0])
	}
	tm.fire()

	expectStates(t, s.States(), StateConnecting, StateConnected)

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	expectState(t, s.States(), StateStopped)
	expectClosed(t, s.States())
}

// TestErrorBurstCollapsesToOneTransition is the same idea through the real
// path: several bus errors from one lost peer, delivered by the pipeline and
// forwarded by the watcher, must produce exactly one DRAINING and one BACKOFF.
// The timer is deliberately never fired, so no straggler can be mistaken for a
// second failure.
//
// As above, the absorbing mechanism is the backoff wait. Nothing here exercises
// drainErrors, and the comment saying otherwise was wrong; the test that does
// exercise it is TestStaleErrorQueuedBeforeTheSinkSwapIsDiscarded.
func TestErrorBurstCollapsesToOneTransition(t *testing.T) {
	p := newFakePipeline()
	clk := newFakeClock()
	s := newSender(p, clk)

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)

	for i := 0; i < 3; i++ {
		p.mustInjectError(t, errPeerGone)
	}
	expectStates(t, s.States(), StateDraining, StateBackoff)

	tm := clk.next(t)
	if tm.d != BackoffLadder[0] {
		t.Fatalf("waited %v, want %v", tm.d, BackoffLadder[0])
	}
	if n := clk.armedCount(); n != 0 {
		t.Fatalf("%d extra backoff timers armed; the burst was counted more than once", n)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	rest := drainStates(t, s.States())
	if len(rest) != 1 || rest[0] != StateStopped {
		t.Fatalf("states after BACKOFF = %v, want exactly [STOPPED]", rest)
	}
}

// TestErrorDuringBackoff: an error arriving while there is no sink installed is
// stale by definition. It must neither shorten nor restart the wait, and must
// not be waiting on the queue to fire a spurious DRAINING once the next connect
// succeeds.
func TestErrorDuringBackoff(t *testing.T) {
	p := newFakePipeline()
	p.failSinks(1, errConnectRefused)
	clk := newFakeClock()
	s := newSender(p, clk)

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateBackoff)

	tm := clk.next(t)
	if tm.d != BackoffLadder[0] {
		t.Fatalf("waited %v, want %v", tm.d, BackoffLadder[0])
	}

	// The errors are placed straight onto the state machine's own queue rather
	// than handed to the pipeline. That is deliberate and it is what makes this
	// test deterministic: the send completes before the poll begins, so seeing
	// the queue empty again is proof that the backoff wait consumed them, with
	// no in-flight window for the poll to miss. The watcher hop that a real
	// pipeline error takes first is state-independent — it neither knows nor
	// cares that the machine is in BACKOFF — and is covered end to end by
	// TestReconnectAfterPeerLoss and TestErrorBurstCollapsesToOneTransition.
	s.errs <- errPeerGone
	s.errs <- errPeerGone
	waitErrsLen(t, s, 0, "the backoff wait did not absorb the stale errors")

	if n := clk.armedCount(); n != 0 {
		t.Fatalf("%d extra timers armed; the stale error restarted the wait", n)
	}
	if got := clk.delays(); len(got) != 1 {
		t.Fatalf("delays = %v, want exactly one wait", got)
	}

	tm.fire()
	expectStates(t, s.States(), StateConnecting, StateConnected)

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	rest := drainStates(t, s.States())
	if len(rest) != 1 || rest[0] != StateStopped {
		t.Fatalf("states after CONNECTED = %v, want exactly [STOPPED]; the stale error survived", rest)
	}
}

// waitErrsLen polls the sender's internal error queue until it holds want
// entries, failing with msg if it never does.
func waitErrsLen(t *testing.T, s *senderImpl, want int, msg string) {
	t.Helper()
	if !awaitErrsLen(s, want) {
		t.Fatalf("%s (queue length %d, want %d)", msg, len(s.errs), want)
	}
}

// awaitErrsLen is waitErrsLen without the testing.T, reporting rather than
// failing. It exists because one caller runs on the state-machine goroutine —
// inside a pipeline hook — where t.Fatalf would call runtime.Goexit on the
// goroutine that drives the whole machine, turning a clear assertion failure
// into a hang with no explanation. That caller records the answer and the test
// goroutine asserts on it.
func awaitErrsLen(s *senderImpl, want int) bool {
	deadline := time.Now().Add(testTimeout)
	for {
		if len(s.errs) == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		runtime.Gosched()
	}
}

// ---------------------------------------------------------------------------
// saying why
// ---------------------------------------------------------------------------

// TestOnConnectErrorReportsEveryFailure: retrying forever is the requirement,
// and it does not change. What changes is that the reason stops being discarded.
// The case that matters is a pipeline that has gone permanently fatal — the
// commentator's Dante endpoint unplugged, which internal/gst marks fatal so that
// every subsequent ReplaceSink returns the same error instantly. The sender then
// climbs to the thirty second cap and stays there, and without this the operator
// sees an amber lamp and nothing else for the rest of the match.
func TestOnConnectErrorReportsEveryFailure(t *testing.T) {
	const failures = 4

	logs := captureLog(t)

	p := newFakePipeline()
	p.failSinks(failures, errConnectRefused)
	clk := newFakeClock()
	s := newSender(p, clk)

	var (
		mu   sync.Mutex
		seen []error
	)
	opts := testOpts()
	opts.OnConnectError = func(err error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, err)
	}

	if err := s.Start(opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectState(t, s.States(), StateConnecting)

	for i := 0; i < failures; i++ {
		expectState(t, s.States(), StateBackoff)

		// The report is made before the transition to BACKOFF, so by the time
		// BACKOFF is observed the callback has already run. Nothing here has to
		// wait for it.
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n != i+1 {
			t.Fatalf("after failure %d the callback had been called %d times, want %d", i+1, n, i+1)
		}

		clk.next(t).fire()
		expectState(t, s.States(), StateConnecting)
	}
	expectState(t, s.States(), StateConnected)

	mu.Lock()
	got := append([]error(nil), seen...)
	mu.Unlock()

	if len(got) != failures {
		t.Fatalf("callback called %d times, want %d (the successful connect must not report)", len(got), failures)
	}
	for i, err := range got {
		if !errors.Is(err, errConnectRefused) {
			t.Fatalf("report %d = %v, want it to be %v", i, err, errConnectRefused)
		}
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The log is the half of this that survives a caller which sets no
	// callback, and it is where a support engineer looks after the match. It
	// must carry the reason, the attempt number and the wait, because a reason
	// that never changes while the attempt count climbs is the signature of the
	// permanently fatal pipeline this exists for.
	text := logs.text()
	for _, want := range []string{
		errConnectRefused.Error(),
		"attempt 1", "attempt 4",
		BackoffLadder[0].String(), BackoffLadder[3].String(),
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the log does not mention %q; it says:\n%s", want, text)
		}
	}
}

// TestOnConnectErrorIsNotCalledForPeerLoss draws the line the name draws. Losing
// the peer after a successful connect is not a connection failure: it has its
// own state, the lamp already says so, and reporting it as an error would bury
// the case above — the one failure that never changes and never recovers — in a
// stream of routine drops.
func TestOnConnectErrorIsNotCalledForPeerLoss(t *testing.T) {
	p := newFakePipeline()
	clk := newFakeClock()
	s := newSender(p, clk)

	var (
		mu   sync.Mutex
		seen []error
	)
	opts := testOpts()
	opts.OnConnectError = func(err error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, err)
	}

	if err := s.Start(opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)

	for cycle := 0; cycle < 3; cycle++ {
		p.mustInjectError(t, errPeerGone)
		expectStates(t, s.States(), StateDraining, StateBackoff)
		clk.next(t).fire()
		expectStates(t, s.States(), StateConnecting, StateConnected)
	}

	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("callback called %d times for peer loss, want 0", n)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestNilOnConnectErrorIsFine: the field is optional and every existing caller
// and test leaves it unset. A nil callback must not panic the state machine,
// which runs the only goroutine that can reconnect.
//
// It asserts on the log rather than only on survival, and it has to. The
// recover that contains a panicking callback also contains a nil one, so a
// version with the nil check deleted survives every behavioural assertion —
// proved by mutation — while quietly panicking and recovering on every single
// failed attempt for the majority of callers, who set no callback at all.
// Reading the log is what makes the difference visible.
func TestNilOnConnectErrorIsFine(t *testing.T) {
	logs := captureLog(t)

	p := newFakePipeline()
	p.failSinks(2, errConnectRefused)
	clk := newFakeClock()
	s := newSender(p, clk)

	opts := testOpts()
	if opts.OnConnectError != nil {
		t.Fatal("testOpts set OnConnectError; this test needs it nil")
	}

	if err := s.Start(opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectState(t, s.States(), StateConnecting)
	for i := 0; i < 2; i++ {
		expectState(t, s.States(), StateBackoff)
		clk.next(t).fire()
		expectState(t, s.States(), StateConnecting)
	}
	expectState(t, s.States(), StateConnected)

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if text := logs.text(); strings.Contains(text, "panicked") {
		t.Fatalf("a nil OnConnectError was called and recovered from; it must be checked, not caught:\n%s", text)
	}
}

// TestOnConnectErrorPanicDoesNotKillTheReconnectLoop. The callback belongs to
// WP-8 and reaches the WebView2 event bridge. A panic there is a bug in
// somebody else's code, but Go gives no way to contain a panic raised on this
// goroutine from outside it, so without the recover it would take the whole
// process down — during a match, for the sake of an error message. The reconnect
// must outlive its own reporting.
func TestOnConnectErrorPanicDoesNotKillTheReconnectLoop(t *testing.T) {
	logs := captureLog(t)

	p := newFakePipeline()
	p.failSinks(2, errConnectRefused)
	clk := newFakeClock()
	s := newSender(p, clk)

	opts := testOpts()
	opts.OnConnectError = func(error) { panic("wslcomms: the event bridge is gone") }

	if err := s.Start(opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectState(t, s.States(), StateConnecting)
	for i := 0; i < 2; i++ {
		expectState(t, s.States(), StateBackoff)
		clk.next(t).fire()
		expectState(t, s.States(), StateConnecting)
	}
	expectState(t, s.States(), StateConnected)

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	expectState(t, s.States(), StateStopped)
	expectClosed(t, s.States())

	// Containing it silently would be its own bug: the operator loses the
	// reason for every failure from then on and nothing says why.
	if text := logs.text(); !strings.Contains(text, "panicked") {
		t.Fatalf("the contained panic was not logged; the log says:\n%s", text)
	}
}

// ---------------------------------------------------------------------------
// pipeline-fatal stops the machine; everything else still retries forever
// ---------------------------------------------------------------------------

// The three tests below drive the real internal/gst stub rather than this
// package's fakePipeline, deliberately: the property under test is a CONTRACT
// between the two packages — a ReplaceSink error wrapping gst.ErrPipelineFatal
// latches and stops the sender — and the stub's MarkFatal wraps the sentinel
// exactly as the real build's bus handler does, so these tests break if either
// side of the contract drifts, not just this one.

// TestPipelineFatalStopsTheSenderInsteadOfRetrying is the sender's half of the
// render-endpoint defect fix. The measured failure: the operator selected a
// playback endpoint as the commentary input, wasapi2's buffer failed
// asynchronously after preroll, and the sender retried forever telling them
// the feed "is not connected and is retrying" — a network message about a
// local device fault no reconnect could ever repair. With the fatal latched,
// the sender must report the real reason exactly once and stop: grey STOPPED
// lamp, which is the documented recovery (Stop, New, Start) being asked of
// the operator, instead of amber forever.
func TestPipelineFatalStopsTheSenderInsteadOfRetrying(t *testing.T) {
	logs := captureLog(t)

	p := gst.NewStubPipeline()
	clk := newFakeClock()
	s := newSender(p, clk)

	// The shape of the live error, latched before the first connect attempt —
	// wasapi2 fails the device open asynchronously, so by the time the sender
	// reaches ReplaceSink the pipeline is already marked.
	boom := errors.New("wasapi2: Failed to open device {0.0.0.00000000}.{8678ce58-7e5d-4bd3-b83e-d47ce7bba971}")
	p.MarkFatal(boom)

	var (
		mu   sync.Mutex
		seen []error
	)
	opts := testOpts()
	opts.OnConnectError = func(err error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, err)
	}

	if err := s.Start(opts); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// No BACKOFF between them: the machine goes from its first CONNECTING
	// straight to STOPPED, and the channel closes, exactly as an operator
	// Stop would leave things.
	expectStates(t, s.States(), StateConnecting, StateStopped)
	expectClosed(t, s.States())

	mu.Lock()
	got := append([]error(nil), seen...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("OnConnectError called %d times, want exactly 1", len(got))
	}
	if !errors.Is(got[0], gst.ErrPipelineFatal) || !errors.Is(got[0], boom) {
		t.Fatalf("reported %v, want a wrap of both gst.ErrPipelineFatal and the cause", got[0])
	}

	// The retry loop is provably dead, not merely resting: it never armed a
	// backoff timer, so there is nothing for any number of ladder intervals to
	// wake. The grace period is the same bounded negative wait expectNoBackoff
	// documents — a machine that were still looping would arm its timer within
	// microseconds of the STOPPED it should never have emitted.
	if d := clk.delays(); len(d) != 0 {
		t.Fatalf("backoff waits armed = %v, want none: a fatal must stop, not back off", d)
	}
	expectNoBackoff(t, clk, 100*time.Millisecond)
	c := p.Counters()
	if c.ReplaceSinks != 1 {
		t.Fatalf("ReplaceSink called %d times, want exactly 1: the latch must not be retried", c.ReplaceSinks)
	}
	if c.Stops != 1 {
		t.Fatalf("pipeline Stop called %d times, want 1: the self-stop must run the full shutdown", c.Stops)
	}

	// A self-stopped sender is still safely stoppable: WP-8 tears senders down
	// unconditionally and must not be punished for the machine getting there
	// first.
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop after the self-stop: %v", err)
	}
	if c := p.Counters(); c.ReplaceSinks != 1 {
		t.Fatalf("ReplaceSink count moved to %d after STOPPED, want it flat at 1", c.ReplaceSinks)
	}

	// The log must not promise a retry that is not coming. "retrying in 0s"
	// here would be the original defect reborn one line over. The guard is on
	// the promise form "retrying in", not the bare word — the honest line
	// deliberately says "stopping instead of retrying".
	text := logs.text()
	if !strings.Contains(text, boom.Error()) {
		t.Fatalf("the log does not carry the device failure; it says:\n%s", text)
	}
	if strings.Contains(text, "retrying in") {
		t.Fatalf("the log promises a retry after a fatal stop; it says:\n%s", text)
	}
}

// TestPlainConnectFailureStillRetriesForeverOnTheStub is the regression guard
// on the narrowing above: stopping is reserved for errors wrapping
// gst.ErrPipelineFatal, and a plain connect refusal — M2L-X's re-accept
// refusal window, a genuinely dead network — must still climb the ladder and
// win in the end. Six failures walk 7, 7, 10, 15, 20, 30 seconds and the
// seventh attempt connects. If someone widens the fatal check to "any repeated
// error", this is the test that stops them silencing the commentary over a
// network blip.
func TestPlainConnectFailureStillRetriesForeverOnTheStub(t *testing.T) {
	const failures = 6

	want := []time.Duration{
		7 * time.Second, 7 * time.Second, 10 * time.Second,
		15 * time.Second, 20 * time.Second, 30 * time.Second,
	}
	if len(want) != failures {
		t.Fatalf("test is inconsistent: %d delays for %d failures", len(want), failures)
	}

	p := gst.NewStubPipeline()
	p.FailNextSinks(failures, errConnectRefused)
	clk := newFakeClock()
	s := newSender(p, clk)

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectState(t, s.States(), StateConnecting)

	for i := 0; i < failures; i++ {
		expectState(t, s.States(), StateBackoff)

		tm := clk.next(t)
		if tm.d != want[i] {
			t.Fatalf("attempt %d waited %v, want %v", i, tm.d, want[i])
		}
		tm.fire()

		expectState(t, s.States(), StateConnecting)
	}
	expectState(t, s.States(), StateConnected)

	if c := p.Counters(); c.ReplaceSinks != failures+1 || c.SinksAttached != 1 {
		t.Fatalf("counters = %+v, want %d ReplaceSinks of which exactly the last attached",
			c, failures+1)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	expectState(t, s.States(), StateStopped)
	expectClosed(t, s.States())
}

// TestFatalWhileConnectedTerminatesAtTheNextReplaceSink covers the mid-match
// shape of the same defect: the session is up and green when the capture chain
// dies — the commentator's endpoint invalidated under a live connection. The
// bus error knocks the machine out of CONNECTED exactly as a peer loss would,
// and that exit is allowed to look like one: DRAINING, one first-rung BACKOFF,
// CONNECTING. The distinction is made where it becomes knowable — the next
// ReplaceSink, which returns the latched fatal — and the machine must stop
// there rather than settle into the eternal amber loop.
func TestFatalWhileConnectedTerminatesAtTheNextReplaceSink(t *testing.T) {
	p := gst.NewStubPipeline()
	clk := newFakeClock()
	s := newSender(p, clk)

	var (
		mu   sync.Mutex
		seen []error
	)
	opts := testOpts()
	opts.OnConnectError = func(err error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, err)
	}

	if err := s.Start(opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)

	// The capture chain dies: the real bus handler latches the fatal and
	// delivers the same error asynchronously, so the stub is driven both ways
	// in that order — latch first, then the wake-up on the Errors channel.
	boom := errors.New("wasapi2: device invalidated mid-session")
	p.MarkFatal(boom)
	if !p.InjectError(boom) {
		t.Fatalf("InjectError(%v) was dropped", boom)
	}

	expectStates(t, s.States(), StateDraining, StateBackoff)

	tm := clk.next(t)
	if tm.d != BackoffLadder[0] {
		t.Fatalf("waited %v after the drop, want the first rung %v", tm.d, BackoffLadder[0])
	}
	tm.fire()

	// The reconnect attempt finds the latch and the machine stops instead of
	// announcing CONNECTED or re-entering BACKOFF.
	expectStates(t, s.States(), StateConnecting, StateStopped)
	expectClosed(t, s.States())

	mu.Lock()
	got := append([]error(nil), seen...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("OnConnectError called %d times, want exactly 1: the peer-loss exit from "+
			"CONNECTED must not report, and the fatal must report once", len(got))
	}
	if !errors.Is(got[0], gst.ErrPipelineFatal) || !errors.Is(got[0], boom) {
		t.Fatalf("reported %v, want a wrap of both gst.ErrPipelineFatal and the cause", got[0])
	}

	// One backoff for the drop out of CONNECTED, and none after the fatal was
	// found: terminating at the ReplaceSink, not looping past it.
	if d := clk.delays(); len(d) != 1 {
		t.Fatalf("backoff waits armed = %v, want exactly the one that followed the drop", d)
	}
	c := p.Counters()
	if c.ReplaceSinks != 2 {
		t.Fatalf("ReplaceSink called %d times, want 2: the connect and the attempt that found the latch", c.ReplaceSinks)
	}
	if c.Stops != 1 {
		t.Fatalf("pipeline Stop called %d times, want 1", c.Stops)
	}
}

// ---------------------------------------------------------------------------
// Stop, from everywhere
// ---------------------------------------------------------------------------

// stopOnState arranges for Stop to be called from another goroutine the moment
// the machine publishes target, and for the machine not to move on until Stop
// has actually closed its quit channel. That makes "Stop was called while the
// sender was in state X" exact for every state including the momentary
// DRAINING, with no sleeps and nothing left to chance.
func stopOnState(t *testing.T, s *senderImpl, target State) <-chan error {
	t.Helper()
	result := make(chan error, 1)
	var once sync.Once

	s.hook = func(st State) {
		if st != target {
			return
		}
		once.Do(func() {
			go func() { result <- s.Stop() }()
			for !s.quitting() {
				runtime.Gosched()
			}
		})
	}
	return result
}

// awaitStop fails unless Stop returns promptly and without error.
func awaitStop(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Stop did not return")
	}
}

// TestStopFromEveryState walks the machine into each of CONNECTING, CONNECTED,
// DRAINING and BACKOFF and stops it there. In every case Stop must return
// promptly, the pipeline must be stopped once, STOPPED must be the final state,
// and the channel must be closed exactly once.
func TestStopFromEveryState(t *testing.T) {
	tests := []struct {
		name string
		// target is the state Stop is called from.
		target State
		// arm prepares the fake before Start and returns the step, if any, that
		// has to run once the machine is under way. It is the same shape
		// TestNewSinkFailureAroundTheSwapIsNotDiscardedIntoAFalseGreen uses, for
		// the same reason: a case that has to meet the machine at a
		// synchronisation point cannot do all of its work before Start.
		arm func(p *fakePipeline) (drive func(t *testing.T, s *senderImpl))
		// want is the full sequence of states expected, STOPPED included.
		want []State
	}{
		{
			name:   "CONNECTING",
			target: StateConnecting,
			arm:    func(*fakePipeline) func(*testing.T, *senderImpl) { return nil },
			want:   []State{StateConnecting, StateStopped},
		},
		{
			name:   "CONNECTED",
			target: StateConnected,
			arm:    func(*fakePipeline) func(*testing.T, *senderImpl) { return nil },
			want:   []State{StateConnecting, StateConnected, StateStopped},
		},
		{
			name:   "DRAINING",
			target: StateDraining,
			arm: func(p *fakePipeline) func(*testing.T, *senderImpl) {
				// The peer disappears once the session is up, and the hook
				// stopOnState installed fires on the DRAINING that follows.
				//
				// Landing that error is the whole difficulty, and it is not a
				// race to be won by spinning. CONNECTING drains s.errs
				// immediately before ReplaceSink, so anything injected before
				// that point is discarded as stale by design — correctly, since
				// no sink exists yet to have failed. DRAINING itself is now a
				// RemoveSink and straight on, so there is no dwell there to aim
				// at either. Gating ForceKeyUnit parks the machine between the
				// two, after the drain and before CONNECTED is announced, and
				// holds it there while this test injects, confirms the error
				// reached the machine's own queue, and only then lets go. Every
				// step is an ordering the machine cannot get past, so DRAINING
				// is reached on every run rather than on most of them.
				release := p.gateForceKeyUnits()
				return func(t *testing.T, s *senderImpl) {
					t.Helper()
					waitForceKeyUnits(t, p, 1)
					p.mustInjectError(t, errPeerGone)
					waitErrsLen(t, s, 1, "the injected error never reached the state machine's queue")
					release()
				}
			},
			want: []State{StateConnecting, StateConnected, StateDraining, StateStopped},
		},
		{
			name:   "BACKOFF",
			target: StateBackoff,
			arm: func(p *fakePipeline) func(*testing.T, *senderImpl) {
				p.failSinks(1, errConnectRefused)
				return nil
			},
			want: []State{StateConnecting, StateBackoff, StateStopped},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newFakePipeline()
			clk := newFakeClock()
			s := newSender(p, clk)

			result := stopOnState(t, s, tt.target)
			drive := tt.arm(p)

			if err := s.Start(testOpts()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if drive != nil {
				drive(t, s)
			}

			awaitStop(t, result)

			expectStates(t, s.States(), tt.want...)
			expectClosed(t, s.States())

			if got := p.snapshot(); got.stops != 1 {
				t.Fatalf("pipeline Stop called %d times, want 1", got.stops)
			}

			// Idempotent in effect: no second close, no panic, same answer.
			if err := s.Stop(); err != nil {
				t.Fatalf("second Stop: %v", err)
			}
			expectClosed(t, s.States())
		})
	}
}

// TestStopFromStoppedState covers the fifth state: Stop on a Sender that has
// already stopped, called several times, must be harmless.
func TestStopFromStoppedState(t *testing.T) {
	p := newFakePipeline()
	s := newSender(p, newFakeClock())

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)

	for i := 0; i < 5; i++ {
		if err := s.Stop(); err != nil {
			t.Fatalf("Stop %d: %v", i, err)
		}
	}
	expectState(t, s.States(), StateStopped)
	expectClosed(t, s.States())

	if got := p.snapshot(); got.stops != 1 {
		t.Fatalf("pipeline Stop called %d times, want 1", got.stops)
	}
}

// TestConcurrentStopClosesOnce hammers Stop from many goroutines at once. A
// double close or a send on a closed channel would panic and fail the test.
func TestConcurrentStopClosesOnce(t *testing.T) {
	const callers = 16

	p := newFakePipeline()
	s := newSender(p, newFakeClock())

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = s.Stop()
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Stop from caller %d: %v", i, err)
		}
	}
	if got := p.snapshot(); got.stops != 1 {
		t.Fatalf("pipeline Stop called %d times, want 1", got.stops)
	}
	expectState(t, s.States(), StateStopped)
	expectClosed(t, s.States())
}

// TestStopIsPromptOnTheRealClock is the test that a bare time.Sleep would fail.
// It runs on the production clock, so the wait the sender arms is a real seven
// seconds; Stop must cut through it in a small fraction of that.
func TestStopIsPromptOnTheRealClock(t *testing.T) {
	p := newFakePipeline()
	p.failSinks(1, errConnectRefused)

	s := New(p).(*senderImpl)

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateBackoff)

	begin := time.Now()
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(begin)

	// The rung being cut short is BackoffLadder[0] = 7 s. One second is a
	// generous bound for a channel close on a loaded CI machine and still two
	// orders of magnitude away from the wait it must not have served.
	if elapsed > time.Second {
		t.Fatalf("Stop from mid-backoff took %v; it must cancel the %v wait, not serve it",
			elapsed, BackoffLadder[0])
	}
	expectState(t, s.States(), StateStopped)
	expectClosed(t, s.States())
}

// TestStopDuringBlockedReplaceSink documents the one wait Stop cannot shorten.
// ReplaceSink is synchronous by contract and the frozen gst.Pipeline offers no
// way to cancel an SRT handshake in flight, so Stop waits for it and then
// unwinds cleanly rather than racing the pipeline.
func TestStopDuringBlockedReplaceSink(t *testing.T) {
	p := newFakePipeline()
	release := p.gateSinks()

	s := newSender(p, newFakeClock())
	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectState(t, s.States(), StateConnecting)

	// Wait until the machine is genuinely parked inside ReplaceSink.
	deadline := time.After(testTimeout)
	for p.snapshot().replaceSinks == 0 {
		select {
		case <-deadline:
			t.Fatal("the sender never called ReplaceSink")
		default:
			runtime.Gosched()
		}
	}

	done := make(chan error, 1)
	go func() { done <- s.Stop() }()

	select {
	case err := <-done:
		t.Fatalf("Stop returned (%v) while ReplaceSink was still in flight", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Stop did not return after ReplaceSink completed")
	}

	// The successful connect is not announced: quit was already closed.
	expectState(t, s.States(), StateStopped)
	expectClosed(t, s.States())
}

// ---------------------------------------------------------------------------
// lifecycle errors
// ---------------------------------------------------------------------------

// TestStartTwice and the cases around it pin the two exported errors.
func TestStartTwice(t *testing.T) {
	p := newFakePipeline()
	s := newSender(p, newFakeClock())

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := s.Start(testOpts()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start = %v, want ErrAlreadyStarted", err)
	}
	if got := p.snapshot().starts; got != 1 {
		t.Fatalf("pipeline Start called %d times, want 1", got)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// A stopped Sender cannot be restarted; New must be called again.
	if err := s.Start(testOpts()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("Start after Stop = %v, want ErrAlreadyStarted", err)
	}
	if got := p.snapshot().starts; got != 1 {
		t.Fatalf("pipeline Start called %d times after a restart attempt, want 1", got)
	}
}

// TestStopBeforeStart: stopping something that was never started is a caller
// error, and must not close the states channel or touch the pipeline.
func TestStopBeforeStart(t *testing.T) {
	p := newFakePipeline()
	s := newSender(p, newFakeClock())

	for i := 0; i < 3; i++ {
		if err := s.Stop(); !errors.Is(err, ErrNotStarted) {
			t.Fatalf("Stop %d = %v, want ErrNotStarted", i, err)
		}
	}
	if got := p.snapshot(); got.stops != 0 || got.starts != 0 {
		t.Fatalf("counts = %+v, want the pipeline untouched", got)
	}

	select {
	case st, ok := <-s.States():
		t.Fatalf("states channel produced (%v, %v) before Start", st, ok)
	default:
	}
}

// TestStatesIsUsableBeforeStart: WP-8 subscribes to the event stream and then
// starts, so States must be valid on a Sender that has never run.
func TestStatesIsUsableBeforeStart(t *testing.T) {
	s := newSender(newFakePipeline(), newFakeClock())
	if s.States() == nil {
		t.Fatal("States() returned nil before Start")
	}
	if s.States() != s.States() {
		t.Fatal("States() returned a different channel on each call")
	}
}

// TestNewReturnsAWorkingSender checks the exported constructor, which is what
// WP-8 calls, rather than only the internal one the other tests use.
func TestNewReturnsAWorkingSender(t *testing.T) {
	p := newFakePipeline()
	var s Sender = New(p)

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	expectState(t, s.States(), StateStopped)
	expectClosed(t, s.States())
}

// ---------------------------------------------------------------------------
// the slow consumer
// ---------------------------------------------------------------------------

// TestSlowConsumerDoesNotStallTheReconnectLoop: the states channel feeds a
// WebView2 renderer. If that renderer stalls — and a browser engine under load
// does stall — the sender must keep reconnecting regardless. Nothing reads the
// channel here at all until the very end.
func TestSlowConsumerDoesNotStallTheReconnectLoop(t *testing.T) {
	// Comfortably more transitions than the channel can hold: each failed
	// attempt emits BACKOFF and CONNECTING.
	const failures = statesBuffer * 3

	p := newFakePipeline()
	p.failSinks(failures, errConnectRefused)
	clk := newFakeClock()
	s := newSender(p, clk)

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for i := 0; i < failures; i++ {
		clk.next(t).fire()
	}

	// The loop reached the successful attempt despite nobody reading.
	deadline := time.After(testTimeout)
	for p.snapshot().forceKeyUnits == 0 {
		select {
		case <-deadline:
			t.Fatal("the reconnect loop stalled behind the unread states channel")
		default:
			runtime.Gosched()
		}
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	got := drainStates(t, s.States())
	if len(got) == 0 {
		t.Fatal("no states were retained at all")
	}
	if len(got) > statesBuffer {
		t.Fatalf("retained %d states, want at most the buffer depth %d", len(got), statesBuffer)
	}
	if last := got[len(got)-1]; last != StateStopped {
		t.Fatalf("last retained state = %s, want %s; the consumer must always converge on the current state",
			last, StateStopped)
	}
}

// TestEmitDropsOldestWhenFull exercises the drop policy directly, including the
// case where the buffer is exactly full, which is the boundary the loop in emit
// has to get right.
func TestEmitDropsOldestWhenFull(t *testing.T) {
	s := newSender(newFakePipeline(), newFakeClock())

	// Fill the buffer, then overflow it by half again.
	const overflow = statesBuffer / 2
	for i := 0; i < statesBuffer; i++ {
		s.emit(StateConnecting)
	}
	for i := 0; i < overflow; i++ {
		s.emit(StateBackoff)
	}

	if got := len(s.states); got != statesBuffer {
		t.Fatalf("buffered %d states, want %d", got, statesBuffer)
	}

	// The oldest CONNECTINGs were discarded and the newest BACKOFFs kept.
	var connecting, backoff int
	for i := 0; i < statesBuffer; i++ {
		switch <-s.states {
		case StateConnecting:
			connecting++
		case StateBackoff:
			backoff++
		}
	}
	if backoff != overflow {
		t.Fatalf("kept %d BACKOFF states, want the %d most recent", backoff, overflow)
	}
	if connecting != statesBuffer-overflow {
		t.Fatalf("kept %d CONNECTING states, want %d", connecting, statesBuffer-overflow)
	}
}

// ---------------------------------------------------------------------------
// goroutines
// ---------------------------------------------------------------------------

// TestNoGoroutineLeak: WP-8 may build and stop a Sender several times during a
// match as the operator changes capture device. A stopped Sender must leave
// nothing of itself running.
func TestNoGoroutineLeak(t *testing.T) {
	settle := func() {
		for i := 0; i < 200; i++ {
			runtime.Gosched()
			time.Sleep(time.Millisecond)
		}
	}

	settle()
	base := runtime.NumGoroutine()

	for i := 0; i < 5; i++ {
		p := newFakePipeline()
		p.failSinks(3, errConnectRefused)
		clk := newFakeClock()
		s := newSender(p, clk)

		if err := s.Start(testOpts()); err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
		for j := 0; j < 3; j++ {
			clk.next(t).fire()
		}
		expectStates(t, s.States(),
			StateConnecting,
			StateBackoff, StateConnecting,
			StateBackoff, StateConnecting,
			StateBackoff, StateConnecting,
			StateConnected,
		)
		if err := s.Stop(); err != nil {
			t.Fatalf("Stop %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(testTimeout)
	for {
		settle()
		now := runtime.NumGoroutine()
		if now <= base {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines went from %d to %d across five sender lifetimes", base, now)
		}
	}
}

// TestWatcherSurvivesAPipelineThatNeverClosesItsErrorChannel: the gst.Pipeline
// contract says Stop closes the channel Errors handed out, but the error
// watcher must not depend on a third party keeping that promise, because the
// consequence of a broken promise would be a Stop that never returns.
func TestWatcherSurvivesAPipelineThatNeverClosesItsErrorChannel(t *testing.T) {
	p := &rudePipeline{errs: make(chan error)}
	s := newSender(p, newFakeClock())

	if err := s.Start(testOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)

	done := make(chan error, 1)
	go func() { done <- s.Stop() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Stop hung on a pipeline that never closed its error channel")
	}
	expectState(t, s.States(), StateStopped)
	expectClosed(t, s.States())
}

// rudePipeline is a pipeline that violates the contract by never closing the
// channel Errors returned. Everything else about it succeeds.
type rudePipeline struct{ errs chan error }

func (p *rudePipeline) Start(gst.PipelineOpts) error   { return nil }
func (p *rudePipeline) ReplaceSink(gst.SinkOpts) error { return nil }
func (p *rudePipeline) RemoveSink() error              { return nil }
func (p *rudePipeline) ForceKeyUnit() error            { return nil }

// The channel-map half of gst.Pipeline. internal/sender never touches routing —
// it reconnects a sink and nothing else — so these are the inert answers of a
// pipeline with a positioned capture device: no negotiated width to report, and
// no matrix to change.
func (p *rudePipeline) InputChannels() int                 { return 0 }
func (p *rudePipeline) SetChannelMap(gst.ChannelMap) error { return nil }

// The cough mute, inert for the same reason: internal/sender never mutes
// anything, and a reconnect must not go near the audio leg at all.
func (p *rudePipeline) SetCommentaryMute(bool) error { return nil }
func (p *rudePipeline) CommentaryMuted() bool        { return false }

func (p *rudePipeline) Errors() <-chan error { return p.errs }
func (p *rudePipeline) Stop() error          { return nil }

var _ gst.Pipeline = (*rudePipeline)(nil)
