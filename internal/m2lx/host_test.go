package m2lx

import (
	"strings"
	"testing"
)

// TestResolveHost_SchemeSelection is the direct regression test for the
// Gate A blocker: the app could not talk to cmd/mockm2lx because
// client.go and watcher.go each hardcoded a scheme instead of deriving
// one from the configured host. Every case here asserts BOTH the REST
// scheme and the WebSocket scheme, and that the two always agree (host.go
// derives wsScheme from the same insecure flag as restScheme, never
// independently) — per the task: "Assert the derived WebSocket URL
// follows the REST scheme in every case."
func TestResolveHost_SchemeSelection(t *testing.T) {
	cases := []struct {
		name         string
		host         string
		wantHostPort string
		wantREST     string // "http" or "https"
		wantWS       string // "ws" or "wss"
	}{
		{
			name:         "explicit https host",
			host:         "https://m2lx.example.com",
			wantHostPort: "m2lx.example.com",
			wantREST:     "https",
			wantWS:       "wss",
		},
		{
			name:         "explicit http host selects the insecure pair",
			host:         "http://127.0.0.1:18081",
			wantHostPort: "127.0.0.1:18081",
			wantREST:     "http",
			wantWS:       "ws",
		},
		{
			name: "bare host with no scheme defaults to the secure pair " +
				"(a config typo must not put a password on the wire in clear)",
			host:         "m2lx.example.com",
			wantHostPort: "m2lx.example.com",
			wantREST:     "https",
			wantWS:       "wss",
		},
		{
			name: "bare host with a port must not have its port mistaken " +
				"for a scheme separator",
			host:         "m2lx.example.com:8443",
			wantHostPort: "m2lx.example.com:8443",
			wantREST:     "https",
			wantWS:       "wss",
		},
		{
			name:         "https host with a trailing slash",
			host:         "https://m2lx.example.com/",
			wantHostPort: "m2lx.example.com",
			wantREST:     "https",
			wantWS:       "wss",
		},
		{
			name:         "http host with a trailing slash",
			host:         "http://127.0.0.1:18081/",
			wantHostPort: "127.0.0.1:18081",
			wantREST:     "http",
			wantWS:       "ws",
		},
		{
			name:         "bare host with a trailing slash",
			host:         "m2lx.example.com/",
			wantHostPort: "m2lx.example.com",
			wantREST:     "https",
			wantWS:       "wss",
		},
		{
			name: "garbage scheme never silently downgrades to insecure",
			host: "ftp://m2lx.example.com",
			// hostPort is still stripped of the (unrecognised) scheme
			// prefix, but the scheme choice itself falls back to the safe
			// default rather than guessing.
			wantHostPort: "m2lx.example.com",
			wantREST:     "https",
			wantWS:       "wss",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rh := resolveHost(tc.host, "test")

			if rh.hostPort != tc.wantHostPort {
				t.Fatalf("hostPort = %q, want %q", rh.hostPort, tc.wantHostPort)
			}
			if got := rh.restScheme(); got != tc.wantREST {
				t.Fatalf("restScheme() = %q, want %q", got, tc.wantREST)
			}
			if got := rh.wsScheme(); got != tc.wantWS {
				t.Fatalf("wsScheme() = %q, want %q", got, tc.wantWS)
			}

			// The WebSocket URL statusURL builds must be prefixed with
			// exactly the scheme this table says, and must carry the
			// stripped hostPort, not the original (possibly
			// scheme-prefixed, possibly trailing-slashed) host string.
			wsURL := statusURL(rh, "tok")
			wantPrefix := tc.wantWS + "://" + tc.wantHostPort + "/"
			if !strings.HasPrefix(wsURL, wantPrefix) {
				t.Fatalf("statusURL = %q, want prefix %q", wsURL, wantPrefix)
			}
		})
	}
}

// TestResolveHost_HTTPIsLoggedAsInsecure does not assert on log output
// (this package has no injectable logger), but it does confirm the http
// path is reachable at all and does not, e.g., panic while producing the
// warning. The warning text itself is verified by reading host.go's
// resolveHost, which is exercised line-for-line by
// TestResolveHost_SchemeSelection above.
func TestResolveHost_HTTPIsLoggedAsInsecure(t *testing.T) {
	rh := resolveHost("http://127.0.0.1:18081", "REST")
	if !rh.insecure {
		t.Fatalf("insecure = false for an explicit http:// host")
	}
}

// TestNewClient_DerivesSchemeFromHost is an end-to-end check, through the
// exported constructor, that NewClient actually wires resolveHost's
// result onto the client it returns — not just that resolveHost itself is
// correct in isolation.
func TestNewClient_DerivesSchemeFromHost(t *testing.T) {
	c := newClient("http://127.0.0.1:18081")
	defer c.Close()
	if got := c.rest.restScheme(); got != "http" {
		t.Fatalf("newClient(%q).rest.restScheme() = %q, want http", "http://127.0.0.1:18081", got)
	}
	if got := c.rest.hostPort; got != "127.0.0.1:18081" {
		t.Fatalf("newClient(%q).rest.hostPort = %q, want 127.0.0.1:18081", "http://127.0.0.1:18081", got)
	}

	bare := newClient("m2lx.example.com")
	defer bare.Close()
	if got := bare.rest.restScheme(); got != "https" {
		t.Fatalf("newClient(%q).rest.restScheme() = %q, want https (never silently downgrade)", "m2lx.example.com", got)
	}
}

// TestNewWatcher_DerivesSchemeFromHost is NewClient's counterpart for
// NewWatcher: the status socket must resolve the same way, from the same
// rules, given the same host string.
func TestNewWatcher_DerivesSchemeFromHost(t *testing.T) {
	fc := &fakeClientToken{}

	w := newWatcher("http://127.0.0.1:18081", fc)
	if got := w.status.wsScheme(); got != "ws" {
		t.Fatalf("newWatcher(%q, ...).status.wsScheme() = %q, want ws", "http://127.0.0.1:18081", got)
	}

	bare := newWatcher("m2lx.example.com", fc)
	if got := bare.status.wsScheme(); got != "wss" {
		t.Fatalf("newWatcher(%q, ...).status.wsScheme() = %q, want wss (never silently downgrade)", "m2lx.example.com", got)
	}
}
