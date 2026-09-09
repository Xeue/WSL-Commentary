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
	// Placement is where the window is now, or where it last was if it has
	// gone. The bool is false only before a window ever existed.
	Placement() (PictureWindowPlacement, bool)
}

// PictureWindowPlacement is where the picture window sits on the desktop: its
// normal (un-maximised) rectangle in Win32 workspace coordinates, and whether it
// is maximised. It exists so the picture can come back WHERE THE OPERATOR PUT IT
// after a Refresh, which restarts the whole picture process: a window that
// jumped back to a default position on every restart would make the button
// cost the operator a drag each time they pressed it.
//
// The rectangle is whatever GetWindowPlacement reported and is handed straight
// back to SetWindowPlacement; nothing here interprets it beyond checking that
// it still lands on a monitor that exists.
type PictureWindowPlacement struct {
	Left, Top, Right, Bottom int32
	Maximised                bool
}

// sane reports whether the rectangle has a usable size. It says nothing about
// monitors; placementOnScreen does that, and needs Win32 to.
func (p PictureWindowPlacement) sane() bool {
	w, h := p.Right-p.Left, p.Bottom-p.Top
	return w >= pictureWindowMinW && h >= pictureWindowMinH && w <= pictureWindowMaxDim && h <= pictureWindowMaxDim
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

	wmClose        = 0x0010
	wmDestroy      = 0x0002
	wmSize         = 0x0005
	wmExitSizeMove = 0x0232

	sizeRestored  = 0 // WM_SIZE wParam
	sizeMaximized = 2

	swShow          = 5 // ShowWindow / WINDOWPLACEMENT.showCmd
	swShowNormal    = 1
	swShowMaximized = 3

	monitorDefaultToNull = 0 // MonitorFromRect: 0 when no monitor intersects

	// The initial client-area-ish size: 16:9, comfortably under a 1080p
	// display, used when there is no remembered placement or the remembered
	// one no longer lands on a monitor.
	pictureWindowDefaultW = 960
	pictureWindowDefaultH = 540

	// Bounds on a remembered placement: a window smaller than this is not one
	// anybody meant, and a larger one is a corrupt file.
	pictureWindowMinW   = 160
	pictureWindowMinH   = 90
	pictureWindowMaxDim = 32767

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

	// placement is the last placement recorded from the window, and havePl
	// whether one ever was. Read by Placement once the window has gone.
	placement PictureWindowPlacement
	havePl    bool
	// wasMax is what the last WM_SIZE said, so a restore from maximised —
	// which arrives as WM_SIZE alone, with no size/move loop around it — is
	// recorded while the WM_SIZE storm during a resize drag is not.
	wasMax bool

	onMoved func(PictureWindowPlacement)

	created chan error    // one value: the CreateWindowExW outcome
	closed  chan struct{} // closed when the pump loop has ended
	done    chan struct{} // closed when the pump goroutine has returned

	closeOnce sync.Once
}

var (
	pictureWindowClassOnce sync.Once
	pictureWindowClassErr  error

	// pictureWindows maps a live HWND to its window, so the window procedure —
	// a package-level function with no closure — can find the object whose
	// placement to record. Registered after CreateWindowExW returns, so the
	// messages sent during creation record nothing, which is right: they
	// describe the default position, not one the operator chose.
	pictureWindowsMu sync.Mutex
	pictureWindows   = map[syscall.Handle]*pictureWindow{}

	procGetWindowPlacement = user32.NewProc("GetWindowPlacement")
	procSetWindowPlacement = user32.NewProc("SetWindowPlacement")
	procMonitorFromRect    = user32.NewProc("MonitorFromRect")
)

// Win32's POINT, RECT and WINDOWPLACEMENT, as GetWindowPlacement and
// SetWindowPlacement take them. length must be set to the struct's size.
type (
	wpPoint struct{ x, y int32 }
	wpRect  struct{ left, top, right, bottom int32 }

	windowPlacementW struct {
		length           uint32
		flags            uint32
		showCmd          uint32
		ptMinPosition    wpPoint
		ptMaxPosition    wpPoint
		rcNormalPosition wpRect
	}
)

// pictureWindowWndProc is the window procedure. WM_CLOSE (the close button, or
// Alt+F4) destroys the window; WM_DESTROY records where the window was and
// ends the message loop; WM_EXITSIZEMOVE and the maximise/restore WM_SIZEs
// record the placement so a Refresh can put the next window back there.
// Everything else is DefWindowProc's, and gstd3d11's subclass runs in front of
// all of it.
func pictureWindowWndProc(hwnd syscall.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmClose:
		procDestroyWindow.Call(uintptr(hwnd))
		return 0
	case wmDestroy:
		if w := lookupPictureWindow(hwnd); w != nil {
			w.recordPlacement()
		}
		procPostQuitMessage.Call(0)
		return 0
	case wmExitSizeMove:
		// The end of a drag or a resize: one record per gesture, not one per
		// mouse move.
		if w := lookupPictureWindow(hwnd); w != nil {
			w.recordPlacement()
		}
	case wmSize:
		// Maximise, and restore-from-maximise, arrive as WM_SIZE alone. A
		// restore is recorded only when the previous WM_SIZE was a maximise,
		// which keeps the WM_SIZE storm of a resize drag out of it.
		if w := lookupPictureWindow(hwnd); w != nil {
			w.mu.Lock()
			wasMax := w.wasMax
			w.wasMax = wParam == sizeMaximized
			w.mu.Unlock()
			if wParam == sizeMaximized || (wParam == sizeRestored && wasMax) {
				w.recordPlacement()
			}
		}
	}
	ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return ret
}

func lookupPictureWindow(hwnd syscall.Handle) *pictureWindow {
	pictureWindowsMu.Lock()
	defer pictureWindowsMu.Unlock()
	return pictureWindows[hwnd]
}

// queryPlacement reads the window's placement from Win32. It may be called from
// any thread: GetWindowPlacement sends no messages.
func queryPlacement(hwnd syscall.Handle) (PictureWindowPlacement, bool) {
	var wp windowPlacementW
	wp.length = uint32(unsafe.Sizeof(wp))
	ret, _, _ := procGetWindowPlacement.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&wp)))
	if ret == 0 {
		return PictureWindowPlacement{}, false
	}
	return PictureWindowPlacement{
		Left: wp.rcNormalPosition.left, Top: wp.rcNormalPosition.top,
		Right: wp.rcNormalPosition.right, Bottom: wp.rcNormalPosition.bottom,
		Maximised: wp.showCmd == swShowMaximized,
	}, true
}

// recordPlacement reads the placement, keeps it, and tells onMoved. It runs on
// the pump thread, from the window procedure.
func (w *pictureWindow) recordPlacement() {
	w.mu.Lock()
	h := w.hwnd
	w.mu.Unlock()
	if h == 0 {
		return
	}
	p, ok := queryPlacement(h)
	if !ok {
		return
	}
	w.mu.Lock()
	w.placement, w.havePl = p, true
	cb := w.onMoved
	w.mu.Unlock()
	if cb != nil {
		cb(p)
	}
}

// placementOnScreen reports whether the placement's rectangle still meets a
// monitor. A remembered position on a second monitor that has since been
// unplugged would otherwise put the picture where nobody can see it, and no
// error would say so.
func placementOnScreen(p PictureWindowPlacement) bool {
	if !p.sane() {
		return false
	}
	rc := wpRect{left: p.Left, top: p.Top, right: p.Right, bottom: p.Bottom}
	mon, _, _ := procMonitorFromRect.Call(uintptr(unsafe.Pointer(&rc)), monitorDefaultToNull)
	return mon != 0
}

// applyPlacement puts a freshly created, still hidden window where p says and
// shows it, maximised if p was. It runs on the pump thread. It returns false,
// having shown nothing, when the placement cannot be used.
func applyPlacement(hwnd syscall.Handle, p PictureWindowPlacement) bool {
	if !placementOnScreen(p) {
		return false
	}
	wp := windowPlacementW{
		showCmd:          swShowNormal,
		rcNormalPosition: wpRect{left: p.Left, top: p.Top, right: p.Right, bottom: p.Bottom},
	}
	wp.length = uint32(unsafe.Sizeof(wp))
	if p.Maximised {
		wp.showCmd = swShowMaximized
	}
	ret, _, _ := procSetWindowPlacement.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&wp)))
	return ret != 0
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
//
// initial, if non-nil and still on a monitor, is where the window appears —
// the placement remembered from the last picture process. Otherwise the shell
// places it at the default size. onMoved, if non-nil, is called on the pump
// thread each time the operator finishes moving or resizing the window,
// maximises or restores it, and when it is destroyed; it is how the placement
// gets remembered for the next process. It must be quick and must not touch
// the window.
func NewPictureWindow(title string, initial *PictureWindowPlacement, onMoved func(PictureWindowPlacement)) (PictureWindow, error) {
	if err := registerPictureWindowClass(); err != nil {
		return nil, err
	}
	w := &pictureWindow{
		onMoved: onMoved,
		created: make(chan error, 1),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	go w.pump(title, initial)
	if err := <-w.created; err != nil {
		return nil, err
	}
	return w, nil
}

// pump creates the window and runs its message loop. It is the window's thread.
func (w *pictureWindow) pump(title string, initial *PictureWindowPlacement) {
	runtime.LockOSThread()
	defer close(w.done)

	hinst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString(pictureWindowClassName)
	windowName, _ := syscall.UTF16PtrFromString(title)

	// Created HIDDEN, at a shell-chosen position, clipping its children so the
	// sink's own internal child window is never painted over. It is shown
	// below, once it is where it should be: a window shown at the default
	// position and then moved to the remembered one is a flash the operator
	// sees on every Refresh.
	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windowName)),
		wsOverlappedWindow|wsClipChildren,
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

	// From here the window procedure records placements for this window.
	pictureWindowsMu.Lock()
	pictureWindows[syscall.Handle(hwnd)] = w
	pictureWindowsMu.Unlock()
	defer func() {
		pictureWindowsMu.Lock()
		delete(pictureWindows, syscall.Handle(hwnd))
		pictureWindowsMu.Unlock()
	}()

	placed := initial != nil && applyPlacement(syscall.Handle(hwnd), *initial)
	if !placed {
		procShowWindow.Call(hwnd, swShow)
	}
	if p, ok := queryPlacement(syscall.Handle(hwnd)); ok {
		w.mu.Lock()
		w.placement, w.havePl = p, true
		w.wasMax = p.Maximised
		w.mu.Unlock()
	}
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

// Placement is the live placement while the window exists — GetWindowPlacement
// is safe from any thread — and the last recorded one after it has gone.
func (w *pictureWindow) Placement() (PictureWindowPlacement, bool) {
	w.mu.Lock()
	h := w.hwnd
	last, have := w.placement, w.havePl
	w.mu.Unlock()
	if h != 0 {
		if p, ok := queryPlacement(h); ok {
			return p, true
		}
	}
	return last, have
}

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
