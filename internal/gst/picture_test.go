// Tests for the SRT picture path's pure-Go half: the option rules, the
// rectangle arithmetic and the reconnect state machine.
//
// This file carries NO build tag on purpose, like return_test.go and for the
// same reason. Everything it exercises lives in picture.go, which is pure Go in
// both builds, so these tests run at Gate A against the stub AND at Gate B
// against the real implementation — where they are the only automated check on
// this path that a machine with GStreamer installed will run. The pipeline
// itself cannot be unit-tested without GStreamer and a GPU, which is exactly why
// so much was kept out of it.
//
// WHAT THESE TESTS CANNOT REACH is stated here rather than left to be
// discovered: nothing below creates a window, hands a handle to
// d3d11videosink, or decodes a frame. The overlay in overlay_windows.go is
// exercised only through its interface, by the fake in app_picture_test.go. See
// the report for what is therefore still unproven.
package gst

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Options
// ---------------------------------------------------------------------------

func TestPictureOptsNormaliseFillsTheDocumentedDefaults(t *testing.T) {
	opts := PictureOpts{Host: "m2lx.example.com", Port: 40501, WindowHandle: 0x1234}
	if err := opts.normalise(); err != nil {
		t.Fatalf("normalise() error = %v", err)
	}
	if opts.LatencyMs != DefaultPictureLatencyMs {
		t.Errorf("LatencyMs = %d, want the documented default %d", opts.LatencyMs, DefaultPictureLatencyMs)
	}
	if opts.PBKeyLen != 0 {
		t.Errorf("PBKeyLen = %d, want 0 with no passphrase: an unencrypted output is the measured "+
			"case for port 40501 and must not be turned into an encrypted one by defaulting",
			opts.PBKeyLen)
	}
}

func TestPictureOptsNormaliseDefaultsPBKeyLenOnlyWithAPassphrase(t *testing.T) {
	opts := PictureOpts{Host: "h", Port: 40501, Passphrase: "s3cret", WindowHandle: 1}
	if err := opts.normalise(); err != nil {
		t.Fatalf("normalise() error = %v", err)
	}
	if opts.PBKeyLen != 16 {
		t.Errorf("PBKeyLen = %d, want 16: a passphrase with no key length is AES-128, which is "+
			"libsrt's own default and what the other two SRT paths do", opts.PBKeyLen)
	}
}

func TestPictureOptsNormaliseRefusesAKeyLengthWithNoPassphrase(t *testing.T) {
	// This is the exact fault that cost the operator an afternoon on the return
	// path: an encrypted session with nothing to encrypt with, which libsrt
	// refuses for ever, silently. It must be a refusal at Start, naming the
	// problem, and not a ladder that climbs in the dark.
	opts := PictureOpts{Host: "h", Port: 40501, PBKeyLen: 16, WindowHandle: 1}
	err := opts.normalise()
	if err == nil {
		t.Fatal("normalise() accepted a key length with no passphrase")
	}
	if !strings.Contains(err.Error(), "no passphrase") {
		t.Errorf("error %q does not say what is missing", err)
	}
}

func TestPictureOptsNormaliseRefusesAMissingWindow(t *testing.T) {
	// A zero handle makes d3d11videosink open its OWN top-level window on the
	// operator's screen, mid-match, outside the layout, with its own close
	// button. Refusing here is far better than discovering it.
	opts := PictureOpts{Host: "h", Port: 40501}
	err := opts.normalise()
	if err == nil {
		t.Fatal("normalise() accepted a zero window handle")
	}
	if !strings.Contains(err.Error(), "window handle") {
		t.Errorf("error %q does not name the window handle", err)
	}
}

func TestPictureOptsNormaliseReportsEveryProblemAtOnce(t *testing.T) {
	// One edit-fail cycle per bad field is how a first run takes twenty minutes.
	opts := PictureOpts{Host: "  ", Port: 70000, LatencyMs: -1, PBKeyLen: 7}
	err := opts.normalise()
	if err == nil {
		t.Fatal("normalise() accepted a configuration with four problems")
	}
	msg := err.Error()
	for _, want := range []string{"host", "port", "latency", "key length", "window handle"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q; the operator sees one problem at a time", msg, want)
		}
	}
}

func TestPictureOptsNeverLogsThePassphrase(t *testing.T) {
	// endpointForLog exists so that nothing is ever tempted to format the struct
	// as a whole. If it ever grows a %+v this test is what catches it before a
	// support bundle does.
	opts := PictureOpts{Host: "m2lx.example.com", Port: 40501, Passphrase: "hunter2-do-not-log"}
	if got := opts.endpointForLog(); strings.Contains(got, "hunter2") {
		t.Fatalf("endpointForLog() = %q, which contains the passphrase", got)
	}
	if want := "srt://m2lx.example.com:40501"; opts.endpointForLog() != want {
		t.Fatalf("endpointForLog() = %q, want %q", opts.endpointForLog(), want)
	}
}

// ---------------------------------------------------------------------------
// The rectangle
// ---------------------------------------------------------------------------

func TestScaleRectAtEveryRatioTheOperatorCanProduce(t *testing.T) {
	// The operator runs a 3840x2088 window. 1.0, 1.25, 1.5 and 2.0 are the
	// Windows display scales that reach it; the rest are what a WebView2 zoom
	// produces.
	cases := []struct {
		name                 string
		x, y, w, h, ratio    float64
		wantX, wantY, wW, wH int
	}{
		{"unscaled", 16, 80, 960, 540, 1.0, 16, 80, 960, 540},
		{"125 percent", 16, 80, 960, 540, 1.25, 20, 100, 1200, 675},
		{"150 percent", 16, 80, 960, 540, 1.5, 24, 120, 1440, 810},
		{"200 percent", 16, 80, 960, 540, 2.0, 32, 160, 1920, 1080},
		{"awkward ratio", 10.5, 20.5, 100, 200, 1.5, 16, 31, 150, 300},
		{"origin", 0, 0, 1920, 1080, 1.0, 0, 0, 1920, 1080},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ScaleRect(c.x, c.y, c.w, c.h, c.ratio)
			want := PictureRect{X: c.wantX, Y: c.wantY, W: c.wW, H: c.wH}
			if got != want {
				t.Fatalf("ScaleRect = %v, want %v", got, want)
			}
		})
	}
}

func TestScaleRectKeepsTheFarEdgeWhereThePagePutIt(t *testing.T) {
	// The seam test. Rounding the position and the size independently loses up
	// to a pixel per edge, which at 1.5x is a visible one-pixel line between the
	// picture and whatever the page draws next to it — on a black background, at
	// the top of a commentary window, for ever.
	//
	// Two rectangles that ABUT in CSS pixels must still abut in physical ones.
	const ratio = 1.5
	top := ScaleRect(0, 0, 400, 225.5, ratio)
	below := ScaleRect(0, 225.5, 400, 100, ratio)

	if top.Y+top.H != below.Y {
		t.Fatalf("the rectangles do not abut: top ends at %d, below starts at %d; "+
			"there is a %d physical pixel seam", top.Y+top.H, below.Y, below.Y-(top.Y+top.H))
	}
}

func TestScaleRectSurvivesANonsenseRatio(t *testing.T) {
	// The ratio arrives from JavaScript across the Wails boundary. A NaN
	// propagated through the multiplication into an int conversion is
	// implementation-defined in Go, and a picture in the wrong place that the
	// operator can see and report beats an undefined one.
	for _, ratio := range []float64{0, -1, 1e9} {
		got := ScaleRect(10, 20, 100, 50, ratio)
		if got != (PictureRect{X: 10, Y: 20, W: 100, H: 50}) {
			t.Fatalf("ScaleRect with ratio %v = %v, want the CSS pixels unchanged", ratio, got)
		}
	}
}

func TestPictureRectEmptyIsAHideNotAnError(t *testing.T) {
	for _, r := range []PictureRect{
		{},
		{X: 10, Y: 10, W: 0, H: 100},
		{X: 10, Y: 10, W: 100, H: 0},
		{W: -5, H: -5},
	} {
		if !r.Empty() {
			t.Errorf("%v.Empty() = false, want true: a page that has not laid out yet produces one "+
				"of these and it must not be an error", r)
		}
	}
	if (PictureRect{W: 1, H: 1}).Empty() {
		t.Error("a one-pixel rectangle is not empty")
	}
}

// ---------------------------------------------------------------------------
// The ladder
// ---------------------------------------------------------------------------

func TestPictureBackoffLadderFirstRungClearsTheReacceptWindow(t *testing.T) {
	// M2L-X's SRT listener accepts one peer, never displaces the incumbent, and
	// refuses re-accept for roughly five seconds after that peer goes away. A
	// first rung inside that window is an attempt that is refused before it is
	// made. Six seconds is the measured floor; the ladder starts at seven.
	if PictureBackoffLadder[0] < 6*time.Second {
		t.Fatalf("the first rung is %v, inside M2L-X's roughly five second re-accept refusal window",
			PictureBackoffLadder[0])
	}
}

func TestPictureBackoffLadderWalksThenCaps(t *testing.T) {
	for i, want := range PictureBackoffLadder {
		if got := pictureBackoffDelay(i); got != want {
			t.Errorf("pictureBackoffDelay(%d) = %v, want %v", i, got, want)
		}
	}
	for _, attempt := range []int{len(PictureBackoffLadder), len(PictureBackoffLadder) + 500} {
		if got := pictureBackoffDelay(attempt); got != PictureBackoffCap {
			t.Errorf("pictureBackoffDelay(%d) = %v, want the cap %v", attempt, got, PictureBackoffCap)
		}
	}
}

func TestPictureBackoffDelayClampsANegativeAttempt(t *testing.T) {
	// It cannot arise from the machine's own bookkeeping. It is clamped rather
	// than allowed to panic because a panic in the process carrying the
	// commentary is worse than any wait.
	if got := pictureBackoffDelay(-3); got != PictureBackoffLadder[0] {
		t.Fatalf("pictureBackoffDelay(-3) = %v, want the first rung %v", got, PictureBackoffLadder[0])
	}
}

// ---------------------------------------------------------------------------
// The reconnect machine
// ---------------------------------------------------------------------------

type pictureHarness struct {
	mu    sync.Mutex
	log   []string
	pipes []*fakePicturePipe
	waits []time.Duration

	newPipeErr      error
	failPlays       int
	closeErrsOnPlay bool
	release         chan time.Time
}

func newPictureHarness() *pictureHarness { return &pictureHarness{} }

func (h *pictureHarness) record(s string) {
	h.mu.Lock()
	h.log = append(h.log, s)
	h.mu.Unlock()
}

func (h *pictureHarness) events() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.log...)
}

func (h *pictureHarness) waitDurations() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.waits...)
}

func (h *pictureHarness) pipeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.pipes)
}

func (h *pictureHarness) lastPipe() *fakePicturePipe {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.pipes) == 0 {
		return nil
	}
	return h.pipes[len(h.pipes)-1]
}

func (h *pictureHarness) newPipe() (picturePipe, error) {
	h.record("new")
	h.mu.Lock()
	if err := h.newPipeErr; err != nil {
		h.mu.Unlock()
		return nil, err
	}
	p := &fakePicturePipe{h: h, errs: make(chan error, 4), closeOnPlay: h.closeErrsOnPlay}
	if h.failPlays > 0 {
		h.failPlays--
		p.playErr = fmt.Errorf("fake: connect refused")
	}
	h.pipes = append(h.pipes, p)
	h.mu.Unlock()
	return p, nil
}

func (h *pictureHarness) newTimer(d time.Duration) (<-chan time.Time, func()) {
	h.record("wait")
	h.mu.Lock()
	h.waits = append(h.waits, d)
	rel := h.release
	h.mu.Unlock()

	if rel != nil {
		return rel, func() {}
	}
	ch := make(chan time.Time, 1)
	ch <- time.Now()
	return ch, func() {}
}

type fakePicturePipe struct {
	h           *pictureHarness
	closeOnPlay bool

	mu       sync.Mutex
	playErr  error
	errs     chan error
	closed   bool
	lastOpts PictureOpts
	plays    int
	closes   int
}

func (p *fakePicturePipe) Play(opts PictureOpts) error {
	p.h.record("play")
	p.mu.Lock()
	defer p.mu.Unlock()
	p.plays++
	p.lastOpts = opts
	if err := p.playErr; err != nil {
		return err
	}
	if p.closeOnPlay && !p.closed {
		p.closed = true
		close(p.errs)
	}
	return nil
}

func (p *fakePicturePipe) Errors() <-chan error { return p.errs }

func (p *fakePicturePipe) Close() error {
	p.h.record("close")
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closes++
	if !p.closed {
		p.closed = true
		close(p.errs)
	}
	return nil
}

func (p *fakePicturePipe) inject(err error) bool {
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

func (p *fakePicturePipe) counts() (plays, closes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.plays, p.closes
}

func (p *fakePicturePipe) opts() PictureOpts {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastOpts
}

var _ picturePipe = (*fakePicturePipe)(nil)
var _ pictureClock = (*pictureHarness)(nil)

// validPictureOpts is a configuration that normalise accepts.
func validPictureOpts() PictureOpts {
	return PictureOpts{
		Host:         "m2lx.example.com",
		Port:         40501,
		WindowHandle: 0x00CAFE00,
	}
}

// expectPictureState reads one state, failing the test if none arrives.
func expectPictureState(t *testing.T, states <-chan PictureState, want PictureState) {
	t.Helper()
	select {
	case got, ok := <-states:
		if !ok {
			t.Fatalf("states channel closed while waiting for %q", want)
		}
		if got != want {
			t.Fatalf("state = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for state %q", want)
	}
}

func TestPictureMonitorReachesShowing(t *testing.T) {
	h := newPictureHarness()
	m := newPictureMonitor(h.newPipe, h)

	if err := m.Start(validPictureOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectPictureState(t, m.States(), PictureStateConnecting)
	expectPictureState(t, m.States(), PictureStateShowing)

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	expectPictureState(t, m.States(), PictureStateStopped)

	if _, ok := <-m.States(); ok {
		t.Fatal("the states channel is still open after Stop; a monitor is single-use")
	}
}

func TestPictureMonitorPassesTheOptionsThroughNormalised(t *testing.T) {
	// The pipeline must be given the DEFAULTED options, not the raw ones. Start
	// normalises its own copy and the monitor carries that copy; a pipeline
	// handed a zero latency would use srtsrc's element default instead of the
	// 120 ms this path chose.
	h := newPictureHarness()
	m := newPictureMonitor(h.newPipe, h)

	opts := validPictureOpts()
	opts.LatencyMs = 0
	if err := m.Start(opts); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectPictureState(t, m.States(), PictureStateConnecting)
	expectPictureState(t, m.States(), PictureStateShowing)
	defer m.Stop()

	got := h.lastPipe().opts()
	if got.LatencyMs != DefaultPictureLatencyMs {
		t.Errorf("the pipeline was given LatencyMs = %d, want the normalised default %d",
			got.LatencyMs, DefaultPictureLatencyMs)
	}
	if got.WindowHandle != opts.WindowHandle {
		t.Errorf("the pipeline was given window handle 0x%x, want the one the overlay produced, 0x%x",
			got.WindowHandle, opts.WindowHandle)
	}
}

func TestPictureMonitorClosesThePipelineBeforeItWaits(t *testing.T) {
	// THE ORDER IS THE POINT. M2L-X refuses re-accept for roughly five seconds
	// after the incumbent peer goes away, so the backoff only clears that window
	// if our socket is already gone when it starts ticking. Close must appear
	// before wait in the log on every failure path, with nothing between them.
	h := newPictureHarness()
	h.failPlays = 1
	m := newPictureMonitor(h.newPipe, h)

	if err := m.Start(validPictureOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectPictureState(t, m.States(), PictureStateConnecting)
	expectPictureState(t, m.States(), PictureStateBackoff)
	expectPictureState(t, m.States(), PictureStateConnecting)
	expectPictureState(t, m.States(), PictureStateShowing)
	defer m.Stop()

	events := h.events()
	closeAt, waitAt := -1, -1
	for i, e := range events {
		if e == "close" && closeAt < 0 {
			closeAt = i
		}
		if e == "wait" && waitAt < 0 {
			waitAt = i
		}
	}
	if closeAt < 0 || waitAt < 0 {
		t.Fatalf("expected a close and a wait in %v", events)
	}
	if closeAt > waitAt {
		t.Fatalf("the pipeline was closed AFTER the backoff wait started (%v); our SRT socket would "+
			"still be the incumbent for the whole of the wait and the retry would be refused", events)
	}
	if waitAt != closeAt+1 {
		t.Fatalf("something happened between the close and the wait: %v", events)
	}
}

func TestPictureMonitorWalksTheLadderAndNeverGivesUp(t *testing.T) {
	h := newPictureHarness()
	h.failPlays = len(PictureBackoffLadder) + 3
	m := newPictureMonitor(h.newPipe, h)

	if err := m.Start(validPictureOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitFor(t, "the ladder to be exhausted and capped twice", func() bool {
		return len(h.waitDurations()) >= len(PictureBackoffLadder)+2
	})
	defer m.Stop()

	waits := h.waitDurations()
	for i, want := range PictureBackoffLadder {
		if waits[i] != want {
			t.Errorf("wait %d = %v, want %v", i, waits[i], want)
		}
	}
	for i := len(PictureBackoffLadder); i < len(PictureBackoffLadder)+2; i++ {
		if waits[i] != PictureBackoffCap {
			t.Errorf("wait %d = %v, want the cap %v", i, waits[i], PictureBackoffCap)
		}
	}
}

func TestPictureMonitorResetsTheLadderAfterASuccessfulConnect(t *testing.T) {
	// A thirty second blip in the first half must not leave the picture waiting
	// thirty seconds after every drop for the rest of the match.
	h := newPictureHarness()
	h.failPlays = 3
	m := newPictureMonitor(h.newPipe, h)

	if err := m.Start(validPictureOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitFor(t, "three failures then a connect", func() bool {
		return h.pipeCount() >= 4
	})
	expectPictureState(t, m.States(), PictureStateConnecting)

	// Drop the working one.
	waitFor(t, "the fourth pipe to be playing", func() bool {
		plays, _ := h.lastPipe().counts()
		return plays > 0
	})
	if !h.lastPipe().inject(errors.New("fake: the SRT peer went away")) {
		t.Fatal("could not inject a mid-match failure")
	}
	waitFor(t, "the wait after the drop", func() bool {
		return len(h.waitDurations()) >= 4
	})
	defer m.Stop()

	waits := h.waitDurations()
	if waits[3] != PictureBackoffLadder[0] {
		t.Fatalf("the wait after a mid-match drop was %v, want the first rung %v: a successful "+
			"connect resets the ladder", waits[3], PictureBackoffLadder[0])
	}
}

func TestPictureMonitorTreatsAClosedErrorChannelAsFailure(t *testing.T) {
	// Only Close closes that channel, and Close has not been called, so a closed
	// channel means the pipeline gave up without saying why. Treating it as
	// healthy would leave a frozen frame on screen with nothing anywhere saying
	// the feed had stopped — which a commentator cannot tell from a still one.
	h := newPictureHarness()
	h.closeErrsOnPlay = true
	m := newPictureMonitor(h.newPipe, h)

	if err := m.Start(validPictureOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectPictureState(t, m.States(), PictureStateConnecting)
	expectPictureState(t, m.States(), PictureStateShowing)
	expectPictureState(t, m.States(), PictureStateBackoff)
	m.Stop()
}

func TestPictureMonitorRetriesWhenThePipelineCannotEvenBeBuilt(t *testing.T) {
	// In practice: the d3d11 plugin is not in the bundle. Retrying will not fix
	// that, but it costs nothing, the reason is logged every time, and the
	// alternative is a picture dead for the rest of the match because the bundle
	// was repaired while it was running.
	h := newPictureHarness()
	h.newPipeErr = errors.New("fake: d3d11h265dec is missing")
	m := newPictureMonitor(h.newPipe, h)

	if err := m.Start(validPictureOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitFor(t, "three build attempts", func() bool {
		return len(h.waitDurations()) >= 3
	})
	m.Stop()

	for _, e := range h.events() {
		if e == "play" {
			t.Fatal("a pipeline that could not be built was played")
		}
	}
}

func TestPictureMonitorStopIsPromptFromTheMiddleOfABackoff(t *testing.T) {
	// The operator pressing Stop and being ignored for thirty seconds is its own
	// kind of fault. The wait is cancelled rather than served.
	h := newPictureHarness()
	h.failPlays = 100
	h.release = make(chan time.Time) // never fires
	m := newPictureMonitor(h.newPipe, h)

	if err := m.Start(validPictureOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitFor(t, "the machine to reach a backoff wait", func() bool {
		return len(h.waitDurations()) >= 1
	})

	done := make(chan error, 1)
	go func() { done <- m.Stop() }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return from the middle of a backoff wait")
	}
}

func TestPictureMonitorStopClosesTheRunningPipeline(t *testing.T) {
	h := newPictureHarness()
	m := newPictureMonitor(h.newPipe, h)

	if err := m.Start(validPictureOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectPictureState(t, m.States(), PictureStateConnecting)
	expectPictureState(t, m.States(), PictureStateShowing)

	pipe := h.lastPipe()
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if _, closes := pipe.counts(); closes == 0 {
		t.Fatal("Stop() left the pipeline open; the SRT socket and the D3D11 device would stay held")
	}
}

func TestPictureMonitorRejectsAConfigurationItCannotUse(t *testing.T) {
	// A configuration that can never work is an error from Start, because it
	// will not fix itself and the operator is standing in front of the settings
	// screen. A connection failure is NOT, because it might.
	h := newPictureHarness()
	m := newPictureMonitor(h.newPipe, h)

	if err := m.Start(PictureOpts{Port: 40501, WindowHandle: 1}); err == nil {
		t.Fatal("Start() accepted a configuration with no host")
	}
	if h.pipeCount() != 0 {
		t.Fatal("Start() built a pipeline for a configuration it should have refused")
	}
	// And it did not mark itself started, so a corrected Start still works.
	if err := m.Start(validPictureOpts()); err != nil {
		t.Fatalf("Start() after a rejected configuration error = %v", err)
	}
	m.Stop()
}

func TestPictureMonitorStartAndStopBookkeeping(t *testing.T) {
	h := newPictureHarness()
	m := newPictureMonitor(h.newPipe, h)

	if err := m.Stop(); !errors.Is(err, ErrPictureNotStarted) {
		t.Fatalf("Stop() before Start error = %v, want ErrPictureNotStarted", err)
	}
	if err := m.Start(validPictureOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := m.Start(validPictureOpts()); !errors.Is(err, ErrPictureAlreadyStarted) {
		t.Fatalf("second Start() error = %v, want ErrPictureAlreadyStarted", err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("second Stop() error = %v, want it to be idempotent", err)
	}
}

func TestPictureAndReturnDoNotShareTheirLadders(t *testing.T) {
	// They are the same shape today and they are allowed to diverge. This is
	// here so that changing one and expecting the other to follow fails loudly
	// rather than quietly: they are separate variables on purpose, and a future
	// decision to lengthen the picture's latency tolerance must be made in one
	// place and not inherited in another.
	if &PictureBackoffLadder[0] == &ReturnBackoffLadder[0] {
		t.Fatal("the picture and return ladders are the same slice; they must be independent")
	}
}
