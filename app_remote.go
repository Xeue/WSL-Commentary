//go:build dev || production || bindings

// The App side of the LAN control bridge: the policy that decides what a remote
// seat may do, and the lifecycle glue that stands the listener up and tears it
// down.
//
// Owner: WP-8. Pairs with internal/remote (WP-REMOTE), which owns the transport
// and knows nothing about *App. The seam between them is remote.Dispatcher; this
// file implements it and app.go wires the resulting remote.Server into the
// application lifecycle.
//
// ===================== WHY AN ALLOWLIST, HAND-WRITTEN =======================
//
// main.go binds []interface{}{app}, so Wails binds EVERY exported method of
// *App automatically — that is the whole reason app.go keeps helpers lower-case.
// The network is a different threat model from a local WebView2 renderer, and
// "is this method safe to expose to a browser on the LAN" must be answered by a
// table somebody has to edit, not by reflection that would silently expose the
// next method somebody binds. So the dispatch here is a hand-written switch over
// an explicit table (remoteAllowlist), NOT a reflective loop. A method absent
// from the table is refused as unknown even though *App implements it, and a
// drift-guard test (app_remote_test.go) fails by NAME if any exported *App
// method is missing from the table — so a new binding cannot default into being
// remotely callable.
//
// ===================== THE TWO GATES, IN ORDER =============================
//
//  1. ALLOWLIST   unknown method            -> refused ("unknown method")
//  2. HOST-ONLY   the six native-surface    -> refused for EVERY connection,
//     methods + the two remote      and absent from the hello
//     admin methods                 methods list (degrade by
//     OMISSION, not by a refusal the
//     frontend must be taught)
//
// There is NO capability gate any more. By the owner's explicit, repeated, final
// decision the listener is UNAUTHENTICATED — it runs on a dedicated private
// facility network, and the network is the access control (see
// docs/remote-access.md). There are no client accounts and no tiers, so every
// connection that is not host-only gets FULL access to every allowlisted method.
// The allowlist and the host-only set remain — they are what keep the reachable
// surface a deliberate, reviewable, drift-guarded decision rather than whatever
// Wails happens to bind — but the per-tier filtering is gone.
//
// Once the two gates pass, a method's arguments are json.Unmarshalled into their
// real Go types and the real bound method is called. Keeping the args opaque
// until here is what let internal/remote stay App-agnostic: it never needed to
// know a method's parameter types.
//
// ===================== HOST-ONLY, AND WHY IT IS PHYSICS =====================
//
// The SRT picture is a native child HWND painted by d3d11videosink on the host
// GPU, outside the DOM (internal/gst/overlay_windows.go). No transport that
// carries the DOM can carry it, and SetPictureRect takes the CALLING page's CSS
// rect and devicePixelRatio — a remote browser at another size or DPI would drag
// the operator's own picture around from its ResizeObserver. So the six picture
// and SRT-return methods are HostOnly: refused for every remote connection. The
// remote page gets the WebRTC mosaic and an honest message; it never gets these
// methods, because the hello frame omits them.
//
// The two remote-admin methods (GetRemoteState, SetRemoteListener) are HostOnly
// for a blunter reason: they change WHETHER the listener runs and on WHAT
// address and ports. A listener that could be reconfigured by one of its own
// remote connections could be turned off — or moved — by whoever first gets in.
// Those methods exist for the local operator's Settings screen only. (The
// per-client admin methods are gone entirely: there are no clients.)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"sort"
	"strings"
	"time"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
	"wslcomms/internal/mixer"
	"wslcomms/internal/remote"
)

// EventConfig carries the saved configuration and the id of the client that
// saved it, emitted from SaveConfig AFTER the write.
//
// It exists because config is written as a WHOLE OBJECT from a per-page cache
// (app.js persistConfig) and App.SaveConfig applies it verbatim with no
// field-level merge. With two controllers, one page's save would clobber the
// other's edits indefinitely; this event shrinks that window to one round trip.
// The originating client id lets a page ignore the echo of its OWN save (which
// would otherwise yank a remote operator out of the Settings screen mid-edit)
// and refresh only on somebody else's.
const EventConfig = "config"

// EventRemote carries the list of currently connected remote seats, so the
// operator at the desk can SEE that someone else has a seat — without it, a
// remote operator pressing STOP is indistinguishable from a crash. Driven by a
// poll of remote.Server.Clients (the transport exposes no connect/disconnect
// hook, deliberately: a poll that compares the set is enough for an indicator
// and adds no coupling), emitted only when the set changes.
const EventRemote = "remote"

// localClientID is the fixed client id of the local WebView2 seat. The local
// window does not go through the remote dispatcher at all — it calls the bound
// methods directly over Wails — so it needs an id of its own for the one place
// that asks WHO the caller was: mixer arm-ownership. It must not collide with a
// remote connection id, which internal/remote mints as base64url of 12 random
// bytes; a human-readable literal cannot.
const localClientID = "local-webview2"

// remoteEventNames is the set of event names advertised in the hello frame, so a
// remote client can wire its subscriptions without guessing. It is every event
// this application emits Go->JS; the transport does not care which of them a
// given client actually uses.
func remoteEventNames() []string {
	return []string{
		EventStatus,
		EventSender,
		EventReturn,
		EventPicture,
		EventError,
		EventStatusKeys,
		EventLevels,
		EventChannelLevels,
		EventChannelMap,
		EventSignal,
		EventConfig,
		EventRemote,
	}
}

// ---------------------------------------------------------------------------
// The allowlist table
// ---------------------------------------------------------------------------

// methodPolicy is one row of the allowlist: whether the method is host-only
// (refused for every remote connection) and whether a remote invocation of it
// should be written to the audit log because it changes state on a machine that
// is on air. There is no capability field: the listener is unauthenticated, so
// a non-host-only method is reachable by every connection.
type methodPolicy struct {
	// hostOnly refuses this method for EVERY remote connection, and omits it from
	// the hello methods list.
	hostOnly bool

	// mutating marks a method whose remote invocation is audit-logged with the
	// caller's source address. SetSecret is additionally logged with its KEY
	// (never its value) in the dispatch switch.
	mutating bool
}

// remoteAllowlist is THE authoritative map from a bound method name to its
// remote policy. Every exported method of *App must appear here — the drift
// guard in app_remote_test.go fails by name if one does not — so that adding a
// binding forces a deliberate classification rather than defaulting it open.
//
// With no capability tiers, the classification is binary: a method is either
// HOST-ONLY (refused for every connection, omitted from the hello list) or
// reachable by every connection. The mutating flag is not a gate — it only
// decides whether an invocation is audit-logged. The grouping below is kept for
// legibility (reads, then state-changing calls, then the arm-gated write, then
// host-only) but carries no access meaning any more.
var remoteAllowlist = map[string]methodPolicy{
	// ---- reads and the always-safe DisarmMixer ----
	//
	// CredentialStoreName is a read too, and reachable rather than host-only on
	// purpose. A remote seat draws the same delete-a-preset dialog the local one
	// does, and refusing the method there would not hide the dialog — it would
	// drop backend.js to its fallback string, which is the WINDOWS name. On a
	// Mac host that sends a remote operator hunting for a control panel neither
	// machine has, which is the exact fault the method was added to fix. It
	// returns what the vault is CALLED and never anything in it, so the "no
	// secret crosses this boundary outbound" rule is untouched.
	//
	// GetConformTarget is a read and is reachable for the same shape of reason.
	// A remote seat draws the same status row, and that row's VIDEO lamp is only
	// as honest as the raster it judges against; refusing the method would drop
	// that seat to lamps.js's 1080p50 fallback, so a remote operator watching a
	// correctly conforming 720p50 feed would see a red lamp the operator at the
	// desk does not. Two seats disagreeing about a lamp is worse than either
	// answer alone. It returns a raster, a rate and where they came from.
	"GetConfig":              {},
	"GetConformTarget":       {},
	"GetKVSCredentials":      {},
	"GetStatusKeyCandidates": {},
	"ListEvents":             {},
	"CredentialStoreName":    {},
	"GetMixerSnapshot":       {},
	"GetMixerGolden":         {},
	"GetPictureState":        {},
	"GetReturnState":         {},
	// GetChannelMap is a read and is reachable, for the same shape of reason
	// GetConformTarget is: it returns what the capture pad negotiated and the
	// routing in force, and a remote seat that could not ask would draw the
	// routing grid with no channels in it — telling an operator who is looking
	// for a commentator they cannot hear that there is nothing there to route.
	"GetChannelMap":             {},
	"IsSRTReturnSelected":       {},
	"ListInputDevices":          {},
	"ListOutputDevices":         {},
	"ListPresets":               {},
	"GetActivePreset":           {},
	"GetPresetCredentialStatus": {},
	"DisarmMixer":               {},

	// ---- configuration and session control (audit-logged) ----
	"Start":        {mutating: true},
	"Stop":         {mutating: true},
	"SaveConfig":   {mutating: true},
	"SetSecret":    {mutating: true},
	"ArmMixer":     {mutating: true},
	"ApplyPreset":  {mutating: true},
	"SavePreset":   {mutating: true},
	"RenamePreset": {mutating: true},
	"DeletePreset": {mutating: true},
	// SetChannelMap is a LIVE OPERATIONAL CONTROL, reachable exactly as the mixer
	// commands are and audit-logged for the same reason: it changes what the
	// commentator is heard on, on air, in about 119 microseconds. It is not
	// host-only, because the seat that notices the wrong channel is often the
	// remote one — a producer watching the meters while the operator is on
	// talkback — and refusing it there would leave them able to SEE the fault and
	// not to fix it. It cannot damage anything a remote seat could not already
	// damage with Stop, and internal/gst refuses any map that does not fit the
	// negotiated width before a byte reaches the element.
	"SetChannelMap": {mutating: true},

	// ---- the arm-gated write path and its baseline (audit-logged) ----
	// SendMixerCommands is still additionally gated on the caller being the seat
	// that armed (arm-ownership, see app_mixer.go) — that gate is about WHICH
	// seat holds the open window, not about authentication, so it stays.
	"SendMixerCommands": {mutating: true},
	"SetMixerGolden":    {mutating: true},

	// ---- host-only: the native picture/return surface ----
	// Refused for every connection and omitted from Methods() so the shim never
	// installs them.
	"SetPictureRect":    {hostOnly: true},
	"SetPictureVisible": {hostOnly: true},
	"StartPicture":      {hostOnly: true},
	"StopPicture":       {hostOnly: true},
	"StartReturn":       {hostOnly: true},
	"StopReturn":        {hostOnly: true},

	// ---- host-only: remote administration (local Settings screen only) ----
	"GetRemoteState":    {hostOnly: true},
	"SetRemoteListener": {hostOnly: true},
}

// ---------------------------------------------------------------------------
// The dispatcher
// ---------------------------------------------------------------------------

// remoteDispatcher adapts *App to remote.Dispatcher WITHOUT putting Call and
// Methods on *App itself — which matters, because Wails binds every exported
// method of the bound object, and a bound Call/Methods would both widen the
// frontend surface and defeat the drift guard. So the interface is satisfied by
// this tiny wrapper, and the real logic lives in unexported App methods.
type remoteDispatcher struct{ app *App }

var _ remote.Dispatcher = (*remoteDispatcher)(nil)

func (d *remoteDispatcher) Methods(client remote.ClientInfo) []string {
	return d.app.remoteMethods(client)
}

func (d *remoteDispatcher) Call(ctx context.Context, client remote.ClientInfo, method string, args []json.RawMessage) (any, error) {
	return d.app.remoteCall(ctx, client, method, args)
}

// remoteMethods is the authoritative list the shim installs on
// window.go.main.App for this connection: every allowlisted method that is NOT
// host-only, sorted for a stable hello frame. With no capability tiers every
// connection sees the same list; host-only methods never appear. Absence is the
// whole degradation mechanism, so this must agree with remoteCall's gates
// exactly — a method the shim installs but Call refuses would be a button that
// errors.
func (a *App) remoteMethods(client remote.ClientInfo) []string {
	out := make([]string, 0, len(remoteAllowlist))
	for name, pol := range remoteAllowlist {
		if pol.hostOnly {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// remoteCall runs the two gates and, if both pass, unmarshals the arguments and
// invokes the real bound method. It is the single chokepoint for every remote
// invocation; the local WebView2 seat never reaches it.
func (a *App) remoteCall(ctx context.Context, client remote.ClientInfo, method string, args []json.RawMessage) (any, error) {
	// Gate 1: allowlist. Unknown is refused even though *App may implement it.
	pol, known := remoteAllowlist[method]
	if !known {
		return nil, fmt.Errorf("remote: unknown method %q", method)
	}
	// Gate 2: host-only. Refused for every connection. A connection can only reach
	// here for a host-only method by CRAFTING a call the shim would never make
	// (the hello frame omitted it), so the attempt is logged.
	if pol.hostOnly {
		log.Printf("wslcomms: refused host-only method %q from %s", method, client.RemoteAddr)
		return nil, fmt.Errorf("remote: method %q is host-only and cannot be used from a remote client", method)
	}

	// Audit every state-changing call before it runs, with the caller's source
	// address. This is a machine that is on air; who changed what, from where,
	// must be in the log — and with no login, the source address is the only
	// identity there is.
	if pol.mutating {
		log.Printf("wslcomms: remote %s from %s", method, client.RemoteAddr)
	}

	return a.remoteInvoke(ctx, client, method, args)
}

// remoteInvoke is the hand-written argument-decoding switch. Every method that
// takes arguments unmarshals each one into its REAL Go type here; a method with
// no arguments simply calls through. SendMixerCommands and ArmMixer route to the
// arm-ownership-aware variants carrying the caller's id (see app_mixer.go), and
// SaveConfig routes to the variant that stamps the config event with it.
//
// The ctx passed here is the session's call context (cancelled on disconnect);
// most bound methods root their own work in a.rootCtx and do not take it, which
// is correct — a remote GetMixerSnapshot dial is bounded by the app's lifetime,
// not by one flaky socket — but it is threaded through so a future method that
// wants per-call cancellation has it.
func (a *App) remoteInvoke(ctx context.Context, client remote.ClientInfo, method string, args []json.RawMessage) (any, error) {
	switch method {

	// -------- view --------
	case "GetConfig":
		return a.GetConfig()
	case "GetKVSCredentials":
		return a.GetKVSCredentials()
	case "GetStatusKeyCandidates":
		return a.GetStatusKeyCandidates()
	case "ListEvents":
		return a.ListEvents()
	// The only bound method that cannot fail, so it is the only case here with a
	// literal nil error rather than a call that supplies one.
	case "CredentialStoreName":
		return a.CredentialStoreName(), nil
	case "GetMixerSnapshot":
		return a.GetMixerSnapshot()
	case "GetMixerGolden":
		return a.GetMixerGolden()
	case "GetPictureState":
		return a.GetPictureState()
	case "GetReturnState":
		return a.GetReturnState()
	case "GetChannelMap":
		return a.GetChannelMap()
	case "IsSRTReturnSelected":
		return a.IsSRTReturnSelected()
	case "ListInputDevices":
		return a.ListInputDevices()
	case "ListOutputDevices":
		return a.ListOutputDevices()
	case "ListPresets":
		return a.ListPresets()
	case "GetActivePreset":
		return a.GetActivePreset()
	case "GetPresetCredentialStatus":
		return a.GetPresetCredentialStatus()
	case "DisarmMixer":
		return nil, a.DisarmMixer()

	// -------- operate --------
	case "Start":
		return nil, a.Start()
	case "Stop":
		return nil, a.Stop()
	case "SaveConfig":
		var c config.Config
		if err := decodeArg(args, 0, &c); err != nil {
			return nil, err
		}
		return nil, a.saveConfigFrom(client.ID, &c)
	case "SetSecret":
		var key, value string
		if err := decodeArg(args, 0, &key); err != nil {
			return nil, err
		}
		if err := decodeArg(args, 1, &value); err != nil {
			return nil, err
		}
		// The KEY is logged and the VALUE never is: this is a passphrase crossing
		// the boundary, and the audit line must not become the leak.
		log.Printf("wslcomms: remote SetSecret key=%q from %s (value withheld)",
			key, client.RemoteAddr)
		return nil, a.SetSecret(key, value)
	case "ArmMixer":
		return a.armMixerFrom(client.ID)
	case "ApplyPreset":
		var id string
		if err := decodeArg(args, 0, &id); err != nil {
			return nil, err
		}
		return a.ApplyPreset(id)
	case "SavePreset":
		var name string
		if err := decodeArg(args, 0, &name); err != nil {
			return nil, err
		}
		return a.SavePreset(name)
	case "RenamePreset":
		var id, name string
		if err := decodeArg(args, 0, &id); err != nil {
			return nil, err
		}
		if err := decodeArg(args, 1, &name); err != nil {
			return nil, err
		}
		return nil, a.RenamePreset(id, name)
	case "DeletePreset":
		var id string
		var alsoDeleteCredentials bool
		if err := decodeArg(args, 0, &id); err != nil {
			return nil, err
		}
		if err := decodeArg(args, 1, &alsoDeleteCredentials); err != nil {
			return nil, err
		}
		return nil, a.DeletePreset(id, alsoDeleteCredentials)
	case "SetChannelMap":
		// Decoded into gst.ChannelMap itself rather than into a []map[string]any
		// and re-read: the wire form IS that type's json tags, so this is the one
		// place the shape is stated and there is no second grammar to drift from
		// it. An entry naming a channel the pad did not negotiate, or a gain
		// outside the [-1, 1] audioconvert clamps to, is refused BY NAME inside
		// SetChannelMap before anything is written.
		var m gst.ChannelMap
		if err := decodeArg(args, 0, &m); err != nil {
			return nil, err
		}
		return nil, a.SetChannelMap(m)

	// -------- mixer --------
	case "SendMixerCommands":
		var cmds []MixerCommand
		if err := decodeArg(args, 0, &cmds); err != nil {
			return nil, err
		}
		return nil, a.sendMixerCommandsFrom(client.ID, cmds)
	case "SetMixerGolden":
		var snap mixer.Snapshot
		if err := decodeArg(args, 0, &snap); err != nil {
			return nil, err
		}
		return nil, a.SetMixerGolden(&snap)

	default:
		// Unreachable: remoteCall already refused anything not in the allowlist,
		// and every non-host-only allowlisted method has a case above. If this is
		// ever hit, a method was added to the table without a decode case, and
		// refusing is the safe answer.
		return nil, fmt.Errorf("remote: method %q has no dispatch case", method)
	}
}

// decodeArg unmarshals the i-th positional argument into dst, or returns a typed
// error the session relays as an ordinary rejection. A missing argument is an
// error rather than a zero value: a SaveConfig with no config, decoded to an
// empty struct, would persist a blank configuration.
func decodeArg(args []json.RawMessage, i int, dst any) error {
	if i >= len(args) {
		return fmt.Errorf("remote: missing argument %d", i)
	}
	if err := json.Unmarshal(args[i], dst); err != nil {
		return fmt.Errorf("remote: decoding argument %d: %w", i, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The listener lifecycle
// ---------------------------------------------------------------------------

// remoteStopBudget bounds the teardown step that closes the listener. Closing a
// TLS http.Server and its session goroutines is prompt — there is no cgo and no
// device to release, only sockets and goroutines that select on a cancelled
// context — so one second is generous. It is a term in shutdownTimeout's sum.
const remoteStopBudget = 1 * time.Second

// remoteClientsPollInterval is how often the connected-client indicator is
// refreshed. Connections change rarely (a producer joining or leaving), so this
// is a cheap lock-and-compare that emits only on a change; a couple of seconds
// is imperceptible for an indicator and costs nothing between changes.
const remoteClientsPollInterval = 2 * time.Second

// startRemote (re)builds and starts the listener from the current settings file.
// It is called once from startup and again by SetRemoteListener when the
// operator changes whether the listener runs or where it binds, so it must be
// safe to call repeatedly: it stops any existing listener first, which is also
// how the new bind/ports take effect and how every live connection is dropped.
func (a *App) startRemote() {
	a.remoteMu.Lock()
	defer a.remoteMu.Unlock()
	a.startRemoteLocked()
}

// stopRemote closes the listener and joins its goroutines, for teardown. It is
// bounded by remoteStopBudget through the teardown step wrapping it; internally
// it releases remoteMu before the Close so a slow Close cannot hold the lock,
// and joins remoteWG so no per-generation goroutine outlives the shutdown. The
// closing flag (raised before this runs) means no restart can Add to remoteWG
// after this joins it.
func (a *App) stopRemote() error {
	a.remoteMu.Lock()
	if a.remoteRunCancel != nil {
		a.remoteRunCancel()
		a.remoteRunCancel = nil
	}
	srv := a.remote.Swap(nil)
	a.remoteMu.Unlock()

	var err error
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), remoteStopBudget)
		err = srv.Close(ctx)
		cancel()
	}
	a.remoteWG.Wait()
	return err
}

// startRemoteLocked does the work of startRemote; the caller holds remoteMu.
func (a *App) startRemoteLocked() {
	// Cancel and close any previous generation first. runCancel stops that
	// generation's client-indicator poll; Close reaps its listener, its
	// connections and its sessions within the budget.
	if a.remoteRunCancel != nil {
		a.remoteRunCancel()
		a.remoteRunCancel = nil
	}
	if old := a.remote.Swap(nil); old != nil {
		a.closeRemoteServer(old)
	}

	// Never stand a new listener up while the window is closing. A listener bound
	// during teardown is a socket and a clutch of goroutines the shutdown has
	// already walked past.
	if a.closing.Load() {
		return
	}

	settings, err := remote.LoadSettings()
	if err != nil {
		a.emitError(fmt.Errorf("wslcomms: the remote-access settings could not be read (%w); "+
			"remote access is OFF this run — repair it on the Settings screen", err))
		return
	}

	srv, err := a.buildRemoteServer(settings)
	if err != nil {
		a.emitError(fmt.Errorf("wslcomms: remote access could not be configured (%w); it is OFF this run", err))
		return
	}

	runCtx, runCancel := context.WithCancel(a.rootCtx)
	a.remoteRunCancel = runCancel

	// The server pointer is published to a.remote ONLY AFTER Start() returns, from
	// inside the goroutine — deliberately not before. Start writes the server's
	// cert fingerprint and http.Server WITHOUT a lock, on the contract that it
	// completes-before any Fingerprint()/Close(); publishing through the atomic
	// only after Start gives every reader (the Settings screen's GetRemoteState,
	// teardown's stopRemote, the event tee) a happens-before edge to those writes.
	// A reader that Loads a nil pointer simply sees "not running yet", which is the
	// truth. The publish is guarded by remoteMu and a runCtx liveness check so a
	// teardown or restart that raced Start closes the server rather than orphaning
	// it — see closeRemoteServer.
	a.remoteWG.Add(1)
	go func() {
		defer a.remoteWG.Done()
		if err := srv.Start(); err != nil {
			// A bind/listen failure must not block the window or crash the app; it
			// is reported on the error banner and the app runs on without remote
			// access, exactly as a first-run configuration problem is. On Windows a
			// busy 80/443 falls back to 8080/8443 inside Start, so this fires only
			// when even the fallback is unavailable.
			a.emitError(fmt.Errorf("wslcomms: the remote-access listener could not start (%w); "+
				"remote access is OFF this run", err))
			return
		}
		if !srv.Running() {
			// Disabled by settings: the do-nothing posture. Nothing bound, nothing
			// to publish, nothing to poll.
			return
		}

		a.remoteMu.Lock()
		if runCtx.Err() != nil {
			// This generation was torn down (teardown) or superseded (a restart)
			// while Start was binding. Do not publish a server nobody will close;
			// close it here, on this goroutine, where Start's writes happen-before
			// the Close read.
			a.remoteMu.Unlock()
			a.closeRemoteServer(srv)
			return
		}
		a.remote.Store(srv)
		a.remoteMu.Unlock()

		log.Printf("wslcomms: remote access listening (UNAUTHENTICATED) on %s and %s (cert %s)",
			srv.HTTPURL(), srv.HTTPSURL(), srv.Fingerprint())
		a.watchRemoteClients(runCtx, srv)
	}()
}

// closeRemoteServer closes one server within the stop budget, logging a failure.
// It is used both to reap a previous generation on restart and to close a server
// whose generation was cancelled while Start was still binding.
func (a *App) closeRemoteServer(srv *remote.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), remoteStopBudget)
	defer cancel()
	if err := srv.Close(ctx); err != nil {
		log.Printf("wslcomms: closing a remote listener: %v", err)
	}
}

// buildRemoteServer assembles the transport's Options from the settings and the
// application's own pieces: the allowlist dispatcher, the SAME embedded frontend
// the local window serves (fs.Sub'd to the dist root so the remote page is
// byte-identical), the cert directory, and the event names for the hello frame.
// There is no authenticator to build — the listener is unauthenticated.
func (a *App) buildRemoteServer(settings *remote.Settings) (*remote.Server, error) {
	dist, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		return nil, fmt.Errorf("locating the embedded frontend: %w", err)
	}
	certDir, err := remote.RemoteDir()
	if err != nil {
		return nil, err
	}
	return remote.NewServer(remote.Options{
		Enabled:    settings.Enabled,
		Bind:       settings.Bind,
		HTTPPort:   settings.HTTPPort,
		HTTPSPort:  settings.HTTPSPort,
		Dispatcher: &remoteDispatcher{app: a},
		Assets:     dist,
		CertDir:    certDir,
		Events:     remoteEventNames(),
		Logf:       log.Printf,
	}), nil
}

// watchRemoteClients emits EventRemote whenever the set of connected seats
// changes, until ctx is cancelled (a restart or teardown). It compares the set
// of connection ids so a client reconnecting under a new id is a change and a
// steady state emits nothing.
func (a *App) watchRemoteClients(ctx context.Context, srv *remote.Server) {
	ticker := time.NewTicker(remoteClientsPollInterval)
	defer ticker.Stop()

	var last string // a cheap fingerprint of the connected-id set
	emit := func() {
		clients := srv.Clients()
		fp := remoteClientsFingerprint(clients)
		if fp == last {
			return
		}
		last = fp
		a.events.send(EventRemote, remoteClientPayloads(clients))
	}
	emit() // publish the initial (empty) set so a page that just loaded is in step

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			emit()
		}
	}
}

// RemoteConnectedClient is one connected seat as the home-screen indicator sees
// it. With no authentication there is no client name, so the seat is identified
// by where it connected from: Name carries the source IP (the host part of the
// peer address) and Addr the full host:port. That is enough for the operator to
// know a second seat is present and roughly where it is.
type RemoteConnectedClient struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

// remoteClientPayloads projects the transport's ClientInfo down to the display
// shape. The local WebView2 seat is not in this list — it does not connect to
// the listener — so the indicator shows only OTHER seats, which is exactly the
// question the operator is asking.
func remoteClientPayloads(clients []remote.ClientInfo) []RemoteConnectedClient {
	out := make([]RemoteConnectedClient, 0, len(clients))
	for _, c := range clients {
		out = append(out, RemoteConnectedClient{Name: remoteSeatName(c.RemoteAddr), Addr: c.RemoteAddr})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Addr != out[j].Addr {
			return out[i].Addr < out[j].Addr
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// remoteSeatName reduces a peer host:port to its host, the only stable identity
// an unauthenticated seat has. It falls back to the whole address if it does not
// split, which is harmless for a display label.
func remoteSeatName(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// remoteClientsFingerprint is a stable string over the connected connection ids,
// used only to decide whether the set changed since the last poll.
func remoteClientsFingerprint(clients []remote.ClientInfo) string {
	ids := make([]string, 0, len(clients))
	for _, c := range clients {
		ids = append(ids, c.ID)
	}
	sort.Strings(ids)
	return strings.Join(ids, "\x00")
}

// ---------------------------------------------------------------------------
// The remote-admin bound surface (HOST-ONLY: local Settings screen only)
// ---------------------------------------------------------------------------

// RemoteState is what the Settings screen renders: whether the listener is
// enabled, where it binds, the two ports, the URLs to reach it in a remote
// browser, and the certificate fingerprint to compare. There are NO client
// records — the listener is unauthenticated. When the listener is running, the
// URLs and ports reflect the ACTUALLY bound values (a fallback may have moved
// 80/443 to 8080/8443); when it is not, they reflect the configured values. A
// non-empty certFingerprint is the "it is running" signal (empty before Start).
type RemoteState struct {
	Enabled     bool   `json:"enabled"`
	Bind        string `json:"bind"`
	HTTPPort    int    `json:"httpPort"`
	HTTPSPort   int    `json:"httpsPort"`
	HTTPURL     string `json:"httpURL"`
	HTTPSURL    string `json:"httpsURL"`
	Fingerprint string `json:"certFingerprint"`
}

// GetRemoteState reports the remote-access configuration and live status. It is
// HOST-ONLY: the remote dispatcher refuses it, so only the local Settings screen
// ever reads it.
func (a *App) GetRemoteState() (RemoteState, error) {
	settings, err := remote.LoadSettings()
	if err != nil {
		return RemoteState{}, err
	}
	st := RemoteState{
		Enabled:   settings.Enabled,
		Bind:      settings.Bind,
		HTTPPort:  settings.HTTPPort,
		HTTPSPort: settings.HTTPSPort,
		HTTPURL:   "http://" + net.JoinHostPort(settings.Bind, fmt.Sprintf("%d", settings.HTTPPort)),
		HTTPSURL:  "https://" + net.JoinHostPort(settings.Bind, fmt.Sprintf("%d", settings.HTTPSPort)),
	}
	if srv := a.remote.Load(); srv != nil && srv.Fingerprint() != "" {
		// A non-empty fingerprint means Start bound the sockets and generated the
		// cert, i.e. the listener is actually running. Prefer the actually-bound
		// ports and URLs, which differ from the configured ones when a fallback ran.
		st.Fingerprint = srv.Fingerprint()
		st.HTTPPort = srv.HTTPPort()
		st.HTTPSPort = srv.HTTPSPort()
		if u := srv.HTTPURL(); u != "" {
			st.HTTPURL = u
		}
		if u := srv.HTTPSURL(); u != "" {
			st.HTTPSURL = u
		}
	}
	return st, nil
}

// SetRemoteListener changes whether the listener runs, where it binds and on
// which two ports, then restarts it. HOST-ONLY. It validates BEFORE saving so a
// bad bind (a hostname) or an out-of-range port is refused with its reason
// rather than written and then failing to start. There is deliberately no guard
// on WIDENING the bind — a wildcard bind is the owner's intended default.
func (a *App) SetRemoteListener(enabled bool, bind string, httpPort, httpsPort int) error {
	settings, err := remote.LoadSettings()
	if err != nil {
		return err
	}
	settings.Enabled = enabled
	settings.Bind = strings.TrimSpace(bind)
	settings.HTTPPort = httpPort
	settings.HTTPSPort = httpsPort
	if err := settings.Validate(); err != nil {
		return err
	}
	if err := settings.Save(); err != nil {
		return err
	}
	a.startRemote()
	return nil
}

// ---------------------------------------------------------------------------
// The event-pump tee
// ---------------------------------------------------------------------------

// broadcastRemote is the event-pump tee: every event that reaches
// wailsruntime.EventsEmit for the local renderer ALSO reaches every connected
// remote seat through here. It reads the server pointer without a lock (it is an
// atomic, written under remoteMu) and never blocks — remote.Server.Broadcast
// fans out through per-session drop-oldest queues, so one stalled remote client
// can neither block the pump nor the other seats.
//
// It is safe on a nil server (before startup, or after a settings load failure)
// and on a closed one (during teardown, when the pump may still be draining):
// Broadcast on a server with no sessions is a no-op.
func (a *App) broadcastRemote(name string, data any) {
	if srv := a.remote.Load(); srv != nil {
		srv.Broadcast(name, data)
	}
}
