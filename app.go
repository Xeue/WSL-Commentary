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

	// pictureDial builds the picture monitor, and overlayDial builds the native
	// overlay window. Both are gst's real constructors in the application and
	// fakes in the tests, which is the only way to exercise the wire-up without a
	// GPU and without a window. Nil means the real one; see
	// App.newPictureMonitor and App.newPictureOverlay.
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

	if err := a.preflightAudioDevice(cfg.AudioDeviceID); err != nil {
		return err
	}

	passphrase, err := a.srtPassphrase(cfg)
	if err != nil {
		return err
	}

	pipe, err := gst.New()
	if err != nil {
		return fmt.Errorf("wslcomms: creating the media pipeline: %w", err)
	}

	snd := a.newSender(pipe)
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

// senderOpts builds the options for one session from a configuration snapshot
// and the passphrase already read from the credential store.
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

			// The input meters. The pipeline's level element measures what is
			// actually encoded and sent and calls this on a streaming thread
			// (gst.PipelineOpts.OnLevels: MUST NOT BLOCK) — the forwarder is a
			// throttle in front of the non-blocking event pump, so it cannot.
			OnLevels: a.levelsForwarder(nil),
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
func silentLevelsPayload() levelsPayload {
	return levelsPayload{
		Peak: []float64{levelsSilenceDB, levelsSilenceDB},
		RMS:  []float64{levelsSilenceDB, levelsSilenceDB},
	}
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
