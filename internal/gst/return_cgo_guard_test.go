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
	"go/ast"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// The return path's cgo files.
//
// The two platform twins are parsed by name, not compiled, so BOTH of them are
// guarded from a single Gate A run on either operating system. That is worth
// more than it sounds: the whole hazard of a build-tagged seam is that half of
// it is invisible on the machine you are working on, and a source guard is the
// only thing in this repository that can see both halves at once.
const (
	returnCgoSourceFile        = "return_cgo.go"
	returnCgoWindowsSourceFile = "return_cgo_windows.go"
	returnCgoDarwinSourceFile  = "return_cgo_darwin.go"
	returnCgoOtherSourceFile   = "return_cgo_other.go"
)

// returnPlatformSourceFiles is every twin that implements the seam for real.
// return_cgo_other.go is deliberately absent: it implements nothing and the
// guards below would all fail on it, correctly.
var returnPlatformSourceFiles = []string{returnCgoWindowsSourceFile, returnCgoDarwinSourceFile}

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
//
// UPDATED FOR THE macOS PORT, and the change is a move rather than a weakening.
// The check now lives in playbackDeviceID in the WINDOWS twin, because it is
// Windows knowledge: wasapi2 republishes render endpoints into the Audio/Source
// class and CoreAudio does no such thing, so on macOS the class IS the direction
// and there is no id to classify. This asserts against the file that now carries
// it. See TestTheDarwinPredicateDoesNotClassifyIDs for the other half of that
// argument.
func TestListOutputDevicesRejectsCaptureIDs(t *testing.T) {
	fset, file := parseSource(t, returnCgoWindowsSourceFile)
	body := funcBody(t, fset, file, "", "playbackDeviceID")

	if !strings.Contains(body, "IsCaptureEndpointID(") {
		t.Error("the Windows enumeration predicate no longer skips capture-namespace endpoint ids; " +
			"a microphone can be offered — and persisted — as the commentator's headphones")
	}
	if !strings.Contains(body, `"wasapi2"`) {
		t.Error("the Windows enumeration predicate no longer restricts itself to the wasapi2 " +
			"provider; Device.ID is passed verbatim to wasapi2sink and no other provider's ids " +
			"mean anything to it")
	}
}

// ---------------------------------------------------------------------------
// The platform seam
// ---------------------------------------------------------------------------

// TestNeitherPlatformTwinSelectsLibav pins the rule in return_cgo.go's header
// across the port.
//
// The rule survived macOS at the cost of one capsfilter, and the temptation to
// undo that trade is real and specific: avdec_aac sits at primary rank on this
// Mac, it takes aacparse's ADTS output with no filter at all, and it decodes
// indistinguishably (-4.9525580303 dB against atdec's -4.9525580510 on the same
// file). It is also gst-libav, which is FFmpeg, which is the concern
// build/bundle-gst.ps1's forbidden list exists for. faad is worse: GPL.
//
// A guard rather than a comment, because the failure is invisible on the
// developer's machine — the plugin is installed there — and only appears as a
// missing dylib on an installed bundle, or as a licence problem that nobody
// notices at all.
func TestNeitherPlatformTwinSelectsLibav(t *testing.T) {
	for _, name := range returnPlatformSourceFiles {
		fset, file := parseSource(t, name)
		for _, fn := range []string{"returnDecodeSpecs", "returnRenderSpecs"} {
			body := funcBody(t, fset, file, "", fn)
			for _, forbidden := range []string{"avdec_", "avenc_", "faad", "libav"} {
				if strings.Contains(body, forbidden) {
					t.Errorf("%s: %s names %q. The AAC decoder must be an LGPL wrapper over a codec "+
						"that is part of the operating system — mfaacdec or atdec — and nothing else.",
						name, fn, forbidden)
				}
			}
		}
	}
}

// TestTheDarwinReturnStripsADTSBeforeTheDecoder guards the return path's most
// dangerous failure on macOS, because it is the one with no symptom.
//
// atdec ACCEPTS aacparse's default stream-format=adts caps. It negotiates, it
// reaches PLAYING, it produces buffers of the right size at the right rate, and
// every sample in them is zero — measured at the level element's -700 dB floor
// against -4.94 dB with the filter in place. There is no error, no warning and
// no bus message, so a monitor that has lost this capsfilter is a green RETURN
// lamp over silence, and the only way to find out is to put headphones on.
//
// The order is asserted as well as the presence: a filter placed AFTER the
// decoder would compile, would look right in a diff, and would do nothing.
func TestTheDarwinReturnStripsADTSBeforeTheDecoder(t *testing.T) {
	fset, file := parseSource(t, returnCgoDarwinSourceFile)
	body := funcBody(t, fset, file, "", "returnDecodeSpecs")

	caps := strings.Index(body, "returnAACRawCaps")
	dec := strings.Index(body, `"atdec"`)
	switch {
	case caps < 0:
		t.Fatal("the macOS decode chain no longer pins returnAACRawCaps. atdec decodes ADTS to " +
			"digital silence with no error anywhere; without this filter the monitor is a green " +
			"lamp over nothing.")
	case dec < 0:
		t.Fatal("the macOS decode chain no longer names atdec")
	case caps > dec:
		t.Error("the macOS decode chain sets the raw-AAC caps AFTER the decoder, where they do " +
			"nothing. aacparse is what strips the ADTS headers, so the filter must be upstream " +
			"of atdec.")
	}
}

// TestTheDarwinRenderTailReconcilesTheChannelCount guards channel selection on
// exactly the devices it was written for.
//
// MixMatrix always returns 2x2 and audioconvert takes BOTH its pad channel
// counts from it. A CoreAudio device need not be stereo — the NDI Audio device
// on the dev Mac is six channels and nothing else — and linking the matrix
// straight at one fails negotiation hard enough to stop the whole transport
// (not-negotiated (-4) out of mpegts_base_loop). So the macOS tail pins the
// matrix's two channels and puts a SECOND audioconvert after the resampler to
// meet the device.
//
// Losing either half puts the monitor back to working only on stereo devices,
// which is to say: on the developer's laptop and not at the facility.
func TestTheDarwinRenderTailReconcilesTheChannelCount(t *testing.T) {
	fset, file := parseSource(t, returnCgoDarwinSourceFile)
	body := funcBody(t, fset, file, "", "returnRenderSpecs")

	if !strings.Contains(body, "returnStereoCaps") {
		t.Error("the macOS render tail no longer pins the mix matrix's two channels; a multichannel " +
			"CoreAudio device fails negotiation and stops the transport")
	}
	if !strings.Contains(body, `"audioconvert"`) {
		t.Error("the macOS render tail no longer ends in an audioconvert, so nothing reconciles the " +
			"pinned two channels with a device that wants six")
	}
	if strings.Index(body, "returnStereoCaps") > strings.Index(body, `"audioconvert"`) {
		t.Error("the macOS render tail converts to the device's channel count before pinning two, " +
			"which is the wrong way round: the pin exists to protect the mix matrix")
	}
}

// TestTheDarwinSinkDeviceIsSetAsAnInteger guards the showstopper the port was
// written to find.
//
// osxaudiosink's device property is a gint AudioDeviceID, not a string.
// setStringProperty checks only that a property EXISTS, so handing it this one
// passes the guard, emits a GLib CRITICAL to stderr, returns nil, leaves the
// property at 0 — and plays the return to the system default device with a green
// RETURN lamp and no diagnosis anywhere.
func TestTheDarwinSinkDeviceIsSetAsAnInteger(t *testing.T) {
	fset, file := parseSource(t, returnCgoDarwinSourceFile)
	body := funcBody(t, fset, file, "", "configureReturnSinkLocked")

	if strings.Contains(body, "setStringProperty(") {
		t.Fatal("the macOS sink is being given its device as a STRING. osxaudiosink's device " +
			"property is a gint; the setter would emit a GLib CRITICAL, return nil, and leave the " +
			"monitor on the default device with nothing anywhere saying so.")
	}
	if !strings.Contains(body, "setIntProperty(") {
		t.Fatal("the macOS sink no longer sets its device with the type-checked integer setter")
	}
	if !strings.Contains(body, "resolveCoreAudioDeviceIndex(") {
		t.Fatal("the macOS sink is no longer resolving the CoreAudio UID to an AudioDeviceID at " +
			"open time. The integer is a runtime handle coreaudiod reuses; persisting one produces " +
			"a monitor that refuses to start after an unrelated device is replugged.")
	}
}

// TestTheDarwinPredicateDoesNotClassifyIDs is the other half of the argument in
// TestListOutputDevicesRejectsCaptureIDs, and it guards against the obvious
// wrong repair.
//
// The bug that started the macOS port is a `device.api != "wasapi2"` test that is
// false for every device on this platform. Adding `|| api == "osxaudio"` fails
// the SAME way, because osxaudiodeviceprovider publishes no device.api at all —
// measured, a macOS audio device's whole property structure is is-default and
// unique-id. And device_id.go's namespace classifiers are Windows knowledge that
// no CoreAudio UID can match, so applying them here would be dead code pretending
// to be a safety check.
func TestTheDarwinPredicateDoesNotClassifyIDs(t *testing.T) {
	fset, file := parseSource(t, returnCgoDarwinSourceFile)
	body := funcBody(t, fset, file, "", "playbackDeviceID")

	if strings.Contains(body, "device.api") {
		t.Error("the macOS enumeration predicate tests device.api, which osxaudiodeviceprovider " +
			"does not publish. It would match nothing and empty the headphone dropdown — the same " +
			"bug as the capture side's, repeated.")
	}
	for _, dead := range []string{"IsCaptureEndpointID(", "IsRenderEndpointID("} {
		if strings.Contains(body, dead) {
			t.Errorf("the macOS enumeration predicate calls %s. Those classify WINDOWS endpoint id "+
				"namespaces; no CoreAudio UID matches either, so this is dead code wearing the "+
				"appearance of a check.", dead)
		}
	}
	if !strings.Contains(body, "propUniqueID") {
		t.Error("the macOS enumeration predicate no longer keys on unique-id, which is the only " +
			"stable identity CoreAudio devices publish")
	}
}

// windowsReturnDecodeSpecs and windowsReturnRenderSpecs are the two Windows
// element lists as they must appear, character for character with the comments
// stripped — which is what funcBody renders.
//
// # Why the whole text and not a search for the element names
//
// This test used to be six strings.Contains checks, and they were satisfied by
// mutations that genuinely changed what the operator's machine plays. A
// containment check cannot see element ORDER, cannot see element COUNT, and
// cannot see a field that has been added to a spec — so an extra audioconvert
// appended to the render tail, a second decoder in front of mfaacdec, or a
// factory pointed at the wrong name const all passed a test whose whole purpose
// was to say the Windows tail had not moved. Five such mutations were tried
// against it and all five passed.
//
// The exact text is not a nicety here. Gate A on a Mac never compiles this file
// — it is cgo && windows — so the only thing standing between an edit made here
// while working on macOS and the on-air machine is a source guard, and a source
// guard that cannot see the change is not a guard.
//
// The cost is that a DELIBERATE change to the Windows tail fails this test. That
// is the intent: CONTRACT.md rule 4 makes the on-air Windows build the thing a
// port may not regress, so touching it has to be a decision that is written
// down. Update the literal here, in the same commit, and say in the message what
// was measured on the operator's machine.
const windowsReturnDecodeSpecs = `func returnDecodeSpecs() []returnElementSpec {
	return []returnElementSpec{
		{factory: "mfaacdec", name: nameReturnDecode},
	}
}`

const windowsReturnRenderSpecs = `func returnRenderSpecs() []returnElementSpec {
	return []returnElementSpec{
		{factory: "audioresample", name: nameReturnResample},
	}
}`

// TestTheWindowsTailIsUnchangedByThePort is the promise the port made to the
// operator's machine.
//
// Every line of return_cgo_windows.go was lifted out of return_cgo.go unmodified.
// mfaacdec takes aacparse's output as it comes, the render tail is the resampler
// and nothing else, the sink is wasapi2sink and takes the endpoint id as a
// string, and low-latency is set to true. If the macOS work ever has to reach
// into the Windows tail, that is a decision somebody makes on purpose and this
// test is where they record it.
//
// The two element lists are pinned as whole text; see the constants above for
// why containment checks were not enough. The sink function is not, because it
// is prose as well as code — its log wording is free to improve — so the three
// facts about it that reach the sound card are pinned individually instead.
func TestTheWindowsTailIsUnchangedByThePort(t *testing.T) {
	fset, file := parseSource(t, returnCgoWindowsSourceFile)

	for _, tc := range []struct{ fn, want, why string }{
		{
			fn:   "returnDecodeSpecs",
			want: windowsReturnDecodeSpecs,
			why: "mfaacdec advertises stream-format={raw,adts} and negotiates whatever aacparse " +
				"offers, so the Windows decode chain is the decoder and nothing else. The " +
				"capsfilter on macOS is there because atdec silently decodes ADTS to digital " +
				"silence, and it must not be copied across on the assumption that it is harmless.",
		},
		{
			fn:   "returnRenderSpecs",
			want: windowsReturnRenderSpecs,
			why: "the mix matrix pins audioconvert at two channels and wasapi2sink opens a stereo " +
				"shared-mode stream, so the Windows render tail has nothing to reconcile and is " +
				"the resampler alone. The macOS tail has more in it because a CoreAudio device " +
				"need not be stereo, which is a fact about that platform and not a better idea.",
		},
	} {
		got := funcBody(t, fset, file, "", tc.fn)
		if got != tc.want {
			t.Errorf("%s in %s is not the text that went on air.\n\n--- it now reads ---\n%s\n"+
				"\n--- it must read ---\n%s\n\n%s\n\nIf the change is deliberate, update the "+
				"literal in this file in the same commit and record what was measured on the "+
				"operator's machine.",
				tc.fn, returnCgoWindowsSourceFile, got, tc.want, tc.why)
		}
	}

	// The sink FACTORY, which nothing else asserts. return_cgo.go builds the sink
	// from this const, so a one-word edit here — to the older "wasapisink", say,
	// during a bundle repair — silently changes which plugin plays the return, and
	// that plugin has no low-latency property for the guard below to find.
	if got := stringConstValue(t, file, "returnSinkFactory"); got != "wasapi2sink" {
		t.Errorf("%s: returnSinkFactory = %q, want \"wasapi2sink\". wasapi2 is what the "+
			"contribution path already captures with, so the bundle carries it; its device "+
			"property takes the IMMDevice endpoint id string that ListOutputDevices reports, "+
			"and no other sink on this platform does.", returnCgoWindowsSourceFile, got)
	}

	sink := funcBody(t, fset, file, "", "configureReturnSinkLocked")
	if !strings.Contains(sink, `setStringProperty(sink, "device", deviceID)`) {
		t.Error("the Windows sink no longer takes its endpoint id as a string through the " +
			"type-checked setter. wasapi2sink's device property is a gchar*, and the check is " +
			"what would make a future plugin that changed it fail loudly rather than leave the " +
			"monitor on the default endpoint with a green lamp.")
	}
	// The literal true, not just the property name. A guard that only looked for
	// "low-latency" would be satisfied by a hasProperty call with the set deleted,
	// and by SetObjectProperty("low-latency", false) — which is the wasapi2 default
	// and adds a shared-mode buffering period to a commentator's monitor with
	// nothing anywhere saying so.
	if !strings.Contains(sink, `sink.SetObjectProperty("low-latency", true)`) {
		t.Error("the Windows sink no longer asks wasapi2 for the low-latency shared-mode period " +
			"with an explicit true")
	}
	if !strings.Contains(sink, `hasProperty(sink, "low-latency")`) {
		t.Error("the low-latency set is no longer guarded by hasProperty. The property is " +
			"wasapi2's own and the older wasapi plugin has not got it, so an unguarded set " +
			"emits a GLib CRITICAL on every reconnect after a bundle repair substitutes one " +
			"for the other.")
	}
}

// stringConstValue returns the unquoted value of a package-level string constant
// declared in file, or fails the test if there is no such declaration.
//
// It reads the AST rather than the file text so that a const named in a comment
// — and this package comments heavily — cannot satisfy a caller looking for the
// declaration. It is the same shape TestPlatformElementContractIsPinned uses on
// the contribution path's element consts, kept local because the two want
// different failure messages.
func stringConstValue(t *testing.T, file *ast.File, name string) string {
	t.Helper()

	var value string
	var found bool
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, ident := range spec.Names {
			if ident.Name != name || i >= len(spec.Values) {
				continue
			}
			lit, ok := spec.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			unquoted, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}
			value, found = unquoted, true
		}
		return true
	})
	if !found {
		t.Fatalf("no package-level string constant %s is declared", name)
	}
	return value
}

// TestBuildLockedVetsTheHeadphoneEndpointBeforeTheSinkSeesIt is where the
// cross-platform config.json is actually made safe.
//
// vetOutputDeviceID lives in return.go — no build tag, no cgo — precisely so that
// both platforms answer "the saved device is not on this machine" identically.
// Before it, they did not: wasapi2sink ignored an unknown id and opened the
// default endpoint silently, and osxaudiosink given a stale integer failed the
// NULL to PAUSED transition outright, so the same config.json produced a
// wrong-ears monitor on one platform and no monitor at all on the other.
//
// Calling configureReturnSinkLocked with opts.OutputDeviceID directly would put
// that behaviour straight back, one platform at a time and silently.
func TestBuildLockedVetsTheHeadphoneEndpointBeforeTheSinkSeesIt(t *testing.T) {
	fset, file := parseSource(t, returnCgoSourceFile)
	body := funcBody(t, fset, file, "returnPipeline", "buildLocked")

	if !strings.Contains(body, "vetOutputDeviceID(opts.OutputDeviceID)") {
		t.Fatal("buildLocked no longer vets the configured headphone endpoint against the devices " +
			"this machine is offering. A config.json carried between Windows and macOS then fails " +
			"silently on one platform and unstartably on the other.")
	}
	if !strings.Contains(body, "configureReturnSinkLocked(") {
		t.Fatal("buildLocked no longer goes through the platform sink configuration")
	}
	if strings.Contains(body, `"wasapi2sink"`) || strings.Contains(body, `"osxaudiosink"`) {
		t.Error("buildLocked names a platform's sink directly. The whole point of the seam is that " +
			"the common half names none, so that neither platform's compiler is silently agreeing " +
			"with a decision made for the other.")
	}
}

// TestTheUnsupportedPlatformTwinRefusesRatherThanImprovises guards the third
// file against becoming a plausible-looking Linux implementation.
//
// A chain that compiles invites the belief that it works. return_cgo_other.go
// exists so that GOOS=linux still builds and vets, and its whole contract is to
// say one clear sentence at the point of use.
func TestTheUnsupportedPlatformTwinRefusesRatherThanImprovises(t *testing.T) {
	fset, file := parseSource(t, returnCgoOtherSourceFile)

	for _, fn := range []string{"returnDecodeSpecs", "returnRenderSpecs"} {
		if body := funcBody(t, fset, file, "", fn); !strings.Contains(body, "return nil") {
			t.Errorf("%s in %s has grown an element list. There is no third audio stack; a chain "+
				"nobody has run is worse than an honest refusal.", fn, returnCgoOtherSourceFile)
		}
	}
	if body := funcBody(t, fset, file, "", "configureReturnSinkLocked"); !strings.Contains(body, "errors.New(") {
		t.Errorf("configureReturnSinkLocked in %s no longer refuses", returnCgoOtherSourceFile)
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
