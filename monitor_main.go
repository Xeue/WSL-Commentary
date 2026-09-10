//go:build dev || production || bindings

// monitor_main.go is the PGM monitor process's main: a second Wails window,
// in a second process, from the same executable.
//
// Owner: WP-P. main.go dispatches here when WSLCOMMS_MONITOR is set, after
// gst.Init and before anything that would make this process a second copy of
// the application — no single-instance lock, no capture, no remote listener.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"wslcomms/internal/monitorlink"
)

const (
	monitorWindowWidth     = 1180
	monitorWindowHeight    = 720
	monitorWindowMinWidth  = 720
	monitorWindowMinHeight = 440
)

// runMonitorAndExit is the monitor process. It never returns: it leaves
// through forceExit on every path, for the reason the application does — the
// GStreamer and GPU-driver DLLs this process loads can deadlock in
// DLL_PROCESS_DETACH, and TerminateProcess is the exit that skips it.
//
// stdout is the link and nothing else writes to it: Wails' own logging is
// routed into the standard log, which main.go has already pointed at this
// process's log file.
func runMonitorAndExit(gstInitErr error) {
	link := monitorlink.NewClient(os.Stdin, os.Stdout)
	app := newMonitorApp(link, gstInitErr)

	err := wails.Run(&options.App{
		Title:            monitorWindowTitle,
		Width:            monitorWindowWidth,
		Height:           monitorWindowHeight,
		MinWidth:         monitorWindowMinWidth,
		MinHeight:        monitorWindowMinHeight,
		BackgroundColour: options.NewRGB(backgroundR, backgroundG, backgroundB),
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup:        app.startup,
		OnDomReady:       app.domReady,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
		WindowStartState: options.Normal,

		// Closing the window ends the process, exactly as the application's
		// does; the application sees the exit and offers to open it again.
		HideWindowOnClose: false,
		// No right-click "Reload" on a window the commentator is watching.
		EnableDefaultContextMenu: false,

		Logger:             stdLogger{},
		LogLevel:           logger.INFO,
		LogLevelProduction: logger.ERROR,

		Menu:    applicationMenu(),
		Mac:     macOptions(),
		Windows: monitorWindowsOptions(),
	})
	if err != nil {
		log.Printf("wslcomms: monitor: wails.Run returned an error: %v", err)
		fmt.Fprintln(os.Stderr, "wslcomms: monitor:", err)
	}

	app.teardown()
	forceExit()
}

// monitorWindowsOptions gives the monitor's WebView2 a user-data folder of its
// own. Two processes can share one folder only if they create their
// environments identically, and a mismatch is a window that opens empty with
// nothing in the log; a folder each is the answer that cannot go wrong.
func monitorWindowsOptions() *windows.Options {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return nil
	}
	return &windows.Options{
		WebviewUserDataPath: filepath.Join(base, "WSLComms", "webview2-pgm-monitor"),
	}
}

// stdLogger routes Wails' logger into the standard log — the process's log
// file — so that nothing Wails prints can land on stdout, which is the link.
type stdLogger struct{}

func (stdLogger) Print(m string)   { log.Print("wails: ", m) }
func (stdLogger) Trace(m string)   { log.Print("wails: TRACE ", m) }
func (stdLogger) Debug(m string)   { log.Print("wails: DEBUG ", m) }
func (stdLogger) Info(m string)    { log.Print("wails: ", m) }
func (stdLogger) Warning(m string) { log.Print("wails: WARNING ", m) }
func (stdLogger) Error(m string)   { log.Print("wails: ERROR ", m) }
func (stdLogger) Fatal(m string)   { log.Print("wails: FATAL ", m) }
