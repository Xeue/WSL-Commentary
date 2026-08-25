// Command probe is a standalone SRT caller that dials an M2L-X output, receives
// the transport stream the app's picture path would receive, and reports what is
// actually in it -- per-PID continuity, and an HEVC NAL census -- ending in a
// verdict (see analyze.go). It exists to end a day of "every log reframes it" by
// measuring the received bytes directly, on both the machine that tears and one
// that does not, so the difference is a diffable number rather than a theory.
//
// It is pure Go over github.com/datarhei/gosrt -- no GStreamer, no cgo, no
// bundle. Build it with CGO_ENABLED=0 and it is one small exe that runs anywhere.
// gosrt is used HERE (a cmd/, not internal/), exactly as cmd/mockm2lx uses it.
//
// # Capture and replay -- the point when the stream is expensive to keep up
//
// -save writes every received byte to a .ts file WHILE analysing, so a single
// live minute is banked for offline work: that file can be re-analysed here with
// -file, and -- because it is a plain MPEG-TS -- replayed through the FULL
// GStreamer pipeline dissector offline (cmd/gstprobe capture.ts, which uses
// filesrc). That replay is the crux experiment: if COMM-01's captured bytes tear
// the pipeline HERE too, the fault travels with the bytes; if they are clean
// here, the fault is the field machine's decode/timing.
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

	srt "github.com/datarhei/gosrt"
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
	addr := normalizeAddr(target)

	cfg := srt.DefaultConfig()
	cfg.Latency = time.Duration(*latency) * time.Millisecond
	cfg.StreamId = *streamid
	cfg.Passphrase = *passphrase

	fmt.Printf("probe: dialing %s as a caller (latency %dms)...\n", addr, *latency)
	conn, err := srt.Dial("srt", addr, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: dial failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "       (is the app or another probe already holding the one SRT peer? stop it first.)")
		os.Exit(1)
	}
	defer conn.Close()

	var capFile *os.File
	if *save != "" {
		capFile, err = os.Create(*save)
		if err != nil {
			fmt.Fprintf(os.Stderr, "probe: could not create %s: %v\n", *save, err)
			os.Exit(1)
		}
		defer capFile.Close()
		fmt.Printf("probe: saving the raw transport stream to %s\n", *save)
	}
	fmt.Printf("probe: connected; capturing for %ds (Ctrl+C to stop early)...\n", *secs)

	start := time.Now()
	an := newAnalyzer(func() time.Duration { return time.Since(start) })

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	timer := time.AfterFunc(time.Duration(*secs)*time.Second, cancel)
	defer timer.Stop()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	buf := make([]byte, 64*1024)
	for {
		n, rerr := conn.Read(buf)
		if n > 0 {
			an.Write(buf[:n])
			if capFile != nil {
				capFile.Write(buf[:n])
			}
		}
		if rerr != nil {
			break
		}
	}

	dur := time.Since(start)
	emit(an.report(addr, dur), hostSlug(addr))
}

// analyseFile runs the analyzer over a previously captured .ts file. The clock is
// byte position rather than wall time -- there is no reception timing in a file
// -- so the timeline is in "stream seconds" derived from the nominal bitrate is
// not attempted; timing-derived lines simply read from the file's own order.
func analyseFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: could not open %s: %v\n", path, err)
		os.Exit(1)
	}
	defer f.Close()
	fmt.Printf("probe: analysing captured stream %s...\n", path)

	start := time.Now()
	an := newAnalyzer(func() time.Duration { return time.Since(start) })
	if _, err := io.Copy(writerFunc(an.Write), f); err != nil {
		fmt.Fprintf(os.Stderr, "probe: read error on %s: %v\n", path, err)
	}
	emit(an.report(path, time.Since(start)), fileSlug(path))
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

// writerFunc adapts a Write method to io.Writer for io.Copy.
type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

// normalizeAddr turns "srt://host:port?query", "host:port" or a bare host into a
// dialable host:port, defaulting the port to M2L-X's return port when omitted.
func normalizeAddr(s string) string {
	s = strings.TrimPrefix(s, "srt://")
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	if !strings.Contains(s, ":") {
		s += ":40504"
	}
	return s
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
