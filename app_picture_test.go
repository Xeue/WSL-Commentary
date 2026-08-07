//go:build dev || production || bindings

// Tests for the SRT picture's bound surface: the exclusivity guard, the
// rectangle, the visibility gate, the state forwarding and the teardown order.
//
// WHAT THESE TESTS DO NOT REACH, stated plainly rather than left to be assumed:
// nothing here creates a window, and the overlay is a fake throughout. That is
// deliberate — a unit test that created a real HWND would need a message loop, a
// parent window and a desktop session, and CI has none of the three — but it
// means the fake is asserting the CONTRACT of gst.PictureOverlay and not its
// implementation. Everything in overlay_windows.go below the interface is
// unproven by this file. See the report.
package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakePictureMonitor is a gst.PictureMonitor that records what it was given and
// lets the test drive the state stream.
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
		// The real monitor emits STOPPED and then closes the channel, which is
		// what lets the forwarding goroutine exit and StopPicture's join return.
		m.states <- gst.PictureStateStopped
		close(m.states)
	}
	return err
}

func (m *fakePictureMonitor) States() <-chan gst.PictureState { return m.states }

func (m *fakePictureMonitor) emit(s gst.PictureState) { m.states <- s }

func (m *fakePictureMonitor) startedWith() (gst.PictureOpts, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opts, m.started
}

var _ gst.PictureMonitor = (*fakePictureMonitor)(nil)

// fakeOverlay is a gst.PictureOverlay that records what it was asked to do.
//
// It asserts the one property the real one promises and the whole design rests
// on: NOTHING BLOCKS. Every method here returns immediately, so a test that
// hangs is a test that found a caller waiting on something it should not be.
type fakeOverlay struct {
	mu       sync.Mutex
	handle   uintptr
	rects    []gst.PictureRect
	visibles []bool
	closes   int
	setErr   error
}

func newFakeOverlay() *fakeOverlay { return &fakeOverlay{handle: 0x0BADF00D} }

func (o *fakeOverlay) Handle() uintptr { return o.handle }

func (o *fakeOverlay) SetRect(r gst.PictureRect) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rects = append(o.rects, r)
	return o.setErr
}

func (o *fakeOverlay) SetVisible(v bool) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.visibles = append(o.visibles, v)
	return o.setErr
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

// withFakePicture wires a fake monitor and a fake overlay into the app.
//
// It also sets returnSource to webrtc, because that is the configuration this
// whole work package exists to produce — the audio comes from Kinesis and SRT
// carries the picture — and because StartPicture correctly refuses anything
// else.
func withFakePicture(a *App) (*fakePictureMonitor, *fakeOverlay) {
	mon := newFakePictureMonitor()
	ov := newFakeOverlay()
	a.pictureDial = func() gst.PictureMonitor { return mon }
	a.overlayDial = func() (gst.PictureOverlay, error) { return ov, nil }

	cfg := validConfig()
	cfg.ReturnSource = config.ReturnSourceWebRTC
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	return mon, ov
}

// ---------------------------------------------------------------------------
// Exclusivity: the picture and the SRT audio return cannot share one output
// ---------------------------------------------------------------------------

func TestStartPictureRefusesWhileTheSRTAudioReturnIsSelected(t *testing.T) {
	// They dial the SAME M2L-X output, and an M2L-X SRT listener accepts exactly
	// one peer and never displaces the incumbent. Two callers on 40501 from one
	// process means one of them sits in its ladder for the whole match and which
	// one wins is a race.
	//
	// This is also the whole point of the work package, so the message has to say
	// it: the audio comes from Kinesis.
	a, _ := newTestApp(t)
	withFakePicture(a)

	cfg := validConfig()
	cfg.ReturnSource = config.ReturnSourceSRT
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	err := a.StartPicture()
	if err == nil {
		t.Fatal("StartPicture() succeeded with the SRT audio return selected; both would dial 40501")
	}
	for _, want := range []string{"returnSource", "Kinesis", config.ReturnSourceWebRTC} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q; the operator has to be told what to change", err, want)
		}
	}
	if a.pic != nil {
		t.Fatal("StartPicture() left a session behind after refusing")
	}
}

func TestStartPictureRefusesWhileShuttingDown(t *testing.T) {
	// Building a pipeline now would open an SRT socket and a D3D11 device that
	// teardown has already walked past, and the process would exit still holding
	// them. Same reasoning as startSession and StartReturn.
	a, _ := newTestApp(t)
	withFakePicture(a)
	a.closing.Store(true)

	if err := a.StartPicture(); !errors.Is(err, errShuttingDown) {
		t.Fatalf("StartPicture() while closing error = %v, want errShuttingDown", err)
	}
}

func TestStartAndStopPictureBookkeeping(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	mon, _ := withFakePicture(a)

	if err := a.StopPicture(); !errors.Is(err, errPictureNotRunning) {
		t.Fatalf("StopPicture() with nothing running error = %v, want errPictureNotRunning", err)
	}
	if err := a.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}
	if err := a.StartPicture(); !errors.Is(err, errPictureAlreadyRunning) {
		t.Fatalf("second StartPicture() error = %v, want errPictureAlreadyRunning", err)
	}
	if _, started := mon.startedWith(); !started {
		t.Fatal("the monitor was never started")
	}
	if err := a.StopPicture(); err != nil {
		t.Fatalf("StopPicture() error = %v", err)
	}
}

func TestStartPictureGivesTheMonitorTheOverlaysHandle(t *testing.T) {
	// A zero handle makes d3d11videosink open its own top-level window on the
	// operator's screen. The handle the pipeline is given must be the handle the
	// overlay actually produced, not a copy of one taken earlier.
	a, _ := newTestApp(t)
	silencePump(a)
	mon, ov := withFakePicture(a)

	if err := a.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}
	defer a.StopPicture()

	opts, _ := mon.startedWith()
	if opts.WindowHandle != ov.Handle() {
		t.Fatalf("the monitor was given handle 0x%x, want the overlay's 0x%x",
			opts.WindowHandle, ov.Handle())
	}
	if opts.Port != config.DefaultSRTReturnPort {
		t.Errorf("the monitor was given port %d, want %d — Output 1, src=pgm, the programme picture",
			opts.Port, config.DefaultSRTReturnPort)
	}
}

func TestPictureOptsCarryNoSecretIntoTheDiagnostic(t *testing.T) {
	// pictureDiagnostic is emitted to the frontend as an error string. It takes
	// PictureOpts by value and reads the passphrase for ONE BOOLEAN; nothing in
	// what it returns may be derived from the contents.
	opts := gst.PictureOpts{Host: "m2lx.example.com", Port: 40501, Passphrase: "hunter2", PBKeyLen: 32}
	got := pictureDiagnostic(opts)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("the diagnostic leaks the passphrase: %q", got)
	}
	if !strings.Contains(got, "AES-256") {
		t.Errorf("the diagnostic %q does not name the key length the operator has to check", got)
	}
	if !strings.Contains(got, "fallback mosaic") {
		t.Errorf("the diagnostic %q does not say what the commentator is actually looking at", got)
	}

	// And the unencrypted case names the opposite mistake.
	plain := pictureDiagnostic(gst.PictureOpts{Host: "h", Port: 40501})
	if !strings.Contains(plain, "NO encryption") {
		t.Errorf("the unencrypted diagnostic %q does not say the session is unencrypted", plain)
	}
}

// ---------------------------------------------------------------------------
// The rectangle
// ---------------------------------------------------------------------------

func TestSetPictureRectScalesCSSPixelsByThePagesOwnRatio(t *testing.T) {
	// The operator runs a 3840x2088 window. A rectangle in CSS pixels is not a
	// rectangle in physical pixels, and the factor is the PAGE's
	// devicePixelRatio rather than the monitor's DPI — the two differ the moment
	// anyone touches the WebView zoom.
	a, _ := newTestApp(t)
	_, ov := withFakePicture(a)

	if err := a.SetPictureRect(16, 80, 960, 540, 1.5); err != nil {
		t.Fatalf("SetPictureRect() error = %v", err)
	}
	got, ok := ov.lastRect()
	if !ok {
		t.Fatal("the overlay was never told where to sit")
	}
	want := gst.PictureRect{X: 24, Y: 120, W: 1440, H: 810}
	if got != want {
		t.Fatalf("the overlay was placed at %v, want %v", got, want)
	}
}

func TestSetPictureRectBeforeTheWindowExistsIsNotAnError(t *testing.T) {
	// Called during startup, before Wails has made the window. It is a normal
	// moment and must not become an error toast; the rectangle is remembered so
	// that an overlay created later is positioned before it is ever shown.
	a, _ := newTestApp(t)
	a.overlayDial = func() (gst.PictureOverlay, error) { return nil, gst.ErrNoHostWindow }

	if err := a.SetPictureRect(0, 0, 100, 100, 1); err != nil {
		t.Fatalf("SetPictureRect() before the window exists error = %v, want nil", err)
	}

	// And once the window arrives, the remembered rectangle is applied without
	// the frontend having to say it again.
	ov := newFakeOverlay()
	a.overlayDial = func() (gst.PictureOverlay, error) { return ov, nil }

	a.picViewMu.Lock()
	_, err := a.pictureOverlayViewLocked()
	a.picViewMu.Unlock()
	if err != nil {
		t.Fatalf("pictureOverlayViewLocked() error = %v", err)
	}
	got, ok := ov.lastRect()
	if !ok || got != (gst.PictureRect{W: 100, H: 100}) {
		t.Fatalf("a freshly created overlay was placed at %v (set=%v), want the remembered rectangle",
			got, ok)
	}
}

// ---------------------------------------------------------------------------
// The visibility gate, which is the one that keeps a black box off the mosaic
// ---------------------------------------------------------------------------

func TestOverlayIsShownOnlyWhenAllThreeConditionsHold(t *testing.T) {
	// want && showing && the rectangle has area. Each one is a different way the
	// picture goes wrong:
	//
	//	no want     a picture covers the Settings screen
	//	no showing  a black rectangle covers the FALLBACK MOSAIC, which is the
	//	            soft picture the commentator is meant to be watching instead
	//	no area     gstd3d11 resizes a swapchain to nothing
	a, _ := newTestApp(t)
	silencePump(a)
	_, ov := withFakePicture(a)

	// Bring the overlay into being with a real rectangle.
	if err := a.SetPictureRect(0, 0, 640, 360, 1); err != nil {
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
			a.picStateMu.Lock()
			a.lastPicture = c.state
			a.picStateMu.Unlock()

			a.picViewMu.Lock()
			a.picWantVisible = c.want
			a.picRect = c.rect
			a.applyPictureVisibilityViewLocked()
			a.picViewMu.Unlock()

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

func TestTheOverlayFollowsTheStateBeforeThePageIsTold(t *testing.T) {
	// The window must be gone by the time the page is told the picture has
	// stopped. The other order shows the page a state it cannot act on for a
	// frame, during which a black rectangle sits over the fallback mosaic.
	a, _ := newTestApp(t)
	silencePump(a)
	mon, ov := withFakePicture(a)

	if err := a.SetPictureRect(0, 0, 640, 360, 1); err != nil {
		t.Fatalf("SetPictureRect() error = %v", err)
	}
	if err := a.SetPictureVisible(true); err != nil {
		t.Fatalf("SetPictureVisible() error = %v", err)
	}
	if err := a.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}

	mon.emit(gst.PictureStateShowing)
	waitForCond(t, "the overlay to be shown", func() bool {
		v, ok := ov.lastVisible()
		return ok && v
	})

	mon.emit(gst.PictureStateBackoff)
	waitForCond(t, "the overlay to be hidden when the picture drops", func() bool {
		v, ok := ov.lastVisible()
		return ok && !v
	})

	if err := a.StopPicture(); err != nil {
		t.Fatalf("StopPicture() error = %v", err)
	}
}

func TestStopPictureHidesTheOverlayEvenWithNothingRunning(t *testing.T) {
	// This is the teardown path. An overlay left visible over the page with a
	// frozen last frame in it is the worst thing this path can leave behind.
	a, _ := newTestApp(t)
	silencePump(a)
	_, ov := withFakePicture(a)

	if err := a.SetPictureRect(0, 0, 640, 360, 1); err != nil {
		t.Fatalf("SetPictureRect() error = %v", err)
	}
	a.picViewMu.Lock()
	a.picWantVisible = true
	a.picViewMu.Unlock()
	a.picStateMu.Lock()
	a.lastPicture = gst.PictureStateShowing
	a.picStateMu.Unlock()
	a.picViewMu.Lock()
	a.applyPictureVisibilityViewLocked()
	a.picViewMu.Unlock()

	if v, _ := ov.lastVisible(); !v {
		t.Fatal("the overlay is not visible; the precondition of this test does not hold")
	}

	// Now the monitor is gone but the state was never updated — which is what a
	// crash-stopped forwarder would leave behind.
	a.picStateMu.Lock()
	a.lastPicture = gst.PictureStateStopped
	a.picStateMu.Unlock()

	if err := a.StopPicture(); !errors.Is(err, errPictureNotRunning) {
		t.Fatalf("StopPicture() error = %v, want errPictureNotRunning", err)
	}
	if v, _ := ov.lastVisible(); v {
		t.Fatal("StopPicture() left the overlay visible over the page")
	}
}

// ---------------------------------------------------------------------------
// State forwarding
// ---------------------------------------------------------------------------

func TestPictureStatesReachTheFrontendAndTheGetter(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	mon, _ := withFakePicture(a)

	if err := a.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}
	mon.emit(gst.PictureStateShowing)

	waitForCond(t, "the getter to report SHOWING", func() bool {
		s, _ := a.GetPictureState()
		return s == gst.PictureStateShowing
	})

	if err := a.StopPicture(); err != nil {
		t.Fatalf("StopPicture() error = %v", err)
	}

	var sawShowing bool
	for _, e := range drainPump(a) {
		if e.name == EventPicture && e.data == gst.PictureStateShowing {
			sawShowing = true
		}
	}
	if !sawShowing {
		t.Fatal("the page was never told the picture was showing")
	}
}

func TestPictureDiagnosticIsEmittedOnceAfterThreeFailures(t *testing.T) {
	// One message, then quiet. A stream of toasts during a match buries the ones
	// that mean the FEED is off air, which is the argument app_return.go makes at
	// length and which applies here unchanged.
	a, _ := newTestApp(t)
	silencePump(a)
	mon, _ := withFakePicture(a)

	if err := a.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}
	for i := 0; i < 6; i++ {
		mon.emit(gst.PictureStateBackoff)
	}
	waitForCond(t, "six backoffs to be forwarded", func() bool {
		s, _ := a.GetPictureState()
		return s == gst.PictureStateBackoff
	})
	if err := a.StopPicture(); err != nil {
		t.Fatalf("StopPicture() error = %v", err)
	}

	diagnostics := 0
	for _, e := range drainPump(a) {
		if e.name != EventError {
			continue
		}
		if s, ok := e.data.(string); ok && strings.Contains(s, "failed to connect") {
			diagnostics++
		}
	}
	if diagnostics != 1 {
		t.Fatalf("the picture emitted %d diagnostics over six consecutive failures, want exactly 1",
			diagnostics)
	}
}

// ---------------------------------------------------------------------------
// Teardown
// ---------------------------------------------------------------------------

func TestTeardownStopsThePictureThenDestroysTheWindow(t *testing.T) {
	// THE ORDER IS FIXED AND IT IS THE ONLY ONE THAT WORKS. The pipeline is
	// rendering into the window; destroying the window first leaves
	// d3d11videosink presenting to a handle that no longer names anything, which
	// is a driver-dependent outcome and not one to find out about during a match.
	a, _ := newTestApp(t)
	silencePump(a)
	mon, ov := withFakePicture(a)

	if err := a.SetPictureRect(0, 0, 640, 360, 1); err != nil {
		t.Fatalf("SetPictureRect() error = %v", err)
	}
	if err := a.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}

	if err := a.stopPictureForTeardown(); err != nil {
		t.Fatalf("stopPictureForTeardown() error = %v", err)
	}

	mon.mu.Lock()
	stopped := mon.stopped
	mon.mu.Unlock()
	if !stopped {
		t.Error("teardown did not stop the monitor")
	}
	if ov.closeCount() != 1 {
		t.Errorf("the overlay window was closed %d times, want exactly 1", ov.closeCount())
	}
	if a.picOverlay != nil {
		t.Error("teardown left the overlay reference behind; a second teardown would close it again")
	}
}

func TestTeardownWithNoPictureStillClosesNothingAndSucceeds(t *testing.T) {
	// teardown calls this unconditionally, and a run where the operator never
	// opened the picture must not log a spurious failure.
	a, _ := newTestApp(t)
	silencePump(a)
	withFakePicture(a)

	if err := a.stopPictureForTeardown(); err != nil {
		t.Fatalf("stopPictureForTeardown() with nothing running error = %v, want nil", err)
	}
}

func TestTeardownReportsAnOverlayThatWouldNotClose(t *testing.T) {
	// The overlay's Close is bounded and returns an error saying it abandoned
	// its message thread. That error must reach the teardown log rather than
	// being swallowed: an abandoned window is the only thing in this whole
	// shutdown that stays on the operator's SCREEN.
	//
	// And it must arrive INSPECTABLE. errors.Join keeps errors.Is working
	// through it; a fmt.Errorf summary with %v would not, and the sentinel is
	// the only thing that tells teardownStep this step did not finish. See the
	// next test for what that decides.
	a, _ := newTestApp(t)
	silencePump(a)
	withFakePicture(a)

	stubborn := &stubbornOverlay{fakeOverlay: *newFakeOverlay()}
	a.picViewMu.Lock()
	a.picOverlay = stubborn
	a.picViewMu.Unlock()

	err := a.stopPictureForTeardown()
	if err == nil || !strings.Contains(err.Error(), "ABANDONED") {
		t.Fatalf("stopPictureForTeardown() error = %v, want the overlay's abandonment reported", err)
	}
	if !errors.Is(err, gst.ErrAbandonedThread) {
		t.Fatalf("stopPictureForTeardown() error = %v, which does not unwrap to gst.ErrAbandonedThread. "+
			"The join flattened it, and teardownStep cannot tell an abandoned message thread from a "+
			"step that finished with a complaint", err)
	}
}

// TestTeardownEndsTheProcessWhenTheOverlayAbandonedItsThread is the whole point
// of the sentinel, and the defect it closes is the shutdown hang coming back.
//
// gst.PictureOverlay.Close is the FIRST Close in this application that returns
// on a hang instead of hanging. teardownStep used to score any step that
// returned as finished, so an overlay that gave up on its message thread was
// counted as a clean stop: the abandoned count stayed at zero, teardown took its
// `n == 0` branch, hardExit was never called and the process left through
// ExitProcess — which terminates that thread wherever it is (inside
// user32!DestroyWindow, or gstd3d11's subclass procedure) and THEN runs
// DLL_PROCESS_DETACH for every loaded DLL under the loader lock, with GStreamer,
// WASAPI, D3D11 and COM all in that set. That is the operator's original bug:
// "the window closes, but in task manager I have to kill it."
//
// So the assertion is not that the error was logged. It is that the process was
// ended by the one exit a wedged media library cannot veto. See exit_windows.go.
func TestTeardownEndsTheProcessWhenTheOverlayAbandonedItsThread(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	withFakePicture(a)

	stubborn := &stubbornOverlay{fakeOverlay: *newFakeOverlay()}
	a.picViewMu.Lock()
	a.picOverlay = stubborn
	a.picViewMu.Unlock()

	exited := make(chan struct{}, 1)
	a.exitProcess = func() {
		select {
		case exited <- struct{}{}:
		default:
		}
	}

	// Nothing here is slow: the stubborn overlay answers at once. A teardown that
	// took a budget would mean the abandonment was detected by the TIMER, which
	// is the case that already worked and is not what this test is about.
	start := time.Now()
	a.teardown()
	if elapsed := time.Since(start); elapsed >= pictureStopBudget {
		t.Fatalf("teardown took %v, reaching the picture step's %v budget; this test must exercise the "+
			"step that RETURNS with a thread abandoned, not the step that overruns", elapsed, pictureStopBudget)
	}

	select {
	case <-exited:
	default:
		t.Fatal("the overlay abandoned its message thread, said so, and teardown still returned through " +
			"the ordinary exit. ExitProcess terminates that thread wherever it is and then runs " +
			"DLL_PROCESS_DETACH over it under the loader lock, with GStreamer, WASAPI, D3D11 and COM " +
			"loaded — which is the shutdown that leaves wslcomms.exe in Task Manager. A step that " +
			"abandoned a thread is not a step that finished")
	}
}

// stubbornOverlay is an overlay whose Close gave up on its message thread: it
// returns PROMPTLY, with the abandonment wrapped, exactly as the real one does
// when overlayCloseBudget expires. The prompt return is the point — it is what
// made this indistinguishable from success.
type stubbornOverlay struct{ fakeOverlay }

func (o *stubbornOverlay) Close() error {
	return fmt.Errorf("gst: overlay: the message thread did not stop and has been ABANDONED: %w",
		gst.ErrAbandonedThread)
}

// waitForCond polls cond until it holds or the deadline passes. The forwarding
// goroutine is not itself a channel, so there is nothing else to select on.
func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
