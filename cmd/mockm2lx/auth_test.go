package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newAuthTestApp returns an App wired for HTTP handler tests: no listeners,
// just the App and its handlers exercised directly with httptest.
func newAuthTestApp(t *testing.T) *App {
	t.Helper()
	opts := Options{
		Alias:          "wsl-comms-ro",
		Password:       "s3cret",
		StatusKey:      "cam7",
		OnePeerOnly:    true,
		RefusalWindow:  6 * time.Second,
		StatusInterval: time.Hour,
		TokenTTL:       time.Hour,
	}
	return NewApp(opts, log.New(newTestWriter(t), "", 0))
}

func doJSON(t *testing.T, h http.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestSignIn_UsernameFieldReturns500(t *testing.T) {
	// CONTRACT.md's headline regression test: a client that regresses to
	// sending "username" instead of "alias" must be caught here, at HTTP
	// 500, exactly as docs/windows-app-spec.md line 78 states the real
	// instance behaves.
	a := newAuthTestApp(t)
	rec := doJSON(t, a.handleSignIn, "POST", `{"username":"wsl-comms-ro","password":"s3cret"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestSignIn_CorrectAliasAndPasswordSucceeds(t *testing.T) {
	a := newAuthTestApp(t)
	rec := doJSON(t, a.handleSignIn, "POST", `{"alias":"wsl-comms-ro","password":"s3cret"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var resp authResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.AccessToken == "" {
		t.Errorf("expected a non-empty access_token")
	}
	if resp.RefreshToken == "" {
		t.Errorf("expected a non-empty refresh_token")
	}
	if resp.ExpiresIn <= 0 {
		t.Errorf("expires_in = %d, want > 0", resp.ExpiresIn)
	}
	if len(resp.RoleIDs) == 0 {
		t.Errorf("expected a non-empty roleIds")
	}

	if _, ok := a.checkToken(resp.AccessToken); !ok {
		t.Errorf("the returned access_token does not validate")
	}
}

func TestSignIn_WrongPasswordReturns400(t *testing.T) {
	a := newAuthTestApp(t)
	rec := doJSON(t, a.handleSignIn, "POST", `{"alias":"wsl-comms-ro","password":"wrong"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSignIn_MissingAliasWithoutUsernameReturns400(t *testing.T) {
	a := newAuthTestApp(t)
	rec := doJSON(t, a.handleSignIn, "POST", `{"password":"s3cret"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (not 500 — only the \"username\" shape is the measured 500)", rec.Code)
	}
}

func TestSignIn_MalformedJSONReturns400(t *testing.T) {
	a := newAuthTestApp(t)
	rec := doJSON(t, a.handleSignIn, "POST", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSignIn_AliasPresentAlongsideUsernameStillSucceeds(t *testing.T) {
	// "instead of" in the spec's wording means alias ABSENT; a body that
	// (defensively, incorrectly) sends both must not trip the 500, since
	// alias is present and that's what the real field is.
	a := newAuthTestApp(t)
	rec := doJSON(t, a.handleSignIn, "POST", `{"alias":"wsl-comms-ro","username":"wsl-comms-ro","password":"s3cret"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func signIn(t *testing.T, a *App) authResponse {
	t.Helper()
	rec := doJSON(t, a.handleSignIn, "POST", `{"alias":"wsl-comms-ro","password":"s3cret"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign-in failed: status %d, body %s", rec.Code, rec.Body.String())
	}
	var resp authResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode sign-in response: %v", err)
	}
	return resp
}

func TestRefresh_ValidRefreshTokenRotatesAndReturnsNewToken(t *testing.T) {
	a := newAuthTestApp(t)
	first := signIn(t, a)

	rec := doJSON(t, a.handleRefreshToken, "POST", `{"refresh_token":"`+first.RefreshToken+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var second authResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode refresh response: %v", err)
	}

	if second.AccessToken == first.AccessToken {
		t.Errorf("expected a new access_token on refresh")
	}
	if second.RefreshToken == first.RefreshToken {
		t.Errorf("expected a new refresh_token on refresh (rotation)")
	}
	if _, ok := a.checkToken(second.AccessToken); !ok {
		t.Errorf("the new access_token does not validate")
	}
	if _, ok := a.checkToken(first.AccessToken); ok {
		t.Errorf("the OLD access_token must be invalidated by a refresh")
	}
}

func TestRefresh_StaleRefreshTokenFailsAfterRotation(t *testing.T) {
	a := newAuthTestApp(t)
	first := signIn(t, a)

	// Refresh once — this rotates first.RefreshToken out.
	rec := doJSON(t, a.handleRefreshToken, "POST", `{"refresh_token":"`+first.RefreshToken+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first refresh failed: %d", rec.Code)
	}

	// Reusing the now-stale refresh token must fail — a client that does
	// not persist the freshly rotated refresh_token must not silently keep
	// working.
	rec2 := doJSON(t, a.handleRefreshToken, "POST", `{"refresh_token":"`+first.RefreshToken+`"}`)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("reusing a stale refresh_token: status = %d, want 401", rec2.Code)
	}
}

func TestRefresh_UnknownRefreshTokenReturns401(t *testing.T) {
	a := newAuthTestApp(t)
	rec := doJSON(t, a.handleRefreshToken, "POST", `{"refresh_token":"this-was-never-issued"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRefresh_EmptyRefreshTokenReturns400(t *testing.T) {
	a := newAuthTestApp(t)
	rec := doJSON(t, a.handleRefreshToken, "POST", `{"refresh_token":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCheckToken_ExpiredTokenIsRejected(t *testing.T) {
	a := newAuthTestApp(t)
	a.opts.TokenTTL = 10 * time.Millisecond
	resp := signIn(t, a)

	time.Sleep(30 * time.Millisecond)

	if _, ok := a.checkToken(resp.AccessToken); ok {
		t.Errorf("expected an expired token to be rejected")
	}
}

func TestCheckToken_EmptyTokenIsRejected(t *testing.T) {
	a := newAuthTestApp(t)
	if _, ok := a.checkToken(""); ok {
		t.Errorf("expected an empty token to be rejected")
	}
}

func TestExpireTokens_MakesTokenExpireEarly(t *testing.T) {
	// This is the /control/expire-token fault: "make the token expire
	// early, to exercise refresh and WebSocket reopen".
	a := newAuthTestApp(t)
	resp := signIn(t, a)

	if _, ok := a.checkToken(resp.AccessToken); !ok {
		t.Fatalf("token should be valid immediately after sign-in")
	}

	n := a.expireTokens(0)
	if n != 1 {
		t.Fatalf("expireTokens reported %d sessions affected, want 1", n)
	}

	if _, ok := a.checkToken(resp.AccessToken); ok {
		t.Errorf("expected the token to be expired immediately")
	}
}

func TestRequireAuth_RejectsMissingAndAcceptsValidBearer(t *testing.T) {
	a := newAuthTestApp(t)
	resp := signIn(t, a)

	called := false
	inner := func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusOK) }
	h := a.requireAuth(inner)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no Authorization header: status = %d, want 401", rec.Code)
	}
	if called {
		t.Errorf("inner handler must not run without a valid token")
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.Header.Set("Authorization", "Bearer "+resp.AccessToken)
	h(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("valid Authorization header: status = %d, want 200", rec2.Code)
	}
	if !called {
		t.Errorf("inner handler should have run with a valid token")
	}
}
