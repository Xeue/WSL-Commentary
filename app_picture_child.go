//go:build dev || production || bindings

// app_picture_child.go runs the SRT programme picture IN ITS OWN PROCESS, and is
// both halves of that: the parent's gst.PictureMonitor that launches and talks
// to the child, and the child's main.
//
// Owner: WP-P, with app_picture.go.
//
// # Why a process and not a goroutine
//
// The picture is a GStreamer pipeline into a GPU decoder into a native window.
// Every one of those can wedge — a driver that stops answering, a decoder that
// hangs inside a state change, a libsrt socket that will not close — and when
// one does, Stop blocks inside the wedge and nothing in this process can unblock
// it. The zombie-exit deadlock (exit_windows.go) was that shape. A wedge in a
// GOROUTINE is a wedge in the application; a wedge in a CHILD PROCESS is one
// TerminateProcess away from gone, with the contribution feed, the audio and the
// window untouched. That is the whole argument, and it is also why the operator
// asked for it in these words: "so we can restart the PGM monitoring separately".
// Refresh, on the picture, is now literally that — kill the process and start a
// fresh one, with a fresh GStreamer, a fresh SRT socket and a fresh decoder.
//
// # The shape
//
// The child is THIS SAME EXECUTABLE, launched with pictureChildEnv set. main.go
// notices that before it does anything an application would do — no Wails, no
// window of the application's, no capture, no single-instance lock — and calls
// runPictureChildAndExit instead. The child gets its options as ONE JSON line on
// stdin and reports on stdout, one line at a time:
//
//	ready                 the window exists and the reconnect loop is running
//	fatal <text>          Start refused (a configuration that cannot work); exiting
//	state <s>             a gst.PictureState transition: stopped|connecting|showing|backoff
//
// Nothing else is ever written to stdout. The software-decode note is NOT the
// child's to raise: the answer comes from the GStreamer registry, which this
// process shares, and the once-per-process guard has to live in the process
// that outlives every Refresh. See App.maybeNotePictureSoftwareDecode. The child's log goes to its own file
// (main.go opens one for every process) and its stderr is relayed into this
// process's log line by line, prefixed, so a crash is diagnosable from here.
//
// # The lifeline
//
// The parent keeps the child's stdin OPEN and never writes to it again. When the
// parent closes it — Stop — the child sees EOF, stops its pipeline, and exits.
// When the parent DIES, the operating system closes it, and the child sees the
// same EOF: there is no way to orphan a picture process rendering into a window
// nobody owns. The child also exits when the operator closes its window, and
// the parent learns of either exit the same way — its stdout reader hits EOF —
// and reports the picture STOPPED.
//
// # The passphrase
//
// It travels on stdin, inside the JSON line, and nowhere else: not on the
// command line, where every process on the machine can read it, and not in the
// environment block, which is inherited by anything the child might spawn. The
// child sets it with g_object_set exactly as the in-process path did. It is not
// logged by either side.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
)

// pictureChildEnv is the environment variable that turns this executable into
// the picture process. Its value is irrelevant; its presence is the signal.
const pictureChildEnv = "WSLCOMMS_PICTURE_CHILD"

// The two budgets are variables rather than constants so the tests can shorten
// them; nothing else assigns them.
var (
	// pictureChildStartTimeout bounds Start's wait for the child's first line.
	// The child has to open its log, initialise the bundled GStreamer (a registry
	// scan, which on a cold registry can take a few seconds), create a window
	// and validate its options before it says anything. A child that has said
	// nothing after this long is not going to.
	pictureChildStartTimeout = 20 * time.Second

	// pictureChildStopBudget bounds Stop's wait for the child to exit after its
	// stdin is closed. A child whose pipeline is wedged inside a state change
	// cannot honour the EOF, and that is the case this whole file exists for:
	// after the budget it is killed.
	pictureChildStopBudget = 5 * time.Second
)

// pictureChildStatesBuffer matches gst's own picture state buffer: a slow
// consumer loses intermediate states, never the current one, and never stalls
// the reader.
const pictureChildStatesBuffer = 8

// pictureChildOpts is the JSON line the parent writes to the child's stdin. It
// is gst.PictureOpts minus the window handle, which the child supplies from a
// window of its own.
type pictureChildOpts struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	LatencyMs  int    `json:"latencyMs"`
	Passphrase string `json:"passphrase"`
	PBKeyLen   int    `json:"pbKeyLen"`
}

// ---------------------------------------------------------------------------
// The parent's half: a gst.PictureMonitor that is a process
// ---------------------------------------------------------------------------

// childPictureMonitor implements gst.PictureMonitor by running the picture in a
// child process. App.newPictureMonitor builds one; app_picture.go drives it
// exactly as it drove the in-process monitor, which is the point of implementing
// the interface rather than inventing another.
type childPictureMonitor struct {
	// spawn builds the child's command. The default runs this executable with
	// pictureChildEnv set; tests substitute a helper process.
	spawn func() (*exec.Cmd, error)

	mu      sync.Mutex
	started bool
	stopped bool
	cmd     *exec.Cmd
	stdin   io.WriteCloser

	states     chan gst.PictureState
	done       chan struct{} // closed when the stdout reader has finished
	stderrDone chan struct{} // closed when the stderr relay has finished
	stopOnce   sync.Once
}

// newChildPictureMonitor builds the parent's half.
func newChildPictureMonitor() *childPictureMonitor {
	return &childPictureMonitor{
		spawn:      defaultPictureChildCommand,
		states:     make(chan gst.PictureState, pictureChildStatesBuffer),
		done:       make(chan struct{}),
		stderrDone: make(chan struct{}),
	}
}

var _ gst.PictureMonitor = (*childPictureMonitor)(nil)

// defaultPictureChildCommand is this executable, marked as the picture process.
//
// It REFUSES when this executable is a Go test binary. A test binary has no
// main and runs its whole suite when launched, so a picture process made from
// one would run every test again, including whichever test launched it — a
// fork bomb, and one that was found the hard way on a Gate B run. The tests
// install fakes and never come here on purpose; this is the belt for the one
// that reaches StartPicture by accident.
func defaultPictureChildCommand() (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("wslcomms: cannot find this executable to launch the picture process: %w", err)
	}
	if isGoTestBinary(exe) {
		return nil, fmt.Errorf("wslcomms: refusing to launch %q as the picture process: it is a test binary, "+
			"and a test binary launched runs its whole suite; install a pictureDial fake", filepath.Base(exe))
	}
	return pictureChildCommand(exe), nil
}

// pictureChildCommand builds the picture process's command for a given
// executable: no arguments — the options go on stdin — and the environment
// with pictureChildEnv set, which is what turns the executable into the child.
func pictureChildCommand(exe string) *exec.Cmd {
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), pictureChildEnv+"=1")
	return cmd
}

// isGoTestBinary reports whether exe is named the way `go test` names the
// binaries it builds: <package>.test or <package>.test.exe.
func isGoTestBinary(exe string) bool {
	base := strings.ToLower(filepath.Base(exe))
	return strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe")
}

// ---------------------------------------------------------------------------
// Where the picture window was: remembered across processes
// ---------------------------------------------------------------------------

// picturePlacementFile sits beside config.json in %APPDATA%\WSLComms. It is
// its own file rather than a config field because it is written by the picture
// PROCESS, on the operator's every drag, and config.json is the application's
// to write; two processes writing one file is a corrupted file eventually.
const picturePlacementFile = "picture-window.json"

// picturePlacementPath is %APPDATA%\WSLComms\picture-window.json, resolved the
// way config.Path resolves config.json so the two always sit together — and so
// the tests' APPDATA redirection covers this file too.
func picturePlacementPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("wslcomms: resolving the user config directory: %w", err)
	}
	return filepath.Join(dir, config.AppDataDirName, picturePlacementFile), nil
}

// loadPicturePlacement reads the remembered placement, or returns nil when
// there is none, the path is empty, or the file is not readable: every one of
// those means "let the shell place the window", which is not an error.
func loadPicturePlacement(path string) *gst.PictureWindowPlacement {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var p gst.PictureWindowPlacement
	if err := json.Unmarshal(data, &p); err != nil {
		log.Printf("wslcomms: picture process: ignoring an unreadable %s: %v", picturePlacementFile, err)
		return nil
	}
	return &p
}

// savePicturePlacement writes the placement atomically: to a sibling temp file,
// then renamed over the real one, so a kill mid-write leaves the previous
// placement rather than half a file.
func savePicturePlacement(path string, p gst.PictureWindowPlacement) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (m *childPictureMonitor) States() <-chan gst.PictureState { return m.states }

// Start launches the child, hands it its options, and waits for it to say
// "ready" or "fatal". It returns as soon as the child's reconnect loop is
// running — the same promise the in-process monitor made — and a configuration
// the child refuses comes back as an error from here, synchronously, with the
// child already gone.
func (m *childPictureMonitor) Start(opts gst.PictureOpts) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return gst.ErrPictureAlreadyStarted
	}
	m.started = true
	m.mu.Unlock()

	cmd, err := m.spawn()
	if err != nil {
		m.finishWithout(err)
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		m.finishWithout(err)
		return fmt.Errorf("wslcomms: picture process: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.finishWithout(err)
		return fmt.Errorf("wslcomms: picture process: stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		m.finishWithout(err)
		return fmt.Errorf("wslcomms: picture process: stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		m.finishWithout(err)
		return fmt.Errorf("wslcomms: could not launch the picture process: %w", err)
	}
	log.Printf("wslcomms: picture process launched, pid %d", cmd.Process.Pid)

	m.mu.Lock()
	m.cmd = cmd
	m.stdin = stdin
	m.mu.Unlock()

	// The child's stderr, into this log, line by line. It is its own goroutine
	// because a child that writes a lot to stderr and is not read would block
	// on a full pipe — inside GStreamer, mid-decode.
	go func() {
		defer close(m.stderrDone)
		relayPictureChildStderr(stderr)
	}()

	// The options, as one line, then nothing more. The passphrase goes here and
	// only here; see the file header.
	line, err := json.Marshal(pictureChildOpts{
		Host:       opts.Host,
		Port:       opts.Port,
		LatencyMs:  opts.LatencyMs,
		Passphrase: opts.Passphrase,
		PBKeyLen:   opts.PBKeyLen,
	})
	if err != nil {
		m.kill()
		return fmt.Errorf("wslcomms: picture process: encoding options: %w", err)
	}
	if _, err := stdin.Write(append(line, '\n')); err != nil {
		m.kill()
		return fmt.Errorf("wslcomms: picture process: writing options: %w", err)
	}

	// The reader owns stdout for the life of the child. Its first line decides
	// whether Start succeeds; every later line is a state, a note or an error.
	first := make(chan string, 1)
	go m.read(stdout, first)

	select {
	case l, ok := <-first:
		if !ok {
			m.kill()
			return errors.New("wslcomms: the picture process exited before it was ready; see its log")
		}
		if strings.HasPrefix(l, "fatal ") {
			m.kill()
			return errors.New("wslcomms: the picture process refused to start: " + strings.TrimPrefix(l, "fatal "))
		}
		if l != "ready" {
			m.kill()
			return fmt.Errorf("wslcomms: the picture process said %q before it said ready", l)
		}
		return nil
	case <-time.After(pictureChildStartTimeout):
		m.kill()
		return fmt.Errorf("wslcomms: the picture process did not become ready within %s", pictureChildStartTimeout)
	}
}

// finishWithout marks a Start that never launched a child, so States closes and
// a consumer ranging over it exits.
func (m *childPictureMonitor) finishWithout(err error) {
	log.Printf("wslcomms: picture process not started: %v", err)
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
	close(m.states)
	close(m.done)
}

// read consumes the child's stdout until EOF. The first line goes to first;
// thereafter states go to the channel. On EOF — the child exited, for any
// reason — it reports STOPPED, closes the states channel and reaps the process.
func (m *childPictureMonitor) read(stdout io.Reader, first chan<- string) {
	defer close(m.done)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)

	gotFirst := false
	for sc.Scan() {
		l := strings.TrimRight(sc.Text(), "\r")
		if !gotFirst {
			// The first line decides Start. A fatal one means Start kills the
			// child and nothing else is expected, but the loop drains to EOF
			// regardless so the pipe closes cleanly.
			gotFirst = true
			first <- l
			continue
		}
		m.dispatch(l)
	}
	if !gotFirst {
		close(first)
	}

	// EOF: the child is gone, or going. It is reaped FIRST, so that by the time
	// STOPPED goes out the process is gone rather than going; then STOPPED —
	// what the in-process monitor emitted last — and the closed channel, which
	// is what lets the forwarder and Stop's join return.
	m.mu.Lock()
	m.stopped = true
	cmd := m.cmd
	m.mu.Unlock()
	if cmd != nil {
		// Wait closes the pipes it created, so the stderr relay is joined
		// first: os/exec says a Wait before the reads have finished is wrong,
		// and the relay's read finishes when the process does.
		<-m.stderrDone
		if err := cmd.Wait(); err != nil {
			log.Printf("wslcomms: picture process exited: %v", err)
		} else {
			log.Print("wslcomms: picture process exited")
		}
	}
	m.emit(gst.PictureStateStopped)
	close(m.states)
}

// dispatch routes one protocol line.
func (m *childPictureMonitor) dispatch(l string) {
	switch {
	case strings.HasPrefix(l, "state "):
		s := gst.PictureState(strings.TrimPrefix(l, "state "))
		switch s {
		case gst.PictureStateStopped, gst.PictureStateConnecting,
			gst.PictureStateShowing, gst.PictureStateBackoff:
			m.emit(s)
		default:
			log.Printf("wslcomms: picture process reported an unknown state %q", s)
		}
	default:
		log.Printf("wslcomms: picture process wrote an unexpected line: %q", l)
	}
}

// emit delivers a state without ever blocking: drop the oldest if full.
func (m *childPictureMonitor) emit(s gst.PictureState) {
	for {
		select {
		case m.states <- s:
			return
		default:
			select {
			case <-m.states:
			default:
			}
		}
	}
}

// kill ends the child unconditionally and waits for the reader to finish. It is
// what a wedged picture gets, and what a failed Start gets.
func (m *childPictureMonitor) kill() {
	m.mu.Lock()
	cmd := m.cmd
	stdin := m.stdin
	m.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	<-m.done
}

// Stop closes the child's stdin — its signal to stop its pipeline and exit —
// waits up to pictureChildStopBudget for it to go, and kills it if it has not.
// By the time it returns the states channel is closed and STOPPED has been sent.
func (m *childPictureMonitor) Stop() error {
	m.mu.Lock()
	started := m.started
	m.mu.Unlock()
	if !started {
		return gst.ErrPictureNotStarted
	}

	m.stopOnce.Do(func() {
		m.mu.Lock()
		stdin := m.stdin
		cmd := m.cmd
		m.mu.Unlock()
		if stdin != nil {
			_ = stdin.Close()
		}
		select {
		case <-m.done:
			return
		case <-time.After(pictureChildStopBudget):
		}
		if cmd != nil && cmd.Process != nil {
			log.Printf("wslcomms: the picture process did not exit within %s of being told to stop; killing it",
				pictureChildStopBudget)
			_ = cmd.Process.Kill()
		}
		<-m.done
	})
	return nil
}

// exited reports whether the child has gone, so a caller holding a session can
// tell "running" from "died on its own" without blocking.
func (m *childPictureMonitor) exited() bool {
	select {
	case <-m.done:
		return true
	default:
		return false
	}
}

// relayPictureChildStderr copies the child's stderr into this process's log.
func relayPictureChildStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		log.Printf("wslcomms: picture process: %s", sc.Text())
	}
}

// ---------------------------------------------------------------------------
// The child's half: main
// ---------------------------------------------------------------------------

// runPictureChildAndExit is the picture process. It never returns: it leaves
// through forceExit on every path, for the reason the application does — the
// GStreamer and GPU-driver DLLs this process loads can deadlock in
// DLL_PROCESS_DETACH, and TerminateProcess is the exit that skips it.
func runPictureChildAndExit(gstInitErr error) {
	out := bufio.NewWriter(os.Stdout)
	say := func(format string, args ...any) {
		fmt.Fprintf(out, format+"\n", args...)
		_ = out.Flush()
	}
	fatal := func(err error) {
		log.Printf("wslcomms: picture process: fatal: %v", err)
		say("fatal %s", strings.ReplaceAll(err.Error(), "\n", " "))
		forceExit()
	}

	if gstInitErr != nil {
		fatal(gstInitErr)
	}

	// The options: one line, and the passphrase never leaves this variable.
	var opts pictureChildOpts
	in := bufio.NewReader(os.Stdin)
	line, err := in.ReadString('\n')
	if err != nil && len(strings.TrimSpace(line)) == 0 {
		fatal(fmt.Errorf("no options arrived on stdin: %w", err))
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &opts); err != nil {
		fatal(fmt.Errorf("the options on stdin were not readable: %w", err))
	}

	// A window of this process's own. Titled so it can be found on a taskbar
	// full of things during a match, and put back WHERE THE OPERATOR LEFT IT:
	// a Refresh is a new process, and a picture that came back at a default
	// position on every press would make the button cost a drag each time.
	// The placement is remembered by this process on every move, resize,
	// maximise and restore, so even a kill loses at most the last gesture.
	placementPath, err := picturePlacementPath()
	if err != nil {
		log.Printf("wslcomms: picture process: the window placement will not be remembered: %v", err)
	}
	win, err := gst.NewPictureWindow(windowTitle+" — Programme", loadPicturePlacement(placementPath),
		func(p gst.PictureWindowPlacement) {
			if placementPath == "" {
				return
			}
			if err := savePicturePlacement(placementPath, p); err != nil {
				log.Printf("wslcomms: picture process: remembering the window placement: %v", err)
			}
		})
	if err != nil {
		fatal(err)
	}

	mon := gst.NewPictureMonitor()
	if err := mon.Start(gst.PictureOpts{
		Host:         opts.Host,
		Port:         opts.Port,
		LatencyMs:    opts.LatencyMs,
		Passphrase:   opts.Passphrase,
		PBKeyLen:     opts.PBKeyLen,
		WindowHandle: win.Handle(),
	}); err != nil {
		_ = win.Close()
		fatal(err)
	}
	say("ready")

	// States, as they happen, until the monitor's channel closes.
	statesDone := make(chan struct{})
	go func() {
		defer close(statesDone)
		for s := range mon.States() {
			say("state %s", s)
		}
	}()

	// The lifeline: stdin EOF means the parent has stopped us or died.
	stdinGone := make(chan struct{})
	go func() {
		defer close(stdinGone)
		_, _ = io.Copy(io.Discard, in)
	}()

	select {
	case <-stdinGone:
		log.Print("wslcomms: picture process: stdin closed; stopping")
	case <-win.Closed():
		log.Print("wslcomms: picture process: the window was closed; stopping")
	case <-statesDone:
		log.Print("wslcomms: picture process: the monitor ended; stopping")
	}

	// Stop emits STOPPED and closes the channel, so the state goroutine writes
	// the last line before we leave.
	if err := mon.Stop(); err != nil && !errors.Is(err, gst.ErrPictureNotStarted) {
		log.Printf("wslcomms: picture process: stopping the monitor: %v", err)
	}
	select {
	case <-statesDone:
	case <-time.After(2 * time.Second):
	}
	_ = win.Close()
	_ = out.Flush()
	forceExit()
}
