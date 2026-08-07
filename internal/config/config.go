// Package config owns the application's JSON configuration file at
// %APPDATA%\WSLComms\config.json, described in section 9 of the specification.
//
// Owner: WP-1. No other work package writes files in this directory.
//
// The three secrets referenced by this configuration — the M2L-X password, the
// SEND path's SRT passphrase and the RETURN path's SRT passphrase — are
// deliberately NOT stored here. They live in Windows Credential Manager and are
// reached through the internal/secrets package. What this file holds is the
// non-secret half of each: which key LENGTH to negotiate (PBKeyLen for the
// send path, SRTReturnPBKeyLen for the return), never the key itself.
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
	// frontend's setSinkId call on the WEBRTC return path.
	//
	// It is not interchangeable with HeadphoneEndpointID below. See that field.
	HeadphoneDeviceID string `json:"headphoneDeviceId"`

	// HeadphoneEndpointID is the WASAPI IMMDevice endpoint ID GUID of the same
	// headphones, used by the SRT return path and passed to wasapi2sink's device
	// property. Empty means the Windows default playback device.
	//
	// # Why there are two of these and why they must not be merged
	//
	// They identify the same physical output and are different KINDS of
	// identifier. HeadphoneDeviceID is a browser mediaDeviceId: a per-origin,
	// per-session salted hash minted by the WebView, meaningful only to
	// enumerateDevices and setSinkId, and regenerated when the browsing
	// context's storage is cleared. HeadphoneEndpointID is an IMMDevice endpoint
	// ID GUID, which is what WASAPI takes and what survives a device rename.
	//
	// Neither can be converted into the other, and the failure of using one
	// where the other belongs is silent in both directions: setSinkId rejects an
	// endpoint GUID and keeps playing to the default device, and wasapi2sink
	// does not recognise a mediaDeviceId and falls back to the default endpoint.
	// In both cases the commentator gets audio, in the wrong ears, with nothing
	// anywhere saying why. Two fields, two dropdowns, two enumerations —
	// gst.ListOutputDevices fills this one.
	HeadphoneEndpointID string `json:"headphoneEndpointId"`

	// ReturnSource selects which return path feeds the headphones: "webrtc"
	// (the default) or "srt".
	//
	// # Why this is a choice rather than both
	//
	// The two paths reach the same headphones by different routes, and running
	// both plays the same programme twice with a few hundred milliseconds
	// between the copies — which is not "a bit of echo", it is unusable to
	// commentate over. So this is exclusive by construction: App.StartReturn
	// refuses unless this is "srt", and the frontend only attaches the WebRTC
	// return when it is "webrtc".
	//
	// The default stays "webrtc" because that is the path that has been used on
	// air. The SRT path exists for the case the WebRTC one cannot serve: the CLN
	// bus now carries FX hard-left and comms hard-right, and monitoring just the
	// effects means picking one CHANNEL, which needs ReturnChannel below and
	// which the browser side has no reliable way to do.
	ReturnSource string `json:"returnSource"`

	// ReturnChannel is which channel of the SRT return reaches the headphones:
	// "stereo" (the default), "left" or "right".
	//
	// On the operator's current routing left is the effects feed and right is
	// the comms feed. "left" and "right" put that source channel into BOTH ears
	// rather than silencing one — a commentator with one dead side spends the
	// match assuming the headphones are broken.
	//
	// It has no effect on the WebRTC return path, which selects a whole bus by
	// ReturnMid and cannot select within it.
	ReturnChannel string `json:"returnChannel"`

	// SRTReturnPort is the port of the M2L-X output the SRT return dials.
	// Default 40501, which is Output 1, src=pgm — THE DIRTY PROGRAMME FEED.
	//
	// The HOST is deliberately absent. It follows the M2L-X host through
	// EffectiveSRTHost, exactly as the send path does, because on every instance
	// seen so far the SRT listener answers on the same name as the REST API and
	// a third host field is a third thing to get wrong under pressure.
	//
	// # Why PGM and not CLN
	//
	// This defaulted to 40503 (src=cln) for one revision and that was wrong. The
	// operator's requirement is the DIRTY picture — programme, with everything on
	// it — because that is what a commentator watches. Clean audio comes from the
	// WebRTC monitor's mid 2, which is the same bus by a different route, so
	// there is nothing the CLN output was needed for.
	//
	// Measured on the live instance: Output 1 src=pgm on 40501, Output 2 src=pvw
	// (whose AUDIO is the master bus, the same as pgm, so it is not a fourth
	// bus), Output 3 src=cln on 40503, and Outputs 4-7 byte-transparent relays of
	// router inputs. M2L-X's output source field accepts only
	// pgm | pvw | cln | <router input id> — aux1, aux2, master and mon1 all
	// return HTTP 400 — so this is the whole menu.
	//
	// Verified by dialling it: 40501 negotiates video/x-h265 hvc1 1920x1080 50/1
	// and audio/mpeg mpegversion=4 base-profile=lc.
	SRTReturnPort int `json:"srtReturnPort"`

	// SRTReturnPBKeyLen is the SRT encryption key length for the RETURN path,
	// 16 or 32. Zero — the default — means encryption is not negotiated at all.
	//
	// It has exactly the semantics of PBKeyLen above, applied to a different
	// endpoint, and it is a SEPARATE field rather than a reuse of that one
	// because M2L-X sets encryption PER OUTPUT. Measured on the live instance:
	//
	//	Output 1  src=pgm  port 40501  encrypted=false
	//	Output 2  src=pvw  port 40502  encrypted=true
	//	Output 3  src=cln  port 40503  encrypted=true
	//
	// So the send path and the return path routinely need different answers,
	// and sharing one field means the operator cannot express the arrangement
	// that is actually in front of them.
	//
	// # THE PASSPHRASE IS NOT HERE
	//
	// It is in Windows Credential Manager under secrets.TargetSRTReturn
	// ("WSLComms/srtreturn"), reached through internal/secrets, exactly as the
	// M2L-X password and the send path's passphrase are. config.json is written
	// to %APPDATA% in plain text, is hand-editable by design, and is the first
	// thing that gets pasted into a support ticket; a passphrase in it is a
	// passphrase in every copy of that file that has ever been mailed. Save
	// must never write one — TestSave_NeverWritesSecretFields enforces it — so
	// what lives here is the key LENGTH, which is not a secret and which
	// internal/gst needs alongside the passphrase to set up srtsrc.
	//
	// This field being non-zero with no stored passphrase is the one
	// combination App.StartReturn refuses outright: it asks for an encrypted
	// session with no key, which cannot succeed and which otherwise fails
	// inside libsrt as ERROR:UNSECURE with nothing on screen to say so.
	SRTReturnPBKeyLen int `json:"srtReturnPBKeyLen"`

	// PictureLatencyMs is srtsrc's latency, in MILLISECONDS, for the PICTURE
	// monitor — the commentator's programme window. Default 120.
	//
	// # Why it is its own field and not SRTLatencyMs
	//
	// It was SRTLatencyMs until this revision: app_picture.go read the SEND
	// path's latency and handed it to the picture. That is two unrelated
	// decisions sharing one number. SRTLatencyMs is how much reordering and
	// retransmission budget the CONTRIBUTION FEED carries on its way out — a
	// figure that trades delay against the feed breaking up on air, where
	// breaking up is unacceptable and delay is nearly free. This is how much
	// budget the commentator's MONITOR carries, where the trade runs the other
	// way: a monitor is a thing to react to, delay is the whole complaint, and a
	// dropped frame on it costs nobody anything. Lowering one to fix the other
	// would have thinned the protection on the feed that actually goes to air.
	//
	// # It is not the only latency on the picture path, and it is not the largest
	//
	// Measured against the live instance on 2026-08-07, GStreamer's own latency
	// query on the picture pipeline reported 855 ms, made up of srtsrc 120 ms,
	// tsdemux 700 ms, h265parse 20 ms and d3d11videosink's 15 ms processing
	// deadline. The dominant term is tsdemux's default and has nothing to do with
	// this field; it is dealt with in internal/gst by not letting the video sink
	// honour it (see pictureSinkSync). An operator who moves this number and
	// expects the whole delay to move with it will be disappointed by roughly
	// seven hundred milliseconds.
	//
	// # THE FAR END CAN FLOOR THIS, AND ON THIS INSTANCE IT APPEARS NOT TO
	//
	// SRT buffers to the LARGER of the two peers' latencies, so a receiver
	// cannot unilaterally get below what the sender demands. The operator's
	// M2L-X Output 1 is configured with Buffer (msec) = 300, which is the reason
	// to expect a floor at 300 — and the Settings hint warns about one, because
	// an operator who lowers this, sees no change and is told nothing concludes
	// the control is broken.
	//
	// MEASURED, 2026-08-07, and it did NOT behave like a floor. Time from
	// process start to first decoded frame:
	//
	//	latency=40     1803, 1884, 1909, 2053, 2341 ms   (n=5, mean 1998)
	//	latency=300    2407, 2430, 2430, 2447, 4045 ms   (n=5, mean 2752)
	//	latency=2000   3865, 3869 ms                     (n=2, mean 3867)
	//
	// The two lower groups do not overlap at all: the slowest latency=40 run
	// (2341 ms) beat the fastest latency=300 run (2407 ms), five times out of
	// five. A floor at 300 would have made them indistinguishable. So on this
	// instance the setting appears to take effect across its whole range, and
	// lowering it below 300 is worth trying rather than pointless.
	//
	// That measurement is NOT conclusive about the mechanism, and the difference
	// matters enough to say why: time-to-first-frame also contains DNS, the SRT
	// handshake, PMT discovery and a wait of up to one GOP for something
	// decodable, which is why the deltas above overshoot the nominal setting
	// differences. The negotiated latency itself was not read — srtsrc's "stats"
	// property is not reachable from gst-launch and nothing at GST_DEBUG
	// srtobject:7 prints it — so this is inferred from end-to-end timing rather
	// than read off the socket.
	PictureLatencyMs int `json:"pictureLatencyMs"`

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

	// ReturnSourceWebRTC is the KVS/WebRTC return, and the default. It is the
	// path that has been used on air.
	ReturnSourceWebRTC = "webrtc"

	// ReturnSourceSRT is the SRT return: a second SRT session, dialled as a
	// caller into an M2L-X output, decoded and played to the headphones by
	// internal/gst. It is what makes single-channel monitoring possible.
	ReturnSourceSRT = "srt"

	// DefaultReturnSource is ReturnSourceWebRTC. Changing the default would
	// change which return path a machine uses on its next launch without anyone
	// asking for it, which is not a thing to do to a commentary position.
	DefaultReturnSource = ReturnSourceWebRTC

	// The three channel selections. They mirror gst.ReturnChannel's values,
	// which are the same strings; config does not import internal/gst, because
	// a configuration package that depends on the cgo package cannot be tested
	// without it.
	ReturnChannelStereo = "stereo"
	ReturnChannelLeft   = "left"
	ReturnChannelRight  = "right"

	// DefaultReturnChannel passes the return through unchanged.
	DefaultReturnChannel = ReturnChannelStereo

	// DefaultSRTReturnPort is Output 1 on the measured instance: src=pgm, the
	// DIRTY programme feed, which is the picture a commentator watches. See the
	// field comment for why this is not the clean feed.
	DefaultSRTReturnPort = 40501

	// DefaultSRTReturnPBKeyLen is 0: no encryption negotiated on the return.
	//
	// That is the right default for DefaultSRTReturnPort, and the two belong
	// together. Output 1 measured encrypted=false, so a default pair of
	// (40501, 0) connects on a stock instance with nothing typed in. Defaulting
	// this to 16 or 32 would make a first run fail against an unencrypted
	// output, which is the mirror image of the fault this whole change exists
	// to stop happening.
	DefaultSRTReturnPBKeyLen = 0

	// DefaultPictureLatencyMs is the picture monitor's SRT latency in
	// milliseconds. It matches DefaultSRTLatencyMs and internal/gst's
	// DefaultPictureLatencyMs — roughly five times the measured 21 ms median
	// round-trip time to the M2L-X instance.
	//
	// It is restated here rather than imported from internal/gst for the reason
	// ReturnChannelStereo and the rest are: internal/config must not depend on
	// the cgo package, because a configuration package that cannot be tested
	// without GStreamer is a configuration package that stops being tested.
	// TestPictureLatencyDefaultMatchesGst pins the two together.
	//
	// It is deliberately NOT lowered as part of the change that introduced it.
	// The second the operator was complaining about was the video sink
	// synchronising to the pipeline clock — 993.7 ms of it, removed in
	// internal/gst — and nothing about this number. Lowering the default as well
	// would have spent real retransmission budget, on an unprotected internet
	// path that already shows continuity errors, to chase a fraction of what had
	// just been fixed for nothing. It is a control now; the operator can lower it
	// and watch, which is the point of making it a field.
	DefaultPictureLatencyMs = 120

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
		SRTLatencyMs:  DefaultSRTLatencyMs,
		ReturnMid:     DefaultReturnMid,
		MonitorTile:   DefaultMonitorTile,
		ReturnGainDB:  DefaultReturnGainDB,
		SlatePath:     DefaultSlateFilename,
		ReturnSource:  DefaultReturnSource,
		ReturnChannel: DefaultReturnChannel,
		SRTReturnPort: DefaultSRTReturnPort,
		// Explicit even though it is the zero value. Defaults() is a table that
		// says what every documented default IS, and a field that is silently
		// absent from it reads as "nobody decided", which for an encryption
		// setting is not the same as "decided: none".
		SRTReturnPBKeyLen: DefaultSRTReturnPBKeyLen,
		PictureLatencyMs:  DefaultPictureLatencyMs,
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

// EffectiveReturnSource returns the configured return path, substituting
// DefaultReturnSource for an empty value.
//
// Load already substitutes defaults for keys ABSENT from config.json, but an
// explicitly empty string survives — a hand-edited file, or a Settings screen
// that saved a half-filled form. "webrtc" is the answer in both cases: it is
// what the position was doing before anyone touched the file.
func (c *Config) EffectiveReturnSource() string {
	if s := strings.TrimSpace(c.ReturnSource); s != "" {
		return s
	}
	return DefaultReturnSource
}

// EffectiveReturnChannel returns the configured channel selection,
// substituting DefaultReturnChannel for an empty value.
func (c *Config) EffectiveReturnChannel() string {
	if s := strings.TrimSpace(c.ReturnChannel); s != "" {
		return s
	}
	return DefaultReturnChannel
}

// EffectiveSRTReturnPort returns the port the SRT return dials, substituting
// DefaultSRTReturnPort for a zero value.
//
// Zero and "unset" are the same thing here — zero is not a valid UDP port — so
// unlike EffectiveReturnSource this substitution cannot mask a deliberate
// setting.
func (c *Config) EffectiveSRTReturnPort() int {
	if c.SRTReturnPort != 0 {
		return c.SRTReturnPort
	}
	return DefaultSRTReturnPort
}

// EffectivePictureLatencyMs returns the SRT latency the picture monitor dials
// with, substituting DefaultPictureLatencyMs for a zero value.
//
// Zero and "unset" are the same thing here, as they are for the return port: a
// zero-millisecond SRT latency is not a setting anyone wants — it disables the
// retransmission window entirely, so a single lost packet is a visible tear —
// and every config.json written before this field existed has zero in it. Those
// files must come up on the default rather than on nothing.
//
// It is also what internal/gst.PictureOpts.normalise would do with a zero, so
// the substitution happens once, here, rather than differently in two places.
func (c *Config) EffectivePictureLatencyMs() int {
	if c.PictureLatencyMs > 0 {
		return c.PictureLatencyMs
	}
	return DefaultPictureLatencyMs
}

// UsesSRTReturn reports whether the SRT return path is the configured one.
func (c *Config) UsesSRTReturn() bool {
	return c.EffectiveReturnSource() == ReturnSourceSRT
}

// ValidateReturn reports every reason the SRT RETURN cannot start, joined one
// message per problem field, or nil when it is ready.
//
// # Why this is separate from Validate and not folded into it
//
// Validate is the gate on Start — on putting the contribution feed on air — and
// nothing in it may be a reason a match does not go out. The return is a
// commentator's monitor: valuable, but a mistyped returnChannel must not be the
// reason the feed stays off. This is the same judgement the statusKey field
// records, and it was made the same way: requiring a field for Start that Start
// does not need made the application unstartable for a reason that had nothing
// to do with sending.
//
// So the two live apart. App.Start calls Validate; App.StartReturn calls this.
// A configuration can be perfectly able to send and unable to monitor, which is
// exactly what happens on the first run after the operator switches
// returnSource to "srt" and has not yet picked a headphone endpoint.
//
// headphoneEndpointId is deliberately NOT required. Empty means wasapi2sink
// opens the Windows default playback device, which on a commentary position is
// very often the right one and is always better than refusing to monitor at all.
func (c *Config) ValidateReturn() error {
	var errs []error

	switch c.EffectiveReturnSource() {
	case ReturnSourceWebRTC, ReturnSourceSRT:
	default:
		errs = append(errs, fmt.Errorf("returnSource must be %q or %q, got %q",
			ReturnSourceWebRTC, ReturnSourceSRT, c.ReturnSource))
	}

	switch c.EffectiveReturnChannel() {
	case ReturnChannelStereo, ReturnChannelLeft, ReturnChannelRight:
	default:
		errs = append(errs, fmt.Errorf("returnChannel must be %q, %q or %q, got %q",
			ReturnChannelStereo, ReturnChannelLeft, ReturnChannelRight, c.ReturnChannel))
	}

	if p := c.EffectiveSRTReturnPort(); p < 1 || p > 65535 {
		errs = append(errs, fmt.Errorf("srtReturnPort must be between 1 and 65535, got %d", p))
	}

	// The PICTURE monitor's SRT latency. Checked here, with the other monitor
	// fields, and deliberately NOT in Validate, for the reason this method's
	// header gives at length: no monitor setting may be a reason the
	// contribution feed does not go on air. A commentator with a mistyped
	// picture latency should still be able to send.
	//
	// A negative value is the only one refused outright. Zero means "use the
	// default" — see EffectivePictureLatencyMs, and note that every config.json
	// written before this field existed has zero in it — so refusing zero would
	// make every upgraded installation fail this check on first launch. The
	// upper bound is generous on purpose: an operator on a bad satellite path
	// may legitimately want seconds of retransmission budget, and this is a
	// monitor, so the cost of them being wrong is their own picture.
	if ms := c.PictureLatencyMs; ms < 0 || ms > 8000 {
		errs = append(errs, fmt.Errorf(
			"pictureLatencyMs must be between 0 and 8000 milliseconds, got %d", ms))
	}

	// The same three values Validate allows for the send path's pbkeylen, and
	// for the same reason: 16 and 32 are the only key lengths SRT's AES-CTR
	// supports, and 0 means no encryption is negotiated. Checked HERE rather
	// than in Validate because it is a return field, and no return field may be
	// a reason the contribution feed does not go on air — see this method's
	// header for the whole argument.
	if k := c.SRTReturnPBKeyLen; k != 0 && k != 16 && k != 32 {
		errs = append(errs, fmt.Errorf("srtReturnPBKeyLen must be 0, 16 or 32, got %d", k))
	}

	if c.EffectiveSRTHost() == "" {
		errs = append(errs, errors.New(
			"the SRT return has no host to dial: set m2lxHost, or srtHost to override it"))
	}

	return errors.Join(errs...)
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
