package sender

import (
	"errors"
	"fmt"
	"runtime"
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
// errors arriving at awkward moments
// ---------------------------------------------------------------------------

// TestErrorWhileDraining: a second error arriving while the machine is already
// in DRAINING must be absorbed. It is deposited on the sender's internal queue
// from inside the DRAINING hook, which runs on the state-machine goroutine
// immediately before DRAINING is published and therefore strictly before the
// drain — so this is deterministic, not a race the test hopes to win.
func TestErrorWhileDraining(t *testing.T) {
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
	deadline := time.After(testTimeout)
	for {
		if len(s.errs) == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("%s (queue length %d, want %d)", msg, len(s.errs), want)
		default:
			runtime.Gosched()
		}
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
		// setup drives the machine towards target.
		setup func(t *testing.T, p *fakePipeline, clk *fakeClock)
		// want is the full sequence of states expected, STOPPED included.
		want []State
	}{
		{
			name:   "CONNECTING",
			target: StateConnecting,
			setup:  func(*testing.T, *fakePipeline, *fakeClock) {},
			want:   []State{StateConnecting, StateStopped},
		},
		{
			name:   "CONNECTED",
			target: StateConnected,
			setup:  func(*testing.T, *fakePipeline, *fakeClock) {},
			want:   []State{StateConnecting, StateConnected, StateStopped},
		},
		{
			name:   "DRAINING",
			target: StateDraining,
			setup: func(t *testing.T, p *fakePipeline, _ *fakeClock) {
				// The peer disappears once the session is up. The hook fires on
				// the DRAINING that follows.
				go func() {
					deadline := time.After(testTimeout)
					for {
						if p.injectError(errPeerGone) {
							return
						}
						select {
						case <-deadline:
							return
						default:
							runtime.Gosched()
						}
					}
				}()
			},
			want: []State{StateConnecting, StateConnected, StateDraining, StateStopped},
		},
		{
			name:   "BACKOFF",
			target: StateBackoff,
			setup: func(_ *testing.T, p *fakePipeline, _ *fakeClock) {
				p.failSinks(1, errConnectRefused)
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
			tt.setup(t, p, clk)

			if err := s.Start(testOpts()); err != nil {
				t.Fatalf("Start: %v", err)
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
func (p *rudePipeline) ForceKeyUnit() error            { return nil }
func (p *rudePipeline) Errors() <-chan error           { return p.errs }
func (p *rudePipeline) Stop() error                    { return nil }

var _ gst.Pipeline = (*rudePipeline)(nil)
