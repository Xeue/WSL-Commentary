//go:build windows

// Tests for the picture process's own window. Unlike the overlay, a top-level
// window needs no parent and no Wails message loop, so these create REAL
// windows — briefly, hidden behind whatever is on screen — and are skipped
// where the desktop cannot give us one (a service session, a CI runner with no
// window station).
package gst

import (
	"testing"
	"time"
)

// newTestPictureWindow creates a window or skips the test if the desktop
// refuses. It is closed when the test ends.
func newTestPictureWindow(t *testing.T, initial *PictureWindowPlacement, onMoved func(PictureWindowPlacement)) PictureWindow {
	t.Helper()
	w, err := NewPictureWindow("wslcomms picture window test", initial, onMoved)
	if err != nil {
		t.Skipf("no window in this session: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func TestPictureWindowExistsUntilClosed(t *testing.T) {
	w := newTestPictureWindow(t, nil, nil)
	if w.Handle() == 0 {
		t.Fatal("the window has no handle; the sink would be given zero and open a window of its own")
	}
	select {
	case <-w.Closed():
		t.Fatal("Closed fired before Close")
	default:
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case <-w.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("Closed did not fire after Close")
	}
	if w.Handle() != 0 {
		t.Fatal("the handle survived Close")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close() error = %v; Close must be idempotent", err)
	}
}

func TestPictureWindowComesBackWhereItWasPut(t *testing.T) {
	// The whole reason placements are remembered: a Refresh is a new process
	// and a new window, and it must appear where the operator left the last
	// one. The rectangle handed to SetWindowPlacement is what GetWindowPlacement
	// then reports — workspace coordinates both ways — so it round-trips exactly.
	want := PictureWindowPlacement{Left: 64, Top: 48, Right: 64 + 800, Bottom: 48 + 450}
	w := newTestPictureWindow(t, &want, nil)

	got, ok := w.Placement()
	if !ok {
		t.Fatal("Placement() reports no placement for a live window")
	}
	if got != want {
		t.Fatalf("the window was placed at %+v, want %+v", got, want)
	}
}

func TestPictureWindowIgnoresAPlacementOffEveryMonitor(t *testing.T) {
	// A placement remembered on a monitor that has since been unplugged would
	// put the picture where nobody can see it, with no error saying so. It must
	// fall back to letting the shell place the window.
	offScreen := PictureWindowPlacement{Left: -30000, Top: -30000, Right: -30000 + 800, Bottom: -30000 + 450}
	if placementOnScreen(offScreen) {
		t.Fatal("placementOnScreen() accepted a rectangle 30000 pixels off the desktop")
	}
	w := newTestPictureWindow(t, &offScreen, nil)
	got, ok := w.Placement()
	if !ok {
		t.Fatal("Placement() reports no placement for a live window")
	}
	if got == offScreen {
		t.Fatalf("the window was placed off every monitor at %+v", got)
	}

	// And a rectangle with no usable size is refused before Win32 is asked.
	for _, bad := range []PictureWindowPlacement{
		{Left: 10, Top: 10, Right: 20, Bottom: 20},
		{Left: 10, Top: 10, Right: 5, Bottom: 300},
		{Left: 0, Top: 0, Right: 100000, Bottom: 100},
	} {
		if bad.sane() {
			t.Errorf("sane() accepted %+v", bad)
		}
	}
}

func TestPictureWindowRecordsItsPlacementWhenDestroyed(t *testing.T) {
	// Even with no drag ever made, the placement is recorded as the window goes
	// — WM_DESTROY is the last chance — so a clean stop leaves the file saying
	// where the window was.
	moved := make(chan PictureWindowPlacement, 8)
	initial := PictureWindowPlacement{Left: 96, Top: 96, Right: 96 + 640, Bottom: 96 + 360}
	w := newTestPictureWindow(t, &initial, func(p PictureWindowPlacement) { moved <- p })

	if err := w.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case p := <-moved:
		if p != initial {
			t.Fatalf("the placement recorded at destroy was %+v, want %+v", p, initial)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onMoved was never called; a clean stop would forget where the window was")
	}
	// After the window has gone, Placement still answers with the last record.
	if got, ok := w.Placement(); !ok || got != initial {
		t.Fatalf("Placement() after Close = %+v, %v; want the last recorded %+v", got, ok, initial)
	}
}
