// golden_test.go — WP-M3.
//
// Two halves, matching the split in golden.go. The first exercises Compare
// against synthetic snapshots built directly from the Strip/BusState/Snapshot
// contract types in mixer.go, one behaviour at a time, because that is the
// only way to pin down severity rules precisely — a real frame is never
// going to isolate "exactly one strip's mute flipped and nothing else". The
// second exercises Compare and Diff.Render against the REAL captured live
// frame, parsed through WP-M2's ParseSnapshotWithWarnings, so the headline
// claim of this whole package — cam22-1 / CLAUDE-COMMS is measured, on the
// live event, routed into the clean feed — is checked against the actual
// evidence and not just against fixtures this file invented.
//
// Every test here asserts something specific enough that removing the logic
// under test (not just the whole file) makes it fail: severities, exact
// Field values, exact Render() sentences — never just "len(diffs) > 0".
package mixer

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fixture builders
// ---------------------------------------------------------------------------

// fixedTime is a UTC instant with no monotonic reading, so a Snapshot built
// with it round-trips through JSON and back to a struct reflect.DeepEqual
// still considers identical (see TestSaveLoadGolden_RoundTrip).
var fixedTime = time.UnixMilli(1785522083212).UTC()

// mkStrip builds a Strip with sane, mutually-equal-by-default field values,
// so a test that varies one field does not accidentally introduce a second,
// unrelated diff.
func mkStrip(name, input, displayName string, muted bool, outputs ...Bus) Strip {
	return Strip{
		Name:         name,
		Input:        input,
		DisplayName:  displayName,
		Muted:        muted,
		Outputs:      append([]Bus{}, outputs...),
		Fader:        [2]float64{0, 0},
		FaderEnabled: [2]bool{true, true},
	}
}

func mkBus(name Bus, muted bool) BusState {
	return BusState{Name: name, Muted: muted, ChannelCount: 2, FaderPresent: true, Fader: 0}
}

func mkSnapshot(strips []Strip, buses []BusState) Snapshot {
	return Snapshot{Strips: strips, Buses: buses, TakenAt: fixedTime}
}

// stdBuses is the seven buses, all unmuted, used as the bus list for tests
// that are only exercising strip-level behaviour.
func stdBuses() []BusState {
	buses := make([]BusState, len(AllBuses))
	for i, b := range AllBuses {
		buses[i] = mkBus(b, false)
	}
	return buses
}

// findDiff returns the first Diff matching kind/target/field, so a test can
// assert on that one Diff specifically rather than scanning the whole slice
// by hand.
func findDiff(diffs []Diff, kind DiffKind, target, field string) (Diff, bool) {
	for _, d := range diffs {
		if d.Kind == kind && d.Target == target && d.Field == field {
			return d, true
		}
	}
	return Diff{}, false
}

func removeBus(list []Bus, target Bus) []Bus {
	out := make([]Bus, 0, len(list))
	for _, b := range list {
		if b != target {
			out = append(out, b)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Compare: the identical case, and the exclusion of levels/peak-hold
// ---------------------------------------------------------------------------

func TestCompare_IdenticalSnapshotsProduceNothing(t *testing.T) {
	g := mkSnapshot(
		[]Strip{mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster, BusAux1)},
		stdBuses(),
	)
	c := mkSnapshot(
		[]Strip{mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster, BusAux1)},
		stdBuses(),
	)
	if got := Compare(g, c); len(got) != 0 {
		t.Errorf("Compare(identical) = %v, want no diffs", got)
	}
}

func TestCompare_LevelOnlyChangesProduceNothing(t *testing.T) {
	// Same configuration on both sides; only meter data differs, on both a
	// strip and a bus. This must NEVER be reported — see golden.go's
	// compareOneStrip / compareOneBus doc comments — because it changes every
	// update cycle and a diff list that fires every second is one nobody
	// reads.
	gStrip := mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster, BusAux1)
	gStrip.Level = [2]float64{-100, -100}
	gStrip.PeakHold = [2]float64{-100, -100}
	gStrip.Metered = true

	cStrip := gStrip
	cStrip.Level = [2]float64{-6.3, -6.1}
	cStrip.PeakHold = [2]float64{-3.0, -2.8}
	cStrip.Metered = true

	gBuses := stdBuses()
	gBuses[0].Level = [2]float64{-99.99999237060547, -99.99999237060547}
	gBuses[0].Metered = true
	cBuses := stdBuses()
	cBuses[0].Level = [2]float64{-1.0, -1.0}
	cBuses[0].PeakHold = [2]float64{0.5, 0.5}
	cBuses[0].Metered = true

	g := mkSnapshot([]Strip{gStrip}, gBuses)
	c := mkSnapshot([]Strip{cStrip}, cBuses)

	if got := Compare(g, c); len(got) != 0 {
		t.Errorf("Compare(level-only change) = %v, want no diffs", got)
	}
}

// ---------------------------------------------------------------------------
// Compare: routing / the clean feed
// ---------------------------------------------------------------------------

func TestCompare_StripGainsAux1IsCriticalAndNamesCleanFeed(t *testing.T) {
	g := mkSnapshot([]Strip{mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster)}, stdBuses())
	c := mkSnapshot([]Strip{mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster, BusAux1)}, stdBuses())

	got := Compare(g, c)
	d, ok := findDiff(got, DiffStrip, "cam22-1", fieldOutputs)
	if !ok {
		t.Fatalf("Compare = %v, want a %q diff on cam22-1", got, fieldOutputs)
	}
	if d.Severity != SeverityCritical {
		t.Errorf("severity = %q, want %q", d.Severity, SeverityCritical)
	}
	const want = "CLAUDE-COMMS (cam22-1) is now routed to the CLEAN FEED (aux1); it was not"
	if got := d.Render(); got != want {
		t.Errorf("Render() = %q, want %q", got, want)
	}
}

func TestCompare_StripLosesAux1IsNotCritical(t *testing.T) {
	g := mkSnapshot([]Strip{mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster, BusAux1, BusAux2)}, stdBuses())
	c := mkSnapshot([]Strip{mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster, BusAux2)}, stdBuses())

	got := Compare(g, c)
	d, ok := findDiff(got, DiffStrip, "cam22-1", fieldOutputs)
	if !ok {
		t.Fatalf("Compare = %v, want a %q diff on cam22-1", got, fieldOutputs)
	}
	if d.Severity == SeverityCritical {
		t.Errorf("severity = %q, want anything but critical for losing aux1 alone", d.Severity)
	}
	if d.Severity != SeverityWarn {
		t.Errorf("severity = %q, want %q", d.Severity, SeverityWarn)
	}
	const want = "CLAUDE-COMMS (cam22-1) is no longer routed to the CLEAN FEED (aux1); it was"
	if got := d.Render(); got != want {
		t.Errorf("Render() = %q, want %q", got, want)
	}
}

func TestCompare_StripLosesMasterIsCritical(t *testing.T) {
	// mixer.go's SeverityCritical doc comment names this the symmetric case
	// to gaining aux1: "Losing BusMaster is also critical: that is programme
	// audio disappearing."
	g := mkSnapshot([]Strip{mkStrip("cam23-1", "cam23", "", false, BusMaster, BusAux2)}, stdBuses())
	c := mkSnapshot([]Strip{mkStrip("cam23-1", "cam23", "", false, BusAux2)}, stdBuses())

	got := Compare(g, c)
	d, ok := findDiff(got, DiffStrip, "cam23-1", fieldOutputs)
	if !ok {
		t.Fatalf("Compare = %v, want a %q diff on cam23-1", got, fieldOutputs)
	}
	if d.Severity != SeverityCritical {
		t.Errorf("severity = %q, want %q", d.Severity, SeverityCritical)
	}
	// DisplayName is "" here, so the label falls back to Input ("cam23") —
	// see stripLabel.
	const want = "cam23 (cam23-1) is no longer routed to PROGRAMME (master); it was"
	if got := d.Render(); got != want {
		t.Errorf("Render() = %q, want %q", got, want)
	}
}

func TestCompare_OtherRoutingChangeIsWarning(t *testing.T) {
	// Gaining mon1 involves neither the clean feed nor programme.
	g := mkSnapshot([]Strip{mkStrip("mic1-1", "mic1", "", false, BusMaster)}, stdBuses())
	c := mkSnapshot([]Strip{mkStrip("mic1-1", "mic1", "", false, BusMaster, BusMon1)}, stdBuses())

	got := Compare(g, c)
	d, ok := findDiff(got, DiffStrip, "mic1-1", fieldOutputs)
	if !ok {
		t.Fatalf("Compare = %v, want a %q diff on mic1-1", got, fieldOutputs)
	}
	if d.Severity != SeverityWarn {
		t.Errorf("severity = %q, want %q", d.Severity, SeverityWarn)
	}
}

func TestCompare_PFLOutputsChangeIsWarningNeverCritical(t *testing.T) {
	g := mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster)
	c := mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster)
	c.PFLOutputs = []Bus{BusMon4}

	got := Compare(mkSnapshot([]Strip{g}, stdBuses()), mkSnapshot([]Strip{c}, stdBuses()))
	d, ok := findDiff(got, DiffStrip, "cam22-1", fieldPFLOutputs)
	if !ok {
		t.Fatalf("Compare = %v, want a %q diff on cam22-1", got, fieldPFLOutputs)
	}
	if d.Severity != SeverityWarn {
		t.Errorf("severity = %q, want %q", d.Severity, SeverityWarn)
	}
}

// ---------------------------------------------------------------------------
// Compare: mute, both directions, strip and bus
// ---------------------------------------------------------------------------

func TestCompare_StripMuteBothDirections(t *testing.T) {
	tests := []struct {
		name        string
		goldenMuted bool
		currentMute bool
		wantSev     Severity
	}{
		{"muted strip becomes unmuted: critical", true, false, SeverityCritical},
		{"unmuted strip becomes muted: not critical", false, true, SeverityWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := mkSnapshot([]Strip{mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", tt.goldenMuted, BusMaster)}, stdBuses())
			c := mkSnapshot([]Strip{mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", tt.currentMute, BusMaster)}, stdBuses())

			got := Compare(g, c)
			d, ok := findDiff(got, DiffStrip, "cam22-1", fieldMuted)
			if !ok {
				t.Fatalf("Compare = %v, want a %q diff on cam22-1", got, fieldMuted)
			}
			if d.Severity != tt.wantSev {
				t.Errorf("severity = %q, want %q", d.Severity, tt.wantSev)
			}
		})
	}
}

func TestCompare_BusMuteBothDirections(t *testing.T) {
	tests := []struct {
		name        string
		goldenMuted bool
		currentMute bool
		wantSev     Severity
	}{
		{"unmuted bus becomes muted: critical", false, true, SeverityCritical},
		{"muted bus becomes unmuted: not critical", true, false, SeverityWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gBuses := stdBuses()
			cBuses := stdBuses()
			for i, b := range gBuses {
				if b.Name == BusMaster {
					gBuses[i].Muted = tt.goldenMuted
					cBuses[i].Muted = tt.currentMute
				}
			}
			got := Compare(mkSnapshot(nil, gBuses), mkSnapshot(nil, cBuses))
			d, ok := findDiff(got, DiffBus, "master", fieldMuted)
			if !ok {
				t.Fatalf("Compare = %v, want a %q diff on master", got, fieldMuted)
			}
			if d.Severity != tt.wantSev {
				t.Errorf("severity = %q, want %q", d.Severity, tt.wantSev)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Compare: presence
// ---------------------------------------------------------------------------

func TestCompare_StripAppearedIsReportedAndCriticalWhenRoutedToCleanFeed(t *testing.T) {
	g := mkSnapshot([]Strip{mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster, BusAux1)}, stdBuses())
	// A brand new strip, arriving at the mixer's documented default routing.
	c := mkSnapshot([]Strip{
		mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster, BusAux1),
		mkStrip("cam99-1", "cam99", "NEW-CAM", false, BusMaster, BusAux1, BusAux2),
	}, stdBuses())

	got := Compare(g, c)
	d, ok := findDiff(got, DiffStrip, "cam99-1", fieldPresence)
	if !ok {
		t.Fatalf("Compare = %v, want a presence diff on cam99-1 rather than it being skipped", got)
	}
	if d.Golden != absentValue {
		t.Errorf("Golden = %q, want %q for an appeared strip", d.Golden, absentValue)
	}
	if d.Severity != SeverityCritical {
		t.Errorf("severity = %q, want %q (default routing includes aux1)", d.Severity, SeverityCritical)
	}
}

func TestCompare_StripDisappearedIsReportedAndCriticalWhenItCarriedMaster(t *testing.T) {
	g := mkSnapshot([]Strip{
		mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster, BusAux1),
		mkStrip("cam23-1", "cam23", "", false, BusMaster, BusAux2),
	}, stdBuses())
	c := mkSnapshot([]Strip{
		mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster, BusAux1),
	}, stdBuses())

	got := Compare(g, c)
	d, ok := findDiff(got, DiffStrip, "cam23-1", fieldPresence)
	if !ok {
		t.Fatalf("Compare = %v, want a presence diff on cam23-1 rather than it being skipped", got)
	}
	if d.Current != absentValue {
		t.Errorf("Current = %q, want %q for a disappeared strip", d.Current, absentValue)
	}
	if d.Severity != SeverityCritical {
		t.Errorf("severity = %q, want %q (strip carried master)", d.Severity, SeverityCritical)
	}
}

func TestCompare_StripDisappearedWithoutMasterIsWarning(t *testing.T) {
	g := mkSnapshot([]Strip{mkStrip("aux-src-1", "aux-src", "", false, BusAux2)}, stdBuses())
	c := mkSnapshot(nil, stdBuses())

	got := Compare(g, c)
	d, ok := findDiff(got, DiffStrip, "aux-src-1", fieldPresence)
	if !ok {
		t.Fatalf("Compare = %v, want a presence diff on aux-src-1", got)
	}
	if d.Severity != SeverityWarn {
		t.Errorf("severity = %q, want %q", d.Severity, SeverityWarn)
	}
}

// ---------------------------------------------------------------------------
// Compare: fader tolerance
// ---------------------------------------------------------------------------

func TestCompare_FaderTolerance(t *testing.T) {
	// Deltas are deliberately not placed exactly on the FaderToleranceDB
	// boundary: 0.1 has no exact float64 representation, so a delta computed
	// as exactly FaderToleranceDB and compared with <= is at the mercy of
	// which way that representation rounds — a real boundary in the
	// production comparison, but not a meaningful thing for a table test to
	// pin down. These deltas sit clearly on one side or the other instead.
	tests := []struct {
		name     string
		delta    float64
		wantDiff bool
	}{
		{"well within tolerance: no diff", 0.02, false},
		{"just under tolerance: no diff", FaderToleranceDB - 0.02, false},
		{"just over tolerance: warns", FaderToleranceDB + 0.05, true},
		{"well over tolerance: warns", 3.0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := mkStrip("cam22-1", "cam22", "", false, BusMaster)
			g.Fader = [2]float64{-2.0, -2.0}
			c := mkStrip("cam22-1", "cam22", "", false, BusMaster)
			c.Fader = [2]float64{-2.0 + tt.delta, -2.0}

			got := Compare(mkSnapshot([]Strip{g}, stdBuses()), mkSnapshot([]Strip{c}, stdBuses()))
			_, ok := findDiff(got, DiffStrip, "cam22-1", fieldFader)
			if ok != tt.wantDiff {
				t.Errorf("fader diff present = %v, want %v (diffs: %v)", ok, tt.wantDiff, got)
			}
		})
	}
}

func TestCompare_FaderEnabledChangeIsReportedEvenWithoutGainChange(t *testing.T) {
	g := mkStrip("cam22-1", "cam22", "", false, BusMaster)
	g.FaderEnabled = [2]bool{false, false}
	c := mkStrip("cam22-1", "cam22", "", false, BusMaster)
	c.FaderEnabled = [2]bool{true, true}
	// Gain unchanged — only the enabled flag differs.

	got := Compare(mkSnapshot([]Strip{g}, stdBuses()), mkSnapshot([]Strip{c}, stdBuses()))
	if _, ok := findDiff(got, DiffStrip, "cam22-1", fieldFader); !ok {
		t.Errorf("Compare = %v, want a %q diff when FaderEnabled changes with gain held constant", got, fieldFader)
	}
}

// ---------------------------------------------------------------------------
// Compare: display name is informational
// ---------------------------------------------------------------------------

func TestCompare_DisplayNameChangeIsInfoSeverity(t *testing.T) {
	g := mkSnapshot([]Strip{mkStrip("cam22-1", "cam22", "cam22-1", false, BusMaster)}, stdBuses())
	c := mkSnapshot([]Strip{mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster)}, stdBuses())

	got := Compare(g, c)
	d, ok := findDiff(got, DiffStrip, "cam22-1", fieldDisplayName)
	if !ok {
		t.Fatalf("Compare = %v, want a %q diff", got, fieldDisplayName)
	}
	if d.Severity != SeverityInfo {
		t.Errorf("severity = %q, want %q", d.Severity, SeverityInfo)
	}
}

// ---------------------------------------------------------------------------
// Compare: overall ordering
// ---------------------------------------------------------------------------

func TestCompare_ResultOrderedMostSevereFirst(t *testing.T) {
	gStrip := mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster)
	gStrip.Fader = [2]float64{0, 0}
	cStrip := mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster, BusAux1) // critical: gained aux1
	cStrip.Fader = [2]float64{5, 5}                                                  // warn: fader moved

	got := Compare(mkSnapshot([]Strip{gStrip}, stdBuses()), mkSnapshot([]Strip{cStrip}, stdBuses()))
	if len(got) < 2 {
		t.Fatalf("Compare = %v, want at least 2 diffs (one critical, one warn)", got)
	}
	if got[0].Severity != SeverityCritical {
		t.Errorf("first diff severity = %q, want %q (most severe first)", got[0].Severity, SeverityCritical)
	}
	if got[len(got)-1].Severity == SeverityCritical {
		t.Errorf("last diff severity = %q, want the warn diff to sort after the critical one", got[len(got)-1].Severity)
	}
}

// ---------------------------------------------------------------------------
// Diff.Render
// ---------------------------------------------------------------------------

func TestDiffRender_MuteSentences(t *testing.T) {
	tests := []struct {
		name   string
		d      Diff
		wantIn []string
	}{
		{
			name:   "strip newly unmuted",
			d:      Diff{Kind: DiffStrip, Target: "cam22-1", Label: "CLAUDE-COMMS", Field: fieldMuted, Golden: "true", Current: "false"},
			wantIn: []string{"CLAUDE-COMMS (cam22-1)", "UNMUTED", "it was muted"},
		},
		{
			name:   "bus newly muted",
			d:      Diff{Kind: DiffBus, Target: "master", Label: "master (PGM)", Field: fieldMuted, Golden: "false", Current: "true"},
			wantIn: []string{"master (PGM)", "MUTED", "it was not"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.d.Render()
			for _, want := range tt.wantIn {
				if !strings.Contains(got, want) {
					t.Errorf("Render() = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

func TestDiffRender_PresenceSentences(t *testing.T) {
	appeared := Diff{Kind: DiffStrip, Target: "cam99-1", Label: "NEW-CAM", Field: fieldPresence,
		Golden: absentValue, Current: "present, routed to master (PGM), aux1 (CLN - clean feed)"}
	if got := appeared.Render(); !strings.Contains(got, "APPEARED") {
		t.Errorf("Render() = %q, want it to say APPEARED", got)
	}

	gone := Diff{Kind: DiffStrip, Target: "cam23-1", Label: "cam23-1", Field: fieldPresence,
		Golden: "present, routed to master (PGM)", Current: absentValue}
	if got := gone.Render(); !strings.Contains(got, "DISAPPEARED") {
		t.Errorf("Render() = %q, want it to say DISAPPEARED", got)
	}
}

func TestDiffRender_GenericFieldFallback(t *testing.T) {
	d := Diff{Kind: DiffStrip, Target: "cam22-1", Label: "CLAUDE-COMMS", Field: fieldFader,
		Golden: "-2.0 dB, -2.0 dB (L enabled, R enabled)", Current: "5.0 dB, 5.0 dB (L enabled, R enabled)"}
	got := d.Render()
	for _, want := range []string{"CLAUDE-COMMS (cam22-1)", "fader", "was -2.0 dB", "is now 5.0 dB"} {
		if !strings.Contains(got, want) {
			t.Errorf("Render() = %q, want it to contain %q", got, want)
		}
	}
}

func TestDiffRender_SatisfiesStringer(t *testing.T) {
	d := Diff{Kind: DiffStrip, Target: "cam22-1", Label: "CLAUDE-COMMS", Field: fieldMuted, Golden: "true", Current: "false"}
	if d.String() != d.Render() {
		t.Errorf("String() = %q, Render() = %q, want them equal", d.String(), d.Render())
	}
}

// ---------------------------------------------------------------------------
// Golden persistence
// ---------------------------------------------------------------------------

// withAppData points %APPDATA% at a fresh temp directory for the duration of
// the test, matching internal/config's own test helper, so GoldenPath/
// SaveGolden/LoadGolden exercise a real filesystem without touching the
// developer's actual %APPDATA%\WSLComms.
func withAppData(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	return dir
}

func TestGoldenPath(t *testing.T) {
	dir := withAppData(t)

	got, err := GoldenPath()
	if err != nil {
		t.Fatalf("GoldenPath() error = %v", err)
	}
	want := filepath.Join(dir, "WSLComms", "mixer-golden.json")
	if got != want {
		t.Errorf("GoldenPath() = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "WSLComms")); !os.IsNotExist(err) {
		t.Errorf("GoldenPath() must not create the WSLComms directory, stat err = %v", err)
	}
}

func TestLoadGolden_MissingReturnsErrNoGolden(t *testing.T) {
	withAppData(t)

	_, err := LoadGolden()
	if !errors.Is(err, ErrNoGolden) {
		t.Errorf("LoadGolden() on a missing file: error = %v, want ErrNoGolden", err)
	}
}

func TestSaveLoadGolden_RoundTrip(t *testing.T) {
	withAppData(t)

	want := mkSnapshot(
		[]Strip{
			mkStrip("cam22-1", "cam22", "CLAUDE-COMMS", false, BusMaster, BusAux1),
			mkStrip("cam22-2", "cam22", "CLAUDE-COMMS", true, BusMaster),
		},
		stdBuses(),
	)
	want.Strips[0].Fader = [2]float64{-1.574803113937378, -1.574803113937378}
	want.Strips[0].FaderEnabled = [2]bool{true, true}
	want.Strips[0].FollowSources = []string{"cam22"}
	want.Strips[0].SubChMode = "ST_W"

	if err := SaveGolden(want); err != nil {
		t.Fatalf("SaveGolden() error = %v", err)
	}

	got, err := LoadGolden()
	if err != nil {
		t.Fatalf("LoadGolden() error = %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadGolden() after SaveGolden() =\n%+v\nwant\n%+v", got, want)
	}
	// Belt and suspenders: the round-tripped snapshot must also compare equal
	// to the original via this package's own Compare, which is the property
	// that actually matters to a caller.
	if diffs := Compare(want, got); len(diffs) != 0 {
		t.Errorf("Compare(original, round-tripped) = %v, want no diffs", diffs)
	}
}

func TestSaveGolden_NoTempFileLeftBehind(t *testing.T) {
	dir := withAppData(t)

	if err := SaveGolden(mkSnapshot(nil, stdBuses())); err != nil {
		t.Fatalf("SaveGolden() error = %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "WSLComms"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != GoldenFileName {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory contents = %v, want exactly [%s]", names, GoldenFileName)
	}
}

func TestSaveGolden_OverwritesAtomically(t *testing.T) {
	withAppData(t)

	first := mkSnapshot([]Strip{mkStrip("cam22-1", "cam22", "first", false, BusMaster)}, stdBuses())
	if err := SaveGolden(first); err != nil {
		t.Fatalf("first SaveGolden() error = %v", err)
	}

	second := mkSnapshot([]Strip{mkStrip("cam22-1", "cam22", "second", false, BusMaster)}, stdBuses())
	if err := SaveGolden(second); err != nil {
		t.Fatalf("second SaveGolden() error = %v", err)
	}

	got, err := LoadGolden()
	if err != nil {
		t.Fatalf("LoadGolden() error = %v", err)
	}
	if len(got.Strips) != 1 || got.Strips[0].DisplayName != "second" {
		t.Errorf("LoadGolden() after overwrite = %+v, want the second snapshot", got)
	}
}

func TestLoadGolden_InvalidJSONReturnsError(t *testing.T) {
	dir := withAppData(t)
	confDir := filepath.Join(dir, "WSLComms")
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, GoldenFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadGolden()
	if err == nil {
		t.Fatal("LoadGolden() with malformed JSON: error = nil, want non-nil")
	}
	if errors.Is(err, ErrNoGolden) {
		t.Error("LoadGolden() with malformed JSON returned ErrNoGolden, want a parse error")
	}
}

// ---------------------------------------------------------------------------
// Integration against the real captured live frame
// ---------------------------------------------------------------------------

// liveFrameFileForGolden mirrors state_test.go's liveFrameFile constant. It
// is redefined rather than shared because this file must keep working even
// if state_test.go (owned by WP-M2) is edited or removed.
const liveFrameFileForGolden = "../m2lx/testdata/switcher_status-live-2026-07-31.json"

func loadLiveSnapshotForGolden(t *testing.T) Snapshot {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(liveFrameFileForGolden))
	if err != nil {
		t.Skipf("live frame not available (%v)", err)
	}
	snap, _, err := ParseSnapshotWithWarnings(raw)
	if err != nil {
		t.Skipf("live frame did not parse (%v); this is WP-M2's territory, not WP-M3's", err)
	}
	return snap
}

// TestCompare_LiveFrameAgainstItselfProducesNothing guards against Compare
// finding false drift in the one frame this whole package was designed
// around.
func TestCompare_LiveFrameAgainstItselfProducesNothing(t *testing.T) {
	snap := loadLiveSnapshotForGolden(t)
	if diffs := Compare(snap, snap); len(diffs) != 0 {
		t.Errorf("Compare(live, live) = %v, want no diffs", diffs)
	}
}

// TestCompare_RemovingCommentaryFromCleanFeedIsTheDiffThisPackageExistsFor
// is the whole point of this work package stated as a test: cam22-1 /
// CLAUDE-COMMS is measured, in the live capture, routed to aux1 (the clean
// feed). A golden snapshot taken with that corrected — aux1 removed from
// cam22-1 — must show, as CRITICAL, the exact moment that correction is
// undone: the strip regaining aux1.
func TestCompare_RemovingCommentaryFromCleanFeedIsTheDiffThisPackageExistsFor(t *testing.T) {
	live := loadLiveSnapshotForGolden(t)

	const target = "cam22-1"
	var found bool
	corrected := make([]Strip, len(live.Strips))
	copy(corrected, live.Strips)
	for i, s := range corrected {
		if s.Name == target {
			if !containsBus(s.Outputs, BusAux1) {
				t.Skipf("%s is not currently routed to aux1 in the live capture; the measured fact this test checks no longer holds", target)
			}
			found = true
			corrected[i].Outputs = removeBus(s.Outputs, BusAux1)
		}
	}
	if !found {
		t.Skipf("%s not present in the live capture", target)
	}

	golden := Snapshot{Strips: corrected, Buses: live.Buses, TakenAt: live.TakenAt}

	// current (live) has aux1 restored relative to golden (corrected) —
	// exactly the "preset recall restores the default" failure mode.
	got := Compare(golden, live)
	d, ok := findDiff(got, DiffStrip, target, fieldOutputs)
	if !ok {
		t.Fatalf("Compare(corrected, live) = %v, want a %q diff on %s", got, fieldOutputs, target)
	}
	if d.Severity != SeverityCritical {
		t.Errorf("severity = %q, want %q", d.Severity, SeverityCritical)
	}
	if !strings.Contains(d.Render(), "CLEAN FEED") {
		t.Errorf("Render() = %q, want it to name the CLEAN FEED", d.Render())
	}
	t.Logf("live-frame diff, in the operator's terms: %s", d.Render())
}
