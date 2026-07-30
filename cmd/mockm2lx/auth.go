// Auth: POST /api/local_auth/signin and POST /api/local_auth/refresh_token.
//
// Response shape, request body and the alias/username distinction are the
// measured facts recorded in docs/archive-windows-app-spec-v1-rejected.md line
// 854 and docs/windows-app-spec.md line 78:
//
//	POST /api/local_auth/signin   {"alias":"...","password":"..."}
//	  -> {refresh_token, access_token, expires_in, id, roleIds}
//	POST /api/local_auth/refresh_token   {"refresh_token":"..."}
//	  -> the same shape
//
// A body using "username" instead of "alias" returns HTTP 500 on the real
// instance; this is the one behaviour docs/windows-app-spec.md states as fact
// with no [EXP]/[U] qualifier, and it is the regression CONTRACT.md calls out
// by name, so it is reproduced exactly rather than approximated.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"
)

// accessTokenRawBytes is chosen so base64 standard encoding produces exactly
// 1088 characters (816 is divisible by 3, so there is no padding), matching
// the measured "opaque ~1088-byte base64 blob" access token
// (docs/archive-windows-app-spec-v1-rejected.md line 854). The value is
// otherwise meaningless: nothing about it is ever parsed, by the real
// instance or by this mock's own clients.
const accessTokenRawBytes = 816

// refreshTokenRawBytes has no measured size; the refresh token's own
// lifetime and rotation behaviour is open question [U] in
// docs/archive-windows-app-spec-v1-rejected.md line 904. 48 bytes is simply
// large enough not to collide.
const refreshTokenRawBytes = 48

// signInRequest is the wire shape of a correct sign-in body.
type signInRequest struct {
	Alias    string `json:"alias"`
	Password string `json:"password"`
}

// authResponse is the wire shape returned by both sign-in and refresh.
type authResponse struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	ExpiresIn    int      `json:"expires_in"`
	ID           int      `json:"id"`
	RoleIDs      []string `json:"roleIds"`
}

// randomToken returns n cryptographically random bytes, standard-base64
// encoded. It panics only if the system CSPRNG is unavailable, which would
// make every other security-relevant thing on the machine equally broken.
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("mockm2lx: crypto/rand unavailable: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(b)
}

// handleSignIn implements POST /api/local_auth/signin.
func (a *App) handleSignIn(w http.ResponseWriter, r *http.Request) {
	body, err := readLimitedBody(r)
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Decode into a generic map first so the alias/username distinction can
	// be made on which KEY is present, independent of whatever the value
	// unmarshals to. A malformed body is a plain 400: only the specific
	// "username instead of alias" shape reproduces the measured 500.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if _, hasAlias := raw["alias"]; !hasAlias {
		if _, hasUsername := raw["username"]; hasUsername {
			a.logf("auth", "signin: body used \"username\" instead of \"alias\" — returning 500, as the real instance does")
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		http.Error(w, `{"error":"alias is required"}`, http.StatusBadRequest)
		return
	}

	var req signInRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.Alias != a.opts.Alias || req.Password != a.opts.Password {
		a.logf("auth", "signin: bad credentials for alias %q", req.Alias)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad credentials"})
		return
	}

	sess := a.newSession(req.Alias)
	a.logf("auth", "signin: alias %q ok, token expires in %s", req.Alias, a.opts.TokenTTL)
	writeJSON(w, http.StatusOK, sess.toResponse())
}

// refreshRequest is the wire shape of POST /api/local_auth/refresh_token.
type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// handleRefreshToken implements POST /api/local_auth/refresh_token. It
// identifies the session by the refresh token in the body, not by an
// Authorization header — matching
// docs/archive-windows-app-spec-v1-rejected.md line 890.
//
// The refresh token is rotated on every successful refresh. Whether the real
// instance rotates it is unmeasured (open question, same doc line 904); this
// mock exercises the harder case, in which a client that reuses a stale
// refresh token after a successful refresh must fail, so that WP-2 cannot
// pass by accident.
func (a *App) handleRefreshToken(w http.ResponseWriter, r *http.Request) {
	body, err := readLimitedBody(r)
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	var req refreshRequest
	if err := json.Unmarshal(body, &req); err != nil || req.RefreshToken == "" {
		http.Error(w, `{"error":"refresh_token is required"}`, http.StatusBadRequest)
		return
	}

	a.mu.Lock()
	sess, ok := a.refreshIndex[req.RefreshToken]
	if !ok {
		a.mu.Unlock()
		a.logf("auth", "refresh: unknown refresh_token")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid refresh token"})
		return
	}

	// Rotate: the old pair stops working the instant the new one is issued.
	delete(a.sessions, sess.accessToken)
	delete(a.refreshIndex, sess.refreshToken)

	sess.accessToken = randomToken(accessTokenRawBytes)
	sess.refreshToken = randomToken(refreshTokenRawBytes)
	sess.issuedAt = time.Now()
	sess.expiresAt = sess.issuedAt.Add(a.opts.TokenTTL)

	a.sessions[sess.accessToken] = sess
	a.refreshIndex[sess.refreshToken] = sess
	a.mu.Unlock()

	a.logf("auth", "refresh: alias %q ok, new token expires in %s", sess.alias, a.opts.TokenTTL)
	writeJSON(w, http.StatusOK, sess.toResponse())
}

// newSession creates and registers a fresh session for alias.
func (a *App) newSession(alias string) *session {
	now := time.Now()
	sess := &session{
		alias:        alias,
		accessToken:  randomToken(accessTokenRawBytes),
		refreshToken: randomToken(refreshTokenRawBytes),
		issuedAt:     now,
		expiresAt:    now.Add(a.opts.TokenTTL),
	}

	a.mu.Lock()
	a.sessions[sess.accessToken] = sess
	a.refreshIndex[sess.refreshToken] = sess
	a.mu.Unlock()

	return sess
}

// toResponse renders sess in the wire shape of authResponse. expires_in is
// recomputed from the remaining lifetime at call time, matching the real
// field's meaning ("seconds from now"), so a session inspected long after
// issuance still reports a sane, non-negative value down to zero.
func (sess *session) toResponse() authResponse {
	remaining := int(time.Until(sess.expiresAt).Seconds())
	if remaining < 0 {
		remaining = 0
	}
	return authResponse{
		AccessToken:  sess.accessToken,
		RefreshToken: sess.refreshToken,
		ExpiresIn:    remaining,
		ID:           1001,
		RoleIDs:      []string{"readonly"},
	}
}

// checkBearerToken validates the Authorization: Bearer <token> header of a
// REST request against live sessions. It returns the session on success.
func (a *App) checkBearerToken(r *http.Request) (*session, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return nil, false
	}
	token := h[len(prefix):]
	return a.checkToken(token)
}

// checkToken validates an access token — used by both the Authorization
// header (REST) and the access_token query parameter (the status
// WebSocket, whose token necessarily rides in the URL because there is no
// header on a WebSocket upgrade's application data).
func (a *App) checkToken(token string) (*session, bool) {
	if token == "" {
		return nil, false
	}
	a.mu.RLock()
	sess, ok := a.sessions[token]
	a.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(sess.expiresAt) {
		return nil, false
	}
	return sess, true
}

// requireAuth wraps h so it 401s any request without a valid bearer token.
func (a *App) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.checkBearerToken(r); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing bearer token"})
			return
		}
		h(w, r)
	}
}

// expireTokens sets every live session to expire after d from now (d<=0
// means already expired). It backs the /control/expire-token fault: "make
// the token expire early, to exercise refresh and WebSocket reopen". It
// returns the number of sessions affected.
func (a *App) expireTokens(d time.Duration) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	if d <= 0 {
		// Push firmly into the past rather than setting expiresAt to
		// time.Now() itself: checkToken takes its own, separately-measured
		// time.Now() a few instructions later, and time.Time.After is a
		// strict inequality — on a coarse or contended clock the two reads
		// can tie, in which case a "d=0" expiry would still validate for
		// one more check. A "made-up" fault must not depend on clock
		// resolution to actually fire.
		d = -time.Second
	}
	newExpiry := time.Now().Add(d)
	n := 0
	for _, sess := range a.sessions {
		sess.expiresAt = newExpiry
		n++
	}
	return n
}
