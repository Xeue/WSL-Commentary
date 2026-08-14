// Package applog gives the application's log a file to land in.
//
// Owner: WP-8 (it is wired in main.go and serves the whole process).
//
// # Why this exists
//
// Everything in this application reports through log.Printf: the sender's
// reconnect ladder, gst.Init's plugin complaints, the SRT return's connect
// diagnoses — internal/gst alone has over forty call sites. But log's default
// destination is stderr, and a `wails build` executable is linked with
// -H windowsgui: it has no console, so stderr is a handle to nowhere. Every one
// of those messages was being composed, redacted, formatted — and discarded.
//
// That was discovered the expensive way: an SRT return failing on a machine
// elsewhere, a config that looked right, and no way to see the diagnosis the
// app had already written. This package is the fix. main() calls Open before
// anything logs, and from then on the log goes to a dated file under
// DefaultDir as well as stderr — so a console run (or a 2> redirect) behaves
// exactly as before, and a double-clicked one finally leaves evidence.
//
// The same argument applies unchanged on macOS, which needs it just as much:
// a .app launched from the Finder inherits no terminal either, so its stderr
// goes to the unified log at best and nowhere at all in practice.
//
// # Why %LOCALAPPDATA% and not %APPDATA% (Windows)
//
// Logs are machine-local diagnostic state, like the GStreamer registry already
// kept at %LOCALAPPDATA%\WSLComms\registry.bin. On a roaming profile, %APPDATA%
// follows the user between machines — and a log from another machine mixed into
// this one's is precisely the confusion a support bundle does not need.
//
// # Why ~/Library/Logs and not ~/Library/Caches (macOS)
//
// Because the whole point of this package is that the file is STILL THERE
// tomorrow. os.UserCacheDir on macOS is ~/Library/Caches, which Apple documents
// as purgeable and which the system may delete under disk pressure — and the
// machine most likely to be short of disk is the one that has been running all
// day at an outside broadcast. Storing the only diagnosis a headless commentary
// position produces somewhere the OS is entitled to delete would reintroduce
// exactly the failure this package was written to fix, and it would do it
// intermittently, which is worse.
//
// ~/Library/Logs is the macOS convention for precisely this file, it is what
// Console.app shows under "Log Reports", and it is where anybody asked to send
// their logs will already be looking.
//
// Note that internal/gst's registryFile makes the OPPOSITE choice, on the same
// facts: the GStreamer registry is a genuine cache, and a purged one costs one
// plugin rescan. Same fallback, two different right answers, decided per caller
// rather than by a rule about os.UserCacheDir.
//
// DefaultDir is declared per platform, in applog_windows.go and
// applog_darwin.go, because the two answers differ in SHAPE and not only in
// their base directory: Windows appends a "logs" subdirectory to a general-
// purpose data root, and macOS must not, because ~/Library/Logs already is one.
// There is deliberately no third file — internal/secrets has no third
// implementation either, so this module does not build on any other GOOS
// regardless.
package applog

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// filePrefix begins every file this package creates, and is what Prune matches.
// Nothing else in DefaultDir is ever touched.
//
// DefaultDir is a WSLComms directory of our own on both platforms, so the prefix
// is belt and braces there — but it is what makes Prune safe if this package is
// ever pointed at a shared directory. On macOS that is one step away:
// ~/Library/Logs itself is shared with every other application on the machine,
// and only the WSLComms component of the path keeps us out of their files.
const filePrefix = "wslcomms-"

// keepFiles is how many log files Prune leaves behind, newest first. Each run
// writes two (the app log and the GStreamer log), so this keeps roughly the
// last six runs — enough to cover "it failed yesterday too", small enough that
// the folder never becomes a surprise on disk.
const keepFiles = 12

// DefaultDir returns the directory this run's log files belong in:
// %LOCALAPPDATA%\WSLComms\logs on Windows and ~/Library/Logs/WSLComms on macOS.
// It is declared per platform, in applog_windows.go and applog_darwin.go. The
// package doc carries the argument for each.
//
// It does not create the directory. Open does.

// A Sink is one run's open log file.
type Sink struct {
	// Path is where the log is being written, for surfacing to the operator.
	Path string

	// Dir is the directory the file lives in — the place a support bundle
	// should collect wholesale.
	Dir string

	file *os.File
}

// Open creates <baseDir>\wslcomms-<stamp>-<pid>.log, points the standard
// logger at it, and prunes old files.
//
// The stamp is passed in rather than read here so the caller can use one
// moment for everything it names this run — see GStreamerLogPath, which must
// sort beside its app log, not one second after it.
//
// The pid is in the name for the one race that can actually happen: a second
// launch. SingleInstanceLock makes it hand over and exit, but it does that
// inside wails.Run — after main() has started logging — so for a moment two
// processes are writing. With the pid in the name they write two files instead
// of interleaving one.
//
// On any failure Open returns an error and changes nothing: the logger still
// writes to stderr exactly as it did before this package existed. A machine
// that cannot create the directory loses the file, not the application.
func Open(baseDir, stamp string) (*Sink, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("applog: creating %q: %w", baseDir, err)
	}

	path := filepath.Join(baseDir, fmt.Sprintf("%s%s-%d.log", filePrefix, stamp, os.Getpid()))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("applog: creating %q: %w", path, err)
	}

	// Both destinations, always. stderr costs nothing when it is a handle to
	// nowhere, and keeps console runs, tests and 2> redirects behaving exactly
	// as they always have. The file is the addition, not a replacement.
	log.SetOutput(io.MultiWriter(os.Stderr, f))

	// Timestamps matter now that the output outlives the moment. LUTC is
	// deliberate: these files travel — the whole reason they exist is machines
	// in other places — and "14:03 whose time?" is not a question a support
	// thread should open with.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.LUTC)

	Prune(baseDir)

	return &Sink{Path: path, Dir: baseDir, file: f}, nil
}

// Close detaches the standard logger from the file and closes it. The logger
// goes back to stderr alone, so anything that logs after Close — teardown
// stragglers, the race-detector's last words — still goes somewhere rather
// than to a closed handle.
func (s *Sink) Close() {
	log.SetOutput(os.Stderr)
	_ = s.file.Close()
}

// Prune deletes the oldest wslcomms-*.log files in dir beyond keepFiles,
// newest-by-name first.
//
// By name, not by modification time: the names embed the stamp, so
// lexicographic order is chronological order, and it stays true for files
// restored from a backup with fresh mtimes. Files not matching the prefix are
// never touched — the directory belongs to this package's files only in the
// sense that it only ever manages its own.
//
// Failures are ignored: a log file held open by another process (that second
// launch again, or an operator's editor) just survives until next time. Prune
// is housekeeping, and housekeeping must never take the application down.
func Prune(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, filePrefix) && strings.HasSuffix(n, ".log") {
			names = append(names, n)
		}
	}
	if len(names) <= keepFiles {
		return
	}

	sort.Sort(sort.Reverse(sort.StringSlice(names))) // newest first
	for _, n := range names[keepFiles:] {
		_ = os.Remove(filepath.Join(dir, n))
	}
}

// GStreamerLogPath is where this run's GStreamer debug log should go, sharing
// the app log's stamp so the pair sort together in the directory listing.
func GStreamerLogPath(baseDir, stamp string) string {
	return filepath.Join(baseDir, fmt.Sprintf("%s%s-%d-gst.log", filePrefix, stamp, os.Getpid()))
}

// SetGStreamerDebugDefaults routes GStreamer's own debug log into logPath —
// unless the operator has already spoken.
//
// GStreamer's logging is controlled entirely by environment variables read at
// gst_init, and its default is ERROR-level to stderr: the same handle to
// nowhere. So without this, the layer most likely to know why an SRT
// connection failed — libsrt speaks through the srt* debug categories — was
// the layer least able to say so.
//
// The level chosen is "2,srt*:4": WARNING everywhere, INFO for the SRT
// elements. WARNING is quiet in steady state; srt* at INFO records each
// connect attempt and its outcome, which is exactly the trail a "the return
// isn't working and the config looks right" report needs, at a volume that is
// perfectly reasonable to leave on for every run.
//
// Both variables defer to the environment: an operator who sets GST_DEBUG or
// GST_DEBUG_FILE for a deep dive gets precisely what they asked for, this
// function touches neither, and their choice of file wins. Set-if-unset, never
// overwrite.
//
// It must run before gst.Init — the variables are read once, at gst_init — and
// therefore before anything can have logged through GStreamer at all.
func SetGStreamerDebugDefaults(logPath string) {
	if os.Getenv("GST_DEBUG") == "" {
		os.Setenv("GST_DEBUG", "2,srt*:4")
	}
	if os.Getenv("GST_DEBUG_FILE") == "" {
		os.Setenv("GST_DEBUG_FILE", logPath)
	}
}
