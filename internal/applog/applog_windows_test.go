//go:build windows

package applog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultDirIsUnderLocalAppData is the Windows half of the guard that
// applog_darwin_test.go carries for macOS: the two of them together are what
// stops somebody tidying the DefaultDir twins back into one function with a
// fallback, which is the change that would quietly move the macOS log into
// purgeable ~/Library/Caches.
//
// The macOS assertion cannot be reused here. It says "not inside
// os.UserCacheDir", and on Windows os.UserCacheDir returns %LocalAppData% —
// the root DefaultDir is documented to use and has used since the package was
// written. Stated untagged it would fail by construction on the on-air
// platform, against a function that is byte-identical to the one shipping now.
//
// So this states the property that is actually true on Windows, in the same
// shape and for the same reason: not the literal directory, which would be a
// change-detector that a new Windows release could break for no good cause, but
// the two things a tidy-up would break. First, the log stays under the
// machine-local data root — %LOCALAPPDATA%, not roaming %APPDATA%, so a log
// from another machine never mixes into this one's on a roaming profile.
// Second, the path still ends in WSLComms\logs, so it remains a subdirectory of
// a general-purpose root rather than acquiring the flat macOS shape, where
// ~/Library/Logs is already the log root and a "logs" component would be
// duplication.
func TestDefaultDirIsUnderLocalAppData(t *testing.T) {
	dir, err := DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir() error = %v", err)
	}

	// An unset LOCALAPPDATA sends DefaultDir down its os.UserCacheDir fallback,
	// which reaches the same directory by another route but leaves nothing
	// independent to compare against — the test would be asserting the
	// implementation against itself. Every real Windows session has it set; a
	// stripped environment is not the case worth failing over.
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		t.Skip("LOCALAPPDATA is unset, so DefaultDir took its fallback and there is no independent root to check")
	}

	rel, err := filepath.Rel(base, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("DefaultDir() = %q is not under %%LOCALAPPDATA%% (%q). Logs are machine-local "+
			"diagnostic state and belong beside internal/gst's registry.bin, not in roaming "+
			"%%APPDATA%% where a support bundle would collect one machine's log from another "+
			"machine's profile", dir, base)
	}

	if want := filepath.Join("WSLComms", "logs"); !strings.HasSuffix(dir, want) {
		t.Errorf("DefaultDir() = %q does not end in %q. %%LOCALAPPDATA%% is a general-purpose "+
			"per-user data root shared with the GStreamer registry, so the log needs its own "+
			"subdirectory rather than scattering a dated file per run across it — that "+
			"subdirectory is what a support bundle is told to collect", dir, want)
	}
}
