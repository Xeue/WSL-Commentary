package sender

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"wslcomms/internal/gst"
)

// The reconnect machine, one loop per SRT OUTPUT.
//
// A pipeline has one or two sink slots (gst.Pipeline.Outputs): the muxer's
// output, or a tee behind it feeding two leaky queues. Each slot gets its own
// loop below — its own ladder, its own errors, its own states — because the
// two outputs dial two different listeners and either can be down while the
// other carries the match. What they share is the pipeline: one Start before
// the loops, one Stop after the last of them, and one fatal condition that
// ends them all.
//
// Output 0 is the PRIMARY: its transitions are what States delivers and what
// the SENDING lamp shows. Every output's transitions go to OutputStates.
//
// # The loop, per output
//
//	CONNECTING  ReplaceSinkOn installs a fresh srtsink; on success CONNECTED,
//	            on failure BACKOFF after reporting the reason.
//	CONNECTED   wait for an error from THIS output's sink (or quit); then
//	DRAINING    RemoveSinkOn, so the leaky queue drops rather than blocks, then
//	BACKOFF     sleep the ladder's delay, draining stale errors, then
//	CONNECTING  again. A gst.ErrPipelineFatal from ReplaceSinkOn — the encode
//	            or mux chain has failed — ends EVERY loop: no reconnect can
//	            repair it, and the sender stops rather than retrying for ever.
//
// # Errors are routed by output
//
// The pipeline wraps a sink-sourced error in gst.OutputError naming its slot,
// and the watcher below hands it to that output's channel alone. An error
// that names no output — the muxer, an encoder — goes to every loop, since
// every output is equally affected.

const (
	statesBuffer = 16
	errsBuffer   = 16
)

type senderImpl struct {
	p     gst.Pipeline
	clock clock

	// states carries output 0's transitions: the lamp. outputStates carries
	// every output's, tagged. Both are drop-oldest so a slow reader can never
	// stall a loop.
	states       chan State
	outputStates chan OutputState

	// errs is output 0's error channel; the others are made per run. It is a
	// field so a test can push an error at the primary loop directly.
	errs chan error

	quit     chan struct{}
	loopDone chan struct{}
	stopErr  error

	// hook, when set (tests only), sees every output-0 transition before it
	// is queued.
	hook func(State)

	mu       sync.Mutex
	started  bool
	stopOnce sync.Once
}

func newSender(p gst.Pipeline, c clock) *senderImpl {
	return &senderImpl{
		p:            p,
		clock:        c,
		states:       make(chan State, statesBuffer),
		outputStates: make(chan OutputState, statesBuffer*2),
		errs:         make(chan error, errsBuffer),
		quit:         make(chan struct{}),
		loopDone:     make(chan struct{}),
	}
}

var _ Sender = (*senderImpl)(nil)

func (s *senderImpl) States() <-chan State { return s.states }

func (s *senderImpl) OutputStates() <-chan OutputState { return s.outputStates }

// Start builds and starts the pipeline, then runs the loops. A pipeline that
// will not start is an error from here and nothing is left running.
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

// Stop ends every loop and stops the pipeline. It returns once the pipeline
// has been stopped and the state channels closed. The one wait it cannot
// shorten is a ReplaceSinkOn already in flight: that call is synchronous by
// contract and the loop checks quit the moment it returns.
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

// fatal ends every loop from inside one of them, the way Stop does from
// outside: a fatal pipeline is fatal for every output.
func (s *senderImpl) fatal() {
	s.stopOnce.Do(func() { close(s.quit) })
}

func (s *senderImpl) run(opts Opts) {
	sinks := opts.sinks()
	if n := s.p.Outputs(); n > 0 && len(sinks) > n {
		log.Printf("sender: %d sinks configured but the pipeline has %d output(s); dialling the first %d",
			len(sinks), n, n)
		sinks = sinks[:n]
	}

	errCh := make([]chan error, len(sinks))
	for i := range errCh {
		if i == 0 {
			errCh[i] = s.errs
		} else {
			errCh[i] = make(chan error, errsBuffer)
		}
	}

	var watcher sync.WaitGroup
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		s.watchErrors(s.p.Errors(), errCh)
	}()

	var loops sync.WaitGroup
	for i := range sinks {
		loops.Add(1)
		go func(i int) {
			defer loops.Done()
			s.loop(i, sinks[i], opts, errCh[i])
		}(i)
	}
	loops.Wait()

	stopErr := s.p.Stop()
	if stopErr != nil {
		stopErr = fmt.Errorf("sender: stop pipeline: %w", stopErr)
	}
	for i := range sinks {
		s.emitOutput(i, StateStopped)
	}
	if len(sinks) == 0 {
		s.emitOutput(0, StateStopped)
	}
	close(s.states)
	close(s.outputStates)
	watcher.Wait()
	s.stopErr = stopErr
	close(s.loopDone)
}

// watchErrors routes the pipeline's asynchronous errors to the loop they
// concern: a gst.OutputError to its output, anything else to every output.
// Sends never block; a loop that is not reading loses the oldest, and one
// error is all a loop needs to know the connection has gone.
func (s *senderImpl) watchErrors(errs <-chan error, outs []chan error) {
	for {
		select {
		case <-s.quit:
			return
		case err, ok := <-errs:
			if !ok {
				return
			}
			if err == nil {
				err = fmt.Errorf("sender: pipeline reported an unspecified error")
			}
			var oe *gst.OutputError
			if errors.As(err, &oe) && oe.Output >= 0 && oe.Output < len(outs) {
				offer(outs[oe.Output], err)
				continue
			}
			for _, ch := range outs {
				offer(ch, err)
			}
		}
	}
}

func offer(ch chan error, err error) {
	select {
	case ch <- err:
	default:
	}
}

// loop is one output's reconnect machine. See the file header.
func (s *senderImpl) loop(i int, sink gst.SinkOpts, opts Opts, errs chan error) {
	report := func(err error) {
		if i == 0 && opts.OnConnectError != nil {
			opts.OnConnectError(err)
		}
		if opts.OnOutputConnectError != nil {
			opts.OnOutputConnectError(i, err)
		}
	}

	attempt := 0
	s.emitOutput(i, StateConnecting)
	for {
		if s.quitting() {
			return
		}
		// Anything queued before this attempt describes the PREVIOUS sink, not
		// the one about to be installed: a stale error read after a successful
		// swap would be a false peer loss on a connection that is fine.
		drain(errs)

		err := s.p.ReplaceSinkOn(i, sink)
		if s.quitting() {
			return
		}
		if err != nil && errors.Is(err, gst.ErrPipelineFatal) {
			s.reportConnectError(report, err, attempt+1, 0)
			s.fatal()
			return
		}
		if err == nil {
			// A fresh sink starts mid-GOP; ask the encoder for a key frame so
			// the listener decodes at once. Harmless on an audio-only pipeline.
			_ = s.p.ForceKeyUnit()
			attempt = 0
			s.emitOutput(i, StateConnected)
			select {
			case <-s.quit:
				return
			case <-errs:
			}
			s.emitOutput(i, StateDraining)
			if rmErr := s.p.RemoveSinkOn(i); rmErr != nil {
				log.Printf("sender: output %d: could not remove the failed sink: %v; backing off and retrying anyway",
					i, rmErr)
			}
			if s.quitting() {
				return
			}
		}

		d := backoffDelay(attempt)
		attempt++
		if err != nil {
			s.reportConnectError(report, err, attempt, d)
		}
		s.emitOutput(i, StateBackoff)
		if !s.sleep(d, errs) {
			return
		}
		s.emitOutput(i, StateConnecting)
	}
}

// reportConnectError logs one failed attempt and hands it to the caller's
// callback, which may panic without ending the loop.
func (s *senderImpl) reportConnectError(report func(error), err error, attempt int, d time.Duration) {
	if d > 0 {
		log.Printf("sender: connection attempt %d failed: %v; retrying in %s", attempt, err, d)
	} else {
		log.Printf("sender: connection attempt %d failed: %v", attempt, err)
	}
	if report == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("sender: OnConnectError panicked: %v; the reconnect loop carries on", r)
		}
	}()
	report(err)
}

// sleep waits d on the clock, draining this output's errors as they arrive —
// during BACKOFF there is no sink, so an error can only be stale — and
// returns false if quit fired first.
func (s *senderImpl) sleep(d time.Duration, errs chan error) bool {
	fire, stop := s.clock.NewTimer(d)
	defer stop()
	for {
		select {
		case <-s.quit:
			return false
		case <-fire:
			return true
		case <-errs:
		}
	}
}

func drain(errs chan error) {
	for {
		select {
		case <-errs:
		default:
			return
		}
	}
}

func (s *senderImpl) quitting() bool {
	select {
	case <-s.quit:
		return true
	default:
		return false
	}
}

// emitOutput queues one transition: on states for output 0, and on
// outputStates for every output. Both drop the oldest when full.
func (s *senderImpl) emitOutput(i int, st State) {
	if i == 0 {
		if s.hook != nil {
			s.hook(st)
		}
		for {
			select {
			case s.states <- st:
				goto tagged
			default:
				select {
				case <-s.states:
				default:
				}
			}
		}
	}
tagged:
	os := OutputState{Output: i, State: st}
	for {
		select {
		case s.outputStates <- os:
			return
		default:
			select {
			case <-s.outputStates:
			default:
			}
		}
	}
}

// emit is emitOutput for output 0, kept for the tests written against the
// single-output machine.
func (s *senderImpl) emit(st State) { s.emitOutput(0, st) }
