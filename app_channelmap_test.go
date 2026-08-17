//go:build dev || production || bindings

// Tests for the DeckLink channel routing's half of the wire-up (app.go).
//
// The routing MODEL is tested in internal/gst/channelmap_test.go, where the
// arithmetic runs at Gate A with no GStreamer present: which maps are legal,
// which way round the matrix goes, and what a rejected coefficient does. What is
// tested HERE is everything between that model and the page.
//
// Three of those things can only be got wrong at this layer, and each of them is
// invisible from either side alone:
//
//   - THE WIDTH. A mix matrix whose width does not match what the capture pad
//     negotiated does not degrade the feed, it stops it — measured, "streaming
//     stopped, reason error (-5)", with every coefficient in the matrix
//     perfectly legal. So the number the routing grid sizes itself from must
//     come from the pad and from nowhere else, and this file is where that
//     number crosses the boundary.
//   - THE ZERO VALUE. An empty map means "nobody has chosen" and resolves to the
//     card's first two embedded channels. Every layer here has an opportunity to
//     helpfully materialise that default into something explicit, and doing so
//     would freeze today's default into every config file and take away the
//     screen's ability to say that nobody has chosen.
//   - THE END OF A SESSION. The grid is a set of controls over a capture pad. A
//     pipeline that has stopped has no pad, and a grid still drawn at sixteen
//     channels over one is sixteen crosspoints that control nothing.
package main

import (
	"strings"
	"testing"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
)

func TestGetChannelMapReportsNoChannelsWithNoSession(t *testing.T) {
	a, _ := newTestApp(t)

	got, err := a.GetChannelMap()
	if err != nil {
		t.Fatalf("GetChannelMap() error = %v; not sending is not an error, it is the state of "+
			"every machine that has not pressed START", err)
	}
	if got.InputChannels != 0 {
		t.Errorf("InputChannels = %d with no session, want 0: nothing has negotiated, and a "+
			"grid sized from a guess is a matrix that stops the capture chain", got.InputChannels)
	}
	// EMPTY, NOT NULL. The routing screen tests Array.isArray before adopting a
	// map; a null takes the "this build cannot tell me" path instead of the
	// "nobody has chosen" one, and those draw differently on purpose.
	if got.Map == nil {
		t.Error("Map is nil; it must be an empty list, so the wire form is [] rather than null")
	}
	if len(got.Map) != 0 || !got.IsDefault {
		t.Errorf("got %+v, want an empty map reported as the default", got)
	}
}

func TestSetChannelMapWithNoSessionSaysToPressStart(t *testing.T) {
	a, _ := newTestApp(t)

	err := a.SetChannelMap(gst.ChannelMap{{Output: gst.OutputLeft, Input: 0, Gain: 1}})
	if err == nil {
		t.Fatal("SetChannelMap succeeded with no pipeline; there is no capture pad to route and " +
			"reporting success would leave the grid showing a routing that is not in force")
	}
	// The message has to say what to DO. "No pipeline" is a true statement an
	// operator cannot act on; "press START once" is the same fact as an
	// instruction, and it is what the screen itself says.
	if !strings.Contains(err.Error(), "START") {
		t.Errorf("SetChannelMap error = %q, want it to name START as the thing to do", err)
	}
}

func TestChannelMapCacheFollowsWhatIsInForceAndForgetsItWithTheSession(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	// A live re-route, recorded. This is the state the routing screen must draw
	// rather than the saved one: they are the same on a normal launch and differ
	// exactly when somebody re-routed and did not press Save, at which moment the
	// saved map is not what the commentator is being heard on.
	inForce := gst.ChannelMap{{Output: gst.OutputRight, Input: 4, Gain: 0.5}}
	a.chanMu.Lock()
	a.lastChannelMap = inForce
	a.lastInputChannels = 16
	a.chanMu.Unlock()

	got, err := a.GetChannelMap()
	if err != nil {
		t.Fatalf("GetChannelMap() error = %v", err)
	}
	if len(got.Map) != 1 || got.Map[0] != inForce[0] {
		t.Fatalf("GetChannelMap reported %+v, want the map in force %+v", got.Map, inForce)
	}
	if got.IsDefault {
		t.Error("a map with a contribution in it reported IsDefault; the screen would then say " +
			"nobody had chosen over a routing somebody did choose")
	}
	// The WIDTH comes from the pad, not from the cache, and there is no pipeline
	// here — so the live read is the authority and it says zero. That is the
	// point of reading it rather than replaying it: a cached sixteen would draw a
	// full grid of controls over a pipeline that no longer exists.
	if got.InputChannels != 0 {
		t.Errorf("InputChannels = %d with no pipeline, want the live answer 0", got.InputChannels)
	}

	drainPump(a)
	a.forgetChannelMap()

	queued := drainPump(a)
	if len(queued) != 1 || queued[0].name != EventChannelMap {
		t.Fatalf("forgetChannelMap queued %+v, want one %s event", queued, EventChannelMap)
	}
	end, ok := queued[0].data.(channelMapPayload)
	if !ok {
		t.Fatalf("the %s event carried a %T, want a channelMapPayload", EventChannelMap, queued[0].data)
	}
	if end.InputChannels != 0 || len(end.Map) != 0 || !end.IsDefault {
		t.Errorf("the end-of-session frame is %+v, want no channels and no map in force", end)
	}

	// And the cache with it, so a page loaded a minute later is not told the old
	// routing all over again — which would be worse than the event being missing,
	// because it would look freshly measured.
	after, err := a.GetChannelMap()
	if err != nil {
		t.Fatalf("GetChannelMap() after the session: %v", err)
	}
	if len(after.Map) != 0 || !after.IsDefault {
		t.Errorf("GetChannelMap after the session reported %+v, want the map forgotten", after)
	}
}

func TestChannelLevelsGoToTheirOwnEventAndRecordTheirWidth(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	frame := gst.Levels{
		PeakDB: []float64{-6, -12, -18, -24},
		RMSDB:  []float64{-9, -15, -21, -27},
	}
	a.channelLevelsForwarder()(frame)

	queued := drainPump(a)
	if len(queued) != 1 {
		t.Fatalf("the forwarder queued %d events, want exactly one: %+v", len(queued), queued)
	}
	// NEVER EventLevels. The two meters measure different points — this one is
	// upstream of the routing, the programme meter is what is actually encoded —
	// and a four-entry frame arriving on the programme meter's event would not
	// crash anything. It would make that meter flicker between two different
	// signals at two different widths, which is a meter reading as live while
	// showing something that is not going to air.
	if queued[0].name != EventChannelLevels {
		t.Fatalf("per-channel levels were emitted on %q, want %q", queued[0].name, EventChannelLevels)
	}
	got, ok := queued[0].data.(levelsPayload)
	if !ok {
		t.Fatalf("the %s event carried a %T, want a levelsPayload", EventChannelLevels, queued[0].data)
	}
	if len(got.Peak) != 4 || len(got.RMS) != 4 {
		t.Fatalf("the frame carried %d peaks and %d RMS values, want 4 of each: the LENGTH is "+
			"what the renderer lays its strips out from", len(got.Peak), len(got.RMS))
	}

	if w := a.chanLevelWidth.Load(); w != 4 {
		t.Errorf("chanLevelWidth = %d after a four-channel frame, want 4; the session's end sizes "+
			"its zero-frame from this, and a wrong width makes strips vanish instead of fall silent",
			w)
	}
}

func TestTheZeroFrameIsSentAtTheWidthTheMeterWasDrawnAt(t *testing.T) {
	// A sixteen-strip picker must FALL SILENT at the end of a session, not shrink
	// to two. Under the wire contract a changed array length means "the pipeline
	// was rebuilt, lay yourself out again", so a two-channel zero-frame would
	// rebuild the meter as two strips at the exact moment the session ended and
	// the operator would watch fourteen channels disappear.
	got := silentLevelsPayloadFor(16)
	if len(got.Peak) != 16 || len(got.RMS) != 16 {
		t.Fatalf("silentLevelsPayloadFor(16) produced %d/%d channels, want 16 of each",
			len(got.Peak), len(got.RMS))
	}
	for i := range got.Peak {
		if got.Peak[i] != levelsSilenceDB || got.RMS[i] != levelsSilenceDB {
			t.Fatalf("channel %d is %v/%v, want the silence floor %d on both",
				i, got.Peak[i], got.RMS[i], levelsSilenceDB)
		}
	}

	// The programme meter's own zero-frame is unchanged and is still the stereo
	// pair the encoder is pinned to. It is on air on Windows; this tier is a
	// widening, not a redesign.
	if prog := silentLevelsPayload(); len(prog.Peak) != gst.ChannelMapOutputs {
		t.Errorf("the programme zero-frame has %d channels, want %d",
			len(prog.Peak), gst.ChannelMapOutputs)
	}
}

func TestGstChannelMapKeepsTheZeroValueMeaningNobodyHasChosen(t *testing.T) {
	// The transcription's one load-bearing property. An empty saved map must stay
	// empty: internal/gst reads that as "nobody has chosen" and resolves it, at
	// the width the pad negotiated, to the card's first two embedded channels.
	// Returning an explicit default here instead would freeze today's default
	// into the pipeline AND take away the screen's ability to say that nobody has
	// chosen — the two failures are one edit apart.
	for _, saved := range [][]config.ChannelContribution{nil, {}} {
		if got := gstChannelMap(saved); got != nil || !got.IsDefault() {
			t.Errorf("gstChannelMap(%+v) = %+v, want a nil map that reports IsDefault", saved, got)
		}
	}

	saved := []config.ChannelContribution{
		{Output: 0, Input: 0, Gain: 1},
		{Output: 1, Input: 7, Gain: -0.25},
	}
	got := gstChannelMap(saved)
	if len(got) != len(saved) {
		t.Fatalf("gstChannelMap kept %d of %d contributions", len(got), len(saved))
	}
	for i := range saved {
		if got[i].Output != saved[i].Output || got[i].Input != saved[i].Input || got[i].Gain != saved[i].Gain {
			t.Errorf("contribution %d transcribed as %+v, want %+v", i, got[i], saved[i])
		}
	}
	// A map with contributions in it is not the default, however ordinary it
	// looks — somebody chose it, and the screen says so.
	if got.IsDefault() {
		t.Error("a two-contribution map reported IsDefault")
	}
}

func TestChannelMapPayloadCopiesTheMapItPublishes(t *testing.T) {
	// The pump holds a queued payload until the webview reads it, and a live
	// SetChannelMap can replace the cached map in the meantime. Sharing the slice
	// would let a queued event change after it was queued — a routing screen
	// showing a state that never existed.
	m := gst.ChannelMap{{Output: gst.OutputLeft, Input: 2, Gain: 1}}
	payload := channelMapPayloadFrom("decklink:2747401380", 16, m)

	m[0].Gain = 0.1
	if payload.Map[0].Gain != 1 {
		t.Error("the payload aliases the caller's map; a later live re-route would rewrite an " +
			"event that had already been published")
	}

	// AND THE WIDTH IS STAMPED WITH THE DEVICE IT WAS MEASURED ON. The routing
	// screen keys its grid on this, and a payload that dropped it would leave the
	// grid hidden on every seat — the gate cannot match a device it was not told
	// about. Spelled out here rather than built from config.AudioDeviceKey, so
	// that a change to that spelling has to be made deliberately in both places
	// instead of agreeing with itself.
	if payload.DeviceKey != "decklink:2747401380" {
		t.Errorf("the payload carried device key %q, want the one it was published for",
			payload.DeviceKey)
	}
	if payload.InputChannels != 16 {
		t.Errorf("the payload carried width %d, want 16", payload.InputChannels)
	}
}

func TestSenderOptsCarriesTheRoutingAndBothNewCallbacks(t *testing.T) {
	a, _ := newTestApp(t)

	// A store holding TWO devices' routings, with the key spelled out rather than
	// asked for: senderOpts must reach the one belonging to the device this
	// configuration captures from, and a test that built its key with the same
	// call the code under test uses would agree with any spelling at all.
	cfg := config.Defaults()
	cfg.AudioSourceKind = config.AudioSourceDeckLink
	cfg.DeckLinkPersistentID = "2747401380"
	cfg.ChannelMaps = map[string][]config.ChannelContribution{
		"decklink:2747401380": {{Output: gst.OutputLeft, Input: 4, Gain: 1}},
		"native:usb-mic-1":    {{Output: gst.OutputLeft, Input: 0, Gain: 1}},
	}

	opts := a.senderOpts(cfg, "")

	if len(opts.Pipeline.ChannelMap) != 1 || opts.Pipeline.ChannelMap[0].Input != 4 {
		t.Errorf("PipelineOpts.ChannelMap = %+v, want the routing saved for the CARD; without it "+
			"a card starts on channels 1 and 2 whatever the operator saved — and picking the "+
			"wrong device's entry would start it on somebody else's microphone",
			opts.Pipeline.ChannelMap)
	}
	// All three hooks, because each one is a whole feature that is inert without
	// it: no OnChannelLevels is a routing grid with dead meters, and no OnSignal
	// is a lamp that never moves off UNKNOWN over a card that has lost its input.
	if opts.Pipeline.OnLevels == nil {
		t.Error("PipelineOpts.OnLevels is nil; the input meters would never move")
	}
	if opts.Pipeline.OnChannelLevels == nil {
		t.Error("PipelineOpts.OnChannelLevels is nil; the routing grid's per-channel meters " +
			"would never move and the operator could not find the commentator")
	}
	if opts.Pipeline.OnSignal == nil {
		t.Error("PipelineOpts.OnSignal is nil; the signal lamp would stay UNKNOWN for the whole " +
			"of a match over a card that had lost its input, which nothing else can detect")
	}
}

// TestAnUnroutedConfigStartsWithNoMapAtAll is requirement five at this layer: a
// seat whose operator has never opened the routing screen must go on air with
// audio, and must do it by carrying NOTHING rather than by carrying a default
// somebody has to maintain.
func TestAnUnroutedConfigStartsWithNoMapAtAll(t *testing.T) {
	a, _ := newTestApp(t)

	opts := a.senderOpts(config.Defaults(), "")
	if !opts.Pipeline.ChannelMap.IsDefault() {
		t.Fatalf("a configuration nobody has routed produced %+v, want the zero map. The zero "+
			"value is what internal/gst resolves to the card's first two channels, and it is "+
			"bit-for-bit what this application sent before the routing screen existed",
			opts.Pipeline.ChannelMap)
	}
}
