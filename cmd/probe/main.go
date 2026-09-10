// Command probe is a standalone SRT caller that dials an M2L-X output, receives
// the transport stream the app's picture path would receive, and reports what is
// actually in it -- per-PID continuity, and an HEVC NAL census -- ending in a
// verdict. It exists to end a day of "every log reframes it" by measuring the
// received bytes directly, on both the machine that tears and one that does
// not, so the difference is a diffable number rather than a theory.
//
// The dissector itself lives in internal/tsprobe, because the field rig (the
// application's WSLCOMMS_RIG mode) runs the same capture; this is the
// standalone, pure-Go, no-GStreamer shell around it. Build it with
// CGO_ENABLED=0 and it is one small exe that runs anywhere.
//
// # Capture and replay -- the point when the stream is expensive to keep up
//
// -save writes every received byte to a .ts file WHILE analysing, so a single
// live minute is banked for offline work: that file can be re-analysed here with
// -file, and -- because it is a plain MPEG-TS -- replayed through the FULL
// GStreamer pipeline dissector offline (cmd/gstprobe capture.ts, which uses
// filesrc). That replay is the crux experiment: if the field machine's captured
// bytes tear the pipeline HERE too, the fault travels with the bytes; if they
// are clean here, the fault is the field machine's decode/timing.
//
// Usage:
//
//	probe [flags] <host:port>          # dial and analyse a live output
//	probe -save cap.ts <host:port>     # ...and bank the bytes for offline work
//	probe -file cap.ts                 # analyse a previously captured .ts, no SRT
//	probe m2lx-wslstudios-matchg.etapsiota.com:40504
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"wslcomms/internal/tsprobe"
)

func main() {
	secs := flag.Int("secs", 30, "seconds to capture before reporting (live mode)")
	latency := flag.Int("latency", 2000, "SRT latency in ms; match the app (2000) so reception is comparable")
	passphrase := flag.String("passphrase", "", "SRT passphrase, if the output is encrypted (MatchG is not)")
	streamid := flag.String("streamid", "", "SRT stream id, if the listener requires one")
	save := flag.String("save", "", "also write the received transport stream to this .ts file")
	file := flag.String("file", "", "analyse a previously captured .ts file instead of dialing SRT")
	flag.Parse()

	if *file != "" {
		analyseFile(*file)
		return
	}

	target := flag.Arg(0)
	if target == "" {
		fmt.Fprintln(os.Stderr, "usage: probe [flags] <host:port | srt://host:port>   (or: probe -file cap.ts)")
		flag.PrintDefaults()
		os.Exit(2)
	}
	addr := tsprobe.NormalizeAddr(target)

	var capFile *os.File
	if *save != "" {
		var err error
		capFile, err = os.Create(*save)
		if err != nil {
			fmt.Fprintf(os.Stderr, "probe: could not create %s: %v\n", *save, err)
			os.Exit(1)
		}
		defer capFile.Close()
		fmt.Printf("probe: saving the raw transport stream to %s\n", *save)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	opts := tsprobe.CaptureOpts{
		LatencyMs:  *latency,
		Passphrase: *passphrase,
		StreamID:   *streamid,
		Duration:   time.Duration(*secs) * time.Second,
		Progress:   func(s string) { fmt.Println("probe:", s) },
	}
	if capFile != nil {
		opts.Save = capFile
	}
	fmt.Printf("probe: capturing for %ds (Ctrl+C to stop early)...\n", *secs)
	an, dur, err := tsprobe.Capture(ctx, addr, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: %v\n", err)
		if an == nil {
			os.Exit(1)
		}
	}
	emit(an.Report(addr, dur), hostSlug(addr))
}

// analyseFile runs the analyzer over a previously captured .ts file. The clock is
// wall time over the read, so timing-derived lines simply read from the file's
// own order.
func analyseFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: could not open %s: %v\n", path, err)
		os.Exit(1)
	}
	defer f.Close()

	fmt.Printf("probe: analysing captured stream %s...\n", path)
	start := time.Now()
	an := tsprobe.NewAnalyzer(func() time.Duration { return time.Since(start) })
	if _, err := io.Copy(an, f); err != nil {
		fmt.Fprintf(os.Stderr, "probe: read error on %s: %v\n", path, err)
	}
	emit(an.Report(path, time.Since(start)), fileSlug(path))
}

// emit prints a report and writes it beside the working directory.
func emit(report, slug string) {
	fmt.Print("\n", report, "\n")
	name := fmt.Sprintf("probe-report-%s.txt", slug)
	if err := os.WriteFile(name, []byte(report), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "probe: could not write %s: %v\n", name, err)
	} else {
		fmt.Printf("probe: report written to %s\n", name)
	}
}

func hostSlug(addr string) string {
	return strings.NewReplacer(":", "-", ".", "_", "/", "-").Replace(addr)
}

func fileSlug(path string) string {
	base := path
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		base = path[i+1:]
	}
	return strings.NewReplacer(".", "_", " ", "-").Replace(base)
}
