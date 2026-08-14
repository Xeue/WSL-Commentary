package presets

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// tempAppData points the user config directory at a temp directory, exactly as
// config's and app.go's tests do, so nothing here can touch the developer's
// real profile. It returns the directory Dir() joins DirName onto.
//
// Which environment variable os.UserConfigDir reads is per-GOOS — see the long
// note on internal/config's withAppData for why setting APPDATA alone made this
// package fail on darwin.
func tempAppData(t *testing.T) string {
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

func mustExtract(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	fields, err := Extract(fullConfig())
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	return fields
}

func TestSaveLoadRoundTrip(t *testing.T) {
	tempAppData(t)

	p := Preset{
		Version:         Version,
		ID:              "wembley",
		Name:            "Wembley",
		CredentialScope: "wembley",
		SavedAt:         time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC),
		Fields:          mustExtract(t),
	}
	if err := Save(p); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := Load("wembley")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.ID != p.ID || got.Name != p.Name || got.CredentialScope != p.CredentialScope {
		t.Errorf("Load() = %+v, want id/name/scope of %+v", got, p)
	}
	if !got.SavedAt.Equal(p.SavedAt) {
		t.Errorf("SavedAt = %v, want %v", got.SavedAt, p.SavedAt)
	}
	if len(got.Fields) != len(p.Fields) {
		t.Errorf("Fields carried %d keys, want %d", len(got.Fields), len(p.Fields))
	}
}

// TestSaveNeverWritesADeviceID asserts on the FILE BYTES, mirroring
// internal/config's TestSave_NeverWritesSecretFields, which is the house
// precedent for this shape of guarantee: not "the struct has no field" but
// "the bytes on disk contain neither the key names nor the values". The
// values are the operator's real hardware ids, and a preset is a file that
// gets mailed.
func TestSaveNeverWritesADeviceID(t *testing.T) {
	tempAppData(t)

	// Every device field populated with a recognisable value BEFORE Extract,
	// so a whitelist regression shows up as those values in the file.
	cfg := fullConfig()
	cfg.AudioDeviceID = "{0.0.1.00000000}.{recognisable-capture-guid}"
	cfg.HeadphoneDeviceID = "recognisable-browser-hash"
	cfg.HeadphoneEndpointID = "{0.0.0.00000000}.{recognisable-render-guid}"
	cfg.SlatePath = `D:\recognisable\slate.png`

	fields, err := Extract(cfg)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	p := Preset{Version: Version, ID: "wembley", Name: "Wembley", SavedAt: time.Now().UTC(), Fields: fields}
	if err := Save(p); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	path, err := PathFor("wembley")
	if err != nil {
		t.Fatalf("PathFor() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the preset file: %v", err)
	}
	lower := strings.ToLower(string(raw))

	for _, forbidden := range []string{
		"deviceid", "endpointid", // the key names, case-insensitively
		"recognisable-capture-guid", "recognisable-browser-hash",
		"recognisable-render-guid", "recognisable",
		"slatepath",
	} {
		if strings.Contains(lower, strings.ToLower(forbidden)) {
			t.Errorf("the preset file on disk contains %q; a preset must never carry a machine field:\n%s",
				forbidden, raw)
		}
	}
}

// TestSaveNeverWritesSecretFields: no key or value shaped like a secret in the
// file bytes. The passphrases live in Credential Manager under the preset's
// scope; only the non-secret key LENGTHS travel.
func TestSaveNeverWritesSecretFields(t *testing.T) {
	tempAppData(t)

	p := Preset{Version: Version, ID: "wembley", Name: "Wembley", SavedAt: time.Now().UTC(), Fields: mustExtract(t)}
	if err := Save(p); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	path, _ := PathFor("wembley")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the preset file: %v", err)
	}
	lower := strings.ToLower(string(raw))
	for _, forbidden := range []string{"password", "passphrase", "secret", "token"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("the preset file on disk contains %q; secrets live in Credential Manager, "+
				"never in a file that gets mailed:\n%s", forbidden, raw)
		}
	}
}

// TestDeriveIDRejects is the security table: the id is a filename under
// %APPDATA% AND a Credential Manager target segment, so each of these is a
// traversal, a target collision or an uncreatable file, not an inconvenience.
func TestDeriveIDRejects(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"single dot", "."},
		{"dot dot is a traversal", ".."},
		{"forward slash is a path separator and a scope separator", "a/b"},
		{"backslash is a path separator", `a\b`},
		{"colon is an NTFS alternate data stream", "a:b"},
		{"CON is a reserved device name", "CON"},
		{"reserved device name in another case", "con"},
		{"reserved device name with an extension", "nul.json"},
		{"COM ports are reserved", "COM7"},
		{"LPT ports are reserved", "lpt3"},
		{"no usable characters", "!!! ***"},
		{"longer than 48 characters", strings.Repeat("a", 65)},
		{"49 characters is one over the line", strings.Repeat("a", 49)},
		// "active" derives the id of the package's OWN record file:
		// PathFor("active") == ActivePath(), so a preset saved under it is
		// destroyed by the active-record write that follows every save, with
		// "Saved" on screen and the credential scope pointed at an empty
		// vault. Found by adversarial review; every step of the collision
		// looks like success.
		{"active collides with the active-preset record", "active"},
		{"active in any case", "ACTIVE"},
		{"active with decoration that derives to the same id", " Active "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := DeriveID(tt.input)
			if err == nil {
				t.Fatalf("DeriveID(%q) = %q, want an error", tt.input, id)
			}
		})
	}
}

func TestDeriveIDAccepts(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Wembley", "wembley"},
		{"Wembley Stadium — Event 2", "wembley-stadium-event-2"},
		{"  spaced  out  ", "spaced-out"},
		{"UPPER", "upper"},
		{"console", "console"}, // CONtains a reserved prefix but is not CON
		{"com10", "com10"},     // COM1-9 are reserved; COM10 is a real filename
		{strings.Repeat("a", 48), strings.Repeat("a", 48)},
	}
	for _, tt := range tests {
		got, err := DeriveID(tt.input)
		if err != nil {
			t.Errorf("DeriveID(%q) error = %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("DeriveID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestDeriveIDIsCaseInsensitive pins the property SavePreset's collision check
// relies on: two names differing only by case derive the SAME id, so "same id"
// IS the case-insensitive collision and no second comparison exists to drift.
func TestDeriveIDIsCaseInsensitive(t *testing.T) {
	a, err := DeriveID("Wembley")
	if err != nil {
		t.Fatalf("DeriveID error = %v", err)
	}
	b, err := DeriveID("WEMBLEY")
	if err != nil {
		t.Fatalf("DeriveID error = %v", err)
	}
	if a != b {
		t.Errorf("DeriveID(\"Wembley\") = %q, DeriveID(\"WEMBLEY\") = %q; case must not mint a second id", a, b)
	}
}

// TestPathForRefusesABadID: Load, Delete and Rename take ids from across the
// Wails boundary, so the filename discipline has to hold against a caller,
// not only against DeriveID's output.
func TestPathForRefusesABadID(t *testing.T) {
	tempAppData(t)
	for _, id := range []string{"", "..", "a/b", `a\b`, "a:b", "CON", "nul", "Wembley", "-leading", "trailing-", "a b"} {
		if _, err := PathFor(id); err == nil {
			t.Errorf("PathFor(%q) accepted an id DeriveID could never produce", id)
		}
	}
}

func TestListIgnoresUnparseableFiles(t *testing.T) {
	appdata := tempAppData(t)

	good := Preset{Version: Version, ID: "wembley", Name: "Wembley", SavedAt: time.Now().UTC(), Fields: mustExtract(t)}
	if err := Save(good); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	dir := filepath.Join(appdata, "WSLComms", "presets")

	// A corrupt file beside it: List must return the good one, not an error.
	if err := os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing the corrupt file: %v", err)
	}
	// And a file whose NAME could never be a preset id (so Load refuses it).
	if err := os.WriteFile(filepath.Join(dir, "UPPER.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("writing the misnamed file: %v", err)
	}
	// active.json lives in the same directory and is not a preset.
	if err := SaveActive(ActiveRecord{ID: "wembley", AppliedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("SaveActive() error = %v", err)
	}

	list, err := List()
	if err != nil {
		t.Fatalf("List() error = %v; one corrupt file must not blank the whole picker", err)
	}
	if len(list) != 1 || list[0].ID != "wembley" {
		t.Fatalf("List() = %+v, want exactly the one readable preset", list)
	}
}

func TestListOnAMissingDirectoryIsEmptyNotAnError(t *testing.T) {
	tempAppData(t)
	list, err := List()
	if err != nil {
		t.Fatalf("List() with no presets directory error = %v; first run is not an error", err)
	}
	if len(list) != 0 {
		t.Fatalf("List() = %+v, want empty", list)
	}
}

func TestUnknownVersionRefusedByName(t *testing.T) {
	appdata := tempAppData(t)
	dir := filepath.Join(appdata, "WSLComms", "presets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "future.json"),
		[]byte(`{"version": 2, "id": "future", "name": "Future", "fields": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load("future")
	if err == nil {
		t.Fatal("Load() accepted an unknown version; it must refuse by name rather than half-apply")
	}
	if !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("Load() error = %v, want ErrUnknownVersion", err)
	}
	if !strings.Contains(err.Error(), "version 2") {
		t.Errorf("the refusal must NAME the version found: %v", err)
	}
}

func TestRenameChangesTheNameAndNothingElse(t *testing.T) {
	tempAppData(t)
	p := Preset{Version: Version, ID: "wembley", Name: "Wembley", CredentialScope: "wembley",
		SavedAt: time.Now().UTC(), Fields: mustExtract(t)}
	if err := Save(p); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if err := Rename("wembley", "Wembley Stadium"); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	got, err := Load("wembley")
	if err != nil {
		t.Fatalf("Load() after rename error = %v; the ID must not have changed", err)
	}
	if got.Name != "Wembley Stadium" {
		t.Errorf("Name = %q, want %q", got.Name, "Wembley Stadium")
	}
	if got.ID != "wembley" {
		t.Errorf("ID = %q; Rename must never change it", got.ID)
	}
	if got.CredentialScope != "wembley" {
		t.Errorf("CredentialScope = %q; Rename changing it orphans the stored passwords silently", got.CredentialScope)
	}

	if err := Rename("wembley", "   "); err == nil {
		t.Error("Rename() accepted a blank name")
	}
}

func TestActiveRecordRoundTrip(t *testing.T) {
	tempAppData(t)

	// Missing: zero record, not found, no error — first run.
	rec, found, err := LoadActive()
	if err != nil || found {
		t.Fatalf("LoadActive() on a missing file = (%+v, %v, %v), want zero record, false, nil", rec, found, err)
	}
	if rec.CredentialScope != "" {
		t.Fatalf("the zero record's scope must be empty (the legacy vault entries), got %q", rec.CredentialScope)
	}

	want := ActiveRecord{ID: "wembley", CredentialScope: "wembley", AppliedAt: time.Now().UTC().Truncate(time.Second)}
	if err := SaveActive(want); err != nil {
		t.Fatalf("SaveActive() error = %v", err)
	}
	rec, found, err = LoadActive()
	if err != nil || !found {
		t.Fatalf("LoadActive() = (%+v, %v, %v), want the record, true, nil", rec, found, err)
	}
	if rec.ID != want.ID || rec.CredentialScope != want.CredentialScope || !rec.AppliedAt.Equal(want.AppliedAt) {
		t.Errorf("LoadActive() = %+v, want %+v", rec, want)
	}
}

func TestLoadActiveOnACorruptFileReturnsTheZeroRecordAndSaysSo(t *testing.T) {
	appdata := tempAppData(t)
	dir := filepath.Join(appdata, "WSLComms", "presets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "active.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec, found, err := LoadActive()
	if err == nil {
		t.Fatal("LoadActive() on a corrupt file returned no error; silence here makes 'wrong vault " +
			"entry' indistinguishable from 'no password stored'")
	}
	if found {
		t.Error("found = true for a record that could not be read")
	}
	if rec != (ActiveRecord{}) {
		t.Errorf("LoadActive() = %+v, want the zero record — the legacy scope, never a guess", rec)
	}
}

func TestSaveRefusesTheWrongVersion(t *testing.T) {
	tempAppData(t)
	p := Preset{Version: 3, ID: "wembley", Name: "Wembley", Fields: map[string]json.RawMessage{}}
	if err := Save(p); err == nil {
		t.Fatal("Save() accepted a version this build does not write")
	}
}

// TestSaveIsAtomicOverAnExistingFile: interrupting semantics cannot be driven
// from a unit test, but the observable half can — writing over an existing
// preset leaves no temp litter and the new content wholly in place.
func TestSaveIsAtomicOverAnExistingFile(t *testing.T) {
	appdata := tempAppData(t)

	p := Preset{Version: Version, ID: "wembley", Name: "Wembley", SavedAt: time.Now().UTC(), Fields: mustExtract(t)}
	if err := Save(p); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	p.Name = "Wembley 2"
	if err := Save(p); err != nil {
		t.Fatalf("second Save() error = %v", err)
	}

	got, err := Load("wembley")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Name != "Wembley 2" {
		t.Errorf("Name = %q, want the second write's content", got.Name)
	}

	entries, err := os.ReadDir(filepath.Join(appdata, "WSLComms", "presets"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file %s left behind after a successful Save", e.Name())
		}
	}
}
