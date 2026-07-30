// Package kvs turns an M2L-X event into the temporary AWS credentials the
// embedded monitor page needs to join the event's Kinesis Video Streams WebRTC
// signalling channel as a viewer.
//
// Owner: WP-4. No other work package writes files in this directory.
//
// The chain is three calls, exactly what Sony's own GUI does:
//
//  1. GET /api/live_operation/kvs/webrtc_info/{eventId}   — region, channel NAME
//  2. GET /api/live_operation/kvs/webrtc_token/{eventId}  — Cognito identity, token
//  3. Cognito GetCredentialsForIdentity                   — temporary credentials
//
// Everything after that — DescribeSignalingChannel to turn the channel name into
// an ARN, GetSignalingChannelEndpoint with role VIEWER, GetIceServerConfig, the
// SigV4-presigned WSS connect, the eight recvonly transceivers — happens in the
// frontend, because AWS ships a supported KVS WebRTC signalling client for
// JavaScript and none for Go. That is why go.mod has the Cognito client but no
// kinesisvideo client, and frontend/package.json has both KVS clients.
//
// Calls 1 and 2 are m2lx.Client's job (owned by WP-2) and already declared:
// this package calls them and treats their normalised m2lx.KVSInfo /
// m2lx.KVSToken as input rather than re-parsing wire JSON itself. Call 3 is
// this package's own job and lives in cognito.go.
//
// The shapes of the two M2L-X endpoints are open question SP-1 and rest on a
// single sample captured in docs/test-results.md around line 141:
//
//	GET /api/live_operation/kvs/webrtc_info/{event}
//	  → {"region":"eu-west-1","signaling_channel":{"pgm":["webrtc-wslstudios-matcht"]}}
//	GET /api/live_operation/kvs/webrtc_token/{event}
//	  → {identity_id, token}
//
// The instance this was measured against is currently powered off, so none of
// this has been exercised against a live M2L-X. Every error this package
// returns names the exact field that was missing, empty, or the wrong shape,
// so that whoever runs this against the live instance for the first time can
// diagnose a shape mismatch from the error string alone. If a live instance
// disagrees with the m2lx.KVSInfo / m2lx.KVSToken shape, that is a change to
// those types (WP-2's) and must be REPORTED under CONTRACT.md rule 3, not
// silently patched around here.
//
// WP-5a codes against the JSON shape of Credentials below; it must not change
// without WP-5a being told.
package kvs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"wslcomms/internal/m2lx"
)

// DefaultLoginsKey is the Cognito GetCredentialsForIdentity Logins map key used
// when m2lx.KVSToken.LoginsKey is empty.
const DefaultLoginsKey = "cognito-identity.amazonaws.com"

// Credentials is what the monitor page receives from the GetKVSCredentials bound
// method. The JSON tags are the property names WP-5a reads in JavaScript and are
// part of the contract.
//
// The credentials are short-lived. There is no refresh scheduler: if the peer
// connection fails or the signalling socket closes, the page tears the whole
// thing down and Go fetches a fresh set.
type Credentials struct {
	// Region is the AWS region hosting the signalling channel, measured as
	// "eu-west-1".
	Region string `json:"region"`

	// ChannelName is the KVS signalling channel to join as a viewer, e.g.
	// "webrtc-wslstudios-matcht". This is the AUTHORITATIVE identifier: it is
	// what M2L-X returns. WP-5a turns it into an ARN with
	// DescribeSignalingChannel before calling GetSignalingChannelEndpoint.
	//
	// A channel serves up to ten viewers, so joining does not displace the
	// gallery operator's browser — though that has not been confirmed live
	// (SP-6).
	ChannelName string `json:"channelName"`

	// ChannelARN is the channel's ARN if M2L-X ever supplies one. In the one
	// measured response it does NOT, so expect this to be empty and do not
	// branch on it: resolve the ARN from ChannelName instead.
	ChannelARN string `json:"channelArn"`

	// AccessKeyID is the temporary access key from GetCredentialsForIdentity.
	AccessKeyID string `json:"accessKeyId"`

	// SecretKey is the temporary secret access key.
	SecretKey string `json:"secretKey"`

	// SessionToken is the temporary session token.
	SessionToken string `json:"sessionToken"`

	// Expiry is when the temporary credentials stop working.
	Expiry time.Time `json:"expiry"`
}

// Expired reports whether c's Expiry has already passed. It is equivalent to
// ExpiresWithin(0).
func (c Credentials) Expired() bool {
	return c.ExpiresWithin(0)
}

// ExpiresWithin reports whether c will have expired within d from now, or
// whether Expiry is unset (the zero Time), which is treated as already
// expired since no caller can safely use credentials with an unknown expiry.
//
// This package has no refresh scheduler by design. Per spec section 7: if the
// peer connection fails or the signalling socket closes, the caller tears the
// whole thing down and calls Fetch again — a full re-run of webrtc_info,
// webrtc_token and GetCredentialsForIdentity — rather than refreshing these
// Credentials in place. ExpiresWithin exists so a caller can decide when that
// re-fetch is due, e.g. before opening a new peer connection.
func (c Credentials) ExpiresWithin(d time.Duration) bool {
	if c.Expiry.IsZero() {
		return true
	}
	return !time.Now().Add(d).Before(c.Expiry)
}

// Fetch runs the whole chain for one event and returns credentials ready to be
// handed to the monitor page.
//
// c must already be signed in; Fetch does not sign in or refresh on its own.
func Fetch(ctx context.Context, c m2lx.Client, eventID string) (Credentials, error) {
	return fetch(ctx, c, eventID, dialCognito)
}

// fetch is Fetch's implementation, parameterised over how the Cognito client
// for step 3 is constructed. Production code always calls it with dialCognito;
// tests substitute a cognitoDialer that returns a fake, so the whole chain can
// be exercised without a network call or AWS credentials in the test
// environment. See cognito.go.
func fetch(ctx context.Context, c m2lx.Client, eventID string, dial cognitoDialer) (Credentials, error) {
	if eventID == "" {
		return Credentials{}, errors.New("kvs: eventID is empty")
	}

	info, err := c.KVSInfo(ctx, eventID)
	if err != nil {
		return Credentials{}, fmt.Errorf("kvs: webrtc_info for event %q: %w", eventID, err)
	}
	if info.Region == "" {
		return Credentials{}, fmt.Errorf("kvs: webrtc_info response for event %q had an empty %q field", eventID, "region")
	}
	if err := validateChannelName(eventID, info); err != nil {
		return Credentials{}, err
	}

	tok, err := c.KVSToken(ctx, eventID)
	if err != nil {
		return Credentials{}, fmt.Errorf("kvs: webrtc_token for event %q: %w", eventID, err)
	}
	if tok.IdentityID == "" {
		return Credentials{}, fmt.Errorf("kvs: webrtc_token response for event %q had an empty %q field", eventID, "identity_id")
	}
	if tok.Token == "" {
		return Credentials{}, fmt.Errorf("kvs: webrtc_token response for event %q had an empty %q field", eventID, "token")
	}

	cc, err := dial(ctx, info.Region)
	if err != nil {
		return Credentials{}, err
	}

	return exchangeCredentials(ctx, cc, info.Region, info.ChannelName, tok)
}

// validateChannelName reports a diagnostic error naming exactly what was wrong
// with info's signalling_channel data when info.ChannelName — already resolved
// by m2lx.Client.KVSInfo as the first entry of Channels[m2lx.ChannelKeyPGM] —
// came back empty. info.Channels is kept whole by WP-2 precisely so this
// diagnosis is possible: it distinguishes "no signaling_channel object at
// all", "signaling_channel present but the pgm key is absent (present under a
// different key instead)", and "pgm key present but its channel list is
// empty", which is otherwise indistinguishable from a caller only holding the
// already-empty ChannelName.
func validateChannelName(eventID string, info m2lx.KVSInfo) error {
	if info.ChannelName != "" {
		return nil
	}

	if len(info.Channels) == 0 {
		return fmt.Errorf("kvs: webrtc_info response for event %q had an empty %q object", eventID, "signaling_channel")
	}

	pgm, hasPGM := info.Channels[m2lx.ChannelKeyPGM]
	if !hasPGM {
		keys := make([]string, 0, len(info.Channels))
		for k := range info.Channels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return fmt.Errorf("kvs: webrtc_info response for event %q had no %q key under %q; keys present: %v",
			eventID, m2lx.ChannelKeyPGM, "signaling_channel", keys)
	}
	if len(pgm) == 0 {
		return fmt.Errorf("kvs: webrtc_info response for event %q had an empty channel list under %q",
			eventID, "signaling_channel."+m2lx.ChannelKeyPGM)
	}
	// hasPGM and non-empty but ChannelName is still blank: the first entry
	// itself must be an empty string.
	return fmt.Errorf("kvs: webrtc_info response for event %q had a blank first channel name under %q (%v)",
		eventID, "signaling_channel."+m2lx.ChannelKeyPGM, pgm)
}
