//go:build cgo && !gststub && darwin

// gstpaths_darwin.go is the macOS half of the two questions Init cannot answer
// without knowing which operating system it is on: WHERE the bundled plugins are
// relative to the executable, and WHAT ELSE has to be in the environment before
// gst_init.
//
// Owner: WP-B (packaging), with build/bundle-gst-darwin.sh, which is what
// actually produces the layout described here and whose hermetic proof sets
// exactly the environment this file sets. Read gstpaths_windows.go beside it.
//
// Every path below matches what the bundler stages and what build/ship-darwin.sh
// signs. If one of them is edited here it has to be edited there, and the
// bundler's step 5 — resolve all 29 wanted elements under `env -i` through a
// gst-inspect inside the bundle — is what catches it if it is not.
//
// # What is different about macOS, and it is not just the directory names
//
// On Windows the bundle is a directory of DLLs beside the .exe and the loader
// finds them by being in the same directory. A macOS .app is a signed, sealed,
// relocatable tree that the operator may drag anywhere, so every path has to be
// derived from the executable at run time — and the vendored dylibs arrive from
// Homebrew with /opt/homebrew compiled into them in two different ways. The
// bundler's install_name_tool pass rewrites the ones that are LOAD COMMANDS, and
// its audit proves there are none left. The ones that are C STRING LITERALS
// inside libgstreamer, libgio and libglib cannot be rewritten and cannot be
// audited for. They are neutralised HERE or they are not neutralised at all,
// which is why three of the four variables below exist.
package gst

import (
	"fmt"
	"os"
	"path/filepath"
)

// bundlePluginDir returns the bundled plugin directory for appDir, which on
// darwin is <App>.app/Contents/MacOS.
//
// It is Contents/RESOURCES/gstreamer-1.0, not Contents/Frameworks/gstreamer-1.0
// where a reader would reasonably expect it, and that is not a preference.
// codesign treats every DIRECTORY under Contents/Frameworks as nested code and
// requires it to be a bundle in its own right, so a plain directory of dylibs
// there makes the ENTIRE .app unsignable. Measured, verbatim:
//
//	bundle format unrecognized, invalid, or unsuitable
//	In subcomponent: .../Contents/Frameworks/gstreamer-1.0
//
// Contents/Resources has no such rule, and the flat core dylibs that stay in
// Contents/Frameworks are fine because they are files rather than directories.
// See build/README-darwin.md section 6.
func bundlePluginDir(appDir string) string {
	return filepath.Join(appDir, "..", "Resources", "gstreamer-1.0")
}

// extraInitEnv returns what has to be set IN ADDITION to the three
// GST_PLUGIN_*PATH variables and GST_REGISTRY_1_0, before gst_init.
//
// appDir is <App>.app/Contents/MacOS. It is already absolute and symlink-resolved
// by the time doInit passes it here.
func extraInitEnv(appDir string) ([]initEnvVar, error) {
	// GST_PLUGIN_SCANNER: libgstreamer spawns an OUT-OF-PROCESS scanner to load
	// each plugin during a registry rebuild, so that a plugin which crashes on
	// load takes the scanner down instead of the application. The path it spawns
	// is compiled in as
	//
	//	/opt/homebrew/Cellar/gstreamer/<version>/libexec/gstreamer-1.0/gst-plugin-scanner
	//
	// which does not exist on a customer's Mac.
	//
	// Getting this wrong does not fail. GStreamer prints "External plugin loader
	// failed" at WARNING and falls back to scanning plugins IN PROCESS — measured
	// with the path forced to /nonexistent, the registry still built, 17 plugins
	// and 291 features, and every element resolved. That is exactly why it would
	// ship unnoticed, and exactly why it is a hard error here: the fallback works
	// until the day a plugin faults on load, and then it takes the commentary
	// position down at the moment GStreamer is being initialised rather than
	// printing a warning.
	scanner := filepath.Join(appDir, "gst-plugin-scanner")
	if _, err := os.Stat(scanner); err != nil {
		return nil, fmt.Errorf("gst: Init: bundled gst-plugin-scanner %q is not present: %w", scanner, err)
	}

	// Ordered rather than a map, so that a failure names the same variable on
	// every run and two logs from two machines can be diffed. They are
	// independent of one another, so the order is only for the reader.
	return []initEnvVar{
		// Both spellings, for the same reason both spellings of the plugin path
		// are set: the versioned name is what current GStreamer reads and the
		// unversioned one is the fallback it consults when the versioned one is
		// absent. Setting one and not the other leaves the other pointing at
		// Homebrew on a machine that has Homebrew.
		{"GST_PLUGIN_SCANNER_1_0", scanner},
		{"GST_PLUGIN_SCANNER", scanner},

		// GIO_MODULE_DIR is the one variable here that is about a machine which
		// DOES have Homebrew, and it is the one that would never be found in the
		// field. libgio scans /opt/homebrew/lib/gio/modules at run time from a
		// compiled-in string, so on a developer's or a power user's Mac a FOREIGN
		// glib's modules get dlopened into this process next to our own vendored
		// glib. Two glibs in one address space is not a configuration anybody
		// tests, and the failure would be a crash on a machine that is not the
		// one the bug is reproduced on.
		//
		// It points at a directory build/bundle-gst-darwin.sh creates and keeps
		// deliberately EMPTY. Pointing it at a non-existent path would work too;
		// an empty real directory is used so that the audit can see it, and so
		// that nothing is tempted to interpret the absence as "unset".
		{"GIO_MODULE_DIR", filepath.Join(appDir, "..", "Resources", "gio-modules")},

		// ORC_CODE is the one entry that is a preference rather than a
		// requirement, and it is set anyway, unconditionally, for the same reason
		// the plugin paths are: this process owns its GStreamer environment
		// completely, and a variable left to whatever the launching shell had is
		// a configuration nobody chose.
		//
		// liborc is GStreamer's SIMD code generator (videoconvert, videoscale,
		// audioconvert, audioresample). Under the hardened runtime on Apple
		// silicon it cannot obtain a writable-executable mapping, so it logs at
		// ERROR level on every single start:
		//
		//	ORC: ERROR: orc_code_region_allocate_codemem(): Failed to create write
		//	and exec mmap regions. This is probably because the Hardened Runtime is
		//	enabled without the com.apple.security.cs.allow-jit entitlement.
		//
		// The entitlement it names does NOT fix it. Measured, with allow-jit, and
		// with allow-jit plus allow-unsigned-executable-memory: identical error
		// both times, because on Apple silicon a W+X mapping cannot be obtained at
		// all. And orc's C fallback costs nothing measurable — 1.62 to 1.65
		// seconds of user CPU over 300 buffers of 720p convert-and-scale in all
		// four configurations, which is inside the noise.
		//
		// So this suppresses the attempt rather than widening the runtime, and
		// keeps a line that reads exactly like a fault out of every log this
		// product will ever produce. The log file is the only diagnosis a
		// commentary position produces; an ERROR in it that means nothing costs
		// somebody an hour on the wrong trail. See
		// build/darwin/wslcomms.entitlements for the entitlement measurement.
		{"ORC_CODE", "backup"},
	}, nil
}
