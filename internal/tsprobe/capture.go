package tsprobe

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	srt "github.com/datarhei/gosrt"
)

// DefaultReturnPort is M2L-X's picture-return port on the facility's
// instances, the one NormalizeAddr supplies when none is given.
const DefaultReturnPort = 40504

// CaptureOpts configures one live capture.
type CaptureOpts struct {
	// LatencyMs is the SRT latency to negotiate; 0 means 2000, which is what
	// the field machines run, so that reception is comparable.
	LatencyMs int

	// Passphrase and StreamID, when the output needs them. The facility's
	// return outputs are unencrypted.
	Passphrase string
	StreamID   string

	// Duration ends the capture; 0 means run until ctx is done or the peer
	// closes.
	Duration time.Duration

	// Save, when set, receives every byte exactly as it arrived — the raw
	// transport stream, replayable through the pipeline dissector.
	Save io.Writer

	// Progress, when set, is told what is happening, one line at a time.
	Progress func(string)
}

// Capture dials addr as an SRT caller — as the app's picture path does — and
// feeds everything received to a fresh Analyzer (and to opts.Save). It returns
// the analyzer, ready for Report and Summary, and how long the capture ran.
//
// A dial failure is an error. Connecting and then receiving nothing is also an
// error, because a report over zero bytes reads as "clean" and is not.
func Capture(ctx context.Context, addr string, opts CaptureOpts) (*Analyzer, time.Duration, error) {
	say := opts.Progress
	if say == nil {
		say = func(string) {}
	}
	if opts.LatencyMs <= 0 {
		opts.LatencyMs = 2000
	}
	cfg := srt.DefaultConfig()
	cfg.Latency = time.Duration(opts.LatencyMs) * time.Millisecond
	cfg.StreamId = opts.StreamID
	cfg.Passphrase = opts.Passphrase

	say(fmt.Sprintf("dialling %s as an SRT caller (latency %d ms)", addr, opts.LatencyMs))
	conn, err := srt.Dial("srt", addr, cfg)
	if err != nil {
		return nil, 0, fmt.Errorf("tsprobe: dial %s: %w (an M2L-X output accepts ONE caller: is the app, "+
			"its PGM monitor or another probe holding it?)", addr, err)
	}
	defer conn.Close()

	start := time.Now()
	an := NewAnalyzer(func() time.Duration { return time.Since(start) })

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if opts.Duration > 0 {
		t := time.AfterFunc(opts.Duration, cancel)
		defer t.Stop()
	}
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	say("connected; receiving")
	buf := make([]byte, 64*1024)
	var total uint64
	nextSay := 10 * time.Second
	for {
		n, rerr := conn.Read(buf)
		if n > 0 {
			an.Write(buf[:n])
			total += uint64(n)
			if opts.Save != nil {
				if _, werr := opts.Save.Write(buf[:n]); werr != nil {
					return an, time.Since(start), fmt.Errorf("tsprobe: saving the capture: %w", werr)
				}
			}
		}
		if el := time.Since(start); el >= nextSay {
			say(fmt.Sprintf("%3.0f s: %d MB received", el.Seconds(), total>>20))
			nextSay += 10 * time.Second
		}
		if rerr != nil {
			break
		}
	}
	elapsed := time.Since(start)
	if total == 0 {
		return an, elapsed, fmt.Errorf("tsprobe: connected to %s but received nothing in %s", addr,
			elapsed.Round(time.Second))
	}
	return an, elapsed, nil
}

// NormalizeAddr turns "srt://host:port?query", "host:port" or a bare host into
// a dialable host:port, defaulting the port to DefaultReturnPort when omitted.
func NormalizeAddr(s string) string {
	s = strings.TrimPrefix(s, "srt://")
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	if !strings.Contains(s, ":") {
		s += fmt.Sprintf(":%d", DefaultReturnPort)
	}
	return s
}
