package monitorlink

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// The child is this test binary, re-run with an environment variable that
// turns TestHelperProcess into a monitor. It speaks the protocol over real
// pipes through the same Client the monitor process uses.
const helperEnv = "MONITORLINK_TEST_HELPER"

func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return
	}
	defer os.Exit(0)

	switch mode {
	case "silent":
		time.Sleep(10 * time.Second)
		return
	case "stubborn":
		c := NewClient(os.Stdin, os.Stdout)
		c.Ready()
		<-c.Done()
		time.Sleep(30 * time.Second) // ignores the lifeline
		return
	case "noisy":
		// Something that is not a frame on stdout, then the protocol.
		fmt.Fprintln(os.Stdout, "GStreamer-WARNING: something printed this")
	}

	c := NewClient(os.Stdin, os.Stdout)
	c.Ready()
	fmt.Fprintln(os.Stderr, "helper: on stderr")

	// A call the parent answers, and one it refuses.
	v, err := c.Call(context.Background(), "Add", 2, 3)
	if err != nil {
		_ = c.Emit("callFailed", err.Error())
	} else {
		_ = c.Emit("sum", json.RawMessage(v))
	}
	if _, err := c.Call(context.Background(), "Refused"); err != nil {
		_ = c.Emit("refused", err.Error())
	}

	// Echo every relayed event until the lifeline closes.
	for {
		select {
		case ev := <-c.Events():
			_ = c.Emit("echo:"+ev.Name, ev.Data)
		case <-c.Done():
			_ = c.Emit("bye", "stdin closed")
			return
		}
	}
}

func helperSpawn(mode string) func() (*exec.Cmd, error) {
	return func() (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		cmd.Env = append(os.Environ(), helperEnv+"="+mode)
		return cmd, nil
	}
}

type recorder struct {
	mu     sync.Mutex
	events []Event
	logs   []string
	got    chan Event
}

func newRecorder() *recorder { return &recorder{got: make(chan Event, 64)} }

func (r *recorder) onEvent(name string, data json.RawMessage) {
	r.mu.Lock()
	r.events = append(r.events, Event{Name: name, Data: data})
	r.mu.Unlock()
	select {
	case r.got <- Event{Name: name, Data: data}:
	default:
	}
}

func (r *recorder) logf(format string, args ...any) {
	r.mu.Lock()
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

func (r *recorder) wait(t *testing.T, name string) Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-r.got:
			if ev.Name == name {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the %q event; got %v", name, r.names())
		}
	}
}

func (r *recorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Name)
	}
	return out
}

func (r *recorder) hasLog(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func dispatch(_ context.Context, method string, args []json.RawMessage) (any, error) {
	switch method {
	case "Add":
		var a, b int
		_ = json.Unmarshal(args[0], &a)
		_ = json.Unmarshal(args[1], &b)
		return a + b, nil
	default:
		return nil, errors.New("no such method: " + method)
	}
}

func newHostFor(t *testing.T, mode string, rec *recorder) *Host {
	t.Helper()
	h := NewHost(HostOptions{
		Spawn:    helperSpawn(mode),
		Dispatch: dispatch,
		OnEvent:  rec.onEvent,
		Logf:     rec.logf,
	})
	t.Cleanup(h.Kill)
	return h
}

func TestLinkRoundTrip(t *testing.T) {
	rec := newRecorder()
	h := newHostFor(t, "well", rec)
	if err := h.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// The child called Add(2, 3) through the parent's dispatcher.
	sum := rec.wait(t, "sum")
	if string(sum.Data) != "5" {
		t.Fatalf("the child's Add(2, 3) came back as %s, want 5", sum.Data)
	}
	// And the refused method came back as the dispatcher's error, verbatim.
	refused := rec.wait(t, "refused")
	if !strings.Contains(string(refused.Data), "no such method") {
		t.Fatalf("the refusal reached the child as %s", refused.Data)
	}

	// Events relayed parent → child come back echoed, in order, intact.
	for i := 0; i < 3; i++ {
		h.Send("levels", map[string]any{"peak": []float64{float64(i), -20}})
	}
	for i := 0; i < 3; i++ {
		ev := rec.wait(t, "echo:levels")
		var frame struct{ Peak []float64 }
		if err := json.Unmarshal(ev.Data, &frame); err != nil || len(frame.Peak) != 2 || frame.Peak[0] != float64(i) {
			t.Fatalf("echoed levels #%d = %s (err %v), want peak[0] == %d", i, ev.Data, err, i)
		}
	}

	// The child's stderr reached the parent's log.
	if !rec.hasLog("helper: on stderr") {
		t.Fatal("the child's stderr was not relayed into the parent's log")
	}

	// Stop closes the lifeline; the child says goodbye and exits cleanly.
	h.Stop()
	if !h.Exited() {
		t.Fatal("Stop returned with the child not reaped")
	}
	if code := h.ExitCode(); code != 0 {
		t.Fatalf("exit code %d, want 0 for a child that left on the lifeline", code)
	}
	if got := rec.names(); got[len(got)-1] != "bye" {
		t.Fatalf("the last event was %q, want the child's bye", got[len(got)-1])
	}
}

func TestLinkToleratesALineThatIsNotAFrame(t *testing.T) {
	rec := newRecorder()
	h := newHostFor(t, "noisy", rec)
	if err := h.Start(); err != nil {
		t.Fatalf("Start() error = %v; a stray stdout line must not end the monitor", err)
	}
	rec.wait(t, "sum")
	if !rec.hasLog("not a frame") {
		t.Fatal("the stray line was not logged")
	}
	h.Stop()
}

func TestLinkGivesUpOnAChildThatNeverSaysReady(t *testing.T) {
	rec := newRecorder()
	h := NewHost(HostOptions{
		Spawn:        helperSpawn("silent"),
		Dispatch:     dispatch,
		OnEvent:      rec.onEvent,
		Logf:         rec.logf,
		ReadyTimeout: 300 * time.Millisecond,
	})
	t.Cleanup(h.Kill)
	start := time.Now()
	err := h.Start()
	if err == nil {
		t.Fatal("Start() succeeded against a child that never said ready")
	}
	if !strings.Contains(err.Error(), "ready") {
		t.Fatalf("Start() error = %q", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Start waited past its budget")
	}
	if !h.Exited() {
		t.Fatal("the silent child was not killed and reaped")
	}
}

func TestLinkKillsAChildThatIgnoresTheLifeline(t *testing.T) {
	rec := newRecorder()
	h := NewHost(HostOptions{
		Spawn:      helperSpawn("stubborn"),
		Dispatch:   dispatch,
		OnEvent:    rec.onEvent,
		Logf:       rec.logf,
		StopBudget: 300 * time.Millisecond,
	})
	t.Cleanup(h.Kill)
	if err := h.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	start := time.Now()
	h.Stop()
	if time.Since(start) > 10*time.Second {
		t.Fatal("Stop waited on a child that ignored the lifeline")
	}
	if !h.Exited() {
		t.Fatal("the stubborn child was not reaped")
	}
	if h.ExitCode() == 0 {
		t.Fatal("a killed child reports exit code 0")
	}
	if !rec.hasLog("killing it") {
		t.Fatal("the kill was not logged")
	}
}

func TestLinkReportsALaunchThatFailed(t *testing.T) {
	h := NewHost(HostOptions{
		Spawn: func() (*exec.Cmd, error) {
			return exec.Command("C:/no/such/dir/wslcomms-monitor.exe"), nil
		},
		Dispatch: dispatch,
	})
	if err := h.Start(); err == nil {
		t.Fatal("Start() succeeded with nothing to launch")
	}
	if !h.Exited() {
		t.Fatal("Done is not closed after a failed launch")
	}
	// Stop and Kill on a host that never launched are harmless.
	h.Stop()
	h.Kill()
}

func TestSendNeverBlocksWhenTheChildIsNotReading(t *testing.T) {
	// The parent's event pump calls Send on every event. A child that has
	// stopped reading must cost dropped frames, never a stalled pump.
	rec := newRecorder()
	h := NewHost(HostOptions{
		Spawn:      helperSpawn("stubborn"), // says ready, then reads nothing
		Dispatch:   dispatch,
		OnEvent:    rec.onEvent,
		Logf:       rec.logf,
		StopBudget: 300 * time.Millisecond,
	})
	t.Cleanup(h.Kill)
	if err := h.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	big := strings.Repeat("x", 4096)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < sendQueue*8; i++ {
			h.Send("levels", big)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Send blocked; the event pump would have stalled behind a wedged monitor")
	}
	if h.Dropped() == 0 {
		t.Fatal("nothing was dropped, so the queue must have blocked somewhere")
	}
	h.Stop()
}

// The child's end against an in-memory parent, so the Client's own contract
// — ids, timeouts, EOF — is pinned without a process.
func TestClientCallsTimeOutAndFailAfterEOF(t *testing.T) {
	inR, inW := io.Pipe()
	var out strings.Builder
	c := NewClient(inR, &out)
	c.callTimeout = 200 * time.Millisecond

	// Nobody answers: the call times out.
	_, err := c.Call(context.Background(), "Slow")
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("Call with no answer error = %v, want a deadline", err)
	}
	if !strings.Contains(out.String(), `"method":"Slow"`) {
		t.Fatalf("the call frame was not written: %q", out.String())
	}

	// An answer with the right id resolves the call.
	go func() {
		// Wait for the call frame to be written, then answer it.
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintln(inW, `{"t":"result","id":2,"ok":true,"value":{"x":1}}`)
	}()
	v, err := c.Call(context.Background(), "Fast")
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if string(v) != `{"x":1}` {
		t.Fatalf("Call() = %s", v)
	}

	// EOF: Done closes and every later call fails at once.
	_ = inW.Close()
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done did not close on EOF")
	}
	if _, err := c.Call(context.Background(), "Anything"); !errors.Is(err, ErrChildExited) {
		t.Fatalf("Call after EOF error = %v, want ErrChildExited", err)
	}
}

func TestClientDeliversEventsNewestFirstWhenSlow(t *testing.T) {
	inR, inW := io.Pipe()
	c := NewClient(inR, io.Discard)
	w := bufio.NewWriter(inW)
	// More events than the buffer holds, with nobody reading yet.
	for i := 0; i < sendQueue+10; i++ {
		fmt.Fprintf(w, `{"t":"event","name":"levels","data":%d}`+"\n", i)
	}
	_ = w.Flush()
	_ = inW.Close()
	<-c.Done()

	var last string
	n := 0
	for _, ev := range drain(c.Events()) {
		last = string(ev.Data)
		n++
	}
	if n != sendQueue {
		t.Fatalf("delivered %d events, want the buffer's %d", n, sendQueue)
	}
	if last != fmt.Sprint(sendQueue+9) {
		t.Fatalf("the newest event was lost: last delivered = %s", last)
	}
}

func drain(ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}
