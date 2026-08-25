//go:build !cgo || gststub

package gst

import (
	"errors"
	"time"
)

// RunPipelineDiagnostic is unavailable in the stub build: the dissector drives a
// real GStreamer pipeline and there is none here. It matches the cgo signature so
// callers compile at Gate A.
func RunPipelineDiagnostic(uri string, dur time.Duration, decoderFactory string) (string, error) {
	_ = dur
	return "", errors.New("gst: diagnose: the pipeline dissector needs the real cgo GStreamer build (" + uri + ")")
}
