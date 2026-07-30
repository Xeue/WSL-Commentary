//go:build production && (!cgo || gststub)

// This file exists to make one specific build combination fail, loudly, at
// compile time. It contains no working code and is never linked into anything
// that builds successfully.
//
// Owner: the coordinator. Added on the WP-8 integration report.
//
// # What it prevents
//
// internal/gst has two implementations chosen by the cgo build tag: gst_cgo.go,
// which links GStreamer and actually sends audio, and gst_stub.go, a pure-Go
// fake that returns invented devices and pretends to connect. The stub is what
// makes Gate A — a machine with no MinGW gcc and no GStreamer — able to build
// and test the entire application.
//
// That is exactly the danger. `production` is the tag the Wails CLI sets for a
// release build, and with CGO_ENABLED=0 the whole tree still compiles: it just
// silently selects the stub. The result is a wslcomms.exe that installs, opens,
// enumerates plausible-looking Dante devices, reports CONNECTED and lights the
// SENDING lamp green, and sends nothing at all. Every automated check passes.
// You would find out during a match.
//
// The build tags on main.go and app.go used to require cgo, which prevented
// this as a side effect — at the cost of excluding roughly a thousand lines of
// lifecycle, event-pump and shutdown-ordering code from every Gate A test run.
// Stating the real constraint here buys that coverage back without the risk:
// the requirement was never "cgo must be enabled", it was "a production binary
// must not contain the stub".
//
// # If you are reading this because your build just failed
//
// You built with `-tags production` and CGO_ENABLED=0. Set CGO_ENABLED=1 and
// make sure MinGW gcc and the GStreamer development installer are present — see
// build/README.md, and docs/project-plan.md for what Gate B is. `wails build`
// from a correctly set-up host does this for you.
package gst

func init() {
	// Deliberately undefined. The compiler error names the problem:
	//   undefined: aProductionBuildMustNotUseTheGStreamerStub_setCGO_ENABLED_1
	aProductionBuildMustNotUseTheGStreamerStub_setCGO_ENABLED_1()
}
