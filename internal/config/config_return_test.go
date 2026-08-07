// Tests for the SRT return path's configuration: the five fields, their
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
	// return HTTP 400 — so the three programme buses are the whole menu.
	d := Defaults()
	if d.ReturnSource != "webrtc" {
		t.Errorf("default returnSource = %q, want \"webrtc\"; changing which return path a "+
			"commentary position uses on its next launch is not a thing to do by default", d.ReturnSource)
	}
	if d.ReturnChannel != "stereo" {
		t.Errorf("default returnChannel = %q, want \"stereo\"", d.ReturnChannel)
	}
	if d.SRTReturnPort != 40501 {
		t.Errorf("default srtReturnPort = %d, want 40501 (Output 1, src=pgm)", d.SRTReturnPort)
	}
	if d.SRTReturnPBKeyLen != 0 {
		t.Errorf("default srtReturnPBKeyLen = %d, want 0 (no encryption negotiated)",
			d.SRTReturnPBKeyLen)
	}
	if d.HeadphoneEndpointID != "" {
		t.Errorf("default headphoneEndpointId = %q, want empty (the Windows default playback device)",
			d.HeadphoneEndpointID)
	}
}

// TestTheReturnDialsTheDirtyProgrammeOutput pins the pair of defaults that a
// commentary position actually depends on, in one place, with the reason.
//
// This is a regression guard on a fault that has already happened once and cost
// an afternoon. The default was 40503 for a revision — Output 3, src=cln, which
// the live instance reports as encrypted=true. The return dialled it with no
// passphrase, the SRT handshake was refused every time, and the reconnect ladder
// retried for ever. Two facts, measured by dialling both by hand:
//
//	40501  src=pgm  encrypted=false  negotiates video/x-h265 hvc1 1920x1080 50/1
//	                                 and audio/mpeg mpegversion=4 base-profile=lc
//	40503  src=cln  encrypted=true   nothing at all without a key
//
// The requirement is the DIRTY programme picture, because that is what a
// commentator watches; clean audio comes from the WebRTC monitor's mid 2, which
// is the same bus by a different route. So the port and the key length go
// together and are asserted together: 40501 with pbkeylen 0 connects on a stock
// instance with nothing typed in, and either half moving on its own re-creates
// the fault.
func TestTheReturnDialsTheDirtyProgrammeOutput(t *testing.T) {
	if DefaultSRTReturnPort != 40501 {
		t.Errorf("DefaultSRTReturnPort = %d, want 40501 — Output 1, src=pgm, the DIRTY "+
			"programme feed. 40503 is src=cln and measured encrypted=true; defaulting to it "+
			"is the configuration that failed every handshake for an afternoon.",
			DefaultSRTReturnPort)
	}
	if DefaultSRTReturnPBKeyLen != 0 {
		t.Errorf("DefaultSRTReturnPBKeyLen = %d, want 0. Output 1 measured encrypted=false, "+
			"so asking for encryption by default makes a first run fail against the very "+
			"output the port default points at.", DefaultSRTReturnPBKeyLen)
	}

	// And through the accessor, which is what App.returnOpts actually calls: a
	// config.json written before either field existed must still dial 40501.
	empty := &Config{}
	if got := empty.EffectiveSRTReturnPort(); got != 40501 {
		t.Errorf("(&Config{}).EffectiveSRTReturnPort() = %d, want 40501", got)
	}
	if empty.SRTReturnPBKeyLen != 0 {
		t.Errorf("(&Config{}).SRTReturnPBKeyLen = %d, want 0", empty.SRTReturnPBKeyLen)
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
	if c.SRTReturnPBKeyLen != DefaultSRTReturnPBKeyLen {
		t.Errorf("srtReturnPBKeyLen = %d, want the default %d",
			c.SRTReturnPBKeyLen, DefaultSRTReturnPBKeyLen)
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
		// The three key lengths SRT's AES-CTR supports, with 0 meaning no
		// encryption is negotiated at all.
		{"return pbkeylen 0", func(c *Config) { c.SRTReturnPBKeyLen = 0 }, false, ""},
		{"return pbkeylen 16", func(c *Config) { c.SRTReturnPBKeyLen = 16 }, false, ""},
		{"return pbkeylen 32", func(c *Config) { c.SRTReturnPBKeyLen = 32 }, false, ""},

		{"unknown returnSource", func(c *Config) { c.ReturnSource = "kvs" }, true, "returnSource"},
		{"unknown returnChannel", func(c *Config) { c.ReturnChannel = "centre" }, true, "returnChannel"},
		{"port too high", func(c *Config) { c.SRTReturnPort = 70000 }, true, "srtReturnPort"},
		{"port negative", func(c *Config) { c.SRTReturnPort = -1 }, true, "srtReturnPort"},
		{"no host anywhere", func(c *Config) { c.M2LXHost = "" }, true, "host"},
		// 24 is a plausible typo for a key length and libsrt does not accept it.
		{"return pbkeylen 24", func(c *Config) { c.SRTReturnPBKeyLen = 24 }, true, "srtReturnPBKeyLen"},
		// Bits rather than bytes: 128 and 256 are what the setting is called
		// everywhere else, and pbkeylen is in BYTES.
		{"return pbkeylen 128 is bits, not bytes", func(c *Config) { c.SRTReturnPBKeyLen = 128 }, true, "srtReturnPBKeyLen"},
		{"return pbkeylen negative", func(c *Config) { c.SRTReturnPBKeyLen = -16 }, true, "srtReturnPBKeyLen"},
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
	c := &Config{ReturnSource: "kvs", ReturnChannel: "centre", SRTReturnPort: 99999, SRTReturnPBKeyLen: 24}
	err := c.ValidateReturn()
	if err == nil {
		t.Fatal("ValidateReturn() accepted an entirely invalid configuration")
	}
	for _, want := range []string{"returnSource", "returnChannel", "srtReturnPort", "srtReturnPBKeyLen", "host"} {
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
	c.SRTReturnPBKeyLen = 24
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
	c.SRTReturnPBKeyLen = 32
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
		got.SRTReturnPBKeyLen != 32 ||
		got.HeadphoneEndpointID != c.HeadphoneEndpointID {
		t.Fatalf("round trip lost a return field: %+v", got)
	}
}

// TestSRTReturnPBKeyLenIsNamedTheWayTheFrontendSpellsIt pins the JSON key.
//
// The Settings screen writes this field by name across the Wails boundary, and
// a config.json key and a JavaScript property that disagree do not fail: the Go
// side simply never sees the value, keeps the zero it started with, and the
// return negotiates no encryption while the screen shows 32. That is the same
// silent-mismatch shape as the two headphone identifiers, and it is pinned the
// same way.
func TestSRTReturnPBKeyLenIsNamedTheWayTheFrontendSpellsIt(t *testing.T) {
	c := Defaults()
	c.SRTReturnPBKeyLen = 16
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"srtReturnPBKeyLen":16`) {
		t.Fatalf("config.json does not carry \"srtReturnPBKeyLen\":16; got %s", data)
	}

	// And the send path's key length is still its own separate key, so that
	// setting one cannot move the other.
	if !strings.Contains(string(data), `"pbkeylen"`) {
		t.Fatal("config.json no longer carries the send path's \"pbkeylen\"")
	}
}

// TestTheTwoKeyLengthsAreIndependent is the config-level half of the property
// internal/secrets pins for the two passphrases: M2L-X sets encryption per
// output, so the send path and the return path routinely need different
// answers, and a shared field means the operator cannot describe what is
// actually in front of them.
func TestTheTwoKeyLengthsAreIndependent(t *testing.T) {
	c := Defaults()
	c.M2LXHost = "m2lx.example.com"
	c.SRTPort = 40001
	c.PBKeyLen = 0           // the commentary INPUT takes no passphrase
	c.SRTReturnPBKeyLen = 32 // the OUTPUT being monitored does

	if err := c.ValidateReturn(); err != nil {
		t.Fatalf("ValidateReturn() = %v; an unencrypted send with an encrypted return "+
			"is the measured arrangement, not a mistake", err)
	}

	c.Alias = "commentary"
	c.EventID = "dl9-5p5ah0bd-empd"
	c.AudioDeviceID = "{0.0.1.00000000}.{bbbb}"
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v; the return's key length must not gate Start", err)
	}
}
