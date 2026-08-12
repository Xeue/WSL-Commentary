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

// ClientInfo identifies one connected seat to the dispatcher. It is the only
// thing about a caller the application-side policy gets to see, and it is
// deliberately minimal: a stable per-connection ID (so arm-ownership can name
// WHO armed the mixer), the authenticated client-record name, the granted
// capabilities, and the source address for the audit log. It carries no cookie,
// no token and no request — the transport keeps those to itself.
type ClientInfo struct {
	// ID is unique for the lifetime of this connection. The local WebView2 seat
	// is given its own fixed ID by the application so "which client armed" has a
	// well-defined answer even when the only client is local.
	ID string
	// Name is the authenticated client-record name (empty for the local seat,
	// which the application supplies directly).
	Name string
	// Caps are the capabilities the authenticated client holds.
	Caps []string
	// RemoteAddr is the TCP peer address, host:port, for logging.
	RemoteAddr string
}

// Dispatcher is the seam that keeps this package App-agnostic. The application
// implements it (app_remote.go) with a hand-written ALLOWLIST switch; this
// package calls it and knows nothing else about *App.
//
// Call invokes method with raw JSON args on behalf of client and returns the
// value to marshal back or an error to relay. It is where the allowlist,
// host-only refusals and capability tiers are enforced — NOT here — because a
// reflective dispatcher would automatically expose the next method someone binds
// to Wails, and "is this method safe to expose over a network" must be answered
// by a hand-written table, not by reflection.
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

// Options configures a Server. Enabled/Bind/Port come from remote.json; Auth is
// built from its client list; Assets is the SAME embed.FS subtree the Wails
// asset server uses (the caller does the fs.Sub to strip the frontend/dist
// prefix); CertDir is where the self-signed cert lives; Events is the event-name
// list advertised in the hello frame.
type Options struct {
	Enabled    bool
	Bind       string
	Port       int
	Dispatcher Dispatcher
	Auth       *Authenticator
	Assets     fs.FS
	CertDir    string
	Events     []string
	Logf       func(string, ...any)
}

// Server is the listener: a TLS http.Server whose only routes are the login
// endpoint, the authenticated WebSocket, the shim, and the static frontend.
type Server struct {
	opts     Options
	logf     func(string, ...any)
	upgrader websocket.Upgrader

	httpSrv *http.Server
	ln      net.Listener
	addr    string
	fp      string // served leaf's SHA-256 fingerprint, for the UI

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
		opts:     opts,
		logf:     logf,
		ctx:      ctx,
		cancel:   cancel,
		sessions: make(map[string]*session),
	}
	s.upgrader = websocket.Upgrader{
		// The explicit same-origin check is the primary gate (handleWS runs it
		// before touching the socket); wiring it here too is belt-and-braces so
		// the upgrade can never succeed with a mismatched Origin even if the
		// route wiring changes. A nil Auth (only in a degenerate test) falls
		// back to refusing every cross-origin upgrade.
		CheckOrigin: func(r *http.Request) bool {
			if opts.Auth == nil {
				return false
			}
			return opts.Auth.checkOrigin(r)
		},
	}
	return s
}

// Start binds the listener and begins serving, returning the bound address.
//
// The three properties this method must guarantee, each with a test:
//   - DISABLED BY DEFAULT: with Enabled false (which is what a missing
//     remote.json yields), it binds NOTHING and returns an empty address and a
//     nil error. Doing nothing is the safe posture and it is reachable by doing
//     nothing.
//   - LOOPBACK DEFAULT / no accidental exposure: bind must be a literal IP.
//   - NO OPEN LISTENER: a non-loopback bind with zero clients is refused before
//     a socket is opened, because a reachable listener nobody can authenticate
//     to is pure attack surface.
func (s *Server) Start() (string, error) {
	if !s.opts.Enabled {
		return "", nil
	}
	if s.opts.Dispatcher == nil || s.opts.Auth == nil {
		return "", errors.New("remote: Start requires a Dispatcher and an Auth")
	}

	ip := net.ParseIP(strings.TrimSpace(s.opts.Bind))
	if ip == nil {
		return "", fmt.Errorf("remote: bind %q is not a literal IP address", s.opts.Bind)
	}
	// Port 0 is permitted here and means "let the OS assign one" — it is how the
	// transport tests bind an ephemeral port. The operator-facing floor (a real,
	// findable port) is enforced one layer up in Settings.Validate, which rejects
	// 0; a production Start is always handed a validated settings port.
	if s.opts.Port < 0 || s.opts.Port > 65535 {
		return "", fmt.Errorf("remote: port %d out of range", s.opts.Port)
	}
	if !ip.IsLoopback() && s.opts.Auth.ClientCount() == 0 {
		return "", fmt.Errorf("remote: refusing non-loopback bind %s with no clients configured", s.opts.Bind)
	}

	cert, fp, err := EnsureCertificate(s.opts.CertDir, ip.String())
	if err != nil {
		return "", err
	}
	s.fp = fp

	addrStr := net.JoinHostPort(ip.String(), fmt.Sprintf("%d", s.opts.Port))
	ln, err := net.Listen("tcp", addrStr)
	if err != nil {
		return "", fmt.Errorf("remote: binding %s: %w", addrStr, err)
	}
	s.ln = ln
	s.addr = ln.Addr().String()

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	tlsLn := tls.NewListener(ln, tlsCfg)

	mux := http.NewServeMux()
	mux.HandleFunc(loginPath, s.opts.Auth.HandleLogin)
	mux.HandleFunc(wsPath, s.handleWS)
	// The shim and every static asset go through the asset server, which injects
	// the shim tag into index.html and serves the shim itself from this
	// package's embed. Registered on "/", the catch-all, so the two exact
	// /__wslremote/ routes above take precedence.
	mux.Handle("/", newAssetServer(s.opts.Assets, s.logf))

	s.httpSrv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          nil,
	}

	go func() {
		if err := s.httpSrv.Serve(tlsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logf("remote: serve stopped: %v", err)
		}
	}()

	s.logf("remote: listening on https://%s (cert %s)", s.addr, s.fp)
	return s.addr, nil
}

// Fingerprint returns the served certificate's SHA-256 fingerprint, for the
// Settings display. Empty before Start.
func (s *Server) Fingerprint() string { return s.fp }

// handleWS authenticates and upgrades one connection, then runs its session.
//
// The order is the security order: same-origin first (a spoofed Origin is 403ed
// before anything else looks at the request), then the session cookie (no valid
// cookie is 401ed), and only then the upgrade. After upgrade the hello frame is
// written before the session is registered for broadcast, so no event can race
// ahead of the method list the shim needs.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.opts.Auth.checkOrigin(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	rec, err := s.opts.Auth.authenticate(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written an error response; nothing more to do.
		return
	}

	info := ClientInfo{
		ID:         newConnID(),
		Name:       rec.name,
		Caps:       rec.caps,
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

	s.logf("remote: %s connected from %s (caps %v)", info.Name, info.RemoteAddr, info.Caps)
	sess.serve()
	s.logf("remote: %s disconnected from %s", info.Name, info.RemoteAddr)
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

// Close shuts the listener down within ctx's budget and leaves no goroutine
// behind — a stray http.Server is a process that will not exit, which the app's
// teardown ordering is meticulous about avoiding.
//
// The sequence: cancel the server context (which cancels every in-flight call),
// close every session (which closes its socket and unblocks its pumps), revoke
// all sessions/tokens, wait for the connection goroutines to drain bounded by
// ctx, and finally Shutdown the http.Server to stop accepting and close the
// listener. Because an upgraded WebSocket is hijacked and http.Server does not
// track it, the explicit session close and connWG wait — not Shutdown — are what
// actually reap the connections.
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
	if s.opts.Auth != nil {
		s.opts.Auth.revokeAll()
	}

	drained := make(chan struct{})
	go func() {
		s.connWG.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-ctx.Done():
		// Budget spent: fall through to Shutdown, which closes the listener
		// regardless, so we do not hang teardown on a client that will not let
		// go. The connection goroutines are already cancelled and closed; this
		// only bounds the WAIT, not the work.
	}

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
