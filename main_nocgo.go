//go:build !(dev || production || bindings)

// This file supplies main() for every build that is NOT a real Wails build, so
// that `go build ./...` and `go test ./...` always succeed and always produce an
// inert binary.
//
// Owner: WP-8, along with main.go and app.go.
//
// # Why the constraint is shaped like this
//
// Wails guards against being built with plain `go build`: when its own build
// tags are absent, wails.Run reaches internal/app.CreateApp, which pops a modal
// Windows MessageBox reading "Wails applications will not build without the
// correct build tags" and blocks until it is dismissed. Its OK button opens a
// browser at wails.io, so the only harmless answer is Cancel.
//
// A constraint of just `//go:build cgo` is not enough to prevent that. Wails on
// Windows is pure Go — it reaches WebView2 through go-webview2 and go-winloader,
// not through cgo — so anyone running `go build` or `go test ./...` with
// CGO_ENABLED=1 links main.go and app.go successfully and gets the dialog. That
// happened: an agent ran `CGO_ENABLED=1 go test ./... -race` and produced a
// stream of modal dialogs.
//
// So the constraint names what is actually required — a build done the Wails
// way — rather than merely cgo being available. dev, production and bindings are
// the tags the Wails CLI sets for `wails dev`, `wails build` and its bindings
// generation step respectively. Anything else lands here instead, and the worst
// a stray build can now do is print one line.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr,
		"wslcomms: this binary is inert. The GUI must be built with the Wails CLI "+
			"(`wails build -webview2 embed`), which requires MinGW gcc and GStreamer — "+
			"see build/README.md and docs/project-plan.md, Gate B.")
	os.Exit(1)
}
