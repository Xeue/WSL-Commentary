package secrets

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

// Platform-neutral half of the package's tests: the key names, the target
// names, ScopedKey and targetFor. None of it touches a credential vault, so it
// runs identically on Windows and macOS. The Windows-only half — the UTF-16LE
// blob encoding and the live Credential Manager round-trip — is in
// secrets_windows_test.go.
//
// --- Pure-logic table-driven tests: no vault access. ---

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

// TestStoreNameNamesThisPlatformsStore pins the phrase five operator-facing
// error messages are built from.
//
// It asserts the SHAPE rather than the exact wording, deliberately. The wording
// may want improving; what may not change is that it is non-empty, that it does
// not name the OTHER platform's facility, and that it reads as the object of a
// preposition, because every call site says "stored in %s under %q". A store
// name that arrives with a trailing full stop or a capital that starts a
// sentence turns five error messages into nonsense at once, and no call site
// would notice.
func TestStoreNameNamesThisPlatformsStore(t *testing.T) {
	name := StoreName()
	if name == "" {
		t.Fatal("StoreName() is empty: five error messages would tell the operator to look " +
			"in nothing at all for their password")
	}
	if strings.TrimSpace(name) != name {
		t.Errorf("StoreName() = %q has surrounding whitespace; it is interpolated into "+
			`"stored in %%s under %%q"`, name)
	}
	if strings.HasSuffix(name, ".") {
		t.Errorf("StoreName() = %q ends in a full stop; it is a noun phrase in the middle of a "+
			"sentence, not a sentence", name)
	}

	// The half that actually catches the bug this exists for: a copy-paste
	// between the two platform files. Whichever build this is, exactly one of
	// the two facilities may be named.
	windows := strings.Contains(name, "Credential Manager")
	macOS := strings.Contains(name, "Keychain")
	if windows == macOS {
		t.Fatalf("StoreName() = %q names neither exactly one of Credential Manager and "+
			"Keychain. On %s it must name that platform's store and no other: an operator "+
			"sent to the wrong control panel is worse off than one sent nowhere",
			name, runtime.GOOS)
	}
	switch runtime.GOOS {
	case "windows":
		if !windows {
			t.Errorf("StoreName() = %q on windows; want Windows Credential Manager", name)
		}
	case "darwin":
		if !macOS {
			t.Errorf("StoreName() = %q on darwin; want the macOS login Keychain", name)
		}
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
