//go:build live

// live_test.go is the Gate C check of the conform derivation against a real
// M2L-X instance.
//
// Owner: WP-2. It is behind the `live` build tag and never runs in a normal
// suite, on the same footing as internal/gst/live_test.go: everything the
// derivation does with a frame is settled by conform_test.go against the
// captures, and the only thing that cannot be settled there is whether a LIVE
// instance's opening snapshot still has the shape those captures have.
//
// It is READ-ONLY in the strongest sense available: it signs in, opens the
// status socket, takes the one frame that carries a whole document, and closes
// it again. It never writes to the socket (RawSnapshot cannot) and it calls no
// REST endpoint that changes anything. CONTRACT.md rule 4 forbids writing to a
// live mixer and nothing here comes near it.
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

// TestLiveConformFormat signs in to a real instance, takes one opening
// snapshot, and reports what the conform derivation makes of it.
//
// It asserts only what MUST be true of any instance — that the frame parses as
// a document with router inputs in it — and PRINTS the rest. A hard assertion
// on 1920x1080p50 would be exactly the compiled-in assumption this whole change
// exists to remove: an instance configured for 720p50 is not a test failure, it
// is the case the derivation is for.
func TestLiveConformFormat(t *testing.T) {
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
	for _, name := range names {
		n := nodes[name]
		t.Logf("  %-12s %-10s %-24q video=%s audio=%d",
			name, n.StreamState, n.DisplayName, orNone(n.Video), n.AudioCount)
	}

	got, ok := ConformFormatFrom(raw)
	if !ok {
		t.Logf("NO CONFORM FORMAT: no node on %s is streaming with a parseable format. "+
			"This is the ordinary state of an instance nobody is feeding, and the "+
			"caller falls back to its configured override and then to 1920x1080p50.", host)
		return
	}
	t.Logf("CONFORM FORMAT: %s (codec %q) read from node %q, %d node(s) agreeing",
		got, got.Codec, got.Node, got.Agreeing)
	t.Logf("  raw: %s", got.Raw)
	if len(got.Disagreeing) > 0 {
		t.Logf("  DISAGREEING: %v — one of these sources is not conforming to the instance",
			got.Disagreeing)
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
