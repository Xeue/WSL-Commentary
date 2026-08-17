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

// SendOpts is EVERYTHING a send pipeline can be configured with, and the whole
// of the list is two bitrates.
//
// That is the seam's central claim made into a type. The send pipeline's source
// of media is a proxysrc, so it opens no device, owns no card, holds no slate,
// renders no preview, writes no channel map and carries no cough mute: every one
// of those is upstream of the proxysink and belongs to CaptureOpts. A send
// pipeline cannot be configured to open a device and a capture pipeline cannot be
// configured to send, which is what makes "a send pipeline with no device behind
// it" unconstructible rather than merely unlikely — see New, which takes the
// CaptureSet the pipeline is minted from.
//
// The conform target is NOT here, and its absence is the most consequential of
// them. It sits on the capture side so that the caps crossing the seam are pinned
// for the life of the process: the card changing raster (a 720x486 NTSC
// placeholder before it locks, the real input after, back again when the cable is
// pulled) renegotiates videoscale's SINK caps only, and the encoder in this
// pipeline never sees a caps change at all.
type SendOpts struct {
	// VideoBitrateKbps is the H.264 encoder's bitrate property, in KILOBITS per
	// second. Zero means DefaultVideoBitrateKbps.
	VideoBitrateKbps int

	// AudioBitrateBps is the AAC encoder's bitrate property, in BITS per second.
	// Zero means DefaultAudioBitrateBps. The two units differ because the two
	// elements' properties do, on both platforms; see DefaultAudioBitrateBps.
	AudioBitrateBps int
}

// PipelineOpts IS DELETED, and this is where it was.
//
// It configured everything upstream of the sink when ONE pipeline carried both
// capture and send: the slate, both capture ids, the conform target, both
// bitrates, the channel map, the mute and the three callbacks. Nine of its
// thirteen fields are now CaptureOpts', two are SendOpts', and it was kept for
// one phase — unreferenced by New and by Start — so that the source-reading
// guards over pipelineDescription went on running while app.go was re-pointed at
// the capture layer. That phase is over and both are gone together.
//
// WHERE EACH FIELD WENT, because a reader arriving from a caller will look:
//
//	SlatePath        CaptureOpts.SlatePath
//	AudioDeviceID    CaptureOpts.AudioDeviceID
//	AudioCaptureID   CaptureOpts.AudioCaptureID
//	VideoCaptureID   CaptureOpts.VideoCaptureID
//	Preview          CaptureOpts.Preview
//	ConformTo        CaptureOpts.ConformTo   — the move that pins the seam caps
//	ChannelMap       CaptureOpts.ChannelMap
//	MuteCommentary   CaptureOpts.MuteCommentary
//	OnLevels         CaptureOpts.OnLevels
//	OnChannelLevels  CaptureOpts.OnChannelLevels
//	OnSignal         CaptureOpts.OnSignal
//	VideoBitrateKbps SendOpts.VideoBitrateKbps
//	AudioBitrateBps  SendOpts.AudioBitrateBps
//
// ConformTo's move is the consequential one and is the reason the whole split is
// worth its cost. With the conform chain in the CAPTURE pipeline the caps
// crossing the proxy are pinned for the life of the process, so the DeckLink
// changing raster — a 720x486 NTSC placeholder before it locks, the real input
// after, back again when the cable is pulled — renegotiates videoscale's SINK
// caps only and the encoder never sees a caps change at all.
//
// CaptureOpts gained one field that was never here: DeviceChannels, the width the
// device ENUMERATION reports. The matrix has to be written while the pipeline is
// in NULL, where a native source's pad publishes only a range, so a width from
// the enumeration is the only thing that can size it before negotiation.

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

// Pipeline is ONE SEND SESSION: two proxysrcs taking the always-live capture
// layer's picture and commentary across the seam, encoded, muxed to MPEG-TS and
// sent to an SRT listener as a caller.
//
// IT OPENS NOTHING. Its source of media is a proxysrc bound to a CapturePipeline's
// proxysink, so there is no device in it, no card, no slate, no preview and no
// meter — every one of those lives in the capture layer, exists before START and
// survives STOP. That is the whole of R1, and it is why this interface lost four
// methods when the seam was cut: InputChannels, SetChannelMap, SetCommentaryMute
// and CommentaryMuted are CapturePipeline's, answered by the pipeline that owns
// the device, whether or not anything is sending. A caller that used to reach
// them through the session's pipeline reaches them through the capture set
// instead, and gets an answer with no session running, which is the point.
//
// A Pipeline is single-use. After Stop it cannot be restarted; call New again.
// All methods are safe for concurrent use.
type Pipeline interface {
	// Start builds the pipeline and takes it to PLAYING, but installs NO SINK.
	// The pipeline runs with the src pad of the leaky srtq queue blocked, which
	// means the encoders are live and correctly paced before the first connection
	// attempt. The caller must follow Start with ReplaceSink to actually connect.
	//
	// BEFORE IT PARSES ANYTHING it takes the single-consumer claim on every
	// proxysink in the capture set this pipeline was minted from and RE-ARMS them,
	// because gstproxysink.c resets sent_stream_start / sent_caps only on
	// READY->PAUSED: without the arming every send session after the first
	// receives no STREAM_START, no CAPS and no SEGMENT, and carries ZERO BYTES
	// with SRT connected and every lamp green. See seam.go for the measurement.
	//
	// AFTER PLAYING it refuses to report success until media has actually reached
	// the muxer, within liveWatchStartGrace. That is the guard that makes a missed
	// arming loud instead of green; a healthy seam beats it by two orders of
	// magnitude (20-60 ms, measured on both rigs).
	//
	// Start pins the system clock and reuses one process-lifetime base time, so
	// that a send pipeline built an hour after the capture it draws from does not
	// restart PTS from zero. It returns an error if called twice or after Stop, and
	// an error wrapping ErrSeamBusy if another send pipeline still holds the seam.
	Start(opts SendOpts) error

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

	// InputChannels, SetChannelMap, SetCommentaryMute and CommentaryMuted USED TO
	// BE HERE, and they are not gone: they are CapturePipeline's, and every
	// measurement that argued for them is on that interface now.
	//
	// They moved because they were never questions about a send session. They are
	// questions about the pipeline that has the device open — how many channels
	// that device's pad negotiated, which of them reach the feed, and whether the
	// commentator's microphone is open — and the capture pipeline holds the device
	// from launch to quit. Answered here they could only ever be answered while
	// something was sending, which is what made the routing grid unsizeable until
	// START and the cough button dead until START. Answered on the capture
	// pipeline they work with no session running, survive STOP, and keep working
	// across a reconnect that never goes near the audio leg.
	//
	// The one that changed meaning rather than address is the mute. Its old
	// pre-Start refusal existed so that one control could not have two memories;
	// with the element live from launch there is only one memory again — the
	// element's own, read back after every write — so a latch set before START is
	// simply still set at START. See CapturePipeline.SetCommentaryMute.

	// Errors returns the pipeline's asynchronous error channel, carrying
	// GST_ELEMENT_ERROR messages from the bus — in practice, srtout losing its
	// peer. Synchronous failures are returned by the method that caused them and
	// do not appear here.
	//
	// The channel is closed by Stop. Consumers must handle the closed case and
	// must drain it: implementations drop rather than block if it is full.
	Errors() <-chan error

	// Stop takes the pipeline to NULL, releases the seam and closes the channel
	// returned by Errors. It is idempotent.
	//
	// IT RELEASES NO DEVICE, and that sentence used to say the opposite. The
	// device belongs to the capture pipelines, which outlive every send session;
	// Stop here gives back the single-consumer claim on their proxysinks so that
	// the next START can take it, and touches nothing else. The order matters and
	// is enforced by the seam itself: the send pipeline reaches NULL first, and only
	// then is the claim released, because a proxysink whose old consumer is still
	// alive cannot be re-armed and the next session would carry zero bytes.
	Stop() error
}
