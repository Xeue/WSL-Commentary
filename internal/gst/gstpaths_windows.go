//go:build cgo && !gststub && windows

// gstpaths_windows.go is the Windows half of the two questions Init cannot
// answer without knowing which operating system it is on: WHERE the bundled
// plugins are relative to the executable, and WHAT ELSE has to be in the
// environment before gst_init.
//
// Owner: WP-B (packaging), with build/bundle-gst.ps1, which is what actually
// produces the layout described here. Read gstpaths_darwin.go beside it: the two
// files exist as a pair so that doInit stays ONE function with no runtime.GOOS
// branch in it, exactly as elements_windows.go and elements_darwin.go do for the
// element contract.
//
// Nothing here is new. Both answers are what Init has always done on Windows,
// moved out of its body unchanged so that the other platform can differ without
// either spelling drifting from the other.
//
// There is deliberately no gstpaths_other.go. internal/gst does not build on any
// third GOOS with cgo enabled anyway — platformRequiredElements and
// captureDeviceID have no third twin either — and inventing one here would
// assert a bundle layout for a platform nobody has ever staged.
package gst

import "path/filepath"

// bundlePluginDir returns the bundled plugin directory for appDir, which on
// Windows is the directory holding wslcomms.exe.
//
// <appDir>\gst\lib\gstreamer-1.0 is what build/bundle-gst.ps1 stages and what
// the installer lays down beside the executable. The core DLLs sit flat in
// <appDir> itself, where the Windows loader finds them next to the .exe with no
// path help at all — which is why this platform needs nothing from
// extraInitEnv.
func bundlePluginDir(appDir string) string {
	return filepath.Join(appDir, "gst", "lib", "gstreamer-1.0")
}

// extraInitEnv returns nothing on Windows.
//
// The three GST_PLUGIN_*PATH variables and GST_REGISTRY_1_0 that doInit sets for
// both platforms are the whole of it here. There is no out-of-process plugin
// scanner to locate — the Windows GStreamer build does not ship one and
// libgstreamer scans in process — no GIO modules to fence off, and no liborc
// error to suppress, because Windows has no hardened runtime denying it a
// writable-executable mapping.
//
// Returning an empty list rather than nil-with-a-comment would be the same
// thing; nil is returned because there is genuinely nothing, and a reader who
// wants to know what the other platform needs should read gstpaths_darwin.go
// rather than an empty slice here.
func extraInitEnv(appDir string) ([]initEnvVar, error) {
	return nil, nil
}
