//go:build cgo && !gststub && windows

// deviceprovider_windows.go is the Windows half of the DEVICE seam: it answers
// the only two questions ListInputDevices and Start ask about a platform's
// audio devices.
//
//	captureDeviceID       does this enumerated device belong to the provider
//	                      this build drives, and if so what string do we persist
//	                      for it?
//	configureCaptureSource  given that persisted string, point the capture source
//	                      at the device it names.
//
// It also carries two small pieces of per-platform VOCABULARY that the shared
// code in gst_cgo.go has to speak but cannot know:
//
//	skipDetail            the platform's tail on the enumeration summary line —
//	                      on Windows, the WASAPI loopback count.
//	bundleAllowlistNoun   what one file in the GStreamer bundle is called, for
//	                      Init's missing-element error.
//
// Neither is a device question, and elements_windows.go is arguably the tidier
// home for the second. They are here because that file is deliberately a CLOSED
// list — its own header promises it and its twin declare exactly the same six
// identifiers and nothing else — whereas this pair is already the place where
// the platform's own vocabulary about its audio stack lives.
//
// Owner: WP-3a. Its twin is deviceprovider_darwin.go, and everything below was
// lifted out of gst_cgo.go unchanged when the twin was written — same checks,
// same order, same log lines. Windows behaviour is byte-identical to what it
// was before the seam existed; that was the constraint the seam was built
// under.
//
// # Why this is a function and not a string comparison
//
// The original code filtered on device.api == "wasapi2" and then applied three
// more wasapi2-specific rules. It is tempting to generalise that to "compare
// device.api to the right value per platform". That does not work, and it fails
// silently in exactly the way the original bug did: macOS devices publish NO
// device.api property at all, so any comparison against any value skips every
// device and returns an empty dropdown. The question a platform can actually
// answer is a predicate over the properties it does publish, which is why the
// seam has this shape.

package gst

import (
	"fmt"
	"log"
	"sync/atomic"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// bundleAllowlistNoun is the Windows word for one file in the bundled
// GStreamer, and it exists so that Init's "the bundle is incomplete" error
// names something the person reading it can search for.
//
// "DLL allowlist" is the term the whole of the rest of this project uses for
// the thing that decides which files reach the bundle — build/bundle-gst.ps1
// implements it, and RUNNING.md, CONTRACT.md, BUILD-NOTES.md,
// docs/windows-app-spec.md, docs/project-plan.md and build/README.md all call it
// that. When Init became platform-neutral the sentence was generalised to
// "plugin allowlist", which reads fine and costs the Windows operator the exact
// phrase that finds the documentation. Restoring it as a per-platform constant
// is the only way to keep it: "DLL" is simply wrong about a macOS bundle, which
// contains none.
//
// A constant per platform rather than a runtime.GOOS branch inside Init, for the
// reason the whole package gives: there is deliberately no runtime.GOOS in it.
const bundleAllowlistNoun = "DLL"

// propLoopback is the GstStructure key under which wasapi2's device provider
// marks an Audio/Source entry as its loopback republication of a RENDER
// (playback) endpoint. captureDeviceID READS it to skip those entries; it is
// never SET on wasapi2src, because loopback capture of the operator's own
// monitor mix on a live commentary feed is the deliberate non-goal recorded in
// device_id.go, and TestLoopbackIsNeverSetInTheCgoSource pins that.
const propLoopback = "wasapi2.device.loopback"

// endpointIDKeys are the GstStructure keys under which a wasapi2 device might
// publish its IMMDevice endpoint ID, most preferred first.
//
// device.actual-id leads deliberately: the provider's two default-device
// pseudo entries ("Default Audio Capture/Render Device") carry a DEVINTERFACE
// class GUID as device.id but publish the REAL endpoint currently behind the
// default as device.actual-id — preferring it resolves the pseudo entry to a
// concrete endpoint that survives the default changing, and lets the de-dup
// in ListInputDevices fold it onto the real entry it mirrors.
//
// UNVERIFIED: gstwasapi2device.c is believed to publish "device.id"; neither
// that nor the others has been read against 1.28.5 on the target machine. The
// fallback below does not depend on any of these names being right.
var endpointIDKeys = []string{"device.actual-id", "device.id", "device.strid", "device.path"}

// captureDeviceID reports whether one enumerated Audio/Source device should be
// offered in the commentary input dropdown, and under what persistable id.
//
// The id it returns is a WASAPI IMMDevice endpoint GUID. It goes straight into
// config.json and straight into wasapi2src's device property, unchanged and
// unresolved — Windows' endpoint GUIDs are stable identities, which is the
// whole reason the Windows path is the simple one. (The macOS twin cannot do
// this; see deviceprovider_darwin.go.)
//
// # Audio/Source is not enough, and this used to be wrong about it
//
// wasapi2's device provider republishes every RENDER (playback) endpoint as an
// Audio/Source "loopback" device carrying wasapi2.device.loopback=true, so the
// class check alone offered playback endpoints as commentary inputs — measured:
// 11 of 25 entries on the dev machine, and an operator on another machine
// selected one. The pipeline prerolled, wasapi2's rbuf failed ASYNCHRONOUSLY
// with "Failed to open device {0.0.0.00000000}.{8678ce58-...}", and the sender
// retried the SRT link forever over a local device fault. So two more filters
// run below: the loopback property, and the endpoint-id namespace classifier
// in device_id.go. Loopback devices are refused, never opened — setting
// loopback=true to "make them work" would put the operator's own monitor mix
// on air, which is the deliberate non-goal recorded in device_id.go.
//
// Every rejection logs its reason, INCLUDING the provider rejection, which used
// to be the one silent one. A device missing from the dropdown is otherwise
// indistinguishable from a device that is not plugged in.
func captureDeviceID(dev gogst.Device, props *gogst.Structure) (string, bool) {
	// Devices from any other provider are discarded rather than offered,
	// because the id below is passed verbatim to wasapi2src and only wasapi2's
	// ids are meaningful there.
	//
	// This branch was silent for the whole of the Windows build's life, and the
	// case for leaving it silent is real: it can fire once per endpoint, and the
	// dev machine enumerates 25 of them. It is logged anyway, for two reasons.
	//
	// First, on the SHIPPED bundle it should fire nought times. The DLL
	// allowlist stages exactly two plugins that register a device provider,
	// wasapi2 and mediafoundation, and mediafoundation's enumerates Video/Source
	// — so every device reaching a monitor filtered to Audio/Source ought to be
	// a wasapi2 one. (UNVERIFIED against 1.28.5 on the target machine; the line
	// is harmless if that is wrong, because it gates nothing.) A machine where
	// it DOES fire is a machine that has picked up a foreign GStreamer, which is
	// the failure the whole GST_PLUGIN_SYSTEM_PATH sequence in Init exists to
	// prevent and which produces no other message anywhere.
	//
	// Second, the alternative was to correct the two doc comments that promise
	// every skip is logged — this one and ListInputDevices'. That trades a
	// diagnostic for a smaller log on the one path where the symptom is "the
	// dropdown is empty" and there is nothing at all to read. If it ever does
	// become noisy, the noise IS the diagnosis rather than a reason to delete
	// the line.
	if api := props.GetString("device.api"); api != "wasapi2" {
		log.Printf("gst: ListInputDevices: skipping %q: device.api is %q, not wasapi2 — its id would "+
			"be meaningless to %s, which is the only element this build opens a capture endpoint "+
			"with; device properties are %s",
			dev.GetDisplayName(), api, captureSourceFactory, structureFieldNames(props))
		return "", false
	}
	// The loopback filter. GetBoolean fails unless the field exists AND is
	// G_TYPE_BOOLEAN, which is how gstwasapi2device.c publishes it; if a
	// future GStreamer changes the type this check silently passes and the
	// namespace check below catches the device anyway, because a loopback
	// republication always carries the render-namespace id of the playback
	// endpoint it mirrors. Checked before endpointID so a device being
	// skipped never costs the CreateElement fallback.
	if loop, ok := props.GetBoolean(propLoopback); ok && loop {
		loopbacksSkipped.Add(1)
		log.Printf("gst: ListInputDevices: skipping %q (%s): %s=true — it is wasapi2's loopback "+
			"republication of a playback endpoint, and opening it would put the operator's own "+
			"monitor mix on air", dev.GetDisplayName(), props.GetString("device.id"), propLoopback)
		return "", false
	}
	id := endpointID(dev, props)
	if id == "" {
		// A wasapi2 device with no recoverable endpoint ID cannot be persisted
		// or passed to wasapi2src, so offering it in the dropdown would produce
		// a selection that silently fails at Start. Log the property names
		// available so that Gate B has something to work with if the provider's
		// key ever changes.
		log.Printf("gst: ListInputDevices: skipping %q: no endpoint ID; device properties are %s",
			dev.GetDisplayName(), structureFieldNames(props))
		return "", false
	}
	// The namespace classifier, device_id.go. Only a POSITIVELY identified
	// render id is refused; an unrecognised shape is offered with a warning,
	// because refusing unknown shapes would turn a future Windows or
	// GStreamer id-shape change into an empty dropdown at a facility.
	if IsRenderEndpointID(id) {
		log.Printf("gst: ListInputDevices: skipping %q (%s): its endpoint id is in the RENDER "+
			"namespace (%s...) — a playback device that cannot be a commentary input",
			dev.GetDisplayName(), id, RenderEndpointPrefix)
		return "", false
	}
	if !IsCaptureEndpointID(id) {
		log.Printf("gst: ListInputDevices: WARNING: %q (%s) has an endpoint id of unrecognised "+
			"shape — offering it anyway (capture ids begin %s); if Start later fails to open it, "+
			"this line is the diagnosis", dev.GetDisplayName(), id, CaptureEndpointPrefix)
	}
	return id, true
}

// loopbacksSkipped counts the loopback republications captureDeviceID refused
// during the enumeration currently in progress. It is DIAGNOSTIC ONLY: nothing
// reads it but skipDetail, and no decision anywhere depends on its value.
//
// atomic rather than a plain int because ListInputDevices is reachable from two
// goroutines at once — the Wails binding and the LAN control bridge both call
// it, and neither serialises against the other. The atomic buys freedom from a
// torn read and from the race detector, and nothing else: two enumerations
// overlapping can still attribute one's loopbacks to the other's summary line.
// That is accepted deliberately, because the alternative is a mutex held across
// a device enumeration to make a log line prettier, and because the number is
// never load-bearing. The per-device skip lines above are the record; this is
// the total at the bottom of them.
var loopbacksSkipped atomic.Int64

// resetSkipDetail starts a fresh count. ListInputDevices calls it before its
// loop, so that the summary describes THIS enumeration rather than every
// enumeration since the process started — which matters because the operator
// reopens the Settings screen, and a number that only ever grows would read as
// loopback devices multiplying.
func resetSkipDetail() { loopbacksSkipped.Store(0) }

// skipDetail is the Windows tail of the enumeration summary log line.
//
// The count it restores was in that line before the platform seam existed, and
// it is the single most useful number in it: on the dev machine 11 of 25
// Audio/Source entries were wasapi2's loopback republications of playback
// endpoints, and offering them is the defect this whole filter exists for. The
// seam moved the filter behind captureDeviceID and the summary was left saying
// only how many devices were skipped in total, which cannot distinguish "the
// loopback filter did its job" from "eleven of the operator's microphones
// vanished".
//
// It prints even when the count is zero, on purpose. Zero loopbacks on a
// Windows machine is itself a surprise worth seeing — wasapi2 republishes every
// render endpoint, so a machine with any playback device at all should have
// some — and a phrase that is always present is a phrase a log search finds.
func skipDetail() string {
	return fmt.Sprintf(", %d of them WASAPI loopback republications of playback endpoints",
		loopbacksSkipped.Load())
}

// endpointID extracts the IMMDevice endpoint ID GUID for one device.
//
// It tries the property keys above first because that is cheap. If none of them
// is present it falls back to gst_device_create_element, which returns a
// wasapi2src with its device property already set to whatever the provider
// considers the device's identity. That fallback is authoritative by
// construction — it is literally the value wasapi2src will be given — and it is
// what makes this function robust against the property key having been renamed.
// It costs one element instantiation per device; no audio endpoint is opened,
// because the element stays in NULL.
func endpointID(dev gogst.Device, props *gogst.Structure) string {
	for _, key := range endpointIDKeys {
		if props.HasField(key) {
			if v := props.GetString(key); v != "" {
				return v
			}
		}
	}

	// The fallback actually running means the provider's property keys have
	// changed. Say so: at Gate B this line plus structureFieldNames in the
	// caller's skip message is the whole diagnosis, and without it the only
	// symptom is enumeration quietly costing an element instantiation per
	// device.
	log.Printf("gst: endpointID: %q publishes none of %v; falling back to gst_device_create_element",
		dev.GetDisplayName(), endpointIDKeys)

	el := dev.CreateElement("")
	if el == nil {
		return ""
	}
	if !hasProperty(el, "device") {
		return ""
	}
	v, ok := el.ObjectProperty("device").(string)
	if !ok {
		return ""
	}
	return v
}

// configureCaptureSource points wasapi2src at the endpoint the operator chose.
//
// deviceID is the value that came out of config.json, and it is passed through
// UNCHANGED. There is nothing to resolve: a WASAPI endpoint GUID is a stable
// identity that survives reboots, replugs and renames, and wasapi2src's device
// property is a G_TYPE_STRING that takes it directly. Start has already logged
// it verbatim, and has already refused it if it is a render endpoint.
func configureCaptureSource(src gogst.Element, deviceID string) error {
	if err := setStringProperty(src, "device", deviceID); err != nil {
		return err
	}
	// low-latency is a nice-to-have on wasapi2src, not a requirement. If a
	// future GStreamer renames it, running at the default latency is better
	// than refusing to start a commentary position.
	if hasProperty(src, "low-latency") {
		src.SetObjectProperty("low-latency", true)
	} else {
		log.Printf("gst: %s has no low-latency property; using the default capture latency", nameAudioSrc)
	}
	return nil
}
