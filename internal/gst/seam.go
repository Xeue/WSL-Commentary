// seam.go is the CAPTURE/SEND SEAM: the caps contract, the element names and
// the queue policies that the always-live capture pipelines and the per-session
// send pipeline both have to agree about, written down in one place because
// NEITHER SIDE MAY CHANGE THEM ALONE.
//
// The seam exists because of R1: the preview, the input meters, the channel
// routing and the signal lamp have to be live before START and survive STOP, so
// capture cannot be built and destroyed with the session. proxysink/proxysrc is
// what lets one always-live capture pipeline hand buffers to a send pipeline
// that is minted at START and destroyed at STOP.
//
// # What is on either side of it
//
//	CAPTURE (always live)                 SEND (built at START)
//	 ... ! queue vproxq ! proxysink  -->  proxysrc vproxsrc ! venc ! ... ! mux.
//	 ... ! queue aproxq ! proxysink  -->  proxysrc aproxsrc ! caps ! atenc ! mux.
//
// The two sides never share a Go type, a lock or a bus. They share exactly four
// things, and all four are in this file: the audio caps, the six element names,
// the two queue policies, and the arming rule that keeps the second send session
// carrying media.
//
// # The three invariants a later reader must not undo
//
//  1. THERE IS A LEAKY QUEUE IN FRONT OF EVERY PROXYSINK. proxysrc's own
//     internal queue is leaky=0 max-size-buffers=200 and exposes no tuning at
//     all — only async-handling, message-forward, name, parent and proxysink —
//     so the ONLY place a send-side stall can be absorbed is on the capture
//     side. Measured on the real card, identical graphs, a 12 s send-side wedge:
//
//     capture queue    card fps   preview fps   meter msg/s
//     leaky=downstream     50.1          36.2          20.0
//     leaky=no             11.6           7.2           7.2  + "Dropped 271 old
//     frames" from the card
//
//     Without the leak a wedged send pipeline destroys the preview and the
//     meters — the two things R1 exists to deliver — and makes the card itself
//     drop packets. Do NOT reach into proxysrc's queue0 by child name; the leak
//     lives here.
//
//  2. THE PROXYSINKS ARE RE-ARMED AT START, NOT AT STOP. See armProxySink and
//     the paragraph below.
//
//  3. A DEVICE CHANGE IS REFUSED WHILE SENDING. That is a safety property and
//     not a UX preference: a new proxysink orphans the existing proxysrc and the
//     feed goes silently dead, with SRT still connected and every lamp green.
//
// # The hazard this seam has, and the arming that answers it
//
// gstproxysink.c:134-139 resets sent_stream_start / sent_caps only on
// READY->PAUSED. A proxysink living in an always-live pipeline never makes that
// transition again, so EVERY PROXYSRC AFTER THE FIRST RECEIVES NO STREAM_START,
// NO CAPS AND NO SEGMENT. Measured twice independently, then reproduced in Go on
// both rigs with arming deliberately skipped:
//
//	cycle 1 (UNARMED): 1133076 bytes   <- the first consumer always works
//	cycle 2 (UNARMED): 0 bytes
//	cycle 3 (UNARMED): 0 bytes
//
// SRT still connects, the lamp still goes green, and the switcher receives
// silence. That is the permanent-false-green class this codebase is organised
// against, firing on the ordinary operator action of pressing STOP and START.
//
// armProxySink (proxy_cgo.go) is the fix, and CapturePipeline.ArmForSend runs it
// at START. With it, on the real card, three armed cycles plus one control:
// 1025164 / 1022532 / 1027796 bytes armed, 0 bytes unarmed, and the capture side
// never noticed — alevel's largest gap 68 ms against a 50 ms interval, chlevel's
// 100 ms against 100 ms, decklinkvideosrc at 29.84/30.17/29.97/29.98 fps with no
// dropped frames, across four build/run/destroy cycles of the send pipeline.
//
// # Single consumer, enforced in Go
//
// A SECOND proxysrc attaching to a live proxysink SILENTLY STEALS THE STREAM AND
// KILLS THE FIRST — measured, consumer A stopped dead at 5.994 s the instant
// consumer B attached at 6.007 s, with nothing on either bus. There is no
// refusal inside the element to lean on, so the rule is an atomic flag per
// proxysink: NewSend claims both or fails, and SendSeam.Stop releases both. See
// capture.go.
//
// This file is untagged and compiles at Gate A on purpose. Every string in it is
// read by both the cgo description builders and the Gate A tests that pin them,
// which is what stops the two sides of the seam drifting in a build nobody runs
// until a match.
package gst

import "strconv"

// seamAudioCaps is THE AUDIO CAPS CONTRACT: the exact format that crosses the
// seam, asserted on BOTH sides.
//
// It is pinned by the capture side's capsfilter above the cough mute, and
// asserted again by the send side's capsfilter immediately below aproxsrc. The
// second one buys the thing this whole file exists for: a violation becomes a
// loud negotiation failure at START rather than a silent wrong encode. Verified
// on this machine — atenc's sink template is exactly
// `S16LE, interleaved, channels: [1,8]`, so anything else crossing here would be
// refused by the encoder with an error naming neither side.
//
// It does not carry a channel-mask. The capture side has already collapsed
// whatever the device presented (1, 2, 3, 16 or 32 unpositioned channels) to a
// positioned stereo pair through the mix-matrix, and stating a mask here would
// pin a second fact about the same stream in a second place.
const seamAudioCaps = "audio/x-raw,format=S16LE,rate=48000,channels=2,layout=interleaved"

// The six element names that make up the seam.
//
// They are declared here, untagged, rather than beside gst_cgo.go's name block,
// for the reason capturefault.go's names are: the fault classifier has to know
// them at Gate A, where gst_cgo.go does not compile.
//
// The video head queue is vproxq and NOT vcapq. The old name said "the capture
// leg's head queue"; this one says "the queue in front of the proxysink", which
// is what it is on both the slate seat and the card seat, and it is what makes
// capturefault.go's vprox* entry cover the whole tail of the picture leg on a
// slate seat where nothing is named vcap-anything.
const (
	nameVideoProxyQueue = "vproxq"    // the picture leg's leaky head queue
	nameAudioProxyQueue = "aproxq"    // the commentary leg's leaky head queue
	nameVideoProxySink  = "vproxsink" // picture, capture side of the seam
	nameAudioProxySink  = "aproxsink" // commentary, capture side of the seam
	nameVideoProxySrc   = "vproxsrc"  // picture, send side of the seam
	nameAudioProxySrc   = "aproxsrc"  // commentary, send side of the seam
)

// The two prefixes capturefault.go classifies the seam's own elements on.
//
// They are prefixes rather than four exact names because the tail of each leg
// fails as one unit: a queue that errors and the proxysink below it are the same
// fault, and two constants that must never disagree are worse than one prefix.
//
// THEY MUST NOT COLLIDE WITH previewNamePrefix ("vprev") OR WITH
// videoCaptureNamePrefix ("vcap"), and they do not: "vprox" shares three letters
// with each and matches neither. That is checked by name in seam_test.go,
// because the failure mode is silent — a preview element that matched the video
// prefix would take the commentary off air on every fused seat.
const (
	videoProxyNamePrefix = "vprox"
	audioProxyNamePrefix = "aprox"
)

// videoProxyQueueBuffers is the picture leg's head-queue depth, and it is a
// JUDGEMENT rather than a measurement. It is flagged as one because the number
// beside it in the same file is not.
//
// What was MEASURED is that immunity to a send-side wedge comes from
// leaky=downstream and not from the depth (see the file header). The
// measured-immune configuration was max-size-time=1s, which for 1080p50 NV12 is
// 50 frames — about 155 MB, and it matches the observed RSS step of 141 MB to
// 306 MB across a single wedge. Eight buffers is about 25 MB and 160 ms, equally
// immune and far cheaper, so that is what ships.
//
// max-size-bytes and max-size-time are pinned to 0 beside it so that the buffer
// count is the only limit that can fire: a plain queue's default 10 MB bound is
// about three frames of 1080p NV12, which would make the real bound depend on
// the raster the operator's switcher happens to be configured for.
const videoProxyQueueBuffers = 8

// audioProxyQueueTimeNs is the commentary leg's head-queue depth: one second,
// bounded by TIME.
//
// One second of S16LE 48 kHz stereo is 192 kB, so the depth argument the picture
// leg has to make does not arise — this is cheap at any plausible bound. Time is
// the right axis for the same reason it is wrong for video: an audio buffer's
// size is a property of the element that produced it, and a count would make the
// real bound depend on the capture source's buffer-time.
//
// A3, answered by the operator on 2026-08-16: during a send-side stall the
// commentary is DROPPED, not delayed. A stall over about a second is already a
// reconnect-class event and late audio is useless to a live switcher. The
// consequence is written down rather than discovered — OnLevels' promise that no
// meter can move while silence goes to air holds in NORMAL OPERATION and NOT
// during a stall. See levels.go.
const audioProxyQueueTimeNs = 1000000000

// videoProxyQueue renders the picture leg's head queue.
//
// It is a function rather than a literal so that the description builder and the
// tests that pin it render through one expression: a queue policy that drifted
// between the string and its test would be a test that passes over a queue
// nobody configured.
func videoProxyQueue() string {
	return "queue name=" + nameVideoProxyQueue +
		" leaky=downstream" +
		" max-size-buffers=" + strconv.Itoa(videoProxyQueueBuffers) +
		" max-size-bytes=0 max-size-time=0"
}

// audioProxyQueue renders the commentary leg's head queue. See videoProxyQueue
// for why it is a function.
func audioProxyQueue() string {
	return "queue name=" + nameAudioProxyQueue +
		" leaky=downstream" +
		" max-size-time=" + strconv.Itoa(audioProxyQueueTimeNs) +
		" max-size-bytes=0 max-size-buffers=0"
}

// videoProxyTail is the whole of the picture leg below the conform target: the
// leaky queue and the proxysink, as one string, so that the slate leg and the
// card leg cannot drift on the one thing they genuinely share.
func videoProxyTail() string {
	return videoProxyQueue() + " ! proxysink name=" + nameVideoProxySink
}

// audioProxyTail is the same for the commentary leg.
func audioProxyTail() string {
	return audioProxyQueue() + " ! proxysink name=" + nameAudioProxySink
}
