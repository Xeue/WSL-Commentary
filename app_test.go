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
	"strings"
	"sync"
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
	c.SRTHost = "127.0.0.1"
	c.SRTPort = 4001
	c.StatusKey = "cam7"
	c.AudioDeviceID = "{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}"
	return c
}

// newTestApp builds an App wired to fakes, with %APPDATA% redirected at a temp
// directory so config.Save cannot touch the developer's real config.json.
//
// It deliberately does not call startup: the tests that want the startup path
// call it themselves.
func newTestApp(t *testing.T) (*App, *fakeStore) {
	t.Helper()
	t.Setenv("APPDATA", t.TempDir())

	a := NewApp(t.TempDir(), nil)
	store := newFakeStore()
	a.store = store

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
	withSRT.SRTHost = "10.0.0.1"
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
	appDir := filepath.FromSlash("C:/Program Files/WSLComms")
	absolute := filepath.FromSlash("D:/slates/wsl-2026.png")

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
	t.Setenv("APPDATA", t.TempDir())

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
	t.Setenv("APPDATA", t.TempDir())

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
	// domReady replays TWO lamps, not one: the return monitor emits only on
	// transitions for the same reason, and a reloaded page would otherwise show
	// the RETURN lamp grey with audio in the commentator's ears. The count is
	// asserted exactly so that a third replay has to be a decision — every event
	// queued here is delivered to a page that has just loaded, and the ones the
	// status lamps would need are deliberately NOT replayed because a cached
	// Status risks showing stale green (specification section 8).
	a, _ := newTestApp(t)
	silencePump(a)

	a.senderMu.Lock()
	a.lastSender = sender.StateConnected
	a.senderMu.Unlock()

	a.domReady(context.Background())

	queued := drainPump(a)
	if len(queued) != 2 {
		t.Fatalf("domReady queued %d events, want the sender and return replays: %+v", len(queued), queued)
	}
	if queued[0].name != EventSender || queued[0].data != sender.StateConnected {
		t.Fatalf("domReady queued %+v, want %s = %s", queued[0], EventSender, sender.StateConnected)
	}
	if queued[1].name != EventReturn || queued[1].data != gst.ReturnStateStopped {
		t.Fatalf("domReady queued %+v, want %s = %s", queued[1], EventReturn, gst.ReturnStateStopped)
	}
}

func TestDomReadyReplaysStoppedBeforeAnySession(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	a.domReady(context.Background())

	queued := drainPump(a)
	if len(queued) != 2 {
		t.Fatalf("domReady queued %+v, want a sender and a return replay", queued)
	}
	if queued[0].data != sender.StateStopped {
		t.Fatalf("domReady queued %+v, want %s before any session has run", queued, sender.StateStopped)
	}
	if queued[1].data != gst.ReturnStateStopped {
		t.Fatalf("domReady queued %+v, want %s before any return monitor has run",
			queued, gst.ReturnStateStopped)
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
	got.SRTHost = "mutated-by-the-caller"

	again, err := a.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	if again.SRTHost == "mutated-by-the-caller" {
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
		// srtHost and statusKey are deliberately absent from this list: an empty
		// srtHost means "the same host as M2L-X" (config.EffectiveSRTHost) and an
		// empty statusKey costs the three WebSocket lamps, not the feed. Both are
		// covered by TestStartAcceptsAnEmptySRTHostAndStatusKey below.
		{"missing srtHost AND m2lxHost", func(c *config.Config) { c.SRTHost = ""; c.M2LXHost = "" }, "srtHost"},
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
	if sink.Host != cfg.SRTHost || sink.Port != cfg.SRTPort {
		t.Fatalf("sink dialled %s:%d, want %s:%d", sink.Host, sink.Port, cfg.SRTHost, cfg.SRTPort)
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
	if opts.Sink.Host != cfg.SRTHost || opts.Sink.Port != cfg.SRTPort {
		t.Fatalf("sink = %s:%d, want %s:%d", opts.Sink.Host, opts.Sink.Port, cfg.SRTHost, cfg.SRTPort)
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
// only needs Token() to decide whether it is worth trying the chain.
type stubClient struct {
	token string
}

func (c stubClient) SignIn(context.Context, string, string) error { return nil }
func (c stubClient) Refresh(context.Context) error                { return nil }
func (c stubClient) Token() string                                { return c.token }

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
	cfg.SRTHost = ""
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	if err := a.Start(); err != nil {
		t.Fatalf("Start() with an empty srtHost error = %v, want nil", err)
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
	cfg.SRTHost = ""
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
