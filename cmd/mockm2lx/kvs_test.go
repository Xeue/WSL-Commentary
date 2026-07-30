package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestKVSInfo_ReturnsMeasuredShape(t *testing.T) {
	// docs/test-results.md line 141:
	// {"region":"eu-west-1","signaling_channel":{"pgm":["webrtc-wslstudios-matcht"]}}
	a := newAuthTestApp(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/live_operation/kvs/webrtc_info/ev1", nil)
	a.handleKVSInfo(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp kvsInfoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Region != "eu-west-1" {
		t.Errorf("region = %q, want %q", resp.Region, "eu-west-1")
	}
	pgm, ok := resp.SignalingChannel["pgm"]
	if !ok || len(pgm) != 1 || pgm[0] != "webrtc-wslstudios-matcht" {
		t.Errorf("signaling_channel[\"pgm\"] = %v, want [\"webrtc-wslstudios-matcht\"]", pgm)
	}

	// Field name check: this must be "signaling_channel" on the wire.
	if !bytesContainField(rec.Body.Bytes(), "signaling_channel") {
		t.Errorf("expected the wire field \"signaling_channel\", got: %s", rec.Body.String())
	}
}

func TestKVSToken_ReturnsMeasuredFieldNames(t *testing.T) {
	a := newAuthTestApp(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/live_operation/kvs/webrtc_token/ev1", nil)
	a.handleKVSToken(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp kvsTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.IdentityID == "" {
		t.Errorf("expected a non-empty identity_id")
	}
	if resp.Token == "" {
		t.Errorf("expected a non-empty token")
	}
	if !bytesContainField(rec.Body.Bytes(), "identity_id") {
		t.Errorf("expected the wire field \"identity_id\", got: %s", rec.Body.String())
	}
}

func TestKVSEndpoints_RequireAuth(t *testing.T) {
	a := newAuthTestApp(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/live_operation/kvs/webrtc_info/{event}", a.requireAuth(a.handleKVSInfo))
	mux.HandleFunc("GET /api/live_operation/kvs/webrtc_token/{event}", a.requireAuth(a.handleKVSToken))

	for _, path := range []string{
		"/api/live_operation/kvs/webrtc_info/ev1",
		"/api/live_operation/kvs/webrtc_token/ev1",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token: status = %d, want 401", path, rec.Code)
		}
	}

	tok := signIn(t, a)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/live_operation/kvs/webrtc_info/ev1", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("with a valid token: status = %d, want 200", rec.Code)
	}
}

func bytesContainField(b []byte, field string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return false
	}
	_, ok := m[field]
	return ok
}
