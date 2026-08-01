package m2lx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// rawSnapshotTimeout bounds one whole RawSnapshot: the dial, the TLS handshake
// and the wait for the opening frame.
//
// It is sized from the two things it has to contain. The dial is the same
// handshake the Watch loop performs, and M2L-X pushes the opening snapshot
// immediately on connect — MEASURED 2026-08-01, the first frame after a
// connection opens is the 84 KB full document (wire.go). Ten seconds is the
// bound this package already puts on its REST calls; fifteen is that plus room
// for an 84 KB frame over a congested facility link, and is still far shorter
// than an operator will wait before deciding the drawer is broken.
const rawSnapshotTimeout = 15 * time.Second

// ErrNoSnapshotFrame is returned by RawSnapshot when the connection delivered
// frames but none of them carried a whole-document state before the deadline.
//
// It is a distinct error rather than a plain timeout because the two mean
// different things to whoever has to fix it. A timeout is "the status socket is
// not delivering at all". This is "the socket is delivering, but only deltas" —
// which would mean the snapshot-then-delta behaviour documented in wire.go no
// longer holds, and nothing in this application can obtain a complete document
// any more.
var ErrNoSnapshotFrame = errors.New("m2lx: the status socket delivered only subtree deltas, never a whole-document frame")

// RawSnapshot implements Watcher. See the interface in m2lx.go for the
// contract this satisfies.
//
// It opens its OWN connection rather than borrowing the Watch loop's, and that
// is the entire point: the socket is snapshot-then-delta (wire.go), so the only
// frame that carries the whole document is the first one after a connection
// opens. A long-lived connection has already spent its snapshot; a fresh one
// has not. Every call therefore costs one dial, and the caller is expected to
// know that — see the interface comment.
func (w *watcher) RawSnapshot(ctx context.Context) ([]byte, error) {
	tok := w.client.Token()
	if tok == "" {
		return nil, ErrNotSignedIn
	}
	red := tokenRedactor(tok)

	ctx, cancel := context.WithTimeout(ctx, rawSnapshotTimeout)
	defer cancel()

	conn, err := w.dial(ctx, statusURL(w.status, tok))
	if err != nil {
		// The URL carries the token in its query string, and gorilla and the
		// net stack beneath it may quote the URL they failed on.
		return nil, fmt.Errorf("m2lx: opening the status socket for a snapshot: %w", red(err))
	}
	defer conn.Close()

	type read struct {
		p   []byte
		err error
	}
	// Buffered, and the send is guarded by ctx, so the reader goroutine can
	// always finish and exit even when this function has already returned. The
	// deferred Close unblocks a ReadMessage that is still in flight.
	frames := make(chan read, 1)
	go func() {
		for {
			_, p, err := conn.ReadMessage()
			select {
			case frames <- read{p, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	sawFrame := false
	for {
		select {
		case <-ctx.Done():
			if sawFrame {
				return nil, ErrNoSnapshotFrame
			}
			return nil, fmt.Errorf("m2lx: waiting for the opening status frame: %w", ctx.Err())
		case r := <-frames:
			if r.err != nil {
				return nil, fmt.Errorf("m2lx: reading the opening status frame: %w", red(r.err))
			}
			sawFrame = true
			// Deltas are skipped rather than returned. A caller handed a delta
			// would read one subtree as though it were the whole document —
			// which for the mixer drawer means an empty routing matrix, i.e.
			// "nothing is in the clean feed". The opening frame IS the
			// snapshot, so in practice this loop runs once; the loop exists so
			// that a device which ever pushes a delta first costs a few
			// milliseconds rather than a false reading.
			if hasWholeNodeEntry(r.p) {
				return r.p, nil
			}
		}
	}
}

// hasWholeNodeEntry reports whether a frame carries at least one entry at
// wholeNodePath, i.e. whether it is a snapshot rather than a subtree delta.
//
// It deliberately does NOT use extractAll: that counts only router inputs,
// which are recognised by their stream_state, and the node this function exists
// to deliver — advanced_audio_mixer — has none. The test here is the path,
// which is the test wire.go applies and the only one that generalises to nodes
// this package does not model.
func hasWholeNodeEntry(payload []byte) bool {
	entries, err := parseFrame(payload)
	if err != nil {
		return false
	}
	for _, raw := range entries {
		var e wireEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}
		if e.isWholeNode() {
			return true
		}
	}
	return false
}

// tokenRedactor returns a function that removes the bearer token from an
// error's text, in both its raw and its percent-encoded form.
//
// Both forms are needed: the token is put into the URL through
// url.Values.Encode, so a quoted URL carries the escaped form, while anything
// that logged the token directly would carry the raw one. Replacing an empty
// token would replace every position in the string, so an empty token is a
// no-op — a caller with no token never reaches a dial anyway.
func tokenRedactor(token string) func(error) error {
	escaped := url.QueryEscape(token)
	return func(err error) error {
		if err == nil || token == "" {
			return err
		}
		msg := err.Error()
		msg = strings.ReplaceAll(msg, escaped, "<redacted>")
		msg = strings.ReplaceAll(msg, token, "<redacted>")
		return errors.New(msg)
	}
}
