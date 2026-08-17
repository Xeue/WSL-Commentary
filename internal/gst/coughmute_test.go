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

// startedMuteCapture is a running StubCapture, since every test below needs one
// and the required options are noise.
//
// IT IS A CAPTURE PIPELINE AND NOT A SEND PIPELINE, which is the whole of what
// changed: the cough mute is a volume element between the resampler's capsfilter
// and the programme meter, upstream of the proxysink, so it belongs to the
// pipeline that has the microphone open. It exists from launch and outlives every
// session.
func startedMuteCapture(t *testing.T, opts CaptureOpts) *StubCapture {
	t.Helper()
	if opts.Legs == (CaptureLegs{}) {
		opts.Legs = CaptureLegs{Commentary: CommentaryNative}
	}
	if opts.AudioDeviceID == "" && opts.AudioCaptureID == "" {
		opts.AudioDeviceID = "{0.0.1.00000000}.{stub}"
	}
	c, err := NewStubCapture(opts)
	if err != nil {
		t.Fatalf("NewCapture: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop() })
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return c
}

// TestCommentaryStartsUnmuted pins the default, which is the state every seat
// that has never seen a cough button is in. The zero value of CaptureOpts must
// put the commentator on air.
func TestCommentaryStartsUnmuted(t *testing.T) {
	c := startedMuteCapture(t, CaptureOpts{})
	if c.CommentaryMuted() {
		t.Fatal("a capture started with the zero CaptureOpts reports the commentary MUTED. " +
			"The default has to be on air: a seat that never touches a cough button must sound " +
			"exactly as it did before the feature existed")
	}
}

// TestStartMutedIsMutedBeforeAnythingRuns is the option that carries a mute into
// the session. The value must be in force the moment Start returns, not applied
// by something later, because the whole point of it is that a capture built to be
// muted never carries one buffer of live commentary.
func TestStartMutedIsMutedBeforeAnythingRuns(t *testing.T) {
	c := startedMuteCapture(t, CaptureOpts{MuteCommentary: true})
	if !c.CommentaryMuted() {
		t.Fatal("CaptureOpts.MuteCommentary did not take. A capture rebuilt to replace one that " +
			"died muted would put the commentator back on air with nobody touching a button")
	}
}

// TestCommentaryMuteRoundTrips is the ordinary live control: the two cough
// buttons are two ways of calling this one boolean, and both directions have to
// work as many times as the operator presses them.
func TestCommentaryMuteRoundTrips(t *testing.T) {
	c := startedMuteCapture(t, CaptureOpts{})

	for i, want := range []bool{true, false, true, true, false} {
		if err := c.SetCommentaryMute(want); err != nil {
			t.Fatalf("call %d: SetCommentaryMute(%t): %v", i, want, err)
		}
		if got := c.CommentaryMuted(); got != want {
			t.Fatalf("call %d: CommentaryMuted() = %t, want %t", i, got, want)
		}
	}
}

// TestCommentaryMuteRefusesAfterStop keeps a dead pipeline from accepting
// writes, for the same reason SetChannelMap does.
//
// THE PRE-START REFUSAL THAT USED TO SIT BESIDE IT IS GONE, and it was deleted
// rather than lost: the operator ruled on 2026-08-16 that a latch set before
// START is still set at START. The argument it answered — that one control must
// not have two memories — is met by there being only ONE memory again, the
// element's own, read back after every write, because the element now exists from
// launch. capture_stub_test.go's TestTheMuteIsCarriedIntoTheSessionAndSettable
// WithoutOne is that rule stated as a test.
func TestCommentaryMuteRefusesAfterStop(t *testing.T) {
	c, err := NewStubCapture(CaptureOpts{
		Legs:          CaptureLegs{Commentary: CommentaryNative},
		AudioDeviceID: "{guid}",
	})
	if err != nil {
		t.Fatalf("NewCapture: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := c.SetCommentaryMute(true); err != nil {
		t.Fatalf("SetCommentaryMute: %v", err)
	}
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if err := c.SetCommentaryMute(false); err == nil {
		t.Fatal("SetCommentaryMute succeeded on a stopped capture pipeline")
	}
	// AND THE STATE SURVIVES. This is the half a reader will want to delete as
	// untidy, and it is the half that carries a cough across a rebuild: the caller
	// reads the mute off the pipeline it is discarding and hands it to the
	// replacement as CaptureOpts.MuteCommentary. A Stop that zeroed it would answer
	// "unmuted" for a capture that ended muted.
	if !c.CommentaryMuted() {
		t.Fatal("Stop cleared the mute state. A caller rebuilding after a latched fatal reads " +
			"this to decide whether the replacement starts muted, and would put a coughing " +
			"commentator back on air")
	}
}

// TestCommentaryMuteSurvivesAReconnect is the requirement stated as a test, and
// the seam has made it STRUCTURAL rather than behavioural.
//
// A reconnect is RemoveSink, backoff, ReplaceSink — internal/sender's loop — and
// none of the three may disturb the mute. It cannot now, because the mute is not
// on the object those methods are called on at all: the send pipeline has no
// audio leg, no volume element and no way to reach one. This drives both halves
// anyway, because "cannot by construction" is the claim, and a test that
// exercises it is how the claim stays true after the next refactor.
func TestCommentaryMuteSurvivesAReconnect(t *testing.T) {
	c := startedMuteCapture(t, CaptureOpts{})
	p := NewStubPipeline(CaptureSet{Picture: c, Commentary: c})
	if err := p.Start(SendOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	if err := p.ReplaceSink(SinkOpts{Host: "m2lx.example", Port: 1234}); err != nil {
		t.Fatalf("ReplaceSink: %v", err)
	}
	if err := c.SetCommentaryMute(true); err != nil {
		t.Fatalf("SetCommentaryMute: %v", err)
	}

	// The reconnect, in the order specification section 6.2 puts it.
	if err := p.RemoveSink(); err != nil {
		t.Fatalf("RemoveSink: %v", err)
	}
	if !c.CommentaryMuted() {
		t.Fatal("RemoveSink cleared the cough mute: the socket dropped and the commentator went " +
			"back on air without touching anything")
	}
	if err := p.ReplaceSink(SinkOpts{Host: "m2lx.example", Port: 1234}); err != nil {
		t.Fatalf("ReplaceSink: %v", err)
	}
	if !c.CommentaryMuted() {
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
	body := captureDescriptionSource(t)

	if !strings.Contains(body, "volume name=") {
		t.Fatal("captureDescription has no volume element. There is no cough mute anywhere at " +
			"all, and every button that claims to mute the commentary is lying")
	}
	if !strings.Contains(body, "nameCoughMute") {
		t.Fatal("the volume element in captureDescription is no longer named through " +
			"nameCoughMute, so GetByName and the parse string can drift and the mute would be " +
			"looked up by a name nothing built")
	}
	// AND IT IS IN THE CAPTURE PIPELINE, WHICH IS WHAT MAKES IT WORK BEFORE START.
	// The element exists from launch, so a latch set before a session is still set
	// at START (PLAN.md 0-BIS A2). A volume element in the send description would
	// put the control back inside the session's lifetime and take the pre-air
	// latch with it.
	if strings.Contains(sendDescriptionSource(t), "volume name=") {
		t.Error("sendDescription builds a volume element. The cough mute belongs to the capture " +
			"pipeline, which exists from launch; one in the send pipeline is a control that " +
			"cannot be reached until START and disappears at STOP")
	}
}

// TestTheCoughMuteIsAboveTheProgrammeMeter is the most important guard in this
// file, and it is about honesty rather than about muting.
//
// alevel exists to measure the exact signal this seat is producing, so that no
// meter can keep moving while silence goes to air — captureDescription's own
// comment says so at length. A mute placed BELOW that meter recreates precisely
// that failure by hand: the commentator coughs, the mute engages, and the
// programme meter goes on bouncing to a voice nobody is receiving.
//
// Above it, the meter reads digital silence and goes on posting at its own rate.
// Measured on 2026-08-16 through this chain: 89 level messages either way, rms
// -12.006563271339424 unmuted against -699.99999984363217 muted.
func TestTheCoughMuteIsAboveTheProgrammeMeter(t *testing.T) {
	body := captureDescriptionSource(t)

	mute := strings.Index(body, "volume name=")
	level := strings.Index(body, "level name=\"+levelElementName")
	// THE LOWER BOUND MOVED WITH THE SEAM. It used to be the AAC encoder, which
	// is now in the send pipeline; the strongest true statement on this side is
	// that the meter is between the mute and the PROXY TAIL, with nothing else in
	// between that could change the audio after it has been measured.
	tail := strings.Index(body, "audioProxyTail()")
	if mute < 0 || level < 0 || tail < 0 {
		t.Fatal("the audio branch has been restructured; re-derive this guard from the new shape")
	}
	if !(mute < level) {
		t.Error("the cough mute sits BELOW the programme meter. The meter would go on showing " +
			"the commentator's voice while silence went to air, which is exactly the false " +
			"reassurance alevel's placement exists to make impossible")
	}
	if !(level < tail) {
		t.Error("the programme meter is no longer between the mute and the seam")
	}

	// And BELOW the per-channel picker, so the mapping panel goes on showing
	// which of a card's sixteen inputs the commentator is on while they cough.
	// That question's answer does not change because they are off air for two
	// seconds.
	picker := strings.Index(body, "level name=\"+channelLevelElementName")
	if picker < 0 {
		t.Fatal("captureDescription no longer builds the per-channel picker meter")
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
	fset, file := parseSource(t, captureCgoSourceFile)

	for _, fn := range []struct{ recv, name string }{
		{"cgoCapture", "SetCommentaryMute"},
		{"cgoCapture", "applyCoughMuteLocked"},
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
	fset, file := parseSource(t, captureCgoSourceFile)
	body := funcBody(t, fset, file, "cgoCapture", "applyCoughMuteLocked")

	// The hasProperty pre-check that used to head this list is gone, and its
	// guarantee is not: the read-back's type assertion answers the same question
	// LATER AND BETTER. A missing property made hasProperty skip the write and
	// return nil — success, with the microphone open — whereas an ObjectProperty
	// that does not come back as a boolean is an ERROR naming the element. The
	// thing being protected against was never "the property is absent"; it was
	// "this code reported a mute it did not perform".
	for _, want := range []string{
		"c.cough.SetObjectProperty(propMute", // the write
		"c.cough.ObjectProperty(propMute)",   // the read BACK
		"if !ok {",                           // a value that is not a boolean is an error
		"got != mute",                        // and the comparison
		"c.muted.Store(got)",                 // storing what the ELEMENT said
	} {
		if !strings.Contains(body, want) {
			t.Errorf("applyCoughMuteLocked no longer contains %q. Without the read-back the "+
				"application can only ever report the mute it BELIEVES it wrote, which is the "+
				"one thing about a cough button that must never be a belief", want)
		}
	}
	if strings.Contains(body, "c.muted.Store(mute)") {
		t.Error("applyCoughMuteLocked stores the REQUESTED value rather than the one read back " +
			"off the element, which turns CommentaryMuted back into a recollection")
	}
}

// TestCoughMuteIsAppliedBeforePlaying pins the ORDER inside the capture build. A
// capture asked to start muted must be muted before it can produce a buffer;
// applying it after the state change would put however many milliseconds of live
// commentary into the proxysink, which for the case the option exists for — a
// latch set before START, which the operator ruled is carried into the session —
// is the exact failure it prevents.
func TestCoughMuteIsAppliedBeforePlaying(t *testing.T) {
	body := captureStartSequence(t)

	apply := strings.Index(body, "applyCoughMuteLocked(")
	if apply < 0 {
		t.Fatal("the capture build never applies CaptureOpts.MuteCommentary, so a capture built " +
			"to carry a pre-air latch starts with the commentator on air")
	}
	play := strings.Index(body, "gogst.StatePlaying")
	if play < 0 {
		t.Fatal("the capture build no longer names gogst.StatePlaying; re-derive this guard")
	}
	if !(apply < play) {
		t.Error("the cough mute is applied AFTER the pipeline is taken to PLAYING. A capture " +
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
