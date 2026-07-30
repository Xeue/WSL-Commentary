//go:build cgo

// Command wslcomms is the WSL Studios commentary contribution application: one
// window, one process, one installer.
//
// Owner: WP-8.
//
// This file is behind //go:build cgo because internal/gst's real implementation
// links GStreamer through cgo. main_nocgo.go supplies a main for CGO_ENABLED=0
// builds so that `go build ./...` stays honest at Gate A, where there is no
// MinGW gcc.
//
// # Startup order, which is not negotiable
//
//  1. WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS, before anything creates a window.
//  2. gst.Init(appDir), before anything else touches GStreamer.
//  3. NewApp, then wails.Run.
//
// Steps 1 and 2 both write process environment variables that a later
// subsystem reads exactly once, so both have to happen before that subsystem
// exists. See setWebView2Arguments and internal/gst's Init.
package main

import (
	"embed"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"wslcomms/internal/gst"
)

// assets is the built frontend, embedded into the executable. Wails locates
// index.html inside it and serves the tree containing it, so no fs.Sub is
// needed here.
//
// The `all:` prefix is required: without it go:embed skips files whose names
// begin with an underscore or a dot, and Vite's output has been known to
// contain both.
//
//go:embed all:frontend/dist
var assets embed.FS

// Window geometry. Specification section 10: one window, the PGM tile filling
// the top scaled to the window width, then one row of controls and the status
// lamps, then the permanent honest line.
//
// The tile's natural size is the 640x360 crop of the KVS mosaic
// (config.DefaultMonitorTile). 1024 wide scales it 1.6x; the remaining height is
// the three control rows, the lamp row and the honest line. The minimums are
// where that layout stops being readable rather than where it stops fitting.
const (
	windowWidth     = 1024
	windowHeight    = 820
	windowMinWidth  = 800
	windowMinHeight = 620

	// windowTitle matches the <title> in frontend/index.html and the header in
	// specification section 10.
	windowTitle = "WSL Commentary"
)

// backgroundRGB is --bg from frontend/src/styles/main.css, #0b0d10. Setting it
// here as well as in CSS stops the window flashing white between being shown and
// the first paint of the embedded page.
const (
	backgroundR = 0x0b
	backgroundG = 0x0d
	backgroundB = 0x10
)

func main() {
	setWebView2Arguments()

	dir, err := appDir()
	if err != nil {
		// Without the executable's directory there is no bundled GStreamer to
		// point at and no slate to send. This is the one failure early enough,
		// and total enough, to be worth refusing to start for.
		fmt.Fprintln(os.Stderr, "wslcomms:", err)
		os.Exit(1)
	}

	// gst.Init must precede every other gst call: it sets
	// GST_PLUGIN_SYSTEM_PATH_1_0, GST_PLUGIN_PATH_1_0 and GST_REGISTRY_1_0 and
	// then calls gst_init, which scans the plugin registry once. Setting those
	// afterwards has no effect, and the failure mode is silent — the app loads
	// whatever GStreamer happens to be on the machine instead of its own bundle.
	//
	// A failure here is carried into the App rather than being fatal. The app is
	// launched from a desktop shortcut, so a message on stderr goes nowhere; the
	// window opening and saying which plugin is missing is the diagnosable
	// outcome. See App.gstInitErr.
	gstInitErr := gst.Init(dir)
	if gstInitErr != nil {
		log.Printf("wslcomms: GStreamer initialisation failed: %v", gstInitErr)
		gstInitErr = fmt.Errorf(
			"the bundled GStreamer in %s could not be initialised, so audio cannot be sent: %w", dir, gstInitErr)
	}

	app := NewApp(dir, gstInitErr)
	// Belt and braces against wails.Run returning by a path that does not fire
	// OnShutdown. teardown is idempotent, so the two cannot both do the work.
	defer app.teardown()

	err = wails.Run(&options.App{
		Title:            windowTitle,
		Width:            windowWidth,
		Height:           windowHeight,
		MinWidth:         windowMinWidth,
		MinHeight:        windowMinHeight,
		BackgroundColour: options.NewRGB(backgroundR, backgroundG, backgroundB),
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
		WindowStartState: options.Normal,

		// Closing the window must end the process. There is one window, no menu
		// and no tray icon (specification section 10), so a hidden window would
		// be a wslcomms.exe nobody can see and nobody can close.
		HideWindowOnClose: false,

		// No right-click "Reload" / "Inspect" on a live contribution window.
		// This is already the production default; it is stated because the
		// commentary position is operated by a commentator, not an engineer,
		// and an accidental reload mid-match drops the monitor and the lamps.
		EnableDefaultContextMenu: false,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "wslcomms:", err)
		os.Exit(1)
	}
}

// setWebView2Arguments sets the Chromium command line WebView2 is created with.
//
// This MUST happen before the window exists. WebView2 reads
// WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS once, when the environment is created,
// and Wails creates that environment inside wails.Run.
//
// Both flags are load-bearing, from specification section 7:
//
//   - --auto-accept-camera-and-microphone-capture grants the media-device
//     permission that navigator.mediaDevices.enumerateDevices() requires before
//     it will return device LABELS. Without it the call still succeeds and still
//     returns the right number of devices, but every label is the empty string,
//     so the headphone dropdown is a list of blanks and requirement R4 cannot be
//     satisfied by the operator. This is the single most easily missed line in
//     the whole application.
//
//   - --autoplay-policy=no-user-gesture-required lets the KVS return audio and
//     the programme video start playing without a click. The monitor page starts
//     itself when credentials arrive, which is not a user gesture, so without
//     this the commentator hears nothing until they happen to click the page.
//
// Go's os.Setenv calls SetEnvironmentVariableW, which is what the WebView2
// loader reads, so this crosses into the native side cleanly.
func setWebView2Arguments() {
	const key = "WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS"
	const value = "--autoplay-policy=no-user-gesture-required " +
		"--auto-accept-camera-and-microphone-capture"

	if err := os.Setenv(key, value); err != nil {
		// Non-fatal, but the operator needs to know why the headphone dropdown
		// is empty, so say so loudly rather than letting it look like a driver
		// problem.
		log.Printf("wslcomms: could not set %s (%v); "+
			"enumerateDevices will return blank device labels and the headphone dropdown will be unusable", key, err)
	}
}

// appDir returns the directory holding this executable, with symlinks resolved.
//
// It is where the bundled GStreamer (<appDir>\gst\) and the default slate.png
// live, both laid down by the installer beside wslcomms.exe (specification
// section 11). Symlinks are resolved because a shortcut target or a junctioned
// Program Files would otherwise put appDir somewhere that has neither.
//
// A symlink that cannot be resolved is not fatal: the unresolved path is used
// instead, on the grounds that a path that is merely unresolved is far more
// likely to be right than no path at all, and gst.Init will say so clearly if it
// is not.
func appDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("could not determine the executable's own path: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		log.Printf("wslcomms: could not resolve symlinks in %q (%v); using it as-is", exe, err)
		resolved = exe
	}

	dir := filepath.Dir(resolved)
	if dir == "" {
		return "", errors.New("the executable's path has no directory component")
	}
	return dir, nil
}
