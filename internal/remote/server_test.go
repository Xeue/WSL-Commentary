package remote

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// Shared test harness
//
// Every transport test drives a REAL server over a REAL TLS socket with a REAL
// gorilla client, because the properties being protected — the origin check,
// the cookie requirement, the fan-out discipline, the mid-call cancellation —
// are properties of the wiring, not of any one function, and a test that mocked
// the wiring would not catch a regression in it. The dispatcher is a fake so the
// package never imports the root or GStreamer and the whole suite runs at Gate A
// with CGO_ENABLED=0.
// ---------------------------------------------------------------------------

// testIndexHTML mimics the built frontend/dist/index.html closely enough to
// exercise shim injection: a deferred module bundle preceded by nothing, so the
// shim tag must land immediately before it.
const testIndexHTML = `<!doctype html>
<html lang="en-GB">
  <head>
    <meta charset="UTF-8" />
    <title>WSL Commentary</title>
    <script type="module" crossorigin src="/assets/index-B3qX8jaX.js"></script>
    <link rel="stylesheet" crossorigin href="/assets/index-c0yp7ugS.css">
  </head>
  <body>
    <div id="app"></div>
  </body>
</html>
`

// testAssetBytes is a static asset that must pass through byte-for-byte.
var testAssetBytes = []byte("// the bundle, unmodified\nconsole.log('hi');\n")

func testAssetsFS() fs.FS {
	return fstest.MapFS{
		"index.html":                &fstest.MapFile{Data: []byte(testIndexHTML)},
		"assets/index-B3qX8jaX.js":  &fstest.MapFile{Data: testAssetBytes},
		"assets/index-c0yp7ugS.css": &fstest.MapFile{Data: []byte("body{}\n")},
	}
}

// testClient builds a client record named "op" with the given capabilities and
// password "s3cret".
func testClient(t *testing.T, name, password string, caps ...string) Client {
	t.Helper()
	p, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	return Client{Name: name, PBKDF2: p, Caps: caps}
}

type harness struct {
	srv  *Server
	auth *Authenticator
	disp *fakeDispatcher
	addr string
	hc   *http.Client
}

// newHarness starts a loopback TLS server with the given clients and returns a
// harness. It shortens the login min-delay so timing-floored tests do not add
// real seconds, and registers cleanup.
func newHarness(t *testing.T, clients []Client) *harness {
	t.Helper()
	disp := newFakeDispatcher()
	auth := NewAuthenticator(clients)
	auth.minDelay = 5 * time.Millisecond
	srv := NewServer(Options{
		Enabled:    true,
		Bind:       "127.0.0.1",
		Port:       0, // OS-assigned ephemeral port for the test
		Dispatcher: disp,
		Auth:       auth,
		Assets:     testAssetsFS(),
		CertDir:    t.TempDir(),
		Events:     []string{"status", "sender", "return", "error", "statusKeyCandidates", "levels"},
		Logf:       t.Logf,
	})
	// Shorten the per-write timeout so a test that deliberately stalls the
	// writer sees it die in a fraction of a second rather than the 10 s
	// production writeWait — the same reason auth.minDelay is shortened above.
	// Set BEFORE Start, so no session ever reads it concurrently with a write.
	// 200 ms is still orders of magnitude longer than a healthy draining
	// client's write, so it changes nothing for every other test.
	srv.writeDeadline = 200 * time.Millisecond
	addr, err := srv.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if addr == "" {
		t.Fatal("Start returned empty address for an enabled server")
	}
	// One shared HTTP client so the login keep-alive pool is a single connection
	// the test can drain, rather than a fresh transport (and a fresh pair of pool
	// goroutines) per call that would masquerade as a leak.
	hc := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	t.Cleanup(func() {
		hc.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Close(ctx)
	})
	return &harness{srv: srv, auth: auth, disp: disp, addr: addr, hc: hc}
}

func (h *harness) httpClient() *http.Client { return h.hc }

// login POSTs credentials and returns the resulting cookies and the HTTP
// status. It never fails the test itself so callers can assert on the status.
func (h *harness) login(t *testing.T, user, password string) ([]*http.Cookie, int, []byte) {
	t.Helper()
	body, _ := json.Marshal(loginRequest{User: user, Password: password})
	req, _ := http.NewRequest(http.MethodPost, "https://"+h.addr+loginPath, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.httpClient().Do(req)
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.Cookies(), resp.StatusCode, respBody
}

// dial opens the WebSocket with the given cookies and Origin. An empty origin
// means "the correct one" (https://<addr>).
func (h *harness) dial(t *testing.T, cookies []*http.Cookie, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	if origin == "" {
		origin = "https://" + h.addr
	}
	d := websocket.Dialer{
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: true},
		HandshakeTimeout: 5 * time.Second,
	}
	hdr := http.Header{}
	hdr.Set("Origin", origin)
	if len(cookies) > 0 {
		var parts []string
		for _, c := range cookies {
			parts = append(parts, c.Name+"="+c.Value)
		}
		hdr.Set("Cookie", strings.Join(parts, "; "))
	}
	return d.Dial("wss://"+h.addr+wsPath, hdr)
}

// connect logs in as user and opens an authenticated socket, reading and
// returning the hello frame. It fails the test on any error.
func (h *harness) connect(t *testing.T, user, password string) (*websocket.Conn, map[string]any) {
	t.Helper()
	cookies, status, _ := h.login(t, user, password)
	if status != http.StatusOK {
		t.Fatalf("login status = %d, want 200", status)
	}
	conn, _, err := h.dial(t, cookies, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	hello := readFrame(t, conn)
	if hello["t"] != FrameHello {
		t.Fatalf("first frame t = %v, want hello", hello["t"])
	}
	return conn, hello
}

// readFrame reads one JSON frame with a deadline.
func readFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode frame %q: %v", data, err)
	}
	return m
}

// rpc sends a call and returns the matching result frame, skipping any events
// that arrive in between.
func rpc(t *testing.T, conn *websocket.Conn, id uint64, method string, args ...any) map[string]any {
	t.Helper()
	raw := make([]json.RawMessage, len(args))
	for i, a := range args {
		b, _ := json.Marshal(a)
		raw[i] = b
	}
	call, _ := json.Marshal(CallFrame{T: FrameCall, ID: id, Method: method, Args: raw})
	if err := conn.WriteMessage(websocket.TextMessage, call); err != nil {
		t.Fatalf("write call: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_ = conn.SetReadDeadline(deadline)
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read result: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		if m["t"] == FrameResult && uint64(m["id"].(float64)) == id {
			return m
		}
	}
}

// ---------------------------------------------------------------------------
// Test 3: off by default, loopback by default, no open unauthenticated listener
// ---------------------------------------------------------------------------

func TestStart_DisabledBindsNoSocket(t *testing.T) {
	// This is the test that protects the operator from the plan itself: "off by
	// default" is a property with a test, not a comment. With Enabled false —
	// which is exactly what a missing remote.json yields via DefaultSettings —
	// Start must bind nothing at all.
	srv := NewServer(Options{
		Enabled:    false,
		Bind:       "127.0.0.1",
		Port:       8443,
		Dispatcher: newFakeDispatcher(),
		Auth:       NewAuthenticator(nil),
		Assets:     testAssetsFS(),
		CertDir:    t.TempDir(),
	})
	addr, err := srv.Start()
	if err != nil {
		t.Fatalf("Start(disabled) error = %v, want nil", err)
	}
	if addr != "" {
		t.Fatalf("Start(disabled) addr = %q, want empty", addr)
	}
	if srv.ln != nil || srv.httpSrv != nil {
		t.Fatal("Start(disabled) opened a listener")
	}
}

func TestDefaultSettings_OffAndLoopback(t *testing.T) {
	// The defaults themselves are the safe posture, so a missing file cannot be
	// an accidentally-listening one.
	d := DefaultSettings()
	if d.Enabled {
		t.Error("DefaultSettings enabled; want disabled")
	}
	if d.Bind != "127.0.0.1" {
		t.Errorf("DefaultSettings bind = %q, want 127.0.0.1", d.Bind)
	}
}

func TestStart_EnabledLoopbackIsNotWildcard(t *testing.T) {
	h := newHarness(t, nil)
	if !strings.HasPrefix(h.addr, "127.0.0.1:") {
		t.Fatalf("bound addr = %q, want a 127.0.0.1 address", h.addr)
	}
	if strings.HasPrefix(h.addr, "0.0.0.0") {
		t.Fatalf("bound addr = %q is a wildcard bind", h.addr)
	}
}

func TestStart_NonLoopbackWithNoClientsRefused(t *testing.T) {
	// A reachable listener nobody can authenticate to is pure attack surface,
	// so it is refused before any socket is opened.
	srv := NewServer(Options{
		Enabled:    true,
		Bind:       "192.0.2.10", // TEST-NET-1, non-loopback
		Port:       8443,
		Dispatcher: newFakeDispatcher(),
		Auth:       NewAuthenticator(nil), // zero clients
		Assets:     testAssetsFS(),
		CertDir:    t.TempDir(),
	})
	addr, err := srv.Start()
	if err == nil {
		t.Fatalf("Start(non-loopback, no clients) succeeded with addr %q, want refusal", addr)
	}
	if srv.ln != nil {
		t.Fatal("Start refused but still opened a listener")
	}
}

// ---------------------------------------------------------------------------
// Test 7: fan-out reaches every client; a stalled client never blocks it
// ---------------------------------------------------------------------------

func TestBroadcast_ReachesEveryClient(t *testing.T) {
	h := newHarness(t, []Client{testClient(t, "op", "s3cret", string(CapView))})
	c1, _ := h.connect(t, "op", "s3cret")
	c2, _ := h.connect(t, "op", "s3cret")

	h.srv.Broadcast("status", map[string]any{"n": 42})

	for i, c := range []*websocket.Conn{c1, c2} {
		f := readFrame(t, c)
		if f["t"] != FrameEvent || f["name"] != "status" {
			t.Fatalf("client %d got frame %v, want a status event", i, f)
		}
		data := f["data"].(map[string]any)
		if data["n"].(float64) != 42 {
			t.Fatalf("client %d event data = %v", i, data)
		}
	}
}

func TestBroadcast_NeverBlocksOnAStalledClient(t *testing.T) {
	h := newHarness(t, []Client{testClient(t, "op", "s3cret", string(CapView))})
	drainer, _ := h.connect(t, "op", "s3cret")
	// staller connects and NEVER reads; its per-session queue must fill and drop
	// oldest rather than back-pressure the broadcast.
	staller, _ := h.connect(t, "op", "s3cret")
	_ = staller

	// Fan out far more events than any queue depth; each Broadcast must return
	// promptly regardless of the staller.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ {
			h.srv.Broadcast("levels", map[string]any{"i": i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Broadcast blocked with a stalled client present")
	}

	// The draining client still receives events (with seq gaps, which is the
	// honest signal a drop happened) — proving the fan-out kept working.
	got := readFrame(t, drainer)
	if got["t"] != FrameEvent || got["name"] != "levels" {
		t.Fatalf("draining client got %v, want a levels event", got)
	}
	if _, ok := got["seq"]; !ok {
		t.Fatal("event frame carried no seq")
	}
}

// TestEnqueueEvent_DropsOldestNeverBlocks drives the drop-oldest discipline
// directly, with no socket, so the bound and the monotonic seq are asserted
// without depending on OS buffer sizes or write deadlines. enqueueEvent touches
// only the queue and the seq counter, so a bare session literal exercises it.
func TestEnqueueEvent_DropsOldestNeverBlocks(t *testing.T) {
	s := &session{eventsCh: make(chan []byte, 4)}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			s.enqueueEvent("levels", map[string]any{"i": i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueueEvent blocked when the queue was never drained")
	}
	if len(s.eventsCh) > cap(s.eventsCh) {
		t.Fatalf("queue overflowed its cap: len %d cap %d", len(s.eventsCh), cap(s.eventsCh))
	}
	if s.seq != 1000 {
		t.Fatalf("seq = %d after 1000 events, want 1000 (monotonic)", s.seq)
	}
	// The retained frames must be the NEWEST, proving oldest was discarded: the
	// last frame in the channel should carry the highest i seen.
	var last map[string]any
	for len(s.eventsCh) > 0 {
		var ef EventFrame
		_ = json.Unmarshal(<-s.eventsCh, &ef)
		last = ef.Data.(map[string]any)
	}
	if last == nil || last["i"].(float64) < 900 {
		t.Fatalf("retained frames are not the newest: %v", last)
	}
}

// ---------------------------------------------------------------------------
// Test 8: disconnect mid-call cancels the call context and leaks no goroutine
// ---------------------------------------------------------------------------

func TestDisconnect_CancelsInFlightCallAndLeaksNoGoroutine(t *testing.T) {
	h := newHarness(t, []Client{testClient(t, "op", "s3cret", string(CapMixer))})

	// First, prove the cancellation itself: a disconnect mid-call cancels the
	// call's context. This is the single highest-consequence property — the call
	// goroutine must not outlive the socket.
	conn, _ := h.connect(t, "op", "s3cret")
	call, _ := json.Marshal(CallFrame{T: FrameCall, ID: 1, Method: "SlowCall"})
	if err := conn.WriteMessage(websocket.TextMessage, call); err != nil {
		t.Fatalf("write SlowCall: %v", err)
	}
	select {
	case <-h.disp.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatcher never entered SlowCall")
	}
	_ = conn.Close() // slam the socket shut mid-call
	select {
	case <-h.disp.cancelledCh:
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight call context was not cancelled on disconnect")
	}

	// Now prove NO GOROUTINE LEAK, by amplification: a per-connection leak of the
	// pumps or the dispatch goroutine would grow the count by ~3 per iteration,
	// so many connect/park/disconnect cycles would leave dozens of stragglers.
	// A stable count after them proves each connection was fully reaped. This is
	// robust to the couple of transient runtime/HTTP-pool goroutines an absolute
	// baseline would trip over.
	base := runtime.NumGoroutine()
	const cycles = 25
	for i := 0; i < cycles; i++ {
		c, _ := h.connect(t, "op", "s3cret")
		cf, _ := json.Marshal(CallFrame{T: FrameCall, ID: 1, Method: "SlowCall"})
		_ = c.WriteMessage(websocket.TextMessage, cf)
		select {
		case <-h.disp.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("dispatcher never entered SlowCall")
		}
		_ = c.Close()
		select {
		case <-h.disp.cancelledCh:
		case <-time.After(3 * time.Second):
			t.Fatal("call context not cancelled")
		}
	}
	h.hc.CloseIdleConnections()

	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		now := runtime.NumGoroutine()
		// Slack well below one leaked connection's worth (~3), so a real
		// per-connection leak over 25 cycles cannot hide under it.
		if now <= base+5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines leaked over %d cycles: base %d, now %d", cycles, base, now)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestWritePumpDeathReapsAStalledFloodingClient is the regression test for the
// hostile-client goroutine leak found in review.
//
// THE BUG: writePump returned on a write failure WITHOUT tearing the session
// down. A client authenticated at ANY capability — even view — that stops
// reading and floods calls past the in-flight cap strands the read pump and up
// to maxInFlightCalls dispatch goroutines forever: the writer blocks on the
// full socket, the results queue fills, and the over-cap SYNCHRONOUS
// enqueueResult on the read goroutine parks on a `done` channel that a dead
// writer never closes. The session never leaves the registry — a permanent
// goroutine + memory leak and a phantom seat on the operator's indicator, on
// the machine that is on air. The fix is writePump's `defer s.close()`.
//
// The test drives the exact path: a real authenticated socket, reading stopped,
// flooded past the cap with BigCall (a payload large enough that a single
// result overflows the socket buffers). The client is left CONNECTED — it is
// not closed. So there is NO read-path rescue: the only way the session can be
// reaped is the write pump dying on its own (the shortened write deadline, set
// by newHarness) and tearing the session down. WITHOUT the fix, writePump
// returns without close(), the read pump stays parked in enqueueResult, and the
// session never leaves Clients() — this fails at the deadline. WITH the fix it
// is reaped shortly after the write deadline fires.
//
// (An earlier version closed the client socket to speed things up, and that
// masked the bug: closing the socket reaps the session via the READ path
// regardless of the writePump fix, so it passed even when broken. Leaving the
// client connected is the whole point.)
func TestWritePumpDeathReapsAStalledFloodingClient(t *testing.T) {
	h := newHarness(t, []Client{testClient(t, "op", "s3cret", string(CapView))})
	conn, _ := h.connect(t, "op", "s3cret")

	// Stop reading and flood past the cap FROM A GOROUTINE, so the main
	// goroutine can start polling immediately. maxInFlightCalls BigCall results
	// (512 KB each — 16 MB) overflow the socket, so the writer blocks; the
	// results queue fills; the over-cap calls reach the synchronous
	// enqueueResult and park the read pump. Nothing here closes the socket, so
	// there is no read-path rescue — only the write deadline can reap it.
	//
	// The flood is on its own goroutine because the CLIENT's own writes block
	// once the server stops reading, and on Windows a blocked write is slow to
	// error even after the server closes the socket; polling on this goroutine
	// would otherwise not begin until that unblocked, masking a fast reap.
	// gorilla permits Close concurrently with a blocked WriteMessage.
	const flood = maxInFlightCalls * 2
	go func() {
		for i := 0; i < flood; i++ {
			cf, _ := json.Marshal(CallFrame{T: FrameCall, ID: uint64(i + 1), Method: "BigCall"})
			if err := conn.WriteMessage(websocket.TextMessage, cf); err != nil {
				return // our send buffer is full or the session is gone; enough are in flight
			}
		}
	}()
	defer conn.Close()

	// The writer dies on the ~200 ms write deadline (newHarness); the fix's
	// defer close() then unparks the read pump and the session leaves the
	// registry within a moment. Without the fix it never does.
	// Reap time is a fixed Windows-loopback stall (~5 s) plus the shortened
	// write deadline; 12 s is generous margin over that, and it is only ever
	// reached when the fix is ABSENT (then this fails, as intended).
	deadline := time.Now().Add(12 * time.Second)
	for {
		if len(h.srv.Clients()) == 0 {
			return // reaped: writePump's own teardown unparked the read pump
		}
		if time.Now().After(deadline) {
			t.Fatalf("a stalled flooding client was not reaped: %d session(s) still registered — "+
				"writePump must tear the session down when its write fails", len(h.srv.Clients()))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestClose_IsBounded proves Close returns within its budget even with a live,
// parked call — the property that keeps a stray http.Server from wedging the
// app's shutdown.
func TestClose_IsBounded(t *testing.T) {
	h := newHarness(t, []Client{testClient(t, "op", "s3cret", string(CapMixer))})
	conn, _ := h.connect(t, "op", "s3cret")
	call, _ := json.Marshal(CallFrame{T: FrameCall, ID: 1, Method: "SlowCall"})
	_ = conn.WriteMessage(websocket.TextMessage, call)
	select {
	case <-h.disp.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatcher never entered SlowCall")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := h.srv.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close took %v, exceeding its budget", elapsed)
	}
}
