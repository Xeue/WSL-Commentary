//go:build dev || production || bindings

// Tests for the SRT return monitor's half of the bound surface (app_return.go).
//
// The state machine itself is tested in internal/gst/return_test.go against a
// fake pipeline. What is tested here is the WIRE-UP, and specifically the four
// properties that keep a headphone monitor from being able to hurt a live match:
// it is refused unless it is the selected return path, it is validated by
// ValidateReturn and never by Validate, it holds a lock the contribution session
// does not, and its states reach the frontend on their own event.
package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
	"wslcomms/internal/secrets"
)

// ---------------------------------------------------------------------------
// A fake ReturnMonitor
// ---------------------------------------------------------------------------

// fakeReturnMonitor is a gst.ReturnMonitor that records what it was given and
// lets a test drive its state stream. It exists so that StartReturn and
// StopReturn can be exercised without a GStreamer pipeline, an SRT peer or a
// sound card.
type fakeReturnMonitor struct {
	mu       sync.Mutex
	started  bool
	stopped  bool
	opts     gst.ReturnOpts
	startErr error
	stopErr  error
	states   chan gst.ReturnState
}

func newFakeReturnMonitor() *fakeReturnMonitor {
	return &fakeReturnMonitor{states: make(chan gst.ReturnState, 8)}
}

func (m *fakeReturnMonitor) Start(opts gst.ReturnOpts) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.startErr; err != nil {
		return err
	}
	m.started = true
	m.opts = opts
	return nil
}

func (m *fakeReturnMonitor) Stop() error {
	m.mu.Lock()
	stopped := m.stopped
	m.stopped = true
	err := m.stopErr
	m.mu.Unlock()

	if !stopped {
		// The real monitor emits STOPPED and then closes the channel, which is
		// what lets the forwarding goroutine exit and StopReturn's join return.
		m.states <- gst.ReturnStateStopped
		close(m.states)
	}
	return err
}

func (m *fakeReturnMonitor) States() <-chan gst.ReturnState { return m.states }

func (m *fakeReturnMonitor) emit(s gst.ReturnState) { m.states <- s }

func (m *fakeReturnMonitor) startedWith() (gst.ReturnOpts, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opts, m.started
}

var _ gst.ReturnMonitor = (*fakeReturnMonitor)(nil)

// withFakeReturn wires a fake monitor into the app and returns it.
func withFakeReturn(a *App) *fakeReturnMonitor {
	mon := newFakeReturnMonitor()
	a.returnDial = func() gst.ReturnMonitor { return mon }
	return mon
}

// srtReturnConfig is validConfig with the SRT return selected.
func srtReturnConfig() *config.Config {
	c := validConfig()
	c.ReturnSource = config.ReturnSourceSRT
	c.ReturnChannel = config.ReturnChannelLeft
	c.SRTReturnPort = 40503
	c.HeadphoneEndpointID = "{0.0.0.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}"
	return c
}

func setConfig(a *App, c *config.Config) {
	a.cfgMu.Lock()
	a.cfg = c
	a.cfgMu.Unlock()
}

// ---------------------------------------------------------------------------
// The bound surface
// ---------------------------------------------------------------------------

func TestListOutputDevices(t *testing.T) {
	a, _ := newTestApp(t)

	devices, err := a.ListOutputDevices()
	if err != nil {
		t.Fatalf("ListOutputDevices() error = %v", err)
	}
	if len(devices) == 0 {
		t.Fatal("ListOutputDevices() returned nothing; the stub offers three")
	}
	for _, d := range devices {
		if d.ID == "" || d.Name == "" {
			t.Fatalf("device %+v has an empty field; the dropdown needs both", d)
		}
	}
}

func TestListOutputDevicesReportsAFailedGstInit(t *testing.T) {
	initErr := errors.New("the bundled GStreamer could not be initialised")
	a := &App{gstInitErr: initErr}

	if _, err := a.ListOutputDevices(); !errors.Is(err, initErr) {
		t.Fatalf("ListOutputDevices() error = %v, want the gst.Init failure", err)
	}
}

func TestOutputDevicesAreNotInputDevices(t *testing.T) {
	// Two enumerations, two identifier spaces, and — on a real machine — two
	// disjoint sets of endpoints. Offering a capture endpoint as a headphone
	// output would fail at StartReturn rather than in the dropdown, so the
	// separation is asserted here.
	a, _ := newTestApp(t)

	ins, err := a.ListInputDevices()
	if err != nil {
		t.Fatalf("ListInputDevices() error = %v", err)
	}
	outs, err := a.ListOutputDevices()
	if err != nil {
		t.Fatalf("ListOutputDevices() error = %v", err)
	}

	inIDs := map[string]bool{}
	for _, d := range ins {
		inIDs[d.ID] = true
	}
	for _, d := range outs {
		if inIDs[d.ID] {
			t.Fatalf("endpoint %q is offered as both a commentary input and a headphone output", d.ID)
		}
	}
}

func TestIsSRTReturnSelected(t *testing.T) {
	// The single place that decides which return path owns the headphones. The
	// frontend must not re-derive it: if the two disagree, both returns play and
	// the commentator hears the programme twice a few hundred milliseconds
	// apart.
	a, _ := newTestApp(t)

	got, err := a.IsSRTReturnSelected()
	if err != nil {
		t.Fatalf("IsSRTReturnSelected() error = %v", err)
	}
	if got {
		t.Fatal("the default configuration selects the WebRTC return, not the SRT one")
	}

	setConfig(a, srtReturnConfig())
	got, err = a.IsSRTReturnSelected()
	if err != nil {
		t.Fatalf("IsSRTReturnSelected() error = %v", err)
	}
	if !got {
		t.Fatal("returnSource \"srt\" must select the SRT return")
	}
}

func TestStartReturnRefusesWhenWebRTCIsSelected(t *testing.T) {
	// The exclusivity, enforced in Go rather than trusted to the frontend.
	//
	// Both paths reach the same headphones by different routes with different
	// latencies. Running them together does not sound like echo; it is two
	// copies of the programme a few hundred milliseconds apart and is unusable
	// to commentate over. The frontend is where a race between a settings save
	// and a monitor page reload would put both of them up at once, which is why
	// the refusal lives here.
	a, _ := newTestApp(t)
	mon := withFakeReturn(a)

	err := a.StartReturn()
	if err == nil {
		t.Fatal("StartReturn() succeeded with returnSource \"webrtc\"")
	}
	if !strings.Contains(err.Error(), "returnSource") {
		t.Fatalf("error %q does not name the setting the operator has to change", err)
	}
	if _, started := mon.startedWith(); started {
		t.Fatal("StartReturn() started a monitor it had already decided to refuse")
	}
}

func TestStartReturnValidatesTheReturnConfigurationAndNamesTheField(t *testing.T) {
	a, _ := newTestApp(t)
	withFakeReturn(a)

	cfg := srtReturnConfig()
	cfg.ReturnChannel = "sideways"
	cfg.SRTReturnPort = 99999
	setConfig(a, cfg)

	err := a.StartReturn()
	if err == nil {
		t.Fatal("StartReturn() accepted an invalid return configuration")
	}
	for _, want := range []string{"returnChannel", "srtReturnPort"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q; ValidateReturn joins one message per bad "+
				"field so the operator sees them all at once", err, want)
		}
	}
}

func TestStartReturnIsNotGatedOnTheContributionConfiguration(t *testing.T) {
	// The monitor must work on a machine that cannot yet send. On the first run
	// after the operator switches to the SRT return, audioDeviceId may well be
	// unset — and refusing to let them hear the return until they have chosen a
	// microphone would be exactly backwards.
	a, _ := newTestApp(t)
	mon := withFakeReturn(a)

	cfg := srtReturnConfig()
	cfg.AudioDeviceID = "" // Validate would reject this; ValidateReturn must not
	cfg.EventID = ""
	setConfig(a, cfg)

	if err := cfg.Validate(); err == nil {
		t.Fatal("the test's premise is wrong: this configuration passes Validate")
	}
	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() = %v; the return must not be gated on the send path's fields", err)
	}
	if _, started := mon.startedWith(); !started {
		t.Fatal("StartReturn() reported success without starting a monitor")
	}
	_ = a.StopReturn()
}

func TestStartReturnPassesTheConfiguredOptions(t *testing.T) {
	a, _ := newTestApp(t)
	mon := withFakeReturn(a)

	cfg := srtReturnConfig()
	cfg.SRTLatencyMs = 200
	setConfig(a, cfg)

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	opts, started := mon.startedWith()
	if !started {
		t.Fatal("the monitor was not started")
	}

	// The host follows the M2L-X host through EffectiveSRTHost, exactly as the
	// send path does. There is deliberately no second host field for the return.
	if opts.Host != cfg.EffectiveSRTHost() {
		t.Errorf("Host = %q, want the effective SRT host %q", opts.Host, cfg.EffectiveSRTHost())
	}
	if opts.Port != 40503 {
		t.Errorf("Port = %d, want the configured 40503", opts.Port)
	}
	if opts.LatencyMs != 200 {
		t.Errorf("LatencyMs = %d, want the configured 200", opts.LatencyMs)
	}
	if opts.Channel != gst.ReturnChannelLeft {
		t.Errorf("Channel = %q, want %q", opts.Channel, gst.ReturnChannelLeft)
	}
	// The IMMDevice endpoint ID, never the browser mediaDeviceId.
	if opts.OutputDeviceID != cfg.HeadphoneEndpointID {
		t.Errorf("OutputDeviceID = %q, want headphoneEndpointId %q",
			opts.OutputDeviceID, cfg.HeadphoneEndpointID)
	}
	if opts.OutputDeviceID == cfg.HeadphoneDeviceID && cfg.HeadphoneDeviceID != "" {
		t.Error("the return was given the browser mediaDeviceId; it takes the WASAPI endpoint ID")
	}

	_ = a.StopReturn()
}

func TestStartReturnUsesTheHostFallbackNotASecondHostField(t *testing.T) {
	// An empty srtHost means "the same host as M2L-X". The return must go
	// through EffectiveSRTHost rather than reading SRTHost directly, or a
	// perfectly ordinary configuration dials the empty string.
	a, _ := newTestApp(t)
	mon := withFakeReturn(a)

	cfg := srtReturnConfig()
	cfg.SRTHost = ""
	cfg.M2LXHost = "https://m2lx.example.com:8443/"
	setConfig(a, cfg)

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	opts, _ := mon.startedWith()
	if opts.Host != "m2lx.example.com" {
		t.Fatalf("Host = %q, want the M2L-X host with its scheme, port and path stripped", opts.Host)
	}
	_ = a.StopReturn()
}

func TestStartReturnRefusesASecondMonitor(t *testing.T) {
	// Two monitors would contend for one headphone endpoint, which is the same
	// hazard sessMu covers for the capture endpoint.
	a, _ := newTestApp(t)
	withFakeReturn(a)
	setConfig(a, srtReturnConfig())

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	if err := a.StartReturn(); !errors.Is(err, errReturnAlreadyRunning) {
		t.Fatalf("second StartReturn() = %v, want errReturnAlreadyRunning", err)
	}
	_ = a.StopReturn()
}

func TestStartReturnRefusesDuringShutdown(t *testing.T) {
	// Building a pipeline now would open a playback endpoint and an SRT socket
	// that teardown has already walked past, and the process would exit still
	// holding them. Same reasoning as startSession; step 0 of the shutdown
	// order in app.go's header.
	a, _ := newTestApp(t)
	withFakeReturn(a)
	setConfig(a, srtReturnConfig())

	a.closing.Store(true)
	if err := a.StartReturn(); !errors.Is(err, errShuttingDown) {
		t.Fatalf("StartReturn() during shutdown = %v, want errShuttingDown", err)
	}
}

func TestStopReturnOnAStoppedMonitor(t *testing.T) {
	// The sentinel is what lets teardown call StopReturn unconditionally without
	// logging a failure every time the operator was on the WebRTC path.
	a, _ := newTestApp(t)
	if err := a.StopReturn(); !errors.Is(err, errReturnNotRunning) {
		t.Fatalf("StopReturn() with nothing running = %v, want errReturnNotRunning", err)
	}
}

func TestStopReturnJoinsTheForwarder(t *testing.T) {
	// By the time StopReturn returns the monitor is stopped and the forwarding
	// goroutine has exited, so a StartReturn immediately afterwards cannot race
	// it for the headphone endpoint.
	a, _ := newTestApp(t)
	withFakeReturn(a)
	setConfig(a, srtReturnConfig())

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	if err := a.StopReturn(); err != nil {
		t.Fatalf("StopReturn() error = %v", err)
	}

	// A fresh monitor must be buildable straight away.
	withFakeReturn(a)
	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() after StopReturn error = %v", err)
	}
	_ = a.StopReturn()
}

func TestStopReturnDoesNotDeadlockOnAStateInFlight(t *testing.T) {
	// The reason retMu and retStateMu are two locks.
	//
	// StopReturn holds retMu across the join of the forwarding goroutine, and
	// that goroutine writes lastReturn on every transition. One lock over both
	// deadlocks the first Stop that lands while a transition is being recorded.
	// The bug is invisible until it happens during a match, so it is asserted
	// with a timeout rather than trusted to the lock order comment.
	a, _ := newTestApp(t)
	mon := withFakeReturn(a)
	setConfig(a, srtReturnConfig())
	silencePump(a)

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	for i := 0; i < 4; i++ {
		mon.emit(gst.ReturnStateConnecting)
		mon.emit(gst.ReturnStateReceiving)
	}

	done := make(chan error, 1)
	go func() { done <- a.StopReturn() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StopReturn() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StopReturn() deadlocked against the state forwarder")
	}
}

func TestReturnStatesReachTheFrontendOnTheirOwnEvent(t *testing.T) {
	// The RETURN lamp is pushed, not polled: the monitor's state changes on its
	// own, from a reconnect loop nobody asks, and a lamp that only updated when
	// the frontend happened to call GetReturnState would show green through an
	// outage the commentator can hear.
	//
	// It is a separate event from "sender" so that neither lamp can be driven by
	// the other's transitions.
	a, _ := newTestApp(t)
	mon := withFakeReturn(a)
	setConfig(a, srtReturnConfig())
	silencePump(a)

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	mon.emit(gst.ReturnStateConnecting)
	mon.emit(gst.ReturnStateReceiving)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s, _ := a.GetReturnState()
		if s == gst.ReturnStateReceiving {
			break
		}
		time.Sleep(time.Millisecond)
	}

	if s, _ := a.GetReturnState(); s != gst.ReturnStateReceiving {
		t.Fatalf("GetReturnState() = %q, want %q", s, gst.ReturnStateReceiving)
	}

	var sawReturn bool
	for _, e := range drainPump(a) {
		if e.name == EventReturn {
			sawReturn = true
			if _, ok := e.data.(gst.ReturnState); !ok {
				t.Fatalf("the %q event carried a %T, want a gst.ReturnState", EventReturn, e.data)
			}
		}
		if e.name == EventSender {
			t.Fatalf("the return monitor emitted a %q event; the two lamps are separate", EventSender)
		}
	}
	if !sawReturn {
		t.Fatalf("no %q event was emitted for a state transition", EventReturn)
	}

	_ = a.StopReturn()
}

func TestGetReturnStateBeforeAnythingHasRun(t *testing.T) {
	// A page that has just loaded must get a grey lamp, not a zero value that
	// renders as an empty string.
	a, _ := newTestApp(t)
	s, err := a.GetReturnState()
	if err != nil {
		t.Fatalf("GetReturnState() error = %v", err)
	}
	if s != gst.ReturnStateStopped {
		t.Fatalf("GetReturnState() = %q, want %q", s, gst.ReturnStateStopped)
	}
}

func TestDomReadyReplaysTheReturnState(t *testing.T) {
	// The monitor emits only on transitions, so a page that reloaded mid-match
	// would otherwise show the RETURN lamp grey while audio was in the
	// commentator's ears. Same reasoning as the sender replay.
	a, _ := newTestApp(t)
	silencePump(a)

	a.retStateMu.Lock()
	a.lastReturn = gst.ReturnStateReceiving
	a.retStateMu.Unlock()

	a.domReady(a.rootCtx)

	var replayed bool
	for _, e := range drainPump(a) {
		if e.name == EventReturn && e.data == gst.ReturnStateReceiving {
			replayed = true
		}
	}
	if !replayed {
		t.Fatalf("domReady did not replay the return state; a page that reloaded mid-match "+
			"would show the RETURN lamp grey with audio playing (events: %v)", drainPump(a))
	}
}

func TestTeardownStopsTheReturnMonitor(t *testing.T) {
	// A wslcomms.exe left holding a WASAPI playback endpoint after a match is a
	// support call.
	a, _ := newTestApp(t)
	mon := withFakeReturn(a)
	setConfig(a, srtReturnConfig())

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}

	a.teardown()

	mon.mu.Lock()
	stopped := mon.stopped
	mon.mu.Unlock()
	if !stopped {
		t.Fatal("teardown did not stop the return monitor")
	}
}

func TestStartReturnReportsAFailedGstInit(t *testing.T) {
	a, _ := newTestApp(t)
	setConfig(a, srtReturnConfig())
	initErr := errors.New("the bundled GStreamer could not be initialised")
	a.gstInitErr = initErr

	if err := a.StartReturn(); !errors.Is(err, initErr) {
		t.Fatalf("StartReturn() = %v, want the gst.Init failure", err)
	}
}

func TestStartReturnLeavesNothingBehindWhenTheMonitorRefusesToStart(t *testing.T) {
	// A monitor whose Start failed has no pipeline and no goroutine, so there is
	// nothing to stop. What must not happen is the App recording a session for
	// it: StopReturn would then join a forwarder that was never launched.
	a, _ := newTestApp(t)
	mon := withFakeReturn(a)
	mon.startErr = errors.New("fake: unusable options")
	setConfig(a, srtReturnConfig())

	if err := a.StartReturn(); err == nil {
		t.Fatal("StartReturn() reported success despite the monitor refusing to start")
	}
	if err := a.StopReturn(); !errors.Is(err, errReturnNotRunning) {
		t.Fatalf("StopReturn() after a failed start = %v, want errReturnNotRunning", err)
	}
}

func TestReturnOptsCarryThePassphraseAndNothingLogsIt(t *testing.T) {
	// The passphrase reaches gst.ReturnOpts, which internal/gst sets with
	// g_object_set rather than in the URI. It must not appear in anything this
	// layer produces.
	a, store := newTestApp(t)
	mon := withFakeReturn(a)
	setConfig(a, srtReturnConfig())

	const secret = "correct-horse-battery-staple"
	if err := store.Set(secrets.KeySRT, secret); err != nil {
		t.Fatalf("store.Set() error = %v", err)
	}

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	opts, _ := mon.startedWith()
	if opts.Passphrase != secret {
		t.Fatalf("Passphrase = %q, want the stored secret", opts.Passphrase)
	}
	// Whatever the return monitor is asked to report about itself, the
	// passphrase is not in it.
	if s, _ := a.GetReturnState(); strings.Contains(string(s), secret) {
		t.Fatal("the return state carries the passphrase")
	}

	_ = a.StopReturn()
}

func TestReturnPassphraseIsForgivingWhereStartIsStrict(t *testing.T) {
	// App.srtPassphrase refuses to start the FEED when pbkeylen is non-zero and
	// no passphrase is stored, because an encrypted session with no key fails
	// inside libsrt with an error nobody can read twenty minutes before
	// kick-off, and Start is the moment to be strict.
	//
	// The return is a monitor. The same combination there costs an amber lamp
	// and a line in the log, and the reconnect loop keeps trying — which is a
	// better outcome than a button that will not work and an error box about a
	// field the operator has not been asked about yet.
	a, _ := newTestApp(t)
	withFakeReturn(a)

	cfg := srtReturnConfig()
	cfg.PBKeyLen = 16 // asks for encryption; no passphrase is stored
	setConfig(a, cfg)

	if _, err := a.srtPassphrase(cfg); err == nil {
		t.Fatal("the test's premise is wrong: srtPassphrase accepted pbkeylen with no passphrase")
	}
	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() = %v; the monitor must not refuse over a credential "+
			"combination the SRT handshake will report on its own", err)
	}
	_ = a.StopReturn()
}

// ---------------------------------------------------------------------------
// The premises the frontend's recovery rests on
// ---------------------------------------------------------------------------

// TestStopReturnDropsItsSessionBeforeItCanFail guards a premise the frontend's
// recovery is built on, in the file where the premise lives.
//
// frontend/src/ui/returnpath.js stops the SRT return before it ever makes the
// WebRTC return audible, and it treats EVERY outcome of that stop — including a
// reported failure — as "the Go side is no longer running a monitor". That is
// only true because StopReturn clears a.ret BEFORE it calls mon.Stop(), so a
// monitor whose Stop failed is still gone from this App's point of view and a
// later StartReturn will build a fresh one rather than refusing.
//
// Move the a.ret = nil after the Stop and the frontend's reasoning silently
// becomes wrong: a failed stop would leave a session behind, the next
// StartReturn would answer errReturnAlreadyRunning forever, and the recovery
// would un-mute WebRTC over the top of a monitor it could never stop.
func TestStopReturnDropsItsSessionBeforeItCanFail(t *testing.T) {
	a, _ := newTestApp(t)
	mon := withFakeReturn(a)
	setConfig(a, srtReturnConfig())

	mon.stopErr = errors.New("gst: return monitor: pipeline would not go to NULL")

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() = %v", err)
	}
	if err := a.StopReturn(); err == nil {
		t.Fatal("the test's premise is wrong: StopReturn reported success on a failing monitor")
	}

	// The session must be gone even though the stop failed, so that the frontend
	// can start again rather than being told one is already running forever.
	a.retMu.Lock()
	sess := a.ret
	a.retMu.Unlock()
	if sess != nil {
		t.Fatal("StopReturn kept its session after a failed Stop. The frontend stops the " +
			"return before it un-mutes WebRTC and treats any outcome as 'no monitor is being " +
			"managed'; keeping the session here makes that false and the next StartReturn " +
			"would refuse with errReturnAlreadyRunning for the rest of the match.")
	}

	// And a second StartReturn must be accepted, which is the observable form of
	// the same property.
	withFakeReturn(a)
	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() after a failed StopReturn = %v, want nil", err)
	}
	_ = a.StopReturn()
}

// TestReturnSentinelsAreSpelledAsTheFrontendMatchesThem guards the string
// contract across the Wails boundary.
//
// Wails flattens a Go error to its message, so neither sentinel survives as
// anything a JavaScript caller can compare identities on.
// frontend/src/ui/backend.js therefore matches on the text, and if the two drift
// apart the frontend stops recognising "already running" — treats it as a failed
// start, falls back to WebRTC and un-mutes it while an orphaned pipeline is
// still writing CLN to the same headphones. returnpath.test.js asserts the same
// pair from the other side; this is the half that fails when the Go text is the
// thing that moved.
func TestReturnSentinelsAreSpelledAsTheFrontendMatchesThem(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errReturnAlreadyRunning, "the return monitor is already running"},
		{errReturnNotRunning, "the return monitor is not running"},
	} {
		if !strings.Contains(strings.ToLower(tc.err.Error()), tc.want) {
			t.Errorf("%q no longer contains %q, which frontend/src/ui/backend.js matches on",
				tc.err.Error(), tc.want)
		}
	}

	// StartReturn must return the sentinel UNWRAPPED, so the message the
	// frontend sees is the sentinel's own text and not a prefix around it.
	a, _ := newTestApp(t)
	withFakeReturn(a)
	setConfig(a, srtReturnConfig())
	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() = %v", err)
	}
	err := a.StartReturn()
	if !errors.Is(err, errReturnAlreadyRunning) {
		t.Fatalf("a second StartReturn = %v, want errReturnAlreadyRunning", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "the return monitor is already running") {
		t.Errorf("the message that crosses the Wails boundary is %q, which the frontend "+
			"cannot recognise as the already-running refusal", err.Error())
	}
	_ = a.StopReturn()
}
