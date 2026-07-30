// KVS credential endpoints:
//
//	GET /api/live_operation/kvs/webrtc_info/{event}
//	GET /api/live_operation/kvs/webrtc_token/{event}
//
// Both return the exact shapes of the single measured sample recorded in
// docs/test-results.md line 141 (also quoted in CONTRACT.md's internal/kvs
// section):
//
//	{"region":"eu-west-1","signaling_channel":{"pgm":["webrtc-wslstudios-matcht"]}}
//	{identity_id, token}
//
// This is one sample from one instance (SP-1, CONTRACT.md "Known unknowns").
// The mock reproduces exactly that sample rather than inventing a richer one,
// so that WP-4 and WP-5a are tested against the same shape the spec is
// written against, not against a shape this package guessed at.
//
// Both endpoints require the same bearer token as everything else under
// /api/live_operation and /api/local_auth — real read-only credentials are
// good for GET everywhere (docs/archive-windows-app-spec-v1-rejected.md line
// 910), and there is no reason for the mock to be looser than that.
package main

import "net/http"

// kvsInfoResponse is the wire shape of GET
// /api/live_operation/kvs/webrtc_info/{event}.
type kvsInfoResponse struct {
	Region           string              `json:"region"`
	SignalingChannel map[string][]string `json:"signaling_channel"`
}

// kvsTokenResponse is the wire shape of GET
// /api/live_operation/kvs/webrtc_token/{event}.
type kvsTokenResponse struct {
	IdentityID string `json:"identity_id"`
	Token      string `json:"token"`
}

// handleKVSInfo implements GET /api/live_operation/kvs/webrtc_info/{event}.
// The eventId path segment is accepted but not validated against any known
// list — on the real instance it names a live event, which this mock has no
// concept of.
func (a *App) handleKVSInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, kvsInfoResponse{
		Region: "eu-west-1",
		SignalingChannel: map[string][]string{
			"pgm": {"webrtc-wslstudios-matcht"},
		},
	})
}

// handleKVSToken implements GET /api/live_operation/kvs/webrtc_token/{event}.
// IdentityID and Token are fabricated: they are opaque strings the frontend
// hands straight to Cognito's GetCredentialsForIdentity, and this mock has no
// AWS account behind it for a real one to mean anything. What matters for
// testing WP-4/WP-5a is the field NAMES and the fact that both are non-empty.
func (a *App) handleKVSToken(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, kvsTokenResponse{
		IdentityID: "eu-west-1:mock-" + randomToken(6),
		Token:      "mock-openid-token." + randomToken(24),
	})
}
