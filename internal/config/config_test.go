package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withAppData points %APPDATA% at a fresh temp directory for the duration of
// the test, so Path/Load/Save exercise a real filesystem without touching
// the developer's actual %APPDATA%\WSLComms.
func withAppData(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	return dir
}

func TestPath(t *testing.T) {
	dir := withAppData(t)

	got, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	want := filepath.Join(dir, "WSLComms", "config.json")
	if got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}

	// Path must not create anything.
	if _, err := os.Stat(filepath.Join(dir, "WSLComms")); !os.IsNotExist(err) {
		t.Errorf("Path() must not create the WSLComms directory, stat err = %v", err)
	}
}

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	withAppData(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() on missing file: error = %v, want nil (first run is not an error)", err)
	}

	want := Defaults()
	if *cfg != *want {
		t.Errorf("Load() on missing file = %+v, want Defaults() = %+v", *cfg, *want)
	}
}

func TestLoad_PartialFileKeepsDefaultsForAbsentFields(t *testing.T) {
	dir := withAppData(t)
	confDir := filepath.Join(dir, "WSLComms")
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Only m2lxHost and alias are present. Every other field, including the
	// nested MonitorTile struct, must come from Defaults(), not the zero
	// value.
	const partial = `{"m2lxHost":"m2lx.example.com","alias":"commentator1"}`
	if err := os.WriteFile(filepath.Join(confDir, "config.json"), []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.M2LXHost != "m2lx.example.com" {
		t.Errorf("M2LXHost = %q, want %q", cfg.M2LXHost, "m2lx.example.com")
	}
	if cfg.Alias != "commentator1" {
		t.Errorf("Alias = %q, want %q", cfg.Alias, "commentator1")
	}
	if cfg.SRTLatencyMs != DefaultSRTLatencyMs {
		t.Errorf("SRTLatencyMs = %d, want default %d", cfg.SRTLatencyMs, DefaultSRTLatencyMs)
	}
	if cfg.ReturnMid != DefaultReturnMid {
		t.Errorf("ReturnMid = %d, want default %d", cfg.ReturnMid, DefaultReturnMid)
	}
	if cfg.ReturnGainDB != DefaultReturnGainDB {
		t.Errorf("ReturnGainDB = %v, want default %v", cfg.ReturnGainDB, DefaultReturnGainDB)
	}
	if cfg.SlatePath != DefaultSlateFilename {
		t.Errorf("SlatePath = %q, want default %q", cfg.SlatePath, DefaultSlateFilename)
	}
	if cfg.MonitorTile != DefaultMonitorTile {
		t.Errorf("MonitorTile = %+v, want default %+v", cfg.MonitorTile, DefaultMonitorTile)
	}
}

func TestLoad_PartialNestedTileMergesFieldByField(t *testing.T) {
	dir := withAppData(t)
	confDir := filepath.Join(dir, "WSLComms")
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// monitorTile present but only overriding x: y, w, h must still come
	// from the default tile, exercising encoding/json's recursive merge into
	// an already-populated struct.
	const partial = `{"monitorTile":{"x":100}}`
	if err := os.WriteFile(filepath.Join(confDir, "config.json"), []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	want := Tile{X: 100, Y: DefaultMonitorTile.Y, W: DefaultMonitorTile.W, H: DefaultMonitorTile.H}
	if cfg.MonitorTile != want {
		t.Errorf("MonitorTile = %+v, want %+v", cfg.MonitorTile, want)
	}
}

func TestLoad_InvalidJSONReturnsError(t *testing.T) {
	dir := withAppData(t)
	confDir := filepath.Join(dir, "WSLComms")
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil {
		t.Error("Load() with malformed JSON: error = nil, want non-nil")
	}
}

func TestSave_CreatesDirectoryAndIsReadable(t *testing.T) {
	withAppData(t)

	cfg := Defaults()
	cfg.M2LXHost = "m2lx.example.com"
	cfg.Alias = "commentator1"
	cfg.EventID = "event-42"

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() after Save() error = %v", err)
	}
	if *got != *cfg {
		t.Errorf("Load() after Save() = %+v, want %+v", *got, *cfg)
	}
}

func TestSave_IsIndentedAndHandEditable(t *testing.T) {
	withAppData(t)

	cfg := Defaults()
	cfg.M2LXHost = "m2lx.example.com"
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\n  \"m2lxHost\"") {
		t.Errorf("config.json is not indented as expected, got:\n%s", data)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("saved config.json is not valid JSON: %v", err)
	}
}

func TestSave_NoTempFileLeftBehind(t *testing.T) {
	dir := withAppData(t)

	cfg := Defaults()
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "WSLComms"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory contents = %v, want exactly [config.json]", names)
	}
}

func TestSave_OverwritesExistingFileAtomically(t *testing.T) {
	withAppData(t)

	first := Defaults()
	first.M2LXHost = "old.example.com"
	if err := first.Save(); err != nil {
		t.Fatalf("first Save() error = %v", err)
	}

	second := Defaults()
	second.M2LXHost = "new.example.com"
	if err := second.Save(); err != nil {
		t.Fatalf("second Save() error = %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.M2LXHost != "new.example.com" {
		t.Errorf("M2LXHost after second Save() = %q, want %q", got.M2LXHost, "new.example.com")
	}
}

func TestSave_NeverWritesSecretFields(t *testing.T) {
	// Config has no password/passphrase field at all — this test guards
	// against a future field being added to the struct without also being
	// routed to internal/secrets instead of config.json.
	withAppData(t)

	cfg := Defaults()
	cfg.M2LXHost = "m2lx.example.com"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(data))
	for _, forbidden := range []string{"password", "passphrase"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("config.json contains forbidden substring %q:\n%s", forbidden, data)
		}
	}
}

func validConfig() *Config {
	cfg := Defaults()
	cfg.M2LXHost = "m2lx.example.com"
	cfg.Alias = "commentator1"
	cfg.EventID = "event-42"
	cfg.SRTPort = 8890
	cfg.StatusKey = "cam7"
	cfg.AudioDeviceID = "{00000000-0000-0000-0000-000000000001}"
	return cfg
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
		wantSub string // substring that must appear in the joined error
	}{
		{
			name:    "fully valid config",
			modify:  func(c *Config) {},
			wantErr: false,
		},
		{
			name:    "empty m2lxHost",
			modify:  func(c *Config) { c.M2LXHost = "" },
			wantErr: true,
			wantSub: "m2lxHost",
		},
		{
			name:    "whitespace-only alias",
			modify:  func(c *Config) { c.Alias = "   " },
			wantErr: true,
			wantSub: "alias",
		},
		{
			name:    "empty eventId",
			modify:  func(c *Config) { c.EventID = "" },
			wantErr: true,
			wantSub: "eventId",
		},
		{
			// There is no srtHost field any more: the SRT host is always the
			// M2L-X host (EffectiveSRTHost), so an empty m2lxHost is the only way
			// to have no SRT host, and that is already reported as m2lxHost.
			name:    "empty m2lxHost leaves no SRT host to dial",
			modify:  func(c *Config) { c.M2LXHost = "" },
			wantErr: true,
			wantSub: "m2lxHost",
		},
		{
			name:    "srtPort zero",
			modify:  func(c *Config) { c.SRTPort = 0 },
			wantErr: true,
			wantSub: "srtPort",
		},
		{
			name:    "srtPort negative",
			modify:  func(c *Config) { c.SRTPort = -1 },
			wantErr: true,
			wantSub: "srtPort",
		},
		{
			name:    "srtPort too large",
			modify:  func(c *Config) { c.SRTPort = 65536 },
			wantErr: true,
			wantSub: "srtPort",
		},
		{
			name:    "srtPort at lower bound is valid",
			modify:  func(c *Config) { c.SRTPort = 1 },
			wantErr: false,
		},
		{
			name:    "srtPort at upper bound is valid",
			modify:  func(c *Config) { c.SRTPort = 65535 },
			wantErr: false,
		},
		{
			// statusKey is not required to send. It names the node the three
			// WebSocket-derived lamps read, and nothing in the M2L-X API will
			// name it — the only way to find it is to watch switcher_status
			// while the feed comes up, which cannot happen if Start refuses to
			// run without it.
			name:    "empty statusKey",
			modify:  func(c *Config) { c.StatusKey = "" },
			wantErr: false,
		},
		{
			name:    "empty audioDeviceId",
			modify:  func(c *Config) { c.AudioDeviceID = "" },
			wantErr: true,
			wantSub: "audioDeviceId",
		},
		{
			name:    "pbkeylen zero is valid",
			modify:  func(c *Config) { c.PBKeyLen = 0 },
			wantErr: false,
		},
		{
			name:    "pbkeylen 16 is valid",
			modify:  func(c *Config) { c.PBKeyLen = 16 },
			wantErr: false,
		},
		{
			name:    "pbkeylen 32 is valid",
			modify:  func(c *Config) { c.PBKeyLen = 32 },
			wantErr: false,
		},
		{
			name:    "pbkeylen 24 is invalid",
			modify:  func(c *Config) { c.PBKeyLen = 24 },
			wantErr: true,
			wantSub: "pbkeylen",
		},
		{
			name:    "returnMid zero is invalid",
			modify:  func(c *Config) { c.ReturnMid = 0 },
			wantErr: true,
			wantSub: "returnMid",
		},
		{
			name:    "returnMid 8 is invalid",
			modify:  func(c *Config) { c.ReturnMid = 8 },
			wantErr: true,
			wantSub: "returnMid",
		},
		{
			name:    "returnMid 1 is valid",
			modify:  func(c *Config) { c.ReturnMid = 1 },
			wantErr: false,
		},
		{
			name:    "returnMid 7 is valid",
			modify:  func(c *Config) { c.ReturnMid = 7 },
			wantErr: false,
		},
		{
			name: "multiple problems joined into one error",
			modify: func(c *Config) {
				c.M2LXHost = ""
				c.Alias = ""
				c.SRTPort = 0
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.modify(cfg)

			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantSub != "" && (err == nil || !strings.Contains(err.Error(), tt.wantSub)) {
				t.Errorf("Validate() error = %v, want substring %q", err, tt.wantSub)
			}
		})
	}
}

func TestValidate_JoinsAllProblems(t *testing.T) {
	cfg := &Config{} // everything zero
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() on zero Config: error = nil, want non-nil")
	}
	// statusKey is absent from this list on purpose: it is not required to
	// send. There is no srtHost either — the SRT host is always the M2L-X host,
	// so a zero Config's missing SRT host IS the missing m2lxHost.
	for _, want := range []string{"m2lxHost", "alias", "eventId", "audioDeviceId", "srtPort", "returnMid"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, missing expected substring %q", err, want)
		}
	}
}

func TestDefaults_PassesValidateExceptRequiredFields(t *testing.T) {
	// Defaults() deliberately leaves required fields empty (per its doc
	// comment) but every field it does set — srtLatencyMs, returnMid,
	// monitorTile, returnGainDb, slatePath — must be within the ranges
	// Validate checks.
	d := Defaults()
	err := d.Validate()
	if err == nil {
		t.Fatal("Validate() on Defaults(): error = nil, want non-nil (required fields are empty)")
	}
	if strings.Contains(err.Error(), "returnMid") {
		t.Errorf("Defaults().ReturnMid = %d should satisfy Validate's range, got error %v", d.ReturnMid, err)
	}
	if strings.Contains(err.Error(), "pbkeylen") {
		t.Errorf("Defaults().PBKeyLen = %d should satisfy Validate's range, got error %v", d.PBKeyLen, err)
	}
}

// ---------------------------------------------------------------------------
// EffectiveSRTHost
// ---------------------------------------------------------------------------

func TestEffectiveSRTHost(t *testing.T) {
	// The operator's complaint that produced this: "I shouldn't need to specify
	// the SRT host again, it will be the same as the m2lx host." There is no
	// srtHost field any more — the SRT host is ALWAYS the M2L-X host with any
	// scheme, path and port stripped off. The cases below are every way m2lxHost
	// can be written on the Settings screen.
	tests := []struct {
		name     string
		m2lxHost string
		want     string
	}{
		{
			name:     "a bare m2lxHost is used as-is",
			m2lxHost: "m2lx-wslstudios-matcht.etapsiota.com",
			want:     "m2lx-wslstudios-matcht.etapsiota.com",
		},
		{
			name:     "the fallback strips an https scheme",
			m2lxHost: "https://m2lx.example.com",
			want:     "m2lx.example.com",
		},
		{
			name:     "the fallback strips an http scheme and a port",
			m2lxHost: "http://127.0.0.1:8080",
			want:     "127.0.0.1",
		},
		{
			name:     "the fallback strips a bare host's port",
			m2lxHost: "m2lx.example.com:8443",
			want:     "m2lx.example.com",
		},
		{
			name:     "the fallback strips a trailing path",
			m2lxHost: "https://m2lx.example.com/live-operation/",
			want:     "m2lx.example.com",
		},
		{
			name:     "an IPv6 literal keeps its brackets and loses its port",
			m2lxHost: "https://[2001:db8::1]:8443",
			want:     "[2001:db8::1]",
		},
		{
			name:     "a bare IPv6 literal is left alone",
			m2lxHost: "[2001:db8::1]",
			want:     "[2001:db8::1]",
		},
		{
			name: "nothing configured resolves to nothing, not to a guess",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{M2LXHost: tt.m2lxHost}
			if got := c.EffectiveSRTHost(); got != tt.want {
				t.Errorf("EffectiveSRTHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEffectiveSRTHostDoesNotMutateTheConfig(t *testing.T) {
	// EffectiveSRTHost derives the SRT host from m2lxHost on every read. It must
	// be a pure read: if it wrote the stripped value back into M2LXHost, the
	// scheme and port a later sign-in needs would be gone.
	c := &Config{M2LXHost: "https://m2lx.example.com:8443"}
	if got := c.EffectiveSRTHost(); got != "m2lx.example.com" {
		t.Fatalf("EffectiveSRTHost() = %q", got)
	}
	if c.M2LXHost != "https://m2lx.example.com:8443" {
		t.Errorf("EffectiveSRTHost() changed M2LXHost to %q", c.M2LXHost)
	}
}
