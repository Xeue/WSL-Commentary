package secrets

import (
	"bytes"
	"errors"
	"testing"

	"github.com/danieljoos/wincred"
)

// --- Pure-logic table-driven tests: no Credential Manager access. ---

func TestUTF16LERoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"empty string", ""},
		{"ascii", "hunter2"},
		{"latin-1 accents", "pàsswörd"},
		{"cjk", "日本語パスワード"},
		{"ascii symbols", "p@$$w0rd!#%^&*()"},
		{"astral plane rune, needs a UTF-16 surrogate pair", "clef𝄞clef"},
		{"mixed script and astral plane", "Ω≈ç√∫ 日本語 𝄞 pàss"},
		{"single space", " "},
		{"leading and trailing whitespace preserved", "  padded  "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := utf16LEBytes(tt.value)
			if len(encoded)%2 != 0 {
				t.Fatalf("utf16LEBytes(%q) produced odd-length output: %d bytes", tt.value, len(encoded))
			}
			decoded, err := stringFromUTF16LE(encoded)
			if err != nil {
				t.Fatalf("stringFromUTF16LE error = %v", err)
			}
			if decoded != tt.value {
				t.Errorf("round trip = %q, want %q", decoded, tt.value)
			}
		})
	}
}

func TestUTF16LEBytes_KnownEncoding(t *testing.T) {
	// "AB" in UTF-16LE is the two code units 0x0041, 0x0042, little-endian:
	// bytes 0x41 0x00 0x42 0x00. This pins the byte layout Windows'
	// Credential Manager UI expects, independent of the round-trip test
	// above (which would pass even if encode and decode shared a
	// compensating bug).
	got := utf16LEBytes("AB")
	want := []byte{0x41, 0x00, 0x42, 0x00}
	if !bytes.Equal(got, want) {
		t.Errorf("utf16LEBytes(\"AB\") = % x, want % x", got, want)
	}
}

func TestStringFromUTF16LE_OddLengthErrors(t *testing.T) {
	_, err := stringFromUTF16LE([]byte{0x41, 0x00, 0x42})
	if err == nil {
		t.Error("stringFromUTF16LE with odd-length input: error = nil, want non-nil")
	}
}

func TestStringFromUTF16LE_Empty(t *testing.T) {
	got, err := stringFromUTF16LE(nil)
	if err != nil {
		t.Fatalf("stringFromUTF16LE(nil) error = %v", err)
	}
	if got != "" {
		t.Errorf("stringFromUTF16LE(nil) = %q, want empty string", got)
	}
}

func TestTargetFor(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		want    string
		wantErr bool
	}{
		{"m2lx key", KeyM2LX, TargetM2LX, false},
		{"srt key", KeySRT, TargetSRT, false},
		{"unknown key", "bogus", "", true},
		{"empty key", "", "", true},
		{"case-sensitive: M2LX is not m2lx", "M2LX", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := targetFor(tt.key)
			if (err != nil) != tt.wantErr {
				t.Fatalf("targetFor(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
			}
			if tt.wantErr {
				if !errors.Is(err, ErrUnknownKey) {
					t.Errorf("targetFor(%q) error = %v, want ErrUnknownKey", tt.key, err)
				}
				return
			}
			if got != tt.want {
				t.Errorf("targetFor(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestTargetNames(t *testing.T) {
	// Pin the exact target strings specification section 9 fixes: a typo
	// here would silently split reads and writes across two different
	// Credential Manager entries.
	if TargetM2LX != "WSLComms/m2lx" {
		t.Errorf("TargetM2LX = %q, want %q", TargetM2LX, "WSLComms/m2lx")
	}
	if TargetSRT != "WSLComms/srt" {
		t.Errorf("TargetSRT = %q, want %q", TargetSRT, "WSLComms/srt")
	}
}

// --- Store.{Get,Set} argument validation: no Credential Manager access. ---

func TestCredManagerStore_UnknownKey(t *testing.T) {
	store := New()

	if _, err := store.Get("bogus"); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("Get(%q) error = %v, want ErrUnknownKey", "bogus", err)
	}
	if err := store.Set("bogus", "value"); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("Set(%q, ...) error = %v, want ErrUnknownKey", "bogus", err)
	}
}

// --- Live Windows Credential Manager integration test. ---
//
// This talks to the real Credential Manager vault of whatever account runs
// `go test`, using the exact targets ("WSLComms/m2lx", "WSLComms/srt") the
// production application uses. To avoid destroying a real stored secret if
// this ever runs on a machine where the app has already been configured,
// each subtest backs up whatever is currently stored under its target
// before touching it and restores that exact state in t.Cleanup, whether
// the test passes or fails.

// backupCredential saves the raw CredentialBlob currently stored under
// target, if any, so the test can restore it byte-for-byte afterwards
// regardless of how it was encoded.
func backupCredential(t *testing.T, target string) (blob []byte, existed bool) {
	t.Helper()
	cred, err := wincred.GetGenericCredential(target)
	if err != nil {
		if errors.Is(err, wincred.ErrElementNotFound) {
			return nil, false
		}
		t.Fatalf("backing up pre-existing credential %s: %v", target, err)
	}
	blob = make([]byte, len(cred.CredentialBlob))
	copy(blob, cred.CredentialBlob)
	return blob, true
}

// restoreCredential undoes the test's writes to target: it deletes the
// credential if none existed before the test, or writes the original blob
// back if one did.
func restoreCredential(t *testing.T, target string, blob []byte, existed bool) {
	t.Helper()
	if !existed {
		cred := wincred.NewGenericCredential(target)
		if err := cred.Delete(); err != nil && !errors.Is(err, wincred.ErrElementNotFound) {
			t.Errorf("cleanup: deleting %s: %v", target, err)
		}
		return
	}
	cred := wincred.NewGenericCredential(target)
	cred.CredentialBlob = blob
	if err := cred.Write(); err != nil {
		t.Errorf("cleanup: restoring original credential %s: %v", target, err)
	}
}

// deleteCredential clears target so the test starts from a known state,
// regardless of what backupCredential found.
func deleteCredential(t *testing.T, target string) {
	t.Helper()
	cred := wincred.NewGenericCredential(target)
	if err := cred.Delete(); err != nil && !errors.Is(err, wincred.ErrElementNotFound) {
		t.Fatalf("clearing pre-test state for %s: %v", target, err)
	}
}

func TestCredManagerStore_RoundTrip(t *testing.T) {
	store := New()

	// A passphrase exercising accented Latin, CJK and an astral-plane
	// character that requires a UTF-16 surrogate pair, per the task's
	// requirement to test a round trip including a non-ASCII passphrase.
	const value = "pàsswörd 日本語 𝄞 tëst"
	const overwrite = "replacement-value-42"

	for _, key := range []string{KeyM2LX, KeySRT} {
		t.Run(key, func(t *testing.T) {
			target, err := targetFor(key)
			if err != nil {
				t.Fatalf("targetFor(%q) error = %v", key, err)
			}

			backup, existed := backupCredential(t, target)
			t.Cleanup(func() { restoreCredential(t, target, backup, existed) })
			deleteCredential(t, target) // start from a known-empty state

			if _, err := store.Get(key); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get(%q) on absent credential: error = %v, want ErrNotFound", key, err)
			}

			if err := store.Set(key, value); err != nil {
				t.Fatalf("Set(%q, <non-ascii>) error = %v", key, err)
			}
			got, err := store.Get(key)
			if err != nil {
				t.Fatalf("Get(%q) after Set error = %v", key, err)
			}
			if got != value {
				t.Errorf("Get(%q) = %q, want %q", key, got, value)
			}

			// Set must replace, not append to, an existing credential.
			if err := store.Set(key, overwrite); err != nil {
				t.Fatalf("overwrite Set(%q, ...) error = %v", key, err)
			}
			got2, err := store.Get(key)
			if err != nil {
				t.Fatalf("Get(%q) after overwrite error = %v", key, err)
			}
			if got2 != overwrite {
				t.Errorf("Get(%q) after overwrite = %q, want %q", key, got2, overwrite)
			}

			// Setting an empty value deletes the credential.
			if err := store.Set(key, ""); err != nil {
				t.Fatalf("Set(%q, \"\") error = %v", key, err)
			}
			if _, err := store.Get(key); !errors.Is(err, ErrNotFound) {
				t.Errorf("Get(%q) after empty Set = %v, want ErrNotFound", key, err)
			}

			// Deleting an already-absent credential is not an error.
			if err := store.Set(key, ""); err != nil {
				t.Errorf("Set(%q, \"\") on already-absent credential: error = %v, want nil", key, err)
			}
		})
	}
}
