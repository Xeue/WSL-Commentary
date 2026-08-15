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
//     WebSocket that is the app's sole liveness truth. It is
//     SNAPSHOT-THEN-DELTA: frame 0 of a connection is the whole 36-node
//     document at path "/", and every frame after it is a subtree delta at
//     about 21 a second (status.go owns the connection, switcherdoc.go owns
//     the document).
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
// is not worth building. See cmd/mockm2lx/README.md for worked examples of
// the three that matter most: the re-accept refusal window that a naive
// reconnect loop fails against, the subtree delta that a parser ignoring
// "path" reads as a whole node (-decoy-delta), and the open question of
// whether a state change is pushed on the status socket at all
// (-transition-push).
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
	statusKey := fs.String("status-key", "cam7", "switcher_status node name for our router input (spec section 9); must be one of the 24 measured router inputs, or internal/m2lx reports it as unknown — which is itself a reachable Gate A case")
	statusInterval := fs.Duration("status-interval", 48*time.Millisecond, "how often the status socket pushes a frame; frame 0 of a connection is the whole document and every frame after it is a subtree delta, so this is the DELTA rate (measured: ~21 frames a second)")
	tokenTTL := fs.Duration("token-ttl", m2lx.TokenLifetime, "access token lifetime handed out in expires_in")
	srtPassphrase := fs.String("srt-passphrase", "", "SRT passphrase required of callers; empty means none required")
	srtPBKeylen := fs.Int("srt-pbkeylen", 16, "SRT crypto key length in bytes when srt-passphrase is set: 16, 24 or 32")
	onePeerOnly := fs.Bool("one-peer-only", true, "refuse a second SRT caller without displacing the incumbent")
	refusalWindow := fs.Duration("refusal-window", 6*time.Second, "how long, after a disconnect, the SRT listener refuses to re-accept")
	stallStatus := fs.Bool("stall-status", false, "start with the status WebSocket stalled (connections succeed, nothing is ever pushed)")
	lieStreamState := fs.String("lie-stream-state", "", "start reporting this stream_state regardless of SRT truth; one of streaming/starting/stopped, empty = report the truth")
	dropAudio := fs.Bool("drop-audio", false, "start with the status document's audio array forced EMPTY — the MP2/AC-3 silent-drop signature, which is not the same as a stopped input's one-element array with a null format")
	decoyDelta := fs.String("decoy-delta", decoyDeltaOff, "send a subtree delta a naive parser would mistake for a whole node: off, statistics (the measured trap frame), or stream-state (a whole-node-looking state at a subtree path)")
	transitionPush := fs.String("transition-push", transitionPushNode, "how a stream_state/format change is published: node (whole-node entry at \"/\"), delta (/streams then /stream_state), or none (never published — only a reconnect reveals it)")
	verbose := fs.Bool("verbose", false, "also log the opening snapshot and a once-a-second delta summary, not just connection/fault/transition events")

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
	switch *decoyDelta {
	case decoyDeltaOff, decoyDeltaStatistics, decoyDeltaStreamState:
	default:
		return Options{}, fmt.Errorf("-decoy-delta must be %s, %s or %s, got %q",
			decoyDeltaOff, decoyDeltaStatistics, decoyDeltaStreamState, *decoyDelta)
	}
	switch *transitionPush {
	case transitionPushNode, transitionPushDelta, transitionPushNone:
	default:
		return Options{}, fmt.Errorf("-transition-push must be %s, %s or %s, got %q",
			transitionPushNode, transitionPushDelta, transitionPushNone, *transitionPush)
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
		DecoyDelta:     *decoyDelta,
		TransitionPush: *transitionPush,
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
	// The FIRST rung of the conform ladder. Without it the mock 404s, the app
	// falls to its 1920x1080p50 default, and every run against this mock
	// exercises the fallback while looking like a success — see
	// configuration.go's header.
	mux.HandleFunc("GET /api/v1/switcher_configuration", a.requireAuth(a.handleSwitcherConfiguration))

	mux.HandleFunc("GET /control/state", a.handleControlState)
	mux.HandleFunc("POST /control/switcher-format", a.handleControlSwitcherFormat)
	mux.HandleFunc("POST /control/drop-srt", a.handleControlDropSRT)
	mux.HandleFunc("POST /control/one-peer-only", a.handleControlOnePeerOnly)
	mux.HandleFunc("POST /control/refusal-window", a.handleControlRefusalWindow)
	mux.HandleFunc("POST /control/stall-status", a.handleControlStallStatus)
	mux.HandleFunc("POST /control/lie", a.handleControlLie)
	mux.HandleFunc("POST /control/drop-audio", a.handleControlDropAudio)
	mux.HandleFunc("POST /control/decoy-delta", a.handleControlDecoyDelta)
	mux.HandleFunc("POST /control/transition-push", a.handleControlTransitionPush)
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
	logger.Printf("[http]   status key: %q, one frame every %s (frame 0 per connection is the whole document, the rest are subtree deltas)",
		opts.StatusKey, opts.StatusInterval)
	if _, ok := lookupRouterInput(opts.StatusKey); !ok {
		// Not fatal, and not patched over either. The mock serves the measured
		// 36-node inventory and lets internal/m2lx report the mismatch itself,
		// which is how StatusKeyNotFoundError — including its "names a node
		// that is not a router input" variant — becomes reachable at Gate A.
		logger.Printf("[http]   NOTE: %q is not one of the %d measured router inputs, so no node will carry the SRT truth "+
			"and internal/m2lx will report it as unknown. That is a supported case, not a misconfiguration of this mock.",
			opts.StatusKey, len(routerInputs))
	}

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
