package sender

import "time"

// clock is the sender's only view of time. It exists for one reason: the
// backoff ladder is 7, 7, 10, 15, 20 then 30 seconds forever, and a test that
// waited those durations for real would take minutes per case and would be the
// first thing anyone deleted. With a fake clock the whole ladder is exercised in
// microseconds and the delays can be asserted exactly.
//
// It is deliberately not exported: the production Sender always runs on
// realClock, and New takes only a gst.Pipeline, so there is no way for a caller
// to substitute time in a shipped build.
type clock interface {
	// NewTimer arms a one-shot timer for d and returns the channel it will fire
	// on together with a function that releases it.
	//
	// The channel must be buffered, or fired by a goroutine, so that a timer
	// whose deadline passes while nobody is selecting on it does not deadlock
	// the firing side. stop must be safe to call whether or not the timer has
	// already fired, and safe to call more than once, because it is invoked from
	// a defer on every path out of the backoff sleep — including the Stop path,
	// which is the one that must be prompt.
	NewTimer(d time.Duration) (fire <-chan time.Time, stop func())
}

// realClock is the production clock: time.Timer, which is cancellable. The
// sender must never use time.Sleep, because Stop has to return promptly from
// the middle of a thirty second backoff wait — during a match, "the operator
// pressed Stop and the app ignored them for half a minute" is an outage of its
// own.
type realClock struct{}

// NewTimer arms a time.Timer for d.
func (realClock) NewTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// Compile-time assertion that the production clock satisfies the seam.
var _ clock = realClock{}
