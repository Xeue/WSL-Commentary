//go:build !cgo || gststub

// This file is the pure-Go stub twin. It compiles with CGO_ENABLED=0 and needs
// no MinGW, no GStreamer and no audio hardware — that is Gate A.
//
// Unlike everything else WP-0 wrote, this file is REAL and is meant to work. It
// is what lets internal/sender, the Wails layer and the frontend be built, run
// and demonstrated before the build host exists. Its behaviour deliberately
// mirrors the contract in gst.go: Start installs no sink, ReplaceSink reports
// connection failure synchronously, Errors carries asynchronous failure and is
// closed by Stop.
//
// It is owned by WP-3a along with the rest of this package, but WP-3b and WP-8
// depend on it behaving exactly as documented, so changes to its semantics are a
// contract change and must be reported, not made quietly.
//
// # What this stub does and does not model about the loopback defect
//
// The stub does NOT model wasapi2's loopback republication of playback
// endpoints — ListInputDevices here returns capture devices only and there is
// no fake loopback entry to filter. What it proves is the REFUSAL, not the
// ENUMERATION filter: Start applies the identical render-endpoint refusal the
// real twin does, through the shared refuseRenderEndpoint in device_id.go, so
// a caller that would hand a playback endpoint to the real pipeline fails the
// same way at Gate A. Correspondingly, the endpoint-id namespaces of the fake
// device lists are CONTRACT: every NATIVE defaultStubDevices id is in the
// capture namespace ({0.0.1.00000000}.) and every defaultStubOutputDevices id
// (in return_stub.go) is in the render namespace ({0.0.0.00000000}.), and
// gst_stub_test.go asserts both against the classifier in device_id.go. The
// DeckLink entry is exempt from the first clause and not from the second: a
// persistent-id belongs to neither namespace on purpose, but nothing in the
// INPUT list may ever classify as a render endpoint.
//
// # This stub is platform-neutral, and there are things it therefore cannot model
//
// It has no build tag beyond !cgo, it runs at Gate A on Windows and on macOS
// alike, and it stays that way deliberately: it exists to let internal/sender
// and the Wails layer be exercised without a toolchain, and giving it a GOOS
// split would put platform knowledge in the one file in this package that has
// none. So the fake device ids stay WINDOWS-SHAPED on every host. That is not
// an oversight — those ids are what makes the render-endpoint refusal testable
// at all, and macOS-shaped fixtures would exercise a classifier that is inert
// on them (device_id.go's "On macOS" section says why that is the right
// answer). SetStubDevices takes any ids a test wants, including CoreAudio ones,
// for the cases that care.
//
// What is consequently NOT covered at Gate A, and must be held in mind when
// reading a green test run on a Mac:
//
//   - The macOS unique-id → AudioDeviceID resolution. That is the single most
//     important structural difference in the port, it is the difference between
//     capturing the operator's chosen microphone and silently capturing the
//     system default, and NOTHING in this file or its tests touches it. It is
//     covered by a source guard over deviceprovider_darwin.go
//     (TestDarwinCaptureIDsAreResolvedNotPersisted) and by nothing else until
//     Gate B.
//   - Which capture source and AAC encoder the real pipeline actually uses.
//     Same answer: a source guard (TestPlatformElementContractIsPinned), because
//     a stub has no encoder to be wrong about.
//
// # What this stub models about the input meters
//
// While started with CaptureOpts.OnLevels set, the stub emits SYNTHETIC
// levels on a 50 ms ticker — the real level element's interval — as a
// deterministic triangle wave, -40 up to -6 dBFS and back over six seconds,
// with the right channel a quarter-period behind the left (levels.go,
// stubLevelsAt). That is enough for the whole UI path — event pump, throttle,
// meters — to be developed and watched moving at Gate A. What it does NOT
// model: any relationship to real audio (there is none to measure), the bus
// threading of the real build (the ticker is an ordinary goroutine, where the
// real callback arrives on a GStreamer streaming thread), message loss under
// load, or per-channel differences beyond the fixed phase offset. The ticker
// stops at Stop, before the error channel closes, and a nil OnLevels starts no
// goroutine at all.

package gst

import (
	"errors"
	"fmt"
	"sync"
)

// errStubStopped is returned by methods called on a stopped pipeline.
var errStubStopped = errors.New("gst: pipeline is stopped")

// StubState is the state of a StubPipeline. It is not a GStreamer state; it is
// the minimum needed to model the contract's three-way distinction between not
// started, started without a sink, and connected.
type StubState string

const (
	// StubStateStopped is the state before Start and after Stop.
	StubStateStopped StubState = "STOPPED"

	// StubStateRunning is the state after Start: capture and encoding are
	// notionally live, no sink is installed.
	StubStateRunning StubState = "RUNNING"

	// StubStateSinkAttached is the state after a successful ReplaceSink: a sink
	// is installed and media is notionally flowing.
	StubStateSinkAttached StubState = "SINK_ATTACHED"
)

// defaultStubDevices is the fake capture device list. The first entry reproduces
// the real double space in the Dante Virtual Soundcard display name, because a
// caller that mishandles it should fail at Gate A rather than at the facility.
//
// Every ID being in the CAPTURE namespace ({0.0.1.00000000}.) is contract, not
// decoration: these must pass IsCaptureEndpointID so that a Gate A caller
// wiring the dropdown to Start never trips the render-endpoint refusal on the
// stub's own data. gst_stub_test.go asserts it.
//
// They stay Windows-shaped when Gate A runs on a Mac. See the file comment: a
// CoreAudio unique-id classifies as neither namespace, so a macOS-shaped
// fixture here would silently stop testing the refusal these ids exist to test.
// The last two entries reproduce the UltraStudio TWIN PAIR: one card
// enumerating twice, once through the platform's own audio stack and once
// through GStreamer's decklink provider, under names an operator cannot tell
// apart. That is the measured shape of the owner's original bug — the native
// twin reads -96 dBFS on all sixteen channels with the mic live, and it is the
// one the dropdown used to offer — and without it in the stub NOTHING at Gate A
// ever exercises the labelling that exists to separate them. frontend's
// backend.js FAKE_DEVICES documents itself as mirroring this table and carries
// the same pair; the two must not drift.
//
// THE TWO NAMES ARE IDENTICAL ON PURPOSE. That is what makes it the collision:
// labelDevices adds the "computer sound input" suffix to a native entry only
// when a DeckLink entry SHARES its name, so a fixture whose twins were named
// differently would add no suffix at all and leave the labelling untested by
// the very case it was written for. 2747401380 is the real persistent-id
// measured off the card.
//
// The DeckLink entry's ID is that bare persistent-id and deliberately NOT in
// the endpoint namespace, because that is what the real provider publishes — a
// gint64 rendered as decimal, with no prefix and no braces. It is the one entry
// here that is not a WASAPI-shaped id, and it is the reason
// IsCaptureEndpointID's contract is a POSITIVE identification rather than a
// refusal of everything unrecognised: an id that classifies as neither
// namespace must not become an unstartable device. frontend's backend.js
// carries the same pair with the same ids.
// Channels is filled in here for the same reason it exists at all: the routing
// panel is sized from what the ENUMERATION advertised, so a stub population in
// which every device is silent about its width can only ever exercise the stereo
// seat. The DeckLink entry advertises 0 on purpose: its provider offers
// `{ 2, 8, 16 }`, which fixes nothing, and a card commentary's width is the
// constant 16 by construction rather than by advertisement.
//
// # The list spans 1, 2, 3, 8, 16 and 32 channels, and that is the requirement
//
// The routing panel appears at EVERY width the pad negotiates — the operator
// overruled a proposed `width > 2` gate on 2026-08-16: "I think we always show
// it. You may want to flip the channels on a stereo source, on a mono you may
// want to route it to be dual mono etc". A grid that has to READ WELL at 1 and 2
// as well as work at 32 cannot be developed against a device list that is three
// stereo entries and a card, so every width the panel must draw has a device here
// to draw it from. StubCapture resolves an unstated width out of this same list,
// so selecting one of these in a dev build sizes the panel to it.
//
// The widths are the shapes that exist on real desks rather than a sweep: a
// headset microphone is mono, a 2i2 is a stereo pair, an aggregate of a mic and a
// loopback is the measured 3-channel CoreAudio case, an 18i20's analogue bank is
// 8, the UltraStudio through CoreAudio is the measured 16, and a MADI interface
// is the 32 that MaxInputChannels was raised to cover.
var defaultStubDevices = []Device{
	{
		ID:       "{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}",
		Name:     "DVS Receive  1-2 (Dante Virtual Soundcard)",
		Kind:     KindNative,
		Channels: 2,
	},
	{
		ID:       "{0.0.1.00000000}.{c41a9d7e-0004-438e-9003-51a46e13a0c1}",
		Name:     "DVS Receive  3-4 (Dante Virtual Soundcard)",
		Kind:     KindNative,
		Channels: 2,
	},
	{
		ID:       "{0.0.1.00000000}.{9f6d2b18-0004-438e-9003-51a46e13a4d5}",
		Name:     "Microphone (Focusrite Scarlett 2i2 USB)",
		Kind:     KindNative,
		Channels: 2,
	},
	{
		// THE MONO CASE the operator named: one input, routed to both sides.
		// DefaultChannelMap already produces exactly that, and the grid it draws
		// is a 2x1 that has to say so in words rather than merely function.
		ID:       "{0.0.1.00000000}.{7c2d4e91-0004-438e-9003-51a46e13c2f4}",
		Name:     "Headset Microphone (Poly Blackwire 3220)",
		Kind:     KindNative,
		Channels: 1,
	},
	{
		// THE MEASURED THREE. A real 3-channel CoreAudio device on this machine
		// negotiated channels=3 channel-mask=0x0 — unpositioned, matrix
		// mandatory — which is the case that proves "unpositioned" is not a
		// synonym for "DeckLink".
		ID:       "{0.0.1.00000000}.{a8b31f60-0004-438e-9003-51a46e13d5a7}",
		Name:     "Aggregate Device (Mic + Loopback)",
		Kind:     KindNative,
		Channels: 3,
	},
	{
		// A NON-SQUARE GRID, and the first place anyone will get the matrix
		// orientation wrong: 2 outputs by 8 inputs, where a transpose is no
		// longer invisible the way it is at 2x2.
		ID:       "{0.0.1.00000000}.{d5e02a44-0004-438e-9003-51a46e13e8b2}",
		Name:     "Analogue 1-8 (Focusrite Scarlett 18i20)",
		Kind:     KindNative,
		Channels: 8,
	},
	{
		ID:       "{0.0.1.00000000}.{4b1e77a2-0004-438e-9003-51a46e13b7e0}",
		Name:     "Blackmagic UltraStudio 4K Mini",
		Kind:     KindNative,
		Channels: 16,
	},
	{
		// THE CEILING. MaxInputChannels is 32 and a 2x32 mix-matrix is measured
		// passing audio with level reporting 32 rms entries per message, so the
		// widest seat this build accepts is one a Gate A test can select rather
		// than one nobody can reach.
		ID:       "{0.0.1.00000000}.{f10c9b73-0004-438e-9003-51a46e13f9c5}",
		Name:     "MADI 1-32 (RME MADIface USB)",
		Kind:     KindNative,
		Channels: 32,
	},
	{
		ID:   "2747401380",
		Name: "Blackmagic UltraStudio 4K Mini",
		Kind: KindDeckLink,
	},
}

var (
	stubMu      sync.Mutex
	stubDevices = append([]Device(nil), defaultStubDevices...)
	stubDevErr  error
	stubAppDir  string
	stubInited  bool
)

// Init records appDir and marks GStreamer as initialised. In the stub build
// there is nothing to initialise, so it never fails and is idempotent.
func Init(appDir string) error {
	stubMu.Lock()
	defer stubMu.Unlock()
	stubAppDir = appDir
	stubInited = true
	return nil
}

// ListInputDevices returns the fake capture device list. Init need not have been
// called; the stub is usable from a bare unit test.
func ListInputDevices() ([]Device, error) {
	stubMu.Lock()
	defer stubMu.Unlock()
	if stubDevErr != nil {
		return nil, stubDevErr
	}
	// Normalised on the way out, exactly as the cgo build does, so that
	// Device.Kind ALWAYS crosses the Wails boundary as one of the two spellings
	// on both builds. Without it a test that injects a Kind-less device through
	// SetStubDevices would exercise a frame shape the real build cannot produce
	// — and the frontend's one dropdown groups on this field, so an entry with
	// no kind lands in neither optgroup and vanishes from the only control that
	// could select it. A dev build that can produce a device the release build
	// cannot is a dev build that hides exactly this.
	out := append([]Device(nil), stubDevices...)
	for i := range out {
		out[i].Kind = NormaliseDeviceKind(out[i].Kind)
	}
	return out, nil
}

// SetStubDevices replaces the fake device list returned by ListInputDevices.
// Passing nil restores the default three devices. It exists so that WP-5b and
// WP-8 can exercise the empty-list and single-device cases in the dropdown.
//
// Stub build only.
func SetStubDevices(devices []Device) {
	stubMu.Lock()
	defer stubMu.Unlock()
	if devices == nil {
		stubDevices = append([]Device(nil), defaultStubDevices...)
		return
	}
	stubDevices = append([]Device(nil), devices...)
}

// SetStubDeviceError makes ListInputDevices fail with err. Passing nil restores
// normal behaviour.
//
// Stub build only.
func SetStubDeviceError(err error) {
	stubMu.Lock()
	defer stubMu.Unlock()
	stubDevErr = err
}

// StubAppDir returns the directory last passed to Init, for tests that want to
// assert the bundling path was computed.
//
// Stub build only.
func StubAppDir() (dir string, inited bool) {
	stubMu.Lock()
	defer stubMu.Unlock()
	return stubAppDir, stubInited
}

// StubCounters counts the calls a StubPipeline has received. It lets a test
// assert, for example, that a ForceKeyUnit followed every successful
// ReplaceSink.
//
// Stub build only.
type StubCounters struct {
	// Starts is the number of Start calls, successful or not.
	Starts int
	// ReplaceSinks is the number of ReplaceSink calls, successful or not.
	ReplaceSinks int
	// SinkRemovals is the number of RemoveSink calls, including the idempotent
	// no-op ones. A reconnect cycle that honours specification section 6.2
	// increments this once on entry to DRAINING, before the backoff wait.
	SinkRemovals int
	// SinksAttached is the number of ReplaceSink calls that succeeded.
	SinksAttached int
	// ForceKeyUnits is the number of successful ForceKeyUnit calls.
	ForceKeyUnits int
	// Stops is the number of Stop calls, including the idempotent repeats.
	Stops int
	// CommentaryMutes is the number of SetCommentaryMute calls, successful or
	// not — including the refused ones, because a UI that calls it on a pipeline
	// that has not started is a bug worth being able to count at Gate A rather
	// than one that shows up as a cough button doing nothing.
	CommentaryMutes int
}

// StubPipeline is the pure-Go Pipeline returned by New in a non-cgo build.
//
// Beyond the Pipeline interface it exposes methods for driving its transitions
// programmatically — FailNextStart, FailNextSinks, InjectError — so that the
// reconnect state machine in internal/sender and the wire-up in main.go can be
// exercised at Gate A. All of its methods are safe for concurrent use.
//
// Stub build only.
type StubPipeline struct {
	mu     sync.Mutex
	state  StubState
	opts   SendOpts
	sink   SinkOpts
	hasSk  bool
	errs   chan error
	closed bool

	// set is the capture layer this send pipeline was minted from, and seam is
	// this session's claim on it. They are the whole of what the stub models
	// about the seam, and modelling them is the point: the two failures they
	// prevent — a second consumer stealing the stream, and a session that never
	// re-armed — are both SILENT on the real element, so Gate A is the only place
	// the refusals themselves can be exercised without a card.
	set  CaptureSet
	seam *SendSeam

	startErr     error
	sinkErr      error
	sinkFailures int

	// fatal models the real pipeline's latched pipeline-fatal state, set by
	// MarkFatal and never cleared. Once set, every ReplaceSink returns it —
	// ahead of the FailNextSinks ladder, exactly as the real implementation
	// checks fatalError() before anything else that can fail.
	fatal error

	// THE FAKE CAPTURE PAD, THE SYNTHETIC METERS AND THE MUTE ARE NOT HERE.
	// StubCapture has all three, because the pipeline that owns the device is what
	// owns them, and a Gate A test that wants a negotiated width or a moving meter
	// now gets one with no send pipeline anywhere in the process — which is the
	// behaviour it is supposed to be checking.

	counters StubCounters
}

// stubErrorBuffer is how many injected errors are held before further ones are
// dropped. The real implementation drops rather than blocks too: a bus callback
// must never wait on a Go consumer.
const stubErrorBuffer = 16

// New mints a stub send pipeline over a capture set, with the identical refusal
// of an empty one the real twin makes and for the identical reason: a send
// pipeline with no capture behind it does not fail at runtime, it reaches PLAYING
// and carries zero bytes with every lamp green.
func New(set CaptureSet) (Pipeline, error) {
	if len(set.Pipelines()) == 0 {
		return nil, errors.New("gst: New was given an empty capture set. The send description is " +
			"invariant — it always has a picture leg and a commentary leg — so a set with neither " +
			"is a planning error, and a send pipeline built from it would reach PLAYING attached " +
			"to nothing and carry zero bytes with SRT connected and every lamp green")
	}
	return NewStubPipeline(set), nil
}

// NewStubPipeline creates a StubPipeline with its concrete type, so that a
// caller can reach the driving methods without a type assertion. It does NOT
// apply New's empty-set refusal, so that a test can build one deliberately.
//
// Stub build only.
func NewStubPipeline(set CaptureSet) *StubPipeline {
	return &StubPipeline{
		state: StubStateStopped,
		errs:  make(chan error, stubErrorBuffer),
		set:   set,
	}
}

// Start claims and arms the capture seam, then moves the pipeline to
// StubStateRunning without installing a sink, exactly as the real twin does.
//
// THE CLAIM AND THE ARMING ARE THE PART WORTH MODELLING. Everything else a stub
// Start can check is a string the real build also checks; these two are behaviour
// that has no symptom on the real element at all. A second consumer attaching to
// a live proxysink silently steals the stream and kills the first, and a session
// that skipped the arming carries zero bytes with SRT connected — so the refusal
// and the re-arm are exercised HERE, at Gate A, on every seat shape, where a
// missing one is a failing test rather than a silent match.
//
// It takes them in NewSend's order — claim first, arm second, release everything
// on either failure — because that is the object both twins share.
func (p *StubPipeline) Start(opts SendOpts) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.counters.Starts++

	if p.closed {
		return errStubStopped
	}
	if p.state != StubStateStopped {
		return errors.New("gst: pipeline already started")
	}
	if err := p.startErr; err != nil {
		p.startErr = nil
		return err
	}

	if opts.VideoBitrateKbps == 0 {
		opts.VideoBitrateKbps = DefaultVideoBitrateKbps
	}
	if opts.AudioBitrateBps == 0 {
		opts.AudioBitrateBps = DefaultAudioBitrateBps
	}
	if opts.VideoBitrateKbps < 0 || opts.AudioBitrateBps < 0 {
		return fmt.Errorf("gst: negative bitrate: video %d kbps, audio %d bps",
			opts.VideoBitrateKbps, opts.AudioBitrateBps)
	}

	seam, err := NewSend(p.set)
	if err != nil {
		return err
	}
	p.seam = seam

	p.opts = opts
	p.state = StubStateRunning
	return nil
}

// ReplaceSink installs a fake sink. It fails, without changing state, while
// FailNextSinks has failures left to hand out — which is how a caller drives the
// CONNECTING to BACKOFF edge of the reconnect state machine.
func (p *StubPipeline) ReplaceSink(opts SinkOpts) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.counters.ReplaceSinks++

	if p.closed || p.state == StubStateStopped {
		return errStubStopped
	}
	// The latch outranks the FailNextSinks ladder, mirroring the real
	// implementation's fatalError() check on entry: once the (notional)
	// capture or mux chain has failed, no reconnect can carry media, so every
	// queued "connection failure" would be a lie about what is wrong.
	if p.fatal != nil {
		return p.fatal
	}
	if p.sinkFailures > 0 {
		p.sinkFailures--
		err := p.sinkErr
		if p.sinkFailures == 0 {
			p.sinkErr = nil
		}
		return err
	}
	if opts.Host == "" || opts.Port == 0 {
		return errors.New("gst: SinkOpts.Host and SinkOpts.Port are required")
	}

	if opts.LatencyMs == 0 {
		opts.LatencyMs = DefaultSRTLatencyMs
	}

	p.sink = opts
	p.hasSk = true
	p.state = StubStateSinkAttached
	p.counters.SinksAttached++
	return nil
}

// RemoveSink detaches the fake sink without installing another, leaving the
// pipeline running. It is idempotent: removing when nothing is attached is not
// an error, which is what lets the reconnect loop call it unconditionally on
// entry to DRAINING.
func (p *StubPipeline) RemoveSink() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.counters.SinkRemovals++

	if p.closed || p.state == StubStateStopped {
		return errStubStopped
	}
	if !p.hasSk {
		return nil
	}

	p.sink = SinkOpts{}
	p.hasSk = false
	p.state = StubStateRunning
	return nil
}

// ForceKeyUnit records the request. It fails only on a pipeline that is not
// running, matching the real implementation, where the event is sent upstream of
// the sink and so does not require one.
func (p *StubPipeline) ForceKeyUnit() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed || p.state == StubStateStopped {
		return errStubStopped
	}
	p.counters.ForceKeyUnits++
	return nil
}

// Errors returns the asynchronous error channel. It is closed by Stop.
func (p *StubPipeline) Errors() <-chan error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.errs
}

// Stop moves the pipeline to StubStateStopped and closes the error channel. It
// is idempotent.
//
// The levels ticker is stopped AND JOINED first, outside the lock. The join is
// what guarantees no OnLevels callback is delivered after Stop returns — a
// promise the real build keeps through busSilenced — and it happens without
// p.mu held because the ticker goroutine calls application code (the callback)
// and nothing that calls application code may run under a lock the application
// can reach. Concurrent Stops are safe: the first takes the channels, the rest
// find nil.
func (p *StubPipeline) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.counters.Stops++
	p.state = StubStateStopped
	p.hasSk = false

	// THE SEAM IS RELEASED HERE AND THE CAPTURE IS NOT TOUCHED. The real twin
	// releases it only after its pipeline has reached NULL, because a proxysink
	// whose old consumer is still alive cannot be re-armed; a stub has no state
	// change to sequence against, so what it models is the release itself — which
	// is what lets the NEXT Start claim the seam, and what a Gate A test asserting
	// three START/STOP cycles is actually asserting.
	p.seam.Stop()
	p.seam = nil

	if !p.closed {
		p.closed = true
		close(p.errs)
	}
	return nil
}

// InjectError delivers err on the channel returned by Errors, simulating a
// GST_ELEMENT_ERROR from srtout — the loss of the SRT peer that drives
// CONNECTED to DRAINING.
//
// It never blocks: if the buffer is full or the pipeline has been stopped, the
// error is dropped and InjectError reports false.
//
// Stub build only.
func (p *StubPipeline) InjectError(err error) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return false
	}
	select {
	case p.errs <- err:
		return true
	default:
		return false
	}
}

// MarkFatal models the real pipeline latching a pipeline-fatal error — a bus
// error whose source is the capture or mux chain rather than the sink. It
// wraps err in ErrPipelineFatal exactly as onBusMessage's markFatal site does,
// keeping the same human-readable tail, so a Gate A test matching either the
// sentinel or the "pipeline-fatal" substring is also a statement about the
// real build.
//
// The latch behaves as the contract in gst.go documents: once marked, every
// subsequent ReplaceSink returns the error — ahead of the FailNextSinks
// ladder — and it NEVER clears; recovery is Stop, New, Start on a fresh
// pipeline. Only the first mark wins, matching the real markFatal. Start,
// RemoveSink and ForceKeyUnit are untouched, because the real fatal path
// gates only ReplaceSink's promise of a working connection.
//
// Stub build only.
func (p *StubPipeline) MarkFatal(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fatal != nil || err == nil {
		return
	}
	p.fatal = fmt.Errorf("%w: %w "+
		"(the capture or mux chain has failed; recover with Stop, New, Start)",
		ErrPipelineFatal, err)
}

// Fatal returns the latched pipeline-fatal error, or nil. It is the stub's
// fatalError.
//
// Stub build only.
func (p *StubPipeline) Fatal() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fatal
}

// FailNextStart makes the next Start call return err instead of starting. Only
// one failure is queued; passing nil cancels it.
//
// Stub build only.
func (p *StubPipeline) FailNextStart(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.startErr = err
}

// FailNextSinks makes the next n ReplaceSink calls return err without changing
// state. Passing n <= 0 cancels any queued failures.
//
// This is how the backoff ladder is tested: queue five failures, watch the
// intervals, then let the sixth attempt succeed.
//
// Stub build only.
func (p *StubPipeline) FailNextSinks(n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n <= 0 {
		p.sinkFailures = 0
		p.sinkErr = nil
		return
	}
	if err == nil {
		err = fmt.Errorf("gst: stub SRT connect failed")
	}
	p.sinkFailures = n
	p.sinkErr = err
}

// State returns the pipeline's current stub state.
//
// Stub build only.
func (p *StubPipeline) State() StubState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// StartedWith returns the options the pipeline was started with, after default
// substitution.
//
// Stub build only.
func (p *StubPipeline) StartedWith() SendOpts {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opts
}

// AttachedSink returns the options of the currently installed sink, and whether
// there is one.
//
// Stub build only.
func (p *StubPipeline) AttachedSink() (SinkOpts, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sink, p.hasSk
}

// Counters returns a snapshot of the call counts.
//
// Stub build only.
func (p *StubPipeline) Counters() StubCounters {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counters
}

// Compile-time assertion that the stub satisfies the contract.
var _ Pipeline = (*StubPipeline)(nil)
