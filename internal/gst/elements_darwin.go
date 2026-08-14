//go:build cgo && !gststub && darwin

// elements_darwin.go is the macOS half of the ELEMENT CONTRACT — the twin of
// elements_windows.go, which should be read first because it explains the shape
// of the seam and this file only explains where macOS differs.
//
// Owner: WP-3a.
//
// Two of the sixteen factories in the send pipeline change on macOS, and no
// others: the capture source and the AAC encoder. The H.264 encoder was already
// resolved at runtime, so it needs new lists rather than a new mechanism. The
// caps chain, the level element, mpegtsmux, both queues, aacparse, h264parse
// and srtsink are the same factories under the same plugin names — checked one
// by one against Homebrew's GStreamer 1.26.10 on macOS arm64 rather than
// assumed.

package gst

// captureSourceFactory is the element that opens the operator's microphone.
//
// osxaudiosrc is the CoreAudio capture source from gst-plugins-good's osxaudio
// plugin. It is the ONLY option: avfaudiosrc, which the GStreamer documentation
// discusses as an alternative, is NOT BUILT in the Homebrew package (checked —
// gst-inspect-1.0 avfaudiosrc finds nothing), so a pipeline written against it
// would fail at Start on the target machine.
//
// Its "device" property is a gint, not a string. That single fact is the reason
// deviceprovider_darwin.go exists in the form it does; read it before touching
// anything on this path.
const captureSourceFactory = "osxaudiosrc"

// aacEncoderFactory is the element that encodes the commentary audio.
//
// # This was chosen on licence first and measurement second, and both were done
//
// atenc is the exact analogue of mfaacenc: a thin LGPL GStreamer wrapper — it
// lives in libgstosxaudio.dylib, the same gst-plugins-good binary as
// osxaudiosrc, so the bundle gains ZERO new files for the encoder — over an
// AAC implementation that is part of the operating system (AudioToolbox;
// otool -L shows the plugin linking /System/Library/Frameworks/AudioToolbox
// and nothing else of interest). So build/licenses/NOTICE.txt section G's
// sentence, "AAC-LC encoding is done by mfaacenc, the Windows encoder, so no
// third-party AAC implementation and no separate AAC patent licence is involved
// on our side", stays literally TRUE on macOS with one word changed. Section F
// gains a line; the bundle gains nothing.
//
// The three alternatives were all present on the machine and all tested. The
// ranking below is on LICENCE, exactly as x264enc was denylisted, and only then
// on measurement — 500 buffers of white noise at a 128 kbit/s target, through
// the identical S16LE/48k/2ch caps this pipeline uses:
//
//	atenc       131.4 kbit/s   LGPL wrapper over AudioToolbox (part of macOS)
//	fdkaacenc   131.4 kbit/s   *** EXCLUDED, see below ***
//	faac        139.3 kbit/s   overshoots the target by 9 per cent, and libfaac
//	                           is LGPL-2.1 — a genuine third-party AAC
//	                           implementation, so it reopens the patent question
//	                           section G closes
//	avenc_aac   not tested     forbidden outright: gst-libav is EXCLUDED by
//	                           NOTICE.txt section G and by return_cgo.go's own
//	                           "No libav, ever" rule
//
// atenc and fdkaacenc are indistinguishable on rate. atenc is chosen because
// fdkaacenc cannot be shipped.
//
// # fdkaacenc's licence, stated accurately, because two sources state it wrongly
//
// It is worth being precise here, because the two places a hurried reader would
// look both give a permissive answer and both are wrong about the thing that
// matters:
//
//   - Homebrew's metadata for the fdk-aac formula says "Apache-2.0". It is not.
//   - gst-inspect-1.0 fdkaacenc says "License: LGPL". That field describes the
//     GStreamer WRAPPER plugin, not the codec library it links; libgstfdkaac.dylib
//     links /opt/homebrew/opt/fdk-aac/lib/libfdk-aac.2.dylib, which carries its
//     own terms.
//
// The actual licence, read from the NOTICE shipped with fdk-aac 2.0.3 on this
// machine, is the "Software License for The Fraunhofer FDK AAC Codec Library
// for Android". Its copyright grant is permissive-ish but carries two
// conditions a commercial binary deliverable cannot wave away — "You must make
// available free of charge copies of the complete source code of the FDK AAC
// Codec and your modifications thereto to recipients of copies in binary form",
// and "You may not charge copyright license fees for anyone to use, copy or
// distribute the FDK AAC Codec software". And then clause 3, which is the
// decisive one for a broadcast contribution product:
//
//	NO PATENT LICENSE (clause 3):
//	NO EXPRESS OR IMPLIED LICENSES TO ANY PATENT CLAIMS, including without
//	limitation the patents of Fraunhofer, ARE GRANTED BY THIS SOFTWARE LICENSE.
//	[...] You may use this FDK AAC Codec software or modifications thereto only
//	for purposes that are authorized by appropriate patent licenses.
//
// In other words shipping fdkaacenc would mean acquiring an AAC patent licence
// separately (Via Licensing, per the same NOTICE). That is precisely the
// obligation NOTICE.txt section G says this product does not have, and it is
// not one to take on to save nothing: the measurement above says it would buy
// 0.0 kbit/s of quality over the encoder Apple already ships.
const aacEncoderFactory = "atenc"

// platformRequiredElements are the two factories in requiredElements that are
// macOS-specific. Both come from the SAME plugin — libgstosxaudio.dylib — which
// is worth knowing when staging the bundle: the capture source and the audio
// encoder cost one file between them.
var platformRequiredElements = []requiredElement{
	{captureSourceFactory, "osxaudio"},
	{aacEncoderFactory, "osxaudio"},
}

// h264EncoderPreference is the order encoders are chosen in, lower index
// winning, REGARDLESS of rank — see the Windows twin for why rank alone is the
// wrong rule.
//
// Both VideoToolbox encoders are present and both rank primary (256) on this
// machine, so rank cannot choose between them and something has to.
//
// vtenc_h264 leads rather than vtenc_h264_hw for the same reason mfh264enc
// leads on Windows: it is the element that is always there. vtenc_h264 uses the
// hardware encode session where VideoToolbox offers one — which on Apple
// silicon is always — and falls back to VideoToolbox's software path where it
// does not, for instance inside a virtual machine or on a Mac whose media
// engine is already saturated. vtenc_h264_hw REQUIRES the hardware session and
// fails if it cannot get one, which is a fine property for a transcoder and a
// bad one twenty minutes before kick-off. It stays in the list as the fallback.
//
// x264enc is NOT in this list and must never be: it is present in a stock
// Homebrew GStreamer, it ranks primary (256), and it is GPL. The denylist in
// gst_cgo.go is what actually stops it, because a name that is merely absent
// from the preference list can still be selected by rank.
var h264EncoderPreference = []string{
	"vtenc_h264",
	"vtenc_h264_hw",
}

// h264EncoderFallbacks are tried by name, in this order, if the factory-list
// query returns nothing usable — belt and braces against factoryTypeMediaVideo
// being wrong.
//
// It is deliberately not longer than the preference list. openh264enc is on the
// Windows fallback list but is NOT BUILT in the Homebrew package, and there is
// no third H.264 encoder on macOS that is both present and shippable.
var h264EncoderFallbacks = []string{
	"vtenc_h264",
	"vtenc_h264_hw",
}

// h264EncoderProps are the same four intentions as the Windows list, spelled in
// vtenc_h264's vocabulary. Every name was read from gst-inspect-1.0 rather than
// guessed, because applyEncoderProperties SKIPS a property the element does not
// have — so a misspelling here does not fail, it silently encodes at defaults.
//
//	rc-mode=cbr             ->  rate-control=cbr
//	                            Same intention, and vtenc's enum has exactly two
//	                            members: abr (the default) and cbr. Without this
//	                            a static slate under average-bitrate control
//	                            drifts down and bursts at every IDR, which is the
//	                            "is it flowing" problem the Windows file records.
//	gop-size=100            ->  max-keyframe-interval=100
//	                            A 2 s GOP at 50p. vtenc's default is 0, meaning
//	                            "let VideoToolbox decide", which on a still image
//	                            means almost never — and M2L-X cannot re-lock
//	                            mid-stream without an IDR.
//	low-latency=true        ->  realtime=true
//	                            Configures the encode session for realtime
//	                            output. vtenc defaults it to false, which is the
//	                            wrong default for a live contribution feed.
//	cabac=true              ->  (no equivalent; vtenc has no entropy-mode
//	                            property, and the High profile caps below it
//	                            already imply CABAC)
//
// allow-frame-reordering=false has no Windows counterpart and is not
// decoration. vtenc DEFAULTS it to true, and B-frames on a contribution feed
// cost reordering latency for nothing at all when the picture is a still slate.
// Windows gets the same result for free, because Media Foundation's H.264 MFT
// emits no B-frames in low-latency mode — which is exactly why the Windows list
// records that bframes=0 is unnecessary there. It IS necessary here.
var h264EncoderProps = []encoderProp{
	{"rate-control", "cbr"},
	{"max-keyframe-interval", "100"},
	{"realtime", "true"},
	{"allow-frame-reordering", "false"},
}
