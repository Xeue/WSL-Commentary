//go:build windows

// Tests for the parts of overlay_windows.go that can be exercised without
// creating a window.
//
// # What this file can and cannot reach, stated so nobody assumes more
//
// It does NOT create the overlay. NewPictureOverlay needs a visible top-level
// window of this process to be a child of, and a `go test` binary has none, so
// everything from CreateWindowEx onwards — the message pump, SetWindowPos,
// ShowWindow, DestroyWindow, and every interaction with gstd3d11's subclass —
// is unproven here and is reported as such.
//
// What it DOES reach is the pair of things that fail by PANICKING at run time
// rather than by failing to compile, and which are therefore the two most
// dangerous unexercised lines in the package:
//
//   - syscall.NewCallback refuses a function whose arguments are not
//     uintptr-sized, and it refuses it by panicking, at the moment of first
//     use. Both callbacks here are created lazily — one under a sync.Once when
//     the class is registered, one under a sync.Once on the first window
//     search — so without this file the first thing that would exercise either
//     is a commentator opening the picture during a match.
//
//   - RegisterClassExW takes a struct whose layout must match the C one
//     exactly. A wrong field order or a wrong cbSize is not a compile error; it
//     is a zero return and an error code, which is what this asserts against.
package gst

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestOverlayWindowIsCreatedWithNoParentNotify(t *testing.T) {
	// WS_EX_NOPARENTNOTIFY, and the reason it is worth a test of its own is that
	// it looks like a bit somebody set for tidiness and is in fact the whole
	// difference between an ordinary quit and a teardown that abandons the
	// picture every time.
	//
	// Without it Windows SENDS the parent a WM_PARENTNOTIFY on create and on
	// destroy. The parent is the Wails top-level window, owned by another thread,
	// and a cross-thread SendMessage blocks with no timeout until that thread
	// pumps. During teardown the Wails thread is inside OnShutdown → teardown,
	// parked in a select and NOT pumping — so DestroyWindow blocks for the whole
	// shutdown, the picture step overruns pictureStopBudget and is abandoned.
	//
	// The window itself is unreachable from here (see the file header), so what
	// is asserted is the style word CreateWindowExW is given, plus the fact that
	// the call site still uses it. Both halves are needed: the constant is where
	// the bit gets dropped and the call site is where the constant gets bypassed.
	if wsExNoParentNotify != 0x00000004 {
		t.Fatalf("wsExNoParentNotify = 0x%08x, want 0x00000004 (WS_EX_NOPARENTNOTIFY)", wsExNoParentNotify)
	}
	if overlayExStyle&wsExNoParentNotify == 0 {
		t.Fatalf("overlayExStyle = 0x%08x and does not contain WS_EX_NOPARENTNOTIFY. Every DestroyWindow "+
			"on this overlay will now SendMessage the Wails window, whose thread is inside teardown and "+
			"is not pumping, and the picture step will be abandoned on an ordinary quit", overlayExStyle)
	}

	src, err := os.ReadFile("overlay_windows.go")
	if err != nil {
		t.Fatalf("reading overlay_windows.go: %v", err)
	}
	// The first argument of CreateWindowExW is dwExStyle. It was a literal 0
	// once; a change back to a literal would leave the constant above correct and
	// the window wrong, which no value-level assertion can see.
	call := regexp.MustCompile(`procCreateWindowExW\.Call\(\s*\n\s*([A-Za-z0-9_|]+),`)
	m := call.FindSubmatch(src)
	if m == nil {
		t.Fatal("could not find the CreateWindowExW call site to check its dwExStyle argument")
	}
	if !strings.Contains(string(m[1]), "overlayExStyle") {
		t.Fatalf("CreateWindowExW is passed dwExStyle = %q, not overlayExStyle; "+
			"WS_EX_NOPARENTNOTIFY is not reaching the window", m[1])
	}
}

func TestOverlayStyleKeepsTheChildDisabledAndClipping(t *testing.T) {
	// The other three bits, so that naming the style word did not quietly drop
	// one of them. wsDisabled is the one with teeth: an enabled native child can
	// take the keyboard focus off the WebView by being clicked, which is exactly
	// what somebody does to a picture they want to look at.
	for _, bit := range []struct {
		name string
		want uintptr
	}{
		{"WS_CHILD", wsChild},
		{"WS_CLIPSIBLINGS", wsClipSiblings},
		{"WS_CLIPCHILDREN", wsClipChildren},
		{"WS_DISABLED", wsDisabled},
	} {
		if overlayStyle&bit.want == 0 {
			t.Errorf("overlayStyle = 0x%08x is missing %s (0x%08x)", overlayStyle, bit.name, bit.want)
		}
	}
	// And NOT visible: the window is shown by SetVisible, never at creation.
	const wsVisible = 0x10000000
	if overlayStyle&wsVisible != 0 {
		t.Error("overlayStyle has WS_VISIBLE: the overlay would appear at 0x0 before the page has " +
			"measured a rectangle for it")
	}
}

func TestOverlayCloseMarksAnAbandonedThreadWithTheSentinel(t *testing.T) {
	// Close is bounded, and when the bound is reached it RETURNS — which is the
	// property that makes the whole shutdown safe to drive, and also the property
	// that makes this error easy to mistake for success. App.teardownStep counts
	// a step that returns as finished unless it can see WHY it returned, so the
	// abandonment has to be an errors.Is-able value and not just a word in a
	// sentence. Without it teardown's abandoned count stays at zero, hardExit is
	// never called, and the process leaves through ExitProcess with this thread
	// killed mid-DestroyWindow and DLL_PROCESS_DETACH still to run.
	//
	// o.done is never closed and no window exists, so Close takes the timer
	// branch. That costs overlayCloseBudget in real time and is the only way to
	// reach the branch at all.
	o := &overlay{created: make(chan error, 1), done: make(chan struct{})}

	start := time.Now()
	err := o.Close()
	if err == nil {
		t.Fatal("Close() on an overlay whose pump never answers returned nil; the caller cannot " +
			"tell an abandoned thread from a clean close")
	}
	if elapsed := time.Since(start); elapsed < overlayCloseBudget {
		t.Fatalf("Close() gave up after %v, before its %v budget", elapsed, overlayCloseBudget)
	}
	if !errors.Is(err, ErrAbandonedThread) {
		t.Fatalf("Close() error = %v; it does not wrap gst.ErrAbandonedThread, so App.teardownStep "+
			"will score the picture step as FINISHED and the process will exit through "+
			"DLL_PROCESS_DETACH over an abandoned thread", err)
	}
	// The sentence still has to name what happened, because it is what the
	// operator's log shows.
	if !strings.Contains(err.Error(), "ABANDONED") {
		t.Errorf("Close() error = %q, which does not say it abandoned anything", err)
	}
	// Idempotent, and the second answer is the same one.
	if again := o.Close(); !errors.Is(again, ErrAbandonedThread) {
		t.Errorf("second Close() error = %v, want the same abandonment", again)
	}
}

func TestOverlayWindowClassRegisters(t *testing.T) {
	// This is the WNDCLASSEXW layout test and the overlayWndProc
	// syscall.NewCallback test in one, because registering is what does both.
	//
	// It is idempotent by construction: registerOverlayClass is behind a
	// sync.Once precisely so that a second call cannot fail with
	// ERROR_CLASS_ALREADY_EXISTS and be mistaken for a real failure.
	atom, err := registerOverlayClass()
	if err != nil {
		t.Fatalf("registerOverlayClass() error = %v; the window class is what the picture is drawn in", err)
	}
	if atom == 0 {
		t.Fatal("registerOverlayClass() returned atom 0 with no error")
	}

	again, err := registerOverlayClass()
	if err != nil || again != atom {
		t.Fatalf("registerOverlayClass() is not idempotent: second call returned (%v, %v), want (%v, nil)",
			again, err, atom)
	}
}

func TestWndClassExWIsTheSizeWindowsExpects(t *testing.T) {
	// WNDCLASSEXW on x64 is 80 bytes: two 32-bit fields, a pointer, two 32-bit
	// fields, then five pointers, then one more pointer. Getting the layout
	// wrong is not a compile error — it is a RegisterClassExW that fails, or
	// worse, one that succeeds against a garbage window procedure.
	//
	// cbSize is what Windows validates, so it is asserted against the same
	// unsafe.Sizeof the code passes, and the total is asserted against the
	// documented figure so that a field added in the wrong place fails here.
	const wantSize = 80
	if got := unsafe.Sizeof(wndClassExW{}); got != wantSize {
		t.Fatalf("sizeof(WNDCLASSEXW) = %d, want %d; the struct layout does not match the C one",
			got, wantSize)
	}
	// MSG is 48 bytes on x64 for the same reason: GetMessageW writes into it.
	const wantMsgSize = 48
	if got := unsafe.Sizeof(msgW{}); got != wantMsgSize {
		t.Fatalf("sizeof(MSG) = %d, want %d; GetMessageW would write past the end of it",
			got, wantMsgSize)
	}
}

func TestFindHostWindowReportsNoHostRatherThanGuessing(t *testing.T) {
	// A `go test` binary has no visible unowned top-level window, so this is the
	// startup case: the frontend asked for the overlay before Wails made the
	// window. It must come back as ErrNoHostWindow — which SetPictureRect treats
	// as "not yet" — and NOT as some other window of this process.
	//
	// It is also the enumWindowsProc syscall.NewCallback test and the EnumWindows
	// marshalling test. Both run whatever the answer is.
	_, err := findHostWindow("a title no window in this process has")
	if !errors.Is(err, ErrNoHostWindow) {
		t.Fatalf("findHostWindow() with an impossible title error = %v, want ErrNoHostWindow", err)
	}
}

func TestFindHostWindowRunsTheCallbackWithNoTitleFilter(t *testing.T) {
	// The empty-title path is a different branch of enumWindowsProc — it skips
	// GetWindowTextW entirely — and it is the fallback NewPictureOverlay uses
	// when no title is given. It must not panic and must not return a window and
	// an error at the same time.
	//
	// Whether it FINDS anything depends on what else the test runner has open in
	// this session, so the assertion is on the invariant and not on the count.
	hwnd, err := findHostWindow("")
	switch {
	case err == nil && hwnd == 0:
		t.Fatal("findHostWindow() returned a nil handle and no error")
	case err != nil && hwnd != 0:
		t.Fatalf("findHostWindow() returned both a handle (0x%x) and an error (%v)", hwnd, err)
	case err != nil && !errors.Is(err, ErrNoHostWindow):
		t.Fatalf("findHostWindow() error = %v, want nil or ErrNoHostWindow", err)
	}
}

func TestOverlayCloseIsIdempotentOnAnOverlayThatNeverOpened(t *testing.T) {
	// Teardown calls Close unconditionally. An overlay whose pump never started
	// — which is what a construction failure leaves behind — must not block for
	// overlayCloseBudget on every call, and must not double-close anything.
	o := &overlay{
		created: make(chan error, 1),
		done:    make(chan struct{}),
	}
	close(o.done) // the pump is already gone

	if err := o.Close(); err != nil {
		t.Fatalf("Close() on an overlay with no window error = %v, want nil", err)
	}
	if err := o.Close(); err != nil {
		t.Fatalf("second Close() error = %v; Close must be idempotent", err)
	}
	if o.Handle() != 0 {
		t.Fatal("Handle() is non-zero after Close")
	}
}

func TestOverlaySetRectAndSetVisibleFailClosedAfterClose(t *testing.T) {
	// After Close there is no window and no thread. A SetRect that quietly
	// succeeded would let app_picture.go believe the picture had been placed.
	o := &overlay{created: make(chan error, 1), done: make(chan struct{})}
	close(o.done)
	o.Close()

	if err := o.SetRect(PictureRect{W: 10, H: 10}); err == nil {
		t.Error("SetRect() succeeded on a closed overlay")
	}
	if err := o.SetVisible(true); err == nil {
		t.Error("SetVisible() succeeded on a closed overlay")
	}
}

func TestOverlayCoalescesUnchangedRequests(t *testing.T) {
	// A ResizeObserver fires on every frame of an animation and reports the same
	// integers most of the time. Every post that gets through is a SetWindowPos,
	// and a SetWindowPos on a window gstd3d11 has subclassed is a swapchain
	// resize — so an unchanged request must not become one.
	//
	// hwnd is left zero, which makes wake() return an error; the assertion is
	// that an UNCHANGED request never reaches wake at all and therefore returns
	// nil.
	o := &overlay{created: make(chan error, 1), done: make(chan struct{})}
	o.rect = PictureRect{X: 1, Y: 2, W: 3, H: 4}
	o.visible = true

	if err := o.SetRect(PictureRect{X: 1, Y: 2, W: 3, H: 4}); err != nil {
		t.Fatalf("SetRect() with an unchanged rectangle error = %v, want nil (it must not post)", err)
	}
	if err := o.SetVisible(true); err != nil {
		t.Fatalf("SetVisible() with an unchanged visibility error = %v, want nil (it must not post)", err)
	}
	// A CHANGED request does try to post, and fails here because there is no
	// window — which is what proves the two calls above took the other branch.
	if err := o.SetRect(PictureRect{X: 9, Y: 9, W: 9, H: 9}); err == nil {
		t.Fatal("SetRect() with a changed rectangle did not attempt to post to the pump")
	}
}
