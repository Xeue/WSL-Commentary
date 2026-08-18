//go:build cgo && !gststub

// return_cgo.go is the go-gst backed return pipeline. It compiles only with
// CGO_ENABLED=1 and needs the same Gate B toolchain gst_cgo.go does.
//
// Read return.go first: it carries the contract, the mix matrix, the reconnect
// machine and the reasoning for all three. This file builds one attempt's
// pipeline and nothing else.
//
// # What is here and what is on the platform twins
//
// Everything in this file is true of the return path on EVERY platform: the
// transport, the demuxer, the dynamic-pad handling, the bus, the teardown and
// the whole threading model. It names no operating system.
//
// The three things that cannot be common live in return_cgo_windows.go and
// return_cgo_darwin.go, with return_cgo_other.go as the honest refusal for
// anything else:
//
//	returnDecodeSpecs           what turns aacparse's output into raw audio
//	returnRenderSpecs           what goes between the mix matrix and the sink
//	returnSinkFactory           the playback sink
//	configureReturnSinkLocked   how that sink is told which device to open
//	playbackDeviceID            what a device id IS in the enumeration
//
// They are files rather than GOOS tests inside these functions for the reason
// overlay_windows.go and overlay_other.go are: a branch that mentions both
// platforms compiles on both, so neither platform's compiler ever tells you it
// is wrong about the other. The cost is that the two lists have to be kept in
// step by hand when the COMMON part changes, which return_cgo_guard_test.go
// checks what it can of.
//
// # It shares a package with the send path and nothing else
//
// It reuses four helpers from gst_cgo.go — resolveSinkHost, stateChangeOK,
// stateChangeWatchdog, hasProperty — because they are stateless functions about
// GStreamer and DNS, not about the contribution feed. It touches no field, no
// mutex and no channel of cgoPipeline, it never calls into it, and a failure
// here cannot reach it. That separation is the whole point of the file; see the
// rule in return.go's header.
//
// # It does not report failures, and must not learn to
//
// This file's job is to fail HONESTLY: every error it returns or delivers on
// Errors carries what GStreamer and libsrt actually said, including the
// ERROR:BADSECRET and ERROR:UNSECURE tokens that arrive in a bus error's debug
// string, wrapped for context but never rewritten or summarised. Getting that
// reason to the operator is returnMonitor.report's job, and it is done once in
// return.go so that this file and return_stub.go cannot drift on it.
// ReturnOpts.OnConnectError is deliberately invisible here: a twin that had to
// remember to call it is a twin that could forget, and the two builds would then
// differ in exactly the behaviour an operator is relying on to diagnose a
// refused handshake.
//
// # Dynamic pads, and the fakesink that is not laziness
//
// tsdemux's pads are created at run time, on a streaming thread, once the
// demuxer has parsed a PMT and knows what streams the transport carries. A
// static link written at build time cannot work: at build time the demuxer has
// no src pads at all. So the audio branch is built, added and linked up to the
// point where it meets the demuxer, and the last link is made in the pad-added
// callback.
//
// The video pad gets a `fakesink sync=false` rather than being ignored. Ignoring
// it looks free and is not:
//
//   - An unlinked src pad on a demuxer is not inert. gst_pad_push returns
//     GST_FLOW_NOT_LINKED, and mpegtsbase aggregates the flow returns of its
//     pads: NOT_LINKED from one pad is tolerated only while ANOTHER pad is
//     returning OK, and the moment the audio branch back-pressures — which it
//     will, because the playback sink is clock-synced on both platforms and the
//     whole chain paces to the sound card — the aggregate becomes NOT_LINKED, the demuxer
//     pauses its task and posts an error. The symptom is a return that plays for
//     a second or two and then stops, which is exactly the kind of fault that
//     looks like a network problem and is not.
//
//   - sync=false is the other half, and it matters more than the element does.
//     A fakesink with the default sync=true waits on the pipeline clock for the
//     running time of every buffer it is given, and the video it is being given
//     is H.265 at 50p that nothing has decoded. It would hold the demuxer's
//     video streaming thread against the clock while the audio branch tried to
//     run ahead of it, and the two would fight over the same MPEG-TS read.
//     sync=false makes it consume and discard as fast as it is fed, which is
//     what "throw the picture away" actually requires.
//
// The picture is thrown away because there is no HEVC decoder on the target
// machine — mfh265dec is absent, the Windows HEVC extension is not installed —
// and because THIS TASK IS AUDIO ONLY. d3d11h265dec exists at primary+1 and
// would work, and adding the d3d11 plugin to the bundle is a decision the
// operator has not taken. Do not take it here.
//
// # No libav, ever
//
// The AAC decoder is an LGPL wrapper over a codec that is PART OF THE OPERATING
// SYSTEM, on both platforms: mfaacdec over Media Foundation on Windows, atdec
// over AudioToolbox on macOS. avdec_aac is present at primary rank on both dev
// machines and must not be used on either. gst-libav is FFmpeg, which is the
// same commercial-shipping concern as x264enc, and build/bundle-gst.ps1's
// forbidden list already refuses to copy anything matching *libav* or *avcodec*.
// Selecting it here would produce an application that works on the developer's
// machine and fails to load a plugin on the installed one.
//
// The rule survived the macOS port intact, and it cost one capsfilter to keep;
// see returnAACRawCaps in return_cgo_darwin.go.
package gst

import (
	"errors"
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// Element names inside the return pipeline. They are the names that appear as
// the source of a bus message, so they are chosen to be legible in a field log
// and to be impossible to confuse with the send path's — nothing here collides
// with mux, srtq, slate, asrc, venc, vscale or srtout-N.
//
// The platform twins add names of their own for the elements only they have;
// those live on the twin, so that this list stays the list of elements every
// build has.
const (
	nameReturnPipeline = "wslcomms-return"
	nameReturnSrc      = "retsrc"   // srtsrc
	nameReturnDemux    = "retdemux" // tsdemux
	nameReturnQueue    = "retq"     // queue at the head of the audio branch
	nameReturnParse    = "retparse" // aacparse
	nameReturnDecode   = "retdec"   // the AAC decoder: mfaacdec or atdec
	nameReturnConvert  = "retconv"  // audioconvert, carrying the mix matrix
	nameReturnResample = "retres"   // audioresample
	nameReturnSink     = "retsink"  // the playback sink: wasapi2sink or osxaudiosink
	nameReturnFakeSink = "retfake"  // fakesink for the video pad
)

// returnElementSpec is one element of the audio branch: what to make it from,
// what to call it, and optionally what caps to pin on it.
//
// It exists so that the platform twins can hand back a LIST rather than being
// asked a question per element. A twin that had to answer "is there a capsfilter
// before the decoder?" would be a twin that could only ever differ in the ways
// somebody had already thought to ask about; a list can differ in shape.
type returnElementSpec struct {
	// factory is the element factory name, e.g. "audioresample".
	factory string

	// name is the element's name in the bin, and is what appears as the source
	// of a bus message. Every name in one pipeline must be distinct or
	// gst_bin_add refuses the second one.
	name string

	// caps, when non-empty, is set on this element's caps property. Only
	// capsfilter has one, and only the macOS chain uses any. It is parsed with
	// gst_caps_from_string and REFUSED if that fails, rather than being set with
	// gst_util_set_object_arg — measured, an unparseable string through the
	// latter leaves the filter at ANY and the pipeline runs on, which for
	// returnAACRawCaps would mean digital silence with no error anywhere.
	caps string

	// out, when non-nil, receives the created element so the caller can keep a
	// reference to it. Elements without one stay alive because the bin holds a
	// ref; the field is for the ones this file has to talk to again.
	out *gogst.Element
}

// returnStateChangeTimeout bounds the asynchronous tail of the return
// pipeline's NULL to PLAYING transition.
//
// Read the timeout commentary at the top of gst_cgo.go before relying on this:
// it does NOT bound the synchronous part. gst_element_set_state takes no timeout
// and runs every change_state function on the calling goroutine, so a wedged
// playback endpoint — a WASAPI one or a CoreAudio one — hangs here forever and
// no value written here changes that. stateChangeWatchdog makes such a hang
// legible in the log; it cannot cure it.
//
// What the number has to cover is srtsrc's caller connect, which libsrt bounds
// at its default SRTO_CONNTIMEO of 3 s, plus opening a playback endpoint. Ten
// seconds is far beyond both, and matches the send path's
// sinkStateChangeTimeout so that the two paths fail on the same scale.
const returnStateChangeTimeout = 10 * time.Second

// returnErrorBuffer is how many asynchronous errors are held before further
// ones are dropped. It matches stubReturnErrorBuffer so the two builds behave
// identically under a slow consumer, and it drops rather than blocks for the
// same reason the send path does: the sender of these errors is a GStreamer
// streaming thread and must never wait on a Go consumer.
const returnErrorBuffer = 8

// returnMissingElements returns a description of every required factory the
// registry does not have, or nil if the bundle is complete for this path.
//
// returnRequiredElements is the PLATFORM twin's list, because the factories
// differ and so do the plugins that carry them. It is checked by newReturnPipe
// rather than by Init, and that is deliberate. Init's requiredElements is the
// CONTRIBUTION path's list, and a failure there means the application cannot
// send and must say so at startup. A missing mpegtsdemux means the commentator
// has no monitor — bad, but it must not stop the feed going on air, and it must
// not make Init fail. So it is checked at the point of use and reported through
// the return monitor's own error path.
func returnMissingElements() []string {
	var missing []string
	for _, req := range returnRequiredElements {
		if gogst.ElementFactoryFind(req.factory) == nil {
			missing = append(missing, fmt.Sprintf("%s (plugin %s)", req.factory, req.plugin))
		}
	}
	return missing
}

// ListOutputDevices returns the audio PLAYBACK endpoints offered in the
// headphone dropdown, for the SRT return path.
//
// It mirrors ListInputDevices: a GstDeviceMonitor filtered to Audio/Sink,
// reporting display-name as Device.Name and this platform's stable device
// identity as Device.ID.
//
// # What counts as a device, and what counts as its id, is per-platform
//
// playbackDeviceID is the seam, and it is a PREDICATE rather than a
// provider name on purpose. The obvious shape — accept device.api == "wasapi2"
// or device.api == "osxaudio" — is wrong twice over: osxaudiodeviceprovider does
// not publish device.api at all, so the macOS half would silently match nothing
// and the dropdown would be empty, which is exactly the bug on the capture side
// that started the macOS port. Each twin therefore tests what its own platform
// actually publishes; see the two implementations.
//
// # This is not the same identifier the browser uses, and they must not be mixed
//
// The WebRTC return path selects headphones with a browser mediaDeviceId and
// HTMLMediaElement.setSinkId. That ID is a per-origin, per-session salted hash
// generated by the WebView; it is meaningless to WASAPI and to CoreAudio alike,
// and it changes when the browsing context's storage is cleared. Device.ID here
// is the operating system's own stable identity — an IMMDevice endpoint ID GUID
// on Windows, a CoreAudio UID on macOS — which is what survives a rename of the
// device and what ReturnOpts.OutputDeviceID documents in full.
//
// That the native half is platform-dependent changes NOTHING about the
// separation. The browser id is not any operating system's identifier for a
// device; it is a token minted by one browsing context. They identify the same
// pair of headphones and are not interchangeable in either direction. Config
// keeps both, as headphoneDeviceId and headphoneEndpointId, and the failure mode
// of conflating them is silent on Windows — wasapi2sink ignores a device string
// it does not recognise and opens the default endpoint, so the commentator gets
// audio in the wrong ears and nothing says why. chooseOutputDevice in return.go
// is what now says why, on both platforms.
func ListOutputDevices() ([]Device, error) {
	if !inited.Load() {
		return nil, errors.New("gst: ListOutputDevices: Init has not been called")
	}

	// Audio/Sink is the class every audio playback provider advertises. Every
	// device is re-checked against it below in any case: without that check a
	// microphone can appear in the headphone dropdown, and on Windows that is
	// not hypothetical — wasapi2 republishes render endpoints into Audio/Source
	// and the two id sets overlap byte for byte.
	devices := enumerateDevices("Audio/Sink")

	out := make([]Device, 0, len(devices))
	for _, dev := range devices {
		if dev == nil {
			continue
		}
		if !dev.HasClasses("Audio/Sink") {
			continue
		}
		props := dev.GetProperties()
		if props == nil {
			// gst_device_get_properties is nullable. Without properties the
			// provider cannot be identified and no id can be recovered, so the
			// device cannot safely be offered.
			log.Printf("gst: ListOutputDevices: skipping %q: it publishes no device properties",
				dev.GetDisplayName())
			continue
		}
		// The platform seam, and the mirror of ListInputDevices' captureDeviceID.
		// It answers two things about one device: does it belong to the provider
		// this build drives, and if so what is the string to persist for it. Every
		// rejection is logged by the twin with its own reason, because a device
		// missing from the dropdown is otherwise indistinguishable from a device
		// that is not plugged in.
		id, ok := playbackDeviceID(dev, props)
		if !ok {
			continue
		}
		out = append(out, Device{ID: id, Name: dev.GetDisplayName()})
	}
	log.Printf("gst: ListOutputDevices: offering %d of %d Audio/Sink devices", len(out), len(devices))
	return out, nil
}

// returnPipeline is the go-gst backed returnPipe: one attempt's pipeline.
//
// # Threading model
//
// Four kinds of thread touch one of these, and the locking below only makes
// sense against this list.
//
//  1. The reconnect machine's goroutine, which calls Play and Close. Both take
//     mu for their whole duration, which means mu is held across GStreamer state
//     changes and therefore for seconds at a time.
//
//  2. Whichever GStreamer thread posts a bus message. gst_element_post_message
//     calls the sync handler synchronously, on the posting thread, before the
//     failing function has returned — so onBusMessage runs on a streaming thread
//     while Play may be holding mu, and MUST NOT take mu. It uses atomics and
//     errMu, which is held only for a non-blocking channel send.
//
//  3. The demuxer's streaming thread, running onPadAdded. It takes padMu, which
//     is never held across a pipeline state change and is never held while mu is
//     held, so it cannot deadlock against the caller.
//
//  4. The audio-pad watchdog's timer goroutine.
//
// Every GStreamer object kept across calls is stored in a field so it stays
// reachable: dropping a Go reference to a live GstElement would let go-gst's
// finalizer unref it.
type returnPipeline struct {
	// mu serialises Play and Close against each other.
	mu     sync.Mutex
	played bool
	closed bool

	// pipeline and its elements are held so they stay reachable.
	pipeline  gogst.Pipeline
	bus       gogst.Bus
	src       gogst.Element
	demux     gogst.Element
	queue     gogst.Element
	queuePad  gogst.Pad
	convert   gogst.Element
	sink      gogst.Element
	fakeSinks []gogst.Element

	// fakeSeq numbers the fakesinks so that no two can ever be given the same
	// element name. It is an atomic rather than a padMu-guarded counter because
	// it is incremented on the demuxer's streaming thread and read nowhere else;
	// see fakeSinkName in return.go for what a repeated name costs.
	fakeSeq atomic.Uint64

	// padMu guards audioLinked, audioReady and fakeSinks, all touched from the
	// demuxer's streaming thread inside onPadAdded.
	padMu       sync.Mutex
	audioLinked bool

	// audioReady is closed by onPadAdded the moment an audio pad is linked to
	// the headphone branch. It is what Play waits on, and it is recreated for
	// each address attempt so that one address's success cannot be read as
	// another's.
	//
	// It is deliberately NOT cleared when it is closed, and audioSignalled is a
	// separate flag rather than a nil check, because the pad-added callback can
	// and does run DURING the BlockSetState that precedes the wait. Clearing the
	// field on signal would leave awaitAudio reading nil, selecting on a channel
	// that can never fire, and waiting the full returnAudioPadTimeout for a
	// monitor that was already working. A closed channel is permanently ready,
	// so a late reader sees the signal immediately, which is the behaviour that
	// race needs.
	audioReady     chan struct{}
	audioSignalled bool

	// tearing is raised by Close before it touches the pipeline, so that a
	// pad-added callback arriving on a streaming thread mid-teardown does not
	// add an element to a pipeline that is going to NULL.
	tearing atomic.Bool

	// busSilenced makes onBusMessage return immediately once teardown has
	// begun. It replaces detaching the sync handler, which is not something to
	// do while a streaming thread may be inside it.
	busSilenced atomic.Bool

	// errMu guards errs and errsClosed. It exists only so that a streaming
	// thread cannot send on a channel Close is closing, and is never held
	// across anything that can block.
	errMu      sync.RWMutex
	errs       chan error
	errsClosed bool
}

var _ returnPipe = (*returnPipeline)(nil)

// newReturnPipe creates a returnPipeline. See return.go.
//
// It refuses early, with a message naming the missing plugin, rather than
// letting the failure surface as a nil from gst_element_factory_make three
// screens further down.
func newReturnPipe() (returnPipe, error) {
	if !inited.Load() {
		return nil, errors.New("gst: return monitor: Init has not been called")
	}
	if !returnPlatformSupported {
		// return_cgo_other.go. The application ships on Windows and macOS; this
		// is the one clear sentence somebody building for a third platform gets,
		// instead of a pipeline made of empty factory names.
		return nil, fmt.Errorf(
			"gst: return monitor: the SRT return is implemented for Windows and macOS only, "+
				"and this is %s", runtime.GOOS)
	}
	if missing := returnMissingElements(); len(missing) > 0 {
		return nil, fmt.Errorf(
			"gst: return monitor: the bundled GStreamer is missing %s "+
				"(add the plugin to the allowlist in build/bundle-gst.ps1 and re-run it)",
			strings.Join(missing, ", "))
	}
	return &returnPipeline{errs: make(chan error, returnErrorBuffer)}, nil
}

// Play builds the pipeline and takes it to PLAYING. See returnPipe.
func (r *returnPipeline) Play(opts ReturnOpts) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return errors.New("gst: return pipeline is closed")
	}
	if r.played {
		return errors.New("gst: return pipeline already played")
	}

	// The mix matrix is resolved before anything is built, so that an unknown
	// channel mode fails without having opened a socket or a sound card.
	matrix, err := MixMatrix(opts.Channel)
	if err != nil {
		return err
	}
	matrixArg := MixMatrixString(matrix)

	// Resolve the host to a literal before building anything.
	//
	// The send path does this because a hostname in an srtsink URI aborts the
	// process on teardown, inside a GLib assertion in GResolver from which there
	// is no return. The same GstSRTObject code backs srtsrc, so the same
	// hazard applies, and the same fix does: nothing but an IP literal ever
	// reaches the element. It also costs nothing here — the lookup is bounded at
	// hostResolveTimeout and happens on every attempt, so an M2L-X instance that
	// moves address mid-match is picked up by the next reconnect.
	addrs, err := resolveSinkHost(opts.Host, hostResolveTimeout)
	if err != nil {
		return err
	}

	element := gogst.NewPipeline(nameReturnPipeline)
	if element == nil {
		return errors.New("gst: return monitor: gst_pipeline_new returned nil")
	}
	pipeline, ok := element.(gogst.Pipeline)
	if !ok {
		return fmt.Errorf("gst: return monitor: gst_pipeline_new returned a %T, not a GstPipeline", element)
	}
	r.pipeline = pipeline

	// From here on any failure must tear down what has been built, or the
	// headphone endpoint and the SRT socket stay open and the next attempt
	// cannot have them.
	abort := func(err error) error {
		r.teardownLocked()
		return err
	}

	if err := r.buildLocked(opts, matrixArg); err != nil {
		return abort(err)
	}

	// The bus sync handler is attached before the first state change so that an
	// error raised during NULL to PLAYING is captured rather than lost. It is a
	// sync handler rather than a watch for the same reason the send path's is:
	// a watch needs a GLib main loop and this process does not have one, because
	// Wails owns the Windows message loop.
	r.bus = pipeline.GetBus()
	if r.bus == nil {
		return abort(errors.New("gst: return monitor: pipeline has no bus"))
	}
	r.busSilenced.Store(false)
	r.bus.SetSyncHandler(r.onBusMessage)

	// A name may front several addresses. Try them in the order resolveSinkHost
	// returned — IPv4 first — and say which one answered.
	var lastErr error
	for i, addr := range addrs {
		where := srtURI(addr, opts.Port)
		if len(addrs) > 1 {
			where = fmt.Sprintf("%s, address %d of %d", where, i+1, len(addrs))
		}

		// Each attempt starts clean. Anything the previous address posted is
		// discarded, and a fresh audioReady is armed, so that neither can be
		// read as belonging to this one.
		r.drainErrors()
		r.armAudioReady()

		if err := r.configureSrcLocked(opts, addr); err != nil {
			return abort(err)
		}

		stopWatchdog := stateChangeWatchdog("return monitor: pipeline to PLAYING for " + where)
		ret := pipeline.BlockSetState(gogst.StatePlaying, gogst.ClockTime(returnStateChangeTimeout))
		stopWatchdog()
		if !stateChangeOK(ret) {
			lastErr = fmt.Errorf("gst: return monitor: could not start the pipeline for %s (%s)", where, ret)
			if busErr := r.drainStartupError(); busErr != nil {
				lastErr = fmt.Errorf("%w: %v", lastErr, busErr)
			}
			r.toNullForRetry()
			continue
		}

		// PLAYING proves NOTHING on this path. srtsrc connects lazily inside
		// gst_srt_src_fill, on a streaming thread, and reports failure on the
		// bus about three seconds later; measured at the live instance on
		// 2026-08-07, see returnAudioPadTimeout. Returning here would report
		// success on a socket that does not exist.
		if err := r.awaitAudio(where); err != nil {
			lastErr = err
			r.toNullForRetry()
			continue
		}

		lastErr = nil
		break
	}
	if lastErr != nil {
		return abort(lastErr)
	}

	r.played = true
	log.Printf("gst: return monitor: receiving audio from %s", opts.endpointForLog())
	return nil
}

// awaitAudio blocks until the demuxer's audio pad is linked to the headphone
// branch, the pipeline reports a failure, or returnAudioPadTimeout expires. It
// is the whole reason Play is honest; see the constant.
//
// r.mu is held throughout, which is fine: nothing that has to make progress
// takes it. The bus handler and the pad-added callback both deliberately avoid
// it, precisely so that this wait is possible.
func (r *returnPipeline) awaitAudio(where string) error {
	r.padMu.Lock()
	ready := r.audioReady
	r.padMu.Unlock()
	if ready == nil {
		// armAudioReady runs immediately before every call, so this cannot
		// happen. It is handled rather than assumed because the alternative is a
		// select on a nil channel, which is a wait that only the timeout ends.
		return errors.New("gst: return monitor: internal error: no audio wait was armed")
	}

	timeout := time.NewTimer(returnAudioPadTimeout)
	defer timeout.Stop()

	select {
	case <-ready:
		return nil

	case err, ok := <-r.errs:
		// A bus error during the connect — in practice srtsrc's "Connection
		// timeout (16)" when nothing is listening on the far end. It is
		// returned rather than left on the channel because the caller is about
		// to throw this whole pipeline away, and an error nobody reads is an
		// outage with no explanation.
		if !ok || err == nil {
			return fmt.Errorf("gst: return monitor: %s failed during startup", where)
		}
		return err

	case <-timeout.C:
		// Connected, or at least not visibly failed, but no AAC ever arrived.
		// The far end is listening and sending nothing, or the output carries no
		// audio stream at all. Either way this is a failed attempt and not a
		// monitor to report as healthy.
		return fmt.Errorf(
			"gst: return monitor: no audio arrived from %s within %s "+
				"(is that M2L-X output started, and does it carry AAC?)",
			where, returnAudioPadTimeout)
	}
}

// armAudioReady creates a fresh audioReady channel for one address attempt.
func (r *returnPipeline) armAudioReady() {
	r.padMu.Lock()
	r.audioLinked = false
	r.audioSignalled = false
	r.audioReady = make(chan struct{})
	r.padMu.Unlock()
}

// signalAudioReady closes the current audioReady channel, exactly once per
// attempt. It runs on the demuxer's streaming thread.
func (r *returnPipeline) signalAudioReady() {
	r.padMu.Lock()
	if r.audioReady != nil && !r.audioSignalled {
		r.audioSignalled = true
		close(r.audioReady)
	}
	r.padMu.Unlock()
}

// toNullForRetry takes the pipeline back to NULL between address attempts, so
// that a failed srtsrc's socket is released rather than left half-open while
// another connect is attempted from the same pipeline.
//
// It also takes the previous attempt's fakesinks back out of the bin. THE
// PIPELINE IS REUSED ACROSS ADDRESSES, and everything the last attempt's
// demuxer built is still in it: sinks linked to pads that no longer exist,
// which the next attempt has no use for and which gst_bin_add would have to
// find distinct names around. fakeSinkName already guarantees the names are
// distinct, so this is not what keeps address 2 working — it is what stops the
// bin growing a sink per undecoded pad per address for the life of an M2L-X
// instance with several A records.
//
// The order is fixed: the pipeline reaches NULL first, which stops the streaming
// threads and brings every child to NULL with it, and only then are the elements
// removed. gst_bin_remove unlinks the pads it finds linked, but removing an
// element from a bin that is still rolling is a race with the thread pushing
// into it.
func (r *returnPipeline) toNullForRetry() {
	if r.pipeline == nil {
		return
	}
	r.pipeline.BlockSetState(gogst.StateNull, gogst.ClockTime(elementShutdownTimeout))

	r.padMu.Lock()
	stale := r.fakeSinks
	r.fakeSinks = nil
	r.padMu.Unlock()

	for _, fake := range stale {
		if fake == nil {
			continue
		}
		if !r.pipeline.Remove(fake) {
			// Logged, not delivered. The next attempt is not harmed by a sink
			// left behind — its own names are unique — so this is a hygiene
			// failure, and putting it on Errors would abort a reconnect that can
			// still succeed.
			log.Printf("gst: return monitor: could not remove %s from the pipeline between attempts",
				fake.GetName())
		}
	}
}

// drainErrors discards everything currently queued, without blocking.
func (r *returnPipeline) drainErrors() {
	r.errMu.RLock()
	defer r.errMu.RUnlock()
	if r.errsClosed {
		return
	}
	for {
		select {
		case <-r.errs:
		default:
			return
		}
	}
}

// buildLocked creates, adds and links everything that can be linked statically.
// r.mu must be held and r.pipeline must be set.
//
// Every element is created, added to the pipeline and linked BEFORE the pipeline
// is started, so that the only thing left for the pad-added callback to do is a
// single gst_pad_link. That keeps the work done on a streaming thread to the
// minimum: adding elements and syncing their state from inside a pad-added
// callback is legal but is the part of the dynamic-pad idiom that goes wrong,
// and the audio branch does not need it.
func (r *returnPipeline) buildLocked(opts ReturnOpts, matrixArg string) error {
	// The transport head. Identical everywhere.
	head := []returnElementSpec{
		{factory: "srtsrc", name: nameReturnSrc, out: &r.src},
		{factory: "tsdemux", name: nameReturnDemux, out: &r.demux},
	}

	// The audio branch, in pipeline order, with the two platform-supplied
	// stretches spliced into the middle of it.
	//
	// The queue, the parser and the mix-matrix audioconvert are common because
	// they are about the TRANSPORT and about the operator's channel selection,
	// neither of which is an operating system's business. Everything either side
	// of the audioconvert comes from the twin, because a decoder and a sink are
	// exactly an operating system's business.
	branch := []returnElementSpec{
		{factory: "queue", name: nameReturnQueue, out: &r.queue},
		{factory: "aacparse", name: nameReturnParse},
	}
	branch = append(branch, returnDecodeSpecs()...)
	branch = append(branch, returnElementSpec{
		factory: "audioconvert", name: nameReturnConvert, out: &r.convert})
	branch = append(branch, returnRenderSpecs()...)
	branch = append(branch, returnElementSpec{
		factory: returnSinkFactory, name: nameReturnSink, out: &r.sink})

	built := make([]gogst.Element, 0, len(branch))
	for _, s := range append(append([]returnElementSpec(nil), head...), branch...) {
		el := gogst.ElementFactoryMake(s.factory, s.name)
		if el == nil {
			return fmt.Errorf("gst: return monitor: could not create %s", s.factory)
		}
		if s.caps != "" {
			// Parsed rather than handed to gst_util_set_object_arg, because that
			// function returns void and simply does nothing with a string it
			// cannot deserialise — leaving the filter at ANY, which for the ADTS
			// filter on macOS means a monitor that plays digital silence with no
			// error anywhere. A caps string this file got wrong must be a refusal
			// to build, not a quiet pass-through.
			caps := gogst.CapsFromString(s.caps)
			if caps == nil {
				return fmt.Errorf("gst: return monitor: %s is not a parseable caps string for %s",
					s.caps, s.name)
			}
			el.SetObjectProperty("caps", caps)
		}
		if !r.pipeline.Add(el) {
			return fmt.Errorf("gst: return monitor: could not add %s to the pipeline", s.name)
		}
		if s.out != nil {
			*s.out = el
		}
		built = append(built, el)
	}
	// Everything after the head is the audio branch, in order.
	audio := built[len(head):]

	// srtsrc to tsdemux is a static link on both sides and is made here. Every
	// link from the demuxer onwards is dynamic; see onPadAdded.
	if !r.src.Link(r.demux) {
		return fmt.Errorf("gst: return monitor: could not link %s to %s", nameReturnSrc, nameReturnDemux)
	}

	// The audio branch, in order. All static pads.
	for i := 0; i+1 < len(audio); i++ {
		if !audio[i].Link(audio[i+1]) {
			return fmt.Errorf("gst: return monitor: could not link the audio branch: %s would not "+
				"link to %s", audio[i].GetName(), audio[i+1].GetName())
		}
	}

	// The queue's sink pad is the one the demuxer's audio pad links to. It is
	// captured here rather than looked up in the callback, because the callback
	// runs on a streaming thread and a field cleared by teardownLocked would be
	// a data race on exactly the path being torn down.
	r.queuePad = r.queue.GetStaticPad("sink")
	if r.queuePad == nil {
		return errors.New("gst: return monitor: " + nameReturnQueue + " has no sink pad")
	}

	// The mix matrix. This is the whole of the channel-selection feature.
	//
	// It is set with gst_util_set_object_arg rather than g_object_set because
	// mix-matrix is a GST_TYPE_ARRAY of GST_TYPE_ARRAY of gfloat, which has no
	// Go representation go-glib can marshal — the string form is the same one
	// gst-launch parses and is deserialised by the same code.
	//
	// The failure mode of getting the string wrong is SILENT:
	// gst_util_set_object_arg returns void and simply does nothing with a value
	// it cannot deserialise, leaving audioconvert's default mapping in place.
	// The commentator would hear stereo when they asked for left, with no error
	// anywhere. That is why the property's PRESENCE is checked below — the one
	// part of this that CAN be checked from here — and why MixMatrixString is
	// asserted against an exact expected string rather than just the numbers.
	//
	// The matrices themselves were measured against this element rather than
	// reasoned about, including the row/column orientation, which is the thing
	// most likely to be wrong and least likely to be noticed. The numbers are in
	// MixMatrix's doc comment.
	if matrixArg != "" {
		if !hasProperty(r.convert, "mix-matrix") {
			return errors.New("gst: return monitor: audioconvert has no mix-matrix property; " +
				"channel selection is not available on this GStreamer")
		}
		gogst.UtilSetObjectArg(r.convert, "mix-matrix", matrixArg)
	}

	// The headphone endpoint, in two steps that are deliberately separate.
	//
	// vetOutputDeviceID (return.go) decides WHETHER the configured id can be
	// used, against the devices this machine is actually offering, and logs the
	// reason if it cannot. It is platform-neutral, because "the operator's saved
	// device is not here" is the same situation everywhere and the two platforms
	// must not answer it differently — a config.json carried between them is a
	// thing that happens, and before this it failed silently on Windows and
	// unstartably on macOS.
	//
	// configureReturnSinkLocked then decides HOW to say it to this platform's
	// sink, which is where the two genuinely differ: a string endpoint id for
	// wasapi2sink, an integer resolved from a CoreAudio UID for osxaudiosink.
	if err := configureReturnSinkLocked(r.sink, vetOutputDeviceID(opts.OutputDeviceID)); err != nil {
		return err
	}

	// The pipeline and the queue's sink pad are captured HERE, into the closure,
	// rather than read from r inside the callback. See onPadAdded for why that
	// is load-bearing rather than stylistic.
	pipeline, queuePad := r.pipeline, r.queuePad
	r.demux.ConnectPadAdded(func(_ gogst.Element, pad gogst.Pad) {
		r.onPadAdded(pipeline, queuePad, pad)
	})
	return nil
}

// configureSrcLocked sets every srtsrc property for one address. r.mu must be
// held.
func (r *returnPipeline) configureSrcLocked(opts ReturnOpts, addr string) error {
	if err := setStringProperty(r.src, "uri", srtURI(addr, opts.Port)); err != nil {
		return fmt.Errorf("gst: return monitor: %w", err)
	}

	// mode=caller. Every M2L-X output has reverse=True, which means M2L-X
	// LISTENS and we dial in. It is set explicitly rather than relied on as the
	// default, and by nickname through gst_util_set_object_arg because the
	// property is a GEnum.
	if hasProperty(r.src, "mode") {
		gogst.UtilSetObjectArg(r.src, "mode", "caller")
	}

	if hasProperty(r.src, "latency") {
		r.src.SetObjectProperty("latency", int32(opts.LatencyMs))
	} else {
		log.Printf("gst: return monitor: srtsrc has no latency property; using the element default")
	}

	// srtsrc's own auto-reconnect is turned off, for the same reason the send
	// path turns srtsink's off: it reopens immediately with no backoff, straight
	// into M2L-X's roughly five second re-accept refusal window, and it does it
	// without telling anyone — so the RETURN lamp would stay green through an
	// outage the commentator can hear. The backoff ladder in return.go is the
	// only reconnect on this path.
	if hasProperty(r.src, "auto-reconnect") {
		r.src.SetObjectProperty("auto-reconnect", false)
	}

	// The passphrase is set with g_object_set and never placed in the URI, so it
	// is neither percent-encoded nor logged. Nothing in this file formats it,
	// and ReturnOpts.endpointForLog exists so that nothing is tempted to.
	if opts.Passphrase != "" {
		if err := setStringProperty(r.src, "passphrase", opts.Passphrase); err != nil {
			return fmt.Errorf("gst: return monitor: the SRT passphrase could not be set: %w", err)
		}
		if hasProperty(r.src, "pbkeylen") {
			r.src.SetObjectProperty("pbkeylen", int32(opts.PBKeyLen))
		}
	}
	return nil
}

// onPadAdded links one of the demuxer's dynamic pads. It runs on the demuxer's
// streaming thread.
//
// The audio pad goes to the audio branch, which is already built, linked and in
// PLAYING, so this is a single gst_pad_link and no state change. Everything else
// — in practice the H.265 video — gets its own fakesink sync=false. See the file
// header for why that is required rather than tidy.
//
// pipeline and queuePad are PARAMETERS, captured by the closure buildLocked
// installs, rather than reads of r.pipeline and r.queuePad. Those fields are
// cleared by teardownLocked on the caller's goroutine, and this function runs on
// a streaming thread, so reading them here would be a data race on precisely the
// path that is being torn down — the one place a race is least likely to be
// noticed and most likely to crash. gst_cgo.go captures srtq's src pad into its
// event probe for the same reason. The tearing flag above narrows the window but
// does not close it; passing the values closes it.
func (r *returnPipeline) onPadAdded(pipeline gogst.Pipeline, queuePad gogst.Pad, pad gogst.Pad) {
	if r.tearing.Load() {
		// Close is running. Adding an element to a pipeline on its way to NULL
		// would be a race with gst_bin_remove for no benefit: there is nothing
		// left to play.
		return
	}
	if pad == nil {
		return
	}

	name := pad.GetName()
	kind := padMediaKind(pad)

	if kind == "audio" {
		r.padMu.Lock()
		already := r.audioLinked
		if !already {
			r.audioLinked = true
		}
		r.padMu.Unlock()

		if already {
			// A transport carrying two audio streams. The first one wins and
			// the rest are discarded, because there is one pair of headphones
			// and picking arbitrarily between streams would be worse than
			// picking the first. They still need a sink; an unlinked pad wedges
			// the demuxer just as the video one would.
			log.Printf("gst: return monitor: %s is a second audio stream; discarding it", name)
			r.fakeSinkFor(pipeline, pad)
			return
		}

		if ret := pad.Link(queuePad); ret != gogst.PadLinkOK {
			// Undo the claim, so that a later audio pad — a PMT update, or the
			// second stream of a transport whose first one could not be linked —
			// still gets a chance at the branch.
			r.padMu.Lock()
			r.audioLinked = false
			r.padMu.Unlock()

			r.deliver(fmt.Errorf("gst: return monitor: could not link %s to %s (%s)",
				name, nameReturnQueue, ret))
			return
		}

		// Release Play. This is the ONLY thing in the file that says audio is
		// genuinely flowing, and it is deliberately after the link rather than
		// on the mere appearance of an audio pad: a pad that could not be linked
		// is not a monitor.
		r.signalAudioReady()
		log.Printf("gst: return monitor: linked demuxer pad %s to the audio branch", name)
		return
	}

	log.Printf("gst: return monitor: discarding demuxer pad %s (%s); this build decodes audio only",
		name, kind)
	r.fakeSinkFor(pipeline, pad)
}

// padMediaKind classifies a demuxer pad as "audio", "video" or something else.
//
// It reads the pad's caps first, because that is authoritative, and falls back
// to the pad NAME only if there are none. tsdemux names its pads audio_%04x and
// video_%04x after the PID, so the name is a genuine second source rather than a
// guess — but it is second, because a pad name is a convention and caps are the
// contract.
func padMediaKind(pad gogst.Pad) string {
	caps := pad.GetCurrentCaps()
	if caps == nil {
		caps = pad.QueryCaps(nil)
	}
	if caps != nil {
		if s := caps.GetStructure(0); s != nil {
			name := s.GetName()
			switch {
			case strings.HasPrefix(name, "audio/"):
				return "audio"
			case strings.HasPrefix(name, "video/"):
				return "video"
			}
			if name != "" {
				return name
			}
		}
	}
	switch {
	case strings.HasPrefix(pad.GetName(), "audio"):
		return "audio"
	case strings.HasPrefix(pad.GetName(), "video"):
		return "video"
	}
	return "unknown"
}

// fakeSinkFor attaches a fakesink sync=false to a pad this build does not
// decode. It runs on a streaming thread.
//
// The element is added to the pipeline, brought up with SyncStateWithParent and
// only then linked. That order matters: linking an element that has no parent
// does not give it the pipeline's clock or base time, and a sink that is still
// in NULL when a buffer arrives returns GST_FLOW_FLUSHING — which the demuxer
// aggregates exactly as it would the NOT_LINKED this whole function exists to
// prevent.
//
// A failure here is delivered on Errors rather than logged and forgotten,
// because its consequence is the demuxer wedging a few seconds later and the
// operator being told nothing.
func (r *returnPipeline) fakeSinkFor(pipeline gogst.Pipeline, pad gogst.Pad) {
	// The name carries a per-pipeline sequence number, NOT just the pad name.
	// tsdemux names pads after the PID, one pipeline is retried against every
	// address resolveSinkHost returned, and gst_bin_add refuses a duplicate name
	// — so a name derived from the pad alone makes every address after the first
	// fail to sink its undecoded pads, and an unsinked pad wedges the demuxer.
	// See fakeSinkName in return.go.
	name := fakeSinkName(nameReturnFakeSink, r.fakeSeq.Add(1), pad.GetName())
	fake := gogst.ElementFactoryMake("fakesink", name)
	if fake == nil {
		r.deliver(errors.New("gst: return monitor: could not create fakesink for " + pad.GetName()))
		return
	}
	// sync=false: consume and discard as fast as the demuxer produces. With the
	// default sync=true this sink would wait on the pipeline clock for the
	// running time of undecoded H.265 and hold the demuxer's video streaming
	// thread against it. See the file header.
	fake.SetObjectProperty("sync", false)
	// async=false: a sink that has not prerolled holds the whole pipeline in
	// ASYNC. This one is never going to be interesting and must not be able to
	// hold the audio branch out of PLAYING.
	if hasProperty(fake, "async") {
		fake.SetObjectProperty("async", false)
	}

	if !pipeline.Add(fake) {
		r.deliver(errors.New("gst: return monitor: could not add " + name + " to the pipeline"))
		return
	}
	// Recorded only once it is IN the bin, because the list means "elements this
	// pipeline put in the bin and must take back out" — see toNullForRetry.
	// Recording an element that was never added would make the retry path call
	// gst_bin_remove on a stranger. `fake` stays reachable across the Add either
	// way, which is the other thing the list has to guarantee.
	r.padMu.Lock()
	r.fakeSinks = append(r.fakeSinks, fake)
	r.padMu.Unlock()

	if !fake.SyncStateWithParent() {
		r.deliver(errors.New("gst: return monitor: " + name + " would not sync state with the pipeline"))
		return
	}
	sinkPad := fake.GetStaticPad("sink")
	if sinkPad == nil {
		r.deliver(errors.New("gst: return monitor: " + name + " has no sink pad"))
		return
	}
	if ret := pad.Link(sinkPad); ret != gogst.PadLinkOK {
		r.deliver(fmt.Errorf("gst: return monitor: could not link %s to %s (%s)",
			pad.GetName(), name, ret))
	}
}

// onBusMessage is the bus sync handler. GStreamer calls it synchronously on
// whichever thread posted the message.
//
// It must not take r.mu: Play holds r.mu while provoking exactly these messages.
//
// It returns BusDrop for every message. Nothing else in this process reads this
// bus, and leaving messages queued on a bus with no watch is a slow leak.
func (r *returnPipeline) onBusMessage(_ gogst.Bus, msg *gogst.Message) gogst.BusSyncReply {
	if msg == nil || r.busSilenced.Load() {
		return gogst.BusDrop
	}

	switch msg.Type() {
	case gogst.MessageError:
		source := "return pipeline"
		if src := msg.Source(); src != nil {
			source = src.GetName()
		}
		debug, gerr := msg.ParseError()
		r.deliver(fmt.Errorf("gst: return monitor: %s: %v (%s)", source, gerr, debug))

	case gogst.MessageEOS:
		// EOS is a FAILURE on this path, not a normal end.
		//
		// srtsrc with auto-reconnect=false answers the loss of its peer with
		// end-of-stream rather than with an error — that is how a source element
		// says "there is no more data", and from libsrt's point of view a peer
		// that has gone away is indistinguishable from one that finished. The
		// return is meant to run for the whole match, so anything that ends it
		// is an outage, and treating EOS as anything other than a failure would
		// leave the machine in RECEIVING with a dead socket and a green lamp for
		// the rest of the afternoon.
		r.deliver(errors.New("gst: return monitor: the return stream ended (the SRT peer went away)"))

	case gogst.MessageWarning:
		debug, gerr := msg.ParseWarning()
		source := "return pipeline"
		if src := msg.Source(); src != nil {
			source = src.GetName()
		}
		// Logged, never delivered: a GStreamer warning is not a failure, and
		// putting it on Errors would make the reconnect machine tear down a
		// working monitor. Unlike the send path this logs inline rather than
		// through a goroutine — the return carries no live capture whose
		// timestamps a blocked streaming thread could bunch, so the cost of
		// log.Printf here is a few microseconds of a monitor, not of the feed.
		log.Printf("gst: return monitor: warning: %s: %v (%s)", source, gerr, debug)
	}

	return gogst.BusDrop
}

// deliver puts an error on the asynchronous channel without blocking, and
// without racing Close's close of it.
func (r *returnPipeline) deliver(err error) {
	r.errMu.RLock()
	defer r.errMu.RUnlock()
	if r.errsClosed {
		return
	}
	select {
	case r.errs <- err:
	default:
	}
}

// drainStartupError takes one error off the channel so that a failure during
// Play can be reported by Play rather than left for a consumer that, on this
// path, is about to throw the whole pipeline away. It never blocks.
func (r *returnPipeline) drainStartupError() error {
	r.errMu.RLock()
	defer r.errMu.RUnlock()
	if r.errsClosed {
		return nil
	}
	select {
	case err := <-r.errs:
		return err
	default:
		return nil
	}
}

// Errors returns the asynchronous error channel. See returnPipe.
func (r *returnPipeline) Errors() <-chan error { return r.errs }

// Close takes the pipeline to NULL and closes the error channel. See returnPipe.
func (r *returnPipeline) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil
	}
	r.closed = true
	return r.teardownLocked()
}

// teardownLocked takes the pipeline to NULL, releases every reference and closes
// the error channel. r.mu must be held. It is the only teardown path in this
// file and is reached both from Close and from Play's abort.
func (r *returnPipeline) teardownLocked() error {
	// Raise both flags before touching anything. From here a pad-added callback
	// returns immediately and the bus handler stops delivering, so nothing on a
	// streaming thread can add an element to a pipeline that is going away.
	r.tearing.Store(true)
	r.busSilenced.Store(true)

	var err error
	if r.pipeline != nil {
		stopWatchdog := stateChangeWatchdog("return monitor: pipeline to NULL")
		ret := r.pipeline.BlockSetState(gogst.StateNull, gogst.ClockTime(elementShutdownTimeout))
		stopWatchdog()
		if !stateChangeOK(ret) {
			err = fmt.Errorf("gst: return monitor: pipeline would not go to NULL (%s)", ret)
		}
	}

	// The bus sync handler is deliberately NOT detached. Detaching races a
	// streaming thread that is already inside it, and busSilenced above has
	// already made it a no-op. The bus goes away with the pipeline.
	r.pipeline = nil
	r.bus = nil
	r.src = nil
	r.demux = nil
	r.queue = nil
	r.queuePad = nil
	r.convert = nil
	r.sink = nil
	r.played = false

	// Release anything still waiting in awaitAudio. Play holds r.mu for the whole
	// of that wait, so teardownLocked cannot be running concurrently with it —
	// but Play's own abort path reaches here, and leaving a live channel behind
	// would let a later attempt observe a stale signal.
	r.padMu.Lock()
	r.fakeSinks = nil
	r.audioLinked = false
	r.audioSignalled = false
	r.audioReady = nil
	r.padMu.Unlock()

	r.errMu.Lock()
	if !r.errsClosed {
		r.errsClosed = true
		close(r.errs)
	}
	r.errMu.Unlock()

	return err
}

// padVideoMediaType returns a video pad's media type ("video/x-h264"), or ""
// when the pad carries no video or has not published caps yet.
//
// It is padMediaKind's fuller answer, and it lives beside it because it reads
// exactly the same caps. padMediaKind was only ever asked audio-or-video and
// threw the codec away — which is why the picture pipeline could not tell an
// H.264 return feed from an H.265 one, and simply failed to link its video pad
// every thirty seconds, for ever, while the same transport decoded fine under
// ffmpeg. See pictureParserFor.
func padVideoMediaType(pad gogst.Pad) string {
	caps := pad.GetCurrentCaps()
	if caps == nil {
		caps = pad.QueryCaps(nil)
	}
	if caps == nil {
		return ""
	}
	s := caps.GetStructure(0)
	if s == nil {
		return ""
	}
	name := s.GetName()
	if !strings.HasPrefix(name, "video/") {
		return ""
	}
	return name
}
