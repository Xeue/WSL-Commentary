package sender

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"wslcomms/internal/gst"
)

// statesBuffer is the depth of the channel returned by States.
//
// The reconnect loop must never be stalled by the UI, so emit drops rather than
// blocks (see emit). Thirty-two is far more than the loop can produce between
// two reads by any consumer that is alive at all: one full connect-fail-backoff
// cycle emits three states and the shortest cycle is bounded below by the seven
// second first rung of BackoffLadder. The buffer therefore only ever fills if
// the consumer has stopped reading entirely, which is exactly the case the
// dropping behaviour is for.
const statesBuffer = 32

// errsBuffer is the depth of the sender's internal copy of the pipeline's error
// channel. One lost SRT peer typically produces a burst of GST_ELEMENT_ERROR
// messages — the sink's own write failure plus whatever the bus posts behind it
// — and only the first of them means anything: they all say the same thing, and
// the state machine collapses the burst into a single CONNECTED -> DRAINING
// transition. Eight is room for a burst; beyond that the watcher drops, because
// a dropped duplicate of a message we have already acted on costs nothing.
const errsBuffer = 8

// senderImpl is the concrete Sender.
//
// # Concurrency design
//
// Every state transition happens on exactly one goroutine — the one running
// run. Nothing else reads or writes the state machine's variables, so there is
// no lock over the state and no possibility of two transitions interleaving.
// Everything else reaches the loop through channels:
//
//   - quit, closed exactly once by Stop, is how a caller on any goroutine asks
//     the loop to unwind. It appears in every select the loop can block in,
//     which is what makes Stop prompt from the middle of a thirty second
//     backoff.
//   - errs, fed by the error-watching goroutine, is how the pipeline's
//     asynchronous failures reach the loop.
//   - states, written only by the loop and closed only by the loop, is how
//     transitions reach the UI. One writer that is also the only closer is what
//     makes "no send on a closed channel" and "no double close" structural
//     rather than something to be careful about.
//
// mu guards only the Start/Stop bookkeeping, and is never held while the loop
// is running or waited on, so it cannot deadlock against the loop.
type senderImpl struct {
	p     gst.Pipeline
	clock clock

	// states is created by newSender so that States can be called, and ranged
	// over, before Start. It is closed by the run goroutine during shutdown.
	states chan State

	// errs carries pipeline errors from the watcher goroutine to the loop. It is
	// never closed: a closed channel is permanently ready, which would spin
	// every select the loop makes. Decoupling from gst.Pipeline.Errors — which
	// Stop does close — is the whole reason the watcher exists.
	errs chan error

	// quit is closed by Stop, exactly once, via stopOnce.
	quit chan struct{}

	// loopDone is closed by the run goroutine after the pipeline is stopped, the
	// final state is emitted, states is closed and the watcher has exited.
	// Anything published before that close is safely visible to a reader that
	// has received from it.
	loopDone chan struct{}

	// stopErr is the result of gst.Pipeline.Stop. Written by the run goroutine
	// before loopDone is closed and read by Stop after loopDone is closed, so it
	// needs no lock.
	stopErr error

	// hook is a test seam: if non-nil it is called on the run goroutine with
	// each state, immediately before that state is offered to the states
	// channel. It is nil in every shipped build — New does not set it and
	// nothing exported can — and exists so that the tests can call Stop while
	// the machine is provably in a chosen state, including the momentary
	// DRAINING, without sleeping and hoping.
	hook func(State)

	mu       sync.Mutex
	started  bool
	stopOnce sync.Once
}

// newSender builds a senderImpl on the given clock. New uses realClock; the
// tests use a fake so the backoff ladder runs in microseconds.
func newSender(p gst.Pipeline, c clock) *senderImpl {
	return &senderImpl{
		p:        p,
		clock:    c,
		states:   make(chan State, statesBuffer),
		errs:     make(chan error, errsBuffer),
		quit:     make(chan struct{}),
		loopDone: make(chan struct{}),
	}
}

// Compile-time assertion that the implementation satisfies the contract.
var _ Sender = (*senderImpl)(nil)

// States returns the state stream. See the Sender interface.
//
// It is valid before Start, so a caller can begin consuming and then start.
func (s *senderImpl) States() <-chan State {
	return s.states
}

// Start plays the pipeline and launches the state machine. See the Sender
// interface.
//
// gst.Pipeline.Start is called while the Start/Stop mutex is held so that two
// concurrent Starts cannot both reach it. A failure there is the one thing that
// is an error from Start: the pipeline will not play, so there is nothing to
// reconnect. A failure to *connect* is not — it is a transition to
// StateBackoff, because the sender retries indefinitely.
//
// After a failed gst.Pipeline.Start the Sender is left not-started, so a caller
// that fixes the configuration may call Start again. After a successful Start,
// and after Stop, Start returns ErrAlreadyStarted: a stopped Sender cannot be
// restarted, because its states channel is closed and its pipeline is at NULL.
func (s *senderImpl) Start(opts Opts) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return ErrAlreadyStarted
	}
	if err := s.p.Start(opts.Pipeline); err != nil {
		return fmt.Errorf("sender: start pipeline: %w", err)
	}
	s.started = true

	go s.run(opts)
	return nil
}

// Stop unwinds the session. See the Sender interface.
//
// It closes quit exactly once and then waits for the run goroutine to finish,
// so that by the time it returns the pipeline is at NULL, StateStopped has been
// emitted and the states channel is closed. Waiting is what makes Stop
// meaningful to WP-8, which stops the sender and then may rebuild it.
//
// Stop is idempotent in effect: the second and later calls close nothing, wait
// on an already-closed loopDone and return the same error as the first.
//
// It cannot deadlock against the error-watching goroutine: that goroutine
// selects on quit as well as on the pipeline's error channel, so it returns
// even if the pipeline never closes the channel Errors handed out, and it never
// takes the Start/Stop mutex.
//
// The one wait Stop cannot shorten is a gst.Pipeline.ReplaceSink call already in
// flight: ReplaceSink is synchronous by contract and the loop checks quit
// immediately on either side of it. Cancelling an SRT handshake mid-flight is
// not something the frozen gst.Pipeline interface offers. That, and not anything
// this package controls, is Stop's worst case — internal/gst bounds the sink
// state change at ten seconds — so Stop must not be called from a UI thread.
// See the Sender interface.
func (s *senderImpl) Stop() error {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()

	if !started {
		return ErrNotStarted
	}

	s.stopOnce.Do(func() { close(s.quit) })
	<-s.loopDone
	return s.stopErr
}

// run owns the whole session on one goroutine: it starts the error watcher,
// runs the state machine until quit is closed, and then performs the shutdown
// sequence in the order the Sender contract promises — stop the pipeline, emit
// StateStopped, close the states channel.
//
// The watcher is joined before loopDone is closed so that a Sender that has
// been stopped has no goroutines of its own left running. That matters because
// WP-8 may create and stop a Sender repeatedly across a match as the operator
// changes device.
func (s *senderImpl) run(opts Opts) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.watchErrors(s.p.Errors())
	}()

	s.loop(opts)

	stopErr := s.p.Stop()
	if stopErr != nil {
		stopErr = fmt.Errorf("sender: stop pipeline: %w", stopErr)
	}

	s.emit(StateStopped)
	close(s.states)

	wg.Wait()

	s.stopErr = stopErr
	close(s.loopDone)
}

// watchErrors forwards the pipeline's asynchronous errors onto s.errs.
//
// It exists so that the state machine never selects on the channel returned by
// gst.Pipeline.Errors: Stop closes that channel, and a closed channel is ready
// forever, which would turn every select in the loop into a busy spin at the
// worst possible moment.
//
// It never blocks on the forward — it drops, exactly as the pipeline
// implementations drop rather than block when their own buffer is full, and for
// the same reason: a lost duplicate of "the peer is gone" costs nothing, and a
// stalled error path costs the match. It returns when the pipeline closes its
// channel or when quit is closed, whichever happens first; the quit case means
// a pipeline that fails to close its channel cannot leak this goroutine.
func (s *senderImpl) watchErrors(errs <-chan error) {
	for {
		select {
		case <-s.quit:
			return
		case err, ok := <-errs:
			if !ok {
				return
			}
			if err == nil {
				// A nil error on the bus channel still means the sink posted
				// something; treat it as a failure rather than dropping it,
				// since the alternative is staying CONNECTED with no peer.
				err = fmt.Errorf("sender: pipeline reported an unspecified error")
			}
			select {
			case s.errs <- err:
			default:
			}
		}
	}
}

// loop is the state machine of specification section 6.2. It returns when quit
// has been closed, and on exactly one failure of its own accord: a ReplaceSink
// error wrapping gst.ErrPipelineFatal, the latched capture-or-mux-chain death
// that no reconnect can repair — see the branch after the ReplaceSink call.
//
//	CONNECTING --connect ok--> CONNECTED --error on srtout--> DRAINING --> BACKOFF --> CONNECTING
//	     '--connect fails-------------------------------------------------> BACKOFF
//
// attempt counts consecutive failures and indexes BackoffLadder. It is reset by
// a successful connect, so the first wait after a mid-match drop is the seven
// second rung that clears M2L-X's re-accept refusal window, not whatever rung
// an earlier outage had climbed to.
//
// The three calls the loop makes on the pipeline are ordered exactly as
// specification section 6.2 orders the states: RemoveSink on entry to DRAINING,
// then the wait, then ReplaceSink on entry to CONNECTING. Collapsing the first
// into the third — which the frozen interface used to force, and which
// gst.Pipeline.RemoveSink was added to make unnecessary — is what makes the wait
// elapse while we still hold the socket.
func (s *senderImpl) loop(opts Opts) {
	attempt := 0
	s.emit(StateConnecting)

	for {
		if s.quitting() {
			return
		}

		// CONNECTING. The first thing this state does is discard whatever is
		// queued, BEFORE the swap and never after it. What a queued error MEANS
		// depends on which side of the swap it arrives on, and that is what makes
		// the placement correct rather than arbitrary:
		//
		//   - Before. Nothing is installed to fail. DRAINING called
		//     gst.Pipeline.RemoveSink on the way out of CONNECTED and the
		//     shortest rung of the ladder has elapsed since, so anything queued
		//     here is the tail of the old sink's death rattle, or srtq's, posted
		//     while it was being driven to NULL. It is stale by construction and
		//     it describes a connection already declared lost. Carrying it across
		//     the swap would tear down the session this iteration is about to
		//     establish: green lamp, amber lamp, seven second wait — and because
		//     attempt is zeroed by every successful connect, the ladder restarts
		//     at its first rung each time, so on a network that has fully
		//     recovered the flap sustains itself indefinitely. sleep absorbs the
		//     bulk of these during the wait; this covers the sliver between the
		//     wait ending and the call, and the first-connect case where Start
		//     itself posted something.
		//
		//   - After it returns nil. The gate is open — gst_cgo.go's last act is
		//     p.gateClosed.Store(false) — buffers are already hitting the new
		//     srtsink, and the private route that diverted that sink's errors
		//     into the call has been cleared. So an error on this channel now
		//     belongs to the NEW sink, or to srtq beneath it. It is live, and it
		//     must be honoured. This is not a corner: an M2L-X listener still
		//     holding its one permitted peer accepts the socket and then fails
		//     the first write, which lands squarely in the window spanning
		//     gst_cgo.go's success log, its unlock, the quit check and the
		//     ForceKeyUnit round trip below. Discarding that error leaves the
		//     machine CONNECTED with nothing on the wire, and it stays there:
		//     onBusMessage closes the gate before delivering, a dropping _BLOCK
		//     probe returns GST_FLOW_OK, so srtq never takes a bad flow return
		//     and no second bus error is ever posted. Permanent false green,
		//     which is the one outcome this package exists to prevent.
		//
		// So the drain goes on the stale side and nowhere else. Leaving it on the
		// live side and shrinking the window does not help, because the window
		// cannot be made not to exist; telling the two cases apart there would
		// need every error to carry a sink generation, which is a change to the
		// frozen gst.Pipeline contract, not a change to this file.
		s.drainErrors()

		// ReplaceSink installs a fresh srtsink: block the srtq src pad, unlink,
		// NULL, remove, recreate, link, sync, unblock. It is synchronous — a nil
		// error means the SRT caller handshake succeeded.
		//
		// On the reconnect path the removal half of that is a no-op, because
		// DRAINING has already torn the old sink out. That is the point: the
		// removal must happen before the wait, not at the end of it, and it is
		// also what makes the drain above safe — there has been no sink in the
		// pipeline for the whole of the backoff.
		err := s.p.ReplaceSink(opts.Sink)
		if s.quitting() {
			return
		}

		// A latched pipeline-fatal is the one failure the ladder must never
		// absorb. errors.Is(err, gst.ErrPipelineFatal) means the capture or
		// mux chain itself is dead — the measured case is the operator
		// selecting a RENDER endpoint as the commentary input, whose wasapi2
		// buffer fails asynchronously after preroll — and internal/gst latches
		// the condition, so every further ReplaceSink on this pipeline returns
		// the same error instantly, forever. Retrying that is not persistence,
		// it is the misdiagnosis this branch exists to end: the sender used to
		// climb to the thirty second cap telling the operator the feed "is not
		// connected and is retrying" — blaming the SRT network for a local
		// device fault no reconnect could ever repair.
		//
		// Returning here runs run's ordinary shutdown unchanged — p.Stop(),
		// StateStopped emitted, states closed — so the SENDING lamp goes grey
		// STOPPED rather than sitting amber for the rest of the match. That is
		// not merely less misleading, it is the documented recovery performed
		// halfway: a latched fatal is repaired only by Stop, New, Start, and a
		// stopped sender is precisely the machine asking the operator to do
		// that. The report goes out first, once, with the real reason; d is
		// zero because no wait follows, and reportConnectError words the log
		// accordingly.
		if err != nil && errors.Is(err, gst.ErrPipelineFatal) {
			s.reportConnectError(opts.OnConnectError, err, attempt+1, 0)
			return
		}

		if err == nil {
			// Force an IDR before announcing the connection. The encoder's GOP
			// is 100 frames at 50p (specification section 5), so without this
			// the far end can wait up to two seconds for a picture; with it the
			// slate is back as soon as the handshake completes.
			//
			// A failure here is deliberately not fatal. It costs at most those
			// two seconds, and the alternative — tearing down a connection that
			// is otherwise working — is strictly worse. If the pipeline really
			// is gone, the error channel says so and the next iteration handles
			// it.
			_ = s.p.ForceKeyUnit()

			attempt = 0

			// Nothing is discarded here. Everything on s.errs from this point on
			// is about the sink that has just been installed — see the drain
			// above the ReplaceSink call — so the CONNECTED select below is
			// entitled to act on the first message it finds, including one that
			// was already queued before this emit.
			s.emit(StateConnected)

			// CONNECTED. The only thing that can happen here is the peer going
			// away, which arrives as an error from the bus.
			select {
			case <-s.quit:
				return
			case <-s.errs:
			}

			// DRAINING. Tear the dead sink out now, before the wait, not as a
			// side effect of the next ReplaceSink. Specification section 6.2
			// orders the cycle DRAINING (block the srtq src pad, unlink, srtout
			// to NULL, remove) -> BACKOFF -> CONNECTING, and that order is the
			// whole point of the ladder: an M2L-X listener accepts one peer,
			// never displaces the incumbent, and refuses re-accept for roughly
			// five seconds, so the >= 6 s first rung only buys anything if OUR
			// socket is already gone when it starts ticking. Leaving the
			// teardown to ReplaceSink would close the socket microseconds before
			// dialling again, landing the retry inside the refusal window and
			// costing a wasted attempt — about fourteen seconds off air instead
			// of seven, on every mid-match reconnect.
			s.emit(StateDraining)

			// A teardown failure must not wedge the machine. Reaching BACKOFF
			// and trying again is strictly better than stopping: the next
			// ReplaceSink performs the same removal itself, so a machine that
			// keeps going can still recover, and one that gave up is off air for
			// the rest of the match.
			if rmErr := s.p.RemoveSink(); rmErr != nil {
				log.Printf("sender: could not remove the failed sink: %v; backing off and retrying anyway", rmErr)
			}
			if s.quitting() {
				return
			}
		}

		// BACKOFF. The wait happens here, on the state machine's own goroutine,
		// never inside a pad probe or a bus callback — a blocked bus callback
		// stalls the whole pipeline. There is no attempt limit for anything a
		// retry could ever fix: on total network loss libsrt declares the peer
		// dead at about 5.27 s and exits, and M2L-X never recovers by itself,
		// so a sender that gave up on a NETWORK fault would leave the
		// commentary off air for the rest of the match. The one exception
		// never reaches this line: a ReplaceSink error wrapping
		// gst.ErrPipelineFatal returned out of the loop above, because that
		// condition is latched and local and every retry is guaranteed to fail
		// identically.
		d := backoffDelay(attempt)
		attempt++
		if err != nil {
			// Only a failed CONNECTING has a reason to report. Arriving here
			// with err nil means the peer went away after a successful connect,
			// which is not a connection error and already has its own state.
			s.reportConnectError(opts.OnConnectError, err, attempt, d)
		}
		s.emit(StateBackoff)
		if !s.sleep(d) {
			return
		}
		s.emit(StateConnecting)
	}
}

// reportConnectError announces why one connection attempt failed: to the log
// always, and to the caller's Opts.OnConnectError if it set one. It is called on
// the state-machine goroutine — immediately before the transition to
// StateBackoff for a retryable failure, and immediately before loop returns
// into run's shutdown for the pipeline-fatal failure that stops the machine.
//
// attempt is the number of consecutive failures including this one, and d the
// wait about to be served. d == 0 means no wait follows because the sender is
// stopping, and the log line says so rather than promising a retry "in 0s":
// this code path exists to end a message that misled the operator, so its own
// message is not allowed to mislead. For retryable failures the attempt/delay
// pair is in the message because a climbing count with an unchanging reason is
// the after-the-match signature of a fault that was never transient, and the
// log is where a support engineer reconstructs that.
//
// The callback is invoked on this goroutine, so a caller that blocks in it
// blocks the reconnect. That is documented on Opts. The recover is there because
// the failure mode is asymmetric: a panic in a UI callback would take down the
// whole process — Go gives no way to contain a panic on another goroutine — and
// during a live match keeping the commentary path alive beats failing fast on
// someone else's bug, which the log line preserves either way.
func (s *senderImpl) reportConnectError(report func(error), err error, attempt int, d time.Duration) {
	if d > 0 {
		log.Printf("sender: connect attempt %d failed: %v; retrying in %v", attempt, err, d)
	} else {
		// The pipeline-fatal stop. The wrapped error already carries the
		// recovery instruction ("recover with Stop, New, Start"), so the log
		// only needs to be honest about what happens next: nothing.
		log.Printf("sender: connect attempt %d failed: %v; stopping instead of retrying", attempt, err)
	}

	if report == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("sender: OnConnectError panicked: %v; the reconnect loop is continuing", r)
		}
	}()
	report(err)
}

// sleep waits for d on the injected clock and reports whether the wait
// completed. It returns false as soon as quit is closed, which is what makes
// Stop prompt from the middle of the thirty second rung.
//
// Errors arriving during the wait are absorbed without restarting or shortening
// it. They are stale by definition: DRAINING has already called
// gst.Pipeline.RemoveSink, so by the time this is entered there is genuinely no
// sink installed to fail and they can only be the tail of the burst that caused
// this backoff. Reacting to them would either double-count the ladder or reset
// the timer forever on a noisy bus.
//
// That claim used to be made here without the removal behind it, and it was
// false: the old sink stayed installed and in PLAYING for the whole wait. It is
// worth stating rather than assuming, because it is the sentence that concealed
// the missing teardown — the reasoning read as sound, so nobody checked that
// anything actually performed it.
//
// It is also why this absorbs rather than drains once on entry: a burst is
// spread over the wait, not queued before it.
func (s *senderImpl) sleep(d time.Duration) bool {
	fire, stop := s.clock.NewTimer(d)
	defer stop()

	for {
		select {
		case <-s.quit:
			return false
		case <-fire:
			return true
		case <-s.errs:
		}
	}
}

// drainErrors removes everything currently queued on s.errs without blocking.
//
// It is called at exactly one place: immediately before ReplaceSink installs a
// new sink, to discard bus messages left over from the sink DRAINING already
// tore out. See the comment at that call site for why it belongs on that side of
// the call and why calling it on the other side is a permanent false green.
//
// It was previously called on entry to BACKOFF as well, where it was dead code —
// sleep selects on s.errs and absorbs the whole burst, and every path from
// DRAINING reaches sleep. That was demonstrated by mutation: with the call
// commented out the entire suite still passed, including the two tests named for
// it. Removing it means the drain is now load-bearing wherever it appears, which
// is the only condition under which a test of it means anything.
func (s *senderImpl) drainErrors() {
	for {
		select {
		case <-s.errs:
		default:
			return
		}
	}
}

// quitting reports whether Stop has been called, without blocking. It is the
// check either side of the one synchronous call the loop makes.
func (s *senderImpl) quitting() bool {
	select {
	case <-s.quit:
		return true
	default:
		return false
	}
}

// emit publishes a state transition. It is called only from the run goroutine,
// which is also the only closer of the channel, so it can never send on a closed
// channel.
//
// It never blocks. If the buffer is full the oldest queued state is discarded to
// make room for the newest, so a consumer that has fallen behind loses
// intermediate transitions but always converges on the current one — which is
// the only one a lamp can render anyway. Blocking here would let a stalled
// WebView2 renderer stop the sender reconnecting, and that trade is not
// available during a match.
//
// The loop below runs at most twice: there is exactly one writer, so a
// successful discard guarantees the following send finds room.
func (s *senderImpl) emit(st State) {
	if s.hook != nil {
		s.hook(st)
	}
	for {
		select {
		case s.states <- st:
			return
		default:
		}
		select {
		case <-s.states:
		default:
		}
	}
}
