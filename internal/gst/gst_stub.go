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
// While started with PipelineOpts.OnLevels set, the stub emits SYNTHETIC
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
	"sync/atomic"
	"time"
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
	opts   PipelineOpts
	sink   SinkOpts
	hasSk  bool
	errs   chan error
	closed bool

	startErr     error
	sinkErr      error
	sinkFailures int

	// fatal models the real pipeline's latched pipeline-fatal state, set by
	// MarkFatal and never cleared. Once set, every ReplaceSink returns it —
	// ahead of the FailNextSinks ladder, exactly as the real implementation
	// checks fatalError() before anything else that can fail.
	fatal error

	// inputChannels is the width the fake capture pad has "negotiated", and
	// channelMap is the map written against it. matrixWritten mirrors the real
	// twin's matrixWidth > 0 test: false means the fake device presents a
	// POSITIONED stream that audioconvert would map on its own, so there is no
	// matrix on this pipeline to change.
	//
	// The default is stubInputChannels — two, positioned, no matrix — because
	// that is what the stub's fake device list is mostly made of and what every
	// Gate A test written before this mechanism existed assumes. A test that
	// wants the DeckLink case calls SetStubInputChannels.
	inputChannels int
	channelMap    ChannelMap
	matrixWritten bool

	// levelStop ends the synthetic-levels ticker goroutine, and levelDone is
	// closed by that goroutine as it exits so Stop can JOIN it rather than
	// merely signal it. The join matters: without it a Stop-then-assert test
	// could observe one more OnLevels callback delivered after Stop returned,
	// which the real build cannot do either — busSilenced is checked on the
	// posting thread before the callback runs. Both are nil when Start was
	// asked for neither meter — ONE goroutine drives both, because they are two
	// rates of the same fake clock and a second ticker would be a second phase
	// for the picker's bars to drift against the programme meter's.
	levelStop chan struct{}
	levelDone chan struct{}

	// muted is the cough mute, and it is an ATOMIC OUTSIDE p.mu for the same
	// reason the real twin's is: CommentaryMuted is a UI-facing read that must
	// never block, and in the real build the lock it would otherwise take is
	// held for the whole of ReplaceSink. A stub whose read took the mutex would
	// let a caller be written against a cheapness the shipped build cannot
	// offer.
	//
	// It survives Stop, exactly as the real twin's does, so that a caller
	// rebuilding a session that died muted can read the state off the pipeline
	// it is discarding. See gst_cgo.go's teardownLocked.
	muted atomic.Bool

	counters StubCounters
}

// stubErrorBuffer is how many injected errors are held before further ones are
// dropped. The real implementation drops rather than blocks too: a bus callback
// must never wait on a Go consumer.
const stubErrorBuffer = 16

// stubInputChannels is the channel count a StubPipeline's fake capture pad
// negotiates unless a test says otherwise.
//
// Two, POSITIONED — the ordinary microphone case — because that is what the
// stub's device list is mostly made of and because it is the state in which no
// mix matrix is written at all. Keeping it as the default means every Gate A
// test written before the channel map existed still describes the same
// pipeline, and the DeckLink case is something a test opts into rather than
// something every test has to opt out of.
const stubInputChannels = ChannelMapOutputs

// New creates a StubPipeline. It never fails.
func New() (Pipeline, error) {
	return NewStubPipeline(), nil
}

// NewStubPipeline creates a StubPipeline with its concrete type, so that a
// caller can reach the driving methods without a type assertion.
//
// Stub build only.
func NewStubPipeline() *StubPipeline {
	return &StubPipeline{
		state:         StubStateStopped,
		errs:          make(chan error, stubErrorBuffer),
		inputChannels: stubInputChannels,
	}
}

// Start validates opts and moves the pipeline to StubStateRunning without
// installing a sink, exactly as the real implementation does.
//
// It fails if SlatePath or AudioDeviceID is empty — both are required by the
// real pipeline, and finding that out at Gate A is the point of this stub. It
// does not check that SlatePath exists on disk, because during development the
// configured path points at the installed location rather than the repository.
func (p *StubPipeline) Start(opts PipelineOpts) error {
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
	if opts.SlatePath == "" {
		return errors.New("gst: PipelineOpts.SlatePath is required")
	}
	// The identical commentary-source rule the real twin applies, through the
	// same shared helper, so the two builds cannot drift on either the rule or
	// the wrapped ErrNotACaptureDevice sentinel: exactly one of AudioDeviceID and
	// AudioCaptureID, a DeckLink id that converts, and no render endpoint. See
	// refuseWrongAudioSource in gst.go, and device_id.go for the render half and
	// its deliberate asymmetry.
	if err := refuseWrongAudioSource(opts.AudioDeviceID, opts.AudioCaptureID); err != nil {
		return err
	}
	// The identical refusal the real twin makes of a video capture id that is
	// not a DeckLink persistent-id, through the identical conversion, so that a
	// value Gate A accepts is a value the card accepts. It is the whole of what
	// a stub can check here: there is no element to open, and "would this card
	// be there" is not a question a build with no GStreamer in it can answer.
	// What it does close is the case that matters — a CoreAudio unique-id or a
	// WASAPI endpoint GUID wired into the video option by a caller that reached
	// for the wrong field, which on the real element is not an error at all but
	// a silent fall back to whichever card enumerated first.
	if opts.VideoCaptureID != "" {
		if _, err := parseDeckLinkPersistentID(opts.VideoCaptureID); err != nil {
			return fmt.Errorf("gst: PipelineOpts.VideoCaptureID names the DeckLink card whose "+
				"input becomes the video leg, and it must be a DeckLink persistent-id rather "+
				"than an audio device id: %w", err)
		}
	}
	// ONE CARD when both legs are on one, the identical refusal the real twin
	// makes and for the identical reason: a DeckLink drives audio capture off
	// the VIDEO clock, so a commentary leg on one card cannot be clocked by
	// another card's video, and the card is exclusive so no third element is
	// available to clock it. The real build discovers this as a seat that will
	// not preroll and names neither card; here it is a sentence, at Gate A,
	// before any hardware is involved.
	if opts.AudioCaptureID != "" && opts.VideoCaptureID != "" &&
		opts.AudioCaptureID != opts.VideoCaptureID {
		return fmt.Errorf("gst: PipelineOpts.VideoCaptureID is card %s and "+
			"PipelineOpts.AudioCaptureID is card %s. A DeckLink drives audio capture off the "+
			"VIDEO clock, so a commentary leg on one card cannot be clocked by another card's "+
			"video, and the card is exclusive so a third element is not available to clock it. "+
			"Both legs must name the same card, or the video leg must be the slate",
			opts.VideoCaptureID, opts.AudioCaptureID)
	}

	if opts.VideoBitrateKbps == 0 {
		opts.VideoBitrateKbps = DefaultVideoBitrateKbps
	}
	if opts.AudioBitrateBps == 0 {
		opts.AudioBitrateBps = DefaultAudioBitrateBps
	}
	// The conform target is resolved here for the same reason the two bitrates
	// are defaulted here: StartedWith() is what the Gate A tests read, and it
	// must report what a pipeline would ACTUALLY have been built with, not what
	// the caller happened to type. The resolution is the shared one in gst.go —
	// there is deliberately no second copy of the fallback rule — so the two
	// twins cannot drift on which formats are refused or on what they fall back
	// to. The real twin logs the reason; the stub has no field log to write to
	// and the resolved value in StartedWith() is the equivalent record.
	opts.ConformTo, _ = opts.ConformTo.resolve()

	// The channel map, through the SAME MixMatrix the real twin writes from, so
	// that a map Gate A accepts is a map the card accepts and a map Gate A
	// refuses is one the real build would have refused before touching the
	// element. Nothing here fakes the validation; the shared model does it.
	//
	// The discriminator is simplified and the simplification is deliberate. The
	// real twin asks the capture source's pad whether it can produce a stereo
	// pair unaided; a stub has no pad to ask, so it uses the condition that
	// question separates in practice — a source presenting MORE channels than
	// the pipeline's two has no way to say what they are, and is the case a
	// matrix exists for. Two channels or one behave as they always did: no
	// matrix, and SetChannelMap refuses with the same message the real twin
	// gives for a positioned device.
	// A DECKLINK COMMENTARY SEAT NEGOTIATES SIXTEEN, and the stub says so rather
	// than leaving the fake pad at its stereo default. decklinkaudiosrc is built
	// with channels=16 and there is no configuration in which it presents a pair,
	// so a Gate A test that started a DeckLink seat and got two channels would be
	// exercising a shape the shipped build cannot produce — no matrix written, no
	// per-channel meter armed, and a routing UI able to depend on both.
	//
	// A caller that has ALREADY widened the pad keeps its own number: 8 is a real
	// card setting and a test that asked for it means it. Only the stereo default
	// — the one value a DeckLink audio seat can never have — is replaced.
	if opts.AudioCaptureID != "" && p.inputChannels <= ChannelMapOutputs {
		p.inputChannels = deckLinkAudioChannels
	}

	if p.inputChannels > ChannelMapOutputs {
		if _, err := opts.ChannelMap.MixMatrix(p.inputChannels); err != nil {
			return err
		}
		p.channelMap = opts.ChannelMap
		p.matrixWritten = true
	}

	// The cough mute, applied at exactly the point the real twin applies it: with
	// the pipeline still notionally in NULL, before it can be said to be
	// carrying anything. The stub has no element to write, so the "read back
	// what the element said" discipline collapses to storing the value — but the
	// STATE MACHINE around it is the same one, and that is what Gate A is for. A
	// pipeline started with MuteCommentary reports CommentaryMuted immediately,
	// and never through a route that Start could have raced.
	p.muted.Store(opts.MuteCommentary)

	p.opts = opts
	p.state = StubStateRunning

	// The synthetic input meters. Only when the caller asked for metering:
	// nil OnLevels means no goroutine, exactly as the real build installs no
	// callback. The goroutine owns nothing but its own step counter and the
	// two channels, so it never takes p.mu — which is what lets Stop join it
	// without holding the lock, and what makes a callback that (wrongly)
	// blocks unable to deadlock anything except the Stop that must wait for
	// it, mirroring the real contract's "MUST NOT BLOCK".
	//
	// The PER-CHANNEL picker rides the same goroutine, on the same conditions the
	// real twin arms chlevel on: a callback to deliver to, AND a matrix having
	// been written. That second condition is the parity that matters. In the real
	// build chlevel is built with post-messages=false and armed only for an
	// unpositioned source, so a seat with an ordinary microphone posts not one
	// per-channel frame; a stub that produced them anyway would let a UI depend
	// on a stream the shipped build does not send.
	//
	// The width is p.inputChannels rather than a constant, so the fake frames are
	// as wide as the fake pad negotiated — sixteen for a test that called
	// SetStubInputChannels(16), which is the DeckLink shape the picker exists for.
	if opts.OnLevels != nil || (opts.OnChannelLevels != nil && p.matrixWritten) {
		programme := opts.OnLevels
		channels := opts.OnChannelLevels
		width := p.inputChannels
		if !p.matrixWritten {
			channels = nil
		}
		stop := make(chan struct{})
		done := make(chan struct{})
		p.levelStop = stop
		p.levelDone = done
		go func() {
			defer close(done)
			// The real level element's interval: 50 ms, twenty frames a
			// second, so the app-side throttle and the UI see Gate A traffic
			// with the same shape Gate B will produce.
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			step := 0
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					if programme != nil {
						programme(stubLevelsAt(step))
					}
					// EVERY OTHER TICK, because chlevel really does run at half
					// the programme meter's rate (channelLevelIntervalNs, 100 ms
					// against 50 ms) and the difference is deliberate. A stub
					// that delivered both at 20 Hz would let a decay animation or
					// a smoothing window be sized against a rate the shipped
					// build does not produce, and the picker would lag by a
					// factor of two — which on a find-the-commentator meter reads
					// as "that channel is not the one" a beat after they stopped
					// talking.
					if channels != nil && step%2 == 0 {
						channels(stubChannelLevelsAt(step, width))
					}
					step++
				}
			}
		}()
	}
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

// InputChannels reports the channel count the fake capture pad has negotiated,
// or 0 when the pipeline is not running.
//
// The zero-when-stopped answer is contract rather than convenience: the real
// twin reads the pad's CURRENT caps, and a pad that has gone to NULL has none,
// so a caller that treats 0 as "no device" behaves identically at Gate A and at
// Gate B.
func (p *StubPipeline) InputChannels() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.state == StubStateStopped {
		return 0
	}
	return p.inputChannels
}

// SetChannelMap validates a map against the negotiated width and records it,
// exactly as the real twin validates before writing.
//
// The refusals are the same refusals, produced by the same MixMatrix, and the
// order matters as much here as it does there: NOTHING IS RECORDED when
// validation fails, because the real element leaves its previous matrix in
// force on a rejected write and a stub that stored the bad map anyway would
// make Gate A disagree with the hardware about what is running.
func (p *StubPipeline) SetChannelMap(m ChannelMap) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed || p.state == StubStateStopped {
		return errStubStopped
	}
	if !p.matrixWritten {
		return fmt.Errorf("%w: the capture device presents a positioned stream, which audioconvert "+
			"maps to stereo on its own; there is no channel map on this pipeline to change. A "+
			"channel map applies to a device that presents unpositioned channels, such as a "+
			"DeckLink card's sixteen", ErrChannelMap)
	}
	if _, err := m.MixMatrix(p.inputChannels); err != nil {
		return err
	}
	p.channelMap = m
	return nil
}

// SetStubInputChannels sets the channel count the fake capture pad negotiates
// on the NEXT Start. Passing 0 restores stubInputChannels.
//
// It is how a Gate A test reaches the DeckLink case: sixteen channels, a matrix
// written at Start, and SetChannelMap live afterwards. It has no effect on a
// pipeline that is already running, because a real pad does not renegotiate its
// channel count under a running feed either — decklinkaudiosrc's channels
// property is not live-settable, which is exactly why sixteen has to be the
// steady state once mapping exists.
//
// Stub build only.
func (p *StubPipeline) SetStubInputChannels(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n <= 0 {
		n = stubInputChannels
	}
	p.inputChannels = n
}

// ChannelMap returns the map currently recorded, and whether a matrix was
// written at all. It is the stub's equivalent of reading the element's
// property, which the real build cannot do — GST_TYPE_ARRAY does not marshal
// back — and is what lets a Gate A test assert that a refused SetChannelMap
// left the previous map in place.
//
// Stub build only.
func (p *StubPipeline) ChannelMap() (ChannelMap, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.channelMap, p.matrixWritten
}

// SetCommentaryMute mutes the commentary on the send path, or unmutes it, with
// the identical refusals the real twin makes: stopped, and not started.
//
// The pre-Start refusal is the one that matters at Gate A. It is not defensive
// tidiness — it is the design decision coughmute.go argues, that the mute has
// exactly one route before Start (PipelineOpts.MuteCommentary) and exactly one
// after it, so that no two places can hold disagreeing memories of whether the
// microphone is open. A stub that quietly accepted the call and latched it would
// let a caller be written against a second route the real build does not have.
func (p *StubPipeline) SetCommentaryMute(mute bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.counters.CommentaryMutes++

	if p.closed {
		return errStubStopped
	}
	if p.state == StubStateStopped {
		return errors.New("gst: pipeline has not been started, so there is no element to mute; " +
			"a pipeline that must begin muted is started with PipelineOpts.MuteCommentary, which " +
			"is applied before it can produce a buffer")
	}
	p.muted.Store(mute)
	return nil
}

// CommentaryMuted reports the mute state, without taking p.mu. See the field.
func (p *StubPipeline) CommentaryMuted() bool {
	return p.muted.Load()
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
	stop, done := p.levelStop, p.levelDone
	p.levelStop, p.levelDone = nil, nil
	p.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.counters.Stops++
	p.state = StubStateStopped
	p.hasSk = false
	// The matrix goes with the pipeline, as it does in the real twin where
	// teardownLocked drops the element and zeroes matrixWidth. The recorded map
	// itself stays readable, so a test can assert what was in force when the
	// pipeline stopped.
	p.matrixWritten = false
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
func (p *StubPipeline) StartedWith() PipelineOpts {
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
