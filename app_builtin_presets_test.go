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
		// Two audio-only outputs per instance since 1.6.2: the same encode to
		// 40901 and 40902. videoSource "none" is the one machine-classed value
		// a preset may carry (presets.travelsAsInstance).
		presetFieldInt(t, p, "srtPort", 40901)
		presetFieldInt(t, p, "srtSecondPort", 40902)
		presetFieldInt(t, p, "srtReturnPort", 40504)
		presetFieldString(t, p, "videoSource", "none")

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
			raw, ok := p.Fields[tag]
			if !ok {
				continue
			}
			// The one exception, with the one value: videoSource "none" says the
			// instance takes no picture, which is a fact about the venue and not
			// about this PC's hardware (presets.travelsAsInstance). Any other
			// videoSource names a device and must not be here.
			if tag == "videoSource" && string(raw) == `"none"` {
				continue
			}
			t.Errorf("built-in preset %q carries machine field %q (%s)", p.ID, tag, raw)
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

func TestSeedBuiltinPresets_UpgradesUneditedBuiltinValuesAndKeepsEdits(t *testing.T) {
	// A preset seeded by a build before 1.6.2 says srtPort 40004 and knows
	// nothing of a second output or of videoSource. The re-seed brings a field
	// that still holds the OLD built-in value up to date and adds the fields
	// that did not exist — but a port the operator set to something of their
	// own is theirs, and is left exactly as they set it.
	a, _ := newTestApp(t)
	a.seedBuiltinPresets()

	old, err := presets.Load("matchg")
	if err != nil {
		t.Fatalf("Load matchg: %v", err)
	}
	old.Fields["srtPort"] = json.RawMessage(`40004`)
	old.Fields["statusKey"] = json.RawMessage(`"cam4"`)
	delete(old.Fields, "srtSecondPort")
	delete(old.Fields, "videoSource")
	if err := presets.Save(old); err != nil {
		t.Fatalf("Save matchg as a pre-1.6.2 preset: %v", err)
	}

	edited, err := presets.Load("matchh")
	if err != nil {
		t.Fatalf("Load matchh: %v", err)
	}
	edited.Fields["srtPort"] = json.RawMessage(`41234`)
	edited.Fields["statusKey"] = json.RawMessage(`"cam9"`)
	delete(edited.Fields, "srtSecondPort")
	if err := presets.Save(edited); err != nil {
		t.Fatalf("Save matchh with an operator's port: %v", err)
	}

	a.seedBuiltinPresets()

	g, err := presets.Load("matchg")
	if err != nil {
		t.Fatalf("Load matchg after re-seed: %v", err)
	}
	presetFieldInt(t, g, "srtPort", 40901)
	presetFieldInt(t, g, "srtSecondPort", 40902)
	presetFieldString(t, g, "videoSource", "none")
	// The status key moved from the router input to the first mic input on
	// 2026-09-10; a preset still saying cam4 follows, one the operator set does not.
	presetFieldString(t, g, "statusKey", "MIC 1")

	h, err := presets.Load("matchh")
	if err != nil {
		t.Fatalf("Load matchh after re-seed: %v", err)
	}
	presetFieldInt(t, h, "srtPort", 41234)
	presetFieldInt(t, h, "srtSecondPort", 40902)
	presetFieldString(t, h, "statusKey", "cam9")
}
