//go:build live && cgo && !gststub

// proxyseamcard_live_test.go is step 0b of the always-live capture plan: step
// 0a's three attach/detach cycles again, with THE REAL CARD upstream of the
// arming instead of videotestsrc and audiotestsrc.
//
// # What 0a could not settle, and this does
//
// 0a proved the go-gst call sequence — the IDLE probe's lifetime, the
// object-valued proxysink write, and that an unarmed second consumer really
// does get zero bytes. Every source in it was a test source, and a test source
// cannot fail in any of the ways this rig's producer can:
//
//   - decklinkvideosrc is not a Go-side timer; it is a driver callback thread
//     that queues frames and DROPS THE OLDEST when the queue backs up, saying so
//     as `Dropped N old frames`. If the arming probe blocks the tee's src pad
//     for longer than the card's frame period, that is where it shows.
//   - decklinkaudiosrc CANNOT PREROLL without a decklinkvideosrc in the same
//     pipeline — the card clocks audio off video — so the two legs of this seam
//     are not independent the way the two test sources were. A READY cycle that
//     disturbed the video leg would take the commentary with it.
//   - the card is EXCLUSIVE. There is no second attempt at opening it inside a
//     run, so a READY cycle that took a decklink element to READY and could not
//     bring it back would not be a slow leg, it would be a dead rig.
//
// The arming does NOT touch either decklink element — it cycles the proxy queue
// and the proxysink only — but "it should not reach them" is exactly the kind of
// claim this project measures rather than asserts.
//
// # The rig, and the one thing that must never be set
//
// UltraStudio 4K Mini on Thunderbolt, persistent-id 2747401380. NO VIDEO SIGNAL
// on the video input: decklinkvideosrc posts `Signal lost` once and, with
// drop-no-signal-frames=false, goes on producing frames, which is all the
// commentary leg needs from it. The microphone is on the card's MIC input,
// selected in Blackmagic Desktop Video Setup.
//
// `connection` IS NOT SET, on any element, in this file or anywhere else. It is
// not a per-pipeline selection: it PERSISTENTLY RECONFIGURES THE CARD and
// overrides what the operator chose in Desktop Video Setup, so a throwaway probe
// that sets it does not read the wrong input, it breaks the rig for everything
// afterwards until a human fixes it by hand. This has been got wrong twice and
// corrected by hand twice.
//
// # The three proofs, and why the second one is the whole point
//
//  1. Non-zero transport-stream bytes on cycles 2 AND 3. Cycle 1 proves nothing:
//     the hazard is specifically about every consumer after the FIRST.
//  2. Capture-side level messages CONTINUOUS across all three cycles, with no
//     wall-clock gap greater than twice the element's interval. R1's whole claim
//     is that the meters and the preview keep working while the send pipeline
//     comes and goes; a seam that delivered air but stuttered the meters at
//     every START would have failed the requirement while passing proof 1.
//  3. The card's own frame rate unchanged across the cycles, and no
//     `Dropped N old frames` — the reading that says the arming probe's block on
//     the tee's src pad is short enough that the driver never backs up.
//
// # What it measured, 2026-08-17, on the fitted card
//
// Three armed cycles then one unarmed control, `-race`, GStreamer 1.26.10:
//
//	cycle 1 ARMED    1023848 bytes   mux:src 778 buffers   first at +118 ms
//	cycle 2 ARMED    1027796 bytes   mux:src 781 buffers   first at  +34 ms
//	cycle 3 ARMED    1025164 bytes   mux:src 779 buffers   first at  +46 ms
//	cycle 4 CONTROL        0 bytes   mux:src   0 buffers   first at   never
//
// The arming cost 170-240 microseconds for BOTH branches together, on a
// pipeline carrying the card's picture and its sixteen channels — the same order
// as the 176-511 microseconds step 0a measured against test sources, so the real
// producer does not change the price. Every armed cycle carried STREAM_START,
// CAPS and SEGMENT across both proxysrcs; the control carried NO EVENT AT ALL,
// stopped at ONE buffer at vproxsrc:src, and put `venc: negotiation problem` on
// the send bus 7 ms after PLAYING.
//
// The capture pipeline did not notice any of it. alevel posted 360 messages with
// a largest wall-clock gap of 68 ms against its 100 ms limit; chlevel posted 180
// with a largest gap of 101 ms against 200 ms; neither element's own running-time
// ever stepped by more than exactly one interval, so nothing was late AND
// nothing was lost. decklinkvideosrc ran at 29.87 / 29.97 / 29.97 / 29.97 fps
// across the four cycles, and GStreamer's debug log recorded neither
// `Dropped N old frames` nor `Dropped N old packets` after the settle.
//
// Two numbers here are worth carrying forward into step 5. The seam ran at
// 50 fps — 200 buffers per four-second cycle at vproxsrc:src — while the card
// ran at 29.97, because videorate sits in the CAPTURE pipeline: PLAN.md section
// 3.6's claim that the caps crossing the proxy never change is not an argument,
// it is this measurement. And the control pushed 187 buffers at aq:src, which is
// what a HEALTHY cycle pushes, so section 3.4's audio watchdog pad reads full
// rate over a completely dead feed on the card exactly as it did on test
// sources. Only vq:src and mux:src went quiet.
//
// THE CARD IS EXCLUSIVE. The capture pipeline is taken to NULL from a t.Cleanup
// registered before it is ever started, because an orphan holds the card from
// the operator and from every other test.

package gst

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// cardCaptureDescription is THE SHIPPED FUSED STRING: captureDescription for
// PLAN.md section 2.6 — the card's picture and the card's commentary in one
// always-live capture pipeline, both legs ending in a leaky queue and a proxysink,
// preview off.
//
// It was a literal here when this file was written, because capturedesc_cgo.go
// did not exist. It does now, and this test measures the string the application
// parses rather than a copy that agrees with it today. The renames that would
// otherwise split them silently — an element name, a queue depth, the order of
// the conform chain — now break this run instead of passing beside it.
//
// Two properties are absent from the string and set with g_object_set after
// parse, exactly as the shipping code does it: persistent-id on both decklink
// elements (it is a gint64 and 2747401380 does not survive the parser's integer
// handling as a gint), and the audioconvert mix-matrix, which is a GST_TYPE_ARRAY.
// `connection` is absent and is set nowhere, here or in captureDescription.
var cardCaptureDescription = captureDescription(
	CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard},
	ConformTarget{Width: 1920, Height: 1080, FrameRateNum: 50, FrameRateDen: 1},
	"")

// The two level elements' intervals, and the gap that fails the run.
//
// TWICE THE INTERVAL is the plan's tolerance, and on this card it has real room
// in it rather than being a number chosen to pass.
//
// decklinkaudiosrc delivers audio in VIDEO-FRAME-SIZED buffers, which is the
// same fact as the card clocking audio off video and is visible directly in the
// summary this test prints: 627 buffers at asrc:src against 627 frames at
// vcapsrc:src over one run, so one buffer per frame, 33.4 ms at the card's
// no-signal 29.97 fps. level emits on a buffer boundary once it has accumulated
// an interval's worth of samples, so alevel's 50 ms interval lands on that
// 33.4 ms grid as an alternating 33 / 67 ms pattern and chlevel's 100 ms as
// 100 / 133 ms. Measured worst cases across the runs: alevel 68 ms of the 100 ms
// allowed, chlevel 101 ms of 200 ms. Both are quantisation, and anything that
// pushes them past the limit is a producer that stopped.
const (
	channelLevelInterval   = 100 * time.Millisecond
	programmeLevelInterval = 50 * time.Millisecond
)

// cardSettleTime is how long the capture pipeline runs before anything is
// measured.
//
// It is not a fudge: decklinkaudiosrc reports dropping its first packets on
// every start — an ordinary consequence of the card having been running before
// the pipeline attached to it — and decklinkvideosrc posts `Signal lost` and
// renegotiates once as it settles on the no-signal raster. Neither is a
// measurement of the seam.
const cardSettleTime = 3 * time.Second

// ---------------------------------------------------------------------------
// The capture-side detector: level continuity
// ---------------------------------------------------------------------------

// levelArrival is one level message, stamped with WALL CLOCK arrival.
//
// Wall clock rather than the message's own running-time, and the distinction is
// the measurement rather than a detail. level computes its report from buffer
// CONTENT, so a capture leg that stalled for a second and then resumed still
// posts a running-time sequence with no hole in it — the second of silence is
// simply reported late. The stall is visible ONLY in when the message arrived.
// The running-time is carried alongside so that a leg which genuinely lost
// samples can be told from one that was merely late.
type levelArrival struct {
	at          time.Time
	runningTime time.Duration
}

// cardLevelWatch is the capture pipeline's bus sync handler: it records every
// level message's arrival, per posting element, and the errors and warnings.
//
// It returns BusDrop for every message, matching production and 0a's
// busRecorder: nothing else in this process reads this bus, and a sync handler
// that passes messages on to a bus with no watch is a slow leak — measured at
// 7,168 queued messages in the capture probe that let one through.
//
// It never touches *testing.T. A bus handler can fire after the test function
// has returned, and a t.Logf from there panics the whole binary.
type cardLevelWatch struct {
	mu       sync.Mutex
	arrivals map[string][]levelArrival
	errors   []busEntry
	warnings []busEntry
}

func newCardLevelWatch() *cardLevelWatch {
	return &cardLevelWatch{arrivals: make(map[string][]levelArrival, 2)}
}

func (w *cardLevelWatch) handler(_ gogst.Bus, msg *gogst.Message) gogst.BusSyncReply {
	if msg == nil {
		return gogst.BusDrop
	}
	now := time.Now()
	source := "pipeline"
	if src := msg.Source(); src != nil {
		source = src.GetName()
	}
	switch msg.Type() {
	case gogst.MessageElement:
		// The same two tests onBusMessage makes, in the same order and for the
		// same reason: the structure name is nearly free and rejects everything
		// that is not a level report, and the SOURCE is what separates chlevel's
		// sixteen unpositioned input channels from alevel's encoded stereo. Both
		// post a structure called "level" and this test asserts a different
		// tolerance for each.
		s := msg.GetStructure()
		if s == nil || s.GetName() != levelStructureName {
			break
		}
		a := levelArrival{at: now, runningTime: structureClockTime(s, "running-time")}
		w.mu.Lock()
		w.arrivals[source] = append(w.arrivals[source], a)
		w.mu.Unlock()

	case gogst.MessageError:
		_, gerr := msg.ParseError()
		w.mu.Lock()
		w.errors = append(w.errors, busEntry{now, fmt.Sprintf("%s: %v", source, gerr)})
		w.mu.Unlock()

	case gogst.MessageWarning:
		_, gerr := msg.ParseWarning()
		w.mu.Lock()
		w.warnings = append(w.warnings, busEntry{now, fmt.Sprintf("%s: %v", source, gerr)})
		w.mu.Unlock()
	}
	return gogst.BusDrop
}

func (w *cardLevelWatch) snapshot(source string) []levelArrival {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]levelArrival(nil), w.arrivals[source]...)
}

func (w *cardLevelWatch) bus() (errors, warnings []busEntry) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]busEntry(nil), w.errors...), append([]busEntry(nil), w.warnings...)
}

// structureClockTime reads a GstClockTime field out of a level message.
//
// go-glib hands a guint64 back as one of several Go integer types depending on
// how the GValue was built, so the type switch is not defensive: a wrong guess
// here would silently report every running-time as zero and make the continuity
// report say the capture never advanced.
func structureClockTime(s *gogst.Structure, field string) time.Duration {
	switch v := s.GetValue(field).(type) {
	case uint64:
		return time.Duration(v)
	case int64:
		return time.Duration(v)
	case gogst.ClockTime:
		return time.Duration(v)
	default:
		return -1
	}
}

// phase is one bracketed window of the run, so a gap can be attributed to the
// cycle it happened in rather than merely reported.
type phase struct {
	name  string
	start time.Time
	end   time.Time
}

// levelGap is the largest wall-clock interval between two consecutive level
// messages, and where it fell.
type levelGap struct {
	largest time.Duration
	at      time.Time
	phase   string
	count   int
}

// largestLevelGap walks one element's arrivals from the first at or after
// `from` and returns the largest interval between consecutive messages.
//
// The gaps are computed over the WHOLE sequence and then attributed to phases,
// rather than per phase in isolation. Per-phase windows would miss the gap that
// matters most: a stall that begins in the last message of one cycle and ends in
// the first message of the next straddles the boundary, and that boundary is
// precisely the instant the send pipeline is being torn down and rebuilt.
func largestLevelGap(arrivals []levelArrival, from time.Time, phases []phase) levelGap {
	out := levelGap{largest: -1}
	var prev time.Time
	for _, a := range arrivals {
		if a.at.Before(from) {
			continue
		}
		out.count++
		if !prev.IsZero() {
			if gap := a.at.Sub(prev); gap > out.largest {
				out.largest, out.at = gap, prev
				out.phase = phaseAt(phases, prev)
			}
		}
		prev = a.at
	}
	return out
}

func phaseAt(phases []phase, at time.Time) string {
	for _, p := range phases {
		if !at.Before(p.start) && at.Before(p.end) {
			return p.name
		}
	}
	return "outside every phase"
}

// runningTimeHole reports the largest jump in a level element's own running-time
// sequence, in units of its interval.
//
// A healthy sequence advances by exactly one interval per message. A jump of two
// or more means the element was handed a discontinuity — samples the card never
// delivered — which is a different failure from a late message and wants a
// different answer.
func runningTimeHole(arrivals []levelArrival, from time.Time, interval time.Duration) (worst time.Duration, at time.Duration) {
	worst, at = -1, -1
	var prev time.Duration = -1
	for _, a := range arrivals {
		if a.at.Before(from) || a.runningTime < 0 {
			continue
		}
		if prev >= 0 {
			if step := a.runningTime - prev; step > worst {
				worst, at = step, prev
			}
		}
		prev = a.runningTime
	}
	return worst, at
}

// ---------------------------------------------------------------------------
// The card's own frame rate, sampled per phase
// ---------------------------------------------------------------------------

// rateSampler turns a cumulative flowCounter into a per-phase frame rate.
//
// The card's rate with no signal on the video input is whatever placeholder
// raster the driver settles on, and this test does not care what that number is
// — only that it is THE SAME in every phase. An arming probe that cost the
// producer frames shows up as a phase with a lower rate than its neighbours, and
// that comparison needs no prior knowledge of the card's mode.
type rateSampler struct {
	c     *flowCounter
	count int64
	at    time.Time
}

func newRateSampler(c *flowCounter) *rateSampler {
	n, _ := c.read()
	return &rateSampler{c: c, count: n, at: time.Now()}
}

func (r *rateSampler) sample() (fps float64, frames int64, window time.Duration) {
	n, _ := r.c.read()
	now := time.Now()
	frames, window = n-r.count, now.Sub(r.at)
	r.count, r.at = n, now
	if window <= 0 {
		return 0, frames, window
	}
	return float64(frames) / window.Seconds(), frames, window
}

// ---------------------------------------------------------------------------
// The test
// ---------------------------------------------------------------------------

// TestLiveProxySeamOnTheCard is step 0b's proof.
//
// Run it alone. It opens the exclusive DeckLink, and a second test in the same
// binary that also opens the card would fail on whichever of the two lost the
// race rather than on anything this file is about:
//
//	CGO_LDFLAGS="-framework UniformTypeIdentifiers" \
//	WSLCOMMS_LIVE_APP_DIR=<symlink farm>/Contents/MacOS \
//	GST_DEBUG=2 GST_DEBUG_FILE=<log> WSLCOMMS_LIVE_GST_LOG=<log> \
//	go test -tags "live dev" -run TestLiveProxySeamOnTheCard -v -count=1 ./internal/gst/
func TestLiveProxySeamOnTheCard(t *testing.T) {
	liveInitDarwin(t)

	card := env("WSLCOMMS_LIVE_CARD", defaultLiveCard)
	encoderName, err := selectH264Encoder()
	if err != nil {
		t.Fatalf("selectH264Encoder: %v", err)
	}
	t.Logf("card %s; H.264 encoder %s; AAC encoder %s", card, encoderName, aacEncoderFactory)

	// ---------------------------------------------------------------- capture
	t.Logf("capture description (PLAN.md 2.6, FUSED):\n%s", cardCaptureDescription)
	element, err := gogst.ParseLaunch(cardCaptureDescription)
	if err != nil {
		t.Fatalf("gst_parse_launch on the card capture description failed: %v", err)
	}
	capture, ok := element.(gogst.Pipeline)
	if !ok {
		t.Fatalf("gst_parse_launch returned a %T, not a GstPipeline", element)
	}
	// REGISTERED BEFORE THE PIPELINE IS EVER STARTED, and before any t.Fatalf
	// below can be reached. The card is exclusive; an orphan holds it from the
	// operator until the machine is rebooted or the process is killed.
	t.Cleanup(func() {
		capture.BlockSetState(gogst.StateNull, gogst.ClockTime(10*time.Second))
	})

	// Both decklink elements by persistent-id, through the SAME
	// configureDeckLinkSource the application uses, so one id reaches both by
	// one route. persistent-id is a gint64 and this card's id is past a gint;
	// the shipping helper is what knows that.
	for _, name := range []string{nameVideoCaptureSrc, nameAudioSrc} {
		if err := configureDeckLinkSource(mustGetElement(t, capture, name), card); err != nil {
			t.Fatalf("configureDeckLinkSource(%s, %s): %v", name, card, err)
		}
	}

	// THE MIX MATRIX, before the pipeline leaves NULL.
	//
	// The card's sixteen channels come back UNPOSITIONED (channel-mask=0x0) and
	// audioconvert has nothing to derive a downmix from, so the chain dies with
	// not-negotiated 0.069 s after PLAYING without one. The matrix is OUTPUT
	// ROWS x INPUT COLUMNS and its width must be what the pad negotiates, which
	// is asserted against the pad's own caps once it is PLAYING.
	aconv := mustGetElement(t, capture, nameAudioConv)
	matrix, err := DefaultChannelMap(deckLinkAudioChannels).MixMatrix(deckLinkAudioChannels)
	if err != nil {
		t.Fatalf("MixMatrix(%d): %v", deckLinkAudioChannels, err)
	}
	if !hasProperty(aconv, propMixMatrix) {
		t.Fatalf("this build's audioconvert has no %s property", propMixMatrix)
	}
	gogst.UtilSetObjectArg(aconv, propMixMatrix, mixMatrixArg(matrix))

	// chlevel ships with post-messages=false — the per-channel meter is turned
	// on only when the routing panel is open. It is turned on here because it is
	// the SECOND independent continuity reading, at a different interval and on
	// the far side of the mix matrix from alevel, and two detectors that fail
	// together say more than one that fails alone.
	mustGetElement(t, capture, channelLevelElementName).SetObjectProperty(propPostMessages, true)

	levels := newCardLevelWatch()
	if b := capture.GetBus(); b != nil {
		b.SetSyncHandler(levels.handler)
	} else {
		t.Fatal("the capture pipeline has no bus")
	}

	// THE CARD'S OWN OUTPUT, upstream of everything the arming touches. This is
	// proof 3's reading and it is taken at the source pad rather than at the
	// proxysink, so a frame lost between them is visible as a difference between
	// the two counts rather than hidden by both dropping together.
	cardFrames := &flowCounter{}
	addFlowProbe(t, capture, nameVideoCaptureSrc, "src", cardFrames)
	cardAudio := &flowCounter{}
	addFlowProbe(t, capture, nameAudioSrc, "src", cardAudio)

	// Both legs' liveness at the seam itself: the guard against the measured
	// catastrophe on the other side of section 3.1's decision, where a READY
	// cycle takes the capture leg down permanently and every send cycle still
	// looks plausible because the send pipeline reads zero either way.
	videoLeg, audioLeg := &flowCounter{}, &flowCounter{}
	addFlowProbe(t, capture, probeVideoProxySink, "sink", videoLeg)
	addFlowProbe(t, capture, probeAudioProxySink, "sink", audioLeg)

	// PLAN.md section 3.5, in this order, before any state change.
	clock := gogst.SystemClockObtain()
	if clock == nil {
		t.Fatal("gst_system_clock_obtain returned nil")
	}
	capture.UseClock(clock)
	capture.SetStartTime(gogst.ClockTimeNone)
	base := clock.GetTime()
	capture.SetBaseTime(base)

	captureUp := time.Now()
	if ret := capture.BlockSetState(gogst.StatePlaying, gogst.ClockTime(20*time.Second)); !stateChangeOK(ret) {
		errs, _ := levels.bus()
		t.Fatalf("the CAPTURE pipeline would not go to PLAYING (%s); bus errors: %v\n"+
			"On this rig the usual cause is the card being held by something else — it is "+
			"exclusive — or decklinkaudiosrc having no decklinkvideosrc to clock it",
			ret, describeBus(errs, captureUp))
	}
	t.Logf("capture PLAYING after %s", time.Since(captureUp))

	// Settle. Nothing before measureFrom is measured.
	//
	// THE SAMPLER IS OPENED FIRST, and the first version of this file did not do
	// that: it constructed the sampler after the wait and took its first reading
	// immediately, so the reference rate every cycle is compared against was
	// 0 frames over a 0-second window. The run failed on its own guard rather
	// than reporting a card that was working perfectly well, which is the right
	// way round but is worth a line so it is not reintroduced.
	settleUntil := time.Now().Add(cardSettleTime)
	rate := newRateSampler(cardFrames)
	waitUntil(settleUntil)
	measureFrom := time.Now()
	debugLog := openCardDebugLog(t)

	// The negotiated width, from the pad's OWN caps rather than from the device's
	// max-channels. The matrix above was written for deckLinkAudioChannels before
	// NULL; if the pad negotiated anything else the matrix is the wrong shape,
	// and a matrix of the wrong shape does not degrade the feed, it stops it.
	if pad := aconv.GetStaticPad("sink"); pad != nil {
		if caps := pad.GetCurrentCaps(); caps != nil && caps.GetSize() > 0 {
			s := caps.GetStructure(0)
			n, _ := s.GetInt("channels")
			t.Logf("%s:sink negotiated %s", nameAudioConv, caps.String())
			if int(n) != deckLinkAudioChannels {
				t.Errorf("the capture pad negotiated %d channels but the matrix was written for "+
					"%d. Every number below is measured through a matrix of the wrong width",
					n, deckLinkAudioChannels)
			}
		}
	}

	settleFPS, settleFrames, settleWindow := rate.sample()
	t.Logf("settle: %s:src %d frames in %s (%.2f fps)",
		nameVideoCaptureSrc, settleFrames, settleWindow.Round(time.Millisecond), settleFPS)
	if settleFrames == 0 {
		t.Fatalf("SHIP-BLOCKER: %s produced no frames at all in %s of settling. Nothing below "+
			"measures the seam; this is the card not capturing",
			nameVideoCaptureSrc, cardSettleTime)
	}

	dir := t.TempDir()

	// ------------------------------------------------------------- the cycles
	//
	// Three armed, then one unarmed control, identical in shape to step 0a so
	// the two runs are directly comparable. Cycle 1's arming is a no-op — nothing
	// has consumed this proxysink yet — so CYCLE 2 AND CYCLE 3 ARE THE RESULT,
	// and cycle 4 is what says the hazard is still there to be fixed.
	//
	// WSLCOMMS_LIVE_SKIP_PROXY_ARMING=1 makes the same binary skip every arming,
	// which is the code as it would stand if step 5 were written without section
	// 3.1. THAT RUN MUST FAIL.
	skipArming := os.Getenv("WSLCOMMS_LIVE_SKIP_PROXY_ARMING") == "1"
	if skipArming {
		t.Log("WSLCOMMS_LIVE_SKIP_PROXY_ARMING=1: no cycle will be armed. This run MUST FAIL; " +
			"a pass means the test does not measure what it claims to.")
	}

	type cyclePhase struct {
		cycle  seamCycle
		fps    float64
		frames int64
		window time.Duration
		legVid int64
		legAud int64
	}

	phases := []phase{{name: "settle", start: measureFrom}}
	var results []cyclePhase
	for n := 1; n <= 4; n++ {
		phases[len(phases)-1].end = time.Now()
		start := time.Now()
		c := runSeamCycle(t, capture, dir, n, n <= 3 && !skipArming, encoderName, clock, base)
		fps, frames, window := rate.sample()
		v, _ := videoLeg.read()
		a, _ := audioLeg.read()
		results = append(results, cyclePhase{cycle: c, fps: fps, frames: frames, window: window,
			legVid: v, legAud: a})
		phases = append(phases, phase{name: fmt.Sprintf("cycle %d", n), start: start, end: time.Now()})
		t.Logf("cycle %d: %s:src %d frames in %s (%.2f fps)",
			n, nameVideoCaptureSrc, frames, window.Round(time.Millisecond), fps)
	}
	phases[len(phases)-1].end = time.Now().Add(time.Hour) // the last phase is open-ended

	// ------------------------------------------- proof 0: the capture survived
	//
	// Asserted before anything else, because if the capture pipeline died the
	// send-side numbers mean nothing at all.
	state, pending, ret := capture.GetState(0)
	if state != gogst.StatePlaying {
		t.Errorf("SHIP-BLOCKER: after four attach/detach cycles the CAPTURE pipeline is in %s "+
			"(pending %s, %s), not PLAYING", state, pending, ret)
	}
	prevVid, prevAud := int64(0), int64(0)
	for i, r := range results {
		if r.legVid <= prevVid {
			t.Errorf("SHIP-BLOCKER: the VIDEO capture leg produced nothing during cycle %d "+
				"(%d buffers at %s:sink, unchanged). The READY cycle has stopped the producer, "+
				"which is the failure PLAN.md section 3.1 chose READY over NULL to avoid",
				i+1, r.legVid, probeVideoProxySink)
		}
		if r.legAud <= prevAud {
			t.Errorf("SHIP-BLOCKER: the AUDIO capture leg produced nothing during cycle %d "+
				"(%d buffers at %s:sink, unchanged)", i+1, r.legAud, probeAudioProxySink)
		}
		prevVid, prevAud = r.legVid, r.legAud
	}

	// ------------------------------- proof 1: transport stream on cycles 2 AND 3
	//
	// THE ASSERTION IS ON ENCODED BYTES ON DISK. A fakesink accepts buffers with
	// no caps — 234 of them "flowed" in the C rig while the real chain produced
	// zero — so nothing but a real encoder chain's output settles this.
	for _, i := range []int{1, 2} {
		c := results[i].cycle
		// Both failures below have to say whether this cycle was actually armed:
		// the falsification run (WSLCOMMS_LIVE_SKIP_PROXY_ARMING=1) reaches them
		// with armed=false, and that is the one run whose output somebody reads
		// line by line to satisfy themselves the test can fail.
		why := "was ARMED and"
		if !c.armed {
			why = "had its arming SKIPPED (WSLCOMMS_LIVE_SKIP_PROXY_ARMING=1) and"
		}
		if c.fileBytes == 0 {
			t.Errorf("SHIP-BLOCKER cycle %d %s wrote ZERO bytes of transport stream from the "+
				"CARD. This is the silent dead feed: SRT would have connected and the lamp would "+
				"have gone green over nothing. %s:src saw %d buffer(s) and these events: %s",
				c.n, why, probeVideoProxySrc, c.videoSrcBuffers, c.videoSrcEvents)
			continue
		}
		if !c.sawVideoCaps || !c.sawAudioCaps || !c.sawVideoSegment || !c.sawAudioSegment {
			t.Errorf("cycle %d %s did not carry the sticky events across the seam: "+
				"%s:src caps=%v segment=%v, %s:src caps=%v segment=%v",
				c.n, why, probeVideoProxySrc, c.sawVideoCaps, c.sawVideoSegment,
				probeAudioProxySrc, c.sawAudioCaps, c.sawAudioSegment)
		}
		if len(c.busErrors) > 0 {
			t.Errorf("cycle %d: the send bus recorded %d error(s): %v", c.n, len(c.busErrors), c.busErrors)
		}
	}
	// Cycle 1 is reported rather than asserted on: it is the first consumer this
	// proxysink has ever had and it would produce bytes with or without the fix,
	// so a failure there would be a rig fault and not this hazard.
	if results[0].cycle.fileBytes == 0 {
		t.Errorf("cycle 1 — the FIRST consumer, which needs no arming to work — wrote zero " +
			"bytes. That is not the proxysink hazard; the capture or the encoder chain is broken " +
			"and cycles 2 and 3 below prove nothing either way")
	}
	// The three armed cycles carry comparable media. A cycle that carries a
	// fraction of it is a partially armed seam, which reaches air as a feed that
	// drops out rather than one that never starts.
	var largest int64
	for _, r := range results[:3] {
		if r.cycle.fileBytes > largest {
			largest = r.cycle.fileBytes
		}
	}
	for _, r := range results[:3] {
		if c := r.cycle; c.fileBytes > 0 && c.fileBytes*2 < largest {
			t.Errorf("cycle %d wrote %d bytes against a best armed cycle of %d — less than half",
				c.n, c.fileBytes, largest)
		}
	}

	// ------------------------------------- proof 2: the meters never hiccupped
	//
	// This is the reading R1 exists for. The send pipeline is built, run for four
	// seconds and destroyed four times over; the capture-side meters must not so
	// much as stutter while it happens.
	for _, m := range []struct {
		name     string
		interval time.Duration
	}{
		{levelElementName, programmeLevelInterval},
		{channelLevelElementName, channelLevelInterval},
	} {
		arrivals := levels.snapshot(m.name)
		gap := largestLevelGap(arrivals, measureFrom, phases)
		limit := 2 * m.interval
		hole, holeAt := runningTimeHole(arrivals, measureFrom, m.interval)
		t.Logf("%s: %d messages after settling, largest wall-clock gap %s (limit %s), during %s; "+
			"largest step in its own running-time %s at %s",
			m.name, gap.count, gap.largest.Round(time.Millisecond), limit, gap.phase,
			hole.Round(time.Millisecond), holeAt.Round(time.Millisecond))
		switch {
		case gap.count == 0:
			t.Errorf("SHIP-BLOCKER: %s posted NOTHING after the settle. The capture leg is not "+
				"producing and the whole point of always-live capture — meters before air — is "+
				"not working at all", m.name)
		case gap.largest > limit:
			t.Errorf("SHIP-BLOCKER: %s went quiet for %s during %s, more than twice its %s "+
				"interval. The send pipeline coming and going is disturbing the CAPTURE "+
				"pipeline, which is the one thing this split exists to prevent: the operator "+
				"sees the meters freeze every time they press START",
				m.name, gap.largest.Round(time.Millisecond), gap.phase, m.interval)
		}
		if hole > limit {
			t.Errorf("%s's own running-time jumped %s at %s, more than twice its %s interval. "+
				"That is not a late message, it is samples the card never delivered",
				m.name, hole.Round(time.Millisecond), holeAt.Round(time.Millisecond), m.interval)
		}
	}

	// ------------------------------ proof 3: the card's rate, and dropped frames
	//
	// The card's no-signal placeholder rate is whatever the driver settles on and
	// this test asserts nothing about its value — only that the four phases agree
	// with the settle window to within a tolerance that a lost frame or two
	// cannot hide behind. 10% of this card's 29.97 fps is three frames a second;
	// the arming probe was measured here at 170-240 microseconds for both
	// branches, which is a seventh of one frame period.
	//
	// The tolerance is asymmetric in practice and it is worth knowing why before
	// somebody tightens it: the SETTLE window consistently reads about 1 fps low
	// (28.99 against the cycles' 29.97) because it contains the card's first
	// frames and the one renegotiation it does as it gives up on finding a
	// signal. So every cycle reads about +3.4% against the reference and none of
	// that is the arming. A cycle reading BELOW the settle window is the
	// direction that would mean something.
	const rateTolerance = 0.10
	for i, r := range results {
		if settleFPS <= 0 {
			break
		}
		drift := (r.fps - settleFPS) / settleFPS
		if drift < -rateTolerance || drift > rateTolerance {
			t.Errorf("SHIP-BLOCKER: %s ran at %.2f fps during cycle %d against %.2f fps while "+
				"settling (%+.1f%%). The arming probe blocks the tee's src pad, and a rate that "+
				"moves when it fires is the card backing up behind it",
				nameVideoCaptureSrc, r.fps, i+1, settleFPS, drift*100)
		}
	}
	assertNoDroppedFrames(t, debugLog)

	// The capture bus. `Signal lost` is EXPECTED on this rig — there is no video
	// signal on the card's video input, which is the documented state of the
	// machine — so warnings are reported rather than failed on, and errors are
	// not tolerated at all.
	busErrs, busWarns := levels.bus()
	if len(busErrs) > 0 {
		t.Errorf("the CAPTURE bus recorded %d error(s): %v", len(busErrs), describeBus(busErrs, captureUp))
	}
	for _, w := range describeBus(busWarns, captureUp) {
		t.Logf("capture bus warning: %s", w)
	}

	// ------------------------------------------------------- the control cycle
	//
	// This asserts the HAZARD, not the fix. If GStreamer ever changes so that a
	// proxysink re-sends its sticky events to a later consumer without help, this
	// fails and someone re-reads the decision to arm at all.
	control := results[3].cycle
	if control.fileBytes == 0 && control.muxBuffers == 0 {
		t.Logf("CONTROL CONFIRMED on the real card: cycle 4 was not armed and produced exactly "+
			"0 bytes and 0 mux buffers, with %d buffer(s) at %s:src and %d at %s:src. The hazard "+
			"is the card's too, and the three armed cycles above are real results",
			control.videoSrcBuffers, probeVideoProxySrc, control.audioSrcBuffers, probeAudioProxySrc)
	} else {
		t.Errorf("READ THIS BEFORE ANYTHING ELSE — THE HAZARD DID NOT REPRODUCE ON THE CARD.\n"+
			"Cycle 4 attached a fresh proxysrc to a proxysink that had already served three "+
			"consumers, WITHOUT the READY cycle, and it still produced %d bytes on disk and %d "+
			"mux buffer(s) (%s:src %d buffers, events %s).\n"+
			"That contradicts step 0a on this same machine. It does NOT mean the arming should "+
			"be deleted on the strength of one run — it means something differs between the two "+
			"rigs and section 3.1 has to be re-argued from a fresh measurement. Tell the operator.",
			control.fileBytes, control.muxBuffers, probeVideoProxySrc,
			control.videoSrcBuffers, control.videoSrcEvents)
	}

	// PLAN.md section 3.4's two watchdog pads, on the card, over a feed that is
	// completely dead. This is printed rather than asserted because step 0a
	// already asserts the shape of it against test sources; what this run adds is
	// that THE CARD DOES NOT CHANGE IT. aq:src reads a healthy 187 buffers over
	// zero bytes of transport stream, so the watchdog's audio half is not
	// evidence of anything and the pad that read zero in every failing case
	// measured on either rig is mux:src.
	healthy := results[1].cycle
	t.Logf("PLAN.md section 3.4's watchdog pads on the dead feed, from the card: vq:src %d "+
		"buffers (healthy %d), aq:src %d (healthy %d), %s:src %d (healthy %d). Only vq:src and "+
		"%s:src went quiet; aq:src ran at full rate over nothing.",
		control.videoMuxInBuffers, healthy.videoMuxInBuffers,
		control.audioMuxInBuffers, healthy.audioMuxInBuffers,
		nameMux, control.muxBuffers, healthy.muxBuffers, nameMux)

	cardTotal, _ := cardFrames.read()
	audioTotal, _ := cardAudio.read()
	t.Log("summary:")
	for _, r := range results {
		t.Logf("  %s | %s:src %.2f fps", r.cycle, nameVideoCaptureSrc, r.fps)
	}
	t.Logf("  card across the whole run: %s:src %d frames, %s:src %d buffers, %s:sink %d, %s:sink %d",
		nameVideoCaptureSrc, cardTotal, nameAudioSrc, audioTotal,
		probeVideoProxySink, prevVid, probeAudioProxySink, prevAud)
}

// cardDebugLog is GStreamer's own debug log, opened at a byte offset so that
// only what the card said DURING THE MEASURED WINDOW is read back.
//
// The offset is the whole design. decklinkaudiosrc reports dropping its first
// packets on more or less every start — an ordinary consequence of the card
// having been running before the pipeline attached to it — so a check that read
// the whole file would either fail on a healthy rig or have to be weakened to
// ignore the very message it exists to catch. The log is append-only, so
// remembering its length at the instant the settle ends is exact and needs no
// timestamp arithmetic against a gst_init the test cannot observe.
type cardDebugLog struct {
	path   string
	offset int64
}

// openCardDebugLog marks the current end of the GStreamer debug log.
//
// WSLCOMMS_LIVE_GST_LOG names the same file GST_DEBUG_FILE does. Both are
// needed: GStreamer only writes the file when GST_DEBUG_FILE is set before
// gst_init, and this test only knows where to read it from the second.
func openCardDebugLog(t *testing.T) *cardDebugLog {
	t.Helper()
	path := os.Getenv("WSLCOMMS_LIVE_GST_LOG")
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Logf("WSLCOMMS_LIVE_GST_LOG=%s cannot be read (%v); proof 3's dropped-frame half will "+
			"be skipped", path, err)
		return nil
	}
	return &cardDebugLog{path: path, offset: info.Size()}
}

// assertNoDroppedFrames reads what the card logged after the settle and fails on
// either of the two messages that mean the driver's own queue backed up.
//
// Neither is a bus message and neither can be: both are GST_WARNING_OBJECT from
// the driver's callback thread inside libgstdecklink, so the only way to see
// them from Go is to have GStreamer write its debug log to a file — GST_DEBUG=2
// plus GST_DEBUG_FILE — and read it. Confirmed present in the shipped plugin
// binary rather than remembered:
//
//	strings /opt/homebrew/lib/gstreamer-1.0/libgstdecklink.dylib | grep -i dropped
//	  Dropped %u old frames from %u:%02u:%02u.%09u to %u:%02u:%02u.%09u
//	  Dropped %u old packets from %u:%02u:%02u.%09u to %u:%02u:%02u.%09u
//
// The frames one is the video source's and is the reading PLAN.md's step 0b
// names. The PACKETS one is decklinkaudiosrc's and is checked with it, because
// the commentary is the payload: a seam that cost the picture nothing and the
// audio a packet per START would pass the stated proof and still be wrong.
//
// PLAN.md section 3.3 measured the frames message directly: with the
// capture-side queue at leaky=no and a wedged send pipeline the card reported
// `Dropped 271 old frames` and fell from 50.1 to 11.6 fps. Its ABSENCE here is
// what says the arming probe's block on the tee's src pad is too short to back
// the driver up.
func assertNoDroppedFrames(t *testing.T, log *cardDebugLog) {
	t.Helper()
	if log == nil {
		t.Logf("WSLCOMMS_LIVE_GST_LOG is unset, so `Dropped N old frames` cannot be checked from " +
			"inside the test. Re-run with GST_DEBUG=2 GST_DEBUG_FILE=<path> " +
			"WSLCOMMS_LIVE_GST_LOG=<path> to close proof 3.")
		return
	}
	data, err := os.ReadFile(log.path)
	if err != nil {
		t.Errorf("reading the GStreamer debug log at %s: %v", log.path, err)
		return
	}
	if int64(len(data)) < log.offset {
		t.Errorf("the GStreamer debug log at %s shrank from %d bytes to %d during the run; "+
			"something else is writing it and proof 3 cannot be read from it",
			log.path, log.offset, len(data))
		return
	}
	window := string(data[log.offset:])
	var dropped []string
	for _, line := range strings.Split(window, "\n") {
		if strings.Contains(line, "Dropped") &&
			(strings.Contains(line, "old frames") || strings.Contains(line, "old packets")) {
			dropped = append(dropped, line)
		}
	}
	if len(dropped) > 0 {
		t.Errorf("SHIP-BLOCKER: the card reported dropping its own queued media %d time(s) after "+
			"the settle. That is the driver backing up behind this pipeline, which is what the "+
			"arming probe must be too short to cause:\n  %s",
			len(dropped), strings.Join(dropped, "\n  "))
		return
	}
	t.Logf("no `Dropped N old frames` and no `Dropped N old packets` in the %d bytes %s wrote "+
		"after the settle (%d bytes of prologue ignored, which is where the card's ordinary "+
		"start-up packet drops would be)",
		len(window), log.path, log.offset)
}

// waitUntil sleeps to an instant. It exists so the settle above reads as one
// statement rather than as arithmetic on time.Sleep.
func waitUntil(t time.Time) {
	if d := time.Until(t); d > 0 {
		time.Sleep(d)
	}
}
