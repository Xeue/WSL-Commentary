//go:build !cgo || gststub

// return_stub.go is the pure-Go twin of return_cgo.go. It compiles with
// CGO_ENABLED=0 and needs no MinGW, no GStreamer and no audio hardware — that is
// Gate A.
//
// Like gst_stub.go, and unlike a mock, it is meant to WORK. It is what lets
// app.go's return bindings, the reconnect machine in return.go and the frontend
// be built, run and demonstrated on a machine that cannot build the real
// pipeline. Its behaviour mirrors the contract in return.go exactly: Play
// reports connection failure synchronously, Errors carries asynchronous failure
// and is closed by Close, and a stub pipe is single-use.
//
// The one thing it deliberately does NOT do is pretend to have received audio.
// A stub that reported RECEIVING and then sat there forever would make the
// happy path indistinguishable from the failure this whole file exists to make
// visible, so the driving methods below make both outcomes something a test has
// to ask for.
package gst

import (
	"errors"
	"sync"
)

// defaultStubOutputDevices is the fake playback endpoint list.
//
// The IDs are deliberately in the same IMMDevice endpoint format as the capture
// stubs in gst_stub.go — a caller that confuses one of these with the browser's
// mediaDeviceId should fail at Gate A, in a test, and not in a commentary booth
// where the symptom is "the headphones do not work and nothing says why".
//
// The first entry reproduces the real double space in the Dante Virtual
// Soundcard display name, for the same reason defaultStubDevices does.
var defaultStubOutputDevices = []Device{
	{
		ID:   "{0.0.0.00000000}.{1e1c6f21-9f8e-4b6a-9a3d-1a6a37e0c2b1}",
		Name: "DVS Transmit  1-2 (Dante Virtual Soundcard)",
	},
	{
		ID:   "{0.0.0.00000000}.{7a2f4b90-3d55-4c11-8f2e-9b04c7d61f83}",
		Name: "Headphones (Focusrite Scarlett 2i2 USB)",
	},
	{
		ID:   "{0.0.0.00000000}.{c9d3e7a4-6b21-4f08-b7c5-2e5f88a0d413}",
		Name: "Speakers (Realtek(R) Audio)",
	},
}

var (
	stubOutMu      sync.Mutex
	stubOutDevices = append([]Device(nil), defaultStubOutputDevices...)
	stubOutDevErr  error

	// stubReturnPipes records every StubReturnPipe newReturnPipe has handed
	// out, newest last, so that a test driving the reconnect machine through
	// NewReturnMonitor can reach the pipe of the attempt in flight.
	stubReturnPipes []*StubReturnPipe

	// stubReturnPipeErr, when set, makes newReturnPipe itself fail — the Gate A
	// stand-in for "mpegtsdemux is not in the bundle".
	stubReturnPipeErr error
)

// ListOutputDevices returns the fake playback endpoint list. Init need not have
// been called; the stub is usable from a bare unit test.
func ListOutputDevices() ([]Device, error) {
	stubOutMu.Lock()
	defer stubOutMu.Unlock()
	if stubOutDevErr != nil {
		return nil, stubOutDevErr
	}
	return append([]Device(nil), stubOutDevices...), nil
}

// SetStubOutputDevices replaces the list returned by ListOutputDevices. Passing
// nil restores the default three. It exists so the headphone dropdown's empty
// and single-device cases can be exercised.
//
// Stub build only.
func SetStubOutputDevices(devices []Device) {
	stubOutMu.Lock()
	defer stubOutMu.Unlock()
	if devices == nil {
		stubOutDevices = append([]Device(nil), defaultStubOutputDevices...)
		return
	}
	stubOutDevices = append([]Device(nil), devices...)
}

// SetStubOutputDeviceError makes ListOutputDevices fail with err. Passing nil
// restores normal behaviour.
//
// Stub build only.
func SetStubOutputDeviceError(err error) {
	stubOutMu.Lock()
	defer stubOutMu.Unlock()
	stubOutDevErr = err
}

// StubReturnPipe is the pure-Go returnPipe.
//
// Beyond the interface it exposes FailNextPlay and InjectError so that the
// reconnect machine's edges can be driven at Gate A.
//
// Stub build only.
type StubReturnPipe struct {
	mu      sync.Mutex
	played  bool
	closed  bool
	opts    ReturnOpts
	errs    chan error
	playErr error

	plays  int
	closes int
}

// stubReturnErrorBuffer matches the real implementation's buffer so the two
// behave identically under a slow consumer.
const stubReturnErrorBuffer = 8

// newReturnPipe creates a StubReturnPipe and records it. See return.go.
func newReturnPipe() (returnPipe, error) {
	stubOutMu.Lock()
	defer stubOutMu.Unlock()
	if err := stubReturnPipeErr; err != nil {
		return nil, err
	}
	p := &StubReturnPipe{errs: make(chan error, stubReturnErrorBuffer)}
	stubReturnPipes = append(stubReturnPipes, p)
	return p, nil
}

// SetStubReturnPipeError makes newReturnPipe fail with err, which is the Gate A
// stand-in for a missing plugin. Passing nil restores normal behaviour.
//
// Stub build only.
func SetStubReturnPipeError(err error) {
	stubOutMu.Lock()
	defer stubOutMu.Unlock()
	stubReturnPipeErr = err
}

// StubReturnPipes returns every pipe newReturnPipe has created, newest last.
//
// Stub build only.
func StubReturnPipes() []*StubReturnPipe {
	stubOutMu.Lock()
	defer stubOutMu.Unlock()
	return append([]*StubReturnPipe(nil), stubReturnPipes...)
}

// ResetStubReturnPipes forgets every recorded pipe. Tests that assert on the
// list call it first, because the slice is package-global and outlives one
// monitor.
//
// Stub build only.
func ResetStubReturnPipes() {
	stubOutMu.Lock()
	defer stubOutMu.Unlock()
	stubReturnPipes = nil
}

// Play records the options and succeeds, unless FailNextPlay has queued a
// failure. It validates nothing that ReturnOpts.normalise has already checked:
// the machine calls normalise before it ever reaches a pipe, and duplicating
// the rules here would let the two drift.
func (p *StubReturnPipe) Play(opts ReturnOpts) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.plays++
	if p.closed {
		return errors.New("gst: return pipeline is closed")
	}
	if p.played {
		return errors.New("gst: return pipeline already played")
	}
	if err := p.playErr; err != nil {
		p.playErr = nil
		return err
	}
	p.opts = opts
	p.played = true
	return nil
}

// Errors returns the asynchronous error channel. It is closed by Close.
func (p *StubReturnPipe) Errors() <-chan error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.errs
}

// Close marks the pipe closed and closes the error channel. It is idempotent.
func (p *StubReturnPipe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closes++
	p.played = false
	if !p.closed {
		p.closed = true
		close(p.errs)
	}
	return nil
}

// FailNextPlay makes the next Play return err. Passing nil cancels it.
//
// Stub build only.
func (p *StubReturnPipe) FailNextPlay(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.playErr = err
}

// InjectError delivers err on the channel returned by Errors, simulating the
// far end going away. It never blocks: if the buffer is full or the pipe has
// been closed the error is dropped and InjectError reports false.
//
// Stub build only.
func (p *StubReturnPipe) InjectError(err error) bool {
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

// PlayedWith returns the options the pipe was last played with, and whether it
// is currently playing.
//
// Stub build only.
func (p *StubReturnPipe) PlayedWith() (ReturnOpts, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opts, p.played
}

// Counts returns how many times Play and Close have been called.
//
// Stub build only.
func (p *StubReturnPipe) Counts() (plays, closes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.plays, p.closes
}

// Compile-time assertion that the stub satisfies the seam.
var _ returnPipe = (*StubReturnPipe)(nil)
