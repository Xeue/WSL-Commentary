//go:build !cgo || gststub

// capturefault_test.go is the Gate A cover for the decision that decides
// whether the commentary goes off air.
//
// It runs with no GStreamer installed, which is the entire reason
// capturefault.go carries no build tag: the classification and the three-way
// diagnosis are the two pieces of this feature whose failure modes are silent,
// and neither of them should be reachable only on a machine with a capture card
// in it.

package gst

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestClassifyBusError(t *testing.T) {
	cases := []struct {
		name   string
		source string
		legs   captureLegs
		want   busErrorClass
	}{
		{
			// Unchanged, and asserted first because it is the only path that is
			// on air today. Whatever else is added to this pipeline, an
			// srtout-N error must still be the sender's to retry.
			name: "the sink", source: "srtout-7", want: classSinkSourced,
		},
		{
			name: "the queue in front of the sink", source: "srtq", want: classSinkSourced,
		},
		{
			name: "the commentary capture", source: "asrc", want: classAudioCapture,
		},
		{
			// The regression this whole file exists to prevent: today a video
			// capture error takes the commentary off air, and it must not.
			name: "the video capture", source: "vcapsrc", want: classVideoCapture,
		},
		{
			// The conform chain fails as one unit with the source it is
			// conforming, which is why the test is a PREFIX and not a set of
			// names that would have to be kept in step.
			name: "the conform chain", source: "vcapscale", want: classVideoCapture,
		},
		{
			// An element of the video leg that does not exist yet. The prefix
			// has to cover it, because this filter must be right on the day the
			// capture leg lands and not merely on the day it was written.
			name: "a video capture element nobody has added yet", source: "vcapdeint", want: classVideoCapture,
		},
		{
			// THE case that looks like an exception. decklinkaudiosrc cannot
			// preroll without a decklinkvideosrc in the same pipeline — the
			// card drives audio capture off the video clock — so with the
			// commentary on the card, an error from the video element is an
			// AUDIO fault and treating it as recoverable would leave a
			// connected sender pushing silence with every lamp green.
			name:   "the video capture when it is clocking the commentary",
			source: "vcapsrc", legs: captureLegs{AudioClockedByVideo: true}, want: classAudioCapture,
		},
		{
			name:   "the clock companion, which only exists in that configuration",
			source: "vcapclock", legs: captureLegs{AudioClockedByVideo: true}, want: classAudioCapture,
		},
		{
			// The confidence monitor. Spared outright, and the next case is why it
			// cannot be classVideoCapture.
			name: "the preview sink", source: "vprevsink", want: classPreview,
		},
		{
			// THE CASE THE SEPARATE CLASS EXISTS FOR. With the commentary clocked by
			// the card's video a classVideoCapture error becomes classAudioCapture and
			// takes the feed off air. A preview must be spared here too — this is the
			// configuration it was built for.
			name:   "the preview sink, with the commentary clocked by the card's video",
			source: "vprevscale", legs: captureLegs{AudioClockedByVideo: true}, want: classPreview,
		},
		{
			// The slate leg is NOT video capture. filesrc/pngdec/imagefreeze
			// cannot fail after Start, so an error from one is a surprise and
			// stays fatal, which is what it is today.
			name: "the slate", source: "slate", want: classFatal,
		},
		{name: "the muxer", source: "mux", want: classFatal},
		{name: "the encoder", source: "venc", want: classFatal},
		{name: "the pipeline itself", source: "pipeline", want: classFatal},
		{
			// A name that merely CONTAINS the prefix is not the video leg. The
			// test is HasPrefix and this pins it, because a substring test here
			// would spare an element nobody meant to spare.
			name: "a name that only contains the prefix", source: "queue-vcap", want: classFatal,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyBusError(tc.source, tc.legs); got != tc.want {
				t.Errorf("classifyBusError(%q, %+v) = %d, want %d", tc.source, tc.legs, got, tc.want)
			}
		})
	}
}

func TestClassifyBusErrorSparesTheSinkInEveryConfiguration(t *testing.T) {
	// The sink test is first for a reason and this is that reason asserted:
	// no combination of capture legs may make a sink error fatal, because the
	// whole reconnect ladder is built on it not being.
	for _, legs := range []captureLegs{{}, {AudioClockedByVideo: true}} {
		for _, source := range []string{"srtq", "srtout-1", "srtout-9999"} {
			if got := classifyBusError(source, legs); got != classSinkSourced {
				t.Errorf("classifyBusError(%q, %+v) = %d, want classSinkSourced", source, legs, got)
			}
		}
	}
}

func TestDiagnoseCaptureFault(t *testing.T) {
	// The measured contention signature: the two texts arrive together and it
	// is the second that carries the information.
	const contention = "Internal data stream error. (gstbasesrc.c(3187): gst_base_src_loop (): " +
		"streaming stopped, reason not-negotiated (-4))"

	// The measured CASCADE signature — BUILD-NOTES.md section 8.6 — where asrc
	// posted the first half of that text on a pipeline whose device was
	// perfectly healthy, because srtsink had failed underneath it. It must not
	// be read as contention.
	const cascade = "Internal data stream error. (gstbasesrc.c(3187): gst_base_src_loop ())"

	cases := []struct {
		name string
		ev   captureEvidence
		want captureFault
	}{
		{
			// The strongest evidence there is: the operating system was asked
			// and ours is not in the list. It needs no help from the text.
			name: "the enumeration says it is gone",
			ev: captureEvidence{
				EnumerationOK: true, Enumerated: false, DeviceCount: 4, Message: contention,
			},
			want: faultDeviceMissing,
		},
		{
			// An enumeration that FAILED is not evidence the device is gone,
			// and reading it as such would blame a Thunderbolt cable for a
			// device-monitor hiccup.
			name: "the enumeration could not be done",
			ev:   captureEvidence{EnumerationOK: false, Enumerated: false, Message: contention},
			want: faultDeviceBusy,
		},
		{
			name: "the enumeration found it and the card has no signal",
			ev: captureEvidence{
				EnumerationOK: true, Enumerated: true, Signal: triFalse, DeckLink: true, Message: cascade,
			},
			want: faultNoSignal,
		},
		{
			// triUnknown is every non-DeckLink source: there is no signal
			// property to ask, and that is not evidence of anything.
			name: "there was no signal property to ask",
			ev:   captureEvidence{Signal: triUnknown, Message: contention},
			want: faultDeviceBusy,
		},
		{
			name: "the card is locked and something else has the device",
			ev:   captureEvidence{Signal: triTrue, DeckLink: true, Message: contention},
			want: faultDeviceBusy,
		},
		{
			// THE discrimination this file turns on. Same first sentence,
			// different second, opposite meaning.
			name: "a cascade from the sink is not contention",
			ev:   captureEvidence{Message: cascade},
			want: faultUnknown,
		},
		{
			name: "the element says the device has gone",
			ev:   captureEvidence{Message: "Resource not found. (gstosxaudiosrc.c: device 74 disappeared)"},
			want: faultDeviceMissing,
		},
		{
			// Missing is diagnosed BEFORE busy, because a device that has been
			// removed also fails to negotiate and the two fixes are different.
			name: "gone AND unnegotiable reads as gone",
			ev:   captureEvidence{Message: "Resource not found; not-negotiated (-4)"},
			want: faultDeviceMissing,
		},
		{
			name: "e_accessdenied",
			ev:   captureEvidence{Message: "Could not open device (E_ACCESSDENIED)", DeckLink: true},
			want: faultDeviceBusy,
		},
		{
			// Nothing matched. This is a designed outcome, not a hole: the
			// message says the three things to check rather than choosing one
			// at random.
			name: "nothing matched",
			ev:   captureEvidence{Message: "some future GStreamer wording"},
			want: faultUnknown,
		},
		{
			name: "no evidence at all",
			ev:   captureEvidence{},
			want: faultUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := diagnoseCaptureFault(tc.ev); got != tc.want {
				t.Errorf("diagnoseCaptureFault(%+v) = %d, want %d", tc.ev, got, tc.want)
			}
		})
	}
}

func TestCaptureFaultMessageIsActionable(t *testing.T) {
	// Every message has one job: an operator twenty minutes before kick-off
	// must be able to read it and know what to DO. These assertions are on the
	// substance and not the wording — each one names the fix that is peculiar
	// to that fault and could not be given for either of the other two.
	ev := captureEvidence{
		DeviceName: "Blackmagic UltraStudio 4K Mini",
		DeckLink:   true,
		HWSerial:   "SN-1234",
		Connection: "sdi",
		Message:    "not-negotiated (-4)",
	}

	cases := []struct {
		fault captureFault
		must  []string
	}{
		{faultDeviceMissing, []string{"no longer present", "Reconnect"}},
		{faultDeviceBusy, []string{"something else", "EXCLUSIVE", "Close the other application"}},
		{faultNoSignal, []string{"NO SIGNAL", "AUDIO capture off the VIDEO clock", "SDI"}},
		{faultUnknown, []string{"did not say why", "BUSY", "MISSING", "NO SIGNAL"}},
	}

	for _, tc := range cases {
		got := captureFaultMessage(tc.fault, ev)
		for _, want := range tc.must {
			if !strings.Contains(got, want) {
				t.Errorf("fault %d message does not mention %q:\n%s", tc.fault, want, got)
			}
		}
		// The card, so a rig with two of them says which.
		if !strings.Contains(got, "SN-1234") || !strings.Contains(got, "UltraStudio") {
			t.Errorf("fault %d message names neither the device nor its serial:\n%s", tc.fault, got)
		}
		// And the evidence the diagnosis was made FROM. This is the one
		// assertion that applies to all four: the diagnosis is an inference
		// and must never replace the thing it was inferred from.
		if !strings.Contains(got, "not-negotiated (-4)") {
			t.Errorf("fault %d message does not quote the GStreamer text:\n%s", tc.fault, got)
		}
	}
}

func TestCaptureFaultMessageWithoutADeckLink(t *testing.T) {
	// A USB interface or a Dante endpoint gets the same three diagnoses and
	// none of the DeckLink prose: exclusivity, Desktop Video Setup and the
	// audio-follows-video clock are facts about the hardware, and on a USB
	// microphone they would be confident nonsense.
	ev := captureEvidence{DeviceName: "Scarlett 2i2", Message: "device not found"}

	got := captureFaultMessage(faultDeviceMissing, ev)
	if strings.Contains(got, "DeckLink") || strings.Contains(got, "Desktop Video") {
		t.Errorf("a non-DeckLink device was given DeckLink advice:\n%s", got)
	}
	if !strings.Contains(got, "Dante") && !strings.Contains(got, "unplugged") {
		t.Errorf("the message says nothing an operator could act on:\n%s", got)
	}
}

func TestCaptureFaultMessageWithNoDeviceName(t *testing.T) {
	// The identity is not always available: the DeckLink id is a number and the
	// macOS device property is a runtime handle, so internal/gst deliberately
	// has no display name to print at this point. The message must degrade
	// rather than produce a quoted empty string.
	got := captureFaultMessage(faultDeviceBusy, captureEvidence{Message: "not-negotiated"})
	if strings.Contains(got, `""`) {
		t.Errorf("an empty device name was rendered as an empty quoted string:\n%s", got)
	}
	if !strings.Contains(got, "the commentary input") {
		t.Errorf("the message does not say what failed:\n%s", got)
	}
}

// TestCaptureFaultNamesMatchThePipeline is the guard on the one duplication in
// this feature.
//
// capturefault.go declares the element names it classifies on, because
// gst_cgo.go is `cgo && !gststub` and its constants do not exist at Gate A —
// where the tests for a decision that takes the commentary off air have to run.
// Three short strings are duplicated as a result, and a rename of either half
// would silently return the audio capture to the fatal default. This is what
// stops that being silent.
func TestCaptureFaultNamesMatchThePipeline(t *testing.T) {
	src, err := os.ReadFile(cgoSourceFile)
	if err != nil {
		t.Fatalf("reading %s: %v", cgoSourceFile, err)
	}

	cases := []struct {
		cgoConst string
		mirror   string
		why      string
	}{
		{"nameSRTQueue", captureSinkQueueName,
			"the leaky queue in front of the sink would stop being spared and every peer loss would be fatal"},
		{"srtSinkNamePrefix", captureSinkNamePrefix,
			"the sink would stop being spared and every reconnect would take the commentary off air"},
		{"nameAudioSrc", captureAudioSrcName,
			"the commentary capture would rejoin the fatal default and its failures would stop being named"},
	}

	for _, tc := range cases {
		re := regexp.MustCompile(tc.cgoConst + `\s*=\s*"([^"]*)"`)
		m := re.FindSubmatch(src)
		if m == nil {
			t.Errorf("%s no longer declares %s as a string constant; capturefault.go mirrors it as %q. %s",
				cgoSourceFile, tc.cgoConst, tc.mirror, tc.why)
			continue
		}
		if got := string(m[1]); got != tc.mirror {
			t.Errorf("%s has %s = %q but capturefault.go mirrors it as %q: %s",
				cgoSourceFile, tc.cgoConst, got, tc.mirror, tc.why)
		}
	}
}

// TestVideoCaptureNamesShareThePrefix pins the invariant the prefix test rests
// on: every named element of the video capture leg begins with it. A constant
// added later that does not is an element that silently rejoins the fatal
// default, which is the exact regression this file exists to prevent.
func TestVideoCaptureNamesShareThePrefix(t *testing.T) {
	for _, name := range []string{nameVideoCaptureSrc, nameVideoCaptureClock} {
		if !strings.HasPrefix(name, videoCaptureNamePrefix) {
			t.Errorf("video capture element %q does not begin with %q, so classifyBusError "+
				"would treat its failures as pipeline-fatal", name, videoCaptureNamePrefix)
		}
	}
}
