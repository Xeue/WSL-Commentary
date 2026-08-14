//go:build darwin

package applog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultDirIsNotSomewhereTheSystemMayDelete is the whole reason DefaultDir
// is a platform twin rather than one function with a fallback.
//
// The Windows implementation resolves LOCALAPPDATA and falls back to
// os.UserCacheDir, which on Windows is the same directory. On macOS
// LOCALAPPDATA is always empty, so that fallback is not a fallback at all — it
// is the answer, and it is ~/Library/Caches, which Apple documents as purgeable
// and which the system may empty under disk pressure. The log file is the only
// diagnosis a headless commentary position ever produces. Losing it
// intermittently, on the machine that has been running hardest, is the exact
// failure this package was written to prevent, wearing a different hat.
//
// The assertion is deliberately about what the path must NOT be. Pinning the
// literal directory would be a change-detector; pinning "not a cache" is the
// property that matters and the one somebody could plausibly undo while tidying
// two similar-looking functions into one.
//
// It lives under //go:build darwin, and not beside the rest of the package's
// tests, because that same sentence is FALSE on Windows: os.UserCacheDir there
// returns %LocalAppData%, the very root DefaultDir is documented to use, so an
// untagged version of this test would have gone red on the first Windows run
// against a function nobody had touched. The property being guarded is a macOS
// property. If this test ever stops appearing in `go test -v ./internal/applog`
// on a Mac, the build tag is wrong and the guard is gone — which is a silent
// failure, so check for the name rather than for a green run.
func TestDefaultDirIsNotSomewhereTheSystemMayDelete(t *testing.T) {
	dir, err := DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir() error = %v", err)
	}

	cache, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("no user cache directory on this machine to compare against: %v", err)
	}
	if rel, err := filepath.Rel(cache, dir); err == nil && !strings.HasPrefix(rel, "..") {
		t.Errorf("DefaultDir() = %q is inside the user cache directory %q. On macOS that is "+
			"~/Library/Caches, which the operating system is entitled to delete without "+
			"warning; the log belongs in ~/Library/Logs. internal/gst's registryFile makes the "+
			"opposite choice on purpose — a purged plugin registry costs one rescan, a purged "+
			"log costs the only evidence there was", dir, cache)
	}
}
