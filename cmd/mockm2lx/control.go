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
	SRT            srtSnapshot `json:"srt"`
	WSClients      int         `json:"wsClients"`
	Sessions       int         `json:"sessions"`
}

func (a *App) handleControlState(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	st := controlState{
		OnePeerOnly:    a.onePeerOnly,
		RefusalWindow:  a.refusalWindow.String(),
		StallStatus:    a.stallStatus,
		LieStreamState: a.lieStreamState,
		DropAudio:      a.dropAudio,
		Sessions:       len(a.sessions),
	}
	a.mu.RUnlock()

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
