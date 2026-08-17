//go:build !cgo || gststub

// capture_stub_test.go is the guard over the CAPTURE STUB'S MODEL — the widths,
// the meters, the signal transitions and the fault routing that everything above
// internal/gst is developed and tested against.
//
// A stub that merely refuses would let every one of those layers be written
// against behaviour the shipped build does not have, and the failures would all
// be of the same shape: a routing panel sized for a device the operator did not
// select, a picker armed on a stereo microphone, a camera lamp claiming a state
// nothing asked the card for, a capture fault reaching internal/sender and
// costing seven seconds of reconnect off air. Each of those has a test here.
package gst

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// stubDeviceAt installs a single fake capture device of the given width and
// returns its id. The list is restored at the end of the test, because it is
// package state that ListInputDevices answers from too.
func stubDeviceAt(t *testing.T, channels int) string {
	t.Helper()
	id := fmt.Sprintf("{0.0.1.00000000}.{stub-%d-channel}", channels)
	SetStubDevices([]Device{{
		ID:       id,
		Name:     fmt.Sprintf("Stub %d-channel input", channels),
		Kind:     KindNative,
		Channels: channels,
	}})
	t.Cleanup(func() { SetStubDevices(nil) })
	return id
}

// startedStubCapture builds and starts a commentary capture against opts, and
// stops it at the end of the test. Every capture pipeline must be stopped —
// including one that was never started — because a goroutine is parked on it from
// construction.
func startedStubCapture(t *testing.T, opts CaptureOpts) *StubCapture {
	t.Helper()
	c, err := NewStubCapture(opts)
	if err != nil {
		t.Fatalf("NewStubCapture(%+v) failed: %v", opts.Legs, err)
	}
	t.Cleanup(func() { c.Stop() })
	if err := c.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	return c
}

// ---------------------------------------------------------------------------
// The width, and where it comes from
// ---------------------------------------------------------------------------

// TestTheDeviceModelPresentsEveryWidthTheOperatorNamed is the reason this twin
// has a device model at all.
//
// The routing panel appears at EVERY width the pad negotiates, including 1 and 2:
// the operator overruled a proposed `width > 2` gate on 2026-08-16 because
// flipping a stereo pair and routing a mono input to both sides are real routing
// decisions. A stub that could only present two channels would leave the grid at
// every other width — the mono line, the non-square 2x8, the 2x32 ceiling —
// undevelopable and untested.
func TestTheDeviceModelPresentsEveryWidthTheOperatorNamed(t *testing.T) {
	for _, width := range []int{1, 2, 3, 8, 16, MaxInputChannels} {
		t.Run(fmt.Sprintf("%d channels", width), func(t *testing.T) {
			id := stubDeviceAt(t, width)
			c := startedStubCapture(t, CaptureOpts{
				Legs:          CaptureLegs{Commentary: CommentaryNative},
				AudioDeviceID: id,
			})

			if got := c.InputChannels(); got != width {
				t.Errorf("InputChannels() = %d, want %d", got, width)
			}
			// THE MATRIX IS UNIFORM, at 1 and 2 as much as at 32. There is no
			// source-kind test and no width test in front of it: audioconvert
			// cannot map unpositioned channels to stereo without one, and no
			// CoreAudio source can emit a positioned mask above two channels.
			if got := c.MatrixWidth(); got != width {
				t.Errorf("MatrixWidth() = %d, want %d — every seat carries a matrix, including "+
					"the stereo and mono ones", got, width)
			}
			if got := c.WidthOrigin(); got != widthFromDeviceModel {
				t.Errorf("WidthOrigin() = %q, want %q: the width has to be a fact about the "+
					"DEVICE, not a guess from the source kind", got, widthFromDeviceModel)
			}
			if _, err := c.ChannelMap().MixMatrix(width); err != nil {
				t.Errorf("the map in force does not build a matrix at width %d: %v", width, err)
			}
		})
	}
}

// TestAWideNativeDeviceBehavesExactlyLikeTheCard is the R2 unblock, stated as an
// assertion.
//
// gst_stub.go:483-493 encoded "a DeckLink commentary means sixteen unpositioned
// channels means a matrix", and while that rule stands nothing at Gate A can tell
// a routing panel that works from one that only works for a card. Settled from
// GStreamer's own source: gstosxcoreaudio.c:886-889 sets `layout = NULL` for
// EVERY source, so a 16-in Focusrite is byte-for-byte the same problem as the
// card.
func TestAWideNativeDeviceBehavesExactlyLikeTheCard(t *testing.T) {
	const card = "2747401380"
	id := stubDeviceAt(t, deckLinkAudioChannels)

	native := startedStubCapture(t, CaptureOpts{
		Legs:          CaptureLegs{Commentary: CommentaryNative},
		AudioDeviceID: id,
	})
	cardSeat := startedStubCapture(t, CaptureOpts{
		Legs:           CaptureLegs{Commentary: CommentaryCard},
		AudioCaptureID: card,
	})

	if native.InputChannels() != cardSeat.InputChannels() {
		t.Errorf("a %d-channel CoreAudio device negotiated %d and the card negotiated %d",
			deckLinkAudioChannels, native.InputChannels(), cardSeat.InputChannels())
	}
	if native.MatrixWidth() != cardSeat.MatrixWidth() {
		t.Errorf("the native seat wrote a %d-wide matrix and the card a %d-wide one; the routing "+
			"panel above this cannot be tested on the path the operator will use",
			native.MatrixWidth(), cardSeat.MatrixWidth())
	}
	if got := cardSeat.WidthOrigin(); got != widthFromCard {
		t.Errorf("the card's WidthOrigin() = %q, want %q", got, widthFromCard)
	}
}

// TestTheCardsWidthIsStatedAndNotDiscovered pins the one source-kind rule that is
// allowed to exist here, and pins it as being about the SHIPPED PARSE STRING.
//
// decklinkaudiosrc is built with channels=16, so an enumeration that says
// something else about the same card is describing its provider's `{ 2, 8, 16 }`
// offer rather than what the element will produce.
func TestTheCardsWidthIsStatedAndNotDiscovered(t *testing.T) {
	c := startedStubCapture(t, CaptureOpts{
		Legs:           CaptureLegs{Commentary: CommentaryCard},
		AudioCaptureID: "2747401380",
		DeviceChannels: 2,
	})
	if got := c.InputChannels(); got != deckLinkAudioChannels {
		t.Errorf("a card commentary negotiated %d channels with DeviceChannels=2; the description "+
			"states channels=%d on the element, so the number is stated rather than discovered",
			got, deckLinkAudioChannels)
	}
}

// TestTheEnumerationOutranksTheDeviceModel keeps the resolution order the real
// twin's: what the application read from the enumerated device's caps is what the
// application will pass, and the model is only the last resort standing in for
// asking the source pad.
func TestTheEnumerationOutranksTheDeviceModel(t *testing.T) {
	id := stubDeviceAt(t, 16)
	c := startedStubCapture(t, CaptureOpts{
		Legs:           CaptureLegs{Commentary: CommentaryNative},
		AudioDeviceID:  id,
		DeviceChannels: 8,
	})
	if got := c.InputChannels(); got != 8 {
		t.Errorf("InputChannels() = %d, want 8 from CaptureOpts.DeviceChannels", got)
	}
	if got := c.WidthOrigin(); got != widthFromEnumeration {
		t.Errorf("WidthOrigin() = %q, want %q", got, widthFromEnumeration)
	}
}

// TestAWidthNothingCouldEstablishWritesNoMatrix is the real twin's judgement, and
// it is a judgement rather than an oversight: a guessed width does not degrade the
// feed, it stops the capture chain with "streaming stopped, reason error (-5)",
// which reads to an operator as a broken device rather than as a bad matrix.
func TestAWidthNothingCouldEstablishWritesNoMatrix(t *testing.T) {
	SetStubDevices([]Device{})
	t.Cleanup(func() { SetStubDevices(nil) })

	widths := make(chan int, 4)
	c := startedStubCapture(t, CaptureOpts{
		Legs:            CaptureLegs{Commentary: CommentaryNative},
		AudioDeviceID:   "{0.0.1.00000000}.{a-device-nothing-enumerated}",
		OnInputChannels: func(_ string, n int) { widths <- n },
	})

	if got := c.MatrixWidth(); got != 0 {
		t.Errorf("a matrix %d wide was written for a device nothing could size", got)
	}
	if got := c.WidthOrigin(); got != widthUnknown {
		t.Errorf("WidthOrigin() = %q, want %q", got, widthUnknown)
	}
	// AND NOTHING IS PUBLISHED. A width on the wire is what sizes the grid, so
	// publishing a zero would draw a panel with no columns instead of no panel.
	select {
	case n := <-widths:
		t.Errorf("OnInputChannels published %d for a device with no known width", n)
	case <-time.After(50 * time.Millisecond):
	}
	// And the routing call refuses by naming the missing thing, rather than
	// writing a matrix against a width nobody has.
	err := c.SetChannelMap(DefaultChannelMap(2))
	if err == nil {
		t.Fatal("SetChannelMap succeeded with no negotiated width to size a matrix against")
	}
	if !strings.Contains(err.Error(), "negotiated") {
		t.Errorf("the refusal does not say the pad has not negotiated a width: %v", err)
	}
}

// TestADeviceWiderThanThisBuildMapsIsRefusedByName — a device above
// MaxInputChannels is a NAMED REFUSAL of that device, off air, and never a Start
// that refuses without saying why.
func TestADeviceWiderThanThisBuildMapsIsRefusedByName(t *testing.T) {
	id := stubDeviceAt(t, MaxInputChannels+32)
	c, err := NewStubCapture(CaptureOpts{
		Legs:          CaptureLegs{Commentary: CommentaryNative},
		AudioDeviceID: id,
	})
	if err != nil {
		t.Fatalf("NewStubCapture failed: %v", err)
	}
	t.Cleanup(func() { c.Stop() })

	startErr := c.Start()
	if startErr == nil {
		t.Fatal("a device presenting more channels than this build maps was started")
	}
	if !strings.Contains(startErr.Error(), fmt.Sprint(MaxInputChannels)) {
		t.Errorf("the refusal does not name the limit it is refusing against: %v", startErr)
	}
}

// TestTheWidthIsPublishedStampedWithItsDevice, and published OFF THE LOCK.
//
// The stamp is not optional: without it there is a window between selecting a
// Focusrite and the capture renegotiating in which the grid still holds the
// previous device's sixteen, and a crosspoint pressed in that window writes a 2x16
// matrix onto a two-channel pad.
//
// The lock half is the one a stub gets wrong invisibly. OnInputChannels' obvious
// implementation reads the width back, sizes the grid and applies the stored
// routing — all of which re-enter this pipeline — so a stub that called it under
// its own mutex would deadlock at Gate A against code that works perfectly at Gate
// B. This test IS that implementation.
func TestTheWidthIsPublishedStampedWithItsDeviceAndOffTheLock(t *testing.T) {
	id := stubDeviceAt(t, 8)

	type published struct {
		key   string
		width int
		back  int
		err   error
	}
	got := make(chan published, 4)
	var c *StubCapture
	c, err := NewStubCapture(CaptureOpts{
		Legs:          CaptureLegs{Commentary: CommentaryNative},
		AudioDeviceID: id,
		OnInputChannels: func(key string, width int) {
			// Exactly what the application does with this callback: read the
			// width back, size the grid, apply the stored routing.
			p := published{key: key, width: width, back: c.InputChannels()}
			p.err = c.SetChannelMap(DefaultChannelMap(width))
			got <- p
		},
	})
	if err != nil {
		t.Fatalf("NewStubCapture failed: %v", err)
	}
	t.Cleanup(func() { c.Stop() })

	if err := c.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	select {
	case p := <-got:
		if want := "native:" + id; p.key != want {
			t.Errorf("the width was stamped %q, want %q", p.key, want)
		}
		if p.width != 8 || p.back != 8 {
			t.Errorf("published width %d, read back %d, want 8 and 8", p.width, p.back)
		}
		if p.err != nil {
			t.Errorf("the routing the callback applied was refused: %v", p.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnInputChannels never returned. It is called under the pipeline's own lock, so " +
			"the obvious implementation — size the grid, apply the routing — deadlocks at Gate A " +
			"against code that works at Gate B")
	}
}

// ---------------------------------------------------------------------------
// The meters
// ---------------------------------------------------------------------------

// meterFrames collects the two synthetic meters a started capture delivers.
type meterFrames struct {
	programme chan Levels
	channels  chan Levels
}

func newMeterFrames() *meterFrames {
	return &meterFrames{
		programme: make(chan Levels, 64),
		channels:  make(chan Levels, 64),
	}
}

// send never blocks: these callbacks run on the stub's meter goroutine, which
// Stop joins, and a test that wedged one would hang its own cleanup rather than
// fail.
func (m *meterFrames) send(to chan Levels) func(Levels) {
	return func(l Levels) {
		select {
		case to <- l:
		default:
		}
	}
}

// TestTheMetersRunWithNoSendPipelineAnywhereInTheProcess is R1, at Gate A.
//
// The meters, the preview, the routing width, the signal lamp and the mute exist
// before START and survive STOP; there is no session, no encoder and no SRT
// anywhere in this test. A stub that only metered while sending would let the
// screen that this whole design exists to deliver be built against a fake that
// cannot show it.
func TestTheMetersRunWithNoSendPipelineAnywhereInTheProcess(t *testing.T) {
	id := stubDeviceAt(t, 2)
	m := newMeterFrames()
	c := startedStubCapture(t, CaptureOpts{
		Legs:          CaptureLegs{Commentary: CommentaryNative},
		AudioDeviceID: id,
		OnLevels:      m.send(m.programme),
	})

	select {
	case l := <-m.programme:
		if len(l.PeakDB) != ChannelMapOutputs || len(l.RMSDB) != ChannelMapOutputs {
			t.Errorf("a programme frame carries %d peaks and %d rms values, want %d of each",
				len(l.PeakDB), len(l.RMSDB), ChannelMapOutputs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no programme meter frame arrived from a running capture with no session")
	}

	// AND NOTHING AFTER Stop. The join is the promise: a frozen meter reads as a
	// live one, which is the direction this project never lets a status display
	// be wrong in.
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	for len(m.programme) > 0 {
		<-m.programme
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(m.programme); n != 0 {
		t.Errorf("%d meter frames were delivered after Stop returned", n)
	}
}

// TestThePickerIsArmedOnlyAboveStereo is PLAN.md 4.3's condition, measured
// through the behaviour rather than through the predicate.
//
// A stereo seat must never push a duplicate of the programme meter over the
// webview bridge ten times a second for the whole of a ninety-minute match, and a
// multichannel seat must push bars as wide as the device — because "ask the
// commentator to talk and watch which bar moves" is the picker's entire job.
func TestThePickerIsArmedOnlyAboveStereo(t *testing.T) {
	for _, width := range []int{1, 2, 3, 16} {
		t.Run(fmt.Sprintf("%d channels", width), func(t *testing.T) {
			id := stubDeviceAt(t, width)
			m := newMeterFrames()
			startedStubCapture(t, CaptureOpts{
				Legs:            CaptureLegs{Commentary: CommentaryNative},
				AudioDeviceID:   id,
				OnLevels:        m.send(m.programme),
				OnChannelLevels: m.send(m.channels),
			})

			// Wait on the PROGRAMME meter, which every width delivers, so that the
			// negative case is a decision this pipeline made rather than a race
			// with a goroutine that had not started yet.
			for i := 0; i < 4; i++ {
				select {
				case <-m.programme:
				case <-time.After(2 * time.Second):
					t.Fatal("the programme meter never delivered")
				}
			}

			wantPicker := width > ChannelMapOutputs
			select {
			case l := <-m.channels:
				if !wantPicker {
					t.Fatalf("a %d-channel seat posted per-channel frames; that is a duplicate of "+
						"the programme meter, ten times a second, for ninety minutes", width)
				}
				if len(l.PeakDB) != width {
					t.Errorf("a per-channel frame carries %d bars, want %d — a picker that has "+
						"drawn the wrong number of inputs cannot find the commentator",
						len(l.PeakDB), width)
				}
			case <-time.After(500 * time.Millisecond):
				if wantPicker {
					t.Fatalf("a %d-channel seat posted no per-channel frames, so the routing "+
						"panel's bars can never move", width)
				}
			}
		})
	}
}

// TestChannelMeterWantedIsTheDeclaredCondition pins the shared predicate both
// twins arm on, at every width the operator named. It is stated here as well as
// exercised above because the real twin asks it twice — against the resolved width
// and against the negotiated one — and neither of those calls is reachable at Gate
// A.
func TestChannelMeterWantedIsTheDeclaredCondition(t *testing.T) {
	for _, tc := range []struct {
		width int
		want  bool
	}{
		{0, false}, {1, false}, {2, false}, {3, true}, {8, true}, {16, true}, {32, true},
	} {
		if got := channelMeterWanted(tc.width, true); got != tc.want {
			t.Errorf("channelMeterWanted(%d, true) = %t, want %t", tc.width, got, tc.want)
		}
		if channelMeterWanted(tc.width, false) {
			t.Errorf("channelMeterWanted(%d, false) armed the picker with nobody to deliver to",
				tc.width)
		}
	}
}

// TestAPictureCaptureMetersNothing is the split seat's other half.
//
// The application hands the same callbacks to both pipelines it plans, and only
// one of them has a level element in it: a picture capture is a slate or a card
// through a conform chain into a proxysink, with no alevel and no chlevel
// anywhere. A stub that metered from it would double every meter on every split
// seat, which is most of them.
func TestAPictureCaptureMetersNothing(t *testing.T) {
	m := newMeterFrames()
	startedStubCapture(t, CaptureOpts{
		Legs:            CaptureLegs{Picture: PictureSlate},
		SlatePath:       "slate.png",
		OnLevels:        m.send(m.programme),
		OnChannelLevels: m.send(m.channels),
	})
	time.Sleep(200 * time.Millisecond)
	if n := len(m.programme) + len(m.channels); n != 0 {
		t.Errorf("a picture-only capture delivered %d meter frames; it has no level element in it", n)
	}
}

// ---------------------------------------------------------------------------
// The signal watchdog
// ---------------------------------------------------------------------------

// stubSignalClock installs a hand-driven clock for the signal watchdog and
// returns the channel that drives it. Real time is not involved: a LOST verdict
// is eight samples deep by design, and waiting two seconds per test to prove it
// would buy nothing the debouncer's own Gate A tests do not already prove.
//
// A SEND ON THIS CHANNEL IS ALSO THE TEST for a pipeline that should have no
// watchdog at all: nothing receives it, so a send that succeeds is a goroutine
// polling a card that is not there.
func stubSignalClock(t *testing.T) chan time.Time {
	t.Helper()
	ticks := make(chan time.Time)
	stubCaptureMu.Lock()
	stubSignalTicks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	stubCaptureMu.Unlock()
	t.Cleanup(func() {
		stubCaptureMu.Lock()
		stubSignalTicks = nil
		stubCaptureMu.Unlock()
	})
	return ticks
}

// TestTheCameraLampIsFedFromTheCardAndOnlyFromTheCard covers both halves of
// signalWatchWanted, which are two different promises.
//
// A seat with a card gets a debounced lamp from launch — the fault surfaces at
// launch rather than twenty minutes before kick-off. A seat WITHOUT one gets no
// goroutine, no ticker and no report at all: reporting SignalOK there would be
// this application inventing a green lamp out of an absence of evidence, and
// SignalLost would put a red lamp on every machine without a card.
func TestTheCameraLampIsFedFromTheCardAndOnlyFromTheCard(t *testing.T) {
	const card = "2747401380"
	ticks := stubSignalClock(t)

	reports := make(chan SignalReport, 8)
	c := startedStubCapture(t, CaptureOpts{
		Legs:           CaptureLegs{Picture: PictureCard},
		VideoCaptureID: card,
		OnSignal:       func(r SignalReport) { reports <- r },
	})

	// The seed is a locked input, so the lamp goes green after signalOKHold and
	// not before: no hold-off is one sample deep, so a single reading can never
	// move it.
	for i := 0; i < signalOKSamples; i++ {
		ticks <- time.Now()
	}
	select {
	case r := <-reports:
		if r.State != SignalOK {
			t.Fatalf("the first report is %q, want %q", r.State, SignalOK)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a card seat delivered no signal report at all")
	}

	// THE CABLE COMES OUT. It takes signalLostHold of continuous no-lock, which
	// is the asymmetry the file argues for: an alarm nobody can verify is worse
	// than one that is 2 s late.
	c.SetStubSignal(false)
	for i := 0; i < signalLostSamples; i++ {
		ticks <- time.Now()
	}
	select {
	case r := <-reports:
		if r.State != SignalLost {
			t.Fatalf("after the cable came out the lamp reports %q, want %q", r.State, SignalLost)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no report followed a sustained loss of lock")
	}
}

// TestASeatWithNoCardClaimsNothingAboutTheSignal is the other half, and it is the
// one that costs nothing when it is right: no element with a signal property means
// no goroutine and no ticker for the life of the pipeline.
func TestASeatWithNoCardClaimsNothingAboutTheSignal(t *testing.T) {
	ticks := stubSignalClock(t)
	id := stubDeviceAt(t, 2)

	reports := make(chan SignalReport, 4)
	c := startedStubCapture(t, CaptureOpts{
		Legs:          CaptureLegs{Commentary: CommentaryNative},
		AudioDeviceID: id,
		OnSignal:      func(r SignalReport) { reports <- r },
	})
	// Even told the cable is out, there is nothing here that could have been
	// asked — and nothing is polling, which is what the send proves.
	c.SetStubSignal(false)
	select {
	case ticks <- time.Now():
		t.Error("a seat with no decklinkvideosrc in it is running a signal watchdog; there is no " +
			"element here with a signal property, so whatever it reports is invented")
	case <-time.After(100 * time.Millisecond):
	}
	if n := len(reports); n != 0 {
		t.Errorf("a seat with no card delivered %d signal reports", n)
	}
}

// TestTheClockCompanionSeatStillWatchesTheCard is the configuration that is easy
// to get wrong: the picture is the slate and the COMMENTARY is the card, so the
// only decklinkvideosrc in the graph is the clock companion — and it is still the
// thing that knows whether the card has locked.
func TestTheClockCompanionSeatStillWatchesTheCard(t *testing.T) {
	const card = "2747401380"
	ticks := stubSignalClock(t)

	reports := make(chan SignalReport, 8)
	startedStubCapture(t, CaptureOpts{
		Legs:           CaptureLegs{Commentary: CommentaryCard},
		AudioCaptureID: card,
		OnSignal:       func(r SignalReport) { reports <- r },
	})
	for i := 0; i < signalOKSamples; i++ {
		ticks <- time.Now()
	}
	select {
	case r := <-reports:
		if r.State != SignalOK {
			t.Errorf("the clock companion's seat reports %q, want %q", r.State, SignalOK)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a card COMMENTARY seat reported nothing about the card it is holding")
	}
}

// ---------------------------------------------------------------------------
// Faults
// ---------------------------------------------------------------------------

// TestACaptureFaultIsRoutedByTheClassifierTheRigUses is the routing an operator
// actually sees, asserted through the stub the application is tested against.
//
// The three outcomes are three different nights: a confidence monitor that dies is
// a warning nobody needs to act on, a picture leg that dies on a split seat leaves
// the commentary metering, and a commentary failure is fatal and named so that the
// send stops deliberately instead of internal/sender spending seven seconds
// reconnecting to a network that is fine.
func TestACaptureFaultIsRoutedByTheClassifierTheRigUses(t *testing.T) {
	const card = "2747401380"

	t.Run("the confidence monitor is spared absolutely", func(t *testing.T) {
		c := startedStubCapture(t, CaptureOpts{
			Legs:           CaptureLegs{Picture: PictureCard, Preview: true},
			VideoCaptureID: card,
		})
		c.InjectBusError(namePreviewSink, errors.New("could not create a GL context"))
		if err := c.Health(); err != nil {
			t.Errorf("a preview failure latched a capture fault: %v", err)
		}
		if len(c.Faults()) != 0 {
			t.Error("a preview failure reached Faults(); nothing it does can reach the feed")
		}
		if len(c.Warnings()) != 1 {
			t.Error("a preview failure delivered no warning either, so it is invisible")
		}
	})

	// REWRITTEN, AND THE OLD ASSERTION IS QUOTED BECAUSE IT LOOKED RIGHT. It was
	// `Health() == nil` and `len(Warnings()) == 1`, i.e. "a picture death on a
	// split seat is invisible except in the log" — which is what shipped, and what
	// made a dead card produce nothing on the capture event, nothing on the error
	// event, a frozen preview, a Health() of nil that let ArmForSend succeed, and a
	// START refused two seconds later by a message about a pad.
	//
	// The commentary half of that claim survives untouched and is still asserted:
	// this is not a death of the pipeline and Health() must stay nil.
	t.Run("a picture failure on a split seat is the picture's alone", func(t *testing.T) {
		c := startedStubCapture(t, CaptureOpts{
			Legs:           CaptureLegs{Picture: PictureCard},
			VideoCaptureID: card,
		})
		c.InjectBusError(nameVideoCaptureSrc, errors.New("Internal data stream error"))

		if err := c.Health(); err != nil {
			t.Errorf("a picture failure on a seat whose commentary is elsewhere latched as a "+
				"whole-pipeline death: %v", err)
		}
		if err := c.PictureHealth(); err == nil {
			t.Fatal("a picture failure latched nothing PictureHealth can report, so the capture " +
				"panel goes on reading live over a dead card and START is refused two seconds " +
				"later by a message about a pad")
		}
		if len(c.Faults()) != 1 {
			t.Errorf("the picture failure put %d errors on Faults(), want 1. A warning is drained "+
				"and discarded by the application, which is why this used to change nothing on "+
				"screen", len(c.Faults()))
		}
		if !errors.Is(c.PictureHealth(), ErrPipelineFatal) {
			t.Error("the latched picture fault does not wrap ErrPipelineFatal, so nothing " +
				"downstream can classify it")
		}
	})

	t.Run("a dead picture leg refuses the arming", func(t *testing.T) {
		// mpegtsmux emits nothing at all while one of its two inputs is silent —
		// measured, vq:src 0, aq:src 187 at full rate, mux:src 0 — so a send over a
		// dead picture is as complete a stop as one over a dead microphone. The
		// refusal has to come HERE, with the named cause, and not from the liveness
		// gate two seconds later with the name of a pad.
		c := startedStubCapture(t, CaptureOpts{
			Legs:           CaptureLegs{Picture: PictureCard},
			VideoCaptureID: card,
		})
		if err := c.ArmForSend(); err != nil {
			t.Fatalf("a healthy capture refused the arming: %v", err)
		}
		c.InjectBusError(nameVideoCaptureSrc, errors.New("Internal data stream error"))
		if err := c.ArmForSend(); err == nil {
			t.Fatal("ArmForSend succeeded over a dead picture leg, so START reaches PLAYING and " +
				"the operator is refused by the muxer watchdog rather than by the card fault")
		}
	})

	t.Run("the same failure on a fused seat is the commentary's", func(t *testing.T) {
		// THE UPGRADE, and the measurement behind it: the card clocks the audio,
		// so a video capture error on a fused seat is an audio fault wearing a
		// video element's name.
		c := startedStubCapture(t, CaptureOpts{
			Legs:           CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard},
			VideoCaptureID: card,
			AudioCaptureID: card,
		})
		c.InjectBusError(nameVideoCaptureSrc, errors.New("Internal data stream error"))
		if err := c.Health(); err == nil {
			t.Fatal("a video capture failure on a FUSED seat was not fatal; the commentary is " +
				"clocked by that element and there is nothing left producing audio")
		}
	})

	t.Run("a commentary failure is fatal and reaches Faults", func(t *testing.T) {
		id := stubDeviceAt(t, 2)
		c := startedStubCapture(t, CaptureOpts{
			Legs:          CaptureLegs{Commentary: CommentaryNative},
			AudioDeviceID: id,
		})
		c.InjectBusError(captureAudioSrcName, errors.New("Could not read from resource"))
		if err := c.Health(); err == nil {
			t.Fatal("a commentary capture failure did not latch")
		}
		select {
		case err := <-c.Faults():
			if err == nil {
				t.Fatal("Faults() delivered nil, which is what a closed channel reads as")
			}
			// The application decides whether to stop the send session by asking
			// this exact question of this exact sentinel.
			if !errors.Is(err, ErrPipelineFatal) {
				t.Errorf("the capture fault does not wrap ErrPipelineFatal: %v", err)
			}
		default:
			t.Fatal("the commentary failure never reached Faults()")
		}
	})
}

// ---------------------------------------------------------------------------
// The build, and what a failure costs
// ---------------------------------------------------------------------------

// TestAPreviewThatWillNotStartCostsThePreviewAndNotTheCapture is the
// retry-without-preview, which is the difference between a seat that comes up
// without a confidence monitor and a seat that comes up with no capture at all —
// no meters, no commentary — because a window would not take a GL context.
func TestAPreviewThatWillNotStartCostsThePreviewAndNotTheCapture(t *testing.T) {
	const card = "2747401380"
	SetStubCaptureStartError(func(legs CaptureLegs) error {
		if legs.Preview {
			return errors.New("glimagesink: could not create a GL context")
		}
		return nil
	})
	t.Cleanup(func() { SetStubCaptureStartError(nil) })

	c := startedStubCapture(t, CaptureOpts{
		Legs:           CaptureLegs{Picture: PictureCard, Preview: true},
		VideoCaptureID: card,
	})
	if !c.PreviewDropped() {
		t.Error("the capture reports a preview it does not have")
	}
}

// TestACardThatWillNotOpenLeavesTheSlateSeatBuildable is PLAN.md step 7's rule at
// the layer below the one that implements it: the app NEVER fails to launch
// because of the card, so the failure has to be a Start that fails cleanly and
// leaves a slate build free to succeed.
func TestACardThatWillNotOpenLeavesTheSlateSeatBuildable(t *testing.T) {
	const card = "2747401380"
	SetStubCaptureStartError(func(legs CaptureLegs) error {
		if legs.HasCard() {
			return errors.New("decklinkvideosrc: not-negotiated (-4)")
		}
		return nil
	})
	t.Cleanup(func() { SetStubCaptureStartError(nil) })

	cardSeat, err := NewStubCapture(CaptureOpts{
		Legs:           CaptureLegs{Picture: PictureCard},
		VideoCaptureID: card,
	})
	if err != nil {
		t.Fatalf("NewStubCapture failed: %v", err)
	}
	t.Cleanup(func() { cardSeat.Stop() })
	if err := cardSeat.Start(); err == nil {
		t.Fatal("the card opened in a test that said it would not")
	}
	// The failed pipeline must still be stoppable — it is the only thing that
	// ends the goroutine parked on it — and the fallback must build.
	if err := cardSeat.Stop(); err != nil {
		t.Errorf("Stop on a capture whose Start failed: %v", err)
	}
	startedStubCapture(t, CaptureOpts{
		Legs:      CaptureLegs{Picture: PictureSlate},
		SlatePath: "slate.png",
	})
}

// ---------------------------------------------------------------------------
// The seam, against the twin the application will actually hold
// ---------------------------------------------------------------------------

// TestEverySendSessionAgainstTheStubTwinClaimsAndArms is the zero-byte second
// session, asserted against the object the application builds rather than against
// a hand-rolled fake.
//
// gstproxysink.c resets sent_stream_start/sent_caps only on READY->PAUSED, so in
// an always-live pipeline every consumer AFTER THE FIRST receives no STREAM_START,
// no CAPS and no SEGMENT: measured 1,133,076 bytes on cycle 1 and 0 bytes on
// cycles 2 and 3, with SRT connected and every lamp green.
func TestEverySendSessionAgainstTheStubTwinClaimsAndArms(t *testing.T) {
	id := stubDeviceAt(t, 2)
	c := startedStubCapture(t, CaptureOpts{
		Legs:          CaptureLegs{Commentary: CommentaryNative},
		AudioDeviceID: id,
	})

	if got := c.Legs(); got.Commentary != CommentaryNative || got.Picture != PictureNone {
		t.Errorf("Legs() = %s, want a commentary-only leg-set", got)
	}
	if got := c.ProxySinks(); len(got) != 1 || got[0] != nameAudioProxySink {
		t.Errorf("ProxySinks() = %v, want [%s]", got, nameAudioProxySink)
	}

	for cycle := 1; cycle <= 3; cycle++ {
		seam, err := NewSend(CaptureSet{Commentary: c})
		if err != nil {
			t.Fatalf("send session %d could not have the seam: %v", cycle, err)
		}
		if got := c.Armings(); got != cycle {
			t.Fatalf("after send session %d the seam had been armed %d times, want %d. EVERY "+
				"session after the first must arm or it carries zero bytes", cycle, got, cycle)
		}
		seam.Stop()
	}
}

// TestStubCaptureDescriptionNamesTheShape keeps the Gate A shape summary honest
// about the two things a reader checks it for: which proxysink each leg ends in,
// and whether this leg-set drags in a clock companion — the decklinkvideosrc that
// exists only because decklinkaudiosrc cannot preroll without one.
func TestStubCaptureDescriptionNamesTheShape(t *testing.T) {
	fused := StubCaptureDescription(CaptureLegs{
		Picture: PictureCard, Commentary: CommentaryCard, Preview: true,
	})
	for _, want := range []string{nameVideoProxySink, nameAudioProxySink, "preview"} {
		if !strings.Contains(fused, want) {
			t.Errorf("the FUSED-PREVIEW summary %q does not mention %s", fused, want)
		}
	}
	if strings.Contains(fused, "clock-companion") {
		t.Errorf("the FUSED summary %q builds a clock companion; vcapsrc IS the clock there, and "+
			"a second decklinkvideosrc cannot have the exclusive card", fused)
	}
	slateSeat := StubCaptureDescription(CaptureLegs{Commentary: CommentaryCard})
	if !strings.Contains(slateSeat, "clock-companion") {
		t.Errorf("the C-CARD summary %q has no clock companion, so its commentary would never "+
			"produce a buffer — measured, 0 level messages against 160", slateSeat)
	}
}

// TestStopEndsEveryGoroutineParkedOnAnUnstartedCapture is the rule NewCapture
// states and the one a caller is most likely to skip: the SECOND leg of a planned
// pair, whose first leg would not open, is abandoned without ever being started —
// and a goroutine has been parked on it since construction.
//
// It also runs two Stops at once, because the teardown paths that call it are a
// shutdown and an operator action and both can be in flight together.
func TestStopEndsEveryGoroutineParkedOnAnUnstartedCapture(t *testing.T) {
	id := stubDeviceAt(t, 2)
	c, err := NewStubCapture(CaptureOpts{
		Legs:            CaptureLegs{Commentary: CommentaryNative},
		AudioDeviceID:   id,
		OnInputChannels: func(string, int) {},
	})
	if err != nil {
		t.Fatalf("NewStubCapture failed: %v", err)
	}

	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { done <- c.Stop() }()
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Stop failed: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("two concurrent Stops did not both return")
		}
	}

	for _, ch := range []struct {
		name string
		open func() bool
	}{
		{"Faults()", func() bool { _, ok := <-c.Faults(); return ok }},
		{"Warnings()", func() bool { _, ok := <-c.Warnings(); return ok }},
	} {
		if ch.open() {
			t.Errorf("%s is still open after Stop, so whatever is parked on it never ends", ch.name)
		}
	}
}

// ---------------------------------------------------------------------------
// The mute
// ---------------------------------------------------------------------------

// TestTheMuteIsCarriedIntoTheSessionAndSettableWithoutOne is PLAN.md 0-BIS A2,
// which REVERSES the argument at app.go:4784 and must therefore be pinned rather
// than left to be re-litigated by a reader who finds that argument first.
//
// The element exists from launch, so a latch set before START is still set at
// START; what answers the fear of a control that lies is VISIBILITY — the mute
// sits upstream of alevel, so a muted commentator has a flat programme meter and a
// mute banner, before and after START.
func TestTheMuteIsCarriedIntoTheSessionAndSettableWithoutOne(t *testing.T) {
	id := stubDeviceAt(t, 2)
	c := startedStubCapture(t, CaptureOpts{
		Legs:           CaptureLegs{Commentary: CommentaryNative},
		AudioDeviceID:  id,
		MuteCommentary: true,
	})
	if !c.CommentaryMuted() {
		t.Fatal("a capture built muted reports itself open")
	}
	// No session exists anywhere in this process, and the call still works.
	if err := c.SetCommentaryMute(false); err != nil {
		t.Fatalf("SetCommentaryMute with no session running: %v", err)
	}
	if c.CommentaryMuted() {
		t.Error("the unmute did not take")
	}
}
