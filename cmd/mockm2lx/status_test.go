package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"wslcomms/internal/m2lx"
)

// ===========================================================================
// Decoding in these tests is deliberately NOT done with the mock's own types.
//
// That is the mistake this whole change exists to correct. The previous
// version of this file decoded into map[string]statusNode — the same type
// status.go encoded — so every test passed while the mock served a document
// shape M2L-X has never sent and the real parser rejected every frame. A test
// that round-trips a program's own types proves the types are self-consistent
// and nothing else.
//
// So the types below are written out from
// internal/m2lx/testdata/switcher_status-live-2026-07-31.json, independently
// of switcherdoc.go, and hold everything below the entry as raw bytes.
// switcherdoc_test.go goes one better and compares against the capture itself.
// ===========================================================================

// wireFrame is one switcher_status push as the CAPTURE shows it.
type wireFrame struct {
	Status    []wireEntry `json:"status"`
	Timestamp int64       `json:"timestamp"`
}

type wireEntry struct {
	Node  string          `json:"node"`
	Path  string          `json:"path"`
	State json.RawMessage `json:"state"`
}

// entryFor returns the entry naming node, if the frame carries one.
func (f wireFrame) entryFor(node string) (wireEntry, bool) {
	for _, e := range f.Status {
		if e.Node == node {
			return e, true
		}
	}
	return wireEntry{}, false
}

// wireInput is the part of a router input's state these tests read.
type wireInput struct {
	DisplayName string `json:"display_name"`
	StreamState string `json:"stream_state"`
	Streams     struct {
		Audio []struct {
			Format json.RawMessage `json:"format"`
		} `json:"audio"`
		Video struct {
			Format json.RawMessage `json:"format"`
		} `json:"video"`
	} `json:"streams"`
}

func decodeInput(t *testing.T, raw json.RawMessage) wireInput {
	t.Helper()
	var in wireInput
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("decoding a router input state: %v\n%s", err, raw)
	}
	return in
}

// isJSONNull reports the measured "no format" shape: the literal null, which
// is neither an absent field nor an empty object.
func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// ===========================================================================
// Harness
// ===========================================================================

// newStatusTestServer wires only the status WebSocket handler behind an
// httptest.Server, and returns a ws:// URL to dial.
func newStatusTestServer(t *testing.T, a *App) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/switcher_status", a.handleStatusWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/switcher_status"
}

// statusWSURL builds the status socket URL for a bearer token, matching
// docs/archive-windows-app-spec-v1-rejected.md line 934's Uri.EscapeDataString
// — the base64 access token routinely contains '+', '/' and '=', all of which
// are meaningful in a query string, so it must be percent-encoded rather than
// concatenated raw.
func statusWSURL(base, token string) string {
	v := url.Values{}
	v.Set("access_token", token)
	return base + "?" + v.Encode()
}

// dialStatus signs in and opens the status socket.
func dialStatus(t *testing.T, a *App) *websocket.Conn {
	t.Helper()
	tok := signIn(t, a)
	conn, _, err := websocket.DefaultDialer.Dial(statusWSURL(newStatusTestServer(t, a), tok.AccessToken), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// readFrame reads one frame, failing the test if none arrives.
func readFrame(t *testing.T, conn *websocket.Conn) wireFrame {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var f wireFrame
	if err := conn.ReadJSON(&f); err != nil {
		t.Fatalf("reading a frame: %v", err)
	}
	return f
}

// startBroadcaster runs a's status broadcaster in the background until the
// test ends, mirroring startListener's (srt_test.go) cancel-then-wait cleanup
// discipline so no log line can fire after the test completes.
func startBroadcaster(t *testing.T, a *App) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.runStatusBroadcaster(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// ===========================================================================
// Upgrade
// ===========================================================================

func TestStatusWS_RejectsMissingToken(t *testing.T) {
	a := newAuthTestApp(t)
	url := newStatusTestServer(t, a)

	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatalf("expected the upgrade to be refused with no access_token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		code := -1
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("status = %d, want 401", code)
	}
}

func TestStatusWS_RejectsInvalidToken(t *testing.T) {
	a := newAuthTestApp(t)
	url := newStatusTestServer(t, a)

	_, resp, err := websocket.DefaultDialer.Dial(url+"?access_token=not-a-real-token", nil)
	if err == nil {
		t.Fatalf("expected the upgrade to be refused with an invalid access_token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 on invalid token")
	}
}

// ===========================================================================
// Frame 0: the opening snapshot
// ===========================================================================

// TestStatusWS_OpeningFrameIsTheWholeDocument pins the half of the protocol
// that is a property of the CONNECTION rather than of the clock.
func TestStatusWS_OpeningFrameIsTheWholeDocument(t *testing.T) {
	a := newAuthTestApp(t)
	f := readFrame(t, dialStatus(t, a))

	if len(f.Status) != 36 {
		t.Errorf("the opening frame carried %d entries, want the measured 36", len(f.Status))
	}
	if f.Timestamp <= 0 {
		t.Errorf("timestamp = %d, want epoch milliseconds", f.Timestamp)
	}
	// Epoch MILLISECONDS: internal/mixer reads this field, and reading it as
	// seconds or nanoseconds lands the drawer in 1970.
	if age := time.Since(time.UnixMilli(f.Timestamp)); age < 0 || age > time.Minute {
		t.Errorf("timestamp %d is %s away from now — it is not epoch milliseconds", f.Timestamp, age)
	}
	for _, e := range f.Status {
		if e.Path != wholeNodePath {
			t.Errorf("node %q in the opening snapshot is at path %q, want %q", e.Node, e.Path, wholeNodePath)
		}
	}
}

// TestStatusWS_SnapshotCarriesTheMeasuredStoppedShape is the shape internal/
// m2lx's format.go is written against: format is JSON null on a stopped input,
// and the audio array has ONE element rather than none.
func TestStatusWS_SnapshotCarriesTheMeasuredStoppedShape(t *testing.T) {
	a := newAuthTestApp(t)
	f := readFrame(t, dialStatus(t, a))

	e, ok := f.entryFor(a.opts.StatusKey)
	if !ok {
		t.Fatalf("the snapshot carries no entry for the status key %q", a.opts.StatusKey)
	}
	in := decodeInput(t, e.State)

	if in.StreamState != m2lx.StreamStateStopped {
		t.Errorf("stream_state = %q, want %q (no SRT peer connected)", in.StreamState, m2lx.StreamStateStopped)
	}
	if !isJSONNull(in.Streams.Video.Format) {
		t.Errorf("video.format = %s, want the measured JSON null of a stopped input", in.Streams.Video.Format)
	}
	// THE DISTINCTION THAT MATTERS. One element with a null format is
	// "stopped". An empty array is the MP2/AC-3 silent-drop signature, which is
	// a completely different fault; see the drop-audio test below.
	if len(in.Streams.Audio) != 1 {
		t.Fatalf("audio has %d element(s), want exactly 1 — a stopped input sends [{\"format\":null}], not []",
			len(in.Streams.Audio))
	}
	if !isJSONNull(in.Streams.Audio[0].Format) {
		t.Errorf("audio[0].format = %s, want JSON null", in.Streams.Audio[0].Format)
	}
}

// TestStatusWS_StreamingShapeIsTheMeasuredFormatObject pins the trap that
// broke this mock in the first place: format is an OBJECT, and frame_rate
// inside it is a STRING while width and height beside it are numbers.
func TestStatusWS_StreamingShapeIsTheMeasuredFormatObject(t *testing.T) {
	a := newAuthTestApp(t)
	a.setLieStreamState(m2lx.StreamStateStreaming)
	// The lie covers stream_state only, so drive the formats from the fault
	// that does reach them by connecting a peer... which no test here can do.
	// Instead assert the format objects the mock renders directly.
	st := a.inputState(mustRouterInput(t, a.opts.StatusKey))
	if st.StreamState != m2lx.StreamStateStreaming {
		t.Fatalf("the lie did not reach stream_state: %q", st.StreamState)
	}
	// ...and that the lie did NOT reach them, which is the point of the lie.
	if st.Streams.Video.Format != nil {
		t.Errorf("video.format = %+v, want null — the lie must not extend to detected formats",
			st.Streams.Video.Format)
	}

	// The measured healthy objects, checked field by field through JSON so the
	// wire types are what is asserted, not the Go ones.
	b, err := json.Marshal(measuredVideoFormat())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var video map[string]json.RawMessage
	if err := json.Unmarshal(b, &video); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := string(video["frame_rate"]); got != `"50"` {
		t.Errorf("frame_rate = %s, want the measured STRING \"50\"", got)
	}
	if got := string(video["width"]); got != "1920" {
		t.Errorf("width = %s, want the measured NUMBER 1920", got)
	}
	if got := string(video["height"]); got != "1080" {
		t.Errorf("height = %s, want the measured NUMBER 1080", got)
	}

	b, err = json.Marshal(measuredAudioFormat())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"bit_depth":0,"channel_count":2,"codec":"aac","sample_rate":48000}`; string(b) != want {
		t.Errorf("audio format = %s, want %s", b, want)
	}
}

func mustRouterInput(t *testing.T, node string) inputSpec {
	t.Helper()
	in, ok := lookupRouterInput(node)
	if !ok {
		t.Fatalf("%q is not one of the measured router inputs", node)
	}
	return in
}

// ===========================================================================
// The deltas
// ===========================================================================

// TestStatusWS_FramesAfterTheSnapshotAreSubtreeDeltas is the other half of the
// protocol, and the half the previous mock did not have at all: a mock that
// only sends snapshots cannot exercise the merge in internal/m2lx/document.go.
func TestStatusWS_FramesAfterTheSnapshotAreSubtreeDeltas(t *testing.T) {
	a := newAuthTestApp(t)
	a.opts.StatusInterval = 5 * time.Millisecond
	conn := dialStatus(t, a)
	readFrame(t, conn) // frame 0
	startBroadcaster(t, a)

	paths := map[string]int{}
	for i := 0; i < 40; i++ {
		f := readFrame(t, conn)
		if len(f.Status) != 1 {
			t.Fatalf("delta %d carried %d entries, want exactly 1", i, len(f.Status))
		}
		e := f.Status[0]
		if e.Path == wholeNodePath {
			t.Fatalf("delta %d is at path %q — that is a whole node, and the merge in "+
				"internal/m2lx would replace the node rather than patch a subtree", i, wholeNodePath)
		}
		paths[e.Path]++
	}
	// The measured mix is overwhelmingly meters, which is exactly why
	// internal/m2lx has an emit gate: about 21 frames a second carrying
	// nothing any lamp reads.
	if paths[pathLevels] == 0 || paths[pathPeakLevels] == 0 {
		t.Errorf("40 deltas produced no %s and/or no %s: %v", pathLevels, pathPeakLevels, paths)
	}
}

// TestDeltaKindFor_ReproducesTheMeasuredMix checks the proportions against the
// 3180-frame measurement recorded in internal/m2lx/wire.go.
func TestDeltaKindFor_ReproducesTheMeasuredMix(t *testing.T) {
	counts := map[deltaKind]int{}
	for n := int64(0); n < 1000; n++ {
		counts[deltaKindFor(n)]++
	}
	// 15 statistics in 3180 frames is 0.47%; 10 in 1000 is 1%. Same order,
	// and frequent enough that a test does not have to wait a minute to see
	// the status key's own node move.
	if counts[deltaStatistics] != 10 {
		t.Errorf("statistics deltas = %d in 1000, want 10", counts[deltaStatistics])
	}
	// 163 peak_hold in 3180 is 5.1%; 40 in 1000 is 4%.
	if counts[deltaPeakHoldLevels] != 40 {
		t.Errorf("peak_hold_levels deltas = %d in 1000, want 40", counts[deltaPeakHoldLevels])
	}
	// The device's levels-to-peak_levels split was 1501/1500, i.e. even. This
	// comes out 500/450, because the peak_hold and statistics slots both land
	// on odd n and so are taken out of peak_levels' half. That is a 53/47 split
	// where the device is 50/50 — close enough that nothing downstream can tell,
	// and not worth complicating the arithmetic to close.
	lv, pk := counts[deltaLevels], counts[deltaPeakLevels]
	if lv+pk != 950 || lv != 500 || pk != 450 {
		t.Errorf("levels=%d peak_levels=%d, want 500/450 of the remaining 950", lv, pk)
	}
}

// ===========================================================================
// The stall fault
// ===========================================================================

func TestStatusWS_StallPushesNothingAtAll(t *testing.T) {
	a := newAuthTestApp(t)
	a.opts.StatusInterval = 5 * time.Millisecond
	a.setStallStatus(true)

	conn := dialStatus(t, a)
	startBroadcaster(t, a)

	// No frame 0 (the stall was already on at connect) and no deltas either:
	// sockets stay open and silent, which is the condition m2lx.StaleAfter
	// exists to catch.
	conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	var f wireFrame
	if err := conn.ReadJSON(&f); err == nil {
		t.Fatalf("expected no frame while stalled, got %d entries", len(f.Status))
	}
}

// TestStatusWS_AClientThatMissedFrameZeroGetsOneWhenTheStallClears is the
// consequence of the protocol being snapshot-then-delta: a delta merged into
// an empty document is a delta discarded (internal/m2lx/document.go), so a
// client that connected during a stall must be baselined before it is sent
// anything else, or it would receive frames forever and learn nothing.
func TestStatusWS_AClientThatMissedFrameZeroGetsOneWhenTheStallClears(t *testing.T) {
	a := newAuthTestApp(t)
	a.opts.StatusInterval = 5 * time.Millisecond
	a.setStallStatus(true)

	conn := dialStatus(t, a)
	startBroadcaster(t, a)
	time.Sleep(30 * time.Millisecond)
	a.setStallStatus(false)

	f := readFrame(t, conn)
	if len(f.Status) != 36 {
		t.Fatalf("the first frame after the stall cleared carried %d entries, want the whole 36-node document",
			len(f.Status))
	}
	if f.Status[0].Path != wholeNodePath {
		t.Fatalf("the first frame after the stall cleared is at path %q, want %q",
			f.Status[0].Path, wholeNodePath)
	}
}

// ===========================================================================
// The lie and drop-audio faults
// ===========================================================================

func TestStatusWS_LieOverridesStreamStateAndNothingElse(t *testing.T) {
	a := newAuthTestApp(t)
	a.setLieStreamState(m2lx.StreamStateStreaming) // claim streaming with no SRT peer
	f := readFrame(t, dialStatus(t, a))

	e, ok := f.entryFor(a.opts.StatusKey)
	if !ok {
		t.Fatalf("no entry for %q", a.opts.StatusKey)
	}
	in := decodeInput(t, e.State)
	if in.StreamState != m2lx.StreamStateStreaming {
		t.Errorf("stream_state = %q, want the lied value %q", in.StreamState, m2lx.StreamStateStreaming)
	}
	// The lie covers stream_state only. The rest of the document still tells
	// the truth, which is exactly what makes the lamp untrustworthy on its own
	// (spec section 8) and what the honest line under it exists for.
	if !isJSONNull(in.Streams.Video.Format) {
		t.Errorf("video.format = %s, want null — the lie must not extend to detected formats",
			in.Streams.Video.Format)
	}
}

// TestDropAudioIsAnEmptyArrayAndStoppedIsNot pins the distinction the whole
// AUDIO OK lamp turns on. They are not the same fault and must not render the
// same on the wire.
func TestDropAudioIsAnEmptyArrayAndStoppedIsNot(t *testing.T) {
	a := newAuthTestApp(t)
	in := mustRouterInput(t, a.opts.StatusKey)

	stopped, err := json.Marshal(a.inputState(in).Streams)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(stopped), `"audio":[{`) {
		t.Errorf("a stopped input rendered %s, want a ONE-element audio array with a null format", stopped)
	}

	a.setDropAudio(true)
	dropped, err := json.Marshal(a.inputState(in).Streams)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Literal [] and not null: WP-2 code checking len(Audio)==0 must see the
	// same shape whether the array is absent or force-emptied.
	if !strings.Contains(string(dropped), `"audio":[]`) {
		t.Errorf("the drop-audio fault rendered %s, want an EMPTY audio array", dropped)
	}
}

// ===========================================================================
// The decoy delta — the bug that condemned a working input once a second
// ===========================================================================

// naiveNodes is the parser this project actually shipped once: it reads every
// entry's "state" as a node and ignores "path" entirely.
//
// It is reproduced here so the decoy fault can be shown to defeat it. If a
// future change to the mock stopped emitting a delta this misreads, the fault
// would have quietly become decorative and this test is what says so.
func naiveNodes(t *testing.T, f wireFrame) map[string]wireInput {
	t.Helper()
	out := map[string]wireInput{}
	for _, e := range f.Status {
		var in wireInput
		if err := json.Unmarshal(e.State, &in); err != nil {
			continue
		}
		out[e.Node] = in
	}
	return out
}

// TestDecoyDelta_Statistics reproduces the measured trap frame.
func TestDecoyDelta_Statistics(t *testing.T) {
	a := newAuthTestApp(t)
	a.opts.StatusInterval = 5 * time.Millisecond
	a.setLieStreamState(m2lx.StreamStateStreaming)
	a.setDecoyDelta(decoyDeltaStatistics)

	conn := dialStatus(t, a)
	readFrame(t, conn) // frame 0, which really does say streaming
	startBroadcaster(t, a)

	f := readFrame(t, conn)
	if len(f.Status) != 1 || f.Status[0].Node != a.opts.StatusKey || f.Status[0].Path != pathStatistics {
		t.Fatalf("decoy frame = %+v, want one %s entry on %q", f.Status, pathStatistics, a.opts.StatusKey)
	}
	// The trap: a parser ignoring "path" reads this state as the node, finds
	// no stream_state, and concludes the one input that is working is not a
	// router input at all.
	naive := naiveNodes(t, f)[a.opts.StatusKey]
	if naive.StreamState != "" {
		t.Fatalf("the decoy was misread as stream_state %q; it must have none, or it does not "+
			"reproduce the bug", naive.StreamState)
	}
}

// TestDecoyDelta_StreamState is the sharper face of the same trap: a state at
// a subtree path that LOOKS like a whole node. A parser ignoring "path" does
// not merely fail to find the node — it believes the decoy and drives the
// lamps from it.
func TestDecoyDelta_StreamState(t *testing.T) {
	a := newAuthTestApp(t)
	a.opts.StatusInterval = 5 * time.Millisecond
	a.setLieStreamState(m2lx.StreamStateStreaming)
	a.setDecoyDelta(decoyDeltaStreamState)

	conn := dialStatus(t, a)
	snap := readFrame(t, conn)
	startBroadcaster(t, a)

	truth, ok := snap.entryFor(a.opts.StatusKey)
	if !ok {
		t.Fatalf("no entry for %q in the snapshot", a.opts.StatusKey)
	}
	if got := decodeInput(t, truth.State).StreamState; got != m2lx.StreamStateStreaming {
		t.Fatalf("the snapshot says %q, want %q — the decoy is only a lie if the truth is streaming",
			got, m2lx.StreamStateStreaming)
	}

	f := readFrame(t, conn)
	if len(f.Status) != 1 || f.Status[0].Path != pathStatistics {
		t.Fatalf("decoy frame = %+v, want one entry at %s", f.Status, pathStatistics)
	}
	naive := naiveNodes(t, f)[a.opts.StatusKey]
	if naive.StreamState != m2lx.StreamStateStopped {
		t.Fatalf("the decoy read as stream_state %q; want %q, so that a naive parser reports the "+
			"input STOPPED while it is streaming", naive.StreamState, m2lx.StreamStateStopped)
	}
	if !isJSONNull(naive.Streams.Video.Format) {
		t.Errorf("the decoy's video.format = %s, want null so a naive parser also reports NO VIDEO",
			naive.Streams.Video.Format)
	}
}

// ===========================================================================
// transition-push
// ===========================================================================

// TestTransitionPush_Node publishes a change as a whole-node entry.
func TestTransitionPush_Node(t *testing.T) {
	a := newAuthTestApp(t)
	a.opts.StatusInterval = 5 * time.Millisecond
	conn := dialStatus(t, a)
	readFrame(t, conn)
	startBroadcaster(t, a)

	a.setLieStreamState(m2lx.StreamStateStreaming)

	f := waitForNodeFrame(t, conn, a.opts.StatusKey)
	if f.Status[0].Path != wholeNodePath {
		t.Fatalf("transition pushed at path %q, want %q", f.Status[0].Path, wholeNodePath)
	}
	if got := decodeInput(t, f.Status[0].State).StreamState; got != m2lx.StreamStateStreaming {
		t.Errorf("transition carried stream_state %q, want %q", got, m2lx.StreamStateStreaming)
	}
}

// TestTransitionPush_Delta publishes the same change as subtree deltas, so it
// is only visible to a consumer that MERGES them.
func TestTransitionPush_Delta(t *testing.T) {
	a := newAuthTestApp(t)
	a.opts.StatusInterval = 5 * time.Millisecond
	a.setTransitionPush(transitionPushDelta)
	conn := dialStatus(t, a)
	readFrame(t, conn)
	startBroadcaster(t, a)

	a.setLieStreamState(m2lx.StreamStateStreaming)

	// Formats first, then the value that lights the lamp.
	first := waitForNodeFrame(t, conn, a.opts.StatusKey)
	if first.Status[0].Path != pathStreams {
		t.Fatalf("first transition frame at path %q, want %q", first.Status[0].Path, pathStreams)
	}
	second := waitForNodeFrame(t, conn, a.opts.StatusKey)
	if second.Status[0].Path != pathStreamStateOnly {
		t.Fatalf("second transition frame at path %q, want %q", second.Status[0].Path, pathStreamStateOnly)
	}
	var state string
	if err := json.Unmarshal(second.Status[0].State, &state); err != nil {
		t.Fatalf("the %s delta's state is not a JSON string: %v", pathStreamStateOnly, err)
	}
	if state != m2lx.StreamStateStreaming {
		t.Errorf("%s delta carried %q, want %q", pathStreamStateOnly, state, m2lx.StreamStateStreaming)
	}
}

// TestTransitionPush_None publishes nothing, which is the unproven worst case
// m2lx.resyncInterval is a backstop against: the change is real, the socket
// never mentions it, and only a reconnect reveals it.
func TestTransitionPush_None(t *testing.T) {
	a := newAuthTestApp(t)
	a.opts.StatusInterval = 5 * time.Millisecond
	a.setTransitionPush(transitionPushNone)
	conn := dialStatus(t, a)
	readFrame(t, conn)
	startBroadcaster(t, a)

	a.setLieStreamState(m2lx.StreamStateStreaming)

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline)
		var f wireFrame
		if err := conn.ReadJSON(&f); err != nil {
			break
		}
		if len(f.Status) > 0 && f.Status[0].Node == a.opts.StatusKey && f.Status[0].Path == wholeNodePath {
			t.Fatalf("a whole-node entry was pushed with -transition-push=none: %+v", f.Status[0])
		}
	}

	// ...and a NEW connection sees the change immediately, because frame 0 is
	// always the current document. That is precisely the reconnect the
	// Watcher's resync performs.
	fresh := readFrame(t, dialStatus(t, a))
	e, ok := fresh.entryFor(a.opts.StatusKey)
	if !ok {
		t.Fatalf("no entry for %q in the fresh snapshot", a.opts.StatusKey)
	}
	if got := decodeInput(t, e.State).StreamState; got != m2lx.StreamStateStreaming {
		t.Errorf("a fresh connection reported %q, want %q — a reconnect must reveal what the "+
			"deltas never announced", got, m2lx.StreamStateStreaming)
	}
}

// waitForNodeFrame reads until a TRANSITION frame about node arrives.
//
// It skips two things: the meter traffic that makes up most of this socket,
// and the periodic "/statistics" delta, which is also about node but is
// routine rather than a transition.
func waitForNodeFrame(t *testing.T, conn *websocket.Conn, node string) wireFrame {
	t.Helper()
	for i := 0; i < 200; i++ {
		f := readFrame(t, conn)
		if len(f.Status) > 0 && f.Status[0].Node == node && f.Status[0].Path != pathStatistics {
			return f
		}
	}
	t.Fatalf("no frame mentioning %q arrived in 200 frames", node)
	return wireFrame{}
}
