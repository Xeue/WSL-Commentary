//go:build darwin

// secrets_darwin.go is the macOS half of the Store: the one route this
// application has to the login Keychain.
//
// Owner: WP-1.
//
// # Why Keychain and not a file beside config.json
//
// The package doc's promise — that none of the three secrets ever appears in
// config.json, in a log line, or in a GStreamer URI — is the reason this
// package exists at all, and it is not a Windows-specific promise. Keychain is
// the macOS facility that keeps it: the secrets live in an OS-managed store
// with per-application access control, not in a file that a support bundle, a
// screen share or a backup sweeps up by accident.
//
// # Why github.com/zalando/go-keyring and not Security.framework directly
//
// CONTRACT.md: "Do not add a cgo import to any package other than
// internal/gst. If you think you need one, you have found a design problem,
// not a missing import." A direct SecItemAdd/SecItemCopyMatching binding needs
// cgo and Security.framework, which would put the second cgo surface in the
// process into the package that handles passwords. go-keyring shells out to
// /usr/bin/security instead, so this package stays pure Go and Gate A
// (CGO_ENABLED=0 go build/vet/test ./...) keeps working on macOS exactly as it
// does on Windows.
//
// The cost of that choice, stated plainly because it is a real one: writing a
// secret runs `security add-generic-password ... -w <secret>`, and the secret
// is an argv element of that short-lived child process. Any process running as
// this user can see it in ps(1) for the duration of the call. Reads and
// deletes do not pass the secret on the command line and are unaffected. The
// exposure window is milliseconds and only opens when the operator saves a
// password on the Settings screen — but it is not nothing, and a future move
// to Security.framework via internal/gst-style cgo, or to a helper that feeds
// the secret over stdin, would close it. Reported under CONTRACT.md rule 3.
//
// # What the operator sees
//
// Nothing, in the shipped app. macOS records an ACL on each item naming the
// code allowed to read it; for a Developer ID-signed binary that ACL is a
// designated requirement (identifier + team), which survives rebuilds and
// re-signings. An UNSIGNED or ad-hoc-signed development build gets a cdhash
// requirement instead, which changes on every `go build`, so a developer
// running an unsigned build is prompted for their login password on every
// read after every rebuild. That is a property of the build, not of this code.
package secrets

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// keychainAccount is the account name every item is stored under.
//
// A Keychain generic password is keyed by the PAIR (service, account), where
// Credential Manager keys by a single target string. targetFor already
// produces the fully-qualified target — "WSLComms/m2lx", or
// "WSLComms/wembley/m2lx" for a scoped one — so the whole identity is carried
// in the service and the account is a constant. Putting the scope in the
// account instead would have split one namespace across two fields and made
// the Windows and macOS target names stop corresponding, which is exactly what
// TestTargetNames exists to prevent.
const keychainAccount = "wslcomms"

// New returns the Store backed by the macOS login Keychain.
//
// It never fails: the login Keychain is always present, and any per-call
// problem surfaces from Get or Set instead. This mirrors the Windows New()
// exactly, so app.go's wire-up is identical on both platforms.
func New() Store {
	return keychainStore{}
}

// keychainStore is the Store implementation backed by the login Keychain via
// /usr/bin/security. It carries no state: every call resolves the logical key
// to a target name and talks to the Keychain directly, exactly as
// credManagerStore does on Windows.
type keychainStore struct{}

// Get implements Store.
func (keychainStore) Get(key string) (string, error) {
	target, err := targetFor(key)
	if err != nil {
		return "", err
	}

	value, err := keyring.Get(target, keychainAccount)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("secrets: reading %s: %w", target, err)
	}
	return value, nil
}

// Set implements Store. Writing an empty value deletes the item rather than
// storing an empty secret, so that a cleared password field on the Settings
// screen actually removes the entry from the Keychain instead of leaving a
// zero-length secret behind. This is the same rule the Windows half applies,
// and app.go depends on it being the same.
func (keychainStore) Set(key, value string) error {
	target, err := targetFor(key)
	if err != nil {
		return err
	}

	if value == "" {
		if err := keyring.Delete(target, keychainAccount); err != nil {
			if errors.Is(err, keyring.ErrNotFound) {
				// Already absent: the caller's goal (no stored secret) holds.
				return nil
			}
			return fmt.Errorf("secrets: deleting %s: %w", target, err)
		}
		return nil
	}

	// keyring.Set replaces an existing item rather than failing on duplicate,
	// which is what the Windows half's cred.Write() does too.
	if err := keyring.Set(target, keychainAccount, value); err != nil {
		return fmt.Errorf("secrets: writing %s: %w", target, err)
	}
	return nil
}
