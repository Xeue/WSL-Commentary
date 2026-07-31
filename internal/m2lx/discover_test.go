package m2lx

import (
	"context"
	"testing"
	"time"
)

// doc is a Document literal with the times measured from base, which is the
// fake clock's epoch (see clock_test.go).
func doc(offset time.Duration, nodes map[string]NodeState) Document {
	return Document{Nodes: nodes, At: base.Add(offset)}
}

func streaming(video string, audio int) NodeState {
	return NodeState{StreamState: StreamStateStreaming, Video: video, AudioCount: audio}
}

func stopped() NodeState {
	return NodeState{StreamState: StreamStateStopped}
}

// ---------------------------------------------------------------------------
// Discovery — the pure logic
// ---------------------------------------------------------------------------

func TestDiscoveryTakesTheFirstDocumentAsTheBaseline(t *testing.T) {
	// The first snapshot is the "before". Nothing in it can be a candidate,
	// however healthy it looks — a node already streaming when we started
	// watching started without us.
	d := NewDiscovery()

	if changed := d.Observe(doc(0, map[string]NodeState{
		"cam7": streaming("h264 1920x1080 50 P", 1),
		"cam8": stopped(),
	})); changed {
		t.Fatal("the baseline document produced a candidate")
	}
	if got := d.Candidates(); len(got) != 0 {
		t.Fatalf("Candidates() after only a baseline = %+v, want none", got)
	}
	if !d.Started() {
		t.Fatal("Started() = false after a baseline was taken")
	}
}

func TestDiscoveryReportsTheNodeThatStartsStreaming(t *testing.T) {
	d := NewDiscovery()
	d.Observe(doc(0, map[string]NodeState{
		"cam7": stopped(),
		"cam8": streaming("h264 1920x1080 50 P", 1),
	}))

	changed := d.Observe(doc(2500*time.Millisecond, map[string]NodeState{
		"cam7": streaming("h264 1920x1080 50 P", 1),
		"cam8": streaming("h264 1920x1080 50 P", 1),
	}))
	if !changed {
		t.Fatal("Observe() reported no change when a node started streaming")
	}

	got := d.Candidates()
	if len(got) != 1 {
		t.Fatalf("Candidates() = %+v, want exactly cam7", got)
	}
	c := got[0]
	if c.Key != "cam7" {
		t.Errorf("candidate key = %q, want cam7 — cam8 was already streaming in the baseline", c.Key)
	}
	if c.Was != StreamStateStopped || c.Now != StreamStateStreaming {
		t.Errorf("candidate transition = %q -> %q, want stopped -> streaming", c.Was, c.Now)
	}
	if c.Video != "h264 1920x1080 50 P" {
		t.Errorf("candidate video = %q, want the format verbatim", c.Video)
	}
	if c.AudioCount != 1 {
		t.Errorf("candidate audioCount = %d, want 1", c.AudioCount)
	}
	if c.AfterSeconds != 2.5 {
		t.Errorf("candidate afterSeconds = %v, want 2.5", c.AfterSeconds)
	}
}

func TestDiscoveryNeverPromotesANodeThatWasAlreadyStreaming(t *testing.T) {
	// This is the whole safety property. Another operator's input, up before we
	// pressed START and still up an hour later, must never be offered as ours.
	d := NewDiscovery()
	d.Observe(doc(0, map[string]NodeState{"cam3": streaming("h264 1920x1080 50 P", 1)}))

	for i := 1; i <= 10; i++ {
		d.Observe(doc(time.Duration(i)*time.Second, map[string]NodeState{
			"cam3": streaming("h264 1920x1080 50 P", 1),
		}))
	}

	if got := d.Candidates(); len(got) != 0 {
		t.Fatalf("Candidates() = %+v, want none; cam3 never changed state", got)
	}
}

func TestDiscoveryReportsEveryNodeThatChanged(t *testing.T) {
	// Two inputs coming up in the same second is exactly the case where picking
	// one would be picking at random. Both are reported and the caller says so.
	d := NewDiscovery()
	d.Observe(doc(0, map[string]NodeState{
		"cam7": stopped(),
		"cam9": stopped(),
	}))
	d.Observe(doc(3*time.Second, map[string]NodeState{
		"cam7": streaming("h264 1920x1080 50 P", 1),
		"cam9": streaming("h264 1280x720 25 P", 0),
	}))

	got := d.Candidates()
	if len(got) != 2 {
		t.Fatalf("Candidates() = %+v, want both cam7 and cam9", got)
	}
	// Same instant, so the tie-break is by name: stable between calls.
	if got[0].Key != "cam7" || got[1].Key != "cam9" {
		t.Errorf("Candidates() order = %q, %q; want a stable alphabetical tie-break", got[0].Key, got[1].Key)
	}
}

func TestDiscoveryOrdersBySoonestFirst(t *testing.T) {
	d := NewDiscovery()
	d.Observe(doc(0, map[string]NodeState{"cam1": stopped(), "cam2": stopped()}))
	d.Observe(doc(6*time.Second, map[string]NodeState{"cam2": streaming("", 0)}))
	d.Observe(doc(9*time.Second, map[string]NodeState{"cam1": streaming("", 0)}))

	got := d.Candidates()
	if len(got) != 2 {
		t.Fatalf("Candidates() = %+v, want two", got)
	}
	if got[0].Key != "cam2" || got[1].Key != "cam1" {
		t.Errorf("Candidates() = %q then %q, want the earlier transition first", got[0].Key, got[1].Key)
	}
}

func TestDiscoveryKeepsTheFirstTransitionForAFlappingNode(t *testing.T) {
	// A node that starts, stops and starts again is one candidate with the
	// timing of the transition that coincided with our START — not the latest
	// one, which by then is minutes away from any evidence.
	d := NewDiscovery()
	d.Observe(doc(0, map[string]NodeState{"cam7": stopped()}))
	d.Observe(doc(2*time.Second, map[string]NodeState{"cam7": streaming("first", 1)}))
	d.Observe(doc(5*time.Second, map[string]NodeState{"cam7": stopped()}))

	changed := d.Observe(doc(8*time.Second, map[string]NodeState{"cam7": streaming("second", 2)}))
	if changed {
		t.Error("Observe() reported a change for a node that was already a candidate")
	}

	got := d.Candidates()
	if len(got) != 1 {
		t.Fatalf("Candidates() = %+v, want one entry for cam7", got)
	}
	if got[0].AfterSeconds != 2 || got[0].Video != "first" {
		t.Errorf("candidate = %+v, want the first transition (2 s, %q)", got[0], "first")
	}
}

func TestDiscoveryMarksANodeMissingFromTheBaselineAsAbsent(t *testing.T) {
	// A node that only appears once it is streaming is still evidence, but the
	// operator is told the difference: there was no "before" for it.
	d := NewDiscovery()
	d.Observe(doc(0, map[string]NodeState{"cam1": stopped()}))
	d.Observe(doc(4*time.Second, map[string]NodeState{
		"cam1": stopped(),
		"cam7": streaming("h264 1920x1080 50 P", 1),
	}))

	got := d.Candidates()
	if len(got) != 1 || got[0].Key != "cam7" {
		t.Fatalf("Candidates() = %+v, want cam7", got)
	}
	if got[0].Was != NodeAbsent {
		t.Errorf("candidate.Was = %q, want %q", got[0].Was, NodeAbsent)
	}
}

func TestDiscoveryIgnoresTransitionsToAnythingButStreaming(t *testing.T) {
	// "starting" is not evidence: an input can sit in it and never come up.
	d := NewDiscovery()
	d.Observe(doc(0, map[string]NodeState{"cam7": stopped()}))
	d.Observe(doc(2*time.Second, map[string]NodeState{"cam7": {StreamState: StreamStateStarting}}))

	if got := d.Candidates(); len(got) != 0 {
		t.Fatalf("Candidates() = %+v, want none for a stopped -> starting change", got)
	}
}

func TestDiscoveryWithNoDocumentsHasNotStarted(t *testing.T) {
	// The distinction the caller reports: "watched and matched nothing" versus
	// "the socket never delivered a thing".
	d := NewDiscovery()
	if d.Started() {
		t.Error("Started() = true before any document arrived")
	}
	if got := d.Candidates(); len(got) != 0 {
		t.Errorf("Candidates() = %+v, want none", got)
	}
}

func TestDiscoveryClampsABackwardsClock(t *testing.T) {
	// The status socket's frames are timestamped on receipt by this process. A
	// clock step backwards mid-discovery must not produce a candidate that
	// appears to have arrived before it was looked for.
	d := NewDiscovery()
	d.Observe(doc(10*time.Second, map[string]NodeState{"cam7": stopped()}))
	d.Observe(doc(4*time.Second, map[string]NodeState{"cam7": streaming("", 0)}))

	got := d.Candidates()
	if len(got) != 1 {
		t.Fatalf("Candidates() = %+v, want one", got)
	}
	if got[0].AfterSeconds != 0 {
		t.Errorf("afterSeconds = %v, want 0 for a backwards clock", got[0].AfterSeconds)
	}
}

// ---------------------------------------------------------------------------
// extractAll
// ---------------------------------------------------------------------------

func TestExtractAllReadsEveryNode(t *testing.T) {
	payload := []byte(`{
		"cam7": {"stream_state":"streaming","streams":{"video":{"format":"h264 1920x1080 50 P"},"audio":[{"format":"aac 48000 2ch"}]}},
		"cam8": {"stream_state":"stopped","streams":{"video":{"format":""},"audio":[]}}
	}`)

	nodes, err := extractAll(payload)
	if err != nil {
		t.Fatalf("extractAll() error = %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("extractAll() = %+v, want two nodes", nodes)
	}
	if nodes["cam7"].StreamState != StreamStateStreaming {
		t.Errorf("cam7 stream_state = %q", nodes["cam7"].StreamState)
	}
	if nodes["cam7"].Video != "h264 1920x1080 50 P" {
		t.Errorf("cam7 video = %q, want the format verbatim", nodes["cam7"].Video)
	}
	if nodes["cam7"].AudioCount != 1 {
		t.Errorf("cam7 audioCount = %d, want 1", nodes["cam7"].AudioCount)
	}
	if nodes["cam8"].AudioCount != 0 {
		t.Errorf("cam8 audioCount = %d, want 0", nodes["cam8"].AudioCount)
	}
}

func TestExtractAllSkipsWhatIsNotANode(t *testing.T) {
	// A snapshot that carries a type tag, a timestamp or any other non-node key
	// must still yield its nodes. Discovery is impossible on an instance whose
	// frames have one extra field if one extra field throws the frame away.
	payload := []byte(`{
		"type": "switcher_status",
		"seq": 41,
		"meta": {"generated_at": "2026-07-31T12:00:00Z"},
		"cam7": {"stream_state":"streaming","streams":{"video":{"format":"h264"},"audio":[]}}
	}`)

	nodes, err := extractAll(payload)
	if err != nil {
		t.Fatalf("extractAll() error = %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("extractAll() = %+v, want only cam7", nodes)
	}
	if _, ok := nodes["cam7"]; !ok {
		t.Errorf("extractAll() lost cam7: %+v", nodes)
	}
}

func TestExtractAllRejectsAPayloadThatIsNotAnObject(t *testing.T) {
	if _, err := extractAll([]byte(`["cam7"]`)); err == nil {
		t.Error("extractAll() on a JSON array: error = nil, want non-nil")
	}
}

// ---------------------------------------------------------------------------
// WatchAll
// ---------------------------------------------------------------------------

func TestWatchAllEmitsWholeDocumentsWithoutDebouncing(t *testing.T) {
	// Watch debounces stream_state for DebounceWindow (4 s). WatchAll must not:
	// the transition it exists to catch is the very thing a debounce delays,
	// and a discovery window that only reported settled state would report the
	// change four seconds after the evidence.
	tw := newTestWatcher(&fakeClientToken{token: "tok"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	docs := tw.w.WatchAll(ctx)

	conn := newFakeConn()
	<-tw.dials
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	conn.push([]byte(`{"cam7":{"stream_state":"stopped","streams":{"video":{"format":""},"audio":[]}}}`))
	first := recvDoc(t, docs)
	if first.Nodes["cam7"].StreamState != StreamStateStopped {
		t.Fatalf("first document = %+v, want cam7 stopped", first.Nodes)
	}

	// Immediately afterwards, with no clock advance at all: a debouncing reader
	// would hold this back.
	conn.push([]byte(`{"cam7":{"stream_state":"streaming","streams":{"video":{"format":"h264 1920x1080 50 P"},"audio":[{"format":"aac 48000 2ch"}]}}}`))
	second := recvDoc(t, docs)
	if second.Nodes["cam7"].StreamState != StreamStateStreaming {
		t.Fatalf("second document = %+v, want cam7 streaming with no debounce delay", second.Nodes)
	}
	if second.Nodes["cam7"].AudioCount != 1 {
		t.Errorf("second document cam7 audioCount = %d, want 1", second.Nodes["cam7"].AudioCount)
	}
}

func TestWatchAllClosesItsChannelWhenTheContextIsDone(t *testing.T) {
	// The discovery goroutine in app.go ranges over this channel and relies on
	// it closing to exit. If it did not, the window's timeout would leak a
	// goroutine per START for the rest of the process.
	tw := newTestWatcher(&fakeClientToken{token: "tok"})
	ctx, cancel := context.WithCancel(context.Background())

	docs := tw.w.WatchAll(ctx)

	conn := newFakeConn()
	<-tw.dials
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	cancel()

	select {
	case _, ok := <-docs:
		if ok {
			// A document already in flight is fine; the close must still follow.
			select {
			case _, ok := <-docs:
				if ok {
					t.Fatal("WatchAll kept emitting after its context was cancelled")
				}
			case <-time.After(testSafetyTimeout):
				t.Fatal("WatchAll did not close its channel after cancellation")
			}
		}
	case <-time.After(testSafetyTimeout):
		t.Fatal("WatchAll did not close its channel after cancellation")
	}
}

func TestWatchAllDropsAMalformedFrameAndKeepsReading(t *testing.T) {
	tw := newTestWatcher(&fakeClientToken{token: "tok"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	docs := tw.w.WatchAll(ctx)

	conn := newFakeConn()
	<-tw.dials
	tw.nextConn <- wsConnOrErr{conn: conn}
	tw.waitStarted(t)

	conn.push([]byte(`not json at all`))
	conn.push([]byte(`{"cam7":{"stream_state":"streaming","streams":{"video":{"format":"h264"},"audio":[]}}}`))

	got := recvDoc(t, docs)
	if got.Nodes["cam7"].StreamState != StreamStateStreaming {
		t.Fatalf("document after a malformed frame = %+v, want cam7 streaming", got.Nodes)
	}
}

func recvDoc(t *testing.T, ch <-chan Document) Document {
	t.Helper()
	select {
	case d, ok := <-ch:
		if !ok {
			t.Fatal("document channel closed unexpectedly")
		}
		return d
	case <-time.After(testSafetyTimeout):
		t.Fatal("no document arrived within the safety timeout")
		return Document{}
	}
}
