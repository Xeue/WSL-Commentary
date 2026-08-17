//go:build live && cgo && !gststub

// proxyseam_live_test.go is step 0a of the always-live capture plan: the
// go-gst proof of the proxysink/proxysrc seam, with NO CARD ANYWHERE IN IT.
//
// # The failure this exists to catch
//
// gstproxysink.c resets sent_stream_start and sent_caps only on READY->PAUSED.
// A proxysink that lives in an always-live CAPTURE pipeline never makes that
// transition again after launch, so every proxysrc after the FIRST receives no
// STREAM_START, no CAPS and no SEGMENT. Measured twice before this file existed,
// once in PyGObject and once in C: 1 buffer at proxysrc:src, 0 encoded, ZERO
// bytes of transport stream, and nothing on either bus. SRT still connects and
// the SENDING lamp still goes green. That is a silent dead feed on the second
// START of the day, which is the ordinary operator action.
//
// Those two measurements were not in this language. What was unverified, and
// what this file settles, is the go-gst call sequence:
//
//   - the IDLE probe's lifetime — whether gst_pad_add_probe's Go closure runs
//     synchronously on the caller's thread when the pad is idle, and whether
//     returning PadProbeRemove releases it cleanly under -race;
//   - element ref handling across a READY cycle driven from Go rather than from
//     C, on elements that go-gst hands back as interface values;
//   - the OBJECT-VALUED property write. proxysrc's "proxysink" property is
//     `Object of type "GstProxySink"` (gst-inspect-1.0 proxysrc, GStreamer
//     1.26.10), NOT a string, so `proxysrc proxysink=ps` in a parse string is
//     refused outright and go-gst has no typed binding for it. The write has to
//     go through SetObjectProperty, whose GValue is initialised from the
//     object's own instance type.
//
// # Why the assertion is on ENCODED BYTES and never on a fakesink
//
// A fakesink accepts buffers with no caps. In the C rig, 234 of them "flowed"
// through a probe while the real encoder chain produced zero bytes of transport
// stream. A fakesink assertion is how this hazard hides, so every cycle here
// ends with a stat() on a file written by a real filesink at the end of a real
// vtenc_h264 + atenc + mpegtsmux alignment=7 chain, cross-checked against a
// BUFFER|BUFFER_LIST probe on mux:src.
//
// BUFFER_LIST in that mask is mandatory, not defensive: mpegtsmux at
// alignment=7 pushes buffer lists, and a BUFFER-only probe reads zero while
// megabytes flow. That cost two independent probes a run each.
//
// # The shape of the run
//
// Three attach/detach cycles WITH arming, then one control cycle WITHOUT. The
// control is last on purpose — the first proxysrc of a process always works, so
// a control run first would prove nothing. By cycle 4 the proxysink has served
// three consumers, which is precisely the state the operator reaches by
// pressing START for the second time.
//
// Cycle 2 is therefore the positive result (arming repairs a proxysink that has
// already been consumed) and cycle 4 is the negative control (the same
// situation without arming). Everything else is corroboration.
//
// # What it measured, on this machine, GStreamer 1.26.10 arm64, under -race
//
//	cycle 1 ARMED    1,133,076 bytes  mux:src 861 buf  first +69 ms  vq:src 200  aq:src 188
//	cycle 2 ARMED    1,125,180 bytes  mux:src 855 buf  first +45 ms  vq:src 199  aq:src 187
//	cycle 3 ARMED    1,129,128 bytes  mux:src 858 buf  first +56 ms  vq:src 199  aq:src 188
//	cycle 4 CONTROL          0 bytes  mux:src   0 buf  first never   vq:src   0  aq:src 187
//
// The hazard reproduces in go-gst exactly as it did in PyGObject and in C, and
// arming clears it three times running. Three further things came out of it that
// the plan did not have:
//
//  1. gst_pad_add_probe RETURNS 0 for this IDLE probe, every time, AFTER the
//     arming has already happened. See armProxySinkLive.
//  2. The two legs do not fail alike. Video stops at one buffer; AUDIO GOES ON
//     PUSHING AT FULL RATE into a muxer that emits nothing — 187 buffers at
//     aq:src against 187 healthy. aq:src is one of PLAN.md section 3.4's two
//     watchdog pads, so half of that watchdog reads green over a dead feed.
//     mux:src is the pad that read zero in every failing case.
//  3. The control is NOT silent on the send bus with the real encoder chain in
//     it: vtenc_h264 posts "negotiation problem" naming venc, 16 ms after
//     PLAYING. The earlier rigs recorded nothing on either bus.
//
// Arming both branches costs 176-511 microseconds, not the ~1 ms per branch the
// plan estimated, and every arming ran synchronously on the calling goroutine.
//
// The falsification run (WSLCOMMS_LIVE_SKIP_PROXY_ARMING=1) is the cleanest
// statement of the mechanism the whole plan rests on — cycle 1 works and every
// cycle after it is dead:
//
//	cycle 1 unarmed  1,133,076 bytes   (the FIRST consumer of the process)
//	cycle 2 unarmed          0 bytes
//	cycle 3 unarmed          0 bytes
//	cycle 4 unarmed          0 bytes
//
// # What this file now measures, and it is no longer its own copy of it
//
// At step 0a armProxySinkLive and bindProxySrcLive WERE the measured sequence,
// living here because there was no production code to point at. Step 5 lifted
// them into internal/gst/proxy_cgo.go, and this file has now been re-pointed as
// that step required: the two helpers are three-line wrappers over armProxySink
// and bindProxySrc, and sendProbeDescription is the shipped sendDescription with
// a filesink spliced onto srtq. The names of the six seam elements are aliases of
// the shipped constants.
//
// That matters because of what the alternative was. Two identical copies of a
// mechanism do not drift on the day they are written; they drift on the day
// somebody improves one of them — and a regression test named
// TestLiveProxySeamRearmsEverySendSession that exercised its own private copy
// would have gone on passing through exactly that. The call ORDER inside the
// probe is the measurement, and it is now measured where it ships.
//
// The two CAPTURE descriptions are still local, and deliberately: this one
// replaces the device sources with videotestsrc/audiotestsrc, which is what makes
// it runnable with no card in the machine, and captureDescription cannot render
// that. Everything downstream of the source in it is character for character what
// captureDescription renders.

package gst

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// Element names inside the descriptions below. They were spelled out as literals
// when this file was written, because internal/gst/seam.go did not exist until
// step 2; it does now, so they are ALIASES OF THE SHIPPED CONSTANTS. A rename
// that split the test's capture legs from the shipped send string would then be a
// compile error rather than a run in which the two proxysrcs find nothing.
const (
	probeVideoProxyQueue = nameVideoProxyQueue
	probeAudioProxyQueue = nameAudioProxyQueue
	probeVideoProxySink  = nameVideoProxySink
	probeAudioProxySink  = nameAudioProxySink
	probeVideoProxySrc   = nameVideoProxySrc
	probeAudioProxySrc   = nameAudioProxySrc
	probeFileSink        = "out"
)

// seamCycleRunTime is how long each send pipeline is left running.
//
// Four seconds is chosen against the ONE number that has to be unambiguous: an
// armed cycle must produce enough transport stream that "non-zero" cannot be
// confused with a flush artefact. At the shipping 2000 kbit/s video target that
// is on the order of a megabyte, against a control that must produce exactly 0.
const seamCycleRunTime = 4 * time.Second

// seamSettleTime is the pause between taking one send pipeline to NULL and
// building the next. It is not a fudge: it is the operator's STOP-then-START,
// and running the cycles back to back would test a case that cannot happen.
const seamSettleTime = 500 * time.Millisecond

// captureProbeDescription is the two always-live capture legs, shaped exactly
// like PLAN.md section 2.2 (P-CARD) and 2.4 (C-NATIVE) with the two device
// sources replaced by test sources. Everything downstream of the source —
// the conform chain, the tee, both level elements, the cough mute, the seam caps
// and both leaky queues — is character for character what the card seat runs,
// because the thing under test is the tail of these legs.
//
// videotestsrc/audiotestsrc carry is-live=true. Without it they are not live
// sources, the capture pipeline prerolls and the whole timing model under test
// is the wrong one.
const captureProbeDescription = "videotestsrc name=vcapsrc is-live=true" +
	" ! videoconvert name=vcapconv" +
	" ! deinterlace name=vcapdeint" +
	" ! videoscale name=vcapscale" +
	" ! videorate name=vcaprate" +
	" ! video/x-raw,format=NV12,width=1920,height=1080,pixel-aspect-ratio=1/1," +
	"colorimetry=bt709,interlace-mode=progressive,framerate=50/1" +
	" ! tee name=vcaptee allow-not-linked=true\n" +

	"vcaptee. ! queue name=" + probeVideoProxyQueue +
	" leaky=downstream max-size-buffers=8 max-size-bytes=0 max-size-time=0" +
	" ! proxysink name=" + probeVideoProxySink + "\n" +

	"audiotestsrc name=" + nameAudioSrc + " is-live=true" +
	" ! audioconvert" +
	" ! level name=chlevel interval=100000000 post-messages=false" +
	" ! audioconvert name=" + nameAudioConv +
	" ! audioresample" +
	" ! audio/x-raw,format=S16LE,rate=48000,channels=2,layout=interleaved" +
	" ! volume name=" + nameCoughMute + " mute=false" +
	" ! level name=alevel interval=50000000" +
	" ! queue name=" + probeAudioProxyQueue +
	" leaky=downstream max-size-time=1000000000 max-size-bytes=0 max-size-buffers=0" +
	" ! proxysink name=" + probeAudioProxySink

// sendProbeDescription is PLAN.md section 2.7's send string with srtsink
// replaced by a filesink. The substitution is the point of the test rather than
// a convenience: a file has a byte count that survives the process, and the
// hazard's whole signature is that SRT connects happily over zero of them.
//
// location is NOT in the string; it is written with g_object_set after parse,
// for the same reason the slate path is — gst_parse_launch's quoting rules are
// not somewhere to send a filesystem path.
//
// sync=false async=false on the filesink is load-bearing and is the same
// requirement srtsink carries in production for a second, independent reason
// (PLAN.md section 3.5): proxysrc emits the PRODUCER's running time, not a
// rebased zero, so a sync=true sink stalls after exactly one buffer waiting for
// a timestamp that is however long the capture pipeline has been up in the
// future. That failure looks identical to the hazard under test, which is why
// it is nailed down here instead of left to a default.
// IT IS THE SHIPPED STRING, SPLICED, NOT A COPY OF IT. sendDescription's first
// chain ends at srtq's src pad with no peer — that is what "START installs no
// sink and closes the gate first" means — so the ONLY edit is a filesink hung on
// the end of that one chain. Everything measured below is therefore measured
// through the string the application actually parses, and a change to it that
// broke the seam would break this test rather than passing beside it.
func sendProbeDescription(encoderName string, audioBitrateBps int) string {
	desc := sendDescription(encoderName, audioBitrateBps)
	head, rest, ok := strings.Cut(desc, "\n")
	if !ok {
		panic("gst: sendDescription no longer has the muxer chain first; the filesink cannot be " +
			"spliced onto srtq and this test would be measuring a different graph")
	}
	return head + " ! filesink name=" + probeFileSink + " sync=false async=false\n" + rest
}

// ---------------------------------------------------------------------------
// Counters. Every one of these is written from a streaming thread and read from
// the test goroutine, so every one is mutex-guarded — the run is under -race.
// ---------------------------------------------------------------------------

// flowCounter counts buffers and bytes across a BUFFER|BUFFER_LIST probe, and
// records when the first one arrived.
//
// The arrival time is not decoration: PLAN.md section 3.4 proposes refusing a
// Start at which nothing has reached the muxer within 2 s of PLAYING, and that
// budget is only affordable if a healthy seam beats it comfortably.
type flowCounter struct {
	mu      sync.Mutex
	buffers int64
	bytes   int64
	first   time.Time
}

func (c *flowCounter) probe(_ gogst.Pad, info *gogst.PadProbeInfo) gogst.PadProbeReturn {
	var n, b int64
	if buf := info.GetBuffer(); buf != nil {
		n, b = 1, int64(buf.GetSize())
	} else if list := info.GetBufferList(); list != nil {
		// The BUFFER_LIST half. mpegtsmux at alignment=7 arrives here and
		// nowhere else; counting the list as one buffer would understate the
		// count by a factor of seven and reading no bytes at all is the measured
		// trap this mask exists to avoid.
		n, b = int64(list.Length()), int64(list.CalculateSize())
	}
	c.mu.Lock()
	if c.buffers == 0 && n > 0 {
		c.first = time.Now()
	}
	c.buffers += n
	c.bytes += b
	c.mu.Unlock()
	return gogst.PadProbeOK
}

func (c *flowCounter) read() (buffers, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buffers, c.bytes
}

// firstAfter is how long after t the first buffer arrived, or -1 if none did.
func (c *flowCounter) firstAfter(t time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.buffers == 0 {
		return -1
	}
	return c.first.Sub(t)
}

// eventRecorder records the downstream events that reach a pad, in order.
//
// This is the hazard's MECHANISM rather than its symptom: the zero byte count
// is what the operator suffers, and "no STREAM_START, no CAPS, no SEGMENT" is
// why. Recording both means a future failure can be told apart from a merely
// similar one.
type eventRecorder struct {
	mu    sync.Mutex
	types []gogst.EventType
}

func (r *eventRecorder) probe(_ gogst.Pad, info *gogst.PadProbeInfo) gogst.PadProbeReturn {
	if ev := info.GetEvent(); ev != nil {
		r.mu.Lock()
		r.types = append(r.types, ev.GetType())
		r.mu.Unlock()
	}
	return gogst.PadProbeOK
}

func (r *eventRecorder) has(typ gogst.EventType) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.types {
		if t == typ {
			return true
		}
	}
	return false
}

func (r *eventRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.types) == 0 {
		return "(NONE)"
	}
	parts := make([]string, 0, len(r.types))
	for _, t := range r.types {
		parts = append(parts, t.String())
	}
	return strings.Join(parts, ", ")
}

// busRecorder collects ERROR and WARNING off a bus.
//
// It returns BusDrop for every message, matching production: nothing else in
// this process reads either bus, and a sync handler that passes messages on to a
// bus with no watch is a slow leak — measured at 7,168 queued messages in the
// capture probe that let one through.
//
// It never touches *testing.T. A bus handler can fire after the test function
// has returned, and a t.Logf from there panics the whole binary.
// Each entry is stamped with the wall clock so the caller can report HOW LONG
// AFTER PLAYING it arrived. That number decides whether a bus error is a
// detector Start can wait for or one that turns up after Start has already
// reported success.
type busRecorder struct {
	mu       sync.Mutex
	errors   []busEntry
	warnings []busEntry
}

type busEntry struct {
	at   time.Time
	text string
}

func (r *busRecorder) handler(_ gogst.Bus, msg *gogst.Message) gogst.BusSyncReply {
	if msg == nil {
		return gogst.BusDrop
	}
	source := "pipeline"
	if src := msg.Source(); src != nil {
		source = src.GetName()
	}
	switch msg.Type() {
	case gogst.MessageError:
		_, gerr := msg.ParseError()
		r.mu.Lock()
		r.errors = append(r.errors, busEntry{time.Now(), fmt.Sprintf("%s: %v", source, gerr)})
		r.mu.Unlock()
	case gogst.MessageWarning:
		_, gerr := msg.ParseWarning()
		r.mu.Lock()
		r.warnings = append(r.warnings, busEntry{time.Now(), fmt.Sprintf("%s: %v", source, gerr)})
		r.mu.Unlock()
	}
	return gogst.BusDrop
}

func (r *busRecorder) snapshot() (errors, warnings []busEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]busEntry(nil), r.errors...), append([]busEntry(nil), r.warnings...)
}

// describeBus renders entries relative to the instant the pipeline reached
// PLAYING.
func describeBus(entries []busEntry, since time.Time) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, fmt.Sprintf("[+%s] %s", e.at.Sub(since).Round(time.Millisecond), e.text))
	}
	return out
}

// ---------------------------------------------------------------------------
// The two operations this step exists to verify
// ---------------------------------------------------------------------------

// bindProxySrcLive and armProxySinkLive are now THIN WRAPPERS OVER THE SHIPPED
// CODE, and that is the point of them still existing.
//
// Step 0a wrote the measured sequence here because there was no production code
// to point at. Step 5 lifted it verbatim into internal/gst/proxy_cgo.go, and the
// note at the head of this file says what has to happen next: "this test is then
// re-pointed at those. Do not let the two drift: the call ORDER inside the probe
// is the measurement." Two identical copies do not drift on the day they are
// written; they drift on the day somebody improves one of them, and a regression
// test that exercises its own private copy of the mechanism would stay green
// through exactly that.
//
// So everything the two functions below documented now lives on armProxySink and
// bindProxySrc — the UPSTREAM src pad, READY and never NULL, the order inside the
// callback, the zero-id trap and the object-valued property write — and what is
// left here is the t.Fatal that a test needs and a library cannot have.

// bindProxySrcLive gives a proxysrc its proxysink through the shipped
// bindProxySrc, whose read-back is what says the write landed: a proxysrc
// attached to nothing is indistinguishable at the pipeline level from the hazard
// this whole file is about.
func bindProxySrcLive(t *testing.T, src, sink gogst.Element) {
	t.Helper()
	if err := bindProxySrc(src, sink); err != nil {
		t.Fatalf("binding %s to %s: %v", src.GetName(), sink.GetName(), err)
	}
}

// armProxySinkLive runs PLAN.md section 3.1's READY cycle through the shipped
// armProxySink and returns what it cost.
//
// The measured cost on this machine is 176-511 microseconds for BOTH branches
// together, and every arming returned a ZERO probe id after having already
// performed the cycle — which is why armProxySink's success test is a completion
// channel and not the id. See proxy_cgo.go.
func armProxySinkLive(t *testing.T, queue, sink gogst.Element) time.Duration {
	t.Helper()
	took, err := armProxySink(queue, sink)
	if err != nil {
		t.Fatalf("arming %s: %v", sink.GetName(), err)
	}
	return took
}

// ---------------------------------------------------------------------------
// One cycle
// ---------------------------------------------------------------------------

type seamCycle struct {
	n     int
	armed bool

	// Encoded output, two independent readings of the same thing.
	fileBytes  int64
	muxBuffers int64
	muxBytes   int64

	// PLAN.md section 3.4's chosen watchdog pads, measured rather than assumed.
	videoMuxInBuffers int64
	audioMuxInBuffers int64

	// The seam itself.
	videoSrcBuffers int64
	audioSrcBuffers int64
	videoSrcEvents  string
	audioSrcEvents  string
	sawVideoCaps    bool
	sawAudioCaps    bool
	sawVideoSegment bool
	sawAudioSegment bool

	// firstMux is how long after PLAYING the first transport-stream buffer left
	// mpegtsmux, or -1 if none ever did. PLAN.md section 3.4's Start-time
	// liveness gate is a 2 s budget against this number.
	firstMux time.Duration

	armDuration time.Duration
	busErrors   []string
	busWarnings []string
}

func (c seamCycle) String() string {
	first := "never"
	if c.firstMux >= 0 {
		first = "+" + c.firstMux.Round(time.Millisecond).String()
	}
	return fmt.Sprintf("cycle %d (%s): %d bytes on disk, mux:src %d buffers / %d bytes, first at %s, "+
		"%s:src %d buffers, %s:src %d buffers, vq:src %d, aq:src %d",
		c.n, map[bool]string{true: "ARMED", false: "UNARMED CONTROL"}[c.armed],
		c.fileBytes, c.muxBuffers, c.muxBytes, first,
		probeVideoProxySrc, c.videoSrcBuffers, probeAudioProxySrc, c.audioSrcBuffers,
		c.videoMuxInBuffers, c.audioMuxInBuffers)
}

// runSeamCycle builds one send pipeline, runs it, tears it down and returns
// what it measured. It never asserts: the caller compares the cycles, and a
// cycle that asserted its own success could not be the control.
func runSeamCycle(t *testing.T, capture gogst.Pipeline, dir string, n int, armed bool,
	encoderName string, clock gogst.Clock, base gogst.ClockTime) seamCycle {
	t.Helper()

	out := seamCycle{n: n, armed: armed}

	if armed {
		vq := mustGetElement(t, capture, probeVideoProxyQueue)
		vs := mustGetElement(t, capture, probeVideoProxySink)
		aq := mustGetElement(t, capture, probeAudioProxyQueue)
		as := mustGetElement(t, capture, probeAudioProxySink)
		out.armDuration = armProxySinkLive(t, vq, vs) + armProxySinkLive(t, aq, as)
		t.Logf("cycle %d: armed both proxysinks in %s", n, out.armDuration)
	} else {
		t.Logf("cycle %d: NOT armed — this is the control", n)
	}

	desc := sendProbeDescription(encoderName, DefaultAudioBitrateBps)
	element, err := gogst.ParseLaunch(desc)
	if err != nil {
		t.Fatalf("cycle %d: gst_parse_launch on the send description failed: %v\n%s", n, err, desc)
	}
	send, ok := element.(gogst.Pipeline)
	if !ok {
		t.Fatalf("cycle %d: gst_parse_launch returned a %T, not a GstPipeline", n, element)
	}

	path := filepath.Join(dir, fmt.Sprintf("cycle-%d.ts", n))
	if err := setStringProperty(mustGetElement(t, send, probeFileSink), "location", path); err != nil {
		t.Fatalf("cycle %d: %v", n, err)
	}
	applyEncoderProperties(mustGetElement(t, send, nameVideoEncod), encoderName, DefaultVideoBitrateKbps)

	// THE OBJECT-VALUED WRITE, both legs, before any state change. proxysrc
	// reads the property in its READY transition, so a bind after PLAYING is a
	// bind that never happens.
	bindProxySrcLive(t, mustGetElement(t, send, probeVideoProxySrc),
		mustGetElement(t, capture, probeVideoProxySink))
	bindProxySrcLive(t, mustGetElement(t, send, probeAudioProxySrc),
		mustGetElement(t, capture, probeAudioProxySink))

	mux := &flowCounter{}
	addFlowProbe(t, send, nameMux, "src", mux)
	vsrc, asrc := &flowCounter{}, &flowCounter{}
	addFlowProbe(t, send, probeVideoProxySrc, "src", vsrc)
	addFlowProbe(t, send, probeAudioProxySrc, "src", asrc)
	// The two pads PLAN.md section 3.4 puts the liveness watchdog on. Measuring
	// them here rather than reasoning about them is the difference between
	// knowing the watchdog catches this failure and hoping it does.
	vin, ain := &flowCounter{}, &flowCounter{}
	addFlowProbe(t, send, "vq", "src", vin)
	addFlowProbe(t, send, "aq", "src", ain)
	vevents, aevents := &eventRecorder{}, &eventRecorder{}
	addEventProbe(t, send, probeVideoProxySrc, "src", vevents)
	addEventProbe(t, send, probeAudioProxySrc, "src", aevents)

	bus := &busRecorder{}
	if b := send.GetBus(); b != nil {
		b.SetSyncHandler(bus.handler)
	} else {
		t.Fatalf("cycle %d: the send pipeline has no bus", n)
	}

	// PLAN.md section 3.5, in this order, before any state change. The start
	// time of NONE is what stops GstPipeline recomputing base time on every
	// PAUSED->PLAYING and throwing the SetBaseTime away.
	send.UseClock(clock)
	send.SetStartTime(gogst.ClockTimeNone)
	send.SetBaseTime(base)

	playingAt := time.Now()
	if ret := send.BlockSetState(gogst.StatePlaying, gogst.ClockTime(10*time.Second)); !stateChangeOK(ret) {
		errs, _ := bus.snapshot()
		t.Fatalf("cycle %d: the send pipeline would not go to PLAYING (%s); bus errors: %v",
			n, ret, describeBus(errs, playingAt))
	}

	time.Sleep(seamCycleRunTime)

	// Straight to NULL rather than an EOS handshake. filesink's stop() flushes
	// and closes, so the byte count on disk is everything that reached it; what
	// is still sitting in srtq at this instant is the only difference between
	// this and the mux:src probe, and bounding that difference is one of the
	// assertions rather than something to engineer away.
	if ret := send.BlockSetState(gogst.StateNull, gogst.ClockTime(10*time.Second)); !stateChangeOK(ret) {
		t.Errorf("cycle %d: the send pipeline would not go to NULL (%s)", n, ret)
	}

	out.muxBuffers, out.muxBytes = mux.read()
	out.firstMux = mux.firstAfter(playingAt)
	out.videoSrcBuffers, _ = vsrc.read()
	out.audioSrcBuffers, _ = asrc.read()
	out.videoMuxInBuffers, _ = vin.read()
	out.audioMuxInBuffers, _ = ain.read()
	out.videoSrcEvents, out.audioSrcEvents = vevents.String(), aevents.String()
	out.sawVideoCaps, out.sawAudioCaps = vevents.has(gogst.EventCaps), aevents.has(gogst.EventCaps)
	out.sawVideoSegment = vevents.has(gogst.EventSegment)
	out.sawAudioSegment = aevents.has(gogst.EventSegment)
	busErrs, busWarns := bus.snapshot()
	out.busErrors = describeBus(busErrs, playingAt)
	out.busWarnings = describeBus(busWarns, playingAt)

	if info, err := os.Stat(path); err != nil {
		t.Errorf("cycle %d: stat %s: %v", n, path, err)
	} else {
		out.fileBytes = info.Size()
	}

	t.Logf("%s", out)
	t.Logf("cycle %d: %s:src events: %s", n, probeVideoProxySrc, out.videoSrcEvents)
	t.Logf("cycle %d: %s:src events: %s", n, probeAudioProxySrc, out.audioSrcEvents)
	for _, e := range out.busErrors {
		t.Logf("cycle %d: send bus ERROR: %s", n, e)
	}
	for _, w := range out.busWarnings {
		t.Logf("cycle %d: send bus warning: %s", n, w)
	}

	time.Sleep(seamSettleTime)
	return out
}

func mustGetElement(t *testing.T, pipeline gogst.Pipeline, name string) gogst.Element {
	t.Helper()
	el := pipeline.GetByName(name)
	if el == nil {
		t.Fatalf("the parsed pipeline has no element named %s", name)
	}
	return el
}

func addFlowProbe(t *testing.T, pipeline gogst.Pipeline, element, padName string, c *flowCounter) {
	t.Helper()
	pad := mustGetElement(t, pipeline, element).GetStaticPad(padName)
	if pad == nil {
		t.Fatalf("%s has no %s pad", element, padName)
	}
	if pad.AddProbe(gogst.PadProbeTypeBuffer|gogst.PadProbeTypeBufferList, c.probe) == 0 {
		t.Fatalf("gst_pad_add_probe failed on %s:%s", element, padName)
	}
}

func addEventProbe(t *testing.T, pipeline gogst.Pipeline, element, padName string, r *eventRecorder) {
	t.Helper()
	pad := mustGetElement(t, pipeline, element).GetStaticPad(padName)
	if pad == nil {
		t.Fatalf("%s has no %s pad", element, padName)
	}
	if pad.AddProbe(gogst.PadProbeTypeEventDownstream, r.probe) == 0 {
		t.Fatalf("gst_pad_add_probe failed for downstream events on %s:%s", element, padName)
	}
}

// ---------------------------------------------------------------------------
// The test
// ---------------------------------------------------------------------------

// TestLiveProxySeamRearmsEverySendSession is step 0a's proof and, from step 5
// onwards, the regression test for a silent dead feed.
//
// It needs no card, no network and no M2L-X: videotestsrc, audiotestsrc,
// vtenc_h264, atenc, mpegtsmux and filesink are all in the bundle.
func TestLiveProxySeamRearmsEverySendSession(t *testing.T) {
	liveInitDarwin(t)

	encoderName, err := selectH264Encoder()
	if err != nil {
		t.Fatalf("selectH264Encoder: %v", err)
	}
	t.Logf("H.264 encoder: %s; AAC encoder: %s", encoderName, aacEncoderFactory)

	// ---------------------------------------------------------------- capture
	t.Logf("capture description:\n%s", captureProbeDescription)
	element, err := gogst.ParseLaunch(captureProbeDescription)
	if err != nil {
		t.Fatalf("gst_parse_launch on the capture description failed: %v", err)
	}
	capture, ok := element.(gogst.Pipeline)
	if !ok {
		t.Fatalf("gst_parse_launch returned a %T, not a GstPipeline", element)
	}
	t.Cleanup(func() {
		capture.BlockSetState(gogst.StateNull, gogst.ClockTime(10*time.Second))
	})

	captureBus := &busRecorder{}
	if b := capture.GetBus(); b != nil {
		b.SetSyncHandler(captureBus.handler)
	} else {
		t.Fatal("the capture pipeline has no bus")
	}

	// The two legs' own liveness, sampled at the proxysinks' sink pads. This is
	// the guard against the measured catastrophe on the other side of section
	// 3.1's decision: a READY cycle that takes the capture leg down permanently
	// would still leave every send cycle looking plausible, because the send
	// pipeline reads zero either way.
	videoLeg, audioLeg := &flowCounter{}, &flowCounter{}
	addFlowProbe(t, capture, probeVideoProxySink, "sink", videoLeg)
	addFlowProbe(t, capture, probeAudioProxySink, "sink", audioLeg)

	clock := gogst.SystemClockObtain()
	if clock == nil {
		t.Fatal("gst_system_clock_obtain returned nil")
	}
	capture.UseClock(clock)
	capture.SetStartTime(gogst.ClockTimeNone)
	base := clock.GetTime()
	capture.SetBaseTime(base)

	captureUp := time.Now()
	if ret := capture.BlockSetState(gogst.StatePlaying, gogst.ClockTime(10*time.Second)); !stateChangeOK(ret) {
		errs, _ := captureBus.snapshot()
		t.Fatalf("the capture pipeline would not go to PLAYING (%s); bus errors: %v",
			ret, describeBus(errs, captureUp))
	}
	// Let the legs settle so that cycle 1 is not measuring preroll.
	time.Sleep(time.Second)

	dir := t.TempDir()

	// ------------------------------------------------------------- the cycles
	//
	// Three armed, then the control. Cycle 1's arming is a no-op — nothing has
	// consumed this proxysink yet — so cycle 2 is the real positive result and
	// cycle 4 is its control.
	//
	// PROVING THE TEST CAN FAIL. WSLCOMMS_LIVE_SKIP_PROXY_ARMING=1 makes the same
	// binary skip the arming on every cycle, which is the code as it would stand
	// if step 5 were written without section 3.1. THAT RUN MUST FAIL. A green run
	// with arming skipped means this test is not measuring what it claims to, and
	// the regression it guards would land unnoticed. Same shape as
	// WSLCOMMS_LIVE_LIFT_EVENT_GATE in live_test.go, for the same reason.
	skipArming := os.Getenv("WSLCOMMS_LIVE_SKIP_PROXY_ARMING") == "1"
	if skipArming {
		t.Log("WSLCOMMS_LIVE_SKIP_PROXY_ARMING=1: no cycle will be armed. This run MUST FAIL; " +
			"a pass means the test does not measure what it claims to.")
	}

	var cycles []seamCycle
	legBefore := make([][2]int64, 0, 4)
	for n := 1; n <= 4; n++ {
		v, _ := videoLeg.read()
		a, _ := audioLeg.read()
		legBefore = append(legBefore, [2]int64{v, a})
		cycles = append(cycles, runSeamCycle(t, capture, dir, n, n <= 3 && !skipArming,
			encoderName, clock, base))
	}

	// ------------------------------------------------- the capture must be fine
	//
	// Asserted before anything else, because if the capture pipeline died the
	// send-side numbers below mean nothing at all.
	state, pending, ret := capture.GetState(0)
	if state != gogst.StatePlaying {
		t.Errorf("SHIP-BLOCKER: after four attach/detach cycles the CAPTURE pipeline is in %s "+
			"(pending %s, %s), not PLAYING", state, pending, ret)
	}
	vTotal, _ := videoLeg.read()
	aTotal, _ := audioLeg.read()
	for i, before := range legBefore {
		if i == 0 {
			continue
		}
		if before[0] <= legBefore[i-1][0] {
			t.Errorf("SHIP-BLOCKER: the VIDEO capture leg produced nothing between cycle %d and "+
				"cycle %d (%d buffers at %s:sink both times). The READY cycle has stopped the "+
				"producer, which is the failure PLAN.md section 3.1 chose READY over NULL to avoid",
				i, i+1, before[0], probeVideoProxySink)
		}
		if before[1] <= legBefore[i-1][1] {
			t.Errorf("SHIP-BLOCKER: the AUDIO capture leg produced nothing between cycle %d and "+
				"cycle %d (%d buffers at %s:sink both times)", i, i+1, before[1], probeAudioProxySink)
		}
	}
	t.Logf("capture legs across the whole run: %s:sink %d buffers, %s:sink %d buffers",
		probeVideoProxySink, vTotal, probeAudioProxySink, aTotal)
	if errs, warns := captureBus.snapshot(); len(errs) > 0 {
		t.Errorf("the capture bus recorded %d error(s): %v", len(errs), describeBus(errs, captureUp))
	} else if len(warns) > 0 {
		t.Logf("the capture bus recorded %d warning(s): %v", len(warns), describeBus(warns, captureUp))
	}

	// ------------------------------------------------------ the armed cycles
	//
	// THE ASSERTION IS ON ENCODED BYTES ON DISK. A fakesink accepts buffers with
	// no caps — 234 of them "flowed" in the C rig while the real chain produced
	// zero — so nothing but a real encoder chain's output settles this.
	var largest int64
	for _, c := range cycles[:3] {
		if c.fileBytes > largest {
			largest = c.fileBytes
		}
	}
	for _, c := range cycles[:3] {
		// Every failure below has to say whether this cycle was actually armed,
		// because the falsification run (WSLCOMMS_LIVE_SKIP_PROXY_ARMING=1) reaches
		// all of them with armed=false — and that run is precisely the one somebody
		// reads line by line to satisfy themselves the test can fail. A message
		// that asserts "was armed" there is the test lying in the one run whose job
		// is to be believed.
		why := "was ARMED and"
		if !c.armed {
			why = "had its arming SKIPPED (WSLCOMMS_LIVE_SKIP_PROXY_ARMING=1) and"
		}
		switch {
		case c.fileBytes == 0:
			t.Errorf("SHIP-BLOCKER cycle %d %s wrote ZERO bytes of transport stream. "+
				"This is the silent dead feed: SRT would have connected and the lamp would have "+
				"gone green over nothing. %s:src saw %d buffer(s) and these events: %s",
				c.n, why, probeVideoProxySrc, c.videoSrcBuffers, c.videoSrcEvents)
		case c.fileBytes*2 < largest:
			t.Errorf("cycle %d wrote %d bytes against a best armed cycle of %d — less than half. "+
				"The cycles run for the same time through the same CBR encoder, so they are "+
				"supposed to be comparable; a cycle that carries a fraction of the media is a "+
				"partially armed seam, which reaches air as a feed that drops out",
				c.n, c.fileBytes, largest)
		}
		if c.muxBuffers == 0 {
			t.Errorf("SHIP-BLOCKER cycle %d: the BUFFER|BUFFER_LIST probe on %s:src counted "+
				"nothing while %d bytes reached the file. Either the mask is wrong or the "+
				"cross-check is not measuring the chain it names", c.n, nameMux, c.fileBytes)
		}
		if c.fileBytes > c.muxBytes {
			t.Errorf("cycle %d: the file holds %d bytes but %s:src only pushed %d; the two "+
				"readings disagree in the impossible direction", c.n, c.fileBytes, nameMux, c.muxBytes)
		}
		// srtq is leaky=downstream max-size-buffers=4000, so at most that many
		// buffers can be in flight when the pipeline goes to NULL. At the
		// measured 1316-byte transport packet that is about 5 MB; anything
		// larger than that means the file is missing media for another reason.
		if gap := c.muxBytes - c.fileBytes; gap > 6<<20 {
			t.Errorf("cycle %d: %d bytes left %s:src but only %d reached the file, a gap of %d — "+
				"far more than srtq can hold", c.n, c.muxBytes, nameMux, c.fileBytes, gap)
		}
		if len(c.busErrors) > 0 {
			t.Errorf("cycle %d: the send bus recorded %d error(s): %v", c.n, len(c.busErrors), c.busErrors)
		}
		if !c.sawVideoCaps || !c.sawAudioCaps || !c.sawVideoSegment || !c.sawAudioSegment {
			t.Errorf("cycle %d %s did not carry the sticky events across the seam: "+
				"%s:src caps=%v segment=%v, %s:src caps=%v segment=%v",
				c.n, why, probeVideoProxySrc, c.sawVideoCaps, c.sawVideoSegment,
				probeAudioProxySrc, c.sawAudioCaps, c.sawAudioSegment)
		}
		// PLAN.md section 3.4 proposes refusing a Start at which nothing has
		// reached the muxer within 2 s of PLAYING. Measured here: the first
		// transport-stream buffer leaves mpegtsmux about 130 ms after PLAYING on
		// every armed cycle, so that budget has an order of magnitude in hand.
		// If it ever stops having one, the gate starts refusing healthy Starts,
		// which is a worse failure than the one it is there to catch.
		if c.firstMux < 0 || c.firstMux > 2*time.Second {
			t.Errorf("cycle %d: the first buffer left %s:src at %s after PLAYING. PLAN.md section "+
				"3.4's Start-time liveness gate is a 2 s budget against this number and would "+
				"refuse this Start", c.n, nameMux, c.firstMux)
		}
	}

	// ------------------------------------------------------- the control cycle
	//
	// This asserts the HAZARD, not the fix, and it is written that way on
	// purpose. If GStreamer ever changes so that a proxysink re-sends its sticky
	// events to a second consumer without help, this fails — and someone re-reads
	// the decision to arm at all, which is the right outcome. A test written to
	// assert the behaviour we would prefer would instead go green on a proxysink
	// that had silently stopped needing the fix, and stay green on one that
	// silently started needing it twice.
	control := cycles[3]
	switch {
	case control.fileBytes == 0 && control.muxBuffers == 0:
		t.Logf("CONTROL CONFIRMED: cycle 4 was not armed and produced exactly 0 bytes and 0 mux "+
			"buffers, with %d buffer(s) at %s:src and %d at %s:src. The hazard reproduces in "+
			"go-gst exactly as it did in PyGObject and in C, and every armed cycle above is a "+
			"real result rather than a coincidence.",
			control.videoSrcBuffers, probeVideoProxySrc, control.audioSrcBuffers, probeAudioProxySrc)
	default:
		t.Errorf("READ THIS BEFORE ANYTHING ELSE — THE HAZARD DID NOT REPRODUCE.\n"+
			"Cycle 4 attached a fresh proxysrc to a proxysink that had already served three "+
			"consumers, WITHOUT the READY cycle, and it still produced %d bytes on disk and %d "+
			"mux buffer(s) (%s:src %d buffers, events %s).\n"+
			"That contradicts the two measurements PLAN.md section 3.1 is built on. It does NOT "+
			"mean the arming should be deleted on the strength of one run — it means this "+
			"GStreamer behaves differently from the one the plan was measured against, and step "+
			"3.1 has to be re-argued from a fresh measurement before step 5 is written. Tell the "+
			"operator.",
			control.fileBytes, control.muxBuffers, probeVideoProxySrc,
			control.videoSrcBuffers, control.videoSrcEvents)
	}
	// ------------------------------- the control's SIGNATURE, which is not what
	//                                 the earlier rigs recorded
	//
	// Two things measured here differ from the PyGObject and C rigs, and both
	// change a decision in the plan. They are asserted rather than logged so
	// that a future GStreamer which changes either of them is noticed. They are
	// only meaningful when the control did fail; the branch above has already
	// shouted if it did not.
	if control.fileBytes == 0 {
		describeControlSignature(t, control, cycles[1])
	}

	// One line per cycle at the end, so the numbers are readable without
	// reconstructing them from the interleaved log.
	t.Log("summary:")
	for _, c := range cycles {
		t.Logf("  %s", c)
	}
}

// describeControlSignature asserts HOW the unarmed cycle failed, separately from
// the fact that it failed, so that a partial change is reported as a partial
// change rather than as the hazard vanishing.
func describeControlSignature(t *testing.T, control, healthy seamCycle) {
	t.Helper()

	// 1. NO EVENT AT ALL crosses the seam — not merely no CAPS. Both proxysrc
	//    src pads recorded an EMPTY downstream-event list: no STREAM_START, no
	//    CAPS, no SEGMENT. That is the mechanism section 3.1 names, seen
	//    directly rather than inferred from the byte count.
	if control.sawVideoCaps || control.sawAudioCaps {
		t.Errorf("the unarmed control produced 0 bytes but CAPS did cross the seam "+
			"(%s:src %v, %s:src %v). The dead feed then has a different cause from the one "+
			"section 3.1 describes, and the fix may not be the right one",
			probeVideoProxySrc, control.sawVideoCaps, probeAudioProxySrc, control.sawAudioCaps)
	}

	// 2. THE TWO LEGS DO NOT FAIL THE SAME WAY, and this is the finding that
	//    changes a decision.
	//
	//    Measured: video stops at ONE buffer at vproxsrc:src — vtenc_h264
	//    refuses the uncapped buffer, posts "negotiation problem" 16 ms after
	//    PLAYING, and the proxysrc's internal queue pauses its loop. AUDIO DOES
	//    NOT STOP. It pushes 187 buffers in four seconds against 188 on a
	//    healthy armed cycle, straight through the seam capsfilter, through aq,
	//    and into mpegtsmux's request pad — GStreamer says so itself, once per
	//    buffer, on stderr:
	//
	//      gst_pad_chain_data_unchecked:<aq:sink> Got data flow before segment event
	//      gst_pad_push_data:<aq:src> Got data flow before stream-start event
	//      gst_pad_chain_data_unchecked:<mux:sink_66> Got data flow before segment event
	//      gst_segment_to_running_time: assertion 'segment->format == format' failed
	//
	//    187 of each per control cycle, and mpegtsmux emits nothing at all.
	//
	//    So on the AUDIO half, PLAN.md section 3.4's chosen watchdog pad —
	//    aq:src — reads a perfectly healthy feed over a dead one. The watchdog
	//    still catches this failure, but only through vq:src. That is worth
	//    stating in section 3.4 rather than leaving to be rediscovered: the two
	//    pads are not interchangeable evidence, and the one pad that read zero
	//    in every failing case measured here is mux:src.
	if control.videoSrcBuffers > 2 {
		t.Errorf("the unarmed control pushed %d buffers at %s:src; it was measured to stop at 1 "+
			"when %s refuses the uncapped buffer. A video leg that keeps flowing changes what a "+
			"detector on this pad would see", control.videoSrcBuffers, probeVideoProxySrc, nameVideoEncod)
	}
	if control.videoMuxInBuffers > 2 {
		t.Errorf("the unarmed control pushed %d buffers at vq:src, one of the two pads PLAN.md "+
			"section 3.4 watches. It was measured to stop at 1, and that silence is the ONLY half "+
			"of the watchdog that catches this failure", control.videoMuxInBuffers)
	}
	if control.audioSrcBuffers*2 < healthy.audioSrcBuffers {
		t.Errorf("the unarmed control pushed only %d buffers at %s:src against %d on the healthy "+
			"cycle 2. The measurement this test records is that the AUDIO leg goes on pushing at "+
			"its normal rate over a dead feed. If that is no longer true, PLAN.md section 3.4 has "+
			"a cheaper option than the watchdog and should be re-read",
			control.audioSrcBuffers, probeAudioProxySrc, healthy.audioSrcBuffers)
	}
	if control.audioMuxInBuffers*2 < healthy.audioMuxInBuffers {
		t.Errorf("the unarmed control pushed only %d buffers at aq:src against %d on the healthy "+
			"cycle 2. This test records that aq:src — one of PLAN.md section 3.4's two watchdog "+
			"pads — reads FULL RATE over a dead feed, so the watchdog's audio half is not evidence "+
			"of anything. A change here means section 3.4 can be simplified",
			control.audioMuxInBuffers, healthy.audioMuxInBuffers)
	}
	t.Logf("PLAN.md section 3.4's watchdog pads on the dead feed: vq:src %d buffers (healthy %d), "+
		"aq:src %d buffers (healthy %d), %s:src %d buffers (healthy %d). Only vq:src and %s:src "+
		"went quiet; aq:src did not.",
		control.videoMuxInBuffers, healthy.videoMuxInBuffers,
		control.audioMuxInBuffers, healthy.audioMuxInBuffers,
		nameMux, control.muxBuffers, healthy.muxBuffers, nameMux)

	// 3. THE CONTROL IS NOT SILENT ON THE SEND BUS, and the earlier rigs said it
	//    was. With the REAL encoder chain in place, vtenc_h264 posts
	//    "negotiation problem" naming venc. That is a genuinely better detector
	//    than anything in section 3.4 — but it is NOT a substitute for the
	//    watchdog, for two reasons this test can show: it comes from the video
	//    leg only, and the audio half of a half-armed seam would produce it
	//    never. It is recorded, with its latency, so the classifier work in
	//    step 6 can decide what to do with it.
	if len(control.busErrors) == 0 {
		t.Logf("NOTE: the unarmed control put NOTHING on the send bus, matching the earlier rigs. " +
			"The failure is therefore entirely silent and only the watchdog of section 3.4 can " +
			"catch it.")
	} else {
		t.Logf("NOTE: the unarmed control DID put %d error(s) on the send bus: %v. The earlier "+
			"rigs measured nothing on either bus, which is what made the failure silent. It names "+
			"%s, so classifyBusError would already treat it as pipeline-fatal — worth knowing in "+
			"step 6, but not a replacement for the watchdog, because it is the VIDEO leg's "+
			"complaint and a seam whose audio half alone was missed would never raise it.",
			len(control.busErrors), control.busErrors, nameVideoEncod)
	}
}
