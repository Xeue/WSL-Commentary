package m2lx

import "time"

// clockSource abstracts wall-clock time and periodic ticking so the
// Watcher's reconnect/debounce/staleness logic can be driven by an
// injectable fake clock in tests instead of real sleeps. Production code
// always uses realClock; tests substitute a manually-advanced fake (see
// clock_test.go) so a 15 s staleness timeout or a 4 s debounce window runs
// instantly and deterministically.
type clockSource interface {
	// Now returns the current time.
	Now() time.Time
	// NewTicker returns a running ticker with period d.
	NewTicker(d time.Duration) tickerSource
}

// tickerSource abstracts *time.Ticker so it can be faked in tests.
type tickerSource interface {
	// C returns the channel on which ticks are delivered.
	C() <-chan time.Time
	// Stop turns off the ticker. It does not close C.
	Stop()
}

// realClock is the production clockSource, backed by the time package.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTicker(d time.Duration) tickerSource {
	return realTicker{time.NewTicker(d)}
}

// realTicker adapts *time.Ticker to tickerSource.
type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }
