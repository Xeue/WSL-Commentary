//go:build cgo && !gststub

// picture_cgo.go is the go-gst backed picture pipeline. It compiles only with
// CGO_ENABLED=1 and needs the same Gate B toolchain gst_cgo.go does.
//
// Read picture.go first: it carries the contract, the reconnect machine, the
// measured caps and the reasoning for all three. This file builds one attempt's
// pipeline and nothing else.
//
// # It shares a package with two other paths and almost nothing else
//
// It reuses five things: resolveSinkHost, srtURI, stateChangeOK,
// stateChangeWatchdog and hasProperty from gst_cgo.go, because they are
// stateless functions about GStreamer and DNS rather than about the contribution
// feed; and fakeSinkName from return.go, because the rule it encodes — a
// dynamically added sink's element name must carry a per-pipeline sequence
// number or gst_bin_add silently refuses it — is a property of tsdemux, not of
// the return path, and restating it would be two places to get it wrong.
//
// It touches no field, no mutex and no channel of cgoPipeline or of
// returnPipeline, it never calls into either, and a failure here cannot reach
// them. A picture that fails must not be able to take the contribution feed off
// air or the commentator's audio out of their ears.
//
// # The one call go-gst does not have
//
// gst_video_overlay_set_window_handle is NOT bound by go-gst v0.0.2. Its
// generated GstVideoOverlay interface has Expose, HandleEvents,
// PrepareWindowHandle and SetRenderRectangle and stops there — girgen skips the
// setter, presumably over the guintptr handle argument. There is no property
// equivalent on d3d11videosink: it has render-rectangle and fullscreen, but the
// window it renders INTO is only reachable through that one function.
//
// Without it d3d11videosink creates its OWN top-level window, which is a second
// window the operator did not ask for, outside the layout, with its own title
// bar and its own close button. So this file declares the function itself, in
// the small cgo preamble below, and calls it on the raw GstElement pointer
// go-gst will hand over through UnsafeElementToGlibNone.
//
// That is the ONLY cgo in the application. It is four lines and it is here
// rather than in a helper package because the alternative — vendoring a patched
// go-gst — is a manifest change, and the manifests are frozen.
//
// # No libav, ever
//
// The decoder is d3d11h265dec: DXVA, in the GPU driver, wrapped by
// gst-plugins-bad under LGPL. avdec_h265 is present on the developer machine at
// primary rank and must not be selected; gst-libav is FFmpeg, which is the same
// commercial-shipping concern as x264enc, and build/bundle-gst.ps1 already
// refuses to copy anything matching *libav* or *avcodec*. Selecting it here
// would produce an application that works on this machine and fails to load a
// plugin on the installed one.
package gst

/*
#cgo pkg-config: gstreamer-video-1.0

#include <gst/gst.h>
#include <gst/video/videooverlay.h>

// wslcomms_set_window_handle is a thin wrapper so that the GST_VIDEO_OVERLAY
// cast macro is applied by the C compiler rather than being open-coded in Go.
// The macro is a checked cast in a debug build of GLib and a plain one
// otherwise; going through it is what makes a wrong element type a legible
// GLib critical rather than a jump through a garbage vtable.
static void wslcomms_set_window_handle(gpointer sink, guintptr handle) {
    gst_video_overlay_set_window_handle(GST_VIDEO_OVERLAY(sink), handle);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// Element names inside the picture pipeline. They are the names that appear as
// the source of a bus message, so they are chosen to be legible in a field log
// and to be impossible to confuse with the send path's (mux, srtq, slate, asrc,
// venc, vscale, srtout-N) or the return path's (retsrc, retdemux, ...).
const (
	namePicPipeline = "wslcomms-picture"
	namePicSrc      = "picsrc"   // srtsrc
	namePicDemux    = "picdemux" // tsdemux
	namePicQueue    = "picq"     // queue at the head of the video branch
	namePicParse    = "picparse" // h265parse
	namePicDecode   = "picdec"   // d3d11h265dec
	namePicSink     = "picsink"  // d3d11videosink
	namePicFakeSink = "picfake"  // fakesink for the audio pad
)

// pictureStateChangeTimeout bounds the asynchronous tail of the picture
// pipeline's NULL to PLAYING transition.
//
// Read the timeout commentary at the top of gst_cgo.go before relying on this:
// it does NOT bound the synchronous part. gst_element_set_state takes no timeout
// and runs every change_state function on the calling goroutine, so a D3D11
// device that will not come up hangs here for ever and no value written here
// changes that. stateChangeWatchdog makes such a hang legible in the log; it
// cannot cure it.
//
// What the number has to cover is srtsrc's caller connect, bounded by libsrt's
// default SRTO_CONNTIMEO of 3 s, plus creating a D3D11 device and a DXGI
// swapchain. Ten seconds is far beyond both and matches the other two paths so
// that all three fail on the same scale.
const pictureStateChangeTimeout = 10 * time.Second

// pictureErrorBuffer is how many asynchronous errors are held before further
// ones are dropped. It matches stubPictureErrorBuffer so the two builds behave
// identically under a slow consumer, and it drops rather than blocks because the
// sender of these errors is a GStreamer streaming thread and must never wait on
// a Go consumer.
const pictureErrorBuffer = 8

// pictureRequiredElements is every element factory this pipeline cannot be built
// without, mapped to the plugin from build/bundle-gst.ps1's allowlist that
// provides it.
//
// It is checked by newPicturePipe rather than by Init, for the reason
// returnRequiredElements gives: Init's list is the CONTRIBUTION path's and a
// failure there means the application cannot send and must say so at startup. A
// missing d3d11 means the commentator has no high-resolution picture — bad, and
// the fallback mosaic covers it — but it must not stop the feed going on air and
// it must not make Init fail.
//
// d3d11 is the entry that was not in the bundle before this work. It provides
// BOTH the decoder and the sink, so a single missing plugin takes the whole
// path; see the Why line on it in build/bundle-gst.ps1.
var pictureRequiredElements = []struct{ factory, plugin string }{
	{"srtsrc", "srt"},
	{"tsdemux", "mpegtsdemux"},
	{"queue", "coreelements"},
	{"fakesink", "coreelements"},
	{"h265parse", "videoparsersbad"},
	{"d3d11h265dec", "d3d11"},
	{"d3d11videosink", "d3d11"},
}

// pictureMissingElements returns a description of every required factory the
// registry does not have, or nil if the bundle is complete for this path.
func pictureMissingElements() []string {
	var missing []string
	for _, req := range pictureRequiredElements {
		if gogst.ElementFactoryFind(req.factory) == nil {
			missing = append(missing, fmt.Sprintf("%s (plugin %s)", req.factory, req.plugin))
		}
	}
	return missing
}

// picturePipeline is the go-gst backed picturePipe: one attempt's pipeline.
//
// # Threading model
//
// Four kinds of thread touch one of these, and the locking below only makes
// sense against this list. It is returnPipeline's model, restated because the
// reader of this file should not have to hold another file in their head.
//
//  1. The reconnect machine's goroutine, which calls Play and Close. Both take
//     mu for their whole duration, which means mu is held across GStreamer state
//     changes and therefore for seconds at a time.
//
//  2. Whichever GStreamer thread posts a bus message. gst_element_post_message
//     calls the sync handler synchronously, on the posting thread, before the
//     failing function has returned — so onBusMessage runs on a streaming thread
//     while Play may be holding mu, and MUST NOT take mu.
//
//  3. The demuxer's streaming thread, running onPadAdded, and the decoder's,
//     running the first-frame probe. Both take padMu, which is never held across
//     a pipeline state change and never held while mu is held.
//
//  4. The state-change watchdog's timer goroutine.
//
// Every GStreamer object kept across calls is stored in a field so it stays
// reachable: dropping a Go reference to a live GstElement would let go-gst's
// finalizer unref it.
type picturePipeline struct {
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
	decode    gogst.Element
	decSrcPad gogst.Pad
	sink      gogst.Element
	fakeSinks []gogst.Element

	// windowHandle is the HWND every attempt renders into, kept so the
	// prepare-window-handle bus message can be answered from a streaming thread
	// without reading PictureOpts, which lives on the caller's stack.
	//
	// It is an atomic because that answer happens on a streaming thread while
	// Play holds mu.
	windowHandle atomic.Uintptr

	// frameProbeID is the probe on the decoder's src pad, kept so teardown can
	// remove it. Zero means none is installed.
	frameProbeID uint32

	// fakeSeq numbers the fakesinks so that no two can ever be given the same
	// element name. It is an atomic rather than a padMu-guarded counter because
	// it is incremented on the demuxer's streaming thread and read nowhere else;
	// see fakeSinkName in return.go for what a repeated name costs.
	fakeSeq atomic.Uint64

	// padMu guards videoLinked, frameReady, frameSignalled and fakeSinks, all
	// touched from a streaming thread.
	padMu       sync.Mutex
	videoLinked bool

	// frameReady is closed by the decoder's src-pad probe the moment a decoded
	// frame appears. It is what Play waits on, and it is recreated for each
	// address attempt so that one address's success cannot be read as another's.
	//
	// It is deliberately NOT cleared when it is closed, and frameSignalled is a
	// separate flag rather than a nil check, because the probe can and does fire
	// DURING the BlockSetState that precedes the wait. Clearing the field on
	// signal would leave awaitFrame reading nil, selecting on a channel that can
	// never fire, and waiting the full pictureFirstFrameTimeout for a picture
	// that was already on screen. A closed channel is permanently ready, so a
	// late reader sees the signal immediately.
	frameReady     chan struct{}
	frameSignalled bool

	// tearing is raised by Close before it touches the pipeline, so that a
	// pad-added callback arriving on a streaming thread mid-teardown does not add
	// an element to a pipeline that is going to NULL.
	tearing atomic.Bool

	// busSilenced makes onBusMessage return immediately once teardown has begun.
	// It replaces detaching the sync handler, which is not something to do while
	// a streaming thread may be inside it.
	busSilenced atomic.Bool

	// errMu guards errs and errsClosed. It exists only so that a streaming thread
	// cannot send on a channel Close is closing, and is never held across
	// anything that can block.
	errMu      sync.RWMutex
	errs       chan error
	errsClosed bool
}

var _ picturePipe = (*picturePipeline)(nil)

// newPicturePipe creates a picturePipeline. See picture.go.
//
// It refuses early, with a message naming the missing plugin, rather than
// letting the failure surface as a nil from gst_element_factory_make three
// screens further down. The message names build/bundle-gst.ps1 because a missing
// d3d11 in the field is a bundling mistake and nothing else: the plugin ships
// with GStreamer and is present on every machine that has the GPU driver this
// application requires anyway.
func newPicturePipe() (picturePipe, error) {
	if !inited.Load() {
		return nil, errors.New("gst: picture monitor: Init has not been called")
	}
	if missing := pictureMissingElements(); len(missing) > 0 {
		return nil, fmt.Errorf(
			"gst: picture monitor: the bundled GStreamer is missing %s "+
				"(add the plugin to the allowlist in build/bundle-gst.ps1 and re-run it)",
			strings.Join(missing, ", "))
	}
	return &picturePipeline{errs: make(chan error, pictureErrorBuffer)}, nil
}

// Play builds the pipeline and takes it to PLAYING. See picturePipe.
func (p *picturePipeline) Play(opts PictureOpts) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return errors.New("gst: picture pipeline is closed")
	}
	if p.played {
		return errors.New("gst: picture pipeline already played")
	}
	if opts.WindowHandle == 0 {
		// PictureOpts.normalise already refuses this, and the monitor normalises
		// before it ever reaches a pipe. It is restated because the consequence
		// of getting here with a zero handle is d3d11videosink opening its own
		// top-level window on the operator's screen, mid-match, which is a far
		// worse outcome than an error.
		return errors.New("gst: picture monitor: no window handle; refusing to let the sink open its own window")
	}
	p.windowHandle.Store(opts.WindowHandle)

	// Resolve the host to a literal before building anything.
	//
	// THIS IS NOT TIDINESS. Taking srtsrc to NULL while GLib's threaded resolver
	// has a lookup in flight trips
	//
	//	GLib-GIO:ERROR:gthreadedresolver.c:1487:cancelled_cb: assertion failed
	//
	// which aborts the PROCESS — the contribution feed, the audio, the lot. Both
	// gst_cgo.go and return_cgo.go resolve first for exactly this reason and it
	// is measured, not inferred. Nothing but an IP literal ever reaches the
	// element. It costs nothing besides: the lookup is bounded at
	// hostResolveTimeout and happens on every attempt, so an M2L-X instance that
	// moves address mid-match is picked up by the next reconnect.
	addrs, err := resolveSinkHost(opts.Host, hostResolveTimeout)
	if err != nil {
		return err
	}

	element := gogst.NewPipeline(namePicPipeline)
	if element == nil {
		return errors.New("gst: picture monitor: gst_pipeline_new returned nil")
	}
	pipeline, ok := element.(gogst.Pipeline)
	if !ok {
		return fmt.Errorf("gst: picture monitor: gst_pipeline_new returned a %T, not a GstPipeline", element)
	}
	p.pipeline = pipeline

	// From here on any failure must tear down what has been built, or the SRT
	// socket and the D3D11 device stay open and the next attempt cannot have
	// them.
	abort := func(err error) error {
		p.teardownLocked()
		return err
	}

	if err := p.buildLocked(opts); err != nil {
		return abort(err)
	}

	// The bus sync handler is attached before the first state change, so that an
	// error raised during NULL to PLAYING is captured rather than lost AND so
	// that the prepare-window-handle message is answered. It is a sync handler
	// rather than a watch for the same reason the other two paths' are: a watch
	// needs a GLib main loop and this process does not have one, because Wails
	// owns the Windows message loop.
	p.bus = pipeline.GetBus()
	if p.bus == nil {
		return abort(errors.New("gst: picture monitor: pipeline has no bus"))
	}
	p.busSilenced.Store(false)
	p.bus.SetSyncHandler(p.onBusMessage)

	// A name may front several addresses. Try them in the order resolveSinkHost
	// returned — IPv4 first — and say which one answered.
	var lastErr error
	for i, addr := range addrs {
		where := srtURI(addr, opts.Port)
		if len(addrs) > 1 {
			where = fmt.Sprintf("%s, address %d of %d", where, i+1, len(addrs))
		}

		// Each attempt starts clean. Anything the previous address posted is
		// discarded, and a fresh frameReady is armed, so that neither can be read
		// as belonging to this one.
		p.drainErrors()
		p.armFrameReady()

		if err := p.configureSrcLocked(opts, addr); err != nil {
			return abort(err)
		}

		stopWatchdog := stateChangeWatchdog("picture monitor: pipeline to PLAYING for " + where)
		ret := pipeline.BlockSetState(gogst.StatePlaying, gogst.ClockTime(pictureStateChangeTimeout))
		stopWatchdog()
		if !stateChangeOK(ret) {
			lastErr = fmt.Errorf("gst: picture monitor: could not start the pipeline for %s (%s)", where, ret)
			if busErr := p.drainStartupError(); busErr != nil {
				lastErr = fmt.Errorf("%w: %v", lastErr, busErr)
			}
			p.toNullForRetry()
			continue
		}

		// PLAYING proves NOTHING on this path; see pictureFirstFrameTimeout.
		if err := p.awaitFrame(where); err != nil {
			lastErr = err
			p.toNullForRetry()
			continue
		}

		lastErr = nil
		break
	}
	if lastErr != nil {
		return abort(lastErr)
	}

	p.played = true
	log.Printf("gst: picture monitor: decoding pictures from %s", opts.endpointForLog())
	return nil
}

// awaitFrame blocks until the decoder has produced a frame, the pipeline reports
// a failure, or pictureFirstFrameTimeout expires. It is the whole reason Play is
// honest; see the constant.
//
// p.mu is held throughout, which is fine: nothing that has to make progress
// takes it. The bus handler, the pad-added callback and the frame probe all
// deliberately avoid it, precisely so this wait is possible.
func (p *picturePipeline) awaitFrame(where string) error {
	p.padMu.Lock()
	ready := p.frameReady
	p.padMu.Unlock()
	if ready == nil {
		// armFrameReady runs immediately before every call, so this cannot
		// happen. It is handled rather than assumed because the alternative is a
		// select on a nil channel, which is a wait that only the timeout ends.
		return errors.New("gst: picture monitor: internal error: no frame wait was armed")
	}

	timeout := time.NewTimer(pictureFirstFrameTimeout)
	defer timeout.Stop()

	select {
	case <-ready:
		return nil

	case err, ok := <-p.errs:
		// A bus error during the connect — in practice srtsrc's "Connection
		// timeout (16)" when nothing is listening on the far end, or the decoder
		// refusing to create a DXVA context. It is returned rather than left on
		// the channel because the caller is about to throw this whole pipeline
		// away, and an error nobody reads is an outage with no explanation.
		if !ok || err == nil {
			return fmt.Errorf("gst: picture monitor: %s failed during startup", where)
		}
		return err

	case <-timeout.C:
		// Connected, or at least not visibly failed, but no frame ever came out
		// of the decoder. The far end is listening and sending nothing, the
		// output carries no video, or the decoder is quietly refusing this
		// stream. Either way it is a failed attempt and not a picture to report
		// as healthy.
		return fmt.Errorf(
			"gst: picture monitor: no decoded frame arrived from %s within %s "+
				"(is that M2L-X output started, and does it carry H.265?)",
			where, pictureFirstFrameTimeout)
	}
}

// armFrameReady creates a fresh frameReady channel for one address attempt.
func (p *picturePipeline) armFrameReady() {
	p.padMu.Lock()
	p.videoLinked = false
	p.frameSignalled = false
	p.frameReady = make(chan struct{})
	p.padMu.Unlock()
}

// signalFrameReady closes the current frameReady channel, exactly once per
// attempt. It runs on the decoder's streaming thread.
func (p *picturePipeline) signalFrameReady() {
	p.padMu.Lock()
	if p.frameReady != nil && !p.frameSignalled {
		p.frameSignalled = true
		close(p.frameReady)
	}
	p.padMu.Unlock()
}

// toNullForRetry takes the pipeline back to NULL between address attempts, so
// that a failed srtsrc's socket is released rather than left half-open while
// another connect is attempted from the same pipeline.
//
// It also takes the previous attempt's fakesinks back out of the bin. THE
// PIPELINE IS REUSED ACROSS ADDRESSES, and everything the last attempt's demuxer
// built is still in it: sinks linked to pads that no longer exist, which the next
// attempt has no use for. fakeSinkName already guarantees the names are
// distinct, so this is not what keeps address 2 working — it is what stops the
// bin growing a sink per undecoded pad per address for the life of an M2L-X
// instance with several A records.
//
// The order is fixed: the pipeline reaches NULL first, which stops the streaming
// threads and brings every child to NULL with it, and only then are the elements
// removed. gst_bin_remove unlinks the pads it finds linked, but removing an
// element from a bin that is still rolling is a race with the thread pushing
// into it.
func (p *picturePipeline) toNullForRetry() {
	if p.pipeline == nil {
		return
	}
	p.pipeline.BlockSetState(gogst.StateNull, gogst.ClockTime(elementShutdownTimeout))

	p.padMu.Lock()
	stale := p.fakeSinks
	p.fakeSinks = nil
	p.padMu.Unlock()

	for _, fake := range stale {
		if fake == nil {
			continue
		}
		if !p.pipeline.Remove(fake) {
			// Logged, not delivered. The next attempt is not harmed by a sink
			// left behind — its own names are unique — so this is a hygiene
			// failure, and putting it on Errors would abort a reconnect that can
			// still succeed.
			log.Printf("gst: picture monitor: could not remove %s from the pipeline between attempts",
				fake.GetName())
		}
	}
}

// drainErrors discards everything currently queued, without blocking.
func (p *picturePipeline) drainErrors() {
	p.errMu.RLock()
	defer p.errMu.RUnlock()
	if p.errsClosed {
		return
	}
	for {
		select {
		case <-p.errs:
		default:
			return
		}
	}
}

// buildLocked creates, adds and links everything that can be linked statically.
// p.mu must be held and p.pipeline must be set.
//
// Every element is created, added and linked BEFORE the pipeline is started, so
// the only thing left for the pad-added callback to do is a single
// gst_pad_link. That keeps the work done on a streaming thread to the minimum:
// adding elements and syncing their state from inside a pad-added callback is
// legal but is the part of the dynamic-pad idiom that goes wrong, and the video
// branch does not need it.
func (p *picturePipeline) buildLocked(opts PictureOpts) error {
	type spec struct {
		factory string
		name    string
		out     *gogst.Element
	}
	var parse gogst.Element
	specs := []spec{
		{"srtsrc", namePicSrc, &p.src},
		{"tsdemux", namePicDemux, &p.demux},
		{"queue", namePicQueue, &p.queue},
		{"h265parse", namePicParse, &parse},
		{"d3d11h265dec", namePicDecode, &p.decode},
		{"d3d11videosink", namePicSink, &p.sink},
	}
	for _, s := range specs {
		el := gogst.ElementFactoryMake(s.factory, s.name)
		if el == nil {
			return fmt.Errorf("gst: picture monitor: could not create %s", s.factory)
		}
		if !p.pipeline.Add(el) {
			return fmt.Errorf("gst: picture monitor: could not add %s to the pipeline", s.name)
		}
		*s.out = el
	}

	// srtsrc to tsdemux is a static link on both sides and is made here. Every
	// link from the demuxer onwards is dynamic; see onPadAdded.
	if !p.src.Link(p.demux) {
		return fmt.Errorf("gst: picture monitor: could not link %s to %s", namePicSrc, namePicDemux)
	}

	// The video branch, in order. All static pads.
	video := []gogst.Element{p.queue, parse, p.decode, p.sink}
	for i := 0; i+1 < len(video); i++ {
		if !video[i].Link(video[i+1]) {
			return fmt.Errorf("gst: picture monitor: could not link the video branch at element %d "+
				"(%s to %s)", i, video[i].GetName(), video[i+1].GetName())
		}
	}

	// The queue's sink pad is the one the demuxer's video pad links to. It is
	// captured here rather than looked up in the callback, because the callback
	// runs on a streaming thread and a field cleared by teardownLocked would be a
	// data race on exactly the path being torn down.
	p.queuePad = p.queue.GetStaticPad("sink")
	if p.queuePad == nil {
		return errors.New("gst: picture monitor: " + namePicQueue + " has no sink pad")
	}

	// The first-frame probe. This is what makes Play honest, and it is on the
	// DECODER's src pad rather than the sink's, deliberately: a buffer arriving
	// at the sink has already been through everything that can fail, but a sink
	// that is dropping every frame as late would never chain one, and a probe
	// there would then time out on a stream that is decoding perfectly well. The
	// decoder's output is the narrowest place that proves the whole chain works.
	p.decSrcPad = p.decode.GetStaticPad("src")
	if p.decSrcPad == nil {
		return errors.New("gst: picture monitor: " + namePicDecode + " has no src pad")
	}
	p.frameProbeID = p.decSrcPad.AddProbe(gogst.PadProbeTypeBuffer,
		func(_ gogst.Pad, _ *gogst.PadProbeInfo) gogst.PadProbeReturn {
			// Runs on the decoder's streaming thread, once per frame, at 50 Hz
			// for the life of the session. It must therefore be as close to free
			// as a Go callback can be after the first frame: signalFrameReady
			// takes an uncontended leaf mutex and returns on a boolean. It is NOT
			// removed after the first frame, because removing a probe from inside
			// itself is a documented deadlock against the pad's stream lock, and
			// 50 uncontended mutex acquisitions a second is not a cost worth
			// taking that risk for.
			p.signalFrameReady()
			return gogst.PadProbeOK
		})
	if p.frameProbeID == 0 {
		return errors.New("gst: picture monitor: could not add the first-frame probe to " + namePicDecode)
	}

	// sync=false. THE SINGLE BIGGEST THING THIS PIPELINE DOES ABOUT LATENCY, and
	// the whole argument for it — including the measured 993.7 ms it removes, and
	// the one condition under which it becomes wrong again — is at
	// pictureSinkSync in picture.go rather than restated here.
	//
	// It is set unconditionally, with no hasProperty guard, because "sync" is
	// GstBaseSink's own property: every sink in GStreamer has it, and a guard here
	// would turn a renamed or replaced sink into a silent return of the second
	// this line exists to remove. If it is ever missing, that is a fault to see.
	p.sink.SetObjectProperty("sync", pictureSinkSync)

	// qos=false, decided together with sync above and argued at pictureSinkQoS.
	// Also a GstBaseSink property, also unguarded, for the same reason.
	p.sink.SetObjectProperty("qos", pictureSinkQoS)

	// force-aspect-ratio: the picture is 16:9 and the rectangle the page gives
	// the overlay may not be. Letterboxing inside the rectangle is right;
	// stretching a football pitch is not, and a commentator judging a distance
	// off a stretched picture is a real error rather than an aesthetic one.
	if hasProperty(p.sink, "force-aspect-ratio") {
		p.sink.SetObjectProperty("force-aspect-ratio", true)
	}

	// enable-navigation-events off. The sink is inside a child window that sits
	// over the page; letting it turn mouse movement into upstream navigation
	// events sends them to srtsrc, which has nothing to do with them, and it is
	// one more reason for the sink to want the input focus it must never take
	// from the page.
	if hasProperty(p.sink, "enable-navigation-events") {
		p.sink.SetObjectProperty("enable-navigation-events", false)
	}

	// fullscreen-toggle-mode is left at "none", its default, and that is stated
	// rather than assumed: the other values make the sink react to Alt+Enter and
	// to double-clicks by taking the window full screen. On a commentary position
	// with one window and no menu, a picture that silently goes full screen over
	// the controls mid-match is unrecoverable without the keyboard shortcut
	// nobody knows.

	// The window handle, set BEFORE the sink is ever taken out of NULL.
	//
	// If it is not set by the time the sink needs somewhere to draw, gstd3d11
	// creates its own top-level window — a second window, outside the layout,
	// with its own title bar. Setting it here is the documented and
	// well-travelled order; answering the prepare-window-handle message in
	// onBusMessage is the belt to this pair of braces, and both are wired
	// because the failure is not recoverable once it has happened.
	p.setWindowHandleLocked(p.sink, opts.WindowHandle)

	// The pipeline and the queue's sink pad are captured HERE, into the closure,
	// rather than read from p inside the callback. See onPadAdded for why that is
	// load-bearing rather than stylistic.
	pipeline, queuePad := p.pipeline, p.queuePad
	p.demux.ConnectPadAdded(func(_ gogst.Element, pad gogst.Pad) {
		p.onPadAdded(pipeline, queuePad, pad)
	})
	return nil
}

// setWindowHandleLocked hands the HWND to a GstVideoOverlay.
//
// This is the one cgo call in the application; see the file header for why it
// cannot go through go-gst. UnsafeElementToGlibNone borrows the pointer without
// touching the reference count, which is correct here because sink is held in a
// field for the life of the pipeline and gst_video_overlay_set_window_handle
// does not retain it — it stores the integer and returns.
//
// runtime.KeepAlive is not written out because sink is reachable through p.sink
// for the whole call; the go-gst generated bindings write it for locals and this
// is not one.
func (p *picturePipeline) setWindowHandleLocked(sink gogst.Element, handle uintptr) {
	if sink == nil || handle == 0 {
		return
	}
	ptr := gogst.UnsafeElementToGlibNone(sink)
	if ptr == nil {
		log.Printf("gst: picture monitor: the sink has no underlying GObject; the window handle was not set")
		return
	}
	C.wslcomms_set_window_handle(C.gpointer(ptr), C.guintptr(handle))
	log.Printf("gst: picture monitor: rendering into window handle 0x%x", handle)
}

// pictureWindowHandleMessage is the name of the GstVideoOverlay element message
// that asks the application for a window.
//
// gst_is_video_overlay_prepare_window_handle_message is exactly a NULL-src check
// plus this string comparison, and it lives in the gstvideo package rather than
// the gst one this file already uses, so it is written out with
// gst_message_has_name rather than pulling in a second large generated package
// for one predicate. The MessageElement type check above is the src check's
// equivalent: only an element posts one.
const pictureWindowHandleMessage = "prepare-window-handle"

// configureSrcLocked sets every srtsrc property for one address. p.mu must be
// held.
func (p *picturePipeline) configureSrcLocked(opts PictureOpts, addr string) error {
	if err := setStringProperty(p.src, "uri", srtURI(addr, opts.Port)); err != nil {
		return fmt.Errorf("gst: picture monitor: %w", err)
	}

	// mode=caller. Every M2L-X output has reverse=True, which means M2L-X LISTENS
	// and we dial in. It is set explicitly rather than relied on as the default,
	// and by nickname through gst_util_set_object_arg because the property is a
	// GEnum.
	if hasProperty(p.src, "mode") {
		gogst.UtilSetObjectArg(p.src, "mode", "caller")
	}

	if hasProperty(p.src, "latency") {
		p.src.SetObjectProperty("latency", int32(opts.LatencyMs))
	} else {
		log.Printf("gst: picture monitor: srtsrc has no latency property; using the element default")
	}

	// srtsrc's own auto-reconnect is turned off, for the reason the other two
	// paths turn theirs off: it reopens immediately with no backoff, straight into
	// M2L-X's roughly five second re-accept refusal window, and it does it without
	// telling anyone — so the picture would stay reported as SHOWING through an
	// outage the commentator can see. The backoff ladder in picture.go is the only
	// reconnect on this path.
	if hasProperty(p.src, "auto-reconnect") {
		p.src.SetObjectProperty("auto-reconnect", false)
	}

	// The passphrase is set with g_object_set and never placed in the URI, so it
	// is neither percent-encoded nor logged. Nothing in this file formats it, and
	// PictureOpts.endpointForLog exists so that nothing is tempted to.
	if opts.Passphrase != "" {
		if err := setStringProperty(p.src, "passphrase", opts.Passphrase); err != nil {
			return fmt.Errorf("gst: picture monitor: the SRT passphrase could not be set: %w", err)
		}
		if hasProperty(p.src, "pbkeylen") {
			p.src.SetObjectProperty("pbkeylen", int32(opts.PBKeyLen))
		}
	}
	return nil
}

// onPadAdded links one of the demuxer's dynamic pads. It runs on the demuxer's
// streaming thread.
//
// The video pad goes to the video branch, which is already built, linked and in
// PLAYING, so this is a single gst_pad_link and no state change. Everything else
// — in practice the AAC audio, which on this path is thrown away because the
// audio comes from Kinesis — gets its own fakesink sync=false.
//
// THE FAKESINK IS NOT TIDINESS. An unlinked src pad on a demuxer is not inert:
// gst_pad_push returns GST_FLOW_NOT_LINKED, mpegtsbase aggregates the flow
// returns of its pads, and NOT_LINKED from one pad is tolerated only while
// another is returning OK. The moment the video branch back-pressures — which it
// will, because d3d11videosink is a clock-synced sink and the whole chain paces
// to it — the aggregate becomes NOT_LINKED, the demuxer pauses its task and
// posts an error. The symptom is a picture that plays for a second or two and
// then stops, which looks exactly like a network problem and is not. return.go
// learned this on the other pad and the discipline is the same one.
//
// sync=false is the other half and matters more than the element does. A
// fakesink with the default sync=true waits on the pipeline clock for the running
// time of every buffer, and would hold the demuxer's audio streaming thread
// against the clock while the video branch ran ahead of it. sync=false makes it
// consume and discard as fast as it is fed, which is what "throw the audio away"
// actually requires.
//
// pipeline and queuePad are PARAMETERS, captured by the closure buildLocked
// installs, rather than reads of p.pipeline and p.queuePad. Those fields are
// cleared by teardownLocked on the caller's goroutine, and this function runs on
// a streaming thread, so reading them here would be a data race on precisely the
// path being torn down. The tearing flag narrows the window but does not close
// it; passing the values closes it.
func (p *picturePipeline) onPadAdded(pipeline gogst.Pipeline, queuePad gogst.Pad, pad gogst.Pad) {
	if p.tearing.Load() {
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

	if kind == "video" {
		p.padMu.Lock()
		already := p.videoLinked
		if !already {
			p.videoLinked = true
		}
		p.padMu.Unlock()

		if already {
			// A transport carrying two video streams. The first one wins and the
			// rest are discarded, because there is one picture and picking
			// arbitrarily between streams would be worse than picking the first.
			// They still need a sink; an unlinked pad wedges the demuxer just as
			// the audio one would.
			log.Printf("gst: picture monitor: %s is a second video stream; discarding it", name)
			p.fakeSinkFor(pipeline, pad)
			return
		}

		if ret := pad.Link(queuePad); ret != gogst.PadLinkOK {
			// Undo the claim, so that a later video pad — a PMT update, or the
			// second stream of a transport whose first one could not be linked —
			// still gets a chance at the branch.
			p.padMu.Lock()
			p.videoLinked = false
			p.padMu.Unlock()

			p.deliver(fmt.Errorf("gst: picture monitor: could not link %s to %s (%s)",
				name, namePicQueue, ret))
			return
		}

		log.Printf("gst: picture monitor: linked demuxer pad %s to the video branch", name)
		return
	}

	log.Printf("gst: picture monitor: discarding demuxer pad %s (%s); the audio comes from Kinesis",
		name, kind)
	p.fakeSinkFor(pipeline, pad)
}

// fakeSinkFor attaches a fakesink sync=false to a pad this path does not use. It
// runs on a streaming thread.
//
// The element is added to the pipeline, brought up with SyncStateWithParent and
// only then linked. That order matters: linking an element that has no parent
// does not give it the pipeline's clock or base time, and a sink still in NULL
// when a buffer arrives returns GST_FLOW_FLUSHING — which the demuxer aggregates
// exactly as it would the NOT_LINKED this whole function exists to prevent.
//
// A failure here is delivered on Errors rather than logged and forgotten,
// because its consequence is the demuxer wedging a few seconds later and the
// operator being told nothing.
func (p *picturePipeline) fakeSinkFor(pipeline gogst.Pipeline, pad gogst.Pad) {
	// The name carries a per-pipeline sequence number, NOT just the pad name.
	// tsdemux names pads after the PID, one pipeline is retried against every
	// address resolveSinkHost returned, and gst_bin_add refuses a duplicate name
	// — so a name derived from the pad alone makes every address after the first
	// fail to sink its unused pads, and an unsinked pad wedges the demuxer. See
	// fakeSinkName in return.go, which this shares.
	name := fakeSinkName(namePicFakeSink, p.fakeSeq.Add(1), pad.GetName())
	fake := gogst.ElementFactoryMake("fakesink", name)
	if fake == nil {
		p.deliver(errors.New("gst: picture monitor: could not create fakesink for " + pad.GetName()))
		return
	}
	fake.SetObjectProperty("sync", false)
	// async=false: a sink that has not prerolled holds the whole pipeline in
	// ASYNC. This one is never going to be interesting and must not be able to
	// hold the video branch out of PLAYING.
	if hasProperty(fake, "async") {
		fake.SetObjectProperty("async", false)
	}

	if !pipeline.Add(fake) {
		p.deliver(errors.New("gst: picture monitor: could not add " + name + " to the pipeline"))
		return
	}
	// Recorded only once it is IN the bin, because the list means "elements this
	// pipeline put in the bin and must take back out" — see toNullForRetry.
	p.padMu.Lock()
	p.fakeSinks = append(p.fakeSinks, fake)
	p.padMu.Unlock()

	if !fake.SyncStateWithParent() {
		p.deliver(errors.New("gst: picture monitor: " + name + " would not sync state with the pipeline"))
		return
	}
	sinkPad := fake.GetStaticPad("sink")
	if sinkPad == nil {
		p.deliver(errors.New("gst: picture monitor: " + name + " has no sink pad"))
		return
	}
	if ret := pad.Link(sinkPad); ret != gogst.PadLinkOK {
		p.deliver(fmt.Errorf("gst: picture monitor: could not link %s to %s (%s)",
			pad.GetName(), name, ret))
	}
}

// onBusMessage is the bus sync handler. GStreamer calls it synchronously on
// whichever thread posted the message.
//
// It must not take p.mu: Play holds p.mu while provoking exactly these messages.
//
// It returns BusDrop for every message. Nothing else in this process reads this
// bus, and leaving messages queued on a bus with no watch is a slow leak.
func (p *picturePipeline) onBusMessage(_ gogst.Bus, msg *gogst.Message) gogst.BusSyncReply {
	if msg == nil || p.busSilenced.Load() {
		return gogst.BusDrop
	}

	switch msg.Type() {
	case gogst.MessageElement:
		// prepare-window-handle. The sink is on a streaming thread, about to
		// create somewhere to draw, asking whether the application has a window
		// for it. Answering here is the documented contract and it MUST happen
		// synchronously, inside this handler, before the message is returned:
		// once the handler returns without an answer the sink creates its own
		// top-level window and there is no taking it back.
		//
		// The handle was already given to the sink in buildLocked. This is the
		// second of two chances, and it is wired because the cost of the first
		// one being wrong is a stray window on the operator's screen mid-match.
		if msg.HasName(pictureWindowHandleMessage) {
			handle := p.windowHandle.Load()
			if handle == 0 {
				log.Printf("gst: picture monitor: the sink asked for a window and there is none")
				return gogst.BusDrop
			}
			if src, ok := msg.Source().(gogst.Element); ok && src != nil {
				p.setWindowHandleLocked(src, handle)
			}
		}

	case gogst.MessageError:
		source := "picture pipeline"
		if src := msg.Source(); src != nil {
			source = src.GetName()
		}
		debug, gerr := msg.ParseError()
		p.deliver(fmt.Errorf("gst: picture monitor: %s: %v (%s)", source, gerr, debug))

	case gogst.MessageEOS:
		// EOS is a FAILURE on this path, not a normal end.
		//
		// srtsrc with auto-reconnect=false answers the loss of its peer with
		// end-of-stream rather than with an error — that is how a source element
		// says "there is no more data", and from libsrt's point of view a peer
		// that has gone away is indistinguishable from one that finished. The
		// picture is meant to run for the whole match, so anything that ends it is
		// an outage. Treating EOS as anything other than a failure would leave the
		// machine in SHOWING with a dead socket and the last decoded frame frozen
		// on screen for the rest of the afternoon, which is the single most
		// dangerous thing this path could do: a commentator cannot tell a frozen
		// picture from a still one.
		p.deliver(errors.New("gst: picture monitor: the picture stream ended (the SRT peer went away)"))

	case gogst.MessageWarning:
		debug, gerr := msg.ParseWarning()
		source := "picture pipeline"
		if src := msg.Source(); src != nil {
			source = src.GetName()
		}
		// Logged, never delivered: a GStreamer warning is not a failure, and
		// putting it on Errors would make the reconnect machine tear down a
		// working picture. tsdemux posts a CONTINUITY warning per lost transport
		// packet — 11 of them in a measured 25 second run — so this is a path that
		// warns routinely and must not be torn down for it.
		log.Printf("gst: picture monitor: warning: %s: %v (%s)", source, gerr, debug)
	}

	return gogst.BusDrop
}

// deliver puts an error on the asynchronous channel without blocking, and
// without racing Close's close of it.
func (p *picturePipeline) deliver(err error) {
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

// drainStartupError takes one error off the channel so that a failure during Play
// can be reported by Play rather than left for a consumer that, on this path, is
// about to throw the whole pipeline away. It never blocks.
func (p *picturePipeline) drainStartupError() error {
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

// Errors returns the asynchronous error channel. See picturePipe.
func (p *picturePipeline) Errors() <-chan error { return p.errs }

// Close takes the pipeline to NULL and closes the error channel. See
// picturePipe.
func (p *picturePipeline) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}
	p.closed = true
	return p.teardownLocked()
}

// teardownLocked takes the pipeline to NULL, releases every reference and closes
// the error channel. p.mu must be held. It is the only teardown path in this file
// and is reached both from Close and from Play's abort.
//
// It does NOT destroy the window. The window is owned by overlay_windows.go and
// outlives every pipeline; destroying it here would mean a reconnect had nowhere
// to draw, and would put a window destroy on a goroutine that does not own the
// window's message queue.
func (p *picturePipeline) teardownLocked() error {
	// Raise both flags before touching anything. From here a pad-added callback
	// returns immediately and the bus handler stops delivering, so nothing on a
	// streaming thread can add an element to a pipeline that is going away.
	p.tearing.Store(true)
	p.busSilenced.Store(true)

	// The frame probe is removed BEFORE the state change rather than after.
	// gst_pad_remove_probe waits for any concurrent callback to return, which is
	// safe from here — this is the caller's goroutine and it holds nothing the
	// probe wants — and doing it after the pipeline has reached NULL would leave
	// the window between NULL and the removal in which the pad is gone and the
	// probe ID names nothing.
	if p.decSrcPad != nil && p.frameProbeID != 0 {
		p.decSrcPad.RemoveProbe(p.frameProbeID)
	}
	p.frameProbeID = 0

	var err error
	if p.pipeline != nil {
		stopWatchdog := stateChangeWatchdog("picture monitor: pipeline to NULL")
		ret := p.pipeline.BlockSetState(gogst.StateNull, gogst.ClockTime(elementShutdownTimeout))
		stopWatchdog()
		if !stateChangeOK(ret) {
			err = fmt.Errorf("gst: picture monitor: pipeline would not go to NULL (%s)", ret)
		}
	}

	// The bus sync handler is deliberately NOT detached. Detaching races a
	// streaming thread that is already inside it, and busSilenced above has
	// already made it a no-op. The bus goes away with the pipeline.
	p.pipeline = nil
	p.bus = nil
	p.src = nil
	p.demux = nil
	p.queue = nil
	p.queuePad = nil
	p.decode = nil
	p.decSrcPad = nil
	p.sink = nil
	p.played = false

	p.padMu.Lock()
	p.fakeSinks = nil
	p.videoLinked = false
	p.frameSignalled = false
	p.frameReady = nil
	p.padMu.Unlock()

	p.errMu.Lock()
	if !p.errsClosed {
		p.errsClosed = true
		close(p.errs)
	}
	p.errMu.Unlock()

	return err
}
