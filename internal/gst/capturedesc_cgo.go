//go:build cgo && !gststub

// capturedesc_cgo.go renders the parse strings for BOTH sides of the seam.
//
// captureDescription renders the seven capture shapes (P-SLATE, P-CARD,
// P-CARD-PREVIEW, C-NATIVE, C-CARD, FUSED, FUSED-PREVIEW) and sendDescription
// renders THE ONE SEND STRING — the same string on every seat, with two
// parameters and no third.
//
// # Why these are pure functions in their own file
//
// Because a mis-ordered chain has to be caught by a golden-string test rather
// than by a card. capturedesc_cgo_test.go renders all eight and compares them
// character for character against PLAN.md section 2, which is the only kind of
// test that catches "videoconvert and videoscale swapped" — an edit that parses,
// starts, sends, and costs 2.85 s of CPU per 500 frames instead of 0.04 s.
//
// They live behind the cgo tag for one reason and it is not a preference:
// captureSourceFactory and aacEncoderFactory are in elements_darwin.go /
// elements_windows.go, which are `cgo && !gststub`, and the whole point of
// naming the two platform elements through consts is that neither port can be
// silently repointed at a different element. Duplicating them into an untagged
// file to move this function to Gate A would defeat the guard that exists to
// stop exactly that.
//
// # What is NOT in either string
//
// The slate path, the CoreAudio device integer, the DeckLink persistent-id and
// the SRT endpoint are all set with g_object_set after parse and never reach the
// parser. gst_parse_launch treats a backslash as an escape inside double quotes,
// so a Windows path would be mangled, and a WASAPI endpoint GUID's braces and
// dots are similarly at the mercy of the quoting rules.
//
// `connection` appears in NO string here and is set by NO g_object_set anywhere
// in this package. It PERSISTENTLY RECONFIGURES THE CARD and overrides Blackmagic
// Desktop Video Setup, and it has had to be undone by hand twice. If the card is
// silent the answer is never another connection value. The mic is on the MIC
// input, which is a card-level setting this application does not own.
package gst

import "strconv"

// captureDescription renders one always-live capture pipeline.
//
// legs is the shape (PlanCapture's output), conform is the raster and rate the
// picture leg is conformed to, and preview is previewBranchFor's output — a
// string that already carries its own leading newline, or empty.
//
// # The chain order is fixed
//
// Picture chains, then commentary chains, then the preview. Nothing depends on
// it at run time — gst_parse_launch does not care — and everything depends on it
// at test time, because golden strings are only stable if the order is.
//
// # Why the conform chain is HERE and not in the send pipeline
//
// It is the whole reason the seam caps never change. The DeckLink changes raster
// every time it locks or loses signal (720x486 NTSC placeholder, then the real
// input, then back when the cable is pulled). With conform in capture, that
// renegotiates videoscale's SINK caps only: the proxy sees one set of caps for
// the life of the process and the encoder never sees a caps change at all. With
// conform in the send pipeline it would be a mid-stream renegotiation of the
// on-air path, every time somebody touched a cable.
func captureDescription(legs CaptureLegs, conform ConformTarget, preview string) string {
	chains := make([]string, 0, 4)

	switch legs.Picture {
	case PictureSlate:
		// THE SLATE LEG, character for character today's, with the encoder
		// replaced by the proxy tail.
		//
		// VIDEOCONVERT AND VIDEOSCALE SIT BEFORE IMAGEFREEZE, and that is the
		// whole optimisation: it converts and scales the ONE frame pngdec
		// produces rather than the same still picture fifty times a second.
		// Measured on macOS arm64, GStreamer 1.26.10, 500 frames at 50 fps,
		// both orders, both slate sizes:
		//
		//	order                     1920x1080 slate   1920x1200 slate
		//	imagefreeze first (old)      2.85 s CPU        3.02 s CPU
		//	videoscale first (new)       0.04 s CPU        0.05 s CPU
		//
		// and the rendered NV12 buffers are BYTE-IDENTICAL between the two
		// orders for both slates. It is a pure reordering.
		//
		// The capsfilter has to SPLIT to allow it, and the split is the subtle
		// part: everything about one PICTURE is pinned above imagefreeze, where
		// videoscale can act on it, and the frame RATE below it, because the
		// rate is the one thing imagefreeze itself decides — it takes a buffer
		// whose upstream framerate is 0/1 and repeats it at whatever its src pad
		// negotiates. Pinning framerate=50/1 above instead asks pngdec for a
		// rate it does not have and the leg fails to negotiate.
		//
		// videoscale is present so the artwork does not have to be exactly the
		// conform target's size. It is replaced every season by someone who will
		// not read this file, and without it a 1920x1200 export fails caps
		// negotiation with no diagnostic that names the size.
		//
		// NO TEE. The preview refuses on a slate seat and always will: there is
		// nothing live to be confident about.
		chains = append(chains, ""+
			"filesrc name="+nameSlateSrc+
			" ! pngdec"+
			" ! videoconvert"+
			" ! videoscale name="+nameVideoScale+
			" ! "+conform.spatialCaps()+
			" ! imagefreeze is-live=true"+
			" ! "+conform.temporalCaps()+
			" ! "+videoProxyTail())

	case PictureCard:
		// THE LIVE CAPTURE LEG, element by element. Every line below was measured
		// on the fitted UltraStudio 4K Mini; none of it is defensive habit, and
		// the whole block moved here whole when pipelineDescription was deleted.
		//
		//	mode=auto             ONLY. A pinned mode that DISAGREES with the input
		//	                      does not fail — measured, mode=pal against a real
		//	                      1080p25 input produced 50 clean PAL buffers with
		//	                      nothing but a warning. Green lamp, real bitrate,
		//	                      black picture.
		//	connection            IS NEVER SET, not here and not anywhere in this
		//	                      package. It is not a per-pipeline selection: it
		//	                      PERSISTENTLY RECONFIGURES THE CARD, overrides what
		//	                      the operator set in Blackmagic Desktop Video Setup,
		//	                      and has had to be undone by hand twice. If a
		//	                      capture is black or silent the answer is NEVER
		//	                      another connection value.
		//	drop-no-signal-frames Left at its default of false, and STATED so that
		//	                      the default changing is a visible edit. False means
		//	                      the card keeps emitting black GAP-flagged frames at
		//	                      full rate FOREVER on signal loss — no error, no EOS,
		//	                      the muxer never starves. That is why the feed's
		//	                      continuity is not at risk, and also why nothing in
		//	                      this pipeline can tell you the signal has gone: the
		//	                      only thing that can is the card's own signal
		//	                      property, which signalwatch.go polls. A card that
		//	                      DROPPED them would starve the muxer on the very
		//	                      seat whose commentary it is also clocking.
		//	videoconvert          The card negotiates UYVY or v210 depending on the
		//	                      input; the encoder wants NV12.
		//	deinterlace           The only thing in this chain that handles a 1080i50
		//	                      camera, which is still what a good deal of outside
		//	                      broadcast kit produces. It passes progressive
		//	                      through at essentially zero cost, so it is not a
		//	                      trade — it is free insurance against the one input
		//	                      format that would otherwise reach the encoder as
		//	                      interlaced frames the switcher will not take.
		//	videoscale            The camera's raster need not be the switcher's.
		//	videorate             MANDATORY, and the least obvious element here.
		//	                      decklinkvideosrc emits a 720x486 NTSC PLACEHOLDER
		//	                      as its FIRST BUFFER on every start, with GAP set
		//	                      and signal=false, and the real caps arrive about
		//	                      170 ms later. A fixed capsfilter with no videorate
		//	                      in front of it dies 0.088 s after PLAYING with
		//	                      not-negotiated (-4), 3 runs out of 3. It also
		//	                      absorbs a camera whose rate is not the switcher's.
		//	tee allow-not-linked  Needed by the DEFAULT configuration, not by the
		//	                      preview: without it a tee with an unlinked src pad
		//	                      returns NOT_LINKED upstream and stops the leg. A
		//	                      SECOND decklinkvideosrc IS NOT AN OPTION — the card
		//	                      is exclusive, two sources in one process fail 3/3
		//	                      and two processes fail 3/3 — so sharing this one
		//	                      through a tee is the only shape a preview can have.
		//
		// THE TEE IS STILL A TEE AND THE SEAM IS STILL A PROXY, and the note that
		// used to sit here saying "proxysrc was measured as an alternative to the
		// tee and REJECTED" is answered rather than deleted, because a reader who
		// finds proxysink two lines below will otherwise think it was overruled.
		//
		// The measurement stands: proxysrc's internal queue is leaky=0
		// max-size-buffers=200 and exposes no tuning, so a wedged consumer stalls
		// the producer from 50 fps to 0 in under two seconds, and the producer's
		// death is silent across the boundary. Both objections are real and both
		// are ANSWERED on the capture side rather than avoided: the leaky queue in
		// front of every proxysink is what the wedge cannot reach through
		// (measured under a 12 s wedge: 50.1 fps and 20.0 meter msg/s with
		// leaky=downstream against 11.6 fps and 7.2 msg/s without), and the send
		// side's muxer watchdog is what makes the silent death loud within two
		// seconds. What was rejected was proxysrc INSTEAD OF the tee, inside one
		// pipeline; what shipped is a tee for the preview and a proxy for the
		// process boundary between two pipelines that must have separate
		// lifetimes. See seam.go for both invariants.
		chains = append(chains, ""+
			videoCaptureFactory+" name="+nameVideoCaptureSrc+
			" mode=auto drop-no-signal-frames=false"+
			" ! videoconvert name="+nameVideoCapConv+
			" ! deinterlace name="+nameVideoCapDeint+
			" ! videoscale name="+nameVideoCapScale+
			" ! videorate name="+nameVideoCapRate+
			" ! "+conform.captureCaps()+
			" ! tee name="+nameVideoCapTee+" allow-not-linked=true")

		// The broadcast branch, off the tee. It is its own chain so that the
		// preview branch below it is a peer rather than a special case.
		chains = append(chains, nameVideoCapTee+". ! "+videoProxyTail())
	}

	if legs.Commentary != CommentaryNone {
		// THE COMMENTARY SOURCE. Exactly one element differs between the two
		// seats; everything below it is written once.
		//
		// THERE IS NO CHANNEL CAPSFILTER ON THE SOURCE, AND THAT IS DELIBERATE.
		// The difference between a 2-channel and an 18-channel Focusrite is not
		// in this string; it is the width of the matrix written to aconv before
		// the pipeline leaves NULL. Measured: a real 3-channel CoreAudio device
		// probed 3 and negotiated 3 with no capsfilter, while FORCING a count
		// silently drifted the format F32LE->S16LE, and channels=2 with
		// channel-mask=0x0 was refused outright.
		//
		// The rate is not pinned here either, for the older reason: wasapi2src in
		// shared mode can only produce its endpoint's mix format, Dante Virtual
		// Soundcard is commonly at 44.1 or 96 kHz, and macOS makes the same point
		// louder — the NDI Audio device on this machine offers 44100 ONLY. The
		// capsfilter that matters is the one below the resampler.
		//
		// channels=16 is the ONE property set on the card source, and
		// deckLinkAudioChannels says why 2 — which would negotiate a positioned
		// pair and need no matrix — is the wrong answer.
		audioSource := captureSourceFactory + " name=" + nameAudioSrc
		if legs.Commentary == CommentaryCard {
			audioSource = audioCaptureFactory + " name=" + nameAudioSrc +
				" channels=" + strconv.Itoa(deckLinkAudioChannels)
		}

		chains = append(chains, ""+
			audioSource+
			// AN audioconvert ABOVE chlevel, AND IT IS NOT COSMETIC. level's
			// sink template accepts five formats and interleaved only;
			// audioconvert's accepts thirty plus non-interleaved. Putting
			// chlevel directly on the source NARROWS what a capture device may
			// negotiate, on every seat — including the on-air Windows build,
			// where wasapi2 does emit S24_32LE for some devices and nothing here
			// can be compiled or run. osxaudiosrc negotiates S16LE either way,
			// which is precisely why it would have shipped.
			//
			// It does NOT flatten the channel layout: measured, chlevel's sink
			// still sees channels=16 channel-mask=0x0 through it.
			" ! audioconvert"+
			// THE PER-CHANNEL PICKER, upstream of the routing so that it
			// measures the device's OWN channels before any decision has been
			// applied to them. alevel cannot answer this question at any price:
			// it sits below the capsfilter that pins channels=2. This is what
			// makes the mapping UI usable — the operator asks the commentator to
			// talk and watches which bar moves.
			//
			// post-messages=false is the built state and armChannelMeterLocked
			// turns it on when there is something worth metering. Measured live
			// at 61 us, stopping that element's messages dead while the other
			// level element in the same pipeline carried on undisturbed.
			" ! level name="+channelLevelElementName+
			" interval="+strconv.Itoa(channelLevelIntervalNs)+
			" post-messages=false"+
			// audioconvert IS NAMED, and the name is the routing engine's handle
			// on it. Its mix-matrix is live-settable while PLAYING — measured at
			// 119 us with no renegotiation — which is what lets the mapping UI be
			// a real-time control rather than an apply-and-restart form.
			" ! audioconvert name="+nameAudioConv+" ! audioresample"+
			" ! "+seamAudioCaps+
			// THE COUGH MUTE, between the capsfilter above and the programme
			// meter below, and that position is the whole of what makes it
			// honest. Above alevel: a mute placed BELOW the meter would let the
			// programme meter bounce on to a voice nobody is receiving. Below
			// chlevel: the operator can still see which of sixteen inputs a
			// coughing commentator is on, because that question's answer does not
			// change because they are off air for two seconds.
			//
			// Measured through the encoder chain, muted and unmuted: 473 AAC
			// access units both times, identical first and last PTS, identical
			// largest inter-packet gap of 21.344 ms. The stream is continuous and
			// silent, not interrupted.
			" ! volume name="+nameCoughMute+" "+propMute+"=false"+
			// THE PROGRAMME METER. It used to sit immediately above the AAC
			// encoder; it now sits immediately above the proxysink, with a leaky
			// queue between it and the encoder. Same buffers in normal operation.
			// During a send-side stall of more than about a second the meter can
			// move while the far end loses samples — that is the leak policy
			// (seam.go, A3) and levels.go says so where the promise is made.
			" ! level name="+levelElementName+" interval=50000000"+
			" ! "+audioProxyTail())
	}

	// THE CLOCK COMPANION, built ONLY when the commentary comes off the card and
	// the picture does not. When the picture does, the chain above already put a
	// decklinkvideosrc in this pipeline and that one is the clock: the card is
	// exclusive, so a second source is not an option to weigh, it is a pipeline
	// that fails.
	//
	// sync=false async=false: this sink must not participate in preroll latching
	// and must not pace against the clock. It exists so the CARD runs, not so
	// anything downstream of it does, and every frame it is handed is thrown
	// away. Measured at 0.6-2.4% of one core.
	if legs.NeedsClockCompanion() {
		chains = append(chains, ""+
			videoCaptureFactory+" name="+nameVideoCaptureClock+
			" mode=auto drop-no-signal-frames=false"+
			" ! fakesink name="+nameVideoCaptureClockSink+" sync=false async=false")
	}

	// THE CONFIDENCE MONITOR, appended whole or not at all. previewBranchFor has
	// already decided; the empty string is the ordinary answer, and the branch
	// carries its own leading newline so nothing here has to know whether there
	// is one.
	return joinChains(chains) + preview
}

// joinChains puts the newline between chains and nowhere else, so that a
// description with one chain has no trailing newline for the preview branch's
// leading one to double.
func joinChains(chains []string) string {
	out := ""
	for i, c := range chains {
		if i > 0 {
			out += "\n"
		}
		out += c
	}
	return out
}

// sendDescription renders THE SEND PIPELINE, and it is INVARIANT: one string,
// every seat, two parameters.
//
// No device, no slate, no preview, no conform target, no channel map, no mute.
// Every one of those is upstream of the seam and is the capture layer's. On
// Windows exactly two factory names differ (mfh264enc, mfaacenc), through the
// existing elements_*.go seam.
//
// Notes, each load-bearing:
//
//   - NO srtsink. START still installs no sink and still closes the gate first,
//     because srtq's src pad has no peer. ReplaceSink, RemoveSink, the three srtq
//     pad probes, rearmQueueLocked and its sticky-event restore are all
//     downstream of the seam and never see a capture element.
//
//   - NO `proxysink=` PROPERTY IN THE STRING. It cannot be there: measured,
//     `proxysrc proxysink=ps` fails with `could not set property "proxysink" in
//     element "proxysrc" to "ps"` because the property is object-typed. Both
//     proxysrcs are looked up by name after parse and given the pointer with
//     g_object_set — see bindProxySrc.
//
//   - THE AUDIO CAPSFILTER AFTER aproxsrc IS THE SEAM CONTRACT, asserted so that
//     a violation is a loud negotiation failure at START rather than a silent
//     wrong encode. Verified on this machine: atenc's sink template is exactly
//     `S16LE, interleaved, channels: [1,8]`.
//
//   - NO audioconvert / audioresample / videoconvert after either proxysrc. The
//     capture side already pins exactly what the encoders take, and elements that
//     can fail on the on-air path buy nothing.
//
//   - NO EXTRA QUEUE after either proxysrc. proxysrc is a GstBin containing its
//     own queue0 and already provides the thread boundary. That queue is
//     leaky=0 max-size-buffers=200 and exposes no tuning, which is why the leak
//     that matters lives on the capture side; see seam.go.
//
//   - alignment=7 gives 7 x 188 = 1316-byte buffers, exactly one SRT payload, so
//     nothing fragments. pcr-interval=3600 is the specification's value.
//     srtq leaky=downstream means output produced during an outage is dropped
//     rather than back-pressuring the encoder.
//
//   - config-interval=-1 puts SPS/PPS in front of every IDR so M2L-X can re-lock
//     mid-stream.
func sendDescription(encoderName string, audioBitrateBps int) string {
	return "" +
		"mpegtsmux name=" + nameMux + " alignment=7 pcr-interval=3600" +
		" ! queue name=" + nameSRTQueue + " leaky=downstream max-size-buffers=4000\n" +

		"proxysrc name=" + nameVideoProxySrc +
		" ! " + encoderName + " name=" + nameVideoEncod +
		" ! video/x-h264,profile=high" +
		" ! h264parse config-interval=-1" +
		" ! video/x-h264,stream-format=byte-stream,alignment=au" +
		" ! queue name=" + nameMuxVideoQueue + " max-size-time=1000000000 ! " + nameMux + ".\n" +

		"proxysrc name=" + nameAudioProxySrc +
		" ! " + seamAudioCaps +
		" ! " + aacEncoderFactory + " bitrate=" + strconv.Itoa(audioBitrateBps) +
		" ! aacparse ! audio/mpeg,mpegversion=4,stream-format=adts" +
		" ! queue name=" + nameMuxAudioQueue + " max-size-time=1000000000 ! " + nameMux + "."
}
