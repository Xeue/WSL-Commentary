//go:build !cgo || gststub

// preview_stub.go is the pure-Go twin of preview_cgo.go. It compiles with
// CGO_ENABLED=0 and needs no MinGW, no GStreamer, no GPU and no window — that is
// Gate A, which is the machine this port is being done on.
//
// Read preview.go for what the confidence monitor is. This file exists so that
// the two functions preview.go cannot write in ordinary Go still have an answer
// in a build that has no GStreamer registry to ask.
//
// # It refuses rather than pretending, and that is the difference from the other stubs
//
// gst_stub.go, return_stub.go and picture_stub.go are all deliberately WORKING
// fakes: they invent plausible devices and let a whole session be driven, because
// what they stand in for is the thing the application is for. This one is not,
// and the difference is the point rather than laziness.
//
// A preview is a native surface with real video in it. There is nothing to fake:
// a Gate A build has no sink to name, and naming one anyway would put an element
// into a parse string that no registry here can be asked about — which on the
// real machine is the one failure mode this whole path is arranged to prevent,
// a missing element turning the CONTRIBUTION pipeline's parse into a hard error.
//
// So resolvePreviewSink says no, previewBranchFor therefore renders nothing, and
// a Gate A pipeline is exactly the pipeline it is today. Every decision that CAN
// be tested here still is: the branch string is a pure function of a factory
// name and preview_test.go calls it directly, the bus classification is ordinary
// Go, and the refusals in PreviewOpts.wanted are checked without either twin.
package gst

import "errors"

// resolvePreviewSink always refuses in this build.
//
// The sentence is written to read as the tail of previewBranchFor's log line,
// like the cgo twin's, and it names the BUILD rather than the machine: a
// developer running the application at Gate A and wondering where their preview
// went has not misconfigured anything, they have built without GStreamer.
func resolvePreviewSink() (string, error) {
	return "", errors.New("this build has no GStreamer at all (CGO_ENABLED=0, or the gststub tag), " +
		"so there is no video sink to render it with")
}

// THERE IS DELIBERATELY NO attachPreview HERE, and its absence is a decision
// rather than an omission.
//
// The seam preview.go needs is resolvePreviewSink alone, because preview.go
// carries no build tag and every build has to be able to compile it.
// attachPreview is different: its only caller is gst_cgo.go's Start, which
// carries the SAME build tag as preview_cgo.go, so in this build there is
// nothing that could call it and a stub would be a function that exists to be
// unreachable.
//
// Writing one anyway would cost something real. It would have to take the
// pipeline as `any`, because naming a go-gst type here would import a cgo
// package into the build whose whole purpose is not to need one — and a seam
// whose two halves have different signatures is a seam that stops being checked
// by the compiler. One function with one signature, in the one build that has a
// pipeline to attach anything to, is the honest shape.
