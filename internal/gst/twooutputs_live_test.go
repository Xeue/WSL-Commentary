//go:build sendlive && cgo && !gststub

// twooutputs_live_test.go proves the AUDIO-ONLY, TWO-OUTPUT send on the real
// GStreamer, with no card, no network and no M2L-X in it: the platform's own
// microphone, two in-process SRT listeners on the loopback, and the shipped
// pipeline between them — sendDescription, the seam, the sink slots, the gates.
//
// It exists because the first field run of an audio-only seat (1.6.2, the
// facility presets) failed at SendSeam.Bind looking for a vproxsrc the
// description had not rendered, with the pipeline parsed and never played.
// Nothing at Gate A could see that: the stub has no parse and no bind. So this
// is the test that RUNS the graph, and it also measures the property the
// operator asked for in words — "audio keeps sending even if one of the two
// outputs stalls for any reason" — two ways: an output whose pad is wedged, and
// an output whose listener has gone away.
//
// Run inside the Gate B environment (the bundled GStreamer on PATH), after a
// build has put the bundle in build\bin:
//
//	go test -tags "dev sendlive" -run TestSendLive -v -count=1 ./internal/gst/
//
// It is on its own tag rather than `live` because the live suite is
// macOS-shaped (Getrusage, the .app plugin layout) and does not compile on
// Windows, where this one has to run.
package gst

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	srt "github.com/datarhei/gosrt"
	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// TestMain leaves by TerminateProcess on Windows for the reason the application
// does (exit_windows.go): ExitProcess over the GStreamer, D3D11 and libsrt DLLs
// this process loaded deadlocks in DLL_PROCESS_DETACH — measured here as a test
// binary that had printed FAIL and then sat for ten minutes.
func TestMain(m *testing.M) {
	code := m.Run()
	sendLiveExit(code)
}

const sendLiveLatencyMs = 120

// sendLiveDir is the bundle Init was pointed at.
var sendLiveDir string

// sendLiveInit points Init at the built bundle: build\bin, where wslcomms.exe
// and its gst\ tree are. WSLCOMMS_SENDLIVE_APP_DIR overrides it.
func sendLiveInit(t *testing.T) {
	t.Helper()
	dir := os.Getenv("WSLCOMMS_SENDLIVE_APP_DIR")
	if dir == "" {
		abs, err := filepath.Abs(filepath.Join("..", "..", "build", "bin"))
		if err != nil {
			t.Fatalf("resolving build/bin: %v", err)
		}
		dir = abs
	}
	if _, err := os.Stat(bundlePluginDir(dir)); err != nil {
		t.Skipf("no bundled GStreamer plugins at %s — build the app first: %v", bundlePluginDir(dir), err)
	}
	sendLiveDir = dir
	if err := Init(dir); err != nil {
		t.Fatalf("Init(%s): %v", dir, err)
	}
	// The bundle carries only what the application uses, and audiotestsrc is
	// not among them: the tone generator comes from the installed GStreamer
	// the bundle was copied from. WSLCOMMS_SENDLIVE_TONE_PLUGIN overrides.
	plugin := os.Getenv("WSLCOMMS_SENDLIVE_TONE_PLUGIN")
	if plugin == "" {
		plugin = "C:/gstreamer/1.0/mingw_x86_64/lib/gstreamer-1.0/libgstaudiotestsrc.dll"
	}
	if os.Getenv("WSLCOMMS_LIVE_AUDIO_DEVICE") == "" {
		if _, err := gogst.PluginLoadFile(plugin); err != nil {
			t.Skipf("no audiotestsrc plugin at %s and no WSLCOMMS_LIVE_AUDIO_DEVICE: %v", plugin, err)
		}
	}
}

// sendLiveAudioDevice is the commentary source: a real endpoint when
// WSLCOMMS_LIVE_AUDIO_DEVICE names one, otherwise a 440 Hz tone through the
// captureSourceOverride hook, which opens no device and so runs on any machine.
// The id returned in the tone case is a placeholder that satisfies the plan's
// "exactly one audio source" rule; the hook makes it unused.
func sendLiveAudioDevice(t *testing.T) string {
	t.Helper()
	if id := os.Getenv("WSLCOMMS_LIVE_AUDIO_DEVICE"); id != "" {
		t.Logf("commentary input: device %s", id)
		return id
	}
	captureSourceOverride = "audiotestsrc is-live=true wave=sine freq=440 volume=0.5"
	t.Cleanup(func() { captureSourceOverride = "" })
	t.Logf("commentary input: %s (no device opened)", captureSourceOverride)
	return "{0.0.1.00000000}.{00000000-0000-0000-0000-00000000tone}"
}

// sendLiveAudioOnlyCapture builds and starts the capture layer of an
// audio-only seat: PlanCapture with NoVideo, which is the commentary leg alone.
func sendLiveAudioOnlyCapture(t *testing.T, devID string) CaptureSet {
	t.Helper()
	var set CaptureSet
	for _, legs := range PlanCapture(CaptureSources{AudioDeviceID: devID, NoVideo: true}) {
		c, err := NewCapture(CaptureOpts{
			Legs:          legs,
			AudioDeviceID: devID,
			NoVideo:       true,
			ConformTo:     FallbackConformTarget(),
		})
		if err != nil {
			t.Fatalf("NewCapture(%s): %v", legs, err)
		}
		t.Cleanup(func() {
			if err := c.Stop(); err != nil {
				t.Errorf("stopping the %s capture: %v — the device may still be held", legs, err)
			}
		})
		if err := c.Start(); err != nil {
			t.Fatalf("starting the %s capture: %v", legs, err)
		}
		if legs.Picture != PictureNone {
			set.Picture = c
		}
		if legs.Commentary != CommentaryNone {
			set.Commentary = c
		}
	}
	if set.Picture != nil {
		t.Fatal("an audio-only plan built a picture leg")
	}
	if set.Commentary == nil {
		t.Fatal("an audio-only plan built no commentary leg")
	}
	return set
}

// srtListener is one SRT listener on the loopback, standing in for one M2L-X
// input. It is pure Go (gosrt, the same library probe.exe dials with) rather
// than a GStreamer srtsrc: independent of the code under test, trivially
// counted, and closed without ceremony — an srtsrc listener at NULL was measured
// to hang the test process in libsrt's accept.
type srtListener struct {
	port int
	ln   srt.Listener
	once sync.Once

	mu      sync.Mutex
	bytes   int64
	packets int64
	conns   int
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free UDP port: %v", err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	_ = pc.Close()
	return port
}

func newSRTListener(t *testing.T, port int) *srtListener {
	t.Helper()
	cfg := srt.DefaultConfig()
	cfg.Latency = sendLiveLatencyMs * time.Millisecond
	ln, err := srt.Listen("srt", fmt.Sprintf("127.0.0.1:%d", port), cfg)
	if err != nil {
		t.Fatalf("listener on %d: %v", port, err)
	}
	l := &srtListener{port: port, ln: ln}
	go l.serve()
	t.Cleanup(l.stop)
	return l
}

func (l *srtListener) serve() {
	for {
		req, err := l.ln.Accept2()
		if err != nil {
			return // closed
		}
		conn, err := req.Accept()
		if err != nil {
			continue
		}
		l.mu.Lock()
		l.conns++
		l.mu.Unlock()
		go func() {
			defer conn.Close()
			buf := make([]byte, 65536)
			for {
				n, err := conn.Read(buf)
				if n > 0 {
					l.mu.Lock()
					l.bytes += int64(n)
					l.packets++
					l.mu.Unlock()
				}
				if err != nil {
					return
				}
			}
		}()
	}
}

// stop closes the listener and every connection it accepted: the M2L-X input
// going away.
func (l *srtListener) stop() {
	l.once.Do(func() { l.ln.Close() })
}

func (l *srtListener) received() (packets, bytes int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.packets, l.bytes
}

// errorLog drains a pipeline's Errors() for the life of the test and files each
// one by the output it names.
type errorLog struct {
	mu   sync.Mutex
	errs []error
}

func drainErrors(pipe Pipeline) *errorLog {
	e := &errorLog{}
	go func() {
		for err := range pipe.Errors() {
			e.mu.Lock()
			e.errs = append(e.errs, err)
			e.mu.Unlock()
		}
	}()
	return e
}

// byOutput splits what has arrived: errors naming output i, and errors naming
// no output at all.
func (e *errorLog) byOutput(i int) (named, unattributed []error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, err := range e.errs {
		var oe *OutputError
		if errors.As(err, &oe) {
			if oe.Output == i {
				named = append(named, err)
			}
			continue
		}
		unattributed = append(unattributed, err)
	}
	return named, unattributed
}

func (e *errorLog) all() []error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]error(nil), e.errs...)
}

// expectGrowth watches a listener for `over`, sampling every 500 ms, and fails
// if ANY window carried nothing. A single dry half-second is a stall of the
// commentary at that input, which is the thing this file is for.
func expectGrowth(t *testing.T, l *srtListener, over time.Duration, what string) {
	t.Helper()
	const window = 500 * time.Millisecond
	_, last := l.received()
	dry := 0
	for elapsed := time.Duration(0); elapsed < over; elapsed += window {
		time.Sleep(window)
		_, now := l.received()
		if now == last {
			dry++
		}
		last = now
	}
	_, total := l.received()
	if dry > 0 {
		t.Errorf("%s: %d of %d half-second windows carried NOTHING on port %d (total %d bytes)",
			what, dry, int(over/window), l.port, total)
	} else {
		t.Logf("%s: every half-second window carried audio on port %d (total %d bytes)", what, l.port, total)
	}
}

func TestSendLiveAudioOnlyTwoOutputsKeepSendingWhenOneStalls(t *testing.T) {
	sendLiveInit(t)
	dev := sendLiveAudioDevice(t)
	set := sendLiveAudioOnlyCapture(t, dev)

	p0, p1 := freeUDPPort(t), freeUDPPort(t)
	l0 := newSRTListener(t, p0)
	l1 := newSRTListener(t, p1)
	sink := func(port int) SinkOpts {
		return SinkOpts{Host: "127.0.0.1", Port: port, LatencyMs: sendLiveLatencyMs}
	}

	pipe, err := New(set)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cp, ok := pipe.(*cgoPipeline)
	if !ok {
		t.Fatalf("New returned a %T, not a *cgoPipeline", pipe)
	}
	t.Cleanup(func() { _ = pipe.Stop() })
	elog := drainErrors(pipe)

	// --- 1. The audio-only, two-output graph builds, binds and plays. -------
	t0 := time.Now()
	if err := pipe.Start(SendOpts{NoVideo: true, SecondOutput: true}); err != nil {
		t.Fatalf("Start(NoVideo, SecondOutput): %v (after %v) — this is the field failure: the "+
			"parsed graph could not be bound or did not carry media", err, time.Since(t0))
	}
	t.Logf("audio-only two-output send reached PLAYING and passed the liveness gate in %v",
		time.Since(t0).Round(time.Millisecond))
	if n := pipe.Outputs(); n != 2 {
		t.Fatalf("Outputs() = %d, want 2", n)
	}

	// --- 2. Both outputs dial their listener and carry the audio. -----------
	if err := pipe.ReplaceSinkOn(0, sink(p0)); err != nil {
		t.Fatalf("ReplaceSinkOn(0): %v", err)
	}
	if err := pipe.ReplaceSinkOn(1, sink(p1)); err != nil {
		t.Fatalf("ReplaceSinkOn(1): %v", err)
	}
	var both sync.WaitGroup
	for _, l := range []*srtListener{l0, l1} {
		both.Add(1)
		go func(l *srtListener) {
			defer both.Done()
			expectGrowth(t, l, 3*time.Second, fmt.Sprintf("output on %d, both connected", l.port))
		}(l)
	}
	both.Wait()
	if errs := elog.all(); len(errs) != 0 {
		t.Fatalf("errors while both outputs were healthy: %v", errs)
	}

	// --- 3. Output 1 WEDGES: its slot's src pad is blocked, as a sink that ---
	// stopped taking data would block it. The tee must not stall behind it,
	// and output 0 must not notice. A wedge is not an error: nothing is posted.
	wedge := cp.slots[1].srcPad.AddProbe(gateProbeMask,
		func(gogst.Pad, *gogst.PadProbeInfo) gogst.PadProbeReturn { return gogst.PadProbeOK })
	_, wedgedAt := l1.received()
	expectGrowth(t, l0, 4*time.Second, "output 0 while output 1 is wedged")
	if _, now := l1.received(); now-wedgedAt > 0 {
		t.Logf("output 1 delivered %d more bytes after the wedge (in-flight data draining)", now-wedgedAt)
	}
	cp.slots[1].srcPad.RemoveProbe(wedge)
	expectGrowth(t, l1, 3*time.Second, "output 1 after the wedge cleared")
	if errs := elog.all(); len(errs) != 0 {
		t.Fatalf("a wedged output posted errors, which would have restarted it: %v", errs)
	}

	// --- 4. Output 1 LOSES ITS PEER: the listener goes away. The failure ----
	// must name output 1 and nothing else, output 0 must keep carrying, and
	// the ordinary reconnect (remove, re-install) must bring output 1 back.
	l1.stop()
	expectGrowth(t, l0, 4*time.Second, "output 0 after output 1 lost its peer")

	deadline := time.Now().Add(20 * time.Second)
	var named []error
	for time.Now().Before(deadline) {
		var unattributed []error
		named, unattributed = elog.byOutput(1)
		if len(unattributed) != 0 {
			t.Fatalf("output 1's peer loss produced an error naming NO output, which would restart "+
				"both: %v", unattributed)
		}
		if other, _ := elog.byOutput(0); len(other) != 0 {
			t.Fatalf("output 1's peer loss produced an error naming OUTPUT 0: %v", other)
		}
		if len(named) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(named) == 0 {
		t.Errorf("no error naming output 1 arrived within 20 s of its listener going away; the "+
			"sender would not know to reconnect it (errors so far: %v)", elog.all())
	} else {
		t.Logf("output 1's peer loss was reported as its own: %v", named[0])
	}

	if err := pipe.RemoveSinkOn(1); err != nil {
		t.Errorf("RemoveSinkOn(1): %v", err)
	}
	expectGrowth(t, l0, 2*time.Second, "output 0 while output 1 is being replaced")
	l1 = newSRTListener(t, p1)
	if err := pipe.ReplaceSinkOn(1, sink(p1)); err != nil {
		t.Fatalf("ReplaceSinkOn(1) after the peer loss: %v", err)
	}
	expectGrowth(t, l1, 3*time.Second, "output 1 reconnected")
	expectGrowth(t, l0, 1*time.Second, "output 0 at the end")

	// --- 5. Clean stop, and the capture outlived the session. ----------------
	if err := pipe.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	for _, c := range set.Pipelines() {
		if err := c.Health(); err != nil {
			t.Errorf("the %s capture did not survive the session: %v", c.Legs(), err)
		}
	}
}
