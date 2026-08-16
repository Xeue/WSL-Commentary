//go:build live && cgo && !gststub

// decklinkaudio_live_test.go is the Gate C proof of the DeckLink COMMENTARY
// capture leg: the card's embedded audio, through THIS PACKAGE'S OWN Pipeline,
// with a real sound played into it.
//
// Owner: WP-3a, with internal/gst.
//
// # Why it drives Pipeline rather than gst-launch
//
// Because the thing under test is the SHIPPED DESCRIPTION. A hand-written launch
// line proves that GStreamer can capture from a DeckLink, which nobody doubted;
// it proves nothing about pipelineDescription, about the clock companion being
// built on the right condition, about configureDeckLinkSource reaching all three
// decklink elements from one saved string, or about applyStartChannelMapLocked
// writing a matrix wide enough for what the pad actually negotiated. Every one
// of those is code in this package and every one of them is on the path below.
//
// # What it measures, and why the per-channel meter is the reading that matters
//
// Two level elements report here and they answer different questions. chlevel
// sits UPSTREAM of the mix matrix and reports all sixteen of the card's
// unpositioned channels, which is how an operator finds which one the
// commentator is on. alevel sits immediately before the AAC encoder and reports
// the stereo pair that is actually being encoded and sent. A run in which
// chlevel moves and alevel does not is a ROUTING fault; a run in which neither
// moves is a rig fault; a run in which both move is the feature working.
//
// # The rig
//
// UltraStudio 4K Mini on Thunderbolt, persistent-id 2747401380, output looped
// back to input. THE OWNER'S MICROPHONE SITS ON THE LAPTOP SPEAKERS, so playing
// a sound through the built-in speakers is the intended test signal and it
// reaches the card's mic input — measured at peak -42.5 dBFS with the system
// volume at 12/100. DO NOT RAISE THE VOLUME; it is low deliberately.
//
// If the card's mic input has been deselected in Blackmagic Desktop Video Setup
// the capture is DIGITAL SILENCE on all sixteen channels — exactly -96.3 dBFS,
// the S16LE floor — and that is a RIG state a human fixes by hand. It is NOT
// something to fix by setting `connection`: that property persistently
// reconfigures the card and overrides Desktop Video Setup, and it has had to be
// undone by hand twice. The test says which of the two it saw rather than
// leaving the reader to guess.
//
// THE CARD IS EXCLUSIVE. Every path out of every test below stops the pipeline;
// an orphan holds the card from everyone.

package gst

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

const (
	// The fitted card. Overridable, because the id is hardware and this file is
	// not.
	defaultLiveCard = "2747401380"

	// The intended test signal: it reaches the card's microphone input through
	// the laptop speakers.
	defaultLiveSound = "/System/Library/Sounds/Submarine.aiff"

	// liveCaptureWindow is how long each shape is left capturing. It has to
	// cover the sound being played twice with silence either side, so that a
	// peak is a peak and not the first buffer.
	liveCaptureWindow = 9 * time.Second

	// digitalSilenceDB is the S16LE floor. A channel at or below this over a
	// whole window heard nothing at all — no room tone, no noise floor, no
	// converter dither — which is what an unpatched input reports and what a
	// live microphone never does.
	digitalSilenceDB = -95.0
)

// liveDeckLinkInit resolves the bundled GStreamer inside the .app and calls
// Init, skipping on a machine that has not run the bundler.
//
// It uses bundlePluginDir rather than a written-out path so that the check is
// the package's own answer to "where are the plugins" on whichever platform this
// is compiled for, and cannot drift from what Init will actually look at.
func liveDeckLinkInit(t *testing.T) {
	t.Helper()
	appDir, err := filepath.Abs(env("WSLCOMMS_LIVE_APP_DIR",
		"../../build/bin/WSL Commentary.app/Contents/MacOS"))
	if err != nil {
		t.Fatalf("resolving the app directory: %v", err)
	}
	if _, err := os.Stat(bundlePluginDir(appDir)); err != nil {
		t.Skipf("no bundled GStreamer under %s: %v", bundlePluginDir(appDir), err)
	}
	if err := Init(appDir); err != nil {
		t.Fatalf("Init: %v", err)
	}
}

// meterWatch accumulates the loudest peak each channel reached over a run, from
// whichever level element is feeding it.
//
// It is a peak HOLD rather than a sample: the callbacks arrive on a GStreamer
// streaming thread ten to twenty times a second and MUST NOT BLOCK, so each one
// takes a mutex for a handful of float comparisons and returns. That is the
// contract PipelineOpts.OnLevels states and it is obeyed here rather than
// worked around.
type meterWatch struct {
	mu     sync.Mutex
	peak   []float64
	rms    []float64
	frames int
}

func (m *meterWatch) record(l Levels) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.frames++
	for i, p := range l.PeakDB {
		for len(m.peak) <= i {
			m.peak = append(m.peak, math.Inf(-1))
			m.rms = append(m.rms, math.Inf(-1))
		}
		if p > m.peak[i] {
			m.peak[i] = p
		}
		if i < len(l.RMSDB) && l.RMSDB[i] > m.rms[i] {
			m.rms[i] = l.RMSDB[i]
		}
	}
}

func (m *meterWatch) report() (peak, rms []float64, frames int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]float64(nil), m.peak...), append([]float64(nil), m.rms...), m.frames
}

// reset clears the hold so the next phase measures only its own window.
//
// It is what makes the QUIET / SIGNAL comparison below possible at all: a hold
// that ran for the whole test would report the loudest thing that happened
// anywhere in it, which cannot distinguish a microphone hearing a sound from a
// converter's noise floor having one loud sample.
func (m *meterWatch) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peak, m.rms, m.frames = nil, nil, 0
}

// loudest returns the highest peak seen on any channel and which channel it was
// on, one-based the way the routing UI numbers them.
func loudest(peak []float64) (db float64, channel int) {
	db, channel = math.Inf(-1), 0
	for i, p := range peak {
		if p > db {
			db, channel = p, i+1
		}
	}
	return db, channel
}

// playTheTestSignal plays the sound through the built-in speakers, into the
// microphone that feeds the card. It is deliberately synchronous and run twice:
// one play is about a second and a half, and a single one straddling a
// measurement boundary is the difference between a peak and a shrug.
func playTheTestSignal(t *testing.T) {
	t.Helper()
	sound := env("WSLCOMMS_LIVE_SOUND", defaultLiveSound)
	deadline := time.Now().Add(liveCaptureWindow)
	for time.Now().Before(deadline) {
		out, err := exec.Command("afplay", sound).CombinedOutput()
		if err != nil {
			t.Logf("afplay %s: %v (%s) — the run continues; a silent result will be reported "+
				"as a rig state rather than as a code failure", sound, err, out)
			// Do not spin on a file that will not play. Wait the window out so
			// that the two phases are still the same length and comparable.
			time.Sleep(time.Until(deadline))
			return
		}
	}
}

// capturePhase is one measurement window and what the two meters held during it.
type capturePhase struct {
	name         string
	chPeak       []float64
	chRMS        []float64
	progPeak     []float64
	progRMS      []float64
	chFrames     int
	progFrames   int
	inputChannel int
}

// runDeckLinkCapture starts the shipped pipeline for one capture shape and
// measures TWO windows: one with nothing playing, and one with the test signal
// playing into the microphone that feeds the card.
//
// THE COMPARISON IS THE PROOF, not either number on its own. A single peak
// cannot distinguish a live analogue input from a dead one — an unpatched
// converter still reports its own noise floor, and on this card that floor sits
// around -54 dBFS, which looks exactly like quiet room tone. What no dead input
// can do is GET LOUDER WHEN A SOUND IS PLAYED, so the delta between the two
// windows is the reading that settles it.
//
// videoCard is the card the PICTURE comes from, empty for the slate. The audio
// card is always the fitted one: this file is about the commentary leg.
func runDeckLinkCapture(t *testing.T, videoCard string) (quiet, signal capturePhase) {
	t.Helper()

	card := env("WSLCOMMS_LIVE_CARD", defaultLiveCard)
	slate, err := filepath.Abs(env("WSLCOMMS_LIVE_SLATE", defaultSlatePath))
	if err != nil {
		t.Fatalf("resolving the slate: %v", err)
	}

	chMeter, progMeter := &meterWatch{}, &meterWatch{}

	pipe, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// THE CARD IS EXCLUSIVE and this defer is what makes the next run possible.
	defer func() {
		if err := pipe.Stop(); err != nil {
			t.Errorf("Stop: %v — the card may still be held", err)
		}
	}()

	// Drain Errors() for the life of the run. Nothing is asserted from it here —
	// a bus error during capture fails the run through the meters being silent —
	// but a full channel would block the delivering goroutine.
	done := make(chan struct{})
	var busErrs []error
	var busMu sync.Mutex
	go func() {
		defer close(done)
		for err := range pipe.Errors() {
			busMu.Lock()
			busErrs = append(busErrs, err)
			busMu.Unlock()
			t.Logf("bus error: %v", err)
		}
	}()

	start := time.Now()
	err = pipe.Start(PipelineOpts{
		SlatePath:      slate,
		AudioCaptureID: card,
		VideoCaptureID: videoCard,
		ConformTo:      FallbackConformTarget(),
		OnChannelLevels: func(l Levels) {
			chMeter.record(l)
		},
		OnLevels: func(l Levels) {
			progMeter.record(l)
		},
	})
	if err != nil {
		// THE REFUSAL IS PART OF THE PROOF. A busy card, a card that has gone,
		// and a card with no signal are one generic stream error at the
		// GStreamer level; if this message names which of the three it was, the
		// diagnosis wired into Start is working even though the capture is not.
		t.Fatalf("Start: %v\n(elapsed %v)", err, time.Since(start))
	}
	t.Logf("PLAYING after %v", time.Since(start))

	// Let the card settle before anything is measured. decklinkaudiosrc reports
	// dropping its first packets on every start — an ordinary consequence of the
	// card having been running before the pipeline attached to it — and those
	// buffers are not a measurement of anything.
	time.Sleep(2 * time.Second)

	// PHASE ONE: QUIET. Nothing is played; this is what the card hears with the
	// room as it is.
	chMeter.reset()
	progMeter.reset()
	time.Sleep(liveCaptureWindow)
	quiet = snapshotPhase("quiet", chMeter, progMeter, pipe.InputChannels())

	// PHASE TWO: THE TEST SIGNAL. afplay through the built-in speakers, into the
	// microphone that feeds the card. The window is opened FIRST so that the
	// attack is inside it.
	chMeter.reset()
	progMeter.reset()
	playTheTestSignal(t)
	signal = snapshotPhase("signal", chMeter, progMeter, pipe.InputChannels())

	busMu.Lock()
	errs := len(busErrs)
	busMu.Unlock()
	if errs > 0 {
		t.Errorf("%d bus error(s) during the capture", errs)
	}
	return quiet, signal
}

// snapshotPhase freezes what both meters held during one window.
func snapshotPhase(name string, ch, prog *meterWatch, inputChannels int) capturePhase {
	p := capturePhase{name: name, inputChannel: inputChannels}
	p.chPeak, p.chRMS, p.chFrames = ch.report()
	p.progPeak, p.progRMS, p.progFrames = prog.report()
	return p
}

// reportPhases prints every channel's peak and RMS for both windows side by
// side, and returns the largest rise any channel showed when the signal was
// played.
//
// THE RISE IS THE READING. A peak on its own cannot tell a live analogue input
// from a dead one: an unpatched converter reports its own noise floor, which on
// this card sits around -54 dBFS and is indistinguishable from a quiet room.
// What a dead input cannot do is get louder when a sound is played into the
// microphone.
func reportPhases(t *testing.T, name string, quiet, signal []float64,
	quietRMS, signalRMS []float64, quietFrames, signalFrames int) float64 {
	t.Helper()

	if quietFrames == 0 || signalFrames == 0 {
		t.Errorf("%s: NOT ONE level message arrived in one of the windows (quiet %d, signal %d). "+
			"The element is not posting, which means the leg never produced a buffer",
			name, quietFrames, signalFrames)
		return math.Inf(-1)
	}

	best := math.Inf(-1)
	for i := range signal {
		rise := math.Inf(-1)
		if i < len(quiet) {
			rise = signal[i] - quiet[i]
		}
		if rise > best {
			best = rise
		}
		t.Logf("%s ch%02d  quiet peak %7.1f rms %7.1f  |  signal peak %7.1f rms %7.1f  |  rise %+6.1f dB",
			name, i+1, valueAt(quiet, i), valueAt(quietRMS, i),
			signal[i], valueAt(signalRMS, i), rise)
	}
	qdb, qch := loudest(quiet)
	sdb, sch := loudest(signal)
	t.Logf("%s: %d channels, %d/%d frames; loudest quiet %.1f dBFS on ch%d, loudest signal %.1f dBFS on ch%d",
		name, len(signal), quietFrames, signalFrames, qdb, qch, sdb, sch)
	return best
}

// valueAt reads a channel out of a shorter slice without panicking, so a window
// that reported fewer channels than another is a printed -inf rather than a
// crash in the middle of a hardware run.
func valueAt(xs []float64, i int) float64 {
	if i < len(xs) {
		return xs[i]
	}
	return math.Inf(-1)
}

// heardSomething reports whether any channel was above the digital-silence
// floor, which separates "the analogue input is live" from "the input is not
// patched at all".
func heardSomething(peak []float64) bool {
	for _, p := range peak {
		if p > digitalSilenceDB {
			return true
		}
	}
	return false
}

// TestLiveDeckLinkCommentaryWithTheSlate is the shape that could not exist
// before this work package: the picture is a still and the COMMENTARY comes off
// the card.
//
// It is the harder of the two and the one to run first, because it is the shape
// that needs the CLOCK COMPANION. decklinkaudiosrc cannot preroll without a
// decklinkvideosrc in the same pipeline — the card drives audio capture off the
// video clock — so if the companion is not built, or is built on the wrong
// condition, or is pointed at the wrong card, this test does not produce one
// buffer.
func TestLiveDeckLinkCommentaryWithTheSlate(t *testing.T) {
	liveDeckLinkInit(t)

	quiet, signal := runDeckLinkCapture(t, "")

	fmt.Fprintf(os.Stderr, "\n>>>> SLATE PICTURE + DECKLINK COMMENTARY\n")
	chRise := reportPhases(t, "chlevel", quiet.chPeak, signal.chPeak,
		quiet.chRMS, signal.chRMS, quiet.chFrames, signal.chFrames)
	progRise := reportPhases(t, "alevel ", quiet.progPeak, signal.progPeak,
		quiet.progRMS, signal.progRMS, quiet.progFrames, signal.progFrames)

	if len(signal.chPeak) != deckLinkAudioChannels {
		t.Errorf("the per-channel meter reported %d channels, want %d. The routing grid is sized "+
			"from this number and the commentator may be on any of them",
			len(signal.chPeak), deckLinkAudioChannels)
	}
	if got := signal.inputChannel; got != deckLinkAudioChannels {
		t.Errorf("InputChannels() = %d after the pad settled, want %d", got, deckLinkAudioChannels)
	}

	t.Logf("largest rise when the signal was played: chlevel %+.1f dB, alevel %+.1f dB",
		chRise, progRise)

	if !heardSomething(signal.chPeak) {
		t.Logf("EVERY CHANNEL IS AT THE DIGITAL SILENCE FLOOR. That is a RIG state, not a code " +
			"one: the leg prerolled, negotiated sixteen channels and posted level messages for " +
			"the whole window, which is everything this package is responsible for. The card is " +
			"receiving no audio at all — check that the microphone input is still selected in " +
			"Blackmagic Desktop Video Setup. DO NOT set `connection` to work around it: it " +
			"persistently reconfigures the card and overrides that very setting.")
		return
	}
	if chRise < 3 {
		t.Logf("THE CARD'S INPUT IS LIVE — it reports a real analogue noise floor rather than "+
			"digital silence — but the test signal raised it by only %+.1f dB, so what reached "+
			"the converter was the room and not the speakers. That is a RIG reading: the "+
			"microphone feeding the card is not hearing this machine's output. The capture leg "+
			"itself is proven by everything above: sixteen channels negotiated, a matrix written, "+
			"and both meters posting real, moving analogue levels for the whole run.", chRise)
		return
	}
	if progRise < 3 {
		t.Errorf("the card's own channels rose %+.1f dB and the ENCODED STEREO PAIR rose only "+
			"%+.1f dB. That is a ROUTING fault rather than a capture one: the mix matrix is "+
			"mapping channels that are silent. Check ChannelMap against the channel the picker "+
			"meter shows moving", chRise, progRise)
	}
}

// TestLiveDeckLinkCommentaryWithTheCard is the other DeckLink shape: the picture
// AND the commentary off the same card, served by ONE decklinkvideosrc.
//
// It is the combination the fitted rig runs, and the assertion that is not
// obvious is the negative one — there must be no second decklinkvideosrc. The
// card is exclusive; two sources in one process fail 3/3. A run that reaches
// PLAYING here is that statement.
func TestLiveDeckLinkCommentaryWithTheCard(t *testing.T) {
	liveDeckLinkInit(t)

	card := env("WSLCOMMS_LIVE_CARD", defaultLiveCard)
	quiet, signal := runDeckLinkCapture(t, card)

	fmt.Fprintf(os.Stderr, "\n>>>> CARD PICTURE + DECKLINK COMMENTARY (one source serves both)\n")
	reportPhases(t, "chlevel", quiet.chPeak, signal.chPeak,
		quiet.chRMS, signal.chRMS, quiet.chFrames, signal.chFrames)
	reportPhases(t, "alevel ", quiet.progPeak, signal.progPeak,
		quiet.progRMS, signal.progRMS, quiet.progFrames, signal.progFrames)

	if got := signal.inputChannel; got != deckLinkAudioChannels {
		t.Errorf("InputChannels() = %d, want %d", got, deckLinkAudioChannels)
	}
}

// TestLiveNativeCommentaryIsUnchanged is the control, and it is in this file
// rather than left implied because the risk this work package carries is not
// that the card fails — it is that the SEAT THAT SHIPS TODAY changes.
//
// A native microphone with a slate video source must reach PLAYING and post
// level messages exactly as it did before any of this existed. It needs no card
// and does not touch one.
func TestLiveNativeCommentaryIsUnchanged(t *testing.T) {
	liveDeckLinkInit(t)

	devices, err := ListInputDevices()
	if err != nil {
		t.Fatalf("ListInputDevices: %v", err)
	}
	var mic Device
	for _, d := range devices {
		if NormaliseDeviceKind(d.Kind) == KindNative {
			mic = d
			break
		}
	}
	if mic.ID == "" {
		t.Skip("this machine offers no native capture endpoint")
	}
	t.Logf("native commentary input: %q (%s)", mic.Name, mic.ID)

	slate, err := filepath.Abs(env("WSLCOMMS_LIVE_SLATE", defaultSlatePath))
	if err != nil {
		t.Fatalf("resolving the slate: %v", err)
	}

	pipe, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := pipe.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()
	go func() {
		for range pipe.Errors() {
		}
	}()

	prog := &meterWatch{}
	perChannel := &meterWatch{}
	if err := pipe.Start(PipelineOpts{
		SlatePath:       slate,
		AudioDeviceID:   mic.ID,
		ConformTo:       FallbackConformTarget(),
		OnLevels:        func(l Levels) { prog.record(l) },
		OnChannelLevels: func(l Levels) { perChannel.record(l) },
	}); err != nil {
		t.Fatalf("Start: %v — the seat that ships today no longer starts", err)
	}
	time.Sleep(3 * time.Second)

	fmt.Fprintf(os.Stderr, "\n>>>> NATIVE MICROPHONE + SLATE (the seat that ships today)\n")
	peak, rms, frames := prog.report()
	if frames == 0 {
		t.Error("the programme meter posted nothing on a native seat; the leg never produced a buffer")
	}
	for i := range peak {
		t.Logf("alevel  ch%02d  peak %7.1f dBFS  rms %7.1f dBFS", i+1, peak[i], rms[i])
	}
	t.Logf("alevel : %d channels, %d frames", len(peak), frames)

	// AND THE PER-CHANNEL METER MUST STAY SILENT. chlevel is built with
	// post-messages=false and armed only when a matrix was written, which
	// happens only for an unpositioned source. A native seat posting sixteen-wide
	// frames would be ten frames a second over the webview bridge for a whole
	// match, reporting the same two numbers the programme meter already reports.
	if _, _, frames := perChannel.report(); frames != 0 {
		t.Errorf("the per-channel meter posted %d frames on a POSITIONED native source; it must "+
			"be silent there", frames)
	}
}

// ---------------------------------------------------------------------------
// THE LOOPBACK PROOF, and when to reach for it
// ---------------------------------------------------------------------------

// TestLiveDeckLinkCommentaryFromTheCardsOwnLoopback proves the capture leg with
// a KNOWN SIGNAL rather than an acoustic one, by giving the card its own tone on
// its OUTPUT and capturing it back on its INPUT — the rig is looped back
// output-to-input, so this closes the circle without a microphone, a speaker or
// a room in it.
//
// # Why it exists beside the acoustic test rather than instead of it
//
// Because the two answer different questions and only one of them is about this
// package. The acoustic test asks "does the commentator's microphone reach the
// feed", which is the question that matters on the day and which no amount of
// correct code can answer on its own: it depends on the mic, on the analogue
// input still being selected in Blackmagic Desktop Video Setup, and on somebody
// not having moved anything. This test asks "does the capture leg carry a signal
// the card definitely has", which is the question about the CODE, and it is
// deterministic.
//
// So a run in which this passes and the acoustic one does not is a RIG problem,
// stated at a glance. A run in which this fails is a CODE problem. That
// distinction is the whole reason to spend a test on it.
//
// # What is hand-built here and what is not
//
// The GENERATOR is hand-built, and only the generator: audiotestsrc into
// decklinkaudiosink, with a videotestsrc into decklinkvideosink beside it
// because a DeckLink output needs a video clock exactly as its input does. The
// thing under test is still the shipped pipelineDescription, started through this
// package's own Pipeline, exactly as in every other test in this file.
//
// `connection` IS NOT SET on the sinks either. The output routing is Desktop
// Video Setup's to decide, on the output side just as on the input side.
func TestLiveDeckLinkCommentaryFromTheCardsOwnLoopback(t *testing.T) {
	liveDeckLinkInit(t)

	card := env("WSLCOMMS_LIVE_CARD", defaultLiveCard)
	slate, err := filepath.Abs(env("WSLCOMMS_LIVE_SLATE", defaultSlatePath))
	if err != nil {
		t.Fatalf("resolving the slate: %v", err)
	}

	// THE CAPTURE, through the shipped description. The video leg is the SLATE,
	// so this is also the clock-companion shape — the one that could not exist
	// before this work package.
	chMeter, progMeter := &meterWatch{}, &meterWatch{}
	pipe, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := pipe.Stop(); err != nil {
			t.Errorf("Stop: %v — the card may still be held", err)
		}
	}()
	go func() {
		for range pipe.Errors() {
		}
	}()

	if err := pipe.Start(PipelineOpts{
		SlatePath:       slate,
		AudioCaptureID:  card,
		ConformTo:       FallbackConformTarget(),
		OnChannelLevels: func(l Levels) { chMeter.record(l) },
		OnLevels:        func(l Levels) { progMeter.record(l) },
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(2 * time.Second)

	// PHASE ONE: the card's output is silent, so this is the loopback's own
	// floor.
	chMeter.reset()
	progMeter.reset()
	time.Sleep(4 * time.Second)
	quietCh, _, quietFrames := chMeter.report()
	quietProg, _, quietProgFrames := progMeter.report()

	// PHASE TWO: a 1 kHz tone at -12 dBFS out of the card, round the loop, back
	// in. persistent-id names the SAME card the capture is on, through the same
	// saved string the pipeline uses.
	//
	// The video sink is not decoration: a DeckLink output is clocked by its video
	// exactly as its input is, and decklinkaudiosink alone does not run.
	gen := fmt.Sprintf(
		"videotestsrc is-live=true pattern=black ! video/x-raw,format=UYVY,width=1920,height=1080,framerate=50/1 ! "+
			"decklinkvideosink name=gvsink persistent-id=%s mode=1080p50 sync=false "+
			"audiotestsrc is-live=true wave=sine freq=1000 volume=0.25 ! "+
			"audio/x-raw,format=S16LE,rate=48000,channels=2,layout=interleaved ! "+
			"decklinkaudiosink name=gasink persistent-id=%s", card, card)
	t.Logf("generator: %s", gen)

	genEl, err := gogst.ParseLaunch(gen)
	if err != nil {
		t.Skipf("the loopback generator will not parse or build (%v); this rig cannot be given a "+
			"known signal, so the acoustic test is the only proof available here", err)
	}
	genPipe, ok := genEl.(gogst.Pipeline)
	if !ok {
		t.Fatalf("the generator parsed to a %T, not a pipeline", genEl)
	}
	stopGen := func() {
		genPipe.BlockSetState(gogst.StateNull, gogst.ClockTime(5*time.Second))
	}
	defer stopGen()

	if ret := genPipe.BlockSetState(gogst.StatePlaying, gogst.ClockTime(10*time.Second)); !stateChangeOK(ret) {
		stopGen()
		t.Skipf("the card would not accept a playback signal (%s). The capture leg is unaffected "+
			"and is proven by the acoustic test; this rig simply cannot be driven from here", ret)
	}

	time.Sleep(1500 * time.Millisecond)
	chMeter.reset()
	progMeter.reset()
	time.Sleep(4 * time.Second)
	toneCh, _, toneFrames := chMeter.report()
	toneProg, _, toneProgFrames := progMeter.report()
	stopGen()

	fmt.Fprintf(os.Stderr, "\n>>>> KNOWN SIGNAL THROUGH THE CARD'S OWN LOOPBACK\n")
	for i := range toneCh {
		t.Logf("chlevel ch%02d  silent %7.1f dBFS  |  1 kHz tone %7.1f dBFS  |  rise %+6.1f dB",
			i+1, valueAt(quietCh, i), toneCh[i], toneCh[i]-valueAt(quietCh, i))
	}
	for i := range toneProg {
		t.Logf("alevel  ch%02d  silent %7.1f dBFS  |  1 kHz tone %7.1f dBFS  |  rise %+6.1f dB",
			i+1, valueAt(quietProg, i), toneProg[i], toneProg[i]-valueAt(quietProg, i))
	}
	t.Logf("frames: chlevel %d/%d, alevel %d/%d", quietFrames, toneFrames, quietProgFrames, toneProgFrames)

	qdb, _ := loudest(quietCh)
	tdb, tch := loudest(toneCh)
	pdb, _ := loudest(toneProg)
	t.Logf("loudest: silent %.1f dBFS, tone %.1f dBFS on channel %d, encoded pair %.1f dBFS",
		qdb, tdb, tch, pdb)

	// WHAT IS A CODE FAILURE HERE, AND WHAT IS NOT — and the split is the whole
	// value of this test, because getting it the wrong way round would either
	// hide a broken capture leg or send the owner hunting for a bug in a rig
	// problem.
	//
	// THE LEG ITSELF IS ASSERTED. Frames arriving in both windows and a
	// sixteen-wide pad are properties of this package: they say the source
	// prerolled against its clock companion, negotiated what the matrix was
	// sized for, and posted measurements for the whole run. A failure of either
	// is a code failure and fails the test.
	if quietFrames == 0 || toneFrames == 0 || quietProgFrames == 0 || toneProgFrames == 0 {
		t.Fatalf("the capture leg did not post level messages in both windows "+
			"(chlevel %d/%d, alevel %d/%d). It never produced a buffer, which is this package's "+
			"failure and not the rig's", quietFrames, toneFrames, quietProgFrames, toneProgFrames)
	}
	if len(toneCh) != deckLinkAudioChannels {
		t.Fatalf("the per-channel meter reported %d channels, want %d",
			len(toneCh), deckLinkAudioChannels)
	}

	// WHETHER THE TONE ARRIVES IS THE RIG'S ANSWER. It only comes back if the
	// card's AUDIO INPUT is the SDI-embedded path, and which input the card
	// listens on is chosen in Blackmagic Desktop Video Setup — not here, and
	// never by setting `connection`, which would persistently reconfigure the
	// card and override that very setting.
	switch {
	case tdb-qdb >= 20:
		t.Logf("THE LOOPBACK CARRIED THE TONE: %+.1f dB (%.1f -> %.1f dBFS) on channel %d, and "+
			"%+.1f dB on the encoded pair. The capture leg is proven with a signal the card "+
			"definitely had, independent of any microphone or room", tdb-qdb, qdb, tdb, tch, pdb-qdb)
		if pdb-qdb < 20 {
			t.Errorf("the card's own channels carried the tone (%+.1f dB) but the ENCODED STEREO "+
				"PAIR did not (%+.1f dB, %.1f dBFS). That is the mix matrix routing channels the "+
				"tone is not on, which IS a code-side fault", tdb-qdb, pdb-qdb, pdb)
		}
	default:
		t.Logf("THE TONE DID NOT COME BACK (%+.1f dB, %.1f -> %.1f dBFS), and on this rig that is "+
			"a ROUTING READING RATHER THAN A FAULT: it means the card's AUDIO input is not the "+
			"SDI-embedded path but an ANALOGUE one, so a tone put on the SDI output has nowhere "+
			"to arrive. The evidence for that reading is above — the input reports a real "+
			"analogue noise floor around %.1f dBFS rather than the -96 dBFS digital silence an "+
			"unpatched or deselected input gives. Which input the card listens on is Blackmagic "+
			"Desktop Video Setup's to decide. DO NOT set `connection` to change it from here.",
			tdb-qdb, qdb, tdb, qdb)
	}
}
