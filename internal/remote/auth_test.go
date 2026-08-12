package remote

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// testClock is a controllable clock so session-expiry can be tested without
// waiting out real hours. All access is mutexed because the server reads it from
// a handler goroutine while the test advances it.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Now()} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Test 1: the login handshake, the cookie, and that the cookie is required
// ---------------------------------------------------------------------------

func TestAuth_UnauthenticatedUpgradeIs401(t *testing.T) {
	h := newHarness(t, []Client{testClient(t, "op", "s3cret", string(CapView))})
	// Correct origin, NO cookie: the upgrade must be refused with 401.
	conn, resp, err := h.dial(t, nil, "")
	if err == nil {
		conn.Close()
		t.Fatal("upgrade without a session cookie succeeded; want 401")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", statusOf(resp))
	}
}

func TestAuth_WrongPasswordIs401(t *testing.T) {
	h := newHarness(t, []Client{testClient(t, "op", "s3cret", string(CapView))})
	_, status, _ := h.login(t, "op", "WRONG")
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong-password login status = %d, want 401", status)
	}
	// A non-existent user must be indistinguishable from a wrong password.
	_, status2, _ := h.login(t, "nobody", "whatever")
	if status2 != http.StatusUnauthorized {
		t.Fatalf("unknown-user login status = %d, want 401", status2)
	}
}

func TestAuth_LoginHasFixedMinimumDelay(t *testing.T) {
	// The fixed floor is what rate-limits online guessing and flattens the
	// timing difference between "no such user" and "wrong password". Raise the
	// floor for this test so the assertion is not racing the scheduler.
	h := newHarness(t, []Client{testClient(t, "op", "s3cret", string(CapView))})
	h.auth.minDelay = 150 * time.Millisecond

	start := time.Now()
	_, status, _ := h.login(t, "op", "WRONG")
	elapsed := time.Since(start)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("login returned in %v, want at least the 150ms floor", elapsed)
	}
}

func TestAuth_CorrectLoginSetsHardenedCookieAndItIsRequired(t *testing.T) {
	h := newHarness(t, []Client{testClient(t, "op", "s3cret", string(CapView))})
	cookies, status, _ := h.login(t, "op", "s3cret")
	if status != http.StatusOK {
		t.Fatalf("correct login status = %d, want 200", status)
	}
	var sess *http.Cookie
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("login set no session cookie")
	}
	if !sess.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if !sess.Secure {
		t.Error("session cookie is not Secure")
	}
	if sess.SameSite != http.SameSiteStrictMode {
		t.Errorf("session cookie SameSite = %v, want Strict", sess.SameSite)
	}

	// The cookie is what the upgrade requires: with it, the upgrade succeeds.
	conn, _, err := h.dial(t, cookies, "")
	if err != nil {
		t.Fatalf("upgrade with a valid cookie failed: %v", err)
	}
	defer conn.Close()
	hello := readFrame(t, conn)
	if hello["t"] != FrameHello {
		t.Fatalf("first frame = %v, want hello", hello["t"])
	}
}

// ---------------------------------------------------------------------------
// Test 2: the Origin spoof that devserver.go leaves open, tested CLOSED
// ---------------------------------------------------------------------------

func TestAuth_OriginSpoofWithMatchingHostRefused(t *testing.T) {
	h := newHarness(t, []Client{testClient(t, "op", "s3cret", string(CapView))})
	cookies, status, _ := h.login(t, "op", "s3cret")
	if status != http.StatusOK {
		t.Fatalf("login status = %d", status)
	}
	// A valid cookie, the correct Host (the dialer sets it from the URL), but a
	// forged Origin. This is precisely the request devserver.go's `return true`
	// would wave through. It must be refused with 403.
	conn, resp, err := h.dial(t, cookies, "https://evil.example")
	if err == nil {
		conn.Close()
		t.Fatal("upgrade with a spoofed Origin succeeded; want 403")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %v, want 403", statusOf(resp))
	}
}

func TestAuth_MissingOriginRefused(t *testing.T) {
	// A client that omits Origin entirely — a non-browser — is refused too. This
	// is stricter than gorilla's default (which allows a missing Origin) and is
	// the whole point of the strict check.
	h := newHarness(t, []Client{testClient(t, "op", "s3cret", string(CapView))})
	cookies, _, _ := h.login(t, "op", "s3cret")

	d := websocket.Dialer{
		TLSClientConfig:  h.httpClient().Transport.(*http.Transport).TLSClientConfig,
		HandshakeTimeout: 5 * time.Second,
	}
	hdr := http.Header{}
	for _, c := range cookies {
		hdr.Add("Cookie", c.Name+"="+c.Value)
	}
	// deliberately no Origin header
	conn, resp, err := d.Dial("wss://"+h.addr+wsPath, hdr)
	if err == nil {
		conn.Close()
		t.Fatal("upgrade with no Origin succeeded; want refusal")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %v, want 403", statusOf(resp))
	}
}

// ---------------------------------------------------------------------------
// Test 9: an expired session is refused on the upgrade
// ---------------------------------------------------------------------------

func TestAuth_ExpiredSessionRefused(t *testing.T) {
	h := newHarness(t, []Client{testClient(t, "op", "s3cret", string(CapView))})
	clk := newTestClock()
	h.auth.now = clk.now

	cookies, status, _ := h.login(t, "op", "s3cret")
	if status != http.StatusOK {
		t.Fatalf("login status = %d", status)
	}
	// Before expiry the cookie works.
	conn, _, err := h.dial(t, cookies, "")
	if err != nil {
		t.Fatalf("pre-expiry upgrade failed: %v", err)
	}
	conn.Close()

	// Advance past the TTL; the same cookie must now be refused.
	clk.advance(defaultSessionTTL + time.Minute)
	conn2, resp, err := h.dial(t, cookies, "")
	if err == nil {
		conn2.Close()
		t.Fatal("expired session was accepted on upgrade")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", statusOf(resp))
	}
}

// ---------------------------------------------------------------------------
// Test 10: per-source-IP lockout after N failures
// ---------------------------------------------------------------------------

func TestAuth_PerIPLockoutAfterNFailures(t *testing.T) {
	h := newHarness(t, []Client{testClient(t, "op", "s3cret", string(CapView))})
	h.auth.lockThreshold = 3

	// Three wrong passwords from the same source IP, each a 401.
	for i := 0; i < 3; i++ {
		_, status, _ := h.login(t, "op", "WRONG")
		if status != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i+1, status)
		}
	}
	// The next attempt is locked out — and this is true even with the CORRECT
	// password, proving the lockout gates before verification.
	_, status, _ := h.login(t, "op", "s3cret")
	if status != http.StatusTooManyRequests {
		t.Fatalf("post-lockout status = %d, want 429", status)
	}
}

// ---------------------------------------------------------------------------
// Test 13 (transport half): no route hands back a stored password
// ---------------------------------------------------------------------------

func TestAuth_LoginResponseNeverLeaksTheStoredHash(t *testing.T) {
	// Build the client explicitly so the test holds the exact stored verifier.
	client := testClient(t, "op", "s3cret", string(CapView))
	h := newHarness(t, []Client{client})

	_, status, body := h.login(t, "op", "s3cret")
	if status != http.StatusOK {
		t.Fatalf("login status = %d", status)
	}
	text := string(body)
	if strings.Contains(text, client.PBKDF2.Hash) {
		t.Error("login response body contains the stored password hash")
	}
	if strings.Contains(text, client.PBKDF2.Salt) {
		t.Error("login response body contains the stored salt")
	}
	if strings.Contains(strings.ToLower(text), "password") {
		t.Error("login response body mentions a password field")
	}
}

// statusOf safely reports a response's status for an error message.
func statusOf(resp *http.Response) any {
	if resp == nil {
		return "no response"
	}
	return resp.StatusCode
}
