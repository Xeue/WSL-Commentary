package m2lx

import (
	"encoding/json"
	"strings"
)

// The mirrored switcher_status document
// -------------------------------------
//
// wire.go establishes the protocol from measurement: the FIRST frame after a
// connection opens is the whole document, 36 entries every one at path "/",
// and every frame after it is a SUBTREE delta — normally one entry, at ~21
// frames a second, whose "path" addresses a subtree of one node and whose
// "state" is the value AT THAT PATH.
//
// The previous change stopped a delta being misread as a whole node, which was
// condemning the only working input once a second. It did that by SKIPPING
// every delta, and the cost of that was the bug this file exists to close: the
// three WebSocket-derived lamps were correct at connect and then frozen for
// the rest of the session. An input that was "stopped" when the socket opened
// read "stopped" for ever, with streams.video.format and streams.audio[0]
// still null underneath it — which is why VIDEO read NO VIDEO and AUDIO read
// BAD FORMAT rather than going grey. They were rendering a stale truth
// faithfully.
//
// A document fixes that and nothing else can: a delta is only meaningful
// against the baseline it updates, so the baseline has to be kept.
//
// WHAT IS STORED. Each node's whole state, as the RAW BYTES that arrived. Not
// a decoded map: decoding and re-encoding a node would silently renormalise
// numbers and drop nothing visibly, and VideoFormat.Raw / AudioFormat.Raw
// (format.go) exist precisely so an operator can see what actually arrived. A
// node no delta has touched therefore still reads byte-for-byte as it came off
// the wire, and a node a delta HAS touched is re-encoded only along the path
// the delta named — for the measured "/statistics" delta that is the node's
// top level only, leaving "streams" untouched.
//
// WHERE THE LINE BETWEEN "MERGE" AND "SKIP" FALLS. Every rule below fails
// CLOSED, i.e. towards keeping the last known-good value and waiting for the
// next whole-document snapshot, because the alternative — inventing state — is
// how the previous two bugs in this area behaved. A delta is SKIPPED when:
//
//   - it names a node the baseline does not carry. A partial node assembled
//     out of subtrees would carry no stream_state, i.e. would read exactly
//     like "your statusKey names a node that is not a router input" — the
//     previous bug, re-created from the other end. This one rule also covers
//     "no snapshot has been seen yet on this connection", because only a
//     whole-node entry ever puts a node in the document: before the first
//     snapshot there is nothing for any delta to name. A separate "have we
//     been baselined?" flag was written first and deleted, because no test
//     could reach it — this check had already refused everything it would
//     have refused.
//   - its path is malformed: empty, not starting with "/", or containing an
//     empty token ("/a//b", "/a/"). None has ever been observed and none has
//     an unambiguous meaning.
//   - the path descends through something that is not a JSON object — an
//     array index, a scalar, a null. RFC 6901 permits array indices; this
//     device has never sent one, and guessing at array semantics against a
//     live desk is not worth the four lines it would save.
//
// A delta whose path names a key the baseline does not have is MERGED, with
// the missing intermediate objects created. That is the faithful reading of
// "the value at /a/b is X", and it is safe because it can only add; it can
// never turn a node into something it was not.
//
// A path of "/" never reaches the merge at all: that is wholeNodePath, and
// wireEntry.isWholeNode — the guard from the previous change, unchanged and
// still the only test applied — routes it to a whole-node replacement instead.

// document is one connection's mirror of the switcher_status document: the
// opening snapshot, with every subsequent subtree delta merged into it.
//
// It is not safe for concurrent use. One document belongs to one Watch loop
// goroutine and is thrown away and rebuilt whenever that loop opens a new
// connection, because a new connection begins a new snapshot and what the last
// one said may no longer be true.
type document struct {
	// nodes maps a node name to its whole state, as raw JSON. It is written
	// ONLY by whole-node entries; a delta can change what is under a node but
	// can never add one. That is what makes "no snapshot yet" and "a node the
	// snapshot did not carry" the same rule.
	nodes map[string]json.RawMessage
}

func newDocument() *document {
	return &document{nodes: make(map[string]json.RawMessage, 36)}
}

// frameEffect is what one frame did to the document.
type frameEffect struct {
	// Snapshot is true when the frame carried at least one whole-node entry,
	// i.e. it re-baselined the nodes it named. Every frame of the measured
	// opening snapshot does; no delta observed ever did.
	Snapshot bool

	// Touched names every node whose stored state this frame changed, whether
	// by whole-node replacement or by a merged delta. It is how the Watch loop
	// knows a frame is worth re-reading its own node for: a "/levels" delta
	// about advanced_audio_mixer touches nothing the lamps read, and there are
	// about fifteen of those a second.
	Touched map[string]bool

	// Merged and Skipped count the delta entries that did and did not apply.
	// They exist for tests and for anyone diagnosing a path vocabulary this
	// package has not met; nothing in the Watch loop reads them.
	Merged  int
	Skipped int
}

// apply merges one raw switcher_status frame into the document.
//
// An error means the payload is not a switcher_status frame at all, and
// nothing has been changed. Anything narrower — an entry that is not an
// object, a delta that cannot be merged — costs only itself, exactly as
// parseFrame's contract intends: one unreadable element must not lose the 35
// good ones beside it.
func (d *document) apply(payload []byte) (frameEffect, error) {
	entries, err := parseFrame(payload)
	if err != nil {
		return frameEffect{}, err
	}

	eff := frameEffect{Touched: make(map[string]bool, len(entries))}
	for _, raw := range entries {
		var e wireEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}
		if e.Node == "" || len(e.State) == 0 {
			continue
		}

		// isWholeNode is the guard from the previous change and is still the
		// ONLY test for "this entry is a whole node". A "/statistics" delta
		// fails it here just as it did before.
		if e.isWholeNode() {
			d.nodes[e.Node] = e.State
			eff.Snapshot = true
			eff.Touched[e.Node] = true
			continue
		}

		if d.mergeDelta(e.Node, e.Path, e.State) {
			eff.Merged++
			eff.Touched[e.Node] = true
		} else {
			eff.Skipped++
		}
	}
	return eff, nil
}

// mergeDelta applies one subtree delta and reports whether the document
// changed. Every reason it can decline is set out in the file comment; all of
// them leave the document exactly as it was.
func (d *document) mergeDelta(node, path string, state json.RawMessage) bool {
	// The whole "is there a baseline?" question, in one lookup: nothing but a
	// whole-node entry ever puts a node here.
	base, ok := d.nodes[node]
	if !ok {
		return false
	}
	tokens, ok := parsePath(path)
	if !ok || len(tokens) == 0 {
		// Malformed, or the root — and the root cannot reach here, because
		// isWholeNode has already claimed it. Either way there is nothing to
		// merge INTO a subtree of.
		return false
	}
	// state is not validated here. It arrived through parseFrame, which
	// unmarshals the "status" array element by element, so it is already known
	// to be well-formed JSON; and if a future caller ever hands over bytes that
	// are not, mergeAt's re-encode of the object holding them fails and the
	// merge is refused there. A json.Valid call here would be a branch no test
	// could reach, which is worse than no branch at all.
	merged, ok := mergeAt(base, tokens, state)
	if !ok {
		return false
	}
	d.nodes[node] = merged
	return true
}

// mergeAt returns obj with the value at tokens replaced by val, or ok false if
// the path cannot be followed through obj.
//
// It descends one level per call and re-encodes only the objects on the path,
// so every sibling subtree is carried across as the raw bytes that arrived.
// A missing intermediate key is created; a key that is present but is not a
// JSON object is refused, because the document and the path then disagree
// about the shape of the node and the device — not this package — is the one
// that gets to settle that, at the next whole-document snapshot.
func mergeAt(obj json.RawMessage, tokens []string, val json.RawMessage) (json.RawMessage, bool) {
	if len(tokens) == 0 {
		return val, true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(obj, &fields); err != nil {
		return nil, false
	}
	// A JSON null unmarshals into a map without error, leaving it nil. That is
	// not an object and must not be treated as an empty one.
	if fields == nil {
		return nil, false
	}

	child, present := fields[tokens[0]]
	if !present {
		child = json.RawMessage("{}")
	}
	sub, ok := mergeAt(child, tokens[1:], val)
	if !ok {
		return nil, false
	}
	fields[tokens[0]] = sub

	out, err := json.Marshal(fields)
	if err != nil {
		return nil, false
	}
	return out, true
}

// parsePath splits a switcher_status "path" into the tokens that address a
// subtree, and reports whether it is a path this package will act on.
//
// It is parsed as a GRAMMAR, not matched against the four paths that have been
// observed ("/statistics", "/levels", "/peak_levels", "/peak_hold_levels").
// Only four have ever been seen and there is no document listing the rest, so
// the fifth is going to arrive one day and it has to merge correctly or be
// skipped safely — never be mistaken for a whole node. Special-casing the four
// would have guaranteed the opposite.
//
// The escapes are RFC 6901's, applied in RFC 6901's order — "~1" before "~0",
// so a literal "~1" written as "~01" survives. None has been observed either,
// but a node whose state has a key with a "/" in it is not a strange idea on a
// device whose node names include "MIC 1".
//
// "/" alone yields zero tokens and ok: it is the whole node, which
// wireEntry.isWholeNode has already dealt with by the time anything calls
// this. Everything else that does not begin with "/", and anything with an
// empty token in it, is not a path this package understands.
func parsePath(p string) ([]string, bool) {
	if p == "" || p[0] != '/' {
		return nil, false
	}
	if p == wholeNodePath {
		return nil, true
	}
	parts := strings.Split(p[1:], "/")
	tokens := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, false
		}
		tokens = append(tokens, unescapeToken(part))
	}
	return tokens, true
}

// unescapeToken applies the RFC 6901 reference-token escapes: "~1" is "/" and
// "~0" is "~", in that order.
func unescapeToken(s string) string {
	if !strings.Contains(s, "~") {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}

// streamNode returns the merged state of a node that IS a router input.
//
// ok is false in all three ways a node is not one to read lamps from: it is
// not in the document, its state is not a shape this package understands, or
// it carries no stream_state. That last is the same test wire.go applies —
// stream_state, not display_name, is what makes a node a router input — and
// keeping it here means a merged document can never present "mixer" or
// "advanced_audio_mixer" as something the lamps could be driven from.
func (d *document) streamNode(name string) (wireNode, bool) {
	raw, ok := d.nodes[name]
	if !ok {
		return wireNode{}, false
	}
	var node wireNode
	if err := json.Unmarshal(raw, &node); err != nil {
		return wireNode{}, false
	}
	if node.StreamState == "" {
		return wireNode{}, false
	}
	return node, true
}
