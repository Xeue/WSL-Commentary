//go:build live && cgo && !gststub

// channelwidth_live_test.go is step 0c of the always-live capture plan, and it
// decides an architecture rather than a detail.
//
// # The question
//
// PLAN.md section 4.2 wants the routing panel to appear AS SOON AS A DEVICE IS
// SELECTED, sized to the width that device will negotiate. The cheap way to
// know that width is to read it off the ENUMERATED GstDevice — structure 0 of
// gst_device_get_caps — because that is a query that opens nothing, costs
// nothing and is already being walked for the dropdown. Section 4.2 step 4 then
// keeps a "the provider lied" recovery path in reserve: open a throwaway
// <factory> ! fakesink, read the real width, rebuild the commentary capture.
//
// If the advertised count is reliable, that recovery path is a bounded fallback
// that will almost never run. If it is NOT reliable, it becomes the PRIMARY
// path — every device selection costs an extra open before the panel can be
// drawn — and it has to be built first. Nothing downstream of this file can be
// designed until the question is answered with a measurement.
//
// # What is measured, per device
//
//  1. ADVERTISED: structure 0's channels and channel-mask from the enumerated
//     GstDevice's caps. No device is opened.
//  2. NEGOTIATED, section 4.2 step 4's own shape: <captureSourceFactory> !
//     fakesink, PLAYING, the source pad's CURRENT caps. This is the exact probe
//     the recovery path would run, so measuring it here also measures whether
//     that path would give the right answer if it ever ran.
//  3. NEGOTIATED AT THE PAD THE PLAN ACTUALLY CONFIRMS AGAINST: the real
//     C-NATIVE head down to the seam caps, with the mix-matrix written in NULL
//     at the ADVERTISED width — section 4.2 steps 2 and 3, run in order. This
//     is the one that matters. A matrix pins audioconvert's sink caps to its own
//     width, so if the advertised number were wrong this probe does not
//     negotiate a different count, it FAILS TO PREROLL. That is section 4.2's
//     claim, and it is only worth resting on if it has been run.
//
// Probe 2 alone would be a weaker test than it looks: a fakesink accepts
// anything, so it reports what the source PREFERS with nothing downstream
// asking for anything. Probe 3 is what the shipping pipeline does.
//
// # The card is enumerated here and never opened
//
// The UltraStudio reaches this file TWICE, under two providers:
//
//	decklink   persistent-id=2747401380, channels={ 2, 8, 16 }   NEVER OPENED
//	CoreAudio  unique-id=90:a3c204a4:00000000:Audio, channels=16  opened
//
// The decklink entry is recorded and deliberately left shut: the card is
// exclusive and the decklink elements are not this step's to touch. Its
// advertised caps are still worth having, because they are the evidence for
// section 4.2 step 1's "for a DeckLink audio id the width is the existing
// constant 16, by construction" — see the assertion below, which runs the
// SHIPPING fixedChannelCount over them rather than eyeballing the string.
//
// connection is set on nothing in this file. There is no decklink element in
// it at all.
package gst

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// widthProbeStateTimeout bounds every state change. It is "the device is
// wedged", not "this machine is slow": measured, the slowest of the four
// CoreAudio devices on this machine reached PLAYING in well under a second.
const widthProbeStateTimeout = 8 * time.Second

// widthProbeCapsTimeout bounds the wait for CURRENT caps to appear after
// PLAYING. Current caps exist once the first CAPS event has crossed the pad,
// which for a live source is the first buffer — so this is a small multiple of
// one CoreAudio buffer (200 ms by default, per deviceprovider_darwin.go).
const widthProbeCapsTimeout = 3 * time.Second

const widthProbePollInterval = 20 * time.Millisecond

// widthProbeFakesinkDescription is PLAN.md section 4.2 step 4's recovery probe,
// written the way that section describes it and no other way. sync=false
// async=false on the sink so that a live source's timestamps cannot stall it.
const widthProbeFakesinkDescription = captureSourceFactory + " name=" + nameAudioSrc +
	" ! fakesink name=widthprobesink sync=false async=false"

// widthProbeCaptureHeadDescription is PLAN.md section 2.4's C-NATIVE chain from
// the source down to the seam caps, with the proxy tail replaced by a fakesink.
// Everything above the tail is character for character what the commentary
// capture will run, because the pad this probe reads — aconv:sink — is the pad
// section 4.2 step 3 confirms the width against in production.
const widthProbeCaptureHeadDescription = captureSourceFactory + " name=" + nameAudioSrc +
	" ! audioconvert" +
	" ! level name=chlevel interval=100000000 post-messages=false" +
	" ! audioconvert name=" + nameAudioConv +
	" ! audioresample" +
	" ! audio/x-raw,format=S16LE,rate=48000,channels=2,layout=interleaved" +
	" ! fakesink name=widthprobesink sync=false async=false"

// builtInMicrophoneUniqueID is the CoreAudio unique-id of the Mac's own
// microphone. It is a stable identity (deviceprovider_darwin.go's whole
// argument), so naming it is safe in a way that naming an AudioDeviceID integer
// would not be. A machine without it skips the mono half of the measurement
// rather than guessing, because there are two one-channel devices here and
// picking the wrong one would answer a different question.
const builtInMicrophoneUniqueID = "BuiltInMicrophoneDevice"

// continuityDeviceNames are the devices this test ENUMERATES BUT NEVER OPENS.
//
// ======================= A DEVICE LIST IS NOT A GUEST LIST ==================
//
// Enumerating is harmless. OPENING is not, and this machine is a rig in a room
// rather than a test bench. "Sam's iPhone Microphone" is a Continuity device:
// the phone is IN A DIFFERENT ROOM from the laptop and the commentary mic, and
// opening it plays the Continuity connection chime out of the phone, at the
// phone's volume, wherever the phone happens to be.
//
// The operator heard exactly that on 2026-08-16 while this test's first version
// was being written: "one of your sub agents has got confused, it's trying to
// use my phone as an audio device, and is playing those chimes out my phone
// which is in a different room to the mic."
//
// The version that did it swept every CoreAudio device and called the extras
// "free — the population is four". They are not free. They are somebody's
// phone. The sweep is still worth having, because a rule about providers is
// worth more the more devices it has survived, so the fix is a named exclusion
// and a loud log line rather than an allow-list of two.
//
// Matched against the device NAME because that is what carries the identity —
// a Continuity device's unique-id is an anonymous UUID, and so is NDI Audio's,
// which is a virtual device that is perfectly safe to open. There is no
// property in the enumerated caps that separates them.
var continuityDeviceNames = []string{"iPhone", "iPad", "Apple Watch"}

// isContinuityDevice reports whether this is a device that lives somewhere else
// and makes a noise when opened. See continuityDeviceNames.
func isContinuityDevice(name string) bool {
	for _, n := range continuityDeviceNames {
		if strings.Contains(name, n) {
			return true
		}
	}
	return false
}

// channelMask is one channel-mask field as GStreamer serialised it, plus the
// fact of its absence — which is a different thing from 0x0 and must not be
// flattened into it. An absent mask means the caps do not speak about position
// at all; 0x0 means they do, and say there is none.
type channelMask struct {
	present bool
	raw     string
}

func (m channelMask) String() string {
	if !m.present {
		return "absent"
	}
	return m.raw
}

func (m channelMask) equal(o channelMask) bool {
	return m.present == o.present && m.raw == o.raw
}

// advertisedWidth is what one enumerated GstDevice says about itself, with
// nothing opened.
type advertisedWidth struct {
	name       string
	kind       DeviceKind
	id         string
	structures int
	channels   int  // structure 0's channels, when it is a fixed integer
	fixed      bool // whether it was
	channelsAs string
	mask       channelMask
	structure0 string

	// everyStructure is the channels field of EVERY structure, in order, and
	// it is here because the first run of this file showed why it has to be:
	// a device's caps routinely carry more than one channel count, and only
	// structure 0's is the one that negotiates. See
	// reportWholeCapsWidthIsNotTheAnswer.
	everyStructure []string

	// wholeCaps is what the SHIPPING fixedChannelCount answers when it is
	// pointed at the whole of the device's caps rather than at structure 0.
	wholeCaps    int
	wholeCapsErr error
}

// negotiatedWidth is what a pad's CURRENT caps say after PLAYING.
type negotiatedWidth struct {
	channels int
	mask     channelMask
	caps     string
	reached  time.Duration
}

func TestLiveAdvertisedChannelCountIsWhatNegotiates(t *testing.T) {
	liveInitDarwin(t)

	advertised := enumerateAdvertisedWidths(t)
	if len(advertised) == 0 {
		t.Fatal("no Audio/Source device was offered at all, so there is nothing to compare; " +
			"ListInputDevices would show an empty dropdown on this machine")
	}

	t.Log("---- ADVERTISED, from the enumerated GstDevice caps, nothing opened ----")
	for _, a := range advertised {
		t.Logf("  %-34q kind=%-8s structures=%d channels=%-16s mask=%s",
			a.name, a.kind, a.structures, a.channelsAs, a.mask)
		t.Logf("      id=%s", a.id)
		t.Logf("      structure 0: %s", a.structure0)
		t.Logf("      channels per structure: [%s]", strings.Join(a.everyStructure, " | "))
	}

	assertDeckLinkWidthIsNotAFixedAdvertisement(t, advertised)
	reportWholeCapsWidthIsNotTheAnswer(t, advertised)

	// The two devices the step names, plus every other CoreAudio device on the
	// machine. The extra ones are free — the population is four — and a rule
	// about providers is worth more the more devices it has survived.
	targets := nativeTargets(t, advertised)

	agree := 0
	disagree := 0
	for _, a := range targets.all {
		required := a.id == targets.monoID || a.id == targets.wideID
		if !probeOneDevice(t, a, required) {
			disagree++
			continue
		}
		agree++
	}

	t.Log("---- VERDICT ----")
	switch {
	case disagree == 0 && agree > 0:
		t.Logf("the advertised channel count equalled the negotiated count on all %d CoreAudio "+
			"device(s) opened, at BOTH the fakesink probe pad and aconv:sink with a matrix "+
			"sized from the advertisement. PLAN.md section 4.2's enumerated-caps read is sound "+
			"and the provider-lied path in step 4 stays a fallback.", agree)
	default:
		t.Errorf("%d of %d CoreAudio device(s) did not negotiate what they advertised. The "+
			"provider-lied recovery path in PLAN.md section 4.2 step 4 is the PRIMARY path and "+
			"must be built before the routing panel can be sized from enumeration.",
			disagree, agree+disagree)
	}
}

// TestLiveTheAdvertisedWidthIsOnlyTrueWithNoChannelCapsfilter is the BOUNDARY of
// the result above, and it is the half a green run would otherwise hide.
//
// The advertised count equalling the negotiated one is not a property of the
// device on its own. It is a property of the device WITH NOTHING DOWNSTREAM
// ASKING FOR ANYTHING ELSE: negotiation fixates on the first structure of the
// intersection, structure 0 is the device's current hardware format, and so
// structure 0 wins by default rather than by right. Put a channel capsfilter
// under the source and the source will happily produce a different count — its
// template caps offer several — and the routing panel's width, the matrix's
// width and the meter's width would all be sized from a number that is no
// longer what flows.
//
// PLAN.md section 2.4 already forbids a channel capsfilter on the source, on
// the separate evidence that FORCING a count silently drifted the format
// F32LE→S16LE and that channels=2,channel-mask=0x0 was refused outright. This
// test records that the same rule is now load-bearing for a SECOND reason —
// section 4.2's entire width discovery rests on it — so that a later reader who
// disposes of the first argument does not thereby dispose of the second without
// noticing.
//
// The measurement runs against the built-in microphone because it is the one
// device here whose template caps clearly offer a count other than structure
// 0's: three structures, channels 1, 2, 1.
func TestLiveTheAdvertisedWidthIsOnlyTrueWithNoChannelCapsfilter(t *testing.T) {
	liveInitDarwin(t)

	var mic advertisedWidth
	for _, a := range enumerateAdvertisedWidths(t) {
		if a.id == builtInMicrophoneUniqueID {
			mic = a
		}
	}
	if mic.id == "" {
		t.Skipf("no CoreAudio device with unique-id %q on this machine", builtInMicrophoneUniqueID)
	}
	if !mic.fixed {
		t.Skipf("%q does not advertise a fixed channel count", mic.name)
	}

	forced := ChannelMapOutputs
	if forced == mic.channels {
		t.Skipf("%q already advertises %d channels, so forcing that count would prove nothing",
			mic.name, forced)
	}

	desc := captureSourceFactory + " name=" + nameAudioSrc +
		" ! audio/x-raw,channels=" + strconv.Itoa(forced) +
		" ! fakesink name=widthprobesink sync=false async=false"
	t.Logf("forcing a count under %q, which advertises %d: %s", mic.name, mic.channels, desc)

	got, err := openAndReadPad(t, desc, mic.id, nameAudioSrc, "src", nil)
	if err != nil {
		t.Logf("with channels=%d asked for, %q would not run at all: %v", forced, mic.name, err)
		t.Logf("BOUNDARY: this device cannot be forced off its advertised width; the equality " +
			"above is a property of the device rather than of the graph")
		return
	}

	if got.channels == mic.channels {
		t.Logf("BOUNDARY: %q negotiated its advertised %d channels even with channels=%d asked "+
			"for, so the advertisement survives a downstream constraint here", mic.name,
			got.channels, forced)
		return
	}

	t.Logf("BOUNDARY CONFIRMED: %q advertises %d channels and negotiated %d when a capsfilter "+
		"asked for %d — caps %s", mic.name, mic.channels, got.channels, forced, got.caps)
	t.Logf("So the enumerated width is what negotiates ONLY because PLAN.md section 2.4 puts no " +
		"channel capsfilter under the capture source. That rule is now load-bearing for section " +
		"4.2's width discovery as well as for the format drift it was written against; adding a " +
		"channel capsfilter would silently de-synchronise the routing panel from the stream.")
}

// TestLiveAWrongMatrixWidthFailsLoudly is the safety net under this step's
// verdict, and it is the reason that verdict can be "the recovery path stays a
// fallback" rather than "the recovery path is unnecessary".
//
// Saying the advertised width is reliable ON THIS MACHINE, TODAY, is not the
// same as saying a wrong one could never reach the matrix — a device the
// operator has not plugged in yet, a CoreAudio update, an aggregate device
// built in Audio MIDI Setup. What makes that survivable is PLAN.md section 4.2
// step 4's claim about the FAILURE MODE:
//
//	"With a matrix set, audioconvert pins its sink caps to the matrix width, so
//	 a wrong width shows up as a preroll failure, not as a different negotiated
//	 number."
//
// If that is true, a wrong width is loud, off air, and recoverable in one extra
// open. If it were false — if audioconvert quietly negotiated some other count
// and mixed against a matrix built for a different one — a wrong width would be
// a silent wrong-routing failure with every lamp green, which is the class this
// codebase is organised against. The whole "fallback" ruling rests on this
// sentence, so the sentence gets run.
//
// It is measured against the widest device available, with a matrix built for
// HALF its channels.
func TestLiveAWrongMatrixWidthFailsLoudly(t *testing.T) {
	liveInitDarwin(t)

	advertised := enumerateAdvertisedWidths(t)
	targets := nativeTargets(t, advertised)
	if targets.wideID == "" {
		t.Skip("no multichannel CoreAudio device on this machine, so a wrong width cannot be posed")
	}

	var wide advertisedWidth
	for _, a := range advertised {
		if a.id == targets.wideID {
			wide = a
		}
	}

	wrong := wide.channels / 2
	if wrong < 1 || wrong == wide.channels {
		t.Skipf("%q advertises %d channels, from which no wrong width can be built",
			wide.name, wide.channels)
	}

	t.Logf("%q negotiates %d channels; writing a %d-wide matrix instead",
		wide.name, wide.channels, wrong)

	got, err := openAndReadPad(t, widthProbeCaptureHeadDescription, wide.id,
		nameAudioConv, "sink", writeProbeMatrix(wrong))
	if err != nil {
		t.Logf("LOUD, as section 4.2 step 4 requires: %v", err)
		t.Logf("A wrong width is therefore a preroll failure off air, not a silent mis-route. " +
			"That is what makes the enumerated-caps read safe to use as the primary path with " +
			"the probe kept in reserve.")
		return
	}

	// Reaching here means the graph ran with a matrix of the wrong width. The
	// only tolerable version of that is audioconvert having pinned the source
	// down to the matrix's width — the stream is then narrower than the device,
	// which is wrong but VISIBLE, because the width published to the UI is read
	// from this same pad. Anything else is the silent case.
	if got.channels == wrong {
		t.Errorf("%q negotiated %d channels against a %d-wide matrix — audioconvert pinned the "+
			"source to the MATRIX's width rather than refusing. A wrong advertised width would "+
			"then narrow the capture silently instead of failing, and the routing panel would be "+
			"drawn from a number nothing checked. Caps: %s",
			wide.name, got.channels, wrong, got.caps)
		return
	}
	t.Errorf("SILENT: %q negotiated %d channels while the matrix was built for %d. Section 4.2 "+
		"step 4's detection mechanism does not hold, so a wrong width would mix against a matrix "+
		"of another shape with nothing reporting a fault. Caps: %s",
		wide.name, got.channels, wrong, got.caps)
}

// probeOneDevice runs both probes against one device and reports whether the
// advertisement held. required says whether a failure to open is itself a
// failure of the step: the two devices step 0c names must open, and a device
// that merely happens to be plugged in need not.
func probeOneDevice(t *testing.T, a advertisedWidth, required bool) bool {
	t.Helper()

	fail := t.Logf
	if required {
		fail = t.Errorf
	}

	if !a.fixed {
		fail("%q advertises %s, which is not a fixed channel count, so there is no width to size "+
			"a routing panel from without opening it", a.name, a.channelsAs)
		return false
	}

	// PROBE A — section 4.2 step 4's own recovery probe.
	viaFakesink, err := openAndReadPad(t, widthProbeFakesinkDescription, a.id,
		nameAudioSrc, "src", nil)
	if err != nil {
		fail("%q: the <factory> ! fakesink probe could not be run: %v", a.name, err)
		return false
	}

	// PROBE B — section 4.2 steps 2 and 3 in order: matrix written in NULL at
	// the ADVERTISED width, then the width confirmed at aconv:sink after
	// PLAYING. A wrong advertisement shows up here as a preroll failure, which
	// is precisely the claim being tested.
	viaCaptureHead, err := openAndReadPad(t, widthProbeCaptureHeadDescription, a.id,
		nameAudioConv, "sink", writeProbeMatrix(a.channels))
	if err != nil {
		fail("%q: the real C-NATIVE head with a %d-wide mix-matrix would not run: %v. "+
			"A matrix pins audioconvert's sink caps to its own width, so this is what a wrong "+
			"advertised count looks like", a.name, a.channels, err)
		return false
	}

	ok := true
	if viaFakesink.channels != a.channels {
		fail("%q: advertised channels=%d but %s ! fakesink negotiated channels=%d",
			a.name, a.channels, captureSourceFactory, viaFakesink.channels)
		ok = false
	}
	if viaCaptureHead.channels != a.channels {
		fail("%q: advertised channels=%d but %s:sink negotiated channels=%d",
			a.name, a.channels, nameAudioConv, viaCaptureHead.channels)
		ok = false
	}
	if !viaFakesink.mask.equal(a.mask) {
		// A mask difference is reported and is NOT a failure of this step. The
		// width is what sizes the panel and what sizes the matrix; the mask
		// only says whether the stream is positioned, and section 4.1 already
		// settles that from GStreamer's source — gstosxcoreaudio.c:886-889 sets
		// layout = NULL for every source unconditionally — rather than from
		// what any one device happens to publish.
		t.Logf("  NOTE %q: advertised mask %s, negotiated %s at the source pad",
			a.name, a.mask, viaFakesink.mask)
	}

	t.Logf("  %-34q advertised %2d ch %-20s | fakesink %2d ch %-20s in %-8s | %s:sink %2d ch %-20s in %-8s | %s",
		a.name, a.channels, a.mask,
		viaFakesink.channels, viaFakesink.mask, viaFakesink.reached.Round(time.Millisecond),
		nameAudioConv, viaCaptureHead.channels, viaCaptureHead.mask,
		viaCaptureHead.reached.Round(time.Millisecond),
		map[bool]string{true: "AGREE", false: "DISAGREE"}[ok])
	t.Logf("      fakesink caps:    %s", viaFakesink.caps)
	t.Logf("      %s:sink caps: %s", nameAudioConv, viaCaptureHead.caps)
	return ok
}

// openAndReadPad builds one throwaway probe pipeline, points its source at
// deviceID through the SHIPPING configureCaptureSource — so the unique-id is
// resolved to the current AudioDeviceID exactly as Start would resolve it —
// takes it to PLAYING and reads the named pad's CURRENT caps.
//
// prepare runs after the device is set and BEFORE any state change, which is
// where a mix-matrix has to be written: it is a negotiation constraint, not a
// gain.
//
// Everything that can go wrong returns an error rather than failing the test,
// because "this device would not open" is an observation about one device and
// the caller decides whether it is fatal to the step.
func openAndReadPad(t *testing.T, desc, deviceID, element, padName string,
	prepare func(gogst.Pipeline) error) (negotiatedWidth, error) {
	t.Helper()

	parsed, err := gogst.ParseLaunch(desc)
	if err != nil {
		return negotiatedWidth{}, fmt.Errorf("gst_parse_launch: %w", err)
	}
	pipeline, ok := parsed.(gogst.Pipeline)
	if !ok {
		return negotiatedWidth{}, fmt.Errorf("gst_parse_launch returned a %T, not a GstPipeline", parsed)
	}
	// The device is closed on every path out of here, including the error
	// paths. A probe that leaked an open CoreAudio device would make the next
	// probe in the loop measure contention rather than negotiation.
	defer pipeline.BlockSetState(gogst.StateNull, gogst.ClockTime(widthProbeStateTimeout))

	bus := &busRecorder{}
	if b := pipeline.GetBus(); b != nil {
		b.SetSyncHandler(bus.handler)
	}

	src := pipeline.GetByName(nameAudioSrc)
	if src == nil {
		return negotiatedWidth{}, errors.New("the probe pipeline has no " + nameAudioSrc)
	}
	if err := configureCaptureSource(src, deviceID); err != nil {
		return negotiatedWidth{}, fmt.Errorf("configureCaptureSource: %w", err)
	}
	if prepare != nil {
		if err := prepare(pipeline); err != nil {
			return negotiatedWidth{}, err
		}
	}

	started := time.Now()
	if ret := pipeline.BlockSetState(gogst.StatePlaying,
		gogst.ClockTime(widthProbeStateTimeout)); !stateChangeOK(ret) {
		errs, _ := bus.snapshot()
		return negotiatedWidth{}, fmt.Errorf("would not reach PLAYING (%s); bus errors: %v",
			ret, describeBus(errs, started))
	}

	el := pipeline.GetByName(element)
	if el == nil {
		return negotiatedWidth{}, errors.New("the probe pipeline has no element named " + element)
	}
	pad := el.GetStaticPad(padName)
	if pad == nil {
		return negotiatedWidth{}, fmt.Errorf("%s has no %s pad", element, padName)
	}

	deadline := time.Now().Add(widthProbeCapsTimeout)
	for {
		if caps := pad.GetCurrentCaps(); caps != nil && caps.GetSize() > 0 {
			s := caps.GetStructure(0)
			if s == nil {
				return negotiatedWidth{}, fmt.Errorf("%s:%s published %s, whose first structure is nil",
					element, padName, caps.String())
			}
			n, okInt := s.GetInt("channels")
			if !okInt {
				return negotiatedWidth{}, fmt.Errorf("%s:%s negotiated %s, in which channels is not a "+
					"fixed integer", element, padName, caps.String())
			}
			return negotiatedWidth{
				channels: int(n),
				mask:     maskOf(s),
				caps:     caps.String(),
				reached:  time.Since(started),
			}, nil
		}
		if time.Now().After(deadline) {
			errs, warns := bus.snapshot()
			return negotiatedWidth{}, fmt.Errorf("%s:%s had no current caps %s after PLAYING; "+
				"bus errors: %v; bus warnings: %v",
				element, padName, widthProbeCapsTimeout, describeBus(errs, started),
				describeBus(warns, started))
		}
		time.Sleep(widthProbePollInterval)
	}
}

// writeProbeMatrix returns PLAN.md section 4.2 step 2 as a prepare hook: the
// mix-matrix, sized to the ADVERTISED width, written while the pipeline is
// still in NULL.
//
// It writes the same string the shipping writeChannelMapLocked writes, through
// the same gst_util_set_object_arg, from the same ChannelMap.MixMatrix — a
// probe that wrote its own matrix by hand would be testing a matrix nothing
// ships.
func writeProbeMatrix(width int) func(gogst.Pipeline) error {
	return func(pipeline gogst.Pipeline) error {
		aconv := pipeline.GetByName(nameAudioConv)
		if aconv == nil {
			return errors.New("the probe pipeline has no " + nameAudioConv)
		}
		matrix, err := DefaultChannelMap(width).MixMatrix(width)
		if err != nil {
			return fmt.Errorf("building a %d-wide matrix: %w", width, err)
		}
		if !hasProperty(aconv, propMixMatrix) {
			return errors.New("this build's audioconvert has no " + propMixMatrix + " property")
		}
		gogst.UtilSetObjectArg(aconv, propMixMatrix, mixMatrixArg(matrix))
		return nil
	}
}

// enumerateAdvertisedWidths walks the SAME population ListInputDevices offers —
// same class filter, same captureDeviceID seam — and records what each device
// says about its own channel layout. Nothing is opened.
//
// It walks the devices rather than calling ListInputDevices because Device does
// not carry Channels yet; that field is step 3, and this step is what decides
// whether it is worth having.
func enumerateAdvertisedWidths(t *testing.T) []advertisedWidth {
	t.Helper()

	var out []advertisedWidth
	for _, dev := range enumerateDevices("Audio/Source") {
		if dev == nil || !dev.HasClasses("Audio/Source") {
			continue
		}
		props := dev.GetProperties()
		if props == nil {
			continue
		}
		id, kind, offer := captureDeviceID(dev, props)
		if !offer {
			continue
		}
		a := advertisedWidth{
			name: dev.GetDisplayName(),
			kind: NormaliseDeviceKind(kind),
			id:   id,
		}
		caps := dev.GetCaps()
		if caps == nil {
			a.channelsAs = "(no caps)"
			out = append(out, a)
			continue
		}
		a.structures = int(caps.GetSize())
		for i := uint(0); i < caps.GetSize(); i++ {
			a.everyStructure = append(a.everyStructure, serialisedField(caps.GetStructure(i), "channels"))
		}
		a.wholeCaps, a.wholeCapsErr = fixedChannelCount(caps)
		s := caps.GetStructure(0)
		if s == nil {
			a.channelsAs = "(no structure 0)"
			out = append(out, a)
			continue
		}
		a.structure0 = s.String()
		a.channelsAs = serialisedField(s, "channels")
		if a.channelsAs == "" {
			a.channelsAs = "(absent)"
		}
		if n, ok := s.GetInt("channels"); ok {
			a.channels, a.fixed = int(n), true
		}
		a.mask = maskOf(s)
		out = append(out, a)
	}
	return out
}

// nativeSelection is which of the enumerated devices this step opens, and which
// two of them it insists on.
type nativeSelection struct {
	all    []advertisedWidth
	monoID string // the built-in microphone
	wideID string // the widest CoreAudio device, the 16-in one on this machine
}

func nativeTargets(t *testing.T, advertised []advertisedWidth) nativeSelection {
	t.Helper()

	sel := nativeSelection{}
	widest := 0
	for _, a := range advertised {
		if a.kind != KindNative {
			continue
		}
		// Enumerated above, deliberately not opened. See continuityDeviceNames.
		if isContinuityDevice(a.name) {
			t.Logf("NOT OPENING %q: it is a Continuity device, it is in a different room, and "+
				"opening it chimes out of the operator's phone. Its advertised caps are in the "+
				"table above and that is all this test wants from it.", a.name)
			continue
		}
		sel.all = append(sel.all, a)
		if a.id == builtInMicrophoneUniqueID {
			sel.monoID = a.id
		}
		if a.fixed && a.channels > widest {
			widest, sel.wideID = a.channels, a.id
		}
	}

	if sel.monoID == "" {
		t.Logf("NOTE: no CoreAudio device with unique-id %q on this machine, so the one-channel "+
			"half of step 0c is not being asserted", builtInMicrophoneUniqueID)
	}
	// Step 0c's second named target is a MULTICHANNEL device. A machine whose
	// widest CoreAudio input is stereo cannot answer the question the routing
	// panel asks, and saying so is worth more than a green run that proved
	// nothing about widths above 2.
	if widest <= 2 {
		t.Logf("NOTE: the widest CoreAudio input on this machine is %d channels, so this run says "+
			"nothing about a multichannel device and the >2 case is UNMEASURED here", widest)
		sel.wideID = ""
	}
	return sel
}

// assertDeckLinkWidthIsNotAFixedAdvertisement runs the SHIPPING
// fixedChannelCount over the decklink provider's advertised caps, without
// opening the card.
//
// PLAN.md section 4.2 step 1 says "for a DeckLink audio id the width is the
// existing constant 16, by construction". This is the evidence for that clause
// rather than a restatement of it: if the card's enumerated caps carry a CHOICE
// of channel counts, then there is nothing to read and the constant is
// mandatory. If a future GStreamer starts publishing a fixed count, that is a
// change worth being told about — and if it ever publishes a fixed count that
// is not 16, the constant has become a lie and this fails.
func assertDeckLinkWidthIsNotAFixedAdvertisement(t *testing.T, advertised []advertisedWidth) {
	t.Helper()

	for _, a := range advertised {
		if a.kind != KindDeckLink {
			continue
		}
		width, err := fixedChannelCountOfAdvertised(a)
		switch {
		case err != nil:
			t.Logf("the decklink provider advertises %q as %s — no fixed count, so section 4.2 "+
				"step 1's constant %d is mandatory rather than a convenience (%v)",
				a.name, a.channelsAs, deckLinkAudioChannels, err)
		case width == deckLinkAudioChannels:
			t.Logf("the decklink provider now advertises %q as a fixed %d channels, which happens "+
				"to equal the constant; the constant is still what is used", a.name, width)
		default:
			t.Errorf("the decklink provider advertises %q as a fixed %d channels while this build "+
				"opens it at the constant %d. One of the two is wrong and the card is where a "+
				"wrong width stops the capture chain", a.name, width, deckLinkAudioChannels)
		}
	}
}

// reportWholeCapsWidthIsNotTheAnswer records the trap the first run of this
// file walked into, so that step 3 does not.
//
// PLAN.md section 4.2 step 1 says the width is read "from structure 0 of the
// enumerated GstDevice's caps", and the obvious way to implement that sentence
// is to reach for the function this package already has for reading a channel
// count out of caps — fixedChannelCount. THAT WOULD BE WRONG, and it would be
// wrong on the commonest device in the building.
//
// fixedChannelCount walks EVERY structure and refuses when they disagree,
// because it was written for a PAD's caps, where a disagreement means "nobody
// has decided yet" and a matrix built on a guess stops the capture chain. A
// DEVICE's caps are a different document: osxaudiodeviceprovider publishes the
// device's CURRENT hardware format as structure 0 and then the element's
// template caps after it, so disagreement is normal and structure 0 is the
// answer. Measured on this machine:
//
//	MacBook Pro Microphone   3 structures, channels 1, 2, 1   -> fixedChannelCount REFUSES
//	NDI Audio                3 structures, channels 2, 2, 1   -> fixedChannelCount REFUSES
//	Blackmagic (CoreAudio)   2 structures, channels 16, 16    -> 16
//
// and in every one of those cases structure 0 is what the pad went on to
// negotiate. A step-3 implementation that reused fixedChannelCount would
// therefore report "no width" for the built-in microphone and for NDI — the
// panel would be correctly hidden by luck, since both are ≤2, and the first
// multichannel device whose template caps differ from its current format would
// be the one that broke.
//
// The assertion here is the contradiction check, which is the part that stays
// true on a machine with other hardware: whenever fixedChannelCount does
// succeed over the whole caps, it must not disagree with structure 0.
func reportWholeCapsWidthIsNotTheAnswer(t *testing.T, advertised []advertisedWidth) {
	t.Helper()

	for _, a := range advertised {
		switch {
		case a.wholeCapsErr != nil:
			t.Logf("  %q: reading the WHOLE caps with fixedChannelCount refuses (%v); structure 0 "+
				"says %s. Step 3 must read structure 0 and must not reuse fixedChannelCount here",
				a.name, a.wholeCapsErr, a.channelsAs)
		case a.fixed && a.wholeCaps != a.channels:
			t.Errorf("  %q: structure 0 advertises %d channels and the whole caps fix %d. Two "+
				"different numbers are readable from one document and only one of them can size "+
				"the matrix", a.name, a.channels, a.wholeCaps)
		}
	}
}

// fixedChannelCountOfAdvertised is fixedChannelCount's question asked of an
// already-recorded advertisement, so that the caps object does not have to be
// carried around.
func fixedChannelCountOfAdvertised(a advertisedWidth) (int, error) {
	if !a.fixed {
		return 0, fmt.Errorf("channels is %s, which fixes no single count", a.channelsAs)
	}
	return a.channels, nil
}

// maskOf reads a channel-mask field, keeping the distinction between "there is
// no such field" and "the field says 0".
//
// It goes through the SERIALISED form rather than a typed getter because
// channel-mask is a GST_TYPE_BITMASK, not a G_TYPE_UINT64:
// gst_structure_get_uint64 does not hold for it and would answer false for a
// mask that is plainly there.
func maskOf(s *gogst.Structure) channelMask {
	if s == nil || !s.HasField("channel-mask") {
		return channelMask{}
	}
	raw := stripTypeTag(serialisedField(s, "channel-mask"))
	// Normalised to a fixed width so that 0x0 and 0x0000000000000000 compare
	// equal — they are the same mask written by two different serialisers.
	if v, err := strconv.ParseUint(strings.TrimPrefix(raw, "0x"), 16, 64); err == nil {
		return channelMask{present: true, raw: fmt.Sprintf("0x%016x", v)}
	}
	return channelMask{present: true, raw: raw}
}

// serialisedField returns one field's value exactly as GStreamer wrote it,
// including the shapes no typed getter can return — "{ 2, 8, 16 }", "[ 1, 32 ]"
// — which are the shapes this step exists to notice.
//
// It scans the serialised structure rather than reflecting over GValues because
// the interesting cases are precisely the ones whose GValue has no Go binding
// in go-gst v0.0.2 (GST_TYPE_LIST, GST_TYPE_INT_RANGE, GST_TYPE_BITMASK).
func serialisedField(s *gogst.Structure, field string) string {
	if s == nil {
		return ""
	}
	text := s.String()
	needle := field + "="
	for i := 0; i+len(needle) <= len(text); i++ {
		if !strings.HasPrefix(text[i:], needle) {
			continue
		}
		// Only at a field boundary, or "channel-mask=" would be found inside
		// nothing and "mask=" would be found inside "channel-mask=".
		if i > 0 && text[i-1] != ' ' && text[i-1] != ',' {
			continue
		}
		return fieldValueEnd(text[i+len(needle):])
	}
	return ""
}

// fieldValueEnd cuts a serialised value at the first comma that is not inside a
// bracket of some kind. The "(int)" type tag counts as a bracket, which is what
// makes "channels=(int){ 2, 8, 16 }, rate=(int)48000" split in the right place.
//
// The ';' terminator matters as much as the comma and was missed once. A
// structure serialises as "...channels=(int)1;", so the LAST field of a
// structure comes back with the terminator attached — and a channel-mask that
// happens to be last then reads "0x0000000000000003;", which parses as no
// number at all, silently skips the normalisation below it and compares unequal
// to the identical mask read from a pad where the field was not last.
func fieldValueEnd(rest string) string {
	depth := 0
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '{', '[', '<', '(':
			depth++
		case '}', ']', '>', ')':
			if depth > 0 {
				depth--
			}
		case ',', ';':
			if depth == 0 {
				return strings.TrimSpace(rest[:i])
			}
		}
	}
	return strings.TrimSpace(rest)
}

// stripTypeTag removes a leading GStreamer type tag — "(bitmask)0x3" becomes
// "0x3" — leaving the value alone when there is none.
func stripTypeTag(v string) string {
	if strings.HasPrefix(v, "(") {
		if j := strings.IndexByte(v, ')'); j >= 0 {
			return strings.TrimSpace(v[j+1:])
		}
	}
	return v
}
