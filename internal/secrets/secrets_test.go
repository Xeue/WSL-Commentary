package secrets

import (
	"bytes"
	"errors"
	"strings"
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
		{"srt return key", KeySRTReturn, TargetSRTReturn, false},
		{"unknown key", "bogus", "", true},
		{"empty key", "", "", true},
		{"case-sensitive: M2LX is not m2lx", "M2LX", "", true},
		{"case-sensitive: srtReturn is not srtreturn", "srtReturn", "", true},
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
	if TargetSRTReturn != "WSLComms/srtreturn" {
		t.Errorf("TargetSRTReturn = %q, want %q", TargetSRTReturn, "WSLComms/srtreturn")
	}
}

// TestTheTwoSRTPassphrasesAreSeparateCredentials is the property the whole
// third target exists for.
//
// M2L-X sets encryption per OUTPUT: on the measured instance Output 1 (pgm,
// 40501) is unencrypted while Outputs 2 and 3 are encrypted. The send path
// dials the commentary input and the return path dials one of those outputs, so
// one shared credential means that entering the key that makes the monitor work
// silently changes the key the FEED goes out with. If these two ever resolve to
// the same Credential Manager entry, that is exactly what happens, and the
// symptom is a working feed that stops working when somebody fixes the
// headphones.
func TestTheTwoSRTPassphrasesAreSeparateCredentials(t *testing.T) {
	if KeySRT == KeySRTReturn {
		t.Fatalf("KeySRT and KeySRTReturn are both %q; the send and return "+
			"passphrases must be different credentials", KeySRT)
	}

	send, err := targetFor(KeySRT)
	if err != nil {
		t.Fatalf("targetFor(KeySRT) error = %v", err)
	}
	ret, err := targetFor(KeySRTReturn)
	if err != nil {
		t.Fatalf("targetFor(KeySRTReturn) error = %v", err)
	}
	if send == ret {
		t.Fatalf("both SRT passphrases resolve to the Credential Manager target %q; "+
			"setting one would overwrite the other", send)
	}
}

// --- Scoped keys: the instance-preset extension. Pure logic, no vault. ---

func TestTargetFor_ScopedKey(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"wembley/m2lx", "WSLComms/wembley/m2lx"},
		{"wembley/srt", "WSLComms/wembley/srt"},
		{"wembley/srtreturn", "WSLComms/wembley/srtreturn"},
		{"twickenham-2/m2lx", "WSLComms/twickenham-2/m2lx"},
	}
	for _, tt := range tests {
		got, err := targetFor(tt.key)
		if err != nil {
			t.Errorf("targetFor(%q) error = %v", tt.key, err)
			continue
		}
		if got != tt.want {
			t.Errorf("targetFor(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestTargetFor_EmptyScopeIsLegacy(t *testing.T) {
	// The migration guarantee: ScopedKey with the empty scope returns the bare
	// key, and the bare key resolves to the ORIGINAL target — so a machine that
	// never applies a preset never notices any of this exists.
	for _, tt := range []struct {
		base string
		want string
	}{
		{KeyM2LX, TargetM2LX},
		{KeySRT, TargetSRT},
		{KeySRTReturn, TargetSRTReturn},
	} {
		key, err := ScopedKey("", tt.base)
		if err != nil {
			t.Fatalf("ScopedKey(\"\", %q) error = %v", tt.base, err)
		}
		if key != tt.base {
			t.Errorf("ScopedKey(\"\", %q) = %q, want the base unchanged", tt.base, key)
		}
		target, err := targetFor(key)
		if err != nil {
			t.Fatalf("targetFor(%q) error = %v", key, err)
		}
		if target != tt.want {
			t.Errorf("the empty scope must resolve to the legacy target: got %q, want %q", target, tt.want)
		}
	}
}

func TestTargetFor_RejectsBadScope(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"a scope containing a slash — the last-slash split leaves it invalid", "a/b/m2lx"},
		{"an empty scope segment", "/m2lx"},
		{"whitespace in the scope", "wem bley/m2lx"},
		{"uppercase in the scope", "WEMBLEY/m2lx"},
		{"a base that is not one of the three keys", "wembley/nonsense"},
		{"an empty base", "wembley/"},
		{"a scope longer than DeriveID can produce", strings.Repeat("a", 49) + "/m2lx"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := targetFor(tt.key); err == nil {
				t.Fatalf("targetFor(%q) = %q, want an error", tt.key, got)
			} else if !errors.Is(err, ErrUnknownKey) {
				t.Errorf("targetFor(%q) error = %v, want ErrUnknownKey", tt.key, err)
			}
		})
	}
}

func TestScopedKey_RejectsWhatTargetForWould(t *testing.T) {
	// ScopedKey is the constructor; it must refuse everything targetFor
	// refuses, so a bad scope fails where it is BUILT rather than where it is
	// first used — which may be the control-plane goroutine mid-match.
	for _, tt := range []struct{ scope, base string }{
		{"a/b", KeyM2LX},
		{"WEMBLEY", KeyM2LX},
		{"wem bley", KeyM2LX},
		{strings.Repeat("a", 49), KeyM2LX},
		{"wembley", "nonsense"},
		{"wembley", ""},
	} {
		if got, err := ScopedKey(tt.scope, tt.base); err == nil {
			t.Errorf("ScopedKey(%q, %q) = %q, want an error", tt.scope, tt.base, got)
		}
	}
}

// TestScopedTargetsDoNotCollide is the test that matters most here: two
// presets — or a preset and a legacy entry — sharing one Credential Manager
// target means entering one instance's password overwrites another's, which is
// the same class of failure the third credential (KeySRTReturn) was created to
// remove.
func TestScopedTargetsDoNotCollide(t *testing.T) {
	// The adversarial pair: a preset NAMED "srt" must not land its M2L-X
	// password on the legacy send-passphrase target.
	key, err := ScopedKey("srt", KeyM2LX)
	if err != nil {
		t.Fatalf("ScopedKey(\"srt\", KeyM2LX) error = %v", err)
	}
	target, err := targetFor(key)
	if err != nil {
		t.Fatalf("targetFor(%q) error = %v", key, err)
	}
	if target == TargetSRT {
		t.Fatalf("scope %q + base %q produced the LEGACY target %q; entering that preset's "+
			"password would overwrite the machine's send passphrase", "srt", KeyM2LX, TargetSRT)
	}

	// Exhaustively: every scoped and legacy target across two scopes and the
	// three bases is distinct from every other.
	seen := map[string]string{
		TargetM2LX:      "legacy m2lx",
		TargetSRT:       "legacy srt",
		TargetSRTReturn: "legacy srtreturn",
	}
	for _, scope := range []string{"srt", "m2lx", "srtreturn", "wembley"} {
		for _, base := range []string{KeyM2LX, KeySRT, KeySRTReturn} {
			k, err := ScopedKey(scope, base)
			if err != nil {
				t.Fatalf("ScopedKey(%q, %q) error = %v", scope, base, err)
			}
			tgt, err := targetFor(k)
			if err != nil {
				t.Fatalf("targetFor(%q) error = %v", k, err)
			}
			who := scope + "/" + base
			if prev, dup := seen[tgt]; dup {
				t.Errorf("credential target %q is shared by %s and %s; one password would overwrite the other",
					tgt, prev, who)
			}
			seen[tgt] = who
		}
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
// `go test`, using the exact targets ("WSLComms/m2lx", "WSLComms/srt",
// "WSLComms/srtreturn") the production application uses. To avoid destroying a
// real stored secret if
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

	for _, key := range []string{KeyM2LX, KeySRT, KeySRTReturn} {
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

// TestSRTAndSRTReturnDoNotShareAVaultEntry is the live half of
// TestTheTwoSRTPassphrasesAreSeparateCredentials: the constants differing is
// necessary but not sufficient, because a mistake in targetFor or in
// Credential Manager's own name matching would still land both on one entry.
// This writes two different values through the real vault and reads both back.
func TestSRTAndSRTReturnDoNotShareAVaultEntry(t *testing.T) {
	store := New()

	const sendValue = "send-path-key-ößü-𝄞"
	const returnValue = "return-path-key-完全に-別"

	for _, key := range []string{KeySRT, KeySRTReturn} {
		target, err := targetFor(key)
		if err != nil {
			t.Fatalf("targetFor(%q) error = %v", key, err)
		}
		backup, existed := backupCredential(t, target)
		t.Cleanup(func() { restoreCredential(t, target, backup, existed) })
		deleteCredential(t, target)
	}

	if err := store.Set(KeySRT, sendValue); err != nil {
		t.Fatalf("Set(KeySRT, ...) error = %v", err)
	}
	if err := store.Set(KeySRTReturn, returnValue); err != nil {
		t.Fatalf("Set(KeySRTReturn, ...) error = %v", err)
	}

	gotSend, err := store.Get(KeySRT)
	if err != nil {
		t.Fatalf("Get(KeySRT) error = %v", err)
	}
	gotReturn, err := store.Get(KeySRTReturn)
	if err != nil {
		t.Fatalf("Get(KeySRTReturn) error = %v", err)
	}
	if gotSend != sendValue {
		t.Errorf("Get(KeySRT) = %q, want %q — the return passphrase overwrote the send one",
			gotSend, sendValue)
	}
	if gotReturn != returnValue {
		t.Errorf("Get(KeySRTReturn) = %q, want %q", gotReturn, returnValue)
	}

	// And clearing one must leave the other alone. Clearing the RETURN
	// passphrase because an output turned out to be unencrypted must not take
	// the contribution feed's key with it.
	if err := store.Set(KeySRTReturn, ""); err != nil {
		t.Fatalf("Set(KeySRTReturn, \"\") error = %v", err)
	}
	if _, err := store.Get(KeySRTReturn); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(KeySRTReturn) after clearing = %v, want ErrNotFound", err)
	}
	stillSend, err := store.Get(KeySRT)
	if err != nil {
		t.Fatalf("Get(KeySRT) after clearing the return passphrase error = %v", err)
	}
	if stillSend != sendValue {
		t.Errorf("Get(KeySRT) after clearing the return passphrase = %q, want %q",
			stillSend, sendValue)
	}
}
