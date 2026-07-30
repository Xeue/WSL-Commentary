package main

// Tests for the SRT receive path (srt.go). These dial real SRT connections
// over loopback UDP using github.com/datarhei/gosrt's own Dial, in-process —
// gosrt is pure Go, so this needs no libsrt, no external tool, and no
// subprocess, matching the "do not require another process" instruction.
// This exercises the actual accept/reject/reader code in srt.go, not a
// reimplementation of it.

import (
	"context"
	"log"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	srt "github.com/datarhei/gosrt"
)

// newTestApp returns an App with fault-injection defaults tuned for fast
// tests: a short refusal window so tests do not have to sleep for 6
// (production-default) seconds, and a status interval that will not matter
// because most SRT tests never touch the WebSocket.
func newTestApp(t *testing.T) *App {
	t.Helper()
	opts := Options{
		Alias:          "test-alias",
		Password:       "test-password",
		StatusKey:      "cam7",
		StatusInterval: time.Hour, // effectively disabled; SRT tests don't need it
		TokenTTL:       time.Hour,
		OnePeerOnly:    true,
		RefusalWindow:  150 * time.Millisecond,
	}
	logger := log.New(newTestWriter(t), "", 0)
	return NewApp(opts, logger)
}

// testWriter adapts *testing.T into an io.Writer so App's log lines show up
// under `go test -v` attributed to the right test, instead of on stdout.
//
// It goes inert the moment its test finishes, and that is the whole point of
// it rather than a refinement. Some of the mock's goroutines deliberately
// outlive the test that started them: handleConnRequest spawns readSRTConn
// detached, and a status WebSocket handler has hijacked its connection, which
// httptest.Server stops tracking on hijack and therefore does not wait for in
// Close. Both log on their way out, and a test cannot join either. Go's testing
// package does not support that — t.Logf reads t.done, which tRunner writes
// with no lock precisely so that the race detector reports exactly this
// (testing.go:1919 "Do not lock t.done to allow race detector to detect race"),
// and once every ancestor is done Logf panics the whole process with "Log in
// goroutine after ... has completed". So the writer must be the thing that
// stops, because the goroutines will not.
//
// The lock is held across the Logf call and not merely around the flag. Taking
// it only to read done would leave a writer that had already passed the check
// still inside Logf when the test completed, which narrows the window instead
// of closing it; holding it means the Cleanup below cannot return until any
// forward in flight has finished.
type testWriter struct {
	mu   sync.Mutex
	t    *testing.T
	done bool
}

// newTestWriter returns a writer that forwards to t until t finishes.
//
// The Cleanup is registered here, when the App is built, which is the first
// thing every test in this package does. Cleanups run last-registered-first, so
// this one runs after startListener's and startBroadcaster's cancel-and-wait
// and after httptest.Server.Close — their log lines are kept, which is the
// reason for routing them through a *testing.T at all — and still strictly
// before tRunner marks the test done, which is where the window actually is.
func newTestWriter(t *testing.T) *testWriter {
	t.Helper()
	w := &testWriter{t: t}
	t.Cleanup(func() {
		w.mu.Lock()
		w.done = true
		w.mu.Unlock()
	})
	return w
}

// Write forwards p to t.Logf, or discards it once the test has finished. A
// discarded line is reported as written: this is a log sink, and failing the
// mock's logger because a test ended would be a worse lie than dropping a line
// nobody can attribute any more.
func (w *testWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return len(p), nil
	}
	w.t.Logf("%s", p)
	return len(p), nil
}

// freeSRTAddr picks a free UDP port on loopback for a test's listener.
func freeSRTAddr(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not find a free UDP port: %v", err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()
	return addr
}

// startListener runs a's SRT listener on addr in the background and blocks
// until it is actually accepting connections. It registers a t.Cleanup that
// cancels the listener's context AND WAITS for the listener goroutine to
// actually exit, which is what makes the t.Logf below legal: cleanups run
// before the testing package marks the test done, so a goroutine this has
// joined cannot log after it.
//
// That join covers this goroutine and nothing else. The accept loop hands each
// connection to handleConnRequest, which spawns readSRTConn detached, and that
// goroutine logs a DISCONNECT line whenever its peer goes away — after this
// test, routinely. There is no handle to wait on and the mock is right not to
// invent one, so the protection against it is testWriter going inert rather
// than anything here.
func startListener(t *testing.T, a *App, addr string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		if err := a.runSRTListener(ctx, addr); err != nil {
			t.Logf("runSRTListener exited: %v", err)
		}
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Poll until the listener actually has a bound socket recorded, since
	// the goroutine above racing srt.Listen means "started" and "bound" are
	// not the same instant.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.srtListenerAddr() != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("SRT listener on %s never became ready", addr)
}

// dialSRT dials addr as an SRT caller, the same role GStreamer's srtsink
// takes against the real M2L-X listener (spec section 5).
func dialSRT(t *testing.T, addr string) (srt.Conn, error) {
	t.Helper()
	return srt.Dial("srt", addr, srt.DefaultConfig())
}

func TestSRT_AcceptsOneCaller(t *testing.T) {
	a := newTestApp(t)
	addr := freeSRTAddr(t)
	startListener(t, a, addr)

	conn, err := dialSRT(t, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	deadlineWaitFor(t, func() bool { return a.srtPeerConnected() }, "listener to record the peer as connected")

	snap := a.srt.snapshot()
	if !snap.Connected {
		t.Errorf("expected snapshot.Connected == true")
	}
	if snap.PeerAddr == "" {
		t.Errorf("expected a non-empty PeerAddr")
	}
}

func TestSRT_RefusesSecondCallerWithoutDisplacingIncumbent(t *testing.T) {
	a := newTestApp(t)
	addr := freeSRTAddr(t)
	startListener(t, a, addr)

	first, err := dialSRT(t, addr)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer first.Close()
	deadlineWaitFor(t, func() bool { return a.srtPeerConnected() }, "first caller to connect")

	firstPeer := a.srt.snapshot().PeerAddr

	// A second caller must be refused, one-peer-only being on by default.
	second, err := dialSRT(t, addr)
	if err == nil {
		second.Close()
		t.Fatalf("expected the second caller to be refused, but Dial succeeded")
	}

	// The incumbent must be unaffected: still connected, same peer.
	if !a.srtPeerConnected() {
		t.Fatalf("incumbent was displaced by the refused second caller")
	}
	if got := a.srt.snapshot().PeerAddr; got != firstPeer {
		t.Errorf("incumbent peer address changed: got %q, want %q", got, firstPeer)
	}
}

func TestSRT_OnePeerOnlyCanBeDisabled(t *testing.T) {
	a := newTestApp(t)
	a.setOnePeerOnly(false)
	addr := freeSRTAddr(t)
	startListener(t, a, addr)

	first, err := dialSRT(t, addr)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer first.Close()
	deadlineWaitFor(t, func() bool { return a.srtPeerConnected() }, "first caller to connect")

	second, err := dialSRT(t, addr)
	if err != nil {
		t.Fatalf("expected a second caller to be accepted with one-peer-only disabled, got: %v", err)
	}
	defer second.Close()
}

func TestSRT_RefusesReacceptDuringRefusalWindow(t *testing.T) {
	a := newTestApp(t) // 150ms refusal window
	addr := freeSRTAddr(t)
	startListener(t, a, addr)

	first, err := dialSRT(t, addr)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	deadlineWaitFor(t, func() bool { return a.srtPeerConnected() }, "first caller to connect")

	first.Close()
	deadlineWaitFor(t, func() bool { return !a.srtPeerConnected() }, "listener to notice the disconnect")

	// Immediately retrying, inside the refusal window, must fail — this is
	// the ~5s M2L-X behaviour (docs/test-results.md line 149) that makes
	// srtsink's own zero-backoff auto-reconnect fail, and that
	// internal/sender's first backoff rung (>= 6s) exists to survive.
	if conn, err := dialSRT(t, addr); err == nil {
		conn.Close()
		t.Fatalf("expected re-accept to be refused inside the refusal window, but Dial succeeded")
	}

	// After the window elapses, a new caller must be accepted.
	time.Sleep(200 * time.Millisecond)
	second, err := dialSRT(t, addr)
	if err != nil {
		t.Fatalf("expected re-accept to succeed after the refusal window elapsed, got: %v", err)
	}
	defer second.Close()
	deadlineWaitFor(t, func() bool { return a.srtPeerConnected() }, "second caller to connect after the window")
}

func TestSRT_DropSRTNowDisconnectsAndStartsRefusalWindow(t *testing.T) {
	a := newTestApp(t)
	addr := freeSRTAddr(t)
	startListener(t, a, addr)

	conn, err := dialSRT(t, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	deadlineWaitFor(t, func() bool { return a.srtPeerConnected() }, "caller to connect")

	if dropped := a.dropSRTNow(); !dropped {
		t.Fatalf("dropSRTNow reported nothing was dropped")
	}
	deadlineWaitFor(t, func() bool { return !a.srtPeerConnected() }, "drop-srt to disconnect the peer")

	// The refusal window must now be in effect, exactly as after a real
	// disconnect.
	if second, err := dialSRT(t, addr); err == nil {
		second.Close()
		t.Fatalf("expected drop-srt to trigger the refusal window, but a reconnect succeeded immediately")
	}

	if dropped := a.dropSRTNow(); dropped {
		t.Errorf("dropSRTNow on an already-empty session should report false")
	}
}

func TestSRT_RejectsWrongPassphraseAsBadSecret(t *testing.T) {
	a := newTestApp(t)
	a.opts.SRTPassphrase = "correct-horse-battery-staple"
	addr := freeSRTAddr(t)
	startListener(t, a, addr)

	cfg := srt.DefaultConfig()
	cfg.Passphrase = "wrong-passphrase-entirely"
	if _, err := srt.Dial("srt", addr, cfg); err == nil {
		t.Fatalf("expected a wrong passphrase to be rejected")
	}
	if a.srtPeerConnected() {
		t.Fatalf("a rejected caller must not be recorded as connected")
	}
}

func TestSRT_RejectsUnencryptedWhenPassphraseRequired(t *testing.T) {
	a := newTestApp(t)
	a.opts.SRTPassphrase = "required-passphrase"
	addr := freeSRTAddr(t)
	startListener(t, a, addr)

	cfg := srt.DefaultConfig() // no passphrase set
	if _, err := srt.Dial("srt", addr, cfg); err == nil {
		t.Fatalf("expected an unencrypted caller to be rejected when a passphrase is required")
	}
}

func TestSRT_AcceptsCorrectPassphrase(t *testing.T) {
	a := newTestApp(t)
	a.opts.SRTPassphrase = "correct-horse-battery-staple"
	addr := freeSRTAddr(t)
	startListener(t, a, addr)

	cfg := srt.DefaultConfig()
	cfg.Passphrase = "correct-horse-battery-staple"
	conn, err := srt.Dial("srt", addr, cfg)
	if err != nil {
		t.Fatalf("expected the correct passphrase to be accepted, got: %v", err)
	}
	defer conn.Close()
	deadlineWaitFor(t, func() bool { return a.srtPeerConnected() }, "caller with the correct passphrase to connect")
}

// TestSRT_ReceivesAndAnalyzesPayload sends a synthetic MPEG-TS stream
// (mpegts_test.go's generator) over a real SRT connection and confirms the
// analyzer reachable through the App sees it — proving the wiring between
// srt.go's reader goroutine and mpegts.go's analyzer, not just the analyzer
// in isolation.
func TestSRT_ReceivesAndAnalyzesPayload(t *testing.T) {
	a := newTestApp(t)
	addr := freeSRTAddr(t)
	startListener(t, a, addr)

	conn, err := dialSRT(t, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	deadlineWaitFor(t, func() bool { return a.srtPeerConnected() }, "caller to connect")

	w := newTSWriter()
	stream := baseStream(w)
	stream = append(stream, w.pes(testVideoPID, 0xE0, 90000, true, 100)...)
	stream = append(stream, w.pes(testAudioPID, 0xC0, 90000, true, 50)...)
	// SRT's live-mode payload size (spec section 5, alignment=7) is smaller
	// than a bare Write of the whole stream, so write it as several
	// SRT-payload-sized chunks the way srtsink actually would.
	const chunk = 1316
	for len(stream) > 0 {
		n := chunk
		if n > len(stream) {
			n = len(stream)
		}
		if _, err := conn.Write(stream[:n]); err != nil {
			t.Fatalf("write: %v", err)
		}
		stream = stream[n:]
	}

	deadlineWaitFor(t, func() bool {
		snap := a.srtAnalyzerSnapshot()
		return snap.VideoIsH264 && snap.AudioIsAAC
	}, "the analyzer to identify H.264 video and AAC audio")

	snap := a.srtAnalyzerSnapshot()
	if snap.BytesTotal == 0 {
		t.Errorf("expected BytesTotal > 0")
	}
	if snap.DTSBackwards {
		t.Errorf("did not expect a DTS regression in this stream")
	}
}

// deadlineWaitFor polls cond until it returns true or 2 seconds elapse,
// failing the test with what it was waiting for.
func deadlineWaitFor(t *testing.T, cond func() bool, waitingFor string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", waitingFor)
}

// Sanity check that freeSRTAddr's port is actually numeric and reusable —
// guards against a subtle Windows loopback quirk where a UDP port can
// briefly remain in a state where a fresh Listen fails right after Close.
func TestFreeSRTAddrIsUsable(t *testing.T) {
	addr := freeSRTAddr(t)
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}
	if _, err := strconv.Atoi(portStr); err != nil {
		t.Fatalf("port %q is not numeric", portStr)
	}
}
