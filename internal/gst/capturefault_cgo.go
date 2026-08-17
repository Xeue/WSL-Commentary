//go:build cgo && !gststub

// capturefault_cgo.go is the evidence half of the bus error filter: the only
// part of it that touches GStreamer.
//
// Owner: WP-2, by agreement with WP-3a. The DECISION lives in capturefault.go,
// which has no build tag and is fully tested at Gate A; everything here reads
// properties off elements that already exist and hands the result over as a
// plain captureEvidence value.
//
// # Everything in this file runs on a streaming thread that is failing
//
// onBusMessage is a bus SYNC handler: GStreamer calls it on whichever thread
// posted the message, which for a GST_ELEMENT_ERROR is the streaming thread in
// the middle of the failure, before that failure has propagated anywhere. The
// file comment on gst_cgo.go is emphatic about what may happen there, and this
// file obeys it:
//
//   - no lock is taken. p.mu in particular: ReplaceSink holds it while
//     deliberately provoking these messages.
//   - no logging. log.Printf takes a process-global mutex and blocks on stderr.
//   - NO DEVICE ENUMERATION, and this is the one that had to be argued rather
//     than merely observed. The single strongest piece of evidence for "your
//     device is missing" is asking the operating system for its device list and
//     not finding ours in it — and obtaining it means gst_device_monitor_start,
//     which probes every provider on the machine INCLUDING the card that has
//     just failed, from a thread nothing is allowed to block. So the
//     enumeration is deliberately not consulted here (EnumerationOK stays
//     false) and capturefault.go's missingMarkers answer instead. The
//     enumeration that WOULD be conclusive already happens where it is safe:
//     App.preflightAudioDevice runs it before the pipeline is built, and names
//     a device that is not on the machine before anything is opened at all.
//
// What is left is g_object_get on elements that are already instantiated, which
// is a struct field read behind a GObject lock — measured in nanoseconds, and
// the same primitive the level path already performs on this thread twenty
// times a second.

package gst

import (
	"fmt"
	"strconv"
	"strings"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// DeckLink element property names read for evidence. They are here rather than
// with decklinkdevices.go's property constants because these are read from a
// LIVE element mid-failure, not from a GstDevice during enumeration, and the
// two lists have no reason to move together.
const (
	// propSignal is decklinkvideosrc's read-only "signal" gboolean: whether the
	// card has locked to an input. It is the ONLY thing that can tell a real
	// 720x486 NTSC picture from the no-signal placeholder of the same size —
	// caps alone cannot, because GST_DECKLINK_MODE_AUTO reports modes[0] when
	// there is nothing there.
	propSignal = "signal"

	// propHWSerial is "hw-serial-number", so a rig with two cards in it says
	// WHICH one failed. Empty on a card that publishes none.
	propHWSerial = "hw-serial-number"

	// propConnection is the input connection ("auto", "sdi", "hdmi",
	// "optical"). It is an enum, and go-glib hands an enum back as an integer
	// with no way to name it from here, so it is read only when it arrives as a
	// string; the no-signal message degrades to not naming the connection
	// rather than naming a number nobody can act on.
	propConnection = "connection"
)

// deckLinkFactoryPrefix identifies a Blackmagic capture element by the factory
// that made it: decklinkvideosrc, decklinkaudiosrc.
//
// The factory rather than the element name, deliberately. The names are ours
// and say what an element is FOR; the factory says what it IS, and the wording
// of every message below — exclusivity, audio driven off the video clock,
// Desktop Video Setup — is true of the hardware and not of the role.
const deckLinkFactoryPrefix = "decklink"

// gatherCaptureEvidence reads everything that can safely be learned about a
// capture failure from the element that reported it.
//
// obj is what posted the error, which onBusMessage already has in hand as
// msg.Source(). It is taken as the GstObject the message actually carries
// rather than as an Element, so that the call site needs no type assertion and
// so that the two cases where it is NOT an element — a message posted by a bin,
// and one whose source has already been disposed — are handled here, once,
// instead of at the one place that must not grow a branch it might get wrong.
// In both of them the diagnosis degrades to the message text, and a message
// text is never nothing.
func gatherCaptureEvidence(obj gogst.Object, message string) captureEvidence {
	ev := captureEvidence{Message: message}
	src, ok := obj.(gogst.Element)
	if !ok || src == nil {
		return ev
	}

	factory := elementFactoryName(src)
	ev.DeckLink = strings.HasPrefix(factory, deckLinkFactoryPrefix)
	ev.DeviceID, ev.DeviceName = captureElementIdentity(src)
	ev.HWSerial = stringPropertyOrEmpty(src, propHWSerial)

	// The signal property belongs to the VIDEO element. When the audio source
	// is the one that failed it does not have it, so the video element in the
	// same pipeline is looked up — which in the DeckLink-audio configuration is
	// the clock companion that exists for exactly this reason, and is the
	// element that actually knows whether the card is locked.
	video := src
	if !hasProperty(video, propSignal) {
		video = siblingElement(src, nameVideoCaptureSrc, nameVideoCaptureClock)
	}
	if video != nil {
		ev.Signal = boolPropertyTriState(video, propSignal)
		if ev.HWSerial == "" {
			ev.HWSerial = stringPropertyOrEmpty(video, propHWSerial)
		}
		ev.Connection = stringPropertyOrEmpty(video, propConnection)
	}
	return ev
}

// captureLegsFor IS DELETED, and the argument it carried is answered here
// because the next reader will look for it.
//
// It walked up from the message source to the parent bin and ran
// gst_bin_get_by_name over the whole graph to ask whether the commentary capture
// was a DeckLink — because the old single pipeline could not say at build time
// which of the four shapes it was, and the coupling being asked about (a
// decklinkaudiosrc cannot preroll without a decklinkvideosrc in the same
// pipeline) is a property of the ELEMENT rather than of a remembered option.
//
// A CapturePipeline knows its leg-set before gst_parse_launch is called, so the
// answer is now a field read — CaptureLegs.AudioClockedByVideo, checked against
// the shape that was actually planned — and there is nothing to establish from
// the graph. The send bus, which was the other caller, no longer classifies
// capture faults at all: see classifySendBusError. The bin traversal that had to
// be kept off the on-air path is therefore not merely moved, it is gone.
//
// elementName went with it: siblingElement is the only remaining walker and it
// takes its names as arguments.

// captureFatalError turns a capture-element bus error into the named,
// pipeline-fatal error the operator actually reads.
//
// The wrap puts ErrPipelineFatal at the head of the chain exactly as
// onBusMessage's original markFatal site did, so internal/sender's errors.Is
// test is unchanged and anything still grepping the log for the substring
// "pipeline-fatal" keeps matching. What changes is everything after it: instead
// of one sentence that fits three problems, the message names which of them
// this is and what to do about it, and then quotes the GStreamer text it was
// inferred from.
func captureFatalError(obj gogst.Object, source string, err error) error {
	ev := gatherCaptureEvidence(obj, err.Error())
	fault := diagnoseCaptureFault(ev)
	return fmt.Errorf("%w: %s: %s (recover with Stop, New, Start)",
		ErrPipelineFatal, source, captureFaultMessage(fault, ev))
}

// elementFactoryName returns the name of the factory that created an element,
// or "" when it cannot be established.
func elementFactoryName(e gogst.Element) string {
	f := e.GetFactory()
	if f == nil {
		return ""
	}
	return f.GetName()
}

// captureElementIdentity returns the device the element was pointed at, as the
// id that was persisted and a name to show a human.
//
// The two spellings are tried in the order a capture element actually uses
// them, and the DeckLink one first because it is the only one that is a NUMBER:
// persistent-id is a gint64 naming the CARD (measured: the audio and video
// devices publish the same one), and it is what config.json stores. device is
// the platform sources' string endpoint id — a WASAPI IMMDevice GUID on
// Windows, and on macOS a gint AudioDeviceID that osxaudiosrc resolves from the
// stored CoreAudio UID, which is why an integer read back from it is a runtime
// handle and NOT something to print as an identity. Hence the name half stays
// empty for that case: internal/gst has no display name to hand at this point,
// and the message degrades to naming the element rather than inventing one.
func captureElementIdentity(e gogst.Element) (id, name string) {
	if hasProperty(e, propPersistentID) {
		if v, ok := e.ObjectProperty(propPersistentID).(int64); ok && v != unsetPersistentID {
			return strconv.FormatInt(v, 10), ""
		}
	}
	if s := stringPropertyOrEmpty(e, "device"); s != "" {
		return s, ""
	}
	return "", ""
}

// siblingElement finds an element by name in the same pipeline as src, trying
// each name in turn.
//
// It walks UP to the parent rather than using a stored pipeline handle,
// deliberately: cgoPipeline.pipeline is written under p.mu and this runs on a
// thread that may not take it, whereas gst_object_get_parent is refcounted and
// safe from anywhere. A nil return is ordinary — the video leg does not exist
// in the slate configuration at all.
func siblingElement(src gogst.Element, names ...string) gogst.Element {
	parent := src.GetParent()
	if parent == nil {
		return nil
	}
	bin, ok := parent.(gogst.Bin)
	if !ok {
		return nil
	}
	for _, n := range names {
		if e := bin.GetByName(n); e != nil {
			return e
		}
	}
	return nil
}

// stringPropertyOrEmpty reads a G_TYPE_STRING property, or returns "" for a
// property that does not exist or is not a string.
//
// It is deliberately silent where setTypedProperty is loud. Setting a property
// that is not what we think it is puts the WRONG DEVICE on air and must fail
// the pipeline; reading one that is not there is the ordinary case for every
// element in this graph that is not a DeckLink, and a diagnosis is worth less
// than nothing if gathering it can itself fail.
func stringPropertyOrEmpty(obj gogst.Element, name string) string {
	if !hasProperty(obj, name) {
		return ""
	}
	s, _ := obj.ObjectProperty(name).(string)
	return strings.TrimSpace(s)
}

// boolPropertyTriState reads a gboolean property into a triState, so that
// "false" and "there was nothing to ask" stay distinguishable. The distinction
// is the whole reason triState exists: one of them means the card has no signal
// and the other means this is not a card.
func boolPropertyTriState(obj gogst.Element, name string) triState {
	if !hasProperty(obj, name) {
		return triUnknown
	}
	b, ok := obj.ObjectProperty(name).(bool)
	if !ok {
		return triUnknown
	}
	if b {
		return triTrue
	}
	return triFalse
}
