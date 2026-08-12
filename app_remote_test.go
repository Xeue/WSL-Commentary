//go:build dev || production || bindings

// Tests for the App side of the LAN control bridge: the allowlist policy in
// app_remote.go, mixer arm-ownership in app_mixer.go, the EventConfig emission
// in app.go, and the listener's place in the ordered teardown.
//
// Owner: WP-8. These run at Gate A (CGO_ENABLED=0, -tags dev) like the rest of
// the root suite — internal/remote is pure Go, so the whole bridge is
// exercisable with no MinGW, no GStreamer and no audio hardware.
//
// THE HEADLINE TEST IS THE DRIFT GUARD. main.go binds every exported method of
// *App, so "is this method safe to expose over a network" is a question that has
// to be asked by a failing test rather than by whoever reviews the PR that adds
// the next binding. TestRemoteAllowlistCoversEveryBoundMethod is that test: a new
// exported method that is not classified in remoteAllowlist fails the build,
// by name.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"wslcomms/internal/mixer"
	"wslcomms/internal/remote"
)

// ---------------------------------------------------------------------------
// The drift guard
// ---------------------------------------------------------------------------

// TestRemoteAllowlistCoversEveryBoundMethod is THE guard. Every exported method
// of *App — everything Wails binds and therefore everything a crafted client
// could name on the wire — must appear in remoteAllowlist with an explicit,
// known capability. A method absent from the table would be refused as unknown
// at runtime (fail-closed, which is safe), but the point of failing HERE is that
// the classification is a decision, made once, visible in review — not a default
// that a reader has to reverse-engineer from the dispatch switch.
func TestRemoteAllowlistCoversEveryBoundMethod(t *testing.T) {
	knownCap := map[remote.Capability]bool{
		remote.CapView:    true,
		remote.CapOperate: true,
		remote.CapMixer:   true,
	}

	// Forward: every exported method is classified.
	for name := range exportedMethodsOfApp() {
		pol, ok := remoteAllowlist[name]
		if !ok {
			t.Errorf("*App exports %q but it is NOT in remoteAllowlist. Classify it: give it a "+
				"capability (view/operate/mixer) and decide whether it is host-only, so a new binding "+
				"cannot default into being remotely callable.", name)
			continue
		}
		if !knownCap[pol.cap] {
			t.Errorf("method %q has capability %q, which is not one of view/operate/mixer", name, pol.cap)
		}
	}

	// Reverse: the table names no phantom method. A row for a method that no
	// longer exists is dead policy that reads as coverage.
	exported := exportedMethodsOfApp()
	for name := range remoteAllowlist {
		if !exported[name] {
			t.Errorf("remoteAllowlist classifies %q, which *App no longer exports; remove the dead row", name)
		}
	}
}

// TestRemoteHostOnlySet pins exactly which methods are host-only, so neither the
// native-surface six nor the admin five can silently gain OR lose that status.
func TestRemoteHostOnlySet(t *testing.T) {
	want := map[string]bool{
		// the native picture / SRT-return surface
		"SetPictureRect": true, "SetPictureVisible": true,
		"StartPicture": true, "StopPicture": true,
		"StartReturn": true, "StopReturn": true,
		// remote administration (local Settings screen only)
		"GetRemoteState": true, "SetRemoteListener": true,
		"AddRemoteClient": true, "SetRemoteClientPassword": true,
		"DeleteRemoteClient": true,
	}
	got := map[string]bool{}
	for name, pol := range remoteAllowlist {
		if pol.hostOnly {
			got[name] = true
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("host-only set = %v, want %v", sortedKeys(got), sortedKeys(want))
	}
}

// ---------------------------------------------------------------------------
// The dispatch gates
// ---------------------------------------------------------------------------

// TestRemoteCallRefusesUnknownMethod proves the allowlist is an allowlist: a
// name not in the table is refused as unknown regardless of anything else.
func TestRemoteCallRefusesUnknownMethod(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	client := remote.ClientInfo{ID: "c1", Name: "op", Caps: []string{string(remote.CapMixer)}}

	_, err := a.remoteCall(context.Background(), client, "NoSuchMethod", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("remoteCall(NoSuchMethod) error = %v, want an 'unknown method' refusal", err)
	}
}

// TestRemoteHostOnlyRefusedAtEveryCapability is the design's test 5 at the App
// layer: a host-only method is refused for every capability set INCLUDING the
// highest, and is absent from the hello methods list a client at that capability
// would receive.
func TestRemoteHostOnlyRefusedAtEveryCapability(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	capSets := [][]string{
		nil,
		{string(remote.CapView)},
		{string(remote.CapOperate)},
		{string(remote.CapMixer)},
	}

	for name, pol := range remoteAllowlist {
		if !pol.hostOnly {
			continue
		}
		for _, caps := range capSets {
			client := remote.ClientInfo{ID: "c1", Name: "op", Caps: caps}

			// Absent from the methods list.
			for _, m := range a.remoteMethods(client) {
				if m == name {
					t.Errorf("host-only %q appeared in remoteMethods for caps %v", name, caps)
				}
			}
			// Refused by Call.
			_, err := a.remoteCall(context.Background(), client, name, nil)
			if err == nil || !strings.Contains(err.Error(), "host-only") {
				t.Errorf("remoteCall(%q) at caps %v error = %v, want a host-only refusal", name, caps, err)
			}
		}
	}
}

// TestRemoteMethodsFollowTheCapabilityTiers checks the authoritative hello list:
// a view client sees the reads but not the writes, operate sees the operate
// writes but not the mixer write, mixer sees them all. DisarmMixer is view, so
// every authenticated client sees it — shutting a gate is always safe.
func TestRemoteMethodsFollowTheCapabilityTiers(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	viewer := set(a.remoteMethods(remote.ClientInfo{Caps: []string{string(remote.CapView)}}))
	operator := set(a.remoteMethods(remote.ClientInfo{Caps: []string{string(remote.CapOperate)}}))
	mixerC := set(a.remoteMethods(remote.ClientInfo{Caps: []string{string(remote.CapMixer)}}))

	// view: reads yes, writes no.
	mustHave(t, "view", viewer, "GetConfig", "ListPresets", "GetActivePreset", "DisarmMixer")
	mustLack(t, "view", viewer, "Start", "SaveConfig", "SendMixerCommands", "SetMixerGolden")

	// operate: operate writes yes, mixer write no.
	mustHave(t, "operate", operator, "Start", "Stop", "SaveConfig", "SetSecret", "ArmMixer", "ApplyPreset")
	mustLack(t, "operate", operator, "SendMixerCommands", "SetMixerGolden")

	// mixer: the write path too.
	mustHave(t, "mixer", mixerC, "SendMixerCommands", "SetMixerGolden", "Start", "GetConfig")

	// Host-only never appears at any tier.
	mustLack(t, "mixer", mixerC, "StartPicture", "StopReturn", "SetRemoteListener")
}

// TestRemoteCapabilityGateRefusesUnderTier proves the gate is enforced by Call,
// not merely reflected in the methods list: a view client naming an operate
// method on the wire is refused with a capability error, before the method runs.
func TestRemoteCapabilityGateRefusesUnderTier(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	view := remote.ClientInfo{ID: "c1", Name: "v", Caps: []string{string(remote.CapView)}}

	for _, m := range []string{"Start", "SaveConfig", "SendMixerCommands"} {
		_, err := a.remoteCall(context.Background(), view, m, nil)
		if err == nil || !strings.Contains(err.Error(), "requires capability") {
			t.Errorf("remoteCall(%q) as view error = %v, want a capability refusal", m, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Mixer arm-ownership
// ---------------------------------------------------------------------------

// TestArmOwnershipRefusesAForeignSeat is the arm-ownership property: the seat
// that armed is the ONLY one whose write is accepted, so with two controllers one
// operator's arm cannot authorise the other's write to the live clean feed. The
// refusal carries mixer.ErrDisarmed like every other write refusal, and — the
// point of the whole feature — nothing reaches the controller.
func TestArmOwnershipRefusesAForeignSeat(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	_, ctl := withFakeMixer(t, a)

	if _, err := a.armMixerFrom("seat-A"); err != nil {
		t.Fatalf("armMixerFrom(seat-A) error = %v", err)
	}

	// Seat B, holding the same open window, is refused.
	err := a.sendMixerCommandsFrom("seat-B", []MixerCommand{routingCommandJSON(t, "cam22-1", "master")})
	if !errors.Is(err, mixer.ErrDisarmed) {
		t.Fatalf("sendMixerCommandsFrom(seat-B) error = %v, want %v", err, mixer.ErrDisarmed)
	}
	if n := len(ctl.batches()); n != 0 {
		t.Fatalf("%d batch(es) reached the controller from a seat that did not arm", n)
	}

	// Seat A, the arming seat, is accepted.
	if err := a.sendMixerCommandsFrom("seat-A", []MixerCommand{routingCommandJSON(t, "cam22-1", "master")}); err != nil {
		t.Fatalf("sendMixerCommandsFrom(seat-A) error = %v, want acceptance", err)
	}
	if n := len(ctl.batches()); n != 1 {
		t.Fatalf("the arming seat's write produced %d batch(es), want 1", n)
	}
}

// TestArmOwnershipLocalArmRefusesRemoteWrite closes the exact two-controller case
// the feature exists for: the LOCAL operator arms (through the bound ArmMixer),
// and a REMOTE seat's SendMixerCommands — arriving through the dispatcher with
// its own connection id — is refused with mixer.ErrDisarmed even though it holds
// the mixer capability.
func TestArmOwnershipLocalArmRefusesRemoteWrite(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	_, ctl := withFakeMixer(t, a)

	// Local seat arms.
	if _, err := a.ArmMixer(); err != nil {
		t.Fatalf("ArmMixer() error = %v", err)
	}

	// Remote mixer-capable seat writes through the dispatcher.
	remoteClient := remote.ClientInfo{ID: "remote-1", Name: "producer", Caps: []string{string(remote.CapMixer)}}
	args := mustArgs(t, []MixerCommand{routingCommandJSON(t, "cam22-1", "master")})
	_, err := a.remoteCall(context.Background(), remoteClient, "SendMixerCommands", args)
	if !errors.Is(err, mixer.ErrDisarmed) {
		t.Fatalf("remote SendMixerCommands under a local arm error = %v, want %v", err, mixer.ErrDisarmed)
	}
	if n := len(ctl.batches()); n != 0 {
		t.Fatalf("%d batch(es) reached the controller from a remote seat under a local arm", n)
	}
}

// ---------------------------------------------------------------------------
// EventConfig
// ---------------------------------------------------------------------------

// TestSaveConfigEmitsConfigEventWithOrigin checks that SaveConfig publishes the
// saved configuration and the id of the seat that saved it, so another seat can
// refresh and the saving seat can ignore its own echo. The bound SaveConfig
// stamps the local seat; the remote dispatcher stamps the connecting one.
func TestSaveConfigEmitsConfigEventWithOrigin(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	// Remote path: a named seat's id is carried through.
	if err := a.saveConfigFrom("seat-remote", validConfig()); err != nil {
		t.Fatalf("saveConfigFrom(seat-remote) error = %v", err)
	}
	ev := lastConfigEvent(t, drainPump(a))
	if ev.Origin != "seat-remote" {
		t.Fatalf("EventConfig origin = %q, want %q", ev.Origin, "seat-remote")
	}
	if ev.Config == nil {
		t.Fatal("EventConfig carried no config; a subscriber has nothing to apply")
	}

	// Local path: the bound method stamps the local seat.
	if err := a.SaveConfig(validConfig()); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	ev = lastConfigEvent(t, drainPump(a))
	if ev.Origin != localClientID {
		t.Fatalf("bound SaveConfig origin = %q, want %q", ev.Origin, localClientID)
	}
}

// ---------------------------------------------------------------------------
// Lifecycle: the listener is stopped by teardown within budget
// ---------------------------------------------------------------------------

// TestTeardownStopsTheRemoteListenerWithinBudget stands a real listener up on an
// ephemeral loopback port, confirms it bound, then tears the app down and asserts
// the listener is gone and the whole teardown finished well inside its budget.
// The listener must never be the process that will not exit.
func TestTeardownStopsTheRemoteListenerWithinBudget(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	// A listener enabled on loopback, ephemeral port (0 — accepted by Start; the
	// operator-facing >=1 floor lives in Settings.Validate, which this bypasses on
	// purpose so the test does not fight for a fixed port). One client so the
	// store is non-empty and realistic.
	s := remote.DefaultSettings()
	s.Enabled = true
	s.Bind = "127.0.0.1"
	s.Port = 0
	if err := s.AddClient("op", []string{string(remote.CapView)}); err != nil {
		t.Fatalf("AddClient: %v", err)
	}
	if err := s.SetClientPassword("op", "pw"); err != nil {
		t.Fatalf("SetClientPassword: %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save remote.json: %v", err)
	}

	a.startRemote()

	// Wait for the bind to complete (Fingerprint is set inside Start).
	waitFor(t, 2*time.Second, "the remote listener to bind", func() bool {
		srv := a.remote.Load()
		return srv != nil && srv.Fingerprint() != ""
	})

	start := time.Now()
	a.teardown()
	elapsed := time.Since(start)

	if elapsed > shutdownTimeout {
		t.Fatalf("teardown took %s, past its %s budget", elapsed, shutdownTimeout)
	}
	if srv := a.remote.Load(); srv != nil {
		t.Fatal("the remote listener is still present after teardown; it was not stopped")
	}
}

// TestStartRemoteDisabledBindsNothing is the safe-posture check at the App layer:
// with the default (missing remote.json → disabled), startRemote builds a server
// but Start binds nothing, so there is no listener and no goroutine to reap.
func TestStartRemoteDisabledBindsNothing(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	a.startRemote()

	// Give the start goroutine a moment; a disabled server returns addr "" and
	// never sets a fingerprint.
	if pollUntil(300*time.Millisecond, func() bool {
		srv := a.remote.Load()
		return srv != nil && srv.Fingerprint() != ""
	}) {
		t.Fatal("a listener bound with remote access disabled; the default posture must bind nothing")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func set(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mustHave(t *testing.T, tier string, have map[string]bool, names ...string) {
	t.Helper()
	for _, n := range names {
		if !have[n] {
			t.Errorf("%s methods lack %q, which a %s client should see", tier, n, tier)
		}
	}
}

func mustLack(t *testing.T, tier string, have map[string]bool, names ...string) {
	t.Helper()
	for _, n := range names {
		if have[n] {
			t.Errorf("%s methods include %q, which a %s client must NOT see", tier, n, tier)
		}
	}
}

// mustArgs marshals a call's Go arguments into the positional raw-JSON form the
// dispatcher decodes, so a test can drive remoteCall exactly as the transport
// would.
func mustArgs(t *testing.T, vals ...any) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, 0, len(vals))
	for _, v := range vals {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshalling a call argument: %v", err)
		}
		out = append(out, b)
	}
	return out
}

// lastConfigEvent returns the configEvent payload of the last EventConfig in the
// queue, failing if there is none.
func lastConfigEvent(t *testing.T, queued []pumpEvent) configEvent {
	t.Helper()
	for i := len(queued) - 1; i >= 0; i-- {
		if queued[i].name != EventConfig {
			continue
		}
		ev, ok := queued[i].data.(configEvent)
		if !ok {
			t.Fatalf("EventConfig carried %T, want configEvent", queued[i].data)
		}
		return ev
	}
	t.Fatalf("no EventConfig was queued; SaveConfig must emit one so a second controller can refresh")
	return configEvent{}
}

// pollUntil polls cond until it is true or the deadline passes, RETURNING
// whether it became true rather than failing — unlike app_test.go's waitFor,
// which fatals on timeout. That distinction matters here: the disabled-listener
// test expects the condition to STAY false, so a timeout is the pass, not the
// failure.
func pollUntil(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
