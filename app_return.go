//go:build dev || production || bindings

// app_return.go is the SRT return monitor's half of the bound surface: five
// methods, one event, and the goroutine that forwards the monitor's states to
// the RETURN lamp.
//
// Owner: WP-R. It is behind the same build tags as app.go, for the same reason,
// and it does not require cgo: at Gate A it drives internal/gst's return stub.
//
// # What this file is careful about
//
// One thing, mostly. THE RETURN MONITOR MUST NOT BE ABLE TO DISTURB THE
// CONTRIBUTION FEED. internal/gst keeps them apart at the pipeline level; this
// file keeps them apart at the application level, and the way it does that is
// worth stating because it is easy to undo by accident:
//
//   - Separate lock. retMu, never sessMu. Nothing here takes sessMu and nothing
//     in the session path takes retMu, so a StartReturn cannot wait on a
//     gst.Pipeline.Stop that has wedged on a broken capture device, and a Start
//     cannot wait on a wedged headphone endpoint.
//
//   - Separate state. A different session struct, a different monitor, a
//     different event. There is no shared field between the two paths except
//     the configuration snapshot, which both read and neither mutates.
//
//   - Separate validation. config.Validate gates Start; config.ValidateReturn
//     gates StartReturn. A mistyped returnChannel must never be a reason a match
//     does not go on air. See that method for the full argument.
//
//   - Separate error reporting. A failed return attempt is logged by
//     internal/gst and shown on the RETURN lamp; it does NOT go to the "error"
//     event. During a match the error toast has to mean "the feed is in
//     trouble", and a second source of rate-limited toasts from a monitor the
//     commentator can already hear is failing would bury the ones that matter.
//
//     AMENDED, with one exception, deliberately narrow. A return that cannot
//     handshake says nothing at all: the lamp shows BACKOFF, which is what it
//     shows for a peer that has gone away for ten seconds, and the reason lives
//     in a log file nobody has open. That cost the operator an afternoon
//     against an ENCRYPTED M2L-X output with no passphrase set. So after
//     returnDiagnoseAfter consecutive failed attempts — never before, so a blip
//     is silent — this file emits exactly ONE message per StartReturn saying
//     what is being dialled and with what encryption. One message, then quiet
//     for the rest of the session, which is not the stream of toasts the
//     paragraph above rules out. See returnDiagnostic.
//
// # And one thing it refuses to do
//
// It will not start while returnSource is "webrtc". Both return paths reach the
// same headphones by different routes with different latencies, so running them
// together plays the programme twice a few hundred milliseconds apart — not
// echo, just unusable. The exclusivity is enforced here rather than trusted to
// the frontend, because the frontend is where a race between a settings save and
// a monitor page reload would put both of them up at once.
package main

import (
	"errors"
	"fmt"
	"sync"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
	"wslcomms/internal/secrets"
)

// errReturnNotRunning is returned by StopReturn when no monitor is running. It
// is a sentinel rather than a string so that teardown can tell "there was
// nothing to stop" from a real failure, exactly as errNotSending does for the
// contribution session.
var errReturnNotRunning = errors.New("wslcomms: the return monitor is not running")

// errReturnAlreadyRunning is returned by StartReturn when one is.
var errReturnAlreadyRunning = errors.New("wslcomms: the return monitor is already running")

// returnDiagnoseAfter is how many consecutive failed connection attempts pass
// before the one diagnostic message is emitted.
//
// Three, because the first two are indistinguishable from an M2L-X output the
// operator has not switched on yet, and because gst.ReturnBackoffLadder makes
// three attempts about 14 seconds of trying — long enough to mean something,
// short enough that the operator is still standing at the machine.
const returnDiagnoseAfter = 3

// returnSession is one running return monitor and the goroutine forwarding its
// states to the frontend. It is the mirror of session, and deliberately not the
// same type: sharing one would be the first step towards sharing a lock.
type returnSession struct {
	mon gst.ReturnMonitor
	wg  sync.WaitGroup
}

// ---------------------------------------------------------------------------
// The bound surface
// ---------------------------------------------------------------------------

// ListOutputDevices returns the audio PLAYBACK endpoints for the headphone
// dropdown on the SRT return path. Device.ID is the WASAPI IMMDevice endpoint
// GUID and is what SaveConfig must be given as headphoneEndpointId;
// Device.Name is for display only.
//
// This is NOT the identifier the WebRTC return path uses. That one is a browser
// mediaDeviceId, obtained from enumerateDevices in JavaScript and stored as
// headphoneDeviceId, and the two cannot be substituted for one another in either
// direction. See config.Config.HeadphoneEndpointID for what goes wrong when they
// are, which is nothing visible — audio in the wrong ears and no diagnostic.
func (a *App) ListOutputDevices() ([]gst.Device, error) {
	if a.gstInitErr != nil {
		return nil, a.gstInitErr
	}
	return gst.ListOutputDevices()
}

// IsSRTReturnSelected reports whether the SRT return is the configured return
// path. The frontend attaches its WebRTC return audio only when this is false.
//
// It exists so that exactly one place decides which path owns the headphones.
// The alternative — the frontend reading returnSource out of GetConfig and
// comparing it to a string literal of its own — puts the same decision in two
// languages, and the failure of the two disagreeing is both of them playing at
// once, which is the one outcome this setting exists to prevent.
func (a *App) IsSRTReturnSelected() (bool, error) {
	return a.snapshotConfig().UsesSRTReturn(), nil
}

// GetReturnState returns the current state of the SRT return monitor, for a
// page that has just loaded and has not yet seen a "return" event.
//
// It is a getter over cached state rather than a query of the pipeline: the
// monitor pushes every transition on the "return" event, and a getter that
// interrogated GStreamer would take a lock held across a state change to tell
// the UI something it is already being told.
func (a *App) GetReturnState() (gst.ReturnState, error) {
	a.retStateMu.Lock()
	defer a.retStateMu.Unlock()
	return a.lastReturn, nil
}

// StartReturn opens the SRT return: it dials the configured M2L-X output as a
// caller, decodes the AAC, applies the channel selection and plays it to the
// headphone endpoint. Progress is reported on the "return" event, not by this
// method, which returns as soon as the reconnect loop is running.
//
// A connection failure is NOT an error from StartReturn. The monitor retries
// indefinitely, and the commentator may well press the button before the
// operator has enabled the output. What StartReturn does return is a
// configuration that cannot work, and it names the field.
func (a *App) StartReturn() error {
	a.retMu.Lock()
	defer a.retMu.Unlock()

	if a.closing.Load() {
		// The window is going away. Building a pipeline now would open a
		// playback endpoint and an SRT socket that teardown has already walked
		// past, and the process would exit still holding them. Same reasoning as
		// startSession; see step 0 of the shutdown order in app.go's header.
		return errShuttingDown
	}
	if a.ret != nil {
		return errReturnAlreadyRunning
	}
	if a.gstInitErr != nil {
		return a.gstInitErr
	}

	cfg := a.snapshotConfig()
	if !cfg.UsesSRTReturn() {
		return fmt.Errorf(
			"wslcomms: cannot start the SRT return: returnSource is %q — "+
				"set it to %q on the Settings screen, or leave the WebRTC return to the monitor page. "+
				"Running both plays the programme twice with a few hundred milliseconds between the copies",
			cfg.EffectiveReturnSource(), config.ReturnSourceSRT)
	}
	if err := cfg.ValidateReturn(); err != nil {
		// ValidateReturn joins one message per bad field, so the operator sees
		// every problem at once instead of one edit-fail cycle at a time.
		return fmt.Errorf("wslcomms: cannot start the SRT return, the configuration is incomplete: %w", err)
	}

	passphrase, err := a.returnPassphrase(cfg)
	if err != nil {
		return err
	}

	mon := a.newReturnMonitor()
	opts := a.returnOpts(cfg, passphrase)

	if err := mon.Start(opts); err != nil {
		// gst.ReturnMonitor.Start only fails on a configuration it cannot use,
		// and it leaves nothing running when it does — a monitor whose Start
		// failed has no pipeline and no goroutine, so unlike the sender there is
		// nothing here to stop and nothing to leak.
		return fmt.Errorf("wslcomms: starting the SRT return: %w", err)
	}

	sess := &returnSession{mon: mon}
	// The diagnostic is built HERE, from the options actually handed to the
	// monitor, and captured by the forwarder below. Building it from a fresh
	// config snapshot later would describe whatever the operator had saved by
	// then rather than what the running pipeline is dialling, which is exactly
	// the confusion it exists to end.
	diag := returnDiagnostic(opts)
	sess.wg.Add(1)
	// Started only after Start has succeeded, for the same reason the sender's
	// forwarder is: on failure the monitor's loop never launches, its states
	// channel is never closed, and a forwarder ranging over it would be a
	// goroutine leaked per failed StartReturn.
	go func() {
		defer sess.wg.Done()
		a.forwardReturnStates(mon.States(), diag)
	}()
	a.ret = sess

	return nil
}

// StopReturn closes the SRT return.
//
// It holds retMu for its whole duration, including the blocking wait inside
// gst.ReturnMonitor.Stop, so that a StartReturn racing it cannot open a second
// pipeline on the same headphone endpoint. By the time it returns the pipeline
// is at NULL, gst.ReturnStateStopped has been emitted and the forwarding
// goroutine has exited.
//
// It returns errReturnNotRunning when nothing was running, which is what lets
// teardown call it unconditionally.
func (a *App) StopReturn() error {
	a.retMu.Lock()
	defer a.retMu.Unlock()

	sess := a.ret
	if sess == nil {
		return errReturnNotRunning
	}
	a.ret = nil

	err := sess.mon.Stop()
	sess.wg.Wait()
	if err != nil {
		return fmt.Errorf("wslcomms: stopping the SRT return: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// newReturnMonitor builds the monitor, through the returnDial seam so the tests
// can substitute a fake. Nil means the real one.
func (a *App) newReturnMonitor() gst.ReturnMonitor {
	if a.returnDial != nil {
		return a.returnDial()
	}
	return gst.NewReturnMonitor()
}

// returnOpts builds the monitor's options from a configuration snapshot and the
// passphrase already read from Credential Manager.
//
// It is separate from StartReturn so that what the monitor is actually given can
// be asserted without running one.
//
// passphrase is a secret. It goes into gst.ReturnOpts, which internal/gst sets
// with g_object_set rather than in the URI, and must not be logged or returned
// across the Wails boundary.
func (a *App) returnOpts(cfg *config.Config, passphrase string) gst.ReturnOpts {
	return gst.ReturnOpts{
		// EffectiveSRTHost, not SRTHost, and not a field of its own. The return
		// follows the M2L-X host exactly as the send path does: on every
		// instance seen so far the SRT listener answers on the same name as the
		// REST API, and a third host field would be a third thing to get wrong
		// under pressure for no case anyone has met.
		Host:      cfg.EffectiveSRTHost(),
		Port:      cfg.EffectiveSRTReturnPort(),
		LatencyMs: cfg.SRTLatencyMs,

		// The RETURN path's own SRT credentials, and NOT the send path's.
		//
		// They used to be shared, on the reasoning that M2L-X's endpoints are
		// on one instance with one key configuration. That reasoning was wrong,
		// and it is what made this path unusable: encryption on M2L-X is set
		// PER OUTPUT, and on the live instance Output 1 (pgm, 40501) is
		// unencrypted while Outputs 2 and 3 are not. Sharing one passphrase and
		// one key length means the operator cannot describe that arrangement at
		// all — whichever way they set it, one of the two paths is wrong.
		//
		// So: a separate Credential Manager entry (secrets.KeySRTReturn) and a
		// separate key length (config srtReturnPBKeyLen). Changing the send
		// passphrase can no longer break a working monitor, or the reverse.
		Passphrase: passphrase,
		PBKeyLen:   cfg.SRTReturnPBKeyLen,

		Channel:        gst.ReturnChannel(cfg.EffectiveReturnChannel()),
		OutputDeviceID: cfg.HeadphoneEndpointID,
	}
}

// returnPassphrase reads the RETURN path's SRT passphrase from Credential
// Manager and checks it against the configured return key length.
//
// It reads secrets.KeySRTReturn, NOT secrets.KeySRT. Those are the keys to two
// different M2L-X endpoints — the commentary input and one of the programme
// outputs — and M2L-X sets encryption per endpoint, so they are routinely
// different values. Reading the send path's key here is what this function used
// to do and it is why the return could not be made to work: there was no way to
// give the monitor a key without also changing the key the feed goes out with.
//
// # Where it is strict and where it is not
//
// It refuses exactly one combination: srtReturnPBKeyLen non-zero with no stored
// passphrase. That asks libsrt for an encrypted session with no key, which
// cannot succeed against anything, and it is the precise shape of the fault
// that cost the operator an afternoon — the return dialled an encrypted output
// with nothing to encrypt with, and the ladder retried in silence for ever.
// Refusing here, naming the Credential Manager target and the setting, is worth
// far more than an amber lamp.
//
// It used to refuse nothing, on the argument that a monitor should not fail
// over "a field the operator has not been asked about yet". That argument was
// right at the time and has expired: the Settings screen now HAS a return
// passphrase field and a return key-length control beside it, so a non-zero key
// length is a statement the operator made, not a default they inherited.
//
// Everything else stays forgiving. A stored passphrase with a zero key length
// is passed through — internal/gst's ReturnOpts.normalise defaults an unset key
// length to 16 when a passphrase is present, so this is a working encrypted
// session rather than a contradiction. A missing passphrase with a zero key
// length is an ordinary unencrypted return, which is what the default port
// (40501, src=pgm, measured encrypted=false) actually wants. And a wrong
// passphrase cannot be detected here at all; only the far end knows.
//
// A Credential Manager that cannot be READ is a fault rather than a state and
// is always reported.
//
// The returned value is a secret and must never reach a log line, an error
// string or the Wails boundary.
func (a *App) returnPassphrase(cfg *config.Config) (string, error) {
	passphrase, err := a.store.Get(secrets.KeySRTReturn)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		if cfg.SRTReturnPBKeyLen != 0 {
			return "", fmt.Errorf(
				"wslcomms: cannot start the SRT return: srtReturnPBKeyLen is %d, which asks for an "+
					"encrypted session with the M2L-X output on port %d, but no return passphrase is "+
					"stored in Windows Credential Manager under %q — enter it on the Settings screen, "+
					"or set the return key length to 0 if that output is not encrypted. "+
					"This is a different passphrase from the one the contribution feed uses",
				cfg.SRTReturnPBKeyLen, cfg.EffectiveSRTReturnPort(), secrets.TargetSRTReturn)
		}
		return "", nil
	case err != nil:
		return "", fmt.Errorf("wslcomms: reading the SRT return passphrase from %q: %w",
			secrets.TargetSRTReturn, err)
	}
	return passphrase, nil
}

// ---------------------------------------------------------------------------
// Saying why the return will not connect
// ---------------------------------------------------------------------------

// returnDiagnostic renders the one message emitted after returnDiagnoseAfter
// consecutive failed attempts: what is being dialled, with what encryption, and
// which way the encryption could be wrong.
//
// # Why it says this and not what libsrt said
//
// libsrt knows the actual answer and distinguishes the two cases by name —
// ERROR:BADSECRET when a passphrase was offered and rejected, ERROR:UNSECURE
// when the two ends disagree about whether there should be one at all. That
// reason reaches internal/gst, which logs it in returnMonitor.report and then
// discards it: gst.ReturnOpts has no OnConnectError callback, the way
// sender.Opts does for the contribution path, so there is no route for it to
// reach this file or the operator's screen. Contract rule 3 says report an
// interface gap rather than edit round it, so it is REPORTED: gst.ReturnOpts
// wants an OnConnectError func(error), and until it has one the raw libsrt
// reason is in the log only.
//
// What is said instead is only what this process can actually observe: the
// endpoint it dialled, the encryption it offered, and the fact that N attempts
// in a row failed. No error string is invented and no cause is asserted. The
// wording names encryption as the thing to check because it is the one input
// the operator controls that fails exactly like this — silently, identically,
// for ever — and because the two ways of getting it wrong are the two the
// message enumerates.
//
// opts carries the passphrase and is taken BY VALUE and read for one boolean.
// Nothing in the returned string is derived from its contents.
//
// The key length is restated here rather than read straight out of opts because
// opts has NOT been through gst.ReturnOpts.normalise: Start takes its argument
// by value, so the defaulting happens on the monitor's own copy and never comes
// back. normalise turns a passphrase with a zero key length into AES-128, and a
// message that reported "AES-0" for the session actually being negotiated would
// send the operator looking for a setting that is not wrong.
func returnDiagnostic(opts gst.ReturnOpts) string {
	endpoint := fmt.Sprintf("srt://%s:%d", opts.Host, opts.Port)

	keylen := opts.PBKeyLen
	if opts.Passphrase != "" && keylen == 0 {
		keylen = 16 // gst.ReturnOpts.normalise's default; see above
	}

	if opts.Passphrase == "" {
		return fmt.Sprintf(
			"the SRT return has failed to connect %d times in a row. It is dialling %s with NO "+
				"encryption. If that M2L-X output has a passphrase set, every handshake will keep "+
				"being refused: set the SRT return passphrase and key length on the Settings screen. "+
				"The exact reason libsrt gave is in the application log",
			returnDiagnoseAfter, endpoint)
	}
	return fmt.Sprintf(
		"the SRT return has failed to connect %d times in a row. It is dialling %s with AES-%d "+
			"encryption. If that passphrase is wrong, or if that M2L-X output is not encrypted at "+
			"all, every handshake will keep being refused: check the SRT return passphrase and key "+
			"length on the Settings screen. Note that it is a different passphrase from the "+
			"contribution feed's. The exact reason libsrt gave is in the application log",
		returnDiagnoseAfter, endpoint, keylen*8)
}

// forwardReturnStates pumps the monitor's state transitions to the frontend and
// records the latest for domReady's replay. It returns when the monitor closes
// the channel, which gst.ReturnMonitor.Stop does after emitting
// gst.ReturnStateStopped.
//
// It is the mirror of forwardSenderStates and goes through the same event pump,
// so a renderer that has stopped reading loses the oldest events rather than
// stalling the reconnect loop.
//
// It takes retStateMu and NEVER retMu. StopReturn holds retMu across the join of
// this goroutine, so taking retMu here would deadlock the first Stop that landed
// while a transition was in flight. That is why the two locks exist; see app.go's
// lock order.
//
// # The failure count
//
// A transition to BACKOFF is one failed attempt, because the monitor's loop
// only reaches BACKOFF from a failure — every other exit from an attempt is
// Stop, which ends the loop instead. RECEIVING resets the count, so a
// mid-match drop that recovers never speaks, and the count is per-session state
// on this goroutine's stack rather than on the App: it belongs to one monitor,
// and an operator who stops and starts the return has said they want to be told
// again.
//
// diag is emitted at most once, on the returnDiagnoseAfter'th consecutive
// failure. It carries no secret: returnDiagnostic builds it from the endpoint
// and the key length only.
func (a *App) forwardReturnStates(states <-chan gst.ReturnState, diag string) {
	failures := 0
	said := false

	for s := range states {
		switch s {
		case gst.ReturnStateBackoff:
			failures++
		case gst.ReturnStateReceiving:
			// It connected. Whatever was wrong is not wrong now, and a later
			// outage starts its own count.
			failures = 0
			said = false
		}

		a.retStateMu.Lock()
		a.lastReturn = s
		a.retStateMu.Unlock()
		a.events.send(EventReturn, s)

		if !said && failures >= returnDiagnoseAfter {
			said = true
			// Emitted AFTER the state, so the RETURN lamp is already showing
			// BACKOFF when the explanation for it arrives.
			a.emitError(errors.New("wslcomms: " + diag))
		}
	}
}
