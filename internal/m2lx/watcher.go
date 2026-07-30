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
	host   string
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

// newWatcher constructs the real Watcher for host, authenticating with c.
func newWatcher(host string, c Client) *watcher {
	return &watcher{
		host:   host,
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

// statusURL builds wss://<host>/api/v1/switcher_status?access_token=<...>,
// percent-encoding the token as CONTRACT.md requires ("PERCENT-ENCODED
// token"). url.Values.Encode performs standard percent-encoding, which is
// the correct treatment regardless of what characters the token contains.
func statusURL(host, token string) string {
	u := url.URL{
		Scheme:   "wss",
		Host:     host,
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

// Watch implements Watcher. See run for the full state machine.
func (w *watcher) Watch(ctx context.Context, statusKey string) <-chan Status {
	out := make(chan Status)
	go w.run(ctx, statusKey, out)
	return out
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

	var conn wsConn
	var lastToken string
	curGen := 0
	msgCh := make(chan connMsg)
	errCh := make(chan connErr, 1)

	closeConn := func() {
		if conn != nil {
			conn.Close()
			conn = nil
		}
	}
	defer closeConn()

	dialNow := func() {
		tok := w.client.Token()
		c, err := w.dial(ctx, statusURL(w.host, tok))
		if err != nil {
			conn = nil
			return
		}
		conn = c
		lastToken = tok
		curGen++
		gen := curGen
		go func(c wsConn, gen int) {
			for {
				_, p, err := c.ReadMessage()
				if err != nil {
					select {
					case errCh <- connErr{gen, err}:
					case <-ctx.Done():
					}
					return
				}
				select {
				case msgCh <- connMsg{gen, p}:
				case <-ctx.Done():
					return
				}
			}
		}(c, gen)
	}

	emit := func(s Status) bool {
		select {
		case out <- s:
			return true
		case <-ctx.Done():
			return false
		}
	}

	backoffN := 0
	var nextAttempt time.Time // zero value: attempt immediately

	dialNow()
	if conn == nil {
		backoffN = 1
		nextAttempt = now.Add(backoffDuration(backoffN))
	}

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

			if conn != nil {
				if tok := w.client.Token(); tok != "" && tok != lastToken {
					closeConn()
				}
			}
			if conn == nil && !now.Before(nextAttempt) {
				dialNow()
				if conn == nil {
					backoffN++
					nextAttempt = now.Add(backoffDuration(backoffN))
				} else {
					backoffN = 0
				}
			}

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

		case ce := <-errCh:
			if ce.gen == curGen {
				closeConn()
				backoffN++
				nextAttempt = w.clk.Now().Add(backoffDuration(backoffN))
			}

		case cm := <-msgCh:
			if cm.gen != curGen {
				continue
			}
			now := w.clk.Now()
			lastMsg = now
			backoffN = 0

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
