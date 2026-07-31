package m2lx

import (
	"encoding/json"
	"fmt"
)

// wireNode is the shape of one entry in a switcher_status push, keyed by
// statusKey at the top level of the message: windows-app-spec.md §8 and
// CONTRACT.md describe the fields the app reads as
// "<statusKey>.stream_state", "<statusKey>.streams.video.format" and
// "<statusKey>.streams.audio[]". This is the wire type; Status (in
// m2lx.go) is the normalised type the rest of the app sees.
//
// Deliberately NOT modelled here: statistics.bitrate. It freezes at its
// last value, so a dead input would advertise a healthy bitrate forever
// (docs/architecture.md §4.8: "Never trust statistics.bitrate for
// liveness"; docs/test-results.md line 669 records the same finding). Do
// not add it here later — see the matching warning on Status in m2lx.go.
type wireNode struct {
	StreamState string      `json:"stream_state"`
	Streams     wireStreams `json:"streams"`
}

// wireStreams is <statusKey>.streams.
type wireStreams struct {
	Video wireVideo   `json:"video"`
	Audio []wireAudio `json:"audio"`
}

// wireVideo is <statusKey>.streams.video.
type wireVideo struct {
	Format string `json:"format"`
}

// wireAudio is one element of <statusKey>.streams.audio.
type wireAudio struct {
	Format string `json:"format"`
}

// extractNode finds statusKey's entry in a raw switcher_status message and
// unmarshals it.
//
// ok is false with a nil error when the message simply does not mention
// statusKey — that is not malformed, just not relevant to this snapshot,
// and must not be treated as a parse failure or cause a spurious Status.
//
// An error is returned only when the payload cannot be understood as a
// JSON object at all, or statusKey's entry cannot be understood as a node,
// so a genuinely unexpected shape is reported by name rather than causing
// a panic on a type assertion or silently producing a zero Status that
// reads as "everything is fine".
func extractNode(payload []byte, statusKey string) (node wireNode, ok bool, err error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return wireNode{}, false, fmt.Errorf("m2lx: switcher_status payload is not a JSON object: %w", err)
	}
	raw, present := top[statusKey]
	if !present {
		return wireNode{}, false, nil
	}
	if err := json.Unmarshal(raw, &node); err != nil {
		return wireNode{}, false, fmt.Errorf("m2lx: switcher_status node %q has an unexpected shape: %w", statusKey, err)
	}
	return node, true, nil
}

// extractAll unmarshals every node in a switcher_status message.
//
// It is extractNode's counterpart for the case where the caller does not yet
// know which node it wants — statusKey discovery, see discover.go.
//
// Where extractNode reports an entry it cannot understand as an error, this
// SKIPS it. The difference is deliberate: extractNode has been asked about one
// named node and silence about it would be a lie, whereas here a top-level key
// whose value is a string, a number or an object of some other shape is simply
// not a switcher node — the message is known to carry keys other than nodes —
// and refusing the whole snapshot because of one of them would make discovery
// impossible on an instance that has any.
func extractAll(payload []byte) (map[string]NodeState, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return nil, fmt.Errorf("m2lx: switcher_status payload is not a JSON object: %w", err)
	}

	nodes := make(map[string]NodeState, len(top))
	for key, raw := range top {
		var node wireNode
		if err := json.Unmarshal(raw, &node); err != nil {
			continue
		}
		// A node with no stream_state at all is not one: every documented
		// switcher_status node carries one, and a JSON object of some unrelated
		// shape unmarshals into wireNode's zero value without error.
		if node.StreamState == "" {
			continue
		}
		nodes[key] = NodeState{
			StreamState: node.StreamState,
			Video:       node.Streams.Video.Format,
			AudioCount:  len(node.Streams.Audio),
		}
	}
	return nodes, nil
}
