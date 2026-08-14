//go:build windows

// secrets_windows.go is the Windows half of the Store: the one route this
// application has to Windows Credential Manager.
//
// Owner: WP-1. Split out of secrets.go unchanged when the macOS half arrived —
// every line below was moved, not rewritten, so the behaviour the original
// tests pin is byte-for-byte the behaviour they pinned before. The shared
// half — the key names, the target names, ScopedKey, validateScope and
// targetFor — stays in secrets.go and is compiled on every platform.
package secrets

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf16"

	"github.com/danieljoos/wincred"
)

// New returns the Store backed by Windows Credential Manager.
//
// It never fails: Credential Manager is always present on Windows 11, and any
// per-call problem surfaces from Get or Set instead.
func New() Store {
	return credManagerStore{}
}

// credManagerStore is the Store implementation backed by
// github.com/danieljoos/wincred, i.e. the Win32 CredReadW / CredWriteW /
// CredDeleteW APIs against the current user's local Credential Manager
// vault. It carries no state: every call resolves the logical key to a
// target name and talks to Credential Manager directly.
type credManagerStore struct{}

// Get implements Store.
func (credManagerStore) Get(key string) (string, error) {
	target, err := targetFor(key)
	if err != nil {
		return "", err
	}

	cred, err := wincred.GetGenericCredential(target)
	if err != nil {
		if errors.Is(err, wincred.ErrElementNotFound) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("secrets: reading %s: %w", target, err)
	}

	value, err := stringFromUTF16LE(cred.CredentialBlob)
	if err != nil {
		return "", fmt.Errorf("secrets: decoding %s: %w", target, err)
	}
	return value, nil
}

// Set implements Store. Writing an empty value deletes the credential rather
// than storing an empty blob, so that a cleared password field on the
// Settings screen actually removes the entry from Credential Manager instead
// of leaving a zero-length secret behind.
func (credManagerStore) Set(key, value string) error {
	target, err := targetFor(key)
	if err != nil {
		return err
	}

	if value == "" {
		cred := wincred.NewGenericCredential(target)
		if err := cred.Delete(); err != nil {
			if errors.Is(err, wincred.ErrElementNotFound) {
				// Already absent: the caller's goal (no stored secret) holds.
				return nil
			}
			return fmt.Errorf("secrets: deleting %s: %w", target, err)
		}
		return nil
	}

	cred := wincred.NewGenericCredential(target)
	cred.CredentialBlob = utf16LEBytes(value)
	if err := cred.Write(); err != nil {
		return fmt.Errorf("secrets: writing %s: %w", target, err)
	}
	return nil
}

// utf16LEBytes encodes s as UTF-16LE with no byte-order mark and no null
// terminator. This is the layout Windows' own Credential Manager UI expects
// in CredentialBlob for a generic credential's password: the wincred package
// treats CredentialBlob as an opaque []byte and does not perform this
// conversion for GenericCredential (only DomainPassword.SetPassword does),
// so callers of the generic-credential API must encode it themselves. Using
// anything else — e.g. the raw UTF-8 bytes of s — round-trips fine through
// this package alone, but produces a value Windows' own tooling renders as
// garbage and silently mis-decodes multi-byte characters if another
// application ever reads WSLComms/m2lx, WSLComms/srt or WSLComms/srtreturn.
func utf16LEBytes(s string) []byte {
	units := utf16.Encode([]rune(s))
	b := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(b[i*2:i*2+2], u)
	}
	return b
}

// stringFromUTF16LE decodes a CredentialBlob written by utf16LEBytes back to
// a Go string. It returns an error if the blob has an odd length, which
// cannot be valid UTF-16LE and indicates the credential was not written by
// this package's encoding.
func stringFromUTF16LE(b []byte) (string, error) {
	if len(b)%2 != 0 {
		return "", errors.New("secrets: credential blob has odd length, not valid UTF-16LE")
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(b[i*2 : i*2+2])
	}
	return string(utf16.Decode(units)), nil
}
