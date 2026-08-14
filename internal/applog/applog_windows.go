//go:build windows

// applog_windows.go answers the one question this package cannot answer without
// knowing which operating system it is on: where the log files go.
//
// Owner: WP-8. Read the package doc in applog.go first — it carries the argument
// for both platforms, and this file is only the Windows half of it. Nothing here
// is new: it is what DefaultDir has always done, moved out of applog.go unchanged
// so that macOS can differ without either spelling drifting from the other.
package applog

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultDir returns %LOCALAPPDATA%\WSLComms\logs, resolved the same way
// internal/gst resolves the registry's home: LOCALAPPDATA first, and
// os.UserCacheDir as the fallback that reaches the same place by another route
// (on Windows it returns %LocalAppData%, so this is one directory by two paths
// rather than two policies).
//
// The two files should always be neighbours on this platform — they are the same
// kind of state, and a support bundle collects the one directory. That is
// deliberately NOT true on macOS, where the registry is a genuine cache and the
// log is not; see the package doc.
func DefaultDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("applog: neither LOCALAPPDATA nor a user cache directory is available: %w", err)
		}
	}
	return filepath.Join(base, "WSLComms", "logs"), nil
}
