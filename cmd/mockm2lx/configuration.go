package main

import (
	"encoding/json"
	"net/http"
)

// GET /api/v1/switcher_configuration — the instance's CONFIGURED video format.
//
// Owner: WP-7.
//
// # Why the mock has to serve this at all
//
// It is the FIRST rung of the video leg's conform ladder
// (switcher_configuration, then videoFormatOverride, then 1920x1080p50), and
// without it here the mock answers 404, the app falls to the bottom rung, and
// every development run and every harness that points at this mock exercises
// the fallback while looking exactly like a success. The rung that decides what
// the contribution feed's raster IS would then be the one rung nothing but a
// live instance ever runs.
//
// That is not hypothetical: the derivation this endpoint replaced WAS exercised
// against this mock, because it read the same switcher_status document the mock
// already serves. Moving to a REST call moved the primary path out of the mock's
// coverage, and this file moves it back.
//
// # The shape, measured rather than invented
//
// Recorded from matchH on 2026-08-15 — 12108 bytes, HTTP 200 in 34 ms, with
// top-level keys [format nodes system_info]. Only the format block is served
// here, because it is the only part internal/m2lx models; nodes[] is 41 entries
// of configuration topology with no format in it, and system_info is a build
// stamp. A verbatim capture of the whole document is in
// internal/m2lx/testdata/switcher_configuration-live-2026-08-15.json for the
// unit tests; this is the mock's minimum, not a second copy of it.
//
// NOTE THE TRAP, which is the same one the node formats have and the same one
// internal/m2lx/format.go documents: frame_rate is a STRING while width and
// height beside it are NUMBERS. Serving it as a number here would make the mock
// pass a parser that the real switcher fails, which is the one thing a mock
// must never do.
//
// signal_type "rec709" is GStreamer's "bt709", which is what the video leg
// pins. It is served so the agreement is exercised rather than assumed.
type switcherConfigurationDoc struct {
	Format struct {
		Video switcherVideoFormatDoc `json:"video"`
	} `json:"format"`
}

// switcherVideoFormatDoc mirrors matchH's format.video byte for byte, including
// the string frame_rate.
type switcherVideoFormatDoc struct {
	BitDepth   int    `json:"bit_depth"`
	ColorSpace string `json:"color_space"`
	FrameRate  string `json:"frame_rate"`
	Height     int    `json:"height"`
	SignalType string `json:"signal_type"`
	Width      int    `json:"width"`
}

// defaultSwitcherConfiguration is matchH's setting: 1920x1080p50 rec709.
//
// It matches the default the app falls back to when nothing answers, ON PURPOSE
// — a mock whose configured format differed from the fallback would make every
// test that never looks at the format pass for the wrong reason, and the ONE
// test that does look would be the only thing distinguishing "read the setting"
// from "gave up and used the default". Tests that need to tell those apart set
// a different format through /control/switcher-format below.
func defaultSwitcherConfiguration() switcherVideoFormatDoc {
	return switcherVideoFormatDoc{
		BitDepth:   8,
		ColorSpace: "YCbCr",
		FrameRate:  "50",
		Height:     1080,
		SignalType: "rec709",
		Width:      1920,
	}
}

// handleSwitcherConfiguration serves GET /api/v1/switcher_configuration.
//
// Bearer-authenticated like the KVS calls — it is wired through requireAuth by
// the caller in main.go, so an unauthenticated request gets the same 401 the
// real instance gives and the app's token handling is exercised on this path
// too.
func (a *App) handleSwitcherConfiguration(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	format := a.switcherFormat
	a.mu.Unlock()

	var doc switcherConfigurationDoc
	doc.Format.Video = format

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(doc); err != nil {
		a.log.Printf("mockm2lx: writing switcher_configuration: %v", err)
	}
}

// handleControlSwitcherFormat is POST /control/switcher-format, the fault- and
// scenario-injection hook that lets a test drive the conform ladder.
//
// The body is the format block itself, so a test states the shape it wants in
// the switcher's own vocabulary rather than in a translation of it:
//
//	{"width":1280,"height":720,"frame_rate":"50","signal_type":"rec709"}
//
// An EMPTY body restores the default. A body with a frame_rate of "0" is
// explicitly allowed and is worth knowing about: that is what matchH really
// reported for a streaming node's detected format, and being able to reproduce
// it here is how the ladder's "the switcher's configuration carries no video
// format this application can read" branch gets a test that does not need a
// live instance in a particular state.
func (a *App) handleControlSwitcherFormat(w http.ResponseWriter, r *http.Request) {
	var got switcherVideoFormatDoc
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		// An empty body is the documented reset, not a malformed request.
		got = defaultSwitcherConfiguration()
	}
	if got.Width == 0 && got.Height == 0 && got.FrameRate == "" {
		got = defaultSwitcherConfiguration()
	}

	a.mu.Lock()
	a.switcherFormat = got
	a.mu.Unlock()

	a.log.Printf("mockm2lx: switcher format is now %dx%d frame_rate=%q signal_type=%q",
		got.Width, got.Height, got.FrameRate, got.SignalType)
	w.WriteHeader(http.StatusNoContent)
}
