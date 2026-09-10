//go:build dev || production || bindings

// Tests for the application's half of the PGM monitor: launching, the lamp's
// state, restart, crash relaunch, the tee, and what the monitor may reach.
//
// The monitor process is a FAKE throughout — a monitorHost that the test
// starts, stops and crashes on cue. The real link is tested against a helper
// process in internal/monitorlink; the real monitor window is not unit-tested
// anywhere.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"wslcomms/internal/config"
	"wslcomms/internal/remote"
	"wslcomms/internal/secrets"
)

// fakeMonitorHost stands in for a launched monitor process.
type fakeMonitorHost struct {
	mu       sync.Mutex
	startErr error
	started  bool
	stopped  bool
	killed   bool
	exitCode int
	sent     []string
	done     chan struct{}
	doneOnce sync.Once
}

func newFakeMonitorHost() *fakeMonitorHost {
	return &fakeMonitorHost{done: make(chan struct{})}
}

func (h *fakeMonitorHost) Start() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.startErr != nil {
		h.exit(1)
		return h.startErr
	}
	h.started = true
	return nil
}

func (h *fakeMonitorHost) Send(name string, _ any) {
	h.mu.Lock()
	h.sent = append(h.sent, name)
	h.mu.Unlock()
}

func (h *fakeMonitorHost) Stop() {
	h.mu.Lock()
	h.stopped = true
	h.mu.Unlock()
	h.exit(0)
}

func (h *fakeMonitorHost) Kill() {
	h.mu.Lock()
	h.killed = true
	h.mu.Unlock()
	h.exit(-1)
}

func (h *fakeMonitorHost) Done() <-chan struct{} { return h.done }

func (h *fakeMonitorHost) ExitCode() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exitCode
}

func (h *fakeMonitorHost) PID() int { return 4242 }

// exit ends the fake process with code, as a crash (non-zero) or a clean
// close (zero) would.
func (h *fakeMonitorHost) exit(code int) {
	h.doneOnce.Do(func() {
		h.exitCode = code
		close(h.done)
	})
}

// crash is exit from outside the lock, for a test simulating the process
// dying on its own.
func (h *fakeMonitorHost) crash(code int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.exit(code)
}

func (h *fakeMonitorHost) sentNames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.sent...)
}

// withFakeMonitors installs a dial that hands out a fresh fake on every launch
// and records them.
func withFakeMonitors(a *App) func() []*fakeMonitorHost {
	var mu sync.Mutex
	var made []*fakeMonitorHost
	a.monitorDial = func() monitorHost {
		h := newFakeMonitorHost()
		mu.Lock()
		made = append(made, h)
		mu.Unlock()
		return h
	}
	return func() []*fakeMonitorHost {
		mu.Lock()
		defer mu.Unlock()
		return append([]*fakeMonitorHost(nil), made...)
	}
}

func monitorEventsFrom(events []pumpEvent) []monitorPayload {
	var out []monitorPayload
	for _, e := range events {
		if e.name != EventMonitor {
			continue
		}
		if p, ok := e.data.(monitorPayload); ok {
			out = append(out, p)
		}
	}
	return out
}

func TestDomReadyLaunchesTheMonitorOnce(t *testing.T) {
	// The monitor opens with the application, from domReady, and a page reload
	// — a second domReady — must not open a second one.
	a, _ := newTestApp(t)
	silencePump(a)
	made := withFakeMonitors(a)

	a.domReady(context.Background())
	waitForCond(t, "the monitor to be running", func() bool {
		return a.GetMonitorState().Process == monitorProcessRunning
	})
	a.domReady(context.Background())
	time.Sleep(20 * time.Millisecond)

	if n := len(made()); n != 1 {
		t.Fatalf("%d monitor processes were launched across two domReady calls, want exactly 1", n)
	}
	if st := a.GetMonitorState(); st.PID != 4242 {
		t.Fatalf("GetMonitorState() = %+v, want the running process's pid", st)
	}
}

func TestRestartMonitorStopsTheOldProcessAndStartsANewOne(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	made := withFakeMonitors(a)

	a.launchMonitor()
	waitForCond(t, "the first monitor to be running", func() bool {
		return a.GetMonitorState().Process == monitorProcessRunning
	})
	if err := a.RestartMonitor(); err != nil {
		t.Fatalf("RestartMonitor() error = %v", err)
	}
	waitForCond(t, "the second monitor to be running", func() bool {
		hosts := made()
		return len(hosts) == 2 && a.GetMonitorState().Process == monitorProcessRunning
	})
	hosts := made()
	hosts[0].mu.Lock()
	stopped := hosts[0].stopped
	hosts[0].mu.Unlock()
	if !stopped {
		t.Fatal("Restart did not stop the old monitor process; two windows would be open")
	}
	// The old process's clean exit on OUR request was not mistaken for the
	// operator closing it, and nothing was relaunched beyond the one.
	time.Sleep(50 * time.Millisecond)
	if n := len(made()); n != 2 {
		t.Fatalf("%d monitors launched, want 2", n)
	}
}

func TestAMonitorTheOperatorClosedStaysClosed(t *testing.T) {
	// Exit code 0 with nobody having asked is the operator closing the window.
	// Respect it: the lamp says closed, the button reopens it, nothing
	// relaunches on its own.
	a, _ := newTestApp(t)
	silencePump(a)
	made := withFakeMonitors(a)

	a.launchMonitor()
	waitForCond(t, "running", func() bool { return a.GetMonitorState().Process == monitorProcessRunning })
	made()[0].crash(0)
	waitForCond(t, "closed", func() bool { return a.GetMonitorState().Process == monitorProcessClosed })
	time.Sleep(50 * time.Millisecond)
	if n := len(made()); n != 1 {
		t.Fatalf("a monitor the operator closed was relaunched (%d launches)", n)
	}

	// And RestartMonitor opens it again.
	if err := a.RestartMonitor(); err != nil {
		t.Fatalf("RestartMonitor() error = %v", err)
	}
	waitForCond(t, "reopened", func() bool {
		return len(made()) == 2 && a.GetMonitorState().Process == monitorProcessRunning
	})
}

func TestAMonitorThatCrashesIsRelaunchedWithinABudget(t *testing.T) {
	// A non-zero exit nobody asked for is a crash: relaunch, up to
	// monitorRelaunchMax times a minute, then give up with the lamp red.
	a, _ := newTestApp(t)
	silencePump(a)
	made := withFakeMonitors(a)

	a.launchMonitor()
	for i := 0; i < monitorRelaunchMax; i++ {
		waitForCond(t, "running", func() bool {
			return len(made()) == i+1 && a.GetMonitorState().Process == monitorProcessRunning
		})
		made()[i].crash(2)
		waitForCond(t, "relaunched", func() bool { return len(made()) == i+2 })
	}
	// The (max+1)th process is up; when it crashes the budget is spent.
	waitForCond(t, "the last relaunch running", func() bool {
		return a.GetMonitorState().Process == monitorProcessRunning
	})
	made()[monitorRelaunchMax].crash(2)
	waitForCond(t, "failed", func() bool { return a.GetMonitorState().Process == monitorProcessFailed })
	time.Sleep(50 * time.Millisecond)
	if n := len(made()); n != monitorRelaunchMax+1 {
		t.Fatalf("%d launches, want %d: the relaunch budget was not honoured", n, monitorRelaunchMax+1)
	}

	// The failures reached the operator.
	errs := errorEventsFrom(drainPump(a))
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "reopening it") || !strings.Contains(joined, "staying closed") {
		t.Fatalf("the relaunches and the final failure were not reported: %q", joined)
	}
}

func TestAMonitorThatCannotStartIsReportedNotRetried(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	a.monitorDial = func() monitorHost {
		h := newFakeMonitorHost()
		h.startErr = errors.New("no such executable")
		return h
	}
	a.launchMonitor()
	waitForCond(t, "failed", func() bool { return a.GetMonitorState().Process == monitorProcessFailed })
	errs := errorEventsFrom(drainPump(a))
	if len(errs) == 0 || !strings.Contains(errs[0], "could not be opened") {
		t.Fatalf("the failed launch was not reported: %v", errs)
	}
}

func TestEveryEventIsRelayedToTheMonitor(t *testing.T) {
	// The pump's tee: what this window's page hears, the monitor's page hears.
	a, _ := newTestApp(t)
	made := withFakeMonitors(a)
	a.launchMonitor()
	waitForCond(t, "running", func() bool { return a.GetMonitorState().Process == monitorProcessRunning })

	a.teeEvents(EventLevels, silentLevelsPayload())
	a.teeEvents(EventConfig, configEvent{Config: validConfig(), Origin: localClientID})
	sent := made()[0].sentNames()
	if len(sent) < 2 || sent[len(sent)-2] != EventLevels || sent[len(sent)-1] != EventConfig {
		t.Fatalf("relayed events = %v, want levels then config at the end", sent)
	}
}

func TestTheMonitorsKVSStateReachesTheLamp(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	withFakeMonitors(a)
	a.launchMonitor()
	waitForCond(t, "running", func() bool { return a.GetMonitorState().Process == monitorProcessRunning })

	a.monitorEvent(EventMonitor, json.RawMessage(`"connected"`))
	st := a.GetMonitorState()
	if st.KVS != "connected" || st.Process != monitorProcessRunning {
		t.Fatalf("GetMonitorState() = %+v after the monitor reported connected", st)
	}
	events := monitorEventsFrom(drainPump(a))
	if len(events) == 0 || events[len(events)-1].KVS != "connected" {
		t.Fatalf("the monitor's KVS state did not reach the page: %+v", events)
	}
}

func TestTheMonitorReachesTheAllowlistAndNothingHostOnly(t *testing.T) {
	// The monitor calls in as the client "pgm-monitor" through the SAME
	// allowlist the LAN bridge uses. GetConfig answers; a host-only method is
	// refused exactly as it would be for a remote seat; an unknown method is
	// unknown.
	a, _ := newTestApp(t)
	silencePump(a)

	v, err := a.monitorDispatch(context.Background(), "GetConfig", nil)
	if err != nil {
		t.Fatalf("GetConfig over the link error = %v", err)
	}
	if _, ok := v.(*config.Config); !ok {
		t.Fatalf("GetConfig over the link returned %T", v)
	}
	if _, err := a.monitorDispatch(context.Background(), "StopReturn", nil); err == nil ||
		!strings.Contains(err.Error(), "host-only") {
		t.Fatalf("a host-only method was reachable from the monitor: err = %v", err)
	}
	if _, err := a.monitorDispatch(context.Background(), "SetRemoteListener", nil); err == nil {
		t.Fatal("SetRemoteListener was reachable from the monitor")
	}
	if _, err := a.monitorDispatch(context.Background(), "NoSuchMethod", nil); err == nil ||
		!strings.Contains(err.Error(), "unknown") {
		t.Fatalf("an unknown method was not refused as unknown: %v", err)
	}
}

func TestTheMonitorsSavesCarryItsOwnOrigin(t *testing.T) {
	// A save from the monitor is echoed on the "config" event with origin
	// "pgm-monitor", so the monitor's page recognises its own echo and the
	// application's page treats it as another seat's.
	a, _ := newTestApp(t)
	silencePump(a)

	cfg := validConfig()
	cfg.ReturnMid = 3
	raw, _ := json.Marshal(cfg)
	if _, err := a.monitorDispatch(context.Background(), "SaveConfig", []json.RawMessage{raw}); err != nil {
		t.Fatalf("SaveConfig over the link error = %v", err)
	}
	var origin string
	for _, e := range drainPump(a) {
		if e.name == EventConfig {
			if p, ok := e.data.(configEvent); ok {
				origin = p.Origin
			}
		}
	}
	if origin != monitorClientID {
		t.Fatalf("the config event's origin was %q, want %q", origin, monitorClientID)
	}
}

func TestPictureOptsAreAnsweredOnTheLinkOnlyAndCarryTheRefusals(t *testing.T) {
	// PictureOpts is the one thing the monitor may ask for that is NOT in the
	// allowlist: it carries the passphrase. It must be unknown to the bridge,
	// answered on the link, and refused when the SRT audio return holds the
	// output the picture needs.
	a, store := newTestApp(t)
	silencePump(a)
	seat := remote.ClientInfo{ID: "x", RemoteAddr: "10.0.0.2:1"}
	if _, err := a.remoteCall(context.Background(), seat, "PictureOpts", nil); err == nil {
		t.Fatal("PictureOpts is reachable through the allowlist; the passphrase would cross the network")
	}

	cfg := validConfig()
	cfg.ReturnSource = config.ReturnSourceWebRTC
	cfg.SRTReturnPBKeyLen = 32
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()
	if err := store.Set(secrets.KeySRTReturn, "hunter2"); err != nil {
		t.Fatal(err)
	}

	v, err := a.monitorDispatch(context.Background(), "PictureOpts", nil)
	if err != nil {
		t.Fatalf("PictureOpts error = %v", err)
	}
	opts, ok := v.(pictureOptsPayload)
	if !ok {
		t.Fatalf("PictureOpts returned %T", v)
	}
	if opts.Port != config.DefaultSRTReturnPort || opts.PBKeyLen != 32 || opts.Passphrase != "hunter2" {
		t.Fatalf("PictureOpts = %+v, want the programme port, the key length and the stored passphrase",
			pictureOptsPayload{Host: opts.Host, Port: opts.Port, LatencyMs: opts.LatencyMs, PBKeyLen: opts.PBKeyLen})
	}

	cfg.ReturnSource = config.ReturnSourceSRT
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()
	if _, err := a.monitorDispatch(context.Background(), "PictureOpts", nil); err == nil ||
		!strings.Contains(err.Error(), "Kinesis") {
		t.Fatalf("PictureOpts with the SRT audio return selected error = %v, want the refusal naming Kinesis", err)
	}
}

func TestTeardownStopsTheMonitorBeforeThePreview(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	made := withFakeMonitors(a)
	a.launchMonitor()
	waitForCond(t, "running", func() bool { return a.GetMonitorState().Process == monitorProcessRunning })

	if err := a.stopMonitorForTeardown(); err != nil {
		t.Fatalf("stopMonitorForTeardown() error = %v", err)
	}
	h := made()[0]
	h.mu.Lock()
	stopped := h.stopped
	h.mu.Unlock()
	if !stopped {
		t.Fatal("teardown did not stop the monitor process")
	}
	// Its clean exit at our request reads as closed, not as a crash to relaunch.
	waitForCond(t, "closed", func() bool { return a.GetMonitorState().Process == monitorProcessClosed })
	time.Sleep(30 * time.Millisecond)
	if n := len(made()); n != 1 {
		t.Fatalf("teardown's stop was relaunched (%d launches)", n)
	}
}

func TestMonitorCommandRefusesATestBinaryAndMarksTheChild(t *testing.T) {
	a, _ := newTestApp(t)
	if cmd, err := a.monitorCommand(); err == nil {
		t.Fatalf("monitorCommand() built %q inside a test binary; it must refuse", cmd.Args)
	} else if !strings.Contains(err.Error(), "test binary") {
		t.Fatalf("the refusal %q does not say what was refused", err)
	}
	cmd := monitorProcessCommand(`C:\somewhere\wslcomms.exe`)
	if len(cmd.Args) != 1 {
		t.Fatalf("the monitor is launched with arguments %q; it takes none", cmd.Args)
	}
	marked := false
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, monitorEnv+"=") {
			marked = true
		}
	}
	if !marked {
		t.Fatalf("the monitor's environment does not carry %s; it would start as a second application", monitorEnv)
	}
	for _, name := range []string{`C:\x\wslcomms.test.exe`, "/tmp/go-build/b001/wslcomms.test"} {
		if !isGoTestBinary(name) {
			t.Errorf("isGoTestBinary(%q) = false", name)
		}
	}
	if isGoTestBinary(`C:\x\wslcomms.exe`) {
		t.Error("isGoTestBinary(wslcomms.exe) = true")
	}
}

// waitForCond polls cond until it holds or the deadline passes.
func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRefreshMonitorPictureKicksARunningMonitorAndOpensAClosedOne(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	made := withFakeMonitors(a)

	// Nothing running: the refresh opens a monitor, the kick from further back.
	if err := a.RefreshMonitorPicture(); err != nil {
		t.Fatalf("RefreshMonitorPicture() with no monitor: %v", err)
	}
	waitForCond(t, "a monitor to be running", func() bool {
		return len(made()) == 1 && a.GetMonitorState().Process == monitorProcessRunning
	})

	// Running: the refresh is an event to its page — and no second process.
	if err := a.RefreshMonitorPicture(); err != nil {
		t.Fatalf("RefreshMonitorPicture() with a monitor: %v", err)
	}
	waitForCond(t, "the refresh event to reach the monitor", func() bool {
		for _, n := range made()[0].sentNames() {
			if n == EventMonitorRefresh {
				return true
			}
		}
		return false
	})
	time.Sleep(50 * time.Millisecond)
	if n := len(made()); n != 1 {
		t.Fatalf("%d monitors launched, want 1: a refresh is an event, not a restart", n)
	}
}
