package main

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wslcomms/internal/m2lx"
)

// ===========================================================================
// The real internal/m2lx Watcher, against this mock, over a real socket.
//
// THIS IS THE TEST THAT WOULD HAVE CAUGHT THE DRIFT. Every other test in this
// package exercises the mock with the mock's own understanding of the wire; if
// that understanding is wrong they all still pass. The previous version of
// this mock served a document shape M2L-X has never sent — a flat object keyed
// by node name, with the formats as single strings — and internal/m2lx's
// parser rejected every frame of it, silently, for as long as the mock
// existed. Its own tests were green throughout.
//
// So this file drives the REAL client (sign-in, bearer token) and the REAL
// Watcher (WebSocket, opening snapshot, subtree-delta merge, debounce, emit
// gate) against the mock, and asserts on the m2lx.Status that comes out — the
// exact value the three WebSocket-derived lamps are drawn from. Nothing here
// decodes a frame itself.
//
// The streaming cases connect a REAL SRT caller to the mock's real listener
// rather than reaching into the mock's state, because that is the path an
// operator takes at Gate A: wslcomms contributes over SRT, the mock's status
// document reports it, and the lamps go green with no cloud instance anywhere.
// ===========================================================================

// liveMock is a running mock: HTTP/WS on httpAddr and SRT on srtAddr.
type liveMock struct {
	app      *App
	httpAddr string // "http://127.0.0.1:port" — the explicit-scheme form m2lx needs for ws
	srtAddr  string
}

// newLiveMock starts the endpoints the control plane talks to — sign-in, token
// refresh, the status socket — plus the real SRT listener.
func newLiveMock(t *testing.T, opts func(*Options)) *liveMock {
	t.Helper()

	srtAddr := freeSRTAddr(t)
	o := Options{
		Alias:          "wsl-comms-ro",
		Password:       "s3cret",
		StatusKey:      "cam7",
		SRTAddr:        srtAddr,
		StatusInterval: 10 * time.Millisecond,
		TokenTTL:       time.Hour,
		OnePeerOnly:    true,
		RefusalWindow:  150 * time.Millisecond,
	}
	if opts != nil {
		opts(&o)
	}
	a := NewApp(o, log.New(newTestWriter(t), "", 0))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/local_auth/signin", a.handleSignIn)
	mux.HandleFunc("POST /api/local_auth/refresh_token", a.handleRefreshToken)
	mux.HandleFunc("GET /api/v1/switcher_status", a.handleStatusWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	startListener(t, a, srtAddr)
	startBroadcaster(t, a)

	// httptest.Server's URL is already "http://127.0.0.1:port", which is the
	// explicit-scheme form internal/m2lx requires before it will speak ws
	// rather than wss. It logs a loud warning about credentials travelling in
	// clear when it sees one; that warning is correct, and is why the form has
	// to be explicit rather than inferred.
	return &liveMock{app: a, httpAddr: srv.URL, srtAddr: srtAddr}
}

// contribute connects a real SRT caller, which is what makes the mock's status
// document report the input as streaming with the measured h264/aac formats.
func (m *liveMock) contribute(t *testing.T) {
	t.Helper()
	conn, err := dialSRT(t, m.srtAddr)
	if err != nil {
		t.Fatalf("dialling the mock's SRT listener: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	deadlineWaitFor(t, m.app.srtPeerConnected, "the mock to register the SRT peer")
}

// watch signs in and starts the real Watcher on the mock's status key.
func (m *liveMock) watch(t *testing.T) <-chan m2lx.Status {
	t.Helper()

	client := m2lx.NewClient(m.httpAddr)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if err := client.SignIn(ctx, m.app.opts.Alias, m.app.opts.Password); err != nil {
		t.Fatalf("SignIn against the mock: %v", err)
	}
	if client.Token() == "" {
		t.Fatal("SignIn succeeded but left no bearer token")
	}
	return m2lx.NewWatcher(m.httpAddr, client).Watch(ctx, m.app.opts.StatusKey)
}

// nextStatus waits for the next Status the Watcher emits.
//
// The wait is generous because the Watcher's own tick is one second and this
// test has no control over its clock. Every assertion below is on WHAT
// arrives, never on how quickly.
func nextStatus(t *testing.T, ch <-chan m2lx.Status, what string) m2lx.Status {
	t.Helper()
	select {
	case s, ok := <-ch:
		if !ok {
			t.Fatalf("the Watcher's channel closed while waiting for %s", what)
		}
		return s
	case <-time.After(20 * time.Second):
		t.Fatalf("no Status arrived within 20s while waiting for %s", what)
		return m2lx.Status{}
	}
}

// awaitStatus waits for a Status satisfying cond, ignoring any before it.
func awaitStatus(t *testing.T, ch <-chan m2lx.Status, what string, cond func(m2lx.Status) bool) m2lx.Status {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case s, ok := <-ch:
			if !ok {
				t.Fatalf("the Watcher's channel closed while waiting for %s", what)
			}
			if cond(s) {
				return s
			}
		case <-deadline:
			t.Fatalf("no Status matching %s arrived within 20s", what)
			return m2lx.Status{}
		}
	}
}

// ---------------------------------------------------------------------------

// TestWatcherReadsAStoppedInputFromTheMock is the minimum claim, and the one
// that was false before this change: the real parser understands the mock's
// opening snapshot and produces a Status with the fields the lamps read. The
// old mock's frames produced no Status at all.
func TestWatcherReadsAStoppedInputFromTheMock(t *testing.T) {
	m := newLiveMock(t, nil)
	s := nextStatus(t, m.watch(t), "the first Status off the opening snapshot")
	t.Logf("Status: %+v", s)

	if s.Stale {
		t.Fatalf("the first Status is Stale (KeyError=%q) — the real parser could not read this "+
			"mock's frames, which is the exact defect this change closes", s.KeyError)
	}
	if s.KeyError != "" {
		t.Fatalf("KeyError = %q, want empty for a status key the mock serves", s.KeyError)
	}
	if s.StreamState != m2lx.StreamStateStopped {
		t.Errorf("StreamState = %q, want %q with no SRT peer connected", s.StreamState, m2lx.StreamStateStopped)
	}
	// A stopped input reports format null, which format.go renders as an empty
	// Raw — what the lamps show as NO VIDEO, rather than as a format they could
	// not parse.
	if s.Video.Raw != "" {
		t.Errorf("Video.Raw = %q, want empty for a stopped input", s.Video.Raw)
	}
	// ONE audio element with a null format. Not an empty slice: that is the
	// MP2/AC-3 silent-drop signature and means something else entirely.
	if len(s.Audio) != 1 {
		t.Fatalf("len(Audio) = %d, want 1 — a stopped input sends one element with a null format, "+
			"and an empty slice is the silent-drop signature", len(s.Audio))
	}
	if s.Audio[0].Raw != "" {
		t.Errorf("Audio[0].Raw = %q, want empty for a null format", s.Audio[0].Raw)
	}
	if s.At.IsZero() {
		t.Error("At is zero; the Watcher stamps every Status with its own receive time")
	}
}

// TestWatcherReadsTheStreamingFormatsFromTheMock is Gate A's green-lamp case:
// a real SRT caller connects to the mock and the real parser reads the values
// the three lamps go green on out of the mock's format OBJECTS — including
// frame_rate, which arrives as the string "50" beside width and height as
// numbers.
func TestWatcherReadsTheStreamingFormatsFromTheMock(t *testing.T) {
	m := newLiveMock(t, nil)
	// Contributing before the Watcher connects puts the reading in the opening
	// snapshot, where the 4 s debounce adopts it immediately: the first
	// observed value has nothing to debounce against.
	m.contribute(t)

	s := awaitStatus(t, m.watch(t), "a streaming Status", func(s m2lx.Status) bool {
		return !s.Stale && s.StreamState == m2lx.StreamStateStreaming
	})
	t.Logf("Status: %+v", s)

	if s.Video.Codec != "h264" || s.Video.Width != 1920 || s.Video.Height != 1080 || s.Video.FrameRate != 50 {
		t.Errorf("Video = %+v, want h264 1920x1080 @ 50", s.Video)
	}
	// frame_rate is a STRING on the wire and a float64 here. If FrameRate ever
	// reads 0 with a non-empty Raw, the mock has started sending it as a number
	// and format.go's parseFrameRate is no longer being exercised at all.
	if !strings.Contains(s.Video.Raw, `frame_rate="50"`) {
		t.Errorf("Video.Raw = %q, want it to carry frame_rate as the measured STRING", s.Video.Raw)
	}
	if len(s.Audio) != 1 {
		t.Fatalf("len(Audio) = %d, want 1", len(s.Audio))
	}
	if s.Audio[0].Codec != "aac" || s.Audio[0].SampleRate != 48000 || s.Audio[0].Channels != 2 {
		t.Errorf("Audio[0] = %+v, want aac 48000 2ch", s.Audio[0])
	}
}

// TestWatcherFollowsASubtreeDeltaFromTheMock is the merge, end to end.
//
// The change is published ONLY as a "/streams" subtree delta
// (-transition-push=delta), so the Watcher cannot see it by reading any one
// frame: it has to merge the delta into the opening snapshot it is holding.
// That is internal/m2lx/document.go, and it is the code that took two attempts
// — first misreading deltas as whole nodes and condemning a working input once
// a second, then skipping them and freezing the lamps for a whole session.
//
// The change chosen is the audio array emptying rather than a stream_state
// change, because stream_state carries a 4 s debounce and the formats do not:
// the lamp value moves as soon as the delta merges, so a pass here is about
// the merge and not about a timer.
func TestWatcherFollowsASubtreeDeltaFromTheMock(t *testing.T) {
	m := newLiveMock(t, func(o *Options) { o.TransitionPush = transitionPushDelta })
	m.contribute(t)
	ch := m.watch(t)

	before := awaitStatus(t, ch, "the pre-change reading", func(s m2lx.Status) bool {
		return !s.Stale && len(s.Audio) == 1 && s.Audio[0].Codec == "aac"
	})
	t.Logf("before: %+v", before)

	// The MP2/AC-3 silent-drop signature: video stays up, the audio array goes
	// empty, and nothing anywhere says so.
	m.app.setDropAudio(true)

	after := awaitStatus(t, ch, "the merged delta", func(s m2lx.Status) bool {
		return !s.Stale && len(s.Audio) == 0
	})
	t.Logf("after: %+v", after)

	if after.StreamState != m2lx.StreamStateStreaming {
		t.Errorf("StreamState = %q after the merge, want %q — the delta touched streams only",
			after.StreamState, m2lx.StreamStateStreaming)
	}
	if after.Video.Codec != "h264" {
		t.Errorf("Video = %+v after the merge, want the snapshot's video carried across untouched "+
			"— a merge that re-encoded the whole node would have lost it", after.Video)
	}
}

// TestWatcherIgnoresTheDecoyDelta is the regression, reproducible on demand.
//
// With -decoy-delta=stream-state the mock sends, at the full frame rate, a
// "/statistics" delta whose state LOOKS like a complete node reporting
// stopped, with null formats. A parser that ignores "path" believes it and
// drives the lamps from it: the operator is told they are off air, once a
// frame, while they are talking.
//
// The real parser must merge it at /statistics — where nothing any lamp reads
// lives — and hold the lamps exactly where they were. So the assertion is that
// NOTHING arrives: no Status saying stopped, and no KeyError either, since the
// other face of this bug was reporting the working input as "not a router
// input at all".
func TestWatcherIgnoresTheDecoyDelta(t *testing.T) {
	m := newLiveMock(t, func(o *Options) { o.DecoyDelta = decoyDeltaStreamState })
	m.contribute(t)
	ch := m.watch(t)

	first := awaitStatus(t, ch, "the opening reading", func(s m2lx.Status) bool {
		return !s.Stale && s.StreamState == m2lx.StreamStateStreaming
	})
	t.Logf("opening: %+v", first)

	// Two seconds is around 200 decoy frames at this mock's test interval. The
	// historical bug fired on the first one.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case s, ok := <-ch:
			if !ok {
				t.Fatal("the Watcher's channel closed")
			}
			if s.Stale || s.KeyError != "" {
				t.Fatalf("a decoy delta produced %+v — the lamps went grey on a streaming input, "+
					"which is the bug this fault reproduces", s)
			}
			if s.StreamState != m2lx.StreamStateStreaming {
				t.Fatalf("a decoy delta moved StreamState to %q on a streaming input: %+v",
					s.StreamState, s)
			}
		case <-deadline:
			t.Log("~200 decoy deltas merged at /statistics and moved no lamp, as they must")
			return
		}
	}
}

// TestWatcherReportsAStatusKeyTheMockDoesNotServe closes the other half of the
// Gate A loop: a wrong status key must be diagnosable offline.
//
// The mock serves the measured 36-node inventory and does not invent a node to
// match whatever -status-key it was given, so internal/m2lx's
// StatusKeyNotFoundError — which names what was looked for and lists every
// node it could have been — is reachable with no M2L-X instance at all. Before
// this change a wrong statusKey and a right one were equally silent.
func TestWatcherReportsAStatusKeyTheMockDoesNotServe(t *testing.T) {
	m := newLiveMock(t, func(o *Options) { o.StatusKey = "cam99" })
	s := awaitStatus(t, m.watch(t), "the status-key report", func(s m2lx.Status) bool {
		return s.KeyError != ""
	})
	t.Logf("KeyError: %s", s.KeyError)

	if !s.Stale {
		t.Error("a Status carrying a KeyError must be Stale: the lamps have nothing honest to show")
	}
	if !strings.Contains(s.KeyError, `"cam99"`) {
		t.Errorf("the report does not name the key that was looked for: %s", s.KeyError)
	}
	// It must offer the alternatives, or an operator has nothing to act on —
	// including the ones whose node name contains a space.
	if !strings.Contains(s.KeyError, "cam7") || !strings.Contains(s.KeyError, "MIC 1") {
		t.Errorf("the report does not list the router inputs it could have been: %s", s.KeyError)
	}
}
