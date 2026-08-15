// device_id_test.go tests the endpoint-id classifier in device_id.go. No build
// tag, deliberately: the classifier is the one piece of the loopback-defect fix
// that both twins, the App pre-flight and the enumeration filter all share, so
// it must be provable at Gate A and must keep compiling in every build.
package gst

import (
	"errors"
	"strings"
	"testing"
)

// operatorRenderID is the ACTUAL id from the field failure: the operator on
// another machine selected a playback endpoint the dropdown should never have
// offered, and wasapi2's rbuf failed asynchronously with "Failed to open
// device {0.0.0.00000000}.{8678ce58-...}" while the sender blamed the SRT
// network and retried forever. It is the regression fixture: if this id ever
// stops classifying as render, the whole defect is back.
const operatorRenderID = "{0.0.0.00000000}.{8678ce58-90c0-4827-8ff7-c9edd8d074ed}"

// captureID is a plausible real capture endpoint, the shape every microphone
// and DVS Receive device carries.
const captureID = "{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}"

func TestCaptureEndpointIsAccepted(t *testing.T) {
	if !IsCaptureEndpointID(captureID) {
		t.Errorf("IsCaptureEndpointID(%q) = false; every real microphone would be refused", captureID)
	}
	if IsRenderEndpointID(captureID) {
		t.Errorf("IsRenderEndpointID(%q) = true; Start would refuse a genuine capture device", captureID)
	}
	if err := refuseRenderEndpoint(captureID); err != nil {
		t.Errorf("refuseRenderEndpoint(%q) = %v, want nil", captureID, err)
	}
}

func TestOperatorsRenderEndpointIsRejected(t *testing.T) {
	if !IsRenderEndpointID(operatorRenderID) {
		t.Errorf("IsRenderEndpointID(%q) = false: the measured field failure — an output selected "+
			"as an input, failing asynchronously and blaming the network — would recur", operatorRenderID)
	}
	if IsCaptureEndpointID(operatorRenderID) {
		t.Errorf("IsCaptureEndpointID(%q) = true; a playback endpoint classifies as capture", operatorRenderID)
	}

	err := refuseRenderEndpoint(operatorRenderID)
	if err == nil {
		t.Fatalf("refuseRenderEndpoint(%q) = nil; the refusal never fires", operatorRenderID)
	}
	if !errors.Is(err, ErrNotACaptureDevice) {
		t.Errorf("the refusal does not wrap ErrNotACaptureDevice: %v", err)
	}
	// The operator diagnosing this has a GUID in config.json and nothing else.
	// The message must quote the offending value and name both namespaces.
	for _, want := range []string{operatorRenderID, CaptureEndpointPrefix, RenderEndpointPrefix} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal message does not contain %q: %v", want, err)
		}
	}
}

// TestDevinterfaceGUIDsClassifyOppositeWays covers the two default-device
// pseudo entries, which are named by DEVINTERFACE class GUIDs rather than by
// endpoint-namespace prefixes. endpointID prefers device.actual-id so these
// normally resolve to real endpoints before classification, but if one leaks
// through it must still land on the right side.
func TestDevinterfaceGUIDsClassifyOppositeWays(t *testing.T) {
	if !IsCaptureEndpointID(DefaultCaptureDeviceID) {
		t.Errorf("IsCaptureEndpointID(DefaultCaptureDeviceID) = false")
	}
	if IsRenderEndpointID(DefaultCaptureDeviceID) {
		t.Errorf("IsRenderEndpointID(DefaultCaptureDeviceID) = true; the default CAPTURE pseudo "+
			"device %q would be refused", DefaultCaptureDeviceID)
	}
	if !IsRenderEndpointID(DefaultRenderDeviceID) {
		t.Errorf("IsRenderEndpointID(DefaultRenderDeviceID) = false; the default RENDER pseudo "+
			"device %q would be opened as a commentary input", DefaultRenderDeviceID)
	}
	if IsCaptureEndpointID(DefaultRenderDeviceID) {
		t.Errorf("IsCaptureEndpointID(DefaultRenderDeviceID) = true")
	}
}

// TestUnrecognisedShapesAreNotRefused pins the DELIBERATE ASYMMETRY. Only a
// POSITIVELY identified render id may be refused; an id of unrecognised shape
// must pass IsRenderEndpointID == false and refuseRenderEndpoint == nil, so
// that a future Windows or GStreamer id-shape change degrades to a logged
// warning rather than an unstartable commentary position. (Such an id fails
// the positive IsCaptureEndpointID too — that is what triggers the caller's
// warning — but failing it must never mean refusal.)
func TestUnrecognisedShapesAreNotRefused(t *testing.T) {
	unknown := []string{
		"",
		"weird-future-id",
		"{0.0.2.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}",    // a data-flow token that does not exist yet
		"\\\\?\\SWD#MMDEVAPI#{9f6d2b18-0004-438e-9003-51a46e13a4d5}", // a path form with neither class GUID
		"default",
	}
	for _, id := range unknown {
		if IsRenderEndpointID(id) {
			t.Errorf("IsRenderEndpointID(%q) = true; an unrecognised shape is being positively "+
				"identified as render", id)
		}
		if err := refuseRenderEndpoint(id); err != nil {
			t.Errorf("refuseRenderEndpoint(%q) = %v: refusing an unrecognised shape turns a future "+
				"id-shape change into an unstartable commentary position", id, err)
		}
		if IsCaptureEndpointID(id) {
			t.Errorf("IsCaptureEndpointID(%q) = true; an unrecognised shape is being positively "+
				"identified as capture, which would silence the caller's warning", id)
		}
	}
}

// TestPrefixConstantsDiffer guards against a copy-paste making the two
// namespaces identical, which would classify everything as both and turn the
// refusal into either "refuse everything" or "refuse nothing" depending on
// check order.
func TestPrefixConstantsDiffer(t *testing.T) {
	if CaptureEndpointPrefix == RenderEndpointPrefix {
		t.Fatalf("CaptureEndpointPrefix == RenderEndpointPrefix == %q", CaptureEndpointPrefix)
	}
	if strings.EqualFold(DefaultCaptureDeviceID, DefaultRenderDeviceID) {
		t.Fatalf("the two DEVINTERFACE GUIDs are the same: %q", DefaultCaptureDeviceID)
	}
}

// TestClassificationIsCaseInsensitive: GUID casing is not stable across the
// APIs that print these ids — MMDevice enumeration reports lowercase hex,
// registry exports and documentation uppercase — and a refusal that only fired
// on one casing would be a refusal that fires on some machines.
func TestClassificationIsCaseInsensitive(t *testing.T) {
	if !IsRenderEndpointID(strings.ToUpper(operatorRenderID)) {
		t.Errorf("IsRenderEndpointID misses the uppercased render id %q", strings.ToUpper(operatorRenderID))
	}
	if !IsCaptureEndpointID(strings.ToUpper(captureID)) {
		t.Errorf("IsCaptureEndpointID misses the uppercased capture id %q", strings.ToUpper(captureID))
	}
	if !IsCaptureEndpointID(strings.ToLower(DefaultCaptureDeviceID)) {
		t.Errorf("IsCaptureEndpointID misses the lowercased DEVINTERFACE_AUDIO_CAPTURE GUID")
	}
	if !IsRenderEndpointID(strings.ToLower(DefaultRenderDeviceID)) {
		t.Errorf("IsRenderEndpointID misses the lowercased DEVINTERFACE_AUDIO_RENDER GUID")
	}
}

// coreAudioIDs are the REAL CoreAudio unique-ids measured with
// gst-device-monitor-1.0 on the macOS port machine, one of each shape the
// provider was seen to produce: a built-in device's symbolic name, an
// aggregate/virtual device's name, and a driver-assigned UUID. The last one is
// deliberately the shape closest to a Windows id, because a UUID with hyphens
// is exactly what a careless prefix or "contains a GUID" test would trip over.
//
// The last entry is the Blackmagic card as its CoreAudio driver publishes it,
// and it earns its place twice over. It is the only measured unique-id
// containing colons, which is what a hand-rolled "kind:id" encoding would have
// collided with. And it is the SILENT device — -96 dBFS on all sixteen channels
// with a microphone live in front of a speaker — that the dropdown offered while
// hiding the DeckLink entry beside it that carries the audio. Its hex middle
// token is the DeckLink persistent-id of the same card: 0xa3c204a4 is
// 2747401380. Nothing reads it that way and nothing may start to.
var coreAudioIDs = []string{
	"BuiltInMicrophoneDevice",
	"BuiltInSpeakerDevice",
	"NDIAudio",
	"BF568F24-731B-41DB-932E-AC7E260BC71A",
	"A33FF27F-E7F1-4055-ABC7-4C0C00000003",
	"90:a3c204a4:00000000:Audio",
}

// TestCoreAudioIDsAreInNeitherWindowsNamespace pins the property that makes it
// SAFE to leave device_id.go untouched by the macOS port: the Windows
// classifiers are inert on CoreAudio ids rather than wrong about them.
//
// Both halves matter and for different reasons.
//
// IsRenderEndpointID must be false, because refuseRenderEndpoint keys on it and
// Start calls that on every platform. A CoreAudio id that classified as render
// would make macOS unstartable with an error about WASAPI playback endpoints —
// a refusal for a reason that does not exist on the machine.
//
// IsCaptureEndpointID must ALSO be false, and that is not a defect: it is the
// asymmetric rule in the file comment working as designed. What follows from it
// is that the "unrecognised shape" warning would fire for every macOS device on
// every enumeration, which is why that warning now lives in the Windows device
// provider rather than in the shared enumeration path.
func TestCoreAudioIDsAreInNeitherWindowsNamespace(t *testing.T) {
	for _, id := range coreAudioIDs {
		if IsRenderEndpointID(id) {
			t.Errorf("IsRenderEndpointID(%q) = true: a macOS device would be refused by "+
				"refuseRenderEndpoint with an error about WASAPI render endpoints, on a machine "+
				"that has no such thing", id)
		}
		if IsCaptureEndpointID(id) {
			t.Errorf("IsCaptureEndpointID(%q) = true: a CoreAudio id is being positively identified "+
				"as a WASAPI capture endpoint, which is a coincidence rather than a fact and would "+
				"silence a warning that is meant to fire", id)
		}
		if err := refuseRenderEndpoint(id); err != nil {
			t.Errorf("refuseRenderEndpoint(%q) = %v, want nil: the macOS pipeline would not start", id, err)
		}
	}
}

// deckLinkIDs are DeckLink persistent-ids as formatDeckLinkPersistentID renders
// them. The first is the REAL one, read off the port machine's UltraStudio 4K
// Mini with gst-device-monitor-1.0; the rest are the boundaries a decimal
// gint64 id can reach, because "digits" is a family with a very wide range and
// a classifier that happened to work on ten characters is not a classifier.
var deckLinkIDs = []string{
	"2747401380",
	"0",
	"1",
	"9223372036854775807",
}

// TestDeckLinkIDsAreInNeitherWindowsNamespace is the DeckLink half of the
// statement TestCoreAudioIDsAreInNeitherWindowsNamespace makes about CoreAudio:
// the Windows classifiers are INERT on the third family of id this application
// now persists, rather than wrong about it.
//
// IsRenderEndpointID matters most, because refuseRenderEndpoint keys on it and
// Start calls that on every platform for every id. A DeckLink id that classified
// as render would make the operator's capture card unselectable with an error
// about WASAPI playback endpoints — a refusal citing a mechanism that has
// nothing to do with the device in front of them.
//
// IsCaptureEndpointID must be false as well, and on Windows that has a visible
// consequence rather than a theoretical one: configureCaptureSource uses exactly
// this test as the cheap gate in front of the "is this actually a DeckLink card"
// enumeration. If a persistent-id ever started classifying as a WASAPI capture
// endpoint, that gate would close, the id would go straight to wasapi2src, and
// the failure would be the asynchronous "Failed to open device 2747401380" the
// gate exists to pre-empt.
func TestDeckLinkIDsAreInNeitherWindowsNamespace(t *testing.T) {
	for _, id := range deckLinkIDs {
		if IsRenderEndpointID(id) {
			t.Errorf("IsRenderEndpointID(%q) = true: a DeckLink capture card would be refused by "+
				"refuseRenderEndpoint as a Windows playback endpoint", id)
		}
		if IsCaptureEndpointID(id) {
			t.Errorf("IsCaptureEndpointID(%q) = true: a DeckLink persistent-id is being positively "+
				"identified as a WASAPI capture endpoint, which would close the gate in front of "+
				"deckLinkCardWithID and send the id to wasapi2src instead", id)
		}
		if err := refuseRenderEndpoint(id); err != nil {
			t.Errorf("refuseRenderEndpoint(%q) = %v, want nil: the card would be unstartable", id, err)
		}
	}
}

// TestPrefixedDeckLinkIDsWouldClassifyAndAreThereforeForbidden is the reason a
// kind is a separate FIELD rather than a prefix inside Device.ID, expressed as
// the thing that would go wrong.
//
// "decklink:2747401380" is a plausible-looking encoding and it is exactly what
// app.go's ListInputDevices doc comment forbids: a shape inside an opaque id.
// This test does not assert that such a string is rejected anywhere — nothing
// produces one, so there would be nothing to reject. It pins the smaller, harder
// fact that makes the rule worth keeping: a prefixed id is a string that the
// Windows classifiers, which are supposed to be inert on this family, would
// still be inert on today and could stop being inert on tomorrow, because
// "contains a substring anywhere" is what IsRenderEndpointID does. The moment an
// id carries structure, every string test in the codebase becomes a potential
// reader of it.
func TestPrefixedDeckLinkIDsWouldClassifyAndAreThereforeForbidden(t *testing.T) {
	const prefixed = "decklink:2747401380"
	if got := formatDeckLinkPersistentID(2747401380); got == prefixed || strings.Contains(got, ":") {
		t.Fatalf("formatDeckLinkPersistentID(2747401380) = %q: Device.ID has grown a discriminator "+
			"inside it. CONTRACT.md keeps the browser mediaDeviceId and the native device id in two "+
			"fields for this reason, and app.go states that nothing above internal/gst may parse "+
			"Device.ID or assume a shape. The kind is Device.Kind", got)
	}
	if _, err := parseDeckLinkPersistentID(prefixed); err == nil {
		t.Errorf("parseDeckLinkPersistentID(%q) succeeded; a prefixed id must not be silently "+
			"accepted, or the forbidden encoding becomes the working one", prefixed)
	}
}
