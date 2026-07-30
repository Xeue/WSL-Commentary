//go:build !cgo

// This file exists so that `go build ./...` and `go test ./...` succeed with
// CGO_ENABLED=0 — Gate A, a machine with no MinGW gcc and no GStreamer. The GUI
// itself cannot be built that way, because Wails links WebView2 through cgo, so
// the binary produced here says so and exits.
//
// Owner: WP-8, along with main.go and app.go.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr,
		"wslcomms: this binary was built with CGO_ENABLED=0; the GUI requires a cgo build (see docs/project-plan.md, Gate B)")
	os.Exit(1)
}
