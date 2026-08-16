// preview.go is the CONFIDENCE MONITOR on the video the card is capturing: a
// small, slow, cheap second rendering of the same pictures the encoder is
// getting, taken off a tee INSIDE the contribution pipeline and drawn in a
// native surface of its own over the page.
//
// Owner: WP-PREVIEW, with preview_cgo.go and preview_stub.go. It does not touch
// picture.go, which is the SRT programme return, and the two must not be
// confused:
//
//	picture.go   what the SWITCHER is putting out, arriving over SRT from
//	             M2L-X, decoded here. It is what the commentator watches.
//	preview.go   what THIS MACHINE is sending, taken off the capture leg
//	             before the encoder. It is what the operator checks when they
//	             want to know that the card is seeing the right thing.
//
// # It is a branch of the contribution pipeline, and that is the sanctioned shape
//
// CONTRACT.md keeps the contribution, picture and return pipelines sharing
// nothing. A tee INSIDE the contribution pipeline does not cross that line, and
// it is the only shape available: proxysrc was measured and REJECTED (its
// internal queue is leaky=0, so a wedged consumer stalls the producer from 50 to
// 0 fps in under two seconds, and the producer's death is silent across it), and
// A SECOND decklinkvideosrc IS IMPOSSIBLE — the card is exclusive, two sources in
// one process fail 3/3 and two processes fail 3/3.
//
// # The branch, and why every element of it is where it is
//
//	<tee>. ! queue    leaky=downstream max-size-buffers=2
//	       ! videorate  ! video/x-raw,framerate=25/2
//	       ! videoscale ! video/x-raw,width=480,height=270
//	       ! videoconvert
//	       ! <glimagesink|d3d11videosink> sync=false
//
// THE HEAD QUEUE MUST BE leaky=downstream. A tee pushes to its src pads SERIALLY,
// on the upstream streaming thread, so anything the preview branch does slowly is
// done in the middle of the broadcast leg's own push. Measured on this machine
// with gst-launch-1.0 1.26.10, 500 frames of 1080p50 into a fakesink on the
// broadcast branch and a deliberately slow consumer (identity sleep-time=30000)
// on the preview branch:
//
//	leaky=downstream   10.18 s, 10.20 s   (49.1 / 49.0 fps — the feed is untouched)
//	leaky=no           18.41 s, 19.40 s   (27.2 / 25.8 fps — HALF RATE ON AIR)
//
// That is the whole reason the queue is there, and max-size-buffers=2 is what
// makes it leak early rather than accumulating a second of 1080p frames — 2
// buffers of 1080p NV12 is 6.2 MB, and the two other limits are set to unlimited
// so that the buffer count is the only criterion that can fire. (Arithmetic, not
// measurement: at 50 fps two buffers is 40 ms and 6.2 MB, so neither the default
// 1 s nor the default 10 MB limit would ever have been reached first. Stating
// them is for the reader, not for the behaviour.)
//
// IT SCALES BEFORE IT CONVERTS, and the order is worth about four times the
// branch's whole cost. Measured, same harness, 500 frames of 1080p50, user CPU
// over a 10.1 s run, three runs each:
//
//	videoconvert then videoscale (naive)   2.12 s, 2.30 s, 2.54 s   21-25 % of a core
//	videorate, videoscale, videoconvert    0.55 s, 0.73 s, 0.58 s   5.4-7.2 % of a core
//	no preview branch at all                            0.06 s/20 s   0.3 % of a core
//
// The rate cap comes FIRST, above the scaler, for the same reason the scaler
// comes above the converter: everything below videorate then runs 12.5 times a
// second instead of 50. A confidence monitor does not need 50 fps — it answers
// "is the right thing on the card", which a quarter-rate picture answers exactly
// as well.
//
// The owner's own measurement of the same reordering, against the real GL sink
// rather than a fakesink, was 18.0 % of a core naive against 1.8-2.4 % written
// this way. The absolute numbers differ because the sink and the source differ
// and this machine was shared while the runs were taken; the RATIO is the finding
// and it reproduced.
//
// AND THE STRING THIS FILE ACTUALLY RENDERS WAS RUN, against the capture leg's
// real shape — the element names, the tee, the broadcast branch's queue and
// gst_cgo.go's own conform capsfilter, with videotestsrc standing in for the
// card because it is agent A's to hold and because a test source isolates the
// branch from capture anyway. It parsed, it negotiated, it played to EOS, and
// the broadcast branch's rate did not move: 500 frames in 10.12 s with the
// preview and 10.13 s without it. The cost of adding it to the leg was +0.57 s
// and +0.21 s of user CPU over 10.1 s in two pairs — a few per cent of a core,
// on a machine that was running another job at the time.
//
// WHAT IS NOT PROVEN HERE, and is written down rather than glossed: no real
// video sink was driven, because that needs a window and the owner is asleep.
// The sink is the one this application already drives on this platform — see
// preview_cgo.go, which takes the picture path's own resolved answer rather than
// making a second one — and glimagesink's plain video/x-raw sink template
// accepts NV12, which is what the conform capsfilter above the tee produces, so
// the videoconvert at the end of the branch is a passthrough on today's target
// and a real conversion on a target that ever changes it.
//
// # EVERY PROPERTY IN THAT STRING IS ONE THAT CANNOT FAIL TO PARSE
//
// This is a rule and not an accident, and it is the single most important thing
// about this file. gst_parse_launch treats an unknown property as a HARD ERROR —
// see the bframes note in elements_windows.go — and the string this branch is
// appended to is THE CONTRIBUTION PIPELINE'S. A property that a future GStreamer
// renames, or that one platform's sink does not have, would not degrade the
// preview: it would stop the commentary going on air, from a feature that is off
// by default and that nobody would think to suspect.
//
// So the branch uses queue's own properties (leaky, max-size-*), GstBaseSink's
// own property (sync), capsfilters, and NOTHING ELSE. drop-only on videorate
// would be a real improvement and is deliberately absent for exactly this reason.
// Everything else the sink wants — force-aspect-ratio, the two spellings of
// "do not take navigation events" — is set afterwards, in preview_cgo.go, behind
// hasProperty, where a missing property is a log line. TestPreviewBranchUsesOnlyPropertiesThatCannotFailToParse
// in preview_test.go fails by name if a property outside that set appears here.
//
// # IT IS BUILT AT START FROM CONFIG AND IS NEVER TOGGLED LIVE
//
// MEASURED, and it is the most expensive lesson behind this file:
// set_state(NULL) on the preview branch inside a blocking pad probe took the
// ON-AIR leg from 50 fps to 0 PERMANENTLY, with the pipeline still reporting
// PLAYING and no error anywhere. There is therefore no SetPreviewEnabled, no
// live add and no live remove. The operator's choice is read at Start, the
// branch is either in the parse string or it is not, and changing it means Stop
// and Start — which is what changing the capture device already means.
//
// "OFF" IS THE ABSENCE OF EVERYTHING. No queue, no scaler, no sink object, no GL
// context, and not one byte of this branch in the parse string: previewBranchFor
// returns the empty string and the pipeline is character for character the one
// that ships today. The tee stays, because it belongs to the video leg rather
// than to the preview, and allow-not-linked=true is what makes a tee with one
// consumer legal.
//
// # "CAN WE SEE THE PICTURE BEFORE GOING LIVE?" — YES, AND IT NEEDS NOTHING NEW
//
// This is the question the paragraph above looks like it forecloses, and it does
// not. The answer was measured on 2026-08-16 on the fitted card
// (TestLivePreviewBeforeStartNeedsNoNewPipeline in coughmute_live_test.go) and
// it is worth stating here, because the two designs a reader will reach for are
// both dangerous and NEITHER IS NECESSARY.
//
// The two dangerous ones first, so nobody re-derives them:
//
//   - A PREVIEW-ONLY PIPELINE THAT START TAKES THE CARD FROM. The card is
//     exclusive — two decklinkvideosrc in one process fail 3/3, two processes
//     fail 3/3 — so there is no atomic handover, and a failed reacquire is NO
//     FEED, which is far worse than no preview. (It would probably work:
//     TestLiveCardReleaseAndReacquire measured 12 of 12 clean release-and-retake
//     cycles in one process, 56-98 ms each. "Probably" is not the bar for the
//     thing that carries the match, and the design is not needed anyway.)
//   - ATTACHING THE ENCODER AND MUX BRANCH AT START to a graph already running.
//     That is the live add/remove this file's previous section measured taking
//     the on-air leg to 0 fps permanently.
//
// And now the reason neither is needed. START ALREADY BRINGS THE WHOLE CAPTURE
// CHAIN TO PLAYING WITH NO SINK INSTALLED. That is the documented contract of
// gst.Pipeline.Start, not an accident of it: the pipeline runs, srtq's src pad
// is held by a blocking probe, and NOTHING LEAVES THE PROCESS until ReplaceSink
// is called. This branch hangs off the video capture tee, upstream of the
// encoder and far upstream of srtq, so it is rendering in precisely that state.
//
// Measured in that state — Start called, ReplaceSink never called:
//
//	programme meter frames in 2.5 s   49   (the full 20 a second)
//	capture channels negotiated       16   (the card's own, matrix written)
//	cough mute settable               yes
//	pipeline                          PLAYING, nothing pending
//
// So "see and hear before starting" is not a pipeline problem. It is the
// application calling gst.Start when a capture device is configured and NOT
// calling ReplaceSink until the operator says go — a split that internal/sender
// already makes internally and that the frozen Pipeline interface already
// exposes as two separate methods. Nothing in internal/gst has to change, and
// nothing in internal/gst was changed for it, which is why there is no
// half-built preview lifecycle in this package to go wrong.
//
// # A DEAD PREVIEW MUST NOT BE ABLE TO TAKE THE FEED DOWN
//
// Two mechanisms, because there are two ways it could:
//
//  1. AT RUN TIME, a GST_ELEMENT_ERROR from a preview element. Every element
//     here is named with previewNamePrefix, and capturefault.go's classifier
//     spares that prefix outright — not as a video-capture error, which upgrades
//     to FATAL when the commentary audio is clocked by the card's video (see
//     capturefault.go), but as its own class that can never upgrade to anything.
//     isPreviewSourced is this file's half of that.
//
//  2. AT START, a sink that will not come up at all — no GL context, no D3D11
//     device. That failure is a state-change failure and never reaches the bus
//     classifier, so it is answered where the state change is made: Start builds
//     once more WITHOUT the preview before giving up. See the note on
//     previewBranchFor. What this file can do on its own it does — the sink
//     factory is resolved against the registry that is really loaded and the
//     branch is dropped if it is absent, and a preview with nowhere to draw is
//     never built at all.
//
// # What this file is, and what its two twins are
//
// This file carries no build tag: the branch string, the names, the option
// validation and the bus-classification predicate are ordinary Go that Gate A
// compiles and tests with no GStreamer anywhere. The seam it needs is ONE
// function — resolvePreviewSink, supplied by preview_cgo.go and by
// preview_stub.go, exactly as newPicturePipe is the seam in picture.go.
//
// attachPreview is the other half of preview_cgo.go and is deliberately NOT part
// of that seam. Its only caller is gst_cgo.go's Start, which carries the same
// build tag, so it has no Gate A twin at all; preview_stub.go says why in the
// place a reader would go looking for one.
package gst

import (
	"log"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Names
// ---------------------------------------------------------------------------

// The element names inside the preview branch, and the tee it hangs off.
//
// They are load-bearing in the same way capturefault.go's are: the bus filter
// classifies on the name of the element that posted, so a renamed element here
// silently rejoins the FATAL default and a preview sink that dies mid-match
// takes the commentary with it. That is why they are constants in this file
// rather than literals in the string below, and why preview_test.go asserts that
// every one of them carries the prefix.
const (
	// previewNamePrefix is what every element of the preview branch is named
	// with, and it is the whole mechanism by which the bus filter spares them.
	// It is a PREFIX and not a set of names for the reason
	// videoCaptureNamePrefix is: the branch fails as one unit — a sink that
	// cannot render makes the converter above it fail too — so classifying the
	// elements individually would be five constants that must never disagree,
	// and it would not cover an element added later.
	//
	// "vprev" and not "vp": it must not be a prefix of, or prefixed by, any
	// other classified name. TestPreviewNamesCannotBeConfusedWithTheFeed pins
	// that against every name the classifier knows.
	previewNamePrefix = "vprev"

	namePreviewQueue = "vprevq" // the leaky head queue: half the defence
	// namePreviewSinkQueue is the OTHER half, immediately above the sink, and
	// it is the half that actually works — the head queue never fills because
	// videorate drains it. Both are named so isPreviewSourced can spare them
	// and so the guard test can assert the branch still has two.
	namePreviewSinkQueue = "vprevsinkq"
	namePreviewRate      = "vprevrate"  // videorate, capping at 12.5 fps
	namePreviewScale     = "vprevscale" // videoscale, down to 480x270
	namePreviewConvert   = "vprevconv"  // videoconvert, into whatever the sink takes
	namePreviewSink      = "vprevsink"  // glimagesink or d3d11videosink

	// namePreviewTee is the tee the branch hangs off, and IT IS NOT A PREVIEW
	// ELEMENT. It sits in the LIVE CAPTURE video leg, it is there whether or not
	// the preview is, and if it ever fails it is the video leg that has failed —
	// so it carries capturefault.go's videoCaptureNamePrefix and is classified
	// exactly as the rest of that leg is. That gives the right answer in all
	// four combinations: recoverable when the commentary comes from elsewhere,
	// fatal and NAMED when the commentary audio is clocked by the card's video,
	// and unaffected by whether the preview is built.
	//
	// IT IS gst_cgo.go's ELEMENT AND THIS IS A MIRROR OF ITS nameVideoCapTee,
	// duplicated for the reason capturefault.go duplicates three of its names: a
	// build tag, not a preference. gst_cgo.go is `cgo && !gststub`, so at Gate A
	// — where the branch string is tested — its constants do not exist. One
	// duplicated short string is the price, and it is not left to trust:
	// TestPreviewTeeMatchesThePipeline reads gst_cgo.go and fails by name if the
	// two ever disagree. They must not, because a preview branch naming a tee
	// that is not in the string is not a preview that fails to appear — it is a
	// gst_parse_launch error, which is the WHOLE CONTRIBUTION PIPELINE failing
	// to start.
	//
	// It is also why the slate leg has no preview: it has no tee. See
	// PreviewOpts.wanted.
	namePreviewTee = "vcaptee"
)

// PreviewSurfacePurpose is the word this application calls the preview's native
// surface by: it names the child window on Windows and every overlay log line on
// both platforms. Pass it to gst.NewOverlaySurface.
//
// It is exported and lives here because app.go creates the surface and this file
// is the only place that knows what the second surface is FOR. The picture's own
// purpose is a literal inside each overlay file's NewPictureOverlay, which is the
// one that has shipped and does not move.
const PreviewSurfacePurpose = "preview"

// ---------------------------------------------------------------------------
// The shape of the picture
// ---------------------------------------------------------------------------

// The raster and rate the preview is scaled and paced to.
//
// They are CONSTANTS rather than options, and that is a decision rather than an
// omission: the 5.4-7.2 % of a core in the file header is a measurement OF THESE
// NUMBERS, and a knob that let a facility ask for 1280x720p50 would be a knob
// that let a facility put the broadcast leg's own cost back on the machine
// twice over without anyone measuring it again. Changing them is one line here,
// and the line is next to the measurement it invalidates.
//
// 480x270 is 16:9 and both axes are even, which matters for the same NV12 reason
// ConformTarget.resolve gives: the chroma plane is half resolution on both axes,
// and an odd dimension is a caps negotiation that fails without naming the
// number that caused it. The picture is upscaled by the sink into whatever
// rectangle the page gives the overlay, with force-aspect-ratio on, so a large
// rectangle shows a soft picture rather than a wrong one — which is the correct
// trade for a confidence monitor and is the whole reason it is cheap.
const (
	previewWidth  = 480
	previewHeight = 270

	// 25/2 is 12.5 fps, written as the exact fraction GStreamer's framerate
	// field requires. A quarter of 50 and a half of 25, so on every conform
	// target this application has met, videorate only ever DROPS frames and
	// never duplicates one.
	previewFrameRateNum = 25
	previewFrameRateDen = 2

	// previewQueueBuffers is the head queue's depth. Two, so it leaks on the
	// third frame the preview has not taken: deep enough to absorb the ordinary
	// jitter of a renderer, far too shallow to hold a backlog worth having. See
	// the file header for what happens when it does not leak at all.
	previewQueueBuffers = 2
)

// ---------------------------------------------------------------------------
// Options
// ---------------------------------------------------------------------------

// PreviewOpts is the operator's choice about the confidence monitor, as it
// reaches the pipeline. It is a field of PipelineOpts and is read exactly once,
// at Start; see the file header for why there is no live toggle.
//
// The ZERO VALUE IS OFF, and off is the default everywhere: in config, in the
// preset classification, and here. A machine nobody has configured builds the
// pipeline it builds today.
type PreviewOpts struct {
	// Enabled is the operator's request. It is a request and not a command:
	// previewBranchFor answers it with the empty string whenever the preview
	// cannot be built — no surface to draw in, no sink in this build's
	// GStreamer — and says why in the log rather than failing Start.
	Enabled bool

	// WindowHandle is the native surface the preview is presented into, as a
	// uintptr: an HWND on Windows and an NSView* on macOS, exactly as
	// PictureOpts.WindowHandle is, and for the reasons documented there.
	//
	// IT MUST BE THE PREVIEW'S OWN SURFACE AND NEVER THE PICTURE'S. Two sinks
	// rendering into one surface is not a layout problem, it is two GL contexts
	// or two swapchains attached to one view; and on macOS glimagesink ADDS A
	// SUBVIEW to whatever it is handed and removes it again on teardown, so the
	// second one to stop would take the first one's picture with it.
	//
	// ZERO MEANS THE PREVIEW IS NOT BUILT, and that is a normal state rather
	// than a fault: the overlay surface cannot exist before Wails has made the
	// window, and app.go creates it lazily on the first layout call. A Start
	// that happens before that legitimately has no handle. Zero must never be
	// passed through to the sink — a GstVideoOverlay with no window handle
	// opens its OWN top-level window, with a title bar and a close button, over
	// the commentator's screen.
	WindowHandle uintptr
}

// wanted reports whether a preview branch should be attempted at all, and says
// in the log why not when the answer is no.
//
// videoCapture is PipelineOpts.VideoCaptureID: empty when the video leg is the
// frozen slate, and a DeckLink persistent-id when it is a live capture.
//
// # THE SLATE CASE IS NOT A NICETY, IT IS THE ONE THAT WOULD TAKE THE FEED OFF AIR
//
// The tee this branch hangs off is built ONLY in the capture leg — see
// pipelineDescription, where the slate leg deliberately ends at its capsfilter
// with nothing after it. A branch rendered against a tee that is not in the
// string is not a preview that fails to appear: it is `vcaptee.` naming an
// element that does not exist, which gst_parse_launch refuses, which fails
// Start, which takes THE WHOLE CONTRIBUTION PIPELINE off air on a seat whose
// only mistake was leaving a checkbox ticked after switching back to the slate.
//
// So the test is here, structurally, in the function that decides whether a
// branch exists at all, rather than in a condition at the call site that a later
// edit could separate from it.
//
// The three refusals are deliberately silent-in-the-normal-case and loud
// otherwise: an operator who has not turned the preview on gets no line at all,
// and one who has turned it on and is not getting it gets a sentence saying
// which of the three reasons it was. A preview that is quietly not there is the
// failure this whole application spends its length preventing.
func (o PreviewOpts) wanted(videoCapture string) bool {
	if !o.Enabled {
		return false
	}
	if videoCapture == "" {
		log.Print("gst: preview: the confidence monitor is switched on and the video leg is the " +
			"SLATE, so there is no capture to be confident about and no tee to take one off. It is " +
			"not being built. Choose a DeckLink video input and the preview appears with it")
		return false
	}
	if o.WindowHandle == 0 {
		log.Print("gst: preview: the confidence monitor is switched on but there is no surface to " +
			"draw it in yet, so it is not being built. That is normal if the window has only just " +
			"opened — the surface is created by the page's first layout call — and it means the " +
			"preview will appear on the next Stop and Start rather than now")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// The branch
// ---------------------------------------------------------------------------

// previewBranchFor renders the preview branch for one Start, or the empty string
// if there is not going to be one.
//
// It is the ONLY function that decides whether the preview happens, and it
// decides it before gst_parse_launch is ever called. The four ways it says no
// are the four ways the preview could otherwise fail loudly at a moment when
// nothing else about the pipeline has gone wrong:
//
//   - the operator has not asked for one, which is the default;
//   - the video leg is the slate, so there is no tee to hang it off — the case
//     that would otherwise be a parse failure of the whole pipeline, see wanted;
//   - there is no surface to draw in yet;
//   - this build's GStreamer has no video sink this application will use, which
//     would be a bundling mistake — and a missing element in a parse string is a
//     parse failure of the WHOLE contribution pipeline, not of the branch.
//
// WHAT IT CANNOT ANSWER, AND WHERE THAT IS ANSWERED. A sink that exists and then
// will not START — no GL context, no D3D11 device, a display that has gone away
// — fails inside the pipeline's NULL to PLAYING transition, which is a state
// change and not a bus error. Nothing here can see that coming. Start therefore
// treats a state-change failure with a preview in the graph as a reason to build
// the pipeline ONCE MORE WITHOUT IT before reporting failure: the commentary is
// the product and the confidence monitor is not, and a retry can only turn a
// failure into a success.
//
// The returned string BEGINS WITH A NEWLINE, so that it can be concatenated onto
// the end of pipelineDescription's string without the caller having to know
// whether there is a branch at all.
func previewBranchFor(opts PreviewOpts, videoCapture string) string {
	if !opts.wanted(videoCapture) {
		return ""
	}

	sink, err := resolvePreviewSink()
	if err != nil {
		log.Printf("gst: preview: the confidence monitor is switched on and %v. The commentary feed "+
			"is unaffected and is being built without it", err)
		return ""
	}

	log.Printf("gst: preview: building the confidence monitor: %dx%d at %s fps into %s, off the %s "+
		"tee. It is capped and scaled before it is converted, and its head queue leaks downstream, "+
		"so it cannot slow the leg it is watching",
		previewWidth, previewHeight, previewRateText(), sink, namePreviewTee)
	return previewBranch(sink)
}

// previewBranch renders the branch itself, for a sink factory that has already
// been established to exist.
//
// It is separated from previewBranchFor so that the string — which is the part
// that has to be exactly right and the part that cannot be run at Gate A — is a
// pure function of one argument, and preview_test.go can assert every element,
// every property and the ORDER of the two that matter without a registry, a
// window or cgo.
//
// An empty factory renders nothing. That is unreachable through previewBranchFor
// and is not decoration: it is what makes "no sink" and "no branch" the same
// value everywhere, so a caller cannot accidentally build "! name=vprevsink".
func previewBranch(sinkFactory string) string {
	if sinkFactory == "" {
		return ""
	}
	return "\n" + namePreviewTee + "." +
		// The leak, and the only thing standing between a slow renderer and the
		// broadcast leg. See the file header for the measurement.
		// max-size-bytes and max-size-time are pinned to unlimited so that the
		// buffer count is the only limit that can fire.
		" ! queue name=" + namePreviewQueue +
		" leaky=downstream max-size-buffers=" + strconv.Itoa(previewQueueBuffers) +
		" max-size-bytes=0 max-size-time=0" +

		// The rate cap FIRST: everything below it runs at 12.5 fps rather than
		// at 50, which is most of why this branch is cheap.
		" ! videorate name=" + namePreviewRate +
		" ! video/x-raw,framerate=" +
		strconv.Itoa(previewFrameRateNum) + "/" + strconv.Itoa(previewFrameRateDen) +

		// Then the scale, and THEN the convert. This order is the measurement
		// in the file header: converting 1920x1080 and scaling the result costs
		// about four times what scaling first and converting 480x270 costs.
		" ! videoscale name=" + namePreviewScale +
		" ! video/x-raw,width=" + strconv.Itoa(previewWidth) +
		",height=" + strconv.Itoa(previewHeight) +
		" ! videoconvert name=" + namePreviewConvert +

		// THE SECOND LEAKY QUEUE, AND IT IS THE ONE THAT ACTUALLY DEFENDS THE
		// BROADCAST LEG. The head queue above cannot: videorate sits below it
		// and discards three of every four buffers immediately, so that queue
		// DRAINS FASTER THAN IT FILLS and its leak never fires. The blocking a
		// slow renderer causes happens down here, below the only queue the
		// branch had, where back-pressure walks straight up through videorate
		// to the tee — and tee pushes serially on the upstream thread.
		//
		// MEASURED on the real card, the exact shipped leg, on-air branch
		// fakesink sync=true num-buffers=500, preview sink made slow with
		// identity sleep-time=100000 (100 ms/frame against this branch's 80 ms
		// budget):
		//
		//	no preview at all                     10.09 s   49.6 fps on air
		//	head queue only (what shipped first)  13.67 s   36.6 fps ON AIR
		//	with this queue as well               10.34 s   48.4 fps on air
		//
		// Element isolation with the same harness put it beyond doubt:
		// queue->slow, queue->videoscale->slow and queue->videoconvert->slow
		// were all protected; queue->VIDEORATE->slow was not. And a sweep of
		// sink cost against the budget showed there was no protection at all
		// rather than a wide margin: 20/40/60 ms all 10.05 s, 79 ms 11.31 s,
		// 100 ms 13.96 s — fine while the renderer keeps up, degrading from the
		// moment it does not.
		//
		// A confidence monitor dropping frames is the CORRECT behaviour: it
		// exists to answer "is the card seeing this", and a stale frame answers
		// that just as well as a fresh one. The broadcast feed slowing down is
		// not correct behaviour under any circumstances, which is why the leak
		// belongs on both sides of the rate cap and not only above it.
		" ! queue name=" + namePreviewSinkQueue +
		" leaky=downstream max-size-buffers=" + strconv.Itoa(previewQueueBuffers) +
		" max-size-bytes=0 max-size-time=0" +

		// sync=false, for the reason pictureSinkSync gives at length on the
		// other path: there is no audio in this branch for a clock to line the
		// video up against, so honouring the running time would buy a delay and
		// nothing else. A confidence monitor is the one picture in this
		// application that must be as close to NOW as it can be, because its
		// entire job is to answer "is the card seeing this".
		//
		// Everything else the sink wants is set in preview_cgo.go behind
		// hasProperty. Only GstBaseSink's own properties may appear here; see
		// the file header.
		" ! " + sinkFactory + " name=" + namePreviewSink + " sync=false"
}

// previewRateText renders 25/2 as "12.5" for a log line. It is deliberately not
// ConformTarget.rateText — that is the switcher's format being reported to an
// operator who is comparing it against a switcher, and this is a fixed internal
// constant. Sharing them would tie one to the other for no gain.
func previewRateText() string {
	return strconv.FormatFloat(float64(previewFrameRateNum)/float64(previewFrameRateDen), 'g', -1, 64)
}

// ---------------------------------------------------------------------------
// The bus
// ---------------------------------------------------------------------------

// isPreviewSourced reports whether a bus message came from the preview branch.
//
// It is this file's half of the rule in capturefault.go: a preview element's
// error is SPARED — the pipeline stays PLAYING, the gate stays open, the
// commentary keeps flowing, and the error is logged as a warning and never
// reaches Errors(), because internal/sender treats any error arriving while
// CONNECTED as the peer going away and would spend a whole DRAINING/BACKOFF
// cycle, seven seconds off air, over a monitor nobody is looking at.
//
// # Why it is its own class and not the video-capture one
//
// classVideoCapture is spared IN THE ORDINARY CASE and UPGRADED TO FATAL when
// the commentary audio is clocked by the card's video — which is the DeckLink
// configuration this whole tier is about, so it would upgrade nearly every time.
// That upgrade is right for the capture leg, where the video element really is
// the clock the commentary is captured against, and it is nonsense for a branch
// that is downstream of everything and feeds a window. A preview class that
// could ever upgrade would be a preview that takes the commentary off air on the
// exact machines it was built for.
//
// So the classifier asks this question early and answers it absolutely. The one
// thing that must stay true for it to keep working is that no preview element is
// ever named anything not covered by the prefix; preview_test.go pins that.
func isPreviewSourced(source string) bool {
	return strings.HasPrefix(source, previewNamePrefix)
}
