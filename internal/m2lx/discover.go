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
// question 5 says how to find it and this file is that method, mechanised:
//
//	"read one switcher_status snapshot and find the node whose stream_state
//	 changes when the app starts."
//
// The two rules that keep this honest rather than merely convenient:
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
	// StreamState is the node's stream_state verbatim.
	StreamState string `json:"streamState"`

	// Video is streams.video.format verbatim, e.g. "h264 1920x1080 50 P". It is
	// unparsed on purpose: it is shown to the operator as corroboration, and a
	// format this application's parser does not understand is still evidence.
	Video string `json:"video"`

	// AudioCount is how many entries streams.audio had.
	AudioCount int `json:"audioCount"`
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

// roundTenths rounds to one decimal place, and turns a negative elapsed time —
// which a clock stepping backwards mid-discovery can produce — into zero rather
// than a candidate that appears to have arrived before it was looked for.
func roundTenths(s float64) float64 {
	if s < 0 {
		return 0
	}
	return float64(int64(s*10+0.5)) / 10
}
