package remote

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"runtime"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// Shared test harness
//
// Every transport test drives a REAL server over REAL sockets with a REAL
// gorilla client, because the properties being protected — that ANY connection
// upgrades with no guard, the fan-out discipline, the mid-call cancellation, the
// dual-listener bind and fallback — are properties of the wiring, not of any one
// function, and a test that mocked the wiring would not catch a regression in
// it. The dispatcher is a fake so the package never imports the root or
// GStreamer and the whole suite runs at Gate A with CGO_ENABLED=0.
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

type harness struct {
	srv       *Server
	disp      *fakeDispatcher
	httpsAddr string // the TLS listener's bound host:port
	httpAddr  string // the plain-HTTP listener's bound host:port
	hc        *http.Client
}

// newHarness starts a loopback server with BOTH listeners on OS-assigned
// ephemeral ports and returns a harness. There is no auth to configure — the
// listener is unauthenticated by design. It registers cleanup.
func newHarness(t *testing.T) *harness {
	t.Helper()
	disp := newFakeDispatcher()
	srv := NewServer(Options{
		Enabled:    true,
		Bind:       "127.0.0.1",
		HTTPPort:   0, // OS-assigned ephemeral port for the test
		HTTPSPort:  0,
		Dispatcher: disp,
		Assets:     testAssetsFS(),
		CertDir:    t.TempDir(),
		Events:     []string{"status", "sender", "return", "error", "statusKeyCandidates", "levels"},
		Logf:       t.Logf,
	})
	// Shorten the per-write timeout so a test that deliberately stalls the writer
	// sees it die in a fraction of a second rather than the 10 s production
	// writeWait. Set BEFORE Start, so no session ever reads it concurrently with a
	// write. 200 ms is still orders of magnitude longer than a healthy draining
	// client's write, so it changes nothing for every other test.
	srv.writeDeadline = 200 * time.Millisecond
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if srv.HTTPSAddr() == "" || srv.HTTPAddr() == "" {
		t.Fatalf("Start bound incompletely: http %q https %q", srv.HTTPAddr(), srv.HTTPSAddr())
	}
	hc := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	t.Cleanup(func() {
		hc.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Close(ctx)
	})
	return &harness{srv: srv, disp: disp, httpsAddr: srv.HTTPSAddr(), httpAddr: srv.HTTPAddr(), hc: hc}
}

func (h *harness) httpClient() *http.Client { return h.hc }

// dial opens the WebSocket over TLS with the given Origin. An empty origin means
// "the matching one" (https://<addr>); a non-empty one is sent verbatim — the
// point of several tests being that ANY origin is accepted. There is no cookie:
// the listener is unauthenticated.
func (h *harness) dial(t *testing.T, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	d := websocket.Dialer{
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: true},
		HandshakeTimeout: 5 * time.Second,
	}
	hdr := http.Header{}
	if origin == "" {
		origin = "https://" + h.httpsAddr
	}
	hdr.Set("Origin", origin)
	return d.Dial("wss://"+h.httpsAddr+wsPath, hdr)
}

// connect opens a socket and reads the hello frame, failing the test on any
// error. No login, no cookie — just connect.
func (h *harness) connect(t *testing.T) (*websocket.Conn, map[string]any) {
	t.Helper()
	conn, _, err := h.dial(t, "")
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
// The open posture: ON by default, wildcard by default
// ---------------------------------------------------------------------------

func TestDefaultSettings_OnAndWildcard(t *testing.T) {
	// The defaults ARE the posture the owner asked for: on, all interfaces. A
	// missing remote.json yields exactly this, so a fresh machine is listening.
	d := DefaultSettings()
	if !d.Enabled {
		t.Error("DefaultSettings is disabled; the owner's decision is ON by default")
	}
	if d.Bind != "0.0.0.0" {
		t.Errorf("DefaultSettings bind = %q, want 0.0.0.0 (all interfaces)", d.Bind)
	}
	if d.HTTPPort != 80 || d.HTTPSPort != 443 {
		t.Errorf("DefaultSettings ports = %d/%d, want 80/443", d.HTTPPort, d.HTTPSPort)
	}
}

func TestStart_DisabledBindsNothing(t *testing.T) {
	// Enabled false is still honoured: it binds nothing and is a clean no-op.
	srv := NewServer(Options{
		Enabled:    false,
		Bind:       "0.0.0.0",
		HTTPPort:   80,
		HTTPSPort:  443,
		Dispatcher: newFakeDispatcher(),
		Assets:     testAssetsFS(),
		CertDir:    t.TempDir(),
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start(disabled) error = %v, want nil", err)
	}
	if srv.Running() || srv.HTTPAddr() != "" || srv.HTTPSAddr() != "" {
		t.Fatalf("Start(disabled) bound something: running=%v http=%q https=%q",
			srv.Running(), srv.HTTPAddr(), srv.HTTPSAddr())
	}
}

// ---------------------------------------------------------------------------
// Start binds BOTH listeners, and both serve
// ---------------------------------------------------------------------------

func TestStart_BindsBothHTTPAndHTTPS(t *testing.T) {
	h := newHarness(t)

	if h.httpAddr == "" || h.httpsAddr == "" {
		t.Fatalf("expected both listeners bound: http %q https %q", h.httpAddr, h.httpsAddr)
	}
	if h.httpAddr == h.httpsAddr {
		t.Fatalf("both listeners share an address %q; they must be distinct sockets", h.httpAddr)
	}

	// The plain-HTTP listener serves the injected index over http://.
	plain := &http.Client{}
	resp, err := plain.Get("http://" + h.httpAddr + "/")
	if err != nil {
		t.Fatalf("GET http:// index: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("http:// index status = %d, want 200", resp.StatusCode)
	}

	// The TLS listener serves it over https://.
	resp2, err := h.httpClient().Get("https://" + h.httpsAddr + "/")
	if err != nil {
		t.Fatalf("GET https:// index: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("https:// index status = %d, want 200", resp2.StatusCode)
	}
}

func TestWS_OverPlainHTTPSucceeds(t *testing.T) {
	// A ws:// upgrade over the plain-HTTP listener works too, with no guard.
	h := newHarness(t)
	d := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := d.Dial("ws://"+h.httpAddr+wsPath, http.Header{})
	if err != nil {
		t.Fatalf("plain ws:// upgrade failed: %v", err)
	}
	defer conn.Close()
	hello := readFrame(t, conn)
	if hello["t"] != FrameHello {
		t.Fatalf("first frame = %v, want hello", hello["t"])
	}
}

// ---------------------------------------------------------------------------
// ANY connection upgrades: no login, no cookie, no origin/CSRF guard
// ---------------------------------------------------------------------------

func TestWS_AnyOriginUpgradesWithNoGuard(t *testing.T) {
	h := newHarness(t)

	// A cross-origin Origin header — exactly the request a same-origin check
	// would refuse — is accepted, because there is deliberately no such check.
	conn, _, err := h.dial(t, "https://evil.example")
	if err != nil {
		t.Fatalf("cross-origin upgrade was refused; the listener must accept any origin: %v", err)
	}
	hello := readFrame(t, conn)
	if hello["t"] != FrameHello {
		t.Fatalf("cross-origin first frame = %v, want hello", hello["t"])
	}
	conn.Close()

	// A request with NO Origin header at all (a non-browser client) is accepted.
	d := websocket.Dialer{
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: true},
		HandshakeTimeout: 5 * time.Second,
	}
	conn2, _, err := d.Dial("wss://"+h.httpsAddr+wsPath, http.Header{}) // no Origin, no Cookie
	if err != nil {
		t.Fatalf("no-origin upgrade was refused; the listener must accept it: %v", err)
	}
	defer conn2.Close()
	hello2 := readFrame(t, conn2)
	if hello2["t"] != FrameHello {
		t.Fatalf("no-origin first frame = %v, want hello", hello2["t"])
	}
}

// ---------------------------------------------------------------------------
// A busy primary port falls back to the secondary
// ---------------------------------------------------------------------------

func TestStart_FallsBackWhenPrimaryPortBusy(t *testing.T) {
	// Occupy a port, then hand it to Start as the HTTP primary. Start must find it
	// busy and drop to the fallback. The fallback is set to 0 (OS-assigned) for
	// determinism, so the assertion is simply "the bound port is neither the busy
	// one nor zero" — proof the fallback path ran.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupying a port: %v", err)
	}
	defer occupied.Close()
	busyPort := occupied.Addr().(*net.TCPAddr).Port

	srv := NewServer(Options{
		Enabled:    true,
		Bind:       "127.0.0.1",
		HTTPPort:   busyPort, // busy
		HTTPSPort:  0,        // ephemeral; not under test here
		Dispatcher: newFakeDispatcher(),
		Assets:     testAssetsFS(),
		CertDir:    t.TempDir(),
		Logf:       t.Logf,
	})
	srv.httpFallback = 0 // deterministic ephemeral fallback
	if err := srv.Start(); err != nil {
		t.Fatalf("Start with a busy primary should have used the fallback, got error: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Close(ctx)
	})

	if srv.HTTPPort() == busyPort {
		t.Fatalf("HTTP bound the busy primary %d; the fallback did not run", busyPort)
	}
	if srv.HTTPPort() == 0 || srv.HTTPAddr() == "" {
		t.Fatalf("HTTP did not bind a real fallback port: port=%d addr=%q", srv.HTTPPort(), srv.HTTPAddr())
	}
}

// ---------------------------------------------------------------------------
// Arm-ownership survives the transport: only the arming seat may write
// ---------------------------------------------------------------------------

func TestArmOwnership_SecondSeatCannotWriteAgainstTheFirstsArm(t *testing.T) {
	// Two connections, two distinct connection ids. Seat A arms; seat B's
	// SendMixerCommands is refused because it did not arm; seat A's is accepted.
	// This proves the transport hands the dispatcher a distinct id per connection
	// and threads it through Call, which is what arm-ownership rests on.
	h := newHarness(t)
	connA, _ := h.connect(t)
	defer connA.Close()
	connB, _ := h.connect(t)
	defer connB.Close()

	if res := rpc(t, connA, 1, "ArmMixer"); !res["ok"].(bool) {
		t.Fatalf("seat A ArmMixer refused: %v", res["error"])
	}

	resB := rpc(t, connB, 1, "SendMixerCommands")
	if ok, _ := resB["ok"].(bool); ok {
		t.Fatal("seat B's SendMixerCommands was accepted against seat A's arm")
	}
	if msg, _ := resB["error"].(string); !contains(msg, "another seat") {
		t.Fatalf("seat B refusal = %q, want an arm-ownership refusal", msg)
	}

	if res := rpc(t, connA, 2, "SendMixerCommands"); !res["ok"].(bool) {
		t.Fatalf("seat A's own SendMixerCommands was refused: %v", res["error"])
	}
}

// ---------------------------------------------------------------------------
// The served certificate names a non-loopback LAN address
// ---------------------------------------------------------------------------

func TestCert_SANsIncludeANonLoopbackIP(t *testing.T) {
	// The listener binds 0.0.0.0 and clients reach it by LAN IP, so the cert must
	// name at least one non-loopback interface address or every browser adds a
	// name-mismatch error. If this machine has no non-loopback interface (a
	// stripped CI container), there is nothing to assert.
	ifaceIPs := interfaceIPs()
	if len(ifaceIPs) == 0 {
		t.Skip("no non-loopback interface IP on this machine to assert against")
	}
	cert, _, err := EnsureCertificate(t.TempDir(), "0.0.0.0")
	if err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	var covered bool
	for _, ip := range ifaceIPs {
		if certCoversIP(leaf, ip.String()) {
			covered = true
			break
		}
	}
	if !covered {
		t.Errorf("cert SANs %v cover no non-loopback interface IP (%v)", leaf.IPAddresses, ifaceIPs)
	}
}

// ---------------------------------------------------------------------------
// Fan-out reaches every client; a stalled client never blocks it
// ---------------------------------------------------------------------------

func TestBroadcast_ReachesEveryClient(t *testing.T) {
	h := newHarness(t)
	c1, _ := h.connect(t)
	c2, _ := h.connect(t)

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
	h := newHarness(t)
	drainer, _ := h.connect(t)
	// staller connects and NEVER reads; its per-session queue must fill and drop
	// oldest rather than back-pressure the broadcast.
	staller, _ := h.connect(t)
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
// Disconnect mid-call cancels the call context and leaks no goroutine
// ---------------------------------------------------------------------------

func TestDisconnect_CancelsInFlightCallAndLeaksNoGoroutine(t *testing.T) {
	h := newHarness(t)

	// First, prove the cancellation itself: a disconnect mid-call cancels the
	// call's context. This is the single highest-consequence property — the call
	// goroutine must not outlive the socket.
	conn, _ := h.connect(t)
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
	// A stable count after them proves each connection was fully reaped.
	base := runtime.NumGoroutine()
	const cycles = 25
	for i := 0; i < cycles; i++ {
		c, _ := h.connect(t)
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
// down. A connection that stops reading and floods calls past the in-flight cap
// strands the read pump and up to maxInFlightCalls dispatch goroutines forever:
// the writer blocks on the full socket, the results queue fills, and the
// over-cap SYNCHRONOUS enqueueResult on the read goroutine parks on a `done`
// channel that a dead writer never closes. The session never leaves the registry
// — a permanent goroutine + memory leak and a phantom seat on the operator's
// indicator, on the machine that is on air. The fix is writePump's
// `defer s.close()`.
//
// The test drives the exact path: a real socket, reading stopped, flooded past
// the cap with BigCall (a payload large enough that a single result overflows
// the socket buffers). The client is left CONNECTED — it is not closed. So there
// is NO read-path rescue: the only way the session can be reaped is the write
// pump dying on its own (the shortened write deadline, set by newHarness) and
// tearing the session down.
func TestWritePumpDeathReapsAStalledFloodingClient(t *testing.T) {
	h := newHarness(t)
	conn, _ := h.connect(t)

	// Stop reading and flood past the cap FROM A GOROUTINE, so the main goroutine
	// can start polling immediately. maxInFlightCalls BigCall results (512 KB /
	// 1 MiB each) overflow the socket, so the writer blocks; the results queue
	// fills; the over-cap calls reach the synchronous enqueueResult and park the
	// read pump. Nothing here closes the socket, so there is no read-path rescue —
	// only the write deadline can reap it.
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

	// The writer dies on the ~200 ms write deadline (newHarness); the fix's defer
	// close() then unparks the read pump and the session leaves the registry
	// within a moment. Without the fix it never does. Reap time is a fixed
	// Windows-loopback stall (~5 s) plus the shortened write deadline; 12 s is
	// generous margin over that, and it is only ever reached when the fix is
	// ABSENT (then this fails, as intended).
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

// TestClose_StopsBothListeners proves Close shuts both sockets: after it returns,
// neither the HTTP nor the HTTPS address accepts a new connection.
func TestClose_StopsBothListeners(t *testing.T) {
	h := newHarness(t)
	httpAddr, httpsAddr := h.httpAddr, h.httpsAddr

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.srv.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A fresh TCP dial to each bound address must now fail (the listener is
	// closed). A short dial timeout keeps a stray accept from hanging the test.
	for _, addr := range []string{httpAddr, httpsAddr} {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			c.Close()
			t.Fatalf("address %s still accepts connections after Close", addr)
		}
	}
}

// TestClose_IsBounded proves Close returns within its budget even with a live,
// parked call — the property that keeps a stray http.Server from wedging the
// app's shutdown.
func TestClose_IsBounded(t *testing.T) {
	h := newHarness(t)
	conn, _ := h.connect(t)
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
