package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// The fake dispatcher
//
// It mirrors app_remote.go's post-auth policy shape closely enough to exercise
// the transport's enforcement seams: an ALLOWLIST (a method absent from the
// table is unknown even if the code could run it) and a HOST-ONLY set (refused
// for every connection and absent from the hello methods list). There are NO
// capability tiers — the listener is unauthenticated by the owner's decision, so
// every connection sees and may call every non-host-only method. It also models
// mixer ARM-OWNERSHIP keyed on the connection id, so the transport-level
// property "only the seat that armed may write" has a test. It exists so
// internal/remote can be tested without importing the root package.
// ---------------------------------------------------------------------------

// fakeKnown is the allowlist: a method not in this set is UNKNOWN to the
// dispatcher, which is how "allowlist not blocklist" is expressed — being
// implemented is not the same as being callable.
var fakeKnown = map[string]bool{
	"GetConfig":         true,
	"GetMixerSnapshot":  true,
	"ListPresets":       true,
	"GetActivePreset":   true,
	"SlowCall":          true, // the parkable call used by the cancel/close tests
	"BigCall":           true, // returns a large payload; used by the stalled-writer leak test
	"Start":             true,
	"Stop":              true,
	"SaveConfig":        true,
	"SetSecret":         true,
	"ArmMixer":          true,
	"SendMixerCommands": true,
	"SetMixerGolden":    true,
	// host-only: present so they read "known", but hostOnly refuses them.
	"SetPictureRect":    true,
	"SetPictureVisible": true,
	"StartPicture":      true,
	"StopPicture":       true,
	"StartReturn":       true,
	"StopReturn":        true,
}

// fakeHostOnly are the six methods that own the host's native overlay geometry
// and its headphones / SRT slots. They are refused for every connection and
// omitted from the hello methods list.
var fakeHostOnly = map[string]bool{
	"SetPictureRect":    true,
	"SetPictureVisible": true,
	"StartPicture":      true,
	"StopPicture":       true,
	"StartReturn":       true,
	"StopReturn":        true,
}

// hostOnlyList is the six, for table-driven tests.
var hostOnlyList = []string{
	"SetPictureRect", "SetPictureVisible", "StartPicture", "StopPicture", "StartReturn", "StopReturn",
}

// implementedButUnlisted names a method the fake could execute but which is
// deliberately NOT in fakeKnown, to prove a method absent from the allowlist is
// refused as unknown even though the dispatcher "implements" it.
const implementedButUnlisted = "SecretBackdoor"

type fakeDispatcher struct {
	mu      sync.Mutex
	called  []string
	armedBy string // connection id that most recently armed; "" if none

	entered     chan string   // SlowCall announces entry here
	cancelledCh chan struct{} // SlowCall signals here when its ctx is cancelled
}

func newFakeDispatcher() *fakeDispatcher {
	return &fakeDispatcher{
		entered:     make(chan string, 8),
		cancelledCh: make(chan struct{}, 8),
	}
}

func (d *fakeDispatcher) Methods(client ClientInfo) []string {
	// The authoritative list the shim installs: every allowlisted method that is
	// NOT host-only. With no capability tiers, every connection sees the same
	// list.
	var out []string
	for m := range fakeKnown {
		if fakeHostOnly[m] {
			continue
		}
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func (d *fakeDispatcher) Call(ctx context.Context, client ClientInfo, method string, args []json.RawMessage) (any, error) {
	d.mu.Lock()
	d.called = append(d.called, method)
	d.mu.Unlock()

	// 1. Allowlist. Unknown is refused even if some code path could serve it —
	//    which is the whole point of an allowlist over a blocklist.
	if !fakeKnown[method] {
		return nil, fmt.Errorf("remote: unknown method %q", method)
	}
	// 2. Host-only. Refused for every connection.
	if fakeHostOnly[method] {
		return nil, fmt.Errorf("remote: method %q is host-only", method)
	}

	// 3. Execute.
	switch method {
	case "ArmMixer":
		// Record the arming seat, keyed on the connection id the transport hands
		// us. This is what lets a later SendMixerCommands enforce arm-ownership.
		d.mu.Lock()
		d.armedBy = client.ID
		d.mu.Unlock()
		return map[string]any{"armed": true}, nil
	case "SendMixerCommands":
		d.mu.Lock()
		owner := d.armedBy
		d.mu.Unlock()
		if owner == "" || owner != client.ID {
			// Not armed, or armed by a DIFFERENT seat: refused. With two
			// controllers, one operator's arm must not authorise the other's write.
			return nil, fmt.Errorf("remote: SendMixerCommands refused: the mixer was armed by another seat")
		}
		return map[string]any{"ok": true}, nil
	case "BigCall":
		// A large payload so that the in-flight results (maxInFlightCalls of these
		// — 32 MiB total) exceed any socket buffer a non-reading client can absorb,
		// making the write pump BLOCK. That is the precondition the stalled-writer
		// leak test needs; see TestWritePumpDeathReapsAStalledFloodingClient.
		return strings.Repeat("x", 1024*1024), nil
	case "SlowCall":
		select {
		case d.entered <- method:
		default:
		}
		<-ctx.Done()
		select {
		case d.cancelledCh <- struct{}{}:
		default:
		}
		return nil, ctx.Err()
	default:
		return map[string]any{"ok": method, "args": len(args)}, nil
	}
}

// ---------------------------------------------------------------------------
// A method absent from the allowlist is refused as unknown
// ---------------------------------------------------------------------------

func TestDispatch_UnknownMethodRefused(t *testing.T) {
	h := newHarness(t)
	conn, _ := h.connect(t)

	res := rpc(t, conn, 1, implementedButUnlisted)
	if res["ok"].(bool) {
		t.Fatal("SecretBackdoor was accepted; the allowlist must refuse it")
	}
	if msg, _ := res["error"].(string); !contains(msg, "unknown") {
		t.Fatalf("error = %q, want an 'unknown method' refusal", msg)
	}
}

// ---------------------------------------------------------------------------
// Host-only methods are refused for every connection and absent from hello
// ---------------------------------------------------------------------------

func TestDispatch_HostOnlyRefused(t *testing.T) {
	h := newHarness(t)
	conn, hello := h.connect(t)

	// Absent from the hello methods list.
	methods := toStringSet(hello["methods"])
	for _, m := range hostOnlyList {
		if methods[m] {
			t.Errorf("host-only method %q appeared in the hello methods list", m)
		}
	}

	// Refused by Call.
	id := uint64(0)
	for _, m := range hostOnlyList {
		id++
		res := rpc(t, conn, id, m)
		if ok, _ := res["ok"].(bool); ok {
			t.Errorf("host-only method %q was accepted", m)
		}
		if msg, _ := res["error"].(string); !contains(msg, "host-only") {
			t.Errorf("host-only method %q error = %q, want a host-only refusal", m, msg)
		}
	}
}

// ---------------------------------------------------------------------------
// small test helpers
// ---------------------------------------------------------------------------

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func toStringSet(v any) map[string]bool {
	out := map[string]bool{}
	arr, ok := v.([]any)
	if !ok {
		return out
	}
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out[s] = true
		}
	}
	return out
}
