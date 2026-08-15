package m2lx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// The switcher's configured format
// --------------------------------
//
// M2L-X can be configured into ANY video format, and EVERY source feeding it
// must be produced in that format. A contribution pipeline that hard-codes
// 1920x1080p50 is therefore a live defect on any instance not configured for
// it, and this file is the whole of how the application avoids that: it ASKS.
//
// The format is a SETTING on the instance, not a property of anything running
// on it. GET /api/v1/switcher_configuration, bearer-authenticated exactly like
// the KVS and events calls — no tenant header, no query string, no body —
// returns it as the FIRST key of its response. MEASURED against the live matchH
// instance on 2026-08-15, 12108 bytes beginning:
//
//	{"format":{"video":{"bit_depth":8,"color_space":"YCbCr","frame_rate":"50",
//	                    "height":1080,"signal_type":"rec709","width":1920}},
//	 "nodes":[...], "system_info":{...}}
//
// Top-level keys are exactly three: format, nodes, system_info. The endpoint was
// found by reading the switcher's own Angular bundle (main-*.js) — the same
// technique events.go records for finding /api/events/overview — and the path is
// the same family as the two this application already speaks:
// internal/mixer/controller.go's /api/v1/switcher_controller and the
// /api/v1/switcher_status WebSocket.
//
// TWO PROPERTIES OF THE WIRE ARE LOAD-BEARING, and neither is re-derived here:
//
//   - frame_rate is a STRING ("50") while width and height BESIDE IT are
//     NUMBERS. This is the same trap format.go documents for the per-node
//     detected formats, and it is answered the same way: parseVideoFormat, and
//     through it parseFrameRate, is the only thing in this package that knows
//     the rule. There is no second parser here.
//   - format.video is the only member of format. There is no audio block and no
//     scan_type — see the tombstone below for what that costs.
//
// WHAT IS DELIBERATELY NOT MODELLED. nodes[] and system_info are ignored on
// decode rather than kept, in the style events.go uses for Event. nodes[] is
// CONFIGURATION topology — 41 entries of {name, type, config} on matchH, e.g.
// {"name":"cam4","type":"mp2ts_receiver"} — and carries no format at all, so
// there is nothing in it this application could conform to; system_info is the
// firmware's build stamp (version 3.19.0, git a4355e67 on matchH). Unlike the
// status format objects, where VideoFormat.Raw exists precisely to make an
// unexpected shape visible, nothing here has to survive a surprise: the
// application needs the video block or it needs the fallback, and Raw already
// renders every field of THAT block including the ones with no typed home.
//
// -------------------------------------------------------------------------
// TOMBSTONE: why the format is no longer DERIVED from a running node
// -------------------------------------------------------------------------
//
// Until 2026-08-15 this package derived the conform format by scanning
// switcher_status for any node that was STREAMING and reading
// state.streams.video.format off it, on the reasoning that since every source
// must match the switcher, any running node is reporting the target. The
// reasoning is sound. The wire does not support it.
//
// MEASURED on the live matchH instance, with a real 1280x720p50 feed accepted
// and STREAMING on cam4 "COMMS", the switcher reported:
//
//	cam4  streaming  "COMMS"  video=codec="h264" width=1280 height=720 frame_rate="0" scan_type="P"
//	                          bit_depth=8 color_space="YCbCr" sample_format="420"
//
// frame_rate="0", stable across 45 s of continuous streaming. The derivation
// correctly REFUSED to build a 0 fps target and fell back — so the feature
// would have shipped, looked right in every test, and silently never fired.
//
// That is why the derivation is REMOVED rather than kept below this call as a
// fallback. A fallback has to be able to answer sometimes; this one answers
// only on the day the node's detected rate is right, and nothing on the wire
// tells us which day that is — a node reporting 0 is indistinguishable from a
// node reporting truthfully, so a second source would sit under the first
// contributing nothing but a status-socket dial on the START path and a rate
// nobody can trust. If you are reading this because you thought "we could just
// read it off a running node": that is what the paragraph above measured.
//
// TWO THINGS WERE LOST WITH IT, and they are named rather than quietly dropped:
//
//   - SCAN. The node's detected format carries scan_type ("P" on every healthy
//     node ever captured); the setting does not. So the warning that used to
//     fire when the switcher was interlaced and our progressive-only video leg
//     could not match it has no source any more. It was best-effort — no
//     interlaced sample has ever been captured, so the test for it was never
//     exercised against a real one — and internal/gst still pins
//     interlace-mode=progressive in its capsfilter, which turns a genuine
//     mismatch into a loud caps failure at Start rather than a silent one.
//   - DISAGREEMENT. Two running nodes streaming different rasters used to be
//     reportable by name. A setting cannot disagree with itself, so there is
//     nothing left to report; a non-conforming source is now the switcher's own
//     business, which is where it always belonged.

// switcherConfigurationPath is GET /api/v1/switcher_configuration: the
// instance's own settings, of which this application reads exactly one — the
// system video format every source feeding it must be produced in.
const switcherConfigurationPath = "/api/v1/switcher_configuration"

// SwitcherConfiguration is what the instance says it is CONFIGURED for: the
// video format every source feeding it, this application's contribution feed
// included, must be produced in.
//
// The embedded VideoFormat is the format itself, parsed by the same
// parseVideoFormat the status path uses, so Raw carries a rendering of every
// field the switcher sent — including bit_depth, color_space and signal_type,
// which have no typed home in VideoFormat and would otherwise be invisible.
//
// Codec is always empty here and that is not a fault: the setting describes a
// RASTER, not a compression. A codec belongs to a stream arriving at a node,
// which is a different question from the one this endpoint answers.
type SwitcherConfiguration struct {
	VideoFormat

	// SignalType is the wire field format.video.signal_type: "rec709" on
	// matchH. It is the COLORIMETRY, in M2L-X's vocabulary rather than
	// GStreamer's — Colorimetry translates it, and says at length why the
	// answer is not plumbed into the pipeline today.
	//
	// It is modelled rather than left to Raw because the video leg makes a
	// hard-coded claim about exactly this field, and a claim that the switcher
	// can contradict has to be checkable in code, not by eye.
	SignalType string
}

// Valid reports whether the configuration carries a usable raster and rate.
//
// All three of width, height and frame rate must have parsed. A partial format
// cannot be conformed to — 1920x0 at 50 is not a raster — and format.go's typed
// fields go to their Go zero when they cannot be parsed, so this is the check
// that stops a zero being read as an answer. A caller with a false here has
// learned nothing and must fall back exactly as if the call had failed.
func (s SwitcherConfiguration) Valid() bool {
	return s.Width > 0 && s.Height > 0 && s.FrameRate > 0
}

// String renders the format the way an operator reads a raster: 1920x1080@50.
//
// The rate is rendered with the smallest number of decimals that is exact, so
// 50 is "50" and 29.97 is "29.97" — a fixed %g would print 29.969999999999999
// for a rate that arrived as the string "29.97" and round-tripped through
// float64, and a fixed %.0f would print both 50 and 59.94 as whole numbers.
func (s SwitcherConfiguration) String() string {
	return strconv.Itoa(s.Width) + "x" + strconv.Itoa(s.Height) +
		"@" + strconv.FormatFloat(s.FrameRate, 'f', -1, 64)
}

// switcherConfigurationWire is the decode target: the one block of the response
// this application reads, kept as a json.RawMessage so that parseVideoFormat —
// not encoding/json — is what interprets a field whose type varies (frame_rate).
// Decoding straight into typed fields here would be the second parser this
// package refuses to have.
type switcherConfigurationWire struct {
	Format struct {
		Video json.RawMessage `json:"video"`
	} `json:"format"`
}

// SwitcherConfiguration implements Client.
//
// It requires a bearer token (ErrNotSignedIn otherwise) and never includes the
// token in an error, the same contract every other call in this package keeps.
//
// A response whose format block is missing or unparseable is NOT an error: the
// call reached the instance and the instance answered, so the value comes back
// with Valid() false and Raw carrying whatever was actually there. That
// distinction matters to a caller's log line — "the switcher would not answer"
// and "the switcher answered something this build does not understand" are
// different faults with different fixes — and to nothing else, because both end
// at the same fallback.
func (c *client) SwitcherConfiguration(ctx context.Context) (SwitcherConfiguration, error) {
	tok := c.Token()
	if tok == "" {
		return SwitcherConfiguration{}, ErrNotSignedIn
	}

	var wire switcherConfigurationWire
	if err := c.doJSON(ctx, http.MethodGet, switcherConfigurationPath, nil, &wire, tok); err != nil {
		return SwitcherConfiguration{}, fmt.Errorf("m2lx: switcher configuration: %w", err)
	}

	conf := SwitcherConfiguration{VideoFormat: parseVideoFormat(wire.Format.Video)}
	if fields, ok := formatFields(wire.Format.Video); ok {
		conf.SignalType, _ = jsonString(fields["signal_type"])
	}
	return conf, nil
}

// Colorimetry translates the switcher's signal_type into the name GStreamer
// uses for the same thing, e.g. "rec709" into "bt709".
//
// # Why this is a translation and not a plumbing job
//
// internal/gst pins colorimetry=bt709 in the video leg's spatial capsfilter
// (ConformTarget.spatialCaps). The switcher reports rec709. Those are the same
// colorimetry under two vocabularies, so the hard-coded value is CORRECT on
// every instance measured — and that agreement is now checkable rather than
// assumed, which is the point of modelling the field at all. app.go compares
// the two at Start and says so in the log.
//
// The value is not fed into the capsfilter today, for a reason that is about
// evidence and not about effort: no instance has ever been observed reporting
// anything but rec709, so a mapping table would be shipping guesses about
// vocabulary this application has never seen, in the one place — negotiation
// caps — where a wrong guess does not degrade gracefully but fails to link the
// pipeline at all. The table below exists so that the day a second value IS
// seen, the work is to pass an already-translated string through rather than to
// start the investigation from the log line that reported it.
//
// ok is false for a signal type this package does not recognise, INCLUDING the
// empty string. There is deliberately no default translation: a caller that
// cannot be told what the switcher meant must keep the value it already has and
// say that it did, which is a far better failure than conforming to a
// colorimetry nobody chose.
func (s SwitcherConfiguration) Colorimetry() (string, bool) {
	name, ok := gstColorimetry[strings.ToLower(strings.TrimSpace(s.SignalType))]
	return name, ok
}

// gstColorimetry maps M2L-X's signal_type vocabulary to GStreamer's colorimetry
// names.
//
// Only "rec709" is MEASURED (matchH, 2026-08-15). The other two are here
// because their GStreamer counterparts are unambiguous and a switcher that
// reports one should produce a log line naming a colorimetry rather than
// "unrecognised": rec601 is standard-definition's, rec2020 is UHD's. Any HDR
// vocabulary — rec2100, HLG, PQ — is deliberately absent, because in GStreamer
// those are not one colorimetry string but a transfer function beside a
// primaries choice, and guessing which spelling a switcher means would be
// exactly the wrong kind of confidence.
var gstColorimetry = map[string]string{
	"rec601":  "bt601",
	"rec709":  "bt709",
	"rec2020": "bt2020",
}
