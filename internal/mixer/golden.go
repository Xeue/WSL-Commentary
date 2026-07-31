// Owner: WP-M3. This file implements the golden-snapshot comparison declared
// as a stub in mixer.go (Compare) and the persistence of a golden Snapshot to
// %APPDATA%\WSLComms\mixer-golden.json.
//
// ===================== WHY THIS FILE EXISTS ==================================
//
// EVERY STRIP DEFAULTS TO ["master","aux1","aux2"] (see mixer.go). A preset
// recall, a re-added input, or a restored device profile can silently put a
// strip back on that default, and aux1 IS the clean feed — so commentary can
// re-enter the clean feed with nobody having touched anything. Comparing the
// live desk against a snapshot the operator approved is how that becomes
// visible before a match instead of during one, which is the entire reason
// Compare exists and why its severity ranking below is not decorative.
//
// ===================== ON THE Compare REDECLARATION ==========================
//
// mixer.go — the seam file, owned by WP-M0 — still carries a stub definition
// of `func Compare(golden, current Snapshot) []Diff` at the moment this file
// was written, and Compare below redeclares that exact name and signature.
// This was a deliberate choice, not an oversight, made after checking the
// sibling work package actually landed in this tree: controller.go (WP-M1)
// redeclares NewController and every Command.Envelope method the same way,
// against the same stubs, in a brand new file, touching nothing in mixer.go.
// That is the established convention for this multi-work-package build, and
// mixer.go is being pruned of stubs from outside this work package as each
// sibling's real implementation lands — NewController and the five Envelope
// stubs were already gone from mixer.go by the time this file was finished,
// pruned by something other than WP-M1 itself. Compare's stub is left for the
// same external process to remove; this file does not touch mixer.go.
//
// This package's own paths for this work package are exactly this file and
// golden_test.go — mixer.go is not among them, and its contract types
// (Snapshot, Strip, BusState, Diff, Severity, DiffKind, Bus) are used exactly
// as declared, never edited, per this work package's rule that a contract
// problem is reported, not fixed. Until mixer.go's Compare stub is pruned,
// `go build ./internal/mixer/...` reports a redeclaration; see this work
// package's report for the isolated build/test evidence that Compare below,
// on its own, is correct.
package mixer

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FaderToleranceDB is how far a fader (channel or bus) must move, in dB,
// before Compare reports it as drift.
//
// This is a CHOSEN value, not a measured one — nothing in the captured live
// frame states a meaningful-versus-noise threshold for fader movement. 0.1 dB
// was picked to match the one calibration precision this codebase does have
// measured evidence for: internal/config's ReturnGainDB comment records the
// KVS return level tracking the M2L-X bus meter to within 0.1 dB. If that
// turns out too tight in practice (an operator's own hand on a fader can move
// it by less than 0.1 dB when nudging), raise this constant; it is
// deliberately the only place a fader tolerance is written down.
const FaderToleranceDB = 0.1

// GoldenFileName is the golden snapshot's file name inside %APPDATA%\WSLComms.
const GoldenFileName = "mixer-golden.json"

// goldenAppDataDirName mirrors internal/config.AppDataDirName. It is
// duplicated rather than imported so this package keeps its existing
// property of having no dependency on internal/config; keep it in step with
// that constant if the application ever renames its data directory.
const goldenAppDataDirName = "WSLComms"

// ErrNoGolden is returned by LoadGolden when no golden snapshot has been
// saved yet.
//
// This is a normal, expected state — first run, or an operator who has not
// yet approved a desk layout — and callers MUST render it as "no golden
// saved", never fall back to an empty Snapshot and silently report "no
// differences": Compare would then have nothing wrong to find and the drawer
// would tell the operator their desk is safe when it has checked nothing at
// all. See frontend/src/ui/mixer/contract.js's getGolden, which documents the
// same requirement on the JS side of this contract.
var ErrNoGolden = errors.New("mixer: no golden snapshot saved")

// GoldenPath returns the absolute path of the saved golden mixer snapshot,
// %APPDATA%\WSLComms\mixer-golden.json. It does not create the directory.
//
// This mirrors internal/config.Path(): os.UserConfigDir resolves %APPDATA%
// on Windows, which lets tests substitute a temp directory by setting the
// APPDATA environment variable, exactly as internal/config's tests do.
func GoldenPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("mixer: resolving user config directory: %w", err)
	}
	return filepath.Join(dir, goldenAppDataDirName, GoldenFileName), nil
}

// SaveGolden writes snap as the golden mixer snapshot, atomically.
//
// Discipline matches internal/config.(*Config).Save exactly, and for the
// same reason: a temp file is created in the SAME directory as the target
// (so the rename below is same-volume, not a copy), written, Sync'd to
// stable storage, closed, and then moved over the target with os.Rename. On
// Windows this is MoveFileEx with MOVEFILE_REPLACE_EXISTING, a single
// directory operation, so a reader — including this process crashing mid
// write, or a power cut during a match — always observes either the old
// complete golden file or the new complete one, never a truncated one. This
// file is the reference an operator trusts to mean "the desk is not putting
// commentary in the clean feed"; a torn write that silently answered "no
// golden saved" or, worse, parsed into a half-written Snapshot would defeat
// the entire point of keeping one.
func SaveGolden(snap Snapshot) error {
	path, err := GoldenPath()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mixer: creating %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("mixer: encoding golden snapshot: %w", err)
	}

	tmp, err := os.CreateTemp(dir, GoldenFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("mixer: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("mixer: writing %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("mixer: syncing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("mixer: closing %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("mixer: renaming %s to %s: %w", tmpPath, path, err)
	}
	renamed = true
	return nil
}

// LoadGolden reads the saved golden mixer snapshot from GoldenPath().
//
// It returns ErrNoGolden — not a zero Snapshot with a nil error — when the
// file does not exist. See ErrNoGolden for why that distinction matters.
func LoadGolden() (Snapshot, error) {
	path, err := GoldenPath()
	if err != nil {
		return Snapshot{}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, ErrNoGolden
		}
		return Snapshot{}, fmt.Errorf("mixer: reading %s: %w", path, err)
	}

	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("mixer: parsing %s: %w", path, err)
	}
	return snap, nil
}

// ---------------------------------------------------------------------------
// Field names. These are the exact strings Compare writes into Diff.Field;
// naming them once here is what keeps a diff's Field value and Render's
// switch on it from drifting apart.
// ---------------------------------------------------------------------------

const (
	fieldPresence      = "presence"
	fieldOutputs       = "outputs"
	fieldPFLOutputs    = "pflOutputs"
	fieldMuted         = "muted"
	fieldFader         = "fader"
	fieldSubChMode     = "subChMode"
	fieldFollow        = "follow"
	fieldFollowSources = "followSources"
	fieldDisplayName   = "displayName"
	fieldChannelCount  = "channelCount"
)

// absentValue and noneValue are the two ways Compare renders "nothing here",
// and they mean different things: absentValue is Diff's own documented
// rendering for a field with no data at all (a strip or bus that does not
// exist on one side of the comparison), and noneValue is an empty-but-present
// list — a strip genuinely routed to zero buses is a real state, not missing
// data, and must not be confused with the former.
const (
	absentValue = "(absent)"
	noneValue   = "(none)"
)

// cleanFeedLabel and programmeLabel are BusLabel's rendering of the two buses
// with egress, computed once from BusLabel itself (never hand-typed) so that
// Diff.Render's clean-feed and programme call-outs can never drift from the
// single table BusLabel reads. See Diff.Render for how they are used:
// TestBusLabelsAreDistinctAndNonEmpty in mixer_test.go guarantees neither is
// a substring of any other bus's label, which is what makes that use safe.
var (
	cleanFeedLabel = BusLabel(BusAux1)
	programmeLabel = BusLabel(BusMaster)
)

// Compare reports how the current mixer state differs from a saved golden
// one. This is the real implementation of the Compare declared in mixer.go —
// see this file's top comment for why it is redeclared here rather than
// edited in place.
//
// Direction is fixed: golden is what the operator approved, current is what
// the desk is doing now. Strips and buses are compared independently and the
// results concatenated before one final stable sort by severity, so that
// within a severity the order is strips (sorted by wire name) then buses (in
// AllBuses order) — deterministic across runs against the same two
// snapshots, which is what lets a UI render this list directly without it
// reordering itself.
//
// Level, PeakHold and Metered are excluded from every comparison this
// function performs, on both strips and buses — see compareOneStrip and
// compareOneBus. They change every update cycle; reporting them as drift
// would swamp this list with noise nobody would read, which is worse than no
// list at all.
func Compare(golden, current Snapshot) []Diff {
	diffs := compareStrips(golden, current)
	diffs = append(diffs, compareBuses(golden, current)...)

	sort.SliceStable(diffs, func(i, j int) bool {
		return severityRank(diffs[i].Severity) < severityRank(diffs[j].Severity)
	})
	return diffs
}

// severityRank orders severities most-severe-first for Compare's final sort.
// Anything not one of the three known severities sorts last rather than
// panicking, so an unrecognised value is visible, not fatal.
func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 0
	case SeverityWarn:
		return 1
	case SeverityInfo:
		return 2
	default:
		return 3
	}
}

// ---------------------------------------------------------------------------
// Strips
// ---------------------------------------------------------------------------

// compareStrips reports every strip-level difference between golden and
// current, keyed by strip wire name and visited in sorted order so the
// result does not depend on either Snapshot's own Strips ordering.
//
// A strip present on only one side is reported as ONE presence Diff (see
// stripPresenceDiff) rather than skipped, per this package's explicit
// requirement: a strip that has vanished from the frame is not "nothing to
// report", and a strip that has newly appeared arrives at the mixer's
// default ["master","aux1","aux2"] routing — already in the clean feed.
func compareStrips(golden, current Snapshot) []Diff {
	gMap := stripsByName(golden.Strips)
	cMap := stripsByName(current.Strips)

	names := unionStripNames(golden.Strips, current.Strips)

	var diffs []Diff
	for _, name := range names {
		g, gOK := gMap[name]
		c, cOK := cMap[name]
		switch {
		case gOK && !cOK:
			diffs = append(diffs, stripPresenceDiff(g, false))
		case !gOK && cOK:
			diffs = append(diffs, stripPresenceDiff(c, true))
		default:
			diffs = append(diffs, compareOneStrip(g, c)...)
		}
	}
	return diffs
}

func stripsByName(strips []Strip) map[string]Strip {
	m := make(map[string]Strip, len(strips))
	for _, s := range strips {
		m[s.Name] = s
	}
	return m
}

// unionStripNames returns every strip name appearing in either slice, sorted,
// each exactly once. Sorting is what makes compareStrips' output order
// independent of parse order.
func unionStripNames(a, b []Strip) []string {
	seen := make(map[string]bool, len(a)+len(b))
	names := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if !seen[s.Name] {
			seen[s.Name] = true
			names = append(names, s.Name)
		}
	}
	for _, s := range b {
		if !seen[s.Name] {
			seen[s.Name] = true
			names = append(names, s.Name)
		}
	}
	sort.Strings(names)
	return names
}

// stripLabel is the operator-facing name for a strip: its DisplayName, or its
// parent Input when DisplayName is empty. Mirrors the fallback documented on
// Strip.DisplayName — never fall back to blank.
func stripLabel(s Strip) string {
	if s.DisplayName != "" {
		return s.DisplayName
	}
	return s.Input
}

// stripPresenceDiff reports a strip that exists in only one snapshot. s is
// the strip as it reads on the side where it DOES exist; appeared is true
// when that side is current (the strip is new) and false when that side is
// golden (the strip is gone).
//
// Severity: an appeared strip already routed to BusAux1 is CRITICAL — it
// arrived at the clean feed, the exact failure mode this package exists to
// catch, and per this package's requirement that is unconditional regardless
// of the strip's mute state. A disappeared strip that was routed to
// BusMaster is CRITICAL for the symmetric reason documented on
// SeverityCritical in mixer.go: that is programme audio disappearing.
// Everything else is a WARNING: a new or removed strip is always worth an
// operator's attention, but neither is "audio is now audible somewhere it
// was not approved to be".
func stripPresenceDiff(s Strip, appeared bool) Diff {
	label := stripLabel(s)
	summary := fmt.Sprintf("present, routed to %s", renderBuses(s.Outputs))
	if s.Muted {
		summary += "; muted"
	}

	d := Diff{Kind: DiffStrip, Target: s.Name, Label: label, Field: fieldPresence}
	if appeared {
		d.Golden = absentValue
		d.Current = summary
		d.Severity = stripPresenceSeverity(true, s.Outputs)
	} else {
		d.Golden = summary
		d.Current = absentValue
		d.Severity = stripPresenceSeverity(false, s.Outputs)
	}
	return d
}

func stripPresenceSeverity(appeared bool, outputs []Bus) Severity {
	if appeared {
		if containsBus(outputs, BusAux1) {
			return SeverityCritical
		}
		return SeverityWarn
	}
	if containsBus(outputs, BusMaster) {
		return SeverityCritical
	}
	return SeverityWarn
}

// compareOneStrip reports every field-level difference between two readings
// of the SAME strip. Level, PeakHold and Metered are deliberately absent from
// this function: they change every update cycle and reporting them as drift
// would swamp the list with noise nobody would read, so this package excludes
// them from comparison entirely rather than merely ranking them SeverityInfo.
func compareOneStrip(g, c Strip) []Diff {
	label := stripLabel(c)
	var diffs []Diff

	if !busSetEqual(g.Outputs, c.Outputs) {
		diffs = append(diffs, Diff{
			Kind: DiffStrip, Target: c.Name, Label: label, Field: fieldOutputs,
			Golden: renderBuses(g.Outputs), Current: renderBuses(c.Outputs),
			Severity: routingSeverity(g.Outputs, c.Outputs),
		})
	}

	if !busSetEqual(g.PFLOutputs, c.PFLOutputs) {
		// PFL routing feeds only mon4, a monitor bus with no egress of its
		// own — never critical, whatever changed.
		diffs = append(diffs, Diff{
			Kind: DiffStrip, Target: c.Name, Label: label, Field: fieldPFLOutputs,
			Golden: renderBuses(g.PFLOutputs), Current: renderBuses(c.PFLOutputs),
			Severity: SeverityWarn,
		})
	}

	if g.Muted != c.Muted {
		diffs = append(diffs, Diff{
			Kind: DiffStrip, Target: c.Name, Label: label, Field: fieldMuted,
			Golden: renderBool(g.Muted), Current: renderBool(c.Muted),
			Severity: stripMuteSeverity(g.Muted, c.Muted),
		})
	}

	if !faderPairEqual(g.Fader, c.Fader) || g.FaderEnabled != c.FaderEnabled {
		diffs = append(diffs, Diff{
			Kind: DiffStrip, Target: c.Name, Label: label, Field: fieldFader,
			Golden: renderStripFader(g.Fader, g.FaderEnabled), Current: renderStripFader(c.Fader, c.FaderEnabled),
			Severity: SeverityWarn,
		})
	}

	if g.SubChMode != c.SubChMode {
		diffs = append(diffs, Diff{
			Kind: DiffStrip, Target: c.Name, Label: label, Field: fieldSubChMode,
			Golden: valueOrAbsent(g.SubChMode), Current: valueOrAbsent(c.SubChMode),
			Severity: SeverityWarn,
		})
	}

	if g.Follow != c.Follow {
		diffs = append(diffs, Diff{
			Kind: DiffStrip, Target: c.Name, Label: label, Field: fieldFollow,
			Golden: renderBool(g.Follow), Current: renderBool(c.Follow),
			Severity: SeverityWarn,
		})
	}

	if !stringSliceEqual(g.FollowSources, c.FollowSources) {
		diffs = append(diffs, Diff{
			Kind: DiffStrip, Target: c.Name, Label: label, Field: fieldFollowSources,
			Golden: renderStringList(g.FollowSources), Current: renderStringList(c.FollowSources),
			Severity: SeverityWarn,
		})
	}

	if g.DisplayName != c.DisplayName {
		// A display name edit changes nothing anybody hears — mixer.go's
		// SeverityInfo doc names this exact case as its example.
		diffs = append(diffs, Diff{
			Kind: DiffStrip, Target: c.Name, Label: label, Field: fieldDisplayName,
			Golden: valueOrAbsent(g.DisplayName), Current: valueOrAbsent(c.DisplayName),
			Severity: SeverityInfo,
		})
	}

	return diffs
}

// routingSeverity classifies a strip's routing change from the raw bus
// lists. A strip that gained BusAux1 is CRITICAL unconditionally — that is
// the clean feed, and this is the rule the whole package exists to enforce.
// A strip that lost BusMaster is CRITICAL for the symmetric reason on
// SeverityCritical in mixer.go: programme audio disappearing. Everything
// else — losing aux1, gaining or losing any other bus — is a WARNING: it
// changed what the strip does, but not in the direction that puts audio
// somewhere it was not approved to be.
func routingSeverity(golden, current []Bus) Severity {
	aux1Gained := !containsBus(golden, BusAux1) && containsBus(current, BusAux1)
	masterLost := containsBus(golden, BusMaster) && !containsBus(current, BusMaster)
	if aux1Gained || masterLost {
		return SeverityCritical
	}
	return SeverityWarn
}

// stripMuteSeverity implements the CRITICAL rule from this package's task
// exactly: a strip that WAS muted and now is not is CRITICAL — audio that was
// silent is now live. The reverse (a live strip going silent) is a WARNING:
// worth an operator's attention, but the safe direction, not the dangerous
// one.
func stripMuteSeverity(golden, current bool) Severity {
	if golden && !current {
		return SeverityCritical
	}
	return SeverityWarn
}

// ---------------------------------------------------------------------------
// Buses
// ---------------------------------------------------------------------------

// compareBuses reports every bus-level difference between golden and
// current, visited in AllBuses order (falling back to name order for a bus
// this package does not recognise, which should not happen against a real
// frame but must not panic if it does).
func compareBuses(golden, current Snapshot) []Diff {
	gMap := busesByName(golden.Buses)
	cMap := busesByName(current.Buses)

	names := unionBusNames(golden.Buses, current.Buses)

	var diffs []Diff
	for _, name := range names {
		g, gOK := gMap[name]
		c, cOK := cMap[name]
		switch {
		case gOK && !cOK:
			diffs = append(diffs, busPresenceDiff(g, false))
		case !gOK && cOK:
			diffs = append(diffs, busPresenceDiff(c, true))
		default:
			diffs = append(diffs, compareOneBus(g, c)...)
		}
	}
	return diffs
}

func busesByName(buses []BusState) map[Bus]BusState {
	m := make(map[Bus]BusState, len(buses))
	for _, b := range buses {
		m[b.Name] = b
	}
	return m
}

func unionBusNames(a, b []BusState) []Bus {
	seen := make(map[Bus]bool, len(a)+len(b))
	names := make([]Bus, 0, len(a)+len(b))
	for _, s := range a {
		if !seen[s.Name] {
			seen[s.Name] = true
			names = append(names, s.Name)
		}
	}
	for _, s := range b {
		if !seen[s.Name] {
			seen[s.Name] = true
			names = append(names, s.Name)
		}
	}
	sort.SliceStable(names, func(i, j int) bool {
		oi, oj := busOrderIndex(names[i]), busOrderIndex(names[j])
		if oi != oj {
			return oi < oj
		}
		return names[i] < names[j]
	})
	return names
}

func busOrderIndex(b Bus) int {
	for i, ab := range AllBuses {
		if ab == b {
			return i
		}
	}
	return len(AllBuses)
}

// busPresenceDiff reports a bus that exists in only one snapshot. In
// practice the seven buses in AllBuses are fixed by the DSP node, so this
// path should never fire against a real device — it exists so that a
// firmware change removing or renaming a bus is reported rather than
// silently ignored. A vanished or newly-appeared master or aux1 — the two
// buses with egress — is CRITICAL for the same reason a strip gaining aux1
// or losing master is; any other bus is a WARNING.
func busPresenceDiff(b BusState, appeared bool) Diff {
	label := BusLabel(b.Name)
	summary := "present, not muted"
	if b.Muted {
		summary = "present, muted"
	}

	d := Diff{Kind: DiffBus, Target: string(b.Name), Label: label, Field: fieldPresence}
	sev := SeverityWarn
	if b.Name == BusMaster || b.Name == BusAux1 {
		sev = SeverityCritical
	}
	if appeared {
		d.Golden = absentValue
		d.Current = summary
	} else {
		d.Golden = summary
		d.Current = absentValue
	}
	d.Severity = sev
	return d
}

// compareOneBus reports every field-level difference between two readings of
// the SAME bus. Level, PeakHold and Metered are excluded for the same reason
// as on strips — see compareOneStrip.
func compareOneBus(g, c BusState) []Diff {
	label := BusLabel(c.Name)
	target := string(c.Name)
	var diffs []Diff

	if g.Muted != c.Muted {
		diffs = append(diffs, Diff{
			Kind: DiffBus, Target: target, Label: label, Field: fieldMuted,
			Golden: renderBool(g.Muted), Current: renderBool(c.Muted),
			Severity: busMuteSeverity(g.Muted, c.Muted),
		})
	}

	faderChanged := g.FaderPresent != c.FaderPresent ||
		(g.FaderPresent && c.FaderPresent && !faderScalarEqual(g.Fader, c.Fader))
	if faderChanged {
		diffs = append(diffs, Diff{
			Kind: DiffBus, Target: target, Label: label, Field: fieldFader,
			Golden: renderBusFader(g.Fader, g.FaderPresent), Current: renderBusFader(c.Fader, c.FaderPresent),
			Severity: SeverityWarn,
		})
	}

	if g.ChannelCount != c.ChannelCount {
		diffs = append(diffs, Diff{
			Kind: DiffBus, Target: target, Label: label, Field: fieldChannelCount,
			Golden: fmt.Sprintf("%d", g.ChannelCount), Current: fmt.Sprintf("%d", c.ChannelCount),
			Severity: SeverityWarn,
		})
	}

	return diffs
}

// busMuteSeverity implements the CRITICAL rule from this package's task
// exactly: a bus that was NOT muted and now is CRITICAL — mixer.go's own
// example is master going silent, which is programme audio disappearing.
// The reverse (a muted bus being unmuted) is a WARNING.
func busMuteSeverity(golden, current bool) Severity {
	if !golden && current {
		return SeverityCritical
	}
	return SeverityWarn
}

// ---------------------------------------------------------------------------
// Rendering: Diff values (bools, bus lists, dB, absence)
// ---------------------------------------------------------------------------

func containsBus(list []Bus, b Bus) bool {
	for _, x := range list {
		if x == b {
			return true
		}
	}
	return false
}

func busSetEqual(a, b []Bus) bool {
	am := toBusSet(a)
	bm := toBusSet(b)
	if len(am) != len(bm) {
		return false
	}
	for k := range am {
		if !bm[k] {
			return false
		}
	}
	return true
}

func toBusSet(list []Bus) map[Bus]bool {
	m := make(map[Bus]bool, len(list))
	for _, b := range list {
		m[b] = true
	}
	return m
}

func stringSliceEqual(a, b []string) bool {
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

func faderPairEqual(a, b [2]float64) bool {
	return math.Abs(a[0]-b[0]) <= FaderToleranceDB && math.Abs(a[1]-b[1]) <= FaderToleranceDB
}

func faderScalarEqual(a, b float64) bool {
	return math.Abs(a-b) <= FaderToleranceDB
}

// renderBool renders a bool the way Diff's own doc comment specifies:
// "true" / "false", never Go's %v spelling of anything else.
func renderBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// renderBuses renders a bus list as labels joined with ", " in AllBuses
// order, never the order the list happened to arrive in — see Diff's doc
// comment. An empty-but-present list (a strip genuinely routed nowhere) is
// noneValue, distinct from absentValue, which means the field itself is
// missing (see the absentValue/noneValue doc comment above). Buses this
// package does not recognise are appended, sorted, after the known ones,
// rather than dropped silently.
func renderBuses(buses []Bus) string {
	if len(buses) == 0 {
		return noneValue
	}
	set := toBusSet(buses)
	parts := make([]string, 0, len(buses))
	for _, b := range AllBuses {
		if set[b] {
			parts = append(parts, BusLabel(b))
			delete(set, b)
		}
	}
	if len(set) > 0 {
		var rest []string
		for b := range set {
			rest = append(rest, string(b))
		}
		sort.Strings(rest)
		for _, b := range rest {
			parts = append(parts, BusLabel(Bus(b)))
		}
	}
	return strings.Join(parts, ", ")
}

// renderStringList renders a general string list (e.g. FollowSources) as a
// comma-joined string, or noneValue when empty.
func renderStringList(list []string) string {
	if len(list) == 0 {
		return noneValue
	}
	return strings.Join(list, ", ")
}

// valueOrAbsent renders a string field that is genuinely absent-when-empty
// (SubChMode and DisplayName both document this — see Strip.DisplayName) as
// absentValue rather than an empty string, per Diff's doc comment: "absent"
// must never render as "".
func valueOrAbsent(s string) string {
	if s == "" {
		return absentValue
	}
	return s
}

// renderDB renders one gain value as Diff's doc comment specifies: one
// decimal place with the unit, e.g. "-1.6 dB".
func renderDB(v float64) string {
	return fmt.Sprintf("%.1f dB", v)
}

// renderStripFader renders a strip's per-channel fader gain AND its
// per-channel enabled flag, because a gain shown without the enabled flag is,
// per Strip.FaderEnabled's doc comment, "a number that may not be applied to
// any audio" — and a strip half enabled is exactly the half-live condition
// that comment says must not be hidden, so the two channels are rendered
// separately rather than collapsed into one "enabled"/"disabled" word.
func renderStripFader(gain [2]float64, enabled [2]bool) string {
	return fmt.Sprintf("%s, %s (L %s, R %s)",
		renderDB(gain[0]), renderDB(gain[1]), onOff(enabled[0]), onOff(enabled[1]))
}

// renderBusFader renders a bus's scalar output-fader gain, or absentValue
// when FaderPresent is false — see BusState.FaderPresent's doc comment on why
// a missing fader must not be drawn as a gain of 0.
func renderBusFader(gain float64, present bool) string {
	if !present {
		return absentValue
	}
	return renderDB(gain)
}

func onOff(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

// ---------------------------------------------------------------------------
// Diff.Render: the human-readable form
// ---------------------------------------------------------------------------

// Render returns a short, human-readable sentence describing this Diff in the
// operator's own terms, e.g.
//
//	CLAUDE-COMMS (cam22-1) is now routed to the CLEAN FEED (aux1); it was not
//
// rather than a struct dump — that sentence beats "outputs: master ->
// master, aux1" for the one person this package exists to warn.
//
// Render works only from the Diff's own already-rendered Golden/Current
// strings, never from typed bus lists — see the Diff doc comment in mixer.go
// on why values cross this boundary as strings. The clean-feed and programme
// call-outs below detect their case by checking for cleanFeedLabel /
// programmeLabel as a substring of the rendered bus list; TestBusLabelsAreDistinctAndNonEmpty
// in mixer_test.go guarantees no bus label is a substring of another, which
// is what makes that safe.
func (d Diff) Render() string {
	switch d.Field {
	case fieldPresence:
		return d.renderPresence()
	case fieldOutputs:
		return d.renderRouting()
	case fieldMuted:
		return d.renderMuted()
	default:
		return fmt.Sprintf("%s (%s) %s changed: was %s; is now %s", d.Label, d.Target, d.Field, d.Golden, d.Current)
	}
}

// String makes Diff satisfy fmt.Stringer with the same rendering as Render,
// so a Diff can be logged or %v-formatted directly during development without
// producing a struct dump.
func (d Diff) String() string { return d.Render() }

func (d Diff) renderPresence() string {
	if d.Golden == absentValue {
		return fmt.Sprintf("%s (%s) has APPEARED: %s", d.Label, d.Target, d.Current)
	}
	return fmt.Sprintf("%s (%s) has DISAPPEARED: was %s", d.Label, d.Target, d.Golden)
}

func (d Diff) renderRouting() string {
	goldenHasAux1 := strings.Contains(d.Golden, cleanFeedLabel)
	currentHasAux1 := strings.Contains(d.Current, cleanFeedLabel)
	switch {
	case currentHasAux1 && !goldenHasAux1:
		return fmt.Sprintf("%s (%s) is now routed to the CLEAN FEED (aux1); it was not", d.Label, d.Target)
	case goldenHasAux1 && !currentHasAux1:
		return fmt.Sprintf("%s (%s) is no longer routed to the CLEAN FEED (aux1); it was", d.Label, d.Target)
	}

	goldenHasMaster := strings.Contains(d.Golden, programmeLabel)
	currentHasMaster := strings.Contains(d.Current, programmeLabel)
	switch {
	case goldenHasMaster && !currentHasMaster:
		return fmt.Sprintf("%s (%s) is no longer routed to PROGRAMME (master); it was", d.Label, d.Target)
	case currentHasMaster && !goldenHasMaster:
		return fmt.Sprintf("%s (%s) is now routed to PROGRAMME (master); it was not", d.Label, d.Target)
	}

	return fmt.Sprintf("%s (%s) routing changed: was routed to %s; is now routed to %s", d.Label, d.Target, d.Golden, d.Current)
}

func (d Diff) renderMuted() string {
	if d.Golden == "true" && d.Current == "false" {
		return fmt.Sprintf("%s (%s) is now UNMUTED; it was muted", d.Label, d.Target)
	}
	return fmt.Sprintf("%s (%s) is now MUTED; it was not", d.Label, d.Target)
}
