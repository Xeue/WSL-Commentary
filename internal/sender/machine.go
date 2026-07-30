package sender

import (
	"fmt"
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
// not something the frozen gst.Pipeline interface offers.
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

// loop is the state machine of specification section 6.2. It returns only when
// quit has been closed.
//
//	CONNECTING --connect ok--> CONNECTED --error on srtout--> DRAINING --> BACKOFF --> CONNECTING
//	     '--connect fails-------------------------------------------------> BACKOFF
//
// attempt counts consecutive failures and indexes BackoffLadder. It is reset by
// a successful connect, so the first wait after a mid-match drop is the seven
// second rung that clears M2L-X's re-accept refusal window, not whatever rung
// an earlier outage had climbed to.
func (s *senderImpl) loop(opts Opts) {
	attempt := 0
	s.emit(StateConnecting)

	for {
		if s.quitting() {
			return
		}

		// CONNECTING. ReplaceSink installs a fresh srtsink: block the srtq src
		// pad, unlink, NULL, remove, recreate, link, sync, unblock. It is
		// synchronous — a nil error means the SRT caller handshake succeeded.
		err := s.p.ReplaceSink(opts.Sink)
		if s.quitting() {
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
			s.emit(StateConnected)

			// CONNECTED. The only thing that can happen here is the peer going
			// away, which arrives as an error from the bus.
			select {
			case <-s.quit:
				return
			case <-s.errs:
			}

			// DRAINING. On the frozen gst.Pipeline interface the teardown the
			// specification describes — block the srtq src pad, unlink, set
			// srtout to NULL, remove it — is performed by the next ReplaceSink,
			// which is documented to do exactly that sequence before installing
			// the new sink. There is therefore no call to make here: DRAINING
			// is the interval between losing the peer and starting to wait, and
			// what it does is absorb the rest of the error burst so that one
			// lost peer produces one transition rather than five.
			s.emit(StateDraining)
			s.drainErrors()
			if s.quitting() {
				return
			}
		}

		// BACKOFF. The wait happens here, on the state machine's own goroutine,
		// never inside a pad probe or a bus callback — a blocked bus callback
		// stalls the whole pipeline. There is no attempt limit: on total network
		// loss libsrt declares the peer dead at about 5.27 s and exits, and
		// M2L-X never recovers by itself, so a sender that gave up would leave
		// the commentary off air for the rest of the match.
		d := backoffDelay(attempt)
		attempt++
		s.emit(StateBackoff)
		if !s.sleep(d) {
			return
		}
		s.emit(StateConnecting)
	}
}

// sleep waits for d on the injected clock and reports whether the wait
// completed. It returns false as soon as quit is closed, which is what makes
// Stop prompt from the middle of the thirty second rung.
//
// Errors arriving during the wait are absorbed without restarting or shortening
// it. They are stale by definition: there is no sink installed to fail, so they
// can only be the tail of the burst that caused this backoff, and reacting to
// them would either double-count the ladder or reset the timer forever on a
// noisy bus.
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
// It is called on entry to BACKOFF so that the burst of bus messages produced
// by one lost peer does not immediately re-trigger the machine on the next
// connect.
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
