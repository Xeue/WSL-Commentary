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
//   - Separate state. A different session struct, a different monitor, a
//     different event. There is no shared field between the two paths except
//     the configuration snapshot, which both read and neither mutates.
//   - Separate validation. config.Validate gates Start; config.ValidateReturn
//     gates StartReturn. A mistyped returnChannel must never be a reason a match
//     does not go on air. See that method for the full argument.
//   - Separate error reporting. A failed return attempt is logged by
//     internal/gst and shown on the RETURN lamp; it does NOT go to the "error"
//     event. During a match the error toast has to mean "the feed is in
//     trouble", and a second source of rate-limited toasts from a monitor the
//     commentator can already hear is failing would bury the ones that matter.
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
	// Started only after Start has succeeded, for the same reason the sender's
	// forwarder is: on failure the monitor's loop never launches, its states
	// channel is never closed, and a forwarder ranging over it would be a
	// goroutine leaked per failed StartReturn.
	sess.wg.Add(1)
	go func() {
		defer sess.wg.Done()
		a.forwardReturnStates(mon.States())
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

		// The same SRT credentials as the send path. M2L-X's outputs are on the
		// same instance with the same key configuration, and asking the operator
		// to store a second passphrase for it would invite the two to drift
		// apart with no way to tell which one a failure came from.
		Passphrase: passphrase,
		PBKeyLen:   cfg.PBKeyLen,

		Channel:        gst.ReturnChannel(cfg.EffectiveReturnChannel()),
		OutputDeviceID: cfg.HeadphoneEndpointID,
	}
}

// returnPassphrase reads the SRT passphrase for the return path.
//
// It is deliberately more forgiving than App.srtPassphrase, which gates the
// contribution feed. That one refuses to start when pbkeylen is non-zero and no
// passphrase is stored, because an encrypted session with no key fails inside
// libsrt with an error nobody can read twenty minutes before kick-off, and
// because Start is the moment to be strict.
//
// Here the same combination is not worth refusing over. The return is a
// monitor; if the key is wrong the SRT handshake fails, the RETURN lamp goes
// amber, the reason is in the log, and the reconnect loop keeps trying — which
// is a better outcome than a button that will not work and an error box about a
// field the operator has not been asked about yet. What is NOT tolerated is a
// Credential Manager that cannot be read at all, because that is a fault rather
// than a state.
//
// The returned value is a secret and must never reach a log line, an error
// string or the Wails boundary.
func (a *App) returnPassphrase(cfg *config.Config) (string, error) {
	passphrase, err := a.store.Get(secrets.KeySRT)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("wslcomms: reading the SRT passphrase from %q: %w", secrets.TargetSRT, err)
	}
	return passphrase, nil
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
func (a *App) forwardReturnStates(states <-chan gst.ReturnState) {
	for s := range states {
		a.retStateMu.Lock()
		a.lastReturn = s
		a.retStateMu.Unlock()
		a.events.send(EventReturn, s)
	}
}
