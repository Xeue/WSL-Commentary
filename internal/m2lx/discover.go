package m2lx

import (
	"sort"
	"time"
)

// Finding the statusKey
// ---------------------
//
// `statusKey` names the switcher_status node that is our router input. Nothing
// in the M2L-X REST API will say which one it is: there is no event list, no
// input list, and /api/user/me returns identity only. Specification open
// question 5 proposed a way to find it, and this file used to be only that:
//
//	"read one switcher_status snapshot and find the node whose stream_state
//	 changes when the app starts."
//
// The 2026-07-31 capture (see wire.go) showed there is something much better in
// the frame, which the old parser never reached because it was looking at the
// wrong level of the document: every node carries state.display_name — the name
// the OPERATOR gave that input in M2L-X and sees on the switcher. cam22 is
// "CLAUDE-COMMS". A human matching that against the name they typed themselves
// is direct evidence; a node that happened to come up while we were starting is
// circumstantial. So discovery now offers the whole list of router inputs with
// their display names and what each is doing (Choices), and keeps the
// transition as a strong hint on top of it (Candidates, and NodeChoice's
// Transitioned).
//
// The two rules that keep this honest rather than merely convenient are
// unchanged:
//
//   - the result is a SUGGESTION, never a saved value. Another operator's input
//     coming up in the same few seconds is indistinguishable from ours from
//     outside, and silently persisting a guess would put three green lamps
//     against somebody else's feed — the single worst failure this application
//     can have, because it reads as confirmation.
//   - when more than one node matches, all of them are reported. Picking the
//     first is picking at random and calling it knowledge.

// NodeState is one switcher_status node as seen in a single frame: enough to
// tell whether it is streaming and to describe it to the operator, and nothing
// more.
type NodeState struct {
	// DisplayName is state.display_name: the input's name in M2L-X, chosen by
	// whoever configured the event and recognisable to the operator —
	// "CLAUDE-COMMS", "REPLAY 1 CLN". Empty when the node has none.
	DisplayName string `json:"displayName"`

	// StreamState is the node's stream_state verbatim.
	StreamState string `json:"streamState"`

	// Video is a rendering of streams.video.format — see VideoFormat.Raw for
	// exactly what it looks like and why. It is deliberately the unparsed
	// rendering rather than a verdict: it is shown to the operator as
	// corroboration, and a format this application's parser does not
	// understand is still evidence.
	Video string `json:"video"`

	// AudioCount is how many entries streams.audio had.
	AudioCount int `json:"audioCount"`
}

// NodeChoice is one router input offered to a human who has to say which one is
// theirs: what M2L-X calls it, what they call it, and what it is doing now.
//
// This is the list a statusKey is actually chosen from. StatusKeyCandidate
// below is the subset of it that came up while we were starting.
type NodeChoice struct {
	// Key is the node name — the value that would go in config.json.
	Key string `json:"key"`

	// DisplayName is the input's name in M2L-X: the thing the operator
	// recognises and picks on.
	DisplayName string `json:"displayName"`

	// StreamState is what the node is doing in the most recent frame.
	StreamState string `json:"streamState"`

	// Video renders streams.video.format; see NodeState.Video.
	Video string `json:"video"`

	// AudioCount is how many entries streams.audio had.
	AudioCount int `json:"audioCount"`

	// Transitioned is true when this node was seen to START streaming while
	// discovery was running — the old evidence, kept as a hint rather than as
	// the whole answer.
	Transitioned bool `json:"transitioned"`

	// AfterSeconds is how long after discovery started that happened. Zero and
	// meaningless when Transitioned is false.
	AfterSeconds float64 `json:"afterSeconds"`
}

// Document is one whole switcher_status frame, normalised: every node in it,
// keyed by the name that would be the statusKey.
type Document struct {
	// Nodes is every top-level entry that parsed as a switcher node.
	Nodes map[string]NodeState `json:"nodes"`

	// At is when the frame was received.
	At time.Time `json:"at"`
}

// StatusKeyCandidate is one node that started streaming while discovery was
// running: a possible statusKey, with what it matched on.
//
// The JSON tags are the shape the frontend receives, so the Settings screen can
// state the evidence rather than just the name.
type StatusKeyCandidate struct {
	// Key is the node name — the value that would go in config.json.
	Key string `json:"key"`

	// DisplayName is the input's name in M2L-X. It is the field the operator
	// should actually be matching on: "is that the name I gave my input?" is a
	// question they can answer, where "did that node come up two seconds after
	// I pressed START?" is one they can only guess at.
	DisplayName string `json:"displayName"`

	// Was is the node's stream_state in the baseline snapshot, or "absent" if
	// the node was not in it at all.
	Was string `json:"was"`

	// Now is the stream_state it changed to. Always StreamStateStreaming: a
	// change to anything else is not evidence of our feed arriving.
	Now string `json:"now"`

	// Video is streams.video.format at the moment of the change.
	Video string `json:"video"`

	// AudioCount is the number of audio streams at the moment of the change.
	AudioCount int `json:"audioCount"`

	// AfterSeconds is how long after discovery started the change was seen.
	// Nearest tenth of a second; the socket pushes about once a second, so more
	// precision than that would be invented.
	AfterSeconds float64 `json:"afterSeconds"`
}

// NodeAbsent is the Was value for a node that was not in the baseline snapshot.
const NodeAbsent = "absent"

// Discovery accumulates switcher_status Documents and reports which nodes
// transitioned INTO streaming since the first one it saw.
//
// It is pure: no sockets, no clock of its own, no I/O. Feed it Documents in the
// order they arrived and ask it for Candidates. It is not safe for concurrent
// use; the one goroutine reading the WatchAll channel owns it.
type Discovery struct {
	// baseline is the stream_state of every node in the first Document, which
	// is the "before" this whole method rests on. Discovery must therefore be
	// started BEFORE the feed comes up, which is what App.Start does.
	baseline map[string]string
	started  time.Time
	seeded   bool

	// latest is the best current picture of every node, which is what Choices
	// reports. It is MERGED, not replaced, because the socket is
	// snapshot-then-delta (wire.go): the first Document carries the whole
	// switcher and later ones carry only the nodes whose whole state was
	// re-sent. Replacing would throw the switcher away on the first delta and
	// leave the operator with nothing to pick from.
	latest map[string]NodeState

	// candidates is keyed by node name, and an entry is written ONCE — at the
	// first transition into streaming. A node that later stops and starts again
	// keeps its original timing, because the first transition is the one that
	// coincided with our feed.
	candidates map[string]StatusKeyCandidate
}

// NewDiscovery returns an empty Discovery. The first Document given to Observe
// becomes the baseline and can never itself produce a candidate.
func NewDiscovery() *Discovery {
	return &Discovery{
		baseline:   make(map[string]string),
		latest:     make(map[string]NodeState),
		candidates: make(map[string]StatusKeyCandidate),
	}
}

// Observe folds one Document in and reports whether the candidate list changed.
//
// A node counts as a candidate when its stream_state is StreamStateStreaming
// now and was something else — or the node was absent — in the baseline. A node
// that was ALREADY streaming in the baseline never becomes a candidate however
// long discovery runs: it was up before we were, so it is not us.
func (d *Discovery) Observe(doc Document) bool {
	for key, node := range doc.Nodes {
		d.latest[key] = node
	}

	if !d.seeded {
		d.seeded = true
		d.started = doc.At
		for key, node := range doc.Nodes {
			d.baseline[key] = node.StreamState
		}
		return false
	}

	changed := false
	for key, node := range doc.Nodes {
		if node.StreamState != StreamStateStreaming {
			continue
		}
		if _, already := d.candidates[key]; already {
			continue
		}
		was, present := d.baseline[key]
		if present && was == StreamStateStreaming {
			continue
		}
		if !present {
			was = NodeAbsent
		}
		d.candidates[key] = StatusKeyCandidate{
			Key:          key,
			DisplayName:  node.DisplayName,
			Was:          was,
			Now:          node.StreamState,
			Video:        node.Video,
			AudioCount:   node.AudioCount,
			AfterSeconds: roundTenths(doc.At.Sub(d.started).Seconds()),
		}
		changed = true
	}
	return changed
}

// Started reports whether a baseline has been taken yet. A discovery that never
// got one saw no switcher_status frames at all, which is a different problem
// from seeing frames and matching nothing — and the operator should be told
// which of the two happened.
func (d *Discovery) Started() bool { return d.seeded }

// Candidates returns the matches, soonest first, then by name so the order is
// stable between calls. The earliest transition is the most likely to be ours,
// but only marginally, which is why they are all returned.
func (d *Discovery) Candidates() []StatusKeyCandidate {
	out := make([]StatusKeyCandidate, 0, len(d.candidates))
	for _, c := range d.candidates {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AfterSeconds != out[j].AfterSeconds {
			return out[i].AfterSeconds < out[j].AfterSeconds
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Choices returns every router input in the most recent frame, described so an
// operator can pick theirs by the name they know it by.
//
// This is the list a statusKey should be chosen from, and Candidates above is
// the subset of it that came up while we were starting. Both are offered
// because they answer different questions: Choices answers "which of these is
// mine?", which a human can settle by reading a name they chose themselves,
// and Candidates answers "which came up when I did?", which is a hint and can
// point at somebody else's input just as easily as at ours.
//
// The order puts the strongest evidence first: nodes that were seen to start
// streaming, soonest first; then nodes that are streaming now; then the rest by
// display name, falling back to node name so it is stable between frames.
//
// Always non-nil, so a caller rendering a list never has to special-case it.
func (d *Discovery) Choices() []NodeChoice {
	out := make([]NodeChoice, 0, len(d.latest))
	for key, node := range d.latest {
		c := NodeChoice{
			Key:         key,
			DisplayName: node.DisplayName,
			StreamState: node.StreamState,
			Video:       node.Video,
			AudioCount:  node.AudioCount,
		}
		if cand, ok := d.candidates[key]; ok {
			c.Transitioned = true
			c.AfterSeconds = cand.AfterSeconds
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Transitioned != b.Transitioned {
			return a.Transitioned
		}
		if a.Transitioned && a.AfterSeconds != b.AfterSeconds {
			return a.AfterSeconds < b.AfterSeconds
		}
		if !a.Transitioned {
			sa := a.StreamState == StreamStateStreaming
			sb := b.StreamState == StreamStateStreaming
			if sa != sb {
				return sa
			}
			if a.DisplayName != b.DisplayName {
				return a.DisplayName < b.DisplayName
			}
		}
		return a.Key < b.Key
	})
	return out
}

// roundTenths rounds to one decimal place, and turns a negative elapsed time —
// which a clock stepping backwards mid-discovery can produce — into zero rather
// than a candidate that appears to have arrived before it was looked for.
func roundTenths(s float64) float64 {
	if s < 0 {
		return 0
	}
	return float64(int64(s*10+0.5)) / 10
}
