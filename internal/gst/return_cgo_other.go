//go:build cgo && !gststub && !windows && !darwin

// return_cgo_other.go is the third twin of return_cgo_windows.go and
// return_cgo_darwin.go: the one for an operating system this application does
// not ship on.
//
// Owner: WP-R.
//
// The return monitor is Windows and macOS. Its tail is an OS audio stack —
// Media Foundation and WASAPI, or AudioToolbox and CoreAudio — and there is no
// third implementation, wanted or planned.
//
// This file exists for the same reason overlay_other.go does: so that
// `go build ./...` and `go vet ./...` still work when somebody runs them with
// GOOS set to something else, which happens, because editors and CI runners do
// it. What they get is one clear sentence at the point of use rather than a page
// of undefined symbols at link time.
//
// Nothing here is a stub of the pipeline. newReturnPipe refuses before any of
// these are reached, so the specs below are empty rather than a plausible-looking
// Linux chain that nobody has ever run and that would be worse than nothing: a
// chain that compiles invites the belief that it works.
package gst

import (
	"errors"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// returnPlatformSupported is false here and true in the two real twins.
// newReturnPipe reads it and refuses, so everything below is unreachable.
const returnPlatformSupported = false

// returnSinkFactory has no meaning on this platform. It is named rather than
// left out because return_cgo.go names it, and an empty string reaching
// gst_element_factory_make would be a nil element rather than a diagnosis.
const returnSinkFactory = ""

// returnRequiredElements is empty: there is no pipeline to check the bundle for.
var returnRequiredElements = []struct{ factory, plugin string }{}

// returnDecodeSpecs is unreachable. See the file comment.
func returnDecodeSpecs() []returnElementSpec { return nil }

// returnRenderSpecs is unreachable. See the file comment.
func returnRenderSpecs() []returnElementSpec { return nil }

// playbackDeviceID offers nothing, so ListOutputDevices returns an empty list
// rather than devices whose ids no sink on this platform could be given.
func playbackDeviceID(_ gogst.Device, _ *gogst.Structure) (string, bool) { return "", false }

// configureReturnSinkLocked is unreachable and says so rather than silently
// succeeding, in case a future change reaches it before this file has been
// thought about.
func configureReturnSinkLocked(_ gogst.Element, _ string) error {
	return errors.New("gst: return monitor: there is no playback sink for this operating system")
}
