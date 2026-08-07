// The switcher_status document this mock serves.
//
// ============================ WHY THIS FILE EXISTS ==========================
//
// It replaces a fiction. Until this change the mock pushed
//
//	{"cam7": {"stream_state":"stopped",
//	          "streams":{"video":{"format":"h264 1920x1080 50 P"},"audio":[]}}}
//
// — a flat object keyed by node name, with the formats as SINGLE STRINGS, one
// snapshot every -status-interval and no deltas at all. Every part of that is
// wrong. It was written from docs/windows-app-spec.md §8 and CONTRACT.md before
// anyone had seen a frame; the frame was captured later
// (internal/m2lx/testdata/switcher_status-live-2026-07-31.json), internal/m2lx
// was rewritten against it, and this mock was not. Its own tests kept passing
// because they decoded with the same wrong types they encoded with, which is
// how a mock drifts without anybody noticing, and the real parser rejected
// every frame it sent. Gate A — working with no M2L-X instance — was broken,
// which is exactly the condition a mock exists for.
//
// The capture is the authority for everything below and nothing below is
// inferred from prose. Where a value here is synthesised rather than measured
// it says so.
//
// ================================ THE SHAPE =================================
//
//		{"status":[{"node":"cam22","path":"/","state":{...}}, ...],
//		 "timestamp":1785522083212}
//
//	  - "status" is an ARRAY. The node NAME is an entry's "node" field, not a
//	    key; everything about that node is under "state".
//	  - "timestamp" is epoch MILLISECONDS. internal/mixer reads it (state.go,
//	    frameTime) and internal/m2lx deliberately does not.
//	  - "path" is load-bearing. See "SNAPSHOT THEN DELTA" below.
//
// A router input's state, MEASURED on cam22 while this application was
// streaming into it:
//
//	{"display_name":"CLAUDE-COMMS",
//	 "settings":{"background_color":"#000000ff"},
//	 "statistics":{"bitrate":507.4496, ... "packet_rate":337.4},
//	 "stream_state":"streaming",
//	 "streams":{"audio":[{"error":{...},"format":{"bit_depth":0,
//	                                              "channel_count":2,
//	                                              "codec":"aac",
//	                                              "sample_rate":48000}}],
//	            "video":{"error":{...},"format":{"bit_depth":8,"codec":"h264",
//	                                             "color_space":"YCbCr",
//	                                             "frame_rate":"50","height":1080,
//	                                             "sample_format":"420",
//	                                             "scan_type":"P","width":1920}}}}
//
// The traps in that, all reproduced verbatim by the types in this file:
//
//   - format is a structured OBJECT, never the string the old docs claimed.
//   - frame_rate is a STRING while width and height beside it are NUMBERS.
//     videoFormat.FrameRate is therefore a string field; do not "fix" it.
//   - on a node that is not streaming, format is JSON null — not absent, not
//     an empty object — for audio as well as video.
//   - a STOPPED input sends audio:[{...,"format":null}], ONE element. The
//     EMPTY array is a different thing entirely: it is the MP2/AC-3
//     silent-drop signature, where M2L-X keeps the video online and discards
//     the audio without saying so. This mock can produce both and they must
//     stay distinguishable — see the dropAudio fault.
//
// ============================ SNAPSHOT THEN DELTA ===========================
//
// MEASURED over 150 s and 3180 frames (internal/m2lx/wire.go):
//
//   - frame 0 of every connection is the WHOLE document: all 36 nodes, every
//     entry at path "/", every state complete, about 84 KB.
//   - every frame after it is a SUBTREE delta: normally ONE entry, at about
//     21 frames a second, whose "path" names a subtree of one node and whose
//     "state" is the value AT THAT PATH.
//
// The observed path mix was 1501 "/levels", 1500 "/peak_levels", 163
// "/peak_hold_levels" (all on advanced_audio_mixer) and 15 "/statistics" on
// cam1, the one node that was streaming. deltaKindFor reproduces that mix.
//
// A mock that only sent snapshots could not exercise the merge in
// internal/m2lx/document.go — the logic that took two attempts to get right,
// having first condemned a working input once a second and then frozen the
// lamps for the rest of the session. So this one sends deltas, and can be told
// to send the specific delta that caused the first of those two bugs; see
// decoyDelta.
//
// ======================== WHAT IS DELIBERATELY UNKNOWN ======================
//
// Nobody has ever seen an input change state on this socket. The 150 s
// measurement caught no transition because none happened, and causing one
// means starting or stopping a feed on a live switcher. So it is genuinely not
// known whether a stream_state change is pushed at all, at "/" or at any
// subtree path — and internal/m2lx's resyncInterval is an explicit backstop
// against that one unproven assumption.
//
// This mock does not pretend to know either. transitionPush makes the
// assumption a FAULT-INJECTION AXIS: "node" pushes the changed node whole,
// "delta" pushes it as subtree deltas, and "none" pushes nothing at all and
// leaves only the reconnect to reveal it. "none" is how you find out whether
// that backstop still earns its place.
package main

import (
	"encoding/json"
	"math"
	"sort"
	"sync"
	"time"

	"wslcomms/internal/m2lx"
)

// wholeNodePath is the "path" of an entry carrying a node's ENTIRE state.
// Every entry of the opening snapshot has it and no delta ever does — that
// single test is what keeps internal/m2lx from reading a "/statistics" delta
// as a node with no stream_state.
const wholeNodePath = "/"

// The subtree paths this mock emits. The first four are MEASURED; the last two
// are what transitionPush "delta" invents, and are marked as such because
// nobody has seen the device push a state change at any path.
const (
	pathStatistics      = "/statistics"       // measured, on the streaming input
	pathLevels          = "/levels"           // measured, on advanced_audio_mixer
	pathPeakLevels      = "/peak_levels"      // measured, on advanced_audio_mixer
	pathPeakHoldLevels  = "/peak_hold_levels" // measured, on advanced_audio_mixer
	pathStreams         = "/streams"          // SYNTHETIC: see transitionPush
	pathStreamStateOnly = "/stream_state"     // SYNTHETIC: see transitionPush
)

// mixerNodeName is the audio mixer DSP node. It must match internal/mixer's
// constant of the same name exactly: the frame also carries a node plainly
// called "mixer", which is the VIDEO mixer, and serving the audio state under
// that name would hand the drawer a mixer with no buses and no strips — i.e.
// "nothing is routed to the clean feed", the most dangerous false statement
// that surface can make.
const mixerNodeName = "advanced_audio_mixer"

// ============================== frame envelope ==============================

// statusFrame is one push on the switcher_status socket.
type statusFrame struct {
	Status    []statusEntry `json:"status"`
	Timestamp int64         `json:"timestamp"`
}

// statusEntry is one element of "status".
type statusEntry struct {
	Node  string `json:"node"`
	Path  string `json:"path"`
	State any    `json:"state"`
}

// frame stamps entries with the device clock. MEASURED as epoch milliseconds;
// reading it as seconds or nanoseconds lands in 1970, which internal/mixer
// would render as a permanently stale drawer.
func (a *App) frame(entries ...statusEntry) statusFrame {
	return statusFrame{Status: entries, Timestamp: time.Now().UnixMilli()}
}

// ============================== node inventory ==============================

// inputSpec is one node of the inventory: its wire name and the name the
// operator sees.
type inputSpec struct {
	node    string
	display string
}

// routerInputs are the 24 nodes that carry a stream_state, i.e. the only nodes
// a statusKey may legitimately name.
//
// MEASURED, names and display names verbatim from the capture, including the
// three whose node name contains a SPACE. Those are not decoration: "MIC 1"
// proves that node names are not identifiers, and internal/mixer's
// inputNameOf splits strip names on the last hyphen precisely because of them.
// A mock serving only "camN" would let a regression there pass.
var routerInputs = []inputSpec{
	{"MIC 1", "MIC 1"},
	{"MIC 2", "MIC 2"},
	{"MIC 3", "CLAUDE-TEST-MIC"},
	{"cam1", "Input 1"},
	{"cam2", "Input 2"},
	{"cam3", "Input 3"},
	{"cam4", "Input 4"},
	{"cam5", "Input 5"},
	{"cam6", "Input 6"},
	{"cam7", "REPLAY 1 CLN"},
	{"cam8", "REPLAY 2 CLN"},
	{"cam9", "REPLAY 1 DIRTY"},
	{"cam10", "REPLAY 2 DIRTY"},
	{"cam14", "Input 14"},
	{"cam15", "Input 15"},
	{"cam16", "Input 16"},
	{"cam17", "Input 17"},
	{"cam18", "Input 18"},
	{"cam19", "Input 19"},
	{"cam20", "CLAUDE-TEST-SRT"},
	{"cam21", "CLAUDE-FX"},
	{"cam22", "CLAUDE-COMMS"},
	{"cam23", "Input 23"},
	{"cam24", "Input 24"},
}

// mixerOnlyInputs feed the audio mixer but are NOT router inputs: they carry a
// display_name and no stream_state.
//
// MEASURED, and they are in this file for one reason: internal/m2lx/wire.go
// records that "display_name alone does not make a node a router input,
// stream_state is the test". These three are the counter-examples that make
// that testable. A mock without them lets a parser that keys off display_name
// pass.
var mixerOnlyInputs = []inputSpec{
	{"replay1", "Replay"},
	{"vtr1", "Clip Player 1"},
	{"vtr2", "Clip Player 2"},
}

// mixerInputs is every input of the audio mixer node, router input or not, in
// the order the strip and meter maps are built from.
func mixerInputs() []inputSpec {
	out := make([]inputSpec, 0, len(routerInputs)+len(mixerOnlyInputs))
	out = append(out, routerInputs...)
	out = append(out, mixerOnlyInputs...)
	return out
}

// lookupRouterInput finds a router input by node name.
func lookupRouterInput(node string) (inputSpec, bool) {
	for _, in := range routerInputs {
		if in.node == node {
			return in, true
		}
	}
	return inputSpec{}, false
}

// statusKeyInput resolves the configured -status-key to a router input.
//
// ok is false when the key names one of the twelve nodes that are not router
// inputs, or nothing at all. Neither case is patched over: the mock serves the
// measured inventory and lets internal/m2lx report the mismatch itself, which
// is how the StatusKeyNotFoundError path — including its "names a node that is
// not a router input" variant — becomes reachable at Gate A. Point -status-key
// at "mixer" for the first and at a typo for the second.
func (a *App) statusKeyInput() (inputSpec, bool) {
	return lookupRouterInput(a.opts.StatusKey)
}

// ============================ router input state ============================

// inputState is a router input's whole state: what an entry at path "/"
// carries for one of the 24.
//
// Field order is the marshalled order and matches the capture. The tags are
// the wire names.
type inputState struct {
	DisplayName string          `json:"display_name"`
	Settings    inputSettings   `json:"settings"`
	Statistics  inputStatistics `json:"statistics"`
	StreamState string          `json:"stream_state"`
	Streams     inputStreams    `json:"streams"`
}

type inputSettings struct {
	BackgroundColor string `json:"background_color"`
}

// inputStatistics is state.statistics.
//
// internal/m2lx deliberately never reads bitrate — it FREEZES at its last
// value, so a dead input advertises a healthy bitrate forever. This mock
// reproduces that freeze on purpose (see App.inputStatistics): a mock that
// zeroed the field on disconnect would make the trap disappear, and the
// comment in internal/m2lx warning future maintainers off the field would have
// nothing behind it.
type inputStatistics struct {
	Bitrate                  float64 `json:"bitrate"`
	DiscontinuousPacketCount int64   `json:"discontinuous_packet_count"`
	DiscontinuousPacketRate  float64 `json:"discontinuous_packet_rate"`
	ErrorPacketCount         int64   `json:"error_packet_count"`
	ErrorPacketRate          float64 `json:"error_packet_rate"`
	PacketCount              int64   `json:"packet_count"`
	PacketRate               float64 `json:"packet_rate"`
}

// inputStreams is state.streams. Audio before video, as measured.
type inputStreams struct {
	Audio []audioStream `json:"audio"`
	Video videoStream   `json:"video"`
}

// streamError is the per-stream error object. MEASURED as
// {"code":"","message":"","severity":"none"} on every node in the capture,
// healthy and stopped alike; with no sample of it saying anything else there
// is nothing to vary, so this mock always sends the healthy one.
type streamError struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

// videoStream is state.streams.video. Format is a POINTER so that a stopped
// input marshals "format":null — the measured shape — rather than an empty
// object, which internal/m2lx/format.go treats differently on purpose.
type videoStream struct {
	Error  streamError  `json:"error"`
	Format *videoFormat `json:"format"`
}

// audioStream is one element of state.streams.audio. Same pointer rule.
type audioStream struct {
	Error  streamError  `json:"error"`
	Format *audioFormat `json:"format"`
}

// videoFormat is the measured video format object.
//
// FrameRate is a string. That is not a mistake and not a mock convenience: the
// device sends "frame_rate":"50" beside "width":1920 and "height":1080 as
// numbers, and internal/m2lx/format.go's parseFrameRate exists to cope with
// exactly that. Typing it as a number here would make the mock unable to
// reproduce the one thing that file is for.
type videoFormat struct {
	BitDepth     int    `json:"bit_depth"`
	Codec        string `json:"codec"`
	ColorSpace   string `json:"color_space"`
	FrameRate    string `json:"frame_rate"`
	Height       int    `json:"height"`
	SampleFormat string `json:"sample_format"`
	ScanType     string `json:"scan_type"`
	Width        int    `json:"width"`
}

// audioFormat is the measured audio format object. bit_depth really is 0 on a
// healthy AAC stream.
type audioFormat struct {
	BitDepth     int    `json:"bit_depth"`
	ChannelCount int    `json:"channel_count"`
	Codec        string `json:"codec"`
	SampleRate   int    `json:"sample_rate"`
}

// healthyStreamError is the error object every stream carries in the capture.
var healthyStreamError = streamError{Severity: "none"}

// measuredVideoFormat is cam22's format while this application was streaming
// into it: h264 1920x1080p50, the contribution format spec section 5 pins.
func measuredVideoFormat() *videoFormat {
	return &videoFormat{
		BitDepth:     8,
		Codec:        "h264",
		ColorSpace:   "YCbCr",
		FrameRate:    "50",
		Height:       1080,
		SampleFormat: "420",
		ScanType:     "P",
		Width:        1920,
	}
}

// measuredAudioFormat is cam22's audio format on the same frame: AAC-LC,
// 48 kHz, stereo.
func measuredAudioFormat() *audioFormat {
	return &audioFormat{ChannelCount: 2, Codec: "aac", SampleRate: 48000}
}

// inputState renders one router input's whole state.
//
// Only the -status-key node reflects anything real. It reports whether an SRT
// peer is connected, subject to the two faults that can override that:
//
//   - the LIE fault replaces stream_state and nothing else, so the formats
//     underneath keep telling the truth. That asymmetry is the point: spec
//     section 8's distrust of the lamp exists because stream_state alone is
//     not proof, and a lie that also faked the formats would be no test of it.
//   - the DROP-AUDIO fault empties the audio array, which is the MP2/AC-3
//     silent-drop signature and is NOT the same as the one-element array with
//     a null format that a stopped input sends.
//
// Every other input is stopped, as 22 of the 24 were in the capture.
func (a *App) inputState(in inputSpec) inputState {
	st := inputState{
		DisplayName: in.display,
		Settings:    inputSettings{BackgroundColor: "#000000ff"},
		StreamState: m2lx.StreamStateStopped,
		Streams: inputStreams{
			// A stopped input sends ONE audio element whose format is null.
			// Not an empty array — that means something else entirely.
			Audio: []audioStream{{Error: healthyStreamError}},
			Video: videoStream{Error: healthyStreamError},
		},
	}
	if in.node != a.opts.StatusKey {
		return st
	}

	st.Statistics = a.inputStatistics()

	streaming := a.srtPeerConnected()
	if streaming {
		st.StreamState = m2lx.StreamStateStreaming
		st.Streams.Video.Format = measuredVideoFormat()
		st.Streams.Audio = []audioStream{{Error: healthyStreamError, Format: measuredAudioFormat()}}
	}
	if a.getDropAudio() {
		// The silent-drop signature: an EMPTY array, non-nil so encoding/json
		// renders "audio":[] and not "audio":null.
		st.Streams.Audio = []audioStream{}
	}
	if lie := a.getLieStreamState(); lie != "" {
		st.StreamState = lie
	}
	return st
}

// inputStatistics renders the -status-key node's statistics, and freezes them
// when the peer goes away.
//
// The numbers are derived from the SRT analyzer that is already counting real
// bytes off the real listener (mpegts.go), in the units the capture shows:
// bitrate in kbit/s, packet_count in 188-byte TS packets, and packet_rate the
// two reconciled — MEASURED cam1 reported bitrate 6932.9888 with packet_rate
// 4609.7, and 4609.7 * 188 * 8 / 1000 is 6932.99.
//
// THE FREEZE IS THE POINT. When the peer disconnects these are not zeroed;
// they hold their last value, exactly as the device's do, so the mock keeps
// advertising a healthy bitrate on a dead input. That is the behaviour
// internal/m2lx refuses to read the field over, and reproducing it is how that
// refusal stays honest rather than becoming folklore.
func (a *App) inputStatistics() inputStatistics {
	connected := a.srtPeerConnected()
	an := a.srtAnalyzerSnapshot()

	a.doc.mu.Lock()
	defer a.doc.mu.Unlock()
	if connected {
		a.doc.stats = inputStatistics{
			Bitrate:     round1(an.BitrateBps / 1000),
			PacketCount: int64(an.BytesTotal / tsPacketSize),
			PacketRate:  round1(an.BitrateBps / (tsPacketSize * 8)),
		}
	}
	return a.doc.stats
}

// round1 trims a synthesised statistic to one decimal place, so a log line or
// a diff of two frames is readable. The capture's own values carry more
// precision than that (507.4496); this is the mock being tidy, not the device.
func round1(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*10) / 10
}

// ============================ the mixer DSP node ============================

// mixerState renders the advanced_audio_mixer node's whole state.
//
// It is here rather than stubbed because internal/mixer parses this node out
// of the same frame (state.go), and a mock that served an empty one would give
// the drawer "no strips" — which renders as "nothing is routed to the clean
// feed", the exact false claim that package exists to prevent. So the shape is
// the measured one: 27 inputs, 54 strips, 7 buses, 34 metered keys, faders on
// the strips plus master/aux1/aux2 and NOT on mon1..mon4.
//
// The routing is the measured default and it is the headline fact of the whole
// mixer work package: every camera strip routes to ["master","aux1","aux2"],
// and aux1 IS the clean feed. Commentary sits in the client's clean feed from
// the factory default, with nothing in Sony's UI saying so. The mock serves
// that default so the drawer has something true to expose.
func (a *App) mixerState() map[string]any {
	inputs := map[string]any{}
	matrix := map[string]any{}
	fader := map[string]any{}
	effect := map[string]any{}

	for _, in := range mixerInputs() {
		strips := map[string]any{
			"assign_list":   []any{[]any{1, 2}},
			"channel_count": 2,
		}
		for i, name := range stripNamesOf(in.node) {
			// MEASURED: channel 1 of an input reports sub_ch_mode "ST_W" and
			// channel 2 reports "MONO", while both report sub_ch_mode_set
			// "ST_W". The two keys genuinely disagree, and internal/mixer
			// reads the effective one; a mock that made them agree would let a
			// regression there through.
			subCh := "ST_W"
			if i == 1 {
				subCh = "MONO"
			}
			strips[name] = map[string]any{
				"display_name":    name,
				"follow":          false,
				"follow_sources":  []any{in.node},
				"muted":           true,
				"sub_ch_mode":     subCh,
				"sub_ch_mode_set": "ST_W",
			}
			matrix[name] = map[string]any{
				"outputs":     busesFor(in.node),
				"pfl_outputs": []any{},
			}
			fader[name] = map[string]any{
				"ch_fader": map[string]any{
					"enabled": []any{false, false},
					"gain":    []any{0, 0},
				},
			}
			effect[name] = measuredEffect()
		}
		inputs[in.node] = strips
	}

	// MEASURED: only master, aux1 and aux2 carry an output_fader. mon1..mon4
	// carry none, which is why internal/mixer has a FaderPresent flag — zero-
	// filling those four would assert unity gain on buses the frame is silent
	// about.
	fader["master"] = map[string]any{"output_fader": map[string]any{"gain": 0}}
	fader["aux1"] = map[string]any{"output_fader": map[string]any{"gain": 1}}
	fader["aux2"] = map[string]any{"output_fader": map[string]any{"gain": 1}}

	outputs := map[string]any{
		"master": map[string]any{"channel_count": 2, "muted": false},
		"aux1":   map[string]any{"channel_count": 2, "muted": false},
		"aux2":   map[string]any{"channel_count": 2, "muted": false},
		"mon1":   map[string]any{"channel_count": 2, "muted": false, "pfl_mode": false},
		"mon2":   map[string]any{"channel_count": 2, "muted": false, "pfl_mode": false},
		"mon3":   map[string]any{"channel_count": 2, "muted": false, "pfl_mode": false},
		// MEASURED: mon4 is the only bus with pfl_mode true. It is the PFL bus.
		"mon4": map[string]any{"channel_count": 2, "muted": false, "pfl_mode": true},
	}

	return map[string]any{
		"effect":           effect,
		"fader":            fader,
		"inputs":           inputs,
		"levels":           a.meterMap(pathLevels),
		"matrix":           matrix,
		"outputs":          outputs,
		"peak_hold_levels": a.meterMap(pathPeakHoldLevels),
		"peak_hold_time":   3,
		"peak_levels":      a.meterMap(pathPeakLevels),
	}
}

// stripNamesOf returns an input's two channel strips, "<input>-1" and
// "<input>-2". Note that this produces "MIC 1-1", which is why internal/mixer
// splits a strip name on its LAST hyphen and never on whitespace.
func stripNamesOf(input string) [2]string {
	return [2]string{input + "-1", input + "-2"}
}

// busesFor returns an input's measured default routing.
//
// MEASURED: the three MIC inputs route to ["master","mon<n>"] — their own
// monitor leg — and all 48 other strips route to ["master","aux1","aux2"].
// That default is why the mixer drawer exists; see mixerState.
func busesFor(input string) []any {
	switch input {
	case "MIC 1":
		return []any{"master", "mon1"}
	case "MIC 2":
		return []any{"master", "mon2"}
	case "MIC 3":
		return []any{"master", "mon3"}
	}
	return []any{"master", "aux1", "aux2"}
}

// measuredEffect is one strip's effect block, verbatim from the capture.
// Nothing in this repository reads it; it is served so that the mixer node's
// key set is the measured one rather than the subset this project happens to
// need today.
func measuredEffect() map[string]any {
	return map[string]any{
		"comp_limit": map[string]any{
			"agc_mode": "off", "compressor_th": 0, "limiter_th": 0, "pre_gain": 0,
		},
		"delay": map[string]any{"enabled": false, "time": 0},
		"eq": map[string]any{
			"enabled": false,
			"eq1":     map[string]any{"eq_mode": "highboost", "f0": 12000, "gain": 0, "q_mode": ""},
			"eq2":     map[string]any{"eq_mode": "peak", "f0": 800, "gain": 0, "q_mode": "default"},
			"eq3":     map[string]any{"eq_mode": "lowboost", "f0": 80, "gain": 0, "q_mode": ""},
		},
		"filter":      map[string]any{"enabled": false, "mode": "off"},
		"pan_balance": map[string]any{"gain": 1},
		"trim":        map[string]any{"enabled": false, "gain": 0},
	}
}

// ================================== meters ==================================

// silenceDB is the level a silent channel reports. MEASURED: -100.0 exactly on
// an idle strip, and -99.99999237060547 on one carrying digital silence — the
// second is a float32 -100 widened to float64, not a different level.
const silenceDB = -100.0

// meterKeys is every key the three meter maps carry: channel 1 of each of the
// 27 inputs, then the 7 buses.
//
// MEASURED as 34 keys, and note what is NOT there: no "-2" strip is metered at
// all. internal/mixer's Metered flag exists so that absence renders as absence
// rather than as a meter pinned at 0 dBFS, and it can only be tested against a
// frame that genuinely omits more than half the strips.
func meterKeys() []string {
	inputs := mixerInputs()
	keys := make([]string, 0, len(inputs)+7)
	for _, in := range inputs {
		keys = append(keys, in.node+"-1")
	}
	keys = append(keys, "master", "aux1", "aux2", "mon1", "mon2", "mon3", "mon4")
	return keys
}

// meterMap renders one of the three meter maps.
//
// The levels MOVE. A frozen meter map would make the ~21 frames a second this
// socket really carries indistinguishable from a socket that has stopped, and
// would hide any bug in the drawer's meter rendering behind a still picture.
// The movement is a slow deterministic sweep off doc.phase, not a random walk,
// so two runs of a test see the same numbers.
//
// Only the -status-key strip and the buses it routes to carry signal, and only
// while an SRT peer is actually connected — the meters follow ground truth,
// not the lie fault, for the same reason the formats do.
func (a *App) meterMap(path string) map[string][2]float64 {
	a.doc.mu.Lock()
	phase := a.doc.phase
	a.doc.mu.Unlock()

	live := ""
	if in, ok := a.statusKeyInput(); ok && a.srtPeerConnected() && !a.getDropAudio() {
		live = in.node + "-1"
	}

	// A commentary strip sits around -25 dBFS with a few dB of movement on it;
	// peak_levels ride a little above the instantaneous level and
	// peak_hold_levels above those, which is what a hold is.
	base := -25.6 + 3*math.Sin(float64(phase)/8)
	var offset float64
	switch path {
	case pathPeakLevels:
		offset = 1.5
	case pathPeakHoldLevels:
		offset = 3.0
	}

	out := make(map[string][2]float64, 34)
	for _, k := range meterKeys() {
		out[k] = [2]float64{silenceDB, silenceDB}
	}
	if live == "" {
		return out
	}
	level := [2]float64{round1(base + offset), round1(base + offset - 1.1)}
	out[live] = level
	// The strip routes to master, aux1 and aux2 by default, so all three carry
	// it. aux1 is the clean feed: a moving meter there is the drawer's whole
	// point, and the mock must be able to show it.
	for _, bus := range busesFor(a.opts.StatusKey) {
		if name, ok := bus.(string); ok {
			out[name] = level
		}
	}
	return out
}

// ============================ the other 11 nodes ============================

// staticNodes are the nodes that are neither router inputs nor the audio
// mixer: eleven of the twelve that carry no stream_state.
//
// Their contents matter less than their PRESENCE and their shape. Twelve of
// the thirty-six measured entries look nothing like a router input, and
// internal/m2lx has to skip them without failing the frame that carries them;
// a mock serving only inputs would never exercise that. replay1, vtr1 and vtr2
// in particular carry a display_name and no streams, which is the counter-
// example to "display_name makes it an input".
//
// Values are the measured ones, trimmed where the capture carried pages of
// data this project has no reader for (the video mixer's animation library).
func staticNodes() map[string]any {
	return map[string]any{
		"discovery1": map[string]any{"sources": []any{}},
		// MEASURED as the STRING "0@1000/1", not a number.
		"lipsync": map[string]any{"offset": "0@1000/1"},
		"live_recorder": map[string]any{
			"error":           map[string]any{"code": "", "message": "", "severity": ""},
			"new_markers":     map[string]any{"hls-rec1": []any{}},
			"new_recorded":    map[string]any{},
			"recorders":       []any{map[string]any{"hls_state": "stopped", "mp4_export": false, "mp4_state": "stopped", "recorder": "hls-rec1", "recording": false, "source": "cam1"}},
			"removed_markers": map[string]any{"hls-rec1": []any{}},
			"updated_markers": map[string]any{"hls-rec1": []any{}},
		},
		"media_transfer": map[string]any{"active": []any{}, "inactive": []any{}},
		"mixer": map[string]any{
			"animations": map[string]any{"entries": []any{}},
			"preview":    map[string]any{"background": "cam22"},
			"program":    map[string]any{"background": "replay1"},
			"snapshots":  map[string]any{"entries": []any{}},
			"transition": map[string]any{"rate": 25, "type": "mix"},
		},
		"output_recorder": map[string]any{
			"error":        map[string]any{"code": "", "message": "", "severity": ""},
			"new_markers":  map[string]any{"cln-rec": []any{}, "pgm-rec": []any{}, "pvw-rec": []any{}},
			"new_recorded": map[string]any{},
			"recorders": []any{
				map[string]any{"hls_state": "stopped", "mp4_export": true, "mp4_state": "stopped", "recorder": "pgm-rec", "recording": false, "source": ""},
				map[string]any{"hls_state": "stopped", "mp4_export": true, "mp4_state": "stopped", "recorder": "pvw-rec", "recording": false, "source": ""},
				map[string]any{"hls_state": "stopped", "mp4_export": true, "mp4_state": "stopped", "recorder": "cln-rec", "recording": false, "source": ""},
			},
			"removed_markers": map[string]any{"cln-rec": []any{}, "pgm-rec": []any{}, "pvw-rec": []any{}},
			"updated_markers": map[string]any{"cln-rec": []any{}, "pgm-rec": []any{}, "pvw-rec": []any{}},
		},
		// display_name, no stream_state, no streams. See the doc comment.
		"replay1": map[string]any{
			"clip":                  map[string]any{"asset_id": "recording/hls-rec1/hls-rec1.m3u8", "audio_channels": 2, "duration": 1694.7, "frame_rate": "50", "loaded": true, "name": "hls-rec1"},
			"default_playback_rate": 1,
			"display_name":          "Replay",
			"error":                 map[string]any{"code": "", "message": "", "severity": "none"},
			"loop_enabled":          false,
			"markers":               []any{},
			"playback":              map[string]any{"paused": false, "rate": 1, "time": 337.46, "timecode": "20260701-21:35:19:17"},
			"selection":             map[string]any{"enabled": false, "has_selection": false, "in_point": 0, "out_point": 1694.68},
			"timeline":              map[string]any{"duration": 1694.7},
		},
		"router": map[string]any{"connections": []any{
			map[string]any{"input": "cam1", "output": "ch1"},
			map[string]any{"input": "cam2", "output": "ch2"},
			map[string]any{"input": "replay1", "output": "replay"},
		}},
		"tally": map[string]any{
			"clean_layers":    []any{"program/background", "program/downstream/1"},
			"clean_sources":   []any{"replay1"},
			"preview_layers":  []any{"preview/background", "preview/downstream/1"},
			"preview_sources": []any{"cam22"},
			"program_layers":  []any{"program/background", "program/downstream/1"},
			"program_sources": []any{"replay1"},
		},
		"vtr1": map[string]any{"clip": map[string]any{"loaded": false}, "display_name": "Clip Player 1"},
		"vtr2": map[string]any{"clip": map[string]any{"loaded": false}, "display_name": "Clip Player 2"},
	}
}

// ================================ the frames ================================

// switcherDoc is the mutable state behind the document synthesis: the delta
// sequence, the meter sweep, the frozen statistics and the last lamp reading
// pushed.
//
// It has its own lock rather than sharing App.mu because the broadcaster
// touches it on every frame — up to 21 times a second — while App.mu guards
// auth state and the fault flags, which change a handful of times a match.
type switcherDoc struct {
	mu sync.Mutex

	// seq counts delta frames and drives deltaKindFor, so the measured path
	// mix is reproduced deterministically rather than sampled at random.
	seq int64

	// phase drives the meter sweep. Separate from seq so that the meters move
	// at the rate meters are actually pushed.
	phase int64

	// stats is the -status-key node's statistics, held across a disconnect.
	// See App.inputStatistics for why they freeze rather than zero.
	stats inputStatistics

	// lamp is the last stream_state+streams reading the socket has published,
	// as JSON. A change in it is what transitionFrames pushes; comparing the
	// rendered JSON rather than the fields means a format change is a
	// transition too, not only a stream_state change.
	lamp string

	// verboseAt throttles the verbose delta log to one line a second. See
	// App.logDeltas.
	verboseAt time.Time
}

func newSwitcherDoc() *switcherDoc { return &switcherDoc{} }

// snapshotFrame builds frame 0 of a connection: every node, whole, at "/".
//
// Entries are sorted by node name, which is the order the device sends them
// in, so a frame diffed against the capture lines up.
//
// It also seeds doc.lamp, because the snapshot IS the publication of the
// current reading: without this the first broadcaster tick after a connection
// would see lamp == "" and push a redundant transition frame.
func (a *App) snapshotFrame() statusFrame {
	states := make(map[string]any, len(routerInputs)+12)
	for _, in := range routerInputs {
		states[in.node] = a.inputState(in)
	}
	states[mixerNodeName] = a.mixerState()
	for name, state := range staticNodes() {
		states[name] = state
	}

	names := make([]string, 0, len(states))
	for name := range states {
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make([]statusEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, statusEntry{Node: name, Path: wholeNodePath, State: states[name]})
	}

	if in, ok := a.statusKeyInput(); ok {
		sig := lampSignature(a.inputState(in))
		a.doc.mu.Lock()
		a.doc.lamp = sig
		a.doc.mu.Unlock()
	}
	return a.frame(entries...)
}

// lampSignature renders the part of a node's state the status lamps read —
// stream_state, streams.video.format and streams.audio — as JSON.
//
// It is a change detector, not a payload. Statistics are excluded on purpose:
// packet_count moves on every frame, so including it would make every frame a
// "transition" and the transitionPush modes meaningless.
func lampSignature(st inputState) string {
	b, err := json.Marshal(struct {
		StreamState string       `json:"stream_state"`
		Streams     inputStreams `json:"streams"`
	}{st.StreamState, st.Streams})
	if err != nil {
		// Unreachable: every field is a plain struct of scalars and slices.
		// Returning a value that can never equal a real signature makes the
		// failure mode "push a transition" rather than "silently stop
		// pushing them", which is the safer of the two.
		return "unmarshalable"
	}
	return string(b)
}

// deltaKind is which subtree the next filler delta updates.
type deltaKind int

const (
	deltaLevels deltaKind = iota
	deltaPeakLevels
	deltaPeakHoldLevels
	deltaStatistics
)

// deltaKindFor chooses the kind of the n-th delta of a run.
//
// MEASURED mix over 3180 frames: 1501 "/levels", 1500 "/peak_levels", 163
// "/peak_hold_levels" and 15 "/statistics". The arithmetic below reproduces
// those proportions to within a frame per hundred — 1 statistics, 4
// peak_hold_levels and the remaining 95 split between levels and peak_levels.
//
// It is deterministic rather than random so that a test can assert "the 99th
// delta touches the status key node" instead of waiting and hoping.
func deltaKindFor(n int64) deltaKind {
	switch {
	case n%100 == 99:
		return deltaStatistics
	case n%20 == 19:
		return deltaPeakHoldLevels
	case n%2 == 0:
		return deltaLevels
	default:
		return deltaPeakLevels
	}
}

// nextFrames returns the frames to push on one broadcaster tick.
//
// The order of precedence is deliberate. A transition — the thing an operator
// is actually waiting to see — pre-empts the meter traffic; the decoy fault,
// when armed, replaces every filler so the trap arrives at the full frame
// rate; and otherwise the measured mix runs.
func (a *App) nextFrames() []statusFrame {
	if frames := a.transitionFrames(); len(frames) > 0 {
		return frames
	}
	if frame, ok := a.decoyFrame(); ok {
		return []statusFrame{frame}
	}
	return []statusFrame{a.fillerFrame()}
}

// transitionFrames pushes a change in the -status-key node's lamp reading, in
// whichever of the three shapes transitionPush selects.
//
// It returns nothing when nothing has changed, when the status key names no
// router input, or when the mode is "none". "none" still records the new
// reading, so the mock does not sit re-detecting the same change forever; the
// change simply goes unpublished, and only a reconnect reveals it.
func (a *App) transitionFrames() []statusFrame {
	in, ok := a.statusKeyInput()
	if !ok {
		return nil
	}
	st := a.inputState(in)
	sig := lampSignature(st)

	a.doc.mu.Lock()
	changed := a.doc.lamp != "" && a.doc.lamp != sig
	seeded := a.doc.lamp != ""
	a.doc.lamp = sig
	a.doc.mu.Unlock()

	// Before the first snapshot has gone out there is no published reading to
	// have changed away from, so there is nothing to announce.
	if !seeded || !changed {
		return nil
	}

	switch a.getTransitionPush() {
	case transitionPushNone:
		a.logf("statusws", "transition to %q NOT pushed (-transition-push=none); only a reconnect will reveal it",
			st.StreamState)
		return nil

	case transitionPushDelta:
		a.logf("statusws", "transition to %q pushed as %s then %s subtree deltas on %q",
			st.StreamState, pathStreams, pathStreamStateOnly, in.node)
		// Formats first, then the state that makes a lamp go green, so a
		// consumer never sees "streaming" with the formats not yet merged.
		// Nobody has measured the device's ordering here — see the file
		// comment — so this is the conservative choice, not a reproduction.
		return []statusFrame{
			a.frame(statusEntry{Node: in.node, Path: pathStreams, State: st.Streams}),
			a.frame(statusEntry{Node: in.node, Path: pathStreamStateOnly, State: st.StreamState}),
		}

	default:
		a.logf("statusws", "transition to %q pushed as a whole-node entry on %q at path %q",
			st.StreamState, in.node, wholeNodePath)
		return []statusFrame{a.frame(statusEntry{Node: in.node, Path: wholeNodePath, State: st})}
	}
}

// decoyFrame builds a delta that a parser ignoring "path" would read as a
// whole node.
//
// THIS IS THE BUG THAT CONDEMNED A WORKING INPUT ONCE A SECOND, and it is
// reproducible here on purpose. The device really sends
//
//	{"node":"cam1","path":"/statistics","state":{"bitrate":6523.6,...}}
//
// and a parser that unmarshals that "state" as a node finds no stream_state in
// it and concludes cam1 — the only input on the switcher that was actually
// working — is not a router input. Every second, forever, while the lamps went
// grey and nothing said why.
//
// Two modes, because the trap has two faces:
//
//   - "statistics" is the measured frame. A naive parser condemns the node.
//   - "stream-state" is the sharper one: the same subtree path carrying a
//     state that LOOKS like a complete node, stopped, with null formats. A
//     naive parser does not merely fail to find the node, it believes the
//     decoy and drives the lamps from it — reporting STOPPED, NO VIDEO and NO
//     AUDIO about an input that is streaming. A correct parser merges it at
//     /statistics, where it changes nothing any lamp reads, and the lamps stay
//     green. That difference is the whole test.
//
// Both are aimed at the -status-key node specifically: a decoy about some
// other node would be skipped for the ordinary reason and would prove nothing.
func (a *App) decoyFrame() (statusFrame, bool) {
	mode := a.getDecoyDelta()
	if mode == decoyDeltaOff {
		return statusFrame{}, false
	}
	in, ok := a.statusKeyInput()
	if !ok {
		return statusFrame{}, false
	}

	if mode == decoyDeltaStreamState {
		decoy := inputState{
			DisplayName: in.display,
			Settings:    inputSettings{BackgroundColor: "#000000ff"},
			Statistics:  a.inputStatistics(),
			StreamState: m2lx.StreamStateStopped,
			Streams: inputStreams{
				Audio: []audioStream{{Error: healthyStreamError}},
				Video: videoStream{Error: healthyStreamError},
			},
		}
		return a.frame(statusEntry{Node: in.node, Path: pathStatistics, State: decoy}), true
	}
	return a.frame(statusEntry{Node: in.node, Path: pathStatistics, State: a.inputStatistics()}), true
}

// fillerFrame builds the ordinary once-per-tick delta: the meter traffic that
// is most of what this socket really carries, and the periodic statistics
// update on the streaming input.
//
// When the status key names no router input there is no node to send
// statistics about, so that slot falls back to meters. That is not a
// workaround: on the real device the "/statistics" deltas came from the one
// node that was streaming, and with nothing streaming there would be none.
func (a *App) fillerFrame() statusFrame {
	a.doc.mu.Lock()
	n := a.doc.seq
	a.doc.seq++
	a.doc.phase++
	a.doc.mu.Unlock()

	switch deltaKindFor(n) {
	case deltaStatistics:
		if in, ok := a.statusKeyInput(); ok {
			return a.frame(statusEntry{Node: in.node, Path: pathStatistics, State: a.inputStatistics()})
		}
		return a.meterFrame(pathLevels)
	case deltaPeakHoldLevels:
		return a.meterFrame(pathPeakHoldLevels)
	case deltaPeakLevels:
		return a.meterFrame(pathPeakLevels)
	default:
		return a.meterFrame(pathLevels)
	}
}

// meterFrame is one meter delta on the audio mixer node.
func (a *App) meterFrame(path string) statusFrame {
	return a.frame(statusEntry{Node: mixerNodeName, Path: path, State: a.meterMap(path)})
}
