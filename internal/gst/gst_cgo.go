//go:build cgo

// This file is the real, go-gst backed implementation. It compiles only with
// CGO_ENABLED=1 and needs MinGW gcc, pkg-config and the GStreamer 1.28.5
// mingw-x86_64 DEVELOPMENT installer on the build host — that is Gate B.
//
// WP-0 leaves it as stubs. WP-3a fills it in. If go-gst v0.0.2 fights the MinGW
// build (open issue #179, no Windows CI), the fallback agreed in the
// specification is a roughly 200-line hand-written cgo shim over about fifteen C
// entry points, behind these same signatures — that is a change inside this
// file, not a change to the contract.

package gst

import (
	"errors"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// Init prepares the bundled GStreamer for use and calls gst_init.
//
// appDir is the directory holding wslcomms.exe. Before gst_init, Init sets:
//
//	GST_PLUGIN_SYSTEM_PATH_1_0 = ""                              (disables the default search)
//	GST_PLUGIN_PATH_1_0        = <appDir>\gst\lib\gstreamer-1.0
//	GST_REGISTRY_1_0           = %LOCALAPPDATA%\WSLComms\registry.bin
//
// Go's os.Setenv calls SetEnvironmentVariableW, which is what GLib reads, so
// this crosses the cgo boundary cleanly. The effect is that any GStreamer
// installed elsewhere on the machine is invisible to this process, and this
// process's bundle is invisible to it.
//
// Init must be called exactly once, before New or ListInputDevices. It is
// idempotent.
func Init(appDir string) error {
	return errors.New("not implemented: WP-3a")
}

// ListInputDevices returns the audio capture endpoints offered in the commentary
// input dropdown.
//
// It runs a GstDeviceMonitor filtered to Audio/Source on the wasapi2 provider,
// and for each device reports display-name as Device.Name and the IMMDevice
// endpoint ID as Device.ID.
func ListInputDevices() ([]Device, error) {
	return nil, errors.New("not implemented: WP-3a")
}

// New creates a Pipeline. Init must have been called first.
func New() (Pipeline, error) {
	return nil, errors.New("not implemented: WP-3a")
}

// cgoPipeline is the go-gst backed Pipeline.
//
// WP-3a: the fields below are a sketch of what the implementation needs, not a
// constraint on it. Named element handles matter because ReplaceSink has to find
// srtq and srtout again without re-parsing the description.
type cgoPipeline struct {
	// pipeline is the GstPipeline built by gst_parse_launch.
	pipeline gogst.Pipeline

	// errs carries GST_ELEMENT_ERROR bus messages to Errors.
	errs chan error
}

func (p *cgoPipeline) Start(opts PipelineOpts) error {
	return errors.New("not implemented: WP-3a")
}

func (p *cgoPipeline) ReplaceSink(opts SinkOpts) error {
	return errors.New("not implemented: WP-3a")
}

func (p *cgoPipeline) ForceKeyUnit() error {
	return errors.New("not implemented: WP-3a")
}

func (p *cgoPipeline) Errors() <-chan error {
	return p.errs
}

func (p *cgoPipeline) Stop() error {
	return errors.New("not implemented: WP-3a")
}

// Compile-time assertion that the real implementation satisfies the contract.
var _ Pipeline = (*cgoPipeline)(nil)
