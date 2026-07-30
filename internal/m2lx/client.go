package m2lx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	signInPath  = "/api/local_auth/signin"
	refreshPath = "/api/local_auth/refresh_token"

	kvsInfoPathFmt  = "/api/live_operation/kvs/webrtc_info/%s"
	kvsTokenPathFmt = "/api/live_operation/kvs/webrtc_token/%s"

	// httpTimeout bounds every REST call. Measured round trip to the dev
	// instance was 20.1/21.7/23.8/45.2 ms (min/median/mean/max, n=12,
	// docs/test-results.md §1.2 item 7); 10 s leaves generous headroom for
	// a slow link without hanging the UI indefinitely on a dead host.
	httpTimeout = 10 * time.Second
)

// signInRequest is the body of POST /api/local_auth/signin.
//
// The field MUST be named "alias". M2L-X's sign-in endpoint returns
// HTTP 500 if this is sent as "username" instead — measured, and recorded
// in windows-app-spec.md §4 and CONTRACT.md. Do not "fix" this to
// "username"; it is not a typo, it is what the server actually requires.
type signInRequest struct {
	Alias    string `json:"alias"`
	Password string `json:"password"`
}

// refreshRequest is the body of POST /api/local_auth/refresh_token.
//
// The wire shape of this endpoint is not in the measurement record
// (docs/test-results.md, docs/architecture.md only establish that it
// exists and that refresh happens at RefreshFraction of TokenLifetime).
// RefreshToken is sent when SignIn's response carried one, in case the
// endpoint is OAuth2-refresh-token-style; the soon-to-expire access token
// is also always sent as the Authorization bearer, in case the endpoint
// instead re-issues based on the caller's current identity. Sending both
// is harmless if the server only consults one, and maximises the chance of
// matching whichever shape M2L-X actually implements. This must be
// verified at Gate C against a live instance; see the WP-2 report.
type refreshRequest struct {
	RefreshToken string `json:"refresh_token,omitempty"`
}

// signInResponse is the flexible decode target for both sign-in and
// refresh responses. Neither response's exact field name for the bearer
// token is in the measurement record, so both common conventions are
// accepted; bearerToken reports a clear error if neither is populated
// rather than silently returning an empty token. expires_in, if any, is
// intentionally ignored: TokenLifetime (86399 s, m2lx.go) is the measured
// constant WP-0 fixed for this, not a value to trust from a single
// response.
type signInResponse struct {
	AccessToken  string `json:"access_token"`
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
}

// bearerToken returns whichever of the two conventional token fields is
// populated, or an error naming the field that was expected.
func (r signInResponse) bearerToken() (string, error) {
	if r.AccessToken != "" {
		return r.AccessToken, nil
	}
	if r.Token != "" {
		return r.Token, nil
	}
	return "", fmt.Errorf(`m2lx: sign-in/refresh response has neither "access_token" nor "token"`)
}

// client is the concrete Client implementation. It never logs the password
// or the bearer token: neither appears in any error message this file
// constructs.
type client struct {
	rest       resolvedHost // REST base: host[:port] plus http-vs-https, decided once (host.go)
	httpClient *http.Client

	// ctx/cancel bound the lifetime of the background refresh goroutine.
	// They are the client's own, not derived from any single call's
	// context: a caller that wraps SignIn in a short-lived
	// context.WithTimeout (entirely reasonable for one REST call) must not
	// thereby kill hours of subsequent token refreshes. Close cancels them
	// and waits for refreshLoop to actually exit before returning.
	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.RWMutex
	alias        string
	password     string
	token        string
	refreshToken string

	// refreshStarted and refreshLoopDone track the lazily-started refresh
	// goroutine, guarded by mu so SignIn (which starts it) and Close
	// (which waits for it) always agree on whether there is anything to
	// wait for. refreshLoopDone is only meaningful — only guaranteed to
	// ever be closed — once refreshStarted is true; Close checks both
	// under the same lock acquisition rather than racing two separate
	// reads against SignIn's writes.
	refreshStarted  bool
	refreshLoopDone chan struct{}
}

// var _ Client = (*client)(nil) is a compile-time check that *client keeps
// satisfying Client, including Close, as both evolve.
var _ Client = (*client)(nil)

// newClient constructs the real Client for host.
func newClient(host string) *client {
	ctx, cancel := context.WithCancel(context.Background())
	return &client{
		rest:       resolveHost(host, "REST"),
		httpClient: &http.Client{Timeout: httpTimeout},
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Close implements Client. It cancels the background token-refresh
// goroutine, if SignIn ever started one, and blocks until that goroutine
// has actually returned — not merely until cancellation was requested —
// so a caller that follows Close with reusing the process's http.Client
// or exiting never races a refresh cycle's in-flight request.
//
// Idempotent and safe to call concurrently with itself, with Token, and
// even with a SignIn racing to start the goroutine for the first time:
// context.CancelFunc is inherently safe to call more than once, and
// refreshStarted/refreshLoopDone are only ever read and written under mu,
// so every caller — Close or SignIn — sees a consistent, final answer to
// "was the loop started, and if so, which channel reports its exit".
// A SignIn that starts the loop for the first time after Close has
// already run is harmless: ctx is already cancelled, so refreshLoop's
// first select observes ctx.Done() and returns immediately.
func (c *client) Close() error {
	c.cancel()

	c.mu.RLock()
	started := c.refreshStarted
	done := c.refreshLoopDone
	c.mu.RUnlock()

	if started {
		<-done
	}
	return nil
}

// SignIn implements Client.
func (c *client) SignIn(ctx context.Context, alias, password string) error {
	var wire signInResponse
	err := c.doJSON(ctx, http.MethodPost, signInPath,
		signInRequest{Alias: alias, Password: password}, &wire, "")
	if err != nil {
		return fmt.Errorf("m2lx: sign-in failed: %w", err)
	}
	tok, err := wire.bearerToken()
	if err != nil {
		return fmt.Errorf("m2lx: sign-in: %w", err)
	}

	c.mu.Lock()
	c.alias = alias
	c.password = password
	c.token = tok
	c.refreshToken = wire.RefreshToken
	c.mu.Unlock()

	// Started at most once per client, regardless of how many times SignIn
	// is later called (e.g. re-authenticating after a Settings change):
	// the same loop keeps refreshing whatever credentials/token are
	// current, so it never needs to be restarted. The check-and-set of
	// refreshStarted happens under mu so a racing Close (which reads the
	// same two fields under mu) can never observe a half-started loop:
	// either it sees refreshStarted still false and has nothing to wait
	// for, or it sees it true together with the exact channel that
	// goroutine will close on exit.
	c.mu.Lock()
	startLoop := !c.refreshStarted
	if startLoop {
		c.refreshStarted = true
		c.refreshLoopDone = make(chan struct{})
	}
	c.mu.Unlock()
	if startLoop {
		go c.refreshLoop()
	}

	return nil
}

// Refresh implements Client. On failure it falls back to a full SignIn
// with the credentials captured at the last successful SignIn, because the
// refresh token's own TTL is unmeasured (docs/architecture.md §5.8: "Verify
// the refresh token's own TTL — it is unmeasured"). Only if that fallback
// also fails does Refresh give up, clearing the held token so a subsequent
// call that requires one sees ErrNotSignedIn instead of silently reusing a
// token M2L-X has likely already expired.
func (c *client) Refresh(ctx context.Context) error {
	c.mu.RLock()
	curToken := c.token
	curRefreshToken := c.refreshToken
	alias := c.alias
	password := c.password
	c.mu.RUnlock()

	if curToken == "" {
		return ErrNotSignedIn
	}

	var wire signInResponse
	refreshErr := c.doJSON(ctx, http.MethodPost, refreshPath,
		refreshRequest{RefreshToken: curRefreshToken}, &wire, curToken)
	if refreshErr == nil {
		tok, tokErr := wire.bearerToken()
		if tokErr == nil {
			c.mu.Lock()
			c.token = tok
			if wire.RefreshToken != "" {
				c.refreshToken = wire.RefreshToken
			}
			c.mu.Unlock()
			return nil
		}
		refreshErr = fmt.Errorf("refresh response malformed: %w", tokErr)
	}

	if alias == "" {
		c.mu.Lock()
		c.token = ""
		c.mu.Unlock()
		return fmt.Errorf("%w: refresh failed (%v) and no credentials are cached for a fallback sign-in", ErrNotSignedIn, refreshErr)
	}

	if signErr := c.SignIn(ctx, alias, password); signErr != nil {
		c.mu.Lock()
		c.token = ""
		c.mu.Unlock()
		return fmt.Errorf("%w: refresh failed (%v) and fallback sign-in failed (%v)", ErrNotSignedIn, refreshErr, signErr)
	}
	return nil
}

// refreshLoop runs for the lifetime of the client, renewing the token at
// RefreshFraction of TokenLifetime (m2lx.go: 0.5 of 86399 s, both measured
// values per CONTRACT.md). It is the "goroutine owned by the client,
// cancellable by context" the WP-2 brief requires: it selects on c.ctx,
// which Close cancels — and closes refreshLoopDone on the way out, which
// is the signal Close blocks on so it returns only once this goroutine
// has actually stopped, not merely once it has been asked to.
//
// A failed refresh cycle is not retried on a shorter timer here — Refresh
// itself already falls back to a full SignIn, and if that also fails it
// clears the held token, which is observable via Token() returning "".
// The next scheduled cycle (still RefreshFraction*TokenLifetime away) will
// try again from empty credentials being cached is exactly the
// ErrNotSignedIn case Refresh returns for.
func (c *client) refreshLoop() {
	defer close(c.refreshLoopDone)
	interval := time.Duration(float64(TokenLifetime) * RefreshFraction)
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-timer.C:
		}
		rctx, cancel := context.WithTimeout(c.ctx, httpTimeout)
		_ = c.Refresh(rctx)
		cancel()
		timer.Reset(interval)
	}
}

// Token implements Client. Safe to call concurrently with Refresh/SignIn.
func (c *client) Token() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token
}

// KVSInfo implements Client.
func (c *client) KVSInfo(ctx context.Context, eventID string) (KVSInfo, error) {
	tok := c.Token()
	if tok == "" {
		return KVSInfo{}, ErrNotSignedIn
	}

	var wire struct {
		Region           string              `json:"region"`
		SignalingChannel map[string][]string `json:"signaling_channel"`
	}
	path := fmt.Sprintf(kvsInfoPathFmt, url.PathEscape(eventID))
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &wire, tok); err != nil {
		return KVSInfo{}, fmt.Errorf("m2lx: kvs webrtc_info: %w", err)
	}

	info := KVSInfo{
		Region:   wire.Region,
		Channels: wire.SignalingChannel,
	}
	// Defensive by construction: SignalingChannel may be nil, may lack the
	// "pgm" key, or may map it to an empty list — every one of those reads
	// as "no channel name" here rather than panicking on an index or a nil
	// map access. Per the KVSInfo.ChannelName doc comment (m2lx.go), an
	// empty result is intentionally left for the caller (WP-4) to treat as
	// an error, not raised as one here: this is a single-sample-measured
	// shape (SP-1) and other keys may legitimately appear.
	if names, ok := info.Channels[ChannelKeyPGM]; ok && len(names) > 0 {
		info.ChannelName = names[0]
	}
	return info, nil
}

// KVSToken implements Client.
func (c *client) KVSToken(ctx context.Context, eventID string) (KVSToken, error) {
	tok := c.Token()
	if tok == "" {
		return KVSToken{}, ErrNotSignedIn
	}

	var wire struct {
		IdentityID string `json:"identity_id"`
		Token      string `json:"token"`
	}
	path := fmt.Sprintf(kvsTokenPathFmt, url.PathEscape(eventID))
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &wire, tok); err != nil {
		return KVSToken{}, fmt.Errorf("m2lx: kvs webrtc_token: %w", err)
	}
	return KVSToken{IdentityID: wire.IdentityID, Token: wire.Token}, nil
}

// doJSON performs one REST call: JSON-encodes body (if non-nil) as the
// request, attaches bearer as an Authorization: Bearer header (unless
// bearer is empty, e.g. for SignIn itself), and JSON-decodes the response
// into out (if non-nil). Errors never include the request body or the
// bearer value, so the password and the token are never captured in an
// error string that might later be logged.
func (c *client) doJSON(ctx context.Context, method, path string, body, out any, bearer string) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body for %s: %w", path, err)
		}
		bodyReader = bytes.NewReader(b)
	}

	u := url.URL{Scheme: c.rest.restScheme(), Host: c.rest.hostPort, Path: path}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
	if err != nil {
		return fmt.Errorf("building request for %s: %w", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned HTTP %d", path, resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decoding response from %s: %w", path, err)
		}
	}
	return nil
}
