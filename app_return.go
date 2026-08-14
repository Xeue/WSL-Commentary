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
//     what is being dialled, with what encryption, and WHY THE LAST ATTEMPT
//     FAILED. One message, then quiet for the rest of the session, which is not
//     the stream of toasts the paragraph above rules out. See returnDiagnostic.
//
//     The last part of that used to be missing. libsrt names the two ways
//     encryption goes wrong — BADSECRET and UNSECURE, in whichever words the
//     installed GStreamer spells them; see srtRejectBadSecret — and this
//     file could only guess between them, because gst.ReturnOpts had no
//     callback to carry the reason out of internal/gst. It has one now
//     (gst.ReturnOpts.OnConnectError), so the message says what libsrt said
//     rather than what this process could infer, and an unrecognised reason is
//     quoted verbatim rather than summarised into a guess.
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
	"strings"
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

// returnReason carries the reason for the most recent failed attempt from the
// goroutine that learns it to the goroutine that speaks.
//
// Those are two different goroutines and always will be.
// gst.ReturnOpts.OnConnectError is called on the monitor's own reconnect loop;
// forwardReturnStates runs on a goroutine of this file's making, ranging over
// the state channel. Passing the error in a plain field would be a data race on
// the first failed attempt, and the race detector runs on every build.
//
// The ORDER is guaranteed by internal/gst rather than assumed here: the monitor
// calls the callback immediately before it emits ReturnStateBackoff, and that
// emission is the channel send this file's forwarder is waiting on. So by the
// time the forwarder counts the failure, the reason for it has been stored.
//
// It keeps only the latest reason, not a history. Exactly one message is emitted
// per session and what the operator needs in it is why the attempt that has just
// failed failed.
type returnReason struct {
	mu  sync.Mutex
	err error
}

// set records the reason. It is the function handed to
// gst.ReturnOpts.OnConnectError, so it runs on the monitor's state-machine
// goroutine and must not block: a mutex held for one assignment is the whole
// budget.
func (r *returnReason) set(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}

// get returns the latest reason, or nil if none has arrived.
func (r *returnReason) get() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// ---------------------------------------------------------------------------
// The bound surface
// ---------------------------------------------------------------------------

// ListOutputDevices returns the audio PLAYBACK endpoints for the headphone
// dropdown on the SRT return path. Device.ID is what SaveConfig must be given as
// headphoneEndpointId; Device.Name is for display only.
//
// # What Device.ID IS depends on the machine, and the frontend must not care
//
// It is the operating system's own stable identity for the device: a WASAPI
// IMMDevice endpoint GUID on Windows, a CoreAudio device UID on macOS. The
// frontend has no business knowing which, and deliberately has no way to tell —
// it receives an opaque string from this method and hands the same string back
// in SaveConfig. gst.ReturnOpts.OutputDeviceID carries the full account of both
// shapes, including why macOS persists the UID rather than the AudioDeviceID
// integer that osxaudiosink's property actually takes.
//
// # It is NOT the identifier the WebRTC return path uses, on any platform
//
// That one is a browser mediaDeviceId, obtained from enumerateDevices in
// JavaScript and stored as headphoneDeviceId. The two cannot be substituted for
// one another in either direction, and CONTRACT.md forbids merging them. Making
// the native half platform-dependent does not soften that by one inch, and it is
// worth saying why plainly: the browser id is not any operating system's
// identifier for a device. It is a per-origin salted token minted by one
// browsing context, meaningless to WASAPI and to CoreAudio alike, and it changes
// when that context's storage is cleared. There is no platform on which the two
// converge.
//
// See config.Config.HeadphoneEndpointID for what goes wrong when they are
// conflated, which is nothing visible — audio in the wrong ears and no
// diagnostic.
//
// # A config.json carried between the two machines
//
// It fails SAFE rather than confusingly: internal/gst checks the saved id
// against what this machine is actually offering before any sink is given it,
// falls back to the default playback device, and logs the id, the reason and the
// devices that ARE on offer. On a Mac it goes further and says outright that a
// WASAPI endpoint id means the settings file was written on Windows. See
// gst.chooseOutputDevice.
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

	// Wired in BEFORE Start, because Start begins dialling and the first attempt
	// can fail before this function has returned. A callback attached afterwards
	// would miss it.
	//
	// It is reason.set and nothing more. The callback runs on the monitor's
	// reconnect loop — the only goroutine that can get the return back — so
	// anything that formats a message, takes retStateMu or touches the event pump
	// belongs on the forwarder's side of this handoff, not here.
	reason := &returnReason{}
	opts.OnConnectError = reason.set

	if err := mon.Start(opts); err != nil {
		// gst.ReturnMonitor.Start only fails on a configuration it cannot use,
		// and it leaves nothing running when it does — a monitor whose Start
		// failed has no pipeline and no goroutine, so unlike the sender there is
		// nothing here to stop and nothing to leak.
		return fmt.Errorf("wslcomms: starting the SRT return: %w", err)
	}

	sess := &returnSession{mon: mon}
	// The forwarder is given THESE options, the ones actually handed to the
	// monitor, by value. Reading a fresh config snapshot when the message is
	// composed would describe whatever the operator had saved by then rather than
	// what the running pipeline is dialling, which is exactly the confusion the
	// message exists to end.
	//
	// The message itself is composed later rather than here, because half of it
	// is the reason for a failure that has not happened yet.
	sess.wg.Add(1)
	// Started only after Start has succeeded, for the same reason the sender's
	// forwarder is: on failure the monitor's loop never launches, its states
	// channel is never closed, and a forwarder ranging over it would be a
	// goroutine leaked per failed StartReturn.
	go func() {
		defer sess.wg.Done()
		a.forwardReturnStates(mon.States(), opts, reason)
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
					"stored in %s under %q — enter it on the Settings screen, "+
					"or set the return key length to 0 if that output is not encrypted. "+
					"This is a different passphrase from the one the contribution feed uses",
				cfg.SRTReturnPBKeyLen, cfg.EffectiveSRTReturnPort(), secrets.StoreName(), secrets.TargetSRTReturn)
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

// The libsrt rejections this file can turn into an instruction. There are two
// FAULTS and four spellings of them, because the wording is a GStreamer version's
// and not a protocol constant.
//
// # The prediction in the old comment came true within one release
//
// This used to be two strings, BADSECRET and UNSECURE, matched because the
// encryption trials recorded in docs/architecture.md produced exactly those
// tokens against the live instance. gst.ReturnOpts.OnConnectError's
// documentation warned in the same breath that the text is NOT a stable
// interface — a GStreamer message wrapped round libsrt's own wording, liable to
// change between versions — and that is precisely what the macOS port found.
// Measured on GStreamer 1.26.10 over an encrypted SRT loopback, gstsrtsrc.c:206
// renders the reject reason through srt_rejectreason_str() instead, so the two
// faults arrive as:
//
//	wrong passphrase   "Failed to authenticate: Incorrect passphrase (10)"
//	none offered       "Failed to authenticate: Password required or unexpected (11)"
//
// The parenthesised numbers are libsrt's own SRT_REJ_BADSECRET and
// SRT_REJ_UNSECURE, which is what makes these the same two faults and not new
// ones. Both spellings are matched, because the application has to run against
// whichever GStreamer the machine it is installed on carries, and because a
// build that recognised only its own platform's phrasing would give the operator
// a worse message on the other one for no reason.
//
// The design is what made this a two-line repair rather than a bug: nothing here
// DEPENDS on a match. A reason matching none of these is quoted verbatim, which
// is the honest answer and is also what the next rename degrades to.
var (
	// srtRejectBadSecret: a passphrase was offered and the far end has a
	// different one.
	srtRejectBadSecret = []string{"BADSECRET", "INCORRECT PASSPHRASE"}

	// srtRejectUnsecure: the two ends disagree about whether there should be a
	// passphrase at all.
	srtRejectUnsecure = []string{"UNSECURE", "PASSWORD REQUIRED OR UNEXPECTED"}
)

// matchesAnySRTReject reports whether an upper-cased reason contains any of the
// spellings of one fault. The tokens are already upper case; upper is the
// caller's ToUpper of the reason, so the match is case-insensitive without this
// function having to know how the reason was cased.
func matchesAnySRTReject(upper string, tokens []string) bool {
	for _, tok := range tokens {
		if strings.Contains(upper, tok) {
			return true
		}
	}
	return false
}

// returnDiagnostic renders the one message emitted after returnDiagnoseAfter
// consecutive failed attempts: what is being dialled, with what encryption, and
// why the last attempt failed.
//
// # Where each half comes from
//
// The first half is what this process knows for certain — the endpoint it
// dialled and the encryption it offered — and it is said whatever the reason
// turns out to be, because "wrong passphrase" is only actionable next to which
// passphrase was offered to which port.
//
// The second half is libsrt's, by way of gst.ReturnOpts.OnConnectError. It used
// to be absent: internal/gst logged the reason and discarded it, this file
// could only enumerate the two ways encryption goes wrong and invite the
// operator to guess between them, and the message ended by pointing at a log
// file. See returnReasonText for what is now done with it, and for the rule
// that an unrecognised reason is quoted rather than summarised.
//
// reason is nil when no attempt has reported one — a caller that set no
// callback, or a monitor whose states arrived without one. That is not treated
// as an error; the old inferred wording is what the message falls back to,
// because it was useful before the reason existed and it is still useful
// without it.
//
// opts carries the passphrase and is taken BY VALUE. Two things are read from
// it: whether the passphrase is empty, and the passphrase itself, used only to
// redact it back out of a string this process did not compose. See
// redactPassphrase.
//
// The key length is restated here rather than read straight out of opts because
// opts has NOT been through gst.ReturnOpts.normalise: Start takes its argument
// by value, so the defaulting happens on the monitor's own copy and never comes
// back. normalise turns a passphrase with a zero key length into AES-128, and a
// message that reported "AES-0" for the session actually being negotiated would
// send the operator looking for a setting that is not wrong.
func returnDiagnostic(opts gst.ReturnOpts, reason error) string {
	endpoint := fmt.Sprintf("srt://%s:%d", opts.Host, opts.Port)

	keylen := opts.PBKeyLen
	if opts.Passphrase != "" && keylen == 0 {
		keylen = 16 // gst.ReturnOpts.normalise's default; see above
	}

	head := fmt.Sprintf(
		"the SRT return has failed to connect %d times in a row. It is dialling %s with NO encryption",
		returnDiagnoseAfter, endpoint)
	if opts.Passphrase != "" {
		head = fmt.Sprintf(
			"the SRT return has failed to connect %d times in a row. It is dialling %s with AES-%d encryption",
			returnDiagnoseAfter, endpoint, keylen*8)
	}

	return head + ". " + returnReasonText(opts, reason)
}

// returnReasonText is the second half of the diagnostic: what the last failed
// attempt actually reported, turned into an instruction where that can be done
// honestly and quoted where it cannot.
//
// # The three shapes
//
// BADSECRET and UNSECURE are the two libsrt rejections an operator can be given
// an instruction for, and the instruction differs by what WE offered, which this
// process does know. They are matched through srtRejectBadSecret and
// srtRejectUnsecure rather than as literals, because the WORDING differs between
// GStreamer versions while the fault does not:
//
//   - BADSECRET — however this GStreamer spells it; see srtRejectBadSecret —
//     means a passphrase was offered and the far end is configured with a
//     different one. There is exactly one fix and it is a field on the
//     Settings screen.
//   - UNSECURE means the two ends disagree about whether the session is
//     encrypted at all. Which end is wrong is not in the reason — but combined
//     with what we offered it is determined: if we offered nothing, that output
//     wants a passphrase; if we offered one, that output is not encrypted.
//
// Anything else is QUOTED, exactly as it arrived, and said to be unrecognised.
// That is deliberate and it is the more important of the two branches. The
// reasons that reach here are not only libsrt's — a missing plugin, a headphone
// endpoint that has gone away, an M2L-X output that is up and carrying no AAC
// all arrive by the same route — and a message that mapped one of those onto
// "check your passphrase" would send the operator to a setting that is correct
// and cost another afternoon. A string they can paste into a search box is
// worth more than a confident wrong summary.
//
// Every branch ends with the verbatim reason, including the two that name a
// cause, so that a support engineer reading a screenshot has the original text
// and not this file's paraphrase of it.
func returnReasonText(opts gst.ReturnOpts, reason error) string {
	const settings = "The SRT return has its own passphrase and its own key length on the Settings screen, " +
		"separate from the contribution feed's."

	if reason == nil {
		// No reason arrived. Say what can be inferred, and say that it is an
		// inference — this is the wording the message carried before libsrt's
		// answer could reach it.
		if opts.Passphrase == "" {
			return "No reason was reported for the last attempt. If that M2L-X output has a passphrase " +
				"set, every handshake will keep being refused: set the SRT return passphrase and key " +
				"length on the Settings screen. The exact reason is in the application log"
		}
		return "No reason was reported for the last attempt. If that passphrase is wrong, or if that " +
			"M2L-X output is not encrypted at all, every handshake will keep being refused: check " +
			"the SRT return passphrase and key length on the Settings screen. Note that it is a " +
			"different passphrase from the contribution feed's. The exact reason is in the " +
			"application log"
	}

	text := redactPassphrase(reason.Error(), opts.Passphrase)
	upper := strings.ToUpper(text)
	quoted := " The reason, exactly as it arrived: " + text

	switch {
	case matchesAnySRTReject(upper, srtRejectBadSecret):
		return "libsrt refused the handshake with a BADSECRET rejection, which means the passphrase " +
			"offered does not match the one that M2L-X output is configured with. " + settings + quoted

	case matchesAnySRTReject(upper, srtRejectUnsecure):
		if opts.Passphrase == "" {
			return "libsrt refused the handshake with an UNSECURE rejection, which means that M2L-X output " +
				"requires encryption and this return offered none. Set the SRT return passphrase " +
				"and key length on the Settings screen. " + settings + quoted
		}
		return "libsrt refused the handshake with an UNSECURE rejection, which means the two ends disagree " +
			"about whether this session is encrypted: a passphrase was offered and that M2L-X " +
			"output is not encrypted at all. Set the SRT return key length to 0 and clear the " +
			"return passphrase. " + settings + quoted

	default:
		return "It is not a reason this application recognises, so it is reproduced rather than " +
			"summarised — search for it as it stands." + quoted
	}
}

// srtMinPassphraseLen is libsrt's own minimum for SRTO_PASSPHRASE. Anything
// shorter is rejected by srt_setsockflag with SRT_EINVPARAM and cannot form an
// encrypted session at all, whatever config.json says.
const srtMinPassphraseLen = 10

// redactPassphrase removes the SRT return passphrase from a string this process
// did not compose.
//
// It should never have anything to do. internal/gst sets the passphrase with
// g_object_set, never puts it in a URI, and documents on
// gst.ReturnOpts.OnConnectError that the reason cannot contain it; on
// everything measured this replaces nothing at all.
//
// It is done anyway because this is the one place in the application where a
// secret and a string from somebody else's library meet, and the result goes
// straight to emitError — which writes it to the log AND pushes it across the
// Wails boundary into the WebView. Whether the passphrase can appear in a
// support bundle should not depend on a third party's error formatting staying
// as it is.
//
// # Why the length test, which is not paranoia about nothing
//
// A blind ReplaceAll is worse than no redaction. Written that way and given the
// two-character passphrase a test happened to use, it turned
//
//	Error on SRT socket: Connection setup failure: ERROR:BADSECRET
//
// into "Error on SRT soc[…]et: …" — corrupting the one thing the verbatim
// branch exists to preserve, in the message an operator is meant to search for.
// A substring long enough to be a legal SRT passphrase is not going to occur by
// accident in a GStreamer error; a two-character one occurs constantly. So this
// only acts where a match means what it says, and a value too short to be a
// passphrase libsrt would accept is left alone — it cannot be encrypting
// anything, so there is nothing to protect.
func redactPassphrase(s, passphrase string) string {
	if len(passphrase) < srtMinPassphraseLen {
		return s
	}
	return strings.ReplaceAll(s, passphrase, "[return passphrase redacted]")
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
// # The message
//
// It is emitted at most once, on the returnDiagnoseAfter'th consecutive
// failure, and it is composed at that moment rather than when the session
// started, because half of it is the reason the last attempt failed and that is
// not known until it has.
//
// opts is the snapshot handed to the monitor; reason is filled in by the
// monitor's own goroutine through gst.ReturnOpts.OnConnectError. Reading it
// here is safe and correctly ordered: the callback runs immediately before the
// ReturnStateBackoff that this loop is receiving, so the reason for a failure
// is always stored before the failure is counted. See returnReason.
//
// Neither the message nor the event carries a secret. opts holds the
// passphrase; returnDiagnostic reads it only to redact it back out of libsrt's
// reason, and TestTheDiagnosticNeverCarriesThePassphrase asserts that end to
// end, over the log as well as the event.
func (a *App) forwardReturnStates(states <-chan gst.ReturnState, opts gst.ReturnOpts, reason *returnReason) {
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
			a.emitError(errors.New("wslcomms: " + returnDiagnostic(opts, reason.get())))
		}
	}
}
