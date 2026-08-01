package m2lx

import (
	"bytes"
	"encoding/json"
	"testing"
)

// The document is tested from both ends, and it needs both.
//
// The three captures prove it does the right thing with the frames the device
// actually sends. They CANNOT prove much else: they contain one snapshot and
// two deltas, at two of the four paths ever observed, and every one of them is
// structurally perfect. A caution proven twice on this project is that a
// mutation to parsing logic leaves every real-fixture test passing, because
// the captured frames happen to be insensitive to it — so the synthetic half
// below is not padding. It is the half that catches things.

// applySnapshot returns a document baselined from the real connect capture.
func applySnapshot(t *testing.T) *document {
	t.Helper()
	d := newDocument()
	eff, err := d.apply(readFixture(t, liveSnapshot))
	if err != nil {
		t.Fatalf("applying the live connect snapshot: %v", err)
	}
	if !eff.Snapshot {
		t.Fatalf("the live connect snapshot was not recognised as a snapshot")
	}
	return d
}

// nodeKeys returns the top-level keys of a node's stored state.
func nodeKeys(t *testing.T, d *document, node string) map[string]json.RawMessage {
	t.Helper()
	raw, ok := d.nodes[node]
	if !ok {
		t.Fatalf("node %q is not in the document", node)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("node %q does not hold a JSON object: %v", node, err)
	}
	return fields
}

// ---------------------------------------------------------------------------
// The real captures
// ---------------------------------------------------------------------------

func TestDocumentTakesTheLiveConnectSnapshotWhole(t *testing.T) {
	d := applySnapshot(t)

	if len(d.nodes) != 36 {
		t.Fatalf("document holds %d nodes, want all 36 entries of the capture", len(d.nodes))
	}

	// The 24 that carry a stream_state are readable as router inputs; the 12
	// that do not are not, and must not become so by being in the document.
	if node, ok := d.streamNode("cam22"); !ok {
		t.Fatal("cam22, the operator's own input, is not readable as a router input")
	} else if node.DisplayName != "CLAUDE-COMMS" || node.StreamState != StreamStateStreaming {
		t.Fatalf("cam22 = %+v", node)
	}
	for _, name := range []string{"advanced_audio_mixer", "mixer", "router", "tally", "vtr1"} {
		if _, ok := d.streamNode(name); ok {
			t.Errorf("%q reads as a router input; it carries no stream_state", name)
		}
	}
	if _, ok := d.streamNode("cam99"); ok {
		t.Error("a node that is not in the document reads as a router input")
	}
}

func TestDocumentMergesTheLiveStatisticsDelta(t *testing.T) {
	// The frame that was the whole trap. It is about cam1 — the one input that
	// WAS working — and a parser that reads it as a whole node concludes cam1
	// is not a router input, once a second, forever.
	d := applySnapshot(t)
	before := nodeKeys(t, d, "cam1")

	eff, err := d.apply(readFixture(t, liveDeltaStatistics))
	if err != nil {
		t.Fatalf("applying the statistics delta: %v", err)
	}
	if eff.Snapshot {
		t.Error("a /statistics delta was taken for a snapshot")
	}
	if eff.Merged != 1 || eff.Skipped != 0 {
		t.Errorf("merged %d, skipped %d; want the one entry merged", eff.Merged, eff.Skipped)
	}
	if len(eff.Touched) != 1 || !eff.Touched["cam1"] {
		t.Errorf("Touched = %v, want cam1 alone", eff.Touched)
	}

	// It did NOT replace the node. Every key cam1 had, it still has.
	after := nodeKeys(t, d, "cam1")
	for k := range before {
		if _, ok := after[k]; !ok {
			t.Errorf("cam1 lost key %q to a /statistics delta", k)
		}
	}
	if len(d.nodes) != 36 {
		t.Errorf("document holds %d nodes after a delta, want 36", len(d.nodes))
	}

	// And cam1 is still a router input, with its formats untouched.
	node, ok := d.streamNode("cam1")
	if !ok {
		t.Fatal("cam1 stopped being a router input after a /statistics delta — the exact regression this guards")
	}
	if node.StreamState != StreamStateStreaming || node.DisplayName != "Input 1" {
		t.Errorf("cam1 = %+v", node)
	}
	if v := parseVideoFormat(node.Streams.Video.Format); v.Codec != "h264" || v.Width != 1920 || v.Height != 1080 {
		t.Errorf("cam1 video = %+v", v)
	}

	// The subtree the delta named really did change, or nothing was merged
	// at all and the assertions above would pass on a no-op.
	var stats struct {
		Bitrate     float64 `json:"bitrate"`
		PacketCount int     `json:"packet_count"`
	}
	if err := json.Unmarshal(after["statistics"], &stats); err != nil {
		t.Fatalf("cam1 statistics: %v", err)
	}
	if stats.Bitrate != 6523.6 || stats.PacketCount != 43375 {
		t.Errorf("cam1 statistics = %+v, want the delta's values (6523.6 / 43375), not the snapshot's", stats)
	}

	// Everything else is byte-identical to what arrived. A delta about cam1
	// must not disturb a single other node.
	if !bytes.Equal(d.nodes["cam22"], nodeRawFromSnapshot(t, "cam22")) {
		t.Error("cam22's stored bytes changed when a delta about cam1 was merged")
	}
}

func TestDocumentMergesTheLiveLevelsDelta(t *testing.T) {
	// The frame the socket actually spends its time sending: about fifteen a
	// second, about a node that is not a router input at all.
	d := applySnapshot(t)
	before := nodeKeys(t, d, "advanced_audio_mixer")

	eff, err := d.apply(readFixture(t, liveDeltaLevels))
	if err != nil {
		t.Fatalf("applying the levels delta: %v", err)
	}
	if eff.Snapshot || eff.Merged != 1 || eff.Skipped != 0 {
		t.Fatalf("effect = %+v, want one merge and no snapshot", eff)
	}
	if len(eff.Touched) != 1 || !eff.Touched["advanced_audio_mixer"] {
		t.Errorf("Touched = %v, want advanced_audio_mixer alone", eff.Touched)
	}

	after := nodeKeys(t, d, "advanced_audio_mixer")
	for _, k := range []string{"effect", "fader", "inputs", "matrix", "outputs", "peak_levels", "peak_hold_levels", "peak_hold_time"} {
		if _, ok := after[k]; !ok {
			t.Errorf("advanced_audio_mixer lost key %q to a /levels delta", k)
		}
	}
	if len(after) != len(before) {
		t.Errorf("advanced_audio_mixer has %d keys after the delta, had %d", len(after), len(before))
	}

	var levels map[string][]float64
	if err := json.Unmarshal(after["levels"], &levels); err != nil {
		t.Fatalf("levels: %v", err)
	}
	if got := levels["cam1-1"]; len(got) != 2 || got[0] != -25.617328643798828 {
		t.Errorf("levels[cam1-1] = %v, want the delta's value", got)
	}

	// It is still not a router input, and merging a delta into it must never
	// make it look like one.
	if _, ok := d.streamNode("advanced_audio_mixer"); ok {
		t.Error("advanced_audio_mixer reads as a router input after a /levels merge")
	}
}

// nodeRawFromSnapshot returns one node's state exactly as it arrives in the
// live capture, for byte-for-byte comparison against the document.
func nodeRawFromSnapshot(t *testing.T, node string) json.RawMessage {
	t.Helper()
	entries, err := parseFrame(readFixture(t, liveSnapshot))
	if err != nil {
		t.Fatalf("parsing the live snapshot: %v", err)
	}
	for _, raw := range entries {
		var e wireEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}
		if e.Node == node {
			return e.State
		}
	}
	t.Fatalf("node %q is not in the live snapshot", node)
	return nil
}

func TestDocumentKeepsUntouchedNodesByteForByte(t *testing.T) {
	// Storing raw bytes rather than a decoded map is deliberate: VideoFormat.Raw
	// and AudioFormat.Raw (format.go) exist so an operator can see what
	// ACTUALLY arrived, and a round trip through map[string]any would quietly
	// renormalise it. A node no delta has touched must be the wire bytes.
	d := applySnapshot(t)
	if _, err := d.apply(readFixture(t, liveDeltaLevels)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cam1", "cam22", "MIC 1", "router"} {
		if !bytes.Equal(d.nodes[name], nodeRawFromSnapshot(t, name)) {
			t.Errorf("node %q was re-encoded although nothing touched it", name)
		}
	}
}

// ---------------------------------------------------------------------------
// The shapes no capture contains
// ---------------------------------------------------------------------------

func TestDocumentSkipsADeltaBeforeAnySnapshot(t *testing.T) {
	// A connection whose first frame is a delta has no baseline. Merging into
	// nothing would build a node out of one subtree — a node with no
	// stream_state, which reads exactly like "your statusKey names something
	// that is not a router input".
	d := newDocument()

	eff, err := d.apply(readFixture(t, liveDeltaStatistics))
	if err != nil {
		t.Fatalf("applying a delta to an empty document: %v", err)
	}
	if eff.Snapshot || eff.Merged != 0 || eff.Skipped != 1 {
		t.Errorf("effect = %+v, want the delta skipped", eff)
	}
	if len(eff.Touched) != 0 {
		t.Errorf("Touched = %v, want nothing", eff.Touched)
	}
	if len(d.nodes) != 0 {
		t.Errorf("document invented %d node(s) from a delta alone: %v", len(d.nodes), d.nodes)
	}

	// And the delta is not retroactively applied when the snapshot finally
	// turns up: what it said is gone, not queued.
	if _, err := d.apply(liveFrame(t)); err != nil {
		t.Fatal(err)
	}
	node, ok := d.streamNode("cam1")
	if !ok {
		t.Fatal("cam1 is not a router input after the snapshot arrived")
	}
	if node.StreamState != StreamStateStreaming {
		t.Errorf("cam1 = %+v", node)
	}
	var stats struct {
		Bitrate float64 `json:"bitrate"`
	}
	if err := json.Unmarshal(nodeKeys(t, d, "cam1")["statistics"], &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Bitrate != 6932.9888 {
		t.Errorf("cam1 bitrate = %v, want the snapshot's 6932.9888: a delta from before the baseline was replayed into it", stats.Bitrate)
	}
}

func TestDocumentSkipsADeltaForANodeNotInTheBaseline(t *testing.T) {
	// The same failure from the other end: the baseline is there, but not for
	// this node. Creating it would produce a partial node that carries no
	// stream_state — the very shape the previous fix exists to prevent being
	// mistaken for a whole node.
	d := applySnapshot(t)

	eff, err := d.apply(frame(deltaEntry("cam99", "/statistics", `{"bitrate":1.0}`)))
	if err != nil {
		t.Fatalf("applying a delta for an unknown node: %v", err)
	}
	if eff.Merged != 0 || eff.Skipped != 1 || len(eff.Touched) != 0 {
		t.Errorf("effect = %+v, want the delta skipped", eff)
	}
	if _, ok := d.nodes["cam99"]; ok {
		t.Fatal("a delta created a node the snapshot never carried")
	}
	if len(d.nodes) != 36 {
		t.Errorf("document holds %d nodes, want the 36 the snapshot carried", len(d.nodes))
	}
}

func TestDocumentMergesPathsItHasNeverSeen(t *testing.T) {
	// Only four paths have ever been observed. The fifth is going to arrive,
	// and it has to merge correctly or be skipped safely — never be mistaken
	// for a whole node. These are all paths no capture contains.
	base := func(t *testing.T) *document {
		t.Helper()
		d := newDocument()
		if _, err := d.apply(frame(
			entry("cam22", streamState("CLAUDE-COMMS", StreamStateStreaming, liveVideoFormat, liveAudioFormat)),
		)); err != nil {
			t.Fatal(err)
		}
		return d
	}

	t.Run("a deeper path than any observed", func(t *testing.T) {
		d := base(t)
		eff, err := d.apply(frame(deltaEntry("cam22", "/streams/video/format",
			`{"codec":"hevc","width":3840,"height":2160,"frame_rate":"25"}`)))
		if err != nil || eff.Merged != 1 {
			t.Fatalf("effect = %+v, err = %v", eff, err)
		}
		node, ok := d.streamNode("cam22")
		if !ok {
			t.Fatal("cam22 stopped being a router input")
		}
		if v := parseVideoFormat(node.Streams.Video.Format); v.Codec != "hevc" || v.Width != 3840 {
			t.Errorf("video = %+v, want the merged value", v)
		}
		// The sibling under streams must survive: this is a subtree merge,
		// not a subtree replacement.
		if len(node.Streams.Audio) != 1 {
			t.Errorf("audio = %+v, want the one stream the baseline had", node.Streams.Audio)
		}
		if node.StreamState != StreamStateStreaming {
			t.Errorf("stream_state = %q", node.StreamState)
		}
	})

	t.Run("a key the baseline does not have, at the top level", func(t *testing.T) {
		d := base(t)
		eff, _ := d.apply(frame(deltaEntry("cam22", "/error", `{"code":"E7","severity":"fatal"}`)))
		if eff.Merged != 1 {
			t.Fatalf("effect = %+v, want the new key merged", eff)
		}
		if got := string(nodeKeys(t, d, "cam22")["error"]); got != `{"code":"E7","severity":"fatal"}` {
			t.Errorf("error = %s", got)
		}
		if _, ok := d.streamNode("cam22"); !ok {
			t.Error("cam22 stopped being a router input when a new key appeared")
		}
	})

	t.Run("a chain of keys none of which exists", func(t *testing.T) {
		d := base(t)
		eff, _ := d.apply(frame(deltaEntry("cam22", "/a/b/c", `42`)))
		if eff.Merged != 1 {
			t.Fatalf("effect = %+v, want the chain created", eff)
		}
		if got := string(nodeKeys(t, d, "cam22")["a"]); got != `{"b":{"c":42}}` {
			t.Errorf("a = %s, want the intermediate objects created", got)
		}
	})

	t.Run("stream_state itself, which is what the lamps hang on", func(t *testing.T) {
		// NOBODY HAS EVER SEEN THIS FRAME. It is the shape a transition would
		// take if transitions are pushed as deltas at all, and the whole
		// question of whether resyncInterval can ever be deleted turns on it.
		d := base(t)
		eff, _ := d.apply(frame(deltaEntry("cam22", "/stream_state", `"stopped"`)))
		if eff.Merged != 1 {
			t.Fatalf("effect = %+v", eff)
		}
		node, ok := d.streamNode("cam22")
		if !ok {
			t.Fatal("cam22 stopped being a router input")
		}
		if node.StreamState != StreamStateStopped {
			t.Errorf("stream_state = %q, want the merged value", node.StreamState)
		}
		if node.DisplayName != "CLAUDE-COMMS" {
			t.Errorf("display_name = %q; the rest of the node must survive", node.DisplayName)
		}
	})

	t.Run("a JSON pointer escape", func(t *testing.T) {
		// RFC 6901's escapes, on a device whose node names contain spaces.
		// Never observed; costs two lines to get right and is unrecoverable
		// to get wrong, because "~1" would otherwise merge into a key nothing
		// ever reads.
		d := base(t)
		if eff, _ := d.apply(frame(deltaEntry("cam22", "/a~1b", `1`))); eff.Merged != 1 {
			t.Fatalf("effect = %+v", eff)
		}
		if _, ok := nodeKeys(t, d, "cam22")["a/b"]; !ok {
			t.Errorf("keys = %v, want the escaped %q", nodeKeys(t, d, "cam22"), "a/b")
		}
	})
}

func TestDocumentSkipsMalformedAndUnfollowablePaths(t *testing.T) {
	// video is the video format the node's baseline carries. It matters for
	// exactly one case: a STOPPED input reports format as JSON null, and a
	// null unmarshals into a Go map without error while leaving it nil — so
	// "descend through a null" is a branch that has to be written and tested
	// deliberately, or a path through a stopped node's format quietly merges
	// into a map that is then thrown away.
	cases := []struct {
		name  string
		path  string
		video string
		why   string
	}{
		{name: "empty", path: "", why: "an entry with no path at all: it is not a whole node and not a subtree either"},
		{name: "no leading slash", path: "statistics", why: "not a path this device has ever produced"},
		{name: "empty token in the middle", path: "/streams//video", why: "an empty reference token has no unambiguous meaning here"},
		{name: "trailing slash", path: "/statistics/", why: "same, at the end"},
		{name: "bare slashes", path: "//", why: "same again"},
		{name: "through a string", path: "/display_name/x", why: "display_name is a string; the path and the document disagree about the shape"},
		{name: "through an array", path: "/streams/audio/0/format", why: "RFC 6901 allows array indices; this device has never sent one"},
		{name: "through a scalar leaf", path: "/streams/video/format/codec/x", why: "format is an object here, but codec inside it is a string"},
		{name: "through a null", path: "/streams/video/format/codec", video: `null`, why: "a stopped input's format is null, which is not an empty object"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			video := c.video
			if video == "" {
				video = liveVideoFormat
			}
			d := newDocument()
			if _, err := d.apply(frame(
				entry("cam22", streamState("CLAUDE-COMMS", StreamStateStreaming, video, liveAudioFormat)),
			)); err != nil {
				t.Fatal(err)
			}
			before := append(json.RawMessage(nil), d.nodes["cam22"]...)

			eff, err := d.apply(frame(deltaEntry("cam22", c.path, `{"x":1}`)))
			if err != nil {
				t.Fatalf("a delta with a %s path failed the whole frame: %v", c.name, err)
			}
			if eff.Merged != 0 || eff.Skipped != 1 {
				t.Errorf("effect = %+v, want the delta skipped (%s)", eff, c.why)
			}
			if len(eff.Touched) != 0 {
				t.Errorf("Touched = %v, want nothing", eff.Touched)
			}
			if !bytes.Equal(d.nodes["cam22"], before) {
				t.Errorf("the node changed although the delta was skipped:\n%s", d.nodes["cam22"])
			}
			// The most important assertion of the eight: whatever else a path
			// this package does not understand does, it must never make the
			// delta's state stand in for the whole node.
			node, ok := d.streamNode("cam22")
			if !ok {
				t.Fatal("cam22 stopped being a router input because of a path this package refused")
			}
			if node.StreamState != StreamStateStreaming {
				t.Errorf("stream_state = %q", node.StreamState)
			}
		})
	}
}

func TestDocumentAWholeNodeEntryReplacesRatherThanMerges(t *testing.T) {
	// The counterpart to every merge test. An entry at "/" is the device
	// restating the node completely, so a key that has gone away must go away
	// — otherwise a stale subtree outlives the state that justified it.
	d := newDocument()
	if _, err := d.apply(frame(entry("cam22", `{"display_name":"OLD","stream_state":"streaming","gone":{"x":1}}`))); err != nil {
		t.Fatal(err)
	}
	if _, err := d.apply(frame(entry("cam22", `{"display_name":"NEW","stream_state":"stopped"}`))); err != nil {
		t.Fatal(err)
	}
	keys := nodeKeys(t, d, "cam22")
	if _, ok := keys["gone"]; ok {
		t.Error("a whole-node entry merged instead of replacing: a key the device dropped is still there")
	}
	node, ok := d.streamNode("cam22")
	if !ok {
		t.Fatal("cam22 is not a router input")
	}
	if node.DisplayName != "NEW" || node.StreamState != StreamStateStopped {
		t.Errorf("cam22 = %+v", node)
	}
}

func TestDocumentSurvivesEntriesItCannotRead(t *testing.T) {
	// One unreadable element must cost only itself: the measured frame has 36
	// entries and losing 35 good ones to a bad one would be the failure mode
	// parseFrame's contract exists to avoid.
	d := newDocument()
	eff, err := d.apply([]byte(`{"status":[` +
		`42,` +
		`{"node":"","path":"/","state":{"stream_state":"streaming"}},` +
		`{"node":"cam22","path":"/"},` +
		entry("cam22", streamState("CLAUDE-COMMS", StreamStateStreaming, liveVideoFormat, liveAudioFormat)) +
		`],"timestamp":1}`))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !eff.Snapshot {
		t.Error("the one good whole-node entry was lost")
	}
	if len(d.nodes) != 1 {
		t.Fatalf("document holds %d nodes, want the one readable entry: %v", len(d.nodes), d.nodes)
	}
	if _, ok := d.streamNode("cam22"); !ok {
		t.Error("cam22 was not stored")
	}
}

func TestDocumentRejectsAFrameThatIsNotSwitcherStatus(t *testing.T) {
	for _, payload := range []string{`{not valid json`, `{"status":"not an array"}`, `[]`, `{}`} {
		d := applySnapshot(t)
		if _, err := d.apply([]byte(payload)); err == nil {
			t.Errorf("apply(%s) returned no error", payload)
		}
		if len(d.nodes) != 36 {
			t.Errorf("apply(%s) disturbed the document", payload)
		}
	}
}

// ---------------------------------------------------------------------------
// parsePath on its own
// ---------------------------------------------------------------------------

func TestParsePath(t *testing.T) {
	cases := []struct {
		path   string
		want   []string
		wantOK bool
	}{
		// The four ever observed, plus the root.
		{"/", nil, true},
		{"/statistics", []string{"statistics"}, true},
		{"/levels", []string{"levels"}, true},
		{"/peak_levels", []string{"peak_levels"}, true},
		{"/peak_hold_levels", []string{"peak_hold_levels"}, true},
		// Never observed.
		{"/streams/video/format", []string{"streams", "video", "format"}, true},
		{"/stream_state", []string{"stream_state"}, true},
		{"/a~1b", []string{"a/b"}, true},
		{"/a~0b", []string{"a~b"}, true},
		{"/a~01b", []string{"a~1b"}, true}, // ~1 before ~0, per RFC 6901
		{"/ ", []string{" "}, true},
		// Refused.
		{"", nil, false},
		{"statistics", nil, false},
		{"//", nil, false},
		{"/a//b", nil, false},
		{"/a/", nil, false},
		{"~1", nil, false},
	}
	for _, c := range cases {
		got, ok := parsePath(c.path)
		if ok != c.wantOK {
			t.Errorf("parsePath(%q) ok = %v, want %v", c.path, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("parsePath(%q) = %v, want %v", c.path, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parsePath(%q) = %v, want %v", c.path, got, c.want)
				break
			}
		}
	}
}

// TestParsePathNeverYieldsAWholeNode is the guard from the previous change,
// restated against the new code. wholeNodePath is the ONLY path that means
// "this entry is the whole node"; nothing parsePath accepts may collapse to
// it, because a zero-token path is what mergeDelta refuses.
func TestParsePathNeverYieldsAWholeNode(t *testing.T) {
	for _, p := range []string{"/statistics", "/levels", "/peak_levels", "/peak_hold_levels",
		"/stream_state", "/streams/video/format", "/a~1b"} {
		tokens, ok := parsePath(p)
		if !ok || len(tokens) == 0 {
			t.Errorf("parsePath(%q) = %v, %v; a real subtree path must not read as the root", p, tokens, ok)
		}
		if (wireEntry{Node: "cam22", Path: p}).isWholeNode() {
			t.Errorf("%q passes isWholeNode", p)
		}
	}
	if tokens, ok := parsePath(wholeNodePath); !ok || len(tokens) != 0 {
		t.Errorf("parsePath(%q) = %v, %v; the root must yield no tokens", wholeNodePath, tokens, ok)
	}
}
