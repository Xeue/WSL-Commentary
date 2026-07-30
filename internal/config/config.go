// Package config owns the application's JSON configuration file at
// %APPDATA%\WSLComms\config.json, described in section 9 of the specification.
//
// Owner: WP-1. No other work package writes files in this directory.
//
// The two secrets referenced by this configuration — the M2L-X password and the
// SRT passphrase — are deliberately NOT stored here. They live in Windows
// Credential Manager and are reached through the internal/secrets package.
package config

import "errors"

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
func Path() (string, error) {
	return "", errors.New("not implemented: WP-1")
}

// Load reads the configuration file. If the file does not exist, Load returns
// Defaults() and a nil error, so that first run is not an error condition.
// Fields absent from an existing file take their values from Defaults().
func Load() (*Config, error) {
	return nil, errors.New("not implemented: WP-1")
}

// Save writes the configuration atomically to Path(), creating the directory if
// it is missing. It must never write the M2L-X password or the SRT passphrase.
func (c *Config) Save() error {
	return errors.New("not implemented: WP-1")
}
