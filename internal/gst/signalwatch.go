// signalwatch.go is the video signal watchdog: the thing that stops this
// application transmitting a healthy, correctly-bitrated H.264 encode of BLACK
// with every lamp green and nothing anywhere reporting a fault.
//
// Owner: WP-DECKLINK tier 2, by agreement with WP-3a, which owns gst_cgo.go and
// starts and stops the watch from there. It is a file of its own with NO BUILD
// TAG, on this package's standing rule that everything decidable without
// GStreamer is lifted into ordinary Go: the poll loop and the whole hysteresis
// decision are compiled and unit-tested at Gate A, under -race, with no card and
// no GStreamer anywhere. The only part that is not here is the ONE property read
// that turns an element into a boolean, which is a closure gst_cgo.go supplies.
//
// # Why this file has to exist at all
//
// MEASURED. decklinkvideosrc with drop-no-signal-frames=false — the default, and
// what this application uses — keeps emitting BLACK, GAP-flagged frames at full
// rate FOREVER when the input loses signal. There is no error, no EOS, and the
// muxer never starves, so every mechanism this application already has for
// noticing trouble stays quiet:
//
//   - internal/sender is CONNECTED, because the SRT socket is fine and media is
//     flowing at exactly the bitrate it should be.
//   - the switcher's own status reports a healthy, correctly-formatted feed,
//     because it is receiving one.
//   - capturefault.go never runs, because nothing posts GST_ELEMENT_ERROR.
//   - the level meters keep moving, because the commentary audio is untouched.
//
// The one thing GStreamer does say is a bus WARNING, and it is useless for this
// twice over: gst_cgo.go routes warnings to logWarnings and DELIBERATELY never
// to Errors(), and the element guards it with its own signal_state so it is
// EDGE-TRIGGERED — it fires ONCE in a ninety-minute match, into a log nobody is
// reading, and never again. A watchdog built on it would miss every fault that
// began before the log was opened and every fault after the first.
//
// So the only truthful indicators are the "signal" GObject property and
// GST_BUFFER_FLAG_GAP, and this file polls the first of them.
//
// # 720x486 IS AMBIGUOUS, AND CAPS CAN NEVER ANSWER THIS QUESTION
//
// State this here because it is exactly the shortcut the next reader will reach
// for. The obvious cheap test — "look at the negotiated caps, a no-signal card
// will be reporting something implausible" — DOES NOT WORK, and it does not work
// in the most misleading way available: under GST_DECKLINK_MODE_AUTO an input
// with nothing on it reports modes[0], which is 720x486 NTSC, and 720x486 NTSC
// is ALSO A REAL RASTER that a real source can legitimately be feeding. The two
// cases are indistinguishable in the caps. A caps-based check would therefore be
// right almost everywhere, wrong on genuine NTSC, and — far worse — would look
// entirely reasonable to anyone reviewing it. The property and the GAP flag are
// the only two things that know, which is the same fact capturefault_cgo.go's
// propSignal comment records from the other direction.
//
// # Why the property, and not a GAP-flag pad probe
//
// Both are truthful; they cost differently and they answer slightly different
// questions.
//
// A pad probe for GST_BUFFER_FLAG_GAP runs ON THE STREAMING THREAD, ONCE PER
// BUFFER — fifty times a second at 1080p50, in the hot path of the video leg,
// forever, whether or not anything is wrong. That is not expensive per call, but
// it is a permanent tax on the one thread this package's file comments are
// emphatic about not loading, and it scales with frame rate rather than with how
// often anybody wants an answer. It is also not exclusively a no-signal
// indicator: GAP is a general-purpose flag that other elements set for other
// reasons, so a probe placed anywhere but directly on the source's own src pad
// would eventually be answering a different question than the one asked.
//
// A property poll is ONE g_object_get on a gboolean — a struct field read behind
// the GObject lock, nanoseconds, the same primitive the level path already
// performs twenty times a second — from an ordinary Go goroutine that is not a
// streaming thread at all. Its cost is fixed by the poll interval and is
// independent of the frame rate, and it reads the very state the element itself
// uses to decide whether to set GAP.
//
// What is given up: SAMPLING. A dropout shorter than one poll interval is
// invisible to this file. That loses nothing, because of the next section — the
// hysteresis deliberately refuses to act on anything shorter than
// signalLostHold, which is eight poll intervals — so a dropout this file cannot
// see is a dropout it would have discarded anyway.
//
// # HYSTERESIS IS THE POINT, AND IT IS ASYMMETRIC
//
// MEASURED: recovery is SINGLE-FRAME. The first good frame flips the element's
// state back with no hold-off and no debounce of any kind. So the raw property
// on a marginal cable, a re-clocking source or a switcher changing format is a
// boolean that rattles, and a lamp driven straight off it would rattle with it —
// which is worse than useless, because an indicator that cries wolf is one an
// operator learns to ignore precisely when it finally means something.
//
// The two directions therefore hold off by DIFFERENT amounts, and the direction
// of the asymmetry is a judgement about which mistake costs more during a match:
//
//   - GOING TO LOST IS SLOW (signalLostHold, 2 s). A signal loss that matters
//     persists — a pulled cable, a camera powered down, a source unpatched — so
//     two seconds of patience costs nothing real. What it buys is immunity to
//     the transients that are NOT faults, chiefly the re-lock while an upstream
//     format changes. Raising "no video signal" at a commentary position in the
//     middle of a match because the truck changed format is a false alarm the
//     operator cannot verify and cannot act on.
//   - COMING BACK IS FAST (signalOKHold, 0.5 s). A stale alarm sitting over a
//     picture that has already returned is the more damaging of the two errors:
//     it is actively wrong, and it is wrong in the direction that makes the
//     operator distrust the indicator.
//
// # What the asymmetry gives up, and what is done about it
//
// An input that alternates faster than signalLostHold never accumulates a long
// enough run of bad readings, so it reads as steady OK. That is the intended
// bias — green over flicker — but on its own it would be a second kind of lie:
// "everything is fine" is not the truth about a cable that is dropping lock
// twice a second.
//
// So the debouncer also COUNTS raw transitions, and SignalReport carries the
// count. A report is emitted when the debounced state changes, and also the
// moment the raw reading has flapped signalFlapAlert times since the last
// report — so a marginal input surfaces as OK with a rising flap count rather
// than as silence. The extra emissions are bounded: signalFlapAlert flaps need
// at least that many poll intervals, so the worst case is one report per second,
// which is the rate m2lx.Watcher already emits status at.
//
// # Costing nothing when there is no card
//
// A native capture — osxaudiosrc, wasapi2src, and the PNG slate leg this
// application still ships with — has no "signal" property to read, and polling
// one would be a goroutine and a timer burning for a value that can never be
// anything but "cannot tell". So the watch is NOT STARTED AT ALL in that case:
// gst_cgo.go takes ONE reading at Start and hands it to signalWatchWanted, and a
// triUnknown there means no goroutine, no ticker, and no further cost for the
// life of the pipeline. That single probe doubles as the debouncer's seed, so it
// is not wasted work either.
//
// # What this file is NOT
//
// It is not a fault reporter and it does not decide anything. It does not stop
// the pipeline, does not touch internal/sender, and never produces an error: a
// video input with no signal is exactly the RECOVERABLE case capturefault.go
// argues at length — the commentary is the product, it keeps flowing, and the
// far end sees black rather than nothing. This file's entire job is to make sure
// somebody is TOLD.

package gst

import (
	"sync"
	"time"
)

// SignalState is the debounced answer to "is there video on the card's input",
// and it is the source of the frontend's signal lamp.
//
// It is a string for the same reason DeviceKind and ReturnState are: it crosses
// the Wails JSON boundary, and "state: 0" in a bug report is a value somebody
// has to go and look up at the moment they can least afford to.
type SignalState string

const (
	// SignalUnknown means THIS APPLICATION CANNOT TELL YOU, and it is the state
	// before the first reading, the state of every non-DeckLink capture, and the
	// state of the slate-only pipeline that ships today.
	//
	// IT IS NOT A FAULT AND MUST NOT BE DRAWN AS ONE. A native capture device has
	// no signal property; reporting it as SignalOK would be this application
	// inventing a green lamp out of an absence of evidence, which is the precise
	// dishonesty the whole file exists to remove, and reporting it as SignalLost
	// would put a red lamp on every machine without a card. It is the same
	// distinction triState draws for capturefault.go, and for the same reason:
	// "false" and "I could not ask" are opposite amounts of information.
	SignalUnknown SignalState = "UNKNOWN"

	// SignalOK means the card reports a locked input and has done so steadily
	// for at least signalOKHold.
	SignalOK SignalState = "OK"

	// SignalLost means the card reports no lock and has done so continuously for
	// at least signalLostHold. The pipeline is still running, still encoding and
	// still sending — at full rate, correctly formatted, and BLACK.
	SignalLost SignalState = "LOST"
)

// SignalReport is one delivery from the watchdog.
//
// It is a struct rather than a bare SignalState because the state alone cannot
// express the case the hysteresis deliberately hides; see Flaps.
type SignalReport struct {
	// State is the debounced state, and is what the lamp shows.
	State SignalState

	// Flaps is how many times the RAW property reading changed between the
	// previous report and this one.
	//
	// Zero on a clean transition — a cable pulled out once reads bad, bad, bad
	// and flaps once at most. A non-trivial count on a report whose State did
	// not change is the marginal-input case: the debouncer is holding the lamp
	// steady, correctly, and this is the number that stops that being a lie. A
	// frontend may render it as an "unstable" qualifier; a field log wants it
	// either way, because "green, 47 flaps" and "green, 0 flaps" are different
	// nights.
	Flaps int
}

// The poll interval and the two hold-offs. Every one of these is a decision
// about what an operator needs weighed against what it costs, and the counts
// derived below are what the state machine actually works in.
const (
	// signalPollInterval is how often the "signal" property is read.
	//
	// COST: one g_object_get on a gboolean, from a goroutine that is not a
	// streaming thread. capturefault_cgo.go measures that primitive in
	// nanoseconds, and four per second against a pipeline encoding fifty frames
	// per second is not a quantity that can be found in a profile.
	//
	// WHY NOT FASTER: nothing here acts on a single reading, so a finer grid buys
	// only a finer grid. The soonest anything can be reported is signalOKHold,
	// and that is a decision about false alarms rather than about sampling.
	//
	// WHY NOT SLOWER: the hold-offs have to be several samples deep or a single
	// anomalous reading moves the answer by a large fraction of the hold-off. At
	// one second, signalLostHold would be a two-sample decision. At 250 ms it is
	// eight, and no individual reading is anywhere near decisive.
	signalPollInterval = 250 * time.Millisecond

	// signalLostHold is how long the card must report NO LOCK CONTINUOUSLY
	// before the lamp says so. See the file comment: a real loss persists, so
	// this patience is free, and what it buys is not raising an alarm an operator
	// can neither verify nor act on during a format change or a re-clock.
	//
	// Two seconds is also inside what the job needs. The question this lamp
	// answers is asked when somebody notices black, and nobody notices black in
	// under a second.
	signalLostHold = 2 * time.Second

	// signalOKHold is how long a locked input must persist before the alarm
	// clears. It is DELIBERATELY SHORTER than signalLostHold; the file comment
	// has the argument.
	//
	// Recovery at the element is single-frame, so one good reading is already
	// proof that the card has locked. Requiring two costs 250 ms and means one
	// anomalous good reading in the middle of a genuine outage cannot clear the
	// alarm on its own — which matters, because that is exactly what a cable
	// hanging out of a socket produces.
	signalOKHold = 500 * time.Millisecond

	// signalUnknownHold is how many unreadable samples it takes to withdraw the
	// lamp back to SignalUnknown, and it matches signalLostHold rather than
	// signalOKHold on purpose: withdrawing a claim is as consequential as making
	// one, and a property that momentarily cannot be read is not evidence about
	// the input. It is only reachable mid-session if the element is disposed
	// underneath the watch, which is a case the bus error path owns; this is the
	// backstop that stops the lamp holding a stale green if it ever happens.
	signalUnknownHold = signalLostHold

	// signalFlapAlert is how many raw transitions since the last report force a
	// report even though the debounced state has not changed. Four is two full
	// round trips (good, bad, good, bad): ONE round trip is a single glitch and
	// is precisely what the hysteresis is for, whereas two of them inside one
	// steady state is a pattern rather than an event.
	signalFlapAlert = 4
)

// The hold-offs as SAMPLE COUNTS, which is the unit the state machine works in.
//
// They are derived rather than written down so that changing signalPollInterval
// cannot silently change the hold-offs it divides. Integer division truncates,
// which is the safe direction — a shortened hold-off would be the dangerous one
// — and TestSignalHoldOffsAreSeveralSamplesDeep pins that none of them collapses
// to a single sample, because a one-sample hold-off is no hysteresis at all and
// would restore exactly the flickering lamp this file exists to prevent.
const (
	signalLostSamples    = int(signalLostHold / signalPollInterval)
	signalOKSamples      = int(signalOKHold / signalPollInterval)
	signalUnknownSamples = int(signalUnknownHold / signalPollInterval)
)

// signalReader is one reading of the card's "signal" property.
//
// triUnknown means the reading could not be taken — no element, no such
// property, or a property that came back as something other than a gboolean —
// and it is NEVER conflated with triFalse. gst_cgo.go builds this as a closure
// over the capture element; capturefault_cgo.go's boolPropertyTriState is
// already exactly the right shape for the body.
type signalReader func() triState

// signalStateFor maps one raw reading to the state it votes for.
func signalStateFor(r triState) SignalState {
	switch r {
	case triTrue:
		return SignalOK
	case triFalse:
		return SignalLost
	default:
		return SignalUnknown
	}
}

// signalHoldSamples is how many consecutive readings it takes to move INTO a
// given state. The asymmetry between them is the whole design; see the file
// comment.
func signalHoldSamples(s SignalState) int {
	switch s {
	case SignalOK:
		return signalOKSamples
	case SignalLost:
		return signalLostSamples
	default:
		return signalUnknownSamples
	}
}

// signalDebouncer is the hysteresis, as a pure value with no clock, no channel
// and no GStreamer in it.
//
// It is a plain struct and not a goroutine because that is what makes the
// decision testable: every branch of it is exercised at Gate A by feeding
// readings in, which is a better test of a state machine than any amount of
// waving a cable at a card would be. The goroutine below is a loop around it and
// contains no policy at all.
//
// The zero value is the correct initial state: SignalUnknown, no reading seen.
type signalDebouncer struct {
	// state is the debounced state, and is what has actually been reported.
	state SignalState

	// last is the previous raw reading and seen says whether there was one, so
	// that the first reading cannot be counted as a transition from nothing.
	last triState
	seen bool

	// want is the state the current run of readings is voting for, and run is
	// how many consecutive readings have voted for it. A reading that votes
	// differently restarts the run at one rather than decrementing it: this is a
	// CONSECUTIVE-run hold-off, not a leaky bucket, because "continuously for two
	// seconds" is the property being asserted and a bucket would let an
	// intermittent input accumulate its way to an alarm it never sustained.
	want SignalState
	run  int

	// flaps counts raw transitions since the last report. See SignalReport.Flaps.
	flaps int
}

// observe feeds one reading in and returns the report to deliver, if any.
//
// It returns ok=false for the overwhelmingly common case — the reading agrees
// with what is already being shown — so the steady state of this whole mechanism
// is a boolean read, an integer increment and a comparison, four times a second.
func (d *signalDebouncer) observe(r triState) (SignalReport, bool) {
	if d.state == "" {
		d.state = SignalUnknown
	}

	target := signalStateFor(r)

	if d.seen && r != d.last {
		d.flaps++
	}
	d.last, d.seen = r, true

	if target == d.want {
		d.run++
	} else {
		d.want, d.run = target, 1
	}

	// The transition. Note that the run is NOT reset on a change: the readings
	// that carried us here go on agreeing with the new state, which costs an
	// increment and keeps the counter meaning what it says.
	if target != d.state && d.run >= signalHoldSamples(target) {
		d.state = target
		return d.report(), true
	}

	// No change of state, but the raw reading has been rattling. See
	// signalFlapAlert: this is what stops a deliberately steady lamp being a
	// silent lie about a marginal input.
	if d.flaps >= signalFlapAlert {
		return d.report(), true
	}

	return SignalReport{}, false
}

// report renders the current state as a delivery and resets the flap counter,
// which is what makes Flaps mean "since the last report" rather than "since the
// pipeline started".
func (d *signalDebouncer) report() SignalReport {
	rep := SignalReport{State: d.state, Flaps: d.flaps}
	d.flaps = 0
	return rep
}

// signalWatch is the running watchdog: a ticker, a reader and the debouncer.
//
// The nil *signalWatch is usable and Stop on it does nothing, so gst_cgo.go can
// hold one field, leave it nil for every pipeline that has no card, and stop it
// unconditionally in one place rather than guarding the call site.
type signalWatch struct {
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// signalWatchWanted reports whether a watch should be started at all.
//
// probe is ONE reading taken at Start, and triUnknown there is the whole
// no-cost path: it means there is no element with a "signal" property in this
// pipeline — every native capture, and the slate-only video leg — so no
// goroutine and no ticker are created and nothing is paid for the life of the
// pipeline. emit being nil means the caller does not want reports, which is
// every caller that has not wired the frontend up.
//
// It is a function rather than two conditions at the call site so that the
// "costs nothing without a card" rule is one testable thing with a name, instead
// of an `if` somebody later widens without noticing what it was for.
func signalWatchWanted(emit func(SignalReport), probe triState) bool {
	return emit != nil && probe != triUnknown
}

// startSignalWatch begins polling on a real ticker and returns the handle to
// stop it. seed is the probe reading signalWatchWanted was given, fed to the
// debouncer immediately so that the state converges from t=0 rather than from
// the first tick — no hold-off is one sample deep, so the seed can never on its
// own produce a report, and startup stays quiet.
//
// emit is called from the watch's OWN goroutine and not from a streaming thread,
// so unlike PipelineOpts.OnLevels it cannot stall the capture chain. It must
// still not block: this goroutine is the only thing taking readings, so an emit
// that waits delays every later sample and the hold-offs quietly stop meaning
// seconds.
func startSignalWatch(seed triState, read signalReader, emit func(SignalReport)) *signalWatch {
	t := time.NewTicker(signalPollInterval)
	w := startSignalWatchOn(t.C, seed, read, emit)
	go func() {
		<-w.done
		t.Stop()
	}()
	return w
}

// startSignalWatchOn is startSignalWatch with the tick source supplied, which is
// how the loop is tested without real time passing. Nothing else differs.
func startSignalWatchOn(ticks <-chan time.Time, seed triState, read signalReader, emit func(SignalReport)) *signalWatch {
	w := &signalWatch{
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go w.run(ticks, seed, read, emit)
	return w
}

// run is the poll loop. It holds no lock and touches nothing outside itself, so
// the debouncer needs no synchronisation of its own.
func (w *signalWatch) run(ticks <-chan time.Time, seed triState, read signalReader, emit func(SignalReport)) {
	defer close(w.done)

	var d signalDebouncer
	deliver := func(r triState) {
		if rep, ok := d.observe(r); ok {
			emit(rep)
		}
	}
	deliver(seed)

	for {
		select {
		case <-w.stop:
			return
		case _, ok := <-ticks:
			if !ok {
				// The tick source has gone. Ending is right rather than
				// spinning on a closed channel, and it cannot happen with the
				// real ticker, which is stopped only after this loop exits.
				return
			}
			deliver(read())
		}
	}
}

// Stop ends the watch and WAITS for the goroutine to finish, so that no report
// can be delivered after it returns.
//
// The join is the point and not tidiness. gst_cgo.go stops the watch before it
// takes the pipeline to NULL, and both halves of that ordering matter: a reader
// closure still running against a disposed element is a property read on freed
// memory, and an emit arriving after the session has ended would put a lamp back
// on for a pipeline that no longer exists — the "frozen meter reads as a live
// one" failure, in the one direction this project never allows a status display
// to be wrong in.
//
// It is idempotent and nil-safe.
func (w *signalWatch) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
}
