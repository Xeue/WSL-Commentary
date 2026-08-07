// The fault-injection control endpoint. This is the reason this package
// exists: CONTRACT.md is explicit that "most of WP-7's value is concentrated
// in fault injection", and a mock that only works when everything works is
// worthless as a test substrate. Every fault below is also settable from a
// command-line flag at startup (main.go); this endpoint is what lets a
// running test drive them mid-session, e.g. from a Go test using net/http
// against a mockm2lx instance it started in-process or as a subprocess.
//
// Every handler is POST (state changes) or GET (state.go's snapshot), takes
// or returns JSON, and never touches go.mod-frozen routing libraries — it is
// a handful of handlers on the same *http.ServeMux as the rest of the mock.
package main

import (
	"encoding/json"
	"net/http"
	"time"

	"wslcomms/internal/m2lx"
)

// controlState is the full fault-injection and connection snapshot returned
// by GET /control/state — everything a test or a human needs to know "what
// is this mock currently doing" without reading the log.
type controlState struct {
	OnePeerOnly    bool        `json:"onePeerOnly"`
	RefusalWindow  string      `json:"refusalWindow"`
	StallStatus    bool        `json:"stallStatus"`
	LieStreamState string      `json:"lieStreamState,omitempty"`
	DropAudio      bool        `json:"dropAudio"`
	DecoyDelta     string      `json:"decoyDelta"`
	TransitionPush string      `json:"transitionPush"`
	SRT            srtSnapshot `json:"srt"`
	WSClients      int         `json:"wsClients"`
	Sessions       int         `json:"sessions"`

	// StatusKey and StatusKeyIsInput say which node carries the SRT truth,
	// and whether it is a router input at all. They are here because
	// "-status-key names nothing" is a supported case rather than a
	// misconfiguration (see main.go), and a test looking at grey lamps needs
	// to be able to tell which of the two situations it is in.
	StatusKey        string `json:"statusKey"`
	StatusKeyIsInput bool   `json:"statusKeyIsInput"`
}

func (a *App) handleControlState(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	st := controlState{
		OnePeerOnly:    a.onePeerOnly,
		RefusalWindow:  a.refusalWindow.String(),
		StallStatus:    a.stallStatus,
		LieStreamState: a.lieStreamState,
		DropAudio:      a.dropAudio,
		DecoyDelta:     a.decoyDelta,
		TransitionPush: a.transitionPush,
		Sessions:       len(a.sessions),
	}
	a.mu.RUnlock()

	st.StatusKey = a.opts.StatusKey
	_, st.StatusKeyIsInput = a.statusKeyInput()

	st.SRT = a.srt.snapshot()

	a.wsMu.Lock()
	st.WSClients = len(a.wsClients)
	a.wsMu.Unlock()

	writeJSON(w, http.StatusOK, st)
}

// handleControlDropSRT implements POST /control/drop-srt: drop the SRT
// session now.
func (a *App) handleControlDropSRT(w http.ResponseWriter, r *http.Request) {
	dropped := a.dropSRTNow()
	writeJSON(w, http.StatusOK, map[string]bool{"dropped": dropped})
}

type onePeerOnlyRequest struct {
	Enabled bool `json:"enabled"`
}

// handleControlOnePeerOnly implements POST /control/one-peer-only: toggle
// the one-peer-only fault. On (the default, and the real listener's actual
// behaviour) refuses any second caller without displacing the incumbent.
func (a *App) handleControlOnePeerOnly(w http.ResponseWriter, r *http.Request) {
	var req onePeerOnlyRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	a.setOnePeerOnly(req.Enabled)
	a.logf("control", "one-peer-only set to %v", req.Enabled)
	writeJSON(w, http.StatusOK, map[string]bool{"onePeerOnly": req.Enabled})
}

type refusalWindowRequest struct {
	Seconds float64 `json:"seconds"`
}

// handleControlRefusalWindow implements POST /control/refusal-window: set
// how long, after a disconnect, the listener refuses to re-accept — the
// measured ~5 s M2L-X behaviour internal/sender's backoff ladder has to
// beat (docs/test-results.md line 149).
func (a *App) handleControlRefusalWindow(w http.ResponseWriter, r *http.Request) {
	var req refusalWindowRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Seconds < 0 {
		http.Error(w, `{"error":"seconds must be >= 0"}`, http.StatusBadRequest)
		return
	}
	d := time.Duration(req.Seconds * float64(time.Second))
	a.setRefusalWindow(d)
	a.logf("control", "refusal window set to %s", d)
	writeJSON(w, http.StatusOK, map[string]string{"refusalWindow": d.String()})
}

type stallStatusRequest struct {
	Enabled bool `json:"enabled"`
}

// handleControlStallStatus implements POST /control/stall-status: stop
// pushing status snapshots without closing any socket, to exercise the 15 s
// staleness path (m2lx.StaleAfter).
func (a *App) handleControlStallStatus(w http.ResponseWriter, r *http.Request) {
	var req stallStatusRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	a.setStallStatus(req.Enabled)
	a.logf("control", "status WS stall set to %v", req.Enabled)
	writeJSON(w, http.StatusOK, map[string]bool{"stallStatus": req.Enabled})
}

type lieRequest struct {
	StreamState string `json:"streamState"`
}

// handleControlLie implements POST /control/lie: override the reported
// stream_state regardless of whether an SRT peer is actually connected.
// streamState must be one of m2lx's three stream-state constants, or "" /
// "auto" to go back to reporting the truth. This is the fault the whole
// spec's distrust of telemetry lamps is built around (spec section 8): the
// mock must be able to lie exactly as the real instance's telemetry can.
func (a *App) handleControlLie(w http.ResponseWriter, r *http.Request) {
	var req lieRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	switch req.StreamState {
	case "", "auto":
		a.setLieStreamState("")
	case m2lx.StreamStateStreaming, m2lx.StreamStateStarting, m2lx.StreamStateStopped:
		a.setLieStreamState(req.StreamState)
	default:
		http.Error(w, `{"error":"streamState must be streaming, starting, stopped, or empty/auto"}`, http.StatusBadRequest)
		return
	}
	a.logf("control", "stream_state lie set to %q (empty = report the truth)", req.StreamState)
	writeJSON(w, http.StatusOK, map[string]string{"lieStreamState": a.getLieStreamState()})
}

type dropAudioRequest struct {
	Enabled bool `json:"enabled"`
}

// handleControlDropAudio implements POST /control/drop-audio: force the
// status document's audio array empty regardless of SRT state, reproducing
// the MP2/AC-3 silent-drop signature (spec section 5 and section 8).
func (a *App) handleControlDropAudio(w http.ResponseWriter, r *http.Request) {
	var req dropAudioRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	a.setDropAudio(req.Enabled)
	a.logf("control", "drop-audio set to %v", req.Enabled)
	writeJSON(w, http.StatusOK, map[string]bool{"dropAudio": req.Enabled})
}

type decoyDeltaRequest struct {
	Mode string `json:"mode"`
}

// handleControlDecoyDelta implements POST /control/decoy-delta: start (or
// stop) sending a subtree delta that a parser ignoring "path" would read as a
// whole node.
//
// This is the fault this endpoint exists for. The device really does send
//
//	{"node":"cam1","path":"/statistics","state":{"bitrate":6523.6,...}}
//
// and a parser that unmarshals that "state" as a node finds no stream_state,
// concludes cam1 is not a router input, and condemns the only input on the
// switcher that is actually working — once a second, forever, with the lamps
// grey and nothing saying why. It was a live defect and it must be
// reproducible somewhere; this is that somewhere. See App.decoyFrame for the
// two modes and what a correct parser does with each.
func (a *App) handleControlDecoyDelta(w http.ResponseWriter, r *http.Request) {
	var req decoyDeltaRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	switch req.Mode {
	case "", decoyDeltaOff:
		a.setDecoyDelta(decoyDeltaOff)
	case decoyDeltaStatistics, decoyDeltaStreamState:
		a.setDecoyDelta(req.Mode)
	default:
		http.Error(w, `{"error":"mode must be off, statistics or stream-state"}`, http.StatusBadRequest)
		return
	}
	a.logf("control", "decoy delta set to %q", a.getDecoyDelta())
	writeJSON(w, http.StatusOK, map[string]string{"decoyDelta": a.getDecoyDelta()})
}

type transitionPushRequest struct {
	Mode string `json:"mode"`
}

// handleControlTransitionPush implements POST /control/transition-push: choose
// how a change in the status-key node's stream_state or formats is published,
// or suppress it entirely.
//
// "none" is the interesting one. Nobody has ever observed a real input change
// state on this socket, so it is genuinely unknown whether a transition is
// pushed at all — and internal/m2lx's resyncInterval is an explicit backstop
// against the answer being "no". Setting this to "none" makes the mock behave
// as though the answer were "no", which is the only way to test that backstop
// without starting and stopping a feed on a live switcher.
func (a *App) handleControlTransitionPush(w http.ResponseWriter, r *http.Request) {
	var req transitionPushRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	switch req.Mode {
	case "", transitionPushNode:
		a.setTransitionPush(transitionPushNode)
	case transitionPushDelta, transitionPushNone:
		a.setTransitionPush(req.Mode)
	default:
		http.Error(w, `{"error":"mode must be node, delta or none"}`, http.StatusBadRequest)
		return
	}
	a.logf("control", "transition push set to %q", a.getTransitionPush())
	writeJSON(w, http.StatusOK, map[string]string{"transitionPush": a.getTransitionPush()})
}

type expireTokenRequest struct {
	// In is a duration string (e.g. "2s"), how far from now every live
	// token should expire. Empty or omitted means immediately.
	In string `json:"in"`
}

// handleControlExpireToken implements POST /control/expire-token: make every
// currently live token expire early, to exercise the refresh path and the
// WebSocket-reopen-on-refresh path (the token rides in the connection URL,
// so a rotated token requires a new socket — spec section 8's last
// sentence).
func (a *App) handleControlExpireToken(w http.ResponseWriter, r *http.Request) {
	var req expireTokenRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	var d time.Duration
	if req.In != "" {
		parsed, err := time.ParseDuration(req.In)
		if err != nil {
			http.Error(w, `{"error":"in must be a duration string, e.g. \"2s\""}`, http.StatusBadRequest)
			return
		}
		d = parsed
	}
	n := a.expireTokens(d)
	a.logf("control", "expired %d live token(s), effective in %s", n, d)
	writeJSON(w, http.StatusOK, map[string]int{"tokensExpired": n})
}

// handleControlReset implements POST /control/reset: return every fault flag
// to its startup value. Does not touch sessions or the SRT connection.
func (a *App) handleControlReset(w http.ResponseWriter, r *http.Request) {
	a.resetFaults()
	a.logf("control", "faults reset to startup defaults")
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}

// decodeJSONBody reads and JSON-decodes r's body into dst. An empty body is
// treated as a zero-valued dst rather than an error, so a control call that
// takes no meaningful fields (or whose field is optional) can be POSTed with
// no body at all. On any other decode failure it writes a 400 and returns
// false; the caller must return immediately when it does.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, err := readLimitedBody(r)
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return false
	}
	if len(body) == 0 {
		return true
	}
	if err := json.Unmarshal(body, dst); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return false
	}
	return true
}
