//go:build cgo && !gststub

// capturedesc_cgo_test.go pins the eight parse strings CHARACTER FOR CHARACTER.
//
// A golden-string test is how a mis-ordered chain gets caught before it reaches
// a card. Every reordering this package has ever regretted — videoconvert and
// videoscale swapped around imagefreeze (2.85 s of CPU per 500 frames instead of
// 0.04 s), the missing videorate that made the card's NTSC placeholder first
// buffer fatal, a capsfilter above imagefreeze that could not negotiate — parses,
// starts and sends. None of them is visible in anything except the string.
//
// The expected strings are written out LONGHAND rather than assembled from the
// same helpers the builder uses. That is the whole value: a test built from
// videoProxyTail() would go on passing if videoProxyTail() changed, which is
// exactly the change worth catching. The two platform FACTORY names are the
// single exception, and they are consts here for the reason gst_stub_test.go's
// TestPlatformElementContractIsPinned gives: it pins their exact values in both
// elements_*.go files, so referencing them here checks both ports at once
// instead of pinning this one.
package gst

import (
	"strconv"
	"strings"
	"testing"
)

// The conform target every golden below is rendered at: the shipping default.
func goldenConform() ConformTarget { return FallbackConformTarget() }

const goldenSpatialCaps = "video/x-raw,format=NV12,width=1920,height=1080," +
	"pixel-aspect-ratio=1/1,colorimetry=bt709,interlace-mode=progressive"

const goldenCaptureCaps = goldenSpatialCaps + ",framerate=50/1"

const goldenSlateChain = "filesrc name=slate" +
	" ! pngdec" +
	" ! videoconvert" +
	" ! videoscale name=vscale" +
	" ! " + goldenSpatialCaps +
	" ! imagefreeze is-live=true" +
	" ! video/x-raw,framerate=50/1" +
	" ! queue name=vproxq leaky=downstream max-size-buffers=8 max-size-bytes=0 max-size-time=0" +
	" ! proxysink name=vproxsink"

const goldenCardChain = "decklinkvideosrc name=vcapsrc mode=auto drop-no-signal-frames=false" +
	" ! videoconvert name=vcapconv" +
	" ! deinterlace name=vcapdeint" +
	" ! videoscale name=vcapscale" +
	" ! videorate name=vcaprate" +
	" ! " + goldenCaptureCaps +
	" ! tee name=vcaptee allow-not-linked=true"

const goldenCardProxyChain = "vcaptee." +
	" ! queue name=vproxq leaky=downstream max-size-buffers=8 max-size-bytes=0 max-size-time=0" +
	" ! proxysink name=vproxsink"

const goldenAudioTail = " ! audioconvert" +
	" ! level name=chlevel interval=100000000 post-messages=false" +
	" ! audioconvert name=aconv ! audioresample" +
	" ! audio/x-raw,format=S16LE,rate=48000,channels=2,layout=interleaved" +
	" ! volume name=coughmute mute=false" +
	" ! level name=alevel interval=50000000" +
	" ! queue name=aproxq leaky=downstream max-size-time=1000000000 max-size-bytes=0" +
	" max-size-buffers=0" +
	" ! proxysink name=aproxsink"

const goldenClockChain = "decklinkvideosrc name=vcapclock mode=auto drop-no-signal-frames=false" +
	" ! fakesink name=vcapclocksink sync=false async=false"

// goldenNativeChain and goldenCardAudioChain are functions rather than consts
// only because the platform factory is one.
func goldenNativeChain() string {
	return captureSourceFactory + " name=asrc" + goldenAudioTail
}

func goldenCardAudioChain() string {
	return "decklinkaudiosrc name=asrc channels=16" + goldenAudioTail
}

// TestCaptureDescriptionRendersEveryPlannedShape is the table PLAN.md section 2
// specifies: seven capture shapes, every one of them rendered and compared
// character for character.
//
// THE CHAIN ORDER IS PART OF THE ASSERTION. Picture chains, then commentary
// chains, then the clock companion, then the preview. gst_parse_launch does not
// care about the order, which is precisely why nothing else would catch a change
// to it — and a golden string is only stable if the order is.
func TestCaptureDescriptionRendersEveryPlannedShape(t *testing.T) {
	preview := previewBranch("glimagesink")
	if preview == "" || !strings.HasPrefix(preview, "\n") {
		t.Fatalf("previewBranch no longer returns a branch beginning with a newline; every "+
			"description below depends on it carrying its own separator. Got %q", preview)
	}

	for _, tc := range []struct {
		name string
		legs CaptureLegs
		prev string
		want string
	}{
		{
			name: "P-SLATE",
			legs: CaptureLegs{Picture: PictureSlate},
			want: goldenSlateChain,
		},
		{
			name: "P-CARD",
			legs: CaptureLegs{Picture: PictureCard},
			want: goldenCardChain + "\n" + goldenCardProxyChain,
		},
		{
			name: "P-CARD-PREVIEW",
			legs: CaptureLegs{Picture: PictureCard, Preview: true},
			prev: preview,
			want: goldenCardChain + "\n" + goldenCardProxyChain + preview,
		},
		{
			name: "C-NATIVE",
			legs: CaptureLegs{Commentary: CommentaryNative},
			want: goldenNativeChain(),
		},
		{
			// The clock companion is its own chain and it is LAST, linked to
			// nothing. It exists because decklinkaudiosrc cannot preroll without
			// a decklinkvideosrc in the same pipeline — measured, 0 buffers and 0
			// level messages against 160.
			name: "C-CARD",
			legs: CaptureLegs{Commentary: CommentaryCard},
			want: goldenCardAudioChain() + "\n" + goldenClockChain,
		},
		{
			// FUSED drops the clock companion: vcapsrc is the clock. Building one
			// anyway would be a SECOND decklinkvideosrc, and the card is
			// exclusive — that is not a wasted element, it is a seat that will
			// not start at all.
			name: "FUSED",
			legs: CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard},
			want: goldenCardChain + "\n" + goldenCardProxyChain + "\n" + goldenCardAudioChain(),
		},
		{
			name: "FUSED-PREVIEW",
			legs: CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard, Preview: true},
			prev: preview,
			want: goldenCardChain + "\n" + goldenCardProxyChain + "\n" +
				goldenCardAudioChain() + preview,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := captureDescription(tc.legs, goldenConform(), tc.prev); got != tc.want {
				t.Errorf("captureDescription(%s) is not the specified string.\n got: %s\nwant: %s",
					tc.legs, got, tc.want)
			}
			// The label the leg-set reports must be the one the plan uses, or a
			// log line and the specification stop being readable side by side.
			if tc.legs.String() != tc.name {
				t.Errorf("CaptureLegs.String() = %q, want %q", tc.legs.String(), tc.name)
			}
		})
	}
}

// TestSendDescriptionIsInvariant pins the ONE send string.
//
// Two parameters and no third. No device, no slate, no preview, no conform
// target, no channel map, no mute: every one of those is upstream of the seam and
// belongs to the capture layer. If a future change needs a third parameter here,
// something has been put on the wrong side of the seam.
func TestSendDescriptionIsInvariant(t *testing.T) {
	want := "mpegtsmux name=mux alignment=7 pcr-interval=3600" +
		" ! queue name=srtq leaky=downstream max-size-buffers=4000\n" +

		"proxysrc name=vproxsrc" +
		" ! vtenc_h264 name=venc" +
		" ! video/x-h264,profile=high" +
		" ! h264parse config-interval=-1" +
		" ! video/x-h264,stream-format=byte-stream,alignment=au" +
		" ! queue name=vq max-size-time=1000000000 ! mux.\n" +

		"proxysrc name=aproxsrc" +
		" ! audio/x-raw,format=S16LE,rate=48000,channels=2,layout=interleaved" +
		" ! " + aacEncoderFactory + " bitrate=128000" +
		" ! aacparse ! audio/mpeg,mpegversion=4,stream-format=adts" +
		" ! queue name=aq max-size-time=1000000000 ! mux."

	if got := sendDescription("vtenc_h264", DefaultAudioBitrateBps); got != want {
		t.Errorf("sendDescription is not the specified string.\n got: %s\nwant: %s", got, want)
	}
}

// TestSendDescriptionVariesOnlyByItsTwoParameters is the invariance claim made
// as an assertion rather than as prose: the string for one seat and the string
// for another differ in exactly the encoder name and the bitrate.
func TestSendDescriptionVariesOnlyByItsTwoParameters(t *testing.T) {
	base := sendDescription("vtenc_h264", 128000)
	other := sendDescription("mfh264enc", 96000)

	normalised := strings.Replace(other, "mfh264enc", "vtenc_h264", 1)
	normalised = strings.Replace(normalised, "bitrate=96000", "bitrate=128000", 1)
	if normalised != base {
		t.Errorf("sendDescription differs by more than its two parameters.\n got: %s\nwant: %s",
			normalised, base)
	}
}

// TestNoCaptureDescriptionEverSetsTheConnectionProperty is the rendered half of
// the guard gst_stub_test.go makes over the source.
//
// Setting decklinkvideosrc's `connection` PERSISTENTLY RECONFIGURES THE CARD and
// overrides Blackmagic Desktop Video Setup; it has had to be undone by hand
// twice on this rig. Reading the source catches the const being introduced;
// rendering every shape catches it arriving through a helper the source scan
// does not follow.
func TestNoCaptureDescriptionEverSetsTheConnectionProperty(t *testing.T) {
	for _, legs := range everyCaptureShape() {
		desc := captureDescription(legs, goldenConform(), "")
		if strings.Contains(desc, "connection") {
			t.Errorf("the %s description mentions `connection`. It is not a per-pipeline input "+
				"selection: it reconfigures the CARD. Delete it:\n%s", legs, desc)
		}
	}
}

// TestEveryCaptureDescriptionEndsInTheSeam is the structural claim the whole
// layer rests on: a capture pipeline's legs end at a proxysink and nowhere else.
//
// An encoder, a muxer or a sink appearing in one of these strings would mean a
// capture pipeline that sends, which is the shape R1 exists to abolish — and it
// would be invisible until the day somebody wondered why STOP blanked the
// preview.
func TestEveryCaptureDescriptionEndsInTheSeam(t *testing.T) {
	for _, legs := range everyCaptureShape() {
		desc := captureDescription(legs, goldenConform(), "")
		for _, forbidden := range []string{"mpegtsmux", "srtsink", "h264parse", "aacparse",
			aacEncoderFactory, "proxysrc"} {
			if strings.Contains(desc, forbidden) {
				t.Errorf("the %s capture description contains %q. Capture ends at the proxysink; "+
					"everything downstream of it belongs to the send pipeline, which is built at "+
					"START and destroyed at STOP:\n%s", legs, forbidden, desc)
			}
		}
		if legs.Picture != PictureNone &&
			!strings.Contains(desc, "proxysink name="+nameVideoProxySink) {
			t.Errorf("the %s description has a picture leg that does not end in %s:\n%s",
				legs, nameVideoProxySink, desc)
		}
		if legs.Commentary != CommentaryNone &&
			!strings.Contains(desc, "proxysink name="+nameAudioProxySink) {
			t.Errorf("the %s description has a commentary leg that does not end in %s:\n%s",
				legs, nameAudioProxySink, desc)
		}
	}
}

// TestEveryProxysinkHasALeakyQueueInFrontOfIt is invariant 1 from seam.go, as an
// assertion.
//
// proxysrc's own internal queue is leaky=0 max-size-buffers=200 and exposes no
// tuning at all, so the capture side is the ONLY place a send-side stall can be
// absorbed. Measured on the real card under a 12 s wedge: with the leak, 50.1 fps
// off the card and 20.0 meter messages a second; without it, 11.6 fps, 7.2 msg/s
// and "Dropped 271 old frames" from the card itself.
func TestEveryProxysinkHasALeakyQueueInFrontOfIt(t *testing.T) {
	for _, legs := range everyCaptureShape() {
		desc := captureDescription(legs, goldenConform(), "")
		for _, want := range []string{
			"queue name=" + nameVideoProxyQueue + " leaky=downstream",
			"queue name=" + nameAudioProxyQueue + " leaky=downstream",
		} {
			sink := nameVideoProxySink
			if strings.Contains(want, nameAudioProxyQueue) {
				sink = nameAudioProxySink
			}
			if !strings.Contains(desc, "proxysink name="+sink) {
				continue
			}
			if !strings.Contains(desc, want) {
				t.Errorf("the %s description has a %s with no leaky queue in front of it. A "+
					"wedged send pipeline then drags the preview to 7.2 fps and the meters to "+
					"7.2 msg/s and makes the card drop packets — the exact failure the always-live "+
					"capture exists to eliminate:\n%s", legs, sink, desc)
			}
		}
	}
}

// TestTheSeamCapsAreIdenticalOnBothSides is the caps contract, checked where it
// can actually break: the capture side pins it above the cough mute and the send
// side asserts it below aproxsrc, and a divergence is a silent wrong encode
// rather than a failure.
func TestTheSeamCapsAreIdenticalOnBothSides(t *testing.T) {
	capture := captureDescription(CaptureLegs{Commentary: CommentaryNative}, goldenConform(), "")
	send := sendDescription("vtenc_h264", DefaultAudioBitrateBps)

	if strings.Count(capture, seamAudioCaps) != 1 {
		t.Errorf("the commentary capture description does not pin the seam caps exactly once:\n%s",
			capture)
	}
	if strings.Count(send, seamAudioCaps) != 1 {
		t.Errorf("the send description does not assert the seam caps exactly once:\n%s", send)
	}
	// atenc's sink template is S16LE, interleaved, channels: [1,8], verified on
	// this machine. Anything else crossing here is refused by the encoder with an
	// error that names neither side of the seam.
	for _, field := range []string{"format=S16LE", "rate=48000", "channels=2",
		"layout=interleaved"} {
		if !strings.Contains(seamAudioCaps, field) {
			t.Errorf("seamAudioCaps no longer pins %s", field)
		}
	}
}

// TestTheQueueDepthsRenderTheDeclaredConstants keeps the policy and the string
// from drifting: a depth changed in the const and not in the description would be
// a comment describing a queue nobody configured.
func TestTheQueueDepthsRenderTheDeclaredConstants(t *testing.T) {
	desc := captureDescription(
		CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard}, goldenConform(), "")
	for _, want := range []string{
		"max-size-buffers=" + strconv.Itoa(videoProxyQueueBuffers),
		"max-size-time=" + strconv.Itoa(audioProxyQueueTimeNs),
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("the fused description does not carry %q:\n%s", want, desc)
		}
	}
}

// everyCaptureShape is the seven, without the preview branch — which is
// preview_test.go's and is appended whole or not at all.
func everyCaptureShape() []CaptureLegs {
	return []CaptureLegs{
		{Picture: PictureSlate},
		{Picture: PictureCard},
		{Picture: PictureCard, Preview: true},
		{Commentary: CommentaryNative},
		{Commentary: CommentaryCard},
		{Picture: PictureCard, Commentary: CommentaryCard},
		{Picture: PictureCard, Commentary: CommentaryCard, Preview: true},
	}
}
