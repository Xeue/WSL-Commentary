//go:build cgo && !gststub && darwin

// deviceprovider_darwin.go is the macOS half of the DEVICE seam — the twin of
// deviceprovider_windows.go, which explains the seam itself and should be read
// first.
//
// Owner: WP-3a.
//
// # READ THIS BEFORE SIMPLIFYING ANYTHING IN THIS FILE
//
// This file contains the single most important structural difference in the
// macOS port, and it looks like redundant work if you do not know why it is
// there. It is this:
//
//	THE ID THIS APPLICATION PERSISTS IS NOT THE ID THE ELEMENT ACCEPTS,
//	AND IT MUST BE RESOLVED FROM ONE TO THE OTHER EVERY TIME THE PIPELINE OPENS.
//
// On Windows the two are the same string. A WASAPI IMMDevice endpoint GUID is a
// stable identity: it survives reboots, replugs and renames, config.json holds
// it, and wasapi2src's device property takes it verbatim. captureDeviceID hands
// it over and configureCaptureSource sets it. There is nothing in between.
//
// On macOS they are two different things of two different types:
//
//   - CoreAudio's unique-id ("BuiltInMicrophoneDevice", "NDIAudio", or a UUID
//     such as "BF568F24-731B-41DB-932E-AC7E260BC71A") is a STABLE IDENTITY. It
//     is the string gst-device-monitor publishes, and it is what this
//     application persists.
//   - osxaudiosrc's "device" property is a gint AudioDeviceID — measured on this
//     machine as 76, 88 and 105 for the three inputs. Those integers are RUNTIME
//     HANDLES allocated by coreaudiod. They are assigned per enumeration and are
//     NOT STABLE across reboots, across replugs, or across another application
//     opening a device first. Nothing about them is an identity.
//
// So the persisted string has to be turned back into the current integer at
// pipeline-open time, from a FRESH enumeration, every time — which is what
// resolveCaptureDeviceIndex does and why configureCaptureSource calls it.
//
// # What breaks if someone "simplifies" this back to storing the integer
//
// It would work. It would work all day on the machine it was written on, and it
// would work in every test anyone thought to run, because nothing reboots
// during a test. Then the operator restarts their Mac before a match,
// coreaudiod hands out different numbers, and config.json's saved 76 now names
// a different device — or names nothing, in which case osxaudiosrc falls back to
// its default of 0, which CoreAudio reads as "the system default input". The
// pipeline starts. The lamp goes green. The commentary feed is the built-in
// microphone under the operator's desk instead of the Dante return they
// selected, and NOTHING anywhere reports a fault. That is a silent
// wrong-device failure at a facility, and it is why the resolution is not
// optional and why a failed resolution below is a hard, loud error rather than
// a fallback to the default.
//
// The same rule applies on the return path — see RET-6 in the port notes:
// headphoneEndpointId stays a string holding the CoreAudio UID, and the integer
// never touches config.json, never crosses the Wails boundary, and never
// appears in a log except as a diagnostic.
//
// # What is NOT here, and why
//
// There is no api check, no loopback filter and no id-namespace classifier.
//
//   - macOS devices publish NO device.api property. That was the bug that
//     started the port: the old inline 'api != "wasapi2"' check skipped every
//     device and ListInputDevices returned an empty list, so the operator's
//     microphone never appeared in the dropdown. Adding an 'api == "osxaudio"'
//     branch would have failed identically, because the property is not
//     published under any name. The whole of a macOS device's published
//     properties is: is-default, unique-id.
//   - CoreAudio has no loopback republication to filter out. There is nothing
//     that turns an output device into a fake input, so there is nothing that
//     could put the operator's monitor mix on air by accident.
//   - Direction comes from the device CLASS — Audio/Source versus Audio/Sink —
//     not from the shape of the id, so the Windows namespace classifiers in
//     device_id.go have nothing to say here. See that file's "On macOS" section:
//     they are inert rather than wrong, and consulting them would log a
//     spurious warning for every device on every enumeration.
//
// is-default is published and deliberately unused. Which device the dropdown
// pre-selects is the App layer's decision and is already made from the saved
// config; picking a different device from the operator's saved one because
// macOS changed its default would be the same wrong-device failure by another
// route.

package gst

import (
	"fmt"
	"log"
	"strings"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// propUniqueID is the GstStructure key under which osxaudiodeviceprovider
// publishes a device's CoreAudio unique id — the stable identity this
// application persists. Verified with gst-device-monitor-1.0 Audio/Source on
// the port machine (Darwin 25.3.0, GStreamer 1.26.10): it is one of exactly two
// properties published, the other being is-default. There is no device.api and
// no numeric id among them.
const propUniqueID = "unique-id"

// bundleAllowlistNoun is the macOS word for one file in the bundled GStreamer,
// used by Init's "the bundle is incomplete" error. See the Windows twin for why
// this is a per-platform constant rather than one shared word: "DLL" is the term
// every Windows build document uses and the phrase a Windows operator will
// search for, and it is simply wrong here, because a .app contains none.
//
// "dylib" is the right word for both halves of the macOS bundle —
// build/bundle-gst-darwin.sh stages Contents/Resources/gstreamer-1.0/*.dylib as
// the plugins and Contents/Frameworks/*.dylib as their closure — and it is what
// that script's own output calls them.
const bundleAllowlistNoun = "dylib"

// resetSkipDetail and skipDetail are the macOS half of the enumeration summary
// line's platform tail. They do nothing, and that is the correct implementation
// rather than a stub waiting to be filled in.
//
// The Windows twin counts wasapi2's loopback republications of playback
// endpoints, because on that platform every render endpoint is republished as a
// fake Audio/Source and the number of them that were filtered out is the most
// useful figure in the line. CoreAudio has no such republication — see this
// file's header — so there is nothing to count. Printing "0 loopbacks" here
// would assert a filter that does not exist and quietly invite a future reader
// to go looking for the macOS loopback rule, of which there is none.
//
// The generic part of the summary — offered, total, skipped — is shared and
// already covers the only skip this platform has, which is a device publishing
// no unique-id.
func resetSkipDetail()   {}
func skipDetail() string { return "" }

// captureDeviceID reports whether one enumerated Audio/Source device should be
// offered in the commentary input dropdown, and under what persistable id.
//
// The predicate is "it is an Audio/Source device carrying a non-empty
// unique-id". That is deliberately the weakest test that is still safe, for the
// reason device_id.go gives about asymmetry: refusing devices of an unfamiliar
// shape turns a future CoreAudio or GStreamer change into an empty dropdown at
// a facility, whereas offering one costs at worst a loud failure at Start.
//
// A device with no unique-id is skipped, because there would be nothing to
// persist and nothing to resolve later — the dropdown entry would produce a
// selection that could never be reopened.
func captureDeviceID(dev gogst.Device, props *gogst.Structure) (string, bool) {
	id := props.GetString(propUniqueID)
	if id == "" {
		// The property names are logged rather than guessed at, so that if a
		// future GStreamer renames the key this line is the whole diagnosis
		// instead of "the dropdown is empty again".
		log.Printf("gst: ListInputDevices: skipping %q: it publishes no %s; device properties are %s",
			dev.GetDisplayName(), propUniqueID, structureFieldNames(props))
		return "", false
	}
	return id, true
}

// configureCaptureSource points osxaudiosrc at the device the operator chose.
//
// deviceID is the CoreAudio unique-id that came out of config.json. It is
// resolved to the current AudioDeviceID integer HERE, at pipeline-open time,
// against a fresh enumeration — see the file comment for why that indirection
// is mandatory and what silently breaks without it.
//
// osxaudiosrc has no low-latency property (its Windows counterpart does, and
// the Windows twin sets it). The nearest equivalents are buffer-time —
// 200000 us by default — and latency-time, and both are deliberately left
// alone: the default is not where this pipeline's latency budget is spent, and
// shortening a CoreAudio capture buffer to chase milliseconds is how a live
// feed acquires dropouts. If that trade is ever revisited it should be
// revisited with a measurement, not with an intuition.
func configureCaptureSource(src gogst.Element, deviceID string) error {
	index, err := resolveCaptureDeviceIndex(deviceID)
	if err != nil {
		return err
	}
	// The integer is logged as a DIAGNOSTIC and nowhere else. It is meaningful
	// only for the lifetime of this enumeration, so a log line pairing it with
	// the unique-id is the only place the two are ever seen together — which is
	// what makes a later CoreAudio complaint about device N traceable back to
	// the device the operator actually picked.
	log.Printf("gst: Start: resolved CoreAudio unique-id %q to AudioDeviceID %d for %s",
		deviceID, index, captureSourceFactory)
	// setIntProperty, not setStringProperty: the property is a gint, and the
	// string setter would emit a GLib CRITICAL nobody reads and leave the
	// property at 0 — which CoreAudio reads as "the system default input".
	return setIntProperty(src, "device", index)
}

// resolveCaptureDeviceIndex turns a persisted CoreAudio unique-id into the
// AudioDeviceID integer osxaudiosrc's device property wants, right now.
func resolveCaptureDeviceIndex(uniqueID string) (int32, error) {
	return resolveCoreAudioDeviceIndex("Audio/Source", "capture", uniqueID)
}

// resolveCoreAudioDeviceIndex is the resolution itself, written for either
// direction from the start.
//
// The return path needs the identical mechanism — RET-6: osxaudiosink's device
// property is the SAME gint AudioDeviceID as osxaudiosrc's, with the same
// instability and the same silently-use-the-default-on-0 behaviour, so
// headphoneEndpointId must stay a string holding the CoreAudio UID and be
// resolved here at pipeline-open time. The class and direction parameters exist
// so that return_cgo.go's macOS twin can call this rather than growing a second
// copy of a rule that must not diverge.
//
// It re-runs the device monitor rather than caching anything. A cache would be
// wrong at exactly the moment it mattered: the integers change when coreaudiod
// re-enumerates, which is precisely what happens when a device is unplugged and
// plugged back in mid-session — the case a commentary position actually meets.
// One probe costs a few milliseconds and happens once per pipeline open.
//
// The integer comes from gst_device_create_element, not from a property,
// because the provider does not publish it: osxaudiodeviceprovider builds an
// osxaudiosrc (or osxaudiosink) with device already set to the AudioDeviceID it
// enumerated. That makes the value authoritative by construction — it is
// literally what the provider would hand the element — and it costs one element
// instantiation. No audio device is opened, because the element stays in NULL.
//
// A device that has gone away is a HARD ERROR naming the id that was looked for
// and every id that was found. There is deliberately no fallback to the default
// device: falling back is how a commentary feed ends up carrying the wrong
// microphone with every indicator green.
func resolveCoreAudioDeviceIndex(class, direction, uniqueID string) (int32, error) {
	devices := enumerateDevices(class)

	// available is built as we go so that a failure can say what the machine
	// DOES have. An operator reading "the device is not present" learns
	// nothing; an operator reading it next to the list of what is present can
	// see at a glance that the Dante driver has not started.
	available := make([]string, 0, len(devices))
	for _, dev := range devices {
		if dev == nil || !dev.HasClasses(class) {
			continue
		}
		props := dev.GetProperties()
		if props == nil {
			continue
		}
		id := props.GetString(propUniqueID)
		if id == "" {
			continue
		}
		if id != uniqueID {
			available = append(available, fmt.Sprintf("%s (%s)", id, dev.GetDisplayName()))
			continue
		}

		el := dev.CreateElement("")
		if el == nil {
			return 0, fmt.Errorf("gst: %s device %q (%s) is present but gst_device_create_element "+
				"returned nil, so its AudioDeviceID cannot be read",
				direction, uniqueID, dev.GetDisplayName())
		}
		v := el.ObjectProperty("device")
		index, ok := v.(int32)
		if !ok {
			// go-glib marshals G_TYPE_INT to int32. Anything else means the
			// property's type has changed under us, and guessing at a
			// conversion here would put us straight back into the silent
			// wrong-device failure this file exists to prevent.
			return 0, fmt.Errorf("gst: the %s element's device property on %q read back as %T, not "+
				"the int32 a G_TYPE_INT AudioDeviceID marshals to; refusing to guess",
				direction, uniqueID, v)
		}
		if index == 0 {
			// kAudioObjectUnknown. It is also the element's own default, so
			// setting it would be indistinguishable from not setting it at all
			// — and not setting it means "the system default device".
			return 0, fmt.Errorf("gst: %s device %q (%s) resolved to AudioDeviceID 0, which is "+
				"kAudioObjectUnknown and is also the element's default — using it would silently "+
				"use the system default device instead", direction, uniqueID, dev.GetDisplayName())
		}
		return index, nil
	}

	return 0, fmt.Errorf("gst: %s device %q is no longer present. CoreAudio currently offers: [%s]. "+
		"The saved id is a stable CoreAudio unique-id, so this means the device itself has gone — "+
		"unplugged, or its driver has not started — rather than that the id has changed. Select a "+
		"device in the dropdown", direction, uniqueID, strings.Join(available, ", "))
}
