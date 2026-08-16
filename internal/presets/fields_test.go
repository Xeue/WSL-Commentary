package presets

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"wslcomms/internal/config"
)

// configJSONTags reflects the top-level json tags out of config.Config, so the
// classification tests are driven by what config ACTUALLY serialises rather
// than by a second hand-written list that could drift alongside the first.
func configJSONTags(t *testing.T) []string {
	t.Helper()
	var tags []string
	typ := reflect.TypeOf(config.Config{})
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		tags = append(tags, strings.Split(tag, ",")[0])
	}
	return tags
}

// classificationTables is every table a config tag may be classified into, in
// the order fields.go declares them. The reflection tests iterate THIS, so
// adding a fifth class is one edit here rather than four scattered literals —
// and, more to the point, a class added without touching this line leaves the
// reflection test still demanding exactly one of the OLD tables, which is the
// failure that tempts somebody to weaken the test instead of extending it.
func classificationTables() [][]string {
	return [][]string{InstanceFields, MachineFields, UIFields, DiscoveredFields}
}

// TestEveryConfigFieldIsClassified is the test that makes the classification
// SURVIVE. Without it the next field added to config.Config is unclassified by
// default, and whoever adds it never learns the question — instance, machine,
// UI or discovered? — was ever asked. It fails WITH THE FIELD NAME, so the
// failure reads as the question.
func TestEveryConfigFieldIsClassified(t *testing.T) {
	tags := configJSONTags(t)
	if len(tags) == 0 {
		t.Fatal("reflected no json tags out of config.Config; the reflection is broken, not the config")
	}

	for _, tag := range tags {
		count := 0
		for _, table := range classificationTables() {
			if slices.Contains(table, tag) {
				count++
			}
		}
		switch count {
		case 1:
			// Classified exactly once: correct.
		case 0:
			t.Errorf("config.Config field %q is UNCLASSIFIED: add it to exactly one of "+
				"InstanceFields (travels in a preset), MachineFields (this PC's hardware or "+
				"filesystem, never travels), UIFields (a live-operational choice, never travels) "+
				"or DiscoveredFields (the app learns it from the instance, so a preset must not "+
				"remember it) in internal/presets/fields.go — the decision rule is in that file's "+
				"comments", tag)
		default:
			t.Errorf("config.Config field %q appears in %d classification tables; it must be in exactly one", tag, count)
		}
	}

	// And the reverse: a classified tag that config no longer serialises is a
	// stale row that would silently stop matching anything.
	for _, table := range classificationTables() {
		for _, tag := range table {
			if !slices.Contains(tags, tag) {
				t.Errorf("classification table names %q, which is not a json tag of config.Config any more", tag)
			}
		}
	}
}

// TestClassificationCounts pins the 14 + 8 + 3 + 1 = 26 split (srtHost was
// removed — the SRT host is always derived from m2lxHost — and eventId moved
// from the whitelist to DiscoveredFields), so growth in any table is a
// deliberate, reviewed change.
//
// The two added with the DeckLink VIDEO leg went to different tables on purpose
// and the split is the reviewed decision this count exists to protect.
// videoSource is the EIGHTH machine field: it is audioSourceKind's twin on the
// other leg, the authority for it is this PC's cabling, and a preset carrying it
// would describe a camera that is not there — unstartable, not merely wrong.
// decklinkPreviewEnabled is the THIRD ui field: it changes nothing that is
// transmitted and everything about what is on the operator's own screen, which
// is returnSource's class exactly.
//
// The four that moved these numbers, and which way each went:
// videoBitrateKbps and videoFormatOverride are INSTANCE — one is how much of
// the circuit to that deployment the feed may take, the other is how that
// deployment's switcher is configured, and every position at a facility shares
// both. audioSourceKind and decklinkPersistentId are MACHINE — which subsystem
// this PC captures from, and which card in it — and a preset carrying either
// would deliver another machine's hardware by post.
//
// The seventh MACHINE field is decklinkChannelMap, and it is the sharpest case
// on the list. The other six fail LOUDLY when they arrive somewhere they are not
// true — a missing device or an absent card does not go on air at all — whereas
// a routing from another building starts perfectly, shows every lamp green, and
// carries the wrong channel or silence until somebody listens.
//
// The fourth UI field is coughMuteMode: whether the cough control is held down
// or latched. It is one step FURTHER from the wire than the other three — it
// changes neither what is transmitted nor what is monitored, only what a button
// does under a finger — and a preset carrying it would change the behaviour of
// a control somebody is about to use under pressure without changing anything
// they can see.
//
// It is also the field to read this file's UIFields comment beside, because the
// paragraph under it is where the decision NOT to have a "coughMuted" field at
// all is argued. The mute is live state, it is false on every path that begins
// a session, and it is on none of these four tables on purpose.
func TestClassificationCounts(t *testing.T) {
	if got := len(InstanceFields); got != 14 {
		t.Errorf("len(InstanceFields) = %d, want 14", got)
	}
	if got := len(MachineFields); got != 8 {
		t.Errorf("len(MachineFields) = %d, want 8", got)
	}
	if got := len(UIFields); got != 4 {
		t.Errorf("len(UIFields) = %d, want 4", got)
	}
	if got := len(DiscoveredFields); got != 1 {
		t.Errorf("len(DiscoveredFields) = %d, want 1", got)
	}
}

// TestEventIDIsDiscoveredAndNeverTravels states R2 as a table lookup: a preset
// answers which venue, and the match id is asked for, not remembered.
func TestEventIDIsDiscoveredAndNeverTravels(t *testing.T) {
	if slices.Contains(InstanceFields, "eventId") {
		t.Error("InstanceFields contains eventId: a preset carrying a match id is correct on the day " +
			"it was saved and points the KVS monitor at a dead channel every day after")
	}
	if IsInstanceField("eventId") {
		t.Error("IsInstanceField(\"eventId\") = true; it must never be whitelisted again")
	}
	if c, ok := Classify("eventId"); !ok || c != ClassDiscovered {
		t.Errorf("Classify(\"eventId\") = (%q, %v), want (%q, true)", c, ok, ClassDiscovered)
	}
	if fields, err := Extract(fullConfig()); err != nil {
		t.Fatalf("Extract() error = %v", err)
	} else if _, ok := fields["eventId"]; ok {
		t.Error("Extract() put eventId into a preset even though it is off the whitelist")
	}
}

// TestInstanceFieldsExcludeEveryDeviceField names all eight MACHINE tags: the
// guarantee "a preset cannot carry a device id" reduced to a table lookup.
//
// audioSourceKind and decklinkPersistentId are on this list for exactly the
// reason the first four are. "decklink" applied to a laptop with no card in it
// is the phantom-endpoint fault with different hardware missing, and it fails
// harder — a busy or absent card gives "Internal data stream error /
// not-negotiated (-4)" in about 100 microseconds, naming neither the device nor
// the cause — so a preset that could carry it would be a fault delivered by
// post with no return address.
//
// decklinkChannelMap is the seventh and it does NOT fail hard, which is why it
// belongs here most of all: a routing from another building negotiates, starts,
// and carries the wrong channel — or nothing — behind a screen full of green.
//
// videoSource is the eighth, and it is the one whose classification was actually
// argued (fields.go records both sides). It is here because the authority for
// "is there a camera cabled into this position" is this PC, and because a preset
// carrying it would leave a seat unstartable — not because it is harmless. What
// keeps it from being changed by a seat that should not is App.SetVideoSource
// being host-only, which is a different mechanism in a different file.
func TestInstanceFieldsExcludeEveryDeviceField(t *testing.T) {
	for _, tag := range []string{
		"audioDeviceId", "headphoneDeviceId", "headphoneEndpointId", "slatePath",
		"audioSourceKind", "decklinkPersistentId", "decklinkChannelMap", "videoSource",
	} {
		if slices.Contains(InstanceFields, tag) {
			t.Errorf("InstanceFields contains %q: a preset carrying it would deliver another "+
				"machine's hardware id by post — the phantom-endpoint fault", tag)
		}
		if IsInstanceField(tag) {
			t.Errorf("IsInstanceField(%q) = true; it must never be whitelisted", tag)
		}
		if c, ok := Classify(tag); !ok || c != ClassMachine {
			t.Errorf("Classify(%q) = (%q, %v), want (%q, true)", tag, c, ok, ClassMachine)
		}
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		tag  string
		want Class
		ok   bool
	}{
		{"m2lxHost", ClassInstance, true},
		{"statusKey", ClassInstance, true},
		{"monitorTile", ClassInstance, true},
		{"returnGainDb", ClassInstance, true},
		{"videoBitrateKbps", ClassInstance, true},
		{"videoFormatOverride", ClassInstance, true},
		{"audioDeviceId", ClassMachine, true},
		{"slatePath", ClassMachine, true},
		{"audioSourceKind", ClassMachine, true},
		{"decklinkPersistentId", ClassMachine, true},
		{"decklinkChannelMap", ClassMachine, true},
		{"videoSource", ClassMachine, true},
		{"returnSource", ClassUI, true},
		{"returnChannel", ClassUI, true},
		{"decklinkPreviewEnabled", ClassUI, true},
		{"eventId", ClassDiscovered, true},
		{"pictureSource", "", false}, // frontend-only key with no Go field: unclassified on purpose
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := Classify(tt.tag)
		if ok != tt.ok || got != tt.want {
			t.Errorf("Classify(%q) = (%q, %v), want (%q, %v)", tt.tag, got, ok, tt.want, tt.ok)
		}
	}
}

// fullConfig returns a config with every field set to a recognisable non-zero
// value, so a merge test can tell "left alone" from "zeroed".
func fullConfig() *config.Config {
	return &config.Config{
		M2LXHost:            "wembley.example.com",
		Alias:               "wsl-comms-ro",
		EventID:             "dl9-wembley",
		SRTPort:             40005,
		SRTLatencyMs:        240,
		PBKeyLen:            16,
		VideoBitrateKbps:    10000,
		VideoFormatOverride: "1920x1080p50",
		StatusKey:           "cam7",
		AudioDeviceID:       "{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}",
		AudioSourceKind:     "decklink",
		// The two video-leg fields, both set to the NON-default value, so that a
		// merge test can tell "left alone" from "quietly reset to what this
		// application used to do".
		VideoSource:            "decklink",
		DeckLinkPreviewEnabled: true,
		// A recognisable persistent-id, NOT a device number. The distinction is
		// the field's whole point and a fixture holding "0" would quietly make
		// the wrong shape look normal.
		DeckLinkPersistentID: "0x0000000000AB12CD",
		HeadphoneDeviceID:    "browser-media-device-hash-1234",
		HeadphoneEndpointID:  "{0.0.0.00000000}.{7a2c1f90-4b3e-4c1a-9d55-0d1b3f8e2a11}",
		ReturnSource:         "webrtc",
		ReturnChannel:        "left",
		SRTReturnPort:        40501,
		SRTReturnPBKeyLen:    32,
		PictureLatencyMs:     120,
		ReturnMid:            2,
		MonitorTile:          config.Tile{X: 10, Y: 360, W: 640, H: 360},
		ReturnGainDB:         18.5,
		SlatePath:            `D:\slates\wembley.png`,
	}
}

func TestExtractCarriesOnlyTheWhitelist(t *testing.T) {
	fields, err := Extract(fullConfig())
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(fields) != len(InstanceFields) {
		t.Fatalf("Extract() produced %d keys, want %d", len(fields), len(InstanceFields))
	}
	for _, tag := range InstanceFields {
		if _, ok := fields[tag]; !ok {
			t.Errorf("Extract() is missing whitelisted key %q", tag)
		}
	}
	var nonInstance []string
	nonInstance = append(nonInstance, MachineFields...)
	nonInstance = append(nonInstance, UIFields...)
	nonInstance = append(nonInstance, DiscoveredFields...)
	for _, tag := range nonInstance {
		if _, ok := fields[tag]; ok {
			t.Errorf("Extract() carried non-instance key %q", tag)
		}
	}
}

func TestFilterDropsAndReports(t *testing.T) {
	fields := map[string]json.RawMessage{
		"m2lxHost":      json.RawMessage(`"twickenham.example.com"`),
		"audioDeviceId": json.RawMessage(`"{0.0.1.00000000}.{aaaa}"`),
		"slatePath":     json.RawMessage(`"C:\\other\\slate.png"`),
		"returnSource":  json.RawMessage(`"srt"`),
		"madeUpKey":     json.RawMessage(`1`),
	}
	kept, ignored := Filter(fields)
	if len(kept) != 1 {
		t.Fatalf("Filter kept %d keys, want 1 (m2lxHost)", len(kept))
	}
	if _, ok := kept["m2lxHost"]; !ok {
		t.Error("Filter dropped m2lxHost, which is whitelisted")
	}
	want := []string{"audioDeviceId", "madeUpKey", "returnSource", "slatePath"}
	if !slices.Equal(ignored, want) {
		t.Errorf("Filter ignored = %v, want %v (sorted)", ignored, want)
	}
}

// TestApplyIsAMergeNotAReplace: a two-field preset onto a full config leaves
// the other fields — including all four device/machine fields — byte-identical.
// This is the property the whole apply path rests on: the operator's hardware
// ids survive because the preset physically does not mention them.
func TestApplyIsAMergeNotAReplace(t *testing.T) {
	live := fullConfig()
	before, err := json.Marshal(live)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	ignored, err := Apply(live, map[string]json.RawMessage{
		"m2lxHost": json.RawMessage(`"twickenham.example.com"`),
		"alias":    json.RawMessage(`"wsl-comms-tw"`),
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(ignored) != 0 {
		t.Fatalf("Apply() ignored %v; both keys are whitelisted", ignored)
	}

	if live.M2LXHost != "twickenham.example.com" || live.Alias != "wsl-comms-tw" {
		t.Fatalf("Apply() did not write the two preset fields: host=%q alias=%q", live.M2LXHost, live.Alias)
	}

	// Every OTHER field byte-identical: write the two applied fields back to
	// their originals and compare whole documents, so a regression anywhere in
	// the 18 untouched fields fails without 18 assertions.
	reference := fullConfig()
	reference.M2LXHost = "twickenham.example.com"
	reference.Alias = "wsl-comms-tw"
	after, err := json.Marshal(live)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wantAfter, err := json.Marshal(reference)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(after) != string(wantAfter) {
		t.Errorf("Apply() changed a field the preset never mentioned.\nbefore: %s\nafter:  %s\nwant:   %s",
			before, after, wantAfter)
	}
	if live.AudioDeviceID != fullConfig().AudioDeviceID {
		t.Error("Apply() touched audioDeviceId")
	}
}

// TestApplyDropsMachineKeysFromAHandEditedFile: the defence in depth. A preset
// mailed from a colleague or edited by hand may carry machine keys; Apply must
// neuter them AND report them, never write them.
func TestApplyDropsMachineKeysFromAHandEditedFile(t *testing.T) {
	live := fullConfig()
	originalDevice := live.AudioDeviceID
	originalEndpoint := live.HeadphoneEndpointID
	originalSlate := live.SlatePath
	originalSource := live.ReturnSource

	ignored, err := Apply(live, map[string]json.RawMessage{
		"m2lxHost":            json.RawMessage(`"twickenham.example.com"`),
		"audioDeviceId":       json.RawMessage(`"{0.0.0.00000000}.{8678ce58-90c0-4827-8ff7-c9edd8d074ed}"`),
		"headphoneEndpointId": json.RawMessage(`"{0.0.0.00000000}.{dead-beef}"`),
		"slatePath":           json.RawMessage(`"C:\\someone\\elses\\slate.png"`),
		"returnSource":        json.RawMessage(`"srt"`),
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	want := []string{"audioDeviceId", "headphoneEndpointId", "returnSource", "slatePath"}
	if !slices.Equal(ignored, want) {
		t.Errorf("Apply() ignored = %v, want %v", ignored, want)
	}
	if live.AudioDeviceID != originalDevice {
		t.Errorf("audioDeviceId was overwritten to %q; the hand-edited key must be dropped", live.AudioDeviceID)
	}
	if live.HeadphoneEndpointID != originalEndpoint {
		t.Errorf("headphoneEndpointId was overwritten to %q", live.HeadphoneEndpointID)
	}
	if live.SlatePath != originalSlate {
		t.Errorf("slatePath was overwritten to %q", live.SlatePath)
	}
	if live.ReturnSource != originalSource {
		t.Errorf("returnSource was overwritten to %q; a preset must not change what is in the ears", live.ReturnSource)
	}
	if live.M2LXHost != "twickenham.example.com" {
		t.Error("the whitelisted key travelling WITH the bad ones must still apply")
	}
}

// TestApplyNeverWritesTheEventID is the low-level half of R2. Load strips the
// key before Apply ever sees it, so this exercises the OTHER door: a fields map
// assembled in memory, or handed straight to Apply by a future caller. The
// stale match id must not reach the config, and — unlike the load path, which
// is deliberately quiet about a key this application's own older builds wrote —
// a caller who built the map itself is told.
func TestApplyNeverWritesTheEventID(t *testing.T) {
	live := fullConfig() // EventID "dl9-wembley": the event chosen on this machine
	const chosenHere = "dl9-wembley"

	ignored, err := Apply(live, map[string]json.RawMessage{
		"m2lxHost": json.RawMessage(`"twickenham.example.com"`),
		"eventId":  json.RawMessage(`"dl9-last-season"`),
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if live.EventID != chosenHere {
		t.Errorf("eventId = %q, want %q left alone — applying a venue must never re-point the KVS "+
			"monitor at the match the preset was saved during", live.EventID, chosenHere)
	}
	if !slices.Equal(ignored, []string{"eventId"}) {
		t.Errorf("Apply() ignored = %v, want [eventId]", ignored)
	}
	if live.M2LXHost != "twickenham.example.com" {
		t.Error("the whitelisted key travelling with the discovered one must still apply")
	}
}

// TestTheVideoLegSettingsTravelWithTheInstance is the positive half of the two
// new INSTANCE fields: a preset saved at a venue carries the bitrate its
// circuit will take and the format its switcher is configured for, because
// those are facts about the venue and every seat at it needs the same answers.
// Without them a position that applies a facility preset still has to be told
// two numbers by somebody on the phone.
func TestTheVideoLegSettingsTravelWithTheInstance(t *testing.T) {
	fields, err := Extract(fullConfig())
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	for tag, want := range map[string]string{
		"videoBitrateKbps":    "10000",
		"videoFormatOverride": `"1920x1080p50"`,
	} {
		got, ok := fields[tag]
		if !ok {
			t.Errorf("Extract() dropped %q, which is whitelisted", tag)
			continue
		}
		if string(got) != want {
			t.Errorf("Extract() carried %q as %s, want %s", tag, got, want)
		}
	}

	live := &config.Config{VideoBitrateKbps: 2000}
	if _, err := Apply(live, fields); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if live.VideoBitrateKbps != 10000 {
		t.Errorf("VideoBitrateKbps after Apply = %d, want 10000", live.VideoBitrateKbps)
	}
	if live.VideoFormatOverride != "1920x1080p50" {
		t.Errorf("VideoFormatOverride after Apply = %q, want %q", live.VideoFormatOverride, "1920x1080p50")
	}
}

// TestApplyDropsTheDeckLinkCaptureKeys is the phantom-endpoint test with the
// hardware that is missing changed. A preset hand-edited (or saved by a future
// build with a wider whitelist) at a DeckLink-equipped seat must not be able to
// tell a laptop with no card in it to capture from one: the failure would be
// "Internal data stream error / not-negotiated (-4)", in about 100
// microseconds, naming neither the device nor the cause — and the operator
// would be looking at a Settings screen with nothing wrong on it.
func TestApplyDropsTheDeckLinkCaptureKeys(t *testing.T) {
	live := fullConfig()
	live.AudioSourceKind = config.AudioSourceNative
	live.DeckLinkPersistentID = ""

	ignored, err := Apply(live, map[string]json.RawMessage{
		"srtPort":              json.RawMessage(`40009`),
		"audioSourceKind":      json.RawMessage(`"decklink"`),
		"decklinkPersistentId": json.RawMessage(`"0x00000000DEADBEEF"`),
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if !slices.Equal(ignored, []string{"audioSourceKind", "decklinkPersistentId"}) {
		t.Errorf("Apply() ignored = %v, want [audioSourceKind decklinkPersistentId]", ignored)
	}
	if live.AudioSourceKind != config.AudioSourceNative {
		t.Errorf("audioSourceKind became %q; a preset must never change which subsystem this PC "+
			"captures from", live.AudioSourceKind)
	}
	if live.DeckLinkPersistentID != "" {
		t.Errorf("decklinkPersistentId became %q; that is another machine's card", live.DeckLinkPersistentID)
	}
	if live.SRTPort != 40009 {
		t.Error("the whitelisted key travelling with the machine ones must still apply")
	}
}

// TestApplyDropsTheDeckLinkVideoKeys is the same phantom-hardware test on the
// leg where the failure is loudest, plus its UI companion.
//
// A preset carrying videoSource="decklink" applied at a seat with no card in it
// describes a camera that is not there, and the result is not a degraded feed
// but no feed at all: an absent or busy card gives not-negotiated (-4) in about
// 100 microseconds, naming neither the device nor the cause, with a Settings
// screen showing nothing wrong. decklinkPreviewEnabled travels with it here
// because the two arrive together in any hand-edited file and they are refused
// for two different reasons — one is this PC's cabling, the other is what is on
// this operator's screen.
func TestApplyDropsTheDeckLinkVideoKeys(t *testing.T) {
	live := fullConfig()
	live.VideoSource = config.VideoSourceSlate
	live.DeckLinkPreviewEnabled = false

	ignored, err := Apply(live, map[string]json.RawMessage{
		"srtLatencyMs":           json.RawMessage(`400`),
		"videoSource":            json.RawMessage(`"decklink"`),
		"decklinkPreviewEnabled": json.RawMessage(`true`),
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if !slices.Equal(ignored, []string{"decklinkPreviewEnabled", "videoSource"}) {
		t.Errorf("Apply() ignored = %v, want [decklinkPreviewEnabled videoSource]", ignored)
	}
	if live.VideoSource != config.VideoSourceSlate {
		t.Errorf("videoSource became %q; a preset must never decide what this position puts on "+
			"air, and this seat may have no camera at all", live.VideoSource)
	}
	if live.DeckLinkPreviewEnabled {
		t.Error("decklinkPreviewEnabled became true; a preset must not open a window on somebody " +
			"else's screen from a configuration form")
	}
	if live.SRTLatencyMs != 400 {
		t.Error("the whitelisted key travelling with the two refused ones must still apply")
	}
}

// TestApplyPartialMonitorTileMergesFieldByField mirrors config's
// TestLoad_PartialNestedTileMergesFieldByField: the unmarshal-onto-live-struct
// primitive gives nested partial merges for free, and this pins that Apply
// keeps using that primitive rather than replacing the Tile wholesale.
func TestApplyPartialMonitorTileMergesFieldByField(t *testing.T) {
	live := fullConfig() // MonitorTile {10, 360, 640, 360}

	if _, err := Apply(live, map[string]json.RawMessage{
		"monitorTile": json.RawMessage(`{"x": 1120, "y": 720}`),
	}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	want := config.Tile{X: 1120, Y: 720, W: 640, H: 360}
	if live.MonitorTile != want {
		t.Errorf("MonitorTile after a partial merge = %+v, want %+v — w and h absent from the "+
			"preset must keep their live values", live.MonitorTile, want)
	}
}
