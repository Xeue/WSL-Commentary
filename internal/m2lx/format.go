package m2lx

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
)

// The format objects
// ------------------
//
// MEASURED, testdata/switcher_status-live-2026-07-31.json, cam22 while this
// app was streaming into it:
//
//	"video": {"error":{...},
//	          "format":{"bit_depth":8,"codec":"h264","color_space":"YCbCr",
//	                    "frame_rate":"50","height":1080,"sample_format":"420",
//	                    "scan_type":"P","width":1920}}
//	"audio": [{"error":{...},
//	           "format":{"bit_depth":0,"channel_count":2,"codec":"aac",
//	                     "sample_rate":48000}}]
//
// A structured OBJECT, not the string "h264 1920x1080 50 P" that
// docs/architecture.md line 406 and docs/test-results.md §2.4 recorded. This is
// strictly better to parse — the fields arrive already separated — but note the
// trap in it: frame_rate is a STRING while width and height beside it are
// NUMBERS. Nothing is assumed here that the capture does not show.
//
// On a node that is not streaming, format is JSON null — on all 22 stopped
// nodes in the capture. It is null, not absent and not an empty object, and it
// is null for audio as well as video.

// videoFormatKeys is the order Raw renders video format keys in: the four the
// lamp reads first, then the rest as measured. Keys not in this list are
// rendered after it in sorted order, so a field a future firmware adds appears
// rather than vanishing.
var videoFormatKeys = []string{
	"codec", "width", "height", "frame_rate",
	"scan_type", "bit_depth", "color_space", "sample_format",
}

// audioFormatKeys is videoFormatKeys' counterpart for audio.
var audioFormatKeys = []string{"codec", "sample_rate", "channel_count", "bit_depth"}

// parseVideoFormat parses state.streams.video.format into VideoFormat.
//
// It never fails. Raw always carries a rendering of whatever was actually
// there, including when nothing could be parsed out of it, so a format this
// package does not understand is visible to the operator on a red lamp instead
// of reading as a silent zero. Fields that cannot be parsed are left at their
// Go zero value; callers must not infer "good state" from a field being zero
// without also checking Raw.
func parseVideoFormat(raw json.RawMessage) VideoFormat {
	fields, ok := formatFields(raw)
	f := VideoFormat{Raw: renderFormat(raw, fields, videoFormatKeys)}
	if !ok {
		return f
	}
	f.Codec, _ = jsonString(fields["codec"])
	f.Width, _ = jsonInt(fields["width"])
	f.Height, _ = jsonInt(fields["height"])
	f.FrameRate, _ = parseFrameRate(fields["frame_rate"])
	return f
}

// parseAudioFormat parses one element's format from state.streams.audio into
// AudioFormat. Same never-fails, Raw-always contract as parseVideoFormat.
//
// bit_depth is measured as 0 on a healthy AAC stream, which is why it is
// rendered into Raw and not modelled as a field: a zero that means "not
// applicable to this codec" would read as a fault in any lamp that used it.
func parseAudioFormat(raw json.RawMessage) AudioFormat {
	fields, ok := formatFields(raw)
	f := AudioFormat{Raw: renderFormat(raw, fields, audioFormatKeys)}
	if !ok {
		return f
	}
	f.Codec, _ = jsonString(fields["codec"])
	f.SampleRate, _ = jsonInt(fields["sample_rate"])
	f.Channels, _ = jsonInt(fields["channel_count"])
	return f
}

// formatFields decodes a format value into its raw fields. ok is false for the
// three ways there is nothing to read: an absent format, a JSON null (the
// measured shape on every stopped node), and a format that is not an object at
// all. In all three the caller still renders Raw, so the difference between
// "no format" and "a format of some shape I have never seen" stays visible.
func formatFields(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if isJSONAbsent(raw) || len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false
	}
	return fields, true
}

// renderFormat produces the readable rendering that goes in Raw.
//
// One rule, so that reading it back needs no key to decode: every field
// present is rendered as key=<the JSON value verbatim>, known keys in the
// order given, then any others sorted. Values keep their JSON quoting, so a
// field arriving as the wrong type is unmistakable — frame_rate="50" is the
// measured shape, frame_rate=50 is a firmware that changed it, and
// width="wide" is a fault you can see rather than a width that silently reads
// as zero.
//
// Two shapes render differently, for good reason:
//
//   - an absent format or a JSON null renders as "", because there genuinely
//     is nothing. The lamps show "NO VIDEO" / "NO AUDIO" for that, which is
//     what a stopped input should read as.
//   - a format that is a bare JSON string renders as its contents, unquoted.
//     That is the shape the old documentation claimed ("h264 1920x1080 50 P");
//     if a firmware ever does send it, it should read as itself and not as a
//     quoted curiosity.
func renderFormat(raw json.RawMessage, fields map[string]json.RawMessage, order []string) string {
	if isJSONAbsent(raw) {
		return ""
	}
	if fields == nil {
		if s, ok := jsonString(raw); ok {
			return s
		}
		return compactJSON(raw)
	}

	known := make(map[string]bool, len(order))
	for _, k := range order {
		known[k] = true
	}
	rest := make([]string, 0, len(fields))
	for k := range fields {
		if !known[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)

	parts := make([]string, 0, len(fields))
	for _, k := range append(append([]string{}, order...), rest...) {
		v, present := fields[k]
		if !present {
			continue
		}
		parts = append(parts, k+"="+compactJSON(v))
	}
	return strings.Join(parts, " ")
}

// isJSONAbsent reports the two ways a value is not there: the field was
// missing, or it was an explicit JSON null.
func isJSONAbsent(raw json.RawMessage) bool {
	return len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

// compactJSON renders a raw JSON value with its insignificant whitespace
// removed, so Raw does not inherit the server's formatting.
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return buf.String()
}

// jsonString reads a JSON string. It requires the value to actually be a
// string rather than accepting anything that could be spelled as one, so that
// a codec arriving as a number is a parse failure the operator can see in Raw
// and not a plausible-looking codec name.
func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// jsonInt reads a JSON integer: width, height, sample_rate, channel_count.
//
// It insists on a JSON number, not a numeric string. encoding/json will
// happily decode "1920" into a json.Number, which would quietly paper over
// exactly the kind of type change this parser exists to notice.
func jsonInt(raw json.RawMessage) (int, bool) {
	if !isJSONNumber(raw) {
		return 0, false
	}
	i, err := json.Number(raw).Int64()
	if err != nil {
		return 0, false
	}
	if i < math.MinInt32 || i > math.MaxInt32 {
		return 0, false
	}
	return int(i), true
}

// parseFrameRate reads video format frame_rate.
//
// MEASURED as the STRING "50", beside width and height which are numbers. A
// JSON number is accepted too: that is tolerance for a firmware that makes the
// obvious correction, not a guess about one, and it is tested for explicitly.
// Anything else leaves FrameRate at zero, the VIDEO lamp red, and the offending
// value written into Raw.
func parseFrameRate(raw json.RawMessage) (float64, bool) {
	if s, ok := jsonString(raw); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	if isJSONNumber(raw) {
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// isJSONNumber reports whether a raw value is a JSON number literal, by the
// only thing that can start one.
func isJSONNumber(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	c := raw[0]
	return c == '-' || (c >= '0' && c <= '9')
}
