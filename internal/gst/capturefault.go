// capturefault.go is the bus error filter: which element posted a
// GST_ELEMENT_ERROR decides whether the commentary goes off air.
//
// Owner: WP-2, by agreement with WP-3a, which owns gst_cgo.go and calls this
// from onBusMessage. It is a file of its own, with no build tag, so that the
// whole decision — the classification and the three-way diagnosis both — is
// ordinary Go that Gate A compiles and unit-tests with no GStreamer anywhere.
// Its cgo twin, capturefault_cgo.go, does nothing but READ the two properties
// that turn a generic stream error into a named one.
//
// # What this replaces, and why it had to change
//
// Until now the rule was one line: isSinkSourced spares srtq and srtout-*, and
// EVERYTHING else goes markFatal -> ErrPipelineFatal -> internal/sender returns
// out of its loop -> the commentary is off air until a human presses STOP and
// then START.
//
// That rule was exactly right for the pipeline it was written for. The video
// leg was a PNG: filesrc, pngdec, imagefreeze. After Start it cannot fail, so
// every error that was not the sink really was the capture or mux chain, and
// treating it as terminal was the honest answer.
//
// It stops being right the moment a CAPTURE DEVICE enters the video leg. A
// DeckLink card can be unplugged, can lose its SDI signal, can be taken by
// another application at Start — and under the old rule any of those takes the
// COMMENTARY off air, which is a regression against a build in which a video
// fault was impossible. That is the whole reason this file is Tier 1 work and
// not something to add after the capture lands.
//
// # The split, and the one case that looks like an exception and is not
//
//   - THE SINK, and the leaky queue in front of it. Unchanged: replacing the
//     sink can repair it, so internal/sender's ladder handles it.
//   - THE VIDEO CAPTURE LEG. RECOVERABLE. Log it, keep the pipeline PLAYING,
//     keep the audio flowing, keep the sender's socket. The far end sees the
//     video freeze or go black and hears uninterrupted commentary, which is
//     the correct degradation: the commentary IS the product.
//   - THE CONFIDENCE MONITOR, the optional preview branch hanging off the
//     capture leg's tee. SPARED ABSOLUTELY, and it is a class of its own rather
//     than part of the video capture leg for the one reason that makes the
//     feature safe: the video class UPGRADES to fatal when the commentary is
//     clocked by the card's video, which is the very configuration a preview is
//     for, so it would upgrade nearly every time. See classPreview.
//   - THE AUDIO CAPTURE. FATAL, because there is nothing to degrade to — but
//     NAMED, because at the GStreamer level "device busy", "device missing"
//     and "no signal" are the same generic stream error, and three problems
//     with three different fixes deserve better than one message twenty
//     minutes before kick-off. See diagnoseCaptureFault.
//   - EVERYTHING ELSE. Fatal, exactly as before.
//
// The apparent exception: when the commentary audio comes from the DeckLink,
// a VIDEO capture error is not recoverable at all, and this file says so.
// MEASURED: decklinkaudiosrc cannot preroll without a decklinkvideosrc in the
// same pipeline, because the card drives audio capture off the video clock —
// with drop-no-signal-frames=true and no video, the audio branch produced ZERO
// buffers, 0 level messages against 160. So in that configuration the video
// capture is not a video source that happens to be there, it is the clock the
// commentary is captured against, and an error from it is an audio fault
// wearing a video element's name. Classifying it as recoverable would keep the
// pipeline PLAYING and the sender connected with nothing but silence going out
// — the one failure mode worse than being off air, because every lamp stays
// green.
//
// # What is given up
//
// The classification is by ELEMENT NAME, which means the names in
// pipelineDescription are load-bearing and a renamed element silently rejoins
// the fatal default. That is the safe direction to fail — an unclassified
// element is treated exactly as it is today — and it is why the names live in
// this file as constants rather than as literals in the parse string.
//
// The diagnosis below is a decision made from three pieces of evidence, none
// of which is conclusive on its own. It can be wrong. Every message it
// produces therefore carries the raw GStreamer text as well, so an operator or
// a field log always has the thing the diagnosis was made FROM.

package gst

import (
	"fmt"
	"strconv"
	"strings"
)

// The element names this filter classifies on.
//
// THEY ARE DECLARED HERE AND NOT SHARED WITH gst_cgo.go's const block, and the
// reason is a build tag rather than a preference: gst_cgo.go is
// `cgo && !gststub`, so at Gate A — which is where the tests for the decision
// that takes the commentary off air have to run — its constants do not exist.
// Three duplicated short strings are the price of that, and the duplication is
// not left to trust: TestCaptureFaultNamesMatchThePipeline reads gst_cgo.go and
// fails by name if either half is renamed.
//
// Every element of the video capture leg begins with videoCaptureNamePrefix,
// and that is the whole mechanism: the conform chain downstream of the capture
// (videoconvert, deinterlace, videoscale, videorate) fails as one unit with the
// source, so classifying them individually would be four constants that must
// never disagree. The prefix also covers elements that do not exist yet, which
// is the case that matters — this filter has to be correct on the day the
// capture leg lands, not merely on the day it is written.
//
// NOTHING IN THE SLATE LEG CARRIES THAT PREFIX, deliberately. filesrc, pngdec
// and imagefreeze cannot fail after Start — the picture is decoded once and
// repeated — so an error from one of them is a genuine surprise and stays
// fatal, which is what it is today.
const (
	// captureSinkQueueName and captureSinkNamePrefix mirror gst_cgo.go's
	// nameSRTQueue and srtSinkNamePrefix: the sink and the leaky queue in front
	// of it, whose errors ReplaceSink can repair.
	captureSinkQueueName  = "srtq"
	captureSinkNamePrefix = "srtout-"

	// captureAudioSrcName mirrors gst_cgo.go's nameAudioSrc: the commentary
	// capture element, whatever platform factory is behind it.
	captureAudioSrcName = "asrc"

	// videoCaptureNamePrefix is what every element of the video capture leg is
	// named with. pipelineDescription must use these names; see above for what
	// happens if it does not.
	videoCaptureNamePrefix = "vcap"

	// nameVideoCaptureSrc is the capture element itself (decklinkvideosrc).
	nameVideoCaptureSrc = "vcapsrc"

	// nameVideoCaptureClock is the decklinkvideosrc that exists ONLY to clock
	// decklinkaudiosrc, in the configuration where the commentary comes off the
	// card and the video does not. It is named separately from the real capture
	// because they are never both present and because a reader of a log needs
	// to know which of the two they are looking at.
	nameVideoCaptureClock = "vcapclock"
)

// busErrorClass is what to DO about one bus error, decided by the element that
// posted it.
type busErrorClass int

const (
	// classFatal is the default and is today's behaviour for everything that
	// is not the sink: markFatal, ErrPipelineFatal, the sender stops.
	classFatal busErrorClass = iota

	// classSinkSourced is the sink or the leaky queue in front of it.
	// Unchanged: not fatal, because ReplaceSink can repair it.
	classSinkSourced

	// classVideoCapture is a video capture leg failure with the commentary
	// audio coming from somewhere else. RECOVERABLE.
	classVideoCapture

	// classAudioCapture is the commentary capture itself. Fatal, and named.
	classAudioCapture

	// classPreview is the DeckLink CONFIDENCE MONITOR's own branch: the leaky
	// queue, the rate cap, the scaler and the sink that draw a small copy of the
	// capture in a native surface over the page. SPARED, ABSOLUTELY.
	//
	// It is its OWN class and not classVideoCapture for one reason, and it is the
	// reason the whole preview is safe: classVideoCapture is UPGRADED to
	// classAudioCapture whenever the commentary audio is clocked by the card's
	// video, which is the DeckLink configuration the preview exists for. It would
	// upgrade nearly every time, and a monitor nobody was looking at would take
	// the commentary off air. This class can never upgrade to anything.
	//
	// Nothing about the preview can affect the feed: it is downstream of a tee
	// whose head queue is leaky=downstream, it feeds a window, and it is absent
	// from the graph entirely on every seat that has not asked for it. See
	// preview.go.
	classPreview
)

// captureLegs is what the classification needs to know about how THIS pipeline
// was built. It is a value rather than a read of the live pipeline because
// onBusMessage runs on a streaming thread and may not take p.mu.
type captureLegs struct {
	// AudioClockedByVideo is true when the commentary audio is captured from
	// the same DeckLink card as the video and is therefore driven off the video
	// clock. It turns every video capture error into an audio capture fault.
	// See the file comment for the measurement.
	AudioClockedByVideo bool
}

// classifyBusError decides what one bus error means, from the name of the
// element that posted it.
//
// The order of the tests is the order of certainty, and the sink test is FIRST
// so that this can never change the behaviour of the one path that is on air
// today: whatever else is added to this pipeline, an srtout-N or srtq error is
// classified exactly as isSinkSourced classified it.
//
// # It is safe, and intended, to call this with the zero captureLegs first
//
// legs is consulted in exactly ONE branch — the video-capture prefix — so for
// every other source, classifyBusError(source, captureLegs{}) and
// classifyBusError(source, anything) are provably the same decision. That is
// what lets onBusMessage run this in two stages: the name-only answer decides
// whether to close the gate, and only a source that came back
// classVideoCapture is worth the cost of establishing the legs.
//
// The cost is the whole reason the split exists. Establishing the legs means
// capturefault_cgo.go's captureLegsFor, which crosses into C and runs
// gst_bin_get_by_name over the graph; doing that before the gate closes would
// put a bin traversal in front of the store on the path an srtout-N error takes
// on every peer loss. Do not "simplify" the call site back into one expression
// — TestBusHandlerClosesTheGateBeforeAnyCgoCall fails by name if it is.
func classifyBusError(source string, legs captureLegs) busErrorClass {
	switch {
	case source == captureSinkQueueName || strings.HasPrefix(source, captureSinkNamePrefix):
		return classSinkSourced
	case isPreviewSourced(source):
		// Second, immediately below the sink, because these are the two classes
		// that must never depend on anything else being right. isPreviewSourced
		// is preview.go's, which is where the naming rule and its test live.
		return classPreview
	case strings.HasPrefix(source, audioProxyNamePrefix):
		// THE COMMENTARY LEG'S TAIL: aproxq and aproxsink, the leaky queue and
		// the proxysink the send pipeline attaches to. It is the SAME FAULT as
		// asrc failing — there is no commentary crossing the seam either way —
		// so it is classified explicitly rather than left to fall through on a
		// prefix accident. It does not begin with "asrc" and would otherwise
		// have reached the nameless fatal default, which says "the capture or
		// mux chain has failed" about a leg this file can name.
		return classAudioCapture
	case source == captureAudioSrcName:
		return classAudioCapture
	case strings.HasPrefix(source, videoProxyNamePrefix):
		// THE PICTURE LEG'S TAIL: vproxq and vproxsink. It is a picture fault
		// and it is NOT upgraded by AudioClockedByVideo the way the capture
		// prefix below is, deliberately.
		//
		// The upgrade exists because a decklinkvideosrc is the CLOCK the
		// commentary is captured against. These two elements are downstream of
		// the tee, downstream of the conform chain and downstream of everything
		// that clocks anything: the card goes on producing, the audio leg goes
		// on being clocked, and the only thing that has died is the picture. An
		// upgrade here would take the commentary off air on every fused seat for
		// a queue.
		//
		// It is an explicit entry rather than a widening of the "vcap" prefix
		// because the tail exists on a SLATE picture leg too, where nothing at
		// all is named vcap-anything.
		return classVideoCapture
	case strings.HasPrefix(source, videoCaptureNamePrefix):
		if legs.AudioClockedByVideo {
			// Not a video fault. The commentary is captured against this
			// element's clock; without it there is no audio at all.
			return classAudioCapture
		}
		return classVideoCapture
	default:
		return classFatal
	}
}

// captureFault names WHICH of the three device failures happened. They are
// indistinguishable at the GStreamer level and have three different fixes.
type captureFault int

const (
	// faultUnknown is the honest answer when none of the three tests matched.
	// It is not a failure of this file; it is a failure to have enough
	// evidence, and the message says so rather than guessing.
	faultUnknown captureFault = iota

	// faultDeviceMissing is the device no longer being present at all: a
	// Thunderbolt DeckLink unplugged, a driver crash, a Dante Virtual
	// Soundcard or NDI endpoint whose source has gone away.
	faultDeviceMissing

	// faultDeviceBusy is another process holding the device. MEASURED on
	// DeckLink: contention produces "Internal data stream error /
	// not-negotiated (-4)" in about 100 microseconds, naming neither the
	// device nor the cause, and Premiere merely being open is enough.
	faultDeviceBusy

	// faultNoSignal is the card being ours and there being nothing on the
	// wire. It is an AUDIO fault on a DeckLink, not merely a video one: the
	// card drives audio capture off the video clock, so no locked video means
	// no commentary at all.
	faultNoSignal
)

// triState is a boolean that may also be "could not be read". It exists for
// the signal property, where "false" and "I could not ask" must never be
// confused: one of them means no signal and the other means no evidence.
type triState int

const (
	triUnknown triState = iota
	triFalse
	triTrue
)

// captureEvidence is everything the diagnosis has to go on, gathered by
// capturefault_cgo.go at the moment the error was posted.
//
// It is a plain value with no GStreamer in it, so the decision below is
// ordinary Go and Gate A tests every branch of it.
type captureEvidence struct {
	// DeviceName is the capture device as the operator chose it in the
	// dropdown, and DeviceID what is stored for it. Both may be empty; the
	// messages degrade to naming the element instead.
	DeviceName string
	DeviceID   string

	// DeckLink is true when the commentary comes off a Blackmagic card. It
	// changes the WORDING rather than the diagnosis — the exclusivity, the
	// audio-follows-video clock and Desktop Video Setup are all DeckLink facts
	// and would be noise on a USB interface.
	DeckLink bool

	// Connection is the configured DeckLink input connection ("sdi", "hdmi",
	// "optical", "auto"), named in the no-signal message because it is the
	// thing most likely to be wrong. MEASURED: connection=auto failed to lock
	// 3/3 against a valid SDI signal that connection=sdi locked 2/2 on, because
	// auto follows a card-level setting owned by Blackmagic Desktop Video Setup
	// that this application cannot see.
	Connection string

	// HWSerial is the card's hw-serial-number, so a rig with two cards in it
	// says WHICH one. Empty when there is none or it could not be read.
	HWSerial string

	// Enumerated reports whether the device is still in this machine's device
	// list, and EnumerationOK whether that list could be obtained at all. The
	// two are separate because "the enumeration says it is gone" and "the
	// enumeration failed" are opposite amounts of evidence, and an enumeration
	// that itself failed must never be read as the device being missing.
	Enumerated    bool
	EnumerationOK bool
	// DeviceCount is how many capture devices the enumeration did find, which
	// is what makes "and none of them is yours" a fact rather than an
	// assertion.
	DeviceCount int

	// Signal is the capture element's "signal" GObject property: whether the
	// card has locked to an input. triUnknown when there was no element to ask
	// or the property does not exist, which is the normal case for every
	// non-DeckLink source.
	Signal triState

	// Message is the GError text and the debug string, exactly as they came off
	// the bus. It is matched against and it is also quoted in every message
	// produced, so the diagnosis never replaces the evidence.
	Message string
}

// negotiationMarkers are the substrings that distinguish a device that could
// not be OPENED from a device that was open and then failed.
//
// The discrimination matters more than it looks. "Internal data stream error"
// alone is NOT one of these, deliberately: BUILD-NOTES.md section 8.6 recorded
// asrc posting exactly that text as the tail of a cascade that began at
// srtsink, on a pipeline whose device was perfectly healthy. Contention
// produces "Internal data stream error" AND "not-negotiated (-4)" together, so
// the negotiation half is the half that carries the information.
var negotiationMarkers = []string{
	"not-negotiated",
	"not negotiated",
	"e_accessdenied",
	"already in use",
	"device is busy",
	"resource busy",
	"could not be opened",
	"failed to open",
}

// missingMarkers are the substrings a device that is no longer THERE produces,
// as opposed to one that is there and refuses.
//
// They exist because the strongest evidence for "missing" — asking the
// operating system for its device list and not finding ours — is evidence this
// filter usually cannot obtain. See gatherCaptureEvidence: the diagnosis runs
// on the GStreamer streaming thread that is in the middle of failing, and
// starting a GstDeviceMonitor there would probe every provider on the machine,
// including the card that has just gone, from a thread nothing may block.
//
// So the enumeration is accepted when a caller can safely supply it and the
// message text is what answers otherwise. These are GST_RESOURCE_ERROR_NOT_FOUND
// and its neighbours as the two platforms' capture elements word them.
var missingMarkers = []string{
	"resource not found",
	"device not found",
	"no such device",
	"cannot find device",
	"device has been removed",
	"device disappeared",
	"was disconnected",
	"is not available",
}

// diagnoseCaptureFault decides which of the three it was.
//
// The tests are ordered by how much the evidence is worth, strongest first,
// and each one is a POSITIVE identification: nothing here concludes "busy"
// merely because it is not the other two. Failing to faultUnknown is the
// designed outcome for a case nobody has met, and its message says the three
// things to check rather than pretending to have chosen between them.
func diagnoseCaptureFault(ev captureEvidence) captureFault {
	// 1. The device is not there. This is the only test whose evidence is a
	//    fact rather than an inference — the operating system was asked for
	//    its device list and the one we hold is not in it — so it goes first
	//    and it needs no corroboration from the message text.
	if ev.EnumerationOK && !ev.Enumerated {
		return faultDeviceMissing
	}

	// 2. The card is ours and reports no lock. Only an explicit false counts:
	//    triUnknown means there was nothing to ask, which is every
	//    non-DeckLink source and is not evidence of anything.
	if ev.Signal == triFalse {
		return faultNoSignal
	}

	lower := strings.ToLower(ev.Message)

	// 3. The device is not there and the element said so. Weaker than the
	//    enumeration above and stronger than the negotiation test below, so it
	//    sits between them: a device that has been removed also fails to
	//    negotiate, and being told "your card has been unplugged" when the fix
	//    is to plug it back in beats being told to close another application.
	for _, m := range missingMarkers {
		if strings.Contains(lower, m) {
			return faultDeviceMissing
		}
	}

	// 4. Something else holds the device. The message has to name a
	//    NEGOTIATION or ACCESS failure, not merely a stream error — see
	//    negotiationMarkers for the cascade this distinction exists to
	//    exclude.
	for _, m := range negotiationMarkers {
		if strings.Contains(lower, m) {
			return faultDeviceBusy
		}
	}

	return faultUnknown
}

// captureFaultMessage renders the diagnosis as the sentence an operator reads.
//
// Every one of them says WHAT happened, WHY this application believes so, and
// WHAT TO DO, in that order, and every one of them ends with the raw GStreamer
// text — because the diagnosis is an inference and the operator or the person
// reading their log is entitled to the thing it was inferred from.
//
// They are deliberately long. The audience is one person, at a commentary
// position, with the match starting, and the cost of a sentence they do not
// need is nothing against the cost of a fix they cannot find.
func captureFaultMessage(fault captureFault, ev captureEvidence) string {
	device := ev.DeviceName
	if device == "" {
		device = ev.DeviceID
	}
	if device == "" {
		device = "the commentary input"
	} else {
		device = "the commentary input " + strconv.Quote(device)
	}
	if ev.HWSerial != "" {
		device += " (card serial " + ev.HWSerial + ")"
	}

	var b strings.Builder
	switch fault {
	case faultDeviceMissing:
		fmt.Fprintf(&b, "%s is no longer present on this machine", device)
		if ev.DeviceCount > 0 {
			fmt.Fprintf(&b, " — %d capture device(s) were found and none of them is that one", ev.DeviceCount)
		}
		b.WriteString(". ")
		if ev.DeckLink {
			b.WriteString("A DeckLink on Thunderbolt that has been unplugged, or a Desktop Video " +
				"driver that has restarted, does exactly this. Reconnect the card, wait for " +
				"Desktop Video Setup to see it, and press START again.")
		} else {
			b.WriteString("Dante Virtual Soundcard and NDI endpoints are created and destroyed as " +
				"their sources come and go, and a USB interface that has been unplugged does the " +
				"same. Reconnect it, choose it again in the Commentary input dropdown, and press " +
				"START again.")
		}

	case faultDeviceBusy:
		fmt.Fprintf(&b, "%s could not be opened because something else on this machine is using it. ", device)
		if ev.DeckLink {
			b.WriteString("A DeckLink card is EXCLUSIVE — Blackmagic Desktop Video Setup, Media " +
				"Express, Premiere, OBS or a previous copy of this application is enough, and the " +
				"card reports only a negotiation failure without naming who has it. Close the other " +
				"application and press START again. Note that nothing can take the card from us " +
				"once we hold it: a second opener fails and the incumbent survives, so this is a " +
				"problem at START and never mid-match.")
		} else {
			b.WriteString("Close whatever else has the device open — another copy of this " +
				"application, a conferencing client, or a DAW holding it exclusively — and press " +
				"START again.")
		}

	case faultNoSignal:
		fmt.Fprintf(&b, "%s is open, but the card reports NO SIGNAL", device)
		if ev.Connection != "" {
			fmt.Fprintf(&b, " on its %s input", strings.ToUpper(ev.Connection))
		}
		b.WriteString(". A DeckLink drives AUDIO capture off the VIDEO clock, so with nothing " +
			"locked there is no commentary audio at all — this is not a fault in the microphone or " +
			"the desk. Check the feed into the card")
		if ev.Connection == "auto" {
			b.WriteString(", and set the input connection explicitly rather than leaving it on " +
				"auto: auto follows a card-level setting owned by Blackmagic Desktop Video Setup " +
				"that this application cannot see, and it has been measured failing to lock on a " +
				"signal that an explicit connection locked to immediately")
		} else {
			b.WriteString(", and that Desktop Video Setup's input connection matches how the card " +
				"is actually patched")
		}
		b.WriteString(".")

	default:
		fmt.Fprintf(&b, "%s failed and did not say why. ", device)
		b.WriteString("At the GStreamer level a device that is BUSY, a device that is MISSING and " +
			"a card with NO SIGNAL all produce the same generic stream error, and this one matched " +
			"none of the three tests this application makes. Check, in this order: that nothing " +
			"else on the machine has the device open; that it is still connected; and that there " +
			"is signal on its input.")
	}

	if ev.Message != "" {
		b.WriteString(" GStreamer said: ")
		b.WriteString(ev.Message)
	}
	return b.String()
}
