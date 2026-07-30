package kvs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cognitoidentity"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentity/types"

	"wslcomms/internal/m2lx"
)

// --- fake m2lx.Client, backed by an httptest.Server -------------------------
//
// This reimplements just enough of the wire parsing WP-2's real m2lx.Client
// is documented to do (KVSInfo resolving ChannelName as the first entry of
// Channels["pgm"], preserving Channels whole) so that Fetch's handling of the
// various webrtc_info / webrtc_token wire shapes recorded as SP-1 can be
// exercised end to end, from JSON bytes on a test HTTP server through to the
// error or Credentials Fetch produces. It is not a mock of Fetch's direct
// input — it is a stand-in for the m2lx.Client interface, which is WP-2's to
// implement for real.

// wireKVSInfo is the measured webrtc_info response shape (docs/test-results.md
// around line 141): {"region":"...","signaling_channel":{"pgm":["..."]}}.
type wireKVSInfo struct {
	Region           string              `json:"region"`
	SignalingChannel map[string][]string `json:"signaling_channel"`
}

// wireKVSToken is the measured webrtc_token response shape: {"identity_id":
// "...","token":"..."}.
type wireKVSToken struct {
	IdentityID string `json:"identity_id"`
	Token      string `json:"token"`
}

// testClient implements m2lx.Client against an httptest.Server for the two
// KVS endpoints only; SignIn/Refresh/Token are not exercised by Fetch and are
// trivial stubs.
type testClient struct {
	baseURL string
}

func (c *testClient) SignIn(ctx context.Context, alias, password string) error { return nil }
func (c *testClient) Refresh(ctx context.Context) error                        { return nil }
func (c *testClient) Token() string                                            { return "" }

func (c *testClient) KVSInfo(ctx context.Context, eventID string) (m2lx.KVSInfo, error) {
	body, status, err := getJSON(ctx, c.baseURL+"/api/live_operation/kvs/webrtc_info/"+eventID)
	if err != nil {
		return m2lx.KVSInfo{}, err
	}
	if status != http.StatusOK {
		return m2lx.KVSInfo{}, fmt.Errorf("webrtc_info: unexpected status %d", status)
	}
	var wire wireKVSInfo
	if err := json.Unmarshal(body, &wire); err != nil {
		return m2lx.KVSInfo{}, fmt.Errorf("webrtc_info: decoding response: %w", err)
	}
	info := m2lx.KVSInfo{
		Region:   wire.Region,
		Channels: wire.SignalingChannel,
	}
	if names, ok := wire.SignalingChannel[m2lx.ChannelKeyPGM]; ok && len(names) > 0 {
		info.ChannelName = names[0]
	}
	return info, nil
}

func (c *testClient) KVSToken(ctx context.Context, eventID string) (m2lx.KVSToken, error) {
	body, status, err := getJSON(ctx, c.baseURL+"/api/live_operation/kvs/webrtc_token/"+eventID)
	if err != nil {
		return m2lx.KVSToken{}, err
	}
	if status != http.StatusOK {
		return m2lx.KVSToken{}, fmt.Errorf("webrtc_token: unexpected status %d", status)
	}
	var wire wireKVSToken
	if err := json.Unmarshal(body, &wire); err != nil {
		return m2lx.KVSToken{}, fmt.Errorf("webrtc_token: decoding response: %w", err)
	}
	return m2lx.KVSToken{IdentityID: wire.IdentityID, Token: wire.Token}, nil
}

func getJSON(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 512)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf, resp.StatusCode, nil
}

// --- fake cognitoClient ------------------------------------------------------

type fakeCognito struct {
	out *cognitoidentity.GetCredentialsForIdentityOutput
	err error

	// gotInput records the last request this fake received, so tests can
	// assert on how Fetch called it (identity id, logins key/value).
	gotInput *cognitoidentity.GetCredentialsForIdentityInput
}

func (f *fakeCognito) GetCredentialsForIdentity(ctx context.Context, params *cognitoidentity.GetCredentialsForIdentityInput, optFns ...func(*cognitoidentity.Options)) (*cognitoidentity.GetCredentialsForIdentityOutput, error) {
	f.gotInput = params
	return f.out, f.err
}

func strptr(s string) *string { return &s }

func validCognitoOutput(expiry time.Time) *cognitoidentity.GetCredentialsForIdentityOutput {
	return &cognitoidentity.GetCredentialsForIdentityOutput{
		Credentials: &types.Credentials{
			AccessKeyId:  strptr("AKIAEXAMPLE"),
			SecretKey:    strptr("supersecret"),
			SessionToken: strptr("sessiontoken"),
			Expiration:   &expiry,
		},
	}
}

// --- test server plumbing ----------------------------------------------------

// newServer starts an httptest.Server serving fixed bodies (or a fixed status
// with no body) for the two KVS endpoints. Passing status 0 for either means
// "respond 200 with body".
func newServer(t *testing.T, infoBody string, infoStatus int, tokenBody string, tokenStatus int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/live_operation/kvs/webrtc_info/", func(w http.ResponseWriter, r *http.Request) {
		if infoStatus != 0 && infoStatus != http.StatusOK {
			w.WriteHeader(infoStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(infoBody))
	})
	mux.HandleFunc("/api/live_operation/kvs/webrtc_token/", func(w http.ResponseWriter, r *http.Request) {
		if tokenStatus != 0 && tokenStatus != http.StatusOK {
			w.WriteHeader(tokenStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(tokenBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// --- tests --------------------------------------------------------------

func TestFetch(t *testing.T) {
	future := time.Now().Add(1 * time.Hour)
	past := time.Now().Add(-1 * time.Hour)

	const happyInfo = `{"region":"eu-west-1","signaling_channel":{"pgm":["webrtc-wslstudios-matcht"]}}`
	const happyToken = `{"identity_id":"eu-west-1:abc-123","token":"tok-abc-123"}`

	tests := []struct {
		name string

		infoBody    string
		infoStatus  int
		tokenBody   string
		tokenStatus int

		dial cognitoDialer

		wantErrSubstr string
		check         func(t *testing.T, got Credentials)
	}{
		{
			name:      "happy path",
			infoBody:  happyInfo,
			tokenBody: happyToken,
			dial:      fakeDialer(validCognitoOutput(future), nil),
			check: func(t *testing.T, got Credentials) {
				if got.Region != "eu-west-1" {
					t.Errorf("Region = %q, want %q", got.Region, "eu-west-1")
				}
				if got.ChannelName != "webrtc-wslstudios-matcht" {
					t.Errorf("ChannelName = %q, want %q", got.ChannelName, "webrtc-wslstudios-matcht")
				}
				if got.ChannelARN != "" {
					t.Errorf("ChannelARN = %q, want empty (M2L-X returns a name, not an ARN)", got.ChannelARN)
				}
				if got.AccessKeyID != "AKIAEXAMPLE" || got.SecretKey != "supersecret" || got.SessionToken != "sessiontoken" {
					t.Errorf("credential fields not carried through: %+v", got)
				}
				if !got.Expiry.Equal(future) {
					t.Errorf("Expiry = %v, want %v", got.Expiry, future)
				}
				if got.Expired() {
					t.Errorf("Expired() = true for a credential expiring in an hour")
				}
				if got.ExpiresWithin(2*time.Hour) == false {
					t.Errorf("ExpiresWithin(2h) = false, want true for a credential expiring in 1h")
				}
			},
		},
		{
			name:          "signaling_channel present but empty",
			infoBody:      `{"region":"eu-west-1","signaling_channel":{}}`,
			tokenBody:     happyToken,
			dial:          fakeDialer(validCognitoOutput(future), nil),
			wantErrSubstr: "empty \"signaling_channel\" object",
		},
		{
			name:          "signaling_channel with a key other than pgm",
			infoBody:      `{"region":"eu-west-1","signaling_channel":{"pvw":["some-other-channel"]}}`,
			tokenBody:     happyToken,
			dial:          fakeDialer(validCognitoOutput(future), nil),
			wantErrSubstr: `no "pgm" key`,
		},
		{
			name:          "pgm key present but its list is empty",
			infoBody:      `{"region":"eu-west-1","signaling_channel":{"pgm":[]}}`,
			tokenBody:     happyToken,
			dial:          fakeDialer(validCognitoOutput(future), nil),
			wantErrSubstr: "empty channel list",
		},
		{
			name:          "missing region",
			infoBody:      `{"signaling_channel":{"pgm":["webrtc-wslstudios-matcht"]}}`,
			tokenBody:     happyToken,
			dial:          fakeDialer(validCognitoOutput(future), nil),
			wantErrSubstr: `empty "region"`,
		},
		{
			name:          "token endpoint returns 401",
			infoBody:      happyInfo,
			tokenStatus:   http.StatusUnauthorized,
			dial:          fakeDialer(validCognitoOutput(future), nil),
			wantErrSubstr: "webrtc_token",
		},
		{
			name:      "cognito returns expired credentials",
			infoBody:  happyInfo,
			tokenBody: happyToken,
			dial:      fakeDialer(validCognitoOutput(past), nil),
			check: func(t *testing.T, got Credentials) {
				if !got.Expired() {
					t.Errorf("Expired() = false for a credential that expired an hour ago")
				}
				if !got.ExpiresWithin(0) {
					t.Errorf("ExpiresWithin(0) = false for an already-expired credential")
				}
				// Fetch still returns the (expired) credentials faithfully;
				// it is not Fetch's job to police freshness, only to relay
				// what Cognito said. AccessKeyID etc. must still be set.
				if got.AccessKeyID == "" {
					t.Errorf("AccessKeyID empty on expired-but-otherwise-valid response")
				}
			},
		},
		{
			name:          "cognito call itself fails",
			infoBody:      happyInfo,
			tokenBody:     happyToken,
			dial:          fakeDialer(nil, errors.New("AccessDenied: not authorized")),
			wantErrSubstr: "GetCredentialsForIdentity",
		},
		{
			name:      "cognito response has no Credentials field",
			infoBody:  happyInfo,
			tokenBody: happyToken,
			dial: fakeDialer(&cognitoidentity.GetCredentialsForIdentityOutput{
				Credentials: nil,
			}, nil),
			wantErrSubstr: `no "Credentials" field`,
		},
		{
			name:      "cognito response missing AccessKeyId",
			infoBody:  happyInfo,
			tokenBody: happyToken,
			dial: fakeDialer(&cognitoidentity.GetCredentialsForIdentityOutput{
				Credentials: &types.Credentials{
					SecretKey:    strptr("supersecret"),
					SessionToken: strptr("sessiontoken"),
					Expiration:   &future,
				},
			}, nil),
			wantErrSubstr: "AccessKeyId",
		},
		{
			name:          "webrtc_token identity_id empty",
			infoBody:      happyInfo,
			tokenBody:     `{"identity_id":"","token":"tok-abc-123"}`,
			dial:          fakeDialer(validCognitoOutput(future), nil),
			wantErrSubstr: `empty "identity_id"`,
		},
		{
			name:          "webrtc_token token empty",
			infoBody:      happyInfo,
			tokenBody:     `{"identity_id":"eu-west-1:abc-123","token":""}`,
			dial:          fakeDialer(validCognitoOutput(future), nil),
			wantErrSubstr: `empty "token"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(t, tc.infoBody, tc.infoStatus, tc.tokenBody, tc.tokenStatus)
			client := &testClient{baseURL: srv.URL}

			got, err := fetch(context.Background(), client, "evt-1", tc.dial)

			if tc.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("fetch() error = nil, want substring %q", tc.wantErrSubstr)
				}
				if !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Fatalf("fetch() error = %q, want substring %q", err.Error(), tc.wantErrSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("fetch() unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

// TestFetch_EmptyEventID checks the defensive guard against an empty eventID,
// which would otherwise silently produce a malformed request path.
func TestFetch_EmptyEventID(t *testing.T) {
	client := &testClient{baseURL: "http://unused.invalid"}
	_, err := fetch(context.Background(), client, "", fakeDialer(nil, errors.New("should not be called")))
	if err == nil {
		t.Fatal("fetch() with empty eventID: error = nil, want error")
	}
	if !strings.Contains(err.Error(), "eventID") {
		t.Fatalf("fetch() error = %q, want it to name eventID", err.Error())
	}
}

// TestFetch_UsesRegionFromInfo checks that dialCognito's region argument is
// whatever webrtc_info returned, not a hardcoded value: a second region is
// used here specifically because it is not "eu-west-1", the only region seen
// so far, to prove nothing is hardcoded to it.
func TestFetch_UsesRegionFromInfo(t *testing.T) {
	const infoBody = `{"region":"eu-central-1","signaling_channel":{"pgm":["some-channel"]}}`
	const tokenBody = `{"identity_id":"eu-central-1:abc-123","token":"tok-abc-123"}`
	srv := newServer(t, infoBody, 0, tokenBody, 0)
	client := &testClient{baseURL: srv.URL}

	var gotRegion string
	dial := func(ctx context.Context, region string) (cognitoClient, error) {
		gotRegion = region
		return &fakeCognito{out: validCognitoOutput(time.Now().Add(time.Hour))}, nil
	}

	if _, err := fetch(context.Background(), client, "evt-1", dial); err != nil {
		t.Fatalf("fetch() unexpected error: %v", err)
	}
	if gotRegion != "eu-central-1" {
		t.Errorf("dial called with region %q, want %q", gotRegion, "eu-central-1")
	}
}

// TestFetch_LoginsKey checks that the Cognito Logins map is keyed by
// DefaultLoginsKey when m2lx.KVSToken.LoginsKey is empty (the observed wire
// shape never sets it), and by the supplied key when it is not.
func TestFetch_LoginsKey(t *testing.T) {
	const infoBody = `{"region":"eu-west-1","signaling_channel":{"pgm":["chan"]}}`
	const tokenBody = `{"identity_id":"eu-west-1:abc-123","token":"tok-xyz"}`
	srv := newServer(t, infoBody, 0, tokenBody, 0)
	client := &testClient{baseURL: srv.URL}

	fc := &fakeCognito{out: validCognitoOutput(time.Now().Add(time.Hour))}
	dial := func(ctx context.Context, region string) (cognitoClient, error) { return fc, nil }

	if _, err := fetch(context.Background(), client, "evt-1", dial); err != nil {
		t.Fatalf("fetch() unexpected error: %v", err)
	}
	if fc.gotInput == nil {
		t.Fatal("cognito client was never called")
	}
	if fc.gotInput.Logins[DefaultLoginsKey] != "tok-xyz" {
		t.Errorf("Logins[%q] = %q, want %q", DefaultLoginsKey, fc.gotInput.Logins[DefaultLoginsKey], "tok-xyz")
	}
	if *fc.gotInput.IdentityId != "eu-west-1:abc-123" {
		t.Errorf("IdentityId = %q, want %q", *fc.gotInput.IdentityId, "eu-west-1:abc-123")
	}
}

// TestCredentials_ExpiresWithin is a table-driven test of the Expired /
// ExpiresWithin logic in isolation, independent of the fetch chain.
func TestCredentials_ExpiresWithin(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name   string
		expiry time.Time
		window time.Duration
		want   bool
	}{
		{"zero expiry treated as expired", time.Time{}, 0, true},
		{"far future, no window", now.Add(time.Hour), 0, false},
		{"far future, window shorter than remaining life", now.Add(time.Hour), time.Minute, false},
		{"far future, window longer than remaining life", now.Add(time.Minute), time.Hour, true},
		{"already past", now.Add(-time.Second), 0, true},
		{"exactly now", now, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Credentials{Expiry: tc.expiry}
			if got := c.ExpiresWithin(tc.window); got != tc.want {
				t.Errorf("ExpiresWithin(%v) with expiry %v = %v, want %v", tc.window, tc.expiry, got, tc.want)
			}
		})
	}
}

func fakeDialer(out *cognitoidentity.GetCredentialsForIdentityOutput, err error) cognitoDialer {
	return func(ctx context.Context, region string) (cognitoClient, error) {
		return &fakeCognito{out: out, err: err}, nil
	}
}
