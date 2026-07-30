//go:build !cgo

package gst

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"runtime"
	"strings"
	"testing"
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
	t.Fatalf("%s has no function %s (receiver %q)", cgoSourceFile, name, receiver)
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
