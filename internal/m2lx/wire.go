package m2lx

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The switcher_status wire shape
// ------------------------------
//
// MEASURED on 2026-07-31 against the live instance while this app was
// streaming into it, and captured verbatim as
// testdata/switcher_status-live-2026-07-31.json. That file is the authority
// for everything in this file; nothing here is inferred from prose.
//
// One frame is an OBJECT with two keys, and every node lives in an ARRAY:
//
//	{"status":[{"node":"cam22","path":"/","state":{...}}, ...],
//	 "timestamp":1785522083212}
//
// This is NOT what windows-app-spec.md §8 and CONTRACT.md describe. They name
// the fields the lamps read as "<statusKey>.stream_state",
// "<statusKey>.streams.video.format" and "<statusKey>.streams.audio[]", which
// reads as "statusKey is a property at the top level". It is not, and never
// was: statusKey is the value of an entry's "node" field, and everything the
// lamps want is nested under that entry's "state". The parser this replaced
// looked up statusKey as a top-level property, so it could never match with
// ANY key — which is why the three WebSocket-derived lamps had never worked.
// The docs are wrong; the capture is right.
//
// "timestamp" is deliberately not modelled. It is M2L-X's clock. Every time in
// this package comes from the receiving process's own clock, so that a
// switcher whose clock has stopped cannot defeat the staleness rule.
//
// The measured frame carries 36 entries: 24 that carry a stream_state (cam1-10,
// cam14-24 and "MIC 1"-"MIC 3" — note the spaces, node names are not
// identifiers) and 12 that do not (advanced_audio_mixer, discovery1, lipsync,
// live_recorder, media_transfer, mixer, output_recorder, replay1, router,
// tally, vtr1, vtr2). Some of the latter — replay1, vtr1, vtr2 — carry a
// display_name but no streams, so display_name alone does not make a node a
// router input. stream_state is the test.
//
// The socket is a SNAPSHOT-THEN-DELTA protocol
// -------------------------------------------
//
// MEASURED 2026-08-01 over 150 s and 3180 frames on the live instance. The
// captured 84 KB fixture is a single frame and CANNOT show this; it took
// watching the socket to find it, and everything written about this socket
// before — spec §8, CONTRACT.md, docs/architecture.md — describes it as
// though every frame were a full snapshot. It is not:
//
//   - The FIRST frame after a connection opens is a full snapshot: all 36
//     nodes, every entry with path "/", every state complete. 84 KB.
//   - Every frame after it is a DELTA: normally ONE entry, about a SUBTREE
//     of one node, at roughly 21 frames a second. "path" says which subtree
//     and "state" is the value AT THAT PATH — not the whole node.
//
// The paths observed in 3180 frames were, in full:
//
//	3180 x  path "/"                  — the opening snapshot only, once
//	1501 x  advanced_audio_mixer "/levels"
//	1500 x  advanced_audio_mixer "/peak_levels"
//	 163 x  advanced_audio_mixer "/peak_hold_levels"
//	  15 x  cam1 "/statistics"        — the one node that was streaming
//
// So "path" is load-bearing and NOT decoration. testdata's
// switcher_status-delta-statistics-2026-08-01.json is the trap in one frame:
//
//	{"node":"cam1","path":"/statistics","state":{"bitrate":6523.6,...}}
//
// A parser that ignores path unmarshals that state into a node, finds no
// stream_state in it, and concludes that cam1 — the only input on the switcher
// that was actually working — is not a router input. Once a second, forever.
// Hence wholeNodePath below: an entry at "/" carries a whole node and nothing
// else does. isWholeNode is the only test applied, here and in document.go.
//
// THE DELTAS ARE NOW MERGED, not skipped. Skipping them was the first fix and
// it closed the misreading above at the cost of a worse bug: the lamps were
// then correct at connect and frozen for the rest of the session, an input
// that was stopped when the socket opened reading STOPPED, NO VIDEO and BAD
// FORMAT however loudly it was actually streaming. The document that makes a
// delta meaningful — the opening snapshot, patched by JSON pointer at each
// delta's path — is document.go, and every rule it applies to a path it has
// never seen is written out at the top of that file.
//
// WHAT IS STILL NOT KNOWN, and it is the same thing as before: whether a
// stream_state change is pushed AT ALL, at "/" or at any subtree path. No
// input changed state during the 150 s window, and finding out means making
// one change state, which this package must not do to a live switcher. If it
// turns out a transition is never pushed, merging deltas cannot help and only
// a reconnect can — which is what watcher.go's resyncInterval is, explicitly a
// backstop against this one unproven assumption. Whoever observes a real
// transition should record the (node, path) of the frames it produces here,
// and then delete that backstop.

// wholeNodePath is the "path" of an entry that carries a node's ENTIRE state.
// Every entry of the opening snapshot has it; no delta observed ever did.
const wholeNodePath = "/"

// wireEntry is one element of the "status" array.
//
// State is held raw so that one entry whose state is an object of an entirely
// unrelated shape — twelve of the thirty-six measured entries are exactly that
// — can be skipped or reported by the caller, rather than failing the frame
// that also carries the node we actually want.
type wireEntry struct {
	Node  string          `json:"node"`
	Path  string          `json:"path"`
	State json.RawMessage `json:"state"`
}

// isWholeNode reports whether this entry carries a complete node state rather
// than a partial update of one of its subtrees.
func (e wireEntry) isWholeNode() bool { return e.Node != "" && e.Path == wholeNodePath }

// wireNode is one entry's "state" when that entry is a router input: the
// fields the lamps read, and nothing else.
//
// Deliberately NOT modelled here: statistics.bitrate. It freezes at its last
// value, so a dead input would advertise a healthy bitrate forever
// (docs/architecture.md line 669, docs/windows-app-spec.md §8). The measured
// frame has it — 507.4 on cam22, 6932.9 on cam1 — and it must stay unread and
// unexposed. Do not add it here later; see the matching warning on Status in
// m2lx.go.
type wireNode struct {
	// DisplayName is the name the operator gave this input in M2L-X and sees
	// on the switcher: "CLAUDE-COMMS" for cam22, "REPLAY 1 CLN" for cam7. It
	// is the only field in the whole frame a human can recognise their own
	// feed by, which makes it the thing statusKey discovery offers them.
	DisplayName string `json:"display_name"`

	StreamState string      `json:"stream_state"`
	Streams     wireStreams `json:"streams"`
}

// wireStreams is state.streams.
type wireStreams struct {
	Video wireVideo   `json:"video"`
	Audio []wireAudio `json:"audio"`
}

// wireVideo is state.streams.video. Beside "format" it carries an "error"
// object, which is {"code":"","message":"","severity":"none"} on every one of
// the measured nodes, healthy and stopped alike; with no sample of it ever
// saying anything else there is nothing to read from it, so it is not modelled.
type wireVideo struct {
	Format json.RawMessage `json:"format"`
}

// wireAudio is one element of state.streams.audio.
type wireAudio struct {
	Format json.RawMessage `json:"format"`
}

// Where the line between "error" and "tolerate" falls
// ---------------------------------------------------
//
// CONTAINERS — state, stream_state, streams, streams.video, streams.audio and
// its elements — are modelled with types, so a shape that is not the measured
// one is an error for the node the caller ASKED about and a skip for a node
// merely being scanned. That distinction was sound in the parser this replaces
// and is kept.
//
// The FORMAT leaf is held raw and never errors. An unrecognised format is
// rendered into VideoFormat.Raw / AudioFormat.Raw and the typed fields are left
// zero, so the lamp goes red with the offending value written on it. Refusing
// the whole frame would instead grey the lamp and say nothing, which is the
// silent failure this rewrite exists to remove.

// parseFrame splits one switcher_status push into its raw entries.
//
// Entries are returned raw rather than decoded so that one unreadable element
// — a number, a string, anything that is not an entry object — costs only
// itself. The measured frame has none, which is exactly why it cannot be the
// only thing this is tested against.
func parseFrame(payload []byte) ([]json.RawMessage, error) {
	var frame struct {
		Status *json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal(payload, &frame); err != nil {
		return nil, fmt.Errorf("m2lx: switcher_status payload is not a JSON object: %w", err)
	}
	if frame.Status == nil {
		return nil, fmt.Errorf(`m2lx: switcher_status payload has no "status" array`)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(*frame.Status, &entries); err != nil {
		return nil, fmt.Errorf(`m2lx: switcher_status "status" is not an array: %w`, err)
	}
	return entries, nil
}

// StatusKeyNotFoundError reports that the configured statusKey names nothing
// this application can watch, on a connection whose opening snapshot has been
// read and understood perfectly.
//
// It exists because a WRONG statusKey used to be indistinguishable from a
// blank one. Neither produced a Status; staleness never fired, because frames
// WERE arriving, just not about the node we asked for; so the three lamps read
// "NO STATUS" forever and nothing anywhere said why. Now that the opening
// snapshot enumerates every node, "cam99 is not one of them" is a fact, and
// this is how it is stated — naming what was looked for and what is there
// instead.
//
// It is raised against the SNAPSHOT, never against a delta. A delta mentions
// one node and says nothing whatever about the other 35; treating a node's
// absence from one as evidence would condemn a perfectly good statusKey
// twenty times a second. See watcher.go's run loop for where the distinction
// is enforced.
type StatusKeyNotFoundError struct {
	// Key is the statusKey that was looked for.
	Key string

	// NotAnInput is true when a node of that name IS on the switcher but
	// carries no stream_state: "mixer", "router" or "tally" rather than a
	// typo. It is a different mistake from a name that is not there at all,
	// and an operator who has made one of them should not be told they made
	// the other.
	NotAnInput bool

	// Available is every node a statusKey could legitimately name — the ones
	// that carry a stream_state. The twelve that do not are left out on
	// purpose: offering "advanced_audio_mixer" as an alternative would be
	// offering a node that can never work.
	Available []NodeChoice
}

func (e *StatusKeyNotFoundError) Error() string {
	var b strings.Builder
	if e.NotAnInput {
		fmt.Fprintf(&b, "m2lx: statusKey %q names a switcher_status node that is not a router input: "+
			"it carries no stream_state, so it can never drive the status lamps", e.Key)
	} else {
		fmt.Fprintf(&b, "m2lx: statusKey %q names no node on the switcher at %s", e.Key, "M2L-X")
	}
	if len(e.Available) == 0 {
		b.WriteString("; no router inputs were found at all")
		return b.String()
	}
	fmt.Fprintf(&b, "; the %d it can be, as \"node\" (display name, stream_state), are: ", len(e.Available))
	for i, c := range e.Available {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q (%s, %s)", c.Key, displayNameOrUnnamed(c.DisplayName), c.StreamState)
	}
	return b.String()
}

// displayNameOrUnnamed keeps an empty display_name from rendering as a blank
// gap in the middle of a list the operator is meant to read.
func displayNameOrUnnamed(name string) string {
	if name == "" {
		return "unnamed"
	}
	return name
}

// lookup is one switcher_status frame resolved against one statusKey.
//
// It reports what THIS FRAME said and nothing more. On the snapshot that opens
// a connection that is the whole switcher; on the deltas that follow it is
// usually one unrelated node's audio meters. Deciding whether a statusKey is
// wrong needs both, and is the run loop's job — see watcher.go.
type lookup struct {
	// Node is statusKey's node, valid only when Found.
	Node  wireNode
	Found bool

	// NotAnInput is true when this frame carried statusKey's whole state and
	// it has no stream_state. That IS conclusive from a single frame: the node
	// exists and is not a router input.
	NotAnInput bool

	// Inputs is every router input whose WHOLE state this frame carried.
	// Empty for every delta observed; all 24 of them for the opening snapshot.
	Inputs []NodeChoice
}

// lookupNode resolves one raw switcher_status frame against one statusKey.
//
// An error means the payload is not a switcher_status frame at all, or that
// statusKey's own entry has a shape this package does not understand — the
// latter reported by name, so a surprise about the node we were ASKED about is
// never silently turned into a zero Status that reads as "all fine". A node
// merely being scanned is skipped instead; that distinction was sound in the
// parser this replaces and is kept.
func lookupNode(payload []byte, statusKey string) (lookup, error) {
	entries, err := parseFrame(payload)
	if err != nil {
		return lookup{}, err
	}

	var out lookup
	out.Inputs = make([]NodeChoice, 0, len(entries))

	for _, raw := range entries {
		var e wireEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}
		if !e.isWholeNode() {
			// A partial update of a subtree. Not something this package can
			// merge, and emphatically not something it may read as a node:
			// cam1's "/statistics" delta would otherwise pass for a router
			// input with no stream_state.
			continue
		}
		var node wireNode
		if err := json.Unmarshal(e.State, &node); err != nil {
			if e.Node == statusKey {
				return lookup{}, fmt.Errorf("m2lx: switcher_status node %q has an unexpected shape: %w", statusKey, err)
			}
			continue
		}
		if node.StreamState == "" {
			if e.Node == statusKey {
				out.NotAnInput = true
			}
			continue
		}
		if e.Node == statusKey {
			out.Node = node
			out.Found = true
		}
		out.Inputs = append(out.Inputs, nodeChoice(e.Node, node))
	}

	sortChoices(out.Inputs)
	return out, nil
}

// extractAll reads every router input in a switcher_status frame.
//
// It is extractNode's counterpart for the case where the caller does not yet
// know which node it wants — statusKey discovery, see discover.go.
//
// Where extractNode reports an entry it cannot understand as an error, this
// SKIPS it. The difference is deliberate: extractNode has been asked about one
// named node and silence about it would be a lie, whereas here an entry that
// is not a router input is simply not what is being looked for. Twelve of the
// thirty-six measured entries are exactly that, so refusing the frame over one
// of them would make discovery impossible on any real instance.
func extractAll(payload []byte) (map[string]NodeState, error) {
	entries, err := parseFrame(payload)
	if err != nil {
		return nil, err
	}
	nodes := make(map[string]NodeState, len(entries))
	for _, raw := range entries {
		name, node, ok := decodeStreamEntry(raw)
		if !ok {
			continue
		}
		nodes[name] = nodeState(node)
	}
	return nodes, nil
}

// decodeStreamEntry decodes one raw entry and reports whether it carries the
// WHOLE state of a router input: an entry object, with a node name, at path
// "/", whose state carries a stream_state. Everything else — an entry that is
// not an object, a node with no name, a delta about a subtree, a state that is
// not an object, a mixer or a router or a tally — is simply not one.
func decodeStreamEntry(raw json.RawMessage) (string, wireNode, bool) {
	var e wireEntry
	if err := json.Unmarshal(raw, &e); err != nil || !e.isWholeNode() {
		return "", wireNode{}, false
	}
	var node wireNode
	if err := json.Unmarshal(e.State, &node); err != nil || node.StreamState == "" {
		return "", wireNode{}, false
	}
	return e.Node, node, true
}

// nodeChoice describes one router input for an operator who has to pick theirs.
func nodeChoice(name string, node wireNode) NodeChoice {
	return NodeChoice{
		Key:         name,
		DisplayName: node.DisplayName,
		StreamState: node.StreamState,
		Video:       parseVideoFormat(node.Streams.Video.Format).Raw,
		AudioCount:  len(node.Streams.Audio),
	}
}

// sortChoices puts streaming nodes first — an operator hunting for their own
// feed is looking for one that is up — then orders by node name, so the list is
// stable between frames.
func sortChoices(cs []NodeChoice) {
	sort.Slice(cs, func(i, j int) bool {
		si := cs[i].StreamState == StreamStateStreaming
		sj := cs[j].StreamState == StreamStateStreaming
		if si != sj {
			return si
		}
		return cs[i].Key < cs[j].Key
	})
}

// nodeState reduces a decoded node to what discovery and the operator need.
func nodeState(node wireNode) NodeState {
	return NodeState{
		DisplayName: node.DisplayName,
		StreamState: node.StreamState,
		Video:       parseVideoFormat(node.Streams.Video.Format).Raw,
		AudioCount:  len(node.Streams.Audio),
	}
}
