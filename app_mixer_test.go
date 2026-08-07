//go:build dev || production || bindings

// Tests for the mixer drawer's half of the bound surface (app_mixer.go).
//
// Owner: WP-8.
//
// Two properties here are worth more than the rest put together, because they
// are the ones a green build does not prove:
//
//   - a write attempted while disarmed reaches nothing (TestSendMixerCommands…
//     Disarmed). The Go arm gate is the second of two independent gates, and
//     the whole value of a second gate is that it is not the one the caller can
//     see. If this host ever grew a path to the mixer that did not go through
//     mixer.Controller.Send, the drawer would look identical and the gate would
//     be gone.
//   - GetMixerSnapshot reads the socket on EVERY call (TestGetMixerSnapshot…
//     NeverServesACachedFrame). drawer.js marks its view fresh at the moment it
//     adopts a frame, so a cache here silently defeats the drawer's whole S2
//     protection and nothing on screen would say so.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"wslcomms/internal/m2lx"
	"wslcomms/internal/mixer"
)

// liveFrameFixture is the captured whole-document frame every read test parses.
// It is owned by internal/m2lx; these tests skip rather than fail if it moves,
// so a change on that side cannot break this one.
const liveFrameFixture = "internal/m2lx/testdata/switcher_status-live-2026-07-31.json"

// deltaFrameFixture is a real advanced_audio_mixer "/levels" delta: the frame
// shape that must never be read as a whole mixer state.
const deltaFrameFixture = "internal/m2lx/testdata/switcher_status-delta-levels-2026-08-01.json"

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("fixture %s is unavailable: %v", path, err)
	}
	return data
}

// ---------------------------------------------------------------------------
// A fake write controller
// ---------------------------------------------------------------------------

// fakeMixerController is a mixer.Controller with the same gate semantics as the
// real one and no socket.
//
// The gate is reimplemented rather than stubbed open, because the property
// under test is that this host routes every write through Send and lets Send
// decide. A fake that accepted everything would pass whether or not the host
// had grown a bypass.
type fakeMixerController struct {
	mu     sync.Mutex
	until  time.Time
	closed bool
	closes int
	sent   [][]mixer.Command
	fail   error
}

func (c *fakeMixerController) Send(_ context.Context, cmds ...mixer.Command) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(cmds) == 0 {
		return nil
	}
	if c.closed {
		return mixer.ErrClosed
	}
	if c.until.IsZero() || !time.Now().Before(c.until) {
		return mixer.ErrDisarmed
	}
	if c.fail != nil {
		return c.fail
	}
	c.sent = append(c.sent, cmds)
	return nil
}

func (c *fakeMixerController) Arm(window time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || window <= 0 {
		c.until = time.Time{}
		return time.Time{}
	}
	c.until = time.Now().Add(window)
	return c.until
}

func (c *fakeMixerController) Disarm() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until = time.Time{}
}

func (c *fakeMixerController) ArmedUntil() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.until
}

func (c *fakeMixerController) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes++
	c.closed = true
	c.until = time.Time{}
	return nil
}

// expire shuts the window the way the real gate's deadline does when it passes:
// without anybody calling Disarm.
func (c *fakeMixerController) expire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until = time.Now().Add(-time.Second)
}

func (c *fakeMixerController) batches() [][]mixer.Command {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]mixer.Command, len(c.sent))
	copy(out, c.sent)
	return out
}

func (c *fakeMixerController) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

var _ mixer.Controller = (*fakeMixerController)(nil)

// withFakeMixer installs a signed-in control plane and a fake write controller,
// and returns both.
func withFakeMixer(t *testing.T, a *App) (*stubWatcher, *fakeMixerController) {
	t.Helper()
	w := withStubControlPlane(t, a)

	a.ctlMu.Lock()
	a.client = stubClient{token: "test-token"}
	a.ctlMu.Unlock()

	ctl := &fakeMixerController{}
	a.mixerDial = func(host, token string) (mixer.Controller, error) {
		if host == "" || token == "" {
			t.Errorf("mixerDial got host=%q token=%q, want both set", host, token)
		}
		return ctl, nil
	}
	return w, ctl
}

// ---------------------------------------------------------------------------
// The bound surface
// ---------------------------------------------------------------------------

// The SET of bound methods is asserted by assertBoundSurface in app_test.go,
// which already owns that contract and now lists these six. What that test
// cannot see is the SHAPE of each one, which is what the frontend actually
// calls against — hence the next test.

// TestMixerBindingShapes checks that each mixer binding has the signature the
// frontend calls it with. A method that exists with the wrong arity or the
// wrong parameter type fails at runtime inside WebView2, where the only symptom
// is a rejected promise.
func TestMixerBindingShapes(t *testing.T) {
	typ := reflect.TypeOf(&App{})

	tests := []struct {
		method string
		in     []reflect.Type
		out    []reflect.Type
	}{
		{"GetMixerSnapshot", nil, []reflect.Type{reflect.TypeOf(mixer.Snapshot{}), errType}},
		{"ArmMixer", nil, []reflect.Type{reflect.TypeOf(MixerArmState{}), errType}},
		{"DisarmMixer", nil, []reflect.Type{errType}},
		{"SendMixerCommands", []reflect.Type{reflect.TypeOf([]MixerCommand(nil))}, []reflect.Type{errType}},
		{"GetMixerGolden", nil, []reflect.Type{reflect.TypeOf(&mixer.Snapshot{}), errType}},
		{"SetMixerGolden", []reflect.Type{reflect.TypeOf(&mixer.Snapshot{})}, []reflect.Type{errType}},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			m, ok := typ.MethodByName(tt.method)
			if !ok {
				t.Fatalf("App has no method %s", tt.method)
			}
			ft := m.Type
			// NumIn includes the receiver.
			if ft.NumIn()-1 != len(tt.in) {
				t.Fatalf("%s takes %d argument(s), want %d", tt.method, ft.NumIn()-1, len(tt.in))
			}
			for i, want := range tt.in {
				if got := ft.In(i + 1); got != want {
					t.Errorf("%s argument %d is %s, want %s", tt.method, i+1, got, want)
				}
			}
			if ft.NumOut() != len(tt.out) {
				t.Fatalf("%s returns %d value(s), want %d", tt.method, ft.NumOut(), len(tt.out))
			}
			for i, want := range tt.out {
				if got := ft.Out(i); got != want {
					t.Errorf("%s result %d is %s, want %s", tt.method, i, got, want)
				}
			}
		})
	}
}

var errType = reflect.TypeOf((*error)(nil)).Elem()

// TestSnapshotJSONMatchesTheDrawerContract checks the JSON keys the drawer
// reads off a snapshot.
//
// frontend/src/ui/mixer/contract.js documents these names as the Go JSON tags
// and frontend/src/ui/mixer/model.js indexes them directly. A rename on the Go
// side would not fail any Go test, and in the drawer it would render as an
// empty matrix — "nothing is in the clean feed".
func TestSnapshotJSONMatchesTheDrawerContract(t *testing.T) {
	snap, _, err := mixer.ParseSnapshotWithWarnings(readFixture(t, liveFrameFixture))
	if err != nil {
		t.Fatalf("parsing the live fixture: %v", err)
	}

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshalling the snapshot: %v", err)
	}
	var doc struct {
		Strips []map[string]json.RawMessage `json:"strips"`
		Buses  []map[string]json.RawMessage `json:"buses"`
		Taken  string                       `json:"takenAt"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("the snapshot does not carry strips/buses/takenAt: %v", err)
	}
	if len(doc.Strips) == 0 || len(doc.Buses) == 0 {
		t.Fatalf("snapshot has %d strips and %d buses, want both non-empty", len(doc.Strips), len(doc.Buses))
	}
	if doc.Taken == "" {
		t.Error("takenAt is empty; the drawer shows it as the frame age")
	}

	for _, key := range []string{
		"name", "input", "displayName", "muted", "outputs", "pflOutputs",
		"level", "peakHold", "metered", "fader", "faderEnabled", "subChMode",
	} {
		if _, ok := doc.Strips[0][key]; !ok {
			t.Errorf("strip is missing %q, which the drawer reads", key)
		}
	}
	for _, key := range []string{"name", "muted", "channelCount", "level", "peakHold", "metered", "fader", "faderPresent"} {
		if _, ok := doc.Buses[0][key]; !ok {
			t.Errorf("bus is missing %q, which the drawer reads", key)
		}
	}
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// TestGetMixerSnapshotNeverServesACachedFrame is the host-side guarantee
// contract.js requires, asserted rather than asserted-in-prose.
//
// drawer.js's applySnapshot sets lastUpdateAt to the local clock at the moment
// it adopts a frame, so a cached frame makes an arbitrarily old matrix look
// live — and set_routing REPLACES a strip's whole bus set from that matrix.
func TestGetMixerSnapshotNeverServesACachedFrame(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	w, _ := withFakeMixer(t, a)
	w.setRaw(readFixture(t, liveFrameFixture), nil)

	for i := 1; i <= 3; i++ {
		if _, err := a.GetMixerSnapshot(); err != nil {
			t.Fatalf("GetMixerSnapshot() call %d error = %v", i, err)
		}
		if got := w.rawSnapshotCalls(); got != i {
			t.Fatalf("after %d calls the status socket was read %d time(s); a cached frame defeats the drawer's freshness gate", i, got)
		}
	}
}

// TestGetMixerSnapshotParsesTheLiveFrame is the guard on the refusal policy in
// GetMixerSnapshot: it refuses a frame with any CRITICAL parse warning, so if
// the captured live frame produced one the drawer would be permanently
// unusable rather than merely cautious.
func TestGetMixerSnapshotParsesTheLiveFrame(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	w, _ := withFakeMixer(t, a)
	w.setRaw(readFixture(t, liveFrameFixture), nil)

	snap, err := a.GetMixerSnapshot()
	if err != nil {
		t.Fatalf("GetMixerSnapshot() on the captured live frame error = %v", err)
	}
	if len(snap.Strips) == 0 {
		t.Fatal("the live frame parsed to zero strips, which renders as \"nothing is in the clean feed\"")
	}
	if len(snap.Buses) != 7 {
		t.Errorf("got %d buses, want the seven of AllBuses", len(snap.Buses))
	}

	// The whole reason the drawer exists: commentary is in the clean feed on
	// the captured frame, by default routing. If this ever stops being true of
	// the fixture the tests around it need rereading, not deleting.
	strip, ok := snap.Strip("cam22-1")
	if !ok {
		t.Fatal("the live frame has no cam22-1 strip")
	}
	var inClean bool
	for _, b := range strip.Outputs {
		if b == mixer.BusAux1 {
			inClean = true
		}
	}
	if !inClean {
		t.Errorf("cam22-1 outputs = %v, want the captured routing that includes %s", strip.Outputs, mixer.BusAux1)
	}
}

// TestGetMixerSnapshotRefusesASubtreeDelta is the guard against internal/mixer's
// parser, which matches the advanced_audio_mixer entry by node name alone and
// ignores "path".
//
// A "/levels" delta arrives about ten times a second on a live instance.
// Read as a whole node it yields a mixer with no strips and no matrix — which
// the drawer renders as "nothing is routed to the clean feed".
func TestGetMixerSnapshotRefusesASubtreeDelta(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	w, _ := withFakeMixer(t, a)
	w.setRaw(readFixture(t, deltaFrameFixture), nil)

	snap, err := a.GetMixerSnapshot()
	if err == nil {
		t.Fatalf("GetMixerSnapshot() accepted a /levels delta and returned %d strips", len(snap.Strips))
	}
	if len(snap.Strips) != 0 {
		t.Errorf("a refused read still returned %d strips; nothing partial may reach the drawer", len(snap.Strips))
	}
}

// TestGetMixerSnapshotRefusesUnknownRouting checks that a frame whose routing
// cannot be read is refused rather than rendered.
//
// A strip present in "inputs" but absent from "matrix" parses to an EMPTY bus
// set, which is indistinguishable on screen from a strip that is genuinely
// clear of the clean feed. internal/mixer marks that critical; this host has
// nowhere to render a warning, so it refuses.
func TestGetMixerSnapshotRefusesUnknownRouting(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	w, _ := withFakeMixer(t, a)
	// cam22-1 is present in "inputs" — internal/mixer detects a strip
	// structurally, by it carrying both "muted" and "display_name" — but has no
	// entry in "matrix", so its buses are unknown rather than empty.
	w.setRaw([]byte(`{"status":[{"node":"advanced_audio_mixer","path":"/","state":{
		"inputs":{"cam22":{"channel_count":2,"cam22-1":{"muted":false,"display_name":"cam22-1"}}},
		"matrix":{},
		"outputs":{"aux1":{"muted":false,"channel_count":2}}
	}}],"timestamp":1785522083212}`), nil)

	if _, err := a.GetMixerSnapshot(); err == nil {
		t.Fatal("GetMixerSnapshot() accepted a frame in which a strip's routing is UNKNOWN, not clear")
	}

	// The operator is told, not only the caller: a mixer nobody can read is
	// worth noticing from whichever screen they are on.
	var told bool
	for _, e := range drainPump(a) {
		if e.name == EventError {
			told = true
		}
	}
	if !told {
		t.Error("no \"error\" event was raised for a mixer state that could not be trusted")
	}
}

// TestGetMixerSnapshotWithoutAControlPlane checks the first-run path: no host
// configured means no watcher, and the drawer must be told why rather than
// shown an empty mixer.
func TestGetMixerSnapshotWithoutAControlPlane(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	if _, err := a.GetMixerSnapshot(); !errors.Is(err, errMixerNoControlPlane) {
		t.Fatalf("GetMixerSnapshot() with no control plane error = %v, want %v", err, errMixerNoControlPlane)
	}
}

// ---------------------------------------------------------------------------
// The arm gate
// ---------------------------------------------------------------------------

// TestSendMixerCommandsIsRefusedBeforeArming is the headline safety property:
// a write attempted while disarmed reaches nothing.
func TestSendMixerCommandsIsRefusedBeforeArming(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	_, ctl := withFakeMixer(t, a)

	err := a.SendMixerCommands([]MixerCommand{routingCommandJSON(t, "cam22-1", "master")})
	if !errors.Is(err, mixer.ErrDisarmed) {
		t.Fatalf("SendMixerCommands() before ArmMixer error = %v, want %v", err, mixer.ErrDisarmed)
	}
	if n := len(ctl.batches()); n != 0 {
		t.Fatalf("%d batch(es) reached the controller while disarmed", n)
	}
	if ctl.closeCount() != 0 {
		t.Error("a refused write built a controller; the socket must not be opened before arming")
	}
}

// TestSendMixerCommandsIsRefusedAfterDisarming checks the same property on the
// other side of a session: the operator armed, changed their mind, and the
// write path must be shut again.
func TestSendMixerCommandsIsRefusedAfterDisarming(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	_, ctl := withFakeMixer(t, a)

	if _, err := a.ArmMixer(); err != nil {
		t.Fatalf("ArmMixer() error = %v", err)
	}
	if err := a.DisarmMixer(); err != nil {
		t.Fatalf("DisarmMixer() error = %v", err)
	}

	err := a.SendMixerCommands([]MixerCommand{routingCommandJSON(t, "cam22-1", "master")})
	if !errors.Is(err, mixer.ErrDisarmed) {
		t.Fatalf("SendMixerCommands() after DisarmMixer error = %v, want %v", err, mixer.ErrDisarmed)
	}
	if n := len(ctl.batches()); n != 0 {
		t.Fatalf("%d batch(es) reached the controller after disarming", n)
	}
	if ctl.closeCount() == 0 {
		t.Error("DisarmMixer did not release the switcher_controller socket")
	}
}

// TestSendMixerCommandsIsRefusedWhenTheWindowExpires checks that the refusal
// comes from the gate at the moment of the write and not from this host
// remembering whether it armed.
//
// The window auto-clears on a deadline; nothing calls Disarm. An operator who
// armed and was called away must come back to a shut gate.
func TestSendMixerCommandsIsRefusedWhenTheWindowExpires(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	_, ctl := withFakeMixer(t, a)

	if _, err := a.ArmMixer(); err != nil {
		t.Fatalf("ArmMixer() error = %v", err)
	}
	ctl.expire()

	err := a.SendMixerCommands([]MixerCommand{routingCommandJSON(t, "cam22-1", "master")})
	if !errors.Is(err, mixer.ErrDisarmed) {
		t.Fatalf("SendMixerCommands() after the window expired error = %v, want %v", err, mixer.ErrDisarmed)
	}
	if n := len(ctl.batches()); n != 0 {
		t.Fatalf("%d batch(es) reached the controller after the window expired", n)
	}
}

// TestArmMixerOpensTheWindowAndSendReachesTheController is the positive case:
// with the gate open, the command the drawer built arrives at the controller
// unchanged.
func TestArmMixerOpensTheWindowAndSendReachesTheController(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	_, ctl := withFakeMixer(t, a)

	state, err := a.ArmMixer()
	if err != nil {
		t.Fatalf("ArmMixer() error = %v", err)
	}
	if !state.Armed || state.ArmedUntil.IsZero() {
		t.Fatalf("ArmMixer() = %+v, want an armed state with a deadline", state)
	}
	if state.WindowSeconds != int(mixer.ArmWindow/time.Second) {
		t.Errorf("WindowSeconds = %d, want %d", state.WindowSeconds, int(mixer.ArmWindow/time.Second))
	}

	if err := a.SendMixerCommands([]MixerCommand{routingCommandJSON(t, "cam22-1", "master")}); err != nil {
		t.Fatalf("SendMixerCommands() while armed error = %v", err)
	}

	batches := ctl.batches()
	if len(batches) != 1 || len(batches[0]) != 1 {
		t.Fatalf("controller received %v, want exactly one batch of one command", batches)
	}
	got, ok := batches[0][0].(mixer.SetRouting)
	if !ok {
		t.Fatalf("controller received %T, want mixer.SetRouting", batches[0][0])
	}
	want := mixer.SetRouting{Matrix: mixer.MatrixOutput, Strip: "cam22-1", Outputs: []mixer.Bus{mixer.BusMaster}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("controller received %+v, want %+v", got, want)
	}
}

// TestArmMixerRefusesWithoutAToken checks that arming against an instance we
// are not signed in to fails now rather than at Apply. Both sockets carry the
// bearer token in their URL, so there is nothing to open without one.
func TestArmMixerRefusesWithoutAToken(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	withStubControlPlane(t, a)

	a.ctlMu.Lock()
	a.client = stubClient{token: ""}
	a.ctlMu.Unlock()
	a.mixerDial = func(string, string) (mixer.Controller, error) {
		t.Error("a controller was dialled with no bearer token")
		return nil, errors.New("unreachable")
	}

	if _, err := a.ArmMixer(); !errors.Is(err, errMixerNotSignedIn) {
		t.Fatalf("ArmMixer() with no token error = %v, want %v", err, errMixerNotSignedIn)
	}
}

// TestDisarmMixerIsIdempotent: the drawer disarms on close, on destroy and when
// the host tells it to, and every one of those arrives whether or not anything
// was ever armed.
func TestDisarmMixerIsIdempotent(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	_, ctl := withFakeMixer(t, a)

	if err := a.DisarmMixer(); err != nil {
		t.Fatalf("DisarmMixer() with nothing armed error = %v", err)
	}
	if _, err := a.ArmMixer(); err != nil {
		t.Fatalf("ArmMixer() error = %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := a.DisarmMixer(); err != nil {
			t.Fatalf("DisarmMixer() call %d error = %v", i+1, err)
		}
	}
	if ctl.closeCount() != 1 {
		t.Errorf("controller Close called %d time(s), want exactly 1", ctl.closeCount())
	}
}

// TestTeardownClosesTheMixerWritePath checks step 2 of the shutdown order: a
// window left open by an operator who closed the app mid-correction must not
// outlive the process's own teardown.
func TestTeardownClosesTheMixerWritePath(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	_, ctl := withFakeMixer(t, a)

	if _, err := a.ArmMixer(); err != nil {
		t.Fatalf("ArmMixer() error = %v", err)
	}
	a.teardown()

	if ctl.closeCount() == 0 {
		t.Fatal("teardown left the switcher_controller socket open")
	}
	if !ctl.ArmedUntil().IsZero() {
		t.Error("teardown left the write window open")
	}
}

// TestArmMixerRefusesWhileShuttingDown closes step 0 of the shutdown order for
// the mixer: a bound method already in flight must not build a socket behind a
// teardown that has walked past it.
func TestArmMixerRefusesWhileShuttingDown(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	withFakeMixer(t, a)

	a.closing.Store(true)
	if _, err := a.ArmMixer(); !errors.Is(err, errShuttingDown) {
		t.Fatalf("ArmMixer() while closing error = %v, want %v", err, errShuttingDown)
	}
}

// ---------------------------------------------------------------------------
// Command decoding
// ---------------------------------------------------------------------------

// routingCommandJSON builds the exact wire shape frontend/src/ui/mixer/model.js
// emits from routingCommand(), so the decoder is tested against the producer
// rather than against this file's idea of it.
func routingCommandJSON(t *testing.T, strip string, buses ...string) MixerCommand {
	t.Helper()
	args, err := json.Marshal(map[string]any{
		"matrix":  "output",
		"input":   strip,
		"outputs": buses,
	})
	if err != nil {
		t.Fatalf("building the test command: %v", err)
	}
	return MixerCommand{Kind: "setRouting", Args: args}
}

func TestDecodeMixerCommand(t *testing.T) {
	tests := []struct {
		name string
		cmd  MixerCommand
		want mixer.Command
		bad  bool
	}{
		{
			name: "setRouting, the only kind the drawer emits",
			cmd:  MixerCommand{Kind: "setRouting", Args: json.RawMessage(`{"matrix":"output","input":"cam22-1","outputs":["master","aux1"]}`)},
			want: mixer.SetRouting{Matrix: mixer.MatrixOutput, Strip: "cam22-1", Outputs: []mixer.Bus{mixer.BusMaster, mixer.BusAux1}},
		},
		{
			name: "setInputMuted",
			cmd:  MixerCommand{Kind: "setInputMuted", Args: json.RawMessage(`{"name":"cam22-1","muted":true}`)},
			want: mixer.SetInputMuted{Strip: "cam22-1", Muted: true},
		},
		{
			name: "setOutputMuted takes the array form",
			cmd:  MixerCommand{Kind: "setOutputMuted", Args: json.RawMessage(`{"buses":["aux1"],"muted":true}`)},
			want: mixer.SetOutputMuted{Buses: []mixer.Bus{mixer.BusAux1}, Muted: true},
		},
		{
			name: "setChFader carries the per-channel pair",
			cmd:  MixerCommand{Kind: "setChFader", Args: json.RawMessage(`{"name":"cam22-1","gain":[-1.5,-1.5]}`)},
			want: mixer.SetChFader{Strip: "cam22-1", Gain: [2]float64{-1.5, -1.5}},
		},
		{
			name: "an unknown kind is refused, not ignored",
			cmd:  MixerCommand{Kind: "setEverything", Args: json.RawMessage(`{}`)},
			bad:  true,
		},
		{
			// A misspelled argument decoding to a zero value would send a
			// set_routing with an EMPTY bus set, which un-routes the strip from
			// programme as well as from the clean feed.
			name: "a misspelled argument is refused rather than zeroed",
			cmd:  MixerCommand{Kind: "setRouting", Args: json.RawMessage(`{"matrix":"output","input":"cam22-1","output":["master"]}`)},
			bad:  true,
		},
		{
			name: "no args at all is refused",
			cmd:  MixerCommand{Kind: "setRouting"},
			bad:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeMixerCommand(tt.cmd)
			if tt.bad {
				if err == nil {
					t.Fatalf("decodeMixerCommand(%+v) = %+v, want an error", tt.cmd, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeMixerCommand(%+v) error = %v", tt.cmd, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("decodeMixerCommand() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestSendMixerCommandsDecodesTheWailsPayload pins the whole boundary in one
// place: the JSON array Wails hands to a bound method, unmarshalled into
// []MixerCommand and decoded.
//
// MixerCommand.Args is a json.RawMessage so that arguments of the wrong shape
// are refused rather than zeroed, and that only works if Wails' inbound decode
// reaches it as raw JSON. Nothing else in this suite exercises the outer
// unmarshal, and a failure there would look like a rejected promise in
// WebView2 with no clue in it.
func TestSendMixerCommandsDecodesTheWailsPayload(t *testing.T) {
	// Exactly what frontend/src/ui/mixer/model.js routingCommand() produces,
	// as one argument of a bound call.
	payload := []byte(`[
		{"kind":"setRouting","args":{"matrix":"output","input":"cam22-1","outputs":["master","aux2"]}},
		{"kind":"setRouting","args":{"matrix":"output","input":"MIC 1-1","outputs":["master","mon1"]}}
	]`)

	var cmds []MixerCommand
	if err := json.Unmarshal(payload, &cmds); err != nil {
		t.Fatalf("unmarshalling the frontend payload: %v", err)
	}

	a, _ := newTestApp(t)
	silencePump(a)
	_, ctl := withFakeMixer(t, a)
	if _, err := a.ArmMixer(); err != nil {
		t.Fatalf("ArmMixer() error = %v", err)
	}
	if err := a.SendMixerCommands(cmds); err != nil {
		t.Fatalf("SendMixerCommands() error = %v", err)
	}

	batches := ctl.batches()
	if len(batches) != 1 || len(batches[0]) != 2 {
		t.Fatalf("controller received %v, want one batch of two commands", batches)
	}
	want := []mixer.Command{
		mixer.SetRouting{Matrix: mixer.MatrixOutput, Strip: "cam22-1", Outputs: []mixer.Bus{mixer.BusMaster, mixer.BusAux2}},
		// Strip names contain spaces — "MIC 1-1" is a real strip in the
		// captured live frame — so the decode must not tokenise them.
		mixer.SetRouting{Matrix: mixer.MatrixOutput, Strip: "MIC 1-1", Outputs: []mixer.Bus{mixer.BusMaster, mixer.BusMon1}},
	}
	if !reflect.DeepEqual(batches[0], want) {
		t.Errorf("controller received %+v, want %+v", batches[0], want)
	}
}

// TestDecodeMixerCommandsRejectsTheWholeBatch: a bad command at the end must
// not let the good ones ahead of it be written first.
func TestDecodeMixerCommandsRejectsTheWholeBatch(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	_, ctl := withFakeMixer(t, a)
	if _, err := a.ArmMixer(); err != nil {
		t.Fatalf("ArmMixer() error = %v", err)
	}

	err := a.SendMixerCommands([]MixerCommand{
		routingCommandJSON(t, "cam22-1", "master"),
		{Kind: "setNonsense", Args: json.RawMessage(`{}`)},
	})
	if err == nil {
		t.Fatal("SendMixerCommands() accepted a batch containing an unknown command")
	}
	if n := len(ctl.batches()); n != 0 {
		t.Fatalf("%d batch(es) were written despite the batch being invalid", n)
	}
}

// TestSendMixerCommandsRefusesAnEmptyBatch. mixer.Send permits zero commands on
// purpose, but nothing reaches this binding except a deliberate write from the
// drawer's Apply path, so an empty one is a caller bug and must say so rather
// than resolve as though it had written.
func TestSendMixerCommandsRefusesAnEmptyBatch(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	withFakeMixer(t, a)
	if _, err := a.ArmMixer(); err != nil {
		t.Fatalf("ArmMixer() error = %v", err)
	}
	if err := a.SendMixerCommands(nil); err == nil {
		t.Fatal("SendMixerCommands(nil) returned nil, which a caller reads as \"sent\"")
	}
}

// ---------------------------------------------------------------------------
// The golden snapshot
// ---------------------------------------------------------------------------

// TestGoldenRoundTrip checks that a saved baseline comes back as it went in,
// and that "never saved" is nil rather than an empty snapshot.
//
// An empty golden compares clean against everything, so the drift panel would
// read "no differences" for ever — the failure golden.go warns about.
func TestGoldenRoundTrip(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	got, err := a.GetMixerGolden()
	if err != nil {
		t.Fatalf("GetMixerGolden() with none saved error = %v", err)
	}
	if got != nil {
		t.Fatalf("GetMixerGolden() with none saved = %+v, want nil so the drawer says \"no golden saved\"", got)
	}

	snap, _, err := mixer.ParseSnapshotWithWarnings(readFixture(t, liveFrameFixture))
	if err != nil {
		t.Fatalf("parsing the live fixture: %v", err)
	}
	if err := a.SetMixerGolden(&snap); err != nil {
		t.Fatalf("SetMixerGolden() error = %v", err)
	}

	back, err := a.GetMixerGolden()
	if err != nil {
		t.Fatalf("GetMixerGolden() after saving error = %v", err)
	}
	if back == nil {
		t.Fatal("GetMixerGolden() returned nil after a successful save")
	}
	if len(back.Strips) != len(snap.Strips) || len(back.Buses) != len(snap.Buses) {
		t.Fatalf("golden round-trip lost state: %d/%d strips, %d/%d buses",
			len(back.Strips), len(snap.Strips), len(back.Buses), len(snap.Buses))
	}
	if !back.TakenAt.Equal(snap.TakenAt) {
		t.Errorf("golden TakenAt = %v, want %v", back.TakenAt, snap.TakenAt)
	}

	// The golden is diffable against the state it came from and reports
	// nothing, which is what makes a later difference meaningful.
	if diffs := mixer.Compare(*back, snap); len(diffs) != 0 {
		t.Errorf("a golden compared against its own source reported %d difference(s): %+v", len(diffs), diffs)
	}
}

// TestSetMixerGoldenRefusesAnEmptySnapshot: saving one would silently turn the
// drift panel off for the rest of the event.
func TestSetMixerGoldenRefusesAnEmptySnapshot(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	if err := a.SetMixerGolden(&mixer.Snapshot{}); err == nil {
		t.Fatal("SetMixerGolden() accepted a snapshot with no strips")
	}
	if err := a.SetMixerGolden(nil); err == nil {
		t.Fatal("SetMixerGolden(nil) was accepted")
	}
	got, err := a.GetMixerGolden()
	if err != nil {
		t.Fatalf("GetMixerGolden() error = %v", err)
	}
	if got != nil {
		t.Error("a refused save still wrote a baseline")
	}
}

// TestGetMixerGoldenReportsACorruptFile rather than reporting "none saved".
// "There is no baseline" and "the baseline is unreadable" look identical on a
// drift panel and are not the same thing.
func TestGetMixerGoldenReportsACorruptFile(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	path, err := mixerGoldenPath()
	if err != nil {
		t.Fatalf("mixerGoldenPath() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating the config directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing a corrupt golden file: %v", err)
	}

	got, err := a.GetMixerGolden()
	if err == nil {
		t.Fatalf("GetMixerGolden() on a corrupt file = %+v, want an error", got)
	}
}

// ---------------------------------------------------------------------------
// Host handling
// ---------------------------------------------------------------------------

func TestMixerHost(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		bad  bool
	}{
		{"a bare host is used as it is", "m2lx.example.com", "m2lx.example.com", false},
		{"a port is kept", "m2lx.example.com:8443", "m2lx.example.com:8443", false},
		{"an https scheme is stripped", "https://m2lx.example.com", "m2lx.example.com", false},
		{"a path is dropped", "https://m2lx.example.com/live-operation/x", "m2lx.example.com", false},
		{"surrounding space is trimmed", "  m2lx.example.com  ", "m2lx.example.com", false},
		{"an http host is refused, not upgraded", "http://127.0.0.1:8080", "", true},
		{"an empty host is refused", "   ", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mixerHost(tt.in)
			if tt.bad {
				if err == nil {
					t.Fatalf("mixerHost(%q) = %q, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("mixerHost(%q) error = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("mixerHost(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestCheckMixerNodeIsWhole pins the path guard on its own, because it is the
// only thing standing between an advanced_audio_mixer subtree delta and a
// drawer that says nothing is in the clean feed.
func TestCheckMixerNodeIsWhole(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		bad  bool
	}{
		{"the mixer node at /", `{"status":[{"node":"advanced_audio_mixer","path":"/","state":{}}]}`, false},
		{"a /levels delta", `{"status":[{"node":"advanced_audio_mixer","path":"/levels","state":{}}]}`, true},
		{"a /peak_levels delta", `{"status":[{"node":"advanced_audio_mixer","path":"/peak_levels","state":{}}]}`, true},
		{"no mixer node at all", `{"status":[{"node":"cam1","path":"/","state":{}}]}`, true},
		{"not a frame", `[]`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkMixerNodeIsWhole([]byte(tt.raw))
			if tt.bad && err == nil {
				t.Fatalf("checkMixerNodeIsWhole(%s) = nil, want an error", tt.raw)
			}
			if !tt.bad && err != nil {
				t.Fatalf("checkMixerNodeIsWhole(%s) error = %v", tt.raw, err)
			}
		})
	}
}

// TestRawSnapshotIsOnTheWatcherInterface documents why this host can honour the
// fresh-snapshot guarantee at all: the whole document exists once per
// connection, so the read has to be able to open one.
func TestRawSnapshotIsOnTheWatcherInterface(t *testing.T) {
	typ := reflect.TypeOf((*m2lx.Watcher)(nil)).Elem()
	if _, ok := typ.MethodByName("RawSnapshot"); !ok {
		t.Fatal("m2lx.Watcher has no RawSnapshot; GetMixerSnapshot would have to cache, which the drawer forbids")
	}
}
