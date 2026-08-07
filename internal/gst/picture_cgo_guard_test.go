//go:build !cgo || gststub

// Structural guards over picture_cgo.go.
//
// Same instrument, same limits and the same reason as return_cgo_guard_test.go:
// the pipeline in that file needs GStreamer, an SRT peer and a GPU, so it only
// compiles at Gate B and cannot be unit-tested. What can be checked without it
// is that the source still says what it has to say, using the parseSource and
// funcBody helpers from gst_stub_test.go. Comments are stripped before the
// search, so a paragraph discussing a call cannot satisfy a guard looking for
// the call itself — which matters here more than usual, because the properties
// below are documented at length in picture.go and the documentation would
// otherwise keep the guard green after the code had gone.
package gst

import (
	"strings"
	"testing"
)

// pictureCgoSourceFile is the picture path's cgo half.
const pictureCgoSourceFile = "picture_cgo.go"

// TestPictureSinkDoesNotSyncToTheClock is the guard on the second of latency
// the operator reported, and it is written at two levels for the reason
// TestOverlayCarriesNoParentNotify gives: the constant is where the value gets
// flipped and the call site is where the constant gets bypassed. Neither check
// can see the other's failure.
//
// # Why this property needs a guard at all
//
// Because it looks like noise. `sync` is a GstBaseSink default, setting it to
// false reads like the sort of line that gets deleted when somebody tidies the
// element setup, the pipeline keeps working either way, and the only symptom is
// that the commentator's picture goes back to being about a second behind the
// match they are describing. Nothing fails, nothing logs, and the regression is
// indistinguishable from a network problem.
//
// Measured on the live instance, 2026-08-07: the srtsrc-to-sink transit time was
// 993.7 ms with sync=true and 1.2 ms with sync=false. The full argument, and the
// one condition that would make sync=true correct again, is at pictureSinkSync.
func TestPictureSinkDoesNotSyncToTheClock(t *testing.T) {
	if pictureSinkSync {
		t.Fatal("pictureSinkSync is true. d3d11videosink will hold every frame until its " +
			"running time arrives on the pipeline clock — measured at 855 ms of reported " +
			"pipeline latency, 993.7 ms of real transit — and there is no audio in this " +
			"pipeline for that wait to be keeping the video in step with. The commentator's " +
			"picture is now about a second behind the match. See pictureSinkSync.")
	}

	fset, file := parseSource(t, pictureCgoSourceFile)
	body := funcBody(t, fset, file, "picturePipeline", "buildLocked")

	if !strings.Contains(body, `p.sink.SetObjectProperty("sync", pictureSinkSync)`) {
		t.Fatal("buildLocked no longer sets \"sync\" on the video sink from pictureSinkSync. " +
			"The property defaults to TRUE, so a deleted line is not a neutral change: it is " +
			"the whole second of latency coming back, silently.")
	}

	// Unguarded on purpose. hasProperty(p.sink, "sync") would look defensive and
	// would turn a renamed or substituted sink into a silent revert — the guard
	// fails to find the property, the line is skipped, and the default of true
	// applies with nothing said. "sync" belongs to GstBaseSink; every sink has it.
	if strings.Contains(body, `hasProperty(p.sink, "sync")`) {
		t.Error("buildLocked guards the \"sync\" property with hasProperty. Every GStreamer sink " +
			"has it, so the guard can only ever suppress the assignment — and a suppressed " +
			"assignment here restores the default of true without a word in the log.")
	}
}

// TestPictureSinkQoSIsDecidedAndNotInherited pins the property that pairs with
// sync above.
//
// d3d11videosink's qos default is TRUE. With sync=false the sink never consults
// the clock, so no buffer is ever measured as late and the lateness figure QoS
// drops frames on does not exist — measured on the live instance, every one of
// 1290 buffers logged "sync disabled" with a jitter of exactly zero, and no run
// dropped a frame or emitted a QoS event. Leaving it enabled therefore changes
// nothing today and leaves a frame-dropping heuristic armed against a deadline
// the sink no longer honours, which upstream d3d11h265dec would act on by
// skipping frames. See pictureSinkQoS.
func TestPictureSinkQoSIsDecidedAndNotInherited(t *testing.T) {
	if pictureSinkQoS {
		t.Fatal("pictureSinkQoS is true while pictureSinkSync is false. QoS would be computed " +
			"from a render deadline the sink does not honour. If sync has come back, this is " +
			"correct and this test should change with it; if it has not, this is judder on a " +
			"picture that was never late.")
	}

	fset, file := parseSource(t, pictureCgoSourceFile)
	body := funcBody(t, fset, file, "picturePipeline", "buildLocked")

	if !strings.Contains(body, `p.sink.SetObjectProperty("qos", pictureSinkQoS)`) {
		t.Fatal("buildLocked no longer sets \"qos\" on the video sink from pictureSinkQoS. " +
			"The element default is true, so the pair (sync, qos) would be half decided and " +
			"half inherited — which is the state this constant exists to end.")
	}
}

// TestPictureSrcStillTakesItsLatencyFromTheOptions guards the other half of the
// operator's control.
//
// PictureOpts.LatencyMs is now fed by its own configuration field,
// config.PictureLatencyMs, rather than by the contribution feed's SRTLatencyMs.
// That is worth nothing if configureSrcLocked stops passing it to srtsrc, which
// would leave the Settings control apparently working and attached to nothing.
func TestPictureSrcStillTakesItsLatencyFromTheOptions(t *testing.T) {
	fset, file := parseSource(t, pictureCgoSourceFile)
	body := funcBody(t, fset, file, "picturePipeline", "configureSrcLocked")

	if !strings.Contains(body, `SetObjectProperty("latency", int32(opts.LatencyMs))`) {
		t.Fatal("configureSrcLocked no longer sets srtsrc's latency from opts.LatencyMs. " +
			"The Picture buffer control in Settings now reaches nothing, and it will look " +
			"to the operator exactly like the M2L-X floor they were warned about.")
	}
}
