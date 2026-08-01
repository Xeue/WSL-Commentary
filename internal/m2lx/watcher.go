package m2lx

import (
	"context"
	"log"
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

// resyncInterval is how often the Watch loop closes a perfectly healthy status
// socket and opens a new one, purely to be handed a fresh whole-document
// snapshot.
//
// THIS IS A BACKSTOP AGAINST AN UNPROVEN ASSUMPTION, and it should be deleted
// as soon as the assumption is settled. document.go now merges subtree deltas,
// so IF a stream_state change is pushed — at "/" or at any subtree path — the
// lamps follow it within a frame and this costs a dial a minute for nothing.
// But nobody has ever observed an input change state on this socket: the 150 s
// / 3180 frame measurement that established the protocol (wire.go) caught no
// transition, because none happened, and making one happen means starting or
// stopping a feed on a live switcher. So it is genuinely NOT KNOWN whether a
// transition is pushed at all. If it is only ever in the opening snapshot,
// merging deltas cannot help and only a reconnect can — hence this.
//
// TO REMOVE IT: watch the socket across a real transition, record the (node,
// path) of every frame it produces, write that into wire.go beside the rest of
// the measurement, and delete this constant and statusSocket.resync with it.
// If the transition turns out to arrive as a delta, nothing else changes; if it
// arrives only at "/", this becomes the mechanism rather than the backstop and
// should be documented as such rather than left looking incidental.
//
// WHY THIRTY SECONDS. It has to be well clear of both timing rules so a resync
// can never be mistaken for one of them: it is 7.5x DebounceWindow (4 s), so a
// resync cannot land inside a pending debounce and resolve it, and 2x
// StaleAfter (15 s), so the two cadences never beat against each other. A
// successful resync is invisible to staleness anyway — the gap it opens is one
// dial, and the fresh connection's first act is to push the snapshot — while a
// resync whose redial FAILS falls into the ordinary backoff ladder and lets
// staleness fire at 15 s, which is the correct and honest outcome.
//
// It is not longer than that because thirty seconds is also the worst case for
// how long a lamp can lie if the assumption above turns out to be true, and a
// lamp that says the operator is off air while they are talking is the whole
// complaint. It is not shorter because the cost is one TLS handshake and one
// 84 KB frame each time; at 30 s that is under 3 KB/s against a socket already
// pushing about 21 frames a second, which is noise, but at 5 s it would start
// to look like an attack on the switcher.
const resyncInterval = 30 * time.Second

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

// resync closes a HEALTHY connection and immediately opens a new one, so that
// the next frame is a fresh whole-document snapshot rather than another
// subtree delta. See resyncInterval for why this exists and when to delete it.
//
// It does nothing when there is no connection: a socket that is already down
// is going to be redialled by maintain, and that redial produces exactly the
// same fresh snapshot. Redialling on top of it would only fight the backoff
// ladder.
//
// The redial is immediate rather than scheduled, because the socket was
// demonstrably working a moment ago and this loop is the one that broke it. If
// the redial nevertheless fails, the ordinary ladder takes over from rung one
// and staleness fires at StaleAfter if it keeps failing — the resync must not
// be able to leave the loop in a state the normal reconnect path does not
// already handle.
func (s *statusSocket) resync(ctx context.Context, now time.Time) {
	if s.conn == nil {
		return
	}
	s.close()
	s.dial(ctx)
	if s.conn == nil {
		s.backoffN++
		s.nextAttempt = now.Add(backoffDuration(s.backoffN))
		return
	}
	s.backoffN = 0
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
//
// A frame carrying no whole-node states is dropped too, and that is not an
// optimisation. The socket is snapshot-then-delta (wire.go): after the opening
// snapshot, all but a handful of the ~21 frames a second are audio-meter
// updates for a node that is not a router input, and they yield an EMPTY
// Document. Emitting those would be worse than useless — Discovery takes its
// baseline from the first Document it is given, and an empty baseline makes
// every already-streaming input on the switcher look like a node that appeared
// from nowhere and started streaming, i.e. a candidate. That is exactly the
// "three green lamps against somebody else's feed" failure discover.go exists
// to prevent.
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
			if err != nil || len(nodes) == 0 {
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

// lampView is exactly the three things the WebSocket-derived lamps read:
// stream_state, streams.video.format and streams.audio (CONTRACT.md,
// windows-app-spec.md §8). Nothing else belongs in it — adding a field would
// make the Watch loop emit on frames the UI has no use for, and adding
// statistics.bitrate in particular would emit about once a second per input
// while advertising a number that freezes on a dead feed (see the warnings on
// wireNode and Status).
//
// It is the "has anything actually changed?" comparison, not a payload; the
// Status the caller receives is built from the same values beside it.
type lampView struct {
	state string
	video VideoFormat
	audio []AudioFormat
}

// equal reports whether two readings would light the lamps identically.
//
// It compares the PARSED formats rather than the raw JSON, because parsed is
// what the lamps read: a firmware that reorders the keys of the format object
// between frames has changed nothing an operator can see, and should not
// produce an event saying it has. VideoFormat and AudioFormat are all scalar
// fields, so == is the whole comparison; only the audio slice needs a loop,
// and its LENGTH matters as much as its contents — an empty audio array is the
// MP2/AC-3 silent-drop signature (see Status.Audio).
func (l lampView) equal(o lampView) bool {
	if l.state != o.state || l.video != o.video || len(l.audio) != len(o.audio) {
		return false
	}
	for i := range l.audio {
		if l.audio[i] != o.audio[i] {
			return false
		}
	}
	return true
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

	// The statusKey-mismatch report. Deciding a statusKey is wrong takes the
	// SNAPSHOT that opens a connection, never a delta: a delta mentions one
	// node and says nothing about the other 35 (wire.go). So the run loop
	// remembers, per connection, what the snapshot enumerated.
	//
	//   inputs      every router input seen at path "/" on this connection,
	//               by node name. Set from the opening snapshot and merged
	//               from any later whole-node state, so an input created
	//               mid-session is picked up.
	//   enumerated  true once a frame carrying whole-node states has been
	//               seen. Until then nothing is known and nothing is claimed.
	//   keyErr      the report currently standing, "" when the key matches.
	//               Held so it is logged and emitted on the transition and on
	//               a heartbeat, not at the 21 frames a second the socket
	//               actually delivers.
	inputs := map[string]NodeChoice{}
	enumerated := false
	keyErr := ""
	var keyErrNext time.Time
	sockGen := 0

	// The mirrored document, and the last lamp reading taken from it.
	//
	//   doc         the opening snapshot with every subsequent subtree delta
	//               merged in (document.go). Without it the lamps freeze at
	//               the state of the first frame, which is the bug this
	//               replaced; a delta is only meaningful against a baseline.
	//   lamps       the last values EMITTED for the three WebSocket-derived
	//               lamps. The socket pushes about 21 frames a second and
	//               roughly fifteen of them are audio meters, so a Status per
	//               frame would be pure noise on the Wails event bus: a Status
	//               is emitted only when one of these three actually moves.
	//   lampsKnown  false before the first reading, and again after any Stale
	//               Status, so coming back from grey always re-emits even if
	//               nothing changed while the socket was quiet.
	doc := newDocument()
	var lamps lampView
	lampsKnown := false

	// resyncAt is when to close a healthy socket and take a fresh snapshot.
	// See resyncInterval: it is a backstop against an assumption nobody has
	// been able to test, not a routine part of the protocol.
	resyncAt := now.Add(resyncInterval)

	sock := newStatusSocket(w)
	defer sock.close()

	emit := func(s Status) bool {
		if s.Stale {
			// A grey lamp is not a reading, so the next real one must be
			// emitted whatever it says: the UI has to be told to come out of
			// grey even when the values underneath never moved.
			lampsKnown = false
		}
		select {
		case out <- s:
			return true
		case <-ctx.Done():
			return false
		}
	}

	// reportKeyErr publishes the standing statusKey report. It logs and emits
	// on the transition, and thereafter only on a StaleAfter heartbeat, so a
	// misconfiguration costs one log line rather than twenty-one a second —
	// while a page that loaded after the first report still learns within
	// fifteen seconds why its lamps are grey.
	reportKeyErr := func(msg string, now time.Time) bool {
		if msg == keyErr && now.Before(keyErrNext) {
			return true
		}
		if msg != keyErr {
			log.Print(msg)
		}
		keyErr = msg
		keyErrNext = now.Add(StaleAfter)
		return emit(Status{Stale: true, KeyError: msg, At: now})
	}

	// availableInputs renders what has been enumerated on this connection, for
	// the report.
	availableInputs := func() []NodeChoice {
		out := make([]NodeChoice, 0, len(inputs))
		for _, c := range inputs {
			out = append(out, c)
		}
		sortChoices(out)
		return out
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

			// Before maintain, so a resync whose redial fails schedules its
			// retry on the ladder and maintain does not immediately dial a
			// second time in the same tick.
			if !now.Before(resyncAt) {
				resyncAt = now.Add(resyncInterval)
				sock.resync(ctx, now)
			}

			sock.maintain(ctx, now)

			if sv, changed := deb.Tick(now); changed {
				if !emit(Status{StreamState: sv, Video: lastVideo, Audio: lastAudio, At: now, Stale: false}) {
					return
				}
			}

			switch {
			case now.Sub(lastMsg) >= StaleAfter:
				// The socket itself has gone quiet. A statusKey report
				// still standing rides along: it is still true, and it is
				// the more actionable of the two facts.
				keyErrNext = now.Add(StaleAfter)
				if !emit(Status{Stale: true, KeyError: keyErr, At: now}) {
					return
				}
			case keyErr != "":
				// The socket is delivering perfectly and the key still
				// matches nothing. Staleness will therefore never fire,
				// which is precisely why this failure used to be silent.
				if !reportKeyErr(keyErr, now) {
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

			// A new connection begins a new snapshot. What the last one
			// enumerated says nothing about this one — the switcher may
			// have been reconfigured while we were away. The document goes
			// with it: merging this connection's deltas into the last
			// connection's baseline would be merging into a document that
			// may describe a switcher that no longer exists.
			if cm.gen != sockGen {
				sockGen = cm.gen
				inputs = map[string]NodeChoice{}
				enumerated = false
				doc = newDocument()
				lamps, lampsKnown = lampView{}, false
				// The snapshot this connection is about to deliver IS a
				// resync, however it was caused, so the clock restarts here
				// rather than running free from whenever the last one was.
				resyncAt = now.Add(resyncInterval)
			}

			// Merge before reading anything. After the opening snapshot every
			// frame is a SUBTREE delta (wire.go) and means nothing on its own;
			// it is only the merged document that has a whole node in it.
			eff, err := doc.apply(cm.p)
			if err != nil {
				// Not a switcher_status frame at all. The document is
				// untouched — see apply — and lastMsg above already proves
				// the socket is alive, so this is not staleness either. It
				// is also NOT evidence about the statusKey: claiming it were
				// would send an operator hunting for a node name over a
				// dropped frame.
				continue
			}

			look, err := lookupNode(cm.p, statusKey)
			if err != nil {
				// Our own node, at path "/", in a shape this package does
				// not understand. Same reasoning as above: nothing usable,
				// and nothing said about the statusKey.
				continue
			}

			// Only a frame carrying whole-node states enumerates anything.
			// Every delta measured carried none (wire.go).
			for _, c := range look.Inputs {
				inputs[c.Key] = c
			}
			if len(look.Inputs) > 0 {
				enumerated = true
			}

			// Whether this frame said anything about OUR node. Two ways it
			// can: it carried the node whole (the snapshot), or one of its
			// deltas merged into it (a "/statistics" update on our own
			// input). doc.streamNode is the reading either way, so the lamps
			// have exactly one source and a merged node and a freshly
			// snapshotted one cannot drift apart.
			node, haveNode := doc.streamNode(statusKey)
			if !haveNode || !eff.Touched[statusKey] {
				_, known := inputs[statusKey]
				switch {
				case look.NotAnInput:
					// Conclusive from this frame alone: the node is
					// there, at path "/", and has no stream_state.
				case enumerated && !known:
					// The opening snapshot listed the switcher and our
					// key was not on it.
				default:
					// A delta about somebody else, a delta before any
					// snapshot, or a connection whose snapshot has not
					// arrived yet. None says anything about our node, so
					// none is reported.
					continue
				}
				msg := (&StatusKeyNotFoundError{
					Key:        statusKey,
					NotAnInput: look.NotAnInput,
					Available:  availableInputs(),
				}).Error()
				if !reportKeyErr(msg, now) {
					return
				}
				continue
			}
			keyErr = ""

			video := parseVideoFormat(node.Streams.Video.Format)
			audio := make([]AudioFormat, 0, len(node.Streams.Audio))
			for _, a := range node.Streams.Audio {
				audio = append(audio, parseAudioFormat(a.Format))
			}
			// Held for the ticker's debounce-commit path, which emits a
			// stream_state change with no fresh frame to read formats from.
			lastVideo, lastAudio = video, audio

			sv, committed := deb.Observe(node.StreamState, now)

			// The emit gate. "/levels" arrives about fifteen times a second
			// and carries nothing any lamp reads; "/statistics" carries a
			// bitrate this package deliberately does not read at all. Emitting
			// on every merge would put ~21 events a second on the Wails bus to
			// say nothing. So a Status goes out only when one of the three
			// values the lamps actually read has moved — or when the debounce
			// committed one, or when there is no previous reading to compare
			// against, which includes every recovery from a Stale Status.
			cur := lampView{state: node.StreamState, video: video, audio: audio}
			if lampsKnown && !committed && cur.equal(lamps) {
				continue
			}
			lamps, lampsKnown = cur, true

			if !emit(Status{StreamState: sv, Video: video, Audio: audio, At: now, Stale: false}) {
				return
			}
		}
	}
}
