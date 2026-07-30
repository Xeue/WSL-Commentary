package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wslcomms/internal/m2lx"
)

func TestControl_OnePeerOnlyToggle(t *testing.T) {
	a := newAuthTestApp(t)
	if !a.getOnePeerOnly() {
		t.Fatalf("expected the default to be true")
	}

	rec := doJSON(t, a.handleControlOnePeerOnly, "POST", `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if a.getOnePeerOnly() {
		t.Errorf("expected one-peer-only to be false after the control call")
	}
}

func TestControl_RefusalWindow(t *testing.T) {
	a := newAuthTestApp(t)

	rec := doJSON(t, a.handleControlRefusalWindow, "POST", `{"seconds":2.5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if got := a.getRefusalWindow(); got != 2500*time.Millisecond {
		t.Errorf("refusal window = %s, want 2.5s", got)
	}
}

func TestControl_RefusalWindowRejectsNegative(t *testing.T) {
	a := newAuthTestApp(t)
	rec := doJSON(t, a.handleControlRefusalWindow, "POST", `{"seconds":-1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestControl_StallStatusToggle(t *testing.T) {
	a := newAuthTestApp(t)
	doJSON(t, a.handleControlStallStatus, "POST", `{"enabled":true}`)
	if !a.getStallStatus() {
		t.Errorf("expected stall-status to be true after the control call")
	}
	doJSON(t, a.handleControlStallStatus, "POST", `{"enabled":false}`)
	if a.getStallStatus() {
		t.Errorf("expected stall-status to be false after the second control call")
	}
}

func TestControl_DropAudioToggle(t *testing.T) {
	a := newAuthTestApp(t)
	doJSON(t, a.handleControlDropAudio, "POST", `{"enabled":true}`)
	if !a.getDropAudio() {
		t.Errorf("expected drop-audio to be true after the control call")
	}
}

func TestControl_LieAcceptsValidValuesAndClearsOnEmpty(t *testing.T) {
	a := newAuthTestApp(t)

	tests := []struct {
		body string
		want string
	}{
		{`{"streamState":"streaming"}`, m2lx.StreamStateStreaming},
		{`{"streamState":"starting"}`, m2lx.StreamStateStarting},
		{`{"streamState":"stopped"}`, m2lx.StreamStateStopped},
		{`{"streamState":""}`, ""},
		{`{"streamState":"streaming"}`, m2lx.StreamStateStreaming},
		{`{"streamState":"auto"}`, ""},
	}
	for _, tc := range tests {
		rec := doJSON(t, a.handleControlLie, "POST", tc.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("body %s: status = %d, want 200", tc.body, rec.Code)
		}
		if got := a.getLieStreamState(); got != tc.want {
			t.Errorf("body %s: lieStreamState = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestControl_LieRejectsInvalidValue(t *testing.T) {
	a := newAuthTestApp(t)
	rec := doJSON(t, a.handleControlLie, "POST", `{"streamState":"not-a-real-state"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestControl_DropSRTReportsFalseWhenNothingConnected(t *testing.T) {
	a := newAuthTestApp(t)
	rec := doJSON(t, a.handleControlDropSRT, "POST", ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"dropped":false`) {
		t.Errorf("expected dropped:false, got %s", rec.Body.String())
	}
}

func TestControl_ExpireTokenExpiresLiveSessions(t *testing.T) {
	a := newAuthTestApp(t)
	tok := signIn(t, a)

	rec := doJSON(t, a.handleControlExpireToken, "POST", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if _, ok := a.checkToken(tok.AccessToken); ok {
		t.Errorf("expected the token to be expired")
	}
}

func TestControl_ExpireTokenRejectsBadDuration(t *testing.T) {
	a := newAuthTestApp(t)
	rec := doJSON(t, a.handleControlExpireToken, "POST", `{"in":"not-a-duration"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestControl_Reset(t *testing.T) {
	a := newAuthTestApp(t)
	a.setOnePeerOnly(false)
	a.setStallStatus(true)
	a.setDropAudio(true)
	a.setLieStreamState(m2lx.StreamStateStreaming)
	a.setRefusalWindow(99 * time.Second)

	rec := doJSON(t, a.handleControlReset, "POST", ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if got := a.getOnePeerOnly(); got != a.opts.OnePeerOnly {
		t.Errorf("onePeerOnly = %v, want startup default %v", got, a.opts.OnePeerOnly)
	}
	if got := a.getStallStatus(); got != a.opts.StallStatus {
		t.Errorf("stallStatus = %v, want startup default %v", got, a.opts.StallStatus)
	}
	if got := a.getDropAudio(); got != a.opts.DropAudio {
		t.Errorf("dropAudio = %v, want startup default %v", got, a.opts.DropAudio)
	}
	if got := a.getLieStreamState(); got != a.opts.LieStreamState {
		t.Errorf("lieStreamState = %q, want startup default %q", got, a.opts.LieStreamState)
	}
	if got := a.getRefusalWindow(); got != a.opts.RefusalWindow {
		t.Errorf("refusalWindow = %s, want startup default %s", got, a.opts.RefusalWindow)
	}
}

func TestControl_State(t *testing.T) {
	a := newAuthTestApp(t)
	signIn(t, a)

	rec := httptest.NewRecorder()
	a.handleControlState(rec, httptest.NewRequest("GET", "/control/state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"onePeerOnly"`, `"refusalWindow"`, `"srt"`, `"sessions":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %s in /control/state response, got: %s", want, body)
		}
	}
}

func TestDecodeJSONBody_EmptyBodyIsNotAnError(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", nil)
	var dst struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSONBody(rec, req, &dst) {
		t.Fatalf("expected an empty body to be accepted")
	}
	if dst.Enabled {
		t.Errorf("expected the zero value on an empty body")
	}
}

func TestDecodeJSONBody_InvalidJSONIs400(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{not json`))
	var dst struct{}
	if decodeJSONBody(rec, req, &dst) {
		t.Fatalf("expected invalid JSON to be rejected")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
