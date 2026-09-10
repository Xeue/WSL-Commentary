//go:build dev || production || bindings

// app_preview.go is the DeckLink PREVIEW's bound surface: the operator's own
// confidence monitor, a native child window painted over this page where the
// operator sees what this position is actually sending.
//
// Owner: WP-P. It is what remains in the application of the native-overlay
// mechanism (internal/gst/overlay_*.go) after the SRT picture moved into the
// PGM monitor process — the picture uses the same mechanism there, over the
// monitor's window; see monitor_picture.go. Everything about the preview is
// unchanged: it is a branch of the capture pipeline, built with it, never
// attached live, and spared every failure.
package main

import (
	"fmt"
	"log"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
)

// newPreviewOverlay builds the PREVIEW's surface, through the overlayDial seam
// so the tests can substitute a fake. Nil means the real one.
//
// windowTitle is main.go's, and it is passed rather than duplicated: it is how
// the overlay identifies which of this process's windows to be a child of, and
// two copies of that string is one more thing to get out of step. The PURPOSE
// argument names the surface in internal/gst's log lines, from the days when
// the SRT picture was a second overlay in this same window; it is kept because
// a field log that says which surface moved is still worth more than one that
// says "the overlay" did.
func (a *App) newPreviewOverlay() (gst.PictureOverlay, error) {
	if a.overlayDial != nil {
		return a.overlayDial()
	}
	return gst.NewOverlaySurface(windowTitle, gst.PreviewSurfacePurpose)
}

// ---------------------------------------------------------------------------
// The DeckLink preview: the operator's own confidence monitor
// ---------------------------------------------------------------------------
//
// A SECOND native surface, on screen at the same time as the picture: the
// programme return the commentator is watching above, and what THIS position is
// actually sending below. It exists because a green SENDING lamp and a healthy
// bitrate say nothing whatever about whether the camera is pointing at the
// pitch, and until this there was nothing anywhere in the application that
// could.
//
// It lives in this file because this is the file that owns the picture path,
// and it is now the ONLY native overlay in this process's window: the SRT
// picture has its own window in its own process. The preview has no monitor,
// no state machine and no lock of its own beyond prevViewMu, because it is not a
// thing that runs. It is a branch of the contribution pipeline.
//
// # THREE RULES, EACH OF WHICH IS A MEASUREMENT
//
//  1. IT IS BUILT AT START, FROM THE CONFIGURATION, AND NEVER ATTACHED LIVE.
//     set_state(NULL) inside a blocking pad probe took the on-air leg from 50
//     fps to 0 PERMANENTLY, with the pipeline still reporting PLAYING. There is
//     therefore no "show the preview" that reaches into a running graph, and the
//     bound setters below can only position and hide a surface that either
//     exists for this session or does not.
//
//  2. IT SHARES THE ONE CAPTURE THROUGH A tee, BECAUSE A SECOND
//     decklinkvideosrc IS IMPOSSIBLE. The card admits exactly one user: two in
//     one process fail 3 of 3, and in two processes fail 3 of 3. So the preview
//     is not a monitor that could be built beside the feed the way the SRT
//     picture is — it is downstream of the same element, and everything about
//     how it is throttled (12.5 fps, scale before convert, leaky=downstream on
//     the head queue) exists to keep it from costing the feed anything. Those
//     are internal/gst's business; what matters here is that the two are not
//     independent and must not be reasoned about as though they were.
//
//  3. ITS FAILURES ARE SPARED. A preview that cannot get a window does not fail
//     Start, does not stop the session and does not change one byte of what is
//     transmitted: it logs, it tells the operator once, and the feed goes out
//     without it. That is the same principle as internal/gst's bus filter
//     treating a VIDEO capture error as recoverable, applied one layer up. A
//     confidence monitor that could take the match off air would be worse than
//     no confidence monitor.

// SetPreviewRect tells the preview surface where it goes.
//
// x, y, w and h are CSS pixels relative to the top-left of the page's viewport,
// which is the same origin as the host window's client area. ratio is the page's
// own window.devicePixelRatio, measured at the same moment as the rectangle.
//
// # Both halves of that are required and the second is the one that gets forgotten
//
// A native child window is positioned in PHYSICAL pixels. The page lays out in
// CSS pixels. The factor between them is not a constant: the operator runs a
// 3840x2088 window, Windows display scaling can change while the application is
// running, dragging the window to a second monitor changes it with no setting
// changing at all, and Ctrl+scroll on a WebView2 changes the page's ratio without
// changing the monitor's DPI at all. Reading the DPI on the Go side instead would
// be a different number measured at a different moment; the page's own ratio is
// authoritative because the page's own layout is what the rectangle must line up
// with. See gst.ScaleRect.
//
// # It must be called on every layout change, and that is the frontend's job
//
// A native child does not participate in CSS layout. It is not moved by a
// flexbox, not clipped by an overflow rule, not scrolled, and not hidden by a
// modal. It sits where it was last told to sit. So the frontend must call this
// from a ResizeObserver on the preview element — which covers window resize,
// maximise, restore, DPI change and every layout change the page makes on its
// own — and must call SetPreviewVisible(false) before it draws anything over that
// rectangle.
//
// An empty or off-screen rectangle hides the window rather than failing. A page
// that has not laid out yet legitimately produces one, and refusing it would turn
// a normal moment during startup into an error toast.
//
// # IT NEVER CREATES THE SURFACE
//
// The surface exists only while a capture whose video leg is a camera is built,
// and a surface created outside one would be an opaque black rectangle over the
// page with nothing rendering into it. So this records the rectangle and applies
// it if there is something to apply it to; the surface is created by
// startCapturePreview and by nothing else.
//
// It is HOST-ONLY. A remote seat must not move or resize a window on somebody
// else's screen.
func (a *App) SetPreviewRect(x, y, w, h, ratio float64) error {
	rect := gst.ScaleRect(x, y, w, h, ratio)

	// prevViewMu ONLY, and it NEVER takes sessMu. The order everywhere in this
	// application is sessMu → prevViewMu — startSession holds sessMu while
	// building the surface — so a layout call that reached for sessMu would be
	// the reverse edge, and this one is called from the page's layout code on a
	// Wails message-handler goroutine, where waiting on a pipeline state change
	// is exactly what must not happen.
	a.prevViewMu.Lock()
	defer a.prevViewMu.Unlock()

	a.prevRect = rect
	if a.prevOverlay == nil {
		// No session, or a session sending the slate. The rectangle is kept, so
		// the next Start positions its surface before it is ever shown.
		return nil
	}
	if err := a.prevOverlay.SetRect(rect); err != nil {
		return err
	}
	// A rectangle can turn the surface from empty into non-empty, which is a
	// visibility change nobody asked for.
	a.applyPreviewVisibilityViewLocked()
	return nil
}

// SetPreviewVisible tells the preview surface whether the page wants it on
// screen. It is an explicit statement rather than something this side could
// infer, because the surface is OPAQUE and ALWAYS ON TOP OF ITS RECTANGLE:
// anything the page draws there — the Settings screen, the mixer drawer, a
// modal — is invisible underneath it, and there is no way for this side to know
// the page has put something there, so the page has to say.
//
// It is a request. The surface is shown only when the page has asked for it AND
// this session actually has a preview branch AND the rectangle has area — see
// applyPreviewVisibilityViewLocked, where the middle condition is what stops a
// black rectangle appearing over the controls of every seat that is sending a
// slate.
//
// It is HOST-ONLY: it shows or hides an opaque native window on the screen of
// whoever is sitting at this machine, over whatever they were looking at, and a
// seat in another building has no business doing that.
func (a *App) SetPreviewVisible(visible bool) error {
	a.prevViewMu.Lock()
	defer a.prevViewMu.Unlock()

	a.prevWantVisible = visible
	a.applyPreviewVisibilityViewLocked()
	return nil
}

// startCapturePreview creates the preview surface for the capture layer about to
// be built and returns the window handle its branch renders into, or 0 for no
// preview at all.
//
// It is called from rebuildCaptureLocked, BEFORE the pipeline exists, because the
// handle is a build-time option — rule 1 in this section's header.
// videoLegIsCamera is the pre-flight's verdict rather than the configuration's
// intention: a seat that asked for a preview but whose video leg resolved to the
// slate has nothing to preview, and it is the resolved answer that decides.
//
// # THE SURFACE'S LIFETIME IS THE CAPTURE'S, NOT A SESSION'S
//
// It used to be startSessionPreview, created at START and destroyed at STOP,
// because the branch it renders into was a branch of the contribution pipeline.
// The picture capture is now built at launch and held until the application
// quits, so a desk with the card selected and the preview ticked has its
// confidence picture BEFORE anything is sent and still has it after STOP —
// which is the period the operator actually uses it in, setting up.
//
// # Every failure here is spared and none of them reaches the caller
//
// It returns 0 rather than an error, and the capture build has no branch for it,
// which is rule 3 made structural instead of remembered. The operator is still
// told — an overlay that will not appear is a control they can see is missing —
// but through the error event, beside a capture that came up perfectly.
func (a *App) startCapturePreview(cfg *config.Config, videoLegIsCamera bool) uintptr {
	if !cfg.DeckLinkPreviewEnabled || !videoLegIsCamera {
		// Nothing to preview on this seat. Any surface left from the last build
		// goes back rather than sitting opaque over the page. Its Close error is
		// logged inside; here there is nothing to do with one.
		_ = a.stopCapturePreview()
		return 0
	}

	a.prevViewMu.Lock()
	defer a.prevViewMu.Unlock()

	overlay, err := a.previewOverlayViewLocked()
	if err != nil {
		log.Printf("wslcomms: the DeckLink preview could not get a window (%v); building the "+
			"capture without it — the meters, the routing and the feed are unaffected", err)
		a.emitError(fmt.Errorf(
			"wslcomms: the preview could not be shown, so the capture is running without it — "+
				"nothing about what is being transmitted has changed: %w", err))
		return 0
	}

	a.prevRunning = true
	a.applyPreviewVisibilityViewLocked()
	return overlay.Handle()
}

// stopCapturePreview takes the preview surface away when the capture that was
// rendering into it has gone: a device change, a Restart capture, a seat that no
// longer wants one, or the application quitting.
//
// Its callers run AFTER the capture pipeline has reached NULL, and that ordering
// is the one stopPictureForTeardown argues at length for both platforms: take the
// surface away first and the video sink is presenting into a handle that no
// longer names anything. It is not negotiable in either direction.
//
// The surface does NOT outlive the pipeline that renders into it: its churn is
// a device change, which is rare and deliberate, and keeping an idle opaque
// window over the page while there is nothing to draw in it would be all cost.
//
// It is idempotent, and it returns the surface's Close error rather than only
// logging it, because that error is the one thing in this process that can say
// an OS thread was ABANDONED — gst.ErrAbandonedThread, wrapped — and teardown
// has to be able to see it. Every caller but stopPictureForTeardown logs it.
func (a *App) stopCapturePreview() error {
	a.prevViewMu.Lock()
	defer a.prevViewMu.Unlock()

	a.prevRunning = false

	overlay := a.prevOverlay
	a.prevOverlay = nil
	if overlay == nil {
		return nil
	}

	// Hidden before it is closed, so that the last thing the operator sees is the
	// page rather than a frozen final frame while the surface is torn down. Both
	// calls record and post rather than blocking; see overlay_windows.go's and
	// overlay_darwin.go's headers.
	if err := overlay.SetVisible(false); err != nil {
		log.Printf("wslcomms: hiding the preview surface as the capture goes down: %v", err)
	}
	if err := overlay.Close(); err != nil {
		log.Printf("wslcomms: closing the preview surface as the capture goes down: %v", err)
		return err
	}
	return nil
}

// previewOverlayViewLocked creates the preview surface. a.prevViewMu must be
// held, and the caller must be startCapturePreview — nothing else may bring one
// into being, for the reason SetPreviewRect gives.
//
// The window is created hidden and positioned from whatever rectangle the page
// last gave, in that order, so it can never appear at 0,0 for a frame before
// moving. On the first session after a launch the page has usually already laid
// out and the rectangle is real; if it has not, the rectangle is empty, the
// surface stays hidden, and the next SetPreviewRect brings it on screen.
func (a *App) previewOverlayViewLocked() (gst.PictureOverlay, error) {
	if a.prevOverlay != nil {
		// A surface left from the previous capture build. rebuildCaptureLocked
		// tears the old capture down before it asks for this, so a live surface
		// here is one whose pipeline has already reached NULL — reusing it is the
		// answer that cannot leak a window, and creating a second one over it is
		// the answer that can.
		return a.prevOverlay, nil
	}
	if a.closing.Load() {
		return nil, errShuttingDown
	}

	overlay, err := a.newPreviewOverlay()
	if err != nil {
		return nil, err
	}
	a.prevOverlay = overlay

	if err := overlay.SetRect(a.prevRect); err != nil {
		// The surface exists and is hidden; it is recorded so stopCapturePreview
		// will close it rather than leaving a window nothing owns.
		return nil, err
	}
	return overlay, nil
}

// applyPreviewVisibilityViewLocked pushes the effective visibility to the
// preview surface. a.prevViewMu must be held.
//
// Effective visibility is the AND of three facts, and the middle one is the
// preview's own:
//
//	the page asked for it       or a preview covers the Settings screen
//	this CAPTURE has a preview  or a black rectangle covers the controls
//	the rectangle has area      or the sink resizes its surface to nothing
//
// The preview has no monitor state to ask, because it is not a thing that
// connects and reconnects: it is either in the capture pipeline that is running
// or there is no such branch. prevRunning is that fact, recorded when the
// capture is built.
//
// It never blocks: gst.PictureOverlay.SetVisible records and posts, on both
// platforms. See overlay_windows.go's and overlay_darwin.go's headers.
func (a *App) applyPreviewVisibilityViewLocked() {
	if a.prevOverlay == nil {
		return
	}

	visible := a.prevWantVisible && a.prevRunning && !a.prevRect.Empty()
	if err := a.prevOverlay.SetVisible(visible); err != nil {
		// Reported rather than swallowed: a surface that will not hide is a
		// window over the operator's controls, which they can see and cannot
		// explain.
		a.emitError(fmt.Errorf("wslcomms: the preview surface would not %s: %w",
			map[bool]string{true: "appear", false: "hide"}[visible], err))
	}
}

// stopPreviewForTeardown is the preview's step of the ordered shutdown, a belt:
// the surface normally goes with the capture layer, whose step ran two steps
// earlier — but a capture step that overran was ABANDONED rather than waited
// for, and this is the last chance to take a window off the operator's screen
// before the process ends. It is idempotent and costs one dispatch when there
// is nothing there, which is every ordinary quit.
//
// Its Close is the one call here that can report gst.ErrAbandonedThread: on
// Windows it bounds itself by overlayCloseBudget and gives up on its message
// thread rather than hanging, and the error it returns then is what turns this
// step from "finished with a complaint" into "abandoned", which ends the
// process by TerminateProcess. See teardownStep and exit_windows.go: a thread
// left inside user32!DestroyWindow, or inside gstd3d11's subclass procedure,
// is exactly what ExitProcess must not be allowed to run DLL_PROCESS_DETACH
// over. The error is returned as it is — never flattened by fmt — so
// errors.Is keeps working through it.
func (a *App) stopPreviewForTeardown() error {
	return a.stopCapturePreview()
}
