package remote

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// A session is one authenticated WebSocket connection. Its whole job is to never
// let a slow or hostile client harm anyone else: not the broadcast (which fans
// out to every session and must never block), not the process (which must be
// able to shut this connection down within a budget), and not an in-flight call
// (whose context must be cancelled the instant the socket dies).
//
// The discipline mirrors app.go's eventPump, which documents that the event
// producers — the status watcher on an unbuffered channel, the sender in front
// of a reconnect machine — must never be stalled by a consumer. A remote fan-out
// that blocked would reintroduce exactly that stall over a LAN link, which is
// strictly worse than a local WebView2 renderer falling behind. So events go
// through a bounded queue that DISCARDS THE OLDEST under back pressure and a
// per-connection monotonic seq lets the client detect the drop, while results —
// which must not be dropped, or a caller's promise hangs — go through a separate
// queue whose depth equals the in-flight cap, so it can never overflow under
// normal operation and unblocks on teardown when it cannot.

const (
	// writeWait is the deadline on a single socket write. A client whose TCP
	// receive window has been full this long is not draining, and the write
	// fails, which tears the session down — the mechanism by which a
	// non-draining client is DROPPED rather than allowed to block the writer.
	writeWait = 10 * time.Second
	// pongWait is how long a silent client may go before it is presumed gone.
	// The read deadline is set to now+pongWait and pushed forward on every pong.
	pongWait = 60 * time.Second
	// pingPeriod is how often a ping is sent, comfortably inside pongWait so a
	// healthy but quiet client is kept alive rather than reaped.
	pingPeriod = (pongWait * 9) / 10
	// maxMessageSize bounds an inbound call frame. A control channel's calls are
	// small; an unbounded read is a memory-exhaustion lever on a peer that has
	// already authenticated but may still be hostile.
	maxMessageSize = 1 << 20
	// outboundEventDepth is the bounded event queue depth, matching the spirit
	// of app.go's eventQueueDepth. Deep enough to ride out a burst, shallow
	// enough that a client that stops draining is noticed quickly.
	outboundEventDepth = 64
	// maxInFlightCalls caps concurrent dispatches per client. A client that
	// exceeds it is answered with an error result for the overflow calls rather
	// than having its read loop blocked — blocking the read loop would also stop
	// pong processing and get the connection reaped for the wrong reason.
	maxInFlightCalls = 32
)

// session holds the per-connection state. One goroutine reads (the caller of
// serve), one goroutine writes (started by serve); gorilla permits exactly one
// concurrent reader and one concurrent writer and this respects that — the write
// pump is the ONLY caller of WriteMessage.
type session struct {
	conn       *websocket.Conn
	dispatcher Dispatcher
	info       ClientInfo
	events     []string
	logf       func(string, ...any)

	// ctx is cancelled on teardown; every in-flight call derives from it, so a
	// disconnect cancels every call context. done is closed alongside so a
	// blocked result send unparks.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// results is drained by the write pump. Its depth equals the in-flight cap,
	// so with at most that many calls outstanding it never overflows under
	// normal operation; a send that cannot complete means the writer is wedged
	// and is unblocked by teardown via done.
	results chan []byte
	// eventsCh is the bounded, drop-oldest queue. enqueueEvent never blocks.
	eventsCh chan []byte

	seq       uint64 // per-connection monotonic event sequence (atomic)
	inFlight  int32  // concurrent dispatches (atomic)
	closeOnce sync.Once

	// writeDeadline is the per-write timeout, defaulting to the writeWait const.
	// It is a field, not the const directly, only so a test can shorten it to
	// force the stalled-writer path quickly — the same reason the authenticator
	// exposes minDelay. Production never changes it.
	writeDeadline time.Duration
}

// newSession wires a session onto an accepted connection. parentCtx is the
// server's context, so a server shutdown cancels every session's calls too.
func newSession(parentCtx context.Context, conn *websocket.Conn, info ClientInfo, d Dispatcher, events []string, logf func(string, ...any)) *session {
	ctx, cancel := context.WithCancel(parentCtx)
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &session{
		conn:          conn,
		dispatcher:    d,
		info:          info,
		events:        events,
		logf:          logf,
		ctx:           ctx,
		cancel:        cancel,
		done:          make(chan struct{}),
		results:       make(chan []byte, maxInFlightCalls),
		eventsCh:      make(chan []byte, outboundEventDepth),
		writeDeadline: writeWait,
	}
}

// writeHello sends the one hello frame, synchronously, before either pump runs.
//
// It must be first on the wire: the shim installs window.go.main.App from this
// frame's Methods list, and an event or result arriving ahead of it would have
// nowhere to land. Because no pump is running yet, this is the only writer, so
// the single-writer invariant is not violated by writing here directly.
func (s *session) writeHello() error {
	methods := s.dispatcher.Methods(s.info)
	hello := HelloFrame{
		T:       FrameHello,
		Client:  s.info.ID,
		Methods: methods,
		Events:  s.events,
	}
	data, err := json.Marshal(hello)
	if err != nil {
		return err
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.writeDeadline))
	return s.conn.WriteMessage(websocket.TextMessage, data)
}

// serve runs the read pump in the calling goroutine and the write pump in a
// second goroutine, returning only when the connection is fully torn down. The
// caller (the server's WS handler) registers the session for broadcast before
// calling serve and unregisters it after.
func (s *session) serve() {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.writePump()
	}()
	s.readPump()
	s.close()
	wg.Wait()
}

// readPump reads and dispatches call frames until the socket fails. It is the
// sole caller of ReadMessage.
func (s *session) readPump() {
	s.conn.SetReadLimit(maxMessageSize)
	_ = s.conn.SetReadDeadline(time.Now().Add(pongWait))
	s.conn.SetPongHandler(func(string) error {
		// A pong is proof the client is alive; push the read deadline forward.
		return s.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, raw, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		call, err := decodeCall(raw)
		if err != nil {
			// A peer that cannot speak the protocol is disconnected rather than
			// guessed at; see errProtocol.
			s.logf("remote: seat %s from %s: %v", s.info.ID, s.info.RemoteAddr, err)
			return
		}
		s.startCall(call)
	}
}

// startCall dispatches one call, enforcing the concurrency cap without blocking
// the read loop. Over the cap, the call is refused with an error result so the
// reader keeps servicing pongs; under it, the call runs on its own goroutine
// with a context derived from the session so teardown cancels it.
func (s *session) startCall(call CallFrame) {
	if atomic.AddInt32(&s.inFlight, 1) > maxInFlightCalls {
		atomic.AddInt32(&s.inFlight, -1)
		s.enqueueResult(ResultFrame{
			T:     FrameResult,
			ID:    call.ID,
			OK:    false,
			Error: "remote: too many concurrent calls",
		})
		return
	}
	go func() {
		defer atomic.AddInt32(&s.inFlight, -1)
		value, err := s.dispatcher.Call(s.ctx, s.info, call.Method, call.Args)
		res := ResultFrame{T: FrameResult, ID: call.ID}
		if err != nil {
			res.OK = false
			res.Error = err.Error()
		} else {
			res.OK = true
			res.Value = value
		}
		s.enqueueResult(res)
	}()
}

// writePump is the sole writer: it drains results and events and sends periodic
// pings, applying a write deadline to every frame so a stuck write tears the
// session down instead of hanging the writer forever.
//
// It calls close() on the way out, symmetric with serve() calling it after
// readPump. Without this a DEAD WRITER strands the reader: a client over the
// in-flight cap is answered by a synchronous enqueueResult on the read-pump
// goroutine (startCall), which parks on `select{ results<- ; <-done }`; and up
// to maxInFlightCalls dispatch goroutines park the same way. If the writer has
// died — a hostile authenticated client that stops draining fills its TCP
// window and trips the writeWait deadline — nothing drains results and done is
// never closed, so the read pump never returns, serve() never returns, and the
// session leaks a goroutine, a phantom seat on the operator's indicator, and
// per-broadcast work, on the machine that is on air. close() is idempotent
// (closeOnce), so calling it here as well as in serve() is safe.
func (s *session) writePump() {
	defer s.close()
	ping := time.NewTicker(pingPeriod)
	defer ping.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.done:
			return
		case frame := <-s.results:
			if !s.write(frame) {
				return
			}
		case frame := <-s.eventsCh:
			if !s.write(frame) {
				return
			}
		case <-ping.C:
			_ = s.conn.SetWriteDeadline(time.Now().Add(s.writeDeadline))
			if err := s.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// write sends one text frame with a deadline, returning false (which ends the
// write pump) on failure.
func (s *session) write(frame []byte) bool {
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.writeDeadline))
	if err := s.conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		return false
	}
	return true
}

// enqueueResult queues a call's answer. It must not be dropped — a lost result
// hangs the caller's promise — so it blocks until the write pump takes it or the
// session tears down. It cannot block for long: results depth equals the
// in-flight cap, so there is always room under normal operation, and a wedged
// writer is unblocked by done when the write deadline fires and teardown runs.
func (s *session) enqueueResult(res ResultFrame) {
	data, err := json.Marshal(res)
	if err != nil {
		// A result we cannot even marshal is replaced with a marshalable error,
		// so the caller still gets a rejection rather than silence.
		data, _ = json.Marshal(ResultFrame{T: FrameResult, ID: res.ID, OK: false, Error: "remote: result encoding failed"})
	}
	select {
	case s.results <- data:
	case <-s.done:
	}
}

// enqueueEvent queues one event for this client, NEVER blocking. It assigns the
// per-connection seq, marshals, and pushes with the exact three-step
// drop-oldest discipline app.go's eventPump uses: try to send; if full, discard
// the oldest and try once more; if still full, discard this one. A client that
// has stopped draining loses events (and sees the seq gap) rather than stalling
// the broadcast — and its dead socket is reaped separately by the write
// deadline.
func (s *session) enqueueEvent(name string, data any) {
	seq := atomic.AddUint64(&s.seq, 1)
	frame, err := json.Marshal(EventFrame{T: FrameEvent, Seq: seq, Name: name, Data: data})
	if err != nil {
		return
	}
	select {
	case s.eventsCh <- frame:
		return
	default:
	}
	select {
	case <-s.eventsCh:
	default:
	}
	select {
	case s.eventsCh <- frame:
	default:
	}
}

// close tears the session down exactly once: it cancels the call context
// (cancelling every in-flight call), signals the write pump via done, and closes
// the socket (which unparks the read pump). It is safe to call from either pump
// and from the server's shutdown.
func (s *session) close() {
	s.closeOnce.Do(func() {
		s.cancel()
		close(s.done)
		_ = s.conn.Close()
	})
}
