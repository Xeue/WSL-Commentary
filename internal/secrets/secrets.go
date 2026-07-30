// Package secrets is the application's only route to Windows Credential
// Manager. It holds exactly two values: the M2L-X sign-in password and the SRT
// passphrase. Neither ever appears in config.json, in a log line, or in a
// GStreamer URI.
//
// Owner: WP-1. No other work package writes files in this directory.
package secrets

import (
	"errors"

	"github.com/danieljoos/wincred"
)

// Logical key names. These are the only values any caller passes to Get or Set.
const (
	// KeyM2LX is the M2L-X sign-in password, sent as the "password" field of
	// POST /api/local_auth/signin.
	KeyM2LX = "m2lx"

	// KeySRT is the SRT passphrase, set on srtsink with g_object_set so that it
	// never has to be percent-encoded into a URI.
	KeySRT = "srt"
)

// Credential Manager generic-credential target names. The mapping from logical
// key to target is fixed by specification section 9.
const (
	// TargetPrefix is prepended to a logical key to form a target name.
	TargetPrefix = "WSLComms/"

	// TargetM2LX is the Credential Manager target holding the M2L-X password.
	TargetM2LX = TargetPrefix + KeyM2LX

	// TargetSRT is the Credential Manager target holding the SRT passphrase.
	TargetSRT = TargetPrefix + KeySRT
)

// ErrNotFound is returned by Get when no credential exists for the key. It is
// not a failure: on first run neither credential has been entered yet, and
// callers must distinguish "not set" from "Credential Manager is broken".
var ErrNotFound = errors.New("secrets: credential not found")

// ErrUnknownKey is returned when a key other than KeyM2LX or KeySRT is used.
var ErrUnknownKey = errors.New("secrets: unknown key")

// Store reads and writes the two application secrets.
//
// Implementations must treat the returned string as sensitive: it must not be
// logged, embedded in a URI, or emitted across the Wails boundary.
type Store interface {
	// Get returns the secret stored under key, which must be KeyM2LX or
	// KeySRT. It returns ErrNotFound if the credential has never been written.
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
	return notImplementedStore{}
}

// notImplementedStore is the WP-0 placeholder. WP-1 replaces it with a wincred
// implementation. It is a value rather than a nil interface so that callers
// wired up before WP-1 lands get an error rather than a panic.
type notImplementedStore struct{}

func (notImplementedStore) Get(key string) (string, error) {
	return "", errors.New("not implemented: WP-1")
}

func (notImplementedStore) Set(key, value string) error {
	return errors.New("not implemented: WP-1")
}

// Referenced so that `go mod tidy` keeps the frozen dependency on wincred
// before WP-1 writes the real implementation. WP-1 deletes this line.
var _ = wincred.GetGenericCredential
