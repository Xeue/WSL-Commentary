// livewatch.go is THE MUXER WATCHDOG: the detector for a capture pipeline that
// has died underneath a running send pipeline.
//
// # The failure it exists for is entirely silent
//
// MEASURED, twice: a capture pipeline dying under a running send pipeline
// produces 0 buffers, NO EOS, NO ERROR and NO WARNING on either bus, with the
// send pipeline still PLAYING and SRT still connected. proxysink returns
// GST_FLOW_OK unconditionally; no error and no back-pressure ever crosses the
// seam. Every indicator in the application goes on saying the feed is healthy.
//
// It is the same shape of failure as the unarmed seam, and it is why this
// watchdog and the arming are two separate mechanisms rather than one: arming
// stops the seam going quiet at START, and this notices if it goes quiet later.
//
// # Which pads, and why three rather than the two the plan chose
//
// The plan chose `vq:src` and `aq:src` — the muxer's two inputs, the last point
// before the thing that actually goes to air. That is right and it is HALF BLIND
// on its own, which is a measurement and not a hunch. On a dead video feed the
// AUDIO leg does NOT stop: it pushes at full rate into the muxer, and the muxer
// emits nothing at all while it waits for the other stream.
//
//	vq:src 0 (healthy 199) | aq:src 187 (healthy 187) | mux:src 0 (healthy 855)
//
// A seam whose AUDIO proxysink alone went unarmed would therefore read fully
// green on both of the plan's chosen pads: aq would be silent (correctly
// alarming) but a seam whose VIDEO half alone failed would show aq at full rate
// and only mux:src at zero. `mux:src` is the one pad that read ZERO in every
// failing case, so it is watched too.
//
// # BUFFER_LIST is mandatory
//
// mpegtsmux at alignment=7 pushes BUFFER LISTS, and a BUFFER-only probe reads
// zero while megabytes flow. That cost two independent probes a run each. The
// mask is BUFFER|BUFFER_LIST everywhere in this file and in the tests that pin
// it.
//
// # It is armed only while the send pipeline is PLAYING
//
// A device change is refused while sending and the capture pipelines are not
// watched by this at all, so there is no legitimate quiet period for it to
// false-positive on. It has one job with two halves: refuse a START at which
// nothing arrived, and latch a fault if a running feed goes quiet.
package gst

import (
	"fmt"
	"time"
)

// The three pads watched, by the name of the element they belong to. All three
// are `:src`.
//
// They are declared untagged so that the timing logic below, its tests, and the
// error messages an operator reads all name the same pads at Gate A — the cgo
// half only attaches the probes.
const (
	nameMuxVideoQueue = "vq"  // the video queue feeding the muxer
	nameMuxAudioQueue = "aq"  // the audio queue feeding the muxer
	nameMuxOutput     = "mux" // the muxer itself: the pad that read zero in EVERY failing case
)

// liveWatchPollInterval is the poller's period. It is the signal watchdog's
// proven shape and its proven number: a quarter second is eight reads inside the
// silence budget below, which is enough that a marginal feed is not diagnosed off
// one unlucky sample.
const liveWatchPollInterval = 250 * time.Millisecond

// liveWatchSilence is how long a watched pad may be quiet before the feed is
// declared dead.
//
// Two seconds is chosen against what is on the other side of the decision. Too
// short and a momentary scheduling stall takes a healthy match off air; too long
// and the operator watches a green lamp over a dead feed. Two seconds is roughly
// a hundred video frames and forty programme-meter frames — an eternity by the
// standards of anything that is actually running, and short enough that nobody
// reads a commentary break into it.
const liveWatchSilence = 2 * time.Second

// liveWatchStartGrace is how long after PLAYING the send pipeline has to produce
// something before Start refuses.
//
// THIS IS THE GUARD THAT MAKES A MISSED ARMING LOUD INSTEAD OF GREEN. It is the
// same budget as the silence above, and it is affordable because a healthy seam
// beats it by two orders of magnitude: measured, the first buffer reached the
// muxer 20-60 ms after PLAYING on both rigs.
//
// It is NOT the only detector for that case, and it is deliberately not the
// fastest. Once the real encoder is in place an unarmed VIDEO seam also posts
// "negotiation problem" naming venc at about +16 ms, which classifyBusError
// already treats as fatal. That is two orders of magnitude quicker — but it is
// the video leg's complaint ONLY, so it is not a substitute for this.
const liveWatchStartGrace = 2 * time.Second

// liveWatchStartPoll is how often Start re-reads the pads while it waits for the
// first buffer inside that grace.
//
// It is much finer than liveWatchPollInterval because it is measured against a
// different number: the poller is looking for a two-second silence and a quarter
// second is eight reads inside it, while this is looking for a first buffer that
// arrives in 20-60 ms, and a 250 ms tick would add up to a quarter second to
// EVERY start to observe something that had already happened. Ten milliseconds
// costs at most a handful of wake-ups on the one path where the operator is
// already waiting for a state change.
//
// It is NOT a signal from the probe, deliberately. The probe runs on a GStreamer
// streaming thread on the on-air path, and the whole of livewatch_cgo.go is
// arranged so that thread does two stores and returns; a condition variable there
// would put a Go scheduler wake-up inside the muxer's push.
const liveWatchStartPoll = 10 * time.Millisecond

// liveWatchSample is one watched pad's reading at one instant: how many buffers
// have crossed it and when the last one did.
//
// It is a plain value with no GStreamer in it so that every decision below is
// ordinary Go and Gate A tests every branch.
type liveWatchSample struct {
	// Pad is the element name, rendered as "<name>:src" in messages.
	Pad string

	// Buffers is the total count since the probe was installed. Buffer LISTS are
	// counted by their length, not as one, because mpegtsmux at alignment=7
	// pushes lists of seven and counting them as one understates by 7x.
	Buffers int64

	// Last is when the most recent buffer arrived, or the zero time if none
	// ever has.
	Last time.Time
}

// liveWatchStartVerdict decides whether a send pipeline that has just reached
// PLAYING is actually carrying media.
//
// It returns nil when every watched pad has seen at least one buffer, and an
// error naming THE PADS THAT SAW NOTHING otherwise. Naming them rather than
// saying "no media" is the whole value: `vq:src` alone silent is the video half
// of the seam, `aq:src` alone is the audio half, and all three is a seam that was
// never armed at all.
func liveWatchStartVerdict(samples []liveWatchSample, grace time.Duration) error {
	if len(samples) == 0 {
		return fmt.Errorf("gst: the liveness gate was given no pads to watch, so a send pipeline "+
			"carrying nothing would report success. Expected probes on %s:src, %s:src and %s:src",
			nameMuxVideoQueue, nameMuxAudioQueue, nameMuxOutput)
	}
	var silent []string
	for _, s := range samples {
		if s.Buffers == 0 {
			silent = append(silent, s.Pad+":src")
		}
	}
	if len(silent) == 0 {
		return nil
	}
	return fmt.Errorf("%w: nothing reached %v within %s of the send pipeline reaching PLAYING. "+
		"The capture seam is not carrying media into the muxer — the usual cause is a proxysink "+
		"that was not re-armed, which does not fail: SRT connects, the lamp goes green and the "+
		"switcher receives silence",
		ErrPipelineFatal, silent, grace)
}

// liveWatchSilenceVerdict decides whether a RUNNING send pipeline has gone quiet.
//
// It returns nil while every watched pad is inside the budget, and an error
// naming the quiet ones otherwise. A pad that has never seen a buffer at all is
// measured from the reference time the caller passes — the moment the pipeline
// reached PLAYING — so that a feed which never started is caught by the same
// arithmetic as one that stopped.
func liveWatchSilenceVerdict(samples []liveWatchSample, playingAt, now time.Time,
	budget time.Duration) error {

	var quiet []string
	for _, s := range samples {
		since := s.Last
		if since.IsZero() {
			since = playingAt
		}
		if now.Sub(since) >= budget {
			quiet = append(quiet, fmt.Sprintf("%s:src (quiet for %s after %d buffers)",
				s.Pad, now.Sub(since).Round(time.Millisecond), s.Buffers))
		}
	}
	if len(quiet) == 0 {
		return nil
	}
	return fmt.Errorf("%w: the feed has gone silent at the muxer: %v. Nothing crosses the capture "+
		"seam as an error or as back-pressure — proxysink returns GST_FLOW_OK unconditionally — so "+
		"this poller is the only thing in the process that can tell, and the lamp would otherwise "+
		"stay green over a dead feed",
		ErrPipelineFatal, quiet)
}
