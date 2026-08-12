package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// The listener's on-disk settings
// ---------------------------------------------------------------------------
//
// # This is an UNAUTHENTICATED listener, by the owner's explicit decision
//
// Everything about the posture below — ON by default, bound to ALL interfaces
// (0.0.0.0), serving BOTH plain HTTP and HTTPS, with NO login, NO client
// accounts, NO origin/CSRF check and NO connection guard of any kind — is the
// owner's repeated, final decision, not an oversight. The app runs in a secure
// remote facility on a DEDICATED PRIVATE NETWORK, and the NETWORK is the access
// control. The risk write-up lives in docs/remote-access.md; do not reintroduce
// any guard here that the owner did not ask for, and do not surface a warning
// about this in the app UI (the Settings section shows the bound-port status and
// nothing else).
//
// Because there is nothing to authenticate, there are no client records and no
// password verifiers on disk any more; remote.json carries only whether the
// listener runs, the literal IP it binds, and the two ports.

// settingsFileName is remote.json, kept in its OWN directory under %APPDATA%,
// deliberately separate from config.json. The frontend's collectConfig()
// rewrites the entire config document from a page-level cache and silently
// deletes any field it does not restate (pictureSource is already lost this
// way). A network listener's bind address must not be reachable by that
// mechanism, so it lives in a file the frontend never writes as a whole.
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

// defaultBind is the wildcard: the listener binds ALL interfaces so a browser on
// any LAN address of the commentary PC can reach it. This is the default, not an
// opt-in — see the file header for why the private network, not a bind guard, is
// the access control.
const defaultBind = "0.0.0.0"

// defaultHTTPPort / defaultHTTPSPort are the privileged well-known ports the
// listener prefers so an operator can type a bare host with no :port. On Windows
// these are frequently held by http.sys (WinRM, the print spooler, IIS), so each
// has a high fallback the listener drops to automatically when the primary is
// busy — see remote.go's Start.
const (
	defaultHTTPPort  = 80
	defaultHTTPSPort = 443

	// httpFallbackPort / httpsFallbackPort are the ports Start binds when the
	// well-known primaries are already taken. 8080/8443 are the conventional
	// unprivileged mirrors of 80/443.
	httpFallbackPort  = 8080
	httpsFallbackPort = 8443
)

// settingsVersion is the schema version written into new files, so a future
// format change can migrate rather than misread an old document.
const settingsVersion = 1

// Settings is the whole of remote.json: whether the listener runs, the literal
// IP it binds, and the two ports it serves. There are no client records — the
// listener is unauthenticated by the owner's decision (see the file header).
type Settings struct {
	// Version is the on-disk schema version.
	Version int `json:"version"`
	// Enabled gates the listener. It defaults TRUE — the listener is on out of
	// the box, and the value on a MISSING file is also true — because the owner
	// wants a second seat reachable on the facility network without a setup step.
	Enabled bool `json:"enabled"`
	// Bind is the literal IP the listener binds. It must be an IP, never a
	// hostname, so that what is exposed cannot change under DNS. "0.0.0.0" (the
	// default) is a valid, deliberate value: bind every interface.
	Bind string `json:"bind"`
	// HTTPPort is the plain-HTTP port. Start tries this, then httpFallbackPort.
	HTTPPort int `json:"httpPort"`
	// HTTPSPort is the TLS port. Start tries this, then httpsFallbackPort.
	HTTPSPort int `json:"httpsPort"`
}

// DefaultSettings returns the open posture: ON, all interfaces, 80/443. It is
// what LoadSettings returns for a missing file, so a fresh machine on the
// facility network is listening out of the box, which is what the owner wants.
func DefaultSettings() *Settings {
	return &Settings{
		Version:   settingsVersion,
		Enabled:   true,
		Bind:      defaultBind,
		HTTPPort:  defaultHTTPPort,
		HTTPSPort: defaultHTTPSPort,
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
// when the file does not exist so that first run is not an error condition.
// Fields absent from an existing file take their defaults, because the document
// is unmarshalled onto a DefaultSettings() struct — the same technique
// internal/config uses so an older or hand-edited file does not silently zero a
// field.
//
// An OLD remote.json from the authenticated era is tolerated rather than
// rejected: its removed keys ("clients", "port") are ignored, its single "port"
// is migrated onto HTTPSPort (the old file was TLS-only), and one line is logged
// so the migration is visible. This keeps a machine that was configured under
// the previous scheme working after an upgrade instead of failing to parse.
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
	migrateLegacy(data, s)
	return s, nil
}

// migrateLegacy folds an old authenticated-era remote.json onto the new schema.
// The old file had a single TLS "port" and a "clients" array; the new one has no
// clients and two ports. A present legacy "port" becomes HTTPSPort (the old
// listener was HTTPS-only), and the presence of either legacy key is logged
// once. json.Unmarshal already ignores the keys onto the new struct; this only
// adds the port migration and the log line.
func migrateLegacy(data []byte, s *Settings) {
	var legacy struct {
		Port    *int              `json:"port"`
		Clients []json.RawMessage `json:"clients"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return
	}
	if legacy.Port == nil && legacy.Clients == nil {
		return
	}
	if legacy.Port != nil {
		s.HTTPSPort = *legacy.Port
	}
	log.Printf("remote: migrated a legacy remote.json (the listener is now unauthenticated by "+
		"owner's decision): dropped %d client record(s); mapped the old TLS port %v onto httpsPort %d",
		len(legacy.Clients), legacyPortString(legacy.Port), s.HTTPSPort)
}

// legacyPortString renders the optional legacy port for the migration log,
// printing "(absent)" rather than a nil pointer when the old file had no port.
func legacyPortString(p *int) string {
	if p == nil {
		return "(absent)"
	}
	return fmt.Sprintf("%d", *p)
}

// Save writes remote.json atomically, with the SAME discipline as
// internal/config.(*Config).Save: MkdirAll 0700, a temp file created in the
// same directory (so the rename is a same-volume metadata operation, not a
// copy), Write, Sync, Close, os.Rename. A reader — including this process
// crashing mid-write — always observes either the whole old file or the whole
// new one, never a half-written listener configuration.
//
// The 0700 directory mode matters because this folder also holds the TLS
// private key, which must not be world-readable on a shared machine.
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
// Only two rules remain, both structural rather than security:
//
//   - Bind must be a LITERAL IP. "0.0.0.0" is allowed and is the default; a
//     hostname is refused, because what is exposed cannot be allowed to change
//     under DNS between the moment an operator reads the address and the moment
//     the socket binds.
//   - Each port must be in range. 0 is permitted and means "let the OS assign
//     one" (used by tests); otherwise 1..65535.
//
// The old "refuse a non-loopback bind with no clients" rule is GONE: there are
// no clients, and the owner's decision is that the private network is the access
// control, so a wildcard bind is the intended default, not a condition to
// refuse.
func (s *Settings) Validate() error {
	if net.ParseIP(strings.TrimSpace(s.Bind)) == nil {
		return fmt.Errorf("remote: bind %q is not a literal IP address", s.Bind)
	}
	if err := validatePort("httpPort", s.HTTPPort); err != nil {
		return err
	}
	if err := validatePort("httpsPort", s.HTTPSPort); err != nil {
		return err
	}
	return nil
}

// validatePort accepts 0 (OS-assigned, for tests) or 1..65535, naming the field
// so a bad value in remote.json points the operator at the right key.
func validatePort(name string, p int) error {
	if p < 0 || p > 65535 {
		return fmt.Errorf("remote: %s %d out of range (0 for OS-assigned, else 1..65535)", name, p)
	}
	return nil
}
