// preview_test.go is the Gate A cover for the confidence monitor.
//
// It runs with no GStreamer, no GPU and no window, which is the entire reason
// preview.go carries no build tag: the three things about this feature that fail
// SILENTLY OR CATASTROPHICALLY are all decidable without any of them.
//
//   - the branch STRING, because a wrong element or an unknown property in it is
//     a gst_parse_launch failure of the whole contribution pipeline, not of the
//     preview;
//   - the ORDER of videoscale and videoconvert, because getting it the other way
//     round costs about four times the branch's whole cost and nothing anywhere
//     would say so;
//   - the NAMES, because the bus filter classifies on them and an unclassified
//     preview element rejoins the fatal default, which means a sink that dies
//     mid-match takes the commentary with it.
//
// What it cannot reach is the part that needs a window: whether glimagesink or
// d3d11videosink actually comes up in a real Wails surface. That is unproven
// here and is reported as unproven rather than asserted.
package gst

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// previewTestSink is a stand-in factory name. The real one is resolved against
// the loaded registry, which is exactly what this file has none of.
const previewTestSink = "glimagesink"

// previewElementNames is every element the branch builds.
//
// It lives here rather than in preview.go because nothing in the shipped code
// needs it: the classification is a PREFIX test, deliberately, so that an
// element added later is covered without anybody remembering a list. The list
// exists to check the prefix rule from the other direction — that the branch
// really does name these six and no others — and a check is a test's job.
func previewElementNames() []string {
	return []string{
		namePreviewQueue,
		namePreviewRate,
		namePreviewScale,
		namePreviewConvert,
		// The second leaky queue, immediately above the sink. It is the one
		// that actually defends the broadcast leg — the head queue is drained
		// by videorate and never fills, so its leak cannot fire. Added after a
		// measurement showed a slow renderer taking the on-air branch from
		// 49.1 fps to 34.3; with this queue it holds 48.4.
		namePreviewSinkQueue,
		namePreviewSink,
	}
}

// TestPreviewBranchIsTheMeasuredShape pins the element order, which is the
// finding this whole branch is built around.
//
// Scaling BEFORE converting measured 0.55-0.73 s of user CPU over a 10.1 s run
// of 1080p50 against 2.12-2.54 s for the other order — about four times — and
// the rate cap has to be above BOTH of them or everything below it runs at 50
// fps rather than 12.5. Nothing at run time would ever report the difference:
// both orders produce identical pictures and neither logs anything.
func TestPreviewBranchIsTheMeasuredShape(t *testing.T) {
	branch := previewBranch(previewTestSink)
	if branch == "" {
		t.Fatal("previewBranch rendered nothing for a sink factory that exists")
	}

	// The elements, in the order they must appear. Anything out of order here is
	// a real regression even though the picture would look identical.
	want := []string{
		namePreviewQueue,
		namePreviewRate,
		namePreviewScale,
		namePreviewConvert,
		namePreviewSink,
	}
	at := -1
	for _, name := range want {
		i := strings.Index(branch, "name="+name)
		if i < 0 {
			t.Fatalf("the preview branch has no element named %q:\n%s", name, branch)
		}
		if i < at {
			t.Errorf("the preview branch has %q out of order. The order is the measurement: "+
				"rate cap, then scale, then convert. Scaling after converting costs about four "+
				"times as much for an identical picture:\n%s", name, branch)
		}
		at = i
	}

	// The rate cap and the raster, as capsfilters rather than as element
	// properties, because that is the only way videorate and videoscale can be
	// told what to produce.
	if !strings.Contains(branch, "framerate="+
		strconv.Itoa(previewFrameRateNum)+"/"+strconv.Itoa(previewFrameRateDen)) {
		t.Errorf("the preview branch does not cap the frame rate at %d/%d. Without the cap every "+
			"element below it runs at the feed's rate:\n%s",
			previewFrameRateNum, previewFrameRateDen, branch)
	}
	if !strings.Contains(branch, "width="+strconv.Itoa(previewWidth)+
		",height="+strconv.Itoa(previewHeight)) {
		t.Errorf("the preview branch does not scale to %dx%d:\n%s",
			previewWidth, previewHeight, branch)
	}

	// It must hang off the tee and nothing else, and it must start on a line of
	// its own — gst_parse_launch reads a newline as the end of a chain, and a
	// branch concatenated onto the previous chain without one would be parsed as
	// a continuation of it.
	if !strings.HasPrefix(branch, "\n"+namePreviewTee+".") {
		t.Errorf("the preview branch does not begin on a new line with %q. Appended to the end of "+
			"pipelineDescription's string it would be read as a continuation of the audio chain:\n%s",
			namePreviewTee+".", branch)
	}
}

// TestPreviewHeadQueueLeaksDownstream is the one property that protects the
// broadcast leg from the preview, and it is asserted on its own because it is
// one word in the middle of a string and its absence is invisible.
//
// MEASURED with gst-launch-1.0 1.26.10 on this machine, 500 frames of 1080p50
// through a tee, with a deliberately slow consumer on the preview branch:
//
//	leaky=downstream   10.18 s, 10.20 s   49.1 / 49.0 fps on the broadcast branch
//	leaky=no           18.41 s, 19.40 s   27.2 / 25.8 fps  — HALF RATE, ON AIR
//
// A tee pushes to its src pads serially on the upstream streaming thread, so a
// preview that merely renders slowly is a preview that renders slowly IN THE
// MIDDLE OF THE BROADCAST LEG'S OWN PUSH.
func TestPreviewHeadQueueLeaksDownstream(t *testing.T) {
	branch := previewBranch(previewTestSink)

	head := "queue name=" + namePreviewQueue
	i := strings.Index(branch, head)
	if i < 0 {
		t.Fatalf("the preview branch has no head queue:\n%s", branch)
	}
	// Everything up to the next element boundary belongs to the queue.
	tail := branch[i:]
	if j := strings.Index(tail, " ! "); j >= 0 {
		tail = tail[:j]
	}

	if !strings.Contains(tail, "leaky=downstream") {
		t.Errorf("the preview's head queue is not leaky=downstream. A preview that renders slowly "+
			"then drags the broadcast leg down with it — measured at 50 fps against 26-27 fps. "+
			"The queue reads: %s", tail)
	}

	// AND THE ONE ABOVE THE SINK, which is the one that actually defends the
	// broadcast leg. The head queue alone does NOT: videorate below it discards
	// three of every four buffers, so that queue drains faster than it fills
	// and its leak never fires, while the blocking a slow renderer causes
	// happens underneath it. Measured on the real card, 500 frames on air with
	// a preview sink overrunning its 80 ms budget at 100 ms/frame:
	//
	//	no preview            10.18 s   49.1 fps on air
	//	head queue ONLY       14.57 s   34.3 fps ON AIR
	//	both queues           10.34 s   48.4 fps on air
	//
	// This assertion exists because the branch reads entirely plausibly with
	// one leaky queue in it, and the defect is invisible until a renderer is
	// slow — which, on a gallery machine during a match, is exactly when it
	// will happen and exactly when nobody can investigate it.
	sinkQ := "queue name=" + namePreviewSinkQueue
	k := strings.Index(branch, sinkQ)
	if k < 0 {
		t.Fatalf("the preview branch has no leaky queue above its sink, so a slow renderer takes "+
			"the broadcast leg from 49 fps to 34. The branch reads:\n%s", branch)
	}
	sinkTail := branch[k:]
	if j := strings.Index(sinkTail, " ! "); j >= 0 {
		sinkTail = sinkTail[:j]
	}
	if !strings.Contains(sinkTail, "leaky=downstream") {
		t.Errorf("the preview's sink queue is not leaky=downstream, which makes it useless: a "+
			"non-leaky queue there back-pressures through videorate to the tee exactly as no queue "+
			"at all does. It reads: %s", sinkTail)
	}
	// It must sit BELOW videorate. Above it, it is the head queue again and is
	// drained by the rate cap before it can ever leak.
	if r := strings.Index(branch, "videorate"); r < 0 || k < r {
		t.Error("the preview's sink queue is above videorate, where the rate cap drains it and its " +
			"leak can never fire. It must sit between the converter and the sink.")
	}
	if !strings.Contains(tail, "max-size-buffers="+strconv.Itoa(previewQueueBuffers)) {
		t.Errorf("the preview's head queue does not bound itself at %d buffers, so it would hold a "+
			"backlog rather than leaking one: %s", previewQueueBuffers, tail)
	}
	// The other two limits pinned to unlimited, so the buffer count is the only
	// criterion that can fire. A queue leaks when ANY of its three limits is
	// reached, and leaving the defaults in place would make WHICH one fired a
	// function of the raster.
	for _, unlimited := range []string{"max-size-bytes=0", "max-size-time=0"} {
		if !strings.Contains(tail, unlimited) {
			t.Errorf("the preview's head queue does not pin %s, so the limit that fires depends on "+
				"the conform target rather than on the buffer count: %s", unlimited, tail)
		}
	}
}

// TestPreviewBranchUsesOnlyPropertiesThatCannotFailToParse is the guard that
// keeps an optional feature from being able to stop the commentary going on air.
//
// gst_parse_launch treats an unknown property as a HARD ERROR — the bframes note
// in elements_windows.go is the same trap on the encoder — and the string this
// branch is appended to is THE CONTRIBUTION PIPELINE'S. So the branch may use
// only properties that every build of GStreamer has on the element in question:
// GstQueue's own, and GstBaseSink's own. Anything else — videorate's drop-only,
// videoscale's method, a sink property that exists on one platform's sink and
// not the other's — must be set through hasProperty in preview_cgo.go, where a
// missing property is a log line instead of a dead feed.
func TestPreviewBranchUsesOnlyPropertiesThatCannotFailToParse(t *testing.T) {
	// Every property assignment in the branch, ignoring the ones inside caps
	// (which are caps FIELDS and are negotiated, not set on an object).
	allowed := map[string]string{
		"leaky":            "GstQueue",
		"max-size-buffers": "GstQueue",
		"max-size-bytes":   "GstQueue",
		"max-size-time":    "GstQueue",
		"sync":             "GstBaseSink",
		"name":             "GstObject",
	}

	branch := previewBranch(previewTestSink)
	// Drop the capsfilters: everything between "video/x-raw" and the next " ! "
	// is caps, where width= and framerate= are fields rather than properties.
	caps := regexp.MustCompile(`video/x-raw[^ ]*`)
	assignments := regexp.MustCompile(`([a-z0-9-]+)=`)

	for _, m := range assignments.FindAllStringSubmatch(caps.ReplaceAllString(branch, ""), -1) {
		if _, ok := allowed[m[1]]; !ok {
			t.Errorf("the preview branch sets %q in the parse string. Only properties that every "+
				"GStreamer has on that element may go there, because an unknown property is not a "+
				"preview that fails — it is gst_parse_launch refusing the WHOLE contribution "+
				"pipeline and the commentary not going on air. Set it in attachPreview behind "+
				"hasProperty instead", m[1])
		}
	}
}

// TestPreviewIsBuiltOnlyWhenItCanBe covers the four refusals, and the second of
// them is the one that matters most: a preview branch rendered when the video
// leg is the slate names a tee that is not in the string, which is a parse
// failure of the whole pipeline rather than a preview that does not appear.
func TestPreviewIsBuiltOnlyWhenItCanBe(t *testing.T) {
	const card = "2747401380"

	cases := []struct {
		name         string
		opts         PreviewOpts
		videoCapture string
	}{
		{
			// The default on every seat, and the one that must cost nothing.
			name: "off", opts: PreviewOpts{WindowHandle: 1}, videoCapture: card,
		},
		{
			name:         "on, but the video leg is the slate",
			opts:         PreviewOpts{Enabled: true, WindowHandle: 1},
			videoCapture: "",
		},
		{
			name:         "on, but there is no surface yet",
			opts:         PreviewOpts{Enabled: true},
			videoCapture: card,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := previewBranchFor(tc.opts, tc.videoCapture); got != "" {
				t.Errorf("previewBranchFor built a branch when it must not have:\n%s", got)
			}
		})
	}

	// And the one that would build, if this build had a registry to resolve a
	// sink against. It does not — see preview_stub.go, which refuses on purpose
	// — so what is asserted here is only that the refusal is total: nothing half
	// rendered, no element named after a sink that was never found.
	if got := previewBranchFor(PreviewOpts{Enabled: true, WindowHandle: 1}, card); got != "" {
		t.Errorf("previewBranchFor built a branch in a build with no GStreamer to resolve a sink "+
			"against:\n%s", got)
	}
	if got := previewBranch(""); got != "" {
		t.Errorf("previewBranch rendered %q for an empty sink factory; no sink must mean no branch, "+
			"not a branch with a nameless sink in it", got)
	}
}

// TestPreviewElementsAreSparedByTheBusFilter is the test that stands between a
// dead preview and a dead feed.
//
// Every element of the branch must be recognised by isPreviewSourced, because
// that is what capturefault.go's classifier keys on, and an element it does not
// recognise falls through to the FATAL default: markFatal, ErrPipelineFatal,
// internal/sender returning out of its loop, and the commentary off air until a
// human presses STOP and then START — over a monitor nobody was looking at.
func TestPreviewElementsAreSparedByTheBusFilter(t *testing.T) {
	for _, name := range previewElementNames() {
		if !strings.HasPrefix(name, previewNamePrefix) {
			t.Errorf("preview element %q does not begin with %q, so the bus filter cannot recognise "+
				"it and its failures would take the commentary off air", name, previewNamePrefix)
		}
		if !isPreviewSourced(name) {
			t.Errorf("isPreviewSourced(%q) is false", name)
		}
	}

	// And the branch really does name those elements and no others: a sixth
	// element added to the string without a constant would be unclassified.
	branch := previewBranch(previewTestSink)
	known := make(map[string]bool, len(previewElementNames()))
	for _, n := range previewElementNames() {
		known[n] = true
	}
	for _, m := range regexp.MustCompile(`name=([A-Za-z0-9_-]+)`).FindAllStringSubmatch(branch, -1) {
		if !known[m[1]] {
			t.Errorf("the preview branch names an element %q that previewElementNames does not know "+
				"about, so nothing checks that the bus filter spares it", m[1])
		}
	}
}

// TestPreviewNamesCannotBeConfusedWithTheFeed pins the separation the whole
// classification rests on, in BOTH directions.
//
// A preview name that began with the video-capture prefix would be classified as
// a video capture fault — which is spared in the ordinary case and UPGRADED TO
// FATAL when the commentary audio is clocked by the card's video, which is the
// DeckLink configuration this tier exists for. It would upgrade nearly every
// time. And a feed name that began with the preview prefix would be spared when
// it must not be.
func TestPreviewNamesCannotBeConfusedWithTheFeed(t *testing.T) {
	feed := []string{
		captureSinkQueueName,
		captureSinkNamePrefix,
		captureAudioSrcName,
		videoCaptureNamePrefix,
		nameVideoCaptureSrc,
		nameVideoCaptureClock,
		namePreviewTee,
	}
	for _, name := range feed {
		if isPreviewSourced(name) {
			t.Errorf("%q is treated as a preview element. Its failures would be SPARED — the "+
				"pipeline left PLAYING and the error logged as a warning — when they are the feed's "+
				"own", name)
		}
	}
	for _, name := range previewElementNames() {
		if strings.HasPrefix(name, videoCaptureNamePrefix) {
			t.Errorf("preview element %q carries the video capture prefix %q. It would be classified "+
				"as a capture fault, which becomes FATAL whenever the commentary audio is clocked by "+
				"the card's video — which is the configuration this feature is for",
				name, videoCaptureNamePrefix)
		}
		if strings.HasPrefix(name, captureSinkNamePrefix) || name == captureSinkQueueName ||
			name == captureAudioSrcName {
			t.Errorf("preview element %q collides with a feed element name", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Source guards
// ---------------------------------------------------------------------------

// cgoSourceFileForPreview is the file the two guards below read. It is
// gst_cgo.go, which Gate A cannot compile — so what is checked is its TEXT.
const cgoSourceFileForPreview = "gst_cgo.go"

// TestPreviewTeeMatchesThePipeline pins the one string this file duplicates.
//
// namePreviewTee mirrors gst_cgo.go's nameVideoCapTee, and it is a copy for a
// build-tag reason rather than a preference: gst_cgo.go is `cgo && !gststub`, so
// its constants do not exist here, where the branch string is tested. If the two
// ever disagree, the preview branch names a tee that is not in the parse string,
// and that is not a preview that fails to appear — it is gst_parse_launch
// refusing the whole contribution pipeline and the commentary not going on air.
func TestPreviewTeeMatchesThePipeline(t *testing.T) {
	src, err := os.ReadFile(cgoSourceFileForPreview)
	if err != nil {
		t.Fatalf("reading %s: %v", cgoSourceFileForPreview, err)
	}

	m := regexp.MustCompile(`nameVideoCapTee\s*=\s*"([^"]*)"`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("%s no longer declares nameVideoCapTee as a string constant. preview.go mirrors it "+
			"as %q, and a preview branch that names a tee the pipeline does not build is a "+
			"gst_parse_launch failure of the whole contribution pipeline",
			cgoSourceFileForPreview, namePreviewTee)
	}
	if got := string(m[1]); got != namePreviewTee {
		t.Fatalf("%s has nameVideoCapTee = %q and preview.go has namePreviewTee = %q. The preview "+
			"branch would name a tee that is not in the parse string, and gst_parse_launch would "+
			"refuse the WHOLE pipeline", cgoSourceFileForPreview, got, namePreviewTee)
	}
}

// TestAPreviewInThePipelineIsSparedByTheBusFilter is a CONDITIONAL guard, and it
// is conditional on purpose.
//
// The preview branch is rendered by this package and appended to
// pipelineDescription's string by gst_cgo.go, and the bus filter that has to
// spare it is capturefault.go's. Those are three files with three owners, and
// the failure of the middle one to be wired to the third is silent: the preview
// works, looks right, and takes the commentary off air the first time its sink
// errors — which is the first time a GPU driver updates mid-match.
//
// So this test asks: IF the contribution pipeline builds a preview branch, THEN
// the classifier must know about preview elements. Before the wiring lands both
// halves are absent and this passes; the moment somebody wires the branch in
// without the classifier, it fails BY NAME and says what to add.
func TestAPreviewInThePipelineIsSparedByTheBusFilter(t *testing.T) {
	pipeline, err := os.ReadFile(cgoSourceFileForPreview)
	if err != nil {
		t.Fatalf("reading %s: %v", cgoSourceFileForPreview, err)
	}
	if !strings.Contains(string(pipeline), "previewBranchFor") {
		t.Skipf("%s does not build a preview branch yet, so there is nothing for the bus filter to "+
			"spare. This test becomes live the moment it does", cgoSourceFileForPreview)
	}

	filter, err := os.ReadFile("capturefault.go")
	if err != nil {
		t.Fatalf("reading capturefault.go: %v", err)
	}
	if !strings.Contains(string(filter), "isPreviewSourced") {
		t.Fatal("gst_cgo.go builds a preview branch and capturefault.go's classifyBusError does not " +
			"call isPreviewSourced. Every preview element therefore falls through to the FATAL " +
			"default: a preview sink that errors — a GPU driver update, a display going away — " +
			"marks the pipeline pipeline-fatal, internal/sender stops retrying, and the COMMENTARY " +
			"goes off air over a monitor nobody was looking at. Add a classPreview branch to " +
			"classifyBusError, ahead of the video-capture prefix, and a case in onBusMessage that " +
			"spares it exactly as classVideoCapture is spared but WITHOUT the audio-clocked-by-video " +
			"upgrade")
	}
}
