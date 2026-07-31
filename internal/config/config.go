// Package config owns the application's JSON configuration file at
// %APPDATA%\WSLComms\config.json, described in section 9 of the specification.
//
// Owner: WP-1. No other work package writes files in this directory.
//
// The two secrets referenced by this configuration — the M2L-X password and the
// SRT passphrase — are deliberately NOT stored here. They live in Windows
// Credential Manager and are reached through the internal/secrets package.
//
// WP-1 addition beyond the WP-0 contract: (*Config).Validate reports which
// fields required for Start to succeed are missing or out of range. It is not
// part of the original interface declaration — Config, Defaults, Path, Load
// and Save are — but WP-8's Start needs a single place that knows which
// fields are mandatory, so it is added here rather than duplicated at every
// call site. Reported to the coordinator per contract rule 3.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Tile is a rectangle in the KVS multiviewer mosaic, in mosaic pixels.
//
// The mosaic delivered on the KVS video track is 2240x1440. The frontend crops
// this rectangle out of it with CSS and displays it as the programme picture
// (requirement R3). It is configuration rather than code because it is an M2L-X
// layout fact measured on the dev event, not an application constant.
type Tile struct {
	// X is the left edge of the tile within the mosaic, in pixels.
	X int `json:"x"`
	// Y is the top edge of the tile within the mosaic, in pixels.
	Y int `json:"y"`
	// W is the tile width in pixels.
	W int `json:"w"`
	// H is the tile height in pixels.
	H int `json:"h"`
}

// Config is the whole of the application's persisted configuration.
//
// The JSON tags are normative: they are both the on-disk field names required by
// specification section 9 and the property names the frontend sees when this
// struct crosses the Wails boundary. They must not be changed.
type Config struct {
	// M2LXHost is the M2L-X host, without scheme, e.g. "m2lx.example.com".
	// The REST base and the status WebSocket URL are both derived from it.
	M2LXHost string `json:"m2lxHost"`

	// Alias is the M2L-X sign-in name. Note that the sign-in request body field
	// is "alias", not "username"; sending "username" returns HTTP 500.
	Alias string `json:"alias"`

	// EventID is the M2L-X event identifier used in the KVS credential
	// endpoints, /api/live_operation/kvs/webrtc_info/{eventId} and
	// /api/live_operation/kvs/webrtc_token/{eventId}.
	EventID string `json:"eventId"`

	// SRTHost is the hostname or address of the M2L-X SRT listener that this
	// application dials as a caller.
	//
	// It is OPTIONAL. Empty means "the same host as M2L-X": on every instance
	// seen so far the SRT listener answers on the same name as the REST API, and
	// asking the operator to type that name a second time is one more thing to
	// get wrong under pressure. Read it through EffectiveSRTHost, never
	// directly, so the fallback happens in exactly one place.
	SRTHost string `json:"srtHost"`

	// SRTPort is the port of that SRT listener.
	SRTPort int `json:"srtPort"`

	// SRTLatencyMs is srtsink's latency property, in MILLISECONDS (not
	// microseconds). Default 120.
	SRTLatencyMs int `json:"srtLatencyMs"`

	// PBKeyLen is the SRT encryption key length, 16 or 32. Zero means the
	// listener has no passphrase set and encryption is not negotiated.
	PBKeyLen int `json:"pbkeylen"`

	// StatusKey is the switcher_status node name for our router input, e.g.
	// "cam7". Every WebSocket-derived status lamp reads <statusKey>.* .
	//
	// It is OPTIONAL and is not required to send. It names the node the three
	// WebSocket-derived lamps read; with it empty those lamps report NO STATUS,
	// which is honest, and the contribution feed is unaffected. Requiring it
	// would be worse than useless: it cannot be derived from any REST endpoint,
	// so the only way to find it is to watch switcher_status while our feed
	// comes up (spec open question 5) — which cannot happen if the app refuses
	// to start without it. See App.GetStatusKeyCandidates.
	StatusKey string `json:"statusKey"`

	// AudioDeviceID is the WASAPI IMMDevice endpoint ID GUID of the commentary
	// input, written by the input dropdown. It is never the friendly name.
	AudioDeviceID string `json:"audioDeviceId"`

	// HeadphoneDeviceID is the browser mediaDeviceId of the commentator's
	// headphone output, written by the output dropdown and consumed only by the
	// frontend's setSinkId call. No Go package enumerates output devices.
	HeadphoneDeviceID string `json:"headphoneDeviceId"`

	// ReturnMid is the WebRTC transceiver mid whose audio is routed to the
	// headphones. Default 2, which is aux1/CLN — effects without commentary.
	// Mid 1 is master/PGM.
	ReturnMid int `json:"returnMid"`

	// MonitorTile is the region of the KVS mosaic that holds the programme
	// picture. Default {0, 360, 640, 360}.
	MonitorTile Tile `json:"monitorTile"`

	// ReturnGainDB is the make-up gain in decibels applied to the return audio
	// before it reaches the headphones. Default +18.
	//
	// The KVS monitor track arrives approximately 18 dB below the level fed into
	// the SRT input — measured repeatably at two injection levels, matching the
	// M2L-X bus meter to within 0.1 dB, cause not established. Without make-up
	// gain the return is far too quiet to commentate over. It is configuration
	// rather than a constant for the same reason MonitorTile is: it is a measured
	// property of M2L-X, not of this application, and if Sony changes it this is
	// one edited number.
	//
	// The frontend applies it as a GainNode value of 10^(ReturnGainDB/20) and the
	// level slider scales that result.
	ReturnGainDB float64 `json:"returnGainDb"`

	// SlatePath is the PNG fed to filesrc ! pngdec ! imagefreeze. It defaults to
	// the slate.png installed alongside the executable.
	SlatePath string `json:"slatePath"`
}

// Default values fixed by specification section 9. They are constants so that
// no other package has to restate them.
const (
	// DefaultSRTLatencyMs is srtsink's latency in milliseconds, roughly five
	// times the measured 21 ms median round-trip time.
	DefaultSRTLatencyMs = 120

	// DefaultReturnMid is the transceiver mid routed to the headphones: aux1/CLN.
	DefaultReturnMid = 2

	// DefaultReturnGainDB is the measured offset between the SRT-ingested peak
	// level and the level the KVS monitor arrives at.
	DefaultReturnGainDB = 18.0

	// DefaultSlateFilename is the slate file name relative to the directory
	// holding the executable.
	DefaultSlateFilename = "slate.png"

	// AppDataDirName is the folder created under %APPDATA% for config.json.
	AppDataDirName = "WSLComms"

	// FileName is the configuration file's name inside AppDataDirName.
	FileName = "config.json"
)

// DefaultMonitorTile is the programme tile's position in the 2240x1440 mosaic,
// as measured on the dev event.
var DefaultMonitorTile = Tile{X: 0, Y: 360, W: 640, H: 360}

// Defaults returns a Config populated with every documented default from
// specification section 9. Fields with no documented default — hosts, alias,
// event and status keys, device IDs — are left at their zero values and must be
// supplied by the user on the Settings screen before Start can succeed.
//
// This is the one function in the WP-0 contract with a real body: it is a table
// of constants, not logic, and stating it here stops nine packages from
// disagreeing about what "default" means.
func Defaults() *Config {
	return &Config{
		SRTLatencyMs: DefaultSRTLatencyMs,
		ReturnMid:    DefaultReturnMid,
		MonitorTile:  DefaultMonitorTile,
		ReturnGainDB: DefaultReturnGainDB,
		SlatePath:    DefaultSlateFilename,
	}
}

// Path returns the absolute path of the configuration file,
// %APPDATA%\WSLComms\config.json. It does not create the directory.
//
// os.UserConfigDir resolves %AppData% on Windows, which is exactly
// %APPDATA%; this is also what lets tests substitute a temp directory by
// setting the APPDATA environment variable.
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: resolving user config directory: %w", err)
	}
	return filepath.Join(dir, AppDataDirName, FileName), nil
}

// Load reads the configuration file. If the file does not exist, Load returns
// Defaults() and a nil error, so that first run is not an error condition.
// Fields absent from an existing file take their values from Defaults().
//
// This works by unmarshalling onto a Defaults()-populated struct rather than
// a zero one: encoding/json only overwrites fields present in the source
// JSON, so a key missing from an older or hand-edited config.json leaves the
// corresponding field at its documented default instead of silently becoming
// the Go zero value (e.g. srtLatencyMs 0, which is not a valid srtsink
// latency, instead of the intended 120).
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Defaults(), nil
		}
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}

	cfg := Defaults()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	return cfg, nil
}

// Save writes the configuration atomically to Path(), creating the directory if
// it is missing. It must never write the M2L-X password or the SRT passphrase.
//
// Atomicity: the new content is written to a temp file created in the same
// directory as the target (so the later rename is same-volume, not a copy),
// flushed to stable storage with Sync, closed, and then moved over the
// target with os.Rename. On Windows, os.Rename uses MoveFileEx with
// MOVEFILE_REPLACE_EXISTING, so the target is replaced in a single directory
// operation: a reader of config.json — including this process crashing mid
// write, or a power cut during a match — always observes either the old
// complete file or the new complete file, never a truncated one.
func (c *Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: creating %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encoding config: %w", err)
	}

	tmp, err := os.CreateTemp(dir, FileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("config: creating temp file: %w", err)
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
		return fmt.Errorf("config: writing %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("config: syncing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: closing %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("config: renaming %s to %s: %w", tmpPath, path, err)
	}
	renamed = true
	return nil
}

// EffectiveSRTHost returns the host this application dials for SRT: SRTHost
// when it is set, and otherwise the M2L-X host with any scheme, path and port
// stripped off.
//
// Empty SRTHost is the normal case, not a misconfiguration — see the field
// comment. An explicit SRTHost is an override for the one case that needs it,
// an SRT ingest published under a different name, and is returned verbatim
// apart from surrounding whitespace.
//
// M2LXHost may carry an explicit "http://" or "https://" prefix (internal/m2lx
// resolveHost accepts one, and cmd/mockm2lx needs it) and a port. Neither
// belongs in the host half of the srt:// URI internal/gst builds, so both are
// removed. An IPv6 literal keeps its brackets, because that URI needs them.
func (c *Config) EffectiveSRTHost() string {
	if h := strings.TrimSpace(c.SRTHost); h != "" {
		return h
	}
	return hostOnly(c.M2LXHost)
}

// hostOnly reduces a host that may carry a scheme, a port and a path to the
// bare host. It is deliberately string surgery rather than net/url parsing: a
// bare "m2lx.example.com" is not a URL, and url.Parse reads it as a path.
func hostOnly(host string) string {
	h := strings.TrimSpace(host)
	if h == "" {
		return ""
	}
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+len("://"):]
	}
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	// An IPv6 literal is bracketed: [::1] or [::1]:8890. Only a colon after the
	// closing bracket is a port separator; the ones inside are the address.
	if strings.HasPrefix(h, "[") {
		if end := strings.IndexByte(h, ']'); end >= 0 {
			return h[:end+1]
		}
		return h
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return h
}

// Validate reports every reason (*Config) is not ready for Start to succeed,
// as a single error joining one message per problem field (via errors.Join)
// so the Settings screen can show the operator every problem at once rather
// than one edit-rebuild-fail cycle at a time. It returns nil when c is ready.
//
// Required non-empty fields: m2lxHost, alias, eventId, audioDeviceId, and an
// EffectiveSRTHost — which m2lxHost alone satisfies. srtPort must be a valid
// TCP/UDP port, 1..65535. pbkeylen must be 0 (no passphrase negotiated), 16 or
// 32 — the only key lengths SRT's AES-CTR supports. returnMid must be 1..7, the
// range of transceiver mids the KVS signalling channel can address.
//
// Deliberately NOT required: statusKey, which only names the node the three
// WebSocket-derived lamps read (see the field comment), and srtHost, which
// defaults to the M2L-X host (see EffectiveSRTHost). Neither is needed to put a
// feed on air, and requiring statusKey in particular made the app unstartable
// until the operator had guessed a value that nothing in the API can tell them.
//
// WP-1 addition beyond the WP-0 contract; see the package doc comment.
func (c *Config) Validate() error {
	var errs []error

	required := []struct {
		name  string
		value string
	}{
		{"m2lxHost", c.M2LXHost},
		{"alias", c.Alias},
		{"eventId", c.EventID},
		{"audioDeviceId", c.AudioDeviceID},
	}
	for _, f := range required {
		if strings.TrimSpace(f.value) == "" {
			errs = append(errs, fmt.Errorf("%s is required", f.name))
		}
	}

	if c.EffectiveSRTHost() == "" {
		errs = append(errs, errors.New(
			"srtHost is required when m2lxHost is empty: leave srtHost blank to dial the M2L-X host"))
	}

	if c.SRTPort < 1 || c.SRTPort > 65535 {
		errs = append(errs, fmt.Errorf("srtPort must be between 1 and 65535, got %d", c.SRTPort))
	}

	if c.PBKeyLen != 0 && c.PBKeyLen != 16 && c.PBKeyLen != 32 {
		errs = append(errs, fmt.Errorf("pbkeylen must be 0, 16 or 32, got %d", c.PBKeyLen))
	}

	if c.ReturnMid < 1 || c.ReturnMid > 7 {
		errs = append(errs, fmt.Errorf("returnMid must be between 1 and 7, got %d", c.ReturnMid))
	}

	return errors.Join(errs...)
}
