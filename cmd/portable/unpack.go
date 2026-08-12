// Package main implements wslcomms-portable: a single .exe that carries the
// whole application inside it.
//
// # WHY THIS EXISTS
//
// wslcomms.exe cannot be copied to another machine on its own. Its PE import
// table names four GStreamer DLLs - libglib-2.0-0.dll, libgobject-2.0-0.dll,
// libgstreamer-1.0-0.dll and libgstvideo-1.0-0.dll - and the Windows loader
// resolves those when the process is created, before any Go code runs. A user
// who copies just the .exe gets a loader error box, not a program.
//
// That same fact rules out the obvious design. This launcher cannot simply
// unpack DLLs next to itself and carry on, because by the time its own main()
// runs the loader has already succeeded or already failed. So the launcher
// imports no GStreamer at all: it is plain Go, it unpacks the real application
// to a directory, and it starts that directory's wslcomms.exe as a new process
// whose loader then finds everything beside it.
//
// This file holds the parts that are pure logic and are unit-tested. The
// Windows-specific parts - where to unpack, how to start the child, how to
// report a failure with no console attached - are in main.go.
package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// appExeName is the application this launcher starts, as named inside the
// payload. It must match the file build\pack-portable.ps1 packs.
const appExeName = "wslcomms.exe"

// unpackPrefix names the temporary directories used while unpacking. The dot
// keeps them out of the way and the prefix makes them identifiable as ours, so
// pruneRuntimes can clear up after a run that was killed mid-unpack.
const unpackPrefix = ".unpack-"

// runtimeIDLen is how many hex characters of the payload digest name the
// unpacked runtime directory. Sixteen is 64 bits: far more than enough to keep
// two builds apart, and short enough that the path stays readable in a bug
// report.
const runtimeIDLen = 16

// safeRelPath converts a zip entry name to a relative path that is guaranteed
// to stay inside the destination directory, or returns an error.
//
// This is the Zip Slip check. The payload is built by our own packing script,
// so in the normal case nothing here can fail - but "the archive is trusted"
// is exactly the assumption that makes this class of bug ship, and the check
// costs one function call per entry. An archive entry named ..\..\Windows\..
// must never be able to write outside the unpack directory.
func safeRelPath(name string) (string, error) {
	if name == "" {
		return "", errors.New("archive entry has an empty name")
	}
	// Zip stores forward slashes by specification. A backslash here means the
	// archive was built by something that ignored that, and since Windows
	// treats both as separators, letting it through would bypass the checks
	// below.
	if strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("archive entry %q contains a backslash", name)
	}
	if path.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("archive entry %q is an absolute path", name)
	}
	// Drive-relative ("C:foo") and UNC forms.
	if len(name) >= 2 && name[1] == ':' {
		return "", fmt.Errorf("archive entry %q names a drive", name)
	}

	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("archive entry %q escapes the destination", name)
	}
	return filepath.FromSlash(clean), nil
}

// unpackZip writes every entry of the zip in payload into destDir, which must
// already exist.
func unpackZip(payload []byte, destDir string) error {
	zr, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return fmt.Errorf("the embedded payload is not a readable archive: %w", err)
	}
	if len(zr.File) == 0 {
		return errors.New("the embedded payload is an empty archive")
	}

	var sawApp bool
	for _, f := range zr.File {
		rel, err := safeRelPath(f.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, rel)

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("creating %q: %w", target, err)
			}
			continue
		}
		if strings.EqualFold(rel, appExeName) {
			sawApp = true
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("creating %q: %w", filepath.Dir(target), err)
		}
		if err := writeZipEntry(f, target); err != nil {
			return err
		}
	}

	// A payload without the application in it would unpack cleanly and then
	// fail at the launch step with a confusing "file not found". Catch it here,
	// where the message can say what is actually wrong.
	if !sawApp {
		return fmt.Errorf("the embedded payload does not contain %s", appExeName)
	}
	return nil
}

// writeZipEntry copies one file out of the archive.
func writeZipEntry(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("reading %q from the payload: %w", f.Name, err)
	}
	defer rc.Close()

	// O_EXCL: two entries with the same name, or a leftover file, should be an
	// error rather than a silent overwrite that leaves a half-written binary.
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return fmt.Errorf("creating %q: %w", target, err)
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return fmt.Errorf("writing %q: %w", target, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", target, err)
	}
	return nil
}

// materialise ensures baseDir\id holds an unpacked copy of payload, and returns
// the path of the application executable inside it.
//
// The unpack is atomic by construction: everything is written to a temporary
// directory alongside the target and then renamed into place, so baseDir\id
// either does not exist or is complete. Nothing observes a partially unpacked
// runtime - which matters because the failure mode of a partial unpack is the
// one that has already bitten this project once, in the form of a GStreamer
// bundle missing three plugins.
//
// It is also safe to run twice at once. Two launcher processes racing will both
// unpack to their own temporary directory; the first to rename wins, and the
// loser notices the destination now exists and uses it.
func materialise(baseDir, id string, payload []byte) (string, error) {
	dir := filepath.Join(baseDir, id)
	exe := filepath.Join(dir, appExeName)

	if st, err := os.Stat(exe); err == nil && !st.IsDir() {
		return exe, nil // already unpacked by a previous run
	}

	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return "", fmt.Errorf("creating %q: %w", baseDir, err)
	}

	tmp, err := os.MkdirTemp(baseDir, unpackPrefix)
	if err != nil {
		return "", fmt.Errorf("creating a temporary directory in %q: %w", baseDir, err)
	}
	// Removes the temporary directory when unpacking failed. After a successful
	// rename there is nothing at tmp and this is a no-op.
	defer os.RemoveAll(tmp)

	if err := unpackZip(payload, tmp); err != nil {
		return "", err
	}

	if err := os.Rename(tmp, dir); err != nil {
		// Losing the race is not an error: another launcher put a complete
		// runtime there while this one was unpacking, and a complete runtime is
		// all we wanted. Any other failure is real.
		if st, statErr := os.Stat(exe); statErr == nil && !st.IsDir() {
			return exe, nil
		}
		return "", fmt.Errorf("moving the unpacked runtime into %q: %w", dir, err)
	}
	return exe, nil
}

// pruneRuntimes deletes unpacked runtimes in baseDir other than keepID, and any
// temporary directory left behind by an interrupted unpack.
//
// Every failure here is ignored on purpose. The common reason a delete fails is
// that another instance of the application is running from that directory and
// Windows will not unlink a mapped executable - which is not a problem worth
// telling anyone about, and certainly not a reason to refuse to start. The
// directory simply gets cleared up on a later run.
func pruneRuntimes(baseDir, keepID string) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keepID {
			continue
		}
		// Only ever remove things this launcher creates: a runtime directory
		// named after a payload digest, or one of our temporary directories.
		if !strings.HasPrefix(e.Name(), unpackPrefix) && !isRuntimeID(e.Name()) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(baseDir, e.Name()))
	}
}

// isRuntimeID reports whether a directory name looks like one materialise
// created: a lower-case hex digest prefix of the fixed length used by runtimeID.
func isRuntimeID(name string) bool {
	if len(name) != runtimeIDLen {
		return false
	}
	for _, r := range name {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
