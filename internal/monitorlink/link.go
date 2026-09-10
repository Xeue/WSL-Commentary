// Package monitorlink is the wire between the application and its PGM MONITOR
// process: JSON lines over the child's stdin and stdout.
//
// Owner: WP-P.
//
// # What travels
//
// The monitor is this same executable relaunched (see app_monitor.go and
// monitor_main.go in package main). It has a window and a page of its own, and
// nearly everything that page needs — the configuration, the KVS credentials,
// the output device list, the meters' levels — belongs to the APPLICATION. So:
//
//	child → parent   ready                      the window is up and listening
//	                 call{id, method, args}     a bound method the page called;
//	                                            the parent answers through the
//	                                            SAME allowlist the LAN bridge uses
//	                 event{name, data}          something the parent's page should
//	                                            hear (the KVS state, for its lamp)
//	parent → child   result{id, ok, value|error}
//	                 event{name, data}          every event the parent's page hears,
//	                                            relayed so the monitor's page can
//	                                            subscribe to it exactly as it would
//	                                            in the application's own window
//
// One frame per line, one JSON object per frame. A line that is not a frame is
// logged and skipped: GStreamer or a library may print something on stdout
// once in a lifetime, and one stray line must not end the monitor.
//
// # The lifeline
//
// The child's stdin is held open by the parent and never used for anything a
// child could mistake for the end. When it closes — the parent stopped the
// monitor, restarted it, or died — the child's Done channel closes and the
// child exits. There is no way to orphan a monitor window rendering into a
// process nobody owns.
//
// # Nothing here blocks the parent
//
// The parent's event pump calls Send on every event it emits, at the rate the
// input meters run. A child that has stopped reading — its page wedged, its
// pipe full — must not stall the pump, so the parent writes through a bounded
// queue that DROPS THE OLDEST frame when full. Results share that queue: a
// child that cannot read a result is a child whose Call has a timeout.
package monitorlink

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Frame is one line on the wire. T says which fields matter.
type Frame struct {
	T      string            `json:"t"`
	ID     int64             `json:"id,omitempty"`
	Method string            `json:"method,omitempty"`
	Args   []json.RawMessage `json:"args,omitempty"`
	OK     bool              `json:"ok,omitempty"`
	Value  json.RawMessage   `json:"value,omitempty"`
	Error  string            `json:"error,omitempty"`
	Name   string            `json:"name,omitempty"`
	Data   json.RawMessage   `json:"data,omitempty"`
}

const (
	frameReady  = "ready"
	frameCall   = "call"
	frameResult = "result"
	frameEvent  = "event"

	// maxLine bounds one frame. A config document is a few kilobytes; a KVS
	// credential set is under that; a levels frame is a hundred bytes.
	maxLine = 4 * 1024 * 1024

	// sendQueue is the parent's outbound queue. At the meters' rate that is
	// many seconds of backlog before anything is dropped.
	sendQueue = 512

	// DefaultReadyTimeout bounds Start's wait for the child's ready. A WebView2
	// window has to be created and its page has to load and evaluate before
	// the child says anything; a cold machine can take a while.
	DefaultReadyTimeout = 30 * time.Second

	// DefaultStopBudget bounds Stop's wait for the child to exit after its
	// stdin closes, before it is killed.
	DefaultStopBudget = 3 * time.Second

	// DefaultCallTimeout bounds a child's Call. Most calls answer in
	// milliseconds; the credential fetch talks to M2L-X and takes seconds.
	DefaultCallTimeout = 20 * time.Second
)

// ErrChildExited is returned by Call when the parent's end has gone.
var ErrChildExited = errors.New("monitorlink: the link has closed")

// Event is one relayed event.
type Event struct {
	Name string
	Data json.RawMessage
}

// Dispatcher answers the child's calls. It runs on its own goroutine per call
// and may block; the result is written when it returns.
type Dispatcher func(ctx context.Context, method string, args []json.RawMessage) (any, error)

// ---------------------------------------------------------------------------
// The parent's end
// ---------------------------------------------------------------------------

// HostOptions configures a Host.
type HostOptions struct {
	// Spawn builds the child's command. The Host owns its pipes.
	Spawn func() (*exec.Cmd, error)
	// Dispatch answers the child's calls. Required.
	Dispatch Dispatcher
	// OnEvent receives the child's events. Optional.
	OnEvent func(name string, data json.RawMessage)
	// Logf receives the child's stderr lines and this package's own notes.
	// Optional.
	Logf func(format string, args ...any)

	ReadyTimeout time.Duration
	StopBudget   time.Duration
}

// Host is the parent's end of the link to one child process. Build it with
// NewHost, Start it once, Stop it once; build another for the next process.
type Host struct {
	opts HostOptions
	logf func(string, ...any)

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	closed bool // stdin closed

	out       chan Frame    // the outbound queue
	outDone   chan struct{} // writer finished
	ready     chan struct{}
	readyOnce sync.Once
	done      chan struct{} // reader finished and process reaped
	exitErr   error
	dropped   atomic.Int64

	calls sync.WaitGroup
	ctx   context.Context
	stop  context.CancelFunc

	stopOnce sync.Once
}

// NewHost builds a Host. Nothing runs until Start.
func NewHost(opts HostOptions) *Host {
	if opts.ReadyTimeout == 0 {
		opts.ReadyTimeout = DefaultReadyTimeout
	}
	if opts.StopBudget == 0 {
		opts.StopBudget = DefaultStopBudget
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Host{
		opts:    opts,
		logf:    logf,
		out:     make(chan Frame, sendQueue),
		outDone: make(chan struct{}),
		ready:   make(chan struct{}),
		done:    make(chan struct{}),
		ctx:     ctx,
		stop:    cancel,
	}
}

// Start launches the child and waits for its ready frame, or for it to exit,
// or for the ready timeout — in which case the child is killed and an error
// returned. After a successful Start the child is running and listening.
func (h *Host) Start() error {
	if h.opts.Dispatch == nil {
		return errors.New("monitorlink: Start requires a Dispatch")
	}
	cmd, err := h.opts.Spawn()
	if err != nil {
		h.finishWithout()
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		h.finishWithout()
		return fmt.Errorf("monitorlink: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		h.finishWithout()
		return fmt.Errorf("monitorlink: stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		h.finishWithout()
		return fmt.Errorf("monitorlink: stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		h.finishWithout()
		return fmt.Errorf("monitorlink: could not launch the monitor process: %w", err)
	}
	h.mu.Lock()
	h.cmd = cmd
	h.stdin = stdin
	h.mu.Unlock()
	h.logf("monitorlink: monitor process launched, pid %d", cmd.Process.Pid)

	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), maxLine)
		for sc.Scan() {
			h.logf("monitor: %s", sc.Text())
		}
	}()
	go h.write(stdin)
	go h.read(stdout, stderrDone)

	select {
	case <-h.ready:
		return nil
	case <-h.done:
		return fmt.Errorf("monitorlink: the monitor process exited before it was ready: %w", h.ExitError())
	case <-time.After(h.opts.ReadyTimeout):
		h.Kill()
		return fmt.Errorf("monitorlink: the monitor process did not become ready within %s", h.opts.ReadyTimeout)
	}
}

// finishWithout closes a Host whose child never launched.
func (h *Host) finishWithout() {
	h.stop()
	close(h.outDone)
	close(h.done)
}

// write drains the outbound queue into stdin. It is the only writer.
func (h *Host) write(stdin io.Writer) {
	defer close(h.outDone)
	w := bufio.NewWriter(stdin)
	for {
		select {
		case <-h.ctx.Done():
			return
		case f := <-h.out:
			b, err := json.Marshal(f)
			if err != nil {
				h.logf("monitorlink: could not encode a %s frame: %v", f.T, err)
				continue
			}
			if _, err := w.Write(append(b, '\n')); err != nil {
				return
			}
			if err := w.Flush(); err != nil {
				return
			}
		}
	}
}

// enqueue puts a frame on the outbound queue, dropping the oldest if full.
func (h *Host) enqueue(f Frame) {
	for {
		select {
		case h.out <- f:
			return
		default:
			select {
			case <-h.out:
				if n := h.dropped.Add(1); n == 1 || n%100 == 0 {
					h.logf("monitorlink: the monitor is not keeping up; %d frames dropped so far", n)
				}
			default:
			}
		}
	}
}

// Send relays an event to the child. It never blocks.
func (h *Host) Send(name string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		h.logf("monitorlink: could not encode the %q event: %v", name, err)
		return
	}
	h.enqueue(Frame{T: frameEvent, Name: name, Data: b})
}

// read consumes the child's stdout until EOF, then reaps the process.
func (h *Host) read(stdout io.Reader, stderrDone <-chan struct{}) {
	defer close(h.done)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var f Frame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			h.logf("monitorlink: the monitor wrote a line that is not a frame: %q", truncate(line, 200))
			continue
		}
		switch f.T {
		case frameReady:
			h.readyOnce.Do(func() { close(h.ready) })
		case frameCall:
			h.calls.Add(1)
			go h.answer(f)
		case frameEvent:
			if h.opts.OnEvent != nil {
				h.opts.OnEvent(f.Name, f.Data)
			}
		default:
			h.logf("monitorlink: the monitor sent an unexpected %q frame", f.T)
		}
	}

	// EOF: the child is gone. Stop the writer, join the stderr relay (Wait
	// closes the pipes), and reap.
	h.stop()
	h.mu.Lock()
	cmd := h.cmd
	h.mu.Unlock()
	<-stderrDone
	if cmd != nil {
		err := cmd.Wait()
		h.mu.Lock()
		h.exitErr = err
		h.mu.Unlock()
		if err != nil {
			h.logf("monitorlink: monitor process exited: %v", err)
		} else {
			h.logf("monitorlink: monitor process exited")
		}
	}
}

// answer dispatches one call and queues its result.
func (h *Host) answer(f Frame) {
	defer h.calls.Done()
	value, err := h.opts.Dispatch(h.ctx, f.Method, f.Args)
	res := Frame{T: frameResult, ID: f.ID}
	if err != nil {
		res.Error = err.Error()
	} else {
		b, merr := json.Marshal(value)
		if merr != nil {
			res.Error = fmt.Sprintf("monitorlink: could not encode the result of %s: %v", f.Method, merr)
		} else {
			res.OK = true
			res.Value = b
		}
	}
	h.enqueue(res)
}

// Stop closes the child's stdin — its signal to exit — waits up to the stop
// budget, and kills it if it has not gone. It returns once the process has
// been reaped. Idempotent.
func (h *Host) Stop() {
	h.stopOnce.Do(func() {
		h.closeStdin()
		select {
		case <-h.done:
			return
		case <-time.After(h.opts.StopBudget):
		}
		h.logf("monitorlink: the monitor process did not exit within %s of being told to; killing it", h.opts.StopBudget)
		h.Kill()
	})
	<-h.done
}

// Kill ends the child unconditionally and waits for it to be reaped.
func (h *Host) Kill() {
	h.closeStdin()
	h.mu.Lock()
	cmd := h.cmd
	h.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	<-h.done
}

func (h *Host) closeStdin() {
	h.mu.Lock()
	stdin := h.stdin
	closed := h.closed
	h.closed = true
	h.mu.Unlock()
	if stdin != nil && !closed {
		// The writer may be mid-write; let it finish its frame, then close.
		h.stop()
		<-h.outDone
		_ = stdin.Close()
	}
}

// Done is closed when the child has exited and been reaped, however that came
// about.
func (h *Host) Done() <-chan struct{} { return h.done }

// Exited reports whether the child has gone.
func (h *Host) Exited() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// ExitError is the process's Wait error, valid after Done: nil for exit code 0.
func (h *Host) ExitError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exitErr
}

// ExitCode is the child's exit code after Done: 0 for a clean exit, the code
// otherwise, -1 if it could not be determined or was killed.
func (h *Host) ExitCode() int {
	err := h.ExitError()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// PID is the child's process id, or 0 before Start.
func (h *Host) PID() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cmd == nil || h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}

// Dropped is how many outbound frames were discarded because the child was
// not keeping up.
func (h *Host) Dropped() int64 { return h.dropped.Load() }

// ---------------------------------------------------------------------------
// The child's end
// ---------------------------------------------------------------------------

// Client is the child's end. It reads frames from in (the process's stdin) and
// writes them to out (its stdout).
type Client struct {
	in  io.Reader
	out io.Writer

	writeMu sync.Mutex

	nextID  atomic.Int64
	pending sync.Map // id → chan Frame

	events chan Event
	done   chan struct{}
	once   sync.Once

	callTimeout time.Duration
}

// NewClient builds the child's end and starts reading. Events arrive on
// Events; Done closes when in reaches EOF.
func NewClient(in io.Reader, out io.Writer) *Client {
	c := &Client{
		in:          in,
		out:         out,
		events:      make(chan Event, sendQueue),
		done:        make(chan struct{}),
		callTimeout: DefaultCallTimeout,
	}
	go c.read()
	return c
}

// Ready tells the parent the child is up and listening.
func (c *Client) Ready() { _ = c.writeFrame(Frame{T: frameReady}) }

// Emit sends an event to the parent.
func (c *Client) Emit(name string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return c.writeFrame(Frame{T: frameEvent, Name: name, Data: b})
}

// Call invokes a parent method and returns its JSON result. It fails with
// ErrChildExited once the link has closed, and with a timeout otherwise
// bounded by ctx or the call timeout, whichever is sooner.
func (c *Client) Call(ctx context.Context, method string, args ...any) (json.RawMessage, error) {
	select {
	case <-c.done:
		return nil, ErrChildExited
	default:
	}
	raw := make([]json.RawMessage, 0, len(args))
	for _, a := range args {
		b, err := json.Marshal(a)
		if err != nil {
			return nil, fmt.Errorf("monitorlink: encoding an argument of %s: %w", method, err)
		}
		raw = append(raw, b)
	}
	id := c.nextID.Add(1)
	reply := make(chan Frame, 1)
	c.pending.Store(id, reply)
	defer c.pending.Delete(id)

	if err := c.writeFrame(Frame{T: frameCall, ID: id, Method: method, Args: raw}); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()
	select {
	case f := <-reply:
		if !f.OK {
			return nil, errors.New(f.Error)
		}
		return f.Value, nil
	case <-c.done:
		return nil, ErrChildExited
	case <-ctx.Done():
		return nil, fmt.Errorf("monitorlink: %s: %w", method, ctx.Err())
	}
}

// Events delivers the parent's relayed events. A slow consumer loses the
// oldest, never the newest.
func (c *Client) Events() <-chan Event { return c.events }

// Done is closed when the parent's end has gone: stdin reached EOF.
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) writeFrame(f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.out.Write(append(b, '\n'))
	return err
}

func (c *Client) read() {
	defer c.once.Do(func() { close(c.done) })
	sc := bufio.NewScanner(c.in)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var f Frame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			continue
		}
		switch f.T {
		case frameResult:
			if ch, ok := c.pending.Load(f.ID); ok {
				select {
				case ch.(chan Frame) <- f:
				default:
				}
			}
		case frameEvent:
			ev := Event{Name: f.Name, Data: f.Data}
			for {
				select {
				case c.events <- ev:
					goto delivered
				default:
					select {
					case <-c.events:
					default:
					}
				}
			}
		delivered:
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
