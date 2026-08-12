//go:build !cgo || gststub

// Structural guards over return_cgo.go.
//
// The pipeline in that file cannot be unit-tested: it needs GStreamer, an SRT
// peer and a sound card, and it only compiles at Gate B. So the properties that
// matter about it are asserted against its SOURCE, exactly as gst_stub_test.go
// does for gst_cgo.go, using the same parseSource/funcBody helpers. Comments are
// stripped before the search, so a note discussing a call cannot satisfy a guard
// looking for it.
//
// A source guard is weaker evidence than a run — it proves the shape of the code
// and not its behaviour. Where a property can be lifted out into pure Go it is,
// and tested for real: fakeSinkName lives in return.go and is exercised by
// TestFakeSinkNamesCannotCollideAcrossAttempts. What is left here is the part
// that cannot be lifted, which is that the cgo half actually calls it.
package gst

import (
	"strings"
	"testing"
)

// returnCgoSourceFile is the return path's cgo half.
const returnCgoSourceFile = "return_cgo.go"

// TestFakeSinkForNamesEveryElementUniquely guards a monitor stuck in backoff for
// a whole match against a healthy M2L-X.
//
// The pipeline is reused across every address resolveSinkHost returned. tsdemux
// names its pads after the PID, so address 2 sees the same pad names as address
// 1, and gst_bin_add refuses a duplicate name in a bin. A fakesink name built
// from the pad name alone therefore fails to be added on the second address, and
// the undecoded pad is left unlinked — the wedge the fakesink exists to prevent.
//
// See fakeSinkName in return.go, which carries the full argument and is where
// the property itself is tested.
func TestFakeSinkForNamesEveryElementUniquely(t *testing.T) {
	fset, file := parseSource(t, returnCgoSourceFile)
	body := funcBody(t, fset, file, "returnPipeline", "fakeSinkFor")

	if !strings.Contains(body, "fakeSinkName(") {
		t.Fatal("fakeSinkFor no longer builds its element name with fakeSinkName. " +
			"A name derived from the pad alone repeats across address attempts, gst_bin_add " +
			"refuses it, and the pad it was for is left unlinked — which wedges the demuxer.")
	}
	if !strings.Contains(body, "r.fakeSeq.Add(1)") {
		t.Fatal("fakeSinkFor does not advance the per-pipeline sequence number. " +
			"Without it fakeSinkName is called with a constant and the names collide again.")
	}
	if strings.Contains(body, `nameReturnFakeSink+"-"+pad.GetName()`) {
		t.Fatal("fakeSinkFor still concatenates the pad name directly into the element name")
	}
}

// TestFakeSinkForRecordsOnlyWhatItAdded guards the retry path's removal.
//
// r.fakeSinks means "elements this pipeline put in the bin and must take back
// out". Recording one before gst_bin_add has accepted it would have
// toNullForRetry call gst_bin_remove on an element that was never in the bin.
func TestFakeSinkForRecordsOnlyWhatItAdded(t *testing.T) {
	fset, file := parseSource(t, returnCgoSourceFile)
	body := funcBody(t, fset, file, "returnPipeline", "fakeSinkFor")

	add := strings.Index(body, "pipeline.Add(fake)")
	record := strings.Index(body, "r.fakeSinks = append(")
	if add < 0 || record < 0 {
		t.Fatal("fakeSinkFor no longer both adds the sink to the bin and records it")
	}
	if record < add {
		t.Error("fakeSinkFor records the fakesink before gst_bin_add has accepted it; " +
			"a failed add leaves an element in the list that is not in the bin")
	}
}

// TestToNullForRetryClearsTheFakeSinks guards the bin growing without bound.
//
// Between address attempts the pipeline goes back to NULL and is reused. Every
// fakesink the last attempt's demuxer caused to be built is still parented to
// it, linked to a pad that no longer exists. Unique names mean the next attempt
// still works, so this is hygiene rather than the fix — but an M2L-X host with
// several A records, reconnecting all afternoon, accumulates one dead sink per
// undecoded pad per attempt for the life of the monitor.
func TestToNullForRetryClearsTheFakeSinks(t *testing.T) {
	fset, file := parseSource(t, returnCgoSourceFile)
	body := funcBody(t, fset, file, "returnPipeline", "toNullForRetry")

	if !strings.Contains(body, "r.fakeSinks = nil") {
		t.Fatal("toNullForRetry does not clear r.fakeSinks. The list is only emptied by " +
			"teardownLocked, so every retried attempt appends to the previous one's.")
	}
	if !strings.Contains(body, "r.pipeline.Remove(") {
		t.Fatal("toNullForRetry drops its references to the fakesinks without removing them " +
			"from the bin, which leaves them parented to a pipeline that is about to be reused")
	}

	// Order: the pipeline reaches NULL before anything is unparented. Removing an
	// element from a bin that is still rolling races the thread pushing into it.
	toNull := strings.Index(body, "BlockSetState(gogst.StateNull")
	remove := strings.Index(body, "r.pipeline.Remove(")
	if toNull < 0 {
		t.Fatal("toNullForRetry no longer takes the pipeline to NULL")
	}
	if remove < toNull {
		t.Error("toNullForRetry removes elements from the bin before the pipeline has " +
			"reached NULL; the streaming threads are still running at that point")
	}
}

// TestListOutputDevicesRejectsCaptureIDs is the headphone dropdown's mirror of
// ListInputDevices' namespace filter.
//
// Measured on the dev machine, the Audio/Sink endpoint-id set was a byte-exact
// subset of Audio/Source — the loopback republication means the two dropdowns
// can offer the same id string — so nothing structural stops a CAPTURE
// endpoint appearing as headphones. wasapi2sink then falls back to the system
// default playback device SILENTLY, and the commentator gets audio in the
// wrong ears with nothing anywhere saying why. Only a POSITIVELY identified
// capture id is skipped; unrecognised shapes stay offered, the same asymmetry
// ListInputDevices applies in the other direction.
func TestListOutputDevicesRejectsCaptureIDs(t *testing.T) {
	fset, file := parseSource(t, returnCgoSourceFile)
	body := funcBody(t, fset, file, "", "ListOutputDevices")

	if !strings.Contains(body, "IsCaptureEndpointID(") {
		t.Error("ListOutputDevices no longer skips capture-namespace endpoint ids; a microphone " +
			"can be offered — and persisted — as the commentator's headphones")
	}
}

// TestReturnTeardownDoesNotDetachTheBusSyncHandler is the return path's copy of
// the guard gst_stub_test.go puts on the send path.
//
// gst_bus_post reads the sync handler under GST_OBJECT_LOCK, unlocks, then calls
// it. Detaching it runs the GDestroyNotify that unregisters the Go closure, so a
// streaming thread that already read the pointer enters go-gst's exported
// callback, finds no closure and panics inside an //export'ed cgo function. A Go
// panic cannot unwind through C: the process dies.
func TestReturnTeardownDoesNotDetachTheBusSyncHandler(t *testing.T) {
	fset, file := parseSource(t, returnCgoSourceFile)
	body := funcBody(t, fset, file, "returnPipeline", "teardownLocked")

	if strings.Contains(body, "SetSyncHandler(nil)") {
		t.Fatal("the return pipeline's teardown detaches the bus sync handler; " +
			"silence it with busSilenced instead")
	}
	if !strings.Contains(body, "r.busSilenced.Store(true)") {
		t.Fatal("the return pipeline's teardown neither detaches nor silences the bus handler")
	}
}
