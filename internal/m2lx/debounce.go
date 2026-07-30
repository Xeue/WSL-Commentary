package m2lx

import "time"

// debouncer implements the stream_state debounce required by
// windows-app-spec.md §8 and CONTRACT.md: a value must persist for
// DebounceWindow before it is reflected in the confirmed value the lamp
// reads, so a momentary transition does not flap it.
//
// It is pure and takes the current time as an explicit argument on every
// call rather than reading the wall clock itself, so it can be exercised
// in tests with fabricated timestamps and no real sleeping.
type debouncer struct {
	window time.Duration

	initialized bool
	confirmed   string

	pendingActive bool
	pendingVal    string
	deadline      time.Time
}

// newDebouncer returns a debouncer that commits a changed value only once
// it has been observed continuously for window.
func newDebouncer(window time.Duration) *debouncer {
	return &debouncer{window: window}
}

// Observe records a value seen at time now — typically because a fresh
// WebSocket message just carried it. It returns the currently confirmed
// value and whether this call changed it.
//
// The very first observed value is adopted immediately: there is nothing
// yet to debounce against, and holding the lamp in an unknown state for a
// further DebounceWindow after the first real message would only delay the
// UI for no benefit.
func (d *debouncer) Observe(raw string, now time.Time) (confirmed string, changed bool) {
	if !d.initialized {
		d.initialized = true
		d.confirmed = raw
		d.pendingActive = false
		return d.confirmed, true
	}
	if raw == d.confirmed {
		// Back to the confirmed value: whatever was pending no longer
		// matters, so cancel it rather than let a stale deadline fire
		// later for a value we are no longer even seeing.
		d.pendingActive = false
		return d.confirmed, false
	}
	if !d.pendingActive || d.pendingVal != raw {
		d.pendingActive = true
		d.pendingVal = raw
		d.deadline = now.Add(d.window)
	}
	if !now.Before(d.deadline) {
		d.confirmed = d.pendingVal
		d.pendingActive = false
		return d.confirmed, true
	}
	return d.confirmed, false
}

// Tick allows a pending value to commit from the passage of time alone,
// for a caller that polls periodically (the Watcher's reconnect/staleness
// ticker) rather than relying solely on the next message to notice a
// deadline has passed. It returns the confirmed value and whether this
// call changed it.
func (d *debouncer) Tick(now time.Time) (confirmed string, changed bool) {
	if d.pendingActive && !now.Before(d.deadline) {
		d.confirmed = d.pendingVal
		d.pendingActive = false
		return d.confirmed, true
	}
	return d.confirmed, false
}
