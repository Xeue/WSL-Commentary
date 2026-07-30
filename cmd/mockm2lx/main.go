// Command mockm2lx is a stand-in for the M2L-X instance, so that the control
// plane, the reconnect state machine and the whole application can be
// exercised with no cloud instance and no libsrt.
//
// Owner: WP-7. No other work package writes files in this directory.
//
// It serves:
//
//   - POST /api/local_auth/signin — body {"alias": ..., "password": ...}. A
//     body using "username" instead of "alias" returns HTTP 500, because
//     that is what the real instance does (auth.go).
//   - POST /api/local_auth/refresh_token (auth.go).
//   - GET /api/live_operation/kvs/webrtc_info/{event} and
//     /webrtc_token/{event}, the measured shapes of docs/test-results.md
//     line 141 (kvs.go).
//   - GET /api/v1/switcher_status?access_token=... — the push-only status
//     WebSocket that is the app's sole liveness truth (status.go).
//   - an SRT listener with the real listener's one-peer, re-accept-refusal
//     semantics, reporting bitrate, detected PIDs and whether DTS ever goes
//     backwards (srt.go, mpegts.go).
//   - POST/GET /control/* — fault injection, driveable at runtime by a test
//     in addition to the startup flags below (control.go).
//
// The SRT listener is github.com/datarhei/gosrt, a pure-Go implementation.
// It is used HERE AND ONLY HERE. The production path uses GStreamer's
// srtsink over libsrt; gosrt exists in this module so that the mock needs no
// native toolchain, and it must never be imported by anything under
// internal/.
//
// # Fault injection is the point
//
// Most of this program's value is in reproducing the failure modes the
// measurements actually found. A mock that only works when everything works
// is not worth building. See cmd/mockm2lx/README.md for a worked example of
// the one that matters most: the re-accept refusal window that a naive
// reconnect loop fails against.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"wslcomms/internal/m2lx"
)

func main() {
	opts, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "mockm2lx:", err)
		os.Exit(2)
	}

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmsgprefix)
	if err := run(opts, logger); err != nil {
		logger.Fatalf("mockm2lx: %v", err)
	}
}

// parseFlags builds an Options from args. Every fault-injection value is
// also settable at runtime via /control (control.go); these flags only set
// where it starts.
func parseFlags(args []string) (Options, error) {
	fs := flag.NewFlagSet("mockm2lx", flag.ContinueOnError)

	addr := fs.String("addr", ":8080", "HTTP/WS listen address (REST, status WebSocket, /control)")
	srtAddr := fs.String("srt-addr", ":4001", "SRT listen address, host:port")
	alias := fs.String("alias", "wsl-comms-ro", "alias POST /api/local_auth/signin must be sent")
	password := fs.String("password", "changeme", "password POST /api/local_auth/signin must be sent")
	statusKey := fs.String("status-key", "cam7", "switcher_status node name for our router input (spec section 9)")
	statusInterval := fs.Duration("status-interval", 2*time.Second, "how often a status snapshot is pushed")
	tokenTTL := fs.Duration("token-ttl", m2lx.TokenLifetime, "access token lifetime handed out in expires_in")
	srtPassphrase := fs.String("srt-passphrase", "", "SRT passphrase required of callers; empty means none required")
	srtPBKeylen := fs.Int("srt-pbkeylen", 16, "SRT crypto key length in bytes when srt-passphrase is set: 16, 24 or 32")
	onePeerOnly := fs.Bool("one-peer-only", true, "refuse a second SRT caller without displacing the incumbent")
	refusalWindow := fs.Duration("refusal-window", 6*time.Second, "how long, after a disconnect, the SRT listener refuses to re-accept")
	stallStatus := fs.Bool("stall-status", false, "start with the status WebSocket stalled (connections succeed, nothing is ever pushed)")
	lieStreamState := fs.String("lie-stream-state", "", "start reporting this stream_state regardless of SRT truth; one of streaming/starting/stopped, empty = report the truth")
	dropAudio := fs.Bool("drop-audio", false, "start with the status document's audio array forced empty")
	verbose := fs.Bool("verbose", false, "also log every periodic status push, not just connection/fault events")

	if err := fs.Parse(args); err != nil {
		return Options{}, err
	}

	if *srtPBKeylen != 16 && *srtPBKeylen != 24 && *srtPBKeylen != 32 {
		return Options{}, fmt.Errorf("-srt-pbkeylen must be 16, 24 or 32, got %d", *srtPBKeylen)
	}
	switch *lieStreamState {
	case "", m2lx.StreamStateStreaming, m2lx.StreamStateStarting, m2lx.StreamStateStopped:
	default:
		return Options{}, fmt.Errorf("-lie-stream-state must be streaming, starting, stopped, or empty, got %q", *lieStreamState)
	}
	if *refusalWindow < 0 {
		return Options{}, fmt.Errorf("-refusal-window must be >= 0")
	}
	if *statusInterval <= 0 {
		return Options{}, fmt.Errorf("-status-interval must be > 0")
	}

	return Options{
		Addr:           *addr,
		SRTAddr:        *srtAddr,
		Alias:          *alias,
		Password:       *password,
		StatusKey:      *statusKey,
		StatusInterval: *statusInterval,
		TokenTTL:       *tokenTTL,
		SRTPassphrase:  *srtPassphrase,
		SRTPBKeylen:    *srtPBKeylen,
		OnePeerOnly:    *onePeerOnly,
		RefusalWindow:  *refusalWindow,
		StallStatus:    *stallStatus,
		LieStreamState: *lieStreamState,
		DropAudio:      *dropAudio,
		Verbose:        *verbose,
	}, nil
}

// run wires up and runs the whole mock until it receives SIGINT/SIGTERM (or,
// on this platform, until Ctrl+C), then shuts down cleanly. It is separate
// from main so tests can call it with a cancellable context instead of
// exercising os.Exit.
func run(opts Options, logger *log.Logger) error {
	a := NewApp(opts, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/local_auth/signin", a.handleSignIn)
	mux.HandleFunc("POST /api/local_auth/refresh_token", a.handleRefreshToken)
	mux.HandleFunc("GET /api/live_operation/kvs/webrtc_info/{event}", a.requireAuth(a.handleKVSInfo))
	mux.HandleFunc("GET /api/live_operation/kvs/webrtc_token/{event}", a.requireAuth(a.handleKVSToken))
	mux.HandleFunc("GET /api/v1/switcher_status", a.handleStatusWS)

	mux.HandleFunc("GET /control/state", a.handleControlState)
	mux.HandleFunc("POST /control/drop-srt", a.handleControlDropSRT)
	mux.HandleFunc("POST /control/one-peer-only", a.handleControlOnePeerOnly)
	mux.HandleFunc("POST /control/refusal-window", a.handleControlRefusalWindow)
	mux.HandleFunc("POST /control/stall-status", a.handleControlStallStatus)
	mux.HandleFunc("POST /control/lie", a.handleControlLie)
	mux.HandleFunc("POST /control/drop-audio", a.handleControlDropAudio)
	mux.HandleFunc("POST /control/expire-token", a.handleControlExpireToken)
	mux.HandleFunc("POST /control/reset", a.handleControlReset)

	httpSrv := &http.Server{
		Addr:    opts.Addr,
		Handler: mux,
	}

	// Bind before logging "listening": if the port is taken, the log should
	// say so, not claim success and then immediately contradict itself.
	httpLn, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return fmt.Errorf("http listen on %s: %w", opts.Addr, err)
	}
	logger.Printf("[http] listening on %s", httpLn.Addr())
	logger.Printf("[http]   sign-in: alias=%q password=%q", opts.Alias, opts.Password)
	logger.Printf("[http]   status key: %q, pushed every %s", opts.StatusKey, opts.StatusInterval)

	errCh := make(chan error, 2)

	go func() {
		if err := httpSrv.Serve(httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
			return
		}
		errCh <- nil
	}()

	go func() {
		if err := a.runSRTListener(ctx, opts.SRTAddr); err != nil {
			errCh <- fmt.Errorf("srt listener: %w", err)
			return
		}
		errCh <- nil
	}()

	go a.runStatusBroadcaster(ctx)

	var runErr error
	select {
	case <-ctx.Done():
		logger.Printf("shutting down")
	case err := <-errCh:
		if err != nil {
			runErr = err
			logger.Printf("fatal: %v — shutting down", err)
		}
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Printf("[http] shutdown: %v", err)
	}

	// Drain the remaining goroutine's completion so its errCh send never
	// blocks after we stop reading it, and so a genuine startup failure on
	// the second goroutine is not lost.
	select {
	case err := <-errCh:
		if err != nil && runErr == nil {
			runErr = err
		}
	case <-time.After(5 * time.Second):
	}

	return runErr
}
