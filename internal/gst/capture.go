// capture.go is the ALWAYS-LIVE CAPTURE LAYER's contract: what a capture
// pipeline is, which legs it owns, and the fusion rule that decides how many of
// them there are.
//
// It carries no cgo. Everything here is a decision that can be made — and tested
// at Gate A — before a single element exists, which is the point: the fusion rule
// below is the difference between "I changed my microphone" and "I changed my
// microphone and my camera died", and it must not be discoverable only by
// running it against the one card in the building.
//
// # Two roles, one type
//
//	PICTURE CAPTURE      built at domReady, rebuilt on video/preview/conform change
//	  slate | decklinkvideosrc -> conform chain -> [tee] -> leaky queue -> proxysink
//	                                                \-> preview branch (always live)
//
//	COMMENTARY CAPTURE   built at domReady, rebuilt on audio device change
//	  osxaudiosrc | decklinkaudiosrc -> audioconvert -> chlevel -> aconv (mix-matrix)
//	    -> resample -> seam caps -> coughmute -> alevel -> leaky queue -> proxysink
//	    [+ decklinkvideosrc clock companion -> fakesink, on a slate seat]
//
// Both roles are ONE Go type, parameterised by a leg-set, so the fused case is
// one instance owning both. That is what keeps the two-pipeline decision cheap:
// one description function, one bus handler, one fault channel type, one
// teardown. If the picture blank on a fused seat later turns out not to matter,
// PlanCapture returns an always-fused set and nothing else in this package
// changes.
//
// # Why two pipelines rather than one
//
// Because with a single capture pipeline, SELECTING A MICROPHONE REBUILDS YOUR
// PICTURE. R2 wants the routing panel to appear as soon as the device is
// selected, which means selecting a device must re-point capture; on one pipeline
// that closes and reopens the exclusive DeckLink and blanks the confidence
// monitor. Splitting picture from commentary is what makes device selection
// cheap.
//
// # The DeckLink is held from launch to quit
//
// Answered by the operator on 2026-08-16 (PLAN.md 0-BIS A1): the card is opened
// when the app starts and released when it quits, and nothing else on the machine
// can use it meanwhile. There is no Acquire/Release control to build. What makes
// that safe is three things, and all three are requirements on the layer above
// this one: the app NEVER fails to launch because of the card (the picture leg
// falls back to the slate and the fault goes on the CAMERA lamp), there is an
// explicit RestartCapture control, and a cable fault now surfaces at launch
// instead of twenty minutes before kick-off.
package gst

import (
	"errors"
	"fmt"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// The leg-set
// ---------------------------------------------------------------------------

// PictureLeg is which picture source one capture pipeline owns, if any.
type PictureLeg int

const (
	// PictureNone is a capture pipeline with no picture leg at all — the
	// commentary half of a split seat.
	PictureNone PictureLeg = iota

	// PictureSlate is filesrc ! pngdec ! ... ! imagefreeze: a still PNG, frozen
	// and paced at the conform target's rate. It is what every seat without a
	// camera sends, and what a card seat falls back to when the card cannot be
	// opened at launch.
	PictureSlate

	// PictureCard is decklinkvideosrc through the conform chain. It brings a tee
	// with it, because the confidence monitor cannot have a second
	// decklinkvideosrc: the card is EXCLUSIVE — two sources in one process fail
	// 3/3 and two processes fail 3/3.
	PictureCard
)

// CommentaryLeg is which commentary source one capture pipeline owns, if any.
type CommentaryLeg int

const (
	// CommentaryNone is a capture pipeline with no commentary leg — the picture
	// half of a split seat.
	CommentaryNone CommentaryLeg = iota

	// CommentaryNative is the platform's own capture element (osxaudiosrc /
	// wasapi2src) pointed at a CoreAudio or WASAPI endpoint, at whatever width
	// that endpoint presents. It is NOT a synonym for "stereo": a Focusrite or an
	// RME is unpositioned above two channels exactly as a card is, because
	// gstosxcoreaudio.c:886-889 sets `layout = NULL; /* no supported for sources */`
	// unconditionally for every source. Measured on this machine — a 3-channel
	// CoreAudio device negotiated channels=3 channel-mask=0x0 and a 16-in device
	// negotiated channels=16 channel-mask=0x0.
	CommentaryNative

	// CommentaryCard is decklinkaudiosrc channels=16: the card's embedded audio.
	// It cannot preroll without a decklinkvideosrc in the SAME pipeline, which is
	// the whole of the fusion rule below.
	CommentaryCard
)

// CaptureLegs is one capture pipeline's leg-set: everything the description
// builder and the fault classifier need to know about its shape, decided before
// anything is parsed.
//
// It is a value and not a read of a live pipeline because the bus handler runs on
// a streaming thread and may not take the pipeline's lock. captureLegsFor's
// parent-bin walk exists today only because the old single pipeline could not say
// at build time which shape it was; with a leg-set this is known before
// gst_parse_launch is called.
type CaptureLegs struct {
	Picture    PictureLeg
	Commentary CommentaryLeg

	// Preview is whether the confidence monitor branch hangs off this
	// pipeline's tee. It is only ever true with Picture == PictureCard: the
	// slate leg builds no tee, and a preview rendered against a tee that is not
	// there is a gst_parse_launch failure of the WHOLE pipeline rather than a
	// missing preview. previewBranchFor enforces that separately and this field
	// records the decision.
	Preview bool
}

// HasCard reports whether this leg-set puts ANY decklink element in the
// pipeline: the picture source, the commentary source, or the clock companion
// that a card commentary on a slate seat drags in.
//
// It is the predicate the fusion rule is written against, and the one a caller
// uses to answer "which of my pipelines is holding the card".
func (l CaptureLegs) HasCard() bool {
	return l.Picture == PictureCard || l.Commentary == CommentaryCard
}

// NeedsClockCompanion reports whether this leg-set has to build a
// decklinkvideosrc that exists only to clock decklinkaudiosrc.
//
// MEASURED: decklinkaudiosrc CANNOT PREROLL ALONE. The card drives audio capture
// off the video clock, so with no decklinkvideosrc in the same pipeline the audio
// branch produces ZERO buffers — 0 level messages against 160 — and the leg never
// starts. When the picture leg is also the card, THAT source is the clock and a
// second one is not an option to weigh, it is a pipeline that fails.
func (l CaptureLegs) NeedsClockCompanion() bool {
	return l.Commentary == CommentaryCard && l.Picture != PictureCard
}

// AudioClockedByVideo is capturefault.go's question, answered from the leg-set
// rather than from a bin traversal: is this pipeline's commentary captured
// against a decklinkvideosrc's clock, so that a VIDEO element's error is an AUDIO
// fault wearing a video element's name?
//
// It is true on the fused seat (vcapsrc clocks both) and on the card-commentary
// slate seat (vcapclock clocks the commentary and feeds nothing else). It is
// false on a picture-only card pipeline, where the commentary is somewhere else
// entirely and a video fault is a frozen picture and nothing more.
func (l CaptureLegs) AudioClockedByVideo() bool {
	return l.Commentary == CommentaryCard
}

// Valid refuses a leg-set nothing can build, by name, before anything is parsed.
func (l CaptureLegs) Valid() error {
	if l.Picture == PictureNone && l.Commentary == CommentaryNone {
		return errors.New("gst: a capture pipeline with neither a picture leg nor a commentary " +
			"leg has nothing to build")
	}
	if l.Preview && l.Picture != PictureCard {
		return errors.New("gst: the confidence monitor hangs off the capture tee, which only the " +
			"card picture leg builds; a preview on a slate leg is a gst_parse_launch failure of " +
			"the whole pipeline rather than a missing preview")
	}
	return nil
}

// String names the shape for a log line, using PLAN.md section 2's own labels so
// that a log and the plan can be read side by side.
func (l CaptureLegs) String() string {
	switch {
	case l.Picture == PictureCard && l.Commentary == CommentaryCard && l.Preview:
		return "FUSED-PREVIEW"
	case l.Picture == PictureCard && l.Commentary == CommentaryCard:
		return "FUSED"
	case l.Picture == PictureCard && l.Preview:
		return "P-CARD-PREVIEW"
	case l.Picture == PictureCard:
		return "P-CARD"
	case l.Picture == PictureSlate:
		return "P-SLATE"
	case l.Commentary == CommentaryCard:
		return "C-CARD"
	case l.Commentary == CommentaryNative:
		return "C-NATIVE"
	default:
		return "EMPTY"
	}
}

// ---------------------------------------------------------------------------
// The fusion rule
// ---------------------------------------------------------------------------

// CaptureSources is what the operator has configured, reduced to the four facts
// the fusion rule turns on. It is deliberately not config.Config: this package
// does not read configuration, and a struct the caller fills in is what keeps the
// rule testable at Gate A with no files on disk.
type CaptureSources struct {
	// VideoCaptureID is the DeckLink persistent-id the picture comes from, or
	// empty for the slate.
	VideoCaptureID string

	// AudioCaptureID is the DeckLink persistent-id the commentary comes from, or
	// empty for the platform microphone. Exactly one of it and AudioDeviceID is
	// given; refuseWrongAudioSource is the rule and both twins call it.
	AudioCaptureID string

	// AudioDeviceID is the CoreAudio / WASAPI endpoint the commentary comes
	// from, when AudioCaptureID is empty.
	AudioDeviceID string

	// Preview is whether the operator has asked for the confidence monitor. It
	// is honoured only on a card picture leg; see CaptureLegs.Preview.
	Preview bool
}

// PlanCapture applies THE FUSION RULE and returns the leg-sets to build, in the
// order picture-then-commentary. One element means a fused pipeline owning both
// legs; two means two independent pipelines.
//
//	picture   commentary   pipelines
//	slate     CoreAudio    two, independent
//	slate     card         two, independent (the commentary carries the clock companion)
//	card      CoreAudio    two, independent
//	card      card         ONE FUSED PIPELINE
//
// # Why the fused row exists, and why it is not negotiable
//
// decklinkaudiosrc cannot preroll without a decklinkvideosrc in the SAME
// pipeline, and the card is EXCLUSIVE — two decklink sources in one process fail
// 3/3, and two processes fail 3/3. Those two facts together mean AT MOST ONE
// PIPELINE MAY EVER CONTAIN DECKLINK ELEMENTS. The operator confirmed the
// exclusivity himself; the preroll requirement is measured (0 buffers, 0 level
// messages against 160).
//
// So when both legs are on the card they must share a pipeline, and the price is
// that only that seat pays a picture blank when the commentary moves on or off the
// card. That is a setup action, done once, and it is the cheaper half of the
// trade: the alternative is that a microphone change closes and reopens the card
// on EVERY seat that has one.
//
// # What the caller must not do with the result
//
// Build them in the order returned and tear them down in the reverse order. A
// picture pipeline holding the card must be at NULL before a commentary pipeline
// that wants it is taken out of NULL, or the second one meets the card's
// contention failure — "Internal data stream error / not-negotiated (-4)" in
// about 100 microseconds, naming neither the device nor the cause.
func PlanCapture(src CaptureSources) []CaptureLegs {
	picture := PictureSlate
	if src.VideoCaptureID != "" {
		picture = PictureCard
	}
	commentary := CommentaryNative
	if src.AudioCaptureID != "" {
		commentary = CommentaryCard
	}
	// The preview is honoured only where there is a tee to hang it off.
	preview := src.Preview && picture == PictureCard

	// THE FUSED ROW. Both legs on the card, so they share one pipeline and one
	// decklinkvideosrc clocks both. Note what is NOT checked here: whether the
	// two ids name the SAME card. config.json carries one decklinkPersistentId
	// for both legs so the application cannot produce two, and NewCapture refuses
	// a hand-built two-card set by name — a commentary leg on one card cannot be
	// clocked by another card's video, and the card being exclusive means there
	// is no third element available to clock it.
	if picture == PictureCard && commentary == CommentaryCard {
		return []CaptureLegs{{Picture: PictureCard, Commentary: CommentaryCard, Preview: preview}}
	}

	return []CaptureLegs{
		{Picture: picture, Preview: preview},
		{Commentary: commentary},
	}
}

// ---------------------------------------------------------------------------
// The interface
// ---------------------------------------------------------------------------

// CaptureOpts configures one capture pipeline. It is PipelineOpts' capture half,
// which is where nine of that struct's thirteen fields are going.
//
// Everything about the SEND pipeline is absent by construction: no bitrates, no
// encoder, no SRT endpoint, no conform target for the send side. A capture
// pipeline cannot be configured to send and a send pipeline cannot be configured
// to open a device, which is what makes "a send pipeline with no device behind
// it" unconstructible rather than merely unlikely.
type CaptureOpts struct {
	// Legs is the shape, from PlanCapture. Required.
	Legs CaptureLegs

	// SlatePath is the PNG fed to filesrc ! pngdec ! imagefreeze. Required
	// whenever Legs.Picture is PictureSlate and ignored otherwise.
	SlatePath string

	// AudioDeviceID / AudioCaptureID are the commentary source, exactly as
	// PipelineOpts documents them: exactly one is given, and the emptiness is the
	// dangerous half — an empty device on osxaudiosrc or wasapi2src is not an
	// error, it is THE SYSTEM DEFAULT INPUT, which is how a match goes on air off
	// the laptop's built-in microphone with every lamp green.
	AudioDeviceID  string
	AudioCaptureID string

	// VideoCaptureID is the DeckLink persistent-id the picture leg opens. It is
	// a persistent-id and never an audio device id; the value is converted rather
	// than pattern-matched, because an id the element will not take leaves
	// persistent-id at its own -1 default, which means "use device-number", which
	// means whichever card the driver enumerated first.
	VideoCaptureID string

	// Preview carries the confidence monitor's window handle and switch. A ZERO
	// WINDOW HANDLE MEANS THE BRANCH IS NOT RENDERED AT ALL, because an
	// unattached GstVideoOverlay opens its own top-level window, with a title bar
	// and a close button, over the commentator's screen. previewBranchFor keeps
	// that whole contract unchanged.
	Preview PreviewOpts

	// ConformTo is the raster and rate the picture leg is conformed to. It is on
	// the CAPTURE side and not the send side, and that is the single most
	// consequential placement decision in the seam: with the conform chain here,
	// the caps crossing the proxy are pinned for the life of the process, so the
	// card changing raster (720x486 NTSC placeholder when it has no lock, the
	// real input when it gets one, back again when the cable is pulled)
	// renegotiates videoscale's SINK caps only and the encoder never sees a caps
	// change at all.
	ConformTo ConformTarget

	// DeviceChannels is the commentary device's width AS THE ENUMERATION
	// REPORTED IT — structure 0 of the GstDevice's caps, read during the
	// ordinary provider walk. It is a query and opens nothing; verified for
	// every Audio/Source on this machine (built-in mic channels=1, NDI
	// channels=2 mask=0x3, UltraStudio-via-CoreAudio channels=16 mask=0x0).
	//
	// It is what sizes the matrix, because the matrix has to be written while
	// the pipeline is still in NULL and a native source's pad publishes a RANGE
	// there — osxaudiosrc's src template is `channels: [1, 2147483647]`, which
	// fixes nothing. A DeckLink commentary ignores this field: the width is the
	// constant 16 by construction, because the description states it on the
	// element rather than discovering it.
	//
	// ZERO MEANS "NOT KNOWN", and the source pad is asked instead. If that also
	// fixes no count, NO MATRIX IS WRITTEN and the log says so — a guessed width
	// does not degrade the feed, it stops the capture chain with "streaming
	// stopped, reason error (-5)", which reads as a broken device.
	DeviceChannels int

	// ChannelMap is the routing written to aconv's mix-matrix before the
	// pipeline leaves NULL, because a matrix is a NEGOTIATION CONSTRAINT and not
	// a gain. It is written UNIFORMLY, at every width including 1 and 2 —
	// measured working at 1, 2, 3, 16 and 32 — so there is no source-kind test
	// anywhere in this package.
	ChannelMap ChannelMap

	// MuteCommentary is the cough mute's state at build time. A latch set before
	// START is still set at START (PLAN.md 0-BIS A2): the mute sits upstream of
	// alevel, so a muted commentator has a flat programme meter AND a mute
	// banner, before and after START. The fear the old no-session refusal
	// answered was of a control that lies, and it is met by VISIBILITY rather
	// than by unavailability.
	MuteCommentary bool

	// OnLevels receives the programme meter's frames from alevel: the exact
	// S16LE 48 kHz stereo that crosses the seam. Nil means no metering.
	OnLevels func(Levels)

	// OnChannelLevels receives the per-channel picker's frames from chlevel: the
	// capture device's OWN channels, before any routing has been applied, which
	// is what lets the operator ask the commentator to talk and watch which bar
	// moves. Nil means no per-channel metering, and the element is silenced at
	// its own post-messages property rather than by a nil check on a streaming
	// thread.
	OnChannelLevels func(Levels)

	// OnSignal receives the video signal watchdog's edges. Nil means no
	// watchdog, and on a pipeline with no element carrying a "signal" property
	// there is no goroutine and no ticker either way.
	OnSignal func(SignalReport)

	// OnInputChannels is published when aconv's sink pad negotiates a width,
	// STAMPED WITH THE DEVICE IT BELONGS TO.
	//
	// The stamp is not optional. Without it there is a window between selecting a
	// Focusrite and the capture renegotiating in which the routing grid still
	// holds the previous device's sixteen, and a crosspoint pressed in that
	// window writes a 2x16 matrix onto a two-channel pad — the measured
	// "streaming stopped, reason error (-5)", which reads as a broken device
	// rather than as a bad matrix.
	OnInputChannels func(deviceKey string, width int)
}

// channelMeterWanted is PLAN.md 4.3's arming condition for the per-channel
// picker, in ONE place because both twins ask it and two copies would eventually
// disagree — and the disagreement would be invisible, because a picker that is
// wrongly armed looks exactly like a picker that is rightly armed.
//
// The condition changed with R2 and the change is the point: chlevel is armed
// when the pad presents MORE channels than the two the programme meter already
// reports, NOT when a matrix happened to be written. Every seat now carries a
// matrix, so the old condition would arm sixteen bars on a stereo microphone — a
// duplicate of the programme meter, ten times a second, over the webview bridge,
// for the whole of a ninety-minute match. The cost argument channelmap.go
// records therefore survives intact.
//
// It lives here rather than in either twin so that Gate A can assert the rule at
// every width the operator named; behind the cgo tag it would be untestable
// without GStreamer.
func channelMeterWanted(width int, haveCallback bool) bool {
	return width > ChannelMapOutputs && haveCallback
}

// DeviceKey is the stamp OnInputChannels carries and the key config.json's
// per-device channel maps are stored under: "<kind>:<id>".
//
// One capture pipeline has exactly one commentary device, so this is a function
// of the opts and not of anything live. A pipeline with no commentary leg has no
// key and returns the empty string, which is what stops a picture pipeline
// publishing a width for a device it does not own.
func (o CaptureOpts) DeviceKey() string {
	switch {
	case o.Legs.Commentary == CommentaryCard:
		return string(KindDeckLink) + ":" + o.AudioCaptureID
	case o.Legs.Commentary == CommentaryNative:
		return string(KindNative) + ":" + o.AudioDeviceID
	default:
		return ""
	}
}

// CapturePipeline is one always-live capture pipeline.
//
// It is built at domReady and lives until the device it opens changes or the
// application quits. It NEVER sends: it ends in one or two proxysinks and knows
// nothing about SRT, the encoder, the muxer or the session.
//
// All methods are safe for concurrent use. Every one of them may be called with
// no send pipeline anywhere in the process, which is the whole of R1: the
// meters, the preview, the routing width, the signal lamp and the mute exist
// before START and survive STOP.
type CapturePipeline interface {
	// Legs reports the shape this pipeline was built to. It never changes; a
	// different shape is a different pipeline.
	Legs() CaptureLegs

	// Start takes the built pipeline to PLAYING, confirms the negotiated channel
	// width against the matrix that was written in NULL, and arms the signal
	// watchdog. It returns an error if called twice or after Stop.
	Start() error

	// ArmForSend re-arms every proxysink this pipeline owns, so that the send
	// pipeline about to attach receives STREAM_START, CAPS and SEGMENT.
	//
	// IT IS CALLED AT START AND NOT AT STOP, and that is not a preference. STOP
	// has abnormal paths through which the arming can be skipped — an aborted
	// teardown, a crash between the two halves, a Stop that failed after taking
	// one leg to NULL — and START has none, because it cannot proceed without
	// it. Arming at START also makes the invariant self-healing: it runs on the
	// first START as a no-op, after a STOP that failed halfway, after an aborted
	// send build, and it can never race a reconnect because no send pipeline
	// exists at that instant.
	//
	// Measured at 108-511 microseconds for both branches together, on a capture
	// pipeline carrying 1080p50 NV12 and 48 kHz stereo.
	ArmForSend() error

	// ProxySinks returns the proxysink names this pipeline owns, in the order
	// video-then-audio, so a caller can tell which half of the seam it is
	// looking at without knowing the leg-set.
	ProxySinks() []string

	// ClaimForSend takes the SINGLE-CONSUMER claim on every proxysink this
	// pipeline owns, or fails with ErrSeamBusy naming the one already taken.
	//
	// A second proxysrc attaching to a live proxysink SILENTLY STEALS THE STREAM
	// AND KILLS THE FIRST — measured, A stopped dead at 5.994 s the instant B
	// attached at 6.007 s, nothing on either bus. There is no refusal inside the
	// element, so this is the refusal.
	ClaimForSend() error

	// ReleaseFromSend gives the claim back. It is idempotent, because the path
	// that calls it is a teardown and a teardown that can fail twice must be
	// callable twice.
	ReleaseFromSend()

	// InputChannels reports how many channels aconv's sink pad ACTUALLY
	// NEGOTIATED, read from that pad's current caps at the moment of the call.
	// It is 0 on a pipeline with no commentary leg and 0 before anything has
	// negotiated.
	InputChannels() int

	// SetChannelMap rewrites the routing while the pipeline is PLAYING, with no
	// state change, no renegotiation and no interruption. Measured at 119 us on
	// the real card.
	SetChannelMap(m ChannelMap) error

	// SetCommentaryMute writes the cough mute and READS IT BACK OFF THE ELEMENT,
	// which is what makes CommentaryMuted an observation rather than a memory.
	// It succeeds with no session running: the element exists from launch.
	SetCommentaryMute(mute bool) error

	// CommentaryMuted is the read-back value, answered without taking the lock
	// that is held across state changes. A mute lamp that stops answering while
	// a device is being opened is a mute lamp nobody can trust at the moment
	// they most need it.
	CommentaryMuted() bool

	// Health is nil while this capture's COMMENTARY leg is carrying media and the
	// latched fault once the pipeline has died.
	//
	// It exists because a latched death does NOT stop the object working: every
	// method still answers, ClaimForSend still succeeds, and on a dead device the
	// arming's IDLE probe fires immediately — the pad is idle because it is dead —
	// so the seam reports itself armed over a producer that will never push. START
	// must ask this before it builds a send pipeline; Faults() cannot answer it,
	// being a channel the application has already drained.
	Health() error

	// PictureHealth is nil while this capture's PICTURE leg is carrying media and
	// the latched, NAMED fault once it has died.
	//
	// IT IS A SECOND QUESTION AND NOT A REFINEMENT OF THE FIRST, because the two
	// deaths have different repairs. A picture death leaves the commentary being
	// captured, metered, routed and muted, so this pipeline is not "down" and must
	// not be reported as such — but nothing downstream of the muxer can tell the
	// difference, because mpegtsmux emits nothing at all while one of its two
	// inputs is silent. ArmForSend therefore refuses on either, and this is what
	// lets the CAPTURE PANEL say which of the two the operator is looking at.
	//
	// Before it existed a picture-leg death reached deliverWarning and stopped
	// there: nothing on screen changed, Health() answered nil, START reached
	// PLAYING and the operator was refused two seconds later by the muxer
	// watchdog's "nothing reached vq:src" — the pad, not the cause.
	PictureHealth() error

	// Faults carries this capture pipeline's fatal errors. It is NOT the send
	// pipeline's Errors(): internal/sender reads any error arriving while
	// CONNECTED as the peer going away, and would spend a whole DRAINING/BACKOFF
	// cycle — seven seconds off air — on a fault that is not the network's.
	Faults() <-chan error

	// Warnings carries the spared classes: a video capture failure on a seat
	// whose commentary is elsewhere, and every confidence-monitor failure.
	Warnings() <-chan string

	// Stop takes the pipeline to NULL, releases the device, joins the signal
	// watchdog and closes the fault channel. It is idempotent, and it must be
	// called on EVERY pipeline that was constructed — including one whose Start
	// failed and one that is abandoned unstarted — because it is the only thing
	// that ends the goroutines parked on those channels.
	//
	// IT REFUSES, with an error wrapping ErrSeamBusy, while a send pipeline still
	// holds the seam. Taking the device to NULL underneath a bound proxysrc is
	// measured silent in every direction: 0 buffers, no EOS, no ERROR and no
	// WARNING on either bus, the send pipeline still PLAYING and SRT still
	// connected. Stop the send session first — SendSeam.Stop releases the claim
	// and is idempotent — which is the order every caller was supposed to use
	// anyway.
	Stop() error
}

// CaptureSet is the App-side ownership of the planned pipelines. Picture and
// Commentary are THE SAME OBJECT on a fused seat, which is what
// CaptureLegs.HasCard being true on one pipeline is arranged to mean.
type CaptureSet struct {
	Picture    CapturePipeline
	Commentary CapturePipeline
}

// Pipelines returns the distinct pipelines in the set, in the order
// picture-then-commentary, with the fused case collapsed to one.
//
// Every loop over a capture set must go through this rather than over the two
// fields: arming a fused pipeline twice would run the READY cycle over the same
// two proxysinks a second time, and claiming it twice would refuse the seat its
// own consumer with ErrSeamBusy.
func (s CaptureSet) Pipelines() []CapturePipeline {
	switch {
	case s.Picture == nil && s.Commentary == nil:
		return nil
	case s.Picture == nil:
		return []CapturePipeline{s.Commentary}
	case s.Commentary == nil:
		return []CapturePipeline{s.Picture}
	case s.Picture == s.Commentary:
		return []CapturePipeline{s.Picture}
	default:
		return []CapturePipeline{s.Picture, s.Commentary}
	}
}

// ---------------------------------------------------------------------------
// The single-consumer claim
// ---------------------------------------------------------------------------

// ErrSeamBusy is returned when a send pipeline is asked to attach to a proxysink
// that already has a consumer.
//
// It is a sentinel because the caller's answer is specific and is not "retry":
// there is a send pipeline still holding the seam, and building a second one
// would not fail — it would SUCCEED, steal the stream, and take the first one
// silently off air. See seam.go for the measurement.
var ErrSeamBusy = errors.New("gst: the capture seam already has a consumer")

// seamClaim is one proxysink's single-consumer flag.
//
// It is an atomic rather than a mutex-guarded bool because the claim is taken on
// the caller's goroutine at START and released on a teardown path that may be
// running while a bus handler is still delivering, and neither may wait for the
// pipeline lock — which is held across state changes for seconds at a time.
type seamClaim struct {
	name string
	held atomic.Bool
}

// claim takes the flag or names the sink that already has it.
func (c *seamClaim) claim() error {
	if c.held.CompareAndSwap(false, true) {
		return nil
	}
	return fmt.Errorf("%w: %s already has a proxysrc attached. A second proxysrc does not fail, "+
		"it silently steals the stream and kills the first — measured, the incumbent stopped dead "+
		"13 ms after the second attached, with nothing on either bus. Stop the session that holds "+
		"it before starting another", ErrSeamBusy, c.name)
}

// release gives the flag back. Idempotent by construction.
func (c *seamClaim) release() { c.held.Store(false) }

// held reports whether the claim is taken, for tests and for a log line.
func (c *seamClaim) taken() bool { return c.held.Load() }

// seamClaims is the set of claims one capture pipeline owns — one per proxysink,
// so a fused pipeline has two and a split one has one.
//
// It claims ALL OR NOTHING. A partial claim would leave the video seam taken by a
// send pipeline that never got built, and the next START would be refused for a
// consumer that does not exist.
type seamClaims []*seamClaim

func (cs seamClaims) claimAll() error {
	for i, c := range cs {
		if err := c.claim(); err != nil {
			for _, done := range cs[:i] {
				done.release()
			}
			return err
		}
	}
	return nil
}

func (cs seamClaims) releaseAll() {
	for _, c := range cs {
		c.release()
	}
}

// takenNames is the claims currently held, so that a refusal can say WHICH half
// of the seam has a consumer rather than that something does.
//
// It is what lets Stop refuse to take a device to NULL underneath a bound
// proxysrc — the measured failure with no EOS, no error on either bus and SRT
// still connected. Without it that ordering rule is enforced only by four call
// sites remembering it.
func (cs seamClaims) takenNames() []string {
	var out []string
	for _, c := range cs {
		if c.taken() {
			out = append(out, c.name)
		}
	}
	return out
}

func (cs seamClaims) names() []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.name)
	}
	return out
}

// newSeamClaims builds the claim set a leg-set implies. The ORDER is
// video-then-audio and is the order everything else in this package reports the
// seam in, so that a log line and a failure name the same half.
func newSeamClaims(legs CaptureLegs) seamClaims {
	var cs seamClaims
	if legs.Picture != PictureNone {
		cs = append(cs, &seamClaim{name: nameVideoProxySink})
	}
	if legs.Commentary != CommentaryNone {
		cs = append(cs, &seamClaim{name: nameAudioProxySink})
	}
	return cs
}
