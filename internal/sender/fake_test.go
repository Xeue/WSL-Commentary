package sender

import (
	"errors"
	"io"
	"log"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"wslcomms/internal/gst"
)

// testTimeout is how long a helper waits for something the state machine should
// produce essentially immediately. Every wait in these tests is on a fake clock
// or a channel, so this bound is only ever hit by a genuine hang, and a hang is
// the failure mode worth catching: the sender must never wedge.
const testTimeout = 5 * time.Second

// errPeerGone stands in for the GST_ELEMENT_ERROR that srtout posts when the
// SRT peer disappears.
var errPeerGone = errors.New("gst: srtout: connection lost")

// errConnectRefused stands in for a synchronous ReplaceSink failure — the SRT
// caller handshake being refused, which during a match is M2L-X's roughly five
// second re-accept refusal window.
var errConnectRefused = errors.New("gst: srtsink: connection refused")

// ---------------------------------------------------------------------------
// the ordered call log
// ---------------------------------------------------------------------------

// callLog is an ordered record of what the state machine did, shared between the
// fake pipeline and the fake clock.
//
// Counters alone cannot express the property specification section 6.2 is
// actually about, which is an ORDER: the sink must be gone before the backoff
// wait starts, not merely gone by the time the wait ends. Both fakes append to
// one log so that "removeSink" and "wait 7s" appear in a single sequence that
// can be asserted literally. Every append happens on the state-machine
// goroutine, which is the only caller of either fake's recorded methods, so the
// order recorded is the order of events with no interleaving to reason about.
type callLog struct {
	mu     sync.Mutex
	events []string
}

// add appends one event. It is nil-safe so that a fake built without a log costs
// nothing.
func (l *callLog) add(ev string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
}

// all returns a copy of the events recorded so far.
func (l *callLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// newRecorded returns a pipeline and a clock that share one ordered log.
func newRecorded() (*fakePipeline, *fakeClock, *callLog) {
	l := &callLog{}
	p := newFakePipeline()
	p.log = l
	clk := newFakeClock()
	clk.log = l
	return p, clk, l
}

// expectEvents fails unless the log holds exactly want, in order.
func expectEvents(t *testing.T, l *callLog, want ...string) {
	t.Helper()
	got := l.all()
	if len(got) != len(want) {
		t.Fatalf("call sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call sequence = %v, want %v (first difference at %d)", got, want, i)
		}
	}
}

// ---------------------------------------------------------------------------
// fake pipeline
// ---------------------------------------------------------------------------

// fakeCounts is a snapshot of what a fakePipeline has been asked to do.
type fakeCounts struct {
	starts        int
	replaceSinks  int
	removeSinks   int
	forceKeyUnits int
	stops         int
}

// fakePipeline is a gst.Pipeline the tests can drive precisely.
//
// It is written here rather than reused from internal/gst deliberately. The
// stub in internal/gst is owned by WP-3a and models the contract for the
// application; what the state machine's tests need is something whose every
// call can be made to fail, block or succeed on cue, and whose call order can be
// asserted. Sharing one fake between the two purposes would make both worse.
//
// All methods are safe for concurrent use, because the sender calls Errors and
// ReplaceSink from the state-machine goroutine and Stop from that same
// goroutine while a test goroutine reads the counters.
type fakePipeline struct {
	mu sync.Mutex

	// log, when set, records each call in order alongside the fake clock's
	// waits. See callLog.
	log *callLog

	startErr   error
	stopErr    error
	removeErr  error
	removeErrN int

	// muted is the cough mute this fake was last told to set. Nothing in the
	// reconnect machine writes it; see SetCommentaryMute.
	muted bool

	// sinkResults are handed to successive ReplaceSink calls in order. Once
	// exhausted, sinkDefault is returned for every further call.
	sinkResults []error
	sinkDefault error

	// sinkGate, when non-nil, is received from by ReplaceSink before it returns.
	// It is how a test parks the machine inside the one synchronous call it
	// makes.
	sinkGate chan struct{}

	forceKeyUnitErr error

	// onForceKeyUnit, when set, is called by ForceKeyUnit with the fake's lock
	// released. It is the seam for the window that matters most on the connect
	// path: ForceKeyUnit is a full round trip into GStreamer, it happens after
	// ReplaceSink has returned nil and opened the data gate, and it happens
	// before the sender announces CONNECTED. A new sink that accepts the socket
	// and fails its first write posts its error right there, and this is how a
	// test puts one there deterministically.
	onForceKeyUnit func()

	// keyUnitGate, when non-nil, is received from by ForceKeyUnit before it
	// returns. It is gateSinks' counterpart one call further along, and it
	// parks the state machine in the only window where an injected error is
	// certain to be honoured rather than merely likely to be.
	//
	// CONNECTING drains s.errs immediately before ReplaceSink, so an error
	// injected earlier than that is discarded as stale by design; the CONNECTED
	// select consumes the first error it finds and nothing drains after the
	// swap, so an error injected here cannot be missed. Between those two
	// points sits exactly one call the fake can hold the machine inside, and
	// this is it. onForceKeyUnit runs the injection on the state-machine
	// goroutine, which suits a test that only needs a side effect; the gate
	// hands control to the TEST goroutine instead, which is what a test needs
	// when it must assert — t.Fatalf on the machine's goroutine calls
	// runtime.Goexit on the goroutine driving the machine and turns a clear
	// failure into a hang.
	keyUnitGate chan struct{}

	errs   chan error
	closed bool

	counts   fakeCounts
	pipeOpts gst.PipelineOpts
	sinkOpts []gst.SinkOpts
}

// fakeErrorBuffer matches the "drop, never block" discipline the gst.Pipeline
// contract requires of the real bus watcher.
const fakeErrorBuffer = 8

// newFakePipeline returns a pipeline whose Start succeeds, whose ReplaceSink
// succeeds, and which posts no asynchronous errors.
func newFakePipeline() *fakePipeline {
	return &fakePipeline{errs: make(chan error, fakeErrorBuffer)}
}

// Start records the options and returns the queued start error, if any.
func (p *fakePipeline) Start(opts gst.PipelineOpts) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts.starts++
	p.log.add("start")
	p.pipeOpts = opts
	return p.startErr
}

// RemoveSink tears the sink out without installing another, as gst.Pipeline
// requires of DRAINING. It is idempotent — a removal with nothing attached is
// not an error — so the fake, like the real thing, does not care whether a sink
// was ever installed; it counts the call, which is the thing under test.
func (p *fakePipeline) RemoveSink() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts.removeSinks++
	p.log.add("removeSink")

	if p.removeErrN > 0 {
		p.removeErrN--
		return p.removeErr
	}
	return nil
}

// failRemovals makes the next n RemoveSink calls return err. It is how the
// "a teardown failure must still reach BACKOFF" path is driven.
func (p *fakePipeline) failRemovals(n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removeErrN = n
	p.removeErr = err
}

// ReplaceSink records the options, pops the next queued result, and — if a gate
// is installed — blocks until the test releases it. The lock is released before
// blocking so that Stop and the counter accessors stay usable while the call is
// parked.
func (p *fakePipeline) ReplaceSink(opts gst.SinkOpts) error {
	p.mu.Lock()
	p.counts.replaceSinks++
	p.log.add("replaceSink")
	p.sinkOpts = append(p.sinkOpts, opts)

	err := p.sinkDefault
	if len(p.sinkResults) > 0 {
		err = p.sinkResults[0]
		p.sinkResults = p.sinkResults[1:]
	}
	gate := p.sinkGate
	p.mu.Unlock()

	if gate != nil {
		<-gate
	}
	return err
}

// InputChannels and SetChannelMap are the channel-map half of gst.Pipeline,
// and they are inert here on purpose. internal/sender reconnects a sink; it has
// no opinion about which of a capture device's channels reach the feed, and a
// fake that grew routing state would be modelling a coupling the state machine
// deliberately does not have. The answers are those of a pipeline with a
// positioned capture device: no negotiated width, no matrix to change.
func (p *fakePipeline) InputChannels() int                 { return 0 }
func (p *fakePipeline) SetChannelMap(gst.ChannelMap) error { return nil }

// The cough mute is inert here for the reason the routing above it is: the
// reconnect state machine neither reads it nor writes it. It is recorded rather
// than discarded so that a future test asserting "a reconnect does not touch the
// mute" has something to assert against — and the recording is what makes the
// claim checkable at all, since the real answer to that question is structural
// (nothing in RemoveSink or ReplaceSink goes near the audio leg) rather than
// something the state machine could get right by accident.
func (p *fakePipeline) SetCommentaryMute(mute bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.muted = mute
	return nil
}

func (p *fakePipeline) CommentaryMuted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.muted
}

// ForceKeyUnit records the request, runs onForceKeyUnit if one is installed,
// and then — if a gate is installed — blocks until the test releases it. Both
// happen with the lock released so that either can inject an error —
// injectError takes the same lock — exactly as the real pipeline's bus thread
// would while the sender is parked in this call.
//
// The count is incremented before either, so waitForceKeyUnits observing it
// means the machine is genuinely inside this call and not merely about to enter
// it.
func (p *fakePipeline) ForceKeyUnit() error {
	p.mu.Lock()
	p.counts.forceKeyUnits++
	p.log.add("forceKeyUnit")
	err := p.forceKeyUnitErr
	hook := p.onForceKeyUnit
	gate := p.keyUnitGate
	p.mu.Unlock()

	if hook != nil {
		hook()
	}
	if gate != nil {
		<-gate
	}
	return err
}

// Errors returns the asynchronous error channel, which Stop closes.
func (p *fakePipeline) Errors() <-chan error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.errs
}

// Stop closes the error channel exactly once and is idempotent, as the
// gst.Pipeline contract requires.
func (p *fakePipeline) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts.stops++
	p.log.add("stop")
	if !p.closed {
		p.closed = true
		close(p.errs)
	}
	return p.stopErr
}

// injectError posts err on the asynchronous channel, reporting false if it was
// dropped because the buffer was full or the pipeline was stopped.
func (p *fakePipeline) injectError(err error) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	select {
	case p.errs <- err:
		return true
	default:
		return false
	}
}

// mustInjectError posts err and fails the test if it was dropped.
func (p *fakePipeline) mustInjectError(t *testing.T, err error) {
	t.Helper()
	if !p.injectError(err) {
		t.Fatalf("injectError(%v) was dropped", err)
	}
}

// queueSinkResults sets the results the next len(results) ReplaceSink calls
// return, in order.
func (p *fakePipeline) queueSinkResults(results ...error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sinkResults = append([]error(nil), results...)
}

// failSinks queues n consecutive ReplaceSink failures.
func (p *fakePipeline) failSinks(n int, err error) {
	results := make([]error, n)
	for i := range results {
		results[i] = err
	}
	p.queueSinkResults(results...)
}

// gateSinks installs a gate that ReplaceSink blocks on. The returned function
// releases every call, present and future.
func (p *fakePipeline) gateSinks() (release func()) {
	gate := make(chan struct{})
	p.mu.Lock()
	p.sinkGate = gate
	p.mu.Unlock()

	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

// gateForceKeyUnits installs a gate that ForceKeyUnit blocks on. The returned
// function releases every call, present and future. See keyUnitGate for what
// the window it holds the machine in is worth.
func (p *fakePipeline) gateForceKeyUnits() (release func()) {
	gate := make(chan struct{})
	p.mu.Lock()
	p.keyUnitGate = gate
	p.mu.Unlock()

	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

// snapshot returns the current call counts.
func (p *fakePipeline) snapshot() fakeCounts {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts
}

// sinks returns the options of every ReplaceSink call so far.
func (p *fakePipeline) sinks() []gst.SinkOpts {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]gst.SinkOpts(nil), p.sinkOpts...)
}

// Compile-time assertion that the fake satisfies the contract the sender is
// written against. If WP-3a ever changes gst.Pipeline, this breaks here first.
var _ gst.Pipeline = (*fakePipeline)(nil)

// ---------------------------------------------------------------------------
// fake clock
// ---------------------------------------------------------------------------

// fakeTimer is one armed backoff wait.
type fakeTimer struct {
	d time.Duration
	c chan time.Time

	mu      sync.Mutex
	fired   bool
	stopped bool
}

// fire releases the timer. It is a no-op on a timer that has already fired,
// which keeps a test that fires defensively from blocking.
func (t *fakeTimer) fire() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fired {
		return
	}
	t.fired = true
	t.c <- time.Now()
}

// wasStopped reports whether the sender released this timer.
func (t *fakeTimer) wasStopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped
}

// fakeClockTimerBuffer is how many armed timers can queue up before the state
// machine would block inside NewTimer.
//
// It bounds the machine's run-ahead, not a test's total. The longest run,
// TestNoAttemptLimit, arms five hundred, but it consumes each one before firing
// it and the machine cannot arm the next until that one has fired, so the real
// backlog never exceeds one. The depth is generous purely so that a test which
// fires timers without draining armed cannot deadlock the machine and turn a
// simple assertion failure into a five second hang.
const fakeClockTimerBuffer = 256

// fakeClock is the injected clock. Every backoff wait becomes an entry on
// armed, which a test can inspect for its exact duration and then fire, so the
// 7/7/10/15/20/30 ladder is asserted precisely and runs in microseconds.
type fakeClock struct {
	mu    sync.Mutex
	delay []time.Duration

	// log, when set, records each armed wait in order alongside the fake
	// pipeline's calls. See callLog.
	log *callLog

	armed chan *fakeTimer
}

// newFakeClock returns a clock with no timers armed.
func newFakeClock() *fakeClock {
	return &fakeClock{armed: make(chan *fakeTimer, fakeClockTimerBuffer)}
}

// NewTimer records the requested duration and publishes the timer.
func (c *fakeClock) NewTimer(d time.Duration) (<-chan time.Time, func()) {
	t := &fakeTimer{d: d, c: make(chan time.Time, 1)}

	c.mu.Lock()
	c.delay = append(c.delay, d)
	l := c.log
	c.mu.Unlock()

	l.add("wait " + d.String())

	c.armed <- t

	return t.c, func() {
		t.mu.Lock()
		t.stopped = true
		t.mu.Unlock()
	}
}

// next waits for the sender to arm its next timer.
func (c *fakeClock) next(t *testing.T) *fakeTimer {
	t.Helper()
	select {
	case tm := <-c.armed:
		return tm
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the sender to arm a backoff timer")
		return nil
	}
}

// delays returns every duration the sender has asked to wait, in order.
func (c *fakeClock) delays() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.delay...)
}

// armedCount returns how many timers have been armed and not yet consumed by
// next.
func (c *fakeClock) armedCount() int { return len(c.armed) }

// Compile-time assertion that the fake satisfies the seam.
var _ clock = (*fakeClock)(nil)

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// testOpts is a representative session: the spec's slate and a plausible DVS
// endpoint GUID, sending to a local listener at the default 120 ms SRT latency.
func testOpts() Opts {
	return Opts{
		Pipeline: gst.PipelineOpts{
			SlatePath:     `C:\Program Files\WSLComms\slate.png`,
			AudioDeviceID: "{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}",
		},
		Sink: gst.SinkOpts{
			Host:      "127.0.0.1",
			Port:      9000,
			LatencyMs: gst.DefaultSRTLatencyMs,
		},
	}
}

// TestMain silences the package's logging by default.
//
// The sender logs every failed connection attempt, which is the point of it,
// but TestNoAttemptLimit alone drives five hundred failures and the suite is
// expected to run under -count=50 in the absence of the race detector. Left on
// stderr that is tens of thousands of lines of noise around the one line that
// matters. Tests that assert on the log opt back in with captureLog.
func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// logCapture collects everything the package logs during one test.
//
// It exists because two of the fixes here are only observable in the log: the
// reason a connection attempt failed, and the fact that a caller's callback
// panicked and was contained. Without reading the log those tests would pass
// whether or not the code did anything, which is worse than not having them.
//
// Writes are serialised by the log package's own mutex and by this one, and the
// sender's goroutine is joined by Stop before any test reads the buffer, so
// there is no ordering to reason about.
type logCapture struct {
	mu  sync.Mutex
	buf []byte
}

// Write implements io.Writer for the standard logger.
func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	return len(p), nil
}

// text returns everything logged so far.
func (c *logCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

// captureLog redirects the standard logger for the duration of the test and
// returns the sink. The previous destination and flags are restored by a
// t.Cleanup, so a failure part-way through cannot leave the whole test binary
// silently writing into a dead buffer.
func captureLog(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(c)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return c
}

// waitReplaceSinks spins until the pipeline has been asked to install n sinks.
// With a gate installed that is how a test knows the machine is genuinely
// parked inside the one synchronous call it makes, rather than about to be.
func waitReplaceSinks(t *testing.T, p *fakePipeline, n int) {
	t.Helper()
	deadline := time.After(testTimeout)
	for p.snapshot().replaceSinks < n {
		select {
		case <-deadline:
			t.Fatalf("the sender called ReplaceSink %d times, want %d", p.snapshot().replaceSinks, n)
		default:
			runtime.Gosched()
		}
	}
}

// waitForceKeyUnits spins until the pipeline has been asked to force n key
// units. With a gate installed that is how a test knows the machine is parked
// inside the call, rather than about to enter it.
func waitForceKeyUnits(t *testing.T, p *fakePipeline, n int) {
	t.Helper()
	deadline := time.After(testTimeout)
	for p.snapshot().forceKeyUnits < n {
		select {
		case <-deadline:
			t.Fatalf("the sender called ForceKeyUnit %d times, want %d", p.snapshot().forceKeyUnits, n)
		default:
			runtime.Gosched()
		}
	}
}

// expectNoBackoff fails if the machine arms a backoff timer within grace.
//
// It is the one bounded negative wait in this file and it earns it: the assertion
// is that the machine STAYED in CONNECTED, and staying somewhere produces no
// event to synchronise on. The polarity is favourable — a machine that did back
// off arms its timer within microseconds of the state it was not supposed to
// leave, so the failing direction is immediate and only the passing direction
// pays the grace period. The companion assertion inside the hook, which runs on
// the state-machine goroutine, is the deterministic half.
func expectNoBackoff(t *testing.T, clk *fakeClock, grace time.Duration) {
	t.Helper()
	select {
	case tm := <-clk.armed:
		t.Fatalf("the sender armed a %v backoff; it should still be CONNECTED", tm.d)
	case <-time.After(grace):
	}
}

// expectState reads the next state and fails unless it is want.
func expectState(t *testing.T, ch <-chan State, want State) {
	t.Helper()
	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatalf("states channel closed while waiting for %s", want)
		}
		if got != want {
			t.Fatalf("state = %s, want %s", got, want)
		}
	case <-time.After(testTimeout):
		t.Fatalf("timed out waiting for state %s", want)
	}
}

// expectStates reads a whole sequence of states in order.
func expectStates(t *testing.T, ch <-chan State, want ...State) {
	t.Helper()
	for _, w := range want {
		expectState(t, ch, w)
	}
}

// expectClosed fails unless the channel is closed, and fails rather than hangs
// if it is neither closed nor delivering.
func expectClosed(t *testing.T, ch <-chan State) {
	t.Helper()
	select {
	case got, ok := <-ch:
		if ok {
			t.Fatalf("wanted a closed states channel, got state %s", got)
		}
	case <-time.After(testTimeout):
		t.Fatal("states channel was not closed")
	}
}

// drainStates reads a closed channel to exhaustion and returns what was left in
// the buffer.
func drainStates(t *testing.T, ch <-chan State) []State {
	t.Helper()
	var got []State
	deadline := time.After(testTimeout)
	for {
		select {
		case st, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, st)
		case <-deadline:
			t.Fatal("timed out draining the states channel")
			return got
		}
	}
}
