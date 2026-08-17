//go:build !cgo || gststub

// seam_test.go is the Gate A guard over the seam's names, its queue policies and
// the two prefixes the fault classifier decides on.
//
// Every constant it pins lives in seam.go, which is untagged, so Gate A is where
// they can be checked without a registry, a device or cgo — and it is where the
// person editing them is standing. The two files it reads as TEXT are behind the
// cgo tag and cannot be called from here, which is the same arrangement
// gst_stub_test.go's guards over gst_cgo.go already use.

package gst

import (
	"strconv"
	"strings"
	"testing"
)

// The source files the guards below read as text. They are declared here rather
// than beside cgoSourceFile in gst_stub_test.go because that file is
// `!cgo || gststub` and these have to be reachable from the cgo test binary too.
const (
	captureDescSourceFile = "capturedesc_cgo.go"
	captureCgoSourceFile  = "capture_cgo.go"
	seamSourceFile        = "seam.go"
	proxyCgoSourceFile    = "proxy_cgo.go"
)

// TestTheSeamPrefixesCannotCollideWithTheOthers is the guard on a silent
// misclassification.
//
// classifyBusError decides fatal-or-spared BY ELEMENT NAME, through four
// prefixes. "vprev" is the confidence monitor and must never take the commentary
// off air; "vcap" is the picture capture and IS upgraded to fatal on a card
// commentary seat; "vprox" and "aprox" are the seam's own tails. Any two of them
// standing in a prefix relationship would silently reclassify a whole leg — and
// the case that would actually happen is a preview error being read as a capture
// error on precisely the fused seats the preview exists for.
func TestTheSeamPrefixesCannotCollideWithTheOthers(t *testing.T) {
	prefixes := map[string]string{
		"preview":         previewNamePrefix,
		"picture capture": videoCaptureNamePrefix,
		"video seam":      videoProxyNamePrefix,
		"audio seam":      audioProxyNamePrefix,
	}
	for aName, a := range prefixes {
		for bName, b := range prefixes {
			if aName == bName {
				continue
			}
			if strings.HasPrefix(a, b) {
				t.Errorf("the %s prefix %q begins with the %s prefix %q, so every element in the "+
					"first leg is classified as belonging to the second. classifyBusError decides "+
					"whether an error takes the commentary off air, and this is the way that "+
					"decision goes wrong without anything failing", aName, a, bName, b)
			}
		}
	}
}

// TestEverySeamElementNameMatchesItsPrefix checks the other half of the same
// rule: the names actually used have to be the ones the prefixes cover.
func TestEverySeamElementNameMatchesItsPrefix(t *testing.T) {
	for _, tc := range []struct{ name, prefix, why string }{
		{nameVideoProxyQueue, videoProxyNamePrefix, "the picture leg's head queue"},
		{nameVideoProxySink, videoProxyNamePrefix, "the picture leg's proxysink"},
		{nameVideoProxySrc, videoProxyNamePrefix, "the send side's picture proxysrc"},
		{nameAudioProxyQueue, audioProxyNamePrefix, "the commentary leg's head queue"},
		{nameAudioProxySink, audioProxyNamePrefix, "the commentary leg's proxysink"},
		{nameAudioProxySrc, audioProxyNamePrefix, "the send side's commentary proxysrc"},
	} {
		if !strings.HasPrefix(tc.name, tc.prefix) {
			t.Errorf("%s is named %q, which does not begin with %q, so a bus error from it "+
				"rejoins the nameless fatal default instead of naming the leg it belongs to",
				tc.why, tc.name, tc.prefix)
		}
	}
}

// TestTheSeamTailsRenderTheDeclaredPolicy pins the two queue policies at the
// point they are rendered, so that a depth changed in the const reaches the
// string and a depth changed in the string is refused here.
func TestTheSeamTailsRenderTheDeclaredPolicy(t *testing.T) {
	video := videoProxyTail()
	audio := audioProxyTail()

	for _, tc := range []struct{ got, want, why string }{
		{video, "queue name=" + nameVideoProxyQueue, "the picture head queue's name"},
		{video, "leaky=downstream", "the leak that keeps a wedged send pipeline off the preview"},
		{video, "max-size-buffers=" + strconv.Itoa(videoProxyQueueBuffers), "the declared depth"},
		{video, "max-size-bytes=0", "the byte bound pinned off so the count is the only limit"},
		{video, "max-size-time=0", "the time bound pinned off for the same reason"},
		{video, "proxysink name=" + nameVideoProxySink, "the picture proxysink"},

		{audio, "queue name=" + nameAudioProxyQueue, "the commentary head queue's name"},
		{audio, "leaky=downstream", "the leak, which is A3's DROP rather than DELAY"},
		{audio, "max-size-time=" + strconv.Itoa(audioProxyQueueTimeNs), "the declared depth"},
		{audio, "max-size-bytes=0", "the byte bound pinned off"},
		{audio, "max-size-buffers=0", "the buffer bound pinned off"},
		{audio, "proxysink name=" + nameAudioProxySink, "the commentary proxysink"},
	} {
		if !strings.Contains(tc.got, tc.want) {
			t.Errorf("the seam tail %q is missing %s (%q)", tc.got, tc.why, tc.want)
		}
	}
}

// TestNoCaptureCodeEverSetsTheConnectionProperty is the twin of
// gst_stub_test.go's guard, pointed at the two NEW files.
//
// Its own comment records why it has to be re-aimed rather than trusted: an
// earlier version of that test silently stopped covering the g_object_set half
// when that code moved once before. The capture layer is that move happening
// again, so the description builder AND the capture pipeline's build sequence are
// both read here, by name, and this test fails if either file stops existing.
func TestNoCaptureCodeEverSetsTheConnectionProperty(t *testing.T) {
	for _, name := range []string{captureDescSourceFile, captureCgoSourceFile} {
		src := readRepoFile(t, name)
		if strings.TrimSpace(src) == "" {
			t.Fatalf("%s is empty or missing; this guard is checking nothing", name)
		}
		for _, line := range strings.Split(src, "\n") {
			// The word appears in the prose that explains the rule, which is the
			// point of the prose. What is forbidden is a property write.
			if strings.Contains(line, `"connection"`) ||
				strings.Contains(line, "connection=") {
				t.Errorf("%s sets the DeckLink `connection` property: %s\n"+
					"It is not a per-pipeline input selection — it persistently reconfigures the "+
					"CARD and overrides Blackmagic Desktop Video Setup, and the owner has had to "+
					"undo that twice. The mic is on the card's MIC input and that is a Desktop "+
					"Video setting this application does not own", name, strings.TrimSpace(line))
			}
		}
	}
}

// TestTheClockCompanionIsPointedAtACard is the guard on a capture pipeline that
// opens a card nobody chose.
//
// The clock companion is a decklinkvideosrc that exists ONLY to clock
// decklinkaudiosrc, on the seat whose picture is the slate and whose commentary
// is the card. Left without its persistent-id it keeps the element's own -1,
// which means "use device-number", which means whichever card the driver
// enumerated first. On a single-card rig that IS the card and it works by
// accident; on a two-card rig the commentary is clocked by nothing — 0 buffers
// and 0 level messages against 160, with nothing on either bus — and a card
// nobody selected is opened and held exclusively from launch to quit.
//
// It is a source guard rather than a behavioural test because reaching it needs a
// parsed pipeline with two decklink elements in it, and the property is read on
// NULL->READY, which is a card open. The shipping path carries the same rule at
// gst_cgo.go:1731-1739 and it was dropped when this build sequence was written,
// which is the whole argument for pinning it here.
func TestTheClockCompanionIsPointedAtACard(t *testing.T) {
	src := readRepoFile(t, captureCgoSourceFile)
	for _, want := range []string{
		"nameVideoCaptureClock",
		"configureDeckLinkSource(clock, c.opts.AudioCaptureID)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("%s no longer contains %q, so the clock companion is built by the description "+
				"and never told WHICH CARD to open. It must open the SAME card as the audio "+
				"source: a companion on a different card clocks nothing and the commentary never "+
				"prerolls, and persistent-id is read on NULL->READY so there is no later chance "+
				"to set it", captureCgoSourceFile, want)
		}
	}
}

// TestTheArmingIsAProbeOnTheUPSTREAMPad reads proxy_cgo.go and fails by name if
// the probe is moved onto the queue's own sink pad.
//
// That is the "simplification" a later reader will reach for, and it DEADLOCKS: a
// sink-pad probe runs with that pad's stream lock held, and the SetState(READY)
// inside the callback would deadlock stopping the queue's own task. The rule is
// worth a test rather than a comment because the wrong version looks tidier and
// fails only under load.
func TestTheArmingIsAProbeOnTheUPSTREAMPad(t *testing.T) {
	src := readRepoFile(t, proxyCgoSourceFile)
	for _, want := range []string{
		`GetStaticPad("sink")`, // the queue's own pad, from which the peer is taken
		"GetPeer()",            // ...and the peer is what the probe goes on
		"PadProbeTypeIdle",
		"StateReady",
		"SyncStateWithParent",
		"PadProbeRemove",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("%s no longer contains %q. The arming is a GST_PAD_PROBE_TYPE_IDLE probe on "+
				"the UPSTREAM SRC pad that takes the queue and the proxysink to READY and back; "+
				"anything else either deadlocks or does not reset the sticky events, and an "+
				"unarmed seam does not fail — it connects, goes green and sends silence",
				proxyCgoSourceFile, want)
		}
	}
	if strings.Contains(src, "StateNull") {
		t.Errorf("%s takes an element to NULL inside the arming probe. set_state(NULL) on a "+
			"branch inside a blocking pad probe was measured taking the on-air leg from 50 fps to "+
			"0 PERMANENTLY, with the pipeline still reporting PLAYING. It is READY, never NULL",
			proxyCgoSourceFile)
	}
}

// TestTheZeroProbeIdTrapIsDocumentedWhereItBites keeps the correction that cost a
// run from being tidied away.
//
// gst_pad_add_probe RETURNS 0 for this probe, on every arming, AFTER the READY
// cycle has already run — documented behaviour for an IDLE probe that runs
// immediately and returns REMOVE. The codebase's existing idiom elsewhere is
// `if id == 0 { fail }`, and copying it here aborts the run after a SUCCESSFUL
// arming. The completion channel is the only correct success test.
func TestTheZeroProbeIdTrapIsDocumentedWhereItBites(t *testing.T) {
	src := readRepoFile(t, proxyCgoSourceFile)
	if !strings.Contains(src, "case <-done:") {
		t.Errorf("%s no longer waits on a completion channel. A zero id from gst_pad_add_probe is "+
			"the DOCUMENTED answer for an IDLE probe that ran immediately and returned REMOVE, so "+
			"the id cannot be the success test", proxyCgoSourceFile)
	}
}

// TestTheSeamFileStatesTheThreeInvariants is a documentation guard, and it earns
// its place: the three rules below are the ones a later reader will undo while
// making something else better, and each of them fails silently.
func TestTheSeamFileStatesTheThreeInvariants(t *testing.T) {
	src := readRepoFile(t, seamSourceFile)
	for _, want := range []string{
		"LEAKY QUEUE IN FRONT OF EVERY PROXYSINK",
		"RE-ARMED AT START",
		"DEVICE CHANGE IS REFUSED WHILE SENDING",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("%s no longer states the invariant %q. All three fail silently — a wedged "+
				"preview, a second session carrying zero bytes, and an orphaned proxysrc over a "+
				"connected socket — so the file that names them is the only warning there is",
				seamSourceFile, want)
		}
	}
}
