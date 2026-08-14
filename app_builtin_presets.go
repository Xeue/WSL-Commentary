//go:build dev || production || bindings

// The seven fixed facility instances, baked into the build.
//
// Owner: presets work package (same as app_presets.go). WSL Studios runs seven
// permanent M2L-X deployments — match<letter> for g, h, i, j, k, l and t (the
// set skips m..s and jumps to t) — and every install should find all seven
// waiting in the instance picker without anyone typing a host, a username or a
// password. This file bakes them in and seeds them at startup.
//
// # What is baked, and what is not
//
// Each preset carries only the instance coordinates the operator specified: the
// host, the sign-in name, and "commentary input number 4" expressed three ways
// — statusKey cam4, the SRT contribution port 40004, and the SRT return port
// 40504. The event id is deliberately NOT baked: it changes per live event and
// is auto-selected after sign-in (see App.ListEvents and events.js). Everything
// else stays at config defaults, so applying a match changes only these five
// fields and leaves an operator's other settings untouched.
//
// # The passwords
//
// These are real credentials for the facility's own private instances, baked in
// at the owner's explicit request. Each preset's M2L-X password is seeded into
// the OS credential store — Windows Credential Manager, or the login Keychain on
// macOS; internal/secrets picks — under that preset's OWN scope
// (WSLComms/match<letter>/m2lx), exactly where the scoped credential path reads
// it at sign-in — so applying "Match G" and pressing START just works. They are
// per-instance and never collide. The target string is the same on both
// platforms, so a preset seeded on one machine is looked for under the same name
// on the other; only the vault it lands in differs.
//
// # Idempotency: "always present" without clobbering
//
// Seeding runs on every launch. A preset FILE is created only if it is missing,
// so a later hand-edit survives and a deleted one reappears next launch (which
// is what "always present" means). The password is (re)written only when it is
// not already the baked value, so a normal launch touches Credential Manager
// not at all.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"wslcomms/internal/presets"
	"wslcomms/internal/secrets"
)

// builtinPresetLetters names the seven always-present instances: match<letter>.
// The set is g..l then t — it is NOT a contiguous range, so it is listed rather
// than generated from a start/end pair.
var builtinPresetLetters = []string{"g", "h", "i", "j", "k", "l", "t"}

const (
	// The commentary input is "number 4" on every instance, spelled three ways:
	// the switcher_status node, the SRT contribution ingest port, and the SRT
	// return (output) port.
	builtinStatusKey     = "cam4"
	builtinSRTPort       = 40004
	builtinSRTReturnPort = 40504

	// builtinHostFmt and builtinPasswordFmt are the per-instance host and
	// password patterns. The password's letter is upper-cased.
	builtinHostFmt     = "m2lx-wslstudios-match%s.etapsiota.com"
	builtinPasswordFmt = "WSLStud10sM4tch%s"
)

// seedBuiltinPresets ensures the seven facility presets and their M2L-X
// passwords exist. It is called from startup BEFORE the active-preset record is
// read, so a machine that already has one applied signs in on this launch.
//
// Every failure is logged and stepped over rather than fatal: a machine that
// cannot write one preset file must still open, and the other six must still
// seed.
func (a *App) seedBuiltinPresets() {
	for _, letter := range builtinPresetLetters {
		upper := strings.ToUpper(letter)
		id := "match" + letter
		host := fmt.Sprintf(builtinHostFmt, letter)
		alias := "match" + letter
		password := fmt.Sprintf(builtinPasswordFmt, upper)

		// The preset file: create only if missing, so edits survive and a
		// deleted one comes back.
		if _, err := presets.Load(id); errors.Is(err, presets.ErrNotFound) {
			p := presets.Preset{
				Version:         presets.Version,
				ID:              id,
				Name:            "Match " + upper,
				CredentialScope: id,
				SavedAt:         time.Now().UTC(),
				Fields: map[string]json.RawMessage{
					"m2lxHost":      jsonRaw(host),
					"alias":         jsonRaw(alias),
					"statusKey":     jsonRaw(builtinStatusKey),
					"srtPort":       jsonRaw(builtinSRTPort),
					"srtReturnPort": jsonRaw(builtinSRTReturnPort),
				},
			}
			if err := presets.Save(p); err != nil {
				log.Printf("wslcomms: seeding built-in preset %q: %v", id, err)
			} else {
				log.Printf("wslcomms: seeded built-in preset %q (%s)", id, host)
			}
		} else if err != nil {
			// A file that exists but will not load (corrupt, wrong version) is
			// left alone — overwriting it could destroy an operator's edit — but
			// the password below is still ensured so the instance is usable.
			log.Printf("wslcomms: built-in preset %q is present but unreadable (%v); leaving the file as is", id, err)
		}

		// The password, under this preset's own scope. Written only when it is
		// not already correct, so a steady-state launch does not touch the vault.
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

// jsonRaw marshals a compile-time-constant value into a preset field. The
// inputs here are always string or int constants, so a marshal error is
// impossible; it is guarded rather than ignored only so a future caller with a
// richer value fails loudly.
func jsonRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("wslcomms: encoding a built-in preset field (%v): %v", v, err)
		return json.RawMessage("null")
	}
	return b
}
