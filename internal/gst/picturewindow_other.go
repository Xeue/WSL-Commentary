//go:build !windows

// picturewindow_other.go is picturewindow_windows.go's twin for every other
// build: the picture process's top-level window does not exist there yet.
//
// The picture used to be an NSView overlay on macOS (overlay_darwin.go), painted
// into the application's own window. Moving the picture into its own process
// means it needs its own NSWindow instead, and that has not been written: this
// file says so, in one sentence, so that a macOS build fails to open the picture
// with a reason rather than with a nil handle handed to glimagesink. The
// application treats the failure exactly as it treats any picture that will not
// start — the fallback mosaic stays on screen and the audio is untouched.
package gst

import (
	"errors"
	"runtime"
)

// PictureWindow is a top-level native window the picture pipeline renders into.
// See picturewindow_windows.go for the real one and for what each method
// promises.
type PictureWindow interface {
	Handle() uintptr
	Closed() <-chan struct{}
	Close() error
	Placement() (PictureWindowPlacement, bool)
}

// PictureWindowPlacement is where the picture window sits on the desktop. See
// picturewindow_windows.go; here it is only a shape the picture process can
// save and load, so that its file format does not depend on the platform.
type PictureWindowPlacement struct {
	Left, Top, Right, Bottom int32
	Maximised                bool
}

// NewPictureWindow always fails in this build. The message names the platform
// because it is the platform, not a build option, that is missing the window.
func NewPictureWindow(_ string, _ *PictureWindowPlacement, _ func(PictureWindowPlacement)) (PictureWindow, error) {
	return nil, errors.New("gst: the picture's own window is implemented on Windows only; on " +
		runtime.GOOS + " the picture process has no window to render into yet")
}
