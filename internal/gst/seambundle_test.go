//go:build !cgo || gststub

// seambundle_test.go is the Gate A guard over the PACKAGING of the seam.
//
// The capture/send seam is proxysink on the capture side and proxysrc on the
// send side. Neither is a nicety and neither is conditional: every seat has the
// seam, so a bundle that does not stage libgstproxy is not a seat with a
// degraded preview — it is an application that cannot start at all, because
// requiredElements refuses Init without both factories.
//
// That refusal is the run-time control and it is the right one; this file is
// the earlier one. It reads the two bundlers and the cgo source as TEXT,
// because Gate A compiles neither gst_cgo.go nor a shell script, and it fails
// in front of the person who is still editing them rather than in front of an
// operator with a bundle that will not launch.

package gst

import (
	"strings"
	"testing"
)

// TestTheProxyPluginIsStagedByBothBundlers is the twin of
// TestTheVolumePluginIsStagedByBothBundlers, for the same reason and against
// the same failure.
//
// ONE plugin file supplies both factories on both platforms — libgstproxy.dylib
// (75,344 bytes, gst-plugins-bad, License LGPL, read out of gst-inspect-1.0
// proxysink against Homebrew's 1.26.10 on macOS arm64) and libgstproxy.dll,
// whose existence in the official MinGW build is REASONED rather than measured
// and is Gate B's to confirm. The macOS half of this test is therefore a
// measurement and the Windows half is a contract: if the Windows file turns out
// to be called something else, the fix is a second candidate name in
// bundle-gst.ps1, and this test keeps failing until it is there.
func TestTheProxyPluginIsStagedByBothBundlers(t *testing.T) {
	for _, b := range []struct{ path, want string }{
		{"../../build/bundle-gst.ps1", "libgstproxy.dll"},
		{"../../build/bundle-gst-darwin.sh", "proxysink proxysrc"},
	} {
		src := readRepoFile(t, b.path)
		if !strings.Contains(src, b.want) {
			t.Errorf("%s does not stage %q. Every seat's pipeline crosses the capture/send seam, "+
				"so a bundle without that plugin is not a seat with no preview — it is Init "+
				"refusing to start the application on every machine", b.path, b.want)
		}
	}
}

// TestTheSeamElementsAreRequiredAtInit reads requiredElements as source.
//
// It is a text check and not a call, because requiredElements lives in
// gst_cgo.go, which Gate A does not compile — the same reason the guards in
// gst_stub_test.go parse that file rather than exercise it. What it defends is
// the pairing of FACTORY to PLUGIN: GStreamer's own failure is `no element
// "proxysink"`, and neither factory is named after the plugin that provides it,
// so an entry that carried the wrong plugin name would send whoever is staging
// a bundle looking for a file called libgstproxysink.
func TestTheSeamElementsAreRequiredAtInit(t *testing.T) {
	src := readRepoFile(t, cgoSourceFile)
	for _, want := range []string{`{"proxysink", "proxy"}`, `{"proxysrc", "proxy"}`} {
		if !strings.Contains(src, want) {
			t.Errorf("%s does not carry %s in requiredElements. Without it a bundle missing "+
				"libgstproxy starts the app, and the operator meets it as gst_parse_launch's "+
				"bare `no element \"proxysink\"` at Start instead of a named plugin at launch",
				cgoSourceFile, want)
		}
	}
}
