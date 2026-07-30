package m2lx

import (
	"context"
	"errors"
	"fmt"
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

const testSafetyTimeout = 5 * time.Second

func newTestWatcher(client Client) *testWatcher {
	tw := &testWatcher{
		clk:      newFakeClock(base),
		dials:    make(chan string, 64),
		nextConn: make(chan wsConnOrErr, 64),
		ticked:   make(chan struct{}, 64),
		started:  make(chan struct{}),
	}
	tw.w = &watcher{
		host:   "m2lx.example.invalid",
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
		}
	}()
	return func() {
		select {
		case <-done:
		case <-time.After(testSafetyTimeout):
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

func statusPayload(statusKey, streamState, videoFmt string, audioFmts ...string) []byte {
	audio := ""
	for i, a := range audioFmts {
		if i > 0 {
			audio += ","
		}
		audio += fmt.Sprintf(`{"format":%q}`, a)
	}
	return []byte(fmt.Sprintf(`{%q:{"stream_state":%q,"streams":{"video":{"format":%q},"audio":[%s]}}}`,
		statusKey, streamState, videoFmt, audio))
}

func TestWatcher_ConnectAndReceiveMessage(t *testing.T) {
	client := &fakeClientToken{token: "tok-1"}
	tw := newTestWatcher(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := tw.w.Watch(ctx, "cam7")

	url := <-tw.dials
	if url != statusURL("m2lx.example.invalid", "tok-1") {
		t.Fatalf("dial URL = %q", url)
	}
	conn := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn}

	conn.push(statusPayload("cam7", "streaming", "h264 1920x1080 50 P", "aac 48000 2ch"))

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

	conn.push(statusPayload("cam7", "streaming", "h264 1920x1080 50 P"))

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

	conn.push(statusPayload("cam7", "streaming", "h264 1920x1080 50 P", "aac 48000 2ch"))
	s := recvStatus(t, out)
	if s.StreamState != "streaming" {
		t.Fatalf("first StreamState = %q", s.StreamState)
	}

	// A momentary drop, well inside the 4s DebounceWindow: must not surface
	// as a change.
	conn.push(statusPayload("cam7", "stopped", "h264 1920x1080 50 P", "aac 48000 2ch"))
	s = recvStatus(t, out)
	if s.StreamState != "streaming" {
		t.Fatalf("StreamState during a momentary flap = %q, want it to stay streaming", s.StreamState)
	}

	conn.push(statusPayload("cam7", "streaming", "h264 1920x1080 50 P", "aac 48000 2ch"))
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

	conn.push(statusPayload("cam7", "streaming", "h264 1920x1080 50 P", "aac 48000 2ch"))
	recvStatus(t, out) // consume the initial "streaming"

	// stream_state goes bad and STAYS bad: after DebounceWindow (4s, i.e. 4
	// ticks at tickInterval=1s) the commit must surface even with no
	// further message, via the ticker's Tick path.
	conn.push(statusPayload("cam7", "stopped", "h264 1920x1080 50 P", "aac 48000 2ch"))
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

	conn.push(statusPayload("cam7", "streaming", "h264 1920x1080 50 P", "aac 48000 2ch"))
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
	conn.push(statusPayload("cam7", "streaming", "h264 1920x1080 50 P", "aac 48000 2ch"))
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

	conn1.push(statusPayload("cam7", "streaming", "h264 1920x1080 50 P", "aac 48000 2ch"))
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

	var redialURL string
	for i := 0; i < 30 && redialURL == ""; i++ {
		wait := tw.tick()
		wait()
		select {
		case redialURL = <-tw.dials:
		default:
		}
	}
	if redialURL == "" {
		t.Fatalf("no redial observed after dropping the connection")
	}

	conn2.push(statusPayload("cam7", "streaming", "h264 1920x1080 50 P", "aac 48000 2ch"))
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
	if firstURL != statusURL("m2lx.example.invalid", "tok-1") {
		t.Fatalf("initial dial URL = %q", firstURL)
	}
	conn1 := newFakeConn()
	tw.nextConn <- wsConnOrErr{conn: conn1}
	tw.waitStarted(t)

	conn1.push(statusPayload("cam7", "streaming", "h264 1920x1080 50 P", "aac 48000 2ch"))
	recvStatus(t, out)

	// Client refreshed: the token in the URL is now stale, and nothing
	// tells the open socket directly, per CONTRACT.md: "a refresh means
	// reopening the socket. Handle that."
	client.set("tok-2")

	conn2 := newFakeConn()
	wait := tw.tick() // detects the rotation, closes conn1, redials immediately
	secondURL := <-tw.dials
	if secondURL != statusURL("m2lx.example.invalid", "tok-2") {
		t.Fatalf("dial URL after rotation = %q, want the new token", secondURL)
	}
	tw.nextConn <- wsConnOrErr{conn: conn2}
	wait()

	conn2.push(statusPayload("cam7", "streaming", "h264 1920x1080 50 P", "aac 48000 2ch"))
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

	conn.push(statusPayload("cam7", "streaming", "h264 1920x1080 50 P", "aac 48000 2ch"))
	recvStatus(t, out)

	// A malformed frame must not panic the watcher goroutine and must not
	// itself produce a Status.
	conn.push([]byte(`{not valid json`))
	expectNoStatus(t, out)

	// The watcher must still be alive and processing subsequent good
	// frames.
	conn.push(statusPayload("cam7", "streaming", "h264 1920x1080 50 P", "aac 48000 2ch"))
	s := recvStatus(t, out)
	if s.StreamState != "streaming" {
		t.Fatalf("StreamState after a malformed frame = %q", s.StreamState)
	}
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
