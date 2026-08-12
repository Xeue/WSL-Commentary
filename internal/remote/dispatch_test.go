package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// The fake dispatcher
//
// It mirrors app_remote.go's policy shape closely enough to exercise the
// transport's enforcement seams: an ALLOWLIST (a method absent from the table is
// unknown even if the code could run it), a HOST-ONLY set (refused at every
// capability and absent from the hello methods list), and CAPABILITY TIERS
// (checked through the shipped Allows helper). It exists so internal/remote can
// be tested without importing the root package.
// ---------------------------------------------------------------------------

// fakeCap is the allowlist: method -> capability it requires. A method not in
// this map is UNKNOWN to the dispatcher, which is how "allowlist not blocklist"
// is expressed — being implemented is not the same as being callable.
var fakeCap = map[string]Capability{
	// view tier
	"GetConfig":        CapView,
	"GetMixerSnapshot": CapView,
	"ListPresets":      CapView,
	"GetActivePreset":  CapView,
	"SlowCall":         CapView, // the parkable call used by the cancel/close tests
	"BigCall":          CapView, // returns a large payload; used by the stalled-writer leak test
	// operate tier
	"Start":      CapOperate,
	"Stop":       CapOperate,
	"SaveConfig": CapOperate,
	"SetSecret":  CapOperate,
	"ArmMixer":   CapOperate,
	// mixer tier
	"SendMixerCommands": CapMixer,
	"SetMixerGolden":    CapMixer,
	// host-only (the capability recorded here is irrelevant — hostOnly refuses
	// them at every tier — but a value is needed so they appear "known").
	"SetPictureRect":    CapView,
	"SetPictureVisible": CapView,
	"StartPicture":      CapView,
	"StopPicture":       CapView,
	"StartReturn":       CapView,
	"StopReturn":        CapView,
}

// fakeHostOnly are the six methods that own the host's native overlay geometry
// and its headphones / SRT slots. They are refused for every remote client at
// every capability and omitted from the hello methods list.
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
// deliberately NOT in fakeCap, to prove a method absent from the allowlist is
// refused as unknown even though the dispatcher "implements" it.
const implementedButUnlisted = "SecretBackdoor"

type fakeDispatcher struct {
	mu     sync.Mutex
	called []string

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
	// NOT host-only and that the client's capabilities permit. Host-only methods
	// never appear regardless of capability; higher-tier methods appear only for
	// clients that hold the tier.
	var out []string
	for m, c := range fakeCap {
		if fakeHostOnly[m] {
			continue
		}
		if Allows(client.Caps, c) {
			out = append(out, m)
		}
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
	cap, known := fakeCap[method]
	if !known {
		return nil, fmt.Errorf("remote: unknown method %q", method)
	}
	// 2. Host-only. Refused for every remote client at every capability.
	if fakeHostOnly[method] {
		return nil, fmt.Errorf("remote: method %q is host-only", method)
	}
	// 3. Capability tier.
	if !Allows(client.Caps, cap) {
		return nil, fmt.Errorf("remote: method %q requires capability %q", method, cap)
	}

	// 4. Execute.
	if method == "BigCall" {
		// A large payload so that the in-flight results (maxInFlightCalls of
		// these — 32 MiB total) exceed any socket buffer a non-reading client
		// can absorb, making the write pump BLOCK. That is the precondition the
		// stalled-writer leak test needs; see
		// TestWritePumpDeathReapsAStalledFloodingClient. The wall-clock to reap
		// is dominated by a fixed Windows loopback timeout, not by this size, so
		// it is kept modest rather than fat.
		return strings.Repeat("x", 1024*1024), nil
	}
	if method == "SlowCall" {
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
	}
	return map[string]any{"ok": method, "args": len(args)}, nil
}

// ---------------------------------------------------------------------------
// Allows: the capability tier helper, unit-tested in isolation
// ---------------------------------------------------------------------------

func TestAllows_TierInclusion(t *testing.T) {
	cases := []struct {
		granted  []string
		required Capability
		want     bool
	}{
		{nil, CapView, false},                                // nothing grants nothing
		{[]string{"view"}, CapView, true},                    // exact
		{[]string{"view"}, CapOperate, false},                // view does not reach operate
		{[]string{"operate"}, CapView, true},                 // operate includes view
		{[]string{"operate"}, CapMixer, false},               // operate does not reach mixer
		{[]string{"mixer"}, CapOperate, true},                // mixer includes operate
		{[]string{"mixer"}, CapView, true},                   // mixer includes view
		{[]string{"bogus"}, CapView, false},                  // unknown grants nothing
		{[]string{"view", "bogus"}, CapView, true},           // unknown ignored, view stands
		{[]string{"operate"}, Capability("nonsense"), false}, // unknown requirement fails closed
	}
	for _, c := range cases {
		if got := Allows(c.granted, c.required); got != c.want {
			t.Errorf("Allows(%v, %q) = %v, want %v", c.granted, c.required, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 4: a method absent from the allowlist is refused as unknown
// ---------------------------------------------------------------------------

func TestDispatch_UnknownMethodRefused(t *testing.T) {
	h := newHarness(t, []Client{testClient(t, "op", "s3cret", string(CapMixer))})
	conn, _ := h.connect(t, "op", "s3cret")

	res := rpc(t, conn, 1, implementedButUnlisted)
	if res["ok"].(bool) {
		t.Fatal("SecretBackdoor was accepted; the allowlist must refuse it")
	}
	if msg, _ := res["error"].(string); !contains(msg, "unknown") {
		t.Fatalf("error = %q, want an 'unknown method' refusal", msg)
	}
}

// ---------------------------------------------------------------------------
// Test 5: host-only methods are refused at every capability and absent from hello
// ---------------------------------------------------------------------------

func TestDispatch_HostOnlyRefusedAtEveryCapability(t *testing.T) {
	// A client holding the HIGHEST tier still cannot call any host-only method.
	h := newHarness(t, []Client{testClient(t, "op", "s3cret", string(CapMixer))})
	conn, hello := h.connect(t, "op", "s3cret")

	// Absent from the hello methods list.
	methods := toStringSet(hello["methods"])
	for _, m := range hostOnlyList {
		if methods[m] {
			t.Errorf("host-only method %q appeared in the hello methods list", m)
		}
	}

	// Refused by Call, even at mixer capability.
	id := uint64(0)
	for _, m := range hostOnlyList {
		id++
		res := rpc(t, conn, id, m)
		if ok, _ := res["ok"].(bool); ok {
			t.Errorf("host-only method %q was accepted at mixer capability", m)
		}
		if msg, _ := res["error"].(string); !contains(msg, "host-only") {
			t.Errorf("host-only method %q error = %q, want a host-only refusal", m, msg)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 6: capability tiers gate the write-bearing methods
// ---------------------------------------------------------------------------

func TestDispatch_CapabilityTiers(t *testing.T) {
	view := newHarness(t, []Client{testClient(t, "v", "pw", string(CapView))})
	operate := newHarness(t, []Client{testClient(t, "o", "pw", string(CapOperate))})
	mixer := newHarness(t, []Client{testClient(t, "m", "pw", string(CapMixer))})

	vc, _ := view.connect(t, "v", "pw")
	oc, _ := operate.connect(t, "o", "pw")
	mc, _ := mixer.connect(t, "m", "pw")

	// view is refused Start (operate), SaveConfig (operate) and SendMixerCommands
	// (mixer), but allowed GetConfig (view).
	assertRefused(t, vc, 1, "Start", "requires capability")
	assertRefused(t, vc, 2, "SaveConfig", "requires capability")
	assertRefused(t, vc, 3, "SendMixerCommands", "requires capability")
	assertAccepted(t, vc, 4, "GetConfig")

	// operate is allowed Start and SaveConfig but refused SendMixerCommands.
	assertAccepted(t, oc, 1, "Start")
	assertAccepted(t, oc, 2, "SaveConfig")
	assertRefused(t, oc, 3, "SendMixerCommands", "requires capability")

	// mixer is allowed SendMixerCommands.
	assertAccepted(t, mc, 1, "SendMixerCommands")
}

// assertRefused sends a call and asserts it came back as an error result whose
// message contains wantSubstr.
func assertRefused(t *testing.T, conn *websocket.Conn, id uint64, method, wantSubstr string) {
	t.Helper()
	res := rpc(t, conn, id, method)
	if ok, _ := res["ok"].(bool); ok {
		t.Fatalf("%s was accepted, want refusal", method)
	}
	if msg, _ := res["error"].(string); !contains(msg, wantSubstr) {
		t.Fatalf("%s error = %q, want it to contain %q", method, msg, wantSubstr)
	}
}

// assertAccepted sends a call and asserts a successful result.
func assertAccepted(t *testing.T, conn *websocket.Conn, id uint64, method string) {
	t.Helper()
	res := rpc(t, conn, id, method)
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("%s was refused (%v), want acceptance", method, res["error"])
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
