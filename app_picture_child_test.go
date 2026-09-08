//go:build dev || production || bindings

// Tests for the parent's half of the picture process — childPictureMonitor —
// against a HELPER PROCESS: this same test binary, re-run with an environment
// variable that turns TestPictureChildHelperProcess into the child. It speaks
// the real protocol over real pipes, exits when its stdin closes, and can be
// told to refuse, to say nothing, to ignore the stop, or to die on its own.
//
// WHAT THIS DOES NOT REACH: the child's half, runPictureChildAndExit, with a
// real window and a real pipeline. Only its two refusals — no GStreamer, no
// options — are exercised here, with the process exit intercepted.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"wslcomms/internal/gst"
)

const (
	helperModeEnv       = "WSLCOMMS_TEST_PICTURE_HELPER"
	helperPassphraseEnv = "WSLCOMMS_TEST_EXPECT_PASSPHRASE"
	helperPassphrase    = "hunter2-on-stdin-only"
	helperHost          = "m2lx.test"
)

// TestPictureChildHelperProcess is not a test. It is the picture process, when
// this binary is launched by helperCommand; otherwise it returns at once.
func TestPictureChildHelperProcess(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		return
	}
	// Leave through os.Exit so the test framework does not print PASS onto the
	// protocol stream.
	defer os.Exit(0)

	in := bufio.NewReader(os.Stdin)
	line, _ := in.ReadString('\n')
	var opts pictureChildOpts
	_ = json.Unmarshal([]byte(strings.TrimSpace(line)), &opts)
	say := func(s string) { fmt.Fprintln(os.Stdout, s) }

	switch mode {
	case "fatal":
		say("fatal the helper was told to refuse")
	case "silent":
		// Never says ready. Start must give up on it.
		time.Sleep(10 * time.Second)
	case "stubborn":
		// Says ready, then ignores the EOF on stdin: the wedged pipeline.
		say("ready")
		say("state connecting")
		_, _ = io.Copy(io.Discard, in)
		time.Sleep(30 * time.Second)
	case "die":
		// Says ready, then exits on its own with nobody having asked.
		say("ready")
		say("state connecting")
	default: // "well"
		say("ready")
		say("state connecting")
		// Whether the options arrived intact is reported as a state, which is
		// the only thing the parent listens to.
		if opts.Passphrase == os.Getenv(helperPassphraseEnv) && opts.Host == helperHost && opts.Port == 40501 {
			say("state showing")
		} else {
			say("state backoff")
		}
		say("this line is not part of the protocol and must be ignored")
		fmt.Fprintln(os.Stderr, "helper: something on stderr, for the parent's log")
		_, _ = io.Copy(io.Discard, in) // the lifeline: wait for EOF
		say("state stopped")
	}
}

// helperCommand builds a spawn function that runs this binary as the helper in
// the given mode. The commands it built are recorded so a test can inspect
// what crossed to the child and how.
func helperCommand(mode string) (func() (*exec.Cmd, error), func() *exec.Cmd) {
	var last *exec.Cmd
	spawn := func() (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPictureChildHelperProcess$")
		cmd.Env = append(os.Environ(),
			helperModeEnv+"="+mode,
			helperPassphraseEnv+"="+helperPassphrase,
		)
		last = cmd
		return cmd, nil
	}
	return spawn, func() *exec.Cmd { return last }
}

func newHelperMonitor(mode string) (*childPictureMonitor, func() *exec.Cmd) {
	m := newChildPictureMonitor()
	spawn, last := helperCommand(mode)
	m.spawn = spawn
	return m, last
}

func helperOpts() gst.PictureOpts {
	return gst.PictureOpts{Host: helperHost, Port: 40501, LatencyMs: 120, Passphrase: helperPassphrase, PBKeyLen: 16}
}

// drainStates collects states until the channel closes or the deadline passes.
func drainStates(t *testing.T, states <-chan gst.PictureState, within time.Duration) ([]gst.PictureState, bool) {
	t.Helper()
	var got []gst.PictureState
	deadline := time.After(within)
	for {
		select {
		case s, ok := <-states:
			if !ok {
				return got, true
			}
			got = append(got, s)
		case <-deadline:
			return got, false
		}
	}
}

func TestChildPictureMonitorRunsTheProtocol(t *testing.T) {
	// Start waits for "ready"; the states after it reach the channel in order;
	// a line that is not protocol is ignored; Stop closes stdin, the child
	// exits, STOPPED is the last state and the channel closes.
	m, last := newHelperMonitor("well")

	if err := m.Start(helperOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// The passphrase went on stdin and NOWHERE ELSE: not the command line,
	// which every process on the machine can read, and not the environment
	// block, which anything the child spawns inherits.
	cmd := last()
	for _, arg := range cmd.Args {
		if strings.Contains(arg, helperPassphrase) {
			t.Fatalf("the passphrase is on the child's command line: %q", cmd.Args)
		}
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, helperPassphraseEnv+"=") {
			continue // the helper's own expectation, set by this test
		}
		if strings.Contains(kv, helperPassphrase) {
			t.Fatalf("the passphrase is in the child's environment: %q", kv)
		}
	}

	// SHOWING is the helper saying the options arrived intact — host, port and
	// passphrase — on stdin.
	var seen []gst.PictureState
	deadline := time.After(10 * time.Second)
	for len(seen) < 2 {
		select {
		case s, ok := <-m.States():
			if !ok {
				t.Fatalf("the states channel closed after %v; the child died", seen)
			}
			seen = append(seen, s)
		case <-deadline:
			t.Fatalf("timed out with states %v", seen)
		}
	}
	if seen[0] != gst.PictureStateConnecting || seen[1] != gst.PictureStateShowing {
		t.Fatalf("states = %v, want [connecting showing]; SHOWING is the helper confirming the options "+
			"reached it on stdin intact", seen)
	}
	if m.exited() {
		t.Fatal("the monitor reports the child gone while it is running")
	}

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	rest, closed := drainStates(t, m.States(), 5*time.Second)
	if !closed {
		t.Fatalf("the states channel did not close after Stop; got %v", rest)
	}
	if len(rest) == 0 || rest[len(rest)-1] != gst.PictureStateStopped {
		t.Fatalf("states after Stop = %v, want STOPPED last", rest)
	}
	if !m.exited() {
		t.Fatal("Stop returned with the child not reaped")
	}
	// A second Stop is a no-op, not a second kill.
	if err := m.Stop(); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
}

func TestChildPictureMonitorStartRefusesWhenTheChildDoes(t *testing.T) {
	// A "fatal" first line is a configuration the child cannot use. Start
	// returns it as an error, synchronously, with the child already gone and
	// the states channel closed so nothing ranges over it for ever.
	m, _ := newHelperMonitor("fatal")

	err := m.Start(helperOpts())
	if err == nil {
		t.Fatal("Start() succeeded against a child that refused")
	}
	if !strings.Contains(err.Error(), "told to refuse") {
		t.Fatalf("Start() error = %q, which does not carry the child's reason", err)
	}
	if _, closed := drainStates(t, m.States(), 5*time.Second); !closed {
		t.Fatal("the states channel was left open after a refused Start")
	}
	if !m.exited() {
		t.Fatal("the refused child was not reaped")
	}
}

func TestChildPictureMonitorGivesUpOnAChildThatNeverSpeaks(t *testing.T) {
	old := pictureChildStartTimeout
	pictureChildStartTimeout = 300 * time.Millisecond
	defer func() { pictureChildStartTimeout = old }()

	m, _ := newHelperMonitor("silent")

	start := time.Now()
	err := m.Start(helperOpts())
	if err == nil {
		t.Fatal("Start() succeeded against a child that never said ready")
	}
	if !strings.Contains(err.Error(), "ready") {
		t.Fatalf("Start() error = %q, which does not say what was waited for", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Start() took %v to give up; the silent child was waited on past its budget", elapsed)
	}
	if _, closed := drainStates(t, m.States(), 5*time.Second); !closed {
		t.Fatal("the states channel was left open after a timed-out Start")
	}
}

func TestChildPictureMonitorKillsAChildThatIgnoresStop(t *testing.T) {
	// The case this whole design exists for: a pipeline wedged inside a state
	// change cannot honour the EOF. After the budget the child is killed, and
	// Stop returns with STOPPED sent and the channel closed either way.
	old := pictureChildStopBudget
	pictureChildStopBudget = 300 * time.Millisecond
	defer func() { pictureChildStopBudget = old }()

	m, _ := newHelperMonitor("stubborn")
	if err := m.Start(helperOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	start := time.Now()
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Stop() took %v; the stubborn child was waited on rather than killed", elapsed)
	}
	rest, closed := drainStates(t, m.States(), 5*time.Second)
	if !closed {
		t.Fatalf("the states channel did not close after the kill; got %v", rest)
	}
	if len(rest) == 0 || rest[len(rest)-1] != gst.PictureStateStopped {
		t.Fatalf("states after the kill = %v, want STOPPED last", rest)
	}
	if !m.exited() {
		t.Fatal("the killed child was not reaped")
	}
}

func TestChildPictureMonitorReportsAChildThatDiedOnItsOwn(t *testing.T) {
	// The operator closed the picture's window, or it crashed. Nobody called
	// Stop; the parent must still see STOPPED and a closed channel — that is
	// what lets App reap the session — and a Stop afterwards must be harmless.
	m, _ := newHelperMonitor("die")
	if err := m.Start(helperOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	got, closed := drainStates(t, m.States(), 10*time.Second)
	if !closed {
		t.Fatalf("the states channel did not close after the child exited; got %v", got)
	}
	if len(got) < 2 || got[0] != gst.PictureStateConnecting || got[len(got)-1] != gst.PictureStateStopped {
		t.Fatalf("states = %v, want [connecting ... stopped]", got)
	}
	// The channel closes a moment before the reader's own done signal; the
	// reap itself has already happened by then.
	waitForCond(t, "the dead child to be reaped", m.exited)
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() after the child died error = %v", err)
	}
}

func TestChildPictureMonitorGuardsItsLifecycle(t *testing.T) {
	m, _ := newHelperMonitor("well")
	if err := m.Stop(); !errors.Is(err, gst.ErrPictureNotStarted) {
		t.Fatalf("Stop() before Start error = %v, want ErrPictureNotStarted", err)
	}
	if err := m.Start(helperOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer m.Stop()
	if err := m.Start(helperOpts()); !errors.Is(err, gst.ErrPictureAlreadyStarted) {
		t.Fatalf("second Start() error = %v, want ErrPictureAlreadyStarted", err)
	}
}

func TestChildPictureMonitorReportsALaunchThatFailed(t *testing.T) {
	// No executable to run. Start fails, and the channel is closed so a
	// forwarder started on it would not leak — though App does not start one.
	m := newChildPictureMonitor()
	m.spawn = func() (*exec.Cmd, error) {
		return exec.Command("C:/this/executable/does/not/exist/wslcomms-picture.exe"), nil
	}
	if err := m.Start(helperOpts()); err == nil {
		t.Fatal("Start() succeeded with nothing to launch")
	}
	if _, closed := drainStates(t, m.States(), 2*time.Second); !closed {
		t.Fatal("the states channel was left open after a failed launch")
	}
}

func TestDefaultPictureChildCommandIsThisExecutableMarkedAsTheChild(t *testing.T) {
	cmd, err := defaultPictureChildCommand()
	if err != nil {
		t.Fatalf("defaultPictureChildCommand() error = %v", err)
	}
	if len(cmd.Args) != 1 {
		t.Fatalf("the child is launched with arguments %q; the options go on stdin, not the command line", cmd.Args)
	}
	marked := false
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, pictureChildEnv+"=") {
			marked = true
		}
	}
	if !marked {
		t.Fatalf("the child's environment does not carry %s; it would start as a second application", pictureChildEnv)
	}
}

// ---------------------------------------------------------------------------
// The child's half: its two refusals, with the exit intercepted
// ---------------------------------------------------------------------------

type childExited struct{}

// runChildWith runs runPictureChildAndExit with stdin fed from input (closed
// after it) and stdout captured, and returns what the child said. forceExit
// is replaced for the duration by a panic that this recovers.
func runChildWith(t *testing.T, input string, gstErr error) string {
	t.Helper()

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut, oldExit := os.Stdin, os.Stdout, forceExit
	os.Stdin, os.Stdout = inR, outW
	forceExit = func() { panic(childExited{}) }
	defer func() {
		os.Stdin, os.Stdout, forceExit = oldIn, oldOut, oldExit
	}()

	go func() {
		_, _ = io.WriteString(inW, input)
		_ = inW.Close()
	}()

	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(childExited); !ok {
					panic(r)
				}
			}
		}()
		runPictureChildAndExit(gstErr)
		t.Fatal("runPictureChildAndExit returned instead of exiting")
	}()

	_ = outW.Close()
	out, _ := io.ReadAll(outR)
	_ = outR.Close()
	_ = inR.Close()
	return string(out)
}

func TestPictureChildRefusesWithoutGStreamer(t *testing.T) {
	out := runChildWith(t, "", errors.New("the bundled GStreamer did not load"))
	if !strings.HasPrefix(out, "fatal ") || !strings.Contains(out, "GStreamer did not load") {
		t.Fatalf("the child said %q, want a fatal line carrying the GStreamer error", out)
	}
}

func TestPictureChildRefusesWithoutOptions(t *testing.T) {
	// stdin closed before a line arrived: the parent died between launching
	// the child and writing its options. The child says so and leaves.
	out := runChildWith(t, "", nil)
	if !strings.HasPrefix(out, "fatal ") || !strings.Contains(out, "stdin") {
		t.Fatalf("the child said %q, want a fatal line about the missing options", out)
	}
}

func TestPictureChildRefusesUnreadableOptions(t *testing.T) {
	out := runChildWith(t, "this is not json\n", nil)
	if !strings.HasPrefix(out, "fatal ") || !strings.Contains(out, "not readable") {
		t.Fatalf("the child said %q, want a fatal line about unreadable options", out)
	}
	if strings.Contains(out, helperPassphrase) {
		t.Fatalf("the child echoed its input: %q", out)
	}
}
