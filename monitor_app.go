//go:build dev || production || bindings

// monitor_app.go is the PGM MONITOR process: the Go side of the second window.
//
// Owner: WP-P, with app_monitor.go (the application's half) and
// internal/monitorlink (the wire between them).
//
// # What the monitor is
//
// The commentator's PROGRAMME PICTURE, the RETURN AUDIO in their headphones and
// the INPUT METERS, in a window of their own, run by a process of their own.
// The application launches it at startup, relays it every event its own page
// hears, answers its calls through the same allowlist the LAN bridge uses, and
// can kill and relaunch it — "Restart monitor" — without touching the
// contribution feed, the capture or the mixer. Inside the window, "Refresh"
// gives whichever picture is active a kick: the mosaic's WebRTC connection is
// rebuilt, or the SRT pipeline restarted.
//
// It exists because a frozen mosaic, a distorted return or a wedged decoder
// used to mean restarting the whole application mid-match. Now they mean one
// button, and the worst case — a monitor process that will not answer — is one
// TerminateProcess away from a fresh one.
//
// # What runs here and what does not
//
// This process runs a Wails window on the SAME embedded frontend as the
// application; the page notices it is bound to MonitorApp rather than App and
// mounts the monitor view. It runs the SRT picture pipeline in-process,
// rendering into an overlay over its own window — the design the application
// itself used before the picture moved out — and nothing else: no capture, no
// send, no mixer, no remote listener, no single-instance lock, no
// configuration file of its own.
//
// Everything the page asks for that belongs to the application — GetConfig,
// SaveConfig, GetKVSCredentials, ListOutputDevices — is a bound method here
// that forwards over the link; the application answers through its
// allowlist as if a remote seat had asked, with one private exception,
// PictureOpts, which carries the SRT passphrase and is never on the network.
//
// # The lifeline
//
// The link's stdin is the process's reason to exist. When it closes the
// window is asked to quit, which runs the ordered teardown (stop the
// pipeline, close the overlay) and ends the process through forceExit. The
// operator closing the window runs the same path; the application sees the
// exit and shows "monitor closed" with a button to open it again.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
	"wslcomms/internal/kvs"
	"wslcomms/internal/monitorlink"
)

const (
	// monitorCallTimeout bounds one forwarded call. The credential fetch
	// talks to M2L-X and can take seconds; everything else answers at once.
	monitorCallTimeout = 20 * time.Second

	// monitorPictureStopBudget bounds the picture step of this process's
	// teardown, for the reason the application bounds its preview step:
	// gst_element_set_state cannot be interrupted, and a wedged decoder
	// would otherwise hold the process open. Past it the step is abandoned
	// and the process ends through forceExit.
	monitorPictureStopBudget = 4 * time.Second

	// monitorShutdownTimeout bounds the whole teardown.
	monitorShutdownTimeout = 8 * time.Second
)

// MonitorApp is the bound object of the monitor window. Its exported methods
// are what the page can call; see the file header for which are forwarded.
type MonitorApp struct {
	link *monitorlink.Client

	events *eventPump
	ctx    atomic.Pointer[context.Context]

	rootCtx    context.Context
	rootCancel context.CancelFunc
	rootWG     sync.WaitGroup

	closing      atomic.Bool
	shutdownOnce sync.Once
	exitProcess  func()

	gstInitErr error

	// cfg is the latest configuration the application relayed: fetched once
	// at startup and replaced on every "config" event. It is what the picture
	// is started against and what GetConfig hands the page.
	cfgMu sync.Mutex
	cfg   *config.Config

	// The SRT picture. The same shape the application had before the picture
	// moved here, with the same lock order: picMu → picViewMu → picStateMu.
	// See monitor_picture.go.
	picMu                   sync.Mutex
	pic                     *pictureSession
	pictureSoftwareNoteOnce sync.Once
	picViewMu               sync.Mutex
	picOverlay              gst.PictureOverlay
	picRect                 gst.PictureRect
	picWantVisible          bool
	picStateMu              sync.Mutex
	lastPicture             gst.PictureState

	// Test seams. Nil means the real thing: gst's monitor and overlay, and
	// the application over the link.
	pictureDial     func() gst.PictureMonitor
	overlayDial     func() (gst.PictureOverlay, error)
	pictureOptsDial func() (gst.PictureOpts, error)
}

// newMonitorApp builds the monitor's bound object over an established link.
func newMonitorApp(link *monitorlink.Client, gstInitErr error) *MonitorApp {
	ctx, cancel := context.WithCancel(context.Background())
	m := &MonitorApp{
		link:        link,
		events:      newEventPump(),
		rootCtx:     ctx,
		rootCancel:  cancel,
		exitProcess: forceExit,
		gstInitErr:  gstInitErr,
	}
	return m
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// startup runs before the page exists: it fetches the configuration, starts
// relaying the application's events into this window's pump, and arms the
// lifeline.
func (m *MonitorApp) startup(ctx context.Context) {
	m.ctx.Store(&ctx)

	if m.gstInitErr != nil {
		m.emitError(m.gstInitErr)
	}

	var cfg config.Config
	if err := m.call("GetConfig", &cfg); err != nil {
		m.emitError(fmt.Errorf("wslcomms: the monitor could not read the configuration from the application: %w", err))
	} else {
		m.cfgMu.Lock()
		m.cfg = &cfg
		m.cfgMu.Unlock()
	}

	m.rootWG.Add(2)
	go func() {
		defer m.rootWG.Done()
		m.relay()
	}()
	go func() {
		defer m.rootWG.Done()
		select {
		case <-m.rootCtx.Done():
		case <-m.link.Done():
			// The application stopped us, restarted us, or died. Either way
			// the window has no reason to exist; Quit runs the ordered
			// teardown through OnShutdown.
			log.Print("wslcomms: monitor: the link to the application closed; quitting")
			if wctx, ok := m.runtimeContext(); ok {
				wailsruntime.Quit(wctx)
			} else {
				m.teardown()
			}
		}
	}()
}

// domReady runs when the page is listening: the pump starts, the last picture
// state is replayed, and the application is told the monitor is ready.
func (m *MonitorApp) domReady(ctx context.Context) {
	m.events.start(m.rootCtx, ctx, &m.rootWG)

	m.picStateMu.Lock()
	lastPic := m.lastPicture
	m.picStateMu.Unlock()
	m.events.send(EventPicture, lastPic)

	m.link.Ready()
}

func (m *MonitorApp) shutdown(_ context.Context) { m.teardown() }

func (m *MonitorApp) runtimeContext() (context.Context, bool) {
	p := m.ctx.Load()
	if p == nil {
		return nil, false
	}
	return *p, true
}

// teardown is the ordered shutdown: the picture first, because its pipeline
// renders into the overlay, then the process. It is bounded, like the
// application's, and abandons a step that will not return rather than hang
// the process on a wedged decoder — which is the very case the application
// kills this process for.
func (m *MonitorApp) teardown() {
	m.shutdownOnce.Do(func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			m.closing.Store(true)
			m.rootCancel()
			if !teardownStep("the monitor's picture", monitorPictureStopBudget, m.stopPictureForTeardown) {
				log.Print("wslcomms: monitor: the picture would not stop; ending the process rather than waiting")
			}
		}()
		select {
		case <-done:
		case <-time.After(monitorShutdownTimeout):
			log.Printf("wslcomms: monitor: shutdown did not complete within %s; exiting anyway", monitorShutdownTimeout)
		}
		m.exitProcess()
	})
}

// relay pumps the application's events into this window and keeps the
// configuration snapshot current. Every event is passed through under its
// own name, so the page subscribes exactly as it does in the application's
// window; the data is forwarded as the JSON it arrived as.
func (m *MonitorApp) relay() {
	for {
		select {
		case <-m.rootCtx.Done():
			return
		case ev, ok := <-m.link.Events():
			if !ok {
				return
			}
			if ev.Name == EventConfig {
				var payload struct {
					Config *config.Config `json:"config"`
				}
				if err := json.Unmarshal(ev.Data, &payload); err == nil && payload.Config != nil {
					m.cfgMu.Lock()
					m.cfg = payload.Config
					m.cfgMu.Unlock()
				}
			}
			m.events.send(ev.Name, ev.Data)
		}
	}
}

// call forwards one method to the application and decodes its result into
// out (which may be nil for a method that returns nothing).
func (m *MonitorApp) call(method string, out any, args ...any) error {
	ctx, cancel := context.WithTimeout(m.rootCtx, monitorCallTimeout)
	defer cancel()
	raw, err := m.link.Call(ctx, method, args...)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("wslcomms: monitor: decoding the result of %s: %w", method, err)
	}
	return nil
}

// snapshotConfig is the latest relayed configuration, or the defaults before
// one has arrived.
func (m *MonitorApp) snapshotConfig() *config.Config {
	m.cfgMu.Lock()
	defer m.cfgMu.Unlock()
	if m.cfg == nil {
		return config.Defaults()
	}
	c := *m.cfg
	return &c
}

func (m *MonitorApp) emitError(err error) {
	if err == nil {
		return
	}
	log.Printf("wslcomms: monitor: %v", err)
	m.events.send(EventError, err.Error())
}

func (m *MonitorApp) emitNote(msg string) {
	if msg == "" {
		return
	}
	log.Printf("wslcomms: monitor: %s", msg)
	m.events.send(EventNote, msg)
}

// ---------------------------------------------------------------------------
// The bound surface: forwarded to the application
// ---------------------------------------------------------------------------

// GetConfig is the application's configuration, as the application answers it.
func (m *MonitorApp) GetConfig() (*config.Config, error) {
	var cfg config.Config
	if err := m.call("GetConfig", &cfg); err != nil {
		return nil, err
	}
	m.cfgMu.Lock()
	m.cfg = &cfg
	m.cfgMu.Unlock()
	return &cfg, nil
}

// SaveConfig hands a configuration to the application to save, exactly as a
// remote seat's save is handled: the application validates it, refuses a
// change to the capture from anywhere but its own window, writes it, and
// emits the "config" event — which comes back here over the link.
func (m *MonitorApp) SaveConfig(c *config.Config) error {
	if c == nil {
		return errors.New("wslcomms: SaveConfig: no configuration supplied")
	}
	return m.call("SaveConfig", nil, c)
}

// GetKVSCredentials is the application's, forwarded.
func (m *MonitorApp) GetKVSCredentials() (kvs.Credentials, error) {
	var creds kvs.Credentials
	if err := m.call("GetKVSCredentials", &creds); err != nil {
		return kvs.Credentials{}, err
	}
	return creds, nil
}

// ListOutputDevices is the application's, forwarded.
func (m *MonitorApp) ListOutputDevices() ([]gst.Device, error) {
	var devices []gst.Device
	if err := m.call("ListOutputDevices", &devices); err != nil {
		return nil, err
	}
	return devices, nil
}

// ReportMonitorState tells the application what the KVS connection in this
// window is doing, so the MONITOR lamp in the application's window keeps
// telling the truth now that the connection lives here.
func (m *MonitorApp) ReportMonitorState(state string) error {
	return m.link.Emit(EventMonitor, state)
}
