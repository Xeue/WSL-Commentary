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
	"testing"
	"unsafe"
)

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
