// state.go — WP-M2. Parsing one switcher_status frame into a Snapshot.
//
// Everything here is READ-ONLY. Nothing in this file opens a socket, and
// nothing in it can change a mixer. It takes bytes and returns a value.
//
// The parsing style is deliberately paranoid, and the reason is in mixer.go's
// package comment: this snapshot is what tells an operator whether commentary
// is sitting in the clean feed. A parser that gives up on the whole frame
// because one meter arrived as a string would blank the drawer; a parser that
// silently zero-fills a field it could not read would state, confidently and
// wrongly, that a strip is not in the clean feed. So every field is decoded
// through a tolerant helper, every failure is recorded as a ParseWarning, and
// the failures that could produce a FALSE SAFETY CLAIM are marked Critical and
// surfaced as an error alongside a snapshot that is still usable.
//
// The frame this was written against is
// internal/m2lx/testdata/switcher_status-live-2026-07-31.json, an 84 KB capture
// from the live dev event. Every "MEASURED" note below cites it.
package mixer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// mixerNodeName is the node name of the audio mixer DSP node in a
// switcher_status frame.
//
// It must be matched EXACTLY. MEASURED: the same frame also carries a node
// plainly called "mixer" — the video mixer, with keys program/preview/
// transition/snapshots/animations — so a prefix, suffix or substring match
// would silently latch onto the wrong node and then report a mixer with no
// buses and no strips, i.e. "nothing is routed to the clean feed".
const mixerNodeName = "advanced_audio_mixer"

// wholeNodePath is the "path" of a status entry that carries a node's ENTIRE
// state, and the only value this parser will read a mixer node from.
//
// THE NODE NAME IS NOT ENOUGH, and matching on it alone was a live defect in
// this file. MEASURED, in the 3180-frame capture recorded at the top of
// internal/m2lx/wire.go: switcher_status is a SNAPSHOT-THEN-DELTA socket. The
// first frame after a connection opens carries all 36 nodes, every entry at
// "/", every state complete. Every frame after it carries ONE entry whose
// "path" addresses a SUBTREE of one node and whose "state" is the value AT
// THAT PATH — not the node. On this node in particular the deltas are
// relentless: "/levels" alone arrived 1501 times in 150 seconds, i.e. ten
// a second, and internal/m2lx/testdata/switcher_status-delta-levels-2026-08-01.json
// is one of them, captured verbatim.
//
// Read as a whole node, that entry makes the mixer's entire state
// {"levels":{...}} — no "matrix", no "inputs", so no strips and no routing.
// The drawer renders that as "nothing is in the clean feed", which is the most
// dangerous false statement this package can make, and the ONLY thing standing
// between the two was ParseWarning.Critical on "matrix": a caller following the
// documented handling — render the Snapshot AND the warnings — would have drawn
// an empty matrix. So the path is tested here, in the parser, and not only at
// whichever call site remembered to.
//
// It mirrors internal/m2lx/wire.go's constant of the same name and value. The
// two packages parse the same socket and neither can reach the other's
// unexported half; sharing it would mean exporting protocol trivia from a
// package whose contract is deliberately about mixers, not about wire formats.
const wholeNodePath = "/"

// displayNameField is the key carrying an operator-facing name on a per-input
// status node, e.g. the "cam22" node's "CLAUDE-COMMS". See Strip.DisplayName.
const displayNameField = "display_name"

// ErrNoMixerNode is returned when a frame carries no WHOLE advanced_audio_mixer
// entry — because it has none at all, or because the only one it has is a
// subtree delta. See wholeNodePath for why the second case is the same failure
// as the first, and errPartialMixerNode for the error that reports it: that one
// wraps this, so errors.Is keeps "this frame has no mixer state I can use" a
// single condition to test for.
//
// This is a hard error and never a degraded empty Snapshot, for the reason
// given on ParseSnapshot: a Snapshot with no strips renders as "nothing is
// routed to the clean feed", which is the most dangerous false statement this
// drawer can make. Callers must show the error, not an empty mixer.
var ErrNoMixerNode = errors.New("mixer: frame contains no " + mixerNodeName + " node")

// errPartialMixerNode reports a frame whose only advanced_audio_mixer entry is
// a subtree delta.
//
// It names the path, because "/levels" and "/peak_levels" mean the frame is
// simply a mid-stream push and the caller wants the next whole-document frame,
// whereas a path nobody has seen before means the device's vocabulary has moved
// and somebody should look. Both are the same refusal; only the diagnosis
// differs, and an error that will not say which is which wastes the one clue
// the frame gave.
func errPartialMixerNode(path string) error {
	return fmt.Errorf("mixer: the frame carries %s only as %s, not its whole state; "+
		"the state of such an entry is the value at that path, so reading it as the node "+
		"would show a mixer with no strips and no routing, i.e. %q: %w",
		mixerNodeName, describeDeltaPath(path), "nothing is in the clean feed", ErrNoMixerNode)
}

// describeDeltaPath renders a delta's path for an operator-facing message. An
// absent or unreadable path is named as such rather than printed as an empty
// pair of quotes, which reads like a bug in the message rather than a fact
// about the frame.
func describeDeltaPath(path string) string {
	if path == "" {
		return `an entry with no readable "path"`
	}
	return fmt.Sprintf("a %q subtree update", path)
}

// ParseWarning is one field the parser could not read, or read only partly.
//
// Warnings exist because the alternative behaviours are both unacceptable on a
// broadcast surface: failing the whole parse hides 53 good strips because of
// one bad one, and silently substituting a zero value invents state. A warning
// records exactly what was lost and where, so the drawer can say "this field is
// unavailable" instead of guessing.
type ParseWarning struct {
	// Path locates the field in the frame using dotted wire keys rooted at the
	// mixer node's state, e.g. "inputs.cam22.cam22-1.muted" or "matrix.cam9-1".
	// Keys are printed raw, so a path can be pasted into a JSON query.
	Path string `json:"path"`

	// Reason says what went wrong, in operator-readable words.
	Reason string `json:"reason"`

	// Critical marks a warning whose field, if rendered from its zero value,
	// would let an operator read a FALSE SAFETY CLAIM off the drawer.
	//
	// In practice that means routing: a strip whose matrix entry is missing or
	// unreadable parses to an empty Outputs, which renders as "not routed to
	// the clean feed" — indistinguishable from a strip that is genuinely
	// clear, and the exact mistake this package exists to prevent.
	//
	// Meter and fader losses are NOT critical, because the contract carries
	// Strip.Metered and BusState.FaderPresent precisely so that absence can be
	// drawn as absence.
	Critical bool `json:"critical"`
}

// String renders a warning for a log line or a drawer row.
func (w ParseWarning) String() string {
	if w.Critical {
		return "CRITICAL " + w.Path + ": " + w.Reason
	}
	return w.Path + ": " + w.Reason
}

// ParseError reports that a Snapshot parsed, but with at least one Critical
// ParseWarning.
//
// THE SNAPSHOT RETURNED ALONGSIDE THIS ERROR IS STILL USABLE and still carries
// every strip and bus that did parse. A caller that discards it on err != nil
// loses good state; a caller that ignores the error entirely presents unknown
// routing as clear routing. The intended handling is to render the snapshot AND
// the warnings together, so the operator sees which rows cannot be trusted.
type ParseError struct {
	// Warnings are all warnings from the parse, critical and not, in the same
	// deterministic order ParseSnapshotWithWarnings returns them.
	Warnings []ParseWarning `json:"warnings"`
}

// Error summarises the critical warnings. Non-critical warnings are counted but
// not spelled out, so the message stays readable when a frame degrades badly.
func (e *ParseError) Error() string {
	var crit []string
	other := 0
	for _, w := range e.Warnings {
		if w.Critical {
			crit = append(crit, w.Path+" ("+w.Reason+")")
			continue
		}
		other++
	}
	msg := fmt.Sprintf("mixer: %d field(s) could not be read and their routing must not be trusted: %s",
		len(crit), strings.Join(crit, "; "))
	if other > 0 {
		msg += fmt.Sprintf(" (plus %d non-critical warning(s))", other)
	}
	return msg
}

// CriticalWarnings returns only the warnings that could produce a false safety
// claim. Useful for a UI that wants to badge specific rows.
func (e *ParseError) CriticalWarnings() []ParseWarning {
	var out []ParseWarning
	for _, w := range e.Warnings {
		if w.Critical {
			out = append(out, w)
		}
	}
	return out
}

// ParseSnapshotWithWarnings parses a frame and reports everything it could not
// read.
//
// This is the full-fidelity entry point; ParseSnapshot in mixer.go is a thin
// wrapper over it that folds critical warnings into a *ParseError. Use this one
// when the caller wants to log or display the degraded fields, which the drawer
// should.
//
// The error result is non-nil ONLY when no usable Snapshot could be produced at
// all — the bytes are not JSON, or the frame carries no WHOLE mixer node, which
// covers both "no such entry" and "the only entry for it is a subtree delta"
// and is ErrNoMixerNode either way (see wholeNodePath). Any other problem is a
// warning against a Snapshot that is still worth rendering.
// Warnings are returned even alongside a fatal error, because the warnings
// collected before the failure usually say why it failed.
//
// Ordering of both results is deterministic: strips sorted by wire name, buses
// in AllBuses order, warnings sorted by path. A frame parsed twice yields
// byte-identical output, so a 1 Hz refresh does not reshuffle the drawer.
func ParseSnapshotWithWarnings(raw []byte) (Snapshot, []ParseWarning, error) {
	c := &collector{}

	// The envelope is decoded field-by-field into json.RawMessage rather than
	// into typed fields, because a single type surprise anywhere in an 84 KB
	// frame would otherwise fail the whole json.Unmarshal and lose everything.
	// MEASURED, and the reason this is not paranoia: the same frame carries
	// "frame_rate":"50" as a STRING while "width":1920 and "height":1080 are
	// numbers, so the device demonstrably does not type its JSON consistently.
	var frame struct {
		Status    []json.RawMessage `json:"status"`
		Timestamp json.RawMessage   `json:"timestamp"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		return Snapshot{}, nil, fmt.Errorf("mixer: parse switcher_status frame: %w", err)
	}

	idx := c.indexNodes(frame.Status)
	if idx.mixerState == nil {
		if idx.sawDelta {
			return Snapshot{}, c.sorted(), errPartialMixerNode(idx.deltaPath)
		}
		return Snapshot{}, c.sorted(), ErrNoMixerNode
	}
	mixerState, displayNames := idx.mixerState, idx.displayNames

	snap := Snapshot{TakenAt: c.frameTime(frame.Timestamp)}

	// matrix is the only sub-map whose loss is critical: it alone says which
	// buses a strip reaches, and an empty routing reads as "clear of the clean
	// feed". inputs, outputs, levels, peak_hold_levels and fader all degrade
	// into fields the contract can render as absent.
	matrix := c.subMap(mixerState, "matrix", true)
	inputs := c.subMap(mixerState, "inputs", false)
	outputs := c.subMap(mixerState, "outputs", false)
	levels := c.subMap(mixerState, "levels", false)
	peaks := c.subMap(mixerState, "peak_hold_levels", false)
	faders := c.subMap(mixerState, "fader", false)

	meters := meterSource{levels: levels, peaks: peaks, c: c}

	snap.Strips = c.buildStrips(inputs, matrix, faders, displayNames, meters)
	snap.Buses = c.buildBuses(outputs, faders, meters)
	c.checkUnclaimedMeters(levels, snap)

	return snap, c.sorted(), nil
}

// parseSnapshot is the implementation behind the exported ParseSnapshot
// declared in mixer.go, which owns that function's doc comment and signature.
//
// It folds the warning list into the (Snapshot, error) contract: critical
// warnings become a *ParseError carrying every warning, non-critical ones are
// dropped from this path because the contract's own absence flags — see
// Strip.Metered and BusState.FaderPresent — already render them.
func parseSnapshot(raw []byte) (Snapshot, error) {
	snap, warnings, err := ParseSnapshotWithWarnings(raw)
	if err != nil {
		return Snapshot{}, err
	}
	for _, w := range warnings {
		if w.Critical {
			return snap, &ParseError{Warnings: warnings}
		}
	}
	return snap, nil
}

// lookupStrip is the implementation behind the exported Snapshot.Strip method
// declared in mixer.go.
//
// Snapshot.Strips is sorted by Name by construction, so this binary-searches
// rather than scanning. Callers must not depend on that: the contract
// deliberately keeps the strategy private.
func (s Snapshot) lookupStrip(name string) (Strip, bool) {
	i := sort.Search(len(s.Strips), func(i int) bool { return s.Strips[i].Name >= name })
	if i < len(s.Strips) && s.Strips[i].Name == name {
		return s.Strips[i], true
	}
	return Strip{}, false
}

// ============================ warning collection ============================

// collector accumulates warnings during a parse. It is not safe for concurrent
// use and never escapes a single ParseSnapshotWithWarnings call.
type collector struct {
	warnings []ParseWarning
}

// warn records a non-critical degradation: a field that is absent or
// unreadable but whose absence the contract can render honestly.
func (c *collector) warn(path, format string, args ...any) {
	c.warnings = append(c.warnings, ParseWarning{Path: path, Reason: fmt.Sprintf(format, args...)})
}

// critical records a degradation that could produce a false safety claim. See
// ParseWarning.Critical.
func (c *collector) critical(path, format string, args ...any) {
	c.warnings = append(c.warnings, ParseWarning{Path: path, Reason: fmt.Sprintf(format, args...), Critical: true})
}

// sorted returns the warnings in a stable order: critical first, then by path,
// then by reason. Warnings are generated by ranging Go maps, whose order is
// randomised per run, so without this a re-parse of an identical frame would
// produce a differently-ordered list and any UI showing it would flicker.
func (c *collector) sorted() []ParseWarning {
	out := c.warnings
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Critical != out[j].Critical {
			return out[i].Critical
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// ============================== frame walking ===============================

// nodeIndex is what one pass over the status array found.
//
// It is a struct rather than a handful of results because two of its fields
// exist only to explain the absence of the first, and a caller that has to
// remember which of four returned values means "the mixer was there but only as
// a delta" will eventually stop checking it — which is precisely how the bug
// wholeNodePath describes survived.
type nodeIndex struct {
	// mixerState is the mixer node's WHOLE state, or nil if the frame carried
	// none. A subtree delta never lands here.
	mixerState map[string]json.RawMessage

	// displayNames maps an input's wire name to the operator-facing name from
	// its own status node, e.g. "cam22" -> "CLAUDE-COMMS".
	displayNames map[string]string

	// sawDelta records that an entry named the mixer node at a path other than
	// "/", and deltaPath and deltaAt are that entry's path and its position in
	// the status array. The FIRST such entry wins, so the message a frame
	// produces does not depend on how many deltas it happened to carry.
	sawDelta  bool
	deltaPath string
	deltaAt   string
}

// indexNodes makes one pass over the status array, returning the mixer node's
// whole state and the input-name -> operator-facing-name map built from every
// OTHER node's display_name.
//
// Both results come from the same pass because the display-name join is the
// whole reason ParseSnapshot takes the entire frame rather than the mixer node:
// the mixer node's own strip objects carry only the strip name repeated
// (MEASURED: strip cam22-1 has "display_name":"cam22-1"), while the name a
// human recognises — "CLAUDE-COMMS" — is on the per-input node "cam22".
//
// ONLY WHOLE-NODE ENTRIES ARE READ, mixer node and display names alike. See
// wholeNodePath for the mixer half, which is the dangerous one. The display
// names are held to the same rule for a smaller but real reason: an entry's
// state is the value at its path, so a delta at, say, "/streams" whose value
// happened to carry a display_name key would otherwise be harvested as the
// NODE's display name and put a wrong, plausible label on a strip — and a
// drawer that exists to let an operator find their own feed by its name must
// not invent one. Nothing observed does this; the rule costs one condition and
// removes the whole question.
//
// A malformed element is skipped with a warning rather than aborting the pass,
// so one bad node cannot cost the mixer node that follows it. A delta is
// skipped SILENTLY: deltas are the normal traffic on this socket, roughly
// twenty entries a second, and warning about each would drown the warnings that
// mean something in the ones that mean "the socket is working".
func (c *collector) indexNodes(status []json.RawMessage) nodeIndex {
	idx := nodeIndex{displayNames: make(map[string]string, len(status))}

	for i, element := range status {
		path := fmt.Sprintf("status[%d]", i)

		var node struct {
			Node  json.RawMessage `json:"node"`
			Path  json.RawMessage `json:"path"`
			State json.RawMessage `json:"state"`
		}
		if err := json.Unmarshal(element, &node); err != nil {
			c.warn(path, "not a status entry object: %v", err)
			continue
		}
		name, ok := decodeString(node.Node)
		if !ok {
			c.warn(path+".node", "node name is not a string")
			continue
		}

		entryPath, whole := readEntryPath(node.Path)
		if !whole {
			// Remembered, not warned about, and only for the mixer: it is the
			// difference between "this frame has no mixer in it" and "this
			// frame is a mid-stream meter push", which are the same refusal
			// with very different next steps for whoever reads the error.
			if name == mixerNodeName && !idx.sawDelta {
				idx.sawDelta = true
				idx.deltaPath = entryPath
				idx.deltaAt = path
			}
			continue
		}

		state, err := decodeObject(node.State)
		if err != nil {
			c.warn(path+".state", "state of node %q is not an object: %v", name, err)
			continue
		}

		if name == mixerNodeName {
			if idx.mixerState != nil {
				// Two mixer nodes in one frame is not something the device has
				// been seen to do; if it ever happens, silently taking the last
				// one would make the drawer's contents depend on array order.
				c.warn(path, "duplicate %s node ignored; using the first", mixerNodeName)
				continue
			}
			idx.mixerState = state
			continue
		}

		rawName, ok := state[displayNameField]
		if !ok {
			continue
		}
		display, ok := decodeString(rawName)
		if !ok {
			c.warn(path+"."+displayNameField, "display name of node %q is not a string", name)
			continue
		}
		if display = strings.TrimSpace(display); display != "" {
			idx.displayNames[name] = display
		}
	}

	// A frame carrying the mixer node BOTH whole and as a delta has never been
	// observed — the opening snapshot and the deltas that follow it are
	// different frames — and the whole node is used, because a partial value
	// can never be preferred to a complete one. It is reported because the two
	// disagree by construction: the delta is the newer reading of its subtree,
	// so a meter drawn from this frame may be one push stale. Not critical:
	// routing lives in "matrix", which a levels delta does not touch.
	if idx.mixerState != nil && idx.sawDelta {
		c.warn(idx.deltaAt, "%s also arrived as %s in the same frame; the whole-node entry is used and the update ignored",
			mixerNodeName, describeDeltaPath(idx.deltaPath))
	}
	return idx
}

// readEntryPath decodes a status entry's "path" and reports whether it
// addresses the WHOLE node.
//
// Anything that is not exactly "/" is treated as a subtree: an absent path, an
// empty one, a path that is not a string at all, and any real path such as
// "/levels". That is deliberately fail-closed and it is not symmetric with the
// rest of this file, where a field of the wrong type degrades to a warning. The
// asymmetry is the point — every other tolerated oddity costs one field, while
// getting this one wrong costs the whole matrix and shows an operator a clean
// feed that is not clean. A device that starts omitting "path" will produce a
// loud refusal here rather than a quiet empty mixer, and that is the right way
// round.
//
// The path is returned even when it is not whole so the caller can name it in
// the error; an unreadable one comes back as "", which describeDeltaPath
// renders as such.
func readEntryPath(raw json.RawMessage) (string, bool) {
	value, ok := decodeString(raw)
	if !ok {
		return "", false
	}
	return value, value == wholeNodePath
}

// frameTime converts the frame's own timestamp to a time.Time.
//
// MEASURED: the top-level "timestamp" is epoch MILLISECONDS — 1785522083212 is
// 2026-07-31. Reading it as seconds or as nanoseconds both land in 1970, which
// would make any staleness indicator on the drawer permanently wrong in a way
// that looks like a broken clock rather than a broken parser.
//
// An unreadable timestamp yields the zero time and a warning, never time.Now():
// substituting the local clock would make a frame that stopped arriving look
// perpetually fresh.
func (c *collector) frameTime(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		c.warn("timestamp", "absent; frame age is unknown")
		return time.Time{}
	}
	ms, ok := decodeFloat(raw)
	if !ok {
		c.warn("timestamp", "not a number; frame age is unknown")
		return time.Time{}
	}
	return time.UnixMilli(int64(ms)).UTC()
}

// subMap pulls one object-valued key out of the mixer node's state.
//
// critical says whether losing this key can produce a false safety claim; only
// "matrix" is critical, because it alone carries routing.
func (c *collector) subMap(state map[string]json.RawMessage, key string, critical bool) map[string]json.RawMessage {
	raw, ok := state[key]
	if !ok {
		c.report(critical, key, "absent from the %s node", mixerNodeName)
		return nil
	}
	obj, err := decodeObject(raw)
	if err != nil {
		c.report(critical, key, "not an object: %v", err)
		return nil
	}
	return obj
}

// report routes to warn or critical so callers can pass the flag through.
func (c *collector) report(critical bool, path, format string, args ...any) {
	if critical {
		c.critical(path, format, args...)
		return
	}
	c.warn(path, format, args...)
}

// ================================= strips ===================================

// stripSource is where a strip was discovered, which decides what is known
// about it.
type stripSource struct {
	input  string          // parent input wire name, e.g. "cam22"
	fields json.RawMessage // the strip object from inputs, nil if matrix-only
}

// buildStrips assembles every strip in the frame, sorted by wire name.
//
// Strips are discovered from BOTH inputs and matrix, and the union is taken
// rather than either alone:
//
//   - a strip in inputs but not in matrix has unknown routing, and dropping it
//     would hide a channel from the operator entirely;
//   - a strip in matrix but not in inputs is ROUTED — possibly to aux1, the
//     clean feed — and dropping it would hide exactly the condition this drawer
//     exists to expose.
//
// MEASURED on the live frame: the two sets are identical, 54 strips each. The
// union logic is for frames where they are not.
func (c *collector) buildStrips(inputs, matrix, faders map[string]json.RawMessage, displayNames map[string]string, meters meterSource) []Strip {
	found := make(map[string]stripSource, len(matrix))

	// Pass 1: the inputs tree. A strip lives INSIDE its input object, keyed by
	// strip name, ALONGSIDE keys that are not strips at all. MEASURED: every
	// one of the 27 inputs carries "assign_list" (an array) and
	// "channel_count" (a number) beside its two strip objects.
	for input, raw := range sortedKeysOf(inputs) {
		path := "inputs." + input
		obj, err := decodeObject(raw)
		if err != nil {
			c.warn(path, "input is not an object: %v", err)
			continue
		}
		for key, value := range obj {
			// Strips are detected STRUCTURALLY — an object carrying both
			// "muted" and "display_name" — and never by the "<input>-<n>" name
			// pattern. Pattern matching would break on the inputs whose names
			// already contain a space and a digit ("MIC 1"), and would silently
			// promote any future scalar key that happened to look like a strip
			// name into a phantom channel on the surface.
			if !looksLikeStrip(value) {
				continue
			}
			if prev, dup := found[key]; dup {
				c.warn(path+"."+key, "strip name also appears under input %q; using the first", prev.input)
				continue
			}
			found[key] = stripSource{input: input, fields: value}
		}
	}

	// Pass 2: strips known only from the routing matrix.
	for name := range matrix {
		if _, ok := found[name]; ok {
			continue
		}
		c.warn("matrix."+name, "strip is routed but absent from inputs; mute and fader are unknown")
		found[name] = stripSource{input: inputNameOf(name)}
	}

	strips := make([]Strip, 0, len(found))
	for name, src := range found {
		strips = append(strips, c.buildStrip(name, src, matrix, faders, displayNames, meters))
	}
	// Sorted by wire name, as Snapshot.Strips documents, and as
	// Snapshot.lookupStrip's binary search requires. Note this is byte order,
	// not natural order, so "cam10-1" precedes "cam2-1" — the same order the
	// device's own frame lists its keys in.
	sort.Slice(strips, func(i, j int) bool { return strips[i].Name < strips[j].Name })
	return strips
}

// buildStrip assembles one strip from every map that mentions it.
func (c *collector) buildStrip(name string, src stripSource, matrix, faders map[string]json.RawMessage, displayNames map[string]string, meters meterSource) Strip {
	s := Strip{
		Name:  name,
		Input: src.input,
	}

	// The display-name join, in the order Strip.DisplayName documents: the
	// per-input status node first, then the strip's own display_name, then the
	// raw wire name. MEASURED: for cam22-1 this resolves to "CLAUDE-COMMS"
	// from the "cam22" node, where the strip's own display_name is only
	// "cam22-1" again.
	s.DisplayName = displayNames[s.Input]

	if src.fields != nil {
		path := "inputs." + src.input + "." + name
		fields, err := decodeObject(src.fields)
		if err != nil {
			// looksLikeStrip already decoded this object once, so this is
			// unreachable in practice; handled rather than ignored because
			// "unreachable" is a claim about code, not about a live device.
			c.warn(path, "strip is not an object: %v", err)
		} else {
			if s.DisplayName == "" {
				if display, ok := decodeString(fields[displayNameField]); ok {
					s.DisplayName = strings.TrimSpace(display)
				}
			}
			s.Muted = c.boolField(fields, path, "muted")
			s.Follow = c.boolField(fields, path, "follow")
			s.FollowSources = c.stringsField(fields, path, "follow_sources")
			// The EFFECTIVE mode, not the requested one: see Strip.SubChMode.
			// MEASURED: cam22-2 reports sub_ch_mode "MONO" with
			// sub_ch_mode_set "ST_W", so the two keys genuinely disagree and
			// picking the wrong one misreports the channel as stereo.
			s.SubChMode = c.stringField(fields, path, "sub_ch_mode")
		}
	}
	if s.DisplayName == "" {
		s.DisplayName = name
	}

	c.applyRouting(&s, matrix)
	s.Level, s.PeakHold, s.Metered = meters.read(name, "strip")
	c.applyChFader(&s, faders)
	return s
}

// applyRouting fills Outputs and PFLOutputs from the routing matrix.
//
// Every failure here is CRITICAL. An empty Outputs renders as "this strip is
// not in the clean feed", and there is no field on Strip that distinguishes
// "measured clear" from "could not be read" — so the distinction has to travel
// as a warning or not at all.
func (c *collector) applyRouting(s *Strip, matrix map[string]json.RawMessage) {
	path := "matrix." + s.Name
	raw, ok := matrix[s.Name]
	if !ok {
		c.critical(path, "strip has no routing entry; its buses are unknown, NOT clear")
		return
	}
	var entry struct {
		Outputs    json.RawMessage `json:"outputs"`
		PFLOutputs json.RawMessage `json:"pfl_outputs"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		c.critical(path, "routing entry is malformed; its buses are unknown, NOT clear: %v", err)
		return
	}
	if len(entry.Outputs) == 0 {
		c.critical(path+".outputs", "absent; the strip's buses are unknown, NOT clear")
	} else {
		names, ok := decodeStrings(entry.Outputs)
		if !ok {
			c.critical(path+".outputs", "not an array of bus names; the strip's buses are unknown, NOT clear")
		} else {
			s.Outputs = toBuses(names)
		}
	}
	// pfl_outputs is not critical: PFL is a listen bus, so a wrong reading
	// there cannot put audio on an output that leaves the building. MEASURED:
	// empty for all 54 strips in the live frame.
	if len(entry.PFLOutputs) > 0 {
		names, ok := decodeStrings(entry.PFLOutputs)
		if !ok {
			c.warn(path+".pfl_outputs", "not an array of bus names; PFL routing not shown")
		} else {
			s.PFLOutputs = toBuses(names)
		}
	}
}

// applyChFader fills Fader and FaderEnabled from fader[strip].ch_fader.
//
// MEASURED: gain is PER CHANNEL, e.g. cam22-1 reports
// "gain":[-1.5748031,-1.5748031] with "enabled":[true,true], while most strips
// report [0,0] with [false,false]. The pair is kept rather than collapsed
// because one channel open and one at MuteFaderDB is a real, and dangerous,
// half-live state.
func (c *collector) applyChFader(s *Strip, faders map[string]json.RawMessage) {
	path := "fader." + s.Name
	raw, ok := faders[s.Name]
	if !ok {
		// Not critical: FaderEnabled stays [false,false], which the contract
		// tells renderers to present as a fader that is not in circuit.
		c.warn(path, "no fader entry; the strip's gain is unknown")
		return
	}
	var entry struct {
		ChFader struct {
			Enabled json.RawMessage `json:"enabled"`
			Gain    json.RawMessage `json:"gain"`
		} `json:"ch_fader"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		c.warn(path, "fader entry is malformed: %v", err)
		return
	}
	if gain, ok := decodeFloatPair(entry.ChFader.Gain); ok {
		s.Fader = gain
	} else if len(entry.ChFader.Gain) > 0 {
		c.warn(path+".ch_fader.gain", "not a pair of numbers; gain not shown")
	}
	if enabled, ok := decodeBoolPair(entry.ChFader.Enabled); ok {
		s.FaderEnabled = enabled
	} else if len(entry.ChFader.Enabled) > 0 {
		c.warn(path+".ch_fader.enabled", "not a pair of booleans; fader shown as not in circuit")
	}
}

// ================================== buses ===================================

// buildBuses assembles the bus list: all seven of AllBuses, in AllBuses order,
// followed by any bus the frame carries that this package does not know about.
//
// All seven are always emitted, even if the frame omits one, so the drawer's
// bus list — which includes the row labelled "aux1 (CLN - clean feed)" — never
// silently loses a line. An omitted bus is emitted with ChannelCount 0,
// Metered false and FaderPresent false, plus a warning.
//
// An UNKNOWN bus is appended rather than dropped, per BusLabel's rule that an
// eighth bus must show the operator something they can report. Consumers that
// assume exactly seven entries should iterate AllBuses instead of ranging this
// slice.
func (c *collector) buildBuses(outputs, faders map[string]json.RawMessage, meters meterSource) []BusState {
	buses := make([]BusState, 0, len(AllBuses))
	known := make(map[Bus]bool, len(AllBuses))
	for _, b := range AllBuses {
		known[b] = true
		buses = append(buses, c.buildBus(b, outputs, faders, meters))
	}

	var extra []Bus
	for name := range outputs {
		if !known[Bus(name)] {
			extra = append(extra, Bus(name))
		}
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i] < extra[j] })
	for _, b := range extra {
		c.warn("outputs."+string(b), "unknown bus not in AllBuses; shown but not understood")
		buses = append(buses, c.buildBus(b, outputs, faders, meters))
	}
	return buses
}

// buildBus assembles one bus.
func (c *collector) buildBus(b Bus, outputs, faders map[string]json.RawMessage, meters meterSource) BusState {
	state := BusState{Name: b}
	path := "outputs." + string(b)

	raw, ok := outputs[string(b)]
	if !ok {
		// Not critical: whether a strip reaches the clean feed is read from
		// matrix, not from here, so an absent bus row degrades detail rather
		// than the safety claim.
		c.warn(path, "bus absent from the frame; its mute state is unknown, shown unmuted")
	} else if fields, err := decodeObject(raw); err != nil {
		c.warn(path, "bus entry is not an object: %v", err)
	} else {
		state.Muted = c.boolField(fields, path, "muted")
		state.ChannelCount = c.intField(fields, path, "channel_count")
	}

	state.Level, state.PeakHold, state.Metered = meters.read(string(b), "bus")

	// MEASURED, and the reason FaderPresent exists: state.fader carries
	// master, aux1 and aux2 but NOT mon1..mon4. Zero-filling those four would
	// assert unity gain on buses the frame says nothing about.
	faderPath := "fader." + string(b)
	if raw, ok := faders[string(b)]; ok {
		var entry struct {
			OutputFader struct {
				Gain json.RawMessage `json:"gain"`
			} `json:"output_fader"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			c.warn(faderPath, "fader entry is malformed: %v", err)
		} else if gain, ok := decodeFloat(entry.OutputFader.Gain); ok {
			state.Fader = gain
			state.FaderPresent = true
		} else if len(entry.OutputFader.Gain) > 0 {
			c.warn(faderPath+".output_fader.gain", "not a number; fader shown as absent")
		}
	}
	return state
}

// ================================= meters ===================================

// meterSource reads the two meter maps together.
type meterSource struct {
	levels map[string]json.RawMessage
	peaks  map[string]json.RawMessage
	c      *collector
}

// read returns the level, the peak hold and whether the pair may be drawn.
//
// Metered requires BOTH maps to have supplied a usable pair. That is stricter
// than it looks and it is deliberate: the contract carries one Metered flag
// covering both fields, so if only one is readable the other would be drawn
// from its zero value — 0.0 dBFS, i.e. FULL SCALE — and a silent channel would
// appear on a broadcast surface as a meter pinned at the top.
//
// MEASURED: the live frame meters 34 keys — the 27 "-1" strips plus the 7
// buses — and does not meter any "-2" strip, so more than half the strips
// legitimately return false here. Absence is normal and must render as absent,
// never as silence.
func (m meterSource) read(name, kind string) (level, peak [2]float64, metered bool) {
	level, haveLevel := m.pair(m.levels, "levels", name, kind)
	peak, havePeak := m.pair(m.peaks, "peak_hold_levels", name, kind)
	if haveLevel != havePeak {
		m.c.warn("levels."+name, "%s has a level but no peak hold (or the reverse); meter not shown", kind)
		return [2]float64{}, [2]float64{}, false
	}
	return level, peak, haveLevel && havePeak
}

// pair reads one two-element dBFS array. A missing key is silent — most strips
// are legitimately unmetered — but a present-and-unreadable one warns.
func (m meterSource) pair(source map[string]json.RawMessage, mapName, name, kind string) ([2]float64, bool) {
	raw, ok := source[name]
	if !ok {
		return [2]float64{}, false
	}
	value, ok := decodeFloatPair(raw)
	if !ok {
		m.c.warn(mapName+"."+name, "%s meter is not a pair of numbers; meter not shown", kind)
		return [2]float64{}, false
	}
	return value, true
}

// checkUnclaimedMeters warns about meter keys that match neither a parsed strip
// nor a parsed bus.
//
// This is the parser checking its own strip detection. If a firmware change
// renamed strips, or the structural detection stopped recognising them, the
// symptom would be a drawer that quietly lists fewer channels — and the meter
// map, which names every audible thing, is the cheapest independent witness to
// that. MEASURED: on the live frame this produces nothing, because all 34 meter
// keys resolve to 27 strips and 7 buses.
func (c *collector) checkUnclaimedMeters(levels map[string]json.RawMessage, snap Snapshot) {
	if len(levels) == 0 {
		return
	}
	claimed := make(map[string]bool, len(snap.Strips)+len(snap.Buses))
	for _, s := range snap.Strips {
		claimed[s.Name] = true
	}
	for _, b := range snap.Buses {
		claimed[string(b.Name)] = true
	}
	var orphans []string
	for name := range levels {
		if !claimed[name] {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	for _, name := range orphans {
		c.warn("levels."+name, "metered but is neither a known strip nor a known bus; a channel may be missing from the drawer")
	}
}

// ============================= field decoding ===============================
//
// Every helper below tolerates the wrong JSON type rather than failing, and
// every caller reports what it lost. See the note on frame_rate in
// ParseSnapshotWithWarnings for why.

// boolField reads a boolean field, warning if it is present but unreadable. An
// absent field is silent and yields false.
func (c *collector) boolField(fields map[string]json.RawMessage, path, key string) bool {
	raw, ok := fields[key]
	if !ok {
		return false
	}
	value, ok := decodeBool(raw)
	if !ok {
		c.warn(path+"."+key, "not a boolean; read as false")
		return false
	}
	return value
}

// stringField reads a string field, warning if it is present but unreadable.
func (c *collector) stringField(fields map[string]json.RawMessage, path, key string) string {
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	value, ok := decodeString(raw)
	if !ok {
		c.warn(path+"."+key, "not a string; not shown")
		return ""
	}
	return value
}

// stringsField reads an array-of-strings field.
func (c *collector) stringsField(fields map[string]json.RawMessage, path, key string) []string {
	raw, ok := fields[key]
	if !ok {
		return nil
	}
	value, ok := decodeStrings(raw)
	if !ok {
		c.warn(path+"."+key, "not an array of strings; not shown")
		return nil
	}
	return value
}

// intField reads an integer field.
func (c *collector) intField(fields map[string]json.RawMessage, path, key string) int {
	raw, ok := fields[key]
	if !ok {
		return 0
	}
	value, ok := decodeFloat(raw)
	if !ok {
		c.warn(path+"."+key, "not a number; read as 0")
		return 0
	}
	return int(value)
}

// isJSONNull reports whether a raw value is the JSON literal null.
//
// This guard is load-bearing and it is NOT obvious. encoding/json unmarshals
// null into a float64, a bool, a string, a slice or a map WITHOUT returning an
// error, leaving the destination at its zero value. Every decoder below would
// therefore report SUCCESS on null and hand back a zero — and on this surface
// those zeros are all lies with a safe-looking face:
//
//	"muted":null      -> false, a strip reported as UNMUTED
//	"timestamp":null  -> 0,     a 2026 frame dated 1970
//	"outputs":null    -> [],    a strip reported as clear of the clean feed
//	"gain":null       -> 0 dB,  unity, on a fader that was never read
//
// All four were live defects in this file, caught by the tests rather than by
// review; TestDecodeObjectRejectsNull pins the stdlib behaviour that causes
// them so a future Go release cannot quietly change it underneath this package.
func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// decodeObject decodes a JSON object, rejecting null. Encoding/json unmarshals
// null into a nil map WITHOUT error, so a null sub-tree would otherwise look
// like an empty one — an empty matrix being read as "nothing is routed".
func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, errors.New("absent")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("null")
	}
	return obj, nil
}

// decodeString accepts a JSON string, and also a number rendered as one, since
// the device has been observed typing numeric fields as strings and there is no
// reason to assume it never does the reverse.
func decodeString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String(), true
	}
	return "", false
}

// decodeBool accepts a JSON boolean, or a string spelling one ("true", "false",
// "1", "0" — whatever strconv.ParseBool takes).
func decodeBool(raw json.RawMessage) (bool, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if parsed, err := strconv.ParseBool(strings.TrimSpace(s)); err == nil {
			return parsed, true
		}
	}
	return false, false
}

// decodeFloat accepts a JSON number or a numeric string. The string case is the
// frame_rate case: "50" where width and height are 1920 and 1080.
func decodeFloat(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

// decodeFloatPair reads a [left, right] pair.
//
// It requires at least two elements and uses the first two. Arrays longer than
// two are accepted rather than rejected — a future multichannel bus should
// degrade to its first two channels on a stereo meter, not vanish from it — but
// a shorter array is refused, because inventing a right channel from a left one
// would draw a stereo meter that is half fiction.
func decodeFloatPair(raw json.RawMessage) ([2]float64, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return [2]float64{}, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) < 2 {
		return [2]float64{}, false
	}
	var out [2]float64
	for i := 0; i < 2; i++ {
		value, ok := decodeFloat(items[i])
		if !ok {
			return [2]float64{}, false
		}
		out[i] = value
	}
	return out, true
}

// decodeBoolPair reads a two-element boolean array. See decodeFloatPair.
func decodeBoolPair(raw json.RawMessage) ([2]bool, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return [2]bool{}, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) < 2 {
		return [2]bool{}, false
	}
	var out [2]bool
	for i := 0; i < 2; i++ {
		value, ok := decodeBool(items[i])
		if !ok {
			return [2]bool{}, false
		}
		out[i] = value
	}
	return out, true
}

// decodeStrings reads an array of strings, rejecting the whole array if any
// element is not one.
//
// All-or-nothing is the right rule for a bus list specifically: silently
// dropping one unreadable element of ["master","aux1","aux2"] would produce a
// SHORTER, PLAUSIBLE routing that could omit aux1 — the clean feed — and
// nothing downstream could tell that from a strip genuinely routed clear.
//
// Note that elements are decoded STRICTLY, unlike everywhere else in this file:
// a bus name must actually be a JSON string. The tolerant decodeString would
// coerce the number 7 into the bus name "7", inventing a routing destination
// that never appeared in the frame. Tolerance is right for a display name and
// wrong for the list that decides what is in the clean feed.
func decodeStrings(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		// The null guard is needed per ELEMENT as well as for the array: a
		// null element unmarshals into a string without error and would enter
		// the routing list as the empty bus name "".
		if isJSONNull(item) {
			return nil, false
		}
		var value string
		if err := json.Unmarshal(item, &value); err != nil {
			return nil, false
		}
		out = append(out, value)
	}
	return out, true
}

// =============================== small helpers ==============================

// looksLikeStrip reports whether a value inside an input object is a channel
// strip, structurally: a JSON object carrying both "muted" and "display_name".
//
// MEASURED: this is what separates the two strip objects of an input from its
// "assign_list" (an array of arrays) and "channel_count" (a number). The keys
// are tested for PRESENCE, not for correct type, so that a strip whose muted
// flag arrives as the string "true" is still shown as a channel — with a
// warning from boolField — rather than disappearing from the drawer.
func looksLikeStrip(raw json.RawMessage) bool {
	obj, err := decodeObject(raw)
	if err != nil {
		return false
	}
	_, hasMuted := obj["muted"]
	_, hasName := obj[displayNameField]
	return hasMuted && hasName
}

// inputNameOf derives a parent input name from a strip name, for strips found
// only in the routing matrix.
//
// Strips are "<input>-<n>", and the split is on the LAST hyphen because input
// names are operator-set and can contain hyphens themselves. MEASURED: input
// names in the live frame include "MIC 1", which also rules out splitting on
// whitespace or assuming the name is alphanumeric. A name with no hyphen is
// returned unchanged rather than emptied, so the display-name join still has
// something to look up.
func inputNameOf(strip string) string {
	if i := strings.LastIndex(strip, "-"); i > 0 {
		return strip[:i]
	}
	return strip
}

// toBuses converts wire names to Bus values. Unknown names are preserved, not
// filtered: BusLabel renders them as "<name> (unknown bus)", and a routing
// destination this package does not recognise is exactly what an operator needs
// told about.
func toBuses(names []string) []Bus {
	out := make([]Bus, 0, len(names))
	for _, n := range names {
		out = append(out, Bus(n))
	}
	return out
}

// sortedKeysOf iterates a raw-message map in sorted key order.
//
// Go randomises map iteration order per run. Where a loop only fills a keyed
// result that is sorted later, that is harmless — but here it also emits
// warnings and resolves duplicate-name collisions ("using the first"), and both
// of those must be reproducible for the same frame.
func sortedKeysOf(m map[string]json.RawMessage) func(func(string, json.RawMessage) bool) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return func(yield func(string, json.RawMessage) bool) {
		for _, k := range keys {
			if !yield(k, m[k]) {
				return
			}
		}
	}
}
