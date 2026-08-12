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
// device lists are CONTRACT: every defaultStubDevices id is in the capture
// namespace ({0.0.1.00000000}.) and every defaultStubOutputDevices id (in
// return_stub.go) is in the render namespace ({0.0.0.00000000}.), and
// gst_stub_test.go asserts both against the classifier in device_id.go.
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
var defaultStubDevices = []Device{
	{
		ID:   "{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}",
		Name: "DVS Receive  1-2 (Dante Virtual Soundcard)",
	},
	{
		ID:   "{0.0.1.00000000}.{c41a9d7e-0004-438e-9003-51a46e13a0c1}",
		Name: "DVS Receive  3-4 (Dante Virtual Soundcard)",
	},
	{
		ID:   "{0.0.1.00000000}.{9f6d2b18-0004-438e-9003-51a46e13a4d5}",
		Name: "Microphone (Focusrite Scarlett 2i2 USB)",
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
	return append([]Device(nil), stubDevices...), nil
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

	// levelStop ends the synthetic-levels ticker goroutine, and levelDone is
	// closed by that goroutine as it exits so Stop can JOIN it rather than
	// merely signal it. The join matters: without it a Stop-then-assert test
	// could observe one more OnLevels callback delivered after Stop returned,
	// which the real build cannot do either — busSilenced is checked on the
	// posting thread before the callback runs. Both are nil when Start was
	// given no OnLevels.
	levelStop chan struct{}
	levelDone chan struct{}

	counters StubCounters
}

// stubErrorBuffer is how many injected errors are held before further ones are
// dropped. The real implementation drops rather than blocks too: a bus callback
// must never wait on a Go consumer.
const stubErrorBuffer = 16

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
		state: StubStateStopped,
		errs:  make(chan error, stubErrorBuffer),
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
	if opts.AudioDeviceID == "" {
		return errors.New("gst: PipelineOpts.AudioDeviceID is required")
	}
	// The identical render-endpoint refusal the real twin applies, through the
	// same shared helper, so the two builds cannot drift on either the rule or
	// the wrapped ErrNotACaptureDevice sentinel. See device_id.go for the rule
	// and its deliberate asymmetry.
	if err := refuseRenderEndpoint(opts.AudioDeviceID); err != nil {
		return err
	}

	if opts.VideoBitrateKbps == 0 {
		opts.VideoBitrateKbps = DefaultVideoBitrateKbps
	}
	if opts.AudioBitrateBps == 0 {
		opts.AudioBitrateBps = DefaultAudioBitrateBps
	}

	p.opts = opts
	p.state = StubStateRunning

	// The synthetic input meters. Only when the caller asked for metering:
	// nil OnLevels means no goroutine, exactly as the real build installs no
	// callback. The goroutine owns nothing but its own step counter and the
	// two channels, so it never takes p.mu — which is what lets Stop join it
	// without holding the lock, and what makes a callback that (wrongly)
	// blocks unable to deadlock anything except the Stop that must wait for
	// it, mirroring the real contract's "MUST NOT BLOCK".
	if opts.OnLevels != nil {
		cb := opts.OnLevels
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
					cb(stubLevelsAt(step))
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
