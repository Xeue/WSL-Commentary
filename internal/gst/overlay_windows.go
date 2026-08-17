//go:build windows

// overlay_windows.go is the NATIVE SURFACE video is drawn on: a child window of
// the application's own window, which d3d11videosink renders into through
// GstVideoOverlay.
//
// Owner: WP-P, with picture_cgo.go.
//
// THERE ARE NOW TWO OF THEM IN ONE PARENT — the SRT programme picture and the
// DeckLink confidence preview — created by NewOverlaySurface, which is the only
// thing that differs between them. What that does and does not change is at
// NewOverlaySurface, and the z-order half of it is at the SetWindowPos call in
// apply, which stays unconditional here and is a test on the Mac. That
// asymmetry is the one place the two platforms genuinely disagree, and it is
// written down in three places for that reason: here, there, and in
// overlay_zorder.go, which holds the rule itself.
//
// # Why this exists at all
//
// A browser <video> element cannot play SRT. There is no MSE demuxer for
// MPEG-TS-over-SRT in WebView2, no way to hand it an HEVC elementary stream, and
// no HEVC decoder in the WebView on a machine where the Windows HEVC extension
// is not installed. So the picture cannot go through the page, and the only
// remaining place to put it is a window — which means a native child HWND
// painted OVER the page, positioned by the page.
//
// That is the whole of the difficulty. A native child does not participate in
// CSS layout. It is not clipped by an overflow rule, it is not moved by a
// flexbox, it does not scroll, and it does not disappear when a modal opens. It
// sits where it was last told to sit, on top, opaque, until it is told
// otherwise. Every requirement below is a way that goes wrong.
//
// # It is deliberately dumb
//
// This window paints nothing, decodes nothing, and knows nothing about
// GStreamer. It exists, it has a handle, it can be moved and it can be hidden.
// gstd3d11 takes the handle, subclasses the window and creates its own internal
// child inside it — which is why the class below has no WM_PAINT handler and a
// black class brush is enough: before the sink is attached the window is black,
// and after it is attached the sink owns every pixel.
//
// Giving gstd3d11 a window of OUR OWN, rather than the Wails window itself, is
// the single most important decision in this file. gstd3d11 SUBCLASSES whatever
// HWND it is given — SetWindowLongPtr(GWLP_WNDPROC) — and restores the previous
// procedure on teardown. Handing it the Wails window would put a GStreamer
// subclass on the same window Wails and go-webview2 have already subclassed, and
// the restore ordering between three parties, one of which can be abandoned
// mid-teardown, is not something to be clever about at a commentary position. A
// window nothing else touches costs one CreateWindowEx and removes that entirely.
//
// # Threading, which is the part that can hang the application
//
// A window belongs to the thread that created it, and that thread must pump its
// messages. This one is created on a dedicated OS-locked goroutine running a
// GetMessage loop, because there is no way to run code on Wails' message-loop
// thread from Go: Wails v2 exposes no invoke-on-main-thread call.
//
// That makes the child window and its parent belong to DIFFERENT threads, and
// Windows attaches the input queues of a cross-thread parent and child. The
// hazard that creates is well known: a call that SENDS a message to a window
// owned by another thread blocks until that thread pumps it, so a wedged pump
// wedges the caller. SetWindowPos and ShowWindow both send messages.
//
// THEREFORE NOTHING OUTSIDE THIS FILE'S OWN THREAD EVER CALLS SetWindowPos OR
// ShowWindow. SetRect, Show and Hide record what is wanted under a mutex and
// PostMessage a wake-up, which never blocks; the pump thread does the actual
// window call. A bound method invoked from a Wails message handler therefore
// cannot be made to wait on this thread, on gstd3d11's subclass procedure, or on
// the GPU, no matter what state any of them are in. It is the one property that
// makes an opaque native overlay safe to drive from a UI callback.
//
// # The exit path
//
// Read App.teardown and exit_windows.go before changing anything here. The
// application's shutdown runs each step under its own budget and force-exits if
// it had to abandon one, and Close below is written to fit that: it posts a quit
// to the pump, waits overlayCloseBudget for the thread to acknowledge, and if it
// does not, it says so in the returned error and RETURNS ANYWAY. It never blocks
// unboundedly and it never leaves the caller without an answer. The abandoned
// thread is a thread inside DestroyWindow or gstd3d11's subclass restore that the
// process is about to exit out from under — which costs nothing the OS does not
// reclaim, and is exactly the trade exit_windows.go documents.
package gst

import (
	"errors"
	"fmt"
	"log"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// ---------------------------------------------------------------------------
// The contract
// ---------------------------------------------------------------------------

// PictureOverlay is one of this application's native video surfaces.
//
// THERE ARE TWO OF THEM: the SRT programme picture, and the DeckLink confidence
// preview. The type keeps the name it was born with because it is what
// app_picture.go is written against and renaming it would touch every file on
// that path to say nothing new; NewOverlaySurface is what says which one is
// being made, and nothing else about a surface differs between the two.
//
// It is created once and outlives every pipeline. The picture monitor is stopped
// and rebuilt whenever the endpoint or the configuration changes, and destroying
// the window with it would mean a reconnect had nowhere to draw and would put a
// window create and a z-order fight into the middle of a reconnect.
//
// All methods are safe for concurrent use, and none of them blocks on the pump
// thread. See the file header.
type PictureOverlay interface {
	// Handle returns the HWND as a uintptr, for PictureOpts.WindowHandle. It is
	// zero once Close has run.
	Handle() uintptr

	// SetRect places the window, in PHYSICAL pixels relative to the top-left of
	// the host window's client area. An empty rectangle hides the window rather
	// than failing: a page that has not laid out yet legitimately produces one.
	//
	// It returns as soon as the request is recorded. The move happens on the
	// pump thread, and successive calls coalesce — only the most recent
	// rectangle is ever applied, which is what a resize drag needs.
	SetRect(r PictureRect) error

	// SetVisible shows or hides the window.
	//
	// Hiding is not cosmetic. The window is OPAQUE and always on top of its
	// rectangle: anything the page draws there — Settings, the mixer drawer, a
	// modal, or the fallback mosaic when SRT is not connected — is invisible
	// underneath it. Every one of those cases is a call to this method, made
	// explicitly by the frontend rather than inferred here.
	SetVisible(visible bool) error

	// Close destroys the window and stops the pump thread.
	//
	// It is bounded: it waits overlayCloseBudget and then returns an error
	// saying the thread was abandoned rather than waiting further. It is
	// idempotent.
	Close() error
}

// overlayCloseBudget is how long Close waits for the pump thread to destroy the
// window and exit.
//
// Two seconds, matching returnStopBudget in app.go, and for the same reason:
// what is being waited for is a DestroyWindow plus gstd3d11's subclass restore,
// both of which take microseconds when they work at all. A budget long enough to
// cover a hang would only delay a shutdown that is already going to end in
// TerminateProcess.
const overlayCloseBudget = 2 * time.Second

// ErrNoHostWindow is returned by NewPictureOverlay when this process has no
// visible top-level window yet. It is a normal condition, not a fault: it means
// the call arrived before Wails created the window, and the caller should try
// again.
var ErrNoHostWindow = errors.New("gst: the application window does not exist yet")

// ---------------------------------------------------------------------------
// Win32
// ---------------------------------------------------------------------------

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

	procRegisterClassExW       = user32.NewProc("RegisterClassExW")
	procCreateWindowExW        = user32.NewProc("CreateWindowExW")
	procDestroyWindow          = user32.NewProc("DestroyWindow")
	procDefWindowProcW         = user32.NewProc("DefWindowProcW")
	procGetMessageW            = user32.NewProc("GetMessageW")
	procTranslateMessage       = user32.NewProc("TranslateMessage")
	procDispatchMessageW       = user32.NewProc("DispatchMessageW")
	procPostMessageW           = user32.NewProc("PostMessageW")
	procPostQuitMessage        = user32.NewProc("PostQuitMessage")
	procSetWindowPos           = user32.NewProc("SetWindowPos")
	procShowWindow             = user32.NewProc("ShowWindow")
	procEnumWindows            = user32.NewProc("EnumWindows")
	procGetWindowThreadProcess = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible        = user32.NewProc("IsWindowVisible")
	procIsWindow               = user32.NewProc("IsWindow")
	procGetWindow              = user32.NewProc("GetWindow")
	procGetWindowTextW         = user32.NewProc("GetWindowTextW")

	// GetDpiForWindow is Windows 10 1607 and later. It is used for a LOG LINE
	// ONLY — the rectangle arithmetic uses the page's own devicePixelRatio, see
	// ScaleRect — so a machine without it loses a diagnostic and nothing else.
	procGetDpiForWindow = user32.NewProc("GetDpiForWindow")

	procGetCurrentProcessID = kernel32.NewProc("GetCurrentProcessId")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")

	procGetStockObject = gdi32.NewProc("GetStockObject")
)

const (
	wsChild        = 0x40000000
	wsClipSiblings = 0x04000000
	wsClipChildren = 0x02000000

	// wsDisabled is on the child deliberately.
	//
	// The picture is not interactive: there is nothing to click on it and
	// nothing behind it in the page that a click should reach. A native child
	// that accepted input would be able to take the keyboard focus away from the
	// WebView, which silently breaks every keyboard shortcut on the page, and it
	// would do so by being clicked — which is exactly what a commentator does to
	// a picture they want to look at.
	//
	// It also disables the internal child gstd3d11 creates inside this one,
	// which is the belt to the enable-navigation-events=false brace in
	// picture_cgo.go.
	wsDisabled = 0x08000000

	// wsExNoParentNotify (WS_EX_NOPARENTNOTIFY) IS LOAD-BEARING AND IT IS NOT A
	// TIDINESS FLAG. DO NOT REMOVE IT.
	//
	// Without it, Windows SENDS the parent a WM_PARENTNOTIFY when this child is
	// created and again when it is destroyed. The parent is the Wails top-level
	// window and it belongs to a DIFFERENT THREAD — see the threading section of
	// the file header, which is about the same hazard from the other direction —
	// and a cross-thread SendMessage blocks, with no timeout, until the owning
	// thread pumps its queue.
	//
	// On the destroy that is fatal to the shutdown. DestroyWindow runs on this
	// file's pump thread during App.teardown, and at that moment the Wails main
	// thread is inside OnShutdown → teardown, parked in a select. IT IS NOT
	// PUMPING. So the notify never gets picked up, destroy() blocks for the whole
	// of teardown, the picture step overruns its budget and is abandoned — on an
	// ORDINARY QUIT, deterministically, with no GPU hang and nothing wrong
	// anywhere. This one bit is the difference between the abandonment path being
	// a rare last resort and being what happens every time the operator closes
	// the application.
	//
	// On the create it is milder but real: the same send makes CreateWindowExW
	// wait on the Wails thread, so NewPictureOverlay's `<-o.created` blocks the
	// Wails handler that called it — while that handler holds picViewMu, which is
	// the lock stopPictureForTeardown needs.
	//
	// Nothing is given up by setting it. WM_PARENTNOTIFY exists so a parent can
	// react to its children being created, destroyed or clicked; the parent here
	// is a WebView2 host that knows nothing about this window, and the child is
	// wsDisabled so it produces no mouse notifications either.
	wsExNoParentNotify = 0x00000004

	swpNoActivate  = 0x0010
	swpShowWindow  = 0x0040
	swpNoZOrder    = 0x0004
	swpNoOwnerZOrd = 0x0200

	swHide           = 0
	swShowNoActivate = 4

	gwOwner = 4

	blackBrush = 4

	// wmAppWake is the message the pump waits for. Everything this file does
	// from another goroutine is "record the wish, post this, return".
	wmAppWake = 0x8000 + 1 // WM_APP + 1

	// wmAppQuit tells the pump to destroy the window and end.
	wmAppQuit = 0x8000 + 2 // WM_APP + 2

	// hwndTop is the z-order position the child is raised to on every layout
	// update. It is re-applied every time rather than once at creation because
	// WebView2 reorders its own windows and there is no notification when it
	// does; a picture that has quietly gone behind the page is indistinguishable
	// from a picture that has stopped.
	hwndTop = 0
)

// overlayClassName is the window class. It is namespaced so it cannot collide
// with a class registered by Wails, go-webview2, Chromium or gstd3d11 — all four
// of which register classes in this process.
const overlayClassName = "WslCommsPictureOverlay"

// overlayExStyle and overlayStyle are the two style words CreateWindowExW is
// given, named rather than written inline so that overlay_windows_test.go can
// assert what is in them without creating a window — which a `go test` binary
// cannot do, having no visible top-level window to be a child of.
//
// Every bit in both has a paragraph above it. wsExNoParentNotify in particular
// is the one that reads like a default and is not: dropping it wedges the whole
// teardown on a main thread that has stopped pumping.
const (
	overlayExStyle = wsExNoParentNotify
	overlayStyle   = wsChild | wsClipSiblings | wsClipChildren | wsDisabled
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     syscall.Handle
	hIcon         syscall.Handle
	hCursor       syscall.Handle
	hbrBackground syscall.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       syscall.Handle
}

type msgW struct {
	hwnd    syscall.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
	private uint32
}

// overlayClassOnce registers the window class exactly once per process.
//
// RegisterClassEx fails with ERROR_CLASS_ALREADY_EXISTS on a second call, which
// is survivable but would mean the failure of a genuine first registration could
// not be told apart from it. Registering once and remembering the outcome makes
// the error legible, and it is required anyway: this window may be created and
// destroyed several times in one run, and the class must outlive all of them.
var (
	overlayClassOnce sync.Once
	overlayClassErr  error
	overlayClassAtom uintptr
)

// overlayWndProc is the window procedure.
//
// It handles NOTHING. Every message goes to DefWindowProcW, which paints the
// class's black brush on WM_ERASEBKGND and does the right thing with everything
// else. That is deliberate: the moment gstd3d11 is given this handle it
// subclasses the window and its procedure runs in front of this one, so any
// message this procedure consumed would be a message gstd3d11 needed and did not
// get.
//
// wmAppWake and wmAppQuit are handled in the pump loop below, BEFORE dispatch,
// so they never reach here.
func overlayWndProc(hwnd syscall.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return ret
}

func registerOverlayClass() (uintptr, error) {
	overlayClassOnce.Do(func() {
		hinst, _, _ := procGetModuleHandleW.Call(0)
		brush, _, _ := procGetStockObject.Call(blackBrush)

		className, err := syscall.UTF16PtrFromString(overlayClassName)
		if err != nil {
			overlayClassErr = fmt.Errorf("gst: overlay: the class name is not representable: %w", err)
			return
		}

		wc := wndClassExW{
			cbSize: uint32(unsafe.Sizeof(wndClassExW{})),
			// No CS_HREDRAW or CS_VREDRAW: they force a full erase-and-repaint on
			// every resize, and on a window whose contents are a DXGI swapchain
			// that is a black flash per mouse-move of a resize drag.
			style:         0,
			lpfnWndProc:   syscall.NewCallback(overlayWndProc),
			hInstance:     syscall.Handle(hinst),
			hbrBackground: syscall.Handle(brush),
			lpszClassName: className,
		}
		atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		if atom == 0 {
			overlayClassErr = fmt.Errorf("gst: overlay: RegisterClassExW failed: %w", callErr)
			return
		}
		overlayClassAtom = atom
	})
	return overlayClassAtom, overlayClassErr
}

// ---------------------------------------------------------------------------
// Finding the window to be a child of
// ---------------------------------------------------------------------------

// findHostWindow returns the HWND of this process's own top-level application
// window.
//
// # Why it is searched for rather than asked for
//
// Wails v2 does not expose its HWND. There is no field, no runtime call and no
// option that yields it, and reaching into its internals would bind this file to
// a version of a dependency the manifests freeze. So the window is found the way
// an external tool would find it, with the search narrowed until it can only
// match one thing:
//
//   - it belongs to THIS process, which excludes every other application;
//   - it is visible, which excludes the message-only and helper windows
//     Chromium and COM create in bulk;
//   - it has no owner, which excludes tool windows and dialogs;
//   - its title matches, when a title is given.
//
// The title is the strongest of the four and is passed in by the caller rather
// than hard-coded, because the string lives in main.go as windowTitle and one
// copy of it is the correct number.
//
// A process that has more than one window matching all four does not exist here:
// the specification is one window, no menu and no tray icon, and
// SingleInstanceLock guarantees one process. If it ever does, the first match
// wins and the count is logged, so the ambiguity is visible rather than silent.
// enumState is what the EnumWindows callback reads and writes.
//
// It is package state guarded by a mutex rather than a closure, and that is not
// a style choice. syscall.NewCallback allocates a trampoline that is NEVER
// FREED — the documentation calls it a limited resource, and the limit is a few
// thousand per process. findHostWindow can be reached from the frontend's layout
// code on every resize for as long as the host window has not appeared, so a
// callback created per call would be a slow leak with a hard ceiling, and the
// symptom at the ceiling is a panic in the runtime rather than anything
// legible. One callback, created once, is the fix.
var (
	enumMu      sync.Mutex
	enumOnce    sync.Once
	enumProc    uintptr
	enumSelf    uintptr
	enumWant    []uint16
	enumMatches []syscall.Handle
)

// enumWindowsProc is the EnumWindows callback. It runs synchronously on the
// calling thread, inside procEnumWindows.Call, with enumMu held by the caller.
func enumWindowsProc(hwnd syscall.Handle, _ uintptr) uintptr {
	var pid uint32
	procGetWindowThreadProcess.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&pid)))
	if uintptr(pid) != enumSelf {
		return 1 // keep enumerating
	}
	if visible, _, _ := procIsWindowVisible.Call(uintptr(hwnd)); visible == 0 {
		return 1
	}
	if owner, _, _ := procGetWindow.Call(uintptr(hwnd), gwOwner); owner != 0 {
		return 1
	}
	if len(enumWant) > 1 {
		// enumWant is NUL-terminated, hence len-1 for the character count.
		buf := make([]uint16, 256)
		n, _, _ := procGetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if int(n) != len(enumWant)-1 {
			return 1
		}
		for i := 0; i < int(n); i++ {
			if buf[i] != enumWant[i] {
				return 1
			}
		}
	}
	enumMatches = append(enumMatches, hwnd)
	return 1
}

func findHostWindow(title string) (syscall.Handle, error) {
	enumOnce.Do(func() { enumProc = syscall.NewCallback(enumWindowsProc) })

	enumMu.Lock()
	defer enumMu.Unlock()

	self, _, _ := procGetCurrentProcessID.Call()
	enumSelf = self
	enumWant = nil
	enumMatches = nil
	if title != "" {
		enumWant = syscall.StringToUTF16(title)
	}

	// EnumWindows is synchronous — it returns only after the last callback — so
	// the state above is live for exactly the duration of this call, which is
	// what makes one mutex enough.
	procEnumWindows.Call(enumProc, 0)
	matches := enumMatches
	enumMatches = nil

	switch len(matches) {
	case 0:
		return 0, ErrNoHostWindow
	case 1:
		return matches[0], nil
	default:
		log.Printf("gst: overlay: this process has %d visible unowned top-level windows matching %q; "+
			"using the first. If the picture appears in the wrong one, that is why", len(matches), title)
		return matches[0], nil
	}
}

// ---------------------------------------------------------------------------
// The overlay
// ---------------------------------------------------------------------------

// overlay is the concrete PictureOverlay.
type overlay struct {
	// purpose is the word this surface is called by in its window name and in
	// every message it can produce — "picture" or "preview". It is set once at
	// construction and never written again, so it is outside the mutex.
	//
	// It exists because there are now two of these windows in one parent, and a
	// log line or a window enumeration that cannot tell them apart is a log line
	// nobody can act on. It changes nothing else: both windows are the same
	// class, the same styles and the same pump.
	purpose string

	// created carries the handle (or the failure) from the pump thread back to
	// NewPictureOverlay, which is synchronous.
	//
	// The window MUST be created on the thread that pumps it, so the constructor
	// cannot create it itself and hand it over.
	created chan error
	done    chan struct{}

	// mu guards everything the pump thread reads and the callers write.
	//
	// It is a leaf: nothing is taken while it is held, and nothing held while it
	// is taken. That is what lets SetRect be called from a Wails message handler
	// without any possibility of waiting on GStreamer.
	mu      sync.Mutex
	hwnd    syscall.Handle
	rect    PictureRect
	visible bool
	closed  bool

	// closeOnce makes Close idempotent and stops a second caller posting a
	// second quit to a thread that has already gone.
	closeOnce sync.Once
	closeErr  error
}

var _ PictureOverlay = (*overlay)(nil)

// NewPictureOverlay creates the SRT picture's window. It is NewOverlaySurface
// with this application's first surface named, and it is kept as its own
// function because app_picture.go has called it since before there was a second
// one and the picture's own purpose is not a thing that moves.
func NewPictureOverlay(title string) (PictureOverlay, error) {
	return NewOverlaySurface(title, "picture")
}

// NewOverlaySurface creates one overlay window as a child of this process's own
// application window, and starts the thread that pumps it.
//
// # There are TWO of these now, and what that does and does not change here
//
// purpose is a short lower-case word naming which surface this is — "picture"
// for the SRT programme monitor, gst.PreviewSurfacePurpose for the DeckLink
// confidence preview. It reaches the window's NAME and this file's log lines,
// and nothing else: both surfaces are the same window class, the same style
// words, the same disabled non-interactive child, each on its own pump thread.
//
// WHAT DOES NOT CHANGE IS THE Z-ORDER RULE, and that asymmetry against
// overlay_darwin.go is deliberate and is argued in full at the SetWindowPos call
// in apply. The short version: on this platform re-raising an already-correct
// window is a syscall that moves an entry in a sibling list, and on macOS it is
// -[NSView addSubview:], which takes a view hosting a live GL surface out of its
// window and puts it back. Two WS_CLIPSIBLINGS children that do not overlap have
// the same visible region whichever way round they are, so the two windows
// exchanging places costs nothing at all — while the unconditional raise remains
// the only defence there is against a WebView2 reorder, which arrives with no
// notification whatever.
//
// The two windows are otherwise entirely independent: separate HWNDs, separate
// pump threads, separate mutexes, and no ordering requirement between them. They
// do exchange places — whichever applied most recently is topmost — and on this
// platform that exchange is invisible and free. It is NOT free on the Mac, which
// is why the twin asks first; the difference between the two files is a
// difference in what the platform charges, not in what either of them wants.
//
// title is the host window's title, used to identify it; pass main.go's
// windowTitle. An empty title falls back to "the only visible unowned top-level
// window of this process", which is correct here but weaker.
//
// It returns ErrNoHostWindow if the application window does not exist yet, which
// is the expected answer before Wails has created it. The caller should treat
// that as "try again on the next layout update" rather than as a failure — see
// App.pictureOverlay in app_picture.go, which does exactly that.
//
// The window is created HIDDEN. It is opaque, and a black rectangle over the
// page before there is anything to show in it would cover the fallback picture
// the commentator is meant to be watching in the meantime.
func NewOverlaySurface(title, purpose string) (PictureOverlay, error) {
	if purpose == "" {
		purpose = "overlay"
	}
	parent, err := findHostWindow(title)
	if err != nil {
		return nil, err
	}
	// One class for both surfaces, registered exactly once per process. The
	// sync.Once was already required — this window is created and destroyed
	// several times in one run — and it is what makes a second surface cost
	// nothing here: RegisterClassEx would otherwise fail the second call with
	// ERROR_CLASS_ALREADY_EXISTS and the failure would be indistinguishable from
	// a genuine one.
	if _, err := registerOverlayClass(); err != nil {
		return nil, err
	}

	o := &overlay{
		purpose: purpose,
		created: make(chan error, 1),
		done:    make(chan struct{}),
	}
	go o.pump(parent)

	if err := <-o.created; err != nil {
		return nil, err
	}

	if procGetDpiForWindow.Find() == nil {
		dpi, _, _ := procGetDpiForWindow.Call(uintptr(parent))
		log.Printf("gst: overlay: the %s window was created as a child of window 0x%x, whose DPI is %d "+
			"(%.2fx). The rectangle arithmetic does NOT use this number — the page's own "+
			"devicePixelRatio does; it is logged so that a picture in the wrong place can be "+
			"told from a page reporting the wrong ratio", purpose, parent, dpi, float64(dpi)/96.0)
	}
	return o, nil
}

// pump creates the window and runs its message loop. It owns the window for its
// whole life and is the ONLY goroutine that calls a window-manipulating function
// on it.
//
// runtime.LockOSThread is mandatory and not a precaution: a window belongs to
// the thread that created it, GetMessage only ever returns messages for the
// calling thread, and a goroutine that migrated to another OS thread would pump
// an empty queue for ever while this window's messages piled up behind it.
// There is no UnlockOSThread on the way out, deliberately — the thread has had a
// window class callback, a subclass from gstd3d11 and a DirectX device
// associated with it, and letting the Go runtime hand it to unrelated
// goroutines afterwards is not worth the microsecond it saves. The runtime
// terminates a locked thread when its goroutine exits.
func (o *overlay) pump(parent syscall.Handle) {
	runtime.LockOSThread()
	defer close(o.done)

	hinst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString(overlayClassName)
	// The window NAME, which is not a title — this is a child window with no
	// caption and the operator never sees it. It is what an external tool shows:
	// Spy++, an accessibility inspector, a support engineer's window enumerator.
	// With two of these in one parent, a name that did not say which is which
	// would make the one diagnostic they offer useless.
	windowName, _ := syscall.UTF16PtrFromString("WSL Commentary " + o.purpose)

	// Created hidden (no WS_VISIBLE), disabled, clipping siblings, at 0x0.
	// Nothing is shown until SetVisible(true), and nothing is positioned until
	// SetRect; both are the frontend's explicit decisions.
	//
	// The extended style is NOT zero. It is WS_EX_NOPARENTNOTIFY, which is what
	// stops this call and the matching DestroyWindow from SENDING a message to a
	// parent window owned by a thread that is not pumping. See wsExNoParentNotify
	// — it is the difference between a clean quit and a teardown that abandons
	// the picture step every single time.
	hwnd, _, callErr := procCreateWindowExW.Call(
		overlayExStyle,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windowName)),
		overlayStyle,
		0, 0, 0, 0,
		uintptr(parent),
		0, hinst, 0,
	)
	if hwnd == 0 {
		o.created <- fmt.Errorf("gst: overlay: CreateWindowExW failed: %w", callErr)
		return
	}

	o.mu.Lock()
	o.hwnd = syscall.Handle(hwnd)
	o.mu.Unlock()
	o.created <- nil

	var m msgW
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			// 0 is WM_QUIT, -1 is an error on a handle that has gone. Either way
			// this thread's window is finished.
			break
		}

		switch m.message {
		case wmAppWake:
			// A layout or visibility change. The wish is read from the fields
			// rather than from the message, so a burst of posts collapses into
			// one apply — which is what a resize drag produces and what the
			// alternative, one SetWindowPos per mouse move, makes visible as
			// tearing.
			o.apply()
			continue
		case wmAppQuit:
			o.destroy()
			procPostQuitMessage.Call(0)
			continue
		}

		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	// Belt and braces: if the loop ended by an error rather than by wmAppQuit,
	// the window still has to go.
	o.destroy()
}

// apply moves and shows or hides the window to match the recorded wish. It runs
// ONLY on the pump thread.
func (o *overlay) apply() {
	o.mu.Lock()
	hwnd, rect, visible, closed := o.hwnd, o.rect, o.visible, o.closed
	o.mu.Unlock()

	if hwnd == 0 || closed {
		return
	}
	if alive, _, _ := procIsWindow.Call(uintptr(hwnd)); alive == 0 {
		return
	}

	// An empty rectangle is a hide, not a zero-sized window. A zero-sized window
	// still exists, still sits in the z-order and still makes gstd3d11 resize a
	// swapchain to nothing, which some drivers dislike.
	show := visible && !rect.Empty()

	if !show {
		procShowWindow.Call(uintptr(hwnd), swHide)
		return
	}

	// SWP_NOACTIVATE, and HWND_TOP rather than SWP_NOZORDER. The child is raised
	// above its siblings on every apply because WebView2 reorders its own
	// windows without notice, and it must never take the activation from the
	// page while doing it.
	//
	// # AND IT STAYS UNCONDITIONAL. DO NOT MAKE IT MATCH THE MAC.
	//
	// overlay_darwin.go used to raise on every apply too, and no longer does: it
	// asks whether anything foreign is above it first, and moves only if
	// something is. That is not a better idea that this file has yet to catch up
	// with. It is a different cost.
	//
	// The only way AppKit offers to reorder an existing subview is
	// -[NSView addSubview:], which takes the view OUT of its superview and puts
	// it back — through a view that is hosting a live GL surface. SetWindowPos
	// does nothing of the kind: it moves an entry in a z-order list, the HWND is
	// never unparented, and gstd3d11's swapchain is bound to the HWND and not to
	// its place among its siblings. The window is not recreated, the swapchain is
	// not touched, and the device is not reset.
	//
	// Tier 3 puts a SECOND overlay child — the DeckLink confidence preview — in
	// the same parent, which is what forced the question on the Mac: with two,
	// only one can be topmost, so each would displace the other on every apply,
	// for ever. The same exchange happens here and is worth nothing: both children
	// are WS_CLIPSIBLINGS and they do not overlap, so their relative order does
	// not change either one's visible region, no invalidation is generated, and
	// nothing is repainted. What it costs is one SetWindowPos per apply, which
	// this file was already paying for the move.
	//
	// Making it conditional would mean a GetWindow(GW_HWNDPREV) walk to buy that
	// syscall back, and would put a test in front of the ONE defence there is
	// against a WebView2 reorder that arrives with no notification at all. This
	// is the on-air platform; the unconditional raise is what has shipped through
	// every match. Reasoned from the API contracts and from what this file
	// already documents, NOT re-measured — the cgo half of this package cannot be
	// built on the machine the port is being done on.
	procSetWindowPos.Call(
		uintptr(hwnd), hwndTop,
		uintptr(int32(rect.X)), uintptr(int32(rect.Y)),
		uintptr(int32(rect.W)), uintptr(int32(rect.H)),
		swpNoActivate|swpShowWindow,
	)
}

// destroy destroys the window. It runs ONLY on the pump thread, because
// DestroyWindow must be called by the thread that created the window — it fails
// with ERROR_ACCESS_DENIED from any other, silently leaving the window on
// screen.
func (o *overlay) destroy() {
	o.mu.Lock()
	hwnd := o.hwnd
	o.hwnd = 0
	o.mu.Unlock()

	if hwnd == 0 {
		return
	}
	if ok, _, callErr := procDestroyWindow.Call(uintptr(hwnd)); ok == 0 {
		log.Printf("gst: overlay: DestroyWindow(0x%x) failed: %v", hwnd, callErr)
	}
}

// pictureWindowAlive reports whether handle still names a live window.
//
// It is the picture monitor's guard against rebuilding a d3d11videosink into a
// window that has been destroyed. The overlay is a CHILD of the application
// window (see the file header), so an ordinary application close destroys this
// HWND along with its parent; d3d11videosink then reports "Output window was
// closed", and without this guard the monitor's reconnect loop would build a
// fresh sink into a handle that no longer names anything — which wedges inside
// gstd3d11 with no timeout and cannot be interrupted, leaving a process that
// will not exit. IsWindow is the exact question, and it is the same call apply()
// already makes before it touches the window. A zero handle is not a window.
func pictureWindowAlive(handle uintptr) bool {
	if handle == 0 {
		return false
	}
	alive, _, _ := procIsWindow.Call(handle)
	return alive != 0
}

// wake posts the pump a nudge. It never blocks, which is the property the whole
// file is arranged around; see the file header.
func (o *overlay) wake() error {
	o.mu.Lock()
	hwnd, closed := o.hwnd, o.closed
	o.mu.Unlock()

	if closed || hwnd == 0 {
		return errors.New("gst: overlay: the " + o.purpose + " window is closed")
	}
	if ok, _, callErr := procPostMessageW.Call(uintptr(hwnd), wmAppWake, 0, 0); ok == 0 {
		return fmt.Errorf("gst: overlay: could not post to the %s window: %w", o.purpose, callErr)
	}
	return nil
}

// Handle returns the HWND. See PictureOverlay.
func (o *overlay) Handle() uintptr {
	o.mu.Lock()
	defer o.mu.Unlock()
	return uintptr(o.hwnd)
}

// SetRect records the rectangle and wakes the pump. See PictureOverlay.
func (o *overlay) SetRect(r PictureRect) error {
	o.mu.Lock()
	unchanged := o.rect == r
	o.rect = r
	o.mu.Unlock()

	if unchanged {
		// A resize observer fires on every frame of an animation and reports the
		// same integers most of the time. Not posting is not an optimisation for
		// its own sake: every post is a SetWindowPos, and a SetWindowPos on a
		// window gstd3d11 has subclassed is a swapchain resize.
		return nil
	}
	return o.wake()
}

// SetVisible records the visibility and wakes the pump. See PictureOverlay.
func (o *overlay) SetVisible(visible bool) error {
	o.mu.Lock()
	unchanged := o.visible == visible
	o.visible = visible
	o.mu.Unlock()

	if unchanged {
		return nil
	}
	return o.wake()
}

// Close destroys the window and stops the pump. See PictureOverlay.
//
// It is bounded and it says so when the bound is reached. See the file header
// on the exit path, and App.teardown, which gives this its own budget on top.
func (o *overlay) Close() error {
	o.closeOnce.Do(func() {
		o.mu.Lock()
		hwnd := o.hwnd
		o.closed = true
		o.mu.Unlock()

		if hwnd != 0 {
			if ok, _, callErr := procPostMessageW.Call(uintptr(hwnd), wmAppQuit, 0, 0); ok == 0 {
				// The queue is gone, which usually means the thread already is.
				// Fall through to the wait, which will find done closed.
				log.Printf("gst: overlay: could not post the quit to the %s window: %v", o.purpose, callErr)
			}
		}

		timer := time.NewTimer(overlayCloseBudget)
		defer timer.Stop()
		select {
		case <-o.done:
		case <-timer.C:
			// WRAPPED, not merely described. The sentence is for the log; the
			// wrapped ErrAbandonedThread is what App.teardownStep tests for, and
			// it is the only reason that teardown scores this step as unfinished
			// and ends the process with TerminateProcess instead of returning
			// into ExitProcess with this thread still inside DestroyWindow or
			// gstd3d11's subclass procedure. See gst.ErrAbandonedThread.
			o.closeErr = fmt.Errorf(
				"gst: overlay: the %s window's message thread did not stop within %s and has been "+
					"ABANDONED; the window may remain on screen until the process exits: %w",
				o.purpose, overlayCloseBudget, ErrAbandonedThread)
			log.Print(o.closeErr)
		}
	})
	return o.closeErr
}
