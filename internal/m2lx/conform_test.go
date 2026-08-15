package m2lx

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// The frame, entry, streamState and readFixture helpers are wire_test.go's and
// are reused deliberately: these tests and that file's are about the SAME wire,
// and a conform test that built its own idea of an entry could pass against a
// shape the rest of the package no longer accepts.

// streamingNode is one running router input reporting a format object.
func streamingNode(name, format string) string {
	return entry(name, streamState(name, StreamStateStreaming, format, liveAudioFormat))
}

// stoppedNode is the measured shape of every input that is not running: a
// stream_state of "stopped" and a video format of JSON null. Twenty-two of the
// twenty-four inputs in the live capture look exactly like this.
func stoppedNode(name string) string {
	return entry(name, streamState(name, StreamStateStopped, "null"))
}

// rasterFormat renders a format object in the measured field order, with
// frame_rate as a STRING and width/height as NUMBERS — the trap format.go
// documents, restated here so a test that stopped exercising it would look
// wrong on the page.
func rasterFormat(width, height int, frameRate string) string {
	return `{"bit_depth":8,"codec":"h264","color_space":"YCbCr","frame_rate":"` + frameRate +
		`","height":` + strconv.Itoa(height) + `,"sample_format":"420","scan_type":"P","width":` +
		strconv.Itoa(width) + `}`
}

func TestConformFormatFrom_LiveCapture(t *testing.T) {
	// The 2026-07-31 capture, verbatim: 36 entries, one of them (cam22)
	// streaming this application's own feed at 1080p50 and the other 22 inputs
	// stopped with a null format. It is the case the derivation exists for —
	// one running node is enough — and it is asserted against the real file so
	// that a change to the wire shape breaks this rather than being absorbed.
	payload, err := os.ReadFile(liveSnapshot)
	if err != nil {
		t.Fatalf("reading the live capture: %v", err)
	}

	got, ok := ConformFormatFrom(payload)
	if !ok {
		t.Fatal("ConformFormatFrom(live capture) = not ok; the capture has a streaming node with a format")
	}
	if got.Width != 1920 || got.Height != 1080 || got.FrameRate != 50 {
		t.Errorf("got %dx%d@%v, want 1920x1080@50", got.Width, got.Height, got.FrameRate)
	}
	if got.Codec != "h264" {
		t.Errorf("Codec = %q, want %q", got.Codec, "h264")
	}
	if got.Raw != liveVideoRaw {
		t.Errorf("Raw = %q, want the format rendering %q", got.Raw, liveVideoRaw)
	}
	if len(got.Disagreeing) != 0 {
		t.Errorf("Disagreeing = %v, want none: only one node in the capture is streaming", got.Disagreeing)
	}
	if got.String() != "1920x1080@50" {
		t.Errorf("String() = %q, want %q", got.String(), "1920x1080@50")
	}
}

func TestConformFormatFrom(t *testing.T) {
	cases := []struct {
		name        string
		payload     []byte
		wantOK      bool
		wantW       int
		wantH       int
		wantRate    float64
		wantNode    string
		wantAgree   int
		wantDisagre []string
	}{
		{
			// The normal case on an instance nobody is feeding: every node
			// stopped, every format null. Not an error — the caller falls back
			// to its configured default and says so once.
			name:    "no node running",
			payload: frame(stoppedNode("cam1"), stoppedNode("cam2"), stoppedNode("cam3")),
			wantOK:  false,
		},
		{
			// One camera up on a 720p50 instance. This is the whole point: the
			// compiled-in 1080p50 would be wrong here and nothing else in the
			// application could ever have known.
			name:      "one running node on a 720p instance",
			payload:   frame(stoppedNode("cam1"), streamingNode("cam7", rasterFormat(1280, 720, "50")), stoppedNode("cam9")),
			wantOK:    true,
			wantW:     1280,
			wantH:     720,
			wantRate:  50,
			wantNode:  "cam7",
			wantAgree: 1,
		},
		{
			// Every source matches, which is the premise. The node named is
			// the first BY NAME, not the first in the array, so the answer does
			// not move about between reads of an unchanged switcher.
			name: "several running nodes agreeing",
			payload: frame(
				streamingNode("cam9", rasterFormat(1920, 1080, "50")),
				streamingNode("cam2", rasterFormat(1920, 1080, "50")),
				streamingNode("cam4", rasterFormat(1920, 1080, "50")),
			),
			wantOK:    true,
			wantW:     1920,
			wantH:     1080,
			wantRate:  50,
			wantNode:  "cam2",
			wantAgree: 3,
		},
		{
			// A fractional rate, arriving as a string exactly as 50 does.
			// 29.97 is the value that would print as 29.969999999999999 under
			// a naive %g, which is why String has its own assertion below.
			name:      "fractional frame rate",
			payload:   frame(streamingNode("cam1", rasterFormat(1920, 1080, "29.97"))),
			wantOK:    true,
			wantW:     1920,
			wantH:     1080,
			wantRate:  29.97,
			wantNode:  "cam1",
			wantAgree: 1,
		},
		{
			// Tolerance for a firmware that makes the obvious correction and
			// sends frame_rate as a number. format.go accepts it explicitly;
			// this is that acceptance seen from the conform end.
			name: "frame_rate as a JSON number",
			payload: frame(`{"node":"cam1","path":"/","state":{"stream_state":"streaming",` +
				`"streams":{"video":{"format":{"codec":"h264","width":1920,"height":1080,"frame_rate":50}},"audio":[]}}}`),
			wantOK:    true,
			wantW:     1920,
			wantH:     1080,
			wantRate:  50,
			wantNode:  "cam1",
			wantAgree: 1,
		},
		{
			// The majority wins and the odd one out is NAMED. Falling back to
			// the compiled-in default on the evidence of one misconfigured node
			// outvoting two correct ones would be the defect this function
			// exists to remove, arrived at from the other direction.
			name: "one node disagreeing",
			payload: frame(
				streamingNode("cam1", rasterFormat(1920, 1080, "50")),
				streamingNode("cam2", rasterFormat(1920, 1080, "50")),
				streamingNode("cam8", rasterFormat(1280, 720, "50")),
			),
			wantOK:      true,
			wantW:       1920,
			wantH:       1080,
			wantRate:    50,
			wantNode:    "cam1",
			wantAgree:   2,
			wantDisagre: []string{"cam8"},
		},
		{
			// A dead heat. Somebody has to win or the pipeline has no raster;
			// the node name breaks it so that the same frame always produces
			// the same answer, whatever Go's map iteration order does today.
			name: "two nodes disagreeing, tie broken by node name",
			payload: frame(
				streamingNode("cam9", rasterFormat(1280, 720, "50")),
				streamingNode("cam3", rasterFormat(1920, 1080, "50")),
			),
			wantOK:      true,
			wantW:       1920,
			wantH:       1080,
			wantRate:    50,
			wantNode:    "cam3",
			wantAgree:   1,
			wantDisagre: []string{"cam9"},
		},
		{
			// "starting" is excluded deliberately: a node that is negotiating
			// can report a half-negotiated raster, and conforming the encoder
			// to a transient is worse than conforming it to the default.
			name: "a starting node does not count",
			payload: frame(`{"node":"cam1","path":"/","state":{"stream_state":"starting",` +
				`"streams":{"video":{"format":` + rasterFormat(1280, 720, "50") + `},"audio":[]}}}`),
			wantOK: false,
		},
		{
			// A format that arrived but did not parse. 1920x0 is not a raster,
			// and format.go's contract is that a zero field must never be read
			// as a good value — this is that contract enforced at the one place
			// that would otherwise build an encoder out of it.
			name:    "a partial format does not count",
			payload: frame(streamingNode("cam1", `{"codec":"h264","width":1920,"frame_rate":"50"}`)),
			wantOK:  false,
		},
		{
			// A node calling itself streaming with a null format. Measured as
			// the stopped shape, but nothing on the wire forbids it here, and
			// the answer is the same: there is no raster to conform to.
			name:    "streaming with a null format",
			payload: frame(streamingNode("cam1", `null`)),
			wantOK:  false,
		},
		{
			// Not a switcher_status frame. The caller does not need to tell
			// this from "nothing is running" — the action is identical.
			name:    "not a frame at all",
			payload: []byte(`{"hello":"world"}`),
			wantOK:  false,
		},
		{
			name:    "not JSON at all",
			payload: []byte(`<html>502 Bad Gateway</html>`),
			wantOK:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ConformFormatFrom(tc.payload)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tc.wantOK, got)
			}
			if !ok {
				return
			}
			if got.Width != tc.wantW || got.Height != tc.wantH || got.FrameRate != tc.wantRate {
				t.Errorf("got %dx%d@%v, want %dx%d@%v",
					got.Width, got.Height, got.FrameRate, tc.wantW, tc.wantH, tc.wantRate)
			}
			if got.Node != tc.wantNode {
				t.Errorf("Node = %q, want %q", got.Node, tc.wantNode)
			}
			if got.Agreeing != tc.wantAgree {
				t.Errorf("Agreeing = %d, want %d", got.Agreeing, tc.wantAgree)
			}
			if strings.Join(got.Disagreeing, ",") != strings.Join(tc.wantDisagre, ",") {
				t.Errorf("Disagreeing = %v, want %v", got.Disagreeing, tc.wantDisagre)
			}
		})
	}
}

// TestConformFormatScanType pins the deliberately narrow interlace test.
//
// It fails PROGRESSIVE on everything that is not plainly interlaced, and the
// asymmetry is the point: the video leg can only produce progressive, so
// reading an interlaced switcher as progressive changes nothing this
// application does, where reading a progressive one as interlaced would put a
// permanent and false "we cannot match your switcher" line in the log.
func TestConformFormatScanType(t *testing.T) {
	cases := []struct {
		scan string
		want bool
	}{
		{`"P"`, false},         // the measured healthy value
		{`"I"`, true},          // the one this is for
		{`"i"`, true},          // a firmware that lower-cases it
		{`"interlaced"`, true}, // a firmware that spells it out
		{`"progressive"`, false},
		{`""`, false},   // present and empty
		{`50`, false},   // present and not a string
		{`null`, false}, // present and null
	}
	for _, tc := range cases {
		format := `{"codec":"h264","width":1920,"height":1080,"frame_rate":"50","scan_type":` + tc.scan + `}`
		got, ok := ConformFormatFrom(frame(streamingNode("cam1", format)))
		if !ok {
			t.Fatalf("scan_type %s: not ok; the raster and rate are both there", tc.scan)
		}
		if got.Interlaced != tc.want {
			t.Errorf("scan_type %s: Interlaced = %v, want %v", tc.scan, got.Interlaced, tc.want)
		}
	}

	// An absent scan_type is not interlaced either, and it is worth its own
	// case because it is the shape a format object this package has never met
	// would arrive in.
	got, ok := ConformFormatFrom(frame(streamingNode("cam1", `{"codec":"h264","width":1920,"height":1080,"frame_rate":"50"}`)))
	if !ok || got.Interlaced {
		t.Errorf("absent scan_type: ok=%v Interlaced=%v, want ok=true Interlaced=false", ok, got.Interlaced)
	}
}

// TestConformFormatFromIgnoresDeltas is the trap from wire.go, applied to this
// function. A "/statistics" delta carries a node name and a state object and no
// stream_state at all; read as a whole node it says "cam1 is not a router
// input", and an earlier parser drew exactly that conclusion once a second
// about the only input on the switcher that was working.
//
// Here the consequence would be quieter and worse: a delta-only read reports no
// conform format, the pipeline falls back to 1080p50, and on a 720p instance
// the feed is silently the wrong raster with nothing anywhere saying why.
func TestConformFormatFromIgnoresDeltas(t *testing.T) {
	delta, err := os.ReadFile(liveDeltaStatistics)
	if err != nil {
		t.Fatalf("reading the delta capture: %v", err)
	}
	if got, ok := ConformFormatFrom(delta); ok {
		t.Fatalf("ConformFormatFrom(a /statistics delta) = %+v, ok; a delta carries no whole node", got)
	}

	// And the same node's WHOLE state in the same connection does answer.
	whole := frame(streamingNode("cam1", rasterFormat(1920, 1080, "50")))
	if _, ok := ConformFormatFrom(whole); !ok {
		t.Fatal("ConformFormatFrom(a whole-node frame) = not ok")
	}
}

// TestConformFormatString pins the rendering, because it is what goes in the
// log line the operator is asked to read back when a feed will not lock.
func TestConformFormatString(t *testing.T) {
	cases := []struct {
		f    ConformFormat
		want string
	}{
		{ConformFormat{VideoFormat: VideoFormat{Width: 1920, Height: 1080, FrameRate: 50}}, "1920x1080@50"},
		{ConformFormat{VideoFormat: VideoFormat{Width: 1280, Height: 720, FrameRate: 59.94}}, "1280x720@59.94"},
		{ConformFormat{VideoFormat: VideoFormat{Width: 1920, Height: 1080, FrameRate: 29.97}}, "1920x1080@29.97"},
		{ConformFormat{VideoFormat: VideoFormat{Width: 720, Height: 576, FrameRate: 25}}, "720x576@25"},
	}
	for _, tc := range cases {
		if got := tc.f.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}
