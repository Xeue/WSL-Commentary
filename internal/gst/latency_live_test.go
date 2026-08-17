//go:build live && cgo && !gststub

// latency_live_test.go MEASURES the send path's audio latency, stage by stage,
// over the real always-live seam with no card and no network.
//
// It exists to answer one question a user asked with a number: the commentary
// audio reaching M2L-X about 2.5 s late, with M2L-X's own SRT input latency at
// 50 ms and the encode+mux measured at ~21 ms. That leaves ~2 s unaccounted for,
// and the only stage neither gst-launch nor the encoder measurements could reach
// is the proxysink/proxysrc seam the always-live capture work introduced.
//
// # How the number is read
//
// The capture and send pipelines SHARE ONE CLOCK AND ONE BASE TIME, exactly as
// runSeamCycle sets them, so a buffer's running time (its PTS, which proxysrc
// emits as the PRODUCER's running time — see sendProbeDescription) is directly
// comparable to (clock.now - base). Their difference is the buffer's AGE: how
// long ago the audio it carries was produced. Measured at:
//
//	aproxsrc:src   the audio entering the send pipeline from the seam — this is
//	               capture latency + seam latency, and it is the stage under test
//	aq:src         one stage further, after the AAC encoder — adds the ~21 ms the
//	               gst-launch runs already attributed to mfaacenc
//
// A healthy low-latency path reads tens of milliseconds at aproxsrc:src. Two
// seconds there is the standing backlog the seam's two 1-second queues can hold.
package gst

import (
	"sort"
	"sync"
	"testing"
	"time"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// latencyProbe records the age of every buffer that crosses a pad, in the shared
// clock the caller hands it.
type latencyProbe struct {
	clock gogst.Clock
	base  gogst.ClockTime

	mu   sync.Mutex
	ages []time.Duration
}

func (p *latencyProbe) probe(_ gogst.Pad, info *gogst.PadProbeInfo) gogst.PadProbeReturn {
	buf := info.GetBuffer()
	if buf == nil {
		return gogst.PadProbeOK
	}
	pts := buf.PTS()
	if pts == gogst.ClockTimeNone {
		return gogst.PadProbeOK
	}
	now := p.clock.GetTime()
	age := time.Duration(uint64(now)-uint64(p.base)) - time.Duration(uint64(pts))
	p.mu.Lock()
	p.ages = append(p.ages, age)
	p.mu.Unlock()
	return gogst.PadProbeOK
}

// summary returns the count, median and max age, ignoring the first few samples
// so a preroll transient does not stand in for steady state.
func (p *latencyProbe) summary() (n int, median, max time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ages := append([]time.Duration(nil), p.ages...)
	if len(ages) > 10 {
		ages = ages[5:] // drop the first five: startup, not steady state
	}
	n = len(ages)
	if n == 0 {
		return 0, 0, 0
	}
	sort.Slice(ages, func(i, j int) bool { return ages[i] < ages[j] })
	median = ages[n/2]
	max = ages[n-1]
	return n, median, max
}

func addLatencyProbe(t *testing.T, send gogst.Pipeline, element string, p *latencyProbe) {
	t.Helper()
	el := mustGetElement(t, send, element)
	pad := el.GetStaticPad("src")
	if pad == nil {
		t.Fatalf("%s has no src pad", element)
	}
	pad.AddProbe(gogst.PadProbeTypeBuffer, p.probe)
}

// TestLiveSendPathAudioLatency builds the real seam and reports where the audio
// latency lives. It asserts nothing beyond "the pipeline ran": the numbers are
// the output, read from the test log with -v.
func TestLiveSendPathAudioLatency(t *testing.T) {
	liveInitDarwin(t)

	encoderName, err := selectH264Encoder()
	if err != nil {
		t.Fatalf("selectH264Encoder: %v", err)
	}
	t.Logf("H.264 encoder: %s; AAC encoder: %s", encoderName, aacEncoderFactory)

	// -------------------------------------------------------------- capture
	element, err := gogst.ParseLaunch(captureProbeDescription)
	if err != nil {
		t.Fatalf("gst_parse_launch on the capture description failed: %v", err)
	}
	capture := element.(gogst.Pipeline)
	t.Cleanup(func() { capture.BlockSetState(gogst.StateNull, gogst.ClockTime(10*time.Second)) })

	captureBus := &busRecorder{}
	capture.GetBus().SetSyncHandler(captureBus.handler)

	clock := gogst.SystemClockObtain()
	capture.UseClock(clock)
	capture.SetStartTime(gogst.ClockTimeNone)
	base := clock.GetTime()
	capture.SetBaseTime(base)

	if ret := capture.BlockSetState(gogst.StatePlaying, gogst.ClockTime(10*time.Second)); !stateChangeOK(ret) {
		t.Fatalf("the capture pipeline would not go to PLAYING (%s)", ret)
	}

	// Let the always-live capture run for a few seconds BEFORE the send pipeline
	// starts, which is the real situation: the operator sets levels, watches the
	// meters, then presses START. This is also when a standing backlog in the
	// leaky queue in front of the un-consumed proxysink would build.
	time.Sleep(3 * time.Second)

	// ------------------------------------------------------------- the send
	aq := mustGetElement(t, capture, probeAudioProxyQueue)
	as := mustGetElement(t, capture, probeAudioProxySink)
	vq := mustGetElement(t, capture, probeVideoProxyQueue)
	vs := mustGetElement(t, capture, probeVideoProxySink)
	armProxySinkLive(t, aq, as)
	armProxySinkLive(t, vq, vs)

	sendEl, err := gogst.ParseLaunch(sendProbeDescription(encoderName, DefaultAudioBitrateBps))
	if err != nil {
		t.Fatalf("gst_parse_launch on the send description failed: %v", err)
	}
	send := sendEl.(gogst.Pipeline)
	t.Cleanup(func() { send.BlockSetState(gogst.StateNull, gogst.ClockTime(10*time.Second)) })

	if err := setStringProperty(mustGetElement(t, send, probeFileSink), "location", t.TempDir()+"/lat.ts"); err != nil {
		t.Fatalf("filesink location: %v", err)
	}
	applyEncoderProperties(mustGetElement(t, send, nameVideoEncod), encoderName, DefaultVideoBitrateKbps)

	bindProxySrcLive(t, mustGetElement(t, send, probeVideoProxySrc), vs)
	bindProxySrcLive(t, mustGetElement(t, send, probeAudioProxySrc), as)

	seam := &latencyProbe{clock: clock, base: base}
	postEnc := &latencyProbe{clock: clock, base: base}
	addLatencyProbe(t, send, probeAudioProxySrc, seam)
	addLatencyProbe(t, send, nameMuxAudioQueue, postEnc)

	send.UseClock(clock)
	send.SetStartTime(gogst.ClockTimeNone)
	send.SetBaseTime(base)

	if ret := send.BlockSetState(gogst.StatePlaying, gogst.ClockTime(10*time.Second)); !stateChangeOK(ret) {
		t.Fatalf("the send pipeline would not go to PLAYING (%s)", ret)
	}

	// Hold long enough that a standing backlog is unambiguous.
	time.Sleep(10 * time.Second)

	send.BlockSetState(gogst.StateNull, gogst.ClockTime(10*time.Second))

	n1, med1, max1 := seam.summary()
	n2, med2, max2 := postEnc.summary()
	t.Logf("=== SEND PATH AUDIO LATENCY (buffer age in the shared clock) ===")
	t.Logf("  aproxsrc:src  (capture + seam)        n=%d  median=%v  max=%v",
		n1, med1.Round(time.Millisecond), max1.Round(time.Millisecond))
	t.Logf("  aq:src        (+ AAC encoder)         n=%d  median=%v  max=%v",
		n2, med2.Round(time.Millisecond), max2.Round(time.Millisecond))
	t.Logf("  (SRT sender latency adds ~%dms at srtsink; M2L-X input latency adds its own on top)",
		DefaultSRTLatencyMs)
}
