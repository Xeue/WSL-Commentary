// Package presets stores and applies M2L-X INSTANCE PRESETS: one deployment's
// coordinates — host, event, ports, keys' lengths, the measured layout facts —
// saved as a file of its own under %APPDATA%\WSLComms\presets and applied onto
// the live configuration as a MERGE.
//
// Owner: presets work package. It deliberately lives outside internal/config,
// which CONTRACT.md gives exclusively to WP-1: this package imports config and
// reflects over config.Config without editing a byte of WP-1's files.
//
// # The one guarantee everything here exists to make true by construction
//
// A preset never carries a device id, a filesystem path, or a live monitoring
// choice. Not "should never" — CANNOT. Extract builds a preset's fields from
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

// Class is a config field's classification: which of the three tables its json
// tag appears in. The decision rule, stated once so future fields classify
// themselves:
//
//   - INSTANCE — a property of the M2L-X deployment/event. The same for anyone
//     who plugs into it; wrong on any other instance. Travels in a preset.
//   - MACHINE — names hardware or a filesystem path on THIS PC (or this
//     WebView profile). Never travels; a MACHINE value from another machine is
//     a fault that arrives by post.
//   - UI — a live-operational choice by the person at the desk right now.
//     Never travels, because a preset applied from a configuration screen must
//     not change what is in a commentator's ears or on their screen as a side
//     effect.
type Class string

// The three classes. TestEveryConfigFieldIsClassified fails, by field name, on
// any config.Config field that is in none of them — or in more than one.
const (
	ClassInstance Class = "instance"
	ClassMachine  Class = "machine"
	ClassUI       Class = "ui"
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
	// The KVS webrtc_info/webrtc_token event.
	"eventId",
	// Override for that instance's SRT ingest name (empty follows m2lxHost).
	"srtHost",
	// That instance's commentary ingest port.
	"srtPort",
	// Retransmission budget for the path to that instance.
	"srtLatencyMs",
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
}

// classes is the lookup the public helpers answer from, built once. A tag in
// two tables panics at init rather than silently answering with one of them —
// a double classification is a bug in this file, not a runtime condition.
var classes = func() map[string]Class {
	m := make(map[string]Class, len(InstanceFields)+len(MachineFields)+len(UIFields))
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
