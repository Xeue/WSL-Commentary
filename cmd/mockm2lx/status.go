// The switcher_status WebSocket: GET /api/v1/switcher_status?access_token=...
//
// Push-only, as the real socket is documented to be (spec section 8: the
// server silently ignores client data frames — see
// docs/archive-windows-app-spec-v1-rejected.md line 938). This mock never
// reads application data from a client, only enough to notice when the
// client closes the connection.
//
// The URL and query parameter name match the one confirmed usage found in
// the bundle for the sibling switcher_controller socket
// (docs/architecture.md line 428, `wss://<host>/api/v1/switcher_controller
// ?access_token=<percent-encoded>`); switcher_status uses the same scheme
// (docs/archive-windows-app-spec-v1-rejected.md line 934).
package main

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"wslcomms/internal/m2lx"
)

// upgrader has CheckOrigin disabled: this mock is a local development and
// test tool, never exposed beyond localhost/a test harness, and the real
// M2L-X instance's CORS policy is not something this package needs to
// reproduce.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// wsClient is one connected status-socket client. There is deliberately no
// per-client fault state: every fault in this mock is process-wide, matching
// how a real test drives the one M2L-X instance the app under test talks to.
//
// writeMu serialises writes to conn. gorilla/websocket allows at most one
// concurrent writer; without this, the immediate push-on-connect
// (handleStatusWS) and the periodic broadcaster (runStatusBroadcaster) could
// both write to a just-registered client at the same time, from two
// different goroutines.
type wsClient struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

// handleStatusWS implements GET /api/v1/switcher_status. A missing or
// invalid access_token is rejected before the WebSocket upgrade, with
// HTTP 401 and body "Token rejected" — matching
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

	// An immediate push on connect, respecting the stall fault exactly as
	// the periodic broadcaster does: a client that connects while stalled
	// must see nothing, not one free snapshot.
	if !a.getStallStatus() {
		a.sendStatusTo(client, a.buildStatusDoc())
	}

	// The only reason to read from this socket at all is to notice the
	// client going away; the real socket ignores client data frames
	// entirely (see file doc comment above), and so does this one.
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

// statusNode is one node of the switcher_status snapshot: stream_state and
// detected formats, per spec section 8. Real snapshots carry many nodes
// (docs/architecture.md line 416 measured 36 on the dev instance); this mock
// only synthesizes the one node under test.
type statusNode struct {
	StreamState string            `json:"stream_state"`
	Streams     statusNodeStreams `json:"streams"`
}

type statusNodeStreams struct {
	Video statusVideo   `json:"video"`
	Audio []statusAudio `json:"audio"`
}

type statusVideo struct {
	Format string `json:"format"`
}

type statusAudio struct {
	Format string `json:"format"`
}

// Measured format strings, spec section 8 / CONTRACT.md internal/m2lx
// section: "the measured wire values of streams.video.format and
// streams.audio[].format are single strings". Reproduced verbatim.
const (
	measuredVideoFormat = "h264 1920x1080 50 P"
	measuredAudioFormat = "aac 48000 2ch"
)

// buildStatusDoc renders the current switcher_status snapshot as
// map[nodeName]statusNode. The configured -status-key node reflects (or, if
// the lie fault is set, misreports) whether an SRT peer is connected; every
// other field it carries reflects ground truth regardless of the lie fault,
// which only overrides stream_state — see Options.LieStreamState's doc
// comment.
func (a *App) buildStatusDoc() map[string]statusNode {
	connected := a.srtPeerConnected()

	streamState := m2lx.StreamStateStopped
	if connected {
		streamState = m2lx.StreamStateStreaming
	}
	if lie := a.getLieStreamState(); lie != "" {
		streamState = lie
	}

	var video statusVideo
	// audio is deliberately a non-nil empty slice, not a nil one: Go's
	// encoding/json renders a nil slice as `null` but an empty one as `[]`,
	// and the MP2/AC-3 silent-drop signature this mock reproduces is
	// specifically an EMPTY ARRAY (spec section 8, AUDIO OK row;
	// internal/m2lx's Status.Audio doc comment), not a null/absent field.
	audio := []statusAudio{}
	if connected {
		video.Format = measuredVideoFormat
		if !a.getDropAudio() {
			audio = []statusAudio{{Format: measuredAudioFormat}}
		}
	}

	return map[string]statusNode{
		a.opts.StatusKey: {
			StreamState: streamState,
			Streams: statusNodeStreams{
				Video: video,
				Audio: audio,
			},
		},
	}
}

// runStatusBroadcaster pushes buildStatusDoc to every connected client every
// StatusInterval, until ctx is cancelled. While the stall fault is set, it
// skips the push entirely — sockets stay open, nothing arrives on them —
// which is exactly the condition that must trip m2lx.StaleAfter.
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
			doc := a.buildStatusDoc()
			n := a.broadcastStatus(doc)
			if a.opts.Verbose {
				a.logf("statusws", "pushed snapshot to %d client(s): %+v", n, doc[a.opts.StatusKey])
			}
		}
	}
}

// broadcastStatus writes doc to every connected client, dropping (and
// unregistering) any client whose write fails. It returns how many clients
// the write was attempted on.
func (a *App) broadcastStatus(doc map[string]statusNode) int {
	a.wsMu.Lock()
	clients := make([]*wsClient, 0, len(a.wsClients))
	for c := range a.wsClients {
		clients = append(clients, c)
	}
	a.wsMu.Unlock()

	for _, c := range clients {
		a.sendStatusTo(c, doc)
	}
	return len(clients)
}

// sendStatusTo writes doc to one client. On failure it unregisters and
// closes the client; the client's own ReadMessage loop will also be
// unblocked by the Close and exit, so there is no double-logging path — that
// loop logs the disconnect, this method just cleans up the map entry so a
// concurrent broadcast doesn't retry a dead socket.
func (a *App) sendStatusTo(c *wsClient, doc map[string]statusNode) {
	c.writeMu.Lock()
	c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := c.conn.WriteJSON(doc)
	c.writeMu.Unlock()

	if err != nil {
		a.wsMu.Lock()
		delete(a.wsClients, c)
		a.wsMu.Unlock()
		c.conn.Close()
	}
}
