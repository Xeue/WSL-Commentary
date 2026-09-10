//go:build dev || production || bindings

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wslcomms/internal/gst"
	"wslcomms/internal/tsprobe"
)

func TestRigRequested(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	none := env(nil)
	if rigRequested(nil, none) {
		t.Fatal("nothing asked for the rig")
	}
	if !rigRequested(nil, env(map[string]string{rigEnv: "1"})) {
		t.Fatal("WSLCOMMS_RIG=1 must ask for the rig")
	}
	for _, arg := range []string{"--rig", "-rig", "rig", "--RIG"} {
		if !rigRequested([]string{"x", arg}, none) {
			t.Errorf("argument %q must ask for the rig", arg)
		}
	}
	if rigRequested([]string{"--rigid"}, none) {
		t.Fatal("--rigid is not --rig")
	}
	if got := rigSeconds(env(map[string]string{rigSecsEnv: "15"})); got != 15 {
		t.Fatalf("rigSeconds = %d, want 15", got)
	}
	if got := rigSeconds(env(map[string]string{rigSecsEnv: "-3"})); got != rigDefaultSecs {
		t.Fatalf("rigSeconds(-3) = %d, want the default %d", got, rigDefaultSecs)
	}
}

func TestRigSelectLogFilesTakesTheLastWeekNewestFirstWithinBudget(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	mk := func(name string, age time.Duration, size int) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	mk("today.log", time.Hour, 100)
	mk("yesterday.log", 30*time.Hour, 100)
	mk("old.log", 10*24*time.Hour, 100)
	mk("huge.log", 2*time.Hour, 1000)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := rigSelectLogFiles(dir, now, 7*24*time.Hour, 250)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, filepath.Base(p))
	}
	// today (100) fits; huge (1000) would break the budget and is skipped...
	// no: the newest-first walk stops at the first file that breaks the budget,
	// so what is kept is what was touched most recently, up to the budget.
	if len(names) == 0 || names[0] != "today.log" {
		t.Fatalf("the newest file must come first, got %v", names)
	}
	for _, n := range names {
		if n == "old.log" {
			t.Fatalf("a ten-day-old log was selected: %v", names)
		}
	}
	var total int
	for _, p := range got {
		st, _ := os.Stat(p)
		total += int(st.Size())
	}
	if total > 250 && len(got) > 1 {
		t.Fatalf("budget exceeded with more than one file: %d bytes in %v", total, names)
	}
}

func TestRigDissectFromReadsTheStagesAndTheBus(t *testing.T) {
	res := gst.DiagnosticResult{
		Elapsed:    2 * time.Second,
		EndedEarly: true,
		Stages: []gst.DiagnosticStage{
			{Name: "raw", Buffers: 4000},
			{Name: "demuxed", Buffers: 900, Flags: map[string]uint64{"DISCONT": 3, "GAP": 1}},
			{Name: "parsed", Buffers: 100},
			{Name: "decoded", Buffers: 100, Flags: map[string]uint64{"CORRUPTED": 7}},
		},
		Bus: map[string]uint64{
			"tsdemux: CONTINUITY: Mismatch packet N, stream N": 12,
			"h265parse: broken/invalid nal Type: N":            2,
			"something else":                                   5,
		},
	}
	d := rigDissectFrom("file", "avdec_h265", res)
	if d.Decoded != 100 || d.Corrupted != 7 || d.Discont != 3 || d.Gap != 1 || d.Parsed != 100 || d.Demuxed != 900 || d.Received != 4000 {
		t.Fatalf("stage numbers misread: %+v", d)
	}
	if d.Continuity != 12 || d.Broken != 2 {
		t.Fatalf("bus census misread: continuity %d broken %d", d.Continuity, d.Broken)
	}
	if fps := d.FPS(); fps != 50 {
		t.Fatalf("FPS = %v, want 50 (100 frames in 2 s)", fps)
	}
	if !strings.Contains(d.line(), "ended at end of stream") {
		t.Fatalf("an early end must be stated: %s", d.line())
	}
}

func TestRigFindingsStateWhatTheNumbersSupport(t *testing.T) {
	probe := &tsprobe.Summary{Verdict: tsprobe.VerdictFlaggedOnly, FlaggedCC: 40}
	live := &rigDissect{Decoded: 2900, Elapsed: 60 * time.Second, Corrupted: 31, Continuity: 55}
	slow := &rigDissect{Decoder: "avdec_h265", Decoded: 3000, Elapsed: 100 * time.Second, Corrupted: 0}
	noHW := &rigDissect{Err: "could not create d3d11h265dec"}

	got := strings.Join(rigFindings(probe, live, slow, noHW), "\n")
	for _, want := range []string{
		"arrives intact",
		"CANNOT KEEP UP",
		"30.0 fps",
		"31 corrupted frames LIVE but 0 replaying",
		"holes are made on this laptop's receive path",
		"No usable hardware HEVC decoder",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("findings lack %q:\n%s", want, got)
		}
	}

	// A fast machine with holes on the wire says so, and does not claim a
	// decode fault it cannot see.
	fast := &rigDissect{Decoder: "avdec_h265", Decoded: 3000, Elapsed: 20 * time.Second}
	holes := &tsprobe.Summary{Verdict: tsprobe.VerdictRealHoles, RealCC: 9, Span: 60 * time.Second}
	got = strings.Join(rigFindings(holes, nil, fast, nil), "\n")
	if !strings.Contains(got, "ARRIVES WITH 9 REAL HOLES") || !strings.Contains(got, "headroom") {
		t.Errorf("findings for a fast machine with a broken stream:\n%s", got)
	}
	if strings.Contains(got, "CANNOT KEEP UP") {
		t.Errorf("a machine decoding at 150 fps was told it cannot keep up:\n%s", got)
	}
}

func TestRigSummaryTextNamesEveryReadingAndNeverThePassphrase(t *testing.T) {
	r := &rig{
		stamp:   "20260910-120000",
		machine: []string{"cpu: test"},
		target: rigTarget{PresetID: "matchg", PresetName: "Match G", M2LXHost: "h", Host: "h", Port: 40504,
			LatencyMs: 120, PBKeyLen: 16, Passphrase: "s3cret-passphrase", HasPassphrase: true},
		probe:    &tsprobe.Summary{Verdict: tsprobe.VerdictClean, VideoPID: 0x41, VideoCodec: "hevc"},
		live:     &rigDissect{Label: "live", Decoded: 10, Elapsed: time.Second},
		fileAV:   &rigDissect{Label: "file av", Decoded: 10, Elapsed: time.Second},
		fileHW:   &rigDissect{Label: "file hw", Err: "no decoder"},
		findings: []string{"finding one"},
		problems: []string{"problem one"},
		files:    []string{"cap.ts"},
	}
	text := rigSummaryText(r)
	for _, want := range []string{"THE FOUR READINGS", "1. probe", "2. live", "3. file av", "4. file hw: NOT AVAILABLE", "finding one", "problem one", "passphrase present true"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary lacks %q", want)
		}
	}
	if strings.Contains(text, "s3cret") {
		t.Fatal("the summary carries the passphrase")
	}
}
