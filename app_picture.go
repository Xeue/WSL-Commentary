//go:build dev || production || bindings

// app_picture.go is the SRT PICTURE path's half of the bound surface: five
// methods, one event, the lazily created native overlay surface and the
// goroutine that forwards the monitor's states to the page.
//
// Owner: WP-P. It is behind the same build tags as app.go, for the same reason,
// and it does not require cgo: at Gate A it drives internal/gst's picture stub
// and the overlay simply fails to be created, which is a state the frontend
// already has to handle.
//
// It is written against gst.PictureOverlay and NOT against either
// implementation. There are two — a child HWND painted by d3d11videosink on
// Windows, an NSView painted by glimagesink on macOS — and nothing in this file
// knows which one it has. The one place the difference is unavoidable is the
// teardown ordering, and it is argued for both platforms at
// stopPictureForTeardown rather than left as Windows prose describing macOS
// behaviour.
//
// # What this is for, stated plainly because it has been got wrong once
//
// THE COMMENTATOR'S PICTURE COMES FROM SRT. THE AUDIO COMES FROM KINESIS.
//
// The picture used to be the KVS multiviewer mosaic: a 2240x1440 track,
// CSS-cropped to a 640x360 tile and scaled up, which is why it looked soft. This
// path replaces it with the M2L-X programme output itself — H.265, 1920x1080 at
// 50p, 15000 kbps — decoded in hardware and presented in a native surface over
// the page. The mosaic stays as the FALLBACK, because a soft picture beats no
// picture and a commentator must never be looking at black.
//
// The audio is not touched by any of this. It is the Kinesis/WebRTC peer
// connection, with its existing bus and channel selection, exactly as before.
//
// # What this file is careful about
//
//  1. THE PICTURE MUST NOT BE ABLE TO DISTURB THE CONTRIBUTION FEED OR THE
//     AUDIO. Its own locks (picMu, picViewMu, picStateMu — never sessMu and
//     never retMu), its own monitor, its own event, its own pipeline in
//     internal/gst. Nothing here takes a lock anything else holds, so a wedged
//     GPU cannot make START slow.
//
//     The three-way split within the path is set out on App.picViewMu and the
//     order is picMu → picViewMu → picStateMu. It is not fastidiousness: two of
//     the three arrangements deadlock, and the one that deadlocks most easily —
//     the state forwarder taking the same lock StopPicture holds across the join
//     of that very forwarder — is what this file did on the first draft.
//
//  2. NOTHING HERE BLOCKS A WAILS MESSAGE HANDLER ON THE THREAD THAT OWNS THE
//     OVERLAY. Every surface operation is a record-and-post: a PostMessage to
//     this application's own pump thread on Windows, a dispatch_async to
//     AppKit's main queue on macOS. See overlay_windows.go's and
//     overlay_darwin.go's headers for why that is load-bearing rather than
//     tidy — on both platforms the alternative is a bound method that can be
//     made to wait on the graphics stack.
//
//  3. THE OVERLAY IS OPAQUE AND ALWAYS ON TOP OF ITS RECTANGLE — a child HWND
//     above the WebView2 on Windows, an NSView above the WKWebView on macOS. It
//     is shown only when the frontend has asked for it AND there are pictures to
//     put in it. Both halves are needed: a black rectangle over the fallback mosaic is
//     worse than the mosaic, and a picture over an open Settings screen is worse
//     than either. The frontend says which it wants; this file refuses to show
//     an empty one.
//
// # The interface gaps this path is living with, reported rather than edited round
//
//   - THERE IS NO srtPicturePort IN internal/config. The picture dials
//     EffectiveSRTReturnPort(), which is 40501 by default — the correct port,
//     Output 1, src=pgm — but it is the field the SRT AUDIO return was given,
//     and the two now mean different things. internal/config is not this work
//     package's to change. WP-5b/config: the picture wants its own
//     srtPicturePort, srtPicturePassphrase and srtPicturePBKeyLen, and until it
//     has them a config that describes an encrypted audio return also describes
//     the picture.
//
//   - gst.PictureOpts HAS NO OnConnectError. The reason libsrt actually gave for
//     a refused handshake — BADSECRET against UNSECURE, which are the two ways
//     an operator gets encryption wrong — reaches internal/gst, is logged, and
//     is then discarded. The same gap is reported for the return path in
//     app_return.go. Until it is closed, pictureDiagnostic below can only say
//     what this process observed.
package main

import (
	"errors"
	"fmt"
	"sync"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
	"wslcomms/internal/secrets"
)

// EventPicture carries a gst.PictureState. It is what tells the page whether the
// high-resolution SRT picture is up, and therefore whether to draw the fallback
// mosaic underneath it.
//
// It is a separate event from EventReturn on purpose. They were the same thing
// for as long as SRT was an audio path, and merging them again would put the
// picture's health and the headphones' health behind one indicator, which is the
// confusion this whole work exists to undo.
const EventPicture = "picture"

// errPictureNotRunning is returned by StopPicture when no monitor is running. It
// is a sentinel so that teardown can tell "there was nothing to stop" from a real
// failure, exactly as errNotSending and errReturnNotRunning do.
var errPictureNotRunning = errors.New("wslcomms: the picture is not running")

// errPictureAlreadyRunning is returned by StartPicture when one is.
var errPictureAlreadyRunning = errors.New("wslcomms: the picture is already running")

// pictureDiagnoseAfter is how many consecutive failed connection attempts pass
// before the one diagnostic message is emitted.
//
// Three, matching returnDiagnoseAfter: the first two are indistinguishable from
// an M2L-X output the operator has not switched on yet, and three attempts is
// about 14 seconds of trying on gst.PictureBackoffLadder — long enough to mean
// something, short enough that the operator is still standing at the machine.
const pictureDiagnoseAfter = 3

// pictureSession is one running picture monitor and the goroutine forwarding its
// states to the frontend. It is the mirror of session and returnSession, and
// deliberately not the same type: sharing one would be the first step towards
// sharing a lock.
type pictureSession struct {
	mon gst.PictureMonitor
	wg  sync.WaitGroup
}

// ---------------------------------------------------------------------------
// The bound surface
// ---------------------------------------------------------------------------

// StartPicture opens the SRT picture: it dials the configured M2L-X programme
// output as a caller, decodes the H.265 on the GPU and presents it in the native
// overlay. Progress is reported on the "picture" event, not by this method, which
// returns as soon as the reconnect loop is running.
//
// A connection failure is NOT an error from StartPicture. The monitor retries
// indefinitely, and the commentator may well press the button before the operator
// has enabled the output. What StartPicture does return is a configuration that
// cannot work, or an overlay that could not be created, and it says which.
//
// It requires the overlay to exist, which means it requires the application
// window to exist. On the ordinary path the frontend has already called
// SetPictureRect from its layout code by the time anything calls this, so the
// overlay is there; if it is not, this creates it.
func (a *App) StartPicture() error {
	a.picMu.Lock()
	defer a.picMu.Unlock()

	if a.closing.Load() {
		// The window is going away. Building a pipeline now would open an SRT
		// socket and a graphics device — a D3D11 device, or an NSOpenGL context
		// and a GstGLNSView — that teardown has already walked past, and the
		// process would exit still holding them. Same reasoning as
		// startSession and StartReturn; see step 0 of the shutdown order in
		// app.go's header.
		return errShuttingDown
	}
	if a.pic != nil {
		return errPictureAlreadyRunning
	}
	if a.gstInitErr != nil {
		return a.gstInitErr
	}

	cfg := a.snapshotConfig()

	// The picture and the SRT AUDIO return dial THE SAME M2L-X OUTPUT, and an
	// M2L-X SRT listener accepts exactly ONE peer and never displaces the
	// incumbent. Two callers on port 40501 from this one process means one of
	// them sits in its backoff ladder for the whole match while the other works,
	// and which one wins is a race.
	//
	// So this refuses, and the message says the thing the operator actually needs
	// to do — which is the point of this entire work package. The audio comes
	// from Kinesis. returnSource must be "webrtc" for the picture to have the
	// output to itself.
	//
	// It is checked against the CONFIGURATION rather than against the running
	// return monitor, deliberately. Reading a.ret needs retMu, StopReturn holds
	// retMu across a blocking pipeline teardown, and taking it here would let a
	// slow headphone endpoint block a bound method on the picture path — which is
	// exactly the coupling the separate locks exist to prevent.
	if cfg.UsesSRTReturn() {
		return fmt.Errorf(
			"wslcomms: cannot start the picture: returnSource is %q, so the SRT audio return is "+
				"dialling the same M2L-X output (port %d) that the picture needs. That output accepts "+
				"one connection and never displaces it, so the two cannot both have it. Set "+
				"returnSource to %q on the Settings screen — the commentator's AUDIO comes from "+
				"Kinesis, and SRT carries the PICTURE",
			cfg.EffectiveReturnSource(), cfg.EffectiveSRTReturnPort(), config.ReturnSourceWebRTC)
	}

	passphrase, err := a.picturePassphrase(cfg)
	if err != nil {
		return err
	}

	// picMu is held; pictureOverlay takes picViewMu inside it. That is the
	// documented order — picMu, then picViewMu, then picStateMu — and it is the
	// only nesting anywhere in this file.
	overlay, err := a.pictureOverlay()
	if err != nil {
		return err
	}

	mon := a.newPictureMonitor()
	opts := a.pictureOpts(cfg, passphrase, overlay.Handle())

	if err := mon.Start(opts); err != nil {
		// gst.PictureMonitor.Start only fails on a configuration it cannot use,
		// and it leaves nothing running when it does — a monitor whose Start
		// failed has no pipeline and no goroutine, so unlike the sender there is
		// nothing here to stop and nothing to leak.
		return fmt.Errorf("wslcomms: starting the picture: %w", err)
	}

	sess := &pictureSession{mon: mon}
	// The diagnostic is built HERE, from the options actually handed to the
	// monitor, and captured by the forwarder below. Building it from a fresh
	// config snapshot later would describe whatever the operator had saved by
	// then rather than what the running pipeline is dialling.
	diag := pictureDiagnostic(opts)
	sess.wg.Add(1)
	// Started only after Start has succeeded, for the same reason the sender's
	// and the return's forwarders are: on failure the monitor's loop never
	// launches, its states channel is never closed, and a forwarder ranging over
	// it would be a goroutine leaked per failed StartPicture.
	go func() {
		defer sess.wg.Done()
		a.forwardPictureStates(mon.States(), diag)
	}()
	a.pic = sess

	return nil
}

// StopPicture closes the SRT picture and hides the overlay.
//
// It holds picMu for its whole duration, including the blocking wait inside
// gst.PictureMonitor.Stop and the join of the forwarding goroutine, so that a
// StartPicture racing it cannot open a second pipeline rendering into the same
// window. By the time it returns the pipeline is at NULL,
// gst.PictureStateStopped has been emitted and the forwarding goroutine has
// exited.
//
// It does NOT hold picViewMu across that join, and must not. The forwarder takes
// picViewMu on every transition to drive the overlay; holding it here would
// deadlock the first Stop that arrived while a transition was in flight. See the
// lock-order comment on App.picViewMu, which records that this was written the
// wrong way round first and hung.
//
// It does NOT destroy the overlay surface. The surface outlives the monitor: the
// monitor is rebuilt whenever the configuration changes, and destroying and
// recreating a native child underneath a running web view is a z-order fight
// with nothing to gain on either platform. Only teardown destroys it.
//
// It returns errPictureNotRunning when nothing was running, which is what lets
// teardown call it unconditionally.
func (a *App) StopPicture() error {
	a.picMu.Lock()
	defer a.picMu.Unlock()

	sess := a.pic
	if sess == nil {
		// Still hide the overlay. A StopPicture on a monitor that is already gone
		// is exactly what teardown does, and an overlay left visible over the page
		// with a frozen last frame in it is the worst thing this path can leave
		// behind.
		a.applyPictureVisibility()
		return errPictureNotRunning
	}
	a.pic = nil

	err := sess.mon.Stop()
	sess.wg.Wait()

	// After the join, so that the forwarder's last transition — STOPPED — has
	// already been through applyPictureVisibility. This is the belt to that
	// brace and it costs one PostMessage.
	a.applyPictureVisibility()

	if err != nil {
		return fmt.Errorf("wslcomms: stopping the picture: %w", err)
	}
	return nil
}

// GetPictureState returns the current state of the picture monitor, for a page
// that has just loaded and has not yet seen a "picture" event.
//
// It is a getter over cached state rather than a query of the pipeline: the
// monitor pushes every transition on the event, and a getter that interrogated
// GStreamer would take a lock held across a state change to tell the UI something
// it is already being told.
func (a *App) GetPictureState() (gst.PictureState, error) {
	a.picStateMu.Lock()
	defer a.picStateMu.Unlock()
	return a.lastPicture, nil
}

// SetPictureRect tells the overlay where the picture goes.
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
// from a ResizeObserver on the picture element — which covers window resize,
// maximise, restore, DPI change and every layout change the page makes on its
// own — and must call SetPictureVisible(false) before it draws anything over that
// rectangle.
//
// An empty or off-screen rectangle hides the window rather than failing. A page
// that has not laid out yet legitimately produces one, and refusing it would turn
// a normal moment during startup into an error toast.
//
// It creates the overlay if it does not exist yet, which makes the first layout
// call after the window appears the thing that brings the overlay into being. It
// returns nil, not an error, when the window does not exist yet: that is a normal
// condition during startup and the next layout call will succeed.
func (a *App) SetPictureRect(x, y, w, h, ratio float64) error {
	rect := gst.ScaleRect(x, y, w, h, ratio)

	// picViewMu ONLY. This is called from the page's layout code, on a Wails
	// message-handler goroutine, and it must never be able to wait on
	// gst.PictureMonitor.Stop — which is what taking picMu here would mean.
	a.picViewMu.Lock()
	defer a.picViewMu.Unlock()

	a.picRect = rect

	overlay, err := a.pictureOverlayViewLocked()
	if err != nil {
		if errors.Is(err, gst.ErrNoHostWindow) {
			// Called before Wails created the window. Normal, and the rectangle
			// has been recorded, so the overlay will be positioned the moment it
			// is created.
			return nil
		}
		return err
	}
	if err := overlay.SetRect(rect); err != nil {
		return err
	}
	// The rectangle can turn the overlay from empty into non-empty, which is a
	// visibility change even though nothing asked for one.
	a.applyPictureVisibilityViewLocked()
	return nil
}

// SetPictureVisible tells the overlay whether the page wants it on screen.
//
// It is the frontend's explicit statement, and it is explicit rather than
// inferred because the overlay is OPAQUE and ALWAYS ON TOP OF ITS RECTANGLE.
// Anything the page draws in that rectangle — the Settings screen, the mixer
// drawer, a modal, the fallback mosaic — is invisible underneath it. There is no
// way for this side to know that the page has put something there, so the page
// has to say.
//
// It is a request, not a command. The overlay is shown only when the page has
// asked for it AND the monitor is in SHOWING AND the rectangle has area. The
// second condition is what stops a black rectangle covering the fallback mosaic
// while SRT is reconnecting, which is the case the operator would notice first
// and mind most.
func (a *App) SetPictureVisible(visible bool) error {
	// picViewMu ONLY, for the reason SetPictureRect gives.
	a.picViewMu.Lock()
	defer a.picViewMu.Unlock()

	a.picWantVisible = visible
	a.applyPictureVisibilityViewLocked()
	return nil
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// newPictureMonitor builds the monitor, through the pictureDial seam so the
// tests can substitute a fake. Nil means the real one.
func (a *App) newPictureMonitor() gst.PictureMonitor {
	if a.pictureDial != nil {
		return a.pictureDial()
	}
	return gst.NewPictureMonitor()
}

// pictureOverlayViewLocked returns the overlay window, creating it on first use.
// a.picViewMu must be held.
//
// # Why it is lazy rather than created at startup
//
// The overlay is a CHILD of the application window, so it cannot exist before the
// application window does. Wails creates that window inside wails.Run, after
// OnStartup and before OnDomReady, and there is no callback that is guaranteed to
// run with the window present and the page not yet laid out.
//
// Creating it on first use sidesteps the ordering entirely: the first thing the
// frontend does with the picture is tell it where to go, and by then the window
// certainly exists. A failure to find the window is returned as
// gst.ErrNoHostWindow and treated by SetPictureRect as "not yet", so an early
// call costs one failed EnumWindows and nothing else.
//
// The window is created hidden. Nothing appears on the operator's screen until
// the frontend asks for it and there are pictures to put in it.
func (a *App) pictureOverlayViewLocked() (gst.PictureOverlay, error) {
	if a.picOverlay != nil {
		return a.picOverlay, nil
	}
	if a.closing.Load() {
		return nil, errShuttingDown
	}

	overlay, err := a.newPictureOverlay()
	if err != nil {
		return nil, err
	}
	a.picOverlay = overlay

	// Apply whatever the frontend has already told us, in the order a fresh
	// window needs it: position first, then visibility, so it can never appear
	// at 0,0 for a frame before moving.
	if err := overlay.SetRect(a.picRect); err != nil {
		return nil, err
	}
	a.applyPictureVisibilityViewLocked()
	return overlay, nil
}

// newPictureOverlay builds the overlay window, through the overlayDial seam so
// the tests can substitute a fake. Nil means the real one.
//
// windowTitle is main.go's, and it is passed rather than duplicated: it is how
// the overlay identifies which of this process's windows to be a child of, and
// two copies of that string is one more thing to get out of step.
func (a *App) newPictureOverlay() (gst.PictureOverlay, error) {
	if a.overlayDial != nil {
		return a.overlayDial()
	}
	return gst.NewPictureOverlay(windowTitle)
}

// applyPictureVisibilityViewLocked pushes the effective visibility to the
// overlay. a.picViewMu must be held.
//
// Effective visibility is the AND of three facts, and each one is a way the
// picture goes wrong on its own:
//
//	the page asked for it       or a picture covers the Settings screen
//	the monitor is SHOWING      or a black rectangle covers the fallback mosaic
//	the rectangle has area      or the sink resizes its surface to nothing
//
// It never blocks: gst.PictureOverlay.SetVisible records and posts, on both
// platforms. See overlay_windows.go's and overlay_darwin.go's headers.
func (a *App) applyPictureVisibilityViewLocked() {
	if a.picOverlay == nil {
		return
	}

	a.picStateMu.Lock()
	showing := a.lastPicture == gst.PictureStateShowing
	a.picStateMu.Unlock()

	visible := a.picWantVisible && showing && !a.picRect.Empty()
	if err := a.picOverlay.SetVisible(visible); err != nil {
		// Logged through the error event rather than swallowed: an overlay that
		// will not hide is a picture over the operator's controls, which they can
		// see but cannot explain.
		a.emitError(fmt.Errorf("wslcomms: the picture overlay would not %s: %w",
			map[bool]string{true: "appear", false: "hide"}[visible], err))
	}
}

// applyPictureVisibility is applyPictureVisibilityViewLocked for callers that do
// not already hold picViewMu — in practice the state-forwarding goroutine.
//
// IT TAKES picViewMu AND NEVER picMu, AND THAT IS THE WHOLE REASON THE TWO LOCKS
// ARE SEPARATE. StopPicture holds picMu across gst.PictureMonitor.Stop and across
// the join of the forwarding goroutine; this function runs ON that goroutine, on
// every transition, because every transition changes whether the overlay should
// be on screen. If it took picMu, the first Stop that arrived while a transition
// was in flight would deadlock — the Stop holding picMu waiting for the join, the
// forwarder waiting for picMu. That is not hypothetical: this file was written
// that way first and it hung.
//
// The equivalent split on the other two paths is senderMu against sessMu and
// retStateMu against retMu, and it is made for exactly this reason.
func (a *App) applyPictureVisibility() {
	a.picViewMu.Lock()
	defer a.picViewMu.Unlock()
	a.applyPictureVisibilityViewLocked()
}

// pictureOverlay returns the overlay, creating it on first use, for a caller that
// holds picMu but not picViewMu — in practice StartPicture. It takes picViewMu,
// which is the documented inner lock.
func (a *App) pictureOverlay() (gst.PictureOverlay, error) {
	a.picViewMu.Lock()
	defer a.picViewMu.Unlock()
	return a.pictureOverlayViewLocked()
}

// pictureOpts builds the monitor's options from a configuration snapshot, the
// passphrase already read from the credential store, and the overlay handle.
//
// It is separate from StartPicture so that what the monitor is actually given can
// be asserted without running one.
//
// passphrase is a secret. It goes into gst.PictureOpts, which internal/gst sets
// with g_object_set rather than in the URI, and must not be logged or returned
// across the Wails boundary.
func (a *App) pictureOpts(cfg *config.Config, passphrase string, handle uintptr) gst.PictureOpts {
	return gst.PictureOpts{
		// EffectiveSRTHost, not SRTHost, and not a field of its own. The picture
		// follows the M2L-X host exactly as the send path and the return do: on
		// every instance seen so far the SRT listener answers on the same name as
		// the REST API, and a third host field would be a third thing to get
		// wrong under pressure for no case anyone has met.
		Host: cfg.EffectiveSRTHost(),

		// EffectiveSRTReturnPort is 40501: Output 1, src=pgm, the programme
		// picture. It is the AUDIO return's config field being read for the
		// picture, which is an interface gap and is reported as one in the file
		// header rather than worked around here.
		Port: cfg.EffectiveSRTReturnPort(),

		// EffectivePictureLatencyMs, and NOT SRTLatencyMs, which is what this
		// read until the operator reported the picture running about a second
		// behind the main feed.
		//
		// SRTLatencyMs is the CONTRIBUTION FEED's retransmission budget: the
		// delay the match tolerates on its way out so it does not break up on
		// air. Handing it to the monitor made one number answer two questions
		// that pull in opposite directions, and it meant the only way to make the
		// commentator's picture quicker was to thin the protection on the feed
		// going to air. They are separate fields now, and this is the monitor's.
		LatencyMs: cfg.EffectivePictureLatencyMs(),

		Passphrase: passphrase,
		PBKeyLen:   cfg.SRTReturnPBKeyLen,

		WindowHandle: handle,
	}
}

// picturePassphrase reads the SRT passphrase for the programme output from the
// operating system's credential store and checks it against the configured key
// length.
//
// WHICH store is internal/secrets' business and not this file's: Windows
// Credential Manager on one platform, the login keychain on the other, behind
// one Store interface. The message below therefore names both rather than
// naming the wrong one to whichever operator is reading it.
//
// It reads secrets.KeySRTReturn — the same entry the audio return used — because
// it is the same M2L-X OUTPUT, and encryption on M2L-X is set per output. It is
// NOT secrets.KeySRT, which is the contribution INPUT's key and is routinely a
// different value.
//
// On the measured instance Output 1 (pgm, 40501) has encrypted=False, so the
// ordinary case here is no passphrase and a zero key length. Outputs 2 and 3 are
// encrypted, so an operator who moves the picture to one of those will need both.
//
// It refuses exactly one combination: a non-zero key length with no stored
// passphrase. That asks libsrt for an encrypted session with no key, which cannot
// succeed against anything, and it is the precise shape of the fault that cost
// the operator an afternoon on the return path — dialling an encrypted output
// with nothing to encrypt with, retrying in silence for ever.
//
// The returned value is a secret and must never reach a log line, an error string
// or the Wails boundary.
func (a *App) picturePassphrase(cfg *config.Config) (string, error) {
	passphrase, err := a.store.Get(secrets.KeySRTReturn)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		if cfg.SRTReturnPBKeyLen != 0 {
			return "", fmt.Errorf(
				"wslcomms: cannot start the picture: srtReturnPBKeyLen is %d, which asks for an "+
					"encrypted session with the M2L-X output on port %d, but no passphrase is stored "+
					"in %s under %q — enter it on the Settings screen, or set the key "+
					"length to 0 if that output is not encrypted",
				cfg.SRTReturnPBKeyLen, cfg.EffectiveSRTReturnPort(), secrets.StoreName(), secrets.TargetSRTReturn)
		}
		return "", nil
	case err != nil:
		return "", fmt.Errorf("wslcomms: reading the picture's SRT passphrase from %q: %w",
			secrets.TargetSRTReturn, err)
	}
	return passphrase, nil
}

// pictureDiagnostic renders the one message emitted after pictureDiagnoseAfter
// consecutive failed attempts: what is being dialled, with what encryption, and
// which way the encryption could be wrong.
//
// It is app_return.go's returnDiagnostic, restated for this path rather than
// shared, because the two say different things about what to do next and because
// sharing would mean one message that has to be right about both. Read that
// function's comment for the full argument about why it says only what this
// process observed and invents no cause.
//
// opts carries the passphrase and is taken BY VALUE and read for one boolean.
// Nothing in the returned string is derived from its contents.
//
// The key length is restated rather than read straight out of opts because opts
// has NOT been through gst.PictureOpts.normalise: Start takes its argument by
// value, so the defaulting happens on the monitor's own copy and never comes
// back. normalise turns a passphrase with a zero key length into AES-128, and a
// message reporting "AES-0" would send the operator looking for a setting that is
// not wrong.
func pictureDiagnostic(opts gst.PictureOpts) string {
	endpoint := fmt.Sprintf("srt://%s:%d", opts.Host, opts.Port)

	keylen := opts.PBKeyLen
	if opts.Passphrase != "" && keylen == 0 {
		keylen = 16 // gst.PictureOpts.normalise's default; see above
	}

	if opts.Passphrase == "" {
		return fmt.Sprintf(
			"the picture has failed to connect %d times in a row. It is dialling %s with NO "+
				"encryption. If that M2L-X output has a passphrase set, every handshake will keep "+
				"being refused. The commentator is seeing the fallback mosaic, which is the soft "+
				"picture, not the high-resolution one. The exact reason libsrt gave is in the "+
				"application log",
			pictureDiagnoseAfter, endpoint)
	}
	return fmt.Sprintf(
		"the picture has failed to connect %d times in a row. It is dialling %s with AES-%d "+
			"encryption. If that passphrase is wrong, or if that M2L-X output is not encrypted at "+
			"all, every handshake will keep being refused. The commentator is seeing the fallback "+
			"mosaic, which is the soft picture, not the high-resolution one. The exact reason "+
			"libsrt gave is in the application log",
		pictureDiagnoseAfter, endpoint, keylen*8)
}

// forwardPictureStates pumps the monitor's state transitions to the frontend,
// records the latest for a page reload, and drives the overlay's visibility.
//
// It is the mirror of forwardReturnStates and goes through the same event pump,
// so a renderer that has stopped reading loses the oldest events rather than
// stalling the reconnect loop.
//
// It takes picStateMu and picMu, in that order and never nested — see
// applyPictureVisibility for why the two locks are separate and what would
// deadlock if they were one.
//
// # The failure count
//
// A transition to BACKOFF is one failed attempt, because the monitor's loop only
// reaches BACKOFF from a failure — every other exit from an attempt is Stop, which
// ends the loop instead. SHOWING resets the count, so a mid-match drop that
// recovers never speaks, and the count is per-session state on this goroutine's
// stack rather than on the App: it belongs to one monitor, and an operator who
// stops and starts the picture has said they want to be told again.
func (a *App) forwardPictureStates(states <-chan gst.PictureState, diag string) {
	failures := 0
	said := false

	for s := range states {
		switch s {
		case gst.PictureStateBackoff:
			failures++
		case gst.PictureStateShowing:
			// It connected. Whatever was wrong is not wrong now, and a later
			// outage starts its own count.
			failures = 0
			said = false
		}

		a.picStateMu.Lock()
		a.lastPicture = s
		a.picStateMu.Unlock()

		// The overlay follows the state BEFORE the event goes out, so that by the
		// time the page is told the picture has stopped, the window over the
		// fallback mosaic has already gone. The other order shows the page a
		// state it cannot act on for a frame.
		a.applyPictureVisibility()

		a.events.send(EventPicture, s)

		if !said && failures >= pictureDiagnoseAfter {
			said = true
			// Emitted AFTER the state, so the page is already showing the
			// fallback when the explanation for it arrives.
			a.emitError(errors.New("wslcomms: " + diag))
		}
	}
}

// stopPictureForTeardown is the picture's step of the ordered shutdown: stop the
// monitor, then take the overlay surface away.
//
// # The order is fixed, and each platform has a different reason for it
//
// It is the same order on both, which is why one function can do it, but a
// reader who knows only one of the reasons will eventually decide the order is
// arbitrary and swap it. Both are written out here.
//
// THE SHARED REASON. The monitor's pipeline is rendering into the surface. Take
// the surface away first and the video sink is presenting into a handle that no
// longer names anything.
//
// ON WINDOWS that means gstd3d11 presenting to a destroyed HWND. gstd3d11
// SUBCLASSES the window it is given — SetWindowLongPtr(GWLP_WNDPROC) — and
// restores the previous procedure on teardown, so a DestroyWindow that races its
// subclass restore runs a window procedure that has been unloaded. The
// consequence is driver-dependent, it happens under the loader lock during
// process exit, and it is not something to find out about during a match. See
// exit_windows.go, which exists for that whole family of hazard.
//
// ON macOS THE HAZARD IS DIFFERENT AND THERE ARE TWO OF THEM, so the Windows
// prose above must not be read as describing this platform:
//
//  1. THE SURFACE IS AN NSView THE SINK HAS PUT ITS OWN VIEW INSIDE. glimagesink
//     adds a GstGLNSView as a subview of the overlay view and holds a reference
//     to the NSOpenGL context bound to it. Releasing the overlay view while the
//     sink is still rendering pulls the superview out from under a live GL
//     surface on a thread that is not the main one. Stopping the monitor first
//     makes the sink remove and release its own view before ours goes anywhere,
//     which is the only ordering in which nothing is holding anything.
//
//  2. THE MAIN RUN LOOP IS ALREADY DEAD BY THE TIME EITHER HALF RUNS. Wails
//     calls OnShutdown AFTER -[NSApplication run] has RETURNED, and App.teardown
//     then runs its steps on a goroutine while the main thread is parked waiting
//     for them. Nothing services the main queue from that moment on. So there is
//     NO main-thread work available to either half of this function, and both
//     are written not to need any: gst.PictureOverlay.Close on macOS posts the
//     removal and returns immediately rather than waiting for it. A Close that
//     waited — or worse, dispatch_sync'd — would hang the whole shutdown here,
//     every single time, on an ordinary quit with nothing wrong anywhere. That
//     is the macOS shape of the trap WS_EX_NOPARENTNOTIFY defuses on Windows,
//     and it is defused by the same principle: never block on the host
//     framework's thread during teardown.
//
// # Bounding
//
// The two halves are bounded differently, and only one of them is bounded by
// anything of its own.
//
// gst.PictureOverlay.Close bounds itself: overlayCloseBudget on Windows, where
// there is a message-pump thread of ours to join, and unconditionally prompt on
// macOS, where there is no thread of ours at all. It is therefore the only half
// that can report gst.ErrAbandonedThread, and only on Windows — which is correct
// rather than an asymmetry to be tidied away: on macOS there is nothing to
// abandon, and wrapping the sentinel would end every ordinary quit with a hard
// exit.
//
// gst.PictureMonitor.Stop is NOT, on either platform, and the timeout in
// picture_cgo.go is not the bound it looks like. That call is
// BlockSetState(StateNull, elementShutdownTimeout) at picture_cgo.go:1253, and
// go-gst v0.0.2's BlockSetState (pkg/gst/element_manual.go:37-45) only reaches
// the timeout at all when SetState returns ASYNC — it calls GetState(timeout)
// inside `if ret == StateChangeAsync`, and returns SetState's own answer
// otherwise. A DOWNWARD transition to NULL never returns ASYNC: GStreamer
// completes it synchronously before returning. So the timeout is dead code on
// this path, and what Stop actually costs is however long
// gst_element_set_state takes, which takes no timeout and cannot be
// interrupted. A wedged audio endpoint or a stuck decoder hangs it for ever.
//
// The only real bound on that half is therefore the one imposed from OUTSIDE:
// this whole function runs inside app.go's teardownStep under
// pictureStopBudget, which ABANDONS it if it overruns and force-exits the
// process. That is not a fallback for the timeout; it is the mechanism. See the
// budget block above App.teardown, which says the same thing generally: none of
// those figures bounds the synchronous half of a state change, which is exactly
// why the abandonment exists and why abandoning anything ends the process rather
// than returning into an exit path that would have to step over a wedged thread.
func (a *App) stopPictureForTeardown() error {
	var problems []error

	if err := a.StopPicture(); err != nil && !errors.Is(err, errPictureNotRunning) {
		problems = append(problems, err)
	}

	// picViewMu, which is where the overlay lives. StopPicture has already
	// released picMu by now, so this takes nothing while holding anything.
	a.picViewMu.Lock()
	overlay := a.picOverlay
	a.picOverlay = nil
	a.picViewMu.Unlock()

	if overlay != nil {
		if err := overlay.Close(); err != nil {
			problems = append(problems, err)
		}
	}
	// errors.Join and NOT a formatted summary, because what teardownStep does
	// with this error depends on being able to see through it: an overlay Close
	// that gave up wraps gst.ErrAbandonedThread, the joined error's Unwrap
	// []error keeps errors.Is working through it, and that is what turns this
	// step from "finished with a complaint" into "abandoned" and ends the
	// process by TerminateProcess. A fmt.Errorf with %v here would flatten the
	// sentinel to text and put the shutdown hang back. See teardownStep.
	return errors.Join(problems...)
}
