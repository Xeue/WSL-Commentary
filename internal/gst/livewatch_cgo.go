//go:build cgo && !gststub

// livewatch_cgo.go is the GStreamer half of the muxer watchdog: the
// BUFFER|BUFFER_LIST probes and the poller that reads them. livewatch.go carries
// the argument and every decision this file makes.
package gst

import (
	"sync"
	"time"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// The watchdog watches BOTH buffer types, and the second is mandatory:
// mpegtsmux at alignment=7 pushes buffer LISTS, and a BUFFER-only probe reads
// zero while megabytes flow. Two independent probes lost a run each to this.
//
// THEY ARE TWO PROBES AND NOT ONE PROBE WITH A COMBINED MASK, which is a
// measurement rather than a preference. gst_pad_probe_info_get_buffer is
// g_return_val_if_fail'd on the info's own type bit, and go-gst v0.0.2 exposes no
// accessor for that bit — so a single callback that tried GetBuffer and fell
// back to GetBufferList emitted
//
//	GStreamer-CRITICAL: gst_pad_probe_info_get_buffer:
//	assertion 'info->type & GST_PAD_PROBE_TYPE_BUFFER' failed
//
// once per buffer list. Measured on this machine on 2026-08-17: about 150 lines a
// second on stderr from one send session with a slate and a microphone, which
// buries the log a field engineer reads at exactly the moment they need it.
//
// Split, each callback calls only the getter its own mask guarantees, so the
// assertion cannot fire and neither callback has a branch in it. The cost is six
// probes on three pads instead of three.
const (
	liveWatchBufferMask = gogst.PadProbeTypeBuffer
	liveWatchListMask   = gogst.PadProbeTypeBufferList
)

// livePad is one watched pad's counter.
//
// It is mutex-guarded rather than atomic because two fields have to move
// together: a count and a timestamp read separately could report "0 buffers, last
// seen just now", which is the one reading that would make the verdict functions
// nonsense. The mutex is taken on a streaming thread for the length of two stores
// and is never held across anything that can block.
type livePad struct {
	name string

	mu      sync.Mutex
	buffers int64
	last    time.Time
}

func (p *livePad) bufferProbe(_ gogst.Pad, info *gogst.PadProbeInfo) gogst.PadProbeReturn {
	if info.GetBuffer() == nil {
		return gogst.PadProbeOK
	}
	p.count(1)
	return gogst.PadProbeOK
}

func (p *livePad) listProbe(_ gogst.Pad, info *gogst.PadProbeInfo) gogst.PadProbeReturn {
	list := info.GetBufferList()
	if list == nil {
		return gogst.PadProbeOK
	}
	// Counted by LENGTH. A list of seven counted as one understates the rate
	// sevenfold, which matters here only for the log line — but the log line is
	// what a field engineer compares against the measured healthy numbers in
	// livewatch.go, and a figure that is wrong by 7x is worse than none.
	p.count(int64(list.Length()))
	return gogst.PadProbeOK
}

func (p *livePad) count(n int64) {
	now := time.Now()
	p.mu.Lock()
	p.buffers += n
	p.last = now
	p.mu.Unlock()
}

func (p *livePad) sample() liveWatchSample {
	p.mu.Lock()
	defer p.mu.Unlock()
	return liveWatchSample{Pad: p.name, Buffers: p.buffers, Last: p.last}
}

// liveWatch is the whole watchdog: the pads, the probe ids for removal, and the
// poller goroutine.
type liveWatch struct {
	pads []*livePad

	// probes are the ids to remove at teardown, paired with the pad they are on.
	// They are removed BEFORE the pipeline goes to NULL, because a probe callback
	// firing against a struct whose pipeline is being disposed is a read on freed
	// memory.
	probes []liveWatchProbe

	// mu guards the two facts Stop has to know before it decides whether to wait
	// for anything: whether run has spawned the poller, and whether Stop has
	// already begun so that a later run does not spawn one.
	//
	// THE PROBES ARE ATTACHED BEFORE PLAYING AND THE POLLER IS STARTED AFTER IT,
	// and everything that can fail between those two moments — BlockSetState
	// refusing, a bus error latching first, the liveness gate refusing the START —
	// reaches teardown and calls Stop. An unconditional `<-w.done` there waits for
	// a goroutine that was never spawned: START never returns, the send pipeline
	// cannot be torn down, and shutdown never completes. Reproduced in this
	// package: Stop() on an attached-but-never-run watch did not return within
	// 1.5 s. signalWatch does not have this because it spawns its goroutine at
	// construction; this one splits attach from run and has to say so.
	mu       sync.Mutex
	started  bool
	stopping bool

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

type liveWatchProbe struct {
	pad gogst.Pad
	id  uint32
}

// attachLiveWatch installs the probes on vq:src, aq:src and mux:src.
//
// A missing element is an ERROR and not a shrug, unlike the missing chlevel
// armChannelMeterLocked survives: this is the only detector in the process for a
// feed that has gone silent while every lamp stays green, and a watchdog that
// silently watches two pads instead of three is worse than none because it is
// believed.
func attachLiveWatch(pipeline gogst.Pipeline) (*liveWatch, error) {
	w := &liveWatch{
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	for _, name := range []string{nameMuxVideoQueue, nameMuxAudioQueue, nameMuxOutput} {
		el := pipeline.GetByName(name)
		if el == nil {
			w.detach()
			return nil, errNoElement(name, "the muxer watchdog has no pad to watch")
		}
		pad := el.GetStaticPad("src")
		if pad == nil {
			w.detach()
			return nil, errNoPad(name, "src", "the muxer watchdog has no pad to watch")
		}
		p := &livePad{name: name}
		w.pads = append(w.pads, p)
		for _, probe := range []struct {
			mask gogst.PadProbeType
			fn   func(gogst.Pad, *gogst.PadProbeInfo) gogst.PadProbeReturn
		}{
			{liveWatchBufferMask, p.bufferProbe},
			{liveWatchListMask, p.listProbe},
		} {
			id := pad.AddProbe(probe.mask, probe.fn)
			if id == 0 {
				w.detach()
				return nil, errProbeFailed(name, "src", "the muxer watchdog")
			}
			w.probes = append(w.probes, liveWatchProbe{pad: pad, id: id})
		}
	}
	return w, nil
}

// samples reads every watched pad. Order is vq, aq, mux — the order the probes
// were installed and the order every message names them in.
func (w *liveWatch) samples() []liveWatchSample {
	if w == nil {
		return nil
	}
	out := make([]liveWatchSample, 0, len(w.pads))
	for _, p := range w.pads {
		out = append(out, p.sample())
	}
	return out
}

// run starts the poller. playingAt is when the pipeline reached PLAYING, and is
// what a pad that has never seen a buffer is measured from. fatal is called at
// most once, on the poller's goroutine.
//
// # fatal MAY call Stop, and that is the expected implementation
//
// The handler's whole job is to take a dead feed off air, which means tearing the
// send pipeline down, which means Stop. So the poller's completion is published
// BEFORE fatal is invoked and not with a defer after it: a `defer close(w.done)`
// makes Stop wait for the fatal handler while the fatal handler waits for Stop,
// and the process wedges on air with the feed already dead. Reproduced in this
// package: with the close deferred, `w.run(..., func(error){ w.Stop() })` never
// returned.
//
// Calling run twice, or calling it after Stop, is a no-op rather than a second
// goroutine or a double close.
func (w *liveWatch) run(playingAt time.Time, fatal func(error)) {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.started || w.stopping {
		w.mu.Unlock()
		return
	}
	w.started = true
	w.mu.Unlock()

	go func() {
		err := w.poll(playingAt)
		close(w.done)
		if err != nil {
			fatal(err)
		}
	}()
}

// poll is the ticker loop, returning the verdict that ended it or nil when Stop
// ended it. It is separate from run's goroutine so that "the poller has finished"
// and "the fatal handler has finished" are two different moments; see run.
func (w *liveWatch) poll(playingAt time.Time) error {
	t := time.NewTicker(liveWatchPollInterval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return nil
		case now := <-t.C:
			if err := liveWatchSilenceVerdict(w.samples(), playingAt, now,
				liveWatchSilence); err != nil {
				return err
			}
		}
	}
}

// Stop stops the poller and JOINS it, then removes the probes. Both halves
// matter and in this order: the poller reads the pads, the probes write them, and
// a teardown that removed the probes first would leave the poller reading a
// counter nothing can update while the pipeline it is about to indict is already
// on its way to NULL.
//
// IT JOINS ONLY WHAT WAS ACTUALLY STARTED. A watch that was attached and never
// run — every failure between attachLiveWatch and PLAYING — has no goroutine to
// wait for, and waiting for one anyway is the deadlock described on the struct.
func (w *liveWatch) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() {
		w.mu.Lock()
		started := w.started
		w.stopping = true
		w.mu.Unlock()

		close(w.stop)
		if started {
			<-w.done
		}
		w.detach()
	})
}

// detach removes every probe installed so far. It is called from Stop and from
// attachLiveWatch's own failure paths, which is why it tolerates a partially
// built watch.
func (w *liveWatch) detach() {
	for _, p := range w.probes {
		if p.pad != nil && p.id != 0 {
			p.pad.RemoveProbe(p.id)
		}
	}
	w.probes = nil
}
