//go:build cgo && (dev || production || bindings)

// This file holds the Wails-bound object: the frontend's entire view of Go, and
// the wire-up that makes nine independently written packages one application.
//
// Owner: WP-8.
//
// It is behind //go:build cgo with main.go. main_nocgo.go supplies a main for
// CGO_ENABLED=0 builds so that `go build ./...` stays honest at Gate A, where
// there is no MinGW gcc.
//
// # The bound surface, which is frozen with the interfaces
//
//	ListInputDevices()        []gst.Device      caller: WP-5b
//	GetConfig()               *config.Config    caller: WP-5b
//	SaveConfig(c)             error             caller: WP-5b
//	SetSecret(key, value)     error             caller: WP-5b
//	Start() / Stop()          error             caller: WP-5b
//	GetKVSCredentials()       kvs.Credentials   caller: WP-5a
//
// Wails binds every EXPORTED method of *App. Those seven are therefore the only
// exported methods this type may ever have: adding an eighth silently widens the
// contract with WP-5a and WP-5b. Everything internal below is lower-case for
// that reason and not merely by habit. There is deliberately no getter for a
// secret — a secret goes into Credential Manager and never comes back out across
// this boundary.
//
// # Events emitted Go to JS
//
//	"status"  an m2lx.Status
//	"sender"  a sender.State
//	"error"   a string
//
// Headphone enumeration and selection are JavaScript-side only, through
// enumerateDevices and setSinkId. No Go package owns output devices.
//
// # Lifecycle: one context tree, one shutdown order
//
// Five things run concurrently in this process: the Wails event loop, the status
// WebSocket watcher, the m2lx client's token-refresh timer, the sender's two
// goroutines, and the event pump below. Every one of them is rooted in a single
// context created by NewApp, and shut down in exactly one order by teardown:
//
//  1. the sender  — Stop blocks until the pipeline is at NULL, so the WASAPI
//     endpoint and the SRT socket are released before anything else moves;
//  2. the status watcher — its context is cancelled and its goroutines joined;
//  3. the token refresh — the m2lx client's own background goroutine, stopped
//     through its Close method (see stopControlPlaneLocked for why that needs a
//     type assertion);
//  4. the root context, then a WaitGroup join of everything left.
//
// The whole sequence is bounded by shutdownTimeout. A process that will not exit
// is a support call, so a wedged pipeline loses the race rather than the window.
//
// # Three locks, in this order
//
//	sessMu -> cfgMu     (Start reads the config while holding the session lock)
//	ctlMu               (never held with either of the others)
//
// No goroutine started here takes any of the three, so the WaitGroup joins
// performed under ctlMu and sessMu cannot deadlock against their own workers.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
	"wslcomms/internal/kvs"
	"wslcomms/internal/m2lx"
	"wslcomms/internal/secrets"
	"wslcomms/internal/sender"
)

// Wails event names emitted from Go to the frontend. WP-5a and WP-5b subscribe
// to these, so the strings are part of the contract. They are mirrored verbatim
// in frontend/src/ui/backend.js as EVENT_STATUS / EVENT_SENDER / EVENT_ERROR.
const (
	// EventStatus carries an m2lx.Status: the debounced, staleness-checked
	// switcher_status snapshot behind the SWITCHER SEES FEED, VIDEO and AUDIO
	// lamps.
	EventStatus = "status"

	// EventSender carries a sender.State behind the SENDING lamp.
	EventSender = "sender"

	// EventError carries a human-readable error string for display.
	EventError = "error"
)

const (
	// eventQueueDepth is how many events the pump holds before it starts
	// discarding the oldest.
	//
	// It is sized against the two producers' measured worst cases. The status
	// watcher emits at most one Status per its one second tick
	// (internal/m2lx/watcher.go, tickInterval), which is the staleness-repeat
	// case; the sender emits three states per failed reconnect cycle and the
	// shortest cycle is bounded below by the seven second first rung of
	// sender.BackoffLadder. Sixty-four is therefore about a minute of the worst
	// combined rate, and can only fill if the WebView2 renderer has stopped
	// reading entirely — which is exactly the case the discarding is for.
	eventQueueDepth = 64

	// shutdownTimeout bounds the whole ordered teardown.
	//
	// The one wait teardown cannot shorten is a gst.Pipeline.ReplaceSink already
	// in flight: it is synchronous by contract and internal/gst bounds it at
	// sinkStateChangeTimeout, ten seconds. Add internal/gst's
	// elementShutdownTimeout of five seconds for taking the pipeline to NULL and
	// fifteen seconds is the sum, not a guess. Past it the process exits anyway
	// and lets the OS reclaim the audio endpoint.
	shutdownTimeout = 15 * time.Second

	// kvsFetchTimeout bounds one run of the M2L-X to Cognito credential chain.
	// It is three REST calls, each of which internal/m2lx already bounds at ten
	// seconds, plus one AWS call.
	kvsFetchTimeout = 45 * time.Second

	// signInTimeout bounds one sign-in attempt. internal/m2lx bounds its own
	// HTTP calls at ten seconds; this is the outer bound on the whole attempt,
	// including a DNS lookup for a host that does not resolve.
	signInTimeout = 30 * time.Second
)

// signInBackoff is the delay ladder between sign-in attempts, followed by
// signInBackoffCap repeated forever until the app shuts down.
//
// It mirrors internal/m2lx/watcher.go's reconnectBackoff, and for the reason
// stated there: unlike internal/sender's SRT ladder — whose first rung is
// measured against M2L-X's roughly five second re-accept refusal window — no
// measurement exists for how fast the control plane may be retried. A short,
// capped ladder reconnects promptly without hammering the instance. This is a
// design choice, not a measured constant.
var signInBackoff = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
}

// signInBackoffCap is the delay used once signInBackoff is exhausted.
const signInBackoffCap = 10 * time.Second

// errNotSending is returned by Stop when no session is running. teardown
// recognises it so that closing the window without ever having pressed START
// does not log a spurious failure.
var errNotSending = errors.New("wslcomms: not sending")

// errAlreadySending is returned by Start when a session is already running.
// Changing the input device mid-match is Stop then Start, by design: it is the
// one path that forces a pipeline rebuild (specification section 6.1).
var errAlreadySending = errors.New("wslcomms: already sending; press STOP first")

// App is the object bound to the frontend. One instance exists for the life of
// the process.
type App struct {
	// appDir is the directory holding wslcomms.exe, already symlink-resolved by
	// main. It is where the bundled GStreamer and the default slate.png live.
	appDir string

	// gstInitErr is the error from gst.Init, or nil.
	//
	// A failed Init is not fatal to the process, deliberately. The app is
	// launched from a desktop shortcut, so a message on stderr goes nowhere a
	// commentator will ever see it. Carrying the error here instead means the
	// window opens, the "error" event says exactly which plugin is missing from
	// the bundle, and ListInputDevices and Start refuse with the same message.
	// That is a diagnosable failure rather than a silent one.
	gstInitErr error

	// ctx is the Wails runtime context, captured by startup. Every
	// wailsruntime call needs it.
	ctx context.Context

	// rootCtx is the root of the app's one context tree; rootCancel ends it.
	// rootWG holds every goroutine whose lifetime is the whole process.
	rootCtx    context.Context
	rootCancel context.CancelFunc
	rootWG     sync.WaitGroup

	// events decouples both event producers from the renderer.
	events *eventPump

	// store is Windows Credential Manager. It is stateless and needs no
	// shutdown.
	store secrets.Store

	// cfgMu guards cfg, which is the in-memory copy of config.json.
	cfgMu sync.Mutex
	cfg   *config.Config

	// ctlMu guards the control plane generation: the m2lx client, the status
	// watcher's cancel function, and the WaitGroup holding their goroutines.
	// A generation is torn down and rebuilt whenever the M2L-X coordinates
	// change, which is what makes first run work without restarting the app.
	ctlMu     sync.Mutex
	client    m2lx.Client
	ctlCancel context.CancelFunc
	ctlWG     *sync.WaitGroup

	// sessMu guards session and is held for the whole of Start and Stop, so a
	// Start cannot begin while a Stop is still taking the previous pipeline to
	// NULL. Two pipelines contending for one WASAPI endpoint is the failure
	// that would cause.
	sessMu  sync.Mutex
	session *session

	// shutdownOnce makes teardown idempotent: it is reachable both from Wails'
	// OnShutdown and from main's defer, and exactly one of them must do the
	// work.
	shutdownOnce sync.Once
}

// session is one running contribution session: the pipeline, the sender driving
// it, and the goroutine forwarding its states to the frontend.
type session struct {
	snd  sender.Sender
	pipe gst.Pipeline
	wg   sync.WaitGroup
}

// NewApp creates the bound application object.
//
// appDir is the directory holding the executable. gstInitErr is the result of
// gst.Init, which main calls before anything else touches GStreamer; see the
// field comment for why a failure there is carried rather than fatal.
func NewApp(appDir string, gstInitErr error) *App {
	ctx, cancel := context.WithCancel(context.Background())
	return &App{
		appDir:     appDir,
		gstInitErr: gstInitErr,
		rootCtx:    ctx,
		rootCancel: cancel,
		events:     newEventPump(),
		store:      secrets.New(),
	}
}

// startup is the Wails OnStartup callback. It captures the context that the
// runtime's event functions require, loads the configuration, and brings up the
// control plane.
//
// It must not block: Wails calls it on the main thread before the window is
// shown, so anything slow here is a window that does not appear. Reading
// config.json is a local file read; everything with a network in it — sign-in,
// the status socket — happens on goroutines owned by startControlPlane.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.events.start(a.rootCtx, ctx, &a.rootWG)

	cfg, err := config.Load()
	if err != nil {
		// A corrupt or unreadable config.json must not stop the app opening,
		// because the Settings screen is the only way to repair it.
		a.emitError(fmt.Errorf("configuration could not be read, starting from defaults: %w", err))
		cfg = config.Defaults()
	}
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	if a.gstInitErr != nil {
		a.emitError(a.gstInitErr)
	}

	a.startControlPlane()
}

// shutdown is the Wails OnShutdown callback, called when the window closes.
func (a *App) shutdown(_ context.Context) {
	a.teardown()
}

// teardown performs the ordered shutdown described in the file comment, bounded
// by shutdownTimeout. It is idempotent.
//
// The bound is the point. The ordered sequence is the correct one and normally
// completes in well under a second, but it contains one call — sender.Stop —
// that can be waiting on a synchronous SRT handshake inside internal/gst. If
// that ever wedges, the process must still exit: the window has already gone,
// and a wslcomms.exe left in Task Manager after a match is a support call.
func (a *App) teardown() {
	a.shutdownOnce.Do(func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			a.teardownOrdered()
		}()

		timer := time.NewTimer(shutdownTimeout)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			log.Printf("wslcomms: shutdown did not complete within %s; exiting anyway", shutdownTimeout)
		}
	})
}

// teardownOrdered is teardown's body: sender, watcher, token refresh, join.
func (a *App) teardownOrdered() {
	if err := a.Stop(); err != nil && !errors.Is(err, errNotSending) {
		log.Printf("wslcomms: stopping the sender during shutdown: %v", err)
	}

	a.ctlMu.Lock()
	a.stopControlPlaneLocked()
	a.ctlMu.Unlock()

	a.rootCancel()
	a.rootWG.Wait()
}

// ---------------------------------------------------------------------------
// The bound surface
// ---------------------------------------------------------------------------

// ListInputDevices returns the audio capture endpoints for the commentary input
// dropdown. Device.ID is the IMMDevice endpoint GUID and is what SaveConfig must
// be given; Device.Name is for display only.
func (a *App) ListInputDevices() ([]gst.Device, error) {
	if a.gstInitErr != nil {
		return nil, a.gstInitErr
	}
	return gst.ListInputDevices()
}

// GetConfig returns the current configuration for the Settings screen.
//
// It returns a copy. The frontend receives JSON either way, but handing out the
// pointer this process is running from would mean a future in-process caller
// could mutate the live configuration without going through SaveConfig.
func (a *App) GetConfig() (*config.Config, error) {
	return a.snapshotConfig(), nil
}

// SaveConfig persists the configuration. It does not restart a running session:
// changing the input device mid-match requires Stop then Start.
//
// It deliberately does not validate. On first run every field the operator has
// not reached yet is empty, and refusing to save a half-filled Settings screen
// would make the screen unusable. Validation happens in Start, which is the
// point at which the fields have to be right.
//
// Changing the M2L-X coordinates — host, alias or statusKey — does restart the
// control plane, because otherwise a first run would need the operator to save
// their settings and then restart the application before anything signed in.
func (a *App) SaveConfig(c *config.Config) error {
	if c == nil {
		return errors.New("wslcomms: SaveConfig: no configuration supplied")
	}
	if err := c.Save(); err != nil {
		return err
	}

	saved := *c
	a.cfgMu.Lock()
	previous := a.cfg
	a.cfg = &saved
	a.cfgMu.Unlock()

	if controlPlaneChanged(previous, &saved) {
		a.startControlPlane()
	}
	return nil
}

// SetSecret writes one of the two Credential Manager secrets from the Settings
// screen. key is secrets.KeyM2LX or secrets.KeySRT. There is deliberately no
// getter: a secret goes into Credential Manager and never comes back out across
// this boundary.
//
// Writing the M2L-X password restarts the control plane, for the same first-run
// reason as SaveConfig: the sign-in loop stops as soon as it finds no stored
// password, and this is what tells it to try again.
func (a *App) SetSecret(key, value string) error {
	if err := a.store.Set(key, value); err != nil {
		return err
	}
	if key == secrets.KeyM2LX {
		a.startControlPlane()
	}
	return nil
}

// Start begins the contribution session: it builds the pipeline and enters the
// reconnect state machine. Progress is reported on the "sender" event, not by
// this method, which returns as soon as the pipeline is playing.
//
// A connection failure is not an error from Start — it is a transition to
// sender.StateBackoff, because the sender retries indefinitely (specification
// section 6.2). What Start does return is a configuration that cannot work, and
// it names the field.
func (a *App) Start() error {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()

	if a.session != nil {
		return errAlreadySending
	}
	if a.gstInitErr != nil {
		return a.gstInitErr
	}

	cfg := a.snapshotConfig()
	if err := cfg.Validate(); err != nil {
		// config.Validate joins one message per bad field, so the operator sees
		// every problem at once instead of one edit-fail cycle at a time.
		return fmt.Errorf("wslcomms: cannot start, the configuration is incomplete: %w", err)
	}

	passphrase, err := a.srtPassphrase(cfg)
	if err != nil {
		return err
	}

	pipe, err := gst.New()
	if err != nil {
		return fmt.Errorf("wslcomms: creating the media pipeline: %w", err)
	}

	snd := sender.New(pipe)
	opts := sender.Opts{
		Pipeline: gst.PipelineOpts{
			SlatePath:     a.slatePath(cfg),
			AudioDeviceID: cfg.AudioDeviceID,
			// The bitrates are left at zero so that internal/gst applies its own
			// documented constants — 2000 kbps video, 128000 bps audio,
			// specification section 5. Codec, resolution and bitrate are
			// explicitly not exposed to the user (specification section 2), so
			// config.Config carries no field for them and nothing here should
			// invent one.
		},
		Sink: gst.SinkOpts{
			Host:       cfg.SRTHost,
			Port:       cfg.SRTPort,
			LatencyMs:  cfg.SRTLatencyMs,
			Passphrase: passphrase,
			PBKeyLen:   cfg.PBKeyLen,
		},
	}

	if err := snd.Start(opts); err != nil {
		// sender.Start leaves the pipeline it was given untouched on failure,
		// and a gst.Pipeline is single-use, so this one is stopped here rather
		// than leaked: Stop is what closes its Errors channel and releases the
		// capture device.
		if stopErr := pipe.Stop(); stopErr != nil {
			log.Printf("wslcomms: stopping the pipeline after a failed start: %v", stopErr)
		}
		return err
	}

	sess := &session{snd: snd, pipe: pipe}
	// The forwarder is started only after sender.Start has succeeded. On failure
	// sender.run never launches, so the states channel is never closed, and a
	// forwarder ranging over it would be a goroutine leaked per failed Start.
	// Nothing is lost by starting late: States is buffered thirty-two deep and
	// discards its oldest entry rather than blocking, so the first transitions
	// are still there microseconds later.
	sess.wg.Add(1)
	go func() {
		defer sess.wg.Done()
		a.forwardSenderStates(snd.States())
	}()
	a.session = sess

	return nil
}

// Stop ends the contribution session.
//
// It holds sessMu for its whole duration, including the blocking wait inside
// sender.Stop, so that a Start racing it cannot open a second pipeline on the
// same WASAPI endpoint. By the time it returns, the pipeline is at NULL,
// sender.StateStopped has been emitted and the forwarding goroutine has exited.
func (a *App) Stop() error {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()

	sess := a.session
	if sess == nil {
		return errNotSending
	}
	a.session = nil

	err := sess.snd.Stop()
	sess.wg.Wait()
	if err != nil {
		return fmt.Errorf("wslcomms: stopping the session: %w", err)
	}
	return nil
}

// GetKVSCredentials runs the M2L-X to Cognito chain and returns credentials for
// the monitor page. The page calls it again from scratch whenever the peer
// connection or the signalling socket dies; there is no refresh scheduler
// (specification section 7).
func (a *App) GetKVSCredentials() (kvs.Credentials, error) {
	a.ctlMu.Lock()
	client := a.client
	a.ctlMu.Unlock()

	if client == nil {
		return kvs.Credentials{}, errors.New(
			"wslcomms: cannot fetch monitor credentials: m2lxHost is not configured — set it on the Settings screen")
	}

	cfg := a.snapshotConfig()
	if cfg.EventID == "" {
		return kvs.Credentials{}, errors.New(
			"wslcomms: cannot fetch monitor credentials: eventId is not configured — set it on the Settings screen")
	}

	ctx, cancel := context.WithTimeout(a.rootCtx, kvsFetchTimeout)
	defer cancel()
	return kvs.Fetch(ctx, client, cfg.EventID)
}

// ---------------------------------------------------------------------------
// Configuration helpers
// ---------------------------------------------------------------------------

// snapshotConfig returns a copy of the current configuration, never nil.
func (a *App) snapshotConfig() *config.Config {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	if a.cfg == nil {
		return config.Defaults()
	}
	c := *a.cfg
	return &c
}

// controlPlaneChanged reports whether the fields the control plane is built from
// differ between two configurations. A nil previous counts as changed, which is
// what makes the first SaveConfig of a first run bring the control plane up.
//
// Only these three matter: m2lxHost and alias are what the client is built from
// and signs in with, and statusKey is the node the watcher subscribes to. The
// SRT and monitor fields are read afresh by Start and GetKVSCredentials.
func controlPlaneChanged(previous, next *config.Config) bool {
	if previous == nil {
		return true
	}
	return previous.M2LXHost != next.M2LXHost ||
		previous.Alias != next.Alias ||
		previous.StatusKey != next.StatusKey
}

// slatePath resolves cfg.SlatePath against the directory holding the
// executable.
//
// The documented default is the bare filename slate.png
// (config.DefaultSlateFilename), which the installer lays down beside
// wslcomms.exe (specification section 11). An absolute path is taken as given,
// so an operator can point the app at a different slate without moving it into
// Program Files.
func (a *App) slatePath(cfg *config.Config) string {
	p := cfg.SlatePath
	if p == "" {
		p = config.DefaultSlateFilename
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(a.appDir, p)
}

// srtPassphrase reads the SRT passphrase from Credential Manager and checks it
// against the configured pbkeylen.
//
// A missing passphrase is a normal first-run condition and means an unencrypted
// session — which is legitimate, because whether M2L-X's commentary input has a
// passphrase set is specification open question 6 and is unmeasured. What is not
// legitimate is a non-zero pbkeylen with no passphrase: that combination asks
// for an encrypted session with no key, and failing here with the Credential
// Manager target named is far more useful than an ERROR:UNSECURE from libsrt
// twenty minutes before kick-off.
//
// The returned value is a secret. It goes straight into gst.SinkOpts, which sets
// it with g_object_set rather than in the URI, and it must never reach a log
// line, an error string or the Wails boundary.
func (a *App) srtPassphrase(cfg *config.Config) (string, error) {
	passphrase, err := a.store.Get(secrets.KeySRT)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		if cfg.PBKeyLen != 0 {
			return "", fmt.Errorf(
				"wslcomms: cannot start: pbkeylen is %d, which asks for an encrypted SRT session, "+
					"but no passphrase is stored in Windows Credential Manager under %q — "+
					"enter it on the Settings screen, or set pbkeylen to 0 for an unencrypted session",
				cfg.PBKeyLen, secrets.TargetSRT)
		}
		return "", nil
	case err != nil:
		return "", fmt.Errorf("wslcomms: reading the SRT passphrase from %q: %w", secrets.TargetSRT, err)
	}
	return passphrase, nil
}

// ---------------------------------------------------------------------------
// The control plane: sign-in, token refresh, status watcher
// ---------------------------------------------------------------------------

// startControlPlane tears down any existing control plane generation and builds
// a fresh one from the current configuration.
//
// It is called once from startup and again from SaveConfig and SetSecret. Doing
// it on a configuration change rather than only at launch is what makes first
// run work: the very first startup has no m2lxHost to sign in to, and requiring
// a restart after the operator fills in the Settings screen would be a poor
// first five minutes with the application.
//
// It blocks for as long as the previous generation takes to unwind, which is
// bounded by context cancellation propagating into net/http and gorilla's
// dialler — well under a second. It is called from the Wails message handler
// goroutine, never from the main thread.
func (a *App) startControlPlane() {
	cfg := a.snapshotConfig()

	a.ctlMu.Lock()
	defer a.ctlMu.Unlock()

	a.stopControlPlaneLocked()

	if cfg.M2LXHost == "" {
		// First run. Not an error and not worth an "error" event: the Settings
		// screen exists precisely for this, and SaveConfig will call us back.
		return
	}

	ctx, cancel := context.WithCancel(a.rootCtx)
	client := m2lx.NewClient(cfg.M2LXHost)
	watcher := m2lx.NewWatcher(cfg.M2LXHost, client)
	wg := &sync.WaitGroup{}

	a.client = client
	a.ctlCancel = cancel
	a.ctlWG = wg

	wg.Add(1)
	go func() {
		defer wg.Done()
		a.signInLoop(ctx, client, cfg.Alias)
	}()

	// A blank statusKey cannot name a switcher_status node, so subscribing would
	// open a socket to M2L-X that could only ever report staleness. The three
	// WebSocket-derived lamps stay at their initial unknown state until the
	// operator supplies one, which is honest.
	if cfg.StatusKey != "" {
		statuses := watcher.Watch(ctx, cfg.StatusKey)
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.forwardStatus(statuses)
		}()
	}
}

// stopControlPlaneLocked unwinds the current control plane generation. ctlMu
// must be held.
//
// The order is: cancel the context, which ends the watcher and any sign-in
// attempt in flight; stop the client's token-refresh goroutine; then join.
//
// # Reported under CONTRACT.md rule 3
//
// m2lx.NewClient returns the m2lx.Client interface, and that interface has no
// Close. The concrete client owns a background token-refresh goroutine started
// by SignIn and bounded by a context of its own — deliberately not derived from
// any call's context, so that a short-lived sign-in context cannot kill hours of
// refreshes — and the only way to stop it is the unexported type's Close method.
// Without the type assertion below, every control plane generation would leak
// one goroutine holding a timer for half of the measured 86399 s token lifetime.
// The assertion works and is not a hack around a bug, but the interface should
// grow Close, or NewClient should return something that has it. WP-8 does not
// edit WP-2's interface; this is the report.
func (a *App) stopControlPlaneLocked() {
	if a.ctlCancel == nil {
		return
	}

	a.ctlCancel()

	if closer, ok := a.client.(interface{ Close() }); ok {
		closer.Close()
	} else {
		log.Printf("wslcomms: m2lx.Client has no Close method; the token refresh goroutine will run until the process exits")
	}

	a.ctlWG.Wait()

	a.ctlCancel = nil
	a.ctlWG = nil
	a.client = nil
}

// signInLoop signs in to M2L-X and returns as soon as it succeeds.
//
// It does not need to keep running afterwards: the m2lx client starts its own
// token-refresh goroutine on the first successful SignIn, refreshing at
// RefreshFraction (0.5) of the measured TokenLifetime (86399 s), and falls back
// to a full sign-in if a refresh fails. This loop's only job is to get the first
// token, retrying because M2L-X may simply not be up yet when a commentator
// opens the application.
//
// It stops without retrying if no password is stored. That is a first-run
// condition, not a failure, and retrying it every ten seconds forever would
// produce nothing but noise on the "error" event; SetSecret restarts the control
// plane, which restarts this loop.
func (a *App) signInLoop(ctx context.Context, client m2lx.Client, alias string) {
	password, err := a.store.Get(secrets.KeyM2LX)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		a.emitError(fmt.Errorf(
			"wslcomms: no M2L-X password is stored in Windows Credential Manager under %q — "+
				"enter it on the Settings screen", secrets.TargetM2LX))
		return
	case err != nil:
		a.emitError(fmt.Errorf("wslcomms: reading the M2L-X password from %q: %w", secrets.TargetM2LX, err))
		return
	}

	for attempt := 0; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, signInTimeout)
		err := client.SignIn(attemptCtx, alias, password)
		cancel()

		if err == nil {
			log.Printf("wslcomms: signed in to M2L-X as %q", alias)
			return
		}
		if ctx.Err() != nil {
			// Shutting down or reconfiguring; the failure is ours, not M2L-X's.
			return
		}

		// The error from internal/m2lx never contains the password or the token
		// — its doJSON is written so that neither can reach an error string —
		// so this is safe to show and to log.
		a.emitError(fmt.Errorf("wslcomms: M2L-X sign-in failed (attempt %d), retrying: %w", attempt+1, err))

		if !sleepCtx(ctx, signInDelay(attempt)) {
			return
		}
	}
}

// signInDelay returns the delay before sign-in attempt number attempt+1,
// counting the attempt that has just failed as attempt zero. It walks
// signInBackoff and then holds at signInBackoffCap forever.
func signInDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt < len(signInBackoff) {
		return signInBackoff[attempt]
	}
	return signInBackoffCap
}

// sleepCtx waits for d and reports whether the wait completed, returning false
// as soon as ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// forwardStatus republishes the watcher's normalised statuses on the "status"
// event.
//
// It ranges rather than selecting on a context: m2lx.Watcher closes the channel
// when its context is done, so the range ends on its own and there is exactly
// one place that decides when this goroutine finishes. The channel the watcher
// writes to is unbuffered, so this loop must never block — which is what the
// event pump is for.
func (a *App) forwardStatus(statuses <-chan m2lx.Status) {
	for status := range statuses {
		a.events.send(EventStatus, status)
	}
}

// forwardSenderStates republishes the sender's transitions on the "sender"
// event. sender.Stop closes the channel after emitting sender.StateStopped, so
// the range ends when the session does.
func (a *App) forwardSenderStates(states <-chan sender.State) {
	for state := range states {
		a.events.send(EventSender, state)
	}
}

// emitError publishes err on the "error" event and logs it.
//
// Callers must be sure err carries no secret. Nothing that reaches here does:
// internal/m2lx keeps the password and bearer token out of its error strings by
// construction, internal/secrets reports only target names, and srtPassphrase
// names the Credential Manager target rather than its contents.
func (a *App) emitError(err error) {
	if err == nil {
		return
	}
	log.Printf("wslcomms: %v", err)
	a.events.send(EventError, err.Error())
}

// ---------------------------------------------------------------------------
// The event pump
// ---------------------------------------------------------------------------

// pumpEvent is one queued Wails event.
type pumpEvent struct {
	name string
	data any
}

// eventPump carries events from their producers to the WebView2 renderer
// without ever letting the renderer stall a producer.
//
// Both producers are on paths that must not be blocked. The status watcher
// writes to an unbuffered channel and a stalled reader stops it noticing that
// the socket has gone stale; the sender's forwarder sits in front of a state
// machine that must keep reconnecting whatever the UI is doing. So send never
// blocks: if the queue is full it discards the oldest event and takes its place,
// and if it is still full it discards the new one. Both lamps are edge-triggered
// displays of a current value, so a consumer that has fallen behind loses
// intermediate transitions but always converges on the latest — which is the
// only one it can render anyway.
type eventPump struct {
	ch chan pumpEvent
}

// newEventPump creates a pump. It does not start the emitting goroutine; that
// needs the Wails context, which does not exist until startup.
func newEventPump() *eventPump {
	return &eventPump{ch: make(chan pumpEvent, eventQueueDepth)}
}

// start launches the emitting goroutine under wg.
//
// rootCtx ends it at shutdown; wailsCtx is what wailsruntime.EventsEmit needs
// and is also watched, because Wails cancels it as the window closes and
// emitting into a dead runtime is pointless.
func (p *eventPump) start(rootCtx, wailsCtx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-wailsCtx.Done():
				return
			case e := <-p.ch:
				wailsruntime.EventsEmit(wailsCtx, e.name, e.data)
			}
		}
	}()
}

// send queues an event, discarding rather than blocking. See the type comment.
//
// The three-step form is deliberate and is bounded. There are several producers,
// so the single-writer trick of looping until the send succeeds could in
// principle spin while other writers keep refilling the slot. This makes at most
// one attempt, one discard and one more attempt, and then gives up.
func (p *eventPump) send(name string, data any) {
	e := pumpEvent{name: name, data: data}

	select {
	case p.ch <- e:
		return
	default:
	}

	select {
	case <-p.ch:
	default:
	}

	select {
	case p.ch <- e:
	default:
	}
}
