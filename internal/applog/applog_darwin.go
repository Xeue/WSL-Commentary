//go:build darwin

// applog_darwin.go answers the one question this package cannot answer without
// knowing which operating system it is on: where the log files go.
//
// Owner: WP-8. Read the package doc in applog.go first — it carries the argument
// for both platforms, and in particular the reason this file exists at all
// rather than letting the Windows fallback answer here.
package applog

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultDir returns ~/Library/Logs/WSLComms.
//
// # Why not the fallback that was already there
//
// LOCALAPPDATA is empty on macOS, so the Windows implementation would have
// fallen through to os.UserCacheDir and landed at
// ~/Library/Caches/WSLComms/logs. That compiles, runs, and is wrong.
//
// Apple documents ~/Library/Caches as PURGEABLE: the system may delete its
// contents when the disk is short, without warning and without asking. The whole
// reason this package exists is that a headless commentary position produces no
// diagnosis except this file, discovered the expensive way when an SRT return
// failed on a machine elsewhere and there was nothing to read. Putting that file
// somewhere the operating system is entitled to delete reintroduces the same
// failure — intermittently, on the machine that has been running hardest all
// day, which is the worst possible way to lose it.
//
// ~/Library/Logs is the macOS convention for exactly this file. Console.app
// surfaces it under Log Reports, and anybody asked for their logs is already
// looking there.
//
// # Why there is no "logs" component on the end
//
// The Windows path is <data root>\WSLComms\logs because %LOCALAPPDATA% is a
// general-purpose per-user data root shared with config.json's sibling and the
// GStreamer registry. ~/Library/Logs is ALREADY the log root, so appending
// another "logs" would give ~/Library/Logs/WSLComms/logs — a directory that
// reads like a mistake and that nobody would think to look inside. This is why
// DefaultDir is a whole function per platform rather than a shared join over a
// per-platform base: the two answers differ in shape, not just in prefix.
//
// # Not the Application Support directory
//
// config.json lives in ~/Library/Application Support/WSLComms, and putting the
// log beside it was the obvious alternative. It is rejected for the reason the
// Windows half chose %LOCALAPPDATA% over %APPDATA%: logs are machine-local
// diagnostic state, Application Support is the user's own data, and the two want
// different treatment from backups, from sync and from anybody clearing space.
//
// # os.UserHomeDir rather than $HOME directly
//
// Same value in practice, but it is the documented accessor and it returns an
// error rather than an empty string when there is no home to speak of — which is
// the case in a sandbox this application is not built for, and which must
// produce a message rather than a log file quietly written to "/Library/Logs".
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("applog: no home directory to put the log directory in: %w", err)
	}
	return filepath.Join(home, "Library", "Logs", "WSLComms"), nil
}
