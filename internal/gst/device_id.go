// device_id.go classifies Windows audio endpoint IDs by namespace. It carries
// no build tag on purpose: the same rule is applied by the real twin's
// enumeration and Start, by the stub twin's Start, by the App layer's
// pre-flight, and by tests at Gate A — so it must live where all of them can
// compile it.
//
// # Why this exists
//
// GStreamer's wasapi2 device provider republishes every Windows RENDER
// (playback) endpoint as an Audio/Source "loopback" device, carrying the
// property wasapi2.device.loopback=true. ListInputDevices used to filter only
// on class Audio/Source + device.api == "wasapi2", so the commentary-input
// dropdown offered playback endpoints — measured: 11 of 25 entries on the dev
// machine were loopback republications (eight "DVS Transmit N-M", Speakers,
// HP Z27, "Default Audio Render Device"). An operator on another machine
// selected one ("we had an output selected as an input" — their words). The
// pipeline prerolled, wasapi2's rbuf then failed ASYNCHRONOUSLY with "Failed
// to open device {0.0.0.00000000}.{8678ce58-...}", and the sender treated
// that as a network failure and retried forever, telling the operator "the
// commentary feed to <host>:40005 is not connected and is retrying" — blaming
// the SRT network for a local device fault.
//
// Windows keeps the two families in disjoint namespaces, which is what makes
// a string classification safe at all:
//
//	{0.0.1.00000000}.{guid}   CAPTURE endpoints (microphones, DVS Receive)
//	{0.0.0.00000000}.{guid}   RENDER endpoints (speakers, DVS Transmit)
//
// and the two default-device pseudo entries are named by the DEVINTERFACE
// class GUIDs below. wasapi2src echoes the REQUESTED id verbatim in its
// error-1551 message (proved by probe — no substitution, no default
// fallback), so an id that classifies as render here is an id that WILL fail
// asynchronously there.
//
// # The rule is deliberately asymmetric
//
// Callers refuse only a POSITIVELY IDENTIFIED render id — one that
// IsRenderEndpointID says yes to. An id of unrecognised shape is ACCEPTED
// (with the caller logging a warning), never refused. Refusing unknown shapes
// would turn a future Windows or GStreamer id-shape change into an
// unstartable commentary position: the operator's saved device would stop
// classifying as capture and Start would refuse it twenty minutes before
// kick-off, for no fault of theirs. A render id sneaking through a shape
// change costs one loud, diagnosable Start-time failure; an over-eager
// refusal costs the feed. So IsCaptureEndpointID and IsRenderEndpointID are
// both POSITIVE identifications, both can be false for the same string, and
// the refusal keys on the render one alone.
//
// # The deliberate non-goal
//
// Never set wasapi2's loopback=true property to "make it work" on a render
// endpoint. That opens the operator's own monitor mix as the commentary
// source — echo and feedback on a live feed. These devices are refused, never
// opened; TestLoopbackIsNeverSetInTheCgoSource pins it.
package gst

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// CaptureEndpointPrefix begins every WASAPI IMMDevice CAPTURE endpoint ID.
	// The middle token is Windows' eRender/eCapture data-flow discriminator:
	// 0.0.1 is capture.
	CaptureEndpointPrefix = "{0.0.1.00000000}."

	// RenderEndpointPrefix begins every WASAPI IMMDevice RENDER (playback)
	// endpoint ID — 0.0.0 is render. This is the namespace of the operator's
	// measured failure: "{0.0.0.00000000}.{8678ce58-...}".
	RenderEndpointPrefix = "{0.0.0.00000000}."

	// DefaultCaptureDeviceID is DEVINTERFACE_AUDIO_CAPTURE, the device
	// interface class GUID Windows uses to name "the default capture device"
	// as a pseudo entry. wasapi2's two default-device entries carry it inside
	// their id; endpointID prefers device.actual-id precisely so these resolve
	// to a real endpoint, but the GUID is still classified here in case one
	// leaks through.
	DefaultCaptureDeviceID = "{2EEF81BE-33FA-4800-9670-1CD474972C3F}"

	// DefaultRenderDeviceID is DEVINTERFACE_AUDIO_RENDER — the render-side
	// twin of the above, and positively a playback identity wherever it
	// appears.
	DefaultRenderDeviceID = "{E6327CAD-DCEC-4949-AE8A-991E976A79D2}"
)

// ErrNotACaptureDevice is wrapped by every refusal of a render endpoint id.
// internal/sender and the App layer match it with errors.Is to tell "the
// operator selected a playback device" apart from a genuine connection
// failure — the misdiagnosis that had the sender retrying the SRT link
// forever over a local device fault.
var ErrNotACaptureDevice = errors.New("gst: not a capture endpoint")

// IsCaptureEndpointID reports whether id is POSITIVELY identified as a
// Windows CAPTURE endpoint: the capture namespace prefix, or the
// DEVINTERFACE_AUDIO_CAPTURE class GUID anywhere in the string (the pseudo
// entries and the \\?\SWD#MMDEVAPI# path form both carry it embedded rather
// than leading). Case-insensitive, because GUID casing is not stable across
// the APIs that print these ids.
//
// False does NOT mean "refuse": an unrecognised shape fails this AND
// IsRenderEndpointID, and the asymmetric rule in the file comment says such
// an id is offered and opened with a warning, not rejected.
func IsCaptureEndpointID(id string) bool {
	l := strings.ToLower(id)
	return strings.HasPrefix(l, strings.ToLower(CaptureEndpointPrefix)) ||
		strings.Contains(l, strings.ToLower(DefaultCaptureDeviceID))
}

// IsRenderEndpointID reports whether id is POSITIVELY identified as a Windows
// RENDER (playback) endpoint — the render namespace prefix, or the
// DEVINTERFACE_AUDIO_RENDER class GUID anywhere in the string,
// case-insensitively. This is the only test a refusal may key on; see the
// file comment for why unrecognised shapes must never be refused.
func IsRenderEndpointID(id string) bool {
	l := strings.ToLower(id)
	return strings.HasPrefix(l, strings.ToLower(RenderEndpointPrefix)) ||
		strings.Contains(l, strings.ToLower(DefaultRenderDeviceID))
}

// refuseRenderEndpoint returns the refusal error for a render endpoint id, or
// nil if id is not positively a render endpoint.
//
// It is shared by both twins' Start so the two builds cannot drift on the
// wording or on the wrapped sentinel — a Gate A test that matches this error
// is then also a statement about the real build. The message quotes the
// offending value and names both namespaces, because the operator reading it
// has a config.json with a GUID in it and no other way to tell which family
// it belongs to.
func refuseRenderEndpoint(id string) error {
	if !IsRenderEndpointID(id) {
		return nil
	}
	return fmt.Errorf(
		"gst: PipelineOpts.AudioDeviceID %q is a Windows RENDER (playback) endpoint, not a capture "+
			"endpoint: capture ids begin %q, render ids begin %q. wasapi2src cannot record from a "+
			"playback device, and opening it as loopback would put the operator's own monitor mix "+
			"on air. Select a capture device in the commentary input dropdown: %w",
		id, CaptureEndpointPrefix, RenderEndpointPrefix, ErrNotACaptureDevice)
}
