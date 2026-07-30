//go:build !cgo

package gst

import (
	"errors"
	"testing"
)

func TestListInputDevicesReturnsFakes(t *testing.T) {
	SetStubDevices(nil)
	devices, err := ListInputDevices()
	if err != nil {
		t.Fatalf("ListInputDevices: %v", err)
	}
	if len(devices) != 3 {
		t.Fatalf("want 3 fake devices, got %d", len(devices))
	}
	if devices[0].Name != "DVS Receive  1-2 (Dante Virtual Soundcard)" {
		t.Errorf("first device name = %q", devices[0].Name)
	}
	if devices[0].ID == devices[0].Name {
		t.Error("device ID must be the endpoint GUID, not the display name")
	}
}

func TestListInputDevicesIsACopy(t *testing.T) {
	SetStubDevices(nil)
	devices, _ := ListInputDevices()
	devices[0].Name = "mutated"
	again, _ := ListInputDevices()
	if again[0].Name == "mutated" {
		t.Error("ListInputDevices leaked its backing array")
	}
}

func TestStartRequiresSlateAndDevice(t *testing.T) {
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{AudioDeviceID: "{guid}"}); err == nil {
		t.Error("want error for empty SlatePath")
	}
	if err := p.Start(PipelineOpts{SlatePath: "slate.png"}); err == nil {
		t.Error("want error for empty AudioDeviceID")
	}
	if p.State() != StubStateStopped {
		t.Errorf("state after failed Start = %q, want %q", p.State(), StubStateStopped)
	}
}

func TestStartInstallsNoSink(t *testing.T) {
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: "{guid}"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p.State() != StubStateRunning {
		t.Errorf("state = %q, want %q", p.State(), StubStateRunning)
	}
	if _, ok := p.AttachedSink(); ok {
		t.Error("Start must not install a sink")
	}
	if got := p.StartedWith().VideoBitrateKbps; got != DefaultVideoBitrateKbps {
		t.Errorf("VideoBitrateKbps = %d, want default %d", got, DefaultVideoBitrateKbps)
	}
	if got := p.StartedWith().AudioBitrateBps; got != DefaultAudioBitrateBps {
		t.Errorf("AudioBitrateBps = %d, want default %d", got, DefaultAudioBitrateBps)
	}
}

func TestReplaceSinkFailureLadder(t *testing.T) {
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: "{guid}"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	boom := errors.New("connection refused")
	p.FailNextSinks(2, boom)

	sink := SinkOpts{Host: "m2lx.example", Port: 9001}
	for i := range 2 {
		if err := p.ReplaceSink(sink); !errors.Is(err, boom) {
			t.Fatalf("attempt %d: err = %v, want %v", i, err, boom)
		}
		if p.State() != StubStateRunning {
			t.Fatalf("a failed ReplaceSink must leave the pipeline running, got %q", p.State())
		}
	}

	if err := p.ReplaceSink(sink); err != nil {
		t.Fatalf("third attempt should succeed, got %v", err)
	}
	if p.State() != StubStateSinkAttached {
		t.Errorf("state = %q, want %q", p.State(), StubStateSinkAttached)
	}
	got, ok := p.AttachedSink()
	if !ok {
		t.Fatal("no sink attached after a successful ReplaceSink")
	}
	if got.LatencyMs != DefaultSRTLatencyMs {
		t.Errorf("LatencyMs = %d, want default %d", got.LatencyMs, DefaultSRTLatencyMs)
	}

	c := p.Counters()
	if c.ReplaceSinks != 3 || c.SinksAttached != 1 {
		t.Errorf("counters = %+v, want 3 attempts and 1 success", c)
	}
}

func TestInjectErrorReachesErrorsChannel(t *testing.T) {
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: "{guid}"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	boom := errors.New("srtout: peer gone")
	if !p.InjectError(boom) {
		t.Fatal("InjectError reported a drop on an empty buffer")
	}
	select {
	case got := <-p.Errors():
		if !errors.Is(got, boom) {
			t.Errorf("got %v, want %v", got, boom)
		}
	default:
		t.Fatal("nothing on the error channel")
	}
}

func TestInjectErrorDropsRatherThanBlocks(t *testing.T) {
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: "{guid}"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range stubErrorBuffer {
		if !p.InjectError(errors.New("fill")) {
			t.Fatal("dropped before the buffer was full")
		}
	}
	if p.InjectError(errors.New("overflow")) {
		t.Error("want a drop once the buffer is full")
	}
}

func TestStopClosesErrorsAndIsIdempotent(t *testing.T) {
	p := NewStubPipeline()
	if err := p.Start(PipelineOpts{SlatePath: "slate.png", AudioDeviceID: "{guid}"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if _, open := <-p.Errors(); open {
		t.Error("Errors must be closed by Stop")
	}
	if p.State() != StubStateStopped {
		t.Errorf("state = %q, want %q", p.State(), StubStateStopped)
	}
	if err := p.ReplaceSink(SinkOpts{Host: "h", Port: 1}); err == nil {
		t.Error("ReplaceSink after Stop must fail")
	}
	if err := p.ForceKeyUnit(); err == nil {
		t.Error("ForceKeyUnit after Stop must fail")
	}
}

func TestNewReturnsAStubPipeline(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := p.(*StubPipeline); !ok {
		t.Fatalf("New returned %T, want *StubPipeline", p)
	}
}

func TestInitIsANoOp(t *testing.T) {
	if err := Init(`C:\Program Files\WSLComms`); err != nil {
		t.Fatalf("Init: %v", err)
	}
	dir, inited := StubAppDir()
	if !inited || dir != `C:\Program Files\WSLComms` {
		t.Errorf("StubAppDir() = %q, %v", dir, inited)
	}
}
