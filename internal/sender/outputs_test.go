package sender

import (
	"errors"
	"sync"
	"testing"
	"time"

	"wslcomms/internal/gst"
)

// Two outputs from one pipeline: two loops, two ladders, one lamp.

func twoOutputOpts() Opts {
	o := testOpts()
	o.Pipeline.SecondOutput = true
	second := o.Sink
	second.Port = o.Sink.Port + 1
	o.Sinks = []gst.SinkOpts{o.Sink, second}
	return o
}

// expectOutputState waits for the next transition of ONE output on
// OutputStates, skipping the other output's.
func expectOutputState(t *testing.T, ch <-chan OutputState, output int, want State) {
	t.Helper()
	deadline := time.After(testTimeout)
	for {
		select {
		case st, ok := <-ch:
			if !ok {
				t.Fatalf("OutputStates closed while waiting for output %d %s", output, want)
			}
			if st.Output != output {
				continue
			}
			if st.State != want {
				t.Fatalf("output %d: state = %s, want %s", output, st.State, want)
			}
			return
		case <-deadline:
			t.Fatalf("timed out waiting for output %d %s", output, want)
		}
	}
}

func TestTwoOutputsAreDialledAndBothConnect(t *testing.T) {
	p := newFakePipeline()
	s := newSender(p, newFakeClock())
	opts := twoOutputOpts()
	if err := s.Start(opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The primary's transitions are the lamp's; both outputs report on
	// OutputStates.
	expectStates(t, s.States(), StateConnecting, StateConnected)
	expectOutputState(t, s.OutputStates(), 1, StateConnecting)
	expectOutputState(t, s.OutputStates(), 1, StateConnected)

	if got := p.sinksOn(0); len(got) != 1 || got[0] != opts.Sinks[0] {
		t.Fatalf("output 0 was dialled with %+v, want %+v", got, opts.Sinks[0])
	}
	if got := p.sinksOn(1); len(got) != 1 || got[0] != opts.Sinks[1] {
		t.Fatalf("output 1 was dialled with %+v, want %+v", got, opts.Sinks[1])
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	expectState(t, s.States(), StateStopped)
	expectClosed(t, s.States())
	if got := p.snapshot(); got.starts != 1 || got.stops != 1 {
		t.Fatalf("counts = %+v, want one Start and one Stop for two outputs", got)
	}
}

func TestASecondOutputPeerLossLeavesThePrimaryConnected(t *testing.T) {
	// The whole point of two loops: the second listener going away restarts
	// the second output alone. The lamp stays CONNECTED and output 0's sink is
	// never touched.
	p, clk, log := newRecorded()
	s := newSender(p, clk)
	if err := s.Start(twoOutputOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)
	expectOutputState(t, s.OutputStates(), 1, StateConnecting)
	expectOutputState(t, s.OutputStates(), 1, StateConnected)

	p.mustInjectError(t, &gst.OutputError{Output: 1, Err: errPeerGone})
	expectOutputState(t, s.OutputStates(), 1, StateDraining)
	expectOutputState(t, s.OutputStates(), 1, StateBackoff)
	// The ladder's first wait, then the reconnect.
	clk.next(t).fire()
	expectOutputState(t, s.OutputStates(), 1, StateConnecting)
	expectOutputState(t, s.OutputStates(), 1, StateConnected)

	// Nothing about the primary moved: no state, no removal, no second dial.
	select {
	case st := <-s.States():
		t.Fatalf("the primary transitioned to %s over the second output's peer loss", st)
	default:
	}
	if got := p.sinksOn(0); len(got) != 1 {
		t.Fatalf("output 0 was dialled %d times, want 1: the second output's loss redialled the primary", len(got))
	}
	if got := p.sinksOn(1); len(got) != 2 {
		t.Fatalf("output 1 was dialled %d times, want 2", len(got))
	}
	// And the removal was the second output's.
	for _, ev := range log.all() {
		if ev == "removeSink" {
			t.Fatal("output 0's sink was removed for output 1's peer loss")
		}
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestAnUnattributedErrorRestartsEveryOutput(t *testing.T) {
	// An error that names no output — the muxer, an encoder — is every
	// output's problem: both loops drain and reconnect.
	p, clk, _ := newRecorded()
	s := newSender(p, clk)
	if err := s.Start(twoOutputOpts()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)
	expectOutputState(t, s.OutputStates(), 1, StateConnecting)
	expectOutputState(t, s.OutputStates(), 1, StateConnected)

	p.mustInjectError(t, errPeerGone)
	expectStates(t, s.States(), StateDraining, StateBackoff)
	expectOutputState(t, s.OutputStates(), 1, StateDraining)
	expectOutputState(t, s.OutputStates(), 1, StateBackoff)
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestASecondOutputThatWillNotConnectWalksItsOwnLadder(t *testing.T) {
	// The second listener refuses; the primary connects at once. The second
	// output's failures reach OnOutputConnectError with its index, and never
	// OnConnectError, which is the primary's.
	p, clk, _ := newRecorded()
	s := newSender(p, clk)
	opts := twoOutputOpts()
	p.queueSinkResultsOn(1, errConnectRefused, errConnectRefused)

	var mu sync.Mutex
	var primary []error
	var byOutput []int
	opts.OnConnectError = func(err error) {
		mu.Lock()
		primary = append(primary, err)
		mu.Unlock()
	}
	opts.OnOutputConnectError = func(output int, err error) {
		mu.Lock()
		byOutput = append(byOutput, output)
		mu.Unlock()
	}
	if err := s.Start(opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)
	expectOutputState(t, s.OutputStates(), 1, StateConnecting)
	expectOutputState(t, s.OutputStates(), 1, StateBackoff)
	clk.next(t).fire()
	expectOutputState(t, s.OutputStates(), 1, StateConnecting)
	expectOutputState(t, s.OutputStates(), 1, StateBackoff)
	clk.next(t).fire()
	expectOutputState(t, s.OutputStates(), 1, StateConnecting)
	expectOutputState(t, s.OutputStates(), 1, StateConnected)

	mu.Lock()
	defer mu.Unlock()
	if len(primary) != 0 {
		t.Fatalf("OnConnectError was called %d times for the second output's failures; it is the primary's", len(primary))
	}
	if len(byOutput) != 2 || byOutput[0] != 1 || byOutput[1] != 1 {
		t.Fatalf("OnOutputConnectError saw outputs %v, want [1 1]", byOutput)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestAFatalPipelineStopsBothOutputs(t *testing.T) {
	// ErrPipelineFatal from either output's ReplaceSinkOn ends the sender:
	// the other loop is told to quit, the pipeline is stopped once, and
	// STOPPED reaches both outputs.
	//
	// The second output is refused once first, so its fatal attempt waits on
	// a timer this test fires — after the primary has been seen CONNECTED.
	// Without that the two loops' first attempts race, and the primary can
	// read quit before it has emitted CONNECTED.
	p, clk, _ := newRecorded()
	s := newSender(p, clk)
	opts := twoOutputOpts()
	fatal := errors.Join(gst.ErrPipelineFatal, errors.New("the encoder failed"))
	p.queueSinkResultsOn(1, errors.New("refused"), fatal)
	if err := s.Start(opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)
	expectOutputState(t, s.OutputStates(), 1, StateConnecting)
	expectOutputState(t, s.OutputStates(), 1, StateBackoff)
	clk.next(t).fire()
	// Output 1's second attempt is fatal; the whole sender stops.
	expectState(t, s.States(), StateStopped)
	expectClosed(t, s.States())
	if got := p.snapshot(); got.stops != 1 {
		t.Fatalf("stops = %d, want exactly 1", got.stops)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop after a fatal stop: %v", err)
	}
}

func TestMoreSinksThanOutputsDialsWhatThePipelineHas(t *testing.T) {
	// A misconfiguration — two sinks for a single-output pipeline — dials the
	// first and logs the rest, rather than asking for a slot that is not there
	// on every attempt for ever.
	p := newFakePipeline()
	s := newSender(p, newFakeClock())
	opts := twoOutputOpts()
	opts.Pipeline.SecondOutput = false
	if err := s.Start(opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expectStates(t, s.States(), StateConnecting, StateConnected)
	if got := p.sinksOn(1); len(got) != 0 {
		t.Fatalf("output 1 was dialled %d times on a single-output pipeline", len(got))
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
