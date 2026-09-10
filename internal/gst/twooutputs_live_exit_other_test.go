//go:build sendlive && cgo && !gststub && !windows

package gst

import "os"

// sendLiveExit is a plain exit off Windows; see TestMain.
func sendLiveExit(code int) { os.Exit(code) }
