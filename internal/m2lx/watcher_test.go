package m2lx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeConn is a wsConn double: a queue of frames to hand back from
// ReadMessage, blocking until one is pushed, closed, or the fake
// connection is closed. It makes no real network connection.
type fakeConn struct {
	msgs   chan []byte
	closed chan struct{}
	once   sync.Once
}

func newFakeConn() *fakeConn {
	return &fakeConn{msgs: make(chan []byte, 16), closed: make(chan struct{})}
}

func (c *fakeConn) push(b []byte) {
	select {
	case c.msgs <- b:
	case <-c.closed:
	}
}

func (c *fakeConn) ReadMessage() (int, []byte, error) {
	select {
	case m := <-c.msgs:
		return 1, m, nil
	case <-c.closed:
		return 0, nil, errors.New("fakeConn: closed")
	}
}

func (c *fakeConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// fakeClientToken is a minimal Client double that only needs to answer
// Token(); the Watcher never calls the other four methods.
type fakeClientToken struct {
	mu    sync.Mutex
	token string
}

func (f *fakeClientToken) set(tok string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token = tok
}

func (f *fakeClientToken) Token() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.token
}

func (f *fakeClientToken) SignIn(ctx context.Context, alias, password string) error { return nil }
func (f *fakeClientToken) Refresh(ctx context.Context) error                        { return nil }
func (f *fakeClientToken) KVSInfo(ctx context.Context, eventID string) (KVSInfo, error) {
	return KVSInfo{}, nil
}
func (f *fakeClientToken) KVSToken(ctx context.Context, eventID string) (KVSToken, error) {
	return KVSToken{}, nil
}

// ListEvents satisfies Client. The watcher never lists events, so it is not
// exercised here.
func (f *fakeClientToken) ListEvents(ctx context.Context) ([]Event, error) { return nil, nil }

// SwitcherConfiguration satisfies Client. The watcher reads the status socket,
// never the instance's settings, so it is not exercised here.
func (f *fakeClientToken) SwitcherConfiguration(ctx context.Context) (SwitcherConfiguration, error) {
	return SwitcherConfiguration{}, nil
}

// Close satisfies the Client interface. fakeClientToken starts no
// goroutine of its own, so there is nothing to cancel or wait for.
func (f *fakeClientToken) Close() error { return nil }

// wsConnOrErr is what the test-controlled dialer hands back for one dial
// attempt.
type wsConnOrErr struct {
	conn wsConn
	err  error
}

// testWatcher wires a watcher up to a fake clock and a scripted, in-memory
// dialer so its reconnect/debounce/staleness state machine can be driven
// deterministically with no real network I/O and no real sleeping.
type testWatcher struct {
	w        *watcher
	clk      *fakeClock
	dials    chan string // URL of each dial attempt, in order
	nextConn chan wsConnOrErr
	ticked   chan struct{}
	started  chan struct{}
}

// testSafetyTimeout bounds every "wait for the watcher to do something"
// select in this file. It exists purely to fail a genuinely hung test
// promptly rather than hang the suite forever; it is not a correctness
// parameter (the fake clock, not this timeout, drives what the watcher
// under test actually does). It is deliberately generous — not the ~1s
// that would suffice for this package's channel ops alone — because this
// tree is worked on by several CPU-heavy sibling agents building and
// testing other packages concurrently (see the task brief), and a 5s
// bound was observed to fire spuriously under that contention: a
// goroutine that is merely descheduled for a few real seconds, not
// hung, otherwise panics tick() and fails a test that has nothing wrong
// with it.
const testSafetyTimeout = 20 * time.Second

// testStatusHost is the resolvedHost every testWatcher dials, matching the
// bare "m2lx.example.invalid" every test in this file asserts against.
// resolvedHost's zero-value insecure=false already means https/wss (see
// host.go), so this is the same secure default a bare host from
// resolveHost would have produced.
var testStatusHost = resolvedHost{hostPort: "m2lx.example.invalid"}

func newTestWatcher(client Client) *testWatcher {
	tw := &testWatcher{
		clk:      newFakeClock(base),
		dials:    make(chan string, 64),
		nextConn: make(chan wsConnOrErr, 64),
		ticked:   make(chan struct{}, 64),
		started:  make(chan struct{}),
	}
	tw.w = &watcher{
		status: testStatusHost,
		client: client,
		clk:    tw.clk,
		dial: func(ctx context.Context, urlStr string) (wsConn, error) {
			tw.dials <- urlStr
			r := <-tw.nextConn
			return r.conn, r.err
		},
		afterTick: func() {
			select {
			case tw.ticked <- struct{}{}:
			default:
			}
		},
		started: func() { close(tw.started) },
	}
	return tw
}

// waitStarted blocks until the watcher's reconnect ticker exists. It must
// be called, after servicing the initial (pre-loop) dial attempt, before
// any test advances the fake clock — see the `started` field's doc comment
// in watcher.go for why: a tick delivered before the ticker is registered
// is simply lost, which otherwise manifests as a mysterious hang on the
// *second* reconnect attempt rather than a clean failure on the first.
func (tw *testWatcher) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-tw.started:
	case <-time.After(testSafetyTimeout):
		t.Fatalf("watcher did not reach its main loop within the safety timeout")
	}
}

// tickOuterSlack is added to testSafetyTimeout for the wait function tick
// returns below, so that timeout is STRICTLY longer than the one the
// background goroutine uses for itself. Without this margin the two
// timeouts started within nanoseconds of each other and raced outright:
// fakeClock.Advance's non-blocking send onto the ticker channel (the same
// doc comment explains why — it mirrors real time.Ticker, which drops a
// tick rather than queuing it for a slow consumer) means run() can
// legitimately coalesce two Advance() calls into one observed tick, so
// tw.ticked sometimes has nothing to deliver for a given call to tick()
// even though the watcher is not remotely hung. That is not an error —
// it is documented above conn1.Close() in
// TestWatcher_ReconnectsAfterConnectionDrop as "wasted ticks" the retry
// budget there already expects — but with equal timeouts it was a coin
// flip whether the background goroutine's own fallback (which always
// closes done) or wait's independent timer (which panics) fired first.
// Reproduced directly: run TestWatcher_ReconnectsAfterConnectionDrop with
// -count=100 before this fix and it panics within a handful of runs, on
// this exact race, not on any real hang. Giving the outer wait strictly
// more time guarantees the inner goroutine's close(done) always wins on a
// merely-coalesced tick, so wait's timeout is reserved for a genuine hang.
const tickOuterSlack = 5 * time.Second

// tick advances the fake clock by one tickInterval on a background
// goroutine and returns a function that blocks until the watcher has
// fully finished processing the resulting tick.
//
// This has to run on a background goroutine, not the calling test's own
// goroutine, because a tick that triggers a Status emission blocks on
// sending to the (unbuffered) output channel until the test reads it, and
// a tick that triggers a dial blocks on the test-controlled dialer until
// the test supplies a connection or error. Callers that expect either must
// service it (via recvStatus/expectNoStatus, or <-dials + nextConn<-)
// before calling the returned wait function.
func (tw *testWatcher) tick() func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		tw.clk.Advance(tickInterval)
		select {
		case <-tw.ticked:
		case <-time.After(testSafetyTimeout):
			// This Advance() did not produce an observable afterTick
			// signal within the ordinary budget. Most likely a
			// coalesced/dropped tick (see tickOuterSlack above), not a
			// hang: fall through and close done regardless, so a caller
			// blocked in wait() below is unblocked by THIS timeout, not
			// racing it.
		}
	}()
	return func() {
		select {
		case <-done:
		case <-time.After(testSafetyTimeout + tickOuterSlack):
			panic("tick: watcher did not finish processing within the safety timeout — a Status or dial was probably left unserviced")
		}
	}
}

// tickPlain is tick for the common case where the caller is certain the
// tick produces neither a Status nor a dial attempt.
func (tw *testWatcher) tickPlain() {
	tw.tick()()
}

func recvStatus(t *testing.T, ch <-chan Status) Status {
	t.Helper()
	select {
	case s, ok := <-ch:
		if !ok {
			t.Fatalf("status channel closed unexpectedly")
		}
		return s
	case <-time.After(testSafetyTimeout):
		t.Fatalf("timed out waiting for a Status")
	}
	return Status{}
}

func expectNoStatus(t *testing.T, ch <-chan Status) {
	t.Helper()
	select {
	case s, ok := <-ch:
		if ok {
			t.Fatalf("unexpected Status: %+v", s)
		}
	case <-time.After(50 * time.Millisecond):
		// Expected: nothing arrived quickly. This is the one place a very
		// short real sleep is used, purely to give a wrongly-eager send a
		// chance to land; the timing under test is entirely driven by the
		// fake clock, not by this wait.
	}
}

// statusPayload builds a switcher_status frame carrying one router input, in
// the shape measured on the live instance (see wire.go): a "status" ARRAY of
// {node, path, state} entries, with everything the lamps read nested under
// "state" and the formats as structured objects.
//
// videoFmt and audioFmts are raw JSON — pass liveVideoFormat / liveAudioFormat
// for the measured healthy shapes, or "null" for a stopped input. Passing no
// audio formats at all produces an empty audio array: the MP2/AC-3 silent-drop
// signature.
func statusPayload(statusKey, state, videoFmt string, audioFmts ...string) []byte {
	return frame(entry(statusKey, streamState(statusKey+" DISPLAY", state, videoFmt, audioFmts...)))
}

func TestWatcher_ConnectAndReceiveMessage(t *testing.T) {
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam7")

	url := <-tw.dials
	if url != statusURL(testStatusHost, "tok-1") {
		t.Fatalf("dial URL = %q", url)
	}
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}

	conn.push(statusPayload("cam7", "streaming", liveVideoFormat, liveAudioFormat))

	s := recvStatus(t, out)
	if s.Stale {
		t.Fatalf("Stale = true on first message")
	}
	if s.StreamState != "streaming" {
		t.Fatalf("StreamState = %q, want streaming", s.StreamState)
	}
	if s.Video.Codec != "h264" || s.Video.Width != 1920 || s.Video.Height != 1080 {
		t.Fatalf("Video = %+v", s.Video)
	}
	if len(s.Audio) != 1 || s.Audio[0].Codec != "aac" || s.Audio[0].Channels != 2 {
		t.Fatalf("Audio = %+v", s.Audio)
	}
}

func TestWatcher_EmptyAudioArraySurvivesAsEmptySlice(t *testing.T) {
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam7")
	<-tw.dials
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}

	conn.push(statusPayload("cam7", "streaming", liveVideoFormat))

	s := recvStatus(t, out)
	if s.Audio == nil {
		t.Fatalf("Audio is nil, want a non-nil empty slice (the MP2/AC-3 silent-drop signature must survive, not be smoothed over)")
	}
	if len(s.Audio) != 0 {
		t.Fatalf("Audio = %+v, want empty", s.Audio)
	}
}

func TestWatcher_DebouncesFlappingStreamState(t *testing.T) {
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam7")
	<-tw.dials
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	conn.push(statusPayload("cam7", "streaming", liveVideoFormat, liveAudioFormat))
	s := recvStatus(t, out)
	if s.StreamState != "streaming" {
		t.Fatalf("first StreamState = %q", s.StreamState)
	}

	// A momentary drop, well inside the 4s DebounceWindow: must not surface
	// as a change.
	conn.push(statusPayload("cam7", "stopped", liveVideoFormat, liveAudioFormat))
	s = recvStatus(t, out)
	if s.StreamState != "streaming" {
		t.Fatalf("StreamState during a momentary flap = %q, want it to stay streaming", s.StreamState)
	}

	conn.push(statusPayload("cam7", "streaming", liveVideoFormat, liveAudioFormat))
	s = recvStatus(t, out)
	if s.StreamState != "streaming" {
		t.Fatalf("StreamState after recovery = %q", s.StreamState)
	}

	// Ticking well past the debounce window must not flip anything: the
	// flap already resolved back to "streaming" before the window elapsed,
	// so nothing is pending to commit.
	for i := 0; i < 6; i++ {
		tw.tickPlain()
	}
	expectNoStatus(t, out)
}

func TestWatcher_DebouncedChangeCommitsAndIsEmitted(t *testing.T) {
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam7")
	<-tw.dials
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	conn.push(statusPayload("cam7", "streaming", liveVideoFormat, liveAudioFormat))
	recvStatus(t, out) // consume the initial "streaming"

	// stream_state goes bad and STAYS bad: after DebounceWindow (4s, i.e. 4
	// ticks at tickInterval=1s) the commit must surface even with no
	// further message, via the ticker's Tick path.
	conn.push(statusPayload("cam7", "stopped", liveVideoFormat, liveAudioFormat))
	recvStatus(t, out) // the message itself, still reporting "streaming" (pending)

	for i := 0; i < 3; i++ {
		tw.tickPlain()
		expectNoStatus(t, out)
	}

	// The 4th tick crosses the 4s window and emits: service it
	// concurrently with the tick's own processing.
	wait := tw.tick()
	s := recvStatus(t, out)
	wait()
	if s.StreamState != "stopped" {
		t.Fatalf("StreamState after the debounce window elapsed = %q, want stopped", s.StreamState)
	}
}

func TestWatcher_StaleAfterSilence(t *testing.T) {
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam7")
	<-tw.dials
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	conn.push(statusPayload("cam7", "streaming", liveVideoFormat, liveAudioFormat))
	recvStatus(t, out)

	// Fewer than StaleAfter (15s) ticks: must not go stale yet.
	for i := 0; i < 14; i++ {
		tw.tickPlain()
		expectNoStatus(t, out)
	}

	// The 15th tick crosses StaleAfter and emits.
	wait := tw.tick()
	s := recvStatus(t, out)
	wait()
	if !s.Stale {
		t.Fatalf("Stale = false after 15s of silence, want true")
	}
	if s.StreamState != "" {
		t.Fatalf("StreamState = %q while Stale, want empty per the Status doc comment", s.StreamState)
	}

	// "Keep emitting so the UI can grey its lamps; do not just go quiet."
	wait = tw.tick()
	s = recvStatus(t, out)
	wait()
	if !s.Stale {
		t.Fatalf("expected a second Stale Status while silence continues")
	}

	// Recovery: a fresh message must clear staleness immediately.
	conn.push(statusPayload("cam7", "streaming", liveVideoFormat, liveAudioFormat))
	s = recvStatus(t, out)
	if s.Stale {
		t.Fatalf("Stale = true after a fresh message arrived")
	}
}

func TestWatcher_ReconnectsWithBackoffOnConnectFailure(t *testing.T) {
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tw.w.Watch(ctx, "cam7")

	// Initial attempt, made synchronously before the loop starts.
	<-tw.dials
	tw.nextConn <- wsConnOrErr{err: errors.New("connection refused")}
	tw.waitStarted(t)

	// backoffDuration(1) == 1s == tickInterval, so the very next tick
	// already reaches the deadline and a redial is attempted.
	wait := tw.tick()
	u := <-tw.dials
	if u == "" {
		t.Fatalf("expected a redial once the first backoff rung (1s) elapsed")
	}
	tw.nextConn <- wsConnOrErr{err: errors.New("still refused")}
	wait()

	// backoffDuration(2) == 2s: one tick (1s since the 2nd failure) must
	// NOT yet redial...
	tw.tickPlain()
	select {
	case u := <-tw.dials:
		t.Fatalf("redialed too early, before the 2s backoff elapsed: %q", u)
	default:
	}

	// ...but the next one (2s total) must.
	wait = tw.tick()
	u = <-tw.dials
	if u == "" {
		t.Fatalf("expected a redial once the second backoff rung (2s) elapsed")
	}
	tw.nextConn <- wsConnOrErr{conn: newFakeConn()}
	wait()
}

func TestWatcher_ReconnectsAfterConnectionDrop(t *testing.T) {
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam7")
	<-tw.dials
	conn1 := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn1}
	tw.waitStarted(t)

	conn1.push(statusPayload("cam7", "streaming", liveVideoFormat, liveAudioFormat))
	recvStatus(t, out)

	// Simulate the connection dropping. The read error surfaces on the
	// watcher's background reader goroutine, which sends it to the
	// watcher's errCh independently of the fake clock; run()'s select then
	// picks it up whenever the Go scheduler gets to it. Under scheduling
	// pressure that can take several of the watcher's own select
	// iterations to be observed (measured up to ~3 "wasted" ticks in a
	// stress run of this suite), so the retry budget below is generous —
	// each iteration is cheap (one fake-clock second, no real sleep) and
	// this only needs to be an upper bound, not a tight one.
	conn1.Close()

	// Pre-load several successful dial results up front: nextConn is
	// buffered, so whichever tick's processing actually calls dialNow()
	// finds a result waiting and never blocks. Only the first is ever
	// consumed; the rest are inert.
	conn2 := newFakeConn()
	for i := 0; i < 5; i++ {
		tw.nextConn <- wsConnOrErr{conn: conn2}
	}

	// Drain `out` for the duration of the retry loop below. If scheduling
	// pressure is severe enough that redial genuinely needs >=StaleAfter
	// (15) ticks — not just a handful of "wasted" ones — run's ticker-case
	// (watcher.go) tries to emit a Stale Status before this loop is done,
	// and emit() blocks on sending to `out` until something reads it. This
	// test does not read `out` again until after the loop, so without a
	// drain here that emit — and with it the ENTIRE watcher goroutine,
	// which cannot get back to its top-level select while parked inside
	// emit() — deadlocks for the rest of the test: every subsequent
	// tick() then burns a full testSafetyTimeout falling through its own
	// fallback path instead of returning promptly, which is slow rather
	// than a clean failure and can blow go test's own -timeout budget
	// under enough contention (reproduced directly: this loop needed
	// >15 ticks under simultaneous heavy parallel load, and without this
	// drain the whole package timed out at 400s instead of failing fast).
	// It is stopped and joined before the loop's final recvStatus below so
	// it can never steal the message that call is meant to observe.
	drainStop := make(chan struct{})
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			select {
			case <-out:
			case <-drainStop:
				return
			}
		}
	}()

	var redialURL string
	for i := 0; i < 30 && redialURL == ""; i++ {
		wait := tw.tick()
		wait()
		select {
		case redialURL = <-tw.dials:
		default:
		}
	}
	close(drainStop)
	<-drainDone
	if redialURL == "" {
		t.Fatalf("no redial observed after dropping the connection")
	}

	conn2.push(statusPayload("cam7", "streaming", liveVideoFormat, liveAudioFormat))
	s := recvStatus(t, out)
	if s.StreamState != "streaming" {
		t.Fatalf("StreamState after reconnect = %q", s.StreamState)
	}
}

func TestWatcher_ReopensSocketOnTokenRotation(t *testing.T) {
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam7")
	firstURL := <-tw.dials
	if firstURL != statusURL(testStatusHost, "tok-1") {
		t.Fatalf("initial dial URL = %q", firstURL)
	}
	conn1 := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn1}
	tw.waitStarted(t)

	conn1.push(statusPayload("cam7", "streaming", liveVideoFormat, liveAudioFormat))
	recvStatus(t, out)

	// Client refreshed: the token in the URL is now stale, and nothing
	// tells the open socket directly, per CONTRACT.md: "a refresh means
	// reopening the socket. Handle that."
	client.set("tok-2")

	conn2 := newFakeConn()
	wait := tw.tick() // detects the rotation, closes conn1, redials immediately
	secondURL := <-tw.dials
	if secondURL != statusURL(testStatusHost, "tok-2") {
		t.Fatalf("dial URL after rotation = %q, want the new token", secondURL)
	}
	tw.nextConn <- wsConnOrErr{conn: conn2}
	wait()

	conn2.push(statusPayload("cam7", "streaming", liveVideoFormat, liveAudioFormat))
	s := recvStatus(t, out)
	if s.StreamState != "streaming" {
		t.Fatalf("StreamState after token rotation reconnect = %q", s.StreamState)
	}
}

func TestWatcher_MalformedFrameDoesNotCrashOrResetStaleness(t *testing.T) {
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam7")
	<-tw.dials
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}

	conn.push(statusPayload("cam7", "streaming", liveVideoFormat, liveAudioFormat))
	recvStatus(t, out)

	// A malformed frame must not panic the watcher goroutine and must not
	// itself produce a Status.
	conn.push([]byte(`{not valid json`))
	expectNoStatus(t, out)

	// The watcher must still be alive and processing subsequent good frames.
	//
	// The recovery frame carries a DIFFERENT stream_state from the first one
	// on purpose. A Status is now emitted only when something a lamp reads has
	// actually moved (watcher.go, lampView), so re-pushing the identical frame
	// would be correctly silent and would prove nothing about whether the
	// watcher is still running. A changed one distinguishes "alive" from
	// "wedged", which is the whole point of the test.
	conn.push(statusPayload("cam7", "stopped", liveVideoFormat, liveAudioFormat))
	s := recvStatus(t, out)
	if s.StreamState != "streaming" {
		// Still "streaming": the change is inside the 4 s debounce window.
		// What matters here is that a Status arrived at all.
		t.Fatalf("StreamState after a malformed frame = %q", s.StreamState)
	}
}

func TestWatcher_WrongStatusKeyIsReportedRatherThanSilentlyIgnored(t *testing.T) {
	// THE failure this rewrite exists to close. A wrong statusKey used to be
	// indistinguishable from a blank one: no Status was emitted at all, and
	// the staleness rule never fired — frames WERE arriving, just not about
	// the node we asked for — so the three lamps read "NO STATUS" for the
	// whole session and nothing anywhere said why.
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam99")
	<-tw.dials
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	conn.push(frame(
		entry("cam22", streamState("CLAUDE-COMMS", StreamStateStreaming, liveVideoFormat, liveAudioFormat)),
		entry("cam7", streamState("REPLAY 1 CLN", StreamStateStopped, `null`, `null`)),
		entry("router", `{"connections":{}}`),
	))

	s := recvStatus(t, out)
	if !s.Stale {
		t.Error("Stale = false; the lamps must be greyed, not left showing values for a node we are not watching")
	}
	if s.KeyError == "" {
		t.Fatal("KeyError is empty; a statusKey that matches nothing must be reported, not swallowed")
	}
	for _, want := range []string{
		`"cam99"`,      // what was looked for
		`"cam22"`,      // what is there instead
		"CLAUDE-COMMS", // by the name the operator would recognise
		`"cam7"`,
		"REPLAY 1 CLN",
	} {
		if !strings.Contains(s.KeyError, want) {
			t.Errorf("KeyError does not mention %q:\n%s", want, s.KeyError)
		}
	}
	// "router" carries no stream_state, so offering it would be offering a
	// statusKey that can never work.
	if strings.Contains(s.KeyError, "router") {
		t.Errorf("KeyError offers a node that is not a router input:\n%s", s.KeyError)
	}
	if s.StreamState != "" || s.Video != (VideoFormat{}) || len(s.Audio) != 0 {
		t.Errorf("Status carries values for a node we never found: %+v", s)
	}

	// It does NOT repeat itself per frame. The socket delivers about twenty
	// frames a second (wire.go), so a report per frame would be a Status
	// storm and a log flood.
	conn.push(frame(entry("cam22", streamState("CLAUDE-COMMS", StreamStateStreaming, liveVideoFormat, liveAudioFormat))))
	expectNoStatus(t, out)

	// But it does keep saying so on a heartbeat, at the same StaleAfter
	// cadence as staleness itself: a page that loaded after the first report
	// must still learn why its lamps are grey.
	for i := 0; i < int(StaleAfter/tickInterval)-1; i++ {
		tw.tickPlain()
		expectNoStatus(t, out)
	}
	wait := tw.tick()
	again := recvStatus(t, out)
	wait()
	if !again.Stale || again.KeyError != s.KeyError {
		t.Errorf("heartbeat produced %+v; the misconfiguration has not gone away", again)
	}
}

func TestWatcher_DeltaFramesAreNotEvidenceAboutTheStatusKey(t *testing.T) {
	// The trap the live capture alone could not show. The socket is
	// snapshot-then-delta: after the opening snapshot it sends about twenty
	// frames a second, each about ONE node's subtree, and a delta about
	// somebody else's audio meters says nothing whatever about our node.
	//
	// Treating that silence as "your statusKey is wrong" would condemn a
	// perfectly good configuration twenty times a second; treating a
	// "/statistics" delta about our OWN node as a node state would report the
	// one input that is working as "not a router input".
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam22")
	<-tw.dials
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	conn.push(frame(
		entry("cam22", streamState("CLAUDE-COMMS", StreamStateStreaming, liveVideoFormat, liveAudioFormat)),
		entry("cam1", streamState("Input 1", StreamStateStreaming, liveVideoFormat, liveAudioFormat)),
	))
	if s := recvStatus(t, out); s.StreamState != StreamStateStreaming || s.KeyError != "" {
		t.Fatalf("opening snapshot produced %+v, want a healthy streaming Status", s)
	}

	// Verbatim off the wire, both of them.
	conn.push(readFixture(t, liveDeltaLevels))
	expectNoStatus(t, out)
	conn.push(readFixture(t, liveDeltaStatistics))
	expectNoStatus(t, out)
	// And one about our own node, which is the nastier half of the trap.
	conn.push(frame(deltaEntry("cam22", "/statistics", `{"bitrate":507.4,"packet_count":3374}`)))
	expectNoStatus(t, out)

	// The delta about our own node has now been MERGED rather than skipped,
	// and the proof that it merged into the right place is that the node is
	// still whole: a following stream_state delta finds a node with a
	// display_name and formats still on it, not the wreckage of a /statistics
	// state that overwrote them.
	conn.push(frame(deltaEntry("cam22", "/stream_state", `"stopped"`)))
	s := recvStatus(t, out)
	if s.Video.Codec != "h264" || len(s.Audio) != 1 || s.Audio[0].Codec != "aac" {
		t.Fatalf("Status after a merged /statistics delta = %+v; the node was damaged by it", s)
	}
	if s.KeyError != "" {
		t.Fatalf("KeyError = %q; the merged node stopped being a router input", s.KeyError)
	}
}

func TestWatcher_AStreamStateDeltaMovesTheLamp(t *testing.T) {
	// THE BUG. The lamps read stream_state, streams.video.format and
	// streams.audio, all three of which live in the document the socket
	// snapshots once and then updates by subtree. Skipping deltas — which is
	// what stopped a "/statistics" frame being misread as a whole node —
	// froze all three at the state of the first frame: an input that was
	// stopped at connect read STOPPED, NO VIDEO and BAD FORMAT for the rest
	// of the session, however loudly it was actually streaming.
	//
	// NOTE what this frame is: a synthetic delta at "/stream_state". Nobody
	// has ever observed a real transition on this socket, so this is the
	// shape one WOULD take, not one that was captured. That is exactly why
	// resyncInterval exists as well.
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam22")
	<-tw.dials
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	// Connect: the input is stopped, so both formats are null. This is the
	// state the operator's lamps were frozen in.
	conn.push(frame(entry("cam22", streamState("CLAUDE-COMMS", StreamStateStopped, `null`, `null`))))
	if s := recvStatus(t, out); s.StreamState != StreamStateStopped {
		t.Fatalf("opening snapshot = %+v, want stopped", s)
	}

	// It comes up. The transition arrives as a subtree delta, and so do the
	// formats that appear with it.
	conn.push(frame(deltaEntry("cam22", "/stream_state", `"`+StreamStateStreaming+`"`)))
	s := recvStatus(t, out)
	if s.StreamState != StreamStateStopped {
		t.Fatalf("StreamState = %q immediately after the delta; the 4 s debounce must still be holding", s.StreamState)
	}

	conn.push(frame(deltaEntry("cam22", "/streams", `{"video":{"format":`+liveVideoFormat+`},`+
		`"audio":[{"format":`+liveAudioFormat+`}]}`)))
	s = recvStatus(t, out)
	if s.Video.Codec != "h264" || s.Video.Width != 1920 || s.Video.FrameRate != 50 {
		t.Fatalf("Video = %+v after a /streams delta, want the merged format", s.Video)
	}
	if len(s.Audio) != 1 || s.Audio[0].Codec != "aac" || s.Audio[0].SampleRate != 48000 {
		t.Fatalf("Audio = %+v after a /streams delta", s.Audio)
	}

	// And the debounce commits the state change from the passage of time,
	// with no further frame, exactly as it does for a whole-node update.
	for i := 0; i < 3; i++ {
		tw.tickPlain()
		expectNoStatus(t, out)
	}
	wait := tw.tick()
	s = recvStatus(t, out)
	wait()
	if s.StreamState != StreamStateStreaming {
		t.Fatalf("StreamState after the debounce window = %q, want streaming — the lamp never moved", s.StreamState)
	}
	if s.Stale {
		t.Fatal("Stale = true; the socket has been delivering throughout")
	}
}

func TestWatcher_ABurstOfLevelsDeltasProducesNoStatus(t *testing.T) {
	// "/levels" arrives about ten times a second and carries nothing any
	// lamp reads. Merging it is right; announcing it is not. Emitting a Status
	// per delta would put ~21 events a second on the Wails bus to say
	// precisely nothing, and would make the event stream useless as a signal
	// that something changed.
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam22")
	<-tw.dials
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	conn.push(liveFrame(t))
	if s := recvStatus(t, out); s.StreamState != StreamStateStreaming || s.KeyError != "" {
		t.Fatalf("the live connect snapshot produced %+v", s)
	}

	// Verbatim off the wire, at the rate it actually arrives.
	levels := readFixture(t, liveDeltaLevels)
	stats := readFixture(t, liveDeltaStatistics)
	for i := 0; i < 15; i++ {
		conn.push(levels)
	}
	// And the one that IS about a router input, but about the one number this
	// package deliberately refuses to read (statistics.bitrate freezes on a
	// dead feed — see wireNode).
	for i := 0; i < 5; i++ {
		conn.push(stats)
	}
	expectNoStatus(t, out)

	// The socket is emphatically not stale: those frames all counted as
	// liveness even though none of them was worth announcing.
	for i := 0; i < int(StaleAfter/tickInterval)-1; i++ {
		tw.tickPlain()
	}
	expectNoStatus(t, out)

	// A frame that DOES move a lamp still gets through, so the gate above is
	// selective rather than simply shut.
	conn.push(frame(deltaEntry("cam22", "/streams/video/format", `null`)))
	if s := recvStatus(t, out); s.Video.Raw != "" {
		t.Fatalf("Video = %+v after the format went null, want it cleared", s.Video)
	}
}

func TestWatcher_ResyncsPeriodicallyAndRebaselinesFromTheFreshSnapshot(t *testing.T) {
	// The backstop, and the reason it is needed is stated plainly in
	// resyncInterval: it is NOT KNOWN whether a stream_state change is pushed
	// on this socket at all. If it only ever appears in the frame that opens a
	// connection, merging deltas cannot help and nothing but a reconnect will.
	//
	// So this drives a socket that is perfectly healthy — no error, no drop,
	// no token rotation — and requires it to be reopened anyway.
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam22")
	<-tw.dials
	conn1 := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn1}
	tw.waitStarted(t)

	conn1.push(frame(entry("cam22", streamState("CLAUDE-COMMS", StreamStateStopped, `null`, `null`))))
	if s := recvStatus(t, out); s.StreamState != StreamStateStopped {
		t.Fatalf("opening snapshot = %+v, want stopped", s)
	}

	// conn1 never says another word — the case the backstop exists for.
	// Pre-load the replacement, buffered, so whichever tick performs the
	// resync finds a connection waiting rather than blocking on the test.
	conn2 := newFakeConn()
	for i := 0; i < 5; i++ {
		tw.nextConn <- wsConnOrErr{conn: conn2}
	}

	// Silence outlasts StaleAfter long before it reaches resyncInterval, so
	// the loop emits Stale Statuses throughout. Drain them: that is correct
	// behaviour, not the thing under test, and emit() blocks on an unread
	// channel. See TestWatcher_ReconnectsAfterConnectionDrop for the same
	// mechanics and why the drain must be stopped before the next recvStatus.
	drainStop := make(chan struct{})
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			select {
			case <-out:
			case <-drainStop:
				return
			}
		}
	}()

	var resyncURL string
	for i := 0; i < int(resyncInterval/tickInterval)+5 && resyncURL == ""; i++ {
		tw.tick()()
		select {
		case resyncURL = <-tw.dials:
		default:
		}
	}
	close(drainStop)
	<-drainDone
	if resyncURL == "" {
		t.Fatalf("the status socket was never reopened; a lamp frozen by a transition that is only ever in the snapshot would stay frozen for the whole session")
	}
	if resyncURL != statusURL(testStatusHost, "tok-1") {
		t.Errorf("resync dialled %q", resyncURL)
	}

	// The fresh connection's snapshot says the input is up now, and carries
	// formats where the old baseline had nulls. Re-baselining is what makes
	// those reach the lamps.
	conn2.push(frame(entry("cam22", streamState("CLAUDE-COMMS", StreamStateStreaming, liveVideoFormat, liveAudioFormat))))
	s := recvStatus(t, out)
	if s.Stale {
		t.Fatal("Stale = true on the resync snapshot")
	}
	if s.Video.Codec != "h264" || len(s.Audio) != 1 || s.Audio[0].Codec != "aac" {
		t.Fatalf("Status = %+v; the document was not re-baselined from the fresh snapshot", s)
	}

	// stream_state is debounced like any other change, resync or not.
	for i := 0; i < 3; i++ {
		tw.tickPlain()
		expectNoStatus(t, out)
	}
	wait := tw.tick()
	s = recvStatus(t, out)
	wait()
	if s.StreamState != StreamStateStreaming {
		t.Fatalf("StreamState after the resync and the debounce window = %q", s.StreamState)
	}
}

func TestStatusSocket_ResyncOnAHealthySocket(t *testing.T) {
	// The ordinary case: close the working connection, open a new one at once
	// (the socket was fine a moment ago and this loop is what broke it), and
	// leave the backoff ladder at rung zero.
	tw := newTestWatcher(&fakeClientToken{token: "tok-1"})
	sock := newStatusSocket(tw.w)
	conn1 := newFakeConn()
	sock.conn, sock.gen, sock.backoffN = conn1, 1, 0

	conn2 := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn2}
	sock.resync(context.Background(), base)

	if u := <-tw.dials; u != statusURL(testStatusHost, "tok-1") {
		t.Errorf("resync dialled %q", u)
	}
	if sock.conn != wsConn(conn2) {
		t.Error("resync did not adopt the new connection")
	}
	if sock.gen != 2 {
		t.Errorf("gen = %d, want the generation bumped so the old connection's frames are discarded", sock.gen)
	}
	if _, _, err := conn1.ReadMessage(); err == nil {
		t.Error("the old connection was left open")
	}
}

func TestStatusSocket_ResyncDoesNothingWhenTheSocketIsAlreadyDown(t *testing.T) {
	// A socket that is already down is going to be redialled by maintain, on
	// the backoff ladder, and that redial produces exactly the same fresh
	// snapshot. Dialling on top of it would only fight the ladder — and the
	// ladder is the thing keeping this application from hammering a switcher
	// that is refusing connections.
	tw := newTestWatcher(&fakeClientToken{token: "tok-1"})
	sock := newStatusSocket(tw.w)
	sock.backoffN = 3
	sock.nextAttempt = base.Add(5 * time.Second)

	// A connection is left waiting in the dialer so that a resync which
	// wrongly dials gets an answer and this test FAILS on the assertion
	// below, rather than blocking forever on an unserviced dial. A hang and a
	// failure look the same to a CI log and are not the same to anybody
	// reading it.
	tw.nextConn <- wsConnOrErr{conn: newFakeConn()}

	sock.resync(context.Background(), base)

	select {
	case u := <-tw.dials:
		t.Fatalf("resync dialled %q while the socket was down and a retry was already scheduled", u)
	default:
	}
	if sock.conn != nil {
		t.Error("resync opened a connection while the backoff ladder was mid-climb")
	}
	if sock.backoffN != 3 || !sock.nextAttempt.Equal(base.Add(5*time.Second)) {
		t.Errorf("resync disturbed the backoff ladder: backoffN = %d, nextAttempt = %v", sock.backoffN, sock.nextAttempt)
	}
}

func TestStatusSocket_ResyncWhoseRedialFailsArmsTheLadder(t *testing.T) {
	// The resync must not be able to leave the loop in a state the ordinary
	// reconnect path does not already handle: a failed redial is a plain
	// disconnection, backoff and all, and staleness fires at StaleAfter if it
	// keeps failing.
	tw := newTestWatcher(&fakeClientToken{token: "tok-1"})
	sock := newStatusSocket(tw.w)
	sock.conn, sock.gen = newFakeConn(), 1

	tw.nextConn <- wsConnOrErr{err: errors.New("connection refused")}
	sock.resync(context.Background(), base)
	<-tw.dials

	if sock.conn != nil {
		t.Fatal("resync kept a connection after the redial failed")
	}
	if sock.backoffN != 1 {
		t.Errorf("backoffN = %d, want the ladder armed at rung one", sock.backoffN)
	}
	if want := base.Add(backoffDuration(1)); !sock.nextAttempt.Equal(want) {
		t.Errorf("nextAttempt = %v, want %v", sock.nextAttempt, want)
	}
}

func TestWatcher_AReconnectRebuildsTheDocumentFromScratch(t *testing.T) {
	// A re-baseline is a REPLACEMENT, and it has to be, for the same reason
	// the enumerated inputs are forgotten on reconnect: the switcher may have
	// been reconfigured while we were away. If the old document survived, a
	// node deleted from the switcher would still be sitting in it, and the
	// first delta that happened to name it would light the lamps green
	// against a node that no longer exists.
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam22")
	<-tw.dials
	conn1 := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn1}
	tw.waitStarted(t)

	conn1.push(frame(entry("cam22", streamState("CLAUDE-COMMS", StreamStateStreaming, liveVideoFormat, liveAudioFormat))))
	if s := recvStatus(t, out); s.StreamState != StreamStateStreaming {
		t.Fatalf("first Status = %+v", s)
	}

	// Force a reconnect the deterministic way: the token lives in the URL, so
	// rotating it obliges the loop to reopen the socket on its next tick.
	conn2 := newFakeConn()
	client.set("tok-2")
	wait := tw.tick()
	if u := <-tw.dials; u != statusURL(testStatusHost, "tok-2") {
		t.Fatalf("redial URL = %q", u)
	}
	tw.nextConn <- wsConnOrErr{conn: conn2}
	wait()

	// The new switcher does not have cam22 at all.
	conn2.push(frame(entry("cam1", streamState("Input 1", StreamStateStreaming, liveVideoFormat, liveAudioFormat))))
	if s := recvStatus(t, out); !s.Stale || s.KeyError == "" {
		t.Fatalf("Status = %+v, want the report: the new snapshot does not carry cam22", s)
	}

	// A delta naming cam22 must now find nothing to merge into. If the old
	// document had survived the reconnect it would merge into the state
	// captured from conn1 and emit a healthy Status for a node that is not on
	// this switcher.
	conn2.push(frame(deltaEntry("cam22", "/stream_state", `"`+StreamStateStreaming+`"`)))
	expectNoStatus(t, out)
	conn2.push(frame(deltaEntry("cam22", "/statistics", `{"bitrate":507.4}`)))
	expectNoStatus(t, out)
}

func TestWatcher_AChangeToStreamsAudioAloneIsEmitted(t *testing.T) {
	// streams.audio is a lamp input in its own right, and its LENGTH is the
	// load-bearing part: an empty audio array is the MP2/AC-3 silent-drop
	// signature — M2L-X keeps the video online and discards the audio without
	// saying so (Status.Audio). A change gate that compared only stream_state
	// and the video format would swallow exactly that.
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam22")
	<-tw.dials
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	conn.push(frame(entry("cam22", streamState("CLAUDE-COMMS", StreamStateStreaming, liveVideoFormat, liveAudioFormat))))
	if s := recvStatus(t, out); len(s.Audio) != 1 {
		t.Fatalf("opening snapshot = %+v", s)
	}

	// The audio is dropped. Nothing else about the node changes: same
	// stream_state, same video format.
	conn.push(frame(deltaEntry("cam22", "/streams/audio", `[]`)))
	s := recvStatus(t, out)
	if s.Audio == nil {
		t.Fatal("Audio is nil, want a non-nil empty slice")
	}
	if len(s.Audio) != 0 {
		t.Fatalf("Audio = %+v, want empty: the silent-drop signature was swallowed", s.Audio)
	}
	if s.StreamState != StreamStateStreaming || s.Video.Codec != "h264" {
		t.Errorf("Status = %+v; only the audio should have moved", s)
	}

	// A second audio stream appearing is a change too, and one that a
	// length-blind comparison would also miss in the other direction.
	conn.push(frame(deltaEntry("cam22", "/streams/audio",
		`[{"format":`+liveAudioFormat+`},{"format":`+liveAudioFormat+`}]`)))
	if s := recvStatus(t, out); len(s.Audio) != 2 {
		t.Fatalf("Audio = %+v, want two streams", s.Audio)
	}

	// The same audio arriving again is NOT a change, and must be silent.
	conn.push(frame(deltaEntry("cam22", "/streams/audio",
		`[{"format":`+liveAudioFormat+`},{"format":`+liveAudioFormat+`}]`)))
	expectNoStatus(t, out)
}

func TestWatcher_ResyncForgetsTheOldConnectionsMergedDeltas(t *testing.T) {
	// A re-baseline has to be a REPLACEMENT. If the new connection's snapshot
	// were merged into the old document, a subtree the switcher has since
	// dropped would outlive it — and the whole point of the resync is that the
	// fresh snapshot is the authority.
	d := newDocument()
	if _, err := d.apply(frame(entry("cam22", streamState("CLAUDE-COMMS", StreamStateStreaming, liveVideoFormat, liveAudioFormat)))); err != nil {
		t.Fatal(err)
	}
	if _, err := d.apply(frame(deltaEntry("cam22", "/stream_state", `"stopped"`))); err != nil {
		t.Fatal(err)
	}
	if node, _ := d.streamNode("cam22"); node.StreamState != StreamStateStopped {
		t.Fatalf("the delta did not merge: %+v", node)
	}

	// What run() does on a new connection generation.
	d = newDocument()
	if _, err := d.apply(frame(entry("cam22", streamState("CLAUDE-COMMS", StreamStateStreaming, liveVideoFormat, liveAudioFormat)))); err != nil {
		t.Fatal(err)
	}
	node, ok := d.streamNode("cam22")
	if !ok || node.StreamState != StreamStateStreaming {
		t.Fatalf("cam22 = %+v, ok = %v; the new snapshot must win outright", node, ok)
	}
}

func TestWatcher_ADeltaBeforeAnySnapshotIsNotAStatus(t *testing.T) {
	// A connection whose first frame is a delta has no baseline to merge into.
	// Assembling a node from subtrees alone would produce one with no
	// stream_state — which reads exactly like "your statusKey names something
	// that is not a router input", i.e. it would report a misconfiguration
	// that does not exist.
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam22")
	<-tw.dials
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	conn.push(frame(deltaEntry("cam22", "/stream_state", `"streaming"`)))
	expectNoStatus(t, out)
	conn.push(frame(deltaEntry("cam22", "/streams/video/format", liveVideoFormat)))
	expectNoStatus(t, out)
	conn.push(readFixture(t, liveDeltaLevels))
	expectNoStatus(t, out)

	// The snapshot arrives late and is the first thing that says anything.
	conn.push(frame(entry("cam22", streamState("CLAUDE-COMMS", StreamStateStopped, `null`, `null`))))
	s := recvStatus(t, out)
	if s.StreamState != StreamStateStopped {
		t.Fatalf("Status = %+v, want the snapshot's value and nothing invented from the deltas before it", s)
	}
	if s.Video.Raw != "" || len(s.Audio) != 1 || s.Audio[0].Raw != "" {
		t.Fatalf("Status = %+v; a delta from before the baseline leaked into it", s)
	}
}

func TestWatcher_AnUnmergeableDeltaLeavesTheLampsAlone(t *testing.T) {
	// The synthetic half of the parsing guard, at the Watch loop rather than
	// the document. Four paths have ever been observed; a path this package
	// cannot follow must leave the last good reading standing, and above all
	// must never let the delta's own state stand in for the node.
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam22")
	<-tw.dials
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	conn.push(frame(entry("cam22", streamState("CLAUDE-COMMS", StreamStateStreaming, liveVideoFormat, liveAudioFormat))))
	if s := recvStatus(t, out); s.StreamState != StreamStateStreaming {
		t.Fatalf("opening snapshot = %+v", s)
	}

	// A whole node's worth of state, arriving with a path that is not "/" and
	// that this package cannot follow. Reading it as a node would report cam22
	// as stopped with no formats; misreading it as a whole node would be
	// yesterday's bug back again.
	for _, path := range []string{"", "stream_state", "/a//b", "/display_name/x", "/streams/audio/0"} {
		conn.push(frame(deltaEntry("cam22", path,
			`{"display_name":"WRONG","stream_state":"stopped","streams":{"audio":[],"video":{"format":null}}}`)))
		expectNoStatus(t, out)
	}

	// Still streaming, still h264, still one AAC stream: nothing above
	// touched the document, and the debounce has nothing pending either.
	for i := 0; i < int(DebounceWindow/tickInterval)+2; i++ {
		tw.tickPlain()
		expectNoStatus(t, out)
	}

	conn.push(frame(deltaEntry("cam22", "/stream_state", `"`+StreamStateStopped+`"`)))
	s := recvStatus(t, out)
	if s.StreamState != StreamStateStreaming {
		t.Fatalf("StreamState = %q; the confirmed value should still be streaming, with stopped only now pending", s.StreamState)
	}
	if s.Video.Codec != "h264" || len(s.Audio) != 1 || s.Audio[0].Codec != "aac" {
		t.Fatalf("Status = %+v; an unmergeable delta damaged the node after all", s)
	}
	if s.KeyError != "" {
		t.Fatalf("KeyError = %q; an unmergeable delta was taken as evidence about the statusKey", s.KeyError)
	}
}

func TestWatcher_AKeyErrorIsOnlyDecidedFromASnapshot(t *testing.T) {
	// A connection whose first frame is a delta has enumerated nothing. It
	// must stay silent rather than guess, because guessing here means telling
	// an operator their configuration is wrong on no evidence at all.
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam22")
	<-tw.dials
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	conn.push(readFixture(t, liveDeltaLevels))
	expectNoStatus(t, out)
	conn.push(readFixture(t, liveDeltaStatistics))
	expectNoStatus(t, out)

	// The snapshot arrives, and only now is there anything to conclude.
	conn.push(frame(entry("cam1", streamState("Input 1", StreamStateStreaming, liveVideoFormat, liveAudioFormat))))
	s := recvStatus(t, out)
	if !s.Stale || s.KeyError == "" {
		t.Fatalf("Status = %+v, want the report once a snapshot has been seen", s)
	}
}

func TestWatcher_ReconnectingForgetsTheOldConnectionsSnapshot(t *testing.T) {
	// Every connection begins with its own snapshot (wire.go), and what the
	// LAST one enumerated says nothing about this one — the switcher may have
	// been reconfigured while we were away.
	//
	// If that knowledge were carried across, an input deleted from the
	// switcher during the outage would still count as "known", the report
	// would never be raised, and the operator would be back to lamps that read
	// grey for ever with no explanation: the exact failure being closed here,
	// reintroduced by the back door.
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam22")
	<-tw.dials
	conn1 := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn1}
	tw.waitStarted(t)

	conn1.push(frame(entry("cam22", streamState("CLAUDE-COMMS", StreamStateStreaming, liveVideoFormat, liveAudioFormat))))
	if s := recvStatus(t, out); s.StreamState != StreamStateStreaming || s.KeyError != "" {
		t.Fatalf("first Status = %+v, want a healthy streaming one", s)
	}

	conn1.Close()
	conn2 := newFakeConn()
	for i := 0; i < 5; i++ {
		tw.nextConn <- wsConnOrErr{conn: conn2}
	}

	// See TestWatcher_ReconnectsAfterConnectionDrop for why the redial loop
	// needs a generous budget and a drain: identical mechanics.
	drainStop := make(chan struct{})
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			select {
			case <-out:
			case <-drainStop:
				return
			}
		}
	}()
	var redialled bool
	for i := 0; i < 30 && !redialled; i++ {
		tw.tick()()
		select {
		case <-tw.dials:
			redialled = true
		default:
		}
	}
	close(drainStop)
	<-drainDone
	if !redialled {
		t.Fatal("no redial observed after dropping the connection")
	}

	// The new connection's snapshot no longer has cam22: it was deleted from
	// the switcher while we were away.
	conn2.push(frame(entry("cam1", streamState("Input 1", StreamStateStreaming, liveVideoFormat, liveAudioFormat))))
	s := recvStatus(t, out)
	if !s.Stale || s.KeyError == "" {
		t.Fatalf("Status = %+v, want the report: the new snapshot does not carry cam22", s)
	}
	if strings.Contains(s.KeyError, "CLAUDE-COMMS") {
		t.Errorf("the report still lists a node from the previous connection:\n%s", s.KeyError)
	}
}

func TestWatcher_AStatusKeyNamingANonRouterNodeIsReportedDistinctly(t *testing.T) {
	// Typing "mixer" is a different mistake from typing "cam99". Both leave
	// the lamps dead; an operator should not have to guess which they made.
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "mixer")
	<-tw.dials
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	conn.push(frame(
		entry("mixer", `{"program":{"source":"cam1"},"transition":{"rate":25}}`),
		entry("cam22", streamState("CLAUDE-COMMS", StreamStateStreaming, liveVideoFormat, liveAudioFormat)),
	))

	s := recvStatus(t, out)
	if !s.Stale || s.KeyError == "" {
		t.Fatalf("Status = %+v, want a greyed, explained one", s)
	}
	if !strings.Contains(s.KeyError, "no stream_state") {
		t.Errorf("KeyError does not say why %q can never work:\n%s", "mixer", s.KeyError)
	}
}

func TestWatcher_RecoversWhenTheStatusKeyStartsMatching(t *testing.T) {
	// A node that is not in the frame yet — an input the gallery has not
	// created — must not poison the watcher: the moment it appears, the lamps
	// come back with real values and no KeyError.
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam22")
	<-tw.dials
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	conn.push(frame(entry("cam7", streamState("REPLAY 1 CLN", StreamStateStopped, `null`, `null`))))
	if s := recvStatus(t, out); !s.Stale || s.KeyError == "" {
		t.Fatalf("Status = %+v, want a greyed, explained one", s)
	}

	conn.push(frame(
		entry("cam7", streamState("REPLAY 1 CLN", StreamStateStopped, `null`, `null`)),
		entry("cam22", streamState("CLAUDE-COMMS", StreamStateStreaming, liveVideoFormat, liveAudioFormat)),
	))
	s := recvStatus(t, out)
	if s.Stale {
		t.Error("Stale = true after the node appeared")
	}
	if s.KeyError != "" {
		t.Errorf("KeyError = %q, want it cleared once the key matched", s.KeyError)
	}
	if s.StreamState != StreamStateStreaming {
		t.Errorf("StreamState = %q, want streaming", s.StreamState)
	}
	if s.Video.Codec != "h264" || s.Video.Width != 1920 || s.Video.Height != 1080 || s.Video.FrameRate != 50 {
		t.Errorf("Video = %+v", s.Video)
	}
}

func TestWatcher_AMalformedFrameIsNotAWrongStatusKey(t *testing.T) {
	// The two must never be conflated. A frame this package cannot read says
	// nothing about whether the statusKey is right, so claiming it does would
	// send an operator hunting for a node name over a dropped packet.
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam7")
	<-tw.dials
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	conn.push([]byte(`{not valid json`))
	expectNoStatus(t, out)

	conn.push([]byte(`{"status":"not an array"}`))
	expectNoStatus(t, out)
}

func TestWatcher_ChannelClosedOnContextCancellation(t *testing.T) {
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())

	out := tw.w.Watch(ctx, "cam7")
	<-tw.dials
	tw.nextConn <- wsConnOrErr{err: errors.New("no connection needed for this test")}

	cancel()

	select {
	case _, ok := <-out:
		if ok {
			t.Fatalf("received a Status after context cancellation, want channel closed")
		}
	case <-time.After(testSafetyTimeout):
		t.Fatalf("channel was not closed after context cancellation")
	}
}
