//go:build dev || production || bindings

// app_conform_test.go covers the one decision Start makes that nothing else in
// the application can make for it: WHICH video format this session's video leg
// is built to.
//
// It is a file of its own rather than more lines in app_test.go because the
// three sources it arbitrates between — the switcher, the operator's
// videoFormatOverride, and the compiled-in fallback — live in three different
// packages, and the ORDER they are consulted in is a decision that has to be
// written down somewhere it can be read on one screen.

package main

import (
	"errors"
	"testing"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
	"wslcomms/internal/m2lx"
)

// conformSnapshot builds an opening-snapshot frame with one streaming node at
// the given raster, in the measured wire shape — frame_rate a STRING beside
// numeric width and height.
func conformSnapshot(node string, width, height int, frameRate string) []byte {
	return []byte(`{"status":[{"node":"` + node + `","path":"/","state":{` +
		`"display_name":"COMMS","stream_state":"streaming","streams":{"audio":[],` +
		`"video":{"format":{"codec":"h264","width":` + itoa(width) +
		`,"height":` + itoa(height) + `,"frame_rate":"` + frameRate + `","scan_type":"P"}}}}}]}`)
}

// stoppedSnapshot is what the live matchH instance actually returned on
// 2026-08-15: every router input stopped, every video format JSON null. It is
// the NORMAL state of a facility before kick-off and therefore the case the
// override exists for.
func stoppedSnapshot() []byte {
	return []byte(`{"status":[{"node":"cam4","path":"/","state":{` +
		`"display_name":"COMMS","stream_state":"stopped","streams":{"audio":[],` +
		`"video":{"format":null}}}}]}`)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// withStubWatcher installs a watcher whose RawSnapshot returns raw, in the
// place conformFormat looks for one: under ctlMu, exactly where
// startControlPlane puts the real one.
func withStubWatcher(a *App, raw []byte, err error) *stubWatcher {
	w := newStubWatcher()
	w.setRaw(raw, err)
	a.ctlMu.Lock()
	a.watcher = w
	a.ctlMu.Unlock()
	return w
}

func TestConformFormatPrefersTheSwitcher(t *testing.T) {
	// The whole point of the derivation. A 720p50 instance with one camera up
	// reports 720p50, and the pipeline must be built to that — the compiled-in
	// 1080p50 is a live defect there and nothing else in the application could
	// ever have known.
	a, _ := newTestApp(t)
	withStubWatcher(a, conformSnapshot("cam1", 1280, 720, "50"), nil)

	got := a.conformFormat(validConfig())
	if got == nil {
		t.Fatal("conformFormat returned nil with a streaming node on the switcher")
	}
	want := gst.ConformTarget{Width: 1280, Height: 720, FrameRateNum: 50, FrameRateDen: 1}
	if *got != want {
		t.Fatalf("conform target = %v, want %v", *got, want)
	}
}

func TestConformFormatConvertsAnNTSCRate(t *testing.T) {
	// 29.97 is a rounding of 30000/1001, not a rate. A capsfilter built from
	// 2997/100 negotiates, plays, and drifts against the switcher for the whole
	// of a match — which is why the conversion lives in internal/gst and is
	// exercised from here rather than assumed.
	a, _ := newTestApp(t)
	withStubWatcher(a, conformSnapshot("cam1", 1920, 1080, "29.97"), nil)

	got := a.conformFormat(validConfig())
	if got == nil {
		t.Fatal("conformFormat returned nil with a streaming node on the switcher")
	}
	if got.FrameRateNum != 30000 || got.FrameRateDen != 1001 {
		t.Fatalf("frame rate = %d/%d, want 30000/1001", got.FrameRateNum, got.FrameRateDen)
	}
}

func TestConformFormatBeatsTheOverride(t *testing.T) {
	// The precedence, stated as a test because it is the one part of this that
	// is a judgement rather than a mechanism: the switcher is the thing being
	// conformed TO, and a node that is streaming is a MEASUREMENT of what it is
	// accepting. A videoFormatOverride typed for another venue cannot be more
	// true than that. The disagreement is logged; see conformFormat.
	a, _ := newTestApp(t)
	withStubWatcher(a, conformSnapshot("cam1", 1280, 720, "50"), nil)

	cfg := validConfig()
	cfg.VideoFormatOverride = "1920x1080p50"

	got := a.conformFormat(cfg)
	if got == nil {
		t.Fatal("conformFormat returned nil with a streaming node on the switcher")
	}
	if got.Width != 1280 || got.Height != 720 {
		t.Fatalf("conform target = %v, want the switcher's 1280x720 and not the override", *got)
	}
}

func TestConformFormatFallsBackToTheOverride(t *testing.T) {
	// MEASURED on the live matchH instance, 2026-08-15: all 24 router inputs
	// stopped, all 24 video formats null. That is the normal state of a
	// facility before kick-off, so this is the path a real position takes far
	// more often than the one above.
	a, _ := newTestApp(t)
	withStubWatcher(a, stoppedSnapshot(), nil)

	cfg := validConfig()
	cfg.VideoFormatOverride = "1280x720p59.94"

	got := a.conformFormat(cfg)
	if got == nil {
		t.Fatal("conformFormat returned nil with an override set")
	}
	want := gst.ConformTarget{Width: 1280, Height: 720, FrameRateNum: 60000, FrameRateDen: 1001}
	if *got != want {
		t.Fatalf("conform target = %v, want the override %v", *got, want)
	}
}

func TestConformFormatFallsBackToNothing(t *testing.T) {
	// Nothing streaming, no override. The answer is nil, which senderOpts turns
	// into a zero ConformTo, which internal/gst resolves to its own
	// 1920x1080p50 — the value that was hardcoded before any of this existed.
	// So a facility that has configured nothing gets exactly the pipeline the
	// on-air build produces today.
	a, _ := newTestApp(t)
	withStubWatcher(a, stoppedSnapshot(), nil)

	if got := a.conformFormat(validConfig()); got != nil {
		t.Fatalf("conform target = %v, want nil so internal/gst applies its own fallback", *got)
	}
}

func TestConformFormatSurvivesEverythingTheSwitcherCanDo(t *testing.T) {
	// Nothing in this path may be a reason a match does not go out — the same
	// rule that keeps statusKey out of config.Validate. Each of these is a
	// genuine live condition and each must fall through to the override rather
	// than fail, panic or block.
	cases := []struct {
		name    string
		raw     []byte
		rawErr  error
		watcher bool
	}{
		{name: "no control plane at all", watcher: false},
		{name: "not signed in yet", rawErr: m2lx.ErrNotSignedIn, watcher: true},
		{name: "the socket would not open", rawErr: errors.New("dial tcp: i/o timeout"), watcher: true},
		{name: "a frame that is not switcher_status", raw: []byte(`<html>502</html>`), watcher: true},
		{name: "an empty frame", raw: []byte(`{"status":[]}`), watcher: true},
		{name: "a raster with no rate", watcher: true, raw: []byte(
			`{"status":[{"node":"cam1","path":"/","state":{"stream_state":"streaming",` +
				`"streams":{"audio":[],"video":{"format":{"width":1920,"height":1080}}}}}]}`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newTestApp(t)
			if tc.watcher {
				withStubWatcher(a, tc.raw, tc.rawErr)
			}

			cfg := validConfig()
			cfg.VideoFormatOverride = "1920x1080p25"

			got := a.conformFormat(cfg)
			if got == nil {
				t.Fatal("conformFormat returned nil; the override should have answered")
			}
			if got.FrameRateNum != 25 || got.FrameRateDen != 1 {
				t.Fatalf("conform target = %v, want the override's 25/1", *got)
			}
		})
	}
}

func TestConformFormatIgnoresAnUnparseableOverride(t *testing.T) {
	// config.Validate refuses such a value before Start completes, so this can
	// only be reached by a hand-edited file or a preset from a newer build. It
	// must not become a panic and must not become a silent 25 fps.
	a, _ := newTestApp(t)
	withStubWatcher(a, stoppedSnapshot(), nil)

	cfg := validConfig()
	cfg.VideoFormatOverride = "the usual one"

	if got := a.conformFormat(cfg); got != nil {
		t.Fatalf("conform target = %v, want nil: an override that will not parse is not a format", *got)
	}
}

func TestSenderOptsCarriesTheConformTarget(t *testing.T) {
	// The seam between the decision and the pipeline. senderOpts reads the
	// atomic Start wrote, because its own signature has callers in the test
	// suite this change is not entitled to move.
	a, _ := newTestApp(t)
	silencePump(a)

	// Nothing stored: the zero ConformTo, which internal/gst documents as
	// "nothing is known". This is what every other senderOpts test sees, and it
	// is what keeps them asserting the pipeline the shipped build produces.
	if got := a.senderOpts(validConfig(), "").Pipeline.ConformTo; got != (gst.ConformTarget{}) {
		t.Fatalf("ConformTo = %v with nothing derived, want the zero value", got)
	}

	want := gst.ConformTarget{Width: 1280, Height: 720, FrameRateNum: 50, FrameRateDen: 1}
	a.conformTo.Store(&want)
	if got := a.senderOpts(validConfig(), "").Pipeline.ConformTo; got != want {
		t.Fatalf("ConformTo = %v, want %v", got, want)
	}
}

func TestSenderOptsCarriesTheConfiguredVideoBitrate(t *testing.T) {
	// The owner has ruled 2000 kbps — chosen for a still slate — far too low
	// for a leg carrying live video. The bitrate is now a setting, and this is
	// the one place its value reaches the encoder.
	a, _ := newTestApp(t)
	silencePump(a)

	cfg := validConfig()
	cfg.VideoBitrateKbps = 10000
	if got := a.senderOpts(cfg, "").Pipeline.VideoBitrateKbps; got != 10000 {
		t.Fatalf("VideoBitrateKbps = %d, want the configured 10000", got)
	}

	// An unset field still encodes at exactly what the on-air build encodes.
	cfg.VideoBitrateKbps = 0
	if got := a.senderOpts(cfg, "").Pipeline.VideoBitrateKbps; got != config.DefaultVideoBitrateKbps {
		t.Fatalf("VideoBitrateKbps = %d, want the default %d", got, config.DefaultVideoBitrateKbps)
	}
}
