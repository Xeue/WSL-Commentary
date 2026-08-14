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
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestListInputDevicesReturnsFakes(t *testing.T) {
	SetStubDevices(nil)
	devices, err := ListInputDevices()
	if err != nil {
		t.Fatalf("ListInputDevices: %v", err)
	}
	if len(devices) != 3 {
		t.Fatalf("want 3 fake devices, got %d", len(devices))
	}
	if devices[0].Name != "DVS Receive  1-2 (Dante Virtual Soundcard)" {
		t.Errorf("first device name = %q", devices[0].Name)
	}
	if devices[0].ID == devices[0].Name {
		t.Error("device ID must be the endpoint GUID, not the display name")
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

func TestStartRequiresSlateAndDevice(t *testing.T) {
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{AudioDeviceID: "{guid}"}); err == nil {
		t.Error("want error for empty SlatePath")
	}
	if err := p.Start(PipelineOpts{SlatePath: "slate.png"}); err == nil {
		t.Error("want error for empty AudioDeviceID")
	}
	if p.State() != StubStateStopped {
		t.Errorf("state after failed Start = %q, want %q", p.State(), StubStateStopped)
	}
}

func TestStartInstallsNoSink(t *testing.T) {
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: "{guid}"}); err != nil {
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
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: "{guid}"}); err != nil {
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
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: "{guid}"}); err != nil {
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
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: "{guid}"}); err != nil {
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
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: "{guid}"}); err != nil {
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
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := p.(*StubPipeline); !ok {
		t.Fatalf("New returned %T, want *StubPipeline", p)
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
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: "{guid}"}); err != nil {
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
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: "{guid}"}); err != nil {
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
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: "{guid}"}); err != nil {
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
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: "{guid}"}); err != nil {
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

// TestStartRefusesARenderEndpoint proves the refusal on the stub, which — via
// the shared refuseRenderEndpoint helper — is also a statement about the real
// twin. The fixture is the operator's actual playback endpoint id.
func TestStartRefusesARenderEndpoint(t *testing.T) {
	p := NewStubPipeline()
	err := p.Start(PipelineOpts{
		SlatePath:     "slate.png",
		AudioDeviceID: "{0.0.0.00000000}.{8678ce58-90c0-4827-8ff7-c9edd8d074ed}",
	})
	if err == nil {
		t.Fatal("Start accepted a RENDER endpoint as the commentary input; the pipeline would " +
			"preroll and then fail asynchronously with wasapi2's error 1551, which the sender " +
			"misreads as a network failure and retries forever")
	}
	if !errors.Is(err, ErrNotACaptureDevice) {
		t.Errorf("the refusal does not wrap ErrNotACaptureDevice: %v", err)
	}
	if p.State() != StubStateStopped {
		t.Errorf("state after the refusal = %q, want %q", p.State(), StubStateStopped)
	}
}

// TestStubDeviceListNamespacesAreContract pins the property the file headers
// of gst_stub.go and return_stub.go now promise: every fake capture id is in
// the capture namespace and every fake playback id is in the render
// namespace, per the classifier both twins share. A stub device that failed
// this would let a wiring bug — the headphone dropdown's value reaching
// PipelineOpts.AudioDeviceID, or vice versa — pass at Gate A and surface in a
// commentary booth.
func TestStubDeviceListNamespacesAreContract(t *testing.T) {
	for _, d := range defaultStubDevices {
		if !IsCaptureEndpointID(d.ID) {
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
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: "{guid}"}); err != nil {
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
func TestPipelineDescriptionHasNoCapsfilterAboveTheResampler(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	body := funcBody(t, fset, file, "", "pipelineDescription")

	if strings.Contains(body, "! audio/x-raw,rate=") {
		t.Fatal("the audio branch pins a sample rate upstream of audioresample; " +
			"a DVS endpoint at 44.1 or 96 kHz will fail negotiation at Start")
	}
	if !strings.Contains(body, "audioconvert ! audioresample") {
		t.Fatal("the audio branch no longer converts and resamples whatever the endpoint gives us")
	}
	if !strings.Contains(body, "audio/x-raw,format=S16LE,rate=48000,channels=2") {
		t.Fatal("nothing pins what enters mfaacenc any more")
	}
}

// TestPipelineDescriptionScalesTheSlate guards the next person to export the
// artwork. Without videoscale the slate PNG must be exactly 1920x1080 or Start
// fails caps negotiation, and videoconvertscale is already a required plugin,
// so the element is free.
func TestPipelineDescriptionScalesTheSlate(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	body := funcBody(t, fset, file, "", "pipelineDescription")

	if !strings.Contains(body, "videoscale") {
		t.Fatal("the slate branch has no videoscale; artwork that is not exactly 1920x1080 " +
			"fails caps negotiation at Start with no diagnostic naming the size")
	}
	if !strings.Contains(body, "width=1920,height=1080") {
		t.Fatal("the slate branch no longer pins 1920x1080")
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

// TestPlatformElementContractIsPinned checks the two factory names that the
// send pipeline swaps per platform, in both files, by exact value.
//
// pipelineDescription builds the audio branch out of captureSourceFactory and
// aacEncoderFactory rather than literals, which makes the ordering guards below
// portable — and makes it possible to repoint either port at a different
// element by editing one const, with nothing else in the package noticing. This
// is what stops that being a silent change.
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

	// And the pipeline string must actually be built from them, or pinning the
	// consts pins nothing.
	fset, file := parseSource(t, cgoSourceFile)
	body := funcBody(t, fset, file, "", "pipelineDescription")
	for _, want := range []string{"captureSourceFactory", "aacEncoderFactory"} {
		if !strings.Contains(body, want) {
			t.Errorf("pipelineDescription no longer uses %s; the audio branch has been hardcoded "+
				"to one platform's element again", want)
		}
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
	fset, file := parseSource(t, cgoSourceFile)
	lines := strings.Split(funcBody(t, fset, file, "cgoPipeline", "Start"), "\n")

	refuse := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "refuseRenderEndpoint(opts.AudioDeviceID)")
	})
	parse := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "ParseLaunch(")
	})
	play := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "BlockSetState(gogst.StatePlaying")
	})
	fatal := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "p.fatalError()")
	})
	started := lastLineMatching(lines, func(s string) bool {
		return s == "p.started = true"
	})

	if refuse < 0 {
		t.Error("Start never calls refuseRenderEndpoint(opts.AudioDeviceID); a playback endpoint " +
			"reaches wasapi2src and fails asynchronously as a fake network error")
	}
	if parse < 0 {
		t.Fatal("Start never calls ParseLaunch")
	}
	if refuse >= 0 && refuse > parse {
		t.Error("Start refuses a render endpoint only after building the pipeline; the refusal " +
			"must be synchronous and up front")
	}
	if play < 0 || started < 0 {
		t.Fatal("Start no longer has the BlockSetState(PLAYING) / p.started = true shape these " +
			"guards expect; re-read them before restructuring")
	}
	if fatal < 0 || fatal < play || fatal > started {
		t.Error("Start does not re-check p.fatalError() between BlockSetState and p.started = true; " +
			"an asynchronous wasapi2 open failure lets Start report a running pipeline whose " +
			"capture chain is already dead")
	}
}

// TestStartLogsTheEndpointID guards the diagnosability of the asynchronous
// failure. wasapi2src echoes the REQUESTED id verbatim in its error 1551
// (proved by probe), so a log line carrying opts.AudioDeviceID immediately
// before the property is set lets a field log match the failure to the
// request instead of leaving the id to be argued about.
func TestStartLogsTheEndpointID(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	body := funcBody(t, fset, file, "cgoPipeline", "Start")

	logged := false
	for idx := strings.Index(body, "log.Printf("); idx >= 0; {
		end := idx + 250
		if end > len(body) {
			end = len(body)
		}
		if strings.Contains(body[idx:end], "opts.AudioDeviceID") {
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
		t.Error("Start never logs opts.AudioDeviceID; wasapi2's asynchronous error 1551 quotes " +
			"the requested id verbatim, and without this line the log cannot say what was requested")
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

func TestStubEmitsLevelsWhileStarted(t *testing.T) {
	frames := make(chan Levels, 128)
	p := NewStubPipeline()
	err := p.Start(PipelineOpts{
		SlatePath:     "slate.png",
		AudioDeviceID: stubCaptureID,
		// Non-blocking capture, as the contract requires of the callback: the
		// stub's ticker goroutine must never wait on this test.
		OnLevels: func(l Levels) {
			select {
			case frames <- l:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	deadline := time.After(3 * time.Second)
	var got []Levels
	for len(got) < 4 {
		select {
		case l := <-frames:
			got = append(got, l)
		case <-deadline:
			t.Fatalf("only %d level frames arrived in 3s at a 50ms interval; want at least 4", len(got))
		}
	}

	for i, l := range got {
		if len(l.PeakDB) != levelStubChannels || len(l.RMSDB) != levelStubChannels {
			t.Fatalf("frame %d has %d peak / %d rms channels, want %d of each: the real "+
				"pipeline pins channels=2 and the fakes must match it",
				i, len(l.PeakDB), len(l.RMSDB), levelStubChannels)
		}
		for ch := range l.PeakDB {
			peak, rms := l.PeakDB[ch], l.RMSDB[ch]
			if peak < levelSilenceDB || peak > 0 {
				t.Errorf("frame %d channel %d peak = %v dBFS, outside [%d, 0]", i, ch, peak, levelSilenceDB)
			}
			if rms < levelSilenceDB || rms > 0 {
				t.Errorf("frame %d channel %d rms = %v dBFS, outside [%d, 0]", i, ch, rms, levelSilenceDB)
			}
			if rms > peak {
				t.Errorf("frame %d channel %d rms %v is above its peak %v; no real signal does that",
					i, ch, rms, peak)
			}
		}
	}
}

func TestStubLevelsStopWhenThePipelineStops(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	p := NewStubPipeline()
	err := p.Start(PipelineOpts{
		SlatePath:     "slate.png",
		AudioDeviceID: stubCaptureID,
		OnLevels: func(Levels) {
			mu.Lock()
			calls++
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}

	deadline := time.Now().Add(3 * time.Second)
	for count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("OnLevels never fired while the pipeline was started")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Stop JOINS the ticker goroutine, so the count is final the moment Stop
	// returns — a callback delivered after this line is the join being lost.
	after := count()
	time.Sleep(150 * time.Millisecond) // three ticker intervals of silence
	if got := count(); got != after {
		t.Fatalf("OnLevels fired %d more time(s) after Stop returned; the ticker was "+
			"signalled but not joined", got-after)
	}
}

func TestStubStartWithNilOnLevelsIsSafe(t *testing.T) {
	// nil OnLevels means no metering and no goroutine: the documented default.
	// This is the "does not panic and does not wedge Stop" case.
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: stubCaptureID}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

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

// TestPipelineDescriptionMetersWhatIsEncoded guards the level element's
// PLACEMENT, which is the whole point of the input meters: after audioconvert,
// audioresample and the S16LE/48k capsfilter, immediately before mfaacenc, so
// what the meter shows is what is actually encoded and sent. A level element
// moved upstream of the resample — or removed — would keep the meters moving
// (or silent) in ways that no longer describe the on-air signal, and nothing
// at Gate A would notice: the stub emits synthetic levels either way.
func TestPipelineDescriptionMetersWhatIsEncoded(t *testing.T) {
	fset, file := parseSource(t, cgoSourceFile)
	body := funcBody(t, fset, file, "", "pipelineDescription")

	level := strings.Index(body, "level name=alevel")
	if level < 0 {
		t.Fatal("the audio branch has no level element named alevel; the input meters have " +
			"nothing to measure and the levels event will never fire on a real build")
	}
	resample := strings.Index(body, "audioconvert ! audioresample")
	// The encoder is named through aacEncoderFactory rather than as a literal
	// since the macOS port — mfaacenc on Windows, atenc on macOS. The ORDER is
	// what this guard is about and it is unaffected;
	// TestPlatformElementContractIsPinned checks the two values themselves.
	enc := strings.Index(body, `aacEncoderFactory + " bitrate="`)
	if resample < 0 || enc < 0 {
		t.Fatal("the audio branch has been restructured; re-derive this guard from the new shape")
	}
	if !(resample < level && level < enc) {
		t.Error("the level element must sit AFTER audioresample and BEFORE the AAC encoder: it " +
			"exists to measure the exact signal that enters the encoder, not the endpoint's raw format")
	}
	if !strings.Contains(body, "interval=50000000") {
		t.Error("the level interval is no longer 50 ms (50000000 ns); the app-side throttle and " +
			"the UI's no-rAF rendering are both sized against 20 frames a second")
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
	fset, file := parseSource(t, cgoSourceFile)
	body := funcBody(t, fset, file, "cgoPipeline", "onBusMessage")

	for _, want := range []string{
		"gogst.MessageElement",
		"levelStructureName",
		"levelsFromStructure(",
		"p.onLevels.Load()",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("onBusMessage no longer contains %q; the real build would post level "+
				"messages that nothing reads and the meters would sit empty at Gate B only", want)
		}
	}

	helper := funcBody(t, fset, file, "", "levelsFromStructure")
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
	fset, file := parseSource(t, cgoSourceFile)
	lines := strings.Split(funcBody(t, fset, file, "cgoPipeline", "Start"), "\n")

	store := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "p.onLevels.Store(")
	})
	parse := lastLineMatching(lines, func(s string) bool {
		return strings.Contains(s, "gogst.ParseLaunch(")
	})
	if store < 0 {
		t.Fatal("Start never stores PipelineOpts.OnLevels; the real build would drop every level message")
	}
	if parse < 0 {
		t.Fatal("Start no longer calls gogst.ParseLaunch; re-derive this guard from the new shape")
	}
	if store > parse {
		t.Error("Start stores OnLevels after building the pipeline; level messages posted " +
			"during startup race the store on a streaming thread")
	}
}
