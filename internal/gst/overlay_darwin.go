//go:build darwin && cgo

// overlay_darwin.go is the NATIVE SURFACE the picture is drawn on when this
// application runs on macOS: an NSView added to the Wails window's contentView
// as a SIBLING of the WKWebView, which glimagesink renders into through
// GstVideoOverlay.
//
// Owner: WP-P, with picture_cgo.go. Read overlay_windows.go first. It carries
// the argument for why the picture cannot go through the page at all, and why
// the surface must be one nothing else in the process touches; this file is that
// design in Cocoa rather than a second design, and only the parts that genuinely
// differ are argued again here.
//
// The correspondence, line for line:
//
//	Windows                             macOS
//	-------                             -----
//	EnumWindows + title match           [NSApp windows] + title match
//	CreateWindowExW(WS_CHILD, parent)   [contentView addSubview:v positioned:NSWindowAbove]
//	SetWindowPos on the pump thread     -[NSView setFrame:] on the MAIN QUEUE
//	ShowWindow(SW_HIDE)                 -[NSView setHidden:YES]
//	a dedicated GetMessage pump thread  dispatch_async(dispatch_get_main_queue())
//	DestroyWindow on the pump thread    -[NSView removeFromSuperview] on the main queue
//	d3d11videosink into an HWND         glimagesink into an NSView*
//
// The correspondence breaks in exactly ONE place, and it is the z-order. Windows
// re-applies HWND_TOP on every apply and that is free, because SetWindowPos moves
// an entry in a list and never unparents the HWND. AppKit has no such call: the
// only way to reorder an existing subview is to addSubview: it again, which takes
// it out of its window and puts it back — through a view that is hosting a live
// GL surface. So this side asks first, and asks the question that is actually
// load-bearing: am I above every sibling that is not one of MY OWN KIND. See
// WSLCommsOverlayView and the raise in wslcomms_overlay_apply, both of which
// exist because Tier 3 puts a second one of these surfaces in the same window.
//
// # It is deliberately dumb, for the same reason
//
// This view paints nothing but its own black backing layer, decodes nothing and
// knows nothing about GStreamer. It exists, it has a pointer, it can be moved
// and it can be hidden. glimagesink takes the pointer and adds its OWN
// GstGLNSView inside it — measured, in a real Wails window: after PLAYING our
// view had exactly one subview, a GstGLNSView at 0,0 filling it. So before the
// sink is attached the view is black, and after it is attached the sink owns
// every pixel.
//
// Giving glimagesink a view of OUR OWN rather than the Wails contentView or the
// WKWebView itself is the same decision overlay_windows.go argues at length, and
// it matters more here, not less: the sink ADDS A SUBVIEW to whatever handle it
// is given and removes it again on teardown. Handed the contentView it would be
// a sibling of the WKWebView with a frame nobody owns; handed the WKWebView it
// would be a subview of a view WebKit reserves entirely to itself.
//
// # Threading, which is the part that can hang the application
//
// EVERY APPKIT CALL IN THIS FILE RUNS ON THE MAIN THREAD. That is not a
// convention, it is AppKit's rule, and GStreamer breaks it by default: the bus
// sync handler runs on whichever streaming thread posted, the pad probes run on
// the decoder's thread, and Wails' bound methods run on a dispatcher goroutine
// that is not the main one either. So every entry point below is
// "record the wish, dispatch_async to the main queue, return".
//
// dispatch_async, and NOT dispatch_sync, everywhere except construction. A
// dispatch_sync from a goroutine blocks until the main thread services the
// queue, which makes it a deadlock the moment the main thread is itself waiting
// on the caller — and the main thread waiting on Go is not hypothetical here,
// it is what App.teardown does on every quit. Construction is the one place a
// value has to come back, so it is the one dispatch_sync, and it checks
// [NSThread isMainThread] first because a dispatch_sync to the queue you are
// already running on deadlocks instantly and unconditionally.
//
// # AND THERE IS NOT ALWAYS A MAIN LOOP AT ALL
//
// [NSThread isMainThread] answers "am I the main thread", which is not the same
// question as "is anybody DRAINING the main queue", and only the second one
// makes a dispatch_sync safe. Under `go test` the answer to the second is no:
// the test binary's main goroutine never calls -[NSApplication run], so the main
// queue has no servicer and a dispatch_sync onto it waits for ever. Measured, in
// a 12-line Objective-C reproduction of exactly this shape (worker thread
// dispatch_syncs, main thread parked in pthread_join): the block never ran and
// the process had to be killed from outside.
//
// That is not a theoretical concern about tests. app_return_test.go sweeps every
// bound method reflectively to prove none of them hands back a secret, and one of
// those methods is StartPicture, which reaches NewPictureOverlay. Before the
// guard below, that single call wedged the ENTIRE tagged root-package suite: it
// hung until `go test`'s ten-minute timeout killed the binary, so every test
// declared after it never ran at all and any regression they would have caught
// was invisible. Measured twice at about 603 seconds each.
//
// So construction asks the real question first — is there an NSApplication and
// is it running — and answers ErrNoHostWindow when there is not, WITHOUT
// dispatching anything. That is the same answer the Windows twin gives when
// EnumWindows finds nothing, it is what App.SetPictureRect already treats as
// "not yet", and it costs one global read plus one BOOL. A timed wait was the
// alternative and is worse: it would have to heap-allocate the out-parameter so
// a late block could not write into a Go frame that had already returned, and it
// would turn a deterministic answer into a race against a timeout.
//
// # THE MAIN RUN LOOP IS ALREADY DEAD BY THE TIME Close IS CALLED
//
// This is the macOS equivalent of the WM_PARENTNOTIFY trap on Windows: a shape
// of the host framework that turns an ordinary quit into a hang if it is not
// designed around. Wails v2's app.Run is
//
//	f.frontend.RunMainLoop()   // -[NSApplication run], returns when it stops
//	f.frontend.WindowClose()
//	a.shutdownCallback(a.ctx)  // OnShutdown -> App.teardown
//
// so OnShutdown runs AFTER -[NSApp run] has RETURNED. From that moment nothing
// services the main queue: a dispatch_async is queued and never executed, and a
// dispatch_sync never returns. App.teardown then runs its steps on a goroutine
// while the main thread is parked in the select that waits for them, so this
// file's Close is called off the main thread, at the one moment the main queue
// is guaranteed to be unable to answer.
//
// Close is therefore FIRE AND FORGET and has no budget: it marks the overlay
// closed, posts the removal, and returns nil. It cannot hang, and — unlike the
// Windows twin — it never reports gst.ErrAbandonedThread, because there is
// nothing here to abandon. There is no thread of ours; the main queue belongs to
// AppKit, and the block that is left on it is a removeFromSuperview against a
// window the process is about to destroy anyway. Wrapping ErrAbandonedThread
// would make App.teardown score the picture step as unfinished and end EVERY
// ordinary macOS quit with the hard exit, which is precisely the failure
// wsExNoParentNotify exists to prevent on the other platform.
//
// What that leaves on the floor is one NSView, unreleased, in a process that is
// exiting. That is the whole cost, and it is stated rather than hidden.
//
// # Coordinates: the rectangle arrives in PIXELS and AppKit wants POINTS
//
// Two conversions, and getting either wrong puts the picture somewhere the
// operator can see is wrong but cannot explain.
//
//  1. ORIGIN. A native child HWND is placed from the top-left. An NSView's frame
//     is placed from the BOTTOM-left of its superview, because the Wails
//     contentView is not flipped. So y has to be turned over against the
//     superview's height, which is the flip cocoaOverlayFrame performs and
//     picture_test.go pins.
//
//  2. SCALE. PictureRect is PHYSICAL PIXELS — the page's CSS pixels multiplied
//     by the page's own devicePixelRatio, see gst.ScaleRect — and AppKit frames
//     are in POINTS. The factor is the host window's backingScaleFactor,
//     measured at 2.0 on the development machine, and dividing by it is not an
//     approximation: on macOS window.devicePixelRatio is the backing scale
//     multiplied by the page zoom, so pixels-divided-by-backing-scale gives back
//     exactly the points the page laid out in, page zoom included. Reading the
//     scale here rather than on the Go side is deliberate for the same reason
//     ScaleRect trusts the page: dragging the window to a second display changes
//     backingScaleFactor with no setting changing anywhere, so it is read fresh
//     on the main thread on every apply.
//
// # What is given up by this approach
//
// The view is positioned by the frontend and by nothing else. It does not
// participate in CSS layout, it is not clipped by an overflow rule, it does not
// scroll, and it does not disappear when a modal opens — exactly as on Windows.
// The one softening is the autoresizing mask below, which keeps it the same
// distance from the TOP of the window while a live resize is in flight, so it
// does not swim downwards for the frames between the drag and the page's
// ResizeObserver catching up.
package gst

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa

#import <Cocoa/Cocoa.h>
#include <stdlib.h>

// The whole Cocoa surface of the picture overlay. It is small on purpose: every
// line of it runs on the main thread, and the only way to keep that true is for
// there to be very little of it.

enum {
    WSLCOMMS_OVERLAY_OK        = 0,
    WSLCOMMS_OVERLAY_NO_HOST   = 1,  // Wails has not made the window yet
    WSLCOMMS_OVERLAY_NO_CONTENT = 2, // it has, and it has no contentView
    WSLCOMMS_OVERLAY_NO_MAIN_LOOP = 3, // nothing is draining the main queue
};

// wslcomms_overlay_info is what construction reports back. matches is the number
// of windows that satisfied every test, so an ambiguous match can be logged
// rather than silently resolved; scale and contentHeight are for the same
// diagnostic purpose as the GetDpiForWindow log line on Windows and are NOT used
// in any arithmetic on the Go side.
typedef struct {
    void   *view;
    int     matches;
    double  scale;
    double  contentWidth;
    double  contentHeight;
} wslcomms_overlay_info;

// WSLCommsOverlayView behaves in every respect like the NSView it derives from.
// It exists so that one of these views can RECOGNISE ANOTHER, and for nothing
// else — there is not a line of behaviour in it, on purpose, because the whole
// argument of this file is that the surface must be something nothing in the
// process reasons about.
//
// It is needed because there is about to be more than one of them. Tier 3 puts a
// DeckLink confidence preview on the same contentView as the SRT return picture,
// and the moment there are two the z-order question changes shape: what each view
// needs is to be ABOVE THE WEBVIEW, and what it emphatically does not need is to
// be above its own kind. Two opaque rectangles that do not overlap have no
// ordering requirement between themselves, and imposing one on them makes them
// fight — see the raise in wslcomms_overlay_apply for what that fight costs.
//
// Recognising our own by class rather than recognising the WEBVIEW by its class
// is deliberate. Naming the webview means importing WebKit into this preamble and
// hard-coding WailsWebView, which is a Wails implementation detail we do not
// control; and it would still be the wrong test the day Wails adds a sibling of
// its own that the picture also has to be above. "Above every sibling that is not
// one of ours" is the property that survives both, and it costs one empty class.
@interface WSLCommsOverlayView : NSView
@end

@implementation WSLCommsOverlayView
@end

// wslcomms_overlay_raise_above answers "is this view low enough to need moving,
// and if it is, what does it have to go above". nil means DO NOTHING AT ALL,
// which on a settled window is the answer on every single apply.
//
// subs is the superview's subview list, BOTTOM FIRST, and self_idx is where our
// view sits in it. The answer is the topmost sibling above us that is not one of
// ours: inserting above that one clears every foreign sibling in a single move,
// and leaves our own kind to fall wherever they fall.
//
// overlayRaiseAbove in overlay_zorder.go is the same scan written in Go, unit
// tested at Gate A — where this file is not even compiled — and
// overlay_zorder_test.go asserts that this function is still spelled the same
// way, for the same reason picture_test.go pins the coordinate flip: two
// expressions of one rule drift.
static NSView *wslcomms_overlay_raise_above(NSArray *subs, NSUInteger self_idx) {
    for (NSUInteger i = [subs count]; i > self_idx + 1; i--) {
        NSView *other = [subs objectAtIndex:(i - 1)];
        if (![other isKindOfClass:[WSLCommsOverlayView class]]) return other;
    }
    return nil;
}

// wslcomms_overlay_create_on_main finds the host window and installs the view.
// MAIN THREAD ONLY.
//
// The search is findHostWindow's, test for test, narrowed until it can only
// match one thing: a window of THIS process (which is all [NSApp windows] ever
// returns), visible, with no parent window — the analogue of the GW_OWNER test,
// which excludes sheets and attached panels — and carrying the expected title.
static int wslcomms_overlay_create_on_main(const char *title, wslcomms_overlay_info *out) {
    NSString *want = nil;
    if (title != NULL && title[0] != '\0') {
        want = [NSString stringWithUTF8String:title];
    }

    NSWindow *host = nil;
    int matches = 0;
    for (NSWindow *w in [NSApp windows]) {
        if (![w isVisible]) continue;
        if ([w parentWindow] != nil) continue;
        if (want != nil && ![[w title] isEqualToString:want]) continue;
        if (host == nil) host = w;
        matches++;
    }
    if (host == nil) return WSLCOMMS_OVERLAY_NO_HOST;

    NSView *content = [host contentView];
    if (content == nil) return WSLCOMMS_OVERLAY_NO_CONTENT;

    // Created at 1x1, HIDDEN, and black. Nothing appears on the operator's
    // screen until the frontend has measured a rectangle AND there are pictures
    // to put in it; an opaque black rectangle over the fallback mosaic is worse
    // than the soft picture it covers.
    //
    // WSLCommsOverlayView rather than NSView so that a second one of these can
    // tell it apart from the webview; see the class above. It adds no behaviour
    // whatever, and glimagesink is handed the same NSView* it always was.
    NSView *view = [[WSLCommsOverlayView alloc] initWithFrame:NSMakeRect(0, 0, 1, 1)];
    [view setHidden:YES];

    // wantsLayer, so the view has a backing layer of its own before glimagesink
    // gives it one. Without it the view is transparent until the first frame and
    // whatever the page had drawn there shows through the gap.
    [view setWantsLayer:YES];
    [[view layer] setBackgroundColor:[[NSColor blackColor] CGColor]];

    // NSViewMinYMargin means "the margin BELOW me is the flexible one", which in
    // an unflipped superview is what keeps a view the same distance from the
    // TOP as the window is resized. The frontend re-measures on every layout
    // change and will overwrite this within a frame; the mask is only for the
    // frames of a live resize drag that arrive before it does.
    [view setAutoresizingMask:NSViewMinYMargin];

    // positioned:NSWindowAbove relativeTo:nil is "above every sibling", which is
    // HWND_TOP on the other platform. The sibling that matters is Wails'
    // WailsWebView, which is added to the same contentView in
    // WailsContext.m's CreateWindow.
    //
    // The blunt form is right HERE and wrong in wslcomms_overlay_apply, and the
    // difference is worth stating because the two lines otherwise look like the
    // same line written twice. This view is one second old, it is 1x1, it is
    // hidden, and no sink has attached anything to it — so putting it on top of
    // a sibling overlay costs nothing, and that sibling will be back above it,
    // or not, without either of them caring. At apply time the same call would
    // be moving a live GL surface.
    [content addSubview:view positioned:NSWindowAbove relativeTo:nil];

    out->view = (void *)view;
    out->matches = matches;
    out->scale = [host backingScaleFactor];
    out->contentWidth = [content bounds].size.width;
    out->contentHeight = [content bounds].size.height;
    return WSLCOMMS_OVERLAY_OK;
}

// wslcomms_overlay_main_loop_is_running answers the question that makes a
// dispatch_sync onto the main queue safe or fatal: is there an NSApplication, and
// has it been started.
//
// Both reads are safe from any thread and neither creates anything. NSApp is a
// plain global that -[NSApplication sharedApplication] assigns — reading it does
// NOT bring the application object into existence, which is the whole reason the
// test is spelled this way round rather than as [NSApplication sharedApplication].
// -isRunning is a BOOL ivar set between run and stop. Measured on this Mac from a
// worker thread: nil/-1 before sharedApplication, non-nil with isRunning 0 after
// sharedApplication but before run, and neither read blocked or trapped.
//
// The isRunning half is not redundant with the nil half. A process can create the
// shared application and never run it — and the moment that matters most is the
// one at the other end, after -[NSApp run] has returned during shutdown, when
// NSApp is still perfectly non-nil and the queue is already dead.
static int wslcomms_overlay_main_loop_is_running(void) {
    return (NSApp != nil && [NSApp isRunning]) ? 1 : 0;
}

// wslcomms_overlay_create is the thread-safe wrapper. It is the ONE dispatch_sync
// in this file, because it is the one call that has to return a value.
//
// The [NSThread isMainThread] test is not defensive dressing: dispatch_sync onto
// the queue you are already running on deadlocks unconditionally, and this can
// legitimately be reached from the main thread by a Wails callback.
//
// The main-loop test in front of it is the one that keeps this call bounded. See
// "AND THERE IS NOT ALWAYS A MAIN LOOP AT ALL" in the file header: without it a
// caller under `go test` waits for ever on a queue nobody drains. It is checked
// only on the dispatching path, because a caller that is ALREADY on the main
// thread runs the work itself and needs no servicer — which is also what keeps
// the test from being a lie in the one case where the run loop is starting up and
// AppKit calls us before -run has flipped the flag.
static int wslcomms_overlay_create(const char *title, wslcomms_overlay_info *out) {
    if ([NSThread isMainThread]) {
        return wslcomms_overlay_create_on_main(title, out);
    }
    if (!wslcomms_overlay_main_loop_is_running()) {
        return WSLCOMMS_OVERLAY_NO_MAIN_LOOP;
    }
    __block int rc = WSLCOMMS_OVERLAY_NO_HOST;
    dispatch_sync(dispatch_get_main_queue(), ^{
        rc = wslcomms_overlay_create_on_main(title, out);
    });
    return rc;
}

// wslcomms_overlay_frame is the coordinate conversion, and it is the only piece
// of arithmetic in this file.
//
// x, y, w, h are PHYSICAL PIXELS from the TOP-LEFT of the superview. The result
// is POINTS from the BOTTOM-LEFT, which is what -[NSView setFrame:] wants.
// cocoaOverlayFrame in picture.go is the same expression in Go, unit-tested at
// Gate A, and picture_test.go asserts that this line still spells it the same
// way — because a sign error here is a picture in the wrong half of the window
// and nothing else in the process would notice.
static NSRect wslcomms_overlay_frame(NSView *sup, double scale, int x, int y, int w, int h) {
    CGFloat ph = (CGFloat)h / scale;
    CGFloat py = [sup bounds].size.height - ((CGFloat)y / scale) - ph;
    return NSMakeRect((CGFloat)x / scale, py, (CGFloat)w / scale, ph);
}

// wslcomms_overlay_apply moves and shows or hides the view. It NEVER BLOCKS: the
// whole body is a block posted to the main queue, which is what makes it safe to
// call from a Wails message handler and from a GStreamer streaming thread alike.
//
// show is the AND that app_picture.go computed — the page wants it, the monitor
// is SHOWING, and the rectangle has area — so a hide here is a hide and never a
// zero-sized frame. A zero-sized frame would make glimagesink resize its GL
// surface to nothing, which is the same hazard d3d11videosink's swapchain has.
static void wslcomms_overlay_apply(void *v, int x, int y, int w, int h, int show) {
    NSView *view = (NSView *)v;
    if (view == nil) return;

    // The block RETAINS view for as long as it is queued, so an apply still in
    // flight when Close runs finds a live object and a nil superview rather than
    // a freed one.
    dispatch_async(dispatch_get_main_queue(), ^{
        NSView *sup = [view superview];
        if (sup == nil) return;  // closed, or never installed
        if (!show) {
            [view setHidden:YES];
            return;
        }

        double scale = (double)[[view window] backingScaleFactor];
        if (!(scale > 0)) scale = 1.0;
        [view setFrame:wslcomms_overlay_frame(sup, scale, x, y, w, h)];

        // THE RAISE, AND WHY IT IS A TEST RATHER THAN AN UNCONDITIONAL addSubview.
        //
        // The Windows twin re-applies HWND_TOP on every apply because WebView2
        // reorders its own windows with no notification. Here the sibling list is
        // Wails' and changes only when Wails changes it — and the cost of raising
        // when it was not needed is far higher on this side, because the only way
        // AppKit offers to reorder an existing subview is -[NSView addSubview:],
        // which takes the view OUT OF ITS WINDOW and puts it back. This view is
        // hosting a GL surface that glimagesink has attached to that window. The
        // test in front of the call is therefore not an optimisation; it is what
        // keeps the surface attached.
        //
        // WHAT THE TEST USED TO BE, AND WHY IT HAD TO CHANGE BEFORE TIER 3.
        //
        // It was "am I the last subview", which is true, cheap and entirely
        // sufficient while there is exactly one of these views in the process —
        // which is the case up to and including today. With TWO, which is what a
        // DeckLink confidence preview alongside the SRT picture means, only one of
        // them can be last. The other one would find the condition true on EVERY
        // apply and re-add itself; and re-adding puts the FIRST one second, so the
        // next apply on that one re-adds it in turn. Two surfaces taking each
        // other's place, forever. Every rectangle change is one of those applies,
        // and during a live resize drag a rectangle change is every frame, on the
        // surface carrying the commentator's programme picture.
        //
        // What actually matters is being above the WEBVIEW, not being last in an
        // absolute sense, so that is what is asked. On a settled window the answer
        // is nil and this whole block is one array read and one loop that finds
        // nothing.
        NSArray *subs = [sup subviews];
        NSUInteger self_idx = [subs indexOfObjectIdenticalTo:view];
        if (self_idx != NSNotFound) {
            // NSNotFound cannot happen — sup came from [view superview] — but the
            // arithmetic below is on an unsigned index and the failure mode of
            // getting that wrong is a wild objectAtIndex:, so it is asked anyway.
            NSView *below = wslcomms_overlay_raise_above(subs, self_idx);
            if (below != nil) {
                [sup addSubview:view positioned:NSWindowAbove relativeTo:below];
            }
        }
        [view setHidden:NO];
    });
}

// wslcomms_overlay_close takes the view out of the window and releases it.
//
// It is fire and forget when it is not already on the main thread, and it is
// unapologetic about that: read the "THE MAIN RUN LOOP IS ALREADY DEAD" section
// of the file header. Waiting here would be waiting for a queue that App.teardown
// has already stopped anybody servicing.
static void wslcomms_overlay_close(void *v) {
    NSView *view = (NSView *)v;
    if (view == nil) return;

    if ([NSThread isMainThread]) {
        [view removeFromSuperview];
        [view release];
        return;
    }
    dispatch_async(dispatch_get_main_queue(), ^{
        [view removeFromSuperview];
        [view release];
    });
}
*/
import "C"

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"unsafe"
)

// ---------------------------------------------------------------------------
// The contract
// ---------------------------------------------------------------------------

// PictureOverlay is the surface the SRT picture is drawn in.
//
// It is the same interface overlay_windows.go declares, method for method,
// because app_picture.go drives both and must not know which it has. Where the
// PROMISES differ the difference is written on the method.
//
// It is created once and outlives every pipeline. The picture monitor is stopped
// and rebuilt whenever the endpoint or the configuration changes, and destroying
// the surface with it would mean a reconnect had nowhere to draw.
//
// All methods are safe for concurrent use, and none of them waits on the main
// thread. See the file header.
type PictureOverlay interface {
	// Handle returns the NSView* as a uintptr, for PictureOpts.WindowHandle.
	// gst_video_overlay_set_window_handle takes it directly: on macOS the
	// guintptr window handle IS an NSView pointer, proven against glimagesink in
	// a real Wails window. It is zero once Close has run.
	Handle() uintptr

	// SetRect places the view, in PHYSICAL pixels relative to the top-left of the
	// host window's content area. The conversion to AppKit's points and
	// bottom-left origin happens on the main thread, at apply time, against the
	// window's current backingScaleFactor; see the file header.
	//
	// An empty rectangle hides the view rather than failing: a page that has not
	// laid out yet legitimately produces one.
	//
	// It returns as soon as the request is recorded. The move happens on the main
	// queue, and successive calls coalesce — only the most recent rectangle is
	// ever applied, which is what a resize drag needs.
	SetRect(r PictureRect) error

	// SetVisible shows or hides the view.
	//
	// Hiding is not cosmetic. The view is OPAQUE and above every sibling in its
	// rectangle: anything the page draws there — Settings, the mixer drawer, a
	// modal, or the fallback mosaic when SRT is not connected — is invisible
	// underneath it. Every one of those cases is a call to this method, made
	// explicitly by the frontend rather than inferred here.
	SetVisible(visible bool) error

	// Close takes the view out of the window and releases it.
	//
	// It is idempotent, it never blocks, and it always returns nil. UNLIKE THE
	// WINDOWS TWIN IT NEVER REPORTS ErrAbandonedThread, because there is no
	// thread of ours to abandon — the removal is a block posted to AppKit's main
	// queue, which by the time App.teardown runs is a queue nobody is servicing.
	// See the file header.
	Close() error
}

// ErrNoHostWindow is returned by NewPictureOverlay when this process has no
// visible top-level window yet. It is a normal condition, not a fault: it means
// the call arrived before Wails created the window, and the caller should try
// again.
//
// It is declared per platform, exactly as the interface is, so that each build
// has one and only one of them. App code tests for it with errors.Is.
var ErrNoHostWindow = errors.New("gst: the application window does not exist yet")

// ---------------------------------------------------------------------------
// The overlay
// ---------------------------------------------------------------------------

// overlay is the concrete PictureOverlay.
//
// It has no thread and no channels, which is the whole difference from the
// Windows twin: there, a window belongs to the thread that created it and that
// thread has to pump, so the file is built around a dedicated OS-locked
// goroutine. Here the thread that owns the view is AppKit's main thread, it
// already exists, it is already being pumped by somebody else, and the only way
// to reach it is dispatch_async. So the mutex below guards four fields and
// nothing else.
type overlay struct {
	// mu is a leaf: nothing is taken while it is held, and nothing is held while
	// it is taken. That is what lets SetRect be called from a Wails message
	// handler with no possibility of waiting on GStreamer or on AppKit.
	//
	// view is kept as an unsafe.Pointer rather than as the uintptr the interface
	// hands out, because it is a C pointer and round-tripping it through uintptr
	// on every call is exactly the pattern go vet's unsafeptr check exists to
	// stop. It points at an NSView, which is not Go memory, so storing it in a Go
	// struct is allowed and the collector has nothing to do with it.
	mu      sync.Mutex
	view    unsafe.Pointer
	rect    PictureRect
	visible bool
	closed  bool

	// closeOnce makes Close idempotent and stops a second caller posting a second
	// release of a view that has already been released.
	closeOnce sync.Once
}

var _ PictureOverlay = (*overlay)(nil)

// NewPictureOverlay installs the picture view in this process's own application
// window.
//
// title is the host window's title, used to identify it; pass main.go's
// windowTitle, which is the string Wails gives -[NSWindow setTitle:]. An empty
// title falls back to "the only visible parentless window of this process",
// which is correct here but weaker.
//
// It returns ErrNoHostWindow if the application window does not exist yet, which
// is the expected answer before Wails has created it. The caller should treat
// that as "try again on the next layout update" rather than as a failure — see
// App.pictureOverlay in app_picture.go, which does exactly that.
//
// It also returns ErrNoHostWindow, wrapped in a sentence saying so, when this
// process has no RUNNING NSApplication — which is every `go test` binary and
// every moment after -[NSApp run] has returned. That case is answered without
// touching AppKit at all; see "AND THERE IS NOT ALWAYS A MAIN LOOP AT ALL" in
// the file header for why it has to be, and what it cost before it was.
//
// It never blocks for longer than one main-queue turn, and on the two paths above
// it does not block at all.
//
// The view is created HIDDEN, at 1x1. Nothing appears on the operator's screen
// until the frontend has told the overlay where the picture goes and the monitor
// has reported that there are pictures to put in it.
func NewPictureOverlay(title string) (PictureOverlay, error) {
	cTitle := C.CString(title)
	defer C.free(unsafe.Pointer(cTitle))

	// info is Go memory holding no Go pointers, filled in synchronously by C
	// before the call returns, which is what the cgo pointer rules require.
	var info C.wslcomms_overlay_info
	switch rc := C.wslcomms_overlay_create(cTitle, &info); rc {
	case C.WSLCOMMS_OVERLAY_OK:
	case C.WSLCOMMS_OVERLAY_NO_HOST:
		return nil, ErrNoHostWindow
	case C.WSLCOMMS_OVERLAY_NO_MAIN_LOOP:
		// Wrapped rather than returned bare, so errors.Is still puts this on
		// SetPictureRect's "not yet, and that is fine" path while the sentence a
		// reader actually sees is the true one. "The window does not exist yet"
		// would be a guess here: we never got far enough to look.
		return nil, fmt.Errorf("gst: overlay: this process has no running NSApplication, "+
			"so nothing would service the main queue the picture view has to be created on: %w", ErrNoHostWindow)
	case C.WSLCOMMS_OVERLAY_NO_CONTENT:
		return nil, errors.New("gst: overlay: the application window has no content view to put the picture in")
	default:
		return nil, fmt.Errorf("gst: overlay: could not create the picture view (code %d)", int(rc))
	}
	if info.view == nil {
		return nil, errors.New("gst: overlay: the picture view was reported created and is nil")
	}

	if int(info.matches) > 1 {
		log.Printf("gst: overlay: this process has %d visible parentless windows titled %q; "+
			"using the first. If the picture appears in the wrong one, that is why",
			int(info.matches), title)
	}

	// The same diagnostic the Windows twin logs its DPI for, and the same
	// warning about it: the scale IS used, on the main thread, to turn physical
	// pixels into points — but the rectangle itself comes from the page's own
	// devicePixelRatio, and the two are different measurements taken at different
	// moments. Logging both is what lets a picture in the wrong place be told
	// from a page reporting the wrong ratio.
	log.Printf("gst: overlay: picture view %p installed in a %.0fx%.0f pt content view, "+
		"backingScaleFactor %.2f. Rectangles arrive in PHYSICAL PIXELS from the page and are "+
		"divided by that factor on the main thread; the page's own devicePixelRatio is what "+
		"produced them",
		info.view, float64(info.contentWidth), float64(info.contentHeight), float64(info.scale))

	return &overlay{view: info.view}, nil
}

// Handle returns the NSView*. See PictureOverlay.
func (o *overlay) Handle() uintptr {
	o.mu.Lock()
	defer o.mu.Unlock()
	return uintptr(o.view)
}

// SetRect records the rectangle and posts the move. See PictureOverlay.
func (o *overlay) SetRect(r PictureRect) error {
	o.mu.Lock()
	unchanged := o.rect == r
	o.rect = r
	o.mu.Unlock()

	if unchanged {
		// A ResizeObserver fires on every frame of an animation and reports the
		// same integers most of the time. Not posting is not an optimisation for
		// its own sake: every post is a setFrame on a view that glimagesink has
		// put a GL surface inside, and that is a surface resize.
		return nil
	}
	return o.apply()
}

// SetVisible records the visibility and posts the change. See PictureOverlay.
func (o *overlay) SetVisible(visible bool) error {
	o.mu.Lock()
	unchanged := o.visible == visible
	o.visible = visible
	o.mu.Unlock()

	if unchanged {
		return nil
	}
	return o.apply()
}

// apply posts the current wish to the main queue. It never blocks.
//
// The wish is read from the fields rather than passed in, so a burst of calls
// collapses into whatever the last one wanted — which is what a resize drag
// produces, and what the alternative, one setFrame per mouse move, makes visible
// as tearing.
//
// # Why the wish can be carried in the block rather than re-read on the far side
//
// The Windows twin deliberately does the opposite: its pump thread re-reads the
// fields when it wakes, so that a post which overtakes another cannot apply a
// stale rectangle. There is no equivalent risk here, for two reasons that hold
// together and are written down because either one changing breaks it. Every
// caller — SetPictureRect, SetPictureVisible, applyPictureVisibility — holds
// App.picViewMu across the call, so two applies are never in flight at once;
// and dispatch_async onto a serial queue runs blocks in the order they were
// submitted. IF ANYTHING EVER CALLS SetRect WITHOUT THAT LOCK, the far side has
// to start re-reading instead.
func (o *overlay) apply() error {
	o.mu.Lock()
	view, rect, visible, closed := o.view, o.rect, o.visible, o.closed
	o.mu.Unlock()

	if closed || view == nil {
		// Fail closed. A SetRect that quietly succeeded on a closed overlay would
		// let app_picture.go believe the picture had been placed.
		return errors.New("gst: overlay: the picture view is closed")
	}

	// An empty rectangle is a hide, not a zero-sized view. A zero-sized view
	// still exists, still sits in the sibling order, and still makes glimagesink
	// resize a GL surface to nothing.
	show := 0
	if visible && !rect.Empty() {
		show = 1
	}
	C.wslcomms_overlay_apply(view,
		C.int(rect.X), C.int(rect.Y), C.int(rect.W), C.int(rect.H), C.int(show))
	return nil
}

// Close takes the view out of the window. See PictureOverlay, and the file
// header's section on the main run loop being dead by the time this is reached.
func (o *overlay) Close() error {
	o.closeOnce.Do(func() {
		o.mu.Lock()
		view := o.view
		o.view = nil
		o.closed = true
		o.mu.Unlock()

		if view != nil {
			C.wslcomms_overlay_close(view)
		}
	})
	return nil
}
