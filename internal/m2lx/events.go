package m2lx

import (
	"context"
	"fmt"
	"net/http"
	"sort"
)

// eventsOverviewPath is GET /api/events/overview: the whole list of events on
// the instance, as a JSON array. It is bearer-authenticated exactly like the
// KVS calls — no tenant header, no Content-Type, no query parameters, no
// pagination (verified against the live instance and by reading the switcher's
// own web bundle on 2026-08-12).
//
// This is the endpoint an earlier pass wrongly concluded did not exist. The
// switcher's Angular app lists events through it; the /api/events base is used
// only for POST sub-paths (create/start/stop/…), which is why a probe of
// /api/events alone found nothing.
const eventsOverviewPath = "/api/events/overview"

// Event is one entry of GET /api/events/overview.
//
// Only the three fields the application needs are modelled — the id it must
// feed to the KVS calls, the name a human recognises, and the running state —
// out of a wire object that also carries endpoints[], region, timestamps and a
// user id. Unmodelled fields are ignored on decode rather than kept, because
// unlike the status format objects (VideoFormat.Raw) nothing here has to make
// an unexpected shape visible: an event either has an id or it is unusable, and
// ListEvents drops the ones that do not.
type Event struct {
	// ID is the wire field "event_id", e.g. "dl9-5p5ah0bd-empd". It is exactly
	// the string the KVS webrtc_info/webrtc_token calls take, and the last
	// path segment of the live-operation URL an operator pastes.
	ID string `json:"id"`

	// Name is the wire field "event_name", the human-readable label, e.g.
	// "MatchT". It is what a picker shows; it is never sent back to M2L-X.
	Name string `json:"name"`

	// Status is the wire field "status", e.g. "Running". Shown beside the name
	// so an operator choosing between several events can tell which is live. Not
	// interpreted here — the set of values is not fully enumerated, so no code
	// keys behaviour on it.
	Status string `json:"status"`
}

// eventWire is the decode target: M2L-X's field names, mapped to Event's.
type eventWire struct {
	EventID   string `json:"event_id"`
	EventName string `json:"event_name"`
	Status    string `json:"status"`
}

// ListEvents fetches the instance's events, newest-usable first.
//
// It is the mechanism behind "you should not have to know your event id": with
// a signed-in client the application can enumerate events and, when there is
// exactly one, choose it without asking; when there are several, show them by
// name and let the operator pick. The event id is otherwise an opaque string
// nobody remembers, previously recoverable only by pasting the browser URL.
//
// Events with no id are dropped, not returned: an id-less entry cannot be fed
// to the KVS calls, so offering it would produce a selection that fails later.
// The order is by name, case-insensitively, so a picker is stable rather than
// in whatever order the server happened to serialise — there is no "created"
// field common to every entry to sort on, and name is what the operator reads.
//
// It requires a bearer token (ErrNotSignedIn otherwise) and never includes the
// token in an error. An empty instance returns an empty slice and no error —
// "no events" is a state the caller renders, not a failure.
func (c *client) ListEvents(ctx context.Context) ([]Event, error) {
	tok := c.Token()
	if tok == "" {
		return nil, ErrNotSignedIn
	}

	var wire []eventWire
	if err := c.doJSON(ctx, http.MethodGet, eventsOverviewPath, nil, &wire, tok); err != nil {
		return nil, fmt.Errorf("m2lx: events overview: %w", err)
	}

	events := make([]Event, 0, len(wire))
	for _, e := range wire {
		if e.EventID == "" {
			continue
		}
		events = append(events, Event{ID: e.EventID, Name: e.EventName, Status: e.Status})
	}
	sort.SliceStable(events, func(i, j int) bool {
		return lessFold(events[i].Name, events[j].Name)
	})
	return events, nil
}

// lessFold is a case-insensitive string less-than, without allocating a lower
// copy of each side on every comparison the way strings.ToLower would.
func lessFold(a, b string) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		ca, cb := foldASCII(a[i]), foldASCII(b[i])
		if ca != cb {
			return ca < cb
		}
	}
	return len(a) < len(b)
}

func foldASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
