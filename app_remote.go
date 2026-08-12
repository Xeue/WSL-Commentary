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
// ===================== THE THREE GATES, IN ORDER ============================
//
//  1. ALLOWLIST   unknown method            -> refused ("unknown method")
//  2. HOST-ONLY   the six native-surface    -> refused at EVERY capability,
//     methods + the five remote     and absent from the hello
//     admin methods                 methods list (degrade by
//     OMISSION, not by a refusal the
//     frontend must be taught)
//  3. CAPABILITY  view / operate / mixer    -> refused ("requires capability")
//
// Only after all three pass are a method's arguments json.Unmarshalled into
// their real Go types and the real bound method called. Keeping the args opaque
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
// and SRT-return methods are HostOnly: refused for every remote client at every
// capability. The remote page gets the WebRTC mosaic and an honest message; it
// never gets these methods, because the hello frame omits them.
//
// The five remote-admin methods (GetRemoteState, SetRemoteListener,
// AddRemoteClient, SetRemoteClientPassword, DeleteRemoteClient) are HostOnly for
// a blunter reason: they change WHO may connect and on WHAT address. A listener
// that could be reconfigured by one of its own remote clients is a listener that
// can be widened to the world by whoever first gets in. Those methods exist for
// the local operator's Settings screen only.
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
		EventConfig,
		EventRemote,
	}
}

// ---------------------------------------------------------------------------
// The allowlist table
// ---------------------------------------------------------------------------

// methodPolicy is one row of the allowlist: the capability tier a method needs,
// whether it is host-only (refused for every remote client), and whether a
// remote invocation of it should be written to the audit log because it changes
// state on a machine that is on air.
type methodPolicy struct {
	// cap is the tier a remote client must hold to call this method. For a
	// host-only method it is a placeholder — the host-only gate refuses the call
	// before the capability gate is reached — but it is stated rather than left
	// zero so the row is self-describing and Methods() can be written uniformly.
	cap remote.Capability

	// hostOnly refuses this method for EVERY remote client at EVERY capability,
	// and omits it from the hello methods list.
	hostOnly bool

	// mutating marks a method whose remote invocation is audit-logged with the
	// caller's name and address. SetSecret is additionally logged with its KEY
	// (never its value) in the dispatch switch.
	mutating bool
}

// remoteAllowlist is THE authoritative map from a bound method name to its
// remote policy. Every exported method of *App must appear here — the drift
// guard in app_remote_test.go fails by name if one does not — so that adding a
// binding forces a deliberate classification rather than defaulting it open.
//
// The classification, stated once so it can be reviewed as a whole:
//
//   - VIEW is read-only plus the two always-safe gestures a viewer needs:
//     reading config/state/devices/presets, the KVS credentials that fetch the
//     mosaic (over TLS), and DisarmMixer — shutting a gate is always safe, so it
//     is open to anyone, which is why it is view and not operate.
//   - OPERATE is configuration and session control: Start/Stop, SaveConfig,
//     SetSecret, the preset writes, and ArmMixer (which changes NOTHING on the
//     desk — it only permits a later write).
//   - MIXER is the arm-gated write path to the live desk (SendMixerCommands) and
//     the drift baseline it is read against (SetMixerGolden). It is the one tier
//     that can change what goes to air, so it is off by default in a client's
//     capabilities and, for SendMixerCommands, additionally gated on the caller
//     being the seat that armed (see app_mixer.go).
//   - HOST-ONLY is the six native-surface methods and the five remote-admin
//     methods; see the file header.
var remoteAllowlist = map[string]methodPolicy{
	// ---- view: read-only, plus the always-safe DisarmMixer ----
	"GetConfig":                 {cap: remote.CapView},
	"GetKVSCredentials":         {cap: remote.CapView},
	"GetStatusKeyCandidates":    {cap: remote.CapView},
	"ListEvents":                {cap: remote.CapView},
	"GetMixerSnapshot":          {cap: remote.CapView},
	"GetMixerGolden":            {cap: remote.CapView},
	"GetPictureState":           {cap: remote.CapView},
	"GetReturnState":            {cap: remote.CapView},
	"IsSRTReturnSelected":       {cap: remote.CapView},
	"ListInputDevices":          {cap: remote.CapView},
	"ListOutputDevices":         {cap: remote.CapView},
	"ListPresets":               {cap: remote.CapView},
	"GetActivePreset":           {cap: remote.CapView},
	"GetPresetCredentialStatus": {cap: remote.CapView},
	"DisarmMixer":               {cap: remote.CapView},

	// ---- operate: configuration and session control ----
	"Start":        {cap: remote.CapOperate, mutating: true},
	"Stop":         {cap: remote.CapOperate, mutating: true},
	"SaveConfig":   {cap: remote.CapOperate, mutating: true},
	"SetSecret":    {cap: remote.CapOperate, mutating: true},
	"ArmMixer":     {cap: remote.CapOperate, mutating: true},
	"ApplyPreset":  {cap: remote.CapOperate, mutating: true},
	"SavePreset":   {cap: remote.CapOperate, mutating: true},
	"RenamePreset": {cap: remote.CapOperate, mutating: true},
	"DeletePreset": {cap: remote.CapOperate, mutating: true},

	// ---- mixer: the arm-gated write path and its baseline ----
	"SendMixerCommands": {cap: remote.CapMixer, mutating: true},
	"SetMixerGolden":    {cap: remote.CapMixer, mutating: true},

	// ---- host-only: the native picture/return surface ----
	// The capability is a placeholder; hostOnly refuses these before the tier is
	// consulted. They are omitted from Methods() so the shim never installs them.
	"SetPictureRect":    {cap: remote.CapView, hostOnly: true},
	"SetPictureVisible": {cap: remote.CapView, hostOnly: true},
	"StartPicture":      {cap: remote.CapOperate, hostOnly: true},
	"StopPicture":       {cap: remote.CapOperate, hostOnly: true},
	"StartReturn":       {cap: remote.CapOperate, hostOnly: true},
	"StopReturn":        {cap: remote.CapOperate, hostOnly: true},

	// ---- host-only: remote administration (local Settings screen only) ----
	"GetRemoteState":          {cap: remote.CapOperate, hostOnly: true},
	"SetRemoteListener":       {cap: remote.CapOperate, hostOnly: true},
	"AddRemoteClient":         {cap: remote.CapOperate, hostOnly: true},
	"SetRemoteClientPassword": {cap: remote.CapOperate, hostOnly: true},
	"DeleteRemoteClient":      {cap: remote.CapOperate, hostOnly: true},
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
// window.go.main.App for this client: every allowlisted method that is NOT
// host-only and that the client's capabilities permit, sorted for a stable hello
// frame. Host-only methods never appear regardless of capability; higher-tier
// methods appear only for a client that holds the tier. Absence is the whole
// degradation mechanism, so this must agree with remoteCall's gates exactly —
// a method the shim installs but Call refuses would be a button that errors.
func (a *App) remoteMethods(client remote.ClientInfo) []string {
	out := make([]string, 0, len(remoteAllowlist))
	for name, pol := range remoteAllowlist {
		if pol.hostOnly {
			continue
		}
		if remote.Allows(client.Caps, pol.cap) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// remoteCall runs the three gates and, if all pass, unmarshals the arguments and
// invokes the real bound method. It is the single chokepoint for every remote
// invocation; the local WebView2 seat never reaches it.
func (a *App) remoteCall(ctx context.Context, client remote.ClientInfo, method string, args []json.RawMessage) (any, error) {
	// Gate 1: allowlist. Unknown is refused even though *App may implement it.
	pol, known := remoteAllowlist[method]
	if !known {
		return nil, fmt.Errorf("remote: unknown method %q", method)
	}
	// Gate 2: host-only. Refused at every capability. A remote client can only
	// reach here for a host-only method by CRAFTING a call the shim would never
	// make (the hello frame omitted it), so the attempt is logged.
	if pol.hostOnly {
		log.Printf("wslcomms: refused host-only method %q from remote client %q (%s)",
			method, client.Name, client.RemoteAddr)
		return nil, fmt.Errorf("remote: method %q is host-only and cannot be used from a remote client", method)
	}
	// Gate 3: capability tier.
	if !remote.Allows(client.Caps, pol.cap) {
		return nil, fmt.Errorf("remote: method %q requires capability %q", method, pol.cap)
	}

	// Audit every state-changing call before it runs, with the caller's name and
	// address. This is a machine that is on air; who changed what, from where,
	// must be in the log.
	if pol.mutating {
		log.Printf("wslcomms: remote %s by client %q from %s", method, client.Name, client.RemoteAddr)
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
	case "GetMixerSnapshot":
		return a.GetMixerSnapshot()
	case "GetMixerGolden":
		return a.GetMixerGolden()
	case "GetPictureState":
		return a.GetPictureState()
	case "GetReturnState":
		return a.GetReturnState()
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
		log.Printf("wslcomms: remote SetSecret key=%q by client %q from %s (value withheld)",
			key, client.Name, client.RemoteAddr)
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
// It is called once from startup and again by each remote-admin method that
// changes the settings, so it must be safe to call repeatedly: it stops any
// existing listener first, which — because internal/remote's Authenticator holds
// a snapshot of the client list — is also how a client add/delete/password
// change takes effect and how every live session is revoked.
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
		addr, err := srv.Start()
		if err != nil {
			// A bind failure must not block the window or crash the app; it is
			// reported on the error banner and the app runs on without remote
			// access, exactly as a first-run configuration problem is.
			a.emitError(fmt.Errorf("wslcomms: the remote-access listener could not start (%w); "+
				"remote access is OFF this run", err))
			return
		}
		if addr == "" {
			// Disabled by settings: the safe, do-nothing posture. Nothing bound,
			// nothing to publish, nothing to poll.
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

		log.Printf("wslcomms: remote access listening on https://%s (cert %s)", addr, srv.Fingerprint())
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
// application's own pieces: the allowlist dispatcher, an Authenticator over the
// client list, the SAME embedded frontend the local window serves (fs.Sub'd to
// the dist root so the remote page is byte-identical), the cert directory, and
// the event names for the hello frame.
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
		Port:       settings.Port,
		Dispatcher: &remoteDispatcher{app: a},
		Auth:       remote.NewAuthenticator(settings.Clients),
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
// it. It deliberately carries no cookie, token or capability detail beyond the
// name and where it connected from — enough for the operator to know who has a
// seat, nothing that would be a credential if the event were logged.
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
		out = append(out, RemoteConnectedClient{Name: c.Name, Addr: c.RemoteAddr})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Addr < out[j].Addr
	})
	return out
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
// enabled, where it binds, whether it is actually running, the URL and cert
// fingerprint to trust in a remote browser, the configured clients (with a
// has-password flag, never the hash), and the seats connected right now.
type RemoteState struct {
	Enabled     bool                    `json:"enabled"`
	Bind        string                  `json:"bind"`
	Port        int                     `json:"port"`
	Running     bool                    `json:"running"`
	Address     string                  `json:"address"`
	URL         string                  `json:"url"`
	Fingerprint string                  `json:"fingerprint"`
	Clients     []RemoteClientStatus    `json:"clients"`
	Connected   []RemoteConnectedClient `json:"connected"`
}

// RemoteClientStatus is one configured client as the Settings screen sees it:
// the name, the granted capabilities, and whether a password has been set — a
// boolean, never the verifier, mirroring the "set / not set" secret-badge
// convention the Settings screen already uses for the M2L-X and SRT passphrases.
type RemoteClientStatus struct {
	Name        string   `json:"name"`
	Caps        []string `json:"caps"`
	HasPassword bool     `json:"hasPassword"`
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
		Enabled: settings.Enabled,
		Bind:    settings.Bind,
		Port:    settings.Port,
		URL:     fmt.Sprintf("https://%s", net.JoinHostPort(settings.Bind, fmt.Sprintf("%d", settings.Port))),
	}
	for _, c := range settings.Clients {
		st.Clients = append(st.Clients, RemoteClientStatus{
			Name:        c.Name,
			Caps:        c.Caps,
			HasPassword: c.PBKDF2.Hash != "",
		})
	}
	if srv := a.remote.Load(); srv != nil {
		if addr := srv.Fingerprint(); addr != "" {
			// A non-empty fingerprint means Start bound a socket and generated the
			// cert, i.e. the listener is actually running.
			st.Running = true
			st.Fingerprint = addr
			st.Connected = remoteClientPayloads(srv.Clients())
		}
	}
	return st, nil
}

// SetRemoteListener changes whether the listener runs and where it binds, then
// restarts it. HOST-ONLY. It validates BEFORE saving so a bad bind (a hostname,
// a routable address with no clients) is refused with its reason rather than
// written and then failing to start.
func (a *App) SetRemoteListener(enabled bool, bind string, port int) error {
	settings, err := remote.LoadSettings()
	if err != nil {
		return err
	}
	settings.Enabled = enabled
	settings.Bind = strings.TrimSpace(bind)
	settings.Port = port
	if err := settings.Validate(); err != nil {
		return err
	}
	if err := settings.Save(); err != nil {
		return err
	}
	a.startRemote()
	return nil
}

// AddRemoteClient adds a named client with the given capabilities and no
// password (the operator sets one with SetRemoteClientPassword before it can log
// in). HOST-ONLY. It restarts the listener so the new client list — held as a
// snapshot in the Authenticator — takes effect.
func (a *App) AddRemoteClient(name string, caps []string) error {
	settings, err := remote.LoadSettings()
	if err != nil {
		return err
	}
	if err := settings.AddClient(name, caps); err != nil {
		return err
	}
	if err := settings.Save(); err != nil {
		return err
	}
	a.startRemote()
	return nil
}

// SetRemoteClientPassword sets or replaces a client's password. HOST-ONLY. The
// plaintext is hashed and never stored; there is deliberately no getter. It
// restarts the listener so the new verifier takes effect and every existing
// session is revoked.
func (a *App) SetRemoteClientPassword(name, password string) error {
	settings, err := remote.LoadSettings()
	if err != nil {
		return err
	}
	if err := settings.SetClientPassword(name, password); err != nil {
		return err
	}
	if err := settings.Save(); err != nil {
		return err
	}
	a.startRemote()
	return nil
}

// DeleteRemoteClient removes a client and restarts the listener, which revokes
// that client's live sessions along with everyone else's. HOST-ONLY.
func (a *App) DeleteRemoteClient(name string) error {
	settings, err := remote.LoadSettings()
	if err != nil {
		return err
	}
	if err := settings.DeleteClient(name); err != nil {
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
