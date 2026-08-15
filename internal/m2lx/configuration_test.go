package m2lx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// liveConfiguration is the VERBATIM 12108-byte body captured from
// GET /api/v1/switcher_configuration on the live matchH instance on 2026-08-15,
// recorded rather than trimmed. It is served whole by the tests below so that
// the claim "nodes[] and system_info are ignored on decode" is proved against
// the real thing — 41 nodes and a firmware build stamp — and not against a
// hand-written two-key object that could never have tripped the decode anyway.
const liveConfiguration = "testdata/switcher_configuration-live-2026-08-15.json"

func TestClient_SwitcherConfiguration_RequiresSignIn(t *testing.T) {
	c := newClient("example.com")
	defer c.Close()
	if _, err := c.SwitcherConfiguration(context.Background()); err != ErrNotSignedIn {
		t.Fatalf("SwitcherConfiguration before sign-in: err = %v, want ErrNotSignedIn", err)
	}
}

func TestClient_SwitcherConfiguration_LiveCapture(t *testing.T) {
	body, err := os.ReadFile(liveConfiguration)
	if err != nil {
		t.Fatalf("reading the live capture: %v", err)
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != switcherConfigurationPath {
			t.Errorf("path = %q, want %q", r.URL.Path, switcherConfigurationPath)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-1" {
			t.Errorf("Authorization = %q, want Bearer tok-1 — the same bearer as the KVS calls", got)
		}
		// The live call sends no query string and no body; assert we send none.
		if r.URL.RawQuery != "" {
			t.Errorf("unexpected query string %q — the endpoint takes no parameters", r.URL.RawQuery)
		}
		w.Write(body)
	}))
	defer srv.Close()

	c := newClient(hostOf(t, srv.URL))
	c.httpClient = srv.Client()
	c.token = "tok-1"
	defer c.Close()

	conf, err := c.SwitcherConfiguration(context.Background())
	if err != nil {
		t.Fatalf("SwitcherConfiguration: %v", err)
	}

	// The measured instance: 1920x1080, 50 fps, rec709.
	if conf.Width != 1920 || conf.Height != 1080 || conf.FrameRate != 50 {
		t.Errorf("got %dx%d@%v, want 1920x1080@50", conf.Width, conf.Height, conf.FrameRate)
	}
	if !conf.Valid() {
		t.Error("Valid() = false for the measured healthy configuration")
	}
	if got, want := conf.String(), "1920x1080@50"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if conf.SignalType != "rec709" {
		t.Errorf("SignalType = %q, want %q", conf.SignalType, "rec709")
	}

	// Raw renders every field the switcher sent, in format.go's order: the
	// known keys first, then the rest sorted. It is the only thing that makes
	// bit_depth, color_space and signal_type visible at all, and a log line
	// quotes it.
	const wantRaw = `width=1920 height=1080 frame_rate="50" bit_depth=8 ` +
		`color_space="YCbCr" signal_type="rec709"`
	if conf.Raw != wantRaw {
		t.Errorf("Raw = %q, want %q", conf.Raw, wantRaw)
	}

	// The setting describes a raster, not a compression. There is no codec in
	// the format block and none is invented.
	if conf.Codec != "" {
		t.Errorf("Codec = %q, want empty: the configuration states no codec", conf.Codec)
	}
}

func TestClient_SwitcherConfiguration_FrameRateIsAStringBesideNumbers(t *testing.T) {
	// THE TRAP, asserted here as well as in format_test.go because this
	// endpoint is a second place it appears and a firmware could change one
	// without the other. A rate of "50" must parse to 50; the same fields
	// arriving as numbers must also parse, since format.go tolerates that
	// deliberately; and a rate that is neither must leave FrameRate at zero,
	// which Valid() reports as unusable rather than as 0 fps.
	cases := []struct {
		name  string
		body  string
		rate  float64
		valid bool
	}{
		{
			name:  "the measured shape: a string rate beside numeric width and height",
			body:  `{"format":{"video":{"frame_rate":"50","height":1080,"width":1920}}}`,
			rate:  50,
			valid: true,
		},
		{
			name:  "a firmware that makes the obvious correction",
			body:  `{"format":{"video":{"frame_rate":50,"height":1080,"width":1920}}}`,
			rate:  50,
			valid: true,
		},
		{
			name:  "an NTSC rate, which is a decimal and not a fraction",
			body:  `{"format":{"video":{"frame_rate":"29.97","height":1080,"width":1920}}}`,
			rate:  29.97,
			valid: true,
		},
		{
			name:  "a rate that is neither a number nor a numeric string",
			body:  `{"format":{"video":{"frame_rate":"variable","height":1080,"width":1920}}}`,
			rate:  0,
			valid: false,
		},
		{
			name:  "a rate of zero — the shape a STREAMING NODE was measured reporting",
			body:  `{"format":{"video":{"frame_rate":"0","height":720,"width":1280}}}`,
			rate:  0,
			valid: false,
		},
		{
			name:  "no format block at all",
			body:  `{"nodes":[],"system_info":{}}`,
			rate:  0,
			valid: false,
		},
		{
			name:  "a format block that is JSON null",
			body:  `{"format":{"video":null}}`,
			rate:  0,
			valid: false,
		},
		{
			name:  "a width that is a string, which is not a width",
			body:  `{"format":{"video":{"frame_rate":"50","height":1080,"width":"1920"}}}`,
			rate:  50,
			valid: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conf := configurationFromBody(t, tc.body)
			if conf.FrameRate != tc.rate {
				t.Errorf("FrameRate = %v, want %v", conf.FrameRate, tc.rate)
			}
			if conf.Valid() != tc.valid {
				t.Errorf("Valid() = %v, want %v (raw %q)", conf.Valid(), tc.valid, conf.Raw)
			}
		})
	}
}

func TestClient_SwitcherConfiguration_UnreadableIsNotAnError(t *testing.T) {
	// The instance answered; this build could not read the answer. That is a
	// value with Valid() false and Raw carrying what arrived — NOT an error,
	// because the caller's log line distinguishes "the switcher would not
	// answer" from "the switcher said something we do not understand", and only
	// the second can quote the shape.
	conf := configurationFromBody(t, `{"format":{"video":{"raster":"1080p50"}}}`)
	if conf.Valid() {
		t.Fatal("Valid() = true for a format block with no width, height or rate")
	}
	if conf.Raw != `raster="1080p50"` {
		t.Errorf("Raw = %q, want the unrecognised field rendered verbatim", conf.Raw)
	}
}

func TestClient_SwitcherConfiguration_HTTPFailuresAreErrors(t *testing.T) {
	// A 401 is what an expired token looks like and a 404 is what a firmware
	// without this endpoint would look like. Both must reach the caller as
	// errors so it falls back and says why, and neither may carry the bearer
	// token into the message that gets logged.
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))

		c := newClient(hostOf(t, srv.URL))
		c.httpClient = srv.Client()
		c.token = "tok-secret"

		_, err := c.SwitcherConfiguration(context.Background())
		if err == nil {
			t.Errorf("HTTP %d: expected an error", status)
		} else if strings.Contains(err.Error(), "tok-secret") {
			t.Errorf("HTTP %d: the error message quotes the bearer token: %v", status, err)
		}

		c.Close()
		srv.Close()
	}
}

func TestClient_SwitcherConfiguration_ContextIsHonoured(t *testing.T) {
	// The call is made on the START path under a short deadline. A cancelled
	// context must come back as an error rather than hanging, because the
	// caller's whole contract is that it falls back and starts anyway.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newClient(hostOf(t, srv.URL))
	c.httpClient = srv.Client()
	c.token = "tok-1"
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.SwitcherConfiguration(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a context.Canceled", err)
	}
}

func TestSwitcherConfiguration_Colorimetry(t *testing.T) {
	// The vocabulary translation. rec709 is the only value ever MEASURED; the
	// other two are in the table because their GStreamer counterparts are
	// unambiguous. Everything else — including an empty signal_type, which is
	// what an older firmware would send — must report not-known rather than a
	// plausible default, so a caller keeps the colorimetry it already pins and
	// says that it did.
	cases := []struct {
		signalType string
		want       string
		known      bool
	}{
		{"rec709", "bt709", true},
		{"REC709", "bt709", true},  // case is not promised by anything
		{" rec709", "bt709", true}, // nor is the absence of whitespace
		{"rec601", "bt601", true},
		{"rec2020", "bt2020", true},
		{"rec2100", "", false}, // HDR is a transfer function beside primaries, not one name
		{"bt709", "", false},   // GStreamer's own spelling is not M2L-X's vocabulary
		{"", "", false},
	}

	for _, tc := range cases {
		conf := SwitcherConfiguration{SignalType: tc.signalType}
		got, known := conf.Colorimetry()
		if got != tc.want || known != tc.known {
			t.Errorf("Colorimetry(%q) = (%q, %v), want (%q, %v)",
				tc.signalType, got, known, tc.want, tc.known)
		}
	}
}

// configurationFromBody serves body from a test server and returns what
// SwitcherConfiguration made of it, so each case above is a WIRE shape rather
// than a hand-built struct that could not have arrived from any switcher.
func configurationFromBody(t *testing.T, body string) SwitcherConfiguration {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	c := newClient(hostOf(t, srv.URL))
	c.httpClient = srv.Client()
	c.token = "tok-1"
	defer c.Close()

	conf, err := c.SwitcherConfiguration(context.Background())
	if err != nil {
		t.Fatalf("SwitcherConfiguration: %v", err)
	}
	return conf
}
