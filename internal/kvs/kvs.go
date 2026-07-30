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
// The shapes of the two M2L-X endpoints are open question SP-1 and rest on a
// single sample captured in docs/test-results.md. If a live instance disagrees,
// that is a change to m2lx.KVSInfo and m2lx.KVSToken and must be REPORTED, not
// edited: WP-5a codes against the JSON shape of Credentials below.
package kvs

import (
	"context"
	"errors"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentity"

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

// Fetch runs the whole chain for one event and returns credentials ready to be
// handed to the monitor page.
//
// c must already be signed in; Fetch does not sign in or refresh on its own.
func Fetch(ctx context.Context, c m2lx.Client, eventID string) (Credentials, error) {
	return Credentials{}, errors.New("not implemented: WP-4")
}

// Referenced so that `go mod tidy` keeps the frozen dependencies on the AWS SDK
// before WP-4 writes the Cognito exchange. WP-4 deletes these lines.
var (
	_ = awsconfig.LoadDefaultConfig
	_ = cognitoidentity.NewFromConfig
)
