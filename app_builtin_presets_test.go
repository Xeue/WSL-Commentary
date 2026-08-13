//go:build dev || production || bindings

package main

import (
	"encoding/json"
	"testing"

	"wslcomms/internal/presets"
	"wslcomms/internal/secrets"
)

// presetFieldString reads a string-valued preset field, failing the test if it
// is absent or not the wanted value.
func presetFieldString(t *testing.T, p presets.Preset, tag, want string) {
	t.Helper()
	raw, ok := p.Fields[tag]
	if !ok {
		t.Fatalf("preset %q has no field %q", p.ID, tag)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("preset %q field %q is not a string (%s): %v", p.ID, tag, raw, err)
	}
	if got != want {
		t.Fatalf("preset %q field %q = %q, want %q", p.ID, tag, got, want)
	}
}

// presetFieldInt reads an int-valued preset field.
func presetFieldInt(t *testing.T, p presets.Preset, tag string, want int) {
	t.Helper()
	raw, ok := p.Fields[tag]
	if !ok {
		t.Fatalf("preset %q has no field %q", p.ID, tag)
	}
	var got int
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("preset %q field %q is not an int (%s): %v", p.ID, tag, raw, err)
	}
	if got != want {
		t.Fatalf("preset %q field %q = %d, want %d", p.ID, tag, got, want)
	}
}

func TestSeedBuiltinPresets_CreatesTheSevenFacilityInstances(t *testing.T) {
	a, store := newTestApp(t)

	a.seedBuiltinPresets()

	list, err := presets.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != len(builtinPresetLetters) {
		t.Fatalf("seeded %d presets, want %d", len(list), len(builtinPresetLetters))
	}
	byID := make(map[string]presets.Preset, len(list))
	for _, p := range list {
		byID[p.ID] = p
	}

	// Spot-check the two ends of the set: the first letter (g) and the odd one
	// out (t). Both carry the facility fields, and both have their own password
	// seeded under their own scope.
	cases := []struct {
		id, name, host, alias, password string
	}{
		{"matchg", "Match G", "m2lx-wslstudios-matchg.etapsiota.com", "matchg", "WSLStud10sM4tchG"},
		{"matcht", "Match T", "m2lx-wslstudios-matcht.etapsiota.com", "matcht", "WSLStud10sM4tchT"},
	}
	for _, tc := range cases {
		p, ok := byID[tc.id]
		if !ok {
			t.Fatalf("preset %q was not seeded", tc.id)
		}
		if p.Name != tc.name {
			t.Errorf("%s name = %q, want %q", tc.id, p.Name, tc.name)
		}
		if p.CredentialScope != tc.id {
			t.Errorf("%s credential scope = %q, want its own id %q", tc.id, p.CredentialScope, tc.id)
		}
		presetFieldString(t, p, "m2lxHost", tc.host)
		presetFieldString(t, p, "alias", tc.alias)
		presetFieldString(t, p, "statusKey", "cam4")
		presetFieldInt(t, p, "srtPort", 40004)
		presetFieldInt(t, p, "srtReturnPort", 40504)

		// The password is under WSLComms/<id>/m2lx, exactly where sign-in reads
		// it once the preset is applied.
		key, err := secrets.ScopedKey(tc.id, secrets.KeyM2LX)
		if err != nil {
			t.Fatalf("ScopedKey(%q): %v", tc.id, err)
		}
		got, err := store.Get(key)
		if err != nil {
			t.Fatalf("password for %q not stored under %q: %v", tc.id, key, err)
		}
		if got != tc.password {
			t.Errorf("%s password = %q, want %q", tc.id, got, tc.password)
		}
	}
}

func TestSeedBuiltinPresets_NeverCarriesAMachineField(t *testing.T) {
	// The whole preset safety story — a preset cannot carry a device id — must
	// hold for the baked-in ones too. They are hand-built here rather than
	// Extract-ed from a config, so this is the guard that a future edit to their
	// Fields map cannot smuggle a machine field in.
	a, _ := newTestApp(t)
	a.seedBuiltinPresets()

	list, err := presets.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, p := range list {
		for _, tag := range presets.MachineFields {
			if _, ok := p.Fields[tag]; ok {
				t.Errorf("built-in preset %q carries machine field %q", p.ID, tag)
			}
		}
	}
}

func TestSeedBuiltinPresets_IsIdempotentAndPreservesEdits(t *testing.T) {
	a, _ := newTestApp(t)
	a.seedBuiltinPresets()

	// Edit one preset's host, then seed again. "Always present" is create-if-
	// missing, so the edit must survive a re-seed and the count must not grow.
	p, err := presets.Load("matchg")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p.Fields["m2lxHost"] = json.RawMessage(`"edited.example.com"`)
	if err := presets.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}

	a.seedBuiltinPresets()

	list, err := presets.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != len(builtinPresetLetters) {
		t.Fatalf("re-seed changed the preset count to %d, want %d", len(list), len(builtinPresetLetters))
	}
	again, err := presets.Load("matchg")
	if err != nil {
		t.Fatalf("Load after re-seed: %v", err)
	}
	presetFieldString(t, again, "m2lxHost", "edited.example.com")
}
