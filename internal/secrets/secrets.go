// Package secrets is the application's only route to Windows Credential
// Manager. It holds exactly three values: the M2L-X sign-in password, the SRT
// passphrase for the SEND path, and the SRT passphrase for the RETURN path.
// None of them ever appears in config.json, in a log line, or in a GStreamer
// URI.
//
// Owner: WP-1. No other work package writes files in this directory.
//
// # Why the two SRT passphrases are two credentials
//
// They are the keys to two different SRT endpoints on M2L-X, and encryption is
// a per-endpoint setting there rather than an instance-wide one. Measured on
// the live instance:
//
//	Output 1  src=pgm  port 40501  encrypted=false
//	Output 2  src=pvw  port 40502  encrypted=true
//	Output 3  src=cln  port 40503  encrypted=true
//
// The send path dials the commentary INPUT; the return path dials one of those
// OUTPUTS. Storing one passphrase for both would mean that the moment the two
// endpoints disagree — which is the normal case above, not an exotic one —
// setting the key that makes the feed work breaks the monitor, or the reverse,
// with nothing on screen saying which of the two a failure came from. Two
// credentials, two Settings fields, two independent failures.
//
// # Credential SCOPES, added for the M2L-X instance presets
//
// The three logical keys can additionally be scoped to an instance preset:
// ScopedKey("wembley", KeyM2LX) is "wembley/m2lx", resolving to the target
// WSLComms/wembley/m2lx. The EMPTY scope resolves to the three legacy targets
// above, byte-for-byte, so nothing about a pre-preset installation changes and
// TestTargetNames still pins the original strings. The Store interface is
// unchanged — the key carries the scope — because widening Get/Set would break
// every implementation, including the tests' fakes. Reported and approved
// under CONTRACT.md rules 2 and 3. There is still no getter across the Wails
// boundary, and secrets are never copied between scopes: a preset changes
// WHICH vault entries are consulted, never their contents.
package secrets

import (
	"errors"
	"fmt"
	"strings"
)

// Logical key names. These are the only values any caller passes to Get or Set.
const (
	// KeyM2LX is the M2L-X sign-in password, sent as the "password" field of
	// POST /api/local_auth/signin.
	KeyM2LX = "m2lx"

	// KeySRT is the SRT passphrase for the SEND path, set on srtsink with
	// g_object_set so that it never has to be percent-encoded into a URI.
	KeySRT = "srt"

	// KeySRTReturn is the SRT passphrase for the RETURN path, set on srtsrc the
	// same way. It is a SEPARATE credential from KeySRT and not a duplicate of
	// it: see the package doc comment for the measurement that makes them
	// different keys to different endpoints.
	KeySRTReturn = "srtreturn"
)

// Credential Manager generic-credential target names. The mapping from logical
// key to target is fixed by specification section 9.
const (
	// TargetPrefix is prepended to a logical key to form a target name.
	TargetPrefix = "WSLComms/"

	// TargetM2LX is the Credential Manager target holding the M2L-X password.
	TargetM2LX = TargetPrefix + KeyM2LX

	// TargetSRT is the Credential Manager target holding the send path's SRT
	// passphrase.
	TargetSRT = TargetPrefix + KeySRT

	// TargetSRTReturn is the Credential Manager target holding the return
	// path's SRT passphrase.
	TargetSRTReturn = TargetPrefix + KeySRTReturn
)

// ErrNotFound is returned by Get when no credential exists for the key. It is
// not a failure: on first run neither credential has been entered yet, and
// callers must distinguish "not set" from "Credential Manager is broken".
var ErrNotFound = errors.New("secrets: credential not found")

// ErrUnknownKey is returned when a key other than KeyM2LX, KeySRT or
// KeySRTReturn is used.
var ErrUnknownKey = errors.New("secrets: unknown key")

// Store reads and writes the three application secrets.
//
// Implementations must treat the returned string as sensitive: it must not be
// logged, embedded in a URI, or emitted across the Wails boundary.
type Store interface {
	// Get returns the secret stored under key, which must be KeyM2LX, KeySRT
	// or KeySRTReturn. It returns ErrNotFound if the credential has never been
	// written.
	Get(key string) (string, error)

	// Set writes value under key, creating or replacing the generic credential.
	// Setting an empty value deletes the credential.
	Set(key, value string) error
}

// New returns the Store backed by this platform's credential vault: Windows
// Credential Manager (secrets_windows.go) or the macOS login Keychain
// (secrets_darwin.go). Both are declared to never fail — the vault is always
// present — so any per-call problem surfaces from Get or Set instead, and
// app.go's wire-up is identical on both platforms.

// ScopedKey builds the key for a PER-INSTANCE-PRESET credential: scope "wembley"
// and base KeyM2LX become "wembley/m2lx", which targetFor resolves to the
// Credential Manager target "WSLComms/wembley/m2lx". The empty scope returns
// the base unchanged, so it resolves to the three LEGACY targets and every
// existing caller and every pre-preset installation keeps working untouched.
//
// This is additive on purpose, and it lives HERE — the only route to
// Credential Manager the package doc allows — so per-preset credentials cannot
// be built anywhere else. The Store interface itself stays frozen: the KEY
// carries the scope, so credManagerStore and every fake implementing
// Get(key)/Set(key, value) work unchanged. Change reported and approved under
// CONTRACT.md rules 2 and 3 (internal/secrets is WP-1's path).
//
// The scope charset is exactly what internal/presets.DeriveID can produce:
// lower-case ASCII alphanumerics and hyphens, 1..48 characters. Everything
// else is rejected, and the rejections are security decisions rather than
// tidiness:
//
//   - a scope containing '/' would let one preset's target collide with
//     another's, because targetFor splits a scoped key at the LAST slash;
//   - uppercase is rejected rather than folded because Credential Manager
//     target names are case-insensitively matched by some tooling and
//     case-preserved by others — one canonical spelling means reads and writes
//     can never split across two entries;
//   - whitespace and an empty segment are the hand-built key this function
//     exists to make unrepresentable.
//
// Note the adversarial case the tests pin: scope "srt" with base KeyM2LX gives
// "WSLComms/srt/m2lx", which shares nothing with the legacy TargetSRT
// ("WSLComms/srt") — scoped targets always carry two segments after the
// prefix, so a scope spelled like a legacy key cannot collide with it.
func ScopedKey(scope, base string) (string, error) {
	switch base {
	case KeyM2LX, KeySRT, KeySRTReturn:
	default:
		return "", ErrUnknownKey
	}
	if scope == "" {
		return base, nil
	}
	if err := validateScope(scope); err != nil {
		return "", err
	}
	return scope + "/" + base, nil
}

// validateScope accepts exactly the ids internal/presets.DeriveID produces.
// The rule is restated here rather than imported because internal/secrets must
// not depend on internal/presets — the dependency runs the other way — and the
// two charsets drifting apart fails loudly at ScopedKey rather than silently
// minting an unreachable vault entry.
func validateScope(scope string) error {
	if scope == "" {
		return fmt.Errorf("%w: empty credential scope segment", ErrUnknownKey)
	}
	if len(scope) > 48 {
		return fmt.Errorf("%w: credential scope %q is longer than 48 characters", ErrUnknownKey, scope)
	}
	for _, r := range scope {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return fmt.Errorf(
				"%w: credential scope %q contains %q; only lower-case letters, digits and hyphens are valid",
				ErrUnknownKey, scope, r)
		}
	}
	return nil
}

// targetFor maps a logical key to its Credential Manager generic-credential
// target name, per the mapping fixed by specification section 9.
//
// A key with no '/' keeps the original exact three-way switch — the legacy
// targets, byte-for-byte, which is what TestTargetNames pins and what makes
// the instance-preset migration cost the operator nothing.
//
// A key WITH a '/' is a scoped key built by ScopedKey: it splits at the LAST
// slash, validates the scope against the same charset DeriveID produces and
// the base against the same three-way switch, and resolves to
// TargetPrefix + scope + "/" + base. The last-slash split is what makes a
// scope containing '/' detectable — the leftover "scope" fails validation —
// rather than silently re-parsed as a different (scope, base) pair.
func targetFor(key string) (string, error) {
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		scope, base := key[:i], key[i+1:]
		if err := validateScope(scope); err != nil {
			return "", err
		}
		switch base {
		case KeyM2LX, KeySRT, KeySRTReturn:
			return TargetPrefix + scope + "/" + base, nil
		default:
			return "", ErrUnknownKey
		}
	}
	switch key {
	case KeyM2LX:
		return TargetM2LX, nil
	case KeySRT:
		return TargetSRT, nil
	case KeySRTReturn:
		return TargetSRTReturn, nil
	default:
		return "", ErrUnknownKey
	}
}
