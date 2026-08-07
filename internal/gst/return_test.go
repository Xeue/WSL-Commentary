// Tests for the SRT return path's pure-Go half: the mix matrix, the option
// rules and the reconnect state machine.
//
// This file carries NO build tag on purpose, unlike gst_stub_test.go. Everything
// it exercises lives in return.go, which is pure Go in both builds, so these
// tests run at Gate A against the stub AND at Gate B against the real
// implementation — where they are the only automated check on the return path
// that a machine with GStreamer installed will run. The pipeline itself cannot
// be unit-tested without GStreamer, which is exactly why so much was kept out of
// it.
package gst

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The mix matrix
// ---------------------------------------------------------------------------

func TestMixMatrixForTheThreeChannelModes(t *testing.T) {
	// The matrices are written out here rather than derived, because a derived
	// expectation reproduces whatever bug the implementation has. Rows are
	// OUTPUT channels and columns are INPUT channels; getting that the wrong way
	// round produces a matrix that is square, negotiates fine, and swaps the
	// commentator's ears.
	cases := []struct {
		ch   ReturnChannel
		want [][]float32
		why  string
	}{
		{
			ch:   ReturnChannelStereo,
			want: [][]float32{{1, 0}, {0, 1}},
			why:  "identity: left to left, right to right",
		},
		{
			ch:   ReturnChannelLeft,
			want: [][]float32{{1, 0}, {1, 0}},
			why:  "source channel 0 into BOTH output channels",
		},
		{
			ch:   ReturnChannelRight,
			want: [][]float32{{0, 1}, {0, 1}},
			why:  "source channel 1 into BOTH output channels",
		},
	}

	for _, tc := range cases {
		t.Run(string(tc.ch), func(t *testing.T) {
			got, err := MixMatrix(tc.ch)
			if err != nil {
				t.Fatalf("MixMatrix(%q) error = %v", tc.ch, err)
			}
			if !matrixEqual(got, tc.want) {
				t.Fatalf("MixMatrix(%q) = %v, want %v (%s)", tc.ch, got, tc.want, tc.why)
			}
		})
	}
}

func TestMixMatrixIsAlwaysTwoByTwo(t *testing.T) {
	// Two rows because the destination is a pair of headphones, two columns
	// because the return is stereo. A matrix of any other shape would make
	// audioconvert negotiate a channel count nothing downstream expects.
	for _, ch := range []ReturnChannel{ReturnChannelStereo, ReturnChannelLeft, ReturnChannelRight} {
		m, err := MixMatrix(ch)
		if err != nil {
			t.Fatalf("MixMatrix(%q) error = %v", ch, err)
		}
		if len(m) != 2 {
			t.Fatalf("MixMatrix(%q) has %d rows, want 2 (one per headphone channel)", ch, len(m))
		}
		for i, row := range m {
			if len(row) != 2 {
				t.Fatalf("MixMatrix(%q) row %d has %d columns, want 2 (the return is stereo)", ch, i, len(row))
			}
		}
	}
}

func TestSingleChannelModesFeedBothEars(t *testing.T) {
	// The requirement this exists for: "left only" must put the LEFT source
	// channel into BOTH output channels, not silence one ear.
	//
	// A commentator wearing headphones with one dead side spends the match
	// assuming the headphones are broken, and monitoring in mono is what they
	// actually asked for. The literal reading of "left only" is the wrong one and
	// this test is what stops somebody implementing it.
	for _, tc := range []struct {
		ch  ReturnChannel
		col int
	}{
		{ReturnChannelLeft, 0},
		{ReturnChannelRight, 1},
	} {
		t.Run(string(tc.ch), func(t *testing.T) {
			m, err := MixMatrix(tc.ch)
			if err != nil {
				t.Fatalf("MixMatrix(%q) error = %v", tc.ch, err)
			}
			for out, row := range m {
				var sum float32
				for _, v := range row {
					sum += v
				}
				if sum == 0 {
					t.Fatalf("MixMatrix(%q) output channel %d is silent; "+
						"a single-channel selection must reach both ears", tc.ch, out)
				}
				if row[tc.col] != 1 {
					t.Fatalf("MixMatrix(%q) output channel %d takes %v from source channel %d, want 1",
						tc.ch, out, row[tc.col], tc.col)
				}
			}
		})
	}
}

func TestMixMatrixRejectsAnUnknownMode(t *testing.T) {
	// Silently defaulting an unrecognised mode to stereo would mean a typo in
	// config.json produced a return that worked and was wrong.
	if _, err := MixMatrix(ReturnChannel("centre")); err == nil {
		t.Fatal("MixMatrix(\"centre\") succeeded; an unknown mode must be an error, not a silent stereo")
	}
	if _, err := MixMatrix(""); err == nil {
		t.Fatal("MixMatrix(\"\") succeeded; ReturnOpts.normalise substitutes the default, not this")
	}
}

func TestMixMatrixStringTagsEveryValueAsFloat(t *testing.T) {
	// This asserts the exact serialisation, not just the numbers.
	//
	// The claim that an untagged matrix would be rejected was TESTED against
	// GStreamer 1.28.5 and is false: "<<1,0>>" deserialises to the same matrix
	// and negotiates identically. The tag is still written and still asserted,
	// for the reason on MixMatrixString — gst_util_set_object_arg returns void
	// and does nothing at all with a value it cannot parse, so if any future
	// deserialiser ever does become stricter, the symptom is stereo when the
	// operator asked for left, with no error anywhere. That is a failure this
	// test can catch and the running system cannot.
	cases := []struct {
		ch   ReturnChannel
		want string
	}{
		{ReturnChannelStereo, "<<(float)1,(float)0>,<(float)0,(float)1>>"},
		{ReturnChannelLeft, "<<(float)1,(float)0>,<(float)1,(float)0>>"},
		{ReturnChannelRight, "<<(float)0,(float)1>,<(float)0,(float)1>>"},
	}
	for _, tc := range cases {
		t.Run(string(tc.ch), func(t *testing.T) {
			m, err := MixMatrix(tc.ch)
			if err != nil {
				t.Fatalf("MixMatrix(%q) error = %v", tc.ch, err)
			}
			got := MixMatrixString(m)
			if got != tc.want {
				t.Fatalf("MixMatrixString(%q) = %q, want %q", tc.ch, got, tc.want)
			}
			if strings.Count(got, "(float)") != 4 {
				t.Fatalf("MixMatrixString(%q) = %q: every value must carry the (float) tag, "+
					"or the property set is a silent no-op", tc.ch, got)
			}
		})
	}
}

func TestMixMatrixStringOfNothingIsEmpty(t *testing.T) {
	// The empty string is the caller's signal to leave the property alone. A
	// literal "<>" would be deserialised as an empty matrix and would pin
	// audioconvert to zero channels.
	if got := MixMatrixString(nil); got != "" {
		t.Fatalf("MixMatrixString(nil) = %q, want the empty string", got)
	}
	if got := MixMatrixString([][]float32{}); got != "" {
		t.Fatalf("MixMatrixString(empty) = %q, want the empty string", got)
	}
}

func matrixEqual(a, b [][]float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Options
// ---------------------------------------------------------------------------

func TestReturnOptsNormaliseFillsTheDocumentedDefaults(t *testing.T) {
	o := ReturnOpts{Host: "m2lx.example.com", Port: 40503}
	if err := o.normalise(); err != nil {
		t.Fatalf("normalise() error = %v", err)
	}
	if o.LatencyMs != DefaultReturnLatencyMs {
		t.Fatalf("LatencyMs = %d, want the documented default %d", o.LatencyMs, DefaultReturnLatencyMs)
	}
	if o.Channel != DefaultReturnChannel {
		t.Fatalf("Channel = %q, want the documented default %q", o.Channel, DefaultReturnChannel)
	}
}

func TestReturnOptsNormaliseDefaultsPBKeyLenOnlyWithAPassphrase(t *testing.T) {
	// A pbkeylen of zero with no passphrase is the ordinary unencrypted case and
	// must stay zero. Inventing 16 there would ask libsrt for an encrypted
	// session with no key.
	o := ReturnOpts{Host: "h", Port: 1}
	if err := o.normalise(); err != nil {
		t.Fatalf("normalise() error = %v", err)
	}
	if o.PBKeyLen != 0 {
		t.Fatalf("PBKeyLen = %d with no passphrase, want 0", o.PBKeyLen)
	}

	o = ReturnOpts{Host: "h", Port: 1, Passphrase: "hunter2"}
	if err := o.normalise(); err != nil {
		t.Fatalf("normalise() error = %v", err)
	}
	if o.PBKeyLen != 16 {
		t.Fatalf("PBKeyLen = %d with a passphrase, want the default 16", o.PBKeyLen)
	}
}

func TestReturnOptsNormaliseReportsEveryProblemAtOnce(t *testing.T) {
	// One joined error rather than the first problem, so the Settings screen can
	// show every field that is wrong instead of one edit-fail cycle at a time.
	o := ReturnOpts{Host: "  ", Port: 70000, LatencyMs: -1, Channel: "sideways"}
	err := o.normalise()
	if err == nil {
		t.Fatal("normalise() accepted an entirely invalid ReturnOpts")
	}
	for _, want := range []string{"Host", "Port", "LatencyMs", "Channel"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %s", err, want)
		}
	}
}

func TestReturnOptsNeverLogsThePassphrase(t *testing.T) {
	// The requirement, enforced. endpointForLog is the only renderer of these
	// options in the package, and it exists precisely so that nothing is tempted
	// to reach for %+v on the struct.
	o := ReturnOpts{
		Host:       "m2lx.example.com",
		Port:       40503,
		LatencyMs:  120,
		Passphrase: "correct-horse-battery-staple",
		PBKeyLen:   32,
		Channel:    ReturnChannelLeft,
	}
	line := o.endpointForLog()
	if strings.Contains(line, o.Passphrase) {
		t.Fatalf("endpointForLog() = %q, which contains the passphrase", line)
	}
	// It must still say whether the session is encrypted: "there is no
	// passphrase" and "the passphrase is wrong" are different faults and the log
	// has to be able to tell them apart.
	if !strings.Contains(line, "aes-256") {
		t.Fatalf("endpointForLog() = %q, want it to report the encryption in use", line)
	}

	plain := ReturnOpts{Host: "h", Port: 1, Channel: ReturnChannelStereo}
	if !strings.Contains(plain.endpointForLog(), "encryption none") {
		t.Fatalf("endpointForLog() = %q, want it to say an unencrypted session is unencrypted",
			plain.endpointForLog())
	}
}

// ---------------------------------------------------------------------------
// The backoff ladder
// ---------------------------------------------------------------------------

func TestReturnBackoffLadderFirstRungClearsTheReacceptWindow(t *testing.T) {
	// M2L-X's SRT listener accepts one peer and refuses re-accept for roughly
	// five seconds after that peer goes away. A first rung shorter than that
	// lands every mid-match retry inside the window it was meant to clear.
	if got := returnBackoffDelay(0); got < 6*time.Second {
		t.Fatalf("first backoff rung is %v; it must be at least 6s to clear M2L-X's "+
			"roughly five second re-accept refusal window", got)
	}
}

func TestReturnBackoffLadderWalksThenCaps(t *testing.T) {
	want := append(append([]time.Duration(nil), ReturnBackoffLadder...),
		ReturnBackoffCap, ReturnBackoffCap, ReturnBackoffCap)
	for i, w := range want {
		if got := returnBackoffDelay(i); got != w {
			t.Fatalf("returnBackoffDelay(%d) = %v, want %v", i, got, w)
		}
	}
}

func TestReturnBackoffDelayClampsANegativeAttempt(t *testing.T) {
	// It cannot arise from the machine's own bookkeeping. It is clamped rather
	// than panicking because during a match an over-long wait is survivable and
	// a panic in the process carrying the commentary is not.
	if got := returnBackoffDelay(-3); got != ReturnBackoffLadder[0] {
		t.Fatalf("returnBackoffDelay(-3) = %v, want the first rung %v", got, ReturnBackoffLadder[0])
	}
}

// ---------------------------------------------------------------------------
// The reconnect machine
// ---------------------------------------------------------------------------

// returnHarness is a fake returnPipe factory AND a fake returnClock, sharing one
// ordered event log.
//
// The two are one object on purpose. The property that matters most about the
// machine is an ORDERING between them — the pipeline must be closed before the
// backoff wait starts, or the wait elapses while we still hold the socket and
// the retry lands inside M2L-X's re-accept refusal window — and an ordering
// cannot be asserted from two independent fakes.
type returnHarness struct {
	mu    sync.Mutex
	log   []string
	pipes []*fakeReturnPipe
	waits []time.Duration

	// newPipeErr makes the factory itself fail: the Gate A stand-in for a
	// missing plugin.
	newPipeErr error

	// failPlays is how many of the next Play calls fail.
	failPlays int

	// failErr is what those failures fail with. Nil means a generic refusal.
	// It exists so a test can put a REALISTIC libsrt rejection into the machine
	// — the text an operator would actually be shown — and follow it out through
	// ReturnOpts.OnConnectError unchanged.
	failErr error

	// closeErrsOnPlay makes a successful Play close its own error channel, which
	// is the "the pipeline gave up without saying why" case.
	closeErrsOnPlay bool

	// release, when non-nil, is what a backoff wait blocks on instead of firing
	// immediately. It is how the Stop-during-backoff case is made deterministic.
	release chan time.Time
}

func newReturnHarness() *returnHarness { return &returnHarness{} }

func (h *returnHarness) record(s string) {
	h.mu.Lock()
	h.log = append(h.log, s)
	h.mu.Unlock()
}

func (h *returnHarness) events() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.log...)
}

func (h *returnHarness) waitDurations() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.waits...)
}

func (h *returnHarness) pipeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.pipes)
}

func (h *returnHarness) lastPipe() *fakeReturnPipe {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.pipes) == 0 {
		return nil
	}
	return h.pipes[len(h.pipes)-1]
}

func (h *returnHarness) newPipe() (returnPipe, error) {
	h.record("new")
	h.mu.Lock()
	if err := h.newPipeErr; err != nil {
		h.mu.Unlock()
		return nil, err
	}
	p := &fakeReturnPipe{h: h, errs: make(chan error, 4), closeOnPlay: h.closeErrsOnPlay}
	if h.failPlays > 0 {
		h.failPlays--
		p.playErr = fmt.Errorf("fake: connect refused")
		if h.failErr != nil {
			p.playErr = h.failErr
		}
	}
	h.pipes = append(h.pipes, p)
	h.mu.Unlock()
	return p, nil
}

func (h *returnHarness) newTimer(d time.Duration) (<-chan time.Time, func()) {
	h.record("wait")
	h.mu.Lock()
	h.waits = append(h.waits, d)
	rel := h.release
	h.mu.Unlock()

	if rel != nil {
		return rel, func() {}
	}
	ch := make(chan time.Time, 1)
	ch <- time.Now()
	return ch, func() {}
}

type fakeReturnPipe struct {
	h           *returnHarness
	closeOnPlay bool

	mu       sync.Mutex
	playErr  error
	errs     chan error
	closed   bool
	lastOpts ReturnOpts
	plays    int
	closes   int
}

func (p *fakeReturnPipe) Play(opts ReturnOpts) error {
	p.h.record("play")
	p.mu.Lock()
	defer p.mu.Unlock()
	p.plays++
	p.lastOpts = opts
	if err := p.playErr; err != nil {
		return err
	}
	if p.closeOnPlay && !p.closed {
		p.closed = true
		close(p.errs)
	}
	return nil
}

func (p *fakeReturnPipe) Errors() <-chan error { return p.errs }

func (p *fakeReturnPipe) Close() error {
	p.h.record("close")
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closes++
	if !p.closed {
		p.closed = true
		close(p.errs)
	}
	return nil
}

func (p *fakeReturnPipe) inject(err error) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	select {
	case p.errs <- err:
		return true
	default:
		return false
	}
}

func (p *fakeReturnPipe) counts() (plays, closes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.plays, p.closes
}

func (p *fakeReturnPipe) opts() ReturnOpts {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastOpts
}

var _ returnPipe = (*fakeReturnPipe)(nil)
var _ returnClock = (*returnHarness)(nil)

// validReturnOpts is a configuration that normalise accepts.
func validReturnOpts() ReturnOpts {
	return ReturnOpts{
		Host:           "m2lx.example.com",
		Port:           40503,
		Channel:        ReturnChannelLeft,
		OutputDeviceID: "{0.0.0.00000000}.{deadbeef}",
	}
}

// expectState reads one state, failing the test if none arrives.
func expectState(t *testing.T, states <-chan ReturnState, want ReturnState) {
	t.Helper()
	select {
	case got, ok := <-states:
		if !ok {
			t.Fatalf("states channel closed while waiting for %q", want)
		}
		if got != want {
			t.Fatalf("state = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for state %q", want)
	}
}

// waitFor polls cond until it holds or the deadline passes. It exists because
// the machine runs on its own goroutine and the thing being waited for — a
// recorded event, a pipe having been created — is not itself a channel.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestReturnMonitorReachesReceiving(t *testing.T) {
	h := newReturnHarness()
	h.release = make(chan time.Time) // never fires; nothing should back off
	m := newReturnMonitor(h.newPipe, h)

	if err := m.Start(validReturnOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectState(t, m.States(), ReturnStateConnecting)
	expectState(t, m.States(), ReturnStateReceiving)

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	expectState(t, m.States(), ReturnStateStopped)

	if _, ok := <-m.States(); ok {
		t.Fatal("the states channel is still open after Stop; it must be closed")
	}

	p := h.lastPipe()
	plays, closes := p.counts()
	if plays != 1 || closes != 1 {
		t.Fatalf("pipe plays = %d, closes = %d; want exactly one of each", plays, closes)
	}
}

func TestReturnMonitorPassesTheOptionsThroughNormalised(t *testing.T) {
	h := newReturnHarness()
	h.release = make(chan time.Time)
	m := newReturnMonitor(h.newPipe, h)

	opts := validReturnOpts()
	opts.LatencyMs = 0 // ask for the default
	if err := m.Start(opts); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectState(t, m.States(), ReturnStateConnecting)
	expectState(t, m.States(), ReturnStateReceiving)

	got := h.lastPipe().opts()
	if got.LatencyMs != DefaultReturnLatencyMs {
		t.Fatalf("the pipeline was given LatencyMs = %d, want the substituted default %d",
			got.LatencyMs, DefaultReturnLatencyMs)
	}
	if got.Channel != ReturnChannelLeft {
		t.Fatalf("the pipeline was given Channel = %q, want %q", got.Channel, ReturnChannelLeft)
	}
	if got.OutputDeviceID != opts.OutputDeviceID {
		t.Fatalf("the pipeline was given OutputDeviceID = %q, want %q",
			got.OutputDeviceID, opts.OutputDeviceID)
	}

	_ = m.Stop()
}

func TestReturnMonitorClosesThePipelineBeforeItWaits(t *testing.T) {
	// THE ordering property of this machine.
	//
	// An M2L-X SRT listener accepts exactly one peer, never displaces the
	// incumbent, and refuses re-accept for roughly five seconds. The backoff
	// only buys anything if OUR socket is already gone when it starts ticking.
	// Closing after the wait — or, worse, only as a side effect of building the
	// next pipeline — means the wait elapses while we still hold the socket and
	// the retry lands inside the refusal window it was supposed to clear. That
	// costs a wasted attempt and roughly double the time off air on every
	// reconnect.
	//
	// It is asserted on the ORDER of the shared event log rather than on a call
	// count, because a call count cannot tell the two arrangements apart.
	h := newReturnHarness()
	h.failPlays = 1
	m := newReturnMonitor(h.newPipe, h)

	if err := m.Start(validReturnOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectState(t, m.States(), ReturnStateConnecting)
	expectState(t, m.States(), ReturnStateBackoff)
	expectState(t, m.States(), ReturnStateConnecting)
	expectState(t, m.States(), ReturnStateReceiving)

	events := h.events()
	want := []string{"new", "play", "close", "wait", "new", "play"}
	if len(events) < len(want) {
		t.Fatalf("event log = %v, want at least %v", events, want)
	}
	for i, w := range want {
		if events[i] != w {
			t.Fatalf("event log = %v, want it to begin %v; "+
				"the pipeline must be closed BEFORE the backoff wait, not after it",
				events, want)
		}
	}

	_ = m.Stop()
}

func TestReturnMonitorWalksTheLadderAndNeverGivesUp(t *testing.T) {
	// There is no attempt limit. A return that gave up after N tries would be a
	// commentator with no reference for the rest of the match because of a
	// thirty second blip in the first half.
	h := newReturnHarness()
	h.failPlays = len(ReturnBackoffLadder) + 3
	m := newReturnMonitor(h.newPipe, h)

	if err := m.Start(validReturnOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	wantWaits := append(append([]time.Duration(nil), ReturnBackoffLadder...),
		ReturnBackoffCap, ReturnBackoffCap, ReturnBackoffCap)
	waitFor(t, "the whole ladder plus three capped rungs", func() bool {
		return len(h.waitDurations()) >= len(wantWaits)
	})

	got := h.waitDurations()[:len(wantWaits)]
	for i := range wantWaits {
		if got[i] != wantWaits[i] {
			t.Fatalf("backoff waits = %v, want %v", got, wantWaits)
		}
	}

	_ = m.Stop()
}

func TestReturnMonitorResetsTheLadderAfterASuccessfulConnect(t *testing.T) {
	// The first wait after a mid-match drop must be the first rung, not whatever
	// rung an earlier outage had climbed to. Otherwise a monitor that lost its
	// peer twice in a match is waiting thirty seconds to recover from the second
	// one for no reason.
	h := newReturnHarness()
	h.failPlays = 2
	m := newReturnMonitor(h.newPipe, h)

	if err := m.Start(validReturnOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectState(t, m.States(), ReturnStateConnecting)
	expectState(t, m.States(), ReturnStateBackoff)
	expectState(t, m.States(), ReturnStateConnecting)
	expectState(t, m.States(), ReturnStateBackoff)
	expectState(t, m.States(), ReturnStateConnecting)
	expectState(t, m.States(), ReturnStateReceiving)

	// The peer goes away.
	waitFor(t, "the connected pipe", func() bool { return h.lastPipe() != nil })
	if !h.lastPipe().inject(errors.New("fake: the SRT peer went away")) {
		t.Fatal("could not inject an error into the connected pipe")
	}
	expectState(t, m.States(), ReturnStateBackoff)

	waitFor(t, "three backoff waits", func() bool { return len(h.waitDurations()) >= 3 })
	waits := h.waitDurations()[:3]
	want := []time.Duration{ReturnBackoffLadder[0], ReturnBackoffLadder[1], ReturnBackoffLadder[0]}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("backoff waits = %v, want %v; the ladder must restart after a successful connect",
				waits, want)
		}
	}

	_ = m.Stop()
}

func TestReturnMonitorTreatsAClosedErrorChannelAsFailure(t *testing.T) {
	// Only Close closes that channel, and the machine has not called Close on
	// this pipe yet — so a closed channel means the implementation gave up
	// without saying why. Reading it as healthy would be a green lamp over
	// silence, which is the one outcome this whole path exists to prevent.
	h := newReturnHarness()
	h.closeErrsOnPlay = true
	m := newReturnMonitor(h.newPipe, h)

	if err := m.Start(validReturnOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectState(t, m.States(), ReturnStateConnecting)
	expectState(t, m.States(), ReturnStateReceiving)
	expectState(t, m.States(), ReturnStateBackoff)

	_ = m.Stop()
}

func TestReturnMonitorRetriesWhenThePipelineCannotEvenBeBuilt(t *testing.T) {
	// The missing-plugin case. Retrying will not repair a bundle, but giving up
	// would mean a monitor that stays dead after the bundle IS repaired, and the
	// reason is logged on every attempt so it is not silent.
	h := newReturnHarness()
	h.newPipeErr = errors.New("fake: tsdemux is not in the bundle")
	m := newReturnMonitor(h.newPipe, h)

	if err := m.Start(validReturnOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectState(t, m.States(), ReturnStateConnecting)
	expectState(t, m.States(), ReturnStateBackoff)
	expectState(t, m.States(), ReturnStateConnecting)
	expectState(t, m.States(), ReturnStateBackoff)

	if h.pipeCount() != 0 {
		t.Fatalf("%d pipes were created despite the factory failing", h.pipeCount())
	}
	for _, e := range h.events() {
		if e == "play" || e == "close" {
			t.Fatalf("event log = %v: nothing may be played or closed when no pipeline was built",
				h.events())
		}
	}

	_ = m.Stop()
}

func TestReturnMonitorStopIsPromptFromTheMiddleOfABackoff(t *testing.T) {
	// The whole reason the clock is injected. The production ladder tops out at
	// thirty seconds, and "the operator pressed Stop and the app ignored them
	// for half a minute" is its own kind of fault.
	h := newReturnHarness()
	h.failPlays = 1000
	h.release = make(chan time.Time) // never fires
	m := newReturnMonitor(h.newPipe, h)

	if err := m.Start(validReturnOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectState(t, m.States(), ReturnStateConnecting)
	expectState(t, m.States(), ReturnStateBackoff)

	done := make(chan error, 1)
	go func() { done <- m.Stop() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return from the middle of a backoff wait")
	}

	expectState(t, m.States(), ReturnStateStopped)
}

func TestReturnMonitorStopClosesTheRunningPipeline(t *testing.T) {
	// Stopping while RECEIVING must release the headphone endpoint and the SRT
	// socket, or the next StartReturn cannot have them.
	h := newReturnHarness()
	h.release = make(chan time.Time)
	m := newReturnMonitor(h.newPipe, h)

	if err := m.Start(validReturnOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectState(t, m.States(), ReturnStateConnecting)
	expectState(t, m.States(), ReturnStateReceiving)

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if _, closes := h.lastPipe().counts(); closes != 1 {
		t.Fatalf("the running pipeline was closed %d times on Stop, want exactly 1", closes)
	}
}

func TestReturnMonitorRejectsAConfigurationItCannotUse(t *testing.T) {
	// A connection failure is not an error from Start — the monitor retries
	// forever. A configuration that can never work IS, and it names the field,
	// because the operator is standing in front of the settings screen and that
	// one will not fix itself.
	h := newReturnHarness()
	m := newReturnMonitor(h.newPipe, h)

	err := m.Start(ReturnOpts{Host: "", Port: 0, Channel: "sideways"})
	if err == nil {
		t.Fatal("Start() accepted an unusable configuration")
	}
	if h.pipeCount() != 0 {
		t.Fatal("Start() built a pipeline despite rejecting the configuration")
	}
	if !errors.Is(m.Stop(), ErrReturnNotStarted) {
		t.Fatal("a monitor whose Start was rejected must still count as not started")
	}
}

func TestReturnMonitorStartAndStopBookkeeping(t *testing.T) {
	h := newReturnHarness()
	h.release = make(chan time.Time)
	m := newReturnMonitor(h.newPipe, h)

	if !errors.Is(m.Stop(), ErrReturnNotStarted) {
		t.Fatal("Stop() before Start must report ErrReturnNotStarted")
	}
	if err := m.Start(validReturnOpts()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if !errors.Is(m.Start(validReturnOpts()), ErrReturnAlreadyStarted) {
		t.Fatal("a second Start must report ErrReturnAlreadyStarted")
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	// A stopped monitor is single-use: its states channel is closed and
	// restarting it would send on a closed channel.
	if !errors.Is(m.Start(validReturnOpts()), ErrReturnAlreadyStarted) {
		t.Fatal("Start() after Stop must be refused; a ReturnMonitor is single-use")
	}
	// Idempotent in effect.
	if err := m.Stop(); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
}

func TestReturnChannelValid(t *testing.T) {
	for _, ch := range []ReturnChannel{ReturnChannelStereo, ReturnChannelLeft, ReturnChannelRight} {
		if !ch.Valid() {
			t.Fatalf("%q must be valid", ch)
		}
	}
	// Empty is deliberately NOT valid: normalise substitutes the default, so a
	// value that reaches Valid empty came from somewhere that skipped it.
	for _, ch := range []ReturnChannel{"", "STEREO", "centre", "l"} {
		if ch.Valid() {
			t.Fatalf("%q must not be valid", ch)
		}
	}
}

// ---------------------------------------------------------------------------
// Element naming for dynamically added pads
// ---------------------------------------------------------------------------

// TestFakeSinkNamesCannotCollideAcrossAttempts guards a monitor that sits in
// backoff for a whole match against an M2L-X answering perfectly.
//
// One pipeline is reused for every address resolveSinkHost returned, and tsdemux
// names its src pads after the PID — audio_0100, video_0101 — so address 2
// demuxes the same transport and produces the same pad names as address 1.
// gst_bin_add REFUSES an element whose name is already taken in the bin, and it
// refuses quietly. A fakesink name derived from the pad alone therefore cannot
// be added on the second attempt, the undecoded pad is left with no sink, and an
// unlinked demuxer src pad returns GST_FLOW_NOT_LINKED until mpegtsbase pauses
// the task and posts an error. That is exactly the wedge the fakesink exists to
// prevent, arriving through the mechanism meant to prevent it.
//
// The scenario is not hypothetical: it needs only a host that has gained a
// second A record and a first address that got far enough to parse a PMT.
func TestFakeSinkNamesCannotCollideAcrossAttempts(t *testing.T) {
	const pad = "video_0101"

	seen := make(map[string]uint64)
	for seq := uint64(1); seq <= 8; seq++ {
		name := fakeSinkName("retfake", seq, pad)
		if first, dup := seen[name]; dup {
			t.Fatalf("attempt %d produced the element name %q, already used by attempt %d: "+
				"gst_bin_add refuses it and the pad is left unlinked", seq, name, first)
		}
		seen[name] = seq

		if !strings.Contains(name, pad) {
			t.Errorf("name %q drops the pad name; a bus message from this element "+
				"cannot be traced back to a stream in a field log", name)
		}
		if !strings.HasPrefix(name, "retfake") {
			t.Errorf("name %q does not carry the prefix it was given", name)
		}
	}
}

// TestFakeSinkNamesDifferPerPadWithinOneAttempt guards the other direction: a
// transport carrying video and a second audio stream produces two undecoded pads
// on the SAME attempt, and both need a sink.
func TestFakeSinkNamesDifferPerPadWithinOneAttempt(t *testing.T) {
	a := fakeSinkName("retfake", 1, "video_0101")
	b := fakeSinkName("retfake", 2, "audio_0102")
	if a == b {
		t.Fatalf("two pads on one attempt got the same element name %q", a)
	}
}

// TestFakeSinkNameSurvivesAnUnnamedPad guards the degenerate case. A pad with no
// name would otherwise yield a name ending in "-", and two of them would collide
// with each other — the sequence number already prevents that, but a name that
// ends in a dangling separator reads as a truncated log line.
func TestFakeSinkNameSurvivesAnUnnamedPad(t *testing.T) {
	name := fakeSinkName("retfake", 3, "")
	if strings.HasSuffix(name, "-") {
		t.Errorf("name %q ends in a separator with nothing after it", name)
	}
	if name == fakeSinkName("retfake", 4, "") {
		t.Error("two unnamed pads produced the same element name")
	}
}

// ---------------------------------------------------------------------------
// Saying why: ReturnOpts.OnConnectError
// ---------------------------------------------------------------------------
//
// These tests carry no build tag, like the rest of this file, so they run at
// Gate A against the stub and at Gate B against the real pipeline. That is the
// point rather than an accident: the callback is honoured once, in return.go,
// and never by a returnPipe, so proving it here proves it for both builds.

// srtBadSecretError is the shape internal/gst delivers when M2L-X refuses an
// encrypted handshake: a bus error from srtsrc, wrapped with its source element,
// carrying libsrt's reject reason inside GStreamer's debug string.
//
// It is spelled out in full rather than abbreviated to the token, because what
// is being tested is that the WHOLE string survives the trip. app_return.go
// classifies on the token and then quotes the rest to the operator, so a machine
// that helpfully tidied the reason on the way past would break the half that
// matters most — the part a support engineer searches for.
var srtBadSecretError = errors.New("gst: return monitor: retsrc: Could not read from resource. " +
	"(../ext/srt/gstsrtsrc.c(206): gst_srt_src_fill (): Error on SRT socket: " +
	"Connection setup failure: ERROR:BADSECRET)")

// logCapture collects everything the package logs during one test.
type logCapture struct {
	mu  sync.Mutex
	buf []byte
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *logCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

// captureLog redirects the standard logger for one test and returns the sink.
// The previous destination and flags are restored by a t.Cleanup, so a failure
// part-way through cannot leave the rest of the binary writing into a dead
// buffer.
//
// Two properties below are only observable in the log: that the reason is still
// logged for the callers who set no callback, and that a nil callback is
// CHECKED rather than caught by the recover. Without reading the log those tests
// would pass whatever the code did.
func captureLog(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(c)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return c
}

// reportSink records what OnConnectError was called with.
type reportSink struct {
	mu   sync.Mutex
	seen []error
}

func (r *reportSink) errs() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]error(nil), r.seen...)
}

// callback returns the func to install on ReturnOpts. It records into the
// harness's ordered event log as well, so that WHEN it fired can be asserted
// against the pipeline's own lifecycle rather than only that it did.
func (r *reportSink) callback(h *returnHarness) func(error) {
	return func(err error) {
		h.record("report")
		r.mu.Lock()
		r.seen = append(r.seen, err)
		r.mu.Unlock()
	}
}

func TestReturnOnConnectErrorCarriesTheReasonVerbatim(t *testing.T) {
	// THE defect. libsrt knew the answer — ERROR:BADSECRET, a passphrase offered
	// and refused — the machine logged it and discarded it, and the operator was
	// told "it will not connect" for an afternoon.
	//
	// errors.Is, not a substring match: it must arrive as the SAME error and not
	// as something built from it, because everything downstream classifies on the
	// text and a reason summarised on the way out would be a guess again.
	h := newReturnHarness()
	h.failPlays = 1
	h.failErr = srtBadSecretError
	m := newReturnMonitor(h.newPipe, h)

	sink := &reportSink{}
	opts := validReturnOpts()
	opts.OnConnectError = sink.callback(h)

	if err := m.Start(opts); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectState(t, m.States(), ReturnStateConnecting)
	expectState(t, m.States(), ReturnStateBackoff)
	expectState(t, m.States(), ReturnStateConnecting)
	expectState(t, m.States(), ReturnStateReceiving)
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	got := sink.errs()
	if len(got) != 1 {
		t.Fatalf("OnConnectError called %d times, want once (a successful attempt must not report): %v",
			len(got), got)
	}
	if !errors.Is(got[0], srtBadSecretError) {
		t.Fatalf("OnConnectError got %v, want the pipeline's own error %v", got[0], srtBadSecretError)
	}
	if !strings.Contains(got[0].Error(), "ERROR:BADSECRET") {
		t.Fatalf("the reason reached the caller without libsrt's reject reason in it: %q", got[0])
	}

	// WHEN it fires is part of the contract: after the pipeline is closed and
	// before the backoff wait. Fired after the wait it would report a failure the
	// operator has already been staring at for seven seconds; fired before Close
	// it would describe a socket still open.
	events := h.events()
	want := []string{"new", "play", "close", "report", "wait"}
	if len(events) < len(want) {
		t.Fatalf("event log = %v, want it to begin %v", events, want)
	}
	for i, w := range want {
		if events[i] != w {
			t.Fatalf("event log = %v, want it to begin %v; the reason must be reported after the "+
				"pipeline is closed and before the backoff wait", events, want)
		}
	}
}

func TestReturnOnConnectErrorReportsAPipelineThatCannotBeBuilt(t *testing.T) {
	// A missing plugin never reaches libsrt at all, and it is the one fault that
	// cannot clear itself. It has to come out of the same hole: an operator whose
	// bundle is missing mpegtsdemux otherwise gets an amber lamp and no words
	// anywhere but the log.
	buildErr := errors.New("gst: return monitor: the bundled GStreamer is missing mpegtsdemux")

	h := newReturnHarness()
	h.newPipeErr = buildErr
	h.release = make(chan time.Time) // park in the first backoff
	m := newReturnMonitor(h.newPipe, h)

	sink := &reportSink{}
	opts := validReturnOpts()
	opts.OnConnectError = sink.callback(h)

	if err := m.Start(opts); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectState(t, m.States(), ReturnStateConnecting)
	expectState(t, m.States(), ReturnStateBackoff)

	waitFor(t, "the build failure to be reported", func() bool { return len(sink.errs()) > 0 })
	if got := sink.errs()[0]; !errors.Is(got, buildErr) {
		t.Fatalf("OnConnectError got %v, want %v", got, buildErr)
	}

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestReturnOnConnectErrorIsCalledForALostPeer(t *testing.T) {
	// The documented difference from sender.Opts.OnConnectError, asserted so that
	// it cannot be quietly "fixed" into agreement with it.
	//
	// The send path stays silent about losing its peer because it has a DRAINING
	// state that already says so. This path has four states and rebuilds the whole
	// pipeline per attempt, so every failure ends an attempt identically — and a
	// monitor that has been dropping its peer every eight seconds for two minutes
	// is something the operator needs the words for.
	peerGone := errors.New("gst: return monitor: the return stream ended (the SRT peer went away)")

	h := newReturnHarness()
	h.release = make(chan time.Time)
	m := newReturnMonitor(h.newPipe, h)

	sink := &reportSink{}
	opts := validReturnOpts()
	opts.OnConnectError = sink.callback(h)

	if err := m.Start(opts); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectState(t, m.States(), ReturnStateConnecting)
	expectState(t, m.States(), ReturnStateReceiving)

	if !h.lastPipe().inject(peerGone) {
		t.Fatal("could not inject the peer loss")
	}
	expectState(t, m.States(), ReturnStateBackoff)

	waitFor(t, "the peer loss to be reported", func() bool { return len(sink.errs()) > 0 })
	if got := sink.errs()[0]; !errors.Is(got, peerGone) {
		t.Fatalf("OnConnectError got %v, want %v", got, peerGone)
	}

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestNilReturnOnConnectErrorIsFine(t *testing.T) {
	// The field is optional and most callers — every other test here, and any
	// caller that only wants the log — leave it nil. A nil callback must not
	// panic the one goroutine that can reconnect.
	//
	// It asserts on the LOG and not only on survival, and it has to. The recover
	// that contains a panicking callback would also contain a nil one, so a
	// version with the nil check deleted passes every behavioural assertion while
	// panicking and recovering on every failed attempt for the majority of
	// callers. Reading the log is what tells the two apart.
	logs := captureLog(t)

	h := newReturnHarness()
	h.failPlays = 2
	h.failErr = srtBadSecretError
	m := newReturnMonitor(h.newPipe, h)

	opts := validReturnOpts()
	if opts.OnConnectError != nil {
		t.Fatal("validReturnOpts set OnConnectError; this test needs it nil")
	}

	if err := m.Start(opts); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectState(t, m.States(), ReturnStateConnecting)
	for i := 0; i < 2; i++ {
		expectState(t, m.States(), ReturnStateBackoff)
		expectState(t, m.States(), ReturnStateConnecting)
	}
	expectState(t, m.States(), ReturnStateReceiving)
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	text := logs.text()
	if strings.Contains(text, "panicked") {
		t.Fatalf("a nil OnConnectError was called and recovered from; it must be checked, not caught:\n%s", text)
	}
	// The log is the half of this that survives a caller which sets no callback,
	// and it is where a support engineer looks after the match. The reason, the
	// attempt number and the wait all have to be in it: a reason that never
	// changes while the attempt count climbs is the signature of a fault that
	// cannot clear itself.
	for _, want := range []string{"ERROR:BADSECRET", "attempt 1", "attempt 2", ReturnBackoffLadder[0].String()} {
		if !strings.Contains(text, want) {
			t.Fatalf("the log does not mention %q; it says:\n%s", want, text)
		}
	}
}

func TestReturnOnConnectErrorPanicDoesNotStopTheReconnectLoop(t *testing.T) {
	// A panic in a caller's callback would otherwise take down the whole process:
	// Go gives no way to contain a panic raised on another goroutine, and this
	// goroutine is inside the process carrying live commentary. Keeping the
	// monitor reconnecting beats failing fast on somebody else's bug.
	logs := captureLog(t)

	h := newReturnHarness()
	h.failPlays = 2
	m := newReturnMonitor(h.newPipe, h)

	var (
		mu    sync.Mutex
		calls int
	)
	opts := validReturnOpts()
	opts.OnConnectError = func(error) {
		mu.Lock()
		calls++
		mu.Unlock()
		panic("a UI callback with a bug in it")
	}

	if err := m.Start(opts); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectState(t, m.States(), ReturnStateConnecting)
	for i := 0; i < 2; i++ {
		expectState(t, m.States(), ReturnStateBackoff)
		expectState(t, m.States(), ReturnStateConnecting)
	}
	// It reached a working monitor with a callback that panicked on every failure
	// on the way.
	expectState(t, m.States(), ReturnStateReceiving)
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 2 {
		t.Fatalf("the panicking callback was called %d times, want 2; the loop stopped reporting", n)
	}
	if !strings.Contains(logs.text(), "OnConnectError panicked") {
		t.Fatalf("a contained panic was not logged; it must not be silent:\n%s", logs.text())
	}
}

func TestReturnOnConnectErrorNeverCarriesThePassphrase(t *testing.T) {
	// The reason goes to the caller, which puts it on an event that crosses the
	// Wails boundary, and to the log, which is what gets mailed in a support
	// bundle. Neither may carry the key to a live encrypted feed.
	//
	// The passphrase cannot reach an error by construction — it is set with
	// g_object_set and never put in a URI — but this asserts that rather than
	// assuming it, over both destinations at once.
	const secret = "correct-horse-battery-staple"

	logs := captureLog(t)

	h := newReturnHarness()
	h.failPlays = 2
	h.failErr = srtBadSecretError
	m := newReturnMonitor(h.newPipe, h)

	sink := &reportSink{}
	opts := validReturnOpts()
	opts.Passphrase = secret
	opts.PBKeyLen = 32
	opts.OnConnectError = sink.callback(h)

	if err := m.Start(opts); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	expectState(t, m.States(), ReturnStateConnecting)
	for i := 0; i < 2; i++ {
		expectState(t, m.States(), ReturnStateBackoff)
		expectState(t, m.States(), ReturnStateConnecting)
	}
	expectState(t, m.States(), ReturnStateReceiving)
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	for i, err := range sink.errs() {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("reported error %d carries the passphrase: %q", i, err)
		}
	}
	text := logs.text()
	if strings.Contains(text, secret) {
		t.Fatalf("the log carries the passphrase:\n%s", text)
	}
	// It must still be possible to tell an encrypted session from an unencrypted
	// one in the log: "there is no passphrase" and "the passphrase is wrong" are
	// different faults with different fixes.
	if !strings.Contains(text, "aes-256") {
		t.Fatalf("the log does not say the session was encrypted at all:\n%s", text)
	}
}
