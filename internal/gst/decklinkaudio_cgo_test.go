//go:build cgo && !gststub

// decklinkaudio_cgo_test.go renders the REAL capture descriptions for all four
// capture combinations and asserts what each one builds.
//
// Owner: WP-3a, with internal/gst.
//
// # It is about the PLAN, which is what capturedesc_cgo_test.go is not
//
// capturedesc_cgo_test.go renders the seven leg-sets and compares each against a
// golden string. This file asks the question one level up: given the two capture
// IDS an operator can configure, WHICH leg-sets get built, and how many DeckLink
// elements exist across the whole set. That is PlanCapture's fusion rule, and it
// is the difference between a seat that starts and a seat that does not — the
// card is EXCLUSIVE, so "exactly one decklinkvideosrc in the process, or none"
// is a hardware constraint and not a tidiness rule.
//
// REWRITTEN FROM pipelineDescription, which is deleted. Every assertion below is
// the one it made; what changed is that a configuration now renders a SET of
// descriptions rather than one string, so the counting is done across the set.
// The set is what the process actually builds, so counting across it is the more
// honest form of the same question.
//
// # The property that matters most is the NEGATIVE one
//
// THE WINDOWS BUILD IS ON AIR. A seat with a native microphone and a slate video
// source must come out of this feature byte for byte unchanged, and
// TestTheNativeSlateSeatIsUntouched is that statement: not "it still works", but
// that the string contains no decklink element of any kind and that the whole
// chain below the capture source is character-for-character the same in all four
// shapes.

package gst

import (
	"strings"
	"testing"
)

// theFourCombinations renders THE WHOLE CAPTURE LAYER for each shape the two
// capture ids can express, with everything else held constant.
//
// It goes through PlanCapture rather than naming leg-sets, so the fusion rule is
// exercised rather than restated: the card+card row is ONE description because
// the plan fuses it, and the counting tests below are counting across exactly
// what the process would build.
//
// The chains are joined with a newline so that a `strings.Count` over the result
// counts elements in the PROCESS rather than in one pipeline, which is the level
// the exclusivity rule is stated at.
func theFourCombinations(t *testing.T) map[string]string {
	t.Helper()
	const card = "2747401380"
	const nativeDevice = "native-endpoint-id"
	conform := FallbackConformTarget()

	render := func(videoCapture, audioCapture string) string {
		audioDevice := nativeDevice
		if audioCapture != "" {
			// Exactly one of the two is ever given; refuseWrongAudioSource is the
			// rule and NewCapture enforces it. An empty device on osxaudiosrc or
			// wasapi2src is not an error, it is THE SYSTEM DEFAULT INPUT.
			audioDevice = ""
		}
		var chains []string
		for _, legs := range PlanCapture(CaptureSources{
			VideoCaptureID: videoCapture,
			AudioCaptureID: audioCapture,
			AudioDeviceID:  audioDevice,
		}) {
			chains = append(chains, captureDescription(legs, conform, ""))
		}
		return strings.Join(chains, "\n")
	}

	return map[string]string{
		"slate+native": render("", ""),
		"card+native":  render(card, ""),
		"card+card":    render(card, card),
		"slate+card":   render("", card),
	}
}

// TestTheNativeSlateSeatIsUntouched is the compatibility statement for the whole
// feature, and it is the test to read first.
//
// The seat that ships today — a microphone and the still slate — must build a
// graph with no Blackmagic element anywhere in it. Not "one that also works":
// one in which the word does not appear, so that a machine with a card fitted
// for an unrelated purpose cannot acquire one by accident and so that the on-air
// Windows build is provably unaffected by everything else in this file.
func TestTheNativeSlateSeatIsUntouched(t *testing.T) {
	desc := theFourCombinations(t)["slate+native"]

	if strings.Contains(desc, "decklink") {
		t.Errorf("the slate + microphone pipeline contains a decklink element:\n%s", desc)
	}
	if !strings.Contains(desc, captureSourceFactory+" name="+nameAudioSrc) {
		t.Errorf("the commentary is not captured by %s on a seat with no card:\n%s",
			captureSourceFactory, desc)
	}
	if !strings.Contains(desc, "filesrc name="+nameSlateSrc) {
		t.Errorf("the slate leg is not built:\n%s", desc)
	}
	if strings.Contains(desc, "fakesink") {
		t.Errorf("a fakesink appears on a seat that needs no clock companion:\n%s", desc)
	}
	// AND THE SEAM IS THE ONLY THING BELOW EITHER LEG. No encoder, no muxer, no
	// srtq: those moved to sendDescription with the split, and a capture
	// description that grew one back would be a device inside the pipeline that
	// is destroyed at STOP.
	for _, gone := range []string{nameVideoEncod, nameMux, nameSRTQueue, "aacparse", "h264parse"} {
		if strings.Contains(desc, gone) {
			t.Errorf("the capture layer contains %q; everything below the proxysinks belongs to "+
				"the send pipeline, which has a different lifetime:\n%s", gone, desc)
		}
	}
}

// TestTheFourCombinationsBuildTheRightSources walks the matrix element by
// element: which capture sources exist, and how many decklinkvideosrc there are.
//
// The count is the assertion with hardware behind it. THE CARD IS EXCLUSIVE —
// two decklinkvideosrc in one process fail 3/3 and two processes fail 3/3 — so
// "exactly one, or none" is not tidiness, it is the difference between a seat
// that starts and a seat that does not.
func TestTheFourCombinationsBuildTheRightSources(t *testing.T) {
	descs := theFourCombinations(t)

	for _, tc := range []struct {
		shape string

		// The commentary source, by factory.
		wantAudioFactory string

		// How many decklinkvideosrc the graph has, and under which name.
		wantVideoSources int
		wantVideoName    string
		wantClockSink    bool
	}{
		{
			shape:            "slate+native",
			wantAudioFactory: captureSourceFactory,
			wantVideoSources: 0,
		},
		{
			shape:            "card+native",
			wantAudioFactory: captureSourceFactory,
			wantVideoSources: 1,
			wantVideoName:    nameVideoCaptureSrc,
		},
		{
			// ONE SOURCE SERVES BOTH. The card cannot give a second.
			shape:            "card+card",
			wantAudioFactory: audioCaptureFactory,
			wantVideoSources: 1,
			wantVideoName:    nameVideoCaptureSrc,
		},
		{
			// THE CLOCK COMPANION. decklinkaudiosrc cannot preroll without a
			// decklinkvideosrc in the same pipeline — measured, 0 buffers and 0
			// level messages against 160 — so the picture being a still does not
			// excuse the graph from having one.
			shape:            "slate+card",
			wantAudioFactory: audioCaptureFactory,
			wantVideoSources: 1,
			wantVideoName:    nameVideoCaptureClock,
			wantClockSink:    true,
		},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			desc := descs[tc.shape]

			if !strings.Contains(desc, tc.wantAudioFactory+" name="+nameAudioSrc) {
				t.Errorf("the commentary source is not %s name=%s:\n%s",
					tc.wantAudioFactory, nameAudioSrc, desc)
			}
			if got := strings.Count(desc, videoCaptureFactory); got != tc.wantVideoSources {
				t.Errorf("%d %s in the graph, want %d. The card is EXCLUSIVE: a second one is a "+
					"pipeline that fails, not an element that is merely wasted:\n%s",
					got, videoCaptureFactory, tc.wantVideoSources, desc)
			}
			if tc.wantVideoName != "" && !strings.Contains(desc, "name="+tc.wantVideoName) {
				t.Errorf("the decklinkvideosrc is not named %s, so the fault classifier and the "+
					"signal watchdog cannot find it:\n%s", tc.wantVideoName, desc)
			}
			if got := strings.Contains(desc, "fakesink name="+nameVideoCaptureClockSink); got != tc.wantClockSink {
				t.Errorf("clock companion sink present = %v, want %v:\n%s", got, tc.wantClockSink, desc)
			}
		})
	}
}

// TestTheDeckLinkCommentarySourceAsksForSixteenChannels pins the property the
// entire routing feature rests on.
//
// channels=2 would negotiate a POSITIONED pair and need no mix matrix at all,
// which is why it looks like the easy answer. It is the wrong one: it can only
// ever reach the card's FIRST pair, and the commentator may be on any of sixteen
// embedded channels. The channels property is not live-settable either, so
// reaching another pair would cost a pipeline restart mid-match.
func TestTheDeckLinkCommentarySourceAsksForSixteenChannels(t *testing.T) {
	for _, shape := range []string{"card+card", "slate+card"} {
		desc := theFourCombinations(t)[shape]
		if !strings.Contains(desc, "channels=16") {
			t.Errorf("%s: the DeckLink commentary source does not ask for 16 channels, so the "+
				"routing grid, the sixteen-bar picker meter and the mix matrix are all "+
				"unreachable:\n%s", shape, desc)
		}
	}
}

// TestNoCaptureLegEverSetsTheConnectionProperty is the one hardware rule in this
// package that cannot be undone by restarting anything, checked against the
// rendered string rather than the source text.
//
// `connection` PERSISTENTLY RECONFIGURES THE CARD and overrides what the operator
// set in Blackmagic Desktop Video Setup. It has had to be undone by hand twice.
// decklinkaudiosrc has its own `connection` enum — embedded, aes, analog,
// analog-xlr, analog-rca — which makes it far more tempting on the audio element
// than it ever was on the video one, and it is exactly as forbidden. If the card
// is silent the answer is NEVER another connection value.
func TestNoCaptureLegEverSetsTheConnectionProperty(t *testing.T) {
	for shape, desc := range theFourCombinations(t) {
		if strings.Contains(desc, "connection") {
			t.Errorf("%s sets connection on a decklink element. It is not a per-pipeline "+
				"selection: it PERSISTENTLY RECONFIGURES THE CARD and overrides Blackmagic "+
				"Desktop Video Setup:\n%s", shape, desc)
		}
	}
}

// TestEverythingBelowTheCommentarySourceIsIdentical is the property internal/sender
// and the timestamp discipline actually rely on: one graph, whatever is on top of
// it.
//
// It compares the four rendered strings from the first audioconvert downwards,
// character for character. A difference there would mean the AAC encoder, the
// muxer or the leaky srtq were seeing something that depended on which
// microphone an operator picked — which is the assumption every reconnect rule
// in internal/sender is written against.
func TestEverythingBelowTheCommentarySourceIsIdentical(t *testing.T) {
	// The chain starts at the audioconvert that makes the format legal for
	// chlevel and ends at the proxysink. Everything between is written once in
	// captureDescription and must render once here.
	//
	// The lower bound MOVED with the seam: it used to be the aq queue feeding the
	// muxer, which is now in the send pipeline. The claim is unchanged — one
	// graph below the source, whatever is on top of it — and it now runs as far as
	// the seam, which is where the send side picks it up with caps it asserts.
	const from = " ! audioconvert ! level name=" + channelLevelElementName
	const to = "proxysink name=" + nameAudioProxySink

	var want, wantShape string
	for shape, desc := range theFourCombinations(t) {
		start := strings.Index(desc, from)
		if start < 0 {
			t.Fatalf("%s: the audio chain does not start with %q:\n%s", shape, from, desc)
		}
		end := strings.Index(desc[start:], to)
		if end < 0 {
			t.Fatalf("%s: the audio chain does not reach %q:\n%s", shape, to, desc)
		}
		got := desc[start : start+end+len(to)]
		if want == "" {
			want, wantShape = got, shape
			continue
		}
		if got != want {
			t.Errorf("the audio chain below the capture source differs between %s and %s.\n"+
				"%s:\n%s\n%s:\n%s\nEverything from the mix matrix down is one graph, and "+
				"internal/sender's reconnect ladder and the timestamp discipline are written "+
				"against that", wantShape, shape, wantShape, want, shape, got)
		}
	}
}

// TestTheClockCompanionCannotDisturbTheSlateLeg pins the two properties that
// keep an element nobody asked for out of everybody's way.
//
// sync=false async=false is what keeps the fakesink out of the pipeline's preroll
// latching and off the clock: it never posts ASYNC_START, never holds a state
// change, and never waits to render. A card with no signal therefore cannot delay
// or stall a transition the slate leg is also making — and the slate leg is the
// one carrying the picture on this seat.
func TestTheClockCompanionCannotDisturbTheSlateLeg(t *testing.T) {
	descs := theFourCombinations(t)
	desc := descs["slate+card"]

	if !strings.Contains(desc, "fakesink name="+nameVideoCaptureClockSink+" sync=false async=false") {
		t.Errorf("the clock companion's sink is not sync=false async=false; it would take part in "+
			"the pipeline's preroll latching and a card with no signal could stall the state "+
			"change the slate leg is making:\n%s", desc)
	}
	if !strings.Contains(desc, "drop-no-signal-frames=false") {
		t.Errorf("the clock companion may drop its no-signal frames. Its whole job is to keep "+
			"producing a clock, and a card that stopped emitting would stop the COMMENTARY, not "+
			"merely blank a picture nobody is watching:\n%s", desc)
	}

	// It is a chain of its own, linked to nothing the slate leg touches.
	slate := descs["slate+native"]
	if !strings.Contains(desc, "filesrc name="+nameSlateSrc) {
		t.Errorf("the slate leg is missing from a slate + card seat:\n%s", desc)
	}
	slateLeg := slate[strings.Index(slate, "filesrc name="+nameSlateSrc):]
	if i := strings.Index(slateLeg, "\n"); i >= 0 {
		slateLeg = slateLeg[:i]
	}
	if !strings.Contains(desc, slateLeg) {
		t.Errorf("the slate leg is not character-for-character what it is without the clock "+
			"companion.\nwithout: %s\nwith:\n%s", slateLeg, desc)
	}
}
