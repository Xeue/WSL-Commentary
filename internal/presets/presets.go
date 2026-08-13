package presets

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"wslcomms/internal/config"
)

// Version is the only preset file version this build reads or writes. Load
// refuses any other BY NAME rather than silently ignoring keys it does not
// recognise: a version-2 file read by a version-1 build could carry a renamed
// key that this build's Filter would drop in silence, and "your preset half
// applied" is a worse outcome than "this preset is from a newer build".
const Version = 1

// DirName is the folder created under %APPDATA%\WSLComms for preset files.
const DirName = "presets"

// activeFileName holds the active-preset record inside Dir(). It is not a
// preset — see ActiveRecord — and List skips it by name.
const activeFileName = "active.json"

// activeID is the id that would collide with activeFileName, and is therefore
// the one id this package reserves for itself. validateID refuses it.
const activeID = "active"

// maxIDLen bounds a derived id. The id becomes both a filename and a
// Credential Manager target segment; 48 keeps the longest target
// (WSLComms/<id>/srtreturn) comfortably inside Credential Manager's limits
// and inside anything a picker can render.
const maxIDLen = 48

// Preset is one M2L-X deployment's coordinates: the on-disk file format.
//
// One file per preset, not one presets.json: each write is independently
// atomic, so a crash saving "Wembley" cannot corrupt "Twickenham";
// export/import is "copy this file", which is the point of the feature; and it
// is hand-editable the way config.json already is — which is why Filter treats
// unknown keys as something to neuter and report rather than trust.
type Preset struct {
	// Version is the file format version; see the constant above.
	Version int `json:"version"`

	// ID is the preset's identity: derived once from the first name it was
	// saved under (DeriveID), and never changed afterwards — not by Rename, not
	// by anything. It is BOTH the filename under %APPDATA% and (usually) the
	// Credential Manager scope segment, which is why DeriveID is the one
	// function in this feature where a naming mistake is a security bug.
	ID string `json:"id"`

	// Name is the display name, the only thing Rename may change.
	Name string `json:"name"`

	// CredentialScope names the Credential Manager scope this preset's three
	// secrets live under: "" for the legacy WSLComms/{m2lx,srt,srtreturn}
	// entries (the migration preset keeps the machine's existing passwords),
	// or the preset's id for WSLComms/<scope>/{m2lx,srt,srtreturn}.
	//
	// It is stored rather than derived from ID so that the migration preset —
	// whose scope is "" while its ID is not — is representable, and so that
	// Rename provably cannot orphan credentials: the scope is a recorded fact,
	// not a recomputation.
	CredentialScope string `json:"credentialScope"`

	// SavedAt is when this file was last written.
	SavedAt time.Time `json:"savedAt"`

	// Fields holds the whitelisted configuration values under their EXACT
	// config.json tags — see InstanceFields. RawMessage rather than a struct so
	// that "absent" and "zero" stay distinguishable: a preset that never
	// mentions srtLatencyMs must not blank an operator's value on apply.
	Fields map[string]json.RawMessage `json:"fields"`
}

// Summary is what ListPresets hands the frontend: everything a picker and a
// confirm dialog need. Fields is included — the confirm dialog diffs the
// preset against the live config before anything is applied, and there is
// deliberately no separate GetPreset binding to fetch it with.
type Summary struct {
	ID              string                     `json:"id"`
	Name            string                     `json:"name"`
	CredentialScope string                     `json:"credentialScope"`
	SavedAt         time.Time                  `json:"savedAt"`
	Fields          map[string]json.RawMessage `json:"fields"`
}

// Summary converts a stored preset to its wire shape.
func (p Preset) Summary() Summary {
	return Summary{
		ID:              p.ID,
		Name:            p.Name,
		CredentialScope: p.CredentialScope,
		SavedAt:         p.SavedAt,
		Fields:          p.Fields,
	}
}

// ActiveRecord says which preset this PC is pointed at right now, and which
// credential scope its secrets are being read from. It lives in its own file
// (ActivePath) beside the presets.
//
// "Which instance this PC points at" is MACHINE state and must never be inside
// a preset body — and it is deliberately NOT a field of config.Config either:
// adding one would make it a fourth thing settings.js's collectConfig() has to
// restate or silently delete, which is the exact trap the frontend-only
// pictureSource key is already caught in (it has no Go field, so every Save
// discards it).
type ActiveRecord struct {
	ID              string    `json:"id"`
	CredentialScope string    `json:"credentialScope"`
	AppliedAt       time.Time `json:"appliedAt"`
}

// ErrUnknownVersion is wrapped by Load when a preset file's version is not
// Version. Callers can errors.Is it; the wrapping message names the version
// found and the version wanted.
var ErrUnknownVersion = errors.New("presets: unknown preset file version")

// ErrNotFound is returned by Load and Delete when no preset file exists for
// the id.
var ErrNotFound = errors.New("presets: preset not found")

// Dir returns the presets directory, %APPDATA%\WSLComms\presets. It does not
// create it.
//
// os.UserConfigDir resolves %APPDATA% on Windows, mirroring config.Path(),
// and for the same reason: tests point APPDATA at a temp directory and the
// whole package becomes exercisable without touching the developer's real
// profile.
func Dir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("presets: resolving user config directory: %w", err)
	}
	return filepath.Join(dir, config.AppDataDirName, DirName), nil
}

// PathFor returns the file path for a preset id, refusing an id that fails
// validation. The refusal is not redundant with DeriveID: Load, Delete and
// Rename take an id from across the Wails boundary, and validating here is
// what makes "presets/<whatever JS sent>" incapable of being a traversal even
// if every caller forgot.
func PathFor(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".json"), nil
}

// ActivePath returns the active-preset record's path,
// %APPDATA%\WSLComms\presets\active.json.
func ActivePath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, activeFileName), nil
}

// reservedDeviceNames are the Windows filenames that cannot be created on
// NTFS whatever the extension: CON.json is still the console. Checked
// case-insensitively against the part of a name before its first dot.
var reservedDeviceNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// DeriveID turns a display name into a preset id: lower-case ASCII
// alphanumerics and hyphens only, runs collapsed, trimmed, 1 to maxIDLen
// characters.
//
// This is the single most security-sensitive function in the feature, because
// the id becomes BOTH a filename under %APPDATA% and a Credential Manager
// target segment. So it rejects rather than repairs anything that could be
// either attack or accident:
//
//   - "" and whitespace-only, "." and ".." — a dot name is a path traversal
//     out of the data directory;
//   - anything containing a path separator or a colon — a "/" surviving into
//     the credential scope would let one preset's target collide with
//     another's (WSLComms/<scope>/<key> parses at the LAST slash), and a colon
//     is an NTFS alternate data stream;
//   - the Windows reserved device names, with or without an extension — a file
//     that cannot be created on NTFS;
//   - a result longer than maxIDLen, rather than truncating: two long names
//     differing only past the cut would silently become one preset.
//
// The rejections are checked against the RAW name, not the sanitised result,
// because sanitisation maps "nul.json" to the innocuous "nul-json" — and a
// name the operator typed as a reserved device name or a path is a mistake to
// tell them about, not to quietly rewrite into something else.
//
// Case-insensitive collision with an existing preset is checked by the caller
// (App.SavePreset), which has the list in hand; ids are already lower-case, so
// "same id" IS the case-insensitive collision.
func DeriveID(name string) (string, error) {
	raw := strings.TrimSpace(name)
	if raw == "" || raw == "." || raw == ".." {
		return "", fmt.Errorf("presets: %q cannot name a preset", name)
	}
	if strings.ContainsAny(raw, "/\\:") {
		return "", fmt.Errorf("presets: a preset name must not contain a path separator or a colon: %q", name)
	}
	base := raw
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if reservedDeviceNames[strings.ToUpper(base)] {
		return "", fmt.Errorf(
			"presets: %q is a Windows reserved device name and cannot be a file: choose another name", name)
	}

	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen && b.Len() > 0 {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	id := strings.Trim(b.String(), "-")
	if id == "" {
		return "", fmt.Errorf("presets: %q contains no usable characters for a preset id", name)
	}
	if len(id) > maxIDLen {
		return "", fmt.Errorf(
			"presets: the name %q derives an id %d characters long; the limit is %d — shorten the name",
			name, len(id), maxIDLen)
	}
	// Everything DeriveID produces must also be everything validateID accepts —
	// they are two ends of the same rule, and running the output through the
	// validator is what keeps them from drifting. Today this catches exactly
	// one thing the loop above cannot: the id "active", which names the
	// package's own record file.
	if err := validateID(id); err != nil {
		return "", err
	}
	return id, nil
}

// validateID accepts exactly what DeriveID can produce. Everything that takes
// an id from outside — PathFor, and through it Load, Save, Delete — funnels
// through this, so the filename discipline exists once.
func validateID(id string) error {
	if id == "" {
		return errors.New("presets: empty preset id")
	}
	if len(id) > maxIDLen {
		return fmt.Errorf("presets: preset id %q is longer than %d characters", id, maxIDLen)
	}
	if strings.HasPrefix(id, "-") || strings.HasSuffix(id, "-") {
		return fmt.Errorf("presets: preset id %q must not start or end with a hyphen", id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return fmt.Errorf(
				"presets: preset id %q contains %q; only lower-case letters, digits and hyphens are valid", id, r)
		}
	}
	if reservedDeviceNames[strings.ToUpper(id)] {
		return fmt.Errorf("presets: preset id %q is a Windows reserved device name", id)
	}
	// "active" is reserved by this package itself: PathFor("active") is the
	// SAME file as ActivePath(), so a preset with that id would be written and
	// then immediately destroyed by the active-record write that follows every
	// save — with "Saved" on screen, no entry in the picker (List skips
	// active.json by name), and the credential scope switched to a vault that
	// holds nothing. The adversarial review found this; nothing else would
	// have, because every step of the collision looks like success.
	if id == activeID {
		return fmt.Errorf(
			"presets: %q is reserved for the package's own active-preset record (%s); choose another name",
			id, activeFileName)
	}
	return nil
}

// Save writes p atomically to PathFor(p.ID), creating the directory if it is
// missing.
//
// The write discipline is byte-for-byte the house pattern (config.Save,
// repeated in internal/mixer's golden store): temp file in the SAME directory
// so the rename is same-volume, Write, Sync, Close, os.Rename — which on
// Windows is MoveFileEx with MOVEFILE_REPLACE_EXISTING — with a deferred
// Remove of the un-renamed temp. A reader, including this process crashing
// mid-write, sees the old complete file or the new complete file, never a
// truncated one.
func Save(p Preset) error {
	if p.Version != Version {
		return fmt.Errorf("presets: refusing to write version %d; this build writes version %d", p.Version, Version)
	}
	path, err := PathFor(p.ID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("presets: encoding preset %q: %w", p.ID, err)
	}
	return writeFileAtomic(path, data)
}

// Load reads one preset by id. A version this build does not know is refused
// by name — see the Version constant for why silence would be worse.
func Load(id string) (Preset, error) {
	path, err := PathFor(id)
	if err != nil {
		return Preset{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Preset{}, fmt.Errorf("presets: no preset %q: %w", id, ErrNotFound)
		}
		return Preset{}, fmt.Errorf("presets: reading %s: %w", path, err)
	}
	var p Preset
	if err := json.Unmarshal(data, &p); err != nil {
		return Preset{}, fmt.Errorf("presets: parsing %s: %w", path, err)
	}
	if p.Version != Version {
		return Preset{}, fmt.Errorf(
			"%w: %s says version %d and this build reads version %d — it was written by a different build of wslcomms",
			ErrUnknownVersion, path, p.Version, Version)
	}
	return p, nil
}

// List returns every readable preset, sorted by name (then id, for two
// presets that share a display name).
//
// It DEGRADES rather than fails: an unparseable, wrong-version or misnamed
// file is skipped and logged, never returned as an error. One corrupt file
// must not blank the whole picker twenty minutes before a match — the presets
// that are fine are exactly what the operator needs at that moment.
func List() ([]Preset, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No presets have ever been saved. Not an error; an empty picker.
			return []Preset{}, nil
		}
		return nil, fmt.Errorf("presets: reading %s: %w", dir, err)
	}

	out := make([]Preset, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == activeFileName || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		p, err := Load(id)
		if err != nil {
			log.Printf("wslcomms: skipping unreadable preset file %s: %v", name, err)
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Delete removes a preset file. It knows nothing about credentials or the
// active record — App.DeletePreset owns those refusals, because they need the
// active scope and the secrets store, neither of which belongs in this
// package.
func Delete(id string) error {
	path, err := PathFor(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("presets: no preset %q: %w", id, ErrNotFound)
		}
		return fmt.Errorf("presets: deleting %s: %w", path, err)
	}
	return nil
}

// Rename changes a preset's display NAME and nothing else — never the id,
// never the credentialScope.
//
// That restriction is the guard against orphaned credentials, and it is silent
// when broken, which is why it is structural rather than advisory: rename the
// scope and the three passwords the operator typed for that instance become
// unreachable, with nothing on screen saying so — the app simply behaves as
// though no password was ever entered.
func Rename(id, newName string) error {
	name := strings.TrimSpace(newName)
	if name == "" {
		return errors.New("presets: a preset needs a name")
	}
	p, err := Load(id)
	if err != nil {
		return err
	}
	p.Name = name
	p.SavedAt = time.Now().UTC()
	return Save(p)
}

// SaveActive writes the active-preset record atomically.
func SaveActive(rec ActiveRecord) error {
	path, err := ActivePath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("presets: encoding the active-preset record: %w", err)
	}
	return writeFileAtomic(path, data)
}

// LoadActive reads the active-preset record. It NEVER guesses:
//
//   - missing file  -> zero record, found=false, nil error — no preset has
//     ever been applied, the legacy credential scope is correct;
//   - corrupt file  -> zero record, found=false, and an error SAYING SO — the
//     caller reports it, because falling back silently means "no password
//     stored" and "the wrong vault entry was consulted" look identical on the
//     lamps;
//   - readable file -> the record, found=true, nil.
//
// The zero record's empty scope is the deliberate worst case: the machine's
// original stored passwords are used and the UI says no preset is applied,
// which is recoverable — unlike a guess at some other instance's credentials.
func LoadActive() (rec ActiveRecord, found bool, err error) {
	path, err := ActivePath()
	if err != nil {
		return ActiveRecord{}, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ActiveRecord{}, false, nil
		}
		return ActiveRecord{}, false, fmt.Errorf("presets: reading %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return ActiveRecord{}, false, fmt.Errorf("presets: parsing %s: %w", path, err)
	}
	return rec, true, nil
}

// writeFileAtomic is the shared body of Save and SaveActive: MkdirAll 0700,
// CreateTemp beside the target, Write, Sync, Close, Rename, with the temp
// removed if anything before the rename fails.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("presets: creating %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("presets: creating temp file in %s: %w", dir, err)
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
		return fmt.Errorf("presets: writing %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("presets: syncing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("presets: closing %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("presets: renaming %s to %s: %w", tmpPath, path, err)
	}
	renamed = true
	return nil
}
