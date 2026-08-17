//go:build live && cgo && !gststub

// coughmute_live_test.go is the Gate C measurement of the cough mute, and of the
// ONE question that decides whether a preview can exist before Start.
//
// The two are in one file because they are one run: both need the real
// GStreamer, the real card and the shipped pipeline, and the second is the
// measurement that has to be taken before any preview lifecycle is designed
// rather than after.
//
// It is behind the `live` build tag and never runs in a normal suite. It needs a
// bundled app under build/bin (or WSLCOMMS_LIVE_APP_DIR) and it opens the fitted
// card, so it must never run alongside anything else that wants either.
//
//	PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig CGO_ENABLED=1 \
//	  go test -tags live ./internal/gst/ -run 'TestLiveCough|TestLiveCardRelease' -v -count=1

package gst

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// coughMuteCycles is how many times TestLiveCardReleaseAndReacquire takes the
// card, gives it back and takes it again IN ONE PROCESS.
//
// The number is not a round one for its own sake. A preview that hands the card
// to Start does exactly one release-then-reacquire per match, so what has to be
// established is not an average but the ABSENCE of a failure — and a run that
// never fails tells you only that the failure rate is below roughly 1/N. Twelve
// is what fits in a run short enough that somebody will actually take it before
// changing this design, and it is reported as a bound rather than as a pass.
const coughMuteCycles = 12

// liveMuteWindow is how long each measurement window is held open. The
// programme meter posts every 50 ms, so a second is twenty frames — enough for
// "the meter kept posting" to be a count rather than an impression.
const liveMuteWindow = 1500 * time.Millisecond

// muteSettle is the one meter window discarded either side of a mute write. See
// the comment at its use: it is a measurement artefact, not a latency.
const muteSettle = 200 * time.Millisecond

// TestLiveCoughMuteOnAPlayingPipeline is the measurement the interface's
// promises are written from: instant, no state change, no renegotiation, the
// meter still posting, and the element agreeing with what it was told.
//
// It runs on the SHIPPED pipeline through this package's own Start, not on a
// gst-launch line, because everything being claimed is about the pipeline the
// product actually builds — the element's position between the resampler's
// capsfilter and the programme meter, and the fact that it is reached under the
// same lock ReplaceSink holds.
func TestLiveCoughMuteOnAPlayingPipeline(t *testing.T) {
	liveDeckLinkInit(t)

	slate, err := filepath.Abs(env("WSLCOMMS_LIVE_SLATE", defaultSlatePath))
	if err != nil {
		t.Fatalf("resolving the slate: %v", err)
	}
	card := env("WSLCOMMS_LIVE_CARD", defaultLiveCard)

	meter := &meterWatch{}

	// THE CAPTURE PIPELINE, AND NO SEND PIPELINE ANYWHERE IN THE PROCESS. That is
	// the change the seam makes to this measurement and it strengthens it: the
	// mute, the meter and the element they act on are live with nothing sending,
	// which is the state the operator's cough button is now available in.
	//
	// The commentary off the card and the picture off the slate: the shape a
	// commentary position actually runs in, and the one that exercises the clock
	// companion as well.
	pipe, err := NewCapture(CaptureOpts{
		Legs:           CaptureLegs{Commentary: CommentaryCard},
		SlatePath:      slate,
		AudioCaptureID: card,
		ConformTo:      FallbackConformTarget(),
		OnLevels:       func(l Levels) { meter.record(l) },
	})
	if err != nil {
		t.Fatalf("NewCapture: %v", err)
	}
	defer func() {
		if err := pipe.Stop(); err != nil {
			t.Errorf("Stop: %v — the card may still be held", err)
		}
	}()

	done := make(chan struct{})
	var busMu sync.Mutex
	var busErrs []error
	go func() {
		defer close(done)
		for err := range pipe.Faults() {
			busMu.Lock()
			busErrs = append(busErrs, err)
			busMu.Unlock()
			t.Logf("capture fault: %v", err)
		}
	}()
	go func() {
		for range pipe.Warnings() {
		}
	}()

	if err := pipe.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// THE DEFAULT. A pipeline started without the option must be on air.
	if pipe.CommentaryMuted() {
		t.Fatal("a pipeline started with no MuteCommentary reports MUTED")
	}
	time.Sleep(2 * time.Second) // let the card settle; its first packets are dropped

	unmutedBefore := holdWindow(meter, liveMuteWindow)

	// THE WRITE, timed. This is the whole latency claim.
	t0 := time.Now()
	if err := pipe.SetCommentaryMute(true); err != nil {
		t.Fatalf("SetCommentaryMute(true): %v", err)
	}
	elapsed := time.Since(t0)

	if !pipe.CommentaryMuted() {
		t.Error("SetCommentaryMute(true) returned nil and CommentaryMuted() is false. The " +
			"read-back is the whole mechanism and it has disagreed with the write")
	}
	// THE PIPELINE MUST NOT HAVE MOVED. A mute that changed state, or left one
	// pending, is not the mechanism this was chosen to be.
	assertStillPlaying(t, pipe, "after muting")

	// SETTLE ONE METER WINDOW BEFORE MEASURING. alevel reports over 50 ms, so
	// the window the write lands inside describes audio from both sides of it
	// and reads as loud as the unmuted stream. holdWindow takes a peak HOLD —
	// the loudest frame in the window — so a single straddling frame would set
	// the whole reading, which is exactly the error that made the first run of
	// this test report a mute that had in fact worked. Discarding one window is
	// not the mute being slow: the write below was measured at tens of
	// microseconds.
	time.Sleep(muteSettle)

	muted := holdWindow(meter, liveMuteWindow)

	t0 = time.Now()
	if err := pipe.SetCommentaryMute(false); err != nil {
		t.Fatalf("SetCommentaryMute(false): %v", err)
	}
	unmuteElapsed := time.Since(t0)
	if pipe.CommentaryMuted() {
		t.Error("SetCommentaryMute(false) returned nil and CommentaryMuted() is still true")
	}
	assertStillPlaying(t, pipe, "after unmuting")

	time.Sleep(muteSettle)
	unmutedAfter := holdWindow(meter, liveMuteWindow)

	fmt.Fprintf(os.Stderr, "\n>>>> COUGH MUTE ON A PLAYING PIPELINE\n")
	fmt.Fprintf(os.Stderr, "  mute write      %v\n", elapsed)
	fmt.Fprintf(os.Stderr, "  unmute write    %v\n", unmuteElapsed)
	for _, w := range []struct {
		name string
		win  muteWindow
	}{
		{"unmuted (before)", unmutedBefore},
		{"MUTED", muted},
		{"unmuted (after)", unmutedAfter},
	} {
		fmt.Fprintf(os.Stderr, "  %-18s frames=%-4d peak=%.4f dBFS  rms=%.4f dBFS\n",
			w.name, w.win.frames, w.win.peak, w.win.rms)
	}

	// THE METER MUST NOT FREEZE. This is the assertion a reader will think is
	// the weakest and it is the one that separates this mechanism from a valve:
	// a stopped stream and a silent one look the same on a meter for the first
	// second, and mean opposite things. The count is deliberately generous —
	// what is being excluded is ZERO, not a slow frame.
	if muted.frames < 5 {
		t.Errorf("the programme meter posted %d frames while muted (want the same steady rate as "+
			"unmuted, %d). A meter that stops is a meter that cannot distinguish a mute from a "+
			"dead capture", muted.frames, unmutedBefore.frames)
	}
	// AND IT MUST READ SILENCE. level's digital-silence floor is around
	// -700 dBFS; anything near the unmuted reading means audio is still going
	// out. The threshold is loose on purpose: the claim is "silent", not "some
	// particular number".
	// SILENCE IS -100 HERE, NOT -inf AND NOT -700. levels.go clamps: the
	// element reports about -700 dBFS for digital silence and clampLevelDB
	// floors it, because -inf does not survive JSON and a meter has a floor
	// anyway. So the assertion is against the floor.
	if muted.peak > -99 {
		t.Errorf("the programme meter read peak %.4f dBFS while muted. The mute is upstream of "+
			"this meter, so a reading anywhere near the unmuted level means commentary is still "+
			"reaching the encoder", muted.peak)
	}
	// The unmuted windows are reported rather than asserted on: this rig's room
	// tone is whatever it is, and a quiet room is not a code failure. What IS
	// asserted is that the meter came back — an unmute that left the element
	// silent would be worse than one that never engaged.
	if unmutedAfter.frames < 5 {
		t.Errorf("the programme meter posted %d frames after unmuting", unmutedAfter.frames)
	}

	busMu.Lock()
	n := len(busErrs)
	busMu.Unlock()
	if n > 0 {
		t.Errorf("%d bus error(s) across the mute cycle; muting must disturb nothing", n)
	}
}

// muteWindow is what the programme meter held over one window.
type muteWindow struct {
	peak, rms float64
	frames    int
}

// holdWindow resets the meter, waits, and reports what it caught.
func holdWindow(m *meterWatch, d time.Duration) muteWindow {
	m.reset()
	time.Sleep(d)
	peak, rms, frames := m.report()
	w := muteWindow{frames: frames, peak: -999, rms: -999}
	for i := range peak {
		if peak[i] > w.peak {
			w.peak = peak[i]
		}
		if valueAt(rms, i) > w.rms {
			w.rms = rms[i]
		}
	}
	return w
}

// assertStillPlaying fails if the pipeline left PLAYING or has a state pending.
//
// It reaches through the concrete type deliberately: the Pipeline interface does
// not expose the GStreamer state and must not, but the claim being checked here
// is a GStreamer one.
func assertStillPlaying(t *testing.T, pipe CapturePipeline, when string) {
	t.Helper()
	p, ok := pipe.(*cgoCapture)
	if !ok {
		t.Fatalf("%s: not a cgoCapture", when)
	}
	p.mu.Lock()
	pl := p.pipeline
	p.mu.Unlock()
	if pl == nil {
		t.Fatalf("%s: the pipeline is gone", when)
	}
	state, pending, ret := pl.GetState(0)
	if state != gogst.StatePlaying || pending != gogst.StateVoidPending {
		t.Errorf("%s: state=%v pending=%v (ret %v), want PLAYING with nothing pending. The mute "+
			"is a property write and must not move the pipeline", when, state, pending, ret)
	}
}

// TestLiveCardReleaseAndReacquire is the measurement that decides whether a
// PREVIEW-ONLY pipeline can hand the card to Start.
//
// The card is exclusive: two decklinkvideosrc in one process fail 3/3 and two
// processes fail 3/3, so there is no atomic handover. A preview that holds the
// card before Start must therefore RELEASE it and let the contribution pipeline
// TAKE it — and the whole risk of that design is concentrated in one question:
// after set_state(NULL), is the card immediately available again in the same
// process, every time?
//
// A failed reacquire is not a missing preview. It is NO FEED, twenty minutes
// before kick-off, caused by a convenience. So the bar is not "usually" and this
// test reports its result as a BOUND rather than as a pass: N cycles without a
// failure establishes that the failure rate is below roughly 1/N, and nothing
// more. Read the numbers before trusting the design, not the exit status.
func TestLiveCardReleaseAndReacquire(t *testing.T) {
	liveDeckLinkInit(t)

	slate, err := filepath.Abs(env("WSLCOMMS_LIVE_SLATE", defaultSlatePath))
	if err != nil {
		t.Fatalf("resolving the slate: %v", err)
	}
	card := env("WSLCOMMS_LIVE_CARD", defaultLiveCard)

	type cycle struct {
		n       int
		start   time.Duration
		stop    time.Duration
		frames  int
		startEr error
	}
	var cycles []cycle
	failures := 0

	for i := 1; i <= coughMuteCycles; i++ {
		meter := &meterWatch{}
		// BOTH legs on the card — the FUSED shape — which is the configuration a
		// preview would hold: a picture to show and the commentary clocked off it.
		// It is one pipeline and not two by construction now, because the card is
		// exclusive and decklinkaudiosrc cannot preroll without a decklinkvideosrc
		// beside it; PlanCapture is where that rule lives.
		pipe, err := NewCapture(CaptureOpts{
			Legs:           CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard},
			SlatePath:      slate,
			VideoCaptureID: card,
			AudioCaptureID: card,
			ConformTo:      FallbackConformTarget(),
			OnLevels:       func(l Levels) { meter.record(l) },
		})
		if err != nil {
			t.Fatalf("cycle %d: NewCapture: %v", i, err)
		}
		go func() {
			for range pipe.Faults() {
			}
		}()
		go func() {
			for range pipe.Warnings() {
			}
		}()

		t0 := time.Now()
		startErr := pipe.Start()
		startedIn := time.Since(t0)

		c := cycle{n: i, start: startedIn, startEr: startErr}
		if startErr != nil {
			failures++
			t.Errorf("cycle %d: Start: %v (after %v). THE CARD DID NOT COME BACK — this is the "+
				"failure that makes a preview handover unshippable", i, startErr, startedIn)
		} else {
			// Long enough for the card to have produced something, so that
			// "PLAYING" is not the only evidence it came back.
			time.Sleep(1200 * time.Millisecond)
			_, _, c.frames = meter.report()
			if c.frames == 0 {
				failures++
				t.Errorf("cycle %d: reached PLAYING and produced NO level frames in 1.2 s. A "+
					"reacquire that starts and carries nothing is worse than one that fails", i)
			}
		}

		t0 = time.Now()
		if err := pipe.Stop(); err != nil {
			t.Errorf("cycle %d: Stop: %v", i, err)
		}
		c.stop = time.Since(t0)
		cycles = append(cycles, c)
	}

	fmt.Fprintf(os.Stderr, "\n>>>> CARD RELEASE AND REACQUIRE, %d CYCLES, ONE PROCESS\n", coughMuteCycles)
	for _, c := range cycles {
		status := "ok"
		if c.startEr != nil {
			status = "START FAILED: " + c.startEr.Error()
		}
		fmt.Fprintf(os.Stderr, "  cycle %2d  start %-12v stop %-12v frames %-4d %s\n",
			c.n, c.start.Round(time.Millisecond), c.stop.Round(time.Millisecond), c.frames, status)
	}
	fmt.Fprintf(os.Stderr, "  failures: %d of %d\n", failures, coughMuteCycles)
	fmt.Fprintf(os.Stderr, "  READ THIS AS A BOUND: %d clean cycles establishes a failure rate "+
		"below roughly 1/%d and nothing more.\n", coughMuteCycles-failures, coughMuteCycles)
}

// TestLivePreviewBeforeStartNeedsNoNewPipeline is the measurement that answers
// "can we see and hear the card before going live", and the answer it gives is
// that the state being asked for ALREADY EXISTS.
//
// # What the question looked like, and why the obvious designs are all wrong
//
// The card is exclusive: two decklinkvideosrc in one process fail 3/3 and two
// processes fail 3/3. So a preview pipeline that Start must take the card away
// from is a handover with no atomic form, and a failed reacquire is no feed at
// all — far worse than no preview. The alternative, attaching the encoder and
// mux branch to a running graph at Start, is the live add/remove that was
// measured to take the on-air leg from 50 fps to 0 PERMANENTLY with the
// pipeline still reporting PLAYING.
//
// # And neither is needed
//
// THE ARGUMENT THIS TEST WAS WRITTEN FROM HAS BEEN OVERTAKEN BY THE SEAM, and it
// is answered here rather than deleted, because the conclusion survived and only
// the mechanism changed.
//
// It used to be that Start brought the whole capture chain to PLAYING WITH NO
// SINK INSTALLED, and that "see and hear before starting" was therefore the
// application calling gst.Start and not calling ReplaceSink until the operator
// said go. That was true, and it had one flaw the operator paid for: pressing
// STOP took it all away again, and nothing could be seen or heard between
// matches or before the first one.
//
// Capture is now a pipeline of its own, built when the application opens and
// released when it quits, ending in proxysinks. The preview, the meters, the
// routing width, the signal lamp and the cough mute are properties of THAT
// pipeline, so they exist from launch, survive STOP, and do not depend on a
// session having been started and not connected. The card handover this test
// exists to refute is not merely unnecessary now, it is unbuildable: there is
// only ever one pipeline holding the card and it never lets go.
//
// This test measures that state directly: a capture pipeline, NO SEND PIPELINE
// ANYWHERE IN THE PROCESS, and the legs running. It asserts what internal/gst can
// assert; that glimagesink draws into the surface it is handed is already true
// today, since that is how the preview works while sending, and nothing about it
// changes.
func TestLivePreviewBeforeStartNeedsNoNewPipeline(t *testing.T) {
	liveDeckLinkInit(t)

	slate, err := filepath.Abs(env("WSLCOMMS_LIVE_SLATE", defaultSlatePath))
	if err != nil {
		t.Fatalf("resolving the slate: %v", err)
	}
	card := env("WSLCOMMS_LIVE_CARD", defaultLiveCard)

	meter := &meterWatch{}

	// BOTH legs on the card: the configuration an operator wanting a preview is
	// in, and the one where the preview branch exists at all (the slate leg has
	// no tee, so previewBranchFor refuses it).
	pipe, err := NewCapture(CaptureOpts{
		Legs:           CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard},
		SlatePath:      slate,
		VideoCaptureID: card,
		AudioCaptureID: card,
		ConformTo:      FallbackConformTarget(),
		OnLevels:       func(l Levels) { meter.record(l) },
	})
	if err != nil {
		t.Fatalf("NewCapture: %v", err)
	}
	defer func() {
		if err := pipe.Stop(); err != nil {
			t.Errorf("Stop: %v — the card may still be held", err)
		}
	}()
	go func() {
		for range pipe.Faults() {
		}
	}()
	go func() {
		for range pipe.Warnings() {
		}
	}()

	if err := pipe.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// NOTHING ELSE EXISTS HERE. No send pipeline, no encoder, no muxer, no sink:
	// this is the pre-live state exactly as the application leaves it from launch
	// to the moment the operator presses START.
	time.Sleep(2500 * time.Millisecond)

	_, _, frames := meter.report()
	width := pipe.InputChannels()

	fmt.Fprintf(os.Stderr, "\n>>>> THE PIPELINE WITH NO SINK INSTALLED\n")
	fmt.Fprintf(os.Stderr, "  programme meter frames in 2.5 s   %d\n", frames)
	fmt.Fprintf(os.Stderr, "  capture channels negotiated       %d\n", width)
	fmt.Fprintf(os.Stderr, "  cough mute settable               ")

	// The mute is a live control in this state too, which matters: an operator
	// checking their microphone before kick-off must be able to cut it.
	muteErr := pipe.SetCommentaryMute(true)
	fmt.Fprintf(os.Stderr, "%v (muted=%t)\n", muteErr == nil, pipe.CommentaryMuted())
	if muteErr != nil {
		t.Errorf("SetCommentaryMute on a pipeline with no sink: %v", muteErr)
	}
	if err := pipe.SetCommentaryMute(false); err != nil {
		t.Errorf("SetCommentaryMute(false): %v", err)
	}

	if frames == 0 {
		t.Fatal("the capture chain produced NOTHING with no sink installed. If that were true, " +
			"a preview before Start really would need a pipeline of its own — and the card being " +
			"exclusive means there is no safe way to build one")
	}
	if width == 0 {
		t.Error("the capture pad has negotiated no channels with no sink installed")
	}
	assertStillPlaying(t, pipe, "with no sink installed")
}
