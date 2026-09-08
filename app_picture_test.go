//go:build dev || production || bindings

// Tests for the SRT picture's bound surface: the exclusivity guard, the state
// forwarding, Refresh, the reaping of a picture process that died, and the
// teardown order.
//
// WHAT THESE TESTS DO NOT REACH, stated plainly rather than left to be assumed:
// the monitor is a fake throughout, standing in for the picture PROCESS. The
// parent's half of that process — childPictureMonitor — is tested against a
// helper process in app_picture_child_test.go; the child's half, with a real
// window and a real pipeline, is not unit-tested anywhere. The preview's
// overlay is a fake too: a unit test that created a real HWND would need a
// message loop, a parent window and a desktop session, and CI has none of the
// three.
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

// withFakePicture wires a fake monitor into the app, in place of the picture
// process, and a fake overlay in place of the DeckLink preview's surface.
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

func TestStartPictureGivesTheMonitorTheProgrammePortAndNoWindow(t *testing.T) {
	// The monitor is the picture PROCESS. It makes its own window, so the
	// handle this side hands it must be zero — a non-zero one would be a window
	// of THIS process, which the child cannot render into — and the port must
	// be Output 1, src=pgm, the programme picture.
	a, _ := newTestApp(t)
	silencePump(a)
	mon, _ := withFakePicture(a)

	if err := a.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}
	defer a.StopPicture()

	opts, _ := mon.startedWith()
	if opts.WindowHandle != 0 {
		t.Fatalf("the monitor was given window handle 0x%x; the picture process makes its own window "+
			"and this process has none to give it", opts.WindowHandle)
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
// Refresh, and a picture process that died on its own
// ---------------------------------------------------------------------------

// withFreshFakePictures installs a pictureDial that hands out a NEW fake on
// every call and records them, so a test can see that Refresh built a second
// monitor rather than restarting the first.
func withFreshFakePictures(a *App) func() []*fakePictureMonitor {
	var mu sync.Mutex
	var made []*fakePictureMonitor
	a.pictureDial = func() gst.PictureMonitor {
		m := newFakePictureMonitor()
		mu.Lock()
		made = append(made, m)
		mu.Unlock()
		return m
	}
	a.overlayDial = func() (gst.PictureOverlay, error) { return newFakeOverlay(), nil }

	cfg := validConfig()
	cfg.ReturnSource = config.ReturnSourceWebRTC
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	return func() []*fakePictureMonitor {
		mu.Lock()
		defer mu.Unlock()
		return append([]*fakePictureMonitor(nil), made...)
	}
}

func TestRefreshPictureStopsTheOldMonitorAndStartsANewOne(t *testing.T) {
	// The operator's "the picture has frozen" button. It is a real remedy only
	// if it is a NEW monitor — a new process, a new GStreamer, a new socket, a
	// new decoder — and not the old one poked; so the old fake must be stopped
	// and a second fake must be the one running afterwards.
	a, _ := newTestApp(t)
	silencePump(a)
	made := withFreshFakePictures(a)

	if err := a.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}
	if err := a.RefreshPicture(); err != nil {
		t.Fatalf("RefreshPicture() error = %v", err)
	}
	defer a.StopPicture()

	mons := made()
	if len(mons) != 2 {
		t.Fatalf("Refresh built %d monitors in total, want 2: the old one stopped, a fresh one started", len(mons))
	}
	mons[0].mu.Lock()
	oldStopped := mons[0].stopped
	mons[0].mu.Unlock()
	if !oldStopped {
		t.Error("Refresh did not stop the old monitor; two picture processes would be dialling the same output")
	}
	if _, started := mons[1].startedWith(); !started {
		t.Error("Refresh did not start the fresh monitor")
	}
	if a.pic == nil || a.pic.mon != mons[1] {
		t.Error("the running session is not the fresh monitor")
	}

	// And its states are the ones the page now hears.
	mons[1].emit(gst.PictureStateShowing)
	waitForCond(t, "the fresh monitor's state to reach the getter", func() bool {
		s, _ := a.GetPictureState()
		return s == gst.PictureStateShowing
	})
}

func TestRefreshPictureStartsAPictureThatWasNotRunning(t *testing.T) {
	// Nothing to stop is not a failure: the operator pressed Refresh because
	// they want a picture, and "it was not running" is not a reason to deny
	// them one.
	a, _ := newTestApp(t)
	silencePump(a)
	made := withFreshFakePictures(a)

	if err := a.RefreshPicture(); err != nil {
		t.Fatalf("RefreshPicture() with nothing running error = %v", err)
	}
	defer a.StopPicture()

	if len(made()) != 1 {
		t.Fatalf("Refresh built %d monitors, want exactly 1", len(made()))
	}
	if a.pic == nil {
		t.Fatal("Refresh left no picture running")
	}
}

func TestStartPictureReapsASessionWhoseMonitorEnded(t *testing.T) {
	// The picture process can die without StopPicture — the operator closes
	// its window, or it crashes — and the parent learns of it only through the
	// states channel closing. A session in that state is NOT "already running":
	// the next StartPicture must reap it and start a fresh one, or the operator
	// is stuck with a dead picture and a button that says it is up.
	a, _ := newTestApp(t)
	silencePump(a)
	made := withFreshFakePictures(a)

	if err := a.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}
	first := made()[0]

	// The process dies: STOPPED, then the channel closes, with nobody having
	// called Stop. Marked stopped first so the fake's own Stop — which the reap
	// calls — does not send on the closed channel.
	first.mu.Lock()
	first.stopped = true
	first.mu.Unlock()
	first.emit(gst.PictureStateStopped)
	close(first.states)

	waitForCond(t, "the forwarder to notice the monitor ended", func() bool {
		a.picMu.Lock()
		defer a.picMu.Unlock()
		return a.pic != nil && a.pic.exited.Load()
	})
	waitForCond(t, "STOPPED to reach the getter", func() bool {
		s, _ := a.GetPictureState()
		return s == gst.PictureStateStopped
	})

	if err := a.StartPicture(); err != nil {
		t.Fatalf("StartPicture() after the picture process died error = %v; the dead session was "+
			"not reaped", err)
	}
	defer a.StopPicture()

	mons := made()
	if len(mons) != 2 {
		t.Fatalf("%d monitors were built, want 2", len(mons))
	}
	if a.pic == nil || a.pic.mon != mons[1] {
		t.Fatal("the running session is not the fresh monitor")
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

func TestPictureSoftwareDecodeNoteIsEmittedOnceAsANote(t *testing.T) {
	// A machine with no hardware HEVC decoder falls to avdec_h265 (software). The
	// operator is told ONCE, as a NOTE — grey and uncounted, because the picture
	// works and this only explains the higher CPU — and NOT on every StartPicture,
	// because the decoder's absence is a fixed property of the GPU. It is a "note"
	// event, not an "error", so the frontend never counts it as a fault.
	gst.SetStubPictureSoftwareDecoder("avdec_h265")
	defer gst.SetStubPictureSoftwareDecoder("")

	a, _ := newTestApp(t)
	silencePump(a)
	withFakePicture(a)

	// Two full start/stop cycles. The note must appear on the first and never
	// again — the once guard is the App's, for the life of the process.
	for i := 0; i < 2; i++ {
		if err := a.StartPicture(); err != nil {
			t.Fatalf("StartPicture() #%d error = %v", i+1, err)
		}
		if err := a.StopPicture(); err != nil {
			t.Fatalf("StopPicture() #%d error = %v", i+1, err)
		}
	}

	notes := 0
	for _, e := range drainPump(a) {
		if e.name != EventNote {
			continue
		}
		notes++
		s, ok := e.data.(string)
		if !ok {
			t.Fatalf("the %q event carried %T, want a string", EventNote, e.data)
		}
		for _, want := range []string{"software", "avdec_h265"} {
			if !strings.Contains(s, want) {
				t.Errorf("the software-decode note %q does not name %q", s, want)
			}
		}
	}
	if notes != 1 {
		t.Fatalf("the picture emitted %d software-decode notes over two starts, want exactly 1", notes)
	}
}

func TestPictureSaysNothingWhenTheDecoderIsHardware(t *testing.T) {
	// The default stub answer is hardware. No note: a machine that decodes on the
	// GPU must not be nagged about a software path it is not on.
	gst.SetStubPictureSoftwareDecoder("")

	a, _ := newTestApp(t)
	silencePump(a)
	withFakePicture(a)

	if err := a.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}
	if err := a.StopPicture(); err != nil {
		t.Fatalf("StopPicture() error = %v", err)
	}

	for _, e := range drainPump(a) {
		if e.name == EventNote {
			t.Fatalf("a hardware-decode machine emitted a note: %v", e.data)
		}
	}
}

// ---------------------------------------------------------------------------
// Teardown
// ---------------------------------------------------------------------------

func TestTeardownStopsThePictureThenClosesThePreviewSurface(t *testing.T) {
	// The picture step of the ordered shutdown ends the picture process and,
	// as a belt, takes the preview surface off the screen — the capture step
	// that normally does that may have been abandoned rather than waited for.
	a, _ := newTestApp(t)
	silencePump(a)
	mon, ov := withFakePicture(a)

	if err := a.StartPicture(); err != nil {
		t.Fatalf("StartPicture() error = %v", err)
	}
	a.prevViewMu.Lock()
	a.prevOverlay = ov
	a.prevRunning = true
	a.prevViewMu.Unlock()

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
		t.Errorf("the preview surface was closed %d times, want exactly 1", ov.closeCount())
	}
	a.prevViewMu.Lock()
	left := a.prevOverlay
	a.prevViewMu.Unlock()
	if left != nil {
		t.Error("teardown left the preview surface reference behind; a second teardown would close it again")
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
	// The preview surface's Close is bounded and returns an error saying it
	// abandoned its message thread. That error must reach the teardown log
	// rather than being swallowed: an abandoned window is the only thing in
	// this whole shutdown that stays on the operator's SCREEN.
	//
	// And it must arrive INSPECTABLE. errors.Join keeps errors.Is working
	// through it; a fmt.Errorf summary with %v would not, and the sentinel is
	// the only thing that tells teardownStep this step did not finish. See the
	// next test for what that decides.
	a, _ := newTestApp(t)
	silencePump(a)
	withFakePicture(a)

	stubborn := &stubbornOverlay{fakeOverlay: *newFakeOverlay()}
	a.prevViewMu.Lock()
	a.prevOverlay = stubborn
	a.prevViewMu.Unlock()

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
// returned as finished, so a surface that gave up on its message thread was
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
	a.prevViewMu.Lock()
	a.prevOverlay = stubborn
	a.prevViewMu.Unlock()

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

// ---------------------------------------------------------------------------
// The picture's SRT latency
// ---------------------------------------------------------------------------

// TestPictureLatencyComesFromItsOwnFieldAndNotTheFeeds is the regression test
// for the operator's report that the picture ran about a second behind.
//
// pictureOpts read cfg.SRTLatencyMs — the CONTRIBUTION FEED's retransmission
// budget — and handed it to the monitor. That is one number answering two
// questions that pull in opposite directions: the feed trades delay for not
// breaking up on air, where breaking up is unacceptable and delay is nearly
// free; the monitor trades the other way round. While they were the same field,
// the only way to make the commentator's picture quicker was to thin the
// protection on the match going out.
func TestPictureLatencyComesFromItsOwnFieldAndNotTheFeeds(t *testing.T) {
	a, _ := newTestApp(t)

	cfg := config.Defaults()
	cfg.M2LXHost = "m2lx.example.com"
	cfg.SRTLatencyMs = 2000   // the feed going to air: heavily protected
	cfg.PictureLatencyMs = 40 // the commentator's monitor: as quick as it goes

	got := a.pictureOpts(cfg, "")
	if got.LatencyMs != 40 {
		t.Fatalf("the monitor was given LatencyMs = %d, want 40. It is reading the "+
			"contribution feed's srtLatencyMs (%d) again, which is how the picture came "+
			"to be a second behind the match", got.LatencyMs, cfg.SRTLatencyMs)
	}
}

// TestPictureLatencyFallsBackToTheDefaultOnAnOldConfig is the upgrade path.
//
// Every config.json written before pictureLatencyMs existed decodes with 0 in
// it. Zero must not reach srtsrc: it disables the retransmission window
// entirely, so a single lost packet on an unprotected internet path is a
// visible tear. EffectivePictureLatencyMs is what makes that true, and this
// asserts pictureOpts actually calls it rather than reading the field raw.
func TestPictureLatencyFallsBackToTheDefaultOnAnOldConfig(t *testing.T) {
	a, _ := newTestApp(t)

	cfg := config.Defaults()
	cfg.M2LXHost = "m2lx.example.com"
	cfg.PictureLatencyMs = 0 // what an upgraded installation has on disk

	got := a.pictureOpts(cfg, "")
	if got.LatencyMs != config.DefaultPictureLatencyMs {
		t.Fatalf("the monitor was given LatencyMs = %d on a config with no picture latency, "+
			"want the default %d; pictureOpts is reading the field raw instead of through "+
			"EffectivePictureLatencyMs", got.LatencyMs, config.DefaultPictureLatencyMs)
	}
}

// TestPictureLatencyDefaultMatchesGst pins the two constants that must agree.
//
// internal/config deliberately does not import internal/gst — a configuration
// package that cannot be tested without GStreamer stops being tested — so the
// default is stated twice. This package imports both and is the only place the
// pair can be compared.
//
// If they drift, nothing fails: gst.PictureOpts.normalise substitutes its own
// figure for a zero it never receives, because config already substituted a
// different one. The operator gets a latency neither file documents.
func TestPictureLatencyDefaultMatchesGst(t *testing.T) {
	if config.DefaultPictureLatencyMs != gst.DefaultPictureLatencyMs {
		t.Fatalf("config.DefaultPictureLatencyMs = %d but gst.DefaultPictureLatencyMs = %d. "+
			"They are restated rather than shared because internal/config must not depend on "+
			"the cgo package; restated means they have to be kept level here.",
			config.DefaultPictureLatencyMs, gst.DefaultPictureLatencyMs)
	}
}
