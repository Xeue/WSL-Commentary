//go:build cgo && !gststub

// decklinkdevices_cgo.go is the GStreamer-facing twin of decklinkdevices.go:
// everything about DeckLink capture cards that has to touch a GstDevice, a
// GstStructure or an element property, and therefore cannot be compiled at
// Gate A.
//
// Owner: WP-3a. Read decklinkdevices.go first — it holds the measurements, the
// rule about two kinds of identifier never sharing one field, and the reason
// Device.Kind is not persisted. This file holds only the three operations that
// need a live registry:
//
//	deckLinkCaptureDeviceID  is this enumerated device a DeckLink card, and if
//	                         so what is its persistable id?
//	configureDeckLinkSource  point a decklink element at that card.
//	deckLinkCardWithID       does any DeckLink card currently claim this id?
//	                         Diagnostics only, and only on a failure path.
//
// # There is no build tag for the platform, and no check that the plugin exists
//
// One file serves Windows and macOS. The decklink plugin, the device provider,
// the property names and the persistent-id semantics are the same code on both
// — gstdecklink is one source tree with a per-platform capture backend under it
// — so a Windows twin of this file would be a byte-for-byte copy waiting to
// drift. The two DEVICE PROVIDER files still differ, because what a NATIVE
// device is differs; what a DeckLink card is does not.
//
// Nothing here asks whether the decklink plugin is loaded, whether the element
// factory exists, or whether the Blackmagic Desktop Video driver is installed.
// That is deliberate and it is the whole of the no-hardware story. Without the
// driver the plugin fails to register — silently, by design, because a machine
// with no card has no use for it — so there is no provider, the monitor
// enumerates no DeckLink devices, deckLinkCaptureDeviceID is never reached with
// one, and ListInputDevices returns precisely the list it returned before this
// file existed, with not one extra log line. An explicit
// ElementFactoryFind("decklinkaudiosrc") check could only produce a message
// about hardware that was never fitted. "No such devices" is not an error
// condition, it is Tuesday.
package gst

import (
	"fmt"
	"log"

	gobject "github.com/go-gst/go-glib/pkg/gobject/v2"
	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// deckLinkCaptureDeviceID reports whether one enumerated Audio/Source device is
// a Blackmagic capture card, and under what persistable id.
//
// Both twins' captureDeviceID ask this FIRST, before their own native tests,
// and the order is not arbitrary. On Windows the native test is a hard skip —
// device.api != "wasapi2" returns immediately and logs — so a DeckLink card
// asked second would never be reached. Asking it first on macOS too keeps the
// two files answering the same questions in the same order, which is the only
// thing that stops them drifting into two different enumeration policies.
//
// Asking first is SAFE because the two discriminators are disjoint by
// construction, not by luck: osxaudiodeviceprovider publishes exactly two
// properties, is-default and unique-id, and wasapi2's publish device.api — and
// measurement on the port machine confirms no native device publishes
// persistent-id and no DeckLink device publishes unique-id.
//
// The silent return is the important one. A device that is not a DeckLink card
// falls out of here having logged NOTHING, because on the overwhelming majority
// of machines that is every device on every enumeration and this function's
// opinion about them is of no interest to anybody. Only a device that IS a
// DeckLink card and is nevertheless unusable gets a line.
func deckLinkCaptureDeviceID(dev gogst.Device, props *gogst.Structure) (string, bool) {
	if !props.HasField(propPersistentID) {
		return "", false
	}

	// GetInt64 fails unless the field exists AND is G_TYPE_INT64, which is how
	// the provider publishes it — measured, with a real UltraStudio 4K Mini:
	// persistent-id is a gint64 of 2747401380, while device-number and
	// max-channels beside it are guints. A card that reaches here and fails this
	// is a card whose provider has changed the type under us, and it is worth a
	// line: the alternative is a device that silently stops being offered and is
	// indistinguishable from one that has been unplugged.
	id, ok := props.GetInt64(propPersistentID)
	if !ok {
		log.Printf("gst: ListInputDevices: skipping %q: it publishes %s but not as the gint64 the "+
			"decklink device provider is measured to publish, so there is nothing safe to persist; "+
			"device properties are %s",
			dev.GetDisplayName(), propPersistentID, structureFieldNames(props))
		return "", false
	}
	if id <= unsetPersistentID {
		// The element's own "unset" value, meaning "fall back to device-number".
		// Persisting it would save a selection that reopens as whichever card
		// the driver happened to enumerate first, which is the enumeration-index
		// failure propDeviceNumber's comment sets out at length.
		log.Printf("gst: ListInputDevices: skipping %q (%s): its %s is %d, which is the element's "+
			"\"unset\" default and would fall back to %s=%d — a saved selection that names whichever "+
			"card enumerated first rather than this one",
			dev.GetDisplayName(), props.GetString(propModelName), propPersistentID, id,
			propDeviceNumber, deckLinkDeviceNumber(props))
		return "", false
	}

	deckLinkDevicesOffered.Add(1)
	return formatDeckLinkPersistentID(id), true
}

// deckLinkDeviceNumber reads the provider's enumeration index, for diagnostics
// only. It is a guint on the device structure — measured as 0 on the port
// machine — and it is NEVER what gets persisted; see propDeviceNumber.
//
// -1 on failure rather than 0, because 0 is a real device number and a
// diagnostic that cannot say "I could not read it" is a diagnostic that will
// one day accuse the wrong card.
func deckLinkDeviceNumber(props *gogst.Structure) int64 {
	if n, ok := props.GetUint(propDeviceNumber); ok {
		return int64(n)
	}
	return -1
}

// configureDeckLinkSource points a decklink element at the card the operator
// chose.
//
// deviceID is the persistent-id that came out of config.json, as decimal digits.
// Unlike the macOS CoreAudio path there is nothing to RESOLVE: a persistent-id
// is derived from the hardware and names the same card after a reboot, a replug
// and a driver update, which is exactly why it rather than device-number is the
// thing stored. All that happens here is a conversion back to the gint64 the
// property is declared as.
//
// This is written for the audio source and the video source alike, and takes a
// gogst.Element rather than naming either, because decklinkaudiosrc cannot
// preroll without a decklinkvideosrc for the same card in the same pipeline —
// DeckLink drives audio capture off the video clock — so the caller that opens
// one will always be opening two, and both need the identical property set from
// the identical saved string. The card publishes ONE persistent-id for both
// entries (measured: the Audio/Source and Video/Source devices carry the same
// 2747401380), which is what makes a single saved id sufficient.
func configureDeckLinkSource(src gogst.Element, deviceID string) error {
	id, err := parseDeckLinkPersistentID(deviceID)
	if err != nil {
		return err
	}
	// setInt64Property, not setIntProperty: 2747401380 does not fit in a gint,
	// and the measured card's id is already past it. A gint setter here would
	// fail the type check loudly, which is better than the alternative but is
	// still a bug that would only appear on hardware.
	if err := setInt64Property(src, propPersistentID, id); err != nil {
		return err
	}
	log.Printf("gst: Start: DeckLink card %s selected by %s on %s", deviceID, propPersistentID,
		src.GetName())
	return nil
}

// setInt64Property sets a G_TYPE_INT64 property, failing loudly if the element
// does not have it or if it is not a gint64.
//
// int64 rather than int in the signature for the reason setIntProperty gives
// about int32: go-glib maps a Go int64 to G_TYPE_INT64 and has no mapping for a
// plain int, so a plain int would arrive as G_TYPE_INVALID and the property
// would keep its default — which on persistent-id is -1, "use device-number
// instead", and therefore whichever card enumerated first.
func setInt64Property(obj gobject.Object, name string, value int64) error {
	return setTypedProperty(obj, name, gobject.TypeInt64, value)
}

// deckLinkCardWithID reports whether a DeckLink card currently claims id, and
// under what name. It is DIAGNOSTIC ONLY.
//
// It exists for one message. Both twins' configureCaptureSource now sit behind a
// dropdown that offers two kinds of device, so the id they are handed can be a
// DeckLink persistent-id that the platform's native element has never heard of.
// Without this the operator gets "device 2747401380 is no longer present,
// CoreAudio currently offers: [...]" — true, unhelpful, and pointing at exactly
// the wrong thing, because the device is present, plugged in and working.
//
// It costs one device enumeration and is therefore called ONLY on paths that
// have already established something is wrong: never on a successful open, and
// never for an id the native platform recognises. See the call sites.
func deckLinkCardWithID(id string) (string, bool) {
	for _, dev := range enumerateDevices("Audio/Source") {
		if dev == nil {
			continue
		}
		props := dev.GetProperties()
		if props == nil {
			continue
		}
		card, ok := props.GetInt64(propPersistentID)
		if !ok || card <= unsetPersistentID {
			continue
		}
		if formatDeckLinkPersistentID(card) != id {
			continue
		}
		name := props.GetString(propModelName)
		if name == "" {
			name = dev.GetDisplayName()
		}
		return name, true
	}
	return "", false
}

// deckLinkKindError is the refusal both twins return when the operator's saved
// id names a DeckLink card and the element in hand cannot open one.
//
// It names the card, because the operator's complaint will be "but it is right
// there and the light is on", and it says which element is refusing, because
// that is the half of the sentence that explains why a device that plainly
// exists cannot be used by this pipeline. It wraps ErrWrongDeviceKind so the
// App layer can tell it apart from a device that has genuinely gone away — the
// two want completely different things from the operator.
func deckLinkKindError(deviceID, card string) error {
	return fmt.Errorf("gst: the saved capture device %q is the DeckLink card %q, not a %s device. "+
		"%s cannot open a DeckLink card: it is opened by decklinkaudiosrc, which this pipeline did "+
		"not build. Select a device in the commentary input dropdown: %w",
		deviceID, card, KindNative, captureSourceFactory, ErrWrongDeviceKind)
}
