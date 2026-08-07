// return.go is the SRT RETURN path: the commentator's off-air monitor.
//
// Owner: WP-R. New file. It does not touch gst.go, gst_cgo.go, gst_stub.go or
// gst_stub_guard.go, which are the contribution path and are frozen.
//
// # Why this exists at all, and why it is not the WebRTC return
//
// The operator needed effects and comms mixed on PGM but hard-panned left and
// right on CLN. M2L-X's pan is per INPUT STRIP, not per bus — pan_balance lives
// at state.effect.<strip> and there is nothing pan-like on state.outputs — so a
// strip's pan applies to every bus that strip feeds. The solution was double
// router inputs: each source arrives twice, one copy panned centre and routed to
// master, the other panned hard L or R and routed to aux1.
//
// The consequence is that the CLN bus now carries FX hard-left and comms
// hard-right. To monitor just the effects, the commentator has to be able to
// pick ONE CHANNEL, not just one bus. The existing WebRTC return picks a
// transceiver mid — a whole bus — and the browser has no per-channel selection
// worth relying on. This path picks a channel.
//
// # The pipeline
//
//	srtsrc uri=srt://<host>:<port> mode=caller latency=<ms>
//	  ! tsdemux
//	  ! aacparse ! mfaacdec
//	  ! audioconvert mix-matrix=<see MixMatrix>
//	  ! audioresample
//	  ! wasapi2sink device=<IMMDevice endpoint id>
//
// Audio only. The picture on Output 3 is H.265 and there is no HEVC decoder on
// the target machine — mfh265dec is missing because the Windows HEVC extension
// is not installed — and adding one is a separate, larger decision the operator
// has not taken. The video pad is demuxed and thrown away; see the pad-added
// handler in return_cgo.go for why throwing it away requires an element rather
// than being free.
//
// # The one rule this file exists to keep
//
// A RETURN MONITOR FAILING MUST NEVER DISTURB THE CONTRIBUTION FEED.
//
// That is why this is a separate type with a separate pipeline, a separate bus
// handler and a separate reconnect machine, rather than a second sink on the
// send pipeline or a second caller of internal/sender. Coupling them through
// shared code is precisely how a headphone monitor takes a live match off air:
// one shared mutex held across a wedged WASAPI endpoint, one shared error
// channel, one shared state machine that cannot tell which half failed. There
// is deliberately no code path from this file into the send pipeline, and none
// back.
//
// internal/sender could not have been reused even if that were wise: it imports
// internal/gst, so this package importing it would be an import cycle. The
// reconnect machine below therefore restates sender's discipline rather than
// citing it, and the places where the two must agree — the shape of the backoff
// ladder, and why it may never give up — say so.
//
// # Where the logic lives
//
// This file carries no build tag and no cgo. Everything that can be tested
// without GStreamer is here: the mix matrix, the option validation, the backoff
// ladder and the whole reconnect state machine. The two build-tagged twins,
// return_cgo.go and return_stub.go, supply exactly one thing between them —
// newReturnPipe, which builds one attempt's pipeline — plus ListOutputDevices.
// That is the same seam gst.go draws, for the same reason: Gate A must be able
// to build and test the application on a machine with no MinGW and no
// GStreamer.
package gst

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultReturnLatencyMs is srtsrc's latency property, in MILLISECONDS. It
// matches DefaultSRTLatencyMs on the send path and for the same reason: roughly
// five times the measured 21 ms median round-trip time to the M2L-X instance.
//
// It is a separate constant rather than a reference to the send path's because
// the two are free to diverge. The send path's latency buys resilience for a
// contribution feed that must not glitch; this one buys it for a monitor, where
// a commentator would rather have 120 ms of delay than a dropout, but where
// somebody may one day decide that 120 ms of extra delay in the ear is worse
// than the dropout. Making that a one-line change here rather than a shared
// constant is the point.
const DefaultReturnLatencyMs = 120

// returnAudioPadTimeout is how long Play waits, after the pipeline reaches
// PLAYING, for the demuxer to produce and link an audio pad before it declares
// the attempt failed.
//
// # This is not a belt-and-braces timeout. It is the only thing that makes Play
// # honest, and the reason is measured
//
// srtsrc reaching PLAYING proves NOTHING. Dialled at the live instance on
// 2026-08-07 against an output that was configured but not started, the pipeline
// reported "Pipeline is live and does not need PREROLL", then "Setting pipeline
// to PLAYING", and only 3.011 s later:
//
//	ERROR: from element .../GstSRTSrc:retsrc: Could not read from resource.
//	../ext/srt/gstsrtsrc.c(206): gst_srt_src_fill ():
//	Error on SRT socket: Connection timeout (16)
//
// The connect happens inside gst_srt_src_fill — on the streaming thread, after
// the state change has already returned success — and libsrt's default
// SRTO_CONNTIMEO of 3 s is what bounds it. This is the OPPOSITE of srtsink on
// the send path, which connects inside its READY to PAUSED transition and
// therefore reports failure synchronously.
//
// So if Play returned as soon as the pipeline was PLAYING, it would report
// success on a socket that does not exist, the machine would emit RECEIVING, and
// the RETURN lamp would go green for three seconds out of every reconnect
// attempt against a dead endpoint. Waiting here instead means a nil error from
// Play means what it says: audio is flowing.
//
// Even a connected socket is not enough on its own — the audio pad only appears
// once tsdemux has seen a PMT and found an AAC stream in it — so this waits for
// the pad rather than for the socket, and covers both facts with one wait.
//
// Ten seconds is far beyond a working stream: libsrt gives up on the connect at
// 3 s, an MPEG-TS PMT repeats several times a second, and the measured SRT lock
// to this instance is about 1.1 s.
const returnAudioPadTimeout = 10 * time.Second

// ReturnBackoffLadder is the reconnect delay sequence for the return monitor,
// followed by ReturnBackoffCap repeated forever.
//
// It is the same shape as internal/sender's ladder, and the first rung is at or
// above six seconds for the same measured reason: M2L-X's SRT listener accepts
// exactly one peer, never displaces the incumbent, and refuses re-accept for
// roughly five seconds after the incumbent goes away. We are the caller on this
// path too — every M2L-X output has reverse=True, so M2L-X listens and we dial —
// so when our own socket was the incumbent, a retry inside that window is
// refused and the attempt is wasted. The wait only buys anything if our socket
// is already gone when it starts, which is why the machine below closes the
// pipeline BEFORE it sleeps and not after.
//
// It is duplicated rather than imported. internal/sender imports internal/gst,
// so importing it back would be a cycle; and beyond that, a monitor and a
// contribution feed have different tolerances and should be free to disagree
// about how hard to retry. If one of the two ladders is ever changed, changing
// the other is a decision, not a chore.
var ReturnBackoffLadder = []time.Duration{
	7 * time.Second,
	7 * time.Second,
	10 * time.Second,
	15 * time.Second,
	20 * time.Second,
}

// ReturnBackoffCap is the delay used for every attempt after the ladder is
// exhausted. There is no attempt limit: the monitor retries until Stop.
//
// A return that dies silently mid-match is a commentator with no reference —
// they cannot hear the effects they are commentating over, and unlike the
// contribution feed nothing downstream will tell anyone. Giving up after N
// attempts would mean a monitor that is dead for the rest of the match because
// of a thirty second network blip in the first half.
const ReturnBackoffCap = 30 * time.Second

// ReturnChannel is which channel of the stereo return reaches the headphones.
//
// The values are the ones persisted in config.json as returnChannel and are
// therefore part of the on-disk contract; they are lower case for the same
// reason every other config enum is.
type ReturnChannel string

const (
	// ReturnChannelStereo passes the return through unchanged: left to left,
	// right to right. This is the default and is what a commentator wants when
	// the CLN bus is a normal stereo mix.
	ReturnChannelStereo ReturnChannel = "stereo"

	// ReturnChannelLeft puts the LEFT source channel into BOTH ears. On the
	// operator's current routing that is the effects feed.
	//
	// Both ears, not one. Silencing an ear would be the literal reading of "left
	// only" and it is the wrong one: a commentator wearing headphones with one
	// dead side spends the match assuming the headphones are broken, and
	// monitoring in mono is what they actually asked for.
	ReturnChannelLeft ReturnChannel = "left"

	// ReturnChannelRight puts the RIGHT source channel into both ears. On the
	// operator's current routing that is the comms feed.
	ReturnChannelRight ReturnChannel = "right"
)

// DefaultReturnChannel is the channel mode assumed when none is configured.
const DefaultReturnChannel = ReturnChannelStereo

// Valid reports whether ch is one of the three defined modes. An empty value is
// NOT valid here; callers that want "unset means stereo" apply that themselves,
// so that a typo in config.json is reported rather than silently defaulted.
func (ch ReturnChannel) Valid() bool {
	switch ch {
	case ReturnChannelStereo, ReturnChannelLeft, ReturnChannelRight:
		return true
	}
	return false
}

// ReturnState is the return monitor's state. It is the source of the RETURN
// lamp and is emitted to the frontend, so these string values are contract.
type ReturnState string

const (
	// ReturnStateStopped is the state before Start and after Stop.
	ReturnStateStopped ReturnState = "STOPPED"

	// ReturnStateConnecting means a pipeline is being built and the SRT caller
	// handshake is in progress.
	ReturnStateConnecting ReturnState = "CONNECTING"

	// ReturnStateReceiving means the handshake succeeded AND the demuxer
	// produced an audio pad that is linked to the headphone branch. It is not
	// emitted merely because the pipeline reached PLAYING, which on this path
	// proves nothing at all — see returnAudioPadTimeout.
	ReturnStateReceiving ReturnState = "RECEIVING"

	// ReturnStateBackoff means the monitor is waiting before the next attempt.
	ReturnStateBackoff ReturnState = "BACKOFF"
)

// There are deliberately only four states, where the send path has five.
//
// internal/sender needs a DRAINING state because tearing the old sink out is a
// step the operator has to be able to see: it happens BEFORE the backoff wait,
// it is what makes the wait clear M2L-X's re-accept window, and getting that
// order wrong was a real bug. Here the whole pipeline is rebuilt per attempt, so
// the teardown is not a separable step that could be got out of order — it is
// the first thing the failure path does, unconditionally, with nothing between
// it and the sleep. A state for it would be a state that is always true for a
// few microseconds and tells nobody anything.

// ErrReturnAlreadyStarted is returned by Start on a monitor that is running.
var ErrReturnAlreadyStarted = errors.New("gst: return monitor already started")

// ErrReturnNotStarted is returned by Stop on a monitor that was never started.
var ErrReturnNotStarted = errors.New("gst: return monitor not started")

// ReturnOpts is everything one return session needs.
type ReturnOpts struct {
	// Host is the M2L-X SRT listener's hostname or address. It follows the
	// M2L-X host through config.EffectiveSRTHost, exactly as the send path does;
	// there is deliberately no second host field anywhere for this path.
	Host string

	// Port is that listener's port. On the measured instance the monitoring
	// output is Output 3, src=cln, port 40503 — the bus that carries FX
	// hard-left and comms hard-right.
	Port int

	// LatencyMs is srtsrc's latency property in MILLISECONDS. Zero means
	// DefaultReturnLatencyMs.
	LatencyMs int

	// Passphrase is the SRT passphrase from Credential Manager. It is set with
	// g_object_set, never placed in the URI, and never logged. Empty means an
	// unencrypted session.
	Passphrase string

	// PBKeyLen is the SRT key length, 16 or 32. Ignored when Passphrase is
	// empty.
	PBKeyLen int

	// Channel is which channel of the return reaches the headphones. Empty
	// means DefaultReturnChannel.
	Channel ReturnChannel

	// OutputDeviceID is the WASAPI IMMDevice endpoint ID passed to
	// wasapi2sink's device property.
	//
	// It is NOT the browser mediaDeviceId the WebRTC return path uses. They are
	// two different identifiers for the same pair of headphones and are not
	// interchangeable in either direction: mediaDeviceId is a per-origin salted
	// hash that means nothing outside the WebView, and the IMMDevice endpoint ID
	// means nothing to setSinkId. Config keeps both, under two names, for
	// exactly this reason. Empty means wasapi2sink's default endpoint, which is
	// whatever Windows currently calls the default playback device.
	OutputDeviceID string
}

// normalise fills in the documented defaults and reports any option that cannot
// work, as a single joined error so the operator sees every problem at once.
func (o *ReturnOpts) normalise() error {
	var errs []error

	if strings.TrimSpace(o.Host) == "" {
		errs = append(errs, errors.New("gst: ReturnOpts.Host is required"))
	}
	if o.Port < 1 || o.Port > 65535 {
		errs = append(errs, fmt.Errorf("gst: ReturnOpts.Port must be between 1 and 65535, got %d", o.Port))
	}
	if o.LatencyMs == 0 {
		o.LatencyMs = DefaultReturnLatencyMs
	}
	if o.LatencyMs < 0 {
		errs = append(errs, fmt.Errorf("gst: ReturnOpts.LatencyMs %d is negative", o.LatencyMs))
	}
	if o.Channel == "" {
		o.Channel = DefaultReturnChannel
	}
	if !o.Channel.Valid() {
		errs = append(errs, fmt.Errorf(
			"gst: ReturnOpts.Channel %q is not one of %q, %q, %q",
			o.Channel, ReturnChannelStereo, ReturnChannelLeft, ReturnChannelRight))
	}
	if o.Passphrase != "" {
		if o.PBKeyLen == 0 {
			o.PBKeyLen = 16
		}
		if o.PBKeyLen != 16 && o.PBKeyLen != 32 {
			errs = append(errs, fmt.Errorf("gst: ReturnOpts.PBKeyLen must be 16 or 32, got %d", o.PBKeyLen))
		}
	}

	return errors.Join(errs...)
}

// endpointForLog renders the return endpoint for a log line.
//
// It exists so that no code path in this package is ever tempted to format
// ReturnOpts as a whole with %+v, which would put the SRT passphrase in the
// field log and then in whatever support bundle the log is mailed in. Nothing
// below formats the struct; everything formats this.
func (o ReturnOpts) endpointForLog() string {
	host := o.Host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	enc := "none"
	if o.Passphrase != "" {
		enc = "aes-" + strconv.Itoa(o.PBKeyLen*8)
	}
	return fmt.Sprintf("srt://%s:%d (latency %d ms, channel %s, encryption %s)",
		host, o.Port, o.LatencyMs, o.Channel, enc)
}

// ---------------------------------------------------------------------------
// The mix matrix
// ---------------------------------------------------------------------------

// MixMatrix returns audioconvert's mix-matrix for one channel mode.
//
// # Shape
//
// The matrix is ROWS = OUTPUT channels, COLUMNS = INPUT channels. Getting it the
// wrong way round produces a matrix that is square, negotiates fine, and swaps
// the ears, so it was measured rather than read: on GStreamer 1.28.5,
//
//	audiotestsrc ! audio/x-raw,channels=2 ! audioconvert mix-matrix="<<(float)1,(float)0>>"
//
// negotiates channels=1 on audioconvert's src pad. One row in, one output
// channel out. Rows are outputs.
//
// Every matrix here is 2x2: two output channels because the destination is a
// pair of headphones, and two input channels because the return is stereo.
//
// # Measured behaviour, GStreamer 1.28.5, 2026-08-07
//
// Fed a stereo signal carrying a 1 kHz tone in the LEFT channel and digital
// silence in the right, and metered per channel with the level element:
//
//	source                             L -4.95 dB   R -700 dB
//	stereo  <<1,0>,<0,1>>   ->  out    L -4.95 dB   R -700 dB
//	left    <<1,0>,<1,0>>   ->  out    L -4.95 dB   R -4.95 dB
//	right   <<0,1>,<0,1>>   ->  out    L -700 dB    R -700 dB
//
// That is the requirement, demonstrated rather than asserted: "left" puts the
// left source channel into BOTH ears at full level, and "right" selects the
// other channel — which in this test signal was silent, which is what selecting
// it should sound like.
//
// # Why the stereo input count is an assumption this file is allowed to make
//
// The channel count of an AAC stream is in the ADTS header, not in the PMT, so
// it is not knowable at the moment the pipeline is built — the demuxer's audio
// pad carries audio/mpeg,mpegversion=4 with no channels field. The alternative
// to assuming is an extra element or a caps probe that would have to reconfigure
// a running audioconvert from a streaming thread.
//
// The assumption is safe because of what the return IS. Left/right selection is
// only meaningful at all because the CLN bus carries FX hard-left and comms
// hard-right; a mono CLN could not carry that hard-panned pair, so a return that
// is not stereo is a return on which this entire feature has no meaning. And it
// fails loudly rather than quietly: audioconvert derives its sink-pad channel
// count from the matrix width, so a mono decoder output would fail caps
// negotiation, post GST_ELEMENT_ERROR, and reach the operator through the state
// machine below as a failed attempt with the reason attached.
//
// # Why a matrix rather than deinterleave
//
// The interleave plugin is not in the bundle allowlist and adding a plugin is a
// specification change. One property on an element that is in the pipeline
// anyway beats two extra elements and a new bundle entry.
func MixMatrix(ch ReturnChannel) ([][]float32, error) {
	switch ch {
	case ReturnChannelStereo:
		// Identity. Left to left, right to right.
		//
		// The identity is set explicitly rather than leaving the property unset,
		// so that all three modes are one code path with one shape, and so that
		// the output channel count is pinned at two in every mode rather than in
		// two of the three. An unset mix-matrix would also work for this case;
		// it would just make stereo the odd one out in every test and every
		// future edit.
		return [][]float32{
			{1, 0},
			{0, 1},
		}, nil
	case ReturnChannelLeft:
		// Source channel 0 into BOTH output channels. Column 1 is zero in both
		// rows, so the right source channel — comms, on the current routing — is
		// discarded rather than attenuated.
		return [][]float32{
			{1, 0},
			{1, 0},
		}, nil
	case ReturnChannelRight:
		// Source channel 1 into both output channels.
		return [][]float32{
			{0, 1},
			{0, 1},
		}, nil
	}
	return nil, fmt.Errorf("gst: MixMatrix: unknown channel mode %q", ch)
}

// MixMatrixString serialises a mix matrix in the GstValueArray syntax that
// gst_util_set_object_arg deserialises, e.g.
//
//	<<(float)1,(float)0>,<(float)1,(float)0>>
//
// The (float) tags are the canonical form: mix-matrix is a GST_TYPE_ARRAY of
// GST_TYPE_ARRAY of gfloat, and the tag states the element type rather than
// leaving it to be inferred.
//
// Whether the tag is strictly REQUIRED was tested rather than assumed, and the
// answer on GStreamer 1.28.5 is no — an untagged "<<1,0>>" deserialises to the
// same matrix, and negotiates identically. The tag is written anyway, for two
// reasons that survive that result. It is what GStreamer's own documentation and
// examples use, so it is the form most likely to keep working. And the failure
// mode if a future deserialiser ever does reject a form is SILENT:
// gst_util_set_object_arg returns void and simply does nothing with a value it
// cannot parse, leaving audioconvert's default mapping in place — stereo when
// the operator asked for left, with no error anywhere. Depending on type
// inference for a property whose misparse is undetectable is a bad trade for
// eight characters.
//
// A nil or empty matrix serialises to the empty string, which callers treat as
// "leave the property alone".
func MixMatrixString(m [][]float32) string {
	if len(m) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('<')
	for i, row := range m {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('<')
		for j, v := range row {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString("(float)")
			// 32-bit precision: the property is gfloat, and printing a float32
			// through the 64-bit formatter turns 0.1 into 0.10000000149011612.
			b.WriteString(strconv.FormatFloat(float64(v), 'g', -1, 32))
		}
		b.WriteByte('>')
	}
	b.WriteByte('>')
	return b.String()
}

// ---------------------------------------------------------------------------
// The contract
// ---------------------------------------------------------------------------

// ReturnMonitor receives the SRT return and plays it to the headphones, staying
// connected for as long as it is running.
//
// A ReturnMonitor is single-use: after Stop it cannot be restarted, because its
// state channel is closed. Call NewReturnMonitor again. All methods are safe for
// concurrent use.
type ReturnMonitor interface {
	// Start validates opts and launches the reconnect loop. It returns as soon
	// as the loop is running, NOT when audio is flowing.
	//
	// That is deliberate and is the one place this contract differs from
	// internal/sender's. A failure to CONNECT is not an error from Start,
	// because the monitor retries indefinitely and because the commentator may
	// well press the button before the M2L-X output has been enabled. A
	// configuration that cannot ever work — no host, a port out of range, a
	// channel mode that is not one of the three — IS an error from Start, and it
	// names the field, because that one will not fix itself and the operator is
	// standing in front of the settings screen.
	//
	// It returns ErrReturnAlreadyStarted if the monitor is already running or
	// has been stopped.
	Start(opts ReturnOpts) error

	// Stop tears the session down: it ends the reconnect loop, takes the
	// pipeline to NULL, releases the headphone endpoint, emits
	// ReturnStateStopped and closes the channel returned by States.
	//
	// Stop blocks until all of that is done and must not be called from a UI
	// thread. Its worst case is a build-and-connect attempt already in flight:
	// that is synchronous inside GStreamer and cannot be cancelled, so Stop
	// waits for it. Everything this file contributes is prompt — the backoff
	// wait is cancelled rather than served.
	//
	// It returns ErrReturnNotStarted if the monitor was never started.
	Stop() error

	// States returns the stream of state transitions. It is buffered so that a
	// slow consumer cannot stall the reconnect loop; a consumer that falls too
	// far behind loses intermediate states but always eventually sees the
	// current one.
	//
	// It is valid before Start, so a caller can begin consuming and then start.
	// The channel is closed by Stop, after ReturnStateStopped has been sent.
	States() <-chan ReturnState
}

// returnPipe is one attempt's pipeline: the seam between this pure-Go file and
// the two build-tagged twins.
//
// It is deliberately smaller than gst.Pipeline. The send path's interface has
// ReplaceSink and RemoveSink because everything upstream of its sink must stay
// in PLAYING for the life of the process — a live capture whose running time
// must never move backwards. NOTHING here has that property. There is no
// capture, no encoder and no muxer; the source of timestamps is the far end, and
// a fresh srtsrc simply starts a new stream. So an attempt is a whole pipeline,
// built and destroyed, and the sink-swapping machinery that the contribution
// path cannot do without would here be complexity with nothing to buy.
type returnPipe interface {
	// Play builds the pipeline, takes it to PLAYING and waits for audio to
	// actually flow. It returns synchronously, and a nil error means the SRT
	// caller handshake succeeded AND the demuxer produced an audio pad that is
	// linked to the headphone branch.
	//
	// It waits for the pad rather than returning at PLAYING because on this path
	// the state change proves nothing: srtsrc connects lazily, inside
	// gst_srt_src_fill on a streaming thread, and reports failure on the bus
	// about three seconds later. See returnAudioPadTimeout, which carries the
	// measurement. Play is therefore slow — up to returnAudioPadTimeout on a
	// dead endpoint, about a second on a live one — and Stop's worst case is one
	// of these already in flight.
	Play(opts ReturnOpts) error

	// Errors carries asynchronous failure: a bus error, an end-of-stream from
	// srtsrc, or the audio-pad timeout above. It is closed by Close.
	// Implementations drop rather than block if it is full.
	Errors() <-chan error

	// Close takes the pipeline to NULL, releases the headphone endpoint and the
	// SRT socket, and closes the channel returned by Errors. It is idempotent
	// and is safe to call on a pipeline whose Play failed or was never called.
	Close() error
}

// returnClock is the monitor's only view of time, for the same reason
// internal/sender has one: the ladder is 7, 7, 10, 15, 20 then 30 seconds
// forever, and a test that waited those durations for real would take minutes
// per case and would be the first thing anyone deleted.
//
// It is unexported and NewReturnMonitor takes no clock, so there is no way for a
// shipped build to run on anything but the real one.
type returnClock interface {
	// newTimer arms a one-shot timer for d and returns the channel it fires on
	// together with a function that releases it. stop must be safe to call
	// whether or not the timer has fired, and safe to call more than once.
	newTimer(d time.Duration) (fire <-chan time.Time, stop func())
}

// realReturnClock is the production clock. The monitor must never use
// time.Sleep: Stop has to return promptly from the middle of a thirty second
// wait, because the operator pressing Stop and being ignored for half a minute
// is its own kind of fault.
type realReturnClock struct{}

func (realReturnClock) newTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

var _ returnClock = realReturnClock{}

// returnStatesBuffer is the depth of the channel returned by States. One full
// connect-fail-backoff cycle emits two states and the shortest cycle is bounded
// below by the seven second first rung of ReturnBackoffLadder, so sixteen only
// fills if the consumer has stopped reading entirely — which is the case the
// dropping is for.
const returnStatesBuffer = 16

// returnBackoffDelay returns the wait before attempt number attempt, counting
// the attempt that has just failed as attempt zero: 7, 7, 10, 15, 20, 30, 30,
// ... with no upper bound on the number of attempts.
//
// A negative attempt cannot arise from this file's own bookkeeping and is
// clamped rather than panicking. During a match an over-long wait is survivable
// and a panic in the process carrying the commentary is not.
func returnBackoffDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt < len(ReturnBackoffLadder) {
		return ReturnBackoffLadder[attempt]
	}
	return ReturnBackoffCap
}

// ---------------------------------------------------------------------------
// The reconnect machine
// ---------------------------------------------------------------------------

// returnMonitor is the concrete ReturnMonitor.
//
// # Concurrency design
//
// It is internal/sender's design, restated rather than shared. Every state
// transition happens on exactly one goroutine, the one running run. Nothing else
// reads or writes the machine's variables, so there is no lock over the state
// and no possibility of two transitions interleaving. Everything else reaches
// the loop through channels:
//
//   - quit, closed exactly once by Stop, appears in every select the loop can
//     block in. That is what makes Stop prompt from the middle of a thirty
//     second backoff.
//   - the current attempt's pipe.Errors(), selected on only while that attempt
//     is the live one, so a stale channel can never be read.
//   - states, written only by the loop and closed only by the loop. One writer
//     that is also the only closer makes "no send on a closed channel" and "no
//     double close" structural rather than something to be careful about.
//
// mu guards only the Start/Stop bookkeeping and is never held while the loop is
// running or waited on.
type returnMonitor struct {
	newPipe func() (returnPipe, error)
	clock   returnClock

	states   chan ReturnState
	quit     chan struct{}
	loopDone chan struct{}

	// stopErr is the result of the final Close. It is written by the run
	// goroutine before loopDone is closed and read by Stop after loopDone is
	// closed, so it needs no lock.
	stopErr error

	// hook is a test seam: if non-nil it is called on the run goroutine with
	// each state immediately before that state is offered to the channel. It is
	// nil in every shipped build — NewReturnMonitor does not set it and nothing
	// exported can — and exists so that a test can call Stop with the machine
	// provably in a chosen state instead of sleeping and hoping.
	hook func(ReturnState)

	mu       sync.Mutex
	started  bool
	stopOnce sync.Once
}

var _ ReturnMonitor = (*returnMonitor)(nil)

// NewReturnMonitor returns a ReturnMonitor backed by the real GStreamer
// pipeline in a cgo build and by the stub in a Gate A build.
func NewReturnMonitor() ReturnMonitor {
	return newReturnMonitor(newReturnPipe, realReturnClock{})
}

// newReturnMonitor builds a monitor over an injected pipeline factory and
// clock. NewReturnMonitor uses the real ones; the tests use a fake pipe and a
// fake clock so the whole ladder runs in microseconds.
func newReturnMonitor(newPipe func() (returnPipe, error), clk returnClock) *returnMonitor {
	return &returnMonitor{
		newPipe:  newPipe,
		clock:    clk,
		states:   make(chan ReturnState, returnStatesBuffer),
		quit:     make(chan struct{}),
		loopDone: make(chan struct{}),
	}
}

// States returns the state stream. See the ReturnMonitor interface.
func (m *returnMonitor) States() <-chan ReturnState { return m.states }

// Start validates opts and launches the loop. See the ReturnMonitor interface.
func (m *returnMonitor) Start(opts ReturnOpts) error {
	if err := opts.normalise(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return ErrReturnAlreadyStarted
	}
	m.started = true

	log.Printf("gst: return monitor starting on %s", opts.endpointForLog())
	go m.run(opts)
	return nil
}

// Stop unwinds the session. See the ReturnMonitor interface.
//
// It closes quit exactly once and then waits for the run goroutine, so that by
// the time it returns the pipeline is at NULL, ReturnStateStopped has been
// emitted and the states channel is closed. Waiting is what lets a caller stop
// the monitor and immediately build another one without two pipelines
// contending for the same headphone endpoint.
//
// It is idempotent in effect: later calls close nothing, wait on an
// already-closed loopDone and return the same error.
func (m *returnMonitor) Stop() error {
	m.mu.Lock()
	started := m.started
	m.mu.Unlock()

	if !started {
		return ErrReturnNotStarted
	}

	m.stopOnce.Do(func() { close(m.quit) })
	<-m.loopDone
	return m.stopErr
}

// run owns the whole session on one goroutine.
func (m *returnMonitor) run(opts ReturnOpts) {
	stopErr := m.loop(opts)

	m.emit(ReturnStateStopped)
	close(m.states)

	m.stopErr = stopErr
	close(m.loopDone)
}

// loop is the state machine. It returns only when quit has been closed, and
// returns the error from the final Close, if any.
//
//	CONNECTING --play ok--> RECEIVING --error/EOS--> (close) --> BACKOFF --> CONNECTING
//	     '--play fails--------------------------------(close) --> BACKOFF
//
// attempt counts consecutive failures and indexes ReturnBackoffLadder. It is
// reset by a successful connect, so the first wait after a mid-match drop is the
// seven second rung that clears M2L-X's re-accept refusal window, not whatever
// rung an earlier outage had climbed to.
//
// The ORDER of close-then-wait is the load-bearing part, and it is the same
// point specification section 6.2 makes for the send path. An M2L-X listener
// accepts one peer and refuses re-accept for roughly five seconds after that
// peer goes away, so the wait only clears the window if our socket is already
// gone when it starts ticking. Every path out of an attempt therefore calls
// Close before it reaches the sleep, and there is nothing between the two.
func (m *returnMonitor) loop(opts ReturnOpts) error {
	attempt := 0
	var lastCloseErr error

	for {
		if m.quitting() {
			return lastCloseErr
		}

		m.emit(ReturnStateConnecting)

		pipe, err := m.newPipe()
		if err != nil {
			// The pipeline could not even be created — in practice a missing
			// plugin. Retrying will not fix that, but it costs nothing and the
			// alternative is a monitor that is dead for the rest of the match
			// because the bundle was repaired while it was running. The reason
			// is logged on every attempt, so it is not silent.
			m.report(err, attempt)
		} else {
			err = pipe.Play(opts)
			if err == nil {
				attempt = 0
				m.emit(ReturnStateReceiving)
				log.Printf("gst: return monitor receiving from %s", opts.endpointForLog())

				// RECEIVING. The only things that can happen are the far end
				// going away — a bus error, or an EOS from srtsrc, or the
				// audio-pad watchdog firing — and Stop.
				//
				// A closed error channel counts as a failure too. Only Close
				// closes it, and Close has not been called on this pipe yet, so
				// a closed channel here means the implementation gave up
				// without saying why. Treating that as healthy would be a green
				// lamp over silence.
				select {
				case <-m.quit:
					if cerr := pipe.Close(); cerr != nil {
						lastCloseErr = cerr
						log.Printf("gst: return monitor: closing the pipeline on Stop: %v", cerr)
					}
					return lastCloseErr
				case e, ok := <-pipe.Errors():
					if !ok {
						e = errors.New("gst: return monitor: the pipeline closed its error channel without being stopped")
					}
					if e == nil {
						e = errors.New("gst: return monitor: the pipeline reported an unspecified error")
					}
					err = e
				}
			}

			// Close BEFORE the wait, on every failure path, so that our socket
			// is gone for the whole of the backoff. See the doc comment above.
			if cerr := pipe.Close(); cerr != nil {
				lastCloseErr = cerr
				log.Printf("gst: return monitor: closing a failed pipeline: %v", cerr)
			}
			m.report(err, attempt)
		}

		if m.quitting() {
			return lastCloseErr
		}

		d := returnBackoffDelay(attempt)
		attempt++
		m.emit(ReturnStateBackoff)
		if !m.sleep(d) {
			return lastCloseErr
		}
	}
}

// report logs why one attempt failed.
//
// It always logs, and it logs the attempt count, because the pair is what
// distinguishes a network blip from the case that matters: a fault that cannot
// clear itself — the headphone endpoint unplugged, say, or the mpegtsdemux
// plugin missing from the bundle — where the attempt count climbs while the
// reason never changes. Retrying forever is still correct; not saying why is
// not.
//
// There is no OnConnectError callback here, unlike internal/sender's Opts. The
// send path has one because a contribution feed that is silently down is a
// broadcast failure the operator must be interrupted about. A monitor that is
// down is something the operator can see on the RETURN lamp and hear in the
// headphones, and adding a second source of rate-limited error toasts during a
// match buries the ones that mean the feed is off air.
func (m *returnMonitor) report(err error, attempt int) {
	if err == nil {
		return
	}
	log.Printf("gst: return monitor: attempt %d failed: %v; retrying in %v",
		attempt+1, err, returnBackoffDelay(attempt))
}

// emit offers a state to the channel without blocking. It drops rather than
// blocks: the reconnect loop must never be stalled by the UI, and a consumer
// that has fallen behind still sees the current state next transition.
func (m *returnMonitor) emit(s ReturnState) {
	if m.hook != nil {
		m.hook(s)
	}
	select {
	case m.states <- s:
	default:
	}
}

// quitting reports whether Stop has been called.
func (m *returnMonitor) quitting() bool {
	select {
	case <-m.quit:
		return true
	default:
		return false
	}
}

// sleep waits for d on the injected clock and reports whether the wait
// completed. It returns false as soon as quit is closed, which is what makes
// Stop prompt from the middle of the thirty second rung.
//
// Unlike internal/sender's, this one does not have to absorb stale errors
// during the wait: the pipeline that produced them has been closed and its
// channel has gone with it, so there is nothing left to select on.
func (m *returnMonitor) sleep(d time.Duration) bool {
	fire, stop := m.clock.newTimer(d)
	defer stop()

	select {
	case <-m.quit:
		return false
	case <-fire:
		return true
	}
}
