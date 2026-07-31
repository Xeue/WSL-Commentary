package m2lx

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// The three captures this package is tested against, all taken from the live
// M2L-X instance while another operator's input (cam1, "Input 1") was
// streaming into it. cam22 is this operator's own input, display_name
// "CLAUDE-COMMS".
//
// They are the authority. The parser they replaced was written against prose
// in docs/architecture.md and could not have worked against any real frame
// with any statusKey at all — see wire.go. Note that the SNAPSHOT alone is not
// enough: it is one frame, and the socket's snapshot-then-delta behaviour is
// invisible in it. The two deltas are here because a parser tested only
// against the snapshot passes while misreading every frame after the first.
const (
	liveSnapshot = "testdata/switcher_status-live-2026-07-31.json"

	// A delta about the ONE node that was actually working. This is the trap:
	// a parser that ignores "path" reads its state as a node, finds no
	// stream_state, and concludes cam1 is not a router input — once a second,
	// for as long as it is streaming.
	liveDeltaStatistics = "testdata/switcher_status-delta-statistics-2026-08-01.json"

	// The frame the socket actually spends its time sending: audio meters for
	// a node that is not a router input at all, about twenty times a second.
	liveDeltaLevels = "testdata/switcher_status-delta-levels-2026-08-01.json"
)

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

func liveFrame(t *testing.T) []byte { return readFixture(t, liveSnapshot) }

// frame assembles a synthetic switcher_status frame from raw entry JSON. It
// exists for the shapes the captures cannot show — none of them has a
// malformed entry, a missing state, or a format of an unexpected type — and
// those are exactly the shapes a parser fails silently on.
func frame(entries ...string) []byte {
	return []byte(`{"status":[` + strings.Join(entries, ",") + `],"timestamp":1785522083212}`)
}

// entry builds one well-formed whole-node entry, at path "/".
func entry(node, state string) string {
	return fmt.Sprintf(`{"node":%q,"path":"/","state":%s}`, node, state)
}

// deltaEntry builds a partial update: an entry at some path OTHER than "/",
// whose state is the value at that path rather than a whole node.
func deltaEntry(node, path, state string) string {
	return fmt.Sprintf(`{"node":%q,"path":%q,"state":%s}`, node, path, state)
}

// streamState builds the state object of a router input, in the measured
// shape: display_name, statistics (never read), stream_state, streams.
func streamState(displayName, state, videoFormat string, audioFormats ...string) string {
	audio := make([]string, 0, len(audioFormats))
	for _, f := range audioFormats {
		audio = append(audio, fmt.Sprintf(`{"error":{"code":"","message":"","severity":"none"},"format":%s}`, f))
	}
	return fmt.Sprintf(`{"display_name":%q,"settings":{"background_color":"#000000ff"},`+
		`"statistics":{"bitrate":507.4496,"packet_count":3374},`+
		`"stream_state":%q,"streams":{"audio":[%s],`+
		`"video":{"error":{"code":"","message":"","severity":"none"},"format":%s}}}`,
		displayName, state, strings.Join(audio, ","), videoFormat)
}

// ---------------------------------------------------------------------------
// The live snapshot
// ---------------------------------------------------------------------------

func TestLookupNodeAgainstTheLiveSnapshot(t *testing.T) {
	payload := liveFrame(t)

	cases := []struct {
		name            string
		key             string
		wantDisplayName string
		wantState       string
		wantVideo       VideoFormat
		wantAudio       []AudioFormat
	}{
		{
			// The whole reason the capture was taken. Every one of these
			// values was unreachable before: the parser was looking for
			// "cam22" as a top-level property of a document whose top level
			// has exactly two keys, "status" and "timestamp".
			name:            "cam22 is the operator's own streaming input",
			key:             "cam22",
			wantDisplayName: "CLAUDE-COMMS",
			wantState:       StreamStateStreaming,
			wantVideo:       VideoFormat{Raw: liveVideoRaw, Codec: "h264", Width: 1920, Height: 1080, FrameRate: 50},
			wantAudio:       []AudioFormat{{Raw: liveAudioRaw, Codec: "aac", SampleRate: 48000, Channels: 2}},
		},
		{
			// Somebody else's feed, up at the same time and identical in
			// every respect except its name. This is why display_name, not
			// "which one is streaming", is what an operator picks on.
			name:            "cam1 is another operator's streaming input",
			key:             "cam1",
			wantDisplayName: "Input 1",
			wantState:       StreamStateStreaming,
			wantVideo:       VideoFormat{Raw: liveVideoRaw, Codec: "h264", Width: 1920, Height: 1080, FrameRate: 50},
			wantAudio:       []AudioFormat{{Raw: liveAudioRaw, Codec: "aac", SampleRate: 48000, Channels: 2}},
		},
		{
			// A stopped input. NOTE what this is not: the audio array is NOT
			// empty, it holds one entry whose format is null. The MP2/AC-3
			// silent-drop signature is an EMPTY array, and conflating the two
			// would make every idle input on the switcher look like a dropped
			// audio track.
			name:            "cam7 is stopped, with null formats and one null audio entry",
			key:             "cam7",
			wantDisplayName: "REPLAY 1 CLN",
			wantState:       StreamStateStopped,
			wantVideo:       VideoFormat{},
			wantAudio:       []AudioFormat{{}},
		},
		{
			// Node names are not identifiers: three of them have spaces.
			name:            "a node name with a space",
			key:             "MIC 3",
			wantDisplayName: "CLAUDE-TEST-MIC",
			wantState:       StreamStateStopped,
			wantVideo:       VideoFormat{},
			wantAudio:       []AudioFormat{{}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			look, err := lookupNode(payload, tc.key)
			if err != nil {
				t.Fatalf("lookupNode(%q) error = %v", tc.key, err)
			}
			if !look.Found {
				t.Fatalf("lookupNode(%q) did not find it", tc.key)
			}
			node := look.Node
			if node.DisplayName != tc.wantDisplayName {
				t.Errorf("display_name = %q, want %q", node.DisplayName, tc.wantDisplayName)
			}
			if node.StreamState != tc.wantState {
				t.Errorf("stream_state = %q, want %q", node.StreamState, tc.wantState)
			}
			if got := parseVideoFormat(node.Streams.Video.Format); got != tc.wantVideo {
				t.Errorf("video:\n got %+v\nwant %+v", got, tc.wantVideo)
			}
			if len(node.Streams.Audio) != len(tc.wantAudio) {
				t.Fatalf("audio has %d entries, want %d", len(node.Streams.Audio), len(tc.wantAudio))
			}
			for i, a := range node.Streams.Audio {
				if got := parseAudioFormat(a.Format); got != tc.wantAudio[i] {
					t.Errorf("audio[%d]:\n got %+v\nwant %+v", i, got, tc.wantAudio[i])
				}
			}
		})
	}
}

func TestLookupNodeEnumeratesTheSnapshot(t *testing.T) {
	look, err := lookupNode(liveFrame(t), "cam22")
	if err != nil {
		t.Fatalf("lookupNode error = %v", err)
	}
	// 24 of the capture's 36 entries carry a stream_state.
	if len(look.Inputs) != 24 {
		t.Fatalf("Inputs has %d nodes, want the capture's 24 router inputs", len(look.Inputs))
	}
	// Streaming first, then by node name: an operator hunting for their own
	// feed is looking for one that is up.
	if look.Inputs[0].Key != "cam1" || look.Inputs[1].Key != "cam22" {
		t.Errorf("Inputs leads with %q, %q; want the two streaming nodes",
			look.Inputs[0].Key, look.Inputs[1].Key)
	}
	if look.Inputs[1].DisplayName != "CLAUDE-COMMS" || look.Inputs[1].Video != liveVideoRaw {
		t.Errorf("Inputs[1] = %+v, want cam22 fully described", look.Inputs[1])
	}
	// The twelve non-router entries must not be offered: pointing a statusKey
	// at one could never work, so listing one would be a wrong answer dressed
	// as a helpful one.
	for _, c := range look.Inputs {
		switch c.Key {
		case "router", "tally", "mixer", "lipsync", "advanced_audio_mixer", "replay1", "vtr1", "vtr2":
			t.Errorf("Inputs offers %q, which carries no stream_state", c.Key)
		}
	}
}

// ---------------------------------------------------------------------------
// The deltas — what the socket actually spends its time sending
// ---------------------------------------------------------------------------

func TestLookupNodeOnALiveStatisticsDelta(t *testing.T) {
	// THE trap frame, verbatim off the wire:
	//
	//   {"node":"cam1","path":"/statistics","state":{"bitrate":6523.6,...}}
	//
	// Ignore "path" and this reads as "cam1 is a node with no stream_state",
	// i.e. "the input you are watching is not a router input" — about the one
	// input on the switcher that was working, once a second, for ever.
	payload := readFixture(t, liveDeltaStatistics)

	look, err := lookupNode(payload, "cam1")
	if err != nil {
		t.Fatalf("lookupNode error = %v; a delta is a perfectly good frame", err)
	}
	if look.Found {
		t.Error("Found = true: a /statistics delta is not a node state")
	}
	if look.NotAnInput {
		t.Error("NotAnInput = true: a delta says NOTHING about whether cam1 is a router input")
	}
	if len(look.Inputs) != 0 {
		t.Errorf("Inputs = %+v, want none: a delta enumerates nothing", look.Inputs)
	}
}

func TestLookupNodeOnALiveLevelsDelta(t *testing.T) {
	// The frame the socket sends about twenty times a second. It must cost
	// nothing and claim nothing.
	payload := readFixture(t, liveDeltaLevels)

	for _, key := range []string{"cam22", "advanced_audio_mixer", "cam1"} {
		t.Run(key, func(t *testing.T) {
			look, err := lookupNode(payload, key)
			if err != nil {
				t.Fatalf("lookupNode error = %v", err)
			}
			if look.Found || look.NotAnInput || len(look.Inputs) != 0 {
				t.Errorf("lookup = %+v, want an entirely empty result", look)
			}
		})
	}
}

func TestExtractAllIgnoresDeltas(t *testing.T) {
	// WatchAll's view of the same thing: a delta contributes no nodes, so
	// Discovery is told nothing rather than told something false.
	for _, path := range []string{liveDeltaStatistics, liveDeltaLevels} {
		t.Run(path, func(t *testing.T) {
			nodes, err := extractAll(readFixture(t, path))
			if err != nil {
				t.Fatalf("extractAll error = %v", err)
			}
			if len(nodes) != 0 {
				t.Errorf("extractAll = %+v, want none from a delta", nodes)
			}
		})
	}
}

func TestLookupNodeSkipsPartialUpdatesAtAnyPath(t *testing.T) {
	// Only "/" carries a whole node state. Every other path is a subtree this
	// package does not merge, and the state at it is NOT a node however much
	// it may resemble one.
	cases := []struct {
		name  string
		path  string
		state string
	}{
		{"statistics, as measured", "/statistics", `{"bitrate":6523.6,"packet_count":43375}`},
		{"a subtree that happens to contain a stream_state", "/streams", `{"stream_state":"streaming"}`},
		{"the stream_state leaf itself", "/stream_state", `"streaming"`},
		{"a scalar at a leaf", "/settings/background_color", `"#000000ff"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			look, err := lookupNode(frame(deltaEntry("cam22", tc.path, tc.state)), "cam22")
			if err != nil {
				t.Fatalf("lookupNode error = %v", err)
			}
			if look.Found {
				t.Errorf("Found = true for a partial update at %q", tc.path)
			}
			if look.NotAnInput {
				t.Errorf("NotAnInput = true for a partial update at %q; it is not evidence either way", tc.path)
			}
			if len(look.Inputs) != 0 {
				t.Errorf("Inputs = %+v; a partial update enumerates nothing", look.Inputs)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// A statusKey that matches nothing
// ---------------------------------------------------------------------------

func TestLookupNodeFindsNothingForAKeyThatIsNotThere(t *testing.T) {
	look, err := lookupNode(liveFrame(t), "cam99")
	if err != nil {
		t.Fatalf("lookupNode error = %v; a key that is not there is not a malformed frame", err)
	}
	if look.Found {
		t.Error("Found = true for cam99")
	}
	if look.NotAnInput {
		t.Error("NotAnInput = true, but there is no cam99 entry in the frame at all")
	}
	if len(look.Inputs) != 24 {
		t.Errorf("Inputs has %d nodes; the frame still enumerates the switcher", len(look.Inputs))
	}
}

func TestLookupNodeDistinguishesANodeThatIsNotARouterInput(t *testing.T) {
	// Typing "mixer" is a different mistake from typing "cam99", and an
	// operator who has made one of them should not be told they made the
	// other. Both nodes really are in the frame; neither can drive a lamp.
	payload := liveFrame(t)

	for _, key := range []string{"router", "tally", "mixer", "lipsync", "advanced_audio_mixer", "vtr1"} {
		t.Run(key, func(t *testing.T) {
			look, err := lookupNode(payload, key)
			if err != nil {
				t.Fatalf("lookupNode(%q) error = %v", key, err)
			}
			if look.Found {
				t.Errorf("Found = true for %q, which carries no stream_state", key)
			}
			if !look.NotAnInput {
				t.Errorf("NotAnInput = false, but %q IS an entry in the frame — just not a router input", key)
			}
		})
	}
}

func TestStatusKeyNotFoundErrorNamesWhatWasLookedForAndWhatIsThere(t *testing.T) {
	look, err := lookupNode(liveFrame(t), "cam99")
	if err != nil {
		t.Fatalf("lookupNode error = %v", err)
	}

	missing := (&StatusKeyNotFoundError{Key: "cam99", Available: look.Inputs}).Error()
	for _, want := range []string{
		`"cam99"`,      // what was looked for
		`"cam22"`,      // and what it could have been
		"CLAUDE-COMMS", // by the name the operator would recognise
		StreamStateStreaming,
		"REPLAY 1 CLN",
	} {
		if !strings.Contains(missing, want) {
			t.Errorf("the report does not mention %q:\n%s", want, missing)
		}
	}

	notAnInput := (&StatusKeyNotFoundError{Key: "mixer", NotAnInput: true, Available: look.Inputs}).Error()
	if !strings.Contains(notAnInput, "no stream_state") {
		t.Errorf("the report does not say why mixer can never work:\n%s", notAnInput)
	}
	if notAnInput == missing {
		t.Error("the two mistakes produce the same report")
	}

	// The degenerate case has to read as a sentence too.
	empty := (&StatusKeyNotFoundError{Key: "cam99"}).Error()
	if !strings.Contains(empty, "no router inputs") {
		t.Errorf("with nothing to offer, the report reads badly:\n%s", empty)
	}

	// An input whose display_name is blank must not leave a hole in the list.
	unnamed := (&StatusKeyNotFoundError{
		Key:       "cam99",
		Available: []NodeChoice{{Key: "cam5", StreamState: StreamStateStopped}},
	}).Error()
	if !strings.Contains(unnamed, "unnamed") {
		t.Errorf("a blank display_name renders as a gap:\n%s", unnamed)
	}
}

// ---------------------------------------------------------------------------
// Synthetic shapes the captures cannot produce
// ---------------------------------------------------------------------------

func TestLookupNodeSyntheticShapes(t *testing.T) {
	good := entry("cam7", streamState("MY FEED", StreamStateStreaming, liveVideoFormat, liveAudioFormat))

	cases := []struct {
		name string
		body []byte
		key  string
		// exactly one of these is expected
		wantFound      bool
		wantNotAnInput bool
		wantMissing    bool
		wantErrText    string
	}{
		{
			name:      "the empty audio array survives",
			body:      frame(entry("cam7", streamState("MY FEED", StreamStateStreaming, liveVideoFormat))),
			key:       "cam7",
			wantFound: true,
		},
		{
			name:        "state missing entirely",
			body:        frame(`{"node":"cam7","path":"/"}`),
			key:         "cam7",
			wantErrText: "unexpected shape",
		},
		{
			name:        "state is not an object",
			body:        frame(`{"node":"cam7","path":"/","state":"streaming"}`),
			key:         "cam7",
			wantErrText: "unexpected shape",
		},
		{
			name:        "stream_state is a number",
			body:        frame(entry("cam7", `{"stream_state":12345}`)),
			key:         "cam7",
			wantErrText: "unexpected shape",
		},
		{
			name:        "streams.video is not an object",
			body:        frame(entry("cam7", `{"stream_state":"streaming","streams":{"video":"nope"}}`)),
			key:         "cam7",
			wantErrText: "unexpected shape",
		},
		{
			name:        "streams.audio is not an array",
			body:        frame(entry("cam7", `{"stream_state":"streaming","streams":{"audio":{}}}`)),
			key:         "cam7",
			wantErrText: "unexpected shape",
		},
		{
			name:        "an audio element is not an object",
			body:        frame(entry("cam7", `{"stream_state":"streaming","streams":{"audio":[42]}}`)),
			key:         "cam7",
			wantErrText: "unexpected shape",
		},
		{
			// Scanning is not asking. A node we were not asked about, in a
			// shape we cannot read, must not take the frame down with it.
			name:      "another node's unreadable state does not lose ours",
			body:      frame(`{"node":"cam8","path":"/","state":"nonsense"}`, good),
			key:       "cam7",
			wantFound: true,
		},
		{
			// The leaf is where tolerance lives: this must NOT error, because
			// erroring greys the lamp and says nothing, where parsing
			// leniently reddens it and writes the offending value on it.
			name:      "a format object of entirely unexpected types is tolerated",
			body:      frame(entry("cam7", streamState("MY FEED", StreamStateStreaming, `{"codec":123,"width":"wide"}`, `"aac 48000 2ch"`))),
			key:       "cam7",
			wantFound: true,
		},
		{
			// One bad apple in the array must not lose the node we asked for.
			name:      "a status array carrying a non-object",
			body:      frame(`42`, good, `"nonsense"`, `null`, `[1,2]`),
			key:       "cam7",
			wantFound: true,
		},
		{
			name:        "an entry whose node name is not a string is skipped",
			body:        frame(`{"node":42,"path":"/","state":{"stream_state":"streaming"}}`),
			key:         "cam7",
			wantMissing: true,
		},
		{
			name:        "an entry with a blank node name can never be a statusKey",
			body:        frame(entry("", streamState("", StreamStateStreaming, liveVideoFormat))),
			key:         "",
			wantMissing: true,
		},
		{
			name:        "an entry with no path at all is not a whole node state",
			body:        frame(`{"node":"cam7","state":` + streamState("MY FEED", StreamStateStreaming, liveVideoFormat) + `}`),
			key:         "cam7",
			wantMissing: true,
		},
		{
			name:        "the payload is not JSON",
			body:        []byte(`not json at all`),
			key:         "cam7",
			wantErrText: "not a JSON object",
		},
		{
			name:        "the payload is an array, as the old parser's tests assumed a frame could be",
			body:        []byte(`["cam7","cam8"]`),
			key:         "cam7",
			wantErrText: "not a JSON object",
		},
		{
			name:        "the payload is empty",
			body:        []byte(``),
			key:         "cam7",
			wantErrText: "not a JSON object",
		},
		{
			// The shape the whole package used to assume: nodes as top-level
			// properties. It must be refused loudly, not read as an empty
			// switcher.
			name:        "the old assumed shape, nodes as top-level properties",
			body:        []byte(`{"cam7":{"stream_state":"streaming"}}`),
			key:         "cam7",
			wantErrText: `no "status" array`,
		},
		{
			name:        "status is present but not an array",
			body:        []byte(`{"status":{"cam7":{}},"timestamp":1}`),
			key:         "cam7",
			wantErrText: "not an array",
		},
		{
			name:        "status is an empty array",
			body:        frame(),
			key:         "cam7",
			wantMissing: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			look, err := lookupNode(tc.body, tc.key)

			if tc.wantErrText != "" {
				if err == nil {
					t.Fatalf("lookupNode() error = nil, want one mentioning %q", tc.wantErrText)
				}
				if !strings.Contains(err.Error(), tc.wantErrText) {
					t.Fatalf("lookupNode() error = %v, want it to mention %q", err, tc.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("lookupNode() error = %v, want none", err)
			}
			if look.Found != tc.wantFound {
				t.Errorf("Found = %v, want %v", look.Found, tc.wantFound)
			}
			if look.NotAnInput != tc.wantNotAnInput {
				t.Errorf("NotAnInput = %v, want %v", look.NotAnInput, tc.wantNotAnInput)
			}
			if tc.wantMissing && (look.Found || look.NotAnInput) {
				t.Errorf("lookup = %+v, want nothing found at all", look)
			}
			if tc.wantFound && look.Node.StreamState != StreamStateStreaming {
				t.Errorf("stream_state = %q, want streaming", look.Node.StreamState)
			}
		})
	}
}

func TestLookupNodeEmptyAudioArrayIsNotANullFormat(t *testing.T) {
	// The MP2/AC-3 silent-drop signature is an EMPTY audio array. The live
	// capture shows that a merely stopped input sends a one-element array
	// whose format is null. Both make the AUDIO lamp red, but only one of
	// them means M2L-X threw the audio away, and the difference must survive
	// the parser.
	empty, err := lookupNode(
		frame(entry("cam7", streamState("MY FEED", StreamStateStreaming, liveVideoFormat))), "cam7")
	if err != nil || !empty.Found {
		t.Fatalf("lookupNode() = (%+v, %v)", empty, err)
	}
	if empty.Node.Streams.Audio == nil {
		t.Error("audio is nil; an explicit [] must survive as a non-nil empty slice")
	}
	if len(empty.Node.Streams.Audio) != 0 {
		t.Errorf("audio = %+v, want empty", empty.Node.Streams.Audio)
	}

	nullFormat, err := lookupNode(liveFrame(t), "cam7")
	if err != nil || !nullFormat.Found {
		t.Fatalf("lookupNode(cam7) = (%+v, %v)", nullFormat, err)
	}
	if len(nullFormat.Node.Streams.Audio) != 1 {
		t.Fatalf("a stopped live input has %d audio entries, want the measured 1",
			len(nullFormat.Node.Streams.Audio))
	}
}

func TestLookupNodeIgnoresStatisticsBitrate(t *testing.T) {
	// statistics.bitrate FREEZES at its last value, so a dead input advertises
	// a healthy bitrate forever (docs/architecture.md line 669). It is in the
	// live capture — 507.4 on cam22 — and it must stay unmodelled, so that no
	// future change can start reading it by accident.
	look, err := lookupNode(liveFrame(t), "cam22")
	if err != nil || !look.Found {
		t.Fatalf("lookupNode(cam22) = (%+v, %v)", look, err)
	}
	if strings.Contains(fmt.Sprintf("%+v", look.Node), "507.4") {
		t.Errorf("the decoded node carries statistics.bitrate: %+v", look.Node)
	}
	if strings.Contains(parseVideoFormat(look.Node.Streams.Video.Format).Raw, "507.4") {
		t.Error("VideoFormat.Raw carries statistics.bitrate")
	}
	for _, c := range look.Inputs {
		if strings.Contains(c.Video, "bitrate") || strings.Contains(c.Video, "507.4") {
			t.Errorf("a NodeChoice carries statistics.bitrate: %+v", c)
		}
	}
}
