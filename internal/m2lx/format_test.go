package m2lx

import (
	"encoding/json"
	"testing"
)

// liveVideoFormat and liveAudioFormat are cam22's format objects copied byte
// for byte out of testdata/switcher_status-live-2026-07-31.json. The table
// below asserts against them so that "what the switcher sends" and "what the
// parser expects" cannot drift apart silently.
const (
	liveVideoFormat = `{"bit_depth":8,"codec":"h264","color_space":"YCbCr","frame_rate":"50",` +
		`"height":1080,"sample_format":"420","scan_type":"P","width":1920}`
	liveAudioFormat = `{"bit_depth":0,"channel_count":2,"codec":"aac","sample_rate":48000}`

	liveVideoRaw = `codec="h264" width=1920 height=1080 frame_rate="50" scan_type="P" ` +
		`bit_depth=8 color_space="YCbCr" sample_format="420"`
	liveAudioRaw = `codec="aac" sample_rate=48000 channel_count=2 bit_depth=0`
)

func TestParseVideoFormat(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want VideoFormat
	}{
		{
			// The whole point of the capture. frame_rate is a STRING here
			// while width and height beside it are numbers.
			name: "measured good state, live frame verbatim",
			raw:  liveVideoFormat,
			want: VideoFormat{Raw: liveVideoRaw, Codec: "h264", Width: 1920, Height: 1080, FrameRate: 50},
		},
		{
			// Measured on all 22 stopped nodes: null, not absent, not {}.
			name: "stopped node sends null",
			raw:  `null`,
			want: VideoFormat{},
		},
		{
			name: "field absent entirely",
			raw:  ``,
			want: VideoFormat{},
		},
		{
			name: "empty object",
			raw:  `{}`,
			want: VideoFormat{},
		},
		{
			name: "fractional frame rate as a string",
			raw:  `{"codec":"h264","width":1280,"height":720,"frame_rate":"29.97","scan_type":"I"}`,
			want: VideoFormat{
				Raw:   `codec="h264" width=1280 height=720 frame_rate="29.97" scan_type="I"`,
				Codec: "h264", Width: 1280, Height: 720, FrameRate: 29.97,
			},
		},
		{
			// Tolerated deliberately, and tested so the tolerance is earned
			// rather than accidental: a firmware that corrects frame_rate to a
			// number must not blank the VIDEO lamp.
			name: "frame rate as a JSON number is tolerated",
			raw:  `{"codec":"h264","width":1920,"height":1080,"frame_rate":50}`,
			want: VideoFormat{
				Raw:   `codec="h264" width=1920 height=1080 frame_rate=50`,
				Codec: "h264", Width: 1920, Height: 1080, FrameRate: 50,
			},
		},
		{
			// The synthetic case the live frame cannot produce: every field
			// the wrong type. Nothing parses, and Raw shows why — with the
			// quoting intact, so "1920 arrived as a string" is legible.
			name: "unexpected types are zeroed and visible in Raw",
			raw:  `{"codec":123,"width":"wide","height":null,"frame_rate":true}`,
			want: VideoFormat{Raw: `codec=123 width="wide" height=null frame_rate=true`},
		},
		{
			name: "numeric strings are NOT accepted for numeric fields",
			raw:  `{"codec":"h264","width":"1920","height":"1080","frame_rate":"50"}`,
			want: VideoFormat{
				Raw:   `codec="h264" width="1920" height="1080" frame_rate="50"`,
				Codec: "h264", FrameRate: 50,
			},
		},
		{
			name: "non-integer dimensions are rejected rather than truncated",
			raw:  `{"codec":"h264","width":1920.5,"height":1080}`,
			want: VideoFormat{Raw: `codec="h264" width=1920.5 height=1080`, Codec: "h264", Height: 1080},
		},
		{
			name: "unparseable frame rate string",
			raw:  `{"codec":"h264","width":1920,"height":1080,"frame_rate":"50/1"}`,
			want: VideoFormat{
				Raw:   `codec="h264" width=1920 height=1080 frame_rate="50/1"`,
				Codec: "h264", Width: 1920, Height: 1080,
			},
		},
		{
			// A field nobody has seen must appear in Raw, not vanish. Known
			// keys keep their order; unknown ones follow, sorted.
			name: "unknown fields are rendered after the known ones, sorted",
			raw:  `{"zulu":1,"codec":"h264","alpha":"a","width":1920}`,
			want: VideoFormat{Raw: `codec="h264" width=1920 alpha="a" zulu=1`, Codec: "h264", Width: 1920},
		},
		{
			// The shape docs/architecture.md line 406 claimed. If a firmware
			// ever really sends it, it must read as itself on the lamp.
			name: "a bare string format renders unquoted",
			raw:  `"h264 1920x1080 50 P"`,
			want: VideoFormat{Raw: "h264 1920x1080 50 P"},
		},
		{
			name: "a scalar format is visible rather than silent",
			raw:  `42`,
			want: VideoFormat{Raw: "42"},
		},
		{
			name: "an array format is visible rather than silent",
			raw:  `["h264"]`,
			want: VideoFormat{Raw: `["h264"]`},
		},
		{
			name: "server whitespace is not inherited",
			raw:  "{\n  \"codec\" : \"h264\" ,\n  \"width\" : 1920\n}",
			want: VideoFormat{Raw: `codec="h264" width=1920`, Codec: "h264", Width: 1920},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseVideoFormat(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Fatalf("parseVideoFormat(%s):\n got %+v\nwant %+v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseAudioFormat(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want AudioFormat
	}{
		{
			name: "measured good state, live frame verbatim",
			raw:  liveAudioFormat,
			want: AudioFormat{Raw: liveAudioRaw, Codec: "aac", SampleRate: 48000, Channels: 2},
		},
		{
			name: "stopped node sends null",
			raw:  `null`,
			want: AudioFormat{},
		},
		{
			name: "field absent entirely",
			raw:  ``,
			want: AudioFormat{},
		},
		{
			name: "mono",
			raw:  `{"codec":"aac","sample_rate":48000,"channel_count":1,"bit_depth":0}`,
			want: AudioFormat{
				Raw:   `codec="aac" sample_rate=48000 channel_count=1 bit_depth=0`,
				Codec: "aac", SampleRate: 48000, Channels: 1,
			},
		},
		{
			// bit_depth 0 is the healthy AAC value. It must stay in Raw and
			// out of any field a lamp reads.
			name: "a codec this app does not want still parses fully",
			raw:  `{"codec":"mp2","sample_rate":48000,"channel_count":2,"bit_depth":16}`,
			want: AudioFormat{
				Raw:   `codec="mp2" sample_rate=48000 channel_count=2 bit_depth=16`,
				Codec: "mp2", SampleRate: 48000, Channels: 2,
			},
		},
		{
			name: "unexpected types are zeroed and visible in Raw",
			raw:  `{"codec":[],"sample_rate":"48kHz","channel_count":"stereo"}`,
			want: AudioFormat{Raw: `codec=[] sample_rate="48kHz" channel_count="stereo"`},
		},
		{
			name: "unknown fields are rendered after the known ones, sorted",
			raw:  `{"language":"eng","codec":"aac","bit_rate":128000}`,
			want: AudioFormat{Raw: `codec="aac" bit_rate=128000 language="eng"`, Codec: "aac"},
		},
		{
			name: "a bare string format renders unquoted",
			raw:  `"aac 48000 2ch"`,
			want: AudioFormat{Raw: "aac 48000 2ch"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAudioFormat(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Fatalf("parseAudioFormat(%s):\n got %+v\nwant %+v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestJSONInt(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   int
		wantOK bool
	}{
		{"integer", `1920`, 1920, true},
		{"zero", `0`, 0, true},
		{"negative", `-1`, -1, true},
		{"numeric string is not a number", `"1920"`, 0, false},
		{"float", `1920.5`, 0, false},
		{"null", `null`, 0, false},
		{"absent", ``, 0, false},
		{"bool", `true`, 0, false},
		{"object", `{}`, 0, false},
		// Anything that would not survive the trip through int on a 32-bit
		// build is refused rather than silently wrapping into a plausible
		// width.
		{"beyond int32", `4294967296`, 0, false},
		{"exponent form is not an integer literal", `1.92e3`, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := jsonInt(json.RawMessage(tc.in))
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("jsonInt(%s) = (%d,%v), want (%d,%v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestParseFrameRate(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   float64
		wantOK bool
	}{
		{"the measured shape, a string", `"50"`, 50, true},
		{"fractional string", `"29.97"`, 29.97, true},
		{"a number is tolerated", `50`, 50, true},
		{"fractional number", `59.94`, 59.94, true},
		{"rational notation is not understood", `"50/1"`, 0, false},
		{"words", `"fifty"`, 0, false},
		{"empty string", `""`, 0, false},
		{"null", `null`, 0, false},
		{"absent", ``, 0, false},
		{"bool", `true`, 0, false},
		// strconv.ParseFloat accepts these; a frame rate must not be either.
		{"infinity", `"Inf"`, 0, false},
		{"not a number", `"NaN"`, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseFrameRate(json.RawMessage(tc.in))
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("parseFrameRate(%s) = (%v,%v), want (%v,%v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestJSONString(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"string", `"h264"`, "h264", true},
		{"empty string", `""`, "", true},
		{"escaped", `"a\"b"`, `a"b`, true},
		{"number", `123`, "", false},
		{"null", `null`, "", false},
		{"absent", ``, "", false},
		{"array", `["h264"]`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := jsonString(json.RawMessage(tc.in))
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("jsonString(%s) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
