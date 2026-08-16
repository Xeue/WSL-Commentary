//go:build live && cgo && !gststub

// coughmute_probe_live_test.go isolates ONE question, away from the card and
// away from this package's pipeline: does writing `mute` on a volume element
// that is ALREADY PLAYING actually silence the stream?
//
// It exists because the answer measured on the shipped pipeline disagreed with
// the answer measured with gst-launch, and a disagreement like that has to be
// resolved by a third measurement rather than by an argument.

package gst

import (
	"fmt"
	"os"
	"testing"
	"time"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// drainBus throws away whatever is queued, so that a window measured after a
// property write contains no message posted before it.
func drainBus(bus gogst.Bus, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if bus.TimedPopFiltered(gogst.ClockTime(10*time.Millisecond), gogst.MessageElement) == nil {
			return
		}
	}
}

// probeLevels runs one synthetic pipeline and reports the mean RMS of the level
// messages seen before and after `mute` is written live.
func probeLevels(t *testing.T, desc string) (before, after float64, beforeN, afterN int) {
	t.Helper()

	el, err := gogst.ParseLaunch(desc)
	if err != nil {
		t.Fatalf("ParseLaunch(%s): %v", desc, err)
	}
	pipeline, ok := el.(gogst.Pipeline)
	if !ok {
		t.Fatalf("not a pipeline: %T", el)
	}
	defer pipeline.BlockSetState(gogst.StateNull, gogst.ClockTime(3*time.Second))

	bus := pipeline.GetBus()
	if ret := pipeline.BlockSetState(gogst.StatePlaying, gogst.ClockTime(5*time.Second)); ret == gogst.StateChangeFailure {
		t.Fatalf("PLAYING failed for %s", desc)
	}

	collect := func(d time.Duration) (float64, int) {
		deadline := time.Now().Add(d)
		sum, n := 0.0, 0
		for time.Now().Before(deadline) {
			msg := bus.TimedPopFiltered(gogst.ClockTime(100*time.Millisecond), gogst.MessageElement)
			if msg == nil {
				continue
			}
			s := msg.GetStructure()
			if s == nil || s.GetName() != levelStructureName {
				continue
			}
			if l, ok := levelsFromStructure(s); ok && len(l.RMSDB) > 0 {
				sum += l.RMSDB[0]
				n++
			}
		}
		if n == 0 {
			return -999, 0
		}
		return sum / float64(n), n
	}

	before, beforeN = collect(800 * time.Millisecond)

	vol := pipeline.GetByName("v")
	if vol == nil {
		t.Fatal("no element named v")
	}
	vol.SetObjectProperty("mute", true)

	// SETTLE BEFORE MEASURING, and the reason is the measurement error this
	// whole file was written to resolve. The level element reports over a 50 ms
	// window; the window the mute lands inside contains audio from BOTH sides of
	// the write, so it reads as loud as the unmuted stream and drags any average
	// or any peak-hold with it. Two frames of settle is not the mute being slow
	// — the write itself is tens of microseconds — it is one measurement window
	// being discarded because it describes two different states.
	time.Sleep(150 * time.Millisecond)
	drainBus(bus, 100*time.Millisecond)

	after, afterN = collect(800 * time.Millisecond)
	return before, after, beforeN, afterN
}

// TestLiveVolumeMuteWrittenOnAPlayingPipeline is the isolation.
//
// Two pipelines, identical but for ONE property in the parse string: the first
// builds the volume element at unity, the second a hair below it. If the two
// behave differently, the difference is GstBaseTransform's PASSTHROUGH — an
// element built at unity with mute=false is a passthrough, and a passthrough
// does not call the transform that would zero the samples.
func TestLiveVolumeMuteWrittenOnAPlayingPipeline(t *testing.T) {
	liveDeckLinkInit(t)

	const src = "audiotestsrc wave=sine freq=1000 volume=0.355 is-live=true ! " +
		"audio/x-raw,format=S16LE,rate=48000,channels=2,layout=interleaved ! "
	const tail = " ! level name=l interval=50000000 ! fakesink sync=false"

	for _, c := range []struct{ name, desc string }{
		{"volume built at unity", src + "volume name=v mute=false" + tail},
		{"volume built at 0.999", src + "volume name=v mute=false volume=0.999" + tail},
	} {
		before, after, bn, an := probeLevels(t, c.desc)
		fmt.Fprintf(os.Stderr, "  %-24s before %9.4f dBFS (%d frames)   after mute=true %9.4f dBFS (%d frames)\n",
			c.name, before, bn, after, an)
		if an == 0 {
			t.Errorf("%s: no level messages after the mute", c.name)
			continue
		}
		if after > -99 {
			t.Errorf("%s: writing mute=true on a PLAYING pipeline left the stream at %.4f dBFS. "+
				"The cough mute does not work this way and the mechanism has to change", c.name, after)
		}
	}
}
