//go:build dev || production || bindings

// app_monitor.go is the application's half of the PGM MONITOR: launching the
// monitor process, keeping it fed, answering it, and restarting it.
//
// Owner: WP-P, with monitor_app.go (the monitor's half) and
// internal/monitorlink (the wire).
//
// # What the application does for the monitor
//
//   - LAUNCHES it at domReady — this same executable with WSLCOMMS_MONITOR set —
//     and relaunches it after a crash, up to a few times a minute, so a monitor
//     that dies mid-match comes back on its own. A monitor the operator CLOSED
//     stays closed; the lamp says so and the button opens it again.
//
//   - RELAYS every event its own page hears over the link, so the monitor's
//     page subscribes to "levels", "config", "status" and the rest exactly as
//     it would in this window. The pump's tee does it.
//
//   - ANSWERS its calls through the SAME allowlist the LAN bridge uses, as the
//     client "pgm-monitor": GetConfig, SaveConfig, GetKVSCredentials,
//     ListOutputDevices. Nothing host-only is reachable that way, and nothing
//     needs to be. The one exception is PictureOpts — host, port, latency, key
//     length and THE PASSPHRASE for the SRT picture — which is answered here,
//     on the link only, and is not in the allowlist: it must never be reachable
//     from the network.
//
//   - RESTARTS it on demand: RestartMonitor, host-only, is "kill it and start
//     a fresh one", the remedy for a monitor whose page or decoder has wedged.
//
// # What this replaces
//
// The picture used to be a pipeline in this process rendering into an overlay
// over this window; then, briefly (1.6.0), a child process with a window of
// its own. The mosaic and the return audio were a WebRTC connection in this
// window's page, with no way to restart them short of restarting the
// application. All of that is in the monitor process now, and this file is
// what remains of the picture here: the options and the launcher.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
	"wslcomms/internal/monitorlink"
	"wslcomms/internal/remote"
	"wslcomms/internal/secrets"
)

const (
	// monitorEnv turns this executable into the monitor process. Its presence
	// is the signal; its value is irrelevant.
	monitorEnv = "WSLCOMMS_MONITOR"

	// monitorWindowTitle is the monitor window's title. The monitor's overlay
	// finds its host window by it, so the two must agree; both read this.
	monitorWindowTitle = "WSL Commentary — PGM Monitor"

	// monitorClientID is how the monitor's calls appear to the allowlist and
	// how its saves are marked as the "config" event's origin, so each page
	// can tell its own save's echo from another seat's.
	monitorClientID = "pgm-monitor"

	// EventMonitor carries a monitorPayload: whether the monitor PROCESS is
	// running and what its KVS connection is doing. It is what drives the
	// MONITOR lamp now that the connection lives in the other window.
	EventMonitor = "monitor"

	// EventMonitorRefresh is sent to the MONITOR PROCESS ONLY, over the link,
	// by RefreshMonitorPicture: its page runs its own Refresh button. It is
	// never emitted to this window's page or broadcast to remote seats.
	EventMonitorRefresh = "monitorRefresh"

	monitorReadyTimeout = 30 * time.Second
	monitorStopBudget   = 3 * time.Second

	// A monitor that keeps crashing is relaunched at most monitorRelaunchMax
	// times in monitorRelaunchWindow, then left down with the lamp red.
	monitorRelaunchWindow = time.Minute
	monitorRelaunchMax    = 3
	monitorRelaunchDelay  = time.Second
)

// The monitor process's states, as the page sees them.
const (
	monitorProcessStarting = "starting"
	monitorProcessRunning  = "running"
	monitorProcessClosed   = "closed" // the operator closed its window, or it was never opened
	monitorProcessFailed   = "failed" // it could not start, or crashed past the relaunch budget
)

// monitorPayload is the "monitor" event and GetMonitorState's answer.
type monitorPayload struct {
	Process string `json:"process"`
	KVS     string `json:"kvs"`
	PID     int    `json:"pid,omitempty"`
}

// pictureOptsPayload is PictureOpts' answer over the link: gst.PictureOpts
// minus the window handle, which the monitor supplies from its own overlay.
// It carries the passphrase and travels on the link and nowhere else.
type pictureOptsPayload struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	LatencyMs  int    `json:"latencyMs"`
	Passphrase string `json:"passphrase"`
	PBKeyLen   int    `json:"pbKeyLen"`
}

// monitorHost is what the application needs of a launched monitor process.
// *monitorlink.Host is the real one; the tests substitute a fake through
// App.monitorDial.
type monitorHost interface {
	Start() error
	Send(name string, data any)
	Stop()
	Kill()
	Done() <-chan struct{}
	ExitCode() int
	PID() int
}

func hostExited(h monitorHost) bool {
	select {
	case <-h.Done():
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Launching
// ---------------------------------------------------------------------------

// newMonitorHost builds the link's parent end over a fresh process, through
// the monitorDial seam in tests.
func (a *App) newMonitorHost() monitorHost {
	if a.monitorDial != nil {
		return a.monitorDial()
	}
	return monitorlink.NewHost(monitorlink.HostOptions{
		Spawn:        a.monitorCommand,
		Dispatch:     a.monitorDispatch,
		OnEvent:      a.monitorEvent,
		Logf:         log.Printf,
		ReadyTimeout: monitorReadyTimeout,
		StopBudget:   monitorStopBudget,
	})
}

// monitorCommand is this executable, marked as the monitor. It REFUSES a Go
// test binary: launched, a test binary runs its whole suite, which would
// launch it again — a fork bomb found the hard way once. The tests install a
// fake host and never come here on purpose; this is the belt.
func (a *App) monitorCommand() (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("wslcomms: cannot find this executable to launch the monitor: %w", err)
	}
	if isGoTestBinary(exe) {
		return nil, fmt.Errorf("wslcomms: refusing to launch %q as the monitor: it is a test binary, "+
			"and a test binary launched runs its whole suite; install a monitorDial fake", filepath.Base(exe))
	}
	return monitorProcessCommand(exe), nil
}

// monitorProcessCommand builds the monitor's command for a given executable:
// no arguments, and the environment with monitorEnv set.
func monitorProcessCommand(exe string) *exec.Cmd {
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), monitorEnv+"=1")
	return cmd
}

// isGoTestBinary reports whether exe is named the way `go test` names the
// binaries it builds: <package>.test or <package>.test.exe.
func isGoTestBinary(exe string) bool {
	base := strings.ToLower(filepath.Base(exe))
	return strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe")
}

// launchMonitor starts a monitor process if none is running. It returns at
// once; the start, the wait for ready and the watch for exit run on their own
// goroutine, and the page hears about each on the "monitor" event.
func (a *App) launchMonitor() {
	a.monMu.Lock()
	if a.closing.Load() {
		a.monMu.Unlock()
		return
	}
	if a.mon != nil && !hostExited(a.mon) {
		// Already running: say so, for a page that has just (re)loaded and
		// asked through domReady.
		a.monMu.Unlock()
		a.publishMonitorState()
		return
	}
	host := a.newMonitorHost()
	a.monGen++
	gen := a.monGen
	a.mon = host
	a.monStopping = false
	a.monProcess = monitorProcessStarting
	a.monKVS = ""
	a.monMu.Unlock()
	a.publishMonitorState()

	if !a.rootGo(1) {
		return
	}
	go func() {
		defer a.rootWG.Done()
		a.runMonitor(host, gen)
	}()
}

// runMonitor starts one monitor process and follows it to its exit. gen is
// the launch generation; a host superseded by a restart stops updating the
// shared state the moment it notices.
func (a *App) runMonitor(host monitorHost, gen int) {
	if err := host.Start(); err != nil {
		// The error goes out BEFORE the state, so that whoever sees the lamp
		// turn red finds the reason already on the banner.
		a.emitError(fmt.Errorf("wslcomms: the PGM monitor window could not be opened: %w", err))
		a.monMu.Lock()
		if a.monGen == gen {
			a.monProcess = monitorProcessFailed
		}
		a.monMu.Unlock()
		a.publishMonitorState()
		return
	}

	a.monMu.Lock()
	if a.monGen == gen {
		a.monProcess = monitorProcessRunning
	}
	a.monMu.Unlock()
	log.Printf("wslcomms: the PGM monitor window is up (pid %d)", host.PID())
	a.publishMonitorState()

	<-host.Done()

	a.monMu.Lock()
	if a.monGen != gen {
		a.monMu.Unlock()
		return
	}
	stopping := a.monStopping
	code := host.ExitCode()
	relaunch := false
	switch {
	case stopping || a.closing.Load():
		a.monProcess = monitorProcessClosed
	case code == 0:
		// A clean exit with nobody having asked: the operator closed the
		// window. Respect it.
		a.monProcess = monitorProcessClosed
		log.Print("wslcomms: the PGM monitor window was closed")
	default:
		// A crash. Relaunch, within the budget.
		now := time.Now()
		recent := a.monRelaunches[:0]
		for _, t := range a.monRelaunches {
			if now.Sub(t) < monitorRelaunchWindow {
				recent = append(recent, t)
			}
		}
		a.monRelaunches = recent
		if len(a.monRelaunches) < monitorRelaunchMax {
			a.monRelaunches = append(a.monRelaunches, now)
			relaunch = true
			a.monProcess = monitorProcessStarting
		} else {
			a.monProcess = monitorProcessFailed
		}
	}
	a.monKVS = ""
	process := a.monProcess
	a.monMu.Unlock()

	// Errors before the state, for the reason above.
	if relaunch {
		a.emitError(fmt.Errorf("wslcomms: the PGM monitor window exited unexpectedly (code %d); reopening it", code))
		a.publishMonitorState()
		time.Sleep(monitorRelaunchDelay)
		a.launchMonitor()
		return
	}
	if process == monitorProcessFailed {
		a.emitError(fmt.Errorf("wslcomms: the PGM monitor window has crashed %d times in a minute (last code %d) "+
			"and is staying closed; use Restart monitor when ready", monitorRelaunchMax, code))
	}
	a.publishMonitorState()
}

// RestartMonitor ends the monitor process — killed, if it will not stop — and
// starts a fresh one. It is the operator's answer to a monitor whose page or
// decoder has wedged, and it is safe mid-match: nothing about the
// contribution feed is touched. A monitor that was not running is simply
// opened. Host-only: it opens and closes a window on this machine's screen.
func (a *App) RestartMonitor() error {
	if a.closing.Load() {
		return errShuttingDown
	}
	a.monMu.Lock()
	host := a.mon
	a.monStopping = true
	a.monMu.Unlock()
	if host != nil && !hostExited(host) {
		log.Print("wslcomms: restarting the PGM monitor window")
		host.Stop()
	}
	a.launchMonitor()
	return nil
}

// RefreshMonitorPicture gives the PGM monitor's picture the kick its own
// Refresh button gives — the mosaic reconnected, the SRT picture restarted,
// whichever is active — from anywhere else: this window's card, or a remote
// seat's browser. It is an EVENT to the monitor's page, which runs exactly its
// onPictureRefresh; nothing about the contribution feed is touched. A monitor
// that is not running is opened instead, which is the same kick from further
// back. Reachable from remote seats on purpose (app_remote.go).
func (a *App) RefreshMonitorPicture() error {
	if a.closing.Load() {
		return errShuttingDown
	}
	a.monMu.Lock()
	host := a.mon
	a.monMu.Unlock()
	if host != nil && !hostExited(host) {
		log.Print("wslcomms: refreshing the PGM monitor's picture")
		host.Send(EventMonitorRefresh, nil)
		return nil
	}
	log.Print("wslcomms: a picture refresh was asked for with no PGM monitor running; opening one")
	a.launchMonitor()
	return nil
}

// GetMonitorState is the "monitor" event's payload, for a page that has just
// loaded.
func (a *App) GetMonitorState() monitorPayload {
	a.monMu.Lock()
	defer a.monMu.Unlock()
	return a.monitorStateLocked()
}

func (a *App) monitorStateLocked() monitorPayload {
	p := monitorPayload{Process: a.monProcess, KVS: a.monKVS}
	if p.Process == "" {
		p.Process = monitorProcessClosed
	}
	if a.mon != nil && !hostExited(a.mon) {
		p.PID = a.mon.PID()
	}
	return p
}

func (a *App) publishMonitorState() {
	a.monMu.Lock()
	p := a.monitorStateLocked()
	a.monMu.Unlock()
	a.events.send(EventMonitor, p)
}

// stopMonitorForTeardown is the monitor's step of the ordered shutdown: close
// its lifeline, wait the stop budget, kill it if it has not gone.
func (a *App) stopMonitorForTeardown() error {
	a.monMu.Lock()
	host := a.mon
	a.monStopping = true
	a.monMu.Unlock()
	if host != nil {
		host.Stop()
	}
	return nil
}

// ---------------------------------------------------------------------------
// The link: events out, calls and events in
// ---------------------------------------------------------------------------

// teeEvents is the pump's tee: every event this window's page hears also goes
// to the LAN bridge and to the monitor.
func (a *App) teeEvents(name string, data any) {
	a.broadcastRemote(name, data)
	a.monitorSend(name, data)
}

// monitorSend relays one event to the running monitor, if any. It never
// blocks: the link queues and drops the oldest.
func (a *App) monitorSend(name string, data any) {
	a.monMu.Lock()
	host := a.mon
	a.monMu.Unlock()
	if host != nil {
		host.Send(name, data)
	}
}

// monitorDispatch answers the monitor's calls. PictureOpts is answered here
// and only here; everything else goes through the allowlist as the client
// "pgm-monitor", so what the monitor can reach is exactly what a remote seat
// can, and nothing host-only leaks through the back of the application.
func (a *App) monitorDispatch(ctx context.Context, method string, args []json.RawMessage) (any, error) {
	if method == "PictureOpts" {
		return a.monitorPictureOpts()
	}
	client := remote.ClientInfo{ID: monitorClientID, RemoteAddr: "pgm-monitor"}
	return a.remoteCall(ctx, client, method, args)
}

// monitorPictureOpts is what the monitor dials, with the application's own
// refusals applied first.
func (a *App) monitorPictureOpts() (pictureOptsPayload, error) {
	cfg := a.snapshotConfig()

	// The picture and the SRT AUDIO return dial THE SAME M2L-X OUTPUT, which
	// accepts one connection and never displaces it. Refuse, and say what to
	// change: the audio comes from Kinesis; SRT carries the picture.
	if cfg.UsesSRTReturn() {
		return pictureOptsPayload{}, fmt.Errorf(
			"returnSource is %q, so the SRT audio return is dialling the same M2L-X output "+
				"(port %d) that the picture needs. That output accepts one connection and never "+
				"displaces it. Set returnSource to %q on the Settings screen — the commentator's "+
				"AUDIO comes from Kinesis, and SRT carries the PICTURE",
			cfg.EffectiveReturnSource(), cfg.EffectiveSRTReturnPort(), config.ReturnSourceWebRTC)
	}
	passphrase, err := a.picturePassphrase(cfg)
	if err != nil {
		return pictureOptsPayload{}, err
	}
	opts := a.pictureOpts(cfg, passphrase)
	return pictureOptsPayload{
		Host:       opts.Host,
		Port:       opts.Port,
		LatencyMs:  opts.LatencyMs,
		Passphrase: opts.Passphrase,
		PBKeyLen:   opts.PBKeyLen,
	}, nil
}

// monitorEvent receives the monitor's events. The one it sends is "monitor":
// the KVS connection's state, a string, folded into this side's payload.
func (a *App) monitorEvent(name string, data json.RawMessage) {
	if name != EventMonitor {
		log.Printf("wslcomms: the PGM monitor sent an unexpected %q event", name)
		return
	}
	var kvsState string
	if err := json.Unmarshal(data, &kvsState); err != nil {
		log.Printf("wslcomms: the PGM monitor's state was not readable: %v", err)
		return
	}
	a.monMu.Lock()
	a.monKVS = kvsState
	a.monMu.Unlock()
	a.publishMonitorState()
}

// ---------------------------------------------------------------------------
// The picture's options, which stay with the configuration
// ---------------------------------------------------------------------------

// pictureOpts builds the picture's options from a configuration snapshot and
// the passphrase already read from the credential store. It is separate from
// its caller so that what the monitor is given can be asserted without
// running one. The passphrase is a secret: it goes to the monitor on the
// link, is set with g_object_set rather than in a URI, and must never be
// logged or returned across the Wails boundary. There is no WindowHandle: the
// monitor's overlay supplies it.
func (a *App) pictureOpts(cfg *config.Config, passphrase string) gst.PictureOpts {
	return gst.PictureOpts{
		// EffectiveSRTReturnHost, not EffectiveSRTHost: the picture is a RETURN
		// and follows the return override to the relay when one is set.
		Host: cfg.EffectiveSRTReturnHost(),
		// 40501 by default: Output 1, src=pgm, the programme picture. It is the
		// AUDIO return's config field being read for the picture — an interface
		// gap, reported rather than worked around.
		Port: cfg.EffectiveSRTReturnPort(),
		// EffectivePictureLatencyMs, NOT SRTLatencyMs: the contribution feed's
		// retransmission budget is a different number pulling the other way.
		LatencyMs:  cfg.EffectivePictureLatencyMs(),
		Passphrase: passphrase,
		PBKeyLen:   cfg.SRTReturnPBKeyLen,
	}
}

// picturePassphrase reads the SRT passphrase for the programme output from the
// credential store — secrets.KeySRTReturn, the same M2L-X output the audio
// return used — and refuses the one combination that cannot work: a non-zero
// key length with no stored passphrase. The value is a secret and must never
// reach a log line, an error string or the Wails boundary.
func (a *App) picturePassphrase(cfg *config.Config) (string, error) {
	passphrase, err := a.store.Get(secrets.KeySRTReturn)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		if cfg.SRTReturnPBKeyLen != 0 {
			return "", fmt.Errorf(
				"wslcomms: cannot start the picture: srtReturnPBKeyLen is %d, which asks for an "+
					"encrypted session with the M2L-X output on port %d, but no passphrase is stored "+
					"in %s under %q — enter it on the Settings screen, or set the key "+
					"length to 0 if that output is not encrypted",
				cfg.SRTReturnPBKeyLen, cfg.EffectiveSRTReturnPort(), secrets.StoreName(), secrets.TargetSRTReturn)
		}
		return "", nil
	case err != nil:
		return "", fmt.Errorf("wslcomms: reading the picture's SRT passphrase from %q: %w",
			secrets.TargetSRTReturn, err)
	}
	return passphrase, nil
}
