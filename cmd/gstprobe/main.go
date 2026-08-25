// Command gstprobe runs the GStreamer pipeline dissector (internal/gst's
// RunPipelineDiagnostic) from the command line, so the picture path can be taken
// apart on a development machine without building the whole Wails app. The field
// machine uses the same dissector through the app's headless mode
// (WSLCOMMS_DIAGNOSE, see main.go); this is the bench equivalent.
//
// It needs a real cgo GStreamer (Gate B): run it inside build/env.ps1, where the
// bundled GStreamer 1.28 is on PATH, and RunPipelineDiagnostic's gogst.Init picks
// it up. Under CGO_ENABLED=0 it links the stub and prints that it needs cgo.
//
// Usage:
//
//	gstprobe <srt://host:port | host:port> [secs] [decoder]
//	gstprobe m2lx-wslstudios-matchg.etapsiota.com:40504 30 avdec_h265
//	gstprobe host:40504 30 d3d11h265dec     # compare the hardware decoder
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"wslcomms/internal/gst"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: gstprobe <srt://host:port | host:port> [secs] [decoder]")
		os.Exit(2)
	}

	// An existing file is replayed through filesrc; anything else is an SRT target
	// (host:port or a full srt:// URI), normalised for srtsrc. The file check must
	// come first so a Windows path like C:\...\cap.ts is never mistaken for a URI.
	uri := os.Args[1]
	if _, err := os.Stat(uri); err != nil && !strings.HasPrefix(uri, "srt://") {
		if i := strings.IndexByte(uri, '?'); i >= 0 {
			uri = uri[:i]
		}
		if !strings.Contains(uri, ":") {
			uri += ":40504"
		}
		uri = "srt://" + uri
	}

	secs := 30
	if len(os.Args) > 2 {
		if n, err := strconv.Atoi(os.Args[2]); err == nil && n > 0 {
			secs = n
		}
	}
	decoder := ""
	if len(os.Args) > 3 {
		decoder = os.Args[3]
	}

	fmt.Printf("gstprobe: dissecting %s for %ds (decoder %q)...\n", uri, secs, decoder)
	report, err := gst.RunPipelineDiagnostic(uri, time.Duration(secs)*time.Second, decoder)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gstprobe:", err)
		os.Exit(1)
	}
	fmt.Println()
	fmt.Println(report)
}
