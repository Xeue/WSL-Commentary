package remote

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// Capabilities
// ---------------------------------------------------------------------------

// Capability is one tier of what a client is allowed to do. The tiers are
// ORDERED and inclusive: a client granted a higher tier is implicitly granted
// every lower one. This is the only capability vocabulary the transport knows;
// the mapping from a specific bound method to the tier it needs is the
// application's policy (app_remote.go), not this package's, which is what keeps
// internal/remote App-agnostic.
type Capability string

const (
	// CapView is read-only: watch the desk, the lamps, the config. It is the
	// floor — every authenticated client has at least this.
	CapView Capability = "view"
	// CapOperate adds configuration and session control (Start/Stop, SaveConfig,
	// SetSecret, presets). It does NOT include writing to the live mixer.
	CapOperate Capability = "operate"
	// CapMixer adds the arm-gated write path to the live broadcast desk
	// (SendMixerCommands, SetMixerGolden). It is off by default because it is
	// the one capability that can change what goes to air.
	CapMixer Capability = "mixer"
)

// capRank orders the tiers so Allows can implement inclusion with a single
// comparison. A capability absent from this map is unknown and grants nothing.
var capRank = map[Capability]int{
	CapView:    1,
	CapOperate: 2,
	CapMixer:   3,
}

// Allows reports whether a client holding the granted capabilities may exercise
// required, honouring tier inclusion: holding "mixer" satisfies a requirement
// of "operate" or "view", holding "operate" satisfies "view", and holding
// nothing satisfies nothing. An unknown string in granted is ignored (it ranks
// zero), so a typo in remote.json can only ever REMOVE authority, never add it.
//
// This is the single chokepoint both the fake dispatcher (tests) and the real
// dispatcher (app_remote.go) run every capability decision through, so the tier
// semantics are defined and tested in exactly one place.
func Allows(granted []string, required Capability) bool {
	need := capRank[required]
	if need == 0 {
		// A requirement this package does not recognise is refused rather than
		// defaulted open: an unrecognised gate is a bug, and a bug on the write
		// path to a live desk must fail closed.
		return false
	}
	best := 0
	for _, g := range granted {
		if r := capRank[Capability(g)]; r > best {
			best = r
		}
	}
	return best >= need
}

// validCapability reports whether s names a capability this package knows.
// remote.json is validated with it so an operator cannot save a client record
// carrying a capability nothing will ever honour and be misled into thinking it
// granted something.
func validCapability(s string) bool { return capRank[Capability(s)] != 0 }

// ---------------------------------------------------------------------------
// PBKDF2 password hashing
// ---------------------------------------------------------------------------

const (
	// pbkdf2Iter is the PBKDF2-HMAC-SHA256 iteration count. It is high on
	// purpose: this hash guards a control channel to a live broadcast desk, the
	// verification happens at most once per login (not per request), and a
	// login taking a few milliseconds longer is invisible to an operator but
	// multiplies an offline attacker's cost. If this number is ever raised,
	// existing stored hashes keep verifying because the iteration count is
	// stored WITH each hash — see PBKDF2Params.Iter.
	pbkdf2Iter = 600000
	// pbkdf2SaltLen is the per-password random salt length in bytes. 16 bytes
	// (128 bits) makes a precomputed-table attack across clients pointless.
	pbkdf2SaltLen = 16
	// pbkdf2KeyLen is the derived key length in bytes, matching SHA-256's
	// natural 32-byte output so no truncation loses entropy.
	pbkdf2KeyLen = 32
)

// PBKDF2Params is a stored password verifier: the salt, the iteration count and
// the derived key, all that is needed to check a password and nothing that can
// reproduce it. It is the on-disk shape under a client's "pbkdf2" key.
//
// The iteration count is stored alongside the hash deliberately: it lets
// pbkdf2Iter be raised later without invalidating every existing client, and it
// makes each record self-describing rather than dependent on a constant that
// might have changed since it was written.
type PBKDF2Params struct {
	// Salt is the base64-encoded per-password random salt.
	Salt string `json:"salt"`
	// Iter is the iteration count this hash was computed with.
	Iter int `json:"iter"`
	// Hash is the base64-encoded derived key.
	Hash string `json:"hash"`
}

// hashPassword derives a fresh verifier for pw with a new random salt.
//
// crypto/rand failing is treated as fatal to the operation rather than papered
// over with a weaker salt: a password verifier built on predictable salt is
// worse than no verifier, because it looks like protection and is not.
func hashPassword(pw string) (PBKDF2Params, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return PBKDF2Params{}, fmt.Errorf("remote: generating salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, pbkdf2KeyLen)
	if err != nil {
		return PBKDF2Params{}, fmt.Errorf("remote: deriving key: %w", err)
	}
	return PBKDF2Params{
		Salt: base64.StdEncoding.EncodeToString(salt),
		Iter: pbkdf2Iter,
		Hash: base64.StdEncoding.EncodeToString(key),
	}, nil
}

// verify reports whether pw reproduces this stored verifier, in constant time.
//
// The comparison uses crypto/subtle so the answer's timing does not leak how
// many leading bytes matched — an information channel that would otherwise let
// an attacker recover the hash byte by byte. A malformed stored record (bad
// base64, zero iterations) verifies as false rather than erroring, because from
// the caller's point of view "this password does not match this record" is the
// correct and safe outcome either way.
func (p PBKDF2Params) verify(pw string) bool {
	salt, err := base64.StdEncoding.DecodeString(p.Salt)
	if err != nil || p.Iter <= 0 {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(p.Hash)
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, p.Iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---------------------------------------------------------------------------
// The client store / settings document
// ---------------------------------------------------------------------------

// settingsFileName is remote.json, kept in its OWN directory under %APPDATA%,
// deliberately separate from config.json. The frontend's collectConfig()
// rewrites the entire config document from a page-level cache and silently
// deletes any field it does not restate (pictureSource is already lost this
// way). A network listener's bind address and its client credentials must not
// be reachable by that mechanism, so they live in a file the frontend never
// writes as a whole.
const settingsFileName = "remote.json"

// remoteDirName is the subdirectory under the WSLComms app-data folder that
// holds remote.json and the self-signed TLS key/cert. Keeping the cert next to
// the settings that name the bind it covers means the two cannot drift into
// different directories.
const remoteDirName = "remote"

// appDataDirName mirrors internal/config's constant. It is duplicated rather
// than imported because internal/remote must not depend on the root or on
// internal/config to stay independently testable at Gate A; the two constants
// naming the same folder are asserted equal by a guard test.
const appDataDirName = "WSLComms"

// defaultPort is the TLS listener port when remote.json does not state one.
const defaultPort = 8443

// defaultBind is the loopback default. The listener is not merely off by
// default; when it IS enabled it still binds only 127.0.0.1 unless an operator
// deliberately widens it, and widening it to a routable address is refused
// unless at least one client exists to authenticate.
const defaultBind = "127.0.0.1"

// settingsVersion is the schema version written into new files, so a future
// format change can migrate rather than misread an old document.
const settingsVersion = 1

// Client is one named remote seat: a login name, a password verifier and the
// capabilities it is granted. The password itself is never stored and cannot be
// read back — only PBKDF2Params, from which a password cannot be recovered.
type Client struct {
	// Name is the login identifier, unique within the store.
	Name string `json:"name"`
	// PBKDF2 is the password verifier. It may be zero for a client whose
	// password has not been set yet, in which case that client cannot log in.
	PBKDF2 PBKDF2Params `json:"pbkdf2"`
	// Caps are the capabilities granted to this client. An empty list means
	// view-only is NOT implied — the client can authenticate but is granted
	// nothing, which Allows treats as refuse-everything. A client meant to watch
	// must be granted CapView explicitly.
	Caps []string `json:"caps"`
}

// Settings is the whole of remote.json: whether the listener runs, where it
// binds, and who may connect. enabled defaults false and bind defaults to
// loopback, so the safe posture is the one you get by doing nothing.
type Settings struct {
	// Version is the on-disk schema version.
	Version int `json:"version"`
	// Enabled gates the listener. False — the default, and the value on a
	// missing file — means Start binds no socket at all.
	Enabled bool `json:"enabled"`
	// Bind is the literal IP the listener binds. It must be an IP, never a
	// hostname, so that what is exposed cannot change under DNS.
	Bind string `json:"bind"`
	// Port is the TLS listener port.
	Port int `json:"port"`
	// Clients are the authorised seats. An empty list on a loopback bind is
	// fine (a developer testing locally); an empty list on a routable bind is
	// refused by Validate, because a reachable listener nobody can log in to is
	// pure attack surface.
	Clients []Client `json:"clients"`
}

// DefaultSettings returns the safe posture: off, loopback, default port, no
// clients. It is what LoadSettings returns for a missing file, so first run is
// not an error and a fresh machine is not accidentally listening.
func DefaultSettings() *Settings {
	return &Settings{
		Version: settingsVersion,
		Enabled: false,
		Bind:    defaultBind,
		Port:    defaultPort,
	}
}

// RemoteDir returns %APPDATA%\WSLComms\remote, the directory this package owns.
// It does not create it. os.UserConfigDir resolves %APPDATA% on Windows, and is
// what lets tests redirect the whole tree by setting the APPDATA environment
// variable — the same mechanism internal/config relies on.
func RemoteDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("remote: resolving user config directory: %w", err)
	}
	return filepath.Join(dir, appDataDirName, remoteDirName), nil
}

// SettingsPath returns the absolute path of remote.json. It does not create the
// directory.
func SettingsPath() (string, error) {
	dir, err := RemoteDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, settingsFileName), nil
}

// LoadSettings reads remote.json, returning DefaultSettings() and a nil error
// when the file does not exist so that a machine that has never enabled remote
// access is not an error condition. Fields absent from an existing file take
// their defaults, because the document is unmarshalled onto a DefaultSettings()
// struct — the same technique internal/config uses so an older or hand-edited
// file does not silently zero a field (a missing "port" becoming 0 would bind
// an ephemeral port nobody could find).
func LoadSettings() (*Settings, error) {
	path, err := SettingsPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultSettings(), nil
		}
		return nil, fmt.Errorf("remote: reading %s: %w", path, err)
	}
	s := DefaultSettings()
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("remote: parsing %s: %w", path, err)
	}
	return s, nil
}

// Save writes remote.json atomically, with the SAME discipline as
// internal/config.(*Config).Save: MkdirAll 0700, a temp file created in the
// same directory (so the rename is a same-volume metadata operation, not a
// copy), Write, Sync, Close, os.Rename. A reader — including this process
// crashing mid-write — always observes either the whole old file or the whole
// new one, never a half-written listener configuration.
//
// The 0700 directory mode matters more here than for config.json: this folder
// holds the TLS private key and the password verifiers, and must not be
// world-readable on a shared machine.
func (s *Settings) Save() error {
	path, err := SettingsPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("remote: creating %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("remote: encoding settings: %w", err)
	}
	tmp, err := os.CreateTemp(dir, settingsFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("remote: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("remote: writing %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("remote: syncing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("remote: closing %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("remote: renaming %s to %s: %w", tmpPath, path, err)
	}
	renamed = true
	return nil
}

// Validate reports the first reason these settings are not safe to bind, or nil.
//
// The two rules that are load-bearing rather than cosmetic:
//
//   - Bind must be a LITERAL IP. A hostname would let what is exposed change
//     under DNS between the moment an operator reads the address in Settings and
//     the moment the socket binds — the exposed surface must be exactly what the
//     operator typed.
//
//   - A non-loopback bind with zero clients is REFUSED. A listener reachable
//     from the LAN that nobody can authenticate to is not a convenience waiting
//     for its first client; it is unauthenticated attack surface on a machine
//     that is on air. Loopback with zero clients is allowed because it is
//     reachable only from the same machine — a developer's own browser.
func (s *Settings) Validate() error {
	ip := net.ParseIP(strings.TrimSpace(s.Bind))
	if ip == nil {
		return fmt.Errorf("remote: bind %q is not a literal IP address", s.Bind)
	}
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("remote: port %d out of range", s.Port)
	}
	if !ip.IsLoopback() && len(s.Clients) == 0 {
		return fmt.Errorf("remote: refusing non-loopback bind %s with no clients configured", s.Bind)
	}
	for _, c := range s.Clients {
		if strings.TrimSpace(c.Name) == "" {
			return errors.New("remote: a client record has an empty name")
		}
		for _, capName := range c.Caps {
			if !validCapability(capName) {
				return fmt.Errorf("remote: client %q has unknown capability %q", c.Name, capName)
			}
		}
	}
	return nil
}

// findClient returns a copy of the named client and whether it was found.
//
// A copy, not a pointer: callers that verify a password must not be able to
// mutate the store through the value they were handed, and the value crosses
// into the authenticator where holding a live pointer into a settings document
// that may be reloaded would be a lifetime hazard.
func (s *Settings) findClient(name string) (Client, bool) {
	for _, c := range s.Clients {
		if c.Name == name {
			return c, true
		}
	}
	return Client{}, false
}

// AddClient appends a new client with the given capabilities and no password
// set. It returns an error if the name is empty or already taken. A password
// must be set separately with SetClientPassword before the client can log in,
// mirroring the "set / not set" secret-badge convention the Settings UI already
// uses for the M2L-X and SRT passphrases.
func (s *Settings) AddClient(name string, caps []string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("remote: client name must not be empty")
	}
	if _, ok := s.findClient(name); ok {
		return fmt.Errorf("remote: client %q already exists", name)
	}
	for _, capName := range caps {
		if !validCapability(capName) {
			return fmt.Errorf("remote: unknown capability %q", capName)
		}
	}
	s.Clients = append(s.Clients, Client{Name: name, Caps: append([]string(nil), caps...)})
	return nil
}

// SetClientPassword replaces the named client's password verifier with a fresh
// PBKDF2 hash of pw. The plaintext is used only to derive the hash and is never
// stored; there is deliberately no method that returns a password, because a
// verifier from which the password could be recovered would defeat the point of
// hashing it.
func (s *Settings) SetClientPassword(name, pw string) error {
	for i := range s.Clients {
		if s.Clients[i].Name == name {
			p, err := hashPassword(pw)
			if err != nil {
				return err
			}
			s.Clients[i].PBKDF2 = p
			return nil
		}
	}
	return fmt.Errorf("remote: no client named %q", name)
}

// SetClientCaps replaces the named client's capability list after validating it.
func (s *Settings) SetClientCaps(name string, caps []string) error {
	for _, capName := range caps {
		if !validCapability(capName) {
			return fmt.Errorf("remote: unknown capability %q", capName)
		}
	}
	for i := range s.Clients {
		if s.Clients[i].Name == name {
			s.Clients[i].Caps = append([]string(nil), caps...)
			return nil
		}
	}
	return fmt.Errorf("remote: no client named %q", name)
}

// DeleteClient removes the named client. Removing a client does not by itself
// revoke that client's live sessions; the caller restarts the listener (which
// revokes all sessions) after changing the store, because sessions are held in
// the authenticator, not in this document.
func (s *Settings) DeleteClient(name string) error {
	for i := range s.Clients {
		if s.Clients[i].Name == name {
			s.Clients = append(s.Clients[:i], s.Clients[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("remote: no client named %q", name)
}
