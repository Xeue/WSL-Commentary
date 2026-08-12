package remote

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// This file is the LAN control bridge's transport. By the owner's explicit,
// repeated, final decision it is FULLY OPEN: it binds all interfaces, serves
// BOTH plain HTTP and HTTPS, and upgrades ANY WebSocket connection with NO
// login, NO cookie, NO origin/CSRF check and NO guard of any kind. The app runs
// in a secure remote facility on a dedicated private network, and the network is
// the access control. The risk write-up is docs/remote-access.md; do not add a
// connection guard here that the owner did not ask for.
//
// (For contrast: Wails' own dev server also sets CheckOrigin to `return true`,
// but additionally has no TLS and an unauthenticated GET /wails/reload. This
// bridge dispatches through a hand-written allowlist and host-only refusals in
// app_remote.go, which is what keeps the reachable surface reviewable even with
// the connection wide open.)

const (
	// wsPath is the WebSocket upgrade endpoint. It is upgraded unconditionally.
	wsPath = "/__wslremote/ws"
	// shimPath serves the transport shim, byte-for-byte, ahead of the bundle.
	shimPath = "/__wslremote/shim.js"
)

// ClientInfo identifies one connected seat to the dispatcher. It is the only
// thing about a caller the application-side policy gets to see, and it is
// deliberately minimal: a stable per-connection ID (so arm-ownership can name
// WHO armed the mixer) and the source address for the audit log and the
// connected-seat indicator. There is no name and no capability set — the
// listener is unauthenticated, so a connection has no identity beyond where it
// dialled from.
type ClientInfo struct {
	// ID is unique for the lifetime of this connection. The local WebView2 seat
	// is given its own fixed ID by the application so "which client armed" has a
	// well-defined answer even when the only client is local.
	ID string
	// RemoteAddr is the TCP peer address, host:port, for logging and the seat
	// indicator.
	RemoteAddr string
}

// Dispatcher is the seam that keeps this package App-agnostic. The application
// implements it (app_remote.go) with a hand-written ALLOWLIST switch; this
// package calls it and knows nothing else about *App.
//
// Call invokes method with raw JSON args on behalf of client and returns the
// value to marshal back or an error to relay. It is where the allowlist and the
// host-only refusals are enforced — NOT here — because a reflective dispatcher
// would automatically expose the next method someone binds to Wails, and "is
// this method safe to expose over a network" must be answered by a hand-written
// table, not by reflection.
//
// Methods returns the authoritative list of method names client may see, which
// becomes the hello frame's methods list and, through it, exactly the functions
// the shim installs on window.go.main.App. A host-only method is absent from
// this list AND refused by Call — omission is the honest UI path, refusal is the
// defence against a crafted client.
type Dispatcher interface {
	Call(ctx context.Context, client ClientInfo, method string, args []json.RawMessage) (any, error)
	Methods(client ClientInfo) []string
}

// Options configures a Server. Enabled/Bind/HTTPPort/HTTPSPort come from
// remote.json; Assets is the SAME embed.FS subtree the Wails asset server uses
// (the caller does the fs.Sub to strip the frontend/dist prefix); CertDir is
// where the self-signed cert lives; Events is the event-name list advertised in
// the hello frame.
type Options struct {
	Enabled    bool
	Bind       string
	HTTPPort   int
	HTTPSPort  int
	Dispatcher Dispatcher
	Assets     fs.FS
	CertDir    string
	Events     []string
	Logf       func(string, ...any)
}

// Server is the listener: ONE http.Server serving the same handler over TWO
// sockets — a plain-HTTP listener and a TLS listener — whose only routes are the
// WebSocket, the shim, and the static frontend.
type Server struct {
	opts     Options
	logf     func(string, ...any)
	upgrader websocket.Upgrader

	// httpSrv serves both listeners; its Shutdown closes both, so the listeners
	// are not held as fields — the addresses and ports below are what readers need.
	httpSrv   *http.Server
	httpAddr  string
	httpsAddr string
	httpPort  int // actual bound HTTP port (may be a fallback)
	httpsPort int // actual bound HTTPS port (may be a fallback)
	fp        string

	// httpFallback / httpsFallback are the ports Start drops to when the primary
	// is busy. They default to the package fallback consts in NewServer and are
	// overridable before Start ONLY by tests, which need a deterministic (often
	// ephemeral) fallback rather than the fixed 8080/8443.
	httpFallback  int
	httpsFallback int

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	sessions map[string]*session
	connWG   sync.WaitGroup

	// writeDeadline, when non-zero, overrides a session's per-write timeout.
	// Test-only, set before Start so no session ever reads it concurrently with
	// a write; production leaves it zero and sessions use the writeWait const.
	writeDeadline time.Duration
}

// NewServer builds a server from opts. It does not bind anything; Start does.
func NewServer(opts Options) *Server {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		opts:          opts,
		logf:          logf,
		ctx:           ctx,
		cancel:        cancel,
		sessions:      make(map[string]*session),
		httpFallback:  httpFallbackPort,
		httpsFallback: httpsFallbackPort,
	}
	s.upgrader = websocket.Upgrader{
		// FULLY OPEN by the owner's decision: accept every origin. There is no
		// same-origin or CSRF check anywhere in this bridge — the private network
		// is the access control. See the file header and docs/remote-access.md.
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	return s
}

// Start binds both listeners and begins serving. It returns nil and binds
// NOTHING when the server is disabled, so a remote.json with enabled=false is a
// clean no-op rather than an error.
//
// Both listeners are bound on the configured Bind. Each prefers its primary port
// and, if that is busy — on Windows 80 and 443 are frequently held by http.sys —
// drops to its fallback. Port 0 on either means "let the OS assign one" and is
// used by tests; a busy primary of 0 cannot happen. Both sockets serve the same
// http.Handler; the HTTPS one is TLS-wrapped with the self-signed cert.
func (s *Server) Start() error {
	if !s.opts.Enabled {
		return nil
	}
	if s.opts.Dispatcher == nil {
		return errors.New("remote: Start requires a Dispatcher")
	}

	ip := net.ParseIP(strings.TrimSpace(s.opts.Bind))
	if ip == nil {
		return fmt.Errorf("remote: bind %q is not a literal IP address", s.opts.Bind)
	}
	if err := validatePort("httpPort", s.opts.HTTPPort); err != nil {
		return err
	}
	if err := validatePort("httpsPort", s.opts.HTTPSPort); err != nil {
		return err
	}

	cert, fp, err := EnsureCertificate(s.opts.CertDir, ip.String())
	if err != nil {
		return err
	}
	s.fp = fp

	// Bind HTTP first. If it cannot bind at all (both primary and fallback busy),
	// fail the whole Start rather than serve HTTPS-only: the owner asked for both.
	httpLn, httpPort, err := listenWithFallback(s.logf, "http", ip, s.opts.HTTPPort, s.httpFallback)
	if err != nil {
		return err
	}
	// Then HTTPS. If it fails, close the HTTP listener we just took so a failed
	// Start leaves no half-open socket behind.
	httpsRaw, httpsPort, err := listenWithFallback(s.logf, "https", ip, s.opts.HTTPSPort, s.httpsFallback)
	if err != nil {
		httpLn.Close()
		return err
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	httpsLn := tls.NewListener(httpsRaw, tlsCfg)

	s.httpAddr = httpLn.Addr().String()
	s.httpsAddr = httpsRaw.Addr().String()
	s.httpPort = httpPort
	s.httpsPort = httpsPort

	mux := http.NewServeMux()
	mux.HandleFunc(wsPath, s.handleWS)
	// The shim and every static asset go through the asset server, which injects
	// the shim tag into index.html and serves the shim itself from this package's
	// embed. Registered on "/", the catch-all, so the exact /__wslremote/ws route
	// above takes precedence.
	mux.Handle("/", newAssetServer(s.opts.Assets, s.logf))

	s.httpSrv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          nil,
	}

	// One http.Server serves both listeners, each on its own goroutine. Shutdown
	// closes both. Serve returns ErrServerClosed on a clean Shutdown, which is not
	// an error worth logging.
	serve := func(ln net.Listener, scheme string) {
		if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logf("remote: %s serve stopped: %v", scheme, err)
		}
	}
	go serve(httpLn, "http")
	go serve(httpsLn, "https")

	s.logf("remote: listening (UNAUTHENTICATED, all origins accepted) on http://%s and https://%s (cert %s)",
		s.httpAddr, s.httpsAddr, s.fp)
	return nil
}

// listenWithFallback binds ip:primary, and on failure ip:fallback, returning the
// listener and the port actually bound. A primary of 0 is an OS-assigned port
// that cannot be "busy", so no fallback is attempted for it. When both fail the
// error names both ports and both underlying reasons, so a total failure is
// diagnosable.
func listenWithFallback(logf func(string, ...any), scheme string, ip net.IP, primary, fallback int) (net.Listener, int, error) {
	addr := net.JoinHostPort(ip.String(), fmt.Sprintf("%d", primary))
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, portOf(ln), nil
	}
	// An OS-assigned primary (0) that failed is a real failure, not a busy
	// well-known port; there is no fallback to try.
	if primary == 0 || fallback == primary {
		return nil, 0, fmt.Errorf("remote: binding %s %s: %w", scheme, addr, err)
	}
	faddr := net.JoinHostPort(ip.String(), fmt.Sprintf("%d", fallback))
	fln, ferr := net.Listen("tcp", faddr)
	if ferr != nil {
		return nil, 0, fmt.Errorf("remote: binding %s: neither %s (%v) nor fallback %s (%v) is available",
			scheme, addr, err, faddr, ferr)
	}
	logf("remote: %s port %d is busy (often http.sys on Windows); using fallback %d instead",
		scheme, primary, portOf(fln))
	return fln, portOf(fln), nil
}

// portOf extracts the integer port from a listener's address.
func portOf(ln net.Listener) int {
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		return tcp.Port
	}
	return 0
}

// Fingerprint returns the served certificate's SHA-256 fingerprint, for the
// Settings display. Empty before Start.
func (s *Server) Fingerprint() string { return s.fp }

// Running reports whether Start bound the listeners (enabled and successful).
func (s *Server) Running() bool { return s.httpSrv != nil }

// HTTPAddr / HTTPSAddr are the bound host:port of each listener, empty before a
// successful Start. HTTPURL / HTTPSURL wrap them with the scheme for display.
func (s *Server) HTTPAddr() string  { return s.httpAddr }
func (s *Server) HTTPSAddr() string { return s.httpsAddr }

func (s *Server) HTTPURL() string {
	if s.httpAddr == "" {
		return ""
	}
	return "http://" + s.httpAddr
}

func (s *Server) HTTPSURL() string {
	if s.httpsAddr == "" {
		return ""
	}
	return "https://" + s.httpsAddr
}

// HTTPPort / HTTPSPort are the ports actually bound, which differ from the
// configured ports when a fallback was used.
func (s *Server) HTTPPort() int  { return s.httpPort }
func (s *Server) HTTPSPort() int { return s.httpsPort }

// handleWS upgrades one connection — unconditionally — and runs its session.
//
// There is NO origin check and NO cookie: by the owner's decision the listener
// is unauthenticated (see the file header). After upgrade the hello frame is
// written before the session is registered for broadcast, so no event can race
// ahead of the method list the shim needs.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written an error response; nothing more to do.
		return
	}

	info := ClientInfo{
		ID:         newConnID(),
		RemoteAddr: r.RemoteAddr,
	}
	sess := newSession(s.ctx, conn, info, s.opts.Dispatcher, s.opts.Events, s.logf)
	if s.writeDeadline > 0 {
		sess.writeDeadline = s.writeDeadline
	}

	if err := sess.writeHello(); err != nil {
		sess.close()
		return
	}

	s.mu.Lock()
	s.sessions[info.ID] = sess
	s.mu.Unlock()
	s.connWG.Add(1)

	defer func() {
		s.mu.Lock()
		delete(s.sessions, info.ID)
		s.mu.Unlock()
		s.connWG.Done()
	}()

	s.logf("remote: seat %s connected from %s", info.ID, info.RemoteAddr)
	sess.serve()
	s.logf("remote: seat %s disconnected from %s", info.ID, info.RemoteAddr)
}

// Broadcast fans an event out to every connected session, and MUST NEVER BLOCK.
// It snapshots the session set under the lock and enqueues outside it; each
// enqueue is the non-blocking, drop-oldest path, so one stalled client can
// neither block the broadcast nor the others. This is the remote mirror of
// app.go's eventPump tee.
func (s *Server) Broadcast(name string, data any) {
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		sess.enqueueEvent(name, data)
	}
}

// Clients returns a snapshot of the connected seats, for the home-screen
// indicator that lets the operator at the desk see that someone else has a seat.
func (s *Server) Clients() []ClientInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ClientInfo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess.info)
	}
	return out
}

// Close shuts BOTH listeners down within ctx's budget and leaves no goroutine
// behind — a stray http.Server is a process that will not exit, which the app's
// teardown ordering is meticulous about avoiding.
//
// The sequence: cancel the server context (which cancels every in-flight call),
// close every session (which closes its socket and unblocks its pumps), wait for
// the connection goroutines to drain bounded by ctx, and finally Shutdown the
// http.Server, which stops accepting and closes both listeners. Because an
// upgraded WebSocket is hijacked and http.Server does not track it, the explicit
// session close and connWG wait — not Shutdown — are what actually reap the
// connections.
func (s *Server) Close(ctx context.Context) error {
	if s.httpSrv == nil {
		// Never started, or started disabled: nothing to tear down.
		return nil
	}
	s.cancel()

	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		sess.close()
	}

	drained := make(chan struct{})
	go func() {
		s.connWG.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-ctx.Done():
		// Budget spent: fall through to Shutdown, which closes both listeners
		// regardless, so we do not hang teardown on a client that will not let
		// go. The connection goroutines are already cancelled and closed; this
		// only bounds the WAIT, not the work.
	}

	// Shutdown closes every listener the http.Server is serving — both of ours.
	return s.httpSrv.Shutdown(ctx)
}

// newConnID returns a short random per-connection identifier. It need not be
// cryptographically unguessable — it never leaves the process as a credential —
// but crypto/rand gives collision-free ids for free, so there is no reason to
// use anything weaker.
func newConnID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// Randomness failing here is not worth failing a connection over: fall
		// back to a time-based id, which is still unique in practice for the
		// lifetime of a process.
		return fmt.Sprintf("conn-%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
