//go:build dev || production || bindings

// Tests for the COUGH MUTE's half of the wire-up (app.go), and for the one
// method that exists to tell the page the truth about the preview.
//
// The mute MECHANISM is tested in internal/gst — whether the property write
// lands, whether the far end sees a discontinuity, whether the meter keeps
// moving. What is tested HERE is everything between that mechanism and the
// operator's finger, and four of those things can only be got wrong at this
// layer:
//
//   - THE ORDER OF A PRESS. A push-to-mute calls this on key-down and again on
//     key-up, and Wails runs each bound call on a goroutine of its own with no
//     ordering between them. A key-up applied before its own key-down leaves the
//     microphone dead on air with nobody holding a key, and nothing downstream
//     of here could ever detect it.
//
//   - THE STATE AT A SESSION BOUNDARY. Start must begin unmuted and Stop must
//     leave nothing latched, on the self-stop path as well as the operator's.
//     Coming on air muted because of a flag left by the last session is the
//     failure this whole feature would otherwise introduce.
//
//   - WHAT THE SCREEN IS TOLD. A mute is SILENT: a page showing an unmuted
//     commentator who is not being heard is wrong in the only direction that
//     costs a match. So the event, not the click, is the authority, and the
//     record must be readable without taking a lock the main thread would block
//     on.
//
//   - WHO DID IT. A remote seat may mute (app_remote.go argues why), and the
//     whole argument rests on the desk always being able to see that it is muted
//     and by whom. If the attribution is not on the wire, the decision was wrong.
package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
	"wslcomms/internal/sender"
)

// ---------------------------------------------------------------------------
// Nothing is sending — which is no longer a reason to refuse
// ---------------------------------------------------------------------------

// TestAMuteLatchedBeforeStartIsAcceptedAndCarried is the operator's A2 ruling
// (PLAN.md 0-BIS), and it REVERSES the test that stood here.
//
// That test asserted the refusal, on the argument written out beside
// SetCommentaryMute: a mute accepted before START would have to be either
// forgotten (a control that lies) or carried into the session (a commentator who
// comes on air muted because of something pressed twenty minutes earlier).
//
// The argument was sound about a build in which the volume element came into
// existence at START. Always-live capture creates the third state whether or not
// anybody wants it, the operator was asked, and the answer was CARRY IT — because
// the fear was of a control that LIES, and it is met by VISIBILITY instead: the
// mute sits upstream of alevel, so a muted commentator has a flat programme meter
// AND a mute banner, before and after START. Both halves of that are asserted
// here, and the answer is written out where the argument stood in app.go.
func TestAMuteLatchedBeforeStartIsAcceptedAndCarried(t *testing.T) {
	a, _ := newTestApp(t)
	captureUp(t, a)

	got, err := a.SetCommentaryMute(true, 1)
	if err != nil {
		t.Fatalf("SetCommentaryMute before START error = %v; the element exists from launch and "+
			"the operator has ruled that a latch set now is carried into the session", err)
	}
	if !got.Muted || !got.Available {
		t.Fatalf("got %+v, want a live, muted control on a capture that is open", got)
	}

	// AND IT IS STILL THERE AT START. This is the half the old design refused in
	// order to avoid; it is now the behaviour, and it is only safe because the
	// meter and the banner have been showing it the whole time.
	useStartingSender(a)
	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })

	if state := a.GetCommentaryMute(); !state.Muted {
		t.Fatalf("the session came up UNMUTED (%+v) over a latch the operator set before it; "+
			"START silently discarding a live control is the failure A2 chose against", state)
	}
}

func TestMuteWithNoCaptureIsRefusedAndPointsAtTheDevice(t *testing.T) {
	// The one refusal that is left, and it is a different fact from the one that
	// went: there is no capture pipeline, so there is no element to write to.
	// The sentence must therefore point at the DEVICE and at Restart capture, and
	// never at START — pressing START would change nothing about whether this
	// call can be served.
	a, _ := newTestApp(t)

	got, err := a.SetCommentaryMute(true, 1)
	if err == nil {
		t.Fatal("SetCommentaryMute succeeded with no capture open; there is no volume element to " +
			"write to, and reporting success would be a control that lies")
	}
	if !errors.Is(err, errMuteNoCapture) {
		t.Errorf("error = %v, want errMuteNoCapture", err)
	}
	// The message has to say what to DO, not merely what is wrong.
	if !strings.Contains(err.Error(), "Restart capture") {
		t.Errorf("error %q does not say how to recover; the operator is left with a fact and no "+
			"action", err)
	}
	if strings.Contains(err.Error(), "START") {
		t.Errorf("error %q still sends the operator to START, which cannot help: the capture is "+
			"built at launch and START does not open a device", err)
	}
	if got.Muted {
		t.Error("the refused call reported Muted; a refusal must not move the state it refused to change")
	}
	if got.Available {
		t.Error("Available is true with no capture; the control must draw itself as not yet live")
	}
}

func TestGetCommentaryMuteBeforeAnyCaptureExplainsItself(t *testing.T) {
	a, _ := newTestApp(t)

	got := a.GetCommentaryMute()
	if got.Muted || got.Available {
		t.Errorf("got %+v, want unmuted and unavailable on a machine with no capture open", got)
	}
	// The zero value must still carry a sentence: a disabled control with no
	// explanation is the defect the Reason field exists to remove.
	if got.Reason == "" {
		t.Error("Reason is empty on the zero value; a control that is switched off must say why")
	}
	if strings.Contains(got.Reason, "press START") {
		t.Errorf("Reason %q tells the operator to press START, which no longer makes the control "+
			"live; the element exists from launch", got.Reason)
	}
	if got.By != "" {
		t.Errorf("By = %q with nothing muted; the seat is a fact about a mute that exists", got.By)
	}
}

// ---------------------------------------------------------------------------
// The order of a press
// ---------------------------------------------------------------------------

func TestAKeyUpThatOvertookItsKeyDownDoesNotLeaveTheMicrophoneDead(t *testing.T) {
	// THE failure this mechanism exists for. Push-to-mute sends true on key-down
	// and false on key-up; Wails gives each call its own goroutine and no
	// ordering. If the key-up lands first and the key-down is then applied on
	// top, the commentary is muted with nobody holding a key and nothing will
	// ever clear it.
	a, _ := newTestApp(t)
	startedSession(t, a)

	// The key-up (stamp 200) arrives and is applied. The key-down (stamp 100) —
	// the older half of the same press — arrives afterwards.
	if _, err := a.SetCommentaryMute(false, 200); err != nil {
		t.Fatalf("the key-up error = %v", err)
	}
	got, err := a.SetCommentaryMute(true, 100)
	if err != nil {
		t.Fatalf("the overtaken key-down error = %v; a request that lost a race is not an error, "+
			"it is a request whose outcome was already decided", err)
	}
	if got.Muted {
		t.Fatal("the stale key-down was applied: the commentary is muted with nobody holding a key. " +
			"This is a dead microphone on air that nothing in the system would ever clear")
	}
	if a.GetCommentaryMute().Muted {
		t.Fatal("the recorded state is muted after a stale key-down")
	}
}

func TestAStaleMuteStopsBeingStaleSoNothingIsWedgedForever(t *testing.T) {
	// The other direction, and the reason staleness is bounded in real time
	// rather than by the stamp alone. A high-water mark that never expired would
	// mean a page reload restarting a counter, a clock that stepped, or a second
	// seat whose stamps run behind, left the cough control refusing every press
	// FOREVER — which is the same dead microphone arrived at from the other side.
	a, _ := newTestApp(t)
	startedSession(t, a)

	if _, err := a.SetCommentaryMute(false, 1_000_000); err != nil {
		t.Fatalf("SetCommentaryMute error = %v", err)
	}

	// Age the record past the window, exactly as wall-clock time would.
	a.muteMu.Lock()
	a.muteSeqAt = time.Now().Add(-2 * muteStaleWindow)
	a.muteMu.Unlock()

	// A far older stamp — a counter that restarted at 1 — must now be honoured.
	got, err := a.SetCommentaryMute(true, 1)
	if err != nil {
		t.Fatalf("SetCommentaryMute error = %v", err)
	}
	if !got.Muted {
		t.Fatal("a press was still being dropped as stale long after any reordering could have " +
			"happened; the control is wedged, which is the failure the window exists to bound")
	}
}

func TestRapidPushToMuteIsSafeToHammer(t *testing.T) {
	// The call is made on a key-down and a key-up, dozens of times in a match,
	// from a control the operator is holding while talking over it. This drives
	// the two halves concurrently and only asserts what must be true: no panic,
	// no data race (this suite runs under -race), and a defined state at the end.
	a, _ := newTestApp(t)
	startedSession(t, a)

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(2)
		down, up := float64(2*i+1), float64(2*i+2)
		go func() { defer wg.Done(); a.SetCommentaryMute(true, down) }()
		go func() { defer wg.Done(); a.SetCommentaryMute(false, up) }()
	}
	wg.Wait()

	// The last stamp issued was a key-UP, so the highest stamp anything can have
	// applied is an unmute. Whatever order the goroutines actually ran in, the
	// ordering rule makes this the only state the record may be left in.
	if got := a.GetCommentaryMute(); got.Muted {
		t.Errorf("after a storm of presses the state is %+v; the newest stamp was a release, so a "+
			"mute here means the ordering rule did not hold under contention", got)
	}
}

// ---------------------------------------------------------------------------
// Session boundaries
// ---------------------------------------------------------------------------

func TestTheCaptureComingUpMakesTheMuteLiveAndSaysSo(t *testing.T) {
	// The moment the cough control becomes live is now the CAPTURE BUILD and no
	// longer START, because the volume element belongs to the pipeline that has
	// the microphone open. A page that had been showing a disabled control has to
	// learn that it is live, and there is no getter it polls — so the event is the
	// only way it can.
	a, _ := newTestApp(t)
	silencePump(a)
	captureUp(t, a)

	got := a.GetCommentaryMute()
	if got.Muted {
		t.Fatal("a freshly built capture came up MUTED with nothing latched; the state published " +
			"must be the one read back off the element")
	}
	if !got.Available {
		t.Error("Available is false over an open capture; the cough control must be live")
	}
	if got.Reason != "" {
		t.Errorf("Reason = %q on an available control; the sentence is for when it is NOT", got.Reason)
	}

	if !containsEvent(drainPump(a), EventMute) {
		t.Error("no mute event was emitted when the capture came up; a page that had been showing " +
			"a disabled control has no way to learn that it is now live")
	}
}

func TestAMuteHeldAcrossStopIsStillHeldAtTheNextStart(t *testing.T) {
	// REVERSED BY THE OPERATOR'S A2 RULING, and this test used to assert the
	// opposite: that a mute latched before the last STOP was cleared by the next
	// START, on the grounds that a commentator inaudible because of something
	// pressed twenty minutes earlier is the worst outcome available.
	//
	// What answers that now is not the session boundary but the SCREEN. The mute
	// is upstream of the programme meter, the meter is live from launch, and the
	// mute banner is drawn from an event that goes out on every change — so the
	// twenty minutes in that sentence are twenty minutes of a flat meter beside a
	// red badge, on both seats. A boundary that silently discarded the operator's
	// own live control was the other way to be wrong, and it is the one they
	// chose against.
	a, _ := newTestApp(t)
	captureUp(t, a)
	useStartingSender(a)

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := a.SetCommentaryMute(true, 1); err != nil {
		t.Fatalf("SetCommentaryMute error = %v", err)
	}
	if !a.GetCommentaryMute().Muted {
		t.Fatal("the mute did not take, so this test would pass for the wrong reason")
	}
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	if got := a.GetCommentaryMute(); !got.Muted {
		t.Fatalf("STOP cleared the mute (%+v); the element it acts on is still open and still "+
			"muted, so the control must go on saying so", got)
	}

	if err := a.Start(); err != nil {
		t.Fatalf("the second Start() error = %v", err)
	}
	defer a.Stop()

	if got := a.GetCommentaryMute(); !got.Muted {
		t.Fatalf("the new session came up UNMUTED (%+v): START discarded a latch the operator "+
			"was holding, which is a live control that lies about its own state", got)
	}
}

func TestStopLeavesTheCoughControlExactlyWhereItWas(t *testing.T) {
	// The other half of R1's promise, and the one an operator notices: everything
	// they were watching survives STOP. The cough control is the sharpest case,
	// because clearing it would UNMUTE a commentator who is still being metered.
	a, _ := newTestApp(t)
	captureUp(t, a)
	useStartingSender(a)

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := a.SetCommentaryMute(true, 1); err != nil {
		t.Fatalf("SetCommentaryMute error = %v", err)
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	got := a.GetCommentaryMute()
	if !got.Muted {
		t.Error("the mute was cleared by the end of the session; the capture that owns the element " +
			"is still open")
	}
	if !got.Available {
		t.Error("Available went false at STOP; the control acts on a capture pipeline, which STOP " +
			"does not touch")
	}
	if got.By != muteSeatDesk {
		t.Errorf("the seat that muted was forgotten (%+v) over a mute that is still in force", got)
	}
}

func TestATeardownClearsTheMuteBecauseTheElementHasGone(t *testing.T) {
	// The moment the control really does go away: the capture is torn down. That
	// is a device change, a Restart capture or the application quitting — and NOT
	// a STOP. Without the event a latched cough button stays red over a pipeline
	// that no longer exists, which is a control the operator cannot release.
	a, _ := newTestApp(t)
	captureUp(t, a)

	if _, err := a.SetCommentaryMute(true, 1); err != nil {
		t.Fatalf("SetCommentaryMute error = %v", err)
	}

	silencePump(a)
	drainPump(a)
	if err := a.stopCaptureForTeardown(); err != nil {
		t.Fatalf("stopCaptureForTeardown() error = %v", err)
	}

	if !containsEvent(drainPump(a), EventMute) {
		t.Error("no mute event when the capture went down; the button stays red over an element " +
			"that no longer exists")
	}
	got := a.GetCommentaryMute()
	if got.Muted {
		t.Error("the mute survived the teardown in the record")
	}
	if got.Available {
		t.Error("Available is true with no capture; the control must go back to being not-yet-live")
	}
	if got.By != "" || got.ByAddr != "" {
		t.Errorf("the seat that muted is still recorded (%+v) against a mute that no longer exists", got)
	}
}

func TestASelfStoppedSessionLeavesTheMuteAlone(t *testing.T) {
	// The path App.Stop does not cover: a sender that stops itself. It used to
	// clear the mute, because it used to take the capture with it; it no longer
	// touches the capture at all, so the control the operator is holding stays
	// exactly as they left it. That is the same answer the operator-Stop path
	// gives, which is the property worth pinning — a mute must not depend on WHICH
	// way a session ended.
	a, _ := newTestApp(t)
	captureUp(t, a)
	latest := useStartingSender(a)

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := a.SetCommentaryMute(true, 1); err != nil {
		t.Fatalf("SetCommentaryMute error = %v", err)
	}

	latest().stop()

	waitFor(t, 5*time.Second, "the session to end", func() bool {
		return !a.sessionUp.Load()
	})
	if got := a.GetCommentaryMute(); !got.Muted || !got.Available {
		t.Fatalf("a self-stopped session moved the cough control (%+v); it acts on a capture "+
			"pipeline, which the sender stopping does not touch", got)
	}
}

// ---------------------------------------------------------------------------
// Who did it
// ---------------------------------------------------------------------------

func TestARemoteMuteIsAttributedSoTheDeskCanAccountForIt(t *testing.T) {
	// The condition the whole "a remote seat may mute" decision rests on. If the
	// attribution is not on the wire, a producer's mute is indistinguishable
	// from a fault, and the decision in remoteAllowlist was wrong.
	a, _ := newTestApp(t)
	startedSession(t, a)

	got, err := a.setCommentaryMuteFrom(muteSeatRemote, "10.0.0.42:51001", true, 1)
	if err != nil {
		t.Fatalf("setCommentaryMuteFrom error = %v", err)
	}
	if !got.Muted {
		t.Fatal("the remote mute did not take")
	}
	if got.By != muteSeatRemote {
		t.Errorf("By = %q, want %q: a mute the desk cannot attribute is a mute the desk cannot "+
			"explain, which is the nightmare this field exists to prevent", got.By, muteSeatRemote)
	}
	if got.ByAddr != "10.0.0.42:51001" {
		t.Errorf("ByAddr = %q, want the seat's source address: with no login on the listener, "+
			"where it came from is the only identity there is", got.ByAddr)
	}

	// And a mute from the desk is NOT dressed up as a remote one.
	got, err = a.SetCommentaryMute(false, 2)
	if err != nil {
		t.Fatalf("SetCommentaryMute error = %v", err)
	}
	if got.By != "" || got.ByAddr != "" {
		t.Errorf("got %+v after unmuting; the seat is a fact about a mute that exists, and keeping "+
			"a remote address against an unmuted feed puts an unexplainable label on the desk's screen", got)
	}
}

func TestAMuteHeldByASeatThatVanishedIsReleased(t *testing.T) {
	// The mute's worst case: a producer's laptop holds the mute for a cough and
	// its wifi drops. Nothing else in the system would ever clear it — the seat
	// that set it is gone, and the desk is looking at a badge naming a machine
	// that is not answering.
	a, _ := newTestApp(t)
	startedSession(t, a)

	if _, err := a.setCommentaryMuteFrom(muteSeatRemote, "10.0.0.42:51001", true, 1); err != nil {
		t.Fatalf("setCommentaryMuteFrom error = %v", err)
	}

	// A poll in which that seat is still present must change nothing: a mute held
	// by a connected seat is that seat's to release.
	a.releaseMuteHeldByGoneSeat(map[string]bool{"10.0.0.42:51001": true})
	if !a.GetCommentaryMute().Muted {
		t.Fatal("the mute was released while the seat holding it was still connected")
	}

	// And a poll in which it is gone must release it.
	a.releaseMuteHeldByGoneSeat(map[string]bool{"10.0.0.99:2000": true})
	if got := a.GetCommentaryMute(); got.Muted {
		t.Fatalf("the mute survived the seat that was holding it (%+v); the commentary is silent "+
			"for the rest of the match with nobody able to clear it", got)
	}
}

func TestADeskMuteIsNeverReleasedByTheRemotePoll(t *testing.T) {
	// The operator is sitting in front of their own cough button. A poll about
	// somebody else's connection must never reach across and release it.
	a, _ := newTestApp(t)
	startedSession(t, a)

	if _, err := a.SetCommentaryMute(true, 1); err != nil {
		t.Fatalf("SetCommentaryMute error = %v", err)
	}
	a.releaseMuteHeldByGoneSeat(map[string]bool{})
	if !a.GetCommentaryMute().Muted {
		t.Fatal("the desk's own mute was released by a remote-client poll; the operator's finger " +
			"is on the button and nothing about a remote connection concerns it")
	}
}

// ---------------------------------------------------------------------------
// The screen follows the pipeline, not its own last click
// ---------------------------------------------------------------------------

func TestReconciliationPublishesTheTruthWhenThePipelineDisagrees(t *testing.T) {
	// If the mute is cleared underneath us — anything internal/gst does to
	// recover — the screen must find out rather than go on drawing the last
	// thing it asked for.
	a, _ := newTestApp(t)
	startedSession(t, a)

	if _, err := a.SetCommentaryMute(true, 1); err != nil {
		t.Fatalf("SetCommentaryMute error = %v", err)
	}

	// The capture pipeline loses the mute without this side being told, which is
	// exactly what a rebuild would look like from here.
	cap, _ := a.currentCommentary()
	if cap == nil {
		t.Fatal("no capture pipeline")
	}
	if err := cap.SetCommentaryMute(false); err != nil {
		t.Fatalf("clearing the mute on the capture pipeline directly: %v", err)
	}

	silencePump(a)
	drainPump(a)
	a.reconcileMute(cap)

	if !containsEvent(drainPump(a), EventMute) {
		t.Error("the reconciliation found the pipeline unmuted and published nothing; the screen " +
			"goes on showing a mute that is not in force")
	}
	if a.GetCommentaryMute().Muted {
		t.Error("the record still says muted after reconciling against an unmuted pipeline")
	}
}

func TestDomReadyReplaysTheMuteBecauseAMuteIsSilent(t *testing.T) {
	// Every other lamp recovers by itself — a sender reconnects, a return monitor
	// retries. A mute makes no sound and emits only on change, so a page that
	// reloaded while one was latched would come back showing an unmuted
	// commentator who is not being heard.
	a, _ := newTestApp(t)
	startedSession(t, a)

	if _, err := a.SetCommentaryMute(true, 1); err != nil {
		t.Fatalf("SetCommentaryMute error = %v", err)
	}

	silencePump(a)
	drainPump(a)
	a.domReady(context.Background())

	var replayed *mutePayload
	for _, e := range drainPump(a) {
		if e.name != EventMute {
			continue
		}
		if p, ok := e.data.(mutePayload); ok {
			replayed = &p
		}
	}
	if replayed == nil {
		t.Fatal("domReady replayed no mute; a page that reloaded mid-mute is never told")
	}
	if !replayed.Muted {
		t.Errorf("the replayed mute is %+v, want muted: the page must come back in step with the "+
			"microphone rather than with its own forgotten click", *replayed)
	}
}

// ---------------------------------------------------------------------------
// The preview's honest answer
// ---------------------------------------------------------------------------

// TestPreviewStateSaysAPreviewNoLongerWaitsForStart replaces
// TestPreviewStateBeforeStartExplainsItselfRatherThanGoingBlank, which asserted
// BeforeStart false "because the card measurements say it cannot".
//
// That was never quite what the measurements said, and previewStatePayload's own
// comment recorded the difference at length: internal/gst measured the preview
// rendering perfectly with the pipeline started and no sink installed, so the
// missing piece was this APPLICATION splitting "bring the capture up" from "go
// on air" — not the card. The always-live capture layer IS that split, and this
// field was deliberately written as a field rather than as a frontend constant
// so that the day it happened, one line would carry it.
//
// What still has to be true is the half that made the old test worth having: a
// preview that is NOT on screen must say why, in a sentence, rather than leaving
// a black rectangle that reads as a fault.
func TestPreviewStateSaysAPreviewNoLongerWaitsForStart(t *testing.T) {
	a, _ := newTestApp(t)

	cfg := validConfig()
	cfg.VideoSource = config.VideoSourceDeckLink
	cfg.DeckLinkPreviewEnabled = true
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	got := a.GetPreviewState()
	if !got.BeforeStart {
		t.Fatal("BeforeStart is false: it tells the page a confidence picture needs a session, " +
			"which stopped being true when the picture capture moved to domReady")
	}
	if got.Running {
		// No NSApplication in a `go test` binary, so no overlay surface: the
		// pipeline could exist and the window cannot.
		t.Fatal("Running is true with no overlay surface")
	}
	if got.Reason == "" {
		t.Fatal("Reason is empty; the page has nothing to say and draws an empty panel, which " +
			"reads as a fault on a machine that is working perfectly")
	}
	// And the reason must no longer be an instruction to press START, because
	// pressing it would not put a picture there.
	if strings.Contains(got.Reason, "START") {
		t.Errorf("Reason %q still tells the operator to press START for a picture that does not "+
			"wait for one", got.Reason)
	}
}

func TestPreviewStateSaysWhenItIsSwitchedOffAndWhenThereIsNothingToSee(t *testing.T) {
	// Three different silences that must not share one sentence: switched off,
	// sending the slate, and not started yet. An operator who is told the wrong
	// one goes looking for a fault in the wrong place.
	a, _ := newTestApp(t)

	off := validConfig()
	off.VideoSource = config.VideoSourceDeckLink
	off.DeckLinkPreviewEnabled = false
	a.cfgMu.Lock()
	a.cfg = off
	a.cfgMu.Unlock()
	offReason := a.GetPreviewState().Reason

	slate := validConfig()
	slate.VideoSource = config.VideoSourceSlate
	slate.DeckLinkPreviewEnabled = true
	a.cfgMu.Lock()
	a.cfg = slate
	a.cfgMu.Unlock()
	slateReason := a.GetPreviewState().Reason

	notSending := validConfig()
	notSending.VideoSource = config.VideoSourceDeckLink
	notSending.DeckLinkPreviewEnabled = true
	a.cfgMu.Lock()
	a.cfg = notSending
	a.cfgMu.Unlock()
	notSendingReason := a.GetPreviewState().Reason

	for name, r := range map[string]string{
		"switched off":  offReason,
		"sending slate": slateReason,
		"not started":   notSendingReason,
	} {
		if r == "" {
			t.Errorf("%s: Reason is empty", name)
		}
	}
	if offReason == slateReason || slateReason == notSendingReason || offReason == notSendingReason {
		t.Error("two of the three reasons are the same sentence; an operator told the wrong one " +
			"goes looking for a fault that is not there")
	}
}

// ---------------------------------------------------------------------------
// The mode is a setting; the mute is not
// ---------------------------------------------------------------------------

func TestCoughMuteModeSurvivesASaveThatDoesNotMentionIt(t *testing.T) {
	// SaveConfig is a whole-object write from a page cache with no field-level
	// merge, which is right for fields a screen owns and wrong for one it does
	// not know about yet. An operator who chose latch and then saved a port
	// number would find their cough button back on push — discovered mid-match,
	// by pressing it.
	a, _ := newTestApp(t)

	chosen := validConfig()
	chosen.CoughMuteMode = config.CoughMuteModeLatch
	if err := a.SaveConfig(chosen); err != nil {
		t.Fatalf("SaveConfig error = %v", err)
	}

	// A save from a page that has never heard of the field.
	unaware := validConfig()
	unaware.CoughMuteMode = ""
	if err := a.SaveConfig(unaware); err != nil {
		t.Fatalf("SaveConfig error = %v", err)
	}

	got, err := a.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig error = %v", err)
	}
	if got.EffectiveCoughMuteMode() != config.CoughMuteModeLatch {
		t.Errorf("coughMuteMode = %q after an unrelated save, want %q preserved",
			got.EffectiveCoughMuteMode(), config.CoughMuteModeLatch)
	}
}

func TestNoConfigFieldRemembersWhetherSomebodyIsMuted(t *testing.T) {
	// The decision argued in internal/presets/fields.go, pinned here so it cannot
	// be undone by somebody adding "one small boolean". config.json is read at
	// launch and applied before anybody is at the desk, so a remembered mute is a
	// commentator whose microphone is dead when they arrive, with every lamp
	// green and the one control that would explain it drawn as somebody else left
	// it three days ago.
	for _, forbidden := range []string{"coughMuted", "muted", "commentaryMuted", "coughMute"} {
		if configHasJSONTag(forbidden) {
			t.Errorf("config.Config serialises %q: a mute that survives a restart is a commentator "+
				"who comes on air silent. It is live state and belongs on the pipeline", forbidden)
		}
	}
	// And the MODE, which is the opposite case, is present: forgetting it costs
	// one press, and remembering the mute costs a match.
	if !configHasJSONTag("coughMuteMode") {
		t.Error("config.Config no longer serialises coughMuteMode; the operator's choice between " +
			"a held and a latched cough key is theirs and must persist")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// containsEvent reports whether name appears among the drained events.
func containsEvent(events []pumpEvent, name string) bool {
	for _, e := range events {
		if e.name == name {
			return true
		}
	}
	return false
}

// captureUp builds the ALWAYS-LIVE CAPTURE LAYER, which is what domReady does at
// launch and what almost every test in this file now needs instead of a session.
//
// The cough mute, the routing grid and the meters all belong to the capture
// pipeline; a session neither creates nor destroys it. A test that started a
// session to get a mute would be testing the lifetime this change removed.
func captureUp(t *testing.T, a *App) {
	t.Helper()
	if err := a.rebuildCapture("a test asked for one"); err != nil {
		t.Fatalf("rebuildCapture() error = %v; the stub capture must come up for this test to "+
			"mean anything", err)
	}
}

// startedSession runs a session whose pipeline is ACTUALLY STARTED, and returns
// the running pipeline.
//
// withFakeSender alone is not enough for anything about the mute. Its fake
// sender accepts Start and never touches the pipeline it was handed, which is
// exactly right for the reaper tests it was written for and wrong here:
// internal/gst refuses SetCommentaryMute on a pipeline that has not started —
// deliberately, because a pipeline that must begin muted is started with
// PipelineOpts.MuteCommentary rather than latched afterwards — so every mute in
// such a session would fail for a reason that has nothing to do with what is
// being tested.
//
// So this fake starts the pipeline with the options App built for it, which is
// what the real sender does, and the mute then behaves as it does in the
// application.
func startedSession(t *testing.T, a *App) gst.Pipeline {
	t.Helper()

	captureUp(t, a)
	useStartingSender(a)
	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })

	pipe := a.currentSend()
	if pipe == nil {
		t.Fatal("Start() left no send pipeline")
	}
	return pipe
}

// useStartingSender installs the dial WITHOUT starting a session, for the tests
// that need to drive the boundaries themselves — two Starts in a row, or a
// sender that stops itself. It returns a function yielding the most recently
// dialled fake.
func useStartingSender(a *App) (latest func() *pipelineStartingSender) {
	var mu sync.Mutex
	var current *pipelineStartingSender
	a.senderDial = func(pipe gst.Pipeline) sender.Sender {
		f := &pipelineStartingSender{pipe: pipe, states: make(chan sender.State, 8)}
		mu.Lock()
		current = f
		mu.Unlock()
		return f
	}
	return func() *pipelineStartingSender {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
}

// pipelineStartingSender is a sender that does the one thing the mute needs from
// the real one — take the pipeline to PLAYING — and nothing else.
type pipelineStartingSender struct {
	pipe   gst.Pipeline
	states chan sender.State
	once   sync.Once
}

func (f *pipelineStartingSender) Start(opts sender.Opts) error {
	if err := f.pipe.Start(opts.Pipeline); err != nil {
		return err
	}
	f.states <- sender.StateConnected
	return nil
}

func (f *pipelineStartingSender) Stop() error {
	f.stop()
	return nil
}

func (f *pipelineStartingSender) States() <-chan sender.State { return f.states }

// stop is the end of the session by either route: emit StateStopped and close,
// exactly as the real sender does as the last act of stopping.
func (f *pipelineStartingSender) stop() {
	f.once.Do(func() {
		_ = f.pipe.Stop()
		f.states <- sender.StateStopped
		close(f.states)
	})
}

var _ sender.Sender = (*pipelineStartingSender)(nil)

// configHasJSONTag reports whether config.Config serialises the given top-level
// json tag. It reflects the struct tags rather than marshalling an instance,
// because omitempty would hide a field whose zero value is false — which is
// exactly the shape any "is anybody muted" field would have.
func configHasJSONTag(tag string) bool {
	typ := reflect.TypeOf(config.Config{})
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == tag {
			return true
		}
	}
	return false
}

// Compile-time statement of the seam this file exercises: the two methods the
// cough control drives are on gst.CapturePipeline itself, so there is no build in
// which the button exists and does nothing.
//
// IT NAMES THE CAPTURE PIPELINE AND NOT THE SEND PIPELINE, and that is where the
// mute has always belonged: it is a volume element in the chain that has the
// microphone open, upstream of the programme meter and of the proxysink. What the
// move buys the operator is that the control is answerable with nothing sending —
// the element exists from launch — and that a reconnect cannot reach it, because
// the object internal/sender drives has no audio leg at all.
var _ interface {
	SetCommentaryMute(bool) error
	CommentaryMuted() bool
} = gst.CapturePipeline(nil)
