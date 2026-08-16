// Package presets stores and applies M2L-X INSTANCE PRESETS: one deployment's
// coordinates — host, ports, keys' lengths, the measured layout facts — saved
// as a file of its own under %APPDATA%\WSLComms\presets and applied onto the
// live configuration as a MERGE.
//
// Owner: presets work package. It deliberately lives outside internal/config,
// which CONTRACT.md gives exclusively to WP-1: this package imports config and
// reflects over config.Config without editing a byte of WP-1's files.
//
// # The one guarantee everything here exists to make true by construction
//
// A preset never carries a device id, a filesystem path, a live monitoring
// choice, or a value the app can discover from the instance itself (the event
// id). Not "should never" — CANNOT. Extract builds a preset's fields from
// the explicit InstanceFields whitelist below, Filter drops anything else from
// a file that arrived by mail or by hand-editing, and Apply unmarshals the
// surviving keys onto the EXISTING *config.Config, so a field a preset omits is
// physically incapable of being overwritten. That is the same primitive
// config.Load already uses (internal/config/config.go, the comment above Load):
// encoding/json only writes fields present in the source.
//
// Why it matters this much: a MACHINE value from another machine is the live
// "Failed to open device {0.0.0.00000000}.{8678ce58-…}" fault, delivered by
// post — a config.json (or a preset) copied between PCs carrying a WASAPI GUID
// the receiving machine has never seen. audioDeviceId is also the only device
// field config.Validate requires, so a preset carrying it would make a machine
// that has never seen that endpoint pass validation and then fail inside
// GStreamer, twenty seconds late, blaming the network.
package presets

import (
	"encoding/json"
	"fmt"
	"sort"

	"wslcomms/internal/config"
)

// Class is a config field's classification: which of the four tables its json
// tag appears in. The decision rule, stated once so future fields classify
// themselves — and note that the three "never travels" classes differ by WHO
// the authority for the value is, not by how inconvenient it would be to get
// wrong:
//
//   - INSTANCE — a property of the M2L-X deployment. The same for anyone who
//     plugs into it; wrong on any other instance. Travels in a preset.
//   - MACHINE — names hardware or a filesystem path on THIS PC (or this
//     WebView profile). Never travels; a MACHINE value from another machine is
//     a fault that arrives by post.
//   - UI — a live-operational choice by the person at the desk right now.
//     Never travels, because a preset applied from a configuration screen must
//     not change what is in a commentator's ears or on their screen as a side
//     effect.
//   - DISCOVERED — a value the application LEARNS from the instance at
//     runtime. Never travels, because the instance is the authority and a
//     remembered copy is a stale answer wearing a confident face.
type Class string

// The four classes. TestEveryConfigFieldIsClassified fails, by field name, on
// any config.Config field that is in none of them — or in more than one.
const (
	ClassInstance   Class = "instance"
	ClassMachine    Class = "machine"
	ClassUI         Class = "ui"
	ClassDiscovered Class = "discovered"
)

// InstanceFields is the WHITELIST: the json tags of config.Config that a
// preset carries. It is an explicit literal slice, never "copy the config and
// delete four keys" — a blacklist silently admits every field added later,
// which is exactly how audioDeviceId would eventually get in. A new field is
// OUT until somebody classifies it here, and the reflection test is what makes
// them notice the question was asked.
var InstanceFields = []string{
	// The deployment's address; the REST base and the status WebSocket derive
	// from it.
	"m2lxHost",
	// The sign-in name on that deployment.
	"alias",
	// That instance's commentary ingest port. (There is no srtHost field: the
	// SRT host is always derived from m2lxHost — see config.EffectiveSRTHost.)
	"srtPort",
	// Retransmission budget for the path to that instance.
	"srtLatencyMs",
	// How much of the circuit to that instance the feed may take. The same kind
	// of fact as the two above it: a venue's contribution path is a venue's
	// contribution path, whichever laptop is plugged into it. (2000 was chosen
	// when the video leg was a still slate; live video wants nearer 10000, and
	// which of those is right is a property of the link, not of the PC.)
	"videoBitrateKbps",
	// The format that instance's switcher is configured for, as the fallback for
	// when no node is streaming and it cannot be derived — MEASURED as the
	// normal case on a position that comes up first: every node reports
	// "format": null while stopped, and no REST endpoint states it. It describes
	// the SWITCHER, so every position at the facility shares one answer, which is
	// exactly what a preset is for.
	"videoFormatOverride",
	// Encryption negotiated with that ingest — the NON-SECRET half. The
	// passphrase itself lives in Credential Manager under the preset's
	// credential scope and never enters a preset file; see presets.go and
	// TestSaveNeverWritesSecretFields.
	"pbkeylen",
	// Per-instance AND per-patch, and nothing in the API can supply it
	// (config.go's field comment) — the single biggest saving a preset offers.
	"statusKey",
	// Which M2L-X OUTPUT to dial for the return/picture.
	"srtReturnPort",
	// Encryption is per output on M2L-X, so this travels with the port or not
	// at all.
	"srtReturnPBKeyLen",
	// Dominated by the far end's Buffer (msec) on that output.
	"pictureLatencyMs",
	// Which transceiver mid is aux1/CLN on that instance.
	"returnMid",
	// "An M2L-X layout fact measured on the dev event" — and it merges
	// field-by-field, because Apply is the same unmarshal-onto-live-struct
	// primitive config.Load uses (TestApplyPartialMonitorTileMergesFieldByField).
	"monitorTile",
	// A measured M2L-X level offset, not a property of this app.
	"returnGainDb",
}

// MachineFields are the json tags that name THIS PC's hardware or filesystem
// and must never travel. Listed by name rather than derived, so that the tests
// asserting their absence from a preset file can be read against this table.
var MachineFields = []string{
	// A WASAPI IMMDevice GUID, passed verbatim to wasapi2src with no existence
	// check — and the only device field config.Validate requires, so a preset
	// carrying it fails INSIDE GStreamer rather than at Validate.
	"audioDeviceId",
	// A browser mediaDeviceId: per-origin, per-session salted hash, meaningless
	// even in another WebView2 profile on the SAME machine.
	"headphoneDeviceId",
	// A WASAPI GUID for wasapi2sink; failure is SILENT — it falls back to the
	// default endpoint. Wrong ears, no error.
	"headphoneEndpointId",
	// A path on this installation.
	"slatePath",
	// WHICH SUBSYSTEM THIS PC CAPTURES COMMENTARY FROM: "native" or "decklink".
	// It is the audioDeviceId failure in a different spelling — a preset
	// carrying "decklink", applied to a laptop with no card in it, describes
	// hardware that is not there — and the fault it produces is worse than a
	// phantom endpoint, not better: the card's absence surfaces from GStreamer
	// as "Internal data stream error / not-negotiated (-4)" in about 100
	// microseconds, naming neither the device nor the cause.
	"audioSourceKind",
	// WHAT THIS PC PUTS ON THE VIDEO LEG: "slate" or "decklink". It is the video
	// twin of audioSourceKind above, and it is the hardest classification in this
	// file. The argument is written out rather than asserted, because the case
	// against is real and the next person to read this table will find it for
	// themselves if it is not here.
	//
	// THE CASE FOR UI, which is not weak. This field decides WHAT GOES ON AIR.
	// UIFields exists precisely to stop a preset, applied from a configuration
	// screen, changing something live as a side effect — and there is no more
	// live thing in this application than the picture the switcher is receiving.
	// A preset that could switch a position from a slate to a camera would be the
	// returnSource fault with the stakes raised: not "the wrong thing in one
	// person's ears" but "the wrong thing on the broadcast".
	//
	// THE CASE FOR MACHINE, which wins. The four classes are separated by WHO THE
	// AUTHORITY FOR THE VALUE IS, not by how much it would hurt to be wrong —
	// that rule is stated at the top of this file and it decides this. The
	// authority for "is there a camera cabled into this position" is this PC and
	// nothing else. A venue preset cannot know it, because two seats at the same
	// facility, sharing every instance coordinate, routinely differ on it. And
	// hardware presence is a BINDING constraint rather than a preference: a
	// preset switching a position to a camera it does not have is not merely
	// wrong, it is UNSTARTABLE — an absent or busy card surfaces from GStreamer as
	// not-negotiated (-4) in about 100 microseconds, naming neither the device nor
	// the cause. It is audioSourceKind's fault exactly, on the leg where the
	// failure is louder, and putting the two halves of one question in two
	// different tables would be two answers to it.
	//
	// AND THE THING THAT MAKES THE CHOICE SAFE RATHER THAN MERELY DEFENSIBLE:
	// both classes forbid travel absolutely, so nothing about the on-air risk
	// turns on which of the two this row is in. The risk is closed where it
	// arises instead — App.SetVideoSource is HOST-ONLY, so no remote seat can
	// change what goes to air, and App.preflightCapture refuses at Start, naming
	// this field and the card, rather than twenty seconds later naming nothing.
	// A classification is not a place to fix an authorisation problem.
	"videoSource",
	// The persistent-id of a Blackmagic card in THIS PC — hardware, by
	// definition. One field serves audio and video because the id names the
	// CARD: both elements publish the same one. It is stored as the
	// persistent-id and never as the device-number, the same rule that stores
	// the CoreAudio UID and never the integer AudioDeviceID, and for the same
	// reason: an index into this boot's enumeration order is not an identity, so
	// it is not a thing that could travel even in principle.
	"decklinkPersistentId",
	// WHICH OF THAT CARD'S CHANNELS CARRY THE COMMENTATOR. It is the wiring of
	// the room this PC is in — which XLR goes to which embedder input — and a
	// preset carrying it would re-route somebody's microphone in a different
	// building from a configuration screen, with the routing screen still
	// showing what the operator there had chosen. The failure is worse than the
	// two above rather than milder: a wrong capture kind or a wrong card fails
	// loudly and does not go on air at all, whereas a wrong ROUTING starts
	// perfectly, shows every lamp green and carries the wrong channel — or
	// silence — for as long as nobody listens.
	"decklinkChannelMap",
}

// UIFields are the json tags of live-operational choices. Instance-DERIVED is
// not a licence to travel: returnChannel reflects that event's CLN routing,
// but it reaches the live Web Audio graph the moment it is applied, and a
// preset must not change somebody's monitoring from a configuration form.
var UIFields = []string{
	// Decides what is in the ears NOW; settings.js already forbids a config
	// screen changing it as a side effect, and app.js overrules it to "webrtc"
	// every launch anyway.
	"returnSource",
	// Reaches the live Web Audio graph via onConfigSaved's setChannelMode.
	"returnChannel",
	// Whether the operator's own confidence monitor is on their screen. It is
	// returnSource's class exactly — live monitoring, chosen by the person at the
	// desk, for the desk they are at — and it is here rather than beside
	// videoSource in MachineFields because it changes NOTHING about what is
	// transmitted: the preview is a tee off the capture that is already running,
	// and turning it off does not alter one byte the switcher receives.
	//
	// What a preset carrying it would do is open or close a window on somebody
	// else's screen, in another building, from a configuration form — over
	// whatever they were looking at, which on this application's home screen is
	// the picture the commentator is watching. That is the same defect
	// returnSource is on this list for, in the other sense.
	"decklinkPreviewEnabled",
}

// DiscoveredFields are the json tags of values the application LEARNS from the
// instance at runtime, and which therefore must never be carried in a preset.
// What separates this class from MACHINE and UI is where the authority for the
// value lives: a machine field belongs to this PC, a UI field to the person at
// the desk, and a discovered field to the M2L-X itself — so the only correct
// way to hold one is to ask for it, and the only way to be wrong about one is
// to remember it.
//
// eventId is the first member and the reason the class exists. A preset answers
// WHICH VENUE; eventId names WHICH MATCH. Two fixtures at the same ground are
// two event ids against one identical set of instance coordinates, so a preset
// carrying it is right on the afternoon it was saved and quietly wrong every
// time afterwards: the KVS monitor dials last week's signalling channel while
// every field on the Settings screen reads exactly as the operator expects, and
// the failure surfaces as "the picture never came up" rather than as a wrong
// value anybody can see. Since internal/m2lx/events.go's ListEvents
// (GET /api/events/overview) the application can simply ask a signed-in
// instance for its events — auto-selecting when there is exactly one — so
// remembering the id buys nothing and costs that.
//
// What would be WRONG to put here: this is not "fields the app happens to fetch
// from somewhere", and it is emphatically not a bin for a field nobody wants to
// classify. A tag belongs in this table only when the INSTANCE is the authority
// for its value AND there is a live call that returns it; without the call,
// "discovered" is a promise the app cannot keep and the field is really an
// instance coordinate the operator must configure. statusKey is exactly that
// case — per-instance, per-patch, and nothing in the API supplies it — which is
// why it stays on the whitelist above.
//
// Listed LAST of the four on purpose: frontend/src/ui/presets.test.js reads
// this file and takes the whitelist to be the text between "var InstanceFields"
// and "// MachineFields", so a table wedged between those two markers would be
// read as part of it.
var DiscoveredFields = []string{
	// The KVS webrtc_info/webrtc_token event — which MATCH, never which venue.
	"eventId",
}

// classes is the lookup the public helpers answer from, built once. A tag in
// two tables panics at init rather than silently answering with one of them —
// a double classification is a bug in this file, not a runtime condition.
var classes = func() map[string]Class {
	m := make(map[string]Class, len(InstanceFields)+len(MachineFields)+len(UIFields)+len(DiscoveredFields))
	add := func(tags []string, c Class) {
		for _, t := range tags {
			if prev, ok := m[t]; ok {
				panic(fmt.Sprintf("presets: %q classified as both %q and %q", t, prev, c))
			}
			m[t] = c
		}
	}
	add(InstanceFields, ClassInstance)
	add(MachineFields, ClassMachine)
	add(UIFields, ClassUI)
	add(DiscoveredFields, ClassDiscovered)
	return m
}()

// Classify returns a json tag's class, and whether it is classified at all. An
// unclassified tag is a config.Config field added after this table was written
// — the reflection test fails on it by name, so "false" here should never be
// reachable from a tag that config actually serialises.
func Classify(tag string) (Class, bool) {
	c, ok := classes[tag]
	return c, ok
}

// IsInstanceField reports whether tag is on the whitelist — whether a preset
// may carry it.
func IsInstanceField(tag string) bool {
	return classes[tag] == ClassInstance
}

// dropDiscoveredFields deletes every DISCOVERED key from a preset's fields, in
// place. It is what makes the preset files already on operators' machines
// harmless: builds up to this one wrote eventId into every preset they saved,
// so a working profile has several files carrying a match id from whenever they
// were last saved.
//
// Dropping the key on LOAD rather than reporting it is the deliberate half.
// Filter's ignored list is the "somebody hand-edited this / it came from a
// different build" channel, and it reaches the operator as a banner; a key this
// application's own previous version wrote into every file it saved does not
// belong on that channel, or the first preset of every match day starts with a
// warning about a key nobody typed. Silence is right here precisely because the
// value is not being half-honoured — it is gone before Filter, before Apply and
// before the Summary the picker diffs against, so no path can re-apply it.
//
// It is called from Load and mutates the map json.Unmarshal has just built for
// that one Preset value, which is why mutating in place is safe: no caller has
// seen the map yet, and nothing else shares it.
func dropDiscoveredFields(fields map[string]json.RawMessage) {
	for tag := range fields {
		if classes[tag] == ClassDiscovered {
			delete(fields, tag)
		}
	}
}

// Extract builds a preset's fields from the live configuration: marshal the
// whole config, then copy across ONLY the whitelisted keys.
//
// Going through JSON rather than reflecting field-by-field means the values in
// a preset file are byte-compatible with config.json by construction — same
// tags, same encodings — so Apply is one Unmarshal with no translation table
// to drift.
func Extract(cfg *config.Config) (map[string]json.RawMessage, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("presets: encoding the configuration: %w", err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("presets: re-reading the encoded configuration: %w", err)
	}

	out := make(map[string]json.RawMessage, len(InstanceFields))
	for _, tag := range InstanceFields {
		if v, ok := all[tag]; ok {
			out[tag] = v
		}
	}
	return out, nil
}

// Filter drops every key that is not on the whitelist and REPORTS what it
// dropped, sorted, so the operator can be told rather than left to wonder.
//
// It exists for the file that did not come from Extract: a preset mailed from
// a colleague's older build, or hand-edited to carry a device id. Refusing
// such a file outright would punish the fields that are fine; silently
// accepting it would deliver the phantom-endpoint fault this package exists to
// prevent. Neutering the bad keys and naming them is the middle that serves.
//
// A DISCOVERED key that reaches here has bypassed Load — a Preset assembled in
// memory rather than read from disk — and is treated like any other key off the
// whitelist: dropped, and named. That is the honest answer for a caller who
// built the map itself. The quiet path for the eventId already sitting in the
// files on operators' machines is dropDiscoveredFields, which runs inside Load
// and is documented there.
func Filter(fields map[string]json.RawMessage) (kept map[string]json.RawMessage, ignored []string) {
	kept = make(map[string]json.RawMessage, len(fields))
	for k, v := range fields {
		if IsInstanceField(k) {
			kept[k] = v
			continue
		}
		ignored = append(ignored, k)
	}
	sort.Strings(ignored)
	return kept, ignored
}

// Apply merges fields onto the LIVE struct: filter, re-marshal the kept map,
// unmarshal onto cfg. It returns the keys Filter dropped.
//
// Unmarshal-onto-the-existing-struct is the load-bearing choice, and it is the
// primitive config.Load already uses: keys absent from the source cannot
// overwrite anything, so the operator's audioDeviceId survives even if every
// other defence failed, and a partial monitorTile merges field-by-field
// exactly as config's TestLoad_PartialNestedTileMergesFieldByField documents.
//
// It is also what makes dropping eventId from the whitelist safe rather than
// destructive: a preset that does not mention the field is incapable of
// blanking it, so applying one keeps whichever event the operator (or the
// events picker) has already chosen on this machine.
func Apply(cfg *config.Config, fields map[string]json.RawMessage) (ignored []string, err error) {
	kept, ignored := Filter(fields)
	data, err := json.Marshal(kept)
	if err != nil {
		return ignored, fmt.Errorf("presets: encoding the preset's fields: %w", err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return ignored, fmt.Errorf("presets: applying the preset onto the configuration: %w", err)
	}
	return ignored, nil
}
