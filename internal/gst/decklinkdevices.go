// decklinkdevices.go is the DEVICE KIND seam: the part of the device layer that
// knows the commentary input dropdown offers more than one KIND of capture
// device, what each kind's stable identity is, and how the two are kept from
// ever being mistaken for one another.
//
// Owner: WP-3a, with deviceprovider_darwin.go and deviceprovider_windows.go.
// The half of it that has to touch GStreamer is decklinkdevices_cgo.go; this
// file is the vocabulary, the identifier arithmetic and the rules, and it
// carries no build tag for the same reason device_id.go carries none — the kind
// discriminator crosses the Wails boundary, reaches the frontend dropdown, is
// set on the stub build's fake devices and is asserted at Gate A, so every build
// has to be able to compile it.
//
// # The defect this file exists to fix, measured
//
// A Blackmagic UltraStudio 4K Mini presents itself to a Mac TWICE, through two
// different drivers, and gst-device-monitor-1.0 Audio/Source shows both:
//
//	name       "UltraStudio 4K Mini (Audio Capture)"   class Audio/Source/Hardware
//	published  device-number=0 (guint), model-name, display-name,
//	           max-channels=16, supports-format-detection, supports-hdr,
//	           supports-colorspace, serial-number="", persistent-id=2747401380
//	           (gint64). NO unique-id.
//
//	name       "Blackmagic UltraStudio 4K Mini"        class Audio/Source
//	published  is-default=false, unique-id="90:a3c204a4:00000000:Audio"
//
// The first is the DeckLink device provider's entry and carries the audio
// actually embedded on the card's SDI/HDMI input. The second is Blackmagic's
// CoreAudio driver publishing the same card to the system audio stack, and it
// was MEASURED at -96 dBFS on all sixteen channels with a microphone live in
// front of a speaker: it is a device that exists, enumerates, opens, and carries
// nothing.
//
// captureDeviceID skipped any device publishing no unique-id. The DeckLink entry
// publishes none, so the dropdown offered the silent CoreAudio entry and hid the
// one carrying the operator's microphone. That is the whole of the owner's
// original bug, and it is why the fix is an enumeration change rather than a
// pipeline change.
//
// The two ids are the SAME CARD, and it is worth knowing that they are, because
// it is the strongest argument against folding them together: 0xa3c204a4 is
// 2747401380, so the CoreAudio unique-id embeds the DeckLink persistent-id in
// hex. A de-duplication clever enough to notice that would keep one entry and
// discard the other, and the one it discarded would be the one with the audio
// on it. They are deliberately offered as two entries of two kinds.
//
// # Two kinds of identifier get two fields, never one field with a prefix
//
// CONTRACT.md's rule about headphoneDeviceId and headphoneEndpointId is the
// precedent and it is directly on point: a browser mediaDeviceId and an
// operating-system device identity "are different kinds of identifier and must
// never be merged. Using one where the other belongs fails silently in both
// directions." The same is true here. A CoreAudio unique-id means something to
// osxaudiosrc and nothing to decklinkaudiosrc; a DeckLink persistent-id means
// something to decklinkaudiosrc and nothing to osxaudiosrc; and neither element
// reports a stranger's id as a stranger's id — osxaudiosrc falls back to the
// system default input, which is the silent wrong-device failure
// deviceprovider_darwin.go exists to prevent.
//
// So the discriminator is a SEPARATE FIELD, Device.Kind, and NOT a prefix inside
// Device.ID. "decklink:2747401380" is forbidden, and not as a matter of taste:
// app.go's ListInputDevices doc comment states that Device.ID is an opaque
// string above internal/gst and that "nothing here, in the frontend, or in
// config may parse it, pattern-match it or assume a shape". A prefixed id is a
// shape, and the first caller to split on the colon is the first caller to break
// when a future id contains one. Device.ID stays exactly what the operating
// system said, byte for byte, on both kinds.
//
// # What is given up by doing it this way
//
// Device.Kind is NOT PERSISTED, and that is a deliberate limitation rather than
// an oversight. config.json keeps the id alone, exactly as it does today, so an
// existing config file is still valid and no field has to be classified in
// internal/presets/fields.go. The cost is that Start cannot know from config
// alone which element to build; it has to learn the kind from the device itself.
// That is the same shape as the macOS unique-id resolution, which already
// re-enumerates at pipeline-open time, and it is strictly safer than trusting a
// saved kind that could disagree with the hardware actually present — a saved
// kind and a saved id can rot apart, whereas a device asked what it is cannot.
package gst

import (
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
)

// DeviceKind, KindNative and KindDeckLink are declared in gst.go, beside the
// Device field that carries them — KindNative is not a DeckLink concept, and
// the frontend pins both spellings by reading that file's source text. The
// DeckLink RULES stay here: what a persistent-id is, why a device-number is
// never persisted, and how the two providers are told apart.

// ErrWrongDeviceKind is wrapped by every refusal to hand one kind's identifier
// to the other kind's element. It exists for the same reason
// ErrNotACaptureDevice does: internal/sender and the App layer need to tell
// "the operator's selection cannot be opened by this element" apart from a
// connection failure, because the misdiagnosis costs a retry loop that blames
// the network for a local device fault.
var ErrWrongDeviceKind = errors.New("gst: device is of a kind this element cannot open")

// NormaliseDeviceKind maps the empty string to KindNative and passes
// everything else through unchanged.
//
// The empty string is not a hypothetical. It is what a Device decoded from a
// config file or a Wails frame written BEFORE Kind existed carries, and it is
// what the stub build's fake device tables carry until they are updated. Reading
// it as native is not a guess: at the time such a value was written the only
// devices this application could enumerate were the platform's own, so "no kind
// recorded" and "native" name the same set exactly.
//
// It deliberately does not validate. An unrecognised kind is passed through so
// that the place which cannot handle it refuses it BY NAME — see
// configureCaptureSource — rather than having it silently rewritten to native
// here, which would send a future kind's identifier to osxaudiosrc and put us
// straight back into the silent wrong-device failure.
func NormaliseDeviceKind(k DeviceKind) DeviceKind {
	if k == "" {
		return KindNative
	}
	return k
}

// propPersistentID is the GstStructure key under which GStreamer's decklink
// device provider publishes a card's persistent id, and it is the ONE property
// this application persists for a DeckLink device.
//
// Measured on the port machine (Darwin 25.3.0, GStreamer 1.26.10) with a real
// UltraStudio 4K Mini: type gint64, value 2747401380, published by BOTH the
// Audio/Source and the Video/Source entry for the card with the identical value.
// That is the point of it — it names the CARD, not the stream — and it is what
// makes it possible to open the audio and the video of one card knowing a single
// saved number, which decklinkaudiosrc requires because DeckLink drives audio
// capture off the video clock and cannot preroll audio alone.
//
// The element's own property of the same name is an Integer64 with range
// -1..G_MAXINT64 and default -1, documented as "Output device instance to use.
// Higher priority than device-number". Reading and writing the same name on the
// device structure and on the element is what keeps the round trip honest.
const propPersistentID = "persistent-id"

// propDeviceNumber is the decklink provider's OTHER identifier, published as a
// guint and measured as 0 on the port machine. It is read only to be named in
// diagnostics, and it must NEVER be what gets persisted.
//
// It is an enumeration index. Two cards in one machine are 0 and 1 in whatever
// order the driver enumerated them, so adding a second card, moving one between
// Thunderbolt ports, or a driver update that changes probe order renumbers a
// card the operator never touched. The saved 0 then names the other card and the
// pipeline starts happily on it — the identical failure mode as storing a
// CoreAudio AudioDeviceID instead of its unique-id, and it is refused here for
// the identical reason. persistent-id is derived from the hardware and is what
// gst-device-monitor's own suggested launch line uses:
//
//	gst-launch-1.0 decklinkaudiosrc persistent-id=2747401380 ! ...
const propDeviceNumber = "device-number"

// propModelName is the decklink provider's human name for the card ("UltraStudio
// 4K Mini"), used only in diagnostics. The device's display-name is the string
// the dropdown shows; this is the one that stays the same when Blackmagic's
// naming does not.
const propModelName = "model-name"

// unsetPersistentID is the decklink elements' default persistent-id, meaning
// "not set — fall back to device-number". A device publishing it would be a
// provider bug, but it is refused explicitly rather than formatted into an id,
// because persisting "-1" would produce a saved selection that silently reopens
// as whatever device-number 0 happens to be.
const unsetPersistentID int64 = -1

// formatDeckLinkPersistentID renders a card's persistent-id as the string that
// goes in Device.ID and in config.json.
//
// Plain base-10, no prefix, no padding, no hex. That is not an arbitrary choice
// of encoding: it is the exact spelling gst-device-monitor-1.0 prints in its
// suggested launch line and the exact spelling decklinkaudiosrc's persistent-id
// property parses from a gst-launch command line, so an id copied out of a log
// into a terminal works, and one copied the other way round works too. A hex or
// 0x-prefixed form would round-trip through this package perfectly and match
// nothing anybody else prints.
func formatDeckLinkPersistentID(id int64) string {
	return strconv.FormatInt(id, 10)
}

// parseDeckLinkPersistentID turns a persisted DeckLink id back into the gint64
// the element's property wants.
//
// This is a CONVERSION, not a classification, and the distinction matters
// because app.go forbids parsing Device.ID. That rule binds everything ABOVE
// internal/gst, and what it forbids is inferring meaning from an id's shape —
// deciding what kind of device it is, or which platform it came from, by looking
// at the characters. This function infers nothing: the caller already knows it
// holds a DeckLink id, because it is talking to an element that has a
// persistent-id property, and all that is left is to turn the stored decimal
// back into the integer type the property is declared as. It is the same job as
// deviceprovider_darwin.go's resolution from a CoreAudio unique-id to an
// AudioDeviceID, minus the instability.
//
// A failure here is a hard error rather than a fallback, for the reason the
// whole device layer gives: decklinkaudiosrc's default persistent-id is -1,
// which means "use device-number instead", which means device-number 0, which
// means whichever card the driver enumerated first. Quietly opening that would
// be a wrong-card commentary feed with every lamp green.
func parseDeckLinkPersistentID(id string) (int64, error) {
	v, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("gst: %q is not a DeckLink persistent-id: it must be the decimal "+
			"gint64 the decklink device provider published and gst-device-monitor prints "+
			"(for example 2747401380): %w", id, err)
	}
	if v < 0 {
		// -1 is the property's own "unset" default and nothing below it is
		// meaningful. Setting it would be indistinguishable from not setting it.
		return 0, fmt.Errorf("gst: DeckLink persistent-id %d is negative; %d is the element's "+
			"\"unset\" default, which falls back to %s and therefore to whichever card the driver "+
			"enumerated first", v, unsetPersistentID, propDeviceNumber)
	}
	return v, nil
}

// deviceDedupKey is the key ListInputDevices de-duplicates the offered list on.
//
// It is scoped by KIND rather than being the id alone. The two id spaces are
// disjoint in every value measured so far — a CoreAudio unique-id is a symbolic
// name, a UUID or a colon-separated token, a WASAPI endpoint id is a braced GUID
// pair, and a DeckLink persistent-id is decimal digits — but "disjoint so far"
// is not a property either operating system promises, and the cost of being
// wrong is the specific failure this whole file exists to fix: one entry folded
// into another and the audible one discarded. Scoping the key makes a collision
// between kinds impossible to express rather than unlikely.
//
// The NUL separator cannot occur in either id — both come from C strings — so
// there is no pair of distinct (kind, id) tuples that can produce the same key.
func deviceDedupKey(kind DeviceKind, id string) string {
	return string(NormaliseDeviceKind(kind)) + "\x00" + id
}

// deckLinkDevicesOffered counts the DeckLink cards offered in the enumeration
// currently in progress. It is DIAGNOSTIC ONLY and nothing branches on it.
//
// atomic for the reason loopbacksSkipped is atomic — ListInputDevices is
// reachable from the Wails binding and the LAN control bridge at once and
// neither serialises against the other — and with the same accepted imprecision:
// two overlapping enumerations can attribute one's cards to the other's summary
// line. A mutex held across a device enumeration to make a log line prettier is
// not a trade worth making.
var deckLinkDevicesOffered atomic.Int64

// resetDeckLinkCount starts a fresh count. Both twins' resetSkipDetail call it,
// so the summary describes THIS enumeration rather than every one since the
// process started.
func resetDeckLinkCount() { deckLinkDevicesOffered.Store(0) }

// deckLinkSkipDetail is the DeckLink tail on the enumeration summary line, and
// it returns the EMPTY STRING when no card was offered.
//
// That silence is a requirement rather than an economy. A machine with no
// Blackmagic hardware, or with the hardware but without the Desktop Video
// driver, must enumerate exactly as it did before this file existed — the plugin
// does not register at all without the driver, so there is no factory, no
// provider and no device, and there is nothing there for an operator to
// diagnose. A line reading "0 DeckLink cards" on every enumeration on every
// machine that has never had one would be a permanent invitation to go looking
// for hardware that was never fitted.
//
// The Windows loopback count is deliberately NOT written this way — it prints
// even at zero, because zero loopbacks on Windows is itself a surprise worth
// seeing. Nothing is surprising about a Mac with no capture card in it.
func deckLinkSkipDetail() string {
	n := deckLinkDevicesOffered.Load()
	if n == 0 {
		return ""
	}
	if n == 1 {
		return ", and 1 of them a DeckLink card"
	}
	return fmt.Sprintf(", and %d of them DeckLink cards", n)
}
