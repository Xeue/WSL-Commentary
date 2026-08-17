//go:build dev || production || bindings

// Command wslcomms is the WSL Studios commentary contribution application: one
// window, one process, one installer.
//
// Owner: WP-8.
//
// This file is behind //go:build dev || production || bindings — the tags the
// Wails CLI sets for `wails dev`, `wails build` and its bindings step.
// main_nocgo.go supplies an inert main for every other build, which is what
// keeps a stray `go build .` from reaching Wails' modal build-tag dialog. The
// constraint does not require cgo, and what that buys differs by platform:
// Wails on Windows is pure Go, so this file is compiled and type-checked at
// Gate A with CGO_ENABLED=0, whereas Wails on macOS reaches Cocoa and WKWebView
// through Objective-C and its whole darwin frontend is behind cgo. So on macOS
//
//	CGO_ENABLED=0 go build -tags dev .
//
// does not fail in anything written here — it fails inside Wails, where
// frontend.go is excluded for want of cgo and the files that reference its
// Frontend type are not. Type-check this package on macOS with CGO_ENABLED=1
// and the tag; Gate A's untagged CGO_ENABLED=0 pass reaches main_nocgo.go
// instead and is unaffected either way. A real executable still needs Gate B on
// both platforms, and a production build without cgo fails deliberately in
// internal/gst/gst_stub_guard.go rather than shipping the stub.
//
// # Startup order, which is not negotiable
//
//  1. the webview's media-capture permission, before anything creates a window.
//  2. gst.Init(appDir), before anything else touches GStreamer.
//  3. NewApp, then wails.Run.
//
// Step 1 is a process environment variable on Windows and is not a variable at
// all on macOS — see setWebView2Arguments, which is where the difference is set
// out — but the ORDER holds on both, because on Windows the value is read
// exactly once when WebView2's environment is created inside wails.Run. Step 2
// writes environment variables that gst_init reads exactly once, so it has the
// same constraint for the same reason; see internal/gst's Init.
//
// # The four Wails callbacks, and why each one is wired
//
//	OnStartup    load config, bring up the control plane. Runs before the page
//	             exists, so it emits nothing — see App.startup.
//	OnDomReady   start the event pump. The page is now listening, so everything
//	             OnStartup queued is delivered here — see App.domReady.
//	OnShutdown   the ordered teardown — see App.teardown.
//	SingleInstanceLock
//	             a second launch hands over to the first and exits.
package main

import (
	"embed"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"

	"wslcomms/internal/applog"
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

	// aboutMessage is the body of the macOS About panel. See macOptions.
	//
	// It deliberately carries NO VERSION NUMBER. The version lives in wails.json
	// (info.productVersion), from which the Wails CLI stamps CFBundleVersion and
	// CFBundleShortVersionString into the bundle's Info.plist; macOS shows those
	// in Finder's Get Info and in the crash reporter. A copy here would be a
	// second place to remember to change and the first one to be wrong, and a
	// stale version in an About panel is worse than no version at all when
	// somebody is reading it out over the phone.
	aboutMessage = "Commentary contribution to the Sony M2L-X cloud switcher.\n" +
		"WSL Studios."

	// singleInstanceID names the OS-level lock that keeps exactly one wslcomms
	// running per user session. Any stable, unique string will do; this one is
	// namespaced so it cannot collide with another Wails application.
	//
	// IT IS NOT THE BUNDLE IDENTIFIER, and the difference is deliberate rather
	// than an oversight to be tidied. build/darwin/Info.plist sets
	// CFBundleIdentifier to tv.wslstudios.commentary, which is the OS-VISIBLE
	// identity: TCC keys the microphone grant on it, the Keychain ACL names it,
	// and the notarisation ticket is issued against it. Changing that string
	// re-prompts every operator for their microphone and puts their stored
	// password out of reach. This one is a lock name — a named mutex on Windows,
	// an flock file plus a distributed-notification name on macOS — with no
	// persistence and no OS meaning beyond uniqueness within a login session.
	//
	// So they are two names for two different things, and the two are allowed to
	// differ. What is NOT allowed is either of them changing by accident, which
	// is why both are literals in files that explain themselves rather than
	// derived from wails.json's project name (the Wails template derives the
	// bundle identifier that way, and renaming the project would silently move
	// the operator's grants).
	//
	// One process is not a stylistic preference here. A second instance would
	// open the same audio capture device — WASAPI shared mode on Windows, and
	// CoreAudio, which shares an input device between processes without
	// complaint, on macOS, so it would succeed on both — and dial the same M2L-X
	// router input, whose SRT listener accepts exactly one peer and never
	// displaces the incumbent (specification section 6.2). The second instance
	// would therefore sit in its backoff ladder forever, showing amber, while the
	// first is the one actually on air. A commentator who double-clicks twice
	// must get their existing window back, not that.
	//
	// WAILS IMPLEMENTS THIS ON BOTH PLATFORMS, BY DIFFERENT MEANS, AND THE
	// DIFFERENCES ARE WORTH KNOWING BEFORE TRUSTING IT. Read from the v2.13.0
	// source rather than from the documentation:
	//
	//   - Windows takes a named mutex and hands over down a named pipe.
	//   - macOS (internal/frontend/desktop/darwin/single_instance.go) flock()s
	//     <NSTemporaryDirectory()>/uk.co.wslstudios.wslcomms.lock and hands over
	//     by NSDistributedNotificationCenter. NSTemporaryDirectory is per-user
	//     and, for a bundled app, per-bundle-container, so the macOS lock is per
	//     LOGIN SESSION rather than per machine: two users logged in at once
	//     under fast user switching would get one wslcomms each. That is the
	//     right answer anyway — they have different audio devices and different
	//     configuration — but it is not the same guarantee as the Windows one and
	//     it should not be described as if it were.
	//   - On macOS the losing instance calls os.Exit(0) from inside wails.Run,
	//     so it never creates a window, never reaches OnStartup, and never runs
	//     App.teardown. Nothing has been started by then, so nothing leaks; but
	//     it does mean gst.Init has already run in that doomed process, which is
	//     harmless and is why Init is careful to be side-effect-free beyond the
	//     registry scan.
	//   - A double-click in Finder usually never gets this far: LaunchServices
	//     activates the running .app instead of starting a second copy. The lock
	//     is what catches `open -n`, a second copy launched from a terminal, and
	//     the same binary started from two different paths.
	singleInstanceID = "uk.co.wslstudios.wslcomms"
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
	// The log file comes first — before anything can log. A release build has no
	// console on either platform: -H windowsgui on Windows, and on macOS an .app
	// launched from Finder or the Dock inherits no terminal, so its stderr goes
	// to the unified log at best and nowhere the operator can reach at worst.
	// Without this every log.Printf in the application would be composed and then
	// discarded. That is not hypothetical; an SRT return failure on a remote
	// machine was undiagnosable for exactly this reason, with the diagnosis being
	// written to a stderr that did not exist.
	//
	// A failure to open the file is logged (to stderr, which is all there is at
	// that point) and otherwise ignored: no machine loses the application over
	// a log file.
	stamp := time.Now().UTC().Format("20060102-150405")
	if logDir, err := applog.DefaultDir(); err != nil {
		log.Printf("wslcomms: no log directory available (%v); logging to stderr only", err)
	} else if sink, err := applog.Open(logDir, stamp); err != nil {
		log.Printf("wslcomms: could not open a log file (%v); logging to stderr only", err)
	} else {
		defer sink.Close()

		// GStreamer's own debug goes to a sibling file. Set-if-unset, and it
		// must happen here — before gst.Init below — because gst_init reads
		// these variables exactly once. srt* at INFO is what turns "the return
		// isn't working" into a file that says why.
		applog.SetGStreamerDebugDefaults(applog.GStreamerLogPath(logDir, stamp))

		exe, _ := os.Executable()
		log.Printf("wslcomms: started; exe=%q pid=%d; logging to %q", exe, os.Getpid(), sink.Path)
	}

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

	err = wails.Run(&options.App{
		Title:            windowTitle,
		Width:            windowWidth,
		Height:           windowHeight,
		MinWidth:         windowMinWidth,
		MinHeight:        windowMinHeight,
		BackgroundColour: options.NewRGB(backgroundR, backgroundG, backgroundB),
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup:        app.startup,
		OnDomReady:       app.domReady,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
		WindowStartState: options.Normal,

		// Closing the window must end the process. There is one window, no
		// in-window menu and no tray icon (specification section 10), so a hidden
		// window would be a process nobody can see and nobody can close.
		//
		// Wails honours this on both platforms, which is worth stating because
		// macOS convention is the opposite: an app whose last window closes
		// normally stays running in the Dock. Wails' darwin WindowDelegate turns
		// the red button into a quit — windowShouldClose posts the same "Q" the
		// Quit menu item does — so the close still runs OnShutdown and App.teardown
		// rather than orphaning a process with no window. The application menu
		// below is what makes Cmd-Q reach the identical path.
		HideWindowOnClose: false,

		// No right-click "Reload" / "Inspect" on a live contribution window.
		// This is already the production default; it is stated because the
		// commentary position is operated by a commentator, not an engineer,
		// and an accidental reload mid-match drops the monitor and the lamps.
		EnableDefaultContextMenu: false,

		// The macOS application menu. Nil everywhere else — see applicationMenu.
		Menu: applicationMenu(),

		// macOS window and webview behaviour. Ignored by the Windows frontend,
		// which never reads the field — see macOptions.
		Mac: macOptions(),

		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               singleInstanceID,
			OnSecondInstanceLaunch: app.secondInstanceLaunched,
		},
	})

	// A wails.Run failure is reported BEFORE the teardown, because teardown no
	// longer returns — it ends the process itself on every path (see App.teardown).
	// The message goes to the LOG FILE as well as stderr: a release build has no
	// console, and a failure this early — a WebView2 environment that would not
	// create, say — is otherwise undiagnosable. The exit status is not read by
	// anything (a GUI process launched from a shortcut), so it is not preserved
	// through the hard exit below; the diagnosis is.
	if err != nil {
		log.Printf("wslcomms: wails.Run returned an error: %v", err)
		fmt.Fprintln(os.Stderr, "wslcomms:", err)
	}

	// teardown is idempotent and normally ran under OnShutdown already. It is
	// repeated here because wails.Run can also return by a path that never fires
	// OnShutdown — a WebView2 environment that fails to create, or a macOS
	// single-instance handover, which os.Exit(0)s from inside wails.Run without
	// unwinding. On the ordinary close it has already run and force-exited from
	// inside OnShutdown, so this call is never reached; on the paths that skip
	// OnShutdown it is what ends the process.
	//
	// IT DOES NOT RETURN. teardown ends the process through TerminateProcess on
	// every path now, not only when it had to abandon a step: ExitProcess over
	// the GStreamer, D3D11, GPU-driver, WASAPI and COM DLLs this process has
	// loaded can deadlock in DLL_PROCESS_DETACH even after a clean shutdown, which
	// is the wslcomms that would not close. See App.teardown and exit_windows.go.
	app.teardown()

	// Unreachable when teardown ended the process, which is every real path. It is
	// the backstop for the one path that could fall through — a teardown that was
	// already run under OnShutdown and so no-ops the second time here — so that the
	// process still leaves by TerminateProcess rather than dropping off the end of
	// main into the Go runtime's ExitProcess and the very DLL_PROCESS_DETACH
	// deadlock teardown exists to avoid.
	forceExit()
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
//
// # ON MACOS THIS VARIABLE DOES NOTHING, AND THAT IS NOT THE WHOLE STORY
//
// WKWebView has no Chromium command line and reads no such variable, so the
// Setenv below is inert there. It is still done unconditionally, and that is a
// judgement rather than an oversight: an unread environment variable costs
// nothing (nothing this process spawns reads it either — /usr/bin/security and
// gst-plugin-scanner are the only children it has), whereas a per-GOOS body
// would split a five-line function across a build-tag matrix of three tags and
// two platforms and leave the assertion in TestSetWebView2Arguments testing
// nothing on the machine the work is being done on. The log line below is what
// carries the information instead, and it is not decoration: both flags have
// macOS counterparts and neither is where a reader would look for them.
//
//   - The capture permission is not a flag but a DELEGATE CALLBACK. WKWebView
//     asks its WKUIDelegate through
//     webView:requestMediaCapturePermissionForOrigin:initiatedByFrame:type:decisionHandler:
//     and Wails v2.13.0 declares WKUIDelegate without implementing it. That is
//     fixed in Wails' Objective-C rather than here; see third_party/README.md.
//
//     WHAT THE MISSING CALLBACK ACTUALLY DOES WAS MEASURED, AND IT IS NOT WHAT
//     THIS COMMENT USED TO SAY. The prediction — and what two standalone
//     WKWebView harnesses showed — was that with no implementation the decision
//     handler is never called and getUserMedia never settles, hanging rather
//     than rejecting and taking the whole headphone dropdown with it. In the
//     REAL application on macOS 26 (Darwin 25.3.0) it does not hang: WebKit's
//     undocumented default is to GRANT. Two production-tag builds differing only
//     in the Wails replace directive, run twice:
//
//     getUserMedia({video:true})   patched: NotAllowedError in 19-25 ms
//     unpatched: GRANTED in 4954 ms
//     getUserMedia({audio:true})   patched: resolved 146-163 ms, track
//     "MacBook Pro Microphone"
//     enumerateDevices audiooutput 3 with labels in BOTH builds
//
//     So the patch is still required, for a sharper reason than the one it was
//     written for: without it WebKit hands the page the CAMERA, in a client that
//     sits in front of a commentator for the length of a match, and on a bundle
//     with no NSCameraUsageDescription that is a TCC HARD KILL rather than a
//     denial — the unpatched control was watched dying of exactly that. Relying
//     on an undocumented WebKit default for microphone capture is not shippable
//     on an operating system that updates itself either way.
//
//   - Autoplay needs nothing. WKWebView as Wails configures it already permits
//     audio and video to start without a user gesture, measured on this machine
//     with no flag and no configuration change.
//
// setSinkId is the one place where macOS is STRICTER rather than merely
// different: it exists and it works, but it requires a genuine user gesture, so
// a device applied from a configuration-load path rejects with NotAllowedError
// where Windows would simply have done it. That is the frontend's, and
// frontend/src/monitor/audio.js now handles it in applySinkId by arming the
// same one-shot gesture listener the autoplay case already used — once per
// requested device, so an un-grantable one is reported as a real failure rather
// than promising "on the next click" for ever.
func setWebView2Arguments() {
	const key = "WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS"
	const value = "--autoplay-policy=no-user-gesture-required " +
		"--auto-accept-camera-and-microphone-capture"

	if runtime.GOOS != "windows" {
		log.Printf("wslcomms: %s is a WebView2 variable and is inert on %s; "+
			"media-capture permission there comes from Wails' WKUIDelegate patch (see third_party/README.md) "+
			"and autoplay needs no equivalent", key, runtime.GOOS)
	}

	if err := os.Setenv(key, value); err != nil {
		// Non-fatal, but the operator needs to know why the headphone dropdown
		// is empty, so say so loudly rather than letting it look like a driver
		// problem.
		log.Printf("wslcomms: could not set %s (%v); on Windows "+
			"enumerateDevices will return blank device labels and the headphone dropdown will be unusable", key, err)
	}
}

// applicationMenu is the macOS application menu, and nil on every other
// platform.
//
// # Why a menu exists at all, in an application whose specification says it has none
//
// Specification section 10 says one window, no menu, no tray icon, and this does
// not contradict it: on macOS the menu bar is not in the window. It belongs to
// the application and lives at the top of the screen whether the application
// asks for one or not — and if the application does not ask, IT GETS AN EMPTY
// ONE, because Wails only calls SetApplicationMenu when options.Menu is non-nil.
//
// An empty menu bar is not a cosmetic loss. macOS routes command-key equivalents
// through the main menu before the responder chain, so a menu-less app has NO
// Cmd-Q, NO Cmd-C, NO Cmd-V and NO Cmd-A. The Settings screen has an M2L-X
// password field and two SRT passphrase fields, and passphrases are pasted from
// a password manager rather than typed. Shipping without this would mean an
// operator could not paste their credentials in, and could not quit with the
// keystroke every other Mac application answers to.
//
// AppMenu and EditMenu are the whole list, deliberately. AppMenu carries About
// (only when Mac.About is set, which macOptions does), Hide, Hide Others, Show
// All and Quit; EditMenu carries Undo, Redo, Cut, Copy, Paste and Select All.
// menu.WindowMenu() exists and is not used: it would add Minimise and Zoom to a
// fixed single-window contribution client that wants neither. There is no View
// menu because Wails v2.13.0 does not offer one — ViewMenu is commented out in
// its menuroles.go — and it would not be wanted if it did, since the role it
// stands for puts Reload and Toggle Developer Tools on the menu bar of a live
// feed, which is the thing EnableDefaultContextMenu is false to prevent.
//
// Quit here is Wails' own selector, not NSApp's terminate:, so Cmd-Q posts the
// same message the red button does and leaves through OnShutdown and
// App.teardown. It is not a way round the ordered shutdown.
//
// Nil on Windows on purpose. options.App.Menu is cross-platform, and a non-nil
// menu there draws a menu bar INSIDE the window, which is exactly what section
// 10 forbids.
func applicationMenu() *menu.Menu {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return menu.NewMenuFromItems(menu.AppMenu(), menu.EditMenu())
}

// macOptions is the macOS half of the window's appearance and behaviour. The
// Windows frontend never reads options.App.Mac, so this is returned
// unconditionally rather than guarded; the guard would only hide which fields
// are set from someone reading on the wrong platform.
//
// TWO settings, which is fewer than it looks like it should be. What is NOT set
// matters as much, and one of the omissions is a trap:
//
//   - DisableZoom IS NOT WHAT ITS NAME SUGGESTS and is deliberately left false.
//     It reads as "stop the operator zooming the web content", which would be a
//     reasonable thing to want on a fixed layout and the natural macOS analogue
//     of EnableDefaultContextMenu being false. It is not that. Wails passes it
//     to CreateWindow as `zoomable`, and the only thing that flag does is
//     disable the green NSWindowZoomButton — the MAXIMISE button in the title
//     bar. Setting it would take away a window control the Windows build has,
//     to solve a problem it does not touch. WKWebView pinch-zoom is a separate
//     setting Wails does not expose at all.
//   - TitleBar stays at the default. A hidden or full-size-content title bar
//     would move the traffic lights over a page frontend/ was laid out without
//     them in mind.
//   - WebviewIsTransparent and WindowIsTranslucent stay false: the page is
//     opaque by design and translucency would put desktop wallpaper behind a
//     PGM tile.
//   - ContentProtection stays false. Excluding the window from screen capture
//     would stop the operator screen-sharing it to whoever is helping them,
//     which is how a commentary position gets supported mid-match.
func macOptions() *mac.Options {
	return &mac.Options{
		// Force the dark system appearance. BackgroundColour above is #0b0d10 and
		// every stylesheet in frontend/ is dark; without this the title bar,
		// scrollbars and native form controls follow the SYSTEM setting, so a Mac
		// in light mode puts a light-grey title bar and light scrollbars on a
		// near-black window. Windows has no equivalent to get wrong.
		Appearance: mac.NSAppearanceNameDarkAqua,

		// The About panel, which is what makes the About item appear in AppMenu
		// at all — Wails omits the item entirely when this is nil, so a bare
		// AppMenu would give a Mac application with no About box, which reads as
		// unfinished.
		//
		// Icon is left nil on purpose. Wails builds its NSImage from the bytes
		// given here, and with none it never calls setIcon:, which leaves the
		// NSAlert showing its default — the application's own icon, the one
		// already in the Dock. Embedding a second copy of that icon in the binary
		// to hand back the same picture would be work with a way to be wrong.
		About: &mac.AboutInfo{
			Title:   windowTitle,
			Message: aboutMessage,
		},
	}
}

// appDir returns the directory holding this executable, with symlinks resolved.
//
// It is where the bundled GStreamer and the default slate.png live: laid down by
// the installer beside wslcomms.exe on Windows (<appDir>\gst\, specification
// section 11), and inside the bundle on macOS, where the executable is at
// wslcomms.app/Contents/MacOS/wslcomms and appDir is that MacOS directory.
//
// Symlinks are resolved because a shortcut target or a junctioned Program Files
// would otherwise put appDir somewhere that has neither. It matters at least as
// much on macOS, where /usr/local/bin symlinks into an .app are a normal way to
// launch one and would otherwise resolve appDir to /usr/local/bin.
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
