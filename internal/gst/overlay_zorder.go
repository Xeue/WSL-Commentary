// overlay_zorder.go is the stacking rule the native picture surfaces obey,
// written once in Go so that it can be argued about and tested somewhere other
// than inside an Objective-C block on AppKit's main thread.
//
// # Why there is a rule at all
//
// Both real overlays — the child HWND in overlay_windows.go and the NSView in
// overlay_darwin.go — are OPAQUE siblings of the webview, and both have to stay
// above it. Neither host framework promises to leave them there: WebView2
// reorders its own windows with no notification, and AppKit's subview list is
// Wails' to rearrange. So each overlay re-asserts its position every time it
// moves.
//
// Up to now "its position" meant "on top of everything", because there was
// exactly one of these surfaces in the process and on top of everything and above
// the webview were the same sentence. Tier 3 adds a DeckLink confidence preview,
// which is a SECOND surface on the same parent, and the two sentences come apart:
// only one view can be topmost, so a rule written as "am I topmost" is false for
// one of them on every single apply, forever.
//
// # Why that is a real cost on macOS and not on Windows
//
// This is the asymmetry the two overlay files now have, and it is written down
// here because it looks like an inconsistency and is not.
//
// On Windows the re-assertion is SetWindowPos(HWND_TOP), which moves an entry in
// a sibling list. It does not unparent the window, gstd3d11's swapchain is bound
// to the HWND rather than to its place among its siblings, and two non-overlapping
// WS_CLIPSIBLINGS children have the same visible region whichever way round they
// are. Doing it when it was not needed costs a syscall.
//
// On macOS the only way to reorder an existing subview is -[NSView addSubview:],
// which removes the view from its superview and adds it back — taking a view that
// is hosting a live GL surface out of its window for an instant. Doing it when it
// was not needed costs the picture.
//
// So Windows re-raises unconditionally and macOS asks first, and what macOS asks
// is the rule below.
//
// # Why the rule lives in Go
//
// For the reason cocoaOverlayFrame in picture.go does: the expression that
// actually runs is the Objective-C one, because it has to run on the main thread
// against a subview list that only exists there, and overlay_darwin.go is not
// compiled at all at Gate A. This file carries no build tag so that the rule can
// be exercised on any platform, without a window and without cgo, and
// overlay_zorder_test.go asserts that the Objective-C still spells the same
// scan.
package gst

// overlayRaiseAbove decides whether an overlay surface is in the right place
// among its siblings, and if it is not, which sibling it has to be put above.
//
// ours[i] answers "is sibling i one of this package's overlay surfaces", in the
// platform's own bottom-first order — subview 0 of an NSView's subviews array is
// the one furthest back. self is the index of the surface being applied.
//
// The answer is the index of the TOPMOST sibling above self that is NOT one of
// ours, or -1 for "already correct, do nothing". Going above that one sibling
// clears every foreign sibling at once, because anything else foreign that is
// above self is below it by construction.
//
// Two properties are being asserted by the shape of this, and both matter:
//
//   - Our own kind are SKIPPED. Two opaque overlays that never overlap have no
//     ordering requirement between themselves, and a rule that gave them one
//     would have each of them displacing the other on every apply.
//
//   - -1 is the common answer, not the exceptional one. A settled window's
//     layout pass must do no work whatever, because on macOS the work is a
//     detach and reattach of a live GL surface.
//
// A self that is not an index into ours is -1 as well: a surface that is not in
// the list is not a surface that can be usefully moved within it, and the caller
// on the far side is doing unsigned arithmetic with the answer.
func overlayRaiseAbove(ours []bool, self int) int {
	if self < 0 || self >= len(ours) {
		return -1
	}
	for i := len(ours) - 1; i > self; i-- {
		if !ours[i] {
			return i
		}
	}
	return -1
}
