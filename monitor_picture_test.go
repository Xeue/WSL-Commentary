//go:build dev || production || bindings

// Tests for the SRT picture inside the PGM monitor process: the bookkeeping,
// the overlay's rectangle and visibility gate, the state forwarding, the
// diagnostics, and the teardown order. They are the application's old
// picture tests, moved with the code they test.
//
// WHAT THESE TESTS DO NOT REACH: nothing here creates a window or a pipeline
// — the monitor and the overlay are fakes asserting the CONTRACT of
// gst.PictureMonitor and gst.PictureOverlay — and nothing here runs the link:
// the options arrive through the pictureOptsDial seam, which in the real
// process is a call to the application.
package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"wslcomms/internal/gst"
	"wslcomms/internal/monitorlink"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakePictureMonitor struct {
	mu       sync.Mutex
	started  bool
	stopped  bool
	opts     gst.PictureOpts
	startErr error
	stopErr  error
	states   chan gst.PictureState
}

func newFakePictureMonitor() *fakePictureMonitor {
	return &fakePictureMonitor{states: make(chan gst.PictureState, 8)}
}

func (m *fakePictureMonitor) Start(opts gst.PictureOpts) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.startErr; err != nil {
		return err
	}
	m.started = true
	m.opts = opts
	return nil
}

func (m *fakePictureMonitor) Stop() error {
	m.mu.Lock()
	stopped := m.stopped
	m.stopped = true
	err := m.stopErr
	m.mu.Unlock()
	if !stopped {
		m.states <- gst.PictureStateStopped
		close(m.states)
	}
	return err
}

func (m *fakePictureMonitor) States() <-chan gst.PictureState { return m.states }
func (m *fakePictureMonitor) emit(s gst.PictureState)         { m.states <- s }

func (m *fakePictureMonitor) startedWith() (gst.PictureOpts, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opts, m.started
}

var _ gst.PictureMonitor = (*fakePictureMonitor)(nil)

// fakeOverlay records what it was asked to do and NEVER BLOCKS, which is the
// property the whole design rests on.
type fakeOverlay struct {
	mu       sync.Mutex
	handle   uintptr
	rects    []gst.PictureRect
	visibles []bool
	closes   int
}

func newFakeOverlay() *fakeOverlay { return &fakeOverlay{handle: 0x0BADF00D} }

func (o *fakeOverlay) Handle() uintptr { return o.handle }
func (o *fakeOverlay) SetRect(r gst.PictureRect) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rects = append(o.rects, r)
	return nil
}
func (o *fakeOverlay) SetVisible(v bool) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.visibles = append(o.visibles, v)
	return nil
}
func (o *fakeOverlay) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closes++
	return nil
}
func (o *fakeOverlay) lastRect() (gst.PictureRect, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.rects) == 0 {
		return gst.PictureRect{}, false
	}
	return o.rects[len(o.rects)-1], true
}
func (o *fakeOverlay) lastVisible() (bool, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.visibles) == 0 {
		return false, false
	}
	return o.visibles[len(o.visibles)-1], true
}
func (o *fakeOverlay) closeCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closes
}

var _ gst.PictureOverlay = (*fakeOverlay)(nil)

// stubbornOverlay is an overlay whose Close gave up on its message thread: it
// returns PROMPTLY, with the abandonment wrapped, exactly as the real one does
// when its budget expires.
type stubbornOverlay struct{ fakeOverlay }

func (o *stubbornOverlay) Close() error {
	return fmt.Errorf("gst: overlay: the message thread did not stop and has been ABANDONED: %w",
		gst.ErrAbandonedThread)
}

// newTestMonitorApp builds a MonitorApp over a link nobody is on the other end
// of, with fakes for the monitor, the overlay and the options.
func newTestMonitorApp(t *testing.T) (*MonitorApp, *fakePictureMonitor, *fakeOverlay) {
	t.Helper()
	pr, pw := io.Pipe()
	link := monitorlink.NewClient(pr, io.Discard)
	m := newMonitorApp(link, nil)
	m.exitProcess = func() {}
	m.events.startOnce.Do(func() {}) // never launch the pump's goroutine

	mon := newFakePictureMonitor()
	ov := newFakeOverlay()
	m.pictureDial = func() gst.PictureMonitor { return mon }
	m.overlayDial = func() (gst.PictureOverlay, error) { return ov, nil }
	m.pictureOptsDial = func() (gst.PictureOpts, error) {
		return gst.PictureOpts{Host: "m2lx.example.com", Port: 40501, LatencyMs: 120}, nil
	}
	t.Cleanup(func() {
		_ = pw.Close()
		m.rootCancel()
	})
	return m, mon, ov
}

func drainMonitorPump(m *MonitorApp) []pumpEvent {
	var out []pumpEvent
	for {
		select {
		case e := <-m.events.ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

// ---------------------------------------------------------------------------
// Bookkeeping
// ---------------------------------------------------------------------------

func TestMonitorStartAndStopPictureBookkeeping(t *testing.T) {
	m, mon, _ := newTestMonitorApp(t)

	if err := m.StopPicture(); !errors.Is(err, errPictureNotRunning) {
		t.Fatalf("StopPicture() with nothing running error = %v, want errPictureNotRunning", err)
	}
	if err := m.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}
	if err := m.StartPicture(); !errors.Is(err, errPictureAlreadyRunning) {
		t.Fatalf("second StartPicture() error = %v, want errPictureAlreadyRunning", err)
	}
	if _, started := mon.startedWith(); !started {
		t.Fatal("the monitor was never started")
	}
	if err := m.StopPicture(); err != nil {
		t.Fatalf("StopPicture() error = %v", err)
	}
}

func TestMonitorStartPictureRefusesWhileShuttingDown(t *testing.T) {
	m, _, _ := newTestMonitorApp(t)
	m.closing.Store(true)
	if err := m.StartPicture(); !errors.Is(err, errShuttingDown) {
		t.Fatalf("StartPicture() while closing error = %v, want errShuttingDown", err)
	}
}

func TestMonitorStartPicturePropagatesTheApplicationsRefusal(t *testing.T) {
	// The application applies its refusals — the SRT audio return holding the
	// output, a key length with no passphrase — when it answers PictureOpts.
	// They reach the operator through StartPicture, with nothing left running.
	m, mon, _ := newTestMonitorApp(t)
	m.pictureOptsDial = func() (gst.PictureOpts, error) {
		return gst.PictureOpts{}, errors.New("returnSource is \"srt\" ... the commentator's AUDIO comes from Kinesis")
	}
	err := m.StartPicture()
	if err == nil || !strings.Contains(err.Error(), "Kinesis") {
		t.Fatalf("StartPicture() error = %v, want the application's refusal", err)
	}
	if _, started := mon.startedWith(); started {
		t.Fatal("the pipeline was started despite the refusal")
	}
	if m.pic != nil {
		t.Fatal("a session was left behind after a refusal")
	}
}

func TestMonitorStartPictureGivesTheMonitorTheOverlaysHandle(t *testing.T) {
	// A zero handle makes d3d11videosink open its own top-level window. The
	// handle the pipeline is given must be the overlay's, and the options must
	// be the application's, verbatim.
	m, mon, ov := newTestMonitorApp(t)
	if err := m.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}
	defer m.StopPicture()
	opts, _ := mon.startedWith()
	if opts.WindowHandle != ov.Handle() {
		t.Fatalf("the monitor was given handle 0x%x, want the overlay's 0x%x", opts.WindowHandle, ov.Handle())
	}
	if opts.Port != 40501 || opts.Host != "m2lx.example.com" || opts.LatencyMs != 120 {
		t.Fatalf("the monitor was given %+v, want the application's options", opts)
	}
}

func TestMonitorRefreshPictureStartsAFreshMonitor(t *testing.T) {
	m, _, _ := newTestMonitorApp(t)
	var mu sync.Mutex
	var made []*fakePictureMonitor
	m.pictureDial = func() gst.PictureMonitor {
		f := newFakePictureMonitor()
		mu.Lock()
		made = append(made, f)
		mu.Unlock()
		return f
	}
	if err := m.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}
	if err := m.RefreshPicture(); err != nil {
		t.Fatalf("RefreshPicture() error = %v", err)
	}
	defer m.StopPicture()
	mu.Lock()
	defer mu.Unlock()
	if len(made) != 2 {
		t.Fatalf("Refresh built %d monitors, want 2", len(made))
	}
	made[0].mu.Lock()
	stopped := made[0].stopped
	made[0].mu.Unlock()
	if !stopped {
		t.Fatal("Refresh did not stop the old monitor")
	}
	if m.pic == nil || m.pic.mon != made[1] {
		t.Fatal("the running session is not the fresh monitor")
	}
}

// ---------------------------------------------------------------------------
// The rectangle and the visibility gate
// ---------------------------------------------------------------------------

func TestMonitorSetPictureRectScalesCSSPixelsByThePagesOwnRatio(t *testing.T) {
	m, _, ov := newTestMonitorApp(t)
	if err := m.SetPictureRect(16, 80, 960, 540, 1.5); err != nil {
		t.Fatalf("SetPictureRect() error = %v", err)
	}
	got, ok := ov.lastRect()
	if !ok {
		t.Fatal("the overlay was never told where to sit")
	}
	if want := (gst.PictureRect{X: 24, Y: 120, W: 1440, H: 810}); got != want {
		t.Fatalf("the overlay was placed at %v, want %v", got, want)
	}
}

func TestMonitorSetPictureRectBeforeTheWindowExistsIsNotAnError(t *testing.T) {
	m, _, _ := newTestMonitorApp(t)
	m.overlayDial = func() (gst.PictureOverlay, error) { return nil, gst.ErrNoHostWindow }
	if err := m.SetPictureRect(0, 0, 100, 100, 1); err != nil {
		t.Fatalf("SetPictureRect() before the window exists error = %v, want nil", err)
	}
	// Once the window arrives, the remembered rectangle is applied unasked.
	ov := newFakeOverlay()
	m.overlayDial = func() (gst.PictureOverlay, error) { return ov, nil }
	if _, err := m.pictureOverlay(); err != nil {
		t.Fatalf("pictureOverlay() error = %v", err)
	}
	if got, ok := ov.lastRect(); !ok || got != (gst.PictureRect{W: 100, H: 100}) {
		t.Fatalf("a freshly created overlay was placed at %v (set=%v), want the remembered rectangle", got, ok)
	}
}

func TestMonitorOverlayIsShownOnlyWhenAllThreeConditionsHold(t *testing.T) {
	// want && showing && the rectangle has area. Each is a different way the
	// picture goes wrong: a picture over the controls, a black rectangle over
	// the fallback mosaic, a swapchain resized to nothing.
	m, _, ov := newTestMonitorApp(t)
	if err := m.SetPictureRect(0, 0, 640, 360, 1); err != nil {
		t.Fatalf("SetPictureRect() error = %v", err)
	}
	cases := []struct {
		name    string
		want    bool
		state   gst.PictureState
		rect    gst.PictureRect
		visible bool
	}{
		{"nothing asked for, nothing showing", false, gst.PictureStateStopped, gst.PictureRect{W: 640, H: 360}, false},
		{"asked for but not showing", true, gst.PictureStateConnecting, gst.PictureRect{W: 640, H: 360}, false},
		{"asked for but backing off", true, gst.PictureStateBackoff, gst.PictureRect{W: 640, H: 360}, false},
		{"showing but not asked for", false, gst.PictureStateShowing, gst.PictureRect{W: 640, H: 360}, false},
		{"showing and asked for, but no area", true, gst.PictureStateShowing, gst.PictureRect{}, false},
		{"all three", true, gst.PictureStateShowing, gst.PictureRect{W: 640, H: 360}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m.picStateMu.Lock()
			m.lastPicture = c.state
			m.picStateMu.Unlock()
			m.picViewMu.Lock()
			m.picWantVisible = c.want
			m.picRect = c.rect
			m.applyPictureVisibilityViewLocked()
			m.picViewMu.Unlock()
			got, ok := ov.lastVisible()
			if !ok {
				t.Fatal("the overlay was never told whether to be visible")
			}
			if got != c.visible {
				t.Fatalf("visible = %v, want %v", got, c.visible)
			}
		})
	}
}

func TestMonitorOverlayFollowsTheStateBeforeThePageIsTold(t *testing.T) {
	m, mon, ov := newTestMonitorApp(t)
	if err := m.SetPictureRect(0, 0, 640, 360, 1); err != nil {
		t.Fatal(err)
	}
	if err := m.SetPictureVisible(true); err != nil {
		t.Fatal(err)
	}
	if err := m.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}
	mon.emit(gst.PictureStateShowing)
	waitForCond(t, "the overlay to be shown", func() bool { v, ok := ov.lastVisible(); return ok && v })
	mon.emit(gst.PictureStateBackoff)
	waitForCond(t, "the overlay to be hidden when the picture drops", func() bool { v, ok := ov.lastVisible(); return ok && !v })
	if err := m.StopPicture(); err != nil {
		t.Fatalf("StopPicture() error = %v", err)
	}
}

// ---------------------------------------------------------------------------
// State forwarding and diagnostics
// ---------------------------------------------------------------------------

func TestMonitorPictureStatesReachThePageAndTheGetter(t *testing.T) {
	m, mon, _ := newTestMonitorApp(t)
	if err := m.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}
	mon.emit(gst.PictureStateConnecting)
	mon.emit(gst.PictureStateShowing)
	waitForCond(t, "SHOWING to reach the getter", func() bool {
		s, _ := m.GetPictureState()
		return s == gst.PictureStateShowing
	})
	if err := m.StopPicture(); err != nil {
		t.Fatal(err)
	}
	var states []gst.PictureState
	for _, e := range drainMonitorPump(m) {
		if e.name == EventPicture {
			states = append(states, e.data.(gst.PictureState))
		}
	}
	want := []gst.PictureState{gst.PictureStateConnecting, gst.PictureStateShowing, gst.PictureStateStopped}
	if len(states) != len(want) {
		t.Fatalf("the page heard %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("the page heard %v, want %v", states, want)
		}
	}
}

func TestMonitorPictureDiagnosticIsEmittedOnceAfterThreeFailures(t *testing.T) {
	m, mon, _ := newTestMonitorApp(t)
	m.pictureOptsDial = func() (gst.PictureOpts, error) {
		return gst.PictureOpts{Host: "m2lx.example.com", Port: 40501, Passphrase: "hunter2", PBKeyLen: 32}, nil
	}
	if err := m.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}
	for i := 0; i < 5; i++ {
		mon.emit(gst.PictureStateBackoff)
	}
	waitForCond(t, "five BACKOFFs to be forwarded", func() bool {
		n := 0
		m.picStateMu.Lock()
		last := m.lastPicture
		m.picStateMu.Unlock()
		if last == gst.PictureStateBackoff {
			n = 1
		}
		return n == 1 && len(mon.states) == 0
	})
	time.Sleep(20 * time.Millisecond)
	if err := m.StopPicture(); err != nil {
		t.Fatal(err)
	}
	var errs []string
	for _, e := range drainMonitorPump(m) {
		if e.name == EventError {
			errs = append(errs, e.data.(string))
		}
	}
	if len(errs) != 1 {
		t.Fatalf("%d diagnostics were emitted over five failures, want exactly 1: %v", len(errs), errs)
	}
	if strings.Contains(errs[0], "hunter2") {
		t.Fatalf("the diagnostic leaks the passphrase: %q", errs[0])
	}
	if !strings.Contains(errs[0], "AES-256") || !strings.Contains(errs[0], "fallback mosaic") {
		t.Fatalf("the diagnostic %q does not name the key length and the fallback", errs[0])
	}
}

func TestMonitorSoftwareDecodeNoteIsEmittedOnce(t *testing.T) {
	gst.SetStubPictureSoftwareDecoder("avdec_h265")
	defer gst.SetStubPictureSoftwareDecoder("")
	m, _, _ := newTestMonitorApp(t)
	for i := 0; i < 2; i++ {
		if err := m.StartPicture(); err != nil {
			t.Fatalf("StartPicture() #%d error = %v", i+1, err)
		}
		if err := m.StopPicture(); err != nil {
			t.Fatalf("StopPicture() #%d error = %v", i+1, err)
		}
	}
	notes := 0
	for _, e := range drainMonitorPump(m) {
		if e.name == EventNote {
			notes++
			if s := e.data.(string); !strings.Contains(s, "avdec_h265") {
				t.Errorf("the note %q does not name the decoder", s)
			}
		}
	}
	if notes != 1 {
		t.Fatalf("%d software-decode notes over two starts, want 1", notes)
	}
}

// ---------------------------------------------------------------------------
// Teardown
// ---------------------------------------------------------------------------

func TestMonitorTeardownStopsThePictureThenDestroysTheOverlay(t *testing.T) {
	m, mon, ov := newTestMonitorApp(t)
	if err := m.SetPictureRect(0, 0, 640, 360, 1); err != nil {
		t.Fatal(err)
	}
	if err := m.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}
	if err := m.stopPictureForTeardown(); err != nil {
		t.Fatalf("stopPictureForTeardown() error = %v", err)
	}
	mon.mu.Lock()
	stopped := mon.stopped
	mon.mu.Unlock()
	if !stopped {
		t.Error("teardown did not stop the monitor")
	}
	if ov.closeCount() != 1 {
		t.Errorf("the overlay was closed %d times, want exactly 1", ov.closeCount())
	}
	if m.picOverlay != nil {
		t.Error("teardown left the overlay reference behind")
	}
}

func TestMonitorTeardownEndsTheProcessEvenWhenTheOverlayAbandonedItsThread(t *testing.T) {
	// The overlay's Close returned promptly, saying it abandoned its thread.
	// The joined error keeps that inspectable, and the process must end through
	// exitProcess — TerminateProcess in production — rather than through an
	// exit that would run DLL_PROCESS_DETACH over the abandoned thread.
	m, _, _ := newTestMonitorApp(t)
	stubborn := &stubbornOverlay{fakeOverlay: *newFakeOverlay()}
	m.picViewMu.Lock()
	m.picOverlay = stubborn
	m.picViewMu.Unlock()

	err := m.stopPictureForTeardown()
	if !errors.Is(err, gst.ErrAbandonedThread) {
		t.Fatalf("stopPictureForTeardown() error = %v, which does not unwrap to gst.ErrAbandonedThread", err)
	}

	exited := make(chan struct{}, 1)
	m.exitProcess = func() {
		select {
		case exited <- struct{}{}:
		default:
		}
	}
	m.picViewMu.Lock()
	m.picOverlay = &stubbornOverlay{fakeOverlay: *newFakeOverlay()}
	m.picViewMu.Unlock()
	start := time.Now()
	m.teardown()
	if time.Since(start) > monitorPictureStopBudget {
		t.Fatal("teardown waited out the budget on an overlay that answered at once")
	}
	select {
	case <-exited:
	default:
		t.Fatal("teardown returned without ending the process")
	}
}
