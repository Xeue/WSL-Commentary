// picture.go is the SRT PICTURE path: the commentator's programme monitor.
//
// Owner: WP-P. New file. It does not touch gst.go, gst_cgo.go or gst_stub.go,
// which are the contribution path, and it does not touch return.go or
// return_cgo.go, which are the SRT AUDIO return. It borrows their shape and
// none of their state.
//
// # The one sentence this file exists to make true
//
// THE COMMENTATOR'S PICTURE COMES FROM SRT. THE AUDIO COMES FROM KINESIS.
//
// That is worth stating in capitals because the first attempt at SRT on this
// machine built it as an AUDIO path — return.go — which is the opposite, and
// which is why selecting "SRT" as the return source today takes the operator's
// audio away. This file is the other half of undoing that. It dials the same
// kind of M2L-X output, over the same protocol, with the same reconnect
// discipline, and it throws the AUDIO away and keeps the PICTURE.
//
// # The pipeline
//
//	srtsrc uri=srt://<ip>:40501 mode=caller latency=<ms>
//	  ! tsdemux
//	  ! h265parse ! <the platform's hardware H.265 decoder>
//	  ! <the platform's video sink, rendering into the overlay surface>
//
// The two angled brackets are d3d11h265dec into d3d11videosink on Windows and
// vtdec_hw into glimagesink on macOS. Nothing else in the pipeline changes, and
// picture_cgo.go's header carries the table and the measurements for both.
//
// The audio pad from tsdemux gets a fakesink sync=false, for exactly the reason
// return_cgo.go gives for the video pad: an unlinked src pad on a demuxer is not
// inert, mpegtsbase aggregates NOT_LINKED across its pads, and the whole
// transport stops a few seconds in. The two files therefore sink the pad they do
// not want in the same way, with the same per-attempt sequence number in the
// element name, and fakeSinkName in return.go is shared between them rather than
// restated. That sharing is deliberate and it is the ONLY thing this path takes
// from the return path.
//
// # What was measured, and where
//
// Dialled by hand at the live instance on 2026-08-07, port 40501, for 25 s:
//
//	tsdemux video pad   video/x-h265, stream-format=byte-stream
//	h265parse src       video/x-h265, stream-format=hev1, width=1920,
//	                    height=1080, framerate=50/1, profile=main, level=4.1,
//	                    colorimetry=bt709, 4:2:0 8-bit
//	d3d11h265dec src    video/x-raw(memory:D3D11Memory), NV12, 1920x1080, 50/1
//	                    on "NVIDIA GeForce RTX 5070", hardware=true, DXVA
//	frames              1178 decoded, PTS 0.816 s to 24.797 s at a clean 20 ms
//	                    spacing; 11 tsdemux CONTINUITY warnings over the run,
//	                    which is ordinary loss on an unprotected internet path
//	                    at 120 ms of SRT latency
//	audio pad           audio/mpeg, mpegversion=4, stream-format=adts —
//	                    present, and DISCARDED here
//
// The stream-format on the wire is byte-stream, not the hvc1 the task note
// recorded; h265parse converts to hev1 for the decoder. Nothing in this file
// depends on which, because h265parse is between them, but it is written down
// because a caps string somebody copies into a capsfilter one day would fail
// against byte-stream and the failure would look like a decoder problem.
//
// # No libav, ever
//
// The decoder is the platform's own, part of gst-plugins-bad, LGPL, wrapping a
// decoder that is in the GPU driver or in the operating system: d3d11h265dec
// (Direct3D 11 / DXVA) on Windows, vtdec_hw (VideoToolbox) on macOS. avdec_h265
// is present at primary rank on BOTH development machines and MUST NOT be
// selected. gst-libav is FFmpeg, which is the same commercial-shipping concern
// as x264enc, and both bundlers' forbidden lists refuse to copy anything
// matching *libav* or *avcodec* — so a build that selected it would work on a
// development machine and fail to load a plugin on the installed one. mfh265dec
// is absent on the Windows target: the HEVC video extension is not installed,
// and requiring the operator to buy it from the Microsoft Store is not a
// deployment step.
//
// # Where the logic lives
//
// This file carries no build tag and no cgo. Everything testable without
// GStreamer is here: option validation, the backoff ladder, the whole reconnect
// state machine, and the rectangle arithmetic that positions the overlay. The
// two build-tagged twins, picture_cgo.go and picture_stub.go, supply exactly one
// thing between them — newPicturePipe, which builds one attempt's pipeline. That
// is the seam gst.go and return.go both draw, for the same reason: Gate A must
// build and test the application on a machine with no MinGW and no GStreamer.
package gst

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

// DefaultPictureLatencyMs is srtsrc's latency property, in MILLISECONDS.
//
// It matches DefaultReturnLatencyMs and DefaultSRTLatencyMs — roughly five times
// the measured 21 ms median round-trip time to the M2L-X instance — and it is a
// separate constant for the same reason theirs are: the three are free to
// diverge and changing one should be a decision rather than a side effect.
//
// There is a real argument for this one being HIGHER than the audio return's.
// A commentator tolerates a late picture far better than a broken one, and the
// picture is not what they are speaking over — but it IS what they are
// commentating on, and lip-sync against the effects in their ear is what a
// commentator actually notices. Until somebody measures that preference, this
// matches the other two rather than guessing.
const DefaultPictureLatencyMs = 120

// pictureSinkSync is the video sink's "sync" property, and it is FALSE.
//
// It is a named constant rather than a literal at the call site for the reason
// overlayExStyle is one: this is a property that reads like tidy-up-able default
// noise, the pipeline works either way, and the cost of it being quietly
// reverted is a second of latency that nobody would attribute to a deleted line.
// picture_cgo_guard_test.go asserts both the value here and the fact that
// buildLocked still passes it.
//
// # Why a video sink on a live feed must not sync, HERE
//
// sync=true is right for playback and is GStreamer's default for good reason: a
// sink holds each buffer until its running time arrives on the pipeline clock,
// which is what keeps a video track lined up with the audio track beside it.
//
// THERE IS NO AUDIO IN THIS PIPELINE. The demuxer's audio pad goes to a
// fakesink — see onPadAdded in picture_cgo.go, which throws it away deliberately
// — and the commentator's audio arrives over an entirely separate path, from
// Kinesis, with its own clock, its own buffering and no relationship to this
// pipeline's clock at all. So there is nothing here for the clock to synchronise
// the video AGAINST. The sink was paying the whole accumulated pipeline latency
// and buying nothing with it.
//
// # What that cost, measured
//
// Dialled at the live instance on 2026-08-07, port 40501, 1080p50, two 30 s
// runs of the shipped element chain differing only in this property. GStreamer's
// own latency query, from the sink's log:
//
//	min(855 ms) = upstream(840 ms) + processing_deadline(15 ms) + render_delay(0)
//	upstream 840 ms = srtsrc 120 + tsdemux 700 + h265parse 20 + d3d11h265dec 0
//
// and the latency tracer's srtsrc-src-pad to sink-pad transit time:
//
//	sync=true    mean 993.7 ms   (n=1309, min 944.6, max 1022.7)
//	sync=false   mean   1.2 ms   (n=1404, min   0.3, max   49.5)
//
// That is the operator's "about a second behind the main feed", in full, and it
// is very nearly all one number: tsdemux's own "latency" property, which
// defaults to 700 ms. That property is a CLAIM made to the latency query and not
// a queue — tsdemux is not holding buffers for 700 ms, which is exactly why the
// transit time collapses to about a millisecond the moment the sink stops
// honouring the claim.
//
// # The condition that makes this wrong again
//
// IF THIS PIPELINE EVER CARRIES THE AUDIO THE COMMENTATOR HEARS, SYNC MUST COME
// BACK. The instant there is an audio branch that reaches their headphones — the
// fakesink in onPadAdded replaced by a real sink, or the Kinesis return retired
// in favour of the audio already present in this transport — the two branches
// have to be paced by the same clock, and a video sink rendering on arrival while
// an audio sink renders on the clock will drift apart without limit. There is no
// warning for that failure other than this paragraph.
const pictureSinkSync = false

// pictureSinkQoS is the video sink's "qos" property, and it is FALSE.
//
// The element's default is TRUE, so this is a change, and it is made together
// with pictureSinkSync rather than left unexamined because the two interact.
//
// A QoS-enabled sink measures how late each buffer is against its render
// deadline, drops the ones it judges too late, and sends the lateness upstream
// so the decoder can skip ahead. On a monitor that is a defensible trade: a
// current picture with holes beats a complete picture falling further behind.
//
// But with sync=false THERE IS NO RENDER DEADLINE. GstBaseSink short-circuits
// its sync step, so no lateness is ever computed and "late" stops having a
// meaning to measure a drop against. That is not inference; it is in the sink's
// own log. Measured over the same runs:
//
//	sync=true    1309 clock waits, GST_CLOCK_OK, mean jitter -18.9 ms
//	sync=false   1290 buffers, every one logged "sync disabled", clock return
//	             GST_CLOCK_BADTIME and a jitter of exactly 0
//
// Neither run dropped a frame and neither emitted a QoS event.
//
// So turning it off changes nothing observable today. What it removes is a
// frame-dropping heuristic left armed against a deadline the sink no longer
// honours — one that upstream d3d11h265dec would act on by skipping frames, on
// a 50p feed, producing judder on a picture that was never actually late.
//
// It pairs with pictureSinkSync: if sync comes back for the reason above, this
// should come back with it, because then the deadline is real again.
const pictureSinkQoS = false

// pictureFirstFrameTimeout is how long Play waits, after the pipeline reaches
// PLAYING, for a DECODED FRAME to leave the decoder.
//
// # It waits for a frame and not for a state change, and that is the point
//
// srtsrc reaching PLAYING proves nothing at all. It connects lazily, inside
// gst_srt_src_fill on a streaming thread, after the state change has already
// returned success, and libsrt's default SRTO_CONNTIMEO of 3 s is what bounds
// it — measured on this instance and written up at returnAudioPadTimeout. So a
// Play that returned at PLAYING would report a working picture on a socket that
// does not exist.
//
// This path can be stricter than the audio return, and is. The return waits for
// its audio pad to be LINKED, because linking is the last thing it can observe
// cheaply. Here there is a decoder in the way, and a decoder is exactly the
// component most likely to be the thing that is broken: the decoder's plugin
// missing from the bundle, a GPU or a media engine that will not give out a
// hardware context, an HEVC profile the hardware refuses. Waiting for the first
// buffer out of the decoder covers the socket, the PMT, the parser and the
// decoder with one wait, and a nil error from Play then means what a commentator
// would mean by it: there are pictures.
//
// Ten seconds is far beyond a working stream — libsrt gives up on the connect at
// 3 s, a PMT repeats several times a second, the measured SRT lock to this
// instance is about 1.1 s and the first frame followed it inside a second — and
// it matches returnAudioPadTimeout so the two paths fail on the same scale.
const pictureFirstFrameTimeout = 10 * time.Second

// PictureBackoffLadder is the reconnect delay sequence, followed by
// PictureBackoffCap repeated for ever.
//
// It is the same shape as ReturnBackoffLadder and internal/sender's, and the
// first rung is at or above six seconds for the same measured reason: M2L-X's
// SRT listener accepts exactly one peer, never displaces the incumbent, and
// refuses re-accept for roughly five seconds after the incumbent goes away.
// Every M2L-X output has reverse=True, so M2L-X listens and we dial; when our
// own socket was the incumbent, a retry inside that window is refused and the
// attempt is wasted. The wait only buys anything if our socket is already gone
// when it starts, which is why the machine below closes the pipeline BEFORE it
// sleeps and not after.
//
// It is duplicated rather than imported for the reason ReturnBackoffLadder gives
// at length: internal/sender imports internal/gst, so importing it back would be
// a cycle, and three paths with three different tolerances should be free to
// disagree about how hard to retry.
var PictureBackoffLadder = []time.Duration{
	7 * time.Second,
	7 * time.Second,
	10 * time.Second,
	15 * time.Second,
	20 * time.Second,
}

// PictureBackoffCap is the delay used for every attempt after the ladder is
// exhausted. There is no attempt limit: the picture retries until Stop.
//
// A picture that dies silently mid-match is a commentator watching a frozen
// frame and describing something that is no longer happening, which is worse
// than no picture. Giving up after N attempts would mean a monitor dead for the
// rest of the match because of a thirty second network blip in the first half.
const PictureBackoffCap = 30 * time.Second

// PictureState is the picture monitor's state. It is the source of whatever the
// frontend puts behind the picture, and it is emitted to the page, so these
// string values are contract.
//
// There are four, matching ReturnState, and the argument for four rather than
// five is the same: the send path needs a DRAINING state because tearing the old
// sink out is a separable step the operator has to be able to see, whereas here
// the whole pipeline is rebuilt per attempt and the teardown is the first thing
// the failure path does unconditionally.
type PictureState string

const (
	// PictureStateStopped means no picture pipeline exists. It is the state
	// before Start and after Stop.
	PictureStateStopped PictureState = "stopped"

	// PictureStateConnecting means an attempt is in flight: the socket is being
	// dialled, or it is up and no frame has been decoded yet.
	PictureStateConnecting PictureState = "connecting"

	// PictureStateShowing means frames have been decoded and are being presented
	// in the overlay. It is the ONLY state in which the overlay surface may be
	// visible, and app_picture.go enforces that rather than trusting it: an
	// opaque black rectangle over the fallback mosaic is worse than the soft
	// picture it is covering.
	PictureStateShowing PictureState = "showing"

	// PictureStateBackoff means an attempt failed and the next one is waiting on
	// the ladder.
	PictureStateBackoff PictureState = "backoff"
)

// ErrPictureAlreadyStarted is returned by Start on a monitor that is running.
var ErrPictureAlreadyStarted = errors.New("gst: picture monitor already started")

// ErrPictureNotStarted is returned by Stop on a monitor that was never started.
var ErrPictureNotStarted = errors.New("gst: picture monitor not started")

// ErrAbandonedThread marks an error whose Close GAVE UP on an OS thread rather
// than joining it. It is wrapped, never returned bare: the wrapping error says
// which thread and what it was doing.
//
// # Why a sentinel and not just a sentence in the message
//
// Because a caller has to be able to ACT on it, and only a sentinel can be
// acted on. App.teardown runs each shutdown step under its own budget and ends
// the process with TerminateProcess if any step had to be abandoned, precisely
// so that the ordinary exit — ExitProcess, which runs DLL_PROCESS_DETACH for
// GStreamer, WASAPI, D3D11 and COM under the loader lock after killing every
// other thread — never runs over a thread that was killed mid-call. See
// exit_windows.go.
//
// A step that RETURNS is scored as finished. overlay.Close on Windows is the
// first Close in this application that returns on a hang instead of hanging, so
// it is the first that can hand teardown a completed step with an abandoned
// thread behind it — the exact shape the hard exit exists to catch, arriving
// through the one door that did not look for it. errors.Is on this value is that
// door.
//
// NOTHING ON macOS PRODUCES IT, and that is correct rather than an omission.
// overlay_darwin.go owns no thread: its Close posts a block to AppKit's main
// queue and returns, so there is never a thread of ours to abandon. Wrapping
// this sentinel there would end every ordinary quit with the hard exit. The
// reasoning is written out in that file's header.
//
// It is declared here, in the file with no build tag, rather than beside
// ErrNoHostWindow in each of overlay_windows.go and overlay_other.go: package
// main tests for it on every platform, and two build-tagged errors.New calls
// are two different values.
var ErrAbandonedThread = errors.New("gst: an OS thread was abandoned rather than joined")

// ---------------------------------------------------------------------------
// Options
// ---------------------------------------------------------------------------

// PictureOpts is everything one picture session needs.
//
// It carries no device selection and no channel selection, which is the whole
// difference between it and ReturnOpts: there is one screen, the picture goes
// where the operator's layout says it goes, and the destination is a native
// surface handle rather than an endpoint the operator picks from a dropdown.
type PictureOpts struct {
	// Host is the M2L-X host. It is a NAME here and is resolved to a literal
	// before it reaches srtsrc; see the resolve step in Play and the hazard it
	// exists for.
	Host string

	// Port is the M2L-X output's SRT port. 40501 on the measured instance:
	// Output 1, src=pgm, encrypted=False, H.265 1920x1080 50p at 15000 kbps.
	Port int

	// LatencyMs is srtsrc's latency in milliseconds. Zero means
	// DefaultPictureLatencyMs.
	LatencyMs int

	// Passphrase is the SRT passphrase for THIS output, or empty for an
	// unencrypted one. Output 1 is unencrypted on the measured instance;
	// Outputs 2 and 3 are not, and M2L-X sets encryption per output, so this is
	// deliberately not shared with either of the other two SRT paths.
	//
	// It is a secret. It is set with g_object_set and never placed in the URI,
	// so it is neither percent-encoded nor logged. Nothing in this package
	// formats PictureOpts as a whole; endpointForLog exists so nothing is
	// tempted to.
	Passphrase string

	// PBKeyLen is the SRT key length in bytes: 0, 16, 24 or 32. Zero with a
	// passphrase present is defaulted to 16 by normalise, matching the send and
	// return paths.
	PBKeyLen int

	// WindowHandle is the native surface the decoded picture is presented into,
	// as a uintptr, and it must be non-zero.
	//
	// It is an HWND on Windows and an NSView* on macOS. That is not a compromise
	// in this struct: it is exactly what GstVideoOverlay's own guintptr means on
	// each platform, and gst_video_overlay_set_window_handle takes it unchanged.
	//
	// It is a plain integer rather than an Overlay because internal/gst's
	// pipeline half has no business owning a window: overlay_windows.go and
	// overlay_darwin.go create and move the surface, this path only renders into
	// it, and keeping the dependency one-way means a wedged pipeline cannot hold
	// a lock the surface needs to be resized. It is the handle of a surface this
	// process owns and keeps alive for the whole session; see PictureOverlay.
	WindowHandle uintptr
}

// normalise fills in the documented defaults and reports every option that
// cannot work as one joined error, so the operator sees all the problems at once
// instead of one edit-fail cycle per field.
func (o *PictureOpts) normalise() error {
	var problems []error

	o.Host = strings.TrimSpace(o.Host)
	if o.Host == "" {
		problems = append(problems, errors.New("no M2L-X host is configured for the picture"))
	}
	if o.Port <= 0 || o.Port > 65535 {
		problems = append(problems, fmt.Errorf("the picture port %d is not a usable TCP/UDP port", o.Port))
	}
	if o.LatencyMs < 0 {
		problems = append(problems, fmt.Errorf("the picture SRT latency %d ms is negative", o.LatencyMs))
	}
	if o.LatencyMs == 0 {
		o.LatencyMs = DefaultPictureLatencyMs
	}

	switch o.PBKeyLen {
	case 0:
		// A passphrase with no key length is AES-128, which is libsrt's own
		// default and what the send and return paths both do. Defaulting rather
		// than refusing means an operator who typed a passphrase and never saw
		// the key-length control gets a working encrypted session.
		if o.Passphrase != "" {
			o.PBKeyLen = 16
		}
	case 16, 24, 32:
	default:
		problems = append(problems, fmt.Errorf(
			"the picture SRT key length %d is not 0, 16, 24 or 32 bytes", o.PBKeyLen))
	}
	if o.Passphrase == "" && o.PBKeyLen != 0 {
		// An encrypted session with nothing to encrypt with. libsrt refuses it,
		// for ever, silently, and this is the exact fault that cost the operator
		// an afternoon on the return path.
		problems = append(problems, errors.New(
			"a picture SRT key length is set but no passphrase is stored, which asks for an "+
				"encrypted session with no key and can never connect"))
	}

	if o.WindowHandle == 0 {
		problems = append(problems, errors.New(
			"no window handle was given for the picture; there is nowhere to render it"))
	}

	if len(problems) > 0 {
		return fmt.Errorf("gst: the picture configuration cannot be used: %w", errors.Join(problems...))
	}
	return nil
}

// endpointForLog renders the picture endpoint for a log line.
//
// It exists so that no code path is ever tempted to format PictureOpts as a
// whole with %+v, which would put the SRT passphrase in the field log and then
// in whatever support bundle the log is mailed in.
func (o PictureOpts) endpointForLog() string {
	return fmt.Sprintf("srt://%s:%d", o.Host, o.Port)
}

// ---------------------------------------------------------------------------
// The overlay rectangle
// ---------------------------------------------------------------------------

// PictureRect is where the picture sits, in PHYSICAL pixels, relative to the
// top-left of the host window's CLIENT area.
//
// It is the SAME on both platforms, deliberately, and neither the frontend nor
// this file knows which one it is talking to. On Windows a child HWND is
// positioned in physical pixels, so the overlay uses these numbers as they
// stand. On macOS an NSView's frame is in POINTS from the bottom left, so
// overlay_darwin.go turns the y axis over and divides by the window's
// backingScaleFactor — a conversion that is exact, because on macOS the page's
// devicePixelRatio IS the backing scale times the page zoom. Keeping one
// representation across the Wails boundary is what stops the frontend needing to
// know the platform.
//
// # Why physical pixels, and why the conversion is not done here
//
// A native child window is positioned in physical pixels. The page that decides
// where the picture goes lays out in CSS pixels. The factor between them is the
// web view's device pixel ratio, which is not a constant: the operator runs a
// 3840x2088 window, display scaling can be changed while the application is
// running, and dragging the window to a second monitor changes it without any
// setting changing at all.
//
// So the frontend sends CSS pixels AND the window.devicePixelRatio it measured
// them with, in one call, and ScaleRect below multiplies them. Sending only CSS
// pixels and reading the DPI on this side would be a different number measured
// at a different moment — GetDpiForWindow is the monitor's scale factor, which
// is only equal to the WebView's device pixel ratio when the WebView is at 100%
// zoom, and Ctrl+scroll on a WebView2 changes one and not the other. The page's
// own ratio is the authoritative one because the page's own layout is what the
// rectangle has to line up with.
type PictureRect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Empty reports whether the rectangle has no area. An empty rectangle is a
// request to show nothing, and the overlay treats it as a hide rather than as an
// error: a page that has not laid out yet, or that has scrolled the picture off
// screen, legitimately produces one.
func (r PictureRect) Empty() bool { return r.W <= 0 || r.H <= 0 }

// String renders the rectangle for a log line.
func (r PictureRect) String() string {
	return fmt.Sprintf("%dx%d+%d+%d", r.W, r.H, r.X, r.Y)
}

// ScaleRect converts a rectangle measured in CSS pixels into physical pixels.
//
// It rounds to nearest rather than truncating, and it rounds the EDGES rather
// than the position and the size independently. Truncation loses up to a pixel
// per edge, which at a device pixel ratio of 1.5 shows up as a one-pixel seam
// between the picture and whatever the page draws next to it — visible, on a
// black background, at the top of a commentary window, for ever. Rounding the
// far edge and subtracting keeps the right and bottom edges where the page put
// them, which is what the seam is about.
//
// A non-finite or non-positive ratio is treated as 1. A page that reports a
// nonsense devicePixelRatio should produce a picture in the wrong place, which
// the operator can see and report, rather than an error dialog or a rectangle
// collapsed to nothing.
func ScaleRect(x, y, w, h, ratio float64) PictureRect {
	if !(ratio > 0) || ratio > 16 {
		// The upper bound is not defensive dressing: ratio arrives from
		// JavaScript across the Wails boundary, and a NaN would propagate
		// through the multiplication into int conversion, which in Go is
		// implementation-defined. 16 is far beyond any shipping display.
		ratio = 1
	}
	left := roundHalfAwayFromZero(x * ratio)
	top := roundHalfAwayFromZero(y * ratio)
	right := roundHalfAwayFromZero((x + w) * ratio)
	bottom := roundHalfAwayFromZero((y + h) * ratio)

	rect := PictureRect{X: left, Y: top, W: right - left, H: bottom - top}
	if rect.W < 0 {
		rect.W = 0
	}
	if rect.H < 0 {
		rect.H = 0
	}
	return rect
}

// roundHalfAwayFromZero rounds to the nearest integer. It is math.Round with the
// NaN and infinity cases already excluded by the caller, written out so that
// this file needs no import for one operation.
func roundHalfAwayFromZero(v float64) int {
	if v >= 0 {
		return int(v + 0.5)
	}
	return int(v - 0.5)
}

// cocoaOverlayFrame turns a PictureRect into an AppKit frame.
//
// A PictureRect is TOP-LEFT origin and PHYSICAL PIXELS, because that is what a
// child HWND wants and what the page measured. An NSView's frame is BOTTOM-LEFT
// origin and POINTS, because the Wails contentView is not flipped and because
// AppKit does not deal in pixels. This is both conversions, in that order:
//
//	x = X / scale
//	y = containerHeight - Y/scale - H/scale
//	w = W / scale
//	h = H / scale
//
// containerHeight is the superview's height IN POINTS and scale is the host
// window's backingScaleFactor, measured at 2.0 on the development machine.
//
// # Why this is here rather than only in the Objective-C
//
// Because a sign error in it is a picture in the wrong half of the window, and
// nothing else in the process would ever notice. overlay_darwin.go does the real
// conversion — it has to, on the main thread, against a superview height and a
// backing scale that change when the operator drags the window to another
// display — and this is the same expression in Go so that it can be exercised at
// Gate A, on any platform, without a window, a display or cgo.
// picture_test.go tests THIS against worked examples and then asserts that
// overlay_darwin.go's C still spells the same thing, which is the only way to
// keep two expressions of one formula honest.
//
// It lives in picture.go, which carries no build tag, for the same reason
// ScaleRect does: it is arithmetic about a rectangle, it is the kind of thing
// that has to be tested, and the test file has no build tag either.
//
// A scale that is not positive is treated as 1. It arrives from
// -[NSWindow backingScaleFactor], which cannot sensibly be zero, and the
// consequence of dividing by it if it ever were is not something to find out
// during a match.
func cocoaOverlayFrame(r PictureRect, containerHeight, scale float64) (x, y, w, h float64) {
	if !(scale > 0) {
		scale = 1
	}
	h = float64(r.H) / scale
	return float64(r.X) / scale,
		containerHeight - float64(r.Y)/scale - h,
		float64(r.W) / scale,
		h
}

// ---------------------------------------------------------------------------
// The contract
// ---------------------------------------------------------------------------

// PictureMonitor receives the SRT programme feed and presents it in the overlay
// surface, staying connected for as long as it is running.
//
// A PictureMonitor is single-use: after Stop it cannot be restarted, because its
// state channel is closed. Call NewPictureMonitor again. All methods are safe
// for concurrent use.
type PictureMonitor interface {
	// Start validates opts and launches the reconnect loop. It returns as soon
	// as the loop is running, NOT when pictures are on screen.
	//
	// A failure to CONNECT is not an error from Start: the monitor retries
	// indefinitely, and the commentator may well open the picture before the
	// M2L-X output has been enabled. A configuration that can never work — no
	// host, a port out of range, a key length with no passphrase, no surface to
	// render into — IS an error from Start, and it names the field.
	//
	// It returns ErrPictureAlreadyStarted if the monitor is already running or
	// has been stopped.
	Start(opts PictureOpts) error

	// Stop tears the session down: it ends the reconnect loop, takes the
	// pipeline to NULL, releases the SRT socket and the hardware decoder, emits
	// PictureStateStopped and closes the channel returned by States.
	//
	// Stop blocks until all of that is done and must not be called from a UI
	// thread. Its worst case is a build-and-connect attempt already in flight:
	// that is synchronous inside GStreamer and cannot be cancelled, so Stop
	// waits for it. Everything this file contributes is prompt — the backoff
	// wait is cancelled rather than served.
	//
	// It does NOT destroy the overlay surface. The surface outlives the monitor,
	// because the monitor is rebuilt whenever the endpoint changes and a native
	// surface destroyed and recreated underneath a running web view is a z-order
	// fight nobody needs. See PictureOverlay.
	//
	// It returns ErrPictureNotStarted if the monitor was never started.
	Stop() error

	// States returns the stream of state transitions. It is buffered so a slow
	// consumer cannot stall the reconnect loop; a consumer that falls too far
	// behind loses intermediate states but always eventually sees the current
	// one.
	//
	// It is valid before Start, so a caller can begin consuming and then start.
	// The channel is closed by Stop, after PictureStateStopped has been sent.
	States() <-chan PictureState
}

// picturePipe is one attempt's pipeline: the seam between this pure-Go file and
// the two build-tagged twins.
//
// It is returnPipe's shape, and for returnPipe's reasons: nothing upstream of
// the sink has to survive an attempt, because there is no capture and no
// encoder here and the source of timestamps is the far end. An attempt is a
// whole pipeline, built and destroyed. The sink-swapping machinery the
// contribution path cannot do without would here be complexity with nothing to
// buy.
type picturePipe interface {
	// Play builds the pipeline, takes it to PLAYING and waits for a decoded
	// frame. It returns synchronously, and a nil error means the SRT caller
	// handshake succeeded, the demuxer found H.265, and the hardware decoder
	// produced at least one frame.
	//
	// It is therefore slow — up to pictureFirstFrameTimeout on a dead endpoint,
	// about two seconds on a live one — and Stop's worst case is one of these
	// already in flight.
	Play(opts PictureOpts) error

	// Errors carries asynchronous failure: a bus error, an end-of-stream from
	// srtsrc, or a decoder that stopped. It is closed by Close. Implementations
	// drop rather than block if it is full.
	Errors() <-chan error

	// Close takes the pipeline to NULL, releases the SRT socket and the decoder,
	// and closes the channel returned by Errors. It is idempotent and is safe to
	// call on a pipeline whose Play failed or was never called.
	Close() error
}

// pictureClock is the monitor's only view of time, for the reason returnClock
// has one: the ladder is 7, 7, 10, 15, 20 then 30 seconds for ever, and a test
// that waited those durations for real would take minutes per case and would be
// the first thing anyone deleted.
//
// It is unexported and NewPictureMonitor takes no clock, so there is no way for
// a shipped build to run on anything but the real one.
type pictureClock interface {
	newTimer(d time.Duration) (fire <-chan time.Time, stop func())
}

// realPictureClock is the production clock. The monitor must never use
// time.Sleep: Stop has to return promptly from the middle of a thirty second
// wait.
type realPictureClock struct{}

func (realPictureClock) newTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

var _ pictureClock = realPictureClock{}

// pictureStatesBuffer is the depth of the channel returned by States. One full
// connect-fail-backoff cycle emits two states and the shortest cycle is bounded
// below by the seven second first rung, so sixteen only fills if the consumer
// has stopped reading entirely — which is the case the dropping is for.
const pictureStatesBuffer = 16

// pictureBackoffDelay returns the wait before attempt number attempt, counting
// the attempt that has just failed as attempt zero: 7, 7, 10, 15, 20, 30, 30,
// ... with no upper bound on the number of attempts.
//
// A negative attempt cannot arise from this file's own bookkeeping and is
// clamped rather than panicking. During a match an over-long wait is survivable
// and a panic in the process carrying the commentary is not.
func pictureBackoffDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt < len(PictureBackoffLadder) {
		return PictureBackoffLadder[attempt]
	}
	return PictureBackoffCap
}

// ---------------------------------------------------------------------------
// The reconnect machine
// ---------------------------------------------------------------------------

// pictureMonitor is the concrete PictureMonitor.
//
// # Concurrency design
//
// Every state transition happens on exactly one goroutine, the one running run.
// Nothing else reads or writes the machine's variables, so there is no lock over
// the state and no possibility of two transitions interleaving. Everything else
// reaches the loop through channels:
//
//   - quit, closed exactly once by Stop, appears in every select the loop can
//     block in. That is what makes Stop prompt from the middle of a thirty
//     second backoff.
//   - the current attempt's pipe.Errors(), selected on only while that attempt
//     is the live one, so a stale channel can never be read.
//   - states, written only by the loop and closed only by the loop. One writer
//     that is also the only closer makes "no send on a closed channel" and "no
//     double close" structural rather than something to be careful about.
//
// mu guards only the Start/Stop bookkeeping and is never held while the loop is
// running or waited on.
type pictureMonitor struct {
	newPipe func() (picturePipe, error)
	clock   pictureClock

	// windowAlive reports whether the render target named by
	// PictureOpts.WindowHandle still exists. It is the guard against the one
	// thing this loop must never do: rebuild a video sink into a window that has
	// been destroyed, which on Windows wedges gstd3d11 with no timeout and cannot
	// be interrupted — the fault that left a wslcomms process unable to exit. See
	// pictureWindowAlive (per platform) and the check at the top of loop.
	//
	// It is a seam for the same reason newPipe and clock are: newPictureMonitor
	// defaults it to "always alive" so the unit tests, which pass synthetic
	// handles that are not real windows, keep reconnecting; NewPictureMonitor
	// wires the real per-platform check for the shipped build.
	windowAlive func(handle uintptr) bool

	states   chan PictureState
	quit     chan struct{}
	loopDone chan struct{}

	// stopErr is the result of the final Close. It is written by the run
	// goroutine before loopDone is closed and read by Stop after loopDone is
	// closed, so it needs no lock.
	stopErr error

	// hook is a test seam: if non-nil it is called on the run goroutine with
	// each state immediately before that state is offered to the channel. It is
	// nil in every shipped build — NewPictureMonitor does not set it and nothing
	// exported can — and exists so that a test can call Stop with the machine
	// provably in a chosen state instead of sleeping and hoping.
	hook func(PictureState)

	mu       sync.Mutex
	started  bool
	stopOnce sync.Once
}

var _ PictureMonitor = (*pictureMonitor)(nil)

// NewPictureMonitor returns a PictureMonitor backed by the real GStreamer
// pipeline in a cgo build and by the stub in a Gate A build.
//
// It wires the real per-platform window-liveness check onto the monitor. That
// wiring lives here rather than in newPictureMonitor because the check is the
// one seam the unit tests must NOT get the real version of: they drive the loop
// with synthetic window handles that are not real windows, and the real check
// would report every one of them dead and stop the loop before it reconnected
// once. See pictureMonitor.windowAlive.
func NewPictureMonitor() PictureMonitor {
	m := newPictureMonitor(newPicturePipe, realPictureClock{})
	m.windowAlive = pictureWindowAlive
	return m
}

// newPictureMonitor builds a monitor over an injected pipeline factory and
// clock. NewPictureMonitor uses the real ones; the tests use a fake pipe and a
// fake clock so the whole ladder runs in microseconds.
//
// windowAlive defaults to "always alive" so a test that has not set it keeps
// its existing behaviour; NewPictureMonitor overrides it with the real check
// and a test that wants to exercise the destroyed-window path sets its own.
func newPictureMonitor(newPipe func() (picturePipe, error), clk pictureClock) *pictureMonitor {
	return &pictureMonitor{
		newPipe:     newPipe,
		clock:       clk,
		windowAlive: func(uintptr) bool { return true },
		states:      make(chan PictureState, pictureStatesBuffer),
		quit:        make(chan struct{}),
		loopDone:    make(chan struct{}),
	}
}

// States returns the state stream. See the PictureMonitor interface.
func (m *pictureMonitor) States() <-chan PictureState { return m.states }

// Start validates opts and launches the loop. See the PictureMonitor interface.
func (m *pictureMonitor) Start(opts PictureOpts) error {
	if err := opts.normalise(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return ErrPictureAlreadyStarted
	}
	m.started = true

	log.Printf("gst: picture monitor starting on %s", opts.endpointForLog())
	go m.run(opts)
	return nil
}

// Stop unwinds the session. See the PictureMonitor interface.
//
// It closes quit exactly once and then waits for the run goroutine, so that by
// the time it returns the pipeline is at NULL, PictureStateStopped has been
// emitted and the states channel is closed. Waiting is what lets a caller stop
// the monitor and immediately build another one without two pipelines rendering
// into the same surface.
//
// It is idempotent in effect: later calls close nothing, wait on an
// already-closed loopDone and return the same error.
func (m *pictureMonitor) Stop() error {
	m.mu.Lock()
	started := m.started
	m.mu.Unlock()

	if !started {
		return ErrPictureNotStarted
	}

	m.stopOnce.Do(func() { close(m.quit) })
	<-m.loopDone
	return m.stopErr
}

// run owns the whole session on one goroutine.
func (m *pictureMonitor) run(opts PictureOpts) {
	stopErr := m.loop(opts)

	m.emit(PictureStateStopped)
	close(m.states)

	m.stopErr = stopErr
	close(m.loopDone)
}

// loop is the state machine. It returns only when quit has been closed, and
// returns the error from the final Close, if any.
//
//	CONNECTING --play ok--> SHOWING --error/EOS--> (close) --> BACKOFF --> CONNECTING
//	     '--play fails-------------------------------(close) --> BACKOFF
//
// attempt counts consecutive failures and indexes PictureBackoffLadder. It is
// reset by a successful connect, so the first wait after a mid-match drop is the
// seven second rung that clears M2L-X's re-accept refusal window, not whatever
// rung an earlier outage had climbed to.
//
// The ORDER of close-then-wait is load-bearing, and it is the same point
// specification section 6.2 makes for the send path. M2L-X accepts one peer and
// refuses re-accept for roughly five seconds after that peer goes away, so the
// wait only clears the window if our socket is already gone when it starts
// ticking. Every path out of an attempt therefore calls Close before it reaches
// the sleep, with nothing between the two.
func (m *pictureMonitor) loop(opts PictureOpts) error {
	attempt := 0
	var lastCloseErr error

	for {
		if m.quitting() {
			return lastCloseErr
		}

		// The render target must still exist before a sink is built into it. This
		// is the guard that keeps the reconnect loop from doing the single worst
		// thing on this path: on Windows the overlay is a CHILD of the application
		// window, so an ordinary application close destroys the overlay HWND with
		// its parent, d3d11videosink reports "Output window was closed", and this
		// loop would otherwise back off and then rebuild a fresh d3d11videosink
		// into a handle that no longer names a window. That rebuild WEDGES inside
		// gstd3d11 — gst_element_set_state to PLAYING never returns, takes no
		// timeout and cannot be interrupted — and a wedged media thread is what
		// turns the process's ordinary exit into a DLL_PROCESS_DETACH deadlock
		// under the loader lock: the wslcomms that will not close and has to be
		// killed by hand. So a destroyed window is TERMINAL for this monitor. It
		// stops, the page falls back to the mosaic, and — since the window is only
		// ever destroyed when the process itself is going — this is the same stop
		// teardown was about to ask for, reached before the wedge instead of after
		// it. See pictureMonitor.windowAlive and exit_windows.go.
		if opts.WindowHandle != 0 && m.windowAlive != nil && !m.windowAlive(opts.WindowHandle) {
			log.Printf("gst: picture monitor: the output window (0x%x) has been destroyed; stopping "+
				"rather than reconnecting — rebuilding the video sink into a window that no longer "+
				"exists cannot connect and wedges the graphics driver with no way back",
				opts.WindowHandle)
			return lastCloseErr
		}

		m.emit(PictureStateConnecting)

		pipe, err := m.newPipe()
		if err != nil {
			// The pipeline could not even be created — in practice the decoder's
			// plugin missing from the bundle. Retrying will not fix that, but it
			// costs nothing and the alternative is a picture that is dead for the
			// rest of the match because the bundle was repaired while it was
			// running. The reason is logged on every attempt, so it is not
			// silent.
			m.report(err, attempt)
		} else {
			err = pipe.Play(opts)
			if err == nil {
				attempt = 0
				m.emit(PictureStateShowing)
				log.Printf("gst: picture monitor showing %s", opts.endpointForLog())

				// SHOWING. The only things that can happen are the far end going
				// away — a bus error, or an EOS from srtsrc — and Stop.
				//
				// A closed error channel counts as a failure too. Only Close
				// closes it, and Close has not been called on this pipe yet, so a
				// closed channel here means the implementation gave up without
				// saying why. Treating that as healthy would leave a frozen frame
				// on screen with nothing anywhere saying the feed had stopped,
				// which is the exact fault this whole path exists to avoid.
				select {
				case <-m.quit:
					if cerr := pipe.Close(); cerr != nil {
						lastCloseErr = cerr
						log.Printf("gst: picture monitor: closing the pipeline on Stop: %v", cerr)
					}
					return lastCloseErr
				case e, ok := <-pipe.Errors():
					if !ok {
						e = errors.New("gst: picture monitor: the pipeline closed its error channel without being stopped")
					}
					if e == nil {
						e = errors.New("gst: picture monitor: the pipeline reported an unspecified error")
					}
					err = e
				}
			}

			// Close BEFORE the wait, on every failure path, so that our socket is
			// gone for the whole of the backoff. See the doc comment above.
			if cerr := pipe.Close(); cerr != nil {
				lastCloseErr = cerr
				log.Printf("gst: picture monitor: closing a failed pipeline: %v", cerr)
			}
			m.report(err, attempt)
		}

		if m.quitting() {
			return lastCloseErr
		}

		d := pictureBackoffDelay(attempt)
		attempt++
		m.emit(PictureStateBackoff)
		if !m.sleep(d) {
			return lastCloseErr
		}
	}
}

// report logs why one attempt failed.
//
// It always logs, and it logs the attempt count, because the pair is what
// distinguishes a network blip from the case that matters: a fault that cannot
// clear itself — the decoder's plugin missing from the bundle, say, or an M2L-X
// output that has been switched off — where the attempt count climbs while the
// reason never changes.
func (m *pictureMonitor) report(err error, attempt int) {
	if err == nil {
		return
	}
	log.Printf("gst: picture monitor: attempt %d failed: %v; retrying in %v",
		attempt+1, err, pictureBackoffDelay(attempt))
}

// emit offers a state to the channel without blocking. It drops rather than
// blocks: the reconnect loop must never be stalled by the UI, and a consumer
// that has fallen behind still sees the current state next transition.
func (m *pictureMonitor) emit(s PictureState) {
	if m.hook != nil {
		m.hook(s)
	}
	select {
	case m.states <- s:
	default:
	}
}

// quitting reports whether Stop has been called.
func (m *pictureMonitor) quitting() bool {
	select {
	case <-m.quit:
		return true
	default:
		return false
	}
}

// sleep waits for d on the injected clock and reports whether the wait
// completed. It returns false as soon as quit is closed, which is what makes
// Stop prompt from the middle of the thirty second rung.
func (m *pictureMonitor) sleep(d time.Duration) bool {
	fire, stop := m.clock.newTimer(d)
	defer stop()

	select {
	case <-m.quit:
		return false
	case <-fire:
		return true
	}
}

// ---------------------------------------------------------------------------
// Which video codec the return transport is carrying
// ---------------------------------------------------------------------------

// pictureParserFor maps a demuxer pad's media type onto the parser that feeds
// the decoder, or "" for a type this branch cannot show.
//
// ===================== WHY THIS IS NOT A decodebin =========================
//
// The obvious answer to "the feed changed codec" is to stop naming elements and
// let decodebin work it out. It is the wrong answer HERE, for three reasons
// that were measured on this machine rather than assumed:
//
//  1. vtdec_hw sits at rank primary+1 (257) and avdec_h264/avdec_h265 at
//     primary (256). decodebin sorts on rank, so the choice between hardware
//     VideoToolbox and FFmpeg is one point wide, and a registry change flips it
//     silently. picture_cgo.go's header argues this at length and it is right.
//  2. The bundlers refuse to copy anything matching *libav* or *avcodec*, so
//     avdec is present on a development machine and absent from the installed
//     one. A decodebin therefore resolves DIFFERENTLY on the two, which is the
//     specific failure mode of testing one decoder and shipping another.
//  3. The picture latency was taken from 995 ms to 1.2 ms deliberately (see the
//     commit of that name). decodebin brings a multiqueue and autoplugged
//     buffering, and giving that measurement back invisibly is a poor trade for
//     saving one switch statement.
//
// What makes the switch cheap is that the DECODER does not change: measured,
// vtdec_hw's sink template accepts video/x-h264, video/x-h265 and
// video/x-prores. Only the parser differs, because the decoder wants
// stream-format=avc1/hev1 with codec_data and the demuxer emits byte-stream.
func pictureParserFor(mediaType string) string {
	switch mediaType {
	case "video/x-h265":
		return "h265parse"
	case "video/x-h264":
		return "h264parse"
	default:
		return ""
	}
}
