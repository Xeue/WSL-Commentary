//go:build dev || production || bindings

// Tests for the M2L-X instance presets' bound surface: app_presets.go against
// the gst stub, the fakeStore, and %APPDATA% pointed at a temp directory —
// all Gate A, exactly like app_test.go, whose newTestApp harness supplies the
// App (with the fake store wrapped by scopedStore, which is itself part of
// what is under test here).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"wslcomms/internal/config"
	"wslcomms/internal/presets"
	"wslcomms/internal/secrets"
)

// quietConfig is validConfig with the M2L-X host blanked, so that ApplyPreset's
// unconditional startControlPlane returns before it builds a client or a
// watcher: these tests are about presets, and a status WebSocket retrying
// against 127.0.0.1 for the life of each test is noise the suite does not
// need. The tests that DO exercise the control plane restart set a host.
func quietConfig() *config.Config {
	c := validConfig()
	c.M2LXHost = ""
	return c
}

// setConfig lives in app_return_test.go and is shared by this file.

// savePresetFile writes a preset file directly, the way a colleague's build or
// a hand-edit would produce one — bypassing SavePreset's extraction so the
// Fields map can carry exactly what the test wants, including keys a preset
// must never honour.
func savePresetFile(t *testing.T, id, name, scope string, fields map[string]json.RawMessage) {
	t.Helper()
	if err := presets.Save(presets.Preset{
		Version:         presets.Version,
		ID:              id,
		Name:            name,
		CredentialScope: scope,
		SavedAt:         time.Now().UTC(),
		Fields:          fields,
	}); err != nil {
		t.Fatalf("writing preset %q: %v", id, err)
	}
}

func readConfigFromDisk(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	return cfg
}

func TestApplyPresetRefusedWhileSending(t *testing.T) {
	a, _ := newTestApp(t)
	setConfig(a, validConfig()) // a startable config; the fake sender needs nothing more
	withFakeSender(a)

	// The on-disk config the refusal must leave untouched.
	if err := a.snapshotConfig().Save(); err != nil {
		t.Fatalf("seeding config.json: %v", err)
	}
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	savePresetFile(t, "twickenham", "Twickenham", "twickenham", map[string]json.RawMessage{
		"eventId": json.RawMessage(`"dl9-twickenham"`),
	})

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer a.Stop()

	_, applyErr := a.ApplyPreset("twickenham")
	if !errors.Is(applyErr, errPresetWhileSending) {
		t.Fatalf("ApplyPreset() while sending error = %v, want errPresetWhileSending — a mid-match "+
			"apply leaves the feed going to the previous instance with every lamp green", applyErr)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the refused apply changed config.json on disk; a refusal must change nothing")
	}
	if got := a.snapshotConfig().EventID; got != validConfig().EventID {
		t.Errorf("the refused apply changed the live config: eventId = %q", got)
	}
}

// TestApplyPresetLeavesDeviceIDsAlone is the regression test for the fault the
// whole design defends against — the live "Failed to open device
// {0.0.0.00000000}.{8678ce58-…}" — reproduced as "a preset from another
// machine tries to set a render-namespace endpoint" and proven impossible.
func TestApplyPresetLeavesDeviceIDsAlone(t *testing.T) {
	const captureGUID = "{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}"
	const renderGUID = "{0.0.0.00000000}.{8678ce58-90c0-4827-8ff7-c9edd8d074ed}"

	a, _ := newTestApp(t)
	cfg := quietConfig()
	cfg.AudioDeviceID = captureGUID
	setConfig(a, cfg)
	if err := a.snapshotConfig().Save(); err != nil {
		t.Fatal(err)
	}

	// A preset hand-doctored to carry a RENDER-namespace endpoint — the id
	// that, applied, would preroll and then fail asynchronously inside
	// GStreamer while the sender blamed the network.
	savePresetFile(t, "mailed", "Mailed From Elsewhere", "", map[string]json.RawMessage{
		"eventId":       json.RawMessage(`"dl9-elsewhere"`),
		"audioDeviceId": json.RawMessage(`"` + renderGUID + `"`),
	})

	merged, err := a.ApplyPreset("mailed")
	if err != nil {
		t.Fatalf("ApplyPreset() error = %v; a bad key is neutered, never fatal", err)
	}

	if merged.AudioDeviceID != captureGUID {
		t.Errorf("the returned config's audioDeviceId = %q, want the machine's own %q", merged.AudioDeviceID, captureGUID)
	}
	if got := a.snapshotConfig().AudioDeviceID; got != captureGUID {
		t.Errorf("the live config's audioDeviceId = %q, want %q byte-identical", got, captureGUID)
	}
	if got := readConfigFromDisk(t).AudioDeviceID; got != captureGUID {
		t.Errorf("config.json's audioDeviceId = %q, want %q byte-identical", got, captureGUID)
	}
	if merged.EventID != "dl9-elsewhere" {
		t.Error("the whitelisted key travelling with the bad one must still apply")
	}

	// And the render guid's KEY is reported as ignored, on the banner event:
	// the operator is told, not left to wonder why the preset "half worked".
	reported := false
	for _, e := range drainPump(a) {
		if e.name != EventError {
			continue
		}
		if s, ok := e.data.(string); ok && strings.Contains(s, "audioDeviceId") {
			reported = true
		}
	}
	if !reported {
		t.Error("no error event named the ignored audioDeviceId key")
	}
}

func TestApplyPresetReturnsTheMergedConfig(t *testing.T) {
	a, _ := newTestApp(t)
	setConfig(a, quietConfig())

	savePresetFile(t, "twickenham", "Twickenham", "twickenham", map[string]json.RawMessage{
		"eventId":   json.RawMessage(`"dl9-twickenham"`),
		"srtPort":   json.RawMessage(`40007`),
		"statusKey": json.RawMessage(`""`),
	})

	merged, err := a.ApplyPreset("twickenham")
	if err != nil {
		t.Fatalf("ApplyPreset() error = %v", err)
	}

	// The return type is the correctness contract: the page assigns this over
	// its whole cached config, so it must BE the authority — equal to what
	// GetConfig hands out afterwards.
	got, err := a.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	if !reflect.DeepEqual(merged, got) {
		t.Errorf("ApplyPreset returned %+v but GetConfig() now says %+v; the page would cache a lie", merged, got)
	}
	if merged.EventID != "dl9-twickenham" || merged.SRTPort != 40007 {
		t.Errorf("the preset's fields did not apply: %+v", merged)
	}
}

func TestApplyPresetSetsTheCredentialScope(t *testing.T) {
	a, store := newTestApp(t)
	setConfig(a, quietConfig())

	savePresetFile(t, "wembley", "Wembley", "wembley", map[string]json.RawMessage{
		"eventId": json.RawMessage(`"dl9-wembley"`),
	})

	if _, err := a.ApplyPreset("wembley"); err != nil {
		t.Fatalf("ApplyPreset() error = %v", err)
	}
	if got := a.credentialScope(); got != "wembley" {
		t.Fatalf("credentialScope() = %q, want %q", got, "wembley")
	}

	// The decorator is what is being proven here: a bare-constant read on any
	// credential path must reach the fake under the SCOPED key.
	store.mu.Lock()
	store.values["wembley/m2lx"] = "the-wembley-password"
	store.mu.Unlock()
	got, err := a.store.Get(secrets.KeyM2LX)
	if err != nil {
		t.Fatalf("store.Get(KeyM2LX) error = %v", err)
	}
	if got != "the-wembley-password" {
		t.Errorf("store.Get(KeyM2LX) = %q, want the value stored under wembley/m2lx", got)
	}

	// And writes scope the same way: this is the key SetSecret would store the
	// operator's newly typed passphrase under.
	if err := a.SetSecret(secrets.KeySRT, "wembley-send-key"); err != nil {
		t.Fatalf("SetSecret() error = %v", err)
	}
	store.mu.Lock()
	scoped := store.values["wembley/srt"]
	_, legacyTouched := store.values["srt"]
	store.mu.Unlock()
	if scoped != "wembley-send-key" {
		t.Errorf("SetSecret wrote %q under wembley/srt, want the typed value", scoped)
	}
	if legacyTouched {
		t.Error("SetSecret also touched the LEGACY srt entry; a preset's passphrase leaked across scopes")
	}

	// The active record survives a restart: it is the durable half of the scope.
	rec, found, err := presets.LoadActive()
	if err != nil || !found {
		t.Fatalf("LoadActive() = (%+v, %v, %v)", rec, found, err)
	}
	if rec.ID != "wembley" || rec.CredentialScope != "wembley" {
		t.Errorf("active record = %+v, want wembley/wembley", rec)
	}
}

func TestSetSecretUsesLegacyTargetsWithNoActivePreset(t *testing.T) {
	a, store := newTestApp(t)
	setConfig(a, quietConfig())

	if err := a.SetSecret(secrets.KeySRT, "legacy-send-key"); err != nil {
		t.Fatalf("SetSecret() error = %v", err)
	}
	store.mu.Lock()
	got := store.values["srt"]
	store.mu.Unlock()
	if got != "legacy-send-key" {
		t.Errorf("with no active preset SetSecret must write the bare key %q; fake store holds %q", "srt", got)
	}
}

func TestFirstPresetAdoptsTheLegacyScopeAndTheSecondDoesNot(t *testing.T) {
	a, _ := newTestApp(t)
	setConfig(a, quietConfig())

	// The migration case: nothing preset-shaped exists, so "Save current as…"
	// must inherit the machine's existing vault entries — nothing retyped.
	first, err := a.SavePreset("Wembley")
	if err != nil {
		t.Fatalf("SavePreset(first) error = %v", err)
	}
	if first.CredentialScope != "" {
		t.Fatalf("the FIRST preset's scope = %q, want \"\" — the legacy entries the machine already has",
			first.CredentialScope)
	}
	if got := a.credentialScope(); got != "" {
		t.Errorf("credentialScope() after the migration save = %q, want \"\"", got)
	}

	second, err := a.SavePreset("Twickenham")
	if err != nil {
		t.Fatalf("SavePreset(second) error = %v", err)
	}
	if second.CredentialScope != "twickenham" {
		t.Errorf("the SECOND preset's scope = %q, want its own id %q", second.CredentialScope, "twickenham")
	}
	if got := a.credentialScope(); got != "twickenham" {
		t.Errorf("credentialScope() after the second save = %q, want %q", got, "twickenham")
	}

	// The migration preset keeps "" forever, including across a re-save.
	resaved, err := a.SavePreset("Wembley")
	if err != nil {
		t.Fatalf("re-SavePreset(Wembley) error = %v", err)
	}
	if resaved.CredentialScope != "" {
		t.Errorf("re-saving the migration preset changed its scope to %q; the legacy passwords would be stranded",
			resaved.CredentialScope)
	}
}

func TestSavePresetRejectsACollidingName(t *testing.T) {
	a, _ := newTestApp(t)
	setConfig(a, quietConfig())

	if _, err := a.SavePreset("Wembley"); err != nil {
		t.Fatalf("SavePreset() error = %v", err)
	}
	// A DIFFERENT name that derives the same id would silently overwrite.
	if _, err := a.SavePreset("wembley!"); err == nil {
		t.Fatal("SavePreset(\"wembley!\") overwrote the preset named \"Wembley\" without saying so")
	}
	// The SAME name in another case is the operator re-saving their preset.
	if _, err := a.SavePreset("WEMBLEY"); err != nil {
		t.Errorf("SavePreset(\"WEMBLEY\") error = %v; a case-changed re-save of the same name must update, not refuse", err)
	}
}

func TestDeletePresetRefusesTheActiveOne(t *testing.T) {
	a, _ := newTestApp(t)
	setConfig(a, quietConfig())

	if _, err := a.SavePreset("Wembley"); err != nil {
		t.Fatalf("SavePreset() error = %v", err)
	}
	if err := a.DeletePreset("wembley", false); !errors.Is(err, errDeleteActivePreset) {
		t.Fatalf("DeletePreset(active) error = %v, want errDeleteActivePreset", err)
	}
	if _, err := presets.Load("wembley"); err != nil {
		t.Error("the refused delete removed the file anyway")
	}
}

func TestDeletePresetRefusesLegacyCredentialDeletion(t *testing.T) {
	a, store := newTestApp(t)
	setConfig(a, quietConfig())

	// The migration preset (scope ""), then a second so the first is no longer
	// active and its deletion is otherwise permitted.
	if _, err := a.SavePreset("Wembley"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SavePreset("Twickenham"); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.values["m2lx"] = "the-only-password-on-this-machine"
	store.mu.Unlock()

	if err := a.DeletePreset("wembley", true); !errors.Is(err, errDeleteLegacyCredentials) {
		t.Fatalf("DeletePreset(legacy scope, alsoDeleteCredentials) error = %v, want errDeleteLegacyCredentials", err)
	}
	store.mu.Lock()
	kept := store.values["m2lx"]
	store.mu.Unlock()
	if kept != "the-only-password-on-this-machine" {
		t.Error("the refusal did not protect the legacy credential")
	}
	if _, err := presets.Load("wembley"); err != nil {
		t.Error("the refused delete removed the file; a refusal must be whole")
	}

	// Without the credentials flag, deleting the non-active migration preset
	// is an ordinary delete.
	if err := a.DeletePreset("wembley", false); err != nil {
		t.Errorf("DeletePreset(legacy scope, false) error = %v", err)
	}
}

func TestDeletePresetDeletesItsOwnScopedCredentials(t *testing.T) {
	a, store := newTestApp(t)
	setConfig(a, quietConfig())

	savePresetFile(t, "twickenham", "Twickenham", "twickenham", map[string]json.RawMessage{})
	store.mu.Lock()
	store.values["twickenham/m2lx"] = "pw"
	store.values["twickenham/srt"] = "send"
	store.values["twickenham/srtreturn"] = "return"
	store.values["m2lx"] = "legacy-untouched"
	store.mu.Unlock()

	if err := a.DeletePreset("twickenham", true); err != nil {
		t.Fatalf("DeletePreset() error = %v", err)
	}
	if _, err := presets.Load("twickenham"); !errors.Is(err, presets.ErrNotFound) {
		t.Errorf("Load after delete error = %v, want ErrNotFound", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, key := range []string{"twickenham/m2lx", "twickenham/srt", "twickenham/srtreturn"} {
		// The fakeStore records Set(key, "") as an empty value; the real store
		// implements it as a CredDeleteW. Either way the secret is gone.
		if store.values[key] != "" {
			t.Errorf("credential %q survived a delete-with-credentials", key)
		}
	}
	if store.values["m2lx"] != "legacy-untouched" {
		t.Error("deleting a preset's scoped credentials touched the legacy entry")
	}
}

func TestRenamePresetDoesNotChangeTheCredentialScope(t *testing.T) {
	a, _ := newTestApp(t)
	setConfig(a, quietConfig())

	if _, err := a.SavePreset("Wembley"); err != nil {
		t.Fatal(err)
	}
	second, err := a.SavePreset("Twickenham")
	if err != nil {
		t.Fatal(err)
	}
	if second.CredentialScope != "twickenham" {
		t.Fatalf("precondition: scope = %q", second.CredentialScope)
	}

	if err := a.RenamePreset("twickenham", "Twickenham Stoop"); err != nil {
		t.Fatalf("RenamePreset() error = %v", err)
	}
	got, err := presets.Load("twickenham")
	if err != nil {
		t.Fatalf("Load() after rename error = %v — the id must not move with the name", err)
	}
	if got.Name != "Twickenham Stoop" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.CredentialScope != "twickenham" {
		t.Errorf("CredentialScope = %q after a rename; the stored passwords for that instance are now "+
			"unreachable with nothing on screen saying so", got.CredentialScope)
	}
}

// TestStatusKeySurvivesARoundTrip pins the single biggest saving the feature
// offers: statusKey is per-instance AND per-patch, nothing in the API can
// supply it, and it is the field an operator least wants to re-derive at a
// venue. Save it in a preset, blank it, apply — it must come back.
func TestStatusKeySurvivesARoundTrip(t *testing.T) {
	a, _ := newTestApp(t)
	cfg := quietConfig()
	cfg.StatusKey = "cam7"
	setConfig(a, cfg)

	saved, err := a.SavePreset("Wembley")
	if err != nil {
		t.Fatalf("SavePreset() error = %v", err)
	}
	if _, ok := saved.Fields["statusKey"]; !ok {
		t.Fatal("the saved preset does not carry statusKey")
	}

	blanked := quietConfig()
	blanked.StatusKey = ""
	setConfig(a, blanked)

	merged, err := a.ApplyPreset("wembley")
	if err != nil {
		t.Fatalf("ApplyPreset() error = %v", err)
	}
	if merged.StatusKey != "cam7" {
		t.Errorf("statusKey after the round trip = %q, want %q", merged.StatusKey, "cam7")
	}
}

func TestStartupSetsTheCredentialScopeFromTheActiveRecord(t *testing.T) {
	a, _ := newTestApp(t)
	if err := presets.SaveActive(presets.ActiveRecord{
		ID: "wembley", CredentialScope: "wembley", AppliedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// startup loads config.json (absent -> Defaults, M2LXHost "" so the
	// control plane is a no-op) and must read the record BEFORE
	// startControlPlane — asserted here only as "the scope is right after
	// startup", which is the observable half.
	a.startup(context.Background())
	if got := a.credentialScope(); got != "wembley" {
		t.Errorf("credentialScope() after startup = %q, want %q from active.json", got, "wembley")
	}
}

func TestStartupReportsACorruptActiveRecordAndFallsBackToLegacy(t *testing.T) {
	a, _ := newTestApp(t)
	dir, err := presets.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "active.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	a.startup(context.Background())

	if got := a.credentialScope(); got != "" {
		t.Errorf("credentialScope() after a corrupt record = %q, want \"\" — the recoverable fallback, never a guess", got)
	}
	reported := false
	for _, e := range drainPump(a) {
		if e.name != EventError {
			continue
		}
		if s, ok := e.data.(string); ok && strings.Contains(s, "active-preset record") {
			reported = true
		}
	}
	if !reported {
		t.Error("a corrupt active.json was absorbed silently; 'wrong vault entry' and 'no password " +
			"stored' look identical on the lamps, so the corruption must be said out loud")
	}
}

func TestGetActivePresetAndCredentialStatus(t *testing.T) {
	a, store := newTestApp(t)
	setConfig(a, quietConfig())

	// Before anything: zero record, legacy scope, whatever the vault holds.
	rec, err := a.GetActivePreset()
	if err != nil || rec.ID != "" {
		t.Fatalf("GetActivePreset() = (%+v, %v), want the zero record", rec, err)
	}
	store.mu.Lock()
	store.values["m2lx"] = "pw"
	store.mu.Unlock()
	status, err := a.GetPresetCredentialStatus()
	if err != nil {
		t.Fatalf("GetPresetCredentialStatus() error = %v", err)
	}
	if status.Scope != "" || !status.M2LX || status.SRT || status.SRTReturn {
		t.Errorf("status = %+v, want legacy scope with only m2lx present", status)
	}

	// After applying a scoped preset: existence is answered for THAT scope,
	// so the just-checked legacy password no longer reads as present.
	savePresetFile(t, "wembley", "Wembley", "wembley", map[string]json.RawMessage{})
	if _, err := a.ApplyPreset("wembley"); err != nil {
		t.Fatal(err)
	}
	status, err = a.GetPresetCredentialStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Scope != "wembley" || status.M2LX {
		t.Errorf("status after apply = %+v, want scope wembley with nothing present", status)
	}

	store.mu.Lock()
	store.values["wembley/srtreturn"] = "return-key"
	store.mu.Unlock()
	status, err = a.GetPresetCredentialStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !status.SRTReturn || status.M2LX || status.SRT {
		t.Errorf("status = %+v, want only srtreturn present under wembley", status)
	}
}
