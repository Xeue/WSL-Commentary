// Package main implements mockm2lx, a stand-in for the M2L-X instance so that
// the control plane, the reconnect state machine and the whole application can
// be exercised with no cloud instance and no libsrt.
//
// Owner: WP-7. No other work package writes files in this directory.
//
// This file holds the App type: the mutable state shared by the REST handlers
// (auth.go, kvs.go), the status WebSocket (status.go), the SRT listener
// (srt.go) and the fault-injection control endpoint (control.go). Every field
// that more than one of those touches is guarded by App.mu; the SRT listener's
// own state is a separate, more narrowly-scoped lock (srt.go) because the
// per-connection reader goroutine and the accept loop touch it far more often
// than anything else touches App.mu.
package main

import (
	"log"
	"sync"
	"time"
)

// Options are the command-line-configured defaults for a run. They seed the
// mutable fault-injection state in App; the /control endpoint (control.go)
// changes that state afterwards without needing a restart.
type Options struct {
	// Addr is the HTTP/WS listen address for the REST API, the status
	// WebSocket and the /control endpoint.
	Addr string

	// SRTAddr is the SRT listen address, host:port. GStreamer's srtsink dials
	// this as an SRT caller (spec section 5).
	SRTAddr string

	// Alias and Password are the only credentials POST
	// /api/local_auth/signin will accept. The real M2L-X instance uses a
	// dedicated read-only account (docs/archive-windows-app-spec-v1-rejected.md
	// line 863); this mock has no such distinction, it just has one account.
	Alias    string
	Password string

	// StatusKey is the switcher_status node name that carries our router
	// input's health — spec section 9's statusKey config field, e.g. "cam7".
	// Every other node in the pushed snapshot is decorative.
	StatusKey string

	// StatusInterval is how often the status WebSocket pushes a frame,
	// absent the stall fault.
	//
	// It is NOT a snapshot interval. The socket is snapshot-then-delta
	// (switcherdoc.go): frame 0 of a connection is the whole document and
	// every frame after it is a subtree delta, so this is the DELTA rate.
	// The default is 48 ms because the measured device pushed about 21
	// frames a second; a test that wants to watch the log can slow it down
	// without changing anything else about the protocol.
	StatusInterval time.Duration

	// TokenTTL is the lifetime handed out in expires_in on sign-in and
	// refresh. Defaults to m2lx.TokenLifetime, the measured 86399 s
	// (docs/architecture.md line 739), but a test run can shorten it with
	// -token-ttl to see the refresh path without waiting 12 hours for
	// RefreshFraction to elapse.
	TokenTTL time.Duration

	// SRTPassphrase is the passphrase this listener requires. Empty means no
	// passphrase is required. Mirrors spec open question 6: connecting with
	// and without a passphrase against the real instance is how
	// ERROR:BADSECRET and ERROR:UNSECURE are told apart; this mock reproduces
	// both.
	SRTPassphrase string

	// SRTPBKeylen is the AES key length required when SRTPassphrase is set:
	// 16, 24 or 32 (spec section 9's pbkeylen).
	SRTPBKeylen int

	// OnePeerOnly starts the SRT listener in the real listener's one-peer
	// mode: a second caller is refused and the incumbent is never displaced
	// (docs/test-results.md line 149). Off is not realistic; it exists so a
	// test can isolate other behaviour from this constraint.
	OnePeerOnly bool

	// RefusalWindow is how long the listener refuses a new caller after the
	// previous one disconnects, reproducing M2L-X's measured ~5 s re-accept
	// refusal window (docs/test-results.md line 149,
	// docs/architecture.md line 377). Default 6 s, deliberately a little
	// above the ~5 s measurement: internal/sender's first backoff rung is 7 s
	// specifically so it clears this with margin, and the mock's default
	// should be the harder case that ladder has to survive, not the easiest.
	RefusalWindow time.Duration

	// StallStatus starts the status WebSocket already stalled: connections
	// succeed but no snapshot is ever pushed, exercising m2lx.StaleAfter.
	StallStatus bool

	// LieStreamState, if non-empty, is reported as <statusKey>.stream_state
	// regardless of whether an SRT peer is actually connected. Must be one of
	// m2lx.StreamStateStreaming/Starting/Stopped.
	LieStreamState string

	// DropAudio starts the status document with an empty audio array on
	// every push, regardless of SRT state — the MP2/AC-3 silent-drop
	// signature (spec section 8, table row AUDIO OK).
	//
	// Note what it is NOT: a STOPPED input sends audio:[{"format":null}],
	// ONE element. The empty array is a different fault and the two must
	// stay distinguishable; see switcherdoc.go.
	DropAudio bool

	// DecoyDelta starts the mock sending, in place of its ordinary meter
	// traffic, a subtree delta that a parser ignoring "path" would read as
	// a whole node. One of decoyDeltaOff, decoyDeltaStatistics or
	// decoyDeltaStreamState.
	//
	// This is the bug that condemned a working input once a second. See
	// App.decoyFrame for what each mode sends and what a correct parser
	// must do with it.
	DecoyDelta string

	// TransitionPush selects how a change in the -status-key node's
	// stream_state or formats is published: transitionPushNode,
	// transitionPushDelta or transitionPushNone.
	//
	// It exists because nobody knows what the real device does. No
	// transition has ever been observed on this socket, and
	// m2lx.resyncInterval is an explicit backstop against that being
	// "nothing at all". transitionPushNone is how you find out whether that
	// backstop still earns its place; see switcherdoc.go.
	TransitionPush string

	// Verbose logs the opening snapshot and a once-a-second delta summary
	// in addition to the connection, fault and transition events that are
	// always logged.
	Verbose bool
}

// The DecoyDelta modes. See Options.DecoyDelta and App.decoyFrame.
const (
	// decoyDeltaOff sends the measured mix of meter and statistics deltas.
	decoyDeltaOff = "off"

	// decoyDeltaStatistics sends the measured trap frame — a "/statistics"
	// delta on the status-key node — at the full frame rate. A parser that
	// ignores "path" reads its state as a node, finds no stream_state, and
	// condemns the one input that is actually working.
	decoyDeltaStatistics = "statistics"

	// decoyDeltaStreamState sends a state that LOOKS like a whole node —
	// stopped, formats null — at a subtree path. A parser that ignores
	// "path" does not merely fail to find the node, it believes the decoy
	// and drives the lamps from it.
	decoyDeltaStreamState = "stream-state"
)

// The TransitionPush modes. See Options.TransitionPush and
// App.transitionFrames.
const (
	// transitionPushNode publishes a change as a whole-node entry at path
	// "/". The default: it is the shape a consumer can least ambiguously
	// act on, and the one this mock's own end-to-end test asserts against.
	transitionPushNode = "node"

	// transitionPushDelta publishes a change as "/streams" then
	// "/stream_state" subtree deltas, so the merge in
	// internal/m2lx/document.go is what makes the lamps move.
	transitionPushDelta = "delta"

	// transitionPushNone publishes nothing. The change is real and the
	// socket never mentions it, so only a reconnect reveals it — the
	// unproven worst case m2lx.resyncInterval is a backstop against.
	transitionPushNone = "none"
)

// App is the mock's whole runtime state.
type App struct {
	opts Options
	log  *log.Logger

	mu sync.RWMutex

	// Auth state. sessions is keyed by access token, refreshIndex by refresh
	// token; both entries of a pair point at the same *session so that
	// refreshing needs one map write on each side, not a linear scan.
	sessions     map[string]*session
	refreshIndex map[string]*session

	// Fault-injection state, settable at startup from Options and at runtime
	// from /control (control.go). Read under mu by status.go and srt.go.
	onePeerOnly    bool
	refusalWindow  time.Duration
	stallStatus    bool
	lieStreamState string
	dropAudio      bool
	decoyDelta     string
	transitionPush string

	// doc is the switcher_status document's own synthesis state: the delta
	// sequence, the meter sweep, the frozen statistics and the last lamp
	// reading published. It has a separate, narrower lock (switcherdoc.go)
	// because the broadcaster touches it on every frame — up to 21 times a
	// second — while App.mu guards auth and the fault flags, which change a
	// handful of times a match.
	doc *switcherDoc

	// srt is the SRT listener's own state. It has a separate, narrower lock
	// (see srt.go) because the accept loop and the per-connection reader
	// goroutine touch it far more often than anything touches App.mu, and
	// nothing in srt.go needs App.mu's auth or WS fields.
	srt *srtState

	// wsMu guards wsClients, the set of connected status-socket clients.
	wsMu      sync.Mutex
	wsClients map[*wsClient]struct{}
}

// session is one signed-in credential: an access token and the refresh token
// that can renew it.
type session struct {
	alias        string
	accessToken  string
	refreshToken string
	issuedAt     time.Time
	expiresAt    time.Time
}

// NewApp builds an App from opts. It does not start any network listener;
// call runHTTP and runSRTListener (or main's wiring) for that.
func NewApp(opts Options, logger *log.Logger) *App {
	return &App{
		opts:           opts,
		log:            logger,
		sessions:       make(map[string]*session),
		refreshIndex:   make(map[string]*session),
		onePeerOnly:    opts.OnePeerOnly,
		refusalWindow:  opts.RefusalWindow,
		stallStatus:    opts.StallStatus,
		lieStreamState: opts.LieStreamState,
		dropAudio:      opts.DropAudio,
		decoyDelta:     normaliseDecoyDelta(opts.DecoyDelta),
		transitionPush: normaliseTransitionPush(opts.TransitionPush),
		doc:            newSwitcherDoc(),
		srt:            newSRTState(),
		wsClients:      make(map[*wsClient]struct{}),
	}
}

// normaliseDecoyDelta maps the empty Options zero value onto the "off" mode,
// so an App built from a partially-filled Options — every test in this package
// does that — behaves as the flag defaults do rather than as an unknown mode.
func normaliseDecoyDelta(v string) string {
	if v == "" {
		return decoyDeltaOff
	}
	return v
}

// normaliseTransitionPush is normaliseDecoyDelta's counterpart. See there.
func normaliseTransitionPush(v string) string {
	if v == "" {
		return transitionPushNode
	}
	return v
}

// logf writes one human-readable, timestamped log line tagged with the
// subsystem it came from. This program is a tool someone watches while
// debugging a reconnect problem, so the log is the product, not an
// afterthought.
func (a *App) logf(tag, format string, args ...any) {
	a.log.Printf("[%s] "+format, append([]any{tag}, args...)...)
}

// --- fault-injection state accessors -------------------------------------
//
// Every fault flag is read far more often (every SRT accept, every status
// tick) than it is written (a handful of /control calls over a match), so
// these take App.mu.RLock for reads and a full Lock only for writes.

func (a *App) getOnePeerOnly() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.onePeerOnly
}

func (a *App) setOnePeerOnly(v bool) {
	a.mu.Lock()
	a.onePeerOnly = v
	a.mu.Unlock()
}

func (a *App) getRefusalWindow() time.Duration {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.refusalWindow
}

func (a *App) setRefusalWindow(d time.Duration) {
	a.mu.Lock()
	a.refusalWindow = d
	a.mu.Unlock()
}

func (a *App) getStallStatus() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.stallStatus
}

func (a *App) setStallStatus(v bool) {
	a.mu.Lock()
	a.stallStatus = v
	a.mu.Unlock()
}

func (a *App) getLieStreamState() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.lieStreamState
}

func (a *App) setLieStreamState(v string) {
	a.mu.Lock()
	a.lieStreamState = v
	a.mu.Unlock()
}

func (a *App) getDropAudio() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.dropAudio
}

func (a *App) setDropAudio(v bool) {
	a.mu.Lock()
	a.dropAudio = v
	a.mu.Unlock()
}

func (a *App) getDecoyDelta() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.decoyDelta
}

func (a *App) setDecoyDelta(v string) {
	a.mu.Lock()
	a.decoyDelta = normaliseDecoyDelta(v)
	a.mu.Unlock()
}

func (a *App) getTransitionPush() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.transitionPush
}

func (a *App) setTransitionPush(v string) {
	a.mu.Lock()
	a.transitionPush = normaliseTransitionPush(v)
	a.mu.Unlock()
}

// resetFaults returns every fault flag to the value it had at startup. It
// does not touch active sessions or the SRT connection.
func (a *App) resetFaults() {
	a.mu.Lock()
	a.onePeerOnly = a.opts.OnePeerOnly
	a.refusalWindow = a.opts.RefusalWindow
	a.stallStatus = a.opts.StallStatus
	a.lieStreamState = a.opts.LieStreamState
	a.dropAudio = a.opts.DropAudio
	a.decoyDelta = normaliseDecoyDelta(a.opts.DecoyDelta)
	a.transitionPush = normaliseTransitionPush(a.opts.TransitionPush)
	a.mu.Unlock()
}
