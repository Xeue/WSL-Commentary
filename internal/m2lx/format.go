package m2lx

import (
	"strconv"
	"strings"
)

// parseVideoFormat parses the measured wire shape of
// <statusKey>.streams.video.format — "h264 1920x1080 50 P" — into
// VideoFormat's structured fields (docs/architecture.md line 406;
// docs/test-results.md §2.4 item 6 records the same shape for audio).
//
// Raw always carries the input verbatim, including when parsing partially
// or completely fails, so a format the parser does not understand is
// visible to the caller instead of silently reading as the zero value.
// Fields that cannot be parsed are left at their Go zero value; callers
// must not infer "good state" from a field being zero without also
// checking Raw for an unparsed or unexpected string.
func parseVideoFormat(raw string) VideoFormat {
	f := VideoFormat{Raw: raw}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return f
	}
	f.Codec = fields[0]
	if len(fields) >= 2 {
		if w, h, ok := parseDimensions(fields[1]); ok {
			f.Width = w
			f.Height = h
		}
	}
	if len(fields) >= 3 {
		if fr, err := strconv.ParseFloat(fields[2], 64); err == nil {
			f.FrameRate = fr
		}
	}
	// fields[3], when present, is the scan indicator ("P"/"I" in the
	// measured sample). VideoFormat has no field for it; Raw preserves it.
	return f
}

// parseDimensions parses a "<width>x<height>" token such as "1920x1080".
// It returns ok=false rather than panicking or returning a partial result
// on anything else, including a missing "x", a non-numeric side, or extra
// separators.
func parseDimensions(s string) (width, height int, ok bool) {
	parts := strings.SplitN(strings.ToLower(s), "x", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, errW := strconv.Atoi(parts[0])
	h, errH := strconv.Atoi(parts[1])
	if errW != nil || errH != nil {
		return 0, 0, false
	}
	return w, h, true
}

// parseAudioFormat parses the measured wire shape of one element of
// <statusKey>.streams.audio — "aac 48000 2ch" — into AudioFormat's
// structured fields. Same Raw-always, zero-on-failure contract as
// parseVideoFormat.
func parseAudioFormat(raw string) AudioFormat {
	f := AudioFormat{Raw: raw}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return f
	}
	f.Codec = fields[0]
	if len(fields) >= 2 {
		if sr, err := strconv.Atoi(fields[1]); err == nil {
			f.SampleRate = sr
		}
	}
	if len(fields) >= 3 {
		if ch, ok := parseChannelCount(fields[2]); ok {
			f.Channels = ch
		}
	}
	return f
}

// parseChannelCount parses a "<n>ch" token such as "2ch". It returns
// ok=false on anything that is not a bare integer with an optional
// trailing, case-insensitive "ch".
func parseChannelCount(s string) (int, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, "ch")
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}
