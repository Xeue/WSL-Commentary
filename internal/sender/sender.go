// Package sender owns section 6 of the specification in full: monotonic
// timestamps across a pipeline restart, and the SRT reconnect state machine.
//
// Owner: WP-3b. No other work package writes files in this directory.
//
// It is pure Go. It never imports cgo and never mentions GStreamer: it drives a
// gst.Pipeline handed to it by New, which in a Gate A build is the pure-Go stub
// and in a Gate B build is the real thing. That is what makes the reconnect
// logic — the part most likely to cause an outage during a match — testable
// today against a fake.
//
// # The state machine
//
//	CONNECTING --connect ok--> CONNECTED --error on srtout--> DRAINING --> BACKOFF --> CONNECTING
//	     '--connect fails------------------------------------------------> BACKOFF
//
// DRAINING blocks the src pad of the leaky srtq queue, unlinks, and removes the
// sink; everything upstream stays in PLAYING. BACKOFF sleeps on a goroutine —
// never inside a pad probe or a bus callback. CONNECTING installs a fresh sink
// and, on success, forces a key unit so the picture recovers at once.
//
// The application must reconnect indefinitely on anything a retry could fix:
// on total network loss libsrt declares the peer dead at about 5.27 s and
// exits, and M2L-X never recovers by itself. The one exception is a connect
// failure wrapping gst.ErrPipelineFatal — the capture or mux chain is dead and
// the condition latches, so every retry fails identically forever. The sender
// stops instead: StateStopped is the documented recovery, Stop then New then
// Start, being asked of the operator, where an eternal amber BACKOFF was a
// local device fault blamed on the network.
package sender

import (
	"errors"
	"time"

	"wslcomms/internal/gst"
)

// State is the sender's connection state. It is the source of the SENDING lamp,
// and is emitted to the frontend on the Wails "sender" event, so these string
// values are part of the contract with WP-5b.
type State string

const (
	// StateConnecting means a sink is being installed: the SRT caller handshake
	// is in progress. The lamp is amber.
	StateConnecting State = "CONNECTING"

	// StateConnected means the handshake succeeded and media is flowing. This is
	// the only good state; the lamp is green.
	StateConnected State = "CONNECTED"

	// StateDraining means the sink has failed and is being torn out. Everything
	// upstream of the leaky queue stays in PLAYING. The lamp is amber.
	StateDraining State = "DRAINING"

	// StateBackoff means the sender is waiting before the next attempt. The lamp
	// is amber.
	StateBackoff State = "BACKOFF"

	// StateStopped means the sender is not running: Start has not been called,
	// Stop has, or a connection attempt returned an error wrapping
	// gst.ErrPipelineFatal and the sender stopped itself, because only Stop,
	// New, Start recovers a latched pipeline. The lamp is grey.
	StateStopped State = "STOPPED"
)

// BackoffLadder is the reconnect delay sequence from specification section 6.2,
// followed by BackoffCap repeated forever.
//
// The first delay must stay at or above six seconds: it has to clear M2L-X's
// re-accept refusal window, and the same ladder is what eventually wins the
// one-peer race against our own stale socket, because an SRT listener accepts
// exactly one peer and never displaces the incumbent.
var BackoffLadder = []time.Duration{
	7 * time.Second,
	7 * time.Second,
	10 * time.Second,
	15 * time.Second,
	20 * time.Second,
}

// BackoffCap is the delay used for every attempt after the ladder is exhausted.
// There is no attempt limit for retryable failures: the sender retries forever
// until Stop. The single failure that is not retried at all — not capped, not
// counted, stopped — is a ReplaceSink error wrapping gst.ErrPipelineFatal,
// which latches for the life of the pipeline and so would fail identically on
// every rung; see the loop in machine.go.
const BackoffCap = 30 * time.Second

// ErrAlreadyStarted is returned by Start on a Sender that is already running.
var ErrAlreadyStarted = errors.New("sender: already started")

// ErrNotStarted is returned by Stop on a Sender that is not running.
var ErrNotStarted = errors.New("sender: not started")

// Opts is everything the sender needs for one session. It is composed from the
// two gst option structs rather than restating their fields, so that there is no
// field-by-field translation for anyone to get wrong.
type Opts struct {
	// Pipeline configures the capture, encode and mux chain, which is built once
	// by Start and never rebuilt for the life of the session.
	Pipeline gst.PipelineOpts

	// Sink configures the srtsink, which is rebuilt on every reconnect.
	Sink gst.SinkOpts

	// OnConnectError, if set, is called with the reason every failed connection
	// attempt failed — immediately before the transition to StateBackoff for a
	// retryable failure, and exactly once, immediately before the sender stops
	// itself, for an error wrapping gst.ErrPipelineFatal.
	//
	// Retrying forever remains the requirement for every failure a retry could
	// fix, and this does not change it. What it changes is that the reason
	// stops being discarded. The case that motivated it is a pipeline gone
	// permanently fatal — the operator selecting a playback endpoint as the
	// commentary input, or the commentator's Dante endpoint unplugged, either
	// of which errors wasapi2src asynchronously and latches internal/gst's
	// fatal state so that every subsequent ReplaceSink returns the same
	// failure instantly. The sender stops on that error rather than backing
	// off: this callback carries the real reason — WP-8 forwards it to the
	// frontend's "error" event — and then the lamp goes grey STOPPED, which is
	// the documented recovery (Stop, New, Start) being asked of the operator.
	// Before either change the operator saw an amber SENDING lamp saying
	// "connecting" for the rest of the match, while the one message that could
	// have said "your audio device is gone" blamed the network instead.
	//
	// It is called on the state-machine goroutine, so it must not block and must
	// not call back into the Sender. A nil value disables reporting.
	//
	// Added after WP-0 by the coordinator, on the adversarial review of
	// internal/sender finding 3.
	OnConnectError func(error)
}

// Sender runs a media session and keeps it connected.
type Sender interface {
	// Start builds and plays the pipeline, then enters the state machine at
	// StateConnecting. It returns as soon as the pipeline is playing; a
	// connection failure is not an error from Start, it is a transition to
	// StateBackoff — the sender retries indefinitely — or, for an error
	// wrapping gst.ErrPipelineFatal, a stop to StateStopped, because no
	// reconnect can repair a latched pipeline.
	//
	// Start returns ErrAlreadyStarted if the sender is already running.
	Start(opts Opts) error

	// Stop tears the session down: it stops the reconnect loop, stops the
	// pipeline, emits StateStopped and closes the channel returned by States.
	// A stopped Sender cannot be restarted; call New again.
	//
	// Stop blocks until all of that is done, and it must not be called from a UI
	// thread. Its worst case is not set by this package: a gst.Pipeline.
	// ReplaceSink already in flight is synchronous by contract and cannot be
	// cancelled, so Stop waits for it, and internal/gst bounds that at its
	// sinkStateChangeTimeout of ten seconds plus whatever the SRT caller
	// handshake has already spent. Everything this package does contribute is
	// prompt — the backoff wait is cancelled rather than served, which is what
	// the injected clock exists for. A measured successful handshake is about
	// 1.1 s (specification section 6); ten seconds is the bound that only a
	// hung state change reaches.
	//
	// Stop returns ErrNotStarted if the sender was never started.
	Stop() error

	// States returns the stream of state transitions, in order and without
	// duplicates. It is buffered so that a slow consumer cannot stall the
	// reconnect loop; a consumer that falls too far behind loses intermediate
	// states but always eventually sees the current one.
	//
	// The channel is closed by Stop, after StateStopped has been sent.
	States() <-chan State
}

// New returns a Sender that drives p.
//
// The pipeline is injected rather than created here so that this package can be
// unit-tested against a fake gst.Pipeline: WP-3b writes that fake in its own
// test files.
//
// Ownership transfers only on a successful Start. From then on the Sender drives
// p and calls p.Stop when it stops, and the caller must not touch p itself. If
// Start returns an error the pipeline is left exactly as it was found — this
// package never calls p.Stop on that path — and since a gst.Pipeline is
// single-use, the caller must stop it or leak the capture device and the
// goroutine reading its Errors channel. app.go does precisely that.
func New(p gst.Pipeline) Sender {
	return newSender(p, realClock{})
}

// backoffDelay returns the delay before reconnect attempt number attempt,
// counting the attempt that has just failed as attempt zero. It walks
// BackoffLadder and then returns BackoffCap for every attempt beyond it, so the
// sequence is 7, 7, 10, 15, 20, 30, 30, 30, ... seconds with no upper bound on
// the number of attempts.
//
// A negative attempt cannot arise from this package's own bookkeeping, but is
// clamped to the first rung rather than panicking: during a match an
// over-long wait is survivable and a panic is not.
func backoffDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt < len(BackoffLadder) {
		return BackoffLadder[attempt]
	}
	return BackoffCap
}
