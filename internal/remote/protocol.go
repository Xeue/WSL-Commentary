// Package remote is the authenticated control bridge that lets a SECOND seat —
// a browser on the facility LAN — drive the same frontend the local WebView2
// window runs, without a single line of transport code in the frontend itself.
//
// # Why this package exists at all, and why it is shaped the way it is
//
// The frontend already reaches Go by string-named binding — window.go.main.App
// [method] — and by window.runtime.EventsOn, never by HTTP or a socket of its
// own (see frontend/src/ui/backend.js). A classic <script> injected ahead of
// the deferred module bundle can therefore install a window.go/window.runtime
// that speak WebSocket to this package, and the entire existing frontend takes
// the "real backend" path with no edit. That shim (shim.js, embedded here) is
// the whole frontend side of the bridge; everything else is this Go package.
//
// # Why it is App-agnostic
//
// This package deliberately knows NOTHING about *App, internal/gst or cgo. It
// talks to the application only through the Dispatcher seam (remote.go): the
// application hands it something that can Call a named method and can list the
// Methods a given client is allowed to see. That is what lets the whole package
// build and be tested at Gate A (CGO_ENABLED=0, no GStreamer) against a fake
// dispatcher — which CONTRACT.md requires of every package except internal/gst.
// It is also the boundary that keeps the network attack surface reviewable: the
// method allowlist and the host-only refusals live in the application's
// dispatcher (app_remote.go), NOT in a reflective loop that would silently
// expose the next method somebody binds to Wails.
//
// # The wire protocol
//
// One frame type per direction of concern, discriminated by a "t" tag so the Go
// decoder and the JS shim can never disagree about what a byte string means.
// The set is intentionally tiny: a client CALLs, the server RESULTs, EVENTs are
// pushed, and one HELLO frame per connection tells the shim who it is and,
// crucially, WHICH methods to install. That last list is the mechanism by which
// capability becomes "which functions exist on window.go.main.App" rather than a
// server-side refusal the frontend would have to be taught to expect — which is
// what makes the picture degrade to the KVS mosaic and makes a missing
// StopReturn read as benign in returnpath.js instead of raising a red banner.
package remote

import "encoding/json"

// Frame discriminator values carried in every frame's "t" field.
//
// These strings are a contract shared with shim.js (which switches on
// msg.t). Changing one here without changing the shim silently breaks the
// bridge, so they live in exactly one place on each side and nowhere else.
const (
	// FrameCall is the only client->server frame: an invocation of a bound
	// method by name.
	FrameCall = "call"
	// FrameResult answers exactly one FrameCall, correlated by id.
	FrameResult = "result"
	// FrameEvent is an unsolicited server->client push mirroring a Wails event.
	FrameEvent = "event"
	// FrameHello is sent once, immediately after the socket is authenticated and
	// upgraded, before any event or result.
	FrameHello = "hello"
)

// CallFrame is a client's request to invoke method with args.
//
// Args is a slice of RAW JSON values, one per Go parameter, decoded no further
// here: this package cannot know a method's real Go parameter types without
// importing the application, and it deliberately does not. The Dispatcher — the
// application's hand-written switch — json.Unmarshals each element into the
// concrete type that method expects. Keeping the args opaque at the transport
// is what preserves the App-agnostic boundary; it is also why a malformed
// argument surfaces as a typed error from the method, not a panic in transport.
type CallFrame struct {
	// T is always FrameCall. It is validated on receipt; a frame with any other
	// tag (or none) is rejected rather than guessed at.
	T string `json:"t"`
	// ID correlates this call with its single result. It is chosen by the
	// client and echoed back verbatim; the server never invents ids.
	ID uint64 `json:"id"`
	// Method is the bound method name, e.g. "GetConfig". It is looked up in the
	// dispatcher's ALLOWLIST; an unknown name is refused as unknown even if some
	// Go method of that name exists.
	Method string `json:"method"`
	// Args are the JSON-encoded positional arguments, left raw for the
	// dispatcher to unmarshal into real Go types.
	Args []json.RawMessage `json:"args"`
}

// ResultFrame is the single answer to a CallFrame, correlated by ID.
//
// Exactly one of Value / Error is meaningful, selected by OK. On success OK is
// true and Value carries whatever the method returned (marshalled by
// encoding/json at send time); on failure OK is false and Error carries the
// error's message — never a Go value, never a stack. A refusal (unknown method,
// host-only, insufficient capability) is an ordinary Error result, so the
// client's single rejection path handles policy and genuine faults alike, which
// is exactly how backend.js already treats a rejected Wails promise.
type ResultFrame struct {
	T string `json:"t"`
	// ID echoes the CallFrame this answers.
	ID uint64 `json:"id"`
	// OK selects which of Value / Error is present.
	OK bool `json:"ok"`
	// Value is the method's return value on success. omitempty so a method that
	// returns nothing does not put a null on the wire.
	Value any `json:"value,omitempty"`
	// Error is the human-readable failure message on failure. omitempty so a
	// success frame carries no empty error string that a client might test.
	Error string `json:"error,omitempty"`
}

// EventFrame mirrors one wailsruntime.EventsEmit onto the socket.
//
// Seq is monotonic PER CONNECTION and is the whole reason it exists: the
// per-client outbound queue is bounded and DISCARDS THE OLDEST under back
// pressure (see session.go), exactly as app.go's eventPump does for the local
// renderer. A client that fell behind and lost an intermediate event sees a gap
// in Seq and can decide for itself whether to resynchronise. The events carried
// here — status, sender, return, levels — are edge-triggered displays of a
// current value, so a dropped one costs an intermediate transition, never the
// latest state, but Seq lets the client KNOW a drop happened rather than
// silently believing it saw everything.
type EventFrame struct {
	T string `json:"t"`
	// Seq is a per-connection monotonic counter assigned when the event is
	// enqueued for this client (not when it is broadcast), so a gap corresponds
	// exactly to a drop in this client's queue.
	Seq uint64 `json:"seq"`
	// Name is the Wails event name, e.g. "status".
	Name string `json:"name"`
	// Data is the event payload, marshalled by encoding/json at send time.
	Data any `json:"data"`
}

// HelloFrame is sent once per connection, before any result or event.
//
// Methods is authoritative: the shim installs exactly these functions on
// window.go.main.App and no others. Host-only methods (the SRT picture geometry
// and the SRT return, which own the host's GPU child window and headphones) are
// simply absent from this list, so on a remote client they do not exist to be
// called — the degradation is by omission, not by a refusal the frontend would
// have to be taught to expect. There are no capabilities: the listener is
// unauthenticated, so every connection sees the same method list.
type HelloFrame struct {
	T string `json:"t"`
	// Client is this connection's per-connection id. It is what the shim
	// publishes as window.__wslcommsRemote.client so the frontend can recognise
	// the echo of its OWN SaveConfig (whose EventConfig origin is this same id).
	Client string `json:"client"`
	// Methods is the authoritative allowlist the shim installs on
	// window.go.main.App. Absence is how a method ceases to exist for a client.
	Methods []string `json:"methods"`
	// Events is the set of event names this connection may receive, so a client
	// can wire its subscriptions without guessing.
	Events []string `json:"events"`
}

// frameTag peeks only at the "t" field of an inbound message so the read pump
// can reject anything that is not a call before spending effort decoding it.
//
// The server accepts exactly one inbound frame type (call). Everything else —
// a result, an event, a hello, or a frame with no tag — is a client that is
// either confused or hostile, and is treated as a protocol error that ends the
// connection rather than something to be interpreted.
type frameTag struct {
	T string `json:"t"`
}

// decodeCall validates that raw is a well-formed call frame and returns it.
//
// It is deliberately strict: the tag must be exactly FrameCall and the method
// name must be non-empty. Args may be nil (a zero-argument method) but is never
// invented. Any failure is returned as an error the read pump turns into a
// protocol-level disconnect, because a peer that cannot speak the protocol
// correctly on this socket is not one to keep guessing for.
func decodeCall(raw []byte) (CallFrame, error) {
	var tag frameTag
	if err := json.Unmarshal(raw, &tag); err != nil {
		return CallFrame{}, errProtocol{"malformed frame: " + err.Error()}
	}
	if tag.T != FrameCall {
		return CallFrame{}, errProtocol{"unexpected frame type " + strconvQuote(tag.T)}
	}
	var c CallFrame
	if err := json.Unmarshal(raw, &c); err != nil {
		return CallFrame{}, errProtocol{"malformed call frame: " + err.Error()}
	}
	if c.Method == "" {
		return CallFrame{}, errProtocol{"call frame with empty method"}
	}
	return c, nil
}

// errProtocol marks a wire-level violation distinct from an application error,
// so the session can log it and disconnect rather than answer it.
type errProtocol struct{ msg string }

func (e errProtocol) Error() string { return "remote: protocol error: " + e.msg }

// strconvQuote is a tiny local helper so protocol.go pulls in nothing beyond
// encoding/json. It quotes s the way strconv.Quote would for the common cases
// that appear in a frame tag, which is all this error message needs.
func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
