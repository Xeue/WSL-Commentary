//go:build dev || production || bindings

// This file holds the Wails-bound object: the frontend's entire view of Go, and
// the wire-up that makes nine independently written packages one application.
//
// Owner: WP-8.
//
// It is behind //go:build dev || production || bindings with main.go — the tags
// the Wails CLI sets, which is what stops a stray `go build .` reaching the
// modal build-tag dialog Wails pops when they are absent. main_nocgo.go supplies
// an inert main for every other build. The constraint deliberately does NOT
// require cgo: Wails on Windows is pure Go, so this file compiles and its tests
// run at Gate A with CGO_ENABLED=0 against the internal/gst stub. Run them with
//
//	go test -tags dev . -count=1
//
// The hazard that requiring cgo used to prevent as a side effect — a production
// build silently backed by the stub — is prevented on purpose in
// internal/gst/gst_stub_guard.go.
//
// # The bound surface, which is frozen with the interfaces
//
//	ListInputDevices()          []gst.Device               caller: WP-5b
//	GetConfig()                 *config.Config             caller: WP-5b
//	SaveConfig(c)               error                      caller: WP-5b
//	SetSecret(key, value)       error                      caller: WP-5b
//	Start() / Stop()            error                      caller: WP-5b
//	GetKVSCredentials()         kvs.Credentials            caller: WP-5a
//	GetStatusKeyCandidates()    []m2lx.StatusKeyCandidate  caller: WP-5b
//
// Wails binds every EXPORTED method of *App, so this list and the set of
// exported methods are the same thing: adding one silently widens the contract
// with WP-5a and WP-5b. Everything internal below is lower-case for that reason
// and not merely by habit. There is deliberately no getter for a secret — a
// secret goes into Credential Manager and never comes back out across this
// boundary.
//
// GetStatusKeyCandidates is the EIGHTH, added after the surface was declared
// frozen, and it is called out here rather than slipped in: with no statusKey
// the three WebSocket-derived lamps can only say NO STATUS, and no M2L-X
// endpoint will name the node (specification open question 5). The alternative
// to this method was leaving the operator to guess, which is what they were
// doing. It is read-only and returns a suggestion the operator must confirm.
//
// # Events emitted Go to JS
//
//	"status"              an m2lx.Status
//	"sender"              a sender.State
//	"error"               a string
//	"statusKeyCandidates" a []m2lx.StatusKeyCandidate
//
// The "error" event carries first-run configuration problems, gst.Init failures,
// sign-in failures, and — rate-limited, because the sender retries forever — the
// reason each connection attempt failed. See connectErrorReporter.
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
//  0. the closing flag is raised, so that a bound method already in flight on a
//     Wails message-handler goroutine cannot build a new session or a new
//     control plane behind the teardown that has just walked past them;
//  1. the sender  — Stop blocks until the pipeline is at NULL, so the WASAPI
//     endpoint and the SRT socket are released before anything else moves;
//  2. the status watcher — its context is cancelled and its goroutines joined;
//  3. the token refresh — the m2lx client's own background goroutine, which is
//     bounded by a context of the client's own and so survives step 2; only
//     m2lx.Client.Close stops it (see stopControlPlaneLocked);
//  4. the root context, then a WaitGroup join of everything left.
//
// Step 0 is what makes the order deterministic rather than merely usual. Both
// races it closes are decided the same way whichever goroutine wins the lock:
// a Start that got sessMu first completes and is then stopped by step 1, and a
// Start that arrives after step 1 is refused. There is no interleaving in which
// a pipeline outlives teardown.
//
// The whole sequence is bounded by shutdownTimeout. A process that will not exit
// is a support call, so a wedged pipeline loses the race rather than the window.
//
// # Four locks, in this order
//
//	sessMu -> cfgMu     (Start reads the config while holding the session lock)
//	ctlMu               (never held with either of the others)
//	senderMu            (leaf; never held while any other lock is taken)
//
// No goroutine started here takes any of the four, so the WaitGroup joins
// performed under ctlMu and sessMu cannot deadlock against their own workers.
//
// connectErrorReporter has a fifth, but it belongs to one session's reporter
// rather than to the App: it is taken only by report, on the sender's own
// goroutine, and nothing else is ever taken while it is held.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/options"
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

	// EventStatusKeys carries []m2lx.StatusKeyCandidate: the switcher_status
	// nodes that started streaming while our feed was coming up, offered to the
	// Settings screen as suggestions for a statusKey the operator has not set.
	// It is emitted only while a discovery is running, and never causes anything
	// to be saved — see App.GetStatusKeyCandidates.
	EventStatusKeys = "statusKeyCandidates"
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

	// statusKeyDiscoveryWindow is how long a statusKey discovery watches
	// switcher_status after START before giving up.
	//
	// It is sized from the two measured delays it has to contain: the pipeline
	// reaches CONNECTED about 1.1 s after Start (specification section 4), and
	// M2L-X's own status socket pushes about once a second and debounces
	// nothing. Ninety seconds is far longer than that sum needs, deliberately:
	// the cost of watching too long is one more candidate to choose between,
	// and the cost of stopping too early is a discovery that finds nothing on a
	// day when the SRT listener took a few extra seconds to accept.
	statusKeyDiscoveryWindow = 90 * time.Second

	// kvsFetchTimeout bounds one run of the M2L-X to Cognito credential chain.
	// It is three REST calls, each of which internal/m2lx already bounds at ten
	// seconds, plus one AWS call.
	kvsFetchTimeout = 45 * time.Second

	// signInTimeout bounds one sign-in attempt. internal/m2lx bounds its own
	// HTTP calls at ten seconds; this is the outer bound on the whole attempt,
	// including a DNS lookup for a host that does not resolve.
	signInTimeout = 30 * time.Second

	// connectErrorRepeat is how long an unchanged connection failure is
	// suppressed for before the operator is told about it again.
	//
	// It is derived from the producer's rate. sender.BackoffCap is thirty
	// seconds and there is no attempt limit, so a fault that cannot clear itself
	// — the commentator's Dante endpoint unplugged, which errors wasapi2src and
	// makes every subsequent ReplaceSink fail immediately — calls
	// Opts.OnConnectError twice a minute for the rest of the match. Forwarding
	// every one of those would put forty identical lines on the screen in the
	// twenty minutes of a second half and bury everything else, which is the same
	// failure as saying nothing at all.
	//
	// Five minutes turns that forty into four: often enough that whatever the
	// operator is looking at is recent rather than historical, rare enough that a
	// genuinely new message is still visible next to it. It is a judgement, not a
	// measurement, and it is stated as one.
	connectErrorRepeat = 5 * time.Minute
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

// errShuttingDown is returned by Start when the window is already closing. See
// step 0 of the shutdown order in the file comment.
var errShuttingDown = errors.New("wslcomms: the application is shutting down")

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
	//
	// It is atomic because it is written on one goroutine and read on another:
	// Wails calls OnStartup on the main thread, and calls
	// OnSecondInstanceLaunch on the goroutine serving the single-instance named
	// pipe. Those are unordered with respect to each other — a second launch
	// during startup is unlikely but entirely possible — so a plain field would
	// be a data race, and one the race detector cannot catch here because it
	// needs cgo (Gate B).
	ctx atomic.Pointer[context.Context]

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
	watcher   m2lx.Watcher
	ctlCtx    context.Context
	ctlCancel context.CancelFunc
	ctlWG     *sync.WaitGroup

	// discovering is raised for the duration of one statusKey discovery, so a
	// second START during the window does not start a second one. It is an
	// atomic rather than a field under ctlMu because the goroutine lowers it as
	// it exits, and stopControlPlaneLocked joins that goroutine while holding
	// ctlMu — taking the same lock on the way out would deadlock the teardown.
	discovering atomic.Bool

	// discMu guards statusKeyCandidates, the most recent suggestions produced by
	// a discovery. It is a leaf lock, like senderMu: nothing is taken while it
	// is held.
	//
	// The candidates are deliberately NOT written into config.json by anything
	// here. See runStatusKeyDiscovery.
	discMu              sync.Mutex
	statusKeyCandidates []m2lx.StatusKeyCandidate

	// sessMu guards session and is held for the whole of Start and Stop, so a
	// Start cannot begin while a Stop is still taking the previous pipeline to
	// NULL. Two pipelines contending for one WASAPI endpoint is the failure
	// that would cause.
	sessMu  sync.Mutex
	session *session

	// senderMu guards lastSender, the most recent sender.State forwarded to the
	// frontend. It is a leaf lock: nothing is taken while it is held.
	//
	// It exists so that domReady can replay the current state to a page that has
	// just loaded. The sender emits only on transitions, so a session that has
	// been CONNECTED for an hour sends nothing; a page that reloaded mid-match
	// would otherwise show the SENDING lamp grey while the feed was up, which is
	// the one direction the status display must never be wrong in.
	senderMu   sync.Mutex
	lastSender sender.State

	// closing is raised by teardown before it stops anything, so that a bound
	// method already running on a Wails message-handler goroutine cannot build a
	// new session or control plane behind it. See step 0 of the shutdown order
	// in the file comment.
	closing atomic.Bool

	// shutdownOnce makes teardown idempotent: it is reachable both from Wails'
	// OnShutdown and from main's error path, and exactly one of them must do the
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
		lastSender: sender.StateStopped,
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
//
// It deliberately does NOT start the event pump. OnStartup runs before the
// frontend is loaded, so anything emitted here would be delivered to a page with
// no listeners and silently lost — and the events this function produces are
// exactly the ones a first run depends on: "no M2L-X password is stored",
// "configuration could not be read", "GStreamer could not be initialised". The
// pump is started by domReady instead, and every event raised in between waits
// in its queue. See eventPump.
func (a *App) startup(ctx context.Context) {
	a.setRuntimeContext(ctx)

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

// domReady is the Wails OnDomReady callback, called once the embedded page has
// loaded and its EventsOn subscriptions exist.
//
// This is where the event pump starts emitting. Everything startup queued —
// including the first-run "enter your password on the Settings screen" message —
// is delivered here, in order, to a page that is now listening.
//
// It also replays the current sender state, because the sender emits only on
// transitions: a page that reloaded during a live session would otherwise show
// the SENDING lamp grey while the feed was up. The status lamps need no such
// replay and deliberately get none — m2lx.Watcher re-emits on its own one second
// tick and declares staleness after m2lx.StaleAfter (15 s), so the three
// WebSocket-derived lamps repopulate themselves from live data within seconds.
// Replaying a cached Status would risk showing stale green, which specification
// section 8 forbids.
func (a *App) domReady(ctx context.Context) {
	a.events.start(a.rootCtx, ctx, &a.rootWG)

	a.senderMu.Lock()
	last := a.lastSender
	a.senderMu.Unlock()
	a.events.send(EventSender, last)
}

// secondInstanceLaunched is the Wails OnSecondInstanceLaunch callback. It runs
// in THIS, the first, process when somebody starts wslcomms again; the second
// process exits without ever creating a window.
//
// All it has to do is make the existing window findable, which is what the
// person who double-clicked the shortcut was actually asking for. It restores
// the window if it was minimised and brings it to the front. It must not touch
// the session: the feed this process is carrying is the one on air, and a second
// launch is not a request to interrupt it.
//
// The launch arguments are ignored. wslcomms takes none — it is started from a
// desktop shortcut with no parameters (specification section 1) — so there is
// nothing in them to act on.
func (a *App) secondInstanceLaunched(_ options.SecondInstanceData) {
	ctx, ok := a.runtimeContext()
	if !ok {
		// A second launch inside the window between wails.Run starting and
		// OnStartup capturing the runtime context. There is nothing to raise yet,
		// and every wailsruntime call would panic on a nil context.
		log.Printf("wslcomms: a second instance was launched before this one finished starting; ignoring it")
		return
	}
	wailsruntime.WindowUnminimise(ctx)
	wailsruntime.WindowShow(ctx)
}

// setRuntimeContext records the Wails runtime context. Called once, by startup.
func (a *App) setRuntimeContext(ctx context.Context) {
	a.ctx.Store(&ctx)
}

// runtimeContext returns the Wails runtime context and whether startup has
// captured it yet. Callers on any goroutine but the main one must check ok:
// passing a nil context to a wailsruntime function panics.
func (a *App) runtimeContext() (context.Context, bool) {
	p := a.ctx.Load()
	if p == nil {
		return nil, false
	}
	return *p, true
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

// teardownOrdered is teardown's body: raise the closing flag, then sender,
// watcher, token refresh, join.
//
// The flag is raised before anything is stopped, which is what makes the order
// hold against a bound method that is already running. See step 0 of the
// shutdown order in the file comment for why both interleavings are safe.
func (a *App) teardownOrdered() {
	a.closing.Store(true)

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
// it names the field. The reason a connection attempt failed reaches the
// operator on the "error" event instead, rate-limited; see senderOpts and
// connectErrorReporter.
//
// Start is a thin wrapper around startSession for two reasons. The statusKey
// discovery it may kick off takes ctlMu, and the lock order in this file's
// header says ctlMu is never held with either of the others — taking it under
// sessMu would be the first exception to that and there is no reason to make
// one. And the discovery has to be armed BEFORE the pipeline: its baseline is
// the first switcher_status frame it sees, and a baseline taken after our feed
// had already reached the switcher would show our own node streaming and never
// report the transition that identifies it.
func (a *App) Start() error {
	stopDiscovery := a.maybeDiscoverStatusKey()
	if err := a.startSession(); err != nil {
		// Nothing is going to come up, so there is nothing to discover. Leaving
		// it running would spend the whole window watching for a change that
		// cannot happen and then report a false "nothing matched".
		stopDiscovery()
		return err
	}
	return nil
}

// startSession is Start's body: everything that happens under sessMu.
func (a *App) startSession() error {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()

	if a.closing.Load() {
		// The window is going away. Building a pipeline now would open the WASAPI
		// endpoint and an SRT socket that teardown has already walked past, and
		// the process would exit still holding them.
		return errShuttingDown
	}
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
	opts := a.senderOpts(cfg, passphrase)

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

	// kvs.Fetch does not sign in on its own — it requires a client that already
	// holds a bearer token, and both of its M2L-X calls would return
	// m2lx.ErrNotSignedIn. Saying so here names the actual problem: the monitor
	// page retries this call whenever its peer connection dies, so during a
	// sign-in outage this is the message the operator would see repeatedly, and
	// it should point at the password rather than at the KVS chain.
	if client.Token() == "" {
		return kvs.Credentials{}, fmt.Errorf(
			"wslcomms: cannot fetch monitor credentials: not signed in to M2L-X at %q yet — "+
				"check the alias and the password stored in Windows Credential Manager under %q",
			cfg.M2LXHost, secrets.TargetM2LX)
	}

	ctx, cancel := context.WithTimeout(a.rootCtx, kvsFetchTimeout)
	defer cancel()
	return kvs.Fetch(ctx, client, cfg.EventID)
}

// GetStatusKeyCandidates returns the switcher_status nodes that were seen to
// start streaming while the last discovery was running: suggestions for a
// statusKey the operator has not set.
//
// It returns an empty slice, never nil, when there is nothing to suggest — the
// frontend renders a list and should not have to special-case null.
//
// These are SUGGESTIONS and this application never acts on one by itself.
// Nothing distinguishes our router input coming up from another operator's
// input coming up in the same second, seen from outside; persisting a guess
// would point three green lamps at somebody else's feed, which reads as
// confirmation and is the worst thing this application could get wrong. The
// operator confirms one on the Settings screen, which is an ordinary SaveConfig.
func (a *App) GetStatusKeyCandidates() ([]m2lx.StatusKeyCandidate, error) {
	a.discMu.Lock()
	defer a.discMu.Unlock()
	out := make([]m2lx.StatusKeyCandidate, len(a.statusKeyCandidates))
	copy(out, a.statusKeyCandidates)
	return out, nil
}

// ---------------------------------------------------------------------------
// The contribution session
// ---------------------------------------------------------------------------

// senderOpts builds the options for one session from a configuration snapshot
// and the passphrase already read from Credential Manager.
//
// It is separate from Start so that what the sender is actually given can be
// asserted without running a session — including the one field with behaviour
// attached to it, OnConnectError.
//
// passphrase is a secret. It goes into gst.SinkOpts, which internal/gst sets
// with g_object_set rather than in the URI, and must not be logged or returned
// across the Wails boundary.
func (a *App) senderOpts(cfg *config.Config, passphrase string) sender.Opts {
	// EffectiveSRTHost, not SRTHost: an empty srtHost means "the same host as
	// M2L-X" (config.Config's field comment). The reporter is given the same
	// resolved host, so the operator is told the name that was actually dialled
	// rather than a blank.
	srtHost := cfg.EffectiveSRTHost()
	reporter := newConnectErrorReporter(a.emitError, srtHost, cfg.SRTPort, time.Now)

	return sender.Opts{
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
			Host:       srtHost,
			Port:       cfg.SRTPort,
			LatencyMs:  cfg.SRTLatencyMs,
			Passphrase: passphrase,
			PBKeyLen:   cfg.PBKeyLen,
		},
		// A fresh reporter per session, so that its memory of what it has
		// already said dies with the session it said it about. An operator who
		// presses STOP and START has told us they want to be told again.
		OnConnectError: reporter.report,
	}
}

// connectErrorReporter forwards the sender's connection failures to the
// frontend's "error" event, rate-limited.
//
// # What it is for
//
// The sender retries forever by design, and without this the reason each
// attempt failed is discarded. That is fine for a peer that has gone away for
// ten seconds and comes back. It is not fine for a fault that cannot clear
// itself:
// unplug the commentator's Dante endpoint and wasapi2src errors, every
// subsequent gst.Pipeline.ReplaceSink fails immediately, and the operator has an
// amber SENDING lamp and nothing else for the rest of the match. The lamp says
// "connecting" when the truthful answer is "your audio device is gone".
//
// # Why it is rate-limited, and how
//
// The producer is unbounded: sender.BackoffCap is thirty seconds and there is no
// attempt limit, so a permanent fault reports twice a minute forever. A wall of
// identical lines is as useless as silence, so:
//
//   - a failure whose message differs from the last one shown is shown at once,
//     because a changed reason is new information — the endpoint coming back and
//     the connection then being refused is a different fault from the endpoint
//     being missing;
//   - a repeat of the same message is suppressed until connectErrorRepeat has
//     passed, and the repeat then says how many attempts were suppressed, so the
//     operator can tell "still broken" from "broken again".
//
// Comparison is on the error's message, not on error identity: the sender
// produces a freshly wrapped error every attempt, so errors.Is would report a
// new fault every thirty seconds and defeat the whole thing.
//
// # Concurrency
//
// report is called on the sender's state-machine goroutine, and must not block
// it: emit is App.emitError, which logs and queues on the event pump, and the
// pump discards rather than blocking. The mutex is held only around the
// bookkeeping and never across emit, so the state machine cannot be stalled
// behind a log write either.
type connectErrorReporter struct {
	// emit publishes one error to the operator. It is App.emitError in the
	// application and a recorder in the tests.
	emit func(error)

	// host and port name the SRT endpoint being dialled, so that the message
	// says where the feed was going. The reason from the sender says what
	// actually failed, which may well be local rather than the network.
	host string
	port int

	// now is time.Now, injected so that the tests can cross connectErrorRepeat
	// without waiting five minutes.
	now func() time.Time

	mu sync.Mutex
	// last is the message of the failure most recently shown, empty before the
	// first one.
	last string
	// lastAt is when it was shown.
	lastAt time.Time
	// suppressed counts identical failures swallowed since lastAt.
	suppressed int
}

// newConnectErrorReporter returns a reporter that publishes through emit, naming
// host and port as the endpoint the feed was going to. now supplies the clock; a
// nil now means time.Now, which is what the application passes.
func newConnectErrorReporter(emit func(error), host string, port int, now func() time.Time) *connectErrorReporter {
	if now == nil {
		now = time.Now
	}
	return &connectErrorReporter{emit: emit, host: host, port: port, now: now}
}

// report is the sender.Opts.OnConnectError callback. It decides whether this
// failure is worth the operator's attention now, and publishes it if so.
//
// A nil error is ignored rather than announced: it would carry no reason, which
// is the one thing this exists to deliver.
func (r *connectErrorReporter) report(err error) {
	if err == nil {
		return
	}

	msg := err.Error()
	if msg == "" {
		// An error whose message is empty would compare equal to the "nothing
		// has been shown yet" sentinel and be suppressed forever, and would
		// render as a message that trails off. Nothing in internal/gst produces
		// one, but this is a callback on a public contract rather than a promise
		// between two files, so it is handled rather than assumed away.
		err = errors.New("an unspecified connection failure")
		msg = err.Error()
	}

	r.mu.Lock()
	at := r.now()
	changed := msg != r.last
	due := !r.lastAt.IsZero() && at.Sub(r.lastAt) >= connectErrorRepeat
	suppressed := r.suppressed

	switch {
	case changed || due:
		r.last = msg
		r.lastAt = at
		r.suppressed = 0
	default:
		r.suppressed++
	}
	r.mu.Unlock()

	if !changed && !due {
		return
	}

	if changed {
		r.emit(fmt.Errorf(
			"wslcomms: the commentary feed to %s:%d is not connected and is retrying: %w",
			r.host, r.port, err))
		return
	}
	r.emit(fmt.Errorf(
		"wslcomms: the commentary feed to %s:%d is still not connected after %d further attempts: %w",
		r.host, r.port, suppressed, err))
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

	if a.closing.Load() {
		// Shutting down. Whichever of this and teardown took ctlMu first, the
		// outcome is the same: the previous generation has just been unwound
		// above, and no new one is built.
		return
	}

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
	// The watcher and this generation's context are kept so that a statusKey
	// discovery started later — at START, which is the only moment the node we
	// are looking for changes state — reuses this generation's socket
	// machinery and dies with it.
	a.watcher = watcher
	a.ctlCtx = ctx
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

// ---------------------------------------------------------------------------
// statusKey discovery
// ---------------------------------------------------------------------------

// maybeDiscoverStatusKey starts a statusKey discovery if one is wanted and
// possible: the operator has not set a statusKey, a control plane exists to
// watch through, and no discovery is already running. It returns a function
// that ends the discovery early, which is a no-op when none was started.
//
// It is called from Start, and only from Start, because START is the one moment
// at which the node being looked for changes state. Specification open question
// 5: "read one switcher_status snapshot and find the node whose stream_state
// changes when the app starts."
//
// Doing nothing is the normal outcome and is never an error: a configured
// statusKey means there is nothing to discover, and no M2L-X host means there is
// nothing to discover it from.
func (a *App) maybeDiscoverStatusKey() (stop func()) {
	noop := func() {}

	if a.snapshotConfig().StatusKey != "" {
		return noop
	}

	a.ctlMu.Lock()
	defer a.ctlMu.Unlock()

	if a.watcher == nil || a.ctlCtx == nil || a.ctlWG == nil || a.closing.Load() {
		return noop
	}
	if !a.discovering.CompareAndSwap(false, true) {
		return noop
	}

	watcher := a.watcher
	ctx, cancel := context.WithTimeout(a.ctlCtx, statusKeyDiscoveryWindow)
	wg := a.ctlWG
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		defer a.discovering.Store(false)
		a.runStatusKeyDiscovery(ctx, watcher)
	}()
	return cancel
}

// runStatusKeyDiscovery watches every switcher_status node for
// statusKeyDiscoveryWindow and publishes the ones that start streaming.
//
// The first frame is the baseline — taken now, as the pipeline is being built,
// which is why this is started from Start and not from startControlPlane. A
// node already streaming in that baseline is somebody else's and never becomes
// a candidate.
//
// Nothing here writes config.json. The candidates go to the frontend, which
// shows them as suggestions with what they matched on; confirming one is an
// ordinary SaveConfig by the operator. See GetStatusKeyCandidates for why that
// distinction is not a nicety.
func (a *App) runStatusKeyDiscovery(ctx context.Context, watcher m2lx.Watcher) {
	disc := m2lx.NewDiscovery()

	// Every discovery starts from nothing rather than adding to the last one:
	// two STARTs an hour apart are two different pieces of evidence, and merging
	// them would produce a list in which the older half is untestable.
	a.setStatusKeyCandidates(nil)

	for doc := range watcher.WatchAll(ctx) {
		if !disc.Observe(doc) {
			continue
		}
		candidates := disc.Candidates()
		a.setStatusKeyCandidates(candidates)
		a.events.send(EventStatusKeys, candidates)
	}

	// DeadlineExceeded, not merely Done: a discovery ended early because the
	// session failed to start, or because the control plane was reconfigured
	// underneath it, has not observed anything about M2L-X and must not report
	// as though it had.
	if !disc.Started() && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// No frame arrived at all in the whole window. That is a different
		// failure from "watched and matched nothing" — the socket never
		// delivered — and saying so stops the operator hunting for a node name
		// when the problem is the connection.
		a.emitError(fmt.Errorf(
			"wslcomms: could not suggest a statusKey: no switcher_status message arrived from %s in %s — "+
				"the status WebSocket is not delivering, so the three status lamps cannot work whatever statusKey is set",
			a.snapshotConfig().M2LXHost, statusKeyDiscoveryWindow))
	}
}

// setStatusKeyCandidates replaces the published suggestions.
func (a *App) setStatusKeyCandidates(candidates []m2lx.StatusKeyCandidate) {
	a.discMu.Lock()
	defer a.discMu.Unlock()
	a.statusKeyCandidates = candidates
}

// stopControlPlaneLocked unwinds the current control plane generation. ctlMu
// must be held.
//
// The order is: cancel the context, which ends the watcher and any sign-in
// attempt in flight; stop the client's token-refresh goroutine; then join.
//
// # Why Close is not optional
//
// The client owns a background token-refresh goroutine, started by SignIn and
// bounded by a context of its own — deliberately not derived from any call's
// context, so that a short-lived sign-in context cannot kill hours of refreshes.
// Cancelling ctx above therefore does not stop it. Without the Close call below,
// every control plane generation would leak one goroutine holding a timer for
// half of the measured 86399 s token lifetime, and startControlPlane runs again
// on every Settings change.
//
// This used to be an interface{ Close() } type assertion, because m2lx.Client
// had no Close and WP-8 does not edit WP-2's interface. It was reported under
// CONTRACT.md rule 3 and the coordinator has since added Close() error to the
// interface, so the assertion — and the silent leak if the concrete type had
// ever changed — is gone.
func (a *App) stopControlPlaneLocked() {
	if a.ctlCancel == nil {
		return
	}

	a.ctlCancel()

	// a.client is set with a.ctlCancel and cleared with it, so it is non-nil
	// here; the check is what makes that assumption visible rather than a nil
	// interface panic during shutdown if it ever stops holding.
	if a.client != nil {
		if err := a.client.Close(); err != nil {
			// Nothing can be done about it at this point — the generation is
			// going away regardless — but a client that will not close is how a
			// refresh goroutine survives into the next generation, and that is
			// worth a line in the log.
			log.Printf("wslcomms: closing the M2L-X client: %v", err)
		}
	}

	a.ctlWG.Wait()

	a.ctlCancel = nil
	a.ctlWG = nil
	a.client = nil
	a.watcher = nil
	a.ctlCtx = nil
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
//
// Each state is also recorded as the current one, so that domReady can replay it
// to a page that has just loaded. The record is taken before the send, so a
// reload racing a transition sees the newer value rather than the older.
func (a *App) forwardSenderStates(states <-chan sender.State) {
	for state := range states {
		a.senderMu.Lock()
		a.lastSender = state
		a.senderMu.Unlock()

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
// Until start is called the queue simply fills, which is what lets startup raise
// first-run errors before the page that must display them exists.
type eventPump struct {
	ch chan pumpEvent

	// startOnce keeps there being exactly one consumer of ch. OnDomReady fires
	// again after a page reload — routine under `wails dev`, and reachable in
	// production through the WebView2 keyboard shortcuts — and two consumers
	// would split the queue between them, so each event would reach the page
	// once but the ordering between them would no longer hold.
	startOnce sync.Once
}

// newEventPump creates a pump. It does not start the emitting goroutine; that
// needs the Wails context and a frontend that is listening, neither of which
// exists until domReady.
func newEventPump() *eventPump {
	return &eventPump{ch: make(chan pumpEvent, eventQueueDepth)}
}

// start launches the emitting goroutine under wg, at most once.
//
// rootCtx ends it at shutdown; wailsCtx is what wailsruntime.EventsEmit needs
// and is also watched, because Wails cancels it as the window closes and
// emitting into a dead runtime is pointless.
func (p *eventPump) start(rootCtx, wailsCtx context.Context, wg *sync.WaitGroup) {
	p.startOnce.Do(func() {
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
	})
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
