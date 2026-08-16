//go:build !cgo || gststub

// audiosource_test.go is the Gate A cover for WHICH ELEMENT OPENS THE
// COMMENTARY, and for the four capture combinations the two source fields can
// express.
//
// Owner: WP-3a, with internal/gst.
//
// # Why this file exists rather than more rows in gst_stub_test.go
//
// Because the rule it tests is the one that decides whether a match goes out
// down the commentator's microphone or down the laptop's. refuseWrongAudioSource
// lives in gst.go with no build tag precisely so that this can be REAL CODE
// RUNNING rather than a source guard reading text, and a file of its own is what
// makes that visible: everything below calls the shipped function and asserts
// what it actually returned.
//
// The measured fact underneath all of it: an empty device id on osxaudiosrc or
// wasapi2src is NOT an error. It is the SYSTEM DEFAULT INPUT. So "the platform
// capture element exists and has no device" is the failure state, it produces no
// diagnostic anywhere, and every lamp in the application stays green while the
// wrong microphone goes to air. The rule makes that state unreachable by
// construction: the native element is built only when AudioDeviceID is
// non-empty, and the only way to have neither a device nor a card is to be
// refused before anything is built.

package gst

import (
	"errors"
	"strings"
	"testing"
)

// TestRefuseWrongAudioSourceIsExactlyOneSource is the whole rule, stated as the
// four states the two fields can be in.
func TestRefuseWrongAudioSourceIsExactlyOneSource(t *testing.T) {
	const (
		endpoint = "{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}"
		card     = "2747401380"
	)

	for _, tc := range []struct {
		name     string
		device   string
		capture  string
		wantErr  bool
		mustName []string
	}{
		{
			name:   "a platform endpoint alone is the seat that ships today",
			device: endpoint,
		},
		{
			name:    "a card alone is the DeckLink commentary seat",
			capture: card,
		},
		{
			name:    "NEITHER is refused, and this is the one that would go to air",
			wantErr: true,
			// Both field names, because the operator's fix is one of the two and
			// a message naming only one sends half of them to the wrong screen.
			mustName: []string{"AudioDeviceID", "AudioCaptureID", "SYSTEM DEFAULT INPUT"},
		},
		{
			name:     "BOTH is refused: which one wins must not be an accident of the builder",
			device:   endpoint,
			capture:  card,
			wantErr:  true,
			mustName: []string{"AudioDeviceID", "AudioCaptureID"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseWrongAudioSource(tc.device, tc.capture)
			if tc.wantErr && err == nil {
				t.Fatal("refuseWrongAudioSource accepted a combination it must refuse")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("refuseWrongAudioSource refused a legitimate seat: %v", err)
			}
			for _, want := range tc.mustName {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q, so the reader cannot act on it: %v",
						want, err)
				}
			}
		})
	}
}

// TestRefuseWrongAudioSourceStillRefusesARenderEndpoint pins that the older rule
// survived being folded into the new one.
//
// It is the same failure device_id.go was written for: wasapi2src accepts a
// PLAYBACK endpoint at construction and fails asynchronously with error 1551,
// which internal/sender then misreads as the network being down and retries
// forever. The sentinel matters as much as the refusal — the App layer tells a
// wrong-kind device from a missing one by it.
func TestRefuseWrongAudioSourceStillRefusesARenderEndpoint(t *testing.T) {
	err := refuseWrongAudioSource("{0.0.0.00000000}.{8678ce58-90c0-4827-8ff7-c9edd8d074ed}", "")
	if err == nil {
		t.Fatal("a RENDER endpoint was accepted as the commentary input")
	}
	if !errors.Is(err, ErrNotACaptureDevice) {
		t.Errorf("the refusal does not wrap ErrNotACaptureDevice: %v", err)
	}
}

// TestRefuseWrongAudioSourceConvertsTheCardID pins that the DeckLink id is
// CONVERTED rather than pattern-matched, by the same parse configureDeckLinkSource
// will make.
//
// A CoreAudio unique-id or a WASAPI GUID reaching decklinkaudiosrc's
// persistent-id property is not an error there either: the property keeps its
// own -1 default, which means "use device-number", which means whichever card
// the driver enumerated first. On a one-card rig that is invisible; on a two-card
// rig it is the wrong card with every lamp green.
func TestRefuseWrongAudioSourceConvertsTheCardID(t *testing.T) {
	for _, id := range []string{
		"BuiltInMicrophoneDevice",
		"{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}",
		"BF568F24-731B-41DB-932E-AC7E260BC71A",
	} {
		if err := refuseWrongAudioSource("", id); err == nil {
			t.Errorf("AudioCaptureID = %q was accepted as a DeckLink persistent-id; the element "+
				"would fall back to device-number and open whichever card enumerated first", id)
		}
	}
	if err := refuseWrongAudioSource("", "2747401380"); err != nil {
		t.Errorf("the fitted card's real persistent-id was refused: %v", err)
	}
}

// TestStubStartsADeckLinkCommentarySeat is the DeckLink seat end to end through
// the stub: no platform endpoint anywhere, sixteen channels negotiated, a matrix
// written and the per-channel picker meter armed.
//
// The width assertion is the one worth reading twice. decklinkaudiosrc is built
// with channels=16 and has no configuration in which it presents a positioned
// pair, so a stub that reported two would let a routing UI be written against a
// shape the shipped build cannot produce — and the sixteen-bar picker is the
// control an operator uses to find which channel the commentator is on.
func TestStubStartsADeckLinkCommentarySeat(t *testing.T) {
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{
		SlatePath:       "slate.png",
		AudioCaptureID:  "2747401380",
		OnChannelLevels: func(Levels) {},
	}); err != nil {
		t.Fatalf("Start refused a DeckLink commentary seat: %v", err)
	}
	if got := p.StartedWith().AudioDeviceID; got != "" {
		t.Errorf("AudioDeviceID = %q on a DeckLink seat; no platform capture element is built and "+
			"an empty id on one is the SYSTEM DEFAULT INPUT", got)
	}
	if got := p.InputChannels(); got != deckLinkAudioChannels {
		t.Errorf("InputChannels() = %d, want %d: a DeckLink commentary seat presents all sixteen "+
			"embedded channels and the routing grid is sized from this number", got,
			deckLinkAudioChannels)
	}
	if _, written := p.ChannelMap(); !written {
		t.Error("no mix matrix was written for an unpositioned sixteen-channel source. The matrix " +
			"is a NEGOTIATION CONSTRAINT and not a gain: audioconvert cannot fold unpositioned " +
			"channels to stereo without one and the leg dies with not-negotiated (-4)")
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestStubRefusesTwoDifferentCards pins the one-card rule, which is what keeps
// the clock companion honest.
//
// A DeckLink drives audio capture off the VIDEO clock. When the video leg is
// also a card, THAT decklinkvideosrc is the clock — the card is exclusive, so a
// second source in the same process fails 3/3 — and two ids naming different
// cards therefore describes a commentary leg clocked by the wrong card's video.
// The real build discovers that as a seat that will not preroll, naming neither
// card; here it is a sentence, before any hardware is involved.
func TestStubRefusesTwoDifferentCards(t *testing.T) {
	p := NewStubPipeline()
	err := p.Start(PipelineOpts{
		SlatePath:      "slate.png",
		VideoCaptureID: "2747401380",
		AudioCaptureID: "1234567890",
	})
	if err == nil {
		t.Fatal("Start accepted a commentary leg on one card and a picture on another")
	}
	for _, want := range []string{"2747401380", "1234567890"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name card %s: %v", want, err)
		}
	}
	if p.State() != StubStateStopped {
		t.Errorf("state after the refusal = %q, want %q", p.State(), StubStateStopped)
	}
}

// TestStubStartsBothLegsOnOneCard is the combination the fitted rig runs: the
// picture and the commentary off the same card, which needs ONE decklinkvideosrc
// and gets one.
func TestStubStartsBothLegsOnOneCard(t *testing.T) {
	const card = "2747401380"
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{
		SlatePath:      "slate.png",
		VideoCaptureID: card,
		AudioCaptureID: card,
	}); err != nil {
		t.Fatalf("Start refused a seat with both legs on one card: %v", err)
	}
	got := p.StartedWith()
	if got.VideoCaptureID != card || got.AudioCaptureID != card {
		t.Errorf("started with video %q audio %q, want both %q",
			got.VideoCaptureID, got.AudioCaptureID, card)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestTheFourCaptureCombinationsAreExpressible states the matrix as one table,
// because it is the thing a reader of this feature needs and the thing two
// separately correct halves can still get wrong between them.
//
// The elements each shape builds are asserted in the cgo twin
// (decklinkaudio_cgo_test.go, which renders pipelineDescription for real). What
// is asserted HERE is that every one of the four is a seat Start accepts, and
// that the two ids arrive at the pipeline unswapped — which is the failure a
// pair of same-typed strings invites and the reason capturePlan is a struct.
func TestTheFourCaptureCombinationsAreExpressible(t *testing.T) {
	const (
		endpoint = "{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}"
		card     = "2747401380"
	)

	for _, tc := range []struct {
		name          string
		device, video string
		audio         string
	}{
		{name: "slate picture, microphone commentary — the seat that ships today", device: endpoint},
		{name: "card picture, microphone commentary", device: endpoint, video: card},
		{name: "card picture, card commentary — one source serves both", video: card, audio: card},
		{name: "slate picture, card commentary — the clock companion case", audio: card},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewStubPipeline()
			if err := p.Start(PipelineOpts{
				SlatePath:      "slate.png",
				AudioDeviceID:  tc.device,
				VideoCaptureID: tc.video,
				AudioCaptureID: tc.audio,
			}); err != nil {
				t.Fatalf("Start refused a combination the application can configure: %v", err)
			}
			got := p.StartedWith()
			if got.AudioDeviceID != tc.device {
				t.Errorf("AudioDeviceID = %q, want %q", got.AudioDeviceID, tc.device)
			}
			if got.VideoCaptureID != tc.video {
				t.Errorf("VideoCaptureID = %q, want %q", got.VideoCaptureID, tc.video)
			}
			if got.AudioCaptureID != tc.audio {
				t.Errorf("AudioCaptureID = %q, want %q", got.AudioCaptureID, tc.audio)
			}
			if err := p.Stop(); err != nil {
				t.Fatalf("Stop: %v", err)
			}
		})
	}
}

// TestPipelineDescriptionBuildsTheCommentarySourceConditionally is the Gate A
// source guard over the half of the feature Gate A cannot run.
//
// The behavioural version is decklinkaudio_cgo_test.go, which renders the real
// string. This one exists because gst_cgo.go does not compile at Gate A and the
// on-air Windows build is checked from whichever host Gate A runs on: a change
// that repointed the commentary at a decklink element unconditionally would
// otherwise reach a Windows commentary position before anything noticed.
func TestPipelineDescriptionBuildsTheCommentarySourceConditionally(t *testing.T) {
	body := pipelineDescriptionSource(t)

	if !strings.Contains(body, `audioCapture != ""`) {
		t.Error("pipelineDescription no longer chooses the commentary source on whether a card id " +
			"was supplied. An empty PipelineOpts.AudioCaptureID must mean the PLATFORM capture " +
			"source, byte for byte, on every seat that has configured nothing")
	}
	if !strings.Contains(body, "audioCaptureFactory") {
		t.Error("pipelineDescription never mentions audioCaptureFactory, so there is no DeckLink " +
			"commentary source in the graph at all — which is the state this work package existed " +
			"to end")
	}

	// THE CLOCK COMPANION'S CONDITION IS AN AND OF BOTH IDS. Getting it wrong is
	// not a wasted element: with the video leg already on the card, a second
	// decklinkvideosrc is a pipeline that FAILS — two sources in one process fail
	// 3/3 — so a seat with a camera and a card microphone would not start at all.
	if !strings.Contains(body, `audioCapture != "" && videoCapture == ""`) {
		t.Error("the clock companion is not conditioned on the commentary being a card AND the " +
			"picture not being one. The card is EXCLUSIVE: when the video leg is also the card, " +
			"vcapsrc IS the clock and a second decklinkvideosrc fails the whole pipeline")
	}

	// The audio chain below the source is written ONCE, for both sources, which
	// is what lets internal/sender and the timestamp discipline reason about one
	// graph however the commentary arrives.
	for _, tail := range []string{
		"audioconvert name=",
		"audio/x-raw,format=S16LE,rate=48000,channels=2,layout=interleaved",
		"level name=alevel interval=50000000",
		"aacparse ! audio/mpeg,mpegversion=4,stream-format=adts",
	} {
		if n := strings.Count(body, tail); n != 1 {
			t.Errorf("%q appears %d times in pipelineDescription, want exactly 1: everything "+
				"below the mix matrix is shared by both commentary sources and a second copy is "+
				"a second graph that only stays equal by hand", tail, n)
		}
	}
}

// TestTheClockCompanionsSinkCarriesTheCapturePrefix is the same rule
// TestVideoCaptureNamesShareThePrefix states for the source names, applied to the
// one element of that leg whose constant lives in gst_cgo.go and is therefore
// invisible to Gate A except as text.
//
// classifyBusError decides by NAME. An unprefixed fakesink would rejoin the
// FATAL default, so a sink erroring on a seat whose commentary was perfectly
// healthy would take the commentary off air.
func TestTheClockCompanionsSinkCarriesTheCapturePrefix(t *testing.T) {
	_, file := parseSource(t, cgoSourceFile)
	name := stringConstValue(t, file, "nameVideoCaptureClockSink")
	if !strings.HasPrefix(name, videoCaptureNamePrefix) {
		t.Errorf("nameVideoCaptureClockSink = %q does not begin with %q, so classifyBusError "+
			"would treat its failures as pipeline-fatal and take the commentary off air over a "+
			"fakesink", name, videoCaptureNamePrefix)
	}
	if !strings.Contains(pipelineDescriptionSource(t), "nameVideoCaptureClockSink") {
		t.Error("pipelineDescription does not name the clock companion's sink, so GStreamer gives " +
			"it one of its own (\"fakesink0\") and the prefix rule above protects nothing")
	}
}
