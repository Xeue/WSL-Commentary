//go:build cgo && !gststub && windows

// return_cgo_windows.go is the WINDOWS tail of the return pipeline: the AAC
// decoder, the playback sink, and what "a headphone endpoint id" means.
//
// Owner: WP-R, with return_cgo.go.
//
// # Why this file exists at all
//
// It exists because the application now also runs on macOS, and NOTHING in it is
// new: every line was lifted out of return_cgo.go unchanged when the macOS twin
// was written, so that the port could not alter Windows behaviour by accident.
// If this file and return_cgo_darwin.go ever disagree about something that is
// not actually a difference between Media Foundation and AudioToolbox, this one
// is the one that has been measured against the operator's machine.
//
// What is given up by drawing the seam here rather than lower down: the two
// platforms no longer share one element list, so a change to the common part of
// the chain — a queue's depth, say — has to be made in return_cgo.go where both
// see it, and a change made here reaches Windows only. That is the trade that
// keeps `if runtime.GOOS == "windows"` out of the middle of buildLocked, where
// it would compile on both platforms and be told it was wrong about neither.
//
// # No libav, ever
//
// mfaacdec is Media Foundation: part of Windows, wrapped by an LGPL GStreamer
// plugin. avdec_aac is present on the dev machine at primary rank and must not
// be used; see the rule in return_cgo.go's header, which the macOS twin obeys
// with atenc's decoder half for exactly the same reason.
package gst

import (
	"fmt"
	"log"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// returnSinkFactory is the playback sink. wasapi2, not the older wasapi plugin:
// it is what the contribution path captures with, so the bundle carries it
// already, and its device property takes the IMMDevice endpoint ID string that
// ListOutputDevices reports.
const returnSinkFactory = "wasapi2sink"

// returnPlatformSupported is true: this platform has a return path.
// return_cgo_other.go is the twin that says otherwise.
const returnPlatformSupported = true

// returnRequiredElements is every element factory this pipeline cannot be built
// without on Windows, mapped to the plugin from build/bundle-gst.ps1's
// allowlist that provides it.
//
// It is checked by newReturnPipe rather than by Init; see returnMissingElements
// in return_cgo.go for why the return path's missing plugin must not be a reason
// the contribution feed does not go on air.
//
// mpegtsdemux is the entry that was not in the bundle before this work; see the
// Why line on it in build/bundle-gst.ps1.
var returnRequiredElements = []struct{ factory, plugin string }{
	{"srtsrc", "srt"},
	{"tsdemux", "mpegtsdemux"},
	{"queue", "coreelements"},
	{"fakesink", "coreelements"},
	{"aacparse", "audioparsers"},
	{"mfaacdec", "mediafoundation"},
	{"audioconvert", "audioconvert"},
	{"audioresample", "audioresample"},
	{"wasapi2sink", "wasapi2"},
}

// returnDecodeSpecs is what goes between aacparse and the mix-matrix
// audioconvert: on Windows, the decoder and nothing else.
//
// mfaacdec takes aacparse's output as it comes. It advertises
// stream-format={raw,adts} on its sink pad and negotiates whichever the parser
// offers, so unlike the macOS twin there is no capsfilter here and no reason to
// add one.
func returnDecodeSpecs() []returnElementSpec {
	return []returnElementSpec{
		{factory: "mfaacdec", name: nameReturnDecode},
	}
}

// returnRenderSpecs is what goes between the mix-matrix audioconvert and the
// sink: on Windows, the resampler and nothing else.
//
// The mix matrix pins audioconvert's output at two channels and wasapi2sink
// opens a stereo shared-mode stream, so nothing between them has to reconcile a
// channel count. The macOS twin has more here, and the reason is a fact about
// CoreAudio devices rather than a better idea — see returnRenderSpecs there.
func returnRenderSpecs() []returnElementSpec {
	return []returnElementSpec{
		{factory: "audioresample", name: nameReturnResample},
	}
}

// playbackDeviceID reports the id ListOutputDevices should offer for one
// enumerated Audio/Sink device, and whether to offer it at all. It is the
// Windows half of the enumeration predicate, and the exact mirror of
// captureDeviceID in deviceprovider_windows.go.
//
// It is the exact mirror of ListInputDevices' filtering, and every part of it is
// load-bearing:
//
//   - device.api == "wasapi2" discards providers whose ids are meaningless to
//     wasapi2sink. Device.ID is passed verbatim to that element, so an id from
//     any other provider is a selection that silently fails.
//
//   - endpointID is gst_cgo.go's, and it is the right function for both
//     directions: it reads the same provider property keys and falls back to
//     gst_device_create_element, which for a sink device returns a wasapi2sink
//     with its device property already set. That fallback is authoritative by
//     construction — it is literally the value the element will be given.
//
//   - the CAPTURE namespace refusal is not hypothetical. Measured on the dev
//     machine, the Audio/Sink id set was a byte-exact subset of Audio/Source, so
//     without it the two dropdowns can offer the same string and a microphone
//     can be persisted as headphones. The asymmetry device_id.go describes holds
//     in this direction too: only a POSITIVE capture identification skips, and
//     an unrecognised shape is offered, because refusing unknown shapes would
//     empty the dropdown on a future id-shape change.
//
// Every rejection logs its reason, INCLUDING the provider rejection. That was
// the one silent branch here for as long as it was silent in captureDeviceID,
// and the two are fixed together on purpose: they are declared mirrors, so one
// logging and one not would leave the two dropdowns disagreeing about what a
// missing device means.
func playbackDeviceID(dev gogst.Device, props *gogst.Structure) (string, bool) {
	// The reasoning for logging this branch is captureDeviceID's, in
	// deviceprovider_windows.go, and is not repeated in full: on the SHIPPED
	// bundle it should fire nought times, because the DLL allowlist stages only
	// wasapi2 and mediafoundation and the latter's provider enumerates
	// Video/Source — so a machine where it DOES fire has picked up a foreign
	// GStreamer, which is exactly what Init's GST_PLUGIN_SYSTEM_PATH sequence
	// exists to prevent and which produces no other message anywhere.
	//
	// The one thing that differs on this side is the SYMPTOM the line has to
	// explain. A capture device missing from the dropdown is at least visible
	// against the machine's other microphones; the headphone dropdown going
	// empty looks identical to "the return path is not built yet", and with no
	// line at all there is nothing to read between that and a bundle fault.
	if api := props.GetString("device.api"); api != "wasapi2" {
		log.Printf("gst: ListOutputDevices: skipping %q: device.api is %q, not wasapi2 — its id "+
			"would be meaningless to %s, which is the only element this build opens a playback "+
			"endpoint with; device properties are %s",
			dev.GetDisplayName(), api, returnSinkFactory, structureFieldNames(props))
		return "", false
	}
	id := endpointID(dev, props)
	if id == "" {
		log.Printf("gst: ListOutputDevices: skipping %q: no endpoint ID; device properties are %s",
			dev.GetDisplayName(), structureFieldNames(props))
		return "", false
	}
	if IsCaptureEndpointID(id) {
		log.Printf("gst: ListOutputDevices: skipping %q (%s): its endpoint id is in the CAPTURE "+
			"namespace (%s...) — a capture device cannot be the commentator's headphones",
			dev.GetDisplayName(), id, CaptureEndpointPrefix)
		return "", false
	}
	return id, true
}

// configureReturnSinkLocked sets every wasapi2sink property. deviceID has
// already been through vetOutputDeviceID, so an empty value means "the operator
// configured nothing, or what they configured is not on this machine" and the
// element's default endpoint is correct.
//
// The device property is a STRING here — that is the whole reason this function
// is per-platform — and setStringProperty is type-checked, so a future
// wasapi2sink that changed it would fail loudly instead of leaving the monitor
// on the default endpoint with a green lamp.
func configureReturnSinkLocked(sink gogst.Element, deviceID string) error {
	if deviceID == "" {
		log.Printf("gst: return monitor: no usable headphone endpoint configured; " +
			"using the Windows default playback device")
	} else if err := setStringProperty(sink, "device", deviceID); err != nil {
		return fmt.Errorf("gst: return monitor: %w", err)
	}

	// low-latency is wasapi2's own property and selects the shared-mode
	// low-latency period. It is guarded rather than assumed because the older
	// wasapi plugin has no such property and a bundle repair could substitute
	// one for the other.
	if hasProperty(sink, "low-latency") {
		sink.SetObjectProperty("low-latency", true)
	}
	return nil
}
