// Tests for the overlay stacking rule.
//
// These are worth having as a unit test for one reason: the fault they describe
// cannot be seen from a test on either real platform, and could not be seen at
// all until a second surface existed. It is a rule that was correct while there
// was one overlay in the process and false for one of the two the moment there
// were two, and the symptom — a GL surface detached and reattached on every frame
// of a resize drag — appears on the operator's screen and nowhere else.
//
// So the rule is written in Go in overlay_zorder.go, exercised here at Gate A,
// and the Objective-C that actually runs is pinned against it at the bottom of
// this file.
//
// Every helper here is named overlaySib* rather than something readable, because
// package gst's test files share one namespace and this file has to be able to
// land in a tree several other people are editing at the same time.
package gst

import (
	"os"
	"strings"
	"testing"
)

// overlaySib is one entry in a superview's subview list, bottom first. ours marks
// the surfaces this package owns — on macOS, "is a WSLCommsOverlayView".
type overlaySib struct {
	name string
	ours bool
}

// overlaySibWeb is the sibling the whole rule exists because of.
func overlaySibWeb() overlaySib { return overlaySib{name: "WailsWebView"} }

// overlaySibSurface is one of ours: an opaque native video surface.
func overlaySibSurface(name string) overlaySib { return overlaySib{name: name, ours: true} }

func overlaySibFlags(list []overlaySib) []bool {
	out := make([]bool, len(list))
	for i, s := range list {
		out[i] = s.ours
	}
	return out
}

func overlaySibIndex(list []overlaySib, name string) int {
	for i, s := range list {
		if s.name == name {
			return i
		}
	}
	return -1
}

// overlaySibMoveAbove is what -[NSView addSubview:positioned:NSWindowAbove
// relativeTo:] does to the list: the view comes out and goes back in immediately
// above the target. It is here so the tests can drive the rule round a loop and
// see where it settles, which is the property that actually matters — a rule that
// is true at some moment but never stops moving things is the bug, not the fix.
func overlaySibMoveAbove(list []overlaySib, self, target int) []overlaySib {
	moved, targetName := list[self], list[target].name

	rest := make([]overlaySib, 0, len(list))
	rest = append(rest, list[:self]...)
	rest = append(rest, list[self+1:]...)

	at := len(rest)
	for i, s := range rest {
		if s.name == targetName {
			at = i + 1
			break
		}
	}
	out := make([]overlaySib, 0, len(list))
	out = append(out, rest[:at]...)
	out = append(out, moved)
	out = append(out, rest[at:]...)
	return out
}

func TestOverlayRaiseAboveLeavesASettledWindowAlone(t *testing.T) {
	// The single-surface case, which is what ships today: the picture is on top
	// of the webview and there is nothing else. Every layout pass must come back
	// "do nothing", because on macOS doing something means addSubview: on a view
	// hosting a live GL surface.
	list := []overlaySib{overlaySibWeb(), overlaySibSurface("picture")}
	if got := overlayRaiseAbove(overlaySibFlags(list), 1); got != -1 {
		t.Fatalf("overlayRaiseAbove(picture on top of the webview) = %d, want -1. A settled "+
			"window must produce no work at all; this answer detaches the GL surface on every "+
			"rectangle change", got)
	}
}

func TestOverlayRaiseAboveCatchesASurfaceThatHasGoneBehindThePage(t *testing.T) {
	// The case the rule is FOR. A picture that has quietly gone behind the page
	// is indistinguishable from a picture that has stopped, which is the sentence
	// overlay_windows.go uses about the same hazard.
	list := []overlaySib{overlaySibSurface("picture"), overlaySibWeb()}
	if got := overlayRaiseAbove(overlaySibFlags(list), 0); got != 1 {
		t.Fatalf("overlayRaiseAbove(picture below the webview) = %d, want 1 (the webview). "+
			"The picture stays behind the page and looks like a picture that has stopped", got)
	}
}

func TestOverlayRaiseAboveIgnoresOurOwnSurfaces(t *testing.T) {
	// THIS IS THE ONE. Two surfaces on one contentView — the SRT return picture
	// and Tier 3's DeckLink preview — and only one of them can be the last
	// subview. Under a rule of "am I last" the other is displaced on every apply;
	// under this rule neither is, because neither is below anything it has to be
	// above.
	list := []overlaySib{overlaySibWeb(), overlaySibSurface("picture"), overlaySibSurface("decklink")}
	ours := overlaySibFlags(list)

	for _, i := range []int{1, 2} {
		if got := overlayRaiseAbove(ours, i); got != -1 {
			t.Errorf("overlayRaiseAbove(%s, with a sibling surface above or below it) = %d, want -1. "+
				"Two opaque surfaces that do not overlap have no ordering requirement between "+
				"themselves, and giving them one makes them take each other's place for ever",
				list[i].name, got)
		}
	}
}

func TestOverlayRaiseAboveDoesNotOscillateBetweenTwoSurfaces(t *testing.T) {
	// The same fact stated as the behaviour an operator would have seen, because
	// the COUNT is the part that mattered: not "one of them is in the wrong
	// place" but "both of them are torn out of the window over and over".
	//
	// Each round is one layout pass: both surfaces get a new rectangle and both
	// re-assert their position. Ten rounds stands in for a fraction of a second
	// of a resize drag.
	list := []overlaySib{overlaySibWeb(), overlaySibSurface("picture"), overlaySibSurface("decklink")}

	moves := 0
	for round := 0; round < 10; round++ {
		for _, name := range []string{"picture", "decklink"} {
			self := overlaySibIndex(list, name)
			if target := overlayRaiseAbove(overlaySibFlags(list), self); target != -1 {
				list = overlaySibMoveAbove(list, self, target)
				moves++
			}
		}
	}
	if moves != 0 {
		t.Fatalf("two settled surfaces produced %d addSubview: calls over 10 layout passes, want 0. "+
			"Each one takes a view hosting a live GL surface out of its window and puts it back, "+
			"and during a resize drag a layout pass is every frame", moves)
	}
}

func TestOverlayRaiseAboveSettlesInOneMove(t *testing.T) {
	// A surface that really is in the wrong place has to be fixed, and then left
	// alone: the rule has to CONVERGE, not merely be true at some moment. Here
	// the picture starts below the webview, which is what a WebView2-style
	// reorder or a Wails re-layout leaves behind.
	list := []overlaySib{overlaySibSurface("picture"), overlaySibWeb(), overlaySibSurface("decklink")}

	moves := 0
	for round := 0; round < 5; round++ {
		for _, name := range []string{"picture", "decklink"} {
			self := overlaySibIndex(list, name)
			if target := overlayRaiseAbove(overlaySibFlags(list), self); target != -1 {
				list = overlaySibMoveAbove(list, self, target)
				moves++
			}
		}
	}
	if moves != 1 {
		t.Fatalf("one misplaced surface took %d moves to settle over 5 layout passes, want 1", moves)
	}
	if got := overlaySibIndex(list, "WailsWebView"); got != 0 {
		t.Fatalf("the webview ended up at index %d of %v, want 0: both surfaces must be above it",
			got, list)
	}
}

func TestOverlayRaiseAbovePicksTheTopmostForeignSibling(t *testing.T) {
	// One move has to clear every foreign sibling at once, not the nearest one.
	// Answering with the nearest would settle eventually but would tear the GL
	// surface out of the window once per foreign view, which is exactly the cost
	// the rule exists to avoid.
	list := []overlaySib{
		overlaySibSurface("picture"),
		overlaySibWeb(),
		{name: "SomeOtherWailsView"},
	}
	if got := overlayRaiseAbove(overlaySibFlags(list), 0); got != 2 {
		t.Fatalf("overlayRaiseAbove with two foreign siblings above = %d, want 2 (the topmost). "+
			"Going above the nearest one costs a second detach and reattach", got)
	}
}

func TestOverlayRaiseAboveRefusesAnIndexThatIsNotInTheList(t *testing.T) {
	// The far side does UNSIGNED arithmetic on this answer against a live
	// NSArray. A view that is not in the list — which cannot happen, since the
	// superview came from the view itself — must come back as "do nothing"
	// rather than as an index into something else.
	for _, self := range []int{-1, 3, 99} {
		if got := overlayRaiseAbove([]bool{false, true, true}, self); got != -1 {
			t.Errorf("overlayRaiseAbove(self = %d) = %d, want -1", self, got)
		}
	}
	if got := overlayRaiseAbove(nil, 0); got != -1 {
		t.Errorf("overlayRaiseAbove(no siblings at all) = %d, want -1", got)
	}
}

func TestOverlayDarwinStillAsksTheSameStackingQuestion(t *testing.T) {
	// The seam. Everything above tests overlayRaiseAbove, and overlayRaiseAbove
	// moves nothing: the code that moves the picture is the Objective-C in
	// overlay_darwin.go, which cannot run here and is not even compiled at Gate A.
	// This is a file read, like picture_test.go's pin on the coordinate flip, and
	// it exists so the two cannot drift apart silently.
	src, err := os.ReadFile("overlay_darwin.go")
	if err != nil {
		t.Fatalf("reading overlay_darwin.go: %v", err)
	}
	text := string(src)

	// The old rule, by name. It was "am I the last subview", it was correct while
	// there was one surface in the process, and it is the thing a tidy-up would
	// most plausibly put back.
	if strings.Contains(text, "lastObject") {
		t.Error("overlay_darwin.go tests [[sup subviews] lastObject] again. That rule is false for " +
			"one of any two surfaces on the same contentView, on every apply: with the DeckLink " +
			"preview alongside the SRT picture the two take each other's place for ever, and each " +
			"exchange takes a live GL surface out of its window")
	}

	// Recognising our own kind is the whole of the fix, and it depends on the
	// view actually being one of them.
	for _, want := range []string{
		"@interface WSLCommsOverlayView : NSView",
		"[[WSLCommsOverlayView alloc] initWithFrame",
		"isKindOfClass:[WSLCommsOverlayView class]",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("overlay_darwin.go no longer contains %q. Without all three the surfaces "+
				"cannot tell each other apart from the webview, and the rule collapses back to "+
				"an unconditional raise", want)
		}
	}

	// And the scan direction, which is what makes one move enough. Downward from
	// the top of the list, stopping at the first foreign view, gives the TOPMOST
	// foreign view; upward from self, stopping at the first, gives the NEAREST
	// one and costs an extra detach per foreign sibling.
	const scan = "for (NSUInteger i = [subs count]; i > self_idx + 1; i--)"
	if !strings.Contains(text, scan) {
		t.Errorf("overlay_darwin.go no longer scans the subview list as %q, so it may no longer "+
			"answer with the TOPMOST foreign sibling that overlayRaiseAbove is tested against",
			scan)
	}

	// The raise must stay conditional AND stay relative. relativeTo:nil at apply
	// time is the unconditional form by another name: it says "above every
	// sibling", which is the rule that cannot be true for two surfaces at once.
	// It is correct in wslcomms_overlay_create_on_main, where the view is one
	// second old and hosting nothing, so only the body of apply is looked at.
	const applyFn = "static void wslcomms_overlay_apply("
	at := strings.Index(text, applyFn)
	if at < 0 {
		t.Fatalf("could not find %q in overlay_darwin.go to check how it raises the view", applyFn)
	}
	if strings.Contains(text[at:], "positioned:NSWindowAbove relativeTo:nil") {
		t.Error("wslcomms_overlay_apply raises relativeTo:nil, which is 'above every sibling' and " +
			"therefore above the other surface too. It has to be positioned relative to the " +
			"foreign view it needs to clear")
	}
}
