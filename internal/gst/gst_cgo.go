//go:build cgo && !gststub

// This file is the real, go-gst backed implementation. It compiles only with
// CGO_ENABLED=1 and needs MinGW gcc, pkg-config and the GStreamer 1.28.5
// mingw-x86_64 DEVELOPMENT installer on the build host — that is Gate B.
//
// It has never been compiled. There is no MinGW gcc and no GStreamer on the
// machine it was written on, so every line below is written defensively and
// every assumption about go-gst v0.0.2's API or about GStreamer's internal
// behaviour is stated in a comment where it is relied on. BUILD-NOTES.md in
// this directory is the companion document: read it before trying to build.
//
// If go-gst v0.0.2 fights the MinGW build (open issue #179, no Windows CI),
// the fallback agreed in the specification is a roughly 200-line hand-written
// cgo shim over about fifteen C entry points, behind these same signatures —
// that is a change inside this file, not a change to the contract.
//
// # The single most important property of this file
//
// EVERYTHING UPSTREAM OF srtq STAYS IN PLAYING FOR THE LIFE OF THE PROCESS.
//
// wasapi2src, the slate branch, both encoders, both parsers and mpegtsmux are
// taken to PLAYING once, by Start, and are not touched again until Stop. A
// reconnect replaces the srtsink and nothing else. Running time is
// clock − base_time; taking any of those elements to READY or NULL re-samples
// the clock, resets base time and makes mpegtsmux restart PTS from zero. That
// is the measured bug: audio DTS jumping backwards by exactly the previous
// run's uptime, 1,523 non-monotonic errors downstream, commentary never
// returning while every indicator read green. The structural fix is that the
// path which actually happens during a match never goes near a state change.
//
// The secondary defence, for the one case that does force a rebuild — the user
// picking a different DVS device mid-match — is savedBase below: one
// process-lifetime base time, reused verbatim on every rebuild, forever.
//
// # Threading model
//
// Five kinds of thread touch a cgoPipeline. They are listed here because the
// locking below only makes sense against this list.
//
//  1. The caller's goroutine. internal/sender calls Start, ReplaceSink,
//     ForceKeyUnit and Stop from its own single goroutine, but the interface
//     promises concurrency safety, so every one of those methods takes p.mu for
//     its whole duration. All slow work — element state changes, and therefore
//     the SRT caller handshake — happens here and nowhere else.
//
//  2. GStreamer streaming threads. These run the pad probe callback
//     (gateProbe). That callback does exactly one atomic load and returns. It
//     takes no lock, allocates nothing, and never calls into application code.
//     A pad probe that blocks is a pipeline that deadlocks, so this one cannot
//     block by construction.
//
//  3. Whichever thread posts a GST_ELEMENT_ERROR. gst_element_post_message
//     calls the bus sync handler synchronously, on the posting thread, before
//     the failing function has returned. onBusMessage therefore runs on a
//     streaming thread and must not take p.mu — ReplaceSink holds p.mu while
//     provoking exactly these messages, so taking it there would deadlock. It
//     uses atomics and p.errMu (a RWMutex held only for a non-blocking channel
//     send) instead.
//
//  4. The consumer of Errors(). Reads a buffered channel. Sends into it never
//     block: if it is full the error is dropped, per the contract in gst.go.
//
//  5. The warning-log goroutine, started by New and ended by Stop. It exists so
//     that no streaming thread ever calls log.Printf. Go's log package takes a
//     process-global mutex and writes to stderr, so under the warning storm a
//     marginal SRT link produces, logging from onBusMessage would serialise
//     every streaming thread in the pipeline behind one file handle. The bus
//     handler does a non-blocking send instead; this goroutine does the I/O.
//
//  6. Go's garbage collector, via go-gst's finalizers. Every GStreamer object
//     this file keeps across calls is stored in a struct field so that it stays
//     reachable; dropping a Go reference to a live GstElement would unref it.
//
// # The gate, and why the pad probe drops instead of blocking
//
// The obvious implementation of ReplaceSink is the textbook GStreamer
// dynamic-relink idiom: add a GST_PAD_PROBE_TYPE_BLOCK_DOWNSTREAM probe and do
// the relinking inside the probe callback. That cannot be used here. The slow
// step of a reconnect is the SRT caller handshake, which happens inside
// srtsink's NULL→PLAYING transition and can take seconds — libsrt's default
// SRTO_CONNTIMEO alone is 3 s. Doing that inside a probe callback parks a
// GStreamer streaming thread for seconds, which stalls mpegtsmux, which
// back-pressures the encoders, which bunches the timestamps of a live capture.
// The cure would cause the disease.
//
// So the probe is a gate, not a block. Two probes — one on srtq's sink pad, one
// on srtq's src pad — share one atomic flag. While the gate is closed they
// return GST_PAD_PROBE_DROP for buffers and buffer lists; while it is open they
// return GST_PAD_PROBE_OK. Dropping is not a compromise: srtq is
// leaky=downstream precisely so that output produced during an outage is
// discarded rather than back-pressuring the live capture (specification
// section 6.2). The gate drops the same data the queue would have dropped, a
// few microseconds earlier.
//
//   - The SRC-pad gate stops the queue's loop pushing into a sink that is
//     absent, half-attached or still handshaking, which would otherwise set the
//     queue's srcresult to GST_FLOW_NOT_LINKED and pause its task.
//
//   - The SINK-pad gate stops mpegtsmux entering gst_queue_chain at all while
//     the queue is unhealthy. gst_queue_chain returns the queue's stored
//     srcresult to its caller, so without this gate a single srtsink write
//     failure propagates from the queue up into mpegtsmux, which pauses its
//     source task and posts its own flow error — and the capture chain, which
//     was never supposed to be affected by a reconnect, is wedged.
//
//   - The SINK pad carries a SECOND probe, for downstream EVENTS, because
//     gst_queue_chain is not the only door into the queue.
//     gst_queue_handle_sink_event refuses a serialized event whenever
//     srcresult is bad, and it refuses it by posting GST_ELEMENT_ERROR and
//     returning FALSE — which is the same wedge by a different route, and it
//     was measured happening on a real peer loss. That probe drops an event
//     only while the queue's loop is actually stopped, never merely because
//     the gate is shut; see eventGateProbe for why the distinction is
//     load-bearing and what it costs.
//
// The probe mask is deliberately GST_PAD_PROBE_TYPE_BLOCK | _BUFFER |
// _BUFFER_LIST and NOT GST_PAD_PROBE_TYPE_BLOCK_DOWNSTREAM. BLOCK_DOWNSTREAM
// also covers GST_PAD_PROBE_TYPE_EVENT_DOWNSTREAM, and dropping a downstream
// event is not free: gstpad.c's push_sticky() marks a sticky event as received
// when the push returns GST_FLOW_OK, and a dropped probe returns GST_FLOW_OK.
// Gating events would therefore let STREAM_START, CAPS and SEGMENT be recorded
// as delivered to a sink that never saw them, and the first buffer after the
// gate reopened would arrive at srtsink with no segment. Keeping BLOCK in the
// mask matters for the same reason from the other direction: gst_pad_push_data
// runs the blocking probes BEFORE it re-pushes pending sticky events, so a drop
// at that point leaves the sticky events pending and they are delivered
// correctly the moment the gate opens.
//
// # Known residual risk
//
// There is one window this design does not close, and the person running the
// match-length soak should know about it. When srtsink's render() fails it
// posts GST_ELEMENT_ERROR — which reaches onBusMessage, on that same thread,
// and closes the gate — and only then returns GST_FLOW_ERROR to the queue,
// which stores it in srcresult. If a buffer from mpegtsmux passed the sink-pad
// gate microseconds before the gate closed and is blocked on the queue's mutex,
// it will read the poisoned srcresult and carry GST_FLOW_ERROR up into the
// muxer. The window is microseconds wide against a ~4.6 ms buffer period
// (roughly 218 transport-stream buffers per second at the specified
// 2.3 Mbit/s), but it is not zero. If it is ever hit, the symptom is a bus
// error whose source is mux rather than srtout-N; this file classifies that as
// pipeline-fatal, refuses all further ReplaceSink calls with it, and reports it
// on Errors(), so the failure is loud instead of a false green. Recovery is
// Stop, New, Start — which savedBase makes timestamp-safe.
package gst

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-gst/go-glib/pkg/gobject/v2"
	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// Timeouts.
//
// READ THIS BEFORE RELYING ON ONE OF THEM: they do NOT bound a state change.
// They bound only the ASYNCHRONOUS TAIL of one. go-gst v0.0.2's BlockSetState
// (element_manual.go) is, in full:
//
//	ret := el.SetState(state)
//	if ret == StateChangeAsync {
//		_, _, ret = el.GetState(timeout)
//	}
//	return ret
//
// gst_element_set_state itself takes no timeout and cannot be given one. It
// holds the element's GST_STATE_LOCK and runs every change_state function
// downwards through the bin synchronously on the calling goroutine. If
// wasapi2src's NULL→READY blocks inside IAudioClient::Initialize on a wedged
// WASAPI endpoint, SetState never returns, the timeouts below are never
// consulted, and Start hangs forever holding p.mu.
//
// A watchdog was considered and rejected. There is nothing to cancel:
// gst_element_set_state offers no abort, the thread is inside C and cannot be
// preempted, and it holds the pipeline's state lock — so the "recovery" path
// (teardownLocked, which sets the pipeline to NULL) would block on that same
// lock the instant it ran. A watchdog would turn one wedged goroutine into a
// wedged goroutine plus a leaked one plus a misleading error return. What is
// implemented instead is stateChangeWatchdog below: it cannot unwedge anything,
// it only makes the hang legible in the field log rather than silent.
//
// The real mitigation is architectural and belongs to the caller: Start,
// ReplaceSink and Stop must never be called from the Wails message loop. See
// BUILD-NOTES.md section 7.
const (
	// pipelineStartTimeout bounds the asynchronous tail of the NULL→PLAYING
	// transition of the capture and encode chain, and is the interval after
	// which the watchdog starts complaining about the synchronous part. The
	// costly parts are opening the WASAPI endpoint in shared mode and
	// initialising the Media Foundation encoder MFTs; ten seconds is far beyond
	// either.
	pipelineStartTimeout = 10 * time.Second

	// sinkStateChangeTimeout bounds the asynchronous tail of the NULL→PLAYING
	// transition of a fresh srtsink. It does NOT bound the SRT caller
	// handshake: async=false means srtsink connects inside its READY→PAUSED
	// change_state, which runs synchronously inside SyncStateWithParent, which
	// takes no timeout argument at all. libsrt's own SRTO_CONNTIMEO — 3 s by
	// default, not overridden by srtsink — is the only thing bounding a
	// connect, and it is the reason a dead listener still fails in seconds. The
	// specification measures a successful lock at about 1.1 s.
	sinkStateChangeTimeout = 10 * time.Second

	// elementShutdownTimeout bounds the asynchronous tail of taking a single
	// element to NULL. Setting a disconnected srtsink to NULL closes a socket;
	// it does not block.
	elementShutdownTimeout = 5 * time.Second

	// watchdogInterval is how long a synchronous state change may run before
	// stateChangeWatchdog logs that it is still running, and how often it
	// repeats afterwards. It is deliberately shorter than every timeout above:
	// the point is to have a log line already written by the time a human
	// notices the application has stopped responding.
	watchdogInterval = 3 * time.Second

	// hostResolveTimeout bounds the DNS lookup ReplaceSink performs before it
	// builds the srtsink URI (see resolveSinkHost). Unlike the constants above
	// this one IS a real bound: Go's Windows resolver runs getaddrinfo on its
	// own goroutine and abandons it when the context expires, so a dead DNS
	// server costs this and no more.
	//
	// It has to be short. ReplaceSink holds p.mu for its whole duration and the
	// caller is internal/sender's state machine, whose first backoff rung is
	// 7 s; a lookup allowed to run longer than a rung would push every retry
	// out of step with M2L-X's roughly five second re-accept window. Three
	// seconds is an order of magnitude beyond a working resolver's answer and
	// still well inside a rung.
	hostResolveTimeout = 3 * time.Second
)

// errorChannelBuffer is how many asynchronous errors are held before further
// ones are dropped. It matches stubErrorBuffer in gst_stub.go so that the two
// implementations behave identically under a slow consumer. The contract in
// gst.go requires dropping rather than blocking: the sender of these errors is
// a GStreamer streaming thread and it must never wait on a Go consumer.
const errorChannelBuffer = 16

// warningChannelBuffer is how many bus warnings are held for the logging
// goroutine before further ones are dropped.
//
// Warnings are NOT errors and never reach Errors(): internal/sender would
// count one as a connection failure. They exist only for the field log. They go
// through a channel for the same reason errors do — the poster is a GStreamer
// streaming thread, and a marginal SRT link produces warnings faster than
// stderr accepts them. Sixteen is a storm's worth; past that the log line count
// is the diagnosis and the individual messages add nothing.
const warningChannelBuffer = 16

// srtSinkNamePrefix is prepended to a serial number to name each srtsink
// uniquely — srtout-1, srtout-2 and so on.
//
// The specification's pipeline string names the element srtout, but Start does
// not create a sink at all, so the name is ours to choose. It is made unique so
// that a bus error can be attributed to a specific reconnect attempt: during a
// swap the outgoing sink and the incoming sink would otherwise share a name,
// and a late error from the one being torn down would be misread as a failure
// of the one being installed. The prefix is also how onBusMessage tells a sink
// error (expected; the caller retries) from an error anywhere else in the
// pipeline (pipeline-fatal; see the file comment).
const srtSinkNamePrefix = "srtout-"

// Element names inside the parsed pipeline. These are the only names this file
// looks up, and they match the specification's section 5 string.
const (
	nameMux        = "mux"    // mpegtsmux
	nameSRTQueue   = "srtq"   // the leaky queue whose src pad feeds the sink
	nameSlateSrc   = "slate"  // filesrc reading the slate PNG
	nameAudioSrc   = "asrc"   // the platform capture source: captureSourceFactory
	nameAudioConv  = "aconv"  // audioconvert, whose mix-matrix carries the channel map
	nameVideoEncod = "venc"   // the H.264 encoder chosen at runtime
	nameVideoScale = "vscale" // videoscale, so a slate that is not the conform size still starts
)

// The LIVE CAPTURE video leg's element names.
//
// The capture source itself is NOT here: it is nameVideoCaptureSrc, declared in
// capturefault.go beside the fault classifier that reads it, because that
// decision has to be testable at Gate A where this file does not compile. These
// are its conform chain, and every one of them begins with
// videoCaptureNamePrefix for a reason that is worth more than the tidiness of a
// shared prefix.
//
// classifyBusError decides whether a bus error takes the commentary off air BY
// ELEMENT NAME. An element left unnamed in the parse string gets a GStreamer
// name of its own — "videoconvert0", "deinterlace0" — which matches no prefix
// and therefore rejoins the FATAL default. So a conform chain written without
// these names does not degrade to a frozen picture when the camera is unplugged
// mid-match: it takes the commentary off air, silently, for a video fault, on a
// pipeline whose audio was perfectly healthy. Naming them is what makes
// capturefault.go's prefix rule true of the whole leg rather than of the source
// alone.
//
// They are separate from the slate leg's names rather than shared with them
// because the two legs are never both built, and a log line naming vcapscale
// says which leg the reader is looking at without having to know what was
// configured.
const (
	nameVideoCapConv  = "vcapconv"  // videoconvert: whatever the card gives us into NV12
	nameVideoCapDeint = "vcapdeint" // deinterlace: the only thing that handles a 1080i camera
	nameVideoCapScale = "vcapscale" // videoscale: the camera's raster into the conform target's
	nameVideoCapRate  = "vcaprate"  // videorate: MANDATORY, see pipelineDescription
	nameVideoCapTee   = "vcaptee"   // tee: the broadcast branch, plus an optional preview
	nameVideoCapQueue = "vcapq"     // the broadcast branch's head queue
)

// nameVideoCaptureClockSink is the fakesink the CLOCK COMPANION feeds: the
// decklinkvideosrc that exists only so decklinkaudiosrc can preroll, on a seat
// whose picture is the slate.
//
// It begins with videoCaptureNamePrefix like everything else in that leg, and
// that is load-bearing rather than tidy. classifyBusError decides by name; an
// unprefixed sink would rejoin the FATAL default, so a fakesink erroring on a
// seat whose commentary is fine would take the commentary off air. With the
// prefix it is classVideoCapture, which — because the commentary IS clocked by
// this element on every pipeline that has it — is then upgraded to
// classAudioCapture by the second stage in onBusMessage. Both answers are
// correct and the difference between them is the difference between a named
// audio fault and a nameless pipeline death.
//
// The companion source itself is nameVideoCaptureClock, declared in
// capturefault.go beside the classifier that reads it.
const nameVideoCaptureClockSink = "vcapclocksink"

// videoCaptureFactory is the element that opens the card's video input.
//
// It is NOT in elements_windows.go / elements_darwin.go, and its absence from
// that seam is the statement that the two ports agree here: the decklink plugin
// is one upstream source tree built against Blackmagic's own SDK, and the
// factory is decklinkvideosrc on both. The seam exists for the elements that
// genuinely differ — the platform's own capture source and AAC encoder — and
// putting an identical constant in both halves would be two places to change a
// name that can only ever change in one.
const videoCaptureFactory = "decklinkvideosrc"

// audioCaptureFactory is the element that opens the card's EMBEDDED AUDIO: the
// sixteen channels a DeckLink presents alongside its video input.
//
// It is beside videoCaptureFactory and NOT in the elements_*.go platform seam,
// for the identical reason: one upstream plugin, built against Blackmagic's own
// SDK, with the same factory name on both ports. The seam exists for the
// elements that genuinely differ — the platform's own capture source and AAC
// encoder — and this is not one of them.
//
// It never appears in the parse string with a `connection` property. See
// pipelineDescription: connection PERSISTENTLY RECONFIGURES THE CARD and
// overrides Blackmagic Desktop Video Setup, and it has had to be undone by hand
// twice. If the card is silent the answer is never another connection value.
const audioCaptureFactory = "decklinkaudiosrc"

// propMixMatrix is audioconvert's routing matrix: the property that says which
// input channel reaches which output, and at what gain. It is the whole of the
// channel-map mechanism at the GStreamer end; channelmap.go is the model above
// it and holds the measurements.
//
// It is named here rather than written as a literal for the reason
// propPersistentID is: the name appears in a setter, in a hasProperty guard and
// in the error a build without it would produce, and those three must agree.
const propMixMatrix = "mix-matrix"

// propPostMessages is the level element's gboolean switch for whether it posts
// its measurements to the bus at all.
//
// It is what makes the per-channel picker meter something that can be turned on
// when there is a reason to have it and left off otherwise, on a pipeline
// already carrying commentary: measured live at 61 us, with that element's
// messages stopping dead and the other level element in the same pipeline
// carrying on untouched. See armChannelMeterLocked.
const propPostMessages = "post-messages"

// levelStructureName is the name of the GstStructure a level element posts in
// its GST_MESSAGE_ELEMENT bus messages ("level", fixed by gst-plugins-good).
//
// onBusMessage matches on it FIRST, because it is the documented contract for a
// level message and is what separates a level report from every other element
// message on this bus. It is not, on its own, enough to say which meter the
// report belongs to: EVERY level element in the process posts this same name,
// so the source element is tested straight afterwards. The element names, and
// the routing they drive, are levelElementName / channelLevelElementName and
// levelKindForSource in levels.go — untagged, so Gate A can check them.
const levelStructureName = "level"

// Package-level GStreamer initialisation state. Init is idempotent because
// gst_init is not: calling it twice is harmless in current GStreamer but the
// environment variables it depends on must be set before the first call and
// must not be changed afterwards, since the plugin registry is scanned once.
var (
	initOnce sync.Once
	initErr  error
	inited   atomic.Bool
)

// savedBase is the process-lifetime pipeline base time, in nanoseconds on the
// pinned system clock. Specification section 6.1.
//
// It is sampled from the system clock the first time a pipeline is started and
// is then NEVER reset, not on Stop, not on Start, not on a device change. Every
// pipeline this process ever builds gets this same value, so mpegtsmux's
// running time — clock − base_time — continues from where the previous
// pipeline left off instead of restarting at zero. A restart from zero is
// exactly the measured backwards-DTS bug, and M2L-X's relay forwards our
// timestamps verbatim, so nothing downstream recovers from it.
//
// ClockTimeNone (0xFFFFFFFFFFFFFFFF) is the "not yet sampled" sentinel, which
// is why the guard below compares against it rather than against zero: zero is
// a legitimate clock reading.
var (
	savedBaseMu sync.Mutex
	savedBase   = gogst.ClockTimeNone
)

// requiredElement is one entry in the bundle contract: a factory the pipeline
// cannot be built without, and the plugin from the allowlist that provides it.
//
// It is a named type rather than the anonymous struct it used to be so that
// the platform halves of the list — platformRequiredElements in
// elements_windows.go and elements_darwin.go — can be declared in their own
// files and appended here.
type requiredElement struct{ factory, plugin string }

// requiredElements is the set of element factories the pipeline cannot be
// built without. Init checks all of them and reports every one that is missing.
//
// This check is the difference between "the app will not start, and here is the
// plugin that is missing from the bundle" and "the app starts, the user presses
// Start twenty minutes before kick-off, and gst_parse_launch says no such
// element". The H.264 encoder is deliberately absent from this list because it
// is resolved by rank at runtime (specification open question 3).
//
// The fourteen entries below are the ones that are the SAME on every platform,
// under identical factory AND plugin names — verified against Homebrew's
// GStreamer 1.26.10 on macOS arm64 on 2026-08-14, factory by factory, because
// "it is probably called the same thing" is how a bundle ships without a
// resampler. The capture source and the AAC encoder are not among them and
// come from platformRequiredElements, which is where the two ports disagree.
var requiredElements = append([]requiredElement{
	{"filesrc", "coreelements"},
	{"queue", "coreelements"},
	{"capsfilter", "coreelements"},
	{"pngdec", "png"},
	{"imagefreeze", "imagefreeze"},
	{"videoconvert", "videoconvertscale"},
	{"videoscale", "videoconvertscale"},
	{"audioconvert", "audioconvert"},
	{"audioresample", "audioresample"},
	{"h264parse", "videoparsersbad"},
	{"aacparse", "audioparsers"},
	{"level", "level"},
	{"mpegtsmux", "mpegtsmux"},
	{"srtsink", "srt"},
}, platformRequiredElements...)

// videoCaptureRequiredElements are the four factories the LIVE CAPTURE video leg
// needs and no other configuration does.
//
// THEY ARE A SEPARATE LIST AND ARE DELIBERATELY NOT IN requiredElements, and the
// reason is the difference between two failures rather than tidiness. A missing
// entry in requiredElements makes Init return an error, which makes the
// application refuse to launch at all. That is exactly right for a resampler
// every seat needs; it is exactly wrong for a plugin only a seat with a card in
// it can use, because it would take a commentary position that has never seen a
// DeckLink off the air over a bundle defect that could not possibly affect it.
//
// So the check runs twice, in two different registers. Init reports a missing
// one as a LOG LINE — the bundle-contract diagnosis, present on every start of
// every seat, which is what somebody staging a bundle needs — and Start refuses
// WITH A NAMED ERROR when a capture is actually configured, which is what the
// operator whose card will not open needs. Neither of those is the twenty
// minutes before kick-off failure the requiredElements comment is about: on the
// seat that needs these, the refusal is at Start with the plugin named, and it
// is the same moment gst_parse_launch would have failed anyway, with a better
// sentence.
//
// tee is in coreelements and cannot be missing from a GStreamer that has queue
// in it, and it is listed anyway. The list's value is that it is exhaustive: a
// reader staging a bundle should be able to satisfy the capture leg from this
// one place, and an entry that is always present costs one registry lookup at
// Init.
var videoCaptureRequiredElements = []requiredElement{
	{videoCaptureFactory, "decklink"},
	{"deinterlace", "deinterlace"},
	{"videorate", "videorate"},
	{"tee", "coreelements"},
}

// audioCaptureRequiredElements are the factories a DECKLINK COMMENTARY seat
// needs, and it is a separate list for the same two-registers reason
// videoCaptureRequiredElements is: Init reports, Start refuses.
//
// decklinkvideosrc IS IN IT, and that is the entry a reader will want to
// delete. It is here because a DeckLink audio seat cannot start without one —
// measured, decklinkaudiosrc produces zero buffers with no video element in the
// pipeline — whether the operator asked for a picture from the card or not. A
// bundle with the decklink plugin present has both, so this can only ever fire
// together with the video leg's entry; listing it anyway is what makes the list
// answer "what does THIS seat need" without the reader having to know that the
// audio drags a video element in with it.
//
// fakesink is coreelements and cannot be missing from a GStreamer that has queue
// in it. It is listed for videoCaptureRequiredElements' reason about tee: the
// value of the list is that it is exhaustive, and an entry that is always
// present costs one registry lookup at Init.
var audioCaptureRequiredElements = []requiredElement{
	{audioCaptureFactory, "decklink"},
	{videoCaptureFactory, "decklink"},
	{"fakesink", "coreelements"},
}

// initEnvVar is one environment variable that has to be in place before
// gst_init, as a name and a value.
//
// It is a named type for the same reason requiredElement is: the platform halves
// of the list — extraInitEnv in gstpaths_windows.go and gstpaths_darwin.go —
// are declared in their own files and consumed here, so doInit needs no
// runtime.GOOS branch and neither platform's answer is expressed in the other's
// vocabulary.
type initEnvVar struct{ name, value string }

// Init prepares the bundled GStreamer for use and calls gst_init.
//
// appDir is the directory holding the executable: the directory holding
// wslcomms.exe on Windows, and <App>.app/Contents/MacOS on macOS. Before
// gst_init, Init sets all four of these to the bundled plugin directory —
// <appDir>\gst\lib\gstreamer-1.0 on Windows, <App>.app/Contents/Resources/
// gstreamer-1.0 on macOS, see bundlePluginDir — plus a per-user registry:
//
//	GST_PLUGIN_SYSTEM_PATH_1_0 = <pluginDir>
//	GST_PLUGIN_SYSTEM_PATH     = <pluginDir>
//	GST_PLUGIN_PATH_1_0        = <pluginDir>
//	GST_REGISTRY_1_0           = <per-user cache>\WSLComms\registry.bin
//
// and then whatever extraInitEnv adds, which on Windows is nothing and on macOS
// is four more: both spellings of GST_PLUGIN_SCANNER, GIO_MODULE_DIR and
// ORC_CODE. Those four exist because Homebrew paths are compiled into the
// vendored dylibs as C strings that install_name_tool cannot rewrite; the
// reasoning is in gstpaths_darwin.go, where a reader will meet it next to the
// paths themselves.
//
// Go's os.Setenv calls SetEnvironmentVariableW on Windows and setenv(3) on
// macOS, both of which are what GLib reads, so this crosses the cgo boundary
// cleanly on both. The effect is that any GStreamer installed elsewhere on the
// machine is invisible to this process, and this process's bundle is invisible
// to it. Getting this wrong does not fail loudly: the app silently loads some
// other GStreamer, which is why Init verifies the bundle afterwards by looking
// up every element the pipeline needs.
//
// Init must be called exactly once, before New or ListInputDevices. It is
// idempotent: subsequent calls return the first call's result and do not
// re-enter gst_init.
func Init(appDir string) error {
	initOnce.Do(func() {
		initErr = doInit(appDir)
		if initErr == nil {
			inited.Store(true)
		}
	})
	return initErr
}

// doInit is Init's body, factored out so that initOnce guards exactly one
// execution of it.
func doInit(appDir string) error {
	if appDir == "" {
		return errors.New("gst: Init: appDir is required")
	}
	abs, err := filepath.Abs(appDir)
	if err != nil {
		return fmt.Errorf("gst: Init: resolving appDir %q: %w", appDir, err)
	}

	pluginDir := bundlePluginDir(abs)
	if fi, err := os.Stat(pluginDir); err != nil {
		return fmt.Errorf("gst: Init: bundled plugin directory %q is not readable: %w", pluginDir, err)
	} else if !fi.IsDir() {
		return fmt.Errorf("gst: Init: bundled plugin path %q is not a directory", pluginDir)
	}

	// All three plugin-path variables are pointed at the bundle, and none of
	// them is set to the empty string.
	//
	// This is NOT because the empty-string idiom is broken. It was claimed to
	// be, and the claim was tested and is false. The claim: os.Setenv calls
	// SetEnvironmentVariableW, Windows cannot represent an empty value, the
	// variable is deleted, and GLib then sees it as unset. The measurement, on
	// Windows 11 26200 with Go 1.24.5 — TestEmptyEnvVarSurvivesOnWindows in
	// gst_stub_test.go, which runs at Gate A:
	//
	//	PARENT LookupEnv -> value="" present=true
	//	PARENT Environ entry -> "WSLCOMMS_EMPTY_ENV_PROBE="
	//	CHILD  LookupEnv -> value="" present=true
	//
	// The entry survives in the block and is inherited by a child process, so
	// GetEnvironmentVariableW did not report ERROR_ENVVAR_NOT_FOUND. (The
	// folklore is real but belongs to the wrappers: cmd.exe's `set FOO=` calls
	// SetEnvironmentVariable with NULL, the CRT's _putenv("FOO=") deletes, and
	// .NET normalises an empty value to null — the same probe run through
	// PowerShell prints $null. None of those is the Win32 API, and none of them
	// is what Go calls.)
	//
	// What is set here is nevertheless a path rather than "", because of the
	// one link in the chain that CANNOT be tested at Gate A: whether GLib's
	// g_getenv maps an empty value to "" or to NULL. Reading gutils.c it
	// returns "", and gstregistry.c then splits "" into zero directories and
	// scans nothing, which is the wanted behaviour. But that is a reading, not
	// a measurement, and if it is wrong the fallthrough is to the compiled-in
	// system directories — silently loading a foreign GStreamer.
	//
	// A non-empty path is correct under BOTH behaviours: the system search path
	// becomes exactly the bundle, whether GLib reports it as set or not. It
	// costs one redundant scan of a directory that is already
	// GST_PLUGIN_PATH_1_0, which gstregistry deduplicates by filename. The
	// unversioned name is set for the same reason — it is the fallback
	// gstregistry.c consults when the versioned one is absent.
	//
	// missingElements() below does not back any of this up. It detects
	// factories that are ABSENT; it cannot detect a foreign install's copy of
	// the same factory outranking ours. The Gate B assertion in BUILD-NOTES.md
	// section 8 — every plugin in gst_registry_get_plugin_list() has a filename
	// under pluginDir — is what actually proves this worked.
	for _, name := range []string{
		"GST_PLUGIN_SYSTEM_PATH_1_0",
		"GST_PLUGIN_SYSTEM_PATH",
		"GST_PLUGIN_PATH_1_0",
	} {
		if err := os.Setenv(name, pluginDir); err != nil {
			return fmt.Errorf("gst: Init: setting %s: %w", name, err)
		}
		if got, ok := os.LookupEnv(name); !ok || got != pluginDir {
			return fmt.Errorf("gst: Init: %s did not take: LookupEnv returned %q, %v", name, got, ok)
		}
	}

	registryPath, err := registryFile()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(registryPath), 0o755); err != nil {
		return fmt.Errorf("gst: Init: creating %q: %w", filepath.Dir(registryPath), err)
	}
	if err := os.Setenv("GST_REGISTRY_1_0", registryPath); err != nil {
		return fmt.Errorf("gst: Init: setting GST_REGISTRY_1_0: %w", err)
	}

	// The platform's own additions, AFTER the shared four and BEFORE gst_init,
	// because two of the macOS ones (the scanner path and GIO_MODULE_DIR) are
	// read during the registry rebuild that gst_init performs. Setting them
	// afterwards would set them for the second run of the application and not
	// this one, which is the shape of bug that reproduces only on a clean
	// machine.
	extra, err := extraInitEnv(abs)
	if err != nil {
		return err
	}
	// Logged by NAME AND VALUE as they are set, and BEFORE gst_init rather than
	// after a successful one, because the failure they prevent is silent by
	// construction: a wrong scanner path makes GStreamer fall back to in-process
	// scanning with a warning nobody reads, and a foreign GIO module directory
	// produces no message at all. Logging here means the lines are present even
	// when the run that follows them fails, which is the run somebody will be
	// reading the log of.
	for _, v := range extra {
		if err := os.Setenv(v.name, v.value); err != nil {
			return fmt.Errorf("gst: Init: setting %s: %w", v.name, err)
		}
		log.Printf("gst: %s=%q", v.name, v.value)
	}

	gogst.Init()

	if missing := missingElements(); len(missing) > 0 {
		// bundleAllowlistNoun, not a fixed word: the Windows build documents
		// this as the "DLL allowlist" in seven places, so that is the phrase a
		// Windows operator reading this line will search for, and it is wrong
		// about a .app. See deviceprovider_windows.go for the full argument.
		return fmt.Errorf(
			"gst: Init: the bundled GStreamer in %q is incomplete: %s "+
				"(check the %s allowlist and that the registry at %q was rebuilt)",
			pluginDir, strings.Join(missing, ", "), bundleAllowlistNoun, registryPath)
	}

	// The live capture leg's plugins, reported and NOT refused. See
	// videoCaptureRequiredElements for why this is a log line and the block above
	// is an error: a seat with no card must not be stopped from going on air by
	// the absence of a plugin it will never load. The line is written on every
	// start of every seat because that is what makes it useful to the person
	// staging a bundle, who has no card either.
	if missing := missingFrom(videoCaptureRequiredElements); len(missing) > 0 {
		log.Printf("gst: Init: the bundled GStreamer in %q cannot build a live video capture leg: "+
			"%s. The slate leg is unaffected and this seat will go on air normally; a seat "+
			"configured for a DeckLink video input will be refused at Start (check the %s allowlist)",
			pluginDir, strings.Join(missing, ", "), bundleAllowlistNoun)
	}

	// And the same again for the COMMENTARY coming off the card, reported
	// separately because it is a separate seat with a separate refusal: a bundle
	// can be missing the decklink plugin and be perfectly able to carry a
	// microphone seat, which is every seat shipping today.
	if missing := missingFrom(audioCaptureRequiredElements); len(missing) > 0 {
		log.Printf("gst: Init: the bundled GStreamer in %q cannot build a DeckLink commentary "+
			"capture leg: %s. A seat whose commentary input is a microphone is unaffected and will "+
			"go on air normally; a seat configured for a DeckLink commentary input will be refused "+
			"at Start (check the %s allowlist)",
			pluginDir, strings.Join(missing, ", "), bundleAllowlistNoun)
	}

	log.Printf("gst: initialised, plugins from %q, registry %q", pluginDir, registryPath)
	return nil
}

// registryFile returns the per-user GStreamer registry cache:
// %LOCALAPPDATA%\WSLComms\registry.bin on Windows, and
// ~/Library/Caches/WSLComms/registry.bin on macOS.
//
// The registry is a cache of the plugin scan and must be per-user and writable;
// putting it next to the executable would fail under a per-machine install
// running as a standard user, and sharing the default location would let a
// system-wide GStreamer's registry and ours overwrite each other.
//
// It needs no platform twin, and that is deliberate rather than an oversight.
// LOCALAPPDATA is empty on macOS, so the fallback below is what answers there,
// and os.UserCacheDir returns ~/Library/Caches — which is the RIGHT place for
// this file on that platform, not merely a place that happens to work. macOS
// documents Caches as purgeable, and a purged GStreamer registry costs one plugin
// rescan on the next start and nothing else. A signed .app is not writable, so
// beside the executable is not available even in principle.
//
// Note that internal/applog deliberately does NOT share this reasoning: the log
// file is the only diagnosis a commentary position ever produces and must not
// live somewhere the operating system is entitled to delete. Same fallback, two
// different right answers, which is why the choice is made per caller.
func registryFile() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		// os.UserCacheDir returns %LocalAppData% on Windows, so on that platform
		// this is the same directory by a different route rather than a different
		// policy. On macOS it is ~/Library/Caches, which is the intended answer.
		dir, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("gst: Init: neither LOCALAPPDATA nor a user cache directory is available: %w", err)
		}
		base = dir
	}
	return filepath.Join(base, "WSLComms", "registry.bin"), nil
}

// missingElements returns a description of every required element factory that
// the registry does not have, or nil if the bundle is complete.
func missingElements() []string {
	return missingFrom(requiredElements)
}

// missingFrom is the same lookup against any of the contract lists. It is
// factored out because the video capture leg's list is checked in two places
// and at two severities — see videoCaptureRequiredElements — and a second copy
// of this loop would be a second chance to format the plugin name differently
// in the two messages somebody has to compare.
func missingFrom(reqs []requiredElement) []string {
	var missing []string
	for _, req := range reqs {
		if gogst.ElementFactoryFind(req.factory) == nil {
			missing = append(missing, fmt.Sprintf("%s (plugin %s)", req.factory, req.plugin))
		}
	}
	return missing
}

// enumerateDevices runs a one-shot GstDeviceMonitor probe for one device class
// and returns whatever it found.
//
// It exists because three callers need the same eleven lines: ListInputDevices
// below, and — on darwin — resolveCaptureDeviceIndex, which has to re-enumerate
// at pipeline-open time to turn a persisted CoreAudio unique-id back into the
// integer osxaudiosrc wants. Factoring it out is what keeps those two from
// drifting apart, which would show up as "the device is in the dropdown but
// Start says it has gone away".
//
// The class filter is applied to the monitor AND re-checked per device by the
// callers. GstDeviceMonitor filters by device class and caps only — it has no
// API for restricting which providers it consults — so the provider question is
// always answered afterwards, by captureDeviceID.
func enumerateDevices(class string) []gogst.Device {
	monitor := gogst.NewDeviceMonitor()
	if monitor == nil {
		log.Printf("gst: enumerateDevices(%s): gst_device_monitor_new returned nil", class)
		return nil
	}

	// A nil caps filter means "any caps of this class". A zero return means the
	// filter was not installed, which matters: without it the monitor also
	// reports Audio/Sink devices, and headphones would appear in the commentary
	// input dropdown. Every device is re-checked by the caller in any case.
	if monitor.AddFilter(class, nil) == 0 {
		log.Printf("gst: enumerateDevices: gst_device_monitor_add_filter(%s) returned 0; "+
			"relying on the per-device class check", class)
	}

	// Starting the monitor makes the providers probe and keeps them hot for
	// hot-plug messages. gst_device_monitor_get_devices also works on a stopped
	// monitor by doing a one-shot probe, so a failure to start is not fatal —
	// it just means no hot-plug bus, which nothing here uses anyway.
	started := monitor.Start()
	if !started {
		log.Printf("gst: enumerateDevices(%s): gst_device_monitor_start failed; "+
			"falling back to a one-shot probe", class)
	}
	devices := monitor.GetDevices()
	if started {
		monitor.Stop()
	}
	return devices
}

// ListInputDevices returns the audio capture endpoints offered in the
// commentary input dropdown.
//
// It runs a GstDeviceMonitor filtered to Audio/Source and asks captureDeviceID
// — the per-platform seam in deviceprovider_windows.go and
// deviceprovider_darwin.go — two things about each device: does it belong to
// the provider this build drives, and if so what is the string to persist for
// it. Everything in this function is platform-neutral by construction; there is
// deliberately no runtime.GOOS anywhere in it.
//
// # Why the provider question cannot be asked here
//
// This function used to answer it inline, with device.api == "wasapi2" followed
// by wasapi2's loopback filter and the endpoint-id namespace classifiers. Every
// one of those is Windows knowledge:
//
//   - macOS devices publish NO device.api property at all. The literal symptom
//     that started the port was this function returning an EMPTY list on macOS,
//     so the operator's microphone never appeared in the dropdown.
//   - There is no loopback republication on CoreAudio to filter out, and
//     nothing that could carry the marker.
//   - A CoreAudio unique-id ("BuiltInMicrophoneDevice") is in neither Windows
//     namespace, so the classifier's "unrecognised shape" warning would fire
//     for every device on every enumeration — a warning that always fires is a
//     warning nobody reads.
//
// Replacing that with an 'api == "osxaudio"' branch would have failed in
// exactly the same way, because the property is not published under any name.
// The per-platform question is a PREDICATE over what the provider actually
// exposes, not a string comparison, which is why the seam is a function.
//
// # What is unchanged
//
// The de-duplication, the display-name handling and the summary log line are
// shared: they are about the dropdown, not about the platform. Every skip is
// logged by captureDeviceID with the reason, because a device missing from the
// dropdown is otherwise indistinguishable from a device that is not plugged in.
//
// The summary line keeps one platform-owned TAIL, skipDetail, because how many
// devices were skipped does not say why and the useful "why" is not the same on
// the two platforms — on Windows it is the loopback count, which was in this
// line before the seam existed and is the number that tells an operator the
// filter worked rather than that their microphones disappeared. It is a string
// appended to a log message and nothing else reads it: no branch anywhere,
// on either platform, depends on what it returns.
func ListInputDevices() ([]Device, error) {
	if !inited.Load() {
		return nil, errors.New("gst: ListInputDevices: Init has not been called")
	}

	devices := enumerateDevices("Audio/Source")

	out := make([]Device, 0, len(devices))
	// byID de-duplicates the offered list. On Windows, with endpointID
	// preferring device.actual-id, the "Default Audio Capture Device" pseudo
	// entry resolves to the same endpoint id as the real device it currently
	// points at, and offering both would let the operator persist two names for
	// one endpoint. First entry with a real display name wins.
	//
	// The key is (kind, id) and not the id alone: see deviceDedupKey in
	// decklinkdevices.go for why a collision between the two id spaces is made
	// impossible to express rather than merely unlikely.
	byID := make(map[string]int, len(devices))
	audioSources := 0 // everything with the Audio/Source class, any provider
	skipped := 0      // Audio/Source devices captureDeviceID refused to offer
	// The platform's own tally of WHY, reset so that the summary below describes
	// this enumeration and not every one since the process started. It is
	// diagnostic only and gates nothing; the platform seam owns both halves
	// because what is worth counting differs entirely between them.
	resetSkipDetail()
	for _, dev := range devices {
		if dev == nil {
			continue
		}
		// Defence in depth against the monitor filter not having been
		// installed: a capture endpoint and a playback endpoint differ only by
		// this class string, and offering a playback endpoint as a commentary
		// input would fail at Start rather than in the dropdown.
		if !dev.HasClasses("Audio/Source") {
			continue
		}
		audioSources++
		props := dev.GetProperties()
		if props == nil {
			// gst_device_get_properties is nullable. Without properties the
			// device cannot be identified at all — neither provider nor id —
			// and Device.ID has to be something the capture source will accept,
			// so this device cannot safely be offered.
			log.Printf("gst: ListInputDevices: skipping %q: it publishes no device properties",
				dev.GetDisplayName())
			skipped++
			continue
		}
		id, kind, offer := captureDeviceID(dev, props)
		if !offer {
			// captureDeviceID has already logged the reason, in the platform's
			// own vocabulary. Adding a second line here would double every skip.
			skipped++
			continue
		}
		name := dev.GetDisplayName()
		key := deviceDedupKey(kind, id)
		if i, dup := byID[key]; dup {
			// The kept name is captured BEFORE the empty-name upgrade below,
			// or the log would quote the duplicate as having duplicated
			// itself.
			kept := out[i].Name
			if out[i].Name == "" && name != "" {
				out[i].Name = name
			}
			log.Printf("gst: ListInputDevices: %q duplicates the device id of %q (%s); keeping the first",
				name, kept, id)
			continue
		}
		byID[key] = len(out)
		// NormaliseDeviceKind, not kind: Device.Kind's json tag has no omitempty
		// precisely so the frontend's one dropdown can group on this field, and
		// that only works if the field always holds one of the two spellings.
		// captureDeviceID is not supposed to answer "" and every platform seam
		// today does not, so this is a belt-and-braces conversion at the single
		// point every offered device passes through rather than a defence
		// against a known caller.
		out = append(out, Device{ID: id, Name: name, Kind: NormaliseDeviceKind(kind)})
	}
	log.Printf("gst: ListInputDevices: offered %d of %d Audio/Source devices (%s); %d were skipped%s",
		len(out), audioSources, captureSourceFactory, skipped, skipDetail())
	reportSilentDeckLinkProvider()
	return out, nil
}

// reportSilentDeckLinkProvider writes ONE line when the decklink plugin is
// loaded and nevertheless enumerated no cards, and is silent in every other
// case.
//
// # The failure it exists to name, measured
//
// A fitted, working UltraStudio 4K Mini did not appear in the commentary input
// list of the SHIPPED, SIGNED, NOTARISED bundle, while appearing perfectly in an
// unsigned build and in gst-device-monitor-1.0 on the same machine one minute
// later. The whole of the evidence GStreamer offered was this function's
// caller's own summary line:
//
//	gst: ListInputDevices: offered 4 of 4 Audio/Source devices (osxaudiosrc); 0 were skipped
//
// — four devices where there are five, no skip, no warning, nothing in the
// GStreamer debug log at any level. The cause was in the KERNEL log:
//
//	AppleMobileFileIntegrity: Library Validation failed: Rejecting
//	'/Library/Frameworks/DeckLinkAPI.framework/Versions/A/DeckLinkAPI'
//	(Team ID: 9ZGFBWLSYP) for process 'wslcomms' (Team ID: 5P76UVY5WF),
//	reason: mapping process and mapped file (non-platform) have different Team IDs
//
// libgstdecklink does not LINK the Blackmagic API — it dlopens
// /Library/Frameworks/DeckLinkAPI.framework at first use — so under the hardened
// runtime without a library-validation waiver the plugin registers, the device
// provider registers, the provider probes, the dlopen is refused, and the
// provider reports zero cards. Every layer above sees a machine with no capture
// card in it. The frontend then greys the DeckLink option out and says "no
// DeckLink card was found in this machine" about a card whose tally light is on.
// See build/darwin/wslcomms.entitlements for the fix and the measurement.
//
// # It stays SILENT unless there is something to act on, and the bar is high
//
// deckLinkSkipDetail's rule is that a machine with no Blackmagic anything must
// log exactly what it logged before DeckLink support existed, down to the
// character, because a permanent line about hardware that was never fitted is a
// permanent invitation to go looking for it. That rule is kept here, and keeping
// it took more than the obvious test.
//
// THE OBVIOUS TEST IS WRONG ON macOS, and it is worth writing down because it
// looks right. "Is the decklink element factory registered" reads as "is the
// driver installed", and on macOS it is not: libgstdecklink.dylib has NO
// link-time dependency on DeckLinkAPI — measured, its whole reference to
// Blackmagic is one string, loaded through CFBundle at first use — so the plugin
// registers, and the elements resolve, on a Mac that has never had Desktop Video
// on it. Gating the line on the factory alone would print it on every
// enumeration on every Mac we ship to.
//
// So the factory check is only the CHEAP half, and the decision belongs to
// deckLinkAPIDiagnosis, which measures. It returns false for both of the
// unremarkable cases — no driver installed, and an API that loads perfectly
// well on a machine that simply has no card plugged in — and true only when the
// API is installed and this process could not load it, which is the one state
// that is invisible from everything else the application prints.
//
// It never refuses anything. A missing card is not an error here; it is an error
// at Start, where preflightCapture can name the field.
func reportSilentDeckLinkProvider() {
	if deckLinkDevicesOffered.Load() != 0 {
		return
	}
	// The FACTORY first, because a factory lookup is one registry hash and the
	// diagnosis below touches the filesystem. Both the elements and the device
	// provider register in the same plugin_init, so no factory means the plugin
	// is not in this bundle at all and there is nothing further to ask.
	if gogst.ElementFactoryFind(videoCaptureFactory) == nil {
		return
	}
	reason, worth := deckLinkAPIDiagnosis()
	if !worth {
		return
	}
	log.Printf("gst: ListInputDevices: the decklink plugin is loaded but enumerated NO cards. %s",
		reason)
}

// structureFieldNames renders a structure's field names for a diagnostic
// message. It deliberately does not render the values: a device property
// structure is not secret, but making "dump everything" the habit in this
// package is how a passphrase eventually ends up in a log line.
func structureFieldNames(s *gogst.Structure) string {
	n := s.NFields()
	names := make([]string, 0, n)
	for i := int32(0); i < n; i++ {
		names = append(names, s.NthFieldName(uint(i)))
	}
	return "[" + strings.Join(names, " ") + "]"
}

// GStreamer element factory list bit flags, from gstelementfactory.h.
//
// go-gst v0.0.2 declares GstElementFactoryListType as a bare uint64 alias and
// does not generate the GST_ELEMENT_FACTORY_TYPE_* macros, because they are C
// preprocessor macros rather than enum members and so do not appear in the GIR.
// They are reproduced here verbatim. If selectH264Encoder ever returns nothing
// on a machine that clearly has an H.264 encoder, these two constants are the
// first thing to check.
const (
	factoryTypeEncoder    uint64 = 1 << 1  // GST_ELEMENT_FACTORY_TYPE_ENCODER
	factoryTypeMediaVideo uint64 = 1 << 49 // GST_ELEMENT_FACTORY_TYPE_MEDIA_VIDEO
)

// h264EncoderDenylist is a set of H.264 encoder factories that must never be
// selected regardless of rank.
//
// x264enc is GPL. The deliverable is commercial, which is why the plugin
// allowlist in specification section 3 is an explicit file list rather than a
// directory copy. It cannot reach the bundle, but a machine with a system-wide
// GStreamer and a mis-set GST_PLUGIN_PATH_1_0 could still surface it, and
// x264enc outranks nothing here worth the licence risk.
//
// The list is deliberately NOT per-platform. x264enc is GPL on macOS too, it is
// present in a stock Homebrew GStreamer, and it is ranked primary (256) there —
// so on the developer's own machine it is a live candidate that the factory
// query would otherwise return. Splitting this per platform would have meant
// two places to forget.
var h264EncoderDenylist = map[string]bool{
	"x264enc": true,
}

// encoderProp is one encoder setting, as a string for gst_util_set_object_arg.
//
// Strings rather than typed values because gst_util_set_object_arg deserialises
// into whatever GType the property is — enum nick, gint or guint — and encoders
// disagree about which of those they use even for identically named properties.
// The lists live in elements_windows.go and elements_darwin.go, because the two
// platforms' encoders spell the same four intentions differently.
type encoderProp struct{ name, value string }

// selectH264Encoder returns the factory name of the best H.264 encoder present
// in the registry: the first entry of h264EncoderPreference that is installed
// and usable, with rank as a tie-break only.
//
// Specification open question 3: "Is the highest-ranked H.264 encoder called
// mfh264enc on the target machine? Resolve the element by rank at runtime
// rather than hardcoding the name." This does that, and logs what it chose so
// that the answer is in the field log rather than in someone's memory.
//
// The function itself is platform-neutral. The three lists it consults —
// h264EncoderPreference, h264EncoderFallbacks and h264EncoderProps — are
// declared per platform in elements_windows.go and elements_darwin.go, along
// with the measurements that put them in that order.
func selectH264Encoder() (string, error) {
	caps := gogst.CapsFromString("video/x-h264")
	if caps == nil {
		return "", errors.New("gst: selectH264Encoder: gst_caps_from_string(\"video/x-h264\") returned nil")
	}

	// RankMarginal rather than RankNone: a rank-zero encoder is one GStreamer
	// itself considers unfit for automatic selection.
	factories := gogst.ElementFactoryListGetElements(
		gogst.ElementFactoryListType(factoryTypeEncoder|factoryTypeMediaVideo),
		gogst.RankMarginal,
	)

	bestName := ""
	bestRank := uint(0)
	bestPref := len(h264EncoderPreference)

	for _, f := range factories {
		if f == nil {
			continue
		}
		name := f.GetName()
		if h264EncoderDenylist[name] {
			continue
		}
		if !f.CanSrcAnyCaps(caps) {
			continue
		}
		rank := f.GetRank()
		pref := preferenceIndex(name)

		// Preference first, rank only as a tie-break between two factories we
		// are equally happy with. See h264EncoderPreference for why this is
		// deliberately not "highest rank wins": on measurement, rank selects
		// whichever GPU vendor's encoder is installed, and the specification's
		// property set only applies to mfh264enc.
		//
		// Anything not in the preference list scores len(list), so a known
		// encoder always beats an unknown one, and among unknowns the highest
		// rank still wins — which is the sensible answer when we have no
		// opinion.
		if bestName == "" || pref < bestPref || (pref == bestPref && rank > bestRank) {
			bestName, bestRank, bestPref = name, rank, pref
		}
	}

	if bestName != "" {
		if bestPref < len(h264EncoderPreference) {
			log.Printf("gst: H.264 encoder %s chosen by preference (rank %d), from %d candidates",
				bestName, bestRank, len(factories))
		} else {
			log.Printf("gst: H.264 encoder %s chosen by rank %d - NOT in the preference list, so the "+
				"specification's encoder settings may not all apply; check the property warnings below",
				bestName, bestRank)
		}
		return bestName, nil
	}

	for _, name := range h264EncoderFallbacks {
		if h264EncoderDenylist[name] {
			continue
		}
		if gogst.ElementFactoryFind(name) != nil {
			log.Printf("gst: H.264 encoder resolved by name fallback: %s "+
				"(the factory-list query matched nothing; check factoryTypeMediaVideo)", name)
			return name, nil
		}
	}

	return "", errors.New("gst: no H.264 encoder is available: neither the ranked factory list nor " +
		"the name fallbacks matched (is the mediafoundation plugin in the bundle?)")
}

// preferenceIndex returns the tie-break position of a factory name, or a value
// past the end of the preference list if it is not in it.
func preferenceIndex(name string) int {
	for i, n := range h264EncoderPreference {
		if n == name {
			return i
		}
	}
	return len(h264EncoderPreference)
}

// New creates a Pipeline. Init must have been called first.
//
// It starts one goroutine, which does nothing but write bus warnings to the
// log; Stop ends it. A Pipeline that is created and never stopped therefore
// leaks that goroutine, which is why the contract in gst.go says Stop is
// idempotent and must always be called.
func New() (Pipeline, error) {
	if !inited.Load() {
		return nil, errors.New("gst: New: Init has not been called")
	}
	p := &cgoPipeline{
		errs:  make(chan error, errorChannelBuffer),
		warns: make(chan string, warningChannelBuffer),
	}
	go p.logWarnings()
	return p, nil
}

// logWarnings writes bus warnings to the log, off the streaming thread that
// produced them. It ends when Stop closes the channel.
//
// This exists so that onBusMessage never calls log.Printf. Go's log package
// takes a process-global mutex and writes synchronously to stderr; a marginal
// SRT link produces warnings in bursts, and serialising several GStreamer
// streaming threads behind one file handle during an outage is exactly the
// wrong moment to add latency to the capture chain.
func (p *cgoPipeline) logWarnings() {
	for w := range p.warns {
		log.Print(w)
	}
}

// sinkErrRoute diverts a GST_ELEMENT_ERROR posted by one specific srtsink away
// from the asynchronous Errors channel and into the ReplaceSink call that
// provoked it.
//
// The contract in gst.go is that a synchronous failure comes back from the
// method that caused it and never appears on Errors(). srtsink reports a failed
// caller handshake by posting an error on the bus AND returning
// GST_STATE_CHANGE_FAILURE, so without this the same failure would be delivered
// twice, once as a returned error and once asynchronously, and internal/sender
// would count one connection attempt as two failures.
type sinkErrRoute struct {
	// name is the unique element name of the sink being installed. Only
	// messages whose source has this exact name are diverted.
	name string
	// ch has capacity one. onBusMessage sends without blocking; a second error
	// from the same sink during the same swap is discarded because the first is
	// the one that explains the failure.
	ch chan error
}

// cgoPipeline is the go-gst backed Pipeline.
type cgoPipeline struct {
	// mu serialises Start, ReplaceSink, ForceKeyUnit and Stop against each
	// other. It is held across GStreamer state changes and is therefore held
	// for seconds at a time; nothing on a streaming thread may wait for it.
	mu sync.Mutex

	started bool
	stopped bool

	// fatal, once set, is a pipeline-level failure that replacing the sink
	// cannot repair — see the file comment. ReplaceSink returns it rather than
	// reporting a connection that would carry no media.
	//
	// It is guarded by errMu, not by mu, because it is written by onBusMessage
	// on a streaming thread and mu is held for the whole of ReplaceSink, which
	// is exactly when that message arrives.
	fatal error

	// pipeline is the GstPipeline built by gst_parse_launch. Held so that it
	// stays reachable: dropping the Go reference would let the finalizer unref
	// the last reference to a running pipeline.
	pipeline gogst.Pipeline

	// clock is the pinned system clock. Held for the same reason.
	clock gogst.Clock

	// bus is the pipeline's bus, held so that Stop can detach the sync handler.
	bus gogst.Bus

	// aconv is the audioconvert element carrying the channel map, and
	// aconvSinkPad is the pad whose NEGOTIATED caps size the matrix written to
	// it. The pad is held rather than looked up per call because
	// InputChannels() is a UI-facing read that may arrive many times a second
	// while the mapping panel is open, and a GetByName plus a GetStaticPad per
	// call would be two cgo round trips to answer a question the pad itself
	// answers in one.
	//
	// Both are nil until Start has parsed the pipeline, and are cleared by
	// teardownLocked. Everything that touches them holds mu — see
	// InputChannels for why that is the right lock even though it is the one
	// held across state changes.
	aconv        gogst.Element
	aconvSinkPad gogst.Pad

	// matrixWidth is the input channel count the mix-matrix currently written to
	// aconv was built for, and is ZERO WHEN NO MATRIX HAS BEEN WRITTEN — the
	// ordinary state on a positioned capture source, where audioconvert's own
	// downmix is doing the work and there is nothing for SetChannelMap to
	// change without renegotiating the caps the feed is running on. Guarded by
	// mu.
	//
	// The MAP itself is deliberately not kept beside it. There is nothing to
	// compare it against — mix-matrix is a GST_TYPE_ARRAY and does not marshal
	// back, so the element cannot be asked what it is running — and a field
	// nothing reads is a second, unverifiable account of the same fact. The
	// record of what was written is the log line each write emits, which is
	// where a field engineer looks anyway.
	matrixWidth int

	// sigWatch is the video signal watchdog, or nil when this pipeline has no
	// element with a "signal" property to poll — every native capture, and the
	// slate-only video leg shipping today. The nil is usable: Stop on it does
	// nothing, so there is one unconditional call at teardown rather than a guard
	// somebody can widen without noticing what it was for.
	sigWatch *signalWatch

	// encoder is the H.264 encoder element, kept for ForceKeyUnit.
	encoder gogst.Element

	// encoderName is the factory name chosen by selectH264Encoder.
	encoderName string

	// srtq is the leaky queue in front of the sink, and its two pads.
	srtq        gogst.Element
	srtqSrcPad  gogst.Pad
	srtqSinkPad gogst.Pad

	// gate probe ids, for removal at Stop. sinkEventProbeID is the event half
	// of the sink-pad gate; see eventGateProbe.
	srcProbeID       uint32
	sinkProbeID      uint32
	sinkEventProbeID uint32

	// eventsDropped counts the downstream events eventGateProbe has kept out of
	// gst_queue_handle_sink_event. It is diagnostics only — a non-zero value on
	// a reconnect is the fix in BUILD-NOTES.md section 8.6 doing its job — and
	// it is atomic because the writer is a GStreamer streaming thread.
	eventsDropped atomic.Int64

	// sink is the srtsink currently installed, or nil when there is none.
	sink gogst.Element
	// sinkSerial numbers the sinks so each gets a unique element name.
	sinkSerial int

	// gateClosed is read by the pad probes on streaming threads and written by
	// ReplaceSink, Stop and onBusMessage. It is the whole of the gate's state;
	// see the file comment for why a probe drops rather than blocks.
	gateClosed atomic.Bool

	// route diverts one sink's bus error into the ReplaceSink call in progress.
	// Written by ReplaceSink under mu, read by onBusMessage without any lock.
	route atomic.Pointer[sinkErrRoute]

	// onLevels is PipelineOpts.OnLevels, or nil when the caller wants no
	// metering. It is an atomic pointer for the same reason route is: the
	// reader is onBusMessage on a GStreamer streaming thread, which must not
	// take p.mu, and the writer is Start — which holds p.mu across the state
	// change that first makes level messages possible, so a plain field would
	// be a write racing the very messages it enables. It is written once,
	// before the pipeline is built, and never cleared: busSilenced already
	// stops delivery at teardown.
	onLevels atomic.Pointer[func(Levels)]

	// onChannelLevels is PipelineOpts.OnChannelLevels — the per-channel picker
	// meter's frames, from chlevel rather than alevel. Same type, same atomic,
	// same reason, and deliberately a SEPARATE field rather than a wider
	// callback: the two elements measure different points, and the whole of the
	// routing in onBusMessage exists so that a frame from one can never be
	// delivered as a frame from the other.
	onChannelLevels atomic.Pointer[func(Levels)]

	// busSilenced makes onBusMessage return immediately once the pipeline is
	// being torn down. It exists because the bus sync handler is NEVER
	// detached; see teardownLocked.
	busSilenced atomic.Bool

	// errs carries GST_ELEMENT_ERROR bus messages to Errors, and warns carries
	// GST_ELEMENT_WARNING to the logging goroutine. Both are guarded by errMu,
	// which exists only so that a streaming thread cannot send on a channel
	// Stop is closing. errMu is never held across anything that can block.
	errMu      sync.RWMutex
	errs       chan error
	warns      chan string
	errsClosed bool
}

// Compile-time assertion that the real implementation satisfies the contract.
var _ Pipeline = (*cgoPipeline)(nil)

// Start builds the pipeline and takes it to PLAYING with no sink installed.
func (p *cgoPipeline) Start(opts PipelineOpts) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return errors.New("gst: pipeline is stopped")
	}
	if p.started {
		return errors.New("gst: pipeline already started")
	}
	if opts.SlatePath == "" {
		return errors.New("gst: PipelineOpts.SlatePath is required")
	}
	// WHICH ELEMENT OPENS THE COMMENTARY, decided and refused here,
	// synchronously, before anything is built.
	//
	// It replaces the bare "AudioDeviceID is required" this check used to be,
	// and the replacement is not a relaxation: exactly one of the two source
	// fields must be given, so the state that check existed to prevent — a
	// platform capture element with an empty device, which is the SYSTEM DEFAULT
	// INPUT and not an error — is still unreachable, and the DeckLink seat that
	// used to be unexpressible now is.
	//
	// refuseWrongAudioSource also carries the RENDER (playback) endpoint refusal
	// unchanged, and it stays synchronous and up front for the reason it always
	// was: wasapi2src accepts such an id at construction and fails
	// ASYNCHRONOUSLY — error 1551 on the bus, after Start has returned success —
	// and the sender then misreads a local device fault as a network failure and
	// retries the SRT link forever. The rule and its deliberate asymmetry (only
	// a POSITIVE render identification refuses; unknown shapes pass) live in
	// device_id.go, shared with the stub twin so the two cannot drift. NEVER
	// "fix" it by setting wasapi2src's loopback property instead: that opens the
	// operator's own monitor mix as the commentary source.
	if err := refuseWrongAudioSource(opts.AudioDeviceID, opts.AudioCaptureID); err != nil {
		return err
	}
	if opts.VideoBitrateKbps == 0 {
		opts.VideoBitrateKbps = DefaultVideoBitrateKbps
	}
	if opts.AudioBitrateBps == 0 {
		opts.AudioBitrateBps = DefaultAudioBitrateBps
	}
	if opts.VideoBitrateKbps < 0 || opts.AudioBitrateBps < 0 {
		return fmt.Errorf("gst: negative bitrate: video %d kbps, audio %d bps",
			opts.VideoBitrateKbps, opts.AudioBitrateBps)
	}

	// THE VIDEO LEG'S SOURCE, decided once, here, and refused here if it cannot
	// be honoured. Both checks are made BEFORE anything is built, because both
	// failures are configuration failures and the message a configuration
	// failure deserves is one that names the value, not a parse error naming an
	// element.
	//
	// The id is validated by CONVERTING it, which is the same test
	// configureDeckLinkSource will make later and is deliberately the same call.
	// A CoreAudio unique-id or a WASAPI endpoint GUID reaching the element's
	// persistent-id property would leave it at its own -1 default, which means
	// "use device-number", which means whichever card the driver enumerated
	// first — a wrong-card video feed with every lamp green. Refusing early
	// costs nothing and makes the wrong-kind case a sentence rather than a
	// picture nobody recognises.
	if opts.VideoCaptureID != "" {
		if _, err := parseDeckLinkPersistentID(opts.VideoCaptureID); err != nil {
			return fmt.Errorf("gst: PipelineOpts.VideoCaptureID names the DeckLink card whose "+
				"input becomes the video leg, and it must be a DeckLink persistent-id rather "+
				"than an audio device id: %w", err)
		}
		// The bundle contract for this leg alone. Init logged it and did not
		// refuse, precisely so a seat with no card could still go on air; this
		// is the seat that does have one, so this is where it becomes an error.
		// See videoCaptureRequiredElements.
		if missing := missingFrom(videoCaptureRequiredElements); len(missing) > 0 {
			return fmt.Errorf("gst: a DeckLink video input is configured but this build's "+
				"GStreamer cannot build the capture leg: %s. The commentary audio is unaffected; "+
				"clearing the video input setting will start this seat with the slate",
				strings.Join(missing, ", "))
		}
		log.Printf("gst: Start: the video leg is a LIVE CAPTURE from DeckLink card %s; "+
			"the slate is not built", opts.VideoCaptureID)
	}

	// THE COMMENTARY LEG'S SOURCE, refused here on the same terms and before
	// anything is built. The id itself has already been converted by
	// refuseWrongAudioSource, so what is left is the two questions that need
	// more than the string.
	if opts.AudioCaptureID != "" {
		// The bundle contract for this seat. Init logged it and did not refuse,
		// so that a seat with no card could still go on air; this is the seat
		// that does have one. Unlike the video leg there is nothing to fall back
		// to — the commentary IS the product — so the message says to change the
		// input rather than to clear a setting.
		if missing := missingFrom(audioCaptureRequiredElements); len(missing) > 0 {
			return fmt.Errorf("gst: the commentary input is a DeckLink card but this build's "+
				"GStreamer cannot build the capture leg: %s. Choose a microphone in the Commentary "+
				"input dropdown to start this seat", strings.Join(missing, ", "))
		}
		// ONE CARD, and this is the refusal that keeps the clock companion
		// honest. When the video leg is also a card, THAT decklinkvideosrc is
		// what clocks this audio — the card is exclusive and a second source
		// fails — so the two ids naming different cards describes a pipeline
		// whose audio would be clocked by the WRONG CARD's video. That does not
		// fail loudly: the audio source would wait for a clock that never starts
		// for it, and the seat would die in negotiation naming neither card.
		//
		// config.json carries ONE decklinkPersistentId for both legs, so the
		// application cannot produce this; a caller that hand-built it is
		// describing a two-card rig that would need a THIRD decklink element,
		// which nothing here has ever run. Refusing by name beats building a
		// shape nobody has measured.
		if opts.VideoCaptureID != "" && opts.VideoCaptureID != opts.AudioCaptureID {
			return fmt.Errorf("gst: PipelineOpts.VideoCaptureID is card %s and "+
				"PipelineOpts.AudioCaptureID is card %s. A DeckLink drives audio capture off the "+
				"VIDEO clock, so a commentary leg on one card cannot be clocked by another card's "+
				"video, and the card is exclusive so a third element is not available to clock it. "+
				"Both legs must name the same card, or the video leg must be the slate",
				opts.VideoCaptureID, opts.AudioCaptureID)
		}
		if opts.VideoCaptureID != "" {
			log.Printf("gst: Start: the commentary is captured from DeckLink card %s, clocked by "+
				"the SAME card's video leg; no clock companion is built", opts.AudioCaptureID)
		} else {
			log.Printf("gst: Start: the commentary is captured from DeckLink card %s while the "+
				"video leg is the slate, so a %s named %s is built to clock it — measured at "+
				"0.6-2.4%% of one core, and it feeds nothing but a fakesink",
				opts.AudioCaptureID, videoCaptureFactory, nameVideoCaptureClock)
		}
	}

	// The conform target is resolved HERE and nowhere else, so that the string
	// handed to gst_parse_launch and the string written to the log are the same
	// decision. resolve never fails — a format that cannot be used falls back
	// to 1920x1080p50 and says why, because no property of a still slate is
	// worth refusing to carry commentary for. See FallbackConformTarget.
	conform, fallbackReason := opts.ConformTo.resolve()
	if fallbackReason != "" {
		// A fault, and one worth a whole line: somebody supplied a target and
		// it was not usable, so the leg is about to be built to a raster nobody
		// chose. M2L-X only accepts a source in its own configured format.
		log.Printf("gst: Start: conform target %v is unusable (%s); falling back to %v. "+
			"If this instance is not configured for %v the video leg will be refused",
			opts.ConformTo, fallbackReason, conform, conform)
	} else {
		log.Printf("gst: Start: conforming the video leg to %v", conform)
	}

	// The levels callback is published BEFORE anything GStreamer is built. The
	// level element starts posting the moment the pipeline reaches PLAYING,
	// which happens further down while p.mu is still held — so onBusMessage,
	// on a streaming thread that must not take p.mu, may read this field while
	// Start is still executing. Storing it first, through an atomic, is what
	// makes that read safe; see the field comment.
	if opts.OnLevels != nil {
		cb := opts.OnLevels
		p.onLevels.Store(&cb)
	}
	// The per-channel picker's callback, published here for exactly the same
	// reason and with one extra consequence. chlevel is the meter the operator
	// stares at while deciding whether the mapping screen is working at all, so
	// frames dropped because the callback was stored a moment too late do not
	// read as a slow start — they read as a dead meter, and the operator goes
	// looking for the fault somewhere else.
	if opts.OnChannelLevels != nil {
		cb := opts.OnChannelLevels
		p.onChannelLevels.Store(&cb)
	}

	encoderName, err := selectH264Encoder()
	if err != nil {
		return err
	}
	p.encoderName = encoderName

	// THE CONFIDENCE MONITOR, decided here and nowhere else, before anything is
	// built. preview.go carries the whole argument. Two things about this call
	// site are load-bearing: the preview is a STRING appended to the parse
	// description and never an element added to a running graph — set_state(NULL)
	// on such a branch inside a blocking pad probe took the on-air leg from 50 fps
	// to 0 PERMANENTLY with the pipeline still reporting PLAYING — and
	// opts.VideoCaptureID is passed so that a preview can never be rendered
	// against a tee the slate leg does not build, which would be a
	// gst_parse_launch failure of this whole pipeline rather than a missing
	// preview.
	//
	// It is resolved ONCE, here, rather than inside the build below, because the
	// build below may run twice: resolving it there would ask the registry for a
	// sink a second time and log the branch a second time for a pipeline that is
	// deliberately not getting one.
	preview := previewBranchFor(opts.Preview, opts.VideoCaptureID)

	if err := p.startBuiltLocked(opts, conform, encoderName, preview); err != nil {
		if preview == "" {
			return err
		}
		// THE CONFIDENCE MONITOR IS OPTIONAL AND THE COMMENTARY IS NOT.
		//
		// Every OTHER way the preview can fail is spared: a bus error from one of
		// its elements is classPreview and never reaches the gate or Errors(). A
		// sink that will not come up at all is the one that is not, because it
		// fails as a STATE CHANGE and the bus filter is not consulted. Building
		// once more without the branch can only turn a failure into a success,
		// and the alternative is a seat that will not go on air because of a
		// window.
		//
		// abort() has already taken the pipeline to NULL and dropped every
		// element reference, so the second attempt starts where the first did.
		// p.fatal is the one thing that survives, and that is correct: nothing a
		// preview element posts can latch it (classPreview never calls markFatal),
		// so a latched fatal means the first attempt failed for a reason the
		// second will fail for too — and it will say so, in the same words.
		//
		// WHAT IT COSTS when the fault is NOT the preview's: one more NULL to
		// PLAYING, so a seat that cannot start at all takes up to
		// pipelineStartTimeout longer to say so. That is paid only by a seat that
		// has both chosen a camera and ticked the monitor, and only when it is
		// already failing.
		log.Printf("gst: Start: the pipeline would not start with the confidence monitor in it "+
			"(%v); rebuilding WITHOUT it. The commentary is the product and the preview is not", err)
		return p.startBuiltLocked(opts, conform, encoderName, "")
	}
	return nil
}

// startBuiltLocked builds the pipeline, points every element at what it is to
// open, and takes it to PLAYING. p.mu must be held; Start is its only caller.
//
// # Why it is a function of its own, and why it may be called twice
//
// It is the whole of Start below the option checks, extracted for exactly one
// reason: a preview sink that EXISTS and then will not START — no GL context, no
// D3D11 device, a display that has gone away — fails inside the NULL to PLAYING
// transition. That is a state change and not a bus error, so capturefault.go's
// classifier never sees it and none of the sparing that protects the preview
// everywhere else applies. The only answer available at that point is to build
// the pipeline again without it, which needs the build to be callable twice.
//
// It is safe to call twice because every path out of it that is not success goes
// through abort(), which runs teardownLocked: the pipeline reaches NULL, the
// capture device and the card are released, every element reference is dropped
// and matrixWidth goes back to zero. The second call therefore starts from the
// same state the first one did, with two deliberate exceptions — p.fatal, which
// is latched and must survive so that a real fault fails the retry too, and the
// process-lifetime base time, which is sampled once and reused forever.
//
// THE ORDER OF WHAT IT DOES IS LOAD-BEARING THROUGHOUT and is guarded from Gate
// A by source-reading tests in gst_stub_test.go, which read this function and
// Start together as one sequence — see startSequence there. Nothing in here may
// be reordered on the strength of it reading better.
func (p *cgoPipeline) startBuiltLocked(opts PipelineOpts, conform ConformTarget,
	encoderName, preview string) error {
	desc := pipelineDescription(encoderName, opts.AudioBitrateBps, conform,
		opts.VideoCaptureID, opts.AudioCaptureID, preview)
	log.Printf("gst: gst_parse_launch:\n%s", desc)

	element, err := gogst.ParseLaunch(desc)
	if err != nil {
		return fmt.Errorf("gst: gst_parse_launch failed: %w", err)
	}
	if element == nil {
		return errors.New("gst: gst_parse_launch returned nil with no error")
	}
	pipeline, ok := element.(gogst.Pipeline)
	if !ok {
		return fmt.Errorf("gst: gst_parse_launch returned a %T, not a GstPipeline", element)
	}
	p.pipeline = pipeline

	// From here on a failure must tear down what has been built, or the WASAPI
	// endpoint stays open and the next Start cannot have it.
	abort := func(err error) error {
		p.teardownLocked()
		return err
	}

	// THE VIDEO LEG'S SOURCE. Exactly one of the two branches below is built,
	// decided by the same condition pipelineDescription used, so the element
	// looked up is always the element the string put there.
	//
	// The slate path and the device ids are set with g_object_set rather than
	// placed in the parse string. gst_parse_launch's quoting rules treat a
	// backslash as an escape inside double quotes, so a Windows path would be
	// mangled, and the endpoint GUID's braces and dots are similarly at the
	// mercy of the parser. Neither value ever reaches the parser.
	if opts.VideoCaptureID == "" {
		slate := pipeline.GetByName(nameSlateSrc)
		if slate == nil {
			return abort(errors.New("gst: parsed pipeline has no element named " + nameSlateSrc))
		}
		if err := setStringProperty(slate, "location", opts.SlatePath); err != nil {
			return abort(err)
		}

		// THE CLOCK COMPANION, on the one configuration that has one: the
		// picture is a still and the commentary comes off the card. It is
		// pointed at the card by the SAME configureDeckLinkSource the other two
		// decklink elements go through, because it must open the SAME CARD as
		// the audio source — a companion on a different card would clock
		// nothing and the audio would never preroll.
		//
		// It sits inside this branch rather than beside it because the condition
		// is exactly the condition pipelineDescription used, so the element
		// looked up is always the element the string put there.
		if opts.AudioCaptureID != "" {
			clock := pipeline.GetByName(nameVideoCaptureClock)
			if clock == nil {
				return abort(errors.New("gst: parsed pipeline has no element named " +
					nameVideoCaptureClock + ", so there is nothing to clock the DeckLink " +
					"commentary capture and it would never produce a buffer"))
			}
			if err := configureDeckLinkSource(clock, opts.AudioCaptureID); err != nil {
				return abort(err)
			}
		}
	} else {
		vsrc := pipeline.GetByName(nameVideoCaptureSrc)
		if vsrc == nil {
			return abort(errors.New("gst: parsed pipeline has no element named " + nameVideoCaptureSrc))
		}
		// The SAME function the audio source goes through, on purpose. One saved
		// persistent-id serves both entries the card publishes, and routing both
		// kinds of decklink element through one setter is what stops the two
		// growing different rules about the identical string. Note what is NOT
		// called anywhere near here: nothing sets `connection`. See
		// pipelineDescription.
		if err := configureDeckLinkSource(vsrc, opts.VideoCaptureID); err != nil {
			return abort(err)
		}
	}

	// add-borders is set here rather than in the parse string because
	// gst_parse_launch treats an unknown property as a hard error, and a
	// commentary position that will not start because a scaler property was
	// renamed is a worse outcome than a picture scaled at the element's default.
	// The value pins the behaviour for a source whose aspect ratio is not the
	// conform target's: letterbox it rather than stretch it. videoscale's own
	// default is already true, so this is a guard against the default changing,
	// not a change.
	//
	// It applies to WHICHEVER leg was built, and the name is chosen from the
	// same condition rather than by trying both: a lookup that misses is logged
	// as a defect below, and a message saying the slate's scaler is missing on a
	// pipeline that was never asked to build one is a false alarm in the one
	// file somebody reads when a feed looks wrong. On the capture leg it matters
	// for a reason the slate never had — a 16:9 camera into a 4:3 switcher
	// configuration is a real facility, and stretching faces is the kind of
	// fault nobody reports as a fault.
	scaleName := nameVideoScale
	if opts.VideoCaptureID != "" {
		scaleName = nameVideoCapScale
	}
	if vscale := pipeline.GetByName(scaleName); vscale != nil {
		if hasProperty(vscale, "add-borders") {
			vscale.SetObjectProperty("add-borders", true)
		} else {
			log.Printf("gst: %s has no add-borders property; a picture that is not the conform "+
				"target's aspect ratio will be stretched", scaleName)
		}
	} else {
		log.Printf("gst: parsed pipeline has no element named %s; a picture that is not exactly "+
			"%dx%d will fail caps negotiation", scaleName, conform.Width, conform.Height)
	}

	asrc := pipeline.GetByName(nameAudioSrc)
	if asrc == nil {
		return abort(errors.New("gst: parsed pipeline has no element named " + nameAudioSrc))
	}
	// The id is logged VERBATIM immediately before it is handed to the capture
	// source, and this line stays wherever the port goes.
	//
	// On Windows wasapi2src echoes the requested id verbatim in its
	// asynchronous error 1551 (proved by probe — no substitution, no default
	// fallback), so a "Failed to open device {...}" later is matched to what
	// was actually requested rather than argued about. On macOS the id is a
	// CoreAudio unique-id that has to be resolved to an integer before
	// osxaudiosrc will accept it, and this line is what says which id the
	// resolution was asked to find. In both cases it is the only record of the
	// value that came out of config.json.
	if opts.AudioCaptureID != "" {
		// THE CARD. The same setter the video source and the clock companion go
		// through, on purpose: the card publishes ONE persistent-id for its audio
		// and video entries alike, and routing every decklink element through one
		// function is what stops three of them growing different rules about the
		// identical saved string. Note what is NOT called anywhere near here:
		// nothing sets `connection`. See pipelineDescription.
		if err := configureDeckLinkSource(asrc, opts.AudioCaptureID); err != nil {
			return abort(err)
		}
	} else {
		log.Printf("gst: Start: %s capture device id: %s", captureSourceFactory, opts.AudioDeviceID)
		// Everything below the log line about WHICH element gets WHAT is platform
		// knowledge and lives in deviceprovider_windows.go / deviceprovider_darwin.go:
		// the two platforms do not even agree on the TYPE of the device property.
		// See the darwin file — this is the single most important structural
		// difference in the port.
		if err := configureCaptureSource(asrc, opts.AudioDeviceID); err != nil {
			return abort(err)
		}
	}

	// The channel map, and it has to happen HERE — after the capture source has
	// been pointed at a device and BEFORE the pipeline leaves NULL.
	//
	// Not earlier, because the source's pad cannot say what it will produce
	// until it knows which device it is. Not later, because a matrix is not a
	// gain applied to a running stream, it is a NEGOTIATION CONSTRAINT:
	// audioconvert cannot map sixteen unpositioned channels to stereo without
	// one, so a pipeline that reaches PLAYING first never reaches it at all.
	// Measured — decklinkaudiosrc channels=16 into this chain with no matrix
	// dies 0.069 s after PLAYING with "streaming stopped, reason
	// not-negotiated (-4)", and written without the resampler in between it
	// does not even parse.
	p.aconv = pipeline.GetByName(nameAudioConv)
	if p.aconv == nil {
		return abort(errors.New("gst: parsed pipeline has no element named " + nameAudioConv))
	}
	p.aconvSinkPad = p.aconv.GetStaticPad("sink")
	if p.aconvSinkPad == nil {
		return abort(errors.New("gst: " + nameAudioConv + " has no sink pad"))
	}
	if err := p.applyStartChannelMapLocked(asrc, opts.ChannelMap); err != nil {
		return abort(err)
	}

	// ARM THE PICKER METER, and only now, because the condition is what
	// applyStartChannelMapLocked just decided. chlevel is built with
	// post-messages=false (see pipelineDescription) so that a seat with a
	// positioned capture source — every microphone, every Dante endpoint, the
	// whole of the on-air Windows path — posts not one per-channel frame for the
	// length of a match. A matrix having been written is exactly the statement
	// "this source presents channels nobody has positioned", which is the only
	// case in which sixteen bars mean anything that the two on the programme
	// meter do not already say.
	//
	// A nil callback disarms it just as firmly: frames posted for a consumer that
	// does not exist are cost with no reader, and the element's own property is a
	// cheaper way to not have them than a nil check on a streaming thread.
	p.armChannelMeterLocked(pipeline, p.matrixWidth > 0 && opts.OnChannelLevels != nil)

	p.encoder = pipeline.GetByName(nameVideoEncod)
	if p.encoder == nil {
		return abort(errors.New("gst: parsed pipeline has no element named " + nameVideoEncod))
	}
	applyEncoderProperties(p.encoder, encoderName, opts.VideoBitrateKbps)

	// The preview sink's surface, and the properties that could not safely go in
	// the parse string. It MUST happen before the pipeline leaves NULL: a
	// GstVideoOverlay with no window handle makes its OWN top-level window, with
	// a title bar and a close button, over the commentator's screen.
	//
	// It does nothing at all when there is no preview branch. Every error it can
	// return means the parsed graph is not the one this package just asked for,
	// so it aborts — and the rebuild in Start is what stops that costing the
	// commentary anything.
	if err := attachPreview(pipeline, opts.Preview, preview); err != nil {
		return abort(err)
	}

	p.srtq = pipeline.GetByName(nameSRTQueue)
	if p.srtq == nil {
		return abort(errors.New("gst: parsed pipeline has no element named " + nameSRTQueue))
	}
	p.srtqSrcPad = p.srtq.GetStaticPad("src")
	if p.srtqSrcPad == nil {
		return abort(errors.New("gst: " + nameSRTQueue + " has no src pad"))
	}
	p.srtqSinkPad = p.srtq.GetStaticPad("sink")
	if p.srtqSinkPad == nil {
		return abort(errors.New("gst: " + nameSRTQueue + " has no sink pad"))
	}

	// Close the gate BEFORE the pipeline can produce a buffer. Start installs
	// no sink, so srtq's src pad has no peer; without the gate the queue's loop
	// would push into nothing, get GST_FLOW_NOT_LINKED, pause its task and post
	// an error before the first ReplaceSink ever ran.
	p.gateClosed.Store(true)
	p.srcProbeID = p.srtqSrcPad.AddProbe(gateProbeMask, p.gateProbe)
	if p.srcProbeID == 0 {
		return abort(errors.New("gst: gst_pad_add_probe failed on " + nameSRTQueue + ":src"))
	}
	p.sinkProbeID = p.srtqSinkPad.AddProbe(gateProbeMask, p.gateProbe)
	if p.sinkProbeID == 0 {
		return abort(errors.New("gst: gst_pad_add_probe failed on " + nameSRTQueue + ":sink"))
	}

	// The event half of the sink-pad gate. Separate probe, separate mask and
	// separate condition from the buffer gate above: it drops a downstream
	// event only while the queue's loop is stopped with a bad flow return,
	// which is the one state in which gst_queue_handle_sink_event answers an
	// event by erroring the capture chain out. See eventGateProbe.
	//
	// srtq:src is captured here rather than read from p.srtqSrcPad inside the
	// callback: the field is cleared by teardownLocked, and a streaming thread
	// reading it while the caller's goroutine nils it would be a data race on
	// the very path that is being torn down.
	srtqSrc := p.srtqSrcPad
	p.sinkEventProbeID = p.srtqSinkPad.AddProbe(eventGateProbeMask,
		func(_ gogst.Pad, info *gogst.PadProbeInfo) gogst.PadProbeReturn {
			return p.eventGateProbe(srtqSrc, info)
		})
	if p.sinkEventProbeID == 0 {
		return abort(errors.New("gst: gst_pad_add_probe failed for downstream events on " +
			nameSRTQueue + ":sink"))
	}

	// The bus sync handler is attached before the first state change so that an
	// error raised during NULL→PLAYING is captured rather than lost. It is a
	// sync handler rather than a watch because a watch needs a GLib main loop
	// and this process does not have one — Wails owns the Windows message loop.
	p.bus = pipeline.GetBus()
	if p.bus == nil {
		return abort(errors.New("gst: pipeline has no bus"))
	}
	// busSilenced is cleared before the handler is attached and set by
	// teardownLocked; it is what replaces detaching the handler. See
	// teardownLocked for why the handler is never detached.
	p.busSilenced.Store(false)
	p.bus.SetSyncHandler(p.onBusMessage)

	// Specification section 6.1, in exactly this order. Every line matters.
	p.clock = gogst.SystemClockObtain()
	if p.clock == nil {
		return abort(errors.New("gst: gst_system_clock_obtain returned nil"))
	}
	// Pin the clock so it is never renegotiated when elements are added or
	// removed. Without this, installing the first srtsink could change the
	// pipeline's clock and move running time under the muxer.
	pipeline.UseClock(p.clock)
	// A start time of GST_CLOCK_TIME_NONE stops GstPipeline recomputing base
	// time on every PAUSED→PLAYING transition. gst_pipeline_change_state only
	// calls gst_element_set_base_time when start time is valid, so this is what
	// makes the SetBaseTime below survive.
	pipeline.SetStartTime(gogst.ClockTimeNone)

	savedBaseMu.Lock()
	if savedBase == gogst.ClockTimeNone {
		savedBase = p.clock.GetTime()
		log.Printf("gst: sampled the process-lifetime base time: %d ns", uint64(savedBase))
	}
	base := savedBase
	savedBaseMu.Unlock()

	// The same value on every rebuild, forever.
	pipeline.SetBaseTime(base)

	// Do NOT set start-time-selection=first on mpegtsmux. That reproduces the
	// measured bug: audio DTS jumping backwards by exactly the previous run's
	// uptime, 1,523 non-monotonic errors downstream, every indicator green.

	stopWatchdog := stateChangeWatchdog("pipeline NULL to PLAYING (opening the capture endpoint " +
		"and initialising the encoders)")
	ret := pipeline.BlockSetState(gogst.StatePlaying, gogst.ClockTime(pipelineStartTimeout))
	stopWatchdog()
	if !stateChangeOK(ret) {
		err := fmt.Errorf("gst: pipeline would not go to PLAYING (%s)", ret)

		// THE THREE RUNGS BELOW EXIST SO THAT A CARD FAILURE IS NAMED HERE,
		// rather than reaching the operator as "not-negotiated (-4)" twenty
		// seconds after START with a commentator waiting.
		//
		// MEASURED: DeckLink contention produces "Internal data stream error /
		// not-negotiated (-4)" in about 100 microseconds, and it names neither
		// the device nor the cause. At the GStreamer level that message is
		// identical for a card another application is holding, a card that has
		// been unplugged and a card with nothing on its input — three problems
		// with three completely different fixes. capturefault.go tells them
		// apart from the card's own evidence, and this is the path that was not
		// yet using it.
		// A bus error arrived during the transition and onBusMessage has ALREADY
		// diagnosed it — classAudioCapture goes through captureFatalError, and
		// on a DeckLink seat a video-element error is upgraded to that class
		// because the commentary is clocked by it. So the named sentence already
		// exists. %v and not %w deliberately: this is a Start failure the caller
		// handles by not starting, and putting ErrPipelineFatal at the head of
		// it would tell internal/sender it had a running pipeline whose chain
		// had died.
		fatal := p.fatalError()
		busErr := p.drainStartupError()
		switch {
		case fatal != nil:
			err = fmt.Errorf("%w: %v", err, fatal)
		case busErr != nil:
			// Kept exactly as it was: a Start that loses an asynchronous error
			// entirely is worse than one that reports it undiagnosed.
			err = fmt.Errorf("%w: %v", err, busErr)
		case opts.AudioCaptureID != "" || opts.VideoCaptureID != "":
			// The state change failed with nothing on the bus at all, and a card
			// is in this graph. The evidence is still there to be read — the
			// card's own signal property above all — so the diagnosis runs here
			// too. It degrades to naming the three things to check, in order,
			// which is what an operator can act on and "(failure)" is not.
			err = fmt.Errorf("%w: %v", err, captureFatalError(asrc, nameAudioSrc, err))
		}
		return abort(err)
	}

	// BlockSetState reporting success is NOT proof the capture chain is
	// healthy. wasapi2src opens its endpoint on its own thread and posts
	// failure asynchronously — error 1551 for a device it cannot open — so
	// NULL→PLAYING can return success while onBusMessage has already latched a
	// pipeline-fatal error. Without this re-check Start reports a running
	// pipeline whose capture chain is dead, and the caller connects a sink
	// that will never carry media. ReplaceSink double-checks fatal before
	// promising success for exactly the same reason; this mirrors it.
	//
	// osxaudiosrc opens its device inside the state change rather than on its
	// own thread, so on macOS the failure is more likely to come back through
	// BlockSetState above. The re-check costs nothing and covers both.
	if err := p.fatalError(); err != nil {
		return abort(err)
	}

	// CONFIRM THE MATRIX AGAINST WHAT THE PAD REALLY NEGOTIATED.
	//
	// The width the matrix was built for came from a caps QUERY, made before
	// the pipeline left NULL and therefore before negotiation had happened. The
	// number below comes from the pad's CURRENT CAPS, which is negotiation's
	// own answer. They are the same number on every device measured, and the
	// point of asking twice is that a disagreement is otherwise undiagnosable:
	// a matrix of the wrong width does not produce a degraded feed, it produces
	// "streaming stopped, reason error (-5)" out of the capture source, which
	// reads as a broken card rather than as a bad matrix.
	//
	// A ZERO here is NOT a failure and must not be treated as one. The capture
	// source is live; BlockSetState returns NO_PREROLL as soon as the state
	// change completes, which can be before the first CAPS event has travelled
	// downstream, so "the pad has not settled yet" is an ordinary race and not
	// a fault. It is logged and nothing more; InputChannels() re-reads the pad
	// on every call and will have the answer by the time any UI asks.
	if p.matrixWidth > 0 {
		switch got := p.negotiatedInputChannelsLocked(); {
		case got == 0:
			log.Printf("gst: Start: the %s matrix was written for %d input channels; the pad has "+
				"not published its negotiated caps yet, so the width is confirmed on the first "+
				"InputChannels call rather than here", nameAudioConv, p.matrixWidth)
		case got != p.matrixWidth:
			return abort(fmt.Errorf("gst: the %s mix-matrix was built for %d input channels and "+
				"%s:sink negotiated %d. A matrix of the wrong width does not attenuate or misroute, "+
				"it stops the capture chain with a flow error naming the source rather than the "+
				"matrix, so this is refused here while it can still be read as what it is",
				nameAudioConv, p.matrixWidth, nameAudioConv, got))
		default:
			log.Printf("gst: Start: %s:sink negotiated %d input channels, matching the matrix",
				nameAudioConv, got)
		}
	}

	// THE VIDEO SIGNAL WATCHDOG, and it is the last thing Start does before
	// returning nil so that no abort() path above can leak its goroutine.
	//
	// signalwatch.go has the measurements. The short version is that a DeckLink
	// which loses its input keeps emitting black frames at full rate forever — no
	// error, no EOS, the muxer never starves — so every other indicator in this
	// application goes on saying the feed is healthy, and the bus warning that
	// would say otherwise is edge-triggered and routed to logWarnings. Polling
	// the element's own "signal" property is the only reading that tracks it.
	//
	// ONE PROPERTY READ DECIDES WHETHER TO WATCH AT ALL, and that read doubles as
	// the debouncer's seed. triUnknown — no element with a signal property, which
	// is every native capture and the slate-only video leg shipping today — means
	// no goroutine, no ticker and nothing paid for the life of the pipeline.
	//
	// BOTH NAMES NOW EXIST IN pipelineDescription, which is what makes this
	// live: vcapsrc when the video leg is the card, and vcapclock when the
	// picture is the slate and the element is there only to clock a DeckLink
	// COMMENTARY. On a seat with neither — every native microphone with a slate,
	// which is the whole of the on-air Windows path — both lookups return nil,
	// signalWatchWanted is never even asked, and this costs one nil check.
	//
	// The clock companion case is the one where this watchdog earns the most.
	// There the card's lock is not a property of the PICTURE at all: a DeckLink
	// drives audio capture off the video clock, so losing signal means losing the
	// COMMENTARY, silently, with the muxer still fed and the sender still
	// CONNECTED. Watching vcapclock is the only reading in this process that can
	// say so, and it is why videoCaptureElement looks for it by name.
	if vsrc := videoCaptureElement(pipeline); vsrc != nil {
		probe := boolPropertyTriState(vsrc, propSignal)
		if signalWatchWanted(opts.OnSignal, probe) {
			watched := nameVideoCaptureSrc
			if opts.VideoCaptureID == "" {
				watched = nameVideoCaptureClock
			}
			p.sigWatch = startSignalWatch(probe,
				func() triState { return boolPropertyTriState(vsrc, propSignal) },
				opts.OnSignal)
			log.Printf("gst: Start: watching %s for input lock every %v; the card's own signal "+
				"property is the only thing in this process that can tell", watched,
				signalPollInterval)
		}
	}

	p.started = true
	log.Printf("gst: pipeline PLAYING with no sink; base time %d ns, encoder %s", uint64(base), encoderName)
	return nil
}

// videoCaptureElement finds the element the signal watchdog polls: the video
// capture source, or the capture element that exists only to clock a DeckLink's
// audio when the video leg is a slate.
//
// Both names are capturefault.go's and are already what the capture-fault
// classifier looks for, which is the point of reusing them rather than minting a
// third name for the same element. Nil — neither present — is the ordinary
// answer on every pipeline shipping today and is not a fault.
func videoCaptureElement(pipeline gogst.Pipeline) gogst.Element {
	if el := pipeline.GetByName(nameVideoCaptureSrc); el != nil {
		return el
	}
	if el := pipeline.GetByName(nameVideoCaptureClock); el != nil {
		return el
	}
	return nil
}

// armChannelMeterLocked turns the per-channel picker meter's messages on or off
// on a pipeline that may already be PLAYING. p.mu is held.
//
// post-messages is a plain gboolean on the level element and is LIVE-SETTABLE:
// measured on a PLAYING pipeline at 61 us, stopping that element's messages dead
// — 0 in two seconds — while the other level element in the same pipeline
// carried on at its own rate, undisturbed, with the pipeline still in PLAYING
// and nothing renegotiated. That is what makes arming a decision this function
// can take at Start and a later work package can take when a drawer opens,
// rather than a decision baked into the parse string until the next restart.
//
// A missing element is LOGGED AND SURVIVED. The name is a literal in
// pipelineDescription and a const here, and a source guard keeps them in step,
// so a nil is not something a shipped build can produce — but the failure it
// would cause is a meter that never moves with nothing anywhere saying why, and
// that is exactly the failure worth one log line rather than a refusal to carry
// commentary.
func (p *cgoPipeline) armChannelMeterLocked(pipeline gogst.Pipeline, on bool) {
	el := pipeline.GetByName(channelLevelElementName)
	if el == nil {
		log.Printf("gst: the pipeline has no element named %s, so the per-channel meters cannot be "+
			"armed and the mapping screen's bars will never move", channelLevelElementName)
		return
	}
	// The same hasProperty discipline applyEncoderProperties and the videoscale
	// add-borders write keep, and for the same reason: a property write against
	// an element that does not have it is a GLib CRITICAL on stderr, where no
	// shipped build is looking, rather than an error anybody can act on.
	if !hasProperty(el, propPostMessages) {
		log.Printf("gst: %s has no %s property, so the per-channel meters cannot be armed or "+
			"silenced", channelLevelElementName, propPostMessages)
		return
	}
	el.SetObjectProperty(propPostMessages, on)
	log.Printf("gst: per-channel metering %s (%s %s=%t)",
		map[bool]string{true: "ON", false: "off"}[on],
		channelLevelElementName, propPostMessages, on)
}

// applyStartChannelMapLocked decides whether this capture source needs a mix
// matrix at all and, if it does, writes the first one. p.mu is held and the
// pipeline is still in NULL.
//
// # The discriminator: can the source give this pipeline a stereo pair by itself?
//
// The question is asked of the SOURCE'S OWN PAD, by intersecting its caps with
// the two-channel caps the rest of the audio leg needs. If the intersection is
// non-empty the source can produce the pair unaided — which is every native
// microphone, every Dante endpoint and the whole of the on-air Windows path —
// and NOTHING IS WRITTEN. That silence is the most important property of this
// function: a build with no capture card behaves exactly as it did before this
// file's mechanism existed, byte for byte, because no property is set.
//
// If the intersection is empty the source cannot, and the only reason a capture
// source cannot produce stereo is that its channels are UNPOSITIONED — sixteen
// of them, channel-mask=0x0, which is what a DeckLink card presents and what
// audioconvert has nothing to derive a downmix from.
//
// It is phrased as "what does this pipeline need, and can the source give it"
// rather than as "is the channel mask zero" deliberately. The mask is the
// CAUSE, but a mask test asks a question about a bitmask type this build's
// bindings do not marshal, and answers it for a condition that could equally
// arise another way. Intersecting caps is GStreamer's own way of asking whether
// two ends can agree, it needs no new type support, and it stays correct for
// any future source whose problem is not the mask.
//
// Measured on the port machine on 2026-08-16, every source this application can
// meet, with the pipeline still in NULL:
//
//	source                        stereo unaided   fixed width
//	decklinkaudiosrc channels=16       no               16
//	decklinkaudiosrc channels=8        no                8
//	decklinkaudiosrc channels=2        YES               2
//	osxaudiosrc                        YES        (a range)
//	audiotestsrc                       YES        (a range)
//
// The two columns say the whole design: exactly the sources that need a matrix
// are refused a stereo pair, and exactly those publish a fixed width to size it
// against. A source that can do stereo never reaches the width question at all,
// which is why "a range" in the last column is not a problem to solve.
//
// # Why the width can be read before negotiation has happened
//
// The count comes from a caps QUERY on the source's own pad, and decklinkaudiosrc
// answers it from its channels property without opening anything. That is not
// the same as trusting the property: the pad is asked, the pad's answer is what
// is used, and the number is CONFIRMED against gst_pad_get_current_caps once the
// pipeline is PLAYING — see the end of Start, where a disagreement is a hard
// failure. Asking earlier is unavoidable, because the matrix is a negotiation
// constraint and negotiation is the thing it has to happen before.
//
// The one setting this cannot resolve is channels=max, which publishes a CHOICE
// in NULL (2, or 8-or-16) and only fixes itself once the card has been opened.
// fixedChannelCount refuses it by name rather than guessing, and the refusal
// says to set the property explicitly. That is the right answer for this
// application anyway: sixteen has to be the steady state, because the channels
// property is not live-settable and reaching pairs 3/4 and up any other way
// costs a pipeline restart.
func (p *cgoPipeline) applyStartChannelMapLocked(asrc gogst.Element, m ChannelMap) error {
	srcPad := asrc.GetStaticPad("src")
	if srcPad == nil {
		return errors.New("gst: " + nameAudioSrc + " has no src pad, so the channel layout it will " +
			"produce cannot be read")
	}

	stereo := gogst.CapsFromString("audio/x-raw,channels=" + strconv.Itoa(ChannelMapOutputs))
	if stereo == nil {
		return errors.New("gst: gst_caps_from_string for the stereo probe caps returned nil")
	}
	// The source is named from its own FACTORY rather than from
	// captureSourceFactory, and that is not pedantry: the whole point of this
	// branch is that the capture source may be a decklink element rather than
	// the platform's own, and a log line that says "osxaudiosrc presents
	// sixteen channels" about a Blackmagic card sends the reader to the wrong
	// half of the system.
	factory := elementFactoryNameOf(asrc)

	if direct := srcPad.QueryCaps(stereo); direct != nil && !direct.IsEmpty() {
		log.Printf("gst: Start: %s can deliver the %d-channel pair this pipeline needs on its own; "+
			"no %s is written and audioconvert maps the layout as it always has",
			factory, ChannelMapOutputs, propMixMatrix)
		return nil
	}

	width, err := fixedChannelCount(srcPad.QueryCaps(nil))
	if err != nil {
		return fmt.Errorf("gst: %s cannot produce %d channels and %w. Without a channel count there "+
			"is no matrix to build, and without a matrix audioconvert has no way to map an "+
			"unpositioned stream to stereo",
			factory, ChannelMapOutputs, err)
	}
	if err := p.writeChannelMapLocked(m, width); err != nil {
		return err
	}
	log.Printf("gst: Start: %s presents %d channels it will not position; %s set to route %s",
		factory, width, propMixMatrix, m)
	return nil
}

// elementFactoryNameOf names the factory an element was made from, falling back
// to the element's own name when the factory cannot be reached.
//
// The fallback is never expected — every element in this pipeline came out of
// gst_parse_launch and therefore out of a factory — and it exists because this
// is used only in log lines and diagnostics, where an unknown name is worth
// less than an approximate one but far more than a crash.
func elementFactoryNameOf(el gogst.Element) string {
	if f := el.GetFactory(); f != nil {
		if name := f.GetName(); name != "" {
			return name
		}
	}
	return el.GetName()
}

// fixedChannelCount reads the ONE channel count a set of caps describes.
//
// It refuses anything else, and the refusal is the useful part. Caps carrying a
// choice — "channels: { 8, 16 }", which is what decklinkaudiosrc publishes in
// NULL when its channels property is left at "max" — describe a stream nobody
// has decided about yet, and a matrix built against either of those two numbers
// is a coin toss whose losing side stops the capture chain. Caps carrying
// several structures with different counts are the same problem.
//
// The caller's own message says what could not be done; this one names what was
// found, because "the source published channels: { 8, 16 }" points straight at
// the channels property, and "no channel count" does not.
func fixedChannelCount(caps *gogst.Caps) (int, error) {
	if caps == nil {
		return 0, errors.New("its pad published no caps at all")
	}
	if caps.IsAny() || caps.IsEmpty() {
		return 0, fmt.Errorf("its pad published %s, which fixes no channel count", caps.String())
	}

	width := 0
	for i := uint(0); i < caps.GetSize(); i++ {
		s := caps.GetStructure(i)
		if s == nil {
			continue
		}
		n, ok := s.GetInt("channels")
		if !ok {
			return 0, fmt.Errorf("its pad published %s, in which the channel count is not a fixed "+
				"integer. Set the source's channels property explicitly: a source still choosing "+
				"cannot be given a matrix", caps.String())
		}
		if width == 0 {
			width = int(n)
			continue
		}
		if int(n) != width {
			return 0, fmt.Errorf("its pad published %s, which offers more than one channel count "+
				"(%d and %d) and therefore fixes none", caps.String(), width, int(n))
		}
	}
	if width <= 0 {
		return 0, fmt.Errorf("its pad published %s, which fixes no channel count", caps.String())
	}
	return width, nil
}

// writeChannelMapLocked validates a map against a width and writes the matrix.
// p.mu is held.
//
// VALIDATION HAPPENS FIRST AND NOTHING IS WRITTEN IF IT FAILS, because the
// element's own rejection is invisible: an out-of-range coefficient makes
// GObject log a CRITICAL to stderr and LEAVE THE PREVIOUS MATRIX IN FORCE, and
// there is nothing readable afterwards that says which of the two is running.
// ChannelGainLimit carries the measurement. Everything this function could get
// wrong is therefore decided in channelmap.go, at Gate A, under -race.
//
// gst_util_set_object_arg rather than a typed setter: mix-matrix is a
// GST_TYPE_ARRAY, which neither go-gst nor go-glib v0.0.2 binds, so the
// property's own gst_value_deserialize is the only way in. mixMatrixArg renders
// the string it parses. This is the same mechanism applyEncoderProperties uses
// and for the same reason.
func (p *cgoPipeline) writeChannelMapLocked(m ChannelMap, width int) error {
	matrix, err := m.MixMatrix(width)
	if err != nil {
		return err
	}
	if !hasProperty(p.aconv, propMixMatrix) {
		return fmt.Errorf("gst: this build's audioconvert has no %s property, so an unpositioned "+
			"capture device cannot be mapped to stereo at all (GStreamer has had it since 1.16)",
			propMixMatrix)
	}
	gogst.UtilSetObjectArg(p.aconv, propMixMatrix, mixMatrixArg(matrix))
	p.matrixWidth = width
	return nil
}

// negotiatedInputChannelsLocked reads the channel count from aconv's sink pad's
// CURRENT caps — negotiation's own answer, not a query about what might happen.
// It returns 0 when nothing has negotiated. p.mu is held.
func (p *cgoPipeline) negotiatedInputChannelsLocked() int {
	if p.aconvSinkPad == nil {
		return 0
	}
	caps := p.aconvSinkPad.GetCurrentCaps()
	if caps == nil || caps.GetSize() == 0 {
		return 0
	}
	s := caps.GetStructure(0)
	if s == nil {
		return 0
	}
	n, ok := s.GetInt("channels")
	if !ok || n <= 0 {
		return 0
	}
	return int(n)
}

// InputChannels reports how many channels the capture pad has negotiated.
//
// It takes p.mu, which is the lock held across state changes and therefore for
// seconds at a time during Start and ReplaceSink. That is deliberate rather
// than an oversight: this method reads two fields that Start writes and
// teardownLocked clears, and a caller who arrives during a state change has
// nothing to be told anyway — there is no negotiated width until Start has
// finished, and a Stop in progress is about to make the answer 0. Blocking
// until the truth exists is better than answering from a cache that Start is
// halfway through invalidating.
func (p *cgoPipeline) InputChannels() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return 0
	}
	return p.negotiatedInputChannelsLocked()
}

// SetChannelMap rewrites the routing on a running pipeline.
//
// It is sized against the pad's NEGOTIATED count read at this moment, never
// against the count Start used and never against anything the caller supplies.
// That is the whole discipline: the caller cannot pass a width, so the caller
// cannot pass a wrong one.
//
// The pipeline never changes state and the feed is never interrupted. Measured
// on the real card on 2026-08-16: 119 us for the write, PLAYING throughout with
// nothing pending, and the change visible in the next level message — a known
// -9 dBFS tone read -8.9996 dBFS at unity, -15.0195 dBFS after a live write of
// 0.5, and -90.3 dBFS after a live rewrite that routed two silent channels
// instead. Routing and gain alike take effect immediately.
func (p *cgoPipeline) SetChannelMap(m ChannelMap) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return errors.New("gst: pipeline is stopped")
	}
	if !p.started || p.aconv == nil {
		return errors.New("gst: pipeline has not been started")
	}
	if p.matrixWidth == 0 {
		// No matrix was written at Start, which means the capture source
		// produces a positioned stream that audioconvert already maps. There is
		// nothing to change, and writing a matrix now would pin a channel count
		// on a pad that has negotiated without one — a renegotiation on a live
		// feed, to achieve a routing the device has no channels to route.
		return fmt.Errorf("%w: the capture device presents a positioned stream, which audioconvert "+
			"maps to stereo on its own; there is no channel map on this pipeline to change. A "+
			"channel map applies to a device that presents unpositioned channels, such as a "+
			"DeckLink card's sixteen", ErrChannelMap)
	}

	width := p.negotiatedInputChannelsLocked()
	if width == 0 {
		return fmt.Errorf("%w: %s:sink has not published negotiated caps, so there is no width to "+
			"size a matrix against. A matrix of the wrong width stops the capture chain",
			ErrChannelMap, nameAudioConv)
	}
	if err := p.writeChannelMapLocked(m, width); err != nil {
		return err
	}
	log.Printf("gst: channel map set live on %d input channels: %s", width, m)
	return nil
}

// drainStartupError takes one error off the asynchronous channel, so that a
// failure during Start can be reported by Start rather than left for a consumer
// that may never read it. It never blocks.
func (p *cgoPipeline) drainStartupError() error {
	p.errMu.RLock()
	defer p.errMu.RUnlock()
	if p.errsClosed {
		return nil
	}
	select {
	case err := <-p.errs:
		return err
	default:
		return nil
	}
}

// pipelineDescription renders specification section 5 as a gst_parse_launch
// string.
//
// Two things are deliberately not in it. The srtsink is absent because Start
// installs no sink — the first ReplaceSink installs the first one, which is
// what lets the chain stay in PLAYING for the life of the process. The slate
// path and the audio device id are absent because they are user-supplied
// strings and the parser's escaping rules are not something to trust a Windows
// path or a GUID to; they are set with g_object_set afterwards.
//
// encoderName is the H.264 factory resolved at runtime. audioBitrateBps is the
// AAC encoder's bitrate property, in bits per second — the same unit on
// mfaacenc and on atenc, which is the one piece of luck in this port. conform
// is the ALREADY-RESOLVED raster and rate the video leg is conformed to: Start
// resolves it and logs what it resolved, so this function never sees a zero or
// a nonsense one and does not check. videoCapture is non-empty when the video
// leg is a LIVE CAPTURE rather than the slate, and audioCapture when the
// COMMENTARY comes off a card rather than off the platform's own audio stack.
// Neither VALUE is used here — both persistent-ids are set with g_object_set for
// the same reason the slate path and the device id are — only whether each is
// empty, and, for the clock companion, whether they are empty TOGETHER.
//
// # Exactly two element names in it are per-platform
//
// captureSourceFactory (wasapi2src / osxaudiosrc) and aacEncoderFactory
// (mfaacenc / atenc), both from elements_windows.go and elements_darwin.go.
// EVERYTHING ELSE — the caps chain, the level element and its 50 ms interval,
// mpegtsmux alignment=7 pcr-interval=3600, both queues, the leaky srtq — is
// byte-identical on both platforms, and that is a deliberate and load-bearing
// property, not an accident of the port. The whole reason internal/sender and
// the timestamp discipline in this file can be trusted on macOS is that the
// graph they reason about is the same graph. A future change that makes any
// other part of this string conditional is a change to that promise and needs
// to be argued for on its own terms.
//
// # The string is no longer a constant, and here is that argument
//
// The two video capsfilters are rendered from conform rather than written out.
// That is a real change to the promise above and it is made deliberately,
// because the promise it was protecting is not the one the sentence literally
// says. What internal/sender and the timestamp discipline rely on is that the
// graph is the SAME GRAPH ON BOTH PLATFORMS — same elements, same order, same
// pad topology, same liveness — not that it is the same text on every run.
//
// That property survives intact, for a specific structural reason: the
// conditional is chosen IDENTICALLY on both platforms, from the same
// ConformTarget, by the same code in gst.go, which carries no build tag and no
// runtime.GOOS. Windows and macOS given the same PipelineOpts produce the same
// bytes here. The per-platform seam therefore stays exactly where it was —
// captureSourceFactory and aacEncoderFactory in elements_*.go, and nowhere
// else — and TestPlatformElementContractIsPinned still checks both ports from
// whichever host Gate A runs on.
//
// What it is NOT is a licence for the next conditional. Anything that varies
// the ELEMENTS, their ORDER or their LIVENESS by platform, by device or by
// configuration breaks the property this comment is actually about, and a
// reader who cites this paragraph as precedent for one has cited the wrong
// half of it.
//
// The alternative was to keep the constant and refuse to start on a switcher
// configured for anything but 1080p50. That is not tidier, it is a commentary
// position that cannot go on air; see FallbackConformTarget.
//
// # The second conditional: which SOURCE the video leg has
//
// This one varies the elements and it is exactly the thing the paragraph above
// says needs arguing for on its own terms. Here is the argument.
//
// What must not vary is the format everything BELOW the encoder sees. It does
// not: the two legs meet at one capsfilter carrying the same ConformTarget, and
// from " ! " + encoderName onwards the string is written ONCE, in one place,
// for both. h264parse, the byte-stream capsfilter, vq, mpegtsmux, srtq and
// every reconnect rule in internal/sender are handed an identical graph
// whichever leg is above them, and that is checked rather than asserted —
// TestBothVideoLegsMeetAtTheSameEncoder reads this function and fails if the
// encoder line is ever written twice.
//
// What varies above it is a SOURCE, and a source is the one thing in this graph
// the application has always been allowed to choose: the audio leg has been
// pointed at a different device on every seat since the first build, and
// nothing downstream has ever known or cared. A camera instead of a PNG is the
// same kind of choice made in the other leg, and the conform chain is what
// makes it the same kind of choice — a 1080i50 camera, a 720p59.94 camera and
// the card's own start-up placeholder all leave the leg as one raster and rate.
//
// The alternative was a second Pipeline implementation, or a slate leg that
// switches source live. Both were rejected on the same measurement:
// set_state(NULL) inside a blocking pad probe took the on-air leg from 50 fps
// to 0 PERMANENTLY, with the pipeline still reporting PLAYING and no error
// anywhere. The source is therefore decided at Start, from configuration, and
// never afterwards.
//
// # The third conditional: which SOURCE the AUDIO leg has, and its companion
//
// This is the second axis and it needs its own argument, because it does two
// things the video axis did not: it varies the element on the leg that IS the
// product, and it adds a CHAIN that feeds no output at all.
//
// THE SOURCE SWAP IS THE SAME KIND OF CHOICE the paragraphs above license, and
// it is the older of the two: the audio leg has been pointed at a different
// device on every seat since the first build, and nothing downstream has ever
// known or cared. What must not vary is the format everything below the mix
// matrix sees, and it does not — audioresample, the S16LE/48000/2ch capsfilter,
// alevel, the AAC encoder, aacparse and the aq queue are written ONCE, below the
// branch, for both sources. A DeckLink presents sixteen unpositioned channels
// instead of a positioned pair, and the NAMED audioconvert is what turns that
// into the same two; channelmap.go holds the model and applyStartChannelMapLocked
// decides, from the pad rather than from configuration, whether a matrix is
// needed at all. On a microphone seat nothing is written and the leg is byte for
// byte what ships today.
//
// THE CLOCK COMPANION IS THE PART THAT IS GENUINELY NEW, and it exists because
// of one measured fact: decklinkaudiosrc CANNOT PREROLL without a
// decklinkvideosrc in the SAME pipeline. The card drives audio capture off the
// video clock, and with no video element the audio branch produced ZERO buffers
// — 0 level messages against 160. It is not a defensive addition; without it the
// feature does not start.
//
// So there are four shapes, and the choice between them is made HERE, from the
// two ids and nothing else:
//
//	video    audio      decklinkvideosrc built            why
//	slate    native     none                              today's pipeline, byte for byte
//	card     native     vcapsrc, feeding the encoder      the video leg that landed already
//	card     card       vcapsrc ONLY — it serves both     the card is EXCLUSIVE. Two sources
//	                                                      in one process fail 3/3 and two
//	                                                      processes fail 3/3, so a second one
//	                                                      is impossible, not merely wasteful
//	slate    card       vcapclock, feeding fakesink       the only way to clock the audio when
//	                                                      the picture is a still
//
// THE SLATE-PLUS-CARD SHAPE IS THE ONE THAT COULD DO HARM, so what it costs was
// measured rather than reasoned about: the companion source straight into
// fakesink is 0.6-2.4 % of one core, and it is a chain of its own with no pad
// linked to anything the slate leg touches. sync=false async=false is what keeps
// it out of the way of everything else — it never participates in the pipeline's
// preroll latching and never paces itself against the clock, so a card with no
// signal cannot delay or stall a state change the slate leg is also making.
//
// THE ALTERNATIVE WAS decklinkaudiosrc ALONE, and it is not a trade-off that was
// balanced — it does not work. The next alternative was a second process holding
// the card for the clock, which the exclusivity measurement rules out. There is
// no third.
//
// # What the capture leg is, element by element, and what breaks without each
//
// Every line below was measured on the fitted UltraStudio 4K Mini on
// 2026-08-16. None of it is defensive habit.
//
//	mode=auto             ONLY. A pinned mode that disagrees with the input does
//	                      not fail — measured, mode=pal against a real 1080p25
//	                      input produced 50 clean PAL buffers with nothing but a
//	                      warning. Green lamp, real bitrate, black picture.
//	connection            IS NEVER SET, not here and not anywhere in this
//	                      package. It is not a per-pipeline selection: it
//	                      PERSISTENTLY RECONFIGURES THE CARD and overrides what
//	                      the operator set in Blackmagic Desktop Video Setup, and
//	                      it has had to be undone by hand twice. The card's input
//	                      is chosen in Desktop Video Setup and leaving the
//	                      property alone is what makes that work. If a capture is
//	                      black or silent the answer is NEVER another connection
//	                      value.
//	drop-no-signal-frames Left at its default of false, and stated so that the
//	                      default changing is a visible edit rather than a silent
//	                      one. False means the card keeps emitting black
//	                      GAP-flagged frames at full rate FOREVER on signal loss
//	                      — no error, no EOS, the muxer never starves. That is
//	                      why the feed's continuity is not at risk, and it is
//	                      also why nothing in this pipeline can tell you the
//	                      signal has gone: the only thing that can is the card's
//	                      own signal property, which is what signalwatch.go
//	                      polls.
//	videoconvert          The card negotiates UYVY or v210 depending on the
//	                      input; the encoder wants NV12.
//	deinterlace           The only thing in this chain that handles a 1080i50
//	                      camera, which is still what a good deal of outside
//	                      broadcast kit produces. It passes progressive through
//	                      at essentially zero cost, so it is not a trade — it is
//	                      free insurance against the one input format that would
//	                      otherwise reach the encoder as interlaced frames the
//	                      switcher will not take.
//	videoscale            The camera's raster need not be the switcher's.
//	videorate             MANDATORY, and the least obvious element here.
//	                      decklinkvideosrc emits a 720x486 NTSC PLACEHOLDER as
//	                      its FIRST BUFFER on every start, with GAP set and
//	                      signal=false, and the real caps arrive about 170 ms
//	                      later. A fixed capsfilter with no videorate in front of
//	                      it dies 0.088 s after PLAYING with not-negotiated (-4),
//	                      3 runs out of 3. It also absorbs a camera whose rate is
//	                      not the switcher's.
//	tee allow-not-linked  The broadcast branch is linked below; the preview
//	                      branch is optional and arrives as this function's
//	                      `preview` argument, rendered by previewBranchFor and
//	                      empty on every seat that has not asked for one. Without
//	                      allow-not-linked=true a tee with an unlinked src pad
//	                      returns NOT_LINKED upstream and stops the leg — which is
//	                      to say the DEFAULT configuration is the one that needs
//	                      the property, not the preview. A SECOND
//	                      decklinkvideosrc IS NOT AN OPTION — the card is
//	                      exclusive, two sources in one process fail 3/3 and two
//	                      processes fail 3/3 — so sharing this one through a tee
//	                      is the only shape a preview can have.
//	queue vcapq           The branch head queue. Bounded by TIME only, one
//	                      second, exactly as vq below is: a plain queue's default
//	                      10 MB bound is about three frames of 1080p NV12, which
//	                      would make the bound depend on the raster. It is NOT
//	                      leaky — this is the branch that goes on air, and
//	                      dropping its frames to protect a preview would be
//	                      backwards. The PREVIEW branch's head queue is the one
//	                      that must be leaky=downstream, because tee pushes
//	                      serially on the upstream thread and a preview that
//	                      merely renders slowly was measured dragging this
//	                      branch from 50 fps to 20.8.
//
// proxysrc was measured as an alternative to the tee and REJECTED: its internal
// queue is leaky=0, so a wedged consumer stalls the producer from 50 fps to 0 in
// under two seconds, and the producer's death is silent across it. A tee INSIDE
// the contribution pipeline crosses no boundary CONTRACT.md draws.
//
// # The capture leg was proven end to end against a live M2L-X, 2026-08-16
//
// Not against a loopback and not against a receiver written to be agreeable:
// against the live instance matchH, through this package's own Pipeline, with
// the card locked to a real 525i59.94 input — 720x486, interlaced,
// bottom-field-first, bt601, pixel aspect 10:11, 29.97 fps, which is the worst
// input the conform chain can be handed.
//
// 45 seconds of it were ingested by cam4 "COMMS", and switcher_status reported,
// on two independent reads twelve seconds apart:
//
//	stream_state              "streaming"
//	video format              h264, width 1920, height 1080, scan_type "P",
//	                          sample_format "420", bit_depth 8, YCbCr
//	audio format              aac, 2 channels, 48000
//	bitrate                   10463.0 then 10454.0 kbit/s (asked for 10000)
//	error_packet_count        0
//	discontinuous_packet_count 0
//
// A local listener-first srtsink run immediately before it captured the same
// stream to disk and settles what was actually on the wire rather than what a
// switcher said about it: 25,301,792 bytes in 19.75 s, PID 0x41 carrying
// 10,041 kbit/s of video (98.0 % of packets) and PID 0x42 179 kbit/s of audio,
// decoding as H.264 High profile level 4.2, 1920x1080, NV12, progressive,
// bt709, pixel aspect 1/1 — which is conform.captureCaps() field for field.
//
// The conform chain is what that proves. A 720x486 interlaced 29.97 fps
// standard-definition input left this leg as 1920x1080p50 square-pixel bt709
// progressive, continuously, for 45 seconds, with not one discontinuous packet
// at the far end.
//
// The full send path was proven end to end over real SRT on macOS on
// 2026-08-14, receiver first: 16.2 s of media reached a listener-first
// srtsrc, the received transport stream carried PID 0x41 video (91.2 %) and
// PID 0x42 audio (7.4 %), gst-discoverer reported "H.264 (High Profile)
// 1920x1080", and the audio decoded to 48 kHz stereo with real room tone on it
// (peak -36.0 dBFS, RMS -59.2 dBFS) rather than the digital silence a
// mis-negotiated AAC path produces.
func pipelineDescription(encoderName string, audioBitrateBps int, conform ConformTarget,
	videoCapture, audioCapture, preview string) string {
	// alignment=7 gives 7 x 188 = 1316-byte buffers, exactly one SRT payload,
	// so nothing fragments. pcr-interval=3600 is the specification's value.
	// leaky=downstream means output produced during an outage is dropped rather
	// than back-pressuring the live capture, so the encoder never stalls and
	// the timestamps never bunch.
	//
	// imagefreeze is-live=true is mandatory: without it the slate branch is not
	// a live source and will not pace correctly.
	//
	// config-interval=-1 puts SPS/PPS in front of every IDR so M2L-X can
	// re-lock mid-stream.
	//
	// videoscale is present so that the slate PNG does not have to be exactly
	// the conform target's size. assets/slate.png is 1920x1080 today and
	// CONTRACT.md says so, but the artwork is replaced every season by someone
	// who will not read this file, and without videoscale a 1920x1200 export
	// fails caps negotiation at Start with no diagnostic that names the size.
	// The videoconvertscale plugin is already required for videoconvert, so the
	// element costs nothing and does nothing at all when the slate already
	// matches. Since the conform target became an option that property matters
	// MORE, not less: a switcher configured for 720p is now a size the same
	// 1920x1080 artwork has to reach.
	//
	// VIDEOCONVERT AND VIDEOSCALE SIT BEFORE IMAGEFREEZE, and that is the whole
	// optimisation.
	//
	// They used to sit after it, which meant converting and scaling the SAME
	// STILL PICTURE fifty times a second for the length of the match. Moving
	// them above imagefreeze does the work ONCE, on the single frame pngdec
	// produces, and imagefreeze then repeats the finished NV12 buffer.
	//
	// Measured on macOS arm64, GStreamer 1.26.10, 2026-08-15, 500 frames at
	// 50 fps into fakesink, both orders, both slate sizes:
	//
	//	order                     1920x1080 slate   1920x1200 slate
	//	imagefreeze first (old)      2.85 s CPU        3.02 s CPU
	//	videoscale first (new)       0.04 s CPU        0.05 s CPU
	//
	// and the rendered NV12 buffers are BYTE-IDENTICAL between the two orders
	// for both slates (md5 of the first three frames, checked per size). It is
	// a pure reordering: nothing is dropped and nothing is approximated.
	//
	// The 1920x1200 property the paragraph above is about SURVIVES the reorder,
	// and survives it for the obvious reason — videoscale is still in the leg,
	// still upstream of the encoder, and now scales the odd-sized export to the
	// conform target BEFORE the freeze rather than after it. The 1920x1200 case
	// is in the measurement above precisely so that this is checked rather than
	// asserted: it started, and it produced the same bytes.
	//
	// The capsfilter has to SPLIT to allow the reorder, and the split is the
	// only subtle part. Everything about one picture — format, size, PAR,
	// colorimetry, interlace-mode — is pinned upstream of imagefreeze, where
	// videoscale can act on it. The frame RATE is pinned downstream, because
	// the rate is the one thing imagefreeze itself decides: it takes a single
	// buffer whose upstream framerate is 0/1 and repeats it at whatever rate
	// its src pad negotiates. Pinning framerate=50/1 upstream instead asks
	// pngdec for a frame rate it does not have, and the leg fails to
	// negotiate.
	//
	// There is deliberately NO capsfilter between the capture source and
	// audioconvert. One used to sit there pinning rate=48000,channels=2,
	// upstream of the resampler that exists to produce exactly that — where it
	// could not help, only refuse. wasapi2src in shared mode can only ever
	// produce its endpoint's mix format, and Dante Virtual Soundcard is
	// commonly configured at 44.1 or 96 kHz, so on a DVS endpoint that is not
	// at 48 k the caps filter fails negotiation at Start, twenty minutes before
	// kick-off, with an error naming neither the sample rate nor the device.
	//
	// macOS makes the same point louder rather than differently: measured on
	// this machine, the built-in microphone offers 48 kHz but the NDI Audio
	// device offers 44100 ONLY, and osxaudiosrc is likewise bound to whatever
	// the CoreAudio device is configured for. audioconvert and audioresample
	// convert whatever the endpoint gives us; the capsfilter that actually
	// matters is the one below them, pinning what enters the AAC encoder.

	// THE SLATE LEG, unchanged, and it is what a caller that configures nothing
	// gets. It ends at a capsfilter carrying the conform target, which is the
	// point the capture leg below also ends at and the reason the encoder line
	// is written once for both.
	videoLeg := "" +
		"filesrc name=" + nameSlateSrc +
		" ! pngdec" +
		" ! videoconvert" +
		" ! videoscale name=" + nameVideoScale +
		" ! " + conform.spatialCaps() +
		" ! imagefreeze is-live=true" +
		" ! " + conform.temporalCaps()

	// THE LIVE CAPTURE LEG, which replaces it whole. The long comment above has
	// the measurement behind every element and every property; the two that are
	// easiest to lose in a later tidy-up are that videorate is what survives the
	// card's NTSC placeholder first buffer, and that connection is NEVER set.
	//
	// The persistent-id is absent from this string deliberately, exactly as the
	// slate path and the audio device id are: it is set with g_object_set in
	// Start, through the same configureDeckLinkSource the audio source uses, so
	// one saved string reaches both kinds of decklink element by one route.
	if videoCapture != "" {
		videoLeg = "" +
			videoCaptureFactory + " name=" + nameVideoCaptureSrc +
			" mode=auto drop-no-signal-frames=false" +
			" ! videoconvert name=" + nameVideoCapConv +
			" ! deinterlace name=" + nameVideoCapDeint +
			" ! videoscale name=" + nameVideoCapScale +
			" ! videorate name=" + nameVideoCapRate +
			" ! " + conform.captureCaps() +
			" ! tee name=" + nameVideoCapTee + " allow-not-linked=true\n" +

			nameVideoCapTee + ". ! queue name=" + nameVideoCapQueue +
			" max-size-time=1000000000 max-size-bytes=0 max-size-buffers=0"
	}

	// THE COMMENTARY CAPTURE SOURCE. Exactly one element, and everything below
	// it in the return statement is written once for both.
	//
	// The persistent-id is absent for the same reason it is absent from the
	// video leg and the audio device id: Start sets it with g_object_set,
	// through the same configureDeckLinkSource, so one saved string reaches
	// every decklink element by one route and never through the parser's
	// quoting rules.
	//
	// channels is the ONE property set here, and deckLinkAudioChannels says why
	// it is 16 and why 2 — which would negotiate a positioned pair and need no
	// matrix at all — is the wrong answer. `connection` is NOT set. It never is.
	audioSource := captureSourceFactory + " name=" + nameAudioSrc
	if audioCapture != "" {
		audioSource = audioCaptureFactory + " name=" + nameAudioSrc +
			" channels=" + strconv.Itoa(deckLinkAudioChannels)
	}

	// THE CLOCK COMPANION, and it is built ONLY when the commentary comes off the
	// card AND the picture does not.
	//
	// When the picture DOES, the videoLeg above already put a decklinkvideosrc in
	// this pipeline and that one is the clock: the card is exclusive, so a second
	// source is not an option to weigh, it is a pipeline that fails. The
	// condition below is therefore an AND of both ids and not a test of the audio
	// one alone, and getting that wrong is not a wasted element — it is a seat
	// with a camera and a card microphone that will not start at all.
	//
	// It is its own chain, linked to nothing, and it carries a leading newline
	// the way the preview branch does so that the return below does not have to
	// know whether it is there.
	clockLeg := ""
	if audioCapture != "" && videoCapture == "" {
		clockLeg = "\n" + videoCaptureFactory + " name=" + nameVideoCaptureClock +
			// mode=auto and drop-no-signal-frames=false for the reasons the video
			// leg gives — and the second one matters MORE here, because this
			// element's whole job is to keep producing a clock. A card that
			// dropped its no-signal frames would stop the commentary rather than
			// merely blank a picture nobody is watching.
			" mode=auto drop-no-signal-frames=false" +
			// sync=false async=false: this sink must not participate in preroll
			// latching and must not pace against the clock. It exists so the CARD
			// runs, not so anything downstream of it does, and every frame it is
			// handed is thrown away.
			" ! fakesink name=" + nameVideoCaptureClockSink + " sync=false async=false"
	}

	return "" +
		"mpegtsmux name=" + nameMux + " alignment=7 pcr-interval=3600" +
		" ! queue name=" + nameSRTQueue + " leaky=downstream max-size-buffers=4000\n" +

		videoLeg +
		" ! " + encoderName + " name=" + nameVideoEncod +
		" ! video/x-h264,profile=high" +
		" ! h264parse config-interval=-1" +
		" ! video/x-h264,stream-format=byte-stream,alignment=au" +
		" ! queue name=vq max-size-time=1000000000 ! " + nameMux + ".\n" +

		// The level element sits AFTER audioconvert/audioresample and their
		// capsfilter, IMMEDIATELY BEFORE the encoder, and that placement is the
		// point: it measures the exact S16LE 48 kHz stereo signal that enters
		// the AAC encoder, so the input meters show what is ACTUALLY being
		// encoded and sent — wrong device, dead Dante endpoint, muted desk send
		// and all. Measuring upstream of the resample (or worse, in the
		// browser) would keep a meter moving while the on-air signal was
		// silence, which is a reassurance the operator must never be given.
		// interval=50000000 is 50 ms in nanoseconds — twenty element messages a
		// second, which the app throttles rather than trusts — and
		// post-messages defaults to true so it is not set here. The element
		// passes buffers through untouched; it adds measurement, not latency.
		//
		// alevel is a literal rather than an entry in the element-name const
		// block, deliberately: those consts exist because GetByName is called
		// with them, and nothing ever looks the level element up — its output
		// arrives as bus messages matched on the STRUCTURE name. The literal
		// is also what lets the Gate A source guard
		// (TestPipelineDescriptionMetersWhatIsEncoded) assert the exact text.
		//
		// The two factory names below are consts rather than literals for one
		// reason and it is not brevity: it makes the Gate A guard able to check
		// BOTH ports at once — it asserts the order of the elements here and
		// the exact value of each const in elements_windows.go and
		// elements_darwin.go, so neither port can be silently repointed at a
		// different encoder.
		//
		// audioSource is one of those two consts or audioCaptureFactory,
		// resolved above; the element NAME is nameAudioSrc either way, because
		// capturefault.go classifies the commentary capture by that name and a
		// second name would return a DeckLink audio failure to the nameless
		// fatal default.
		audioSource +
		// The PER-CHANNEL PICKER meter, and everything about where it sits is
		// deliberate. It is UPSTREAM of the audioconvert below, so it measures
		// the capture device's OWN channels — all sixteen of a DeckLink card's
		// unpositioned ones — before any routing decision has been applied to
		// them. alevel, further down, cannot answer this question at any price:
		// it sits after the capsfilter that pins channels=2, so by construction
		// it measures the post-mix stereo and can never say which of the
		// sixteen inputs the commentator is on. That is the whole reason the
		// mapping UI is usable — the operator asks the commentator to talk and
		// watches which bar moves.
		//
		// The name is a LITERAL and must stay one. levels.go's
		// channelLevelElementName holds the same string and onBusMessage routes
		// on it; TestEveryLevelElementInThePipelineIsRouted reads this literal
		// out of the source and fails BY NAME if levelKindForSource does not
		// know it, because a level element the handler does not recognise posts
		// frames that are silently dropped — a meter that never moves with
		// nothing in the log to say why.
		//
		// post-messages=false IS THE STEADY STATE, and it is what keeps a seat
		// with no capture card exactly as it was. Every native microphone and
		// every Dante endpoint presents a POSITIONED stereo pair, for which
		// this meter would report the same two numbers alevel already reports —
		// a duplicate of the programme meter, ten times a second, over the
		// webview bridge, for the whole of a ninety-minute match. So the
		// element is built silent and armed only when there is something worth
		// metering: applyStartChannelMapLocked writes a matrix exactly when the
		// source is unpositioned, and arms this element in the same breath.
		// MEASURED live on a PLAYING pipeline — setting post-messages took
		// 61 us, stopped that element's messages dead (0 in two seconds) and
		// left the OTHER level element in the same pipeline posting at its own
		// rate, undisturbed, with the pipeline still in PLAYING.
		//
		// It stays in the graph on a native seat rather than being built
		// conditionally, because whether the source is positioned is not
		// knowable until the element exists and its pad can be asked — which is
		// after gst_parse_launch has run.
		//
		// WHAT THAT COSTS THE ON-AIR PATH WAS MEASURED, because adding an
		// element to the audio chain every shipping seat runs is not something
		// to reason about from first principles. Four paired runs of the real
		// stereo chain (audiotestsrc, 2 channels, through audioconvert,
		// audioresample, the 2-channel capsfilter and alevel into a synced
		// fakesink), 32 seconds of audio each, on the port machine 2026-08-16:
		//
		//	without chlevel   user 0.11, 0.11, 0.10, 0.18 s
		//	with chlevel      user 0.12, 0.12, 0.12, 0.10 s
		//
		// The difference is smaller than the run-to-run noise — one run WITHOUT
		// the element was the most expensive of the eight. A SILENT level
		// element is nearly free, which is not what the posting measurements in
		// levels.go would have led you to expect: those bracket an element that
		// is analysing and posting, and post-messages=false takes that work out
		// rather than merely dropping the message at the end of it. So the
		// ability to arm this meter live, on a pipeline already carrying
		// commentary, is bought for no measurable cost on the path that never
		// uses it.
		//
		// The tidier end state is still to build this element only on the
		// DeckLink capture leg, which becomes possible the day that leg exists:
		// it is one condition in this string, decided by the same thing that
		// decides which capture source to build.
		// AN audioconvert ABOVE chlevel, AND IT IS NOT COSMETIC. level's sink
		// template accepts five formats — S8, S16LE, S32LE, F32LE, F64LE — and
		// interleaved only. audioconvert's accepts thirty, plus non-interleaved.
		// Putting chlevel directly on the capture source therefore NARROWS what
		// a capture device is allowed to negotiate, on every seat, including the
		// on-air Windows build where nothing here can be compiled or run.
		//
		// Measured on this machine, the failure it prevents:
		//
		//	audiotestsrc ! audio/x-raw,format=S24_32LE,... ! chlevel ! ...
		//	  WARNING: erroneous pipeline: could not link audiotestsrc0 to
		//	  chlevel, chlevel can't handle caps audio/x-raw,
		//	  format=(string)S24_32LE, rate=(int)48000, channels=(int)2
		//
		// osxaudiosrc negotiates S16LE either way, so darwin never sees it —
		// which is precisely why it would have shipped. wasapi2src takes its
		// caps from the device's WASAPI mix format and gst-plugins-bad's wasapi2
		// does emit S24_32LE for some devices, so the seat this breaks is a
		// Windows one, on hardware nobody here owns, on the platform that is
		// broadcasting.
		//
		// This converter costs one element and restores the old tolerance
		// exactly. It does NOT flatten the channel layout: measured, chlevel's
		// sink still sees channels=16 channel-mask=0x0 through it, so the meter
		// still reports every one of a DeckLink's unpositioned channels. The
		// mix-matrix conversion is still the NAMED audioconvert below; this one
		// only makes the format legal.
		" ! audioconvert" +
		" ! level name=chlevel interval=" + strconv.Itoa(channelLevelIntervalNs) +
		" post-messages=false" +
		// audioconvert IS NAMED, and the name is the routing engine's handle on
		// it. Its mix-matrix property is where a DeckLink card's sixteen
		// UNPOSITIONED channels are turned into the stereo pair pinned below,
		// and it is live-settable while the pipeline is PLAYING — measured at
		// 119 us with no renegotiation, which is what lets the mapping UI be a
		// real-time control rather than an apply-and-restart form. Start writes
		// the matrix before the first buffer; SetChannelMap rewrites it
		// afterwards. channelmap.go holds the model and every measurement.
		//
		// Nothing else about this element changes, and on a POSITIONED source —
		// every microphone, every Dante endpoint, the whole of the on-air
		// Windows path — no matrix is written at all and the element behaves
		// exactly as it did before it had a name.
		" ! audioconvert name=" + nameAudioConv + " ! audioresample" +
		" ! audio/x-raw,format=S16LE,rate=48000,channels=2,layout=interleaved" +
		" ! level name=alevel interval=50000000" +
		" ! " + aacEncoderFactory + " bitrate=" + strconv.Itoa(audioBitrateBps) +
		" ! aacparse ! audio/mpeg,mpegversion=4,stream-format=adts" +
		" ! queue name=aq max-size-time=1000000000 ! " + nameMux + "." +

		// THE CLOCK COMPANION, appended whole or not at all, for exactly the
		// reason the preview below is: the empty string is the ordinary answer
		// and leaves this description character for character the one that ships
		// today. It is a chain of its own and links to nothing above it.
		clockLeg +

		// THE CONFIDENCE MONITOR, appended whole or not at all. previewBranchFor
		// has already decided; the empty string is the ordinary answer and leaves
		// this description character for character the one that ships today. It
		// carries its own leading newline, so nothing here has to know whether
		// there is a branch.
		preview
}

// applyEncoderProperties sets the platform's encoder settings on whichever
// H.264 encoder was chosen, skipping any property that encoder does not have.
//
// A missing property is logged, not fatal. The encoder is resolved at runtime,
// so on a machine where the preferred encoder is absent most of these will be
// absent too; running that encoder at its own defaults and telling the log
// which settings did not apply is better than refusing to send commentary.
//
// bitrate is deliberately handled here rather than in h264EncoderProps, because
// it is the one setting that comes from PipelineOpts. Its UNIT is kilobits per
// second on mfh264enc and on vtenc_h264 alike — checked on both, because a
// factor-of-1000 disagreement here is a 2 kbit/s or a 2 Gbit/s feed, and
// neither fails loudly.
func applyEncoderProperties(enc gogst.Element, factoryName string, bitrateKbps int) {
	applied := make([]string, 0, len(h264EncoderProps)+1)
	skipped := make([]string, 0, len(h264EncoderProps)+1)

	set := func(name, value string) {
		if !hasProperty(enc, name) {
			skipped = append(skipped, name)
			return
		}
		// gst_util_set_object_arg deserialises the string into the property's
		// own GType, so one call handles enum nicks ("cbr"), gint and guint
		// alike. Encoders disagree about the type of identically named
		// properties, which is why this is not g_object_set with a typed value.
		gogst.UtilSetObjectArg(enc, name, value)
		applied = append(applied, name+"="+value)
	}

	// bitrate is in KILOBITS per second on mfh264enc ("Bitrate in kbit/sec")
	// and on vtenc_h264 ("Target video bitrate in kbps"), which is the unit
	// DefaultVideoBitrateKbps and PipelineOpts.VideoBitrateKbps use. Measured
	// on macOS: bitrate=2000 produced a 2.05 Mbit/s video PID.
	set("bitrate", strconv.Itoa(bitrateKbps))
	for _, prop := range h264EncoderProps {
		set(prop.name, prop.value)
	}

	log.Printf("gst: encoder %s: applied %s; not supported: %s",
		factoryName, strings.Join(applied, " "), strings.Join(skipped, " "))
}

// gateProbeMask is GST_PAD_PROBE_TYPE_BLOCK | _BUFFER | _BUFFER_LIST.
//
// It is NOT GST_PAD_PROBE_TYPE_BLOCK_DOWNSTREAM, which would add
// _EVENT_DOWNSTREAM. See the file comment: dropping a downstream event marks a
// sticky event as received by a sink that never got it, and the first buffer
// after the gate reopened would then reach srtsink with no segment. Keeping
// _BLOCK is what makes gst_pad_push_data exit before it re-pushes pending
// sticky events, which is exactly the behaviour wanted.
const gateProbeMask = gogst.PadProbeTypeBlock | gogst.PadProbeTypeBuffer | gogst.PadProbeTypeBufferList

// gateProbe is the pad probe callback, and the only code in this file that runs
// on a GStreamer streaming thread by design.
//
// It must stay exactly this cheap. One atomic load, one comparison, one return.
// No locks, no allocation, no calls into application code, and above all no
// waiting: a pad probe that waits is a media pipeline that deadlocks. All of
// the slow work of a sink swap happens on the goroutine that called
// ReplaceSink.
// The open case returns GST_PAD_PROBE_PASS, NOT GST_PAD_PROBE_OK. That is not
// a style choice and it is the difference between this package sending media
// and sending nothing at all.
//
// gst_pad_add_probe sets GST_PAD_FLAG_BLOCKED on the pad for the whole life of
// any probe whose mask contains GST_PAD_PROBE_TYPE_BLOCK — which gateProbeMask
// does, deliberately, for the reason in BUILD-NOTES.md section 4.4.
// do_probe_callbacks then parks the streaming thread in GST_PAD_BLOCK_WAIT
// after the callbacks have run, unless a callback answered DROP (item
// discarded, thread returns) or PASS (item delivered, thread returns, and the
// probe is consulted again for the next item). OK means "I have no opinion",
// and the pad stays blocked.
//
// Measured at Gate C on 2026-07-31, 300 ms of free-running fakesrc through
// `fakesrc ! queue ! fakesink sync=false async=false` with this exact mask:
//
//	DROP:  126555 probe calls,     0 buffers delivered   (gate shut)
//	OK:         1 probe call,      0 buffers delivered   (pad blocked for ever)
//	PASS:   56980 probe calls, 56980 buffers delivered   (gate open, 1:1)
//
// TestLiveGateProbeDoesNotBlockWhenOpen in live_test.go is that measurement.
// With OK the pipeline wedges the instant ReplaceSink opens the gate: mux's
// srcpad task blocks in the srtq:sink probe, aggregator's sink queues fill,
// wasapi2src stops, and M2L-X reports a connected peer that never locks. Do
// not "simplify" this back to OK.
func (p *cgoPipeline) gateProbe(_ gogst.Pad, _ *gogst.PadProbeInfo) gogst.PadProbeReturn {
	if p.gateClosed.Load() {
		return gogst.PadProbeDrop
	}
	return gogst.PadProbePass
}

// eventGateProbeMask is GST_PAD_PROBE_TYPE_EVENT_DOWNSTREAM and nothing else.
//
// It carries NO _BLOCK bit, deliberately. BUILD-NOTES.md section 8.3 measured
// what happens when a probe whose mask contains _BLOCK answers anything other
// than DROP or PASS: gst_pad_add_probe raises GST_PAD_FLAG_BLOCKED for the life
// of the probe and do_probe_callbacks parks the streaming thread. Without the
// bit there is no block to escape from and GST_PAD_PROBE_OK is simply "carry
// on", which is what the pass case below returns.
//
// It is installed ONLY on srtq's SINK pad. It must never be added to srtq's SRC
// pad — see eventGateProbe.
const eventGateProbeMask = gogst.PadProbeTypeEventDownstream

// eventGateProbe stops a downstream event reaching gst_queue_handle_sink_event
// while the queue's loop is poisoned. It is the event half of the sink-pad
// gate, and it exists because a peer loss was measured taking the whole capture
// chain down with it.
//
// # The failure (BUILD-NOTES.md section 8.6)
//
// On one of three genuine peer losses at Gate C, inside a single 21 ms window:
//
//	gst: srtout-7: Failed to write to SRT socket: Connection timeout (16)
//	    (gstsrtsink.c(240): gst_srt_sink_render ())
//	gst: srtq:        Internal data stream error. (gstqueue.c(1083):
//	    gst_queue_handle_sink_event ())
//	gst: asrc:        Internal data stream error. (gstbasesrc.c(3187):
//	    gst_base_src_loop ())
//	gst: aq / imagefreeze0 / vq: Internal data stream error.
//
// asrc is not sink-sourced, so markFatal fired and every later ReplaceSink
// returned pipeline-fatal. The capture chain — the one thing this file exists
// to keep in PLAYING for the life of the process (specification section 6.1) —
// was down, and its only documented recovery is Stop, New, Start, which nothing
// below the application layer can perform. The commentator is off air until a
// human presses STOP and then START.
//
// # The mechanism, and why the existing gate did not cover it
//
// gstqueue.c's sink event handler refuses a serialized event outright when
// queue->srcresult is not GST_FLOW_OK: it posts GST_ELEMENT_ERROR(STREAM,
// FAILED) and returns FALSE. gst_pad_push_event then fails back into
// GstAggregator, mpegtsmux stops, and the failure unwinds through every element
// above it.
//
// The BUFFER half of this gate cannot help, because gateProbeMask deliberately
// excludes _EVENT_DOWNSTREAM (section 4.4) — dropping an event on srtq's SRC
// pad would let push_sticky() mark STREAM_START, CAPS or SEGMENT as received by
// a sink that never saw it, and section 4.10's whole repair depends on those
// events being genuinely delivered through srtq:src. So events bypass the gate
// and reach the queue even with the gate shut. That is correct on the src pad
// and was simply never covered on the sink pad, where the reasoning does not
// apply: dropping here keeps the event out of the QUEUE, which is upstream of
// every sticky list section 4.10 touches.
//
// # The condition, and why it is the flow return rather than the gate flag
//
// The trigger is exactly "the queue's srcresult is bad", so that is exactly
// what is tested. gst_pad_get_last_flow_return on srtq:src is the same proxy
// rearmQueueLocked already uses and section 8.2 verified against real peer
// losses, and it is set by gst_pad_push a moment BEFORE gst_queue_loop copies
// it into srcresult — so the probe errs early, never late.
//
// Keying on gateClosed instead was considered and rejected. The gate is shut
// from Start until the first ReplaceSink succeeds, and that is precisely when
// mpegtsmux delivers STREAM_START, CAPS and SEGMENT for the first time; a gate
// that dropped events would throw them away before srtq:src ever recorded them,
// leaving rearmQueueLocked nothing to snapshot and the first real sink with no
// segment. The flow-return condition can only become true after media has
// already flowed.
//
// # What it costs, and the residual window
//
// A caps or tag update that mpegtsmux emits during an outage is lost: the
// queue never sees it, so srtq's sticky caps stay as they were. That is
// cosmetic here — section 8.7 records that mpegtsmux's streamheader caps
// already never update, srtsink writes bytes and does not read caps, and M2L-X
// locks normally either way — and it is strictly better than the alternative,
// which is the capture chain stopping.
//
// One window is not closed: an event can pass this probe while the flow return
// is still OK and then block on the queue's own mutex until after the loop has
// stored the failure. That is the same nanosecond-scale race the file comment
// already documents for buffers, against a gap between the peer loss and the
// next event that is measured in milliseconds.
//
// It runs on a GStreamer streaming thread, so it does not log; it counts, and
// hands a line to the warning goroutine by the same non-blocking route
// onBusMessage uses.
func (p *cgoPipeline) eventGateProbe(srtqSrc gogst.Pad, info *gogst.PadProbeInfo) gogst.PadProbeReturn {
	if srtqSrc == nil || srtqSrc.GetLastFlowReturn() == gogst.FlowOK {
		return gogst.PadProbeOK
	}

	p.eventsDropped.Add(1)
	kind := "event"
	if info != nil {
		if ev := info.GetEvent(); ev != nil {
			kind = ev.GetType().String()
		}
	}
	p.deliverWarning(fmt.Sprintf(
		"gst: dropped a downstream %s at %s:sink: the queue's loop is stopped with %s, and "+
			"gst_queue_handle_sink_event would have answered it by erroring the capture chain out "+
			"(BUILD-NOTES.md section 8.6). Total dropped this pipeline: %d",
		kind, nameSRTQueue, srtqSrc.GetLastFlowReturn(), p.eventsDropped.Load()))

	return gogst.PadProbeDrop
}

// onBusMessage is the bus sync handler.
//
// GStreamer calls it synchronously on whichever thread posted the message,
// which for a GST_ELEMENT_ERROR is the streaming thread that is in the middle
// of failing — the handler runs before that failure has propagated anywhere.
// That timing is load-bearing: closing the gate here is what stops srtsink's
// GST_FLOW_ERROR reaching mpegtsmux through the queue.
//
// It must not take p.mu. ReplaceSink holds p.mu while deliberately provoking
// these messages, so taking it here would deadlock the streaming thread against
// the caller.
//
// It returns BusDrop for every message: nothing else in this process reads the
// bus, and leaving messages queued on a bus with no watch is a slow leak.
func (p *cgoPipeline) onBusMessage(_ gogst.Bus, msg *gogst.Message) gogst.BusSyncReply {
	if msg == nil {
		return gogst.BusDrop
	}
	// Teardown has begun. Everything below this point would be pointless work
	// on a pipeline that is going to NULL, and deliver would drop it anyway.
	if p.busSilenced.Load() {
		return gogst.BusDrop
	}

	switch msg.Type() {
	case gogst.MessageError:
		source := "pipeline"
		if src := msg.Source(); src != nil {
			source = src.GetName()
		}
		debug, gerr := msg.ParseError()

		// CLASSIFY BEFORE CLOSING THE GATE, AND CLASSIFY FROM THE NAME ALONE.
		//
		// Something has to run before the store, because a VIDEO CAPTURE failure
		// must NOT close the gate: the gate is what stops media reaching the
		// sink, and the whole point of sparing a video fault is that the
		// commentary keeps flowing through it. Closing it would starve the SRT
		// peer and defeat the sparing entirely, so this genuinely cannot be
		// folded into the switch below — it has to move ahead of the store.
		//
		// A CONFIDENCE MONITOR failure must leave the gate open for the same
		// reason and a stronger one: the gate is between the mux and the sink,
		// and the preview hangs off a tee far upstream of both. Nothing it can do
		// reaches the feed, so closing the gate over one would starve the SRT peer
		// to no purpose whatever.
		//
		// WHAT RUNS THERE IS THE PART THAT COSTS NOTHING, and the split is not
		// cosmetic. classifyBusError with the zero captureLegs is three string
		// comparisons on a string already in hand: no allocation, no cgo, no
		// GObject lock. captureLegsFor is the opposite of all three — it crosses
		// into C for the element's name, walks up to the parent bin and runs
		// gst_bin_get_by_name over the whole graph — and running it here would
		// put a bin traversal in front of the store on the ON-AIR path, where an
		// srtout-N error arrives on every peer loss and the buffer carrying
		// GST_FLOW_ERROR into srtq is racing us. That race is not hypothetical:
		// BUILD-NOTES.md section 8.6 is the 21 ms window in which losing it took
		// the whole capture chain down and the commentary off air.
		//
		// The zero value is exactly right as a first answer. classifyBusError
		// consults legs in ONE branch — the video-capture prefix — so for every
		// other source the two stages are provably the same decision, and for
		// that one branch the refinement happens below, after the gate is
		// settled. See capturefault.go.
		class := classifyBusError(source, captureLegs{})

		if class != classVideoCapture && class != classPreview {
			// Close the gate before building the error value. Everything after
			// this point is allocation, and the buffer that is about to carry
			// GST_FLOW_ERROR into the queue is racing us.
			p.gateClosed.Store(true)
		}

		// STAGE TWO, and it is deliberately below the store rather than above
		// it. THE CASE THAT LOOKS LIKE AN EXCEPTION: when the commentary audio
		// comes off the same DeckLink as the video, the card drives audio
		// capture off the video clock, so an error from a video element is an
		// audio fault wearing a video element's name and must go fatal. Asking
		// that question is what costs the bin traversal, so it is asked only on
		// the one path that can be affected by the answer — never on the sink
		// path, which is the one that is on air.
		//
		// The gate is closed HERE when the answer upgrades the class, because
		// the first stage deliberately left it open. Closing it a few
		// microseconds late costs nothing on this path: the fault is at the
		// card, upstream of the mux, and there is no failing sink pushing a
		// GST_FLOW_ERROR buffer at srtq to lose a race against.
		if class == classVideoCapture && captureLegsFor(msg.Source()).AudioClockedByVideo {
			class = classAudioCapture
			p.gateClosed.Store(true)
		}

		err := fmt.Errorf("gst: %s: %v (%s)", source, gerr, debug)

		// A failure of the sink currently being installed belongs to the
		// ReplaceSink call that is installing it, not on the asynchronous
		// channel.
		if r := p.route.Load(); r != nil && r.name == source {
			select {
			case r.ch <- err:
			default:
			}
			return gogst.BusDrop
		}

		switch class {
		case classSinkSourced:
			// Unchanged, and tested first so the on-air path cannot move:
			// replacing the sink can repair it, so it goes to internal/sender
			// on Errors() and the connection ladder handles it.

		case classVideoCapture:
			// RECOVERABLE. The pipeline stays PLAYING, mpegtsmux keeps
			// aggregating audio, the sender keeps its socket.
			//
			// It must NOT reach Errors(): internal/sender treats ANY error
			// arriving while CONNECTED as the peer going away, and would spend a
			// whole DRAINING/BACKOFF cycle — seven seconds off air — on a fault
			// that never touched the feed.
			p.deliverWarning("gst: the video capture failed and the commentary is unaffected: " +
				err.Error())
			return gogst.BusDrop

		case classPreview:
			// The operator's confidence monitor. SPARED, and unlike
			// classVideoCapture there is no second stage that can upgrade it: the
			// preview is downstream of a leaky tee branch and feeds a window, so
			// nothing it does can reach the feed. This is a log line and nothing
			// more.
			//
			// It must NOT reach Errors(): internal/sender treats any error arriving
			// while CONNECTED as the peer going away and would spend a whole
			// DRAINING/BACKOFF cycle — seven seconds off air — over a monitor.
			p.deliverWarning("gst: the confidence monitor failed and the commentary and the feed " +
				"are unaffected: " + err.Error())
			return gogst.BusDrop

		case classAudioCapture:
			// Fatal, because the commentary IS the product and there is nothing
			// to degrade to — but NAMED. At this level "device busy", "device
			// missing" and "no signal" are the same generic stream error and
			// have three different fixes; capturefault_cgo.go reads the card's
			// own evidence to tell them apart.
			p.markFatal(captureFatalError(msg.Source(), source, err))

		default:
			// Not the sink and not a capture leg: replacing the sink cannot
			// repair this, so mark it and let ReplaceSink refuse rather than
			// report a connection that carries no media.
			//
			// The wrap puts ErrPipelineFatal — whose text is "gst:
			// pipeline-fatal", so the rendered message is unchanged and
			// anything still grepping for the substring keeps matching — at
			// the head of the chain, which is what lets internal/sender use
			// errors.Is to stop retrying a failure no reconnect can fix.
			p.markFatal(fmt.Errorf("%w: %w "+
				"(the capture or mux chain has failed; recover with Stop, New, Start)",
				ErrPipelineFatal, err))
		}
		p.deliver(err)

	case gogst.MessageWarning:
		debug, gerr := msg.ParseWarning()
		source := "pipeline"
		if src := msg.Source(); src != nil {
			source = src.GetName()
		}
		// Warnings are logged and NOT delivered on Errors(). A GStreamer
		// warning is not a pipeline failure, and putting it there would make
		// internal/sender treat it as one.
		//
		// The log call itself happens on logWarnings' goroutine, not here.
		// log.Printf takes a process-global mutex and blocks on stderr; a
		// marginal SRT link produces warnings in bursts, and this function runs
		// on a GStreamer streaming thread. Serialising the streaming threads
		// behind Go's log mutex during an outage would add latency to the
		// capture chain at the one moment it must not have any.
		p.deliverWarning(fmt.Sprintf("gst: warning: %s: %v (%s)", source, gerr, debug))

	case gogst.MessageElement:
		// A level element's measurement reports. Everything here runs on the
		// posting streaming thread, so the whole path is: two cheap rejections,
		// name the source, convert, hand to a callback that the contract in
		// gst.go requires not to block. No locks, no logging.
		//
		// TWO TESTS, IN THIS ORDER, AND THE ORDER IS THE POINT.
		//
		// The STRUCTURE name comes first because it is nearly free — a string
		// compare against a name the message already carries — and because it
		// is what rejects the element messages this handler is not interested
		// in at all. Other elements in this pipeline are free to post their
		// own and several do.
		//
		// The SOURCE ELEMENT comes second, and it used not to be tested at all.
		// That was the defect: every level element in the process posts a
		// structure named "level", so matching the structure alone means
		// matching EVERY level element there will ever be. With one element it
		// was merely sloppy. This tier makes a second one possible — chlevel on
		// the capture device's own sixteen unpositioned channels for the
		// picker, alevel on the mixed-down stereo that is actually encoded —
		// and then it is a silent cross-wire. MEASURED with both in one
		// pipeline: 39 level messages a second, every one of them matching the
		// structure name, so OnLevels would have been fed a sixteen-entry frame
		// and a two-entry frame alternately, twenty times a second each. The
		// programme meter would not have failed; it would have flickered
		// between two different signals at two different widths, and a meter
		// that reads as live while showing the wrong signal is the one failure
		// this application refuses to ship. msg.Source() separated them
		// perfectly in that measurement — 39 attributed, 0 unattributed.
		//
		// Naming the source costs a cgo call, a GObject lock and a string, so
		// it is deliberately BELOW the structure test: it is paid only for
		// messages that really are level reports, at most a few tens a second,
		// and never for the element messages of anything else.
		s := msg.GetStructure()
		if s == nil || s.GetName() != levelStructureName {
			break
		}
		source := ""
		if src := msg.Source(); src != nil {
			source = src.GetName()
		}
		switch levelKindForSource(source) {
		case levelKindProgramme:
			// alevel: the stereo that is being encoded and sent. Unchanged,
			// and it is the on-air path, so the callback load stays where it
			// was — after the match, so that a session with no metering does
			// no work per message beyond the two rejections above.
			f := p.onLevels.Load()
			if f == nil || *f == nil {
				break
			}
			if levels, ok := levelsFromStructure(s); ok {
				(*f)(levels)
			}

		case levelKindChannels:
			// chlevel: the capture device's own channels, upstream of the mix
			// down to the encoder's two. It is what the mapping UI meters, and
			// it is the only measurement in this pipeline that can say WHICH
			// input the commentator is on — alevel sits after the capsfilter
			// that pins channels=2 and by construction cannot.
			//
			// The three lines are deliberately identical to the programme
			// case's, and the callbacks are two fields rather than one so that
			// this frame cannot reach OnLevels however the code above it is
			// later rearranged. A sixteen-entry frame delivered to the
			// programme meter is not a crash; it is a meter that reads as live
			// while showing a signal that is not going to air.
			f := p.onChannelLevels.Load()
			if f == nil || *f == nil {
				break
			}
			if levels, ok := levelsFromStructure(s); ok {
				(*f)(levels)
			}

		default:
			// A level message this pipeline did not ask for. Dropped. An
			// unattributable level frame is precisely the cross-wire the
			// source test exists to prevent, and a meter fed from an unknown
			// element is worse than a meter that does not move.
		}
	}

	return gogst.BusDrop
}

// levelsFromStructure reads the "peak" and "rms" fields of a level element
// message structure into a clamped gst.Levels.
//
// Each field is a GValueArray of G_TYPE_DOUBLE, one entry per channel — the
// level element has posted exactly that shape since gst-plugins-good kept the
// deprecated GValueArray for message compatibility, and go-glib v0.0.2
// registers a marshaler for it (gobject/valuearray.go), so Structure.GetValue
// delivers a gobject.ValueArray, which is a named []any of float64s. The type
// switch below also accepts a plain []any, in case a future go-glib flattens
// the alias; anything else — a go-gst that starts returning InvalidValue for
// boxed types, a level element that switched to GstValueArray — makes this
// return ok=false and the meters simply stay empty, which is the correct
// degradation for a display: silent absence, never a crash on a streaming
// thread and never invented numbers.
//
// The message also carries a "decay" array of the same length, and this
// deliberately ignores it: decay is peak with peak-ttl's fall-back applied, and
// the fall-back the meters use is the UI's own, so reading it here would be a
// second opinion about the same number.
//
// It runs on a streaming thread: no locks, no logging, allocation limited to
// the two output slices. The unwrapping is the ONLY part of this that needs
// GStreamer; every decision about what the numbers mean — the channel count,
// the equal-length invariant the renderers rely on, the silence floor — is
// levelsFrom's, in levels.go, where Gate A runs it under -race.
func levelsFromStructure(s *gogst.Structure) (Levels, bool) {
	return levelsFrom(anyList(s.GetValue("peak")), anyList(s.GetValue("rms")))
}

// anyList unwraps the two list shapes go-glib may hand back for a GValueArray
// field. A nil return means the field was absent or of an unexpected type.
func anyList(v any) []any {
	switch l := v.(type) {
	case gobject.ValueArray:
		return []any(l)
	case []any:
		return l
	default:
		return nil
	}
}

// isSinkSourced reports whether a bus message source name belongs to the sink
// or to the leaky queue immediately in front of it.
//
// The queue counts because its errors are always consequential: it reports
// GST_FLOW_ERROR or GST_FLOW_NOT_LINKED that came from the sink, and
// ReplaceSink re-arms it. Anything else is upstream of the gate and is
// pipeline-fatal.
func isSinkSourced(source string) bool {
	return source == nameSRTQueue || strings.HasPrefix(source, srtSinkNamePrefix)
}

// markFatal records the first pipeline-fatal error. It is called from a
// streaming thread and so cannot take p.mu; errMu is reused because it is the
// only lock in this type that is safe to take there.
func (p *cgoPipeline) markFatal(err error) {
	p.errMu.Lock()
	if p.fatal == nil {
		p.fatal = err
	}
	p.errMu.Unlock()
}

// fatalError returns the recorded pipeline-fatal error, if any.
func (p *cgoPipeline) fatalError() error {
	p.errMu.RLock()
	defer p.errMu.RUnlock()
	return p.fatal
}

// deliver puts an error on the asynchronous channel without blocking, and
// without racing Stop's close.
//
// The contract in gst.go requires dropping rather than blocking: the caller is
// a GStreamer streaming thread and must never wait on a Go consumer.
func (p *cgoPipeline) deliver(err error) {
	p.errMu.RLock()
	defer p.errMu.RUnlock()
	if p.errsClosed {
		return
	}
	select {
	case p.errs <- err:
	default:
	}
}

// deliverWarning hands a pre-formatted warning line to the logging goroutine
// without blocking, and without racing Stop's close.
//
// Dropping under a storm is the correct behaviour: past a burst of sixteen the
// number of warnings is the diagnosis and the individual lines add nothing,
// whereas a streaming thread waiting on stderr adds latency to a live capture.
func (p *cgoPipeline) deliverWarning(line string) {
	p.errMu.RLock()
	defer p.errMu.RUnlock()
	if p.errsClosed {
		return
	}
	select {
	case p.warns <- line:
	default:
	}
}

// ReplaceSink installs a fresh srtsink with the given properties, replacing any
// sink already present.
//
// It returns synchronously. A nil error means the SRT caller handshake
// succeeded and media is flowing; a non-nil error means it did not, and
// internal/sender is responsible for backing off and trying again. Nothing
// upstream of srtq leaves PLAYING either way.
func (p *cgoPipeline) ReplaceSink(opts SinkOpts) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return errors.New("gst: pipeline is stopped")
	}
	if !p.started {
		return errors.New("gst: pipeline has not been started")
	}
	if err := p.fatalError(); err != nil {
		return err
	}
	if opts.Host == "" || opts.Port == 0 {
		return errors.New("gst: SinkOpts.Host and SinkOpts.Port are required")
	}
	if opts.Port < 1 || opts.Port > 65535 {
		return fmt.Errorf("gst: SinkOpts.Port %d is out of range", opts.Port)
	}
	if opts.LatencyMs == 0 {
		opts.LatencyMs = DefaultSRTLatencyMs
	}
	if opts.LatencyMs < 0 {
		return fmt.Errorf("gst: SinkOpts.LatencyMs %d is negative", opts.LatencyMs)
	}
	if opts.Passphrase != "" {
		if opts.PBKeyLen == 0 {
			opts.PBKeyLen = 16
		}
		if opts.PBKeyLen != 16 && opts.PBKeyLen != 32 {
			return fmt.Errorf("gst: SinkOpts.PBKeyLen must be 16 or 32, got %d", opts.PBKeyLen)
		}
	}

	// 1. Resolve the host to IP literals, BEFORE anything is torn down.
	//
	//    A hostname in the srtsink URI aborts the whole process on the next
	//    RemoveSink — a GLib assertion inside GResolver, from which there is no
	//    return. resolveSinkHost carries the measurement and the reasoning; the
	//    only thing that ever reaches srtsink is a literal.
	//
	//    It is done here, ahead of removeSinkLocked, for two reasons. A lookup
	//    failure then leaves a working sink working instead of trading a live
	//    feed for a DNS hiccup, and the up-to-three seconds it can cost are
	//    spent while the old socket is still carrying commentary rather than
	//    added to the time off air.
	addrs, err := resolveSinkHost(opts.Host, hostResolveTimeout)
	if err != nil {
		return err
	}

	// 2. Tear out whatever is there. This is the SAME teardown path RemoveSink
	//    uses — close the gate, unlink, NULL, remove, re-arm the queue — and it
	//    is the only one in this file. When internal/sender has honoured
	//    specification section 6.2 and already called RemoveSink on entry to
	//    DRAINING, this is a cheap no-op: there is no sink to detach and the
	//    queue's last flow return is already GST_FLOW_OK.
	if err := p.removeSinkLocked(); err != nil {
		return err
	}

	// 3. Backstop for the early returns below ONLY. The success path clears the
	//    route explicitly at step 7 and must keep doing so: a deferred clear
	//    runs after the function body has finished, which would leave the route
	//    installed across the final drain, across the gate opening and across
	//    the log line — and an error arriving in that window would be swallowed
	//    by a channel nobody ever reads again. That is the false green this
	//    whole package exists to prevent. Do not delete step 7 and lean on this.
	//
	//    It is declared once, outside the loop, rather than once per attempt:
	//    each attempt installs its own route over the previous one, so a single
	//    clear at return is enough and a defer inside the loop would stack.
	defer p.route.Store(nil)

	// A name may front several addresses — an SRT listener is one host, but DNS
	// does not know that. Try them in the order resolveSinkHost returned, IPv4
	// first, and report which one answered. lastErr carries the most recent
	// failure so that a caller who runs out of addresses is told why the last
	// one did not work rather than "no addresses left".
	var lastErr error
	for i, addr := range addrs {
		// 4. Build the new sink. The URI gets the literal; every log line and
		//    every error message below gets opts.Host, the name the operator
		//    typed.
		p.sinkSerial++
		name := srtSinkNamePrefix + strconv.Itoa(p.sinkSerial)
		where := dialledEndpointForLog(opts, addr)
		if len(addrs) > 1 {
			where = fmt.Sprintf("%s, address %d of %d", where, i+1, len(addrs))
		}

		sink := gogst.ElementFactoryMake("srtsink", name)
		if sink == nil {
			return errors.New("gst: could not create srtsink (is the srt plugin in the bundle?)")
		}
		if err := configureSRTSink(sink, opts, addr); err != nil {
			return err
		}

		// 5. Add and link. Adding before linking is required: gst_pad_link
		//    across a bin boundary on an element with no parent does not give
		//    it the pipeline's clock or base time.
		if !p.pipeline.Add(sink) {
			return fmt.Errorf("gst: could not add %s to the pipeline", name)
		}
		sinkPad := sink.GetStaticPad("sink")
		if sinkPad == nil {
			p.pipeline.Remove(sink)
			return fmt.Errorf("gst: %s has no sink pad", name)
		}
		if ret := p.srtqSrcPad.Link(sinkPad); ret != gogst.PadLinkOK {
			p.pipeline.Remove(sink)
			return fmt.Errorf("gst: could not link %s:src to %s:sink (%s)", nameSRTQueue, name, ret)
		}

		// 6. Divert this sink's bus errors into this call, then bring it up.
		//    The route is installed before the state change because srtsink
		//    posts the error and returns STATE_CHANGE_FAILURE from the same
		//    call.
		route := &sinkErrRoute{name: name, ch: make(chan error, 1)}
		p.route.Store(route)

		stopWatchdog := stateChangeWatchdog(name + ": SRT caller handshake to " + where)
		if !sink.SyncStateWithParent() {
			stopWatchdog()
			p.abandonSinkLocked(sink, sinkPad)
			lastErr = fmt.Errorf("gst: %s: SRT caller handshake to %s failed: %v",
				name, where, routeErrOr(route, errors.New("gst_element_sync_state_with_parent returned FALSE")))
			p.route.Store(nil)
			continue
		}
		ret := sink.BlockSetState(gogst.StatePlaying, gogst.ClockTime(sinkStateChangeTimeout))
		stopWatchdog()
		if !stateChangeOK(ret) {
			p.abandonSinkLocked(sink, sinkPad)
			lastErr = fmt.Errorf("gst: %s: SRT caller handshake to %s failed (%s): %v",
				name, where, ret, routeErrOr(route, errors.New("no bus error was posted")))
			p.route.Store(nil)
			continue
		}

		// 7. Clear the route BEFORE the last drain, and drain after clearing.
		//
		//    Order is the whole point. From the instant this store lands,
		//    onBusMessage stops diverting srtout-N's errors into a channel that
		//    is about to be abandoned and starts putting them on Errors(),
		//    where internal/sender reads them and reconnects. The drain that
		//    follows catches anything that arrived before the store.
		//
		//    Doing this with `defer` instead — which is what was here — leaves
		//    the route installed through the drain, through the gate opening
		//    and through the success log. srtsink accepting the socket and then
		//    failing its first write is M2L-X's ordinary one-peer / re-accept
		//    behaviour, not an exotic case; such an error would be matched by
		//    name, pushed into r.ch, and read by nobody. It would reach neither
		//    Errors() nor p.fatal, while onBusMessage had already set
		//    gateClosed. ReplaceSink would return nil, sender would go
		//    CONNECTED, the lamp would go green, and no reconnect would ever be
		//    triggered: commentary off air with every indicator healthy.
		p.route.Store(nil)
		if err := routeErr(route); err != nil {
			p.abandonSinkLocked(sink, sinkPad)
			lastErr = fmt.Errorf("gst: %s: SRT connection to %s failed immediately: %w",
				name, where, err)
			continue
		}

		// 8. A pipeline-fatal error — one whose source is mux or the capture
		//    chain rather than the sink — can have been posted by the churn of
		//    adding and starting an element. fatal is checked on entry to this
		//    function; check it again before promising success, so that the
		//    synchronous answer and the asynchronous one cannot disagree.
		//    Without this a caller can be told the connection came up in the
		//    same instant Errors() is told the muxer has stopped.
		//
		//    This one returns rather than trying the next address: no address
		//    can repair a broken capture chain.
		if err := p.fatalError(); err != nil {
			p.abandonSinkLocked(sink, sinkPad)
			return err
		}

		// 9. Open the gate. From this instant media flows to the new sink; the
		//    sticky events left pending by the gated pushes are delivered ahead
		//    of the first buffer by gst_pad_push_data's check_sticky.
		//
		//    A residual window remains and is deliberate: an error posted
		//    between step 7 and here sets gateClosed true, and this store then
		//    reopens a gate onto a sink that has already failed. That is not a
		//    false green, and the reason is a property of the CALLER, not of
		//    this file: the error is on Errors() by construction, and
		//    internal/sender's state machine performs no drain of its error
		//    queue after ReplaceSink returns — it drains only immediately
		//    BEFORE the call, where a queued message can only belong to the
		//    sink DRAINING already removed. Given that, the message survives,
		//    the sender tears the sink down, and the worst case is a few
		//    milliseconds of buffers pushed into a dead socket.
		//
		//    The dependency is stated because it has already been broken once.
		//    If a drain is ever reinstated on the far side of ReplaceSink, this
		//    window stops being harmless and becomes a PERMANENT false green:
		//    the discarded message is the only one there will ever be.
		//    onBusMessage has already set gateClosed, and the gate probe drops
		//    rather than blocks — a dropped probe returns GST_FLOW_OK, as the
		//    file comment on the gate explains — so srtq never takes a bad flow
		//    return, mpegtsmux never notices, and no further bus error is
		//    posted. The lamp stays green with nothing on the wire and no
		//    reconnect, and nothing in this file would say so.
		//
		//    Closing the window here instead would need the gate to be a
		//    compare-and-swap against a generation counter, which is more
		//    machinery than a microsecond window on a path that already
		//    recovers correctly.
		p.sink = sink
		p.gateClosed.Store(false)

		log.Printf("gst: %s connected to %s, latency %d ms, encryption %s",
			name, where, opts.LatencyMs, encryptionForLog(opts))
		return nil
	}

	if lastErr == nil {
		// resolveSinkHost never returns an empty list without an error, so this
		// is unreachable. It is here because the alternative to an unreachable
		// error is a nil return on a call that installed no sink, which is the
		// false green this package exists to prevent.
		return fmt.Errorf("gst: no address to dial for %s", endpointForLog(opts))
	}
	return lastErr
}

// RemoveSink tears the current sink out without installing another.
//
// Specification section 6.2 orders a reconnect DRAINING (tear down) -> BACKOFF
// (wait) -> CONNECTING (install), and that order is load-bearing: an M2L-X SRT
// listener accepts exactly one peer, never displaces the incumbent, and refuses
// re-accept for roughly five seconds. The wait only clears that window if our
// socket is already gone when it starts. See the doc comment on Pipeline in
// gst.go for the full argument.
//
// It is idempotent. Removing when no sink is installed is not an error — it
// still closes the gate and re-arms the queue, which is exactly what a caller
// entering DRAINING after a failed connect attempt needs.
func (p *cgoPipeline) RemoveSink() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return errors.New("gst: pipeline is stopped")
	}
	if !p.started {
		return errors.New("gst: pipeline has not been started")
	}
	return p.removeSinkLocked()
}

// removeSinkLocked is the ONE teardown path in this file. p.mu must be held.
//
// ReplaceSink is this followed by an install, rather than a second copy of the
// same three steps, so that there is a single place where the ordering of gate,
// unlink, NULL, remove and queue re-arm can be got right or wrong.
func (p *cgoPipeline) removeSinkLocked() error {
	// 1. Close the gate. Both probes now drop buffers, which isolates srtq from
	//    the sink about to be removed and isolates mpegtsmux from srtq.
	p.gateClosed.Store(true)

	// 2. Detach and destroy the sink, in the order unlink, NULL, remove.
	//    Removing an element that is not in NULL is what produces the
	//    "removing element in state PLAYING" warnings and leaks its resources.
	if err := p.detachSinkLocked(); err != nil {
		return err
	}

	// 3. Re-arm srtq if its loop was poisoned by the failure that got us here.
	//    A queue that took GST_FLOW_ERROR or GST_FLOW_NOT_LINKED from
	//    downstream stores it in srcresult, pauses its task, and thereafter
	//    returns that same error upstream from gst_queue_chain — so a sink swap
	//    alone would reconnect SRT and still deliver nothing.
	//
	//    Nothing upstream of srtq is touched. wasapi2src, both encoders and
	//    mpegtsmux stay in PLAYING, which is the single most important property
	//    of this file.
	p.rearmQueueLocked()
	return nil
}

// routeErr takes the diverted bus error for this swap, if one arrived. It never
// blocks.
func routeErr(route *sinkErrRoute) error {
	select {
	case err := <-route.ch:
		return err
	default:
		return nil
	}
}

// routeErrOr returns the diverted bus error, or fallback if none arrived. The
// bus error is always the more useful of the two — it carries libsrt's own
// diagnosis, which is what distinguishes ERROR:BADSECRET from ERROR:UNSECURE
// from a listener that already has its one permitted peer.
func routeErrOr(route *sinkErrRoute, fallback error) error {
	if err := routeErr(route); err != nil {
		return err
	}
	return fallback
}

// detachSinkLocked unlinks, stops and removes the currently installed sink, if
// there is one. p.mu must be held and the gate must already be closed.
func (p *cgoPipeline) detachSinkLocked() error {
	if p.sink == nil {
		return nil
	}
	sink := p.sink
	p.sink = nil

	if pad := sink.GetStaticPad("sink"); pad != nil {
		p.srtqSrcPad.Unlink(pad)
	}
	if ret := sink.BlockSetState(gogst.StateNull, gogst.ClockTime(elementShutdownTimeout)); !stateChangeOK(ret) {
		// Report it but carry on removing: an srtsink that will not go to NULL
		// is not a reason to abandon the reconnect, and leaving it in the
		// pipeline would be worse.
		log.Printf("gst: %s would not go to NULL (%s); removing it anyway", sink.GetName(), ret)
	}
	if !p.pipeline.Remove(sink) {
		return fmt.Errorf("gst: could not remove %s from the pipeline", sink.GetName())
	}
	return nil
}

// abandonSinkLocked tears down a sink that failed to connect. p.mu must be
// held. Errors here are logged rather than returned: the caller already has the
// error that matters, and losing it behind a cleanup failure would hide the
// reason the connection did not come up.
func (p *cgoPipeline) abandonSinkLocked(sink gogst.Element, sinkPad gogst.Pad) {
	if sinkPad != nil {
		p.srtqSrcPad.Unlink(sinkPad)
	}
	if ret := sink.BlockSetState(gogst.StateNull, gogst.ClockTime(elementShutdownTimeout)); !stateChangeOK(ret) {
		log.Printf("gst: %s would not go to NULL after a failed connect (%s)", sink.GetName(), ret)
	}
	if !p.pipeline.Remove(sink) {
		log.Printf("gst: could not remove %s after a failed connect", sink.GetName())
	}
}

// stickyEventTypes are the sticky events that must be present on srtq's src pad
// before a buffer can legally reach a freshly installed srtsink, in ascending
// GstEventType order.
//
// The first three are mandatory. STREAM_COLLECTION and TAG are carried because
// mpegtsmux may have sent them and losing them silently changes what is
// signalled downstream; neither is fatal by itself.
//
// gstpad.c's store_event() places each event at its own sticky index, so the
// order in this slice is documentation rather than mechanism.
var stickyEventTypes = []gogst.EventType{
	gogst.EventStreamStart,
	gogst.EventCaps,
	gogst.EventSegment,
	gogst.EventStreamCollection,
	gogst.EventTag,
}

// maxStickyEventsPerType bounds the per-type index scan in stickyEventsOf. Only
// TAG realistically has more than one; eight is generous and stops a malformed
// pad turning a reconnect into an unbounded loop.
const maxStickyEventsPerType = 8

// rearmQueueLocked restores srtq's loop if it stopped with a bad flow return,
// and puts back the sticky events that restoring it destroys.
//
// # Why a re-arm is needed at all
//
// gst_queue_loop stores the flow return of its last push in srcresult and
// pauses its task on anything other than GST_FLOW_OK. gst_queue_chain then
// returns that stored value to mpegtsmux on the next buffer. Deactivating and
// reactivating the src pad calls gst_queue_src_activate_mode, which resets
// srcresult to GST_FLOW_OK, flushes the stale queued data — which is what
// leaky=downstream would have done to it anyway — and restarts the loop task.
//
// # Why the re-arm has to be repaired afterwards
//
// gst_pad_set_active(pad, FALSE) reaches post_activate() with new_mode
// GST_PAD_MODE_NONE, and that branch calls remove_events(pad), which drops
// EVERY sticky event the pad holds. That is precisely the damage this function
// used to say it had rejected a FLUSH_START/FLUSH_STOP pair in order to avoid —
// and it is worse than the flush pair, which only removes SEGMENT, EOS and
// STREAM_GROUP_DONE and leaves STREAM_START and CAPS alone.
//
// Nothing puts them back on its own. mpegtsmux sends STREAM_START, CAPS and
// SEGMENT once, at the start of the stream; a queue forwards them once;
// gst_pad_link's events_foreach(mark_event_not_received) re-marks the src pad's
// sticky list for the new peer, but that list is now EMPTY, so it marks
// nothing. The fresh srtsink would then receive buffers with no segment, and
// gstbasesink would log "Received buffer without a new-segment. Assuming
// timestamps start from 0" — timestamps restarting at zero being the exact
// measured fault this entire file is built to prevent.
//
// This is not an exotic path. rearmQueueLocked runs whenever srtq:src's last
// flow return is not GST_FLOW_OK, and on a genuine mid-match disconnect the
// in-flight buffer is inside srtsink's render() when the gate closes, so the
// last flow return IS an error. Every real reconnect takes it.
//
// # The repair
//
// There is no re-arm in GStreamer that leaves the sticky events alone —
// deactivation clears all of them, a flush pair clears SEGMENT, and taking the
// queue element to READY does both plus more. So the events are snapshotted
// before the deactivation and stored back afterwards with
// gst_pad_store_sticky_event, which inserts them with received = FALSE and sets
// GST_PAD_FLAG_PENDING_EVENTS. check_sticky() then pushes them ahead of the
// first buffer the moment the gate opens, which is the ordinary delivery path
// and not a special case.
//
// The snapshot is taken from srtq's src pad, which is what the peer would
// actually have received. Anything missing there is taken from srtq's sink pad,
// whose own sticky list is untouched by any of this — the queue forwarded the
// same events from mpegtsmux, so it is the same data by a different route.
//
// UNVERIFIED, and a Gate B must-verify: see BUILD-NOTES.md section 4.10 for the
// gst-launch reproduction.
//
// It is a no-op on the healthy path, which is every first connect and every
// reconnect where the gate closed before the queue saw the failure.
func (p *cgoPipeline) rearmQueueLocked() {
	last := p.srtqSrcPad.GetLastFlowReturn()
	if last == gogst.FlowOK {
		return
	}
	log.Printf("gst: %s stopped with %s; re-arming its loop", nameSRTQueue, last)

	// Snapshot BEFORE the deactivation destroys them.
	saved := stickyEventsOf(p.srtqSrcPad)
	for _, ev := range stickyEventsOf(p.srtqSinkPad) {
		if _, have := saved[ev.key]; !have {
			saved[ev.key] = ev
		}
	}
	if _, have := saved[stickyKey{gogst.EventSegment, 0}]; !have {
		log.Printf("gst: WARNING: %s has no sticky SEGMENT event to preserve across the re-arm; "+
			"the next sink will be told timestamps start from zero", nameSRTQueue)
	}

	if !p.srtqSrcPad.SetActive(false) {
		log.Printf("gst: could not deactivate %s:src while re-arming", nameSRTQueue)
	}
	if !p.srtqSrcPad.SetActive(true) {
		log.Printf("gst: could not reactivate %s:src while re-arming; "+
			"media will not flow until the pipeline is rebuilt", nameSRTQueue)
	}

	restoreStickyEvents(p.srtqSrcPad, saved)
}

// stickyKey identifies one sticky event on a pad: its type and, for the types
// that permit several, its index.
type stickyKey struct {
	typ gogst.EventType
	idx uint
}

// stickyEvent is one snapshotted sticky event and the key it was found under.
type stickyEvent struct {
	key   stickyKey
	event *gogst.Event
}

// stickyEventsOf snapshots the sticky events of interest from a pad.
//
// It reads by type and index with gst_pad_get_sticky_event rather than
// iterating, because go-gst v0.0.2 does not bind gst_pad_sticky_events_foreach
// (it takes a C callback and is not in the generated surface). The set in
// stickyEventTypes is the complete set that matters here: srtq sits between one
// muxer and one sink and carries a single stream.
func stickyEventsOf(pad gogst.Pad) map[stickyKey]stickyEvent {
	out := make(map[stickyKey]stickyEvent, len(stickyEventTypes))
	if pad == nil {
		return out
	}
	for _, typ := range stickyEventTypes {
		for idx := uint(0); idx < maxStickyEventsPerType; idx++ {
			ev := pad.GetStickyEvent(typ, idx)
			if ev == nil {
				break
			}
			key := stickyKey{typ: typ, idx: idx}
			out[key] = stickyEvent{key: key, event: ev}
		}
	}
	return out
}

// restoreStickyEvents stores a snapshot back onto a pad whose sticky list was
// emptied by a deactivation.
//
// gst_pad_store_sticky_event inserts with received = FALSE, so check_sticky()
// pushes each one to the peer ahead of the next buffer. A failure is logged
// rather than returned: the caller is mid-reconnect and there is nothing better
// to do than continue and let the resulting bus error be the report.
func restoreStickyEvents(pad gogst.Pad, saved map[stickyKey]stickyEvent) {
	if pad == nil || len(saved) == 0 {
		return
	}
	restored := 0
	// Iterate stickyEventTypes rather than the map so the log line is stable
	// and the store order matches gstpad.c's own sticky ordering.
	for _, typ := range stickyEventTypes {
		for idx := uint(0); idx < maxStickyEventsPerType; idx++ {
			ev, ok := saved[stickyKey{typ: typ, idx: idx}]
			if !ok {
				continue
			}
			if ret := pad.StoreStickyEvent(ev.event); ret != gogst.FlowOK {
				log.Printf("gst: could not restore the sticky %s event on %s:src after re-arming (%s); "+
					"the next sink may see buffers with no segment", typ, nameSRTQueue, ret)
				continue
			}
			restored++
		}
	}
	log.Printf("gst: restored %d sticky event(s) on %s:src after re-arming", restored, nameSRTQueue)
}

// configureSRTSink applies every srtsink property from specification section 5.
//
// dialAddr is the IP LITERAL that goes into the URI. It is a separate parameter
// rather than being taken from opts.Host so that the two can never be confused:
// opts.Host is the operator's hostname and belongs only in logs and errors, and
// putting it in the URI aborts the process on teardown (see resolveSinkHost).
//
// The passphrase is set with g_object_set_property and is never placed in the
// URI. That is not stylistic: a URI is percent-encoded, is printed by
// GStreamer's own debug output, and appears in the element's error messages. A
// passphrase in the URI is a passphrase in the log.
func configureSRTSink(sink gogst.Element, opts SinkOpts, dialAddr string) error {
	// srtsink's own auto-reconnect must stay false. Reading gstsrtobject.c: on
	// a write failure it closes the socket, reopens it immediately with no
	// backoff, and retries once; if that single reopen fails it raises
	// GST_ELEMENT_ERROR(RESOURCE, WRITE) and the pipeline errors out — fired
	// straight into M2L-X's roughly five second re-accept refusal window, which
	// is exactly when it cannot succeed. internal/sender owns the loop instead.
	//
	// sync=false and async=false because this is a contribution encoder, not a
	// player: the sink must not throttle to the clock and must not require a
	// preroll buffer before the pipeline can reach PLAYING.
	for _, prop := range []struct {
		name  string
		value bool
	}{
		{"auto-reconnect", false},
		{"sync", false},
		{"async", false},
	} {
		if !hasProperty(sink, prop.name) {
			return fmt.Errorf("gst: srtsink has no %s property "+
				"(GStreamer is older than expected, or a different srtsink is being loaded)", prop.name)
		}
		sink.SetObjectProperty(prop.name, prop.value)
	}

	if err := setStringProperty(sink, "uri", srtURI(dialAddr, opts.Port)); err != nil {
		return err
	}

	// mode and pbkeylen are enum properties. gst_util_set_object_arg
	// deserialises the nick, which is the only way to set an enum belonging to
	// a plugin whose GType go-gst has no binding for.
	if !hasProperty(sink, "mode") {
		return errors.New("gst: srtsink has no mode property")
	}
	gogst.UtilSetObjectArg(sink, "mode", "caller")

	// latency is in MILLISECONDS. srtsink's property is milliseconds despite
	// what the original brief's example URI suggested; 120 ms is roughly five
	// times the measured 21 ms median RTT.
	if !hasProperty(sink, "latency") {
		return errors.New("gst: srtsink has no latency property")
	}
	gogst.UtilSetObjectArg(sink, "latency", strconv.Itoa(opts.LatencyMs))

	if opts.Passphrase != "" {
		if err := setStringProperty(sink, "passphrase", opts.Passphrase); err != nil {
			return err
		}
		if !hasProperty(sink, "pbkeylen") {
			return errors.New("gst: srtsink has no pbkeylen property")
		}
		gogst.UtilSetObjectArg(sink, "pbkeylen", strconv.Itoa(opts.PBKeyLen))
	}

	return nil
}

// resolveSinkHost turns SinkOpts.Host into the IP literals that may be put in
// an srtsink URI, most preferred first.
//
// # WHY THIS EXISTS. DO NOT "SIMPLIFY" IT AWAY.
//
// A HOSTNAME IN THE srtsink URI ABORTS THE PROCESS ON TEARDOWN. Measured twice
// at Gate C, on the FIRST RemoveSink of two separate runs against
// srt://m2lx-wslstudios-matcht.etapsiota.com:40022, and recorded in
// BUILD-NOTES.md section 8.5:
//
//	>>>> cycle 1: RemoveSink
//	0:00:17.656553500 DEBUG GST_PADS gstpad.c:1139:gst_pad_set_active:
//	    <srtout-1:sink> deactivating pad from push mode
//	GLib-GIO:ERROR:../gio/gthreadedresolver.c:1487:cancelled_cb:
//	    assertion failed: (g_cancellable_is_cancelled (cancellable))
//	Bail out! GLib-GIO:ERROR:../gio/gthreadedresolver.c:1487:cancelled_cb:
//	    assertion failed: (g_cancellable_is_cancelled (cancellable))
//
// A GLib assertion calls abort(). There is no recovering from it, no error on
// the bus, no Go error to return and nothing for internal/sender to retry: the
// application simply vanishes. gstsrtobject.c resolves a hostname through
// GResolver, which leaves a cancelled_cb on the GCancellable that srtsink
// cancels and resets around every open and close, and taking the sink to NULL
// while that lookup is still in flight trips the assertion inside GLib.
//
// The same test against the IP literal srt://34.242.91.248:40022 ran four full
// reconnect cycles and shut down cleanly: an IP literal needs no resolver and
// installs no handler. So the resolution is done HERE, in Go, and only a
// literal ever reaches srtsink.
//
// This is not a corner case. RemoveSink is called on entry to DRAINING on EVERY
// mid-match reconnect (internal/sender, specification section 6.2), and the
// operator types a hostname into the settings screen because a hostname is what
// M2L-X gives them.
//
// # Behaviour
//
//   - An IP literal — with or without IPv6 brackets — is returned untouched and
//     no lookup happens at all.
//   - Otherwise the name is looked up with a bounded context, and IPv4 addresses
//     are returned before IPv6 ones. An SRT listener is one host, but a name may
//     front several, so ReplaceSink tries them in order and says which answered.
//   - The lookup happens on EVERY ReplaceSink, never once at Start. M2L-X is in
//     AWS behind a name that can move, and a process that lives for a whole
//     match must not pin an address it resolved ninety minutes ago.
//
// The caller keeps opts.Host for every log line and every error message: an
// operator reading "connect failed" needs the name they typed, not an address
// they have never seen.
func resolveSinkHost(host string, timeout time.Duration) ([]string, error) {
	// An IPv6 literal may arrive bracketed, because that is how it is written
	// in a URI and srtURI accepts it that way.
	bare := host
	if strings.HasPrefix(bare, "[") && strings.HasSuffix(bare, "]") {
		bare = bare[1 : len(bare)-1]
	}
	if net.ParseIP(bare) != nil {
		return []string{bare}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("gst: could not resolve %q: %w", host, err)
	}

	// IPv4 first. srtsink's caller socket is created for the family of the
	// address it is given, and every measured M2L-X endpoint is v4; a v6 answer
	// that happens to sort first would otherwise be dialled on a host with no
	// v6 route and cost a connect timeout before the v4 address was reached.
	var v4, v6 []string
	for _, a := range addrs {
		if a.IP.To4() != nil {
			v4 = append(v4, a.IP.String())
			continue
		}
		if a.Zone != "" {
			// A link-local address with a zone cannot be written into a URI in
			// a form libsrt will parse. Skip it rather than hand srtsink
			// something it will reject.
			continue
		}
		v6 = append(v6, a.IP.String())
	}
	out := make([]string, 0, len(v4)+len(v6))
	out = append(out, v4...)
	out = append(out, v6...)
	if len(out) == 0 {
		return nil, fmt.Errorf("gst: %q resolved to no usable address", host)
	}
	return out, nil
}

// srtURI builds the srtsink URI. It carries the endpoint and nothing else:
// every other setting, and in particular the passphrase, is a property.
//
// The host given to it is ALWAYS an IP literal on the production path —
// ReplaceSink resolves SinkOpts.Host with resolveSinkHost first, because a
// hostname here aborts the process on teardown. See resolveSinkHost.
//
// An IPv6 literal is bracketed. M2L-X is reached by hostname in practice, but a
// facility engineer typing an address into the settings screen should not
// discover that the field only accepts IPv4.
func srtURI(host string, port int) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return "srt://" + host + ":" + strconv.Itoa(port)
}

// endpointForLog renders the SRT endpoint for a log line or an error message.
// It exists so that no code path is tempted to format opts as a whole, which
// would put the passphrase in the log.
//
// It uses opts.Host — the name the operator typed — and never the resolved
// address. See dialledEndpointForLog for the one that says both.
func endpointForLog(opts SinkOpts) string {
	return srtURI(opts.Host, opts.Port)
}

// dialledEndpointForLog names the endpoint the operator configured AND the
// address actually being dialled, when the two differ.
//
// Both halves matter. The name is what the operator recognises and what they
// will check against the M2L-X page in front of them; the address is what
// actually went into the URI, which is the only way to tell "the name resolves
// to something that is not listening" from "the name does not resolve" and the
// only way to say which of several A records answered.
func dialledEndpointForLog(opts SinkOpts, addr string) string {
	endpoint := endpointForLog(opts)
	if addr == "" || strings.Contains(endpoint, "//"+addr+":") {
		return endpoint
	}
	return endpoint + " (address " + addr + ")"
}

// encryptionForLog says whether the session is encrypted and at what key
// length, without saying anything about the passphrase itself.
func encryptionForLog(opts SinkOpts) string {
	if opts.Passphrase == "" {
		return "none"
	}
	return "aes-" + strconv.Itoa(opts.PBKeyLen*8)
}

// ForceKeyUnit sends a GstForceKeyUnit event upstream so that the encoder emits
// an IDR immediately.
//
// It is called after a successful ReplaceSink so the picture recovers at once
// instead of waiting up to two seconds for the next scheduled IDR — gop-size is
// 100 frames at 50p.
//
// The event goes to the encoder element rather than to the pipeline.
// gst_element_send_event's default implementation routes an upstream event to a
// random src pad of the element it is given, so sending it to the encoder puts
// it straight into GstVideoEncoder's src-pad event handler, which is what
// actually forces the keyframe. Sending it to the pipeline would route it to a
// sink and hope mpegtsmux forwards it to the video branch.
func (p *cgoPipeline) ForceKeyUnit() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return errors.New("gst: pipeline is stopped")
	}
	if !p.started || p.encoder == nil {
		return errors.New("gst: pipeline has not been started")
	}

	event := newForceKeyUnitEvent()
	if event == nil {
		return errors.New("gst: could not build a GstForceKeyUnit event")
	}
	if !p.encoder.SendEvent(event) {
		return fmt.Errorf("gst: %s refused the GstForceKeyUnit event", p.encoderName)
	}
	return nil
}

// newForceKeyUnitEvent builds the upstream force-key-unit event by hand.
//
// This is gst_video_event_new_upstream_force_key_unit reimplemented over
// GstStructure, so that this package links only gstreamer-1.0 and gobject-2.0
// and does not add gstreamer-video-1.0 to the pkg-config requirements of a
// build nobody has performed yet. The structure name and field names are
// GStreamer's, from gstvideoutils / gstvideoevent: a custom-upstream event
// carrying a structure named GstForceKeyUnit, with all-headers and count.
//
// running-time is omitted. gst_video_event_parse_upstream_force_key_unit
// substitutes GST_CLOCK_TIME_NONE when the field is absent, which means "as
// soon as possible" and is exactly what is wanted after a reconnect.
//
// all-headers is true so that SPS and PPS accompany the forced IDR. h264parse
// already has config-interval=-1, so this is belt and braces; it costs a few
// dozen bytes once per reconnect.
func newForceKeyUnitEvent() *gogst.Event {
	s := gogst.NewStructureEmpty("GstForceKeyUnit")
	if s == nil {
		return nil
	}
	s.SetValue("all-headers", true)
	// uint32 rather than uint: go-glib's gobject.NewValue panics on a bare Go
	// uint or int because their size is platform dependent.
	s.SetValue("count", uint32(0))
	return gogst.NewEventCustom(gogst.EventCustomUpstream, s)
}

// Errors returns the pipeline's asynchronous error channel.
//
// It carries GST_ELEMENT_ERROR messages from the bus — in practice srtout
// losing its peer. Synchronous failures are returned by the method that caused
// them and do not appear here. The channel is closed by Stop, and sends into it
// are dropped rather than blocked when it is full.
func (p *cgoPipeline) Errors() <-chan error {
	return p.errs
}

// Stop takes the pipeline to NULL, releases the capture device and closes the
// channel returned by Errors. It is idempotent.
func (p *cgoPipeline) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return nil
	}
	p.stopped = true
	p.gateClosed.Store(true)
	p.route.Store(nil)

	// Stop the signal watchdog BEFORE the pipeline goes to NULL. Its reader
	// closure holds the capture element, and a property read against a disposed
	// element is a read on freed memory. Stop JOINS, so no report can arrive after
	// this line — a lamp coming back on for a pipeline that no longer exists is
	// the direction this project never lets a status display be wrong in.
	//
	// Joining under p.mu is safe: the watchdog goroutine never takes p.mu, and its
	// emit path is the application's, which the interface contract requires not to
	// block. Nil-safe, so there is no guard here.
	p.sigWatch.Stop()
	p.sigWatch = nil

	err := p.teardownLocked()

	// Close the channels last, after teardownLocked has silenced the bus
	// handler and the pipeline has reached NULL. errMu is taken for writing
	// here; a streaming thread inside deliver or deliverWarning holds it for
	// reading only for the duration of a non-blocking channel send, so this
	// cannot wait long.
	//
	// It does not deadlock against a message posted by the state change on this
	// same goroutine either: errMu is NOT held during teardownLocked, so a
	// re-entrant deliver would take it for reading and return. (An earlier
	// comment here claimed the opposite and used it to justify detaching the
	// bus sync handler. That justification was wrong; see teardownLocked.)
	//
	// errsClosed guards both channels: nothing sends on either without holding
	// errMu for reading and checking it first.
	p.errMu.Lock()
	if !p.errsClosed {
		p.errsClosed = true
		close(p.errs)
		close(p.warns)
	}
	p.errMu.Unlock()

	return err
}

// teardownLocked silences the bus handler, removes the pad probes and takes the
// pipeline to NULL. p.mu must be held. It is safe to call on a partially built
// pipeline, which is how Start unwinds a failure.
//
// # Why the bus sync handler is silenced with a flag and never detached
//
// gst_bus_set_sync_handler(bus, NULL, ...) would look tidier and would kill the
// process. gst_bus_post reads the handler and its user_data under
// GST_OBJECT_LOCK, drops the lock, and only then calls it. Setting the handler
// to NULL runs the GDestroyNotify go-gst installed alongside it, which calls
// userdata.Delete and unregisters the Go closure. A streaming thread that had
// already read the pointer then enters go-gst's exported callback
// _gogst_gst1_BusSyncHandler, finds userdata.Load returns nil, and executes
//
//	panic(`callback not found`)
//
// in an //export'ed cgo function. A Go panic cannot unwind through a C frame,
// so the process dies. Worse, go-glib's userdata allocator recycles the freed C
// pointer onto a free list, so a later registration can hand the same address
// out again and the late call reaches somebody else's closure instead.
//
// The window is narrow, but it is at Stop — the end of every match, and every
// mid-match capture-device change — and the failure is process death during a
// live broadcast.
//
// So the handler stays attached for the life of the bus and onBusMessage
// returns immediately once busSilenced is set. The cost is one userdata
// registration per pipeline, released when the GstBus is finalised. The
// deadlock this detachment was said to prevent does not exist: Stop does not
// hold errMu while teardownLocked runs, and errsClosed already makes a late
// delivery safe.
func (p *cgoPipeline) teardownLocked() error {
	p.busSilenced.Store(true)

	// gst_pad_remove_probe waits for a running callback to return. gateProbe
	// never blocks, so this cannot wait meaningfully, and removing the probes
	// before the state change keeps callbacks out of the teardown entirely.
	if p.srtqSrcPad != nil && p.srcProbeID != 0 {
		p.srtqSrcPad.RemoveProbe(p.srcProbeID)
		p.srcProbeID = 0
	}
	if p.srtqSinkPad != nil && p.sinkProbeID != 0 {
		p.srtqSinkPad.RemoveProbe(p.sinkProbeID)
		p.sinkProbeID = 0
	}
	if p.srtqSinkPad != nil && p.sinkEventProbeID != 0 {
		p.srtqSinkPad.RemoveProbe(p.sinkEventProbeID)
		p.sinkEventProbeID = 0
	}

	var err error
	if p.pipeline != nil {
		// The whole pipeline goes to NULL in one call; there is no need to take
		// the sink down separately, and doing so would only add a way to fail.
		stopWatchdog := stateChangeWatchdog("pipeline to NULL (releasing the WASAPI endpoint)")
		ret := p.pipeline.BlockSetState(gogst.StateNull, gogst.ClockTime(pipelineStartTimeout))
		stopWatchdog()
		if !stateChangeOK(ret) {
			err = fmt.Errorf("gst: pipeline would not go to NULL (%s)", ret)
		}
	}

	// Drop the element references so their finalizers can unref. The pipeline
	// reference is dropped last because everything else is one of its children.
	p.sink = nil
	p.encoder = nil
	p.srtq = nil
	p.srtqSrcPad = nil
	p.srtqSinkPad = nil
	p.aconv = nil
	p.aconvSinkPad = nil
	// matrixWidth goes back to zero with the pad it described. Leaving it set
	// would make SetChannelMap on a torn-down pipeline get as far as reading a
	// nil pad rather than being refused by the started/stopped guards above it,
	// which is a longer path to the same answer through more nil checks.
	p.matrixWidth = 0
	p.bus = nil
	p.clock = nil
	p.pipeline = nil

	return err
}

// stateChangeOK reports whether a GstStateChangeReturn means the state change
// completed.
//
// NO_PREROLL counts as success: it is what a pipeline containing a live source
// returns, and both wasapi2src and imagefreeze is-live=true are live sources,
// so it is the expected answer here rather than an edge case. ASYNC does not
// count, because every caller in this file has already waited for the change
// with a timeout — an ASYNC at that point means the timeout expired.
func stateChangeOK(ret gogst.StateChangeReturn) bool {
	return ret == gogst.StateChangeSuccess || ret == gogst.StateChangeNoPreroll
}

// stateChangeWatchdog logs, every watchdogInterval, that a synchronous
// GStreamer state change has still not returned. The returned function stops
// it and must be called on every path out, including the failure paths.
//
// IT CANNOT RECOVER ANYTHING, and it is not a timeout. Read the comment on the
// timeout constants at the top of this file: gst_element_set_state takes no
// timeout, holds the element's GST_STATE_LOCK, and cannot be cancelled or
// preempted from Go. If wasapi2src wedges inside IAudioClient::Initialize on a
// broken endpoint, the calling goroutine is gone for the life of the process
// and it is holding p.mu.
//
// What this buys is the difference between "the application froze" and a log
// with a line in it naming the element and the transition. That is the whole
// claim; nothing here promises a bound.
//
// It runs on its own timer goroutine, so it does not call log.Printf from a
// streaming thread, and it does not touch the pipeline.
func stateChangeWatchdog(what string) (stop func()) {
	var (
		mu      sync.Mutex
		stopped bool
		timer   *time.Timer
		started = time.Now()
	)
	var arm func()
	arm = func() {
		timer = time.AfterFunc(watchdogInterval, func() {
			mu.Lock()
			defer mu.Unlock()
			if stopped {
				return
			}
			log.Printf("gst: WATCHDOG: %s has not returned after %s. "+
				"gst_element_set_state cannot be interrupted; if this repeats, the audio driver "+
				"or an encoder MFT is wedged and only restarting the application will clear it.",
				what, time.Since(started).Round(time.Second))
			arm()
		})
	}
	mu.Lock()
	arm()
	mu.Unlock()

	return func() {
		mu.Lock()
		defer mu.Unlock()
		stopped = true
		if timer != nil {
			timer.Stop()
		}
	}
}

// propertyType returns the GType of an installed property, or TypeInvalid if
// the object has no property of that name.
//
// go-glib exposes PropertyType on gobject.ObjectInstance, which every gst
// element embeds, but does not list it in the gobject.Object interface — hence
// the type assertion. If this ever fails at Gate B, the assertion is the thing
// to look at, not the callers.
func propertyType(obj gobject.Object, name string) gobject.Type {
	pt, ok := obj.(interface {
		PropertyType(string) gobject.Type
	})
	if !ok {
		return gobject.TypeInvalid
	}
	return pt.PropertyType(name)
}

// hasProperty reports whether a GObject has an installed property of this name.
func hasProperty(obj gobject.Object, name string) bool {
	return propertyType(obj, name) != gobject.TypeInvalid
}

// setTypedProperty sets a property after checking BOTH that it exists and that
// it has the GType being handed to it.
//
// # The type half of that check is not defensive programming, it is a measured bug
//
// The original setter checked existence only, and that was enough for as long
// as the product was Windows-only, because every property this package sets by
// name happens to be a string there. It is not enough on macOS.
// osxaudiosink's "device" property is a gint CoreAudio AudioDeviceID, not a
// string, and the existence-only guard passes it: g_object_set_property emits a
// GLib CRITICAL to stderr where nobody is looking, the setter returns nil, the
// property keeps its default of 0, and CoreAudio helpfully interprets 0 as "the
// system default device". The return-audio monitor then plays out of the wrong
// device with a GREEN lamp and no diagnosis anywhere. osxaudiosrc's "device" is
// the same gint on the capture side.
//
// Comparing the GType turns that entire class of failure — every future
// property whose type differs between the platforms' elements — into a loud
// error naming the property and the type it actually wanted. It costs one
// comparison at pipeline-open time.
func setTypedProperty(obj gobject.Object, name string, want gobject.Type, value any) error {
	got := propertyType(obj, name)
	if got == gobject.TypeInvalid {
		return fmt.Errorf("gst: element has no %s property", name)
	}
	if got != want {
		return fmt.Errorf("gst: element's %s property is a %s, not a %s: setting it would emit a "+
			"GLib CRITICAL and leave the property at its default, which is how a pipeline ends up "+
			"quietly using the wrong device", name, got.Name(), want.Name())
	}
	obj.SetObjectProperty(name, value)
	return nil
}

// setStringProperty sets a G_TYPE_STRING property, failing loudly if the
// element does not have it or if it is not a string.
//
// The explicit check exists because the alternatives are silent:
// g_object_set_property on an unknown property emits a GLib warning to stderr
// and carries on, and gst_util_set_object_arg simply returns. A pipeline that
// starts with no slate path or no capture device set is a pipeline that sends
// nothing while looking healthy.
func setStringProperty(obj gobject.Object, name, value string) error {
	return setTypedProperty(obj, name, gobject.TypeString, value)
}

// setIntProperty sets a G_TYPE_INT property, failing loudly if the element does
// not have it or if it is not a gint.
//
// int32 rather than int in the signature is not incidental: go-glib maps a Go
// int32 to G_TYPE_INT and has no mapping at all for a plain int, so passing one
// through SetObjectProperty would arrive as G_TYPE_INVALID. The one thing a
// caller must not do is pass a value it has not resolved from a stable
// identifier first — see deviceprovider_darwin.go for why every gint device id
// on macOS is a runtime handle rather than an identity.
func setIntProperty(obj gobject.Object, name string, value int32) error {
	return setTypedProperty(obj, name, gobject.TypeInt, value)
}
