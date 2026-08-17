//go:build !cgo || gststub

// capture_stub.go is CapturePipeline's Gate A twin: the same state machine, the
// same refusals and the same channel-map arithmetic, with no GStreamer under it.
//
// # It is a MODEL and not a pile of no-ops
//
// gst_stub.go set the standard this file is held to: it emits synthetic level
// frames on the real element's interval so that the whole meter path — event
// pump, throttle, bars — can be built and watched moving with no audio hardware
// in the machine. The capture layer needs more of that rather than less, because
// everything R1 and R2 added is now upstream of START and there is no session to
// hang a fake on. Four things are therefore modelled here, and each one exists
// because a layer above cannot be developed or tested without it:
//
//   - THE NEGOTIATED WIDTH, from a device model. The routing panel is sized from
//     it, and the operator asked for that panel at every width including 1 and 2.
//   - LEVEL FRAMES, programme and per-channel, at the two real intervals. The
//     picker's whole job is "ask the commentator to talk and watch which bar
//     moves", which cannot be built against a stub that never moves one.
//   - SIGNAL TRANSITIONS, through the REAL debouncer in signalwatch.go. The
//     CAMERA lamp is live from launch now, so a stub that never reports leaves it
//     grey for the entire development of the screen it dominates.
//   - FAULTS, through the REAL classifier in capturefault.go, so that a Gate A
//     test can assert what an operator sees: a preview failure is a warning, a
//     picture failure is a warning, a commentary failure is fatal and named.
//
// # It models width from a DEVICE and not from the source kind
//
// That is the one thing about this twin that must not be copied from the old
// stub pipeline. gst_stub.go:483-493 encodes "a DeckLink commentary means
// sixteen unpositioned channels means a matrix", which was true of the shipped
// build at the time and is now a rule that hides the whole of R2: a stub
// Focusrite at 8 channels must write a matrix and arm the picker exactly as a
// stub card does, or every Gate A test of the routing panel is a test of the
// DeckLink path wearing another device's name.
//
// So the width comes from CaptureOpts.DeviceChannels, or failing that from the
// stub device list — which is the same list ListInputDevices answers from, so a
// dev build that selects a device in the dropdown gets that device's width in the
// routing panel — and the card is the ONE case that overrides both, because there
// the width is stated on the element by the description rather than discovered,
// which is a fact about the shipped string and not about the source kind.
//
// # What it deliberately does not model
//
// Anything whose failure is a property of GStreamer rather than of this
// package's decisions: negotiation, the streaming threads the real callbacks
// arrive on, the cost of a device open, the mix-matrix actually attenuating
// anything. This twin is deliberately incomplete, and anything a later agent adds
// to it should be added because a test needs it, not for symmetry.
package gst

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// The device model
// ---------------------------------------------------------------------------

// stubCaptureWidthOrigin says where a stub capture's width came from. It is the
// answer to the one question this twin exists to keep honest — "was that width a
// fact about the DEVICE, or a guess from the source kind?" — and StubCapture
// publishes it so a test can assert the answer rather than infer it.
type stubCaptureWidthOrigin = string

const (
	// widthFromCard is the constant 16 the description states on
	// decklinkaudiosrc. It is the ONLY source-kind rule in this file.
	widthFromCard stubCaptureWidthOrigin = "card"

	// widthFromEnumeration is CaptureOpts.DeviceChannels: what the provider walk
	// read out of the enumerated device's caps, which is what the application
	// will really pass.
	widthFromEnumeration stubCaptureWidthOrigin = "enumeration"

	// widthFromDeviceModel is the stub device list, looked up by id. It stands in
	// for the real twin's last resort — asking the source pad for its own fixed
	// count — and it is what makes a dev build's dropdown and its routing panel
	// agree without the application having to wire DeviceChannels first.
	widthFromDeviceModel stubCaptureWidthOrigin = "device model"

	// widthUnknown is a commentary device nothing could size. NO MATRIX IS
	// WRITTEN, exactly as the real twin writes none: a guessed width does not
	// degrade the feed, it stops the capture chain with "streaming stopped,
	// reason error (-5)", which reads as a broken device.
	widthUnknown stubCaptureWidthOrigin = "unknown"
)

// stubDeviceChannels is the device model: how many input channels the stub list
// says this endpoint presents, and whether it said anything at all.
//
// It reads the SAME list ListInputDevices answers from — including whatever a
// test installed with SetStubDevices — because the point of a device model is
// that the picker and the routing panel cannot disagree about the same device.
// A DeckLink entry is skipped rather than matched: the provider's own entry
// advertises `{ 2, 8, 16 }`, which fixes nothing, and a card commentary's width
// is the constant 16 by construction.
func stubDeviceChannels(deviceID string) (int, bool) {
	if deviceID == "" {
		return 0, false
	}
	stubMu.Lock()
	defer stubMu.Unlock()
	for _, d := range stubDevices {
		if d.ID != deviceID || NormaliseDeviceKind(d.Kind) == KindDeckLink {
			continue
		}
		return d.Channels, d.Channels > 0
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// The build-failure injector
// ---------------------------------------------------------------------------

var (
	stubCaptureMu       sync.Mutex
	stubCaptureStartErr func(CaptureLegs) error
)

// SetStubCaptureStartError makes Start fail for the leg-sets fn names — the
// stub's stand-in for a device that will not open.
//
// It is PACKAGE-LEVEL and not a method because the caller whose behaviour it
// exists to test never sees the object: the application plans its leg-sets and
// builds them itself, and the case that matters is the one PLAN.md step 7 makes
// non-negotiable — a card that will not open at launch must leave the seat coming
// up on the slate with the fault on the CAMERA lamp, not a seat that will not
// launch. A predicate rather than a bare error is what lets a test fail the card
// leg and let the slate through in the same run.
//
// It also drives the retry-without-preview: fn is consulted a second time with
// Preview cleared, so a predicate that refuses only `legs.Preview` produces
// exactly the real twin's outcome — a capture that comes up without a confidence
// monitor rather than a seat with no capture at all.
//
// Passing nil restores ordinary behaviour. Stub build only.
func SetStubCaptureStartError(fn func(CaptureLegs) error) {
	stubCaptureMu.Lock()
	defer stubCaptureMu.Unlock()
	stubCaptureStartErr = fn
}

func stubCaptureStartFailure(legs CaptureLegs) error {
	stubCaptureMu.Lock()
	fn := stubCaptureStartErr
	stubCaptureMu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(legs)
}

// stubSignalTicks lets an in-package test supply the signal watchdog's clock, so
// that a transition costs eight sends rather than two seconds of real time. Nil —
// every build that is not that test — takes the real ticker at signalPollInterval,
// which is what a dev build needs and what makes the lamp behave as it will on the
// rig.
//
// It is read under stubCaptureMu, like the failure injector beside it: both are
// package state a test writes while another test's pipeline may still be tearing
// down.
var stubSignalTicks func() (ticks <-chan time.Time, stop func())

// ---------------------------------------------------------------------------
// The pipeline
// ---------------------------------------------------------------------------

// capturedWidth is one negotiated width on its way from the pipeline to the
// application, stamped with the device it belongs to.
//
// It is declared in both twins rather than shared because capture_cgo.go is
// `cgo && !gststub` and this file is its exact complement, so the two are never
// compiled together — and the hop it exists for is the same hop in both. See
// StubCapture.widths.
type capturedWidth struct {
	deviceKey string
	channels  int
}

// StubCapture is the Gate A capture pipeline.
//
// It is exported for the reason StubPipeline is: the driving methods below —
// Fail, InjectBusError, SetStubSignal, Armings — are how a test in another
// package makes this object behave like a rig, and an unexported type would put
// all of them out of reach of the application's own suite.
type StubCapture struct {
	mu sync.Mutex

	legs      CaptureLegs
	opts      CaptureOpts
	deviceKey string

	started bool
	stopped bool

	// stopDone is closed by the Stop that actually tore this pipeline down, so a
	// concurrent second Stop can wait for it rather than returning while the
	// meters are still delivering. See Stop.
	stopDone chan struct{}

	// previewDropped records that the build succeeded only on the second
	// attempt, without the confidence monitor. The real twin logs this and
	// nothing else can see it; here it is observable, because "the picture is up
	// and the preview is not" is a state the screen has to draw.
	previewDropped bool

	claims seamClaims

	// armings counts the ArmForSend calls, because the invariant a Gate A test
	// has to be able to assert is that EVERY send session arms, not that the
	// first one did.
	armings int

	// inputChannels is the width the fake pad has negotiated, matrixWidth the
	// width the matrix in force was built for, and widthOrigin where the number
	// came from. The first two are two fields for the same reason the real twin
	// keeps two: a matrix written for a width the pad did not negotiate is the
	// measured way to stop a capture chain, and a test that cannot tell them
	// apart cannot catch it.
	inputChannels int
	matrixWidth   int
	widthOrigin   stubCaptureWidthOrigin
	channelMap    ChannelMap

	// publishedWidth and widths are the real twin's hop, kept because the hop is
	// the contract and not an implementation detail: OnInputChannels' obvious
	// implementation reads the width back, sizes the grid and applies the stored
	// routing, all of which re-enter this pipeline. Called under mu it would
	// deadlock at Gate A against code that works at Gate B, which is the one
	// disagreement between the twins nobody would look for.
	publishedWidth int
	widths         chan capturedWidth

	// muted is an ATOMIC OUTSIDE mu for the reason the real twin's is: "is the
	// microphone open" must go on being answerable while mu is held across a
	// device change, which is exactly when somebody reaches for the cough button.
	muted atomic.Bool

	// levelStop ends the synthetic-levels goroutine and levelDone is closed as it
	// exits, so Stop can JOIN it rather than merely signal it. Both nil when this
	// pipeline arms neither meter.
	levelStop chan struct{}
	levelDone chan struct{}

	// sigWatch is the REAL watchdog from signalwatch.go, polling the reading
	// below. Using the shipped debouncer rather than a fake one is what makes a
	// Gate A transition mean something: the hold-offs, the flap counter and the
	// UNKNOWN state are the ones that will run on the rig.
	sigWatch    *signalWatch
	sigTickStop func()
	signal      atomic.Int32 // a triState

	// fatal is the latched death, guarded by errMu and not by mu, because Health
	// is asked by the send side at START while mu may be held for a rebuild.
	errMu      sync.RWMutex
	fatal      error
	errs       chan error
	warns      chan string
	errsClosed bool
}

var _ CapturePipeline = (*StubCapture)(nil)

// NewCapture validates the options and returns an unstarted stub capture
// pipeline. The refusals are the real twin's, in the same order, because a
// configuration Gate A accepts must be one the card accepts.
//
// EVERY PIPELINE THIS RETURNS MUST EVENTUALLY BE STOPPED, including one whose
// Start failed and one that is abandoned unstarted, because a goroutine is parked
// on this object from here and Stop is the only thing that ends it. That is the
// real twin's rule and it is modelled rather than merely documented, so a caller
// that forgets it leaks at Gate A too — where a leak is findable.
func NewCapture(opts CaptureOpts) (CapturePipeline, error) {
	return NewStubCapture(opts)
}

// NewStubCapture is NewCapture with the concrete type, so a caller can reach the
// driving methods without a type assertion.
//
// Stub build only.
func NewStubCapture(opts CaptureOpts) (*StubCapture, error) {
	if err := opts.Legs.Valid(); err != nil {
		return nil, err
	}
	if opts.Legs.Picture == PictureSlate && opts.SlatePath == "" {
		return nil, errors.New("gst: CaptureOpts.SlatePath is required when the picture leg is " +
			"the slate")
	}
	if opts.Legs.Picture == PictureCard {
		if _, err := parseDeckLinkPersistentID(opts.VideoCaptureID); err != nil {
			return nil, fmt.Errorf("gst: CaptureOpts.VideoCaptureID names the DeckLink card whose "+
				"input becomes the picture, and it must be a DeckLink persistent-id rather than "+
				"an audio device id: %w", err)
		}
	}
	if opts.Legs.Commentary != CommentaryNone {
		if err := refuseWrongAudioSource(opts.AudioDeviceID, opts.AudioCaptureID); err != nil {
			return nil, err
		}
		if opts.Legs.Commentary == CommentaryCard && opts.Legs.Picture == PictureCard &&
			opts.VideoCaptureID != opts.AudioCaptureID {
			return nil, fmt.Errorf("gst: the picture is card %s and the commentary is card %s. A "+
				"DeckLink drives audio capture off the VIDEO clock and the card is exclusive, so "+
				"both legs must name the same card or the picture must be the slate",
				opts.VideoCaptureID, opts.AudioCaptureID)
		}
	}
	opts.ConformTo, _ = opts.ConformTo.resolve()

	c := &StubCapture{
		legs:        opts.Legs,
		opts:        opts,
		deviceKey:   opts.DeviceKey(),
		claims:      newSeamClaims(opts.Legs),
		widthOrigin: widthUnknown,
		stopDone:    make(chan struct{}),
		// The same depths gst_cgo.go's errorChannelBuffer and
		// warningChannelBuffer give the real twin, written as literals because
		// those consts live behind the cgo tag. All three channels drop when full
		// on both twins: the first fault is the diagnosis and the tenth is noise.
		errs:   make(chan error, 16),
		warns:  make(chan string, 16),
		widths: make(chan capturedWidth, 16),
	}
	// THE CARD IS SEEDED LOCKED, and every other seat unreadable. triUnknown is
	// not a pessimistic triFalse: it is "there is no element here with a signal
	// property to ask", which is every native seat and every slate leg, and it is
	// what signalWatchWanted turns into no goroutine and no lamp claim at all.
	c.signal.Store(int32(triUnknown))
	if opts.Legs.Picture == PictureCard || opts.Legs.NeedsClockCompanion() {
		c.signal.Store(int32(triTrue))
	}
	go c.publishWidths()
	return c, nil
}

func (c *StubCapture) Legs() CaptureLegs { return c.legs }

// Start models the build, in the real twin's order: the device is opened, the
// width is established, the matrix is written against it, the mute is applied,
// and only then is the pipeline running and the meters moving.
func (c *StubCapture) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		return errors.New("gst: capture pipeline is stopped")
	}
	if c.started {
		return errors.New("gst: capture pipeline already started")
	}

	// THE DEVICE OPEN, and the retry that costs the preview rather than the
	// capture. Building once more without the confidence monitor can only turn a
	// failure into a success — nothing else in the graph depends on it — and the
	// alternative is a seat with no meters and no commentary because a window
	// would not take a GL context.
	if err := stubCaptureStartFailure(c.legs); err != nil {
		if !c.legs.Preview {
			return err
		}
		withoutPreview := c.legs
		withoutPreview.Preview = false
		if err := stubCaptureStartFailure(withoutPreview); err != nil {
			return err
		}
		c.previewDropped = true
	}

	if c.legs.Commentary != CommentaryNone {
		width, origin := c.resolveWidth()
		c.widthOrigin = origin
		if width > MaxInputChannels {
			return fmt.Errorf("gst: the commentary device presents %d input channels and this "+
				"build maps at most %d. A device wider than that is refused at SELECTION time, "+
				"off air, rather than at a START that would leave a commentator waiting",
				width, MaxInputChannels)
		}
		if width > 0 {
			m := c.opts.ChannelMap
			if len(m) == 0 {
				// DefaultChannelMap is what a one-channel device needs to be heard
				// on both sides, and it is the same function that produces the
				// dual-mono the operator asked for at width 1.
				m = DefaultChannelMap(width)
			}
			// Through the SAME MixMatrix the real twin writes from, so that a map
			// Gate A accepts is a map the card accepts. The matrix is UNIFORM: it
			// is written at 1, 2, 3, 8, 16 and 32 alike, and there is no
			// source-kind test anywhere near it.
			if _, err := m.MixMatrix(width); err != nil {
				return err
			}
			c.inputChannels = width
			c.matrixWidth = width
			c.channelMap = m
			c.publishWidthLocked(width)
		}
		c.muted.Store(c.opts.MuteCommentary)
	}

	c.started = true
	c.startLevelsLocked()
	c.startSignalWatchLocked()
	return nil
}

// resolveWidth is the real twin's width order, and the order is the whole of R2:
// nothing in it asks what KIND of source this is except the first line, which is
// a fact about the shipped parse string.
//
//  1. A DeckLink commentary is the constant 16 BY CONSTRUCTION — the description
//     sets channels=16 on the element, so the number is stated, not discovered.
//  2. CaptureOpts.DeviceChannels, which the application read from the enumerated
//     device's caps during the ordinary provider walk.
//  3. The device model, standing in for the real twin's last resort of asking the
//     source pad for its own fixed count.
//
// A width that cannot be established writes NOTHING, and that is the same
// judgement the real twin makes: no matrix is better than a guessed one, because
// a wrong width does not degrade the feed, it stops the capture chain.
func (c *StubCapture) resolveWidth() (int, stubCaptureWidthOrigin) {
	switch {
	case c.legs.Commentary == CommentaryCard:
		return deckLinkAudioChannels, widthFromCard
	case c.opts.DeviceChannels > 0:
		return c.opts.DeviceChannels, widthFromEnumeration
	}
	if n, ok := stubDeviceChannels(c.opts.AudioDeviceID); ok {
		return n, widthFromDeviceModel
	}
	return 0, widthUnknown
}

// publishWidthLocked hands a negotiated width to the publisher goroutine, once
// per change. mu is held; nothing here calls the application.
func (c *StubCapture) publishWidthLocked(width int) {
	if c.opts.OnInputChannels == nil || width == c.publishedWidth {
		return
	}
	select {
	case c.widths <- capturedWidth{deviceKey: c.deviceKey, channels: width}:
		c.publishedWidth = width
	default:
	}
}

// publishWidths calls the application on its OWN goroutine, which is what makes
// OnInputChannels safe to implement the obvious way — read the width back, size
// the grid, apply the stored routing, all of which re-enter this pipeline. It
// ends when Stop closes the channel.
func (c *StubCapture) publishWidths() {
	for w := range c.widths {
		if cb := c.opts.OnInputChannels; cb != nil {
			cb(w.deviceKey, w.channels)
		}
	}
}

// ---------------------------------------------------------------------------
// The meters
// ---------------------------------------------------------------------------

// startLevelsLocked begins the synthetic meters, on the same conditions the real
// twin arms the two level elements on. mu is held.
//
// ONE GOROUTINE DRIVES BOTH, because they are two rates of the same fake clock
// and a second ticker would be a second phase for the picker's bars to drift
// against the programme meter's. A pipeline with no commentary leg starts
// nothing at all — a picture capture has no alevel and no chlevel in it — which
// matters because the application hands the same callbacks to both halves of a
// split seat and only one of them may answer.
func (c *StubCapture) startLevelsLocked() {
	if c.legs.Commentary == CommentaryNone {
		return
	}
	programme := c.opts.OnLevels
	// PLAN.md 4.3's arming condition, through the shared predicate both twins
	// call: the picker is armed when the pad presents MORE channels than the two
	// the programme meter already reports, so a stereo seat never pushes a
	// duplicate of the programme meter over the webview bridge ten times a second
	// for the whole of a ninety-minute match.
	var picker func(Levels)
	if channelMeterWanted(c.inputChannels, c.opts.OnChannelLevels != nil) {
		picker = c.opts.OnChannelLevels
	}
	if programme == nil && picker == nil {
		return
	}

	width := c.inputChannels
	stop := make(chan struct{})
	done := make(chan struct{})
	c.levelStop, c.levelDone = stop, done

	go func() {
		defer close(done)
		// The real level element's interval: 50 ms, twenty frames a second, so
		// the app-side throttle and the UI see Gate A traffic with the same shape
		// Gate B will produce.
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for step := 0; ; step++ {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			if programme != nil {
				programme(stubLevelsAt(step))
			}
			// EVERY OTHER TICK, because chlevel really does run at half the
			// programme meter's rate — 100 ms against 50 ms — and the difference
			// is deliberate. A stub that delivered both at 20 Hz would let a decay
			// animation be sized against a rate the shipped build does not
			// produce, and the picker would lag by a factor of two, which on a
			// find-the-commentator meter reads as "that channel is not the one" a
			// beat after they stopped talking.
			if picker != nil && step%2 == 0 {
				picker(stubChannelLevelsAt(step, width))
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// The signal watchdog
// ---------------------------------------------------------------------------

// startSignalWatchLocked starts the REAL watchdog over the modelled reading, on
// the real twin's condition: a caller who wants reports, and an element with a
// signal property to ask. mu is held.
//
// The watch's reader must never take mu. Stop joins the goroutine while holding
// it, so a reader that queued for it would deadlock the teardown against its own
// watchdog — which is why the reading is an atomic and not a field.
func (c *StubCapture) startSignalWatchLocked() {
	probe := c.signalReading()
	if !signalWatchWanted(c.opts.OnSignal, probe) {
		return
	}
	read := func() triState { return c.signalReading() }
	stubCaptureMu.Lock()
	clock := stubSignalTicks
	stubCaptureMu.Unlock()
	if clock == nil {
		c.sigWatch = startSignalWatch(probe, read, c.opts.OnSignal)
		return
	}
	ticks, stop := clock()
	c.sigWatch = startSignalWatchOn(ticks, probe, read, c.opts.OnSignal)
	c.sigTickStop = stop
}

// signalReading is one reading of the modelled card. It is triUnknown — "there
// was nothing here to ask" — on every leg-set that builds no decklinkvideosrc,
// whatever SetStubSignal was told, because the alternative is a stub inventing a
// lamp state out of an absence of evidence.
func (c *StubCapture) signalReading() triState {
	if c.legs.Picture != PictureCard && !c.legs.NeedsClockCompanion() {
		return triUnknown
	}
	return triState(c.signal.Load())
}

// SetStubSignal models the cable: true is a locked input, false is no lock. The
// REAL debouncer decides what the lamp says about it and when, so a transition
// costs signalLostHold exactly as it does on the rig.
//
// It has no effect on a leg-set with no decklinkvideosrc in it; see
// signalReading.
//
// Stub build only.
func (c *StubCapture) SetStubSignal(locked bool) {
	if locked {
		c.signal.Store(int32(triTrue))
		return
	}
	c.signal.Store(int32(triFalse))
}

// ---------------------------------------------------------------------------
// The seam
// ---------------------------------------------------------------------------

// ArmForSend records that the seam was armed. It refuses on a pipeline that is
// not running for the reason the real twin does: arming a graph that is not
// PLAYING is a no-op that reports success, and the whole value of arming at START
// is that it cannot be skipped.
func (c *StubCapture) ArmForSend() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return errors.New("gst: capture pipeline is stopped, so its seam cannot be armed")
	}
	if !c.started {
		return errors.New("gst: capture pipeline has not been started, so its seam cannot be armed")
	}
	// A LATCHED FAULT REFUSES THE ARMING, as it does on the real twin. On a dead
	// device the arming reports success — the IDLE probe fires immediately because
	// the pad is idle BECAUSE it is dead — and the session that follows is a green
	// lamp over silence.
	if err := c.Health(); err != nil {
		return fmt.Errorf("gst: this capture pipeline has already failed, so arming its seam would "+
			"report success over a device that is producing nothing: %w", err)
	}
	c.armings++
	return nil
}

// Armings is how many times ArmForSend has run, for the Gate A regression test
// that every send session after the first still arms.
//
// Stub build only.
func (c *StubCapture) Armings() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.armings
}

func (c *StubCapture) ProxySinks() []string { return c.claims.names() }

func (c *StubCapture) ClaimForSend() error { return c.claims.claimAll() }

func (c *StubCapture) ReleaseFromSend() { c.claims.releaseAll() }

// ---------------------------------------------------------------------------
// Routing and the mute
// ---------------------------------------------------------------------------

func (c *StubCapture) InputChannels() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped || !c.started {
		return 0
	}
	return c.inputChannels
}

func (c *StubCapture) SetChannelMap(m ChannelMap) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return errors.New("gst: capture pipeline is stopped")
	}
	if !c.started || c.legs.Commentary == CommentaryNone {
		return errors.New("gst: this capture pipeline has no commentary leg to route")
	}
	if c.inputChannels <= 0 {
		return errors.New("gst: the commentary pad has not negotiated a channel count yet, so " +
			"there is no width to size a matrix against")
	}
	// VALIDATION FIRST AND NOTHING RECORDED IF IT FAILS, because the real
	// element's own rejection is invisible — a CRITICAL on stderr and the
	// PREVIOUS matrix left in force — so a stub that stored the bad map anyway
	// would make Gate A disagree with the hardware about what is running.
	if _, err := m.MixMatrix(c.inputChannels); err != nil {
		return err
	}
	c.channelMap = m
	c.matrixWidth = c.inputChannels
	return nil
}

// ChannelMap is the map currently in force, for Gate A.
//
// Stub build only.
func (c *StubCapture) ChannelMap() ChannelMap {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.channelMap
}

// MatrixWidth is the width the matrix in force was built for, or 0 when none has
// been written.
//
// Stub build only.
func (c *StubCapture) MatrixWidth() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.matrixWidth
}

// WidthOrigin says where this pipeline's width came from: the card's constant,
// the enumeration, the device model, or nowhere. It is how a test asserts that a
// width was a fact about the DEVICE rather than a guess from the source kind,
// which is the rule this twin exists to keep.
//
// Stub build only.
func (c *StubCapture) WidthOrigin() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.widthOrigin
}

// PreviewDropped reports that the build succeeded only without the confidence
// monitor. The real twin writes this to the log and nowhere else; here it is
// observable, because "the picture is up and the preview is not" is a state the
// screen has to draw.
//
// Stub build only.
func (c *StubCapture) PreviewDropped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.previewDropped
}

func (c *StubCapture) SetCommentaryMute(mute bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return errors.New("gst: capture pipeline is stopped")
	}
	if !c.started || c.legs.Commentary == CommentaryNone {
		return errors.New("gst: this capture pipeline has no commentary leg to mute")
	}
	// IT SUCCEEDS WITH NO SESSION RUNNING (PLAN.md 0-BIS A2). The element exists
	// from launch, the mute sits upstream of alevel, and a latch set before START
	// is still set at START.
	c.muted.Store(mute)
	return nil
}

// CommentaryMuted is the read-back value, answered without taking the lock that
// is held across a device change.
func (c *StubCapture) CommentaryMuted() bool { return c.muted.Load() }

// ---------------------------------------------------------------------------
// Faults
// ---------------------------------------------------------------------------

// Health mirrors the real twin's: nil until something has latched. It is what a
// send pipeline's START asks before it builds.
func (c *StubCapture) Health() error {
	c.errMu.RLock()
	defer c.errMu.RUnlock()
	return c.fatal
}

func (c *StubCapture) Faults() <-chan error { return c.errs }

// Warnings carries the spared classes. Every line reaches this channel, which is
// the real twin's contract too — there it takes a hop through logWarnings, which
// writes the field log and forwards, precisely so that the log and the
// application are not two consumers splitting one channel between them. This twin
// has no field log to write, so the hop collapses to the delivery.
func (c *StubCapture) Warnings() <-chan string { return c.warns }

// InjectBusError delivers a synthetic bus error FROM A NAMED ELEMENT, routed by
// the same classifier the real twin's bus handler routes with.
//
// That routing is the behaviour a Gate A test has to be able to assert, because
// it is what the operator sees: a confidence monitor that dies is a warning and
// must never reach Faults(), a picture leg that dies on a split seat is a warning
// and the commentary goes on metering, and a commentary failure is fatal and
// stops the send deliberately rather than letting internal/sender spend seven
// seconds reconnecting to a network that is fine.
//
// Stub build only.
func (c *StubCapture) InjectBusError(source string, err error) {
	if err == nil {
		return
	}
	switch classifyBusError(source, captureLegs{AudioClockedByVideo: c.legs.AudioClockedByVideo()}) {
	case classPreview:
		c.deliverWarning("gst: the confidence monitor failed and the commentary and the picture " +
			"are unaffected: " + err.Error())
	case classVideoCapture:
		c.deliverWarning("gst: the picture capture failed; the commentary capture is a separate " +
			"pipeline and is still running, but if a send session is up the muxer stops entirely " +
			"until this is rebuilt: " + err.Error())
	default:
		c.Fail(err)
	}
}

// Fail latches a fatal fault and delivers it, exactly as the real twin's bus
// handler does for the commentary leg, so that Health answers afterwards.
//
// IT WRAPS ErrPipelineFatal, because every error the real twin puts on Faults()
// does — through captureFatalError on a named leg and through markFatal
// otherwise — and the application decides whether to stop the send session by
// asking exactly that question of exactly that sentinel. An already-wrapped error
// is passed through rather than wrapped twice.
//
// Stub build only.
func (c *StubCapture) Fail(err error) {
	if err == nil {
		return
	}
	if !errors.Is(err, ErrPipelineFatal) {
		err = fmt.Errorf("%w: %w (this capture pipeline has failed; the meters, the preview and "+
			"the routing it feeds are down until it is rebuilt)", ErrPipelineFatal, err)
	}
	c.errMu.Lock()
	if c.errsClosed {
		c.errMu.Unlock()
		return
	}
	if c.fatal == nil {
		c.fatal = err
	}
	select {
	case c.errs <- err:
	default:
	}
	c.errMu.Unlock()
}

func (c *StubCapture) deliverWarning(line string) {
	c.errMu.RLock()
	defer c.errMu.RUnlock()
	if c.errsClosed {
		return
	}
	select {
	case c.warns <- line:
	default:
	}
}

// ---------------------------------------------------------------------------
// Teardown
// ---------------------------------------------------------------------------

// Stop ends the meters, the watchdog and the publisher, and closes the fault
// channels. It is idempotent.
//
// The meters and the watchdog are stopped AND JOINED OUTSIDE mu, and the join
// outside the lock is the point: both call application code, and nothing that
// calls application code may be waited on under a lock the application can reach.
// What the join buys is the real twin's promise — no callback is delivered after
// Stop returns, which is what stops a frozen meter or a stale green lamp
// outliving the pipeline it described.
//
// A SECOND, CONCURRENT Stop WAITS for the first rather than returning early, so
// that promise holds for whichever caller returns first. The real twin gets that
// from holding mu for the whole teardown; this one cannot, because it has to let
// go of the lock to join goroutines that call the application, so it waits on the
// first caller's completion instead.
func (c *StubCapture) Stop() error {
	c.mu.Lock()
	if c.stopped {
		done := c.stopDone
		c.mu.Unlock()
		<-done
		return nil
	}
	// The real twin refuses to take a device to NULL underneath a bound proxysrc;
	// see cgoCapture.Stop. A configuration Gate A accepts must be one the card
	// accepts, and that includes the teardown order.
	if held := c.claims.takenNames(); len(held) > 0 {
		c.mu.Unlock()
		return fmt.Errorf("%w: %v still has a consumer, so this capture will not go to NULL. "+
			"Stop the send session first (SendSeam.Stop releases the claim and is idempotent)",
			ErrSeamBusy, held)
	}
	c.stopped = true
	stop, done := c.levelStop, c.levelDone
	c.levelStop, c.levelDone = nil, nil
	watch, tickStop := c.sigWatch, c.sigTickStop
	c.sigWatch, c.sigTickStop = nil, nil
	c.mu.Unlock()

	if stop != nil {
		close(stop)
		<-done
	}
	watch.Stop()
	if tickStop != nil {
		tickStop()
	}

	c.mu.Lock()
	c.started = false
	c.inputChannels = 0
	c.matrixWidth = 0
	c.mu.Unlock()

	c.errMu.Lock()
	if !c.errsClosed {
		c.errsClosed = true
		close(c.errs)
		close(c.warns)
		close(c.widths)
	}
	c.errMu.Unlock()
	close(c.stopDone)
	return nil
}

// StubCaptureDescription renders a description-shaped summary of what this stub
// would have built, for a Gate A test that wants to assert the SHAPE without
// asserting the parse string — which lives behind the cgo tag with the two
// platform factory names it is built from.
//
// Stub build only.
func StubCaptureDescription(legs CaptureLegs) string {
	parts := make([]string, 0, 4)
	switch legs.Picture {
	case PictureSlate:
		parts = append(parts, "slate->"+nameVideoProxySink)
	case PictureCard:
		parts = append(parts, "card->"+nameVideoProxySink)
	}
	if legs.Commentary != CommentaryNone {
		kind := "native"
		if legs.Commentary == CommentaryCard {
			kind = "card"
		}
		parts = append(parts, kind+"->"+nameAudioProxySink)
	}
	if legs.NeedsClockCompanion() {
		parts = append(parts, "clock-companion")
	}
	if legs.Preview {
		parts = append(parts, "preview")
	}
	return strings.Join(parts, " | ")
}
