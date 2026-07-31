//go:build live && cgo && !gststub

// live_test.go is the Gate C verification of BUILD-NOTES.md section 4.10 — the
// sticky events that rearmQueueLocked destroys and puts back by hand.
//
// It drives THIS package's real implementation. gst-launch cannot exercise
// rearmQueueLocked because the re-arm lives in our code, not in a launch line,
// so the only way to settle section 4.10 is to run the Pipeline itself against
// a real SRT listener and look at what the far end says afterwards.
//
// It is behind the `live` build tag and never runs in a normal suite. See
// BUILD-NOTES.md section 8.2 for how to run it and what to grep the log for.
//
// # What it proves, and how
//
// The decisive assertion is in-process rather than in the log:
// stickyEventsOf(srtq:src) is snapshotted immediately BEFORE RemoveSink and
// again immediately AFTER it, and the two are compared by GstEvent SEQNUM.
// rearmQueueLocked restores the very same GstEvent objects it snapshotted, so
// equal seqnums prove the restore ran and put the originals back — not that
// something else happened to re-push a fresh SEGMENT that would have been
// equally green and completely wrong.
//
// The log is the confirming evidence, not the primary one:
//
//	"Received buffer without a new-segment. Assuming timestamps start from 0"
//
// from srtout-N after any reconnect means the restore did not work. See
// BUILD-NOTES.md section 4.10 for the fallback.
//
// # Firing rearmQueueLocked at all
//
// rearmQueueLocked runs only when srtq:src's last flow return is not
// GST_FLOW_OK, so the test has to make that happen.
//
// The provocation is a GENUINE one: provokeGenuine kills a real SRT listener
// mid-stream. srtsink's render() fails, GST_ELEMENT_ERROR is posted, and the
// queue's loop takes a real GST_FLOW_ERROR — the exact production trigger, the
// one section 4.10 says every mid-match reconnect takes.
//
// A synthetic poison was tried and abandoned, and the reason belongs here
// because it is a property of the design rather than of the test. Unlinking
// srtq:src, or lifting its gate probe, while the gate is open does poison the
// queue — and about 4.6 ms later mpegtsmux's next push takes that same
// NOT_LINKED out of gst_queue_chain, GstAggregator answers it with
// GST_ELEMENT_FLOW_ERROR naming `mux`, and the run is over: pipeline-fatal,
// capture chain down. Go cannot close the gate inside that window. Only
// srtsink's own error can, because onBusMessage runs synchronously on the
// streaming thread that is still inside render(), BEFORE the queue's srcresult
// is set. That is BUILD-NOTES section 4.2's timing dependency seen from the
// other side, and this test is the measurement that it is load-bearing.
//
// provokeUnlinked survives as a fallback for a machine with no spare port. It
// runs only with the gate ALREADY CLOSED and the sink already removed, so the
// SINK-side probe is dropping and mpegtsmux is never exposed. It is not
// reliable — with the gate shut the queue is being starved, so its loop may
// have nothing left to push — and it reports honestly when it achieves
// nothing.
package gst

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// Defaults are the M2L-X dev instance and the hardware this was verified on.
// Every one is overridable by environment variable; the password has no
// default on purpose, so that no credential lives in the repository.
const (
	defaultM2LXHost     = "m2lx-wslstudios-matcht.etapsiota.com"
	defaultM2LXEvent    = "dl9-5p5ah0bd-empd"
	defaultM2LXAlias    = "matcht"
	defaultRouterInput  = 22
	defaultSRTPort      = 40022
	defaultAudioDevice  = "{0.0.1.00000000}.{d6ca5cf3-b02c-47dc-b237-0ab704e62a5e}"
	defaultAppDir       = "../../build/bin"
	defaultSlatePath    = "../../assets/slate.png"
	defaultKillPeerPort = 9001

	// cmd/mockm2lx, started by hand before the peer-loss test. Its
	// POST /control/drop-srt is a deliberate peer loss at an instant the test
	// chooses, with the listener still alive afterwards, which a killed
	// gst-launch listener cannot offer.
	defaultMockControl = "http://127.0.0.1:8099"
	defaultMockSRTPort = 9002

	// backoffRung is specification section 6.2's first backoff rung. It must
	// stay >= 6 s: an M2L-X SRT listener accepts exactly one peer and refuses
	// re-accept for roughly five seconds after the incumbent goes away.
	backoffRung = 7 * time.Second

	// settleTime is how long each connection is left running before the next
	// provocation, so that there is real media on the wire to lose.
	settleTime = 10 * time.Second

	// statusTimeout bounds how long M2L-X may take to report a new peer as
	// locked. The specification measures a successful lock at about 1.1 s.
	statusTimeout = 25 * time.Second
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// mark writes a marker straight to stderr as well as to the test log. The
// stderr copy is the point: it interleaves with GST_DEBUG output in the
// captured file, so "after the reconnect" is answerable by reading the log
// top to bottom instead of by inference.
func mark(t *testing.T, format string, args ...any) {
	t.Helper()
	line := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "\n>>>> LIVE-TEST %s %s\n", time.Now().Format("15:04:05.000"), line)
	t.Log(line)
}

// ---------------------------------------------------------------------------
// M2L-X control plane
// ---------------------------------------------------------------------------

// routerInput is one record from GET /api/input/router/list/{event}. Raw
// carries the whole object so that a field this struct does not name — a new
// error counter, say — still reaches the report.
type routerInput struct {
	ID              int    `json:"id"`
	Status          string `json:"status"`
	StatusMessageID string `json:"status_message_id"`
	Name            string `json:"name"`
	Codec           string `json:"codec"`
	Width           int    `json:"width"`
	Height          int    `json:"height"`
	FrameRate       int    `json:"frame_rate"`
	AudioCodec      string `json:"audio_codec"`
	AudioFreq       int    `json:"audio_freq"`
	Port            int    `json:"port"`

	Raw string `json:"-"`
}

// format renders the fields step 4 of the brief asks to be held constant.
func (r routerInput) format() string {
	return fmt.Sprintf("%s %dx%d@%d / %s/%d",
		r.Codec, r.Width, r.Height, r.FrameRate, r.AudioCodec, r.AudioFreq)
}

type m2lxClient struct {
	base  string
	event string
	token string
	http  *http.Client
}

func newM2LXClient(t *testing.T) *m2lxClient {
	t.Helper()

	host := env("WSLCOMMS_LIVE_M2LX_HOST", defaultM2LXHost)
	password := os.Getenv("WSLCOMMS_LIVE_M2LX_PASSWORD")
	if password == "" {
		t.Skip("WSLCOMMS_LIVE_M2LX_PASSWORD is not set; " +
			"this test cannot verify the far end without it (see BUILD-NOTES.md section 8.2)")
	}

	c := &m2lxClient{
		base:  "https://" + host,
		event: env("WSLCOMMS_LIVE_M2LX_EVENT", defaultM2LXEvent),
		http:  &http.Client{Timeout: 20 * time.Second},
	}

	body := fmt.Sprintf(`{"alias":%q,"password":%q}`,
		env("WSLCOMMS_LIVE_M2LX_ALIAS", defaultM2LXAlias), password)
	req, err := http.NewRequest(http.MethodPost, c.base+"/api/local_auth/signin", strings.NewReader(body))
	if err != nil {
		t.Fatalf("building the signin request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("M2L-X signin: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// The body is not logged: it would carry the refresh token.
		t.Fatalf("M2L-X signin returned HTTP %d", resp.StatusCode)
	}
	var signin struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &signin); err != nil {
		t.Fatalf("decoding the signin response: %v", err)
	}
	if signin.AccessToken == "" {
		t.Fatal("M2L-X signin returned no access_token")
	}
	c.token = signin.AccessToken
	return c
}

// input reads one router input. It never fails the test: a control-plane
// hiccup mid-reconnect should be reported, not mistaken for a media fault.
func (c *m2lxClient) input(id int) (routerInput, error) {
	req, err := http.NewRequest(http.MethodGet, c.base+"/api/input/router/list/"+c.event, nil)
	if err != nil {
		return routerInput{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return routerInput{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return routerInput{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return routerInput{}, fmt.Errorf("router list returned HTTP %d", resp.StatusCode)
	}

	var objs []json.RawMessage
	if err := json.Unmarshal(raw, &objs); err != nil {
		return routerInput{}, fmt.Errorf("decoding the router list: %w", err)
	}
	for _, obj := range objs {
		var in routerInput
		if err := json.Unmarshal(obj, &in); err != nil {
			continue
		}
		if in.ID == id {
			in.Raw = string(obj)
			return in, nil
		}
	}
	return routerInput{}, fmt.Errorf("router input %d is not in the list", id)
}

// waitForStatus polls until the input reports want, or the timeout expires. It
// returns the last reading either way.
func (c *m2lxClient) waitForStatus(t *testing.T, id int, want string, timeout time.Duration) (routerInput, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last routerInput
	for {
		in, err := c.input(id)
		if err == nil {
			last = in
			if in.Status == want {
				return in, true
			}
		} else {
			t.Logf("reading router input %d: %v", id, err)
		}
		if time.Now().After(deadline) {
			return last, false
		}
		time.Sleep(time.Second)
	}
}

// waitForLock waits for the input to be online AND to have finished detecting
// the stream, rather than merely accepting the peer.
//
// The two are not the same event and the gap is measurable: M2L-X reports
// status "online" as soon as it has a locked SRT session, and fills
// audio_codec in a second or so afterwards. Asserting the format on the first
// online reading is a race, and it fails intermittently with audio_codec "".
func (c *m2lxClient) waitForLock(t *testing.T, id int, timeout time.Duration) (routerInput, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last routerInput
	for {
		in, err := c.input(id)
		if err == nil {
			last = in
			if in.Status == "online" && in.Codec != "" && in.AudioCodec != "" {
				return in, true
			}
		}
		if time.Now().After(deadline) {
			return last, last.Status == "online"
		}
		time.Sleep(time.Second)
	}
}

// waitForNotStatus is the mirror image, used to confirm the far end really did
// see us go away between cycles.
func (c *m2lxClient) waitForNotStatus(t *testing.T, id int, unwanted string, timeout time.Duration) (routerInput, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last routerInput
	for {
		in, err := c.input(id)
		if err == nil {
			last = in
			if in.Status != unwanted {
				return in, true
			}
		}
		if time.Now().After(deadline) {
			return last, false
		}
		time.Sleep(time.Second)
	}
}

// ---------------------------------------------------------------------------
// Sticky event digest — the primary evidence
// ---------------------------------------------------------------------------

// stickyInfo is what this test compares across a re-arm.
//
// seqnum is the load-bearing field. gst_pad_store_sticky_event stores the
// GstEvent it is handed, and rearmQueueLocked hands it the very objects it
// snapshotted, so an unchanged seqnum means the original event is back on the
// pad. A CHANGED seqnum with the event still present would mean something else
// re-pushed a fresh one — which for SEGMENT is precisely the silent failure
// this test exists to detect.
type stickyInfo struct {
	seqnum uint32
	detail string
}

func stickyDigestOf(pad gogst.Pad) map[stickyKey]stickyInfo {
	out := make(map[stickyKey]stickyInfo)
	for key, ev := range stickyEventsOf(pad) {
		info := stickyInfo{seqnum: ev.event.GetSeqnum()}
		switch key.typ {
		case gogst.EventStreamStart:
			info.detail = "stream-id=" + ev.event.ParseStreamStart()
		case gogst.EventCaps:
			if caps := ev.event.ParseCaps(); caps != nil {
				info.detail = caps.String()
			}
		case gogst.EventSegment:
			if seg := ev.event.ParseSegment(); seg != nil {
				// A fixed position mapped through the segment. If the segment
				// were replaced by a fresh one with a different base or start,
				// this changes with it.
				info.detail = fmt.Sprintf("running-time(10s)=%d",
					seg.ToRunningTime(gogst.FormatTime, uint64(10*time.Second)))
			}
		}
		out[key] = info
	}
	return out
}

func describeDigest(d map[stickyKey]stickyInfo) string {
	keys := make([]stickyKey, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].typ != keys[j].typ {
			return keys[i].typ < keys[j].typ
		}
		return keys[i].idx < keys[j].idx
	})
	var b strings.Builder
	for _, k := range keys {
		info := d[k]
		fmt.Fprintf(&b, "\n    %-18s idx=%d seqnum=%d %s", k.typ, k.idx, info.seqnum, info.detail)
	}
	if b.Len() == 0 {
		return " (NONE)"
	}
	return b.String()
}

// mandatorySticky are the three whose loss is the fault. STREAM_COLLECTION and
// TAG are carried by rearmQueueLocked but mpegtsmux may legitimately never have
// sent them, so their absence is not an error.
var mandatorySticky = []gogst.EventType{
	gogst.EventStreamStart,
	gogst.EventCaps,
	gogst.EventSegment,
}

// ---------------------------------------------------------------------------
// Asynchronous error recorder
// ---------------------------------------------------------------------------

// errRecorder drains Errors(). The contract in gst.go requires a consumer:
// implementations drop rather than block, and a full channel would hide the
// very message this test waits for.
type errRecorder struct {
	mu   sync.Mutex
	seen []string
}

func newErrRecorder(p Pipeline) *errRecorder {
	r := &errRecorder{}
	go func() {
		for err := range p.Errors() {
			r.mu.Lock()
			r.seen = append(r.seen, err.Error())
			r.mu.Unlock()
			fmt.Fprintf(os.Stderr, "\n>>>> LIVE-TEST %s pipeline error: %v\n",
				time.Now().Format("15:04:05.000"), err)
		}
	}()
	return r
}

func (r *errRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

func (r *errRecorder) since(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n >= len(r.seen) {
		return nil
	}
	out := make([]string, len(r.seen)-n)
	copy(out, r.seen[n:])
	return out
}

// waitForNew blocks until at least one error has arrived after index n.
func (r *errRecorder) waitForNew(n int, timeout time.Duration) []string {
	deadline := time.Now().Add(timeout)
	for {
		if got := r.since(n); len(got) > 0 {
			return got
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// A killable real SRT listener
// ---------------------------------------------------------------------------

// killablePeer is a gst-launch-1.0 srtsrc in listener mode, run as a separate
// process so that killing it is a genuine mid-stream peer loss — the thing
// section 4.10 says to reproduce.
type killablePeer struct {
	cmd  *exec.Cmd
	port int
	log  *os.File
}

// startKillablePeer launches the listener with GStreamer's plugin path
// variables STRIPPED. Init sets those on this process to isolate the bundle,
// and a child that inherited them would look for its plugins in our bundle and
// keep its own registry there.
func startKillablePeer(t *testing.T, port int) *killablePeer {
	t.Helper()

	logPath := filepath.Join(t.TempDir(), fmt.Sprintf("killable-peer-%d.log", port))
	lf, err := os.Create(logPath)
	if err != nil {
		t.Logf("could not create the killable peer log: %v", err)
		return nil
	}

	cmd := exec.Command("gst-launch-1.0", "-q",
		"srtsrc", "uri=srt://127.0.0.1:"+strconv.Itoa(port), "mode=listener",
		"!", "fakesink", "sync=false")
	cmd.Stdout = lf
	cmd.Stderr = lf

	var clean []string
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		switch name := strings.ToUpper(kv[:eq]); {
		case strings.HasPrefix(name, "GST_PLUGIN"),
			strings.HasPrefix(name, "GST_REGISTRY"),
			name == "GST_DEBUG",
			name == "GST_DEBUG_FILE":
			continue
		}
		clean = append(clean, kv)
	}
	cmd.Env = clean

	if err := cmd.Start(); err != nil {
		t.Logf("could not start the killable SRT listener: %v", err)
		lf.Close()
		return nil
	}
	p := &killablePeer{cmd: cmd, port: port, log: lf}
	t.Cleanup(p.kill)

	// srtsrc binds and starts accepting inside its READY->PAUSED transition.
	time.Sleep(3 * time.Second)
	return p
}

func (p *killablePeer) kill() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	_, _ = p.cmd.Process.Wait()
	p.cmd = nil
	if p.log != nil {
		p.log.Close()
		p.log = nil
	}
}

// ---------------------------------------------------------------------------
// The test
// ---------------------------------------------------------------------------

// cycle describes one reconnect.
type cycle struct {
	name string

	// provoke runs while the pipeline is connected and must leave srtq:src's
	// last flow return in whatever state it is testing. It reports whether it
	// believes it succeeded.
	provoke func(t *testing.T, h *harness) bool

	// mustRearm is true when the provocation guarantees a non-OK flow return,
	// and therefore guarantees that rearmQueueLocked ran.
	mustRearm bool

	// connectFirst, when non-empty, is dialled BEFORE the provocation instead
	// of staying on M2L-X. Used by the genuine peer-loss cycle.
	connectFirst *SinkOpts
}

type harness struct {
	p    *cgoPipeline
	m2lx *m2lxClient
	errs *errRecorder

	inputID  int
	sinkOpts SinkOpts

	peer     *killablePeer
	peerPort int

	// rearmsObserved counts the cycles whose last flow return was not
	// GST_FLOW_OK — i.e. the cycles that actually took rearmQueueLocked.
	rearmsObserved int
}

func TestLiveReconnectPreservesStickyEvents(t *testing.T) {
	appDir, err := filepath.Abs(env("WSLCOMMS_LIVE_APP_DIR", defaultAppDir))
	if err != nil {
		t.Fatalf("resolving the app directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "gst", "lib", "gstreamer-1.0")); err != nil {
		t.Skipf("no bundled GStreamer under %s (run build\\bundle-gst.ps1): %v", appDir, err)
	}
	slate, err := filepath.Abs(env("WSLCOMMS_LIVE_SLATE", defaultSlatePath))
	if err != nil {
		t.Fatalf("resolving the slate: %v", err)
	}
	if _, err := os.Stat(slate); err != nil {
		t.Skipf("no slate at %s: %v", slate, err)
	}

	h := &harness{
		inputID:  envInt("WSLCOMMS_LIVE_INPUT_ID", defaultRouterInput),
		peerPort: envInt("WSLCOMMS_LIVE_KILLABLE_PORT", defaultKillPeerPort),
		sinkOpts: SinkOpts{
			Host:      env("WSLCOMMS_LIVE_SRT_HOST", env("WSLCOMMS_LIVE_M2LX_HOST", defaultM2LXHost)),
			Port:      envInt("WSLCOMMS_LIVE_SRT_PORT", defaultSRTPort),
			LatencyMs: DefaultSRTLatencyMs,
		},
	}
	h.m2lx = newM2LXClient(t)

	// 1. Init, New, Start.
	mark(t, "Init(%s)", appDir)
	if err := Init(appDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	devID := env("WSLCOMMS_LIVE_AUDIO_DEVICE", defaultAudioDevice)
	devices, err := ListInputDevices()
	if err != nil {
		t.Fatalf("ListInputDevices: %v", err)
	}
	found := false
	for _, d := range devices {
		t.Logf("audio device: %q  %s", d.Name, d.ID)
		if d.ID == devID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the requested capture device %s is not present; %d devices were enumerated", devID, len(devices))
	}

	pipe, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cp, ok := pipe.(*cgoPipeline)
	if !ok {
		t.Fatalf("New returned a %T, not a *cgoPipeline", pipe)
	}
	h.p = cp
	h.errs = newErrRecorder(pipe)

	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = pipe.Stop()
		}
	})

	mark(t, "Start(slate=%s device=%s)", slate, devID)
	if err := pipe.Start(PipelineOpts{SlatePath: slate, AudioDeviceID: devID}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 2. First connect, and the far end's opinion of it.
	mark(t, "ReplaceSink -> %s:%d (first connect)", h.sinkOpts.Host, h.sinkOpts.Port)
	if err := pipe.ReplaceSink(h.sinkOpts); err != nil {
		t.Fatalf("ReplaceSink: %v", err)
	}
	if err := pipe.ForceKeyUnit(); err != nil {
		t.Errorf("ForceKeyUnit after the first connect: %v", err)
	}

	baseline, online := h.m2lx.waitForLock(t, h.inputID, statusTimeout)
	if !online {
		t.Fatalf("M2L-X input %d did not reach online after the first connect; last reading: %s",
			h.inputID, baseline.Raw)
	}
	mark(t, "M2L-X input %d online: %s", h.inputID, baseline.format())
	t.Logf("baseline record: %s", baseline.Raw)

	// 3-5. Several reconnect cycles, not one.
	//
	// Three of the four are genuine peer losses. That is not belt and braces:
	// it is the ONLY safe way to poison the queue, and the reason is BUILD-NOTES
	// section 4.2's timing. A synthetic poison — unlinking srtq:src, or lifting
	// its gate probe, while the gate is open — leaves mpegtsmux's very next
	// push (about 4.6 ms later, at 1316-byte buffers and 2.3 Mbit/s) taking
	// NOT_LINKED out of gst_queue_chain, and GstAggregator answers that with
	// GST_ELEMENT_FLOW_ERROR naming `mux`, which is pipeline-fatal. No amount of
	// care from Go wins a 4.6 ms race. Only srtsink's own error closes the gate
	// early enough, because onBusMessage runs synchronously on the streaming
	// thread that is inside render(), BEFORE the queue's srcresult is set.
	local := SinkOpts{Host: "127.0.0.1", Port: h.peerPort, LatencyMs: DefaultSRTLatencyMs}
	genuine := func(n int) cycle {
		return cycle{
			name:         fmt.Sprintf("genuine peer loss #%d (a real SRT listener killed mid-stream)", n),
			provoke:      provokeGenuine,
			connectFirst: &local,
		}
	}
	cycles := []cycle{
		{
			name:    "orderly (RemoveSink/ReplaceSink, no provocation)",
			provoke: func(*testing.T, *harness) bool { return true },
		},
		genuine(1),
		genuine(2),
		genuine(3),
	}
	if h.peerPort <= 0 {
		// No killable listener available. Fall back to the synthetic poison,
		// which is safe ONLY because it runs with the gate already closed and
		// the sink already gone; see provokeUnlinked.
		cycles = []cycle{
			{name: "orderly", provoke: func(*testing.T, *harness) bool { return true }},
			{name: "synthetic NOT_LINKED poison", provoke: provokeUnlinked},
			{name: "synthetic NOT_LINKED poison (second)", provoke: provokeUnlinked},
		}
	}

	for i, c := range cycles {
		runCycle(t, h, i+1, len(cycles), c, baseline)
	}

	mark(t, "%d of the cycles took a non-OK flow return and therefore ran rearmQueueLocked",
		h.rearmsObserved)

	// Leave the instance clean.
	mark(t, "teardown: RemoveSink, Stop")
	if err := pipe.RemoveSink(); err != nil {
		t.Errorf("final RemoveSink: %v", err)
	}
	if err := pipe.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	stopped = true
	h.peer.kill()

	if in, gone := h.m2lx.waitForNotStatus(t, h.inputID, "online", 30*time.Second); !gone {
		t.Errorf("M2L-X input %d is STILL online after Stop; something is holding the listener: %s",
			h.inputID, in.Raw)
	} else {
		mark(t, "M2L-X input %d final status %q — the listener is free", h.inputID, in.Status)
	}
}

func runCycle(t *testing.T, h *harness, n, total int, c cycle, baseline routerInput) {
	mark(t, "=========== CYCLE %d/%d: %s ===========", n, total, c.name)

	// Optionally move to a different endpoint first (the genuine cycle needs a
	// listener it is allowed to kill).
	if c.connectFirst != nil {
		h.peer = startKillablePeer(t, h.peerPort)
		if h.peer == nil {
			t.Errorf("cycle %d: could not start the killable SRT listener; "+
				"this cycle cannot exercise a genuine peer loss", n)
			return
		}
		mark(t, "cycle %d: ReplaceSink -> %s:%d (the killable peer)", n, c.connectFirst.Host, c.connectFirst.Port)
		if err := h.p.ReplaceSink(*c.connectFirst); err != nil {
			t.Errorf("cycle %d: ReplaceSink to the killable peer: %v", n, err)
			h.peer.kill()
			h.peer = nil
			return
		}
	}
	time.Sleep(settleTime)

	if !c.provoke(t, h) {
		t.Logf("cycle %d: the provocation did not achieve what it wanted; continuing", n)
	}

	// Everything below is the measurement.
	lastFlow := h.p.srtqSrcPad.GetLastFlowReturn()
	before := stickyDigestOf(h.p.srtqSrcPad)
	mark(t, "cycle %d: srtq:src last flow return is %s; sticky events BEFORE RemoveSink:%s",
		n, lastFlow, describeDigest(before))

	rearmExpected := lastFlow != gogst.FlowOK
	if rearmExpected {
		h.rearmsObserved++
	}
	if c.mustRearm && !rearmExpected {
		t.Errorf("cycle %d: the provocation was supposed to guarantee a non-OK flow return, "+
			"but srtq:src reports %s — rearmQueueLocked will NOT run and this cycle proves nothing",
			n, lastFlow)
	}

	// Every mandatory sticky event must be there BEFORE the re-arm, or the
	// comparison afterwards is meaningless.
	for _, typ := range mandatorySticky {
		if _, ok := before[stickyKey{typ: typ, idx: 0}]; !ok {
			t.Errorf("cycle %d: srtq:src has no sticky %s event even BEFORE the re-arm; "+
				"the pipeline is not in the state this test assumes", n, typ)
		}
	}

	mark(t, "cycle %d: RemoveSink (rearmQueueLocked %s run)", n,
		map[bool]string{true: "WILL", false: "will not"}[rearmExpected])
	if err := h.p.RemoveSink(); err != nil {
		t.Errorf("cycle %d: RemoveSink: %v", n, err)
		return
	}

	after := stickyDigestOf(h.p.srtqSrcPad)
	mark(t, "cycle %d: sticky events AFTER RemoveSink:%s", n, describeDigest(after))

	for _, typ := range mandatorySticky {
		key := stickyKey{typ: typ, idx: 0}
		wasThere, had := before[key]
		nowThere, have := after[key]
		switch {
		case !have && had && rearmExpected:
			t.Errorf("SHIP-BLOCKER cycle %d: the sticky %s event was DESTROYED by the re-arm and "+
				"not restored. BUILD-NOTES.md section 4.10's repair does not work on this "+
				"GStreamer; the next sink will be told timestamps start from zero.", n, typ)
		case !have && had:
			t.Errorf("cycle %d: the sticky %s event vanished from srtq:src without a re-arm "+
				"(last flow return was %s)", n, typ, lastFlow)
		case have && had && nowThere.seqnum != wasThere.seqnum:
			t.Errorf("SHIP-BLOCKER cycle %d: the sticky %s event on srtq:src has a DIFFERENT "+
				"seqnum after the re-arm (%d -> %d). The original event was not restored; "+
				"something else supplied a fresh one, which for SEGMENT means a new time base. "+
				"before=%q after=%q",
				n, typ, wasThere.seqnum, nowThere.seqnum, wasThere.detail, nowThere.detail)
		case have && had && nowThere.detail != wasThere.detail:
			t.Errorf("SHIP-BLOCKER cycle %d: the sticky %s event on srtq:src has the same seqnum "+
				"but different content after the re-arm: %q -> %q",
				n, typ, wasThere.detail, nowThere.detail)
		}
	}

	// The far end should notice we have gone. Not fatal: M2L-X's status is
	// polled, and a short cycle can outrun its own bookkeeping.
	if in, gone := h.m2lx.waitForNotStatus(t, h.inputID, "online", 15*time.Second); gone {
		t.Logf("cycle %d: M2L-X input %d went %q after RemoveSink", n, h.inputID, in.Status)
	} else {
		t.Logf("cycle %d: M2L-X input %d still reads online %s after RemoveSink (polling lag)",
			n, h.inputID, "")
	}

	// BACKOFF, then CONNECTING — specification section 6.2, in that order.
	mark(t, "cycle %d: backoff %s", n, backoffRung)
	time.Sleep(backoffRung)

	mark(t, "cycle %d: ReplaceSink -> %s:%d (reconnect)", n, h.sinkOpts.Host, h.sinkOpts.Port)
	if err := h.p.ReplaceSink(h.sinkOpts); err != nil {
		t.Errorf("cycle %d: ReplaceSink after the re-arm: %v", n, err)
		return
	}
	if err := h.p.ForceKeyUnit(); err != nil {
		t.Errorf("cycle %d: ForceKeyUnit: %v", n, err)
	}

	in, online := h.m2lx.waitForLock(t, h.inputID, statusTimeout)
	if !online {
		t.Errorf("cycle %d: M2L-X input %d did not return to online after the reconnect; "+
			"last reading: %s", n, h.inputID, in.Raw)
		return
	}
	mark(t, "cycle %d: M2L-X input %d online: %s", n, h.inputID, in.format())
	t.Logf("cycle %d record: %s", n, in.Raw)

	if got, want := in.format(), baseline.format(); got != want {
		t.Errorf("cycle %d: M2L-X reports a DIFFERENT format after the reconnect: %q, was %q",
			n, got, want)
	}
	if in.Codec != "h264" || in.Width != 1920 || in.Height != 1080 || in.FrameRate != 50 {
		t.Errorf("cycle %d: M2L-X video is %s, want h264 1920x1080@50", n, in.format())
	}
	if !strings.Contains(in.AudioCodec, "aac") || in.AudioFreq != 48000 {
		t.Errorf("cycle %d: M2L-X audio is %s/%d, want aac/48000", n, in.AudioCodec, in.AudioFreq)
	}
	if in.StatusMessageID != "" {
		t.Errorf("cycle %d: M2L-X reports status_message_id %q on input %d",
			n, in.StatusMessageID, h.inputID)
	}
}

// provokeUnlinked leaves srtq:src's last flow return at GST_FLOW_NOT_LINKED,
// deterministically and without letting the failure reach mpegtsmux.
//
// It performs the DRAINING step itself (RemoveSink), which closes the gate and
// removes the sink, and only then lifts the SRC probe. With the gate closed and
// the SINK probe still installed, mpegtsmux's pushes are dropped and answered
// GST_FLOW_OK, so the muxer never sees the queue's poisoned srcresult. The
// caller's own RemoveSink is what then runs rearmQueueLocked.
func provokeUnlinked(t *testing.T, h *harness) bool {
	t.Helper()

	if err := h.p.RemoveSink(); err != nil {
		t.Errorf("provokeUnlinked: the preparatory RemoveSink failed: %v", err)
		return false
	}
	if h.p.srtqSrcPad.GetLastFlowReturn() != gogst.FlowOK {
		// Already poisoned by something real. Nothing to do, and better
		// evidence than anything this function could manufacture.
		mark(t, "provokeUnlinked: srtq:src is already at %s; leaving it alone",
			h.p.srtqSrcPad.GetLastFlowReturn())
		return true
	}

	mark(t, "provokeUnlinked: lifting the srtq:src gate probe so the queue's loop "+
		"pushes into a pad with no peer")
	h.p.srtqSrcPad.RemoveProbe(h.p.srcProbeID)

	poisoned := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.p.srtqSrcPad.GetLastFlowReturn() != gogst.FlowOK {
			poisoned = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Put it back BEFORE returning, whatever happened. An srtq:src with no gate
	// probe is a pipeline with no gate.
	h.p.srcProbeID = h.p.srtqSrcPad.AddProbe(gateProbeMask, h.p.gateProbe)
	if h.p.srcProbeID == 0 {
		t.Fatalf("provokeUnlinked: could not re-add the gate probe to srtq:src; " +
			"the pipeline is now ungated and the rest of this run is meaningless")
	}

	if !poisoned {
		t.Errorf("provokeUnlinked: srtq:src stayed at GST_FLOW_OK for 5 s with no peer and " +
			"no probe; either the queue's loop is not running or the push is not reaching " +
			"the peer check")
		return false
	}
	mark(t, "provokeUnlinked: srtq:src is now %s", h.p.srtqSrcPad.GetLastFlowReturn())
	return true
}

// TestLiveGateProbeDoesNotBlockWhenOpen measures what a gateProbeMask probe
// actually does to a pad when its callback returns each of the three plausible
// GstPadProbeReturn values.
//
// It needs no M2L-X, no capture hardware and no network: fakesrc, queue and
// fakesink are all in the bundle. It exists because the live reconnect test
// could not reach its subject — the pipeline stops dead the instant
// ReplaceSink opens the gate — and this is the smallest experiment that says
// why.
//
// The discriminator is the probe's own call count. A pad that is left blocked
// calls the probe once and never again; a pad that keeps flowing calls it
// thousands of times in 300 ms of free-running fakesrc.
func TestLiveGateProbeDoesNotBlockWhenOpen(t *testing.T) {
	appDir, err := filepath.Abs(env("WSLCOMMS_LIVE_APP_DIR", defaultAppDir))
	if err != nil {
		t.Fatalf("resolving the app directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "gst", "lib", "gstreamer-1.0")); err != nil {
		t.Skipf("no bundled GStreamer under %s: %v", appDir, err)
	}
	if err := Init(appDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	cases := []struct {
		name string
		ret  gogst.PadProbeReturn
	}{
		{"PadProbeDrop (the gate CLOSED)", gogst.PadProbeDrop},
		{"PadProbeOK (the trap: looks open, holds the thread)", gogst.PadProbeOK},
		{"PadProbePass", gogst.PadProbePass},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			element, err := gogst.ParseLaunch(
				// async=false matters: with the gate closed no buffer ever
				// reaches the sink, so an asynchronous sink would never
				// preroll and the state change would be the thing under test
				// instead of the probe.
				"fakesrc name=src ! queue name=q max-size-buffers=8 ! fakesink name=sink sync=false async=false")
			if err != nil {
				t.Fatalf("gst_parse_launch: %v", err)
			}
			pipeline, ok := element.(gogst.Pipeline)
			if !ok {
				t.Fatalf("gst_parse_launch returned a %T", element)
			}
			defer pipeline.BlockSetState(gogst.StateNull, gogst.ClockTime(5*time.Second))

			q := pipeline.GetByName("q")
			if q == nil {
				t.Fatal("no element named q")
			}
			pad := q.GetStaticPad("src")
			if pad == nil {
				t.Fatal("q has no src pad")
			}

			var calls, delivered int64
			var mu sync.Mutex
			id := pad.AddProbe(gateProbeMask, func(gogst.Pad, *gogst.PadProbeInfo) gogst.PadProbeReturn {
				mu.Lock()
				calls++
				mu.Unlock()
				return tc.ret
			})
			if id == 0 {
				t.Fatal("gst_pad_add_probe failed")
			}

			// A second, ordinary probe on the SINK's own pad counts what
			// actually arrived. Calling the gate probe is not the same as
			// delivering the buffer — GST_PAD_PROBE_PASS would be worthless if
			// it passed the probe and dropped the data.
			sink := pipeline.GetByName("sink")
			if sink == nil {
				t.Fatal("no element named sink")
			}
			sinkPad := sink.GetStaticPad("sink")
			if sinkPad == nil {
				t.Fatal("sink has no sink pad")
			}
			if sinkPad.AddProbe(gogst.PadProbeTypeBuffer,
				func(gogst.Pad, *gogst.PadProbeInfo) gogst.PadProbeReturn {
					mu.Lock()
					delivered++
					mu.Unlock()
					return gogst.PadProbeOK
				}) == 0 {
				t.Fatal("could not add the counting probe to the sink")
			}

			if ret := pipeline.BlockSetState(gogst.StatePlaying, gogst.ClockTime(5*time.Second)); !stateChangeOK(ret) {
				t.Fatalf("pipeline would not go to PLAYING: %s", ret)
			}
			time.Sleep(300 * time.Millisecond)

			mu.Lock()
			n, got := calls, delivered
			mu.Unlock()
			t.Logf("returning %s: gate probe called %d time(s), %d buffer(s) reached the sink, "+
				"in 300 ms; pad.IsBlocked()=%v pad.IsBlocking()=%v lastFlowReturn=%s",
				tc.ret, n, got, pad.IsBlocked(), pad.IsBlocking(), pad.GetLastFlowReturn())

			// Each case asserts the behaviour that was MEASURED, so this test
			// passes and stays a guard. Writing it to assert what the three
			// returns were assumed to do made it fail permanently on the OK
			// case, which reads as a regression and trains people to ignore it.
			switch tc.ret {
			case gogst.PadProbeDrop:
				// The gate CLOSED. The callback keeps being invoked — the
				// streaming thread runs, it just discards — so a high call
				// count with nothing delivered is the signature.
				if got != 0 {
					t.Errorf("PadProbeDrop delivered %d buffer(s); the gate is not closing", got)
				}
				if n <= 1 {
					t.Errorf("PadProbeDrop was called only %d time(s); it should be called "+
						"for every buffer, not block the thread", n)
				}

			case gogst.PadProbeOK:
				// The trap. With GST_PAD_PROBE_TYPE_BLOCK in the mask the pad
				// stays flagged blocked for the probe's lifetime, and OK parks
				// the streaming thread instead of letting the buffer through.
				// Measured: 1 call, 0 buffers, against Pass's ~57000 of each.
				//
				// This is asserted rather than merely logged because it is the
				// whole reason gateProbe returns Pass. If a future GStreamer
				// changes it so that OK does pass buffers, this fails and
				// someone re-reads the decision — which is the right outcome.
				if got != 0 {
					t.Errorf("PadProbeOK delivered %d buffer(s); GStreamer's behaviour has changed "+
						"and gateProbe's use of PadProbePass should be revisited", got)
				}
				if n > 1 {
					t.Errorf("PadProbeOK was called %d times; it was measured to be called once "+
						"and then hold the thread", n)
				}

			case gogst.PadProbePass:
				// What gateProbe actually returns: the pad's block flag is
				// ignored for this buffer and the thread runs on.
				if n <= 1 {
					t.Errorf("PadProbePass stopped the pad after %d call(s); the streaming thread "+
						"is being held blocked, which is what Pass exists to avoid", n)
				}
				if got == 0 {
					t.Errorf("PadProbePass delivered NOTHING to the sink; the gate does not open, " +
						"and no media would ever leave this package")
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// BUILD-NOTES.md section 8.6 — a peer loss must not take the capture chain down
// ---------------------------------------------------------------------------

// captureChainElements are the elements that must NEVER leave PLAYING for the
// life of the process (specification section 6.1). srtq is included because a
// queue whose loop task has been stopped by a flow error is the first domino;
// the sink is deliberately absent, because replacing it is the whole design.
var captureChainElements = []string{nameAudioSrc, nameVideoEncod, nameMux, nameSRTQueue}

// assertCaptureChainPlaying reads the real GstState of every element that must
// stay in PLAYING, rather than trusting the absence of a bus message.
//
// A pipeline-fatal error is the loud symptom, but it is not the definition: an
// element whose task has stopped can sit in PLAYING with nothing running, and
// an element that has actually left PLAYING is unambiguous. Both are checked —
// the state here, p.fatalError() at the call site.
func assertCaptureChainPlaying(t *testing.T, p *cgoPipeline, when string) {
	t.Helper()
	if p.pipeline == nil {
		t.Fatalf("%s: the pipeline is gone", when)
	}
	state, pending, ret := p.pipeline.GetState(0)
	if state != gogst.StatePlaying {
		t.Errorf("SHIP-BLOCKER %s: the PIPELINE is in %s (pending %s, %s), not PLAYING",
			when, state, pending, ret)
	}
	for _, name := range captureChainElements {
		el := p.pipeline.GetByName(name)
		if el == nil {
			t.Errorf("SHIP-BLOCKER %s: element %s has vanished from the pipeline", when, name)
			continue
		}
		state, pending, ret := el.GetState(0)
		if state != gogst.StatePlaying {
			t.Errorf("SHIP-BLOCKER %s: %s is in %s (pending %s, %s), not PLAYING — "+
				"the capture chain is down and only Stop/New/Start recovers it",
				when, name, state, pending, ret)
		}
	}
}

// nonStickyPoisonEvent is a SERIALIZED, NON-STICKY downstream event.
//
// It is one of exactly two things gstqueue.c refuses when srcresult is bad,
// and it is the cheaper of the two to reason about. From gstqueue.c 1.28.5,
// gst_queue_handle_sink_event:
//
//	if (queue->srcresult != GST_FLOW_OK) {
//	  if (!GST_EVENT_IS_STICKY (event)) {
//	    GST_QUEUE_MUTEX_UNLOCK (queue);
//	    goto out_flow_error;                 // returns queue->srcresult
//	  } else if (GST_EVENT_TYPE (event) == GST_EVENT_EOS) {
//	    ...
//	    GST_ELEMENT_FLOW_ERROR (queue, queue->srcresult);   // line 1083
//	    goto out_flow_error;
//	  }
//	}
//
// gst_queue_handle_sink_event returns a GstFlowReturn, not a gboolean, so
// out_flow_error hands GST_FLOW_ERROR back to whoever pushed the event.
func nonStickyPoisonEvent(n int) *gogst.Event {
	s := gogst.NewStructureEmpty("WSLCommsPoisonProbe")
	if s == nil {
		return nil
	}
	s.SetValue("cycle", uint32(n))
	return gogst.NewEventCustom(gogst.EventCustomDownstream, s)
}

// muxSinkPad returns the mpegtsmux request pad that the named branch queue is
// linked to. mpegtsmux's sink pads are request pads with generated names, so
// they are found through the peer rather than by name.
func muxSinkPad(p *cgoPipeline, branchQueue string) gogst.Pad {
	q := p.pipeline.GetByName(branchQueue)
	if q == nil {
		return nil
	}
	src := q.GetStaticPad("src")
	if src == nil {
		return nil
	}
	return src.GetPeer()
}

// dropMockSRT asks cmd/mockm2lx to disconnect its current SRT caller.
//
// This is a DELIBERATE peer loss, at an instant of the test's choosing, with
// the listener process still alive afterwards so the next cycle can reconnect
// to it. It is what WP-7's fault injection exists for, and it is a better
// provocation than killing a gst-launch listener: killing a process leaves the
// timing of the loss to the operating system, and this does not.
func dropMockSRT(t *testing.T, control string) bool {
	t.Helper()
	resp, err := http.Post(control+"/control/drop-srt", "application/json", nil)
	if err != nil {
		t.Logf("POST %s/control/drop-srt: %v", control, err)
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Logf("POST %s/control/drop-srt returned HTTP %d: %s", control, resp.StatusCode, body)
		return false
	}
	return strings.Contains(string(body), "true")
}

// TestLivePeerLossDoesNotKillTheCaptureChain is the Gate C verification of
// BUILD-NOTES.md section 8.6.
//
// # What section 8.6 recorded, and what it got wrong
//
// It recorded, on one of three genuine peer losses:
//
//	gst: srtq:        Internal data stream error. (gstqueue.c(1083):
//	    gst_queue_handle_sink_event ())
//	gst: asrc:        Internal data stream error. (gstbasesrc.c(3187):
//	    gst_base_src_loop ())
//	gst: aq / imagefreeze0 / vq: Internal data stream error.
//
// and proposed the mechanism as "a caps update when the PMT is rewritten, a
// tag". THAT PART IS WRONG, and the source says so. gstqueue.c 1.28.5 line 1083
// sits inside `else if (GST_EVENT_TYPE (event) == GST_EVENT_EOS)`; a STICKY
// non-EOS event — which is what a caps update and a tag both are — falls
// through and is enqueued normally even when srcresult is bad. Only two kinds
// of event are refused: EOS, which is refused loudly with
// GST_ELEMENT_FLOW_ERROR, and any serialized NON-sticky event, which is
// refused quietly by returning GST_FLOW_ERROR to the pusher.
//
// gstbasesrc.c:3187 is basesrc's pause path, which posts GST_ELEMENT_FLOW_ERROR
// and then PUSHES EOS DOWNSTREAM. That is the amplifier: one flow error
// anywhere becomes an EOS travelling into every queue in the pipeline, and each
// queue that is already poisoned answers it at line 1083. It is why the
// recorded cascade names five elements and the same source line three times.
//
// # What this test does
//
//  1. Connects to cmd/mockm2lx's SRT listener and lets real media flow.
//  2. Asks the mock to drop the session — a deliberate peer loss at a chosen
//     instant. srtsink's render() fails, GST_ELEMENT_ERROR is posted and the
//     queue's loop stores GST_FLOW_ERROR: the production condition.
//  3. While the queue is poisoned, pushes BOTH refused kinds of event into
//     srtq:sink — a serialized non-sticky one and an EOS — so that every cycle
//     exercises the mechanism instead of one in three.
//  4. Asserts no pipeline-fatal error, no "Internal data stream error" anywhere,
//     and every element that must never leave PLAYING still in PLAYING.
//  5. RemoveSink, backoff past the mock's re-accept refusal window, repeat.
//
// A final phase then sends a REAL end-to-end EOS through the pipeline while the
// queue is poisoned, so the EOS arrives at srtq:sink from mpegtsmux rather than
// from the test. That is the full recorded cascade, and it is destructive, so
// it runs once at the end.
//
// # Proving the test can fail
//
// WSLCOMMS_LIVE_LIFT_EVENT_GATE=1 makes the same binary remove eventGateProbe
// from srtq:sink at the start of every cycle, which is the code as it stood
// when section 8.6 was written. That run MUST fail. A green run with the gate
// lifted means this test is not measuring what it claims to.
func TestLivePeerLossDoesNotKillTheCaptureChain(t *testing.T) {
	appDir, err := filepath.Abs(env("WSLCOMMS_LIVE_APP_DIR", defaultAppDir))
	if err != nil {
		t.Fatalf("resolving the app directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "gst", "lib", "gstreamer-1.0")); err != nil {
		t.Skipf("no bundled GStreamer under %s: %v", appDir, err)
	}
	slate, err := filepath.Abs(env("WSLCOMMS_LIVE_SLATE", defaultSlatePath))
	if err != nil {
		t.Fatalf("resolving the slate: %v", err)
	}

	control := env("WSLCOMMS_LIVE_MOCK_CONTROL", defaultMockControl)
	srtPort := envInt("WSLCOMMS_LIVE_MOCK_SRT_PORT", defaultMockSRTPort)
	if resp, err := http.Get(control + "/control"); err != nil {
		t.Skipf("cmd/mockm2lx is not answering on %s (%v). Start it first:\n"+
			"    go build -o mockm2lx.exe ./cmd/mockm2lx\n"+
			"    ./mockm2lx.exe -addr 127.0.0.1:8099 -srt-addr 127.0.0.1:%d", control, err, srtPort)
	} else {
		resp.Body.Close()
	}

	liftGate := os.Getenv("WSLCOMMS_LIVE_LIFT_EVENT_GATE") == "1"
	cycles := envInt("WSLCOMMS_LIVE_PEER_LOSS_CYCLES", 12)

	if err := Init(appDir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	devID := env("WSLCOMMS_LIVE_AUDIO_DEVICE", defaultAudioDevice)

	pipe, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p, ok := pipe.(*cgoPipeline)
	if !ok {
		t.Fatalf("New returned a %T, not a *cgoPipeline", pipe)
	}
	errs := newErrRecorder(pipe)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = pipe.Stop()
		}
	})

	mark(t, "Start(slate=%s device=%s); %d peer-loss cycles against %s; event gate %s",
		slate, devID, cycles, control,
		map[bool]string{true: "LIFTED (expecting the section 8.6 fault)", false: "installed"}[liftGate])
	if err := pipe.Start(PipelineOpts{SlatePath: slate, AudioDeviceID: devID}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertCaptureChainPlaying(t, p, "immediately after Start")

	local := SinkOpts{Host: "127.0.0.1", Port: srtPort, LatencyMs: DefaultSRTLatencyMs}
	deliberateLosses, poisoned, dropped := 0, 0, int64(0)

	liftIfAsked := func() {
		if liftGate && p.sinkEventProbeID != 0 {
			p.srtqSinkPad.RemoveProbe(p.sinkEventProbeID)
			p.sinkEventProbeID = 0
		}
	}
	restoreIfLifted := func() {
		if liftGate && p.sinkEventProbeID == 0 {
			srtqSrc := p.srtqSrcPad
			p.sinkEventProbeID = p.srtqSinkPad.AddProbe(eventGateProbeMask,
				func(_ gogst.Pad, info *gogst.PadProbeInfo) gogst.PadProbeReturn {
					return p.eventGateProbe(srtqSrc, info)
				})
		}
	}

	for n := 1; n <= cycles; n++ {
		mark(t, "=========== PEER-LOSS CYCLE %d/%d ===========", n, cycles)
		liftIfAsked()
		if liftGate {
			mark(t, "cycle %d: eventGateProbe REMOVED from %s:sink (fault-demonstration mode)",
				n, nameSRTQueue)
		}

		if err := pipe.ReplaceSink(local); err != nil {
			t.Fatalf("cycle %d: ReplaceSink to the mock: %v", n, err)
		}
		_ = pipe.ForceKeyUnit()

		// Real media on the wire before it is taken away, so that there is a
		// push in flight to fail.
		time.Sleep(6 * time.Second)

		before := errs.count()
		droppedBefore := p.eventsDropped.Load()
		mark(t, "cycle %d: asking the mock to drop the SRT session", n)
		if !dropMockSRT(t, control) {
			t.Errorf("cycle %d: the mock did not report dropping a session; "+
				"this cycle has no peer loss in it", n)
		} else {
			deliberateLosses++
		}

		if got := errs.waitForNew(before, 40*time.Second); len(got) == 0 {
			t.Errorf("cycle %d: no bus error arrived within 40 s of the drop", n)
		} else {
			for _, e := range got {
				t.Logf("cycle %d: bus error: %s", n, e)
			}
		}

		// The queue's loop must actually be poisoned, or nothing below is a
		// test of anything. It is set by gst_pad_push a moment before
		// gst_queue_loop copies it into srcresult, so poll rather than assume.
		lastFlow := gogst.FlowOK
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if lastFlow = p.srtqSrcPad.GetLastFlowReturn(); lastFlow != gogst.FlowOK {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		mark(t, "cycle %d: %s:src last flow return is %s", n, nameSRTQueue, lastFlow)

		if lastFlow == gogst.FlowOK {
			t.Errorf("cycle %d: the queue was NOT poisoned by this peer loss, so nothing in "+
				"this cycle can exercise section 8.6", n)
		} else {
			poisoned++

			// Both refused kinds, in the order of increasing consequence.
			if ev := nonStickyPoisonEvent(n); ev != nil {
				accepted := p.srtqSinkPad.SendEvent(ev)
				mark(t, "cycle %d: pushed a serialized NON-STICKY downstream event into %s:sink "+
					"while it was %s; gst_pad_send_event returned %v (false means the queue "+
					"answered out_flow_error and handed GST_FLOW_ERROR back to the pusher)",
					n, nameSRTQueue, lastFlow, accepted)
			}
			accepted := p.srtqSinkPad.SendEvent(gogst.NewEventEOS())
			mark(t, "cycle %d: pushed an EOS into %s:sink while it was %s; "+
				"gst_pad_send_event returned %v (false means gstqueue.c line 1083 ran)",
				n, nameSRTQueue, lastFlow, accepted)

			// The same event again, but handed to MPEGTSMUX rather than to the
			// queue, so that the refusal has somewhere real to propagate.
			// GstAggregator queues a serialized event on its sink pad and
			// forwards it downstream from its own thread, which makes the push
			// into srtq:sink mux's push and not the test's. If a refused event
			// can reach the capture chain at all, this is how.
			if muxSink := muxSinkPad(p, "aq"); muxSink != nil {
				if ev := nonStickyPoisonEvent(n + 1000); ev != nil {
					accepted := muxSink.SendEvent(ev)
					mark(t, "cycle %d: handed %s:%s a serialized non-sticky downstream event "+
						"to forward into the poisoned queue; gst_pad_send_event returned %v",
						n, nameMux, muxSink.GetName(), accepted)
				}
			}
		}

		// Hold the poisoned window open. internal/sender reaches RemoveSink
		// within microseconds of reading the error, so two seconds is a harder
		// test than the field.
		time.Sleep(2 * time.Second)

		if d := p.eventsDropped.Load() - droppedBefore; d > 0 {
			dropped += d
			mark(t, "cycle %d: eventGateProbe dropped %d downstream event(s)", n, d)
		}

		if err := p.fatalError(); err != nil {
			t.Fatalf("SHIP-BLOCKER cycle %d: the pipeline is fatal after a peer loss, which "+
				"means the capture chain is down and only Stop/New/Start recovers it: %v", n, err)
		}
		assertCaptureChainPlaying(t, p, fmt.Sprintf("cycle %d, after the peer loss", n))

		if err := pipe.RemoveSink(); err != nil {
			t.Fatalf("cycle %d: RemoveSink: %v", n, err)
		}
		assertCaptureChainPlaying(t, p, fmt.Sprintf("cycle %d, after RemoveSink", n))
		restoreIfLifted()

		// BACKOFF. The mock refuses re-accept for 6 s after a disconnect, the
		// same one-peer behaviour M2L-X has, so this must outlast it.
		time.Sleep(backoffRung)
	}

	mark(t, "%d/%d cycles produced a deliberate peer loss; %d of them poisoned the queue; "+
		"eventGateProbe dropped %d downstream event(s) in total",
		deliberateLosses, cycles, poisoned, dropped)

	// ---- Final phase: a REAL EOS from mpegtsmux, not from the test. --------
	//
	// gst_element_send_event on a GstBin routes a downstream event to the bin's
	// SOURCE elements, and GstBaseSrc turns EOS into a forced end-of-stream from
	// its own streaming thread. So this is the ordinary "-e" shutdown path:
	// slate and asrc go EOS, both branches drain, mpegtsmux forwards EOS to
	// srtq:sink. With the queue poisoned that is section 8.6's exact cascade —
	// srtq answers at line 1083 and hands GST_FLOW_ERROR back to the aggregator.
	//
	// It ends the stream either way, so it is last.
	mark(t, "FINAL PHASE: poison the queue once more, then send a real end-to-end EOS")
	liftIfAsked()
	if err := pipe.ReplaceSink(local); err != nil {
		t.Fatalf("final phase: ReplaceSink: %v", err)
	}
	_ = pipe.ForceKeyUnit()
	time.Sleep(6 * time.Second)

	beforeEOS := errs.count()
	if !dropMockSRT(t, control) {
		t.Error("final phase: the mock did not report dropping a session")
	}
	errs.waitForNew(beforeEOS, 40*time.Second)

	lastFlow := gogst.FlowOK
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if lastFlow = p.srtqSrcPad.GetLastFlowReturn(); lastFlow != gogst.FlowOK {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mark(t, "final phase: %s:src last flow return is %s; sending EOS into the pipeline",
		nameSRTQueue, lastFlow)
	if lastFlow == gogst.FlowOK {
		t.Error("final phase: the queue was not poisoned, so the EOS proves nothing")
	}

	if !p.pipeline.SendEvent(gogst.NewEventEOS()) {
		t.Log("final phase: gst_element_send_event(pipeline, EOS) returned FALSE")
	}
	time.Sleep(6 * time.Second)

	if err := p.fatalError(); err != nil {
		t.Errorf("SHIP-BLOCKER final phase: an end-to-end EOS reaching a poisoned %s made the "+
			"pipeline fatal — the capture chain is down: %v", nameSRTQueue, err)
	}
	assertCaptureChainPlaying(t, p, "after an end-to-end EOS with the queue poisoned")

	// ---- The whole-run verdict --------------------------------------------
	//
	// "Internal data stream error." is GST_ELEMENT_FLOW_ERROR's message and is
	// the entire signature of section 8.6. Not one of them may appear, from any
	// element, at any point in the run.
	for _, line := range errs.since(0) {
		if strings.Contains(line, "Internal data stream error") {
			t.Errorf("SHIP-BLOCKER: section 8.6's signature appeared: %s", line)
		}
		for _, name := range []string{nameAudioSrc, nameVideoEncod, nameMux, "imagefreeze", "aq", "vq"} {
			if strings.Contains(line, "gst: "+name+":") {
				t.Errorf("SHIP-BLOCKER: an error names %s, which is upstream of the gate: %s",
					name, line)
			}
		}
	}

	mark(t, "teardown: RemoveSink, Stop")
	_ = pipe.RemoveSink()
	if err := pipe.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	stopped = true
}

// provokeGenuine kills a real SRT listener while media is flowing into it. This
// is the production trigger: srtsink's render() fails, GST_ELEMENT_ERROR goes
// to the bus, and the queue's loop takes GST_FLOW_ERROR from the push that was
// in flight.
//
// It is not deterministic. onBusMessage closes the gate synchronously from
// inside render(), and if it wins the race the queue never sees the failure and
// rearmQueueLocked is a no-op. The caller records which happened.
func provokeGenuine(t *testing.T, h *harness) bool {
	t.Helper()
	if h.peer == nil {
		return false
	}

	before := h.errs.count()
	mark(t, "provokeGenuine: killing the SRT listener on 127.0.0.1:%d", h.peerPort)
	h.peer.kill()
	h.peer = nil

	// libsrt declares a silent peer dead at about 5.27 s; srtsink then raises
	// GST_ELEMENT_ERROR(RESOURCE, WRITE) because auto-reconnect is false.
	errs := h.errs.waitForNew(before, 30*time.Second)
	if len(errs) == 0 {
		t.Logf("provokeGenuine: no bus error arrived within 30 s of killing the listener")
		return false
	}
	for _, e := range errs {
		t.Logf("provokeGenuine: bus error: %s", e)
	}
	mark(t, "provokeGenuine: srtq:src last flow return is %s", h.p.srtqSrcPad.GetLastFlowReturn())
	return true
}
