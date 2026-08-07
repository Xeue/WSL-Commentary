package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"wslcomms/internal/mixer"
)

// capturePath is the 84 KB frame captured from the live dev event on
// 2026-07-31 while this application was streaming into it.
//
// This test file READS it — it does not copy it, and it does not paraphrase
// it. That is the whole defence against this mock drifting again: the previous
// version was written from prose, the capture arrived later, internal/m2lx was
// rewritten against the capture and the mock was not, and nothing failed
// because the mock's tests decoded with the mock's own types. Comparing
// against the capture is the only check that can fail when the device and the
// mock disagree.
const capturePath = "../../internal/m2lx/testdata/switcher_status-live-2026-07-31.json"

// loadCapture reads the captured frame.
func loadCapture(t *testing.T) wireFrame {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(capturePath))
	if err != nil {
		t.Fatalf("reading the captured frame: %v", err)
	}
	var f wireFrame
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("the captured frame does not parse: %v", err)
	}
	return f
}

// mockSnapshot renders this mock's opening snapshot as bytes and as a decoded
// frame, going through JSON so what is asserted is the wire and not the Go
// types that produced it.
func mockSnapshot(t *testing.T, a *App) ([]byte, wireFrame) {
	t.Helper()
	b, err := json.Marshal(a.snapshotFrame())
	if err != nil {
		t.Fatalf("marshalling the snapshot: %v", err)
	}
	var f wireFrame
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("the mock's own snapshot does not parse: %v", err)
	}
	return b, f
}

func nodeNames(f wireFrame) []string {
	out := make([]string, 0, len(f.Status))
	for _, e := range f.Status {
		out = append(out, e.Node)
	}
	sort.Strings(out)
	return out
}

func keysOf(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("not a JSON object: %v", err)
	}
	out := make([]string, 0, len(obj))
	for k := range obj {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ===========================================================================
// The mock against the capture
// ===========================================================================

// TestSnapshotNodeSetMatchesTheCapture is the anti-drift check. If the device
// ever gains or loses a node and the capture is refreshed, this fails and the
// mock has to be brought along.
func TestSnapshotNodeSetMatchesTheCapture(t *testing.T) {
	a := newAuthTestApp(t)
	_, mock := mockSnapshot(t, a)
	capture := loadCapture(t)

	got, want := nodeNames(mock), nodeNames(capture)
	if !equalStrings(got, want) {
		t.Errorf("node set differs from the capture\n got: %v\nwant: %v", got, want)
	}
}

// TestSnapshotRouterInputStatesMatchTheCapture compares the state key set of
// every node that carries a stream_state, and the key set of the format
// objects underneath.
//
// The format objects are the reason this test exists at all. The old mock sent
// them as SINGLE STRINGS, because docs/architecture.md and
// docs/test-results.md said so; the capture shows structured objects with
// frame_rate as a string and width and height as numbers. Comparing key sets
// against the capture catches that class of mistake without pinning values
// that legitimately vary.
func TestSnapshotRouterInputStatesMatchTheCapture(t *testing.T) {
	a := newAuthTestApp(t)
	_, mock := mockSnapshot(t, a)
	capture := loadCapture(t)

	// cam22 was the commentary input and was streaming; cam2 was stopped. The
	// two together cover both format shapes: the object and the JSON null.
	for _, node := range []string{"cam22", "cam2"} {
		want, ok := capture.entryFor(node)
		if !ok {
			t.Fatalf("the capture has no %q entry", node)
		}
		got, ok := mock.entryFor(node)
		if !ok {
			t.Fatalf("the mock's snapshot has no %q entry", node)
		}
		if g, w := keysOf(t, got.State), keysOf(t, want.State); !equalStrings(g, w) {
			t.Errorf("%s state keys\n got: %v\nwant: %v", node, g, w)
		}
	}

	// The healthy format objects, which no stopped node in the capture can
	// show. cam22's are the ones this application produced.
	want, _ := capture.entryFor("cam22")
	captured := decodeInput(t, want.State)

	video, err := json.Marshal(measuredVideoFormat())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if g, w := keysOf(t, video), keysOf(t, captured.Streams.Video.Format); !equalStrings(g, w) {
		t.Errorf("video format keys\n got: %v\nwant: %v", g, w)
	}
	audio, err := json.Marshal(measuredAudioFormat())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(captured.Streams.Audio) != 1 {
		t.Fatalf("the capture's cam22 has %d audio streams, want 1", len(captured.Streams.Audio))
	}
	if g, w := keysOf(t, audio), keysOf(t, captured.Streams.Audio[0].Format); !equalStrings(g, w) {
		t.Errorf("audio format keys\n got: %v\nwant: %v", g, w)
	}
}

// TestSnapshotMixerNodeMatchesTheCapture covers the node internal/mixer parses.
func TestSnapshotMixerNodeMatchesTheCapture(t *testing.T) {
	a := newAuthTestApp(t)
	_, mock := mockSnapshot(t, a)
	capture := loadCapture(t)

	want, ok := capture.entryFor(mixerNodeName)
	if !ok {
		t.Fatalf("the capture has no %q entry", mixerNodeName)
	}
	got, ok := mock.entryFor(mixerNodeName)
	if !ok {
		t.Fatalf("the mock's snapshot has no %q entry", mixerNodeName)
	}
	if g, w := keysOf(t, got.State), keysOf(t, want.State); !equalStrings(g, w) {
		t.Errorf("%s state keys\n got: %v\nwant: %v", mixerNodeName, g, w)
	}

	// The sub-maps whose SIZES are the measured facts internal/mixer is built
	// around: 27 inputs, 54 strips in the routing matrix, 7 buses, and 34
	// metered keys — i.e. more than half the strips are legitimately not
	// metered at all.
	for _, c := range []struct {
		key  string
		size int
	}{
		{"inputs", 27},
		{"matrix", 54},
		{"outputs", 7},
		{"levels", 34},
		{"peak_levels", 34},
		{"peak_hold_levels", 34},
		{"fader", 57},
		{"effect", 54},
	} {
		var state map[string]json.RawMessage
		if err := json.Unmarshal(got.State, &state); err != nil {
			t.Fatalf("mixer state: %v", err)
		}
		if n := len(keysOf(t, state[c.key])); n != c.size {
			t.Errorf("mixer %s has %d keys, want the measured %d", c.key, n, c.size)
		}
	}
}

// ===========================================================================
// The inventory
// ===========================================================================

// TestInventorySplitsInputsFromEverythingElse pins the rule internal/m2lx
// applies: stream_state, not display_name, is what makes a node a router
// input. replay1, vtr1 and vtr2 are the counter-examples that make it
// testable, and a mock without them would let a parser keying off
// display_name pass.
func TestInventorySplitsInputsFromEverythingElse(t *testing.T) {
	a := newAuthTestApp(t)
	_, mock := mockSnapshot(t, a)

	inputs, named := 0, 0
	for _, e := range mock.Status {
		var state struct {
			DisplayName string          `json:"display_name"`
			StreamState string          `json:"stream_state"`
			Streams     json.RawMessage `json:"streams"`
		}
		if err := json.Unmarshal(e.State, &state); err != nil {
			t.Fatalf("node %q state: %v", e.Node, err)
		}
		if state.StreamState != "" {
			inputs++
			if len(state.Streams) == 0 {
				t.Errorf("node %q carries a stream_state but no streams", e.Node)
			}
			continue
		}
		if state.DisplayName != "" {
			named++
			if len(state.Streams) != 0 {
				t.Errorf("node %q has no stream_state but does have streams", e.Node)
			}
		}
	}
	if inputs != 24 {
		t.Errorf("%d nodes carry a stream_state, want the measured 24", inputs)
	}
	if named != len(mixerOnlyInputs) {
		t.Errorf("%d nodes carry a display_name and no stream_state, want %d "+
			"(replay1, vtr1, vtr2 — the counter-examples to \"display_name makes it an input\")",
			named, len(mixerOnlyInputs))
	}
}

// TestStatusKeyOutsideTheInventoryIsNotPatchedOver: the mock serves the
// measured inventory and lets internal/m2lx report a bad status key itself.
// That is how StatusKeyNotFoundError, in both its variants, is reachable at
// Gate A.
func TestStatusKeyOutsideTheInventoryIsNotPatchedOver(t *testing.T) {
	for _, key := range []string{"cam99", "mixer", "advanced_audio_mixer"} {
		a := newAuthTestApp(t)
		a.opts.StatusKey = key
		if _, ok := a.statusKeyInput(); ok {
			t.Errorf("%q resolved to a router input; it must not", key)
		}
		_, mock := mockSnapshot(t, a)
		if len(mock.Status) != 36 {
			t.Errorf("with -status-key=%q the snapshot carried %d entries, want the full 36 — "+
				"the mock must not invent a node to match the key", key, len(mock.Status))
		}
	}
}

// ===========================================================================
// internal/mixer, against this mock's snapshot
// ===========================================================================

// TestMixerParsesTheMockSnapshotCleanly is the check that makes the mixer
// drawer usable at Gate A.
//
// A frame internal/mixer cannot parse yields a Snapshot with no strips, which
// that package's own documentation calls the most dangerous false statement
// the drawer can make: "nothing is routed to the clean feed". So the mock has
// to serve a mixer node that parses, with no CRITICAL warnings — a critical
// warning is by definition a field whose loss could produce a false safety
// claim.
func TestMixerParsesTheMockSnapshotCleanly(t *testing.T) {
	a := newAuthTestApp(t)
	raw, _ := mockSnapshot(t, a)

	snap, warnings, err := mixer.ParseSnapshotWithWarnings(raw)
	if err != nil {
		t.Fatalf("internal/mixer cannot parse this mock's snapshot: %v", err)
	}
	for _, w := range warnings {
		if w.Critical {
			t.Errorf("critical parse warning: %s", w)
		}
	}
	if len(snap.Strips) != 54 {
		t.Errorf("%d strips, want the measured 54", len(snap.Strips))
	}
	if len(snap.Buses) != len(mixer.AllBuses) {
		t.Errorf("%d buses, want %d", len(snap.Buses), len(mixer.AllBuses))
	}

	// The headline fact. Every camera strip defaults to ["master","aux1",
	// "aux2"], and aux1 IS the clean feed — so commentary sits in the client's
	// clean feed from the factory default with nothing in Sony's UI saying so.
	// The mock serves that default because it is what the drawer exists to
	// expose; if this ever stops being true, the drawer has nothing to find.
	strip, ok := snap.Strip(a.opts.StatusKey + "-1")
	if !ok {
		t.Fatalf("no strip %q in the parsed snapshot", a.opts.StatusKey+"-1")
	}
	inClean := false
	for _, b := range strip.Outputs {
		if b == mixer.BusAux1 {
			inClean = true
		}
	}
	if !inClean {
		t.Errorf("strip %q routes to %v, want the measured default including %s (the clean feed)",
			strip.Name, strip.Outputs, mixer.BusAux1)
	}

	// The display-name join: the strip's own display_name is only "cam7-1",
	// and the name a human recognises comes from the per-input node.
	want, _ := lookupRouterInput(a.opts.StatusKey)
	if strip.DisplayName != want.display {
		t.Errorf("strip display name = %q, want %q joined from the %q node",
			strip.DisplayName, want.display, a.opts.StatusKey)
	}
}

// ===========================================================================
// statistics
// ===========================================================================

// TestStatisticsFreezeRatherThanZero reproduces, on purpose, the property that
// makes statistics.bitrate unusable: it holds its last value after the feed
// dies instead of falling to zero.
//
// internal/m2lx deliberately never reads the field and says so in two places.
// This is what stands behind those comments: without a mock that reproduces
// the freeze, the warning is folklore and somebody will eventually "fix" the
// parser by reading the field.
func TestStatisticsFreezeRatherThanZero(t *testing.T) {
	a := newAuthTestApp(t)

	// Seed a session's worth of counters, as a connected peer would.
	a.doc.mu.Lock()
	a.doc.stats = inputStatistics{Bitrate: 6932.9, PacketCount: 46097, PacketRate: 4609.7}
	a.doc.mu.Unlock()

	// Nothing is connected — srtPeerConnected is false — so this is the "feed
	// is dead" reading.
	got := a.inputStatistics()
	if got.Bitrate == 0 || got.PacketCount == 0 {
		t.Fatalf("statistics zeroed on a dead input: %+v\n"+
			"they must FREEZE, or the mock cannot reproduce the reason internal/m2lx refuses to read them",
			got)
	}
	if got.Bitrate != 6932.9 || got.PacketCount != 46097 {
		t.Errorf("statistics = %+v, want the last live values held", got)
	}
}

// TestStatisticsUnitsAreInternallyConsistent checks the relationship the
// capture shows: cam1 reported bitrate 6932.9888 with packet_rate 4609.7, and
// 4609.7 * 188 * 8 / 1000 is 6932.99. Getting the units wrong would make the
// mock's numbers plausible and meaningless.
func TestStatisticsUnitsAreInternallyConsistent(t *testing.T) {
	const bps = 6932988.8
	kbit := round1(bps / 1000)
	packets := round1(bps / (tsPacketSize * 8))
	if kbit != 6933.0 {
		t.Errorf("bitrate = %v kbit/s, want 6933 for %v bit/s", kbit, bps)
	}
	if packets != 4609.7 {
		t.Errorf("packet_rate = %v, want the measured 4609.7 for %v bit/s", packets, bps)
	}
}

// ===========================================================================
// The meters
// ===========================================================================

// TestMeterMapCoversEveryMeteredKeyAndMoves. A frozen meter map would make a
// socket running at 21 frames a second indistinguishable from one that has
// stopped, and would hide any bug in the drawer's meter rendering behind a
// still picture.
func TestMeterMapCoversEveryMeteredKeyAndMoves(t *testing.T) {
	a := newAuthTestApp(t)

	first := a.meterMap(pathLevels)
	if len(first) != 34 {
		t.Errorf("%d metered keys, want the measured 34", len(first))
	}
	for _, k := range meterKeys() {
		if _, ok := first[k]; !ok {
			t.Errorf("meter map is missing %q", k)
		}
	}
	// Silence is -100 dBFS, not 0 — a zero here would draw every channel on
	// the surface pinned at full scale.
	if first["cam1-1"] != [2]float64{silenceDB, silenceDB} {
		t.Errorf("an idle strip meters %v, want %v", first["cam1-1"], silenceDB)
	}

	// peak_hold rides above peak, which rides above the instantaneous level.
	// Compared at a fixed phase, since the sweep advances with the frames.
	lv := a.meterMap(pathLevels)["master"][0]
	pk := a.meterMap(pathPeakLevels)["master"][0]
	hold := a.meterMap(pathPeakHoldLevels)["master"][0]
	if lv != silenceDB && !(hold > pk && pk > lv) {
		t.Errorf("levels=%v peak=%v hold=%v, want hold > peak > level", lv, pk, hold)
	}
}
