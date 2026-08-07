// Tests for the SRT return path's configuration: the four fields, their
// defaults, their accessors and ValidateReturn.
//
// They are in their own file rather than appended to config_test.go so that the
// one property that matters most about them stays visible as a group: the return
// settings are validated SEPARATELY from the settings that gate Start, and a
// broken return configuration must never be a reason a match does not go on air.
// See TestValidateDoesNotGateStartOnReturnFields.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaults_ReturnFields(t *testing.T) {
	// Asserted against literals rather than against the constants, so that
	// changing a constant is a two-place decision rather than a silent one.
	//
	// The port in particular is a MEASURED fact about the live instance, not a
	// preference. M2L-X's output source field accepts only
	// pgm | pvw | cln | <router input id> — aux1, aux2, master and mon1 all
	// return HTTP 400 — so Output 3 on 40503 is the only way to hear CLN, and
	// CLN is the bus carrying FX hard-left and comms hard-right.
	d := Defaults()
	if d.ReturnSource != "webrtc" {
		t.Errorf("default returnSource = %q, want \"webrtc\"; changing which return path a "+
			"commentary position uses on its next launch is not a thing to do by default", d.ReturnSource)
	}
	if d.ReturnChannel != "stereo" {
		t.Errorf("default returnChannel = %q, want \"stereo\"", d.ReturnChannel)
	}
	if d.SRTReturnPort != 40503 {
		t.Errorf("default srtReturnPort = %d, want 40503 (Output 3, src=cln)", d.SRTReturnPort)
	}
	if d.HeadphoneEndpointID != "" {
		t.Errorf("default headphoneEndpointId = %q, want empty (the Windows default playback device)",
			d.HeadphoneEndpointID)
	}
}

func TestDefaults_ReturnFieldsPassValidateReturn(t *testing.T) {
	// A freshly defaulted configuration with a host must be able to monitor. If
	// this fails, the default of some return field has drifted out of the range
	// its own validator accepts.
	c := Defaults()
	c.M2LXHost = "m2lx.example.com"
	if err := c.ValidateReturn(); err != nil {
		t.Fatalf("Defaults() plus a host does not pass ValidateReturn: %v", err)
	}
}

func TestLoad_ReturnFieldsAbsentFromFileTakeDefaults(t *testing.T) {
	// The upgrade case: a config.json written before the return path existed.
	// Unmarshalling onto a Defaults()-populated struct is what stops
	// returnChannel becoming "" and srtReturnPort becoming 0 — neither of which
	// is a usable value — on every installation that already exists.
	dir := withAppData(t)
	if err := os.MkdirAll(filepath.Join(dir, AppDataDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"m2lxHost":"m2lx.example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ReturnSource != DefaultReturnSource {
		t.Errorf("returnSource = %q, want the default %q", c.ReturnSource, DefaultReturnSource)
	}
	if c.ReturnChannel != DefaultReturnChannel {
		t.Errorf("returnChannel = %q, want the default %q", c.ReturnChannel, DefaultReturnChannel)
	}
	if c.SRTReturnPort != DefaultSRTReturnPort {
		t.Errorf("srtReturnPort = %d, want the default %d", c.SRTReturnPort, DefaultSRTReturnPort)
	}
}

func TestEffectiveReturnAccessorsSubstituteForEmptyValues(t *testing.T) {
	// Load fills in keys that are ABSENT. An explicitly empty string — a
	// hand-edited file, or a Settings screen that saved a half-filled form —
	// survives that, so the accessors have to substitute as well.
	c := &Config{}
	if got := c.EffectiveReturnSource(); got != DefaultReturnSource {
		t.Errorf("EffectiveReturnSource() = %q, want %q", got, DefaultReturnSource)
	}
	if got := c.EffectiveReturnChannel(); got != DefaultReturnChannel {
		t.Errorf("EffectiveReturnChannel() = %q, want %q", got, DefaultReturnChannel)
	}
	if got := c.EffectiveSRTReturnPort(); got != DefaultSRTReturnPort {
		t.Errorf("EffectiveSRTReturnPort() = %d, want %d", got, DefaultSRTReturnPort)
	}

	// Whitespace counts as empty. An operator who selected a value and then
	// deleted it left a blank field, not a setting named " ".
	c = &Config{ReturnSource: "  ", ReturnChannel: "\t"}
	if got := c.EffectiveReturnSource(); got != DefaultReturnSource {
		t.Errorf("EffectiveReturnSource() on whitespace = %q, want %q", got, DefaultReturnSource)
	}
	if got := c.EffectiveReturnChannel(); got != DefaultReturnChannel {
		t.Errorf("EffectiveReturnChannel() on whitespace = %q, want %q", got, DefaultReturnChannel)
	}
}

func TestEffectiveReturnAccessorsReturnAnExplicitSetting(t *testing.T) {
	c := &Config{
		ReturnSource:  ReturnSourceSRT,
		ReturnChannel: ReturnChannelRight,
		SRTReturnPort: 40507,
	}
	if got := c.EffectiveReturnSource(); got != ReturnSourceSRT {
		t.Errorf("EffectiveReturnSource() = %q, want %q", got, ReturnSourceSRT)
	}
	if got := c.EffectiveReturnChannel(); got != ReturnChannelRight {
		t.Errorf("EffectiveReturnChannel() = %q, want %q", got, ReturnChannelRight)
	}
	if got := c.EffectiveSRTReturnPort(); got != 40507 {
		t.Errorf("EffectiveSRTReturnPort() = %d, want 40507", got)
	}
}

func TestUsesSRTReturn(t *testing.T) {
	if (&Config{}).UsesSRTReturn() {
		t.Error("an unconfigured Config must not select the SRT return; the default is webrtc")
	}
	if (&Config{ReturnSource: ReturnSourceWebRTC}).UsesSRTReturn() {
		t.Error("returnSource webrtc must not select the SRT return")
	}
	if !(&Config{ReturnSource: ReturnSourceSRT}).UsesSRTReturn() {
		t.Error("returnSource srt must select the SRT return")
	}
	// The comparison is exact. Running the SRT path on a near-miss would put two
	// returns in the same headphones a few hundred milliseconds apart, which is
	// the outcome this setting exists to prevent.
	if (&Config{ReturnSource: "SRT"}).UsesSRTReturn() {
		t.Error("returnSource is compared exactly; \"SRT\" is a typo, not a selection")
	}
}

func TestValidateReturn(t *testing.T) {
	base := func() *Config {
		c := Defaults()
		c.M2LXHost = "m2lx.example.com"
		return c
	}

	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
		wantSub string
	}{
		{"defaults plus a host", func(c *Config) {}, false, ""},
		{"srt selected", func(c *Config) { c.ReturnSource = ReturnSourceSRT }, false, ""},
		{"left channel", func(c *Config) { c.ReturnChannel = ReturnChannelLeft }, false, ""},
		{"right channel", func(c *Config) { c.ReturnChannel = ReturnChannelRight }, false, ""},
		{"srtHost overrides an empty m2lxHost", func(c *Config) {
			c.M2LXHost = ""
			c.SRTHost = "srt.example.com"
		}, false, ""},

		{"unknown returnSource", func(c *Config) { c.ReturnSource = "kvs" }, true, "returnSource"},
		{"unknown returnChannel", func(c *Config) { c.ReturnChannel = "centre" }, true, "returnChannel"},
		{"port too high", func(c *Config) { c.SRTReturnPort = 70000 }, true, "srtReturnPort"},
		{"port negative", func(c *Config) { c.SRTReturnPort = -1 }, true, "srtReturnPort"},
		{"no host anywhere", func(c *Config) { c.M2LXHost = "" }, true, "host"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.modify(c)
			err := c.ValidateReturn()
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateReturn() = nil, want an error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateReturn() = %v, want nil", err)
			}
			if tt.wantSub != "" && !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("error %q does not name %q", err, tt.wantSub)
			}
		})
	}
}

func TestValidateReturn_JoinsAllProblems(t *testing.T) {
	c := &Config{ReturnSource: "kvs", ReturnChannel: "centre", SRTReturnPort: 99999}
	err := c.ValidateReturn()
	if err == nil {
		t.Fatal("ValidateReturn() accepted an entirely invalid configuration")
	}
	for _, want := range []string{"returnSource", "returnChannel", "srtReturnPort", "host"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error %q does not name %q; the Settings screen shows every "+
				"problem at once rather than one edit-fail cycle at a time", err, want)
		}
	}
}

func TestValidateDoesNotGateStartOnReturnFields(t *testing.T) {
	// THE separation that matters, and the reason there are two validators.
	//
	// A mistyped returnChannel must never be the reason a match does not go on
	// air. This is the same judgement the statusKey field records: requiring a
	// field for Start that Start does not need made the application unstartable
	// for a reason that had nothing to do with sending. Validate gates the
	// contribution feed, ValidateReturn gates the monitor, and folding them
	// together would undo that.
	c := validConfig()
	c.ReturnSource = "kvs"
	c.ReturnChannel = "sideways"
	c.SRTReturnPort = -99
	c.HeadphoneEndpointID = ""

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v; a broken RETURN configuration must not stop the "+
			"contribution feed from starting", err)
	}
	if err := c.ValidateReturn(); err == nil {
		t.Fatal("ValidateReturn() accepted the same broken configuration; " +
			"the separation is only safe if the return validator actually catches it")
	}
}

func TestValidateReturnDoesNotRequireAHeadphoneEndpoint(t *testing.T) {
	// Empty means wasapi2sink opens the Windows default playback device, which
	// on a commentary position is very often the right one and is always better
	// than refusing to monitor at all.
	c := Defaults()
	c.M2LXHost = "m2lx.example.com"
	c.ReturnSource = ReturnSourceSRT
	c.HeadphoneEndpointID = ""
	if err := c.ValidateReturn(); err != nil {
		t.Fatalf("ValidateReturn() = %v; headphoneEndpointId is optional", err)
	}
}

func TestTheTwoHeadphoneIdentifiersAreSeparateFields(t *testing.T) {
	// They identify the same physical output and are different KINDS of
	// identifier: a browser mediaDeviceId for setSinkId on the WebRTC path, and
	// a WASAPI IMMDevice endpoint GUID for wasapi2sink on the SRT path. Neither
	// can be converted into the other, and using one where the other belongs
	// fails SILENTLY in both directions — the element or the API falls back to
	// the system default and the commentator gets audio in the wrong ears with
	// no diagnostic anywhere.
	//
	// This test exists so that a future tidy-up which merges them has to delete
	// it first.
	c := Defaults()
	c.HeadphoneDeviceID = "browser-media-device-id"
	c.HeadphoneEndpointID = "{0.0.0.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}"

	if c.HeadphoneDeviceID == c.HeadphoneEndpointID {
		t.Fatal("the two headphone identifiers must be independently settable")
	}

	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"headphoneDeviceId"`, `"headphoneEndpointId"`} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("config.json is missing %s; both identifiers are persisted", key)
		}
	}
}

func TestReturnFieldsRoundTripThroughTheFile(t *testing.T) {
	withAppData(t)

	c := Defaults()
	c.M2LXHost = "m2lx.example.com"
	c.ReturnSource = ReturnSourceSRT
	c.ReturnChannel = ReturnChannelLeft
	c.SRTReturnPort = 40503
	c.HeadphoneEndpointID = "{0.0.0.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.ReturnSource != ReturnSourceSRT ||
		got.ReturnChannel != ReturnChannelLeft ||
		got.SRTReturnPort != 40503 ||
		got.HeadphoneEndpointID != c.HeadphoneEndpointID {
		t.Fatalf("round trip lost a return field: %+v", got)
	}
}
