//go:build dev || production || bindings

// monitor_picture.go is the SRT PICTURE inside the PGM monitor process: six
// bound methods, one event, the lazily created native overlay surface and the
// goroutine that forwards the pipeline's states to the page.
//
// Owner: WP-P. It is the design the application used for the picture before
// the picture moved into the monitor process: the pipeline runs in THIS
// process, decodes on the GPU and renders into a native child window painted
// over the page — over the mosaic tile, exactly the rectangle the page reserves
// for it. What changed is the process. A decoder or a driver that wedges here
// wedges the monitor, and the application kills and relaunches the monitor;
// the contribution feed never notices.
//
// # What this file is careful about
//
//  1. NOTHING HERE BLOCKS A WAILS MESSAGE HANDLER ON THE THREAD THAT OWNS THE
//     OVERLAY. Every surface operation is a record-and-post; see
//     overlay_windows.go's and overlay_darwin.go's headers.
//
//  2. THE OVERLAY IS OPAQUE AND ALWAYS ON TOP OF ITS RECTANGLE. It is shown
//     only when the page has asked for it AND the pipeline is SHOWING AND the
//     rectangle has area. A black rectangle over the fallback mosaic is worse
//     than the mosaic, and a picture over a control is worse than either.
//
//  3. THE LOCK ORDER IS picMu → picViewMu → picStateMu, and picMu is the only
//     one held across something that blocks. The state forwarder takes
//     picViewMu on every transition to drive the overlay; StopPicture holds
//     picMu across the join of that forwarder; the two must never be one lock.
//
// # Where the options come from
//
// The host, port, latency, key length and passphrase are the application's:
// it holds the configuration and the credential store. StartPicture asks it
// for them over the link — PictureOpts, a method that exists on the link and
// nowhere else, so the passphrase never touches the LAN bridge — and the
// application applies its own refusals first (the SRT audio return holding
// the same output; a key length with no passphrase).
package main

import (
	"errors"
	"fmt"
	"log"
	"sync"

	"wslcomms/internal/gst"
)

// EventPicture carries a gst.PictureState to the monitor's page: whether the
// high-resolution SRT picture is up, and therefore whether the mosaic
// underneath it is what the commentator is looking at.
const EventPicture = "picture"

// errPictureNotRunning is returned by StopPicture when nothing is running, so
// teardown can tell "nothing to stop" from a failure.
var errPictureNotRunning = errors.New("wslcomms: the picture is not running")

// errPictureAlreadyRunning is returned by StartPicture when one is.
var errPictureAlreadyRunning = errors.New("wslcomms: the picture is already running")

// pictureDiagnoseAfter is how many consecutive failed connection attempts pass
// before the one diagnostic message is emitted: about 14 seconds of trying on
// gst.PictureBackoffLadder, long enough to mean something.
const pictureDiagnoseAfter = 3

// pictureSession is one running picture monitor and the goroutine forwarding
// its states to the page.
type pictureSession struct {
	mon gst.PictureMonitor
	wg  sync.WaitGroup
}

// StartPicture opens the SRT picture: it asks the application for the
// options, dials the M2L-X programme output, decodes the H.265 on the GPU and
// presents it in the overlay. Progress is reported on the "picture" event; this
// returns as soon as the reconnect loop is running.
//
// A connection failure is NOT an error from here — the monitor retries for
// ever. What is returned is a configuration that cannot work, or an overlay
// that could not be created, and it says which.
func (m *MonitorApp) StartPicture() error {
	m.picMu.Lock()
	defer m.picMu.Unlock()

	if m.closing.Load() {
		return errShuttingDown
	}
	if m.pic != nil {
		return errPictureAlreadyRunning
	}
	if m.gstInitErr != nil {
		return m.gstInitErr
	}

	base, err := m.fetchPictureOpts()
	if err != nil {
		return err
	}

	// picMu is held; pictureOverlay takes picViewMu inside it — the documented
	// order and the only nesting in this file.
	overlay, err := m.pictureOverlay()
	if err != nil {
		return err
	}

	mon := m.newPictureMonitor()
	opts := base
	opts.WindowHandle = overlay.Handle()

	if err := mon.Start(opts); err != nil {
		// Start fails only on options it cannot use and leaves nothing running.
		return fmt.Errorf("wslcomms: starting the picture: %w", err)
	}

	m.maybeNotePictureSoftwareDecode()

	sess := &pictureSession{mon: mon}
	// Built from the options actually handed to the pipeline, not from a
	// later snapshot that may describe something else.
	diag := pictureDiagnostic(opts)
	sess.wg.Add(1)
	go func() {
		defer sess.wg.Done()
		m.forwardPictureStates(mon.States(), diag)
	}()
	m.pic = sess
	return nil
}

// RefreshPicture restarts the SRT picture from nothing: stop, then start. It
// is the SRT half of the monitor window's Refresh button. A picture that was
// not running is simply started; a stop that failed is logged and the start
// still attempted.
//
// A pipeline wedged so badly that Stop never returns wedges this method with
// it. That is the case "Restart monitor" in the application exists for: it
// kills this process and starts a fresh one.
func (m *MonitorApp) RefreshPicture() error {
	if err := m.StopPicture(); err != nil && !errors.Is(err, errPictureNotRunning) {
		log.Printf("wslcomms: monitor: refreshing the picture: the old one did not stop cleanly: %v", err)
	}
	return m.StartPicture()
}

// maybeNotePictureSoftwareDecode tells the operator once per process that the
// picture is decoding in software, when it is.
func (m *MonitorApp) maybeNotePictureSoftwareDecode() {
	software, factory := gst.PictureDecoderIsSoftware()
	if !software {
		return
	}
	m.pictureSoftwareNoteOnce.Do(func() {
		m.emitNote(fmt.Sprintf(
			"This machine has no hardware HEVC decoder, so the picture is being decoded in "+
				"software (%s). That uses noticeably more CPU and can judder at 1080p50 on a "+
				"low-powered machine. To get hardware decoding back, update the graphics driver; "+
				"or set that M2L-X output to H.264, which this machine can decode in hardware.",
			factory))
	})
}

// StopPicture closes the SRT picture and hides the overlay. It holds picMu for
// its whole duration, including the pipeline's blocking stop and the join of
// the forwarder, so a StartPicture racing it cannot open a second pipeline into
// the same window. It does NOT destroy the overlay: that outlives the pipeline
// and only teardown removes it.
func (m *MonitorApp) StopPicture() error {
	m.picMu.Lock()
	defer m.picMu.Unlock()

	sess := m.pic
	if sess == nil {
		// Still hide the overlay: a surface left visible with a frozen last
		// frame in it is the worst thing this path can leave behind.
		m.applyPictureVisibility()
		return errPictureNotRunning
	}
	m.pic = nil

	err := sess.mon.Stop()
	sess.wg.Wait()

	// After the join, so the forwarder's last transition — STOPPED — has
	// already been through applyPictureVisibility; this is the belt.
	m.applyPictureVisibility()

	if err != nil {
		return fmt.Errorf("wslcomms: stopping the picture: %w", err)
	}
	return nil
}

// GetPictureState returns the last state forwarded, for a page that has just
// loaded and not yet seen a "picture" event.
func (m *MonitorApp) GetPictureState() (gst.PictureState, error) {
	m.picStateMu.Lock()
	defer m.picStateMu.Unlock()
	return m.lastPicture, nil
}

// SetPictureRect tells the overlay where the picture goes: x, y, w and h in
// CSS pixels relative to the page's viewport, and ratio the page's own
// window.devicePixelRatio measured at the same moment. Go multiplies (see
// gst.ScaleRect), because the ratio Go could read for itself is a different
// number measured at a different moment. The frontend calls this from a
// ResizeObserver on the tile; an empty rectangle hides rather than fails. It
// creates the overlay on first use and answers nil, not an error, before the
// window exists.
func (m *MonitorApp) SetPictureRect(x, y, w, h, ratio float64) error {
	rect := gst.ScaleRect(x, y, w, h, ratio)

	// picViewMu ONLY: this runs on a Wails message-handler goroutine and must
	// never wait on the pipeline's Stop, which is what taking picMu would mean.
	m.picViewMu.Lock()
	defer m.picViewMu.Unlock()

	m.picRect = rect

	overlay, err := m.pictureOverlayViewLocked()
	if err != nil {
		if errors.Is(err, gst.ErrNoHostWindow) {
			return nil // before the window exists: the rectangle is remembered
		}
		return err
	}
	if err := overlay.SetRect(rect); err != nil {
		return err
	}
	m.applyPictureVisibilityViewLocked()
	return nil
}

// SetPictureVisible is the page's explicit statement of whether it wants the
// overlay on screen. It is a request: the overlay is shown only when the page
// asked AND the pipeline is SHOWING AND the rectangle has area.
func (m *MonitorApp) SetPictureVisible(visible bool) error {
	m.picViewMu.Lock()
	defer m.picViewMu.Unlock()
	m.picWantVisible = visible
	m.applyPictureVisibilityViewLocked()
	return nil
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// fetchPictureOpts asks the application for the picture's options, through the
// pictureOptsDial seam in tests.
func (m *MonitorApp) fetchPictureOpts() (gst.PictureOpts, error) {
	if m.pictureOptsDial != nil {
		return m.pictureOptsDial()
	}
	var p pictureOptsPayload
	if err := m.call("PictureOpts", &p); err != nil {
		return gst.PictureOpts{}, fmt.Errorf("wslcomms: cannot start the picture: %w", err)
	}
	return gst.PictureOpts{
		Host:       p.Host,
		Port:       p.Port,
		LatencyMs:  p.LatencyMs,
		Passphrase: p.Passphrase,
		PBKeyLen:   p.PBKeyLen,
	}, nil
}

// newPictureMonitor builds the pipeline's monitor, through the pictureDial
// seam in tests.
func (m *MonitorApp) newPictureMonitor() gst.PictureMonitor {
	if m.pictureDial != nil {
		return m.pictureDial()
	}
	return gst.NewPictureMonitor()
}

// pictureOverlayViewLocked returns the overlay, creating it on first use.
// m.picViewMu must be held. It is lazy because the overlay is a child of the
// window and the window does not exist until Wails has made it; a failure to
// find the window is gst.ErrNoHostWindow, which SetPictureRect treats as "not
// yet". The window is created hidden.
func (m *MonitorApp) pictureOverlayViewLocked() (gst.PictureOverlay, error) {
	if m.picOverlay != nil {
		return m.picOverlay, nil
	}
	if m.closing.Load() {
		return nil, errShuttingDown
	}
	overlay, err := m.newPictureOverlay()
	if err != nil {
		return nil, err
	}
	m.picOverlay = overlay
	// Position first, then visibility, so it can never appear at 0,0 for a
	// frame before moving.
	if err := overlay.SetRect(m.picRect); err != nil {
		return nil, err
	}
	m.applyPictureVisibilityViewLocked()
	return overlay, nil
}

// newPictureOverlay builds the overlay, through the overlayDial seam in tests.
// It is a child of THIS process's window, found by its title.
func (m *MonitorApp) newPictureOverlay() (gst.PictureOverlay, error) {
	if m.overlayDial != nil {
		return m.overlayDial()
	}
	return gst.NewPictureOverlay(monitorWindowTitle)
}

// applyPictureVisibilityViewLocked pushes the effective visibility — the page
// asked AND the pipeline is SHOWING AND the rectangle has area — to the
// overlay. m.picViewMu must be held. It never blocks.
func (m *MonitorApp) applyPictureVisibilityViewLocked() {
	if m.picOverlay == nil {
		return
	}
	m.picStateMu.Lock()
	showing := m.lastPicture == gst.PictureStateShowing
	m.picStateMu.Unlock()

	visible := m.picWantVisible && showing && !m.picRect.Empty()
	if err := m.picOverlay.SetVisible(visible); err != nil {
		m.emitError(fmt.Errorf("wslcomms: the picture overlay would not %s: %w",
			map[bool]string{true: "appear", false: "hide"}[visible], err))
	}
}

// applyPictureVisibility is applyPictureVisibilityViewLocked for a caller that
// does not hold picViewMu — the state forwarder. IT TAKES picViewMu AND NEVER
// picMu: StopPicture holds picMu across the join of the forwarder, and a
// forwarder waiting for picMu would deadlock the first Stop that arrived
// mid-transition.
func (m *MonitorApp) applyPictureVisibility() {
	m.picViewMu.Lock()
	defer m.picViewMu.Unlock()
	m.applyPictureVisibilityViewLocked()
}

// pictureOverlay returns the overlay, creating it on first use, for a caller
// holding picMu but not picViewMu — StartPicture.
func (m *MonitorApp) pictureOverlay() (gst.PictureOverlay, error) {
	m.picViewMu.Lock()
	defer m.picViewMu.Unlock()
	return m.pictureOverlayViewLocked()
}

// pictureDiagnostic renders the one message emitted after pictureDiagnoseAfter
// consecutive failed attempts. opts is read for one boolean — whether there is
// a passphrase — and nothing in the result is derived from its contents. The
// key length is restated because opts has not been through normalise, which
// turns a passphrase with a zero length into AES-128.
func pictureDiagnostic(opts gst.PictureOpts) string {
	endpoint := fmt.Sprintf("srt://%s:%d", opts.Host, opts.Port)
	keylen := opts.PBKeyLen
	if opts.Passphrase != "" && keylen == 0 {
		keylen = 16
	}
	if opts.Passphrase == "" {
		return fmt.Sprintf(
			"the picture has failed to connect %d times in a row. It is dialling %s with NO "+
				"encryption. If that M2L-X output has a passphrase set, every handshake will keep "+
				"being refused. The commentator is seeing the fallback mosaic, which is the soft "+
				"picture, not the high-resolution one. The exact reason libsrt gave is in the "+
				"monitor's log",
			pictureDiagnoseAfter, endpoint)
	}
	return fmt.Sprintf(
		"the picture has failed to connect %d times in a row. It is dialling %s with AES-%d "+
			"encryption. If that passphrase is wrong, or if that M2L-X output is not encrypted at "+
			"all, every handshake will keep being refused. The commentator is seeing the fallback "+
			"mosaic, which is the soft picture, not the high-resolution one. The exact reason "+
			"libsrt gave is in the monitor's log",
		pictureDiagnoseAfter, endpoint, keylen*8)
}

// forwardPictureStates pumps the pipeline's transitions to the page, records
// the latest for a reload, and drives the overlay's visibility. It takes
// picStateMu and picViewMu, never picMu. A transition to BACKOFF is one failed
// attempt; SHOWING resets the count; the diagnostic is said once per run of
// failures.
func (m *MonitorApp) forwardPictureStates(states <-chan gst.PictureState, diag string) {
	failures := 0
	said := false
	for s := range states {
		switch s {
		case gst.PictureStateBackoff:
			failures++
		case gst.PictureStateShowing:
			failures = 0
			said = false
		}

		m.picStateMu.Lock()
		m.lastPicture = s
		m.picStateMu.Unlock()

		// The overlay follows the state BEFORE the event goes out, so that by
		// the time the page is told the picture has stopped, the window over
		// the mosaic has already gone.
		m.applyPictureVisibility()

		m.events.send(EventPicture, s)

		if !said && failures >= pictureDiagnoseAfter {
			said = true
			m.emitError(errors.New("wslcomms: " + diag))
		}
	}
}

// stopPictureForTeardown is the monitor's shutdown step: stop the pipeline,
// THEN destroy the overlay it renders into. The order is fixed: gstd3d11
// subclasses the window it is given, and a DestroyWindow racing its subclass
// restore runs an unloaded window procedure. The overlay's Close bounds itself
// and reports gst.ErrAbandonedThread when it gave up; the joined error keeps
// that inspectable, and the caller ends the process by TerminateProcess
// rather than exit through a wedged thread.
func (m *MonitorApp) stopPictureForTeardown() error {
	var problems []error
	if err := m.StopPicture(); err != nil && !errors.Is(err, errPictureNotRunning) {
		problems = append(problems, err)
	}

	m.picViewMu.Lock()
	overlay := m.picOverlay
	m.picOverlay = nil
	m.picViewMu.Unlock()
	if overlay != nil {
		if err := overlay.Close(); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}
