//go:build dev || production || bindings

// Tests for the video signal lamp's half of the wire-up (app.go).
//
// The hysteresis itself is tested in internal/gst/signalwatch_test.go as a pure
// state machine with injected readings. What is tested HERE is everything
// between that state machine and the page: that a report reaches the frontend on
// an event of its own, that it is cached for the reload replay, that the replay
// says UNKNOWN rather than an empty string before anything has been measured,
// and that the session's end puts the lamp back to "cannot tell" instead of
// leaving it lit for a pipeline that no longer exists.
//
// That last property is the one worth a test rather than a comment. Every other
// lamp in this application is driven by something that keeps talking — a sender
// that reconnects, a return monitor that retries — so a stale value corrects
// itself within seconds. A locked capture input reports ONCE and is then silent
// for ninety minutes, so anything this file gets wrong stays wrong for the whole
// match.
package main

import (
	"encoding/json"
	"testing"

	"wslcomms/internal/gst"
)

func TestSignalReportsReachTheFrontendOnTheirOwnEvent(t *testing.T) {
	a, _ := newTestApp(t)

	a.signalForwarder()(gst.SignalReport{State: gst.SignalLost, Flaps: 1})

	var saw bool
	for _, e := range drainPump(a) {
		switch e.name {
		case EventSignal:
			saw = true
			p, ok := e.data.(signalPayload)
			if !ok {
				t.Fatalf("the %q event carried a %T, want a signalPayload", EventSignal, e.data)
			}
			if p.State != gst.SignalLost {
				t.Errorf("payload state = %q, want %q", p.State, gst.SignalLost)
			}
			if p.Flaps != 1 {
				t.Errorf("payload flaps = %d, want 1", p.Flaps)
			}
		case EventSender, EventError:
			// The signal lamp is its own lamp. A capture input with nothing on
			// it is NOT a session failure — the commentary keeps flowing and the
			// far end sees black, which internal/gst/capturefault.go argues at
			// length is the correct degradation — so it must not move the
			// SENDING lamp and must not raise an error banner.
			t.Fatalf("a signal report emitted a %q event as well; the lamps are separate", e.name)
		}
	}
	if !saw {
		t.Fatalf("no %q event was emitted for a signal report", EventSignal)
	}
}

func TestSignalForwarderCachesForTheReplay(t *testing.T) {
	a, _ := newTestApp(t)

	a.signalForwarder()(gst.SignalReport{State: gst.SignalOK, Flaps: 7})

	a.sigMu.Lock()
	got := a.lastSignal
	a.sigMu.Unlock()

	if got.State != gst.SignalOK || got.Flaps != 7 {
		t.Fatalf("lastSignal = %+v, want {OK 7}", got)
	}
}

func TestDomReadyReplaysTheSignalState(t *testing.T) {
	// A healthy input speaks once and then never again, so a page reloaded at
	// half-time has heard nothing at all. Without the replay its lamp would be
	// grey — "this application cannot tell you" — over a picture it can see.
	a, _ := newTestApp(t)
	silencePump(a)

	a.signalForwarder()(gst.SignalReport{State: gst.SignalOK})
	drainPump(a)

	a.domReady(a.rootCtx)

	var replayed *signalPayload
	for _, e := range drainPump(a) {
		if e.name == EventSignal {
			p, ok := e.data.(signalPayload)
			if !ok {
				t.Fatalf("the replayed %q event carried a %T, want a signalPayload", EventSignal, e.data)
			}
			replayed = &p
		}
	}
	if replayed == nil {
		t.Fatal("domReady replayed no signal state; a reloaded page would show a grey lamp over a good input")
	}
	if replayed.State != gst.SignalOK {
		t.Errorf("replayed state = %q, want %q", replayed.State, gst.SignalOK)
	}
}

func TestDomReadyBeforeAnythingHasRunReplaysUnknown(t *testing.T) {
	// UNKNOWN, and never the empty string: a lamp has no case for "" and would
	// draw whatever its default branch is, which is exactly the kind of accident
	// that puts a green indicator on a machine with no card in it.
	a, _ := newTestApp(t)
	silencePump(a)

	a.domReady(a.rootCtx)

	for _, e := range drainPump(a) {
		if e.name != EventSignal {
			continue
		}
		p := e.data.(signalPayload)
		if p.State != gst.SignalUnknown {
			t.Fatalf("the first %q event said %q, want %q", EventSignal, p.State, gst.SignalUnknown)
		}
		return
	}
	t.Fatalf("domReady emitted no %q event at all", EventSignal)
}

func TestForgetSignalPublishesUnknownAndClearsTheCache(t *testing.T) {
	// The session-end frame, and both halves of it. Without the EVENT the lamp
	// keeps its last value for a pipeline that has been torn down; without the
	// CACHE CLEAR a page loaded a minute later is told that value again, which
	// is worse, because a replay looks freshly measured.
	a, _ := newTestApp(t)
	a.signalForwarder()(gst.SignalReport{State: gst.SignalOK, Flaps: 3})
	drainPump(a)

	a.forgetSignal()

	var saw bool
	for _, e := range drainPump(a) {
		if e.name == EventSignal {
			saw = true
			if p := e.data.(signalPayload); p.State != gst.SignalUnknown {
				t.Errorf("the session-end frame said %q, want %q", p.State, gst.SignalUnknown)
			}
		}
	}
	if !saw {
		t.Fatalf("no %q event was emitted when the session ended", EventSignal)
	}

	a.sigMu.Lock()
	got := a.lastSignal
	a.sigMu.Unlock()
	if got.State != gst.SignalUnknown {
		t.Fatalf("lastSignal after the session ended = %q, want %q; a later reload would "+
			"resurrect the stale lamp", got.State, gst.SignalUnknown)
	}
}

func TestSignalPayloadFromNamesTheEmptyState(t *testing.T) {
	// A zero gst.SignalReport must never cross the boundary as "". Every path
	// out of app.go goes through this function so the three words are spelled in
	// one place.
	if p := signalPayloadFrom(gst.SignalReport{}); p.State != gst.SignalUnknown {
		t.Errorf("signalPayloadFrom(zero).State = %q, want %q", p.State, gst.SignalUnknown)
	}
	if p := signalPayloadFrom(gst.SignalReport{State: gst.SignalLost, Flaps: 2}); p.State != gst.SignalLost || p.Flaps != 2 {
		t.Errorf("signalPayloadFrom passed through as %+v, want {LOST 2}", p)
	}
}

func TestSignalPayloadWireShape(t *testing.T) {
	// The JSON keys are the contract with frontend/src/ui/backend.js. They are
	// lower-case and they are pinned here for the same reason levelsPayload's
	// are: renaming a Go field is a refactor, and renaming a wire key is a
	// silently broken lamp on a build nobody notices until a match.
	b, err := json.Marshal(signalPayload{State: gst.SignalOK, Flaps: 4})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if got, want := string(b), `{"state":"OK","flaps":4}`; got != want {
		t.Fatalf("signalPayload marshalled as %s, want %s", got, want)
	}
}

func TestSignalStatesSpellingIsStable(t *testing.T) {
	// The three states cross to a frontend that switches on them. They are
	// asserted as literals here, in the application, rather than only in
	// internal/gst, so that a rename has to be made in two places on purpose.
	for _, tc := range []struct {
		state gst.SignalState
		want  string
	}{
		{gst.SignalUnknown, "UNKNOWN"},
		{gst.SignalOK, "OK"},
		{gst.SignalLost, "LOST"},
	} {
		if string(tc.state) != tc.want {
			t.Errorf("signal state spelled %q, want %q", tc.state, tc.want)
		}
	}
}
