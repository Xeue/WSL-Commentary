//go:build !cgo || gststub

// coughmute_test.go is the Gate A half of the cough mute: the behaviour of the
// stub twin, and source guards over the cgo twin that Gate A can read but not
// run.
//
// The source guards are not a substitute for the hardware measurement —
// coughmute.go carries that — they are the thing that keeps the hardware
// measurement TRUE. Every one of them is about a change that would leave the
// application reporting a mute that is not in force, or a meter reporting a
// voice that is not on air, with nothing at either gate going red.

package gst

import (
	"os"
	"strings"
	"testing"
)

// readRepoFile reads a file from outside this package by relative path. It is
// used by exactly one guard below and is a t.Fatal rather than a skip on
// purpose: a repository checkout in which build/ is absent is not a case worth
// tolerating quietly, since the thing being checked is what gets shipped.
func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// startedMuteStub is a running StubPipeline, since every test below needs one
// and the required options are noise.
func startedMuteStub(t *testing.T, opts PipelineOpts) *StubPipeline {
	t.Helper()
	if opts.SlatePath == "" {
		opts.SlatePath = "slate.png"
	}
	if opts.AudioDeviceID == "" && opts.AudioCaptureID == "" {
		opts.AudioDeviceID = "{0.0.1.00000000}.{stub}"
	}
	p := NewStubPipeline()
	if err := p.Start(opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	return p
}

// TestCommentaryStartsUnmuted pins the default, which is the state every seat
// that has never seen a cough button is in. The zero value of PipelineOpts must
// put the commentator on air.
func TestCommentaryStartsUnmuted(t *testing.T) {
	p := startedMuteStub(t, PipelineOpts{})
	if p.CommentaryMuted() {
		t.Fatal("a pipeline started with the zero PipelineOpts reports the commentary MUTED. " +
			"The default has to be on air: a seat that never touches a cough button must sound " +
			"exactly as it did before the feature existed")
	}
}

// TestStartMutedIsMutedBeforeAnythingRuns is the option that carries a mute
// across a rebuild. The value must be in force the moment Start returns, not
// applied by something later, because the whole point of it is that a session
// rebuilt after a fatal never puts a muted commentator back on air.
func TestStartMutedIsMutedBeforeAnythingRuns(t *testing.T) {
	p := startedMuteStub(t, PipelineOpts{MuteCommentary: true})
	if !p.CommentaryMuted() {
		t.Fatal("PipelineOpts.MuteCommentary did not take. A pipeline rebuilt to replace one " +
			"that died muted would put the commentator back on air with nobody touching a button")
	}
}

// TestCommentaryMuteRoundTrips is the ordinary live control: the two cough
// buttons are two ways of calling this one boolean, and both directions have to
// work as many times as the operator presses them.
func TestCommentaryMuteRoundTrips(t *testing.T) {
	p := startedMuteStub(t, PipelineOpts{})

	for i, want := range []bool{true, false, true, true, false} {
		if err := p.SetCommentaryMute(want); err != nil {
			t.Fatalf("call %d: SetCommentaryMute(%t): %v", i, want, err)
		}
		if got := p.CommentaryMuted(); got != want {
			t.Fatalf("call %d: CommentaryMuted() = %t, want %t", i, got, want)
		}
	}
	if n := p.Counters().CommentaryMutes; n != 5 {
		t.Errorf("CommentaryMutes = %d, want 5", n)
	}
}

// TestCommentaryMuteRefusesBeforeStart is the design decision, not a nil check.
//
// There is exactly ONE route to the mute before Start — PipelineOpts.MuteCommentary
// — and exactly one after it. A latch here would be a second memory of the same
// fact, and two memories of "is the microphone open" can disagree. That is the
// central charge coughmute.go lays against zeroing the mix matrix, so building
// it here would be incoherent.
func TestCommentaryMuteRefusesBeforeStart(t *testing.T) {
	p := NewStubPipeline()

	if err := p.SetCommentaryMute(true); err == nil {
		t.Fatal("SetCommentaryMute succeeded on a pipeline that has not started. It must refuse: " +
			"a second place to remember the wanted mute is a second place for it to disagree with " +
			"PipelineOpts.MuteCommentary, and which one won would be a property of the order of " +
			"two lines rather than of anything the operator can see")
	}
	if p.CommentaryMuted() {
		t.Fatal("a refused SetCommentaryMute changed the state anyway, which is the worst of " +
			"both: the call reported failure and the application now believes it is muted")
	}
}

// TestCommentaryMuteRefusesAfterStop keeps a dead pipeline from accepting
// writes, for the same reason SetChannelMap does.
func TestCommentaryMuteRefusesAfterStop(t *testing.T) {
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: "{guid}"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.SetCommentaryMute(true); err != nil {
		t.Fatalf("SetCommentaryMute: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if err := p.SetCommentaryMute(false); err == nil {
		t.Fatal("SetCommentaryMute succeeded on a stopped pipeline")
	}
	// AND THE STATE SURVIVES. This is the half a reader will want to delete as
	// untidy, and it is the half that carries a cough across a rebuild: the
	// caller reads the mute off the pipeline it is discarding and hands it to
	// the replacement as PipelineOpts.MuteCommentary. A Stop that zeroed it
	// would answer "unmuted" for a session that ended muted.
	if !p.CommentaryMuted() {
		t.Fatal("Stop cleared the mute state. A caller rebuilding after a latched fatal reads " +
			"this to decide whether the replacement starts muted, and would put a coughing " +
			"commentator back on air")
	}
}

// TestCommentaryMuteSurvivesAReconnect is the requirement stated as a test.
//
// A reconnect is RemoveSink, backoff, ReplaceSink — internal/sender's loop — and
// none of the three may disturb the mute. On the real build this is structural
// (everything upstream of srtq stays in PLAYING and no sink method goes near the
// audio leg, which TestReconnectDoesNotTouchTheCoughMute reads the source to
// confirm); here it is checked behaviourally, on the twin every Gate A caller
// is actually developed against.
func TestCommentaryMuteSurvivesAReconnect(t *testing.T) {
	p := startedMuteStub(t, PipelineOpts{})

	if err := p.ReplaceSink(SinkOpts{Host: "m2lx.example", Port: 1234}); err != nil {
		t.Fatalf("ReplaceSink: %v", err)
	}
	if err := p.SetCommentaryMute(true); err != nil {
		t.Fatalf("SetCommentaryMute: %v", err)
	}

	// The reconnect, in the order specification section 6.2 puts it.
	if err := p.RemoveSink(); err != nil {
		t.Fatalf("RemoveSink: %v", err)
	}
	if p.CommentaryMuted() != true {
		t.Fatal("RemoveSink cleared the cough mute: the socket dropped and the commentator went " +
			"back on air without touching anything")
	}
	if err := p.ReplaceSink(SinkOpts{Host: "m2lx.example", Port: 1234}); err != nil {
		t.Fatalf("ReplaceSink: %v", err)
	}
	if p.CommentaryMuted() != true {
		t.Fatal("the mute cleared when the socket reconnected. That is a cough on air")
	}
}

// ---------------------------------------------------------------------------
// Source guards over the cgo twin
// ---------------------------------------------------------------------------

// TestCoughMuteElementIsInThePipeline checks that the name GetByName is called
// with is the name the parse string uses. A drift here is not a mute that
// misbehaves, it is startBuiltLocked aborting — which is at least loud — or, if
// somebody softens that abort, a cough button that reports success and does
// nothing.
func TestCoughMuteElementIsInThePipeline(t *testing.T) {
	body := pipelineDescriptionSource(t)

	if !strings.Contains(body, "volume name=") {
		t.Fatal("pipelineDescription has no volume element. There is no cough mute on the send " +
			"path at all, and every button that claims to mute the commentary is lying")
	}
	if !strings.Contains(body, "nameCoughMute") {
		t.Fatal("the volume element in pipelineDescription is no longer named through " +
			"nameCoughMute, so GetByName and the parse string can drift and the mute would be " +
			"looked up by a name nothing built")
	}
}

// TestTheCoughMuteIsAboveTheProgrammeMeter is the most important guard in this
// file, and it is about honesty rather than about muting.
//
// alevel exists to measure the exact signal entering the AAC encoder, so that no
// meter can keep moving while silence goes to air — pipelineDescription's own
// comment says so at length. A mute placed BELOW that meter recreates precisely
// that failure by hand: the commentator coughs, the mute engages, and the
// programme meter goes on bouncing to a voice nobody is receiving.
//
// Above it, the meter reads digital silence and goes on posting at its own rate.
// Measured on 2026-08-16 through this chain: 89 level messages either way, rms
// -12.006563271339424 unmuted against -699.99999984363217 muted.
func TestTheCoughMuteIsAboveTheProgrammeMeter(t *testing.T) {
	body := pipelineDescriptionSource(t)

	mute := strings.Index(body, "volume name=")
	level := strings.Index(body, "level name=alevel")
	enc := strings.Index(body, `aacEncoderFactory + " bitrate="`)
	if mute < 0 || level < 0 || enc < 0 {
		t.Fatal("the audio branch has been restructured; re-derive this guard from the new shape")
	}
	if !(mute < level) {
		t.Error("the cough mute sits BELOW the programme meter. The meter would go on showing " +
			"the commentator's voice while silence went to air, which is exactly the false " +
			"reassurance alevel's placement exists to make impossible")
	}
	if !(level < enc) {
		t.Error("the programme meter is no longer between the mute and the encoder")
	}

	// And BELOW the per-channel picker, so the mapping panel goes on showing
	// which of a card's sixteen inputs the commentator is on while they cough.
	// That question's answer does not change because they are off air for two
	// seconds.
	picker := strings.Index(body, "level name="+channelLevelElementName)
	if picker < 0 {
		t.Fatal("pipelineDescription no longer builds the per-channel picker meter")
	}
	if !(picker < mute) {
		t.Error("the cough mute sits ABOVE the per-channel picker, so every one of the mapping " +
			"panel's bars falls to silence during a cough and the operator cannot see which " +
			"channel the commentator is on at the moment they most want to check")
	}
}

// TestCoughMuteIsNotTheMixMatrix keeps the two mechanisms apart in the source.
//
// The routing and the mute are separate properties on separate elements
// precisely so that they cannot disagree — coughmute.go's second charge against
// the mix-matrix route. A SetCommentaryMute that reached for audioconvert, or an
// applyCoughMuteLocked that wrote a matrix, would reintroduce the coupling while
// leaving every behavioural test above green.
func TestCoughMuteIsNotTheMixMatrix(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)

	for _, fn := range []struct{ recv, name string }{
		{"cgoPipeline", "SetCommentaryMute"},
		{"cgoPipeline", "applyCoughMuteLocked"},
	} {
		body := funcBody(t, fset, file, fn.recv, fn.name)
		for _, forbidden := range []string{"nameAudioConv", "mix-matrix", "MixMatrix", "aconv"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s mentions %q. The mute must not travel through the channel map: one "+
					"property carrying both the routing and the mute is two controls that can "+
					"disagree, and mix-matrix cannot be read back so the state could never be "+
					"reported truthfully", fn.name, forbidden)
			}
		}
	}
}

// TestCoughMuteIsReadBackFromTheElement is what makes CommentaryMuted an
// observation rather than a recollection.
//
// Setting a GObject property is a void call. A mute that silently did not take
// looks exactly like one that did, and the operator's only evidence would be
// that the far end heard them cough. So the write is followed by a read of the
// same property, the read-back value is what is stored, and a disagreement is an
// error rather than a log line.
func TestCoughMuteIsReadBackFromTheElement(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	body := funcBody(t, fset, file, "cgoPipeline", "applyCoughMuteLocked")

	for _, want := range []string{
		"hasProperty(p.cough, propMute)",     // no GLib CRITICAL into a void
		"p.cough.SetObjectProperty(propMute", // the write
		"p.cough.ObjectProperty(propMute)",   // the read BACK
		"got != mute",                        // and the comparison
		"p.muted.Store(got)",                 // storing what the ELEMENT said
	} {
		if !strings.Contains(body, want) {
			t.Errorf("applyCoughMuteLocked no longer contains %q. Without the read-back the "+
				"application can only ever report the mute it BELIEVES it wrote, which is the "+
				"one thing about a cough button that must never be a belief", want)
		}
	}
	if strings.Contains(body, "p.muted.Store(mute)") {
		t.Error("applyCoughMuteLocked stores the REQUESTED value rather than the one read back " +
			"off the element, which turns CommentaryMuted back into a recollection")
	}
}

// TestCoughMuteIsAppliedBeforePlaying pins the ORDER inside startBuiltLocked. A
// pipeline asked to start muted must be muted before it can produce a buffer;
// applying it after the state change would put however many milliseconds of live
// commentary on air, which for the one case the option exists for — rebuilding a
// session that was muted when it died — is the exact failure it prevents.
func TestCoughMuteIsAppliedBeforePlaying(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	body := funcBody(t, fset, file, "cgoPipeline", "startBuiltLocked")

	apply := strings.Index(body, "applyCoughMuteLocked(opts.MuteCommentary)")
	if apply < 0 {
		t.Fatal("startBuiltLocked never applies PipelineOpts.MuteCommentary, so a pipeline built " +
			"to replace one that died muted starts with the commentator on air")
	}
	play := strings.Index(body, "gogst.StatePlaying")
	if play < 0 {
		t.Fatal("startBuiltLocked no longer names gogst.StatePlaying; re-derive this guard")
	}
	if !(apply < play) {
		t.Error("the cough mute is applied AFTER the pipeline is taken to PLAYING. A pipeline " +
			"that must begin muted would carry live commentary for the length of the state change")
	}
}

// TestReconnectDoesNotTouchTheCoughMute is the structural half of "mute must
// survive a reconnect".
//
// A reconnect is RemoveSink then ReplaceSink and nothing else. Everything
// upstream of srtq — which is the whole audio leg, the volume element included —
// stays in PLAYING for the life of the process, so the mute survives by
// construction rather than by being re-applied. This guard is what keeps that
// true: a sink method that started touching the cough element would be a mute
// with a second, sink-shaped lifetime, and the first symptom would be a
// commentator going back on air at the moment the socket recovered.
func TestReconnectDoesNotTouchTheCoughMute(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)

	for _, name := range []string{"ReplaceSink", "RemoveSink", "rearmQueueLocked"} {
		body := funcBody(t, fset, file, "cgoPipeline", name)
		for _, forbidden := range []string{"p.cough", "p.muted", "nameCoughMute", "propMute"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s mentions %q. The reconnect path must not go near the cough mute: a "+
					"mute that clears when the socket reconnects is a cough on air", name, forbidden)
			}
		}
	}
}

// TestTheVolumePluginIsStagedByBothBundlers is the one guard here that reads
// outside this package, and it is the difference between a feature and a
// shipping failure.
//
// The volume element is in the audio leg UNCONDITIONALLY, on every seat. A
// bundle that does not stage its plugin is not a seat with no cough mute — it is
// gst_parse_launch failing at Start, on every machine, twenty minutes before
// kick-off. requiredElements turns that into a named plugin at launch; this
// turns it into a red test at Gate A, which is earlier and is where the person
// who added the element is still looking.
func TestTheVolumePluginIsStagedByBothBundlers(t *testing.T) {
	for _, b := range []struct{ path, want string }{
		{"../../build/bundle-gst.ps1", "libgstvolume.dll"},
		{"../../build/bundle-gst-darwin.sh", "volume"},
	} {
		src := readRepoFile(t, b.path)
		if !strings.Contains(src, b.want) {
			t.Errorf("%s does not stage %q. The pipeline now contains a volume element on every "+
				"seat, so a bundle without that plugin fails gst_parse_launch at Start on every "+
				"machine — not a missing cough mute, a missing feed", b.path, b.want)
		}
	}
}
