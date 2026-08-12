package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildZip makes an in-memory zip from name->content pairs, so tests never need
// the real 20 MB payload.
func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("creating zip entry %q: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("writing zip entry %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}
	return buf.Bytes()
}

// validPayload is the smallest archive that unpackZip accepts.
func validPayload(t *testing.T) []byte {
	t.Helper()
	return buildZip(t, map[string]string{
		appExeName:                       "MZ fake executable",
		"gst/lib/gstreamer-1.0/plug.dll": "plugin bytes",
		"libglib-2.0-0.dll":              "runtime bytes",
	})
}

func TestSafeRelPath_RejectsEscapes(t *testing.T) {
	// Zip Slip and its neighbours. Every one of these must be refused, because
	// each would write outside the unpack directory.
	bad := []string{
		"",
		"..",
		"../evil.dll",
		"../../Windows/System32/evil.dll",
		"a/../../evil.dll",
		"/absolute.dll",
		"C:/drive.dll",
		"C:relative.dll",
		`..\windows.dll`,
		`sub\dir.dll`, // backslash: not a legal zip separator, and Windows honours it
	}
	for _, name := range bad {
		t.Run(name, func(t *testing.T) {
			if got, err := safeRelPath(name); err == nil {
				t.Fatalf("safeRelPath(%q) returned %q and no error; it must be refused", name, got)
			}
		})
	}
}

func TestSafeRelPath_AcceptsOrdinaryNames(t *testing.T) {
	cases := map[string]string{
		"wslcomms.exe":                   "wslcomms.exe",
		"gst/lib/gstreamer-1.0/plug.dll": filepath.Join("gst", "lib", "gstreamer-1.0", "plug.dll"),
		"./slate.png":                    "slate.png",
		"licenses/NOTICE.txt":            filepath.Join("licenses", "NOTICE.txt"),
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			got, err := safeRelPath(in)
			if err != nil {
				t.Fatalf("safeRelPath(%q): unexpected error %v", in, err)
			}
			if got != want {
				t.Fatalf("safeRelPath(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestUnpackZip_WritesEveryEntry(t *testing.T) {
	dir := t.TempDir()
	if err := unpackZip(validPayload(t), dir); err != nil {
		t.Fatalf("unpackZip: %v", err)
	}

	want := map[string]string{
		appExeName: "MZ fake executable",
		filepath.Join("gst", "lib", "gstreamer-1.0", "plug.dll"): "plugin bytes",
		"libglib-2.0-0.dll": "runtime bytes",
	}
	for rel, body := range want {
		got, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		if string(got) != body {
			t.Fatalf("%s = %q, want %q", rel, got, body)
		}
	}
}

func TestUnpackZip_RefusesAnEscapingEntry(t *testing.T) {
	dir := t.TempDir()
	payload := buildZip(t, map[string]string{
		appExeName:       "MZ",
		"../escaped.txt": "should never be written",
	})

	err := unpackZip(payload, dir)
	if err == nil {
		t.Fatal("unpackZip accepted an archive containing ../escaped.txt")
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(dir), "escaped.txt")); statErr == nil {
		t.Fatal("unpackZip wrote a file outside the destination directory")
	}
}

func TestUnpackZip_RejectsAPayloadWithoutTheApplication(t *testing.T) {
	// A payload that unpacks cleanly but has no wslcomms.exe would otherwise
	// fail later, at the launch step, with a much less useful message.
	payload := buildZip(t, map[string]string{"libglib-2.0-0.dll": "runtime"})

	err := unpackZip(payload, t.TempDir())
	if err == nil {
		t.Fatal("unpackZip accepted a payload with no application in it")
	}
	if !strings.Contains(err.Error(), appExeName) {
		t.Fatalf("error should name the missing executable, got: %v", err)
	}
}

func TestUnpackZip_RejectsRubbish(t *testing.T) {
	if err := unpackZip([]byte("not a zip at all"), t.TempDir()); err == nil {
		t.Fatal("unpackZip accepted bytes that are not an archive")
	}
	if err := unpackZip(buildZip(t, map[string]string{}), t.TempDir()); err == nil {
		t.Fatal("unpackZip accepted an empty archive")
	}
}

func TestMaterialise_UnpacksThenReusesWithoutRewriting(t *testing.T) {
	base := t.TempDir()
	payload := validPayload(t)

	exe, err := materialise(base, "abcdef0123456789", payload)
	if err != nil {
		t.Fatalf("first materialise: %v", err)
	}
	if _, err := os.Stat(exe); err != nil {
		t.Fatalf("application not present after materialise: %v", err)
	}

	// Mark the unpacked copy. A second call must not unpack again - if it did,
	// it would be rewriting binaries that a running instance may have mapped.
	marker := filepath.Join(filepath.Dir(exe), "marker.txt")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing marker: %v", err)
	}

	exe2, err := materialise(base, "abcdef0123456789", payload)
	if err != nil {
		t.Fatalf("second materialise: %v", err)
	}
	if exe2 != exe {
		t.Fatalf("second call returned %q, want %q", exe2, exe)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the second call re-unpacked over an existing runtime")
	}
}

func TestMaterialise_LeavesNothingBehindWhenUnpackingFails(t *testing.T) {
	// The property that matters: a failed unpack must not leave a partial
	// runtime that a later run would mistake for a complete one. That is
	// precisely the failure this project has already hit once, with a
	// half-written GStreamer bundle.
	base := t.TempDir()
	bad := buildZip(t, map[string]string{"../escape.txt": "no"})

	if _, err := materialise(base, "0123456789abcdef", bad); err == nil {
		t.Fatal("materialise succeeded on an archive it should have refused")
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("reading base: %v", err)
	}
	for _, e := range entries {
		t.Fatalf("failed unpack left %q behind; the directory should be empty", e.Name())
	}
}

func TestPruneRuntimes_KeepsCurrentAndRemovesOld(t *testing.T) {
	base := t.TempDir()
	keep := "1111111111111111"
	old := "2222222222222222"
	tmpLeftover := unpackPrefix + "crashed"
	// Not ours: a directory that does not look like a runtime ID must survive,
	// so that a mistaken base directory cannot cause collateral deletion.
	foreign := "important-user-data"

	for _, name := range []string{keep, old, tmpLeftover, foreign} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o755); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
	}

	pruneRuntimes(base, keep)

	for _, name := range []string{keep, foreign} {
		if _, err := os.Stat(filepath.Join(base, name)); err != nil {
			t.Fatalf("%s should have been kept: %v", name, err)
		}
	}
	for _, name := range []string{old, tmpLeftover} {
		if _, err := os.Stat(filepath.Join(base, name)); err == nil {
			t.Fatalf("%s should have been removed", name)
		}
	}
}

func TestIsRuntimeID(t *testing.T) {
	if !isRuntimeID("0123456789abcdef") {
		t.Fatal("a 16-character lower-case hex name should be recognised")
	}
	for _, bad := range []string{
		"0123456789ABCDEF", // upper case is not what runtimeID produces
		"0123456789abcde",  // too short
		"0123456789abcdef0",
		"zzzzzzzzzzzzzzzz",
		"",
	} {
		if isRuntimeID(bad) {
			t.Fatalf("%q should not be treated as a runtime directory", bad)
		}
	}
}
