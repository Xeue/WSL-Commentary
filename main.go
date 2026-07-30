//go:build cgo

// Command wslcomms is the WSL Studios commentary contribution application: one
// window, one process, one installer.
//
// Owner: WP-8. WP-0 left this as a compiling stub.
//
// This file is behind //go:build cgo because Wails links WebView2 through cgo.
// main_nocgo.go supplies a main for CGO_ENABLED=0 builds so that
// `go build ./...` stays honest at Gate A, where there is no MinGW gcc.
package main

import (
	"fmt"
	"os"

	"github.com/wailsapp/wails/v2"
)

// wailsRun is referenced so that `go mod tidy` keeps the frozen dependency on
// Wails before WP-8 writes the real wire-up. WP-8 replaces this with a real
// wails.Run call.
var wailsRun = wails.Run

// WP-8, in order:
//
//  1. Set WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS to
//     "--autoplay-policy=no-user-gesture-required
//     --auto-accept-camera-and-microphone-capture" BEFORE the window is created,
//     or enumerateDevices returns blank labels and the headphone dropdown is
//     unusable.
//  2. Call gst.Init(appDir) with the directory holding this executable, before
//     any other gst call, so the bundled plugins are the only ones visible.
//  3. Load config, construct the m2lx client and watcher, the secrets store, the
//     pipeline and the sender, and bind the App from app.go.
//  4. Run Wails, embedding frontend/dist.
func main() {
	_ = wailsRun
	fmt.Fprintln(os.Stderr, "wslcomms: not implemented: WP-8")
	os.Exit(1)
}
