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
	"encoding/json"
	"errors"
	"log"
	"reflect"
	"strconv"
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

// fail is one failed attempt, reported the way the real monitor reports one:
// gst.ReturnOpts.OnConnectError first, then the transition to BACKOFF.
//
// That ORDER is the contract app_return.go relies on — the reason for a failure
// is stored before the failure is counted — so the fake has to keep it or the
// tests would prove something the real machine does not do. See
// internal/gst/return.go's loop, where report runs immediately before
// emit(ReturnStateBackoff).
//
// A nil err is a failure with no reason attached, which is what a monitor built
// by an older caller or a path that never reached libsrt looks like.
func (m *fakeReturnMonitor) fail(err error) {
	m.mu.Lock()
	notify := m.opts.OnConnectError
	m.mu.Unlock()

	if notify != nil && err != nil {
		notify(err)
	}
	m.states <- gst.ReturnStateBackoff
}

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
// Reading the log
// ---------------------------------------------------------------------------

// logCapture collects everything the application logs during one test.
//
// It exists for one assertion that cannot be made any other way: emitError
// writes the diagnostic to the standard logger as well as pushing it across the
// Wails boundary, and the log is what gets mailed in a support bundle. A test
// that only read the event would let a secret through in the half of the output
// that travels furthest.
type logCapture struct {
	mu  sync.Mutex
	buf []byte
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *logCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

// captureLog redirects the standard logger for one test and returns the sink.
// The previous destination and flags are restored by a t.Cleanup, so a failure
// part-way through cannot leave the rest of the binary writing into a dead
// buffer.
func captureLog(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(c)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return c
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
	// The SRT host is always the M2L-X host. The return must go through
	// EffectiveSRTHost, which strips the scheme, port and path off m2lxHost, or
	// a perfectly ordinary configuration dials a host with a scheme in it.
	a, _ := newTestApp(t)
	mon := withFakeReturn(a)

	cfg := srtReturnConfig()
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
	if err := store.Set(secrets.KeySRTReturn, secret); err != nil {
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

// TestTheReturnUsesItsOwnPassphraseAndKeyLength is the property the whole
// change exists for.
//
// M2L-X sets encryption PER OUTPUT — Output 1 (pgm, 40501) measured
// encrypted=false while Outputs 2 and 3 measured encrypted=true — so the
// commentary INPUT the feed goes to and the OUTPUT the monitor comes from
// routinely need different answers. When the return read secrets.KeySRT and
// cfg.PBKeyLen there was no way to express that: setting the key that makes the
// monitor work changed the key the feed goes out with, and the two faults are
// indistinguishable from the outside.
func TestTheReturnUsesItsOwnPassphraseAndKeyLength(t *testing.T) {
	a, store := newTestApp(t)
	mon := withFakeReturn(a)

	const sendSecret = "the-contribution-input-key"
	const returnSecret = "the-programme-output-key"
	if err := store.Set(secrets.KeySRT, sendSecret); err != nil {
		t.Fatalf("store.Set(KeySRT) error = %v", err)
	}
	if err := store.Set(secrets.KeySRTReturn, returnSecret); err != nil {
		t.Fatalf("store.Set(KeySRTReturn) error = %v", err)
	}

	cfg := srtReturnConfig()
	cfg.PBKeyLen = 16          // the send path negotiates AES-128
	cfg.SRTReturnPBKeyLen = 32 // the return negotiates AES-256
	setConfig(a, cfg)

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	opts, _ := mon.startedWith()

	if opts.Passphrase == sendSecret {
		t.Fatal("the return was given the SEND path's passphrase. Those are the keys to two " +
			"different M2L-X endpoints; sharing them means fixing the monitor breaks the feed.")
	}
	if opts.Passphrase != returnSecret {
		t.Fatalf("Passphrase = %q, want the one stored under %q", opts.Passphrase, secrets.TargetSRTReturn)
	}
	if opts.PBKeyLen != 32 {
		t.Errorf("PBKeyLen = %d, want srtReturnPBKeyLen's 32, not pbkeylen's %d",
			opts.PBKeyLen, cfg.PBKeyLen)
	}

	_ = a.StopReturn()
}

func TestStartReturnRefusesAnEncryptedSessionWithNoKey(t *testing.T) {
	// The exact fault that cost an afternoon, caught before anything is dialled.
	//
	// srtReturnPBKeyLen non-zero with no stored passphrase asks libsrt for an
	// encrypted session with no key. It cannot succeed against anything, and
	// left to run it produces a lamp stuck in BACKOFF and an ERROR:UNSECURE
	// buried in a log file. The Settings screen now has both controls, so a
	// non-zero key length is a statement the operator made rather than a
	// default they inherited, and refusing here with the Credential Manager
	// target named is worth more than an amber lamp.
	for _, keylen := range []int{16, 32} {
		t.Run(strconv.Itoa(keylen), func(t *testing.T) {
			a, _ := newTestApp(t)
			mon := withFakeReturn(a)

			cfg := srtReturnConfig()
			cfg.SRTReturnPBKeyLen = keylen
			setConfig(a, cfg)

			err := a.StartReturn()
			if err == nil {
				t.Fatal("StartReturn() = nil; an encrypted session with no key cannot connect")
			}
			// It must say where to put the passphrase, which Credential Manager
			// entry that is, and that it is not the feed's one.
			msg := err.Error()
			for _, want := range []string{secrets.TargetSRTReturn, "srtReturnPBKeyLen", "Settings"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not mention %q", msg, want)
				}
			}
			if _, started := mon.startedWith(); started {
				t.Error("a monitor was started for a configuration that cannot handshake")
			}
		})
	}
}

func TestTheSendPathsKeyLengthDoesNotGateTheReturn(t *testing.T) {
	// The mirror of the test above, and the reason the two key lengths are two
	// fields. App.srtPassphrase refuses to start the FEED when pbkeylen is
	// non-zero and no SEND passphrase is stored. That must have no bearing on
	// the monitor: an encrypted commentary input and an unencrypted programme
	// output is a perfectly ordinary arrangement, and it is the one measured on
	// the live instance.
	a, _ := newTestApp(t)
	mon := withFakeReturn(a)

	cfg := srtReturnConfig()
	cfg.PBKeyLen = 16         // the send path asks for encryption; no send passphrase is stored
	cfg.SRTReturnPBKeyLen = 0 // the output being monitored is not encrypted
	setConfig(a, cfg)

	if _, err := a.srtPassphrase(cfg); err == nil {
		t.Fatal("the test's premise is wrong: srtPassphrase accepted pbkeylen with no passphrase")
	}
	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() = %v; the SEND path's credentials must not gate the monitor", err)
	}
	opts, started := mon.startedWith()
	if !started {
		t.Fatal("the monitor was not started")
	}
	if opts.PBKeyLen != 0 {
		t.Errorf("PBKeyLen = %d, want 0; the return took the send path's key length", opts.PBKeyLen)
	}
	if opts.Passphrase != "" {
		t.Error("the return was given a passphrase it has none stored for")
	}
	_ = a.StopReturn()
}

func TestAStoredReturnPassphraseWithNoKeyLengthIsStillOffered(t *testing.T) {
	// Where the return stays forgiving. gst.ReturnOpts.normalise defaults an
	// unset key length to 16 when a passphrase is present, so this is a working
	// AES-128 session rather than a contradiction — and an operator who typed a
	// passphrase and left the dropdown alone meant to use it.
	a, store := newTestApp(t)
	mon := withFakeReturn(a)

	const secret = "typed-the-passphrase-only"
	if err := store.Set(secrets.KeySRTReturn, secret); err != nil {
		t.Fatalf("store.Set() error = %v", err)
	}
	cfg := srtReturnConfig()
	cfg.SRTReturnPBKeyLen = 0
	setConfig(a, cfg)

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() = %v", err)
	}
	opts, _ := mon.startedWith()
	if opts.Passphrase != secret {
		t.Fatalf("Passphrase = %q, want the stored secret", opts.Passphrase)
	}
	_ = a.StopReturn()
}

func TestReturnPassphraseReportsAnUnreadableCredentialStore(t *testing.T) {
	// A Credential Manager that cannot be READ is a fault rather than a state,
	// and it is not the same thing as "nothing has been entered yet". It must
	// not be swallowed into an unencrypted session that then fails at the far
	// end for a reason that has nothing to do with the far end.
	a, store := newTestApp(t)
	store.getErr = errors.New("credential manager is not available")

	cfg := srtReturnConfig()
	_, err := a.returnPassphrase(cfg)
	if err == nil {
		t.Fatal("returnPassphrase() = nil error on an unreadable store")
	}
	if !strings.Contains(err.Error(), secrets.TargetSRTReturn) {
		t.Errorf("error %q does not name the Credential Manager target it could not read", err)
	}
}

// ---------------------------------------------------------------------------
// Saying why the return will not connect
// ---------------------------------------------------------------------------

func TestTheReturnSaysWhyItIsNotConnecting(t *testing.T) {
	// Before this, a return that could not handshake said NOTHING: the lamp
	// showed BACKOFF, which is also what it shows for a peer that has gone away
	// for ten seconds, and the reason lived in a log file nobody had open. That
	// is what cost the operator an afternoon against an encrypted output with no
	// passphrase set.
	a, _ := newTestApp(t)
	mon := withFakeReturn(a)
	setConfig(a, srtReturnConfig())
	silencePump(a)

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	for i := 0; i < returnDiagnoseAfter; i++ {
		mon.emit(gst.ReturnStateConnecting)
		mon.emit(gst.ReturnStateBackoff)
	}
	_ = a.StopReturn() // joins the forwarder, so every event has been queued

	msgs := errorEventsFrom(drainPump(a))
	if len(msgs) != 1 {
		t.Fatalf("got %d %q events, want exactly 1: %q", len(msgs), EventError, msgs)
	}
	// It must name the endpoint that is actually being dialled and the
	// encryption that is actually being offered, and point at the fix.
	for _, want := range []string{"40503", "NO encryption", "Settings"} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("the diagnostic %q does not mention %q", msgs[0], want)
		}
	}
}

func TestTheReturnSaysNothingAboutABlip(t *testing.T) {
	// Fewer than returnDiagnoseAfter failures is indistinguishable from an
	// M2L-X output the operator has not switched on yet, and a toast for that
	// is the noise the "error" event must not carry during a match.
	a, _ := newTestApp(t)
	mon := withFakeReturn(a)
	setConfig(a, srtReturnConfig())
	silencePump(a)

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	for i := 0; i < returnDiagnoseAfter-1; i++ {
		mon.emit(gst.ReturnStateConnecting)
		mon.emit(gst.ReturnStateBackoff)
	}
	_ = a.StopReturn()

	if msgs := errorEventsFrom(drainPump(a)); len(msgs) != 0 {
		t.Fatalf("got %d %q events after %d failures, want none: %q",
			len(msgs), EventError, returnDiagnoseAfter-1, msgs)
	}
}

func TestTheReturnSaysItOnceAndThenStaysQuiet(t *testing.T) {
	// The reconnect ladder caps at thirty seconds and never gives up, so a
	// permanent fault would otherwise report twice a minute for the rest of the
	// match and bury the toasts that mean the FEED is off air. One message per
	// StartReturn, and a successful connect re-arms it — an outage that clears
	// and returns is new information.
	a, _ := newTestApp(t)
	mon := withFakeReturn(a)
	setConfig(a, srtReturnConfig())
	silencePump(a)

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	for i := 0; i < returnDiagnoseAfter*4; i++ {
		mon.emit(gst.ReturnStateConnecting)
		mon.emit(gst.ReturnStateBackoff)
	}
	_ = a.StopReturn()

	if msgs := errorEventsFrom(drainPump(a)); len(msgs) != 1 {
		t.Fatalf("got %d %q events over %d failures, want exactly 1",
			len(msgs), EventError, returnDiagnoseAfter*4)
	}
}

func TestAConnectedReturnResetsTheFailureCount(t *testing.T) {
	// RECEIVING means the handshake succeeded and audio is flowing, so whatever
	// was wrong is not wrong now. A count that survived it would announce an
	// encryption problem because of two blips an hour apart.
	a, _ := newTestApp(t)
	mon := withFakeReturn(a)
	setConfig(a, srtReturnConfig())
	silencePump(a)

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	for i := 0; i < returnDiagnoseAfter*2; i++ {
		mon.emit(gst.ReturnStateConnecting)
		mon.emit(gst.ReturnStateBackoff)
		mon.emit(gst.ReturnStateReceiving)
	}
	_ = a.StopReturn()

	if msgs := errorEventsFrom(drainPump(a)); len(msgs) != 0 {
		t.Fatalf("got %d %q events for failures that each recovered, want none: %q",
			len(msgs), EventError, msgs)
	}
}

func TestTheDiagnosticNamesTheEncryptionItOffered(t *testing.T) {
	// Two ways to get encryption wrong, two different messages. Reporting "not
	// connected" for both is what made the fault undiagnosable in the first
	// place.
	//
	// The reason is nil throughout: this is the half of the message that is built
	// from what THIS process knows, and it has to stand on its own for a caller
	// that set no callback. What libsrt said is the other half and is tested
	// below.
	tests := []struct {
		name       string
		opts       gst.ReturnOpts
		want       []string
		wantAbsent []string
	}{
		{
			name: "nothing offered",
			opts: gst.ReturnOpts{Host: "m2lx.example.com", Port: 40503},
			want: []string{"srt://m2lx.example.com:40503", "NO encryption", "has a passphrase set"},
		},
		{
			name: "aes-128 offered",
			opts: gst.ReturnOpts{Host: "m2lx.example.com", Port: 40503, Passphrase: "k", PBKeyLen: 16},
			want: []string{"AES-128", "wrong", "not encrypted at all"},
			// The two must not be confusable: an operator who reads "NO
			// encryption" here would go and set a passphrase that is already set.
			wantAbsent: []string{"NO encryption"},
		},
		{
			name: "aes-256 offered",
			opts: gst.ReturnOpts{Host: "m2lx.example.com", Port: 40501, Passphrase: "k", PBKeyLen: 32},
			want: []string{"AES-256", "srt://m2lx.example.com:40501"},
		},
		{
			// A passphrase with no key length. gst.ReturnOpts.normalise turns
			// that into AES-128 — but on the monitor's own copy of the options,
			// because Start takes them by value, so the defaulting never comes
			// back here. Reporting "AES-0" would send the operator looking for
			// a setting that is not wrong.
			name:       "a passphrase with no key length is the AES-128 gst will negotiate",
			opts:       gst.ReturnOpts{Host: "m2lx.example.com", Port: 40503, Passphrase: "k"},
			want:       []string{"AES-128"},
			wantAbsent: []string{"AES-0", "NO encryption"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := returnDiagnostic(tt.opts, nil)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("returnDiagnostic() = %q, does not contain %q", got, want)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("returnDiagnostic() = %q, must not contain %q", got, absent)
				}
			}
		})
	}
}

func TestTheDiagnosticNeverCarriesThePassphrase(t *testing.T) {
	// It is built from gst.ReturnOpts, which HOLDS the passphrase, and it goes to
	// the "error" event — which crosses the Wails boundary AND is written to the
	// log by emitError, which is what ends up in a support bundle. Asserted rather
	// than assumed, over both destinations.
	const secret = "correct-horse-battery-staple"

	// Half of this message is now a string this process did not compose. The
	// reason comes from libsrt by way of GStreamer, and the guarantee that a
	// secret cannot appear in it must not rest on somebody else's error
	// formatting: the worst case is put in deliberately and must come back out.
	reasons := []error{
		nil,
		errors.New("gst: return monitor: retsrc: Connection setup failure: ERROR:BADSECRET"),
		errors.New("gst: return monitor: retsrc: something went wrong with " + secret + " somehow"),
	}
	for _, keylen := range []int{0, 16, 32} {
		for _, reason := range reasons {
			got := returnDiagnostic(gst.ReturnOpts{
				Host:       "m2lx.example.com",
				Port:       40503,
				Passphrase: secret,
				PBKeyLen:   keylen,
			}, reason)
			if strings.Contains(got, secret) {
				t.Fatalf("the diagnostic leaks the passphrase (reason %v): %q", reason, got)
			}
		}
	}

	// And end to end, through the event the frontend actually receives and the
	// log line emitError writes beside it.
	logs := captureLog(t)

	a, store := newTestApp(t)
	mon := withFakeReturn(a)
	if err := store.Set(secrets.KeySRTReturn, secret); err != nil {
		t.Fatalf("store.Set() error = %v", err)
	}
	cfg := srtReturnConfig()
	cfg.SRTReturnPBKeyLen = 32
	setConfig(a, cfg)
	silencePump(a)

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	for i := 0; i < returnDiagnoseAfter; i++ {
		mon.emit(gst.ReturnStateConnecting)
		mon.fail(errors.New("gst: return monitor: refused, and the key was " + secret))
	}
	_ = a.StopReturn()

	for _, e := range drainPump(a) {
		if s, ok := e.data.(string); ok && strings.Contains(s, secret) {
			t.Fatalf("the %q event leaks the return passphrase: %q", e.name, s)
		}
	}
	if text := logs.text(); strings.Contains(text, secret) {
		t.Fatalf("the application log leaks the return passphrase:\n%s", text)
	}
}

// ---------------------------------------------------------------------------
// What libsrt said
// ---------------------------------------------------------------------------

func TestTheDiagnosticSaysWhatLibsrtSaid(t *testing.T) {
	// The defect, closed. libsrt names the two encryption faults differently and
	// the reason now travels out of internal/gst on
	// gst.ReturnOpts.OnConnectError, so the operator is told which one it was
	// instead of being handed both and asked to choose.
	badSecret := errors.New("gst: return monitor: retsrc: Could not read from resource. " +
		"(Error on SRT socket: Connection setup failure: ERROR:BADSECRET)")
	unsecure := errors.New("gst: return monitor: retsrc: Could not read from resource. " +
		"(Error on SRT socket: Connection setup failure: ERROR:UNSECURE)")

	tests := []struct {
		name       string
		opts       gst.ReturnOpts
		reason     error
		want       []string
		wantAbsent []string
	}{
		{
			// A passphrase was offered and the far end has a different one.
			// There is exactly one fix and it is one field.
			name:   "badsecret with a passphrase offered",
			opts:   gst.ReturnOpts{Host: "m2lx.example.com", Port: 40503, Passphrase: "k", PBKeyLen: 32},
			reason: badSecret,
			want: []string{
				"AES-256",
				"ERROR:BADSECRET",
				"does not match",
				"Settings",
				// The verbatim reason is quoted even when it has been
				// classified, so a screenshot carries the original text and
				// not this file's paraphrase of it.
				badSecret.Error(),
			},
			// It must not still be offering the operator the menu of guesses
			// the message used to end with.
			wantAbsent: []string{"No reason was reported"},
		},
		{
			// We offered nothing and the far end wants encryption. Combined with
			// what we offered, UNSECURE is determined: the fix is to SET a
			// passphrase.
			name:   "unsecure with nothing offered",
			opts:   gst.ReturnOpts{Host: "m2lx.example.com", Port: 40503},
			reason: unsecure,
			want: []string{
				"NO encryption",
				"ERROR:UNSECURE",
				"requires encryption",
				"Set the SRT return passphrase",
			},
		},
		{
			// The mirror image, and the reason the two branches exist. Here the
			// fix is the opposite one — clear the passphrase — and a message
			// that told this operator to set one would send them further away.
			name:   "unsecure with a passphrase offered",
			opts:   gst.ReturnOpts{Host: "m2lx.example.com", Port: 40503, Passphrase: "k", PBKeyLen: 16},
			reason: unsecure,
			want: []string{
				"AES-128",
				"ERROR:UNSECURE",
				"not encrypted at all",
				"key length to 0",
			},
			wantAbsent: []string{"Set the SRT return passphrase and key length on the Settings screen"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := returnDiagnostic(tt.opts, tt.reason)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("returnDiagnostic() = %q, does not contain %q", got, want)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("returnDiagnostic() = %q, must not contain %q", got, absent)
				}
			}
		})
	}
}

func TestAnUnrecognisedReasonIsQuotedAndNotGuessedAt(t *testing.T) {
	// The more important of the two branches.
	//
	// Most reasons that reach here are not libsrt's at all: a plugin missing from
	// the bundle, a headphone endpoint that has gone away, an M2L-X output that is
	// up and carrying no AAC. Mapping one of those onto "check your passphrase"
	// would send the operator to a setting that is correct and cost another
	// afternoon. And the reason text is not a stable interface — it is GStreamer's
	// wording round libsrt's, and it changes between versions — so this is also
	// what a future rename of the two tokens degrades to.
	//
	// An unrecognised string an operator can paste into a search box beats a
	// confident wrong summary.
	reasons := []error{
		errors.New("gst: return monitor: the bundled GStreamer is missing mpegtsdemux"),
		errors.New("gst: return monitor: no audio arrived from srt://10.0.0.1:40503 within 10s"),
		errors.New("gst: return monitor: retsrc: Error on SRT socket: Connection timeout (16)"),
		errors.New("gst: return monitor: retsink: Failed to open the audio device"),
	}
	for _, reason := range reasons {
		got := returnDiagnostic(gst.ReturnOpts{Host: "m2lx.example.com", Port: 40503}, reason)

		if !strings.Contains(got, reason.Error()) {
			t.Errorf("returnDiagnostic() = %q, does not quote the reason %q verbatim", got, reason)
		}
		if !strings.Contains(got, "not a reason this application recognises") {
			t.Errorf("returnDiagnostic() = %q, does not say the reason is unrecognised", got)
		}
		// It must not have been mapped onto either of the two it can name.
		for _, guess := range []string{"ERROR:BADSECRET", "ERROR:UNSECURE"} {
			if strings.Contains(got, guess) {
				t.Errorf("returnDiagnostic() = %q, asserts %s for a reason that did not say so", got, guess)
			}
		}
	}
}

func TestRedactionNeverCorruptsTheReason(t *testing.T) {
	// Caught by this suite the first time it was written, and worth keeping.
	//
	// Redacting the passphrase out of a string this process did not compose is
	// belt and braces — internal/gst guarantees it is not in there — but done
	// blindly it destroys the thing the verbatim branch exists for. A short value
	// occurs inside ordinary English: with the passphrase "k", "socket" became
	// "soc[redacted]et" in the one message an operator is told to search for.
	//
	// libsrt will not accept a passphrase shorter than ten characters, so a value
	// that short is not encrypting anything and there is nothing to protect.
	reason := errors.New("gst: return monitor: retsrc: Error on SRT socket: " +
		"Connection setup failure: ERROR:BADSECRET")

	for _, short := range []string{"k", "et", "on", "or", "SRT", "socket"} {
		got := returnDiagnostic(gst.ReturnOpts{
			Host: "m2lx.example.com", Port: 40503, Passphrase: short, PBKeyLen: 16,
		}, reason)
		if !strings.Contains(got, reason.Error()) {
			t.Errorf("a %d-character passphrase %q corrupted the quoted reason: %q", len(short), short, got)
		}
	}

	// And the other direction: a value long enough to be a real passphrase is
	// still taken out, wherever it appears.
	const real = "correct-horse-battery-staple"
	if got := redactPassphrase("prefix "+real+" suffix", real); strings.Contains(got, real) {
		t.Errorf("redactPassphrase left a full-length passphrase in place: %q", got)
	}
}

func TestABadSecretReasonReachesTheOperator(t *testing.T) {
	// End to end, over the wire the operator is actually on: the monitor reports
	// the reason on gst.ReturnOpts.OnConnectError, the forwarder counts the
	// failures, and the one message emitted after returnDiagnoseAfter of them
	// carries libsrt's answer.
	//
	// This is the whole defect in one test. Before it, the same run produced a
	// message that named the endpoint, listed both ways encryption can be wrong,
	// and pointed at a log file.
	badSecret := errors.New("gst: return monitor: retsrc: Could not read from resource. " +
		"(Error on SRT socket: Connection setup failure: ERROR:BADSECRET)")

	a, store := newTestApp(t)
	mon := withFakeReturn(a)
	if err := store.Set(secrets.KeySRTReturn, "wrong-passphrase"); err != nil {
		t.Fatalf("store.Set() error = %v", err)
	}
	cfg := srtReturnConfig()
	cfg.SRTReturnPBKeyLen = 32
	setConfig(a, cfg)
	silencePump(a)

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	// The callback must have been wired in before Start, or the first attempt's
	// reason is lost.
	opts, started := mon.startedWith()
	if !started {
		t.Fatal("the monitor was not started")
	}
	if opts.OnConnectError == nil {
		t.Fatal("the monitor was started with no OnConnectError; the reason has no route out of internal/gst")
	}

	for i := 0; i < returnDiagnoseAfter; i++ {
		mon.emit(gst.ReturnStateConnecting)
		mon.fail(badSecret)
	}
	_ = a.StopReturn() // joins the forwarder, so every event has been queued

	msgs := errorEventsFrom(drainPump(a))
	if len(msgs) != 1 {
		t.Fatalf("got %d %q events, want exactly 1: %q", len(msgs), EventError, msgs)
	}
	for _, want := range []string{
		"ERROR:BADSECRET", // what libsrt said
		"does not match",  // what it means
		"Settings",        // what to do about it
		"AES-256",         // and what was offered, so the fix is locatable
		badSecret.Error(), // the original text, quoted
	} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("the diagnostic %q does not mention %q", msgs[0], want)
		}
	}
}

func TestTheReturnStillSpeaksWhenNoReasonArrives(t *testing.T) {
	// A failure that reported nothing — a state transition without a preceding
	// callback — must not silence the message. It was useful before the reason
	// existed and it is still useful without one; it just says that it is
	// inferring.
	a, _ := newTestApp(t)
	mon := withFakeReturn(a)
	setConfig(a, srtReturnConfig())
	silencePump(a)

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	for i := 0; i < returnDiagnoseAfter; i++ {
		mon.emit(gst.ReturnStateConnecting)
		mon.fail(nil)
	}
	_ = a.StopReturn()

	msgs := errorEventsFrom(drainPump(a))
	if len(msgs) != 1 {
		t.Fatalf("got %d %q events, want exactly 1: %q", len(msgs), EventError, msgs)
	}
	for _, want := range []string{"40503", "NO encryption", "No reason was reported", "Settings"} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("the diagnostic %q does not mention %q", msgs[0], want)
		}
	}
}

func TestTheDiagnosticReportsTheLatestReason(t *testing.T) {
	// The message is composed when it is emitted, not when the session started,
	// and it reports why the LAST attempt failed. A fault that changes — an
	// output that is switched on and turns out to be encrypted — must be
	// described by what is wrong now, not by what was wrong three attempts ago.
	first := errors.New("gst: return monitor: retsrc: Error on SRT socket: Connection timeout (16)")
	latest := errors.New("gst: return monitor: retsrc: Connection setup failure: ERROR:BADSECRET")

	a, _ := newTestApp(t)
	mon := withFakeReturn(a)
	setConfig(a, srtReturnConfig())
	silencePump(a)

	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	for i := 0; i < returnDiagnoseAfter-1; i++ {
		mon.emit(gst.ReturnStateConnecting)
		mon.fail(first)
	}
	mon.emit(gst.ReturnStateConnecting)
	mon.fail(latest)
	_ = a.StopReturn()

	msgs := errorEventsFrom(drainPump(a))
	if len(msgs) != 1 {
		t.Fatalf("got %d %q events, want exactly 1: %q", len(msgs), EventError, msgs)
	}
	if !strings.Contains(msgs[0], "ERROR:BADSECRET") {
		t.Errorf("the diagnostic %q reports something other than the latest reason", msgs[0])
	}
	if strings.Contains(msgs[0], "Connection timeout") {
		t.Errorf("the diagnostic %q reports a stale reason", msgs[0])
	}
}

func TestNoBoundMethodHandsBackTheReturnPassphrase(t *testing.T) {
	// SetSecret is write-only by design and there is no getter anywhere on the
	// bound surface. The new credential must not have quietly become the
	// exception — GetConfig is the method a Settings screen calls on every open,
	// and a passphrase in the config struct would cross the boundary on every
	// one of them.
	a, store := newTestApp(t)
	const secret = "correct-horse-battery-staple"
	if err := store.Set(secrets.KeySRTReturn, secret); err != nil {
		t.Fatalf("store.Set() error = %v", err)
	}

	cfg, err := a.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("GetConfig() carries the return passphrase across the Wails boundary: %s", encoded)
	}

	// The key length is not a secret and MUST be there, or the Settings screen
	// cannot show what is configured.
	if !strings.Contains(string(encoded), "srtReturnPBKeyLen") {
		t.Fatalf("GetConfig() does not carry srtReturnPBKeyLen: %s", encoded)
	}

	// Reflection over the whole bound surface: no exported method may return
	// the passphrase. Every one that takes no arguments is called and its
	// results searched.
	v := reflect.ValueOf(a)
	for i := 0; i < v.NumMethod(); i++ {
		m := v.Type().Method(i)
		if m.Type.NumIn() != 1 { // the receiver only
			continue
		}
		switch m.Name {
		case "Start", "Stop", "StartReturn", "StopReturn", "ArmMixer", "DisarmMixer":
			// State-changing. Not getters, and not worth starting a pipeline
			// inside an assertion about strings.
			continue
		}
		for _, out := range v.Method(i).Call(nil) {
			if !out.IsValid() || !out.CanInterface() {
				continue
			}
			blob, err := json.Marshal(out.Interface())
			if err != nil {
				continue // not JSON-encodable, so not what Wails sends either
			}
			if strings.Contains(string(blob), secret) {
				t.Fatalf("%s() returns the SRT return passphrase across the Wails boundary", m.Name)
			}
		}
	}
}

// errorEventsFrom picks the "error" event payloads out of a drained pump.
func errorEventsFrom(events []pumpEvent) []string {
	var out []string
	for _, e := range events {
		if e.name != EventError {
			continue
		}
		s, ok := e.data.(string)
		if !ok {
			continue
		}
		out = append(out, s)
	}
	return out
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
