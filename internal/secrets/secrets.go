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
package secrets

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf16"

	"github.com/danieljoos/wincred"
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

// targetFor maps a logical key to its Credential Manager generic-credential
// target name, per the mapping fixed by specification section 9.
func targetFor(key string) (string, error) {
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
