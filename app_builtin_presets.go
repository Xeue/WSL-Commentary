//go:build dev || production || bindings

// app_builtin_presets.go seeds the facility's seven M2L-X instance presets and
// their passwords, and keeps the built-in values current across upgrades.
//
// Owner: WP-5b/presets.
//
// # What a built-in preset says
//
// Each of the seven instances — Match G, H, I, J, K, L and T — is one M2L-X
// deployment with the same shape: a host named after its letter, an alias to
// sign in with, the status key the lamps read, and the SRT ports. Since 1.6.2
// the commentary is sent to TWO audio-only mic inputs per instance, ports
// 40901 and 40902, carrying the same encode; the picture return is unchanged.
// The status key is therefore the first MIC input's node, "MIC 1" — the
// switcher_status frame carries the mic inputs as nodes of exactly the router
// inputs' shape (internal/m2lx/testdata, 2026-07-31) — rather than cam4.
// That is why the presets carry srtSecondPort and videoSource "none" — the
// one videoSource value a preset may carry, because it describes the
// instance's input rather than this PC's hardware (see
// internal/presets.travelsAsInstance).
//
// # Seeding, and the upgrade rule
//
// A preset that does not exist is written whole. One that does exist is the
// operator's: an edit they made survives every re-seed (the test pins it). But
// the built-in VALUES change — the ports did — and an operator who never
// touched a preset should get the new ones without re-installing. So a field
// is rewritten only when it is ABSENT, or still holds a PREVIOUS built-in
// value; a field holding anything else was edited and is left alone. The
// password is re-asserted every launch: it is the facility's, not the
// operator's.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"wslcomms/internal/config"
	"wslcomms/internal/presets"
	"wslcomms/internal/secrets"
)

// builtinPresetLetters are the seven instances' letters. The preset id is
// "match<letter>", the host "m2lx-wslstudios-match<letter>.etapsiota.com".
var builtinPresetLetters = []string{"g", "h", "i", "j", "k", "l", "t"}

const (
	builtinStatusKey     = "MIC 1"
	builtinSRTPort       = 40901
	builtinSRTSecondPort = 40902
	builtinSRTReturnPort = 40504
	builtinVideoSource   = config.VideoSourceNone

	builtinHostFmt     = "m2lx-wslstudios-match%s.etapsiota.com"
	builtinPasswordFmt = "WSLStud10sM4tch%s"
)

// builtinPreviousValues are values a built-in field used to hold. A preset
// still holding one was never edited by the operator and is upgraded; one
// holding anything else was, and is left alone.
var builtinPreviousValues = map[string][]json.RawMessage{
	"srtPort":   {jsonRaw(40004)},
	"statusKey": {jsonRaw("cam4")},
}

// builtinFields are the values a built-in preset carries today.
func builtinFields(letter string) map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"m2lxHost":      jsonRaw(fmt.Sprintf(builtinHostFmt, letter)),
		"alias":         jsonRaw("match" + letter),
		"statusKey":     jsonRaw(builtinStatusKey),
		"srtPort":       jsonRaw(builtinSRTPort),
		"srtSecondPort": jsonRaw(builtinSRTSecondPort),
		"srtReturnPort": jsonRaw(builtinSRTReturnPort),
		"videoSource":   jsonRaw(builtinVideoSource),
	}
}

// seedBuiltinPresets ensures the seven facility presets and their M2L-X
// passwords exist, and upgrades built-in values the operator never edited.
// It runs at every startup and is idempotent.
func (a *App) seedBuiltinPresets() {
	for _, letter := range builtinPresetLetters {
		upper := strings.ToUpper(letter)
		id := "match" + letter
		password := fmt.Sprintf(builtinPasswordFmt, upper)
		fields := builtinFields(letter)

		p, err := presets.Load(id)
		switch {
		case errors.Is(err, presets.ErrNotFound):
			p = presets.Preset{
				Version:         presets.Version,
				ID:              id,
				Name:            "Match " + upper,
				CredentialScope: id,
				SavedAt:         time.Now().UTC(),
				Fields:          fields,
			}
			if err := presets.Save(p); err != nil {
				log.Printf("wslcomms: seeding built-in preset %q: %v", id, err)
			} else {
				log.Printf("wslcomms: seeded built-in preset %q (%s)", id, string(fields["m2lxHost"]))
			}
		case err != nil:
			log.Printf("wslcomms: built-in preset %q is present but unreadable (%v); leaving the file as is", id, err)
		default:
			if changed := upgradeBuiltinFields(p.Fields, fields); len(changed) > 0 {
				p.SavedAt = time.Now().UTC()
				if err := presets.Save(p); err != nil {
					log.Printf("wslcomms: upgrading built-in preset %q: %v", id, err)
				} else {
					log.Printf("wslcomms: upgraded built-in preset %q: %s", id, strings.Join(changed, ", "))
				}
			}
		}

		key, err := secrets.ScopedKey(id, secrets.KeyM2LX)
		if err != nil {
			log.Printf("wslcomms: scoping the built-in password for %q: %v", id, err)
			continue
		}
		if cur, err := a.store.Get(key); err != nil || cur != password {
			if err := a.store.Set(key, password); err != nil {
				log.Printf("wslcomms: seeding the built-in password for %q: %v", id, err)
			}
		}
	}
}

// upgradeBuiltinFields writes the current built-in values into an existing
// preset's fields where a field is absent or still holds a previous built-in
// value, and returns the tags it changed. An edited field is left alone.
func upgradeBuiltinFields(have, want map[string]json.RawMessage) []string {
	var changed []string
	for tag, value := range want {
		cur, present := have[tag]
		if present && bytes.Equal(bytes.TrimSpace(cur), bytes.TrimSpace(value)) {
			continue
		}
		if present && !isPreviousBuiltinValue(tag, cur) {
			continue
		}
		have[tag] = value
		changed = append(changed, tag)
	}
	return changed
}

func isPreviousBuiltinValue(tag string, cur json.RawMessage) bool {
	for _, old := range builtinPreviousValues[tag] {
		if bytes.Equal(bytes.TrimSpace(cur), bytes.TrimSpace(old)) {
			return true
		}
	}
	return false
}

func jsonRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("wslcomms: encoding a built-in preset field (%v): %v", v, err)
		return json.RawMessage("null")
	}
	return b
}
