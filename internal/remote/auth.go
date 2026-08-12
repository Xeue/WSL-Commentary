package remote

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// This file closes the exact hole Wails' own dev server leaves open. That
// server's devserver.go sets CheckOrigin to `return true` unconditionally, has
// no auth and no TLS, and reloads the operator's live window on an
// unauthenticated GET. The bridge this package builds controls SendMixerCommands
// (a write path to a live broadcast desk), SetSecret (passphrases) and
// GetKVSCredentials (live AWS session credentials), so an unauthenticated
// listener is not acceptable at any bind address. Everything here — the
// constant-time password check, the per-IP lockout, the HttpOnly/Secure/
// SameSite=Strict cookie, and the strict same-origin check on the upgrade —
// exists to make sure the only thing that reaches the dispatcher is a request
// from a client that proved who it is, from the page this server itself served.

const (
	// loginPath is the JSON login endpoint. A successful POST sets the session
	// cookie the WebSocket upgrade then requires.
	loginPath = "/__wslremote/login"
	// wsPath is the authenticated WebSocket upgrade endpoint.
	wsPath = "/__wslremote/ws"
	// shimPath serves the transport shim, byte-for-byte, ahead of the bundle.
	shimPath = "/__wslremote/shim.js"

	// sessionCookieName is the name of the session cookie. The double-underscore
	// __Host- style prefix is deliberately NOT used: __Host- forbids a Domain
	// attribute and requires Secure+Path=/ which we satisfy, but some WebView
	// and self-signed-cert combinations treat __Host- cookies inconsistently,
	// and the protections we actually rely on (HttpOnly, Secure, SameSite
	// strict) are set explicitly regardless of the name.
	sessionCookieName = "wslremote_session"
)

// Defaults for the authenticator's timing and lockout policy. They are named
// constants rather than literals so the reasoning lives with the number, and so
// a test can reference the same value it is asserting against.
const (
	// defaultSessionTTL is how long a login is good for. Eight hours covers a
	// match day including the long tail of a delayed fixture without asking an
	// operator to re-authenticate mid-event, and expires overnight so a browser
	// left open on a facility PC is not a standing key.
	defaultSessionTTL = 8 * time.Hour
	// defaultLockThreshold is the number of failed logins from one source IP
	// within the window before that IP is locked out. Five tolerates a fat
	// finger or two; it does not tolerate guessing.
	defaultLockThreshold = 5
	// defaultLockWindow is the sliding window over which failures accumulate.
	defaultLockWindow = 5 * time.Minute
	// defaultLockDuration is how long an IP stays locked once the threshold is
	// hit. Fifteen minutes turns an online guessing attack into a crawl without
	// permanently locking out an operator who genuinely forgot a password.
	defaultLockDuration = 15 * time.Minute
	// defaultLoginMinDelay is the fixed floor on how long a login response
	// takes, success or failure. It does two things: it rate-limits online
	// guessing on its own, and it flattens the timing difference between "no
	// such user" and "wrong password" so the response time is not an oracle for
	// which usernames exist.
	defaultLoginMinDelay = 250 * time.Millisecond
)

// sessionRecord is one live login: the client it authenticated as, the
// capabilities that client holds, and when the login expires. Sessions live
// only in memory and are all dropped when the listener is disabled or the
// process exits, so there is no persistent token to steal from disk.
type sessionRecord struct {
	name    string
	caps    []string
	expires time.Time
}

// ipState tracks failed logins from a single source IP for the lockout.
type ipState struct {
	fails       int
	windowStart time.Time
	lockedUntil time.Time
}

// Authenticator owns login, the in-memory session table and the per-IP lockout.
//
// It holds an immutable SNAPSHOT of the client records taken when the listener
// started. Changing the client list (add/delete/set-password) is done by
// editing the settings document and restarting the listener, which builds a
// fresh authenticator and — by dropping this one's session table — revokes
// every session. That is simpler to reason about than mutating credentials
// under a live session table, and "disabling or reconfiguring the listener
// revokes all sessions" is a property worth having for free.
type Authenticator struct {
	// clients is the read-only snapshot, indexed by name.
	clients map[string]Client
	// dummy is a throwaway verifier used when the named user does not exist, so
	// the login path runs the same PBKDF2 work whether or not the user is real
	// and does not leak user existence through timing.
	dummy PBKDF2Params

	mu       sync.Mutex
	sessions map[string]sessionRecord
	ipFails  map[string]*ipState

	// Policy knobs, defaulted by NewAuthenticator. Left as fields (not consts)
	// so in-package tests can shorten the delays and thresholds without waiting
	// out real minutes.
	ttl           time.Duration
	lockThreshold int
	lockWindow    time.Duration
	lockDuration  time.Duration
	minDelay      time.Duration

	// Seams. now and randRead are overridable in tests so session expiry and
	// token generation are deterministic; in production they are time.Now and
	// crypto/rand.
	now      func() time.Time
	randRead func([]byte) (int, error)
}

// NewAuthenticator builds an authenticator over a snapshot of clients with the
// default timing and lockout policy. The clients slice is copied into an index;
// the caller may reuse it.
//
// The dummy verifier is derived once here from a random password. Its only
// purpose is to give the "no such user" branch the same cost as the "wrong
// password" branch; it can never match a real login because the password it
// hashes is never revealed to anyone.
func NewAuthenticator(clients []Client) *Authenticator {
	idx := make(map[string]Client, len(clients))
	for _, c := range clients {
		idx[c.Name] = c
	}
	dummyPw := make([]byte, 32)
	_, _ = rand.Read(dummyPw)
	dummy, _ := hashPassword(string(dummyPw))
	return &Authenticator{
		clients:       idx,
		dummy:         dummy,
		sessions:      make(map[string]sessionRecord),
		ipFails:       make(map[string]*ipState),
		ttl:           defaultSessionTTL,
		lockThreshold: defaultLockThreshold,
		lockWindow:    defaultLockWindow,
		lockDuration:  defaultLockDuration,
		minDelay:      defaultLoginMinDelay,
		now:           time.Now,
		randRead:      rand.Read,
	}
}

// ClientCount reports how many client records this authenticator knows. The
// server consults it to enforce the "no non-loopback bind without a client"
// rule, so that check reads the same snapshot the logins verify against.
func (a *Authenticator) ClientCount() int { return len(a.clients) }

// loginRequest / loginResponse are the JSON bodies of loginPath.
type loginRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

type loginResponse struct {
	OK     bool     `json:"ok"`
	Client string   `json:"client,omitempty"`
	Caps   []string `json:"caps,omitempty"`
	Error  string   `json:"error,omitempty"`
}

// HandleLogin verifies credentials and, on success, issues the session cookie.
//
// The shape is chosen so that neither branch is faster than the other in a way
// an attacker can measure: a missing user runs the same PBKDF2 against a dummy
// verifier, both branches are floored at minDelay, and the comparison inside
// verify is constant-time. The failure response says only "invalid credentials"
// — never whether the user or the password was the problem — for the same
// reason.
func (a *Authenticator) HandleLogin(w http.ResponseWriter, r *http.Request) {
	start := a.now()
	// Deferring the delay guarantees it applies to EVERY exit path, including
	// the early returns for a bad method or an unparseable body, so none of
	// them becomes a fast path that stands out from a real credential check.
	defer a.enforceMinDelay(start)

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ip := clientIP(r)
	if a.lockedOut(ip) {
		// A locked-out source is told the same thing a wrong password is told,
		// and importantly is NOT told how long it is locked for: a lockout that
		// announces its own duration is a schedule for the next attempt.
		writeLoginError(w, http.StatusTooManyRequests)
		return
	}

	// Bound the body: a login is a few dozen bytes, and an unbounded read on an
	// unauthenticated endpoint is a trivial memory-exhaustion lever.
	var req loginRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	if err := dec.Decode(&req); err != nil {
		writeLoginError(w, http.StatusBadRequest)
		return
	}

	rec, ok := a.clients[req.User]
	// Whether or not the user exists, run a full verification: the real record's
	// verifier when it exists, the dummy verifier when it does not. The boolean
	// result is combined so a nonexistent user and a wrong password take the
	// same path and the same time.
	var authed bool
	if ok {
		authed = rec.PBKDF2.verify(req.Password)
	} else {
		_ = a.dummy.verify(req.Password)
		authed = false
	}

	if !authed {
		a.recordFailure(ip)
		writeLoginError(w, http.StatusUnauthorized)
		return
	}

	token, err := a.newToken()
	if err != nil {
		// A token we could not generate securely is a login we must not grant:
		// failing closed here is the only safe option.
		writeLoginError(w, http.StatusInternalServerError)
		return
	}

	a.mu.Lock()
	a.sessions[token] = sessionRecord{
		name:    rec.Name,
		caps:    append([]string(nil), rec.Caps...),
		expires: a.now().Add(a.ttl),
	}
	a.clearFailures(ip)
	a.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,                    // no script, remote or injected, can read it
		Secure:   true,                    // never sent over plain HTTP; TLS is mandatory anyway
		SameSite: http.SameSiteStrictMode, // a cross-site page cannot cause it to be sent
		Expires:  a.now().Add(a.ttl),
		MaxAge:   int(a.ttl.Seconds()),
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(loginResponse{OK: true, Client: rec.Name, Caps: rec.Caps})
}

// authenticate resolves a request's session cookie to a live, unexpired session
// or returns an error. It is what the WebSocket upgrade calls before touching
// the socket: no valid cookie, no upgrade.
//
// An expired session is deleted as it is rejected, so the table does not grow a
// tail of dead entries, and the rejection is indistinguishable from "no cookie"
// to the caller — both are simply "not authenticated".
func (a *Authenticator) authenticate(r *http.Request) (sessionRecord, error) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return sessionRecord{}, errors.New("remote: no session cookie")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, ok := a.sessions[c.Value]
	if !ok {
		return sessionRecord{}, errors.New("remote: unknown session")
	}
	if !a.now().Before(rec.expires) {
		delete(a.sessions, c.Value)
		return sessionRecord{}, errors.New("remote: session expired")
	}
	return rec, nil
}

// checkOrigin enforces STRICT same-origin on the upgrade: the Origin header's
// host:port must equal the request's Host, and a request with no Origin at all
// is refused.
//
// This is the precise inversion of the devserver hole. gorilla's own default
// CheckOrigin makes this comparison too, but it ALLOWS a request that carries no
// Origin — and a non-browser client simply omits Origin. Requiring Origin to be
// present and to match means a page served from any other origin, and any
// scripted client that forgets to forge the header, is turned away before the
// socket is upgraded. It is not a substitute for the cookie (an attacker who can
// set the header still needs a valid session), but it removes the class of
// cross-site request that rides the operator's own cookie.
func (a *Authenticator) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// revokeAll drops every live session. The server calls it on Close so that a
// disabled or shut-down listener leaves no token that would work if it came
// back up with the same in-memory table — which it never does, but revoking is
// cheap and states the intent.
func (a *Authenticator) revokeAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions = make(map[string]sessionRecord)
}

// sessionCount is a test and diagnostic helper reporting how many live sessions
// exist. It is not part of the request path.
func (a *Authenticator) sessionCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sessions)
}

// newToken returns a fresh 32-byte random session token, base64url-encoded.
// 256 bits of entropy from crypto/rand makes the token unguessable; a failure
// to read randomness is surfaced as an error so the caller fails the login
// rather than issuing a weak token.
func (a *Authenticator) newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := a.randRead(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// lockedOut reports whether ip is currently locked. It takes the lock because
// the lockout maps are shared with the failure recorder.
func (a *Authenticator) lockedOut(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.ipFails[ip]
	if st == nil {
		return false
	}
	return a.now().Before(st.lockedUntil)
}

// recordFailure counts a failed login from ip and locks the source once the
// threshold is reached within the window.
func (a *Authenticator) recordFailure(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	st := a.ipFails[ip]
	if st == nil {
		st = &ipState{windowStart: now}
		a.ipFails[ip] = st
	}
	// A failure that arrives after the window has elapsed since counting began
	// starts a fresh window: the threshold is N failures in a WINDOW, not N
	// failures ever, so an operator who mistypes once a week is never locked.
	if now.Sub(st.windowStart) > a.lockWindow {
		st.windowStart = now
		st.fails = 0
	}
	st.fails++
	if st.fails >= a.lockThreshold {
		st.lockedUntil = now.Add(a.lockDuration)
		st.fails = 0
		st.windowStart = now
	}
}

// clearFailures resets an IP's failure state, called on a successful login so a
// correct password wipes the slate. It assumes the caller holds a.mu.
func (a *Authenticator) clearFailures(ip string) { delete(a.ipFails, ip) }

// enforceMinDelay sleeps until minDelay has elapsed since start. See the field
// comment on defaultLoginMinDelay for why every login response is floored.
func (a *Authenticator) enforceMinDelay(start time.Time) {
	elapsed := a.now().Sub(start)
	if elapsed < a.minDelay {
		time.Sleep(a.minDelay - elapsed)
	}
}

// writeLoginError sends a uniform JSON failure. It never distinguishes "no such
// user" from "wrong password" from "locked out" in its body; only the status
// code varies, and even that carries no detail an attacker can act on.
func writeLoginError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(loginResponse{OK: false, Error: "invalid credentials"})
}

// clientIP extracts the source IP from r.RemoteAddr, dropping the port.
//
// It deliberately does NOT consult X-Forwarded-For or any other client-supplied
// header: this listener is meant to be reached directly on a LAN, and trusting a
// forwarded-for header would let an attacker spoof a different source IP on
// every request and walk straight past the per-IP lockout. If a reverse proxy
// is ever put in front, that trust has to be added deliberately, not inherited.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
