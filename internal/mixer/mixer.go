// Package mixer is the type contract for the M2L-X advanced audio mixer
// drawer: the read-only bus/routing view, its comparison against a saved
// golden state, and the arm-gated write path that can correct routing.
//
// Owner: WP-M0. THIS FILE IS THE SEAM. Every other mixer work package compiles
// against the declarations here, so the types are stable and the bodies are
// not: apart from BusLabel, every function in this file is a stub that returns
// a zero value and an error naming the work package that will implement it.
//
//	WP-M1  NewController, Controller transport, Command.Envelope wire shapes
//	WP-M2  ParseSnapshot
//	WP-M3  Compare / Diff rendering
//	WP-M4  the drawer UI (frontend/src/ui/mixer/, see contract.js)
//
// ===================== WHY THIS PACKAGE EXISTS ==============================
//
// The M2L-X advanced_audio_mixer is NOT a REST resource. /api/audio/mixer/list
// and its neighbours 404. Strip state is read from the switcher_status frame
// and written over a WebSocket:
//
//	wss://<host>/api/v1/switcher_controller?access_token=<PERCENT-ENCODED>
//
// The DSP node has seven stereo buses. Two of them leave the building:
//
//	master -> the PGM output
//	aux1   -> the CLN output
//
// aux1 IS the clean feed. The mixer surface in Sony's GUI calls that bus
// "AUX"; the output list calls the same bus "cln". Nothing in Sony's UI states
// that they are the same thing. That single missing sentence is the whole
// reason this drawer exists, because of the next fact:
//
// EVERY STRIP DEFAULTS TO ["master","aux1","aux2"]. So any input that is
// unmuted joins the clean feed unless somebody corrects its routing. In the
// captured live frame (internal/m2lx/testdata/switcher_status-live-2026-07-31.json)
// the commentary input cam22-1, whose operator-facing display name is
// "CLAUDE-COMMS", is routed exactly that way: {"outputs":["master","aux1","aux2"]}.
// Commentary is sitting in the clean feed on the live event, from the default,
// with nothing in the vendor UI naming it. Making that visible is this
// package's entire job, and BusLabel below is where the naming is fixed.
//
// ===================== WHAT THIS PACKAGE DOES NOT DO ========================
//
// It does not own the switcher_status subscription, the sign-in, or the token.
// internal/m2lx owns those. This package takes a raw frame (ParseSnapshot) and
// a host plus token (NewController) and nothing else, so that the drawer can be
// built and tested without a second consumer of the live socket.
package mixer

import (
	"context"
	"time"
)

// Bus is one of the seven stereo buses of the advanced_audio_mixer DSP node.
//
// The string values are the wire names. They appear as keys of state.outputs,
// state.levels, state.peak_hold_levels and state.fader, and as members of the
// "outputs" and "pfl_outputs" arrays in state.matrix, so a Bus can be used
// directly as a map key against a parsed frame and directly in a set_routing
// argument without translation.
type Bus string

// The seven buses. Egress — whether audio on the bus can actually leave the
// unit — is documented per constant because it is not discoverable from the
// mixer node, and two of the three non-obvious cases have bitten this project.
const (
	// BusMaster is the programme sum, delivered on the PGM output.
	//
	// MEASURED, and the reason SetCompLimit exists: master sums at unity with
	// NO limiter. Two sources at -5 dBFS summed to +1 dBFS and produced a
	// -27 dB distortion residual. Gain staging on the contributing strips, or
	// a limiter placed with SetCompLimit, is the only mitigation; there is no
	// bus-level protection to fall back on.
	BusMaster Bus = "master"

	// BusAux1 is THE CLEAN FEED. It is delivered on the output the M2L-X
	// output list calls "cln", while the mixer surface calls this same bus
	// "AUX". Sony's UI never states the equivalence.
	//
	// This is the bus an operator must never put commentary into by accident,
	// and — see the package comment — the default routing of every strip puts
	// them there. Anything rendering a bus name to a human must render it
	// through BusLabel, never by printing the raw value "aux1".
	BusAux1 Bus = "aux1"

	// BusAux2 is routable and live — its fader works and its meter moves — but
	// it HAS NO EGRESS. MEASURED: no output on the dev event accepts aux2 as a
	// source, and it does not appear on the WebRTC monitor. Audio sent here is
	// audible to nobody.
	//
	// It is carried in this contract rather than hidden precisely because it
	// looks functional. An operator who sees a moving meter will otherwise
	// plan a mix-minus or a talkback leg around a bus that cannot be
	// delivered. Show it, and label it undeliverable.
	BusAux2 Bus = "aux2"

	// BusMon1 is a per-MIC monitor bus. MEASURED in the live frame: strips
	// MIC 1-1 and MIC 1-2 route to ["master","mon1"], MIC 2-* to mon2 and
	// MIC 3-* to mon3, i.e. mon1..mon3 are the three MIC monitor legs.
	BusMon1 Bus = "mon1"

	// BusMon2 is the MIC 2 monitor bus. See BusMon1.
	BusMon2 Bus = "mon2"

	// BusMon3 is the MIC 3 monitor bus. See BusMon1.
	BusMon3 Bus = "mon3"

	// BusMon4 is the PFL (pre-fade listen) bus. MEASURED: it is the only bus
	// whose entry in state.outputs carries "pfl_mode":true; mon1..mon3 carry
	// false. It is the destination of the "pfl" routing matrix and of the
	// pfl_outputs list on a strip, not of ordinary output routing.
	BusMon4 Bus = "mon4"
)

// AllBuses is every bus, in the order a surface should present them: the two
// buses with egress first, then the undeliverable one, then the monitors.
//
// Iterate this rather than ranging a map when rendering or diffing, so that
// two runs against the same state produce byte-identical output and a Diff
// list is stable between 1 Hz updates.
var AllBuses = []Bus{BusMaster, BusAux1, BusAux2, BusMon1, BusMon2, BusMon3, BusMon4}

// busLabels is the single place where a bus is given the name a human sees.
//
// It is a package-level table rather than a switch inside BusLabel so that it
// cannot drift between call sites: the aux1/cln equivalence and the aux2
// egress warning exist in exactly one location in this codebase.
var busLabels = map[Bus]string{
	BusMaster: "master (PGM)",
	BusAux1:   "aux1 (CLN - clean feed)",
	BusAux2:   "aux2 (no egress)",
	BusMon1:   "mon1",
	BusMon2:   "mon2",
	BusMon3:   "mon3",
	BusMon4:   "mon4 (PFL)",
}

// BusLabel returns the operator-facing label for a bus.
//
// This is the one function in this file with a real body, because it is the
// mitigation for the failure this whole drawer is built to prevent. An
// operator reading "aux1" has no way to know they are looking at the clean
// feed, and an operator reading "aux2" has no way to know that bus goes
// nowhere. Every rendering path — Go-side logging, the drawer, the diff list —
// must call this rather than printing a Bus value directly.
//
// An unrecognised bus is returned unchanged with a marker, never as the empty
// string: if Sony adds an eighth bus, the correct behaviour is to show the
// operator something they can report, not a blank cell.
func BusLabel(b Bus) string {
	if label, ok := busLabels[b]; ok {
		return label
	}
	if b == "" {
		return "(unnamed bus)"
	}
	return string(b) + " (unknown bus)"
}

// SilenceDBFS is the value the mixer reports for digital silence, in dBFS.
//
// MEASURED: strip meters report exactly -100.0 and bus meters report
// -99.99999237060547 for the same silence, so a meter must be treated as
// silent at or below this value rather than compared for equality.
const SilenceDBFS = -100.0

// MuteFaderDB is the channel-fader gain that means muted, in dB.
//
// MEASURED: set_ch_fader takes gain in dB and -144 is its mute value. Note
// that a strip can be silent for two independent reasons — this fader value,
// or the muted flag carried in Strip.Muted — and correcting one does not
// change the other.
const MuteFaderDB = -144.0

// Strip is one channel strip of the mixer: one channel-pair of one input.
//
// Strips are named "<input>-<n>", e.g. "cam22-1". In the switcher_status frame
// they live INSIDE their input object, keyed by strip name, alongside keys that
// are not strips at all — "assign_list" and "channel_count". A parser must key
// off the shape of the value, not assume every key of an input is a strip.
type Strip struct {
	// Name is the wire name of the strip, e.g. "cam22-1". It is the key used
	// by state.matrix, state.levels, state.fader and state.effect, and the
	// "input" argument of set_routing and the "name" argument of the
	// per-strip commands.
	Name string `json:"name"`

	// Input is the parent input name, e.g. "cam22". It is the key of the
	// enclosing object in state.inputs AND the node name of the per-input
	// status node from which DisplayName is taken.
	Input string `json:"input"`

	// DisplayName is the OPERATOR-FACING name of the parent input, e.g.
	// "CLAUDE-COMMS" or "Input 23".
	//
	// It does NOT come from the mixer node. The display_name found on a strip
	// inside state.inputs is merely the strip name repeated — MEASURED: the
	// cam22-1 strip carries "display_name":"cam22-1". The name a human
	// recognises is on the PER-INPUT status node: the entry of the status
	// array whose node is "cam22" has state.display_name "CLAUDE-COMMS".
	// WP-M2 performs that join, so that the matrix shows names the operator
	// set rather than camera numbers.
	//
	// Empty when the per-input node is absent from the frame; a renderer
	// should fall back to Input, never to a blank.
	DisplayName string `json:"displayName"`

	// Muted is the strip's mute flag, from the strip object in state.inputs.
	// It is independent of the fader: see MuteFaderDB.
	Muted bool `json:"muted"`

	// Follow reports whether the strip follows its sources rather than holding
	// a fixed state, from "follow".
	Follow bool `json:"follow"`

	// FollowSources are the sources followed when Follow is set, from
	// "follow_sources". MEASURED: this is populated even when Follow is false
	// — every strip in the live frame carries its own parent input here — so
	// it must not be used as a proxy for Follow.
	FollowSources []string `json:"followSources"`

	// SubChMode is the EFFECTIVE sub-channel mode of the strip, from
	// "sub_ch_mode": observed values "ST_W" and "MONO".
	//
	// The frame also carries "sub_ch_mode_set", the REQUESTED mode, and the
	// two can disagree — MEASURED: strip cam22-2 reports sub_ch_mode "MONO"
	// with sub_ch_mode_set "ST_W". This field deliberately carries the
	// effective value, because the drawer's purpose is to show what the mixer
	// is doing rather than what it was asked to do.
	SubChMode string `json:"subChMode"`

	// Outputs are the buses this strip is routed to, from
	// state.matrix[strip].outputs. Presence of BusAux1 here means this strip
	// is IN THE CLEAN FEED.
	Outputs []Bus `json:"outputs"`

	// PFLOutputs are the strip's pre-fade-listen destinations, from
	// state.matrix[strip].pfl_outputs — the "pfl" routing matrix rather than
	// the "output" one. MEASURED: empty for every strip in the live frame.
	PFLOutputs []Bus `json:"pflOutputs"`

	// Level is the current meter, in dBFS, [left, right], from state.levels.
	// See SilenceDBFS. Meaningless unless Metered is set.
	Level [2]float64 `json:"level"`

	// PeakHold is the held peak, in dBFS, [left, right], from
	// state.peak_hold_levels. The hold time is state.peak_hold_time (observed
	// 3, presumed seconds) and is settable with set_peak_hold_time.
	// Meaningless unless Metered is set.
	PeakHold [2]float64 `json:"peakHold"`

	// Metered reports whether the frame carried meter data for this strip at
	// all, and MUST be checked before Level or PeakHold is drawn.
	//
	// MEASURED, and the reason this field exists: the live frame routes 54
	// strips but meters only 34 keys, and the unmetered ones include every
	// "-2" strip. Without this flag an absent meter parses to the zero value
	// 0.0 dBFS, which is FULL SCALE — a silent channel would draw as a meter
	// pinned at the top of its scale on a broadcast surface. Absent must be
	// rendered as absent, not as silence and certainly not as zero.
	Metered bool `json:"metered"`

	// Fader is the channel-fader gain in dB, [left, right], from
	// state.fader[strip].ch_fader.gain.
	//
	// It is per-channel, not scalar: MEASURED, cam22-1 reports
	// "gain":[-1.574803113937378,-1.574803113937378]. The pair is carried
	// rather than collapsed because the two channels can legitimately differ,
	// and a strip with one channel at MuteFaderDB and the other open is
	// exactly the half-live condition this drawer must not hide.
	Fader [2]float64 `json:"fader"`

	// FaderEnabled reports, per channel, whether the channel fader is in
	// circuit, from state.fader[strip].ch_fader.enabled.
	//
	// MEASURED: most strips in the live frame report enabled [false,false]
	// with gain [0,0], while cam22-1 reports [true,true] with a real gain. A
	// gain shown without this flag is a number that may not be applied to any
	// audio, so a renderer must present a disabled fader distinctly rather
	// than printing its gain as fact.
	FaderEnabled [2]bool `json:"faderEnabled"`
}

// InCleanFeed is not defined here on purpose. Whether a strip is audible in the
// clean feed depends on its routing AND its mute AND its fader AND its parent
// input's state, and collapsing that into one boolean in the contract would let
// every consumer inherit a judgement they cannot see. Compare reports the
// routing fact; the drawer shows mute and fader beside it.

// BusState is the state of one bus.
type BusState struct {
	// Name is the bus. Render it with BusLabel, never directly.
	Name Bus `json:"name"`

	// Muted is the bus mute flag, from state.outputs[bus].muted. Note the
	// asymmetry with the write path: reading a mute is a plain bool here, but
	// setting one requires the array form documented on SetOutputMuted.
	Muted bool `json:"muted"`

	// ChannelCount is state.outputs[bus].channel_count. MEASURED: 2 for all
	// seven buses on the dev event.
	ChannelCount int `json:"channelCount"`

	// Level is the bus meter, in dBFS, [left, right], from state.levels.
	// Meaningless unless Metered is set. MEASURED: bus silence reads
	// -99.99999237060547 rather than the -100.0 strips report.
	Level [2]float64 `json:"level"`

	// PeakHold is the held bus peak, in dBFS, [left, right]. Meaningless
	// unless Metered is set.
	PeakHold [2]float64 `json:"peakHold"`

	// Metered reports whether the frame carried meter data for this bus. See
	// Strip.Metered for why absence is not silence. MEASURED: all seven buses
	// are metered in the live frame, but the flag is carried for the same
	// reason as on Strip — a zero value must never be drawn as 0 dBFS.
	Metered bool `json:"metered"`

	// Fader is the output-fader gain from state.fader[bus].output_fader.gain.
	// Scalar, unlike the per-channel Strip.Fader. Meaningless unless
	// FaderPresent is set.
	//
	// UNITS ARE NOT CONFIRMED. MEASURED: master reports 0 and aux1/aux2
	// report 1. Read as dB that is unity on master and +1 dB on the auxes,
	// which is plausible; read as a linear multiplier it would make master —
	// the bus carrying PGM — silent, which contradicts the observed
	// programme. So this contract treats it as dB, and WP-M1 must confirm
	// against set_output_fader before any UI offers to change it.
	Fader float64 `json:"fader"`

	// FaderPresent reports whether the frame carried an output_fader for this
	// bus at all.
	//
	// MEASURED, and the reason this field exists: state.fader contains master,
	// aux1 and aux2 but NOT mon1..mon4. Without the flag, the monitor buses
	// would all parse to a fader of 0, which under the dB reading above is
	// unity — a confident, wrong claim about four buses the frame says nothing
	// about.
	FaderPresent bool `json:"faderPresent"`
}

// Snapshot is one whole reading of the mixer: every strip, every bus, and the
// instant the frame was produced.
//
// It is a value, copied freely, and it is the unit of comparison: the golden
// state saved by the operator and the live state polled at ~1 Hz are both
// Snapshots, and Compare takes two of them.
type Snapshot struct {
	// Strips are all strips found in the frame. Order is the parser's
	// responsibility and must be deterministic — sorted by Name — so that a
	// UI list does not reorder itself between updates.
	Strips []Strip `json:"strips"`

	// Buses are the seven buses, in AllBuses order.
	Buses []BusState `json:"buses"`

	// TakenAt is the frame's own timestamp, not the local clock.
	//
	// MEASURED: the top-level "timestamp" field is epoch MILLISECONDS
	// (1785522083212 is 2026-07-31), so WP-M2 must convert with
	// time.UnixMilli. Treating it as seconds dates the frame to 1970 and
	// treating it as nanoseconds dates it to 1970 as well; either would make
	// a staleness check on the drawer permanently wrong.
	TakenAt time.Time `json:"takenAt"`
}

// Strip returns the strip with the given wire name, e.g. "cam22-1".
//
// The second result reports whether it was found; the zero Strip is never a
// valid reading, since a real strip always has a Name.
//
// Implemented by WP-M2 alongside ParseSnapshot, because the lookup strategy —
// linear scan or an index built at parse time — is that package's choice and
// callers must not depend on which.
func (s Snapshot) Strip(name string) (Strip, bool) {
	return s.lookupStrip(name) // WP-M2, state.go
}

// ParseSnapshot builds a Snapshot from one whole switcher_status frame.
//
// The input is the ENTIRE frame, not a pre-extracted node:
// {"status":[...],"timestamp":...} where status is an ARRAY of
// {"node":"...","path":"/","state":{...}}. The whole frame is required because
// the operator-facing display names are not in the mixer node — they are on the
// per-input status nodes — and the join cannot be done from the mixer node
// alone. See Strip.DisplayName.
//
// A frame with no advanced_audio_mixer entry is an error, not an empty
// Snapshot: an empty mixer view would render as "nothing is routed to the
// clean feed", which is the most dangerous possible false statement this
// drawer could make.
//
// Implemented by WP-M2.
func ParseSnapshot(raw []byte) (Snapshot, error) {
	return parseSnapshot(raw) // WP-M2, state.go
}

// Severity ranks a Diff for presentation and for whether it should block.
type Severity string

// The severities. The ranking is about audibility on an output that leaves the
// building, not about how many fields changed.
const (
	// SeverityInfo is a difference with no effect on what anybody hears:
	// meter and peak-hold movement, a display name edit.
	SeverityInfo Severity = "info"

	// SeverityWarn is a difference that changes audio but only on a bus that
	// is monitored or undeliverable: a mon1..mon4 routing change, a change
	// affecting BusAux2, a fader move on a muted strip.
	SeverityWarn Severity = "warn"

	// SeverityCritical is a difference that changes what is heard on an
	// output that leaves the building.
	//
	// A STRIP GAINING BusAux1 IS THE DEFINING CASE, and WP-M3 must classify it
	// here unconditionally. aux1 is the CLN output, so a strip that has gained
	// it is now in the clean feed — and because every strip DEFAULTS to
	// ["master","aux1","aux2"], this is what a reset, a re-added input or a
	// restored device profile silently produces. It is the failure the golden
	// comparison exists to catch. Losing BusMaster is also critical: that is
	// programme audio disappearing.
	SeverityCritical Severity = "critical"
)

// DiffKind says whether a Diff is about a strip or a bus, so a renderer can
// group them without parsing Target.
type DiffKind string

// The two kinds of diff target.
const (
	// DiffStrip means Target is a strip wire name, e.g. "cam22-1".
	DiffStrip DiffKind = "strip"
	// DiffBus means Target is a bus wire name, e.g. "aux1", and should be
	// rendered through BusLabel.
	DiffBus DiffKind = "bus"
)

// Diff is one difference between a golden Snapshot and the current one.
//
// Values are carried as rendered strings rather than as typed fields, because
// this type crosses into JavaScript and the drawer's job is to show an operator
// what changed, not to recompute it. The rendering is WP-M3's and must be
// deterministic, so that an unchanged mixer produces an unchanged diff list at
// every 1 Hz update:
//
//	bools      "true" / "false"
//	bus lists  labels joined with ", " in AllBuses order, never map order
//	dB values  one decimal place with the unit, e.g. "-1.6 dB"
//	absent     "(absent)" — never "" and never "0", see Strip.Metered
type Diff struct {
	// Kind says whether this is a strip or a bus difference.
	Kind DiffKind `json:"kind"`

	// Target is the wire name of the strip or bus the difference is on.
	Target string `json:"target"`

	// Label is the human-facing name of the target: the input's DisplayName
	// for a strip, BusLabel for a bus. Carried so the drawer never has to
	// re-derive a name that the operator has to trust.
	Label string `json:"label"`

	// Field is the contract field name that differs, e.g. "outputs",
	// "muted", "fader".
	Field string `json:"field"`

	// Golden is the rendered value from the saved golden snapshot.
	Golden string `json:"golden"`

	// Current is the rendered value from the live snapshot.
	Current string `json:"current"`

	// Severity ranks the difference. See SeverityCritical for the aux1 rule.
	Severity Severity `json:"severity"`
}

// Compare is declared and implemented in golden.go.
//
// It is NOT declared here, deliberately, and this note stands in its place so
// that nobody restores the contract stub that used to sit at this line. That
// stub was a duplicate declaration: the package did not compile, so its entire
// test suite — every safety property in this package — had never once run.
//
// The repair is asymmetric and that is the point. Deleting golden.go's real
// implementation ALSO produces a green build, and Compare then returns nil for
// ever: the drawer's drift panel reports "no differences" permanently. A preset
// recall re-adds a commentary strip to aux1, the panel stays clean, and the
// operator goes to air with commentary in the client's clean feed, having
// checked the one screen built to prevent exactly that. A green build is not
// evidence here; the tests in golden_test.go are.
//
// Compare's contract lives with its implementation in golden.go.

// Command is one write to the mixer.
//
// Envelope returns the complete message to be sent on the switcher_controller
// WebSocket, not just the arguments:
//
//	{"node":"advanced_audio_mixer","command":"<name>","args":{...}}
//
// It returns an error rather than panicking on an invalid command, because
// these values originate in a UI and the write path is live: a malformed
// set_routing must fail before transmission, not be repaired into a different
// routing than the operator asked for.
//
// Implementations are values, not pointers, and must be safe to build and
// inspect without side effects. Building a Command must never write anything.
type Command interface {
	// Envelope renders the full WebSocket message for this command.
	Envelope() (map[string]any, error)
}

// RoutingMatrix selects which routing matrix a SetRouting addresses.
type RoutingMatrix string

// The routing matrices. MEASURED: the enum has exactly these two members.
const (
	// MatrixOutput is ordinary bus routing — the matrix that decides whether
	// a strip is in the clean feed.
	MatrixOutput RoutingMatrix = "output"
	// MatrixPFL is pre-fade-listen routing, which feeds BusMon4.
	MatrixPFL RoutingMatrix = "pfl"
)

// SetRouting replaces the set of buses a strip is routed to.
//
// MEASURED argument shape:
//
//	{"matrix":"output","input":"cam22-1","outputs":["master","aux1"]}
//
// Note that "input" takes a STRIP name ("cam22-1"), not an input name
// ("cam22"), despite the key.
//
// This is a REPLACE, not an add or a remove: Outputs is the complete resulting
// set. Removing a strip from the clean feed therefore means sending every bus
// it should keep, and sending an empty Outputs silently un-routes the strip
// from programme as well. WP-M1 must reject a nil Outputs rather than treat it
// as "no change".
type SetRouting struct {
	// Matrix is MatrixOutput or MatrixPFL.
	Matrix RoutingMatrix `json:"matrix"`
	// Strip is the strip wire name, e.g. "cam22-1". Serialised as "input".
	Strip string `json:"input"`
	// Outputs is the complete resulting bus set for this strip.
	Outputs []Bus `json:"outputs"`
}

// Envelope for SetRouting is implemented in controller.go (WP-M1).

// SetInputMuted mutes or unmutes one strip.
//
// The command name is set_input_muted and it addresses a STRIP, matching
// set_routing's use of "input" for a strip name.
//
// THE ARGUMENT SHAPE IS NOT MEASURED. Its sibling set_output_muted requires an
// array and rejects the object form with HTTP 400 (see SetOutputMuted), which
// makes the array form the likelier shape here too — but likelier is not
// verified, and this command's write lands on live inputs. WP-M1 must confirm
// the shape against the dev mixer before the write path is armed, and must
// record the result in this doc comment rather than leaving this paragraph
// standing.
type SetInputMuted struct {
	// Strip is the strip wire name, e.g. "cam22-1".
	Strip string `json:"name"`
	// Muted is the desired state.
	Muted bool `json:"muted"`
}

// Envelope for SetInputMuted is implemented in controller.go (WP-M1). NOTE for
// the contract owner: the "THE ARGUMENT SHAPE IS NOT MEASURED" paragraph above
// is now stale. The shape is settled as the ARRAY form from Sony's own bundle
// (docs/architecture.md line 430); see the Envelope doc comment in
// controller.go for the evidence and its class.

// SetOutputMuted mutes or unmutes whole buses.
//
// MEASURED, and non-obvious: the arguments REQUIRE THE ARRAY FORM.
//
//	[{"name":"aux1","muted":true}]
//
// The object form — {"name":"aux1","muted":true} — returns HTTP 400. A single
// bus is therefore still sent as a one-element array. WP-M1 must not
// "simplify" the single-bus case back to an object.
//
// Muting a bus here is a blunt instrument and is not the fix for commentary in
// the clean feed: muting aux1 removes the ENTIRE clean feed, not one strip
// from it. The correct correction is SetRouting on the offending strip.
type SetOutputMuted struct {
	// Buses is the set of buses to change, sent as the array documented
	// above. Each entry carries its own muted value via Muted, which applies
	// to all buses named here; to set different values, send more than one
	// command.
	Buses []Bus `json:"buses"`
	// Muted is the desired state for every bus in Buses.
	Muted bool `json:"muted"`
}

// Envelope for SetOutputMuted is implemented in controller.go (WP-M1).

// SetChFader sets a strip's channel-fader gain.
//
// MEASURED: gain is in dB, and the mute value is MuteFaderDB (-144). The
// reported state is per-channel — see Strip.Fader — so this command carries a
// per-channel pair rather than a scalar; WP-M1 must confirm whether the wire
// accepts the pair directly, as the read side reports it, or requires one
// message per channel, and record the answer here.
//
// A fader move is not a routing fix. Pulling a strip to MuteFaderDB leaves it
// routed to aux1, so the next state restore or follow event brings it back
// audible in the clean feed with no further operator action.
type SetChFader struct {
	// Strip is the strip wire name, e.g. "cam22-1".
	Strip string `json:"name"`
	// Gain is the desired gain in dB, [left, right]. MuteFaderDB mutes.
	Gain [2]float64 `json:"gain"`
}

// Envelope for SetChFader is implemented in controller.go (WP-M1), where the
// unresolved question above is answered as far as it can be without writing to
// a live mixer — including a measured reason this command may be inert on a
// strip whose ch_fader.enabled is false.

// AGCMode selects the dynamics processor mode of a strip.
type AGCMode string

// The AGC modes relevant to this application.
const (
	// AGCOff is no dynamics processing. MEASURED: this is the state of every
	// strip in the live frame.
	AGCOff AGCMode = "off"

	// AGCLimiter enables the limiter, and is the ONLY mode under which
	// SetCompLimit's LimiterTh does anything. See SetCompLimit.
	AGCLimiter AGCMode = "limiter"
)

// SetCompLimit configures a strip's compressor/limiter.
//
// MEASURED argument shape:
//
//	{"name":"cam23-1","agc_mode":"limiter","pre_gain":0,
//	 "compressor_th":0,"limiter_th":-3}
//
// THE TRAP: AGCMode MUST be AGCLimiter, or LimiterTh is SILENTLY INERT. The
// command is accepted, the value is stored and read back unchanged in
// switcher_status, and no limiting occurs. A read-back check therefore does
// NOT detect this mistake — the state will show exactly what was sent — so
// WP-M1 must reject a SetCompLimit that carries a LimiterTh with any AGCMode
// other than AGCLimiter rather than relying on verification after the fact.
//
// This command is the mitigation for the master bus summing at unity with no
// limiter: see BusMaster, where two sources at -5 dBFS summed to +1 dBFS with
// a -27 dB distortion residual.
type SetCompLimit struct {
	// Strip is the strip wire name, e.g. "cam23-1". Serialised as "name".
	Strip string `json:"name"`

	// AGCMode must be AGCLimiter for LimiterTh to take effect.
	AGCMode AGCMode `json:"agc_mode"`

	// PreGain is gain applied ahead of the dynamics stage, in dB. MEASURED:
	// it is exactly dB-calibrated — a change of n here moves the measured
	// level by n dB — so it can be used for gain staging without a
	// calibration pass.
	PreGain float64 `json:"pre_gain"`

	// CompressorTh is the compressor threshold in dBFS.
	CompressorTh float64 `json:"compressor_th"`

	// LimiterTh is the limiter threshold in dBFS. Inert unless AGCMode is
	// AGCLimiter — see the type comment.
	LimiterTh float64 `json:"limiter_th"`
}

// Envelope for SetCompLimit is implemented in controller.go (WP-M1), which
// enforces the AGCLimiter precondition documented on the type.

// Controller is the write path to the mixer's switcher_controller WebSocket.
//
// It exists only for writes. Reading is done from the switcher_status frame
// through ParseSnapshot, and nothing in this package requires a Controller in
// order to display the mixer — the drawer is fully functional, and safe,
// with no Controller at all.
type Controller interface {
	// Send transmits commands in order and returns when they have been
	// written.
	//
	// THIS IS THE ARM-GATED WRITE PATH. It changes a live mixer that feeds
	// PGM and CLN outputs, and the drawer must not call it unless the
	// operator has explicitly armed the UI — see setArmed in
	// frontend/src/ui/mixer/contract.js.
	//
	// A nil return means the commands were SENT, not that they took effect.
	// The mixer acknowledges at the transport level, and at least one command
	// in this vocabulary is silently inert when misused (SetCompLimit with the
	// wrong AGCMode). Callers MUST confirm the intended change by reading the
	// next switcher_status frame back through ParseSnapshot and comparing.
	// Treating a nil error as confirmation is how a routing "fix" that never
	// applied gets reported to the operator as done.
	//
	// Sending zero commands is a no-op and not an error. The context bounds
	// the write; on cancellation, commands already written have already been
	// applied and are not rolled back.
	Send(ctx context.Context, cmds ...Command) error

	// Close releases the WebSocket. It is safe to call more than once, and a
	// Controller must not be used after it.
	Close() error
}

// NewController is implemented in controller.go (WP-M1), together with the
// *WSController it returns and the wire shape of every Command.
