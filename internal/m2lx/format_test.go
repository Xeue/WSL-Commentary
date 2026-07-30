package m2lx

import "testing"

func TestParseVideoFormat(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want VideoFormat
	}{
		{
			name: "measured good state",
			raw:  "h264 1920x1080 50 P",
			want: VideoFormat{Raw: "h264 1920x1080 50 P", Codec: "h264", Width: 1920, Height: 1080, FrameRate: 50},
		},
		{
			name: "fractional frame rate",
			raw:  "h264 1280x720 29.97 I",
			want: VideoFormat{Raw: "h264 1280x720 29.97 I", Codec: "h264", Width: 1280, Height: 720, FrameRate: 29.97},
		},
		{
			name: "no scan indicator",
			raw:  "h264 1920x1080 50",
			want: VideoFormat{Raw: "h264 1920x1080 50", Codec: "h264", Width: 1920, Height: 1080, FrameRate: 50},
		},
		{
			name: "codec only",
			raw:  "h264",
			want: VideoFormat{Raw: "h264", Codec: "h264"},
		},
		{
			name: "empty string",
			raw:  "",
			want: VideoFormat{Raw: ""},
		},
		{
			name: "whitespace only",
			raw:  "   ",
			want: VideoFormat{Raw: "   "},
		},
		{
			name: "malformed dimensions - no x",
			raw:  "h264 19201080 50 P",
			want: VideoFormat{Raw: "h264 19201080 50 P", Codec: "h264", FrameRate: 50},
		},
		{
			name: "malformed dimensions - non numeric",
			raw:  "h264 fullhd 50 P",
			want: VideoFormat{Raw: "h264 fullhd 50 P", Codec: "h264", FrameRate: 50},
		},
		{
			name: "non numeric frame rate",
			raw:  "h264 1920x1080 fast P",
			want: VideoFormat{Raw: "h264 1920x1080 fast P", Codec: "h264", Width: 1920, Height: 1080},
		},
		{
			name: "completely unrecognised text still preserves Raw",
			raw:  "some garbage the parser has never seen",
			want: VideoFormat{Raw: "some garbage the parser has never seen", Codec: "some"},
		},
		{
			name: "uppercase X separator",
			raw:  "h264 1920X1080 50 P",
			want: VideoFormat{Raw: "h264 1920X1080 50 P", Codec: "h264", Width: 1920, Height: 1080, FrameRate: 50},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseVideoFormat(tc.raw)
			if got != tc.want {
				t.Fatalf("parseVideoFormat(%q) = %+v, want %+v", tc.raw, got, tc.want)
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
			name: "measured good state",
			raw:  "aac 48000 2ch",
			want: AudioFormat{Raw: "aac 48000 2ch", Codec: "aac", SampleRate: 48000, Channels: 2},
		},
		{
			name: "mono",
			raw:  "aac 48000 1ch",
			want: AudioFormat{Raw: "aac 48000 1ch", Codec: "aac", SampleRate: 48000, Channels: 1},
		},
		{
			name: "uppercase channel suffix",
			raw:  "aac 48000 2CH",
			want: AudioFormat{Raw: "aac 48000 2CH", Codec: "aac", SampleRate: 48000, Channels: 2},
		},
		{
			name: "codec only",
			raw:  "aac",
			want: AudioFormat{Raw: "aac", Codec: "aac"},
		},
		{
			name: "empty string",
			raw:  "",
			want: AudioFormat{Raw: ""},
		},
		{
			name: "missing ch suffix",
			raw:  "aac 48000 2",
			want: AudioFormat{Raw: "aac 48000 2", Codec: "aac", SampleRate: 48000, Channels: 2},
		},
		{
			name: "non numeric sample rate - later fields still parse independently",
			raw:  "aac fast 2ch",
			want: AudioFormat{Raw: "aac fast 2ch", Codec: "aac", Channels: 2},
		},
		{
			name: "non numeric channel count",
			raw:  "aac 48000 manych",
			want: AudioFormat{Raw: "aac 48000 manych", Codec: "aac", SampleRate: 48000},
		},
		{
			name: "unrecognised text still preserves Raw",
			raw:  "totally unknown shape",
			want: AudioFormat{Raw: "totally unknown shape", Codec: "totally"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAudioFormat(tc.raw)
			if got != tc.want {
				t.Fatalf("parseAudioFormat(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseDimensions(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		wantW  int
		wantH  int
		wantOK bool
	}{
		{"good", "1920x1080", 1920, 1080, true},
		{"uppercase", "1920X1080", 1920, 1080, true},
		{"no separator", "19201080", 0, 0, false},
		{"non numeric width", "wxh", 0, 0, false},
		{"empty", "", 0, 0, false},
		{"extra separators", "1920x1080x50", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, h, ok := parseDimensions(tc.in)
			if w != tc.wantW || h != tc.wantH || ok != tc.wantOK {
				t.Fatalf("parseDimensions(%q) = (%d,%d,%v), want (%d,%d,%v)", tc.in, w, h, ok, tc.wantW, tc.wantH, tc.wantOK)
			}
		})
	}
}

func TestParseChannelCount(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   int
		wantOK bool
	}{
		{"good", "2ch", 2, true},
		{"uppercase", "8CH", 8, true},
		{"bare number", "2", 2, true},
		{"non numeric", "stereo", 0, false},
		{"empty", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseChannelCount(tc.in)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("parseChannelCount(%q) = (%d,%v), want (%d,%v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
