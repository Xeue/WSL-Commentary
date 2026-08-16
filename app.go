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
// require cgo, though what that buys differs by platform. Wails on Windows is
// pure Go, so this file compiles and its tests run at Gate A with CGO_ENABLED=0
// against the internal/gst stub:
//
//	go test -tags dev . -count=1
//
// Wails on macOS is not pure Go — its whole darwin frontend reaches Cocoa and
// WKWebView through Objective-C — so with CGO_ENABLED=0 the tagged build fails
// inside Wails rather than here, and the same tests need a second build tag and
// a linker flag that Windows never has to think about:
//
//	CGO_LDFLAGS="-framework UniformTypeIdentifiers" CGO_ENABLED=1 \
//		go test -tags "dev gststub" . -count=1
//
// gststub is what makes that line EQUIVALENT to the Windows line above rather
// than a different test. internal/gst selects its halves on cgo, not on the
// platform — gst_cgo.go is `cgo && !gststub` — so turning cgo on to satisfy
// Wails also swaps the real go-gst pipeline in underneath these tests, and they
// reference stub-only symbols: gst.StubPipeline at app_test.go:1165 and ten
// more. Without the tag the package does not COMPILE, never mind touch a device.
// With it, cgo is on for Wails and off for GStreamer, which is exactly the split
// this gate wants.
//
// The framework flag is upstream Wails' omission, not ours. Its darwin frontend
// references UTType but the package declares no matching #cgo LDFLAGS, so the
// Wails CLI injects the flag from outside, at
// third_party/wails-v2.13.0/pkg/commands/build/base.go:349. `go test` is not the
// Wails CLI and injects nothing, so the link — not the compile — fails with
// "Undefined symbols for architecture arm64: _OBJC_CLASS_$_UTType".
// build/ship-darwin.sh:176 already exports the same flag for the same reason;
// this is that flag, at test time. Note the failure only reaches a build that
// LINKS: `go vet` type-checks and links nothing, which is why main.go's vet
// instruction needs neither of these and is correct as it stands.
//
// Gate A itself is untagged, reaches main_nocgo.go instead of this file, and is
// unaffected on both platforms.
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
//	ListEvents()                []m2lx.Event               caller: WP-5b
//	CredentialStoreName()       string                     caller: WP-5b
//
// and the six added for the mixer drawer, which live in app_mixer.go:
//
//	GetMixerSnapshot()          mixer.Snapshot             caller: WP-M4
//	ArmMixer()                  MixerArmState              caller: WP-M4
//	DisarmMixer()               error                      caller: WP-M4
//	SendMixerCommands(cmds)     error                      caller: WP-M4
//	GetMixerGolden()            *mixer.Snapshot            caller: WP-M4
//	SetMixerGolden(snapshot)    error                      caller: WP-M4
//
// and the five added for the SRT return monitor, which live in app_return.go:
//
//	ListOutputDevices()         []gst.Device               caller: WP-5b
//	StartReturn() / StopReturn() error                     caller: WP-5b
//	GetReturnState()            gst.ReturnState            caller: WP-5b
//	IsSRTReturnSelected()       bool                       caller: WP-5b
//
// and the five added for the SRT PICTURE, which live in app_picture.go:
//
//	StartPicture() / StopPicture()      error              caller: WP-5b
//	GetPictureState()                   gst.PictureState   caller: WP-5b
//	SetPictureRect(x,y,w,h,ratio)       error              caller: WP-5b
//	SetPictureVisible(visible)          error              caller: WP-5b
//
// and the seven added for the M2L-X INSTANCE PRESETS, which live in
// app_presets.go — the TWENTIETH to TWENTY-SIXTH, recorded in CONTRACT.md's
// bound-surface table with the same deliberateness as the rest:
//
//	ListPresets()                       []presets.Summary       caller: WP-5b
//	SavePreset(name)                    presets.Summary         caller: WP-5b
//	ApplyPreset(id)                     *config.Config          caller: WP-5b
//	RenamePreset(id, name)              error                   caller: WP-5b
//	DeletePreset(id, alsoCreds)         error                   caller: WP-5b
//	GetActivePreset()                   presets.ActiveRecord    caller: WP-5b
//	GetPresetCredentialStatus()         PresetCredentialStatus  caller: WP-5b
//
// and the four added for the VIDEO LEG — the DeckLink camera and the operator's
// confidence monitor. All four are HOST-ONLY, which is unusual enough on this
// surface to be worth saying here rather than only in remoteAllowlist:
//
//	SetVideoSource(source)              error                   caller: NONE — see below
//	SetDeckLinkPreviewEnabled(enabled)  error                   caller: NONE — see below
//	SetPreviewRect(x,y,w,h,ratio)       error                   caller: WP-5b
//	SetPreviewVisible(visible)          error                   caller: WP-5b
//
// SetVideoSource is the only method anywhere on this surface that decides WHAT A
// BROADCAST SWITCHER RECEIVES, which is why it is a method of its own rather
// than one more field somebody has to remember to guard inside SaveConfig. The
// other two rectangle methods concern a native window on the screen of whoever
// is at this machine, and are host-only for the reason SetPictureRect and
// SetPictureVisible are.
//
// THE FIRST TWO HAVE NO CALLER, DELIBERATELY, and the annotation says so rather
// than naming one that does not exist. The Settings screen writes both fields
// through its single Save, with the two controls DISABLED while sending, so the
// refusal these methods make is unreachable by construction from the only page
// that could reach it. They are kept because they are the DECLARATION that
// remoteAllowlist enforces: a host-only classification on a method is what
// TestRemoteHostOnlySet pins, and deleting them would leave the two most
// dangerous fields on this surface reachable only through SaveConfig, whose
// host-only-ness is a runtime argument in refuseRemoteVideoLegChange rather than
// a table entry. If a future page wants to write either field on change instead
// of on Save, these are what it calls and nothing else has to move.
//
// HOST-ONLY IS NOT SELF-ENFORCING HERE, and the gap is closed rather than
// noted: SaveConfig is remotely reachable and is a whole-document write, so
// refuseRemoteVideoLegChange refuses a remote save that would change either of
// the two configuration fields. Without it the classification would be a
// decoration.
//
// Wails binds every EXPORTED method of *App, so this list and the set of
// exported methods are the same thing: adding one silently widens the contract
// with WP-5a and WP-5b. Everything internal below is lower-case for that reason
// and not merely by habit. There is deliberately no getter for a secret — a
// secret goes into the OS credential store and never comes back out across this
// boundary. GetPresetCredentialStatus is the one recorded, deliberate
// narrowing of that rule: it reports whether a credential EXISTS for the
// active preset scope — three booleans, never a value — because after applying
// a preset the operator has to know whether to type the passwords, and the
// frontend's "set this session" badge is a lie for a scope never written to in
// this run. The reasoning lives with the type in app_presets.go.
//
// GetStatusKeyCandidates is the EIGHTH, added after the surface was declared
// frozen, and it is called out here rather than slipped in: with no statusKey
// the three WebSocket-derived lamps can only say NO STATUS, and no M2L-X
// endpoint will name the node (specification open question 5). The alternative
// to this method was leaving the operator to guess, which is what they were
// doing. It is read-only and returns a suggestion the operator must confirm.
//
// ListEvents is a later, deliberate widening of the same kind, recorded here
// rather than slipped in. It is read-only — GET /api/events/overview on the
// already-signed-in client — and it exists so an operator need never know their
// event id: the frontend auto-selects the sole event and offers a picker only
// when there is a real choice, superseding the id an operator used to recover by
// pasting a live-operation URL. Like GetStatusKeyCandidates it acts on nothing;
// it hands the frontend a list to render.
//
// CredentialStoreName is the third of that kind and the smallest: it returns a
// per-platform NAME — "Windows Credential Manager" or "the macOS login
// Keychain" — so a dialog can say where the passwords it is about to delete
// live. It was added with the macOS port and was, until now, the one exported
// method with no row in remoteAllowlist and no row in app_test.go's frozen
// list, which is why both drift guards were failing; classifying it is what
// this line and those two rows are. It is NOT host-only, deliberately: a remote
// seat renders the same delete-a-preset dialog, and refusing it there would
// send that operator back to the fallback string — the Windows name on a Mac,
// which is the exact defect the method exists to remove. It returns the name of
// the store and never a credential, so the "no secret crosses this boundary
// outbound" rule is untouched.
//
// The six mixer methods are the NINTH to FOURTEENTH, and the count was kept
// down on purpose: they are the smallest set that lets the drawer read state,
// open and shut the Go-side write window, write, and keep a golden baseline.
// Three of them are read-only. Of the three that are not, two only move the arm
// gate — which changes nothing on the mixer, it only permits a later write —
// and exactly ONE, SendMixerCommands, can change a live desk. That method
// reaches the mixer solely through mixer.Controller.Send, which refuses with
// mixer.ErrDisarmed unless ArmMixer has opened an unexpired window; there is no
// second path, and app_mixer.go says why adding one would be a bug rather than
// a feature. SetMixerGolden writes a local file and never touches the mixer.
//
// The five return methods are the FIFTEENTH to NINETEENTH. Four of the five are
// read-only or purely local; StartReturn and StopReturn open and close a second
// SRT session that only ever RECEIVES. Nothing in app_return.go can write to
// M2L-X, and nothing in it can reach the contribution session — the two hold
// different locks, drive different pipelines and share no state, which is the
// property that stops a headphone monitor taking a live feed off air.
//
// IsSRTReturnSelected is the one that looks redundant next to GetConfig and is
// not: it is the single place that decides which return path owns the
// headphones, and the frontend has to agree with Go about that or the
// commentator hears the same programme twice a few hundred milliseconds apart.
// Deriving it from a config string on both sides of the boundary is how the two
// come to disagree.
//
// The event list below is deliberately NOT extended for the mixer. Mixer
// snapshots are pulled by GetMixerSnapshot rather than pushed, because the
// status socket only carries a complete document once per connection and a
// pushed "latest known" frame would be exactly the cached stale frame the
// drawer's freshness gate is built to reject. See app_mixer.go.
//
// It IS extended for the return, and for the opposite reason: the return
// monitor's state changes on its own, from a reconnect loop nobody polls, and a
// RETURN lamp that only updated when the frontend happened to ask would show
// green through an outage the commentator can hear.
//
// # Events emitted Go to JS
//
//	"status"              an m2lx.Status
//	"sender"              a sender.State
//	"return"              a gst.ReturnState
//	"error"               a string
//	"statusKeyCandidates" a []m2lx.StatusKeyCandidate
//	"levels"              a levelsPayload {peak:[...], rms:[...]} — the input meters
//	"channelLevels"       a levelsPayload, one entry per CAPTURE channel — the picker
//	"channelMap"          a channelMapPayload {inputChannels, map, isDefault}
//	"signal"              a signalPayload {state, flaps} — the video capture's input lock
//
// The "error" event carries first-run configuration problems, gst.Init failures,
// sign-in failures, and — rate-limited, because the sender retries forever — the
// reason each connection attempt failed. See connectErrorReporter.
//
// Headphone enumeration and selection for the WEBRTC return are JavaScript-side
// only, through enumerateDevices and setSinkId. The SRT return needs a
// PLATFORM device identity instead — a WASAPI IMMDevice endpoint ID on Windows,
// a CoreAudio device UID on macOS — which no browser API can produce, so
// ListOutputDevices enumerates those on the Go side and config keeps both
// identifiers under two names. They are not interchangeable; see
// config.Config.HeadphoneEndpointID.
//
// The macOS half is a string for a reason worth stating where somebody will
// read it. osxaudiosink's own "device" property is an integer AudioDeviceID
// allocated by coreaudiod at enumeration time and NOT stable across a reboot or
// a replug, so what is stored and what crosses this boundary is the stable UID
// ("BuiltInSpeakerDevice", "NDIAudio", a UUID); internal/gst resolves it to the
// integer when it opens the pipeline. An integer must never reach config.json.
//
// # Lifecycle: one context tree, one shutdown order
//
// Six things run concurrently in this process: the Wails event loop, the status
// WebSocket watcher, the m2lx client's token-refresh timer, the sender's two
// goroutines, the return monitor's two, and the event pump below. Every one of
// them is rooted in a single context created by NewApp, and shut down in exactly
// one order by teardown:
//
//  0. the closing flag is raised, so that a bound method already in flight on a
//     Wails message-handler goroutine cannot build a new session or a new
//     control plane behind the teardown that has just walked past them;
//  1. the ROOT CONTEXT is cancelled, which is the one step that cannot block
//     and so is the one step that must never be behind a step that can. It ends
//     every context-bound piece of work in the process at once: the mixer
//     drawer's in-flight GetMixerSnapshot dials, a KVS credential fetch, the
//     control plane generation whose context derives from it, the event pump;
//  2. the sender  — Stop blocks until the pipeline is at NULL, so the audio
//     capture device and the SRT socket are released before anything else moves;
//  3. the return monitor — same reason, and deliberately AFTER the sender: both
//     release an audio device and an SRT socket, and if the whole sequence
//     overruns shutdownTimeout the process exits regardless, so the one that
//     must already have finished is the contribution path;
//  4. the mixer write path — it is closed BEFORE the control plane, because a
//     Send in flight is a write to a live desk and must not be left racing a
//     teardown; Close also disarms, so the gate is shut whatever happens next;
//  5. the status watcher — its context is cancelled and its goroutines joined;
//  6. the token refresh — the m2lx client's own background goroutine, which is
//     bounded by a context of the client's own and so survives step 5; only
//     m2lx.Client.Close stops it (see stopControlPlaneLocked);
//  7. a WaitGroup join of everything left.
//
// Step 0 is what makes the order deterministic rather than merely usual. Both
// races it closes are decided the same way whichever goroutine wins the lock:
// a Start that got sessMu first completes and is then stopped by step 2, and a
// Start that arrives after step 2 is refused. There is no interleaving in which
// a pipeline outlives teardown.
//
// # EVERY STEP HAS ITS OWN BOUND, AND THAT IS THE POINT
//
// The order above used to be a plain sequence inside one overall timeout. That
// is not the same thing, and the difference is what left a wslcomms running in
// Task Manager after a match: the timeout bounded the WAIT, not the WORK, so the
// first step that would not return took the whole budget and EVERY STEP BEHIND
// IT WAS SIMPLY NEVER RUN. Measured, with a return monitor that could not be
// stopped: after the twenty seconds were up the mixer's write socket was still
// open and still armed, the status watcher and the token-refresh timer were
// still running, and the root context had never been cancelled — so every
// GetMixerSnapshot dial the drawer had in flight was still live too. Nothing
// had been released by a shutdown that had already given up.
//
// So each step is bounded on its own and a step that overruns is ABANDONED, in
// so many words, and the next one runs anyway. The steps take disjoint locks —
// see the lock order below — so a sender still wedged in cgo cannot stop the
// return monitor, the mixer socket or the watcher from being closed behind it.
//
// # WHAT IS ABANDONED, AND WHY THAT IS SAFE
//
// Only ever a GStreamer state change that will not complete. gst_element_set_state
// takes no timeout and runs on the calling goroutine inside cgo, so when it
// wedges — an audio endpoint that will not release, whether that is a WASAPI
// endpoint or a CoreAudio device, or an SRT socket mid-handshake — there is no
// Go-side context, deadline or cancellation that can reach it.
// internal/gst says so at length; see the timeout constants there.
//
// Abandoning it costs nothing that process exit does not already recover. The
// audio endpoint, both SRT sockets and the M2L-X fan-out slot are all released
// by the OS when the process goes, whether or not GStreamer was asked politely
// first. Nothing is buffered waiting to be written: config.json and the mixer
// golden file are both written synchronously through a temp file and a rename,
// long before any of this.
//
// It is safe HERE and would not be safe anywhere else, which is why the bound
// lives in teardown rather than inside internal/gst. Everywhere else, "the
// pipeline has reached NULL" is what stops the next Start opening a second
// pipeline on the same endpoint. At teardown there is no next Start: the
// closing flag is raised at step 0 and both Start paths refuse.
//
// # AND THEN THE PROCESS EXITS, WHATEVER IT TAKES
//
// A teardown that abandoned something has deliberately stopped being able to
// account for a thread. Returning normally hands that thread to the Go runtime's
// ordinary exit path, and on BOTH supported platforms that path runs code
// belonging to the media libraries the abandoned thread is still inside. The
// mechanisms have nothing in common; the outcome does.
//
// On Windows the ordinary exit is ExitProcess: it kills the other threads first
// and then runs the detach handler of every loaded DLL under the loader lock,
// and GStreamer, WASAPI, D3D11 and COM are all in that set. A detach handler
// that takes a lock the killed thread was holding deadlocks, and the process
// never dies.
//
// On macOS the ordinary exit is exit(3), which runs the atexit handlers and the
// C++ static destructors — MEASURED, on os.Exit and on syscall.Exit alike, and
// measured also to run them while every other thread is STILL RUNNING rather
// than after killing them as Windows does. The hooks are not hypothetical: with
// atexit interposed, creating srtsink registers libsrt's global destructors,
// among them srt::CUDTUnited::~CUDTUnited tearing down the global socket table
// and destroying the mutex that guards it. So the macOS shape is a race against
// a live abandoned thread rather than a deadlock against a dead one — and it was
// measured that a termination hook which blocks hangs the exit for ever, with no
// timeout and nothing on the Go side able to reach it. exit_darwin.go carries
// the full measurement.
//
// Neither can be made safe from here, and both fail in precisely the way being
// fixed: a window that has gone and a process that has not. So when something
// has been abandoned the teardown does not return and hope; it ends the process
// itself, by the one call the OS in question cannot refuse. See hardExit,
// exit_windows.go and exit_darwin.go.
//
// A process that will not exit is a support call, so a wedged pipeline loses the
// race rather than the window.
//
// # Five locks, in this order
//
//	sessMu -> cfgMu     (Start reads the config while holding the session lock)
//	ctlMu               (never held with either of the others)
//	senderMu            (leaf; never held while any other lock is taken)
//	mixMu               (leaf; never held while any other lock is taken)
//	retMu -> cfgMu      (StartReturn reads the config while holding it)
//	retStateMu          (leaf; never held while any other lock is taken)
//
// retMu is a SIXTH and it is deliberately disjoint from sessMu rather than
// nested under it. The two guard two independent media sessions that share no
// device, no socket and no pipeline, and the whole reason app_return.go exists
// as a separate path is that a monitor must not be able to block the feed. A
// single lock over both would have made StartReturn wait on a wedged
// gst.Pipeline.Stop, which is precisely the coupling that was designed out.
//
// retStateMu is a SEVENTH, and it is split off retMu for a concrete reason
// rather than for symmetry: StopReturn holds retMu across the join of the
// state-forwarding goroutine, and that goroutine writes lastReturn. One lock
// over both deadlocks the first Stop that lands while a transition is in
// flight. senderMu is split off sessMu for exactly this reason and it is worth
// two locks in both places.
//
// No goroutine started here takes any of the five, so the WaitGroup joins
// performed under ctlMu and sessMu cannot deadlock against their own workers.
//
// mixMu is a leaf by construction rather than by luck: every mixer method needs
// the m2lx client or the configuration, and reads both — under ctlMu and cfgMu
// respectively — and releases them BEFORE taking mixMu. The one call made while
// holding mixMu that can block is mixer.Controller.Close during teardown, and
// nothing that runs under any other lock ever waits on it.
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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/options"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
	"wslcomms/internal/kvs"
	"wslcomms/internal/m2lx"
	"wslcomms/internal/mixer"
	"wslcomms/internal/presets"
	"wslcomms/internal/remote"
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

	// EventReturn carries a gst.ReturnState behind the RETURN lamp: the state
	// of the SRT return monitor. It is emitted only when returnSource is "srt";
	// on the WebRTC path nothing produces it and the lamp is the browser's
	// business.
	EventReturn = "return"

	// EventError carries a human-readable error string for display.
	EventError = "error"

	// EventStatusKeys carries []m2lx.StatusKeyCandidate: the switcher_status
	// nodes that started streaming while our feed was coming up, offered to the
	// Settings screen as suggestions for a statusKey the operator has not set.
	// It is emitted only while a discovery is running, and never causes anything
	// to be saved — see App.GetStatusKeyCandidates.
	EventStatusKeys = "statusKeyCandidates"

	// EventLevels carries a levelsPayload: the send pipeline's own peak/RMS
	// measurement of the commentary audio, per channel in dBFS, behind the
	// input meters beside the big picture. Emitted only while a session is
	// running — throttled to at most one per levelsMinInterval, because a
	// meter must degrade to SLOWER under pressure, never to bursty — plus one
	// final all-silence frame when the session ends, so the meters fall to
	// nothing rather than freezing at the last level and reading as live.
	EventLevels = "levels"

	// EventChannelLevels carries a levelsPayload measured at a DIFFERENT POINT
	// from EventLevels, and the two must never be treated as substitutes.
	//
	// EventLevels is the stereo that is actually encoded and sent. This is the
	// capture device's OWN channels — sixteen of them on a DeckLink card —
	// measured upstream of the audioconvert that mixes them down to that stereo,
	// so it can say WHICH input the commentator is on and the programme meter
	// cannot. That question is the entire reason the routing grid is usable: the
	// operator asks the commentator to talk and watches which bar moves.
	//
	// TEN frames a second against the programme meter's twenty, deliberately. The
	// measurements are on gst.channelLevelIntervalNs; the short version is that
	// what is rationed is the webview bridge rather than GStreamer, and that
	// speech syllables run 150-250 ms so a talker is unmistakable at 10 Hz. There
	// is NO app-side throttle on this event for that reason — the element's own
	// interval already puts it below the floor a throttle would check against, so
	// a second one could only add jitter.
	//
	// It is emitted only while a session is running AND only when the capture
	// source presents unpositioned channels: internal/gst builds the element with
	// post-messages=false and arms it only when a mix matrix was written, so an
	// ordinary microphone produces not one frame of this for a whole match.
	EventChannelLevels = "channelLevels"

	// EventChannelMap carries a channelMapPayload: how many channels the capture
	// pad NEGOTIATED, the routing in force, and whether that routing is the
	// unchosen default.
	//
	// The width is the load-bearing half. The routing grid is sized from it and
	// from nothing else, because a mix matrix whose width does not match what the
	// pad negotiated does not degrade the feed — it stops it, measured, with the
	// capture chain dead before the next level message and every coefficient in
	// the matrix perfectly legal. What a DeckLink ADVERTISES is not that number:
	// the device publishes max-channels=16 whatever its element is configured to
	// produce. Zero is a normal value and means "nothing has negotiated" — before
	// the first Start, and after a session ends — and it draws no grid at all.
	//
	// It is emitted when a session starts (which is when the pad negotiates) and
	// when one ends, and replayed on domReady, so a Settings screen left open
	// across a START sizes its grid without being reopened.
	EventChannelMap = "channelMap"

	// EventSignal carries a signalPayload: whether the DeckLink video capture
	// has a locked input, debounced by internal/gst's signal watchdog.
	//
	// It is the one event that exists because NOTHING ELSE IN THIS APPLICATION
	// CAN TELL. A DeckLink that loses signal keeps emitting black frames at full
	// rate forever — no error, no EOS, the muxer never starves — so the sender
	// stays CONNECTED, the switcher reports a healthy correctly-formatted feed,
	// and the level meters keep moving on commentary audio that is untouched.
	// Every lamp on the screen is green and the picture going out is black. See
	// internal/gst/signalwatch.go for the measurements and for why the hold-offs
	// are asymmetric.
	//
	// It is emitted only while a session is running, only when the debounced
	// state CHANGES (or the raw reading has been rattling; see
	// gst.SignalReport.Flaps), plus one final UNKNOWN frame when the session
	// ends — for the same reason the levels event sends a silent frame, and with
	// the same consequence if it did not: a lamp left on for a pipeline that no
	// longer exists.
	//
	// gst.SignalUnknown IS NOT A FAULT and must not be drawn as one. It is the
	// state of every machine with no capture card in it, which is every machine
	// running this application today.
	EventSignal = "signal"
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
	// combined rate, and can only fill if the webview renderer — WebView2 on
	// Windows, WKWebView on macOS — has stopped reading entirely, which is
	// exactly the case the discarding is for.
	eventQueueDepth = 64

	// shutdownTimeout bounds the whole ordered teardown, and the four constants
	// under it divide that budget between the steps. a.Stop() and a.StopReturn()
	// are the two steps in it that can block on a pipeline; both are named below
	// and neither is silently assumed to be covered.
	//
	// IT IS THE SUM OF THE PER-STEP BUDGETS, NOT A SECOND OPINION ABOUT THEM.
	// It used to be the only bound there was, and one step that would not return
	// therefore consumed all of it and left every later step unrun — the mixer
	// socket still open and armed, the watcher still running, the root context
	// never cancelled. Each step now carries its own bound, and this is the
	// backstop for the arithmetic being wrong rather than for the work being
	// slow.
	//
	// # What the per-step budgets cover
	//
	// a.Stop's worst case is a gst.Pipeline.ReplaceSink already in flight. That
	// is synchronous by contract and internal/gst bounds it at
	// sinkStateChangeTimeout, ten seconds; add elementShutdownTimeout, five
	// seconds, for taking the pipeline to NULL. Fifteen seconds is that sum, and
	// senderStopBudget is fifteen seconds: the contribution feed is the path that
	// must be given every chance to finish, so it keeps the largest share and it
	// still goes first.
	//
	// a.StopReturn is normally prompt — the monitor sits in RECEIVING, its
	// backoff wait is cancelled rather than served, and Close is bounded at
	// elementShutdownTimeout — so returnStopBudget is two seconds, which is that
	// ordinary case with room to spare.
	//
	// # WHAT THEY DO NOT COVER, AND ARE NOT MEANT TO
	//
	// A StopReturn that lands while gst returnPipeline.Play is in flight WILL BE
	// CUT OFF. Play holds the pipeline lock across a DNS resolve
	// (hostResolveTimeout, three seconds) and then, per resolved address, a state
	// change to PLAYING (returnStateChangeTimeout, ten), a wait for the demuxer's
	// audio pad (returnAudioPadTimeout, ten) and a return to NULL
	// (elementShutdownTimeout, five) — thirty-three seconds for a single address
	// and more for a name with several. No value written here would contain that
	// without making a closed window hang for the best part of a minute.
	//
	// That is an accepted loss, not an oversight. The window has already gone; a
	// wslcomms left running after a match — in Task Manager, or in Activity
	// Monitor with its icon still in the Dock — is a support call. What is
	// abandoned is a headphone endpoint and an SRT socket, and process exit
	// releases both — including the M2L-X fan-out slot, which goes when the
	// socket closes whether or not GStreamer was asked politely.
	//
	// None of these figures bounds the SYNCHRONOUS half of a GStreamer state
	// change: gst_element_set_state takes no timeout and runs on the calling
	// goroutine, so a wedged audio endpoint hangs past any of this. That is what
	// the abandonment is for, and why a teardown that abandons anything ends the
	// process itself rather than returning into an exit path that would have to
	// step over the wedged thread. See teardown.
	// a.stopPictureForTeardown is two halves in sequence, and only the second
	// bounds itself. The monitor's Stop is ordinarily prompt for the same reasons
	// StopReturn's is, but that is a measurement, not a guarantee: it ends in a
	// synchronous state change, and the paragraph above applies to it in full.
	// The overlay window's Close does guarantee it — it posts a quit to its own
	// message thread and waits gst's overlayCloseBudget, two seconds, before
	// abandoning that thread and saying so. pictureStopBudget is four seconds,
	// which is that pair with room. See the Bounding note on
	// stopPictureForTeardown, which sets out which half is bounded by what and
	// why the timeout in picture_cgo.go is not the bound it looks like.
	//
	// The remote listener adds one more second (remoteStopBudget, in
	// app_remote.go): closing a TLS http.Server and its session goroutines is
	// prompt — no cgo, no device, only sockets and goroutines selecting on a
	// cancelled context — so a second is generous, and it takes the total to
	// twenty-five. Unlike the media stops it cannot wedge, so it is a term in the
	// sum rather than a wait expected to be cut off.
	shutdownTimeout = 25 * time.Second

	// senderStopBudget bounds step 2, a.Stop. Fifteen seconds is the sender's own
	// bounded worst case; see above.
	senderStopBudget = 15 * time.Second

	// returnStopBudget bounds step 3, a.StopReturn. Two seconds is the ordinary
	// case with room to spare; a Play in flight is expected to be cut off and is
	// accounted for above.
	returnStopBudget = 2 * time.Second

	// pictureStopBudget bounds the picture step: stop the monitor, then destroy
	// the overlay window. Four seconds is two for the pipeline and two for the
	// window's message thread; see the arithmetic above shutdownTimeout.
	//
	// The window half is the unusual one. Everything else in this teardown is
	// releasing a socket or a device, which the process exit would release
	// anyway. A window is on the operator's SCREEN, so an abandoned one is
	// visible until the process goes — which is why gst's overlay Close says in
	// its error that it abandoned the thread rather than returning a bare nil.
	pictureStopBudget = 4 * time.Second

	// mixerCloseBudget bounds step 4, closeMixerController. Closing a
	// switcher_controller socket is a Close and a goroutine join, both prompt.
	// The one thing that can stretch it is a reconnect dial already in flight,
	// which internal/mixer bounds at its own ten second dialTimeout — half the
	// whole shutdown budget for a socket the process is about to drop anyway, so
	// it is abandoned instead. Disarm happens before Close, so the write gate is
	// shut either way.
	mixerCloseBudget = 1 * time.Second

	// controlPlaneStopBudget bounds steps 5 and 6, stopControlPlaneLocked. Its
	// context is already cancelled by step 1, m2lx.Client.Close is bounded by
	// that same cancellation, and the join is of goroutines that all select on
	// it. One second is generous for work that should be instant.
	controlPlaneStopBudget = 1 * time.Second

	// rootJoinBudget bounds step 7, the join of everything rooted in rootCtx —
	// in practice the event pump, which exits on the cancellation step 1 already
	// performed. One second is generous for the same reason.
	rootJoinBudget = 1 * time.Second

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

	// conformFetchTimeout bounds the one REST call START makes to read the
	// switcher's configured video format (see conformFormat).
	//
	// It is SHORT, and deliberately shorter than internal/m2lx's own ten-second
	// httpTimeout, because the whole design of that read says so: the answer
	// improves the feed and its absence costs nothing that was not already
	// being paid, so it must never be the reason START takes noticeably longer
	// or — far worse — the reason START fails. An operator configuring twenty
	// minutes before kick-off, against an instance that is not up yet, must
	// still be able to go on air on the override.
	//
	// Three seconds is thirty-five times the measured cost. GET
	// /api/v1/switcher_configuration against the live matchH instance on
	// 2026-08-15, each call a fresh connection including the TLS handshake:
	// 72.2/85.0/94.7/153.2 ms (min/median/mean/max, n=6) for a 12108-byte
	// response. It is also short enough that an instance which has gone away
	// delays the pipeline by less than one rung of the sender's backoff ladder.
	conformFetchTimeout = 3 * time.Second

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
	// — the commentator's Dante endpoint unplugged, which errors the capture
	// source (wasapi2src, or osxaudiosrc on macOS) and makes every subsequent
	// ReplaceSink fail immediately — calls
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

	// levelsMinInterval is the App-side floor between two "levels" events: at
	// most twenty a second, matching the level element's own 50 ms interval.
	//
	// The producer already runs at that rate, so on a healthy day this drops
	// nothing. It exists for the unhealthy day: the pump discards OLDEST under
	// pressure, which is the right policy for edge-triggered state events but
	// the wrong shape for a meter — a meter that falls behind must degrade to
	// SLOWER, not to bursts of stale frames arriving together and painting a
	// jerky history of two seconds ago. Throttling at the producer keeps the
	// queue shallow so the frame that does arrive is recent. It also protects
	// every other event in the shared pump: at 20 Hz an unthrottled producer
	// could purge the 64-slot queue of sender transitions in three seconds of
	// renderer stall.
	levelsMinInterval = 50 * time.Millisecond
)

// levelsSilenceDB is the all-channels-silent level, in dBFS, of the zero-frame
// emitted when a session ends. It mirrors internal/gst's documented clamp
// floor — Levels values are clamped to -100, matching the mixer drawer's own
// digital-silence reading — so the falling meter lands on the same number the
// live meter uses for silence.
const levelsSilenceDB = -100

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
	// appDir is the directory holding the executable, already symlink-resolved by
	// main: the installation directory on Windows, and Contents/MacOS inside the
	// .app on macOS. It is where the bundled GStreamer and the default slate.png
	// live.
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
	// OnSecondInstanceLaunch off the single-instance handover — the goroutine
	// serving the named pipe on Windows, and on macOS a goroutine draining the
	// buffer an NSDistributedNotificationCenter observer writes to. Those are
	// unordered with respect to each other on both platforms — a second launch
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

	// store is the OS credential store — Windows Credential Manager, or the login
	// Keychain on macOS; internal/secrets picks — wrapped by app_presets.go's
	// scopedStore so that every read and write resolves through the ACTIVE
	// credential scope. It is stateless and needs no shutdown.
	store secrets.Store

	// credScope is the active credential scope: "" until an instance preset
	// with a scope of its own is applied, and then that preset's scope. It is
	// an atomic rather than a field under cfgMu because signInLoop reads the
	// M2L-X password on the control-plane goroutine while ctlMu is held, and
	// the lock order below says ctlMu is never held with cfgMu — a lock-free
	// read has no ordering to violate. Accessors in app_presets.go.
	credScope atomic.Pointer[string]

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

	// conformTo is the video format the NEXT pipeline will be built to: the
	// format the switcher says it is configured for, the operator's
	// videoFormatOverride when the instance did not answer, and nil when
	// neither could (internal/gst then applies its own 1920x1080p50 fallback).
	//
	// It is written by Start, immediately before startSession, and read by
	// senderOpts inside it. It is an ATOMIC and not a field under sessMu for a
	// lock-order reason, not a performance one: reading it needs the m2lx
	// client, which lives under ctlMu, and the lock order below says ctlMu is
	// never held with sessMu — so the read has to happen OUTSIDE startSession
	// and hand its answer in through something that needs no lock.
	//
	// Nil is the correct value on every path that never read one, which
	// includes every unit test that calls senderOpts directly: a nil here is a
	// zero gst.ConformTarget in PipelineOpts, which internal/gst documents as
	// "nothing is known" and resolves to exactly the format that was hardcoded
	// before this field existed.
	conformTo atomic.Pointer[gst.ConformTarget]

	// switcherFmt caches GetSwitcherFormat's answer, and switcherFmtAt is when
	// it was read. Both guarded by switcherFmtMu.
	//
	// THE CACHE IS ABOUT THE SETTINGS SCREEN AND NOT ABOUT COST. The REST call
	// behind it is 85 ms against a live instance and bounded at
	// conformFetchTimeout, and neither number would justify a cache. What
	// justifies it is the instance that is NOT up: three seconds of stall,
	// every time the operator opens Settings, on the exact screen they are
	// using twenty minutes before kick-off precisely because the facility is
	// not switched on yet. One three-second wait is a slow screen; one per open
	// is a screen that feels broken.
	//
	// A NEGATIVE ANSWER IS CACHED TOO, and that is the half that matters: nil
	// is what an unreachable instance produces, so caching only successes would
	// cache away the fast case and keep the slow one.
	//
	// It is deliberately NOT invalidated when m2lxHost changes. The window is
	// switcherFmtTTL and the wrong answer inside it is a READOUT beside the
	// override box, not anything the pipeline is built to — Start reads the
	// switcher itself, through conformFormat, and never looks here.
	switcherFmtMu sync.Mutex
	switcherFmt   *ConformTargetView
	switcherFmtAt time.Time

	// sessMu guards session and is held for the whole of Start and Stop, so a
	// Start cannot begin while a Stop is still taking the previous pipeline to
	// NULL. Two pipelines contending for one capture device is the failure
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

	// sigMu guards lastSignal, the most recent gst.SignalReport the video signal
	// watchdog delivered. It is a leaf lock, like senderMu: nothing is taken
	// while it is held, and the only thing done under it is one assignment.
	//
	// It exists for the reason lastSender and lastReturn do — the watchdog emits
	// only on transitions, so a page that reloaded mid-match would show the
	// signal lamp grey over a perfectly good picture — and for one reason they
	// do not. The other two lamps have a state that changes: a sender
	// reconnects, a return monitor retries. A LOCKED CAPTURE INPUT EMITS ONCE
	// AND THEN NEVER AGAIN for the whole of a ninety-minute match, which is
	// exactly what a healthy input looks like, so without this cache a reload at
	// half-time would leave the lamp grey until the signal was lost.
	//
	// It is written from internal/gst's watchdog goroutine (NOT a GStreamer
	// streaming thread; see signalForwarder) and read by domReady.
	sigMu      sync.Mutex
	lastSignal gst.SignalReport

	// chanMu guards lastChannelMap, the DeckLink routing CURRENTLY IN FORCE on
	// the running pipeline. Another leaf lock, held for one assignment.
	//
	// It is not the same thing as config.Config.DeckLinkChannelMap and the
	// difference is the whole reason it exists. The config field is what was
	// SAVED; this is what the pipeline was started with and what every live
	// SetChannelMap since has written. They agree on a normal launch and diverge
	// the moment somebody re-routes without pressing Save — and at that moment
	// the routing screen must show what the commentator is actually being heard
	// on, not what the file says. internal/gst deliberately keeps no copy of its
	// own: mix-matrix cannot be read back, so a second account down there would
	// be unfalsifiable. Up here it is falsifiable, because this is the only code
	// that ever calls SetChannelMap.
	//
	// Empty means "nobody has chosen", exactly as it does in gst.ChannelMap and
	// in config.json, and it is what the cache goes back to when a session ends.
	//
	// lastInputChannels is the width that went out with the last "channelMap"
	// event. It is a CACHE and not the authority: GetChannelMap re-reads the pad
	// itself, because that is the call that must recover from a pad which had not
	// yet published caps when the session started. This copy exists for domReady,
	// which runs on the Wails main thread and therefore may not take a lock
	// internal/gst holds across state changes — a page reloaded during a
	// reconnect would freeze the window for the length of it.
	chanMu            sync.Mutex
	lastChannelMap    gst.ChannelMap
	lastInputChannels int

	// chanLevelWidth is how many channels the per-channel picker meter was last
	// drawn at, or 0 if it has never been fed this session.
	//
	// It is written by the channel-levels forwarder on a GStreamer streaming
	// thread — hence the atomic, and hence a store per frame rather than a lock —
	// and read once, when the session ends, to size the all-silence frame.
	//
	// The width MUST be the one the meter was last drawn at. Under the wire
	// contract a changed array length means "the pipeline was rebuilt, lay
	// yourself out again", so a two-channel zero-frame sent to a sixteen-strip
	// meter would make fourteen channels VANISH at the moment the session ended
	// rather than fall silent — the exact failure the zero-frame exists to
	// prevent. Zero means no frame at all was forwarded, which is every session
	// on a positioned capture source, and then no zero-frame is sent either:
	// laying out strips for a meter that never ran would be inventing a display.
	chanLevelWidth atomic.Int64

	// mixMu guards mixCtl, the mixer write path. It is a leaf lock: nothing
	// else is taken while it is held, and the two facts ArmMixer needs from
	// elsewhere — the m2lx client and the configuration — are read and
	// released before it is acquired. See app_mixer.go.
	mixMu sync.Mutex

	// mixCtl is the arm-gated switcher_controller socket, or nil when the
	// operator has not armed the mixer drawer.
	//
	// It is nil for the whole of a normal session. NewController opens a socket
	// that can change a live clean feed, so it is built by ArmMixer and torn
	// down by DisarmMixer — never on application start.
	mixCtl mixer.Controller

	// mixArmedBy records WHICH seat opened the current arm window, guarded by
	// mixMu alongside mixCtl. It is the arm-OWNERSHIP gate: a SendMixerCommands
	// from any other seat is refused with mixer.ErrDisarmed, so with two
	// controllers one operator's arm cannot silently authorise the other's write
	// to the live clean feed. The local webview seat arms as localClientID; a
	// remote seat arms as its per-connection id. Empty when nothing is armed;
	// cleared by closeMixerController. See app_mixer.go and app_remote.go.
	mixArmedBy string

	// retMu guards ret: the SRT return monitor.
	//
	// It is held for the whole of StartReturn and StopReturn, so that a
	// StartReturn racing a StopReturn cannot open a second pipeline on the same
	// headphone endpoint — the same hazard sessMu covers for the capture
	// endpoint, and the reason the two are separate locks is in the file header.
	retMu sync.Mutex

	// ret is the running return session, or nil when the monitor is stopped. It
	// is nil for the whole of a session on the WebRTC return path.
	ret *returnSession

	// retStateMu guards lastReturn, and is a DIFFERENT lock from retMu on
	// purpose. StopReturn holds retMu across the join of the state-forwarding
	// goroutine, and that goroutine writes lastReturn on every transition; one
	// lock over both would deadlock the first Stop that arrived while a
	// transition was in flight. It is the same split senderMu makes against
	// sessMu, for the same reason.
	//
	// It is a leaf: nothing is taken while it is held.
	retStateMu sync.Mutex

	// lastReturn is the most recent gst.ReturnState forwarded to the frontend,
	// replayed by domReady for the same reason lastSender is: the monitor emits
	// only on transitions, so a page that reloaded mid-match would otherwise
	// show the RETURN lamp grey while audio was in the commentator's ears.
	lastReturn gst.ReturnState

	// The SRT PICTURE path takes THREE locks, and the split between them is not
	// fastidiousness — two of the three arrangements deadlock. The rule is:
	//
	//	picMu       →  picViewMu  →  picStateMu
	//
	// taken in that order, never any other, and picMu is the only one ever held
	// across something that blocks.
	//
	// picMu guards pic, the running session. It is held for the whole of
	// StartPicture and StopPicture, INCLUDING the blocking wait inside
	// gst.PictureMonitor.Stop and the join of the state-forwarding goroutine.
	//
	// It is a THIRD subsystem lock, not a reuse of retMu, for the reason that
	// made retMu a second lock rather than a reuse of sessMu: the picture and the
	// audio return reach different hardware — a GPU decoder and a window against
	// an audio endpoint — and either can wedge without the other. One lock over
	// both would mean a StartPicture waiting on a wedged headphone endpoint.
	picMu sync.Mutex

	// pic is the running picture session, or nil when the picture is stopped.
	pic *pictureSession

	// picViewMu guards picOverlay, picRect and picWantVisible: everything about
	// WHERE THE PICTURE IS DRAWN, as opposed to whether it is running.
	//
	// IT IS SEPARATE FROM picMu BECAUSE THE FORWARDING GOROUTINE NEEDS IT WHILE
	// StopPicture IS HOLDING picMu AND WAITING FOR THAT GOROUTINE TO EXIT. Every
	// state transition drives the overlay's visibility, so the forwarder touches
	// these fields on every transition; if they lived under picMu, the first Stop
	// that arrived while a transition was in flight would deadlock — the Stop
	// holding picMu waiting on the join, the forwarder waiting on picMu. That is
	// not hypothetical: it was written that way first and it hung.
	//
	// Nothing held under it may block. Every gst.PictureOverlay method records
	// and posts; see overlay_windows.go's header for why that is the property the
	// whole design rests on, and overlay_darwin.go for the same property upheld
	// by a different rule — every Cocoa call must be made on the main thread, so
	// the darwin overlay dispatches rather than blocking here too.
	picViewMu sync.Mutex

	// picOverlay is the native child window the picture is rendered into, or nil
	// before the first layout call. It OUTLIVES pic: the monitor is rebuilt
	// whenever the configuration changes, and a window destroyed and recreated
	// underneath a running webview is a z-order fight with nothing to gain. Only
	// teardown destroys it.
	picOverlay gst.PictureOverlay

	// picRect is the last rectangle the frontend gave, in PHYSICAL pixels
	// relative to the window's client area, and picWantVisible is the last
	// visibility it asked for. Both are kept even when picOverlay is nil, so that
	// an overlay created later is positioned before it is ever shown.
	picRect        gst.PictureRect
	picWantVisible bool

	// picStateMu guards lastPicture. It is the innermost of the three and it is a
	// leaf: nothing is taken while it is held.
	picStateMu sync.Mutex

	// lastPicture is the most recent gst.PictureState forwarded to the frontend,
	// replayed by domReady for the same reason lastSender and lastReturn are: the
	// monitor emits only on transitions, so a page that reloaded mid-match would
	// otherwise draw the fallback mosaic over a working high-resolution picture.
	lastPicture gst.PictureState

	// THE DECKLINK PREVIEW — the operator's own confidence monitor — has its own
	// trio of these, and the duplication is deliberate rather than a missed
	// factoring. App holds exactly ONE picOverlay under picViewMu, because the
	// SRT picture is one surface; the preview is a SECOND, SIMULTANEOUS surface
	// showing something else. They are on screen at the same time — the
	// commentator's programme return above, what this position is sending below —
	// so one handle and one rectangle cannot serve both, and sharing picViewMu
	// would put the preview's layout calls behind whatever the picture path is
	// doing.
	//
	// prevViewMu is picViewMu's twin and obeys the same rule: nothing held under
	// it may block, and every gst.PictureOverlay method records and posts. The
	// one ordering it does have is sessMu → prevViewMu, because startSession
	// builds and releases the surface while holding sessMu; nothing may take them
	// the other way round, which is why the two bound setters below take
	// prevViewMu alone and never ask the session anything. It is NOT ordered
	// against picViewMu, because nothing anywhere takes both — the two surfaces
	// share no code path, which is what makes a deadlock between them impossible
	// rather than merely unlikely.
	prevViewMu sync.Mutex

	// prevOverlay is the native child window the DeckLink preview renders into,
	// or nil when there is none. Unlike picOverlay it is created and destroyed
	// WITH THE SESSION, because the preview is a branch of the contribution
	// pipeline rather than a monitor of its own: the tee it hangs off exists only
	// while that pipeline does, and set_state(NULL) inside a blocking pad probe
	// was MEASURED to take the on-air leg from 50 fps to 0 permanently with the
	// pipeline still reporting PLAYING. So it is built at Start, from the
	// configuration, and never attached or detached live.
	prevOverlay gst.PictureOverlay

	// prevRect and prevWantVisible are the preview's half of what picRect and
	// picWantVisible are for the picture: the last rectangle the page gave, in
	// PHYSICAL pixels, and the last visibility it asked for. Both are kept
	// across sessions so that the overlay built by the next Start is positioned
	// before it is ever shown.
	prevRect        gst.PictureRect
	prevWantVisible bool

	// prevRunning records whether THIS session's pipeline actually has a preview
	// branch in it — the configuration asked for one AND an overlay was created
	// for it. It is the preview's equivalent of "the monitor is SHOWING" in
	// applyPictureVisibilityViewLocked: without it, a page that asked to see the
	// preview would be shown an empty opaque rectangle over its own controls on
	// every seat that is sending a slate.
	prevRunning bool

	// pictureDial builds the picture monitor, and overlayDial builds the native
	// overlay window. Both are gst's real constructors in the application and
	// fakes in the tests, which is the only way to exercise the wire-up without a
	// GPU and without a window. Nil means the real one; see
	// App.newPictureMonitor and App.newPictureOverlay.
	//
	// overlayDial builds the PREVIEW's surface too. One seam serves both because
	// they are the same type of object created the same way — a native child of
	// the same host window — and a second dial would be a second thing for a test
	// to forget to install.
	pictureDial func() gst.PictureMonitor
	overlayDial func() (gst.PictureOverlay, error)

	// returnDial builds the return monitor. It is gst.NewReturnMonitor in the
	// application and a fake in the tests, which is how the wire-up is exercised
	// without a GStreamer pipeline. Nil means the real one; see
	// App.newReturnMonitor.
	returnDial func() gst.ReturnMonitor

	// senderDial builds the sender that drives a contribution pipeline. It is
	// sender.New in the application and a fake in the tests — which is the only
	// way to exercise the self-stop reaper in startSession at Gate A: the real
	// sender stops itself only on gst.ErrPipelineFatal, a condition the stub
	// pipeline reaches through a live reconnect cycle the test would otherwise
	// have to choreograph in real time. Nil means the real one; see
	// App.newSender.
	senderDial func(gst.Pipeline) sender.Sender

	// mixerDial builds the mixer write controller. It is mixer.NewController in
	// the application and a fake in the tests, which is the only way to
	// exercise the arm gate without a live switcher_controller socket. Nil
	// means the real one; see App.dialMixerController.
	mixerDial func(host, token string) (mixer.Controller, error)

	// The LAN control bridge (internal/remote). The listener is off by default
	// and bound to loopback by default; startup builds and starts it on a
	// goroutine, guarded on the remote.json enabled flag, and teardown closes it
	// with its own bounded budget. It is App-agnostic — it reaches back into the
	// application only through the remoteDispatcher allowlist in app_remote.go.
	//
	// remoteMu serializes building and replacing the server (startup and every
	// remote-admin method). The pointer itself is an atomic so the event-pump tee
	// (broadcastRemote) and the connected-client poll can read it lock-free, on
	// goroutines that must not take remoteMu. remoteRunCancel ends the current
	// generation's client-indicator poll on a restart or teardown; remoteWG holds
	// the per-generation goroutines so teardown can join them within its budget
	// rather than through rootWG (whose join must not race a restart's Add).
	remoteMu        sync.Mutex
	remote          atomic.Pointer[remote.Server]
	remoteRunCancel context.CancelFunc
	remoteWG        sync.WaitGroup

	// closing is raised by teardown before it stops anything, so that a bound
	// method already running on a Wails message-handler goroutine cannot build a
	// new session or control plane behind it. See step 0 of the shutdown order
	// in the file comment.
	closing atomic.Bool

	// shutdownOnce makes teardown idempotent: it is reachable both from Wails'
	// OnShutdown and from main's error path, and exactly one of them must do the
	// work.
	shutdownOnce sync.Once

	// exitProcess ends the process when a teardown has had to abandon a step.
	// NewApp installs forceExit; it is a field so that a test can watch the
	// decision being made without the test binary being the thing that dies.
	//
	// Nothing but teardown may call it. It is not an error path and it is not a
	// panic — it is the last line of "the window has gone, so the process goes".
	exitProcess func()
}

// forceExit ends this process immediately, and is the one thing in the
// application that is allowed to.
//
// The value here is the FALLBACK, not the shipped behaviour. os.Exit is what any
// Go program does when main returns, and it is exactly what must not happen on
// either platform this application ships on: it is the exit that the media
// libraries get to run code inside, and this variable is only ever called with a
// GStreamer thread abandoned mid-state-change. Each shipping platform replaces
// it in an init:
//
//	exit_windows.go   TerminateProcess, which skips DLL_PROCESS_DETACH
//	exit_darwin.go    SIGKILL to self, which skips atexit and static destructors
//
// Both files carry the reasoning and, for macOS, the measurements. A variable
// rather than a build-tagged function so that a test can watch the decision
// being made without the test binary being the thing that dies.
//
// It stays os.Exit on any other platform, which today means a developer's Linux
// checkout and nothing that ships. That is the honest default: os.Exit at least
// ends the process in every case the hard exit is NOT for, and inventing an
// exit_other.go to look symmetrical would be asserting knowledge about a
// platform nobody has measured this on.
//
// The status is zero because the operator asked the application to close and it
// closed. What could not be stopped on the way out is a log line, not a failed
// run — and a GUI process's exit status is not read by anything here anyway.
// (On macOS not even that survives: SIGKILL means the process is reported as
// signalled rather than as having exited zero. exit_darwin.go says why that is
// an acceptable price and why it produces no crash report.)
var forceExit = func() { os.Exit(0) }

// hardExit ends the process. See forceExit and teardown.
func (a *App) hardExit() {
	if a.exitProcess == nil {
		// Only reachable from an App built by something other than NewApp.
		forceExit()
		return
	}
	a.exitProcess()
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
	a := &App{
		appDir:      appDir,
		gstInitErr:  gstInitErr,
		rootCtx:     ctx,
		rootCancel:  cancel,
		events:      newEventPump(),
		lastSender:  sender.StateStopped,
		lastReturn:  gst.ReturnStateStopped,
		lastPicture: gst.PictureStateStopped,
		// UNKNOWN and not OK. Nothing has been measured yet, and on every machine
		// without a capture card in it nothing ever will be; claiming a locked
		// input from an absence of evidence is the exact dishonesty the watchdog
		// was added to remove.
		lastSignal:  gst.SignalReport{State: gst.SignalUnknown},
		exitProcess: forceExit,
	}
	// The credential store is reached ONLY through the scope decorator, installed
	// once, here — never at the call sites. That is what keeps the four
	// credential read sites (two of them in WP-P's and WP-R's files) untouched
	// by the instance-preset feature: they keep passing the three bare keys,
	// and the decorator resolves each through the active scope. See
	// app_presets.go for the whole argument.
	a.store = scopedStore{inner: secrets.New(), scope: a.credentialScope}

	// Install the event-pump tee so every event the local renderer receives also
	// reaches every connected remote seat. It is set here, before the pump's
	// consumer goroutine can exist (that starts in domReady), and reads the
	// remote server pointer lazily on each call, so it is safe even though the
	// server is not built until startup.
	a.events.tee = a.broadcastRemote
	return a
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

	// The seven fixed facility instances are baked in (app_builtin_presets.go).
	// Seed their preset files and M2L-X passwords BEFORE the active-preset
	// record is read below: a machine that already has one applied must sign in
	// on this launch, and a fresh machine must find all seven in the picker.
	a.seedBuiltinPresets()

	// The active-preset record decides WHICH credential-store entries the
	// control plane signs in with, so it must be read BEFORE startControlPlane
	// — the sign-in loop reads the M2L-X password immediately, and a scope set
	// late means the first sign-in of every launch consults the wrong vault
	// entry. A corrupt record is reported rather than silently absorbed,
	// because "no password stored" and "the wrong password target was
	// consulted" look identical on the lamps; the fallback itself is the empty
	// scope — the machine's original entries — which is the recoverable wrong
	// answer, never a guess at another instance's.
	rec, _, err := presets.LoadActive()
	if err != nil {
		a.emitError(fmt.Errorf(
			"wslcomms: the active-preset record could not be read (%v) — no preset is treated as applied "+
				"and the machine's original stored passwords are in use. Apply a preset from the Settings "+
				"screen to repair the record", err))
	}
	a.setCredentialScope(rec.CredentialScope)

	a.startControlPlane()

	// The LAN control bridge, LAST, and on a goroutine of its own inside
	// startRemote. It reads its OWN settings file (remote.json, never config.json)
	// so nothing above had to load it, and it is off by default — a machine that
	// has never enabled remote access binds no socket. startup must not block
	// (Wails runs it on the main thread before the window shows), and standing up
	// a TLS listener means an ECDSA keygen on first enable and a socket bind; both
	// happen on the goroutine startRemote spawns, and any failure is reported on
	// the error event rather than being allowed to delay the window appearing.
	a.startRemote()
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

	a.retStateMu.Lock()
	lastRet := a.lastReturn
	a.retStateMu.Unlock()
	a.events.send(EventReturn, lastRet)

	// The picture, for the same reason and with one extra consequence. A page
	// that reloaded mid-match has forgotten that it asked for the overlay, so
	// picWantVisible is still true on this side while the page believes nothing
	// is showing. Replaying the state is what lets the page put its own view back
	// in step; it will call SetPictureRect from its layout code either way, which
	// is what re-establishes the rectangle.
	a.picStateMu.Lock()
	lastPic := a.lastPicture
	a.picStateMu.Unlock()
	a.events.send(EventPicture, lastPic)

	// The video signal lamp, and this replay does MORE work than the three above
	// it. A sender reconnects and a return monitor retries, so both of those
	// lamps repopulate themselves within seconds of a reload even without a
	// replay; a locked capture input reports once at the start of the match and
	// is then silent for ninety minutes, because silence is what healthy looks
	// like. Without this line a page reloaded at half-time would show the lamp
	// grey — "this application cannot tell you" — over a picture it can see
	// perfectly well.
	a.sigMu.Lock()
	lastSig := a.lastSignal
	a.sigMu.Unlock()
	a.events.send(EventSignal, signalPayloadFrom(lastSig))

	// The routing grid, from the CACHE and never from the pad. This is the one
	// replay that could block the main thread if it asked the pipeline directly:
	// internal/gst holds its lock across state changes, so a page reloaded during
	// a reconnect would freeze the window until the sink swap finished. The
	// routing screen calls GetChannelMap when it opens, which does read the pad,
	// so the live number is one screen-open away and this one is free.
	a.chanMu.Lock()
	chanWidth, chanMap := a.lastInputChannels, a.lastChannelMap
	a.chanMu.Unlock()
	a.events.send(EventChannelMap, channelMapPayloadFrom(chanWidth, chanMap))
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
// Both calls do the right thing on macOS as well as on Windows, which is not
// something to take on trust from the names: Wails' darwin Show is
// makeKeyAndOrderFront followed by activateIgnoringOtherApps:YES, so the app is
// raised above whatever the commentator had in front of it rather than merely
// unhidden behind it. What differs is the ROUTE here. On Windows this callback
// arrives on the goroutine serving the single-instance named pipe; on macOS the
// second process posts an NSDistributedNotification, which the first process's
// AppDelegate receives on the Cocoa main thread and hands to Go. Either way it
// is unordered with respect to startup, which is what the a.ctx atomic and the
// !ok branch below are for.
//
// The launch arguments are ignored. wslcomms takes none — it is started from a
// desktop shortcut or from the Dock with no parameters (specification section 1)
// — so there is nothing in them to act on.
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

// teardown performs the ordered shutdown described in the file comment. It is
// idempotent.
//
// The bounds are the point. The ordered sequence is the correct one and normally
// completes in well under a second, but it contains two calls — sender.Stop and
// the return monitor's Stop — that can be waiting on a GStreamer state change
// inside cgo, where nothing on the Go side can reach them. Each step therefore
// carries its own budget and an overrun abandons that step rather than the rest
// of the shutdown; shutdownTimeout is the backstop over the lot.
//
// If anything WAS abandoned, this does not return. The window has already gone,
// a thread is unaccounted for, and a wslcomms left running after a match is a
// support call — so the process is ended here rather than handed to
// an exit path that would have to step over that thread. See the file comment
// and hardExit.
func (a *App) teardown() {
	a.shutdownOnce.Do(func() {
		abandoned := make(chan int, 1)
		go func() {
			abandoned <- a.teardownOrdered()
		}()

		timer := time.NewTimer(shutdownTimeout)
		defer timer.Stop()
		select {
		case n := <-abandoned:
			if n == 0 {
				return
			}
			log.Printf("wslcomms: shutdown abandoned %d step(s) that would not return; "+
				"ending the process rather than exiting through them", n)
		case <-timer.C:
			// The per-step budgets sum to shutdownTimeout, so reaching this is
			// arithmetic gone wrong rather than work gone slow. Same answer.
			log.Printf("wslcomms: shutdown did not complete within %s; exiting anyway", shutdownTimeout)
		}
		a.hardExit()
	})
}

// teardownOrdered is teardown's body: raise the closing flag, cancel the root
// context, then sender, return monitor, mixer write path, watcher, token
// refresh, join. It returns how many steps had to be abandoned.
//
// The flag is raised before anything is stopped, which is what makes the order
// hold against a bound method that is already running. See step 0 of the
// shutdown order in the file comment for why both interleavings are safe.
//
// Nothing here returns early. Every step runs even when the one in front of it
// had to be abandoned, because the steps take disjoint locks and because the
// alternative is what this function used to do: leave the mixer's write socket
// open and armed, the status watcher running and the root context uncancelled,
// because the sender would not reach NULL.
func (a *App) teardownOrdered() int {
	a.closing.Store(true)

	// FIRST, not last. It cannot block, and it is what ends every context-bound
	// piece of work in the process — the drawer's in-flight GetMixerSnapshot
	// dials, a KVS fetch, the control plane generation, the event pump. Behind a
	// step that can wedge it is a cancellation that never happens.
	a.rootCancel()

	abandoned := 0
	step := func(what string, budget time.Duration, stop func() error) {
		if !teardownStep(what, budget, stop) {
			abandoned++
		}
	}

	step("the sender", senderStopBudget, func() error {
		if err := a.Stop(); err != nil && !errors.Is(err, errNotSending) {
			return err
		}
		return nil
	})

	// The return monitor after the sender, never before it. Both release a
	// audio endpoint and an SRT socket, and if the whole sequence overruns the
	// process exits regardless — so the one that must already have finished is
	// the contribution path.
	step("the return monitor", returnStopBudget, func() error {
		if err := a.StopReturn(); err != nil && !errors.Is(err, errReturnNotRunning) {
			return err
		}
		return nil
	})

	// The picture after the return monitor, never before it. It holds an SRT
	// socket, a GPU decoder and a WINDOW; the window is the only thing in this
	// whole teardown that the operator can see, so it goes as late as it can
	// while still going before the control plane — and it goes after both audio
	// paths, because a commentator would rather lose the picture last.
	step("the picture", pictureStopBudget, a.stopPictureForTeardown)

	step("the mixer write path", mixerCloseBudget, a.closeMixerController)

	// The remote listener BEFORE the control plane: closing it cancels every
	// in-flight remote call's context, so a remote GetMixerSnapshot or
	// SendMixerCommands cannot still be reaching for the m2lx client or the mixer
	// socket the next steps are about to close. It cannot wedge — no cgo, no
	// device — so its bound is a formality, but it carries one so a stray
	// http.Server can never be the process that will not exit.
	step("the remote listener", remoteStopBudget, a.stopRemote)

	step("the status watcher and the token refresh", controlPlaneStopBudget, func() error {
		a.ctlMu.Lock()
		defer a.ctlMu.Unlock()
		a.stopControlPlaneLocked()
		return nil
	})

	step("the background goroutines", rootJoinBudget, func() error {
		a.rootWG.Wait()
		return nil
	})

	return abandoned
}

// teardownStep runs one step of the ordered shutdown under its own bound and
// reports whether it finished. A step that overruns is left running on its own
// goroutine and the caller carries on without it.
//
// Abandoning is stated in the log rather than implied by silence, and it names
// what is being left behind, because the next person to read that log line is
// diagnosing a window that took a moment to go and needs to know which
// subsystem did not answer.
//
// The abandoned goroutine is not leaked in any sense that matters: it is a
// GStreamer state change inside cgo that the process is about to exit out from
// under. See the file comment for what that costs, which is nothing the OS does
// not reclaim.
//
// # A step can finish and STILL have abandoned a thread
//
// Returning is not the same as finishing, and the difference is not cosmetic.
// gst.PictureOverlay.Close is bounded: at gst's overlayCloseBudget it stops
// waiting for its message thread, says so, and RETURNS. Scoring that as a
// finished step is how the abandoned-thread count stays at zero, hardExit is
// never called, and the process leaves through the ordinary exit with a thread
// still unaccounted for — which on Windows means ExitProcess terminating that
// thread wherever it is (inside user32!DestroyWindow, or gstd3d11's subclass
// procedure) and then running DLL_PROCESS_DETACH over the wreckage under the
// loader lock with GStreamer, WASAPI, D3D11 and COM all loaded, and on macOS
// means exit(3) running libsrt's and GObject's atexit handlers ALONGSIDE that
// still-running thread. Those are the two shutdown hangs exit_windows.go and
// exit_darwin.go exist to prevent, arriving through the one door that used to be
// unwatched.
//
// So a step that comes back wrapping gst.ErrAbandonedThread is NOT finished.
// The picture is the first subsystem whose Close returns on a hang rather than
// hanging, so it is the first that could reach this; anything else that grows a
// bounded Close must wrap the same sentinel.
func teardownStep(what string, budget time.Duration, stop func() error) (finished bool) {
	done := make(chan error, 1)
	go func() { done <- stop() }()

	timer := time.NewTimer(budget)
	defer timer.Stop()

	select {
	case err := <-done:
		if err != nil {
			log.Printf("wslcomms: stopping %s during shutdown: %v", what, err)
		}
		if errors.Is(err, gst.ErrAbandonedThread) {
			log.Printf("wslcomms: %s returned, but it ABANDONED a thread rather than joining it. "+
				"Counting the step as unfinished: the process must end by the hard exit rather than "+
				"run the media libraries' own termination hooks over a thread it cannot account for", what)
			return false
		}
		return true
	case <-timer.C:
		log.Printf("wslcomms: %s did not stop within %s. ABANDONING it and continuing the "+
			"shutdown; the process is going, which releases the device and the socket anyway", what, budget)
		return false
	}
}

// ---------------------------------------------------------------------------
// The bound surface
// ---------------------------------------------------------------------------

// ListInputDevices returns the audio capture endpoints for the commentary input
// dropdown. Device.ID is what SaveConfig must be given; Device.Name is for
// display only.
//
// Device.ID is an OPAQUE STRING to everything above internal/gst, and that is
// the contract rather than an implementation detail, because what is in it
// differs by platform: a WASAPI IMMDevice endpoint GUID on Windows, a CoreAudio
// device UID on macOS. Both are stable across reboots and replugs, which is the
// only property config.json depends on. Nothing here, in the frontend, or in
// config may parse it, pattern-match it or assume a shape — preflightAudioDevice
// is the single place that inspects one, it does so through internal/gst's own
// classifiers rather than by reading the string, and its comment sets out which
// half of that knowledge is Windows-only and why it stays.
//
// # THIS IS ALSO THE CARD LISTING, AND THERE IS DELIBERATELY NO SECOND BINDING
//
// The Settings screen needs three lists and this method is all three of them:
//
//	the commentary input   every entry, grouped by Device.Kind — one dropdown
//	                       over both families, which is the whole point of the
//	                       Kind field
//	the DeckLink cards     the entries whose Kind is "decklink"
//	the video source's     the same entries again: is a card fitted at all, and
//	  card                 which one
//
// A ListDeckLinkCards() binding was considered and is NOT added, because it
// would return a filter of this list and nothing else. The card publishes ONE
// persistent-id and it names the CARD rather than a stream — measured on the
// fitted UltraStudio 4K Mini, whose Audio/Source and Video/Source entries both
// publish 2747401380 — which is exactly why config.json holds one
// decklinkPersistentId that the audio leg and the video leg share, and why
// resolveDeckLinkCard can answer a question about the VIDEO leg out of an AUDIO
// enumeration. A second binding could only publish the same numbers under a
// second name, and the first time the two disagreed about which cards exist
// would be the first time somebody added a filter to one of them.
//
// Device.Kind is the ONLY thing that may be read to tell the families apart. It
// always crosses the boundary — its json tag has no omitempty, and internal/gst
// normalises every entry before returning, so the frontend sees an explicit
// "native" or "decklink" and never a missing field. Inferring the kind from the
// shape of an id is forbidden by the paragraph above and would be wrong the
// first time a platform minted an id that looked like the other family's.
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
	return a.saveConfigFrom(localClientID, c)
}

// saveConfigFrom is SaveConfig's body, carrying the id of the seat that saved.
//
// The bound SaveConfig passes localClientID; the remote dispatcher passes the
// connecting seat's id (app_remote.go). After the write it emits EventConfig
// carrying the saved config AND that id, so every OTHER seat can refresh its
// cached config while the seat that saved ignores the echo of its own write.
// Without it, two controllers writing config — a whole-object write from each
// page's cache — clobber one another indefinitely; with it the stale window is
// one round trip. The event carries no secret: config.Save never writes one and
// config.Config has no secret field.
func (a *App) saveConfigFrom(originClientID string, c *config.Config) error {
	if c == nil {
		return errors.New("wslcomms: SaveConfig: no configuration supplied")
	}
	if originClientID != localClientID {
		if err := a.refuseRemoteVideoLegChange(c); err != nil {
			return err
		}
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

	a.events.send(EventConfig, configEvent{Config: &saved, Origin: originClientID})
	return nil
}

// configEvent is the EventConfig payload: the freshly-saved configuration and
// the id of the seat that saved it. A page compares Origin against its own id
// and refreshes only on somebody else's save; see app_remote.go's EventConfig.
type configEvent struct {
	Config *config.Config `json:"config"`
	Origin string         `json:"origin"`
}

// refuseRemoteVideoLegChange is what makes SetVideoSource and
// SetDeckLinkPreviewEnabled being HOST-ONLY mean something.
//
// # Why a host-only method is not by itself a guarantee
//
// SaveConfig is remotely reachable — deliberately, so a producer's laptop can
// fix a port or a status key — and it is a WHOLE-DOCUMENT write from a page
// cache. So without this check, marking the two dedicated setters host-only
// would protect nothing at all: a remote seat could put a camera on air by
// saving a configuration with videoSource changed, through a method it is
// entitled to call, and the host-only classification would be a decoration.
// This is the enforcement; the classification is the declaration.
//
// # It refuses the whole save rather than dropping the two fields
//
// Silently keeping the live values and writing everything else would be the
// gentler behaviour and it is the wrong one: the remote page would show the
// change it thought it had made, its next cache refresh would put it back, and
// nobody would ever be told which of the two seats was right. A refusal naming
// the field is one operator asking another to make the change, which is what
// should happen.
//
// The comparison is against the LIVE configuration and only a real difference
// refuses, so an ordinary remote save — a port, a key length, a status key,
// with the video leg restated exactly as the page was told it — passes through
// untouched. A remote seat's cache is refreshed by the "config" event on every
// save, so a difference is an intention rather than staleness.
func (a *App) refuseRemoteVideoLegChange(c *config.Config) error {
	live := a.snapshotConfig()

	if c.EffectiveVideoSource() != live.EffectiveVideoSource() {
		return fmt.Errorf(
			"wslcomms: refused: videoSource is %q here and this save would make it %q, and what "+
				"this position puts ON AIR cannot be changed from a remote seat. Ask the operator at "+
				"the desk to change it there",
			live.EffectiveVideoSource(), c.EffectiveVideoSource())
	}
	if c.DeckLinkPreviewEnabled != live.DeckLinkPreviewEnabled {
		return fmt.Errorf(
			"wslcomms: refused: decklinkPreviewEnabled is %v here and this save would make it %v, "+
				"and the preview is a window on the operator's own screen rather than anything that "+
				"is transmitted. Ask the operator at the desk to change it there",
			live.DeckLinkPreviewEnabled, c.DeckLinkPreviewEnabled)
	}
	return nil
}

// The two refusals the video-leg setters make while a session is running. They
// are separate errors rather than one shared sentinel because they send the
// operator to two different places, and a message that had to be right about
// both would say neither.
var (
	errVideoSourceWhileSending = errors.New(
		"wslcomms: cannot change the video source while sending: the video leg is built at START " +
			"and there is no way to swap it under a running feed — set_state(NULL) inside a blocking " +
			"pad probe was measured to take the leg from 50 fps to 0 permanently with the pipeline " +
			"still reporting PLAYING. Press STOP, change it, then START")

	errPreviewChangeWhileSending = errors.New(
		"wslcomms: cannot turn the preview on or off while sending: it is a branch of the " +
			"contribution pipeline and is built with it at START, so it can be neither attached nor " +
			"detached live. Press STOP, change it, then START")
)

// SetVideoSource chooses WHAT THE VIDEO LEG CARRIES: config.VideoSourceSlate,
// the still picture this application has always transmitted, or
// config.VideoSourceDeckLink, live video from the Blackmagic card.
//
// # Why this exists at all when SaveConfig could write the same field
//
// Because of who is allowed to call it. This is the one setting on the whole
// bound surface that decides WHAT A BROADCAST SWITCHER RECEIVES, and it is
// HOST-ONLY: refused for every remote connection and omitted from the hello
// frame, so the shim never installs it. A producer's laptop on the facility
// network can watch the meters, fix a routing and stop the feed; it cannot
// decide that this position is now showing its camera.
//
// The classification alone does not achieve that, because SaveConfig writes the
// same field and is reachable — refuseRemoteVideoLegChange is what closes it.
// Having a method of its own is still the right shape: it is where the value is
// checked before anything is written, and it is the row in remoteAllowlist that
// STATES the rule the other check enforces.
//
// NOTHING CALLS IT TODAY, and that is settled rather than pending. The Settings
// screen writes videoSource through its single Save, with the control disabled
// while sending, so this method's refusals are unreachable from the only page
// that could reach them. It is kept for the classification — see the
// bound-surface list at the top of this file — and is the thing to call if a
// later page wants to write the field on change instead of on Save.
//
// # It refuses while sending, and that is not caution
//
// The video leg is built at Start and cannot be exchanged under a running feed;
// the alternative to refusing is a control that appears to switch a camera on
// air and silently does nothing until the next restart, which is the worst
// outcome available here. See errVideoSourceWhileSending for the measurement.
//
// It persists through the same path SaveConfig uses, so the "config" event goes
// out and every OTHER seat's cached configuration refreshes rather than sitting
// on a stale answer until somebody reloads it.
func (a *App) SetVideoSource(source string) error {
	s := strings.TrimSpace(source)
	switch s {
	case config.VideoSourceSlate, config.VideoSourceDeckLink:
	default:
		// Named the way config.Validate names it, because the operator may well
		// meet both messages about the same value and they must agree.
		return fmt.Errorf("wslcomms: videoSource must be %q or %q, got %q",
			config.VideoSourceSlate, config.VideoSourceDeckLink, source)
	}

	a.sessMu.Lock()
	if a.closing.Load() {
		a.sessMu.Unlock()
		return errShuttingDown
	}
	running := a.session != nil
	a.sessMu.Unlock()
	if running {
		return errVideoSourceWhileSending
	}

	cfg := a.snapshotConfig()
	cfg.VideoSource = s
	return a.saveConfigFrom(localClientID, cfg)
}

// SetDeckLinkPreviewEnabled turns the operator's own confidence monitor on or
// off. It changes nothing about what is transmitted; see the field comment on
// config.Config.DeckLinkPreviewEnabled for what it costs and why it defaults to
// off.
//
// It is HOST-ONLY for a different reason from SetVideoSource's, and the
// difference is worth keeping straight: this one cannot reach air at all. What
// it can do is open or close an opaque native window on the screen of whoever is
// sitting at this machine — over whatever they were looking at — and a seat in
// another building has no business doing that. It is the same argument that puts
// SetPictureVisible on the host-only list.
//
// It refuses while sending for the reason SetVideoSource does: the preview is a
// tee branch of the contribution pipeline, built with it.
func (a *App) SetDeckLinkPreviewEnabled(enabled bool) error {
	a.sessMu.Lock()
	if a.closing.Load() {
		a.sessMu.Unlock()
		return errShuttingDown
	}
	running := a.session != nil
	a.sessMu.Unlock()
	if running {
		return errPreviewChangeWhileSending
	}

	cfg := a.snapshotConfig()
	cfg.DeckLinkPreviewEnabled = enabled
	return a.saveConfigFrom(localClientID, cfg)
}

// SetSecret writes one of the three OS credential-store secrets from the
// Settings screen. key is secrets.KeyM2LX, secrets.KeySRT (the SEND path's SRT
// passphrase) or secrets.KeySRTReturn (the RETURN path's). There is deliberately
// no getter: a secret goes into the credential store and never comes back out
// across this boundary.
//
// The two SRT passphrases are separate keys because encryption on M2L-X is set
// per endpoint — the commentary input and the programme outputs disagree about
// it on the measured instance — so conflating them means fixing the monitor
// breaks the feed, or the reverse. secrets.targetFor rejects anything else, so
// a mistyped key from the frontend fails here rather than writing a fourth
// credential nobody reads.
//
// Writing the M2L-X password restarts the control plane, for the same first-run
// reason as SaveConfig: the sign-in loop stops as soon as it finds no stored
// password, and this is what tells it to try again. Neither SRT passphrase
// does: they are read afresh by Start and StartReturn, and restarting sign-in
// over one would drop a working control plane for nothing.
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
// Start is a thin wrapper around startSession for THREE reasons, two of them
// the same reason. The statusKey discovery it may kick off takes ctlMu, and the
// lock order in this file's header says ctlMu is never held with either of the
// others — taking it under sessMu would be the first exception to that and
// there is no reason to make one. The conform-format read takes ctlMu for
// exactly the same reason, and its answer is handed to startSession through
// a.conformTo rather than as an argument, because senderOpts has callers in
// the test suite whose signature is not this change's to move. And the
// discovery has to be armed BEFORE the pipeline: its baseline is the first
// switcher_status frame it sees, and a baseline taken after our feed had
// already reached the switcher would show our own node streaming and never
// report the transition that identifies it.
//
// The ORDER of the first two no longer matters, and the reason it used to is
// worth keeping. The conform read was once an opening snapshot of the status
// socket, which is the same socket the discovery watches, so it had to be done
// FIRST — before the discovery armed its baseline — to keep two readers of one
// stream from interfering. It is now a REST call to
// /api/v1/switcher_configuration and touches nothing the discovery uses. It
// stays first because it must finish before startSession builds the pipeline
// that consumes its answer, and because its bound (conformFetchTimeout, three
// seconds) is the only delay it can add to a START.
func (a *App) Start() error {
	a.conformTo.Store(a.conformFormat(a.snapshotConfig()))

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
		// The window is going away. Building a pipeline now would open the audio
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

	// Every hardware question this session's two legs raise, answered before a
	// single element is built: which video source, which audio subsystem, which
	// card, and whether the machine actually has it. It returns the DeckLink
	// persistent-id the video leg is to open, empty for the slate.
	videoCaptureID, err := a.preflightCapture(cfg)
	if err != nil {
		return err
	}

	passphrase, err := a.srtPassphrase(cfg)
	if err != nil {
		return err
	}

	// The operator's confidence monitor, if they have asked for one and the video
	// leg is a camera. It is built HERE, before the pipeline, because the preview
	// is a branch of that pipeline and its handle is a build-time option: there is
	// no attaching one to a running graph. Its failures are SPARED — a preview
	// that cannot get a window leaves videoCaptureID untouched and the feed goes
	// out without it — which is the whole rule for this path.
	previewHandle := a.startSessionPreview(cfg, videoCaptureID != "")

	pipe, err := gst.New()
	if err != nil {
		// The preview surface exists by now and nothing will ever render into it,
		// so it goes back. Ordinarily it is released by the sender-state
		// forwarder's exit, and this Start never reaches the point where that
		// forwarder is launched — so without this, a Start that failed here would
		// leave an opaque native window over the page with no session behind it
		// and nothing that would ever take it away. The same applies at the
		// snd.Start failure below. Both are idempotent.
		a.stopSessionPreview()
		return fmt.Errorf("wslcomms: creating the media pipeline: %w", err)
	}

	snd := a.newSender(pipe)
	opts := a.senderOpts(cfg, passphrase)

	// The two options the PRE-FLIGHT decided rather than the configuration, and
	// the reason they are set here instead of inside senderOpts: one is a card id
	// resolved against what this machine is actually offering, the other a native
	// window handle. Neither is a function of the configuration alone, and
	// senderOpts is deliberately a pure reading of it — see its comment.
	opts.Pipeline.VideoCaptureID = videoCaptureID
	opts.Pipeline.Preview = gst.PreviewOpts{
		// Enabled is the operator's request AND the pre-flight's verdict, ANDed
		// here rather than left to internal/gst, so that a seat which asked for a
		// preview while sending the slate is simply not asking for one. Passing
		// the request through with no handle would have the pipeline log that the
		// monitor is switched on and has no surface, which is a true sentence
		// about a machine that has nothing to preview and would send the operator
		// looking for a window fault that is not there.
		Enabled:      cfg.DeckLinkPreviewEnabled && videoCaptureID != "",
		WindowHandle: previewHandle,
	}

	if err := snd.Start(opts); err != nil {
		// sender.Start leaves the pipeline it was given untouched on failure,
		// and a gst.Pipeline is single-use, so this one is stopped here rather
		// than leaked: Stop is what closes its Errors channel and releases the
		// capture device.
		if stopErr := pipe.Stop(); stopErr != nil {
			log.Printf("wslcomms: stopping the pipeline after a failed start: %v", stopErr)
		}
		// After the pipeline, never before it: the branch that was rendering into
		// the surface has to be at NULL first. See stopSessionPreview.
		a.stopSessionPreview()
		return err
	}

	sess := &session{snd: snd, pipe: pipe}

	// The routing that is now in force, recorded before anything can change it.
	// It is taken from the options the pipeline was ACTUALLY started with rather
	// than re-derived from the config, so there is one derivation and no way for
	// the cache and the pipeline to disagree about what was written.
	a.chanMu.Lock()
	a.lastChannelMap = opts.Pipeline.ChannelMap
	a.chanMu.Unlock()

	// And the width, which exists only now: the pad negotiates as the pipeline
	// goes to PLAYING, so this is the first moment there is a real number to
	// size a routing grid against. A Settings screen left open across a START
	// therefore sizes itself without being reopened.
	a.publishChannelMap(pipe)

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
		// The session is over — the sender closed its states channel, on the
		// operator-Stop path and the self-stop path alike — so the meters get
		// one final all-silence frame. Without it they freeze at the last
		// level the pipeline reported, and a frozen meter reads as a live one.
		// It is sent from here, before wg.Done runs, so that by the time
		// App.Stop returns (it waits on sess.wg) the zero-frame is already
		// queued behind the StateStopped that preceded it. It bypasses the
		// levels throttle deliberately: the throttle drops frames on the
		// assumption another is 50 ms behind, and this is the frame after
		// which nothing follows.
		a.events.send(EventLevels, silentLevelsPayload())
		// The per-channel picker gets its own zero-frame, AT ITS OWN WIDTH, and
		// only if it ever ran. Swap rather than Load, so the width belongs to the
		// session that recorded it and a later session starts from nothing. A
		// zero here is a session in which no per-channel frame was ever forwarded
		// — every positioned capture source, which is every microphone — and
		// sending a silent frame then would lay out strips for a meter that never
		// existed.
		if n := int(a.chanLevelWidth.Swap(0)); n > 0 {
			a.events.send(EventChannelLevels, silentLevelsPayloadFor(n))
		}
		// The routing grid goes back to "no pad, nothing negotiated". Its
		// crosspoints are controls over a capture pad, and the pad is gone.
		a.forgetChannelMap()
		// And the signal lamp goes back to "cannot tell", for the same reason
		// and with a sharper consequence. The watchdog dies with the pipeline,
		// so whatever it last said — OK or LOST — would stand forever: a green
		// signal lamp beside a grey SENDING lamp, describing a capture element
		// that no longer exists. UNKNOWN is the truth once there is nothing left
		// to poll. It clears the replay cache too, so a page loaded after the
		// session ends does not resurrect it.
		a.forgetSignal()
		// And the preview surface goes, because the pipeline that was rendering
		// into it has gone. This runs HERE rather than in App.Stop so that it
		// covers the self-stop path too — a capture chain that died takes its
		// preview with it — and it runs AFTER the sender closed its states
		// channel, which it does as the last act of stopping the pipeline. That
		// ordering is the same one stopPictureForTeardown insists on and it is
		// insisted on for the same reason: take the surface away first and the
		// video sink is presenting into a handle that no longer names anything.
		a.stopSessionPreview()
	}()
	a.session = sess

	// The reaper: clear a.session once this session's sender is finished, so
	// that a sender which stopped ITSELF does not leave Start refusing with
	// errAlreadySending forever. The sender stops itself on
	// gst.ErrPipelineFatal — the capture chain is dead, no reconnect can carry
	// media, and retrying would be the misdiagnosis that sentinel exists to end
	// — and until this goroutine existed the only thing that ever cleared
	// a.session was App.Stop, so a self-stopped session left the operator with
	// a grey SENDING lamp, a button reading START, and a Start that refused
	// because a session that had already died was still recorded as running.
	//
	// It keys on the forwarder's completion (sess.wg), because the forwarder
	// ranges over snd.States() and the sender closes that channel as the last
	// act of stopping — on the self-stop path exactly as on the operator-Stop
	// path. It clears a.session ONLY if it still holds this same pointer:
	// on the operator-Stop path App.Stop has already cleared it (and may even
	// have installed a successor by the time the reaper gets the lock), and
	// reaping a session it does not own would tear down someone else's.
	//
	// It is its OWN goroutine, deliberately NOT tracked by sess.wg. App.Stop
	// holds sessMu across sess.wg.Wait() (see Stop), so a reaper counted in
	// that WaitGroup — blocked on the sessMu that Stop holds — would deadlock
	// every Stop. As its own goroutine the interleaving is safe in both
	// directions: Stop waits only for the forwarder, and the reaper's lock
	// acquisition simply queues behind Stop and then finds the pointer gone.
	//
	// One consequence is accepted rather than fixed: after a self-stop, an
	// operator STOP that lands after the reaper returns errNotSending. That is
	// tolerable — the states channel closed, so the frontend has already seen
	// StateStopped and flipped the button back to START.
	go func() {
		sess.wg.Wait()
		a.sessMu.Lock()
		if a.session == sess {
			a.session = nil
		}
		a.sessMu.Unlock()
	}()

	return nil
}

// Stop ends the contribution session.
//
// It holds sessMu for its whole duration, including the blocking wait inside
// sender.Stop, so that a Start racing it cannot open a second pipeline on the
// same capture device. By the time it returns, the pipeline is at NULL,
// sender.StateStopped has been emitted and the forwarding goroutine has exited.
//
// A Stop that lands after the sender stopped ITSELF — gst.ErrPipelineFatal,
// the capture chain dead — may find the session already reaped and return
// errNotSending. That is accepted, not a defect: the frontend saw
// StateStopped when the sender emitted it and the button already reads START,
// so the click this Stop came from was pressed against a stale screen at
// worst. See the reaper in startSession.
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
				"check the alias and the password stored in %s under %q",
			cfg.M2LXHost, secrets.StoreName(), secrets.TargetM2LX)
	}

	ctx, cancel := context.WithTimeout(a.rootCtx, kvsFetchTimeout)
	defer cancel()
	return kvs.Fetch(ctx, client, cfg.EventID)
}

// ListEvents lists the events on the configured M2L-X instance, so the frontend
// need never ask an operator to know their event id. It is the mechanism behind
// the operator's "by default for the event to be auto selected when there is
// only 1": with the list in hand the frontend auto-selects the sole event and
// offers a picker only when there is a genuine choice. The event id is otherwise
// derived from a pasted live-operation URL; this supersedes that whenever the
// app can talk to the instance.
//
// It mirrors GetKVSCredentials' guards exactly, because the failure it is most
// likely to hit is the same one — a client that exists but is not signed in —
// and the operator is owed the same specific message rather than a bare
// ErrNotSignedIn bubbling up from the m2lx package:
//
//   - no client at all means m2lxHost is not configured; say so and name the
//     Settings field, since nothing downstream can be tried until it is set.
//   - a client with no bearer token means sign-in has not succeeded (wrong
//     alias, wrong password, or the instance unreachable at start-up); point at
//     the alias and the password in the credential store under secrets.TargetM2LX,
//     the same place GetKVSCredentials points, because it is the same fix.
//
// The call is bounded by kvsFetchTimeout off a.rootCtx — the events overview is
// one small GET, well inside the budget already sized for the whole KVS chain,
// so it reuses that constant rather than introducing a second one that would
// have to be kept in step with it.
// CredentialStoreName returns what this platform's credential vault is CALLED,
// for an operator reading it in the GUI: "Windows Credential Manager" or "the
// macOS login Keychain".
//
// It exists because the frontend was naming one platform's facility on both.
// settings.js's delete-a-preset flow asks a second question — "also delete the
// stored passwords?" — and had "Windows Credential Manager" written into the
// string. On a Mac that sends the operator hunting for a control panel their
// machine does not have, at the moment they are trying to undo something.
//
// It went unnoticed until the macOS port because that dialog had never been
// SEEN on a Mac: WKWebView routes window.confirm through its WKUIDelegate, and
// upstream Wails implements no JavaScript panel methods, so every preset dialog
// silently answered "cancelled" and drew nothing. Fixing the delegate
// (third_party/patches/0002) made the wrong words visible for the first time.
//
// The name is resolved in GO rather than switched on in JavaScript, for the
// same reason secrets.StoreName itself is per-platform beside New: internal/
// secrets is the only thing that KNOWS, and a runtime.GOOS switch in the
// frontend would be a second place to change if a third platform or a different
// backing store ever appears. There is still no getter for a credential VALUE
// across the Wails boundary — this returns the name of the store, never its
// contents.
func (a *App) CredentialStoreName() string {
	return secrets.StoreName()
}

func (a *App) ListEvents() ([]m2lx.Event, error) {
	a.ctlMu.Lock()
	client := a.client
	a.ctlMu.Unlock()

	if client == nil {
		return nil, errors.New(
			"wslcomms: cannot list events: m2lxHost is not configured — set it on the Settings screen")
	}

	cfg := a.snapshotConfig()
	if client.Token() == "" {
		return nil, fmt.Errorf(
			"wslcomms: cannot list events: not signed in to M2L-X at %q yet — "+
				"check the alias and the password stored in %s under %q",
			cfg.M2LXHost, secrets.StoreName(), secrets.TargetM2LX)
	}

	ctx, cancel := context.WithTimeout(a.rootCtx, kvsFetchTimeout)
	defer cancel()
	return client.ListEvents(ctx)
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

// newSender builds the sender for one contribution session, through the
// senderDial seam so the tests can substitute a fake. Nil means the real one.
func (a *App) newSender(pipe gst.Pipeline) sender.Sender {
	if a.senderDial != nil {
		return a.senderDial(pipe)
	}
	return sender.New(pipe)
}

// preflightAudioDevice refuses, BEFORE a pipeline is built, a saved commentary
// input id that is known not to work: a Windows RENDER (playback) endpoint, or
// an id that no longer exists on this machine.
//
// # Why Start checks this at all
//
// Both bad ids used to fail twenty seconds too late and blame the wrong thing.
// wasapi2src accepts any device id at Start, prerolls, and then fails
// ASYNCHRONOUSLY — "Failed to open device {0.0.0.00000000}.{8678ce58-...}",
// measured live — and the sender treated that bus error as a network failure
// and retried forever, telling the operator "the commentary feed to
// <host>:40005 is not connected and is retrying" about a fault that was
// sitting in their own config.json. gst.ErrPipelineFatal now ends that retry
// loop, but ending it is triage; this is prevention. The id is a string in
// hand and both conditions are decidable RIGHT NOW, synchronously, with the
// START button still under the operator's finger — which is worth twenty
// minutes of confusion on a match day.
//
// The two refusals are deliberately different messages, because the operator's
// next move differs: a playback endpoint means "you picked the wrong entry"
// (the dropdown used to offer them; see internal/gst/device_id.go for the
// wasapi2 loopback republication this cleans up after), while a vanished id
// means "this machine no longer has that device" — Dante Virtual Soundcard and
// NDI endpoints come and go with their sources, and a config.json copied from
// another machine carries ids that were never here at all.
//
// # WHICH HALF OF THIS IS WINDOWS KNOWLEDGE, AND WHY IT STILL EARNS ITS PLACE
//
// Check (i) is Windows-only in effect and that is by design rather than by
// omission. gst.IsRenderEndpointID classifies WASAPI endpoint GUIDs by their
// namespace prefix, and no CoreAudio identity — "BuiltInMicrophoneDevice", a
// UUID, an NDI name — matches either prefix, so on macOS the classifier simply
// returns false and this branch never fires. That degrades safely: the check
// asserts POSITIVE knowledge and macOS gives it none, so it stands aside rather
// than guessing. It is deliberately NOT deleted, for two reasons. It is correct,
// tested knowledge about the platform this application still ships on; and a
// config.json copied from a commentator's Windows machine to a Mac carries
// exactly those GUIDs, so the branch fires on macOS precisely when the message
// it prints — "this is a Windows PLAYBACK endpoint" — is the literal truth about
// what is in the file.
//
// Check (ii) needs no platform knowledge at all: it asks ListInputDevices what
// is actually here. That is what catches a Windows id on a Mac, a Mac UID on a
// Windows box, and a device that has been unplugged, in one test on both
// platforms.
//
// # Why enumeration failure does NOT refuse
//
// Step (ii) is advisory: if ListInputDevices errors, or returns nothing, the
// start proceeds and the hiccup goes to the log. The device monitor is a
// GStreamer subsystem with failure modes of its own, and a monitor that could
// veto Start would be a NEW way to be unable to go on air — with the saved
// device possibly sitting there working the whole time. The namespace check
// above it needs no enumeration and still fires; and a genuinely absent device
// still fails exactly as it did before this function existed, loudly, through
// the pipeline-fatal path. Refusal is reserved for POSITIVE knowledge.
//
// The presence test compares ids case-insensitively, matching the classifiers
// in internal/gst/device_id.go: GUID casing is not stable across the APIs that
// print these ids, and refusing a working device over casing would be this
// function causing the outage it exists to prevent. That reasoning is a Windows
// one and the comparison stays case-insensitive on macOS anyway, which is a
// judgement rather than a measurement: a CoreAudio UID is case-SENSITIVE in
// principle, so two devices whose UIDs differed only in case would fold
// together here. Against that, the cost of being wrong in the other direction is
// a commentator who cannot start. Two CoreAudio UIDs differing only in case has
// never been observed; a saved id that will not match is an outage.
func (a *App) preflightAudioDevice(id string) error {
	if gst.IsRenderEndpointID(id) {
		// Positively a playback endpoint. Refused, never opened, and never
		// "made to work" via wasapi2's loopback mode — loopback would put the
		// operator's own monitor mix on air (echo and feedback on a live feed).
		//
		// Reachable on macOS, and the message stays literally true when it is:
		// the only way a CoreAudio machine gets here is a config.json carried
		// over from Windows, in which case the saved id really is a Windows
		// playback endpoint and saying so is the fastest route to the fix.
		return fmt.Errorf(
			"wslcomms: cannot start: the saved commentary input %s is a Windows PLAYBACK endpoint, "+
				"not a microphone — capture endpoints begin %s and this one begins %s. Choose a device "+
				"from the Commentary input dropdown on the main screen and press START again.",
			id, gst.CaptureEndpointPrefix, gst.RenderEndpointPrefix)
	}

	devices, err := a.ListInputDevices()
	if err != nil {
		log.Printf("wslcomms: could not enumerate audio inputs during the pre-flight check (%v); "+
			"starting anyway — the device monitor must not become a new way to be unable to start", err)
		return nil
	}
	if len(devices) == 0 {
		log.Printf("wslcomms: the audio input enumeration returned no devices; starting anyway — " +
			"an empty list is a device-monitor hiccup until proven otherwise")
		return nil
	}
	for _, d := range devices {
		if strings.EqualFold(d.ID, id) {
			return nil
		}
	}
	return fmt.Errorf(
		"wslcomms: cannot start: the saved commentary input %s is not present on this machine — "+
			"%d inputs were found and none of them is that one. Dante Virtual Soundcard and NDI "+
			"endpoints are created and destroyed as their sources come and go. Choose a device from "+
			"the Commentary input dropdown on the main screen and press START again. (If this id was "+
			"typed into the Settings screen's Commentary input device ID box, it belongs to another "+
			"machine.)",
		id, len(devices))
}

// preflightCapture answers every hardware question this session's two legs
// raise, BEFORE a single element is built, and returns the DeckLink
// persistent-id the VIDEO leg is to open — empty for the still slate.
//
// # Why any of this is here rather than left to GStreamer
//
// Because of what GStreamer says when the answer is no. A DeckLink card that is
// absent, or busy, or named by an id that no longer resolves, produces
// "Internal data stream error / not-negotiated (-4)" in about 100 microseconds
// — MEASURED, and it names neither the device nor the cause. The operator gets
// a red lamp and a message about a data stream, several seconds after START,
// with a commentator waiting. Every refusal below names the FIELD to go and fix
// and the DEVICE it could not find, which is the entire point of the function.
//
// It is deliberately in Start and not in config.Validate. Validate decides
// whether a configuration document is well formed; this decides whether THIS
// MACHINE, right now, has the hardware that document describes — a different
// question with a different answer on a different day, and one that has to be
// asked again at every Start rather than once when a field was typed.
//
// # What it will NOT refuse
//
// An enumeration that fails, or that comes back empty, is treated as a device
// monitor hiccup and is not by itself a reason a match does not go out — the
// rule preflightAudioDevice states at length. The one exception is the case
// where the enumeration is the only thing that could have supplied an answer:
// a camera seat that names no card at all, where "carry on anyway" would mean
// silently transmitting a slate to a switcher the operator expects to be
// receiving their camera. That is a wrong-source failure with every lamp green,
// which is the class of defect this application exists to make impossible, so
// it refuses.
func (a *App) preflightCapture(cfg *config.Config) (string, error) {
	// ============ THE DECKLINK AUDIO LEG IS NOT BUILT IN THIS REVISION ======
	//
	// The VIDEO leg is: gst.PipelineOpts.VideoCaptureID opens the card and
	// conforms its input in place of the slate. The AUDIO leg is not —
	// PipelineOpts still carries AudioDeviceID alone and pipelineDescription
	// still builds the platform's own source unconditionally — so the two halves
	// of this feature landed apart and this gate is what keeps the unbuilt half
	// from being reachable.
	//
	// WITHOUT IT THAT COMBINATION IS A SILENT WRONG-DEVICE FAILURE.
	// config.Validate stopped requiring audioDeviceId for a DeckLink seat —
	// correctly, since its commentary never touches CoreAudio or WASAPI — so a
	// seat set to "decklink" passes validation with that field EMPTY, and an
	// empty device on osxaudiosrc or wasapi2src is not an error: it is the SYSTEM
	// DEFAULT INPUT. The match would go out from the laptop's built-in microphone
	// with every lamp green.
	//
	// THIS BLOCK, AND THE `if` BELOW IT, ARE WHAT TO DELETE WHEN THE AUDIO LEG
	// LANDS. Nothing else in this function is temporary. The guard below is
	// already written the way it has to be afterwards — a DeckLink seat must not
	// be pre-flighted against a CoreAudio endpoint it never opens — so removing
	// this refusal is a deletion and not a rewrite.
	if cfg.UsesDeckLinkAudio() {
		return "", fmt.Errorf("wslcomms: cannot start: audioSourceKind is %q, so the commentary "+
			"input is a Blackmagic DeckLink card — and this version cannot capture AUDIO from one "+
			"yet. The card is offered in the device list and its VIDEO can now be sent, but the "+
			"audio capture leg for it is not built. Set the commentary input back to the computer "+
			"sound input on the Settings screen and choose the device there; videoSource is a "+
			"separate setting and is unaffected",
			config.AudioSourceDeckLink)
	}
	if !cfg.UsesDeckLinkAudio() {
		if err := a.preflightAudioDevice(cfg.AudioDeviceID); err != nil {
			return "", err
		}
	}

	if !cfg.UsesDeckLinkVideo() {
		// The slate: no card, no enumeration, nothing to resolve. A seat that has
		// configured nothing must reach the pipeline having done exactly what it
		// did before this function existed, which includes not having asked the
		// device monitor a question.
		return "", nil
	}

	return a.resolveDeckLinkCard(cfg)
}

// resolveDeckLinkCard turns "the operator wants the camera" into the one
// persistent-id gst.PipelineOpts.VideoCaptureID can be given, or into a refusal
// that names the field and the hardware.
//
// # The empty id is a real answer and it has to be resolved, not passed through
//
// decklinkPersistentId is documented as MEANINGFUL when empty — "the card this
// machine has" — and that is right for a commentary position, which has one. But
// an empty VideoCaptureID means THE SLATE to internal/gst, which is the opposite
// instruction, so the emptiness cannot simply be forwarded. This is the one
// place the two vocabularies meet and it is where the translation belongs: ask
// the machine which card it has, and hand the leg that card's id.
//
// # The id that comes back is the ENUMERATED spelling, not the typed one
//
// A hand-typed id that differs only in case is accepted here and then handed on
// exactly as the device monitor spelled it, so that the string reaching the
// element is one this build produced. Matching case-insensitively is the same
// judgement preflightAudioDevice records: refusing a card that is plainly
// present, over the case of a hex digit, would be this function causing the
// outage it exists to prevent.
func (a *App) resolveDeckLinkCard(cfg *config.Config) (string, error) {
	want := strings.TrimSpace(cfg.DeckLinkPersistentID)

	devices, err := a.ListInputDevices()
	if err != nil || len(devices) == 0 {
		if want == "" {
			// The enumeration was the ONLY thing that could have said which card,
			// so there is nothing to carry on with. Starting anyway would send the
			// slate to a switcher the operator believes is receiving their camera.
			return "", fmt.Errorf(
				"wslcomms: cannot start: videoSource is %q, so the video leg is a Blackmagic card, "+
					"but decklinkPersistentId is empty and this machine's capture devices could not be "+
					"listed, so there is nothing to say WHICH card. Set decklinkPersistentId on the "+
					"Settings screen, or set videoSource back to %q to send the slate",
				config.VideoSourceDeckLink, config.VideoSourceSlate)
		}
		// A card WAS named, so the enumeration is not the only source of truth
		// and a monitor hiccup must not be a new way to be unable to start. The
		// element gets the operator's id and says its own piece if it is wrong.
		log.Printf("wslcomms: could not list capture devices during the video pre-flight (%v, %d "+
			"devices); starting anyway with the configured decklinkPersistentId %q",
			err, len(devices), want)
		return want, nil
	}

	var cards []gst.Device
	for _, d := range devices {
		if gst.NormaliseDeviceKind(d.Kind) == gst.KindDeckLink {
			cards = append(cards, d)
		}
	}

	if want != "" {
		for _, c := range cards {
			if strings.EqualFold(c.ID, want) {
				return c.ID, nil
			}
		}
		return "", fmt.Errorf(
			"wslcomms: cannot start: videoSource is %q and decklinkPersistentId is %s, which is not "+
				"a Blackmagic card on this machine — %s. A persistent-id is minted by the card and "+
				"survives a reboot, so an id that no longer resolves means a different card, or none. "+
				"Choose the card on the Settings screen, clear decklinkPersistentId if this machine "+
				"has only one, or set videoSource back to %q to send the slate",
			config.VideoSourceDeckLink, want, describeDeckLinkCards(cards, len(devices)),
			config.VideoSourceSlate)
	}

	switch len(cards) {
	case 1:
		// The ordinary case at a commentary position, and it is logged because
		// "which card did it pick" is the first question anybody asks when the
		// picture is not the one they expected.
		log.Printf("wslcomms: video leg: decklinkPersistentId is empty and this machine has one "+
			"Blackmagic card, %q (%s); capturing from it", cards[0].Name, cards[0].ID)
		return cards[0].ID, nil
	case 0:
		return "", fmt.Errorf(
			"wslcomms: cannot start: videoSource is %q, so the video leg is a Blackmagic card, but "+
				"there is no Blackmagic capture card on this machine — %d capture devices were found "+
				"and none of them is one. Without the Desktop Video driver installed the card is not "+
				"offered at all, even when it is plugged in. Set videoSource back to %q on the "+
				"Settings screen to send the slate",
			config.VideoSourceDeckLink, len(devices), config.VideoSourceSlate)
	default:
		return "", fmt.Errorf(
			"wslcomms: cannot start: videoSource is %q and decklinkPersistentId is empty, which means "+
				"\"the card this machine has\" — but this machine has %d: %s. The card is EXCLUSIVE, "+
				"so guessing would take a card another application may be holding and would send "+
				"whichever one the driver happened to enumerate first. Name the one you want in "+
				"decklinkPersistentId on the Settings screen",
			config.VideoSourceDeckLink, len(cards), describeDeckLinkCards(cards, len(devices)))
	}
}

// describeDeckLinkCards renders the cards an enumeration found, for the message
// an operator reads when the one they asked for is not among them.
//
// It names the cards rather than counting them because the id is a 16-digit hex
// string nobody can hold in their head: the operator's next move is to copy one
// of these into the Settings screen, and a message that said "2 cards" would
// send them looking for a list that exists nowhere in the application.
func describeDeckLinkCards(cards []gst.Device, total int) string {
	if len(cards) == 0 {
		return fmt.Sprintf("%d capture devices were found and none of them is a Blackmagic card", total)
	}
	parts := make([]string, 0, len(cards))
	for _, c := range cards {
		parts = append(parts, fmt.Sprintf("%q (%s)", c.Name, c.ID))
	}
	return "the Blackmagic cards on this machine are " + strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// The conform format: what raster and rate the video leg is built to
// ---------------------------------------------------------------------------

// conformFormat decides the format this session's video leg is conformed to,
// or nil when nothing could answer and internal/gst should apply its own
// fallback.
//
// # The three sources, in the order they are consulted
//
//  1. THE SWITCHER'S OWN SETTING, read with one bearer-authenticated GET of
//     /api/v1/switcher_configuration (internal/m2lx's SwitcherConfiguration).
//     M2L-X is CONFIGURED into a format and requires every source to match it,
//     so this is not evidence about the format — it is the format.
//  2. videoFormatOverride, the operator's declaration.
//  3. nothing: nil, and internal/gst's FallbackConformTarget() — 1920x1080p50,
//     the value that was hardcoded before any of this existed.
//
// # Why the setting beats the declaration, and what that costs
//
// The switcher is the thing being conformed TO, and it now states its own
// answer. A stale override typed weeks ago for a different venue cannot be more
// true than the setting the instance is running on, so the override is a
// fallback for the case that is genuinely common — an instance that is not
// reachable yet, twenty minutes before kick-off, while an operator sets a
// position up — and not a way to contradict a switcher we can read.
//
// The cost is stated rather than hidden: an operator who believes the switcher
// is wrong has no way to force a value while the instance answers. That is why
// a disagreement between the two is logged at the moment it happens, naming
// both, rather than the override simply being ignored in silence. If it ever
// turns out that operators need the escape hatch more than they need the
// setting, this is the one function that has to change.
//
// # THE FALLBACK HAPPENS HERE, ONCE, AND SAYS SO
//
// This is the only place in the application that turns "the format is unknown"
// into a format. Every way of not knowing — no control plane, not signed in
// yet, an instance that will not answer, a configuration this build cannot
// read, an override that will not parse — falls through to the next rung with a
// log line naming what failed, and the bottom rung logs that it was reached.
// "1080p50 because the switcher says so" and "1080p50 because we could not read
// anything" are the same pipeline and very different facts, and the log is
// where the difference lives.
//
// # It never fails, and never delays START by more than conformFetchTimeout
//
// Nothing here may be a reason a match does not go out; that is the same rule
// that keeps statusKey out of config.Validate.
func (a *App) conformFormat(cfg *config.Config) *gst.ConformTarget {
	// The override is parsed FIRST even though it is consulted second, so that
	// a value which cannot be parsed is reported once, here, whether or not the
	// switcher answers. config.Validate refuses such a value before Start
	// completes, so in practice this line fires only for a hand-edited file or
	// a preset from a newer build — the two cases in which nothing else would
	// ever mention it.
	spec, haveOverride, err := cfg.VideoFormatOverrideSpec()
	if err != nil {
		log.Printf("wslcomms: ignoring videoFormatOverride: %v", err)
	}

	if conf, ok := a.switcherConfiguredFormat(); ok {
		// ConformTargetFromRate, not a struct literal: the switcher states the
		// rate as a DECIMAL (frame_rate arrives as the string "50"), and 29.97
		// is a rounding of 30000/1001 rather than a rate. internal/gst is where
		// that conversion is written down, once.
		t, err := gst.ConformTargetFromRate(conf.Width, conf.Height, conf.FrameRate)
		if err != nil {
			// A raster the pipeline cannot be built to — a rate that will not
			// reduce, or a size past internal/gst's sanity limits. Pinning the
			// size without the rate would leave imagefreeze free to negotiate
			// any rate at all, which is the unpinned behaviour this whole
			// change replaces, so this falls through to the override instead.
			log.Printf("wslcomms: the switcher is configured for a format this application "+
				"cannot conform to (%s): %v", conf.Raw, err)
		} else {
			// Both sides of the colorimetry claim on ONE line: what the leg
			// pins, and — inside Raw — the signal_type the switcher stated. So
			// agreement needs no log line of its own, and disagreement gets the
			// two below.
			log.Printf("wslcomms: conforming the video leg to %s, read from the switcher's "+
				"configuration (%s); colorimetry stays pinned at %s", t, conf.Raw, videoLegColorimetry)

			switch c, known := conf.Colorimetry(); {
			case !known && conf.SignalType != "":
				log.Printf("wslcomms: WARNING: the switcher states signal_type %q, which this "+
					"application does not recognise; the video leg is pinned to colorimetry=%s. "+
					"If the feed is accepted but looks wrong in colour, this line is why",
					conf.SignalType, videoLegColorimetry)
			case known && c != videoLegColorimetry:
				log.Printf("wslcomms: WARNING: the switcher is configured for %s colorimetry "+
					"(signal_type %q) and the video leg is pinned to %s; the feed will be "+
					"colorimetrically wrong even though the raster and rate match",
					c, conf.SignalType, videoLegColorimetry)
			}

			if haveOverride && !sameConformTarget(t, spec) {
				log.Printf("wslcomms: WARNING: videoFormatOverride says %s but the switcher is "+
					"configured for %s; using the switcher. Clear the override, or correct it, if "+
					"this position is meant to send %s", spec.Canonical(), t, spec.Canonical())
			}
			return &t
		}
	}

	if haveOverride {
		// The spec's fraction is used as it stands rather than reconstructed
		// from spec.FrameRate(): config.ParseVideoFormat has already done the
		// decimal-to-fraction conversion, with the same 1000/1001 family
		// recognised, and round-tripping it through a float64 could only lose.
		t := gst.ConformTarget{
			Width:        spec.Width,
			Height:       spec.Height,
			FrameRateNum: spec.FrameRateNum,
			FrameRateDen: spec.FrameRateDen,
		}
		log.Printf("wslcomms: conforming the video leg to %s, from videoFormatOverride — "+
			"the switcher did not tell us what it is configured for", t)
		return &t
	}

	// Neither. This is the one place "unknown" becomes a format: nil, which
	// senderOpts passes to internal/gst as the zero ConformTo and internal/gst
	// resolves to FallbackConformTarget. It is logged because the alternative
	// is a pipeline nobody can account for — "1080p50 because the switcher says
	// so" and "1080p50 because we could not read anything" are the same
	// pipeline and very different facts.
	log.Printf("wslcomms: the switcher did not tell us what it is configured for and no "+
		"videoFormatOverride is set; the video leg falls back to %s. Set videoFormatOverride "+
		"(%s) if this instance is configured for anything else.",
		gst.FallbackConformTarget(), config.VideoFormatExample)
	return nil
}

// videoLegColorimetry is the colorimetry internal/gst pins in the video leg's
// spatial capsfilter (ConformTarget.spatialCaps: colorimetry=bt709).
//
// It is TRANSCRIBED here rather than imported because internal/gst exports no
// constant for it, and widening that package's API is not this change's to do.
// The duplication is deliberate and cheap: the value is compared against what
// the switcher states, so if internal/gst ever pins something else, the two
// disagree and this file's log line is the thing that is wrong — a comment
// mismatch, not a broken pipeline. m2lx.SwitcherConfiguration.Colorimetry
// carries the reasoning for why the pipeline is not simply built to whatever
// the switcher states.
const videoLegColorimetry = "bt709"

// switcherConfiguredFormat asks the instance what video format it is
// configured for: one bearer-authenticated GET, bounded by conformFetchTimeout.
//
// It reads a SETTING, which is why this is a REST call and not a status-socket
// dial. The version of this that scanned switcher_status for a streaming node
// is gone, and internal/m2lx/configuration.go's tombstone has the measurement
// that killed it — a live 720p50 feed reported by the switcher as
// frame_rate="0" — for the next person who thinks the detected format of a
// running node is a cheaper source than this call. It is not cheaper and it is
// not right: 72 ms of REST beats a value that is sometimes zero.
//
// Every failure is a log line and ok false, never an error and never a panic.
// In particular ErrNotSignedIn is ordinary rather than exceptional:
// startControlPlane's sign-in runs on its own goroutine, and a START pressed
// within a second or two of launch legitimately arrives before it has finished.
// So does an instance that is not up yet — the operator can be configuring a
// position long before the facility switches on, and that must still start.
func (a *App) switcherConfiguredFormat() (m2lx.SwitcherConfiguration, bool) {
	a.ctlMu.Lock()
	client := a.client
	a.ctlMu.Unlock()

	if client == nil {
		// No M2L-X host configured, or the control plane has not been built
		// yet. Not worth a log line of its own — the caller's fallback line
		// says what happened and this would only add "and here is one of the
		// several reasons".
		return m2lx.SwitcherConfiguration{}, false
	}

	ctx, cancel := context.WithTimeout(a.rootCtx, conformFetchTimeout)
	defer cancel()

	conf, err := client.SwitcherConfiguration(ctx)
	if err != nil {
		log.Printf("wslcomms: could not read the switcher's configured video format: %v", err)
		return m2lx.SwitcherConfiguration{}, false
	}
	if !conf.Valid() {
		// The instance answered and this build could not read its answer: a
		// format block that is absent, or whose width/height/frame_rate did not
		// parse into a raster. Distinct from the error above and worth its own
		// line, because the fix is a firmware or a parser rather than a network
		// — and Raw quotes exactly what arrived, so the next person does not
		// have to reproduce it to see it.
		log.Printf("wslcomms: the switcher's configuration carries no video format this "+
			"application can read (%q); falling back", conf.Raw)
		return m2lx.SwitcherConfiguration{}, false
	}
	return conf, true
}

// sameConformTarget compares what the switcher reports against what the
// operator declared, on the three things that decide a capsfilter and on
// nothing else.
//
// The rates are compared by CROSS-MULTIPLYING rather than by comparing the two
// fractions field by field, so that 50/1 and 100/2 are the same rate. Neither
// producer emits an unreduced fraction today; the comparison costs one
// multiplication and removes a whole class of false warning if either ever
// does.
//
// Scan is excluded on purpose: config.ParseVideoFormat's grammar describes
// progressive video only, so a spec can never say interlaced and comparing it
// would report a disagreement no operator could ever resolve.
func sameConformTarget(t gst.ConformTarget, spec config.VideoFormatSpec) bool {
	return t.Width == spec.Width && t.Height == spec.Height &&
		t.FrameRateNum*spec.FrameRateDen == spec.FrameRateNum*t.FrameRateDen
}

// The conform-target provenance strings. They cross to the frontend inside
// ConformTargetView.Source and exist so a readout can say WHERE the number came
// from: a lamp judging against a number nobody can trace is not worth having.
const (
	// conformSourceSession is the strongest answer there is — the target the
	// RUNNING pipeline was actually built to, read back rather than recomputed.
	conformSourceSession = "session"

	// conformSourceOverride is the operator's videoFormatOverride, reported
	// when nothing is running to have been built to anything.
	conformSourceOverride = "override"

	// conformSourceSwitcher is the INSTANCE'S OWN SETTING, read live by
	// GetSwitcherFormat. GetConformTarget never returns it, and that is not an
	// oversight: see GetSwitcherFormat for why the two questions are two
	// bindings.
	conformSourceSwitcher = "switcher"
)

// switcherFmtTTL is how long GetSwitcherFormat's cached answer is reused for.
//
// It is a SETTING, not a measurement — an instance's configured video format
// changes when an engineer changes it, which is a thing that happens between
// events rather than during one — so the only thing the TTL trades away is how
// long a Settings screen left open would keep showing the old raster after
// somebody reconfigured the switcher underneath it. Thirty seconds is short
// enough that reopening the screen is the fix, and long enough that the
// unreachable-instance case costs one three-second wait rather than one per
// open.
const switcherFmtTTL = 30 * time.Second

// ConformTargetView is App.GetConformTarget's answer: the video format every
// source feeding this M2L-X instance must be produced in.
//
// Width, Height and FrameRate are the load-bearing three — they are what
// lamps.js compares the detected format against. The rest is PROVENANCE and
// nothing reads it yet; it is carried because the fields that explain an answer
// have to be gathered at the moment the answer is decided or they cannot be
// gathered at all.
//
// FrameRate is the OPERATOR-FACING DECIMAL and deliberately not the exact
// fraction — gst.ConformTarget.DisplayFrameRate says at length why 30000/1001
// must cross this boundary as 29.97, and the summary is that the frontend
// compares it with === against a rate M2L-X reports as the string "29.97".
type ConformTargetView struct {
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	FrameRate float64 `json:"frameRate"`

	// Source is one of the conformSource* constants above.
	Source string `json:"source"`

	// There used to be three more fields here — node, agreeing, disagreeing —
	// describing which streaming node the format had been derived from and how
	// many others agreed with it. They went with the derivation itself
	// (internal/m2lx/configuration.go's tombstone), and they are not replaced:
	// a SETTING has no node to name and cannot disagree with itself. The
	// frontend's typedef in backend.js still lists them as optional, which
	// costs nothing and is honest — nothing sends them now.

	// Raw is the format as it was written down — the operator's
	// videoFormatOverride string, canonicalised. It is what a readout should
	// show when it wants to quote rather than describe.
	Raw string `json:"raw,omitempty"`
}

// GetConformTarget reports the video format the contribution feed's video leg
// is — or would be — conformed to, or nil when nothing is known.
//
// It is the binding that closes the last hardcoded raster in the application.
// frontend/src/ui/lamps.js used to judge the switcher's detected format against
// a constant {h264, 1920, 1080, 50}, so on a correctly configured 720p50
// facility the VIDEO OK lamp read RED on a feed arriving perfectly, and the only
// remedy at the desk was to learn to ignore a red lamp — the habit the whole
// status row exists to prevent.
//
// # A RUNNING SESSION IS ANSWERED FROM THE SESSION, and that is the whole point
//
// When a pipeline is up, this returns the target THAT pipeline was built to,
// read back out of a.conformTo rather than derived again. It is not an
// optimisation. The lamp's question is "does what the switcher sees match what
// we are sending", and what we are sending was decided once, at Start, from a
// switcher that may since have changed. Re-deriving here would let the lamp
// judge our feed against a target the feed was never built to — a red lamp on a
// correct feed, or worse a green one on a feed that no longer conforms — which
// is a subtler version of the very defect being fixed. The pipeline's own answer
// is the only one that can be right about the pipeline.
//
// # WITH NO SESSION IT DOES NOT DIAL, and reports only the override
//
// conformFormat's first source is the switcher's own setting, read with one
// REST call bounded at conformFetchTimeout. This does not make that call, for
// two reasons that point the same way. It is called on the page's startup path,
// and a UI call that can block for three seconds on an unreachable instance to
// refine a lamp is a bad trade — the bound is what it can cost, not the 72 ms it
// usually costs; and with no session there is no feed for the lamp to judge,
// because our own node cannot be streaming when we are not sending — so the
// answer is advisory until Start, at which point Start reads the switcher
// properly and the frontend re-reads.
//
// The consequence is stated rather than hidden: before Start this reports the
// operator's declaration and not the switcher's setting, and it says so in
// Source. It can therefore differ from what Start will go on to choose. What it
// can NEVER do is contradict a pipeline that exists, which is the property that
// matters.
//
// # NEVER AN ERROR, AND NEVER PARTIAL
//
// Every way of not knowing returns nil, which backend.js documents as the normal
// answer and lamps.js answers with DEFAULT_CONFORM_TARGET — today's 1080p50, so
// an unknown switcher behaves exactly as this application always has. A partial
// target is never returned: normaliseConformTarget replaces non-positive fields
// INDIVIDUALLY, which is safe but would silently mix two sources into one
// raster that neither of them describes.
func (a *App) GetConformTarget() *ConformTargetView {
	// The running pipeline first. sessMu is held only to read the pointer —
	// conformTo is an atomic and is not covered by it — and nothing here calls
	// out under the lock, so this cannot interact with the Start/Stop path it
	// shares the mutex with.
	a.sessMu.Lock()
	running := a.session != nil
	a.sessMu.Unlock()

	if running {
		// A session whose conformTo is nil derived nothing at Start: the
		// pipeline was built to internal/gst's own fallback. Report that as the
		// session's target rather than as "not known", because it IS what the
		// feed is being produced in and the lamp should judge against it.
		t := gst.FallbackConformTarget()
		if stored := a.conformTo.Load(); stored != nil {
			t = *stored
		}
		return conformTargetView(t, conformSourceSession, "")
	}

	spec, ok, err := a.snapshotConfig().VideoFormatOverrideSpec()
	if err != nil || !ok {
		// A value that will not parse is NOT reported here. config.Validate
		// refuses it at Start naming the field, and validate.js refuses it
		// before it can be saved; repeating it as a lamp fault would put a
		// third, vaguer voice on the same defect. Nil is the honest answer:
		// nothing is known.
		return nil
	}

	return conformTargetView(gst.ConformTarget{
		Width:        spec.Width,
		Height:       spec.Height,
		FrameRateNum: spec.FrameRateNum,
		FrameRateDen: spec.FrameRateDen,
	}, conformSourceOverride, spec.Canonical())
}

// conformTargetView renders a target for the frontend, or nil if it is not a
// usable one.
//
// The Valid check is the guard against a partial answer described on
// GetConformTarget. It is cheap and it is the only place the invariant can be
// enforced, because every caller above has already decided it has an answer.
func conformTargetView(t gst.ConformTarget, source, raw string) *ConformTargetView {
	if !t.Valid() {
		return nil
	}
	return &ConformTargetView{
		Width:     t.Width,
		Height:    t.Height,
		FrameRate: t.DisplayFrameRate(),
		Source:    source,
		Raw:       raw,
	}
}

// GetSwitcherFormat reports the video format THE M2L-X INSTANCE IS CONFIGURED
// FOR, read live from the instance, or nil when that cannot be established.
//
// # Why this is a second binding and not a branch inside GetConformTarget
//
// Because they are two different questions and the Settings screen wants both
// at once, side by side:
//
//	GetConformTarget    what will WE produce?    (the running pipeline's target,
//	                                              or the operator's declaration)
//	GetSwitcherFormat   what does the SWITCHER   (the instance's own setting,
//	                    require?                  read over REST)
//
// The whole value of showing them together is that a DIVERGENCE is visible: an
// override typed for last month's venue, against a switcher configured for this
// one. Folding the switcher's answer into GetConformTarget would collapse the
// two into one number and destroy exactly the comparison the screen exists to
// make.
//
// There is a second, harder reason, and it is why GetConformTarget must not
// simply start dialling. GetConformTarget is called on the PAGE'S STARTUP PATH,
// by the status lamps, and its own doc commits to not making a network call
// there — a lamp that costs up to conformFetchTimeout on an unreachable
// instance is a bad trade. This binding is called from the Settings screen's
// open, which is a screen the operator is already waiting on and which is not on
// the air path. Same data, different budget, so: different method.
//
// # IT NEEDS NO SESSION, WHICH IS THE ENTIRE POINT
//
// switcherConfiguredFormat reads a SETTING through one bearer-authenticated GET
// of /api/v1/switcher_configuration. It needs the control-plane client — built
// at startup from m2lxHost, and signed in on its own goroutine — and nothing
// else. It does NOT need a pipeline, a streaming node, or anything to be on air.
// That matters because the Settings screen is precisely where there is no
// session: an operator picking a format an hour before kick-off can be shown
// what the facility is set to, which is the one fact that makes the choice
// obvious.
//
// The version of this that read a streaming node's DETECTED format could not
// have done that, and would have been wrong anyway — internal/m2lx's tombstone
// has the measurement, a live 720p50 feed reported by the switcher as
// frame_rate="0".
//
// # Every way of not knowing is nil
//
// No m2lxHost, not signed in yet, an instance that is not up, a format block
// this build cannot read, a raster internal/gst will not accept: all nil, never
// an error. The frontend renders nothing rather than a wrong number, exactly as
// it does for GetConformTarget. Each of those has already been logged by
// switcherConfiguredFormat with the reason, so this adds no line of its own
// except for the one case that function cannot see.
func (a *App) GetSwitcherFormat() *ConformTargetView {
	a.switcherFmtMu.Lock()
	defer a.switcherFmtMu.Unlock()

	// The lock is held ACROSS the REST call, deliberately. Settings screens are
	// opened one at a time by one person; two seats opening one at the same
	// instant would serialise, and the second would then find the first's
	// answer already cached. The alternative — dropping the lock, dialling,
	// retaking it — buys concurrency nothing measures and admits two in-flight
	// reads racing to store different answers.
	if !a.switcherFmtAt.IsZero() && time.Since(a.switcherFmtAt) < switcherFmtTTL {
		return a.switcherFmt
	}

	view := a.readSwitcherFormat()
	a.switcherFmt = view
	a.switcherFmtAt = time.Now()
	return view
}

// readSwitcherFormat is GetSwitcherFormat's uncached body: one instance read,
// turned into the view or into nil.
func (a *App) readSwitcherFormat() *ConformTargetView {
	conf, ok := a.switcherConfiguredFormat()
	if !ok {
		// Already logged, with the reason, by switcherConfiguredFormat.
		return nil
	}
	// ConformTargetFromRate, for the reason conformFormat gives: the switcher
	// states its rate as a DECIMAL, and 29.97 is a rounding of 30000/1001
	// rather than a rate. This is a READOUT and Start's derivation is the real
	// decision, so the two must not reach the operator's screen having done the
	// conversion two different ways.
	t, err := gst.ConformTargetFromRate(conf.Width, conf.Height, conf.FrameRate)
	if err != nil {
		// The one failure switcherConfiguredFormat cannot log, because it is
		// about what internal/gst will accept rather than about what arrived.
		log.Printf("wslcomms: the switcher is configured for %s, which this application cannot "+
			"render as a video format (%v); the Settings screen will show nothing beside the "+
			"override", conf, err)
		return nil
	}
	// t.String() is the OPERATOR'S spelling — "1920x1080p50" — and is the same
	// function that writes the format into the log at Start, so the readout and
	// the log cannot disagree about how a rate is printed.
	return conformTargetView(t, conformSourceSwitcher, t.String())
}

// senderOpts builds the options for one session from a configuration snapshot
// and the passphrase already read from the credential store.
//
// It is separate from Start so that what the sender is actually given can be
// asserted without running a session — including the one field with behaviour
// attached to it, OnConnectError.
//
// TWO OPTIONS ARE DELIBERATELY NOT SET HERE and startSession adds them after
// this returns: the video leg's DeckLink persistent-id and the preview's window
// handle. Neither is a function of the configuration. One is a card id resolved
// against what this machine is actually offering — an empty decklinkPersistentId
// means "the card this machine has", which only an enumeration can turn into an
// id — and the other is a native surface that has to exist. Deriving either
// here would make this function reach hardware, which is exactly what it is for
// not doing.
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

	// The conform format Start derived, or the zero VideoFormat when it derived
	// none — which internal/gst documents as "nothing is known" and resolves to
	// its own 1920x1080p50. A nil here is therefore not a missing value to
	// guard against; it is the third of the three answers conformFormat can
	// give, and it is the one every test that calls senderOpts directly
	// receives, so those tests keep asserting exactly the pipeline the shipped
	// build produces today.
	var conformTo gst.ConformTarget
	if f := a.conformTo.Load(); f != nil {
		conformTo = *f
	}

	return sender.Opts{
		Pipeline: gst.PipelineOpts{
			SlatePath:     a.slatePath(cfg),
			AudioDeviceID: cfg.AudioDeviceID,
			ConformTo:     conformTo,
			// The AUDIO bitrate is left at zero so that internal/gst applies
			// its own documented constant of 128000 bps (specification section
			// 5); the codec is likewise not exposed to the user. The VIDEO
			// bitrate no longer is: the owner has ruled 2000 kbps — chosen for
			// a still slate — far too low for a leg carrying live video, and
			// config.EffectiveVideoBitrateKbps substitutes the same 2000 for an
			// unset field, so a configuration nobody has touched still encodes
			// exactly what the on-air build encodes.
			VideoBitrateKbps: cfg.EffectiveVideoBitrateKbps(),

			// The input meters. The pipeline's level element measures what is
			// actually encoded and sent and calls this on a streaming thread
			// (gst.PipelineOpts.OnLevels: MUST NOT BLOCK) — the forwarder is a
			// throttle in front of the non-blocking event pump, so it cannot.
			OnLevels: a.levelsForwarder(nil),

			// The per-channel picker meter, from the OTHER level element: the
			// capture device's own channels, upstream of the routing. It is a
			// separate callback because it is a separate measurement, and the
			// separation is what stops a sixteen-entry frame ever being drawn on
			// the meter that is supposed to show what is going to air.
			OnChannelLevels: a.channelLevelsForwarder(),

			// The signal lamp. Unlike OnLevels this is not called on a streaming
			// thread — internal/gst's watchdog polls from a goroutine of its own —
			// so the forwarder may take sigMu for the one assignment it makes.
			OnSignal: a.signalForwarder(),

			// The routing the card starts with. The zero value is not silence: it
			// means nobody has chosen, and internal/gst resolves it to the card's
			// first two embedded channels — bit-for-bit what this application sent
			// before the routing screen existed. It is IGNORED on a positioned
			// capture source, which is every microphone and the whole of the
			// on-air Windows path.
			ChannelMap: gstChannelMap(cfg.DeckLinkChannelMap),
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
// frontend's "error" event, rate-limited — except for the one failure that is
// terminal, which bypasses the rate limit and says so.
//
// # What it is for
//
// The sender retries forever by design, and without this the reason each
// attempt failed is discarded. That is fine for a peer that has gone away for
// ten seconds and comes back. It is not fine for a fault that cannot clear
// itself: unplug the commentator's Dante endpoint and the capture source errors
// — wasapi2src on Windows, osxaudiosrc on macOS — every subsequent
// gst.Pipeline.ReplaceSink fails immediately, and the operator has an amber
// SENDING lamp and nothing else for the rest of the match. The lamp says
// "connecting" when the truthful answer is "your audio device is gone".
//
// # The terminal branch: gst.ErrPipelineFatal
//
// That Dante-unplugged fault is no longer retried at all. internal/gst latches
// a capture-chain death as gst.ErrPipelineFatal, and the sender STOPS on it —
// it reports the error here once, emits StateStopped and closes its states
// channel. So a report satisfying errors.Is(err, gst.ErrPipelineFatal) is not
// one more failed attempt in an unbounded series; it is the session's last
// word, and report treats it differently on every axis:
//
//   - the message says the session has STOPPED and what to do about it (check
//     the commentary input device, press START), because the SENDING lamp is
//     about to go grey and the operator needs the reason next to it;
//   - it does NOT carry the SRT host and port. Naming the network target on a
//     local device fault is the measured misdiagnosis this whole chain exists
//     to end — "the commentary feed to <host>:40005 is not connected and is
//     retrying", said forever, about a playback endpoint in config.json;
//   - it bypasses the connectErrorRepeat suppression entirely, and touches
//     none of its bookkeeping. A terminal message is by definition not a
//     repeat: the sender stops after delivering it, so there is no series to
//     suppress, and a fresh session gets a fresh reporter anyway.
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

	if errors.Is(err, gst.ErrPipelineFatal) {
		// Terminal. The sender is stopping over this — see the type comment's
		// terminal-branch section for why it skips the rate limit, skips the
		// bookkeeping, and does not name the SRT endpoint: the fault is the
		// commentary input device or the capture chain on THIS machine, and
		// prefixing it with host:port is the misdiagnosis being retired.
		r.emit(fmt.Errorf(
			"wslcomms: the commentary session has STOPPED and is not retrying: the commentary input "+
				"device or its capture chain has failed, which is a fault on this machine, not the "+
				"network — no reconnect can repair it. Check the commentary input device on the main "+
				"screen, then press START to begin a new session: %w", err))
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
// wslcomms.exe on Windows (specification section 11) and which the bundle
// carries in Contents/MacOS on macOS. An absolute path is taken as given, so an
// operator can point the app at a different slate without writing inside
// Program Files or inside a signed .app — and on macOS writing inside the bundle
// would break its signature and its notarisation, so the absolute path is the
// only way there.
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

// srtPassphrase reads the SRT passphrase from the OS credential store and checks
// it against the configured pbkeylen.
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
					"but no passphrase is stored in %s under %q — "+
					"enter it on the Settings screen, or set pbkeylen to 0 for an unencrypted session",
				cfg.PBKeyLen, secrets.StoreName(), secrets.TargetSRT)
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
			"wslcomms: no M2L-X password is stored in %s under %q — "+
				"enter it on the Settings screen", secrets.StoreName(), secrets.TargetM2LX))
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

// levelsPayload is the "levels" event's wire shape: peak and RMS per channel,
// dBFS, silence at levelsSilenceDB. The lower-case JSON keys are the contract
// with frontend/src/ui/backend.js — gst.Levels is not sent directly because
// its exported Go field names would cross the boundary as PeakDB/RMSDB and tie
// the frontend to this package's internal naming.
type levelsPayload struct {
	Peak []float64 `json:"peak"`
	RMS  []float64 `json:"rms"`
}

// silentLevelsPayload is the zero-frame emitted once when a session ends:
// every channel at the silence floor, so the meters fall to nothing instead of
// freezing at the last reported level. A frozen meter reads as a live one,
// which is the direction the status display must never be wrong in — the same
// reasoning that makes the lamps prefer grey over stale green.
// It is the PROGRAMME meter's zero-frame, and its width is the stereo pair the
// AAC encoder is pinned to. The per-channel picker needs the same thing at
// whatever width IT was running at, which is silentLevelsPayloadFor and is not
// interchangeable with this.
func silentLevelsPayload() levelsPayload {
	return silentLevelsPayloadFor(gst.ChannelMapOutputs)
}

// silentLevelsPayloadFor is silentLevelsPayload at an arbitrary channel count:
// the zero-frame for a meter that was showing N strips.
//
// THE COUNT MUST BE THE ONE THE METER WAS LAST DRAWN AT, and not a default.
// Under the wire contract a changed array length means "the pipeline was
// rebuilt, lay yourself out again" — so a two-channel zero-frame sent to a
// sixteen-strip picker would rebuild it as two strips at the exact moment the
// session ended, and the operator would watch fourteen channels VANISH rather
// than fall silent. Falling to nothing is the entire purpose of the frame.
func silentLevelsPayloadFor(channels int) levelsPayload {
	peak := make([]float64, channels)
	rms := make([]float64, channels)
	for i := range peak {
		peak[i] = levelsSilenceDB
		rms[i] = levelsSilenceDB
	}
	return levelsPayload{Peak: peak, RMS: rms}
}

// levelsForwarder returns the OnLevels callback for one session: it forwards
// each frame to the "levels" event, dropping frames that arrive within
// levelsMinInterval of the last one forwarded.
//
// It is called on internal/gst's bus goroutine — a real GStreamer streaming
// thread at Gate B — so it must not block and must not take any App lock:
// the whole body is one CAS loop and a non-blocking pump send. The throttle
// state is per-forwarder rather than per-App because two sessions never
// overlap (sessMu) and a fresh session deserves a fresh meter, not a 50 ms
// debt inherited from the last one.
//
// now supplies the clock; nil means time.Now, which is what the application
// passes. It is a parameter so the throttle is testable without real sleeps.
func (a *App) levelsForwarder(now func() time.Time) func(gst.Levels) {
	if now == nil {
		now = time.Now
	}
	var last atomic.Int64 // UnixNano of the last frame forwarded; 0 = none yet
	return func(l gst.Levels) {
		t := now().UnixNano()
		for {
			prev := last.Load()
			if prev != 0 && t-prev < int64(levelsMinInterval) {
				// Too soon. Dropping HERE, before the queue, is the point:
				// the pump drops oldest, and a meter must degrade to slower,
				// not to bursty. See levelsMinInterval.
				return
			}
			if last.CompareAndSwap(prev, t) {
				break
			}
		}
		a.events.send(EventLevels, levelsPayload{Peak: l.PeakDB, RMS: l.RMSDB})
	}
}

// channelLevelsForwarder returns the OnChannelLevels callback for one session:
// it records the width the picker is being drawn at and forwards the frame on
// the "channelLevels" event.
//
// THERE IS DELIBERATELY NO THROTTLE, unlike levelsForwarder. The level element
// behind this one runs at gst.channelLevelIntervalNs — 100 ms, ten frames a
// second — which is already SLOWER than levelsMinInterval, the floor a throttle
// would compare against. A throttle here could therefore never drop a frame for
// being too soon; all it could do is add jitter to a stream that is already
// under the limit, by making one frame's arrival time depend on the last one's.
// The rate is enforced where it is decided, in the element.
//
// It runs on a GStreamer streaming thread, so the whole body is one atomic store
// and a non-blocking pump send: no locks, no allocation beyond the payload, and
// nothing that can wait.
func (a *App) channelLevelsForwarder() func(gst.Levels) {
	return func(l gst.Levels) {
		// The width, for the zero-frame the session's end will need. A store per
		// frame rather than a compare-and-store, because ten stores a second of a
		// value that has not changed is cheaper than the branch that would avoid
		// them and this runs where nothing may be slow.
		a.chanLevelWidth.Store(int64(len(l.PeakDB)))
		a.events.send(EventChannelLevels, levelsPayload{Peak: l.PeakDB, RMS: l.RMSDB})
	}
}

// channelMapPayload is the "channelMap" event's wire shape and GetChannelMap's
// return value: what the capture pad negotiated, the routing in force against
// it, and whether that routing is the one nobody chose.
//
// The three fields answer three different questions and the UI needs all three.
// InputChannels sizes the grid — and ONLY it may, because a matrix built against
// any other number stops the capture chain rather than degrading it. Map is what
// is in force right now, which after a live re-route is not what config.json
// says. IsDefault is what separates "nobody has routed this position" from "one
// contribution happens to look like the default", so the screen can say the
// first rather than implying somebody chose it.
type channelMapPayload struct {
	InputChannels int            `json:"inputChannels"`
	Map           gst.ChannelMap `json:"map"`
	IsDefault     bool           `json:"isDefault"`
}

// channelMapPayloadFrom builds one payload, copying the map.
//
// The copy is not defensiveness about aliasing so much as about lifetime: the
// event pump holds a queued payload until the webview reads it, and the cached
// map behind it can be replaced by a live SetChannelMap in the meantime. A
// sixteen-entry copy is nothing; a payload that changed after it was queued
// would be a routing screen showing a state that never existed.
//
// The map is always a non-nil slice, so the wire form is [] rather than null:
// the frontend tests Array.isArray before adopting a map, and null would take
// the "this build cannot tell me" path rather than the "nobody has chosen" one.
func channelMapPayloadFrom(inputChannels int, m gst.ChannelMap) channelMapPayload {
	out := make(gst.ChannelMap, 0, len(m))
	out = append(out, m...)
	return channelMapPayload{
		InputChannels: inputChannels,
		Map:           out,
		IsDefault:     m.IsDefault(),
	}
}

// gstChannelMap transcribes the persisted routing into internal/gst's model.
//
// It is a field-by-field copy of two structs that are deliberately identical —
// see config.ChannelContribution for why they cannot simply be the same type —
// and it preserves the ONE semantic that everything else rests on: an empty
// saved map stays an empty gst.ChannelMap, which means "nobody has chosen" and
// resolves to the default routing at the width the pad negotiated. Returning
// an explicit default here instead would freeze today's default into the
// pipeline and hide the fact that nobody had chosen from the screen that says
// so.
func gstChannelMap(saved []config.ChannelContribution) gst.ChannelMap {
	if len(saved) == 0 {
		return nil
	}
	m := make(gst.ChannelMap, len(saved))
	for i, c := range saved {
		m[i] = gst.ChannelContribution{Output: c.Output, Input: c.Input, Gain: c.Gain}
	}
	return m
}

// currentPipeline returns the running session's pipeline, or nil when no session
// is running.
//
// sessMu is held only to read the pointer and is released before the caller
// touches the pipeline, which is the same discipline GetConformTarget keeps and
// for the same reason: internal/gst takes its own lock across state changes, so
// calling into it under sessMu would couple a UI read to a reconnect.
func (a *App) currentPipeline() gst.Pipeline {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	if a.session == nil {
		return nil
	}
	return a.session.pipe
}

// GetChannelMap reports the DeckLink routing state: the negotiated channel
// count, the map in force, and whether that map is the unchosen default.
//
// THE WIDTH IS READ LIVE, from the pad, on every call — never from the cache the
// event and the domReady replay use. That is what makes this the recovery path
// for the one race the capture chain has: a live source returns NO_PREROLL as
// soon as its state change completes, which can be before the first CAPS event
// has travelled downstream, so the count can genuinely be 0 for a moment after
// Start. Asking again is the whole answer, and the routing screen asks when it
// opens.
//
// It costs a lock internal/gst also holds across state changes, so a call
// arriving during a reconnect waits for it. That is correct here and would not
// be on the main thread: Wails runs a bound method on a goroutine of its own,
// and there is no honest answer to give while the pipeline is being rebuilt
// anyway. domReady, which DOES run on the main thread, replays the cache instead.
//
// No session is not an error. Zero channels draws no grid and the screen says
// why, which is the truth on every machine that has not pressed START.
func (a *App) GetChannelMap() (channelMapPayload, error) {
	width := 0
	if pipe := a.currentPipeline(); pipe != nil {
		width = pipe.InputChannels()
	}

	a.chanMu.Lock()
	a.lastInputChannels = width
	m := a.lastChannelMap
	a.chanMu.Unlock()

	return channelMapPayloadFrom(width, m), nil
}

// SetChannelMap changes which of the capture device's channels reach the feed's
// left and right, LIVE, on the running pipeline.
//
// Measured on the real card: the property write took 119 µs, the pipeline stayed
// PLAYING with nothing pending, and the change was audible in the very next level
// message. That is why the screen calling this has no Apply button.
//
// IT DOES NOT SAVE. The routing screen writes the same map into the Settings form
// and the operator presses Save to keep it for the next launch; this is the live
// control and config.json is the record. Doing both here would mean a routing
// experiment mid-match rewrote the file the next launch reads.
//
// The error is returned VERBATIM and must reach the operator. internal/gst
// validates the map against the negotiated width and writes NOTHING if it does
// not fit — because audioconvert rejects an out-of-range coefficient silently,
// leaving the previous matrix in force with nothing readable afterwards to say
// which one is running. A caller that swallowed this would leave the grid showing
// a routing that is not the one on air.
func (a *App) SetChannelMap(m gst.ChannelMap) error {
	pipe := a.currentPipeline()
	if pipe == nil {
		return errors.New("wslcomms: nothing is sending, so there is no capture pad to route. " +
			"Press START once; the routing can then be changed live, while the feed is running")
	}
	if err := pipe.SetChannelMap(m); err != nil {
		return err
	}

	// Only now is the cache updated, and only on success: it is the record of
	// what is IN FORCE, and a refused write left the previous matrix in force.
	a.chanMu.Lock()
	a.lastChannelMap = m
	width := a.lastInputChannels
	a.chanMu.Unlock()

	// Republished so that a SECOND SEAT follows the change. A remote operator's
	// routing grid must not go on showing the map it last read while somebody at
	// the desk re-routes the commentator; two seats disagreeing about where the
	// audio is coming from is worse than either answer alone.
	a.events.send(EventChannelMap, channelMapPayloadFrom(width, m))
	return nil
}

// publishChannelMap emits the current routing state and refreshes the cache the
// domReady replay reads. It is called once when a session starts, which is when
// the capture pad negotiates and therefore the first moment the width is real.
func (a *App) publishChannelMap(pipe gst.Pipeline) {
	width := 0
	if pipe != nil {
		width = pipe.InputChannels()
	}

	a.chanMu.Lock()
	a.lastInputChannels = width
	m := a.lastChannelMap
	a.chanMu.Unlock()

	a.events.send(EventChannelMap, channelMapPayloadFrom(width, m))
}

// forgetChannelMap publishes the end-of-session state — no negotiated channels,
// no map in force — and clears the cache with it.
//
// Both halves matter, exactly as they do in forgetSignal. Without the EVENT a
// routing grid stays drawn at sixteen channels for a pipeline that no longer
// has a pad, and every crosspoint on it is a control over nothing. Without the
// CACHE CLEAR a page loaded a minute later is told the same width again, which is
// worse because it looks freshly measured.
//
// The MAP is cleared rather than kept, and that is the deliberate half. An empty
// map means "nobody has chosen", which is precisely what the routing screen must
// fall back to once there is no pipeline: it then draws what config.json says,
// rather than a live routing that has stopped being live.
func (a *App) forgetChannelMap() {
	a.chanMu.Lock()
	a.lastChannelMap = nil
	a.lastInputChannels = 0
	a.chanMu.Unlock()

	a.events.send(EventChannelMap, channelMapPayloadFrom(0, nil))
}

// signalPayload is the "signal" event's wire shape.
//
// The lower-case JSON keys are the contract with frontend/src/ui/backend.js, and
// gst.SignalReport is not sent directly for the reason levelsPayload is not:
// its exported Go field names would cross the boundary as State/Flaps and tie
// the frontend to this package's internal naming.
//
// State is one of gst.SignalUnknown/SignalOK/SignalLost — "UNKNOWN", "OK",
// "LOST" — and the frontend MUST NOT draw UNKNOWN as a fault. It is the state of
// every machine with no capture card in it, and of every session before the
// first measurement; it means this application cannot tell, which is a different
// thing from a card telling us there is nothing there.
//
// Flaps is how many times the raw property reading changed between the previous
// report and this one, and it exists because the hysteresis deliberately hides
// something. An input dropping lock twice a second never accumulates a long
// enough run to move the lamp, so it reads as steady OK — correctly, because a
// strobing lamp helps nobody — and this number is what stops that being a lie.
// A non-zero count on a report whose state did not change is a marginal input,
// and a frontend may render it as an "unstable" qualifier beside a lamp that is
// otherwise green.
type signalPayload struct {
	State gst.SignalState `json:"state"`
	Flaps int             `json:"flaps"`
}

// signalPayloadFrom converts one watchdog report to the wire shape. It exists so
// that the field mapping is written once, rather than at the forwarder, the
// replay and the session-end frame separately.
func signalPayloadFrom(r gst.SignalReport) signalPayload {
	// A zero gst.SignalReport — nothing has run yet — carries the empty state
	// rather than "UNKNOWN", and the empty string would reach the frontend as a
	// value no lamp has a case for. Naming it here means every path out of this
	// file speaks the same three words.
	if r.State == "" {
		r.State = gst.SignalUnknown
	}
	return signalPayload{State: r.State, Flaps: r.Flaps}
}

// signalForwarder returns the OnSignal callback for one session: it records the
// report for the domReady replay and publishes it on the "signal" event.
//
// THERE IS NO THROTTLE HERE, and that is a decision rather than an omission.
// internal/gst already bounds this: a report is produced only when the debounced
// state changes, or when the raw reading has flapped signalFlapAlert times since
// the last report — which needs at least that many poll intervals, so the worst
// case is about one event per second, the rate m2lx.Watcher emits status at. The
// levels throttle exists because a level element posts twenty messages a second
// whatever is happening; nothing analogous applies to a lamp that speaks when
// something changes.
//
// It takes sigMu, unlike levelsForwarder, which takes nothing. The difference is
// the thread: OnLevels is called on a GStreamer streaming thread, where this
// package's rule is that no lock may be taken at all, whereas the signal
// watchdog polls from an ordinary goroutine of its own. sigMu is a leaf and the
// only thing done under it is one assignment, so it cannot block the watchdog
// past the next poll.
func (a *App) signalForwarder() func(gst.SignalReport) {
	return func(r gst.SignalReport) {
		a.sigMu.Lock()
		a.lastSignal = r
		a.sigMu.Unlock()

		a.events.send(EventSignal, signalPayloadFrom(r))
	}
}

// forgetSignal publishes the UNKNOWN frame that ends a session and clears the
// replay cache with it.
//
// Both halves matter and they fail differently. Without the EVENT, the lamp
// keeps whatever it last showed for a pipeline that has been torn down — a green
// signal lamp beside a grey SENDING lamp. Without the CACHE CLEAR, the event is
// right and a page loaded a minute later is told the stale value all over again,
// which is the worse of the two because it looks freshly measured.
func (a *App) forgetSignal() {
	cleared := gst.SignalReport{State: gst.SignalUnknown}

	a.sigMu.Lock()
	a.lastSignal = cleared
	a.sigMu.Unlock()

	a.events.send(EventSignal, signalPayloadFrom(cleared))
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
// names the credential-store target rather than its contents.
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

// eventPump carries events from their producers to the webview renderer
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
	// production through the webview's own keyboard shortcuts — and two consumers
	// would split the queue between them, so each event would reach the page
	// once but the ordering between them would no longer hold.
	startOnce sync.Once

	// tee, if set, receives every event the pump delivers to the local renderer,
	// so the SAME stream reaches every connected remote seat. It is the single
	// tap point the remote bridge hooks: called once per delivered event, from
	// the pump's one consumer goroutine, right after wailsruntime.EventsEmit — so
	// a remote client sees exactly the events the local page sees, including the
	// drops (a client that fell behind detects its own gap through the frame seq,
	// see internal/remote/session.go). It must not block; broadcastRemote fans out
	// through per-session drop-oldest queues and returns immediately. Nil until
	// NewApp installs App.broadcastRemote, and harmless when nil.
	tee func(name string, data any)
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
					// TEE: the same event, to every connected remote seat. This is
					// the one tap point — after the local emit, so remote sees what
					// local sees and no event is broadcast that the pump dropped
					// before reaching here. tee never blocks; see the field comment.
					if p.tee != nil {
						p.tee(e.name, e.data)
					}
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
