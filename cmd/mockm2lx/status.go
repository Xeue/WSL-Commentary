// The switcher_status WebSocket: GET /api/v1/switcher_status?access_token=...
//
// Push-only, as the real socket is (spec section 8: the server silently
// ignores client data frames — docs/archive-windows-app-spec-v1-rejected.md
// line 938). This mock never reads application data from a client, only
// enough to notice when the client closes the connection.
//
// The URL and query parameter name match the one confirmed usage found in the
// bundle for the sibling switcher_controller socket (docs/architecture.md line
// 428, `wss://<host>/api/v1/switcher_controller?access_token=<percent-encoded>`);
// switcher_status uses the same scheme
// (docs/archive-windows-app-spec-v1-rejected.md line 934).
//
// WHAT THIS FILE OWNS is the CONNECTION half of the protocol; switcherdoc.go
// owns the document. The division matters because the protocol is
// snapshot-then-delta and the snapshot is a property of the CONNECTION, not of
// the clock: frame 0 after an upgrade is the whole document, and everything
// after it is a subtree delta that means nothing without it. So a client is
// tracked as baselined or not, and a client that has not had frame 0 — because
// it connected while the stall fault was set — gets one before it is sent any
// delta. A delta merged into an empty document is a delta discarded
// (internal/m2lx/document.go), so sending one to an unbaselined client would
// be sending it nothing at all.
package main

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// upgrader has CheckOrigin disabled: this mock is a local development and test
// tool, never exposed beyond localhost/a test harness, and the real M2L-X
// instance's CORS policy is not something this package needs to reproduce.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// wsClient is one connected status-socket client. There is deliberately no
// per-client fault state: every fault in this mock is process-wide, matching
// how a real test drives the one M2L-X instance the app under test talks to.
//
// writeMu serialises writes to conn. gorilla/websocket allows at most one
// concurrent writer; without this, the push-on-connect (handleStatusWS) and
// the broadcaster (runStatusBroadcaster) could both write to a just-registered
// client at the same time, from two different goroutines.
//
// baselined records whether this connection has been sent frame 0. It is
// atomic rather than guarded by writeMu because the broadcaster reads it to
// decide what to send BEFORE taking any write lock, and a read that had to
// queue behind an in-flight 84 KB snapshot write would serialise the whole
// broadcast on the slowest client.
type wsClient struct {
	conn      *websocket.Conn
	writeMu   sync.Mutex
	baselined atomic.Bool
}

// handleStatusWS implements GET /api/v1/switcher_status. A missing or invalid
// access_token is rejected before the WebSocket upgrade, with HTTP 401 and
// body "Token rejected" — matching
// docs/archive-windows-app-spec-v1-rejected.md line 938's description of the
// real socket's upgrade failure, which is also exactly the condition
// internal/m2lx's Watcher must treat as "reconnect immediately, without
// consuming backoff, after a token refresh".
func (a *App) handleStatusWS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("access_token")
	if _, ok := a.checkToken(token); !ok {
		a.logf("statusws", "upgrade refused from %s — invalid or missing access_token", r.RemoteAddr)
		http.Error(w, "Token rejected", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		a.logf("statusws", "upgrade failed from %s: %v", r.RemoteAddr, err)
		return
	}

	client := &wsClient{conn: conn}
	a.wsMu.Lock()
	a.wsClients[client] = struct{}{}
	clientCount := len(a.wsClients)
	a.wsMu.Unlock()

	a.logf("statusws", "client connected from %s (%d now connected)", r.RemoteAddr, clientCount)

	// Frame 0, immediately — that is the protocol, not a convenience. The
	// stall fault is respected exactly as the broadcaster respects it: a
	// client that connects while stalled must see nothing at all, not one free
	// snapshot. It stays unbaselined and the broadcaster will hand it frame 0
	// when the fault clears.
	if !a.getStallStatus() {
		a.sendSnapshotTo(client, a.snapshotFrame())
	}

	// The only reason to read from this socket at all is to notice the client
	// going away; the real socket ignores client data frames entirely (see the
	// file comment), and so does this one.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}

	a.wsMu.Lock()
	delete(a.wsClients, client)
	remaining := len(a.wsClients)
	a.wsMu.Unlock()
	conn.Close()
	a.logf("statusws", "client disconnected from %s (%d remain)", r.RemoteAddr, remaining)
}

// runStatusBroadcaster pushes one tick of the document to every connected
// client every StatusInterval, until ctx is cancelled.
//
// While the stall fault is set it pushes nothing — sockets stay open and
// silent, which is exactly the condition m2lx.StaleAfter exists to catch and
// the one failure that looks like nothing at all.
func (a *App) runStatusBroadcaster(ctx context.Context) {
	t := time.NewTicker(a.opts.StatusInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if a.getStallStatus() {
				if a.opts.Verbose {
					a.logf("statusws", "push skipped — stall fault active")
				}
				continue
			}
			a.pushTick()
		}
	}
}

// pushTick sends one broadcaster tick's worth of frames.
//
// Building nothing when nobody is connected is not just an optimisation: the
// meter sweep and the delta sequence advance as frames are built, and the
// transition detector publishes a reading as it sends it. Advancing all of
// that against an audience of nobody would mean a client connecting later
// found a document whose history it had missed the announcement of.
func (a *App) pushTick() {
	clients := a.statusClients()
	if len(clients) == 0 {
		return
	}

	// Frame 0 for anyone still without one. See the file comment: this is the
	// path a client takes when it connected while the socket was stalled.
	if anyUnbaselined(clients) {
		snap := a.snapshotFrame()
		for _, c := range clients {
			if !c.baselined.Load() {
				a.sendSnapshotTo(c, snap)
			}
		}
	}

	frames := a.nextFrames()
	sent := 0
	for _, f := range frames {
		for _, c := range clients {
			// A client whose frame 0 failed to send is skipped rather than
			// given a delta it cannot merge.
			if c.baselined.Load() {
				a.sendFrameTo(c, f)
				sent++
			}
		}
	}
	a.logDeltas(frames, len(clients), sent)
}

// statusClients snapshots the client set, so the broadcast is not holding
// wsMu while writing to sockets.
func (a *App) statusClients() []*wsClient {
	a.wsMu.Lock()
	defer a.wsMu.Unlock()
	clients := make([]*wsClient, 0, len(a.wsClients))
	for c := range a.wsClients {
		clients = append(clients, c)
	}
	return clients
}

func anyUnbaselined(clients []*wsClient) bool {
	for _, c := range clients {
		if !c.baselined.Load() {
			return true
		}
	}
	return false
}

// sendSnapshotTo writes frame 0 and, if it lands, marks the client baselined
// so it starts receiving deltas.
func (a *App) sendSnapshotTo(c *wsClient, f statusFrame) {
	if !a.sendFrameTo(c, f) {
		return
	}
	c.baselined.Store(true)
	if a.opts.Verbose {
		a.logf("statusws", "pushed the opening snapshot: %d nodes, every entry at path %q", len(f.Status), wholeNodePath)
	}
}

// sendFrameTo writes one frame to one client and reports whether it landed. On
// failure it unregisters and closes the client; the client's own ReadMessage
// loop is unblocked by the Close and exits, so there is no double-logging path
// — that loop logs the disconnect, this just stops a concurrent broadcast
// retrying a dead socket.
func (a *App) sendFrameTo(c *wsClient, f statusFrame) bool {
	c.writeMu.Lock()
	c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := c.conn.WriteJSON(f)
	c.writeMu.Unlock()

	if err != nil {
		a.wsMu.Lock()
		delete(a.wsClients, c)
		a.wsMu.Unlock()
		c.conn.Close()
		return false
	}
	return true
}

// logDeltas writes the verbose delta log, at most once a second.
//
// The real socket runs at about 21 frames a second and this mock's default
// matches it, so a line per frame would bury the connect, fault and transition
// lines that someone watching this log is actually there for. The summary
// names the last frame's node and path, which is the part that varies.
func (a *App) logDeltas(frames []statusFrame, clients, writes int) {
	if !a.opts.Verbose || len(frames) == 0 {
		return
	}
	last := frames[len(frames)-1]
	node, path := "", ""
	if len(last.Status) > 0 {
		node, path = last.Status[0].Node, last.Status[0].Path
	}

	a.doc.mu.Lock()
	seq := a.doc.seq
	due := time.Now().After(a.doc.verboseAt)
	if due {
		a.doc.verboseAt = time.Now().Add(time.Second)
	}
	a.doc.mu.Unlock()

	if !due {
		return
	}
	a.logf("statusws", "%d delta(s) pushed so far to %d client(s) (%d write(s) this tick); last: node=%q path=%q",
		seq, clients, writes, node, path)
}
