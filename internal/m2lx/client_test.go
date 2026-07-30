package m2lx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// hostOf strips the scheme from an httptest.Server URL, since Client's host
// argument is documented as "a bare hostname or host:port with no scheme".
func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	return u.Host
}

func TestClient_SignIn_SendsAliasNotUsername(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != signInPath {
			t.Fatalf("path = %q, want %q", r.URL.Path, signInPath)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": "tok-1"})
	}))
	defer srv.Close()

	c := newClient(hostOf(t, srv.URL))
	c.httpClient = srv.Client()
	defer c.Close()

	if err := c.SignIn(context.Background(), "commentator1", "hunter2"); err != nil {
		t.Fatalf("SignIn: %v", err)
	}

	if _, hasUsername := gotBody["username"]; hasUsername {
		t.Fatalf(`request body contains "username" — M2L-X returns HTTP 500 for this field name`)
	}
	if gotBody["alias"] != "commentator1" {
		t.Fatalf(`request body "alias" = %v, want "commentator1"`, gotBody["alias"])
	}
	if gotBody["password"] != "hunter2" {
		t.Fatalf(`request body "password" = %v`, gotBody["password"])
	}
	if got := c.Token(); got != "tok-1" {
		t.Fatalf("Token() = %q, want tok-1", got)
	}
}

func TestClient_SignIn_HTTP500IsAnError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newClient(hostOf(t, srv.URL))
	c.httpClient = srv.Client()
	defer c.Close()

	if err := c.SignIn(context.Background(), "commentator1", "hunter2"); err == nil {
		t.Fatalf("expected an error from a 500 response")
	}
	if got := c.Token(); got != "" {
		t.Fatalf("Token() = %q after a failed sign-in, want empty", got)
	}
}

func TestClient_SignIn_AcceptsTokenFieldConvention(t *testing.T) {
	// The exact response field name is not in the measurement record; the
	// client must accept either "access_token" or "token".
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"token": "tok-legacy"})
	}))
	defer srv.Close()

	c := newClient(hostOf(t, srv.URL))
	c.httpClient = srv.Client()
	defer c.Close()

	if err := c.SignIn(context.Background(), "a", "b"); err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if got := c.Token(); got != "tok-legacy" {
		t.Fatalf("Token() = %q, want tok-legacy", got)
	}
}

func TestClient_SignIn_MissingTokenFieldIsAnError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"unrelated_field": "x"})
	}))
	defer srv.Close()

	c := newClient(hostOf(t, srv.URL))
	c.httpClient = srv.Client()
	defer c.Close()

	err := c.SignIn(context.Background(), "a", "b")
	if err == nil {
		t.Fatalf("expected an error when the response has neither access_token nor token")
	}
}

func TestClient_Token_EmptyBeforeSignIn(t *testing.T) {
	c := newClient("example.invalid")
	defer c.Close()
	if got := c.Token(); got != "" {
		t.Fatalf("Token() = %q before any SignIn, want empty", got)
	}
}

func TestClient_KVSInfo_RequiresSignIn(t *testing.T) {
	c := newClient("example.invalid")
	defer c.Close()
	if _, err := c.KVSInfo(context.Background(), "event-1"); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("KVSInfo before SignIn = %v, want ErrNotSignedIn", err)
	}
}

func TestClient_KVSToken_RequiresSignIn(t *testing.T) {
	c := newClient("example.invalid")
	defer c.Close()
	if _, err := c.KVSToken(context.Background(), "event-1"); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("KVSToken before SignIn = %v, want ErrNotSignedIn", err)
	}
}

// TestClient_KVSInfo_MeasuredShape reproduces the one measured response
// recorded in docs/test-results.md §2.4 item 6 and CONTRACT.md:
//
//	{"region":"eu-west-1","signaling_channel":{"pgm":["webrtc-wslstudios-matcht"]}}
func TestClient_KVSInfo_MeasuredShape(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/live_operation/kvs/webrtc_info/dl9-5p5ah0bd-empd"
		if r.URL.Path != wantPath {
			t.Fatalf("path = %q, want %q", r.URL.Path, wantPath)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-1" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Write([]byte(`{"region":"eu-west-1","signaling_channel":{"pgm":["webrtc-wslstudios-matcht"]}}`))
	}))
	defer srv.Close()

	c := newClient(hostOf(t, srv.URL))
	c.httpClient = srv.Client()
	c.token = "tok-1"
	defer c.Close()

	info, err := c.KVSInfo(context.Background(), "dl9-5p5ah0bd-empd")
	if err != nil {
		t.Fatalf("KVSInfo: %v", err)
	}
	if info.Region != "eu-west-1" {
		t.Fatalf("Region = %q", info.Region)
	}
	if info.ChannelName != "webrtc-wslstudios-matcht" {
		t.Fatalf("ChannelName = %q", info.ChannelName)
	}
	if len(info.Channels["pgm"]) != 1 || info.Channels["pgm"][0] != "webrtc-wslstudios-matcht" {
		t.Fatalf("Channels = %+v", info.Channels)
	}
}

// TestClient_KVSInfo_DefensiveOnUnexpectedShape covers the instruction to
// parse defensively: the signaling_channel map may have keys other than
// "pgm", and lists may be empty. Neither should error or panic; ChannelName
// is simply left empty for the caller (WP-4/kvs.Fetch) to treat as an
// error, per the KVSInfo.ChannelName doc comment in m2lx.go.
func TestClient_KVSInfo_DefensiveOnUnexpectedShape(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"pgm key absent, other key present", `{"region":"eu-west-1","signaling_channel":{"cln":["some-channel"]}}`},
		{"pgm present but empty list", `{"region":"eu-west-1","signaling_channel":{"pgm":[]}}`},
		{"signaling_channel absent entirely", `{"region":"eu-west-1"}`},
		{"signaling_channel null", `{"region":"eu-west-1","signaling_channel":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := newClient(hostOf(t, srv.URL))
			c.httpClient = srv.Client()
			c.token = "tok-1"
			defer c.Close()

			info, err := c.KVSInfo(context.Background(), "event-1")
			if err != nil {
				t.Fatalf("KVSInfo returned an error for a defensively-handleable shape: %v", err)
			}
			if info.ChannelName != "" {
				t.Fatalf("ChannelName = %q, want empty", info.ChannelName)
			}
		})
	}
}

func TestClient_KVSToken_MeasuredShape(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"identity_id":"eu-west-1:abc-123","token":"eyJ..."}`))
	}))
	defer srv.Close()

	c := newClient(hostOf(t, srv.URL))
	c.httpClient = srv.Client()
	c.token = "tok-1"
	defer c.Close()

	tok, err := c.KVSToken(context.Background(), "event-1")
	if err != nil {
		t.Fatalf("KVSToken: %v", err)
	}
	if tok.IdentityID != "eu-west-1:abc-123" || tok.Token != "eyJ..." {
		t.Fatalf("KVSToken = %+v", tok)
	}
}

func TestClient_Refresh_UpdatesToken(t *testing.T) {
	calls := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != refreshPath {
			t.Fatalf("path = %q, want %q", r.URL.Path, refreshPath)
		}
		calls++
		if got := r.Header.Get("Authorization"); got != "Bearer old-token" {
			t.Fatalf("Authorization = %q, want the pre-refresh token", got)
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": "new-token"})
	}))
	defer srv.Close()

	c := newClient(hostOf(t, srv.URL))
	c.httpClient = srv.Client()
	c.token = "old-token"
	c.alias = "commentator1"
	c.password = "hunter2"
	defer c.Close()

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := c.Token(); got != "new-token" {
		t.Fatalf("Token() = %q, want new-token", got)
	}
	if calls != 1 {
		t.Fatalf("refresh endpoint called %d times, want 1", calls)
	}
}

// TestClient_Refresh_FallsBackToSignIn covers the documented contract: "The
// refresh token's own TTL is unmeasured, so Refresh failing must fall back
// to a full SignIn rather than giving up" (m2lx.go, Client.Refresh).
func TestClient_Refresh_FallsBackToSignIn(t *testing.T) {
	var signInCalls, refreshCalls int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case refreshPath:
			refreshCalls++
			w.WriteHeader(http.StatusUnauthorized)
		case signInPath:
			signInCalls++
			var body signInRequest
			json.NewDecoder(r.Body).Decode(&body)
			if body.Alias != "commentator1" {
				t.Fatalf("fallback sign-in alias = %q, want commentator1", body.Alias)
			}
			json.NewEncoder(w).Encode(map[string]string{"access_token": "fresh-from-signin"})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := newClient(hostOf(t, srv.URL))
	c.httpClient = srv.Client()
	c.token = "old-token"
	c.alias = "commentator1"
	c.password = "hunter2"
	defer c.Close()

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh endpoint called %d times, want 1", refreshCalls)
	}
	if signInCalls != 1 {
		t.Fatalf("sign-in endpoint (fallback) called %d times, want 1", signInCalls)
	}
	if got := c.Token(); got != "fresh-from-signin" {
		t.Fatalf("Token() = %q, want fresh-from-signin", got)
	}
}

// TestClient_Refresh_BothFailClearsTokenAndReturnsErrNotSignedIn covers the
// ErrNotSignedIn doc comment in m2lx.go: "...or after the token has expired
// and Refresh has failed."
func TestClient_Refresh_BothFailClearsTokenAndReturnsErrNotSignedIn(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newClient(hostOf(t, srv.URL))
	c.httpClient = srv.Client()
	c.token = "old-token"
	c.alias = "commentator1"
	c.password = "wrong-password-now"
	defer c.Close()

	err := c.Refresh(context.Background())
	if !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("Refresh error = %v, want it to wrap ErrNotSignedIn", err)
	}
	if got := c.Token(); got != "" {
		t.Fatalf("Token() = %q after refresh and fallback both failed, want empty", got)
	}
}

func TestClient_Refresh_WithoutPriorSignInIsErrNotSignedIn(t *testing.T) {
	c := newClient("example.invalid")
	defer c.Close()
	if err := c.Refresh(context.Background()); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("Refresh() = %v, want ErrNotSignedIn", err)
	}
}

// TestClient_PasswordAndTokenNeverAppearInErrors is a light guard against
// the "never log the password or the token" requirement leaking through
// error strings, which are the one place this package produces
// human-readable text about a request.
func TestClient_PasswordAndTokenNeverAppearInErrors(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newClient(hostOf(t, srv.URL))
	c.httpClient = srv.Client()
	defer c.Close()

	const password = "sUpEr-SecreT-99"
	err := c.SignIn(context.Background(), "commentator1", password)
	if err == nil {
		t.Fatalf("expected an error")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("error message leaked the password: %v", err)
	}
}
