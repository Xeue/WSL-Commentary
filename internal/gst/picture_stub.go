//go:build !cgo || gststub

// picture_stub.go is the pure-Go twin of picture_cgo.go. It compiles with
// CGO_ENABLED=0 and needs no MinGW, no GStreamer, no d3d11 and no GPU — that is
// Gate A.
//
// Like gst_stub.go and return_stub.go, and unlike a mock, it is meant to WORK.
// It is what lets app_picture.go, the reconnect machine in picture.go and the
// frontend be built, run and demonstrated on a machine that cannot build the
// real pipeline. Its behaviour mirrors the contract in picture.go exactly: Play
// reports connection failure synchronously, Errors carries asynchronous failure
// and is closed by Close, and a stub pipe is single-use.
//
// The one thing it deliberately does NOT do is pretend to have decoded a frame
// on its own initiative. A stub that reported SHOWING and then sat there would
// make the happy path indistinguishable from the failure this whole file exists
// to make visible, so the driving methods below make both outcomes something a
// test has to ask for.
package gst

import (
	"errors"
	"sync"
)

// stubPictureErrorBuffer matches the real implementation's buffer so the two
// behave identically under a slow consumer.
const stubPictureErrorBuffer = 8

var (
	stubPicMu sync.Mutex

	// stubPicturePipes records every StubPicturePipe newPicturePipe has handed
	// out, newest last, so that a test driving the reconnect machine through
	// NewPictureMonitor can reach the pipe of the attempt in flight.
	stubPicturePipes []*StubPicturePipe

	// stubPicturePipeErr, when set, makes newPicturePipe itself fail — the Gate A
	// stand-in for "the d3d11 plugin is not in the bundle".
	stubPicturePipeErr error
)

// StubPicturePipe is the pure-Go picturePipe.
//
// Beyond the interface it exposes FailNextPlay and InjectError so that the
// reconnect machine's edges can be driven at Gate A.
//
// Stub build only.
type StubPicturePipe struct {
	mu      sync.Mutex
	played  bool
	closed  bool
	opts    PictureOpts
	errs    chan error
	playErr error

	plays  int
	closes int
}

// newPicturePipe creates a StubPicturePipe and records it. See picture.go.
func newPicturePipe() (picturePipe, error) {
	stubPicMu.Lock()
	defer stubPicMu.Unlock()
	if err := stubPicturePipeErr; err != nil {
		return nil, err
	}
	p := &StubPicturePipe{errs: make(chan error, stubPictureErrorBuffer)}
	stubPicturePipes = append(stubPicturePipes, p)
	return p, nil
}

// SetStubPicturePipeError makes newPicturePipe fail with err, which is the Gate
// A stand-in for a missing plugin. Passing nil restores normal behaviour.
//
// Stub build only.
func SetStubPicturePipeError(err error) {
	stubPicMu.Lock()
	defer stubPicMu.Unlock()
	stubPicturePipeErr = err
}

// StubPicturePipes returns every pipe newPicturePipe has created, newest last.
//
// Stub build only.
func StubPicturePipes() []*StubPicturePipe {
	stubPicMu.Lock()
	defer stubPicMu.Unlock()
	return append([]*StubPicturePipe(nil), stubPicturePipes...)
}

// ResetStubPicturePipes forgets every recorded pipe. Tests that assert on the
// list call it first, because the slice is package-global and outlives one
// monitor.
//
// Stub build only.
func ResetStubPicturePipes() {
	stubPicMu.Lock()
	defer stubPicMu.Unlock()
	stubPicturePipes = nil
}

// Play records the options and succeeds, unless FailNextPlay has queued a
// failure.
//
// It validates nothing PictureOpts.normalise has already checked: the machine
// calls normalise before it ever reaches a pipe, and duplicating the rules here
// would let the two drift. It does record the window handle, because the one
// thing a Gate A test genuinely wants to assert about this path is that the
// handle the overlay produced is the handle the pipeline was given.
func (p *StubPicturePipe) Play(opts PictureOpts) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.plays++
	if p.closed {
		return errors.New("gst: picture pipeline is closed")
	}
	if p.played {
		return errors.New("gst: picture pipeline already played")
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
func (p *StubPicturePipe) Errors() <-chan error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.errs
}

// Close marks the pipe closed and closes the error channel. It is idempotent.
func (p *StubPicturePipe) Close() error {
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
func (p *StubPicturePipe) FailNextPlay(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.playErr = err
}

// InjectError delivers err on the channel returned by Errors, simulating the far
// end going away. It never blocks: if the buffer is full or the pipe has been
// closed the error is dropped and InjectError reports false.
//
// Stub build only.
func (p *StubPicturePipe) InjectError(err error) bool {
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
func (p *StubPicturePipe) PlayedWith() (PictureOpts, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opts, p.played
}

// Counts returns how many times Play and Close have been called.
//
// Stub build only.
func (p *StubPicturePipe) Counts() (plays, closes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.plays, p.closes
}

// Compile-time assertion that the stub satisfies the seam.
var _ picturePipe = (*StubPicturePipe)(nil)
