//go:build !windows

// main_other.go is the non-Windows twin of main_windows.go.
//
// Owner: WP-6, with build/.
//
// # Why this file exists at all
//
// cmd/portable is a Windows self-extracting launcher: it unpacks an embedded
// ~20 MB zip of the GStreamer runtime beside the executable and then runs
// wslcomms.exe against it. None of that has a macOS analogue — a .app bundle is
// already a self-contained directory, and the runtime is vendored into
// Contents/Frameworks at build time rather than unpacked at first run — so
// there is deliberately no port of run() here.
//
// What there has to be is a main(). Without one, `go build ./...` on any
// non-Windows GOOS fails the whole repo with
//
//	runtime.main_main·f: function main is undeclared in the main package
//
// which breaks CONTRACT.md's Gate A ("CGO_ENABLED=0 go build ./... must pass,
// at every commit, from the repo root") on darwin and linux while passing on
// Windows. That asymmetry is the bug this file fixes: Gate A is supposed to be
// the property that anyone can pick the project up with only Go and Node, and
// an editor or CI runner with GOOS set to anything else got a failure that had
// nothing to do with their change.
//
// It is the same reasoning, and the same shape, as main_nocgo.go at the repo
// root: supply an inert main() that says one true sentence, so a stray build
// produces something harmless rather than a confusing link error.
package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	fmt.Fprintf(os.Stderr,
		"wslcomms portable launcher: this is a Windows-only tool and this is %s.\n"+
			"On macOS the GStreamer runtime is vendored into the .app bundle at build\n"+
			"time; there is nothing to unpack.\n", runtime.GOOS)
	os.Exit(1)
}
