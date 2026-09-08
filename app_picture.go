//go:build dev || production || bindings

// app_picture.go is the SRT PICTURE path's half of the bound surface: four
// methods, one event, and the goroutine that forwards the picture's states to
// the page. The DeckLink PREVIEW's surface lives here too, further down.
//
// Owner: WP-P. It is behind the same build tags as app.go, for the same reason,
// and it does not require cgo: at Gate A it drives a fake monitor, and the real
// one is a child process it never has to run.
//
// # THE PICTURE IS A SEPARATE PROCESS
//
// gst.PictureMonitor is implemented here by childPictureMonitor
// (app_picture_child.go): this executable relaunched with WSLCOMMS_PICTURE_CHILD
// set, decoding the SRT programme output and rendering it into a top-level
// window OF ITS OWN. The main window never shows the SRT picture; its tile
// always carries the WebRTC mosaic, and the high-resolution picture is a second
// window the commentator puts wherever they like.
//
// It used to be a native child window painted over the page — overlay_windows.go
// is that, and the preview still uses it. The picture moved out because a
// decoder, a GPU driver or a libsrt socket that wedges INSIDE THIS PROCESS is a
// wedge in the application: Stop blocks in it, teardown abandons it, and the
// operator's remedy was to restart the whole program mid-match, feed and all.
// In a child, the same wedge is one TerminateProcess away from gone, with the
// contribution feed, the audio and the window untouched. RefreshPicture is that
// remedy as a button.
//
// # What this is for, stated plainly because it has been got wrong once
//
// THE COMMENTATOR'S PICTURE COMES FROM SRT. THE AUDIO COMES FROM KINESIS.
//
// The picture used to be the KVS multiviewer mosaic: a 2240x1440 track,
// CSS-cropped to a 640x360 tile and scaled up, which is why it looked soft. This
// path adds the M2L-X programme output itself — H.265, 1920x1080 at 50p, 15000
// kbps — decoded in hardware and presented in its own window. The mosaic stays
// on the page as the FALLBACK, because a soft picture beats no picture and a
// commentator must never be looking at black.
//
// The audio is not touched by any of this. It is the Kinesis/WebRTC peer
// connection, with its existing bus and channel selection, exactly as before.
//
// # What this file is careful about
//
//  1. THE PICTURE MUST NOT BE ABLE TO DISTURB THE CONTRIBUTION FEED OR THE
//     AUDIO. Its own locks (picMu, picStateMu — never sessMu and never retMu),
//     its own monitor, its own event, its own PROCESS. Nothing here takes a lock
//     anything else holds, so a wedged GPU cannot make START slow — and now
//     cannot make anything in this process slow at all.
//
//     The order is picMu → picStateMu, picMu is the only one held across
//     something that blocks, and the state forwarder takes only picStateMu. See
//     the lock comments on App.
//
//  2. A PICTURE PROCESS THAT DIES IS NOT "ALREADY RUNNING". The operator can
//     close its window, it can crash, and the parent finds out through the
//     monitor's states channel closing. pictureSession.exited records that so
//     the next StartPicture reaps the dead session and starts a fresh one rather
//     than refusing.
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
//     an operator gets encryption wrong — reaches the picture process, is logged
//     there, and is then discarded. The same gap is reported for the return path
//     in app_return.go. Until it is closed, pictureDiagnostic below can only say
//     what this process observed.
package main

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

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

	// exited is set by the forwarding goroutine when the monitor's states
	// channel closes. For the picture process that can happen without
	// StopPicture — the process died, or the operator closed its window — and
	// StartPicture uses it to reap such a session instead of refusing to start.
	exited atomic.Bool
}

// ---------------------------------------------------------------------------
// The bound surface
// ---------------------------------------------------------------------------

// StartPicture opens the SRT picture: it launches the picture process, which
// dials the configured M2L-X programme output as a caller, decodes the H.265 on
// the GPU and presents it in a window of its own. Progress is reported on the
// "picture" event, not by this method, which returns as soon as the process is
// up and its reconnect loop is running.
//
// A connection failure is NOT an error from StartPicture. The monitor retries
// indefinitely, and the commentator may well press the button before the operator
// has enabled the output. What StartPicture does return is a configuration that
// cannot work, or a picture process that could not be launched or refused its
// options, and it says which.
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
	// A session whose monitor has already ended — the picture process died, or
	// the operator closed its window — is not "already running"; it is reaped
	// here and the start goes ahead. The join is prompt: the forwarder has
	// already exited, which is how exited came to be set. It is checked BEFORE
	// the already-running refusal below, or a dead picture would be refused a
	// restart for ever.
	if a.pic != nil && a.pic.exited.Load() {
		sess := a.pic
		a.pic = nil
		_ = sess.mon.Stop()
		sess.wg.Wait()
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

	mon := a.newPictureMonitor()
	opts := a.pictureOpts(cfg, passphrase)

	if err := mon.Start(opts); err != nil {
		// gst.PictureMonitor.Start only fails on a configuration it cannot use,
		// and it leaves nothing running when it does: the child process, if one
		// was launched, has already been killed by the monitor, so unlike the
		// sender there is nothing here to stop and nothing to leak.
		return fmt.Errorf("wslcomms: starting the picture: %w", err)
	}

	// If this machine has no hardware HEVC decoder the picture is now on the
	// software path (avdec_h265). Tell the operator once, quietly. It is decided
	// HERE rather than in the child because the answer is a property of the
	// GStreamer registry this process shares with the child, and the once guard
	// has to live in the process that outlives every Refresh.
	a.maybeNotePictureSoftwareDecode()

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
		// The channel closed: the monitor stopped, whether because StopPicture
		// asked or because the picture process died or its window was closed.
		// Recorded so that the next StartPicture reaps a dead session rather
		// than refusing it as "already running".
		sess.exited.Store(true)
	}()
	a.pic = sess

	return nil
}

// RefreshPicture restarts the picture from nothing: it stops the running picture
// process, if any, and starts a fresh one against the saved configuration.
//
// It is the operator's "the picture has frozen" button, and because the picture
// is a separate process it is a real remedy rather than a hopeful one: the old
// process is ended — killed, if its pipeline has wedged and it will not exit —
// and the new one has a fresh GStreamer, a fresh SRT socket, a fresh decoder and
// a fresh window. Nothing about the contribution feed or the audio is touched.
//
// A picture that was not running is simply started. A stop that fails is
// reported and the start is still attempted, because the thing the operator
// wants is a picture, and the old process being awkward about leaving is not a
// reason to deny them one.
func (a *App) RefreshPicture() error {
	if err := a.StopPicture(); err != nil && !errors.Is(err, errPictureNotRunning) {
		log.Printf("wslcomms: refreshing the picture: the old one did not stop cleanly: %v", err)
	}
	return a.StartPicture()
}

// maybeNotePictureSoftwareDecode emits the software-decode note at most once for
// the life of the process, if and only if the picture will decode on the CPU
// because the machine has no hardware HEVC decoder.
//
// It is a NOTE, not an error: the picture works, and on a machine with the
// headroom it works well — this only explains why the CPU is busier. On a
// low-powered box it may also be the reason the picture judders, which is exactly
// the fault that went undiagnosed before the software fallback existed. The once
// guard is the App's, so stopping and restarting the picture does not repeat it;
// a fresh process says it again, which is right, because that is a fresh machine
// being set up. On a build with the hardware decoder present,
// gst.PictureDecoderIsSoftware returns false and nothing is said.
func (a *App) maybeNotePictureSoftwareDecode() {
	software, factory := gst.PictureDecoderIsSoftware()
	if !software {
		return
	}
	a.pictureSoftwareNoteOnce.Do(func() {
		a.emitNote(fmt.Sprintf(
			"This machine has no hardware HEVC decoder, so the picture is being decoded in "+
				"software (%s). That uses noticeably more CPU and can judder at 1080p50 on a "+
				"low-powered machine. To get hardware decoding back, update the graphics driver; "+
				"or set that M2L-X output to H.264, which this machine can decode in hardware.",
			factory))
	})
}

// StopPicture closes the SRT picture: the picture process is told to stop and
// exits, taking its window with it, and it is killed if it will not.
//
// It holds picMu for its whole duration, including the blocking wait inside
// gst.PictureMonitor.Stop and the join of the forwarding goroutine, so that a
// StartPicture racing it cannot launch a second process dialling the same
// output. By the time it returns the process is gone, gst.PictureStateStopped
// has been emitted and the forwarding goroutine has exited.
//
// It returns errPictureNotRunning when nothing was running, which is what lets
// teardown call it unconditionally.
func (a *App) StopPicture() error {
	a.picMu.Lock()
	defer a.picMu.Unlock()

	sess := a.pic
	if sess == nil {
		return errPictureNotRunning
	}
	a.pic = nil

	err := sess.mon.Stop()
	sess.wg.Wait()

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

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// newPictureMonitor builds the monitor, through the pictureDial seam so the
// tests can substitute a fake. Nil means the real one, which is the picture
// PROCESS — see app_picture_child.go. gst.NewPictureMonitor, the in-process
// monitor, is what that process runs; this process never builds one.
func (a *App) newPictureMonitor() gst.PictureMonitor {
	if a.pictureDial != nil {
		return a.pictureDial()
	}
	return newChildPictureMonitor()
}

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

// pictureOpts builds the monitor's options from a configuration snapshot and
// the passphrase already read from the credential store.
//
// It is separate from StartPicture so that what the monitor is actually given can
// be asserted without running one.
//
// passphrase is a secret. It goes into gst.PictureOpts, which the picture process
// sets with g_object_set rather than in the URI, and must not be logged or
// returned across the Wails boundary. childPictureMonitor carries it to the
// process on stdin and nowhere else.
//
// There is NO WindowHandle here. The picture process makes its own window and
// fills that field in itself; this process has no handle to give it and
// gst.PictureOpts.normalise would refuse a zero one.
func (a *App) pictureOpts(cfg *config.Config, passphrase string) gst.PictureOpts {
	return gst.PictureOpts{
		// EffectiveSRTReturnHost, not EffectiveSRTHost: the picture follows the
		// M2L-X host exactly as the send does UNLESS the return override is on, in
		// which case the picture and the SRT audio return both move to the relay
		// and the send stays put. The picture is a RETURN, so it takes the return's
		// host. See config.SRTReturnOverrideURL.
		Host: cfg.EffectiveSRTReturnHost(),

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

// forwardPictureStates pumps the monitor's state transitions to the frontend
// and records the latest for a page reload.
//
// It is the mirror of forwardReturnStates and goes through the same event pump,
// so a renderer that has stopped reading loses the oldest events rather than
// stalling the reconnect loop.
//
// It takes picStateMu and NEVER picMu. StopPicture holds picMu across the join
// of this very goroutine; a transition that waited for picMu while a Stop was
// waiting for the transition would be the deadlock the two locks exist to
// prevent.
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

		a.events.send(EventPicture, s)

		if !said && failures >= pictureDiagnoseAfter {
			said = true
			// Emitted AFTER the state, so the page is already showing the
			// fallback when the explanation for it arrives.
			a.emitError(errors.New("wslcomms: " + diag))
		}
	}
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

// stopPictureForTeardown is the picture's step of the ordered shutdown: end the
// picture process, then take the preview surface away as a belt.
//
// # The picture process
//
// StopPicture closes the child's stdin, waits up to pictureChildStopBudget for
// it to exit, and kills it if it has not. The ordering the picture used to
// argue at length here — stop the monitor, THEN destroy the window it renders
// into, because gstd3d11 subclasses that window and a DestroyWindow racing its
// subclass restore runs an unloaded window procedure — is now the child's own
// business, done inside runPictureChildAndExit in a process where nothing else
// is at stake, and a child that wedges in it is killed rather than waited for.
// This process holds no handle, no sink and no window of the picture's.
//
// # The preview surface
//
// It normally goes with the capture layer, whose step teardownOrdered runs two
// steps before this one — but a capture step that overran was ABANDONED rather
// than waited for, and this is the last chance to take a window off the
// operator's screen before the process ends. It is idempotent and costs one
// dispatch when there is nothing there, which is every ordinary quit.
//
// Its Close is the one call here that can report gst.ErrAbandonedThread: on
// Windows it bounds itself by overlayCloseBudget and gives up on its message
// thread rather than hanging, and the error it returns then is what turns this
// step from "finished with a complaint" into "abandoned", which ends the
// process by TerminateProcess. See teardownStep and exit_windows.go: a thread
// left inside user32!DestroyWindow, or inside gstd3d11's subclass procedure,
// is exactly what ExitProcess must not be allowed to run DLL_PROCESS_DETACH
// over.
//
// # Bounding
//
// The whole function runs inside app.go's teardownStep under pictureStopBudget,
// which ABANDONS it if it overruns and force-exits the process. That is the
// bound on StopPicture's wait, over and above the child's own; the preview's
// Close bounds itself.
func (a *App) stopPictureForTeardown() error {
	var problems []error

	if err := a.StopPicture(); err != nil && !errors.Is(err, errPictureNotRunning) {
		problems = append(problems, err)
	}

	if err := a.stopCapturePreview(); err != nil {
		problems = append(problems, err)
	}

	// errors.Join and NOT a formatted summary, because what teardownStep does
	// with this error depends on being able to see through it: a preview Close
	// that gave up wraps gst.ErrAbandonedThread, the joined error's Unwrap
	// []error keeps errors.Is working through it, and that is what turns this
	// step from "finished with a complaint" into "abandoned" and ends the
	// process by TerminateProcess. A fmt.Errorf with %v here would flatten the
	// sentinel to text and put the shutdown hang back. See teardownStep.
	return errors.Join(problems...)
}
