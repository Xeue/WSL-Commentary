//go:build dev || production || bindings

// The M2L-X instance presets' bound surface, and the credential-scope decorator
// that makes per-preset secrets work without editing a single credential read
// site.
//
// Owner: the presets work package (see CONTRACT.md's ownership row). Same build
// tags as app.go, because these are methods on *App and Wails binds every
// exported one — the seven public methods in this file ARE a contract widening,
// recorded in app.go's header list and in CONTRACT.md's bound-surface table.
//
// # What a preset is, in one paragraph
//
// One M2L-X deployment's coordinates — host, alias, ports, key lengths,
// statusKey, the measured tile and gain — saved as a file under
// %APPDATA%\WSLComms\presets and applied onto the live config as a MERGE.
// internal/presets owns the whitelist that makes "a preset cannot carry a
// device id" true by construction; this file owns the wiring: which locks, in
// which order, what is refused, and which Credential Manager scope the three
// passwords are read from.
//
// A preset says WHICH VENUE and never which match: the event id is discovered
// from the instance (App.ListEvents over internal/m2lx's GET
// /api/events/overview), not remembered — see internal/presets.DiscoveredFields
// for the reasoning and for what that means for the files already on disk.
//
// # The credential scope, and why it is a decorator rather than four edits
//
// Each preset's secrets live under their own Credential Manager targets —
// WSLComms/<scope>/{m2lx,srt,srtreturn} — with the EMPTY scope resolving to
// the three legacy targets, so a machine that never applies a preset never
// changes behaviour. The scope is applied by scopedStore, installed once in
// NewApp around the real store, NOT at the call sites: the four credential
// read sites (app.go's srtPassphrase and signInLoop, app_picture.go's
// picturePassphrase, app_return.go's returnPassphrase) need ZERO edits, and
// two of them are in WP-P's and WP-R's exclusively-owned files, which a
// call-site change would have trespassed on.
//
// The active scope is an atomic.Pointer[string] on App, NOT a field under
// cfgMu, because app.go's signInLoop reads the M2L-X password on the
// control-plane goroutine while ctlMu is held, and app.go's own lock-order
// header says ctlMu is never held with cfgMu — a lock-free read has no
// ordering to violate.
package main

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"wslcomms/internal/config"
	"wslcomms/internal/presets"
	"wslcomms/internal/secrets"
)

// errPresetWhileSending is ApplyPreset's refusal while a session is running.
//
// It is a refusal, not a warning dialog, and the reason is invisible failure:
// SaveConfig deliberately does not restart a session and Start snapshots the
// config once, so a mid-match apply would leave the feed going to the
// PREVIOUS instance's ingest with every control on screen showing the new one
// and every lamp green. The refusal gate is also what makes the unconditional
// monitor teardown on apply affordable — see ApplyPreset.
var errPresetWhileSending = errors.New(
	"wslcomms: a preset cannot be applied while the feed is SENDING — the running session would keep " +
		"sending to the previous instance while every control showed the new one. Press STOP, apply " +
		"the preset, then START")

// errDeleteActivePreset is DeletePreset's refusal to delete the preset this PC
// is currently pointed at.
//
// Deleting it would leave the active credential scope pointing at vault
// entries no preset owns any more, and clearing the scope back to "" instead
// would silently re-point a running app at the machine's legacy passwords
// mid-event. Applying another preset first is the one path that keeps the
// record and the vault in step.
var errDeleteActivePreset = errors.New(
	"wslcomms: this preset is the ACTIVE one — apply another preset first, then delete this one")

// errDeleteLegacyCredentials is DeletePreset's refusal to delete the legacy
// (empty-scope) Credential Manager entries, even when asked.
//
// The migration preset keeps the empty scope so that the machine's existing
// passwords carry over without retyping. Its credentials are therefore the
// SAME vault entries a pre-preset installation uses, and a Delete pressed on
// that one preset must not wipe the only passwords on the machine.
var errDeleteLegacyCredentials = errors.New(
	"wslcomms: this preset uses the machine's original Credential Manager entries " +
		"(WSLComms/m2lx, WSLComms/srt, WSLComms/srtreturn); they are not deleted with it. " +
		"Delete the preset without its credentials, or clear the passwords from the Settings screen")

// scopedStore decorates a secrets.Store with the ACTIVE credential scope: every
// Get/Set resolves the logical key through secrets.ScopedKey(scope(), key)
// before it reaches the inner store.
//
// A key that already contains '/' is passed through UNCHANGED. That is what
// lets DeletePreset address a NON-active preset's scope explicitly (it builds
// "scope/base" with secrets.ScopedKey and hands it straight down) without this
// decorator re-scoping it into "active/scope/base", which ScopedKey would
// rightly reject. Nothing else in the application ever builds a scoped key;
// the four credential read sites all pass the three bare constants.
type scopedStore struct {
	inner secrets.Store
	// scope returns the active credential scope: "" for the legacy targets. It
	// is App.credentialScope — a lock-free atomic read, callable from the
	// control-plane goroutine while ctlMu is held.
	scope func() string
}

func (s scopedStore) resolve(key string) (string, error) {
	if strings.ContainsRune(key, '/') {
		// Already scoped by a caller that named a scope deliberately
		// (DeletePreset). The inner store's targetFor validates it.
		return key, nil
	}
	return secrets.ScopedKey(s.scope(), key)
}

// Get implements secrets.Store.
func (s scopedStore) Get(key string) (string, error) {
	k, err := s.resolve(key)
	if err != nil {
		return "", err
	}
	return s.inner.Get(k)
}

// Set implements secrets.Store.
func (s scopedStore) Set(key, value string) error {
	k, err := s.resolve(key)
	if err != nil {
		return err
	}
	return s.inner.Set(k, value)
}

var _ secrets.Store = scopedStore{}

// credentialScope returns the active credential scope: "" until a preset with
// its own scope has been applied. Lock-free — see the file comment for why it
// must be.
func (a *App) credentialScope() string {
	p := a.credScope.Load()
	if p == nil {
		return ""
	}
	return *p
}

// setCredentialScope records the active credential scope.
func (a *App) setCredentialScope(scope string) {
	a.credScope.Store(&scope)
}

// PresetCredentialStatus reports whether each of the three credentials EXISTS
// for the active scope — three booleans, never a value.
//
// This is a deliberate, narrow exception to "there is deliberately no getter
// for a secret" (app.go's header, internal/secrets' package doc, CONTRACT.md),
// and the exception is recorded in all three places rather than slipped in.
// The reason it exists: after applying a preset the operator has to know
// whether to type the passwords for that instance, and the frontend's
// "set this session" badge is a lie for a scope that was never written to in
// this run of the app. Existence is not the secret; the values still never
// cross the Wails boundary, are never logged, and cannot be fetched.
type PresetCredentialStatus struct {
	// Scope is the active credential scope, "" meaning the legacy entries.
	Scope string `json:"scope"`
	// M2LX / SRT / SRTReturn report whether a credential exists under this
	// scope for each of the three logical keys.
	M2LX      bool `json:"m2lx"`
	SRT       bool `json:"srt"`
	SRTReturn bool `json:"srtreturn"`
}

// ListPresets returns every readable preset for the Settings picker, sorted by
// name. Each Summary carries the preset's whitelisted fields, because the
// confirm dialog diffs a preset against the live config BEFORE anything is
// applied and there is deliberately no separate per-preset fetch binding.
//
// It returns an empty slice, never nil — the frontend renders a list and
// should not have to special-case null.
func (a *App) ListPresets() ([]presets.Summary, error) {
	list, err := presets.List()
	if err != nil {
		return nil, err
	}
	out := make([]presets.Summary, 0, len(list))
	for _, p := range list {
		out = append(out, p.Summary())
	}
	return out, nil
}

// SavePreset extracts the whitelisted fields of the CURRENT SAVED configuration
// into a preset named name, writes it, and points the active record at it.
//
// # The migration rule, in Go rather than in the UI
//
// The FIRST preset ever created on a machine with no active record adopts the
// legacy credential scope "": it resolves to WSLComms/m2lx, WSLComms/srt and
// WSLComms/srtreturn — the entries the machine already has — so the upgrade
// path is "press Save current as… and nothing is retyped", and
// internal/secrets' TestTargetNames keeps passing untouched. Every preset
// after that gets its own scope (its id). Secrets are NEVER copied between
// scopes: copying would duplicate a passphrase into a second vault entry that
// deleting the first would not remove, and there is no getter to copy with —
// deliberately.
//
// config.json itself is untouched by all of this: no schema change, no version
// field, no new key, which is the whole reason presets live in their own
// directory.
//
// Saving under a name whose id already exists UPDATES that preset — keeping
// its id and, critically, its credentialScope — when the names match
// case-insensitively (the operator re-saving the same instance). A DIFFERENT
// name that derives the same id is refused by name: silently overwriting
// "Wembley" because somebody typed "wembley!" is a preset lost without a
// keystroke that said so.
func (a *App) SavePreset(name string) (presets.Summary, error) {
	id, err := presets.DeriveID(name)
	if err != nil {
		return presets.Summary{}, err
	}
	displayName := strings.TrimSpace(name)

	list, err := presets.List()
	if err != nil {
		return presets.Summary{}, err
	}
	_, activeFound, activeErr := presets.LoadActive()

	scope := id
	for _, existing := range list {
		if existing.ID != id {
			continue
		}
		if !strings.EqualFold(existing.Name, displayName) {
			return presets.Summary{}, fmt.Errorf(
				"wslcomms: the name %q would collide with the existing preset %q (both become the id %q) — "+
					"choose a more distinct name", displayName, existing.Name, id)
		}
		// A re-save of the same instance: keep its scope, whatever it is. In
		// particular the migration preset keeps "", so re-saving it cannot
		// silently strand the legacy passwords.
		scope = existing.CredentialScope
	}
	if len(list) == 0 && !activeFound && activeErr == nil {
		// The migration case: nothing preset-shaped has ever existed here, so
		// this preset IS the machine's current identity and inherits the
		// legacy vault entries by keeping the empty scope.
		scope = ""
	}

	fields, err := presets.Extract(a.snapshotConfig())
	if err != nil {
		return presets.Summary{}, err
	}
	now := time.Now().UTC()
	p := presets.Preset{
		Version:         presets.Version,
		ID:              id,
		Name:            displayName,
		CredentialScope: scope,
		SavedAt:         now,
		Fields:          fields,
	}
	if err := presets.Save(p); err != nil {
		return presets.Summary{}, err
	}

	// The preset just saved describes the configuration this PC is running
	// RIGHT NOW, so the active record points at it in both the migration case
	// and the ordinary one, and the credential scope follows.
	if err := presets.SaveActive(presets.ActiveRecord{ID: id, CredentialScope: scope, AppliedAt: now}); err != nil {
		return presets.Summary{}, err
	}
	a.setCredentialScope(scope)

	return p.Summary(), nil
}

// ApplyPreset merges a preset onto the live configuration and RETURNS the
// merged config.
//
// # The return type is a correctness contract, not a convenience
//
// The frontend re-writes the WHOLE currentConfig on the next dropdown change,
// so an apply that returned only an error would be clobbered by the very next
// control the operator touched — the stale page cache winning over the preset
// that was just applied, deterministically. Returning the merged document and
// having the page ASSIGN it (`currentConfig = await backend.applyPreset(id)`)
// is what removes that window; if this ever stops returning the config, that
// bug comes straight back.
//
// It also makes the merge's OMISSIONS load-bearing, and eventId is the one that
// matters: the page adopts this document wholesale, so a merged config with a
// blank event id would blank the box and take the KVS monitor down for anyone
// who applied a preset — the regression the operator would report as "picking
// my venue killed the picture". It cannot happen, because the merge starts from
// snapshotConfig() and presets.Apply only writes keys the preset actually
// carries, and eventId is no longer one of them. Pinned by
// TestApplyPresetKeepsTheEventIDTheMachineAlreadyHad.
//
// # The order of operations, and which lock is held where
//
//  1. sessMu: refuse if a session is running (or the app is closing), release.
//  2. Load the preset and Filter its fields (whitelist; hand-edited keys are
//     dropped and REPORTED, not trusted and not fatal).
//  3. cfgMu: copy the live config, presets.Apply onto the COPY, release.
//  4. Save the copy to config.json (no lock — Save is the same atomic write
//     SaveConfig uses).
//  5. cfgMu: install the copy as the live config, release.
//  6. Record the active preset and switch the credential scope.
//  7. DisarmMixer.
//  8. startControlPlane.
//  9. Return the merged config.
//
// Steps 1-5 respect app.go's documented sessMu -> cfgMu order; steps 7 and 8
// run with NO lock held, because startControlPlane calls snapshotConfig (which
// takes cfgMu) before taking ctlMu.
//
// Step 7 is not tidiness: mixer.ArmWindow is two minutes, so an apply inside
// an open arm window would leave a write gate open that is now pointed at a
// DIFFERENT desk, and SendMixerCommands' only gate is that window.
//
// Step 8 — and the frontend's unconditional KVS-monitor and picture rebuild —
// happen whether or not any numeric field changed, because the credential
// SCOPE changed: the return passphrase is different even when no port is.
// That is affordable precisely because step 1 refused while sending; a future
// "apply anyway" escape hatch would reintroduce the feed-to-the-old-instance
// failure this refusal exists to prevent.
func (a *App) ApplyPreset(id string) (*config.Config, error) {
	// Step 1. The refusal is the load-bearing gate — see the error's comment.
	a.sessMu.Lock()
	if a.closing.Load() {
		a.sessMu.Unlock()
		return nil, errShuttingDown
	}
	if a.session != nil {
		a.sessMu.Unlock()
		return nil, errPresetWhileSending
	}
	a.sessMu.Unlock()

	// Step 2.
	p, err := presets.Load(id)
	if err != nil {
		return nil, err
	}
	kept, ignored := presets.Filter(p.Fields)

	// Step 3. Apply onto a COPY, so a failure mid-merge cannot leave the live
	// config half-written. snapshotConfig takes and releases cfgMu itself.
	merged := a.snapshotConfig()
	if _, err := presets.Apply(merged, kept); err != nil {
		return nil, err
	}

	// Step 4. Persist BEFORE the in-memory swap, mirroring SaveConfig: if the
	// disk write fails, the running app still agrees with config.json.
	if err := merged.Save(); err != nil {
		return nil, err
	}

	// Step 5.
	a.cfgMu.Lock()
	a.cfg = merged
	a.cfgMu.Unlock()

	// Step 6. The record first, then the in-memory scope: a crash between the
	// two comes back up reading the record, which is the durable truth.
	now := time.Now().UTC()
	if err := presets.SaveActive(presets.ActiveRecord{ID: p.ID, CredentialScope: p.CredentialScope, AppliedAt: now}); err != nil {
		// The config applied but the active record did not stick: the scope is
		// NOT switched, so credentials and record stay consistent with each
		// other (both still the previous instance's). Said out loud, because
		// the operator now has a merged config with the old passwords.
		return nil, fmt.Errorf(
			"wslcomms: the preset's fields were applied, but recording it as the active preset failed — "+
				"the previous credential scope is still in use: %w", err)
	}
	a.setCredentialScope(p.CredentialScope)

	// Step 7.
	if err := a.DisarmMixer(); err != nil {
		// Disarm failing is worth a log line, not a failed apply: the gate
		// window expires by itself within two minutes either way.
		log.Printf("wslcomms: disarming the mixer while applying preset %q: %v", p.ID, err)
	}

	// Step 8.
	a.startControlPlane()

	// The dropped keys are surfaced on the "error" event — the banner — rather
	// than returned, because the bound signature is the correctness contract
	// above and Wails methods return one value plus an error. The frontend
	// also computes them client-side for the confirm dialog; this is the
	// authoritative after-the-fact record.
	if len(ignored) > 0 {
		a.emitError(fmt.Errorf(
			"wslcomms: preset %q carried keys that are not part of a preset and were ignored: %s. "+
				"Device ids, the slate path and live monitoring choices belong to this PC and were left untouched",
			p.Name, strings.Join(ignored, ", ")))
	}

	// Step 9.
	return merged, nil
}

// RenamePreset changes a preset's display name — never its id, never its
// credential scope. internal/presets.Rename enforces that structurally; see
// its comment for the orphaned-credentials failure the restriction prevents.
func (a *App) RenamePreset(id, name string) error {
	return presets.Rename(id, name)
}

// DeletePreset removes a preset file and, only when asked AND only for a scope
// of the preset's own, its three Credential Manager entries.
//
// Two refusals, both named:
//
//   - the ACTIVE preset cannot be deleted (apply another first) — see
//     errDeleteActivePreset;
//   - the legacy (empty) scope's credentials cannot be deleted through this
//     path even when alsoDeleteCredentials is true — see
//     errDeleteLegacyCredentials. The whole call refuses rather than half
//     succeeding, so the operator is told before anything is gone.
//
// Credential deletion goes through store.Set(scopedKey, ""), which
// internal/secrets already implements as a delete; the scoped key names the
// PRESET'S scope explicitly, and scopedStore passes an already-scoped key
// through untouched rather than re-scoping it to the active preset's.
func (a *App) DeletePreset(id string, alsoDeleteCredentials bool) error {
	p, err := presets.Load(id)
	if err != nil {
		return err
	}

	rec, _, recErr := presets.LoadActive()
	if recErr == nil && rec.ID == p.ID {
		return errDeleteActivePreset
	}

	if alsoDeleteCredentials && p.CredentialScope == "" {
		return errDeleteLegacyCredentials
	}
	// A preset's scope is its own id — SavePreset writes nothing else (the one
	// exception, the migration preset's legacy "" scope, is refused above). A
	// FILE whose scope names some other id can only be a hand-edit, and
	// honouring it would let deleting one preset silently wipe another's three
	// vault entries. Refuse the credential half outright; the operator who
	// really wants that deletion can apply the other preset and delete IT.
	if alsoDeleteCredentials && p.CredentialScope != p.ID {
		return fmt.Errorf(
			"wslcomms: preset %q names credential scope %q, which is not its own — the file has been "+
				"edited by hand. Refusing to delete another preset's credentials; delete the preset "+
				"without its credentials, or delete the preset that owns that scope", p.ID, p.CredentialScope)
	}

	if err := presets.Delete(id); err != nil {
		return err
	}

	if !alsoDeleteCredentials {
		return nil
	}
	var errs []error
	for _, base := range []string{secrets.KeyM2LX, secrets.KeySRT, secrets.KeySRTReturn} {
		key, err := secrets.ScopedKey(p.CredentialScope, base)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := a.store.Set(key, ""); err != nil {
			errs = append(errs, fmt.Errorf("deleting the %s credential for scope %q: %w", base, p.CredentialScope, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf(
			"wslcomms: the preset file was deleted, but not all of its Credential Manager entries were: %w", err)
	}
	return nil
}

// GetActivePreset returns the active-preset record: which preset this PC is
// pointed at, and under which credential scope. The zero record — empty id,
// empty scope — means no preset has been applied and the legacy vault entries
// are in use.
//
// A corrupt record returns the zero record rather than an error: the UI's
// honest rendering of that state is "no preset applied", which is exactly what
// the credential fallback does too (startup already reported the corruption on
// the error event, once, when it happened).
func (a *App) GetActivePreset() (presets.ActiveRecord, error) {
	rec, _, err := presets.LoadActive()
	if err != nil {
		log.Printf("wslcomms: reading the active-preset record: %v", err)
		return presets.ActiveRecord{}, nil
	}
	return rec, nil
}

// GetPresetCredentialStatus reports whether each of the three credentials
// exists under the ACTIVE scope — booleans only, never values. See
// PresetCredentialStatus for why this narrow exception to "no getter" exists
// and where it is recorded.
func (a *App) GetPresetCredentialStatus() (PresetCredentialStatus, error) {
	status := PresetCredentialStatus{Scope: a.credentialScope()}

	for _, probe := range []struct {
		key string
		set func(bool)
	}{
		{secrets.KeyM2LX, func(v bool) { status.M2LX = v }},
		{secrets.KeySRT, func(v bool) { status.SRT = v }},
		{secrets.KeySRTReturn, func(v bool) { status.SRTReturn = v }},
	} {
		_, err := a.store.Get(probe.key)
		switch {
		case err == nil:
			probe.set(true)
		case errors.Is(err, secrets.ErrNotFound):
			probe.set(false)
		default:
			// Credential Manager itself failing is a fault, not a status, and
			// pretending "not set" would send the operator off to retype a
			// password into a vault that is broken.
			return PresetCredentialStatus{}, fmt.Errorf(
				"wslcomms: checking whether a credential exists for scope %q: %w", status.Scope, err)
		}
	}
	return status, nil
}
