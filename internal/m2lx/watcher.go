package m2lx

import (
	"context"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

// reconnectBackoff is the ladder used when the switcher_status socket
// fails to dial or drops. Unlike internal/sender's SRT backoff ladder
// (spec §6.2), which is measured against M2L-X's ~5 s SRT listener
// re-accept refusal window, no equivalent measurement exists for this
// WebSocket: it is a read-only telemetry push, not a single-peer listener,
// so there is no re-accept window to clear. A short, capped ladder is used
// so a flapping status socket reconnects quickly without hammering the
// server; it is a design choice, not a measured constant, and should be
// revisited if M2L-X publishes guidance.
var reconnectBackoff = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
}

// reconnectBackoffCap is the ceiling the ladder holds at once exhausted.
const reconnectBackoffCap = 10 * time.Second

// backoffDuration returns the delay before reconnect attempt n (1-based;
// n<=0 means "no delay, attempt now").
func backoffDuration(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	idx := n - 1
	if idx >= len(reconnectBackoff) {
		return reconnectBackoffCap
	}
	return reconnectBackoff[idx]
}

// tickInterval is how often the watch loop re-checks staleness, pending
// debounce commits, and token rotation when no message has just arrived.
// It must be comfortably under both DebounceWindow (4 s) and StaleAfter
// (15 s) so neither is detected late.
const tickInterval = 1 * time.Second

// wsConn is the subset of *websocket.Conn the Watcher uses. The status
// socket is push-only (windows-app-spec.md §8, CONTRACT.md: "never write
// to it, and do not expect a reply to anything"), so only reading and
// closing are needed; this also lets tests substitute a fake connection
// with no real network I/O. *websocket.Conn satisfies this directly.
type wsConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	Close() error
}

// watcher is the concrete Watcher implementation.
type watcher struct {
	status resolvedHost // status socket host[:port] plus ws-vs-wss, decided once (host.go)
	client Client
	clk    clockSource

	// dial opens the status socket. Overridable in tests so no real
	// network connection is made; production uses dialStatusSocket.
	dial func(ctx context.Context, urlStr string) (wsConn, error)

	// afterTick, when set, is called synchronously at the end of
	// processing each ticker firing, strictly after any Status that
	// tick produced has been sent (or the loop has decided not to send
	// one). It exists only so tests can deterministically observe "this
	// tick has been fully processed" without a real sleep or a busy
	// poll; it is nil, and never called, in production.
	afterTick func()

	// started, when set, is called synchronously once, immediately after
	// the reconnect ticker is created and just before the main select
	// loop begins. It exists purely so tests using a fake clock have a
	// signal that the ticker has been registered before they advance the
	// clock — advancing it earlier would not be observed, since a tick
	// delivered to a ticker that does not exist yet is simply lost. Nil,
	// and never called, in production.
	started func()
}

// var _ Watcher = (*watcher)(nil) is a compile-time check that *watcher
// keeps satisfying Watcher as both evolve.
var _ Watcher = (*watcher)(nil)

// newWatcher constructs the real Watcher for host, authenticating with c.
func newWatcher(host string, c Client) *watcher {
	return &watcher{
		status: resolveHost(host, "status socket"),
		client: c,
		clk:    realClock{},
		dial:   dialStatusSocket,
	}
}

// dialStatusSocket opens the switcher_status WebSocket. It never writes to
// the connection it returns.
func dialStatusSocket(ctx context.Context, urlStr string) (wsConn, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, urlStr, nil)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// statusURL builds <ws-scheme>://<host>/api/v1/switcher_status?access_token=<...>,
// percent-encoding the token as CONTRACT.md requires ("PERCENT-ENCODED
// token"). url.Values.Encode performs standard percent-encoding, which is
// the correct treatment regardless of what characters the token contains.
//
// The scheme is rh.wsScheme(): wss for every production host, and ws only
// when rh was explicitly resolved as insecure (host.go) — never decided
// separately from the REST scheme that produced tok in the first place.
func statusURL(rh resolvedHost, token string) string {
	u := url.URL{
		Scheme:   rh.wsScheme(),
		Host:     rh.hostPort,
		Path:     "/api/v1/switcher_status",
		RawQuery: url.Values{"access_token": {token}}.Encode(),
	}
	return u.String()
}

// connMsg tags a received frame with the generation of the connection it
// arrived on, so a message from a connection that has since been
// superseded (token rotation, error, explicit close) can be told apart
// from one belonging to the current connection.
type connMsg struct {
	gen int
	p   []byte
}

// connErr is connMsg's counterpart for read errors.
type connErr struct {
	gen int
	err error
}

// statusSocket owns one switcher_status connection: dialling it, tagging the
// frames it produces with a generation, backing off when it fails, and
// reopening it when the bearer token in its URL changes underneath it.
//
// It exists because there are now two readers of the same socket shape — Watch,
// which follows one node, and WatchAll, which reports the whole document for
// statusKey discovery — and the connection half of the state machine is
// identical for both. The parts that differ (debounce, staleness, what is
// emitted) stay in the two run loops.
//
// It is not safe for concurrent use: every method is called from the one
// goroutine driving the loop that owns it. The only cross-goroutine traffic is
// msgCh/errCh, written by the per-connection reader goroutine.
type statusSocket struct {
	w *watcher

	conn      wsConn
	lastToken string
	// gen is bumped on every successful dial. A frame or error tagged with an
	// older generation belongs to a connection this loop has already superseded
	// and must be discarded rather than acted on.
	gen int

	msgCh chan connMsg
	errCh chan connErr

	backoffN    int
	nextAttempt time.Time // zero value: attempt immediately
}

func newStatusSocket(w *watcher) *statusSocket {
	return &statusSocket{
		w:     w,
		msgCh: make(chan connMsg),
		errCh: make(chan connErr, 1),
	}
}

// close shuts the current connection, if any. Idempotent.
func (s *statusSocket) close() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

// dial opens a connection and starts its reader goroutine. A failure leaves
// conn nil and is not an error: the caller schedules a retry.
func (s *statusSocket) dial(ctx context.Context) {
	tok := s.w.client.Token()
	c, err := s.w.dial(ctx, statusURL(s.w.status, tok))
	if err != nil {
		s.conn = nil
		return
	}
	s.conn = c
	s.lastToken = tok
	s.gen++
	gen := s.gen
	go func(c wsConn, gen int) {
		for {
			_, p, err := c.ReadMessage()
			if err != nil {
				select {
				case s.errCh <- connErr{gen, err}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case s.msgCh <- connMsg{gen, p}:
			case <-ctx.Done():
				return
			}
		}
	}(c, gen)
}

// dialFirst performs the opening attempt and arms the backoff if it failed.
func (s *statusSocket) dialFirst(ctx context.Context, now time.Time) {
	s.dial(ctx)
	if s.conn == nil {
		s.backoffN = 1
		s.nextAttempt = now.Add(backoffDuration(s.backoffN))
	}
}

// maintain is the once-per-tick connection work: drop the socket if the token
// in its URL has been rotated out from under it, and redial when a scheduled
// attempt has come due.
func (s *statusSocket) maintain(ctx context.Context, now time.Time) {
	if s.conn != nil {
		if tok := s.w.client.Token(); tok != "" && tok != s.lastToken {
			s.close()
		}
	}
	if s.conn == nil && !now.Before(s.nextAttempt) {
		s.dial(ctx)
		if s.conn == nil {
			s.backoffN++
			s.nextAttempt = now.Add(backoffDuration(s.backoffN))
		} else {
			s.backoffN = 0
		}
	}
}

// fail handles a read error from the current connection. Errors from a
// superseded connection are ignored.
func (s *statusSocket) fail(ce connErr, now time.Time) {
	if ce.gen != s.gen {
		return
	}
	s.close()
	s.backoffN++
	s.nextAttempt = now.Add(backoffDuration(s.backoffN))
}

// accept reports whether a frame belongs to the current connection. A frame
// that does resets the backoff ladder: the socket is demonstrably working.
func (s *statusSocket) accept(cm connMsg) bool {
	if cm.gen != s.gen {
		return false
	}
	s.backoffN = 0
	return true
}

// Watch implements Watcher. See run for the full state machine.
func (w *watcher) Watch(ctx context.Context, statusKey string) <-chan Status {
	out := make(chan Status)
	go w.run(ctx, statusKey, out)
	return out
}

// WatchAll implements Watcher. See runAll.
func (w *watcher) WatchAll(ctx context.Context) <-chan Document {
	out := make(chan Document)
	go w.runAll(ctx, out)
	return out
}

// runAll reads the same socket as run and emits every frame as a whole
// Document, with no debounce, no staleness and no filtering by node.
//
// It exists for one job: finding the statusKey. Nothing can tell this
// application which switcher_status node is its own router input — there is no
// endpoint that lists them (spec open question 5) — so the only way to find it
// is to watch every node and see which one starts streaming as our feed comes
// up. Debouncing would blur exactly the transition being looked for, and
// staleness is the Watch loop's business, so neither is applied here.
//
// A frame that is not a JSON object is dropped: unlike Watch, which has a
// caller waiting on a specific node, there is nothing useful to say about a
// malformed snapshot to somebody who is guessing.
func (w *watcher) runAll(ctx context.Context, out chan<- Document) {
	defer close(out)

	sock := newStatusSocket(w)
	defer sock.close()

	sock.dialFirst(ctx, w.clk.Now())

	ticker := w.clk.NewTicker(tickInterval)
	defer ticker.Stop()

	if w.started != nil {
		w.started()
	}

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C():
			sock.maintain(ctx, w.clk.Now())
			if w.afterTick != nil {
				w.afterTick()
			}

		case ce := <-sock.errCh:
			sock.fail(ce, w.clk.Now())

		case cm := <-sock.msgCh:
			if !sock.accept(cm) {
				continue
			}
			nodes, err := extractAll(cm.p)
			if err != nil {
				continue
			}
			select {
			case out <- Document{Nodes: nodes, At: w.clk.Now()}:
			case <-ctx.Done():
				return
			}
		}
	}
}

// run is the Watcher's entire state machine: connect, read, debounce
// stream_state, detect staleness, reconnect with backoff on drop, and
// reopen the socket when the client's token changes underneath it (the
// token lives in the URL, so a Client-side Refresh is otherwise invisible
// to an already-open socket). It owns no locks and touches no shared state
// beyond reading w.client.Token(); everything else is local to this
// goroutine, so the only concurrency hazard is the generation-tagging on
// connMsg/connErr, which exists specifically to discard messages/errors
// from a connection this loop has already superseded.
func (w *watcher) run(ctx context.Context, statusKey string, out chan<- Status) {
	defer close(out)

	deb := newDebouncer(DebounceWindow)
	now := w.clk.Now()
	lastMsg := now
	var lastVideo VideoFormat
	lastAudio := []AudioFormat{}

	sock := newStatusSocket(w)
	defer sock.close()

	emit := func(s Status) bool {
		select {
		case out <- s:
			return true
		case <-ctx.Done():
			return false
		}
	}

	sock.dialFirst(ctx, now)

	ticker := w.clk.NewTicker(tickInterval)
	defer ticker.Stop()

	if w.started != nil {
		w.started()
	}

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C():
			now := w.clk.Now()

			sock.maintain(ctx, now)

			if sv, changed := deb.Tick(now); changed {
				if !emit(Status{StreamState: sv, Video: lastVideo, Audio: lastAudio, At: now, Stale: false}) {
					return
				}
			}

			if now.Sub(lastMsg) >= StaleAfter {
				if !emit(Status{Stale: true, At: now}) {
					return
				}
			}

			if w.afterTick != nil {
				w.afterTick()
			}

		case ce := <-sock.errCh:
			sock.fail(ce, w.clk.Now())

		case cm := <-sock.msgCh:
			if !sock.accept(cm) {
				continue
			}
			now := w.clk.Now()
			lastMsg = now

			node, ok, err := extractNode(cm.p, statusKey)
			if err != nil || !ok {
				// Malformed frame, or a snapshot that does not mention
				// our statusKey this time: lastMsg above already proves
				// the socket is alive, so this is not staleness — there
				// is just nothing new for our node in this message.
				continue
			}

			video := parseVideoFormat(node.Streams.Video.Format)
			audio := make([]AudioFormat, 0, len(node.Streams.Audio))
			for _, a := range node.Streams.Audio {
				audio = append(audio, parseAudioFormat(a.Format))
			}
			lastVideo, lastAudio = video, audio

			sv, _ := deb.Observe(node.StreamState, now)
			if !emit(Status{StreamState: sv, Video: video, Audio: audio, At: now, Stale: false}) {
				return
			}
		}
	}
}
