// state_test.go — WP-M2.
//
// Two halves. The first parses the REAL captured frame
// (internal/m2lx/testdata/switcher_status-live-2026-07-31.json) and asserts
// facts that were read out of it by hand, so the parser is pinned to the device
// rather than to its own opinion. The second feeds synthetic frames that the
// live capture cannot exercise — a missing mixer node, a mangled matrix entry,
// a strip in one map and not the other — because those are the cases where a
// parser is most likely to invent a reassuring answer.
//
// Every assertion about the live frame below was verified against the file
// before the parser was written.
package mixer

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// liveFrameFile is the captured frame. It is owned by internal/m2lx; these
// tests skip rather than fail if it moves, matching the convention already set
// in mixer_test.go.
const liveFrameFile = "../m2lx/testdata/switcher_status-live-2026-07-31.json"

// loadLiveFrame reads the capture, or skips.
func loadLiveFrame(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(liveFrameFile))
	if err != nil {
		t.Skipf("live frame not available (%v)", err)
	}
	return raw
}

// parseLive parses the capture and fails on any error or warning. The live
// frame is well-formed, so a warning here means the parser has become
// suspicious of something real.
func parseLive(t *testing.T) Snapshot {
	t.Helper()
	snap, warnings, err := ParseSnapshotWithWarnings(loadLiveFrame(t))
	if err != nil {
		t.Fatalf("ParseSnapshotWithWarnings: %v", err)
	}
	for _, w := range warnings {
		t.Errorf("unexpected warning on the live frame: %s", w)
	}
	return snap
}

// closeTo compares dBFS values with a tolerance well below anything audible.
func closeTo(got, want float64) bool { return math.Abs(got-want) < 0.001 }

// =========================== the live capture ===============================

// TestLiveFrameStripCount pins the number of strips and the number of distinct
// parent inputs.
//
// THESE TWO NUMBERS ARE DIFFERENT AND CONFUSING THEM IS A REAL RISK: the frame
// has 27 INPUTS, each carrying TWO strips, for 54 strips. state.matrix has 54
// keys and state.inputs has 27, and the WP-M2 brief asserted "27 strips found"
// — the input count. A parser that returned 27 strips would be hiding one
// channel of every input, including cam22-2, from a surface whose job is to
// show what is in the clean feed.
func TestLiveFrameStripCount(t *testing.T) {
	snap := parseLive(t)

	if got, want := len(snap.Strips), 54; got != want {
		t.Errorf("len(Strips) = %d, want %d", got, want)
	}
	inputs := map[string]bool{}
	for _, s := range snap.Strips {
		inputs[s.Input] = true
	}
	if got, want := len(inputs), 27; got != want {
		t.Errorf("distinct inputs = %d, want %d", got, want)
	}
}

// TestLiveFrameNonStripKeysAreNotStrips is the structural-detection test.
//
// Every input object carries "assign_list" (an array of arrays) and
// "channel_count" (a number) beside its two strip objects. Either promoted to a
// strip would put a phantom channel on the mixer surface — one with no routing,
// which would render as "not in the clean feed" and pad out the count that an
// operator uses to sanity-check the drawer against the desk.
func TestLiveFrameNonStripKeysAreNotStrips(t *testing.T) {
	snap := parseLive(t)

	for _, s := range snap.Strips {
		switch s.Name {
		case "assign_list", "channel_count":
			t.Errorf("non-strip key %q was parsed as a strip", s.Name)
		}
		if s.Name == "" {
			t.Error("a strip parsed with an empty name")
		}
	}
	if _, ok := snap.Strip("assign_list"); ok {
		t.Error(`Strip("assign_list") found; it is a key of an input, not a strip`)
	}
	if _, ok := snap.Strip("channel_count"); ok {
		t.Error(`Strip("channel_count") found; it is a key of an input, not a strip`)
	}
}

// TestLiveFrameCam22_1 is the headline case: the commentary strip.
//
// cam22 is named "CLAUDE-COMMS" by the operator, and cam22-1 is routed
// ["master","aux1","aux2"] — the untouched default, which includes aux1, the
// CLN output. This single row is the reason the drawer exists, so every field
// of it is pinned.
func TestLiveFrameCam22_1(t *testing.T) {
	snap := parseLive(t)

	s, ok := snap.Strip("cam22-1")
	if !ok {
		t.Fatal(`Strip("cam22-1") not found`)
	}
	if s.Input != "cam22" {
		t.Errorf("Input = %q, want %q", s.Input, "cam22")
	}
	// The join that makes the matrix readable: the name comes from the "cam22"
	// STATUS NODE, not from the mixer node, where the strip's own display_name
	// is only "cam22-1" again.
	if s.DisplayName != "CLAUDE-COMMS" {
		t.Errorf("DisplayName = %q, want %q (the per-input status node's display_name)", s.DisplayName, "CLAUDE-COMMS")
	}
	if !s.Muted {
		t.Error("Muted = false, want true")
	}
	if got, want := s.Outputs, []Bus{BusMaster, BusAux1, BusAux2}; !reflect.DeepEqual(got, want) {
		t.Errorf("Outputs = %v, want %v", got, want)
	}
	if len(s.PFLOutputs) != 0 {
		t.Errorf("PFLOutputs = %v, want empty", s.PFLOutputs)
	}
	if s.Follow {
		t.Error("Follow = true, want false")
	}
	if got, want := s.FollowSources, []string{"cam22"}; !reflect.DeepEqual(got, want) {
		t.Errorf("FollowSources = %v, want %v", got, want)
	}
	if s.SubChMode != "ST_W" {
		t.Errorf("SubChMode = %q, want %q", s.SubChMode, "ST_W")
	}
	if !s.Metered {
		t.Fatal("Metered = false, want true")
	}
	if !closeTo(s.PeakHold[0], -52.78746) || !closeTo(s.PeakHold[1], -52.64630) {
		t.Errorf("PeakHold = %v, want about [-52.787 -52.646]", s.PeakHold)
	}
	if !closeTo(s.Level[0], -87.69414) || !closeTo(s.Level[1], -87.65318) {
		t.Errorf("Level = %v, want about [-87.694 -87.653]", s.Level)
	}
	if !closeTo(s.Fader[0], -1.574803) || !closeTo(s.Fader[1], -1.574803) {
		t.Errorf("Fader = %v, want about [-1.5748 -1.5748] dB", s.Fader)
	}
	if got, want := s.FaderEnabled, [2]bool{true, true}; got != want {
		t.Errorf("FaderEnabled = %v, want %v", got, want)
	}
}

// TestLiveFrameCam22_2 covers the second strip of the same input, which differs
// from the first in the two ways most likely to be papered over.
func TestLiveFrameCam22_2(t *testing.T) {
	snap := parseLive(t)

	s, ok := snap.Strip("cam22-2")
	if !ok {
		t.Fatal(`Strip("cam22-2") not found`)
	}
	if s.DisplayName != "CLAUDE-COMMS" {
		t.Errorf("DisplayName = %q, want %q; both strips of an input share its name", s.DisplayName, "CLAUDE-COMMS")
	}
	// The EFFECTIVE mode, not the requested one. The frame says
	// sub_ch_mode "MONO" with sub_ch_mode_set "ST_W"; reporting the latter
	// would tell the operator this channel is stereo when the mixer is
	// summing it.
	if s.SubChMode != "MONO" {
		t.Errorf("SubChMode = %q, want %q (sub_ch_mode, not sub_ch_mode_set)", s.SubChMode, "MONO")
	}
	// This strip is routed but NOT metered, and that is normal: the frame
	// meters 34 keys, none of them a "-2" strip. Absent must not become 0.0
	// dBFS, which is full scale.
	if s.Metered {
		t.Error("Metered = true, want false; no -2 strip is metered in this frame")
	}
	if s.Level != [2]float64{} || s.PeakHold != [2]float64{} {
		t.Errorf("unmetered strip carries Level %v PeakHold %v, want zero values guarded by Metered", s.Level, s.PeakHold)
	}
}

// TestLiveFrameMeterCoverage pins which strips are metered, because the count
// is the evidence for Strip.Metered existing at all.
func TestLiveFrameMeterCoverage(t *testing.T) {
	snap := parseLive(t)

	metered := 0
	for _, s := range snap.Strips {
		if s.Metered {
			metered++
			if !strings.HasSuffix(s.Name, "-1") {
				t.Errorf("strip %q is metered; only -1 strips are metered in this frame", s.Name)
			}
		}
	}
	if want := 27; metered != want {
		t.Errorf("metered strips = %d, want %d (one per input)", metered, want)
	}
}

// TestLiveFrameDisplayNameJoin covers the join across several inputs, including
// one whose name contains a space and one the operator has not renamed.
func TestLiveFrameDisplayNameJoin(t *testing.T) {
	snap := parseLive(t)

	tests := []struct {
		strip     string
		wantInput string
		wantName  string
	}{
		{"cam22-1", "cam22", "CLAUDE-COMMS"},
		{"cam21-2", "cam21", "CLAUDE-FX"},
		{"cam20-1", "cam20", "CLAUDE-TEST-SRT"},
		{"cam10-1", "cam10", "REPLAY 2 DIRTY"},
		// An input name containing a space, which also rules out any
		// whitespace-based split of the strip name.
		{"MIC 3-1", "MIC 3", "CLAUDE-TEST-MIC"},
		{"MIC 1-2", "MIC 1", "MIC 1"},
		// Never renamed by the operator; the device's own default.
		{"cam23-1", "cam23", "Input 23"},
		{"replay1-2", "replay1", "Replay"},
		{"vtr1-1", "vtr1", "Clip Player 1"},
	}
	for _, tt := range tests {
		t.Run(tt.strip, func(t *testing.T) {
			s, ok := snap.Strip(tt.strip)
			if !ok {
				t.Fatalf("Strip(%q) not found", tt.strip)
			}
			if s.Input != tt.wantInput {
				t.Errorf("Input = %q, want %q", s.Input, tt.wantInput)
			}
			if s.DisplayName != tt.wantName {
				t.Errorf("DisplayName = %q, want %q", s.DisplayName, tt.wantName)
			}
		})
	}
}

// TestLiveFrameNoStripIsNamedAfterItself guards the whole point of the join. If
// the parser ever falls back to the strip's own display_name for an input that
// HAS a status node, every row would read "cam22-1" and the drawer would be
// back to camera numbers.
func TestLiveFrameNoStripIsNamedAfterItself(t *testing.T) {
	snap := parseLive(t)

	for _, s := range snap.Strips {
		if s.DisplayName == s.Name {
			t.Errorf("strip %q has DisplayName %q; the per-input node's name was not joined", s.Name, s.DisplayName)
		}
	}
}

// TestLiveFrameBuses pins all seven buses.
func TestLiveFrameBuses(t *testing.T) {
	snap := parseLive(t)

	if got, want := len(snap.Buses), len(AllBuses); got != want {
		t.Fatalf("len(Buses) = %d, want %d", got, want)
	}
	for i, b := range snap.Buses {
		if b.Name != AllBuses[i] {
			t.Errorf("Buses[%d] = %q, want %q (AllBuses order)", i, b.Name, AllBuses[i])
		}
		if b.Muted {
			t.Errorf("bus %s: Muted = true, want false", BusLabel(b.Name))
		}
		if b.ChannelCount != 2 {
			t.Errorf("bus %s: ChannelCount = %d, want 2", BusLabel(b.Name), b.ChannelCount)
		}
		if !b.Metered {
			t.Errorf("bus %s: Metered = false, want true; all seven buses are metered in this frame", BusLabel(b.Name))
		}
	}
}

// TestLiveFrameBusDetail pins the per-bus values that differ, including the
// fader asymmetry that BusState.FaderPresent exists for.
func TestLiveFrameBusDetail(t *testing.T) {
	snap := parseLive(t)

	byName := map[Bus]BusState{}
	for _, b := range snap.Buses {
		byName[b.Name] = b
	}

	tests := []struct {
		bus              Bus
		wantPeak         [2]float64
		wantFader        float64
		wantFaderPresent bool
	}{
		// Digital silence on a bus reads -100 in peak_hold_levels.
		{BusMaster, [2]float64{-100, -100}, 0, true},
		{BusAux1, [2]float64{-100, -100}, 1, true},
		{BusAux2, [2]float64{-100, -100}, 1, true},
		// MEASURED: state.fader carries master, aux1 and aux2 only. The four
		// monitor buses have NO fader entry, and must report absence rather
		// than a zero that reads as unity gain.
		{BusMon1, [2]float64{-100, -100}, 0, false},
		{BusMon2, [2]float64{-100, -100}, 0, false},
		{BusMon3, [2]float64{-100, -100}, 0, false},
		{BusMon4, [2]float64{-100, -100}, 0, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.bus), func(t *testing.T) {
			b, ok := byName[tt.bus]
			if !ok {
				t.Fatalf("bus %q missing from the snapshot", tt.bus)
			}
			if !closeTo(b.PeakHold[0], tt.wantPeak[0]) || !closeTo(b.PeakHold[1], tt.wantPeak[1]) {
				t.Errorf("PeakHold = %v, want %v", b.PeakHold, tt.wantPeak)
			}
			if b.FaderPresent != tt.wantFaderPresent {
				t.Errorf("FaderPresent = %v, want %v", b.FaderPresent, tt.wantFaderPresent)
			}
			if b.FaderPresent && b.Fader != tt.wantFader {
				t.Errorf("Fader = %v, want %v", b.Fader, tt.wantFader)
			}
		})
	}
	// Bus silence is not strip silence: -99.99999 against the strips' -100.0.
	// Anything testing for silence must compare against SilenceDBFS rather
	// than for equality with it.
	master := byName[BusMaster]
	if !closeTo(master.Level[0], -99.99999237060547) {
		t.Errorf("master Level[0] = %v, want about -99.99999237", master.Level[0])
	}
	if master.Level[0] == SilenceDBFS {
		t.Error("master Level[0] equals SilenceDBFS exactly; the fixture value is -99.99999237, which is why silence must be a <= test")
	}
}

// TestLiveFrameCleanFeedCensus is the assertion the drawer is built to make.
//
// It records the routing distribution of the live event: 48 of 54 strips carry
// the untouched default ["master","aux1","aux2"] and are therefore in the CLN
// feed, and the six MIC strips are routed to master plus a monitor bus. It also
// names the one strip that is both routed to aux1 AND unmuted.
func TestLiveFrameCleanFeedCensus(t *testing.T) {
	snap := parseLive(t)

	inCleanFeed, defaultRouted := 0, 0
	var unmutedInCleanFeed []string
	for _, s := range snap.Strips {
		routed := false
		for _, b := range s.Outputs {
			if b == BusAux1 {
				routed = true
			}
		}
		if !routed {
			continue
		}
		inCleanFeed++
		if reflect.DeepEqual(s.Outputs, []Bus{BusMaster, BusAux1, BusAux2}) {
			defaultRouted++
		}
		if !s.Muted {
			unmutedInCleanFeed = append(unmutedInCleanFeed, s.Name)
		}
	}
	if want := 48; inCleanFeed != want {
		t.Errorf("strips routed to aux1 (CLN) = %d, want %d", inCleanFeed, want)
	}
	if want := 48; defaultRouted != want {
		t.Errorf("strips with the untouched default routing = %d, want %d", defaultRouted, want)
	}
	// MEASURED: cam10-1 ("REPLAY 2 DIRTY") is the only unmuted strip in the
	// whole frame, and it is in the clean feed.
	if want := []string{"cam10-1"}; !reflect.DeepEqual(unmutedInCleanFeed, want) {
		t.Errorf("unmuted strips in the clean feed = %v, want %v", unmutedInCleanFeed, want)
	}
}

// TestLiveFrameTakenAt pins the timestamp unit. Epoch MILLISECONDS: read as
// seconds or nanoseconds this frame lands in 1970 and every staleness check
// downstream is permanently wrong.
func TestLiveFrameTakenAt(t *testing.T) {
	snap := parseLive(t)

	want := time.UnixMilli(1785522083212).UTC()
	if !snap.TakenAt.Equal(want) {
		t.Errorf("TakenAt = %v, want %v", snap.TakenAt, want)
	}
	if snap.TakenAt.Year() != 2026 {
		t.Errorf("TakenAt year = %d, want 2026; the timestamp is epoch milliseconds", snap.TakenAt.Year())
	}
}

// TestLiveFrameOrderingIsDeterministic parses the same bytes twice and requires
// identical output. The drawer refreshes at about 1 Hz off Go maps, whose
// iteration order is randomised per run, so an unsorted result would reshuffle
// the surface under the operator's cursor several times a minute.
func TestLiveFrameOrderingIsDeterministic(t *testing.T) {
	raw := loadLiveFrame(t)

	first, _, err := ParseSnapshotWithWarnings(raw)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	for i := range 5 {
		next, _, err := ParseSnapshotWithWarnings(raw)
		if err != nil {
			t.Fatalf("parse %d: %v", i, err)
		}
		if !reflect.DeepEqual(first, next) {
			t.Fatalf("parse %d differs from the first parse; output is not deterministic", i)
		}
	}
	// Sorted by wire name, which is also what Snapshot.Strip's search needs.
	for i := 1; i < len(first.Strips); i++ {
		if first.Strips[i-1].Name >= first.Strips[i].Name {
			t.Fatalf("Strips not sorted by Name: %q then %q", first.Strips[i-1].Name, first.Strips[i].Name)
		}
	}
	if first.Strips[0].Name != "MIC 1-1" {
		t.Errorf("first strip = %q, want %q", first.Strips[0].Name, "MIC 1-1")
	}
}

// TestLiveFrameParseSnapshotContract checks the contract entry point declared
// in mixer.go, not just the warning-rich one. If the delegation in mixer.go is
// ever reverted to its stub, this test fails loudly rather than the drawer
// going quietly blank.
func TestLiveFrameParseSnapshotContract(t *testing.T) {
	snap, err := ParseSnapshot(loadLiveFrame(t))
	if err != nil {
		t.Fatalf("ParseSnapshot: %v", err)
	}
	if len(snap.Strips) != 54 || len(snap.Buses) != 7 {
		t.Fatalf("ParseSnapshot returned %d strips and %d buses, want 54 and 7", len(snap.Strips), len(snap.Buses))
	}
}

// TestSnapshotStripLookup covers the accessor, including the names that sort
// adjacent to a miss.
func TestSnapshotStripLookup(t *testing.T) {
	snap := parseLive(t)

	tests := []struct {
		name      string
		want      bool
		wantInput string
	}{
		{"cam22-1", true, "cam22"},
		{"MIC 1-1", true, "MIC 1"},   // first in sort order
		{"vtr2-2", true, "vtr2"},     // last in sort order
		{"cam22-3", false, ""},       // between two real strips
		{"", false, ""},              // sorts before everything
		{"zzz", false, ""},           // sorts after everything
		{"CAM22-1", false, ""},       // lookup is case sensitive
		{"assign_list", false, ""},   // not a strip
		{"channel_count", false, ""}, // not a strip
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ok := snap.Strip(tt.name)
			if ok != tt.want {
				t.Fatalf("Strip(%q) found = %v, want %v", tt.name, ok, tt.want)
			}
			if ok && s.Input != tt.wantInput {
				t.Errorf("Input = %q, want %q", s.Input, tt.wantInput)
			}
			if !ok && s.Name != "" {
				t.Errorf("miss returned a non-zero Strip: %+v", s)
			}
		})
	}
}

// ============================ synthetic frames ==============================

// buildFrame wraps a mixer state object and any extra status nodes into a whole
// switcher_status frame.
func buildFrame(mixerState string, extraNodes ...string) []byte {
	nodes := []string{fmt.Sprintf(`{"node":%q,"path":"/","state":%s}`, mixerNodeName, mixerState)}
	nodes = append(nodes, extraNodes...)
	return []byte(fmt.Sprintf(`{"status":[%s],"timestamp":1785522083212}`, strings.Join(nodes, ",")))
}

// inputNode renders a per-input status node carrying an operator-facing name.
func inputNode(name, display string) string {
	return fmt.Sprintf(`{"node":%q,"path":"/","state":{"display_name":%q}}`, name, display)
}

// stripState is a minimal but complete mixer state with one input, one strip
// and the seven buses, used as the base for the degradation cases.
const minimalState = `{
	"inputs":{"cam1":{"assign_list":[[1,2]],"channel_count":2,
		"cam1-1":{"display_name":"cam1-1","follow":false,"follow_sources":["cam1"],
			"muted":true,"sub_ch_mode":"ST_W","sub_ch_mode_set":"ST_W"}}},
	"matrix":{"cam1-1":{"outputs":["master","aux1","aux2"],"pfl_outputs":[]}},
	"outputs":{"master":{"channel_count":2,"muted":false},"aux1":{"channel_count":2,"muted":false},
		"aux2":{"channel_count":2,"muted":false},"mon1":{"channel_count":2,"muted":false},
		"mon2":{"channel_count":2,"muted":false},"mon3":{"channel_count":2,"muted":false},
		"mon4":{"channel_count":2,"muted":false}},
	"levels":{"cam1-1":[-20,-21]},
	"peak_hold_levels":{"cam1-1":[-10,-11]},
	"fader":{"cam1-1":{"ch_fader":{"enabled":[true,true],"gain":[-3,-3]}}}
}`

// objectNeighboursState is minimalState with one addition that the live frame
// cannot supply: non-strip keys inside an input whose values ARE JSON objects.
//
// WHY IT HAD TO BE SYNTHESISED. TestLiveFrameNonStripKeysAreNotStrips looks
// like it covers looksLikeStrip and does not. Every non-strip key in the real
// frame is "assign_list" (an array) or "channel_count" (a number), and
// decodeObject rejects both before the muted/display_name test is ever
// consulted. So the live frame stays green with looksLikeStrip's body replaced
// by `return true` — the detector was structurally untested by the fixture
// that appeared to test it.
//
// The three neighbours below close that off, one per way the detector can be
// weakened:
//
//	"eq"       an object with NEITHER marker key — passes decodeObject, so
//	           only the marker test rejects it. Catches `return true`.
//	"agc"      an object with muted but no display_name. Catches hasMuted ||
//	           hasName, and mirrors a bus entry, which really does have that
//	           shape.
//	"settings" an object with display_name but no muted. Catches the same
//	           mutation from the other side, and mirrors the per-input status
//	           node, whose shape this genuinely is.
//
// A promoted neighbour is not a cosmetic bug: it appears on the surface as a
// channel with no routing entry, which the drawer renders as NOT in the clean
// feed, and it inflates the strip count an operator uses to check the drawer
// against the desk.
const objectNeighboursState = `{
	"inputs":{"cam1":{"assign_list":[[1,2]],"channel_count":2,
		"eq":{"enabled":true,"bands":[{"freq":100,"gain":0}]},
		"agc":{"muted":false,"channel_count":2},
		"settings":{"display_name":"CLAUDE-COMMS","phantom":true},
		"cam1-1":{"display_name":"cam1-1","follow":false,"follow_sources":["cam1"],
			"muted":true,"sub_ch_mode":"ST_W","sub_ch_mode_set":"ST_W"}}},
	"matrix":{"cam1-1":{"outputs":["master","aux1","aux2"],"pfl_outputs":[]}},
	"outputs":{"master":{"channel_count":2,"muted":false},"aux1":{"channel_count":2,"muted":false},
		"aux2":{"channel_count":2,"muted":false},"mon1":{"channel_count":2,"muted":false},
		"mon2":{"channel_count":2,"muted":false},"mon3":{"channel_count":2,"muted":false},
		"mon4":{"channel_count":2,"muted":false}},
	"levels":{"cam1-1":[-20,-21]},
	"peak_hold_levels":{"cam1-1":[-10,-11]},
	"fader":{"cam1-1":{"ch_fader":{"enabled":[true,true],"gain":[-3,-3]}}}
}`

// TestObjectNeighboursOfAStripAreNotStrips exercises looksLikeStrip through
// the whole parse, against neighbours the live frame does not contain.
//
// See objectNeighboursState for why this fixture exists and what each of the
// three neighbours catches.
func TestObjectNeighboursOfAStripAreNotStrips(t *testing.T) {
	snap, warnings, err := ParseSnapshotWithWarnings(buildFrame(objectNeighboursState, inputNode("cam1", "Input 1")))
	if err != nil {
		t.Fatalf("ParseSnapshotWithWarnings: %v", err)
	}
	for _, w := range warnings {
		t.Errorf("unexpected warning: %s", w)
	}

	if len(snap.Strips) != 1 {
		names := make([]string, 0, len(snap.Strips))
		for _, s := range snap.Strips {
			names = append(names, s.Name)
		}
		t.Fatalf("len(Strips) = %d %v, want 1 [cam1-1]: an object sitting beside a strip was promoted to a phantom channel", len(snap.Strips), names)
	}
	if snap.Strips[0].Name != "cam1-1" {
		t.Fatalf("Strips[0].Name = %q, want %q", snap.Strips[0].Name, "cam1-1")
	}

	for _, name := range []string{"eq", "agc", "settings"} {
		if _, ok := snap.Strip(name); ok {
			t.Errorf("Strip(%q) found; it is an object key of an input, not a channel strip", name)
		}
	}

	// The real strip must still be intact, so the test cannot pass by the
	// detector having become too strict instead of too loose.
	s := snap.Strips[0]
	if !s.Muted || s.SubChMode != "ST_W" || s.DisplayName != "Input 1" {
		t.Errorf("strip = %+v, want muted / ST_W / Input 1", s)
	}
	if len(s.Outputs) != 3 {
		t.Errorf("Outputs = %v, want the three default buses", s.Outputs)
	}
}

// TestParseSnapshotFatalCases covers the frames that must NOT yield a Snapshot.
//
// The rule these enforce is the one from ParseSnapshot's contract: an empty
// mixer view renders as "nothing is routed to the clean feed", which is the
// most dangerous false statement this drawer can make. So an unusable frame is
// an error, never an empty success.
func TestParseSnapshotFatalCases(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr error // nil means "any error"
	}{
		{"empty input", ``, nil},
		{"not json", `<html>signed out</html>`, nil},
		{"json but not an object", `[1,2,3]`, nil},
		{"status is not an array", `{"status":{"node":"advanced_audio_mixer"},"timestamp":1}`, nil},
		{"no status key at all", `{"timestamp":1785522083212}`, ErrNoMixerNode},
		{"empty status array", `{"status":[],"timestamp":1785522083212}`, ErrNoMixerNode},
		{
			// The video mixer node is called "mixer" and lives in the same
			// array. Matching it would produce a mixer with no buses.
			"only the video mixer node",
			`{"status":[{"node":"mixer","path":"/","state":{"program":{}}}],"timestamp":1785522083212}`,
			ErrNoMixerNode,
		},
		{
			"mixer node name is a near miss",
			`{"status":[{"node":"advanced_audio_mixer2","path":"/","state":{}}],"timestamp":1}`,
			ErrNoMixerNode,
		},
		{
			"mixer node state is null",
			`{"status":[{"node":"advanced_audio_mixer","path":"/","state":null}],"timestamp":1}`,
			ErrNoMixerNode,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap, err := ParseSnapshot([]byte(tt.raw))
			if err == nil {
				t.Fatalf("ParseSnapshot returned nil error; an unusable frame must never parse as an empty mixer")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
			if len(snap.Strips) != 0 || len(snap.Buses) != 0 {
				t.Errorf("fatal parse returned %d strips and %d buses, want none", len(snap.Strips), len(snap.Buses))
			}
		})
	}
}

// TestParseSnapshotMinimalFrame checks the synthetic base parses cleanly, so
// that a failure in the degradation tests below is attributable to the
// degradation rather than to the fixture.
func TestParseSnapshotMinimalFrame(t *testing.T) {
	snap, warnings, err := ParseSnapshotWithWarnings(buildFrame(minimalState, inputNode("cam1", "Input 1")))
	if err != nil {
		t.Fatalf("ParseSnapshotWithWarnings: %v", err)
	}
	for _, w := range warnings {
		t.Errorf("unexpected warning: %s", w)
	}
	if len(snap.Strips) != 1 {
		t.Fatalf("len(Strips) = %d, want 1", len(snap.Strips))
	}
	if len(snap.Buses) != 7 {
		t.Fatalf("len(Buses) = %d, want 7", len(snap.Buses))
	}
	s := snap.Strips[0]
	if s.Name != "cam1-1" || s.Input != "cam1" || s.DisplayName != "Input 1" {
		t.Errorf("strip = %+v, want cam1-1 / cam1 / Input 1", s)
	}
	if !s.Metered || s.Level != [2]float64{-20, -21} || s.PeakHold != [2]float64{-10, -11} {
		t.Errorf("meters = %v %v metered=%v, want [-20 -21] [-10 -11] true", s.Level, s.PeakHold, s.Metered)
	}
}

// TestParseSnapshotDegradedCases is the heart of the defensive contract.
//
// Each case damages one field and asserts three things: the snapshot survives,
// the damaged field degrades to something the contract can render honestly, and
// the loss is visible as a warning with the right criticality.
func TestParseSnapshotDegradedCases(t *testing.T) {
	tests := []struct {
		name         string
		state        string
		extraNodes   []string
		wantStrips   int
		wantCritical bool
		wantWarnPath string // substring match on at least one warning path
		check        func(t *testing.T, snap Snapshot)
	}{
		{
			// The named case: a strip that IS routed but whose routing cannot
			// be read. Empty Outputs renders as "clear of the clean feed", so
			// this must be critical.
			name: "matrix entry is malformed",
			state: `{"inputs":{"cam1":{"cam1-1":{"display_name":"cam1-1","muted":false}}},
				"matrix":{"cam1-1":"master,aux1"},
				"outputs":{},"levels":{},"peak_hold_levels":{},"fader":{}}`,
			wantStrips:   1,
			wantCritical: true,
			wantWarnPath: "matrix.cam1-1",
			check: func(t *testing.T, snap Snapshot) {
				s, _ := snap.Strip("cam1-1")
				if len(s.Outputs) != 0 {
					t.Errorf("Outputs = %v, want empty when routing is unreadable", s.Outputs)
				}
			},
		},
		{
			name: "matrix outputs is not an array of strings",
			state: `{"inputs":{"cam1":{"cam1-1":{"display_name":"cam1-1","muted":false}}},
				"matrix":{"cam1-1":{"outputs":[1,2],"pfl_outputs":[]}},
				"outputs":{},"levels":{},"peak_hold_levels":{},"fader":{}}`,
			wantStrips:   1,
			wantCritical: true,
			wantWarnPath: "matrix.cam1-1.outputs",
		},
		{
			// REGRESSION. encoding/json unmarshals null into a slice without
			// error, so this parsed as an empty routing — a strip reported as
			// clear of the clean feed — with no warning at all, until
			// isJSONNull was added.
			name: "matrix outputs is null",
			state: `{"inputs":{"cam1":{"cam1-1":{"display_name":"cam1-1","muted":false}}},
				"matrix":{"cam1-1":{"outputs":null,"pfl_outputs":[]}},
				"outputs":{},"levels":{},"peak_hold_levels":{},"fader":{}}`,
			wantStrips:   1,
			wantCritical: true,
			wantWarnPath: "matrix.cam1-1.outputs",
			check: func(t *testing.T, snap Snapshot) {
				s, _ := snap.Strip("cam1-1")
				if len(s.Outputs) != 0 {
					t.Errorf("Outputs = %v, want empty", s.Outputs)
				}
			},
		},
		{
			// REGRESSION, and the worst of the null family: this read as
			// "muted": false — an UNMUTED strip in the clean feed — silently.
			name: "muted is null",
			state: `{"inputs":{"cam1":{"cam1-1":{"display_name":"cam1-1","muted":null}}},
				"matrix":{"cam1-1":{"outputs":["master","aux1"],"pfl_outputs":[]}},
				"outputs":{},"levels":{},"peak_hold_levels":{},"fader":{}}`,
			wantStrips:   1,
			wantCritical: false,
			wantWarnPath: "muted",
			check: func(t *testing.T, snap Snapshot) {
				if _, ok := snap.Strip("cam1-1"); !ok {
					t.Fatal("strip vanished because muted was null")
				}
			},
		},
		{
			// REGRESSION: a null gain read as 0 dB — unity — on a fader that
			// was never actually read.
			name: "fader gain is null",
			state: `{"inputs":{"cam1":{"cam1-1":{"display_name":"cam1-1","muted":true}}},
				"matrix":{"cam1-1":{"outputs":["master"],"pfl_outputs":[]}},
				"outputs":{},"levels":{},"peak_hold_levels":{},
				"fader":{"cam1-1":{"ch_fader":{"enabled":null,"gain":null}}}}`,
			wantStrips:   1,
			wantCritical: false,
			wantWarnPath: "fader.cam1-1.ch_fader",
			check: func(t *testing.T, snap Snapshot) {
				s, _ := snap.Strip("cam1-1")
				if s.FaderEnabled != [2]bool{} {
					t.Errorf("FaderEnabled = %v, want [false false]", s.FaderEnabled)
				}
			},
		},
		{
			name: "strip is in inputs but not in matrix",
			state: `{"inputs":{"cam1":{"cam1-1":{"display_name":"cam1-1","muted":false}}},
				"matrix":{},"outputs":{},"levels":{},"peak_hold_levels":{},"fader":{}}`,
			wantStrips:   1,
			wantCritical: true,
			wantWarnPath: "matrix.cam1-1",
			check: func(t *testing.T, snap Snapshot) {
				// It must still be listed. Dropping it would hide a channel.
				if _, ok := snap.Strip("cam1-1"); !ok {
					t.Error("strip disappeared; an unrouted strip must still be shown")
				}
			},
		},
		{
			// The dangerous direction: routed, possibly into the clean feed,
			// but absent from inputs. It must be shown with its routing.
			name: "strip is in matrix but not in inputs",
			state: `{"inputs":{},
				"matrix":{"cam9-1":{"outputs":["master","aux1"],"pfl_outputs":[]}},
				"outputs":{},"levels":{},"peak_hold_levels":{},"fader":{}}`,
			wantStrips:   1,
			wantCritical: false,
			wantWarnPath: "matrix.cam9-1",
			check: func(t *testing.T, snap Snapshot) {
				s, ok := snap.Strip("cam9-1")
				if !ok {
					t.Fatal("routed strip absent from the snapshot; it may be in the clean feed")
				}
				if s.Input != "cam9" {
					t.Errorf("Input = %q, want %q derived from the strip name", s.Input, "cam9")
				}
				if got, want := s.Outputs, []Bus{BusMaster, BusAux1}; !reflect.DeepEqual(got, want) {
					t.Errorf("Outputs = %v, want %v", got, want)
				}
			},
		},
		{
			name: "matrix is missing entirely",
			state: `{"inputs":{"cam1":{"cam1-1":{"display_name":"cam1-1","muted":false}}},
				"outputs":{},"levels":{},"peak_hold_levels":{},"fader":{}}`,
			wantStrips:   1,
			wantCritical: true,
			wantWarnPath: "matrix",
		},
		{
			name: "a bus is absent from outputs",
			state: `{"inputs":{},"matrix":{},
				"outputs":{"master":{"channel_count":2,"muted":false}},
				"levels":{},"peak_hold_levels":{},"fader":{}}`,
			wantStrips:   0,
			wantCritical: false,
			wantWarnPath: "outputs.aux1",
			check: func(t *testing.T, snap Snapshot) {
				// All seven rows are still drawn, so the CLN row never
				// silently vanishes from the drawer.
				if len(snap.Buses) != 7 {
					t.Fatalf("len(Buses) = %d, want 7 even when the frame omits some", len(snap.Buses))
				}
				if snap.Buses[1].Name != BusAux1 || snap.Buses[1].Metered {
					t.Errorf("aux1 row = %+v, want present and unmetered", snap.Buses[1])
				}
			},
		},
		{
			// BusLabel promises an eighth bus is shown, not hidden.
			name: "an unknown eighth bus",
			state: `{"inputs":{},"matrix":{},
				"outputs":{"master":{"channel_count":2,"muted":false},"aux1":{"channel_count":2,"muted":false},
					"aux2":{"channel_count":2,"muted":false},"mon1":{"channel_count":2,"muted":false},
					"mon2":{"channel_count":2,"muted":false},"mon3":{"channel_count":2,"muted":false},
					"mon4":{"channel_count":2,"muted":false},"aux3":{"channel_count":2,"muted":true}},
				"levels":{},"peak_hold_levels":{},"fader":{}}`,
			wantStrips:   0,
			wantCritical: false,
			wantWarnPath: "outputs.aux3",
			check: func(t *testing.T, snap Snapshot) {
				if len(snap.Buses) != 8 {
					t.Fatalf("len(Buses) = %d, want 8; an unknown bus must be shown, not dropped", len(snap.Buses))
				}
				last := snap.Buses[7]
				if last.Name != Bus("aux3") || !last.Muted {
					t.Errorf("eighth bus = %+v, want aux3 muted", last)
				}
				if got, want := BusLabel(last.Name), "aux3 (unknown bus)"; got != want {
					t.Errorf("BusLabel = %q, want %q", got, want)
				}
			},
		},
		{
			// One meter present, the other missing. Metered must go false, or
			// the missing half draws from its zero value: 0 dBFS, full scale.
			name: "level without a peak hold",
			state: `{"inputs":{"cam1":{"cam1-1":{"display_name":"cam1-1","muted":true}}},
				"matrix":{"cam1-1":{"outputs":["master"],"pfl_outputs":[]}},
				"outputs":{},"levels":{"cam1-1":[-20,-21]},"peak_hold_levels":{},"fader":{}}`,
			wantStrips:   1,
			wantCritical: false,
			wantWarnPath: "levels.cam1-1",
			check: func(t *testing.T, snap Snapshot) {
				s, _ := snap.Strip("cam1-1")
				if s.Metered {
					t.Error("Metered = true with no peak hold; the missing half would draw at full scale")
				}
				if s.Level != [2]float64{} {
					t.Errorf("Level = %v, want zeroed when not metered", s.Level)
				}
			},
		},
		{
			name: "meter is a single number instead of a pair",
			state: `{"inputs":{"cam1":{"cam1-1":{"display_name":"cam1-1","muted":true}}},
				"matrix":{"cam1-1":{"outputs":["master"],"pfl_outputs":[]}},
				"outputs":{},"levels":{"cam1-1":-20},"peak_hold_levels":{"cam1-1":[-10,-11]},"fader":{}}`,
			wantStrips:   1,
			wantCritical: false,
			wantWarnPath: "levels.cam1-1",
			check: func(t *testing.T, snap Snapshot) {
				s, _ := snap.Strip("cam1-1")
				if s.Metered {
					t.Error("Metered = true from a malformed meter")
				}
			},
		},
		{
			name: "meter is a one-element array",
			state: `{"inputs":{"cam1":{"cam1-1":{"display_name":"cam1-1","muted":true}}},
				"matrix":{"cam1-1":{"outputs":["master"],"pfl_outputs":[]}},
				"outputs":{},"levels":{"cam1-1":[-20]},"peak_hold_levels":{"cam1-1":[-10,-11]},"fader":{}}`,
			wantStrips:   1,
			wantCritical: false,
			wantWarnPath: "levels.cam1-1",
			check: func(t *testing.T, snap Snapshot) {
				s, _ := snap.Strip("cam1-1")
				if s.Metered {
					t.Error("Metered = true from a one-element meter; the right channel would be invented")
				}
			},
		},
		{
			name: "fader entry is malformed",
			state: `{"inputs":{"cam1":{"cam1-1":{"display_name":"cam1-1","muted":true}}},
				"matrix":{"cam1-1":{"outputs":["master"],"pfl_outputs":[]}},
				"outputs":{},"levels":{},"peak_hold_levels":{},
				"fader":{"cam1-1":{"ch_fader":{"enabled":"yes","gain":"loud"}}}}`,
			wantStrips:   1,
			wantCritical: false,
			wantWarnPath: "fader.cam1-1.ch_fader",
			check: func(t *testing.T, snap Snapshot) {
				s, _ := snap.Strip("cam1-1")
				if s.FaderEnabled != [2]bool{} {
					t.Errorf("FaderEnabled = %v, want [false false] so the gain is drawn as not in circuit", s.FaderEnabled)
				}
			},
		},
		{
			name: "bus fader gain is not a number",
			state: `{"inputs":{},"matrix":{},
				"outputs":{"master":{"channel_count":2,"muted":false}},
				"levels":{},"peak_hold_levels":{},
				"fader":{"master":{"output_fader":{"gain":{}}}}}`,
			wantStrips:   0,
			wantCritical: false,
			wantWarnPath: "fader.master.output_fader.gain",
			check: func(t *testing.T, snap Snapshot) {
				if snap.Buses[0].FaderPresent {
					t.Error("FaderPresent = true from an unreadable gain")
				}
			},
		},
		{
			// muted arriving as a string must not delete the strip. The
			// structural test is for key PRESENCE, and the value degrades.
			name: "muted is a string",
			state: `{"inputs":{"cam1":{"cam1-1":{"display_name":"cam1-1","muted":"true"}}},
				"matrix":{"cam1-1":{"outputs":["master","aux1"],"pfl_outputs":[]}},
				"outputs":{},"levels":{},"peak_hold_levels":{},"fader":{}}`,
			wantStrips:   1,
			wantCritical: false,
			check: func(t *testing.T, snap Snapshot) {
				s, ok := snap.Strip("cam1-1")
				if !ok {
					t.Fatal("strip vanished because muted was a string")
				}
				if !s.Muted {
					t.Error("Muted = false; the string \"true\" is parseable")
				}
			},
		},
		{
			name: "muted is a value no reading can rescue",
			state: `{"inputs":{"cam1":{"cam1-1":{"display_name":"cam1-1","muted":{"why":"not"}}}},
				"matrix":{"cam1-1":{"outputs":["master","aux1"],"pfl_outputs":[]}},
				"outputs":{},"levels":{},"peak_hold_levels":{},"fader":{}}`,
			wantStrips:   1,
			wantCritical: false,
			wantWarnPath: "muted",
			check: func(t *testing.T, snap Snapshot) {
				s, ok := snap.Strip("cam1-1")
				if !ok {
					t.Fatal("strip vanished because muted was unreadable")
				}
				// Reading an unknown mute as UNMUTED is the alarming
				// direction, and on a routing surface alarming beats
				// reassuring.
				if s.Muted {
					t.Error("Muted = true from an unreadable value; unknown must read as unmuted, the alarming direction")
				}
			},
		},
		{
			// The test that forces strip detection to be STRUCTURAL. The live
			// frame cannot force it, because its only non-strip keys —
			// assign_list and channel_count — contain no hyphen, so a lazy
			// `strings.Contains(key,"-")` detector passes every assertion
			// against the real capture. A mutation run proved exactly that,
			// which is why this case exists. A hyphenated non-strip key must
			// not become a phantom channel with no routing, rendering as
			// "not in the clean feed".
			name: `an input carries a hyphenated key that is not a strip`,
			state: `{"inputs":{"cam1":{"assign_list":[[1,2]],"channel_count":2,
					"sub-ch-map":[1,2],"mix-minus":false,"gain-trim":{"gain":0},
					"cam1-1":{"display_name":"cam1-1","muted":true}}},
				"matrix":{"cam1-1":{"outputs":["master"],"pfl_outputs":[]}},
				"outputs":{},"levels":{},"peak_hold_levels":{},"fader":{}}`,
			wantStrips:   1,
			wantCritical: false,
			check: func(t *testing.T, snap Snapshot) {
				for _, name := range []string{"sub-ch-map", "mix-minus", "gain-trim"} {
					if _, ok := snap.Strip(name); ok {
						t.Errorf("non-strip key %q was parsed as a strip; detection is matching the name, not the shape", name)
					}
				}
				if snap.Strips[0].Name != "cam1-1" {
					t.Errorf("Strips[0].Name = %q, want %q", snap.Strips[0].Name, "cam1-1")
				}
			},
		},
		{
			// The mirror of the case above: a strip whose name has no hyphen
			// at all must still be found, because detection is structural.
			name: `a strip whose name has no hyphen`,
			state: `{"inputs":{"aes":{"channel_count":2,
					"aes":{"display_name":"aes","muted":false}}},
				"matrix":{"aes":{"outputs":["master","aux1"],"pfl_outputs":[]}},
				"outputs":{},"levels":{},"peak_hold_levels":{},"fader":{}}`,
			wantStrips:   1,
			wantCritical: false,
			check: func(t *testing.T, snap Snapshot) {
				s, ok := snap.Strip("aes")
				if !ok {
					t.Fatal("strip with no hyphen in its name was not found")
				}
				if s.Input != "aes" {
					t.Errorf("Input = %q, want %q", s.Input, "aes")
				}
				if got, want := s.Outputs, []Bus{BusMaster, BusAux1}; !reflect.DeepEqual(got, want) {
					t.Errorf("Outputs = %v, want %v; this strip is in the clean feed and unmuted", got, want)
				}
			},
		},
		{
			// An input whose value is not an object at all.
			name: "input is not an object",
			state: `{"inputs":{"cam1":"broken","cam2":{"cam2-1":{"display_name":"cam2-1","muted":true}}},
				"matrix":{"cam2-1":{"outputs":["master"],"pfl_outputs":[]}},
				"outputs":{},"levels":{},"peak_hold_levels":{},"fader":{}}`,
			wantStrips:   1,
			wantCritical: false,
			wantWarnPath: "inputs.cam1",
			check: func(t *testing.T, snap Snapshot) {
				if _, ok := snap.Strip("cam2-1"); !ok {
					t.Error("a broken sibling input cost cam2-1 its row")
				}
			},
		},
		{
			// A broken status element must not cost the mixer node that
			// follows it in the array.
			name:       "a malformed status element precedes the mixer node",
			state:      minimalState,
			extraNodes: []string{`"not an object"`, `{"node":42,"state":{}}`, inputNode("cam1", "Input 1")},
			wantStrips: 1,
			check: func(t *testing.T, snap Snapshot) {
				s, ok := snap.Strip("cam1-1")
				if !ok {
					t.Fatal("mixer node lost to a malformed sibling element")
				}
				if s.DisplayName != "Input 1" {
					t.Errorf("DisplayName = %q, want %q; the join survived the bad elements", s.DisplayName, "Input 1")
				}
			},
		},
		{
			// No per-input status node: fall back down the chain documented on
			// Strip.DisplayName, never to a blank cell.
			name:       "no per-input status node for the display-name join",
			state:      minimalState,
			wantStrips: 1,
			check: func(t *testing.T, snap Snapshot) {
				s, _ := snap.Strip("cam1-1")
				if s.DisplayName != "cam1-1" {
					t.Errorf("DisplayName = %q, want the strip's own name as the last fallback", s.DisplayName)
				}
			},
		},
		{
			name:       "per-input display name is blank",
			state:      minimalState,
			extraNodes: []string{inputNode("cam1", "   ")},
			wantStrips: 1,
			check: func(t *testing.T, snap Snapshot) {
				s, _ := snap.Strip("cam1-1")
				if strings.TrimSpace(s.DisplayName) == "" {
					t.Error("DisplayName is blank; a whitespace name must fall through, never render as an empty cell")
				}
			},
		},
		{
			// A meter key naming something that parsed as neither strip nor
			// bus means the strip detection may have missed a channel.
			name: "a metered key is neither strip nor bus",
			state: `{"inputs":{},"matrix":{},"outputs":{},
				"levels":{"ghost-1":[-20,-21]},"peak_hold_levels":{"ghost-1":[-10,-11]},"fader":{}}`,
			wantStrips:   0,
			wantCritical: false,
			wantWarnPath: "levels.ghost-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := buildFrame(tt.state, tt.extraNodes...)
			snap, warnings, err := ParseSnapshotWithWarnings(raw)
			if err != nil {
				t.Fatalf("ParseSnapshotWithWarnings returned a fatal error %v; a damaged field must not cost the whole snapshot", err)
			}
			if got := len(snap.Strips); got != tt.wantStrips {
				t.Errorf("len(Strips) = %d, want %d", got, tt.wantStrips)
			}
			if len(snap.Buses) < len(AllBuses) {
				t.Errorf("len(Buses) = %d, want at least %d", len(snap.Buses), len(AllBuses))
			}

			// The loss must be visible.
			if tt.wantWarnPath != "" {
				found := false
				for _, w := range warnings {
					if strings.Contains(w.Path, tt.wantWarnPath) {
						found = true
					}
				}
				if !found {
					t.Errorf("no warning with a path containing %q; got %v", tt.wantWarnPath, warnings)
				}
			}
			gotCritical := false
			for _, w := range warnings {
				if w.Critical {
					gotCritical = true
				}
			}
			if gotCritical != tt.wantCritical {
				t.Errorf("critical warning present = %v, want %v; warnings: %v", gotCritical, tt.wantCritical, warnings)
			}

			// ParseSnapshot must surface criticality as an error while still
			// returning the usable snapshot.
			contractSnap, contractErr := ParseSnapshot(raw)
			if tt.wantCritical {
				var perr *ParseError
				if !errors.As(contractErr, &perr) {
					t.Errorf("ParseSnapshot error = %v, want a *ParseError", contractErr)
				} else if len(perr.CriticalWarnings()) == 0 {
					t.Error("*ParseError carries no critical warnings")
				}
				if len(contractSnap.Strips) != tt.wantStrips {
					t.Errorf("ParseSnapshot dropped the snapshot alongside the error: %d strips, want %d",
						len(contractSnap.Strips), tt.wantStrips)
				}
			} else if contractErr != nil {
				t.Errorf("ParseSnapshot error = %v, want nil for a non-critical degradation", contractErr)
			}

			if tt.check != nil {
				tt.check(t, snap)
			}
		})
	}
}

// TestParseSnapshotTimestampForms covers the timestamp, including the shapes
// that must degrade rather than lie.
func TestParseSnapshotTimestampForms(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantZero bool
		wantUnix int64
	}{
		{"epoch milliseconds", `1785522083212`, false, 1785522083212},
		// Numeric strings happen on this device: frame_rate is "50" while
		// width and height are numbers.
		{"epoch milliseconds as a string", `"1785522083212"`, false, 1785522083212},
		{"absent", ``, true, 0},
		{"null", `null`, true, 0},
		{"not a number", `"just now"`, true, 0},
		{"an object", `{"ms":1785522083212}`, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"status":[{"node":%q,"path":"/","state":%s}]`, mixerNodeName, minimalState)
			if tt.raw != "" {
				body += `,"timestamp":` + tt.raw
			}
			body += `}`

			snap, warnings, err := ParseSnapshotWithWarnings([]byte(body))
			if err != nil {
				t.Fatalf("ParseSnapshotWithWarnings: %v", err)
			}
			if tt.wantZero {
				if !snap.TakenAt.IsZero() {
					t.Errorf("TakenAt = %v, want the zero time", snap.TakenAt)
				}
				// Never time.Now(): a frame that stopped arriving would look
				// perpetually fresh.
				if snap.TakenAt.After(time.Now().Add(-time.Hour)) && !snap.TakenAt.IsZero() {
					t.Error("TakenAt was substituted with something near the local clock")
				}
				found := false
				for _, w := range warnings {
					if w.Path == "timestamp" {
						found = true
					}
				}
				if !found {
					t.Errorf("no timestamp warning; got %v", warnings)
				}
				return
			}
			if got := snap.TakenAt.UnixMilli(); got != tt.wantUnix {
				t.Errorf("TakenAt.UnixMilli() = %d, want %d", got, tt.wantUnix)
			}
			// A parsed snapshot must still be usable.
			if len(snap.Strips) != 1 {
				t.Errorf("len(Strips) = %d, want 1", len(snap.Strips))
			}
		})
	}
}

// TestWarningsAreDeterministic requires the warning list itself to be stable.
// Warnings are generated by ranging Go maps, so without sorting a UI showing
// them would reorder its own rows at every refresh.
func TestWarningsAreDeterministic(t *testing.T) {
	state := `{"inputs":{"cam1":{"cam1-1":{"display_name":"cam1-1","muted":true},
			"cam1-2":{"display_name":"cam1-2","muted":true}},
		"cam2":{"cam2-1":{"display_name":"cam2-1","muted":true}}},
		"matrix":{},"outputs":{},
		"levels":{"ghost-1":[-1,-1],"ghost-2":[-1,-1],"ghost-3":[-1,-1]},
		"peak_hold_levels":{"ghost-1":[-1,-1],"ghost-2":[-1,-1],"ghost-3":[-1,-1]},"fader":{}}`
	raw := buildFrame(state)

	_, first, err := ParseSnapshotWithWarnings(raw)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	if len(first) < 6 {
		t.Fatalf("got %d warnings, want at least 6 to make ordering meaningful: %v", len(first), first)
	}
	for i := range 5 {
		_, next, err := ParseSnapshotWithWarnings(raw)
		if err != nil {
			t.Fatalf("parse %d: %v", i, err)
		}
		if !reflect.DeepEqual(first, next) {
			t.Fatalf("warning order is not deterministic:\n%v\n%v", first, next)
		}
	}
	// Critical warnings sort first so a truncated log still shows the ones
	// that matter.
	seenNonCritical := false
	for _, w := range first {
		if !w.Critical {
			seenNonCritical = true
			continue
		}
		if seenNonCritical {
			t.Errorf("critical warning %q sorts after a non-critical one", w.Path)
		}
	}
}

// TestParseErrorMessage checks the error text names the affected paths, because
// it is what lands in a log when the drawer refuses to trust a row.
func TestParseErrorMessage(t *testing.T) {
	err := &ParseError{Warnings: []ParseWarning{
		{Path: "matrix.cam22-1", Reason: "strip has no routing entry", Critical: true},
		{Path: "fader.cam1-1", Reason: "no fader entry"},
	}}
	msg := err.Error()
	for _, want := range []string{"matrix.cam22-1", "strip has no routing entry", "1 non-critical"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, want it to contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "fader.cam1-1") {
		t.Errorf("Error() = %q, want non-critical paths summarised rather than listed", msg)
	}
	if got := len(err.CriticalWarnings()); got != 1 {
		t.Errorf("len(CriticalWarnings()) = %d, want 1", got)
	}
}

// TestParseWarningString covers the rendering used in logs and the drawer.
func TestParseWarningString(t *testing.T) {
	tests := []struct {
		name string
		w    ParseWarning
		want string
	}{
		{"critical is flagged", ParseWarning{Path: "matrix.cam22-1", Reason: "no routing entry", Critical: true},
			"CRITICAL matrix.cam22-1: no routing entry"},
		{"non critical is plain", ParseWarning{Path: "fader.cam1-1", Reason: "no fader entry"},
			"fader.cam1-1: no fader entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.w.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ============================== unit helpers ================================

// TestLooksLikeStrip is the structural detector in isolation. The two false
// cases are the exact keys that sit beside strips in every input.
func TestLooksLikeStrip(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"a real strip", `{"display_name":"cam22-1","follow":false,"muted":true,"sub_ch_mode":"ST_W"}`, true},
		{"a strip with only the two marker keys", `{"display_name":"x","muted":false}`, true},
		{"a strip whose muted is the wrong type is still a strip", `{"display_name":"x","muted":"true"}`, true},
		{"assign_list", `[[1,2]]`, false},
		{"channel_count", `2`, false},
		{"a bus entry has muted but no display_name", `{"channel_count":2,"muted":false}`, false},
		{"an input node state has display_name but no muted", `{"display_name":"CLAUDE-COMMS","settings":{}}`, false},
		{"null", `null`, false},
		{"a string", `"cam22-1"`, false},
		{"an empty object", `{}`, false},
		{"absent", ``, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeStrip(json.RawMessage(tt.raw)); got != tt.want {
				t.Errorf("looksLikeStrip(%s) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

// TestInputNameOf covers the strip-name split, including the input names in the
// live frame that break the obvious implementations.
func TestInputNameOf(t *testing.T) {
	tests := []struct {
		strip string
		want  string
	}{
		{"cam22-1", "cam22"},
		{"cam22-2", "cam22"},
		{"replay1-1", "replay1"},
		{"vtr2-2", "vtr2"},
		// Contains a space: rules out splitting on whitespace.
		{"MIC 1-1", "MIC 1"},
		{"MIC 3-2", "MIC 3"},
		// Split on the LAST hyphen: operator-set names may contain one.
		{"CLAUDE-COMMS-1", "CLAUDE-COMMS"},
		{"a-b-c-2", "a-b-c"},
		// No hyphen: return it unchanged so the display-name join still has a
		// key to look up.
		{"weird", "weird"},
		// A leading hyphen is not a separator; there is no input name to the
		// left of it.
		{"-1", "-1"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.strip, func(t *testing.T) {
			if got := inputNameOf(tt.strip); got != tt.want {
				t.Errorf("inputNameOf(%q) = %q, want %q", tt.strip, got, tt.want)
			}
		})
	}
}

// TestDecodeStrings covers the all-or-nothing rule for bus lists.
func TestDecodeStrings(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		want   []string
		wantOK bool
	}{
		{"a bus list", `["master","aux1","aux2"]`, []string{"master", "aux1", "aux2"}, true},
		{"empty", `[]`, []string{}, true},
		// The whole array is refused rather than silently shortened: a
		// three-bus routing quietly becoming two could drop aux1, and nothing
		// downstream could tell that from a strip routed clear.
		{"one bad element rejects the array", `["master",7,"aux2"]`, nil, false},
		{"one null element rejects the array", `["master",null]`, nil, false},
		{"not an array", `"master"`, nil, false},
		{"an object", `{"0":"master"}`, nil, false},
		{"absent", ``, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := decodeStrings(json.RawMessage(tt.raw))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("= %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDecodeFloat covers the tolerant number reader. The string case is real:
// MEASURED, the same frame carries "frame_rate":"50" beside "width":1920.
func TestDecodeFloat(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		want   float64
		wantOK bool
	}{
		{"a number", `-52.78746032714844`, -52.78746032714844, true},
		{"an integer", `3`, 3, true},
		{"zero", `0`, 0, true},
		{"a numeric string", `"50"`, 50, true},
		{"a negative numeric string", `"-3.5"`, -3.5, true},
		{"a padded numeric string", `" -3.5 "`, -3.5, true},
		{"a non-numeric string", `"loud"`, 0, false},
		{"a boolean", `true`, 0, false},
		{"null", `null`, 0, false},
		{"an object", `{}`, 0, false},
		{"an array", `[1]`, 0, false},
		{"absent", ``, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := decodeFloat(json.RawMessage(tt.raw))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("= %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDecodeBool covers the tolerant boolean reader.
func TestDecodeBool(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		want   bool
		wantOK bool
	}{
		{"true", `true`, true, true},
		{"false", `false`, false, true},
		{`"true"`, `"true"`, true, true},
		{`"false"`, `"false"`, false, true},
		{`"1"`, `"1"`, true, true},
		{`"0"`, `"0"`, false, true},
		// A bare number is refused: 1 could be a channel index as easily as a
		// truth value, and guessing would invent a mute state.
		{"a number", `1`, false, false},
		{"a word", `"yes"`, false, false},
		{"null", `null`, false, false},
		{"an object", `{}`, false, false},
		{"absent", ``, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := decodeBool(json.RawMessage(tt.raw))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("= %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDecodeFloatPair covers meter and gain pairs.
func TestDecodeFloatPair(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		want   [2]float64
		wantOK bool
	}{
		{"a meter", `[-87.69413757324219,-87.65318298339844]`, [2]float64{-87.69413757324219, -87.65318298339844}, true},
		{"silence", `[-100,-100]`, [2]float64{-100, -100}, true},
		{"numeric strings", `["-3","-3"]`, [2]float64{-3, -3}, true},
		// Longer arrays degrade to the first two rather than vanishing, so a
		// future multichannel bus still meters.
		{"more than two elements", `[-1,-2,-3,-4]`, [2]float64{-1, -2}, true},
		// Shorter ones are refused: inventing a right channel would draw a
		// stereo meter that is half fiction.
		{"one element", `[-1]`, [2]float64{}, false},
		{"empty array", `[]`, [2]float64{}, false},
		{"a scalar", `-1`, [2]float64{}, false},
		{"a bad element", `[-1,"loud"]`, [2]float64{}, false},
		{"null", `null`, [2]float64{}, false},
		{"absent", ``, [2]float64{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := decodeFloatPair(json.RawMessage(tt.raw))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("= %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDecodeBoolPair covers the fader-enabled pair.
func TestDecodeBoolPair(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		want   [2]bool
		wantOK bool
	}{
		{"both enabled", `[true,true]`, [2]bool{true, true}, true},
		{"both disabled", `[false,false]`, [2]bool{false, false}, true},
		// Half in circuit is a real and dangerous state, so it must survive.
		{"one enabled", `[true,false]`, [2]bool{true, false}, true},
		{"strings", `["true","false"]`, [2]bool{true, false}, true},
		{"one element", `[true]`, [2]bool{}, false},
		{"a scalar", `true`, [2]bool{}, false},
		{"a bad element", `[true,"maybe"]`, [2]bool{}, false},
		{"absent", ``, [2]bool{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := decodeBoolPair(json.RawMessage(tt.raw))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("= %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDecodeObjectRejectsNull pins the encoding/json behaviour this parser
// would otherwise be caught by: null unmarshals into a nil map WITHOUT an
// error, so a null matrix would read as "nothing is routed" rather than as a
// failure.
func TestDecodeObjectRejectsNull(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"an object", `{"a":1}`, false},
		{"an empty object", `{}`, false},
		{"null", `null`, true},
		{"absent", ``, true},
		{"an array", `[]`, true},
		{"a string", `"x"`, true},
		{"a number", `1`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeObject(json.RawMessage(tt.raw))
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}

	// The behaviour being guarded against, stated directly.
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`null`), &m); err != nil || m != nil {
		t.Fatalf("encoding/json no longer decodes null into a nil map without error (err=%v, m=%v); decodeObject's null guard needs review", err, m)
	}
}

// TestDecodeString covers the tolerant string reader.
func TestDecodeString(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		want   string
		wantOK bool
	}{
		{"a string", `"CLAUDE-COMMS"`, "CLAUDE-COMMS", true},
		{"an empty string", `""`, "", true},
		{"a number renders as one", `50`, "50", true},
		{"a float renders as one", `1.5`, "1.5", true},
		{"a boolean", `true`, "", false},
		{"null", `null`, "", false},
		{"an object", `{}`, "", false},
		{"absent", ``, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := decodeString(json.RawMessage(tt.raw))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("= %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSnapshotIsJSONSerialisable checks the snapshot survives the trip to the
// drawer, since the whole contract is tagged for it and a NaN or an Inf from a
// malformed meter would fail the marshal at runtime rather than here.
func TestSnapshotIsJSONSerialisable(t *testing.T) {
	snap := parseLive(t)

	encoded, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("json.Marshal(Snapshot): %v", err)
	}
	for _, want := range []string{`"cam22-1"`, `"CLAUDE-COMMS"`, `"aux1"`, `"master"`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("encoded snapshot does not contain %s", want)
		}
	}
}
