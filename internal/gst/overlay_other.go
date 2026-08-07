//go:build !windows

// overlay_other.go is the non-Windows twin of overlay_windows.go.
//
// This application ships on Windows and only on Windows: the capture is
// wasapi2src, the encoders are Media Foundation, the decoder is DXVA and the
// window is an HWND. There is no port and none is planned.
//
// This file exists so that `go build ./...` and `go vet ./...` still work when
// somebody runs them with GOOS set to something else — which happens, because
// editors and CI runners do it — and so that the failure they get is one clear
// sentence rather than a page of undefined syscalls. It is the same reason
// main_nocgo.go exists.
package gst

import (
	"errors"
	"runtime"
)

// PictureOverlay is the window the SRT picture is drawn in. See
// overlay_windows.go for the real one and for what every method has to promise.
type PictureOverlay interface {
	Handle() uintptr
	SetRect(r PictureRect) error
	SetVisible(visible bool) error
	Close() error
}

// ErrNoHostWindow is returned when the application window does not exist yet. It
// is declared here so callers can test for it on any platform.
var ErrNoHostWindow = errors.New("gst: the application window does not exist yet")

// NewPictureOverlay always fails away from Windows. The picture is presented by
// d3d11videosink into an HWND, and neither of those exists here.
func NewPictureOverlay(string) (PictureOverlay, error) {
	return nil, errors.New("gst: the picture overlay needs Windows: it is an HWND rendered into by " +
		"d3d11videosink, and this is " + runtime.GOOS)
}
