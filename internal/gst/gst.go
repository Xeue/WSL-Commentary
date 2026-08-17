// Package gst is the application's entire GStreamer surface, and the only
// package in the module that touches cgo.
//
// Owner: WP-3a. No other work package writes files in this directory.
//
// It is deliberately thin. It builds a pipeline, lists capture devices and swaps
// a sink. Every decision — when to reconnect, how long to wait, what the lamps
// say — lives in internal/sender, which is pure Go and never imports cgo.
//
// # Two implementations
//
// The interfaces and option types below carry no build tag. Two files implement
// them and are selected by whether cgo is enabled:
//
//   - gst_cgo.go  (//go:build cgo)  — the real go-gst pipeline. Requires MinGW
//     gcc and the GStreamer development installer (Gate B).
//   - gst_stub.go (//go:build !cgo) — a pure-Go fake that returns plausible
//     device names and a Pipeline whose transitions can be driven
//     programmatically. It requires no toolchain at all (Gate A) and is what
//     lets the rest of the application be built and demonstrated today.
//
// Each file provides Init, ListInputDevices and New. Everything above this seam
// is identical in both builds.
//
// # And, inside the real build, two platforms
//
// gst_cgo.go is itself platform-neutral. What differs between Windows and macOS
// is confined to two pairs of files, on this codebase's usual per-file
// build-tag idiom:
//
//   - elements_windows.go / elements_darwin.go — the ELEMENT contract: the
//     capture source and AAC encoder factory names, the platform half of the
//     bundle's required-element list, and the H.264 encoder preference and
//     settings.
//   - deviceprovider_windows.go / deviceprovider_darwin.go — the DEVICE seam:
//     which enumerated devices this build can offer, what string is persisted
//     for each, and how that string is turned back into whatever the capture
//     element will actually accept.
//
// There is deliberately no runtime.GOOS anywhere in this package.
//
// # The pipeline this package builds
//
// Specification section 5, as one gst_parse_launch string. EXACTLY TWO element
// names vary by platform; nothing else in the graph does.
//
//	mpegtsmux name=mux alignment=7 pcr-interval=3600
//	  ! queue name=srtq leaky=downstream max-size-buffers=4000
//	  ! srtsink name=srtout sync=false async=false auto-reconnect=false
//	filesrc location=<slate> ! pngdec ! videoconvert ! videoscale name=vscale
//	  ! video/x-raw,format=NV12,width=1920,height=1080,...   <- PipelineOpts.ConformTo
//	  ! imagefreeze is-live=true
//	  ! video/x-raw,framerate=50/1                           <- PipelineOpts.ConformTo
//	  ! mfh264enc name=venc bitrate=2000 rc-mode=cbr gop-size=100
//	    low-latency=true cabac=true
//	  ! h264parse config-interval=-1 ! queue ! mux.
//
// The two capsfilters in the video leg are the only part of the string that is
// not a constant: they are rendered from PipelineOpts.ConformTo, whose zero
// value reproduces the 1920x1080p50 that used to be written there. The
// argument for that one conditional — and why it does not move the
// Windows/macOS seam — is in pipelineDescription's own comment.
//
// # The video leg has a second form, and only a second form
//
// When PipelineOpts.VideoCaptureID names a DeckLink card, the four lines above
// between mpegtsmux and the encoder are replaced by a live capture conformed to
// the same target:
//
//	decklinkvideosrc name=vcapsrc mode=auto drop-no-signal-frames=false
//	  ! videoconvert name=vcapconv ! deinterlace name=vcapdeint
//	  ! videoscale name=vcapscale ! videorate name=vcaprate
//	  ! video/x-raw,format=NV12,...                          <- ConformTo
//	  ! tee name=vcaptee allow-not-linked=true
//	vcaptee. ! queue name=vcapq max-size-time=1000000000
//	  ! mfh264enc name=venc ...                              <- unchanged
//
// NOTHING FROM THE ENCODER DOWN CHANGES, on either platform, in either form.
// That is the property the whole shape is built around: h264parse, both
// queues, mpegtsmux, the leaky srtq, ReplaceSink and internal/sender see one
// fixed format forever, and the conform chain is what guarantees it whatever
// the camera is doing. See PipelineOpts.VideoCaptureID and pipelineDescription.
//
// # The audio leg has a second form too, and it drags a video element in with it
//
// When PipelineOpts.AudioCaptureID names a card, the platform capture source is
// replaced by the card's embedded audio and NOTHING BELOW THE MIX MATRIX
// CHANGES:
//
//	decklinkaudiosrc name=asrc channels=16
//	  ! audioconvert ! level name=chlevel ...        <- unchanged
//	  ! audioconvert name=aconv                      <- the mix matrix, unchanged
//	  ! audioresample ! audio/x-raw,...,channels=2   <- unchanged
//	  ! level name=alevel ! <aac> ! aacparse ! queue ! mux.
//
// MEASURED: decklinkaudiosrc cannot preroll without a decklinkvideosrc in the
// SAME pipeline, because the card drives audio capture off the video clock. So
// when the video leg is the SLATE, one more chain appears and clocks it:
//
//	decklinkvideosrc name=vcapclock mode=auto drop-no-signal-frames=false
//	  ! fakesink name=vcapclocksink sync=false async=false
//
// and when the video leg is the SAME CARD, that chain is absent because vcapsrc
// already is it. The card is exclusive; there is never more than one
// decklinkvideosrc. See PipelineOpts.AudioCaptureID and pipelineDescription.
//
//	wasapi2src name=asrc device=<endpoint id> low-latency=true
//	  ! audioconvert ! audioresample ! level name=alevel interval=50000000
//	  ! mfaacenc bitrate=128000
//	  ! aacparse ! queue ! mux.
//
// On macOS the same graph reads:
//
//	  ! vtenc_h264 name=venc bitrate=2000 rate-control=cbr
//	    max-keyframe-interval=100 realtime=true allow-frame-reordering=false
//	  ! h264parse config-interval=-1 ! queue ! mux.
//	osxaudiosrc name=asrc device=<AudioDeviceID resolved at Start>
//	  ! audioconvert ! audioresample ! level name=alevel interval=50000000
//	  ! atenc bitrate=128000
//	  ! aacparse ! queue ! mux.
//
// srtsink's own auto-reconnect is set to false and must stay false: on a write
// failure it reopens immediately with no backoff, retries once, and then errors
// the whole pipeline out — straight into M2L-X's roughly five second re-accept
// refusal window.
package gst

import (
	"errors"
	"fmt"
	"math"
	"strconv"
)

// ErrPipelineFatal is wrapped by every error reporting a failure that
// replacing the sink cannot repair — the capture or mux chain, not the SRT
// link. It exists so that internal/sender can stop treating such a failure as
// a connection problem: before this sentinel, a wasapi2 device that failed to
// open asynchronously (the operator selecting a playback endpoint as the
// commentary input) was retried as though the network were down, telling the
// operator "the feed is not connected and is retrying" forever about a fault
// no reconnect could ever fix.
//
// The condition LATCHES for the life of the pipeline: once one error wrapping
// this sentinel has been returned or delivered, every subsequent ReplaceSink
// on the same Pipeline returns it. Recovery is Stop, New, Start — nothing
// less rebuilds the failed chain.
//
// The message text deliberately keeps the "pipeline-fatal" substring that the
// pre-sentinel error carried, so anything still matching on the string —
// field-log greps, BUILD-NOTES.md instructions — keeps matching. New code
// must use errors.Is(err, ErrPipelineFatal) instead.
var ErrPipelineFatal = errors.New("gst: pipeline-fatal")

// Encoder settings fixed by specification section 5. The units differ between
// the two encoders and the names here say which is which.
const (
	// DefaultVideoBitrateKbps is the H.264 encoder's bitrate property, in
	// KILOBITS per second, for a caller that asks for nothing. Constant
	// bitrate, not quality-targeted: a static slate under QVBR collapses to
	// 200-350 kbps, which is cheaper but bursty at every IDR and makes "is it
	// flowing" harder to observe on a router that shows a bitrate and nothing
	// else.
	//
	// 2000 was chosen for the video leg this application actually has — ONE
	// STILL SLATE, re-encoded fifty times a second — and the owner has ruled it
	// FAR TOO LOW for a leg carrying live video, wanting something nearer
	// 10000 available. That ruling is why PipelineOpts.VideoBitrateKbps exists
	// and why this number is a DEFAULT rather than the value: leaving it at
	// 2000 means a build nobody has configured encodes exactly what the on-air
	// Windows build encodes today, and a facility that puts real motion into
	// the video leg sets the option instead of editing this file. Nothing in
	// this package imposes a ceiling — 10000 is an ordinary number to both
	// encoders, whose property is a guint of kilobits.
	DefaultVideoBitrateKbps = 2000

	// DefaultAudioBitrateBps is mfaacenc's bitrate property, in BITS per second.
	DefaultAudioBitrateBps = 128000

	// DefaultSRTLatencyMs is srtsink's latency property, in MILLISECONDS. The
	// property is milliseconds despite what the original brief's example URI
	// suggested.
	DefaultSRTLatencyMs = 120
)

// Device is one audio capture endpoint offered in the commentary input
// dropdown.
type Device struct {
	// ID is the operating system's own STABLE identity for the endpoint. It is
	// what is persisted to config.json, and it survives a rename — which is why
	// it, rather than Name, is the thing stored. It sidesteps the double space
	// in "DVS Receive  1-2 (Dante Virtual Soundcard)" entirely.
	//
	// The shape and the meaning are per-platform, and the difference is bigger
	// than it looks:
	//
	//	Windows  a WASAPI IMMDevice endpoint ID GUID, e.g.
	//	         "{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}".
	//	         It is passed to wasapi2src's device property UNCHANGED.
	//	macOS    a CoreAudio unique-id, e.g. "BuiltInMicrophoneDevice" or
	//	         "BF568F24-731B-41DB-932E-AC7E260BC71A". It is NOT what
	//	         osxaudiosrc accepts — that property is a gint AudioDeviceID,
	//	         a runtime handle that coreaudiod reassigns on every
	//	         enumeration — so it is resolved to the current integer at
	//	         pipeline-open time. See deviceprovider_darwin.go; storing the
	//	         integer instead is a silent wrong-device failure after the
	//	         operator's next reboot.
	//
	// What is true on both, and is the contract callers may rely on: this value
	// is stable across restarts, it is the only thing that should ever be
	// persisted, and it is meaningless to the browser (whose mediaDeviceId is a
	// per-origin salted hash and identifies nothing at the operating-system
	// level).
	ID string `json:"id"`

	// Name is the endpoint's display-name, shown in the dropdown and never
	// persisted or passed to GStreamer.
	Name string `json:"name"`

	// Kind says which FAMILY of capture device this is, and therefore which
	// vocabulary ID is spoken in — the platform's own audio stack, or a
	// Blackmagic card enumerated by GStreamer's decklink provider. The two id
	// spaces are different kinds of identifier in exactly the sense CONTRACT.md
	// uses of headphoneDeviceId and headphoneEndpointId, and neither element
	// reports a stranger's id as a stranger's: osxaudiosrc handed a DeckLink
	// persistent-id falls back to the system default input, which is the silent
	// wrong-device failure this package spends most of its length preventing.
	//
	// It is a SEPARATE FIELD and never a prefix inside ID, because ID is
	// documented as an opaque string that nothing above internal/gst may parse.
	// It is NOT PERSISTED: config.json keeps the id alone, so an existing file
	// stays valid and no field is added to internal/presets. The empty string
	// therefore means "written before this field existed" and reads as native;
	// use NormaliseDeviceKind rather than comparing against "".
	//
	// The DeckLink half of the argument — what a persistent-id is, why it is
	// never a device-number, and how the two providers are told apart — is in
	// decklinkdevices.go, which WP-DECKLINK owns. The type itself is directly
	// below, beside the field that carries it.
	//
	// # The json tag has NO omitempty, and that is now load-bearing
	//
	// It had one, and it was wrong in a way that only shows up on the frontend.
	// The commentary input is ONE dropdown over BOTH kinds, split into optgroups
	// keyed on this field, so the frontend asks each device which group it
	// belongs in. With omitempty a Kind of "" is omitted from the frame
	// ENTIRELY, the frontend's `kind === 'native'` reads undefined, and the
	// device lands in NEITHER group — it vanishes from the only control that
	// could select it. That is the original enumeration bug reproduced one layer
	// up, and the whole reason this field exists is to stop the operator's
	// microphone disappearing out of a list.
	//
	// The Go-side guarantee that pairs with the tag: ListInputDevices passes
	// every entry through NormaliseDeviceKind before returning, so the wire
	// carries an explicit "native" or "decklink" and never an empty string. The
	// tag alone would serialise "", which is honest but is a third spelling for
	// the frontend to know about; normalising at the source means it has two.
	// NormaliseDeviceKind stays the rule for anything DECODED — a Device read
	// back out of a frame written by an older build still carries "".
	Kind DeviceKind `json:"kind"`

	// Channels is how many input channels this device ADVERTISES: structure 0 of
	// the enumerated GstDevice's caps, read during the ordinary provider walk.
	// Zero means it advertised nothing fixed.
	//
	// # It is a QUERY. Nothing is opened to answer it
	//
	// Measured on this machine, with no device opened: the built-in microphone
	// advertises channels=1, NDI Audio channels=2 channel-mask=0x3, and the
	// UltraStudio through CoreAudio channels=16 channel-mask=0x0. The DeckLink
	// provider's own entry for the same card advertises `{ 2, 8, 16 }`, which
	// fixes nothing and reads here as 0 — correctly, because a card commentary's
	// width is the constant 16 BY CONSTRUCTION (the description states
	// channels=16 on the element) rather than discovered.
	//
	// # What it is for, and why a seat cannot start without it
	//
	// It is what fills CaptureOpts.DeviceChannels, which is what SIZES THE
	// MIX-MATRIX while the capture pipeline is still in NULL. A matrix is a
	// negotiation constraint and not a gain: no CoreAudio source can emit a
	// positioned channel-mask above two channels — gstosxcoreaudio.c:886-889 sets
	// `layout = NULL; /* no supported for sources */` unconditionally for every
	// source — so audioconvert cannot map a 16-in Focusrite or RME to stereo
	// without one, and the leg dies about 0.07 s after PLAYING with
	// not-negotiated (-4). The source pad cannot be asked instead: osxaudiosrc's
	// src template is `channels: [1, 2147483647]`, which fixes nothing in NULL.
	//
	// So this field is not a decoration on the dropdown. It is the only thing in
	// the process that can tell a multichannel commentary seat how wide it is
	// before it has to commit.
	//
	// # There is deliberately no Positioned field beside it
	//
	// The plan proposed one. Nothing would read it: the matrix is written
	// UNIFORMLY at every width including 1 and 2 (measured working at 1, 2, 3, 16
	// and 32), so there is no decision anywhere in this package that positioning
	// changes, and a field nothing consumes is a fact with nobody to keep it
	// true. The one thing it would have encoded is already settled from
	// GStreamer's own source and written down in capture.go. Reading it also is
	// not free — channel-mask is a GST_TYPE_BITMASK with no typed getter in
	// go-gst, so it can only be recovered by scanning the serialised structure.
	Channels int `json:"channels"`
}

// DeviceKind names which family of capture device a Device belongs to, and
// therefore which element can open it and what its ID means.
//
// It is a string rather than an integer so that it survives the Wails JSON
// boundary as something a human can read in a log or a devtools inspector. An
// integer enum would cross the wire as 0 and 1, and "kind: 0" in a bug report
// is a value somebody has to go and look up at the moment they can least afford
// to.
//
// It is a NAMED TYPE and not a bare string so that the compiler refuses any
// other string assigned to Device.Kind. That matters more here than it looks:
// the failure mode of a wrong kind is not an error but a SILENT fall-through to
// the unlabelled native default, which is the exact indistinguishable-twin
// defect the field was added to close.
//
// It lives here, next to Device, rather than in decklinkdevices.go where the
// DeckLink rules live, for two reasons. KindNative is not a DeckLink concept —
// it is the absence of one, and every device this application enumerated before
// a card was ever fitted is one. And frontend/src/ui/devices.test.js pins these
// two spellings by READING THIS FILE'S SOURCE TEXT, the same way the WASAPI
// endpoint prefixes are pinned against device_id.go; a contract string whose
// mirror has to consult two files to check one field is a mirror that will
// eventually check the wrong one.
type DeviceKind string

const (
	// KindNative is a device belonging to the platform's own audio stack:
	// a WASAPI endpoint on Windows, a CoreAudio device on macOS. Its ID is that
	// stack's stable identity — see Device.ID — and captureSourceFactory opens
	// it.
	//
	// "native" rather than "coreaudio"/"wasapi" because the value crosses to the
	// frontend, which is one build for both platforms and must not have to know
	// which one it is running on to recognise the ordinary case.
	KindNative DeviceKind = "native"

	// KindDeckLink is a Blackmagic capture card enumerated by GStreamer's
	// decklink device provider. Its ID is the card's persistent-id, and only a
	// decklink element can open it.
	KindDeckLink DeviceKind = "decklink"
)

// The conform target: the raster and rate the contribution pipeline's VIDEO
// LEG produces, and the reason it is an option rather than four numbers in a
// string literal.
//
// # Why this type exists at all
//
// gst_cgo.go's pipelineDescription used to hard-code
// width=1920,height=1080,framerate=50/1 in the slate leg's capsfilter, and
// frontend/src/ui/lamps.js hard-codes GOOD_VIDEO={h264,1920,1080,50} beside
// it. The owner has confirmed that M2L-X can be configured into ANY format and
// that every source feeding it must match whatever it is configured for, so
// both constants are a live defect on any instance not configured for 1080p50
// — a feed that locks, reports a healthy bitrate, and is the wrong raster.
//
// # Why a fraction and not a float
//
// GStreamer's framerate caps field takes a GstFraction and nothing else, and
// the rates that matter are not all decimals: 29.97 is 30000/1001 exactly and
// 29.97 only approximately. The switcher reports the decimal — frame_rate
// arrives as the STRING "50", see internal/m2lx/format.go — so the conversion
// has to happen somewhere, and it happens once, in ConformTargetFromRate, with
// the 1000/1001 family recognised explicitly rather than left to an
// approximation that would produce 2997/100 and look entirely plausible. A
// pipeline paced at 2997/100 against a source at 30000/1001 gains a frame
// every 1001 frames, which at 30p is a frame every 33 seconds — well inside a
// match, and invisible in a twenty-second test.
//
// # What is given up: scan type
//
// There is no interlaced/progressive field, and its absence is a decision
// rather than an omission. This leg begins at pngdec, which emits progressive
// frames; there is no interlacer between there and the encoder; and asking for
// one in the caps does not conjure one. Pinning interlace-mode=interleaved
// downstream of videoconvert/videoscale fails the whole leg at Start with
// "Internal data stream error / not-negotiated (-4)" — measured, GStreamer
// 1.26.10 on macOS arm64, 2026-08-15 — because videoconvert passes
// interlace-mode through rather than converting it, and the `interlace`
// element that would do the job is in NEITHER bundler's allowlist today.
//
// So there is no interlaced target this pipeline could honour if it were told
// about one, and a field that can only ever be set wrongly is all cost. A
// switcher configured for 1080i50 gets 1920x1080p50 out of us and the frame
// RATE is what is matched; spatialCaps states interlace-mode=progressive
// rather than leaving it to default, so that is visible in the parse string
// and in the log rather than inferred. If a facility ever runs an interlaced
// instance, the fix is to add `interlace` to build/bundle-gst.ps1 and
// build/bundle-gst-darwin.sh and to put it in this leg — not to widen the
// capsfilter and hope.
//
// Colorimetry, pixel aspect ratio and pixel format are absent for a duller
// reason: they are fixed by this pipeline (bt709, 1/1, NV12) and are not
// things the switcher varies independently of the raster in any configuration
// anyone has measured.

// The compiled-in fallback: the format every instance measured so far has been
// configured for, and the one this application transmitted for its whole life
// before the conform target existed.
//
// It is a FALLBACK and not a default in the ordinary sense. It is what Start
// uses when the caller knows nothing — no node running, no control plane, an
// instance never signed in to — and every path that reaches it says so in the
// log, because transmitting 1080p50 into a 720p50 switcher is the exact
// failure this type exists to make visible.
const (
	FallbackConformWidth        = 1920
	FallbackConformHeight       = 1080
	FallbackConformFrameRateNum = 50
	FallbackConformFrameRateDen = 1
)

// ConformTarget is the raster and rate the video leg is conformed to.
//
// The FIELD NAMES are deliberately the ones config.VideoFormatSpec uses, so
// that the bridge from a parsed videoFormatOverride to a pipeline option is a
// transcription with nothing to get subtly wrong. The other source — the
// format read off the switcher — arrives as a decimal rate and comes through
// ConformTargetFromRate.
//
// The ZERO VALUE IS NOT A USABLE TARGET and Valid reports so. Callers must not
// read a zero Width as "the default", which is the mistake PipelineOpts' bitrate
// fields deliberately make safe and this type deliberately does not: a zero
// here means nobody has decided, and exactly one place in the application is
// entitled to turn that into 1080p50 and log that it did. That place is Start.
type ConformTarget struct {
	// Width and Height are the raster in pixels.
	Width  int
	Height int

	// FrameRateNum and FrameRateDen are the frame rate as the exact fraction
	// GStreamer wants: 50/1, 25/1, 30000/1001. Den is never zero on a target
	// that came out of ConformTargetFromRate.
	FrameRateNum int
	FrameRateDen int
}

// FallbackConformTarget is the target used when the caller knows nothing. See
// the constants above for why it is a fallback and not a default.
func FallbackConformTarget() ConformTarget {
	return ConformTarget{
		Width:        FallbackConformWidth,
		Height:       FallbackConformHeight,
		FrameRateNum: FallbackConformFrameRateNum,
		FrameRateDen: FallbackConformFrameRateDen,
	}
}

// Valid reports whether this target can be rendered into caps GStreamer will
// accept. A zero value is not valid, and neither is anything with a
// non-positive field: framerate=0/1 negotiates against a live source as "any
// rate", which would silently defeat the whole point of conforming.
func (t ConformTarget) Valid() bool {
	return t.Width > 0 && t.Height > 0 && t.FrameRateNum > 0 && t.FrameRateDen > 0
}

// String renders the target the way an operator reads a format — 1920x1080p50,
// 1280x720p29.97 — because it is what goes in the log line at Start and in the
// message when a feed will not lock. It is the operator-facing spelling, not
// the caps spelling.
//
// The "p" is unconditional; the block comment above says why.
//
// The rate is rendered with the FEWEST decimal places that still identify it,
// which is the whole reason this is not one FormatFloat call. 30000/1001 is
// 29.970029970... and an operator comparing our log against a switcher that
// says "29.97" should not have to work out whether 29.97003 is the same
// number. Nor should 50 acquire a decimal point. See conformRateText.
func (t ConformTarget) String() string {
	if !t.Valid() {
		return "(no conform target)"
	}
	return strconv.Itoa(t.Width) + "x" + strconv.Itoa(t.Height) + "p" + t.rateText()
}

// rateText renders the frame rate the way it is written down: "50", "29.97",
// "23.976", "12.5".
//
// It tries 0, 1, 2 and 3 decimal places in turn and takes the first whose
// value is within conformRateTextTolerance of the exact fraction. That
// produces the conventional spelling of every rate anyone writes — the
// 1000/1001 family included, which is the case a fixed number of places cannot
// serve, since 29.97 needs two and 23.976 needs three.
//
// The tolerance is 1e-4 and NOT ntscTolerance, and the difference matters:
// ntscTolerance (0.005) is how far a rate READ off the switcher may sit from
// an exact value and still be recognised as it, whereas this is how much error
// a rendering may hide. At 0.005 this function would print 24000/1001 as
// "23.98", which is inside the tolerance and is not what anybody calls that
// rate. Four decimal places of agreement is enough to identify a rate and not
// enough to disguise one.
func (t ConformTarget) rateText() string {
	const conformRateTextTolerance = 1e-4
	exact := float64(t.FrameRateNum) / float64(t.FrameRateDen)
	for places := 0; places < 3; places++ {
		text := strconv.FormatFloat(exact, 'f', places, 64)
		if shown, err := strconv.ParseFloat(text, 64); err == nil &&
			math.Abs(shown-exact) < conformRateTextTolerance {
			return text
		}
	}
	return strconv.FormatFloat(exact, 'f', 3, 64)
}

// DisplayFrameRate is the frame rate as the NUMBER an operator would write
// down: 50, 29.97, 23.976. It is rateText parsed back, and it is a separate
// method rather than float64(Num)/float64(Den) for one specific reason.
//
// The exact fraction 30000/1001 is 29.970029970029970..., and everything this
// value is ever compared against is the ROUNDED spelling. M2L-X's
// switcher_status reports frame_rate as the string "29.97"; internal/m2lx
// parses that to the float64 29.97; frontend/src/ui/lamps.js then compares the
// two with ===, which is a comparison the exact fraction loses. A VIDEO lamp
// sitting red on a perfectly conforming 1080p29.97 feed, with "1080P29.97" in
// green text beside "1080P29.970029970029970" in the log, is a worse failure
// than the hardcoded constant this whole mechanism replaced, because it looks
// like a rounding argument rather than a bug.
//
// So the rate that crosses the Wails boundary is the same rate String() prints,
// arrived at by the same function. The pipeline is still built from the EXACT
// fraction — FrameRateNum/FrameRateDen, which is what temporalCaps renders and
// what GStreamer requires — and this is only ever the spelling. Nothing may
// build caps from it.
func (t ConformTarget) DisplayFrameRate() float64 {
	if !t.Valid() {
		return 0
	}
	shown, err := strconv.ParseFloat(t.rateText(), 64)
	if err != nil {
		// Unreachable: rateText's every return is FormatFloat's output. Falling
		// back to the exact quotient rather than zero keeps a caller that has
		// somehow got here comparing against a real rate instead of against a
		// value no format has.
		return float64(t.FrameRateNum) / float64(t.FrameRateDen)
	}
	return shown
}

// ntscDen is the denominator of the 1000/1001 family: the rates that are
// exactly n*1000/1001 and are conventionally written as a truncated decimal —
// 23.976, 29.97, 59.94, 119.88. They are recognised by construction rather
// than tabulated, so a member nobody has met yet is handled too.
const ntscDen = 1001

// ntscTolerance is how far a reported rate may sit from an exact 1000/1001
// value and still be read as that value.
//
// 0.005 is chosen against the two things it has to separate. The reported
// "29.97" differs from 30000/1001 by 3.0e-5, so every truncation anyone writes
// down is comfortably inside it. The nearest INTEGER rate, 30, differs from
// 30000/1001 by 0.03 — six times the tolerance — so an instance genuinely
// configured for 30/1 can never be read as 30000/1001. Anything between the
// two is neither, and falls through to the exact-decimal case, which is the
// honest answer for a rate nobody recognises.
const ntscTolerance = 0.005

// maxConformFPS is a sanity bound, not a policy. 1000 fps is not a
// contribution format; a value above it means the numerator and denominator
// have been swapped, or the field that was read was never a frame rate.
const maxConformFPS = 1000

// maxConformDimension is the same idea in the space axis, and generous on
// purpose: 8192 covers every UHD and DCI raster anyone would put through a
// switcher and refuses the result of having read a field that was never a
// pixel count. It exists so that a garbled value falls back with a log line
// rather than reaching gst_parse_launch, where an absurd raster is either caps
// that fail to negotiate or — worse — caps that negotiate and allocate.
const maxConformDimension = 8192

// ConformTargetFromRate builds a target from a raster and a DECIMAL frame
// rate, which is the only form the switcher reports.
//
// The rate is resolved in three steps, most specific first:
//
//  1. a whole number becomes n/1: 50, 25, 60.
//  2. a rate within ntscTolerance of n*1000/1001 becomes n*1000/1001 EXACTLY:
//     29.97, 59.94, 23.976, 47.952, 119.88.
//  3. anything else becomes the exact decimal reduced to lowest terms, to three
//     decimal places: 30.5 becomes 61/2.
//
// Step 3 exists so that a rate nobody has met is carried faithfully rather
// than rounded into one of the rates that were anticipated. Three decimal
// places is the limit of what the switcher has ever been observed to report,
// and it keeps the numerator inside an int on every platform.
//
// An error names the offending value and is never "use the fallback instead":
// the caller decides that, and has to say in its log which fallback it took.
func ConformTargetFromRate(width, height int, fps float64) (ConformTarget, error) {
	if width <= 0 || height <= 0 {
		return ConformTarget{}, fmt.Errorf("gst: conform raster %dx%d is not a raster", width, height)
	}
	if math.IsNaN(fps) || math.IsInf(fps, 0) || fps <= 0 || fps > maxConformFPS {
		return ConformTarget{}, fmt.Errorf("gst: conform frame rate %v is not a frame rate", fps)
	}

	t := ConformTarget{Width: width, Height: height}

	if fps == math.Trunc(fps) {
		t.FrameRateNum, t.FrameRateDen = int(fps), 1
		return t, nil
	}

	// The 1000/1001 family. n is the rate this WOULD be if it were exact — 30
	// for 29.97 — and the test is whether n*1000/1001 lands back on what
	// arrived.
	if n := math.Round(fps * ntscDen / 1000); n >= 1 {
		if math.Abs(fps-n*1000/ntscDen) < ntscTolerance {
			t.FrameRateNum, t.FrameRateDen = int(n)*1000, ntscDen
			return t, nil
		}
	}

	num, den := int(math.Round(fps*1000)), 1000
	g := gcdConform(num, den)
	t.FrameRateNum, t.FrameRateDen = num/g, den/g
	return t, nil
}

// gcdConform is Euclid's, guarding the zero that would make the caller divide
// by it. The stdlib has no integer gcd, and reaching for math/big to reduce a
// fraction whose denominator is 1000 would be the more surprising choice.
func gcdConform(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}

// resolve returns the conform target the pipeline will actually be built with,
// and a non-empty reason when that is not the target the caller asked for.
//
// The two outcomes are deliberately different things and Start logs them
// differently. The ZERO ConformTarget is the ordinary "nothing known yet"
// state — the switcher has not been read, or every node is stopped and
// reporting format: null, which is the measured shape on all 22 stopped nodes
// in the 2026-07-31 capture — and it resolves to FallbackConformTarget() with
// NO reason. It is not a fault and must not read as one in a field log, or the
// one line that does mean something is lost among the ones that do not.
// Anything else that cannot be used resolves to the fallback WITH a reason
// naming the value, because a switcher that reported 1920x1081 is a fact
// somebody needs.
//
// It never returns an unusable target and it never returns an error. Both are
// the same decision, and it is the one FallbackConformTarget records: refusing
// to start without a known switcher format would be the tidier rule and it
// would be wrong here, for the same reason statusKey is deliberately not in
// config.Validate. Nothing may be a reason a match does not go out. The format
// is only knowable by reading the switcher, the switcher reports nothing for a
// node that is not streaming, and the commentary position is routinely started
// before anything is running at the other end.
func (t ConformTarget) resolve() (ConformTarget, string) {
	if t == (ConformTarget{}) {
		return FallbackConformTarget(), ""
	}
	if !t.Valid() {
		return FallbackConformTarget(), fmt.Sprintf(
			"%dx%d at %d/%d is not a raster and a rate",
			t.Width, t.Height, t.FrameRateNum, t.FrameRateDen)
	}

	// EVEN DIMENSIONS ARE AN NV12 RULE, NOT A SWITCHER RULE, which is why the
	// check is here and not in Valid — a target this pipeline cannot encode is
	// still a perfectly good description of what the switcher is doing. This
	// leg encodes NV12, whose chroma plane is half resolution on both axes, so
	// an odd dimension is not a rounding question: it is a caps negotiation
	// that fails at Start with "not-negotiated (-4)" and names neither the
	// element nor the number that caused it. It falls back rather than
	// rounding, because a slate one pixel narrower than the switcher expects is
	// a silent format mismatch of exactly the kind this type exists to end.
	if t.Width%2 != 0 || t.Height%2 != 0 {
		return FallbackConformTarget(), fmt.Sprintf(
			"raster %dx%d is odd on one axis and NV12 chroma is half resolution on both",
			t.Width, t.Height)
	}
	if t.Width > maxConformDimension || t.Height > maxConformDimension {
		return FallbackConformTarget(), fmt.Sprintf(
			"raster %dx%d exceeds the %d-pixel sanity limit",
			t.Width, t.Height, maxConformDimension)
	}
	if t.FrameRateNum > maxConformFPS*t.FrameRateDen {
		return FallbackConformTarget(), fmt.Sprintf(
			"frame rate %d/%d exceeds the %d fps sanity limit",
			t.FrameRateNum, t.FrameRateDen, maxConformFPS)
	}
	return t, ""
}

// spatialCaps renders the capsfilter pinned UPSTREAM of imagefreeze:
// everything about ONE PICTURE, and nothing about how often it is repeated.
//
// The split between this and temporalCaps is not a property of a conform
// target at all — it is a property of the order the slate leg's elements are
// in, which is what lets videoconvert and videoscale sit above imagefreeze
// instead of below it. pipelineDescription has the argument and the
// measurement.
//
// interlace-mode=progressive is STATED rather than left to default. It is the
// one thing this application asserts about scan, it is what the leg can
// actually produce, and pinning it means that a future GStreamer negotiating
// differently is a loud caps failure at Start rather than a feed the switcher
// quietly will not take. The block comment above has the measurement.
func (t ConformTarget) spatialCaps() string {
	return "video/x-raw,format=NV12" +
		",width=" + strconv.Itoa(t.Width) +
		",height=" + strconv.Itoa(t.Height) +
		",pixel-aspect-ratio=1/1,colorimetry=bt709,interlace-mode=progressive"
}

// temporalCaps renders the capsfilter pinned DOWNSTREAM of imagefreeze: the one
// field imagefreeze itself decides, which is the rate at which it repeats the
// single frame it was handed.
func (t ConformTarget) temporalCaps() string {
	return "video/x-raw," + t.rateCapsField()
}

// captureCaps renders the SINGLE capsfilter the LIVE CAPTURE video leg is
// conformed to: everything spatialCaps pins about one picture plus the frame
// rate, in one filter at the bottom of the conform chain.
//
// The two legs pin the same fields in different PLACES, and the difference is a
// property of the elements above them rather than of the target. The slate leg
// has to split because imagefreeze sits between the two halves and is the thing
// that decides the rate — see pipelineDescription. The capture leg has nothing
// between videorate and the tee, and one filter is both correct and the only
// shape that works: videorate needs the rate on its SRC pad to know what to
// retime to, and videoscale needs the raster below it for the same reason, so
// splitting them would leave one of the two elements with nothing to negotiate
// against.
//
// It is spatialCaps plus the rate rather than a second full string, so that the
// two legs cannot drift on format, pixel aspect ratio, colorimetry or scan. A
// capture leg that sent bt601 where the slate sent bt709 would be a colorimetry
// change the switcher sees every time the operator reconfigured the source, and
// it would be invisible in this file.
func (t ConformTarget) captureCaps() string {
	return t.spatialCaps() + "," + t.rateCapsField()
}

// rateCapsField is the framerate field alone, as GStreamer's exact fraction. It
// exists so that the two legs' capsfilters render the rate through one function
// rather than two, which is the only way a change to how a rate is written can
// reach both.
func (t ConformTarget) rateCapsField() string {
	return "framerate=" +
		strconv.Itoa(t.FrameRateNum) + "/" + strconv.Itoa(t.FrameRateDen)
}

// PipelineOpts configures everything upstream of the sink: the slate branch, the
// capture branch, the encoders and the muxer. These cannot be changed without
// rebuilding the pipeline, which is why the SRT endpoint is not among them.
type PipelineOpts struct {
	// SlatePath is the PNG fed to filesrc ! pngdec ! imagefreeze. Required.
	//
	// It stays REQUIRED even when VideoCaptureID names a card and the slate leg
	// is therefore not built, and that is deliberate rather than an oversight.
	// The slate is what the video leg reverts to the moment a seat is configured
	// without a card — which is every seat shipping today — so a caller that
	// stopped supplying it would go on working on the one machine it was tested
	// on and fail on all the others. Validating it unconditionally is what makes
	// that a Start error on the developer's machine instead of at a commentary
	// position.
	SlatePath string

	// AudioDeviceID is the PLATFORM capture endpoint to open — the WASAPI or
	// CoreAudio device that wasapi2src / osxaudiosrc is pointed at. It is
	// Device.ID, never Device.Name, and the whole of what that means —
	// including the fact that macOS has to resolve it before the element will
	// take it — is documented on Device.ID.
	//
	// IT IS REQUIRED WHENEVER AudioCaptureID IS EMPTY, and forbidden when it is
	// not. The two fields are the two answers to one question and exactly one of
	// them is given; refuseWrongAudioSource is the whole rule and both twins
	// call it before anything is built.
	//
	// The emptiness is the dangerous half and is why the rule is a refusal
	// rather than a convention. An empty device on wasapi2src or osxaudiosrc is
	// NOT an error: it is THE SYSTEM DEFAULT INPUT. So a DeckLink seat that
	// reached the platform element with this field empty would put the match on
	// air off the laptop's built-in microphone with every lamp green — which is
	// why the native element is built only when this field is non-empty, and
	// why "the platform source exists and has no device" is unreachable by
	// construction rather than merely unlikely.
	AudioDeviceID string

	// AudioCaptureID, when non-empty, turns the COMMENTARY CAPTURE into a
	// DeckLink card's embedded audio: decklinkaudiosrc opens the card it names,
	// presents its sixteen unpositioned channels, and the mix matrix on the
	// named audioconvert routes the commentator's pair into the feed. It is a
	// persistent-id, the same string VideoCaptureID takes, because one
	// persistent-id names the CARD and serves its audio and video entries alike
	// (measured: the fitted UltraStudio 4K Mini publishes 2747401380 for both).
	//
	// THE ZERO VALUE IS THE PLATFORM MICROPHONE, and that is the whole
	// compatibility statement. A seat that leaves it empty builds byte-for-byte
	// the audio leg the on-air Windows build has always run — captureSourceFactory
	// pointed at AudioDeviceID — because the source is chosen by whether this
	// field is empty and by nothing else.
	//
	// # It brings a decklinkvideosrc with it, and that is not an implementation detail
	//
	// MEASURED: decklinkaudiosrc CANNOT PREROLL ALONE. The card drives audio
	// capture off the VIDEO clock, so with no decklinkvideosrc in the same
	// pipeline the audio branch produces ZERO buffers — 0 level messages against
	// 160 — and the leg never starts. So this field always builds a decklink
	// VIDEO element too:
	//
	//	VideoCaptureID == AudioCaptureID   ONE decklinkvideosrc (vcapsrc) serves
	//	                                   the video leg AND the audio clock. The
	//	                                   card is EXCLUSIVE — two sources in one
	//	                                   process fail 3/3 and two processes fail
	//	                                   3/3 — so a second one is impossible,
	//	                                   not merely wasteful.
	//	VideoCaptureID == ""               a decklinkvideosrc named vcapclock is
	//	                                   built beside the slate leg, feeding
	//	                                   fakesink, purely to clock this. It
	//	                                   costs 0.6-2.4 % of one core, measured,
	//	                                   and touches nothing else in the graph.
	//
	// THE TWO IDS MUST NAME THE SAME CARD when both are set, and Start refuses
	// when they do not. config.json carries ONE decklinkPersistentId for both
	// legs, so the application cannot produce a disagreement; a caller that
	// hand-built one is describing a two-card rig whose audio clock would have
	// to be a THIRD decklink element on the other card, which nothing here has
	// ever run. Refusing by name beats building a shape nobody has measured.
	//
	// It is IGNORED BY EVERYTHING BELOW THE MIX MATRIX. audioresample, the
	// S16LE/48k/2ch capsfilter, alevel, the AAC encoder, aacparse, mpegtsmux and
	// srtsink see exactly what they see on a microphone seat, which is what lets
	// internal/sender and the timestamp discipline go on reasoning about one
	// graph. See ChannelMap for the routing and channelmap.go for why the matrix
	// is a NEGOTIATION CONSTRAINT rather than a gain.
	AudioCaptureID string

	// VideoCaptureID, when non-empty, turns the VIDEO LEG INTO A LIVE CAPTURE:
	// the DeckLink card it names is opened, conformed to ConformTo and encoded,
	// in place of the frozen slate. It is the persistent-id of a Device whose
	// Kind is KindDeckLink — the same string, from the same dropdown, that
	// AudioDeviceID takes for a card, because ONE persistent-id names the card
	// and serves its audio and video entries alike (measured: the Audio/Source
	// and Video/Source devices for the fitted UltraStudio 4K Mini both publish
	// 2747401380).
	//
	// THE ZERO VALUE IS THE SLATE, and that is the whole compatibility statement
	// for this feature. A seat that configures nothing builds byte-for-byte the
	// graph the on-air Windows build has always run — filesrc, pngdec,
	// imagefreeze — because the leg is chosen by whether this field is empty and
	// by nothing else. There is no card to detect, no enumeration at Start and
	// no way for a machine that happens to have a DeckLink fitted to start
	// transmitting its input because somebody plugged it in.
	//
	// It is a SECOND SOURCE MODE rather than a rewrite of the first. Everything
	// downstream of the H.264 encoder — h264parse, both queues, mpegtsmux,
	// ReplaceSink and the whole of internal/sender — sees one fixed format
	// forever, whichever leg is above it, which is what lets the timestamp
	// discipline and the reconnect ladder go on reasoning about the same graph.
	// The conform chain is what buys that: videoconvert, deinterlace, videoscale
	// and videorate between the card and the capsfilter, so a 1080i50 camera, a
	// 720p59.94 camera and the card's own 720x486 NTSC start-up placeholder all
	// leave the leg as the one raster and rate ConformTo names.
	//
	// It is an ID and not a bool for the reason Device.ID is persisted rather
	// than Device.Name: the machine may have two cards in it, and "the DeckLink"
	// is not a thing that can be resolved on a rig where a second one is fitted
	// for a different purpose. A value that is not a DeckLink persistent-id is
	// refused at Start, before anything is built — a CoreAudio unique-id or a
	// WASAPI endpoint GUID handed to the decklink element's persistent-id
	// property would leave it at its own -1 default, which means "use
	// device-number", which means whichever card the driver enumerated first.
	//
	// # What it costs, measured rather than reasoned about
	//
	// On the port machine (macOS arm64, GStreamer 1.26.10, the fitted
	// UltraStudio 4K Mini), 2026-08-16, 500 frames per run, as a percentage of
	// ONE core:
	//
	//	the card alone, straight to fakesink                     2.4 %
	//	the card through the whole conform chain                18.5 %
	//	the same, plus the tee, the queue and vtenc_h264 @10M   25.8 %
	//	the SHIPPING SLATE LEG plus the same encoder @10M        4.8 %
	//	the SHIPPING SLATE LEG plus the same encoder @2M         4.0 %
	//	the whole application pipeline, through this package,
	//	  capture and audio and mux and SRT to a cloud host,
	//	  45.6 s including Init and the plugin registry scan     26.8 %
	//
	// READ THE CONFORM COST WITH ITS INPUT, because it is the entire story. The
	// card was locked to 525i59.94 — 720x486, INTERLACED, bt601, pixel aspect
	// 10:11, 29.97 fps — and the target was 1920x1080p50 bt709 progressive
	// square-pixel NV12. That is the most expensive conversion this chain can
	// ever be asked for: deinterlace, a four-and-a-half-times area upscale, a
	// colorimetry conversion and a rate conversion, all at once. It is not the
	// case a facility will run.
	//
	// The case a facility WILL run was measured beside it: a conforming
	// 1920x1080p50 source through the identical chain and encoder cost 3.00 s
	// of CPU per 500 frames against 3.00 s for the source on its own — the
	// conform chain and the H.264 encode together were indistinguishable from
	// nothing, because vtenc_h264 runs on the media engine and every element in
	// the chain passes through when there is nothing to convert. That is what
	// deinterlace and videoscale being "free on progressive input" means in
	// numbers.
	//
	// So the honest summary is: there is no CPU argument against this feature,
	// and the one number that could look alarming is the price of a standard
	// definition interlaced input nobody is going to commentate over.
	VideoCaptureID string

	// Preview is the operator's choice about the CONFIDENCE MONITOR: a small,
	// slow, cheap second rendering of the pictures the encoder is getting, taken
	// off the capture leg's tee and drawn in a native surface. preview.go owns
	// the whole of it — the branch, the measurements, and every reason it might
	// decline to build.
	//
	// The ZERO VALUE IS OFF and off is the default everywhere. It is meaningful
	// only on a pipeline whose video leg is a live capture: the tee it hangs off
	// exists only there, and previewBranchFor refuses a preview on a slate
	// pipeline rather than naming an element gst_parse_launch would not find —
	// which would not be a missing preview, it would be a parse failure of the
	// whole contribution pipeline.
	//
	// It is WIRED: Start renders the branch with previewBranchFor, appends it to
	// the parse description, and hands the sink its surface with attachPreview
	// before the pipeline leaves NULL. Two things had to be true first, and both
	// are, so do not take either of them back out:
	//
	//   - capturefault.go classifies preview elements as classPreview and
	//     onBusMessage spares them. Without it a preview sink erroring — a GPU
	//     driver update mid-match, a display going away — falls through to the
	//     FATAL default and takes the COMMENTARY off air over a monitor nobody
	//     was looking at. preview_test.go's
	//     TestAPreviewInThePipelineIsSparedByTheBusFilter fails by name if the
	//     wiring is ever present without the classification.
	//   - Start rebuilds ONCE WITHOUT the branch if the pipeline will not go to
	//     PLAYING with it. A sink that exists and then will not start is a state
	//     change failure, which the bus filter never sees, so it is the one way
	//     an optional monitor could otherwise stop a seat going on air. See
	//     cgoPipeline.startBuiltLocked.
	Preview PreviewOpts

	// ConformTo is the raster and rate the VIDEO LEG is conformed to: what the
	// slate is scaled and paced into before it reaches the H.264 encoder. It is
	// the switcher's configured format, and it has to be, because M2L-X can be
	// configured into any format and requires every source to match the one it
	// is configured for. Until this field existed the leg was hardcoded to
	// 1920x1080p50, which is a live defect on any instance that is not.
	//
	// THE ZERO VALUE MEANS "NOTHING IS KNOWN" and resolves to
	// FallbackConformTarget() — 1920x1080p50, the value that was written into
	// the parse string. That is a normal state and not a fault: the switcher is
	// routinely unread when Start is pressed, and refusing to go on air over it
	// would be the wrong trade by a distance. Note that this is the OPPOSITE
	// reading of a zero from ConformTarget.Valid, which reports a zero target as
	// unusable — deliberately, so that a caller cannot pass a zero on to
	// something that needs a real one by accident. The fallback decision is made
	// once, here, in Start, and it is logged.
	//
	// The caller builds one with gst.ConformTargetFromRate — the switcher
	// reports its rate as a decimal and GStreamer takes nothing but a fraction —
	// or, for the operator-typed override, by transcribing the four fields of a
	// config.VideoFormatSpec, which (*config.Config).VideoFormatOverrideSpec
	// returns and config.ParseVideoFormat produces. app.go's conformFormat is
	// the one place that does both.
	//
	// THERE IS DELIBERATELY NO PARSER IN THIS PACKAGE. One grammar, one parser:
	// internal/config cannot import internal/gst — a config package that needs
	// GStreamer installed to be tested stops being tested — so the parse has to
	// live in config for config.Validate to be able to refuse a bad value at
	// all. A second parser here would be a second grammar, and the two would
	// drift into disagreeing about which strings are legal, which is a defect
	// whose symptom is a value the Settings screen accepts and Start rejects.
	// The field names of ConformTarget and config.VideoFormatSpec are identical
	// so that the transcription has nothing to get subtly wrong.
	ConformTo ConformTarget

	// VideoBitrateKbps is the H.264 encoder's bitrate in kilobits per second.
	// Zero means DefaultVideoBitrateKbps. Kilobits is the unit on mfh264enc and
	// on vtenc_h264 alike.
	//
	// The default is sized for the still slate this leg carries today and the
	// owner has ruled it far too low for live video; ~10000 is the figure
	// asked for. Nothing here caps the value — see DefaultVideoBitrateKbps for
	// why the default stays where it is anyway.
	VideoBitrateKbps int

	// AudioBitrateBps is the AAC encoder's bitrate in bits per second. Zero
	// means DefaultAudioBitrateBps. Bits is the unit on mfaacenc and on atenc
	// alike.
	AudioBitrateBps int

	// OnLevels, if set, is called with the audio level of the commentary being
	// sent — peak and RMS per channel, in dBFS — roughly twenty times a second
	// (the level element's 50 ms interval). It is what feeds the input meters
	// beside the big picture, and it measures the signal at the one point that
	// answers the operator's actual question: after audioconvert and
	// audioresample, immediately upstream of the AAC encoder, so what the meter
	// shows is what is ACTUALLY being encoded and sent. A meter fed from the
	// browser's idea of the microphone would keep moving while the wrong device
	// was selected, which is a reassurance nobody should be given.
	//
	// It is called ON THE BUS/STREAMING GOROUTINE and MUST NOT BLOCK: in the
	// real build the caller is a GStreamer streaming thread inside
	// gst_element_post_message, and a callback that waits there stalls the
	// capture chain the way the file comment in gst_cgo.go forbids at length.
	// Hand the value to a queue or an atomic and return.
	//
	// Values are clamped to the -100 dBFS floor before delivery — the level
	// element reports -inf for digital silence, and -inf neither survives JSON
	// nor draws on a meter. Nil means no metering; nothing else changes.
	OnLevels func(Levels)

	// OnChannelLevels, if set, is called with the level of the CAPTURE DEVICE'S
	// OWN CHANNELS — one entry per channel the input pad negotiated, sixteen on
	// a DeckLink card — ten times a second. It is the meter the operator uses to
	// find which channel the commentator is on, and it measures UPSTREAM of the
	// audioconvert that mixes those channels down to the encoder's two.
	//
	// It is a second callback rather than a wider OnLevels because the two
	// measure different points and answering one question with the other is a
	// meter that reads as live while showing the wrong signal. OnLevels is the
	// on-air stereo, after the matrix, and it is what a clipping indicator is
	// allowed to watch; this is what is arriving, before any routing decision has
	// been applied, and it is by definition unable to say what is going out. Both
	// elements post a GstStructure named "level", so the two are told apart by
	// the posting element's name — MEASURED, with both in one pipeline: 39 level
	// messages a second, every one of them matching the structure name, which
	// without that attribution would have fed one meter a sixteen-entry frame and
	// a two-entry frame alternately, twenty times a second each.
	//
	// The same thread rule as OnLevels applies without weakening: it is called on
	// a GStreamer streaming thread and MUST NOT BLOCK.
	//
	// The frame's LENGTH is the channel count, and it changes only when the
	// pipeline is rebuilt — index i is input channel i for the life of the
	// pipeline, which is what lets a renderer lay out its strips once. Nil means
	// no per-channel metering, and on a positioned capture source (every
	// microphone, every Dante endpoint) it is redundant rather than wrong: the
	// channels it would report are the two OnLevels already reports.
	OnChannelLevels func(Levels)

	// OnSignal, if set, is called when the VIDEO CAPTURE's input lock changes:
	// whether the card can see a picture at all, debounced. It is what feeds the
	// signal lamp, and it exists because nothing else in this application can
	// tell — a DeckLink that loses signal keeps emitting black GAP-flagged frames
	// at full rate forever, so the muxer never starves, the sender stays
	// CONNECTED and the switcher reports a healthy feed. See signalwatch.go.
	//
	// Unlike OnLevels this is NOT called on a streaming thread: the watchdog
	// polls from an ordinary goroutine of its own, so a callback here cannot
	// stall the capture chain. It must still not block — that goroutine is the
	// only thing taking readings, so an emit that waits delays every later sample
	// and the hold-offs quietly stop meaning seconds.
	//
	// Nil means no watchdog at all, and so does a pipeline whose video leg has no
	// element with a "signal" property: not a goroutine, not a ticker, nothing.
	OnSignal func(SignalReport)

	// ChannelMap is which of the capture device's input channels reach the
	// left and right of the commentary feed, and at what gain. It is applied
	// before the pipeline first produces a buffer, and can be changed at any
	// time afterwards with SetChannelMap.
	//
	// THE ZERO VALUE IS THE DEFAULT MAP, not silence — input 1 on the left,
	// input 2 on the right, at unity — and channelmap.go argues both halves of
	// that: why an unset map must produce audio rather than nothing, and why
	// that particular default is bit-for-bit what this application already sent
	// from a DeckLink card configured for two channels. An operator who never
	// opens the mapping UI hears exactly what they heard before it existed.
	//
	// It is IGNORED on a capture source that presents a positioned stream,
	// which is every native microphone and every Dante endpoint: audioconvert's
	// own downmix already knows what those channels are, and pinning a matrix
	// over it would be a change to the on-air Windows path for no gain. The
	// matrix is written only for an UNPOSITIONED stream — the sixteen channels
	// with channel-mask=0x0 a DeckLink card presents, which is the case
	// audioconvert cannot resolve on its own. Start logs which of the two it
	// found.
	ChannelMap ChannelMap

	// MuteCommentary starts the pipeline with the commentary muted on the SEND
	// PATH: the volume element named coughmute is written while the pipeline is
	// still in NULL, so a pipeline that is meant to be born muted never carries
	// one buffer of live commentary. coughmute.go carries the whole argument for
	// the mechanism and the measurements behind it.
	//
	// THE ZERO VALUE IS UNMUTED, which is what every seat that has never touched
	// a cough button gets, and it renders the identical property write the
	// element already has in the parse string.
	//
	// It exists because a Pipeline is SINGLE-USE. A reconnect does not need it —
	// an SRT drop swaps the sink and never touches the audio leg, so a live mute
	// survives it by construction — but the latched-fatal path discards the whole
	// pipeline, and the next one has no memory of anything. A caller that holds a
	// cough mute across such a rebuild reads CommentaryMuted off the pipeline it
	// is discarding and passes it here, and the replacement is muted BEFORE its
	// first buffer instead of a moment after it.
	//
	// It is the ONLY way to set the mute before Start. SetCommentaryMute refuses
	// on a pipeline that has not started rather than latching a second copy of
	// the same intent; see coughmute.go for why one control with two memories is
	// the failure this design is avoiding.
	MuteCommentary bool
}

// refuseWrongAudioSource is the WHOLE RULE about which element opens the
// commentary, applied by both twins before anything is built.
//
// It lives here, with no build tag, for the reason the conform resolution does:
// this is the check that stops a match going out down the wrong microphone, and
// a check that only exists in the cgo build is a check Gate A cannot run. Both
// Starts call it, so a combination Gate A accepts is a combination the card
// accepts and a combination Gate A refuses is one the real build refuses before
// touching an element.
//
// # The two fields are exclusive, and the emptiness is why
//
// AudioDeviceID names a platform endpoint; AudioCaptureID names a DeckLink
// card. They are two answers to one question, and the failure of allowing both
// is not ambiguity — it is that WHICH ONE WINS becomes a property of
// pipelineDescription rather than of anything the operator can see.
//
// The failure of allowing NEITHER is worse and is the one this application
// exists to make impossible. An empty device on wasapi2src or osxaudiosrc is not
// an error, it is THE SYSTEM DEFAULT INPUT — so a DeckLink seat that reached the
// platform element with an empty id would transmit the laptop's built-in
// microphone with every lamp green. Requiring exactly one is what makes that
// state unreachable by construction: the native element is built only when
// AudioDeviceID is non-empty, and the only way to get no native element and no
// card is to be refused here.
//
// # What each branch checks
//
// A DeckLink id is CONVERTED, not pattern-matched, by the same
// parseDeckLinkPersistentID configureDeckLinkSource will use — a CoreAudio
// unique-id or a WASAPI GUID reaching decklinkaudiosrc's persistent-id would
// leave it at its own -1 default, which means "use device-number", which means
// whichever card the driver enumerated first.
//
// A platform id goes through refuseRenderEndpoint, unchanged and for the reason
// device_id.go gives at length: wasapi2src accepts a PLAYBACK endpoint at
// construction and fails asynchronously with error 1551, which internal/sender
// then misreads as a network failure and retries forever.
func refuseWrongAudioSource(audioDeviceID, audioCaptureID string) error {
	switch {
	case audioDeviceID == "" && audioCaptureID == "":
		return errors.New("gst: the commentary capture has no source: PipelineOpts.AudioDeviceID " +
			"(a WASAPI or CoreAudio endpoint) and PipelineOpts.AudioCaptureID (a DeckLink card's " +
			"persistent-id) are both empty, and exactly one is required. An empty device id is not " +
			"an error on wasapi2src or osxaudiosrc — it is the SYSTEM DEFAULT INPUT — so a pipeline " +
			"built from this would go on air off whatever microphone the machine happens to prefer")

	case audioDeviceID != "" && audioCaptureID != "":
		return fmt.Errorf("gst: the commentary capture has two sources: PipelineOpts.AudioDeviceID "+
			"is %q and PipelineOpts.AudioCaptureID is %q, and exactly one is required. Which one "+
			"reached the element would be a property of the pipeline builder rather than of "+
			"anything the operator chose", audioDeviceID, audioCaptureID)

	case audioCaptureID != "":
		if _, err := parseDeckLinkPersistentID(audioCaptureID); err != nil {
			return fmt.Errorf("gst: PipelineOpts.AudioCaptureID names the DeckLink card the "+
				"commentary is captured from, and it must be a DeckLink persistent-id rather than "+
				"an audio device id: %w", err)
		}
		return nil
	}

	return refuseRenderEndpoint(audioDeviceID)
}

// deckLinkAudioChannels is the `channels` property decklinkaudiosrc is built
// with, and 16 is the only value this application can use.
//
// MEASURED on the fitted UltraStudio 4K Mini, 2026-08-16, asking the source pad
// what it will produce with the pipeline still in NULL:
//
//	channels=16   channel-mask=0x0, UNPOSITIONED, no stereo pair available
//	channels=8    channel-mask=0x0, UNPOSITIONED, no stereo pair available
//	channels=2    a POSITIONED 0x3 pair, negotiates with no matrix at all
//
// channels=2 therefore looks like the easy way out of the unpositioned problem
// and it is the wrong answer, for a reason that has nothing to do with
// negotiation: it can only ever reach the card's FIRST PAIR. The commentator may
// be on any of sixteen embedded channels — which is the entire reason the
// routing model, the mix matrix, the sixteen-bar picker meter and the mapping
// screen were built — so a source that cannot see channels 3 to 16 makes all of
// that unreachable. Sixteen has to be the steady state for the further reason
// that `channels` is NOT live-settable: reaching another pair by changing it
// would cost a pipeline restart, mid-match.
//
// `max` is deliberately not used. It publishes a CHOICE in NULL (2, or 8-or-16)
// and fixes itself only once the card is open, which is after the moment the mix
// matrix has to be sized. fixedChannelCount refuses it by name for that reason.
const deckLinkAudioChannels = 16

// It lives here, with no build tag, rather than beside audioCaptureFactory in
// gst_cgo.go, because the stub twin models it: a DeckLink commentary seat whose
// fake pad negotiated a stereo pair is a shape the real build cannot produce,
// and a Gate A test written against one would be testing a fiction.

// Levels is one audio level report from the send pipeline: what the level
// element measured over its last 50 ms window, immediately upstream of the AAC
// encoder.
//
// PeakDB and RMSDB carry one entry per channel — [left, right] for the
// specification's stereo pipeline — in dBFS, 0 being digital full scale.
// Silence is -100, never -inf: the producer clamps (see clampLevelDB), because
// -inf does not survive JSON and a meter has a floor anyway.
type Levels struct {
	// PeakDB is the per-channel peak over the measurement window, dBFS.
	PeakDB []float64
	// RMSDB is the per-channel RMS over the same window, dBFS. Always at or
	// below the peak for any real signal.
	RMSDB []float64
}

// SinkOpts configures the srtsink alone. It is separate from PipelineOpts
// because reconnecting replaces only the sink: the capture, encode and mux chain
// never leaves PLAYING for the life of the process, so running time never moves
// backwards and the measured non-monotonic DTS jump cannot arise.
type SinkOpts struct {
	// Host is the M2L-X SRT listener's hostname or address.
	Host string

	// Port is that listener's port.
	Port int

	// LatencyMs is srtsink's latency property in MILLISECONDS. Zero means
	// DefaultSRTLatencyMs.
	LatencyMs int

	// Passphrase is the SRT passphrase from Credential Manager. It is set with
	// g_object_set, never placed in the URI, so it is neither percent-encoded
	// nor logged. Empty means an unencrypted session.
	Passphrase string

	// PBKeyLen is the SRT key length, 16 or 32. Ignored when Passphrase is
	// empty.
	PBKeyLen int
}

// Pipeline is one media pipeline: slate plus commentary audio, encoded, muxed to
// MPEG-TS and sent to an SRT listener as a caller.
//
// A Pipeline is single-use. After Stop it cannot be restarted; call New again.
// All methods are safe for concurrent use.
type Pipeline interface {
	// Start builds the pipeline and takes it to PLAYING, but installs NO SINK.
	// The pipeline runs with the src pad of the leaky srtq queue blocked, which
	// means capture and encoding are live and correctly paced before the first
	// connection attempt. The caller must follow Start with ReplaceSink to
	// actually connect.
	//
	// Start pins the system clock and reuses one process-lifetime base time, so
	// that a pipeline rebuilt after a device change does not restart PTS from
	// zero. It returns an error if called twice or after Stop.
	Start(opts PipelineOpts) error

	// ReplaceSink installs a fresh srtsink with the given properties, replacing
	// any sink already present: block the srtq src pad, unlink, set the old sink
	// to NULL, remove it, create and add the new one, link, SyncStateWithParent,
	// unblock.
	//
	// It returns synchronously. A nil error means the SRT caller handshake
	// succeeded and media is flowing; a non-nil error means it did not, and the
	// caller — internal/sender — is responsible for backing off and trying
	// again. Nothing upstream of srtq leaves PLAYING either way.
	//
	// One class of failure is not retryable, and ReplaceSink says so: an error
	// satisfying errors.Is(err, ErrPipelineFatal) means the capture or mux
	// chain itself has failed and no sink swap can carry media again. The
	// condition latches for the life of the pipeline — every subsequent
	// ReplaceSink returns it — and the only recovery is Stop, New, Start.
	// Backing off and retrying such an error is the measured misdiagnosis this
	// sentinel exists to end: a broken local device reported to the operator
	// as a network that keeps refusing to connect.
	ReplaceSink(opts SinkOpts) error

	// RemoveSink tears the current sink out without installing another: block the
	// srtq src pad, unlink, set srtsink to NULL, remove it. Everything upstream
	// stays in PLAYING. It is idempotent — removing when no sink is installed is
	// not an error.
	//
	// This exists because specification section 6.2 orders the reconnect
	// DRAINING (tear down) -> BACKOFF (wait) -> CONNECTING (install), and that
	// order is load-bearing rather than cosmetic. An M2L-X SRT listener accepts
	// exactly one peer, never displaces the incumbent, and refuses re-accept for
	// roughly five seconds; the >= 6 s first rung of sender.BackoffLadder is
	// sized to outlast that window. The wait only achieves anything if OUR socket
	// is already gone when it starts.
	//
	// Without this method the only teardown available is the one inside
	// ReplaceSink, which destroys the old sink microseconds before dialling the
	// new one — so the backoff elapses while we still hold the socket, and the
	// retry lands inside the refusal window it was supposed to clear. That costs
	// a wasted attempt, and roughly fourteen seconds off air instead of seven, on
	// every mid-match reconnect.
	//
	// Added after WP-0 by the coordinator, on the adversarial review of
	// internal/sender finding 1. It is the one change to this interface since it
	// was frozen.
	RemoveSink() error

	// ForceKeyUnit sends a GstForceKeyUnit event upstream so that the encoder
	// emits an IDR immediately. It is called after a successful ReplaceSink so
	// the picture recovers at once instead of waiting up to two seconds for the
	// next scheduled IDR.
	ForceKeyUnit() error

	// InputChannels reports how many channels the capture source's pad ACTUALLY
	// NEGOTIATED, read from that pad's current caps at the moment of the call.
	// It is 0 before Start, and 0 whenever nothing has negotiated.
	//
	// It is the ONLY number a caller may size a channel map against, and it is
	// deliberately not the same number as the device's advertised max-channels:
	// a DeckLink card publishes max-channels=16 whatever its element is
	// configured to produce, and a matrix built against a width the pad did not
	// negotiate does not degrade the feed, it stops it — measured, "streaming
	// stopped, reason error (-5)", with the capture chain dead before the next
	// level message. ChannelMap.MixMatrix takes the count as an argument for
	// that reason, so that the only way to build a matrix is to have asked.
	//
	// It is also what the mapping UI draws: sixteen faders on a card presenting
	// sixteen channels, two on a microphone.
	InputChannels() int

	// SetChannelMap changes the routing WHILE THE PIPELINE IS PLAYING, with no
	// state change, no renegotiation and no interruption to the feed. The
	// mapping UI is therefore a real-time control rather than an
	// apply-and-restart form.
	//
	// Measured on the real card on 2026-08-16: the property write took 119 µs,
	// the pipeline stayed PLAYING with nothing pending, and the change was
	// audible in the very next level message — a known -9 dBFS tone read
	// -8.9996 dBFS at unity and -15.0195 dBFS after a live write of 0.5, a
	// delta of -6.0199 dB against the -6.0206 dB the arithmetic says.
	//
	// The map is validated against InputChannels() BEFORE anything is written,
	// and a map that does not fit is refused with an error wrapping
	// ErrChannelMap and NOTHING IS SENT TO THE ELEMENT. That order is not
	// defensive tidiness: audioconvert rejects an out-of-range coefficient
	// silently, leaving the previous matrix in force, so a rejected write and a
	// successful one are indistinguishable at the property level and the
	// application can never learn afterwards which matrix is running. See
	// ChannelGainLimit for the measurement.
	//
	// It returns an error, and does not write, on a pipeline that is not
	// started, that has been stopped, or whose capture source presents a
	// POSITIONED stream — the last because there is no matrix in force on such
	// a pipeline to change, and installing one live would renegotiate the very
	// caps the feed is running on.
	SetChannelMap(m ChannelMap) error

	// SetCommentaryMute takes the commentary off the SEND PATH, or puts it back,
	// WHILE THE PIPELINE IS PLAYING. It is what the cough buttons drive — both
	// the push-to-mute one, which sets true on press and false on release, and
	// the latching one, which toggles. Neither mode is a concept this package
	// has: they are two ways of calling one boolean, and keeping it that way is
	// what makes the answer to "is the microphone open" a single fact.
	//
	// It is a property write on a volume element sitting between the resampler's
	// capsfilter and the programme meter. No state change, no renegotiation, no
	// element added to or removed from a running graph, and NOTHING SHARED with
	// the channel map: the routing and the mute are separate properties on
	// separate elements and cannot disagree. coughmute.go holds the argument
	// against the two alternatives — zeroing the mix matrix, and a valve — and
	// the measurements that settle it.
	//
	// MEASURED ON THE SHIPPED PIPELINE with the fitted card on 2026-08-16: the
	// mute write took 109.792 us and the unmute 101 us, the pipeline was PLAYING
	// with nothing pending across both, no bus message was posted, and the
	// programme meter went from -57.0538 dBFS to -100.0000 — the clamped
	// digital-silence floor — and back, at an unchanged thirty frames a window.
	//
	// WHAT THE FAR END SEES IS A CONTINUOUS STREAM THAT HAPPENS TO BE SILENT.
	// Measured through the shipped encoder chain into mpegtsmux on 2026-08-16,
	// muted and unmuted: 473 AAC access units either way, the same first PTS,
	// the same last PTS, and the same largest gap between packets — one AAC
	// frame, which is the floor. There is no discontinuity to recover from.
	//
	// THE PROGRAMME METER TELLS THE TRUTH THROUGHOUT, because the mute is
	// upstream of it: OnLevels goes on arriving at its own rate and reads
	// digital silence rather than freezing. A frozen meter and a silent one look
	// the same on screen for the first second and mean opposite things.
	//
	// It returns an error, and writes nothing, on a pipeline that is stopped or
	// that has not been started. The pre-Start case is deliberately a refusal
	// rather than a latch — PipelineOpts.MuteCommentary is that route, and one
	// control with two memories is the failure mode this design exists to avoid.
	//
	// It survives a reconnect by construction. internal/sender's reconnect is
	// RemoveSink, backoff, ReplaceSink; none of the three goes near the audio
	// leg, and everything upstream of srtq stays in PLAYING for the life of the
	// process.
	SetCommentaryMute(mute bool) error

	// CommentaryMuted reports whether the commentary is muted on the send path.
	//
	// IT IS AN OBSERVATION, NOT A RECOLLECTION. The value returned is the one
	// READ BACK OFF THE ELEMENT after the last successful write — the volume
	// element's mute property is readable, and both Start and SetCommentaryMute
	// re-read it immediately after setting it, so a write that did not take
	// cannot leave the application believing it did. That is the property that
	// disqualified zeroing the mix matrix, which does not marshal back out of
	// audioconvert at any price.
	//
	// It takes no lock and never blocks, which is why it is safe to call from a
	// UI poll and from a status assembly that runs while a reconnect is in
	// flight: the reads come off an atomic mirror rather than out of the
	// GStreamer graph, and ReplaceSink can hold the pipeline lock for seconds.
	//
	// Before Start it reports the pipeline's configured PipelineOpts.MuteCommentary
	// only once Start has applied it; on a pipeline that has never started it is
	// false, and on a stopped one it is the last state the element confirmed —
	// which is what a caller rebuilding after a fatal reads and hands to the
	// replacement's PipelineOpts.MuteCommentary.
	CommentaryMuted() bool

	// Errors returns the pipeline's asynchronous error channel, carrying
	// GST_ELEMENT_ERROR messages from the bus — in practice, srtout losing its
	// peer. Synchronous failures are returned by the method that caused them and
	// do not appear here.
	//
	// The channel is closed by Stop. Consumers must handle the closed case and
	// must drain it: implementations drop rather than block if it is full.
	Errors() <-chan error

	// Stop takes the pipeline to NULL, releases the capture device and closes
	// the channel returned by Errors. It is idempotent.
	Stop() error
}
