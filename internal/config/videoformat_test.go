package config

import (
	"strings"
	"testing"
)

// The videoFormatOverride grammar's tests: what ParseVideoFormat accepts, what
// it refuses, and — the half that matters most on a match day — that every
// refusal says something an operator standing at a commentary position can act
// on.
//
// Owner: WP-1.

func TestParseVideoFormat_TheFormatsThatWillBeTyped(t *testing.T) {
	tests := []struct {
		in        string
		width     int
		height    int
		num, den  int
		canonical string
	}{
		// The measured healthy shape on the live instance, and the default the
		// application used to hard-code.
		{"1920x1080p50", 1920, 1080, 50, 1, "1920x1080p50"},
		{"1920x1080p25", 1920, 1080, 25, 1, "1920x1080p25"},
		{"1280x720p50", 1280, 720, 50, 1, "1280x720p50"},
		{"3840x2160p60", 3840, 2160, 60, 1, "3840x2160p60"},
		// A capital X and P: what a keyboard produces as often as not, and no
		// reason at all to refuse.
		{"1920X1080P50", 1920, 1080, 50, 1, "1920x1080p50"},
		// Surrounding space: a paste, not a mistake.
		{"  1920x1080p50\t", 1920, 1080, 50, 1, "1920x1080p50"},
		// THE NTSC FAMILY. The operator writes the two-decimal spelling and
		// means the exact fraction; the exact fraction is what is stored, because
		// a pipeline built from 59.94 drifts against the switcher by a part in
		// ten thousand for the length of a match.
		{"1920x1080p59.94", 1920, 1080, 60000, 1001, "1920x1080p59.94"},
		{"1920x1080p29.97", 1920, 1080, 30000, 1001, "1920x1080p29.97"},
		{"1920x1080p23.98", 1920, 1080, 24000, 1001, "1920x1080p23.976"},
		{"1920x1080p23.976", 1920, 1080, 24000, 1001, "1920x1080p23.976"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseVideoFormat(tt.in)
			if err != nil {
				t.Fatalf("ParseVideoFormat(%q) error = %v", tt.in, err)
			}
			if got.Width != tt.width || got.Height != tt.height {
				t.Errorf("size = %dx%d, want %dx%d", got.Width, got.Height, tt.width, tt.height)
			}
			if got.FrameRateNum != tt.num || got.FrameRateDen != tt.den {
				t.Errorf("frame rate = %d/%d, want %d/%d",
					got.FrameRateNum, got.FrameRateDen, tt.num, tt.den)
			}
			if got.Canonical() != tt.canonical {
				t.Errorf("Canonical() = %q, want %q", got.Canonical(), tt.canonical)
			}
			// Raw is what the operator typed, trimmed and NOT normalised. It is
			// the whole reason this is a string rather than a struct: an
			// unexpected value has to stay visible, so a message can quote it
			// back rather than reporting three plausible zeros.
			if got.Raw != strings.TrimSpace(tt.in) {
				t.Errorf("Raw = %q, want the trimmed input %q", got.Raw, strings.TrimSpace(tt.in))
			}
		})
	}
}

// TestParseVideoFormat_RefusesInterlace is its own test because it is the one
// refusal that will actually be met in the field: 1080i25 is a real M2L-X
// configuration, somebody will type it, and what they must be told is that this
// application cannot send interlaced video — not that they spelled something
// wrong.
func TestParseVideoFormat_RefusesInterlace(t *testing.T) {
	for _, in := range []string{"1920x1080i25", "1920X1080I25", "720x576i25"} {
		got, err := ParseVideoFormat(in)
		if err == nil {
			t.Errorf("ParseVideoFormat(%q) = %v, want an error: this build sends progressive video only", in, got)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "interlaced") {
			t.Errorf("ParseVideoFormat(%q) error = %v; it must say the format is interlaced, or the "+
				"operator reads it as a typo and retypes the same thing", in, err)
		}
		if !strings.Contains(msg, in) {
			t.Errorf("ParseVideoFormat(%q) error = %v; it must quote the value", in, err)
		}
	}
}

func TestParseVideoFormat_Refusals(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // a substring the message must carry
	}{
		{"empty", "", "blank"},
		{"prose", "1080p", "<width>x<height>"},
		{"no frame rate", "1920x1080", "frame rate"},
		{"nothing after the p", "1920x1080p", "frame rate"},
		{"spaces inside", "1920 x 1080 p50", "spaces"},
		{"width is not a number", "hdx1080p50", "width"},
		{"height is not a number", "1920xhdp50", "height"},
		{"zero width", "0x1080p50", "width"},
		{"absurd width", "99999x1080p50", "width"},
		{"zero height", "1920x0p50", "height"},
		{"zero frame rate", "1920x1080p0", "frame rate"},
		{"absurd frame rate", "1920x1080p100000", "frame rate"},
		// A decimal that is not an NTSC rate is a typo, and conforming to 50.5
		// frames per second would be this parser inventing a video standard.
		{"a decimal nobody means", "1920x1080p50.5", "NTSC"},
		// strconv would take both of these and produce a number the operator did
		// not type. isAllDigits is what stops that.
		{"a signed frame rate", "1920x1080p+50", "frame rate"},
		{"an underscore separator", "1920x1080p5_0", "frame rate"},
		// The old documented spelling of a format, and the shape switcher_status
		// actually sends. Neither is this field's grammar, and being told so is
		// better than a silent partial parse.
		{"the switcher's own rendering", "h264 1920x1080 50 P", "spaces"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseVideoFormat(tt.in)
			if err == nil {
				t.Fatalf("ParseVideoFormat(%q) = %v, want an error", tt.in, got)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ParseVideoFormat(%q) error = %v, want it to mention %q", tt.in, err, tt.want)
			}
		})
	}
}

// TestParseVideoFormatErrorsAreReadableByAnOperator is the test that keeps the
// messages useful rather than merely present. Every refusal is read on the
// Settings screen by somebody with a match starting, so it must quote what they
// typed and show the form the field wants — the two things that turn "invalid
// input" into an edit they can make.
func TestParseVideoFormatErrorsAreReadableByAnOperator(t *testing.T) {
	for _, in := range []string{"1080p", "1920x1080", "1920x1080p50.5", "hdx1080p50", "1920x1080i25"} {
		_, err := ParseVideoFormat(in)
		if err == nil {
			t.Fatalf("ParseVideoFormat(%q): want an error", in)
		}
		msg := err.Error()
		if !strings.Contains(msg, in) {
			t.Errorf("ParseVideoFormat(%q) error = %v; it must quote the value that was typed", in, err)
		}
		if !strings.Contains(msg, VideoFormatExample) {
			t.Errorf("ParseVideoFormat(%q) error = %v; it must show the accepted form (%s)",
				in, err, VideoFormatExample)
		}
	}
}

// TestVideoFormatFractionsAreExact pins the thing the fraction exists for. The
// float is for messages; the pipeline is built from the fraction.
func TestVideoFormatFractionsAreExact(t *testing.T) {
	spec, err := ParseVideoFormat("1920x1080p59.94")
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.FrameRateFraction(); got != "60000/1001" {
		t.Errorf("FrameRateFraction() = %q, want %q — 59.94 is a rounding of 60000/1001, not a frame rate",
			got, "60000/1001")
	}
	if got := spec.FrameRate(); got < 59.939 || got > 59.941 {
		t.Errorf("FrameRate() = %v, want about 59.94", got)
	}

	fifty, err := ParseVideoFormat("1920x1080p50")
	if err != nil {
		t.Fatal(err)
	}
	if got := fifty.FrameRateFraction(); got != "50/1" {
		t.Errorf("FrameRateFraction() = %q, want %q", got, "50/1")
	}
}

// TestVideoFormatZeroValueIsSafeToAsk: VideoFormatSpec{} is what a caller holds
// when there is no override, which is the DEFAULT and the common case. Asking
// it for a frame rate must not divide by zero.
func TestVideoFormatZeroValueIsSafeToAsk(t *testing.T) {
	var zero VideoFormatSpec
	if got := zero.FrameRate(); got != 0 {
		t.Errorf("VideoFormatSpec{}.FrameRate() = %v, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// The Config-level reader
// ---------------------------------------------------------------------------

// TestVideoFormatOverrideSpec_TheThreeAnswers pins the shape callers branch on:
// empty is not an error, a good value parses, and a bad value is REPORTED
// rather than quietly falling back to "derive". The last is the one that
// matters: deriving from a switcher with nothing streaming produces the 1080p50
// guess this field exists to replace, and doing that silently is how a feed
// that cannot negotiate becomes a mystery.
func TestVideoFormatOverrideSpec_TheThreeAnswers(t *testing.T) {
	var c Config

	spec, ok, err := c.VideoFormatOverrideSpec()
	if err != nil || ok {
		t.Errorf("empty override: got (%v, %v, %v), want the zero spec, false, nil", spec, ok, err)
	}

	// Whitespace is empty. A hand-edited file with a stray space in the value
	// must mean "derive", not "a format I cannot parse".
	c.VideoFormatOverride = "   "
	if _, ok, err := c.VideoFormatOverrideSpec(); err != nil || ok {
		t.Errorf("whitespace override: ok = %v, err = %v; want false, nil", ok, err)
	}

	c.VideoFormatOverride = "1280x720p59.94"
	spec, ok, err = c.VideoFormatOverrideSpec()
	if err != nil || !ok {
		t.Fatalf("good override: got (%v, %v, %v)", spec, ok, err)
	}
	if spec.Width != 1280 || spec.Height != 720 || spec.FrameRateDen != 1001 {
		t.Errorf("good override parsed as %+v", spec)
	}

	c.VideoFormatOverride = "1920x1080i25"
	if _, ok, err := c.VideoFormatOverrideSpec(); err == nil || ok {
		t.Error("a value that cannot be parsed must be reported, never treated as \"derive\"")
	}
}
