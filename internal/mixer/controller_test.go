package mixer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ============================================================================
// Envelope serialisation
// ============================================================================

// TestEnvelopeMatchesMeasuredJSON pins every Command's wire form to the exact
// JSON measured against the device or read out of Sony's own bundle.
//
// The comparison is on the marshalled bytes, not on the map, because the map
// is not the thing the mixer sees. encoding/json sorts map keys, so these
// strings are stable; if that ever changes the test fails loudly rather than
// letting an unstable message shape reach a live clean feed.
//
// Each want string is annotated with where it came from. A test that pins a
// wire format to nothing but the implementation's own opinion proves nothing.
func TestEnvelopeMatchesMeasuredJSON(t *testing.T) {
	tests := []struct {
		name   string
		source string
		cmd    Command
		want   string
	}{
		{
			name:   "set_routing to master and the clean feed",
			source: "MEASURED shape, task brief: {\"matrix\":\"output\",\"input\":\"cam22-1\",\"outputs\":[\"master\",\"aux1\"]}",
			cmd: SetRouting{
				Matrix:  MatrixOutput,
				Strip:   "cam22-1",
				Outputs: []Bus{BusMaster, BusAux1},
			},
			want: `{"args":{"input":"cam22-1","matrix":"output","outputs":["master","aux1"]},"command":"set_routing","node":"advanced_audio_mixer"}`,
		},
		{
			name:   "set_routing removing commentary from the clean feed",
			source: "the correction this drawer exists to make: cam22-1 keeps master, loses aux1",
			cmd: SetRouting{
				Matrix:  MatrixOutput,
				Strip:   "cam22-1",
				Outputs: []Bus{BusMaster},
			},
			want: `{"args":{"input":"cam22-1","matrix":"output","outputs":["master"]},"command":"set_routing","node":"advanced_audio_mixer"}`,
		},
		{
			name:   "set_routing with an explicit empty set is [] and never null",
			source: "an explicit un-route; null would be a different message",
			cmd: SetRouting{
				Matrix:  MatrixOutput,
				Strip:   "cam22-1",
				Outputs: []Bus{},
			},
			want: `{"args":{"input":"cam22-1","matrix":"output","outputs":[]},"command":"set_routing","node":"advanced_audio_mixer"}`,
		},
		{
			name:   "set_routing on the PFL matrix",
			source: "docs/architecture.md: the PFL path is confirmed, {matrix: PFL, input, outputs:[MON4]}",
			cmd: SetRouting{
				Matrix:  MatrixPFL,
				Strip:   "cam22-1",
				Outputs: []Bus{BusMon4},
			},
			want: `{"args":{"input":"cam22-1","matrix":"pfl","outputs":["mon4"]},"command":"set_routing","node":"advanced_audio_mixer"}`,
		},
		{
			name:   "set_input_muted is the ARRAY form",
			source: "docs/architecture.md line 430, from Sony's bundle: args:[{\"name\":\"cam23-1\",\"muted\":true}]",
			cmd:    SetInputMuted{Strip: "cam23-1", Muted: true},
			want:   `{"args":[{"muted":true,"name":"cam23-1"}],"command":"set_input_muted","node":"advanced_audio_mixer"}`,
		},
		{
			name:   "set_input_muted unmuting",
			source: "same shape, muted false",
			cmd:    SetInputMuted{Strip: "cam22-1", Muted: false},
			want:   `{"args":[{"muted":false,"name":"cam22-1"}],"command":"set_input_muted","node":"advanced_audio_mixer"}`,
		},
		{
			name:   "set_output_muted is the ARRAY form even for one bus",
			source: "MEASURED: [{\"name\":\"aux1\",\"muted\":true}]; the object form returns HTTP 400",
			cmd:    SetOutputMuted{Buses: []Bus{BusAux1}, Muted: true},
			want:   `{"args":[{"muted":true,"name":"aux1"}],"command":"set_output_muted","node":"advanced_audio_mixer"}`,
		},
		{
			name:   "set_output_muted across several buses repeats muted per element",
			source: "the array form, extended to the multi-bus case",
			cmd:    SetOutputMuted{Buses: []Bus{BusAux1, BusAux2}, Muted: false},
			want:   `{"args":[{"muted":false,"name":"aux1"},{"muted":false,"name":"aux2"}],"command":"set_output_muted","node":"advanced_audio_mixer"}`,
		},
		{
			name:   "set_ch_fader carries the per-channel gain pair in dB",
			source: "read side: fader[\"cam22-1\"].ch_fader.gain = [-1.5748…,-1.5748…]",
			cmd:    SetChFader{Strip: "cam22-1", Gain: [2]float64{-1.5, -1.5}},
			want:   `{"args":{"gain":[-1.5,-1.5],"name":"cam22-1"},"command":"set_ch_fader","node":"advanced_audio_mixer"}`,
		},
		{
			name:   "set_ch_fader mute is -144 dB",
			source: "MEASURED: MuteFaderDB",
			cmd:    SetChFader{Strip: "cam22-1", Gain: [2]float64{MuteFaderDB, MuteFaderDB}},
			want:   `{"args":{"gain":[-144,-144],"name":"cam22-1"},"command":"set_ch_fader","node":"advanced_audio_mixer"}`,
		},
		{
			name:   "set_comp_limit arming a limiter",
			source: "MEASURED shape, task brief: {\"name\":\"cam23-1\",\"agc_mode\":\"limiter\",\"pre_gain\":0,\"compressor_th\":0,\"limiter_th\":-3}",
			cmd: SetCompLimit{
				Strip:        "cam23-1",
				AGCMode:      AGCLimiter,
				PreGain:      0,
				CompressorTh: 0,
				LimiterTh:    LimiterAt(-3),
			},
			want: `{"args":{"agc_mode":"limiter","compressor_th":0,"limiter_th":-3,"name":"cam23-1","pre_gain":0},"command":"set_comp_limit","node":"advanced_audio_mixer"}`,
		},
		{
			name:   "set_comp_limit off is the factory state and stays legal",
			source: "live frame: every strip is {agc_mode:\"off\", compressor_th:0, limiter_th:0, pre_gain:0}",
			cmd: SetCompLimit{
				Strip:   "cam22-1",
				AGCMode: AGCOff,
			},
			want: `{"args":{"agc_mode":"off","compressor_th":0,"limiter_th":0,"name":"cam22-1","pre_gain":0},"command":"set_comp_limit","node":"advanced_audio_mixer"}`,
		},
		{
			name:   "set_comp_limit pre_gain is dB-calibrated and carried verbatim",
			source: "MEASURED: pre_gain is exactly dB-calibrated",
			cmd: SetCompLimit{
				Strip:     "cam22-1",
				AGCMode:   AGCLimiter,
				PreGain:   -6.5,
				LimiterTh: LimiterAt(-3),
			},
			want: `{"args":{"agc_mode":"limiter","compressor_th":0,"limiter_th":-3,"name":"cam22-1","pre_gain":-6.5},"command":"set_comp_limit","node":"advanced_audio_mixer"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := tt.cmd.Envelope()
			if err != nil {
				t.Fatalf("Envelope() error = %v (source: %s)", err, tt.source)
			}
			got, err := json.Marshal(env)
			if err != nil {
				t.Fatalf("marshal envelope: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("envelope mismatch\n got: %s\nwant: %s\nsource: %s", got, tt.want, tt.source)
			}
		})
	}
}

// TestEveryEnvelopeCarriesTheNode checks the invariant that no command can be
// sent to the wrong DSP node. The node is the only thing tying a write to the
// mixer the read path parses.
func TestEveryEnvelopeCarriesTheNode(t *testing.T) {
	cmds := []Command{
		SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster}},
		SetInputMuted{Strip: "cam22-1", Muted: true},
		SetOutputMuted{Buses: []Bus{BusAux1}, Muted: true},
		SetChFader{Strip: "cam22-1", Gain: [2]float64{0, 0}},
		SetCompLimit{Strip: "cam22-1", AGCMode: AGCOff},
	}
	for _, cmd := range cmds {
		t.Run(commandName(cmd), func(t *testing.T) {
			env, err := cmd.Envelope()
			if err != nil {
				t.Fatalf("Envelope() error = %v", err)
			}
			if env["node"] != NodeName {
				t.Errorf("node = %v, want %q", env["node"], NodeName)
			}
			if env["command"] != commandName(cmd) {
				t.Errorf("command = %v, want %q", env["command"], commandName(cmd))
			}
			if _, ok := env["args"]; !ok {
				t.Error("envelope has no args member")
			}
		})
	}
}

// TestMuteCommandsUseTheArrayForm asserts the shape of the two *_muted
// commands directly, rather than only through their rendered JSON.
//
// It is a separate test because the array form is the one thing about these
// commands that a future maintainer is most likely to "simplify": a
// single-target command that takes a one-element array looks like an
// accident. MEASURED, it is not — set_output_muted rejects the object form
// with HTTP 400.
func TestMuteCommandsUseTheArrayForm(t *testing.T) {
	tests := []struct {
		name string
		cmd  Command
	}{
		{"set_input_muted", SetInputMuted{Strip: "cam22-1", Muted: true}},
		{"set_output_muted", SetOutputMuted{Buses: []Bus{BusAux1}, Muted: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := tt.cmd.Envelope()
			if err != nil {
				t.Fatalf("Envelope() error = %v", err)
			}
			raw, err := json.Marshal(env["args"])
			if err != nil {
				t.Fatalf("marshal args: %v", err)
			}
			if !strings.HasPrefix(string(raw), "[") {
				t.Fatalf("args = %s, want a JSON ARRAY; the object form returns HTTP 400", raw)
			}
			var arr []map[string]any
			if err := json.Unmarshal(raw, &arr); err != nil {
				t.Fatalf("args is not an array of objects: %v", err)
			}
			if len(arr) != 1 {
				t.Fatalf("len(args) = %d, want 1", len(arr))
			}
			if _, ok := arr[0]["name"]; !ok {
				t.Error("args[0] has no \"name\" member")
			}
			if _, ok := arr[0]["muted"]; !ok {
				t.Error("args[0] has no \"muted\" member")
			}
		})
	}
}

// TestSetCompLimitCarriesAGCMode asserts that agc_mode is present and correct
// on every set_comp_limit, because a limiter threshold without it is silently
// inert and reads back unchanged — the one mistake in this package that a
// read-back check cannot catch.
func TestSetCompLimitCarriesAGCMode(t *testing.T) {
	tests := []struct {
		name   string
		mode   AGCMode
		th     *float64
		want   string
		wantTh float64
	}{
		{"limiter armed", AGCLimiter, LimiterAt(-3), "limiter", -3},
		{"limiter armed at 0 threshold", AGCLimiter, LimiterAt(0), "limiter", 0},
		{"dynamics off", AGCOff, nil, "off", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := SetCompLimit{Strip: "cam23-1", AGCMode: tt.mode, LimiterTh: tt.th}.Envelope()
			if err != nil {
				t.Fatalf("Envelope() error = %v", err)
			}
			args, ok := env["args"].(map[string]any)
			if !ok {
				t.Fatalf("args is %T, want a JSON object", env["args"])
			}
			got, ok := args["agc_mode"]
			if !ok {
				t.Fatal("args has no agc_mode; limiter_th is SILENTLY INERT without it")
			}
			if got != tt.want {
				t.Errorf("agc_mode = %v, want %q", got, tt.want)
			}
			// The wire shape is unchanged by LimiterTh becoming a pointer: a
			// nil threshold still serialises as a bare 0, exactly as the
			// vendor's own agc_mode:"off" example does.
			if args["limiter_th"] != tt.wantTh {
				t.Errorf("limiter_th = %v, want %v", args["limiter_th"], tt.wantTh)
			}
		})
	}
}

// TestLimiterAtZeroIsAThresholdNotAnAbsentLimiter is the S5 regression.
//
// The guard used to read `c.LimiterTh != 0 && c.AGCMode != AGCLimiter`, which
// is correct for every threshold except the one an engineer reaches for on a
// bus that is MEASURED to sum past full scale: a brickwall at 0 dBFS. With a
// plain float64 that request was indistinguishable from the zero value, so it
// passed validation with AGCOff, was accepted by the mixer, read back
// unchanged, and did no limiting — silently disabling the only mitigation
// this package has for BusMaster.
//
// The fix is that "no limiter" has its own spelling. These four cases are the
// whole of it, and each one fails if LimiterTh goes back to a bare float64.
func TestLimiterAtZeroIsAThresholdNotAnAbsentLimiter(t *testing.T) {
	t.Run("a brickwall at 0 dBFS with the AGC off is REFUSED", func(t *testing.T) {
		env, err := (SetCompLimit{Strip: "cam22-1", AGCMode: AGCOff, LimiterTh: LimiterAt(0)}).Envelope()
		if err == nil {
			t.Fatalf("Envelope() = %v, want a refusal: a limiter at 0 dBFS with agc_mode off is accepted, reads back unchanged and does no limiting", env)
		}
		if !strings.Contains(err.Error(), "SILENTLY INERT") {
			t.Errorf("error = %q, want it to name the silent-inertness trap", err.Error())
		}
	})

	t.Run("a brickwall at 0 dBFS with the limiter armed is ALLOWED", func(t *testing.T) {
		env, err := (SetCompLimit{Strip: "cam22-1", AGCMode: AGCLimiter, LimiterTh: LimiterAt(0)}).Envelope()
		if err != nil {
			t.Fatalf("Envelope() error = %v, want nil: a brickwall at digital full scale is a real and useful threshold", err)
		}
		args := env["args"].(map[string]any)
		if args["limiter_th"] != 0.0 {
			t.Errorf("limiter_th = %v, want 0", args["limiter_th"])
		}
	})

	t.Run("no limiter at all stays legal and is spelled nil", func(t *testing.T) {
		if _, err := (SetCompLimit{Strip: "cam22-1", AGCMode: AGCOff, PreGain: -3}).Envelope(); err != nil {
			t.Fatalf("Envelope() error = %v, want nil: agc off with no limiter is the factory state of every strip in the live frame", err)
		}
	})

	t.Run("arming the limiter without saying where it clamps is REFUSED", func(t *testing.T) {
		// The converse guess: this would send limiter_th 0 and brickwall the
		// strip at digital full scale without the caller ever naming a
		// threshold.
		env, err := (SetCompLimit{Strip: "cam22-1", AGCMode: AGCLimiter}).Envelope()
		if err == nil {
			t.Fatalf("Envelope() = %v, want a refusal: AGCLimiter with no LimiterTh sends limiter_th 0", env)
		}
		if !strings.Contains(err.Error(), "LimiterAt") {
			t.Errorf("error = %q, want it to point at LimiterAt", err.Error())
		}
	})
}

// TestEnvelopeRejects covers every refusal, because each one is a message that
// the mixer would otherwise accept and act on differently from what the caller
// meant. wantSubstr pins the error to its reason, not merely to non-nil.
func TestEnvelopeRejects(t *testing.T) {
	tests := []struct {
		name       string
		cmd        Command
		wantSubstr string
	}{
		{
			name:       "SetRouting with nil Outputs, which would un-route the strip",
			cmd:        SetRouting{Matrix: MatrixOutput, Strip: "cam22-1"},
			wantSubstr: "Outputs is nil",
		},
		{
			name:       "SetRouting with no matrix",
			cmd:        SetRouting{Strip: "cam22-1", Outputs: []Bus{BusMaster}},
			wantSubstr: "Matrix is empty",
		},
		{
			name:       "SetRouting with the stale bundle value MAIN",
			cmd:        SetRouting{Matrix: RoutingMatrix("MAIN"), Strip: "cam22-1", Outputs: []Bus{BusMaster}},
			wantSubstr: "unknown matrix",
		},
		{
			name:       "SetRouting with no strip",
			cmd:        SetRouting{Matrix: MatrixOutput, Outputs: []Bus{BusMaster}},
			wantSubstr: "Strip is empty",
		},
		{
			name:       "SetRouting to a bus that does not exist",
			cmd:        SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster, Bus("aux3")}},
			wantSubstr: `unknown bus "aux3"`,
		},
		{
			name:       "SetRouting with a duplicated bus",
			cmd:        SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster, BusMaster}},
			wantSubstr: "appears more than once",
		},
		{
			name:       "SetInputMuted with no strip",
			cmd:        SetInputMuted{Muted: true},
			wantSubstr: "Strip is empty",
		},
		{
			name:       "SetOutputMuted naming no bus at all",
			cmd:        SetOutputMuted{Muted: true},
			wantSubstr: "Buses is empty",
		},
		{
			name:       "SetOutputMuted naming a bus that does not exist",
			cmd:        SetOutputMuted{Buses: []Bus{Bus("cln")}, Muted: true},
			wantSubstr: `unknown bus "cln"`,
		},
		{
			name:       "SetChFader with no strip",
			cmd:        SetChFader{Gain: [2]float64{0, 0}},
			wantSubstr: "Strip is empty",
		},
		{
			name:       "SetCompLimit with a limiter threshold and the AGC off - SILENTLY INERT",
			cmd:        SetCompLimit{Strip: "cam23-1", AGCMode: AGCOff, LimiterTh: LimiterAt(-3)},
			wantSubstr: "SILENTLY INERT",
		},
		{
			name:       "SetCompLimit with a limiter threshold and no AGC mode at all",
			cmd:        SetCompLimit{Strip: "cam23-1", LimiterTh: LimiterAt(-3)},
			wantSubstr: "AGCMode is empty",
		},
		{
			name:       "SetCompLimit with an AGC mode outside the contract",
			cmd:        SetCompLimit{Strip: "cam23-1", AGCMode: AGCMode("compressor")},
			wantSubstr: "unknown AGCMode",
		},
		{
			name:       "SetCompLimit with no strip",
			cmd:        SetCompLimit{AGCMode: AGCLimiter, LimiterTh: LimiterAt(-3)},
			wantSubstr: "Strip is empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := tt.cmd.Envelope()
			if err == nil {
				t.Fatalf("Envelope() = %v, want an error mentioning %q", env, tt.wantSubstr)
			}
			if env != nil {
				t.Errorf("Envelope() returned a non-nil envelope %v alongside its error; a rejected command must render nothing", env)
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tt.wantSubstr)
			}
		})
	}
}

// TestSetCompLimitOffWithZeroThresholdIsAllowed guards the shape of the AGC
// rule: it is keyed on a limiter threshold being MEANT, not on the mode.
// Turning dynamics off is the factory state of every strip in the live frame
// and must stay expressible.
func TestSetCompLimitOffWithZeroThresholdIsAllowed(t *testing.T) {
	if _, err := (SetCompLimit{Strip: "cam22-1", AGCMode: AGCOff, PreGain: -3}).Envelope(); err != nil {
		t.Fatalf("Envelope() error = %v, want nil: agc off with no limiter threshold is the factory state", err)
	}
}

// ============================================================================
// URL construction and token redaction
// ============================================================================

// TestControllerURL checks the endpoint and, crucially, that the token is
// percent-encoded. A token containing a + or a / that reached the query
// unencoded would be silently corrupted and the upgrade refused 401, which
// presents as "wrong password" to an operator who typed the right one.
func TestControllerURL(t *testing.T) {
	tests := []struct {
		name  string
		host  string
		token string
		want  string
	}{
		{
			name:  "plain token",
			host:  "m2lx.example.com",
			token: "abc123",
			want:  "wss://m2lx.example.com/api/v1/switcher_controller?access_token=abc123",
		},
		{
			name:  "token with characters that must be encoded",
			host:  "m2lx.example.com",
			token: "a+b/c=d e&f",
			want:  "wss://m2lx.example.com/api/v1/switcher_controller?access_token=a%2Bb%2Fc%3Dd+e%26f",
		},
		{
			name:  "host with a port",
			host:  "10.0.0.4:8443",
			token: "abc123",
			want:  "wss://10.0.0.4:8443/api/v1/switcher_controller?access_token=abc123",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := controllerURL(tt.host, tt.token); got != tt.want {
				t.Errorf("controllerURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNewControllerRejectsBadArguments checks the fail-fast cases. None of
// them dial, so none of them can be exercised against the live instance.
func TestNewControllerRejectsBadArguments(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		token      string
		wantSubstr string
	}{
		{"empty host", "", "tok", "host is empty"},
		{"host with a scheme", "wss://m2lx.example.com", "tok", "bare host"},
		{"host with a path", "m2lx.example.com/api", "tok", "bare host"},
		{"empty token", "m2lx.example.com", "", "token is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewController(tt.host, tt.token)
			if err == nil {
				c.Close()
				t.Fatalf("NewController(%q, ...) = nil error, want one mentioning %q", tt.host, tt.wantSubstr)
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tt.wantSubstr)
			}
		})
	}
}

// TestScrubRemovesTheToken is the table-driven core of the redaction rule.
//
// The two rules exist because either alone has a hole: the literal token
// covers a bare quote of it, and the access_token= truncation covers a value
// that was re-encoded on its way into the message and so no longer matches
// the literal.
func TestScrubRemovesTheToken(t *testing.T) {
	const token = "eyJhbGciOi.SUPER-SECRET.abcdef"
	r := redactor{token: token}

	tests := []struct {
		name    string
		in      string
		wantNot string
		want    string
	}{
		{
			name:    "bare token",
			in:      "dial failed for " + token,
			wantNot: token,
			want:    "dial failed for REDACTED",
		},
		{
			name:    "token in a URL",
			in:      "Get \"wss://h/api/v1/switcher_controller?access_token=" + token + "\": timeout",
			wantNot: token,
			want:    "Get \"wss://h/api/v1/switcher_controller?access_token=REDACTED\": timeout",
		},
		{
			name:    "percent-encoded token in a URL",
			in:      "parse \"wss://h/p?access_token=" + url.QueryEscape(token) + "&x=1\": bad",
			wantNot: url.QueryEscape(token),
			want:    "parse \"wss://h/p?access_token=REDACTED&x=1\": bad",
		},
		{
			name:    "a different token, not ours, is still truncated",
			in:      "access_token=some-other-value at the end",
			wantNot: "some-other-value",
			want:    "access_token=REDACTED at the end",
		},
		{
			name:    "nothing to redact is left alone",
			in:      "websocket: bad handshake (HTTP 401 Unauthorized)",
			wantNot: token,
			want:    "websocket: bad handshake (HTTP 401 Unauthorized)",
		},
		{
			name:    "an empty access_token parameter stays empty",
			in:      "url ?access_token=&x=1",
			wantNot: token,
			want:    "url ?access_token=&x=1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.scrub(tt.in)
			if got != tt.want {
				t.Errorf("scrub() = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, tt.wantNot) {
				t.Errorf("scrub() = %q, still contains %q", got, tt.wantNot)
			}
		})
	}
}

// TestScrubWithNoTokenDoesNotCorruptEverything guards the empty-needle trap: a
// strings.ReplaceAll with an empty old string matches at every position.
func TestScrubWithNoTokenDoesNotCorruptEverything(t *testing.T) {
	const in = "websocket: bad handshake"
	if got := (redactor{}).scrub(in); got != in {
		t.Errorf("scrub() with an empty token = %q, want %q unchanged", got, in)
	}
}

// TestTokenNeverAppearsInAnyError is the assertion the brief demands, applied
// to every error this package can hand back on the connection path.
//
// It uses a real dial to a real (dead) local address, so gorilla and the net
// stack beneath it get their chance to quote the URL — which is where the
// token would leak from — rather than a substitute that could not leak
// anything.
func TestTokenNeverAppearsInAnyError(t *testing.T) {
	const token = "TOKEN-THAT-MUST-NEVER-BE-LOGGED-a+b/c"

	// A listener that is closed immediately gives a port nothing is on, so
	// the dial fails for a real reason rather than a fabricated one.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadHost := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()

	var errs []error

	if _, err := NewController(deadHost, token); err != nil {
		errs = append(errs, err)
	} else {
		t.Fatal("NewController against a closed port returned no error")
	}

	// The same URL through the send path: a controller whose dial always
	// fails, so acquire and sendOne both produce errors carrying the URL.
	c := newController(controllerURL(deadHost, token), token, dialWebSocket)
	c.backoff = []time.Duration{time.Millisecond}
	if err := c.start(); err != nil {
		errs = append(errs, err)
	}
	// start failed, so the controller never ran; build one that is up and then
	// force a write failure with a connection that reports the URL in its
	// error.
	up := newController(controllerURL(deadHost, token), token, func(context.Context, string) (wsConn, error) {
		return newLeakyConn(controllerURL(deadHost, token)), nil
	})
	up.backoff = []time.Duration{time.Millisecond}
	if err := up.start(); err != nil {
		t.Fatalf("start with a stub dial: %v", err)
	}
	defer up.Close()
	up.Arm(ArmWindow)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := up.Send(ctx, SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster}}); err != nil {
		errs = append(errs, err)
	} else {
		t.Error("Send over a connection that always fails to write returned nil")
	}

	if len(errs) == 0 {
		t.Fatal("no errors were produced; this test asserts nothing")
	}
	for i, err := range errs {
		msg := err.Error()
		if strings.Contains(msg, token) {
			t.Errorf("error %d leaks the token: %q", i, msg)
		}
		if strings.Contains(msg, url.QueryEscape(token)) {
			t.Errorf("error %d leaks the percent-encoded token: %q", i, msg)
		}
		if strings.Contains(msg, "access_token=TOKEN") {
			t.Errorf("error %d leaks the start of the token: %q", i, msg)
		}
		// %v through a wrapping chain must not resurrect it either.
		if wrapped := fmt.Sprintf("wrapped: %v", err); strings.Contains(wrapped, token) {
			t.Errorf("error %d leaks the token when formatted: %q", i, wrapped)
		}
	}
}

// leakyConn is a connection whose every write fails with an error quoting the
// full URL, token and all. It exists to prove the redaction actually runs on
// the send path rather than only on the dial path.
type leakyConn struct {
	urlStr string
	ch     chan struct{}
	once   sync.Once
}

func newLeakyConn(urlStr string) *leakyConn {
	return &leakyConn{urlStr: urlStr, ch: make(chan struct{})}
}

func (c *leakyConn) WriteMessage(int, []byte) error {
	return fmt.Errorf("write tcp -> %s: broken pipe", c.urlStr)
}

func (c *leakyConn) ReadMessage() (int, []byte, error) {
	<-c.ch // block until closed, like a real idle socket
	return 0, nil, errors.New("closed")
}

func (c *leakyConn) SetWriteDeadline(time.Time) error { return nil }

func (c *leakyConn) Close() error {
	c.once.Do(func() { close(c.ch) })
	return nil
}

// ============================================================================
// Connection lifecycle
// ============================================================================

// wsTestServer is a local WebSocket server standing in for the mixer's
// switcher_controller endpoint.
//
// It records every frame it receives, in order, and can be told to drop the
// current connection — the two things the lifecycle assertions need.
type wsTestServer struct {
	t    *testing.T
	srv  *httptest.Server
	up   websocket.Upgrader
	conn chan *websocket.Conn // one entry per accepted connection

	mu       sync.Mutex
	received [][]byte
	current  *websocket.Conn
	push     []byte // sent to each new connection when non-nil
}

func newWSTestServer(t *testing.T) *wsTestServer {
	t.Helper()
	s := &wsTestServer{t: t, conn: make(chan *websocket.Conn, 8)}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *wsTestServer) handle(w http.ResponseWriter, r *http.Request) {
	conn, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.current = conn
	push := s.push
	s.mu.Unlock()

	select {
	case s.conn <- conn:
	default:
	}

	if push != nil {
		_ = conn.WriteMessage(websocket.TextMessage, push)
	}

	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.received = append(s.received, append([]byte(nil), payload...))
		s.mu.Unlock()
	}
}

// url returns the ws:// URL of this server, with a token in the query so the
// redaction path is exercised end to end.
func (s *wsTestServer) url(token string) string {
	return "ws" + strings.TrimPrefix(s.srv.URL, "http") + controllerPath + "?" + url.Values{"access_token": {token}}.Encode()
}

// dropCurrent closes the connection the server is holding, simulating the
// mixer or a network element dropping the socket.
func (s *wsTestServer) dropCurrent() {
	s.mu.Lock()
	conn := s.current
	s.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

func (s *wsTestServer) frames() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.received...)
}

// waitForFrames blocks until the server has received at least n frames.
func (s *wsTestServer) waitForFrames(n int) [][]byte {
	s.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f := s.frames(); len(f) >= n {
			return f
		}
		time.Sleep(time.Millisecond)
	}
	s.t.Fatalf("timed out waiting for %d frames; got %d", n, len(s.frames()))
	return nil
}

// dialTest is the production dial pointed at a ws:// test server. The scheme
// is the only difference; NewController is wss-only by design, so the
// lifecycle tests construct the controller directly.
func dialTest(ctx context.Context, urlStr string) (wsConn, error) {
	d := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := d.DialContext(ctx, urlStr, nil)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// newTestController builds a started controller against s, with a backoff
// ladder short enough for a test to wait through.
//
// It returns the controller DISARMED, exactly as NewController does, because
// the whole point of the Go-side gate is that nothing opens it implicitly. The
// transport tests below are about the socket rather than the gate, so they
// call armed() to open a window first — which is also what the real caller
// has to do, so the tests exercise the real sequence.
func newTestController(t *testing.T, s *wsTestServer) *WSController {
	t.Helper()
	const token = "test-token"
	c := newController(s.url(token), token, dialTest)
	c.backoff = []time.Duration{time.Millisecond, time.Millisecond}
	if err := c.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// armed opens a write window on c for the duration of the test, and asserts
// that the controller was disarmed before it did. That assertion is the reason
// this is a helper rather than a bare c.Arm call at each site: every transport
// test now also proves that a freshly dialled controller cannot write.
func armed(t *testing.T, c *WSController) *WSController {
	t.Helper()
	if until := c.ArmedUntil(); !until.IsZero() {
		t.Fatalf("a freshly dialled controller reports ArmedUntil %s, want the zero Time: nothing may open the write gate implicitly", until)
	}
	c.Arm(ArmWindow)
	t.Cleanup(c.Disarm)
	return c
}

// TestLifecycleConnectSendDropReconnectClose is the whole connection
// lifecycle in one pass, in the order the brief names it.
func TestLifecycleConnectSendDropReconnectClose(t *testing.T) {
	s := newWSTestServer(t)
	c := armed(t, newTestController(t, s))

	// Connect.
	select {
	case <-s.conn:
	case <-time.After(5 * time.Second):
		t.Fatal("server never accepted the first connection")
	}

	// Send.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Send(ctx, SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := s.waitForFrames(1)
	want := `{"args":{"input":"cam22-1","matrix":"output","outputs":["master"]},"command":"set_routing","node":"advanced_audio_mixer"}`
	if string(got[0]) != want {
		t.Errorf("frame 0 = %s, want %s", got[0], want)
	}

	// Drop, from the far end.
	s.dropCurrent()

	// Reconnect, with no Send to provoke it: the supervisor must do this on
	// its own, because an operator who armed the drawer expects the socket to
	// still be there when they finally press the button.
	select {
	case <-s.conn:
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not reconnect after the socket dropped")
	}

	// Send again, over the replacement connection.
	if err := c.Send(ctx, SetInputMuted{Strip: "cam22-1", Muted: true}); err != nil {
		t.Fatalf("Send after reconnect: %v", err)
	}
	got = s.waitForFrames(2)
	want = `{"args":[{"muted":true,"name":"cam22-1"}],"command":"set_input_muted","node":"advanced_audio_mixer"}`
	if string(got[1]) != want {
		t.Errorf("frame 1 = %s, want %s", got[1], want)
	}

	// Close.
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// ============================================================================
// The Go-side arm gate (S3)
// ============================================================================

// TestSendIsRefusedWhileDisarmed is the S3 regression, and the property the
// whole gate exists for: the write path is CLOSED by default and no amount of
// dialling, reconnecting or previously succeeding opens it.
//
// Before this gate the arm check lived only in JavaScript. Once the drawer is
// behind a Wails binding, that binding is reachable from anything in the
// webview — a devtools console, a future code path that forgets to construct
// createWriteGate — and every one of those was an ungated write to a live
// clean feed. Nothing here removes the JS gate; the point is that a bypass now
// needs both.
func TestSendIsRefusedWhileDisarmed(t *testing.T) {
	s := newWSTestServer(t)
	c := newTestController(t, s)

	// The socket is up: this is a controller that CAN write and refuses to.
	select {
	case <-s.conn:
	case <-time.After(5 * time.Second):
		t.Fatal("server never accepted a connection")
	}

	if until := c.ArmedUntil(); !until.IsZero() {
		t.Fatalf("ArmedUntil = %s on a freshly dialled controller, want the zero Time", until)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := c.Send(ctx, SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster}})
	if err == nil {
		t.Fatal("Send on a disarmed controller returned nil; the Go-side gate is not enforcing")
	}
	if !errors.Is(err, ErrDisarmed) {
		t.Errorf("Send error = %v, want it to wrap ErrDisarmed", err)
	}
	var be *BatchError
	if !errors.As(err, &be) {
		t.Fatalf("Send returned %T, want a *BatchError", err)
	}
	if be.Written != 0 {
		t.Errorf("BatchError.Written = %d, want 0: a refused batch must write nothing", be.Written)
	}

	// The decisive assertion. Not "an error was returned" — that a routing
	// change for a live clean feed never reached the wire.
	time.Sleep(50 * time.Millisecond)
	if f := s.frames(); len(f) != 0 {
		t.Fatalf("a disarmed Send put %d frame(s) on the wire: %s", len(f), f[0])
	}

	// And that arming is what unblocks it, so the test cannot pass by the
	// socket simply being broken.
	c.Arm(ArmWindow)
	if err := c.Send(ctx, SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster}}); err != nil {
		t.Fatalf("Send after Arm: %v", err)
	}
	s.waitForFrames(1)
}

// TestArmWindowExpiresOnItsOwn covers the auto-clear. An armed write path that
// stays armed because nothing remembered to close it is the failure mode the
// timeout exists for: an operator called away, a webview that crashed with the
// gate open, a code path that forgot to disarm.
//
// The expiry is a stored deadline compared at the moment of the write, so this
// test can prove it with a short window and no timer racing anything.
func TestArmWindowExpiresOnItsOwn(t *testing.T) {
	s := newWSTestServer(t)
	c := newTestController(t, s)

	c.Arm(30 * time.Millisecond)
	if c.ArmedUntil().IsZero() {
		t.Fatal("ArmedUntil is the zero Time immediately after Arm")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Inside the window.
	if err := c.Send(ctx, SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster}}); err != nil {
		t.Fatalf("Send inside the arm window: %v", err)
	}
	s.waitForFrames(1)

	time.Sleep(60 * time.Millisecond)

	// Outside it. Nothing was called to close the window.
	if until := c.ArmedUntil(); !until.IsZero() {
		t.Errorf("ArmedUntil = %s after the window lapsed, want the zero Time: an expired window must read as disarmed, not as a stale deadline", until)
	}
	err := c.Send(ctx, SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster}})
	if !errors.Is(err, ErrDisarmed) {
		t.Fatalf("Send after the window lapsed = %v, want ErrDisarmed", err)
	}
	// The error says the window lapsed rather than only that it was shut: an
	// operator whose correction did not go out needs to know which.
	if !strings.Contains(err.Error(), "closed at") {
		t.Errorf("error = %q, want it to say the window opened and lapsed", err.Error())
	}

	time.Sleep(50 * time.Millisecond)
	if f := s.frames(); len(f) != 1 {
		t.Errorf("frames on the wire = %d, want 1: the second Send must not have written", len(f))
	}
}

// TestDisarmAndCloseShutTheWindow covers the two explicit closes.
//
// Close disarming matters beyond tidiness: Close is what a shutdown path, a
// reconnect-with-new-credentials and a failed teardown all run, and none of
// them should be able to leave a write window open behind a socket somebody
// then re-establishes.
func TestDisarmAndCloseShutTheWindow(t *testing.T) {
	t.Run("Disarm", func(t *testing.T) {
		s := newWSTestServer(t)
		c := newTestController(t, s)
		c.Arm(ArmWindow)
		c.Disarm()
		if until := c.ArmedUntil(); !until.IsZero() {
			t.Errorf("ArmedUntil = %s after Disarm, want the zero Time", until)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := c.Send(ctx, SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster}}); !errors.Is(err, ErrDisarmed) {
			t.Errorf("Send after Disarm = %v, want ErrDisarmed", err)
		}
		time.Sleep(30 * time.Millisecond)
		if f := s.frames(); len(f) != 0 {
			t.Errorf("a disarmed Send wrote %d frame(s)", len(f))
		}
	})

	t.Run("Arm(0) is Disarm", func(t *testing.T) {
		s := newWSTestServer(t)
		c := newTestController(t, s)
		c.Arm(ArmWindow)
		c.Arm(0)
		if until := c.ArmedUntil(); !until.IsZero() {
			t.Errorf("ArmedUntil = %s after Arm(0), want the zero Time", until)
		}
	})

	t.Run("Close", func(t *testing.T) {
		s := newWSTestServer(t)
		c := newTestController(t, s)
		c.Arm(ArmWindow)
		c.Close()
		if until := c.ArmedUntil(); !until.IsZero() {
			t.Errorf("ArmedUntil = %s after Close, want the zero Time: a closed Controller is never armed", until)
		}
		// Re-arming a closed controller must not resurrect the write path.
		c.Arm(ArmWindow)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err := c.Send(ctx, SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster}})
		if !errors.Is(err, ErrClosed) {
			t.Errorf("Send after Close then Arm = %v, want ErrClosed", err)
		}
	})
}

// TestArmWindowIsSaneAndBounded pins the constant itself, in both directions.
//
// The lower bound is the gesture it has to survive: arm, read the matrix,
// stage, Apply, and wait out the drawer's 4 s confirmation window, possibly
// across a reconnect whose backoff caps at 10 s. The upper bound is the thing
// it must NOT survive: a break in play or a handover, during which an armed
// write path is a hazard nobody is watching.
func TestArmWindowIsSaneAndBounded(t *testing.T) {
	if ArmWindow < 30*time.Second {
		t.Errorf("ArmWindow = %s, too short: an operator staging a correction and waiting for read-back confirmation would have the gate close mid-gesture on a live desk", ArmWindow)
	}
	if ArmWindow > 5*time.Minute {
		t.Errorf("ArmWindow = %s, too long: it must not survive a break in play, or it stops being a gate", ArmWindow)
	}
	if ArmWindow <= reconnectBackoffCap {
		t.Errorf("ArmWindow = %s is not longer than the reconnect backoff cap %s; a socket coming back up would outlive the window that permits the write waiting on it", ArmWindow, reconnectBackoffCap)
	}
}

// TestSendAppliesCommandsInOrder checks the ordering guarantee. A batch that
// removes a strip from the clean feed and then arms a limiter must arrive in
// that order; reversed, there is a window in which the limiter is armed on a
// strip still feeding CLN.
func TestSendAppliesCommandsInOrder(t *testing.T) {
	s := newWSTestServer(t)
	c := armed(t, newTestController(t, s))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmds := []Command{
		SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster}},
		SetCompLimit{Strip: "cam22-1", AGCMode: AGCLimiter, LimiterTh: LimiterAt(-3)},
		SetInputMuted{Strip: "cam22-1", Muted: false},
	}
	if err := c.Send(ctx, cmds...); err != nil {
		t.Fatalf("Send: %v", err)
	}

	frames := s.waitForFrames(3)
	wantOrder := []string{"set_routing", "set_comp_limit", "set_input_muted"}
	for i, want := range wantOrder {
		var env map[string]any
		if err := json.Unmarshal(frames[i], &env); err != nil {
			t.Fatalf("frame %d is not JSON: %v", i, err)
		}
		if env["command"] != want {
			t.Errorf("frame %d command = %v, want %q", i, env["command"], want)
		}
	}
}

// TestSendZeroCommandsIsANoOp checks that an empty batch neither errors nor
// writes. A UI that computes "nothing to correct" must be able to call Send
// unconditionally.
func TestSendZeroCommandsIsANoOp(t *testing.T) {
	s := newWSTestServer(t)
	c := newTestController(t, s)

	// Deliberately NOT armed. A batch that writes nothing cannot change a
	// mixer, so the arm gate does not stand in front of it — and a caller that
	// computed "nothing to correct" must not have to open a live write window
	// in order to say so.
	if until := c.ArmedUntil(); !until.IsZero() {
		t.Fatalf("ArmedUntil = %s, want the zero Time", until)
	}
	if err := c.Send(context.Background()); err != nil {
		t.Fatalf("Send() with no commands = %v, want nil", err)
	}
	time.Sleep(20 * time.Millisecond)
	if f := s.frames(); len(f) != 0 {
		t.Errorf("Send() with no commands wrote %d frames, want 0", len(f))
	}
}

// TestInvalidCommandInBatchWritesNothing is the half-application guarantee for
// validation failures: every envelope is built before any is written, so a bad
// command at the end of a batch cannot leave the earlier ones applied.
func TestInvalidCommandInBatchWritesNothing(t *testing.T) {
	s := newWSTestServer(t)
	c := armed(t, newTestController(t, s))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.Send(ctx,
		SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster}},
		SetInputMuted{Strip: "cam22-1", Muted: true},
		SetCompLimit{Strip: "cam22-1", AGCMode: AGCOff, LimiterTh: LimiterAt(-3)}, // silently inert
	)
	if err == nil {
		t.Fatal("Send with an inert SetCompLimit returned nil")
	}

	var be *BatchError
	if !errors.As(err, &be) {
		t.Fatalf("error is %T, want *BatchError", err)
	}
	if be.Index != 2 {
		t.Errorf("Index = %d, want 2", be.Index)
	}
	if be.Written != 0 {
		t.Errorf("Written = %d, want 0: validation must happen before ANY write", be.Written)
	}
	if be.Total != 3 {
		t.Errorf("Total = %d, want 3", be.Total)
	}
	if be.Command != "set_comp_limit" {
		t.Errorf("Command = %q, want \"set_comp_limit\"", be.Command)
	}

	time.Sleep(20 * time.Millisecond)
	if f := s.frames(); len(f) != 0 {
		t.Errorf("a batch that failed validation wrote %d frames, want 0", len(f))
	}
}

// TestMidBatchTransportFailureIsObservable is the other half of the
// half-application contract: a transport failure part-way through CAN leave
// commands applied, and the error must say exactly how many.
//
// The failing connection here refuses the second write and every write after
// it, including on the reconnect, so the batch cannot complete.
func TestMidBatchTransportFailureIsObservable(t *testing.T) {
	fc := &failAfterConn{failAfter: 1}
	c := newController("ws://test/"+controllerPath, "test-token", func(context.Context, string) (wsConn, error) {
		return fc, nil
	})
	c.backoff = []time.Duration{time.Millisecond}
	if err := c.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Close()
	c.Arm(ArmWindow)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := c.Send(ctx,
		SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster}},
		SetRouting{Matrix: MatrixOutput, Strip: "cam23-1", Outputs: []Bus{BusMaster}},
		SetRouting{Matrix: MatrixOutput, Strip: "cam24-1", Outputs: []Bus{BusMaster}},
	)
	if err == nil {
		t.Fatal("Send over a connection that fails mid-batch returned nil")
	}

	var be *BatchError
	if !errors.As(err, &be) {
		t.Fatalf("error is %T, want *BatchError", err)
	}
	if be.Index != 1 {
		t.Errorf("Index = %d, want 1", be.Index)
	}
	if be.Written != 1 {
		t.Errorf("Written = %d, want 1: the first command reached the mixer", be.Written)
	}
	if be.Total != 3 {
		t.Errorf("Total = %d, want 3", be.Total)
	}
	// The message must state the split in words, because that is what an
	// operator reads when a routing correction half-lands.
	for _, want := range []string{"command 2 of 3", "set_routing", "1 of 3 command(s) were written", "NOT rolled back"} {
		if !strings.Contains(be.Error(), want) {
			t.Errorf("BatchError message %q does not mention %q", be.Error(), want)
		}
	}
	if got := fc.writes(); got != 1 {
		t.Errorf("%d write(s) succeeded, want 1", got)
	}
}

// failAfterConn accepts failAfter successful writes and then fails every write
// forever, including after a reconnect (the dial returns this same value).
type failAfterConn struct {
	failAfter int

	mu        sync.Mutex
	succeeded int
	closedCh  chan struct{}
	initOnce  sync.Once
}

func (c *failAfterConn) init() {
	c.initOnce.Do(func() { c.closedCh = make(chan struct{}) })
}

func (c *failAfterConn) WriteMessage(int, []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.succeeded >= c.failAfter {
		return errors.New("write: connection reset by peer")
	}
	c.succeeded++
	return nil
}

func (c *failAfterConn) writes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.succeeded
}

func (c *failAfterConn) ReadMessage() (int, []byte, error) {
	c.init()
	<-c.closedCh
	return 0, nil, errors.New("closed")
}

func (c *failAfterConn) SetWriteDeadline(time.Time) error { return nil }

func (c *failAfterConn) Close() error {
	c.init()
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closedCh:
	default:
		close(c.closedCh)
	}
	return nil
}

// TestWriteFailureIsRetriedOnceOnAFreshConnection checks the single retry, and
// that it is a retry rather than a duplicate: the first connection takes the
// failed write, the replacement takes the successful one, and the mixer sees
// the command once.
func TestWriteFailureIsRetriedOnceOnAFreshConnection(t *testing.T) {
	var mu sync.Mutex
	dials := 0
	first := &failAfterConn{failAfter: 0} // fails every write
	second := &failAfterConn{failAfter: 100}

	c := newController("ws://test/"+controllerPath, "test-token", func(context.Context, string) (wsConn, error) {
		mu.Lock()
		defer mu.Unlock()
		dials++
		if dials == 1 {
			return first, nil
		}
		return second, nil
	})
	c.backoff = []time.Duration{time.Millisecond}
	if err := c.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Close()
	c.Arm(ArmWindow)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Send(ctx, SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster}}); err != nil {
		t.Fatalf("Send should have succeeded on the retry: %v", err)
	}
	if got := first.writes(); got != 0 {
		t.Errorf("first connection took %d writes, want 0", got)
	}
	if got := second.writes(); got != 1 {
		t.Errorf("replacement connection took %d writes, want exactly 1 (a retry, not a duplicate)", got)
	}
}

// TestCloseIsIdempotentAndDoesNotDeadlock calls Close from several goroutines
// at once while the reader is parked on a socket that never speaks — which,
// MEASURED, is exactly what the live socket does.
func TestCloseIsIdempotentAndDoesNotDeadlock(t *testing.T) {
	s := newWSTestServer(t)
	c := newTestController(t, s)

	select {
	case <-s.conn:
	case <-time.After(5 * time.Second):
		t.Fatal("server never accepted a connection")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.Close()
			}()
		}
		wg.Wait()
		// And again, serially, long after the reader is gone.
		c.Close()
		c.Close()
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close deadlocked")
	}
}

// TestCloseDuringReconnectDoesNotHang is a regression test for a deadlock this
// package actually had.
//
// The window is between the supervisor's dial returning a connection and its
// reader parking on that connection. A Close landing in there used to set
// closed, find c.conn still nil, close nothing — and then wait forever for a
// supervisor that had just parked on a socket nobody was going to shut. On the
// real socket, which is MEASURED to push nothing for 45 seconds at a stretch,
// that read never returns on its own, so the hang was permanent: an operator
// disarming the drawer would wedge the application.
//
// The test drives the window directly. The dial blocks until released, and
// Close is called while it is blocked, so the connection is manufactured
// strictly after Close has begun.
func TestCloseDuringReconnectDoesNotHang(t *testing.T) {
	release := make(chan struct{})
	dialing := make(chan struct{}, 4)
	var once sync.Once

	c := newController("ws://test/"+controllerPath, "tok", func(ctx context.Context, _ string) (wsConn, error) {
		var first bool
		once.Do(func() { first = true })
		if first {
			return newBlockingConn(), nil
		}
		select {
		case dialing <- struct{}{}:
		default:
		}
		<-release
		// Deliberately ignores ctx: this returns a live connection AFTER
		// Close has started, which is the whole point.
		return newBlockingConn(), nil
	})
	c.backoff = []time.Duration{time.Millisecond}
	if err := c.start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Take the first connection down so the supervisor goes round to redial.
	c.mu.Lock()
	first := c.conn
	c.mu.Unlock()
	first.Close()

	select {
	case <-dialing:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor never redialled")
	}

	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()

	// Wait until Close has definitely marked the controller closed before
	// letting the dial return.
	//
	// This is not tidiness. Close sets closed and reads the connection it must
	// shut under ONE acquisition of the mutex, so observing closed here proves
	// Close has already looked for a connection and found none. Releasing the
	// dial only now puts the new connection strictly inside the window. Without
	// this wait the test passes against the deadlock it exists to catch,
	// because the dial usually wins the race and hands Close a connection it
	// can see — verified by reverting the fix in setConn and watching this test
	// go green.
	if !waitFor(func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.closed
	}) {
		t.Fatal("Close never marked the controller closed")
	}
	close(release)

	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung waiting for a supervisor that had just parked on a fresh connection")
	}
}

// waitFor polls cond until it holds, and reports whether it did within a
// generous ceiling. Polling is used rather than a channel because the
// conditions being waited on are states of the controller's own mutex-guarded
// fields, which is exactly what a caller racing it would observe.
func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// blockingConn never returns from ReadMessage until it is closed, which is how
// the live switcher_controller socket behaves: MEASURED, 45 seconds with no
// frame and the connection still open.
type blockingConn struct {
	ch   chan struct{}
	once sync.Once
}

func newBlockingConn() *blockingConn { return &blockingConn{ch: make(chan struct{})} }

func (c *blockingConn) WriteMessage(int, []byte) error { return nil }

func (c *blockingConn) ReadMessage() (int, []byte, error) {
	<-c.ch
	return 0, nil, errors.New("closed")
}

func (c *blockingConn) SetWriteDeadline(time.Time) error { return nil }

func (c *blockingConn) Close() error {
	c.once.Do(func() { close(c.ch) })
	return nil
}

// TestSendAfterCloseFailsAndWritesNothing checks that a closed controller is
// inert rather than reconnecting behind the caller's back.
func TestSendAfterCloseFailsAndWritesNothing(t *testing.T) {
	s := newWSTestServer(t)
	c := armed(t, newTestController(t, s))
	c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := c.Send(ctx, SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster}})
	if err == nil {
		t.Fatal("Send after Close returned nil")
	}
	if !errors.Is(err, ErrClosed) {
		t.Errorf("errors.Is(err, ErrClosed) = false for %v", err)
	}
	var be *BatchError
	if !errors.As(err, &be) {
		t.Fatalf("error is %T, want *BatchError", err)
	}
	if be.Written != 0 {
		t.Errorf("Written = %d, want 0", be.Written)
	}
	if f := s.frames(); len(f) != 0 {
		t.Errorf("Send after Close wrote %d frames, want 0", len(f))
	}
}

// TestSendWaitsForReconnectAndRespectsContext checks that a Send issued while
// the socket is down waits for it rather than failing instantly, and that the
// wait is bounded by the caller's context rather than by anything internal.
func TestSendWaitsForReconnectAndRespectsContext(t *testing.T) {
	block := make(chan struct{})
	var once sync.Once
	c := newController("ws://test/"+controllerPath, "test-token", func(ctx context.Context, _ string) (wsConn, error) {
		var first bool
		once.Do(func() { first = true })
		if first {
			return &failAfterConn{failAfter: 100}, nil
		}
		// Every redial blocks until the test releases it.
		select {
		case <-block:
			return &failAfterConn{failAfter: 100}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	c.backoff = []time.Duration{time.Millisecond}
	if err := c.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		close(block)
		c.Close()
	}()
	c.Arm(ArmWindow)

	// Take the socket down and leave it down.
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	conn.Close()
	c.dropConn(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := c.Send(ctx, SetRouting{Matrix: MatrixOutput, Strip: "cam22-1", Outputs: []Bus{BusMaster}})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Send with the socket down and no reconnect returned nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false for %v", err)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("Send gave up after %v; it must wait for the reconnect until the caller's context says otherwise", elapsed)
	}
}

// ============================================================================
// What the peer says
// ============================================================================

// TestPeerMessagesAreRetainedAndClassified checks that inbound frames are kept
// and flagged rather than discarded — and, by the absence of any correlation
// assertion, documents that they are not attributed to a command.
func TestPeerMessagesAreRetainedAndClassified(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{"REST-style detail", `{"detail":"Output source is invalid."}`, true},
		{"error member", `{"error":"nope"}`, true},
		{"numeric status at 400", `{"status":400}`, true},
		{"numeric code above 400", `{"code":500,"x":1}`, true},
		{"message id", `{"message_id":"MESSAGE.9301"}`, true},
		{"a healthy 200", `{"status":200}`, false},
		{"an unremarkable object", `{"ok":true}`, false},
		{"not JSON at all", `pong`, false},
		{"a JSON array", `[1,2,3]`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errorish([]byte(tt.payload)); got != tt.want {
				t.Errorf("errorish(%s) = %v, want %v", tt.payload, got, tt.want)
			}
		})
	}
}

// TestPeerMessagesSurfaceOverTheWire checks the same thing end to end: a frame
// pushed by the server is readable through PeerMessages, which is the only way
// anything the mixer says can reach a human.
func TestPeerMessagesSurfaceOverTheWire(t *testing.T) {
	s := newWSTestServer(t)
	s.mu.Lock()
	s.push = []byte(`{"detail":"Output source is invalid."}`)
	s.mu.Unlock()

	c := newTestController(t, s)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs := c.PeerMessages()
		if len(msgs) > 0 {
			if string(msgs[0].Payload) != `{"detail":"Output source is invalid."}` {
				t.Errorf("payload = %s, want the frame verbatim", msgs[0].Payload)
			}
			if !msgs[0].LooksLikeError {
				t.Error("LooksLikeError = false for a frame carrying a detail member")
			}
			if msgs[0].At.IsZero() {
				t.Error("At is zero")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the pushed frame never appeared in PeerMessages")
}

// TestPeerMessagesAreBounded checks the ring, so an unexpectedly chatty peer
// cannot grow this slice for a whole match.
func TestPeerMessagesAreBounded(t *testing.T) {
	c := newController("ws://test/"+controllerPath, "tok", nil)
	for i := 0; i < maxPeerMessages*3; i++ {
		c.recordPeer([]byte(fmt.Sprintf(`{"n":%d}`, i)))
	}
	msgs := c.PeerMessages()
	if len(msgs) != maxPeerMessages {
		t.Fatalf("len(PeerMessages()) = %d, want %d", len(msgs), maxPeerMessages)
	}
	// The ring must keep the NEWEST, since those are the ones nearest whatever
	// just went wrong.
	wantFirst := fmt.Sprintf(`{"n":%d}`, maxPeerMessages*3-maxPeerMessages)
	if string(msgs[0].Payload) != wantFirst {
		t.Errorf("oldest retained = %s, want %s", msgs[0].Payload, wantFirst)
	}
}

// TestBackoffDuration pins the ladder's shape, including the cap.
func TestBackoffDuration(t *testing.T) {
	ladder := []time.Duration{time.Second, 2 * time.Second}
	tests := []struct {
		name string
		n    int
		want time.Duration
	}{
		{"before the first attempt there is no delay", 0, 0},
		{"negative is treated as no delay", -1, 0},
		{"first step", 1, time.Second},
		{"second step", 2, 2 * time.Second},
		{"past the end holds at the cap", 3, reconnectBackoffCap},
		{"far past the end still holds at the cap", 99, reconnectBackoffCap},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backoffDuration(ladder, tt.n); got != tt.want {
				t.Errorf("backoffDuration(%d) = %v, want %v", tt.n, got, tt.want)
			}
		})
	}
}

// TestProductionBackoffIsSaneAndBounded guards the real ladder, which is the
// one an operator waits through.
func TestProductionBackoffIsSaneAndBounded(t *testing.T) {
	if len(reconnectBackoff) == 0 {
		t.Fatal("reconnectBackoff is empty; a drop would busy-loop the dialler")
	}
	prev := time.Duration(0)
	for i, d := range reconnectBackoff {
		if d <= 0 {
			t.Errorf("reconnectBackoff[%d] = %v, want a positive delay", i, d)
		}
		if d < prev {
			t.Errorf("reconnectBackoff[%d] = %v is shorter than its predecessor %v", i, d, prev)
		}
		prev = d
	}
	if prev > reconnectBackoffCap {
		t.Errorf("the ladder ends at %v, above the cap %v", prev, reconnectBackoffCap)
	}
}

// ============================================================================
// Live probe
// ============================================================================

// TestLiveControllerAcceptsAConnection dials the real dev event and confirms
// the socket comes up and stays up, WITHOUT SENDING ANYTHING.
//
// It is skipped unless M2LX_LIVE_HOST and M2LX_LIVE_TOKEN are set, so it never
// runs in an ordinary suite and never touches the live event by accident.
//
// It is read-only by construction: it opens a controller, waits, asserts the
// socket is still connected, and closes. It calls Send on nothing. This work
// package was prohibited from writing to the live mixer and this test is the
// boundary of what it may do.
//
// Run:
//
//	M2LX_LIVE_HOST=<host> M2LX_LIVE_TOKEN=<token> \
//	  go test ./internal/mixer/ -run TestLiveControllerAcceptsAConnection -v
//
// MEASURED with exactly this on 2026-07-31: the socket connected and reported
// zero peer messages.
func TestLiveControllerAcceptsAConnection(t *testing.T) {
	host := os.Getenv("M2LX_LIVE_HOST")
	token := os.Getenv("M2LX_LIVE_TOKEN")
	if host == "" || token == "" {
		t.Skip("set M2LX_LIVE_HOST and M2LX_LIVE_TOKEN to run the live probe")
	}

	ctrl, err := NewController(host, token)
	if err != nil {
		t.Fatalf("NewController against the live event: %v", err)
	}
	defer ctrl.Close()

	c, ok := ctrl.(*WSController)
	if !ok {
		t.Fatalf("NewController returned %T, want *WSController", ctrl)
	}

	// Hold the socket open and see whether the peer says anything unbidden.
	time.Sleep(10 * time.Second)

	c.mu.Lock()
	connected := c.conn != nil
	c.mu.Unlock()
	if !connected {
		t.Error("the live socket dropped within 10 s of connecting")
	}

	msgs := c.PeerMessages()
	t.Logf("live socket: connected=%v, unsolicited frames in 10 s=%d", connected, len(msgs))
	for i, m := range msgs {
		t.Logf("  peer frame %d (looksLikeError=%v): %s", i, m.LooksLikeError, m.Payload)
	}
}
