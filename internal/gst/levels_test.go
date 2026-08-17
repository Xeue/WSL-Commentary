//go:build !cgo || gststub

// Tests for the level-meter model, and the Gate A source guard that keeps the
// real build's bus handler honest about WHICH level element a reading came from.
//
// Owner: WP-DECKLINK tier 2, with levels.go.
//
// The tag is the same one gst_stub_test.go carries, and for the same two
// reasons. It is what Gate A compiles, so everything here runs with no
// GStreamer present and under -race; and the source guards below read
// gst_cgo.go AS TEXT through parseSource/funcBody, which is a helper that lives
// in that file. Nothing here needs a pipeline, a bus or a message: the whole
// point of levels.go is that the decisions are pure Go, and the whole point of
// a source guard is that it can check a file it cannot link.
//
// What is given up by testing the cgo half as text: these guards prove the
// handler still SAYS the right thing, not that GStreamer still DOES it. The
// doing was measured by hand — the numbers are recorded in levels.go's comments
// — and a source guard is what stops the next refactor quietly undoing it
// between measurements.

package gst

import (
	"math"
	"strings"
	"testing"
)

// TestLevelMessagesAreAttributedToTheirSourceElement is THE guard for this
// tier's one-line defect, and it is written to be hard to undo rather than easy
// to read.
//
// Before this tier, onBusMessage matched a level message on its structure name
// alone and never asked which element posted it. Every level element in the
// process posts a structure named "level", so that handler matched all of them.
// With one element it was sloppy; with the second one this tier makes possible
// — chlevel on the capture device's sixteen unpositioned channels, alevel on
// the mixed-down stereo actually being encoded — it is a silent cross-wire, and
// it was MEASURED: 39 level messages a second, every one matching, so the
// programme meter would alternate between a sixteen-entry frame and a two-entry
// frame twenty times a second each.
//
// The guard checks the ORDER as well as the presence, because presence alone is
// satisfied by a source lookup that happens after the callback has already been
// handed the frame. TestPipelineDescriptionMetersWhatIsEncoded is the precedent
// for that shape: assert the indices, not just the substrings.
func TestLevelMessagesAreAttributedToTheirSourceElement(t *testing.T) {
	// THE CAPTURE BUS, because that is where a level element now posts. alevel and
	// chlevel are upstream of the proxysinks, so the send pipeline's handler has no
	// level case at all — and a guard that went on reading it would have passed for
	// ever by no longer being able to see the code it is about.
	fset, file := parseSource(t, captureCgoSourceFile)
	body := funcBody(t, fset, file, "cgoCapture", "onBusMessage")

	start := strings.Index(body, "case gogst.MessageElement:")
	if start < 0 {
		t.Fatal("onBusMessage no longer has a gogst.MessageElement case; level messages would " +
			"not be read at all and the meters would sit empty at Gate B only")
	}
	element := body[start:]

	structure := strings.Index(element, "levelStructureName")
	source := strings.Index(element, "msg.Source()")
	kind := strings.Index(element, "levelKindForSource(")
	deliver := strings.Index(element, "c.onLevels.Load()")

	if structure < 0 {
		t.Error("the level case no longer matches on levelStructureName; it would read the " +
			"element messages of everything else in the pipeline as level reports")
	}
	if source < 0 {
		t.Fatal("the level case no longer calls msg.Source(). This is the defect this tier " +
			"exists to fix: a level message matched on its structure name alone belongs to ANY " +
			"level element in the process, and the second one this tier makes possible would " +
			"feed the programme meter the capture device's raw channels every other frame")
	}
	if kind < 0 {
		t.Fatal("the level case no longer routes through levelKindForSource. The mapping " +
			"between element names and meters is the policy, it is pure Go so that it is " +
			"testable here, and a comparison open-coded in the handler is a policy Gate A " +
			"cannot see")
	}
	if deliver < 0 {
		t.Fatal("the level case no longer loads the OnLevels callback; the input meters would " +
			"never move on a real build")
	}
	if !(source < deliver && kind < deliver) {
		t.Error("the source of a level message must be named and classified BEFORE the frame " +
			"reaches the OnLevels callback. Naming it afterwards satisfies every substring " +
			"check above and still delivers the wrong meter's reading to the programme meter")
	}
	if !(structure < source) {
		t.Error("the structure name must be tested BEFORE msg.Source(): the structure test is a " +
			"string compare on a name the message already carries, and naming the source costs " +
			"a cgo call, a GObject lock and a string. Reversing them pays that for every " +
			"element message any element in this pipeline posts, on a streaming thread")
	}
}

// TestEveryLevelElementInThePipelineIsRouted binds the two halves together: the
// names in the parse string and the names the handler will accept.
//
// It is what makes the guard above survive the neighbouring work package adding
// the per-channel level element. A level element whose name levelKindForSource
// does not know is an element whose messages are dropped — the meter stays
// empty and nothing at Gate B says why — and a name that agrees with nothing is
// exactly the kind of mistake a parse string built by string concatenation
// invites.
func TestEveryLevelElementInThePipelineIsRouted(t *testing.T) {
	// BOTH level elements live in the CAPTURE description now: alevel and chlevel
	// are upstream of the proxysinks, and the send pipeline has no meter at all
	// (a meter below the seam would read the encoder's input rather than the
	// microphone). The routing they are checked against is cgoCapture's bus
	// handler, which is where levelKindForSource is consulted.
	body := captureDescriptionSource(t)

	// BOTH ELEMENTS ARE NAMED FROM CONSTANTS, so the names are read out of the
	// SOURCE EXPRESSION rather than out of a rendered literal. That is what the
	// split made necessary and it is a stronger check than the old one: the old
	// version scanned for the literal text `level name=alevel`, which a build that
	// named the element from a constant would have satisfied with zero matches and
	// no failure at all had the `found == 0` guard not existed.
	const marker = `level name="+`
	found := 0
	for i := strings.Index(body, marker); i >= 0; {
		rest := body[i+len(marker):]
		ident := rest
		if cut := strings.IndexAny(ident, " +\n\""); cut >= 0 {
			ident = ident[:cut]
		}
		found++

		// The constant's VALUE, resolved from levels.go, because that is the name
		// GStreamer will give the element and the name onBusMessage will route on.
		var name string
		switch ident {
		case "levelElementName":
			name = levelElementName
		case "channelLevelElementName":
			name = channelLevelElementName
		default:
			t.Errorf("the parse string builds a level element named from %q, which this guard "+
				"cannot resolve. Every level element must be named from a constant in levels.go, "+
				"beside levelKindForSource, so that the name that is BUILT and the name that is "+
				"ROUTED cannot drift", ident)
		}
		if name != "" && levelKindForSource(name) == levelKindUnknown {
			t.Errorf("the parse string builds a level element named %q, which "+
				"levelKindForSource classifies as unknown: every message it posts would be "+
				"dropped by onBusMessage and the meter behind it would never move. Add the "+
				"name to levels.go, do not loosen the routing", name)
		}
		next := strings.Index(rest, marker)
		if next < 0 {
			break
		}
		i = i + len(marker) + next
	}
	if found != 2 {
		t.Fatalf("the capture description builds %d level elements, want 2 — the programme meter "+
			"and the per-channel picker. Zero means the input meters have nothing to measure and "+
			"the levels event will never fire on a real build", found)
	}
	// The programme meter is the on-air one and is not optional.
	if !strings.Contains(body, marker+"levelElementName") {
		t.Errorf("the parse string no longer names the programme meter from levelElementName "+
			"(%q); onBusMessage routes on that exact name and would drop every frame it posts",
			levelElementName)
	}
}

// TestLevelKindForSourceRoutesOnlyTheTwoKnownElements pins the policy itself.
// Anything unrecognised — including the empty string go-gst hands back for a
// message whose source has already been disposed — must classify as unknown and
// therefore be dropped, because an unattributable level frame is the exact
// cross-wire the source test exists to prevent.
func TestLevelKindForSourceRoutesOnlyTheTwoKnownElements(t *testing.T) {
	for _, tt := range []struct {
		name string
		want levelKind
	}{
		{levelElementName, levelKindProgramme},
		{channelLevelElementName, levelKindChannels},
		{"", levelKindUnknown},
		{"level0", levelKindUnknown},        // gst_parse_launch's own default name
		{"alevel2", levelKindUnknown},       // a near miss, and near is not a match
		{"audioconvert0", levelKindUnknown}, // not a level element at all
		{"returnlevel", levelKindUnknown},   // another pipeline in this process
	} {
		if got := levelKindForSource(tt.name); got != tt.want {
			t.Errorf("levelKindForSource(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
	if levelElementName == channelLevelElementName {
		t.Fatal("the two level elements have the same name; msg.Source() cannot tell them " +
			"apart and the routing is decorative")
	}
}

// TestLevelListToDBCarriesEveryChannelOfASixteenChannelFrame is the widening,
// tested at the width that motivated it.
//
// The values are the MEASURED ones: sixteen audiotestsrc at 3 dB steps,
// interleaved into one unpositioned sixteen-channel stream, came back from the
// level element as peaks descending in 3 dB steps IN INPUT ORDER. What is
// asserted here is the property the mapping UI depends on — the count survives
// and the order survives, so array index i is input channel i.
func TestLevelListToDBCarriesEveryChannelOfASixteenChannelFrame(t *testing.T) {
	const channels = 16
	vals := make([]any, channels)
	for i := range vals {
		vals[i] = -3 * float64(i)
	}

	got := levelListToDB(vals)
	if len(got) != channels {
		t.Fatalf("levelListToDB kept %d of %d channels; a picker cannot show a channel it was "+
			"never told about", len(got), channels)
	}
	for i, v := range got {
		if want := -3 * float64(i); v != want {
			t.Fatalf("channel %d = %v, want %v: the order must survive, or the operator maps "+
				"the wrong channel and hears silence on air", i, v, want)
		}
	}
}

// TestLevelsFromEnforcesTheEqualLengthInvariant covers the one guarantee every
// renderer is allowed to rely on: the two arrays are the same length.
//
// It cannot happen with today's level element — MEASURED sixteen and sixteen on
// the DeckLink shape — and that is exactly why it is enforced at the producer.
// A UI that walks peak and indexes rms with the same subscript is the obvious
// way to draw sixteen strips, and it is an out-of-range panic on a GStreamer
// streaming thread the first time the two disagree.
func TestLevelsFromEnforcesTheEqualLengthInvariant(t *testing.T) {
	peak := []any{-1.0, -2.0, -3.0, -4.0}
	rms := []any{-10.0, -20.0}

	got, ok := levelsFrom(peak, rms)
	if !ok {
		t.Fatal("levelsFrom rejected a frame it can honestly deliver two channels of")
	}
	if len(got.PeakDB) != 2 || len(got.RMSDB) != 2 {
		t.Fatalf("levelsFrom returned %d peaks and %d RMS values, want 2 and 2: a mismatch must "+
			"resolve to the SHORTER, because a channel we have half a measurement for is a "+
			"channel we cannot honestly draw", len(got.PeakDB), len(got.RMSDB))
	}
	if got.PeakDB[0] != -1 || got.RMSDB[0] != -10 {
		t.Fatalf("levelsFrom truncated from the wrong end: got peak %v rms %v", got.PeakDB, got.RMSDB)
	}

	// And the other way round, because "take the shorter" is only correct if it
	// is symmetric.
	got, ok = levelsFrom(rms, peak)
	if !ok || len(got.PeakDB) != 2 || len(got.RMSDB) != 2 {
		t.Fatalf("levelsFrom is not symmetric in its two arguments: ok=%v peaks=%d rms=%d",
			ok, len(got.PeakDB), len(got.RMSDB))
	}
}

// TestLevelsFromCapsAnAbsurdChannelCount pins the truncation, and the choice of
// truncation over rejection. A meter showing the first sixty-four of sixty-five
// channels is a working meter with a documented edge; an empty one is no meter.
func TestLevelsFromCapsAnAbsurdChannelCount(t *testing.T) {
	vals := make([]any, levelMaxChannels+8)
	for i := range vals {
		vals[i] = -6.0
	}

	got, ok := levelsFrom(vals, vals)
	if !ok {
		t.Fatal("an over-long frame was rejected outright; the meter would show nothing at all")
	}
	if len(got.PeakDB) != levelMaxChannels || len(got.RMSDB) != levelMaxChannels {
		t.Fatalf("levelsFrom returned %d/%d channels, want %d",
			len(got.PeakDB), len(got.RMSDB), levelMaxChannels)
	}
}

// TestLevelsFromRejectsAFrameItCannotDraw covers the degradation this file
// prefers everywhere: silent absence, never invented numbers. An absent or
// empty field means the meters are left exactly as they are.
func TestLevelsFromRejectsAFrameItCannotDraw(t *testing.T) {
	for _, tt := range []struct {
		name      string
		peak, rms []any
	}{
		{"both absent", nil, nil},
		{"peak absent", nil, []any{-1.0}},
		{"rms absent", []any{-1.0}, nil},
		{"both empty", []any{}, []any{}},
	} {
		if _, ok := levelsFrom(tt.peak, tt.rms); ok {
			t.Errorf("%s: levelsFrom reported a usable frame; the meters would be redrawn from "+
				"nothing", tt.name)
		}
	}
}

// TestLevelsFromClampsEveryChannel checks the floor still applies at sixteen
// channels — the mono-microphone case that produced -699.99999984363217 dBFS on
// this machine is not special to channel 1 of 2, and a DeckLink with fourteen
// unpatched inputs is fourteen channels of exactly that.
func TestLevelsFromClampsEveryChannel(t *testing.T) {
	peak := []any{math.Inf(-1), -699.99999984363217, math.NaN(), -6.0, math.Inf(1)}
	got, ok := levelsFrom(peak, peak)
	if !ok {
		t.Fatal("levelsFrom rejected a frame of clampable values")
	}
	want := []float64{levelSilenceDB, levelSilenceDB, levelSilenceDB, -6, 0}
	for i := range want {
		if got.PeakDB[i] != want[i] {
			t.Errorf("channel %d clamped to %v, want %v", i, got.PeakDB[i], want[i])
		}
	}
}

// TestSilentLevelsForKeepsTheMeterAtItsWidth is about what the operator sees at
// the end of a match, which is the reason the zero-frame exists at all.
//
// A two-channel zero-frame sent to a sixteen-strip meter changes the array
// length, and under the wire contract in levels.go a changed length means "the
// pipeline was rebuilt, lay yourself out again" — so fourteen channels would
// VANISH at the moment the session ended instead of falling silent.
func TestSilentLevelsForKeepsTheMeterAtItsWidth(t *testing.T) {
	for _, channels := range []int{1, 2, 8, 16, levelMaxChannels} {
		l := silentLevelsFor(channels)
		if len(l.PeakDB) != channels || len(l.RMSDB) != channels {
			t.Fatalf("silentLevelsFor(%d) has %d/%d channels", channels, len(l.PeakDB), len(l.RMSDB))
		}
		for i := range l.PeakDB {
			if l.PeakDB[i] != levelSilenceDB || l.RMSDB[i] != levelSilenceDB {
				t.Fatalf("silentLevelsFor(%d) channel %d is not at the floor: %v/%v",
					channels, i, l.PeakDB[i], l.RMSDB[i])
			}
		}
	}

	// Out of range is clamped, never empty: an empty frame reads as "no usable
	// reading" to every consumer and leaves the meter frozen at its last value,
	// which is the thing the zero-frame exists to prevent.
	if got := len(silentLevelsFor(0).PeakDB); got != 1 {
		t.Errorf("silentLevelsFor(0) produced %d channels, want 1", got)
	}
	if got := len(silentLevelsFor(-4).PeakDB); got != 1 {
		t.Errorf("silentLevelsFor(-4) produced %d channels, want 1", got)
	}
	if got := len(silentLevelsFor(levelMaxChannels * 4).PeakDB); got != levelMaxChannels {
		t.Errorf("silentLevelsFor(huge) produced %d channels, want %d", got, levelMaxChannels)
	}
}

// TestSilentLevelsIsUnchangedByTheWidening is the "this is a widening, not a
// replacement" half of the contract, checked rather than asserted: silentLevels
// now delegates to silentLevelsFor, and must still produce exactly the stereo
// zero-frame the on-air build produces.
func TestSilentLevelsIsUnchangedByTheWidening(t *testing.T) {
	l := silentLevels()
	if len(l.PeakDB) != levelStubChannels || len(l.RMSDB) != levelStubChannels {
		t.Fatalf("silentLevels has %d/%d channels, want %d", len(l.PeakDB), len(l.RMSDB), levelStubChannels)
	}
	for i := range l.PeakDB {
		if l.PeakDB[i] != levelSilenceDB || l.RMSDB[i] != levelSilenceDB {
			t.Fatalf("silentLevels channel %d is not at the floor", i)
		}
	}
}

// TestStubLevelsAtIsUnchangedByTheWidening recomputes the two-channel stub
// waveform from its own definition and requires the refactored code to agree
// value for value.
//
// stubLevelAt's phase arithmetic moved into stubLevelForPhase so that the
// N-channel synthesiser could use a different phase rule. That is precisely the
// kind of change that alters a waveform by one step and is never noticed, and
// the two-channel numbers are what the on-air Windows build's development
// meters show.
func TestStubLevelsAtIsUnchangedByTheWidening(t *testing.T) {
	triangle := func(step, channel int) float64 {
		phase := (step + channel*stubLevelRightOffset) % stubLevelPeriod
		half := stubLevelPeriod / 2
		span := float64(stubLevelHighDB - stubLevelLowDB)
		if phase < half {
			return stubLevelLowDB + span*float64(phase)/float64(half)
		}
		return stubLevelHighDB - span*float64(phase-half)/float64(half)
	}

	for step := 0; step < stubLevelPeriod*3; step++ {
		l := stubLevelsAt(step)
		if len(l.PeakDB) != levelStubChannels {
			t.Fatalf("step %d: stubLevelsAt has %d channels, want %d", step, len(l.PeakDB), levelStubChannels)
		}
		for ch := range l.PeakDB {
			want := clampLevelDB(triangle(step, ch))
			if l.PeakDB[ch] != want {
				t.Fatalf("step %d channel %d: peak %v, want %v", step, ch, l.PeakDB[ch], want)
			}
			if wantRMS := clampLevelDB(triangle(step, ch) - stubLevelRMSBelowPeakDB); l.RMSDB[ch] != wantRMS {
				t.Fatalf("step %d channel %d: rms %v, want %v", step, ch, l.RMSDB[ch], wantRMS)
			}
		}
	}
}

// TestStubChannelLevelsAtNeverMovesTwoChannelsTogether is the reason the
// per-channel stub does not reuse stubLevelAt's phase rule.
//
// That rule staggers each channel by a QUARTER of the period, which wraps every
// four channels: at sixteen channels 0, 4, 8 and 12 would move in perfect
// lockstep. Four groups of four identical bars is exactly the appearance of a
// picker that has drawn one channel four times — so the fake would be
// manufacturing, by construction, the failure the fake exists to expose. The
// test asserts the property rather than the arithmetic: over a full period, no
// two of the sixteen channels have the same waveform.
func TestStubChannelLevelsAtNeverMovesTwoChannelsTogether(t *testing.T) {
	const channels = 16

	frames := make([]Levels, stubLevelPeriod)
	for step := range frames {
		frames[step] = stubChannelLevelsAt(step, channels)
		if len(frames[step].PeakDB) != channels || len(frames[step].RMSDB) != channels {
			t.Fatalf("step %d: %d/%d channels, want %d",
				step, len(frames[step].PeakDB), len(frames[step].RMSDB), channels)
		}
	}

	for a := 0; a < channels; a++ {
		for b := a + 1; b < channels; b++ {
			same := true
			for step := range frames {
				if frames[step].PeakDB[a] != frames[step].PeakDB[b] {
					same = false
					break
				}
			}
			if same {
				t.Fatalf("stub channels %d and %d are identical at every step of the period: a "+
					"sixteen-strip picker fed this fake cannot be told apart from one that has "+
					"drawn the same channel twice, which is the failure the fake is for", a, b)
			}
		}
	}

	// The old rule is what this replaces; state the failure in the test so the
	// next person to "simplify" the two synthesisers into one sees the cost.
	if stubLevelAt(0, 0) != stubLevelAt(0, stubLevelPeriod/stubLevelRightOffset) {
		t.Skip("stubLevelAt's stagger no longer wraps; re-derive this note")
	}
}

// TestStubChannelLevelsAtClampsItsChannelCount covers the edges by the same
// argument as silentLevelsFor: never an empty frame, never an unbounded one.
func TestStubChannelLevelsAtClampsItsChannelCount(t *testing.T) {
	if got := len(stubChannelLevelsAt(0, 0).PeakDB); got != 1 {
		t.Errorf("stubChannelLevelsAt(0, 0) produced %d channels, want 1", got)
	}
	if got := len(stubChannelLevelsAt(0, -3).PeakDB); got != 1 {
		t.Errorf("stubChannelLevelsAt(0, -3) produced %d channels, want 1", got)
	}
	if got := len(stubChannelLevelsAt(0, levelMaxChannels*2).PeakDB); got != levelMaxChannels {
		t.Errorf("stubChannelLevelsAt(0, huge) produced %d channels, want %d", got, levelMaxChannels)
	}
	// A negative step is the ticker's zero, not a panic and not a negative
	// phase; stubLevelAt has always clamped it and this must match.
	if stubChannelLevelsAt(-5, 4).PeakDB[0] != stubChannelLevelsAt(0, 4).PeakDB[0] {
		t.Error("a negative step must clamp to zero, as it does in stubLevelAt")
	}
}

// TestChannelLevelIntervalIsSlowerThanTheProgrammeMeter pins the deliberate
// decision rather than the number: the picker meter answers "which of these
// sixteen moves when I talk" and runs slower than the programme meter, which
// answers "is what is going to air clipping" and stays at 50 ms.
//
// The measurements behind the choice are in levels.go beside the constant. What
// is guarded here is the RELATIONSHIP, because that is what a later change to
// either value could quietly invert.
func TestChannelLevelIntervalIsSlowerThanTheProgrammeMeter(t *testing.T) {
	const programmeIntervalNs = 50000000 // the parse string's own value

	if channelLevelIntervalNs <= programmeIntervalNs {
		t.Errorf("the per-channel meter's interval is %d ns, at or below the programme meter's "+
			"%d ns: sixteen channels at the programme meter's rate is eight times its wire "+
			"traffic on the same bridge, for a meter answering a question that does not need "+
			"the rate", channelLevelIntervalNs, programmeIntervalNs)
	}
	// Slower has a limit too. Below about five frames a second a bar stops
	// reading as movement and starts reading as a stutter, and the operator
	// cannot tell a talking channel from a noisy one.
	if channelLevelIntervalNs > 200000000 {
		t.Errorf("the per-channel meter's interval is %d ns, slower than five frames a second: "+
			"the operator is watching for which bar MOVES, and that stops being visible",
			channelLevelIntervalNs)
	}
	// The parse string's programme interval is guarded separately by
	// TestTheProgrammeMeterMeasuresWhatCrossesTheSeam; this only needs them to be
	// talking about the same number.
	if body := captureDescriptionSource(t); !strings.Contains(body, "interval=50000000") {
		t.Error("the programme meter's interval in the parse string is no longer 50 ms; the " +
			"comparison above is against a number that has moved")
	}
}

// BenchmarkLevelsFrom is what the interval decision was sized against on the
// application side: the per-message cost of turning one level element message
// into a deliverable frame, at the two widths that matter. It is the work that
// happens on a GStreamer streaming thread, once per message, for every meter in
// the pipeline.
//
// Measured on the port machine, 2026-08-16:
//
//	BenchmarkLevelsFrom/stereo-10     22 ns/op    32 B/op   2 allocs/op
//	BenchmarkLevelsFrom/sixteen-10    63 ns/op   256 B/op   2 allocs/op
//
// Eight times the channels for under three times the time and always two
// allocations, because the slices are sized once and filled. At the per-channel
// meter's ten frames a second that is well under a microsecond of CPU and about
// 2.6 kB of garbage per second — which is the finding that MOVED the interval
// argument off this package and onto the webview bridge. See
// channelLevelIntervalNs.
func BenchmarkLevelsFrom(b *testing.B) {
	for _, channels := range []int{2, 16} {
		vals := make([]any, channels)
		for i := range vals {
			vals[i] = -12.5
		}
		b.Run(map[bool]string{true: "stereo", false: "sixteen"}[channels == 2], func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, ok := levelsFrom(vals, vals); !ok {
					b.Fatal("levelsFrom rejected the benchmark frame")
				}
			}
		})
	}
}
