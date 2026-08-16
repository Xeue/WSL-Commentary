// signalwatch_test.go is the Gate A cover for the video signal watchdog.
//
// No build tag, deliberately, and the reason is the same one that made
// signalwatch.go untagged: the hysteresis is the entire feature, its failure
// modes are silent in both directions — a lamp that flickers, and a lamp that
// stays green over black — and neither should be reachable only on the one
// machine in the building with a DeckLink in it. The state machine is exercised
// here as a pure unit with injected readings, which is a better test of it than
// waving a cable at a card could ever be: every sequence below is one somebody
// would have to produce by hand, at the right speed, repeatably.
//
// The poll loop is driven through startSignalWatchOn with a tick channel the
// test owns, so it runs in microseconds and asserts exact deliveries rather than
// sleeping and hoping.

package gst

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The tunables
// ---------------------------------------------------------------------------

func TestSignalHoldOffsAreSeveralSamplesDeep(t *testing.T) {
	// A one-sample hold-off is not hysteresis, it is the raw property with extra
	// steps — and it would restore exactly the flickering lamp this file exists
	// to prevent. This is the guard on somebody later lengthening
	// signalPollInterval without noticing which constants it divides.
	if signalOKSamples < 2 {
		t.Errorf("signalOKSamples = %d; a hold-off below two samples is no hysteresis at all", signalOKSamples)
	}
	if signalLostSamples < 2 {
		t.Errorf("signalLostSamples = %d; a hold-off below two samples is no hysteresis at all", signalLostSamples)
	}
	if signalUnknownSamples < 2 {
		t.Errorf("signalUnknownSamples = %d; a hold-off below two samples is no hysteresis at all", signalUnknownSamples)
	}
}

func TestSignalHoldOffsAreAsymmetricInTheRightDirection(t *testing.T) {
	// THE decision this file makes. A spurious "no video signal" during a match
	// is worse than a fraction of a second's delay in clearing one, so raising
	// the alarm is patient and clearing it is prompt. If these two ever come out
	// equal or the wrong way round, the design has been reversed by accident.
	if signalLostSamples <= signalOKSamples {
		t.Errorf("signalLostSamples = %d, signalOKSamples = %d; going TO lost must be strictly slower "+
			"than coming back", signalLostSamples, signalOKSamples)
	}
}

func TestSignalHoldOffsDivideThePollIntervalExactly(t *testing.T) {
	// The sample counts are derived by integer division, so a hold-off that is
	// not a whole number of intervals silently becomes a shorter one than the
	// constant says. Truncation is the safe direction but a hold-off that does
	// not mean what it reads is a comment that lies.
	for _, tc := range []struct {
		name  string
		hold  time.Duration
		count int
	}{
		{"lost", signalLostHold, signalLostSamples},
		{"ok", signalOKHold, signalOKSamples},
		{"unknown", signalUnknownHold, signalUnknownSamples},
	} {
		if got := time.Duration(tc.count) * signalPollInterval; got != tc.hold {
			t.Errorf("%s hold-off is %v but %d samples at %v is %v",
				tc.name, tc.hold, tc.count, signalPollInterval, got)
		}
	}
}

func TestSignalStateFor(t *testing.T) {
	// triUnknown must never map to SignalLost. "I could not ask" and "there is
	// no signal" are opposite amounts of information, and conflating them would
	// put a red lamp on every machine that has no card in it.
	for _, tc := range []struct {
		in   triState
		want SignalState
	}{
		{triTrue, SignalOK},
		{triFalse, SignalLost},
		{triUnknown, SignalUnknown},
	} {
		if got := signalStateFor(tc.in); got != tc.want {
			t.Errorf("signalStateFor(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// The debouncer
// ---------------------------------------------------------------------------

// feed pushes a run of identical readings in and returns every report produced.
func feed(d *signalDebouncer, r triState, n int) []SignalReport {
	var out []SignalReport
	for i := 0; i < n; i++ {
		if rep, ok := d.observe(r); ok {
			out = append(out, rep)
		}
	}
	return out
}

func TestSignalDebouncerStartsUnknownAndSaysNothing(t *testing.T) {
	var d signalDebouncer
	if d.state != "" {
		t.Fatalf("the zero signalDebouncer should have no state yet, got %q", d.state)
	}
	// One reading can never produce a report, because no hold-off is one sample
	// deep. That is what keeps startup quiet: the probe reading that decided
	// whether to watch at all is fed straight in as the seed.
	if rep, ok := d.observe(triTrue); ok {
		t.Errorf("one reading produced %+v; no hold-off may be a single sample", rep)
	}
	if d.state != SignalUnknown {
		t.Errorf("state after one reading = %q, want %q", d.state, SignalUnknown)
	}
}

func TestSignalDebouncerReachesOKAfterExactlyTheOKHoldOff(t *testing.T) {
	var d signalDebouncer

	if reps := feed(&d, triTrue, signalOKSamples-1); len(reps) != 0 {
		t.Fatalf("%d readings produced %+v, want nothing before the hold-off is met",
			signalOKSamples-1, reps)
	}
	reps := feed(&d, triTrue, 1)
	if len(reps) != 1 {
		t.Fatalf("the %dth good reading produced %d reports, want exactly 1", signalOKSamples, len(reps))
	}
	if reps[0].State != SignalOK {
		t.Errorf("report state = %q, want %q", reps[0].State, SignalOK)
	}
	if reps[0].Flaps != 0 {
		t.Errorf("report flaps = %d, want 0 for a clean run of identical readings", reps[0].Flaps)
	}

	// And it does not keep saying so. A lamp is edge-triggered; a report per
	// poll would be four events a second for the whole of a healthy match.
	if reps := feed(&d, triTrue, 20); len(reps) != 0 {
		t.Errorf("a steady locked input produced %+v, want silence after the first report", reps)
	}
}

func TestSignalDebouncerReachesLostOnlyAfterTheFullLostHoldOff(t *testing.T) {
	d := lockedDebouncer(t)

	if reps := feed(&d, triFalse, signalLostSamples-1); len(reps) != 0 {
		t.Fatalf("%d bad readings produced %+v; the alarm must not be raised before %v of "+
			"CONTINUOUS loss", signalLostSamples-1, reps, signalLostHold)
	}
	reps := feed(&d, triFalse, 1)
	if len(reps) != 1 || reps[0].State != SignalLost {
		t.Fatalf("the %dth bad reading produced %+v, want one %q report", signalLostSamples, reps, SignalLost)
	}
	if reps[0].Flaps != 1 {
		// One transition: the good run ended and the bad run began. Anything
		// else means the flap counter is measuring something other than raw
		// transitions since the last report.
		t.Errorf("report flaps = %d, want 1 for a single clean drop-out", reps[0].Flaps)
	}
}

func TestSignalDebouncerClearsTheAlarmFast(t *testing.T) {
	d := lockedDebouncer(t)
	feed(&d, triFalse, signalLostSamples) // now LOST

	if reps := feed(&d, triTrue, signalOKSamples); len(reps) != 1 || reps[0].State != SignalOK {
		t.Fatalf("%d good readings produced %+v, want one %q report", signalOKSamples, reps, SignalOK)
	}
	// The asserted consequence of the asymmetry, in samples: clearing takes
	// strictly fewer readings than raising. Recovery at the element is
	// single-frame, and a stale alarm over a picture that has come back is the
	// more damaging of the two errors.
	if signalOKSamples >= signalLostSamples {
		t.Fatalf("clearing took %d samples and raising took %d; the asymmetry is inverted",
			signalOKSamples, signalLostSamples)
	}
}

func TestSignalDebouncerRequiresACONSECUTIVERun(t *testing.T) {
	// The hold-off is a consecutive run and NOT a leaky bucket. An input that is
	// bad far more often than it is good must still never raise the alarm unless
	// it is bad CONTINUOUSLY for signalLostHold, because "continuously for two
	// seconds" is the property the lamp asserts.
	d := lockedDebouncer(t)

	for cycle := 0; cycle < 5; cycle++ {
		if reps := feed(&d, triFalse, signalLostSamples-1); len(reps) != 0 {
			for _, r := range reps {
				if r.State == SignalLost {
					t.Fatalf("cycle %d: an interrupted run of bad readings raised %q; the hold-off "+
						"is accumulating instead of restarting", cycle, SignalLost)
				}
			}
		}
		feed(&d, triTrue, 1) // one good reading restarts the run
	}
}

func TestSignalDebouncerNeverReadsUnreadableAsLost(t *testing.T) {
	// A property that cannot be read is not evidence about the input. It must
	// withdraw the lamp to UNKNOWN — never to LOST, which would be this
	// application asserting a fault it has no evidence for.
	d := lockedDebouncer(t)

	if reps := feed(&d, triUnknown, signalUnknownSamples-1); len(reps) != 0 {
		t.Fatalf("%d unreadable samples produced %+v, want nothing yet", signalUnknownSamples-1, reps)
	}
	reps := feed(&d, triUnknown, 1)
	if len(reps) != 1 {
		t.Fatalf("produced %d reports, want exactly 1", len(reps))
	}
	if reps[0].State != SignalUnknown {
		t.Errorf("state = %q, want %q; an unreadable property is not a fault", reps[0].State, SignalUnknown)
	}
}

func TestSignalDebouncerRecoversFromUnknownAtTheOKSpeed(t *testing.T) {
	// UNKNOWN is where the machine starts and where it withdraws to, and coming
	// out of it towards OK is the ordinary startup path — it must be as prompt
	// as any other recovery, not as patient as raising an alarm.
	var d signalDebouncer
	if reps := feed(&d, triTrue, signalOKSamples); len(reps) != 1 || reps[0].State != SignalOK {
		t.Fatalf("%d good readings from UNKNOWN produced %+v, want one %q report",
			signalOKSamples, reps, SignalOK)
	}
}

// ---------------------------------------------------------------------------
// The marginal input: what the asymmetry deliberately hides, and what stops
// that being a lie
// ---------------------------------------------------------------------------

func TestSignalDebouncerHoldsTheLampSteadyThroughAFlappingInput(t *testing.T) {
	// The intended bias, asserted rather than assumed. A cable dropping lock
	// twice a second never accumulates signalLostSamples consecutive bad
	// readings, so the lamp stays OK. An operator gets a steady indicator
	// instead of a strobing one, which is the whole reason the hold-off exists.
	d := lockedDebouncer(t)

	for i := 0; i < 40; i++ {
		r := triTrue
		if i%2 == 0 {
			r = triFalse
		}
		for _, rep := range feed(&d, r, 1) {
			if rep.State != SignalOK {
				t.Fatalf("reading %d: a flapping input moved the lamp to %q; the hold-off is not "+
					"holding", i, rep.State)
			}
		}
	}
	if d.state != SignalOK {
		t.Errorf("state after flapping = %q, want %q", d.state, SignalOK)
	}
}

func TestSignalDebouncerReportsFlapsWithoutAStateChange(t *testing.T) {
	// ...and this is what stops the steady lamp being a silent lie. The state
	// does not change, so nothing else in this design would ever speak; the flap
	// alert is the one mechanism by which "OK, but the input is rattling"
	// reaches anybody at all.
	d := lockedDebouncer(t)

	var reps []SignalReport
	for i := 0; i < signalFlapAlert; i++ {
		r := triTrue
		if i%2 == 0 {
			r = triFalse
		}
		reps = append(reps, feed(&d, r, 1)...)
	}
	if len(reps) != 1 {
		t.Fatalf("%d raw transitions produced %d reports, want exactly 1", signalFlapAlert, len(reps))
	}
	if reps[0].State != SignalOK {
		t.Errorf("flap report state = %q, want the unchanged %q", reps[0].State, SignalOK)
	}
	if reps[0].Flaps != signalFlapAlert {
		t.Errorf("flap report flaps = %d, want %d", reps[0].Flaps, signalFlapAlert)
	}

	// The counter resets, so Flaps means "since the last report" rather than
	// "since the pipeline started" — otherwise a rig that flapped once at
	// kick-off would still be flagged at full time.
	if reps := feed(&d, triTrue, 20); len(reps) != 0 {
		t.Errorf("a settled input produced %+v; the flap counter did not reset", reps)
	}
}

func TestSignalFlapAlertRateIsBounded(t *testing.T) {
	// The flap alert is the one path that can emit without a state change, so it
	// is the one that could in principle chatter. Its floor is signalFlapAlert
	// polls per report — a second at the current interval, which is the rate
	// m2lx.Watcher already emits status at — and this pins that arithmetic.
	d := lockedDebouncer(t)

	const samples = 400
	n := 0
	for i := 0; i < samples; i++ {
		r := triTrue
		if i%2 == 0 {
			r = triFalse
		}
		n += len(feed(&d, r, 1))
	}
	if max := samples / signalFlapAlert; n > max {
		t.Errorf("%d samples of maximal flapping produced %d reports, want at most %d", samples, n, max)
	}
	if n == 0 {
		t.Error("maximal flapping produced no reports at all; the marginal input is invisible")
	}
}

// lockedDebouncer returns a debouncer already settled in SignalOK, which is the
// state nearly every interesting sequence starts from.
func lockedDebouncer(t *testing.T) signalDebouncer {
	t.Helper()
	var d signalDebouncer
	if reps := feed(&d, triTrue, signalOKSamples); len(reps) != 1 || reps[0].State != SignalOK {
		t.Fatalf("could not settle the debouncer into %q: %+v", SignalOK, reps)
	}
	return d
}

// ---------------------------------------------------------------------------
// Costing nothing without a card
// ---------------------------------------------------------------------------

func TestSignalWatchWanted(t *testing.T) {
	emit := func(SignalReport) {}
	for _, tc := range []struct {
		name  string
		emit  func(SignalReport)
		probe triState
		want  bool
	}{
		{
			// The whole no-cost path. Every native capture and the slate-only
			// video leg this application ships today land here: no goroutine, no
			// ticker, nothing paid for the life of the pipeline.
			name: "no signal property to read", emit: emit, probe: triUnknown, want: false,
		},
		{
			name: "nobody wants reports", emit: nil, probe: triTrue, want: false,
		},
		{
			name: "a locked card", emit: emit, probe: triTrue, want: true,
		},
		{
			// A card that is already unlocked at Start is exactly when the
			// watchdog is most needed — this is the operator who pressed START
			// before the camera was patched.
			name: "a card with nothing on its input", emit: emit, probe: triFalse, want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := signalWatchWanted(tc.emit, tc.probe); got != tc.want {
				t.Errorf("signalWatchWanted(_, %v) = %v, want %v", tc.probe, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The poll loop
// ---------------------------------------------------------------------------

// watchHarness drives startSignalWatchOn one sample at a time, with no real time
// passing and no sleeps anywhere.
//
// The determinism comes from both channels being UNBUFFERED. Sending a tick
// blocks until the loop receives it; supplying the reading blocks until the loop
// asks for one; and sending the NEXT tick therefore cannot complete until the
// loop has finished observing the previous reading and come back to its select.
// So by the time step returns, every report that sample was going to produce has
// already been delivered.
type watchHarness struct {
	ticks    chan time.Time
	readings chan triState
	reports  chan SignalReport
	w        *signalWatch
}

func newWatchHarness(seed triState) *watchHarness {
	h := &watchHarness{
		ticks:    make(chan time.Time),
		readings: make(chan triState),
		reports:  make(chan SignalReport, 64),
	}
	h.w = startSignalWatchOn(h.ticks, seed,
		func() triState { return <-h.readings },
		func(r SignalReport) { h.reports <- r })
	return h
}

// step delivers one poll: a tick, then the reading that poll returns.
func (h *watchHarness) step(r triState) {
	h.ticks <- time.Time{}
	h.readings <- r
}

// drain returns every report delivered so far. It is called after Stop, which
// joins the goroutine, so nothing can still be in flight.
func (h *watchHarness) drain() []SignalReport {
	var out []SignalReport
	for {
		select {
		case r := <-h.reports:
			out = append(out, r)
		default:
			return out
		}
	}
}

func TestSignalWatchSeedAloneReportsNothing(t *testing.T) {
	// A watch that starts and is immediately stopped must be silent, whatever
	// the card was doing at Start. The seed is one reading and no hold-off is
	// one sample deep — the same property the debouncer test asserts, checked
	// here through the loop, because it is the loop that would show a lamp
	// flicking on and off at every Start if it were wrong.
	for _, seed := range []triState{triTrue, triFalse, triUnknown} {
		h := newWatchHarness(seed)
		h.w.Stop()
		if reps := h.drain(); len(reps) != 0 {
			t.Errorf("seed %v produced %+v at start-up, want silence", seed, reps)
		}
	}
}

func TestSignalWatchPollsAndReportsThroughTheLoop(t *testing.T) {
	h := newWatchHarness(triTrue)

	// The seed counted as one good reading, so one more settles it.
	for i := 0; i < signalOKSamples-1; i++ {
		h.step(triTrue)
	}
	// Then a full lost hold-off.
	for i := 0; i < signalLostSamples; i++ {
		h.step(triFalse)
	}
	// Then recovery.
	for i := 0; i < signalOKSamples; i++ {
		h.step(triTrue)
	}
	h.w.Stop()

	reps := h.drain()
	want := []SignalState{SignalOK, SignalLost, SignalOK}
	if len(reps) != len(want) {
		t.Fatalf("got %d reports (%+v), want %d", len(reps), reps, len(want))
	}
	for i := range want {
		if reps[i].State != want[i] {
			t.Errorf("report %d state = %q, want %q", i, reps[i].State, want[i])
		}
	}
}

func TestSignalWatchStopDeliversNothingAfterwards(t *testing.T) {
	// Stop JOINS, so no report can arrive after it returns. That is what stops a
	// lamp coming back on for a pipeline that no longer exists — the "a frozen
	// meter reads as a live one" failure, in the direction this project never
	// allows a status display to be wrong in.
	h := newWatchHarness(triFalse)
	for i := 0; i < signalLostSamples-2; i++ {
		h.step(triFalse)
	}
	h.w.Stop()

	before := len(h.drain())
	// The loop is gone: nothing is reading h.ticks, so a send would block
	// forever rather than produce a report. Assert the goroutine is finished
	// instead, which is the property Stop actually promises.
	select {
	case <-h.w.done:
	default:
		t.Fatal("Stop returned while the watch goroutine was still running")
	}
	if after := len(h.drain()); after != 0 || before != 0 {
		t.Errorf("reports before Stop = %d, after = %d; want none of either — the alarm was one "+
			"sample short of its hold-off", before, after)
	}
}

func TestSignalWatchStopIsIdempotentAndNilSafe(t *testing.T) {
	// gst_cgo.go holds one field, leaves it nil for every pipeline with no card,
	// and stops it unconditionally from one place. Both of those depend on this.
	var none *signalWatch
	none.Stop()

	h := newWatchHarness(triTrue)
	h.w.Stop()
	h.w.Stop()
	h.w.Stop()
}

func TestSignalWatchEndsWhenItsTickSourceCloses(t *testing.T) {
	// A closed tick channel must end the loop rather than spin on it. It cannot
	// happen with the real ticker — startSignalWatch stops that only after the
	// loop has exited — but a loop that would burn a core if it ever did is not
	// something to leave to the call site being right forever.
	h := newWatchHarness(triTrue)
	close(h.ticks)
	select {
	case <-h.w.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the watch did not exit when its tick source closed")
	}
	h.w.Stop() // still safe, and still returns
}
