//go:build cgo && !gststub && windows

// elements_windows.go is the Windows half of the ELEMENT CONTRACT: which
// GStreamer factories the send pipeline is built from, what has to be in the
// bundle for them to exist, and which settings are applied to the encoder.
//
// Owner: WP-3a. Its twin is elements_darwin.go. The two files declare exactly
// the same six identifiers and nothing else; if one of them drifts the package
// stops compiling, which is the point of splitting them this way rather than
// branching on runtime.GOOS inside gst_cgo.go. A third platform needs a third
// file, and the failure that tells you so is a compile error naming the missing
// symbol — loud, at build time, in the one file that has to be written.
//
// Nothing in this file is a new decision. Every value is what the Windows build
// already had, lifted out of gst_cgo.go byte for byte along with the
// measurements that justify it, so that the darwin twin has somewhere to
// disagree without touching a line of Windows behaviour.

package gst

// captureSourceFactory is the element that opens the operator's microphone.
//
// wasapi2src rather than wasapisrc: the older element is deprecated upstream,
// and wasapi2 is the provider whose device enumeration deviceprovider_windows.go
// is written against.
const captureSourceFactory = "wasapi2src"

// aacEncoderFactory is the element that encodes the commentary audio.
//
// mfaacenc is Media Foundation's AAC-LC encoder, which is part of Windows. That
// is the whole argument: build/licenses/NOTICE.txt section G excludes fdkaac,
// faac and voaacenc on the grounds that "AAC-LC encoding is done by mfaacenc,
// the Windows encoder, so no third-party AAC implementation and no separate AAC
// patent licence is involved on our side", and the bundle therefore ships no
// AAC codec of its own. See elements_darwin.go for how that sentence is kept
// literally true on macOS.
const aacEncoderFactory = "mfaacenc"

// platformRequiredElements are the two factories in requiredElements that are
// Windows-specific. Both come from plugins in the specification section 3
// allowlist.
var platformRequiredElements = []requiredElement{
	{captureSourceFactory, "wasapi2"},
	{aacEncoderFactory, "mediafoundation"},
}

// h264EncoderPreference is the order encoders are chosen in, lower index
// winning, REGARDLESS of rank. Rank is used only to exclude factories GStreamer
// has marked unusable.
//
// # This reverses specification open question 3, on measurement
//
// OQ3 asked "is the highest-ranked H.264 encoder called mfh264enc on the target
// machine?" and instructed resolving by rank rather than hardcoding the name.
// That was the right instinct before anyone could measure it. Measured at Gate B
// on 2026-07-30, on a machine with an RTX 5070, GStreamer 1.28.5 ranks them:
//
//	nvh264enc    primary + 1 (257)
//	x264enc      primary (256)      <- denylisted, GPL
//	amfh264enc   primary (256)
//	mfh264enc    secondary (128)
//	openh264enc  marginal (64)
//
// So the answer to OQ3 is no: mfh264enc is NOT the highest-ranked encoder, and
// resolving by rank selects whichever GPU vendor's element happens to be
// installed. Three consequences make that the wrong choice here:
//
//  1. The property set in h264EncoderProps was written against mfh264enc, and
//     the values are applied only where the chosen factory has a property of
//     that name. On nvh264enc most of them are silently skipped, so the
//     deliberate CBR-not-QVBR decision — taken because a static slate under
//     variable rate collapses to 200-350 kbps and makes "is it flowing" hard to
//     observe — quietly does not happen.
//  2. It makes behaviour depend on the graphics card. Two commentary positions
//     running the same build would encode differently, and a fault reproducible
//     at one would not reproduce at the other. For a contribution path that has
//     to behave identically everywhere, that is a bad trade for encoder
//     efficiency that a 1920x1080 still frame cannot use.
//  3. mfh264enc is Media Foundation, which is part of Windows. It is present on
//     every target machine by definition, so preferring it costs no
//     availability.
//
// The hardware encoders stay in the list below mfh264enc, so a machine whose
// Media Foundation H.264 MFT is missing or broken still has somewhere to go
// rather than failing to start twenty minutes before kick-off.
var h264EncoderPreference = []string{
	"mfh264enc",
	"qsvh264enc",
	"nvh264enc",
	"d3d11h264enc",
	"amfh264enc",
}

// h264EncoderFallbacks are tried by name, in this order, if the factory-list
// query returns nothing usable. This is belt and braces against
// factoryTypeMediaVideo being wrong.
var h264EncoderFallbacks = []string{
	"mfh264enc",
	"qsvh264enc",
	"nvh264enc",
	"d3d11h264enc",
	"amfh264enc",
	"openh264enc",
}

// h264EncoderProps are the specification section 5 encoder settings other than
// bitrate.
//
// They are applied one at a time, and only to properties the chosen factory
// actually has, because the encoder is resolved at runtime and a different
// vendor's element will not have mfh264enc's property names.
//
// Why these values (specification section 5): rc-mode=cbr rather than
// quality-targeted, because a static slate under QVBR collapses to 200-350 kbps
// which is cheaper but bursty at every IDR and makes "is it flowing" harder to
// observe. gop-size=100 is a 2 s GOP at 50p, matching the profile M2L-X locked
// cleanly. low-latency=true because there is nothing to gain from reordering a
// slate.
//
// # bframes is deliberately absent, and the specification is wrong about it
//
// Specification section 5 sets bframes=0. Verified at Gate B on 2026-07-30:
// mfh264enc in GStreamer 1.28.5 HAS NO bframes PROPERTY. Its full property set
// is adapter-luid, bitrate, cabac, d3d11-aware, gop-size, low-latency,
// max-bitrate, max-qp, min-qp, qos, qp, qp-b, qp-i, qp-p, quality-vs-speed,
// rc-mode, ref and vbv-buffer-size.
//
// Listing it here would have been harmless — the apply loop skips properties
// the factory does not have — but it would have logged a warning on every
// single start, and a warning that always fires is one nobody reads. The
// specification's pipeline string is a real defect rather than a cosmetic one,
// because gst_parse_launch rejects an unknown property outright:
//
//	ERROR: no property "bframes" in element "mfh264enc"
//
// That is a parse failure, not a warning, so anyone pasting section 5 into
// gst-launch-1.0 to reproduce a fault gets nothing at all. It is corrected in
// the specification and in this package's doc comment.
//
// The intent behind bframes=0 still holds, and low-latency=true delivers it:
// Media Foundation's H.264 MFT does not emit B-frames in low-latency mode, so
// there is no reordering to remove.
var h264EncoderProps = []encoderProp{
	{"rc-mode", "cbr"},
	{"gop-size", "100"},
	{"low-latency", "true"},
	{"cabac", "true"},
}
