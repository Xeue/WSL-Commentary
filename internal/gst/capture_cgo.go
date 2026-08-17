//go:build cgo && !gststub

// capture_cgo.go is the always-live capture pipeline: the implementation behind
// CapturePipeline.
//
// It owns a device from launch to quit, ends in one or two proxysinks, and knows
// nothing at all about SRT, the encoder, the muxer or the session. Everything
// about WHY it is shaped this way is in capture.go and seam.go; this file is the
// GStreamer.
//
// # It has its own lock, its own bus handler and its own fault channel
//
// CONTRACT.md's three-way split inside internal/gst becomes four-way with this
// file, and the capture pipelines get the same stated guarantees the other three
// have. The one thing they DO share is buffers with the send pipeline, through
// the proxy — which breaks the letter of CONTRACT.md:107-110 and is an amendment
// rather than an undocumented exception.
//
// # Nothing here may take the send pipeline's lock, and nothing there may take
// this one's
//
// The two are joined by a proxysink pointer and by nothing else. A capture fault
// reaches the application on Faults(); it must NEVER reach sender.Errors(),
// because internal/sender reads any error arriving while CONNECTED as the peer
// going away and would spend a whole DRAINING/BACKOFF cycle — seven seconds off
// air — on a fault the network had nothing to do with.
package gst

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// captureStartTimeout bounds the NULL to PLAYING transition. It is the send
// pipeline's own budget, for the same reason: opening a CoreAudio endpoint or a
// DeckLink is the slow part, and a transition that has not completed in this long
// is not slow, it is stuck.
const captureStartTimeout = pipelineStartTimeout

// cgoCapture is one always-live capture pipeline.
type cgoCapture struct {
	// mu serialises Start, SetChannelMap, SetCommentaryMute and Stop against
	// each other. It is held across state changes and therefore for seconds at a
	// time; nothing on a streaming thread may wait for it.
	mu sync.Mutex

	legs CaptureLegs
	opts CaptureOpts

	// deviceKey stamps every width this pipeline publishes. See
	// CaptureOpts.DeviceKey for why an unstamped width is a measured way to
	// break a live feed.
	deviceKey string

	started bool
	stopped bool

	pipeline gogst.Pipeline
	clock    gogst.Clock
	bus      gogst.Bus

	// THE SEAM. Each queue/proxysink pair is what ArmForSend cycles and what a
	// send pipeline attaches to. Nil on the half this pipeline does not own.
	vproxq, vproxsink gogst.Element
	aproxq, aproxsink gogst.Element

	// claims is the single-consumer flag, one per proxysink. It is NOT cleared
	// by teardown: a Stop that releases the claim and then fails halfway would
	// leave a live proxysink claimable by a second consumer, which is the one
	// state the flag exists to prevent.
	claims seamClaims

	// aconv carries the mix-matrix; aconvSinkPad is the pad whose NEGOTIATED
	// caps size it. The pad is held rather than looked up per call because
	// InputChannels is a UI-facing read that arrives many times a second while
	// the routing panel is open.
	//
	// sinkPadMu guards THE PAD ONLY, and it exists so that read does not have to
	// take mu. Measured: with mu held, a concurrent InputChannels() blocked for
	// 302 ms — and mu is held across BlockSetState, whose budget is
	// captureStartTimeout and which buildLocked can run TWICE on the
	// retry-without-preview path. A routing panel polling a width through a device
	// change would freeze for the length of the device open. CommentaryMuted was
	// given an atomic for exactly this argument; this is the same argument about
	// the same panel.
	//
	// LOCK ORDER: mu then sinkPadMu, never the other way. Nothing may take mu
	// while holding this.
	aconv        gogst.Element
	sinkPadMu    sync.RWMutex
	aconvSinkPad gogst.Pad
	capsProbeID  uint32

	// matrixWidth is the width the matrix currently in force was built for, and
	// 0 when none has been written. The MAP is deliberately not kept beside it:
	// mix-matrix is a GST_TYPE_ARRAY and does not marshal back, so a stored copy
	// would be a second, unverifiable account of the same fact.
	matrixWidth int

	// deviceWidth is the width this build RESOLVED for the commentary device —
	// the card's constant 16, the enumeration's count, or the source pad's fixed
	// count — whether or not a matrix could be written for it. It is what arms
	// the per-channel picker before anything has negotiated; matrixWidth cannot
	// do that job, because a device whose width is known but whose matrix was
	// refused would leave the picker dark behind a grid of bars that can never
	// move.
	deviceWidth int

	// publishedWidth is the last width handed to OnInputChannels, so the CAPS
	// probe does not republish the same number on every renegotiation. Atomic
	// because the writer is a streaming thread.
	publishedWidth atomic.Int64

	// widths carries a negotiated width from the CAPS probe's STREAMING THREAD to
	// the goroutine that hands it to the application.
	//
	// The hop is what removes a lock inversion, and the inversion is not
	// theoretical: teardownLocked removes the CAPS probe while holding mu, and
	// gst_pad_remove_probe blocks until a running callback returns — so a callback
	// that re-entered this pipeline (InputChannels, SetChannelMap,
	// SetCommentaryMute all take mu) would deadlock both threads. Re-entry is the
	// OBVIOUS implementation of OnInputChannels: its whole job is to publish a
	// width the application then sizes and applies routing against.
	widths chan capturedWidth

	// cough is the volume element and muted is the READ-BACK answer
	// CommentaryMuted gives. Two fields rather than one for the reason gst.go's
	// twin gives: cough is reached only under mu, and muted is read without it,
	// because "is the microphone open" must go on being answerable while mu is
	// held across a device change.
	cough gogst.Element
	muted atomic.Bool

	// sigWatch is the video signal watchdog, or nil when this pipeline has no
	// element with a "signal" property to poll. Stop on nil does nothing, so
	// there is one unconditional call at teardown.
	sigWatch *signalWatch

	// busSilenced makes onBusMessage return immediately once teardown has begun.
	// The sync handler is never detached; see teardownLocked.
	busSilenced atomic.Bool

	onLevels        atomic.Pointer[func(Levels)]
	onChannelLevels atomic.Pointer[func(Levels)]
	onInputChannels atomic.Pointer[func(string, int)]

	// fatal latches this capture's death. Guarded by errMu and not by mu,
	// because it is written by onBusMessage on a streaming thread while mu may
	// be held for the whole of a device change.
	//
	// It is CLEARED by teardownLocked, and that is load-bearing rather than
	// tidiness: buildLocked's abort() runs teardownLocked, and Start may build a
	// second time without the preview. A latch left standing from the first
	// attempt makes the second one abort on a pipeline that is perfectly healthy —
	// the seat comes up with no capture at all, no meters and no commentary,
	// instead of coming up without a confidence monitor.
	errMu sync.RWMutex
	fatal error

	// pictureFatal latches a PICTURE-LEG death on a pipeline whose commentary is
	// still running. It is a second field rather than a second use of fatal
	// because the two have different consequences and only one of them is
	// recoverable in place: the commentary goes on being captured, metered and
	// muted, and RestartCapture is the repair.
	//
	// IT EXISTS BECAUSE A PICTURE DEATH USED TO BE INVISIBLE. classVideoCapture
	// reached deliverWarning and nothing else: Health() answered nil, ArmForSend's
	// latched-fault refusal did not fire, START reached PLAYING over a dead card
	// and the operator was refused two seconds later by the muxer's "nothing
	// reached vq:src" — the pad, not the cause — while capturefault.go's named
	// diagnosis of that exact fault had already been produced and thrown away one
	// layer down.
	//
	// It is CLEARED by teardownLocked for the reason fatal is: buildLocked's
	// abort() runs teardownLocked and Start may build a second time without the
	// preview.
	pictureFatal error

	errs chan error

	// warns is what deliverWarning writes from the streaming thread, and warnsOut
	// is what Warnings() hands the application. THEY ARE TWO CHANNELS AND NOT ONE,
	// because logWarnings ranges over the first: a single channel with an internal
	// logger AND an application consumer on it is a channel whose lines are SPLIT
	// between them at random, so a confidence-monitor failure would reach the log
	// or the screen and never both, and which one got it would change with the
	// scheduler. logWarnings closes warnsOut as it exits, so every line is
	// delivered before the close is.
	warns      chan string
	warnsOut   chan string
	errsClosed bool
}

// capturedWidth is one negotiated width on its way from the CAPS probe to the
// application, stamped with the device it belongs to. See cgoCapture.widths.
type capturedWidth struct {
	deviceKey string
	channels  int
}

var _ CapturePipeline = (*cgoCapture)(nil)

// NewCapture validates the options and returns an unstarted capture pipeline.
// Nothing is parsed and no device is opened until Start.
//
// Every refusal here is a CONFIGURATION failure, and each one is refused before
// anything is built so that the message names the value rather than an element.
//
// EVERY PIPELINE THIS RETURNS MUST EVENTUALLY BE STOPPED, including one whose
// Start failed and one that is abandoned without ever being started — the second
// leg of a planned pair whose first leg would not open, for instance. Two
// goroutines are parked on this object's channels from here (the warning log and
// the width publisher) and Stop is the only thing that closes them. Stop on a
// pipeline that was never started is cheap and does not fail.
func NewCapture(opts CaptureOpts) (CapturePipeline, error) {
	// THE SAME GUARD New, ListInputDevices, the picture monitor and the return
	// monitor all open with, and it is not a formality here.
	//
	// `inited` false does not only mean "a required element is missing". Several
	// of doInit's failure paths return BEFORE gst_init is ever called — a plugin
	// directory that cannot be read, a Setenv that will not take, a registry path
	// that cannot be created — so on a bundle whose Contents/Resources/gstreamer-1.0
	// is missing, this is reached with GStreamer never initialised at all, and
	// gst_parse_launch against that is undefined behaviour rather than an error
	// return. The window used to come up with main.go's named banner and START
	// refused by name; the always-live layer builds at domReady instead, which is
	// what made this the FIRST thing to touch GStreamer on such a machine.
	if !inited.Load() {
		return nil, errors.New("gst: NewCapture: Init has not been called")
	}
	if err := opts.Legs.Valid(); err != nil {
		return nil, err
	}
	if opts.Legs.Picture == PictureSlate && opts.SlatePath == "" {
		return nil, errors.New("gst: CaptureOpts.SlatePath is required when the picture leg is " +
			"the slate")
	}
	if opts.Legs.Picture == PictureCard {
		if _, err := parseDeckLinkPersistentID(opts.VideoCaptureID); err != nil {
			return nil, fmt.Errorf("gst: CaptureOpts.VideoCaptureID names the DeckLink card whose "+
				"input becomes the picture, and it must be a DeckLink persistent-id rather than "+
				"an audio device id: %w", err)
		}
		if missing := missingFrom(videoCaptureRequiredElements); len(missing) > 0 {
			return nil, fmt.Errorf("gst: a DeckLink video input is configured but this build's "+
				"GStreamer cannot build the capture leg: %s. The commentary is unaffected; "+
				"clearing the video input setting starts this seat with the slate",
				strings.Join(missing, ", "))
		}
	}
	if opts.Legs.Commentary != CommentaryNone {
		// THE ONE-SOURCE RULE, and it is the dangerous half that matters: an
		// EMPTY device on osxaudiosrc or wasapi2src is not an error, it is THE
		// SYSTEM DEFAULT INPUT, which is how a match goes on air off the
		// laptop's built-in microphone with every lamp green.
		if err := refuseWrongAudioSource(opts.AudioDeviceID, opts.AudioCaptureID); err != nil {
			return nil, err
		}
		if opts.Legs.Commentary == CommentaryCard {
			if missing := missingFrom(audioCaptureRequiredElements); len(missing) > 0 {
				return nil, fmt.Errorf("gst: the commentary input is a DeckLink card but this "+
					"build's GStreamer cannot build the capture leg: %s. Choose a microphone in "+
					"the Commentary input dropdown to start this seat",
					strings.Join(missing, ", "))
			}
			// ONE CARD. The card is exclusive and drives audio capture off the
			// VIDEO clock, so two ids naming different cards describes a pipeline
			// whose audio would wait for a clock that never starts for it. There
			// is no third element available to clock it.
			if opts.Legs.Picture == PictureCard && opts.VideoCaptureID != opts.AudioCaptureID {
				return nil, fmt.Errorf("gst: the picture is card %s and the commentary is card "+
					"%s. A DeckLink drives audio capture off the VIDEO clock and the card is "+
					"exclusive, so both legs must name the same card or the picture must be the "+
					"slate", opts.VideoCaptureID, opts.AudioCaptureID)
			}
		}
	}

	c := &cgoCapture{
		legs:      opts.Legs,
		opts:      opts,
		deviceKey: opts.DeviceKey(),
		claims:    newSeamClaims(opts.Legs),
		errs:      make(chan error, errorChannelBuffer),
		warns:     make(chan string, warningChannelBuffer),
		warnsOut:  make(chan string, warningChannelBuffer),
		widths:    make(chan capturedWidth, warningChannelBuffer),
	}
	// The callbacks are published BEFORE anything GStreamer exists. The level
	// elements start posting the moment the pipeline reaches PLAYING, which
	// happens while mu is held — so onBusMessage, on a streaming thread that must
	// not take mu, may read these while Start is still executing.
	if opts.OnLevels != nil {
		cb := opts.OnLevels
		c.onLevels.Store(&cb)
	}
	if opts.OnChannelLevels != nil {
		cb := opts.OnChannelLevels
		c.onChannelLevels.Store(&cb)
	}
	if opts.OnInputChannels != nil {
		cb := opts.OnInputChannels
		c.onInputChannels.Store(&cb)
	}
	go c.logWarnings()
	go c.publishWidths()
	return c, nil
}

// Legs reports the shape. It never changes.
func (c *cgoCapture) Legs() CaptureLegs { return c.legs }

// Start builds the pipeline and takes it to PLAYING.
//
// The retry-without-preview is here rather than in the application, and it moved
// with the preview: a sink that EXISTS and then will not START — no GL context, a
// display that has gone away — fails inside the state change, which is not a bus
// error, so none of the sparing that protects the preview elsewhere applies.
// Building once more without the branch can only turn a failure into a success,
// and it is now retryable by the operator through RestartCapture rather than
// being a decision for the whole process.
func (c *cgoCapture) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		return errors.New("gst: capture pipeline is stopped")
	}
	if c.started {
		return errors.New("gst: capture pipeline already started")
	}

	conform, fallbackReason := c.opts.ConformTo.resolve()
	if fallbackReason != "" {
		log.Printf("gst: capture: conform target %v is unusable (%s); falling back to %v. If this "+
			"instance is not configured for %v the video leg will be refused",
			c.opts.ConformTo, fallbackReason, conform, conform)
	}

	preview := ""
	if c.legs.Preview {
		// previewBranchFor takes the video-capture id because it must never
		// render a branch against a tee the slate leg does not build — that is a
		// gst_parse_launch failure of the whole pipeline rather than a missing
		// preview. It is resolved ONCE, here, so a rebuild below does not ask the
		// registry for a sink a second time.
		preview = previewBranchFor(c.opts.Preview, c.opts.VideoCaptureID)
	}

	if err := c.buildLocked(conform, preview); err != nil {
		if preview == "" {
			return err
		}
		log.Printf("gst: capture: the pipeline would not start with the confidence monitor in it "+
			"(%v); rebuilding WITHOUT it. The commentary is the product and the preview is not", err)
		return c.buildLocked(conform, "")
	}
	return nil
}

// buildLocked parses, points every element at what it is to open, and takes the
// pipeline to PLAYING. mu is held; Start is its only caller, and it may call it
// twice — every path out of it that is not success goes through abort(), which
// runs teardownLocked and leaves the second attempt starting where the first did.
func (c *cgoCapture) buildLocked(conform ConformTarget, preview string) error {
	desc := captureDescription(c.legs, conform, preview)
	log.Printf("gst: capture %s: gst_parse_launch:\n%s", c.legs, desc)

	element, err := gogst.ParseLaunch(desc)
	if err != nil {
		return fmt.Errorf("gst: capture: gst_parse_launch failed: %w", err)
	}
	pipeline, ok := element.(gogst.Pipeline)
	if !ok || pipeline == nil {
		return fmt.Errorf("gst: capture: gst_parse_launch returned a %T, not a GstPipeline", element)
	}
	c.pipeline = pipeline

	// From here on a failure must tear down what has been built, or the device
	// stays open and the next attempt cannot have it.
	abort := func(err error) error {
		c.teardownLocked()
		return err
	}

	// ---- the picture leg -------------------------------------------------
	switch c.legs.Picture {
	case PictureSlate:
		slate := pipeline.GetByName(nameSlateSrc)
		if slate == nil {
			return abort(errNoElement(nameSlateSrc, "the slate picture leg was not built"))
		}
		if err := setStringProperty(slate, "location", c.opts.SlatePath); err != nil {
			return abort(err)
		}

	case PictureCard:
		vsrc := pipeline.GetByName(nameVideoCaptureSrc)
		if vsrc == nil {
			return abort(errNoElement(nameVideoCaptureSrc, "the card picture leg was not built"))
		}
		// The SAME setter the audio source and the clock companion go through.
		// The card publishes ONE persistent-id for its audio and video entries
		// alike, and routing every decklink element through one function is what
		// stops three of them growing different rules about the identical saved
		// string. Note what is NOT called anywhere near here: nothing sets
		// `connection`.
		if err := configureDeckLinkSource(vsrc, c.opts.VideoCaptureID); err != nil {
			return abort(err)
		}
	}

	// add-borders, ON WHICHEVER LEG WAS BUILT, so that artwork or a camera raster
	// which is not the conform target's aspect ratio is letterboxed rather than
	// stretched. videoscale's own default is already true, so this is a guard
	// against the default changing rather than a change — and the name is chosen
	// from the same condition that chose the leg rather than by trying both, so a
	// miss is a defect worth logging instead of a false alarm about a scaler the
	// pipeline was never asked to build.
	//
	// It matters more on the CARD leg than on the slate, which is the leg that
	// lost it when this build sequence was first written: a 16:9 camera into a 4:3
	// switcher configuration is a real facility, and stretching faces is the kind
	// of fault nobody reports as a fault. The write is behind hasProperty because
	// a write against an element without it is a GLib CRITICAL on stderr, where no
	// shipped build is looking.
	if c.legs.Picture != PictureNone {
		scaleName := nameVideoScale
		if c.legs.Picture == PictureCard {
			scaleName = nameVideoCapScale
		}
		switch vscale := pipeline.GetByName(scaleName); {
		case vscale == nil:
			log.Printf("gst: capture: no element named %s; a picture that is not exactly %dx%d "+
				"will fail caps negotiation", scaleName, conform.Width, conform.Height)
		case !hasProperty(vscale, "add-borders"):
			log.Printf("gst: capture: %s has no add-borders property; a picture that is not the "+
				"conform target's aspect ratio will be stretched", scaleName)
		default:
			vscale.SetObjectProperty("add-borders", true)
		}
	}

	// ---- the commentary leg ----------------------------------------------
	if c.legs.Commentary != CommentaryNone {
		asrc := pipeline.GetByName(nameAudioSrc)
		if asrc == nil {
			return abort(errNoElement(nameAudioSrc, "the commentary leg was not built"))
		}
		if c.legs.Commentary == CommentaryCard {
			if err := configureDeckLinkSource(asrc, c.opts.AudioCaptureID); err != nil {
				return abort(err)
			}
			// THE CLOCK COMPANION'S CARD, through the SAME setter, on the one
			// shape that has one. It must open the SAME CARD as the audio source:
			// a companion on a different card would clock nothing and the audio
			// would never preroll — the measured signature is 0 buffers and 0
			// level messages against 160, with nothing on either bus.
			//
			// Left unset, persistent-id keeps the element's own -1, which means
			// "use device-number", which means whichever card the driver
			// enumerated first. On a single-card rig that is the card and it works
			// by accident; on a two-card rig it opens and holds a card nobody
			// selected, from launch to quit, and clocks the commentary with
			// nothing. persistent-id is read on NULL->READY, so this is the last
			// moment it can be written.
			if c.legs.NeedsClockCompanion() {
				clock := pipeline.GetByName(nameVideoCaptureClock)
				if clock == nil {
					return abort(errNoElement(nameVideoCaptureClock, "there is nothing to clock the "+
						"DeckLink commentary capture and it would never produce a buffer"))
				}
				if err := configureDeckLinkSource(clock, c.opts.AudioCaptureID); err != nil {
					return abort(err)
				}
			}
		} else {
			// The id is logged VERBATIM immediately before it is handed to the
			// element. On Windows wasapi2src echoes it in its asynchronous error
			// 1551, so a later "Failed to open device {...}" is matched to what
			// was requested; on macOS it is a CoreAudio unique-id that has to be
			// resolved to an integer before osxaudiosrc will take it, and this is
			// the only record of which id the resolution was asked to find.
			log.Printf("gst: capture: %s device id: %s", captureSourceFactory, c.opts.AudioDeviceID)
			if err := configureCaptureSource(asrc, c.opts.AudioDeviceID); err != nil {
				return abort(err)
			}
		}

		// THE MATRIX, written HERE — after the source has been pointed at a
		// device and BEFORE the pipeline leaves NULL. Not earlier, because the
		// source cannot say what it will produce until it knows which device it
		// is. Not later, because a matrix is a NEGOTIATION CONSTRAINT and not a
		// gain: audioconvert cannot map unpositioned channels to stereo without
		// one, so a pipeline that reaches PLAYING first never reaches it at all.
		// Measured — decklinkaudiosrc channels=16 into this chain with no matrix
		// dies 0.069 s after PLAYING with "streaming stopped, reason
		// not-negotiated (-4)".
		c.aconv = pipeline.GetByName(nameAudioConv)
		if c.aconv == nil {
			return abort(errNoElement(nameAudioConv, "there is nowhere to write the channel map"))
		}
		pad := c.aconv.GetStaticPad("sink")
		if pad == nil {
			return abort(errNoPad(nameAudioConv, "sink", "the negotiated width cannot be read"))
		}
		c.setAconvSinkPad(pad)
		if err := c.applyStartChannelMapLocked(asrc); err != nil {
			return abort(err)
		}
		// The width publisher. A CAPS event on this pad is negotiation's own
		// answer arriving, which is the earliest moment the routing panel can be
		// sized correctly — measured on the fitted card at t=0.1176 s, with no
		// consumer pipeline, no encoder and no SRT anywhere in the process.
		if err := c.watchNegotiatedWidthLocked(); err != nil {
			return abort(err)
		}

		// THE COUGH MUTE, applied while still in NULL. A pipeline asked to start
		// muted must be muted before it can produce a buffer.
		//
		// A missing element ABORTS, and that is a deliberate departure from the
		// way a missing chlevel is survived below. A meter that does not move is
		// a diagnosis problem; a cough button that does nothing is a commentator
		// who believes they are off air and is not.
		c.cough = pipeline.GetByName(nameCoughMute)
		if c.cough == nil {
			return abort(errors.New("gst: parsed capture pipeline has no element named " +
				nameCoughMute + ", so there is no cough mute on this feed and the buttons that " +
				"drive it would do nothing while reporting success"))
		}
		if err := c.applyCoughMuteLocked(c.opts.MuteCommentary); err != nil {
			return abort(err)
		}

		// ARM THE PICKER METER. The condition is PLAN.md 4.3's, and it is not the
		// old one: chlevel is armed when the pad presents MORE channels than the
		// two the programme meter already reports, not when a matrix happened to
		// be written. With a uniform matrix at every width the old condition
		// would arm sixteen bars on a stereo microphone — a duplicate of the
		// programme meter, ten times a second, over the webview bridge, for the
		// whole of a ninety-minute match.
		//
		// It is deviceWidth and NOT matrixWidth, because nothing has negotiated
		// yet and the two differ in the one case that matters: a device whose
		// width is known but for which no matrix could be written would leave the
		// picker silenced behind a grid of bars the panel HAS sized and which can
		// never move. The decision is retaken against the pad's real answer once
		// the pipeline is PLAYING, below.
		c.armChannelMeterLocked(pipeline, c.wantChannelMeter(c.deviceWidth))
	}

	// ---- the seam --------------------------------------------------------
	if c.legs.Picture != PictureNone {
		if c.vproxq, err = mustGet(pipeline, nameVideoProxyQueue); err != nil {
			return abort(err)
		}
		if c.vproxsink, err = mustGet(pipeline, nameVideoProxySink); err != nil {
			return abort(err)
		}
	}
	if c.legs.Commentary != CommentaryNone {
		if c.aproxq, err = mustGet(pipeline, nameAudioProxyQueue); err != nil {
			return abort(err)
		}
		if c.aproxsink, err = mustGet(pipeline, nameAudioProxySink); err != nil {
			return abort(err)
		}
	}

	// The preview sink's surface and the properties that could not safely go in
	// the parse string. It MUST happen before the pipeline leaves NULL: a
	// GstVideoOverlay with no window handle makes its OWN top-level window, with
	// a title bar and a close button, over the commentator's screen.
	if err := attachPreview(pipeline, c.opts.Preview, preview); err != nil {
		return abort(err)
	}

	// The bus sync handler is attached before the first state change so that an
	// error raised during NULL->PLAYING is captured rather than lost. It is a
	// sync handler rather than a watch because a watch needs a GLib main loop and
	// this process does not have one — Wails owns the message loop.
	c.bus = pipeline.GetBus()
	if c.bus == nil {
		return abort(errors.New("gst: capture pipeline has no bus"))
	}
	c.busSilenced.Store(false)
	c.bus.SetSyncHandler(c.onBusMessage)

	// THE CLOCK AND THE BASE TIME, before any state change, in this order.
	// gstproxysrc.c:51-67 states it as a requirement of the element: the capture
	// pipelines and the send pipeline must share a clock and a base time or the
	// consumer's running time is not the producer's.
	//
	// SetStartTime(NONE) is what makes SetBaseTime survive — gst_pipeline_change_state
	// only recomputes base time when start time is valid, and a probe that omitted
	// this measured base time being overwritten on the transition to PLAYING.
	c.clock = gogst.SystemClockObtain()
	if c.clock == nil {
		return abort(errors.New("gst: gst_system_clock_obtain returned nil"))
	}
	pipeline.UseClock(c.clock)
	pipeline.SetStartTime(gogst.ClockTimeNone)

	savedBaseMu.Lock()
	if savedBase == gogst.ClockTimeNone {
		savedBase = c.clock.GetTime()
		log.Printf("gst: sampled the process-lifetime base time: %d ns", uint64(savedBase))
	}
	base := savedBase
	savedBaseMu.Unlock()
	pipeline.SetBaseTime(base)

	stop := stateChangeWatchdog("capture pipeline NULL to PLAYING (opening the capture device)")
	ret := pipeline.BlockSetState(gogst.StatePlaying, gogst.ClockTime(captureStartTimeout))
	stop()
	if !stateChangeOK(ret) {
		err := fmt.Errorf("gst: capture pipeline would not go to PLAYING (%s)", ret)
		// A bus error arriving during the transition has ALREADY been diagnosed
		// by onBusMessage against the card's own evidence, so the named sentence
		// exists. %v and not %w deliberately: this is a build failure the caller
		// handles by not having a capture, and putting ErrPipelineFatal at the
		// head of it would tell a reader there was a running pipeline whose chain
		// had died.
		if fatal := c.fatalError(); fatal != nil {
			err = fmt.Errorf("%w: %v", err, fatal)
		} else if c.legs.HasCard() {
			// The transition failed with nothing on the bus and a card is in this
			// graph. The evidence is still there to be read — the card's own
			// signal property above all — so the diagnosis runs here too, and it
			// degrades to naming the three things to check in order, which is
			// what an operator can act on and "(failure)" is not.
			//
			// WHICH ELEMENT IS INTERROGATED matters and is not always asrc: a
			// picture-only card pipeline has no commentary source at all, and
			// gatherCaptureEvidence would then be handed nothing and answer from
			// no evidence. The video source is the element that knows whether the
			// card is locked either way — it is what the evidence gatherer walks
			// to from asrc anyway — so it is the fallback here.
			el, name := pipeline.GetByName(nameAudioSrc), nameAudioSrc
			if el == nil {
				el, name = videoCaptureElement(pipeline), nameVideoCaptureSrc
			}
			if el != nil {
				err = fmt.Errorf("%w: %v", err, captureFatalError(el, name, err))
			}
		}
		return abort(err)
	}

	// BlockSetState reporting success is NOT proof the capture chain is healthy.
	// wasapi2src opens its endpoint on its own thread and posts failure
	// asynchronously — error 1551 — so NULL->PLAYING can return success while
	// onBusMessage has already latched a fatal.
	if err := c.fatalError(); err != nil {
		return abort(err)
	}

	// CONFIRM THE MATRIX AGAINST WHAT THE PAD REALLY NEGOTIATED. The width the
	// matrix was built for came from a query made before negotiation; this comes
	// from the pad's CURRENT CAPS, which is negotiation's own answer. A ZERO is
	// not a failure — the source is live, so "the pad has not settled yet" is an
	// ordinary race — but a DISAGREEMENT is, because a matrix of the wrong width
	// does not degrade the feed, it stops the capture chain with a flow error
	// naming the source rather than the matrix.
	if c.matrixWidth > 0 {
		switch got := c.negotiatedInputChannels(); {
		case got == 0:
			log.Printf("gst: capture: the %s matrix was written for %d input channels; the pad has "+
				"not published its negotiated caps yet, so the width is confirmed on the first "+
				"InputChannels call rather than here", nameAudioConv, c.matrixWidth)
		case got != c.matrixWidth:
			return abort(fmt.Errorf("gst: the %s mix-matrix was built for %d input channels and "+
				"%s:sink negotiated %d. A matrix of the wrong width does not attenuate or "+
				"misroute, it stops the capture chain with a flow error naming the source rather "+
				"than the matrix, so this is refused here while it can still be read as what it is",
				nameAudioConv, c.matrixWidth, nameAudioConv, got))
		default:
			log.Printf("gst: capture: %s:sink negotiated %d input channels, matching the matrix",
				nameAudioConv, got)
		}
	}

	// AND RETAKE THE PICKER'S ARMING AGAINST THE PAD'S OWN ANSWER. PLAN.md 4.3
	// states the condition against the NEGOTIATED width, and until this moment
	// there was nothing negotiated to state it against. It matters in both
	// directions and only here: a device that presents more channels than it was
	// believed to would otherwise have a sized grid with dead bars, and one that
	// presents fewer would post sixteen rms entries a message for a stereo
	// microphone. A zero means the pad has not settled and the build-time decision
	// stands.
	if c.legs.Commentary != CommentaryNone {
		if got := c.negotiatedInputChannels(); got > 0 && got != c.deviceWidth {
			log.Printf("gst: capture: %s:sink negotiated %d input channels against the %d this "+
				"build resolved; re-deciding the per-channel meter against the pad",
				nameAudioConv, got, c.deviceWidth)
			c.armChannelMeterLocked(pipeline, c.wantChannelMeter(got))
		}
	}

	// THE VIDEO SIGNAL WATCHDOG, last, so that no abort() path above can leak its
	// goroutine. Its lifecycle now belongs to whichever capture pipeline owns a
	// decklinkvideosrc — vcapsrc on a card picture leg, vcapclock on a slate seat
	// whose COMMENTARY is the card — and it is stopped AND JOINED before that
	// pipeline goes to NULL, because its reader closure holds the element and a
	// property read against a disposed element is a read on freed memory.
	if vsrc := videoCaptureElement(pipeline); vsrc != nil {
		probe := boolPropertyTriState(vsrc, propSignal)
		if signalWatchWanted(c.opts.OnSignal, probe) {
			c.sigWatch = startSignalWatch(probe,
				func() triState { return boolPropertyTriState(vsrc, propSignal) },
				c.opts.OnSignal)
			log.Printf("gst: capture: watching %s for input lock every %v; the card's own signal "+
				"property is the only thing in this process that can tell",
				vsrc.GetName(), signalPollInterval)
		}
	}

	c.started = true
	log.Printf("gst: capture %s PLAYING; base time %d ns, seam %v",
		c.legs, uint64(base), c.ProxySinks())
	return nil
}

// ---------------------------------------------------------------------------
// The seam
// ---------------------------------------------------------------------------

// ProxySinks names the proxysinks this pipeline owns, video then audio.
func (c *cgoCapture) ProxySinks() []string { return c.claims.names() }

// ClaimForSend takes the single-consumer claim on every proxysink this pipeline
// owns, all or nothing.
func (c *cgoCapture) ClaimForSend() error { return c.claims.claimAll() }

// seamSinks hands the send side the two elements it binds its proxysrcs to, UNDER
// mu, or refuses.
//
// Every other cross-goroutine access to these fields is guarded and this one has
// to be too: teardownLocked nils them while holding mu. The refusals matter as
// much as the lock — a capture that is stopped, unstarted or already failed still
// has a non-nil-looking object graph for a moment, and a send pipeline bound to
// one of those reaches PLAYING attached to a producer that will never push,
// which reads to the operator as a connected feed carrying nothing.
func (c *cgoCapture) seamSinks() (video, audio gogst.Element, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch {
	case c.stopped:
		return nil, nil, errors.New("gst: this capture pipeline is stopped, so there is no " +
			"proxysink for a send pipeline to attach to")
	case !c.started:
		return nil, nil, errors.New("gst: this capture pipeline has not been started, so its " +
			"proxysinks would push nothing at a send pipeline that reached PLAYING regardless")
	}
	// EITHER LEG'S LATCHED DEATH REFUSES. A dead picture is as complete a stop as
	// a dead commentary is: mpegtsmux emits nothing at all while one of its two
	// inputs is silent, so the feed carries zero bytes either way.
	if err := c.anyFatalError(); err != nil {
		return nil, nil, fmt.Errorf("gst: this capture pipeline has already failed, so binding a "+
			"send pipeline to it would produce a connected feed carrying zero bytes: %w", err)
	}
	return c.vproxsink, c.aproxsink, nil
}

// ReleaseFromSend gives the claims back. Idempotent.
func (c *cgoCapture) ReleaseFromSend() { c.claims.releaseAll() }

// Health is nil while this capture is carrying media, and the latched fault
// otherwise. It is the question the SEND side has no other way to ask.
//
// Faults() cannot answer it: it is a channel, the application has already drained
// it, and a second reader would steal the diagnosis. A latched death does not
// clear started or set stopped — the pipeline object is intact and every method
// on it still works — so without this a START after the card was unplugged claims
// the seam, arms an idle-because-dead pad, binds, reaches PLAYING and leaves only
// the 2 s liveness gate between the operator and a green lamp over silence.
func (c *cgoCapture) Health() error { return c.fatalError() }

// PictureHealth is nil while this pipeline's PICTURE leg is carrying media and
// the latched, NAMED fault once it has died. See the pictureFatal field.
func (c *cgoCapture) PictureHealth() error { return c.pictureFatalError() }

// ArmForSend runs the READY cycle over every proxysink this pipeline owns.
//
// It takes mu because it touches elements a teardown may be dropping, and it
// refuses on a pipeline that is not PLAYING: arming a graph in NULL would be a
// no-op that reported success, and the whole value of arming at START is that it
// cannot be skipped.
func (c *cgoCapture) ArmForSend() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		return errors.New("gst: capture pipeline is stopped, so its seam cannot be armed")
	}
	if !c.started {
		return errors.New("gst: capture pipeline has not been started, so its seam cannot be armed")
	}
	// A LATCHED FAULT IN EITHER LEG REFUSES THE ARMING, and this is the only place
	// the send side's build can be stopped by it. On a dead device the IDLE probe
	// fires immediately — the pad is idle BECAUSE it is dead — so the arming reports
	// success over a proxysink with nothing behind it, and PLAN.md step 6's
	// liveness gate is left as the backstop for a MISSED arming rather than for an
	// arming that succeeded over a corpse.
	//
	// The PICTURE leg counts here even though it is recoverable in place, because
	// this is the send side asking: mpegtsmux emits nothing at all while one of its
	// two inputs is silent, so a START over a dead picture reaches PLAYING and is
	// refused two seconds later by a message about a pad rather than by the named
	// card fault this pipeline already holds.
	if err := c.anyFatalError(); err != nil {
		return fmt.Errorf("gst: this capture pipeline has already failed, so arming its seam would "+
			"report success over a device that is producing nothing: %w", err)
	}

	type branch struct {
		queue, sink gogst.Element
	}
	var branches []branch
	if c.vproxsink != nil {
		branches = append(branches, branch{c.vproxq, c.vproxsink})
	}
	if c.aproxsink != nil {
		branches = append(branches, branch{c.aproxq, c.aproxsink})
	}
	if len(branches) == 0 {
		return errors.New("gst: this capture pipeline owns no proxysink to arm")
	}

	total := time.Duration(0)
	for _, b := range branches {
		took, err := armProxySink(b.queue, b.sink)
		if err != nil {
			return err
		}
		total += took
	}
	log.Printf("gst: capture %s: armed %v in %s", c.legs, c.ProxySinks(), total)
	return nil
}

// ---------------------------------------------------------------------------
// Channel routing
// ---------------------------------------------------------------------------

// applyStartChannelMapLocked writes the first mix-matrix, UNIFORMLY, at whatever
// width this commentary source presents. mu is held and the pipeline is in NULL.
//
// # The old stereo-probe discriminator is gone, and its removal is the R2 unblock
//
// It decided whether to write a matrix by intersecting the source pad's caps with
// channels=2 and writing nothing when the intersection was non-empty.
// osxaudiosrc's src template is `channels: [1, 2147483647]`, so that intersection
// is NEVER empty and NO MATRIX WAS EVER WRITTEN ON THE NATIVE PATH — which is
// why the routing panel could only ever appear for a DeckLink.
//
// Settled from GStreamer's source rather than inferred: gstosxcoreaudio.c:886-889
// sets `layout = NULL; /* no supported for sources */` unconditionally for EVERY
// source, so no CoreAudio source can emit a positioned channel-mask above two
// channels, for any device. A Focusrite or an RME is byte-for-byte the same
// problem as the card. Corroborated on this machine: a 3-channel CoreAudio device
// negotiated channels=3 channel-mask=0x0 and a 16-in device negotiated
// channels=16 channel-mask=0x0, identical in shape to decklinkaudiosrc's 16.
//
// So the matrix is uniform — written at width 1, 2, 3, 16 and 32 alike, all
// measured working — and there is no source-kind test anywhere in this package.
//
// # Where the width comes from, in order
//
//  1. A DeckLink commentary is the constant 16, BY CONSTRUCTION: the description
//     sets channels=16 on the element, so the number is not discovered, it is
//     stated.
//  2. CaptureOpts.DeviceChannels, which the caller read from the enumerated
//     GstDevice's caps during the ordinary provider walk. That is a QUERY and
//     opens nothing.
//  3. The source pad's own fixed count, for a caller that supplied neither.
//
// A width that cannot be established writes NOTHING and says so. That is the
// PROVIDER-LIED path's entry point (PLAN.md 4.2 step 4): with a matrix set,
// audioconvert pins its sink caps to the matrix width, so a wrong width shows up
// as a preroll failure rather than as a different negotiated number, and the
// recovery is a throwaway `<factory> ! fakesink` probe pipeline off air. Until
// that lands, no matrix is better than a guessed one.
func (c *cgoCapture) applyStartChannelMapLocked(asrc gogst.Element) error {
	factory := elementFactoryNameOf(asrc)

	width := 0
	switch {
	case c.legs.Commentary == CommentaryCard:
		width = deckLinkAudioChannels
	case c.opts.DeviceChannels > 0:
		width = c.opts.DeviceChannels
	default:
		srcPad := asrc.GetStaticPad("src")
		if srcPad == nil {
			return errNoPad(nameAudioSrc, "src", "the channel layout it will produce cannot be read")
		}
		n, err := fixedChannelCount(srcPad.QueryCaps(nil))
		if err != nil {
			log.Printf("gst: capture: %s was given no device width and %v, so no %s is written. "+
				"Every seat is supposed to carry one; a stereo source will still negotiate and "+
				"audioconvert will downmix as it always has, but the routing panel cannot be "+
				"sized for this device. A device that is UNPOSITIONED above two channels — which "+
				"every CoreAudio source is, gstosxcoreaudio.c:886-889 — cannot be mapped to stereo "+
				"without one, and will stop this chain with not-negotiated (-4) about 0.07 s after "+
				"PLAYING. Populate CaptureOpts.DeviceChannels from the enumeration",
				factory, err, propMixMatrix)
			return nil
		}
		width = n
	}
	// Recorded even when the matrix write below fails, because it is what arms the
	// per-channel picker: the width is a fact about the device and the matrix is a
	// decision about it.
	c.deviceWidth = width

	if width > MaxInputChannels {
		return fmt.Errorf("gst: %s presents %d input channels and this build maps at most %d. "+
			"A device wider than that is refused at SELECTION time, off air, rather than at a "+
			"START that would leave a commentator waiting", factory, width, MaxInputChannels)
	}

	m := c.opts.ChannelMap
	if len(m) == 0 {
		// DefaultChannelMap is what a one-channel device needs to be heard on
		// both sides, and it is the same function that produces the dual-mono the
		// operator asked for at width 1.
		m = DefaultChannelMap(width)
	}
	if err := c.writeChannelMapLocked(m, width); err != nil {
		return err
	}
	log.Printf("gst: capture: %s presents %d input channels; %s set to route %s",
		factory, width, propMixMatrix, m)
	return nil
}

// writeChannelMapLocked validates against a width and writes the matrix.
//
// VALIDATION FIRST AND NOTHING WRITTEN IF IT FAILS, because the element's own
// rejection is invisible: an out-of-range coefficient makes GObject log a
// CRITICAL to stderr and LEAVE THE PREVIOUS MATRIX IN FORCE, with nothing
// readable afterwards to say which of the two is running.
func (c *cgoCapture) writeChannelMapLocked(m ChannelMap, width int) error {
	matrix, err := m.MixMatrix(width)
	if err != nil {
		return err
	}
	if !hasProperty(c.aconv, propMixMatrix) {
		return fmt.Errorf("gst: this build's audioconvert has no %s property, so a capture device "+
			"cannot be routed at all (GStreamer has had it since 1.16)", propMixMatrix)
	}
	gogst.UtilSetObjectArg(c.aconv, propMixMatrix, mixMatrixArg(matrix))
	c.matrixWidth = width
	return nil
}

// setAconvSinkPad publishes the pad the width is read from, and forgetAconvSinkPad
// takes it away at teardown. Both are one line; they exist so that every write to
// the field goes through sinkPadMu and none of them is a line somebody has to
// remember to guard.
func (c *cgoCapture) setAconvSinkPad(pad gogst.Pad) {
	c.sinkPadMu.Lock()
	c.aconvSinkPad = pad
	c.sinkPadMu.Unlock()
}

func (c *cgoCapture) forgetAconvSinkPad() gogst.Pad {
	c.sinkPadMu.Lock()
	pad := c.aconvSinkPad
	c.aconvSinkPad = nil
	c.sinkPadMu.Unlock()
	return pad
}

// negotiatedInputChannels reads the channel count from aconv's sink pad's CURRENT
// caps: negotiation's own answer, not a query about what might happen.
//
// It takes sinkPadMu and NOT mu, so a routing panel polling it many times a
// second does not queue behind a device open. See the field's comment.
func (c *cgoCapture) negotiatedInputChannels() int {
	c.sinkPadMu.RLock()
	pad := c.aconvSinkPad
	c.sinkPadMu.RUnlock()

	if pad == nil {
		return 0
	}
	caps := pad.GetCurrentCaps()
	if caps == nil || caps.GetSize() == 0 {
		return 0
	}
	s := caps.GetStructure(0)
	if s == nil {
		return 0
	}
	n, ok := s.GetInt("channels")
	if !ok || n <= 0 {
		return 0
	}
	return int(n)
}

// wantChannelMeter is PLAN.md 4.3's arming condition against this pipeline's
// callback. It is asked twice — once against the width this build resolved and
// once against the width the pad negotiated — and the RULE itself lives in
// capture.go, shared with the stub twin, so that the two builds cannot arm the
// picker on different conditions and so that Gate A can assert it at every width.
func (c *cgoCapture) wantChannelMeter(width int) bool {
	return channelMeterWanted(width, c.onChannelLevels.Load() != nil)
}

// watchNegotiatedWidthLocked installs the EVENT_DOWNSTREAM probe that publishes
// the negotiated width to the application, stamped with this pipeline's device.
//
// It is a CAPS-event probe rather than a poll because the answer arrives once and
// then does not change: measured on the fitted card, aconv:sink negotiated
// channels=16 at t=0.1176 s with no consumer pipeline, no encoder and no SRT
// anywhere in the process.
// # THE CALLBACK DOES NOT RUN ON THE STREAMING THREAD
//
// The probe hands the width to a channel and a goroutine calls the application
// (publishWidths). That is what makes OnInputChannels safe to implement the
// obvious way — read the width back, size the grid, apply the stored routing —
// all of which re-enter this pipeline and take mu. On the streaming thread that
// would deadlock against teardownLocked, which removes THIS PROBE while holding
// mu and blocks until a running callback returns.
func (c *cgoCapture) watchNegotiatedWidthLocked() error {
	if c.onInputChannels.Load() == nil {
		return nil
	}
	key := c.deviceKey
	c.sinkPadMu.RLock()
	pad := c.aconvSinkPad
	c.sinkPadMu.RUnlock()
	if pad == nil {
		return errNoPad(nameAudioConv, "sink", "the negotiated width publisher has nothing to watch")
	}
	id := pad.AddProbe(gogst.PadProbeTypeEventDownstream,
		func(_ gogst.Pad, info *gogst.PadProbeInfo) gogst.PadProbeReturn {
			ev := info.GetEvent()
			if ev == nil || ev.GetType() != gogst.EventCaps {
				return gogst.PadProbeOK
			}
			caps := ev.ParseCaps()
			if caps == nil || caps.GetSize() == 0 {
				return gogst.PadProbeOK
			}
			s := caps.GetStructure(0)
			if s == nil {
				return gogst.PadProbeOK
			}
			n, ok := s.GetInt("channels")
			if !ok || n <= 0 {
				return gogst.PadProbeOK
			}
			// Republishing the same number on every renegotiation would make the
			// routing panel rebuild its grid — and lose the operator's
			// half-finished crosspoint — for no change at all.
			//
			// The width is marked published only once it is ON the channel: a drop
			// that had already recorded the number would leave the panel unsized
			// with no second chance, and this is the only publisher there is.
			if c.publishedWidth.Load() == int64(n) {
				return gogst.PadProbeOK
			}
			select {
			case c.widths <- capturedWidth{deviceKey: key, channels: int(n)}:
				c.publishedWidth.Store(int64(n))
			default:
				// Nothing is logged here. This is a streaming thread and log.Printf
				// takes a process-global mutex and blocks on stderr.
			}
			return gogst.PadProbeOK
		})
	if id == 0 {
		return errProbeFailed(nameAudioConv, "sink", "the negotiated width publisher")
	}
	c.capsProbeID = id
	return nil
}

// publishWidths hands negotiated widths to the application on its OWN goroutine.
// See cgoCapture.widths for why the hop exists. It ends when Stop closes the
// channel.
//
// # A WIDTH QUEUED BEFORE Stop IS DRAINED AND NOT DELIVERED
//
// Closing a buffered channel does NOT discard what is already in it: a range
// yields every buffered value before it ends. So a width the CAPS probe queued
// while this goroutine was descheduled would be delivered after the capture that
// measured it had been stopped and replaced — stamped with the OLD device key,
// because the stamp is taken in the probe.
//
// That is not a stale number that the next one corrects. The application's
// forwarder writes the last width and device key it is given and emits
// channelMap with them, and the frontend gates the routing panel on the key
// matching the SELECTED device, so the panel hides. It then stays hidden: the
// replacement's publishedWidth de-duplication means its probe will never publish
// that width again, and only reopening the routing screen (which reads the pad
// directly) repairs it.
//
// The guard is a read of errsClosed rather than a second channel, because
// closeFaultChannels sets it under errMu immediately before the close: anything
// still queued at that point belongs to a pipeline the application has already
// been told is gone.
func (c *cgoCapture) publishWidths() {
	for w := range c.widths {
		if c.faultChannelsClosed() {
			continue
		}
		if cb := c.onInputChannels.Load(); cb != nil {
			(*cb)(w.deviceKey, w.channels)
		}
	}
}

// faultChannelsClosed reports that Stop has closed this object's channels, which
// is also the moment after which nothing it measured is still true.
func (c *cgoCapture) faultChannelsClosed() bool {
	c.errMu.RLock()
	defer c.errMu.RUnlock()
	return c.errsClosed
}

// InputChannels reports the pad's negotiated width, or 0 on a pipeline with no
// commentary leg.
//
// It does NOT take mu. It is a UI-facing read that arrives many times a second
// while the routing panel is open, and mu is held across the whole of a device
// open; the pad it reads is guarded by its own lock for exactly this.
func (c *cgoCapture) InputChannels() int { return c.negotiatedInputChannels() }

// SetChannelMap rewrites the routing on a running pipeline, with no state change
// and no renegotiation. Measured on the real card at 119 us.
//
// It is sized against the pad's NEGOTIATED count read at this moment, never
// against the count the build used and never against anything the caller
// supplies: the caller cannot pass a width, so the caller cannot pass a wrong one.
//
// It NO LONGER REFUSES WHEN NOTHING IS SENDING. That refusal was a property of a
// pipeline that only existed during a session; this one exists from launch, which
// is the whole of R1.
func (c *cgoCapture) SetChannelMap(m ChannelMap) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		return errors.New("gst: capture pipeline is stopped")
	}
	if !c.started || c.aconv == nil {
		return errors.New("gst: this capture pipeline has no commentary leg to route")
	}
	width := c.negotiatedInputChannels()
	if width <= 0 {
		return errors.New("gst: the commentary pad has not negotiated a channel count yet, so " +
			"there is no width to size a matrix against")
	}
	return c.writeChannelMapLocked(m, width)
}

// armChannelMeterLocked turns the per-channel picker's messages on or off.
//
// A missing element is survived rather than fatal, unlike the cough mute: a meter
// that does not move is a diagnosis problem, and refusing to carry commentary
// over one would be the wrong trade.
func (c *cgoCapture) armChannelMeterLocked(pipeline gogst.Pipeline, on bool) {
	el := pipeline.GetByName(channelLevelElementName)
	if el == nil {
		log.Printf("gst: capture: no element named %s, so the per-channel meters cannot be armed "+
			"and the routing panel's bars will never move", channelLevelElementName)
		return
	}
	if !hasProperty(el, propPostMessages) {
		log.Printf("gst: capture: %s has no %s property, so the per-channel meters cannot be armed "+
			"or silenced", channelLevelElementName, propPostMessages)
		return
	}
	el.SetObjectProperty(propPostMessages, on)
	log.Printf("gst: capture: per-channel metering %s (%s %s=%t)",
		map[bool]string{true: "ON", false: "off"}[on], channelLevelElementName, propPostMessages, on)
}

// ---------------------------------------------------------------------------
// The cough mute
// ---------------------------------------------------------------------------

// SetCommentaryMute writes the mute and reads it back.
//
// It SUCCEEDS WITH NO SESSION RUNNING, which reverses the argument at
// app.go:4784 — and the argument is answered rather than deleted. The fear was of
// a control that lies, and it is met by VISIBILITY rather than by unavailability:
// the mute sits upstream of alevel, so a muted commentator has a flat programme
// meter AND a mute banner, before and after START, and the write-then-read-back
// discipline is untouched.
func (c *cgoCapture) SetCommentaryMute(mute bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		return errors.New("gst: capture pipeline is stopped")
	}
	if !c.started || c.cough == nil {
		return errors.New("gst: this capture pipeline has no commentary leg to mute")
	}
	return c.applyCoughMuteLocked(mute)
}

// applyCoughMuteLocked writes the property and stores what the ELEMENT says
// afterwards, never what was asked for. mu is held.
func (c *cgoCapture) applyCoughMuteLocked(mute bool) error {
	c.cough.SetObjectProperty(propMute, mute)

	got, ok := c.cough.ObjectProperty(propMute).(bool)
	if !ok {
		return fmt.Errorf("gst: %s.%s did not read back as a boolean, so there is no way to know "+
			"whether the commentary is muted", nameCoughMute, propMute)
	}
	if got != mute {
		return fmt.Errorf("gst: %s.%s was set to %t and reads back %t; the mute did not take, and "+
			"a cough button that reports success while the microphone stays open is the one "+
			"failure this control cannot have", nameCoughMute, propMute, mute, got)
	}
	c.muted.Store(got)
	return nil
}

// CommentaryMuted is the read-back value, answered without mu.
func (c *cgoCapture) CommentaryMuted() bool { return c.muted.Load() }

// ---------------------------------------------------------------------------
// The bus
// ---------------------------------------------------------------------------

// Faults carries this capture pipeline's fatal errors.
func (c *cgoCapture) Faults() <-chan error { return c.errs }

// Warnings carries the spared classes. See the field: it is NOT the channel
// logWarnings drains, because two consumers on one channel each get an arbitrary
// half of it.
func (c *cgoCapture) Warnings() <-chan string { return c.warnsOut }

// onBusMessage is the capture bus's sync handler. It runs on a streaming thread
// and MAY NOT TAKE mu.
//
// It returns BusDrop for every message, and that is not tidiness: nothing else in
// this process reads this bus, and a sync handler that passes messages on to a bus
// with no watch is a slow leak — measured at 7,168 queued messages in the capture
// probe that let one through.
//
// The classification needs NO BIN TRAVERSAL here. captureLegsFor's parent-bin walk
// existed because the old single pipeline could not say at build time which shape
// it was; a capture pipeline knows its leg-set before gst_parse_launch is called,
// so the answer is a field read.
func (c *cgoCapture) onBusMessage(_ gogst.Bus, msg *gogst.Message) gogst.BusSyncReply {
	if msg == nil || c.busSilenced.Load() {
		return gogst.BusDrop
	}

	switch msg.Type() {
	case gogst.MessageError:
		source := "pipeline"
		if src := msg.Source(); src != nil {
			source = src.GetName()
		}
		debug, gerr := msg.ParseError()
		err := fmt.Errorf("gst: capture: %s: %v (%s)", source, gerr, debug)

		switch classifyBusError(source, captureLegs{AudioClockedByVideo: c.legs.AudioClockedByVideo()}) {
		case classPreview:
			// The confidence monitor. SPARED, absolutely: it is downstream of a
			// leaky tee branch and feeds a window, so nothing it does can reach
			// the feed. It must never reach Faults().
			c.deliverWarning("gst: the confidence monitor failed and the commentary and the " +
				"picture are unaffected: " + err.Error())

		case classVideoCapture:
			// The picture is down. RECOVERABLE for this pipeline — the commentary
			// capture goes on metering, muting and routing, and RestartCapture is
			// the repair — so it is latched in its OWN field and never in fatal,
			// which would take a fused seat's microphone off the air for a queue.
			//
			// IT IS DELIVERED ON Faults() AND NO LONGER ONLY ON Warnings(), and that
			// is the correction rather than a widening. A warning is drained and
			// discarded by the application; nothing on screen changed, Health()
			// answered nil, ArmForSend's latched-fault refusal did not fire, START
			// reached PLAYING over a dead card and the refusal that finally came was
			// the muxer watchdog's "nothing reached vq:src" two seconds later — the
			// pad, not the cause — with capturefault.go's named diagnosis of that
			// exact fault produced and discarded one layer down.
			//
			// IT IS NOT "the commentary is unaffected" WHILE A SESSION IS UP, and
			// this message must not say so. mpegtsmux emits nothing at all while
			// one of its two inputs is silent — measured, vq:src 0, aq:src 187 at
			// full rate, mux:src 0 — so a dead picture takes the WHOLE transport
			// stream down. The send side's 2 s muxer watchdog is still the detector
			// while a feed is up; this is what names the cause.
			//
			// captureFatalError, and not a sentence of its own: at the GStreamer
			// level a card that is busy, a card that has been unplugged and a card
			// with nothing on its input are one generic stream error with three
			// different fixes, and that diagnosis is exactly what was being computed
			// and thrown away.
			c.markPictureFatal(fmt.Errorf("%w. The commentary capture is unaffected and is "+
				"still running, but while a send session is up the muxer stops entirely until "+
				"the picture is rebuilt",
				captureFatalError(msg.Source(), source, err)))
			c.deliver(c.pictureFatalError())

		case classAudioCapture:
			// Fatal for THIS capture, and NAMED. At the GStreamer level "device
			// busy", "device missing" and "no signal" are the same generic stream
			// error with three different fixes; capturefault_cgo.go reads the
			// card's own evidence to tell them apart.
			c.markFatal(captureFatalError(msg.Source(), source, err))
			c.deliver(c.fatalError())

		default:
			// Not a leg this classifier could name. The honest answer is that
			// this capture is down, said in those words rather than guessed at.
			c.markFatal(fmt.Errorf("%w: %w (this capture pipeline has failed; the meters, the "+
				"preview and the routing it feeds are down until it is rebuilt)",
				ErrPipelineFatal, err))
			c.deliver(c.fatalError())
		}
		return gogst.BusDrop

	case gogst.MessageWarning:
		debug, gerr := msg.ParseWarning()
		source := "pipeline"
		if src := msg.Source(); src != nil {
			source = src.GetName()
		}
		c.deliverWarning(fmt.Sprintf("gst: capture: %s: %v (%s)", source, gerr, debug))
		return gogst.BusDrop

	case gogst.MessageElement:
		s := msg.GetStructure()
		if s == nil || s.GetName() != levelStructureName {
			return gogst.BusDrop
		}
		levels, ok := levelsFromStructure(s)
		if !ok {
			return gogst.BusDrop
		}
		source := ""
		if src := msg.Source(); src != nil {
			source = src.GetName()
		}
		switch levelKindForSource(source) {
		case levelKindProgramme:
			if cb := c.onLevels.Load(); cb != nil {
				(*cb)(levels)
			}
		case levelKindChannels:
			if cb := c.onChannelLevels.Load(); cb != nil {
				(*cb)(levels)
			}
		}
		return gogst.BusDrop
	}
	return gogst.BusDrop
}

func (c *cgoCapture) markFatal(err error) {
	c.errMu.Lock()
	if c.fatal == nil {
		c.fatal = err
	}
	c.errMu.Unlock()
}

func (c *cgoCapture) fatalError() error {
	c.errMu.RLock()
	defer c.errMu.RUnlock()
	return c.fatal
}

// markPictureFatal latches a picture-leg death. First one wins, as markFatal
// does: the first fault is the diagnosis and the tenth is what it knocked over.
func (c *cgoCapture) markPictureFatal(err error) {
	c.errMu.Lock()
	if c.pictureFatal == nil {
		c.pictureFatal = err
	}
	c.errMu.Unlock()
}

func (c *cgoCapture) pictureFatalError() error {
	c.errMu.RLock()
	defer c.errMu.RUnlock()
	return c.pictureFatal
}

// anyFatalError is the question the SEND side asks: is there ANY latched death
// in this pipeline, of either leg.
//
// Both answers stop a send, and for the same measured reason: mpegtsmux emits
// nothing at all while one of its two inputs is silent — vq:src 0, aq:src 187 at
// full rate, mux:src 0 — so a dead picture takes the whole transport stream down
// exactly as a dead commentary does. What differs is the REPAIR, which is why
// the two are latched separately and only this one merges them.
func (c *cgoCapture) anyFatalError() error {
	c.errMu.RLock()
	defer c.errMu.RUnlock()
	if c.fatal != nil {
		return c.fatal
	}
	return c.pictureFatal
}

// deliver puts an error on Faults() without ever blocking a streaming thread. A
// full channel drops, because the first fault is the diagnosis and the tenth is
// noise.
func (c *cgoCapture) deliver(err error) {
	if err == nil {
		return
	}
	c.errMu.RLock()
	defer c.errMu.RUnlock()
	if c.errsClosed {
		return
	}
	select {
	case c.errs <- err:
	default:
	}
}

func (c *cgoCapture) deliverWarning(line string) {
	c.errMu.RLock()
	defer c.errMu.RUnlock()
	if c.errsClosed {
		return
	}
	select {
	case c.warns <- line:
	default:
	}
}

// logWarnings writes every warning to the log and FORWARDS it to Warnings(), on
// its own goroutine, because log.Printf takes a process-global mutex and blocks
// on stderr while the bus handler runs on a streaming thread.
//
// It forwards rather than competes. A single channel read by both this logger and
// the application would split the lines between them at random — a confidence
// monitor failing would reach the field log or the operator's screen, never both,
// and which one depended on the scheduler.
//
// The forward is non-blocking for the reason the delivery is: an application that
// has stopped draining must not be able to stop this goroutine, which is also the
// only thing writing the field log. The close is what tells the application's
// `for range` to end, and it happens after the last line has been forwarded.
func (c *cgoCapture) logWarnings() {
	defer close(c.warnsOut)
	for line := range c.warns {
		log.Print(line)
		select {
		case c.warnsOut <- line:
		default:
		}
	}
}

// ---------------------------------------------------------------------------
// Teardown
// ---------------------------------------------------------------------------

// Stop takes the pipeline to NULL, releases the device and closes the fault
// channels. It is idempotent.
//
// # It REFUSES while a send pipeline still holds the seam
//
// Taking the device to NULL underneath a bound proxysrc is the measured
// completely-silent failure: 0 buffers, no EOS, no ERROR and no WARNING on either
// bus, the send pipeline still PLAYING and SRT still connected. proxysink returns
// GST_FLOW_OK unconditionally, so nothing crosses the seam to say the producer has
// gone. Only the send side's 2 s muxer watchdog would ever notice, and by then the
// switcher has had two seconds of nothing.
//
// The ordering rule — stop the sender, then the capture — was written only in
// prose, and prose is enforced by four call sites getting it right. This is the
// enforcement. The claims are released by SendSeam.Stop, which the send
// pipeline's teardown calls and which is idempotent, so the way out of this
// refusal is the same one the caller was supposed to take first.
func (c *cgoCapture) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return nil
	}
	if held := c.claims.takenNames(); len(held) > 0 {
		return fmt.Errorf("%w: %v still has a consumer, so this capture will not go to NULL. A "+
			"capture pipeline taken down under a bound proxysrc is silent in every direction — no "+
			"EOS, no error on either bus, SRT still connected and the switcher receiving nothing. "+
			"Stop the send session first (SendSeam.Stop releases the claim and is idempotent)",
			ErrSeamBusy, held)
	}
	c.stopped = true
	err := c.teardownLocked()
	c.closeFaultChannels()
	return err
}

// closeFaultChannels ends Faults(), Warnings() and the width publisher, and with
// them the two goroutines parked on this object.
//
// IT IS CALLED FROM Stop AND FROM NOWHERE ELSE — above all not from
// teardownLocked, which buildLocked's abort() also runs. Closing them there
// deafened the object permanently on the FIRST failed build: the documented
// retry-without-preview then produced a live capture pipeline whose Faults()
// channel was already closed, so a card unplugged twenty minutes later reached an
// application whose `for range` had exited at launch. The shipped convention is
// the same one for the same reason — cgoPipeline closes its channels in Stop and
// not in its teardown.
func (c *cgoCapture) closeFaultChannels() {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.errsClosed {
		return
	}
	c.errsClosed = true
	close(c.errs)
	close(c.widths)
	// warns and NOT warnsOut: logWarnings closes the outward half as it exits, so
	// that the application's last warning is delivered before its `for range`
	// ends. Closing it here would race that goroutine's own send.
	close(c.warns)
}

// teardownLocked tears down THIS PIPELINE ONLY and must never touch a send
// pipeline. mu is held.
//
// THE ORDER IS LOAD-BEARING:
//
//  1. Silence the bus handler. It is never DETACHED — gst_bus_set_sync_handler
//     with NULL races a handler already running on a streaming thread — so the
//     flag is what replaces detaching it.
//  2. Stop and JOIN the signal watchdog. Its reader closure holds a capture
//     element, and a property read against a disposed element is a read on freed
//     memory. This must happen before the pipeline goes to NULL, including on a
//     device change.
//  3. Remove the CAPS probe, for the same reason.
//  4. NULL, which is what actually closes the device.
//  5. Drop every element reference, so the finalizer can run.
//
// The single-consumer claims are deliberately NOT released here. They belong to
// whatever send pipeline took them, and a teardown that handed them back would
// let a second consumer attach to a proxysink that still has one.
//
// IT IS ALSO buildLocked's ABORT PATH, which is why it clears the latched fault
// and the published width as well as the elements: everything it leaves behind is
// inherited by the retry, and a retry that inherits the first attempt's diagnosis
// aborts on a pipeline that is healthy. What it must NOT do is close the fault
// channels; see closeFaultChannels.
func (c *cgoCapture) teardownLocked() error {
	c.busSilenced.Store(true)

	if c.sigWatch != nil {
		c.sigWatch.Stop()
		c.sigWatch = nil
	}
	if pad := c.forgetAconvSinkPad(); pad != nil && c.capsProbeID != 0 {
		pad.RemoveProbe(c.capsProbeID)
	}
	c.capsProbeID = 0

	var err error
	if c.pipeline != nil {
		stop := stateChangeWatchdog("capture pipeline to NULL (releasing the capture device)")
		ret := c.pipeline.BlockSetState(gogst.StateNull, gogst.ClockTime(captureStartTimeout))
		stop()
		if !stateChangeOK(ret) {
			err = fmt.Errorf("gst: capture pipeline would not go to NULL (%s); the device may "+
				"still be held", ret)
		}
	}

	c.pipeline = nil
	c.clock = nil
	c.bus = nil
	c.vproxq, c.vproxsink = nil, nil
	c.aproxq, c.aproxsink = nil, nil
	c.aconv = nil
	c.cough = nil
	c.matrixWidth = 0
	c.deviceWidth = 0
	c.started = false

	// The published width goes with the pipeline that negotiated it. Left
	// standing, a rebuild at the same width takes the probe's de-duplication
	// early-return and never republishes — and PLAN.md step 7 moves forgetChannelMap
	// into capture teardown, after which the panel would be cleared and then never
	// re-sized.
	c.publishedWidth.Store(0)

	// The latched faults belong to the pipeline that died, not to this object. See
	// the doc above and the two fields' own comments.
	c.errMu.Lock()
	c.fatal = nil
	c.pictureFatal = nil
	c.errMu.Unlock()
	return err
}

// ---------------------------------------------------------------------------
// Small shared helpers
// ---------------------------------------------------------------------------

// mustGet looks an element up by name and produces the error that names what its
// absence costs, rather than "nil".
func mustGet(pipeline gogst.Pipeline, name string) (gogst.Element, error) {
	el := pipeline.GetByName(name)
	if el == nil {
		return nil, errNoElement(name, "the parsed graph is not the one this package asked for")
	}
	return el, nil
}

func errNoElement(name, consequence string) error {
	return fmt.Errorf("gst: the parsed pipeline has no element named %s, so %s", name, consequence)
}

func errNoPad(element, pad, consequence string) error {
	return fmt.Errorf("gst: %s has no %s pad, so %s", element, pad, consequence)
}

func errProbeFailed(element, pad, what string) error {
	return fmt.Errorf("gst: gst_pad_add_probe failed on %s:%s, so %s is not installed",
		element, pad, what)
}
