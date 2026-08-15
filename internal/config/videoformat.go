// The videoFormatOverride grammar: one string, one parser, one canonical form.
//
// Owner: WP-1, with internal/config. It is a file of its own rather than
// eighty more lines in config.go because it is the only thing in this package
// that PARSES anything — everything else here reads a number, a port or an
// enum out of JSON and checks its range — and because the argument for the
// representation is long enough that burying it between two struct fields
// would lose it.
//
// # What this exists to decide
//
// M2L-X can be configured into any video format and every source feeding it
// must match; the format is derivable from switcher_status only while some node
// is actually streaming (internal/m2lx parses state.streams.video.format for
// exactly that), and it is derivable from nothing at all when none is.
// MEASURED against the live matchH instance, 2026-08-15: all 35 nodes reported
// "format": null with stream_state "stopped" — our own commentary input, cam4
// "COMMS", among them — and seven plausible REST paths for an instance-wide
// setting all answered 404. So a position that comes up first, which is the
// normal case, has to be TOLD, and config.Config.VideoFormatOverride is where
// it is told. This file is what turns what the operator typed into numbers a
// capsfilter can be built from.
//
// # Why a string and not a struct
//
// The full argument is on the field in config.go. In one line each: a string
// keeps an unexpected value VISIBLE instead of turning it into three plausible
// zeros — the same reason m2lx.VideoFormat carries Raw beside its parsed
// fields — and a string cannot HALF-ARRIVE through this package's
// unmarshal-onto-the-live-struct merge, where a nested {"width":1280} would
// leave the old height in place and conform to 1280x1080.
//
// # WHAT IS GIVEN UP BY THAT CHOICE, stated rather than glossed
//
// A string can be misspelled and a struct cannot. Everything below is the price
// of that, and it is paid in one place so that no caller has to think about it:
// ONE grammar, ONE parser, and config.Validate refusing a value this function
// rejects — with the field name, the value and the accepted form in the message
// — so that a typo is a sentence on the Settings screen and never a
// not-negotiated (-4) from inside GStreamer half a minute after START.
//
// The second thing given up is expressiveness, deliberately. The grammar below
// describes progressive video and nothing else. It cannot say interlaced, it
// cannot say a colour space, a bit depth or a chroma sampling, and it does not
// accept a raw n/d fraction. Each of those is refused rather than tolerated,
// because a format this application accepts and then cannot produce is worth
// less than one it declines to accept — see the interlace refusal below, which
// is the case that will actually turn up.
package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// VideoFormatExample is the canonical spelling, used in every message that has
// to show the operator what the field wants and in the Settings screen's hint.
// One string, so the form the app asks for cannot drift between the three
// places it is quoted.
const VideoFormatExample = "1920x1080p50"

// Video dimension and frame-rate bounds. They are sanity limits, not a
// statement about what any encoder supports: their whole job is to turn a
// mistyped number into a message about this field instead of a caps string
// nothing can negotiate.
//
// 8192 covers 8K (7680x4320) with room above it. 1000 fps is far beyond any
// broadcast format and still refuses the kind of value a slipped keypress
// produces.
const (
	maxVideoDimension = 8192
	maxVideoFrameRate = 1000
)

// VideoFormatSpec is a parsed videoFormatOverride: the conform target for the
// contribution feed's video leg.
//
// It is NOT called VideoFormat, and the difference matters at the one place the
// two types meet. internal/m2lx.VideoFormat is what the SWITCHER REPORTS a node
// is currently carrying — a measurement, possibly of a format this application
// has never heard of, which is why it keeps Raw. This is what the operator has
// DECLARED the switcher is configured for. The derive-or-override decision has
// one of each in scope, and two types called VideoFormat in that function would
// be an invitation to assign the wrong one.
//
// Raw is kept for the same reason m2lx.VideoFormat keeps its own: it is what
// the operator typed, and it is what an error message, a log line or a Settings
// field should show them. Canonical() is what to show when the app is stating
// the format itself.
type VideoFormatSpec struct {
	// Raw is the value as it was written, trimmed of surrounding space and
	// nothing else. "1920X1080P50" stays upper-case here and normalises only in
	// Canonical.
	Raw string

	// Width and Height in pixels, both at least 1.
	Width  int
	Height int

	// FrameRateNum and FrameRateDen are the frame rate as an EXACT fraction:
	// 50/1, or 60000/1001 for the NTSC family. Den is never zero on a spec that
	// came out of ParseVideoFormat.
	//
	// A fraction rather than a float because that is what the format actually
	// is and what GStreamer's framerate field takes. 59.94 is not a frame rate;
	// it is a two-decimal rounding of 60000/1001, and a pipeline built from the
	// rounding is a pipeline whose timestamps drift against the switcher's by a
	// part in ten thousand for the length of a match.
	FrameRateNum int
	FrameRateDen int
}

// ParseVideoFormat parses "1920x1080p50" — <width>x<height>p<frame rate> — into
// a VideoFormatSpec. It is the ONLY parser of this grammar; nothing else in the
// application may take this string apart.
//
// The comparison is case-insensitive ("1920X1080P50" is accepted, because a
// capital X is what a keyboard produces as often as not) and surrounding space
// is ignored. Everything else is refused, and every refusal names the value and
// says what the field wants — this function's errors are read by an operator on
// the Settings screen, not by a developer in a log.
//
// FRAME RATES. Whole numbers are exact: p50 is 50/1. The NTSC family is
// accepted as the decimals people actually write and converted to the fraction
// it really is — 59.94 → 60000/1001, 29.97 → 30000/1001, 23.98 and 23.976 →
// 24000/1001 — because a broadcaster writes 59.94 and means 60000/1001, and a
// field that refused the spelling everyone uses would be a field nobody could
// fill in. Any other decimal is refused rather than rounded: 50.5 is not a
// format, it is a typo, and quietly conforming to 50.5/1 would be this
// function inventing a video standard.
func ParseVideoFormat(raw string) (VideoFormatSpec, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		// Reachable only from a caller that did not check for empty first —
		// Config.VideoFormatOverrideSpec and Validate both do, because empty is
		// the DEFAULT and means "derive from the switcher", not "wrong".
		return VideoFormatSpec{}, fmt.Errorf(
			"is empty; write it as %q, or leave it blank to take the format from the switcher",
			VideoFormatExample)
	}

	lower := strings.ToLower(s)
	if strings.ContainsAny(lower, " \t") {
		return VideoFormatSpec{}, fmt.Errorf("%q has spaces in it; write it as one word, e.g. %q", s, VideoFormatExample)
	}

	x := strings.IndexByte(lower, 'x')
	if x < 0 {
		return VideoFormatSpec{}, fmt.Errorf(
			"%q is not a video format; write it as <width>x<height>p<frame rate>, e.g. %q", s, VideoFormatExample)
	}

	rest := lower[x+1:]
	scan := strings.IndexAny(rest, "pi")
	if scan < 0 {
		return VideoFormatSpec{}, fmt.Errorf(
			"%q does not say the frame rate; write it as <width>x<height>p<frame rate>, e.g. %q", s, VideoFormatExample)
	}

	// INTERLACE IS REFUSED, AND THAT IS THE POINT OF READING THE LETTER AT ALL.
	//
	// 1080i25 is a real M2L-X configuration and somebody will type it. This
	// application's video leg produces progressive frames and there is no
	// interlacer in either bundler's element list, so accepting the string would
	// buy nothing but a later failure with a worse message. The refusal names
	// the letter, so an operator whose switcher really is interlaced learns that
	// this is a limitation of the app rather than a rejected spelling.
	if rest[scan] == 'i' {
		return VideoFormatSpec{}, fmt.Errorf(
			"%q is interlaced (the \"i\"); this application can only send progressive video, so an "+
				"interlaced switcher cannot be conformed to from here — write a progressive format such as %q "+
				"if the switcher can be set to one", s, VideoFormatExample)
	}

	width, err := parseVideoDimension(lower[:x], "width", s)
	if err != nil {
		return VideoFormatSpec{}, err
	}
	height, err := parseVideoDimension(rest[:scan], "height", s)
	if err != nil {
		return VideoFormatSpec{}, err
	}
	num, den, err := parseVideoFrameRate(rest[scan+1:], s)
	if err != nil {
		return VideoFormatSpec{}, err
	}

	return VideoFormatSpec{
		Raw:          s,
		Width:        width,
		Height:       height,
		FrameRateNum: num,
		FrameRateDen: den,
	}, nil
}

// parseVideoDimension reads the width or the height. what names which one, and
// whole names the value being parsed, so a message can quote both the part that
// is wrong and the format it came out of.
func parseVideoDimension(part, what, whole string) (int, error) {
	if part == "" {
		return 0, fmt.Errorf("%q has no %s; write it as <width>x<height>p<frame rate>, e.g. %q", whole, what, VideoFormatExample)
	}
	if !isAllDigits(part) {
		return 0, fmt.Errorf("%q has a %s of %q, which is not a whole number of pixels; e.g. %q",
			whole, what, part, VideoFormatExample)
	}
	// Atoi on an all-digit string fails only on overflow, which the bound below
	// would refuse anyway; it is checked rather than ignored so a 30-digit
	// paste cannot arrive as a negative.
	v, err := strconv.Atoi(part)
	if err != nil || v < 1 || v > maxVideoDimension {
		return 0, fmt.Errorf("%q has a %s of %q; it must be between 1 and %d pixels, e.g. %q",
			whole, what, part, maxVideoDimension, VideoFormatExample)
	}
	return v, nil
}

// parseVideoFrameRate reads the part after the "p" into an exact fraction. See
// ParseVideoFormat's header for which spellings are accepted and why the NTSC
// decimals are among them.
func parseVideoFrameRate(part, whole string) (num, den int, err error) {
	if part == "" {
		return 0, 0, fmt.Errorf("%q has no frame rate after the \"p\"; e.g. %q", whole, VideoFormatExample)
	}

	if isAllDigits(part) {
		v, err := strconv.Atoi(part)
		if err != nil || v < 1 || v > maxVideoFrameRate {
			return 0, 0, fmt.Errorf("%q has a frame rate of %q; it must be between 1 and %d, e.g. %q",
				whole, part, maxVideoFrameRate, VideoFormatExample)
		}
		return v, 1, nil
	}

	// The NTSC family. A decimal is accepted ONLY when it is the rounding of
	// some n*1000/1001 — which is what 23.98, 23.976, 29.97 and 59.94 all are —
	// and the exact fraction is what gets stored. The tolerance is 0.005, which
	// is wide enough for the two-decimal spellings people write (23.98 is 0.004
	// away from 24000/1001) and narrow enough that no rate anybody means as
	// itself falls inside it.
	f, ferr := strconv.ParseFloat(part, 64)
	if ferr != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > maxVideoFrameRate {
		return 0, 0, fmt.Errorf("%q has a frame rate of %q, which is not a number of frames per second; e.g. %q",
			whole, part, VideoFormatExample)
	}
	n := int(math.Round(f * 1001 / 1000))
	if n >= 1 && n <= maxVideoFrameRate && math.Abs(f-float64(n)*1000/1001) <= 0.005 {
		return n * 1000, 1001, nil
	}
	return 0, 0, fmt.Errorf(
		"%q has a frame rate of %q; whole numbers (24, 25, 30, 50, 60) and the NTSC rates "+
			"(23.98, 29.97, 59.94) are the frame rates there are, e.g. %q", whole, part, VideoFormatExample)
}

// isAllDigits reports whether every byte is an ASCII digit — and that the
// string is not empty, so a caller cannot read "" as a number.
//
// It is used instead of leaving the job to strconv because strconv.Atoi
// accepts a leading sign and an underscore separator, and "+50" or "5_0" as a
// frame rate is a value that parses and means the operator typed something
// else.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// FrameRate is the frame rate as a float, for a message or a comparison. It is
// NOT what a pipeline should be built from — use FrameRateNum/FrameRateDen, or
// FrameRateFraction — because 60000/1001 has no exact float.
//
// Zero on the zero value, rather than a division by zero: a VideoFormatSpec{}
// is what a caller holds when there is no override, and the zero value must be
// safe to ask questions of.
func (f VideoFormatSpec) FrameRate() float64 {
	if f.FrameRateDen == 0 {
		return 0
	}
	return float64(f.FrameRateNum) / float64(f.FrameRateDen)
}

// FrameRateFraction renders the frame rate as "50/1" or "60000/1001": the form
// GStreamer's framerate field takes. It is a formatter and not a caps builder —
// assembling caps belongs to internal/gst, which is the only package that knows
// what else has to be in them.
func (f VideoFormatSpec) FrameRateFraction() string {
	return strconv.Itoa(f.FrameRateNum) + "/" + strconv.Itoa(f.FrameRateDen)
}

// Canonical renders the spec in the grammar's own spelling: "1920x1080p50",
// "1280x720p59.94". This is what to print when the APPLICATION is stating the
// format; Raw is what to print when quoting the operator back to themselves.
func (f VideoFormatSpec) Canonical() string {
	return fmt.Sprintf("%dx%dp%s", f.Width, f.Height, f.frameRateText())
}

// String makes a spec printable with %v and %s. It is Canonical, not Raw:
// anything that formats a spec without asking is the application talking, and
// the application should use its own spelling.
func (f VideoFormatSpec) String() string {
	return f.Canonical()
}

// frameRateText is the frame rate as it appears in Canonical: a whole number
// where the denominator is 1, and otherwise the familiar two- or three-decimal
// NTSC spelling with the trailing zeros taken off (59.94, not 59.940).
func (f VideoFormatSpec) frameRateText() string {
	if f.FrameRateDen == 1 {
		return strconv.Itoa(f.FrameRateNum)
	}
	s := strconv.FormatFloat(f.FrameRate(), 'f', 3, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// THERE IS DELIBERATELY NO ErrNoVideoFormat SENTINEL, and this note is here so
// that nobody adds one. "No override is set" is not an error condition — it is
// the DEFAULT, and it means derive the format from the switcher.
// Config.VideoFormatOverrideSpec reports it as (spec, false, nil), a shape a
// caller cannot mistake for a failure. A sentinel would invite exactly that
// mistake: every caller receiving one would have to remember it was not a
// problem, and the first one that forgot would refuse to start a feed over a
// field the operator had correctly left blank.
