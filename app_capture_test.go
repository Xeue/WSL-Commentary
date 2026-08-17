//go:build dev || production || bindings

// Tests for the ALWAYS-LIVE CAPTURE LAYER's half of the wire-up (app.go).
//
// The capture layer's mechanism is tested in internal/gst — whether a proxysink
// arms, whether a matrix lands, whether a pad negotiates. What is tested HERE is
// the thing internal/gst cannot see: WHO OWNS THE PIPELINES, and therefore what
// survives what.
//
// Four properties can only be got wrong at this layer, and each one is a
// requirement rather than a preference:
//
//   - IT IS NOT THE SESSION'S. The set lives on the App under its own lock, so
//     the meters, the routing grid, the signal lamp, the preview and the cough
//     mute all exist before START and survive STOP. That is R1, and it is the
//     only reason any of the rest of this exists.
//
//   - A DEVICE CHANGE WHILE SENDING IS REFUSED. It is a SAFETY property. A new
//     proxysink orphans the running send pipeline's proxysrc and the feed goes
//     silently dead — measured, consumer A stopped at 5.994 s the instant
//     consumer B attached at 6.007 s, with SRT still connected and every lamp
//     still green. Nothing downstream of here could ever detect that.
//
//   - THE WIDTH COMES FROM THE CALLBACK. InputChannels() reads 0 for about 7 ms
//     after Start returns (measured: Start completed at +108 ms, aconv:sink
//     published its negotiated caps at +115 ms), so a synchronous read at build
//     time is a zero this side would have no reason to ask about again — and a
//     routing grid sized from it never appears.
//
//   - EVERY BOUND METHOD IS CLASSIFIED. A missing remoteAllowlist row is a
//     method that works at the desk and fails from the second seat, which is the
//     kind of fault that is found during a match.
package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
)

// ---------------------------------------------------------------------------
// Ownership: the set is the App's, not the session's
// ---------------------------------------------------------------------------

func TestTheCaptureLayerIsBuiltWithNoSessionAnywhere(t *testing.T) {
	// R1 in one assertion. Every screen this change exists for reads through
	// currentCapture, and if that answered only inside a session then none of
	// them would work before START — which is the state the operator spends the
	// hour before kick-off in.
	a, _ := newTestApp(t)
	captureUp(t, a)

	if a.sessionUp.Load() {
		t.Fatal("a session is running; this test is about the state in which none is")
	}
	set := a.currentCapture()
	if len(set.Pipelines()) == 0 {
		t.Fatal("currentCapture() is empty with no session running; the whole of R1 is that the " +
			"capture layer does not wait for one")
	}
	if set.Commentary == nil {
		t.Error("the set has no commentary pipeline; the meters, the routing and the cough mute " +
			"all hang off it")
	}
	if set.Picture == nil {
		t.Error("the set has no picture pipeline; a seat sending the slate still has a picture leg")
	}
	if a.currentSend() != nil {
		t.Error("currentSend() answered with no session; it is the OTHER question, and conflating " +
			"the two is what made the routing grid unsizeable until START")
	}
}

func TestTheCaptureSurvivesAWholeSession(t *testing.T) {
	// The direct R1 assertion at this layer: START must not replace the capture
	// and STOP must not take it away. An operator who presses STOP at half-time
	// keeps their meters, their picture, their routing and their signal lamp.
	a, _ := newTestApp(t)
	captureUp(t, a)

	before := a.currentCapture()
	useStartingSender(a)
	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	during := a.currentCapture()
	if during.Commentary != before.Commentary || during.Picture != before.Picture {
		t.Fatal("START replaced the capture pipelines; the send pipeline is minted OVER the " +
			"capture layer and must not rebuild it — a rebuild at START would blank the picture " +
			"at the one moment nobody can afford it")
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	after := a.currentCapture()
	if after.Commentary != before.Commentary || after.Picture != before.Picture {
		t.Fatalf("STOP took the capture away (%v -> %v); the meters, the preview, the routing "+
			"width and the mute are supposed to survive it", before, after)
	}
	if got := a.GetCaptureState(); got.Commentary != captureStateLive {
		t.Errorf("the capture reports %q after a STOP, want %q", got.Commentary, captureStateLive)
	}
}

// TestTheFirstStartDoesNotRebuildACaptureThatIsAlreadyCorrect is
// TestTheCaptureSurvivesAWholeSession's missing half, and it is the version that
// would have failed.
//
// That test passes on a newTestApp only because newTestApp has no control-plane
// client and no videoFormatOverride, so the conform target is unknown at launch
// AND unknown at START and the drift check compares zero against zero. A REAL
// seat gets a real answer from the switcher on every press. This one gives it one.
//
// The failure it pins: a.conformTo is written in exactly ONE place, App.Start, so
// at domReady it is nil. If the capture then RECORDS that nil as the zero
// ConformTarget while internal/gst BUILDS the picture to its 1920x1080p50
// fallback, the first START of the day compares the zero against the switcher's
// answer, finds a difference that does not exist, and rebuilds — closing and
// reopening the exclusive DeckLink, blanking the preview, dropping the meters and
// releasing the cough latch at the exact moment the operator pressed START, with
// no capture at all if the card does not come back.
func TestTheFirstStartDoesNotRebuildACaptureThatIsAlreadyCorrect(t *testing.T) {
	a, _ := newTestApp(t)

	// A switcher that answers, configured for the same raster the picture leg is
	// built to when nothing is known. This is the ordinary seat: the fallback is
	// 1080p50 because that is what almost every M2L-X instance runs.
	cfg := validConfig()
	cfg.VideoFormatOverride = gst.FallbackConformTarget().String()
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	// NOTHING IS STORED BEFORE THE BUILD, deliberately. That is the state
	// domReady is in whenever the launch resolve could not read anything — no
	// control plane yet, an instance still coming up — and it is the state
	// a.conformTo was ALWAYS in before the launch resolve existed, because Start
	// is its only other writer. The picture leg is therefore built to
	// internal/gst's fallback, and what this pins is that the record says so.
	if a.conformTo.Load() != nil {
		t.Fatal("a fresh App already has a conform target; this test is about the state in " +
			"which it does not")
	}
	captureUp(t, a)
	before := a.currentCapture()

	useStartingSender(a)
	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })

	during := a.currentCapture()
	if during.Picture != before.Picture || during.Commentary != before.Commentary {
		t.Fatalf("THE FIRST START REBUILT THE CAPTURE: picture %p -> %p, commentary %p -> %p. "+
			"The picture leg was already conformed to %v and the switcher asked for %v; a "+
			"rebuild here takes the card down at the one moment nobody can afford it",
			before.Picture, during.Picture, before.Commentary, during.Commentary,
			gst.FallbackConformTarget(), cfg.VideoFormatOverride)
	}
}

// TestTheLaunchResolvesTheConformTargetBeforeItBuildsThePicture is the other
// half of the same promise, and the one that makes it true on a seat whose
// switcher is NOT configured for the fallback.
//
// The conform chain lives in the CAPTURE pipeline — which is what pins the caps
// crossing the proxy for the life of the process (PLAN.md 3.6) — so the raster is
// decided when the capture is built. Leaving that decision until START means the
// first press of every day finds a picture built to the wrong target and has no
// choice but to rebuild it, card and all.
func TestTheLaunchResolvesTheConformTargetBeforeItBuildsThePicture(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	cfg := validConfig()
	cfg.VideoFormatOverride = "1280x720p50"
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	a.domReady(context.Background())

	// domReady's build runs on a goroutine of its own — it holds the Wails MAIN
	// THREAD otherwise, and a card that is slow to lock takes the best part of a
	// second — so this waits rather than reading straight through.
	waitFor(t, 5*time.Second, "the launch build to finish", func() bool {
		return len(a.currentCapture().Pipelines()) > 0
	})

	a.capMu.Lock()
	built := a.capConform
	a.capMu.Unlock()
	if built.Width != 1280 || built.Height != 720 {
		t.Fatalf("the launch built the picture to %v, want 1280x720p50. The conform target has "+
			"to be resolved BEFORE the capture, or the first START of the day rebuilds it — "+
			"closing and reopening the exclusive card at the worst possible moment", built)
	}
}

// TestATransientSwitcherFailureDoesNotRebuildThePicture is the second-order half.
//
// conformFormat returns nil whenever it could read NOTHING — a three-second REST
// timeout to the M2L-X is enough — and nil is the absence of an answer, not a new
// one. Treating it as a change would take the card down over a network blip and
// then conform the picture to the 1080p50 fallback, which is the one raster the
// switcher had already been observed not to be configured for.
func TestATransientSwitcherFailureDoesNotRebuildThePicture(t *testing.T) {
	a, _ := newTestApp(t)

	cfg := validConfig()
	cfg.VideoFormatOverride = "1280x720p50"
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	a.conformTo.Store(a.conformFormat(a.snapshotConfig()))
	captureUp(t, a)
	before := a.currentCapture()
	if got := a.capConform; got.Width != 1280 {
		t.Fatalf("the capture recorded conform target %v, want the 1280-wide override — this "+
			"test is meaningless unless the capture was built to something other than the "+
			"fallback", got)
	}

	// The blip: nothing to read at all this press.
	a.conformTo.Store(nil)

	useStartingSender(a)
	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })

	if during := a.currentCapture(); during.Picture != before.Picture {
		t.Fatal("a press on which the switcher could not be read rebuilt the picture. The " +
			"absence of an answer cannot contradict the answer we already have, and rebuilding " +
			"here conforms the feed to the fallback over a network blip")
	}
}

// TestACoughLatchSurvivesACaptureRebuild is PLAN.md 0-BIS A2 at the layer that
// can actually get it wrong.
//
// captureOpts argues at length that the latch is carried, and it was not:
// rebuildCaptureLocked's first act is teardownCaptureLocked, which calls
// forgetMute, which sets a.muted false — so a read taken inside captureOpts could
// only ever return false, and the whole paragraph described behaviour the code
// did not have. Measured before the fix: latch, RestartCapture, and mutedNow goes
// true -> false with the rebuilt pipeline reporting muted=false.
func TestACoughLatchSurvivesACaptureRebuild(t *testing.T) {
	a, _ := newTestApp(t)
	captureUp(t, a)

	if _, err := a.SetCommentaryMute(true, 1); err != nil {
		t.Fatalf("SetCommentaryMute(true) error = %v; the latch must be settable with no session", err)
	}
	if !a.mutedNow() {
		t.Fatal("the latch did not take")
	}

	if err := a.RestartCapture(); err != nil {
		t.Fatalf("RestartCapture() error = %v", err)
	}

	if !a.mutedNow() {
		t.Fatal("the cough latch was released by the rebuild. A pre-air latch is CARRIED " +
			"(PLAN.md 0-BIS A2), and the operator who muted is the operator restarting the " +
			"capture, seconds apart, watching a flat meter")
	}
	cap, _ := a.currentCommentary()
	if cap == nil {
		t.Fatal("the rebuild produced no commentary capture")
	}
	if !cap.CommentaryMuted() {
		t.Fatal("the rebuilt pipeline reports itself UNMUTED while this side says muted. The " +
			"value is READ BACK off the element, so this is a commentator who is audible under " +
			"a control that says they are not")
	}
}

// TestACaptureThatDiesRepaintsThePanel is the fault half of the capture event.
//
// publishCapture is otherwise called only from a build or a teardown path, so a
// pipeline that died at run time left the panel reading "live" for the rest of
// the day — and a page reloaded at half-time read the same stale record through
// GetCaptureState and drew a healthy microphone over a dead one.
func TestACaptureThatDiesRepaintsThePanel(t *testing.T) {
	a, _ := newTestApp(t)
	captureUp(t, a)

	if got := a.GetCaptureState(); got.Commentary != captureStateLive {
		t.Fatalf("the capture reports %q before anything died, want %q", got.Commentary, captureStateLive)
	}

	cap, _ := a.currentCommentary()
	stub, ok := cap.(*gst.StubCapture)
	if !ok {
		t.Fatalf("the commentary capture is a %T, not a *gst.StubCapture", cap)
	}
	stub.Fail(errors.New("the microphone was unplugged"))

	waitFor(t, 5*time.Second, "the capture panel to report the death", func() bool {
		return a.GetCaptureState().Commentary == captureStateFailed
	})
	if got := a.GetCaptureState(); !strings.Contains(got.Reason, "unplugged") {
		t.Errorf("the capture state's reason is %q; it must carry the fault, because the panel "+
			"is where an operator looks", got.Reason)
	}
}

// TestAPictureDeathIsVisibleAndRefusesStart is the same property for the leg that
// had none at all.
//
// A picture-leg fault reached deliverWarning and stopped there: nothing on the
// capture event, nothing on the error event, a frozen preview, and Health() == nil
// — so ArmForSend's latched-fault refusal did not fire, START reached PLAYING and
// the refusal that finally came was the muxer watchdog's "nothing reached vq:src"
// two seconds later, naming a pad rather than the card fault internal/gst had
// already diagnosed and thrown away.
func TestAPictureDeathIsVisibleAndRefusesStart(t *testing.T) {
	a, _ := newTestApp(t)
	captureUp(t, a)

	set := a.currentCapture()
	stub, ok := set.Picture.(*gst.StubCapture)
	if !ok {
		t.Fatalf("the picture capture is a %T, not a *gst.StubCapture", set.Picture)
	}
	if set.Picture == set.Commentary {
		t.Fatal("this seat is fused; the split case is the one where a picture death used to be " +
			"invisible")
	}
	stub.FailPicture(errors.New("the capture card has been unplugged"))

	waitFor(t, 5*time.Second, "the capture panel to report the picture death", func() bool {
		return a.GetCaptureState().Picture == captureStateFailed
	})
	if got := a.GetCaptureState(); got.Commentary != captureStateLive {
		t.Errorf("the commentary reads %q after a PICTURE death, want %q — it is a separate "+
			"pipeline and is still metering", got.Commentary, captureStateLive)
	}

	useStartingSender(a)
	err := a.Start()
	if err == nil {
		_ = a.Stop()
		t.Fatal("START succeeded over a dead picture leg. mpegtsmux emits nothing at all while " +
			"one of its two inputs is silent, so the feed would carry zero bytes with SRT " +
			"connected and every lamp green")
	}
	if !strings.Contains(err.Error(), "unplugged") {
		t.Errorf("START was refused with %q, which does not name the cause the capture layer "+
			"had already diagnosed", err)
	}
}

// TestARefusedCaptureStopKeepsTheSet is the ordering rule's enforcement, checked
// from the side that used to discard it.
//
// CapturePipeline.Stop refuses with ErrSeamBusy while a send pipeline still holds
// the seam, and a capture that refused to stop IS STILL RUNNING: it holds the
// exclusive card, its seam claim, and the two goroutines draining its fault
// channels — which Stop is the only thing that ends. Clearing the set regardless
// dropped the last reference to all of it, after which rootWG.Wait() was
// GUARANTEED to be abandoned because those goroutines range over channels nothing
// would ever close.
func TestARefusedCaptureStopKeepsTheSet(t *testing.T) {
	a, _ := newTestApp(t)
	captureUp(t, a)
	before := a.currentCapture()

	// The send pipeline's claim, taken directly: this is the state a shutdown
	// reaches when the sender step overruns its budget and is abandoned.
	for _, c := range before.Pipelines() {
		if err := c.ClaimForSend(); err != nil {
			t.Fatalf("claiming the seam: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, c := range before.Pipelines() {
			c.ReleaseFromSend()
		}
	})

	a.capMu.Lock()
	err := a.teardownCaptureLocked()
	a.capMu.Unlock()

	if err == nil {
		t.Fatal("teardownCaptureLocked reported success while the seam was still claimed; the " +
			"refusal is what enforces sender-then-capture")
	}
	if !errors.Is(err, gst.ErrSeamBusy) {
		t.Errorf("the refusal is %v, want one wrapping ErrSeamBusy", err)
	}
	after := a.currentCapture()
	if after.Picture != before.Picture || after.Commentary != before.Commentary {
		t.Fatal("the set was cleared anyway. The capture is still running and still holds its " +
			"device, and nothing can ever stop it now — which also parks the fault-drain " +
			"goroutines on rootWG forever")
	}
}

// ---------------------------------------------------------------------------
// The safety property
// ---------------------------------------------------------------------------

func TestAnAudioDeviceChangeWhileSendingIsRefused(t *testing.T) {
	// IT IS A SAFETY PROPERTY AND NOT A PREFERENCE, and it is worth stating in
	// the test because the obvious future "improvement" is to allow it.
	// SelectCommentaryInput builds a NEW proxysink; the send pipeline's proxysrc
	// is bound to the OLD one, and a proxysrc whose producer has gone reports
	// nothing at all — no EOS, no error, no warning, still PLAYING, SRT still
	// connected, every lamp still green. The switcher receives silence.
	a, _ := newTestApp(t)
	captureUp(t, a)
	useStartingSender(a)

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })

	before := a.currentCapture()

	err := a.SelectCommentaryInput(config.AudioSourceNative, "{0.0.1.00000000}.{c41a9d7e-0004-438e-9003-51a46e13a0c1}", "")
	if err == nil {
		t.Fatal("SelectCommentaryInput succeeded while sending; the feed would have gone silently " +
			"dead with every lamp still green")
	}
	if !strings.Contains(err.Error(), "STOP") {
		t.Errorf("error %q does not tell the operator what to press", err)
	}

	if err := a.RestartCapture(); err == nil {
		t.Fatal("RestartCapture succeeded while sending; it is a device change by another name " +
			"and takes the same feed off air")
	}

	if after := a.currentCapture(); after.Commentary != before.Commentary {
		t.Fatal("a refused device change replaced the capture anyway; a refusal must not move the " +
			"thing it refused to change")
	}
}

func TestARemoteSaveChangingTheCommentaryDeviceIsRefused(t *testing.T) {
	// SaveConfig is remotely reachable — deliberately, so a producer's laptop can
	// fix a port — and it is a WHOLE-DOCUMENT write. A save now RE-POINTS the live
	// capture, so without this guard a remote seat could take the desk's
	// microphone away through a method it is entitled to call, and the host-only
	// classification on SelectCommentaryInput would be a decoration.
	a, _ := newTestApp(t)

	for _, tc := range []struct {
		name  string
		edit  func(*config.Config)
		names string
	}{
		{"the platform endpoint", func(c *config.Config) { c.AudioDeviceID = "somebody-elses-microphone" }, "audioDeviceId"},
		{"the capture kind", func(c *config.Config) { c.AudioSourceKind = config.AudioSourceDeckLink }, "audioSourceKind"},
		{"the card", func(c *config.Config) { c.DeckLinkPersistentID = "9999999999" }, "decklinkPersistentId"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := a.snapshotConfig()
			tc.edit(cfg)

			err := a.saveConfigFrom("some-remote-seat", cfg)
			if err == nil {
				t.Fatalf("a remote save changing %s was accepted; it would re-point the capture "+
					"the operator at the desk is using", tc.names)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("error %q does not name the field it refused, so the remote operator "+
					"cannot tell which of their edits was the problem", err)
			}
			if !strings.Contains(err.Error(), "operator at the desk") {
				t.Errorf("error %q does not say who can make the change; a refusal with no route "+
					"out of it is a refusal read twice", err)
			}
		})
	}

	// And an ordinary remote save, with the capture fields restated exactly as
	// the page was told them, still passes through untouched. Without this the
	// guard would refuse every remote save of a port number.
	if err := a.saveConfigFrom("some-remote-seat", a.snapshotConfig()); err != nil {
		t.Fatalf("an unchanged remote save was refused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The bound methods
// ---------------------------------------------------------------------------

func TestSelectCommentaryInputRepointsTheCaptureAndDoesNotSave(t *testing.T) {
	// The contract frontend/src/ui/backend.js states: it re-points the LIVE
	// capture and writes nothing. That is what lets an operator audition
	// microphones twenty minutes before kick-off without rewriting config.json,
	// and the Settings screen's Save is still what makes a choice stick.
	a, _ := newTestApp(t)
	captureUp(t, a)

	const other = "{0.0.1.00000000}.{a8b31f60-0004-438e-9003-51a46e13d5a7}" // the measured three-channel one
	saved := a.snapshotConfig().AudioDeviceID
	if saved == other {
		t.Fatal("the fixture already selects the device this test switches to")
	}

	if err := a.SelectCommentaryInput(config.AudioSourceNative, other, ""); err != nil {
		t.Fatalf("SelectCommentaryInput error = %v", err)
	}

	// The CAPTURE moved...
	if _, key := a.currentCommentary(); key != config.AudioDeviceKeyFor(config.AudioSourceNative, other) {
		t.Errorf("the capture reports device key %q, want the one just selected", key)
	}
	// ...and the DOCUMENT did not.
	if got := a.snapshotConfig().AudioDeviceID; got != saved {
		t.Errorf("SelectCommentaryInput wrote audioDeviceId = %q to the configuration; it is the "+
			"LIVE control and Save is the record, or a mid-match audition rewrites the file the "+
			"next launch reads", got)
	}
}

func TestSelectCommentaryInputRefusesAKindItCannotBuild(t *testing.T) {
	a, _ := newTestApp(t)

	err := a.SelectCommentaryInput("carrier-pigeon", "x", "")
	if err == nil {
		t.Fatal("an unknown capture kind was accepted; it would reach preflightCapture as a " +
			"native seat and open the system default input")
	}
	if !strings.Contains(err.Error(), config.AudioSourceDeckLink) {
		t.Errorf("error %q does not name the values that are allowed", err)
	}

	// EMPTY is not an error: it is the default, exactly as
	// config.EffectiveAudioSourceKind reads it. A caller that has never chosen a
	// KIND — the frontend's dropdown value for a device written before the field
	// existed — selects a platform endpoint rather than failing.
	const anEndpoint = "{0.0.1.00000000}.{c41a9d7e-0004-438e-9003-51a46e13a0c1}"
	if err := a.SelectCommentaryInput("", anEndpoint, ""); err != nil {
		t.Errorf("an empty kind was refused (%v); it is the documented default", err)
	}
	if _, key := a.currentCommentary(); key != config.AudioDeviceKeyFor(config.AudioSourceNative, anEndpoint) {
		t.Errorf("an empty kind produced device key %q; it must read as %q, or a routing saved "+
			"under one spelling is looked up under another and lost",
			key, config.AudioSourceNative)
	}
}

func TestASavedConfigurationDropsTheLiveSelection(t *testing.T) {
	// The one thing that ends an audition. Without it the live selection would go
	// on winning silently over a document the operator has just committed, and
	// "I saved the Focusrite and it is still on the built-in mic" is a fault
	// nothing on screen would explain.
	a, _ := newTestApp(t)
	captureUp(t, a)

	const audition = "{0.0.1.00000000}.{a8b31f60-0004-438e-9003-51a46e13d5a7}"
	const committed = "{0.0.1.00000000}.{c41a9d7e-0004-438e-9003-51a46e13a0c1}"
	if err := a.SelectCommentaryInput(config.AudioSourceNative, audition, ""); err != nil {
		t.Fatalf("SelectCommentaryInput error = %v", err)
	}

	cfg := a.snapshotConfig()
	cfg.AudioDeviceID = committed
	if err := a.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig error = %v", err)
	}

	want := config.AudioDeviceKeyFor(config.AudioSourceNative, committed)
	if _, key := a.currentCommentary(); key != want {
		t.Errorf("after a save the capture is on %q, want %q: a save is the operator committing, "+
			"and the document becomes the answer again", key, want)
	}
}

func TestRestartCaptureBuildsAFreshSetOverTheSameSeat(t *testing.T) {
	// The ONLY recovery from a capture fault, because the card is held from launch
	// to quit and there is deliberately no Acquire/Release control (A1). It has to
	// produce NEW pipelines — a repair that returned the same objects would not
	// have reopened the device the operator has just plugged back in.
	a, _ := newTestApp(t)
	captureUp(t, a)
	before := a.currentCapture()

	if err := a.RestartCapture(); err != nil {
		t.Fatalf("RestartCapture() error = %v", err)
	}
	after := a.currentCapture()

	if after.Commentary == before.Commentary {
		t.Fatal("RestartCapture returned the same commentary pipeline; nothing was reopened, so " +
			"the microphone the operator has just plugged in is still not being read")
	}
	if len(after.Pipelines()) == 0 {
		t.Fatal("RestartCapture left nothing open")
	}
	if got := a.GetCaptureState(); got.Commentary != captureStateLive {
		t.Errorf("after a restart the capture reports %q, want %q", got.Commentary, captureStateLive)
	}
}

func TestGetCaptureStateAnswersBeforeAnythingIsBuilt(t *testing.T) {
	// It is called on a page's startup path by a screen whose job is to EXPLAIN a
	// fault, and it is replayed from domReady on the Wails main thread. Two
	// properties follow and both are asserted: it cannot fail, and the zero value
	// is four readable strings rather than four empty ones — an empty state is a
	// value no screen has a case for, and would draw as nothing at all.
	a, _ := newTestApp(t)

	got := a.GetCaptureState()
	if got.Picture != captureStateOff || got.Commentary != captureStateOff {
		t.Fatalf("got %+v, want both legs %q before anything has been built", got, captureStateOff)
	}
	if got.Reason != "" {
		t.Errorf("Reason = %q with nothing failed; the sentence is for a leg that FAILED", got.Reason)
	}
}

func TestGetCaptureStateNamesTheDeviceThatOpened(t *testing.T) {
	// The form shows a SELECTION; this shows what OPENED, and the two differ for
	// the whole of the window between choosing a microphone and the capture
	// renegotiating — and permanently, if the chosen one is not present. A panel
	// that could not tell them apart would leave an operator looking at a dead
	// meter beside the name of a device that was never taken.
	a, _ := newTestApp(t)
	captureUp(t, a)

	got := a.GetCaptureState()
	if got.Commentary != captureStateLive {
		t.Fatalf("the commentary leg reports %q, want %q", got.Commentary, captureStateLive)
	}
	// validConfig selects the first stub device, whose display name is what the
	// dropdown shows.
	if !strings.Contains(got.AudioDeviceName, "Dante") {
		t.Errorf("AudioDeviceName = %q, want the enumerated display name of the device that "+
			"opened", got.AudioDeviceName)
	}
}

func TestTheCaptureEventCarriesEveryTransition(t *testing.T) {
	// The page has no getter it polls: onCapture is how a screen learns that a
	// device is opening, and then that it opened. OPENING is not a transient
	// worth skipping — a card that is merely slow to lock can sit there for a
	// second or more, and a screen that drew "failed" over it would have an
	// operator pulling cables at a pipeline that was about to come up.
	a, _ := newTestApp(t)
	silencePump(a)
	captureUp(t, a)

	var states []string
	for _, e := range drainPump(a) {
		if e.name != EventCapture {
			continue
		}
		p, ok := e.data.(capturePayload)
		if !ok {
			t.Fatalf("a %q event carried a %T, want capturePayload", EventCapture, e.data)
		}
		states = append(states, p.Commentary)
	}
	if len(states) < 2 || states[0] != captureStateOpening || states[len(states)-1] != captureStateLive {
		t.Fatalf("the capture event sequence was %v, want %q first and %q last",
			states, captureStateOpening, captureStateLive)
	}
}

// ---------------------------------------------------------------------------
// The width, and where it is allowed to come from
// ---------------------------------------------------------------------------

func TestTheRoutingWidthArrivesStampedWithItsDevice(t *testing.T) {
	// THE CALLBACK IS THE ONLY SOURCE, because InputChannels() reads 0 for about
	// 7 ms after Start returns — measured on the card: Start completed at +108 ms
	// and aconv:sink published its negotiated caps at +115 ms. A build that took
	// the width from a synchronous read would publish that zero and never ask
	// again, and the routing panel would never appear.
	//
	// AND THE STAMP IS NOT OPTIONAL. Without it there is a window between
	// selecting a Focusrite and the capture renegotiating in which the grid still
	// holds the previous device's sixteen, and a crosspoint pressed in that window
	// writes a 2x16 matrix onto a two-channel pad — the measured "streaming
	// stopped, reason error (-5)", which reads as a broken device.
	a, _ := newTestApp(t)

	if a.captureOpts(validConfig(), capturePlan{}, 0, gst.FallbackConformTarget(), false).OnInputChannels == nil {
		t.Fatal("CaptureOpts.OnInputChannels is nil; nothing would ever size the routing grid, " +
			"because a synchronous read after Start is a zero this side cannot recognise")
	}

	captureUp(t, a)

	const wantKey = "native:{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}"
	var got channelMapPayload
	waitFor(t, 5*time.Second, "a negotiated width to reach the frontend queue", func() bool {
		for _, e := range drainPump(a) {
			p, ok := e.data.(channelMapPayload)
			if e.name == EventChannelMap && ok && p.InputChannels > 0 {
				got = p
			}
		}
		return got.InputChannels > 0
	})

	if got.DeviceKey != wantKey {
		t.Errorf("the width arrived stamped %q, want %q — config.AudioDeviceKeyFor's spelling, "+
			"which is the key the frontend matches its form against", got.DeviceKey, wantKey)
	}
}

func TestTheRoutingWidthIsForgottenWhenTheDeviceGoes(t *testing.T) {
	// The other half: a grid drawn at a width nothing is negotiating is a set of
	// crosspoints over nothing. It goes when the DEVICE goes — not when a session
	// does.
	a, _ := newTestApp(t)
	captureUp(t, a)
	waitFor(t, 5*time.Second, "a width to be recorded", func() bool {
		got, _ := a.GetChannelMap()
		return got.InputChannels > 0
	})

	if err := a.stopCaptureForTeardown(); err != nil {
		t.Fatalf("stopCaptureForTeardown() error = %v", err)
	}

	got, err := a.GetChannelMap()
	if err != nil {
		t.Fatalf("GetChannelMap() error = %v", err)
	}
	if got.InputChannels != 0 || got.DeviceKey != "" {
		t.Fatalf("got %+v after the capture went down, want no width and no device: with no "+
			"pipeline there is nothing this report could be about", got)
	}
}

// ---------------------------------------------------------------------------
// The teardown order
// ---------------------------------------------------------------------------

func TestTheCaptureStepRunsAfterTheSenderInTheOrderedTeardown(t *testing.T) {
	// RULE 2 of the capture layer, read out of the source because the ordering is
	// what makes it safe rather than any value in it: CapturePipeline.Stop refuses
	// with ErrSeamBusy while a send pipeline still holds the seam, because taking
	// a device to NULL underneath a bound proxysrc is measured SILENT in every
	// direction — 0 buffers, no EOS, no ERROR and no WARNING on either bus, with
	// the send pipeline still PLAYING.
	raw, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("reading app.go: %v", err)
	}
	src := string(raw)
	teardown := src[strings.Index(src, "func (a *App) teardownOrdered() int {"):]
	teardown = teardown[:strings.Index(teardown, "\n}")]

	sender := strings.Index(teardown, `step("the sender"`)
	capture := strings.Index(teardown, `step("the capture"`)
	ret := strings.Index(teardown, `step("the return monitor"`)
	if sender < 0 || capture < 0 || ret < 0 {
		t.Fatal("teardownOrdered no longer names the sender, the capture and the return monitor " +
			"as steps; this guard has stopped covering anything")
	}
	if !(sender < capture && capture < ret) {
		t.Fatalf("the ordered teardown runs sender@%d capture@%d return@%d; the capture MUST come "+
			"after the sender (the proxysrc consumer has to be gone first) and is placed before "+
			"the return monitor because it holds the exclusive card", sender, capture, ret)
	}
}

func TestTeardownReleasesTheCapture(t *testing.T) {
	// A wslcomms left running after a match is a support call, and a wslcomms
	// holding the DeckLink after its window has gone is worse: nothing else on
	// the machine can open the card until the process dies.
	a, _ := newTestApp(t)
	captureUp(t, a)

	a.teardown()

	if got := len(a.currentCapture().Pipelines()); got != 0 {
		t.Fatalf("teardown left %d capture pipeline(s) open; the card is exclusive and the "+
			"operator cannot get it back without killing the process", got)
	}
	if got := a.GetCaptureState(); got.Picture != captureStateOff || got.Commentary != captureStateOff {
		t.Errorf("after teardown the capture reports %+v, want both legs %q", got, captureStateOff)
	}
}

// ---------------------------------------------------------------------------
// A device that will not open is a STATE, not a failed launch
// ---------------------------------------------------------------------------

func TestACardThatWillNotOpenLeavesTheSeatOnTheSlate(t *testing.T) {
	// PLAN.md 0-BIS A1's FIRST MITIGATION, and it is what makes holding the
	// DeckLink from launch to quit safe: the application NEVER fails to come up
	// because of the card. It falls back to the slate, says which of the two the
	// operator is looking at, and offers Restart capture.
	//
	// Without it, a machine whose SDI cable was unplugged overnight would open to
	// nothing at all — no meters, no routing, no picture — over a fault that costs
	// one cable.
	gst.SetStubCaptureStartError(func(legs gst.CaptureLegs) error {
		if legs.Picture == gst.PictureCard {
			return errors.New("the card is held by another application")
		}
		return nil
	})
	t.Cleanup(func() { gst.SetStubCaptureStartError(nil) })

	a, _ := newTestApp(t)
	cfg := validConfig()
	cfg.VideoSource = config.VideoSourceDeckLink
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	if err := a.rebuildCapture("a test asked for one"); err != nil {
		t.Fatalf("the capture build returned an error (%v); a card that will not open must leave "+
			"the seat USABLE, with the fault reported as a state", err)
	}

	set := a.currentCapture()
	if set.Commentary == nil {
		t.Fatal("the commentary capture is down because the CARD would not open; the microphone " +
			"is on a platform endpoint and has nothing to do with it")
	}
	if set.Picture == nil {
		t.Fatal("there is no picture capture at all; the slate fallback did not happen and this " +
			"seat has no video to send")
	}
	if set.Picture.Legs().Picture != gst.PictureSlate {
		t.Fatalf("the picture leg is %v, want the slate: the card refused and the fallback is what "+
			"keeps the position on air", set.Picture.Legs().Picture)
	}

	got := a.GetCaptureState()
	if got.Picture != captureStateFailed {
		t.Errorf("the picture leg reports %q, want %q: the operator asked for their camera and is "+
			"not getting it, whatever is being sent instead", got.Picture, captureStateFailed)
	}
	if got.Commentary != captureStateLive {
		t.Errorf("the commentary leg reports %q, want %q", got.Commentary, captureStateLive)
	}
	if !strings.Contains(got.Reason, "slate") || !strings.Contains(got.Reason, "Restart capture") {
		t.Errorf("Reason = %q; it must say what IS going out — so nobody reads this as 'no video' "+
			"— and name the recovery control", got.Reason)
	}
}

func TestAFusedSeatWhoseCardIsGoneStaysDownAndRefusesStart(t *testing.T) {
	// The other half of A1, and the case with no fallback available: the
	// commentary is on the card too, so dropping it would take the microphone
	// with the picture. The seat stays down, the reason is on screen, and START
	// refuses WITH THE SAME SENTENCE rather than inventing a second description
	// of one fault.
	gst.SetStubCaptureStartError(func(gst.CaptureLegs) error {
		return errors.New("the card is held by another application")
	})
	t.Cleanup(func() { gst.SetStubCaptureStartError(nil) })

	a, _ := newTestApp(t)
	cfg := validConfig()
	cfg.VideoSource = config.VideoSourceDeckLink
	cfg.AudioSourceKind = config.AudioSourceDeckLink
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	buildErr := a.rebuildCapture("a test asked for one")
	if buildErr == nil {
		t.Fatal("the fused build reported success with the card refusing every leg")
	}
	if len(a.currentCapture().Pipelines()) != 0 {
		t.Fatal("a failed fused build left pipelines behind; a half-built capture on an exclusive " +
			"card is the state a later Restart capture cannot recover from")
	}
	if got := a.GetCaptureState(); got.Picture != captureStateFailed || got.Commentary != captureStateFailed {
		t.Fatalf("the capture reports %+v, want both legs %q", got, captureStateFailed)
	}

	useStartingSender(a)
	startErr := a.Start()
	if startErr == nil {
		t.Fatal("START succeeded with no capture open; the send pipeline's two sources are " +
			"proxysrcs, so it would have gone on air over nothing")
	}
	if !strings.Contains(startErr.Error(), "card") {
		t.Errorf("START failed with %q, which does not name the card; the refusal must be the same "+
			"sentence the capture panel is already showing, not a second description of one fault",
			startErr)
	}
}

func TestAPreviewThatCannotGetAWindowStillLeavesTheCaptureUp(t *testing.T) {
	// The retry-without-preview, which is a SPARED failure by design: the
	// confidence monitor is the operator's own picture and nothing about the feed,
	// the audio or the routing depends on it. A build that gave up because a
	// window would not take a GL context would trade a whole position for a
	// convenience.
	gst.SetStubCaptureStartError(func(legs gst.CaptureLegs) error {
		if legs.Preview {
			return errors.New("no GL context for the overlay")
		}
		return nil
	})
	t.Cleanup(func() { gst.SetStubCaptureStartError(nil) })

	a, _ := newTestApp(t)
	cfg := validConfig()
	cfg.VideoSource = config.VideoSourceDeckLink
	cfg.DeckLinkPreviewEnabled = true
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	if err := a.rebuildCapture("a test asked for one"); err != nil {
		t.Fatalf("the capture build failed over the PREVIEW (%v); it is the one branch whose "+
			"failure must never cost the position its meters", err)
	}
	if got := a.GetCaptureState(); got.Picture != captureStateLive || got.Commentary != captureStateLive {
		t.Fatalf("the capture reports %+v, want both legs %q", got, captureStateLive)
	}
}
