package m2lx

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newRawWatcher builds a watcher whose dial is under the test's control and
// whose client answers with tok. It makes no network connection.
func newRawWatcher(tok string, dial func(ctx context.Context, urlStr string) (wsConn, error)) *watcher {
	w := newWatcher("m2lx.example.com", &fakeClientToken{token: tok})
	w.dial = dial
	return w
}

// TestRawSnapshotReturnsTheOpeningFrame is the property the mixer drawer's
// freshness gate rests on: one dial, one whole document, no cache.
func TestRawSnapshotReturnsTheOpeningFrame(t *testing.T) {
	frame := []byte(`{"status":[{"node":"advanced_audio_mixer","path":"/","state":{"matrix":{}}}],"timestamp":1785522083212}`)

	var dials int32
	w := newRawWatcher("tok", func(ctx context.Context, _ string) (wsConn, error) {
		atomic.AddInt32(&dials, 1)
		c := newFakeConn()
		c.push(frame)
		return c, nil
	})

	for i := 1; i <= 3; i++ {
		got, err := w.RawSnapshot(context.Background())
		if err != nil {
			t.Fatalf("RawSnapshot() call %d error = %v", i, err)
		}
		if string(got) != string(frame) {
			t.Fatalf("RawSnapshot() call %d = %s, want the frame verbatim", i, got)
		}
		if n := atomic.LoadInt32(&dials); int(n) != i {
			t.Fatalf("after %d calls there had been %d dial(s); a complete document exists once per connection, so a cache here would be stale", i, n)
		}
	}
}

// TestRawSnapshotSkipsSubtreeDeltas. A delta is one subtree of one node. Handed
// to mixer.ParseSnapshot it would be read as that node's entire state, which
// for the mixer means an empty routing matrix — "nothing is in the clean feed".
func TestRawSnapshotSkipsSubtreeDeltas(t *testing.T) {
	delta := []byte(`{"status":[{"node":"advanced_audio_mixer","path":"/levels","state":{"aux1":[-100,-100]}}],"timestamp":1}`)
	stats := []byte(`{"status":[{"node":"cam1","path":"/statistics","state":{"bitrate":6523.6}}],"timestamp":2}`)
	snapshot := []byte(`{"status":[{"node":"cam1","path":"/","state":{"stream_state":"streaming"}}],"timestamp":3}`)

	w := newRawWatcher("tok", func(ctx context.Context, _ string) (wsConn, error) {
		c := newFakeConn()
		c.push(delta)
		c.push(stats)
		c.push(snapshot)
		return c, nil
	})

	got, err := w.RawSnapshot(context.Background())
	if err != nil {
		t.Fatalf("RawSnapshot() error = %v", err)
	}
	if string(got) != string(snapshot) {
		t.Fatalf("RawSnapshot() = %s, want the whole-document frame, not a delta", got)
	}
}

// TestRawSnapshotRefusesWithoutAToken. Both sockets carry the bearer token in
// their URL, so there is nothing to open before sign-in has succeeded.
func TestRawSnapshotRefusesWithoutAToken(t *testing.T) {
	w := newRawWatcher("", func(context.Context, string) (wsConn, error) {
		t.Error("a connection was dialled with no bearer token")
		return nil, errors.New("unreachable")
	})
	if _, err := w.RawSnapshot(context.Background()); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("RawSnapshot() with no token error = %v, want %v", err, ErrNotSignedIn)
	}
}

// TestRawSnapshotReportsADialFailureWithoutTheToken. The URL carries the token
// in its query string and gorilla may quote the URL it failed on.
func TestRawSnapshotHidesTheTokenInErrors(t *testing.T) {
	const secret = "eyJhbGciOi.super+secret/token=="

	t.Run("a dial failure", func(t *testing.T) {
		w := newRawWatcher(secret, func(_ context.Context, urlStr string) (wsConn, error) {
			// The dialler is what would quote the URL, so the fake does too.
			return nil, errors.New("dial " + urlStr + ": connection refused")
		})
		_, err := w.RawSnapshot(context.Background())
		if err == nil {
			t.Fatal("RawSnapshot() succeeded against a dial that failed")
		}
		assertNoToken(t, err, secret)
	})

	t.Run("a read failure", func(t *testing.T) {
		w := newRawWatcher(secret, func(context.Context, string) (wsConn, error) {
			c := newFakeConn()
			c.Close() // the next ReadMessage errors
			return c, nil
		})
		if _, err := w.RawSnapshot(context.Background()); err == nil {
			t.Fatal("RawSnapshot() succeeded against a connection that was already closed")
		}
	})
}

func assertNoToken(t *testing.T, err error, token string) {
	t.Helper()
	msg := err.Error()
	if strings.Contains(msg, token) {
		t.Fatalf("the error carries the bearer token verbatim: %q", msg)
	}
	// The escaped form is what actually appears in a quoted URL, and is the one
	// a naive redaction misses.
	escaped := "eyJhbGciOi.super%2Bsecret%2Ftoken%3D%3D"
	if strings.Contains(msg, escaped) {
		t.Fatalf("the error carries the percent-encoded bearer token: %q", msg)
	}
}

// TestRawSnapshotGivesUpOnDeltasOnly distinguishes "the socket is silent" from
// "the socket is delivering, but only deltas". They need different fixes, so
// they get different errors.
func TestRawSnapshotGivesUpOnDeltasOnly(t *testing.T) {
	delta := []byte(`{"status":[{"node":"advanced_audio_mixer","path":"/levels","state":{}}],"timestamp":1}`)

	w := newRawWatcher("tok", func(ctx context.Context, _ string) (wsConn, error) {
		c := newFakeConn()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				c.push(delta)
			}
		}()
		return c, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, err := w.RawSnapshot(ctx)
	if !errors.Is(err, ErrNoSnapshotFrame) {
		t.Fatalf("RawSnapshot() against a delta-only socket error = %v, want %v", err, ErrNoSnapshotFrame)
	}
}

// TestRawSnapshotReportsSilenceAsATimeout is the other half of the pair above:
// nothing arrived at all, which is a connection problem rather than a protocol
// one.
func TestRawSnapshotReportsSilenceAsATimeout(t *testing.T) {
	w := newRawWatcher("tok", func(context.Context, string) (wsConn, error) {
		return newFakeConn(), nil // never pushes
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := w.RawSnapshot(ctx)
	if err == nil {
		t.Fatal("RawSnapshot() succeeded against a silent socket")
	}
	if errors.Is(err, ErrNoSnapshotFrame) {
		t.Fatalf("a silent socket reported %v, which says frames were arriving", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RawSnapshot() against a silent socket error = %v, want a deadline error", err)
	}
}

// TestHasWholeNodeEntryAgainstTheCapturedFrames pins the snapshot-versus-delta
// test against the real captures rather than against this file's idea of them.
func TestHasWholeNodeEntryAgainstTheCapturedFrames(t *testing.T) {
	tests := []struct {
		file string
		want bool
	}{
		{"testdata/switcher_status-live-2026-07-31.json", true},
		{"testdata/switcher_status-delta-levels-2026-08-01.json", false},
		{"testdata/switcher_status-delta-statistics-2026-08-01.json", false},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			data, err := os.ReadFile(tt.file)
			if err != nil {
				t.Skipf("fixture unavailable: %v", err)
			}
			if got := hasWholeNodeEntry(data); got != tt.want {
				t.Errorf("hasWholeNodeEntry(%s) = %v, want %v", tt.file, got, tt.want)
			}
		})
	}
}

func TestHasWholeNodeEntryRejectsRubbish(t *testing.T) {
	for _, raw := range []string{"", "null", "[]", `{"status":"not an array"}`, `{"nostatus":[]}`, `{"status":[1,2,3]}`} {
		if hasWholeNodeEntry([]byte(raw)) {
			t.Errorf("hasWholeNodeEntry(%q) = true, want false", raw)
		}
	}
}
