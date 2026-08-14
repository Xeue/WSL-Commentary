package remote

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"wslcomms/internal/config"
)

// withAppData points the user config directory at a fresh temp directory for
// the duration of the test, so RemoteDir/Save/LoadSettings exercise a real
// filesystem without touching the developer's actual profile. It returns the
// directory RemoteDir joins appDataDirName onto.
//
// RemoteDir resolves os.UserConfigDir, and which environment variable that
// reads is per-GOOS — see the long note on internal/config's withAppData for
// why setting APPDATA alone made this package fail on darwin.
func withAppData(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("APPDATA", home)
	case "darwin":
		t.Setenv("HOME", home)
	default:
		t.Setenv("XDG_CONFIG_HOME", home)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("resolving the redirected user config directory: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating the redirected user config directory: %v", err)
	}
	return dir
}

// TestAppDataDirName_MatchesConfig guards the one fact this package duplicates
// rather than imports: the app-data folder name. internal/remote must not depend
// on internal/config to stay independently testable, so it restates the folder
// name — and this test fails loudly if the two ever drift, which would put
// remote.json under a different %APPDATA% subtree than config.json.
func TestAppDataDirName_MatchesConfig(t *testing.T) {
	if appDataDirName != config.AppDataDirName {
		t.Fatalf("appDataDirName %q != config.AppDataDirName %q", appDataDirName, config.AppDataDirName)
	}
}

func TestSettings_ValidateRules(t *testing.T) {
	cases := []struct {
		name    string
		s       Settings
		wantErr string
	}{
		{
			name:    "hostname bind refused",
			s:       Settings{Bind: "example.com", HTTPPort: 80, HTTPSPort: 443},
			wantErr: "not a literal IP",
		},
		{
			name: "wildcard bind is the open default and is fine",
			s:    Settings{Bind: "0.0.0.0", HTTPPort: 80, HTTPSPort: 443},
		},
		{
			name: "a specific LAN bind is fine",
			s:    Settings{Bind: "192.0.2.1", HTTPPort: 80, HTTPSPort: 443},
		},
		{
			name: "loopback is fine",
			s:    Settings{Bind: "127.0.0.1", HTTPPort: 8080, HTTPSPort: 8443},
		},
		{
			name: "port 0 is allowed (OS-assigned, for tests)",
			s:    Settings{Bind: "0.0.0.0", HTTPPort: 0, HTTPSPort: 0},
		},
		{
			name:    "negative http port refused",
			s:       Settings{Bind: "0.0.0.0", HTTPPort: -1, HTTPSPort: 443},
			wantErr: "httpPort",
		},
		{
			name:    "out-of-range https port refused",
			s:       Settings{Bind: "0.0.0.0", HTTPPort: 80, HTTPSPort: 70000},
			wantErr: "httpsPort",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.s.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, c.wantErr)
			}
		})
	}
}

func TestSettings_SaveLoadRoundTripAtomic(t *testing.T) {
	// Redirect the user config dir to a temp tree so the real config folder is untouched.
	dir := withAppData(t)

	s := DefaultSettings()
	s.Bind = "127.0.0.1"
	s.HTTPPort = 8080
	s.HTTPSPort = 8443
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The file exists under %APPDATA%\WSLComms\remote\remote.json and no temp file
	// is left behind.
	remoteDir := filepath.Join(dir, appDataDirName, remoteDirName)
	entries, _ := os.ReadDir(remoteDir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("Save left a temp file behind: %s", e.Name())
		}
	}

	got, err := LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if !got.Enabled || got.Bind != "127.0.0.1" || got.HTTPPort != 8080 || got.HTTPSPort != 8443 {
		t.Fatalf("round-trip lost data: %+v", got)
	}
}

func TestLoadSettings_MissingFileReturnsOpenDefaults(t *testing.T) {
	withAppData(t)
	got, err := LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings on a missing file: %v", err)
	}
	// The owner's decision: a machine with no remote.json is ON, all interfaces.
	if !got.Enabled {
		t.Error("missing remote.json loaded as disabled; want enabled (ON by default)")
	}
	if got.Bind != "0.0.0.0" {
		t.Errorf("missing remote.json bind = %q, want 0.0.0.0", got.Bind)
	}
	if got.HTTPPort != 80 || got.HTTPSPort != 443 {
		t.Errorf("missing remote.json ports = %d/%d, want 80/443", got.HTTPPort, got.HTTPSPort)
	}
}

// TestLoadSettings_MigratesLegacyFile proves an old authenticated-era remote.json
// — with a single TLS "port" and a "clients" array — loads without error, drops
// the clients, and folds the old port onto httpsPort. A machine upgraded from the
// previous scheme must keep working, not fail to parse.
func TestLoadSettings_MigratesLegacyFile(t *testing.T) {
	dir := withAppData(t)

	remoteDir := filepath.Join(dir, appDataDirName, remoteDirName)
	if err := os.MkdirAll(remoteDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacy := `{
  "version": 1,
  "enabled": true,
  "bind": "192.0.2.1",
  "port": 9443,
  "clients": [ { "name": "op", "caps": ["view"], "pbkdf2": {"salt":"x","iter":1000,"hash":"y"} } ]
}`
	if err := os.WriteFile(filepath.Join(remoteDir, settingsFileName), []byte(legacy), 0o600); err != nil {
		t.Fatalf("writing legacy file: %v", err)
	}

	got, err := LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings on a legacy file: %v", err)
	}
	if !got.Enabled || got.Bind != "192.0.2.1" {
		t.Fatalf("legacy fields not preserved: %+v", got)
	}
	if got.HTTPSPort != 9443 {
		t.Errorf("legacy port 9443 was not migrated onto httpsPort (got %d)", got.HTTPSPort)
	}
	if got.HTTPPort != defaultHTTPPort {
		t.Errorf("httpPort = %d, want the default %d (the legacy file had no httpPort)", got.HTTPPort, defaultHTTPPort)
	}
	// The migrated settings must still be bindable.
	if err := got.Validate(); err != nil {
		t.Errorf("migrated settings do not validate: %v", err)
	}
}
