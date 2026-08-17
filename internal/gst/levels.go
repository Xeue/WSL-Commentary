// Level-meter arithmetic shared by both build twins.
//
// Owner: WP-3a, with the rest of this package.
//
// This file is deliberately UNTAGGED. The rule in this package is that
// everything decidable without GStreamer is lifted into pure Go, so that Gate A
// runs it under -race and Gate B links the identical code: the cgo twin's bus
// handler and the stub twin's synthetic ticker both produce gst.Levels through
// the functions below, which is what stops the two builds disagreeing about
// what "-inf" or a NaN turns into on a meter. The cgo-only part — reading a
// GValueArray out of a level element message — stays in gst_cgo.go, but it
// hands the raw []any straight to levelsFrom here, and makes no decision of its
// own about the channel count, the floor, or what an unusable field means.
//
// # Widened from two channels to N (WP-DECKLINK, tier 2)
//
// Everything here used to be able to assume two channels. The specification's
// pipeline pins channels=2 upstream of the encoder, and every native capture
// endpoint the application had seen arrived as one or two. A DeckLink card does
// not: decklinkaudiosrc channels=16 negotiates SIXTEEN UNPOSITIONED channels
// (channel-mask 0x0), and an operator cannot pick the commentator out of
// sixteen unlabelled channels without seeing which bar moves when they talk.
// That meter is the entire reason the mapping UI is usable, so the model here
// carries N.
//
// MEASURED FIRST, because a widened model is worth nothing if the level element
// cannot do it. On the port machine, GStreamer 1.26.10:
//
//	audiotestsrc ! audio/x-raw,format=S16LE,rate=48000,channels=16,
//	  channel-mask=(bitmask)0x0,layout=interleaved
//	  ! level interval=50000000 ! fakesink
//
// posts ONE message per interval carrying peak, rms AND decay as GValueArrays
// of SIXTEEN doubles each. It does not truncate to two, it does not refuse an
// unpositioned mask, and it needs no property set to do it. Read back through
// go-gst rather than through gst-launch's printf, the two fields arrive as
// gobject.ValueArray of float64 — the same shape the stereo path already
// handles, sixteen entries long.
//
// Sixteen audiotestsrc at 3 dB steps, interleaved into one unpositioned
// sixteen-channel stream, came back as peaks of -0.0003, -3.0004, -6.0005 ...
// -45.013 dBFS IN INPUT ORDER. That is the fact the whole mapping UI rests on
// and it is worth stating plainly: ARRAY INDEX i IS INPUT CHANNEL i, and it
// stays index i for the life of the pipeline. Nothing above this package has to
// re-derive it per frame.
//
// The two-channel path is a WIDENING and not a replacement: silentLevels and
// stubLevelsAt still produce exactly the frames they produced before, byte for
// byte, so every native device behaves as it did on the on-air Windows build.
// The N-channel entry points sit beside them rather than through them.
//
// # Which level element a message came from
//
// A level element's messages are matched on the STRUCTURE name, and that name
// is "level" for every level element in the process. With one element that was
// merely sloppy. Tier 2 makes a SECOND possible — one metering the sixteen
// unpositioned channels for the picker, one metering the mixed-down stereo that
// is actually encoded — and then it is a silent cross-wire.
//
// MEASURED with both in one pipeline: the bus carries 39 level messages a
// second and EVERY ONE of them matches the structure name, so a handler that
// matches on the name alone feeds the programme meter a two-entry frame and a
// sixteen-entry frame alternately, twenty times a second each. The meter would
// not fail; it would flicker between two different signals, which is the kind
// of wrong a status display must never be. msg.Source() names the element and
// separated them perfectly — 39 attributed, 0 unattributed — so the routing
// policy is levelKindForSource below, kept here as a pure function so that Gate
// A tests it with no GStreamer present.

package gst

import "math"

// levelSilenceDB is the clamped floor of every delivered level, in dBFS.
//
// The level element reports -inf for digital silence, and -inf is unusable
// downstream twice over: encoding/json refuses it (the "levels" event would
// silently stop arriving the moment the commentator went quiet, which reads as
// a frozen meter), and a meter has a floor anyway. -100 matches the mixer's own
// digital-silence reading (frontend/src/ui/mixer/model.js measured -100.0 on an
// idle strip), so both meters in this application say silence with the same
// number.
//
// The clamp does real work on macOS and not only in the -inf case. Measured on
// the port machine with the actual pipeline — osxaudiosrc ! audioconvert !
// audioresample ! S16LE/48k/2ch ! level — the built-in microphone is a MONO
// CoreAudio device, audioconvert widens it to the stereo the encoder is pinned
// to, and the level element then reports the second channel as
// -699.99999984363217 dBFS rather than -inf. That is not silence-as-infinity,
// it is a very large finite negative number, and it would have gone through
// encoding/json intact and rendered as a bar somewhere below the bottom of the
// scale. The "below the floor clamps up" branch is what makes it read as
// silence, which is what it is. A commentator on a mono microphone therefore
// sees one live bar and one at the floor, correctly.
const levelSilenceDB = -100

// levelStubChannels is how many channels the stub twin synthesises for the
// PROGRAMME meter, and how many the zero-frame emitted at session end carries:
// the specification's pipeline pins channels=2 upstream of the encoder, so a
// level report from the real build's alevel is stereo and the fakes must match
// it. It is deliberately NOT the per-channel picker's count — see
// stubChannelLevelsAt, which takes the count as an argument because the whole
// point of the picker is that the count is whatever the card negotiated.
const levelStubChannels = 2

// levelMaxChannels is the largest channel count this package will build a
// frame for.
//
// It is not a memory bound — sixty-four channels of peak and RMS is a kilobyte
// per frame, twenty kilobytes a second, which is nothing. It is a SANITY bound
// with two jobs. GStreamer's own positioned-channel maximum is 64, so a level
// message longer than that means the negotiated caps or the message shape has
// become something this code was not written against, and inventing a meter for
// it would be pretending to know more than we do. And a channel picker is a
// list of strips the operator reads with their eyes: sixteen is the DeckLink
// card, sixty-four is a full Dante receive, and past that the UI has stopped
// being a picker whatever this package does.
//
// Over-long frames are TRUNCATED rather than rejected, because a meter showing
// the first sixty-four of sixty-five channels is a working meter with a
// documented edge, and an empty one is no meter at all. Index i is still input
// channel i for everything shown.
//
// # It is deliberately NOT channelmap.go's MaxInputChannels, which is 32
//
// Whoever notices the two numbers next will want to unify them. They are two
// ceilings on two different failures and unifying them would make one of the two
// wrong.
//
// A METER should show any width the element actually reports. Being handed more
// channels than expected is not a fault there — the worst case is a display with
// more strips on it than anybody wanted, and refusing to draw is strictly worse
// than drawing a lot.
//
// A MATRIX must refuse any width the pad did not negotiate, because a matrix of
// the wrong width does not misroute or attenuate: it stops the capture chain,
// measured, with "streaming stopped, reason error (-5)" out of the source and
// every coefficient in the matrix perfectly legal. Sixteen is what
// decklinkaudiosrc can present and therefore what this application will build a
// matrix for; a wider number arriving there is a garbled reading, not an
// opportunity.
//
// So the meter's bound is generous and the matrix's is exact, and they move for
// different reasons — this one when a wider device is worth METERING, that one
// when a wider device is worth ROUTING, with a measurement beside it.
const levelMaxChannels = 64

// The names of the level elements, and the only two names this package will
// accept a level message from.
//
// They live HERE rather than beside the other element-name constants in
// gst_cgo.go for one concrete reason: this file is untagged, so Gate A compiles
// it with no GStreamer present and the source guard in levels_test.go can
// assert the VALUES rather than only the text of a string literal it found by
// parsing. A guard that can only read the parse string cannot tell you that the
// handler and the pipeline agree about which element is which.
//
// alevel is the PROGRAMME meter: the exact S16LE 48 kHz stereo that is encoded
// and sent, measured after the cough mute and after the routing. It is what feeds
// OnLevels.
//
// # WHERE IT SITS, AND THE ONE PROMISE THAT CHANGED WITH IT
//
// In the shipping single pipeline it sits immediately before the AAC encoder, and
// that is what made OnLevels' promise absolute: no meter could move while silence
// went to air, because the buffers it measured were the buffers being encoded.
//
// In the ALWAYS-LIVE CAPTURE PIPELINE it sits immediately before aproxq and the
// proxysink — a leaky, one-second queue and a whole pipeline boundary above the
// encoder (capturedesc_cgo.go, seam.go). Same buffers in normal operation. During
// a SEND-SIDE STALL OF MORE THAN ABOUT A SECOND THEY ARE NOT THE SAME BUFFERS:
// aproxq is leaky=downstream, so the far end LOSES that second of audio while
// this meter goes on moving. That is A3, answered by the operator on 2026-08-16 —
// DROP, not delay, because a stall over a second is already a reconnect-class
// event and late audio is useless to a live switcher, and because the non-leaky
// alternative was measured dragging the preview to 7.2 fps and the meters to
// 7.2 msg/s and making the card itself drop packets.
//
// So the promise now reads: no meter can move while silence goes to air IN NORMAL
// OPERATION, and NOT during a stall. The detector for the stall is the send side's
// muxer watchdog (livewatch.go), not this meter, and that is the division of
// labour rather than an omission.
//
// chlevel is the PER-CHANNEL PICKER meter: it sits on the capture source's own
// output, upstream of the audioconvert that mixes sixteen unpositioned channels
// down to the encoder's two, so it measures what is COMING IN rather than what
// is going out. It is the meter the operator uses to find the commentator, and
// it is the whole reason msg.Source() has to be consulted.
const (
	levelElementName        = "alevel"
	channelLevelElementName = "chlevel"
)

// channelLevelIntervalNs is the interval property for the per-channel picker
// meter, in nanoseconds: 100 ms, ten frames a second, HALF the rate of the
// programme meter beside it.
//
// The decision was made against measurement rather than intuition, and the
// measurement says the cost is not where it looks. Ten seconds of a live
// sixteen-channel stream through level, on the port machine:
//
//	2ch  @ 50 ms   200 messages   0.11 s CPU
//	16ch @ 50 ms   200 messages   0.13 s CPU
//	16ch @ 100 ms  100 messages   0.11 s CPU
//	16ch @ 200 ms   50 messages   0.10 s CPU
//	16ch, no level element         0.07 s CPU
//
// Eight times the channels costs about 0.02 s of CPU in ten seconds and the
// interval barely moves it at all, because level accumulates EVERY SAMPLE
// whatever the interval is; the interval only decides how often it divides and
// posts. So GStreamer is not the thing being rationed here. What is being
// rationed is everything above the bus: two float64 slices allocated per
// message on a streaming thread, a JSON payload of 2N numbers, and a webview
// that has to lay them out — and that traffic is linear in channels TIMES rate.
// At 50 ms a sixteen-channel meter would be eight times the programme meter's
// wire traffic on the same bridge that carries it.
//
// The Go half of that is not the expensive half either, and it was measured
// rather than assumed: BenchmarkLevelsFrom in levels_test.go turns one message
// into a frame in 22 ns at two channels and 63 ns at sixteen, two allocations
// either way, 32 and 256 bytes. Ten frames a second of sixteen channels is
// under a microsecond of CPU and about 2.6 kB of garbage per second. So what is
// actually being rationed is the webview bridge and the redraw at the far end
// of it, which is why the number chosen here is about what the OPERATOR needs
// and not about what this package can afford.
//
// 100 ms is chosen because of what the meter is FOR. It answers "which of these
// sixteen moves when I talk", and speech syllables run at roughly 150-250 ms,
// so a talker is unmistakable at ten frames a second. It is NOT a clipping
// meter: clipping is watched on the programme meter, which stays at 50 ms
// because it shows what is actually going to air, and a channel hot enough to
// clip will clip there too the moment it is mapped. Different questions, and
// there is no reason both should be answered at the same rate.
//
// It is a STARTING VALUE and not a fixed cost, which is the other half of the
// decision. MEASURED on a PLAYING pipeline: setting interval took 32 us and the
// message rate changed immediately with no renegotiation, and setting
// post-messages=false took 61 us and stopped this element's messages dead —
// 0 in two seconds — while the other level element in the same pipeline carried
// on at its own rate, undisturbed, with the pipeline still in PLAYING. So the
// mapping UI can run this meter at 100 ms while its drawer is open and turn it
// off entirely when it is closed, and the steady-state cost of per-channel
// metering for a match nobody is remapping is zero.
const channelLevelIntervalNs = 100000000

// levelKind says which meter a level message belongs to. It is the routing
// policy for the bus handler, expressed as a value so that the decision is
// testable at Gate A with no bus, no message and no GStreamer.
type levelKind int

const (
	// levelKindUnknown is a level message this package did not ask for: a
	// level element somebody added to another pipeline in this process, or a
	// message whose source could not be named at all. It is DROPPED. That is
	// the deliberate choice — a level frame that cannot be attributed is
	// exactly the cross-wire this routing exists to prevent, and a meter fed
	// from an unknown element is worse than a meter that does not move.
	levelKindUnknown levelKind = iota

	// levelKindProgramme is alevel: the stereo actually being encoded and sent.
	levelKindProgramme

	// levelKindChannels is chlevel: the capture device's own channels, as many
	// as it negotiated, upstream of the mix down to the encoder's two.
	levelKindChannels
)

// levelKindForSource classifies a level message by the name of the element that
// posted it — msg.Source().GetName() in the real build.
//
// The empty string means the message had no source, which go-gst can hand back
// for a message posted by an element already being disposed. It is unknown, and
// unknown is dropped.
func levelKindForSource(name string) levelKind {
	switch name {
	case levelElementName:
		return levelKindProgramme
	case channelLevelElementName:
		return levelKindChannels
	default:
		return levelKindUnknown
	}
}

// clampLevelDB makes one dBFS reading deliverable: -inf, NaN and anything below
// the floor become levelSilenceDB, +inf becomes 0 (a meter's ceiling; the level
// element cannot legitimately report above digital full scale, so a +inf here
// is a bug upstream and full-scale red is the honest rendering of it).
func clampLevelDB(db float64) float64 {
	if math.IsNaN(db) {
		return levelSilenceDB
	}
	if db < levelSilenceDB {
		return levelSilenceDB
	}
	if math.IsInf(db, 1) {
		return 0
	}
	return db
}

// levelListToDB converts one field of a level element message — a GValueArray
// of G_TYPE_DOUBLE, one entry per channel, delivered by go-glib as []any — into
// a clamped []float64. Elements that are not doubles are reported as silence
// rather than skipped, so the channel count is preserved: a meter whose left
// bar silently became its right bar would be worse than one showing a silent
// channel.
//
// A nil or empty list returns nil, which the callers treat as "no usable
// reading in this message".
func levelListToDB(vals []any) []float64 {
	if len(vals) == 0 {
		return nil
	}
	out := make([]float64, len(vals))
	for i, v := range vals {
		switch n := v.(type) {
		case float64:
			out[i] = clampLevelDB(n)
		case float32:
			out[i] = clampLevelDB(float64(n))
		default:
			out[i] = levelSilenceDB
		}
	}
	return out
}

// levelsFrom builds one deliverable frame from the two raw message fields, and
// is the ONLY place a Levels is made from a level element's message.
//
// It exists so that the guarantee the consumers are given is enforced by the
// producer rather than asserted in a comment. That guarantee, in full:
//
//	len(PeakDB) == len(RMSDB), always, and that length is the channel count.
//	Index i is input channel i, for the life of the pipeline.
//	Every value is finite, and is between levelSilenceDB and 0 inclusive.
//
// It matters far more at sixteen channels than it did at two. A consumer that
// walks peak and indexes rms with the same subscript is the obvious way to draw
// sixteen strips, and it is a panic on a streaming thread the first time the
// two fields disagree about how many channels there are. gst-plugins-good's
// level fills both from the same channel count and MEASURED sixteen and sixteen
// on the DeckLink shape, so this cannot happen today — which is precisely why
// it must be enforced here, where a future level element or a future go-glib
// cannot quietly make it happen.
//
// A disagreement is resolved by taking the SHORTER of the two rather than
// padding the shorter with silence: a channel we have half a measurement for is
// a channel we cannot honestly draw, and one strip fewer is a smaller lie than
// one strip that reads as silent when it is not.
//
// ok=false means there was no usable reading in the message — an absent field,
// an empty array, a shape go-glib did not unwrap. The callers leave the meters
// alone, which is the correct degradation for a display: silent absence, never
// invented numbers, and never a crash on a streaming thread.
func levelsFrom(peakVals, rmsVals []any) (Levels, bool) {
	n := len(peakVals)
	if len(rmsVals) < n {
		n = len(rmsVals)
	}
	if n == 0 {
		return Levels{}, false
	}
	if n > levelMaxChannels {
		n = levelMaxChannels
	}
	peak := levelListToDB(peakVals[:n])
	rms := levelListToDB(rmsVals[:n])
	if peak == nil || rms == nil {
		return Levels{}, false
	}
	return Levels{PeakDB: peak, RMSDB: rms}, true
}

// # The wire shape of a per-channel meter frame, for whoever renders it
//
// The frame that leaves this package for the picker is a Levels, and it reaches
// the frontend as the "channelLevels" event (EVENT_CHANNEL_LEVELS in
// frontend/src/ui/backend.js, app.go's EventChannelLevels) carrying exactly this
// and nothing else:
//
//	{"peak": [ ...N numbers... ], "rms": [ ...N numbers... ]}
//
// Two parallel arrays of plain numbers, the SAME LENGTH as each other, dBFS,
// 0 being digital full scale and levelSilenceDB (-100) silence. Index i is
// input channel i of the capture device and stays index i for the life of the
// pipeline. It is deliberately the same shape as the programme meter's "levels"
// event, so that one meter model draws both and there is no second scale
// waiting to disagree with the first.
//
// THE FRAME IS NOT WHERE THE LAYOUT COMES FROM, and that is the design. The
// grid's width arrives once, on the "channelMap" event, as the width the
// capture pad actually NEGOTIATED — the same number the mix-matrix must be
// sized against, where a disagreement kills the pipeline instantly. So a
// renderer builds its strips from that event, which fires when a session starts
// and when a card is opened, and thereafter every channelLevels frame is 2N
// numbers written into strips that already exist. Nothing per frame inspects a
// length, allocates a label, or decides which channel a number belongs to.
//
// What this package owes that arrangement is the invariant levelsFrom enforces:
// len(peak) == len(rms), always. A renderer walking peak and indexing rms with
// the same subscript is the obvious way to draw sixteen strips and it must not
// be a way to panic.
//
// Two other shapes were considered and rejected; both look tidier at first.
//
// An array of per-channel objects — [{"ch":0,"peak":-12.3,"rms":-18.1}, ...] —
// is self-describing and is exactly the thing that has to be re-parsed per
// frame: sixteen object allocations, forty-eight key lookups and a channel
// index re-derived from something that has not changed in ninety minutes, ten
// times a second, on the webview's main thread.
//
// A generation or epoch counter alongside the arrays was rejected because it is
// redundant state that can disagree with them. The array length already changes
// exactly when the layout must be rebuilt, cannot get out of step with itself,
// and is cross-checked against the channelMap width the renderer laid out
// against. A second thing to compare would be a second answer to one question.

// silentLevels returns the all-silence frame: every channel at the floor.
//
// It is what the application emits once when a session ends, so the meter falls
// to nothing instead of freezing at the last level the pipeline happened to
// report — a frozen meter reads as a live one, which is the status-display
// direction this project never allows to be wrong.
//
// It is the stereo programme meter's zero-frame, unchanged. The per-channel
// picker needs the same thing at whatever width it was running at, which is
// silentLevelsFor.
func silentLevels() Levels {
	return silentLevelsFor(levelStubChannels)
}

// silentLevelsFor is silentLevels at an arbitrary channel count: the zero-frame
// for a meter that was showing N strips.
//
// The count MUST be the one the meter was last drawn at, and not a default.
// Sending a two-channel zero-frame to a sixteen-strip meter would change the
// array length, which under the wire contract above means "the pipeline was
// rebuilt, lay yourself out again" — so the meter would rebuild itself as two
// strips at the exact moment the session ended, and the operator would watch
// fourteen channels VANISH rather than fall silent. Falling to nothing is the
// entire purpose of the frame.
//
// A count below one returns a single silent channel rather than an empty frame,
// because an empty frame reads as "no usable reading" to every consumer and
// would leave the meter frozen at its last value — the thing this exists to
// prevent.
func silentLevelsFor(channels int) Levels {
	if channels < 1 {
		channels = 1
	}
	if channels > levelMaxChannels {
		channels = levelMaxChannels
	}
	peak := make([]float64, channels)
	rms := make([]float64, channels)
	for i := range peak {
		peak[i] = levelSilenceDB
		rms[i] = levelSilenceDB
	}
	return Levels{PeakDB: peak, RMSDB: rms}
}

// Stub-twin synthetic waveform. These constants and stubLevelAt are used only
// by gst_stub.go's ticker, but they live here untagged so the shape of the
// fake signal is a testable pure function rather than arithmetic buried in a
// goroutine.
const (
	// stubLevelLowDB..stubLevelHighDB is the triangle wave's range: loud enough
	// to cross the mixer convention's -18 amber and -6-adjacent red boundaries
	// during development, quiet enough to spend most of its time green.
	stubLevelLowDB  = -40
	stubLevelHighDB = -6

	// stubLevelPeriod is the triangle's period in ticker steps. At the 50 ms
	// tick that is a six-second sweep up and down — slow enough to watch, fast
	// enough that a stuck meter is obvious within seconds.
	stubLevelPeriod = 120

	// stubLevelRightOffset staggers the right channel by a quarter period, so
	// the two bars visibly move independently and a UI that accidentally draws
	// channel 0 twice fails by eye at Gate A.
	stubLevelRightOffset = stubLevelPeriod / 4

	// stubLevelRMSBelowPeakDB is how far the synthetic RMS sits under the
	// synthetic peak. Real programme keeps RMS below peak; 8 dB is a plausible
	// crest factor for speech and keeps the two bars visually distinct.
	stubLevelRMSBelowPeakDB = 8
)

// stubLevelAt returns the synthetic peak level, in dBFS, for one channel at one
// ticker step: a triangle wave from stubLevelLowDB up to stubLevelHighDB and
// back, with the right channel (and any further channels) offset so they do not
// move in lockstep. Deterministic in step, so a test can assert exact values.
func stubLevelAt(step, channel int) float64 {
	if step < 0 {
		step = 0
	}
	if channel < 0 {
		channel = 0
	}
	return stubLevelForPhase(step + channel*stubLevelRightOffset)
}

// stubLevelForPhase is the triangle itself, evaluated at an unreduced phase. It
// is factored out of stubLevelAt so that the N-channel synthesiser can use a
// different phase RULE without touching the waveform the two-channel path has
// always produced — the two-channel numbers are on air on Windows and this tier
// is a widening, not a redesign.
func stubLevelForPhase(phase int) float64 {
	if phase < 0 {
		phase = 0
	}
	phase %= stubLevelPeriod
	half := stubLevelPeriod / 2
	span := float64(stubLevelHighDB - stubLevelLowDB)
	if phase < half {
		return stubLevelLowDB + span*float64(phase)/float64(half)
	}
	return stubLevelHighDB - span*float64(phase-half)/float64(half)
}

// stubLevelsAt builds the whole synthetic Levels frame for one ticker step:
// peaks from stubLevelAt, RMS a fixed distance below, everything clamped
// through the same clampLevelDB the real build uses.
func stubLevelsAt(step int) Levels {
	peak := make([]float64, levelStubChannels)
	rms := make([]float64, levelStubChannels)
	for ch := range peak {
		p := stubLevelAt(step, ch)
		peak[ch] = clampLevelDB(p)
		rms[ch] = clampLevelDB(p - stubLevelRMSBelowPeakDB)
	}
	return Levels{PeakDB: peak, RMSDB: rms}
}

// stubChannelLevelsAt is the same synthetic frame for the PER-CHANNEL PICKER at
// an arbitrary channel count: the stub twin's stand-in for a sixteen-channel
// DeckLink, so that Gate A — and a developer with no card on the desk — can
// build and watch the mapping UI at the width it will really run at.
//
// IT DOES NOT USE stubLevelAt'S PHASE RULE, and the reason is a bug that would
// otherwise land in the one place the fake exists to catch things. That rule
// staggers each channel by stubLevelRightOffset, a QUARTER of the period, which
// is exactly right for a stereo pair and wraps every four channels: at sixteen
// channels, 0, 4, 8 and 12 would move in perfect lockstep, as would 1, 5, 9, 13
// and so on. Four groups of four identical bars is precisely the appearance of
// a picker that has drawn one channel four times, so the fake would be
// producing, by construction, the failure the fake is meant to expose.
//
// The rule here spreads the channels EVENLY over the period instead — channel c
// starts at c*period/channels — so no two of the sixteen ever have the same
// phase, and a UI that duplicates or transposes a strip fails by eye at Gate A.
// For two channels that is a half-period stagger rather than the quarter-period
// one stubLevelsAt uses; the two functions are deliberately not the same fake,
// because they are answering different questions and stubLevelsAt is the one
// whose output is already relied upon.
//
// The count is clamped into 1..levelMaxChannels for the same reasons
// silentLevelsFor clamps it.
func stubChannelLevelsAt(step, channels int) Levels {
	if step < 0 {
		step = 0
	}
	if channels < 1 {
		channels = 1
	}
	if channels > levelMaxChannels {
		channels = levelMaxChannels
	}
	// Integer division, and it does not need to divide exactly: what is
	// required is that the sixteen starting phases are DISTINCT, not that they
	// are evenly spaced to the sample. The floor is 1 so that a channel count
	// larger than the period still staggers rather than collapsing to zero.
	spread := stubLevelPeriod / channels
	if spread < 1 {
		spread = 1
	}
	peak := make([]float64, channels)
	rms := make([]float64, channels)
	for ch := range peak {
		p := stubLevelForPhase(step + ch*spread)
		peak[ch] = clampLevelDB(p)
		rms[ch] = clampLevelDB(p - stubLevelRMSBelowPeakDB)
	}
	return Levels{PeakDB: peak, RMSDB: rms}
}
