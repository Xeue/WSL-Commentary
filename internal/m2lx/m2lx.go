// Package m2lx is the M2L-X control plane: REST sign-in and token refresh, the
// KVS credential endpoints, and the switcher_status WebSocket that is the sole
// liveness truth for the status lamps.
//
// Owner: WP-2. No other work package writes files in this directory.
//
// This package makes no call that starts or stops anything on M2L-X. The
// contribution path is SRT only; M2L-X's router input is a listener and needs no
// REST handshake.
package m2lx

import (
	"context"
	"errors"
	"time"
)

// Stream states reported by <statusKey>.stream_state on the status WebSocket.
// StreamStateStreaming is the only good one.
const (
	StreamStateStreaming = "streaming"
	StreamStateStarting  = "starting"
	StreamStateStopped   = "stopped"
)

// Timing constants fixed by specification section 8.
const (
	// DebounceWindow is how long a stream_state change must persist before a
	// Status carrying it is emitted, so a momentary transition does not flap the
	// lamp.
	DebounceWindow = 4 * time.Second

	// StaleAfter is how long the WebSocket may be silent before the three
	// WebSocket-derived lamps must be greyed and STATUS UNAVAILABLE shown. A
	// Status with Stale set to true is emitted when this elapses.
	StaleAfter = 15 * time.Second

	// TokenLifetime is the measured lifetime of the bearer token returned by
	// sign-in: 86399 seconds.
	TokenLifetime = 86399 * time.Second

	// RefreshFraction is how far into TokenLifetime the token is refreshed.
	RefreshFraction = 0.5
)

// ErrNotSignedIn is returned when a call requiring a bearer token is made before
// SignIn has succeeded, or after the token has expired and Refresh has failed.
var ErrNotSignedIn = errors.New("m2lx: not signed in")

// VideoFormat is the detected video format of our router input, parsed from
// <statusKey>.streams.video.format on the status WebSocket.
//
// These are DETECTED values. The REST width/height/frame_rate/codec fields are
// the CONFIGURED values and will cheerfully report 1080p50 over a 720p25 stream;
// they must not be used for the VIDEO OK lamp.
type VideoFormat struct {
	// Raw is the format string verbatim, as measured: "h264 1920x1080 50 P".
	// It is kept so that the parse below can be checked against reality at
	// Gate C, and so a format the parser does not understand can still be shown
	// to the user instead of silently reading as zero.
	Raw string `json:"raw"`

	// Codec is the detected codec, e.g. "h264".
	Codec string `json:"codec"`
	// Width in pixels. Good state is 1920.
	Width int `json:"width"`
	// Height in pixels. Good state is 1080.
	Height int `json:"height"`
	// FrameRate in frames per second. Good state is 50.
	FrameRate float64 `json:"frameRate"`
}

// AudioFormat is one detected audio stream of our router input, parsed from an
// element of <statusKey>.streams.audio.
type AudioFormat struct {
	// Raw is the format string verbatim, as measured: "aac 48000 2ch".
	Raw string `json:"raw"`

	// Codec is the detected codec. Good state is "aac".
	Codec string `json:"codec"`
	// SampleRate in Hz. Good state is 48000.
	SampleRate int `json:"sampleRate"`
	// Channels is the channel count. Good state is 2.
	Channels int `json:"channels"`
}

// Status is one normalised snapshot of our router input's health, already
// debounced and staleness-checked by the Watcher.
//
// This is not the wire format. WP-2 unmarshals M2L-X's switcher_status payload
// into its own private types and produces this. The JSON tags here are the shape
// the frontend receives on the Wails "status" event, so they must not change
// without WP-5b being told.
type Status struct {
	// StreamState is the debounced <statusKey>.stream_state: one of
	// StreamStateStreaming, StreamStateStarting or StreamStateStopped. It is
	// empty when Stale is true.
	StreamState string `json:"streamState"`

	// Video is the detected video format. Zero when Stale is true.
	Video VideoFormat `json:"video"`

	// Audio is the detected audio format list. An EMPTY OR ABSENT slice is the
	// MP2/AC-3 silent-drop signature: M2L-X keeps the video online and discards
	// the audio without saying so. This is a slice, not a single value, so that
	// callers can see the empty case; the AUDIO OK lamp reads Audio[0] and must
	// go red rather than panic when the slice is empty.
	Audio []AudioFormat `json:"audio"`

	// At is when the underlying WebSocket message was received, or when
	// staleness was declared.
	At time.Time `json:"at"`

	// Stale is true when no WebSocket message has arrived for StaleAfter. The
	// three WebSocket-derived lamps must then be greyed and STATUS UNAVAILABLE
	// shown rather than the last known values held green. The next live message
	// produces a Status with Stale false.
	Stale bool `json:"stale"`
}

// ChannelKeyPGM is the key under which the programme mosaic's signalling
// channel appears in KVSInfo.Channels. It is the only key observed in the one
// measured response.
const ChannelKeyPGM = "pgm"

// KVSInfo is the response of GET /api/live_operation/kvs/webrtc_info/{eventId}:
// which AWS region and which signalling channel the event's monitor uses.
//
// One response has been observed, in docs/test-results.md:
//
//	{"region":"eu-west-1","signaling_channel":{"pgm":["webrtc-wslstudios-matcht"]}}
//
// Note what it does NOT contain: a channel ARN. M2L-X supplies a channel NAME,
// and the ARN is resolved from it with DescribeSignalingChannel, which happens
// in the frontend because that is where the AWS KVS SDK lives.
//
// This is open question SP-1 and rests on a single sample. If a live instance
// disagrees, WP-4 must report it rather than edit this type.
type KVSInfo struct {
	// Region is the AWS region, measured as "eu-west-1".
	Region string `json:"region"`

	// Channels is the signaling_channel object verbatim: a bus name mapped to
	// the signalling channel names serving it. Kept whole so that nothing in the
	// one measured sample is thrown away before it is understood.
	Channels map[string][]string `json:"channels"`

	// ChannelName is the channel the monitor joins, resolved by WP-2 as the
	// first entry of Channels[ChannelKeyPGM]. Empty if that key is absent, which
	// callers must treat as an error rather than a blank channel name.
	ChannelName string `json:"channelName"`
}

// KVSToken is the response of GET
// /api/live_operation/kvs/webrtc_token/{eventId}: the Cognito identity and the
// token that is exchanged for temporary AWS credentials.
//
// The observed response body is {identity_id, token}. Its token lifetime and
// refresh behaviour are untested — there is no refresh scheduler, and a dead
// peer connection is answered by fetching the whole chain again.
//
// This is open question SP-1. If a live instance disagrees, WP-4 must report it
// rather than edit this type.
type KVSToken struct {
	// IdentityID is the Cognito identity ID, wire field "identity_id", passed as
	// IdentityId to GetCredentialsForIdentity.
	IdentityID string `json:"identityId"`

	// Token is the OpenID token, wire field "token", passed in the Logins map of
	// GetCredentialsForIdentity.
	Token string `json:"token"`

	// LoginsKey is the Logins map key under which Token is presented. It is not
	// part of the observed response; when empty the caller uses
	// kvs.DefaultLoginsKey. It exists so that a Logins key other than the
	// standard one can be carried without a contract change.
	LoginsKey string `json:"loginsKey"`
}

// Client is the M2L-X REST surface: authentication and the two KVS credential
// endpoints. It holds the bearer token for the process lifetime.
//
// Implementations must be safe for concurrent use: the Watcher reads Token while
// the UI may be calling Refresh.
type Client interface {
	// SignIn performs POST /api/local_auth/signin with body
	// {"alias": alias, "password": password} and stores the returned bearer
	// token. The field is "alias"; sending "username" returns HTTP 500.
	SignIn(ctx context.Context, alias, password string) error

	// Refresh renews the bearer token through /api/local_auth/refresh_token.
	// The measured access token lifetime is TokenLifetime; refresh at
	// RefreshFraction of it. The refresh token's own TTL is unmeasured, so
	// Refresh failing must fall back to a full SignIn rather than giving up.
	//
	// Because the status WebSocket carries the token in its URL, a successful
	// Refresh obliges the Watcher to reopen its socket.
	Refresh(ctx context.Context) error

	// Token returns the current bearer token, or the empty string if SignIn has
	// not succeeded.
	Token() string

	// KVSInfo fetches the signalling channel's region and ARN for an event.
	KVSInfo(ctx context.Context, eventID string) (KVSInfo, error)

	// KVSToken fetches the Cognito identity and token for an event.
	KVSToken(ctx context.Context, eventID string) (KVSToken, error)

	// Close cancels the background token-refresh goroutine started by
	// SignIn, if one was ever started, and blocks until it has actually
	// exited. Idempotent and safe to call concurrently with itself and
	// with Token. Callers that construct a Client for a bounded lifetime
	// (WP-8's per-generation control plane, restarted on every config
	// change) must call Close when tearing that generation down, or the
	// goroutine leaks for the rest of the process holding a timer for up
	// to RefreshFraction*TokenLifetime.
	Close() error
}

// Watcher owns the switcher_status WebSocket.
type Watcher interface {
	// Watch opens the status WebSocket and returns a channel of normalised
	// Status values for the node named by statusKey.
	//
	// Watch applies the DebounceWindow to stream_state and emits a Status with
	// Stale set after StaleAfter of silence. It reconnects on its own — including
	// after a token Refresh, since the token is in the URL — and only stops when
	// ctx is cancelled. The channel is closed when ctx is done.
	Watch(ctx context.Context, statusKey string) <-chan Status

	// WatchAll opens the status WebSocket and returns every frame as a whole
	// Document, with no debounce, no staleness and no filtering by node.
	//
	// It exists because statusKey cannot be discovered any other way: no REST
	// endpoint lists the switcher's nodes, so the only way to learn which one is
	// our router input is to watch all of them and see which starts streaming as
	// our feed comes up (specification open question 5). Debouncing would blur
	// exactly that transition, so this channel is raw.
	//
	// Reconnection and token rotation behave as they do for Watch. The channel
	// is closed when ctx is done. Feed it to a Discovery; see discover.go.
	//
	// ADDED after the WP-0 contract was frozen, and reported under CONTRACT.md
	// rule 3 rather than worked around: the alternative was for WP-8 to open a
	// second WebSocket to the same endpoint behind this package's back, which
	// would have duplicated the reconnect and token-rotation logic that is the
	// whole point of the Watcher.
	WatchAll(ctx context.Context) <-chan Document
}

// NewClient returns a Client for the M2L-X instance at host.
//
// host is a bare hostname or host:port with no scheme, OR host:port
// prefixed with an explicit "http://" or "https://". A bare host, and an
// explicit "https://" host, both choose https for REST and wss for the
// status socket — that is the production path and the only one that
// cannot put a commentator's password on the wire in clear. An explicit
// "http://" host chooses http/ws instead; its only legitimate use is
// cmd/mockm2lx (WP-7), which speaks plain HTTP with no TLS option at Gate
// A. See host.go (resolveHost) for the full decision, including what
// happens with a scheme this package does not recognise.
func NewClient(host string) Client {
	return newClient(host)
}

// NewWatcher returns a Watcher for the M2L-X instance at host that authenticates
// using the bearer token held by c.
//
// host follows the same scheme rules as NewClient's host (host.go,
// resolveHost): a bare host or an explicit "https://" host uses wss, an
// explicit "http://" host uses ws. Callers must pass the SAME host string
// given to the NewClient that produced c, so the REST and status-socket
// schemes can never disagree about which M2L-X instance — real or mock —
// they are both pointed at.
func NewWatcher(host string, c Client) Watcher {
	return newWatcher(host, c)
}
