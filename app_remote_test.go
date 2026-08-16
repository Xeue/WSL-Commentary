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
	"os"
	"reflect"
	"sort"
	"strconv"
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
	// Forward: every exported method is classified. With no capability tiers the
	// classification is binary — a method is host-only or it is not — so being
	// present in the table is the whole requirement.
	for name := range exportedMethodsOfApp() {
		if _, ok := remoteAllowlist[name]; !ok {
			t.Errorf("*App exports %q but it is NOT in remoteAllowlist. Classify it: decide whether it "+
				"is host-only, so a new binding cannot default into being remotely callable.", name)
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
// native-surface six nor the two remote-admin methods can silently gain OR lose
// that status. The per-client admin methods are gone — there are no clients.
func TestRemoteHostOnlySet(t *testing.T) {
	want := map[string]bool{
		// the native picture / SRT-return surface
		"SetPictureRect": true, "SetPictureVisible": true,
		"StartPicture": true, "StopPicture": true,
		"StartReturn": true, "StopReturn": true,
		// the DeckLink preview's surface: the same argument as the picture's two
		"SetPreviewRect": true, "SetPreviewVisible": true,
		// what this position puts ON AIR, and the window on the operator's screen
		"SetVideoSource": true, "SetDeckLinkPreviewEnabled": true,
		// remote administration (local Settings screen only)
		"GetRemoteState": true, "SetRemoteListener": true,
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

// TestRemoteDispatchCoversEveryReachableMethod closes the gap between the two
// halves of the remote surface: remoteAllowlist says a method is REACHABLE, and
// remoteInvoke's switch is what actually reaches it. Nothing tied them together,
// and they had come apart.
//
// MEASURED, and this is the whole reason the test exists: GetConformTarget was
// in the allowlist with a paragraph of reasoning about why a remote seat's VIDEO
// lamp needs it, and had no case in the switch. Every remote seat that asked for
// it got
//
//	remote: method "GetConformTarget" has no dispatch case
//
// — from the switch's own default, whose comment reads "Unreachable: ... every
// non-host-only allowlisted method has a case above". The assertion was true
// when it was written and had quietly stopped being true, which is precisely the
// class of drift TestRemoteAllowlistCoversEveryBoundMethod exists to prevent one
// step earlier.
//
// # Why it reads the source text rather than calling the methods
//
// Because calling them is not an option. The reachable set includes Start, Stop,
// SaveConfig and ApplyPreset; a test that invoked every one of them to see
// whether it was routed would open capture devices, dial a switcher and rewrite
// the operator's configuration. Reading which case labels the switch contains is
// the only way to ask "is it routed" without doing the thing.
//
// Reading a Go file's source in a test is an established idiom here rather than
// a novelty — frontend/src/ui pins internal/gst's two DeviceKind spellings by
// reading gst.go's source for the same reason: the fact being asserted is the
// TEXT, and a parse of the text is the only honest way to assert it.
//
// It is deliberately crude — a scan for `case "Name":` between the function's
// opening line and its default branch — because a crude scan that over-reports
// coverage would have to be defeated by somebody writing a case label for a
// method they did not route, and a crude scan that under-reports is a failing
// test somebody reads. Neither failure is silent.
func TestRemoteDispatchCoversEveryReachableMethod(t *testing.T) {
	const file = "app_remote.go"
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	text := string(src)

	// Bound the scan to remoteInvoke's body, so that a case label belonging to
	// some other switch in this file can never be read as dispatch coverage.
	start := strings.Index(text, "func (a *App) remoteInvoke(")
	if start < 0 {
		t.Fatalf("%s no longer declares remoteInvoke; this test's anchor is stale", file)
	}
	end := strings.Index(text[start:], "\n\tdefault:")
	if end < 0 {
		t.Fatalf("remoteInvoke in %s has no default branch; this test's anchor is stale", file)
	}
	body := text[start : start+end]

	routed := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `case "`) {
			continue
		}
		// Every label on a `case "A", "B":` line, so a future grouped case is
		// read correctly rather than half-read.
		for _, part := range strings.Split(strings.TrimSuffix(line[len("case "):], ":"), ",") {
			if name, err := strconv.Unquote(strings.TrimSpace(part)); err == nil {
				routed[name] = true
			}
		}
	}

	// A SOURCE SCAN THAT PARSES NOTHING PASSES EVERYTHING, which would make this
	// test worse than no test: it would report coverage it never looked for. The
	// floor is deliberately loose — it is a smoke check on the parse, not a
	// second frozen list to maintain — and any reformatting that drops the count
	// below it is exactly the change that should stop and be looked at.
	if len(routed) < 20 {
		t.Fatalf("only %d case labels were parsed out of remoteInvoke; the scan has stopped "+
			"working and would now pass regardless of coverage", len(routed))
	}

	for name, pol := range remoteAllowlist {
		if pol.hostOnly {
			// Refused before dispatch and omitted from Methods(), so a case
			// would be dead code rather than coverage.
			continue
		}
		if !routed[name] {
			t.Errorf("remoteAllowlist makes %q remotely reachable but remoteInvoke has no "+
				"case for it, so every remote call of it fails with \"no dispatch case\". "+
				"Add the case, or classify the method host-only.", name)
		}
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
	client := remote.ClientInfo{ID: "c1", RemoteAddr: "192.0.2.9:5000"}

	_, err := a.remoteCall(context.Background(), client, "NoSuchMethod", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("remoteCall(NoSuchMethod) error = %v, want an 'unknown method' refusal", err)
	}
}

// TestRemoteHostOnlyRefused is the design's host-only test at the App layer: a
// host-only method is refused for every connection and is absent from the hello
// methods list. With no capabilities, "every connection" is a single case.
func TestRemoteHostOnlyRefused(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	client := remote.ClientInfo{ID: "c1", RemoteAddr: "192.0.2.9:5000"}
	installed := set(a.remoteMethods(client))

	for name, pol := range remoteAllowlist {
		if !pol.hostOnly {
			continue
		}
		// Absent from the methods list.
		if installed[name] {
			t.Errorf("host-only %q appeared in remoteMethods", name)
		}
		// Refused by Call.
		_, err := a.remoteCall(context.Background(), client, name, nil)
		if err == nil || !strings.Contains(err.Error(), "host-only") {
			t.Errorf("remoteCall(%q) error = %v, want a host-only refusal", name, err)
		}
	}
}

// TestRemoteMethodsAreEveryNonHostOnlyMethod checks the authoritative hello list:
// with no capability tiers, every connection sees EVERY allowlisted method that
// is not host-only, and no host-only method ever appears.
func TestRemoteMethodsAreEveryNonHostOnlyMethod(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	got := set(a.remoteMethods(remote.ClientInfo{ID: "c1", RemoteAddr: "192.0.2.9:5000"}))

	// Every non-host-only allowlisted method is present; every host-only one is
	// absent. This is derived from the table itself so it cannot drift from it.
	for name, pol := range remoteAllowlist {
		if pol.hostOnly {
			if got[name] {
				t.Errorf("host-only %q must not appear in remoteMethods", name)
			}
			continue
		}
		if !got[name] {
			t.Errorf("non-host-only %q is missing from remoteMethods", name)
		}
	}

	// Spot-check the reads, the writes and the arm-gated write are all there —
	// every connection can reach them now.
	mustHave(t, "open", got, "GetConfig", "ListPresets", "DisarmMixer",
		"Start", "SaveConfig", "SetSecret", "ArmMixer", "SendMixerCommands", "SetMixerGolden")
	// Host-only never appears.
	mustLack(t, "open", got, "StartPicture", "StopReturn", "GetRemoteState", "SetRemoteListener")
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
// its own connection id — is refused with mixer.ErrDisarmed. Arm-ownership is
// about WHICH seat holds the open window, not authentication, so it survives the
// move to the unauthenticated listener unchanged.
func TestArmOwnershipLocalArmRefusesRemoteWrite(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	_, ctl := withFakeMixer(t, a)

	// Local seat arms.
	if _, err := a.ArmMixer(); err != nil {
		t.Fatalf("ArmMixer() error = %v", err)
	}

	// A remote seat writes through the dispatcher with its own connection id.
	remoteClient := remote.ClientInfo{ID: "remote-1", RemoteAddr: "192.0.2.9:5000"}
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

	// A listener enabled on loopback, ephemeral ports (0 — accepted by Start and
	// used here so the test does not fight for the fixed 80/443 or their
	// fallbacks). There are no clients to configure; the listener is
	// unauthenticated.
	s := remote.DefaultSettings()
	s.Enabled = true
	s.Bind = "127.0.0.1"
	s.HTTPPort = 0
	s.HTTPSPort = 0
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

// TestStartRemoteDisabledBindsNothing is the off-switch check at the App layer:
// an operator who has turned remote access OFF (enabled=false in remote.json)
// gets no listener. The listener is ON by default now, so this test must write
// the disabled state explicitly rather than rely on a missing file.
func TestStartRemoteDisabledBindsNothing(t *testing.T) {
	a, _ := newTestApp(t) // newTestApp already writes a disabled remote.json

	silencePump(a)

	a.startRemote()

	// Give the start goroutine a moment; a disabled server binds nothing and
	// never sets a fingerprint.
	if pollUntil(300*time.Millisecond, func() bool {
		srv := a.remote.Load()
		return srv != nil && srv.Fingerprint() != ""
	}) {
		t.Fatal("a listener bound with remote access disabled; OFF must bind nothing")
	}
}

// disableRemoteListenerForTest writes a remote.json that turns the LAN listener
// OFF, under the APPDATA the caller has already redirected. The listener is now
// ON by default and binds 0.0.0.0:80/443 (falling back to 8080/8443), so a test
// that calls startup() or startRemote() without this would try to open a REAL
// LAN listener — a firewall prompt and a resource leak. Tests that WANT a
// listener save their own enabled settings after newTestApp has run.
func disableRemoteListenerForTest(t *testing.T) {
	t.Helper()
	s := remote.DefaultSettings()
	s.Enabled = false
	if err := s.Save(); err != nil {
		t.Fatalf("disabling the remote listener for the test: %v", err)
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
