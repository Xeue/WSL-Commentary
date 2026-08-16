//go:build live && cgo && !gststub

// kindprobe_live_test.go answers one question the operator asked out loud:
// "what ever happened to the channel selector on the inputs?"
//
// The selector is gated, in this order, on:
//
//   1. ListInputDevices offering the card at all,
//   2. that entry carrying Kind == KindDeckLink on the wire, because the
//      commentary-input picker uses the kind to decide which optgroup an entry
//      belongs to and what audioSourceKind to write, and
//   3. audioSourceKind reading "decklink", which is what un-hides the DeckLink
//      channel routing group in Settings.
//
// Step 2 is the one that has been silently false twice — once because Kind
// carried `omitempty` and a legitimate value was dropped from the frame, and
// once because hardened-runtime library validation stopped the DeckLink plugin
// loading at all, so nothing was enumerated to carry it. Both are fixed; this
// test is what stops either returning without anybody noticing.

package gst

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// liveInitDarwin resolves a macOS GStreamer for the live tests and calls Init.
//
// ==================== WHY THIS DOES NOT USE THE .app ========================
//
// It is the obvious thing to do and it SEGFAULTS. Since bundle-gst-darwin.sh
// started producing a genuinely self-contained bundle, every plugin in it
// resolves the GStreamer core through @loader_path:
//
//	otool -L .../Resources/gstreamer-1.0/libgstdecklink.dylib
//	  @loader_path/../../Frameworks/libgstreamer-1.0.0.dylib
//	  @loader_path/../../Frameworks/libglib-2.0.0.dylib
//
// while THIS test binary is linked by cgo against Homebrew's copies of the same
// libraries. Loading the bundle's plugins here puts two GObject type systems in
// one process, and the second one to register a type finds the first's ids
// meaningless:
//
//	g_param_spec_boxed: assertion 'G_TYPE_IS_BOXED (boxed_type)' failed
//	cannot initialize GValue with type '(null)'
//	ERROR: Caught a segmentation fault while loading plugin file: libgstdecklink
//
// That is the bundle being CORRECT — the shipped .app has no Homebrew to mix
// with — so the fix belongs here rather than in the bundler. The live tests need
// a plugin set that matches what they are linked against, which means Homebrew's.
//
// Set WSLCOMMS_LIVE_APP_DIR to a directory shaped like <App>.app/Contents/MacOS
// whose sibling Resources/gstreamer-1.0 and Resources/gio-modules are symlinks
// into /opt/homebrew, with a gst-plugin-scanner symlink beside the binary:
//
//	mkdir -p live.app/Contents/{MacOS,Resources}
//	ln -s /opt/homebrew/lib/gstreamer-1.0  live.app/Contents/Resources/gstreamer-1.0
//	ln -s /opt/homebrew/lib/gio/modules    live.app/Contents/Resources/gio-modules
//	ln -s /opt/homebrew/Cellar/gstreamer/*/libexec/gstreamer-1.0/gst-plugin-scanner \
//	      live.app/Contents/MacOS/gst-plugin-scanner
//
// The default below is that layout under the repo's build directory, so a run
// with no environment set says what is missing rather than crashing.
func liveInitDarwin(t *testing.T) {
	t.Helper()
	appDir := os.Getenv("WSLCOMMS_LIVE_APP_DIR")
	if appDir == "" {
		abs, err := filepath.Abs(filepath.Join("..", "..", "build", "live.app", "Contents", "MacOS"))
		if err != nil {
			t.Fatalf("resolving the app directory: %v", err)
		}
		appDir = abs
	}
	if _, err := os.Stat(bundlePluginDir(appDir)); err != nil {
		t.Skipf("no GStreamer plugin directory at %s — see this function's comment for the "+
			"symlink farm the live tests need, and do NOT point them at the shipped .app: %v",
			bundlePluginDir(appDir), err)
	}
	if err := Init(appDir); err != nil {
		t.Fatalf("Init: %v", err)
	}
}

func TestLiveDeckLinkIsOfferedWithItsKind(t *testing.T) {
	liveInitDarwin(t)

	devices, err := ListInputDevices()
	if err != nil {
		t.Fatalf("ListInputDevices: %v", err)
	}

	var cards []Device
	for _, d := range devices {
		t.Logf("offered: kind=%-8s name=%q id=%s", d.Kind, d.Name, d.ID)
		if d.Kind == KindDeckLink {
			cards = append(cards, d)
		}
	}

	if len(cards) == 0 {
		t.Skip("no DeckLink enumerated on this machine; nothing to prove about the gate")
	}

	for _, d := range cards {
		// THE WIRE, not the struct. The picker reads the JSON, and the bug that
		// hid this control was a tag rather than a value.
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("marshal %q: %v", d.Name, err)
		}
		if !strings.Contains(string(raw), `"kind":"decklink"`) {
			t.Errorf("REGRESSION: %q reaches the frontend as %s — with no kind on the wire the "+
				"picker cannot put it in the DeckLink optgroup, audioSourceKind never reads "+
				"'decklink', and the channel routing group stays hidden for ever", d.Name, raw)
		}
		if d.ID == "" {
			t.Errorf("%q carries no id; the picker has nothing to write into the settings", d.Name)
		}
	}
	t.Logf("%d DeckLink capture device(s) offered with kind and persistent id intact", len(cards))
}
