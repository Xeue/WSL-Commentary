package applog

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetLogger restores the standard logger after a test that redirects it, so
// a failing assertion in one test cannot silence every test after it.
func resetLogger(t *testing.T) {
	t.Helper()
	out := log.Writer()
	flags := log.Flags()
	t.Cleanup(func() {
		log.SetOutput(out)
		log.SetFlags(flags)
	})
}

// TestDefaultDirIsUsableAsALogDirectory holds the half of DefaultDir's contract
// that is the same sentence on both platforms: an absolute path, with a
// WSLComms component, that Prune can be pointed at.
//
// The half that differs is asserted in the tagged twins beside this file,
// because the two platforms disagree about which directory is the wrong answer
// and a single test could only have stated one of them. applog_darwin_test.go
// carries "not the user cache directory"; applog_windows_test.go carries
// "under %LOCALAPPDATA%, ending in WSLComms\logs" — which on Windows IS the
// user cache directory, os.UserCacheDir returning %LocalAppData% there. Asking
// for "not a cache" on Windows fails by construction against a DefaultDir that
// has never been anything but correct, so it is asked only where it is true.
func TestDefaultDirIsUsableAsALogDirectory(t *testing.T) {
	dir, err := DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir() error = %v", err)
	}
	if dir == "" {
		t.Fatal("DefaultDir() is empty")
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("DefaultDir() = %q is relative; the log must not follow the working directory", dir)
	}
	if !strings.Contains(dir, "WSLComms") {
		t.Errorf("DefaultDir() = %q has no WSLComms component. On macOS the parent is "+
			"~/Library/Logs, which is shared with every other application on the machine, and "+
			"Prune walks whatever it is given", dir)
	}
}

func TestOpen_WritesLogLinesToTheFile(t *testing.T) {
	resetLogger(t)
	dir := t.TempDir()

	s, err := Open(dir, "20260812-120000")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	log.Printf("the return isn't working and this line must survive")

	got, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatalf("reading the log back: %v", err)
	}
	if !strings.Contains(string(got), "this line must survive") {
		t.Fatalf("the logged line is not in the file; file contains: %q", got)
	}
	// The whole point is that the file outlives the moment, so lines must be
	// timestamped — and unambiguously, which means UTC.
	if log.Flags()&log.LUTC == 0 {
		t.Fatal("Open did not set LUTC; timestamps in files that travel between machines must be UTC")
	}
}

func TestOpen_NamesTheFileWithStampAndPid(t *testing.T) {
	resetLogger(t)
	dir := t.TempDir()

	s, err := Open(dir, "20260812-120000")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	want := fmt.Sprintf("wslcomms-20260812-120000-%d.log", os.Getpid())
	if filepath.Base(s.Path) != want {
		t.Fatalf("log file is %q, want %q", filepath.Base(s.Path), want)
	}
	if s.Dir != dir {
		t.Fatalf("Sink.Dir = %q, want %q", s.Dir, dir)
	}
}

func TestOpen_FailureLeavesTheLoggerOnStderr(t *testing.T) {
	resetLogger(t)
	// A file where the directory should be: MkdirAll must fail, and the
	// logger must be left exactly as it was.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	before := log.Writer()
	if _, err := Open(blocker, "20260812-120000"); err == nil {
		t.Fatal("Open succeeded inside a path blocked by a file")
	}
	if log.Writer() != before {
		t.Fatal("a failed Open changed the logger's destination; it must change nothing")
	}
}

func TestClose_ReturnsTheLoggerToStderrAlone(t *testing.T) {
	resetLogger(t)
	dir := t.TempDir()

	s, err := Open(dir, "20260812-120000")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.Close()

	if w := log.Writer(); w != io.Writer(os.Stderr) {
		t.Fatalf("after Close the logger writes to %T, want os.Stderr", w)
	}
	// Logging after Close must not blow up on the closed file.
	log.Printf("teardown straggler")
}

func TestPrune_KeepsTheNewestAndIgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()

	// 15 of ours, in a scrambled creation order so mtime cannot be what sorts
	// them, plus two foreign files that must survive whatever happens.
	var ours []string
	for _, h := range []int{7, 1, 14, 3, 11, 0, 9, 5, 13, 2, 8, 4, 12, 6, 10} {
		n := fmt.Sprintf("wslcomms-20260812-%02d0000-100.log", h)
		ours = append(ours, n)
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"keep-me.txt", "wslcomms-but-not-a-log.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	Prune(dir)

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var logs, foreign []string
	for _, e := range left {
		if strings.HasPrefix(e.Name(), "wslcomms-") && strings.HasSuffix(e.Name(), ".log") {
			logs = append(logs, e.Name())
		} else {
			foreign = append(foreign, e.Name())
		}
	}

	if len(logs) != keepFiles {
		t.Fatalf("prune left %d log files, want %d", len(logs), keepFiles)
	}
	if len(foreign) != 2 {
		t.Fatalf("prune touched foreign files; %d remain, want 2", len(foreign))
	}
	// The survivors must be the lexicographically newest — hours 03..14 — so
	// the oldest three (00, 01, 02) are the ones gone.
	for _, n := range logs {
		for _, gone := range []string{"-000000-", "-010000-", "-020000-"} {
			if strings.Contains(n, gone) {
				t.Fatalf("prune kept %q, which should have been among the oldest deleted", n)
			}
		}
	}
}

func TestPrune_UnderTheLimitDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < keepFiles; i++ {
		n := fmt.Sprintf("wslcomms-20260812-%02d0000-100.log", i)
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	Prune(dir)
	left, _ := os.ReadDir(dir)
	if len(left) != keepFiles {
		t.Fatalf("prune deleted from a directory at exactly the limit: %d left, want %d", len(left), keepFiles)
	}
}

func TestGStreamerLogPath_SortsBesideTheAppLog(t *testing.T) {
	p := GStreamerLogPath(`C:\logs`, "20260812-120000")
	want := fmt.Sprintf("wslcomms-20260812-120000-%d-gst.log", os.Getpid())
	if filepath.Base(p) != want {
		t.Fatalf("GStreamerLogPath = %q, want %q", filepath.Base(p), want)
	}
}

func TestSetGStreamerDebugDefaults_SetsOnlyWhatIsUnset(t *testing.T) {
	// Sets both when neither is present.
	t.Setenv("GST_DEBUG", "")
	t.Setenv("GST_DEBUG_FILE", "")
	os.Unsetenv("GST_DEBUG")
	os.Unsetenv("GST_DEBUG_FILE")

	SetGStreamerDebugDefaults(`C:\logs\wslcomms-x-gst.log`)

	if got := os.Getenv("GST_DEBUG"); got != "2,srt*:4" {
		t.Fatalf("GST_DEBUG = %q, want the 2,srt*:4 default", got)
	}
	if got := os.Getenv("GST_DEBUG_FILE"); got != `C:\logs\wslcomms-x-gst.log` {
		t.Fatalf("GST_DEBUG_FILE = %q, want the log path", got)
	}
}

func TestSetGStreamerDebugDefaults_NeverOverridesTheOperator(t *testing.T) {
	// An operator mid-deep-dive has both set; this must be a no-op on each.
	t.Setenv("GST_DEBUG", "srtobject:9")
	t.Setenv("GST_DEBUG_FILE", `D:\investigations\deep.log`)

	SetGStreamerDebugDefaults(`C:\logs\wslcomms-x-gst.log`)

	if got := os.Getenv("GST_DEBUG"); got != "srtobject:9" {
		t.Fatalf("GST_DEBUG was overridden to %q; the operator's value must win", got)
	}
	if got := os.Getenv("GST_DEBUG_FILE"); got != `D:\investigations\deep.log` {
		t.Fatalf("GST_DEBUG_FILE was overridden to %q; the operator's value must win", got)
	}
}
