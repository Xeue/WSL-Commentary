// coughmute.go is the COUGH MUTE: the one control that takes the commentator's
// microphone off the SEND PATH — what M2L-X receives — and puts it back.
//
// It carries no build tag. The element name, the placement rule and the
// argument for the mechanism are ordinary Go that Gate A compiles and reads;
// the two halves that touch GStreamer are gst_cgo.go's SetCommentaryMute and
// gst_stub.go's twin, exactly as channelmap.go is the model and gst_cgo.go's
// SetChannelMap is the write.
//
// # What it is
//
// One `volume` element in the audio leg, named coughmute, with its `mute`
// property written live. Nothing else changes: no state change, no
// renegotiation, no element added to or removed from a running graph, no
// property shared with anything else.
//
// # Why a volume element and not the other two candidates
//
// Four requirements decide it. The mute must be INSTANT, because a cough does
// not wait. It must not renegotiate caps or disturb the encoder. It must not
// produce a discontinuity the far end reads as an error. And it must be
// UNAMBIGUOUS — the application must always be able to say truthfully whether
// audio is going out, which means the state has to be READ rather than assumed.
//
// ## The mix matrix, which is the candidate that looks free
//
// audioconvert's mix-matrix is already live-settable at 119 us with no
// renegotiation (channelmap.go has the measurement), so zeroing it looks like a
// mute that costs no elements at all. Measured here on 2026-08-16, it does even
// work: a zero 2x2 matrix on a stereo source read
//
//	rms=< -699.99999984363217, -699.99999984363217 >
//
// out of a level element downstream, which is digital silence. It is still the
// wrong mechanism, for three reasons and any one of them is enough.
//
//  1. IT WOULD NOT EXIST ON MOST SEATS. A matrix is written only for an
//     UNPOSITIONED capture source — a DeckLink card's sixteen channels with
//     channel-mask=0x0. Every native microphone, every Dante endpoint and the
//     whole of the on-air Windows path present a POSITIONED stereo pair, for
//     which applyStartChannelMapLocked deliberately writes nothing and
//     SetChannelMap refuses by contract, because installing a matrix live on
//     such a pipeline renegotiates the very caps the feed is running on. A
//     cough mute that is missing on the seats that are broadcasting today is
//     not a cough mute.
//
//  2. THE ROUTING AND THE MUTE WOULD SHARE ONE PROPERTY AND COULD DISAGREE.
//     Two independent controls — the mapping panel and the cough button —
//     would write the same mix-matrix, and each write is a whole matrix rather
//     than a delta. A mapping change made during a cough either restores the
//     routing (audio on air mid-cough) or is silently lost. There is no
//     ordering of the two writers that has neither failure, because the
//     property cannot express both facts at once.
//
//  3. THE STATE COULD NOT BE READ BACK. mix-matrix is a GST_TYPE_ARRAY and does
//     not marshal back out of the element — gst_cgo.go's matrixWidth comment
//     already says so, and it is why no copy of the running map is kept. So the
//     application could only ever report the mute it BELIEVES it wrote. "Is the
//     microphone open?" is the one question in this product that must never be
//     answered from memory.
//
// ## The valve, which is the candidate that looks obvious
//
// valve drop=true stops buffers dead, which is exactly the problem: the audio
// elementary stream STOPS. Measured here on 2026-08-16, the same 470-buffer
// source through the shipped encoder chain into mpegtsmux:
//
//	valve drop=true  ->  0 bytes of transport stream
//
// The muxer received nothing to mux. In the full pipeline the video PID would
// carry on and the audio PID would simply cease, then resume on un-mute with a
// PTS jump the size of the cough — a discontinuity the far end is entitled to
// read as an error, on a product whose central measured bug was already
// timestamps that jumped. The encoder is starved for the duration too, which is
// the thing gst_cgo.go's file comment exists to prevent.
//
// ## The volume element, and what was measured
//
// Same source, same shipped encoder chain (atenc, aacparse, ADTS, mpegtsmux),
// muted and unmuted, on 2026-08-16 on the port machine:
//
//	                     packets  first_pts   last_pts    max gap
//	volume mute=false      473    3600.0000  3610.069322  0.021344 s
//	volume mute=true       473    3600.0000  3610.069322  0.021344 s
//
// IDENTICAL. Same number of AAC access units, same first PTS, same last PTS,
// same largest interval between packets — one 1024-sample AAC frame at 48 kHz,
// which is the floor. The far end receives a continuous audio stream that
// happens to be silent. There is nothing for it to recover from, because
// nothing was interrupted.
//
// The `mute` property is flagged `readable, writable, controllable` on the
// element, so the state can be read straight back out of the running graph —
// which is what SetCommentaryMute does after every write, and what makes
// CommentaryMuted an observation rather than a recollection.
//
// ## And what it does on the SHIPPED pipeline, with the real card
//
// The measurements above are of the mechanism. This one is of the product:
// this package's own Start, the DeckLink commentary leg with its sixteen
// unpositioned channels and its mix matrix, the slate picture and the clock
// companion, on the fitted UltraStudio 4K Mini on 2026-08-16
// (TestLiveCoughMuteOnAPlayingPipeline):
//
//	mute write                109.792 us
//	unmute write              101 us
//	pipeline state            PLAYING throughout, nothing pending, both writes
//	bus errors                0
//
//	                    frames   peak dBFS   rms dBFS
//	  unmuted (before)     30     -57.0538   -66.4568
//	  MUTED                30    -100.0000  -100.0000
//	  unmuted (after)      30     -56.3296   -66.2508
//
// Thirty frames in every window: the programme meter did not slow down, it read
// silence. -100 rather than -700 is levels.go's clamp — clampLevelDB floors the
// element's digital-silence reading because -inf does not survive JSON.
//
// ## The measurement error that nearly buried all of this, recorded on purpose
//
// The first run of that test reported the muted window at -57.4 dBFS — the
// card's noise floor, indistinguishable from unmuted — and read as a mute that
// did not work at all. It was a fault in the measurement, not in the mechanism.
// The meter helper takes a peak HOLD, the loudest frame in the window, and level
// reports over 50 ms windows: the ONE window the property write lands inside
// contains audio from both sides of it, so it reads as loud as the unmuted
// stream and sets the hold for the entire window after it.
//
// It was settled by a third measurement rather than by an argument —
// TestLiveVolumeMuteWrittenOnAPlayingPipeline, which writes mute on a running
// synthetic pipeline and reads -12.0066 dBFS before and -100.0000 dBFS after,
// for a volume element built at unity (a GstBaseTransform PASSTHROUGH) and for
// one built just below it, identically. Passthrough is not a hazard here: the
// element leaves it when the mute is written.
//
// Anyone re-measuring this must discard one meter window either side of the
// write. That is a property of a 50 ms meter, not a latency: the write itself is
// the hundred microseconds above.
//
// # Where it sits, and why that is not a detail
//
// UPSTREAM OF alevel AND DOWNSTREAM OF chlevel.
//
// alevel is the programme meter, and pipelineDescription's comment on it is
// explicit that it measures the exact signal entering the AAC encoder because
// "measuring upstream of the resample would keep a meter moving while the
// on-air signal was silence, which is a reassurance the operator must never be
// given". A cough mute placed BELOW that meter would recreate precisely that
// failure by hand: the commentator coughs, the mute engages, and the programme
// meter goes on bouncing to a voice nobody is receiving.
//
// So the mute goes above it, and the meter tells the truth for free. Measured,
// on the same chain, at a 50 ms interval:
//
//	unmuted   rms=< -12.006563271339424, ... >   89 level messages
//	muted     rms=< -699.99999984363217, ... >   89 level messages
//
// The same number of messages either way: the meter does not freeze, it reads
// silence. A frozen meter and a silent one look identical on screen for the
// first second and mean completely different things.
//
// chlevel — the per-channel picker the mapping panel draws — stays ABOVE the
// mute, so it goes on showing which of a card's sixteen inputs the commentator
// is on even while they are coughing. That is the right way round: the picker
// answers "which channel is this person" and the answer does not change because
// they are off air for two seconds.
//
// # Two writers, one state, and why there are not two
//
// The mute has exactly one route before Start (PipelineOpts.MuteCommentary,
// applied while the pipeline is still in NULL, so a pipeline that is meant to
// be born muted never carries one buffer of live commentary) and exactly one
// route afterwards (SetCommentaryMute). SetCommentaryMute REFUSES on a pipeline
// that has not started, rather than latching an intent for Start to pick up,
// and the refusal is the whole point: two places that both remember the wanted
// mute are two places that can disagree, which is the second of the three
// charges laid against the mix-matrix above. It would be inconsistent to
// convict that mechanism of it and then build it here.
//
// # Surviving a reconnect
//
// An SRT drop does NOT rebuild this pipeline. internal/sender's loop calls
// RemoveSink, waits out the backoff ladder, then ReplaceSink; the pipeline
// object is created once per session in the application layer. Everything
// upstream of srtq — which is every element in the audio leg, this one included
// — stays in PLAYING for the life of the process, and that is the single most
// important property of gst_cgo.go. So the cough mute survives every reconnect
// by construction, not by re-application: there is no code path in a reconnect
// that touches this element, and TestReconnectDoesNotTouchTheCoughMute reads
// the two sink methods to keep it that way.
//
// What a reconnect cannot do, a REBUILD can: the latched-fatal path tears the
// session down and the next Start is a different Pipeline with no memory. That
// is why the state is readable and why Start takes it as an option — the caller
// reads CommentaryMuted off the pipeline it is discarding and hands it to the
// next one as PipelineOpts.MuteCommentary, and the new pipeline is muted before
// its first buffer rather than a moment after it.
package gst

// nameCoughMute is the volume element the cough mute writes, and the name is
// load-bearing in the two ways every name in this graph is.
//
// GetByName finds the element with it, so it must match pipelineDescription
// exactly — TestCoughMuteElementIsInThePipeline reads both and fails by name if
// they ever drift, because a lookup that returns nil is a cough button that does
// nothing on air.
//
// capturefault.go's classifier decides by name, and this one is deliberately
// NOT given any of the spared prefixes. It carries neither videoCaptureNamePrefix
// nor previewNamePrefix, and it is not captureAudioSrcName, so an error posted
// by it lands in classifyBusError's FATAL default. That is correct rather than
// careless: this element is on the send path between the resampler and the AAC
// encoder, and a volume element that has failed there is a pipeline that is no
// longer carrying commentary. TestCoughMuteNameCannotBeConfusedWithTheFeed pins
// the non-collision.
const nameCoughMute = "coughmute"

// propMute is the gboolean on that element. It is `readable, writable,
// controllable`, and the readable half is what CommentaryMuted is built on.
const propMute = "mute"
