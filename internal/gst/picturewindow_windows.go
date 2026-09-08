//go:build windows

// picturewindow_windows.go is the TOP-LEVEL window the SRT programme picture is
// drawn in when the picture runs as its own process.
//
// Owner: WP-P, with picture_cgo.go and overlay_windows.go.
//
// # Why a top-level window and not the overlay
//
// overlay_windows.go is a CHILD HWND of the application window, positioned over
// the page by the page, hidden whenever anything must appear above it. That
// shape exists because the picture used to live in the application's own
// process, sharing its window. It no longer does: the picture is a separate
// process, launched by the application, so that a wedged GStreamer or GPU
// driver in the decode path can be killed and relaunched without touching the
// contribution feed — and a separate process has no application window to be a
// child OF. It gets a window of its own: titled, resizable, movable to a second
// monitor, with a close button the operator can use.
//
// Everything that made the overlay difficult — CSS-pixel rectangles, device
// pixel ratios, z-order against a WebView, the three-way visibility rule — is
// absent here because none of it applies. This window is where the picture is.
// The sink is given its handle once and owns every pixel of its client area,
// resizing itself with the window; the page knows nothing about it.
//
// # It is as dumb as the overlay, for the same reason
//
// It paints nothing and knows nothing about GStreamer. gstd3d11 SUBCLASSES the
// HWND it is given and handles WM_SIZE and WM_PAINT in front of this procedure,
// so this file's window procedure handles exactly two messages — WM_CLOSE and
// WM_DESTROY, which are how the operator's click on the close button becomes
// the Closed channel — and forwards everything else to DefWindowProc. A black
// class brush is what shows before the sink attaches.
//
// # Threading, which is the same story as the overlay's
//
// A window belongs to the thread that created it, and that thread must pump its
// messages, so the window is created on a dedicated OS-locked goroutine running
// a GetMessage loop. There is no Wails message loop in the picture process, so
// there is no cross-thread parent and none of the overlay's attached-input-queue
// hazards; but Close still goes through a PostMessage to the pump rather than a
// direct DestroyWindow, because DestroyWindow must be called on the creating
// thread and this file has no other way to get there.
package gst

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// PictureWindow is a top-level native window the picture pipeline renders into,
// owned by the picture process.
type PictureWindow interface {
	// Handle is the HWND, for PictureOpts.WindowHandle. Non-zero for the life of
	// the window.
	Handle() uintptr
	// Closed is closed when the window has gone — the operator closed it, or
	// Close was called — so the process that owns it knows to stop the pipeline
	// and exit.
	Closed() <-chan struct{}
	// Close destroys the window and returns when the pump thread has finished,
	// or after pictureWindowCloseBudget if it has not. Idempotent.
	Close() error
}

const (
	// pictureWindowClassName is registered once per process. It is distinct
	// from overlayClassName so that the two window procedures — one that
	// handles WM_CLOSE, one that must not — can never be attached to the wrong
	// window.
	pictureWindowClassName = "WslCommsPictureWindow"

	// Win32 style words this file adds to overlay_windows.go's set.
	wsOverlappedWindow = 0x00CF0000 // caption, sysmenu, thick frame, min/max boxes
	wsVisible          = 0x10000000
	cwUseDefault       = 0x80000000 // CW_USEDEFAULT: let the shell place it

	wmClose   = 0x0010
	wmDestroy = 0x0002

	// The initial client-area-ish size: 16:9, comfortably under a 1080p
	// display. The operator resizes and moves it; nothing here remembers where.
	pictureWindowDefaultW = 960
	pictureWindowDefaultH = 540

	// pictureWindowCloseBudget bounds Close's wait for the pump thread, for the
	// reason overlayCloseBudget bounds the overlay's: a thread that has stopped
	// answering is not one to wait on for ever at exit.
	pictureWindowCloseBudget = 2 * time.Second
)

// ErrPictureWindowAbandoned is returned by Close when the pump thread did not
// finish within the budget. The process is about to exit anyway; the window
// goes with it.
var ErrPictureWindowAbandoned = errors.New("gst: picture window: the pump thread did not finish")

type pictureWindow struct {
	mu   sync.Mutex
	hwnd syscall.Handle

	created chan error    // one value: the CreateWindowExW outcome
	closed  chan struct{} // closed when the pump loop has ended
	done    chan struct{} // closed when the pump goroutine has returned

	closeOnce sync.Once
}

var (
	pictureWindowClassOnce sync.Once
	pictureWindowClassErr  error
)

// pictureWindowWndProc is the window procedure. WM_CLOSE (the close button, or
// Alt+F4) destroys the window; WM_DESTROY ends the message loop. Everything
// else is DefWindowProc's, and gstd3d11's subclass runs in front of all of it.
func pictureWindowWndProc(hwnd syscall.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmClose:
		procDestroyWindow.Call(uintptr(hwnd))
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return ret
}

func registerPictureWindowClass() error {
	pictureWindowClassOnce.Do(func() {
		hinst, _, _ := procGetModuleHandleW.Call(0)
		brush, _, _ := procGetStockObject.Call(blackBrush)
		className, _ := syscall.UTF16PtrFromString(pictureWindowClassName)
		wc := wndClassExW{
			cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
			lpfnWndProc:   syscall.NewCallback(pictureWindowWndProc),
			hInstance:     syscall.Handle(hinst),
			hbrBackground: syscall.Handle(brush),
			lpszClassName: className,
		}
		if atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
			pictureWindowClassErr = fmt.Errorf("gst: picture window: RegisterClassExW failed: %w", callErr)
		}
	})
	return pictureWindowClassErr
}

// NewPictureWindow creates and shows a top-level window titled title, on its own
// pump thread, and returns once it exists.
func NewPictureWindow(title string) (PictureWindow, error) {
	if err := registerPictureWindowClass(); err != nil {
		return nil, err
	}
	w := &pictureWindow{
		created: make(chan error, 1),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	go w.pump(title)
	if err := <-w.created; err != nil {
		return nil, err
	}
	return w, nil
}

// pump creates the window and runs its message loop. It is the window's thread.
func (w *pictureWindow) pump(title string) {
	runtime.LockOSThread()
	defer close(w.done)

	hinst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString(pictureWindowClassName)
	windowName, _ := syscall.UTF16PtrFromString(title)

	// Shown immediately, at a shell-chosen position, clipping its children so
	// the sink's own internal child window is never painted over.
	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windowName)),
		wsOverlappedWindow|wsVisible|wsClipChildren,
		cwUseDefault, cwUseDefault, pictureWindowDefaultW, pictureWindowDefaultH,
		0, 0, hinst, 0,
	)
	if hwnd == 0 {
		w.created <- fmt.Errorf("gst: picture window: CreateWindowExW failed: %w", callErr)
		w.closeOnce.Do(func() { close(w.closed) })
		return
	}

	w.mu.Lock()
	w.hwnd = syscall.Handle(hwnd)
	w.mu.Unlock()
	w.created <- nil

	var m msgW
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			// 0 is WM_QUIT — posted by WM_DESTROY, whichever way the window
			// went; -1 is an error on a handle that has gone.
			break
		}
		if m.message == wmAppQuit {
			// Close() asked, from another thread. Destroying here on the pump
			// thread posts WM_DESTROY → WM_QUIT and the loop ends on the next
			// GetMessage.
			w.mu.Lock()
			h := w.hwnd
			w.mu.Unlock()
			if h != 0 {
				procDestroyWindow.Call(uintptr(h))
			}
			continue
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	w.mu.Lock()
	w.hwnd = 0
	w.mu.Unlock()
	w.closeOnce.Do(func() { close(w.closed) })
}

func (w *pictureWindow) Handle() uintptr {
	w.mu.Lock()
	defer w.mu.Unlock()
	return uintptr(w.hwnd)
}

func (w *pictureWindow) Closed() <-chan struct{} { return w.closed }

func (w *pictureWindow) Close() error {
	w.mu.Lock()
	h := w.hwnd
	w.mu.Unlock()
	if h != 0 {
		// A PostMessage, never a direct DestroyWindow: the window must be
		// destroyed on the thread that created it.
		procPostMessageW.Call(uintptr(h), wmAppQuit, 0, 0)
	}
	select {
	case <-w.done:
		return nil
	case <-time.After(pictureWindowCloseBudget):
		return ErrPictureWindowAbandoned
	}
}
