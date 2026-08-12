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
