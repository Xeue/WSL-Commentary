//go:build sendlive && cgo && !gststub

package gst

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// TestSendLiveCatchUpDropsToKeyframesBehindASlowConsumer runs the catch-up
// probe (catchup.go, catchup_cgo.go) on the real GStreamer, on a real
// transport stream, behind a consumer deliberately slower than the source:
//
//	filesrc ! tsdemux ! queue name=picq [catch-up probe] ! fakesink [throttle]
//
// filesrc reads as fast as it can, the fakesink is held for a few
// milliseconds per access unit, so the queue fills past the high-water mark
// within a second and the probe must drop — and every access unit that
// reaches the sink after a drop must be a keyframe flagged DISCONT, with the
// totals the probe reports matching what the sink saw.
//
// Run as twooutputs_live_test.go says (compile with the toolchain PATH, run
// with build\bin first on PATH). The stream is COMM-01's capture; set
// WSLCOMMS_SENDLIVE_TS to another .ts.
func TestSendLiveCatchUpDropsToKeyframesBehindASlowConsumer(t *testing.T) {
	sendLiveInit(t)
	ts := os.Getenv("WSLCOMMS_SENDLIVE_TS")
	if ts == "" {
		ts = filepath.Join(os.Getenv("LOCALAPPDATA"), "Temp", "claude",
			"c--Users-samsw-GitProjects-M2LX-Commentary", "2609fb7f-8b67-46ba-a5de-35b19c362845",
			"scratchpad", "comm01", "WSLComms-rig-20260910-143536", "cap.ts")
	}
	if _, err := os.Stat(ts); err != nil {
		t.Skipf("no transport stream to replay (WSLCOMMS_SENDLIVE_TS): %v", err)
	}

	el := gogst.NewPipeline("catchup-live")
	pipeline, ok := el.(gogst.Pipeline)
	if !ok {
		t.Fatalf("NewPipeline returned a %T", el)
	}
	src := gogst.ElementFactoryMake("filesrc", "src")
	demux := gogst.ElementFactoryMake("tsdemux", "demux")
	queue := gogst.ElementFactoryMake("queue", namePicQueue)
	sink := gogst.ElementFactoryMake("fakesink", "sink")
	if src == nil || demux == nil || queue == nil || sink == nil {
		t.Fatal("could not make filesrc/tsdemux/queue/fakesink")
	}
	if err := setStringProperty(src, "location", ts); err != nil {
		t.Fatal(err)
	}
	gogst.UtilSetObjectArg(src, "blocksize", "12032")
	gogst.UtilSetObjectArg(sink, "sync", "false")
	gogst.UtilSetObjectArg(sink, "async", "false")
	if !pipeline.Add(src) || !pipeline.Add(demux) || !pipeline.Add(queue) || !pipeline.Add(sink) {
		t.Fatal("could not add the elements")
	}
	if !src.Link(demux) || !queue.Link(sink) {
		t.Fatal("could not link")
	}
	demux.ConnectPadAdded(func(_ gogst.Element, pad gogst.Pad) {
		caps := ""
		if c := pad.GetCurrentCaps(); c != nil {
			caps = c.String()
		}
		if len(caps) >= 6 && caps[:6] == "video/" {
			if qsink := queue.GetStaticPad("sink"); qsink != nil {
				pad.Link(qsink)
			}
			return
		}
		// Audio to nowhere.
		fake := gogst.ElementFactoryMake("fakesink", "afake")
		if fake == nil {
			return
		}
		gogst.UtilSetObjectArg(fake, "sync", "false")
		gogst.UtilSetObjectArg(fake, "async", "false")
		pipeline.Add(fake)
		fake.SyncStateWithParent()
		if fsink := fake.GetStaticPad("sink"); fsink != nil {
			pad.Link(fsink)
		}
	})

	// The probe under test, on a picturePipeline that has nothing else.
	p := &picturePipeline{queue: queue}
	t.Setenv(catchUpHighEnv, "")
	t.Setenv(catchUpEnv, "")
	if err := p.installCatchUp(); err != nil {
		t.Fatalf("installCatchUp: %v", err)
	}

	// The slow consumer, and the audit of what it received.
	type seen struct {
		discont bool
		irap    bool
	}
	var mu sync.Mutex
	var received []seen
	spad := sink.GetStaticPad("sink")
	if spad == nil {
		t.Fatal("fakesink has no sink pad")
	}
	spad.AddProbe(gogst.PadProbeTypeBuffer, func(_ gogst.Pad, info *gogst.PadProbeInfo) gogst.PadProbeReturn {
		buf := info.GetBuffer()
		if buf == nil {
			return gogst.PadProbeOK
		}
		defer gogst.UnsafeBufferUnref(buf)
		s := seen{discont: buf.HasFlags(gogst.BufferFlagDiscont)}
		if mi, ok := buf.Map(gogst.MapRead); ok {
			s.irap = hasIRAP(mi.Data())
			mi.Unmap()
		}
		mu.Lock()
		received = append(received, s)
		mu.Unlock()
		time.Sleep(4 * time.Millisecond) // 250 fps at most, against a source with no clock
		return gogst.PadProbeOK
	})

	bus := pipeline.GetBus()
	if bus == nil {
		t.Fatal("no bus")
	}
	done := make(chan struct{})
	var doneOnce sync.Once
	var busErr string
	bus.SetSyncHandler(func(_ gogst.Bus, msg *gogst.Message) gogst.BusSyncReply {
		switch msg.Type() {
		case gogst.MessageEOS:
			doneOnce.Do(func() { close(done) })
		case gogst.MessageError:
			_, gerr := msg.ParseError()
			mu.Lock()
			busErr = gerr.Error()
			mu.Unlock()
			doneOnce.Do(func() { close(done) })
		}
		return gogst.BusDrop
	})
	if ret := pipeline.BlockSetState(gogst.StatePlaying, gogst.ClockTime(10*time.Second)); !stateChangeOK(ret) {
		t.Fatalf("PLAYING: %s", ret)
	}
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("the replay did not end in 90 s")
	}
	pipeline.BlockSetState(gogst.StateNull, gogst.ClockTime(5*time.Second))

	mu.Lock()
	defer mu.Unlock()
	if busErr != "" {
		t.Fatalf("bus error: %s", busErr)
	}
	dropped, episodes := p.catchUp.Totals()
	t.Logf("catch-up: %d access units skipped in %d episodes; the sink received %d", dropped, episodes, len(received))
	if episodes == 0 || dropped == 0 {
		t.Fatalf("a consumer at 250 fps behind a source with no clock never filled the queue past %d: episodes %d, dropped %d",
			catchUpHighWater, episodes, dropped)
	}
	if len(received) == 0 {
		t.Fatal("the sink received nothing")
	}
	// Every discontinuity the sink saw after the first buffer is a resume, and a
	// resume is a keyframe. Their number is the number of episodes.
	resumes := 0
	for i, s := range received[1:] {
		if s.discont {
			resumes++
			if !s.irap {
				t.Errorf("buffer %d resumed after a skip on a NON-keyframe; the decoder would show garbage", i+1)
			}
		}
	}
	if uint64(resumes) != episodes {
		t.Errorf("the sink saw %d DISCONT resumes, the probe counted %d episodes", resumes, episodes)
	}
	if p.catchUpDropped.Load() != dropped || p.catchUpEpisodes.Load() != episodes {
		t.Errorf("the stats-line atomics (%d/%d) disagree with the totals (%d/%d)",
			p.catchUpDropped.Load(), p.catchUpEpisodes.Load(), dropped, episodes)
	}
}
