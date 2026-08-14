//go:build cgo && !gststub && darwin

// return_cgo_darwin.go is the macOS tail of the return pipeline: the AAC
// decoder, the playback sink, and what "a headphone endpoint id" means on a
// machine where the sink's device property is an integer.
//
// Owner: WP-R, with return_cgo.go. Read return.go first for the contract and
// return_cgo_windows.go for the shape this file is the twin of.
//
// # What is DIFFERENT here, and why none of it is a preference
//
// Four things, each of them a fact about CoreAudio or AudioToolbox rather than a
// second opinion about the pipeline. All four were measured on the dev Mac
// (Homebrew GStreamer 1.26.10, macOS 15) with live pipelines; the numbers are on
// the functions.
//
//  1. atdec must be fed RAW AAC, not ADTS. Given aacparse's default output it
//     produces DIGITAL SILENCE — no error, no warning, a green RETURN lamp over
//     nothing at all. One capsfilter fixes it. See returnDecodeSpecs.
//
//  2. A CoreAudio sink can be MULTICHANNEL, and the mix matrix pins two.
//     audioconvert straight into a six-channel device fails negotiation and
//     kills the whole transport. See returnRenderSpecs.
//
//  3. osxaudiosink's device property is a gint AudioDeviceID, not a string, and
//     the integers are runtime handles. The stable identity is the CoreAudio
//     UID; the integer is resolved from it at pipeline-open time and never
//     persisted. See configureReturnSinkLocked.
//
//  4. There is no device.api property on macOS devices at all, so the
//     enumeration predicate cannot be an api string. See playbackDeviceID.
//
// # No libav, ever — the rule holds, and it costs one capsfilter
//
// The decoder is atdec: AudioToolbox, part of macOS, wrapped by an LGPL plugin
// out of gst-plugins-good. It is the exact analogue of mfaacdec on Windows —
// an OS codec behind a thin LGPL wrapper, no third-party AAC implementation and
// no separate AAC patent licence on our side — and it lives in
// libgstosxaudio.dylib, the SAME dylib as osxaudiosrc and osxaudiosink, so the
// bundle gains nothing for it.
//
// The two rejected candidates were measured, not assumed:
//
//	atdec      -4.9525580510 dB   libgstosxaudio.dylib, gst-plugins-good, LGPL
//	avdec_aac  -4.9525580303 dB   libgstlibav.dylib,    gst-libav,        FFmpeg
//	faad       (not run)          libgstfaad.dylib,     gst-plugins-bad,  GPL
//
// on the same AAC-in-MPEG-TS test file, metered with the level element. So the
// decode is indistinguishable and the choice is entirely about what ships.
// avdec_aac is gst-libav, which is FFmpeg — the same commercial-shipping concern
// as x264enc, and the thing build/bundle-gst.ps1's forbidden list already refuses
// to copy on the Windows side. faad is GPL. atdec's one cost is that it needs
// the ADTS headers stripped, which is item 1 above, and that is a capsfilter.
package gst

import (
	"fmt"
	"log"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// returnSinkFactory is the playback sink: osxaudiosink, the CoreAudio sink out
// of gst-plugins-good's osxaudio plugin. It is the same dylib the capture side
// and the decoder come from.
const returnSinkFactory = "osxaudiosink"

// returnPlatformSupported is true: this platform has a return path.
// return_cgo_other.go is the twin that says otherwise.
const returnPlatformSupported = true

// Element names for the two capsfilters and the second audioconvert that only
// the macOS chain has. They follow the ret- convention of return_cgo.go so that
// a bus message names something a field log can be searched for.
const (
	nameReturnRawCaps    = "retraw"    // capsfilter: strip the ADTS headers for atdec
	nameReturnStereoCaps = "retstereo" // capsfilter: pin the mix matrix's two channels
	nameReturnRender     = "retrender" // audioconvert: reconcile with the device's channel count
)

// returnRequiredElements is every element factory this pipeline cannot be built
// without on macOS, mapped to the plugin that provides it in the vendored
// GStreamer.
//
// It is checked by newReturnPipe rather than by Init; see returnMissingElements
// in return_cgo.go for why a plugin missing from the RETURN path must never be a
// reason the contribution feed does not go on air.
//
// capsfilter is coreelements, which is in the bundle for the queues and
// fakesinks already, so items 1 and 2 in the file header cost no new plugin.
// atdec and osxaudiosink both come out of the osxaudio plugin, which the
// contribution path needs for osxaudiosrc, so the whole macOS tail adds nothing
// to the bundle that was not already going in it.
var returnRequiredElements = []struct{ factory, plugin string }{
	{"srtsrc", "srt"},
	{"tsdemux", "mpegtsdemux"},
	{"queue", "coreelements"},
	{"fakesink", "coreelements"},
	{"capsfilter", "coreelements"},
	{"aacparse", "audioparsers"},
	{"atdec", "osxaudio"},
	{"audioconvert", "audioconvert"},
	{"audioresample", "audioresample"},
	{"osxaudiosink", "osxaudio"},
}

// returnAACRawCaps is the capsfilter between aacparse and atdec.
//
// # This is the one that fails silently, and it is the whole reason it is here
//
// aacparse's src pad defaults to stream-format=adts when that is what came out
// of the transport, which is what M2L-X sends. atdec ACCEPTS those caps — it
// negotiates, it reaches PLAYING, it produces buffers of exactly the right size
// at exactly the right rate — and every sample in them is zero. Measured with
// the level element on a 1 kHz tone through an AAC-in-MPEG-TS file:
//
//	aacparse ! atdec                                 rms -699.99999984 dB
//	aacparse ! audio/mpeg,stream-format=raw ! atdec  rms   -4.94 dB
//
// -700 dB is the level element's floor: it is digital silence, not quiet audio.
// There is no error, no warning and no bus message anywhere in the first case,
// so without this filter the return monitor is a green lamp over nothing and the
// only way to find out is to put headphones on.
//
// Asking for stream-format=raw makes aacparse strip the ADTS headers and put the
// configuration in codec_data on the caps instead, which is what AudioToolbox's
// converter actually needs to be told the profile and sample rate. The alternative
// — avdec_aac, which handles ADTS itself — is forbidden by this path's own rule;
// see the file header.
const returnAACRawCaps = "audio/mpeg,stream-format=raw"

// returnStereoCaps is the capsfilter between the mix-matrix audioconvert and the
// resampler.
//
// # Why the return path must pin two channels on macOS and not on Windows
//
// MixMatrix always returns a 2x2 matrix — two ears, a stereo return — and
// audioconvert derives BOTH its pad channel counts from it. On Windows that is
// the end of the story, because wasapi2sink opens a stereo shared-mode stream.
//
// CoreAudio devices are not all stereo. The NDI Audio device on the dev Mac
// reports six channels and nothing else, and the facility interfaces this
// application will meet are exactly the multichannel kind. Linking the matrix
// straight at such a sink fails negotiation, and it does not fail politely —
// measured, the whole transport stops:
//
//	streaming stopped, reason not-negotiated (-4)
//	  .../mpegtsdemux/mpegtsbase.c(1780): mpegts_base_loop ()
//
// so channel selection, which is the entire reason return.go exists, is broken
// on precisely the devices it was written for.
//
// Pinning channels=2 here and putting a SECOND audioconvert after the resampler
// separates the two jobs that were being asked of one element: this half does the
// operator's channel selection, at two channels, exactly as measured on Windows;
// the other half reconciles that stereo with whatever the device wants. Measured
// on the six-channel device, that chain plays to EOS with no negotiation failure,
// and the three modes meter identically to the Windows numbers in MixMatrix's
// doc comment (-4.95 dB / -700 dB, in the documented ears).
const returnStereoCaps = "audio/x-raw,channels=2"

// returnSinkBufferTimeUs is osxaudiosink's ring buffer size in MICROSECONDS.
//
// # What it replaces
//
// wasapi2sink's low-latency property, which osxaudiosink does not have — the
// hasProperty guard on the Windows twin makes that line a silent no-op here, so
// without this the macOS monitor would simply run untuned.
//
// # The measurement
//
// osxaudiosink's default buffer-time is 200000 us, and the sink reports its own
// latency accordingly. Queried through audiobasesink's own debug output on the
// built-in output device:
//
//	buffer-time=200000   min latency 0:00:00.241333333
//	buffer-time=60000    min latency 0:00:00.101333333
//
// so the default puts 241 ms in the commentator's ear before srtsrc's 120 ms is
// counted — about 360 ms of programme delay on a MONITOR, and the return carries
// comms as well as effects, which makes it a talkback delay too.
//
// 60 ms was chosen rather than something smaller because smaller buys nothing
// measurable: the reported latency has a fixed ~41 ms floor under it that is the
// AudioUnit's own, so 20 ms of ring buffer would save 40 ms more and halve the
// margin. Stability was checked rather than assumed — three 25-second soaks each
// at 200, 100 and 60 ms over a live encrypted SRT link produced one or two
// audiobasesink resyncs in EVERY configuration INCLUDING the untouched default,
// so the resyncs are the stream settling and not the ring size. That is a null
// result and it is recorded as one: the evidence says shortening this is free at
// these values, not that it is beneficial beyond the 140 ms.
//
// latency-time is deliberately left at its 10000 us default. It is the write
// granularity, it is already one CoreAudio period, and nothing measured suggests
// touching it.
//
// If a facility interface ever underruns on this, this constant is the one line
// to change and 200000 is the value to change it back to.
const returnSinkBufferTimeUs = 60000

// returnDecodeSpecs is what goes between aacparse and the mix-matrix
// audioconvert: the ADTS-stripping capsfilter and then atdec. See
// returnAACRawCaps for what happens without the first of the two.
func returnDecodeSpecs() []returnElementSpec {
	return []returnElementSpec{
		{factory: "capsfilter", name: nameReturnRawCaps, caps: returnAACRawCaps},
		{factory: "atdec", name: nameReturnDecode},
	}
}

// returnRenderSpecs is what goes between the mix-matrix audioconvert and the
// sink: pin the matrix's two channels, resample, then reconcile with whatever
// the CoreAudio device actually wants. See returnStereoCaps.
func returnRenderSpecs() []returnElementSpec {
	return []returnElementSpec{
		{factory: "capsfilter", name: nameReturnStereoCaps, caps: returnStereoCaps},
		{factory: "audioresample", name: nameReturnResample},
		{factory: "audioconvert", name: nameReturnRender},
	}
}

// playbackDeviceID reports the id ListOutputDevices should offer for one
// enumerated Audio/Sink device, and whether to offer it at all. It is the macOS
// half of the enumeration predicate, and the exact mirror of captureDeviceID in
// deviceprovider_darwin.go.
//
// # It is a predicate, NOT an api string, and that is the point
//
// The bug that started the macOS port is gst_cgo.go's capture-side test
// `device.api != "wasapi2"`, which is false for every device on this platform
// and therefore empties the dropdown. The obvious repair — accept
// device.api == "osxaudio" as well — FAILS THE SAME WAY, because
// osxaudiodeviceprovider does not publish device.api at all. Measured, the whole
// property structure of a macOS audio device is two fields:
//
//	is-default = true
//	unique-id  = BuiltInSpeakerDevice
//
// So the test that can be made here is the one this platform actually supports:
// the device carries a non-empty stable UID, which is what makes it something the
// operator's choice can be persisted as.
//
// # Direction comes from the CLASS, not from the id
//
// device_id.go's namespace classifiers are Windows knowledge and they are INERT
// here — no CoreAudio UID begins with either prefix — so there is nothing to
// check an id against to find out whether it is a microphone. There does not need
// to be: this device came out of a monitor filtered to Audio/Sink and is
// re-checked against that class by the caller. On Windows the extra id check
// earns its keep because wasapi2 republishes render endpoints into the SOURCE
// class; CoreAudio does no such thing, so the class IS the direction.
//
// Being inert rather than wrong is exactly why device_id.go is left alone rather
// than taught about macOS: it is correct, tested Windows knowledge that degrades
// safely, and chooseOutputDevice in return.go turns that inertness into a real
// diagnosis — a POSITIVE Windows classification on a Mac means the config.json
// was carried across.
func playbackDeviceID(dev gogst.Device, props *gogst.Structure) (string, bool) {
	uid := props.GetString(propUniqueID)
	if uid == "" {
		// Nothing stable to persist. Offering it would give the operator a
		// dropdown entry that cannot survive being saved, which is worse than
		// not offering it — so say so rather than letting it vanish silently.
		log.Printf("gst: ListOutputDevices: skipping %q: it publishes no %s, so there is nothing "+
			"stable to persist for it; device properties are %s",
			dev.GetDisplayName(), propUniqueID, structureFieldNames(props))
		return "", false
	}
	return uid, true
}

// configureReturnSinkLocked sets every osxaudiosink property. deviceID has
// already been through vetOutputDeviceID, so an empty value means "the operator
// configured nothing, or what they configured is not on this machine" and the
// element's default is correct.
//
// device=0 is osxaudiosink's default and IS the system default output device;
// measured, a pipeline with device=0 plays to the same place as one with no
// device property set at all. So "leave it alone" and "use the default" are the
// same instruction here, which is what makes the fallback in chooseOutputDevice
// safe on this platform.
//
// # Where the integer comes from, and why the failure is handled HERE
//
// The resolution itself is resolveCoreAudioDeviceIndex, in
// deviceprovider_darwin.go, which the capture side wrote for both directions on
// purpose: osxaudiosink's device property is the SAME gint AudioDeviceID as
// osxaudiosrc's, allocated by coreaudiod per enumeration and reused, so a second
// copy of that rule here is a second place for it to drift. The class and
// direction arguments are all this path has to supply. Measured on the dev Mac,
// the built-in speakers were 81 and the NDI Audio device 92 in one session, and
// those numbers are not a promise about the next one — which is why the integer
// is resolved fresh on every pipeline open and never persisted.
//
// What the two directions do NOT share is what happens when the resolution
// fails, and the difference is deliberate. The capture side refuses outright: a
// commentary feed carrying the wrong microphone with every indicator green is a
// disaster, so no fallback is offered there at all. The return side falls back
// to the default output device and says so, because a monitor in the wrong ears
// is something the commentator notices in one second and the operator fixes from
// the log line, whereas a monitor that refuses to start is a commentary position
// working blind. That decision belongs to the CALLER of the resolution, which is
// why the shared function returns an error rather than making it.
//
// # Why setIntProperty and not setStringProperty
//
// This is the shape of the bug that made the macOS return path silently wrong.
// osxaudiosink HAS a device property, so a presence-only check passes,
// g_object_set_property is handed a string for a gint, GLib emits a CRITICAL
// nobody reads, the setter returns nil, the property stays at 0 — and the
// monitor plays to the system default device with a GREEN RETURN LAMP and no
// diagnosis anywhere. setIntProperty compares the property's actual GType before
// setting it, which is what makes that impossible rather than merely absent.
func configureReturnSinkLocked(sink gogst.Element, deviceID string) error {
	if deviceID == "" {
		log.Printf("gst: return monitor: no usable headphone endpoint configured; " +
			"using the macOS default output device")
	} else if index, err := resolveCoreAudioDeviceIndex("Audio/Sink", "headphone", deviceID); err != nil {
		// A UID that vetOutputDeviceID has just seen in the device list and that
		// cannot be resolved a moment later is a genuine race — the interface was
		// unplugged between the two calls. Falling back rather than failing the
		// attempt is the same trade chooseOutputDevice makes and for the same
		// reason: a monitor in the wrong ears is diagnosable from this line, and
		// no monitor at all is a commentary position working blind.
		log.Printf("gst: return monitor: the headphone endpoint %q could not be resolved to a "+
			"CoreAudio device id (%v); playing to the macOS default output device instead",
			deviceID, err)
	} else if err := setIntProperty(sink, "device", index); err != nil {
		return fmt.Errorf("gst: return monitor: %w", err)
	}

	// The ring buffer. See returnSinkBufferTimeUs for the measurement and for
	// what this replaces on the Windows side.
	//
	// Guarded, because buffer-time is GstAudioBaseSink's rather than
	// osxaudiosink's own and a sink substituted into this slot by a bundle repair
	// might not derive from it. Being untuned is survivable; a GLib CRITICAL on
	// every reconnect is not.
	if hasProperty(sink, "buffer-time") {
		sink.SetObjectProperty("buffer-time", int64(returnSinkBufferTimeUs))
	}
	return nil
}
