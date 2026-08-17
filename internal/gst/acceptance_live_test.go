//go:build live && cgo && !gststub

// acceptance_live_test.go is the acceptance run for the send pipeline over the
// always-live seam, ON THE WIRE.
//
// # What this adds to the proofs that already exist
//
// TestLiveProxySeamRearmsEverySendSession proves the MECHANISM against
// hand-built pipelines. TestLiveShippedSendPipelineCarriesMediaOverTheSeam
// proves the SHIPPED code performs it — but deliberately with no sink at all,
// reading the in-process buffer counters and stopping there.
//
// Neither of them puts a byte on a socket, and neither of them can answer the
// question this file exists for: WHAT DOES THE FAR END ACTUALLY RECEIVE, three
// send sessions in a row, across a reconnect, on the card?
//
// So everything here runs against cmd/mockm2lx — its real SRT listener, with
// the real one-peer re-accept-refusal semantics, and its from-scratch TS/PES
// parser reading the bytes back off the wire. The assertions are the parser's:
// bytes arrived, the PIDs are H.264 and AAC, and DTS never went backwards.
//
// # The one that matters most, and its exact scope
//
// cmd/mockm2lx/mpegts.go exists for one measured failure: "audio DTS jumping
// backwards by exactly the previous run's uptime, 1,523 non-monotonic errors
// downstream, commentary never returning while every indicator read healthy".
// That is a PIPELINE-RESTART bug, and this refactor changes how pipelines
// restart — the capture clock now runs continuously while the send pipeline is
// destroyed and rebuilt underneath it. It is the single most likely thing this
// work has broken.
//
// BUT THE MOCK'S DETECTOR IS PER-SRT-SESSION, and that limit has to be written
// down rather than discovered by someone trusting a green run. srt.go:203
// installs a FRESH tsAnalyzer on every accept, so `DTSBackwards` latches over
// one connection and one only. A START/STOP/START cycle makes three connections
// and therefore three independent detectors, and no amount of reading
// /control/state can compare cycle 2's timeline against cycle 1's.
//
// That is why muxInputTimeline below exists. It is an in-process BUFFER probe on
// `aq:src` — the audio queue feeding the muxer, the exact stream the measured
// bug was in, carrying plain buffers rather than the alignment=7 LISTS that
// mux:src pushes — and it is installed ONCE and read across ALL THREE CYCLES.
// Between them it answers both halves:
//
//   - the mock: DTS is monotonic WITHIN each session, on the real wire format,
//     parsed by something that is not this package;
//   - the probe: the muxer's input timeline ACROSS sessions, so "cycle 2 began
//     at the previous run's uptime" is a number in the log rather than an
//     inference.
//
// # The cross-cycle numbers, and the two expectations that are BOTH wrong
//
// The obvious assertion — "the capture ran continuously, so cycle 2's first
// `aq:src` PTS must be ABOVE cycle 1's last" — is wrong. Its opposite is also
// wrong. Both were written, run and falsified here before this comment replaced
// them, because THE TWO SOURCE KINDS DO NOT AGREE. Measured on this rig,
// 2026-08-17, three 30 s cycles with the far end's refusal window waited out
// between them:
//
//	slate + built-in microphone (native)      fused card (picture + commentary)
//	cycle 1   64 ms ..  30.165 s              cycle 1    231 ms ..    30.247 s
//	cycle 2   43 ms ..  30.123 s              cycle 2  38.311 s ..  1m08.370 s
//	cycle 3   21 ms ..  30.101 s              cycle 3  1m16.482 s .. 1m46.541 s
//
// The native seat RESTARTS its muxer-input timeline near zero on every session.
// The card CARRIES ON, session 2 beginning at 38 s against a capture 38 s old.
// Same seam, same shipped code, same three cycles; the difference is the
// segment the producer puts across the proxy, and no comment in this package
// claims either shape.
//
// AND THE WIRE IS CORRECT IN BOTH. Every one of those six sessions carried
// ~8.15 MB of H.264 + AAC with `DTSBackwards=false` at the far end, because
// mpegtsmux normalises the TS's first PTS per session — which is exactly what
// PLAN.md section 3.5 recorded ("normalised TS first PTS to 0.000000 on every
// cycle at every join offset") and what section 9 step 5 accepted ("TS first
// PTS 0.000000 with capture already an hour old"). savedBase keeps the
// pipeline's RUNNING TIME continuous, which is what gst_cgo.go's savedBase doc
// is about; buffer PTS at `aq:src` is segment-relative and is a different
// quantity. The product invariant lives on the wire and both shapes satisfy it.
//
// So the guard below asserts neither shape. It asserts the thing that is true
// of both and would be false of a corrupted timeline: a new session either
// RESTARTS (near zero) or CONTINUES (at or after the previous session's last
// stamp), and never lands somewhere INSIDE the previous session's span. That
// third reading is the one no mechanism produces on purpose, and it is the
// shape "jumping backwards by exactly the previous run's uptime" would take if
// it ever came back on a seat where the timeline is supposed to carry on.
//
// # What this opens
//
// Two shapes. The native tests open the platform's own capture endpoint and the
// slate PNG, and NEVER a Continuity device — liveNativeInputID is what enforces
// that, and the reason is that the operator's phone is in another room and
// opening it chimes out of the phone. The card tests open the UltraStudio.
//
// `connection` IS NOT SET, on any element, anywhere in this file. It is not a
// per-pipeline selection: it PERSISTENTLY RECONFIGURES THE CARD and overrides
// what the operator chose in Desktop Video Setup. This has been got wrong twice
// and corrected by hand twice.
//
// # Running it
//
//	go build -o mockm2lx ./cmd/mockm2lx
//	./mockm2lx -addr 127.0.0.1:8099 -srt-addr 127.0.0.1:9002 -status-interval=1s
//
//	WSLCOMMS_LIVE_APP_DIR=<symlink farm>/Contents/MacOS \
//	CGO_LDFLAGS="-framework UniformTypeIdentifiers" \
//	go test -tags "live cgo" -run TestLiveAcceptance -timeout 30m -v ./internal/gst
//
// Do NOT point WSLCOMMS_LIVE_APP_DIR at the shipped .app: its plugins resolve
// the core through @loader_path and loading them beside Homebrew segfaults.
package gst

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

const (
	// acceptanceHold is how long each send session is left connected before it
	// is measured. Thirty seconds is the operator's number and it is not
	// arbitrary: it is long enough that BytesTotal is a RATE rather than a
	// preroll burst, so a cycle that connected, emitted its headers and then
	// went quiet reads as a bitrate collapse instead of as a pass.
	acceptanceHold = 30 * time.Second

	// acceptanceMinBytes is the floor a 30 s session must clear on the wire.
	//
	// It is set from the encoder's own configuration rather than from an
	// observed number, so that it stays meaningful if the rig changes: audio
	// alone is 128 kbps = 480 kB over 30 s, and any session carrying real video
	// as well is far above that. The failure this catches is the measured one —
	// a session that connects, goes green and writes ZERO — and for that a floor
	// three orders of magnitude below the expected value is as good as a tight
	// one, while never failing on a slow machine.
	acceptanceMinBytes = 480 * 1024
)

// ---------------------------------------------------------------------------
// The far end: cmd/mockm2lx's /control/state
// ---------------------------------------------------------------------------

// mockAnalyzerSnapshot mirrors cmd/mockm2lx's AnalyzerSnapshot.
//
// THE FIELD NAMES ARE THE WIRE KEYS. That struct carries no json tags, so
// encoding/json renders it by Go field name; a tag added here would silently
// decode to zero values, which on DTSBackwards would read as "no regression"
// and pass. Left untagged deliberately, and it is checked: mockState below
// fails the test if a connected session decodes an analyzer with no bytes in
// it, which is what a shape mismatch looks like.
type mockAnalyzerSnapshot struct {
	BytesTotal         uint64
	BitrateBps         float64
	HaveVideoPID       bool
	HaveAudioPID       bool
	VideoIsH264        bool
	AudioIsAAC         bool
	DTSBackwards       bool
	DTSBackwardsDetail string
}

// describe renders one session's reading as a single log line.
func (a mockAnalyzerSnapshot) describe() string {
	return fmt.Sprintf("%.2f MB  %.2f Mbps  video=%v(h264=%v) audio=%v(aac=%v)  dtsBackwards=%v",
		float64(a.BytesTotal)/(1024*1024), a.BitrateBps/1e6,
		a.HaveVideoPID, a.VideoIsH264, a.HaveAudioPID, a.AudioIsAAC, a.DTSBackwards)
}

type mockSRTSnapshot struct {
	Connected      bool                  `json:"connected"`
	PeerAddr       string                `json:"peerAddr"`
	ConnectedAt    *time.Time            `json:"connectedAt"`
	DisconnectedAt *time.Time            `json:"disconnectedAt"`
	Analyzer       *mockAnalyzerSnapshot `json:"analyzer"`
}

type mockControlState struct {
	OnePeerOnly   bool            `json:"onePeerOnly"`
	RefusalWindow string          `json:"refusalWindow"`
	SRT           mockSRTSnapshot `json:"srt"`
}

// mockState reads GET /control/state.
func mockState(t *testing.T, control string) mockControlState {
	t.Helper()
	resp, err := http.Get(control + "/control/state")
	if err != nil {
		t.Fatalf("GET %s/control/state: %v", control, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s/control/state: HTTP %d", control, resp.StatusCode)
	}
	var st mockControlState
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decoding /control/state: %v", err)
	}
	return st
}

// requireMock skips unless cmd/mockm2lx is answering, and returns its control
// URL and SRT port.
//
// It SKIPS rather than fails: the mock is started by hand, and a developer
// running the whole live suite without it should be told what to start, not
// handed a red run they cannot interpret.
func requireMock(t *testing.T) (control string, srtPort int) {
	t.Helper()
	control = env("WSLCOMMS_LIVE_MOCK_CONTROL", defaultMockControl)
	srtPort = envInt("WSLCOMMS_LIVE_MOCK_SRT_PORT", defaultMockSRTPort)
	resp, err := http.Get(control + "/control/state")
	if err != nil {
		t.Skipf("cmd/mockm2lx is not answering on %s (%v). Start it first:\n"+
			"    go build -o mockm2lx ./cmd/mockm2lx\n"+
			"    ./mockm2lx -addr 127.0.0.1:8099 -srt-addr 127.0.0.1:%d -status-interval=1s",
			control, err, srtPort)
	}
	resp.Body.Close()
	return control, srtPort
}

// waitForMockReady blocks until cmd/mockm2lx will actually accept a caller.
//
// IT IS NOT A BLIND SLEEP, and the difference cost a red run. The listener
// refuses re-accept for its refusal window (6 s by default, deliberately above
// the ~5 s measured on the real M2L-X) after a peer goes away, and a test that
// merely slept between its OWN cycles still walked into the window left by the
// PREVIOUS TEST in the same binary: the card run failed at cycle 1 with
// "ReplaceSink returned nil but the mock never reported a peer" purely because
// the native run above it had stopped 5 s earlier.
//
// So the wait is computed from the far end's own numbers — disconnectedAt plus
// the refusal window it reports — and is therefore correct whatever ran before,
// and whatever the window is set to.
func waitForMockReady(t *testing.T, control string) {
	t.Helper()
	st := mockState(t, control)
	if st.SRT.Connected {
		t.Fatalf("cmd/mockm2lx already has a peer (%s): something else is sending to it, and "+
			"one-peer-only means this run cannot connect", st.SRT.PeerAddr)
	}
	if st.SRT.DisconnectedAt == nil {
		return // nothing has ever connected; no window to wait out
	}
	window, err := time.ParseDuration(st.RefusalWindow)
	if err != nil {
		// Unparseable is not fatal: fall back to the documented default rather
		// than failing a run over a log-formatting change at the far end.
		window = 6 * time.Second
	}
	// One second of margin over the window's own end. internal/sender's first
	// backoff rung is 7 s against a 6 s window for the same reason: a test that
	// sits exactly on a production tolerance turns a timing wobble into a red
	// run.
	until := st.SRT.DisconnectedAt.Add(window + time.Second)
	if d := time.Until(until); d > 0 {
		t.Logf("waiting %v for the mock's re-accept refusal window (%v) to clear",
			d.Round(time.Millisecond), window)
		time.Sleep(d)
	}
}

// waitForMockAccept blocks until the mock reports a connected SRT peer, so that
// a cycle measures a session that exists.
//
// ReplaceSink returning nil already means the caller handshake succeeded, so
// this is the FAR END's confirmation of the same fact rather than a substitute
// for it — and it is the assertion that catches a refusal window the test
// walked into, which srtsink itself reports as an ordinary connect failure.
func waitForMockAccept(t *testing.T, control string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if mockState(t, control).SRT.Connected {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// ---------------------------------------------------------------------------
// The muxer's INPUT timeline, across sessions
// ---------------------------------------------------------------------------

// muxInputTimeline records the PTS of every buffer crossing `aq:src` — the
// audio queue feeding mpegtsmux.
//
// It is the cross-session half of the DTS question, for the reason in the file
// comment: the mock's detector is reset on every accept and cannot see past one
// connection. This is installed once per send pipeline and its readings are
// carried across all three cycles by the caller.
//
// `aq:src` and not `mux:src`: mpegtsmux at alignment=7 pushes buffer LISTS, and
// gst_pad_probe_info_get_buffer is g_return_val_if_fail'd on the info's own type
// bit, so a BUFFER probe there reads nothing while megabytes flow — the same
// trap livewatch_cgo.go pays six probes to avoid. `aq` carries aacparse's plain
// output buffers and needs one.
type muxInputTimeline struct {
	mu        sync.Mutex
	haveFirst bool
	first     time.Duration
	last      time.Duration
	buffers   int64

	// backwards latches an input-timeline regression WITHIN one send session,
	// independently of the mock. It is belt and braces against the case where
	// the muxer hides a bad input behind a re-stamped output.
	backwards       bool
	backwardsDetail string
}

func (m *muxInputTimeline) probe(_ gogst.Pad, info *gogst.PadProbeInfo) gogst.PadProbeReturn {
	buf := info.GetBuffer()
	if buf == nil {
		return gogst.PadProbeOK
	}
	pts := buf.PTS()
	if pts == gogst.ClockTimeNone {
		return gogst.PadProbeOK
	}
	at := time.Duration(pts)

	m.mu.Lock()
	if !m.haveFirst {
		m.first, m.haveFirst = at, true
	} else if at < m.last && !m.backwards {
		m.backwards = true
		m.backwardsDetail = fmt.Sprintf("aq:src PTS went backwards: %v -> %v (Δ -%v)",
			m.last.Round(time.Millisecond), at.Round(time.Millisecond),
			(m.last - at).Round(time.Millisecond))
	}
	m.last = at
	m.buffers++
	m.mu.Unlock()
	return gogst.PadProbeOK
}

// read returns this session's first and last input PTS and how many buffers
// were seen.
func (m *muxInputTimeline) read() (first, last time.Duration, buffers int64, backwards bool, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.first, m.last, m.buffers, m.backwards, m.backwardsDetail
}

// attachMuxInputTimeline installs the probe on a started send pipeline's
// `aq:src`.
func attachMuxInputTimeline(t *testing.T, p *cgoPipeline) *muxInputTimeline {
	t.Helper()
	q := p.pipeline.GetByName(nameMuxAudioQueue)
	if q == nil {
		t.Fatalf("no %q element in the send pipeline — sendDescription has changed shape and "+
			"this test's cross-session DTS proof is no longer measuring anything", nameMuxAudioQueue)
	}
	pad := q.GetStaticPad("src")
	if pad == nil {
		t.Fatalf("%s has no src pad", nameMuxAudioQueue)
	}
	tl := &muxInputTimeline{}
	pad.AddProbe(gogst.PadProbeTypeBuffer, tl.probe)
	return tl
}

// ---------------------------------------------------------------------------
// One send session, start to stop, measured at both ends
// ---------------------------------------------------------------------------

// sendCycle is one START / connect / hold / STOP, and everything both ends saw.
type sendCycle struct {
	n         int
	startedIn time.Duration
	analyzer  mockAnalyzerSnapshot

	// firstInput / lastInput are this session's muxer-input PTS range. They are
	// what the cross-session comparison is made on.
	firstInput, lastInput time.Duration
	inputBuffers          int64
	inputBackwards        bool
	inputDetail           string

	// pads is the liveness watchdog's reading at the end of the hold: the
	// in-process cross-check that the byte count on the wire came from media
	// crossing the muxer rather than from anything else.
	pads []liveWatchSample

	// shape is how this session's timeline related to the previous one. Empty
	// on cycle 1, which has nothing to be related to.
	shape timelineShape
}

// runSendCycle mints one send pipeline over an existing capture set, connects it
// to the mock, holds it, measures it and stops it.
//
// THE CAPTURE SET IS NOT TOUCHED. That is the whole point of the seam and it is
// what makes cycles 2 and 3 the interesting ones: the proxysinks never make
// another READY->PAUSED transition of their own, so without ArmForSend at each
// START they carry no STREAM_START, no CAPS and no SEGMENT to the second
// consumer and the feed is silent with every lamp green.
func runSendCycle(t *testing.T, set CaptureSet, n int, control string, srtPort int, hold time.Duration) sendCycle {
	t.Helper()

	res := sendCycle{n: n}

	pipe, err := New(set)
	if err != nil {
		t.Fatalf("cycle %d: New: %v", n, err)
	}
	cp, ok := pipe.(*cgoPipeline)
	if !ok {
		t.Fatalf("cycle %d: New returned a %T, not a *cgoPipeline", n, pipe)
	}
	// Drained, not ignored: a full error channel drops, and a dropped error is
	// the one this run would otherwise miss.
	var errMu sync.Mutex
	var sendErrs []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range pipe.Errors() {
			errMu.Lock()
			sendErrs = append(sendErrs, e.Error())
			errMu.Unlock()
		}
	}()

	stopped := false
	defer func() {
		if !stopped {
			_ = pipe.Stop()
		}
		<-done
		errMu.Lock()
		for _, e := range sendErrs {
			t.Errorf("cycle %d: the send pipeline reported: %s", n, e)
		}
		errMu.Unlock()
	}()

	t0 := time.Now()
	if err := pipe.Start(SendOpts{}); err != nil {
		t.Fatalf("cycle %d: Start: %v (after %v). THIS IS THE ZERO-BYTE SESSION: the liveness "+
			"gate refused because nothing reached the muxer, which on the shipped code means the "+
			"seam was not armed, not bound, or bound to a proxysink that had already been consumed",
			n, err, time.Since(t0))
	}
	res.startedIn = time.Since(t0)

	tl := attachMuxInputTimeline(t, cp)

	sink := SinkOpts{Host: "127.0.0.1", Port: srtPort, LatencyMs: DefaultSRTLatencyMs}
	waitForMockReady(t, control)
	if err := pipe.ReplaceSink(sink); err != nil {
		t.Fatalf("cycle %d: ReplaceSink to the mock at 127.0.0.1:%d: %v", n, srtPort, err)
	}
	if !waitForMockAccept(t, control, 5*time.Second) {
		t.Fatalf("cycle %d: ReplaceSink returned nil but the mock never reported a peer; "+
			"the most likely cause is the re-accept refusal window (%s) still being open",
			n, mockState(t, control).RefusalWindow)
	}
	_ = pipe.ForceKeyUnit()

	time.Sleep(hold)

	// READ BEFORE STOP, while the session is still up. The mock keeps the last
	// session's analyzer after a disconnect, but reading it live is what makes
	// BitrateBps a measurement of this cycle rather than of a stream that ended
	// at an unknown instant.
	st := mockState(t, control)
	if st.SRT.Analyzer == nil {
		t.Fatalf("cycle %d: the mock reports no analyzer after %v connected; either nothing "+
			"was ever accepted or /control/state's shape has moved", n, hold)
	}
	res.analyzer = *st.SRT.Analyzer
	res.pads = cp.live.samples()
	res.firstInput, res.lastInput, res.inputBuffers, res.inputBackwards, res.inputDetail = tl.read()

	if err := pipe.Stop(); err != nil {
		t.Errorf("cycle %d: Stop: %v", n, err)
	}
	stopped = true

	return res
}

// assertCycle applies every per-cycle assertion the acceptance run calls for.
func assertCycle(t *testing.T, c sendCycle) {
	t.Helper()

	a := c.analyzer
	if a.BytesTotal < acceptanceMinBytes {
		t.Errorf("cycle %d: only %d bytes reached the far end in %v (floor %d). "+
			"A cycle after the first writing near zero with SRT connected IS the measured "+
			"un-armed-proxysink failure", c.n, a.BytesTotal, acceptanceHold, acceptanceMinBytes)
	}
	if !a.HaveVideoPID || !a.VideoIsH264 {
		t.Errorf("cycle %d: the far end did not see an H.264 video PID (haveVideo=%v h264=%v)",
			c.n, a.HaveVideoPID, a.VideoIsH264)
	}
	if !a.HaveAudioPID || !a.AudioIsAAC {
		t.Errorf("cycle %d: the far end did not see an AAC audio PID (haveAudio=%v aac=%v). "+
			"An audio PID that is present but NOT AAC is the MP2/AC-3 signature M2L-X drops "+
			"silently", c.n, a.HaveAudioPID, a.AudioIsAAC)
	}
	if a.DTSBackwards {
		t.Errorf("cycle %d: DTS WENT BACKWARDS ON THE WIRE: %s. This is the measured "+
			"pipeline-restart bug — commentary never returns while every indicator reads "+
			"healthy", c.n, a.DTSBackwardsDetail)
	}
	if c.inputBackwards {
		t.Errorf("cycle %d: the MUXER'S INPUT timeline went backwards: %s", c.n, c.inputDetail)
	}
	if c.inputBuffers == 0 {
		t.Errorf("cycle %d: nothing at all crossed %s:src, so this cycle's cross-session "+
			"timeline reading is empty", c.n, nameMuxAudioQueue)
	}
	for _, s := range c.pads {
		if s.Buffers == 0 {
			t.Errorf("cycle %d: %s:src carried nothing across the whole hold", c.n, s.Pad)
		}
	}
}

// acceptanceRestartCeiling is how far into its own timeline a fresh send
// session's first muxer-input buffer may be stamped.
//
// It is generous — a whole second against a measured 21-85 ms — because its job
// is to separate "restarted at zero, as designed" from "carried on from a
// capture that is 40 s or an hour old", two readings that differ by orders of
// magnitude. A tight bound here would only make the test fragile on a slow
// start with nothing gained.
const acceptanceRestartCeiling = time.Second

// timelineShape is how one session's muxer-input timeline relates to the
// previous session's.
type timelineShape string

const (
	// timelineRestarted: the new session begins near zero, however old the
	// capture underneath it is. Measured on the native seat.
	timelineRestarted timelineShape = "restarted"

	// timelineContinued: the new session begins at or after the previous
	// session's last stamp. Measured on the fused card.
	timelineContinued timelineShape = "continued"

	// timelineCorrupt: neither — the new session begins INSIDE the previous
	// session's span. No mechanism in this design produces this on purpose.
	timelineCorrupt timelineShape = "CORRUPT"
)

// classifyTimeline says which of the three a consecutive pair is.
func classifyTimeline(prev, cur sendCycle) timelineShape {
	switch {
	case cur.firstInput <= acceptanceRestartCeiling:
		return timelineRestarted
	case cur.firstInput >= prev.lastInput:
		return timelineContinued
	default:
		return timelineCorrupt
	}
}

// assertTimelineCoherent is the cross-session guard the mock cannot make,
// because its analyzer is reset on every accept and cannot see past one
// connection.
//
// IT ASSERTS NEITHER SHAPE, and the file comment above carries the measurement
// that says why: the native seat restarts its muxer-input timeline near zero on
// every session and the fused card carries on from where the last one ended,
// with the same shipped code, and the wire is correct in both because mpegtsmux
// normalises the TS per session. An assertion that demanded either one would be
// pinning an accident of the producer's segment and would fail on half the
// seats this product ships to.
//
// What it does assert is the reading NEITHER mechanism produces: a session
// beginning inside the previous session's span, which is neither a clean
// restart nor a continuation. On a seat where the timeline is supposed to carry
// on, that is the shape "jumping backwards by exactly the previous run's
// uptime" would take, and it is worth one comparison to catch it.
func assertTimelineCoherent(t *testing.T, prev, cur sendCycle) timelineShape {
	t.Helper()
	if cur.inputBuffers == 0 || prev.inputBuffers == 0 {
		return timelineCorrupt // already reported by assertCycle
	}
	shape := classifyTimeline(prev, cur)
	if shape == timelineCorrupt {
		t.Errorf("cycle %d began at muxer-input PTS %v, which is neither a restart (<= %v) nor "+
			"a continuation (>= cycle %d's last stamp %v) but INSIDE the previous session's "+
			"span %v..%v. A timeline that neither restarted nor carried on is the shape a "+
			"backwards leap of the previous run's uptime takes; see this file's header",
			cur.n, cur.firstInput.Round(time.Millisecond), acceptanceRestartCeiling,
			prev.n, prev.lastInput.Round(time.Millisecond),
			prev.firstInput.Round(time.Millisecond), prev.lastInput.Round(time.Millisecond))
	}
	return shape
}

// reportCycles writes the whole run as one block, so a reader compares the three
// cycles side by side instead of scrolling.
func reportCycles(t *testing.T, title string, cycles []sendCycle) {
	t.Helper()
	fmt.Fprintf(os.Stderr, "\n>>>> %s\n", title)
	for _, c := range cycles {
		var pads []string
		for _, s := range c.pads {
			pads = append(pads, fmt.Sprintf("%s:src %d", s.Pad, s.Buffers))
		}
		fmt.Fprintf(os.Stderr, "  cycle %d  start %-9v  %s\n", c.n,
			c.startedIn.Round(time.Millisecond), c.analyzer.describe())
		shape := string(c.shape)
		if shape == "" {
			shape = "first session"
		}
		fmt.Fprintf(os.Stderr, "           mux input %v..%v (%d buffers, %s)  |  %s\n",
			c.firstInput.Round(time.Millisecond), c.lastInput.Round(time.Millisecond),
			c.inputBuffers, shape, strings.Join(pads, "  "))
	}
}

// ---------------------------------------------------------------------------
// 1 + 2. Three START/STOP cycles, native then card
// ---------------------------------------------------------------------------

// TestLiveAcceptanceEverySendSessionReachesTheWire is the acceptance run's first
// scenario: three full send sessions over ONE always-live capture, with bytes
// counted and parsed at the far end on every one of them.
//
// # The source, and an honest note about "test sources"
//
// The acceptance brief asked for test sources — slate + audiotestsrc — for this
// first pass, so that a failure here cannot be a card failure wearing a seam
// failure's name. THE REAL CAPTURE LAYER HAS NO TEST-SOURCE LEG: CommentaryLeg
// is None, Native or Card and nothing else, and adding a fourth purely for a
// test would put a description in the shipped set that no seat ever builds.
//
// The faithful equivalent, and what this runs, is slate + the platform's own
// capture endpoint: a picture that cannot lose signal and an audio source that
// cannot fail in any of the card-specific ways — no exclusivity, no driver
// callback thread, no `Dropped N old frames`, no preroll dependency on a video
// clock. TestLiveAcceptanceEverySendSessionReachesTheWireOnTheCard below is the
// same run with all of those back.
func TestLiveAcceptanceEverySendSessionReachesTheWire(t *testing.T) {
	liveInitDarwin(t)
	control, srtPort := requireMock(t)

	slate, err := filepath.Abs(env("WSLCOMMS_LIVE_SLATE", defaultSlatePath))
	if err != nil {
		t.Fatalf("resolving the slate: %v", err)
	}
	if _, err := os.Stat(slate); err != nil {
		t.Skipf("no slate at %s: %v", slate, err)
	}
	devID := liveNativeInputID(t)

	// ONE capture layer for all three sessions.
	set := liveNativeCapture(t, slate, devID)

	var cycles []sendCycle
	for n := 1; n <= 3; n++ {
		// No sleep here: runSendCycle waits out the far end's refusal window
		// from the far end's own numbers, which is correct across tests as well
		// as across cycles.
		c := runSendCycle(t, set, n, control, srtPort, acceptanceHold)
		assertCycle(t, c)
		if n > 1 {
			c.shape = assertTimelineCoherent(t, cycles[n-2], c)
		}
		cycles = append(cycles, c)
	}

	reportCycles(t, fmt.Sprintf("THREE SEND SESSIONS ON THE WIRE — slate %s + %s",
		filepath.Base(slate), devID), cycles)

	// R1 in one assertion: three send sessions have come and gone underneath the
	// capture layer and the device is still open and still carrying media.
	for _, c := range set.Pipelines() {
		if err := c.Health(); err != nil {
			t.Errorf("the %s capture did not survive three send sessions: %v", c.Legs(), err)
		}
	}
}

// TestLiveAcceptanceEverySendSessionReachesTheWireOnTheCard is scenario 1 again
// with THE REAL CARD as the commentary source, fused with the card picture.
//
// # Why the fused shape and not the slate one
//
// PlanCapture's fusion rule puts card picture and card commentary in ONE
// pipeline, because decklinkaudiosrc cannot preroll without a decklinkvideosrc
// beside it and the card is exclusive. That fused pipeline is the shape a real
// card seat runs, it is the shape where a READY cycle on the proxy queue could
// take the commentary down with the picture, and it is the only shape in which
// the arming's IDLE probe blocks a pad that a DRIVER CALLBACK THREAD is pushing
// into. All three are things a test source cannot reproduce.
//
// The rig has NO VIDEO SIGNAL on the card's video input. That is not a
// limitation here: with drop-no-signal-frames=false the element posts `Signal
// lost` once and goes on producing frames, which is everything both legs need.
func TestLiveAcceptanceEverySendSessionReachesTheWireOnTheCard(t *testing.T) {
	liveInitDarwin(t)
	control, srtPort := requireMock(t)

	card := env("WSLCOMMS_LIVE_CARD", defaultLiveCard)

	set := liveCardCapture(t, card)

	var cycles []sendCycle
	for n := 1; n <= 3; n++ {
		// No sleep here: runSendCycle waits out the far end's refusal window
		// from the far end's own numbers, which is correct across tests as well
		// as across cycles.
		c := runSendCycle(t, set, n, control, srtPort, acceptanceHold)
		assertCycle(t, c)
		if n > 1 {
			c.shape = assertTimelineCoherent(t, cycles[n-2], c)
		}
		cycles = append(cycles, c)
	}

	reportCycles(t, "THREE SEND SESSIONS ON THE WIRE — FUSED CARD (picture + commentary)", cycles)

	for _, c := range set.Pipelines() {
		if err := c.Health(); err != nil {
			t.Errorf("the %s capture did not survive three send sessions: %v", c.Legs(), err)
		}
	}
}

// liveCardCapture builds the FUSED card capture set: card picture and card
// commentary in one pipeline, no preview.
//
// No preview because the confidence monitor needs a window handle and this
// process has no window — CONTRACT.md rule 4 forbids launching the GUI, and
// PreviewOpts documents a zero handle as "do not build the branch" rather than
// as an error.
//
// `connection` is not set. See the file comment.
func liveCardCapture(t *testing.T, card string) CaptureSet {
	t.Helper()

	capture, err := NewCapture(CaptureOpts{
		Legs:           CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard},
		VideoCaptureID: card,
		AudioCaptureID: card,
		ConformTo:      FallbackConformTarget(),
		DeviceChannels: deckLinkAudioChannels,
		ChannelMap:     DefaultChannelMap(deckLinkAudioChannels),
	})
	if err != nil {
		t.Fatalf("NewCapture(FUSED, card %s): %v", card, err)
	}
	// REGISTERED BEFORE Start and before any Fatalf below can be reached. The
	// card is exclusive; an orphan holds it from the operator until the process
	// is killed, and NewCapture has already started goroutines that only Stop
	// ends.
	t.Cleanup(func() {
		if err := capture.Stop(); err != nil {
			t.Errorf("stopping the fused card capture: %v — the card may still be held", err)
		}
	})
	// The two channels the application drains. A full fault channel drops, and a
	// dropped fault is the one this run would otherwise miss.
	go func() {
		for err := range capture.Faults() {
			t.Errorf("the fused card capture faulted: %v", err)
		}
	}()
	go func() {
		for w := range capture.Warnings() {
			t.Logf("capture warning: %s", w)
		}
	}()

	if err := capture.Start(); err != nil {
		t.Fatalf("starting the fused card capture: %v", err)
	}
	return CaptureSet{Picture: capture, Commentary: capture}
}

// ---------------------------------------------------------------------------
// 3. A disconnect mid-send, and what survives it
// ---------------------------------------------------------------------------

// TestLiveAcceptanceReconnectDoesNotRestartTheCapture forces a peer loss in the
// middle of a send session and proves the three things that must hold across the
// repair.
//
// # What a reconnect is, in this design, and why that is the risk
//
// A reconnect is RemoveSink then ReplaceSink. The SEND pipeline is never
// destroyed and the CAPTURE pipeline is never touched — only the sink under the
// gate is swapped. That is what makes the muxer's timeline CONTINUE rather than
// restart, and it is the property this test is here to hold on to:
//
//  1. commentary comes back — the far end sees an AAC PID and real bytes again;
//  2. the capture pipeline never restarts — same object, still healthy, still
//     PLAYING, with the send pipeline's own claim never released;
//  3. DTS is monotonic in the new session, AND the muxer's input timeline
//     CARRIES ON from where the first session left it rather than restarting.
//
// Point 3's second half is the one the mock cannot see. Its analyzer is reset on
// every accept (srt.go:203), so the post-reconnect session's DTS is measured
// from a fresh baseline; only the in-process probe on `aq:src`, which is never
// reinstalled because the send pipeline is never rebuilt, spans both halves.
func TestLiveAcceptanceReconnectDoesNotRestartTheCapture(t *testing.T) {
	liveInitDarwin(t)
	control, srtPort := requireMock(t)

	slate, err := filepath.Abs(env("WSLCOMMS_LIVE_SLATE", defaultSlatePath))
	if err != nil {
		t.Fatalf("resolving the slate: %v", err)
	}
	if _, err := os.Stat(slate); err != nil {
		t.Skipf("no slate at %s: %v", slate, err)
	}
	devID := liveNativeInputID(t)
	set := liveNativeCapture(t, slate, devID)

	pipe, err := New(set)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cp := pipe.(*cgoPipeline)
	errs := newErrRecorder(pipe)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = pipe.Stop()
		}
	})

	if err := pipe.Start(SendOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tl := attachMuxInputTimeline(t, cp)

	sink := SinkOpts{Host: "127.0.0.1", Port: srtPort, LatencyMs: DefaultSRTLatencyMs}
	waitForMockReady(t, control)
	if err := pipe.ReplaceSink(sink); err != nil {
		t.Fatalf("ReplaceSink (first connect): %v", err)
	}
	if !waitForMockAccept(t, control, 5*time.Second) {
		t.Fatalf("the mock never accepted the first connection")
	}
	_ = pipe.ForceKeyUnit()
	time.Sleep(10 * time.Second)

	before := mockState(t, control)
	if before.SRT.Analyzer == nil || before.SRT.Analyzer.BytesTotal == 0 {
		t.Fatalf("nothing was flowing before the provocation; there is no disconnect to test")
	}
	_, lastBefore, buffersBefore, _, _ := tl.read()

	// The capture layer's identity and health BEFORE the loss, so that "never
	// restarted" is a comparison rather than a claim.
	captureBefore := set.Commentary
	errCountBefore := errs.count()

	fmt.Fprintf(os.Stderr, "\n>>>> RECONNECT MID-SEND\n")
	fmt.Fprintf(os.Stderr, "  before: %s\n", before.SRT.Analyzer.describe())

	if !dropMockSRT(t, control) {
		t.Fatalf("the mock did not report dropping a session; there is no peer loss in this run")
	}

	// The production repair, in the production order: take the dead sink out,
	// wait out the listener's re-accept refusal window, put a fresh one in. This
	// is internal/sender's DRAINING -> BACKOFF -> CONNECTING with the backoff
	// rung supplied by the test.
	if err := pipe.RemoveSink(); err != nil {
		t.Errorf("RemoveSink after the peer loss: %v", err)
	}

	// THE CAPTURE MUST STILL BE UP WITH NO SINK AT ALL. This is the assertion
	// that separates "the feed came back" from "the feed came back because
	// everything was rebuilt".
	if set.Commentary != captureBefore {
		t.Errorf("the capture set's commentary pipeline was REPLACED across the peer loss")
	}
	if err := captureBefore.Health(); err != nil {
		t.Errorf("the capture pipeline died during the peer loss: %v", err)
	}
	if err := captureBefore.ClaimForSend(); !errors.Is(err, ErrSeamBusy) {
		t.Errorf("during the reconnect the seam claim was %v, want ErrSeamBusy — the send "+
			"pipeline released its hold on the capture layer while merely swapping a sink, "+
			"which is a window in which a second consumer could steal the stream", err)
	}

	// internal/sender's first backoff rung is 7 s precisely to clear this
	// window; waiting it out from the far end's own numbers is the same act with
	// the arithmetic done for us.
	waitForMockReady(t, control)

	if err := pipe.ReplaceSink(sink); err != nil {
		t.Fatalf("ReplaceSink (reconnect): %v", err)
	}
	if !waitForMockAccept(t, control, 10*time.Second) {
		t.Fatalf("the mock never accepted the reconnection")
	}
	_ = pipe.ForceKeyUnit()
	time.Sleep(15 * time.Second)

	after := mockState(t, control)
	if after.SRT.Analyzer == nil {
		t.Fatalf("no analyzer after the reconnect")
	}
	firstAfterSession, lastAfter, buffersAfter, backwards, detail := tl.read()
	_ = firstAfterSession

	fmt.Fprintf(os.Stderr, "  after:  %s\n", after.SRT.Analyzer.describe())
	fmt.Fprintf(os.Stderr, "  mux input timeline spanned the whole run: %v at the drop -> %v "+
		"at the end (%d -> %d buffers, one uninterrupted probe)\n",
		lastBefore.Round(time.Millisecond), lastAfter.Round(time.Millisecond),
		buffersBefore, buffersAfter)

	// 1. Commentary came back.
	if !after.SRT.Analyzer.HaveAudioPID || !after.SRT.Analyzer.AudioIsAAC {
		t.Errorf("after the reconnect the far end has no AAC audio PID (haveAudio=%v aac=%v): "+
			"commentary did not return", after.SRT.Analyzer.HaveAudioPID, after.SRT.Analyzer.AudioIsAAC)
	}
	if after.SRT.Analyzer.BytesTotal < acceptanceMinBytes/2 {
		t.Errorf("after the reconnect only %d bytes arrived in 15 s", after.SRT.Analyzer.BytesTotal)
	}

	// 2. The capture pipeline never restarted, and the send pipeline was never
	//    rebuilt — which is what makes the probe reading above one continuous
	//    measurement.
	if buffersAfter <= buffersBefore {
		t.Errorf("no new buffers crossed %s:src after the reconnect (%d -> %d)",
			nameMuxAudioQueue, buffersBefore, buffersAfter)
	}
	if err := captureBefore.Health(); err != nil {
		t.Errorf("the capture pipeline is unhealthy after the reconnect: %v", err)
	}

	// 3. DTS monotonic on the wire, and the input timeline carried on rather
	//    than restarting.
	if after.SRT.Analyzer.DTSBackwards {
		t.Errorf("DTS WENT BACKWARDS after the reconnect: %s", after.SRT.Analyzer.DTSBackwardsDetail)
	}
	if backwards {
		t.Errorf("the muxer's input timeline went backwards across the reconnect: %s", detail)
	}
	if lastAfter <= lastBefore {
		t.Errorf("the muxer's input timeline did not advance across the reconnect (%v -> %v)",
			lastBefore, lastAfter)
	}

	if err := pipe.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	stopped = true

	// The sender's own contract: a peer loss is the NETWORK's fault and must not
	// arrive as a pipeline error, because internal/sender reads any error while
	// CONNECTED as the peer going away and would burn a DRAINING/BACKOFF cycle —
	// seven seconds off air — on a fault that is not the network's.
	for _, e := range errs.since(errCountBefore) {
		t.Logf("send pipeline reported during the reconnect: %s", e)
	}
}

// ---------------------------------------------------------------------------
// 4. A device change while sending
// ---------------------------------------------------------------------------

// TestLiveAcceptanceADeviceChangeWhileSendingIsRefused proves the safety
// property PLAN.md section 10 item 17 states: a device change while sending is
// REFUSED, and not silently allowed to orphan the proxysrc.
//
// # Why this is a safety property and not a UX preference
//
// A device change means taking the commentary capture to NULL and building
// another. Doing that under a bound proxysrc is MEASURED SILENT IN EVERY
// DIRECTION: 0 buffers, no EOS, no ERROR and no WARNING on either bus, the send
// pipeline still PLAYING and SRT still connected, because proxysink returns
// GST_FLOW_OK unconditionally and no back-pressure ever crosses the seam. The
// operator would be on air, green, sending nothing.
//
// There is no refusal inside the element, so there are two in Go, and this test
// exercises BOTH against a session that is genuinely on the wire:
//
//   - CapturePipeline.Stop() refuses while a send pipeline holds the claim, so
//     the device cannot be closed underneath the proxysrc;
//   - a second Pipeline.Start() over the same capture set refuses, so the new
//     device's proxysink cannot acquire a second consumer.
//
// Both must name ErrSeamBusy, and the FEED MUST STILL BE UP AFTERWARDS — a
// refusal that damaged the running session on its way out would be no better
// than the failure it prevents.
func TestLiveAcceptanceADeviceChangeWhileSendingIsRefused(t *testing.T) {
	liveInitDarwin(t)
	control, srtPort := requireMock(t)

	slate, err := filepath.Abs(env("WSLCOMMS_LIVE_SLATE", defaultSlatePath))
	if err != nil {
		t.Fatalf("resolving the slate: %v", err)
	}
	if _, err := os.Stat(slate); err != nil {
		t.Skipf("no slate at %s: %v", slate, err)
	}
	devID := liveNativeInputID(t)
	set := liveNativeCapture(t, slate, devID)

	pipe, err := New(set)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go func() {
		for range pipe.Errors() {
		}
	}()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = pipe.Stop()
		}
	})
	if err := pipe.Start(SendOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sink := SinkOpts{Host: "127.0.0.1", Port: srtPort, LatencyMs: DefaultSRTLatencyMs}
	waitForMockReady(t, control)
	if err := pipe.ReplaceSink(sink); err != nil {
		t.Fatalf("ReplaceSink: %v", err)
	}
	if !waitForMockAccept(t, control, 5*time.Second) {
		t.Fatalf("the mock never accepted the connection")
	}
	_ = pipe.ForceKeyUnit()
	time.Sleep(6 * time.Second)

	bytesBefore := uint64(0)
	if a := mockState(t, control).SRT.Analyzer; a != nil {
		bytesBefore = a.BytesTotal
	}
	if bytesBefore == 0 {
		t.Fatalf("nothing was flowing before the device change; the refusal proves nothing")
	}

	fmt.Fprintf(os.Stderr, "\n>>>> A DEVICE CHANGE WHILE SENDING\n")

	// THE FIRST HALF: the old device cannot be closed.
	for _, c := range set.Pipelines() {
		err := c.Stop()
		if !errors.Is(err, ErrSeamBusy) {
			t.Errorf("stopping the %s capture while sending returned %v, want ErrSeamBusy. "+
				"Taking the device to NULL under a bound proxysrc is silent in every "+
				"direction: the feed would go dead with SRT connected and every lamp green",
				c.Legs(), err)
		} else {
			fmt.Fprintf(os.Stderr, "  %s capture Stop refused: %v\n", c.Legs(), err)
		}
	}

	// THE SECOND HALF: the new device cannot attach.
	second, err := New(set)
	if err != nil {
		t.Fatalf("New (the would-be replacement session): %v", err)
	}
	go func() {
		for range second.Errors() {
		}
	}()
	err = second.Start(SendOpts{})
	if !errors.Is(err, ErrSeamBusy) {
		t.Errorf("a second send pipeline over the same capture set started with %v, want "+
			"ErrSeamBusy. A second proxysrc on a live proxysink SILENTLY STEALS THE STREAM "+
			"AND KILLS THE FIRST — measured, A stopped dead at 5.994 s the instant B attached "+
			"at 6.007 s, with nothing on either bus", err)
	} else {
		fmt.Fprintf(os.Stderr, "  a second send pipeline was refused: %v\n", err)
	}
	if err := second.Stop(); err != nil {
		t.Errorf("stopping the refused session: %v", err)
	}

	// AND THE FEED IS STILL UP. A refusal that broke what it was protecting
	// would be no better than the failure it prevents.
	time.Sleep(6 * time.Second)
	after := mockState(t, control)
	if !after.SRT.Connected {
		t.Errorf("the SRT session dropped while the device change was being refused")
	}
	if after.SRT.Analyzer == nil || after.SRT.Analyzer.BytesTotal <= bytesBefore {
		t.Errorf("no new bytes reached the far end after the refusal (%d -> %v): the refusal "+
			"protected the session by killing it", bytesBefore, after.SRT.Analyzer)
	} else {
		fmt.Fprintf(os.Stderr, "  the feed survived: %d -> %d bytes, dtsBackwards=%v\n",
			bytesBefore, after.SRT.Analyzer.BytesTotal, after.SRT.Analyzer.DTSBackwards)
	}
	if after.SRT.Analyzer != nil && after.SRT.Analyzer.DTSBackwards {
		t.Errorf("DTS went backwards while the device change was refused: %s",
			after.SRT.Analyzer.DTSBackwardsDetail)
	}

	if err := pipe.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	stopped = true
}

// ---------------------------------------------------------------------------
// 5. The routing panel's reason for existing
// ---------------------------------------------------------------------------

// TestLiveAcceptanceWidthIsPublishedBeforeAnySendExists is R2's acceptance
// assertion and the routing panel's entire reason for existing.
//
// Before this work the negotiated width was a property of the SESSION's
// pipeline, so the grid could not be sized until START and the panel's own copy
// said "Press START once to size this grid". The claim now is that a width
// arrives, stamped with the device it belongs to, with NO SEND PIPELINE ANYWHERE
// IN THE PROCESS.
//
// It is asserted on two devices because one would not distinguish the claim from
// a constant: the card at 16, and a native endpoint at whatever it presents. The
// device STAMP is checked as hard as the number, because without it there is a
// window between selecting a device and the capture renegotiating in which the
// grid still holds the previous device's width — and a crosspoint pressed in
// that window writes a 2x16 matrix onto a two-channel pad, which is the measured
// `streaming stopped, reason error (-5)`.
func TestLiveAcceptanceWidthIsPublishedBeforeAnySendExists(t *testing.T) {
	liveInitDarwin(t)

	card := env("WSLCOMMS_LIVE_CARD", defaultLiveCard)
	slate, err := filepath.Abs(env("WSLCOMMS_LIVE_SLATE", defaultSlatePath))
	if err != nil {
		t.Fatalf("resolving the slate: %v", err)
	}
	nativeID := liveNativeInputID(t)

	cases := []struct {
		name string
		opts CaptureOpts
		want int
	}{{
		name: "the card, fused",
		opts: CaptureOpts{
			Legs:           CaptureLegs{Picture: PictureCard, Commentary: CommentaryCard},
			VideoCaptureID: card,
			AudioCaptureID: card,
			ConformTo:      FallbackConformTarget(),
			DeviceChannels: deckLinkAudioChannels,
			ChannelMap:     DefaultChannelMap(deckLinkAudioChannels),
		},
		// By construction, not by probe: decklinkaudiosrc is built with
		// channels=16 and the card's width is the constant.
		want: deckLinkAudioChannels,
	}, {
		name: "a native endpoint on a slate seat",
		opts: CaptureOpts{
			Legs:          CaptureLegs{Picture: PictureSlate, Commentary: CommentaryNative},
			SlatePath:     slate,
			AudioDeviceID: nativeID,
			ConformTo:     FallbackConformTarget(),
		},
		// 0 means "whatever the endpoint presents" — the number is read from
		// the callback and reported, not pinned, because pinning it here would
		// make this test a property of one machine's microphone.
		want: 0,
	}}

	fmt.Fprintf(os.Stderr, "\n>>>> A WIDTH BEFORE ANY SEND PIPELINE EXISTS\n")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			type arrival struct {
				key   string
				width int
				at    time.Duration
			}
			var mu sync.Mutex
			var arrivals []arrival

			begin := time.Now()
			opts := tc.opts
			opts.OnInputChannels = func(key string, width int) {
				mu.Lock()
				arrivals = append(arrivals, arrival{key, width, time.Since(begin)})
				mu.Unlock()
			}

			capture, err := NewCapture(opts)
			if err != nil {
				t.Fatalf("NewCapture: %v", err)
			}
			t.Cleanup(func() {
				if err := capture.Stop(); err != nil {
					t.Errorf("Stop: %v — the device may still be held", err)
				}
			})
			go func() {
				for err := range capture.Faults() {
					t.Errorf("capture fault: %v", err)
				}
			}()
			go func() {
				for range capture.Warnings() {
				}
			}()
			if err := capture.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}

			// THE WIDTH IS TAKEN FROM THE CALLBACK AND NEVER FROM A SYNCHRONOUS
			// READ. Measured: InputChannels() reads 0 for about 7 ms after Start
			// returns — Start completed at +108 ms and aconv:sink published its
			// negotiated caps at +115 ms. That is the shipped contract, not a
			// defect, and a test that polled InputChannels() immediately would
			// be encoding the race it exists to forbid.
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				mu.Lock()
				n := len(arrivals)
				mu.Unlock()
				if n > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}

			mu.Lock()
			got := append([]arrival(nil), arrivals...)
			mu.Unlock()

			if len(got) == 0 {
				t.Fatalf("no width was published within 3 s of Start, with no send pipeline " +
					"in the process. This is R2's blocker unmoved: the routing panel cannot " +
					"be sized before START")
			}

			first := got[0]
			wantKey := opts.DeviceKey()
			fmt.Fprintf(os.Stderr, "  %-36s width=%-3d key=%s at +%v (no send pipeline exists)\n",
				tc.name, first.width, first.key, first.at.Round(time.Millisecond))

			if first.key != wantKey {
				t.Errorf("the width was stamped %q, want %q. Without the right stamp the grid "+
					"holds the previous device's width and a crosspoint pressed in that window "+
					"writes a matrix onto the wrong pad", first.key, wantKey)
			}
			if first.width < 1 {
				t.Errorf("published width %d is not a usable width", first.width)
			}
			if tc.want != 0 && first.width != tc.want {
				t.Errorf("published width %d, want %d", first.width, tc.want)
			}

			// And the synchronous read agrees, once it has had the ~7 ms.
			if n := capture.InputChannels(); n != first.width {
				t.Errorf("InputChannels() reads %d but the callback published %d", n, first.width)
			}
		})
	}
}
