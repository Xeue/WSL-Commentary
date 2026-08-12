//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// appDirName is the folder under %LOCALAPPDATA% that the application already
// uses for its GStreamer registry, so the unpacked runtimes sit beside state
// the user would expect to find there rather than in a second location.
const appDirName = "WSLComms"

func main() {
	if err := run(); err != nil {
		fatal(err)
	}
}

func run() error {
	if len(payload) == 0 {
		return errors.New("this launcher was built without its payload. " +
			"Build it with build\\pack-portable.ps1, which packs build\\bin " +
			"into the executable; 'go build ./cmd/portable' alone produces " +
			"this non-functional stub")
	}

	// The runtime directory is named after the payload's digest. That gives
	// upgrades for free: a new build has new content, so it unpacks to a new
	// directory and cannot half-overwrite the one a running instance is using.
	sum := sha256.Sum256(payload)
	id := hex.EncodeToString(sum[:])[:runtimeIDLen]

	base, err := runtimeBase()
	if err != nil {
		return err
	}

	exe, err := materialise(base, id, payload)
	if err != nil {
		return err
	}

	// After the current runtime is known good, not before: a failed unpack must
	// not take the previous working copy with it.
	pruneRuntimes(base, id)

	return launch(exe)
}

// runtimeBase returns %LOCALAPPDATA%\WSLComms\runtime.
//
// Deliberately not next to the launcher itself. The whole point of a portable
// build is that it runs from wherever it was dropped - a read-only share, a
// USB stick, the Downloads folder of a machine where the user cannot write to
// Program Files - and none of those can be assumed writable.
func runtimeBase() (string, error) {
	dir := os.Getenv("LOCALAPPDATA")
	if dir == "" {
		// os.UserCacheDir resolves %LocalAppData% on Windows, so this is the
		// same location by another route rather than a different policy.
		var err error
		dir, err = os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("neither LOCALAPPDATA nor a user cache directory is available, "+
				"so there is nowhere to unpack to: %w", err)
		}
	}
	return filepath.Join(dir, appDirName, "runtime"), nil
}

// launch starts the unpacked application and returns without waiting for it.
//
// Not waiting is a deliberate choice. The launcher's only job is to make the
// application startable; once the child is running, a parent sitting in a Wait
// would be a second wslcomms entry in Task Manager for the whole session, doing
// nothing. The cost is that the child's exit code is not propagated, which
// nothing consumes: this is a GUI application, and the shell does not wait for
// it either.
func launch(exe string) error {
	cmd := exec.Command(exe, os.Args[1:]...)

	// The application resolves its GStreamer bundle relative to its own
	// executable, so the working directory does not strictly matter - but
	// pointing it at the runtime directory keeps any relative path the
	// application opens inside the unpacked tree rather than wherever the
	// launcher happened to be started from.
	cmd.Dir = filepath.Dir(exe)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %q: %w", exe, err)
	}
	// Release lets this process exit without leaving the child parented to a
	// handle nobody will close.
	return cmd.Process.Release()
}

// fatal reports an error and exits.
//
// A message box rather than stderr, because this binary is linked with
// -H windowsgui and therefore has no console: anything written to stderr goes
// nowhere at all. An unpack that fails silently, leaving the user double
// clicking an icon that does nothing, is the worst possible outcome for the one
// piece of software whose entire purpose is to be easy to run.
func fatal(err error) {
	msg := "WSL Commentary could not start.\r\n\r\n" + err.Error()

	title, terr := windows.UTF16PtrFromString("WSL Commentary")
	body, berr := windows.UTF16PtrFromString(msg)
	if terr == nil && berr == nil {
		// MB_ICONERROR | MB_OK
		_, _ = windows.MessageBox(0, body, title, 0x00000010|0x00000000)
	}
	os.Exit(1)
}
