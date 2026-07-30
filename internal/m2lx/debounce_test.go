package m2lx

import (
	"testing"
	"time"
)

// base is an arbitrary fixed reference instant; tests advance from it in
// whole seconds so the expected outcomes below are easy to check by eye.
var base = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

func at(seconds int) time.Time { return base.Add(time.Duration(seconds) * time.Second) }

func TestDebouncer_FirstValueAdoptedImmediately(t *testing.T) {
	d := newDebouncer(4 * time.Second)
	got, changed := d.Observe("starting", at(0))
	if got != "starting" || !changed {
		t.Fatalf("first Observe = (%q,%v), want (\"starting\",true)", got, changed)
	}
}

func TestDebouncer_MomentaryFlapDoesNotChangeConfirmed(t *testing.T) {
	d := newDebouncer(4 * time.Second)
	d.Observe("streaming", at(0))

	// Drops for 2s, well under the 4s window, then recovers.
	if got, changed := d.Observe("stopped", at(1)); got != "streaming" || changed {
		t.Fatalf("Observe(stopped, t=1) = (%q,%v), want (\"streaming\",false)", got, changed)
	}
	if got, changed := d.Observe("streaming", at(3)); got != "streaming" || changed {
		t.Fatalf("Observe(streaming, t=3) = (%q,%v), want (\"streaming\",false) — a momentary flap must not flap the lamp", got, changed)
	}
	// Even after the original window would have elapsed, the confirmed
	// value never moved, so nothing should be pending to commit either.
	if got, changed := d.Tick(at(10)); got != "streaming" || changed {
		t.Fatalf("Tick(t=10) after recovery = (%q,%v), want (\"streaming\",false)", got, changed)
	}
}

func TestDebouncer_PersistedChangeCommitsAfterWindow(t *testing.T) {
	d := newDebouncer(4 * time.Second)
	d.Observe("streaming", at(0))
	d.Observe("stopped", at(1))

	// Not yet 4s since the change was first observed.
	if got, changed := d.Observe("stopped", at(4)); got != "streaming" || changed {
		t.Fatalf("Observe(stopped, t=4) = (%q,%v), want (\"streaming\",false)", got, changed)
	}
	// At/after t=1+4=5s the pending value commits.
	got, changed := d.Observe("stopped", at(5))
	if got != "stopped" || !changed {
		t.Fatalf("Observe(stopped, t=5) = (%q,%v), want (\"stopped\",true)", got, changed)
	}
	// Subsequent observations of the now-confirmed value report no change.
	if got, changed := d.Observe("stopped", at(6)); got != "stopped" || changed {
		t.Fatalf("Observe(stopped, t=6) = (%q,%v), want (\"stopped\",false)", got, changed)
	}
}

func TestDebouncer_TickCommitsWithoutFurtherObserve(t *testing.T) {
	d := newDebouncer(4 * time.Second)
	d.Observe("streaming", at(0))
	d.Observe("starting", at(0))

	if got, changed := d.Tick(at(3)); got != "streaming" || changed {
		t.Fatalf("Tick(t=3) = (%q,%v), want (\"streaming\",false)", got, changed)
	}
	got, changed := d.Tick(at(4))
	if got != "starting" || !changed {
		t.Fatalf("Tick(t=4) = (%q,%v), want (\"starting\",true)", got, changed)
	}
	// Ticking again after commit must not re-report a change.
	if got, changed := d.Tick(at(5)); got != "starting" || changed {
		t.Fatalf("Tick(t=5) = (%q,%v), want (\"starting\",false)", got, changed)
	}
}

func TestDebouncer_RestartingPendingValueResetsDeadline(t *testing.T) {
	d := newDebouncer(4 * time.Second)
	d.Observe("streaming", at(0))
	d.Observe("stopped", at(0)) // pending "stopped", deadline t=4

	// A different pending value arrives before the first deadline: the
	// window must restart against the new value, not carry over.
	d.Observe("starting", at(2)) // pending "starting", deadline t=6

	if got, changed := d.Tick(at(4)); got != "streaming" || changed {
		t.Fatalf("Tick(t=4) = (%q,%v), want (\"streaming\",false) — restarted deadline must not have committed yet", got, changed)
	}
	got, changed := d.Tick(at(6))
	if got != "starting" || !changed {
		t.Fatalf("Tick(t=6) = (%q,%v), want (\"starting\",true)", got, changed)
	}
}

func TestDebouncer_TableOfSequences(t *testing.T) {
	type step struct {
		t    int
		raw  string
		want string
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{
			name: "streaming stays streaming through a single glitch",
			steps: []step{
				{0, "streaming", "streaming"},
				{1, "starting", "streaming"},
				{2, "streaming", "streaming"},
				{20, "streaming", "streaming"},
			},
		},
		{
			name: "stopped confirms after sustained window",
			steps: []step{
				{0, "streaming", "streaming"},
				{10, "stopped", "streaming"},
				{13, "stopped", "streaming"},
				{14, "stopped", "stopped"},
				{15, "stopped", "stopped"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDebouncer(4 * time.Second)
			for _, s := range tc.steps {
				got, _ := d.Observe(s.raw, at(s.t))
				if got != s.want {
					t.Fatalf("at t=%d: Observe(%q) confirmed=%q, want %q", s.t, s.raw, got, s.want)
				}
			}
		})
	}
}
