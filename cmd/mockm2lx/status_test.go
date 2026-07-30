package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"wslcomms/internal/m2lx"
)

// newStatusTestServer wires only the status WebSocket handler behind an
// httptest.Server, and returns the App plus a ws:// URL to dial.
func newStatusTestServer(t *testing.T, a *App) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/switcher_status", a.handleStatusWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/switcher_status"
}

// statusWSURL builds the status socket URL for a bearer token, matching
// docs/archive-windows-app-spec-v1-rejected.md line 934's
// Uri.EscapeDataString — the base64 access token routinely contains '+',
// '/' and '=', all of which are meaningful in a query string, so it must be
// percent-encoded, not concatenated raw.
func statusWSURL(base, token string) string {
	v := url.Values{}
	v.Set("access_token", token)
	return base + "?" + v.Encode()
}

func TestStatusWS_RejectsMissingToken(t *testing.T) {
	a := newAuthTestApp(t)
	url := newStatusTestServer(t, a)

	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatalf("expected the upgrade to be refused with no access_token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		code := -1
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("status = %d, want 401", code)
	}
}

func TestStatusWS_RejectsInvalidToken(t *testing.T) {
	a := newAuthTestApp(t)
	url := newStatusTestServer(t, a)

	_, resp, err := websocket.DefaultDialer.Dial(url+"?access_token=not-a-real-token", nil)
	if err == nil {
		t.Fatalf("expected the upgrade to be refused with an invalid access_token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 on invalid token")
	}
}

func TestStatusWS_AcceptsValidTokenAndPushesInitialSnapshot(t *testing.T) {
	a := newAuthTestApp(t)
	tok := signIn(t, a)
	url := newStatusTestServer(t, a)

	conn, _, err := websocket.DefaultDialer.Dial(statusWSURL(url, tok.AccessToken), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var doc map[string]statusNode
	if err := conn.ReadJSON(&doc); err != nil {
		t.Fatalf("expected an immediate snapshot on connect: %v", err)
	}

	node, ok := doc[a.opts.StatusKey]
	if !ok {
		t.Fatalf("snapshot missing node %q: %+v", a.opts.StatusKey, doc)
	}
	if node.StreamState != m2lx.StreamStateStopped {
		t.Errorf("stream_state = %q, want %q (no SRT peer connected)", node.StreamState, m2lx.StreamStateStopped)
	}
	if node.Streams.Audio == nil || len(node.Streams.Audio) != 0 {
		t.Errorf("audio = %+v, want an empty (non-null) array", node.Streams.Audio)
	}
}

func TestStatusWS_StallSkipsPeriodicPush(t *testing.T) {
	a := newAuthTestApp(t)
	a.opts.StatusInterval = 20 * time.Millisecond
	tok := signIn(t, a)
	a.setStallStatus(true)
	url := newStatusTestServer(t, a)

	conn, _, err := websocket.DefaultDialer.Dial(statusWSURL(url, tok.AccessToken), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	startBroadcaster(t, a)

	// No initial push (stall was already on at connect time) and no
	// periodic push either: a ReadJSON must time out.
	conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	var doc map[string]statusNode
	if err := conn.ReadJSON(&doc); err == nil {
		t.Fatalf("expected no message while stalled, got: %+v", doc)
	}
}

func TestStatusWS_PeriodicPushDeliversWhenNotStalled(t *testing.T) {
	a := newAuthTestApp(t)
	a.opts.StatusInterval = 20 * time.Millisecond
	tok := signIn(t, a)
	url := newStatusTestServer(t, a)

	conn, _, err := websocket.DefaultDialer.Dial(statusWSURL(url, tok.AccessToken), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	startBroadcaster(t, a)

	// Drain the immediate on-connect push, then expect at least one more
	// from the periodic broadcaster.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var doc map[string]statusNode
	if err := conn.ReadJSON(&doc); err != nil {
		t.Fatalf("expected the immediate push: %v", err)
	}
	if err := conn.ReadJSON(&doc); err != nil {
		t.Fatalf("expected a periodic push: %v", err)
	}
}

func TestStatusWS_LieOverridesStreamState(t *testing.T) {
	a := newAuthTestApp(t)
	tok := signIn(t, a)
	a.setLieStreamState(m2lx.StreamStateStreaming) // lie: claim streaming with no SRT peer
	url := newStatusTestServer(t, a)

	conn, _, err := websocket.DefaultDialer.Dial(statusWSURL(url, tok.AccessToken), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var doc map[string]statusNode
	if err := conn.ReadJSON(&doc); err != nil {
		t.Fatalf("read: %v", err)
	}

	node := doc[a.opts.StatusKey]
	if node.StreamState != m2lx.StreamStateStreaming {
		t.Errorf("stream_state = %q, want the lied value %q", node.StreamState, m2lx.StreamStateStreaming)
	}
	// The lie only covers stream_state — the rest of the document must
	// still tell the truth (no video/audio, since nothing is connected),
	// which is exactly what makes the lamp untrustworthy on its own.
	if node.Streams.Video.Format != "" {
		t.Errorf("video.format = %q, want empty — the lie must not extend to detected formats", node.Streams.Video.Format)
	}
}

func TestBuildStatusDoc_DropAudioForcesEmptyArray(t *testing.T) {
	a := newAuthTestApp(t)
	a.setDropAudio(true)

	doc := a.buildStatusDoc()
	node := doc[a.opts.StatusKey]
	if node.Streams.Audio == nil || len(node.Streams.Audio) != 0 {
		t.Errorf("audio = %+v, want an empty array", node.Streams.Audio)
	}

	// And it must round-trip through JSON as [], not null — WP-2 code that
	// checks len(Audio)==0 must see the same shape whether the array is
	// absent or force-emptied.
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"audio":[]`) {
		t.Errorf("expected literal \"audio\":[] in the JSON, got: %s", b)
	}
}

// startBroadcaster runs a's status broadcaster in the background until the
// test ends, mirroring startListener's (srt_test.go) cancel-then-wait
// cleanup discipline so no log line can fire after the test completes.
func startBroadcaster(t *testing.T, a *App) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.runStatusBroadcaster(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}
