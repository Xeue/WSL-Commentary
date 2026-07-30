//go:build cgo

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
//  5. Go's garbage collector, via go-gst's finalizers. Every GStreamer object
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
	"errors"
	"fmt"
	"log"
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

// Timeouts. All of these bound a synchronous GStreamer state change so that a
// hung element surfaces as an error rather than as a wedged application. They
// are upper bounds on a hang, not expected durations.
const (
	// pipelineStartTimeout bounds the NULL→PLAYING transition of the capture
	// and encode chain. The costly parts are opening the WASAPI endpoint in
	// shared mode and initialising the Media Foundation encoder MFTs. Ten
	// seconds is far beyond either; it exists so that a wedged audio driver
	// fails visibly at Start instead of hanging the UI thread that called it.
	pipelineStartTimeout = 10 * time.Second

	// sinkStateChangeTimeout bounds the NULL→PLAYING transition of a fresh
	// srtsink, which is where the SRT caller handshake happens. libsrt's
	// SRTO_CONNTIMEO defaults to 3 s and srtsink does not override it, so a
	// refused or unreachable listener reports itself well inside this. The
	// specification measures a successful lock at about 1.1 s. Ten seconds only
	// fires if the state change itself hangs.
	sinkStateChangeTimeout = 10 * time.Second

	// elementShutdownTimeout bounds taking a single element to NULL. Setting a
	// disconnected srtsink to NULL closes a socket; it does not block.
	elementShutdownTimeout = 5 * time.Second
)

// errorChannelBuffer is how many asynchronous errors are held before further
// ones are dropped. It matches stubErrorBuffer in gst_stub.go so that the two
// implementations behave identically under a slow consumer. The contract in
// gst.go requires dropping rather than blocking: the sender of these errors is
// a GStreamer streaming thread and it must never wait on a Go consumer.
const errorChannelBuffer = 16

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
	nameMux        = "mux"   // mpegtsmux
	nameSRTQueue   = "srtq"  // the leaky queue whose src pad feeds the sink
	nameSlateSrc   = "slate" // filesrc reading the slate PNG
	nameAudioSrc   = "asrc"  // wasapi2src
	nameVideoEncod = "venc"  // the H.264 encoder chosen at runtime
)

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

// requiredElements is the set of element factories the specification's pipeline
// cannot be built without, mapped to the plugin from the allowlist that
// provides each. Init checks all of them and reports every one that is missing.
//
// This check is the difference between "the app will not start, and here is the
// plugin that is missing from the bundle" and "the app starts, the user presses
// Start twenty minutes before kick-off, and gst_parse_launch says no such
// element". The H.264 encoder is deliberately absent from this list because it
// is resolved by rank at runtime (specification open question 3).
var requiredElements = []struct{ factory, plugin string }{
	{"filesrc", "coreelements"},
	{"queue", "coreelements"},
	{"capsfilter", "coreelements"},
	{"pngdec", "png"},
	{"imagefreeze", "imagefreeze"},
	{"videoconvert", "videoconvertscale"},
	{"audioconvert", "audioconvert"},
	{"audioresample", "audioresample"},
	{"h264parse", "videoparsersbad"},
	{"aacparse", "audioparsers"},
	{"wasapi2src", "wasapi2"},
	{"mfaacenc", "mediafoundation"},
	{"mpegtsmux", "mpegtsmux"},
	{"srtsink", "srt"},
}

// Init prepares the bundled GStreamer for use and calls gst_init.
//
// appDir is the directory holding wslcomms.exe. Before gst_init, Init sets:
//
//	GST_PLUGIN_SYSTEM_PATH_1_0 = ""                              (disables the default search)
//	GST_PLUGIN_PATH_1_0        = <appDir>\gst\lib\gstreamer-1.0
//	GST_REGISTRY_1_0           = %LOCALAPPDATA%\WSLComms\registry.bin
//
// Go's os.Setenv calls SetEnvironmentVariableW, which is what GLib reads, so
// this crosses the cgo boundary cleanly. The effect is that any GStreamer
// installed elsewhere on the machine is invisible to this process, and this
// process's bundle is invisible to it. Getting this wrong does not fail loudly:
// the app silently loads some other GStreamer, which is why Init verifies the
// bundle afterwards by looking up every element the pipeline needs.
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

	// An empty GST_PLUGIN_SYSTEM_PATH_1_0 is not the same as an unset one:
	// unset means "search the compiled-in system directories", empty means
	// "search nothing". os.Setenv with "" sets it to empty, which is what is
	// wanted. Do not switch this to os.Unsetenv.
	if err := os.Setenv("GST_PLUGIN_SYSTEM_PATH_1_0", ""); err != nil {
		return fmt.Errorf("gst: Init: setting GST_PLUGIN_SYSTEM_PATH_1_0: %w", err)
	}

	pluginDir := filepath.Join(abs, "gst", "lib", "gstreamer-1.0")
	if err := os.Setenv("GST_PLUGIN_PATH_1_0", pluginDir); err != nil {
		return fmt.Errorf("gst: Init: setting GST_PLUGIN_PATH_1_0: %w", err)
	}
	if fi, err := os.Stat(pluginDir); err != nil {
		return fmt.Errorf("gst: Init: bundled plugin directory %q is not readable: %w", pluginDir, err)
	} else if !fi.IsDir() {
		return fmt.Errorf("gst: Init: bundled plugin path %q is not a directory", pluginDir)
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

	gogst.Init()

	if missing := missingElements(); len(missing) > 0 {
		return fmt.Errorf(
			"gst: Init: the bundled GStreamer in %q is incomplete: %s "+
				"(check the DLL allowlist and that the registry at %q was rebuilt)",
			pluginDir, strings.Join(missing, ", "), registryPath)
	}

	log.Printf("gst: initialised, plugins from %q, registry %q", pluginDir, registryPath)
	return nil
}

// registryFile returns %LOCALAPPDATA%\WSLComms\registry.bin.
//
// The registry is a cache of the plugin scan and must be per-user and writable;
// putting it next to the executable would fail under a per-machine install
// running as a standard user, and sharing the default location would let a
// system-wide GStreamer's registry and ours overwrite each other.
func registryFile() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		// os.UserCacheDir returns %LocalAppData% on Windows, so this is the
		// same directory by a different route rather than a different policy.
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
	var missing []string
	for _, req := range requiredElements {
		if gogst.ElementFactoryFind(req.factory) == nil {
			missing = append(missing, fmt.Sprintf("%s (plugin %s)", req.factory, req.plugin))
		}
	}
	return missing
}

// ListInputDevices returns the audio capture endpoints offered in the
// commentary input dropdown.
//
// It runs a GstDeviceMonitor filtered to Audio/Source, keeps only the devices
// whose provider reports device.api = "wasapi2", and for each one reports
// display-name as Device.Name and the IMMDevice endpoint ID as Device.ID.
//
// The filter is applied after enumeration rather than before because
// GstDeviceMonitor filters by device class and caps only — it has no API for
// restricting the providers it uses. Devices from any other provider that might
// end up in the bundle are discarded rather than offered, because Device.ID is
// passed verbatim to wasapi2src and only wasapi2's IDs are meaningful there.
func ListInputDevices() ([]Device, error) {
	if !inited.Load() {
		return nil, errors.New("gst: ListInputDevices: Init has not been called")
	}

	monitor := gogst.NewDeviceMonitor()
	if monitor == nil {
		return nil, errors.New("gst: ListInputDevices: gst_device_monitor_new returned nil")
	}

	// A nil caps filter means "any caps of this class". Audio/Source is the
	// class every audio capture provider advertises. A zero return means the
	// filter was not installed, which matters: without it the monitor also
	// reports Audio/Sink devices, and headphones would appear in the commentary
	// input dropdown. Every device is re-checked below in any case.
	if monitor.AddFilter("Audio/Source", nil) == 0 {
		log.Printf("gst: ListInputDevices: gst_device_monitor_add_filter(Audio/Source) returned 0; " +
			"relying on the per-device class check")
	}

	// Starting the monitor makes the providers probe and keeps them hot for
	// hot-plug messages. gst_device_monitor_get_devices also works on a stopped
	// monitor by doing a one-shot probe, so a failure to start is not fatal —
	// it just means no hot-plug bus, which this function does not use anyway.
	started := monitor.Start()
	if !started {
		log.Printf("gst: ListInputDevices: gst_device_monitor_start failed; falling back to a one-shot probe")
	}
	devices := monitor.GetDevices()
	if started {
		monitor.Stop()
	}

	out := make([]Device, 0, len(devices))
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
		props := dev.GetProperties()
		if props == nil {
			// gst_device_get_properties is nullable. Without properties the
			// provider cannot be identified, and Device.ID is only meaningful
			// for wasapi2, so this device cannot safely be offered.
			log.Printf("gst: ListInputDevices: skipping %q: it publishes no device properties",
				dev.GetDisplayName())
			continue
		}
		if api := props.GetString("device.api"); api != "wasapi2" {
			continue
		}
		id := endpointID(dev, props)
		if id == "" {
			// A wasapi2 device with no recoverable endpoint ID cannot be
			// persisted or passed to wasapi2src, so offering it in the dropdown
			// would produce a selection that silently fails at Start. Log the
			// property names available so that Gate B has something to work
			// with if the provider's key ever changes.
			log.Printf("gst: ListInputDevices: skipping %q: no endpoint ID; device properties are %s",
				dev.GetDisplayName(), structureFieldNames(props))
			continue
		}
		out = append(out, Device{ID: id, Name: dev.GetDisplayName()})
	}
	return out, nil
}

// endpointIDKeys are the GstStructure keys under which a wasapi2 device might
// publish its IMMDevice endpoint ID, most likely first.
//
// UNVERIFIED: gstwasapi2device.c is believed to publish it as "device.id", but
// that has not been read against 1.28.5 on the target machine and no GStreamer
// installation was available to check. The fallback below does not depend on
// any of these names being right.
var endpointIDKeys = []string{"device.id", "device.strid", "device.path"}

// endpointID extracts the IMMDevice endpoint ID GUID for one device.
//
// It tries the property keys above first because that is cheap. If none of them
// is present it falls back to gst_device_create_element, which returns a
// wasapi2src with its device property already set to whatever the provider
// considers the device's identity. That fallback is authoritative by
// construction — it is literally the value wasapi2src will be given — and it is
// what makes this function robust against the property key having been renamed.
// It costs one element instantiation per device; no audio endpoint is opened,
// because the element stays in NULL.
func endpointID(dev gogst.Device, props *gogst.Structure) string {
	for _, key := range endpointIDKeys {
		if props.HasField(key) {
			if v := props.GetString(key); v != "" {
				return v
			}
		}
	}

	el := dev.CreateElement("")
	if el == nil {
		return ""
	}
	if !hasProperty(el, "device") {
		return ""
	}
	v, ok := el.ObjectProperty("device").(string)
	if !ok {
		return ""
	}
	return v
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
var h264EncoderDenylist = map[string]bool{
	"x264enc": true,
}

// h264EncoderPreference breaks ties between factories of equal rank, lower
// index winning. It is a tie-break only: a higher-ranked encoder always beats a
// preferred one, which is what "resolve by rank at runtime" in specification
// open question 3 asks for. mfh264enc is preferred at equal rank because it is
// the element the specification's property set was written against and the one
// the 2 s GOP profile was measured on.
var h264EncoderPreference = []string{
	"mfh264enc",
	"qsvh264enc",
	"nvh264enc",
	"d3d11h264enc",
	"amfh264enc",
}

// h264EncoderFallbacks are tried by name, in this order, if the factory-list
// query returns nothing usable. This is belt and braces against
// factoryTypeMediaVideo being wrong.
var h264EncoderFallbacks = []string{
	"mfh264enc",
	"qsvh264enc",
	"nvh264enc",
	"d3d11h264enc",
	"amfh264enc",
	"openh264enc",
}

// h264EncoderProps are the specification section 5 encoder settings other than
// bitrate, as strings for gst_util_set_object_arg.
//
// They are applied one at a time, and only to properties the chosen factory
// actually has, because the encoder is resolved by rank and a different vendor's
// element will not have mfh264enc's property names. Strings rather than typed
// values because gst_util_set_object_arg deserialises into whatever GType the
// property is — enum nick, gint or guint — and encoders disagree about which of
// those they use even for identically named properties.
//
// Why these values (specification section 5): rc-mode=cbr rather than
// quality-targeted, because a static slate under QVBR collapses to 200-350 kbps
// which is cheaper but bursty at every IDR and makes "is it flowing" harder to
// observe. gop-size=100 is a 2 s GOP at 50p, matching the profile M2L-X locked
// cleanly. bframes=0 and low-latency=true because there is nothing to gain from
// reordering a slate.
var h264EncoderProps = []struct{ name, value string }{
	{"rc-mode", "cbr"},
	{"gop-size", "100"},
	{"bframes", "0"},
	{"low-latency", "true"},
	{"cabac", "true"},
}

// selectH264Encoder returns the factory name of the highest-ranked H.264
// encoder present in the registry.
//
// Specification open question 3: "Is the highest-ranked H.264 encoder called
// mfh264enc on the target machine? Resolve the element by rank at runtime
// rather than hardcoding the name." This does that, and logs what it chose so
// that the answer is in the field log rather than in someone's memory.
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
		if bestName == "" || rank > bestRank || (rank == bestRank && pref < bestPref) {
			bestName, bestRank, bestPref = name, rank, pref
		}
	}

	if bestName != "" {
		log.Printf("gst: H.264 encoder resolved by rank: %s (rank %d), from %d candidate video encoders",
			bestName, bestRank, len(factories))
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
func New() (Pipeline, error) {
	if !inited.Load() {
		return nil, errors.New("gst: New: Init has not been called")
	}
	return &cgoPipeline{
		errs: make(chan error, errorChannelBuffer),
	}, nil
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

	// encoder is the H.264 encoder element, kept for ForceKeyUnit.
	encoder gogst.Element

	// encoderName is the factory name chosen by selectH264Encoder.
	encoderName string

	// srtq is the leaky queue in front of the sink, and its two pads.
	srtq        gogst.Element
	srtqSrcPad  gogst.Pad
	srtqSinkPad gogst.Pad

	// gate probe ids, for removal at Stop.
	srcProbeID  uint32
	sinkProbeID uint32

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

	// errs carries GST_ELEMENT_ERROR bus messages to Errors. Guarded by errMu,
	// which exists only so that a streaming thread cannot send on a channel
	// Stop is closing. errMu is never held across anything that can block.
	errMu      sync.RWMutex
	errs       chan error
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
	if opts.AudioDeviceID == "" {
		return errors.New("gst: PipelineOpts.AudioDeviceID is required")
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

	encoderName, err := selectH264Encoder()
	if err != nil {
		return err
	}
	p.encoderName = encoderName

	desc := pipelineDescription(encoderName, opts.AudioBitrateBps)
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

	// The slate path and the device GUID are set with g_object_set rather than
	// placed in the parse string. gst_parse_launch's quoting rules treat a
	// backslash as an escape inside double quotes, so a Windows path would be
	// mangled, and the endpoint GUID's braces and dots are similarly at the
	// mercy of the parser. Neither value ever reaches the parser.
	slate := pipeline.GetByName(nameSlateSrc)
	if slate == nil {
		return abort(errors.New("gst: parsed pipeline has no element named " + nameSlateSrc))
	}
	if err := setStringProperty(slate, "location", opts.SlatePath); err != nil {
		return abort(err)
	}

	asrc := pipeline.GetByName(nameAudioSrc)
	if asrc == nil {
		return abort(errors.New("gst: parsed pipeline has no element named " + nameAudioSrc))
	}
	if err := setStringProperty(asrc, "device", opts.AudioDeviceID); err != nil {
		return abort(err)
	}
	// low-latency is a nice-to-have on wasapi2src, not a requirement. If a
	// future GStreamer renames it, running at the default latency is better
	// than refusing to start a commentary position.
	if hasProperty(asrc, "low-latency") {
		asrc.SetObjectProperty("low-latency", true)
	} else {
		log.Printf("gst: %s has no low-latency property; using the default capture latency", nameAudioSrc)
	}

	p.encoder = pipeline.GetByName(nameVideoEncod)
	if p.encoder == nil {
		return abort(errors.New("gst: parsed pipeline has no element named " + nameVideoEncod))
	}
	applyEncoderProperties(p.encoder, encoderName, opts.VideoBitrateKbps)

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

	// The bus sync handler is attached before the first state change so that an
	// error raised during NULL→PLAYING is captured rather than lost. It is a
	// sync handler rather than a watch because a watch needs a GLib main loop
	// and this process does not have one — Wails owns the Windows message loop.
	p.bus = pipeline.GetBus()
	if p.bus == nil {
		return abort(errors.New("gst: pipeline has no bus"))
	}
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

	if ret := pipeline.BlockSetState(gogst.StatePlaying, gogst.ClockTime(pipelineStartTimeout)); !stateChangeOK(ret) {
		err := fmt.Errorf("gst: pipeline would not go to PLAYING (%s)", ret)
		if busErr := p.drainStartupError(); busErr != nil {
			err = fmt.Errorf("%w: %v", err, busErr)
		}
		return abort(err)
	}

	p.started = true
	log.Printf("gst: pipeline PLAYING with no sink; base time %d ns, encoder %s", uint64(base), encoderName)
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
// path and the audio device GUID are absent because they are user-supplied
// strings and the parser's escaping rules are not something to trust a Windows
// path or a GUID to; they are set with g_object_set afterwards.
//
// encoderName is the factory resolved by rank. audioBitrateBps is mfaacenc's
// bitrate property, in bits per second.
func pipelineDescription(encoderName string, audioBitrateBps int) string {
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
	return "" +
		"mpegtsmux name=" + nameMux + " alignment=7 pcr-interval=3600" +
		" ! queue name=" + nameSRTQueue + " leaky=downstream max-size-buffers=4000\n" +

		"filesrc name=" + nameSlateSrc +
		" ! pngdec" +
		" ! imagefreeze is-live=true" +
		" ! videoconvert" +
		" ! video/x-raw,format=NV12,width=1920,height=1080,framerate=50/1," +
		"pixel-aspect-ratio=1/1,colorimetry=bt709" +
		" ! " + encoderName + " name=" + nameVideoEncod +
		" ! video/x-h264,profile=high" +
		" ! h264parse config-interval=-1" +
		" ! video/x-h264,stream-format=byte-stream,alignment=au" +
		" ! queue name=vq max-size-time=1000000000 ! " + nameMux + ".\n" +

		"wasapi2src name=" + nameAudioSrc +
		" ! audio/x-raw,rate=48000,channels=2" +
		" ! audioconvert ! audioresample" +
		" ! audio/x-raw,format=S16LE,rate=48000,channels=2,layout=interleaved" +
		" ! mfaacenc bitrate=" + strconv.Itoa(audioBitrateBps) +
		" ! aacparse ! audio/mpeg,mpegversion=4,stream-format=adts" +
		" ! queue name=aq max-size-time=1000000000 ! " + nameMux + "."
}

// applyEncoderProperties sets the specification's encoder settings on whichever
// H.264 encoder was chosen, skipping any property that encoder does not have.
//
// A missing property is logged, not fatal. The encoder is resolved by rank, so
// on a machine where the top-ranked encoder is not mfh264enc most of these will
// be absent; running that encoder at its own defaults and telling the log which
// settings did not apply is better than refusing to send commentary.
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

	// bitrate is in KILOBITS per second on mfh264enc, which is the unit
	// DefaultVideoBitrateKbps and PipelineOpts.VideoBitrateKbps use.
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
func (p *cgoPipeline) gateProbe(_ gogst.Pad, _ *gogst.PadProbeInfo) gogst.PadProbeReturn {
	if p.gateClosed.Load() {
		return gogst.PadProbeDrop
	}
	return gogst.PadProbeOK
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

	switch msg.Type() {
	case gogst.MessageError:
		source := "pipeline"
		if src := msg.Source(); src != nil {
			source = src.GetName()
		}
		debug, gerr := msg.ParseError()

		// Close the gate first, before building the error value. Everything
		// after this point is allocation, and the buffer that is about to carry
		// GST_FLOW_ERROR into the queue is racing us.
		p.gateClosed.Store(true)

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

		if !isSinkSourced(source) {
			// Not the sink and not the queue in front of it: replacing the sink
			// cannot repair this, so mark it and let ReplaceSink refuse rather
			// than report a connection that carries no media.
			p.markFatal(fmt.Errorf("gst: pipeline-fatal: %w "+
				"(the capture or mux chain has failed; recover with Stop, New, Start)", err))
		}
		p.deliver(err)

	case gogst.MessageWarning:
		debug, gerr := msg.ParseWarning()
		source := "pipeline"
		if src := msg.Source(); src != nil {
			source = src.GetName()
		}
		// Warnings are logged and not delivered. A GStreamer warning is not a
		// pipeline failure, and putting it on Errors() would make
		// internal/sender treat it as one.
		log.Printf("gst: warning: %s: %v (%s)", source, gerr, debug)
	}

	return gogst.BusDrop
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

	// 1. Close the gate. Both probes now drop buffers, which isolates srtq from
	//    the sink about to be removed and isolates mpegtsmux from srtq.
	p.gateClosed.Store(true)

	// 2. Detach and destroy the old sink, in the order unlink, NULL, remove.
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
	p.rearmQueueLocked()

	// 4. Build the new sink.
	p.sinkSerial++
	name := srtSinkNamePrefix + strconv.Itoa(p.sinkSerial)
	sink := gogst.ElementFactoryMake("srtsink", name)
	if sink == nil {
		return errors.New("gst: could not create srtsink (is the srt plugin in the bundle?)")
	}
	if err := configureSRTSink(sink, opts); err != nil {
		return err
	}

	// 5. Add and link. Adding before linking is required: gst_pad_link across a
	//    bin boundary on an element with no parent does not give it the
	//    pipeline's clock or base time.
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

	// 6. Divert this sink's bus errors into this call, then bring it up. The
	//    route is installed before the state change because srtsink posts the
	//    error and returns STATE_CHANGE_FAILURE from the same call.
	route := &sinkErrRoute{name: name, ch: make(chan error, 1)}
	p.route.Store(route)
	defer p.route.Store(nil)

	if !sink.SyncStateWithParent() {
		p.abandonSinkLocked(sink, sinkPad)
		return fmt.Errorf("gst: %s: SRT caller handshake to %s failed: %v",
			name, endpointForLog(opts), routeErrOr(route, errors.New("gst_element_sync_state_with_parent returned FALSE")))
	}
	if ret := sink.BlockSetState(gogst.StatePlaying, gogst.ClockTime(sinkStateChangeTimeout)); !stateChangeOK(ret) {
		p.abandonSinkLocked(sink, sinkPad)
		return fmt.Errorf("gst: %s: SRT caller handshake to %s failed (%s): %v",
			name, endpointForLog(opts), ret, routeErrOr(route, errors.New("no bus error was posted")))
	}

	// A late error can still have arrived while the state change was reported
	// as successful — srtsink can accept the socket and then fail its first
	// write. Checking here means ReplaceSink does not report a connection that
	// has already gone.
	if err := routeErr(route); err != nil {
		p.abandonSinkLocked(sink, sinkPad)
		return fmt.Errorf("gst: %s: SRT connection to %s failed immediately: %w",
			name, endpointForLog(opts), err)
	}

	// 7. Open the gate. From this instant media flows to the new sink; the
	//    sticky events left pending by the gated pushes are delivered ahead of
	//    the first buffer by gst_pad_push_data's check_sticky.
	p.sink = sink
	p.gateClosed.Store(false)

	log.Printf("gst: %s connected to %s, latency %d ms, encryption %s",
		name, endpointForLog(opts), opts.LatencyMs, encryptionForLog(opts))
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

// rearmQueueLocked restores srtq's loop if it stopped with a bad flow return.
//
// gst_queue_loop stores the flow return of its last push in srcresult and
// pauses its task on anything other than GST_FLOW_OK. gst_queue_chain then
// returns that stored value to mpegtsmux on the next buffer. Deactivating and
// reactivating the src pad calls gst_queue_src_activate_mode, which resets
// srcresult to GST_FLOW_OK, flushes the stale queued data — which is what
// leaky=downstream would have done to it anyway — and restarts the loop task.
//
// This deliberately does NOT send a flush event pair through the queue. A
// FLUSH_STOP removes the sticky SEGMENT event from the pad it is sent to, and
// nothing would then re-push it, which is a far more obscure failure than the
// one being repaired.
//
// It is a no-op on the healthy path, which is every first connect and every
// reconnect where the gate closed before the queue saw the failure.
func (p *cgoPipeline) rearmQueueLocked() {
	last := p.srtqSrcPad.GetLastFlowReturn()
	if last == gogst.FlowOK {
		return
	}
	log.Printf("gst: %s stopped with %s; re-arming its loop", nameSRTQueue, last)
	if !p.srtqSrcPad.SetActive(false) {
		log.Printf("gst: could not deactivate %s:src while re-arming", nameSRTQueue)
	}
	if !p.srtqSrcPad.SetActive(true) {
		log.Printf("gst: could not reactivate %s:src while re-arming; "+
			"media will not flow until the pipeline is rebuilt", nameSRTQueue)
	}
}

// configureSRTSink applies every srtsink property from specification section 5.
//
// The passphrase is set with g_object_set_property and is never placed in the
// URI. That is not stylistic: a URI is percent-encoded, is printed by
// GStreamer's own debug output, and appears in the element's error messages. A
// passphrase in the URI is a passphrase in the log.
func configureSRTSink(sink gogst.Element, opts SinkOpts) error {
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

	if err := setStringProperty(sink, "uri", srtURI(opts.Host, opts.Port)); err != nil {
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

// srtURI builds the srtsink URI. It carries the endpoint and nothing else:
// every other setting, and in particular the passphrase, is a property.
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
func endpointForLog(opts SinkOpts) string {
	return srtURI(opts.Host, opts.Port)
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

	err := p.teardownLocked()

	// Close the error channel last, and only after the sync handler has been
	// detached and the pipeline has stopped posting. errMu is taken for writing
	// here; a streaming thread inside deliver holds it for reading only for the
	// duration of a non-blocking channel send, so this cannot wait long.
	//
	// The handler is detached inside teardownLocked BEFORE the state change,
	// because gst_element_set_state posts messages synchronously on the calling
	// goroutine: with the handler still attached, this goroutine would re-enter
	// deliver while holding errMu for writing and deadlock against itself.
	p.errMu.Lock()
	if !p.errsClosed {
		p.errsClosed = true
		close(p.errs)
	}
	p.errMu.Unlock()

	return err
}

// teardownLocked detaches the bus handler, removes the pad probes and takes the
// pipeline to NULL. p.mu must be held. It is safe to call on a partially built
// pipeline, which is how Start unwinds a failure.
func (p *cgoPipeline) teardownLocked() error {
	if p.bus != nil {
		p.bus.SetSyncHandler(nil)
	}

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

	var err error
	if p.pipeline != nil {
		// The whole pipeline goes to NULL in one call; there is no need to take
		// the sink down separately, and doing so would only add a way to fail.
		if ret := p.pipeline.BlockSetState(gogst.StateNull, gogst.ClockTime(pipelineStartTimeout)); !stateChangeOK(ret) {
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

// hasProperty reports whether a GObject has an installed property of this name.
//
// go-glib exposes PropertyType on gobject.ObjectInstance, which every gst
// element embeds, but does not list it in the gobject.Object interface — hence
// the type assertion. If this ever fails at Gate B, the assertion is the thing
// to look at, not the callers.
func hasProperty(obj gobject.Object, name string) bool {
	pt, ok := obj.(interface {
		PropertyType(string) gobject.Type
	})
	if !ok {
		return false
	}
	return pt.PropertyType(name) != gobject.TypeInvalid
}

// setStringProperty sets a string property, failing loudly if the element does
// not have it.
//
// The explicit check exists because the alternatives are silent:
// g_object_set_property on an unknown property emits a GLib warning to stderr
// and carries on, and gst_util_set_object_arg simply returns. A pipeline that
// starts with no slate path or no capture device set is a pipeline that sends
// nothing while looking healthy.
func setStringProperty(obj gobject.Object, name, value string) error {
	if !hasProperty(obj, name) {
		return fmt.Errorf("gst: element has no %s property", name)
	}
	obj.SetObjectProperty(name, value)
	return nil
}
