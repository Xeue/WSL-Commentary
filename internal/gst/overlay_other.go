//go:build !windows && (!darwin || !cgo)

// overlay_other.go is the twin of overlay_windows.go and overlay_darwin.go for
// every build that has neither of them.
//
// IT IS NO LONGER THE macOS ANSWER. There are now two real overlays: a child
// HWND painted by d3d11videosink on Windows, and an NSView painted by
// glimagesink on macOS. What is left for this file is the two builds that have
// no native surface to offer at all:
//
//   - GOOS that this application has never run on. `go build ./...` and
//     `go vet ./...` get run with GOOS set to all sorts of things by editors and
//     CI runners, and the failure they get should be one clear sentence rather
//     than a page of undefined syscalls. That is the same reason main_nocgo.go
//     exists.
//
//   - macOS WITHOUT cgo, which is Gate A on the development machine and is the
//     case that matters day to day. overlay_darwin.go is Objective-C behind
//     `import "C"`, so CGO_ENABLED=0 drops it, and without this file the whole
//     of package main would stop compiling on the machine the work is done on.
//     A Gate A build therefore gets an overlay that refuses politely, which is
//     exactly the state app_picture.go already handles: SetPictureRect treats a
//     failure to create one as "not yet" and carries on.
//
// The build constraint is `!windows && (!darwin || !cgo)` rather than
// `!windows && !darwin` for that second reason alone. Narrowing it to
// `!windows && !darwin` compiles here and breaks Gate A.
package gst

import (
	"errors"
	"runtime"
)

// PictureOverlay is the surface the SRT picture is drawn in. See
// overlay_windows.go and overlay_darwin.go for the real ones and for what every
// method has to promise.
type PictureOverlay interface {
	Handle() uintptr
	SetRect(r PictureRect) error
	SetVisible(visible bool) error
	Close() error
}

// ErrNoHostWindow is returned when the application window does not exist yet. It
// is declared here so callers can test for it on any platform.
var ErrNoHostWindow = errors.New("gst: the application window does not exist yet")

// NewPictureOverlay always fails in this build. There is a native surface for
// Windows and one for macOS, and neither can be reached from here: on an
// unsupported GOOS there is no window server this package knows how to talk to,
// and on macOS without cgo there is no way to call AppKit at all.
//
// The message names the reason rather than the platform, because the macOS case
// is a build option the reader can change and the other case is not.
func NewPictureOverlay(string) (PictureOverlay, error) {
	if runtime.GOOS == "darwin" {
		return nil, errors.New("gst: the picture overlay on macOS is an NSView rendered into by " +
			"glimagesink, which needs cgo; this binary was built with CGO_ENABLED=0")
	}
	return nil, errors.New("gst: the picture overlay needs Windows (a child HWND rendered into by " +
		"d3d11videosink) or macOS (an NSView rendered into by glimagesink), and this is " +
		runtime.GOOS)
}
