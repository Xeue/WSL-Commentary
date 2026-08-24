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
	"bytes"
	"go/ast"
	"go/printer"
	"go/token"
	"strings"
	"testing"
)

// pictureCgoSourceFile is the picture path's cgo half.
const pictureCgoSourceFile = "picture_cgo.go"

// pictureCgoCode renders the whole file's CODE, with every comment stripped.
//
// parseSource parses with mode 0, which does not attach comments to the tree, so
// printing the file back gives the code alone. That is the property the guards
// need: picture_cgo.go's header discusses decodebin at length, precisely to say
// it must never be used, and now names avdec_h265/avdec_h264 as the software
// fallbacks the candidate lists deliberately DO carry — so a search over the raw
// bytes would find those sentences and could neither prove the decodebin ban nor
// tell an allowed name in the code from one merely discussed in a comment.
func pictureCgoCode(t *testing.T, fset *token.FileSet, file *ast.File) string {
	t.Helper()
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, file); err != nil {
		t.Fatalf("rendering %s: %v", pictureCgoSourceFile, err)
	}
	return buf.String()
}

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

// TestPictureChoosesDecodersByExplicitNameNotByRank is the guard that keeps the
// software fallback in its place: last, and never reached by rank.
//
// gst-libav was admitted on 2026-08-24 as a software decoder (avdec_h265,
// avdec_h264) for Windows machines whose GPU exposes no hardware HEVC profile —
// so avdec_ and libav are now ALLOWED in the code, and this test no longer
// forbids them. What it still forbids is any element that would let GStreamer
// CHOOSE a decoder BY RANK. avdec_h265 ranks PRIMARY on both development
// machines, above d3d11h265dec and vtdec_hw, so a decodebin — or a playbin, or a
// "let GStreamer choose" refactor — would land on the software decoder OVER the
// hardware one and burn a core on a machine with an idle media engine. The
// candidate lists must stay explicit, hardware-first, and named.
func TestPictureChoosesDecodersByExplicitNameNotByRank(t *testing.T) {
	fset, file := parseSource(t, pictureCgoSourceFile)
	src := pictureCgoCode(t, fset, file)

	for _, forbidden := range []string{"decodebin", "playbin", "uridecodebin"} {
		if strings.Contains(src, forbidden) {
			t.Errorf("picture_cgo.go's code (not its comments) names %q. The decoder on this path is "+
				"chosen by explicit, hardware-first name: avdec_h265 ranks PRIMARY, so anything that "+
				"lets GStreamer choose by rank picks the software decoder over the GPU's", forbidden)
		}
	}
}

// TestPictureChoosesTheHardwareDecoderFirstOnEachPlatform pins the two elements
// that differ per platform.
//
// It is a source guard rather than a call to pictureDecoderCandidates() because
// this test only builds at Gate A, where picture_cgo.go is not compiled at all —
// the same limitation, and the same instrument, as the tests above.
//
// The ORDER inside each list is what is being protected. vtdec_hw before vtdec
// is hardware before software; glimagesink before osxvideosink is the sink that
// was driven in a real Wails window before the marginal-rank one that was not.
// A list reordered by somebody tidying alphabetically would still work, on a
// slower decoder and an unproven sink, with nothing anywhere saying so.
func TestPictureChoosesTheHardwareDecoderFirstOnEachPlatform(t *testing.T) {
	fset, file := parseSource(t, pictureCgoSourceFile)

	decoders := funcBody(t, fset, file, "", "pictureDecoderCandidates")
	if !strings.Contains(decoders, `"d3d11h265dec"`) {
		t.Error("pictureDecoderCandidates no longer offers d3d11h265dec, which is DXVA and the " +
			"hardware HEVC decoder on the Windows target: mfh265dec needs an HEVC extension the " +
			"operator would have to buy from the Microsoft Store, and avdec_h265 below it is the " +
			"software last resort, not a substitute for the GPU path")
	}
	hw := strings.Index(decoders, `"vtdec_hw"`)
	sw := strings.Index(decoders, `"vtdec"`)
	if hw < 0 {
		t.Error("pictureDecoderCandidates no longer offers vtdec_hw, the VideoToolbox hardware " +
			"decoder at rank primary+1, which is the measured macOS answer")
	}
	if hw >= 0 && sw >= 0 && sw < hw {
		t.Error("pictureDecoderCandidates offers vtdec before vtdec_hw. That is a software HEVC " +
			"decode of 1080p50 on a machine that has a media engine sitting idle")
	}
	// The Windows software fallback comes AFTER the hardware decoder, never
	// before. avdec_h265 ranks PRIMARY, so listing it first — or a tidy that
	// alphabetised the list — would software-decode on every machine that had
	// d3d11h265dec sitting right there. See the file header.
	h265hw := strings.Index(decoders, `"d3d11h265dec"`)
	h265sw := strings.Index(decoders, `"avdec_h265"`)
	if h265sw >= 0 && (h265hw < 0 || h265sw < h265hw) {
		t.Error("pictureDecoderCandidates lists avdec_h265 at or before d3d11h265dec. The software " +
			"decoder must be the LAST resort on Windows: it ranks primary and would otherwise be " +
			"chosen over the GPU's DXVA decoder")
	}

	// The H.264 decoder is the Windows half of "parse whichever codec the
	// transport carries": on Windows H.264 needs d3d11h264dec, a DIFFERENT element
	// from the H.265 d3d11h265dec, because that one decodes H.265 only. A build
	// that forgot it shows an H.265 return and a black screen for an H.264 one.
	decodersH264 := funcBody(t, fset, file, "", "pictureDecoderCandidatesH264")
	if !strings.Contains(decodersH264, `"d3d11h264dec"`) {
		t.Error("pictureDecoderCandidatesH264 no longer offers d3d11h264dec. On Windows d3d11h265dec " +
			"decodes H.265 ONLY, so an H.264 return has no hardware decoder and the video branch cannot link")
	}
	// The H.264 software fallback, like the H.265 one, comes last — never before
	// the GPU's own d3d11h264dec.
	h264hw := strings.Index(decodersH264, `"d3d11h264dec"`)
	h264sw := strings.Index(decodersH264, `"avdec_h264"`)
	if h264sw >= 0 && (h264hw < 0 || h264sw < h264hw) {
		t.Error("pictureDecoderCandidatesH264 lists avdec_h264 at or before d3d11h264dec. The software " +
			"decoder must be the last resort: it ranks primary and would otherwise be chosen over the GPU's")
	}

	sinks := funcBody(t, fset, file, "", "pictureSinkCandidates")
	if !strings.Contains(sinks, `"d3d11videosink"`) {
		t.Error("pictureSinkCandidates no longer offers d3d11videosink")
	}
	gl := strings.Index(sinks, `"glimagesink"`)
	osx := strings.Index(sinks, `"osxvideosink"`)
	if gl < 0 {
		t.Error("pictureSinkCandidates no longer offers glimagesink. It is the ONE macOS sink " +
			"proven to take an NSView* through gst_video_overlay_set_window_handle in a real " +
			"Wails window; caopengllayersink failed to link in the same harness")
	}
	if gl >= 0 && osx >= 0 && osx < gl {
		t.Error("pictureSinkCandidates offers osxvideosink before glimagesink. osxvideosink is at " +
			"MARGINAL rank, is the older NSOpenGL path, and has never been driven by this " +
			"application")
	}
	if strings.Contains(sinks, "caopengllayersink") {
		t.Error("pictureSinkCandidates offers caopengllayersink. It FAILED TO LINK in the " +
			"proof-of-concept harness on the target machine")
	}
}

// TestPictureBuildsTheChainItResolved guards the join between the candidate
// lists and the pipeline.
//
// A list that is correct and a buildLocked that still hard-codes d3d11 would be
// two things that each look right and a macOS build that cannot create an
// element. The names must reach the factory call.
func TestPictureBuildsTheChainItResolved(t *testing.T) {
	fset, file := parseSource(t, pictureCgoSourceFile)
	body := funcBody(t, fset, file, "picturePipeline", "buildLocked")

	for _, want := range []string{
		"p.chain.decoder.factory",
		"p.chain.decoderH264.factory",
		"p.chain.sink.factory",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("buildLocked does not create its element from %s. Either it has gone back to "+
				"a hard-coded factory name — which cannot be right on both platforms — or the "+
				"resolved chain is being computed and thrown away", want)
		}
	}
}

// TestPictureDecouplesTheSoftwareDecoderFromTheSink pins the mitigation that
// makes a software decode survivable on a machine that cannot present as fast as
// it decodes — in the field, a Dell whose HEVC hardware decoder the OEM fused
// off, driven over a remote desktop that keeps the iGPU busy.
//
// Without the leaky present queue, a slow sink back-pressures the decoder, which
// stalls the demuxer, which stops srtsrc draining, which overflows libsrt and
// drops COMPRESSED packets — and a lost NAL shreds the picture. The queue moves
// the loss to whole decoded frames instead. It is software-only, so this also
// pins that the choice is made through pictureDecoderIsSoftwareFactory rather
// than applied to the measured hardware path.
func TestPictureDecouplesTheSoftwareDecoderFromTheSink(t *testing.T) {
	fset, file := parseSource(t, pictureCgoSourceFile)
	body := funcBody(t, fset, file, "picturePipeline", "buildLocked")

	if !strings.Contains(body, "pictureDecoderIsSoftwareFactory(decoderFactory)") {
		t.Error("buildLocked no longer decides the software path from pictureDecoderIsSoftwareFactory; " +
			"the present queue and output-corrupt must apply to software decode ONLY, never the " +
			"measured hardware path")
	}
	for _, want := range []string{"namePicPresentQueue", `"leaky"`, `"downstream"`} {
		if !strings.Contains(body, want) {
			t.Errorf("buildLocked no longer builds the leaky present queue (missing %s). A slow sink "+
				"will then back-pressure the software decoder into an SRT buffer overflow, which loses "+
				"compressed data and corrupts the picture instead of dropping clean decoded frames", want)
		}
	}
	if !strings.Contains(body, `"output-corrupt"`) {
		t.Error("buildLocked no longer sets output-corrupt on the software decoder; a decoder that has " +
			"lost reference data will paint the damage rather than hold the last good frame")
	}
	if !strings.Contains(body, `"thread-type"`) {
		t.Error("buildLocked no longer sets thread-type on the software decoder. FRAME threading tears " +
			"the picture on a heterogeneous CPU (fast P + slow E cores) with \"Could not find ref\"; " +
			"SLICE threading decodes frames in order and is the field fix")
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
