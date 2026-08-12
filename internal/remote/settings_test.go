package remote

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wslcomms/internal/config"
)

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

func TestPBKDF2_RoundTripAndRejectsWrong(t *testing.T) {
	p, err := hashPassword("correct horse")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if p.Salt == "" || p.Hash == "" || p.Iter <= 0 {
		t.Fatalf("hash produced an incomplete verifier: %+v", p)
	}
	if !p.verify("correct horse") {
		t.Error("verify rejected the correct password")
	}
	if p.verify("correct hors") {
		t.Error("verify accepted a wrong password")
	}
	// Two hashes of the same password differ (random salt), so a stolen file
	// cannot be reversed by matching identical hashes across clients.
	p2, _ := hashPassword("correct horse")
	if p.Hash == p2.Hash {
		t.Error("two hashes of the same password are identical; salt is not random")
	}
}

func TestPBKDF2_MalformedRecordVerifiesFalse(t *testing.T) {
	if (PBKDF2Params{}).verify("anything") {
		t.Error("an empty verifier accepted a password")
	}
	if (PBKDF2Params{Salt: "!!notbase64!!", Iter: 1000, Hash: "x"}).verify("anything") {
		t.Error("a malformed verifier accepted a password")
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
			s:       Settings{Bind: "example.com", Port: 8443},
			wantErr: "not a literal IP",
		},
		{
			name:    "non-loopback with no clients refused",
			s:       Settings{Bind: "192.0.2.1", Port: 8443},
			wantErr: "no clients",
		},
		{
			name: "non-loopback with a client is fine",
			s:    Settings{Bind: "192.0.2.1", Port: 8443, Clients: []Client{{Name: "op", Caps: []string{"view"}}}},
		},
		{
			name: "loopback with no clients is fine",
			s:    Settings{Bind: "127.0.0.1", Port: 8443},
		},
		{
			name:    "bad port refused",
			s:       Settings{Bind: "127.0.0.1", Port: 0},
			wantErr: "port",
		},
		{
			name:    "unknown capability refused",
			s:       Settings{Bind: "127.0.0.1", Port: 8443, Clients: []Client{{Name: "op", Caps: []string{"root"}}}},
			wantErr: "unknown capability",
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
	// Redirect %APPDATA% to a temp tree so the real config folder is untouched.
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)

	s := DefaultSettings()
	s.Enabled = true
	s.Bind = "127.0.0.1"
	if err := s.AddClient("producer", []string{"view", "operate"}); err != nil {
		t.Fatalf("AddClient: %v", err)
	}
	if err := s.SetClientPassword("producer", "hunter2"); err != nil {
		t.Fatalf("SetClientPassword: %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The file exists under %APPDATA%\WSLComms\remote\remote.json and no temp
	// file is left behind.
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
	if !got.Enabled || got.Bind != "127.0.0.1" || len(got.Clients) != 1 {
		t.Fatalf("round-trip lost data: %+v", got)
	}
	if !got.Clients[0].PBKDF2.verify("hunter2") {
		t.Error("stored password does not verify after round-trip")
	}
}

func TestLoadSettings_MissingFileReturnsSafeDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	got, err := LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings on a missing file: %v", err)
	}
	if got.Enabled {
		t.Error("missing remote.json loaded as enabled; want disabled")
	}
	if got.Bind != "127.0.0.1" {
		t.Errorf("missing remote.json bind = %q, want loopback", got.Bind)
	}
}

// TestSettings_NeverSerializesPlaintext is the mirror of internal/config's
// TestSave_NeverWritesSecretFields: whatever remote.json contains, it must never
// contain a plaintext password. Only the PBKDF2 verifier is on disk.
func TestSettings_NeverSerializesPlaintext(t *testing.T) {
	s := DefaultSettings()
	_ = s.AddClient("op", []string{"view"})
	_ = s.SetClientPassword("op", "s0oper-secret-passphrase")
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "s0oper-secret-passphrase") {
		t.Fatal("remote.json serialization contains the plaintext password")
	}
}
