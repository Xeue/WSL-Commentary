//go:build (dev || production) && windows

// NOT BUILT INTO THE BINDINGS BINARY. `wails build` compiles a second binary
// with `-tags bindings`, runs it to dump the bound-method metadata and reads
// its exit status, and a build tool has no business ending through a hard exit:
// it never creates an srtsink and never loads the driver DLLs whose detach
// handlers this file exists to skip.
//
// It never showed a symptom here — TerminateProcess(self, 0) reports a clean
// exit status 0 — but the darwin twin could not hide it, and the exclusion is
// correct on both. See exit_darwin.go for the failure it caused there.

// The one exit this application is allowed to take that Windows cannot refuse.
//
// Owner: WP-8, with app.go and main.go.
//
// # Why TerminateProcess and not os.Exit
//
// They are not the same call and the difference only shows up in the one case
// this file exists for. os.Exit is ExitProcess, which:
//
//  1. terminates every thread in the process except the calling one, wherever
//     they happen to be;
//  2. takes the loader lock and calls DllMain(DLL_PROCESS_DETACH) for every
//     loaded DLL;
//  3. then ends the process.
//
// Step 2 runs OVER the wreckage of step 1. If a thread was killed while holding
// a lock that a detach handler goes on to take, ExitProcess deadlocks and the
// process never dies. That is a documented Windows hazard and it is not
// hypothetical here: this is only ever reached after App.teardown has abandoned
// a GStreamer state change inside cgo — gst_element_set_state takes no timeout
// and cannot be interrupted from Go — and the DLLs loaded into this process at
// that moment include the whole bundled GStreamer, WASAPI and COM.
//
// TerminateProcess does step 1 and step 3 and NOT step 2. No detach handler
// runs, so no detach handler can block. It is the only exit that a wedged media
// library cannot veto, and "a process that will not exit is a support call" is
// the promise being kept.
//
// # What is given up by not running DLL_PROCESS_DETACH
//
// Nothing this application relies on. Everything durable is already committed
// by the time teardown runs: config.json and mixer-golden.json are both written
// through a temp file and an os.Rename, synchronously, inside the call that
// wrote them. Credential Manager writes are synchronous too. There is no
// buffered writer, no database and no flush-on-exit anywhere in the process.
// The audio endpoint, both SRT sockets and the M2L-X fan-out slot are released
// by the kernel when the process object goes, which is exactly as true here as
// under any other exit.
//
// # Why it is not the normal exit
//
// Because a shutdown that completed has nothing to hide from and the ordinary
// path is better behaved: it lets the Go runtime finish, it lets Wails return
// its error to main, and it lets anything that ever does need a detach handler
// have one. This replaces forceExit, which teardown calls only when it has
// already abandoned a step.
package main

import (
	"log"
	"os"
	"syscall"
	"testing"
)

func init() {
	// NOT UNDER `go test`. The default in app.go carries the argument: teardown
	// leaves through forceExit on every close now, so installing a real TerminateProcess
	// here would end the test runner the first time any test closed an App. A
	// test that wants to observe the ending injects App.exitProcess instead.
	if testing.Testing() {
		return
	}
	forceExit = terminateSelf
}

// terminateSelf ends this process without running any DLL detach handler.
//
// GetCurrentProcess returns a pseudo-handle that is always valid, needs no
// OpenProcess and must not be closed, so there is nothing between here and the
// kernel that can fail for an ordinary reason. If either call returns anyway
// the fallback is the ordinary exit — worse, but better than a function called
// terminateSelf returning to a caller that has already logged the process as
// ending.
func terminateSelf() {
	self, err := syscall.GetCurrentProcess()
	if err == nil {
		err = syscall.TerminateProcess(self, 0)
	}
	if err != nil {
		log.Printf("wslcomms: could not terminate this process (%v); falling back to the ordinary exit, "+
			"which may block in DLL_PROCESS_DETACH if a media thread was abandoned", err)
	}
	os.Exit(0)
}
