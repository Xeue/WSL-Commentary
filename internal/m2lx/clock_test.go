package m2lx

import (
	"sync"
	"time"
)

// fakeClock is a manually-advanced clockSource for tests, so debounce,
// staleness and reconnect-backoff timing can be exercised deterministically
// and instantly instead of with real sleeps.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*fakeTicker
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) NewTicker(d time.Duration) tickerSource {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTicker{c: make(chan time.Time, 1), interval: d, next: f.now.Add(d)}
	f.tickers = append(f.tickers, t)
	return t
}

// Advance moves the fake clock forward by d, firing (non-blocking, as
// real time.Ticker does — a slow consumer does not queue up missed ticks)
// any ticker whose next deadline falls at or before the new time.
func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	for _, t := range f.tickers {
		for !t.next.After(f.now) {
			select {
			case t.c <- f.now:
			default:
			}
			t.next = t.next.Add(t.interval)
		}
	}
}

// fakeTicker is fakeClock's tickerSource.
type fakeTicker struct {
	c        chan time.Time
	interval time.Duration
	next     time.Time
	stopped  bool
}

func (t *fakeTicker) C() <-chan time.Time { return t.c }
func (t *fakeTicker) Stop()               { t.stopped = true }
