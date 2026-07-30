// The SRT listener. This is the half of the mock that matters most: M2L-X's
// router input is an SRT listener with one-peer semantics and a re-accept
// refusal window after a disconnect (docs/test-results.md line 149), and
// that behaviour — not the happy path — is what internal/sender's backoff
// ladder has to survive (spec section 6.2).
//
// github.com/datarhei/gosrt is a pure-Go SRT implementation, used HERE AND
// ONLY HERE (CONTRACT.md). The production path is GStreamer's srtsink over
// libsrt; gosrt exists in this module purely so the mock needs no native
// toolchain at Gate A.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	srt "github.com/datarhei/gosrt"
)

// srtState is the SRT listener's own runtime state, separate from App.mu
// because the accept loop and the per-connection reader goroutine touch it
// on every packet, far more often than anything touches auth or WebSocket
// state.
type srtState struct {
	mu sync.Mutex

	ln srt.Listener

	current  srt.Conn // nil when no peer is connected
	peerAddr string
	streamID string

	connectedAt    time.Time
	disconnectedAt time.Time // zero value: never disconnected yet

	analyzer *tsAnalyzer // set on accept, kept (not cleared) after disconnect so /control/state can still report the last session
}

func newSRTState() *srtState {
	return &srtState{}
}

// Snapshot is a point-in-time read of the SRT session, for the status
// broadcaster and the /control/state endpoint.
//
// ConnectedAt and DisconnectedAt are *time.Time, not time.Time: encoding/
// json's omitempty does not treat a zero-valued struct (which is what an
// unset time.Time is) as empty, only nil pointers, so a value type here
// would always render a spurious "0001-01-01T00:00:00Z" before either event
// has ever happened.
type srtSnapshot struct {
	Connected      bool              `json:"connected"`
	PeerAddr       string            `json:"peerAddr,omitempty"`
	StreamID       string            `json:"streamId,omitempty"`
	ConnectedAt    *time.Time        `json:"connectedAt,omitempty"`
	DisconnectedAt *time.Time        `json:"disconnectedAt,omitempty"`
	Analyzer       *AnalyzerSnapshot `json:"analyzer,omitempty"`
}

func (s *srtState) snapshot() srtSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap := srtSnapshot{
		Connected: s.current != nil,
		PeerAddr:  s.peerAddr,
		StreamID:  s.streamID,
	}
	if !s.connectedAt.IsZero() {
		t := s.connectedAt
		snap.ConnectedAt = &t
	}
	if !s.disconnectedAt.IsZero() {
		t := s.disconnectedAt
		snap.DisconnectedAt = &t
	}
	if s.analyzer != nil {
		as := s.analyzer.Snapshot()
		snap.Analyzer = &as
	}
	return snap
}

// runSRTListener opens the SRT listener on addr and accepts callers until
// ctx is cancelled. It never returns a non-nil error except at startup: once
// listening, a per-connection failure is logged and the loop continues,
// because a real M2L-X listener does not stop existing when one caller
// misbehaves.
func (a *App) runSRTListener(ctx context.Context, addr string) error {
	cfg := srt.DefaultConfig()

	ln, err := srt.Listen("srt", addr, cfg)
	if err != nil {
		return fmt.Errorf("srt listen on %s: %w", addr, err)
	}
	a.srt.mu.Lock()
	a.srt.ln = ln
	a.srt.mu.Unlock()

	a.logf("srt", "listening on %s (passphrase required: %v, one-peer-only: %v, refusal window: %s)",
		addr, a.opts.SRTPassphrase != "", a.getOnePeerOnly(), a.getRefusalWindow())

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		req, err := ln.Accept2()
		if err != nil {
			if errors.Is(err, srt.ErrListenerClosed) {
				a.logf("srt", "listener closed")
				return nil
			}
			a.logf("srt", "accept error: %v", err)
			continue
		}
		a.handleConnRequest(req)
	}
}

// handleConnRequest decides whether to accept or reject one incoming SRT
// handshake, applying the refusal-window and one-peer-only faults before
// anything else — those are the two behaviours that make this mock worth
// having, so they are checked first and unconditionally, ahead of
// passphrase validation.
func (a *App) handleConnRequest(req srt.ConnRequest) {
	remote := req.RemoteAddr()

	a.srt.mu.Lock()

	if !a.srt.disconnectedAt.IsZero() {
		window := a.getRefusalWindow()
		elapsed := time.Since(a.srt.disconnectedAt)
		if elapsed < window {
			a.srt.mu.Unlock()
			remaining := window - elapsed
			a.logf("srt", "REFUSE %s — re-accept refusal window active, %s remaining", remote, remaining.Round(10*time.Millisecond))
			req.Reject(srt.REJ_CLOSE)
			return
		}
	}

	if a.getOnePeerOnly() && a.srt.current != nil {
		a.srt.mu.Unlock()
		a.logf("srt", "REFUSE %s — one peer already connected (%s), incumbent is not displaced", remote, a.srt.peerAddr)
		req.Reject(srt.REJ_PEER)
		return
	}

	a.srt.mu.Unlock()

	// Passphrase check. This distinguishes ERROR:BADSECRET (wrong secret)
	// from ERROR:UNSECURE (a passphrase was expected on one side and not
	// offered on the other) exactly as libsrt does — spec open question 6
	// asks precisely this question of the real instance.
	//
	// Opts.SRTPBKeylen is deliberately not enforced here: gosrt's
	// ConnRequest interface exposes IsEncrypted/SetPassphrase but never the
	// caller's negotiated key length (crypto.New's klen argument is derived
	// entirely from the incoming handshake in conn_request.go and is not
	// surfaced), and gosrt's own listener Config.PBKeylen is likewise only
	// consumed on the DIALER side (dial.go). A caller offering the wrong key
	// length still goes through SetPassphrase and simply fails to decrypt
	// correctly, which this mock cannot tell apart from an outright wrong
	// passphrase with this library version. SRTPBKeylen is kept as
	// documented, logged configuration for that reason, not as an enforced
	// gate.
	required := a.opts.SRTPassphrase
	switch {
	case req.IsEncrypted() && required == "":
		a.logf("srt", "REFUSE %s — caller offered a passphrase, none is configured (ERROR:UNSECURE)", remote)
		req.Reject(srt.REJ_UNSECURE)
		return
	case !req.IsEncrypted() && required != "":
		a.logf("srt", "REFUSE %s — passphrase required, caller offered none (ERROR:UNSECURE)", remote)
		req.Reject(srt.REJ_UNSECURE)
		return
	case req.IsEncrypted() && required != "":
		if err := req.SetPassphrase(required); err != nil {
			a.logf("srt", "REFUSE %s — passphrase mismatch (ERROR:BADSECRET): %v", remote, err)
			req.Reject(srt.REJ_BADSECRET)
			return
		}
	}

	conn, err := req.Accept()
	if err != nil {
		a.logf("srt", "accept failed for %s: %v", remote, err)
		return
	}

	a.srt.mu.Lock()
	a.srt.current = conn
	a.srt.peerAddr = conn.RemoteAddr().String()
	a.srt.streamID = conn.StreamId()
	a.srt.connectedAt = time.Now()
	a.srt.disconnectedAt = time.Time{}
	a.srt.analyzer = newTSAnalyzer()
	a.srt.mu.Unlock()

	a.logf("srt", "ACCEPT %s (stream id %q) — caller connected", conn.RemoteAddr(), conn.StreamId())

	go a.readSRTConn(conn)
}

// srtReadBufferSize is comfortably larger than one SRT payload (spec section
// 5's alignment=7 gives 1316-byte MPEG-TS buffers) so a single Read call
// almost always returns one whole write from the sender's side, though
// nothing in this analyzer depends on that.
const srtReadBufferSize = 64 * 1024

// readSRTConn drains conn until it errors (peer disconnect, our own Close
// from /control/drop-srt, or the listener shutting down), feeding every
// byte read to the session's tsAnalyzer.
func (a *App) readSRTConn(conn srt.Conn) {
	buf := make([]byte, srtReadBufferSize)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			a.srt.mu.Lock()
			an := a.srt.analyzer
			a.srt.mu.Unlock()
			if an != nil {
				an.Write(buf[:n])
				if detail, first := an.FirstDTSBackwards(); first {
					a.logf("srt", "DTS REGRESSION on %s — %s", conn.RemoteAddr(), detail)
				}
			}
		}
		if err != nil {
			break
		}
	}
	conn.Close()
	a.onSRTDisconnect(conn)
}

// onSRTDisconnect records the end of a session and starts the re-accept
// refusal window. It is idempotent against being called twice for the same
// connection — once from readSRTConn's natural exit and, in the
// /control/drop-srt case, that IS the only call, since dropSRTNow closes the
// connection and lets readSRTConn's own Read error unwind into this
// function; it never calls this function directly itself.
func (a *App) onSRTDisconnect(conn srt.Conn) {
	a.srt.mu.Lock()
	defer a.srt.mu.Unlock()

	if a.srt.current != conn {
		// Superseded already — nothing to do. Can't happen with
		// one-peer-only on, but the check keeps this function correct
		// even when a test has turned that fault off.
		return
	}

	peer := a.srt.peerAddr
	a.srt.current = nil
	a.srt.disconnectedAt = time.Now()

	window := a.getRefusalWindow()
	a.logf("srt", "DISCONNECT %s — refusing re-accept for %s", peer, window)
}

// dropSRTNow closes the current SRT session, if any, simulating the peer
// vanishing. It backs the /control/drop-srt fault. It reports whether a
// session was actually dropped.
func (a *App) dropSRTNow() bool {
	a.srt.mu.Lock()
	conn := a.srt.current
	a.srt.mu.Unlock()
	if conn == nil {
		return false
	}
	a.logf("srt", "drop-srt: forcing disconnect of %s", conn.RemoteAddr())
	conn.Close() // unblocks readSRTConn's Read, which does the rest
	return true
}

// srtPeerConnected reports whether an SRT caller is currently connected. It
// is the ground truth the status document reports, before the "lie" fault
// (control.go) is allowed to override it.
func (a *App) srtPeerConnected() bool {
	a.srt.mu.Lock()
	defer a.srt.mu.Unlock()
	return a.srt.current != nil
}

// srtAnalyzerSnapshot returns the current session's analyzer snapshot, or
// the zero value if no session has ever connected.
func (a *App) srtAnalyzerSnapshot() AnalyzerSnapshot {
	a.srt.mu.Lock()
	an := a.srt.analyzer
	a.srt.mu.Unlock()
	if an == nil {
		return AnalyzerSnapshot{}
	}
	return an.Snapshot()
}

// srtListenerAddr returns the address the SRT listener is bound to. Used
// only for the startup log line; net.Addr's String is good enough there.
func (a *App) srtListenerAddr() net.Addr {
	a.srt.mu.Lock()
	defer a.srt.mu.Unlock()
	if a.srt.ln == nil {
		return nil
	}
	return a.srt.ln.Addr()
}
