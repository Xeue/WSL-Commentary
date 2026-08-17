//go:build !cgo || gststub

package gst

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"math"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestListInputDevicesReturnsFakes(t *testing.T) {
	SetStubDevices(nil)
	devices, err := ListInputDevices()
	if err != nil {
		t.Fatalf("ListInputDevices: %v", err)
	}
	if len(devices) == 0 {
		t.Fatal("the stub offered no devices at all")
	}
	if devices[0].Name != "DVS Receive  1-2 (Dante Virtual Soundcard)" {
		t.Errorf("first device name = %q", devices[0].Name)
	}
	if devices[0].ID == devices[0].Name {
		t.Error("device ID must be the endpoint GUID, not the display name")
	}

	// THE TWIN PAIR: one card enumerating twice, once through the platform's own
	// audio stack and once through GStreamer's decklink provider, under names an
	// operator cannot tell apart. It is the measured shape of the owner's original
	// bug and the case the dropdown's labelling exists for, and a stub in which
	// the two never collide is one where the labelling that separates them can be
	// broken without any test noticing.
	byName := map[string]map[DeviceKind]bool{}
	for _, d := range devices {
		if byName[d.Name] == nil {
			byName[d.Name] = map[DeviceKind]bool{}
		}
		byName[d.Name][NormaliseDeviceKind(d.Kind)] = true
	}
	twins := 0
	for _, kinds := range byName {
		if kinds[KindNative] && kinds[KindDeckLink] {
			twins++
		}
	}
	if twins != 1 {
		t.Errorf("the stub offers %d name collisions between a native and a DeckLink entry, want "+
			"exactly 1: without the twin pair nothing at Gate A exercises the labelling that "+
			"separates them", twins)
	}

	// EVERY WIDTH THE ROUTING PANEL MUST DRAW has a device to draw it from. The
	// panel appears at every width the pad negotiates — the operator overruled a
	// `width > 2` gate, because flipping a stereo pair and routing a mono input to
	// both sides are real routing decisions — so a device list that is three
	// stereo entries and a card can only ever test one shape of grid.
	widths := map[int]bool{}
	for _, d := range devices {
		widths[d.Channels] = true
	}
	for _, want := range []int{1, 2, 3, 8, 16, MaxInputChannels} {
		if !widths[want] {
			t.Errorf("no stub device presents %d input channels, so no Gate A test can size the "+
				"routing panel to it", want)
		}
	}
}

func TestListInputDevicesIsACopy(t *testing.T) {
	SetStubDevices(nil)
	devices, _ := ListInputDevices()
	devices[0].Name = "mutated"
	again, _ := ListInputDevices()
	if again[0].Name == "mutated" {
		t.Error("ListInputDevices leaked its backing array")
	}
}

// TestStartRequiresSlateAndDevice, TestStartRefusesARenderEndpoint,
// TestStubRefusesAVideoCaptureIDThatIsNotACard and TestStubResolvesTheConformTarget
// WERE HERE, AND EVERY RULE THEY ASSERTED STILL RUNS.
//
// All four were about what a pipeline may be told to OPEN — a slate, a device, a
// card, a raster — and a send pipeline is told none of those. NewStubCapture makes
// the identical refusals in the identical order, through the same shared helpers
// (refuseWrongAudioSource, parseDeckLinkPersistentID, ConformTarget.resolve), and
// capture_stub_test.go exercises them on every seat shape the operator has.
//
// They are named here rather than silently deleted because a reader who
// remembers them needs to be told where they went, not left to conclude the
// rules were dropped.
func TestStartInstallsNoSink(t *testing.T) {
	p := NewStubPipeline(stubSeatCapture(t))
	if err := p.Start(SendOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p.State() != StubStateRunning {
		t.Errorf("state = %q, want %q", p.State(), StubStateRunning)
	}
	if _, ok := p.AttachedSink(); ok {
		t.Error("Start must not install a sink")
	}
	if got := p.StartedWith().VideoBitrateKbps; got != DefaultVideoBitrateKbps {
		t.Errorf("VideoBitrateKbps = %d, want default %d", got, DefaultVideoBitrateKbps)
	}
	if got := p.StartedWith().AudioBitrateBps; got != DefaultAudioBitrateBps {
		t.Errorf("AudioBitrateBps = %d, want default %d", got, DefaultAudioBitrateBps)
	}
}

func TestReplaceSinkFailureLadder(t *testing.T) {
	p := NewStubPipeline(stubSeatCapture(t))
	if err := p.Start(SendOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	boom := errors.New("connection refused")
	p.FailNextSinks(2, boom)

	sink := SinkOpts{Host: "m2lx.example", Port: 9001}
	for i := range 2 {
		if err := p.ReplaceSink(sink); !errors.Is(err, boom) {
			t.Fatalf("attempt %d: err = %v, want %v", i, err, boom)
		}
		if p.State() != StubStateRunning {
			t.Fatalf("a failed ReplaceSink must leave the pipeline running, got %q", p.State())
		}
	}

	if err := p.ReplaceSink(sink); err != nil {
		t.Fatalf("third attempt should succeed, got %v", err)
	}
	if p.State() != StubStateSinkAttached {
		t.Errorf("state = %q, want %q", p.State(), StubStateSinkAttached)
	}
	got, ok := p.AttachedSink()
	if !ok {
		t.Fatal("no sink attached after a successful ReplaceSink")
	}
	if got.LatencyMs != DefaultSRTLatencyMs {
		t.Errorf("LatencyMs = %d, want default %d", got.LatencyMs, DefaultSRTLatencyMs)
	}

	c := p.Counters()
	if c.ReplaceSinks != 3 || c.SinksAttached != 1 {
		t.Errorf("counters = %+v, want 3 attempts and 1 success", c)
	}
}

func TestInjectErrorReachesErrorsChannel(t *testing.T) {
	p := NewStubPipeline(stubSeatCapture(t))
	if err := p.Start(SendOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	boom := errors.New("srtout: peer gone")
	if !p.InjectError(boom) {
		t.Fatal("InjectError reported a drop on an empty buffer")
	}
	select {
	case got := <-p.Errors():
		if !errors.Is(got, boom) {
			t.Errorf("got %v, want %v", got, boom)
		}
	default:
		t.Fatal("nothing on the error channel")
	}
}

func TestInjectErrorDropsRatherThanBlocks(t *testing.T) {
	p := NewStubPipeline(stubSeatCapture(t))
	if err := p.Start(SendOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range stubErrorBuffer {
		if !p.InjectError(errors.New("fill")) {
			t.Fatal("dropped before the buffer was full")
		}
	}
	if p.InjectError(errors.New("overflow")) {
		t.Error("want a drop once the buffer is full")
	}
}

func TestStopClosesErrorsAndIsIdempotent(t *testing.T) {
	p := NewStubPipeline(stubSeatCapture(t))
	if err := p.Start(SendOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if _, open := <-p.Errors(); open {
		t.Error("Errors must be closed by Stop")
	}
	if p.State() != StubStateStopped {
		t.Errorf("state = %q, want %q", p.State(), StubStateStopped)
	}
	if err := p.ReplaceSink(SinkOpts{Host: "h", Port: 1}); err == nil {
		t.Error("ReplaceSink after Stop must fail")
	}
	if err := p.ForceKeyUnit(); err == nil {
		t.Error("ForceKeyUnit after Stop must fail")
	}
}

func TestNewReturnsAStubPipeline(t *testing.T) {
	p, err := New(stubSeatCapture(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := p.(*StubPipeline); !ok {
		t.Fatalf("New returned %T, want *StubPipeline", p)
	}
}

// TestNewRefusesASendPipelineWithNothingBehindIt is the refusal that makes the
// central claim of the seam a compile-and-run guarantee rather than a convention.
//
// A proxysrc with no proxysink bound to it does not fail. It reaches PLAYING, SRT
// connects, every lamp goes green and the switcher receives silence — the
// permanent-false-green class this codebase is organised against. There is no
// symptom to notice and no error to classify, so the refusal has to be at
// construction, and it has to be in BOTH twins or Gate A accepts what the card
// does not.
func TestNewRefusesASendPipelineWithNothingBehindIt(t *testing.T) {
	if _, err := New(CaptureSet{}); err == nil {
		t.Fatal("New built a send pipeline over an empty capture set. Nothing downstream would " +
			"report it: the pipeline reaches PLAYING, the sink connects and the feed carries zero " +
			"bytes with every indicator green")
	}
}

// TestEverySendSessionAfterTheFirstStillCarriesMedia is the zero-byte second
// session, asserted against the SHIPPED Start rather than against NewSend.
//
// It is named in PLAN.md step 5 so that it cannot be quietly dropped, and the two
// halves it pins are the two the seam can lose silently.
//
// THE ARMING. gstproxysink.c resets sent_stream_start/sent_caps only on
// READY->PAUSED, so in an always-live capture pipeline every consumer AFTER THE
// FIRST receives no STREAM_START, no CAPS and no SEGMENT — measured 1,133,076
// bytes on cycle 1 and 0 bytes on cycles 2 and 3, with SRT connected and every
// lamp green. Nothing downstream reports it: proxysink returns GST_FLOW_OK
// unconditionally. So the assertion is a COUNT, one arming per session, and not
// "the first one worked".
//
// THE RELEASE. Stop has to give the claim back, or the second START is refused
// with ErrSeamBusy over a session that no longer exists — which is a feed that
// will not start rather than a feed that carries nothing, but it is the same
// mechanism failing in the other direction and one test should catch both.
func TestEverySendSessionAfterTheFirstStillCarriesMedia(t *testing.T) {
	set := stubSeatCapture(t)
	commentary, ok := set.Commentary.(*StubCapture)
	if !ok {
		t.Fatalf("the commentary capture is %T, want the Gate A stub", set.Commentary)
	}

	for cycle := 1; cycle <= 3; cycle++ {
		p, err := New(set)
		if err != nil {
			t.Fatalf("cycle %d: New: %v", cycle, err)
		}
		if err := p.Start(SendOpts{}); err != nil {
			t.Fatalf("cycle %d: Start: %v. A second send session refused the seam, which means "+
				"the previous Stop did not release it", cycle, err)
		}
		if got := commentary.Armings(); got != cycle {
			t.Fatalf("after send session %d the commentary seam had been armed %d times, want "+
				"%d. A session that skips the arming does not fail: SRT connects, the lamp goes "+
				"green and the switcher receives silence", cycle, got, cycle)
		}
		if err := p.Stop(); err != nil {
			t.Fatalf("cycle %d: Stop: %v", cycle, err)
		}
	}
}

// TestASecondSendPipelineIsRefusedWhileTheFirstIsRunning is the other half of the
// single-consumer rule, at the level the application holds.
//
// A second proxysrc attaching to a live proxysink does not fail — it SILENTLY
// STEALS THE STREAM AND KILLS THE FIRST, measured, consumer A stopped dead at
// 5.994 s the instant consumer B attached at 6.007 s, with nothing on either bus
// and both pipelines still reporting PLAYING. There is no refusal inside the
// element, so this one is the refusal.
func TestASecondSendPipelineIsRefusedWhileTheFirstIsRunning(t *testing.T) {
	set := stubSeatCapture(t)

	first, err := New(set)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := first.Start(SendOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer first.Stop()

	second, err := New(set)
	if err != nil {
		t.Fatalf("New (second): %v", err)
	}
	err = second.Start(SendOpts{})
	if err == nil {
		t.Fatal("a second send pipeline took a seam the first still holds. On the real element " +
			"that does not fail: it steals the stream and takes the running feed off air with " +
			"nothing on either bus")
	}
	if !errors.Is(err, ErrSeamBusy) {
		t.Errorf("the refusal does not wrap ErrSeamBusy: %v", err)
	}
}

// TestACaptureWillNotGoToNullUnderARunningSendPipeline is the teardown ORDER,
// enforced rather than documented.
//
// Taking a device to NULL underneath a bound proxysrc is measured silent in every
// direction: 0 buffers, no EOS, no ERROR and no WARNING on either bus, the send
// pipeline still PLAYING and SRT still connected. The refusal is what turns that
// into a caller error at the moment it is made.
func TestACaptureWillNotGoToNullUnderARunningSendPipeline(t *testing.T) {
	set := stubSeatCapture(t)

	p, err := New(set)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Start(SendOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := set.Commentary.Stop(); err == nil {
		t.Fatal("the commentary capture went to NULL under a running send pipeline. Nothing " +
			"downstream would report it — the send pipeline stays PLAYING, SRT stays connected " +
			"and the switcher receives silence")
	} else if !errors.Is(err, ErrSeamBusy) {
		t.Errorf("the refusal does not wrap ErrSeamBusy: %v", err)
	}

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// And once the seam is released it goes down, which is the half that proves
	// the refusal is a sequencing rule and not a deadlock.
	if err := set.Commentary.Stop(); err != nil {
		t.Errorf("the commentary capture would not stop after the send pipeline had: %v", err)
	}
}

func TestInitIsANoOp(t *testing.T) {
	if err := Init(`C:\Program Files\WSLComms`); err != nil {
		t.Fatalf("Init: %v", err)
	}
	dir, inited := StubAppDir()
	if !inited || dir != `C:\Program Files\WSLComms` {
		t.Errorf("StubAppDir() = %q, %v", dir, inited)
	}
}

// ---------------------------------------------------------------------------
// RemoveSink — the contract change the coordinator made after WP-0.
// ---------------------------------------------------------------------------

// TestRemoveSinkDetachesWithoutInstalling is the behaviour specification
// section 6.2 depends on: DRAINING must give the socket back BEFORE the backoff
// starts, or the backoff elapses while M2L-X still sees us as its one permitted
// peer and the retry lands inside the re-accept refusal window it was sized to
// clear.
func TestRemoveSinkDetachesWithoutInstalling(t *testing.T) {
	p := NewStubPipeline(stubSeatCapture(t))
	if err := p.Start(SendOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.ReplaceSink(SinkOpts{Host: "m2lx.example", Port: 9001}); err != nil {
		t.Fatalf("ReplaceSink: %v", err)
	}
	if _, ok := p.AttachedSink(); !ok {
		t.Fatal("no sink attached after ReplaceSink")
	}

	if err := p.RemoveSink(); err != nil {
		t.Fatalf("RemoveSink: %v", err)
	}
	if _, ok := p.AttachedSink(); ok {
		t.Error("RemoveSink left a sink attached")
	}
	if p.State() != StubStateRunning {
		t.Errorf("state after RemoveSink = %q, want %q: everything upstream stays in PLAYING",
			p.State(), StubStateRunning)
	}
	if c := p.Counters(); c.SinkRemovals != 1 {
		t.Errorf("SinkRemovals = %d, want 1", c.SinkRemovals)
	}
}

// TestRemoveSinkIsIdempotent pins the property that lets the reconnect loop
// call RemoveSink unconditionally on entry to DRAINING without first asking
// whether a sink is installed — including after a connect attempt that failed.
func TestRemoveSinkIsIdempotent(t *testing.T) {
	p := NewStubPipeline(stubSeatCapture(t))
	if err := p.Start(SendOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// No sink has ever been installed.
	if err := p.RemoveSink(); err != nil {
		t.Fatalf("RemoveSink with nothing attached: %v", err)
	}
	if err := p.RemoveSink(); err != nil {
		t.Fatalf("second RemoveSink: %v", err)
	}
	if p.State() != StubStateRunning {
		t.Errorf("state = %q, want %q", p.State(), StubStateRunning)
	}
	if c := p.Counters(); c.SinkRemovals != 2 {
		t.Errorf("SinkRemovals = %d, want 2: the no-op calls must still be counted", c.SinkRemovals)
	}
}

// TestRemoveSinkAfterStopFails keeps RemoveSink's stopped-pipeline behaviour in
// step with ReplaceSink's and ForceKeyUnit's, so a caller shutting down cannot
// tell them apart.
func TestRemoveSinkAfterStopFails(t *testing.T) {
	p := NewStubPipeline(stubSeatCapture(t))
	if err := p.Start(SendOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := p.RemoveSink(); err == nil {
		t.Error("RemoveSink after Stop must fail")
	}
}

// TestRemoveSinkThenReplaceSinkReconnects walks the whole specification section
// 6.2 cycle on the stub: CONNECTED, DRAINING (RemoveSink), BACKOFF, CONNECTING
// (ReplaceSink). It is the sequence internal/sender is required to perform.
func TestRemoveSinkThenReplaceSinkReconnects(t *testing.T) {
	p := NewStubPipeline(stubSeatCapture(t))
	if err := p.Start(SendOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sink := SinkOpts{Host: "m2lx.example", Port: 9001}
	if err := p.ReplaceSink(sink); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	if err := p.RemoveSink(); err != nil {
		t.Fatalf("RemoveSink: %v", err)
	}
	if err := p.ReplaceSink(sink); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if p.State() != StubStateSinkAttached {
		t.Errorf("state = %q, want %q", p.State(), StubStateSinkAttached)
	}
	c := p.Counters()
	if c.SinkRemovals != 1 || c.SinksAttached != 2 {
		t.Errorf("counters = %+v, want 1 removal and 2 attachments", c)
	}
}

// ---------------------------------------------------------------------------
// The render-endpoint refusal and the pipeline-fatal latch (the loopback
// defect's Gate A half — see device_id.go for the field failure).
// ---------------------------------------------------------------------------

// TestStubDeviceListNamespacesAreContract pins the property the file headers
// of gst_stub.go and return_stub.go now promise: every fake capture id is in
// the capture namespace and every fake playback id is in the render
// namespace, per the classifier both twins share. A stub device that failed
// this would let a wiring bug — the headphone dropdown's value reaching
// CaptureOpts.AudioDeviceID, or vice versa — pass at Gate A and surface in a
// commentary booth.
// The capture half is scoped to the NATIVE entries. A DeckLink persistent-id
// is a bare gint64 rendered as decimal and belongs to neither namespace by
// construction — device_id.go's classifier is a POSITIVE identification in both
// directions precisely so that an id it does not recognise is never refused —
// so asserting it into the capture namespace would be asserting a falsehood
// about the real provider's data. What must remain true of EVERY entry, kind
// regardless, is the second clause: nothing in the input list may classify as a
// render endpoint, because that is the wiring bug this test exists to catch.
func TestStubDeviceListNamespacesAreContract(t *testing.T) {
	for _, d := range defaultStubDevices {
		if NormaliseDeviceKind(d.Kind) == KindNative && !IsCaptureEndpointID(d.ID) {
			t.Errorf("defaultStubDevices %q: %s does not classify as a capture endpoint", d.Name, d.ID)
		}
		if IsRenderEndpointID(d.ID) {
			t.Errorf("defaultStubDevices %q: %s classifies as a RENDER endpoint; the stub's own "+
				"dropdown data would be refused by Start", d.Name, d.ID)
		}
	}
	for _, d := range defaultStubOutputDevices {
		if IsCaptureEndpointID(d.ID) {
			t.Errorf("defaultStubOutputDevices %q: %s classifies as a CAPTURE endpoint", d.Name, d.ID)
		}
		if !IsRenderEndpointID(d.ID) {
			t.Errorf("defaultStubOutputDevices %q: %s does not classify as a render endpoint, so a "+
				"test feeding it to Start would not trip the refusal the real build applies", d.Name, d.ID)
		}
	}
}

// TestMarkFatalLatchesEveryReplaceSink models the contract documented on
// Pipeline.ReplaceSink: a pipeline-fatal error latches for the life of the
// pipeline, outranks the ordinary connection-failure ladder, and never
// clears — recovery is Stop, New, Start.
func TestMarkFatalLatchesEveryReplaceSink(t *testing.T) {
	p := NewStubPipeline(stubSeatCapture(t))
	if err := p.Start(SendOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sink := SinkOpts{Host: "m2lx.example", Port: 9001}
	if err := p.ReplaceSink(sink); err != nil {
		t.Fatalf("ReplaceSink before the fatal: %v", err)
	}

	boom := errors.New("gst: asrc: Internal data stream error")
	p.MarkFatal(boom)

	if f := p.Fatal(); !errors.Is(f, ErrPipelineFatal) || !errors.Is(f, boom) {
		t.Fatalf("Fatal() = %v, want a wrap of both ErrPipelineFatal and the cause", f)
	}

	// The latch outranks the FailNextSinks ladder: a queued connection
	// failure must not be what the caller sees, because "the network refused
	// again" is exactly the misdiagnosis the sentinel exists to end.
	other := errors.New("connection refused")
	p.FailNextSinks(1, other)
	for i := range 3 {
		err := p.ReplaceSink(sink)
		if !errors.Is(err, ErrPipelineFatal) {
			t.Fatalf("ReplaceSink attempt %d after MarkFatal = %v, want errors.Is(_, ErrPipelineFatal)", i, err)
		}
		if errors.Is(err, other) {
			t.Fatalf("ReplaceSink attempt %d returned the FailNextSinks error %v ahead of the latch", i, err)
		}
	}

	// Only the first mark wins, as in the real markFatal: the first error is
	// the one that explains the failure.
	p.MarkFatal(errors.New("a later, less informative error"))
	if f := p.Fatal(); !errors.Is(f, boom) {
		t.Errorf("a second MarkFatal replaced the first: %v", f)
	}

	// Start, RemoveSink and ForceKeyUnit are untouched by the latch.
	if err := p.RemoveSink(); err != nil {
		t.Errorf("RemoveSink after MarkFatal: %v", err)
	}
	if err := p.ForceKeyUnit(); err != nil {
		t.Errorf("ForceKeyUnit after MarkFatal: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The Windows environment-variable behaviour Init depends on.
// ---------------------------------------------------------------------------

// TestEmptyEnvVarSurvivesOnWindows records a MEASUREMENT that contradicts a
// widely repeated claim, because doInit's comment cites it.
//
// The claim, made against the original GST_PLUGIN_SYSTEM_PATH_1_0="" line: Go's
// os.Setenv calls SetEnvironmentVariableW, the Win32 environment block cannot
// represent an empty value, so the variable is deleted, GetEnvironmentVariableW
// afterwards returns ERROR_ENVVAR_NOT_FOUND, GLib's g_getenv sees it as unset,
// and gstregistry.c falls through to the compiled-in system directories.
//
// It does not reproduce. The variable survives with an empty value, it is
// present in os.Environ() as the literal entry "NAME=", and it is inherited by
// a child process. GetEnvironmentVariableW therefore did not report
// ERROR_ENVVAR_NOT_FOUND. The folklore belongs to the wrappers, not to Win32:
// cmd.exe's `set FOO=` passes NULL, the CRT's _putenv("FOO=") deletes, and
// .NET's GetEnvironmentVariable normalises empty to null. Go calls the API
// directly and none of those apply.
//
// doInit still sets a path rather than "", for a different and honest reason
// stated there — GLib's own empty-to-NULL mapping is the one link that cannot
// be tested without GLib. This test exists so that the disproven claim is not
// re-adopted from memory, and so that a future Windows or Go release that
// changes the behaviour is caught here rather than at a facility.
func TestEmptyEnvVarSurvivesOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skipf("the deliverable is Windows-only; %s says nothing about SetEnvironmentVariableW",
			runtime.GOOS)
	}
	const name = "WSLCOMMS_EMPTY_ENV_PROBE"
	t.Setenv(name, "placeholder")
	if err := os.Setenv(name, ""); err != nil {
		t.Fatalf("Setenv: %v", err)
	}

	value, present := os.LookupEnv(name)
	if value != "" {
		t.Fatalf("LookupEnv(%s) = %q, want the empty string", name, value)
	}
	if !present {
		t.Errorf("an empty %s was DELETED by SetEnvironmentVariableW. That is the behaviour "+
			"the original claim asserted, and it is not what this machine did when the "+
			"comment in doInit was written. Re-read that comment before changing anything: "+
			"setting the variable to the bundle path is correct under either behaviour.", name)
	}

	var found string
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, name+"=") {
			found = entry
		}
	}
	if found != name+"=" {
		t.Errorf("os.Environ() entry for %s is %q, want %q: the empty value is not being "+
			"carried in the environment block a child process would inherit",
			name, found, name+"=")
	}
}

// ---------------------------------------------------------------------------
// Source-level guards on gst_cgo.go.
//
// READ THIS BEFORE ADDING TO OR TRUSTING THESE.
//
// gst_cgo.go carries //go:build cgo and CANNOT BE COMPILED at Gate A — no MinGW
// gcc, no GStreamer. Nothing in this test binary links a line of it. So these
// are not behavioural tests and they are not a substitute for one: they read
// the file as text, parse it with go/parser, and assert structural properties
// of the source.
//
// That is worth doing for exactly two reasons.
//
//  1. Nothing else at Gate A even proves gst_cgo.go is valid Go. A missing
//     brace sits in the repository until somebody opens Gate B.
//  2. Several of the defects this file has already had were structural and
//     invisible to a reviewer: a route cleared by `defer` instead of before the
//     gate opened, a method missing from the interface implementation, a
//     log.Printf on a streaming thread. Each is one AST query.
//
// Every guard below names the failure it is guarding against. If one starts
// failing because the code was legitimately restructured, change the guard —
// but read the reason first, because each of them was a live bug.
// ---------------------------------------------------------------------------

// cgoSourceFile is the file these guards read.
const cgoSourceFile = "gst_cgo.go"

// contractSourceFile is the frozen interface file the guards check against.
const contractSourceFile = "gst.go"

// The per-platform halves of gst_cgo.go, added by the macOS port.
//
// Reading them here is not optional politeness. Several of the guards below
// were written when all of this knowledge lived in one file, and the code they
// guard has MOVED — the wasapi2 loopback filter and the endpoint-id namespace
// checks now sit in deviceprovider_windows.go, and the capture source and AAC
// encoder factory names in elements_windows.go / elements_darwin.go. A guard
// that kept reading only gst_cgo.go would have gone quietly green while
// checking nothing at all, which is the one failure mode a source guard must
// not have. Parsing is by filename and ignores build tags, so every guard below
// checks BOTH ports from whichever host Gate A runs on.
const (
	windowsProviderSourceFile = "deviceprovider_windows.go"
	darwinProviderSourceFile  = "deviceprovider_darwin.go"
	windowsElementsSourceFile = "elements_windows.go"
	darwinElementsSourceFile  = "elements_darwin.go"
)

// parseSource parses one file of this package WITHOUT comments, so that the
// rendered function bodies the guards search contain code only. A comment that
// discusses `p.route.Store(nil)` must not be able to satisfy a guard looking
// for the call.
func parseSource(t *testing.T, name string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, nil, 0)
	if err != nil {
		t.Fatalf("%s does not parse as Go: %v", name, err)
	}
	return fset, file
}

// funcBody renders the comment-free source of a named function or method.
// receiver is "" for a package-level function.
func funcBody(t *testing.T, fset *token.FileSet, file *ast.File, receiver, name string) string {
	t.Helper()
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name {
			continue
		}
		if receiverName(fn) != receiver {
			continue
		}
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, fn); err != nil {
			t.Fatalf("rendering %s: %v", name, err)
		}
		return buf.String()
	}
	t.Fatalf("%s has no function %s (receiver %q)",
		fset.Position(file.Pos()).Filename, name, receiver)
	return ""
}

// startSequence is Start's body followed by startBuiltLocked's, as one text.
//
// EVERY GUARD BELOW THAT USED TO READ Start ALONE READS THIS INSTEAD, and the
// reason is a refactor rather than a preference. Start used to be one function:
// the option checks, then the build, then NULL to PLAYING. It is now two,
// because a preview sink that exists and then will not START fails inside the
// state change — which is not a bus error, so none of the sparing that protects
// the confidence monitor everywhere else applies — and the only answer is to
// build again without it. That needs the build callable twice, so it moved into
// cgoPipeline.startBuiltLocked and Start kept the option checks and the retry.
//
// READING THE TWO AS ONE TEXT IS EXACT, not a convenience. Start runs its
// prologue and then calls startBuiltLocked, so concatenating them in that order
// is the order the statements execute in, which is the only property the
// ordering guards below assert. Every one of them compares positions across the
// seam — the OnLevels store in the first half against gst_parse_launch in the
// second, the render-endpoint refusal against the same — and gets the same
// answer it got when both halves were in one body.
//
// READING BOTH HALVES IS ALSO MANDATORY RATHER THAN TIDY. Two of the guards
// below — the DeckLink `connection` prohibition and the "Start must not
// enumerate devices" rule — would still have PASSED against Start alone after
// the extraction, because the code they forbid had moved into the half they
// stopped reading. A guard that passes because it can no longer see the code is
// worse than one that fails, and the connection rule is the one hardware rule in
// this package that cannot be undone by restarting anything.
//
// The retry block itself sits in the first half, after the call. Nothing in it
// matches any guard below; if a future edit puts something there that does, read
// this comment before assuming the position it reports is wrong.
func startSequence(t *testing.T, fset *token.FileSet, file *ast.File) string {
	t.Helper()
	return funcBody(t, fset, file, "cgoPipeline", "Start") + "\n" +
		funcBody(t, fset, file, "cgoPipeline", "startBuiltLocked")
}

// captureStartSequence is the CAPTURE pipeline's build, as one text: cgoCapture's
// Start followed by buildLocked.
//
// EVERY GUARD THAT USED TO READ startSequence FOR A DEVICE READS THIS INSTEAD,
// and the move is exactly the move the code made. When one pipeline carried both
// capture and send, the slate path, the two persistent-ids, the render-endpoint
// refusal, the mix matrix, the cough mute and the signal watchdog were all in
// cgoPipeline.startBuiltLocked. They are now in cgoCapture.buildLocked, upstream
// of the proxysink, and a send pipeline has no device in it at all.
//
// The comment on startSequence says a guard that passes because it can no longer
// see the code it forbids is worse than one that fails. That is precisely what
// re-pointing these guards prevents, and it has now happened twice to the same
// set of rules: once when the build was extracted from Start, and once when the
// capture was extracted from the pipeline. Read that comment before moving any
// of them again.
func captureStartSequence(t *testing.T) string {
	t.Helper()
	fset, file := parseSource(t, captureCgoSourceFile)
	return funcBody(t, fset, file, "", "NewCapture") + "\n" +
		funcBody(t, fset, file, "cgoCapture", "Start") + "\n" +
		funcBody(t, fset, file, "cgoCapture", "buildLocked")
}

// stubSeatCapture is the ordinary seat's capture layer — a slate picture and a
// platform microphone, as two independent pipelines — built and started, and
// returned as the set a stub send pipeline is minted over.
//
// It exists because a send pipeline is MINTED ONLY BY CAPTURE. Every test below
// that used to say NewStubPipeline() now has to say what is behind it, and that
// is the point rather than a cost: "a send pipeline with no device behind it"
// does not fail at runtime, it reaches PLAYING and carries zero bytes with every
// lamp green, so the type makes it unconstructible instead.
//
// The captures are stopped through t.Cleanup, which runs
// last-registered-first — so a send pipeline the test stopped itself, or one
// registered for cleanup after this call, is already gone by the time
// CapturePipeline.Stop runs and refuses a still-held seam.
func stubSeatCapture(t *testing.T) CaptureSet {
	t.Helper()

	const endpoint = "{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}"
	var set CaptureSet
	for _, legs := range PlanCapture(CaptureSources{AudioDeviceID: endpoint}) {
		c, err := NewStubCapture(CaptureOpts{
			Legs:          legs,
			SlatePath:     "slate.png",
			AudioDeviceID: endpoint,
		})
		if err != nil {
			t.Fatalf("building the %s capture: %v", legs, err)
		}
		t.Cleanup(func() { _ = c.Stop() })
		if err := c.Start(); err != nil {
			t.Fatalf("starting the %s capture: %v", legs, err)
		}
		if legs.Picture != PictureNone {
			set.Picture = c
		}
		if legs.Commentary != CommentaryNone {
			set.Commentary = c
		}
	}
	return set
}

// receiverName returns the type name of a method's receiver, without the
// pointer star, or "" for a package-level function.
func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	expr := fn.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// TestCgoSourceParses is the only check at Gate A that gst_cgo.go is valid Go
// at all. Every other guard below depends on it, and so does anybody who edits
// the file without a GStreamer to compile against.
func TestCgoSourceParses(t *testing.T) {
	_, file := parseSource(t, cgoSourceFile)
	if file.Name.Name != "gst" {
		t.Fatalf("package name is %q, want gst", file.Name.Name)
	}
}

// TestCgoPipelineImplementsEveryPipelineMethod is the guard that would have
// caught RemoveSink being added to the Pipeline interface and to the stub but
// not to the real implementation.
//
// gst_cgo.go has `var _ Pipeline = (*cgoPipeline)(nil)`, which is the right
// assertion and is completely useless here: it is only checked by a compiler
// that has cgo, GStreamer and MinGW. Until Gate B opens, a Pipeline method
// added by the coordinator can sit unimplemented in the real pipeline
// indefinitely, and the first symptom is a build failure on the build host on
// the day someone needs a binary.
func TestCgoPipelineImplementsEveryPipelineMethod(t *testing.T) {
	_, contract := parseSource(t, contractSourceFile)

	var methods []string
	ast.Inspect(contract, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name.Name != "Pipeline" {
			return true
		}
		iface, ok := spec.Type.(*ast.InterfaceType)
		if !ok {
			return true
		}
		for _, field := range iface.Methods.List {
			for _, name := range field.Names {
				methods = append(methods, name.Name)
			}
		}
		return false
	})
	if len(methods) == 0 {
		t.Fatalf("found no methods on the Pipeline interface in %s", contractSourceFile)
	}

	_, cgoFile := parseSource(t, cgoSourceFile)
	implemented := make(map[string]bool)
	for _, decl := range cgoFile.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && receiverName(fn) == "cgoPipeline" {
			implemented[fn.Name.Name] = true
		}
	}
	for _, m := range methods {
		if !implemented[m] {
			t.Errorf("Pipeline.%s is declared in %s but *cgoPipeline does not implement it; "+
				"the package will not compile at Gate B", m, contractSourceFile)
		}
	}
}

// TestReplaceSinkClearsTheRouteBeforeOpeningTheGate is the guard on the
// false-green defect.
//
// `defer p.route.Store(nil)` runs after the function returns, so the route stays
// installed through the final drain, through the gate opening and through the
// success log. A GST_ELEMENT_ERROR from srtout-N in that window is matched by
// name in onBusMessage, pushed into the route's one-slot channel, and read by
// nobody: it reaches neither Errors() nor p.fatal. ReplaceSink returns nil,
// internal/sender goes CONNECTED, the lamp goes green, no reconnect is ever
// triggered, and commentary is off air with every indicator healthy. srtsink
// accepting the socket and then failing its first write is M2L-X's ordinary
// one-peer behaviour, not an exotic case.
//
// The fix is an ORDER, which is why this is checked as one: clear the route,
// then drain, then open the gate.
func TestReplaceSinkClearsTheRouteBeforeOpeningTheGate(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	lines := strings.Split(funcBody(t, fset, file, "cgoPipeline", "ReplaceSink"), "\n")

	// A DEFERRED clear does not count and must not be allowed to satisfy this
	// guard — it is the exact defect. Deferred statements run after the
	// function body has finished, which is after the gate has been opened.
	clear := lastLineMatching(lines, func(s string) bool {
		return s == "p.route.Store(nil)"
	})
	open := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "p.gateClosed.Store(false)")
	})
	drain := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "routeErr(route)") && !strings.HasPrefix(s, "defer ")
	})

	if clear < 0 {
		t.Fatal("ReplaceSink never clears p.route with a plain statement. A deferred clear is " +
			"NOT enough: it runs after the final drain, after the gate has been opened and " +
			"after the success log, so a sink error in that window is pushed into a channel " +
			"nobody reads again and ReplaceSink returns nil on a connection that is already gone")
	}
	if open < 0 {
		t.Fatal("ReplaceSink never opens the gate")
	}
	if drain < 0 {
		t.Fatal("ReplaceSink never drains the route")
	}
	if clear > open {
		t.Error("ReplaceSink opens the gate before clearing p.route: a sink error in that " +
			"window is swallowed and the caller is told the connection succeeded")
	}
	if drain < clear {
		t.Error("ReplaceSink's last drain of the route happens before the route is cleared; " +
			"an error arriving between the two reaches neither Errors() nor the caller")
	}
	if drain > open {
		t.Error("ReplaceSink drains the route after opening the gate")
	}
}

// lastLineMatching returns the index of the last line whose leading and
// trailing whitespace-trimmed text satisfies pred, or -1.
//
// Guards that care about STATEMENT ORDER must work on whole lines rather than
// byte offsets, because `defer x()` and `x()` contain the same substring and
// mean opposite things about when the call happens.
func lastLineMatching(lines []string, pred func(string) bool) int {
	found := -1
	for i, line := range lines {
		if pred(strings.TrimSpace(line)) {
			found = i
		}
	}
	return found
}

// TestReplaceSinkRechecksFatalBeforeSucceeding guards the disagreement between
// the synchronous and asynchronous answers: fatal is checked on entry, so an
// error posted by mpegtsmux during the swap gives the caller a nil return
// alongside a pipeline-fatal error on Errors().
func TestReplaceSinkRechecksFatalBeforeSucceeding(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	body := funcBody(t, fset, file, "cgoPipeline", "ReplaceSink")

	if n := strings.Count(body, "p.fatalError()"); n < 2 {
		t.Fatalf("ReplaceSink checks p.fatalError() %d time(s), want at least 2: once on entry "+
			"and once before promising success", n)
	}
	last := strings.LastIndex(body, "p.fatalError()")
	open := strings.Index(body, "p.gateClosed.Store(false)")
	if open < 0 {
		t.Fatal("ReplaceSink never opens the gate")
	}
	if last > open {
		t.Error("the final p.fatalError() check happens after the gate is opened")
	}
}

// TestRearmQueueRestoresStickyEvents guards the sticky-event destruction.
//
// gst_pad_set_active(pad, FALSE) reaches post_activate() with GST_PAD_MODE_NONE,
// which calls remove_events(pad) and drops EVERY sticky event on the pad.
// Nothing puts them back: mpegtsmux sends STREAM_START, CAPS and SEGMENT once,
// and gst_pad_link's mark_event_not_received re-marks a list that is now empty.
// The fresh srtsink then receives buffers with no segment and gstbasesink
// assumes timestamps start from zero — the exact fault this package exists to
// prevent. rearmQueueLocked runs on every genuine mid-match reconnect, because
// the in-flight buffer is inside srtsink's render() when the gate closes.
func TestRearmQueueRestoresStickyEvents(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	body := funcBody(t, fset, file, "cgoPipeline", "rearmQueueLocked")

	deactivate := strings.Index(body, "SetActive(false)")
	if deactivate < 0 {
		// A re-arm that does not deactivate the pad does not destroy the sticky
		// events, so there is nothing to restore.
		return
	}
	snapshot := strings.Index(body, "stickyEventsOf(")
	restore := strings.Index(body, "restoreStickyEvents(")
	if snapshot < 0 || restore < 0 {
		t.Fatal("rearmQueueLocked deactivates srtq:src but does not snapshot and restore its " +
			"sticky events; the next srtsink will be told timestamps start from zero")
	}
	if snapshot > deactivate {
		t.Error("rearmQueueLocked snapshots the sticky events after deactivating the pad, " +
			"by which time remove_events has already destroyed them")
	}
	reactivate := strings.LastIndex(body, "SetActive(true)")
	if reactivate >= 0 && restore < reactivate {
		t.Error("rearmQueueLocked restores the sticky events before reactivating the pad")
	}
}

// TestTeardownDoesNotDetachTheBusSyncHandler guards a process kill.
//
// gst_bus_post reads the sync handler under GST_OBJECT_LOCK, unlocks, then calls
// it. SetSyncHandler(nil) runs the GDestroyNotify that unregisters the Go
// closure, so a streaming thread that already read the pointer enters go-gst's
// exported callback, finds no closure and executes panic("callback not found")
// inside an //export'ed cgo function. A Go panic cannot unwind through C: the
// process dies. It is narrow, but it is at Stop — the end of every match and
// every mid-match device change.
func TestTeardownDoesNotDetachTheBusSyncHandler(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	body := funcBody(t, fset, file, "cgoPipeline", "teardownLocked")

	if strings.Contains(body, "SetSyncHandler(nil)") {
		t.Fatal("teardownLocked detaches the bus sync handler. A streaming thread already " +
			"inside gst_bus_post then panics in an exported cgo callback and kills the process. " +
			"Silence the handler with a flag instead.")
	}
	if !strings.Contains(body, "p.busSilenced.Store(true)") {
		t.Fatal("teardownLocked neither detaches nor silences the bus handler; " +
			"messages will be processed against a pipeline that is going to NULL")
	}
}

// TestBusHandlerDoesNotLogOnTheStreamingThread guards streaming-thread latency.
//
// onBusMessage runs on whichever GStreamer thread posted the message. Go's log
// package takes a process-global mutex and writes synchronously to stderr, so a
// warning storm — which is what a marginal SRT link produces — serialises every
// streaming thread in the pipeline behind one file handle, at the one moment
// the capture chain must not acquire latency.
func TestBusHandlerDoesNotLogOnTheStreamingThread(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	body := funcBody(t, fset, file, "cgoPipeline", "onBusMessage")

	if strings.Contains(body, "log.Print") {
		t.Fatal("onBusMessage calls into the log package on a GStreamer streaming thread; " +
			"hand the line to the logWarnings goroutine instead")
	}
	if !strings.Contains(body, "p.deliverWarning(") {
		t.Fatal("onBusMessage no longer routes warnings through deliverWarning; " +
			"they are either being dropped or being logged inline")
	}
}

// TestPipelineDescriptionHasNoCapsfilterAboveTheResampler guards a failure to
// start twenty minutes before kick-off.
//
// A capsfilter pinning rate=48000,channels=2 between wasapi2src and
// audioconvert sits UPSTREAM of the resampler that exists to produce exactly
// that, where it cannot convert anything and can only refuse. wasapi2src in
// shared mode can only produce its endpoint's mix format, and Dante Virtual
// Soundcard is commonly run at 44.1 or 96 kHz. The capsfilter that matters is
// the one below audioresample, pinning what enters mfaacenc.
func TestCaptureDescriptionHasNoCapsfilterAboveTheResampler(t *testing.T) {
	body := captureDescriptionSource(t)

	if strings.Contains(body, "! audio/x-raw,rate=") {
		t.Fatal("the audio branch pins a sample rate upstream of audioresample; " +
			"a DVS endpoint at 44.1 or 96 kHz will fail negotiation at Start")
	}
	// audioconvert acquired a NAME when the channel map landed — the mapping
	// mechanism needs a handle on the element whose mix-matrix carries it — so
	// the two elements are checked as a sequence rather than as one literal.
	// The property being asserted is unchanged: convert and resample whatever
	// the endpoint gives us, in that order, with nothing pinned above them.
	convert := strings.Index(body, "audioconvert name=")
	resample := strings.Index(body, "audioresample")
	if convert < 0 || resample < 0 || convert > resample {
		t.Fatal("the audio branch no longer converts and resamples whatever the endpoint gives us")
	}
	// THE CAPSFILTER THAT MATTERS IS NOW THE SEAM CONTRACT, and it is rendered
	// from seamAudioCaps rather than written out — because the SEND side asserts
	// the identical string after aproxsrc, and two spellings of one contract is
	// how the two sides drift into a silent wrong encode. capturedesc_cgo_test.go
	// checks the two rendered strings against each other; this checks that the
	// capture side still pins anything at all.
	if !strings.Contains(body, "seamAudioCaps") {
		t.Fatal("nothing pins what crosses the seam any more; the send side's capsfilter after " +
			"aproxsrc would then be asserting a contract the capture side does not keep")
	}
}

// TestPipelineDescriptionScalesTheSlate guards the next person to export the
// artwork. Without videoscale the slate PNG must be exactly the conform
// target's size or Start fails caps negotiation, and videoconvertscale is
// already a required plugin, so the element is free.
//
// THE SECOND ASSERTION CHANGED WHEN THE CONFORM TARGET BECAME AN OPTION, and
// the reason is worth stating rather than leaving to `git blame`. It used to
// read `strings.Contains(body, "width=1920,height=1080")`. That literal is
// gone from this function on purpose: 1920x1080 is now the DEFAULT, in
// FallbackConformTarget, and pinning it here again would re-assert exactly the
// hardcoding this change removed — an instance configured for 720p50 must get
// 720p50 in its caps. What replaces it is stronger, not weaker: this guard
// checks that the caps come from the parameter, and
// TestFallbackConformTargetReproducesTheShippedCaps checks — behaviourally, not
// by reading source — that the default renders the exact string that used to
// be written here.
func TestPipelineDescriptionScalesTheSlate(t *testing.T) {
	slate := slateLegSource(t)

	if !strings.Contains(slate, "videoscale") {
		t.Fatal("the slate branch has no videoscale; artwork that is not exactly the conform " +
			"target's size fails caps negotiation at Start with no diagnostic naming the size")
	}
	for _, want := range []string{"conform.spatialCaps()", "conform.temporalCaps()"} {
		if !strings.Contains(slate, want) {
			t.Errorf("the slate leg no longer renders %s. If the caps have gone back to being "+
				"written out, they are written out for ONE switcher configuration, and M2L-X "+
				"refuses any source that is not in the format it is configured for", want)
		}
	}
	// The hardcoding check is made against the WHOLE function rather than the
	// slate leg alone, deliberately: the live capture leg conforms to the same
	// target and would be the easier of the two to write a literal into, because
	// a reader who has just learned what a card produces is thinking about 1080
	// rather than about the switcher's configuration.
	body := captureDescriptionSource(t)
	for _, literal := range []string{"width=1920", "height=1080", "framerate=50"} {
		if strings.Contains(body, literal) {
			t.Errorf("captureDescription contains the literal %q again; the conform target is "+
				"an option and 1080p50 belongs in FallbackConformTarget, where a facility that is "+
				"not configured for it can be given something else", literal)
		}
	}
}

// TestPipelineDescriptionScalesBeforeFreezing guards the reorder, which is the
// change in this area with the best ratio of value to risk and the only one
// that lands on the ON-AIR Windows build.
//
// videoconvert and videoscale used to sit AFTER imagefreeze, converting and
// scaling the same still picture fifty times a second for the length of a
// match. Above it they do the work once, on the single frame pngdec produces.
// Measured on macOS arm64 / GStreamer 1.26.10 on 2026-08-15, 500 frames at
// 50 fps into fakesink: 2.85 s of CPU (1920x1080 slate) and 3.02 s (1920x1200)
// in the old order, 0.04 s and 0.05 s in the new one, with byte-identical NV12
// output in both sizes. It is a pure reordering.
//
// It is guarded because it is exactly the kind of edit a later reader
// "tidies" back — imagefreeze reads naturally as the first element of a slate
// leg — and nothing else at Gate A would notice: the change is invisible to
// every behavioural test in this package and costs only CPU, which is the one
// resource a commentary machine has spare right up until it does not.
func TestPipelineDescriptionScalesBeforeFreezing(t *testing.T) {
	body := slateLegSource(t)

	convert := strings.Index(body, "videoconvert")
	scale := strings.Index(body, "videoscale name=")
	spatial := strings.Index(body, "conform.spatialCaps()")
	freeze := strings.Index(body, "imagefreeze is-live=true")
	temporal := strings.Index(body, "conform.temporalCaps()")

	if convert < 0 || scale < 0 || freeze < 0 || spatial < 0 || temporal < 0 {
		t.Fatal("the slate leg has been restructured out of the shape this guard reads; " +
			"re-derive it from the new one rather than deleting it")
	}
	if !(convert < freeze && scale < freeze) {
		t.Error("videoconvert/videoscale are back below imagefreeze: the same still picture is " +
			"being converted and scaled fifty times a second, for 2.9 s of CPU per 500 frames " +
			"instead of 0.04 s, to produce byte-identical output")
	}
	if spatial > freeze {
		t.Error("the spatial caps are pinned below imagefreeze, so videoscale has no target to " +
			"scale to and a slate that is not already the conform size fails negotiation")
	}
	if temporal < freeze {
		t.Error("the frame rate is pinned ABOVE imagefreeze. pngdec produces one buffer at no " +
			"frame rate; the rate is what imagefreeze itself decides on its src pad, so asking " +
			"for it upstream fails the whole leg at Start")
	}
}

// ---------------------------------------------------------------------------
// The CAPTURE description's two picture legs.
//
// captureDescription renders one of two picture legs, chosen by legs.Picture,
// and the guards below have to know which half they are reading. slateLegSource
// and captureLegSource are that split, and it is made by TEXT POSITION rather
// than by re-rendering the function, for the same reason every guard in this
// file reads source: capturedesc_cgo.go cannot be compiled at Gate A, so there
// is nothing to call.
//
// THEY READ capturedesc_cgo.go NOW. They used to read pipelineDescription, which
// is deleted: it rendered capture AND send as one string, built at START and
// destroyed at STOP. Every assertion below survived the move — what changed is
// which function is opened and, for the handful noted at their own sites, where
// the leg now ENDS, because the encoder and the muxer are the send pipeline's.
//
// The split is deliberately FRAGILE IN THE SAFE DIRECTION. If the function is
// restructured so the marker is not there, these fail loudly and immediately
// rather than silently returning the whole body and letting a slate assertion
// be satisfied by a line in the capture leg — which is exactly the failure a
// position-based split invites and the one thing it must not do.
// ---------------------------------------------------------------------------

// captureLegMarker is the first token of the capture leg's construction. It is
// the factory constant rather than a literal element name so that renaming the
// element does not silently unsplit the two halves.
const captureLegMarker = "videoCaptureFactory"

// slateLegSource is captureDescription's source ABOVE the capture leg: the slate
// branch and its proxy tail.
func slateLegSource(t *testing.T) string {
	t.Helper()
	body := captureDescriptionSource(t)
	i := strings.Index(body, captureLegMarker)
	if i < 0 {
		t.Fatalf("captureDescription no longer mentions %s, so the slate leg and the live "+
			"capture leg cannot be told apart in its source. Re-derive this split from the new "+
			"shape rather than deleting it: without it a guard written for the slate can be "+
			"satisfied by a line in the capture leg and go green while checking nothing",
			captureLegMarker)
	}
	return body[:i]
}

// captureLegSource is captureDescription's source FROM the capture leg onwards:
// the conform chain, the tee, the broadcast branch and the commentary chain.
func captureLegSource(t *testing.T) string {
	t.Helper()
	body := captureDescriptionSource(t)
	i := strings.Index(body, captureLegMarker)
	if i < 0 {
		t.Fatalf("captureDescription no longer mentions %s; see slateLegSource", captureLegMarker)
	}
	return body[i:]
}

// captureDescriptionSource is the comment-free source of the one function both
// halves come out of.
func captureDescriptionSource(t *testing.T) string {
	t.Helper()
	fset, file := parseSource(t, captureDescSourceFile)
	return funcBody(t, fset, file, "", "captureDescription")
}

// sendDescriptionSource is the comment-free source of the OTHER half of the
// seam: the one string every seat sends through, with two parameters and no
// third.
func sendDescriptionSource(t *testing.T) string {
	t.Helper()
	fset, file := parseSource(t, captureDescSourceFile)
	return funcBody(t, fset, file, "", "sendDescription")
}

// TestCaptureLegNeverSetsTheConnectionProperty is the guard on the one
// hardware rule in this package that cannot be undone by restarting anything.
//
// decklinkvideosrc's `connection` property is NOT a per-pipeline input
// selection. Setting it PERSISTENTLY RECONFIGURES THE CARD and overrides what
// the operator chose in Blackmagic Desktop Video Setup, and it has had to be
// undone by hand twice on this rig. The card's input is chosen in Desktop Video
// Setup; leaving the property unset is what makes the microphone on the card's
// MIC input work at all. When a capture is black or silent the answer is never
// another connection value — which is precisely the intuition a person
// debugging at midnight will have, which is why this is a test and not a
// comment.
//
// It reads BOTH DESCRIPTIONS AND BOTH START SEQUENCES, because those are the two
// places a property can reach an element: the parse string and g_object_set. That
// is not tidiness, it is the whole guard — this test has twice been left reading
// only the place the code USED to be. The g_object_set half moved into
// startBuiltLocked once, and then the whole capture half moved into
// capture_cgo.go and capturedesc_cgo.go; a version that went on reading one
// function would have passed for ever by no longer being able to see the code it
// forbids.
// capturefault_cgo.go is deliberately not covered — it READS the property to
// name it in a no-signal message, which is the opposite operation and is
// somebody else's file.
func TestCaptureLegNeverSetsTheConnectionProperty(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)

	// THE CAPTURE LAYER'S HALVES, added when the description was split in two.
	// The comment above says a version of this test that went on reading one
	// place would pass for ever by no longer being able to see the code it
	// forbids, and splitting one description function into two is exactly that
	// happening again. Both new functions and the new build sequence are named
	// here so that dropping one of them is a compile-time failure of this test
	// rather than a silent loss of coverage.
	descFset, descFile := parseSource(t, captureDescSourceFile)
	capFset, capFile := parseSource(t, captureCgoSourceFile)

	for _, fn := range []struct{ name, body string }{
		{"the send Start sequence", startSequence(t, fset, file)},
		{"captureDescription", funcBody(t, descFset, descFile, "", "captureDescription")},
		{"sendDescription", funcBody(t, descFset, descFile, "", "sendDescription")},
		{"the capture Start sequence",
			funcBody(t, capFset, capFile, "cgoCapture", "Start") + "\n" +
				funcBody(t, capFset, capFile, "cgoCapture", "buildLocked")},
	} {
		if strings.Contains(fn.body, "connection") {
			t.Errorf("%s mentions the DeckLink `connection` property. It is not a per-pipeline "+
				"selection: it persistently reconfigures the CARD and overrides Blackmagic "+
				"Desktop Video Setup, and the owner has had to undo that twice. Delete it",
				fn.name)
		}
	}
}

// TestCaptureLegNamesEveryElementWithTheCapturePrefix is the guard that keeps a
// video fault from taking the commentary off air.
//
// classifyBusError decides fatal-or-recoverable BY ELEMENT NAME, and an element
// left unnamed in the parse string is named by GStreamer — "videoconvert0" —
// which matches no prefix and rejoins the FATAL default. So an unnamed element
// in this leg does not degrade to a frozen picture when a camera is unplugged
// mid-match: it takes the commentary off air, for a video fault, on a pipeline
// whose audio was perfectly healthy. capturefault.go's own guard covers the two
// constants it declares; this covers the conform chain, which is where the
// elements a later change would add actually go.
func TestCaptureLegNamesEveryElementWithTheCapturePrefix(t *testing.T) {
	// The constants are read out of gst_cgo.go AS TEXT rather than referenced,
	// for the reason capturefault.go's own duplication exists: that file is
	// `cgo && !gststub` and its identifiers do not exist in this test binary.
	// Reading the declarations is stronger than mirroring them anyway — it
	// checks the source of truth instead of a copy of it.
	src, err := os.ReadFile(cgoSourceFile)
	if err != nil {
		t.Fatalf("reading %s: %v", cgoSourceFile, err)
	}
	// nameVideoCapQueue IS GONE FROM THIS LIST, and it went with the head queue
	// it named. The broadcast branch's queue is now the SEAM's — nameVideoProxyQueue,
	// declared in seam.go with the leak policy beside it, because the queue in
	// front of every proxysink is one of the three invariants that file exists to
	// hold. Its prefix rule is checked in seam_test.go, against the classifier's
	// vprox entry rather than against the vcap one.
	for _, decl := range []string{
		"nameVideoCapConv", "nameVideoCapDeint", "nameVideoCapScale",
		"nameVideoCapRate", "nameVideoCapTee",
	} {
		m := regexp.MustCompile(decl + `\s*=\s*"([^"]*)"`).FindSubmatch(src)
		if m == nil {
			t.Errorf("%s no longer declares %s as a string constant; the capture leg's elements "+
				"must be named from constants so this guard can check them", cgoSourceFile, decl)
			continue
		}
		if name := string(m[1]); !strings.HasPrefix(name, videoCaptureNamePrefix) {
			t.Errorf("capture leg element %s = %q does not begin with %q, so a bus error from "+
				"it would be classified pipeline-fatal and take the commentary off air over a "+
				"video fault", decl, name, videoCaptureNamePrefix)
		}
	}

	// And every element in the leg must actually be given one of those names,
	// which is the half a constant cannot check on its own.
	body := captureLegSource(t)
	for _, want := range []string{
		"videoconvert name=", "deinterlace name=", "videoscale name=",
		"videorate name=", "tee name=",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the capture leg has an element built without %q. An unnamed element is "+
				"named by GStreamer, matches no prefix, and its failures become fatal", want)
		}
	}
	// The branch's head queue is the seam's and is rendered by videoProxyTail, so
	// the naming rule for it is checked where the tail is written rather than
	// here — but the leg must still USE the tail, or it ends somewhere of its own.
	if !strings.Contains(body, "videoProxyTail()") {
		t.Error("the capture leg no longer ends in videoProxyTail(); its head queue and its " +
			"proxysink would be written out by hand, which is a second spelling of the seam")
	}
	seam, err := os.ReadFile(seamSourceFile)
	if err != nil {
		t.Fatalf("reading %s: %v", seamSourceFile, err)
	}
	if !strings.Contains(string(seam), `"queue name=" + nameVideoProxyQueue`) {
		t.Errorf("%s no longer names the picture leg's head queue from nameVideoProxyQueue; an "+
			"unnamed queue is named by GStreamer, matches no prefix, and its failures become "+
			"fatal", seamSourceFile)
	}
}

// TestCaptureLegConformsBeforeTheTee pins the ORDER of the conform chain, and
// each position in it was paid for.
//
// videoconvert first because the card negotiates UYVY or v210 and the rest of
// the chain wants neither. deinterlace above videoscale because scaling
// interleaved fields mixes two moments in time into one line pair. videorate
// LAST and MANDATORY: decklinkvideosrc emits a 720x486 NTSC placeholder as its
// first buffer on every start, and a fixed capsfilter with no videorate in
// front of it dies 0.088 s after PLAYING with not-negotiated (-4), 3 runs of 3.
// The caps then sit above the tee so that BOTH branches — the broadcast one and
// any preview — see the conformed format rather than whatever the camera is.
func TestCaptureLegConformsBeforeTheTee(t *testing.T) {
	body := captureLegSource(t)

	positions := []struct {
		what  string
		token string
	}{
		{"the capture source", captureLegMarker},
		{"videoconvert", "videoconvert name="},
		{"deinterlace", "deinterlace name="},
		{"videoscale", "videoscale name="},
		{"videorate", "videorate name="},
		{"the conform capsfilter", "conform.captureCaps()"},
		{"the tee", "tee name="},
	}
	last := -1
	for _, p := range positions {
		i := strings.Index(body, p.token)
		if i < 0 {
			t.Fatalf("the capture leg has no %s (%q). Re-derive this guard from the new shape "+
				"rather than deleting it, and read the comment above first — every element in "+
				"that list is there because something measurable broke without it", p.what, p.token)
		}
		if i < last {
			t.Errorf("%s is out of order in the capture leg; the required order is "+
				"source, videoconvert, deinterlace, videoscale, videorate, caps, tee", p.what)
		}
		last = i
	}

	if !strings.Contains(body, "mode=auto") {
		t.Error("the capture source no longer asks for mode=auto. A PINNED mode that disagrees " +
			"with the input does not fail: measured, mode=pal against a real 1080p25 input " +
			"produced 50 clean PAL buffers with only a warning — green lamp, real bitrate, " +
			"black picture")
	}
	if !strings.Contains(body, "allow-not-linked=true") {
		t.Error("the tee no longer allows an unlinked branch, so a build without the optional " +
			"preview returns NOT_LINKED upstream and stops the broadcast leg. A second " +
			"decklinkvideosrc is not an alternative: the card is exclusive and two sources " +
			"fail 3/3 in one process and 3/3 across two")
	}
}

// TestBothVideoLegsMeetAtTheSameSeam is the guard the whole two-leg design rests
// on, and the one to read first if anything downstream ever behaves differently
// depending on what the picture leg is.
//
// REWRITTEN, AND THE MEETING POINT MOVED. It used to be
// TestBothVideoLegsMeetAtTheSameEncoder and it counted the H.264 encoder line in
// pipelineDescription. The encoder is now in the SEND pipeline, on the far side
// of the proxy, so the two picture legs cannot meet there — they meet at the
// PROXY TAIL, and that is the stronger form of the same claim: everything from
// videoProxyTail down (the leaky queue, the proxysink, and then in the send
// pipeline the encoder, h264parse, vq, mpegtsmux, srtq, ReplaceSink and every
// reconnect rule in internal/sender) is one graph, written once, for both
// sources.
//
// Written twice it would be two graphs that are equal today, and the day they
// stopped being equal the symptom would not be a build failure: it would be a
// feed that behaves differently on the seat with a card in it.
func TestBothVideoLegsMeetAtTheSameSeam(t *testing.T) {
	body := captureDescriptionSource(t)

	// The tail's INSERTION, once per leg and rendered from ONE function, so a
	// second spelling of the queue or the proxysink cannot appear on one leg.
	if n := strings.Count(body, "videoProxyTail()"); n != 2 {
		t.Errorf("captureDescription inserts videoProxyTail() %d times, want exactly 2 — once "+
			"for the slate leg and once for the card leg's broadcast branch. Anything else is "+
			"either a leg that ends somewhere of its own or a second spelling of the seam that "+
			"only has to stay equal by hand", n)
	}
	// And the SEND side is written once for the one video leg it has: the tail
	// below the encoder is what internal/sender reasons about.
	send := sendDescriptionSource(t)
	if n := strings.Count(send, `encoderName + " name="`); n != 1 {
		t.Errorf("sendDescription inserts the H.264 encoder %d times, want exactly 1", n)
	}
	for _, tail := range []string{
		"h264parse config-interval=-1",
		"video/x-h264,stream-format=byte-stream,alignment=au",
		"nameMuxVideoQueue",
	} {
		if n := strings.Count(send, tail); n != 1 {
			t.Errorf("%q appears %d times in sendDescription, want exactly 1: the mux tail is "+
				"what lets internal/sender reason about one graph", tail, n)
		}
	}
}

// TestTheVideoLegIsChosenOnlyByTheConfiguredCard pins the compatibility
// statement for the whole feature: a seat that configures nothing builds the
// slate leg, and nothing else can change that.
//
// It matters because the tempting "improvement" is to detect a card and use it
// — which would turn a DeckLink somebody fitted for an unrelated purpose into
// the picture a commentary position transmits, with no setting anywhere saying
// so. The condition must be the option field and only the option field.
func TestTheVideoLegIsChosenOnlyByTheConfiguredCard(t *testing.T) {
	// THE DECISION MOVED UP ONE LEVEL AND THE CLAIM IS UNCHANGED. It used to be
	// pipelineDescription testing `videoCapture != ""`; it is now PlanCapture
	// turning an empty VideoCaptureID into PictureSlate, once, before anything is
	// built, and captureDescription switching on the leg it was handed. That is
	// the same decision made in one place instead of two, and PlanCapture is
	// untagged so this half is checked BEHAVIOURALLY at Gate A rather than by
	// reading text — see TestPlanCaptureAppliesTheFusionRule.
	if got := PlanCapture(CaptureSources{AudioDeviceID: "some-endpoint"}); len(got) == 0 ||
		got[0].Picture != PictureSlate {
		t.Errorf("PlanCapture with no VideoCaptureID planned %+v; an empty id must mean THE "+
			"SLATE, byte for byte, on every seat that has configured nothing", got)
	}
	if got := PlanCapture(CaptureSources{VideoCaptureID: "2747401380", AudioDeviceID: "e"}); len(got) == 0 ||
		got[0].Picture != PictureCard {
		t.Errorf("PlanCapture with a card id planned %+v, want a card picture leg", got)
	}

	body := captureDescriptionSource(t)
	if !strings.Contains(body, "PictureSlate") || !strings.Contains(body, "PictureCard") {
		t.Error("captureDescription no longer branches on the picture leg it was handed, so the " +
			"planner's decision and the string are two decisions again")
	}

	// The capture build must decide the same way, and must not consult an
	// enumeration. It reads the LEG-SET rather than the id, which is the one
	// change: PlanCapture turns an empty VideoCaptureID into PictureSlate before
	// anything is built, so the branch below is the same decision made once and
	// carried, rather than the same string tested twice.
	start := captureStartSequence(t)
	if !strings.Contains(start, "PictureCard") && !strings.Contains(start, "PictureSlate") {
		t.Error("the capture build no longer branches on the picture leg, so the element it " +
			"configures and the element captureDescription built can disagree")
	}
	if strings.Contains(start, "ListInputDevices") {
		t.Error("the capture build enumerates devices to decide the video leg. The leg is chosen " +
			"by CONFIGURATION alone: a card that happens to be fitted must never become the " +
			"picture a commentary position transmits")
	}
}

// TestCaptureLegSetsThePersistentIDOutsideTheParseString mirrors the rule the
// slate path and the audio device id already follow, for the reason
// captureDescription gives: user-supplied strings do not go through
// gst_parse_launch's quoting rules. It also pins the reuse — the id goes to the
// video source through the SAME configureDeckLinkSource the audio source uses,
// because one saved persistent-id serves both entries the card publishes and
// two setters would be two chances to disagree about the same string.
func TestCaptureLegSetsThePersistentIDOutsideTheParseString(t *testing.T) {
	body := captureDescriptionSource(t)
	if strings.Contains(body, "persistent-id") || strings.Contains(body, "propPersistentID") {
		t.Error("the capture leg puts the persistent-id in the parse string. User-supplied " +
			"strings are set with g_object_set; the parser's quoting rules are not something to " +
			"trust a persisted id to")
	}

	start := captureStartSequence(t)
	if !strings.Contains(start, "configureDeckLinkSource") {
		t.Error("the capture build no longer points the video capture source at the card through " +
			"configureDeckLinkSource. That function is shared with the audio source on purpose: " +
			"the card publishes ONE persistent-id for both, and a second setter is a second set " +
			"of rules about the identical saved string")
	}
}

// ---------------------------------------------------------------------------
// The conform target. Unlike the guards above these are ORDINARY TESTS: the
// type and every rule about it live in gst.go, which carries no build tag, so
// Gate A links and runs the real code rather than reading it as text.
// ---------------------------------------------------------------------------

// TestCaptureCapsAreTheSpatialCapsPlusTheRate pins the caps the live capture
// leg conforms to, and it is a behavioural test rather than a source guard
// because ConformTarget lives in gst.go where Gate A can run it.
//
// The exact string below is the one that was on the wire during the end-to-end
// proof on 2026-08-16: a real DeckLink capture through this leg was ingested by
// the live M2L-X instance matchH, which reported cam4 "COMMS" streaming at
// h264 1920x1080, scan type P, 4:2:0, 8-bit, with 0 error packets. Changing it
// is changing what a switcher was measured accepting.
//
// The two legs must agree on everything except WHERE the rate is pinned, which
// is why this is asserted against spatialCaps rather than written out twice: a
// capture leg sending bt601 where the slate sent bt709 would be a colorimetry
// change at every source swap and would be invisible in the source.
func TestCaptureCapsAreTheSpatialCapsPlusTheRate(t *testing.T) {
	f := FallbackConformTarget()

	const want = "video/x-raw,format=NV12,width=1920,height=1080," +
		"pixel-aspect-ratio=1/1,colorimetry=bt709,interlace-mode=progressive,framerate=50/1"
	if got := f.captureCaps(); got != want {
		t.Errorf("captureCaps() = %q\nwant %q", got, want)
	}

	// The relationship, not just the value: whatever spatialCaps says about one
	// picture, the capture leg says too.
	if !strings.HasPrefix(f.captureCaps(), f.spatialCaps()+",") {
		t.Errorf("captureCaps() = %q no longer begins with spatialCaps() = %q. The two legs "+
			"have started describing a picture differently, which is a format change the "+
			"switcher sees and this file does not", f.captureCaps(), f.spatialCaps())
	}

	// And a non-default target, because the whole point of the type is that
	// 1080p50 is not the only answer. 720p50 is the configuration that was
	// measured being ingested by M2L-X as width=1280 height=720.
	small := ConformTarget{Width: 1280, Height: 720, FrameRateNum: 50, FrameRateDen: 1}
	if got := small.captureCaps(); !strings.Contains(got, "width=1280,height=720") ||
		!strings.HasSuffix(got, ",framerate=50/1") {
		t.Errorf("captureCaps() for 720p50 = %q, which does not carry the target it was given", got)
	}

	// 29.97 must stay the exact fraction. A capture leg paced at 2997/100
	// against a 30000/1001 source gains a frame every 33 seconds — well inside
	// a match and invisible in a twenty-second test.
	ntsc := ConformTarget{Width: 1920, Height: 1080, FrameRateNum: 30000, FrameRateDen: 1001}
	if !strings.HasSuffix(ntsc.captureCaps(), ",framerate=30000/1001") {
		t.Errorf("captureCaps() for 1080p29.97 = %q; the rate must be the exact fraction",
			ntsc.captureCaps())
	}
}

// TestFallbackConformTargetReproducesTheShippedCaps is the compatibility
// statement for the whole change, and it is the test to read first if this
// area ever misbehaves on air.
//
// The two strings below are the caps that were written into gst_cgo.go's parse
// string before the conform target became an option, split at the point the
// videoscale-before-imagefreeze reorder required. As long as this passes, an
// installation that configures nothing — which is every installation until
// somebody wires the switcher's format through — builds the same graph out of
// the same caps it has been running on air with.
//
// ONE FIELD IS NOT IN THE SHIPPED STRING and is stated here rather than left
// for somebody to find with `git diff`: interlace-mode=progressive is new. The
// shipped caps named format, width, height, framerate, pixel-aspect-ratio and
// colorimetry, and said nothing about scan. spatialCaps says it, for the reason
// given on that method — it is the one thing this leg can actually produce, and
// pinning it turns a future GStreamer negotiating otherwise into a loud caps
// failure at Start instead of a feed the switcher quietly will not take.
//
// It is an ADDED CONSTRAINT on a leg that is on air, so it was verified rather
// than reasoned about. Measured on macOS arm64 / GStreamer 1.26.10 on
// 2026-08-15, the old caps and the new split pair were each run through
// `filesrc ! pngdec ! ... ! filesink` for three frames, at both slate sizes:
// the rendered NV12 was byte-identical (md5 3295158d… for the 1920x1080 slate,
// 40bea7f4… for a 1920x1200 export scaled down), so the field fixates where it
// was already being fixated by default and constrains nothing that was
// previously free.
func TestFallbackConformTargetReproducesTheShippedCaps(t *testing.T) {
	f := FallbackConformTarget()

	const wantSpatial = "video/x-raw,format=NV12,width=1920,height=1080," +
		"pixel-aspect-ratio=1/1,colorimetry=bt709,interlace-mode=progressive"
	const wantTemporal = "video/x-raw,framerate=50/1"

	if got := f.spatialCaps(); got != wantSpatial {
		t.Errorf("spatialCaps() = %q\nwant %q", got, wantSpatial)
	}
	if got := f.temporalCaps(); got != wantTemporal {
		t.Errorf("temporalCaps() = %q\nwant %q", got, wantTemporal)
	}
	if got := f.String(); got != "1920x1080p50" {
		t.Errorf("String() = %q, want 1920x1080p50", got)
	}

	// The zero value must resolve to exactly this and must not read as a fault:
	// "the switcher has not been read yet" is the ordinary state at Start.
	got, reason := ConformTarget{}.resolve()
	if got != f {
		t.Errorf("the zero ConformTarget resolves to %v, want %v", got, f)
	}
	if reason != "" {
		t.Errorf("the zero ConformTarget resolves with reason %q; nothing known yet is not a "+
			"fault and a field log that says it is buries the line that means something", reason)
	}
}

// TestConformTargetResolve pins which formats reach gst_parse_launch and which
// are turned back into the default. The rule that matters more than any single
// row: NOTHING here returns an error. A still slate is not worth refusing to
// carry commentary for.
func TestConformTargetResolve(t *testing.T) {
	def := FallbackConformTarget()
	tests := []struct {
		name       string
		in         ConformTarget
		want       ConformTarget
		wantReason bool
	}{
		{
			name: "a measured switcher format passes through",
			in:   ConformTarget{Width: 1920, Height: 1080, FrameRateNum: 50, FrameRateDen: 1},
			want: ConformTarget{Width: 1920, Height: 1080, FrameRateNum: 50, FrameRateDen: 1},
		},
		{
			name: "720p50 passes through: this is the case the option exists for",
			in:   ConformTarget{Width: 1280, Height: 720, FrameRateNum: 50, FrameRateDen: 1},
			want: ConformTarget{Width: 1280, Height: 720, FrameRateNum: 50, FrameRateDen: 1},
		},
		{
			// A numerator with no denominator is NOT rescued into n/1. It is
			// the shape a caller gets by building the struct by hand instead of
			// going through ConformTargetFromRate, and rescuing it would make
			// the hand-built path look like it worked right up until somebody
			// hand-built 30000 and got 30000 fps.
			name:       "a numerator with no denominator is not rescued",
			in:         ConformTarget{Width: 1920, Height: 1080, FrameRateNum: 25},
			want:       def,
			wantReason: true,
		},
		{
			name: "29.97 as a fraction survives intact",
			in:   ConformTarget{Width: 1920, Height: 1080, FrameRateNum: 30000, FrameRateDen: 1001},
			want: ConformTarget{Width: 1920, Height: 1080, FrameRateNum: 30000, FrameRateDen: 1001},
		},
		{
			// An interlaced instance is 1080i25/1080i50 in the switcher's own
			// vocabulary and 1920x1080 at the reported RATE here: there is no
			// scan field to set, by the argument in gst.go. This row exists so
			// that the rate half is not quietly rejected along with the scan.
			name: "the rate of an interlaced instance passes through",
			in:   ConformTarget{Width: 1920, Height: 1080, FrameRateNum: 25, FrameRateDen: 1},
			want: ConformTarget{Width: 1920, Height: 1080, FrameRateNum: 25, FrameRateDen: 1},
		},
		{
			name:       "a size with no frame rate is not a format",
			in:         ConformTarget{Width: 1920, Height: 1080},
			want:       def,
			wantReason: true,
		},
		{
			name:       "a frame rate with no size is not a format",
			in:         ConformTarget{FrameRateNum: 50, FrameRateDen: 1},
			want:       def,
			wantReason: true,
		},
		{
			name:       "an odd width cannot be NV12",
			in:         ConformTarget{Width: 1921, Height: 1080, FrameRateNum: 50, FrameRateDen: 1},
			want:       def,
			wantReason: true,
		},
		{
			name:       "an odd height cannot be NV12",
			in:         ConformTarget{Width: 1920, Height: 1081, FrameRateNum: 50, FrameRateDen: 1},
			want:       def,
			wantReason: true,
		},
		{
			name:       "a negative size is refused rather than passed to the parser",
			in:         ConformTarget{Width: -1920, Height: 1080, FrameRateNum: 50, FrameRateDen: 1},
			want:       def,
			wantReason: true,
		},
		{
			name:       "an absurd size is refused before anything allocates",
			in:         ConformTarget{Width: 100000, Height: 100000, FrameRateNum: 50, FrameRateDen: 1},
			want:       def,
			wantReason: true,
		},
		{
			name:       "a swapped fraction reads as an absurd rate",
			in:         ConformTarget{Width: 1920, Height: 1080, FrameRateNum: 1001, FrameRateDen: 1},
			want:       def,
			wantReason: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := tt.in.resolve()
			if got != tt.want {
				t.Errorf("resolve() = %v, want %v", got, tt.want)
			}
			if (reason != "") != tt.wantReason {
				t.Errorf("resolve() reason = %q, wantReason %v", reason, tt.wantReason)
			}
			// Whatever comes back must be usable, in every row: that is the
			// property Start relies on when it hands the result to the parser
			// without checking it again.
			if got.Width <= 0 || got.Height <= 0 || got.Width%2 != 0 || got.Height%2 != 0 ||
				got.FrameRateNum <= 0 || got.FrameRateDen <= 0 {
				t.Errorf("resolve() returned an unusable format %+v", got)
			}
		})
	}
}

// TestConformTargetFromRate pins the conversion internal/m2lx's float64 frame
// rate has to go through, and in particular pins the 1000/1001 family by exact
// value. 29.97 rendered as 2997/100 negotiates, plays, and drifts against the
// switcher by a frame every 33 seconds at 30p — visible well inside a match,
// invisible in a twenty-second test.
func TestConformTargetFromRate(t *testing.T) {
	tests := []struct {
		fps      float64
		num, den int
		wantErr  bool
	}{
		{fps: 50, num: 50, den: 1},
		{fps: 25, num: 25, den: 1},
		{fps: 60, num: 60, den: 1},
		{fps: 24, num: 24, den: 1},
		{fps: 29.97, num: 30000, den: 1001},
		{fps: 59.94, num: 60000, den: 1001},
		{fps: 23.976, num: 24000, den: 1001},
		{fps: 119.88, num: 120000, den: 1001},
		// 30 is SIX TIMES the tolerance away from 30000/1001, so an instance
		// genuinely configured for 30/1 can never be read as the NTSC rate.
		{fps: 30, num: 30, den: 1},
		// A rate nobody has met, carried faithfully rather than rounded into
		// one that was anticipated.
		{fps: 12.5, num: 25, den: 2},
		{fps: 0, wantErr: true},
		{fps: -50, wantErr: true},
		{fps: math.NaN(), wantErr: true},
		{fps: math.Inf(1), wantErr: true},
		{fps: 100000, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(strconv.FormatFloat(tt.fps, 'g', -1, 64), func(t *testing.T) {
			got, err := ConformTargetFromRate(1920, 1080, tt.fps)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ConformTargetFromRate(1920, 1080, %v) = %v, want an error", tt.fps, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ConformTargetFromRate(1920, 1080, %v): %v", tt.fps, err)
			}
			if got.FrameRateNum != tt.num || got.FrameRateDen != tt.den {
				t.Fatalf("rate = %d/%d, want %d/%d",
					got.FrameRateNum, got.FrameRateDen, tt.num, tt.den)
			}
			// And the fraction must be within a rounding of the input. An
			// exact-value table alone would not catch the table and the code
			// being wrong in the same direction.
			if rate := float64(got.FrameRateNum) / float64(got.FrameRateDen); math.Abs(rate-tt.fps) > ntscTolerance {
				t.Fatalf("rate = %d/%d = %v, which is not %v",
					got.FrameRateNum, got.FrameRateDen, rate, tt.fps)
			}
		})
	}

	// A raster that is not a raster is an error and never a silent fallback:
	// the caller has to be the one that decides to fall back, and has to log it.
	for _, r := range [][2]int{{0, 1080}, {1920, 0}, {-1920, 1080}} {
		if got, err := ConformTargetFromRate(r[0], r[1], 50); err == nil {
			t.Errorf("ConformTargetFromRate(%d, %d, 50) = %v, want an error", r[0], r[1], got)
		}
	}
}

// TestConformTargetString pins the rendering that reaches the field log, because
// a format mismatch is diagnosed by reading two of these lines side by side
// and a fraction printed raw would not be read at all.
func TestConformTargetString(t *testing.T) {
	tests := []struct {
		in   ConformTarget
		want string
	}{
		{ConformTarget{1920, 1080, 50, 1}, "1920x1080p50"},
		{ConformTarget{1920, 1080, 25, 1}, "1920x1080p25"},
		{ConformTarget{1280, 720, 30000, 1001}, "1280x720p29.97"},
		{ConformTarget{1920, 1080, 60000, 1001}, "1920x1080p59.94"},
		{ConformTarget{1920, 1080, 24000, 1001}, "1920x1080p23.976"},
		{ConformTarget{1920, 1080, 25, 2}, "1920x1080p12.5"},
		{ConformTarget{}, "(no conform target)"},
	}
	for _, tt := range tests {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("%+v String() = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestListInputDevicesFiltersLoopbackAndRenderIDs guards the enumeration half
// of the loopback defect.
//
// wasapi2's device provider republishes every RENDER endpoint as an
// Audio/Source "loopback" device carrying wasapi2.device.loopback=true, so a
// ListInputDevices that filters only on class + device.api offers playback
// endpoints in the commentary input dropdown — measured: 11 of 25 entries,
// and an operator selected one. The fix is two checks, and both must stay:
// the loopback property (read through the propLoopback const) and the
// endpoint-id namespace classifier in device_id.go.
//
// THE ASSERTIONS ARE UNCHANGED; ONLY THE FUNCTION THEY READ HAS MOVED, and that
// move is the whole macOS port in miniature. The three checks are Windows
// knowledge — macOS publishes no device.api, has no loopback republication, and
// has no id namespaces — so applying them on macOS skipped every device and
// returned an EMPTY dropdown, which is the bug the port started from. They now
// live in captureDeviceID in deviceprovider_windows.go, which ListInputDevices
// calls; the guard follows them there rather than being weakened to match a
// function that no longer contains them.
func TestListInputDevicesFiltersLoopbackAndRenderIDs(t *testing.T) {
	fset, file := parseSource(t, windowsProviderSourceFile)
	body := funcBody(t, fset, file, "", "captureDeviceID")

	if !strings.Contains(body, "propLoopback") {
		t.Error("the Windows captureDeviceID no longer reads the wasapi2.device.loopback property; " +
			"every playback endpoint's loopback republication is back in the input dropdown")
	}
	if !strings.Contains(body, "IsRenderEndpointID(") {
		t.Error("the Windows captureDeviceID no longer refuses render-namespace endpoint ids; a " +
			"playback endpoint that loses its loopback marker would be offered as a commentary input")
	}
	if !strings.Contains(body, "IsCaptureEndpointID(") {
		t.Error("the Windows captureDeviceID no longer consults IsCaptureEndpointID; the warning " +
			"for unrecognised id shapes — the asymmetry's other half — has been lost")
	}
	if !strings.Contains(body, `props.GetString("device.api")`) {
		t.Error("the Windows captureDeviceID no longer checks device.api; devices from another " +
			"provider would be offered under ids wasapi2src cannot open")
	}

	// The other half of the same statement: ListInputDevices must be the
	// platform-NEUTRAL caller. If any of this leaks back into it, macOS breaks
	// again in exactly the original way.
	fset, file = parseSource(t, cgoSourceFile)
	shared := funcBody(t, fset, file, "", "ListInputDevices")
	if !strings.Contains(shared, "captureDeviceID(") {
		t.Fatal("ListInputDevices no longer goes through the captureDeviceID seam")
	}
	for _, leaked := range []string{"propLoopback", "device.api", "IsRenderEndpointID(", "IsCaptureEndpointID("} {
		if strings.Contains(shared, leaked) {
			t.Errorf("ListInputDevices names %s directly. That is Windows knowledge in the shared "+
				"enumeration path: on macOS there is no device.api property, no loopback "+
				"republication and no id namespace, so this is how the dropdown came to be empty "+
				"there in the first place", leaked)
		}
	}
}

// TestDarwinCaptureIDsAreResolvedNotPersisted is the guard on the single most
// important structural difference in the macOS port, and the one thing in it a
// well-meaning future reader is most likely to "simplify" away.
//
// The persisted id and the id the element accepts are DIFFERENT THINGS on
// macOS. CoreAudio's unique-id ("BuiltInMicrophoneDevice") is a stable identity
// and is what reaches config.json. osxaudiosrc's device property is a gint
// AudioDeviceID — a runtime handle coreaudiod reassigns on every enumeration,
// on every reboot, on every replug. Storing the integer works perfectly until
// the operator restarts their Mac, after which the saved number names a
// different device, or nothing — and osxaudiosrc's default of 0 means "the
// system default input". The feed then carries the wrong microphone with every
// indicator green and no error anywhere. So:
//
//   - captureDeviceID must return the unique-id PROPERTY and must not go
//     anywhere near CreateElement, which is where the integer comes from;
//   - configureCaptureSource must resolve, and must set the property with the
//     typed integer setter rather than the string one.
func TestDarwinCaptureIDsAreResolvedNotPersisted(t *testing.T) {
	fset, file := parseSource(t, darwinProviderSourceFile)

	enumerate := funcBody(t, fset, file, "", "captureDeviceID")
	if !strings.Contains(enumerate, "propUniqueID") {
		t.Error("the darwin captureDeviceID no longer reads the unique-id property; there is " +
			"nothing else on a macOS device that is stable enough to persist")
	}
	if strings.Contains(enumerate, "CreateElement(") {
		t.Fatal("the darwin captureDeviceID calls CreateElement, which is where the gint " +
			"AudioDeviceID comes from. If that integer is what reaches Device.ID then it is what " +
			"reaches config.json, and after the operator's next reboot it names a different device " +
			"— or nothing, in which case osxaudiosrc falls back to the system default input and " +
			"the commentary feed is silently the wrong microphone")
	}

	configure := funcBody(t, fset, file, "", "configureCaptureSource")
	if !strings.Contains(configure, "resolveCaptureDeviceIndex(") {
		t.Fatal("configureCaptureSource no longer resolves the unique-id to the current " +
			"AudioDeviceID against a fresh enumeration; see the file comment in " +
			darwinProviderSourceFile)
	}
	if !strings.Contains(configure, `setIntProperty(src, "device"`) {
		t.Error(`configureCaptureSource does not set device with setIntProperty. osxaudiosrc's ` +
			`device property is a gint; setStringProperty would emit a GLib CRITICAL nobody reads ` +
			`and leave the property at 0, which CoreAudio reads as "the system default input"`)
	}

	// The resolution must fail rather than fall back. A default-device fallback
	// is the silent wrong-device failure by another name.
	resolve := funcBody(t, fset, file, "", "resolveCoreAudioDeviceIndex")
	if !strings.Contains(resolve, "enumerateDevices(") {
		t.Error("resolveCoreAudioDeviceIndex does not re-enumerate; a cached AudioDeviceID is " +
			"exactly the stale handle this whole mechanism exists to avoid")
	}
	if !strings.Contains(resolve, "return 0, fmt.Errorf(") {
		t.Error("resolveCoreAudioDeviceIndex never returns an error. A device that has gone away " +
			"must be a hard, loud failure — falling back to the default device is how a match is " +
			"commentated down the wrong microphone")
	}
}

// TestPlatformElementContractIsPinned checks the two factory names the port
// swaps per platform, in both files, by exact value.
//
// THEY ARE NOW ON OPPOSITE SIDES OF THE SEAM, which is why this test looks in two
// functions instead of one. captureSourceFactory (wasapi2src / osxaudiosrc) is
// the COMMENTARY CAPTURE's source and belongs to captureDescription;
// aacEncoderFactory (mfaacenc / atenc) is the SEND pipeline's encoder and belongs
// to sendDescription. Both are built from consts rather than literals, which
// makes the ordering guards portable and makes it possible to repoint either port
// by editing one const with nothing else in the package noticing. This is what
// stops that being a silent change.
//
// aacEncoderFactory is the one that carries a licence consequence.
// build/licenses/NOTICE.txt section G says this product ships no third-party
// AAC implementation because AAC-LC encoding is done by the operating system's
// own encoder. mfaacenc (Media Foundation) and atenc (AudioToolbox) are both
// exactly that. fdkaacenc, faac, voaacenc and avenc_aac are not, and any of
// them appearing here would make that sentence false without anybody noticing
// until a licence review.
func TestPlatformElementContractIsPinned(t *testing.T) {
	want := map[string]map[string]string{
		windowsElementsSourceFile: {
			"captureSourceFactory": "wasapi2src",
			"aacEncoderFactory":    "mfaacenc",
		},
		darwinElementsSourceFile: {
			"captureSourceFactory": "osxaudiosrc",
			"aacEncoderFactory":    "atenc",
		},
	}
	for filename, consts := range want {
		_, file := parseSource(t, filename)
		found := map[string]string{}
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range spec.Names {
				if _, wanted := consts[name.Name]; !wanted || i >= len(spec.Values) {
					continue
				}
				if lit, ok := spec.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					found[name.Name] = strings.Trim(lit.Value, `"`)
				}
			}
			return true
		})
		for name, value := range consts {
			if found[name] != value {
				t.Errorf("%s: %s = %q, want %q", filename, name, found[name], value)
			}
		}
	}

	// And the two strings must actually be built from them, or pinning the consts
	// pins nothing. One each, on the side of the seam that owns it — a guard that
	// looked for both in one function would go green on a build where either had
	// been hardcoded, because the other would satisfy it.
	if body := captureDescriptionSource(t); !strings.Contains(body, "captureSourceFactory") {
		t.Error("captureDescription no longer uses captureSourceFactory; the commentary source " +
			"has been hardcoded to one platform's element again")
	}
	if body := sendDescriptionSource(t); !strings.Contains(body, "aacEncoderFactory") {
		t.Error("sendDescription no longer uses aacEncoderFactory; the AAC encoder has been " +
			"hardcoded to one platform's element again, which build/licenses/NOTICE.txt " +
			"section G is a claim about")
	}
}

// TestLoopbackIsNeverSetInTheCgoSource pins the DELIBERATE NON-GOAL.
//
// The tempting one-line "fix" for a render endpoint in the input dropdown is
// to set wasapi2src's loopback property to true so that opening the playback
// endpoint succeeds. It would succeed — at putting the operator's own monitor
// mix on air, as echo or feedback on a live feed. The property must be READ
// during enumeration (to skip the republications) and never SET anywhere.
// The scan now covers the Windows device provider as well as gst_cgo.go, because
// that is where the const and the only legitimate read of it moved to. Scanning
// only the old file would have left the setter rule unenforced in precisely the
// file most likely to break it.
func TestLoopbackIsNeverSetInTheCgoSource(t *testing.T) {
	// The const must exist with the exact provider key, or the enumeration
	// read is silently checking nothing.
	_, provider := parseSource(t, windowsProviderSourceFile)
	constOK := false
	ast.Inspect(provider, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if name.Name != "propLoopback" || i >= len(spec.Values) {
				continue
			}
			if lit, ok := spec.Values[i].(*ast.BasicLit); ok && lit.Value == `"wasapi2.device.loopback"` {
				constOK = true
			}
		}
		return true
	})
	if !constOK {
		t.Errorf(`%s has no const propLoopback = "wasapi2.device.loopback"`, windowsProviderSourceFile)
	}

	// No setter call in any of the real build's source may name loopback,
	// whether as a string literal or through the const.
	for _, name := range []string{cgoSourceFile, windowsProviderSourceFile, darwinProviderSourceFile} {
		_, file := parseSource(t, name)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee := calleeName(call)
			if !strings.Contains(callee, "Set") && callee != "UtilSetObjectArg" {
				return true
			}
			for _, arg := range call.Args {
				switch a := arg.(type) {
				case *ast.BasicLit:
					if a.Kind == token.STRING && strings.Contains(strings.ToLower(a.Value), "loopback") {
						t.Errorf("%s: %s is passed a loopback property name: the operator's monitor "+
							"mix would go on air", name, callee)
					}
				case *ast.Ident:
					if a.Name == "propLoopback" {
						t.Errorf("%s: %s is passed propLoopback: the property is read-only by design",
							name, callee)
					}
				}
			}
			return true
		})
	}
}

// calleeName returns the rightmost identifier of a call's function expression:
// "SetObjectProperty" for el.SetObjectProperty(...), "UtilSetObjectArg" for
// gogst.UtilSetObjectArg(...). "" if the shape is something else.
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// TestStartRefusesRenderIDsAndRechecksFatal guards the two Start-side halves
// of the loopback defect.
//
// The refusal must run before gst_parse_launch: wasapi2src accepts a render
// id at construction and fails ASYNCHRONOUSLY, after Start has already
// reported success, so anything later than the option checks is too late.
//
// The fatal re-check must sit between BlockSetState and `p.started = true`:
// wasapi2 opens the endpoint on its own thread and posts failure on the bus,
// so NULL→PLAYING can report success while onBusMessage has already latched a
// pipeline-fatal error. ReplaceSink double-checks fatal before promising
// success for exactly this reason; Start must mirror it or it reports a
// running pipeline whose capture chain is already dead.
func TestStartRefusesRenderIDsAndRechecksFatal(t *testing.T) {
	// THE REFUSAL IS THE CAPTURE BUILD'S, because the device is. Nothing in a send
	// pipeline opens an endpoint, so a guard that went on reading the send Start
	// would have passed for ever by no longer being able to see the code it
	// requires — the failure startSequence's comment warns about, happening for a
	// second time to the same rule.
	capLines := strings.Split(captureStartSequence(t), "\n")
	capRefuse := lastLineMatching(capLines, func(s string) bool {
		return strings.Contains(s, "refuseWrongAudioSource(")
	})
	capParse := lastLineMatching(capLines, func(s string) bool {
		return strings.Contains(s, "ParseLaunch(")
	})
	if capRefuse < 0 {
		t.Error("the capture build never calls refuseWrongAudioSource; a playback endpoint reaches " +
			"wasapi2src and fails asynchronously as a fake network error, and a seat with NEITHER " +
			"source reaches osxaudiosrc with an empty device, which is the SYSTEM DEFAULT INPUT " +
			"and not an error")
	}
	if capParse < 0 {
		t.Fatal("the capture build never calls ParseLaunch")
	}
	if capRefuse >= 0 && capRefuse > capParse {
		t.Error("the capture build refuses a render endpoint only after building the pipeline; " +
			"the refusal must be synchronous and up front")
	}

	// THE FATAL RE-CHECK IS STILL THE SEND PIPELINE'S, and it is still between
	// PLAYING and p.started, for the same reason: a state change can return
	// success while a bus error has already latched.
	fset, file := parseSource(t, cgoSourceFile)
	lines := strings.Split(startSequence(t, fset, file), "\n")
	play := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "BlockSetState(gogst.StatePlaying")
	})
	fatal := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "p.fatalError()")
	})
	started := lastLineMatching(lines, func(s string) bool {
		return s == "p.started = true"
	})
	if play < 0 || started < 0 {
		t.Fatal("Start no longer has the BlockSetState(PLAYING) / p.started = true shape these " +
			"guards expect; re-read them before restructuring")
	}
	if fatal < 0 || fatal < play || fatal > started {
		t.Error("Start does not re-check p.fatalError() between BlockSetState and p.started = true; " +
			"an asynchronous failure lets Start report a running pipeline whose chain is already dead")
	}
}

// TestStartLogsTheEndpointID guards the diagnosability of the asynchronous
// failure. wasapi2src echoes the REQUESTED id verbatim in its error 1551
// (proved by probe), so a log line carrying opts.AudioDeviceID immediately
// before the property is set lets a field log match the failure to the
// request instead of leaving the id to be argued about.
func TestStartLogsTheEndpointID(t *testing.T) {
	body := captureStartSequence(t)

	logged := false
	for idx := strings.Index(body, "log.Printf("); idx >= 0; {
		end := idx + 250
		if end > len(body) {
			end = len(body)
		}
		if strings.Contains(body[idx:end], "AudioDeviceID") {
			logged = true
			break
		}
		next := strings.Index(body[idx+1:], "log.Printf(")
		if next < 0 {
			break
		}
		idx += 1 + next
	}
	if !logged {
		t.Error("the capture build never logs the requested AudioDeviceID; wasapi2's asynchronous " +
			"error 1551 quotes the requested id verbatim, and without this line the log cannot " +
			"say what was requested")
	}
}

// TestTheSendStartClaimsAndArmsTheSeamBeforeTheParse is the ORDER GUARD over the
// most consequential five lines in this package, and every position in it was
// paid for in a measurement.
//
// NewSend BEFORE ParseLaunch, because it both CLAIMS and ARMS. Arming after the
// parse would still be before PLAYING and would still look right; what it would
// lose is the claim, and a second proxysrc attaching to a live proxysink does not
// fail — it steals the stream and kills the first, measured, with nothing on
// either bus.
//
// Bind AFTER the arming and BEFORE PLAYING. Binding first attaches a consumer to
// a proxysink whose sticky events have not been reset, which is the zero-byte
// session this whole seam exists to prevent; binding after PLAYING attaches it to
// a pipeline that has already decided it has no upstream.
//
// attachLiveWatch BEFORE PLAYING, because a probe added afterwards misses the
// buffers the liveness gate is then waiting for, and a healthy seam would be
// refused by its own detector.
//
// The gate closed BEFORE PLAYING, unchanged from the pre-seam build: Start
// installs no sink, so srtq's src pad has no peer, and without the gate the
// queue's loop pushes into nothing and posts an error before the first
// ReplaceSink ever runs.
func TestTheSendStartClaimsAndArmsTheSeamBeforeTheParse(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	lines := strings.Split(startSequence(t, fset, file), "\n")

	at := func(what, needle string) int {
		i := lastLineMatching(lines, func(s string) bool { return strings.Contains(s, needle) })
		if i < 0 {
			t.Fatalf("the send Start no longer %s (%q). Re-derive this guard from the new shape "+
				"rather than deleting it, and read the comment above first — every position in "+
				"that list is there because something measurable broke without it", what, needle)
		}
		return i
	}

	seam := at("claims and arms the capture seam", "NewSend(p.set)")
	parse := at("parses a pipeline", "gogst.ParseLaunch(")
	bind := at("binds the proxysrcs", "p.seam.Bind(")
	gate := at("closes the gate", "p.gateClosed.Store(true)")
	watch := at("attaches the muxer watchdog", "attachLiveWatch(")
	play := at("goes to PLAYING", "BlockSetState(gogst.StatePlaying")
	verdict := at("gates on media having arrived", "p.awaitFirstMediaLocked()")
	poller := at("starts the liveness poller", "p.live.run(")
	started := at("marks itself started", "p.started = true")

	for _, order := range []struct {
		earlier, later int
		what           string
	}{
		{seam, parse, "the seam must be claimed and ARMED before anything is parsed: a session " +
			"that skipped the arming carries zero bytes with SRT connected and every lamp green"},
		{parse, bind, "the proxysrcs cannot be bound before they have been parsed"},
		{bind, play, "binding after PLAYING attaches the consumer to a pipeline that has already " +
			"decided it has no upstream"},
		{gate, play, "the gate must be shut before the pipeline can produce a buffer, because " +
			"srtq's src pad has no peer until the first ReplaceSink"},
		{watch, play, "the watchdog's probes must be in place before PLAYING or the first " +
			"buffers cross unseen and the liveness gate refuses a healthy seam"},
		{play, verdict, "the liveness gate measures what arrived AFTER the pipeline was playing"},
		{verdict, poller, "the poller must not be running while the gate is deciding whether " +
			"there was ever a feed: both would be measuring the same silence, and the poller's " +
			"verdict calls back into an object still inside Start"},
		{verdict, started, "Start must not report success before the liveness gate has passed; " +
			"that gate is the only thing standing between a missed arming and a green lamp over " +
			"silence"},
	} {
		if order.earlier > order.later {
			t.Error(order.what)
		}
	}
}

// TestTheSendTeardownReleasesTheSeamAfterReachingNull is the other end of the
// same rule, and it is the one that cannot be recovered from afterwards.
//
// gst_proxy_src_dispose clears only the src's weak reference on the sink; the
// SINK's reference on the old src survives until the old src is finalised, which
// Go may not do promptly. Release the claim while this pipeline is still PAUSED
// or PLAYING and the next session's arming cannot repair it —
// gst_proxy_sink_sink_chain re-stores the sticky events on the old proxysrc's
// still-active pad, the flags go TRUE before the new proxysrc binds, and the new
// session carries ZERO BYTES with SRT connected and every lamp green.
//
// The watchdog goes the other way round, BEFORE the state change, because its
// poller reads pads the probes write: removing the probes first would leave the
// poller reading a counter nothing can update on a pipeline already going to NULL.
func TestTheSendTeardownReleasesTheSeamAfterReachingNull(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	lines := strings.Split(funcBody(t, fset, file, "cgoPipeline", "teardownLocked"), "\n")

	watch := lastLineMatching(lines, func(s string) bool { return strings.Contains(s, "p.live.Stop()") })
	null := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "BlockSetState(gogst.StateNull")
	})
	release := lastLineMatching(lines, func(s string) bool { return strings.Contains(s, "p.seam.Stop()") })

	if release < 0 {
		t.Fatal("teardownLocked never releases the capture seam. The claim would be held for the " +
			"life of the process and every later START refused with ErrSeamBusy over a session " +
			"that no longer exists")
	}
	if null < 0 {
		t.Fatal("teardownLocked no longer takes the pipeline to NULL; re-derive this guard")
	}
	if release < null {
		t.Error("teardownLocked releases the seam BEFORE the pipeline has reached NULL. The next " +
			"session's arming cannot repair a proxysink whose old consumer is still alive, and " +
			"that session carries zero bytes with SRT connected and every lamp green")
	}
	if watch < 0 {
		t.Fatal("teardownLocked never stops the muxer watchdog; its poller would outlive the " +
			"pipeline and read probes that have gone")
	}
	if watch > null {
		t.Error("teardownLocked takes the pipeline to NULL before joining the watchdog's poller, " +
			"which leaves it reading counters nothing can update on a pipeline being disposed")
	}
}

// TestTheSendPipelineOpensNoDevice is the seam's central claim, checked as text
// because it is a claim about what the source does NOT contain.
//
// A send pipeline whose description or build reached for a device would be the
// pre-seam pipeline growing back one line at a time, and every symptom of that is
// downstream and slow: the card held by two pipelines, a device change that
// blanks the picture, a preview that dies with the session. Naming the forbidden
// identifiers here makes it a compile-time-visible failure at the moment it is
// written.
func TestTheSendPipelineOpensNoDevice(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	descFset, descFile := parseSource(t, captureDescSourceFile)

	for _, fn := range []struct{ name, body string }{
		{"sendDescription", funcBody(t, descFset, descFile, "", "sendDescription")},
		{"the send Start sequence", startSequence(t, fset, file)},
	} {
		for _, forbidden := range []string{
			"configureCaptureSource",  // the platform endpoint
			"configureDeckLinkSource", // the card
			"captureSourceFactory",
			"videoCaptureFactory",
			"audioCaptureFactory",
			"nameSlateSrc",
			"attachPreview",
			"startSignalWatch",
			"applyStartChannelMapLocked",
			"applyCoughMuteLocked",
		} {
			if strings.Contains(fn.body, forbidden) {
				t.Errorf("%s mentions %s. The send pipeline's source of media is a proxysrc and "+
					"it opens NOTHING: a device, a slate, a preview, a matrix or a mute here is "+
					"the pre-seam pipeline growing back, and it would take the always-live "+
					"capture down with it", fn.name, forbidden)
			}
		}
	}
}

// TestMarkFatalSiteWrapsErrPipelineFatal guards the sentinel. The bus
// handler's fatal classification must wrap ErrPipelineFatal so that
// internal/sender can errors.Is its way out of retrying a failure no
// reconnect can fix, while the rendered text keeps the "pipeline-fatal"
// substring older greps rely on.
func TestMarkFatalSiteWrapsErrPipelineFatal(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	body := funcBody(t, fset, file, "cgoPipeline", "onBusMessage")

	if !strings.Contains(body, "p.markFatal(") {
		t.Fatal("onBusMessage no longer marks non-sink errors as pipeline-fatal")
	}
	if !strings.Contains(body, "ErrPipelineFatal") {
		t.Error("onBusMessage's markFatal site does not wrap ErrPipelineFatal; ReplaceSink's " +
			"latched error is unmatchable with errors.Is and the sender goes back to retrying " +
			"an unfixable failure as a network fault")
	}
}

// TestBusHandlerClosesTheGateBeforeAnyCgoCall guards the ordering the gate's
// whole value depends on, and it is here rather than in capturefault_test.go
// because what it is checking is a property of the CALL SITE, not of the
// decision being called.
//
// The gate is what stops media reaching a sink that has just failed. The
// comment on the store says why it goes first — "the buffer that is about to
// carry GST_FLOW_ERROR into the queue is racing us" — and BUILD-NOTES.md
// section 8.6 is the 21 ms window in which losing that race took the capture
// chain down and the commentary off air, which is the failure the whole
// sink-pad gate was added to prevent.
//
// # WHAT THIS TEST USED TO SAY, AND WHY THAT CLAIM IS GONE
//
// It used to assert a two-stage classification: classifyBusError with the zero
// captureLegs ABOVE the store, and captureLegsFor — a cgo call, a parent lookup
// and a gst_bin_get_by_name over the whole graph — strictly BELOW it, on the one
// path whose class it could still change. The second stage existed because a
// video capture fault had to leave the gate open, and because on a fused seat a
// video element's error is an audio fault wearing a video element's name.
//
// The send bus no longer carries any capture element at all. The slate, both
// DeckLink sources, the conform chain, the preview branch and both level elements
// live in a CapturePipeline with a bus of its own; this graph is two proxysrcs,
// two encoders, the muxer and the sink, so there is no error on it that may leave
// the gate open and nothing to establish from the graph. The expensive stage is
// therefore not merely below the store — it is deleted, along with captureLegsFor
// itself.
//
// What survives is the property the test was actually protecting: NOTHING that
// crosses into C runs before the store. That is now checkable as an absolute
// rather than as an ordering, which is a stronger guard than the one it replaces.
//
// Nothing else at Gate A would notice: the ordering is invisible to every
// behavioural test in this package, and the cost of getting it wrong is not a
// failure but a widened race that only shows up as an outage at a facility.
func TestBusHandlerClosesTheGateBeforeAnyCgoCall(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	body := funcBody(t, fset, file, "cgoPipeline", "onBusMessage")

	gate := strings.Index(body, "p.gateClosed.Store(true)")
	if gate < 0 {
		t.Fatal("onBusMessage no longer closes the gate on a bus error; a failing sink now keeps " +
			"receiving media and BUILD-NOTES.md section 8.6 is back")
	}
	classify := strings.Index(body, "classifySendBusError(")
	if classify < 0 {
		t.Fatal("onBusMessage no longer classifies bus errors, so an srtout-N error is no longer " +
			"told apart from a fatal one and internal/sender's ladder cannot repair a sink it " +
			"could have replaced")
	}
	if classify > gate {
		t.Error("the classification has moved BELOW the gate close. It has to be above it: the " +
			"store is unconditional only because the classification costs three string " +
			"comparisons, and a class that decided whether to store would have to be computed " +
			"first")
	}

	// THE ABSOLUTE. Any of these before the store is a cgo call, a GObject lock or
	// a bin traversal in front of the one store that is racing a buffer carrying
	// GST_FLOW_ERROR into srtq.
	for _, banned := range []string{"captureLegsFor(", "captureFatalError(", "GetByName(", "GetParent("} {
		if i := strings.Index(body, banned); i >= 0 && i < gate {
			t.Errorf("%s is called BEFORE the gate closes. It crosses into C, and this path is "+
				"taken by every srtout-N error on every peer loss: it puts a cgo call in front of "+
				"the store while the buffer carrying GST_FLOW_ERROR into srtq is racing us",
				banned)
		}
	}

	// AND THE CAPTURE CLASSES ARE GONE FROM THIS HANDLER. They describe elements
	// that cannot post on this bus, and capturefault.go's proxy prefixes ("vprox",
	// "aprox") match THIS graph's vproxsrc and aproxsrc as well as the capture
	// side's tails — so a vproxsrc error would have been spared with the gate left
	// OPEN and an aproxsrc error handed to a DeckLink diagnosis of a graph with no
	// card in it.
	for _, gone := range []string{"classVideoCapture", "classPreview", "classAudioCapture"} {
		if strings.Contains(body, gone) {
			t.Errorf("onBusMessage still branches on %s. No capture element exists in the send "+
				"graph, and that branch is reachable only by the two proxysrcs whose names share "+
				"a prefix with the capture side's proxy tails", gone)
		}
	}
}

// TestInitPointsEveryPluginPathAtTheBundle guards against a foreign GStreamer
// install being loaded.
//
// Setting GST_PLUGIN_SYSTEM_PATH_1_0 to "" is the documented idiom for "scan no
// system directories" and it does survive on Windows — see
// TestEmptyEnvVarSurvivesOnWindows, which disproves the claim that it does not.
// A path is set instead because GLib's own empty-to-NULL mapping cannot be
// tested at Gate A, and a path is correct either way. If g_getenv did return
// NULL for an empty value, gstregistry.c would fall through to
// GST_PLUGIN_SYSTEM_PATH and then to the compiled-in system directories.
// missingElements() cannot detect that: it finds factories that are absent, not
// somebody else's copy of the same factory outranking ours.
func TestInitPointsEveryPluginPathAtTheBundle(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	body := funcBody(t, fset, file, "", "doInit")

	if strings.Contains(body, `"GST_PLUGIN_SYSTEM_PATH_1_0", ""`) {
		t.Fatal(`doInit sets GST_PLUGIN_SYSTEM_PATH_1_0 to "", which Windows turns into a ` +
			`deletion; GStreamer then scans its compiled-in system directories`)
	}
	for _, name := range []string{
		"GST_PLUGIN_SYSTEM_PATH_1_0",
		"GST_PLUGIN_SYSTEM_PATH",
		"GST_PLUGIN_PATH_1_0",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("doInit never sets %s; a machine-wide GStreamer install can outrank the bundle", name)
		}
	}
}

// TestInitResolvesTheBundleThroughThePlatformSeam pins the two questions doInit
// must not answer for itself.
//
// Both were literals in doInit until the macOS port, and both are wrong on one
// of the two platforms if they stay literals. The plugin directory is
// <appDir>\gst\lib\gstreamer-1.0 on Windows and <App>.app/Contents/Resources/
// gstreamer-1.0 on macOS — different not by taste but because codesign refuses
// to sign an .app with a plain directory under Contents/Frameworks. And macOS
// needs four more environment variables that Windows needs none of, all four
// neutralising Homebrew paths compiled into the vendored dylibs as C strings
// that install_name_tool cannot reach.
//
// The failure this guards is the quiet one. Hardcode the Windows path again and
// macOS does not silently misbehave, it fails at Init with every element
// missing — loud, and somebody would fix it. Drop extraInitEnv and macOS starts
// perfectly, scans its plugins in process because the compiled-in scanner path
// does not exist, and works right up until a plugin faults on load. That is the
// one worth a test.
//
// It asserts the CALLS rather than the paths, because the paths belong to
// gstpaths_windows.go and gstpaths_darwin.go and only one of those two files is
// in any given build. This test runs at Gate A on both platforms and can see
// neither of them, which is exactly why it checks the seam and not the answer.
func TestInitResolvesTheBundleThroughThePlatformSeam(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	body := funcBody(t, fset, file, "", "doInit")

	if !strings.Contains(body, "bundlePluginDir(") {
		t.Error("doInit no longer calls bundlePluginDir. If the plugin directory is spelled " +
			"inline it is spelled for one platform, and on the other one Init reports every " +
			"element missing")
	}
	if strings.Contains(body, `"gst", "lib", "gstreamer-1.0"`) {
		t.Error(`doInit contains the literal Windows plugin path again. It belongs in ` +
			`gstpaths_windows.go, where the macOS build cannot see it`)
	}
	if !strings.Contains(body, "extraInitEnv(") {
		t.Fatal("doInit no longer calls extraInitEnv. On macOS that means GST_PLUGIN_SCANNER " +
			"still points into /opt/homebrew, and GStreamer will scan plugins IN PROCESS with " +
			"one warning rather than failing — measured: the registry still built, 17 plugins, " +
			"291 features, every element resolved. It ships unnoticed and faults later")
	}

	// Order, which is the part that would be got wrong by somebody tidying up.
	// Two of the macOS variables are read DURING the registry rebuild that
	// gst_init performs, so setting them afterwards sets them for the next run of
	// the application and not this one.
	initAt := strings.Index(body, "gogst.Init()")
	envAt := strings.Index(body, "extraInitEnv(")
	if initAt >= 0 && envAt >= 0 && envAt > initAt {
		t.Error("doInit calls extraInitEnv AFTER gogst.Init(). The scanner path and " +
			"GIO_MODULE_DIR are read during the registry rebuild gst_init performs, so setting " +
			"them afterwards configures the next run of the application rather than this one")
	}
}

// ---------------------------------------------------------------------------
// The input meters: the stub's synthetic levels, the shared pure helpers in
// levels.go, and the source guards on the real build's level element.
// ---------------------------------------------------------------------------

// stubCaptureID is a capture-namespace endpoint id that passes the
// render-endpoint refusal, for tests that are not about that refusal.
const stubCaptureID = "{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}"

func TestClampLevelDB(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want float64
	}{
		{"negative infinity is the silence floor", math.Inf(-1), levelSilenceDB},
		{"NaN is the silence floor", math.NaN(), levelSilenceDB},
		{"below the floor clamps up", -144, levelSilenceDB},
		{"the floor itself passes", levelSilenceDB, levelSilenceDB},
		{"an ordinary level passes", -20.5, -20.5},
		{"zero passes", 0, 0},
		{"positive infinity clamps to full scale", math.Inf(1), 0},
		{"above full scale passes as measured", 1.2, 1.2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampLevelDB(tt.in); got != tt.want {
				t.Fatalf("clampLevelDB(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestLevelListToDB(t *testing.T) {
	if got := levelListToDB(nil); got != nil {
		t.Fatalf("levelListToDB(nil) = %v, want nil", got)
	}
	if got := levelListToDB([]any{}); got != nil {
		t.Fatalf("levelListToDB(empty) = %v, want nil", got)
	}

	got := levelListToDB([]any{-3.5, float32(-10), math.Inf(-1), "not a number"})
	want := []float64{-3.5, -10, levelSilenceDB, levelSilenceDB}
	if len(got) != len(want) {
		t.Fatalf("levelListToDB kept %d entries, want %d: the channel count must survive "+
			"bad elements or the left bar becomes the right bar", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestStubLevelWaveIsDeterministicAndInRange(t *testing.T) {
	// The exact endpoints: the wave starts at the bottom of its range and
	// reaches the top half a period later.
	if got := stubLevelAt(0, 0); got != stubLevelLowDB {
		t.Fatalf("stubLevelAt(0, 0) = %v, want %v", got, float64(stubLevelLowDB))
	}
	if got := stubLevelAt(stubLevelPeriod/2, 0); got != stubLevelHighDB {
		t.Fatalf("stubLevelAt(period/2, 0) = %v, want %v", got, float64(stubLevelHighDB))
	}
	// The right channel is the left channel a quarter period later, which is
	// what makes the two bars visibly independent at Gate A.
	if l, r := stubLevelAt(stubLevelRightOffset, 0), stubLevelAt(0, 1); l != r {
		t.Fatalf("channel offset broken: left at step %d = %v, right at step 0 = %v",
			stubLevelRightOffset, l, r)
	}
	for step := 0; step < 3*stubLevelPeriod; step++ {
		l := stubLevelsAt(step)
		for ch := range l.PeakDB {
			if l.PeakDB[ch] < stubLevelLowDB || l.PeakDB[ch] > stubLevelHighDB {
				t.Fatalf("step %d channel %d peak %v escapes [%d, %d]",
					step, ch, l.PeakDB[ch], stubLevelLowDB, stubLevelHighDB)
			}
			if want := l.PeakDB[ch] - stubLevelRMSBelowPeakDB; l.RMSDB[ch] != clampLevelDB(want) {
				t.Fatalf("step %d channel %d rms = %v, want peak-%d = %v",
					step, ch, l.RMSDB[ch], stubLevelRMSBelowPeakDB, want)
			}
		}
	}
}

func TestSilentLevelsIsAllChannelsAtTheFloor(t *testing.T) {
	l := silentLevels()
	if len(l.PeakDB) != levelStubChannels || len(l.RMSDB) != levelStubChannels {
		t.Fatalf("silentLevels has %d/%d channels, want %d", len(l.PeakDB), len(l.RMSDB), levelStubChannels)
	}
	for ch := range l.PeakDB {
		if l.PeakDB[ch] != levelSilenceDB || l.RMSDB[ch] != levelSilenceDB {
			t.Fatalf("channel %d = %v/%v, want %d/%d",
				ch, l.PeakDB[ch], l.RMSDB[ch], levelSilenceDB, levelSilenceDB)
		}
	}
}

// TestTheProgrammeMeterMeasuresWhatCrossesTheSeam guards the level element's
// PLACEMENT, which is the whole point of the input meters.
//
// REWRITTEN, AND THE MEASUREMENT POINT MOVED BY EXACTLY ONE QUEUE. It used to
// assert `resample < alevel < aacEncoderFactory` inside pipelineDescription: the
// meter sat immediately before the AAC encoder, so what it showed was what was
// being encoded and sent. The encoder is now in the send pipeline, on the far
// side of the proxy, so the strongest true statement is that alevel sits AFTER
// audioresample and the seam capsfilter and IMMEDIATELY BEFORE the proxy tail —
// with one leaky queue between it and the encoder.
//
// WHAT THAT COSTS IS STATED RATHER THAN GLOSSED (PLAN.md 0-BIS A3): in normal
// operation these are the same buffers, so the promise holds; during a send-side
// stall of more than about a second the queue drops and the meter can move while
// the far end loses samples. levels.go restates the doc claim where the promise
// is made. What has NOT changed is the thing this guard exists for — the meter is
// still below the resampler and the routing matrix, so it measures the signal
// this seat is producing and not the endpoint's raw format.
func TestTheProgrammeMeterMeasuresWhatCrossesTheSeam(t *testing.T) {
	body := captureDescriptionSource(t)

	level := strings.Index(body, "level name=\"+levelElementName")
	if level < 0 {
		t.Fatal("the commentary chain has no programme level element built from levelElementName; " +
			"the input meters have nothing to measure and the levels event will never fire on a " +
			"real build")
	}
	resample := strings.Index(body, "audioresample")
	tail := strings.Index(body, "audioProxyTail()")
	if resample < 0 || tail < 0 {
		t.Fatal("the commentary chain has been restructured; re-derive this guard from the new shape")
	}
	if !(resample < level && level < tail) {
		t.Error("the programme level element must sit AFTER audioresample and IMMEDIATELY BEFORE " +
			"the proxy tail: it exists to measure the exact signal this seat is producing, not " +
			"the endpoint's raw format, and anything between it and the seam is a second thing " +
			"that can change the audio after it has been measured")
	}
	if !strings.Contains(body, "interval=50000000") {
		t.Error("the level interval is no longer 50 ms (50000000 ns); the app-side throttle and " +
			"the UI's no-rAF rendering are both sized against 20 frames a second")
	}

	// AND NOTHING MEASURES BELOW THE SEAM. A level element in the send pipeline
	// would read the encoder's input rather than the microphone, which on a
	// send-side stall is exactly the reassurance nobody should be given.
	if send := sendDescriptionSource(t); strings.Contains(send, "level name=") {
		t.Error("sendDescription builds a level element. A meter measuring what has crossed the " +
			"seam reads the encoder's input rather than the microphone")
	}
}

// TestBusHandlerForwardsLevelMessages guards the real build's receive half:
// onBusMessage must handle GST_MESSAGE_ELEMENT, match the structure NAMED
// "level" (through levelStructureName — the structure name is the documented
// contract; the element name is not), and hand the reading to the OnLevels
// callback through the atomic. It runs on a streaming thread, so the
// level-reading helper is held to the same no-logging rule
// TestBusHandlerDoesNotLogOnTheStreamingThread pins for the handler itself.
func TestBusHandlerForwardsLevelMessages(t *testing.T) {
	fset, file := parseSource(t, captureCgoSourceFile)
	body := funcBody(t, fset, file, "cgoCapture", "onBusMessage")

	for _, want := range []string{
		"gogst.MessageElement",
		"levelStructureName",
		"levelsFromStructure(",
		"c.onLevels.Load()",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("onBusMessage no longer contains %q; the real build would post level "+
				"messages that nothing reads and the meters would sit empty at Gate B only", want)
		}
	}

	// levelsFromStructure is package-level in gst_cgo.go and is read by whichever
	// bus handler is delivering, so it is fetched from that file rather than from
	// this one's.
	cgoFset, cgoFile := parseSource(t, cgoSourceFile)
	helper := funcBody(t, cgoFset, cgoFile, "", "levelsFromStructure")
	if strings.Contains(helper, "log.") {
		t.Error("levelsFromStructure calls into the log package on a GStreamer streaming thread; " +
			"see TestBusHandlerDoesNotLogOnTheStreamingThread for why that is forbidden")
	}
}

// TestStartPublishesOnLevelsBeforeBuildingThePipeline guards an ordering that
// is easy to lose in a refactor: the level element starts posting the moment
// the pipeline reaches PLAYING, which happens inside Start while p.mu is held,
// and onBusMessage reads p.onLevels without taking p.mu — so the callback must
// be published (through the atomic) BEFORE gst_parse_launch ever runs, or the
// first frames of every session race the store.
func TestStartPublishesOnLevelsBeforeBuildingThePipeline(t *testing.T) {
	lines := strings.Split(captureStartSequence(t), "\n")

	store := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "c.onLevels.Store(")
	})
	parse := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "gogst.ParseLaunch(")
	})
	if store < 0 {
		t.Fatal("the capture build never stores CaptureOpts.OnLevels; the real build would drop " +
			"every level message")
	}
	if parse < 0 {
		t.Fatal("the capture build no longer calls gogst.ParseLaunch; re-derive this guard from " +
			"the new shape")
	}
	if store > parse {
		t.Error("the capture build stores OnLevels after building the pipeline; level messages " +
			"posted during startup race the store on a streaming thread")
	}
}

// TestStartPublishesOnChannelLevelsBeforeBuildingThePipeline is the same
// guarantee for the PER-CHANNEL picker, and it matters more rather than less.
//
// The programme meter that drops its first few frames looks like a meter coming
// up. The picker is what the operator is staring at while they decide whether
// the mapping screen works at all, so frames dropped at the start read as a dead
// meter and send them looking for the fault somewhere else entirely.
func TestStartPublishesOnChannelLevelsBeforeBuildingThePipeline(t *testing.T) {
	lines := strings.Split(captureStartSequence(t), "\n")

	store := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "c.onChannelLevels.Store(")
	})
	parse := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "gogst.ParseLaunch(")
	})
	if store < 0 {
		t.Fatal("the capture build never stores CaptureOpts.OnChannelLevels; the per-channel " +
			"meters would have no consumer and every chlevel frame would be dropped")
	}
	if parse < 0 {
		t.Fatal("the capture build no longer calls gogst.ParseLaunch; re-derive this guard from " +
			"the new shape")
	}
	if store > parse {
		t.Error("the capture build stores OnChannelLevels after building the pipeline; the level " +
			"element starts posting the moment the pipeline reaches PLAYING, which happens inside " +
			"the build")
	}
}

// TestBusHandlerRoutesEachMeterToItsOwnCallback is the cross-wire guard, at the
// receive end.
//
// The two level elements post an identically named structure, so the ONLY thing
// keeping the picker's sixteen-entry frames out of the programme meter is that
// each kind loads a different callback. MEASURED with both in one pipeline: 39
// level messages a second, every one of them matching the structure name. A
// programme meter fed alternately with two-entry and sixteen-entry frames does
// not fail — it flickers between two different signals, which is a meter reading
// as live while showing something that is not going to air.
func TestBusHandlerRoutesEachMeterToItsOwnCallback(t *testing.T) {
	fset, file := parseSource(t, captureCgoSourceFile)
	body := funcBody(t, fset, file, "cgoCapture", "onBusMessage")

	if !strings.Contains(body, "c.onChannelLevels.Load()") {
		t.Error("onBusMessage no longer loads onChannelLevels; chlevel's frames would be dropped " +
			"and the routing screen's meters would never move, with nothing in the log to say why")
	}
	prog := strings.Index(body, "c.onLevels.Load()")
	chans := strings.Index(body, "c.onChannelLevels.Load()")
	if prog < 0 || chans < 0 {
		t.Fatal("onBusMessage no longer loads both meter callbacks; re-derive this guard")
	}
	// Each callback is loaded exactly once. Two loads of the same field would be
	// two delivery paths for one meter, which is how a frame ends up on both.
	if strings.Count(body, "c.onLevels.Load()") != 1 || strings.Count(body, "c.onChannelLevels.Load()") != 1 {
		t.Error("a meter callback is loaded more than once in onBusMessage; there must be exactly " +
			"one delivery path per meter, or a frame can reach both")
	}
}

// TestPerChannelMeterIsArmedFromTheMatrixDecision pins the property that keeps a
// seat with no capture card exactly as it was.
//
// chlevel is built with post-messages=false, so it is silent until something
// arms it, and the condition it is armed on is "a mix matrix was written" —
// which is precisely the statement "this source presents channels nobody has
// positioned". Every microphone and every Dante endpoint presents a positioned
// stereo pair, for which this meter would report the same two numbers the
// programme meter already reports: a duplicate, ten times a second, over the
// webview bridge, for a whole match.
func TestPerChannelMeterIsArmedFromTheMatrixDecision(t *testing.T) {
	descFset, descFile := parseSource(t, captureDescSourceFile)
	desc := funcBody(t, descFset, descFile, "", "captureDescription")
	if !strings.Contains(desc, "post-messages=false") {
		t.Error("the per-channel level element is no longer built with post-messages=false; " +
			"every native capture would start posting frames nobody asked for")
	}

	start := captureStartSequence(t)
	arm := strings.Index(start, "c.armChannelMeterLocked(")
	if arm < 0 {
		t.Fatal("the capture build never arms the per-channel meter; it is built silent, so the " +
			"routing screen's meters would never move on any machine")
	}

	// THE CONDITION ITSELF CHANGED WITH R2 AND THE CHANGE IS THE POINT. It used
	// to be "a matrix was written", which was the same question as "this source is
	// unpositioned" only while a matrix was written conditionally. Every seat now
	// carries one — measured working at 1, 2, 3, 16 and 32 — so that test would arm
	// sixteen bars on a stereo microphone. The condition is now the width itself,
	// and the cost argument it encodes survives intact: a stereo seat still never
	// pushes a duplicate of the programme meter over the bridge ten times a second.
	// channelMeterWanted is the single expression both twins ask, so it is named
	// here rather than restated.
	if !strings.Contains(start[arm:], "c.wantChannelMeter(") &&
		!strings.Contains(start[arm:], "channelMeterWanted(") {
		t.Error("the per-channel meter is no longer armed through the shared condition; " +
			"channelMeterWanted is the one place the width test and the callback test live, and " +
			"a second copy of it would eventually disagree invisibly")
	}
}

// TestStopStopsTheSignalWatchdogBeforeTheTeardown is an ordering guard on the
// one thing in Stop that touches a GStreamer element from another goroutine.
//
// The watchdog's reader closure holds the capture element. teardownLocked takes
// the pipeline to NULL and drops it, so a poll that ran afterwards would be a
// property read on freed memory. Stop JOINS, which additionally guarantees no
// report can arrive after the session has ended — a lamp coming back on for a
// pipeline that no longer exists is the direction this project never lets a
// status display be wrong in.
func TestStopStopsTheSignalWatchdogBeforeTheTeardown(t *testing.T) {
	// The capture's Stop and its teardown are read as ONE TEXT, in the order they
	// execute, because the watchdog stop moved INSIDE teardownLocked when the
	// capture layer was cut: the build's own abort() path runs the same teardown,
	// and a watchdog stopped only in Stop would outlive every failed build.
	fset, file := parseSource(t, captureCgoSourceFile)
	lines := strings.Split(funcBody(t, fset, file, "cgoCapture", "teardownLocked"), "\n")

	stop := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "sigWatch.Stop()")
	})
	teardown := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "BlockSetState(gogst.StateNull")
	})
	if stop < 0 {
		t.Fatal("the capture teardown never stops the signal watchdog; its goroutine would " +
			"outlive the pipeline and go on reading a property off a disposed element")
	}
	if teardown < 0 {
		t.Fatal("the capture teardown no longer takes the pipeline to NULL; re-derive this guard " +
			"from the new shape")
	}
	if stop > teardown {
		t.Error("the capture teardown takes the pipeline to NULL before stopping the watchdog; " +
			"the reader closure holds the capture element, and a read after disposal is a read on " +
			"freed memory")
	}
}

// TestStartStartsTheWatchdogOnlyAfterPlaying pins the other half of the
// watchdog's lifecycle: it is the last thing Start does before returning nil, so
// that no abort() path can leak the goroutine.
func TestStartStartsTheWatchdogOnlyAfterPlaying(t *testing.T) {
	lines := strings.Split(captureStartSequence(t), "\n")

	watch := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "startSignalWatch(")
	})
	play := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "gogst.StatePlaying")
	})
	abort := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "return abort(")
	})
	if watch < 0 {
		t.Fatal("the capture build never starts the signal watchdog; the lamp would stay UNKNOWN " +
			"for the whole of every match and the one fault nothing else can see would be invisible")
	}
	if play < 0 {
		t.Fatal("the capture build no longer sets the pipeline PLAYING; re-derive this guard")
	}
	if watch < play {
		t.Error("the signal watchdog is started before the pipeline is PLAYING; it would be " +
			"polling an element that has not opened its device")
	}
	if abort >= 0 && watch < abort {
		t.Error("the signal watchdog is started before the build's last abort() path; a failure " +
			"after it would leak the goroutine and its ticker for the life of the process")
	}
}

// --- The channel map ------------------------------------------------------
//
// The tests below drive the STUB, but almost everything they assert is decided
// by the shared model in channelmap.go, which the real twin writes from
// unchanged. What is genuinely stub-specific is the plumbing: which pipeline
// states accept a map, what a positioned device does, and that a refused write
// leaves the previous map in force.

// TestCgoPipelineSizesTheMatrixFromTheNegotiatedPad is a source guard over the
// half of this mechanism Gate A cannot run.
//
// The decisive property is that SetChannelMap takes NO WIDTH FROM ITS CALLER.
// It reads the pad's negotiated caps itself, so a caller cannot pass a stale
// count saved in config.json or the max-channels the device advertises. A
// matrix of the wrong width does not attenuate or misroute — measured on the
// card, a 2x8 matrix written live onto a sixteen-channel stream was accepted by
// the property and killed the capture chain before the next level message.
func TestCgoPipelineSizesTheMatrixFromTheNegotiatedPad(t *testing.T) {
	fset, file := parseSource(t, captureCgoSourceFile)

	set := funcBody(t, fset, file, "cgoCapture", "SetChannelMap")
	if !strings.Contains(set, "c.negotiatedInputChannels()") {
		t.Error("SetChannelMap no longer reads the width off the pad; the only widths it may use " +
			"are the pad's own, because every other source of one can be stale")
	}
	if !strings.Contains(set, "c.writeChannelMapLocked(m, width)") {
		t.Error("SetChannelMap no longer writes through writeChannelMapLocked, which is where " +
			"validation happens before anything reaches the element")
	}

	write := funcBody(t, fset, file, "cgoCapture", "writeChannelMapLocked")
	matrix := strings.Index(write, "m.MixMatrix(width)")
	arg := strings.Index(write, "gogst.UtilSetObjectArg(")
	if matrix < 0 || arg < 0 {
		t.Fatal("writeChannelMapLocked has been restructured out of the shape this guard reads; " +
			"re-derive it from the new one rather than deleting it")
	}
	if matrix > arg {
		t.Error("writeChannelMapLocked writes the property before validating the map. " +
			"audioconvert rejects an out-of-range coefficient SILENTLY and leaves the previous " +
			"matrix in force, so validation after the write can never happen")
	}

	// The matrix has to be in place before the pipeline leaves NULL: it is a
	// negotiation constraint, not a gain on a running stream. Measured —
	// sixteen unpositioned channels into this chain with no matrix die 0.069 s
	// after PLAYING with not-negotiated (-4).
	lines := strings.Split(captureStartSequence(t), "\n")
	apply := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "c.applyStartChannelMapLocked(")
	})
	playing := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "gogst.StatePlaying")
	})
	if apply < 0 || playing < 0 {
		t.Fatal("the capture build no longer applies a channel map before going to PLAYING; " +
			"re-derive this guard from the new shape")
	}
	if apply > playing {
		t.Error("the capture build writes the mix-matrix after the state change. A matrix is a " +
			"negotiation constraint, not a gain: a pipeline that reaches PLAYING without one on " +
			"an unpositioned device never reaches it at all")
	}
}
