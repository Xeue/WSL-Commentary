//go:build cgo && !gststub

// diagnose_cgo.go is a pipeline dissector. It builds the picture path the app
// builds -- srtsrc ! tsdemux ! queue ! h265parse ! avdec_h265 ! fakesink -- and
// taps every boundary in it with a probe, so the data can be watched degrading
// stage by stage and the exact element where it breaks can be named rather than
// guessed. It is the answer to a day of "every log reframes it": one run prints,
// for each pad, how many buffers crossed, their flag histogram (DISCONT,
// CORRUPTED, GAP...), and the caps; a NAL census of what reaches the parser; the
// decoded/corrupt frame count out of the decoder; and every bus message posted
// underneath (h265parse "broken/invalid", tsdemux "CONTINUITY", libav errors).
//
// It forces the SOFTWARE decoder by default so a run here and a run on the field
// machine are comparable, and leaves avdec output-corrupt at its default TRUE so
// a frame the decoder knows is damaged comes out FLAGGED and is counted rather
// than hidden. Run it on the box that tears and one that does not, diff the two
// stage tables, and the first stage whose numbers diverge is the fault.
//
// It is separate from the live pipeline in picture_cgo.go and shares none of its
// state: a diagnostic must never be able to take a contribution feed off air.
package gst

/*
#include <gst/gst.h>

// wslcomms_diag_replace_buffer is the harness's copy of the app's probe buffer
// swap (picture_cgo.go): it exists so WSLCOMMS_DIAGNOSE_REINJECT can exercise the
// exact parameter re-injection surgery on real buffers through a real decoder.
// cgo preambles are per-file, so the app's static helper is not visible here.
static void wslcomms_diag_replace_buffer(gpointer info_ptr, gpointer new_buf) {
    GstPadProbeInfo *info = (GstPadProbeInfo *)info_ptr;
    GstBuffer *nb = (GstBuffer *)new_buf;
    GstBuffer *old = GST_PAD_PROBE_INFO_BUFFER(info); // info->data, transfer none
    if (old) {
        gst_buffer_copy_into(nb, old,
            GST_BUFFER_COPY_FLAGS | GST_BUFFER_COPY_TIMESTAMPS | GST_BUFFER_COPY_META,
            0, (gsize) -1);
        gst_buffer_unref(old);
    }
    GST_PAD_PROBE_INFO_DATA(info) = nb;
    info->size = gst_buffer_get_size(nb);
}
*/
import "C"

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// diagFlags is the buffer-flag set the stage tables report. Order is fixed so two
// machines' reports line up column for column.
var diagFlags = []struct {
	name string
	flag gogst.BufferFlags
}{
	{"DISCONT", gogst.BufferFlagDiscont},
	{"CORRUPTED", gogst.BufferFlagCorrupted},
	{"GAP", gogst.BufferFlagGap},
	{"MARKER", gogst.BufferFlagMarker},
	{"HEADER", gogst.BufferFlagHeader},
	{"DELTA_UNIT", gogst.BufferFlagDeltaUnit},
	{"RESYNC", gogst.BufferFlagResync},
}

// diagStage accumulates what crossed one pad.
type diagStage struct {
	name    string
	buffers uint64
	bytes   uint64
	flags   map[string]uint64
	events  map[string]uint64 // event-type name -> count (CAPS/SEGMENT/FLUSH/GAP/...)
	caps    string            // last caps seen crossing the pad
	nals    map[uint8]uint64  // non-nil only for the NAL-census stage
}

func newDiagStage(name string, censusNALs bool) *diagStage {
	s := &diagStage{name: name, flags: make(map[string]uint64), events: make(map[string]uint64)}
	if censusNALs {
		s.nals = make(map[uint8]uint64)
	}
	return s
}

// diag holds the whole run.
type diag struct {
	mu     sync.Mutex
	stages []*diagStage
	msgs   map[string]uint64 // bus message census: normalised text -> count
	msgEx  map[string]string // one verbatim example per normalised text
	errs   []string          // ERROR messages, verbatim, in order

	done     chan struct{} // closed on EOS, so a file replay stops when it ends
	doneOnce sync.Once

	// repaired counts the transport packets tsRepairProbe rewrote before
	// tsdemux (tsrepair.go); zero when WSLCOMMS_DIAGNOSE_TSREPAIR=0.
	repaired atomic.Uint64
}

// RunPipelineDiagnostic builds and runs the instrumented picture pipeline against
// uri for dur, then returns the report. decoderFactory picks the decoder;
// "avdec_h265" (software) is the comparable default. gogst.Init is idempotent, so
// this is safe whether or not the app already initialised GStreamer.
func RunPipelineDiagnostic(uri string, dur time.Duration, decoderFactory string) (string, error) {
	res, err := RunPipelineDiagnosticResult(uri, dur, decoderFactory, DiagnosticOpts{})
	if err != nil {
		return "", err
	}
	return res.Report, nil
}

// RunPipelineDiagnosticResult is RunPipelineDiagnostic with the numbers as
// well as the text, and with the return's SRT options, for the field rig
// (rig.go) which reasons over a live run and a file replay side by side.
func RunPipelineDiagnosticResult(uri string, dur time.Duration, decoderFactory string, opts DiagnosticOpts) (DiagnosticResult, error) {
	var none DiagnosticResult
	gogst.Init()
	if decoderFactory == "" {
		decoderFactory = "avdec_h265"
	}

	d := &diag{msgs: map[string]uint64{}, msgEx: map[string]string{}, done: make(chan struct{})}

	element := gogst.NewPipeline("wslcomms-diagnose")
	if element == nil {
		return none, fmt.Errorf("gst: diagnose: could not create the pipeline")
	}
	pipeline, ok := element.(gogst.Pipeline)
	if !ok {
		return none, fmt.Errorf("gst: diagnose: NewPipeline returned a %T, not a GstPipeline", element)
	}

	mk := func(factory, name string) (gogst.Element, error) {
		el := gogst.ElementFactoryMake(factory, name)
		if el == nil {
			return nil, fmt.Errorf("gst: diagnose: could not create %s", factory)
		}
		if !pipeline.Add(el) {
			return nil, fmt.Errorf("gst: diagnose: could not add %s", name)
		}
		return el, nil
	}

	// srt:// is a live SRT caller; anything else is a path to a captured .ts,
	// replayed through filesrc so a banked stream can be dissected offline.
	isFile := !strings.HasPrefix(uri, "srt://")
	srcFactory := "srtsrc"
	if isFile {
		srcFactory = "filesrc"
	}

	src, err := mk(srcFactory, "d-src")
	if err != nil {
		return none, err
	}
	demux, err := mk("tsdemux", "d-demux")
	if err != nil {
		return none, err
	}
	queue, err := mk("queue", "d-queue")
	if err != nil {
		return none, err
	}
	parse, err := mk("h265parse", "d-parse")
	if err != nil {
		return none, err
	}
	dec, err := mk(decoderFactory, "d-dec")
	if err != nil {
		return none, err
	}
	sink, err := mk("fakesink", "d-sink")
	if err != nil {
		return none, err
	}

	if isFile {
		if err := setStringProperty(src, "location", uri); err != nil {
			return none, fmt.Errorf("gst: diagnose: %w", err)
		}
		// Whole packets per buffer, as srtsrc delivers them: the repair probe
		// below works on a 188-byte grid, and filesrc's default block would
		// split packets across buffers.
		gogst.UtilSetObjectArg(src, "blocksize", strconv.Itoa(64*tsRepairPacketSize))
	} else {
		// srtsrc configured as the app configures it, so reception is comparable.
		if err := setStringProperty(src, "uri", uri); err != nil {
			return none, fmt.Errorf("gst: diagnose: %w", err)
		}
		if hasProperty(src, "mode") {
			gogst.UtilSetObjectArg(src, "mode", "caller")
		}
		latency := opts.LatencyMs
		if latency <= 0 {
			latency = 2000
		}
		if hasProperty(src, "latency") {
			src.SetObjectProperty("latency", int32(latency))
		}
		// The return's own encryption, when it has any: set the way the picture
		// path sets it — as properties, never in the URI, so the passphrase is
		// neither percent-encoded nor logged.
		if opts.PBKeyLen != 0 && opts.Passphrase != "" {
			if err := setStringProperty(src, "passphrase", opts.Passphrase); err != nil {
				return none, fmt.Errorf("gst: diagnose: %w", err)
			}
			if hasProperty(src, "pbkeylen") {
				gogst.UtilSetObjectArg(src, "pbkeylen", strconv.Itoa(opts.PBKeyLen))
			}
		}
		if hasProperty(src, "auto-reconnect") {
			src.SetObjectProperty("auto-reconnect", false)
		}
	}
	sink.SetObjectProperty("sync", false)
	sink.SetObjectProperty("async", false)

	// Static links: srtsrc -> tsdemux, and queue -> parse -> dec -> sink. The
	// demuxer -> queue link is dynamic (onPadAdded below).
	if !src.Link(demux) {
		return none, fmt.Errorf("gst: diagnose: could not link srtsrc to tsdemux")
	}
	if !queue.Link(parse) || !parse.Link(dec) || !dec.Link(sink) {
		return none, fmt.Errorf("gst: diagnose: could not link the video branch")
	}

	// Stage taps. queue:sink is what tsdemux hands the parser -- the NAL census
	// lives there. h265parse:src is the parsed access units into the decoder.
	// d-dec:src is the decoded frames; a CORRUPTED flag there is a damaged frame.
	sRaw := newDiagStage("1 srtsrc:src        (raw transport stream)", false)
	sDemux := newDiagStage("2 queue:sink       (demuxed video -> parser)", true)
	sParse := newDiagStage("3 h265parse:src    (parsed AU -> decoder)", false)
	sDec := newDiagStage("4 "+decoderFactory+":src (decoded frames)", false)
	d.stages = []*diagStage{sRaw, sDemux, sParse, sDec}

	tap := func(el gogst.Element, padName string, st *diagStage) error {
		pad := el.GetStaticPad(padName)
		if pad == nil {
			return fmt.Errorf("gst: diagnose: %s has no %s pad", el.GetName(), padName)
		}
		pad.AddProbe(gogst.PadProbeTypeBuffer, d.bufferProbe(st))
		// Events both ways: a mid-stream CAPS, SEGMENT, FLUSH or upstream
		// RECONFIGURE crossing the parser is exactly the kind of thing that could
		// reset it -- the open question of the tearing investigation.
		pad.AddProbe(gogst.PadProbeTypeEventBoth, d.eventProbe(st))
		return nil
	}
	if err := tap(src, "src", sRaw); err != nil {
		return none, err
	}
	if err := tap(queue, "sink", sDemux); err != nil {
		return none, err
	}
	if err := tap(parse, "src", sParse); err != nil {
		return none, err
	}
	if err := tap(dec, "src", sDec); err != nil {
		return none, err
	}

	// The transport-packet repair (tsrepair.go), exactly as the picture path
	// applies it, so this dissection measures what the application decodes.
	// WSLCOMMS_DIAGNOSE_TSREPAIR=0 leaves the packets as they came, for the A/B.
	switch strings.ToLower(os.Getenv("WSLCOMMS_DIAGNOSE_TSREPAIR")) {
	case "0", "off", "false", "no":
	default:
		if spad := src.GetStaticPad("src"); spad != nil {
			spad.AddProbe(gogst.PadProbeTypeBuffer, tsRepairProbe(&d.repaired))
		}
	}

	// Optional: exercise the shipping parameter re-injection surgery on real
	// buffers so WSLCOMMS_DIAGNOSE_REINJECT proves the cgo buffer-replace path
	// decodes cleanly. Installed on the queue's src pad, exactly like the app.
	if os.Getenv("WSLCOMMS_DIAGNOSE_REINJECT") != "" {
		if qsrc := queue.GetStaticPad("src"); qsrc != nil {
			qsrc.AddProbe(gogst.PadProbeTypeBuffer, diagReinjectProbe(&h265ParamCache{}))
		}
	}

	// Dynamic demuxer pads: video to the queue, everything else to its own
	// fakesink so mpegtsbase never sees a NOT_LINKED pad.
	var fakeSeq int
	var padMu sync.Mutex
	demux.ConnectPadAdded(func(_ gogst.Element, pad gogst.Pad) {
		caps := ""
		if c := pad.GetCurrentCaps(); c != nil {
			caps = c.String()
		}
		if strings.HasPrefix(caps, "video/") {
			if qsink := queue.GetStaticPad("sink"); qsink != nil {
				pad.Link(qsink)
			}
			return
		}
		padMu.Lock()
		fakeSeq++
		name := fmt.Sprintf("d-fake-%d", fakeSeq)
		padMu.Unlock()
		fake := gogst.ElementFactoryMake("fakesink", name)
		if fake == nil {
			return
		}
		fake.SetObjectProperty("sync", false)
		fake.SetObjectProperty("async", false)
		pipeline.Add(fake)
		fake.SyncStateWithParent()
		if fsink := fake.GetStaticPad("sink"); fsink != nil {
			pad.Link(fsink)
		}
	})

	// Bus draining: a sync handler records every message and drops it. This needs
	// no GMainLoop, and it sees WARNING/INFO/ELEMENT as well as ERROR.
	bus := pipeline.GetBus()
	if bus == nil {
		return none, fmt.Errorf("gst: diagnose: pipeline has no bus")
	}
	bus.SetSyncHandler(func(_ gogst.Bus, msg *gogst.Message) gogst.BusSyncReply {
		d.record(msg)
		return gogst.BusDrop
	})

	started := time.Now()
	if ret := pipeline.BlockSetState(gogst.StatePlaying, gogst.ClockTime(10*time.Second)); !stateChangeOK(ret) {
		return none, fmt.Errorf("gst: diagnose: pipeline would not reach PLAYING (%s)", ret)
	}

	// Run for dur, or until EOS (a file replay ending) or an error closes done.
	endedEarly := false
	select {
	case <-time.After(dur):
	case <-d.done:
		endedEarly = true
	}
	elapsed := time.Since(started)
	pipeline.BlockSetState(gogst.StateNull, gogst.ClockTime(5*time.Second))
	return d.result(uri, dur, elapsed, endedEarly, decoderFactory), nil
}

// DiagnosticOpts carries the return's SRT options into the dissector. The zero
// value is the historical behaviour: 2000 ms latency, no encryption.
type DiagnosticOpts struct {
	LatencyMs  int
	PBKeyLen   int
	Passphrase string
}

// DiagnosticStage is one tapped pad's totals.
type DiagnosticStage struct {
	Name    string
	Buffers uint64
	Bytes   uint64
	Flags   map[string]uint64 // buffer-flag name -> count (DISCONT, CORRUPTED, GAP, ...)
	Events  map[string]uint64
	Caps    string
}

// DiagnosticResult is the dissection as numbers and as the text report.
type DiagnosticResult struct {
	Report string

	// Requested is dur; Elapsed is how long the pipeline actually ran, which is
	// shorter when a file reached EOS or the pipeline failed (EndedEarly).
	Requested  time.Duration
	Elapsed    time.Duration
	EndedEarly bool

	// Stages in pipeline order: raw transport stream, demuxed video into the
	// parser, parsed access units into the decoder, decoded frames.
	Stages []DiagnosticStage

	// Bus is the bus-message census: normalised text -> count.
	Bus map[string]uint64

	// Errors are the bus ERROR messages, verbatim, at most twenty.
	Errors []string

	// Repaired is how many zero-payload transport packets were made compliant
	// before tsdemux (tsrepair.go).
	Repaired uint64
}

// Decoded is the decoded-frame count (the last stage), or 0.
func (r DiagnosticResult) Decoded() uint64 {
	if len(r.Stages) == 0 {
		return 0
	}
	return r.Stages[len(r.Stages)-1].Buffers
}

// Corrupted is the CORRUPTED flag count on the decoded frames, or 0.
func (r DiagnosticResult) Corrupted() uint64 {
	if len(r.Stages) == 0 {
		return 0
	}
	return r.Stages[len(r.Stages)-1].Flags["CORRUPTED"]
}

// BusMatching sums the census rows whose text contains sub, case-insensitively:
// "CONTINUITY" for tsdemux's mismatches, "broken/invalid" for h265parse's drops.
func (r DiagnosticResult) BusMatching(sub string) uint64 {
	sub = strings.ToLower(sub)
	var n uint64
	for k, v := range r.Bus {
		if strings.Contains(strings.ToLower(k), sub) {
			n += v
		}
	}
	return n
}

func (d *diag) result(uri string, dur, elapsed time.Duration, endedEarly bool, decoder string) DiagnosticResult {
	report := d.report(uri, dur, elapsed, endedEarly, decoder)
	d.mu.Lock()
	defer d.mu.Unlock()
	res := DiagnosticResult{
		Report:     report,
		Requested:  dur,
		Elapsed:    elapsed,
		EndedEarly: endedEarly,
		Bus:        make(map[string]uint64, len(d.msgs)),
		Errors:     append([]string(nil), d.errs...),
		Repaired:   d.repaired.Load(),
	}
	for k, v := range d.msgs {
		res.Bus[k] = v
	}
	for _, st := range d.stages {
		s := DiagnosticStage{Name: st.name, Buffers: st.buffers, Bytes: st.bytes, Caps: st.caps,
			Flags: make(map[string]uint64, len(st.flags)), Events: make(map[string]uint64, len(st.events))}
		for k, v := range st.flags {
			s.Flags[k] = v
		}
		for k, v := range st.events {
			s.Events[k] = v
		}
		res.Stages = append(res.Stages, s)
	}
	return res
}

// bufferProbe returns a per-buffer probe that tallies one stage.
func (d *diag) bufferProbe(st *diagStage) func(gogst.Pad, *gogst.PadProbeInfo) gogst.PadProbeReturn {
	return func(pad gogst.Pad, info *gogst.PadProbeInfo) gogst.PadProbeReturn {
		buf := info.GetBuffer()
		if buf == nil {
			return gogst.PadProbeOK
		}
		defer gogst.UnsafeBufferUnref(buf) // drop GetBuffer's ref + its finalizer

		size := uint64(buf.GetSize())

		d.mu.Lock()
		st.buffers++
		st.bytes += size
		for _, ff := range diagFlags {
			if buf.HasFlags(ff.flag) {
				st.flags[ff.name]++
			}
		}
		if st.caps == "" {
			if c := pad.GetCurrentCaps(); c != nil {
				st.caps = c.String()
			}
		}
		d.mu.Unlock()

		if st.nals != nil {
			if mi, ok := buf.Map(gogst.MapRead); ok {
				marks := scanNALs(mi.Data())
				mi.Unmap()
				d.mu.Lock()
				for _, m := range marks {
					st.nals[m.typ]++
				}
				d.mu.Unlock()
			}
		}
		return gogst.PadProbeOK
	}
}

// diagReinjectProbe is the harness's parameter re-injection probe, matching the
// app's reinjectProbe (picture_cgo.go) so a WSLCOMMS_DIAGNOSE_REINJECT run proves
// the same cache+rewrite+cgo-replace surgery decodes cleanly on real buffers.
func diagReinjectProbe(cache *h265ParamCache) func(gogst.Pad, *gogst.PadProbeInfo) gogst.PadProbeReturn {
	return func(_ gogst.Pad, info *gogst.PadProbeInfo) gogst.PadProbeReturn {
		buf := info.GetBuffer()
		if buf == nil {
			return gogst.PadProbeOK
		}
		defer gogst.UnsafeBufferUnref(buf)
		mi, ok := buf.Map(gogst.MapRead)
		if !ok {
			return gogst.PadProbeOK
		}
		out, changed := cache.rewrite(mi.Data())
		mi.Unmap()
		if !changed {
			return gogst.PadProbeOK
		}
		nb := gogst.NewBufferAllocate(nil, uint(len(out)), nil)
		if nb == nil {
			return gogst.PadProbeOK
		}
		wmi, ok := nb.Map(gogst.MapWrite)
		if !ok {
			gogst.UnsafeBufferUnref(nb)
			return gogst.PadProbeOK
		}
		copy(wmi.Data(), out)
		wmi.Unmap()
		C.wslcomms_diag_replace_buffer(
			C.gpointer(gogst.UnsafePadProbeInfoToGlibNone(info)),
			C.gpointer(gogst.UnsafeBufferToGlibFull(nb)),
		)
		return gogst.PadProbeOK
	}
}

// eventProbe returns a probe that tallies the event types crossing a pad. A
// mid-stream CAPS, FLUSH, SEGMENT or upstream RECONFIGURE is a candidate for the
// h265parse state reset the investigation cannot otherwise explain.
func (d *diag) eventProbe(st *diagStage) func(gogst.Pad, *gogst.PadProbeInfo) gogst.PadProbeReturn {
	return func(_ gogst.Pad, info *gogst.PadProbeInfo) gogst.PadProbeReturn {
		ev := info.GetEvent()
		if ev == nil {
			return gogst.PadProbeOK
		}
		name := ev.GetType().String()
		d.mu.Lock()
		st.events[name]++
		d.mu.Unlock()
		return gogst.PadProbeOK
	}
}

// record folds one bus message into the census.
func (d *diag) record(msg *gogst.Message) {
	if msg == nil {
		return
	}
	typ := msg.Type()
	if typ == gogst.MessageEOS {
		d.doneOnce.Do(func() { close(d.done) })
	}
	src := "?"
	if o := msg.Source(); o != nil {
		src = o.GetName()
	}
	var text string
	switch typ {
	case gogst.MessageError:
		if s, gerr := msg.ParseError(); gerr != nil {
			text = gerr.Error() + " | " + s
		}
	case gogst.MessageWarning:
		if s, gerr := msg.ParseWarning(); gerr != nil {
			text = gerr.Error()
			_ = s
		}
	case gogst.MessageInfo:
		if s, gerr := msg.ParseInfo(); gerr != nil {
			text = gerr.Error()
			_ = s
		}
	default:
		// Everything else is counted by type only; the detail rarely helps and
		// the volume (STATE_CHANGED, STREAM_STATUS) would bury the signal.
	}

	line := fmt.Sprintf("%s %s", diagMsgType(typ), src)
	if text != "" {
		line += ": " + text
	}
	key := normaliseMsg(line)

	d.mu.Lock()
	d.msgs[key]++
	if _, ok := d.msgEx[key]; !ok {
		d.msgEx[key] = line
	}
	if typ == gogst.MessageError && len(d.errs) < 20 {
		d.errs = append(d.errs, line)
	}
	d.mu.Unlock()
}

func (d *diag) report(uri string, dur, elapsed time.Duration, endedEarly bool, decoder string) string {
	d.mu.Lock()
	defer d.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "=== WSLComms pipeline dissection ===\n")
	fmt.Fprintf(&b, "target : %s\n", uri)
	how := "ran to the requested time"
	if endedEarly {
		how = "ended early: end of stream or a pipeline error"
	}
	fmt.Fprintf(&b, "ran    : %s of %s requested (%s)\n", elapsed.Round(time.Millisecond), dur.Round(time.Millisecond), how)
	fmt.Fprintf(&b, "decoder: %s\n", decoder)
	fmt.Fprintf(&b, "repair : zero-payload TS packets made compliant before tsdemux: %d (WSLCOMMS_DIAGNOSE_TSREPAIR=0 to leave them)\n\n",
		d.repaired.Load())

	fmt.Fprintf(&b, "--- per-stage buffer flow ---\n")
	for _, st := range d.stages {
		fmt.Fprintf(&b, "%s\n", st.name)
		fmt.Fprintf(&b, "    buffers %d, bytes %d%s\n", st.buffers, st.bytes, flagSummary(st.flags))
		if st.caps != "" {
			fmt.Fprintf(&b, "    caps  %s\n", trim(st.caps, 100))
		}
		if len(st.events) > 0 {
			fmt.Fprintf(&b, "    events %s\n", eventSummary(st.events))
		}
		if st.nals != nil {
			fmt.Fprintf(&b, "    NALs  %s\n", nalSummary(st.nals))
		}
	}

	fmt.Fprintf(&b, "\n--- bus messages (count x normalised) ---\n")
	keys := make([]string, 0, len(d.msgs))
	for k := range d.msgs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return d.msgs[keys[i]] > d.msgs[keys[j]] })
	for _, k := range keys {
		fmt.Fprintf(&b, "  %6d  %s\n", d.msgs[k], trim(d.msgEx[k], 140))
	}

	if len(d.errs) > 0 {
		fmt.Fprintf(&b, "\n--- ERRORS (verbatim) ---\n")
		for _, e := range d.errs {
			fmt.Fprintf(&b, "  %s\n", e)
		}
	}

	b.WriteString("\n--- how to read this ---\n")
	b.WriteString(diagHowToRead())
	return b.String()
}

func diagHowToRead() string {
	return "Compare the four stages top to bottom, then against a machine that does not\n" +
		"tear. Buffer counts should fall in step (many NALs -> one AU per picture ->\n" +
		"one frame per picture). A CORRUPTED count at the decoder is the picture\n" +
		"tearing, counted. A DISCONT/GAP that appears at stage 2 but not stage 1 is\n" +
		"the demuxer reacting to a transport hole. h265parse \"broken/invalid\" in the\n" +
		"bus census, with a stalled AU count at stage 3, is the parser dropping\n" +
		"slices. Whatever stage first diverges from the good machine is the fault.\n"
}

// --- small formatters ---

func flagSummary(flags map[string]uint64) string {
	if len(flags) == 0 {
		return ""
	}
	parts := make([]string, 0, len(diagFlags))
	for _, ff := range diagFlags {
		if n := flags[ff.name]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", ff.name, n))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "  [" + strings.Join(parts, " ") + "]"
}

func eventSummary(events map[string]uint64) string {
	names := make([]string, 0, len(events))
	for n := range events {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", n, events[n]))
	}
	return strings.Join(parts, " ")
}

func nalSummary(nals map[uint8]uint64) string {
	types := make([]int, 0, len(nals))
	for t := range nals {
		types = append(types, int(t))
	}
	sort.Ints(types)
	parts := make([]string, 0, len(types))
	for _, t := range types {
		parts = append(parts, fmt.Sprintf("%s=%d", diagNALName(uint8(t)), nals[uint8(t)]))
	}
	return strings.Join(parts, " ")
}

func diagNALName(t uint8) string {
	switch {
	case t == 32:
		return "VPS"
	case t == 33:
		return "SPS"
	case t == 34:
		return "PPS"
	case t == 35:
		return "AUD"
	case t == 39 || t == 40:
		return "SEI"
	case t == 19 || t == 20:
		return "IDR"
	case t == 21:
		return "CRA"
	case t <= 21:
		return "slice"
	default:
		return fmt.Sprintf("t%d", t)
	}
}

func diagMsgType(t gogst.MessageType) string {
	switch t {
	case gogst.MessageError:
		return "ERROR  "
	case gogst.MessageWarning:
		return "WARNING"
	case gogst.MessageInfo:
		return "INFO   "
	case gogst.MessageElement:
		return "ELEMENT"
	case gogst.MessageStateChanged:
		return "STATE  "
	case gogst.MessageEOS:
		return "EOS    "
	case gogst.MessageQos:
		return "QOS    "
	case gogst.MessageStreamStatus:
		return "STREAM "
	case gogst.MessageTag:
		return "TAG    "
	default:
		return fmt.Sprintf("MSG(0x%x)", uint32(t))
	}
}

// normaliseMsg collapses digits and hex to keep the census tight: a hundred
// "CONTINUITY: Mismatch packet 14, stream 12" lines become one row with a count.
func normaliseMsg(s string) string {
	var out strings.Builder
	prevDigit := false
	for _, r := range s {
		isHex := (r >= '0' && r <= '9')
		if isHex {
			if !prevDigit {
				out.WriteByte('#')
			}
			prevDigit = true
			continue
		}
		prevDigit = false
		out.WriteRune(r)
	}
	return out.String()
}

func trim(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
