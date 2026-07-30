// Command mockm2lx is a stand-in for the M2L-X instance, so that the control
// plane, the reconnect state machine and the whole application can be exercised
// with no cloud instance and no libsrt.
//
// Owner: WP-7. No other work package writes files in this directory.
//
// It serves:
//
//   - POST /api/local_auth/signin — body {"alias": ..., "password": ...}. A body
//     using "username" instead of "alias" must return HTTP 500, because that is
//     what the real instance does and code that gets it wrong should fail here.
//   - GET /api/live_operation/kvs/webrtc_info/{eventId}
//   - GET /api/live_operation/kvs/webrtc_token/{eventId}
//   - the switcher_status WebSocket, token in the URL
//   - an SRT listener that accepts one caller and logs what it receives
//
// The SRT listener is github.com/datarhei/gosrt, a pure-Go implementation. It is
// used HERE AND ONLY HERE. The production path uses GStreamer's srtsink over
// libsrt; gosrt exists in this module so that the mock needs no native
// toolchain, and it must never be imported by anything under internal/.
//
// # Fault injection is the point
//
// Most of this program's value is in reproducing the four failure modes the
// measurements actually found. A mock that only works when everything works is
// not worth building:
//
//  1. drop the SRT session on command
//  2. hold the listener socket open after a disconnect, reproducing M2L-X's
//     roughly five second re-accept refusal window
//  3. stall the status WebSocket, so the fifteen second staleness path is
//     exercised
//  4. report a stream_state that disagrees with reality, so nothing downstream
//     is allowed to treat the lamp as proof
package main

import (
	"fmt"
	"os"

	"github.com/gorilla/websocket"

	srt "github.com/datarhei/gosrt"
)

func main() {
	fmt.Fprintln(os.Stderr, "mockm2lx: not implemented: WP-7")
	os.Exit(1)
}

// Referenced so that `go mod tidy` keeps the frozen dependencies on
// gorilla/websocket and datarhei/gosrt before WP-7 writes the mock. WP-7 deletes
// these lines.
var (
	_ = websocket.Upgrader{}
	_ = srt.Listen
)
