//go:build sendlive && cgo && !gststub && windows

package gst

import "golang.org/x/sys/windows"

// sendLiveExit ends the test process by TerminateProcess; see TestMain.
func sendLiveExit(code int) {
	_ = windows.TerminateProcess(windows.CurrentProcess(), uint32(code))
}
