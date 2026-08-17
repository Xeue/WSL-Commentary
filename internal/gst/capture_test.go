//go:build !cgo || gststub

// capture_test.go is the Gate A guard over the decisions the capture layer makes
// before any element exists: the fusion rule, the single-consumer claim, the
// device stamp and the muxer watchdog's arithmetic.
//
// None of them needs a card, a device or cgo, and every one of them fails
// silently in production — a second decklink element in one process, a stolen
// proxysink, a routing grid stamped with the wrong device, a feed that has gone
// quiet behind a green lamp.
package gst

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The fusion rule
// ---------------------------------------------------------------------------

// TestPlanCaptureIsTheFusionRule is PLAN.md section 1's table, as an assertion.
//
// The row that matters is the last one. decklinkaudiosrc cannot preroll without a
// decklinkvideosrc in the SAME pipeline, and the card is exclusive, so both legs
// on the card MUST share a pipeline. Every other row must NOT fuse, because a
// fused pipeline blanks the picture when the microphone changes and "I changed my
// mic and my camera died" is a worse failure than any amount of extra code.
func TestPlanCaptureIsTheFusionRule(t *testing.T) {
	const card = "2747401380"

	for _, tc := range []struct {
		name string
		src  CaptureSources
		want []CaptureLegs
	}{
		{
			name: "slate + CoreAudio: two, independent",
			src:  CaptureSources{AudioDeviceID: "BF568F24"},
			want: []CaptureLegs{
				{Picture: PictureSlate},
				{Commentary: CommentaryNative},
			},
		},
		{
			name: "slate + card: two, and the commentary carries the clock companion",
			src:  CaptureSources{AudioCaptureID: card},
			want: []CaptureLegs{
				{Picture: PictureSlate},
				{Commentary: CommentaryCard},
			},
		},
		{
			name: "card + CoreAudio: two, independent",
			src:  CaptureSources{VideoCaptureID: card, AudioDeviceID: "BF568F24"},
			want: []CaptureLegs{
				{Picture: PictureCard},
				{Commentary: CommentaryNative},
			},
		},
		{
			name: "card + card: ONE FUSED PIPELINE",
			src:  CaptureSources{VideoCaptureID: card, AudioCaptureID: card},
			want: []CaptureLegs{
				{Picture: PictureCard, Commentary: CommentaryCard},
			},
		},
		{
			name: "the preview is honoured only where there is a tee",
			src:  CaptureSources{VideoCaptureID: card, AudioDeviceID: "BF568F24", Preview: true},
			want: []CaptureLegs{
				{Picture: PictureCard, Preview: true},
				{Commentary: CommentaryNative},
			},
		},
		{
			name: "a preview asked for on a slate seat is dropped, not built",
			src:  CaptureSources{AudioDeviceID: "BF568F24", Preview: true},
			want: []CaptureLegs{
				{Picture: PictureSlate},
				{Commentary: CommentaryNative},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := PlanCapture(tc.src)
			if len(got) != len(tc.want) {
				t.Fatalf("PlanCapture returned %d leg-sets (%v), want %d (%v)",
					len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("leg-set %d is %+v (%s), want %+v (%s)",
						i, got[i], got[i], tc.want[i], tc.want[i])
				}
			}
		})
	}
}

// TestAtMostOnePipelineEverContainsDeckLinkElements is the constraint the fusion
// rule exists to satisfy, checked over every configuration rather than over the
// four rows above.
//
// The card is EXCLUSIVE: two decklink sources in one process fail 3/3 and two
// processes fail 3/3. A plan that put decklink elements in two pipelines would
// not degrade — the second pipeline would meet "Internal data stream error /
// not-negotiated (-4)" in about 100 microseconds, naming neither the device nor
// the cause, and the operator would go looking at cables.
func TestAtMostOnePipelineEverContainsDeckLinkElements(t *testing.T) {
	const card = "2747401380"
	for _, src := range []CaptureSources{
		{AudioDeviceID: "x"},
		{AudioCaptureID: card},
		{VideoCaptureID: card, AudioDeviceID: "x"},
		{VideoCaptureID: card, AudioCaptureID: card},
		{VideoCaptureID: card, AudioCaptureID: card, Preview: true},
	} {
		withCard := 0
		for _, legs := range PlanCapture(src) {
			if legs.HasCard() {
				withCard++
			}
		}
		if withCard > 1 {
			t.Errorf("PlanCapture(%+v) puts decklink elements in %d pipelines. The card is "+
				"exclusive — two sources in one process fail 3/3 — so this is not a wasteful "+
				"plan, it is a seat that will not start", src, withCard)
		}
	}
}

// TestTheClockCompanionIsBuiltExactlyWhenItIsNeeded pins the AND of both
// conditions.
//
// Getting it wrong in one direction is a wasted element; getting it wrong in the
// other is a seat with a camera and a card microphone that will not start at all,
// because the second decklinkvideosrc cannot have the exclusive card.
func TestTheClockCompanionIsBuiltExactlyWhenItIsNeeded(t *testing.T) {
	for _, tc := range []struct {
		legs CaptureLegs
		want bool
	}{
		{CaptureLegs{Picture: PictureSlate}, false},
		{CaptureLegs{Picture: PictureCard}, false},
		{CaptureLegs{Commentary: CommentaryNative}, false},
		{CaptureLegs{Commentary: CommentaryCard}, true},
		{CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard}, false},
		{CaptureLegs{Picture: PictureSlate, Commentary: CommentaryCard}, true},
	} {
		if got := tc.legs.NeedsClockCompanion(); got != tc.want {
			t.Errorf("%s.NeedsClockCompanion() = %t, want %t", tc.legs, got, tc.want)
		}
	}
}

// TestAudioClockedByVideoIsKnownFromTheLegSet is what deletes captureLegsFor's
// parent-bin walk.
//
// The old classifier had to traverse the graph on a streaming thread to answer
// this, because a single pipeline could not say at build time which shape it was.
// A capture pipeline knows its leg-set before gst_parse_launch is called, so the
// answer is a field read — and the bin traversal that used to sit in front of the
// gate store on the on-air path is gone.
func TestAudioClockedByVideoIsKnownFromTheLegSet(t *testing.T) {
	for _, tc := range []struct {
		legs CaptureLegs
		want bool
	}{
		// A card COMMENTARY is clocked by a decklinkvideosrc either way: by
		// vcapsrc when the picture is the card, and by vcapclock when it is not.
		{CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard}, true},
		{CaptureLegs{Commentary: CommentaryCard}, true},
		// A card PICTURE with the commentary somewhere else is not: losing the
		// card there is a frozen picture and nothing more.
		{CaptureLegs{Picture: PictureCard, Commentary: CommentaryNative}, false},
		{CaptureLegs{Picture: PictureCard}, false},
		{CaptureLegs{Picture: PictureSlate, Commentary: CommentaryNative}, false},
	} {
		if got := tc.legs.AudioClockedByVideo(); got != tc.want {
			t.Errorf("%s.AudioClockedByVideo() = %t, want %t", tc.legs, got, tc.want)
		}
	}
}

// TestAnEmptyOrPreviewOnlyLegSetIsRefused checks the two shapes nothing can
// build, refused before anything is parsed.
func TestAnEmptyOrPreviewOnlyLegSetIsRefused(t *testing.T) {
	if err := (CaptureLegs{}).Valid(); err == nil {
		t.Error("a leg-set with neither leg was accepted; there is nothing to build")
	}
	if err := (CaptureLegs{Picture: PictureSlate, Preview: true}).Valid(); err == nil {
		t.Error("a preview on a slate picture leg was accepted. The slate builds no tee, so the " +
			"branch is a gst_parse_launch failure of the WHOLE pipeline rather than a missing " +
			"preview")
	}
	if err := (CaptureLegs{Picture: PictureCard, Preview: true}).Valid(); err != nil {
		t.Errorf("a preview on a card picture leg was refused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The device stamp
// ---------------------------------------------------------------------------

// TestTheDeviceKeyStampsTheWidthWithItsDevice is PLAN.md section 4.2 step 5's
// requirement, and the stamp is not optional.
//
// Without it there is a window between selecting a Focusrite and the capture
// renegotiating in which the routing grid still holds the previous device's
// sixteen, and a crosspoint pressed in that window writes a 2x16 matrix onto a
// two-channel pad — the measured "streaming stopped, reason error (-5)", which
// reads as a broken device rather than as a bad matrix.
func TestTheDeviceKeyStampsTheWidthWithItsDevice(t *testing.T) {
	for _, tc := range []struct {
		opts CaptureOpts
		want string
	}{
		{CaptureOpts{Legs: CaptureLegs{Commentary: CommentaryCard}, AudioCaptureID: "2747401380"},
			"decklink:2747401380"},
		{CaptureOpts{Legs: CaptureLegs{Commentary: CommentaryNative}, AudioDeviceID: "BF568F24"},
			"native:BF568F24"},
		// A pipeline with no commentary leg has no key, which is what stops a
		// PICTURE pipeline publishing a width for a device it does not own.
		{CaptureOpts{Legs: CaptureLegs{Picture: PictureCard}}, ""},
	} {
		if got := tc.opts.DeviceKey(); got != tc.want {
			t.Errorf("DeviceKey() = %q, want %q", got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// The single-consumer claim
// ---------------------------------------------------------------------------

// seamTestCapture is a CapturePipeline that does nothing but hold claims and
// count armings. It is not the stub twin: it exists so that NewSend can be tested
// in BOTH builds against a set whose behaviour the test controls.
type seamTestCapture struct {
	claims        seamClaims
	armings       int
	armErr        error
	health        error
	pictureHealth error
}

func newSeamTestCapture(legs CaptureLegs) *seamTestCapture {
	return &seamTestCapture{claims: newSeamClaims(legs)}
}

func (c *seamTestCapture) Legs() CaptureLegs { return CaptureLegs{} }
func (c *seamTestCapture) Start() error      { return nil }
func (c *seamTestCapture) ArmForSend() error {
	c.armings++
	return c.armErr
}
func (c *seamTestCapture) ProxySinks() []string           { return c.claims.names() }
func (c *seamTestCapture) ClaimForSend() error            { return c.claims.claimAll() }
func (c *seamTestCapture) ReleaseFromSend()               { c.claims.releaseAll() }
func (c *seamTestCapture) Health() error                  { return c.health }
func (c *seamTestCapture) PictureHealth() error           { return c.pictureHealth }
func (c *seamTestCapture) InputChannels() int             { return 0 }
func (c *seamTestCapture) SetChannelMap(ChannelMap) error { return nil }
func (c *seamTestCapture) SetCommentaryMute(bool) error   { return nil }
func (c *seamTestCapture) CommentaryMuted() bool          { return false }
func (c *seamTestCapture) Faults() <-chan error           { return nil }
func (c *seamTestCapture) Warnings() <-chan string        { return nil }
func (c *seamTestCapture) Stop() error                    { return nil }

// TestASecondSendIsRefusedWhileTheFirstHoldsTheSeam is the single-consumer rule.
//
// A second proxysrc attaching to a live proxysink does not fail: it SILENTLY
// STEALS THE STREAM AND KILLS THE FIRST — measured, consumer A stopped dead at
// 5.994 s the instant consumer B attached at 6.007 s, with nothing on either bus.
// There is no refusal inside the element, so this is the refusal.
func TestASecondSendIsRefusedWhileTheFirstHoldsTheSeam(t *testing.T) {
	picture := newSeamTestCapture(CaptureLegs{Picture: PictureCard})
	commentary := newSeamTestCapture(CaptureLegs{Commentary: CommentaryNative})
	set := CaptureSet{Picture: picture, Commentary: commentary}

	first, err := NewSend(set)
	if err != nil {
		t.Fatalf("the first NewSend failed: %v", err)
	}
	if picture.armings != 1 || commentary.armings != 1 {
		t.Errorf("NewSend armed %d picture and %d commentary seams, want 1 each. Arming at START "+
			"is what stops the second session carrying zero bytes", picture.armings,
			commentary.armings)
	}

	second, err := NewSend(set)
	if err == nil {
		second.Stop()
		t.Fatal("a second NewSend succeeded while the first held the seam. Building it does not " +
			"fail — it steals the stream and takes the first silently off air")
	}
	if !errors.Is(err, ErrSeamBusy) {
		t.Errorf("the refusal does not wrap ErrSeamBusy: %v", err)
	}
	if !strings.Contains(err.Error(), nameVideoProxySink) {
		t.Errorf("the refusal does not name the proxysink that is taken: %v", err)
	}

	// STOP RELEASES BOTH, and only then can another session have them.
	first.Stop()
	third, err := NewSend(set)
	if err != nil {
		t.Fatalf("NewSend failed after the holder stopped: %v", err)
	}
	if picture.armings != 2 || commentary.armings != 2 {
		t.Errorf("the second session armed %d picture and %d commentary seams, want 2 each. "+
			"EVERY send session after the first must arm, or it carries zero bytes",
			picture.armings, commentary.armings)
	}
	third.Stop()
}

// TestAFusedCaptureSetIsClaimedAndArmedExactlyOnce is the trap CaptureSet.Pipelines
// exists for.
//
// On a fused seat Picture and Commentary are THE SAME OBJECT. Looping over the
// two fields would claim it twice — refusing the seat its own consumer with
// ErrSeamBusy — and arm it twice, running the READY cycle over the same two
// proxysinks a second time for no reason.
func TestAFusedCaptureSetIsClaimedAndArmedExactlyOnce(t *testing.T) {
	fused := newSeamTestCapture(CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard})
	set := CaptureSet{Picture: fused, Commentary: fused}

	if got := len(set.Pipelines()); got != 1 {
		t.Fatalf("a fused CaptureSet reports %d pipelines, want 1", got)
	}
	seam, err := NewSend(set)
	if err != nil {
		t.Fatalf("NewSend on a fused set failed: %v", err)
	}
	if fused.armings != 1 {
		t.Errorf("the fused pipeline was armed %d times, want 1", fused.armings)
	}
	seam.Stop()
}

// TestAPartialClaimReleasesWhatItTook is the all-or-nothing rule.
//
// A partial claim would leave the video seam taken by a send pipeline that never
// got built, and the NEXT start would be refused for a consumer that does not
// exist — a seat that cannot go on air, with the fix being to restart the
// application.
func TestAPartialClaimReleasesWhatItTook(t *testing.T) {
	picture := newSeamTestCapture(CaptureLegs{Picture: PictureCard})
	commentary := newSeamTestCapture(CaptureLegs{Commentary: CommentaryNative})

	// Somebody else already holds the commentary half.
	if err := commentary.ClaimForSend(); err != nil {
		t.Fatalf("the setup claim failed: %v", err)
	}

	if _, err := NewSend(CaptureSet{Picture: picture, Commentary: commentary}); err == nil {
		t.Fatal("NewSend succeeded with the commentary seam already taken")
	}
	if picture.claims[0].taken() {
		t.Error("the picture seam is still claimed after a failed NewSend. The next START would " +
			"be refused for a consumer that was never built")
	}
}

// TestAFailedArmingReleasesTheClaim is the same rule for the second half of
// NewSend. An arming that fails is a session that will carry zero bytes, so it
// must not also leave the seam locked.
func TestAFailedArmingReleasesTheClaim(t *testing.T) {
	c := newSeamTestCapture(CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard})
	c.armErr = errors.New("the streaming thread is not idle")

	if _, err := NewSend(CaptureSet{Picture: c, Commentary: c}); err == nil {
		t.Fatal("NewSend succeeded with an arming that failed")
	}
	for _, claim := range c.claims {
		if claim.taken() {
			t.Errorf("%s is still claimed after a failed arming", claim.name)
		}
	}
}

// TestSendSeamStopIsIdempotent — the path that calls it is a teardown, and a
// teardown that can fail twice must be callable twice.
func TestSendSeamStopIsIdempotent(t *testing.T) {
	c := newSeamTestCapture(CaptureLegs{Commentary: CommentaryNative})
	seam, err := NewSend(CaptureSet{Commentary: c})
	if err != nil {
		t.Fatalf("NewSend failed: %v", err)
	}
	seam.Stop()
	seam.Stop()
	if c.claims[0].taken() {
		t.Error("the claim survived Stop")
	}
}

// TestTheClaimIsSafeUnderConcurrentSends runs the refusal under -race, because
// the two callers it has to separate are two goroutines: the session forwarder
// and whatever the operator pressed.
func TestTheClaimIsSafeUnderConcurrentSends(t *testing.T) {
	c := newSeamTestCapture(CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard})
	set := CaptureSet{Picture: c, Commentary: c}

	const attempts = 16
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		won  int
		held []*SendSeam
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seam, err := NewSend(set)
			if err != nil {
				return
			}
			mu.Lock()
			won++
			held = append(held, seam)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if won != 1 {
		t.Errorf("%d of %d concurrent NewSend calls took the seam, want exactly 1. More than one "+
			"consumer on a proxysink is not a race that degrades — the newcomer takes the stream "+
			"and the incumbent goes silently off air", won, attempts)
	}
	for _, s := range held {
		s.Stop()
	}
}

// TestNewSendRefusesAnEmptyCaptureSet — a send pipeline is minted only by
// capture, so "a send pipeline with no device behind it" is unconstructible
// rather than merely unlikely.
func TestNewSendRefusesAnEmptyCaptureSet(t *testing.T) {
	if _, err := NewSend(CaptureSet{}); err == nil {
		t.Fatal("NewSend accepted an empty capture set")
	}
}

// TestACaptureWillNotGoToNullUnderItsOwnConsumer is the teardown ORDER, asserted
// on the twin Gate A can run.
//
// A capture pipeline taken to NULL under a bound proxysrc is the measured
// completely-silent failure: 0 buffers, no EOS, no ERROR and no WARNING on either
// bus, the send pipeline still PLAYING and SRT still connected. Nothing crosses
// the seam to report it, because proxysink returns GST_FLOW_OK unconditionally.
// The rule was previously written only in prose and enforced by four call sites —
// RestartCapture, ApplyPreset, a conform change and shutdown — remembering to stop
// the sender first.
func TestACaptureWillNotGoToNullUnderItsOwnConsumer(t *testing.T) {
	p, err := NewCapture(CaptureOpts{
		Legs:          CaptureLegs{Commentary: CommentaryNative},
		AudioDeviceID: "BF568F24",
	})
	if err != nil {
		t.Fatalf("NewCapture failed: %v", err)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	seam, err := NewSend(CaptureSet{Commentary: p})
	if err != nil {
		t.Fatalf("NewSend failed: %v", err)
	}
	stopErr := p.Stop()
	if stopErr == nil {
		t.Fatal("the capture stopped with a send pipeline still attached to its proxysink")
	}
	if !errors.Is(stopErr, ErrSeamBusy) {
		t.Errorf("the refusal does not wrap ErrSeamBusy: %v", stopErr)
	}

	// The way out is the way round the caller was supposed to go.
	seam.Stop()
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop failed after the send seam was released: %v", err)
	}
}

// TestNewSendRefusesACaptureThatHasAlreadyDied is the guard on a green lamp over
// silence.
//
// A latched capture fault clears neither started nor stopped, so the object goes
// on answering: the claim succeeds, and on a dead device the arming's IDLE probe
// fires IMMEDIATELY — the pad is idle because it is dead — and reports success.
// PLAN.md step 6's 2 s liveness gate is the backstop for a MISSED arming, not for
// one that succeeded over a corpse.
func TestNewSendRefusesACaptureThatHasAlreadyDied(t *testing.T) {
	p, err := NewCapture(CaptureOpts{
		Legs:          CaptureLegs{Commentary: CommentaryNative},
		AudioDeviceID: "BF568F24",
	})
	if err != nil {
		t.Fatalf("NewCapture failed: %v", err)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if err := p.Health(); err != nil {
		t.Fatalf("a freshly started capture reports Health() = %v", err)
	}

	failure := errors.New("asrc: Could not read from resource (the device went away)")
	p.(*StubCapture).Fail(failure)

	if err := p.Health(); err == nil {
		t.Fatal("Health() is nil on a capture whose fault channel has already carried its death")
	}
	if _, err := NewSend(CaptureSet{Commentary: p}); err == nil {
		t.Fatal("NewSend built a session against a capture pipeline that has already died. The " +
			"seam would report itself armed over a producer that will never push")
	}
	// And it left nothing claimed, so the capture can still be rebuilt.
	if p.(*StubCapture).claims[0].taken() {
		t.Error("the failed NewSend left the seam claimed, so a rebuild could not have it")
	}
}

// ---------------------------------------------------------------------------
// The muxer watchdog
// ---------------------------------------------------------------------------

// TestTheLivenessGateNamesThePadsThatSawNothing is the guard that makes a missed
// arming loud instead of green.
//
// Naming the silent pads rather than saying "no media" is the whole value: vq
// alone is the video half of the seam, aq alone is the audio half, and all three
// is a seam that was never armed at all.
func TestTheLivenessGateNamesThePadsThatSawNothing(t *testing.T) {
	healthy := []liveWatchSample{
		{Pad: nameMuxVideoQueue, Buffers: 199},
		{Pad: nameMuxAudioQueue, Buffers: 187},
		{Pad: nameMuxOutput, Buffers: 855},
	}
	if err := liveWatchStartVerdict(healthy, liveWatchStartGrace); err != nil {
		t.Errorf("a healthy start was refused: %v", err)
	}

	// THE MEASURED HALF-BLIND CASE. A dead video feed does not stop the audio
	// leg: it pushes at full rate into a muxer that emits nothing. Watching only
	// the plan's two chosen pads would read aq as green here.
	deadVideo := []liveWatchSample{
		{Pad: nameMuxVideoQueue, Buffers: 0},
		{Pad: nameMuxAudioQueue, Buffers: 187},
		{Pad: nameMuxOutput, Buffers: 0},
	}
	err := liveWatchStartVerdict(deadVideo, liveWatchStartGrace)
	if err == nil {
		t.Fatal("a send pipeline with a dead video leg and a full-rate audio leg was accepted")
	}
	if !errors.Is(err, ErrPipelineFatal) {
		t.Errorf("the refusal does not wrap ErrPipelineFatal: %v", err)
	}
	for _, want := range []string{nameMuxVideoQueue + ":src", nameMuxOutput + ":src"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), nameMuxAudioQueue+":src") {
		t.Errorf("the refusal names %s:src, which was at full rate: %v", nameMuxAudioQueue, err)
	}
}

// TestTheWatchdogWatchesTheMuxerOutputAsWellAsItsInputs is correction 4 stated as
// a test rather than as a comment, because it is the correction most likely to be
// undone by somebody tidying "a redundant third pad" away.
func TestTheWatchdogWatchesTheMuxerOutputAsWellAsItsInputs(t *testing.T) {
	// The exact failing reading from the rig: only mux:src distinguishes it from
	// a healthy feed once the audio leg is discounted.
	onlyMuxSilent := []liveWatchSample{
		{Pad: nameMuxVideoQueue, Buffers: 199},
		{Pad: nameMuxAudioQueue, Buffers: 187},
		{Pad: nameMuxOutput, Buffers: 0},
	}
	if err := liveWatchStartVerdict(onlyMuxSilent, liveWatchStartGrace); err == nil {
		t.Fatal("a send pipeline whose MUXER emitted nothing was accepted because both of its " +
			"inputs were flowing. mux:src is the one pad that read zero in every failing case")
	}
	if err := liveWatchStartVerdict(nil, liveWatchStartGrace); err == nil {
		t.Fatal("a liveness gate with no pads at all reported success")
	}
}

// TestTheSilenceVerdictMeasuresAnUnstartedPadFromPLAYING covers the case a
// running-feed watchdog gets wrong if it only looks at last-seen: a pad that has
// NEVER produced has no last-seen time, and treating a zero time as "just now"
// would make a feed that never started invisible to the poller as well as to the
// gate.
func TestTheSilenceVerdictMeasuresAnUnstartedPadFromPLAYING(t *testing.T) {
	playing := time.Now().Add(-5 * time.Second)
	now := time.Now()

	never := []liveWatchSample{{Pad: nameMuxOutput, Buffers: 0}}
	if err := liveWatchSilenceVerdict(never, playing, now, liveWatchSilence); err == nil {
		t.Error("a pad that has never produced, five seconds after PLAYING, was reported healthy")
	}

	flowing := []liveWatchSample{
		{Pad: nameMuxVideoQueue, Buffers: 1000, Last: now.Add(-100 * time.Millisecond)},
		{Pad: nameMuxAudioQueue, Buffers: 400, Last: now.Add(-50 * time.Millisecond)},
		{Pad: nameMuxOutput, Buffers: 5000, Last: now.Add(-10 * time.Millisecond)},
	}
	if err := liveWatchSilenceVerdict(flowing, playing, now, liveWatchSilence); err != nil {
		t.Errorf("a flowing feed was indicted: %v", err)
	}

	stalled := []liveWatchSample{
		{Pad: nameMuxVideoQueue, Buffers: 1000, Last: now.Add(-100 * time.Millisecond)},
		{Pad: nameMuxOutput, Buffers: 5000, Last: now.Add(-3 * time.Second)},
	}
	err := liveWatchSilenceVerdict(stalled, playing, now, liveWatchSilence)
	if err == nil {
		t.Fatal("a feed whose muxer stopped three seconds ago was reported healthy")
	}
	if !errors.Is(err, ErrPipelineFatal) {
		t.Errorf("the silence verdict does not wrap ErrPipelineFatal: %v", err)
	}
	if !strings.Contains(err.Error(), nameMuxOutput+":src") {
		t.Errorf("the silence verdict does not name the quiet pad: %v", err)
	}
}

// TestTheWatchdogBudgetsAreTheDeclaredOnes keeps the two numbers that decide
// whether a match goes off air from drifting under a comment that still quotes
// the old ones.
func TestTheWatchdogBudgetsAreTheDeclaredOnes(t *testing.T) {
	if liveWatchSilence != 2*time.Second {
		t.Errorf("liveWatchSilence is %v, want 2s", liveWatchSilence)
	}
	if liveWatchStartGrace != 2*time.Second {
		t.Errorf("liveWatchStartGrace is %v, want 2s", liveWatchStartGrace)
	}
	if liveWatchPollInterval != 250*time.Millisecond {
		t.Errorf("liveWatchPollInterval is %v, want 250ms", liveWatchPollInterval)
	}
	// Eight reads inside the budget. One or two would diagnose a healthy match
	// off a single unlucky sample.
	if reads := liveWatchSilence / liveWatchPollInterval; reads < 4 {
		t.Errorf("the poller gets only %d reads inside the silence budget", reads)
	}
}

// ---------------------------------------------------------------------------
// The classifier
// ---------------------------------------------------------------------------

// TestTheSeamTailsAreClassifiedExplicitly is the guard on the entries added to
// capturefault.go for vprox* and aprox*.
//
// Before them both fell through to the nameless FATAL default, which says "the
// capture or mux chain has failed" about a leg this package can name — and, worse,
// took the commentary off air for a picture-side queue.
func TestTheSeamTailsAreClassifiedExplicitly(t *testing.T) {
	fused := captureLegs{AudioClockedByVideo: true}
	split := captureLegs{}

	for _, tc := range []struct {
		source string
		legs   captureLegs
		want   busErrorClass
		why    string
	}{
		{nameAudioProxyQueue, split, classAudioCapture, "the commentary leg's head queue"},
		{nameAudioProxySink, split, classAudioCapture, "the commentary proxysink"},
		{nameAudioProxySink, fused, classAudioCapture, "the commentary proxysink on a fused seat"},

		{nameVideoProxyQueue, split, classVideoCapture, "the picture leg's head queue"},
		{nameVideoProxySink, split, classVideoCapture, "the picture proxysink"},
		// NOT upgraded on a fused seat: these two are downstream of the tee and
		// downstream of everything that clocks anything, so the card goes on
		// producing and the commentary goes on being clocked.
		{nameVideoProxySink, fused, classVideoCapture,
			"the picture proxysink on a fused seat, which must not take the commentary off air"},

		// The preview must still win against the new prefixes.
		{namePreviewSink, fused, classPreview, "the confidence monitor"},
	} {
		if got := classifyBusError(tc.source, tc.legs); got != tc.want {
			t.Errorf("classifyBusError(%q) = %v, want %v — %s",
				tc.source, got, tc.want, tc.why)
		}
	}
}

// TestCaptureLegsStringUsesThePlansOwnLabels keeps a log line and PLAN.md section
// 2 readable side by side.
func TestCaptureLegsStringUsesThePlansOwnLabels(t *testing.T) {
	got := fmt.Sprint(CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard})
	if got != "FUSED" {
		t.Errorf("CaptureLegs.String() = %q, want %q", got, "FUSED")
	}
}
