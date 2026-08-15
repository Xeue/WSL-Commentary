//go:build dev || production || bindings

// app_conform_test.go covers the one decision Start makes that nothing else in
// the application can make for it: WHICH video format this session's video leg
// is built to.
//
// It is a file of its own rather than more lines in app_test.go because the
// three sources it arbitrates between — the switcher's own setting, the
// operator's videoFormatOverride, and the compiled-in fallback — live in three
// different packages, and the ORDER they are consulted in is a decision that
// has to be written down somewhere it can be read on one screen.

package main

import (
	"errors"
	"testing"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
	"wslcomms/internal/m2lx"
)

// configuredFormat is what a healthy instance answers
// GET /api/v1/switcher_configuration with, as internal/m2lx has already parsed
// it: a raster, a rate, and the signal_type that is the colorimetry.
//
// Raw is filled in the shape parseVideoFormat renders, because two of the tests
// below assert on what a log line would quote and a Raw that did not look like
// the real thing would prove nothing.
func configuredFormat(width, height int, frameRate float64, signalType string) m2lx.SwitcherConfiguration {
	return m2lx.SwitcherConfiguration{
		VideoFormat: m2lx.VideoFormat{
			Raw: `width=` + itoa(width) + ` height=` + itoa(height) +
				` frame_rate="` + ftoa(frameRate) + `" bit_depth=8 color_space="YCbCr"` +
				` signal_type="` + signalType + `"`,
			Width:     width,
			Height:    height,
			FrameRate: frameRate,
		},
		SignalType: signalType,
	}
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

// ftoa renders a rate the way the wire does: "50", "29.97".
func ftoa(f float64) string {
	whole := int(f)
	if float64(whole) == f {
		return itoa(whole)
	}
	hundredths := int(f*100+0.5) % 100
	return itoa(whole) + "." + itoa(hundredths/10) + itoa(hundredths%10)
}

// withStubClient installs a client that answers SwitcherConfiguration with
// conf/err, in the place conformFormat looks for one: under ctlMu, exactly
// where startControlPlane puts the real one.
func withStubClient(a *App, conf m2lx.SwitcherConfiguration, err error) {
	a.ctlMu.Lock()
	a.client = stubClient{token: "tok", fakeConfig: conf, fakeConfigErr: err}
	a.ctlMu.Unlock()
}

func TestConformFormatPrefersTheSwitcher(t *testing.T) {
	// The whole point. A 720p50 instance says so, and the pipeline must be
	// built to that — the compiled-in 1080p50 is a live defect there and
	// nothing else in the application could ever have known.
	a, _ := newTestApp(t)
	withStubClient(a, configuredFormat(1280, 720, 50, "rec709"), nil)

	got := a.conformFormat(validConfig())
	if got == nil {
		t.Fatal("conformFormat returned nil with a switcher that answered")
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
	withStubClient(a, configuredFormat(1920, 1080, 29.97, "rec709"), nil)

	got := a.conformFormat(validConfig())
	if got == nil {
		t.Fatal("conformFormat returned nil with a switcher that answered")
	}
	if got.FrameRateNum != 30000 || got.FrameRateDen != 1001 {
		t.Fatalf("frame rate = %d/%d, want 30000/1001", got.FrameRateNum, got.FrameRateDen)
	}
}

func TestConformFormatBeatsTheOverride(t *testing.T) {
	// The precedence, stated as a test because it is the one part of this that
	// is a judgement rather than a mechanism: the switcher is the thing being
	// conformed TO and it states its own format, so a videoFormatOverride typed
	// for another venue cannot be more true than that. The disagreement is
	// logged; see conformFormat.
	a, _ := newTestApp(t)
	withStubClient(a, configuredFormat(1280, 720, 50, "rec709"), nil)

	cfg := validConfig()
	cfg.VideoFormatOverride = "1920x1080p50"

	got := a.conformFormat(cfg)
	if got == nil {
		t.Fatal("conformFormat returned nil with a switcher that answered")
	}
	if got.Width != 1280 || got.Height != 720 {
		t.Fatalf("conform target = %v, want the switcher's 1280x720 and not the override", *got)
	}
}

func TestConformFormatFallsBackToTheOverride(t *testing.T) {
	// The case that is actually common, and the reason a failure to reach the
	// endpoint must not be fatal: an operator setting a position up long before
	// kick-off, against an instance that is not answering yet. It has to start.
	a, _ := newTestApp(t)
	withStubClient(a, m2lx.SwitcherConfiguration{}, errors.New("dial tcp: i/o timeout"))

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
	// No switcher, no override. The answer is nil, which senderOpts turns into
	// a zero ConformTo, which internal/gst resolves to its own 1920x1080p50 —
	// the value that was hardcoded before any of this existed. So a facility
	// that has configured nothing gets exactly the pipeline the on-air build
	// produces today. conformFormat logs that it reached this rung; it is the
	// only place in the application that turns unknown into a format.
	a, _ := newTestApp(t)
	withStubClient(a, m2lx.SwitcherConfiguration{}, errors.New("dial tcp: i/o timeout"))

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
		name   string
		conf   m2lx.SwitcherConfiguration
		err    error
		client bool
	}{
		{name: "no control plane at all", client: false},
		{name: "not signed in yet", err: m2lx.ErrNotSignedIn, client: true},
		{name: "the instance will not answer", err: errors.New("dial tcp: i/o timeout"), client: true},
		{name: "HTTP 500 from the endpoint", err: errors.New("returned HTTP 500"), client: true},
		{name: "an empty configuration", client: true},
		{name: "a raster with no rate", client: true,
			conf: m2lx.SwitcherConfiguration{VideoFormat: m2lx.VideoFormat{
				Raw: `width=1920 height=1080`, Width: 1920, Height: 1080}}},
		{
			// THE MEASUREMENT THAT KILLED THE OLD DERIVATION, kept as a live
			// case rather than only as a comment: the switcher reported
			// frame_rate="0" for a real 1280x720p50 feed that was streaming.
			// The setting has never been seen to do this, but a zero rate is
			// not a rate wherever it comes from, and it must fall through
			// rather than build a 0 fps capsfilter.
			name: "a rate of zero, the shape a running node reported",
			conf: configuredFormat(1280, 720, 0, "rec709"), client: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newTestApp(t)
			if tc.client {
				withStubClient(a, tc.conf, tc.err)
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

func TestConformFormatTakesTheRasterWhateverTheColorimetry(t *testing.T) {
	// The colorimetry is REPORTED, never obeyed: internal/gst pins
	// colorimetry=bt709 in the video leg's capsfilter and this change does not
	// widen that package. So an instance stating something else — or something
	// unrecognised, or nothing at all — must still get its raster and rate
	// conformed to, with the disagreement in the log and nowhere else. A
	// signal_type that silently changed the raster would be far worse than one
	// that is only mentioned.
	for _, signalType := range []string{"rec709", "rec2020", "something-new", ""} {
		a, _ := newTestApp(t)
		withStubClient(a, configuredFormat(1280, 720, 50, signalType), nil)

		got := a.conformFormat(validConfig())
		if got == nil {
			t.Fatalf("signal_type %q: conformFormat returned nil", signalType)
		}
		want := gst.ConformTarget{Width: 1280, Height: 720, FrameRateNum: 50, FrameRateDen: 1}
		if *got != want {
			t.Fatalf("signal_type %q: conform target = %v, want %v", signalType, *got, want)
		}
	}
}

func TestVideoLegColorimetryMatchesWhatTheSwitcherStates(t *testing.T) {
	// The one thing that makes the hard-coded colorimetry defensible: the
	// switcher agrees with it. matchH states signal_type "rec709", which is
	// bt709 under GStreamer's name for it, which is what
	// gst.ConformTarget.spatialCaps pins. If either side ever changes, this
	// fails and the decision has to be taken again rather than discovered on
	// air.
	got, ok := configuredFormat(1920, 1080, 50, "rec709").Colorimetry()
	if !ok {
		t.Fatal(`Colorimetry() of the measured signal_type "rec709" is not recognised`)
	}
	if got != videoLegColorimetry {
		t.Fatalf("the switcher states %q colorimetry and the video leg pins %q; "+
			"the leg's capsfilter is now wrong on every instance", got, videoLegColorimetry)
	}
}

func TestConformFormatIgnoresAnUnparseableOverride(t *testing.T) {
	// config.Validate refuses such a value before Start completes, so this can
	// only be reached by a hand-edited file or a preset from a newer build. It
	// must not become a panic and must not become a silent 25 fps.
	a, _ := newTestApp(t)
	withStubClient(a, m2lx.SwitcherConfiguration{}, errors.New("dial tcp: i/o timeout"))

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
