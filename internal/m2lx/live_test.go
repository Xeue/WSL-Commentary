//go:build live

// live_test.go is the Gate C check of the conform format against a real M2L-X
// instance.
//
// Owner: WP-2. It is behind the `live` build tag and never runs in a normal
// suite, on the same footing as internal/gst/live_test.go: everything the
// parsers do with a body is settled by configuration_test.go and format_test.go
// against recorded captures, and the only thing that cannot be settled there is
// whether a LIVE instance still has the shape those captures have.
//
// It is READ-ONLY in the strongest sense available: it signs in, GETs the
// instance's configuration, opens the status socket, takes the one frame that
// carries a whole document, and closes it again. It never writes to the socket
// (RawSnapshot cannot) and it calls no REST endpoint that changes anything.
// CONTRACT.md rule 4 forbids writing to a live mixer and nothing here comes
// near it.
//
// Run it against one of the seven facility instances, receiver-first concerns
// not applying because nothing is transmitted:
//
//	M2LX_LIVE_HOST=m2lx-wslstudios-matchh.etapsiota.com \
//	M2LX_LIVE_ALIAS=matchh \
//	M2LX_LIVE_PASSWORD=... \
//	go test ./internal/m2lx -tags live -run Live -count=1 -v
//
// The password is the one in the OS credential store under
// WSLComms/<instance>/m2lx. It is taken from the environment rather than read
// from the store here so that this file has no dependency on internal/secrets
// and cannot, even by accident, become a way to get a secret back out of the
// store — the "there is deliberately no getter" rule in that package's doc.
//
// It SKIPS rather than fails when the environment is not set, so a full
// `-tags live` run on a machine with no instance to hand is quiet.
package m2lx

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"testing"
	"time"
)

// liveEnv reads the three values that name an instance, or skips.
func liveEnv(t *testing.T) (host, alias, password string) {
	t.Helper()
	host = os.Getenv("M2LX_LIVE_HOST")
	alias = os.Getenv("M2LX_LIVE_ALIAS")
	password = os.Getenv("M2LX_LIVE_PASSWORD")
	if host == "" || alias == "" || password == "" {
		t.Skip("M2LX_LIVE_HOST, M2LX_LIVE_ALIAS and M2LX_LIVE_PASSWORD are not all set")
	}
	return host, alias, password
}

// TestLiveSwitcherConfiguration signs in to a real instance and reads the
// authoritative answer: GET /api/v1/switcher_configuration.
//
// It PRINTS the response verbatim — the whole body's top-level keys, the format
// block byte for byte, and what the parser made of it — because the point of a
// live test is to put the wire on the record for the next person, not to
// restate what the recorded fixture already proves.
//
// What it ASSERTS is only what must be true of any instance: that the endpoint
// answers, that its body has a format block, and that the block parses into a
// usable raster and rate. A hard assertion on 1920x1080p50 would be exactly the
// compiled-in assumption this whole change exists to remove — an instance
// configured for 720p50 is not a test failure, it is the case the read is for.
func TestLiveSwitcherConfiguration(t *testing.T) {
	host, alias, password := liveEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// newClient rather than NewClient: this test reports the VERBATIM body
	// beside the parsed value, which needs the concrete client's doJSON. The
	// two calls below therefore go out over the same connection the shipped
	// code uses, with the same bearer and the same ten-second httpTimeout.
	client := newClient(host)
	defer client.Close()

	if err := client.SignIn(ctx, alias, password); err != nil {
		t.Fatalf("signing in to %s as %q: %v", host, alias, err)
	}

	// 1. The body, verbatim and undecoded.
	var body json.RawMessage
	start := time.Now()
	if err := client.doJSON(ctx, http.MethodGet, switcherConfigurationPath, nil, &body, client.Token()); err != nil {
		t.Fatalf("GET %s from %s: %v", switcherConfigurationPath, host, err)
	}
	elapsed := time.Since(start)
	t.Logf("GET %s: %d bytes in %v", switcherConfigurationPath, len(body), elapsed.Round(time.Millisecond))

	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("the response is not a JSON object: %v", err)
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("top-level keys: %v (%d bytes of format, the rest is nodes[] and system_info, "+
		"both deliberately unmodelled)", keys, len(top["format"]))
	t.Logf("format VERBATIM: %s", top["format"])

	// 2. The same body through the shipped parser.
	conf, err := client.SwitcherConfiguration(ctx)
	if err != nil {
		t.Fatalf("SwitcherConfiguration: %v", err)
	}
	if !conf.Valid() {
		t.Fatalf("the configuration parsed to nothing usable (raw %q) — this instance would "+
			"fall back to videoFormatOverride and then to 1920x1080p50", conf.Raw)
	}
	t.Logf("CONFIGURED FORMAT: %s", conf)
	t.Logf("  raw: %s", conf.Raw)

	// 3. The colorimetry, which the video leg hard-codes as bt709. Reported
	//    rather than asserted: an instance stating something else is a fact to
	//    find out about, not a test failure here.
	c, known := conf.Colorimetry()
	switch {
	case !known:
		t.Logf("  COLORIMETRY: signal_type %q is not in this application's table; the video "+
			"leg stays pinned at bt709", conf.SignalType)
	case c == "bt709":
		t.Logf("  COLORIMETRY: signal_type %q is GStreamer's %q, which is exactly what the "+
			"video leg pins. The hard-coded value agrees with the switcher.", conf.SignalType, c)
	default:
		t.Logf("  COLORIMETRY: signal_type %q is GStreamer's %q and the video leg pins bt709 — "+
			"they DISAGREE, and app.go logs a warning at Start", conf.SignalType, c)
	}
}

// TestLiveConformSources is the side-by-side that settles the design question:
// the instance's SETTING against the detected format of whatever is streaming
// on it.
//
// It exists because the second of those was once the mechanism, and the
// measurement that retired it is only reproducible against a live instance with
// a feed running. On matchH on 2026-08-15, with a real 1280x720p50 feed
// accepted and STREAMING on cam4, the node reported frame_rate="0" while the
// setting said 50. This test prints both, so the next person who wonders
// whether the derivation could be brought back can re-run it and see.
//
// It asserts nothing about the nodes. With every input stopped — the normal
// state of a facility before kick-off — there is simply nothing to compare, and
// that is reported rather than failed.
func TestLiveConformSources(t *testing.T) {
	host, alias, password := liveEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client := NewClient(host)
	defer client.Close()

	if err := client.SignIn(ctx, alias, password); err != nil {
		t.Fatalf("signing in to %s as %q: %v", host, alias, err)
	}

	conf, err := client.SwitcherConfiguration(ctx)
	if err != nil {
		t.Fatalf("reading the switcher configuration from %s: %v", host, err)
	}
	t.Logf("THE SETTING (/api/v1/switcher_configuration): %s — %s", conf, conf.Raw)

	raw, err := NewWatcher(host, client).RawSnapshot(ctx)
	if err != nil {
		t.Fatalf("taking an opening snapshot from %s: %v", host, err)
	}
	t.Logf("opening snapshot: %d bytes", len(raw))

	nodes, err := extractAll(raw)
	if err != nil {
		t.Fatalf("the opening snapshot is not a switcher_status frame: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatal("the opening snapshot carries no router inputs at all")
	}

	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	sort.Strings(names)

	streaming := 0
	for _, name := range names {
		n := nodes[name]
		t.Logf("  %-12s %-10s %-24q video=%s audio=%d",
			name, n.StreamState, n.DisplayName, orNone(n.Video), n.AudioCount)
		if n.StreamState == StreamStateStreaming {
			streaming++
		}
	}

	if streaming == 0 {
		t.Logf("NOTHING IS STREAMING on %s, so there is no detected format to compare the "+
			"setting against. That is the ordinary state of an instance nobody is feeding — "+
			"and precisely why the setting, which is always there, is what the video leg is "+
			"built from.", host)
		return
	}

	// Something is running. Print what it detects beside what the instance is
	// configured for. A rate of 0 here is the measurement in this test's
	// comment, reproduced.
	entries, err := parseFrame(raw)
	if err != nil {
		t.Fatalf("parsing the opening snapshot: %v", err)
	}
	for _, e := range entries {
		name, node, ok := decodeStreamEntry(e)
		if !ok || node.StreamState != StreamStateStreaming {
			continue
		}
		f := parseVideoFormat(node.Streams.Video.Format)
		t.Logf("DETECTED on node %q: %dx%d@%v — %s", name, f.Width, f.Height, f.FrameRate, f.Raw)
		if f.FrameRate <= 0 {
			t.Logf("  ^ THE MEASUREMENT: this node is STREAMING and reports no usable frame "+
				"rate. Deriving the conform format from it would produce nothing, which is "+
				"why the derivation was removed. The setting says %s.", conf)
		}
	}
}

// TestLiveVideoFormatShape re-checks the two wire facts the derivation rests
// on, on a live instance rather than on a 2026-07-31 capture: frame_rate is a
// STRING beside numeric width and height, and a stopped node's format is JSON
// null. Both are asserted per node and reported rather than assumed, because if
// a firmware ever changes either one this is the test that says so instead of
// the conform target quietly going to zero.
func TestLiveVideoFormatShape(t *testing.T) {
	host, alias, password := liveEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client := NewClient(host)
	defer client.Close()
	if err := client.SignIn(ctx, alias, password); err != nil {
		t.Fatalf("signing in to %s as %q: %v", host, alias, err)
	}
	raw, err := NewWatcher(host, client).RawSnapshot(ctx)
	if err != nil {
		t.Fatalf("taking an opening snapshot from %s: %v", host, err)
	}

	entries, err := parseFrame(raw)
	if err != nil {
		t.Fatalf("parsing the opening snapshot: %v", err)
	}

	var running, stopped, nullFormat, stringRate, numberRate int
	for _, e := range entries {
		name, node, ok := decodeStreamEntry(e)
		if !ok {
			continue
		}
		if node.StreamState == StreamStateStreaming {
			running++
		} else {
			stopped++
		}
		f := node.Streams.Video.Format
		switch {
		case isJSONAbsent(f):
			nullFormat++
			if node.StreamState == StreamStateStreaming {
				t.Errorf("node %q is streaming and its video format is null", name)
			}
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(f, &fields); err != nil {
			t.Errorf("node %q has a video format that is not an object: %s", name, f)
			continue
		}
		switch {
		case len(fields["frame_rate"]) > 0 && fields["frame_rate"][0] == '"':
			stringRate++
		case isJSONNumber(fields["frame_rate"]):
			numberRate++
			t.Logf("NOTE: node %q sends frame_rate as a NUMBER (%s). format.go tolerates "+
				"it; the trap it was written for is the string.", name, fields["frame_rate"])
		default:
			t.Errorf("node %q sends frame_rate as neither string nor number: %s", name, fields["frame_rate"])
		}
		if !isJSONNumber(fields["width"]) || !isJSONNumber(fields["height"]) {
			t.Errorf("node %q sends width/height as something other than numbers: %s / %s",
				name, fields["width"], fields["height"])
		}
	}

	t.Logf("%d router inputs: %d streaming, %d not; %d null formats, "+
		"%d string frame_rates, %d numeric frame_rates",
		running+stopped, running, stopped, nullFormat, stringRate, numberRate)
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
