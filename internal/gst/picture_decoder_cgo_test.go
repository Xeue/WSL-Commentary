//go:build cgo && !gststub

// picture_decoder_cgo_test.go is the Gate B check on the H.264/H.265 decoder
// pairing — the half of "parse whichever codec the transport carries" that the
// parser-only change did not cover on Windows.
//
// It runs against the REAL registry (cgo), so it is the one place that can call
// the candidate functions rather than read them as source. The source guards in
// picture_cgo_guard_test.go pin the factory NAMES at Gate A; this pins the
// RELATIONSHIP between the two lists on the platform actually building.
package gst

import (
	"runtime"
	"testing"
)

// TestPictureDecoderMatchesTheParserCodec is the H.264/H.265 join.
//
// buildLocked chooses the parser from the transport — h265parse by default,
// h264parse once a previous attempt has seen H.264 — and the decoder MUST move
// with it. On Windows d3d11h265dec decodes H.265 ONLY, so an H.264 return needs a
// DIFFERENT element, d3d11h264dec; on macOS vtdec_hw decodes both and the two
// lists are identical, which is exactly why the parser-only change was enough
// there and left Windows showing a black picture on an H.264 feed.
func TestPictureDecoderMatchesTheParserCodec(t *testing.T) {
	h265 := pictureDecoderCandidates()
	h264 := pictureDecoderCandidatesH264()
	if len(h265) == 0 || len(h264) == 0 {
		t.Skipf("no picture decoders on %s; nothing to pair", runtime.GOOS)
	}

	switch runtime.GOOS {
	case "windows":
		if h264[0].factory == h265[0].factory {
			t.Fatalf("the H.264 and H.265 decoders are the same element (%s) on Windows, but "+
				"d3d11h265dec decodes H.265 only — an H.264 return would fail to link h264parse "+
				"to it with PadLinkNoformat", h264[0].factory)
		}
		if h264[0].factory != "d3d11h264dec" {
			t.Errorf("the Windows H.264 decoder is %q, want d3d11h264dec (DXVA, from the same "+
				"d3d11 plugin as d3d11h265dec)", h264[0].factory)
		}
		if h265[0].factory != "d3d11h265dec" {
			t.Errorf("the Windows H.265 decoder is %q, want d3d11h265dec", h265[0].factory)
		}
	case "darwin":
		if h264[0].factory != h265[0].factory {
			t.Errorf("the H.264 decoder %q differs from the H.265 one %q on macOS, but vtdec_hw "+
				"decodes both — only the parser should differ there", h264[0].factory, h265[0].factory)
		}
	}
}
