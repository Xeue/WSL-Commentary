//go:build live && cgo && !gststub

// captureidle_live_test.go measures THE STATE THE APPLICATION SITS IN FOR MOST
// OF ITS LIFE: a capture pipeline holding the card, with the meters, the routing
// width and the signal lamp all live, and NO CONSUMER ANYWHERE IN THE PROCESS.
//
// The operator arrives twenty minutes before kick-off. From launch until they
// press START there is no send pipeline, no encoder, no muxer and no SRT — only
// this. Everything else that has been measured about the always-live design was
// measured with a send pipeline being built and destroyed beside it, or through a
// test rig's own gst_parse_launch. Nothing had yet held the SHIPPING TYPE open
// with nothing attached to it and read the cost.
//
// # Why it goes through CapturePipeline and not through a launch string
//
// The seam proofs (proxyseam_live_test.go, proxyseamcard_live_test.go) build
// their capture legs from local descriptions, and they say why: they replace the
// device sources so they can run with no card in the machine. The consequence is
// that everything the SHIPPING type does around the description — the after-parse
// property writes, the matrix, the CAPS probe, the bus sync handler, the signal
// watchdog, the two goroutines NewCapture starts and the teardown that has to
// join them — has never been the thing under measurement. A leak or a log storm
// in any of those is invisible to a launch-string rig and would be discovered by
// an operator ninety minutes into a match.
//
// # What it asserts, and what it merely records
//
// ASSERTED, because each one is a defect if it fails:
//
//   - the card opens and the pipeline is PLAYING;
//   - aconv:sink negotiates the card's sixteen channels with no consumer in the
//     process, and InputChannels goes on answering it for the whole hold;
//   - Health() stays nil and Faults() stays empty;
//   - the programme meter runs at alevel's 50 ms interval and the per-channel
//     picker at chlevel's 100 ms, CONTINUOUSLY — a meter that stops is R1's whole
//     promise breaking, silently, with nothing on the bus;
//   - the bus does not queue. Both handlers return BusDrop and nothing else in
//     the process reads this bus, so a sync handler that let a message through
//     would be a slow leak — measured at 7,168 queued messages in the capture
//     probe that let one through;
//   - Stop returns cleanly and releases the card.
//
// RECORDED and not asserted against a fixed number, because there is no prior
// measurement to hold them to and inventing a threshold would be inventing the
// result: CPU, RSS and its drift, and the log line rate. The numbers this run
// produced are in the test log and belong in BUILD-NOTES.md beside the rest.
//
// # Running it
//
//	CGO_LDFLAGS="-framework UniformTypeIdentifiers" \
//	WSLCOMMS_LIVE_APP_DIR=<symlink farm>/Contents/MacOS \
//	go test -tags "live cgo" -run TestLiveCaptureHoldsTheCardWithNoConsumer \
//	    -timeout 15m -v -count=1 ./internal/gst/
//
// Run it ALONE: it opens the exclusive DeckLink. WSLCOMMS_LIVE_CAPTURE_HOLD
// overrides the three-minute hold — PLAN.md step 11(e) asks for ninety minutes
// before ship, and this is the same test with a longer argument.
package gst

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// captureHoldDefault is the hold this runs unattended.
//
// Three minutes is not the operator's twenty and does not pretend to be. It is
// the shortest hold that can distinguish the three failures worth catching here —
// a meter that stops, a bus that queues and an RSS that climbs — from start-up
// transients, at 20 programme frames and 10 picker frames a second: 3,600 and
// 1,800 samples, against a settle of ten seconds.
const captureHoldDefault = 3 * time.Minute

// captureHoldSample is how often CPU, RSS and the meter rates are sampled. Every
// fifteen seconds gives twelve readings across the default hold, which is enough
// for a drift to be a trend rather than two points.
const captureHoldSample = 15 * time.Second

// captureHoldSettle is how much of the beginning is excluded from the RATE
// assertions. The card's own start-up, the first CAPS negotiation and the
// registry warm-up all land inside it, and none of them is what this measures.
const captureHoldSettle = 10 * time.Second

// ---------------------------------------------------------------------------
// Meters
// ---------------------------------------------------------------------------

// meterCounter counts level frames and remembers when each arrived, so that both
// the RATE and the largest GAP can be reported. The gap is the reading that
// matters: a meter averaging 20 frames a second with a four-second hole in the
// middle is a meter that stopped, and an average alone hides it.
type meterCounter struct {
	name string

	mu    sync.Mutex
	count int
	first time.Time
	last  time.Time
	gap   time.Duration
	gapAt time.Time
	// channels is the width of the most recent frame, so that the picker's
	// promise — sixteen bars, not two — is read off the frames themselves.
	channels int
}

func (m *meterCounter) add(l Levels) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.count++
	m.channels = len(l.RMSDB)
	if m.first.IsZero() {
		m.first = now
	} else if d := now.Sub(m.last); d > m.gap {
		m.gap, m.gapAt = d, now
	}
	m.last = now
}

func (m *meterCounter) snapshot() (count int, first, last time.Time, gap time.Duration, gapAt time.Time, channels int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count, m.first, m.last, m.gap, m.gapAt, m.channels
}

func (m *meterCounter) countOnly() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

// ---------------------------------------------------------------------------
// Process cost
// ---------------------------------------------------------------------------

// processRSS is the test process's resident set in bytes, read from ps.
//
// ps and not runtime.MemStats, and the difference is the whole point: almost
// every byte a capture pipeline costs is allocated by GStreamer, CoreVideo and
// the DeckLink driver, and the Go heap knows nothing about any of it. MemStats
// would report a flat few megabytes over a pipeline that had eaten a gigabyte.
//
// ru_maxrss is not used either: on darwin it is the process's PEAK and never
// comes back down, so it cannot show a drift returning.
func processRSS() (int64, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0, err
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing ps rss output %q: %w", out, err)
	}
	return kb * 1024, nil
}

// processCPU is cumulative user+system CPU time for this process.
func processCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	tv := func(t syscall.Timeval) time.Duration {
		return time.Duration(t.Sec)*time.Second + time.Duration(t.Usec)*time.Microsecond
	}
	return tv(ru.Utime) + tv(ru.Stime)
}

// ---------------------------------------------------------------------------
// Log volume
// ---------------------------------------------------------------------------

// logLineCounter counts the lines this package writes through the standard
// logger while it is installed, WITHOUT swallowing them: it is half of an
// io.MultiWriter and the other half is wherever the log was already going.
//
// The reading it produces is the one an operator cares about. A capture pipeline
// that logs one line a second fills a field log with 5,400 lines a match and
// buries the one line that matters — and nothing else in this suite would ever
// notice, because a log line is not a failure.
type logLineCounter struct {
	mu    sync.Mutex
	lines int
}

func (c *logLineCounter) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.lines += strings.Count(string(p), "\n")
	c.mu.Unlock()
	return len(p), nil
}

func (c *logLineCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lines
}

// ---------------------------------------------------------------------------
// The test
// ---------------------------------------------------------------------------

func TestLiveCaptureHoldsTheCardWithNoConsumer(t *testing.T) {
	liveInitDarwin(t)

	card := env("WSLCOMMS_LIVE_CARD", defaultLiveCard)
	hold := captureHoldDefault
	if v := os.Getenv("WSLCOMMS_LIVE_CAPTURE_HOLD"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("WSLCOMMS_LIVE_CAPTURE_HOLD=%q is not a duration: %v", v, err)
		}
		hold = d
	}

	programme := &meterCounter{name: levelElementName}
	picker := &meterCounter{name: channelLevelElementName}

	// The three callbacks the application subscribes to, all three live from
	// launch. They are counted rather than rendered, but they are the SAME
	// callbacks — a width published on the wrong goroutine or a level frame
	// delivered from a streaming thread would show up here under -race.
	var widthMu sync.Mutex
	type widthArrival struct {
		key   string
		width int
		at    time.Duration
	}
	var widths []widthArrival
	var signalMu sync.Mutex
	var signalReports []SignalReport

	begin := time.Now()
	capture, err := NewCapture(CaptureOpts{
		Legs: CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard},
		// FUSED, and no preview. The confidence monitor needs a window handle
		// and this process has no window: CONTRACT.md rule 4 forbids launching
		// the GUI, and PreviewOpts documents a zero handle as "do not build the
		// branch" rather than as an error. What is measured here is therefore the
		// floor — a seat with the monitor on pays glimagesink on top of it.
		VideoCaptureID: card,
		AudioCaptureID: card,
		ConformTo:      FallbackConformTarget(),
		ChannelMap:     DefaultChannelMap(deckLinkAudioChannels),
		OnLevels:       programme.add,
		OnChannelLevels: func(l Levels) {
			picker.add(l)
		},
		OnSignal: func(r SignalReport) {
			signalMu.Lock()
			signalReports = append(signalReports, r)
			signalMu.Unlock()
		},
		OnInputChannels: func(key string, width int) {
			widthMu.Lock()
			widths = append(widths, widthArrival{key, width, time.Since(begin)})
			widthMu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("NewCapture: %v", err)
	}
	// REGISTERED BEFORE Start, and before any t.Fatalf below can be reached. The
	// card is exclusive; an orphan holds it from the operator until the process
	// is killed. NewCapture has also already started two goroutines on this
	// object and Stop is the only thing that ends them.
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			if err := capture.Stop(); err != nil {
				t.Errorf("Stop in cleanup: %v", err)
			}
		}
	})

	// The two channels the application drains. They must be drained here for the
	// same reason: a full fault channel drops, and a dropped fault is the one
	// this test would otherwise miss.
	var faultMu sync.Mutex
	var faults []string
	var warnings []string
	var drainWG sync.WaitGroup
	drainWG.Add(2)
	go func() {
		defer drainWG.Done()
		for err := range capture.Faults() {
			faultMu.Lock()
			faults = append(faults, fmt.Sprintf("[+%s] %v", time.Since(begin).Round(time.Millisecond), err))
			faultMu.Unlock()
		}
	}()
	go func() {
		defer drainWG.Done()
		for line := range capture.Warnings() {
			faultMu.Lock()
			warnings = append(warnings, fmt.Sprintf("[+%s] %s", time.Since(begin).Round(time.Millisecond), line))
			faultMu.Unlock()
		}
	}()

	// ------------------------------------------------------------------ start
	startAt := time.Now()
	if err := capture.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	startTook := time.Since(startAt)
	t.Logf("capture %s PLAYING in %s, holding %s with NO consumer in this process",
		capture.Legs(), startTook.Round(time.Millisecond), hold)

	cgo, ok := capture.(*cgoCapture)
	if !ok {
		t.Fatalf("NewCapture returned a %T, not a *cgoCapture", capture)
	}

	// THE WIDTH, with no consumer pipeline, no encoder and no SRT anywhere in
	// the process. This is R2's central claim and the number it is held to is the
	// card's own sixteen.
	//
	// IT IS POLLED AND NOT READ ONCE, and the reason is a measurement rather than
	// caution. Start returned at +108 ms and aconv:sink published its negotiated
	// caps at +115 ms, so a single read the instant Start returns finds 0 about as
	// often as it finds 16 — a seven-millisecond race against a live source that
	// has to produce a buffer before anything can negotiate. That is the shipped
	// contract and not a defect: buildLocked treats a zero here as "the pad has not
	// settled yet" and says so in the log, CaptureOpts documents InputChannels as 0
	// before anything has negotiated, and the application is driven by
	// OnInputChannels rather than by polling.
	//
	// So what is asserted is the thing the routing panel actually needs: the width
	// arrives, it is the card's sixteen, and it arrives FAST ENOUGH THAT NOBODY
	// SEES THE GAP. One second is two orders of magnitude above the 115 ms measured
	// and still far below the twenty minutes the operator has before kick-off.
	const widthDeadline = time.Second
	widthAt := time.Duration(0)
	for waited := time.Duration(0); waited <= widthDeadline; waited += 5 * time.Millisecond {
		if capture.InputChannels() == deckLinkAudioChannels {
			widthAt = time.Since(startAt)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if widthAt == 0 {
		t.Errorf("SHIP-BLOCKER: InputChannels() never reached %d within %s of Start (it reads %d). "+
			"The routing panel is sized from this and it is answered before anything has been sent",
			deckLinkAudioChannels, widthDeadline, capture.InputChannels())
	} else {
		t.Logf("aconv:sink negotiated %d input channels at +%s, with no consumer pipeline, no "+
			"encoder and no SRT anywhere in this process",
			deckLinkAudioChannels, widthAt.Round(time.Millisecond))
	}

	// The log counter is installed AFTER Start, so that the build's own (correct
	// and wanted) description dump is not counted as steady-state noise.
	counter := &logLineCounter{}
	prevOut := log.Writer()
	log.SetOutput(io.MultiWriter(prevOut, counter))
	defer log.SetOutput(prevOut)

	gstLogAt := int64(-1)
	if p := os.Getenv("WSLCOMMS_LIVE_GST_LOG"); p != "" {
		if info, err := os.Stat(p); err == nil {
			gstLogAt = info.Size()
		}
	}

	// ------------------------------------------------------------------- hold
	type sample struct {
		at        time.Duration
		rss       int64
		cpu       time.Duration
		programme int
		picker    int
		logLines  int
	}
	var samples []sample
	record := func() sample {
		rss, err := processRSS()
		if err != nil {
			t.Logf("reading RSS: %v", err)
		}
		s := sample{
			at:        time.Since(startAt),
			rss:       rss,
			cpu:       processCPU(),
			programme: programme.countOnly(),
			picker:    picker.countOnly(),
			logLines:  counter.count(),
		}
		samples = append(samples, s)
		return s
	}
	record()

	deadline := time.Now().Add(hold)
	settled := startAt.Add(captureHoldSettle)
	var settledSample sample
	for time.Now().Before(deadline) {
		next := time.Now().Add(captureHoldSample)
		if next.After(deadline) {
			next = deadline
		}
		waitUntil(next)
		s := record()
		if settledSample.at == 0 && time.Now().After(settled) {
			settledSample = s
		}

		prev := samples[len(samples)-2]
		window := s.at - prev.at
		t.Logf("[+%5s] rss %6.1f MB | cpu %5.1f%% of one core | alevel %5.1f/s | chlevel %5.1f/s | %d log line(s)",
			s.at.Round(time.Second),
			float64(s.rss)/(1024*1024),
			100*float64(s.cpu-prev.cpu)/float64(window),
			float64(s.programme-prev.programme)/window.Seconds(),
			float64(s.picker-prev.picker)/window.Seconds(),
			s.logLines-prev.logLines)

		// The health question is asked EVERY sample and not only at the end. A
		// capture that died at minute one and a capture that died at minute three
		// are the same reading afterwards, and they are not the same fault.
		if err := capture.Health(); err != nil {
			t.Fatalf("[+%s] Health() reports the capture has died: %v", s.at.Round(time.Second), err)
		}
		// The bus, every sample, for the same reason. A queue that grows is
		// invisible until the process runs out of memory.
		if cgo.bus != nil && cgo.bus.HavePending() {
			t.Errorf("[+%s] the capture bus has QUEUED messages. Both handlers must return "+
				"BusDrop: nothing else in this process reads this bus, so anything left on it is a "+
				"leak that grows for the life of the application — measured at 7,168 messages in "+
				"the probe that let one through", s.at.Round(time.Second))
		}
		if got := capture.InputChannels(); got != deckLinkAudioChannels {
			t.Errorf("[+%s] InputChannels() = %d, want %d — the routing width stopped being "+
				"answerable while the capture was up", s.at.Round(time.Second), got, deckLinkAudioChannels)
		}
	}

	first, last := samples[0], samples[len(samples)-1]
	total := last.at

	// ------------------------------------------------------------- the meters
	//
	// alevel is interval=50000000 and chlevel is interval=100000000, so 20/s and
	// 10/s are what the elements were configured for rather than what was hoped
	// for. The bound is generous in both directions — this is a live source and
	// the interval is a floor, not a promise — but a HOLE is not, and that is the
	// second reading.
	assertMeter := func(m *meterCounter, wantPerSecond float64, interval time.Duration) {
		t.Helper()
		count, mfirst, mlast, gap, gapAt, channels := m.snapshot()
		if count == 0 {
			t.Errorf("SHIP-BLOCKER: %s posted NOTHING in %s. The meters are R1's whole promise and "+
				"they are dead with no send pipeline in the process", m.name, total.Round(time.Second))
			return
		}
		span := mlast.Sub(mfirst)
		rate := float64(count-1) / span.Seconds()
		t.Logf("%s: %d frames over %s = %.2f/s (configured %.0f/s), largest gap %s at +%s, "+
			"last frame carried %d channel(s)",
			m.name, count, span.Round(time.Second), rate, wantPerSecond,
			gap.Round(time.Millisecond), gapAt.Sub(startAt).Round(time.Second), channels)

		if rate < wantPerSecond*0.8 || rate > wantPerSecond*1.2 {
			t.Errorf("SHIP-BLOCKER: %s ran at %.2f frames a second against the %.0f its interval "+
				"asks for", m.name, rate, wantPerSecond)
		}
		// A gap of more than four intervals is a stall rather than jitter. The
		// seam proof holds the same elements to 68 ms against a 50 ms interval
		// across four build/run/destroy cycles of a send pipeline; with NOTHING
		// attached there is even less to disturb them.
		if limit := 4 * interval; gap > limit {
			t.Errorf("SHIP-BLOCKER: %s went quiet for %s at +%s, against a %s interval. A meter "+
				"that stops is R1 breaking silently — there is nothing on the bus when it happens",
				m.name, gap.Round(time.Millisecond), gapAt.Sub(startAt).Round(time.Second), interval)
		}
		// AND IT MUST STILL BE RUNNING NOW. A meter that stopped in the last few
		// seconds leaves a gap that has not happened yet, so the gap reading above
		// cannot see it; without this a capture that died at the very end of the
		// hold passes every assertion in this function.
		if since := time.Since(mlast); since > 4*interval {
			t.Errorf("SHIP-BLOCKER: %s's last frame arrived %s ago, against a %s interval; it "+
				"stopped before the hold did", m.name, since.Round(time.Millisecond), interval)
		}
	}
	assertMeter(programme, 20, 50*time.Millisecond)
	assertMeter(picker, 10, 100*time.Millisecond)

	// The picker's frames must carry the CARD's own sixteen and not the
	// programme pair. chlevel sits above the mix matrix precisely so that an
	// operator can ask a commentator to talk and watch which of sixteen bars
	// moves; two bars there would be a duplicate of the programme meter.
	if _, _, _, _, _, ch := picker.snapshot(); ch != deckLinkAudioChannels {
		t.Errorf("%s frames carry %d channel(s), want %d — the per-channel picker is not "+
			"measuring the device's own inputs", channelLevelElementName, ch, deckLinkAudioChannels)
	}

	// ------------------------------------------------------------ the process
	cpuPercent := 100 * float64(last.cpu-first.cpu) / float64(total)
	rssDrift := last.rss - first.rss
	var peak int64
	for _, s := range samples {
		if s.rss > peak {
			peak = s.rss
		}
	}
	t.Logf("PROCESS over %s with no consumer: CPU %.1f%% of one core; RSS %.1f MB -> %.1f MB "+
		"(peak %.1f MB, drift %+.1f MB); %d log line(s) = %.1f a minute",
		total.Round(time.Second), cpuPercent,
		float64(first.rss)/(1024*1024), float64(last.rss)/(1024*1024),
		float64(peak)/(1024*1024), float64(rssDrift)/(1024*1024),
		last.logLines, float64(last.logLines)/total.Minutes())
	if settledSample.at > 0 {
		t.Logf("PROCESS after the %s settle: RSS %.1f MB -> %.1f MB (drift %+.1f MB over %s)",
			captureHoldSettle,
			float64(settledSample.rss)/(1024*1024), float64(last.rss)/(1024*1024),
			float64(last.rss-settledSample.rss)/(1024*1024),
			(last.at - settledSample.at).Round(time.Second))
	}
	if gstLogAt >= 0 {
		if info, err := os.Stat(os.Getenv("WSLCOMMS_LIVE_GST_LOG")); err == nil {
			t.Logf("GStreamer debug log grew %d bytes during the hold (GST_DEBUG=%s)",
				info.Size()-gstLogAt, os.Getenv("GST_DEBUG"))
		}
	}

	// ------------------------------------------------------------- the faults
	faultMu.Lock()
	gotFaults := append([]string(nil), faults...)
	gotWarnings := append([]string(nil), warnings...)
	faultMu.Unlock()

	for _, w := range gotWarnings {
		t.Logf("warning: %s", w)
	}
	if len(gotFaults) > 0 {
		t.Errorf("SHIP-BLOCKER: %d fault(s) reached the application while the capture merely sat "+
			"there with nothing attached to it:\n  %s", len(gotFaults), strings.Join(gotFaults, "\n  "))
	}
	if err := capture.Health(); err != nil {
		t.Errorf("SHIP-BLOCKER: Health() reports the capture died during the hold: %v", err)
	}

	widthMu.Lock()
	gotWidths := append([]widthArrival(nil), widths...)
	widthMu.Unlock()
	for _, w := range gotWidths {
		t.Logf("OnInputChannels: %s = %d channels at +%s", w.key, w.width, w.at.Round(time.Millisecond))
	}
	if len(gotWidths) == 0 {
		t.Errorf("OnInputChannels was never called. The routing panel is gated on a width STAMPED " +
			"with its device, so a panel that never receives one stays hidden for ever")
	} else {
		if got := gotWidths[0].width; got != deckLinkAudioChannels {
			t.Errorf("the first published width was %d, want %d", got, deckLinkAudioChannels)
		}
		if want := string(KindDeckLink) + ":" + card; gotWidths[0].key != want {
			t.Errorf("the width was stamped %q, want %q", gotWidths[0].key, want)
		}
		// Republishing the same width every renegotiation would drive the panel
		// to rebuild its grid repeatedly over the whole match.
		if len(gotWidths) > 3 {
			t.Errorf("OnInputChannels fired %d times in %s for one device that never changed "+
				"width; the de-duplication in publishWidths is not holding", len(gotWidths),
				total.Round(time.Second))
		}
	}

	signalMu.Lock()
	gotSignals := append([]SignalReport(nil), signalReports...)
	signalMu.Unlock()
	t.Logf("signal watchdog: %d report(s) over the hold: %v", len(gotSignals), gotSignals)

	// -------------------------------------------------------------- teardown
	//
	// Stop is asserted rather than left to the cleanup, because releasing the
	// exclusive card is half of what the always-live design promises: the card is
	// held from launch to quit and QUIT MUST GIVE IT BACK.
	stopAt := time.Now()
	if err := capture.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stopped = true
	t.Logf("Stop returned in %s; the card is released", time.Since(stopAt).Round(time.Millisecond))

	// Stop closes the fault channels, which is what ends the two draining
	// goroutines. If it did not, this waits for ever and the test times out
	// naming this line — which is the correct failure for a teardown that leaks
	// the goroutines every capture rebuild would create afresh.
	drainWG.Wait()

	if err := capture.Stop(); err != nil {
		t.Errorf("the second Stop returned %v; it is documented idempotent and a teardown that "+
			"cannot be run twice cannot be run from a failure path", err)
	}
}
