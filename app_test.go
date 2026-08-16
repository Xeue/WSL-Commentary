//go:build dev || production || bindings

// Tests for the WP-8 wire-up: main.go and app.go.
//
// Owner: WP-8.
//
// # Why this file carries the same build tag as app.go
//
// app.go and main.go are selected by dev || production || bindings, so a test
// file without a matching tag would not compile — the symbols it tests would not
// exist. With the tag it compiles exactly when they do, and:
//
//	CGO_ENABLED=0 go test -tags dev . -count=1
//
// runs the whole suite at Gate A, with no MinGW, no GStreamer and no audio
// hardware. That invocation is part of Gate A; see RUNNING.md section 1. The
// plain `go test ./...` still reports "? wslcomms [no test files]" and runs none
// of this, which is why the tagged command is listed separately there.
//
// Nothing here calls wails.Run, and nothing here calls any wailsruntime function
// that needs a live runtime context: eventPump.start is the only such caller and
// every test that would reach it neutralises its sync.Once first. So these tests
// cannot pop the modal Wails build-tag dialog and cannot open a window.
//
// The pipeline under test is internal/gst's pure-Go stub twin, which is what
// makes the whole session lifecycle — Start, reconnect, Stop, teardown —
// exercisable with no MinGW, no GStreamer and no audio hardware.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wailsapp/wails/v2/pkg/options"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
	"wslcomms/internal/m2lx"
	"wslcomms/internal/secrets"
	"wslcomms/internal/sender"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeStore is an in-memory secrets.Store. The real one is Windows Credential
// Manager, which a unit test must not write to.
type fakeStore struct {
	mu     sync.Mutex
	values map[string]string
	getErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{values: map[string]string{}}
}

func (s *fakeStore) Get(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return "", s.getErr
	}
	v, ok := s.values[key]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return v, nil
}

func (s *fakeStore) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
	return nil
}

var _ secrets.Store = (*fakeStore)(nil)

// validConfig returns a configuration that passes config.Validate, so that a
// test which is not about validation does not have to restate ten fields.
func validConfig() *config.Config {
	c := config.Defaults()
	c.M2LXHost = "127.0.0.1:8080"
	c.Alias = "wsl-comms-ro"
	c.EventID = "matcht"
	c.SRTPort = 4001
	c.StatusKey = "cam7"
	c.AudioDeviceID = "{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}"
	return c
}

// redirectAppDataForTest points the per-user application data directory at a
// fresh temp directory, so that nothing in this package can read or write the
// developer's real config.json, presets, mixer golden or remote settings.
//
// WHICH environment variable does that is per-GOOS, and getting it wrong is not
// a no-op — it is a test that silently runs against the machine's own state.
// os.UserConfigDir reads %APPDATA% on Windows and $HOME/Library/Application
// Support on darwin, so t.Setenv("APPDATA", ...) alone — which is what every
// caller here used to do — redirected nothing at all on a Mac. Nine tests in
// this package failed because of it, and every one of them failed for a
// DIFFERENT and entirely misleading reason: seven built-in presets became ten
// because the developer had three of their own, GetMixerGolden reported the real
// mixer-golden.json as corrupt, the first preset adopted a scope of "wembley"
// from a real active.json, and TestStartupWithNoM2LXHostBuildsNoControlPlane
// built a control plane because the real config.json has an M2L-X host in it.
// None of those points at the cause, and on a machine with an empty profile they
// would all have passed.
//
// internal/config's withAppData already had this fix, for exactly the same
// reason; this is the same helper in the package that also needed it. Setting
// HOME on darwin additionally redirects applog.DefaultDir, which is wanted:
// nothing under test should be writing into ~/Library/Logs either.
func redirectAppDataForTest(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("APPDATA", home)
	case "darwin":
		t.Setenv("HOME", home)
	default:
		t.Setenv("XDG_CONFIG_HOME", home)
	}
}

// newTestApp builds an App wired to fakes, with the per-user application data
// directory redirected at a temp directory so config.Save cannot touch the
// developer's real config.json.
//
// It deliberately does not call startup: the tests that want the startup path
// call it themselves.
func newTestApp(t *testing.T) (*App, *fakeStore) {
	t.Helper()
	redirectAppDataForTest(t)
	// The LAN listener is ON by default and would bind 0.0.0.0:80/443 (or the
	// 8080/8443 fallback) the moment a test calls startup()/startRemote() — a real
	// listener and a firewall prompt mid-test. Turn it OFF for the redirected
	// %APPDATA%; the handful of tests that WANT a listener save their own enabled
	// settings after this.
	disableRemoteListenerForTest(t)

	a := NewApp(t.TempDir(), nil)
	store := newFakeStore()
	// Wrapped by the same scope decorator NewApp installs around the real
	// store, not installed raw: the decorator is part of the credential path
	// under test — a bare fake would pass keys through unscoped and every
	// preset-scope test would be testing nothing.
	a.store = scopedStore{inner: store, scope: a.credentialScope}

	// The real exitProcess ends the process, which under `go test` is the test
	// binary and every other test in it. Every App a test builds gets a no-op
	// instead; a test that cares whether teardown decided to end the process
	// installs its own recorder over this one.
	a.exitProcess = func() {}

	a.cfgMu.Lock()
	a.cfg = validConfig()
	a.cfgMu.Unlock()

	t.Cleanup(a.teardown)
	return a, store
}

// silencePump marks the event pump as already started, so that a test may call
// domReady without eventPump.start launching a goroutine that would call
// wailsruntime.EventsEmit with a context that has no Wails runtime in it.
func silencePump(a *App) {
	a.events.startOnce.Do(func() {})
}

// drainPump returns every event currently queued, in order.
func drainPump(a *App) []pumpEvent {
	var out []pumpEvent
	for {
		select {
		case e := <-a.events.ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestSignInDelay(t *testing.T) {
	tests := []struct {
		name    string
		attempt int
		want    time.Duration
	}{
		{"negative clamps to the first rung", -3, 1 * time.Second},
		{"first failure", 0, 1 * time.Second},
		{"second", 1, 2 * time.Second},
		{"third", 2, 4 * time.Second},
		{"fourth", 3, 8 * time.Second},
		{"first past the ladder holds at the cap", 4, signInBackoffCap},
		{"far past the ladder still holds at the cap", 500, signInBackoffCap},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := signInDelay(tt.attempt); got != tt.want {
				t.Fatalf("signInDelay(%d) = %v, want %v", tt.attempt, got, tt.want)
			}
		})
	}
}

func TestSignInBackoffIsMonotonicAndBounded(t *testing.T) {
	// The ladder is a design choice, not a measurement (see its doc comment),
	// but it must still be non-decreasing and must not hammer the instance.
	prev := time.Duration(0)
	for i, d := range signInBackoff {
		if d < prev {
			t.Fatalf("signInBackoff[%d] = %v is shorter than its predecessor %v", i, d, prev)
		}
		if d <= 0 {
			t.Fatalf("signInBackoff[%d] = %v must be positive", i, d)
		}
		prev = d
	}
	if signInBackoffCap < prev {
		t.Fatalf("signInBackoffCap %v is shorter than the last rung %v", signInBackoffCap, prev)
	}
}

func TestControlPlaneChanged(t *testing.T) {
	base := validConfig()

	withHost := *base
	withHost.M2LXHost = "other.example.com"
	withAlias := *base
	withAlias.Alias = "someone-else"
	withStatusKey := *base
	withStatusKey.StatusKey = "cam9"
	withSRT := *base
	withSRT.SRTPort = 5000
	withEvent := *base
	withEvent.EventID = "another-event"
	withDevice := *base
	withDevice.AudioDeviceID = "{different-guid}"

	tests := []struct {
		name     string
		previous *config.Config
		next     *config.Config
		want     bool
	}{
		{"nil previous is the first run and must rebuild", nil, base, true},
		{"identical", base, base, false},
		{"m2lxHost", base, &withHost, true},
		{"alias", base, &withAlias, true},
		{"statusKey", base, &withStatusKey, true},
		{"srt fields are read afresh by Start", base, &withSRT, false},
		{"eventId is read afresh by GetKVSCredentials", base, &withEvent, false},
		{"audioDeviceId is read afresh by Start", base, &withDevice, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := controlPlaneChanged(tt.previous, tt.next); got != tt.want {
				t.Fatalf("controlPlaneChanged = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSlatePath(t *testing.T) {
	// Both fixtures are per-GOOS, because what "absolute" MEANS is per-GOOS and
	// this test is about exactly that distinction.
	//
	// "D:/slates/wsl-2026.png" is an absolute path on Windows and a RELATIVE one
	// on darwin — filepath.IsAbs says so, correctly, because a drive letter is
	// not a rooted path anywhere else — so slatePath joined it onto appDir and
	// the subtest failed with
	//
	//	got "C:/Program Files/WSLComms/D:/slates/wsl-2026.png"
	//
	// which is slatePath behaving exactly as documented against a fixture that
	// had stopped meaning what it was written to mean. The same redirection was
	// already done for the %APPDATA% helpers; see withAppData in
	// internal/config/config_test.go, which carries the argument.
	//
	// slatePath's body is untouched by the port and needs no platform knowledge:
	// filepath.IsAbs already has it.
	appDir := filepath.FromSlash("C:/Program Files/WSLComms")
	absolute := filepath.FromSlash("D:/slates/wsl-2026.png")
	if runtime.GOOS != "windows" {
		appDir = filepath.FromSlash("/Applications/WSL Commentary.app/Contents/MacOS")
		absolute = filepath.FromSlash("/Users/commentary/slates/wsl-2026.png")
	}

	tests := []struct {
		name  string
		slate string
		want  string
	}{
		{
			"empty falls back to the documented default beside the exe",
			"",
			filepath.Join(appDir, config.DefaultSlateFilename),
		},
		{
			"the documented default resolves beside the exe",
			config.DefaultSlateFilename,
			filepath.Join(appDir, config.DefaultSlateFilename),
		},
		{
			"a relative path resolves beside the exe",
			filepath.FromSlash("assets/alt.png"),
			filepath.Join(appDir, "assets", "alt.png"),
		},
		{
			"an absolute path is taken as given",
			absolute,
			absolute,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &App{appDir: appDir}
			cfg := config.Defaults()
			cfg.SlatePath = tt.slate
			if got := a.slatePath(cfg); got != tt.want {
				t.Fatalf("slatePath(%q) = %q, want %q", tt.slate, got, tt.want)
			}
		})
	}
}

func TestAppDir(t *testing.T) {
	dir, err := appDir()
	if err != nil {
		t.Fatalf("appDir() error = %v", err)
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("appDir() = %q, want an absolute path", dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("appDir() = %q, which does not stat: %v", dir, err)
	}
	if !info.IsDir() {
		t.Fatalf("appDir() = %q, which is not a directory", dir)
	}
}

func TestSetWebView2Arguments(t *testing.T) {
	// Specification section 7. Without these two flags enumerateDevices returns
	// blank labels and the headphone dropdown is a list of empty strings, so the
	// exact flag names are load-bearing rather than cosmetic.
	t.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", "")

	setWebView2Arguments()

	got := os.Getenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS")
	for _, want := range []string{
		"--autoplay-policy=no-user-gesture-required",
		"--auto-accept-camera-and-microphone-capture",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS = %q, missing %q", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// The SRT passphrase, which is where a secret meets a policy decision
// ---------------------------------------------------------------------------

func TestSRTPassphrase(t *testing.T) {
	boom := errors.New("credential manager is unavailable")

	tests := []struct {
		name     string
		stored   string
		hasValue bool
		getErr   error
		pbKeyLen int
		want     string
		wantErr  bool
		// errMentions is checked against the error string, so that the operator
		// is told which Credential Manager target to look at.
		errMentions string
	}{
		{
			name:     "no passphrase and no encryption requested is a valid unencrypted session",
			pbKeyLen: 0,
			want:     "",
		},
		{
			name:        "pbkeylen 16 with no passphrase is refused before libsrt gets the chance",
			pbKeyLen:    16,
			wantErr:     true,
			errMentions: secrets.TargetSRT,
		},
		{
			name:        "pbkeylen 32 with no passphrase is refused too",
			pbKeyLen:    32,
			wantErr:     true,
			errMentions: secrets.TargetSRT,
		},
		{
			name:     "a stored passphrase is returned",
			stored:   "correct horse battery staple",
			hasValue: true,
			pbKeyLen: 16,
			want:     "correct horse battery staple",
		},
		{
			name:     "a stored passphrase with pbkeylen 0 is still returned",
			stored:   "s3cret",
			hasValue: true,
			pbKeyLen: 0,
			want:     "s3cret",
		},
		{
			name:        "a broken credential store is an error, not an unencrypted session",
			getErr:      boom,
			pbKeyLen:    0,
			wantErr:     true,
			errMentions: secrets.TargetSRT,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			if tt.hasValue {
				if err := store.Set(secrets.KeySRT, tt.stored); err != nil {
					t.Fatalf("seeding the store: %v", err)
				}
			}
			store.getErr = tt.getErr

			a := &App{store: store}
			cfg := config.Defaults()
			cfg.PBKeyLen = tt.pbKeyLen

			got, err := a.srtPassphrase(cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("srtPassphrase() = %q, nil; want an error", got)
				}
				if tt.errMentions != "" && !strings.Contains(err.Error(), tt.errMentions) {
					t.Fatalf("error %q does not mention %q", err, tt.errMentions)
				}
				// A secret must never reach an error string.
				if tt.stored != "" && strings.Contains(err.Error(), tt.stored) {
					t.Fatalf("error %q leaks the passphrase", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("srtPassphrase() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("srtPassphrase() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The event pump
// ---------------------------------------------------------------------------

func TestEventPumpNeverBlocks(t *testing.T) {
	p := newEventPump()

	// Overfill it by a wide margin. Nothing is consuming, so if send could block
	// this would deadlock and the test would time out rather than fail.
	const overfill = eventQueueDepth * 8
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < overfill; i++ {
			p.send(EventError, fmt.Sprintf("event %d", i))
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("eventPump.send blocked with a full queue; a slow renderer would stall the sender")
	}

	if got := len(p.ch); got > eventQueueDepth {
		t.Fatalf("queue holds %d events, more than its depth %d", got, eventQueueDepth)
	}
}

func TestEventPumpKeepsTheNewestEvent(t *testing.T) {
	p := newEventPump()

	for i := 0; i < eventQueueDepth*3; i++ {
		p.send(EventSender, i)
	}

	// A lamp renders the current value, so what must survive is the latest.
	var last any
	for {
		select {
		case e := <-p.ch:
			last = e.data
			continue
		default:
		}
		break
	}
	if last != eventQueueDepth*3-1 {
		t.Fatalf("newest queued event = %v, want %v", last, eventQueueDepth*3-1)
	}
}

func TestEventPumpConcurrentProducersTerminate(t *testing.T) {
	// The three-step send is deliberately bounded rather than a retry loop,
	// because several producers write to this queue: the status forwarder, the
	// sender forwarder and emitError from any goroutine. A retry loop could spin
	// while the others keep refilling the slot. This asserts termination under
	// exactly that contention.
	p := newEventPump()

	const producers = 16
	const each = 500

	var wg sync.WaitGroup
	for i := 0; i < producers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < each; j++ {
				p.send(EventStatus, id*each+j)
			}
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("concurrent producers did not all return; eventPump.send is not bounded")
	}
}

// ---------------------------------------------------------------------------
// Startup
// ---------------------------------------------------------------------------

func TestStartupEmitsNothingUntilDomReady(t *testing.T) {
	// This is the whole reason startup and domReady are separate. Wails runs
	// OnStartup before the page exists, so an event emitted there reaches a page
	// with no listeners and is lost — and the events startup produces are the
	// first-run ones that matter most.
	redirectAppDataForTest(t)
	disableRemoteListenerForTest(t) // startup() calls startRemote; keep it off real ports

	a := NewApp(t.TempDir(), errors.New("GStreamer bundle is missing libsrt"))
	a.store = newFakeStore()
	t.Cleanup(a.teardown)

	a.startup(context.Background())

	if a.events.ch == nil {
		t.Fatal("event queue was not created")
	}
	queued := drainPump(a)
	if len(queued) == 0 {
		t.Fatal("startup queued no events; the gst.Init failure should have been reported")
	}

	var sawGstError bool
	for _, e := range queued {
		if e.name != EventError {
			continue
		}
		if s, ok := e.data.(string); ok && strings.Contains(s, "libsrt") {
			sawGstError = true
		}
	}
	if !sawGstError {
		t.Fatalf("the gst.Init failure was not queued for the frontend; queued = %+v", queued)
	}
}

func TestStartupWithNoM2LXHostBuildsNoControlPlane(t *testing.T) {
	// First run: nothing is configured. This must not be treated as an error and
	// must not open a socket to a host that does not exist.
	redirectAppDataForTest(t)
	disableRemoteListenerForTest(t) // startup() calls startRemote; keep it off real ports

	a := NewApp(t.TempDir(), nil)
	a.store = newFakeStore()
	t.Cleanup(a.teardown)

	a.startup(context.Background())

	a.ctlMu.Lock()
	client, cancel := a.client, a.ctlCancel
	a.ctlMu.Unlock()

	if client != nil || cancel != nil {
		t.Fatal("a control plane was built with no m2lxHost configured")
	}
}

func TestDomReadyReplaysTheCurrentSenderState(t *testing.T) {
	// A page that reloads mid-match must not show the SENDING lamp grey while
	// the feed is up. The sender only emits on transitions, so the current state
	// is replayed here instead.
	//
	// domReady replays FIVE, not one: the return monitor and the picture monitor
	// both emit only on transitions for the same reason, and a reloaded page
	// would otherwise show the RETURN lamp grey with audio in the commentator's
	// ears, and draw the fallback mosaic over a working high-resolution picture.
	// The count is asserted exactly so that a sixth replay has to be a decision
	// — every event queued here is delivered to a page that has just loaded, and
	// the ones the status lamps would need are deliberately NOT replayed because
	// a cached Status risks showing stale green (specification section 8).
	//
	// The fourth is the video signal lamp, and it needs the replay MORE than the
	// three above it rather than less. A sender reconnects and a return monitor
	// retries, so those lamps repopulate themselves from live traffic within
	// seconds of a reload; a locked capture input reports ONCE at the start of
	// the match and is then silent for ninety minutes, because silence is what
	// healthy looks like. Without the replay a page reloaded at half-time shows
	// the lamp grey — "this application cannot tell you" — over a picture the
	// operator can see perfectly well.
	//
	// The fifth is the DeckLink routing state, and it is the one replay that must
	// come from the CACHE rather than from the pipeline. Reading the pad would
	// take a lock internal/gst holds across state changes, on the Wails MAIN
	// THREAD — so a page reloaded during a reconnect would freeze the window for
	// as long as the sink swap took. The routing screen calls GetChannelMap when
	// it opens, and that call does read the pad.
	a, _ := newTestApp(t)
	silencePump(a)

	a.senderMu.Lock()
	a.lastSender = sender.StateConnected
	a.senderMu.Unlock()

	a.domReady(context.Background())

	queued := drainPump(a)
	if len(queued) != 5 {
		t.Fatalf("domReady queued %d events, want the sender, return, picture, signal and "+
			"channel-map replays: %+v", len(queued), queued)
	}
	if queued[0].name != EventSender || queued[0].data != sender.StateConnected {
		t.Fatalf("domReady queued %+v, want %s = %s", queued[0], EventSender, sender.StateConnected)
	}
	if queued[1].name != EventReturn || queued[1].data != gst.ReturnStateStopped {
		t.Fatalf("domReady queued %+v, want %s = %s", queued[1], EventReturn, gst.ReturnStateStopped)
	}
	if queued[2].name != EventPicture || queued[2].data != gst.PictureStateStopped {
		t.Fatalf("domReady queued %+v, want %s = %s", queued[2], EventPicture, gst.PictureStateStopped)
	}
	if queued[3].name != EventSignal || queued[3].data != (signalPayload{State: gst.SignalUnknown}) {
		t.Fatalf("domReady queued %+v, want %s = %s", queued[3], EventSignal, gst.SignalUnknown)
	}
	// Zero channels and an empty map: no session has negotiated a pad, so there
	// is no width to size a routing grid against and nothing is in force. The map
	// must be an EMPTY LIST rather than a null — the frontend tests
	// Array.isArray before adopting one, and null would take the "this build
	// cannot tell me" path instead of "nobody has chosen".
	gotMap, ok := queued[4].data.(channelMapPayload)
	if queued[4].name != EventChannelMap || !ok {
		t.Fatalf("domReady queued %+v, want a %s carrying a channelMapPayload",
			queued[4], EventChannelMap)
	}
	if gotMap.InputChannels != 0 || gotMap.Map == nil || len(gotMap.Map) != 0 || !gotMap.IsDefault {
		t.Fatalf("domReady queued %+v, want no negotiated channels and an empty, non-nil map",
			gotMap)
	}
}

func TestDomReadyReplaysStoppedBeforeAnySession(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	a.domReady(context.Background())

	queued := drainPump(a)
	if len(queued) != 5 {
		t.Fatalf("domReady queued %+v, want a sender, a return, a picture, a signal and a "+
			"channel-map replay", queued)
	}
	if queued[0].data != sender.StateStopped {
		t.Fatalf("domReady queued %+v, want %s before any session has run", queued, sender.StateStopped)
	}
	if queued[1].data != gst.ReturnStateStopped {
		t.Fatalf("domReady queued %+v, want %s before any return monitor has run",
			queued, gst.ReturnStateStopped)
	}
	if queued[2].data != gst.PictureStateStopped {
		t.Fatalf("domReady queued %+v, want %s before any picture monitor has run",
			queued, gst.PictureStateStopped)
	}
	// UNKNOWN and not LOST. Before any session has run there is no capture
	// element to poll, and on a machine with no card there never will be — the
	// lamp must say "this application cannot tell", which is a different claim
	// from a card telling us there is nothing on its input.
	if queued[3].data != (signalPayload{State: gst.SignalUnknown}) {
		t.Fatalf("domReady queued %+v, want %s before any capture has been measured",
			queued, gst.SignalUnknown)
	}
	if got, ok := queued[4].data.(channelMapPayload); !ok || got.InputChannels != 0 {
		t.Fatalf("domReady queued %+v, want no negotiated channels before any pipeline has run",
			queued)
	}
}

func TestRuntimeContextBeforeStartup(t *testing.T) {
	// Wails serves OnSecondInstanceLaunch on the single-instance named pipe
	// goroutine, which is unordered with respect to OnStartup. A second launch
	// arriving first must be ignored, not passed a nil context — every
	// wailsruntime function panics on one.
	a := &App{}

	if _, ok := a.runtimeContext(); ok {
		t.Fatal("runtimeContext() reported a context before startup captured one")
	}

	// Must not panic.
	a.secondInstanceLaunched(options.SecondInstanceData{})

	ctx := context.Background()
	a.setRuntimeContext(ctx)
	got, ok := a.runtimeContext()
	if !ok || got != ctx {
		t.Fatalf("runtimeContext() = %v, %v; want the context startup captured", got, ok)
	}
}

func TestRuntimeContextIsSafeUnderConcurrentAccess(t *testing.T) {
	// The write is startup's, on the main thread; the reads are the named-pipe
	// goroutine's. This is the interleaving, run often enough to be worth
	// something without the race detector (unavailable at Gate A: it needs cgo).
	a := &App{}

	var wg sync.WaitGroup
	begin := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-begin
		a.setRuntimeContext(context.Background())
	}()

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-begin
			for j := 0; j < 1000; j++ {
				if ctx, ok := a.runtimeContext(); ok && ctx == nil {
					t.Error("runtimeContext() reported ok with a nil context")
					return
				}
			}
		}()
	}

	close(begin)
	wg.Wait()
}

// ---------------------------------------------------------------------------
// The bound surface
// ---------------------------------------------------------------------------

func TestListInputDevices(t *testing.T) {
	a, _ := newTestApp(t)

	devices, err := a.ListInputDevices()
	if err != nil {
		t.Fatalf("ListInputDevices() error = %v", err)
	}
	if len(devices) == 0 {
		t.Fatal("ListInputDevices() returned nothing; the stub offers three")
	}
	for _, d := range devices {
		if d.ID == "" || d.Name == "" {
			t.Fatalf("device %+v has an empty field; the dropdown needs both", d)
		}
	}
}

func TestListInputDevicesReportsAFailedGstInit(t *testing.T) {
	initErr := errors.New("the bundled GStreamer could not be initialised")
	a := &App{gstInitErr: initErr}

	if _, err := a.ListInputDevices(); !errors.Is(err, initErr) {
		t.Fatalf("ListInputDevices() error = %v, want the gst.Init failure", err)
	}
}

func TestGetConfigReturnsACopy(t *testing.T) {
	a, _ := newTestApp(t)

	got, err := a.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	got.M2LXHost = "mutated-by-the-caller"

	again, err := a.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	if again.M2LXHost == "mutated-by-the-caller" {
		t.Fatal("GetConfig() handed out the live configuration; a caller mutated it without SaveConfig")
	}
}

func TestGetConfigOnAnEmptyAppReturnsDefaults(t *testing.T) {
	a := &App{}
	got, err := a.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	if got == nil {
		t.Fatal("GetConfig() returned nil; the Settings screen would have nothing to render")
	}
	if got.SRTLatencyMs != config.DefaultSRTLatencyMs {
		t.Fatalf("srtLatencyMs = %d, want the documented default %d", got.SRTLatencyMs, config.DefaultSRTLatencyMs)
	}
}

func TestSaveConfigRejectsNil(t *testing.T) {
	a, _ := newTestApp(t)
	if err := a.SaveConfig(nil); err == nil {
		t.Fatal("SaveConfig(nil) succeeded; it must not")
	}
}

func TestSaveConfigPersistsAndDoesNotValidate(t *testing.T) {
	// Half-filled Settings must be savable: on first run every field the operator
	// has not reached yet is empty, and refusing to save would make the screen
	// unusable. Validation belongs to Start.
	a, _ := newTestApp(t)

	partial := config.Defaults()
	partial.Alias = "half-way-through"

	if err := a.SaveConfig(partial); err != nil {
		t.Fatalf("SaveConfig() on a half-filled configuration error = %v", err)
	}

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	if loaded.Alias != "half-way-through" {
		t.Fatalf("alias = %q, want it persisted", loaded.Alias)
	}

	got, err := a.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	if got.Alias != "half-way-through" {
		t.Fatalf("in-memory alias = %q, want it updated", got.Alias)
	}
}

func TestSetSecretWritesThroughAndHasNoGetter(t *testing.T) {
	a, store := newTestApp(t)

	if err := a.SetSecret(secrets.KeySRT, "hunter2"); err != nil {
		t.Fatalf("SetSecret() error = %v", err)
	}
	got, err := store.Get(secrets.KeySRT)
	if err != nil {
		t.Fatalf("store.Get() error = %v", err)
	}
	if got != "hunter2" {
		t.Fatalf("stored %q, want %q", got, "hunter2")
	}

	// None of the bound methods reads a secret, and the surface is exactly the
	// list app.go's header documents. Wails binds every exported method, so one
	// more would silently widen the contract with WP-5a and WP-5b.
	assertBoundSurface(t)
}

// assertBoundSurface checks that *App exports exactly the methods app.go's
// header declares. Wails binds every exported method of the bound object, so
// this is the contract with the frontend, enforced.
//
// It was seven. GetStatusKeyCandidates is the eighth, added deliberately and
// documented in app.go's header: the three WebSocket-derived lamps are useless
// without a statusKey, and no M2L-X endpoint will name one (specification open
// question 5). This list is the place that decision has to be re-made rather
// than drifted into, which is why the test asserts equality in both directions.
//
// The six mixer methods are the ninth to fourteenth, added when the mixer
// drawer was wired to Settings and documented in the same header. They are
// listed second here so the original eight stay legible as one group. Their
// shapes and their safety properties are asserted in app_mixer_test.go; this
// list exists only to make an addition a decision rather than an accident.
//
// The five return methods are the fifteenth to nineteenth, added with the SRT
// return monitor and documented in the same header. Four are read-only or
// purely local; StartReturn and StopReturn open and close a second SRT session
// that only ever RECEIVES, on a separate lock, a separate pipeline and a
// separate validation path from the contribution feed. They are listed third for
// the same reason the mixer six are listed second: so that each group stays
// legible as one decision.
//
// The five picture methods are the twentieth to twenty-fourth, added when SRT
// became the PICTURE path and documented in the same header. Three of them
// exist because the picture is a NATIVE CHILD WINDOW painted over the page
// rather than an element in it: SetPictureRect and SetPictureVisible are the
// frontend telling an overlay that does not participate in CSS layout where to
// sit and when to get out of the way, and they are on this surface precisely
// because there is no other way for the page to say it. They are listed fourth
// so that group stays legible too.
//
// The seven preset methods are the next group, added with the M2L-X instance
// presets and documented in the same header. Four are read-only or rename a
// display string; SavePreset writes files under %APPDATA% only; ApplyPreset is
// the one that changes the running configuration and it refuses outright while
// a session is sending. GetPresetCredentialStatus is the recorded, deliberate
// exception to "no secret crosses this boundary outbound": it reports whether
// a credential EXISTS for the active preset scope — booleans, never values —
// and app_presets.go carries the whole argument beside the type.
//
// CredentialStoreName arrived with the macOS port and is the one addition that
// was NOT a decision when it landed — it was exported without a row here or in
// remoteAllowlist, which is precisely the drift both guards exist to catch, and
// they duly failed until it was classified. It returns what this platform's
// credential vault is CALLED so a dialog can name it, which is a per-platform
// constant and not a credential: the "no secret crosses this boundary outbound"
// rule is untouched, and GetPresetCredentialStatus remains its only narrowing.
//
// The two remote-access methods are the last group, added with the LAN control
// bridge and documented in app_remote.go. They are BOTH host-only — the remote
// dispatcher refuses them from every connection — because they change WHETHER
// the listener runs and on WHAT address and ports, and a listener reconfigurable
// by its own remote connections could be turned off or moved by whoever first
// gets in. They are on this bound surface solely so the LOCAL Settings screen
// can drive them. (There used to be three more — AddRemoteClient,
// SetRemoteClientPassword, DeleteRemoteClient — but the listener is now
// unauthenticated by the owner's decision, so there are no client accounts to
// manage; see app_remote.go and docs/remote-access.md.) GetRemoteState carries
// no secret, keeping the "no secret crosses this boundary outbound" rule that
// GetPresetCredentialStatus is the only recorded narrowing of.
//
// GetChannelMap and SetChannelMap are the newest pair, added with the DeckLink
// routing screen. They are the two halves of ONE control and neither is useful
// alone: the read reports what the capture pad NEGOTIATED, which is the only
// number a mix matrix may be sized against — the device's advertised
// max-channels is not that number, and a matrix of the wrong width stops the
// capture chain rather than degrading it — and the write applies a routing to
// the running pipeline in about 119 microseconds with no state change. There is
// no Apply button on the screen that calls them, which is why the write is a
// binding rather than a field on SaveConfig: config.json records the routing for
// the next launch, and this changes what the commentator is heard on now.
//
// GetConformTarget is the last, added with the conform work. It is read-only and
// carries no secret: the raster and rate the video leg is — or would be — built
// to, plus the provenance of that answer. It is on this surface because the
// alternative was the defect it replaces, a VIDEO OK lamp judging every feed
// against a hardcoded 1080p50 and therefore reading red on a correctly
// configured 720p50 facility. It is the frontend's ONLY route to that number,
// deliberately: deriving it a second time in JavaScript is how the page and the
// pipeline start disagreeing about which of them is telling the truth.
func assertBoundSurface(t *testing.T) {
	t.Helper()

	want := map[string]bool{
		"ListInputDevices":       true,
		"GetConfig":              true,
		"SaveConfig":             true,
		"SetSecret":              true,
		"Start":                  true,
		"Stop":                   true,
		"GetKVSCredentials":      true,
		"GetStatusKeyCandidates": true,
		"ListEvents":             true,
		"CredentialStoreName":    true,

		"GetMixerSnapshot":  true,
		"ArmMixer":          true,
		"DisarmMixer":       true,
		"SendMixerCommands": true,
		"GetMixerGolden":    true,
		"SetMixerGolden":    true,

		"ListOutputDevices":   true,
		"StartReturn":         true,
		"StopReturn":          true,
		"GetReturnState":      true,
		"IsSRTReturnSelected": true,

		"StartPicture":      true,
		"StopPicture":       true,
		"GetPictureState":   true,
		"SetPictureRect":    true,
		"SetPictureVisible": true,

		"ListPresets":               true,
		"SavePreset":                true,
		"ApplyPreset":               true,
		"RenamePreset":              true,
		"DeletePreset":              true,
		"GetActivePreset":           true,
		"GetPresetCredentialStatus": true,

		"GetRemoteState":    true,
		"SetRemoteListener": true,

		"GetConformTarget": true,

		// GetSwitcherFormat is GetConformTarget's other half and is a SEPARATE
		// binding on purpose. GetConformTarget answers "what will WE produce",
		// which before Start is the operator's own declaration echoed back;
		// this answers "what is the INSTANCE configured for", read live over
		// REST. Drawn side by side on the Settings screen they make a
		// divergence visible — an override typed for last month's venue against
		// the switcher this position is actually feeding — which is the entire
		// point and which one merged number could not express. It is not folded
		// into GetConformTarget for a second reason too: that method is on the
		// page's startup path and commits to making no network call there.
		"GetSwitcherFormat": true,

		"GetChannelMap": true,
		"SetChannelMap": true,

		// The video leg. SetVideoSource is the one method on this whole surface
		// that decides what a broadcast switcher receives, and the other three are
		// the operator's confidence monitor: whether it exists, where it goes and
		// whether it is on screen. All four are host-only — see
		// TestRemoteHostOnlySet — and the two that write configuration are backed
		// by App.refuseRemoteVideoLegChange, because SaveConfig is remotely
		// reachable and would otherwise be the way round them.
		"SetVideoSource":            true,
		"SetDeckLinkPreviewEnabled": true,
		"SetPreviewRect":            true,
		"SetPreviewVisible":         true,
	}

	got := exportedMethodsOfApp()
	for name := range got {
		if !want[name] {
			t.Fatalf("*App exports %q, which is not in the frozen bound surface; "+
				"Wails would bind it and widen the contract with WP-5a/WP-5b", name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Fatalf("*App no longer exports %q, which the frontend calls", name)
		}
	}
}

func TestGetKVSCredentialsWithoutAControlPlane(t *testing.T) {
	a, _ := newTestApp(t)

	_, err := a.GetKVSCredentials()
	if err == nil {
		t.Fatal("GetKVSCredentials() succeeded with no client; want an error naming the missing setting")
	}
	if !strings.Contains(err.Error(), "m2lxHost") {
		t.Fatalf("error %q does not name the field the operator must fill in", err)
	}
}

func TestGetKVSCredentialsWithoutAnEventID(t *testing.T) {
	a, _ := newTestApp(t)

	cfg := validConfig()
	cfg.EventID = ""
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	// Give it a client so the eventId check is the one that fires.
	a.ctlMu.Lock()
	a.client = stubClient{}
	a.ctlMu.Unlock()

	_, err := a.GetKVSCredentials()
	if err == nil || !strings.Contains(err.Error(), "eventId") {
		t.Fatalf("GetKVSCredentials() error = %v, want one naming eventId", err)
	}
}

func TestGetKVSCredentialsWhenNotSignedIn(t *testing.T) {
	// The monitor page retries this whenever its peer connection dies, so during
	// a sign-in outage this is the message the operator sees repeatedly. It must
	// point at the password, not at the KVS chain.
	a, _ := newTestApp(t)

	a.ctlMu.Lock()
	a.client = stubClient{token: ""}
	a.ctlMu.Unlock()

	_, err := a.GetKVSCredentials()
	if err == nil {
		t.Fatal("GetKVSCredentials() succeeded while not signed in")
	}
	if !strings.Contains(err.Error(), secrets.TargetM2LX) {
		t.Fatalf("error %q does not name the Credential Manager target holding the password", err)
	}
}

// TestListEventsReturnsTheClientsEvents is the happy path: a signed-in client
// with events yields exactly them, so the frontend can auto-select the sole
// event or offer a picker.
func TestListEventsReturnsTheClientsEvents(t *testing.T) {
	a, _ := newTestApp(t)

	want := []m2lx.Event{
		{ID: "e1", Name: "Alpha", Status: "Running"},
		{ID: "e2", Name: "Bravo", Status: "Idle"},
	}
	a.ctlMu.Lock()
	a.client = stubClient{token: "tok", fakeEvents: want}
	a.ctlMu.Unlock()

	got, err := a.ListEvents()
	if err != nil {
		t.Fatalf("ListEvents() error = %v, want nil", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListEvents() = %v, want %v", got, want)
	}
}

// TestListEventsWhenNotSignedIn proves the not-signed-in guard fires BEFORE the
// client is called: with a token-less client the operator is pointed at the
// alias and the password, the same fix GetKVSCredentials names, rather than a
// bare ErrNotSignedIn bubbling up.
func TestListEventsWhenNotSignedIn(t *testing.T) {
	a, _ := newTestApp(t)

	a.ctlMu.Lock()
	a.client = stubClient{token: ""}
	a.ctlMu.Unlock()

	_, err := a.ListEvents()
	if err == nil {
		t.Fatal("ListEvents() succeeded while not signed in")
	}
	if !strings.Contains(err.Error(), secrets.TargetM2LX) {
		t.Fatalf("error %q does not name the Credential Manager target holding the password", err)
	}
}

// TestListEventsWithoutAControlPlane proves the no-client guard: with no
// configured m2lxHost there is no client to call, and the error must name the
// Settings field the operator has to fill in.
func TestListEventsWithoutAControlPlane(t *testing.T) {
	a, _ := newTestApp(t)

	_, err := a.ListEvents()
	if err == nil {
		t.Fatal("ListEvents() succeeded with no client; want an error naming the missing setting")
	}
	if !strings.Contains(err.Error(), "m2lxHost") {
		t.Fatalf("error %q does not name the field the operator must fill in", err)
	}
}

// ---------------------------------------------------------------------------
// The session lifecycle
// ---------------------------------------------------------------------------

func TestStartRefusesAnIncompleteConfigurationAndNamesTheFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.Config)
		mustSay string
	}{
		{"missing m2lxHost", func(c *config.Config) { c.M2LXHost = "" }, "m2lxHost"},
		{"missing alias", func(c *config.Config) { c.Alias = "" }, "alias"},
		{"missing eventId", func(c *config.Config) { c.EventID = "" }, "eventId"},
		// statusKey is deliberately absent from this list: an empty statusKey
		// costs the three WebSocket lamps, not the feed, and is covered by
		// TestStartAcceptsAnEmptySRTHostAndStatusKey below. There is no srtHost
		// field any more — the SRT host is always the M2L-X host, so a missing
		// SRT host IS a missing m2lxHost, already the first case above.
		{"missing audioDeviceId", func(c *config.Config) { c.AudioDeviceID = "" }, "audioDeviceId"},
		{"port zero", func(c *config.Config) { c.SRTPort = 0 }, "srtPort"},
		{"port out of range", func(c *config.Config) { c.SRTPort = 70000 }, "srtPort"},
		{"pbkeylen not an SRT key length", func(c *config.Config) { c.PBKeyLen = 24 }, "pbkeylen"},
		{"returnMid out of the transceiver range", func(c *config.Config) { c.ReturnMid = 9 }, "returnMid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := newTestApp(t)

			cfg := validConfig()
			tt.mutate(cfg)
			a.cfgMu.Lock()
			a.cfg = cfg
			a.cfgMu.Unlock()

			err := a.Start()
			if err == nil {
				_ = a.Stop()
				t.Fatal("Start() succeeded on an invalid configuration")
			}
			if !strings.Contains(err.Error(), tt.mustSay) {
				t.Fatalf("Start() error = %q, which does not name the bad field %q", err, tt.mustSay)
			}

			// A refused Start must leave nothing behind. sender.Start leaves the
			// pipeline it was given untouched on failure, and a gst.Pipeline is
			// single-use, so a leaked one would hold the capture device for the
			// rest of the process.
			a.sessMu.Lock()
			sess := a.session
			a.sessMu.Unlock()
			if sess != nil {
				t.Fatal("a refused Start left a session behind")
			}
		})
	}
}

func TestStartStopRoundTrip(t *testing.T) {
	a, store := newTestApp(t)
	if err := store.Set(secrets.KeySRT, "a-passphrase"); err != nil {
		t.Fatalf("seeding the store: %v", err)
	}
	cfg := validConfig()
	cfg.PBKeyLen = 16
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	a.sessMu.Lock()
	sess := a.session
	a.sessMu.Unlock()
	if sess == nil {
		t.Fatal("Start() left no session")
	}

	stub, ok := sess.pipe.(*gst.StubPipeline)
	if !ok {
		t.Fatalf("pipeline is %T, want the Gate A stub", sess.pipe)
	}

	// The sink must have been given the SRT coordinates from config and the
	// passphrase from Credential Manager, not from config.json.
	waitFor(t, 5*time.Second, "a sink to be attached", func() bool {
		_, attached := stub.AttachedSink()
		return attached
	})
	sink, _ := stub.AttachedSink()
	if sink.Host != cfg.EffectiveSRTHost() || sink.Port != cfg.SRTPort {
		t.Fatalf("sink dialled %s:%d, want %s:%d", sink.Host, sink.Port, cfg.EffectiveSRTHost(), cfg.SRTPort)
	}
	if sink.Passphrase != "a-passphrase" {
		t.Fatalf("sink passphrase = %q, want the one from the credential store", sink.Passphrase)
	}
	if sink.PBKeyLen != 16 {
		t.Fatalf("sink pbkeylen = %d, want 16", sink.PBKeyLen)
	}
	if sink.LatencyMs != cfg.SRTLatencyMs {
		t.Fatalf("sink latency = %d ms, want %d ms", sink.LatencyMs, cfg.SRTLatencyMs)
	}

	// The slate and the capture device come from PipelineOpts.
	started := stub.StartedWith()
	if started.AudioDeviceID != cfg.AudioDeviceID {
		t.Fatalf("pipeline device = %q, want the configured endpoint GUID", started.AudioDeviceID)
	}
	if !filepath.IsAbs(started.SlatePath) {
		t.Fatalf("slate path %q is not absolute; it must resolve beside the executable", started.SlatePath)
	}

	// A successful connect must be followed by a forced key unit, so the far end
	// gets a picture at once instead of waiting up to two seconds for the next
	// scheduled IDR (specification section 6.2).
	waitFor(t, 5*time.Second, "a forced key unit after the connect", func() bool {
		return stub.Counters().ForceKeyUnits > 0
	})

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if got := stub.State(); got != gst.StubStateStopped {
		t.Fatalf("pipeline state after Stop = %v, want %v", got, gst.StubStateStopped)
	}

	a.sessMu.Lock()
	remaining := a.session
	a.sessMu.Unlock()
	if remaining != nil {
		t.Fatal("Stop() left a session behind")
	}
}

func TestStartTwiceIsRefused(t *testing.T) {
	a, _ := newTestApp(t)

	if err := a.Start(); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	defer func() { _ = a.Stop() }()

	err := a.Start()
	if !errors.Is(err, errAlreadySending) {
		t.Fatalf("second Start() error = %v, want errAlreadySending", err)
	}
}

func TestStopWithoutStart(t *testing.T) {
	a, _ := newTestApp(t)

	if err := a.Stop(); !errors.Is(err, errNotSending) {
		t.Fatalf("Stop() error = %v, want errNotSending", err)
	}
}

func TestStartReportsAFailedGstInit(t *testing.T) {
	a, _ := newTestApp(t)
	initErr := errors.New("libsrt is missing from the bundle")
	a.gstInitErr = initErr

	if err := a.Start(); !errors.Is(err, initErr) {
		t.Fatalf("Start() error = %v, want the gst.Init failure", err)
	}
}

// ---------------------------------------------------------------------------
// The audio-device pre-flight
// ---------------------------------------------------------------------------
//
// startSession checks the saved commentary input id BEFORE building a
// pipeline, because both bad ids used to fail twenty seconds too late and
// blame the network: wasapi2src accepts any id at Start and fails
// asynchronously, and the sender read that bus error as a connection failure —
// "the commentary feed to <host>:40005 is not connected and is retrying",
// measured live, about a playback endpoint in config.json. These tests pin the
// two refusals AND the two deliberate non-refusals: an enumeration hiccup must
// never become a new way to be unable to go on air.

// withStubDevices replaces the gst stub's input device list for one test and
// restores the default three afterwards. Pass an empty (non-nil) slice for the
// empty-list case — nil is the stub's "restore defaults" sentinel.
func withStubDevices(t *testing.T, devices []gst.Device) {
	t.Helper()
	gst.SetStubDevices(devices)
	t.Cleanup(func() { gst.SetStubDevices(nil) })
}

// withStubDeviceError makes the stub's ListInputDevices fail for one test.
func withStubDeviceError(t *testing.T, err error) {
	t.Helper()
	gst.SetStubDeviceError(err)
	t.Cleanup(func() { gst.SetStubDeviceError(nil) })
}

// renderEndpointID is a Windows RENDER (playback) endpoint id in the measured
// shape of the operator's live failure — the namespace is the discriminator:
// 0.0.0 is render, 0.0.1 is capture.
const renderEndpointID = "{0.0.0.00000000}.{8678ce58-7b71-4bd4-810f-1c4a7f11ec71}"

// assertNoSession fails the test if a refused Start left a session behind.
func assertNoSession(t *testing.T, a *App) {
	t.Helper()
	a.sessMu.Lock()
	sess := a.session
	a.sessMu.Unlock()
	if sess != nil {
		t.Fatal("a refused Start left a session behind")
	}
}

func TestStartRefusesAVanishedAudioDevice(t *testing.T) {
	// The saved id is a well-formed CAPTURE id that this machine no longer has
	// — a Dante Virtual Soundcard channel whose source has gone, or a
	// config.json copied from another machine. Without the pre-flight this
	// prerolls, fails asynchronously and is retried as a network fault.
	a, _ := newTestApp(t)
	withStubDevices(t, []gst.Device{
		{ID: "{0.0.1.00000000}.{c41a9d7e-0004-438e-9003-51a46e13a0c1}", Name: "DVS Receive  3-4 (Dante Virtual Soundcard)"},
		{ID: "{0.0.1.00000000}.{9f6d2b18-0004-438e-9003-51a46e13a4d5}", Name: "Microphone (Focusrite Scarlett 2i2 USB)"},
	})

	cfg := validConfig()
	cfg.AudioDeviceID = "{0.0.1.00000000}.{aaaaaaaa-0004-438e-9003-51a46e139bfc}"
	setConfig(a, cfg)

	err := a.Start()
	if err == nil {
		_ = a.Stop()
		t.Fatal("Start() succeeded with a saved device id that is not on this machine")
	}
	for _, want := range []string{
		cfg.AudioDeviceID,
		"not present on this machine",
		"2 inputs were found",
		"Commentary input dropdown",
		"belongs to another machine",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Start() error %q does not say %q", err, want)
		}
	}
	assertNoSession(t, a)
}

func TestStartRefusesARenderEndpointWhetherOfferedOrNot(t *testing.T) {
	// The operator's live defect: "we had an output selected as an input".
	// The namespace check fires FIRST, before enumeration is even consulted,
	// so the refusal is the playback message in both cases — including the
	// case where a stale enumeration would have offered the device, which is
	// exactly how the id got saved in the first place.
	tests := []struct {
		name    string
		devices []gst.Device
	}{
		{
			// The dropdown-regression case: enumeration still lists the render
			// endpoint (an old build, or a future filter regression). Presence
			// in the list must not launder a playback device into a microphone.
			name: "offered by enumeration",
			devices: []gst.Device{
				{ID: renderEndpointID, Name: "Speakers (Realtek(R) Audio)"},
				{ID: "{0.0.1.00000000}.{9f6d2b18-0004-438e-9003-51a46e13a4d5}", Name: "Microphone (Focusrite Scarlett 2i2 USB)"},
			},
		},
		{
			// The filtered case: enumeration no longer offers it, so the id
			// would ALSO fail the presence check — but the operator must be
			// told it is a playback endpoint, not merely that it is absent,
			// because their next move differs.
			name: "not offered by enumeration",
			devices: []gst.Device{
				{ID: "{0.0.1.00000000}.{9f6d2b18-0004-438e-9003-51a46e13a4d5}", Name: "Microphone (Focusrite Scarlett 2i2 USB)"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := newTestApp(t)
			withStubDevices(t, tt.devices)

			cfg := validConfig()
			cfg.AudioDeviceID = renderEndpointID
			setConfig(a, cfg)

			err := a.Start()
			if err == nil {
				_ = a.Stop()
				t.Fatal("Start() opened a Windows PLAYBACK endpoint as the commentary input")
			}
			for _, want := range []string{
				renderEndpointID,
				"PLAYBACK",
				gst.CaptureEndpointPrefix,
				gst.RenderEndpointPrefix,
				"Commentary input dropdown",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Start() error %q does not say %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "not present") {
				t.Errorf("Start() error %q reads as the vanished-device refusal; "+
					"the namespace check must fire first", err)
			}
			assertNoSession(t, a)
		})
	}
}

func TestStartProceedsWhenDeviceEnumerationFails(t *testing.T) {
	// The presence check is advisory. A device monitor that could veto Start
	// would be a NEW way to be off air — with the saved device possibly
	// sitting there working the whole time — so an enumeration failure is
	// logged and the start proceeds.
	a, _ := newTestApp(t)
	withStubDeviceError(t, errors.New("the wasapi2 device monitor did not start"))

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v; an enumeration failure must not block the feed", err)
	}
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestStartProceedsWhenDeviceEnumerationIsEmpty(t *testing.T) {
	// An empty list is a device-monitor hiccup until proven otherwise: zero
	// capture devices on a commentary machine is far less likely than the
	// monitor coming up empty, and refusing would strand a working device.
	a, _ := newTestApp(t)
	withStubDevices(t, []gst.Device{})

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v; an empty enumeration must not block the feed", err)
	}
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestStartAcceptsThePresentSavedDevice(t *testing.T) {
	// The check is not vacuous in either direction: validConfig's id is the
	// stub's first capture device, so a Start that refused here would be
	// refusing every correctly saved configuration.
	a, _ := newTestApp(t)

	devices, err := a.ListInputDevices()
	if err != nil {
		t.Fatalf("ListInputDevices() error = %v", err)
	}
	var present bool
	for _, d := range devices {
		if d.ID == validConfig().AudioDeviceID {
			present = true
		}
	}
	if !present {
		t.Fatal("validConfig's device id is no longer in the stub's default list; this test would prove nothing")
	}

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v; the saved device is present and must be accepted", err)
	}
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

// ---------------------------------------------------------------------------
// The VIDEO leg's pre-flight
// ---------------------------------------------------------------------------
//
// The same argument as the audio pre-flight above, on the leg where GStreamer is
// least helpful. An absent, busy or unresolvable DeckLink card gives
// "Internal data stream error / not-negotiated (-4)" in about 100 microseconds
// and names neither the device nor the cause, so every refusal below is checked
// for the FIELD an operator would go and change. And the two deliberate
// non-refusals are pinned too: a slate seat must not be made to consult the
// device monitor at all, and a named card must survive an enumeration hiccup.

// deckLinkStubDevice is a card as the device monitor reports one, in the shape
// decklinkdevices.go formats: a 16-digit hex persistent-id, and Kind decklink.
func deckLinkStubDevice(id, name string) gst.Device {
	return gst.Device{ID: id, Name: name, Kind: gst.KindDeckLink}
}

// nativeStubDevice is the machine's own audio input, carrying validConfig's
// saved id so that the audio half of the pre-flight passes and the video half is
// what each test is actually exercising.
func nativeStubDevice() gst.Device {
	return gst.Device{
		ID:   validConfig().AudioDeviceID,
		Name: "Microphone (Realtek High Definition Audio)",
		Kind: gst.KindNative,
	}
}

// TestPreflightSlateNeverConsultsTheDeviceMonitorForACard is the compatibility
// statement for every seat shipping today, expressed as a test: with the video
// source left alone, a device monitor that is failing outright cannot stop a
// match going out, and no card is resolved.
func TestPreflightSlateNeverConsultsTheDeviceMonitorForACard(t *testing.T) {
	a, _ := newTestApp(t)
	withStubDeviceError(t, errors.New("the device monitor is unavailable"))

	plan, err := a.preflightCapture(a.snapshotConfig())
	if err != nil {
		t.Fatalf("preflightCapture() error = %v; a slate seat must be unaffected by the device "+
			"monitor, which is every seat shipping today", err)
	}
	if plan.VideoCaptureID != "" {
		t.Errorf("preflightCapture() resolved %q for a slate seat; an empty id is what tells "+
			"internal/gst to build the slate leg", plan.VideoCaptureID)
	}
	if plan.AudioCaptureID != "" {
		t.Errorf("preflightCapture() resolved %q for the COMMENTARY on a microphone seat; an "+
			"empty id is what tells internal/gst to build the platform capture source",
			plan.AudioCaptureID)
	}
}

func TestPreflightRefusesACameraWithNoCardOnTheMachine(t *testing.T) {
	a, _ := newTestApp(t)
	withStubDevices(t, []gst.Device{nativeStubDevice()})

	cfg := a.snapshotConfig()
	cfg.VideoSource = config.VideoSourceDeckLink

	_, err := a.preflightCapture(cfg)
	if err == nil {
		t.Fatal("preflightCapture() accepted a camera on a machine with no Blackmagic card; the " +
			"pipeline would fail with not-negotiated (-4) naming nothing")
	}
	if !strings.Contains(err.Error(), "videoSource") {
		t.Errorf("the refusal does not name videoSource, which is the box to go and change: %v", err)
	}
}

func TestPreflightRefusesAPersistentIDThatNoLongerResolves(t *testing.T) {
	a, _ := newTestApp(t)
	const fitted = "0x0000000000AB12CD"
	withStubDevices(t, []gst.Device{
		nativeStubDevice(),
		deckLinkStubDevice(fitted, "Blackmagic UltraStudio 4K Mini"),
	})

	cfg := a.snapshotConfig()
	cfg.VideoSource = config.VideoSourceDeckLink
	cfg.DeckLinkPersistentID = "0x00000000DEADBEEF"

	_, err := a.preflightCapture(cfg)
	if err == nil {
		t.Fatal("preflightCapture() accepted an id no card claims")
	}
	// The field, the id that was asked for, and the card that IS there — the
	// operator's next move is to copy the third of those into the first.
	for _, want := range []string{"decklinkPersistentId", "0x00000000DEADBEEF", fitted} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// TestPreflightResolvesTheOnlyCard pins the translation this whole function
// exists for: decklinkPersistentId empty means "the card this machine has", and
// an empty VideoCaptureID means THE SLATE — the opposite instruction — so the
// emptiness must be resolved here and never forwarded.
func TestPreflightResolvesTheOnlyCard(t *testing.T) {
	a, _ := newTestApp(t)
	const fitted = "0x0000000000AB12CD"
	withStubDevices(t, []gst.Device{
		nativeStubDevice(),
		deckLinkStubDevice(fitted, "Blackmagic UltraStudio 4K Mini"),
	})

	cfg := a.snapshotConfig()
	cfg.VideoSource = config.VideoSourceDeckLink

	plan, err := a.preflightCapture(cfg)
	if err != nil {
		t.Fatalf("preflightCapture() error = %v; one card and no id named is the ordinary case at "+
			"a commentary position", err)
	}
	if plan.VideoCaptureID != fitted {
		t.Errorf("preflightCapture() video = %q, want %q — an empty result would build the slate "+
			"leg on a seat the operator has pointed at a camera", plan.VideoCaptureID, fitted)
	}
	if plan.AudioCaptureID != "" {
		t.Errorf("preflightCapture() audio = %q on a seat whose commentary is a MICROPHONE; the "+
			"two settings are independent and a card resolved for the picture must not move the "+
			"commentary onto it", plan.AudioCaptureID)
	}
}

// TestPreflightRefusesTwoCardsWithNothingNamingOne says why guessing is not on
// offer: the card is EXCLUSIVE, so picking whichever one enumerated first can
// take a card another application is holding, and the enumeration order is not
// an identity in the first place.
func TestPreflightRefusesTwoCardsWithNothingNamingOne(t *testing.T) {
	a, _ := newTestApp(t)
	withStubDevices(t, []gst.Device{
		nativeStubDevice(),
		deckLinkStubDevice("0x0000000000AB12CD", "Blackmagic UltraStudio 4K Mini"),
		deckLinkStubDevice("0x00000000001234EF", "Blackmagic DeckLink Duo 2"),
	})

	cfg := a.snapshotConfig()
	cfg.VideoSource = config.VideoSourceDeckLink

	_, err := a.preflightCapture(cfg)
	if err == nil {
		t.Fatal("preflightCapture() picked one of two cards; which one it picked would be the " +
			"driver's enumeration order, which is not an identity")
	}
	if !strings.Contains(err.Error(), "decklinkPersistentId") {
		t.Errorf("the refusal does not name decklinkPersistentId, which is the box that resolves "+
			"the ambiguity: %v", err)
	}
}

// TestPreflightCarriesOnWithANamedCardWhenEnumerationFails is the other half of
// the rule the audio pre-flight states: a device monitor hiccup must never
// become a new way to be unable to go on air. A card WAS named, so the
// enumeration is not the only source of truth and the element gets the
// operator's id.
func TestPreflightCarriesOnWithANamedCardWhenEnumerationFails(t *testing.T) {
	a, _ := newTestApp(t)
	withStubDeviceError(t, errors.New("the device monitor is unavailable"))

	cfg := a.snapshotConfig()
	cfg.VideoSource = config.VideoSourceDeckLink
	cfg.DeckLinkPersistentID = "0x0000000000AB12CD"

	plan, err := a.preflightCapture(cfg)
	if err != nil {
		t.Fatalf("preflightCapture() error = %v; the card was named, so a failing enumeration is "+
			"not the only thing that could have answered", err)
	}
	if plan.VideoCaptureID != "0x0000000000AB12CD" {
		t.Errorf("preflightCapture() = %q, want the configured id", plan.VideoCaptureID)
	}
}

// TestPreflightRefusesACameraWithNothingToNameItAndNoEnumeration is the one
// place the hiccup rule gives way, and it gives way because the alternative is
// worse than not starting: an empty result builds the SLATE, so carrying on
// would send a still picture to a switcher the operator believes is receiving
// their camera, with every lamp green.
func TestPreflightRefusesACameraWithNothingToNameItAndNoEnumeration(t *testing.T) {
	a, _ := newTestApp(t)
	withStubDeviceError(t, errors.New("the device monitor is unavailable"))

	cfg := a.snapshotConfig()
	cfg.VideoSource = config.VideoSourceDeckLink

	_, err := a.preflightCapture(cfg)
	if err == nil {
		t.Fatal("preflightCapture() carried on with no card and no way to find one; that would " +
			"transmit the slate from a seat configured for a camera")
	}
	if !strings.Contains(err.Error(), "decklinkPersistentId") {
		t.Errorf("the refusal does not name decklinkPersistentId: %v", err)
	}
}

// ---------------------------------------------------------------------------
// What a remote seat may not change
// ---------------------------------------------------------------------------

// TestARemoteSaveCannotChangeWhatGoesOnAir is the test that makes SetVideoSource
// being host-only mean anything. SaveConfig is remotely reachable and is a
// WHOLE-DOCUMENT write, so without this check the host-only classification would
// be a decoration a remote seat could walk straight round.
func TestARemoteSaveCannotChangeWhatGoesOnAir(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	const remoteSeat = "remote-1"

	// A remote save that restates the video leg exactly as it is passes: the
	// ordinary case, a port or a status key being fixed from another desk.
	unchanged := a.snapshotConfig()
	unchanged.StatusKey = "cam9"
	if err := a.saveConfigFrom(remoteSeat, unchanged); err != nil {
		t.Fatalf("a remote save that changes nothing about the video leg was refused: %v", err)
	}

	for _, tc := range []struct {
		name  string
		field string
		edit  func(*config.Config)
	}{
		{"the camera", "videoSource", func(c *config.Config) { c.VideoSource = config.VideoSourceDeckLink }},
		{"the preview", "decklinkPreviewEnabled", func(c *config.Config) { c.DeckLinkPreviewEnabled = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := a.snapshotConfig()
			tc.edit(c)
			err := a.saveConfigFrom(remoteSeat, c)
			if err == nil {
				t.Fatalf("a remote seat changed %s", tc.field)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("the refusal does not name %s: %v", tc.field, err)
			}
			// And nothing was written: a refusal that had already saved would be
			// the worst of both, an error message beside the change it describes.
			if live := a.snapshotConfig(); live.UsesDeckLinkVideo() || live.DeckLinkPreviewEnabled {
				t.Errorf("the refused save reached the live configuration: %+v", live)
			}

			// The SAME edit from the local seat is accepted, which is what makes
			// this a restriction on the caller rather than on the value.
			local := a.snapshotConfig()
			tc.edit(local)
			if err := a.saveConfigFrom(localClientID, local); err != nil {
				t.Fatalf("the local seat was refused: %v", err)
			}
			// Put it back for the next subtest.
			if err := a.saveConfigFrom(localClientID, validConfig()); err != nil {
				t.Fatalf("restoring the configuration: %v", err)
			}
		})
	}
}

// TestSetVideoSourceRefusesWhileSending pins the other refusal: the video leg is
// built at START and cannot be exchanged under a running feed, so accepting the
// change would be a control that appears to switch a camera on air and silently
// does nothing until the next restart.
func TestSetVideoSourceRefusesWhileSending(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = a.Stop() }()

	if err := a.SetVideoSource(config.VideoSourceDeckLink); err == nil {
		t.Fatal("SetVideoSource was accepted while sending; nothing would have changed until the " +
			"next START and the operator would not have been told")
	}
	if err := a.SetDeckLinkPreviewEnabled(true); err == nil {
		t.Fatal("SetDeckLinkPreviewEnabled was accepted while sending; the preview is a branch of " +
			"the running pipeline and cannot be attached to it")
	}
	if a.snapshotConfig().UsesDeckLinkVideo() {
		t.Error("the refused SetVideoSource wrote the configuration anyway")
	}
}

// TestSetVideoSourceRefusesAValueWithNoLegBehindIt keeps the method's message
// and config.Validate's agreeing: an operator may well meet both about the same
// typed value.
func TestSetVideoSourceRefusesAValueWithNoLegBehindIt(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	err := a.SetVideoSource("ndi")
	if err == nil {
		t.Fatal("SetVideoSource(\"ndi\") was accepted; there is no leg behind it")
	}
	for _, want := range []string{"videoSource", "ndi"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

func TestStartForwardsSenderStates(t *testing.T) {
	a, _ := newTestApp(t)

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = a.Stop() }()

	waitFor(t, 5*time.Second, "CONNECTED to reach the frontend queue", func() bool {
		a.senderMu.Lock()
		defer a.senderMu.Unlock()
		return a.lastSender == sender.StateConnected
	})

	var sawConnected bool
	for _, e := range drainPump(a) {
		if e.name == EventSender && e.data == sender.StateConnected {
			sawConnected = true
		}
	}
	if !sawConnected {
		t.Fatal("no CONNECTED state was queued for the frontend")
	}
}

// TestRepeatedStartStopCyclesLeakNothing is the mid-match device change: the
// operator picks a different DVS input, which is Stop then Start, and is the one
// path that forces a pipeline rebuild (specification section 6.1).
//
// Each cycle creates a fresh gst.Pipeline and a fresh sender with two goroutines
// of its own. If Stop did not join them, this would accumulate goroutines and
// eventually leave a pipeline holding the WASAPI endpoint.
func TestRepeatedStartStopCyclesLeakNothing(t *testing.T) {
	a, _ := newTestApp(t)

	const cycles = 25
	var pipes []*gst.StubPipeline

	for i := 0; i < cycles; i++ {
		if err := a.Start(); err != nil {
			t.Fatalf("Start() on cycle %d error = %v", i, err)
		}

		a.sessMu.Lock()
		stub, ok := a.session.pipe.(*gst.StubPipeline)
		a.sessMu.Unlock()
		if !ok {
			t.Fatalf("cycle %d: pipeline is not the Gate A stub", i)
		}
		pipes = append(pipes, stub)

		if err := a.Stop(); err != nil {
			t.Fatalf("Stop() on cycle %d error = %v", i, err)
		}
	}

	// Every pipeline ever created must be at NULL. A single one left running
	// holds the capture device open for the rest of the process.
	for i, p := range pipes {
		if got := p.State(); got != gst.StubStateStopped {
			t.Fatalf("pipeline from cycle %d is %v, want %v", i, got, gst.StubStateStopped)
		}
	}

	// Distinct pipelines, not one reused: a gst.Pipeline is single-use.
	seen := map[*gst.StubPipeline]bool{}
	for _, p := range pipes {
		if seen[p] {
			t.Fatal("a pipeline was reused across a Stop/Start cycle; gst.Pipeline is single-use")
		}
		seen[p] = true
	}
}

// ---------------------------------------------------------------------------
// The self-stop reaper
// ---------------------------------------------------------------------------

// fakeSelfStoppingSender is a sender.Sender the test can stop from the
// SENDER'S side, modelling the contract the real sender honours on
// gst.ErrPipelineFatal: it emits sender.StateStopped and closes its states
// channel without anyone having called App.Stop. That close is the reaper's
// trigger, so a fake that performs exactly it — and nothing else — is what
// isolates the reaper from the reconnect choreography the real sender would
// need to reach the same place.
//
// Stop performs the same sequence, once, whoever asks first: the real sender's
// Stop is idempotent in effect, and the race test below relies on a self-stop
// and an operator Stop landing together being indistinguishable from either
// alone.
type fakeSelfStoppingSender struct {
	states chan sender.State
	once   sync.Once
}

func newFakeSelfStoppingSender() *fakeSelfStoppingSender {
	return &fakeSelfStoppingSender{states: make(chan sender.State, 8)}
}

func (f *fakeSelfStoppingSender) Start(sender.Opts) error { return nil }

func (f *fakeSelfStoppingSender) Stop() error {
	f.selfStop()
	return nil
}

func (f *fakeSelfStoppingSender) States() <-chan sender.State { return f.states }

// selfStop is the sender deciding on its own that the session is over: emit
// StateStopped, close the channel. Safe to race with Stop; only one wins.
func (f *fakeSelfStoppingSender) selfStop() {
	f.once.Do(func() {
		f.states <- sender.StateStopped
		close(f.states)
	})
}

var _ sender.Sender = (*fakeSelfStoppingSender)(nil)

// withFakeSender installs the senderDial seam and returns a function that
// yields the most recently dialled fake.
func withFakeSender(a *App) (latest func() *fakeSelfStoppingSender) {
	var mu sync.Mutex
	var current *fakeSelfStoppingSender
	a.senderDial = func(gst.Pipeline) sender.Sender {
		f := newFakeSelfStoppingSender()
		mu.Lock()
		current = f
		mu.Unlock()
		return f
	}
	return func() *fakeSelfStoppingSender {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
}

func TestASelfStoppedSessionIsReapedSoStartWorksAgain(t *testing.T) {
	// The defect this pins: a.session used to be cleared only by App.Stop, so
	// a sender that stopped itself — gst.ErrPipelineFatal, the capture chain
	// dead — left the pointer in place and every later Start returned
	// errAlreadySending. The operator saw a grey SENDING lamp, a button
	// reading START, and a START that refused because a session that had
	// already died was still on the books; the only way out was restarting
	// the application.
	a, _ := newTestApp(t)
	latest := withFakeSender(a)

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	latest().selfStop()

	waitFor(t, 5*time.Second, "the reaper to clear the self-stopped session", func() bool {
		a.sessMu.Lock()
		defer a.sessMu.Unlock()
		return a.session == nil
	})

	// And the point of the reaping: the next START is accepted.
	if err := a.Start(); err != nil {
		t.Fatalf("Start() after a self-stop error = %v; the dead session was never reaped", err)
	}
	if err := a.Stop(); err != nil && !errors.Is(err, errNotSending) {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestSelfStopRacingAnOperatorStopNeverDeadlocks(t *testing.T) {
	// The reaper is deliberately NOT tracked by sess.wg: App.Stop holds sessMu
	// across sess.wg.Wait(), so a reaper counted in that WaitGroup — parked on
	// the sessMu that Stop holds — would deadlock every Stop that raced a
	// self-stop. This drives the two together repeatedly, under -race in the
	// Gate A suite, and accepts either documented outcome per cycle: Stop won
	// the session, or the reaper did and Stop returned errNotSending — the
	// button had already flipped back to START either way.
	a, _ := newTestApp(t)
	latest := withFakeSender(a)

	for i := 0; i < 50; i++ {
		if err := a.Start(); err != nil {
			t.Fatalf("Start() on cycle %d error = %v", i, err)
		}
		f := latest()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			f.selfStop()
		}()
		go func() {
			defer wg.Done()
			if err := a.Stop(); err != nil && !errors.Is(err, errNotSending) {
				t.Errorf("Stop() racing a self-stop error = %v; want nil or errNotSending", err)
			}
		}()
		wg.Wait()

		// Whichever won, the session must be gone before the next cycle — by
		// Stop's own clearing or by the reaper's.
		waitFor(t, 5*time.Second, "the session to be cleared after the race", func() bool {
			a.sessMu.Lock()
			defer a.sessMu.Unlock()
			return a.session == nil
		})
	}
}

// ---------------------------------------------------------------------------
// Shutdown
// ---------------------------------------------------------------------------

func TestTeardownStopsALiveSession(t *testing.T) {
	a, _ := newTestApp(t)

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	a.sessMu.Lock()
	stub := a.session.pipe.(*gst.StubPipeline)
	a.sessMu.Unlock()

	a.teardown()

	if got := stub.State(); got != gst.StubStateStopped {
		t.Fatalf("pipeline state after teardown = %v, want %v; the WASAPI endpoint was never released", got, gst.StubStateStopped)
	}
}

func TestTeardownIsIdempotent(t *testing.T) {
	// It is reachable from Wails' OnShutdown and from main's error path, and
	// exactly one of them must do the work.
	a, _ := newTestApp(t)

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	a.sessMu.Lock()
	stub := a.session.pipe.(*gst.StubPipeline)
	a.sessMu.Unlock()

	for i := 0; i < 5; i++ {
		a.teardown()
	}

	if got := stub.Counters().Stops; got == 0 {
		t.Fatal("teardown never stopped the pipeline")
	}
}

func TestTeardownCompletesPromptly(t *testing.T) {
	// A process that will not exit is a support call. With the Gate A stub every
	// step is immediate, so anything approaching shutdownTimeout means the
	// ordering has deadlocked rather than merely being slow.
	a, _ := newTestApp(t)

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	start := time.Now()
	a.teardown()
	elapsed := time.Since(start)

	if elapsed >= shutdownTimeout {
		t.Fatalf("teardown took %v, reaching the %v bound; the ordered shutdown is wedged", elapsed, shutdownTimeout)
	}
}

// wedgedReturnMonitor is a gst.ReturnMonitor whose Stop never returns.
//
// That is not a contrived fake. internal/gst's returnMonitor.Stop waits on the
// run goroutine, which can be inside returnPipeline.Play, which holds the
// pipeline lock across gst_element_set_state — a cgo call that takes no timeout
// and cannot be interrupted from Go. internal/gst says so beside its own
// timeout constants. A monitor that will not stop is the measured worst case,
// not a hypothesis.
type wedgedReturnMonitor struct {
	states chan gst.ReturnState
	block  chan struct{}
}

func newWedgedReturnMonitor() *wedgedReturnMonitor {
	return &wedgedReturnMonitor{
		states: make(chan gst.ReturnState, 8),
		block:  make(chan struct{}),
	}
}

func (m *wedgedReturnMonitor) Start(gst.ReturnOpts) error     { return nil }
func (m *wedgedReturnMonitor) States() <-chan gst.ReturnState { return m.states }
func (m *wedgedReturnMonitor) Stop() error                    { <-m.block; return nil }

// release lets the abandoned Stop finish, so the test does not leave a
// goroutine parked for the rest of the run.
func (m *wedgedReturnMonitor) release() { close(m.block); close(m.states) }

var _ gst.ReturnMonitor = (*wedgedReturnMonitor)(nil)

// TestTeardownAbandonsAWedgedStepAndStillRunsTheRest is the operator's bug:
// "when I close the app, it doesn't close properly, the window closes, but in
// task manager I have to kill it."
//
// The teardown used to be a plain sequence inside one overall timeout, so the
// FIRST step that would not return consumed the whole budget and every step
// behind it was never run at all. Measured before the fix, with this same
// wedged monitor: teardown took the full twenty seconds and, when it gave up,
// the mixer's write socket was still open and still armed, the status watcher
// and the token-refresh timer were still running, and the root context had
// never been cancelled — so every GetMixerSnapshot dial the drawer had in
// flight was still live too. A shutdown that had already given up had released
// nothing.
//
// This test fails on every one of those counts without the per-step bounds: on
// the deadline first, and then on each thing that was never stopped.
func TestTeardownAbandonsAWedgedStepAndStillRunsTheRest(t *testing.T) {
	a, _ := newTestApp(t)
	setConfig(a, srtReturnConfig())
	silencePump(a)

	_, ctl := withFakeMixer(t, a)
	if _, err := a.ArmMixer(); err != nil {
		t.Fatalf("ArmMixer() error = %v", err)
	}

	mon := newWedgedReturnMonitor()
	a.returnDial = func() gst.ReturnMonitor { return mon }
	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	t.Cleanup(mon.release)

	// The deadline is the return step's own budget plus the four prompt steps
	// behind it, with room to spare — and far below shutdownTimeout, which is
	// what the unfixed code took.
	const deadline = returnStopBudget + 5*time.Second

	exited := make(chan struct{}, 1)
	a.exitProcess = func() {
		select {
		case exited <- struct{}{}:
		default:
		}
	}

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		a.teardown()
		done <- time.Since(start)
	}()

	select {
	case elapsed := <-done:
		t.Logf("teardown returned after %v", elapsed)
	case <-time.After(deadline):
		t.Fatalf("teardown did not return within %v with one step wedged; the window has gone and "+
			"the process is still here, which is the fault being fixed", deadline)
	}

	// Everything BEHIND the wedged step must still have happened. These are the
	// four things the unfixed teardown left running.
	if ctl.closeCount() == 0 {
		t.Error("the mixer write path was never closed: a switcher_controller socket that can " +
			"change a live clean feed outlived the window because the return monitor would not stop")
	}
	if a.rootCtx.Err() == nil {
		t.Error("the root context was never cancelled: every in-flight GetMixerSnapshot dial, the " +
			"KVS fetch and the event pump were all still running after the shutdown gave up")
	}
	a.ctlMu.Lock()
	stillUp := a.ctlCancel != nil
	a.ctlMu.Unlock()
	if stillUp {
		t.Error("the control plane was never stopped: the status socket and the m2lx token-refresh " +
			"goroutine outlived the window")
	}

	select {
	case <-exited:
	default:
		t.Error("teardown abandoned a step and then returned normally. A step was abandoned because " +
			"it is inside a cgo call nothing here can reach; handing that thread to the ordinary " +
			"exit path is what leaves a process in Task Manager. It must end the process itself")
	}
}

// TestTeardownDoesNotEndTheProcessWhenNothingWasAbandoned keeps the hard exit
// rare. It is the last resort for a thread that cannot be accounted for, and a
// shutdown that stopped everything it was asked to must leave by the ordinary
// door: Wails still has an error to return to main, and main still has an exit
// status to set from it.
func TestTeardownDoesNotEndTheProcessWhenNothingWasAbandoned(t *testing.T) {
	a, _ := newTestApp(t)
	setConfig(a, srtReturnConfig())
	silencePump(a)

	_, ctl := withFakeMixer(t, a)
	if _, err := a.ArmMixer(); err != nil {
		t.Fatalf("ArmMixer() error = %v", err)
	}
	withFakeReturn(a)
	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	var exits atomic.Int64
	a.exitProcess = func() { exits.Add(1) }

	a.teardown()

	if got := exits.Load(); got != 0 {
		t.Errorf("teardown ended the process %d time(s) on a clean shutdown; the hard exit is for a "+
			"step that could not be stopped, not for every close", got)
	}
	if ctl.closeCount() == 0 {
		t.Error("the mixer write path was not closed by a clean teardown")
	}
}

// TestTeardownStepBudgetsFitInsideTheOverallBound checks the arithmetic the
// shutdownTimeout comment now claims.
//
// shutdownTimeout stopped being the thing that bounds the work and became the
// backstop for these six adding up. If they ever exceed it the backstop fires
// first, teardown is cut off mid-sequence again, and the steps behind the cut
// stop running — which is exactly the defect the per-step budgets removed.
//
// pictureStopBudget is the sixth. It was added with the SRT picture, and adding
// it is what took shutdownTimeout from twenty seconds to twenty-four: the sum
// is the bound, so a new step is a new term on both sides or this test fails,
// which is the whole point of it being written as a sum rather than a constant.
func TestTeardownStepBudgetsFitInsideTheOverallBound(t *testing.T) {
	total := senderStopBudget + returnStopBudget + pictureStopBudget +
		mixerCloseBudget + controlPlaneStopBudget + rootJoinBudget
	if total > shutdownTimeout {
		t.Fatalf("the per-step budgets total %v, over shutdownTimeout's %v: the overall bound would "+
			"cut the ordered teardown short and the last steps would stop running again", total, shutdownTimeout)
	}

	// And the sender keeps the largest share. It is the contribution feed; it
	// goes first and it is the one path given every chance to finish.
	for name, budget := range map[string]time.Duration{
		"returnStopBudget":       returnStopBudget,
		"pictureStopBudget":      pictureStopBudget,
		"mixerCloseBudget":       mixerCloseBudget,
		"controlPlaneStopBudget": controlPlaneStopBudget,
		"rootJoinBudget":         rootJoinBudget,
	} {
		if budget > senderStopBudget {
			t.Errorf("%s (%v) is larger than senderStopBudget (%v); the contribution feed is the "+
				"path that must be given every chance to finish", name, budget, senderStopBudget)
		}
	}
}

// TestTeardownCancelsTheRootContextBeforeAnythingThatCanBlock pins the ordering
// that makes the abandonment safe.
//
// rootCancel cannot block, and it is the only step that releases every
// context-bound piece of work at once. Behind a step that CAN block it is a
// cancellation that never happens, which is how the drawer's in-flight status
// dials survived a shutdown. Being first costs nothing: neither the sender nor
// the return monitor is driven by that context.
func TestTeardownCancelsTheRootContextBeforeAnythingThatCanBlock(t *testing.T) {
	a, _ := newTestApp(t)
	setConfig(a, srtReturnConfig())
	silencePump(a)

	cancelledFirst := make(chan bool, 1)
	mon := newWedgedReturnMonitor()
	a.returnDial = func() gst.ReturnMonitor { return mon }
	if err := a.StartReturn(); err != nil {
		t.Fatalf("StartReturn() error = %v", err)
	}
	t.Cleanup(mon.release)

	// Watched from the wedged step itself: by the time anything that can block
	// is running, the root context must already be done.
	go func() {
		<-a.rootCtx.Done()
		cancelledFirst <- true
	}()

	a.exitProcess = func() {}
	a.teardown()

	select {
	case <-cancelledFirst:
	case <-time.After(time.Second):
		t.Fatal("the root context was not cancelled by a teardown whose media step was wedged; " +
			"rootCancel is behind a step that can block again")
	}
}

func TestStartAfterTeardownIsRefused(t *testing.T) {
	a, _ := newTestApp(t)
	a.teardown()

	if err := a.Start(); !errors.Is(err, errShuttingDown) {
		t.Fatalf("Start() after teardown error = %v, want errShuttingDown", err)
	}

	a.sessMu.Lock()
	sess := a.session
	a.sessMu.Unlock()
	if sess != nil {
		t.Fatal("a session was built after teardown; the process would exit still holding the capture device")
	}
}

func TestStartControlPlaneAfterTeardownBuildsNothing(t *testing.T) {
	a, _ := newTestApp(t)
	a.teardown()

	a.startControlPlane()

	a.ctlMu.Lock()
	client, cancel, wg := a.client, a.ctlCancel, a.ctlWG
	a.ctlMu.Unlock()

	if client != nil || cancel != nil || wg != nil {
		t.Fatal("a control plane generation was built after teardown; its goroutines would never be joined")
	}
}

// TestStopControlPlaneClosesTheClient is the regression test for a leak that
// cancelling the context does not cover.
//
// The m2lx client's token-refresh goroutine is bounded by a context of its own —
// deliberately, so that a short-lived sign-in context cannot kill hours of
// refreshes — so the generation's context going away leaves it running. Only
// Close stops it, and startControlPlane runs again on every change to m2lxHost,
// alias, statusKey or the stored password.
func TestStopControlPlaneClosesTheClient(t *testing.T) {
	a, _ := newTestApp(t)
	spy := &closeSpyClient{}

	_, cancel := context.WithCancel(context.Background())
	a.ctlMu.Lock()
	a.client = spy
	a.ctlCancel = cancel
	a.ctlWG = &sync.WaitGroup{}
	a.stopControlPlaneLocked()
	client, ctlCancel := a.client, a.ctlCancel
	a.ctlMu.Unlock()

	if got := spy.closeCount(); got != 1 {
		t.Fatalf("Close was called %d times, want exactly once; "+
			"the token refresh goroutine outlives every control plane generation", got)
	}
	if client != nil || ctlCancel != nil {
		t.Fatal("the generation was not cleared after being stopped")
	}
}

// TestStopControlPlaneSurvivesACloseFailure keeps a failing Close from wedging
// shutdown: the generation is going away regardless, so the error is logged and
// the unwind continues.
func TestStopControlPlaneSurvivesACloseFailure(t *testing.T) {
	a, _ := newTestApp(t)
	spy := &closeSpyClient{err: errors.New("the refresh timer would not stop")}

	_, cancel := context.WithCancel(context.Background())
	a.ctlMu.Lock()
	a.client = spy
	a.ctlCancel = cancel
	a.ctlWG = &sync.WaitGroup{}
	a.stopControlPlaneLocked()
	client := a.client
	a.ctlMu.Unlock()

	if spy.closeCount() != 1 {
		t.Fatalf("Close was called %d times, want exactly once", spy.closeCount())
	}
	if client != nil {
		t.Fatal("a client that failed to close was left in place")
	}
}

// TestShutdownRacesAreDecidedDeterministically hammers the bound surface from
// several goroutines while teardown runs, which is the interleaving Wails
// actually produces: OnShutdown fires on the main thread while message-handler
// goroutines may still be inside a bound method.
//
// The invariant is not that any particular call wins. It is that when the dust
// settles no session survives teardown, because the closing flag is raised
// before anything is stopped: a Start that got sessMu first completes and is
// then stopped, and a Start that arrives later is refused.
//
// Run this with -count=20 or higher. The race detector is unavailable at Gate A
// (it needs cgo and a C compiler on Windows), so repetition and deliberate
// contention are the substitute.
func TestShutdownRacesAreDecidedDeterministically(t *testing.T) {
	a, _ := newTestApp(t)

	const callers = 8
	var wg sync.WaitGroup
	begin := make(chan struct{})

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-begin
			switch id % 4 {
			case 0:
				if err := a.Start(); err == nil {
					_ = a.Stop()
				}
			case 1:
				_ = a.Stop()
			case 2:
				cfg := validConfig()
				cfg.Alias = fmt.Sprintf("alias-%d", id)
				_ = a.SaveConfig(cfg)
			case 3:
				_, _ = a.GetConfig()
				_, _ = a.ListInputDevices()
			}
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-begin
		a.teardown()
	}()

	close(begin)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a bound method or teardown deadlocked under contention")
	}

	// Whatever order they ran in, teardown must have won.
	a.sessMu.Lock()
	sess := a.session
	a.sessMu.Unlock()
	if sess != nil {
		t.Fatal("a session survived teardown")
	}

	a.ctlMu.Lock()
	client := a.client
	a.ctlMu.Unlock()
	if client != nil {
		t.Fatal("a control plane survived teardown")
	}
}

// TestConcurrentControlPlaneRestarts drives the path SaveConfig and SetSecret
// take on first run, from several goroutines at once. Each restart tears down
// the previous generation and joins its goroutines under ctlMu, so a mistake
// here is a deadlock rather than a wrong answer.
func TestConcurrentControlPlaneRestarts(t *testing.T) {
	a, _ := newTestApp(t)

	const callers = 6
	var wg sync.WaitGroup
	begin := make(chan struct{})

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-begin
			for j := 0; j < 5; j++ {
				cfg := validConfig()
				// Vary a control-plane field so every save forces a restart.
				cfg.StatusKey = fmt.Sprintf("cam%d", (id+j)%8)
				_ = a.SaveConfig(cfg)
			}
		}(i)
	}

	close(begin)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("concurrent control plane restarts deadlocked")
	}
}

// ---------------------------------------------------------------------------
// Connection failures reaching the operator
// ---------------------------------------------------------------------------

// testClock is a hand-wound clock for connectErrorReporter, so that crossing
// connectErrorRepeat costs no wall-clock time.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newTestClock() *testClock {
	// An arbitrary fixed instant. It must not be the zero Time: the reporter
	// uses a zero lastAt to mean "nothing has been shown yet".
	return &testClock{at: time.Date(2026, 5, 16, 15, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// errorRecorder collects what a connectErrorReporter published.
type errorRecorder struct {
	mu   sync.Mutex
	sent []string
}

func (r *errorRecorder) emit(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, err.Error())
}

func (r *errorRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sent...)
}

// newTestReporter returns a reporter on a hand-wound clock, aimed at the SRT
// coordinates validConfig uses.
func newTestReporter() (*connectErrorReporter, *errorRecorder, *testClock) {
	rec := &errorRecorder{}
	clk := newTestClock()
	return newConnectErrorReporter(rec.emit, "127.0.0.1", 4001, clk.now), rec, clk
}

func TestConnectErrorReporterShowsTheFirstFailureAtOnce(t *testing.T) {
	// The whole point: the operator must be told why the SENDING lamp is amber,
	// and told the first time it happens rather than after a delay.
	r, rec, _ := newTestReporter()

	r.report(errors.New("gst: wasapi2src: the audio endpoint has gone away"))

	sent := rec.all()
	if len(sent) != 1 {
		t.Fatalf("published %d errors, want exactly the first failure: %q", len(sent), sent)
	}
	for _, want := range []string{"127.0.0.1", "4001", "the audio endpoint has gone away"} {
		if !strings.Contains(sent[0], want) {
			t.Fatalf("published %q, which does not mention %q", sent[0], want)
		}
	}
}

func TestConnectErrorReporterSuppressesIdenticalRepeats(t *testing.T) {
	// A permanently fatal pipeline fails every sender.BackoffCap — thirty
	// seconds — for the rest of the match. Forty of these is twenty minutes of a
	// second half. The operator must get one message, not forty.
	r, rec, clk := newTestReporter()

	err := errors.New("gst: replace sink: srtsink: connection setup failure: connection refused")
	for i := 0; i < 40; i++ {
		r.report(err)
		clk.advance(sender.BackoffCap)
	}

	sent := rec.all()
	// Twenty minutes at the thirty second cap crosses connectErrorRepeat three
	// times, so: the first message plus three repeats.
	const wantMax = 5
	if len(sent) == 0 {
		t.Fatal("published nothing across forty failed attempts; the operator would see only an amber lamp")
	}
	if len(sent) > wantMax {
		t.Fatalf("published %d messages for forty identical failures, want at most %d — "+
			"the error pane would be a wall of the same line:\n%q", len(sent), wantMax, sent)
	}
}

func TestConnectErrorReporterRepeatsAfterTheInterval(t *testing.T) {
	// Suppression must not become silence: a fault that is still there after
	// connectErrorRepeat is said again, and the repeat says how many attempts
	// were swallowed so "still broken" reads differently from "broken again".
	r, rec, clk := newTestReporter()
	err := errors.New("gst: replace sink: connection timed out")

	r.report(err)
	// Two more inside the window, which must be swallowed.
	clk.advance(connectErrorRepeat / 3)
	r.report(err)
	clk.advance(connectErrorRepeat / 3)
	r.report(err)

	if got := len(rec.all()); got != 1 {
		t.Fatalf("published %d messages inside the repeat window, want 1: %q", got, rec.all())
	}

	clk.advance(connectErrorRepeat)
	r.report(err)

	sent := rec.all()
	if len(sent) != 2 {
		t.Fatalf("published %d messages, want the first plus one repeat: %q", len(sent), sent)
	}
	if !strings.Contains(sent[1], "still not connected") {
		t.Fatalf("the repeat %q does not read as a continuation", sent[1])
	}
	if !strings.Contains(sent[1], "2 further attempts") {
		t.Fatalf("the repeat %q does not say how many attempts were suppressed", sent[1])
	}
}

func TestConnectErrorReporterShowsAChangedReasonImmediately(t *testing.T) {
	// A different reason is new information and must not wait out the repeat
	// window: "the endpoint is gone" becoming "connection refused" means the
	// device came back and the far end is now the problem.
	r, rec, clk := newTestReporter()

	r.report(errors.New("gst: wasapi2src: the audio endpoint has gone away"))
	clk.advance(time.Second)
	r.report(errors.New("gst: replace sink: connection refused"))

	sent := rec.all()
	if len(sent) != 2 {
		t.Fatalf("published %d messages, want both distinct reasons: %q", len(sent), sent)
	}
	if !strings.Contains(sent[1], "connection refused") {
		t.Fatalf("the second message %q does not carry the changed reason", sent[1])
	}
}

func TestConnectErrorReporterTreatsAPipelineFatalAsTerminal(t *testing.T) {
	// gst.ErrPipelineFatal is the sender's last word — it reports once, emits
	// StateStopped and closes its states channel — so the reporter must treat
	// it as a terminal announcement, not one more failed attempt: say the
	// session has STOPPED, blame the input device and not the network, and
	// never let the repeat filter swallow it. The pre-sentinel behaviour was
	// the measured misdiagnosis: a playback endpoint in config.json reported
	// forever as "the commentary feed to <host>:40005 is not connected and is
	// retrying".
	r, rec, _ := newTestReporter()

	fatal := fmt.Errorf("%w: wasapi2src rbuf: Failed to open device %s "+
		"(the capture or mux chain has failed; recover with Stop, New, Start)",
		gst.ErrPipelineFatal, "{0.0.0.00000000}.{8678ce58-7b71-4bd4-810f-1c4a7f11ec71}")

	r.report(fatal)

	sent := rec.all()
	if len(sent) != 1 {
		t.Fatalf("published %d messages for a pipeline-fatal error, want 1: %q", len(sent), sent)
	}
	for _, want := range []string{
		"STOPPED",
		"input",
		"device",
		"START",
		// The underlying reason, device id included, must survive the wrap:
		// it is the only thing that names WHICH device failed.
		"{0.0.0.00000000}.{8678ce58-7b71-4bd4-810f-1c4a7f11ec71}",
	} {
		if !strings.Contains(sent[0], want) {
			t.Errorf("the terminal message %q does not say %q", sent[0], want)
		}
	}
	// It must NOT be framed as a network failure: no SRT host, no port, no
	// "not connected and is retrying".
	for _, banned := range []string{"127.0.0.1", "4001", "not connected"} {
		if strings.Contains(sent[0], banned) {
			t.Errorf("the terminal message %q names %q; a capture-chain death is not a network fault",
				sent[0], banned)
		}
	}

	// The repeat filter must not apply: an identical fatal a moment later is
	// still published, with no clock movement at all.
	r.report(fatal)
	if got := len(rec.all()); got != 2 {
		t.Fatalf("published %d messages after a repeated fatal, want 2; "+
			"the connectErrorRepeat suppression swallowed a terminal message", got)
	}

	// And it leaves the ordinary bookkeeping alone: a non-fatal failure shown
	// before the fatal is still suppressed as a repeat after it.
	ordinary := errors.New("gst: replace sink: connection refused")
	r.report(ordinary)
	r.report(ordinary)
	if got := len(rec.all()); got != 3 {
		t.Fatalf("published %d messages, want 3 — the fatal branch must not touch the repeat "+
			"bookkeeping either way: %q", got, rec.all())
	}
}

func TestConnectErrorReporterHandlesAnEmptyMessage(t *testing.T) {
	// An error whose message is empty would otherwise compare equal to the
	// "nothing has been shown yet" sentinel and be suppressed forever.
	r, rec, _ := newTestReporter()

	r.report(errors.New(""))

	sent := rec.all()
	if len(sent) != 1 {
		t.Fatalf("published %d messages for an empty error, want 1: %q", len(sent), sent)
	}
	if strings.HasSuffix(sent[0], ": ") {
		t.Fatalf("published %q, which trails off with no reason", sent[0])
	}
}

func TestConnectErrorReporterIgnoresNil(t *testing.T) {
	// A nil error carries no reason, which is the one thing this exists to
	// deliver. Announcing it would be a blank line in the error pane.
	r, rec, _ := newTestReporter()

	r.report(nil)

	if got := rec.all(); len(got) != 0 {
		t.Fatalf("published %q for a nil error", got)
	}
}

func TestSenderOptsCarriesAConnectErrorReporterToTheErrorEvent(t *testing.T) {
	// sender.Opts.OnConnectError is the only route by which the reason a
	// connection attempt failed leaves internal/sender. If Start does not set it,
	// the reason is discarded and the operator has an amber lamp and nothing
	// else for the rest of the match.
	a, _ := newTestApp(t)
	silencePump(a)

	cfg := validConfig()
	opts := a.senderOpts(cfg, "a-passphrase")

	if opts.OnConnectError == nil {
		t.Fatal("senderOpts left OnConnectError nil; every connection failure would be discarded")
	}

	// The sink is still built from the configuration and the credential store.
	if opts.Sink.Host != cfg.EffectiveSRTHost() || opts.Sink.Port != cfg.SRTPort {
		t.Fatalf("sink = %s:%d, want %s:%d", opts.Sink.Host, opts.Sink.Port, cfg.EffectiveSRTHost(), cfg.SRTPort)
	}
	if opts.Sink.Passphrase != "a-passphrase" {
		t.Fatalf("sink passphrase = %q, want the one from the credential store", opts.Sink.Passphrase)
	}
	if opts.Pipeline.AudioDeviceID != cfg.AudioDeviceID {
		t.Fatalf("pipeline device = %q, want the configured endpoint GUID", opts.Pipeline.AudioDeviceID)
	}

	opts.OnConnectError(errors.New("gst: replace sink: connection refused"))

	queued := drainPump(a)
	if len(queued) != 1 {
		t.Fatalf("a connection failure queued %d events, want exactly one: %+v", len(queued), queued)
	}
	if queued[0].name != EventError {
		t.Fatalf("a connection failure was queued as %q, want %q", queued[0].name, EventError)
	}
	msg, ok := queued[0].data.(string)
	if !ok {
		t.Fatalf("the %q event carried %T, want a string", EventError, queued[0].data)
	}
	if !strings.Contains(msg, "connection refused") {
		t.Fatalf("the operator was told %q, which does not carry the reason", msg)
	}
	// A secret must never cross the Wails boundary outbound, and the passphrase
	// is in the same Opts this callback was built from.
	if strings.Contains(msg, "a-passphrase") {
		t.Fatalf("the %q event %q leaks the SRT passphrase", EventError, msg)
	}
}

func TestSenderOptsGivesEachSessionItsOwnReporter(t *testing.T) {
	// The memory of what has already been said must die with the session it was
	// said about: an operator who presses STOP and then START has asked to be
	// told again.
	a, _ := newTestApp(t)
	silencePump(a)

	cfg := validConfig()
	err := errors.New("gst: replace sink: connection refused")

	if a.senderOpts(cfg, "").OnConnectError == nil {
		t.Fatal("senderOpts left OnConnectError nil; every connection failure would be discarded")
	}

	a.senderOpts(cfg, "").OnConnectError(err)
	first := drainPump(a)

	a.senderOpts(cfg, "").OnConnectError(err)
	second := drainPump(a)

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("session one queued %d and session two queued %d, want one each: %+v %+v",
			len(first), len(second), first, second)
	}
}

// ---------------------------------------------------------------------------
// Gate A end-to-end against cmd/mockm2lx
// ---------------------------------------------------------------------------

// mockEnv reads one of the WSLCOMMS_MOCK_* settings, falling back to
// cmd/mockm2lx's own documented default.
func mockEnv(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// TestControlPlaneAgainstMockM2LX is the end-to-end check WP-8 exists to make:
// the real control plane — App.startControlPlane, the sign-in loop, the m2lx
// client and the status Watcher — against the real cmd/mockm2lx, over a real
// socket, with the events arriving on the real event pump.
//
// It could not be written until two things landed. The root package is now
// inside the Gate A build (its constraint no longer requires cgo), and
// internal/m2lx now accepts an explicit "http://" prefix, so the app can reach a
// mock that serves plain HTTP. Before that this test recorded the scheme
// mismatch as a failure; it now records the fix.
//
// WSLCOMMS_MOCK_ADDR must carry the scheme: "http://127.0.0.1:18081". Without
// it internal/m2lx resolves the host to https/wss — which is the correct
// default and must not be changed — and the mock will answer a TLS handshake
// with plain HTTP.
//
// It is skipped unless that variable is set, so that the suite never depends on
// a subprocess it did not start. Run it with:
//
//	go run ./cmd/mockm2lx -addr 127.0.0.1:18081 -srt-addr 127.0.0.1:14001
//	$env:WSLCOMMS_MOCK_ADDR='http://127.0.0.1:18081'
//	go test -tags dev -run MockM2LX -v .
func TestControlPlaneAgainstMockM2LX(t *testing.T) {
	addr := os.Getenv("WSLCOMMS_MOCK_ADDR")
	if addr == "" {
		t.Skip("set WSLCOMMS_MOCK_ADDR to a running cmd/mockm2lx, scheme included: http://127.0.0.1:18081")
	}

	a, store := newTestApp(t)
	silencePump(a)

	if err := store.Set(secrets.KeyM2LX, mockEnv("WSLCOMMS_MOCK_PASSWORD", "changeme")); err != nil {
		t.Fatalf("seeding the credential store: %v", err)
	}

	cfg := validConfig()
	cfg.M2LXHost = addr
	cfg.Alias = mockEnv("WSLCOMMS_MOCK_ALIAS", "wsl-comms-ro")
	cfg.StatusKey = mockEnv("WSLCOMMS_MOCK_STATUS_KEY", "cam7")
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	// Exactly what startup does once config.json has been read.
	a.startControlPlane()

	// The sign-in loop retries on its own ladder, so this is a wait rather than
	// a single attempt: the mock may have been started a moment ago.
	waitFor(t, 30*time.Second, "a bearer token from the mock", func() bool {
		a.ctlMu.Lock()
		client := a.client
		a.ctlMu.Unlock()
		return client != nil && client.Token() != ""
	})

	// The Watcher opens the status socket with the token in the URL, so this is
	// also proof that sign-in and the socket agree about the token. The mock
	// pushes a snapshot every -status-interval, two seconds by default.
	var status m2lx.Status
	waitFor(t, 30*time.Second, "a status snapshot to reach the event pump", func() bool {
		for _, e := range drainPump(a) {
			if e.name != EventStatus {
				continue
			}
			if s, ok := e.data.(m2lx.Status); ok {
				status = s
				return true
			}
		}
		return false
	})

	if status.Stale {
		t.Fatalf("the first status was already stale: %+v", status)
	}
	if status.StreamState == "" {
		t.Fatalf("status carried no stream_state: %+v", status)
	}
	t.Logf("signed in to %s as %q and received %+v", addr, cfg.Alias, status)
}

// TestStatusGoesStaleWhenTheMockStallsTheSocket drives one of cmd/mockm2lx's
// fault injections all the way through to the event the frontend renders.
//
// Stalling the status socket is the failure that looks like nothing: the
// WebSocket stays open, so a naive implementation holds its last known values
// green forever while the switcher's view of the feed is hours old.
// m2lx.StaleAfter (15 s) is what catches it, and specification section 8 says
// the three WebSocket-derived lamps must then grey out under STATUS
// UNAVAILABLE. This asserts the Status carrying that verdict actually reaches
// the pump, and that clearing the fault clears the verdict.
//
// Same gate and same address as TestControlPlaneAgainstMockM2LX. It takes a
// little over m2lx.StaleAfter to run, which is why it is not in the default
// suite.
func TestStatusGoesStaleWhenTheMockStallsTheSocket(t *testing.T) {
	addr := os.Getenv("WSLCOMMS_MOCK_ADDR")
	if addr == "" {
		t.Skip("set WSLCOMMS_MOCK_ADDR to a running cmd/mockm2lx, scheme included: http://127.0.0.1:18081")
	}

	a, store := newTestApp(t)
	silencePump(a)

	if err := store.Set(secrets.KeyM2LX, mockEnv("WSLCOMMS_MOCK_PASSWORD", "changeme")); err != nil {
		t.Fatalf("seeding the credential store: %v", err)
	}
	cfg := validConfig()
	cfg.M2LXHost = addr
	cfg.Alias = mockEnv("WSLCOMMS_MOCK_ALIAS", "wsl-comms-ro")
	cfg.StatusKey = mockEnv("WSLCOMMS_MOCK_STATUS_KEY", "cam7")
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	a.startControlPlane()
	waitForStatus(t, a, 30*time.Second, "a live status snapshot", func(s m2lx.Status) bool {
		return !s.Stale
	})

	control(t, addr, "/control/stall-status", `{"enabled":true}`)
	t.Cleanup(func() { control(t, addr, "/control/reset", "") })

	waitForStatus(t, a, m2lx.StaleAfter+20*time.Second, "the staleness verdict", func(s m2lx.Status) bool {
		return s.Stale
	})
	t.Logf("the stalled socket produced Stale=true after m2lx.StaleAfter (%s)", m2lx.StaleAfter)

	control(t, addr, "/control/stall-status", `{"enabled":false}`)
	waitForStatus(t, a, 30*time.Second, "the lamps to repopulate from live data", func(s m2lx.Status) bool {
		return !s.Stale
	})
}

// waitForStatus drains the event pump until a Status satisfying cond arrives.
// Status events are what the three WebSocket-derived lamps are drawn from.
func waitForStatus(t *testing.T, a *App, limit time.Duration, what string, cond func(m2lx.Status) bool) {
	t.Helper()
	waitFor(t, limit, what, func() bool {
		for _, e := range drainPump(a) {
			if e.name != EventStatus {
				continue
			}
			if s, ok := e.data.(m2lx.Status); ok && cond(s) {
				return true
			}
		}
		return false
	})
}

// control POSTs to one of cmd/mockm2lx's fault-injection endpoints. body may be
// empty for the endpoints that take no arguments.
func control(t *testing.T, addr, path, body string) {
	t.Helper()

	// The mock's /control API is plain HTTP on the same listener as the REST
	// API, and WSLCOMMS_MOCK_ADDR already carries the scheme internal/m2lx
	// needs, so it is usable here verbatim.
	resp, err := http.Post(strings.TrimSuffix(addr, "/")+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("POST %s returned %s", path, resp.Status)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// waitFor polls cond until it is true or the deadline passes. It exists because
// the sender's transitions happen on their own goroutine and the alternative —
// sleeping a fixed time and hoping — is how flaky tests are written.
func waitFor(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", limit, what)
}

// exportedMethodsOfApp returns the names of every exported method of *App. This
// is what Wails binds, so it is the frontend's entire view of Go.
func exportedMethodsOfApp() map[string]bool {
	out := map[string]bool{}
	typ := reflect.TypeOf(&App{})
	for i := 0; i < typ.NumMethod(); i++ {
		out[typ.Method(i).Name] = true
	}
	return out
}

// stubClient is an m2lx.Client that makes no network call. GetKVSCredentials
// only needs Token() to decide whether it is worth trying the chain, and
// App.ListEvents needs a canned event list (or error) to return once it has
// passed its own client/token guards — so fakeEvents and fakeEventsErr are
// settable and drive the App.ListEvents tests directly.
type stubClient struct {
	token string

	fakeEvents    []m2lx.Event
	fakeEventsErr error

	// fakeConfig and fakeConfigErr drive conformFormat's first source: the
	// switcher's configured video format. The zero value is an instance that
	// answers with nothing usable, which conformFormat must survive by falling
	// through to the override — see app_conform_test.go, which sets these.
	fakeConfig    m2lx.SwitcherConfiguration
	fakeConfigErr error
}

func (c stubClient) SignIn(context.Context, string, string) error { return nil }
func (c stubClient) Refresh(context.Context) error                { return nil }
func (c stubClient) Token() string                                { return c.token }

// ListEvents returns the canned events (or error) the test configured, so that
// App.ListEvents can be exercised without a live instance.
func (c stubClient) ListEvents(context.Context) ([]m2lx.Event, error) {
	return c.fakeEvents, c.fakeEventsErr
}

// SwitcherConfiguration returns the canned configuration (or error) the test
// configured, so the conform ladder can be exercised without a live instance.
func (c stubClient) SwitcherConfiguration(context.Context) (m2lx.SwitcherConfiguration, error) {
	return c.fakeConfig, c.fakeConfigErr
}

func (c stubClient) KVSInfo(context.Context, string) (m2lx.KVSInfo, error) {
	return m2lx.KVSInfo{}, errors.New("stubClient: KVSInfo is not implemented")
}

func (c stubClient) KVSToken(context.Context, string) (m2lx.KVSToken, error) {
	return m2lx.KVSToken{}, errors.New("stubClient: KVSToken is not implemented")
}

// Close satisfies m2lx.Client. The stub owns no goroutine, so there is nothing
// to stop.
func (c stubClient) Close() error { return nil }

var _ m2lx.Client = stubClient{}

// closeSpyClient counts Close calls, which is how the control plane's one
// non-obvious shutdown obligation is asserted: cancelling the generation's
// context does not stop the client's token-refresh goroutine, only Close does.
type closeSpyClient struct {
	stubClient

	mu     sync.Mutex
	closes int
	err    error
}

func (c *closeSpyClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes++
	return c.err
}

func (c *closeSpyClient) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

var _ m2lx.Client = (*closeSpyClient)(nil)

// ---------------------------------------------------------------------------
// The SRT host default
// ---------------------------------------------------------------------------

func TestStartAcceptsAnEmptySRTHostAndDialsTheM2LXHost(t *testing.T) {
	// The operator: "I shouldn't need to specify the SRT host again, it will be
	// the same as the m2lx host." An empty srtHost is now the normal case, and
	// Start must not only accept it but dial the right machine.
	a, _ := newTestApp(t)
	silencePump(a)

	cfg := validConfig()
	cfg.M2LXHost = "http://m2lx.example.com:8080"
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	if err := a.Start(); err != nil {
		t.Fatalf("Start() dialling the derived SRT host error = %v, want nil", err)
	}
	t.Cleanup(func() { _ = a.Stop() })

	a.sessMu.Lock()
	sess := a.session
	a.sessMu.Unlock()
	stub, ok := sess.pipe.(*gst.StubPipeline)
	if !ok {
		t.Fatalf("pipeline is %T, want the Gate A stub", sess.pipe)
	}

	waitFor(t, 5*time.Second, "a sink to be attached", func() bool {
		_, attached := stub.AttachedSink()
		return attached
	})
	sink, _ := stub.AttachedSink()
	if sink.Host != "m2lx.example.com" {
		t.Fatalf("sink dialled %q, want the M2L-X host with the scheme and port stripped", sink.Host)
	}
	if sink.Port != cfg.SRTPort {
		t.Fatalf("sink port = %d, want the configured %d — only the HOST is inherited", sink.Port, cfg.SRTPort)
	}
}

func TestStartAcceptsAnEmptyStatusKey(t *testing.T) {
	// statusKey costs the three WebSocket-derived lamps, not the feed. Refusing
	// to start without it made the app unusable until the operator guessed a
	// value that nothing in the M2L-X API will tell them.
	a, _ := newTestApp(t)
	silencePump(a)

	cfg := validConfig()
	cfg.StatusKey = ""
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	if err := a.Start(); err != nil {
		t.Fatalf("Start() with an empty statusKey error = %v, want nil", err)
	}
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestSenderOptsResolvesTheSRTHostAndReportsUnderThatName(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	cfg := validConfig()
	cfg.M2LXHost = "m2lx.example.com"
	opts := a.senderOpts(cfg, "")

	if opts.Sink.Host != "m2lx.example.com" {
		t.Fatalf("sink host = %q, want the M2L-X host", opts.Sink.Host)
	}

	// The reporter must name the host that was actually dialled: an error
	// reading "the commentary feed to :4001" helps nobody.
	opts.OnConnectError(errors.New("connection refused"))
	queued := drainPump(a)
	if len(queued) != 1 {
		t.Fatalf("queued %d events, want one: %+v", len(queued), queued)
	}
	msg, _ := queued[0].data.(string)
	if !strings.Contains(msg, "m2lx.example.com") {
		t.Fatalf("the operator was told %q, which does not name the host that was dialled", msg)
	}
}

// ---------------------------------------------------------------------------
// statusKey discovery
// ---------------------------------------------------------------------------

// stubWatcher is an m2lx.Watcher whose WatchAll channel the test drives. Watch
// is not used by the discovery path and returns a closed channel.
type stubWatcher struct {
	docs chan m2lx.Document

	// raw is what RawSnapshot returns, and rawErr what it fails with. rawCalls
	// counts the calls, which is how the "GetMixerSnapshot never serves a
	// cache" property is asserted: two calls must produce two reads.
	rawMu    sync.Mutex
	raw      []byte
	rawErr   error
	rawCalls int
}

func newStubWatcher() *stubWatcher {
	return &stubWatcher{docs: make(chan m2lx.Document, 8)}
}

// setRaw installs the frame RawSnapshot will return.
func (w *stubWatcher) setRaw(raw []byte, err error) {
	w.rawMu.Lock()
	defer w.rawMu.Unlock()
	w.raw, w.rawErr = raw, err
}

func (w *stubWatcher) rawSnapshotCalls() int {
	w.rawMu.Lock()
	defer w.rawMu.Unlock()
	return w.rawCalls
}

func (w *stubWatcher) RawSnapshot(context.Context) ([]byte, error) {
	w.rawMu.Lock()
	defer w.rawMu.Unlock()
	w.rawCalls++
	if w.rawErr != nil {
		return nil, w.rawErr
	}
	return w.raw, nil
}

func (w *stubWatcher) Watch(context.Context, string) <-chan m2lx.Status {
	ch := make(chan m2lx.Status)
	close(ch)
	return ch
}

func (w *stubWatcher) WatchAll(ctx context.Context) <-chan m2lx.Document {
	out := make(chan m2lx.Document)
	go func() {
		defer close(out)
		for {
			select {
			case d := <-w.docs:
				select {
				case out <- d:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

var _ m2lx.Watcher = (*stubWatcher)(nil)

// withStubControlPlane installs a control plane generation whose watcher the
// test drives. It stands in for startControlPlane, which would open a real
// socket to a host that does not exist.
func withStubControlPlane(t *testing.T, a *App) *stubWatcher {
	t.Helper()
	w := newStubWatcher()
	ctx, cancel := context.WithCancel(context.Background())

	a.ctlMu.Lock()
	a.watcher = w
	a.ctlCtx = ctx
	a.ctlCancel = cancel
	a.ctlWG = &sync.WaitGroup{}
	wg := a.ctlWG
	a.ctlMu.Unlock()

	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return w
}

func TestStartDiscoversTheStatusKeyFromTheNodeThatStartsStreaming(t *testing.T) {
	// Specification open question 5, mechanised: "read one switcher_status
	// snapshot and find the node whose stream_state changes when the app
	// starts."
	a, _ := newTestApp(t)
	silencePump(a)
	w := withStubControlPlane(t, a)

	cfg := validConfig()
	cfg.StatusKey = ""
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })

	now := time.Now()
	// The baseline: cam3 is somebody else's input, already up.
	w.docs <- m2lx.Document{At: now, Nodes: map[string]m2lx.NodeState{
		"cam3": {StreamState: m2lx.StreamStateStreaming, Video: "h264 1920x1080 50 P", AudioCount: 1},
		"cam7": {StreamState: m2lx.StreamStateStopped},
	}}
	// Our feed arrives.
	w.docs <- m2lx.Document{At: now.Add(2 * time.Second), Nodes: map[string]m2lx.NodeState{
		"cam3": {StreamState: m2lx.StreamStateStreaming, Video: "h264 1920x1080 50 P", AudioCount: 1},
		"cam7": {StreamState: m2lx.StreamStateStreaming, Video: "h264 1920x1080 50 P", AudioCount: 1},
	}}

	waitFor(t, 5*time.Second, "a statusKey suggestion", func() bool {
		got, _ := a.GetStatusKeyCandidates()
		return len(got) > 0
	})

	got, err := a.GetStatusKeyCandidates()
	if err != nil {
		t.Fatalf("GetStatusKeyCandidates() error = %v", err)
	}
	if len(got) != 1 || got[0].Key != "cam7" {
		t.Fatalf("candidates = %+v, want only cam7 — cam3 was already streaming", got)
	}
	if got[0].Was != m2lx.StreamStateStopped || got[0].Now != m2lx.StreamStateStreaming {
		t.Errorf("candidate transition = %q -> %q, want stopped -> streaming", got[0].Was, got[0].Now)
	}

	// The suggestion is published, and it is NOT written to the configuration.
	var announced bool
	for _, e := range drainPump(a) {
		if e.name == EventStatusKeys {
			announced = true
			if _, ok := e.data.([]m2lx.StatusKeyCandidate); !ok {
				t.Fatalf("the %q event carried %T, want []m2lx.StatusKeyCandidate", EventStatusKeys, e.data)
			}
		}
	}
	if !announced {
		t.Fatalf("no %q event was queued for the frontend", EventStatusKeys)
	}
	if saved := a.snapshotConfig().StatusKey; saved != "" {
		t.Fatalf("discovery wrote %q into the configuration; it must only ever suggest", saved)
	}
}

func TestDiscoveryOffersEveryNodeThatCameUpAtOnce(t *testing.T) {
	// Two nodes changing together is the case where picking one would be
	// picking at random and calling it knowledge.
	a, _ := newTestApp(t)
	silencePump(a)
	w := withStubControlPlane(t, a)

	cfg := validConfig()
	cfg.StatusKey = ""
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })

	now := time.Now()
	w.docs <- m2lx.Document{At: now, Nodes: map[string]m2lx.NodeState{
		"cam7": {StreamState: m2lx.StreamStateStopped},
		"cam9": {StreamState: m2lx.StreamStateStopped},
	}}
	w.docs <- m2lx.Document{At: now.Add(time.Second), Nodes: map[string]m2lx.NodeState{
		"cam7": {StreamState: m2lx.StreamStateStreaming},
		"cam9": {StreamState: m2lx.StreamStateStreaming},
	}}

	waitFor(t, 5*time.Second, "both statusKey suggestions", func() bool {
		got, _ := a.GetStatusKeyCandidates()
		return len(got) == 2
	})
}

func TestNoDiscoveryWhenTheStatusKeyIsAlreadyConfigured(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)
	w := withStubControlPlane(t, a)

	// validConfig has a statusKey, so there is nothing to discover.
	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })

	select {
	case w.docs <- m2lx.Document{At: time.Now(), Nodes: map[string]m2lx.NodeState{
		"cam7": {StreamState: m2lx.StreamStateStreaming},
	}}:
	default:
	}

	// Nothing is reading those documents if no discovery started, so the
	// evidence is the absence of suggestions after long enough for one to have
	// produced some.
	time.Sleep(200 * time.Millisecond)
	if got, _ := a.GetStatusKeyCandidates(); len(got) != 0 {
		t.Fatalf("candidates = %+v, want none: a statusKey is already configured", got)
	}
}

func TestAFailedStartDoesNotLeaveADiscoveryRunning(t *testing.T) {
	// The discovery is armed BEFORE the pipeline, so that its baseline predates
	// our feed reaching the switcher. That means a Start which then fails
	// validation has to unwind it, or a mistyped Settings screen leaves a
	// ninety-second watcher running that can only ever report nothing.
	a, _ := newTestApp(t)
	silencePump(a)
	withStubControlPlane(t, a)

	cfg := validConfig()
	cfg.StatusKey = ""
	cfg.EventID = "" // fails config.Validate
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	if err := a.Start(); err == nil {
		_ = a.Stop()
		t.Fatal("Start() succeeded on an invalid configuration")
	}

	waitFor(t, 5*time.Second, "the discovery to unwind", func() bool {
		return !a.discovering.Load()
	})
}

func TestGetStatusKeyCandidatesReturnsACopyAndNeverNil(t *testing.T) {
	a, _ := newTestApp(t)

	got, err := a.GetStatusKeyCandidates()
	if err != nil {
		t.Fatalf("GetStatusKeyCandidates() error = %v", err)
	}
	if got == nil {
		t.Fatal("GetStatusKeyCandidates() = nil; the frontend renders a list and must not have to special-case null")
	}

	a.setStatusKeyCandidates([]m2lx.StatusKeyCandidate{{Key: "cam7"}})
	first, _ := a.GetStatusKeyCandidates()
	first[0].Key = "mutated-by-the-caller"

	second, _ := a.GetStatusKeyCandidates()
	if second[0].Key != "cam7" {
		t.Fatalf("a caller mutated the stored candidates: %q", second[0].Key)
	}
}

// TestShutdownTimeoutJustificationNamesEveryStopItBounds guards against a
// justification that has stopped being true.
//
// shutdownTimeout is not a number on its own. It is a number plus an argument
// about which waits it contains, and that argument is what the next person
// reading it will trust instead of re-deriving. The failure this catches is
// silent and has already happened once: teardownOrdered grew a StopReturn call
// while the comment still explained fifteen seconds as "the sum" of two timeouts
// that belong entirely to the SENDER's path — so the sum was arithmetically
// right about a set of waits that was no longer the set being performed.
//
// The rule is not "the budget must cover everything". Some of it genuinely
// cannot be covered — a return Stop landing on a Play in flight runs to
// thirty-three seconds for one address, and containing that would make a closed
// window hang for the best part of a minute. The rule is that every stop the
// teardown performs must be ACCOUNTED FOR in the justification: covered, or
// named as expected to be cut off. Either is honest; silence is not.
func TestShutdownTimeoutJustificationNamesEveryStopItBounds(t *testing.T) {
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("reading app.go: %v", err)
	}
	text := string(src)

	// The doc comment on the constant: from "// shutdownTimeout bounds" to the
	// declaration itself.
	start := strings.Index(text, "// shutdownTimeout bounds")
	if start < 0 {
		t.Fatal("app.go no longer documents shutdownTimeout")
	}
	end := strings.Index(text[start:], "shutdownTimeout =")
	if end < 0 {
		t.Fatal("app.go no longer declares shutdownTimeout after its comment")
	}
	justification := text[start : start+end]

	// Every a.Something() call teardownOrdered makes that can block on a
	// pipeline. Read from the source rather than listed here, so that a step
	// added later is caught rather than assumed away.
	// Matched without the return type and the brace: teardownOrdered grew a
	// count of the steps it had to abandon, and this test is about what the
	// justification says, not about the signature.
	bodyStart := strings.Index(text, "func (a *App) teardownOrdered()")
	if bodyStart < 0 {
		t.Fatal("app.go no longer has teardownOrdered")
	}
	body := text[bodyStart : bodyStart+strings.Index(text[bodyStart:], "\n}\n")]

	for _, call := range []string{"a.Stop()", "a.StopReturn()"} {
		if !strings.Contains(body, call) {
			continue // no longer part of the teardown; nothing to justify
		}
		name := strings.TrimSuffix(strings.TrimPrefix(call, "a."), "()")
		if !strings.Contains(justification, name) {
			t.Errorf("teardownOrdered calls %s but shutdownTimeout's justification never "+
				"mentions %s. Say what its worst case is and whether this budget covers it — "+
				"a stop that is expected to be cut off is a fine answer, an unstated one is not.",
				call, name)
		}
	}

	// The specific false claim that was there before: fifteen seconds presented
	// as "the sum" of the sender's two timeouts, with a second stop in the same
	// budget. If the justification ever claims to be a complete sum again, it has
	// to also say what is outside it.
	if strings.Contains(justification, "the sum, not a guess") &&
		strings.Contains(body, "a.StopReturn()") {
		t.Error("shutdownTimeout is justified as a complete sum while teardownOrdered runs a " +
			"second stop inside the same budget; the sum is of the sender's waits only")
	}

	// And it must be at least the sender's own worst case, which is the one wait
	// the teardown genuinely cannot shorten.
	const senderWorstCase = 15 * time.Second
	if shutdownTimeout < senderWorstCase {
		t.Errorf("shutdownTimeout = %s, below the sender's own bounded worst case of %s",
			shutdownTimeout, senderWorstCase)
	}
}

// ---------------------------------------------------------------------------
// The input meters' "levels" events
// ---------------------------------------------------------------------------

// isSilentLevels reports whether a payload is the session-end zero-frame:
// every channel at the silence floor. An empty payload is NOT silent for this
// test's purposes — it is malformed, and the assertions below should see it.
func isSilentLevels(p levelsPayload) bool {
	if len(p.Peak) == 0 {
		return false
	}
	for _, v := range p.Peak {
		if v > levelsSilenceDB {
			return false
		}
	}
	return true
}

func TestSessionEmitsLevelsAndAZeroFrameOnStop(t *testing.T) {
	// The whole path at Gate A: the stub pipeline's synthetic ticker calls
	// PipelineOpts.OnLevels (wired by senderOpts), the App-side forwarder
	// throttles and queues "levels" events on the pump, and the session's end
	// queues one final all-silence frame so the meters fall rather than
	// freeze. The real build swaps only the producer.
	a, _ := newTestApp(t)

	if err := a.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	var live *levelsPayload
	waitFor(t, 5*time.Second, "a live levels frame to reach the frontend queue", func() bool {
		for _, e := range drainPump(a) {
			if e.name != EventLevels {
				continue
			}
			p, ok := e.data.(levelsPayload)
			if !ok {
				t.Fatalf("a %q event carried a %T, want levelsPayload", EventLevels, e.data)
			}
			if !isSilentLevels(p) {
				pp := p
				live = &pp
			}
		}
		return live != nil
	})

	if len(live.Peak) != 2 || len(live.RMS) != 2 {
		t.Fatalf("live frame has %d peak / %d rms channels, want 2 of each (the pipeline pins stereo)",
			len(live.Peak), len(live.RMS))
	}
	for i := range live.Peak {
		if live.Peak[i] < levelsSilenceDB || live.Peak[i] > 0 {
			t.Errorf("channel %d peak = %v dBFS, outside [%d, 0]", i, live.Peak[i], levelsSilenceDB)
		}
		if live.RMS[i] > live.Peak[i] {
			t.Errorf("channel %d rms %v is above its peak %v", i, live.RMS[i], live.Peak[i])
		}
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	// The zero-frame is already queued by the time Stop returns: the sender
	// stops the pipeline (which joins the stub's ticker) BEFORE closing its
	// states channel, the forwarder goroutine sends the zero-frame after the
	// channel closes and before its wg.Done, and Stop waits on that WaitGroup.
	// So the LAST levels event in the queue is deterministically the silence.
	var last *levelsPayload
	for _, e := range drainPump(a) {
		if e.name != EventLevels {
			continue
		}
		if p, ok := e.data.(levelsPayload); ok {
			pp := p
			last = &pp
		}
	}
	if last == nil {
		t.Fatal("no levels event was queued during Stop; the zero-frame never arrived and " +
			"the meters would freeze at the last live level")
	}
	if !isSilentLevels(*last) {
		t.Fatalf("the final levels frame after Stop is %+v, want every channel at %v dBFS: "+
			"a meter frozen at the last level reads as a live one", *last, float64(levelsSilenceDB))
	}
}

func TestLevelsForwarderThrottlesToTheMinInterval(t *testing.T) {
	// The producer runs at 20 Hz and the pump drops OLDEST under pressure, so
	// without this throttle a stalled renderer turns the meter bursty and
	// purges other events from the shared queue. The clock is injected, so no
	// sleeps: four calls inside one 50 ms window must forward exactly two —
	// the first, and the one landing exactly on the interval boundary.
	a, _ := newTestApp(t)

	clock := time.Unix(1_700_000_000, 0)
	fwd := a.levelsForwarder(func() time.Time { return clock })
	frame := gst.Levels{PeakDB: []float64{-10, -12}, RMSDB: []float64{-18, -20}}

	fwd(frame) // forwarded: the first frame
	fwd(frame) // dropped: same instant
	clock = clock.Add(49 * time.Millisecond)
	fwd(frame) // dropped: inside the floor
	clock = clock.Add(1 * time.Millisecond)
	fwd(frame) // forwarded: exactly levelsMinInterval after the first

	count := 0
	for _, e := range drainPump(a) {
		if e.name == EventLevels {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("the forwarder queued %d levels events for four calls in one interval window, want 2", count)
	}
}
