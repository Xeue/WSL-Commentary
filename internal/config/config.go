// Package config owns the application's JSON configuration file at
// %APPDATA%\WSLComms\config.json on Windows and
// ~/Library/Application Support/WSLComms/config.json on macOS, described in
// section 9 of the specification.
//
// Owner: WP-1. No other work package writes files in this directory.
//
// The three secrets referenced by this configuration — the M2L-X password, the
// SEND path's SRT passphrase and the RETURN path's SRT passphrase — are
// deliberately NOT stored here. They live in the operating system's credential
// store — Windows Credential Manager, or the login Keychain on macOS — and are
// reached through the internal/secrets package, which is also where the store's
// operator-facing name comes from. What this file holds is the non-secret half
// of each: which key LENGTH to negotiate (PBKeyLen for the send path,
// SRTReturnPBKeyLen for the return), never the key itself.
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

// ChannelContribution is one cell of a DeckLink card's routing: one of its
// embedded input channels reaching one side of the commentary feed, at one gain.
//
// It is the PERSISTED form of internal/gst's ChannelContribution, and the field
// names and json tags are IDENTICAL to that type's on purpose — the same
// discipline VideoFormatSpec keeps with gst.ConformTarget. This package cannot
// import internal/gst (that package is the only one allowed a cgo import, and
// config is loaded by tooling that must never link GStreamer), so the two
// structs are transcribed field by field in app.go; making them identical is
// what leaves the transcription nothing to get subtly wrong.
//
// A map is a LIST of these and not a dense 2xN grid, for a reason that shows up
// exactly here, at the point where it is written to disk and read back on a
// different day: a grid saved against a card presenting sixteen channels and
// reloaded against one presenting eight has to be truncated by somebody, whereas
// a list naming a channel that no longer exists is refused by name.
type ChannelContribution struct {
	// Output is which side of the commentary feed this contribution feeds:
	// 0 is left, 1 is right. There are two and there is no path to a third —
	// the AAC encoder is pinned to a stereo pair.
	Output int `json:"output"`

	// Input is the ZERO-BASED index of the card's input channel. Channel 1 on
	// the operator's embedder is Input 0 here; the +1 belongs to the UI and is
	// done in exactly one place there, because a conversion applied twice is
	// a commentator routed one channel along from where they are.
	Input int `json:"input"`

	// Gain is the linear coefficient, and audioconvert HARD-CLAMPS it to
	// [-1, 1]: 1.0 is accepted, 1.0000001 is refused, and the refusal is silent
	// — it leaves the previous matrix in force with nothing readable afterwards
	// to say which one is running. So this is a router with attenuation, not a
	// mixer with make-up gain. A negative value inverts polarity, which is a
	// real thing to want on a desk that has sent a leg out of phase.
	Gain float64 `json:"gain"`
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

	// The SRT host is NOT a field. It is ALWAYS the M2L-X host with any scheme,
	// path and port stripped off — see EffectiveSRTHost. On every instance the
	// SRT listener answers on the same name as the REST API, so a separate,
	// overridable srtHost was a field that could only drift out of step (and
	// did — it silently kept a stale value when an instance was switched). It
	// was removed at the operator's request; a config.json or preset that still
	// carries the old "srtHost" key is simply ignored on load.

	// SRTPort is the port of that SRT listener.
	SRTPort int `json:"srtPort"`

	// SRTLatencyMs is srtsink's latency property, in MILLISECONDS (not
	// microseconds). Default 120.
	SRTLatencyMs int `json:"srtLatencyMs"`

	// PBKeyLen is the SRT encryption key length, 16 or 32. Zero means the
	// listener has no passphrase set and encryption is not negotiated.
	PBKeyLen int `json:"pbkeylen"`

	// VideoBitrateKbps is the H.264 encoder's target bitrate for the video leg
	// of the contribution feed, in KILOBITS per second. Zero means
	// DefaultVideoBitrateKbps — see EffectiveVideoBitrateKbps.
	//
	// KILOBITS is the unit on both platforms' encoders (mfh264enc's "Bitrate in
	// kbit/sec", vtenc_h264's "Target video bitrate in kbps") and it is NOT the
	// unit of the AAC encoder beside it, which takes bits per second. The two
	// live one line apart in internal/gst; the suffix on this field's name is
	// there so a value copied from one to the other is off by a thousand in the
	// name as well as in the number.
	//
	// # Why this became a setting
	//
	// It was the constant 2000, and 2000 was chosen for what the video leg used
	// to be: a STILL SLATE, one PNG through imagefreeze, where the encoder has
	// nothing to spend a bitrate on and 2 Mbit/s is already generous. Live video
	// on that leg is a different picture entirely and the operator has ruled
	// 2000 too low for it — nearer 10000 is the figure they want. A constant
	// that was right for one kind of picture and wrong for the other is a
	// setting, and this is it.
	//
	// # Why INSTANCE, and not MACHINE
	//
	// It is a property of the PATH to that M2L-X deployment: how much of the
	// uplink between this seat and that ingest the feed may take. That is the
	// same kind of fact as srtLatencyMs and srtPort, which sit either side of it
	// in internal/presets.InstanceFields for the same reason — a venue's
	// contribution circuit is a venue's contribution circuit, whichever laptop
	// is plugged into it.
	//
	// # The default is today's effective value, deliberately
	//
	// DefaultVideoBitrateKbps is 2000: exactly what internal/gst has been
	// substituting for an unset bitrate all along. So an existing config.json
	// (which has no such key, and takes the default through Load) and a new one
	// both encode at the bitrate the shipped build already encodes at, and
	// NOTHING changes until somebody sets this. A default of 10000 would have
	// raised every position's uplink usage fivefold on the next launch, on no
	// measurement, as a side effect of adding a control.
	VideoBitrateKbps int `json:"videoBitrateKbps"`

	// VideoFormatOverride is the video format the contribution feed conforms
	// its video leg to when the format cannot be discovered from the switcher:
	// a string like "1920x1080p50". EMPTY MEANS DERIVE, and empty is the
	// default.
	//
	// # What it describes, and why it travels in a preset
	//
	// M2L-X can be configured into any format, and every source feeding it must
	// match: a 1080p50 slate into a 1080i25 instance does not "look wrong", it
	// fails to negotiate. So this describes how THAT SWITCHER is configured —
	// not this PC, not this laptop's screen — and every commentary position at
	// a facility is looking at the same answer. That is precisely what the
	// baked-in facility presets exist to carry (app_builtin_presets.go), which
	// is why it is classified INSTANCE.
	//
	// # Why there is an override at all
	//
	// The format is derivable from switcher_status when a node is streaming —
	// internal/m2lx parses state.streams.video.format into m2lx.VideoFormat —
	// and it is derivable from NOTHING when one is not. MEASURED against the
	// live matchH instance on 2026-08-15: all 35 nodes reported
	// "format": null with stream_state "stopped", including cam4 ("COMMS", our
	// own input), and seven plausible REST paths (/api/system, /api/settings,
	// /api/switcher, /api/config, /api/events/{id}, /api/live_operation/outputs,
	// /api/router) all answered 404. There is no endpoint that states the
	// instance's configured format, so a commentary position coming up first —
	// which is the normal case, an hour before anybody else — has nothing to
	// derive from and needs to be told.
	//
	// # THE REPRESENTATION: a string, parsed by exactly one function
	//
	// Not a nested struct of width/height/rate. Two reasons, and the first is
	// the one internal/m2lx already learned the hard way (see VideoFormat.Raw
	// there): what the operator typed must stay VISIBLE. A struct turns
	// "1920x1080p59.94" that this build cannot express into three plausible
	// zeros; a string keeps it, so Validate can quote it back, the Settings
	// field can show it, and a preset can be read in a text editor and
	// understood. A format nobody anticipated must read as itself and not as a
	// silent {0, 0, 0}.
	//
	// The second reason is this package's own merge primitive. Load and
	// presets.Apply both unmarshal onto an ALREADY-POPULATED struct, so a nested
	// object merges FIELD BY FIELD — that is documented and desirable for
	// monitorTile, where {"x":1120,"y":720} keeping the live w and h is exactly
	// right. It is a trap for a video format: a preset carrying {"width":1280}
	// would leave height 1080 and produce a 1280x1080 conform target nobody
	// asked for and nothing can negotiate. A string cannot half-arrive. It
	// merges whole or not at all, which is the only correct behaviour for a
	// value whose parts are meaningless apart.
	//
	// The cost, stated plainly: a string can be misspelled, and a struct cannot.
	// That cost is paid by ParseVideoFormat (videoformat.go) — the ONE parser,
	// with the ONE grammar — and by Validate refusing a value it cannot parse,
	// naming this field. See videoformat.go's header for the grammar and for
	// what is deliberately not in it.
	VideoFormatOverride string `json:"videoFormatOverride"`

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
	//
	// It is required by Validate ONLY when AudioSourceKind is "native": a seat
	// capturing from a DeckLink card takes its commentary audio from
	// decklinkaudiosrc and never opens a CoreAudio or WASAPI endpoint at all.
	// See the switch in Validate.
	AudioDeviceID string `json:"audioDeviceId"`

	// AudioSourceKind selects WHERE the commentary audio is captured from:
	// AudioSourceNative ("native", the default — the platform's own audio API,
	// wasapi2src or osxaudiosrc, reading AudioDeviceID) or AudioSourceDeckLink
	// ("decklink" — decklinkaudiosrc, reading DeckLinkPersistentID).
	//
	// # Why this is MACHINE state and can never travel in a preset
	//
	// It answers "what is plugged into THIS PC", and it is the audioDeviceId
	// failure exactly: a preset carrying "decklink", applied to a laptop with no
	// card in it, describes hardware that is not there. That fault arrives by
	// post — a config or a preset copied from the machine where it was true —
	// and it surfaces from inside GStreamer as
	// "Internal data stream error / not-negotiated (-4)", which is measured to
	// appear in about 100 microseconds and to name NEITHER the device NOR the
	// cause. It is classified MACHINE in internal/presets/fields.go, and the
	// whitelist there is what makes carrying it impossible rather than merely
	// discouraged.
	//
	// # Why it is a kind rather than being inferred from the device id
	//
	// Because the two device fields answer to different subsystems and an empty
	// one is a legitimate state in both. "Nothing in decklinkPersistentId"
	// cannot be read as "not a DeckLink seat": on a single-card machine it is
	// the ordinary way to say "the only card". An explicit kind means the
	// question is answered once, by the operator, on a control that says what it
	// does — not guessed from which of two boxes happens to be empty.
	//
	// EffectiveAudioSourceKind substitutes "native" for an empty value, so every
	// config.json written before this field existed keeps doing exactly what it
	// did.
	AudioSourceKind string `json:"audioSourceKind"`

	// DeckLinkPersistentID names WHICH Blackmagic card in THIS PC to capture
	// from when AudioSourceKind is "decklink". Empty means the only card — see
	// the note on that below.
	//
	// # ONE field for audio AND video, because the id names the CARD
	//
	// MEASURED: decklinkaudiosrc and decklinkvideosrc on the same card publish
	// the SAME persistent-id. It identifies the hardware, not the stream. And
	// the two elements must be in one pipeline anyway — decklinkaudiosrc CANNOT
	// preroll alone, because DeckLink drives audio capture off the video clock —
	// so a pair of fields could only ever hold one value twice, with a way to
	// disagree. There is no arrangement of hardware in which two fields would be
	// right, so there is one field.
	//
	// # PERSISTENT-ID, NEVER DEVICE-NUMBER
	//
	// This is the same rule as HeadphoneEndpointID's, which stores the CoreAudio
	// UID and never the integer AudioDeviceID, and it is the same rule for the
	// same reason. decklinkvideosrc/decklinkaudiosrc also take a "device-number"
	// property: an index into whatever order the driver enumerated the cards in
	// this boot. It is not an identity — plug in a second card, or reboot, and
	// device-number 0 is a different piece of hardware while every config on the
	// machine still says 0. persistent-id is minted by the card and survives
	// both. The integer, if internal/gst ever needs one, is resolved from this
	// string at pipeline-open time and never written here.
	//
	// # Empty, and why it is allowed rather than required
	//
	// Empty means "the card this machine has", and a commentary position has
	// one. Requiring an id would make a DeckLink seat unstartable until somebody
	// had gone and found a 16-digit number for a machine with exactly one thing
	// it could possibly mean — the same mistake that requiring statusKey was.
	// The exclusivity is what makes empty safe to resolve: the card admits ONE
	// user (two decklinkvideosrc in one process fail 3/3, and in two processes
	// fail 3/3), and the incumbent survives, so once this application holds the
	// card nothing can take it away mid-match.
	DeckLinkPersistentID string `json:"decklinkPersistentId"`

	// DeckLinkChannelMap is which of the card's embedded audio channels reach
	// the left and right of the commentary feed, and at what gain. It is read
	// only when AudioSourceKind is "decklink"; a native seat has no unpositioned
	// channels to route and the whole field is inert.
	//
	// # THE ABSENT VALUE IS NOT SILENCE, AND MUST NEVER BE MATERIALISED
	//
	// An empty or missing map means "NOBODY HAS CHOSEN", and internal/gst
	// resolves that to input 1 on the left and input 2 on the right at unity —
	// which is bit-for-bit what this application already sent from a card
	// configured for two channels, so a seat whose operator never opens the
	// routing screen hears exactly what it heard before the screen existed.
	//
	// A well-meaning migration that wrote that default out explicitly would
	// freeze TODAY'S default into every config file on every machine, and the
	// next change to it would silently not reach any of them. Leave the field
	// absent. Nothing here defaults it, there is no EffectiveDeckLinkChannelMap,
	// and that is deliberate: the resolution belongs to the one package that
	// knows the negotiated channel count to resolve it against.
	//
	// It is the one field in this struct carrying omitempty, and that is the
	// same decision said in the encoder: a seat that has never routed anything
	// writes a config.json byte-identical to the one it writes today, rather than
	// growing a "decklinkChannelMap": null nobody chose. Reading is unaffected —
	// absent, null and [] are the same nil slice and the same meaning.
	//
	// # Why this is MACHINE state
	//
	// It describes which XLR on which embedder carries the commentator in THIS
	// room — a property of the building's wiring, not of the facility being
	// covered. It is classified in internal/presets/fields.go beside
	// audioSourceKind and decklinkPersistentId, and for a sharper version of
	// their reason: a preset carrying a routing would silently move somebody's
	// microphone from a configuration screen, in a different building, mid-match.
	DeckLinkChannelMap []ChannelContribution `json:"decklinkChannelMap,omitempty"`

	// HeadphoneDeviceID is the browser mediaDeviceId of the commentator's
	// headphone output, written by the output dropdown and consumed only by the
	// frontend's setSinkId call on the WEBRTC return path.
	//
	// It is not interchangeable with HeadphoneEndpointID below. See that field.
	HeadphoneDeviceID string `json:"headphoneDeviceId"`

	// HeadphoneEndpointID is the operating system's own stable identity for the
	// same headphones, used by the SRT return path. It is a WASAPI IMMDevice
	// endpoint ID GUID on Windows and a CoreAudio device UID on macOS. Empty
	// means this platform's default playback device.
	//
	// On macOS it is deliberately NOT the integer that osxaudiosink's own
	// "device" property takes. That integer is an AudioDeviceID: a handle
	// coreaudiod allocates per enumeration and reuses, so it does not survive a
	// reboot or a replug and is not an identity at all. internal/gst resolves it
	// from this string every time it opens a pipeline, and the integer never
	// reaches this file, never crosses the Wails boundary, and appears in a log
	// only as a diagnostic. See gst.ReturnOpts.OutputDeviceID.
	//
	// # Why there are two of these and why they must not be merged
	//
	// They identify the same physical output and are different KINDS of
	// identifier. HeadphoneDeviceID is a browser mediaDeviceId: a per-origin,
	// per-session salted hash minted by the WebView, meaningful only to
	// enumerateDevices and setSinkId, and regenerated when the browsing
	// context's storage is cleared. HeadphoneEndpointID is the OS's own identity
	// for the device — an IMMDevice endpoint ID GUID, or a CoreAudio UID — which
	// is what the platform's sink can be resolved from and what survives a device
	// rename.
	//
	// That the native half is platform-dependent does not soften the rule by one
	// inch. The browser id is not ANY operating system's identifier for a device;
	// it is a per-origin salted token minted by one browsing context. There is no
	// platform on which the two converge, so there is no platform on which
	// merging them is closer to working than it is here.
	//
	// Neither can be converted into the other, and the failure of using one
	// where the other belongs is silent in both directions: setSinkId rejects an
	// endpoint GUID and keeps playing to the default device, and the native sink
	// does not recognise a mediaDeviceId. internal/gst now catches THAT half
	// before the sink ever sees it and logs why — see gst.chooseOutputDevice —
	// rather than leaving the commentator with audio in the wrong ears and
	// nothing anywhere saying so. Two fields, two dropdowns, two enumerations;
	// gst.ListOutputDevices fills this one.
	//
	// # A config.json carried between a Windows machine and a Mac
	//
	// This is the one field where that is not merely untidy: the value is a valid
	// identity on the machine it was written on and meaningless on the other. It
	// must fail SAFE, and it does. gst.chooseOutputDevice checks the saved id
	// against what the machine is actually offering, falls back to the default
	// playback device, and logs the id, the reason, and what IS on offer instead.
	// A monitor in the wrong ears is diagnosable in a second; no monitor at all is
	// a commentary position working blind.
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
	// It is in the OS credential store under secrets.TargetSRTReturn
	// ("WSLComms/srtreturn") — the same target string on both platforms, in
	// Credential Manager or the login Keychain — reached through
	// internal/secrets, exactly as the M2L-X password and the send path's
	// passphrase are. config.json is written to the per-user application data
	// directory in plain text, is hand-editable by design, and is the first
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

	// DefaultVideoBitrateKbps is the H.264 target bitrate for the video leg, in
	// KILOBITS per second. 2000 is not a new decision: it is the value
	// internal/gst has been substituting for an unset bitrate since the video
	// leg was a still slate, so a build with this field added encodes exactly as
	// the build without it did until an operator changes the number.
	//
	// It restates internal/gst.DefaultVideoBitrateKbps rather than importing it,
	// for the reason DefaultPictureLatencyMs and the ReturnChannel constants do:
	// a configuration package that cannot be tested without GStreamer is a
	// configuration package that stops being tested. The two cannot drift into
	// anything dangerous, and it is worth saying why rather than trusting it —
	// Defaults() now sets this field, and EffectivePictureLatencyMs's sibling
	// EffectiveVideoBitrateKbps substitutes it for a zero, so the application
	// path always hands internal/gst an explicit non-zero bitrate. Its own
	// default is reachable only by a caller that omits the option, which means
	// the stub twin and its tests.
	//
	// MEASURED on macOS: bitrate=2000 produced a 2.05 Mbit/s video PID, which is
	// the confirmation that the unit really is kilobits on vtenc_h264 as well as
	// on mfh264enc (internal/gst/gst_cgo.go, applyEncoderProperties).
	DefaultVideoBitrateKbps = 2000

	// MaxVideoBitrateKbps is the largest video bitrate Validate accepts:
	// 100 Mbit/s expressed in the field's own unit.
	//
	// It is a TYPO GUARD, not an engineering limit, and it is set where it is so
	// that it can only ever catch one. The operator wants around 10000; a stray
	// extra digit gives 100000, which is a number no contribution circuit will
	// carry and which — unlike a bad port or a bad key length — fails by
	// saturating the uplink and taking the feed with it rather than by refusing
	// to start. Ten times the largest figure anybody has asked for is far enough
	// away that refusing above it cannot refuse a setting somebody meant.
	MaxVideoBitrateKbps = 100000

	// The two commentary capture kinds. They are the strings stored in
	// audioSourceKind and the values of the Settings screen's control.
	//
	// AudioSourceNative is the platform's own audio API — wasapi2src on Windows,
	// osxaudiosrc on macOS — reading the endpoint named by audioDeviceId.
	AudioSourceNative = "native"
	// AudioSourceDeckLink is decklinkaudiosrc, reading the card named by
	// decklinkPersistentId. It exists because the CoreAudio device a Blackmagic
	// card publishes is not the one carrying the microphone: the app measured
	// -96 dBFS on all 16 channels of "Blackmagic UltraStudio 4K Mini" with the
	// mic live, because the device that does carry it publishes no unique-id and
	// is therefore skipped by the dropdown (deviceprovider_darwin.go). Capturing
	// through the card's own element is the way to that audio.
	AudioSourceDeckLink = "decklink"

	// DefaultAudioSourceKind is AudioSourceNative. Changing this default would
	// change which subsystem a machine captures from on its next launch without
	// anyone asking for it — see DefaultReturnSource for the same rule applied
	// to the return path.
	DefaultAudioSourceKind = AudioSourceNative

	// DefaultReturnMid is the transceiver mid routed to the headphones: mid 4,
	// MIC1 — the mix-minus feed the operator labels "Monitor 1" and chose as the
	// default return.
	DefaultReturnMid = 4

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
		// Today's effective bitrate, stated. See the field comment: the point of
		// this default is that adding the control changes nothing.
		VideoBitrateKbps: DefaultVideoBitrateKbps,
		// "native" is a real value rather than the zero value, so it has to be
		// written here or a fresh config.json would carry an empty kind that only
		// EffectiveAudioSourceKind makes sense of.
		AudioSourceKind: DefaultAudioSourceKind,
		// videoFormatOverride and decklinkPersistentId are deliberately absent.
		// Both are documented as MEANINGFUL when empty — "derive the format from
		// the switcher" and "the only card in this machine" — so unlike
		// srtReturnPBKeyLen above there is nothing a table of defaults could say
		// about them that the zero value does not already say correctly.
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

// EffectiveSRTHost returns the host this application dials for SRT: always the
// M2L-X host with any scheme, path and port stripped off. There is no longer a
// separate srtHost override — see the note where the field used to be.
//
// The name and signature are kept so the four call sites (the send path,
// app_picture, app_return, internal/gst) need no change; it is simply now a
// one-line derivation.
//
// M2LXHost may carry an explicit "http://" or "https://" prefix (internal/m2lx
// resolveHost accepts one, and cmd/mockm2lx needs it) and a port. Neither
// belongs in the host half of the srt:// URI internal/gst builds, so both are
// removed. An IPv6 literal keeps its brackets, because that URI needs them.
func (c *Config) EffectiveSRTHost() string {
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

// EffectiveVideoBitrateKbps returns the H.264 target bitrate the contribution
// feed encodes at, substituting DefaultVideoBitrateKbps for a zero or negative
// value.
//
// Zero and "unset" are the same thing here, as they are for the return port and
// the picture latency: every config.json written before this field existed has
// no key at all (Load gives those the default) and a hand-edited or older-build
// file can hold an explicit 0, which is not a bitrate anybody wants — it is what
// internal/gst already substitutes the default FOR. Doing the substitution here
// as well means the number the application hands to the encoder is the number
// Validate checked, rather than a zero that turns into 2000 two packages later.
//
// A NEGATIVE value is substituted rather than passed on, even though Validate
// refuses one: this accessor is also reached from a config that never went
// through Validate (a hand-edited file, a preset), and internal/gst's response
// to a negative bitrate is to refuse to build the pipeline at all.
func (c *Config) EffectiveVideoBitrateKbps() int {
	if c.VideoBitrateKbps > 0 {
		return c.VideoBitrateKbps
	}
	return DefaultVideoBitrateKbps
}

// EffectiveAudioSourceKind returns the configured commentary capture kind,
// substituting DefaultAudioSourceKind for an empty value.
//
// Load already substitutes defaults for keys ABSENT from config.json, but an
// explicitly empty string survives — a hand-edited file, or a preset applied
// over a config written by a build that had no such field. "native" is the
// answer in both cases: it is what the machine was doing before anybody touched
// the file. Compare EffectiveReturnSource, which exists for the same reason.
func (c *Config) EffectiveAudioSourceKind() string {
	if s := strings.TrimSpace(c.AudioSourceKind); s != "" {
		return s
	}
	return DefaultAudioSourceKind
}

// UsesDeckLinkAudio reports whether commentary audio is captured from a
// Blackmagic card rather than from a platform audio endpoint.
//
// It is the one question the rest of the application asks about
// audioSourceKind — which capture element to build, and whether audioDeviceId
// means anything on this machine — so it is answered once, here, rather than by
// string comparison at each call site. Mirrors UsesSRTReturn above.
func (c *Config) UsesDeckLinkAudio() bool {
	return c.EffectiveAudioSourceKind() == AudioSourceDeckLink
}

// VideoFormatOverrideSpec parses videoFormatOverride.
//
// The three answers are distinct and every caller has to tell them apart:
//
//	("", false, nil)     no override is set: DERIVE the format from the
//	                     switcher, which is the normal and default case.
//	(spec, true, nil)    conform to spec.
//	("", false, err)     a value is set and cannot be parsed. Validate refuses
//	                     this before Start, so a caller reaching it has a config
//	                     that never went through Validate — a hand-edited file
//	                     or a preset from a newer build. It must be reported,
//	                     never silently treated as "derive": deriving from a
//	                     switcher with nothing streaming produces the 1080p50
//	                     guess this field exists to replace, and doing that
//	                     quietly is how the operator ends up watching a feed
//	                     that will not negotiate with no idea why.
func (c *Config) VideoFormatOverrideSpec() (spec VideoFormatSpec, ok bool, err error) {
	raw := strings.TrimSpace(c.VideoFormatOverride)
	if raw == "" {
		return VideoFormatSpec{}, false, nil
	}
	spec, err = ParseVideoFormat(raw)
	if err != nil {
		return VideoFormatSpec{}, false, err
	}
	return spec, true, nil
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
			"the SRT return has no host to dial: set m2lxHost"))
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
// Required non-empty fields: m2lxHost, alias, eventId, and an EffectiveSRTHost
// — which m2lxHost alone satisfies. audioDeviceId is required TOO, but only for
// a native capture: see the note beside that check. srtPort must be a valid
// TCP/UDP port, 1..65535. pbkeylen must be 0 (no passphrase negotiated), 16 or
// 32 — the only key lengths SRT's AES-CTR supports. returnMid must be 1..7, the
// range of transceiver mids the KVS signalling channel can address.
// audioSourceKind must be one of the two capture kinds. videoBitrateKbps must be
// 0 (meaning the default) or within MaxVideoBitrateKbps. videoFormatOverride, if
// it is set at all, must be a format ParseVideoFormat can read.
//
// Deliberately NOT required: statusKey, which only names the node the three
// WebSocket-derived lamps read (see the field comment). It is not needed to put
// a feed on air, and requiring it once made the app unstartable until the
// operator had guessed a value that nothing in the API can tell them. Nor
// decklinkPersistentId, which is empty on the single-card machine that is the
// normal case, and would be the same mistake with different hardware.
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
	}
	for _, f := range required {
		if strings.TrimSpace(f.value) == "" {
			errs = append(errs, fmt.Errorf("%s is required", f.name))
		}
	}

	// audioSourceKind decides which capture element Start builds, so an
	// unrecognised one has no pipeline behind it at all.
	switch c.EffectiveAudioSourceKind() {
	case AudioSourceNative, AudioSourceDeckLink:
	default:
		errs = append(errs, fmt.Errorf("audioSourceKind must be %q or %q, got %q",
			AudioSourceNative, AudioSourceDeckLink, c.AudioSourceKind))
	}

	// audioDeviceId is required for a NATIVE capture and meaningless for a
	// DeckLink one, so the requirement follows the kind.
	//
	// This used to be unconditional, in the table above, and leaving it there
	// would have made the DeckLink seat unstartable: its audio comes from
	// decklinkaudiosrc, no CoreAudio or WASAPI endpoint is ever opened, and the
	// operator would have been made to pick an irrelevant device from a dropdown
	// to satisfy a check about a subsystem their commentary is not going
	// through. Note what is NOT required in the DeckLink branch either —
	// decklinkPersistentId, which is empty on the single-card machine that is
	// the normal case; see that field's comment.
	//
	// It stays required for a native capture for the reason it always was: it is
	// the one device field Start genuinely cannot proceed without, and an empty
	// one otherwise fails inside GStreamer twenty seconds late, blaming the
	// network.
	if !c.UsesDeckLinkAudio() && strings.TrimSpace(c.AudioDeviceID) == "" {
		errs = append(errs, errors.New("audioDeviceId is required"))
	}

	// EffectiveSRTHost is empty exactly when m2lxHost is, which the required
	// check above already reports — so there is no separate SRT-host error to
	// add here any more (there is no srtHost field to be the other cause).

	if c.SRTPort < 1 || c.SRTPort > 65535 {
		errs = append(errs, fmt.Errorf("srtPort must be between 1 and 65535, got %d", c.SRTPort))
	}

	if c.PBKeyLen != 0 && c.PBKeyLen != 16 && c.PBKeyLen != 32 {
		errs = append(errs, fmt.Errorf("pbkeylen must be 0, 16 or 32, got %d", c.PBKeyLen))
	}

	if c.ReturnMid < 1 || c.ReturnMid > 7 {
		errs = append(errs, fmt.Errorf("returnMid must be between 1 and 7, got %d", c.ReturnMid))
	}

	// The video leg's bitrate. Zero is accepted and means the default (see
	// EffectiveVideoBitrateKbps, and note that every config.json written before
	// this field existed has no key at all); a negative one is refused here
	// rather than by internal/gst, which would otherwise report it as a pipeline
	// failure after the operator has pressed START. The upper bound is a typo
	// guard — see MaxVideoBitrateKbps for why it is set as far out as it is.
	if k := c.VideoBitrateKbps; k < 0 || k > MaxVideoBitrateKbps {
		errs = append(errs, fmt.Errorf(
			"videoBitrateKbps must be between 0 (the default, %d) and %d kilobits per second, got %d",
			DefaultVideoBitrateKbps, MaxVideoBitrateKbps, k))
	}

	// ================ THE ONE CHECK THAT MUST NOT BE SOFTENED ================
	//
	// A videoFormatOverride this application cannot parse is refused HERE, with
	// the field's name, the value that was typed and the accepted form in the
	// message — because the alternative is what it replaces. Without this check
	// an unparseable override reaches the capsfilter on the video leg, and a
	// capsfilter that cannot negotiate fails as
	// "Internal data stream error / not-negotiated (-4)": a message that names
	// no field, no value and no cause, arriving several seconds after START,
	// with a commentator waiting. An error that says which box to go and fix is
	// worth more than every other consideration on this line.
	//
	// It is in Validate — the gate on Start — and not in ValidateReturn, and
	// that is a deliberate exception to the rule that nothing in Validate may be
	// a reason a match does not go out. The rule protects settings whose failure
	// would be someone else's: statusKey, the monitor fields, the picture
	// latency. This one is not like those. A bad value here means the
	// contribution feed CANNOT be built, so the match does not go out either
	// way; the only thing in question is whether the operator is told why, at
	// the moment they can still fix it, or twenty seconds later by a message
	// about a data stream.
	//
	// EMPTY IS NOT AN ERROR and never becomes one. Empty means derive, it is the
	// default, and it is what every existing installation holds.
	if raw := strings.TrimSpace(c.VideoFormatOverride); raw != "" {
		if _, err := ParseVideoFormat(raw); err != nil {
			errs = append(errs, fmt.Errorf("videoFormatOverride: %w", err))
		}
	}

	return errors.Join(errs...)
}
