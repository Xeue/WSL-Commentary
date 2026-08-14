//go:build dev || production || bindings

// The mixer drawer's half of the bound surface.
//
// Owner: WP-8. The six exported methods here are listed in app.go's header
// alongside the original eight, because Wails binds every exported method of
// *App and that list IS the contract.
//
// ===================== WHAT IS BEING WIRED, AND WHY =========================
//
// internal/mixer and frontend/src/ui/mixer/ are a complete drawer with nothing
// connecting them. This file is that connection, and it is short on purpose:
// the safety properties live in the two packages it joins, and every one of
// them is defeated by a host that is helpful in the wrong place.
//
//	READ   GetMixerSnapshot   fresh, never cached — see below
//	GATE   ArmMixer/DisarmMixer   opens and shuts the Go-side write window
//	WRITE  SendMixerCommands  the only method here that can change a live desk
//	STATE  Get/SetMixerGolden a local file; never touches the mixer
//
// ===================== THE FRESH-SNAPSHOT GUARANTEE =========================
//
// contract.js states this as a HOST-SIDE guarantee, and the drawer's whole S2
// protection rests on it: drawer.js's applySnapshot marks the view fresh at the
// moment it adopts a frame, so a host that answers getSnapshot from a cache
// makes a forty-second-old matrix look live. That matters because set_routing
// is an ABSOLUTE REPLACE — the drawer builds the argument from the routing it
// is holding — so a write planned on a stale matrix is one intended change plus
// a rollback of every other bus on that strip, applied to a desk that is on air.
//
// So GetMixerSnapshot caches NOTHING. Every call opens a fresh status
// connection through m2lx.Watcher.RawSnapshot, takes the opening frame, parses
// it and closes. THAT IS THE ONLY WAY TO SATISFY THE GUARANTEE, because of the
// protocol: the switcher_status socket is snapshot-then-delta (internal/m2lx/
// wire.go, MEASURED 2026-08-01 over 3180 frames), so a complete document exists
// exactly once per connection. A held-open socket delivers one snapshot and
// then subtree deltas for ever.
//
// ===================== THE OPEN QUESTION, NOT PAPERED OVER ==================
//
// It is NOT KNOWN whether a routing or mute CHANGE is pushed as a whole-node
// state at path "/" or as a subtree delta. internal/m2lx says so plainly and
// this file does not pretend otherwise. The consequences, stated rather than
// hidden:
//
//   - No mixer state is pushed to the frontend. There is no "mixer" event, and
//     the event list in app.go's header is unchanged. A pushed "latest known"
//     frame is precisely the cached stale frame the drawer must not be given.
//   - The frontend gets a current picture by CALLING GetMixerSnapshot, and it
//     calls it repeatedly while the drawer is open (settings.js, MIXER_POLL_MS).
//     Each call is one dial. That cost is real and is the price of the open
//     question; when the question is answered — if changes do arrive at "/" —
//     a delta-fed subscription can replace the polling and this comment should
//     go with it.
//   - If the polling stops or fails, the drawer stops receiving update() and
//     declares itself STALE after model.js's STALE_AFTER_MS, which blocks its
//     Apply path. That is the stale-snapshot warning the drawer already
//     implements, reached honestly rather than simulated.
//
// ===================== ONE WRITE PATH, DELIBERATELY =========================
//
// SendMixerCommands reaches the mixer only through mixer.Controller.Send, which
// refuses with mixer.ErrDisarmed unless ArmMixer has opened a window that has
// not expired. There is no second path and there must never be one: the Go gate
// is the SECOND of two independent gates (the first is createWriteGate in
// frontend/src/ui/mixer/model.js), and the entire value of two gates is that a
// bypass needs both. A convenience binding that built a Command and wrote it
// without the Controller would be a bypass with a friendly name.
//
// The controller is built by ArmMixer and closed by DisarmMixer, never on
// application start: mixer.NewController opens a socket capable of changing a
// live clean feed, and a socket that exists is a socket that can be used.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wslcomms/internal/config"
	"wslcomms/internal/mixer"
	"wslcomms/internal/secrets"
)

const (
	// mixerSnapshotTimeout bounds one GetMixerSnapshot from the frontend's
	// point of view. internal/m2lx bounds the socket work at its own
	// rawSnapshotTimeout (15 s); this is that plus room for the parse of an
	// 84 KB frame, so the inner bound is the one that normally fires and its
	// message — which says whether the socket answered — is the one the
	// operator sees.
	mixerSnapshotTimeout = 20 * time.Second

	// mixerSendTimeout bounds one batch of writes, including any wait for the
	// controller to reconnect. internal/mixer retries a failed write once on a
	// fresh connection, so this has to cover two dials; ten seconds does, and
	// an operator staring at a live desk should not wait longer than that to
	// find out whether their correction went.
	mixerSendTimeout = 10 * time.Second

	// mixerGoldenFileName is the golden snapshot's file, kept beside
	// config.json in %APPDATA%\WSLComms.
	//
	// It is a separate file rather than a field of config.Config on purpose: it
	// is large, it is operational rather than configuration, and a corrupt one
	// must not be able to take config.json — and with it the M2L-X coordinates
	// — down with it.
	mixerGoldenFileName = "mixer-golden.json"
)

// errMixerNoControlPlane is returned when a mixer method is called before the
// application has an M2L-X client, i.e. before m2lxHost is configured.
var errMixerNoControlPlane = errors.New(
	"wslcomms: the mixer is unavailable: no M2L-X host is configured — set it on the Settings screen")

// errMixerNotSignedIn is returned when there is a client but no bearer token
// yet. Both the status socket and the controller socket carry the token in
// their URL, so neither can be opened without one.
//
// The store is named through secrets.StoreName rather than spelled out, so the
// operator is sent to a facility their machine actually has. It is a package
// variable built at init rather than a constant because of that call, which is
// the whole cost of the change: errors.Is still matches the same value, and
// app_mixer_test.go compares against the variable rather than the text.
var errMixerNotSignedIn = fmt.Errorf(
	"wslcomms: the mixer is unavailable: not signed in to M2L-X yet — "+
		"check the alias and the password stored in %s", secrets.StoreName())

// MixerArmState is what ArmMixer reports back to the drawer.
//
// It carries the DEADLINE rather than a remaining duration, because the window
// auto-clears on a wall-clock deadline in Go (mixer.ArmWindow) and a duration
// handed across the boundary starts ageing the moment it is sent. A frontend
// that wants a countdown can compute one; a frontend that trusts a stale
// remaining-seconds number would show a gate as open after it had shut.
//
// It is a report and never a permission. mixer.Controller.Send re-checks the
// deadline at the moment of the write, so nothing here needs to be, or can be,
// relied on by a caller deciding whether to write.
type MixerArmState struct {
	// Armed is whether a window is open as at the moment this was built.
	Armed bool `json:"armed"`

	// ArmedUntil is when the window closes. Zero when Armed is false.
	ArmedUntil time.Time `json:"armedUntil"`

	// WindowSeconds is the length of the window that was just opened, so the
	// drawer can say "two minutes" without hard-coding mixer.ArmWindow in
	// JavaScript and having the two drift.
	WindowSeconds int `json:"windowSeconds"`
}

// MixerCommand is one write as it crosses the Wails boundary.
//
// It mirrors the MixerCommand typedef in frontend/src/ui/mixer/contract.js:
// a kind and an args object whose shape depends on the kind. Args is held raw
// and decoded per kind by decodeMixerCommand, so that an args object of the
// wrong shape is REJECTED rather than silently decoded into a zero value —
// a set_routing whose outputs failed to decode would otherwise become an
// empty bus set, which un-routes the strip from programme as well as from the
// clean feed.
type MixerCommand struct {
	// Kind is one of the five names in contract.js: setRouting,
	// setInputMuted, setOutputMuted, setChFader, setCompLimit.
	Kind string `json:"kind"`

	// Args is the command's arguments, in the wire shapes documented on the
	// corresponding types in internal/mixer/mixer.go.
	Args json.RawMessage `json:"args"`
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// GetMixerSnapshot returns the mixer's state now.
//
// IT NEVER RETURNS A CACHED FRAME. Every call opens a fresh switcher_status
// connection, takes the opening whole-document frame and closes again; see this
// file's header for why the protocol leaves no other honest option, and for
// what one call costs.
//
// It is READ-ONLY and safe to call at any time, armed or not — it does not
// touch the controller socket at all, and the status socket is push-only.
//
// # It refuses rather than returning a mixer it cannot vouch for
//
// mixer.ParseSnapshotWithWarnings can return a usable Snapshot alongside
// CRITICAL warnings: fields whose loss, rendered from a zero value, would let an
// operator read a false safety claim off the drawer. In practice that means
// routing — a strip whose matrix entry is missing parses to an empty Outputs,
// which renders identically to a strip that is genuinely clear of the clean
// feed.
//
// internal/mixer's intended handling is to render the snapshot AND the warnings
// together. THIS HOST CANNOT DO THAT: MixerDrawerOptions has no member for
// warnings, so anything returned here is rendered as fact. Given the choice
// between showing an operator "this strip is not in the clean feed" when the
// truth is "unknown", and showing them nothing at all, this returns the error.
// The drawer then reports that it could not read the mixer and — because
// nothing refreshed its view — goes stale and blocks its own write path, which
// is the correct outcome for a mixer nobody can currently read.
//
// Non-critical warnings are logged and do not fail the call: the contract's own
// absence flags (Strip.Metered, BusState.FaderPresent) already render them.
func (a *App) GetMixerSnapshot() (mixer.Snapshot, error) {
	a.ctlMu.Lock()
	watcher := a.watcher
	a.ctlMu.Unlock()

	if watcher == nil {
		return mixer.Snapshot{}, errMixerNoControlPlane
	}

	ctx, cancel := context.WithTimeout(a.rootCtx, mixerSnapshotTimeout)
	defer cancel()

	raw, err := watcher.RawSnapshot(ctx)
	if err != nil {
		return mixer.Snapshot{}, fmt.Errorf("wslcomms: reading the mixer from the status socket: %w", err)
	}

	// The frame has to carry the mixer node as a WHOLE node.
	//
	// A subtree delta — "/levels" is pushed about ten times a second on this
	// very node, 1501 frames in 150 s of live capture — read as the node's
	// entire state yields a mixer with no strips and no matrix. That renders as
	// "nothing is routed to the clean feed", which is the most dangerous false
	// statement this drawer can make.
	//
	// THIS IS THE THIRD CHECK, NOT THE ONLY ONE, and the distinction matters
	// because an earlier version of this comment said internal/mixer "does not
	// look at path" — which was true when written and is not now. Reading that
	// and concluding the parser is unsafe is one step from deleting the parser's
	// own check as redundant. All three are deliberate:
	//
	//   1. internal/m2lx's RawSnapshot returns only whole-document frames.
	//   2. internal/mixer's ParseSnapshot refuses an entry whose path is not "/",
	//      and names the offending path when it does.
	//   3. this call, on the one node whose emptiness is dangerous.
	//
	// The cost of being wrong here is a client hearing commentary on their clean
	// feed, so a redundant check is the correct trade.
	if err := checkMixerNodeIsWhole(raw); err != nil {
		return mixer.Snapshot{}, err
	}

	snap, warnings, err := mixer.ParseSnapshotWithWarnings(raw)
	if err != nil {
		return mixer.Snapshot{}, fmt.Errorf("wslcomms: reading the mixer state: %w", err)
	}

	var critical []string
	for _, w := range warnings {
		if w.Critical {
			critical = append(critical, w.String())
			continue
		}
		log.Printf("wslcomms: mixer snapshot warning: %s", w)
	}
	if len(critical) > 0 {
		err := fmt.Errorf(
			"wslcomms: the mixer state could not be read completely, so it is not being shown: "+
				"%d field(s) whose routing is UNKNOWN, not clear — %s",
			len(critical), strings.Join(critical, "; "))
		// The operator sees this on the error banner as well as inside the
		// drawer, because a drawer that cannot be trusted is worth noticing
		// even if it is not the screen they are looking at.
		a.emitError(err)
		return mixer.Snapshot{}, err
	}

	return snap, nil
}

// mixerNodeName is the DSP node the drawer reads. It is duplicated from
// internal/mixer's unexported constant of the same value rather than exported
// from there, because this is a guard against that package's parser and a guard
// that shares a constant with the thing it guards is not a guard.
const mixerNodeName = "advanced_audio_mixer"

// wholeNodePath is the "path" of a status entry carrying a node's entire state.
// Mirrors internal/m2lx/wire.go's constant of the same name and for the same
// reason as mixerNodeName.
const wholeNodePath = "/"

// checkMixerNodeIsWhole reports an error unless the frame carries the mixer
// node at path "/". See GetMixerSnapshot for why this is checked here.
func checkMixerNodeIsWhole(raw []byte) error {
	var frame struct {
		Status []json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		return fmt.Errorf("wslcomms: the status frame is not a JSON object: %w", err)
	}
	for _, element := range frame.Status {
		var entry struct {
			Node string `json:"node"`
			Path string `json:"path"`
		}
		if err := json.Unmarshal(element, &entry); err != nil {
			continue
		}
		if entry.Node != mixerNodeName {
			continue
		}
		if entry.Path == wholeNodePath {
			return nil
		}
		return fmt.Errorf(
			"wslcomms: the status frame carries %s only as a %q subtree update, not its whole state; "+
				"a partial frame would render as an empty mixer, i.e. \"nothing is in the clean feed\"",
			mixerNodeName, entry.Path)
	}
	return fmt.Errorf("wslcomms: the status frame contains no %s node", mixerNodeName)
}

// ---------------------------------------------------------------------------
// The arm gate
// ---------------------------------------------------------------------------

// ArmMixer opens the Go-side write window and returns when it closes.
//
// It builds the switcher_controller socket on first use, because
// mixer.NewController opens a connection that can change a live clean feed and
// that belongs on the operator arming the drawer rather than on application
// start. The first dial is synchronous and its failure is returned: MEASURED,
// a stale token is refused with HTTP 401 at the upgrade, and an operator arming
// against bad credentials must be told now rather than when they press Apply.
//
// ARMING CHANGES NOTHING ON THE MIXER. It only permits a later SendMixerCommands
// to. The window auto-clears after mixer.ArmWindow whatever anybody does or
// forgets to do, so an operator who arms and walks away ends with a shut gate.
//
// Arming again extends the window rather than stacking one.
//
// The bound ArmMixer arms as the LOCAL WebView2 seat; a remote seat arms as its
// own connection id through armMixerFrom (app_remote.go). Whichever seat armed
// is the only one SendMixerCommands will accept a write from — see mixArmedBy
// and sendMixerCommandsFrom — so two controllers cannot cross-authorise a write
// to the live clean feed.
func (a *App) ArmMixer() (MixerArmState, error) {
	return a.armMixerFrom(localClientID)
}

// armMixerFrom is ArmMixer's body, recording WHICH seat armed so the write gate
// can enforce arm-ownership. clientID is localClientID for the local window and
// the connecting seat's id for a remote one.
func (a *App) armMixerFrom(clientID string) (MixerArmState, error) {
	if a.closing.Load() {
		return MixerArmState{}, errShuttingDown
	}

	// Both of these are read and released BEFORE mixMu is taken, which is what
	// keeps mixMu a leaf lock. See app.go's header.
	a.ctlMu.Lock()
	client := a.client
	a.ctlMu.Unlock()
	if client == nil {
		return MixerArmState{}, errMixerNoControlPlane
	}
	token := client.Token()
	if token == "" {
		return MixerArmState{}, errMixerNotSignedIn
	}

	host, err := mixerHost(a.snapshotConfig().M2LXHost)
	if err != nil {
		return MixerArmState{}, err
	}

	a.mixMu.Lock()
	defer a.mixMu.Unlock()

	if a.mixCtl == nil {
		ctl, err := a.dialMixerController(host, token)
		if err != nil {
			return MixerArmState{}, fmt.Errorf("wslcomms: opening the mixer write path: %w", err)
		}
		a.mixCtl = ctl
	}

	// Record the arming seat under the same lock as the controller it authorises.
	// A second arm from a DIFFERENT seat takes ownership — the window is shared
	// (one controller, one desk), so the last arm wins and the drift is bounded by
	// mixer.ArmWindow; the point of ownership is that a write needs the seat that
	// armed most recently, not that arming is exclusive.
	a.mixArmedBy = clientID

	until := a.mixCtl.Arm(mixer.ArmWindow)
	return MixerArmState{
		Armed:         true,
		ArmedUntil:    until,
		WindowSeconds: int(mixer.ArmWindow / time.Second),
	}, nil
}

// DisarmMixer shuts the write window immediately and releases the socket.
//
// It closes the controller rather than only disarming it, for the same reason
// ArmMixer builds it late: a switcher_controller socket that exists is a socket
// that can be written to, and there is no reason for one to outlive the
// operator's intention to use it. mixer.Controller.Close also disarms, so the
// gate is shut even if the close itself reports a problem.
//
// Idempotent, and not an error when nothing was armed: the drawer disarms on
// close, on destroy and on the host telling it to, and every one of those
// arrives whether or not it was ever armed.
func (a *App) DisarmMixer() error {
	return a.closeMixerController()
}

// closeMixerController disarms and releases the mixer write path. It is
// DisarmMixer's body and is also called by teardown, which is why it is
// separate: teardown must not depend on a bound method's error contract.
func (a *App) closeMixerController() error {
	a.mixMu.Lock()
	ctl := a.mixCtl
	a.mixCtl = nil
	// Clear the arming seat along with the controller: once the socket is gone
	// there is nothing to own, and the next arm re-establishes ownership from
	// scratch. Leaving a stale owner would let a disarmed-then-rearmed window be
	// judged against a seat that no longer holds it.
	a.mixArmedBy = ""
	a.mixMu.Unlock()

	if ctl == nil {
		return nil
	}
	// Disarm before Close so that the gate is shut even if Close is the call
	// that fails. Both are idempotent and safe on an already-closed controller.
	ctl.Disarm()
	if err := ctl.Close(); err != nil {
		return fmt.Errorf("wslcomms: closing the mixer write path: %w", err)
	}
	return nil
}

// dialMixerController builds the write controller, using the test seam when one
// is installed. mixMu is held by the caller.
func (a *App) dialMixerController(host, token string) (mixer.Controller, error) {
	if a.mixerDial != nil {
		return a.mixerDial(host, token)
	}
	return mixer.NewController(host, token)
}

// mixerHost reduces the configured M2L-X host to the bare host[:port]
// mixer.NewController requires.
//
// The port is KEPT — an instance published on a non-default port is still one
// host — while a scheme and any path are removed, because config.Config's
// M2LXHost is documented as accepting an explicit scheme (internal/config,
// EffectiveSRTHost) and mixer.NewController rejects one rather than guessing.
//
// An explicit "http://" is refused rather than upgraded. It means cmd/mockm2lx,
// which speaks plain HTTP and has no switcher_controller endpoint; silently
// dialling wss:// to it would produce a handshake error that named the wrong
// problem.
func mixerHost(raw string) (string, error) {
	h := strings.TrimSpace(raw)
	if h == "" {
		return "", errMixerNoControlPlane
	}
	const insecure, secure = "http://", "https://"
	if len(h) >= len(insecure) && strings.EqualFold(h[:len(insecure)], insecure) {
		return "", fmt.Errorf(
			"wslcomms: the mixer write path is wss-only, but the M2L-X host is configured as %q — "+
				"an explicit http:// host is the mock, which has no switcher_controller endpoint", raw)
	}
	if len(h) >= len(secure) && strings.EqualFold(h[:len(secure)], secure) {
		h = h[len(secure):]
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	h = strings.TrimSpace(h)
	if h == "" {
		return "", fmt.Errorf("wslcomms: the M2L-X host %q has no host part", raw)
	}
	return h, nil
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// SendMixerCommands writes to the mixer. It is the ONLY method on this object
// that can change a live desk.
//
// Every command goes through mixer.Controller.Send, which refuses with
// mixer.ErrDisarmed unless ArmMixer opened a window that has not expired, and
// which builds every envelope before it writes any of them so a malformed batch
// cannot half-apply. Nothing here duplicates those checks and nothing here
// works around them.
//
// A nil return means SENT, NOT APPLIED. Two commands in this vocabulary are
// known to accept a write and ignore it, and both read back exactly as sent, so
// the drawer confirms from a following snapshot and must not report success on
// this method returning. See mixer.Controller.Send.
//
// An empty batch is refused rather than treated as a no-op. mixer.Send permits
// zero commands deliberately — a caller that computed "nothing to correct"
// should not have to arm first — but nothing reaches THIS method except a
// deliberate write from the drawer's Apply path, so an empty one is a bug in
// the caller and should say so rather than resolve as though it had written.
func (a *App) SendMixerCommands(cmds []MixerCommand) error {
	return a.sendMixerCommandsFrom(localClientID, cmds)
}

// sendMixerCommandsFrom is SendMixerCommands' body, carrying the id of the
// calling seat so arm-OWNERSHIP can be enforced: the write is refused unless the
// caller is the seat that armed. The bound SendMixerCommands passes
// localClientID; a remote seat's id is passed by the dispatcher (app_remote.go).
//
// EVERY refusal carries mixer.ErrDisarmed — not armed, disarmed since, window
// expired, or armed by ANOTHER seat all read as the same sentinel — so a caller
// deciding whether a write landed has one condition to test, and a foreign-seat
// refusal cannot be mistaken for success. That the refusal is ErrDisarmed and
// not a new sentinel is deliberate: the whole write gate is "is there an open
// window I own", and a seat that does not own the window is, for its purposes,
// disarmed.
func (a *App) sendMixerCommandsFrom(clientID string, cmds []MixerCommand) error {
	if len(cmds) == 0 {
		return errors.New("wslcomms: SendMixerCommands: no commands supplied")
	}

	decoded, err := decodeMixerCommands(cmds)
	if err != nil {
		return err
	}

	a.mixMu.Lock()
	ctl := a.mixCtl
	owner := a.mixArmedBy
	a.mixMu.Unlock()

	if ctl == nil {
		// Never armed, or disarmed since. Reported as ErrDisarmed rather than
		// as a distinct "no controller" error so that every refusal of a write
		// carries the same sentinel, whichever of the two gates shut first.
		return fmt.Errorf("%w: press Arm in the mixer drawer before making changes", mixer.ErrDisarmed)
	}
	if owner != clientID {
		// Armed by a DIFFERENT seat. This is the arm-ownership gate: with two
		// controllers, one operator's arm must not authorise the other's write to
		// the live clean feed. Same sentinel, so a write refused for lack of
		// ownership is indistinguishable from a write refused for lack of an arm —
		// both mean "you do not currently hold the gate".
		return fmt.Errorf("%w: the mixer was armed by another seat; press Arm here before making changes",
			mixer.ErrDisarmed)
	}

	ctx, cancel := context.WithTimeout(a.rootCtx, mixerSendTimeout)
	defer cancel()
	return ctl.Send(ctx, decoded...)
}

// decodeMixerCommands converts the wire shapes into internal/mixer commands.
//
// The whole batch is decoded before any of it is returned, so a bad command at
// index 3 cannot be discovered after the first three have been written. That
// property is also enforced inside mixer.Send; it is repeated here only so that
// a decode failure never reaches the socket at all.
func decodeMixerCommands(cmds []MixerCommand) ([]mixer.Command, error) {
	out := make([]mixer.Command, 0, len(cmds))
	for i, c := range cmds {
		cmd, err := decodeMixerCommand(c)
		if err != nil {
			return nil, fmt.Errorf("wslcomms: command %d of %d (%q): %w", i+1, len(cmds), c.Kind, err)
		}
		out = append(out, cmd)
	}
	return out, nil
}

// decodeMixerCommand converts one wire command.
//
// json.Unmarshal is given the args object directly, because the field tags on
// the internal/mixer types ARE the wire shapes documented there. DisallowUnknownFields
// is used so that a misspelled argument — "output" for "outputs" — is refused
// instead of decoding to a zero value and sending a set_routing with an empty
// bus set, which would un-route the strip from programme as well as from the
// clean feed.
func decodeMixerCommand(c MixerCommand) (mixer.Command, error) {
	decode := func(dst any) error {
		if len(c.Args) == 0 {
			return errors.New("no args supplied")
		}
		dec := json.NewDecoder(strings.NewReader(string(c.Args)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(dst); err != nil {
			return fmt.Errorf("args could not be read: %w", err)
		}
		return nil
	}

	switch c.Kind {
	case "setRouting":
		var v mixer.SetRouting
		if err := decode(&v); err != nil {
			return nil, err
		}
		return v, nil
	case "setInputMuted":
		var v mixer.SetInputMuted
		if err := decode(&v); err != nil {
			return nil, err
		}
		return v, nil
	case "setOutputMuted":
		var v mixer.SetOutputMuted
		if err := decode(&v); err != nil {
			return nil, err
		}
		return v, nil
	case "setChFader":
		var v mixer.SetChFader
		if err := decode(&v); err != nil {
			return nil, err
		}
		return v, nil
	case "setCompLimit":
		var v mixer.SetCompLimit
		if err := decode(&v); err != nil {
			return nil, err
		}
		return v, nil
	default:
		return nil, fmt.Errorf("unknown command kind %q", c.Kind)
	}
}

// ---------------------------------------------------------------------------
// The golden snapshot
// ---------------------------------------------------------------------------

// GetMixerGolden returns the saved golden snapshot, or nil when the operator
// has never saved one.
//
// NIL IS A NORMAL STATE and the drawer renders it as "no golden saved". It must
// never be turned into an empty Snapshot here: an empty golden compares clean
// against everything, so the drift panel would report "no differences" for ever
// — the exact silent failure internal/mixer's golden.go warns about.
//
// An unreadable or corrupt file is reported as an error rather than as nil, for
// the same reason: "there is no baseline" and "the baseline could not be read"
// look identical on a drift panel and are not the same thing.
func (a *App) GetMixerGolden() (*mixer.Snapshot, error) {
	path, err := mixerGoldenPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("wslcomms: reading the saved mixer golden state from %s: %w", path, err)
	}
	var snap mixer.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf(
			"wslcomms: the saved mixer golden state in %s is unreadable, so there is no baseline to compare against: %w",
			path, err)
	}
	return &snap, nil
}

// SetMixerGolden saves a snapshot as the golden baseline.
//
// IT WRITES APPLICATION STATE ONLY and does not touch the mixer, which is why
// it is safe to call while disarmed. It is an operator gesture and never
// automatic: silently re-baselining would erase the very difference the drawer
// exists to show.
//
// The write is atomic — temp file in the same directory, then rename — so that
// a crash or a power cut mid-save leaves either the old baseline or the new
// one, never a truncated file that reads as "no differences".
func (a *App) SetMixerGolden(snapshot *mixer.Snapshot) error {
	if snapshot == nil {
		return errors.New("wslcomms: SetMixerGolden: no snapshot supplied")
	}
	if len(snapshot.Strips) == 0 {
		// A golden with no strips compares clean against every future state,
		// so the drift panel would sit at "no differences" for the rest of the
		// event. Refusing is the only outcome that cannot mislead.
		return errors.New(
			"wslcomms: SetMixerGolden: the snapshot has no channel strips, so it would compare clean " +
				"against everything — nothing was saved")
	}

	path, err := mixerGoldenPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("wslcomms: creating %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("wslcomms: encoding the mixer golden state: %w", err)
	}

	tmp, err := os.CreateTemp(dir, mixerGoldenFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("wslcomms: creating a temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("wslcomms: writing %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("wslcomms: syncing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("wslcomms: closing %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("wslcomms: renaming %s to %s: %w", tmpPath, path, err)
	}
	renamed = true
	return nil
}

// mixerGoldenPath returns the golden snapshot's absolute path, beside
// config.json. It does not create the directory.
func mixerGoldenPath() (string, error) {
	cfgPath, err := config.Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfgPath), mixerGoldenFileName), nil
}
