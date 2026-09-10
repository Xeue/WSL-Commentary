//go:build !cgo || gststub

package gst

import (
	"errors"
	"strings"
	"time"
)

// RunPipelineDiagnostic is unavailable in the stub build: the dissector drives a
// real GStreamer pipeline and there is none here. It matches the cgo signature so
// callers compile at Gate A.
func RunPipelineDiagnostic(uri string, dur time.Duration, decoderFactory string) (string, error) {
	_ = dur
	return "", errors.New("gst: diagnose: the pipeline dissector needs the real cgo GStreamer build (" + uri + ")")
}

// DiagnosticOpts matches the cgo build's; see diagnose_cgo.go.
type DiagnosticOpts struct {
	LatencyMs  int
	PBKeyLen   int
	Passphrase string
}

// DiagnosticStage matches the cgo build's; see diagnose_cgo.go.
type DiagnosticStage struct {
	Name    string
	Buffers uint64
	Bytes   uint64
	Flags   map[string]uint64
	Events  map[string]uint64
	Caps    string
}

// DiagnosticResult matches the cgo build's; see diagnose_cgo.go.
type DiagnosticResult struct {
	Report     string
	Requested  time.Duration
	Elapsed    time.Duration
	EndedEarly bool
	Stages     []DiagnosticStage
	Bus        map[string]uint64
	Errors     []string
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

// BusMatching sums the census rows whose text contains sub, case-insensitively.
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

// RunPipelineDiagnosticResult is unavailable in the stub build, like
// RunPipelineDiagnostic.
func RunPipelineDiagnosticResult(uri string, dur time.Duration, decoderFactory string, opts DiagnosticOpts) (DiagnosticResult, error) {
	_, _, _ = dur, decoderFactory, opts
	return DiagnosticResult{}, errors.New("gst: diagnose: the pipeline dissector needs the real cgo GStreamer build (" + uri + ")")
}
