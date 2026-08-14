//go:build cgo && !gststub && darwin

// Tests for gstpaths_darwin.go — the two macOS-only answers Init needs.
//
// These run at GATE B and not at Gate A, because the file under test is behind
// `cgo && !gststub` and Gate A compiles neither half of that pair on this
// platform. That is stated rather than worked around: the alternative would be
// moving the paths into a platform-neutral file so an untagged test could see
// them, and a platform-neutral file that knows about Contents/Resources is a
// worse thing than a test that only runs under one build.
//
// The wiring — that doInit calls these at all, and in the right order — IS
// covered at Gate A, by TestInitResolvesTheBundleThroughThePlatformSeam in
// gst_stub_test.go, which reads the source rather than running it.
//
//	PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig CGO_ENABLED=1 go test ./internal/gst/
package gst

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBundlePluginDirLandsInResourcesAndNotInFrameworks pins the answer that
// codesign forces on this platform.
//
// Contents/Frameworks is where a directory of dylibs obviously belongs and is
// the one place it may not go: codesign treats every DIRECTORY under
// Contents/Frameworks as nested code and requires it to be a bundle, so a plain
// directory there makes the whole .app unsignable —
//
//	bundle format unrecognized, invalid, or unsuitable
//	In subcomponent: .../Contents/Frameworks/gstreamer-1.0
//
// That failure arrives at the very END of a release: after the build, after the
// staging, at the signing step, and it reads like a signing problem rather than
// a layout one. This test is here so the layout cannot drift back to the
// obvious answer without something saying so first.
//
// It also pins the ".." resolution, which is the part that is easy to get
// subtly wrong. appDir is Contents/MACOS, so the path has to climb one level
// before descending into Resources; a Join that forgets to climb produces
// Contents/MacOS/Resources/gstreamer-1.0, which does not exist and which fails
// with "bundled plugin directory is not readable" — a message that sends the
// reader to look at the bundler rather than at this line.
func TestBundlePluginDirLandsInResourcesAndNotInFrameworks(t *testing.T) {
	const appDir = "/Applications/WSL Commentary.app/Contents/MacOS"
	got := bundlePluginDir(appDir)

	want := "/Applications/WSL Commentary.app/Contents/Resources/gstreamer-1.0"
	if got != want {
		t.Fatalf("bundlePluginDir(%q)\n = %q\nwant %q", appDir, got, want)
	}
	if strings.Contains(got, "Frameworks") {
		t.Error("the plugin directory is under Contents/Frameworks. codesign treats every " +
			"directory there as nested code and refuses to sign the bundle at all")
	}
	if strings.Contains(got, "MacOS") {
		t.Errorf("bundlePluginDir = %q still contains MacOS: the \"..\" did not resolve, so this "+
			"is a path inside the executable's own directory rather than beside it", got)
	}
}

// TestExtraInitEnvNeutralisesEveryCompiledInHomebrewPath is the one that
// protects a silent failure rather than a loud one.
//
// Each variable below exists because a Homebrew path is compiled into a
// vendored dylib as a C STRING, which install_name_tool cannot rewrite and the
// bundler's load-command audit cannot see. Drop any of them and the application
// still starts:
//
//   - no GST_PLUGIN_SCANNER: GStreamer prints one warning and scans plugins IN
//     PROCESS. Measured with the path forced to /nonexistent — registry still
//     built, 17 plugins, 291 features, every element resolved.
//   - no GIO_MODULE_DIR: on a Mac that has Homebrew, a foreign glib's GIO
//     modules are dlopened next to our own vendored glib. No message at all.
//   - no ORC_CODE: every log this product produces opens with an ERROR naming
//     an entitlement that does not fix it.
//
// None of those is visible from the outside, which is why they need a test
// rather than a first run.
func TestExtraInitEnvNeutralisesEveryCompiledInHomebrewPath(t *testing.T) {
	// A real directory with a real gst-plugin-scanner in it, because
	// extraInitEnv refuses rather than returning a path to nothing.
	appDir := t.TempDir()
	scanner := filepath.Join(appDir, "gst-plugin-scanner")
	if err := os.WriteFile(scanner, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("staging a fake scanner: %v", err)
	}

	vars, err := extraInitEnv(appDir)
	if err != nil {
		t.Fatalf("extraInitEnv() error = %v", err)
	}

	got := make(map[string]string, len(vars))
	for _, v := range vars {
		if _, dup := got[v.name]; dup {
			t.Errorf("extraInitEnv returns %s twice; the later one silently wins", v.name)
		}
		got[v.name] = v.value
	}

	// Both spellings of the scanner. Setting one and not the other leaves the
	// other pointing into /opt/homebrew on a machine that has /opt/homebrew.
	for _, name := range []string{"GST_PLUGIN_SCANNER_1_0", "GST_PLUGIN_SCANNER"} {
		if got[name] != scanner {
			t.Errorf("%s = %q, want the bundled scanner %q", name, got[name], scanner)
		}
	}
	if got["ORC_CODE"] != "backup" {
		t.Errorf("ORC_CODE = %q, want \"backup\": liborc cannot obtain a writable-executable "+
			"mapping under the hardened runtime on Apple silicon and logs at ERROR level on "+
			"every start. The entitlement it names does not fix it — measured", got["ORC_CODE"])
	}
	if gio := got["GIO_MODULE_DIR"]; !strings.HasSuffix(gio, filepath.Join("Resources", "gio-modules")) {
		t.Errorf("GIO_MODULE_DIR = %q, want the bundle's own empty gio-modules directory. "+
			"Unset, libgio scans /opt/homebrew/lib/gio/modules from a compiled-in string and "+
			"loads a FOREIGN glib's modules into this process next to our vendored one", gio)
	}

	// And no variable may be left empty. GLib's empty-to-NULL behaviour is the
	// one link in this chain that cannot be tested from Go, and an empty value
	// that GLib reads as unset falls through to the compiled-in Homebrew path —
	// which is the exact failure every variable here exists to prevent.
	for name, value := range got {
		if value == "" {
			t.Errorf("%s is set to the empty string. If GLib maps that to NULL it falls back to "+
				"the compiled-in /opt/homebrew path, which is what this variable exists to stop", name)
		}
	}
}

// TestExtraInitEnvRefusesAMissingScanner is the other half of the scanner
// decision: it is a HARD ERROR and not a warning.
//
// The temptation is to shrug — GStreamer copes, the fallback works, and the
// application starts. It copes right up until a plugin faults on load, which is
// the day the out-of-process scanner exists for, and it then takes the
// commentary position down instead of printing a warning. A bundle without its
// scanner is a broken bundle and must fail while somebody is still building it.
func TestExtraInitEnvRefusesAMissingScanner(t *testing.T) {
	vars, err := extraInitEnv(t.TempDir())
	if err == nil {
		t.Fatalf("extraInitEnv() with no gst-plugin-scanner returned %v and no error", vars)
	}
	if !strings.Contains(err.Error(), "gst-plugin-scanner") {
		t.Errorf("error %q does not name gst-plugin-scanner, so a reader cannot tell which file "+
			"the bundler failed to stage", err)
	}
}
