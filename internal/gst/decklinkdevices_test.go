// decklinkdevices_test.go covers the DEVICE KIND seam at Gate A: the kind
// vocabulary, the identifier arithmetic for a DeckLink card, the de-duplication
// key and the no-hardware path.
//
// No build tag, matching decklinkdevices.go and for the same reason — everything
// tested here has to compile in every build, including the CGO_ENABLED=0 one the
// contract's Gate A runs.
//
// # What this file can and cannot prove
//
// It cannot enumerate. Reading a persistent-id off a GstDevice needs a live
// registry and a card, so the classification test here is over the ARITHMETIC —
// the id a measured card produces, what it round-trips to, and what it must
// never look like — with the measured values written down as constants rather
// than probed. deckLinkCaptureDeviceID itself is exercised at Gate B and by hand
// against the real UltraStudio 4K Mini; the values it is fed are pinned here.
//
// What it CAN prove, and what nothing else does, is the no-hardware path. That
// is the requirement most likely to be broken by a well-meaning change — a
// summary line made more informative, a check made more explicit — and the
// machine it breaks on is not the machine anybody is developing on.
package gst

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// measuredPersistentID and measuredPersistentIDText are the real card, read off
// the port machine with gst-device-monitor-1.0 Audio/Source:
//
//	name          "UltraStudio 4K Mini (Audio Capture)"
//	class         Audio/Source/Hardware
//	persistent-id 2747401380   (gint64)
//	device-number 0            (guint)
//
// The pair is the regression fixture for the whole change. If the id this
// application persists for that card ever stops being this string, an operator's
// saved selection stops naming their card.
const (
	measuredPersistentID     int64 = 2747401380
	measuredPersistentIDText       = "2747401380"

	// measuredDeviceNumber is the OTHER number the same device published, and it
	// is here to be tested AGAINST. It is an enumeration index: fitting a second
	// card, moving this one between Thunderbolt ports or a driver update that
	// changes probe order renumbers it, and a saved 0 then names a different
	// card while the pipeline starts happily on it.
	measuredDeviceNumber int64 = 0
)

// TestDeckLinkPersistentIDRoundTrips is the round trip the operator's config
// file depends on: the id formatted out of an enumerated card must parse back to
// the same gint64 the element property will be set to.
//
// The exact TEXT is asserted, not just the round trip, because the text is a
// published interface in two directions — it is what gst-device-monitor prints
// in its suggested launch line and what decklinkaudiosrc parses off a gst-launch
// command line, so an id copied out of a log into a terminal has to work.
func TestDeckLinkPersistentIDRoundTrips(t *testing.T) {
	got := formatDeckLinkPersistentID(measuredPersistentID)
	if got != measuredPersistentIDText {
		t.Fatalf("formatDeckLinkPersistentID(%d) = %q, want %q — every saved selection naming this "+
			"card has just stopped naming it", measuredPersistentID, got, measuredPersistentIDText)
	}
	back, err := parseDeckLinkPersistentID(got)
	if err != nil {
		t.Fatalf("parseDeckLinkPersistentID(%q) = %v; the id this package writes is one it cannot "+
			"read", got, err)
	}
	if back != measuredPersistentID {
		t.Errorf("round trip changed the id: %d -> %q -> %d", measuredPersistentID, got, back)
	}
}

// TestDeckLinkPersistentIDIsNotDeviceNumber pins the choice of WHICH of the two
// numbers the provider publishes gets persisted.
//
// The two are trivially swappable in the source — both are integers on the same
// device structure, both address the same card on the machine they were read on,
// and a build that used device-number would pass every test anyone would think
// to write on a machine with one card in it. The failure arrives at a facility
// with two, or after a driver update, and it is the same silent wrong-device
// failure as storing a CoreAudio AudioDeviceID: the pipeline starts, the lamps
// go green, and the commentary is coming off the other card.
func TestDeckLinkPersistentIDIsNotDeviceNumber(t *testing.T) {
	if propPersistentID != "persistent-id" {
		t.Errorf("propPersistentID = %q, want %q — this is the property name on the device "+
			"structure AND on the element, and the round trip needs both", propPersistentID,
			"persistent-id")
	}
	if propDeviceNumber != "device-number" {
		t.Errorf("propDeviceNumber = %q, want %q", propDeviceNumber, "device-number")
	}
	if propPersistentID == propDeviceNumber {
		t.Fatal("the two DeckLink identifier properties have become the same name")
	}
	if formatDeckLinkPersistentID(measuredPersistentID) == formatDeckLinkPersistentID(measuredDeviceNumber) {
		t.Fatalf("the measured card's persistent-id and device-number format identically (%q); "+
			"this test can no longer tell which one is being persisted",
			formatDeckLinkPersistentID(measuredDeviceNumber))
	}
}

// TestUnsetPersistentIDIsRefused covers the value the element itself defaults
// to. -1 means "not set, fall back to device-number", so a persisted -1 is a
// saved selection that silently reopens as whichever card enumerated first.
// Formatting it would produce an id that looks perfectly ordinary in
// config.json.
func TestUnsetPersistentIDIsRefused(t *testing.T) {
	if unsetPersistentID != -1 {
		t.Fatalf("unsetPersistentID = %d, want -1: that is decklinkaudiosrc's documented default "+
			"for the persistent-id property", unsetPersistentID)
	}
	for _, id := range []string{"-1", "-2", "-9223372036854775808"} {
		if _, err := parseDeckLinkPersistentID(id); err == nil {
			t.Errorf("parseDeckLinkPersistentID(%q) succeeded; %q would set the property to its "+
				"own \"unset\" value or below, which falls back to %s", id, id, propDeviceNumber)
		}
	}
}

// TestParseDeckLinkPersistentIDRejectsNonIntegers checks that the conversion
// fails loudly rather than yielding a zero.
//
// Zero is the trap. strconv's error is easy to drop, and a dropped one hands the
// element persistent-id=0 — a plausible card id, and on a single-card machine
// very possibly THE card, which is how a bug like this survives every test and
// then picks the wrong card at the one facility with two.
func TestParseDeckLinkPersistentIDRejectsNonIntegers(t *testing.T) {
	for _, id := range []string{
		"",
		"BuiltInMicrophoneDevice",    // a CoreAudio unique-id
		"90:a3c204a4:00000000:Audio", // the same card's CoreAudio unique-id
		"{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}", // a WASAPI endpoint id
		"0x a3c204a4",
		"2747401380 ",
		"27474013800000000000000", // past gint64
	} {
		v, err := parseDeckLinkPersistentID(id)
		if err == nil {
			t.Errorf("parseDeckLinkPersistentID(%q) = %d, want an error", id, v)
			continue
		}
		if v != 0 {
			t.Errorf("parseDeckLinkPersistentID(%q) returned %d beside its error; a caller that "+
				"drops the error must not get a usable-looking card id", id, v)
		}
		if !strings.Contains(err.Error(), id) && id != "" {
			t.Errorf("parseDeckLinkPersistentID(%q) does not quote the offending value: %v", id, err)
		}
	}
}

// TestDeviceKindValues pins the two kind strings by exact value, because they
// cross the Wails boundary as JSON and the frontend compares against them. A
// rename here is a silent frontend change: an unknown kind renders as no badge
// at all, which puts the dropdown back to two similarly named entries the
// operator cannot tell apart — the confusion this whole change exists to remove.
func TestDeviceKindValues(t *testing.T) {
	if KindNative != "native" {
		t.Errorf("KindNative = %q, want %q", KindNative, "native")
	}
	if KindDeckLink != "decklink" {
		t.Errorf("KindDeckLink = %q, want %q", KindDeckLink, "decklink")
	}
	if KindNative == KindDeckLink {
		t.Fatal("the two kinds have the same value; the discriminator discriminates nothing")
	}
}

// TestNormaliseDeviceKindReadsSilenceAsNative covers the upgrade case. A Device
// decoded from anything written before Kind existed carries the empty string,
// and reading it as native is exact rather than a guess: at the time such a
// value was written, native devices were the only ones this application could
// enumerate.
//
// The second half matters more. An UNRECOGNISED kind must pass through
// unchanged, so that whatever cannot handle it refuses it by name. Rewriting it
// to native here would send a future kind's identifier to osxaudiosrc, and
// osxaudiosrc given an id it does not know falls back to the system default
// input — the silent wrong-device failure the whole device layer is built
// around.
func TestNormaliseDeviceKindReadsSilenceAsNative(t *testing.T) {
	if got := NormaliseDeviceKind(""); got != KindNative {
		t.Errorf("NormaliseDeviceKind(\"\") = %q, want %q: a config or Wails frame written before "+
			"Kind existed would stop naming a device it named perfectly well", got, KindNative)
	}
	for _, k := range []DeviceKind{KindNative, KindDeckLink, "ndi", "SomeFutureKind"} {
		if got := NormaliseDeviceKind(k); got != k {
			t.Errorf("NormaliseDeviceKind(%q) = %q; an unrecognised kind must survive so that the "+
				"code which cannot open it can say so by name", k, got)
		}
	}
}

// TestDeviceDedupKeySeparatesKinds is the guard on ListInputDevices' de-dup map.
//
// The failure it prevents is precisely the original bug wearing a different hat:
// two entries for one card, one of them audible, and a de-duplication that keeps
// the wrong one. The id spaces are disjoint in every value measured so far, so
// this cannot fire today — which is exactly why it is worth pinning, because
// "cannot fire today" is what stops somebody simplifying the key back to the
// bare id.
func TestDeviceDedupKeySeparatesKinds(t *testing.T) {
	const shared = "2747401380"
	if deviceDedupKey(KindNative, shared) == deviceDedupKey(KindDeckLink, shared) {
		t.Fatal("two kinds sharing one id collapse to one dropdown entry; the entry that gets " +
			"discarded is the one that carries the audio")
	}
	// Distinct ids of one kind must still collide only with themselves, or the
	// de-dup stops de-duplicating and the two default-device pseudo entries come
	// back.
	if deviceDedupKey(KindNative, "a") == deviceDedupKey(KindNative, "b") {
		t.Error("distinct ids of one kind produce the same key")
	}
	// The empty kind is normalised, so a Device that predates Kind de-duplicates
	// against a freshly enumerated native one rather than appearing twice.
	if deviceDedupKey("", shared) != deviceDedupKey(KindNative, shared) {
		t.Error("an unset kind does not de-duplicate against native; an upgraded config would " +
			"show the operator's microphone twice")
	}
}

// TestNoDeckLinkHardwareIsSilent is the requirement that a machine with no
// Blackmagic card — which is every machine this product has ever shipped to —
// logs exactly what it logged before DeckLink support existed.
//
// It is not a cosmetic requirement. The enumeration summary is the line an
// operator is asked to read out when the dropdown is wrong, and a permanent "0
// DeckLink cards" on a laptop that has never had one sends them looking for a
// driver they do not need. The plugin does not even register without the Desktop
// Video driver, so on such a machine there is no provider, no factory and no
// device: nothing here should have an opinion.
func TestNoDeckLinkHardwareIsSilent(t *testing.T) {
	resetDeckLinkCount()
	if got := deckLinkSkipDetail(); got != "" {
		t.Fatalf("deckLinkSkipDetail() = %q with no card enumerated, want \"\": every machine "+
			"without Blackmagic hardware has gained a log line about hardware it does not have", got)
	}
}

// TestDeckLinkCountAppearsOnlyWhenThereIsHardware is the other half: when a card
// IS present the summary must say so, in a phrase that reads as English at one
// card and at several, and that names the number.
func TestDeckLinkCountAppearsOnlyWhenThereIsHardware(t *testing.T) {
	resetDeckLinkCount()
	deckLinkDevicesOffered.Add(1)
	one := deckLinkSkipDetail()
	if !strings.Contains(one, "1") || !strings.Contains(one, "DeckLink") {
		t.Errorf("deckLinkSkipDetail() = %q with one card; it must name the count and the kind", one)
	}
	if strings.Contains(one, "cards") {
		t.Errorf("deckLinkSkipDetail() = %q: one card is not \"cards\"", one)
	}

	deckLinkDevicesOffered.Add(2)
	three := deckLinkSkipDetail()
	if !strings.Contains(three, "3") || !strings.Contains(three, "DeckLink cards") {
		t.Errorf("deckLinkSkipDetail() = %q with three cards", three)
	}

	// And the reset must actually reset, or the count grows every time the
	// operator reopens the Settings screen and reads as cards multiplying.
	resetDeckLinkCount()
	if got := deckLinkSkipDetail(); got != "" {
		t.Errorf("deckLinkSkipDetail() = %q after resetDeckLinkCount()", got)
	}
}

// TestWrongKindErrorIsMatchable pins the sentinel rather than the wording. The
// App layer and internal/sender need to tell "the operator's saved device is the
// wrong KIND for this element" apart from a connection failure, because the two
// ask completely different things of the person watching: one is "pick a
// different device", the other is "the network is down". Getting that wrong is
// the measured defect device_id.go records, where the sender retried an SRT link
// forever over a local device fault.
func TestWrongKindErrorIsMatchable(t *testing.T) {
	if ErrWrongDeviceKind == nil {
		t.Fatal("ErrWrongDeviceKind is nil")
	}
	if errors.Is(ErrWrongDeviceKind, ErrNotACaptureDevice) {
		t.Error("ErrWrongDeviceKind matches ErrNotACaptureDevice; a capture card would be reported " +
			"to the operator as a playback endpoint")
	}
	// The formatted refusal must survive wrapping, because that is how every
	// caller will meet it.
	wrapped := errors.Join(ErrWrongDeviceKind, errors.New("context"))
	if !errors.Is(wrapped, ErrWrongDeviceKind) {
		t.Error("ErrWrongDeviceKind does not survive wrapping")
	}
}

// TestMeasuredIDFitsAnInt64AndNotAnInt32 records why the element property is
// read and written as a gint64 throughout, and why setInt64Property exists
// beside setIntProperty.
//
// The measured card's id is already past the top of a gint. A gint setter on
// this path would fail the type check loudly rather than silently — that is what
// setTypedProperty is for — but it would fail only on hardware, on a machine
// nobody is testing on, twenty minutes before kick-off.
func TestMeasuredIDFitsAnInt64AndNotAnInt32(t *testing.T) {
	if measuredPersistentID <= (1<<31)-1 {
		t.Fatalf("the measured persistent-id %d now fits in a gint; this test's premise has "+
			"changed and the int64 property path should be re-justified rather than kept out of "+
			"habit", measuredPersistentID)
	}
	// And it must survive the string form without loss, which is the form
	// config.json holds it in.
	if _, err := strconv.ParseInt(measuredPersistentIDText, 10, 64); err != nil {
		t.Fatalf("the measured id text does not parse as a gint64: %v", err)
	}
}
