package sender

import (
	"errors"
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
// fake pipeline
// ---------------------------------------------------------------------------

// fakeCounts is a snapshot of what a fakePipeline has been asked to do.
type fakeCounts struct {
	starts        int
	replaceSinks  int
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

	startErr error
	stopErr  error

	// sinkResults are handed to successive ReplaceSink calls in order. Once
	// exhausted, sinkDefault is returned for every further call.
	sinkResults []error
	sinkDefault error

	// sinkGate, when non-nil, is received from by ReplaceSink before it returns.
	// It is how a test parks the machine inside the one synchronous call it
	// makes.
	sinkGate chan struct{}

	forceKeyUnitErr error

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
	p.pipeOpts = opts
	return p.startErr
}

// ReplaceSink records the options, pops the next queued result, and — if a gate
// is installed — blocks until the test releases it. The lock is released before
// blocking so that Stop and the counter accessors stay usable while the call is
// parked.
func (p *fakePipeline) ReplaceSink(opts gst.SinkOpts) error {
	p.mu.Lock()
	p.counts.replaceSinks++
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

// ForceKeyUnit records the request.
func (p *fakePipeline) ForceKeyUnit() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts.forceKeyUnits++
	return p.forceKeyUnitErr
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

// fakeClockTimerBuffer is how many armed timers can queue up before a test that
// is not consuming them would block. No test here arms more than a hundred.
const fakeClockTimerBuffer = 256

// fakeClock is the injected clock. Every backoff wait becomes an entry on
// armed, which a test can inspect for its exact duration and then fire, so the
// 7/7/10/15/20/30 ladder is asserted precisely and runs in microseconds.
type fakeClock struct {
	mu    sync.Mutex
	delay []time.Duration

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
	c.mu.Unlock()

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
