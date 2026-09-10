//go:build cgo && !gststub

package gst

/*
#cgo pkg-config: gstreamer-1.0
#include <gst/gst.h>

// wslcomms_queue_level_buffers reads a GstQueue's current-level-buffers: how
// many buffers it holds right now. One g_object_get on a guint, which the
// element serves from its own lock; safe from the streaming thread.
static guint wslcomms_queue_level_buffers(gpointer element) {
    guint n = 0;
    g_object_get(G_OBJECT(element), "current-level-buffers", &n, NULL);
    return n;
}

// wslcomms_probe_mark_discont flags the buffer a BUFFER pad probe carries as a
// discontinuity — the honest description of the first access unit after a
// catch-up skipped some — making it writable first (in place when the pad's
// reference is the only one) and putting it back into the probe.
static void wslcomms_probe_mark_discont(gpointer info_ptr) {
    GstPadProbeInfo *info = (GstPadProbeInfo *)info_ptr;
    GstBuffer *b = GST_PAD_PROBE_INFO_BUFFER(info);
    if (!b) return;
    b = gst_buffer_make_writable(b);
    GST_BUFFER_FLAG_SET(b, GST_BUFFER_FLAG_DISCONT);
    GST_PAD_PROBE_INFO_DATA(info) = b;
}
*/
import "C"

import (
	"errors"
	"log"
	"os"
	"strconv"
	"strings"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

const (
	// catchUpEnv disables the catch-up (0 | off | false | no), for an A/B.
	catchUpEnv = "WSLCOMMS_PIC_CATCHUP"

	// catchUpHighEnv overrides the high-water mark, in access units.
	catchUpHighEnv = "WSLCOMMS_PIC_CATCHUP_AU"
)

// installCatchUp puts the catch-up probe (catchup.go) on picq's src pad. It
// must be installed BEFORE the parameter re-injection probe on the same pad:
// probes run in the order they were added, and an access unit that is being
// dropped should not first be rewritten.
func (p *picturePipeline) installCatchUp() error {
	switch strings.ToLower(os.Getenv(catchUpEnv)) {
	case "0", "off", "false", "no":
		log.Printf("gst: picture monitor: catch-up is OFF (%s); a slow decoder will fall behind", catchUpEnv)
		return nil
	}
	high := catchUpHighWater
	if v := os.Getenv(catchUpHighEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			high = n
		}
	}
	low := catchUpLowWater
	if low >= high {
		low = high - 1
	}

	ptr := gogst.UnsafeElementToGlibNone(p.queue)
	if ptr == nil {
		return errors.New("gst: picture monitor: " + namePicQueue + " has no native handle for the catch-up")
	}
	pad := p.queue.GetStaticPad("src")
	if pad == nil {
		return errors.New("gst: picture monitor: " + namePicQueue + " has no src pad for the catch-up")
	}
	p.catchUp = newCatchUp(high, low)
	p.catchUpQueue = ptr
	id := pad.AddProbe(gogst.PadProbeTypeBuffer, p.catchUpProbe)
	if id == 0 {
		return errors.New("gst: picture monitor: could not add the catch-up probe to " + namePicQueue)
	}
	p.catchUpPad, p.catchUpProbeID = pad, id
	log.Printf("gst: picture monitor: catch-up is on (%s:src): at %d queued access units drop to the next keyframe, resume at %d or fewer",
		namePicQueue, high, low)
	return nil
}

// catchUpProbe runs on the queue's streaming thread, once per access unit
// leaving picq for h265parse. It reads the queue's fill, asks catchUp what to
// do, and drops, passes, or passes with DISCONT. The keyframe question maps the
// buffer only while an episode is on; a machine that keeps up never maps.
func (p *picturePipeline) catchUpProbe(_ gogst.Pad, info *gogst.PadProbeInfo) gogst.PadProbeReturn {
	c := p.catchUp
	if c == nil || p.catchUpQueue == nil {
		return gogst.PadProbeOK
	}
	backlog := int(C.wslcomms_queue_level_buffers(C.gpointer(p.catchUpQueue)))
	v := c.decide(backlog, func() bool {
		buf := info.GetBuffer()
		if buf == nil {
			return false
		}
		defer gogst.UnsafeBufferUnref(buf)
		mi, ok := buf.Map(gogst.MapRead)
		if !ok {
			return false
		}
		irap := hasIRAP(mi.Data())
		mi.Unmap()
		return irap
	})
	if v.Began {
		dropped, episodes := c.Totals()
		p.catchUpDropped.Store(dropped)
		p.catchUpEpisodes.Store(episodes)
		log.Printf("gst: picture monitor: the decoder is %d access units behind; dropping to the next keyframe rather than fall behind",
			v.Backlog)
	}
	if v.Resumed {
		dropped, episodes := c.Totals()
		p.catchUpDropped.Store(dropped)
		p.catchUpEpisodes.Store(episodes)
		log.Printf("gst: picture monitor: caught up: skipped %d access units to a keyframe (%d queued now; %d skipped in %d episodes so far)",
			v.Skipped, v.Backlog, dropped, episodes)
		C.wslcomms_probe_mark_discont(C.gpointer(gogst.UnsafePadProbeInfoToGlibNone(info)))
		return gogst.PadProbeOK
	}
	if v.Drop {
		return gogst.PadProbeDrop
	}
	return gogst.PadProbeOK
}
