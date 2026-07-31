// controller.go is WP-M1: the switcher_controller WebSocket client and the
// wire shape of every Command.
//
// It owns the only code in this application that can change a live mixer, so
// the ordering of concerns here is deliberate: validate, then serialise, then
// write, and never the other way round.
//
// # What the socket actually is, MEASURED on the live dev event
//
// On 2026-07-31, against m2lx-wslstudios-matcht.etapsiota.com:
//
//   - A dial to wss://<host>/api/v1/switcher_controller?access_token=<percent
//     -encoded> completes the HTTP 101 upgrade and the socket stays open.
//   - With a bad token the upgrade is refused with HTTP 401 before any
//     WebSocket exists. A dial failure is therefore authoritative about
//     credentials, which is why NewController surfaces the first dial error to
//     the caller instead of retrying quietly behind it.
//   - The peer pushed NOTHING for 45 seconds on an idle connection, and the
//     socket remained open throughout. There is no unsolicited status push and
//     no application-level heartbeat on this socket. It is not the telemetry
//     socket; switcher_status is (internal/m2lx).
//
// # Why there is no correlation of responses, stated plainly
//
// The task of correlating a reply to the command that caused it cannot be done
// honestly from what has been measured, so this file does not pretend to:
//
//   - Nothing arrives unsolicited (measured above), so an idle observation
//     cannot reveal a response shape.
//   - Observing a response requires SENDING a command, and this work package
//     was prohibited from writing to the live mixer. No set_* command was sent.
//   - docs/architecture.md line 81 records, from an earlier session, that
//     set_ch_fader and set_output_fader writes were "ACKed 200 but silently
//     dropped". So some form of acknowledgement probably exists — but its
//     envelope, whether it carries any request identifier, and whether the
//     vocabulary even accepts one, are all UNVERIFIED.
//
// Inventing a request id and matching replies to it would produce code that
// looks like it verifies writes and does not. Instead: every inbound frame is
// read, retained and exposed by (*WSController).PeerMessages, flagged if it
// looks like an error, and NEVER attributed to a particular Send. Confirmation
// that a write took effect comes from reading the next switcher_status frame
// back through ParseSnapshot — see the Controller.Send contract, and note that
// at least two commands in this vocabulary are known to be accepted and then
// silently ignored.
package mixer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// NodeName is the DSP node every command in this package addresses.
//
// It is the "node" member of the command envelope and the "node" of the
// switcher_status entry the read path parses, so the write path and the read
// path are provably talking about the same object.
const NodeName = "advanced_audio_mixer"

// controllerPath is the WebSocket path of the control socket. The token is a
// query parameter on this path, never a header: MEASURED, the upgrade succeeds
// with access_token in the query string and is refused 401 without it.
const controllerPath = "/api/v1/switcher_controller"

// dialTimeout bounds a single connection attempt, including the TLS handshake
// and the HTTP 101 upgrade.
//
// Not a measured constant. The live upgrade completed well inside a second;
// this is a generous ceiling chosen so a black-holed host fails in a time an
// operator will wait through rather than hanging the arm action.
const dialTimeout = 10 * time.Second

// writeTimeout bounds a single WriteMessage.
//
// It exists because a TCP connection to a vanished peer accepts writes into
// the kernel buffer and then blocks forever once it fills. Without a deadline
// a Send with no context deadline would hang an operator's arm-gated action
// indefinitely; with one it fails, the connection is dropped, and the
// supervisor reconnects.
const writeTimeout = 5 * time.Second

// maxPeerMessages is how many inbound frames PeerMessages retains.
//
// The peer is measured to push nothing, so anything at all arriving is
// noteworthy and a small ring is ample. It is bounded rather than unbounded
// because this process runs for a whole match and an unexpectedly chatty peer
// must not be able to grow this slice without limit.
const maxPeerMessages = 16

// ErrClosed is returned by Send after Close.
//
// It is wrapped in a *BatchError, so callers may test for it with errors.Is
// while still reading how much of the batch was written — which, for a closed
// controller, is always none.
var ErrClosed = errors.New("mixer: controller is closed")

// ErrDisarmed is returned by Send when no write window is open.
//
// Like ErrClosed it is wrapped in a *BatchError with Written 0, because a
// refused batch and a half-written one must be distinguishable by a caller
// deciding whether to re-read switcher_status.
var ErrDisarmed = errors.New("mixer: controller is disarmed; call Arm before writing to a live mixer")

// ArmWindow is the write window Arm is expected to be given, and the value a
// caller should use unless it has a reason not to.
//
// TWO MINUTES, and here is the reasoning, because a number chosen carelessly
// here is either a gate that is not a gate or a gate that fires mid-gesture on
// a live desk.
//
// What has to fit inside it: an operator arms, reads a 54-row matrix, stages
// one or more crosspoints, presses Apply, and waits for the change to be
// confirmed from a following switcher_status frame. The drawer's own
// confirmation window is 4 s (four frames at the measured ~1 Hz) and its
// two-press golden confirm is 5 s, so the gesture itself is seconds. Two
// minutes is roughly an order of magnitude of headroom over the slowest
// realistic version of it — including a reconnect, whose backoff ladder caps
// at 10 s and whose acquire wait is unbounded by design.
//
// What must NOT fit inside it: a break in play, a walk to the machine room, a
// handover. Those are the states in which an armed write path is a hazard
// nobody is watching, and two minutes is far shorter than any of them.
//
// Why a duration and not a latch: re-arming costs one call, so an operator who
// overruns loses a click. An over-long or never-clearing window is not
// recoverable by anyone, because nothing in the system notices it is open.
//
// The expiry is evaluated AT THE MOMENT OF THE WRITE against a stored
// deadline, not by a timer goroutine. A timer can be delayed by a suspended or
// heavily loaded process and leave the gate open past its deadline; comparing
// a deadline cannot. It also means there is no goroutine to leak and no race
// between a firing timer and a Send already in flight.
const ArmWindow = 2 * time.Minute

// armGate is the Go half of the two-gate write path.
//
// It is EMBEDDED in the Controller rather than sitting beside it so that a
// binding cannot be handed the socket without it — see the Controller
// interface comment. Its zero value is disarmed, which is the only safe
// default: a Controller that has just been dialled must not be able to write.
type armGate struct {
	mu sync.Mutex
	// until is the instant the window closes. The zero Time means disarmed,
	// and a past value means expired — both are refused by armedAt, so
	// expiry needs no timer and no cleanup.
	until time.Time
}

// arm opens the window and returns its deadline. A non-positive window
// disarms.
func (g *armGate) arm(window time.Duration) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	if window <= 0 {
		g.until = time.Time{}
		return time.Time{}
	}
	g.until = time.Now().Add(window)
	return g.until
}

// disarm shuts the window immediately.
func (g *armGate) disarm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.until = time.Time{}
}

// armedUntil reports the deadline, or the zero Time once it has passed. An
// expired window reads as disarmed rather than as a stale deadline, so a
// caller cannot mistake "it ran out four minutes ago" for "it is open".
func (g *armGate) armedUntil() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.until.IsZero() || !time.Now().Before(g.until) {
		return time.Time{}
	}
	return g.until
}

// armedAt reports whether a write happening now is permitted, and says why not
// when it is not. The reason names the expiry explicitly, because "disarmed"
// and "armed four minutes ago and it lapsed" are different things to an
// operator whose routing change did not go out.
func (g *armGate) armedAt(now time.Time) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.until.IsZero() {
		return false, ErrDisarmed
	}
	if !now.Before(g.until) {
		return false, fmt.Errorf("%w (the write window opened earlier and closed at %s; arm again)", ErrDisarmed, g.until.Format(time.RFC3339))
	}
	return true, nil
}

// ============================================================================
// Command envelopes
// ============================================================================

// knownBuses is the set of buses a command may name, derived from AllBuses so
// that it cannot drift from the contract.
//
// A command naming a bus that does not exist is not rejected by the mixer: it
// is accepted and addresses nothing. That is the same silent-success failure
// mode as the SetCompLimit AGC trap, so unknown bus names are rejected here,
// before transmission, rather than discovered by a read-back that shows no
// change and gives no reason.
var knownBuses = func() map[Bus]bool {
	m := make(map[Bus]bool, len(AllBuses))
	for _, b := range AllBuses {
		m[b] = true
	}
	return m
}()

// envelope assembles the command envelope shared by every command:
//
//	{"node":"advanced_audio_mixer","command":"<name>","args":...}
//
// args is deliberately any, not map[string]any: the vocabulary is not uniform.
// set_input_muted and set_output_muted take an ARRAY of per-target objects,
// while set_routing and set_comp_limit take a single object. Forcing one shape
// on both is exactly the mistake that returns HTTP 400 from set_output_muted.
func envelope(command string, args any) map[string]any {
	return map[string]any{
		"node":    NodeName,
		"command": command,
		"args":    args,
	}
}

// checkBusSet validates a set of bus names destined for the wire.
//
// It rejects unknown names (see knownBuses) and duplicates. A duplicate is
// harmless to the mixer but means the caller's model of the routing set is
// wrong, and this is the write path to a clean feed: a caller that does not
// know what it is asking for should not be transmitting.
func checkBusSet(field string, buses []Bus) error {
	seen := make(map[Bus]bool, len(buses))
	for i, b := range buses {
		if !knownBuses[b] {
			return fmt.Errorf("mixer: %s[%d]: unknown bus %q; valid buses are %s", field, i, string(b), busListForError())
		}
		if seen[b] {
			return fmt.Errorf("mixer: %s[%d]: bus %q appears more than once", field, i, string(b))
		}
		seen[b] = true
	}
	return nil
}

// busListForError renders the valid bus names for an error message, in
// AllBuses order so the text is stable between runs.
func busListForError() string {
	names := make([]string, 0, len(AllBuses))
	for _, b := range AllBuses {
		names = append(names, string(b))
	}
	return strings.Join(names, ", ")
}

// busStrings converts buses to wire strings for marshalling.
//
// It returns a non-nil empty slice for an empty input so that the JSON is [],
// never null. An "outputs":null would be a different message from
// "outputs":[], and only one of them is the un-route the caller asked for.
func busStrings(buses []Bus) []string {
	out := make([]string, 0, len(buses))
	for _, b := range buses {
		out = append(out, string(b))
	}
	return out
}

// Envelope renders set_routing.
//
// MEASURED shape:
//
//	{"node":"advanced_audio_mixer","command":"set_routing",
//	 "args":{"matrix":"output","input":"cam22-1","outputs":["master","aux1"]}}
//
// Note that "input" carries a STRIP name, not an input name, and that this is
// a REPLACE: Outputs is the complete resulting set.
//
// Rejections, all of them because the mixer would accept the message and do
// something other than what the caller meant:
//
//   - nil Outputs. The contract requires it: nil is how Go spells "I did not
//     set this field", and treating it as an empty set would un-route the strip
//     from programme. An explicit, non-nil empty slice is accepted, because
//     that is a caller stating deliberately that the strip goes nowhere.
//   - an unknown or duplicated bus name (see checkBusSet).
//   - a Matrix outside the measured enum. "MAIN" appears in an older bundle
//     transcription (docs/architecture.md line 434) and is NOT the wire value;
//     sending it would address no matrix at all.
func (c SetRouting) Envelope() (map[string]any, error) {
	switch c.Matrix {
	case MatrixOutput, MatrixPFL:
	case "":
		return nil, errors.New("mixer: SetRouting: Matrix is empty; want \"output\" or \"pfl\"")
	default:
		return nil, fmt.Errorf("mixer: SetRouting: unknown matrix %q; want %q or %q", string(c.Matrix), string(MatrixOutput), string(MatrixPFL))
	}
	if c.Strip == "" {
		return nil, errors.New("mixer: SetRouting: Strip is empty; want a strip name such as \"cam22-1\"")
	}
	if c.Outputs == nil {
		return nil, fmt.Errorf("mixer: SetRouting: Outputs is nil for strip %q; set_routing REPLACES the routing set, so nil is not \"leave unchanged\" — pass the complete set of buses the strip should keep, or an explicit empty slice to un-route it entirely", c.Strip)
	}
	if err := checkBusSet("SetRouting.Outputs", c.Outputs); err != nil {
		return nil, err
	}
	return envelope("set_routing", map[string]any{
		"matrix":  string(c.Matrix),
		"input":   c.Strip,
		"outputs": busStrings(c.Outputs),
	}), nil
}

// Envelope renders set_input_muted, in the ARRAY form.
//
// THE ARGUMENT SHAPE IS NOW SETTLED, and the type comment on SetInputMuted in
// mixer.go — which still says it is not measured — is stale. The evidence is
// Sony's own bundle, transcribed at docs/architecture.md line 430:
//
//	{"node":"advanced_audio_mixer","command":"set_input_muted",
//	 "args":[{"name":"cam23-1","muted":true}]}
//
// So both *_muted commands take an array of {name,muted} objects, and the
// single-strip case is still a one-element array. It was NOT re-confirmed
// against the live mixer by this work package, because doing so requires
// muting a strip on a live event and this work package was prohibited from
// writing. The evidence class is [C] (read out of the vendor's code), not [E]
// (measured against the device); if the write path is ever armed and this
// command returns HTTP 400, the object form is the thing to try next.
func (c SetInputMuted) Envelope() (map[string]any, error) {
	if c.Strip == "" {
		return nil, errors.New("mixer: SetInputMuted: Strip is empty; want a strip name such as \"cam22-1\"")
	}
	return envelope("set_input_muted", []map[string]any{{
		"name":  c.Strip,
		"muted": c.Muted,
	}}), nil
}

// Envelope renders set_output_muted, in the measured ARRAY form.
//
// MEASURED, and the object form returns HTTP 400:
//
//	{"node":"advanced_audio_mixer","command":"set_output_muted",
//	 "args":[{"name":"aux1","muted":true}]}
//
// A single bus is still sent as a one-element array; this is not simplified
// back to an object. Muted applies to every bus in Buses, which is why each
// array element repeats it.
//
// An empty Buses is rejected rather than sent as an empty array. An empty
// array is a message that does nothing, and a Send that returns nil having
// done nothing is indistinguishable, to the caller, from one that worked —
// on the code path whose whole risk is silent no-ops.
func (c SetOutputMuted) Envelope() (map[string]any, error) {
	if len(c.Buses) == 0 {
		return nil, errors.New("mixer: SetOutputMuted: Buses is empty; name at least one bus")
	}
	if err := checkBusSet("SetOutputMuted.Buses", c.Buses); err != nil {
		return nil, err
	}
	args := make([]map[string]any, 0, len(c.Buses))
	for _, b := range c.Buses {
		args = append(args, map[string]any{
			"name":  string(b),
			"muted": c.Muted,
		})
	}
	return envelope("set_output_muted", args), nil
}

// Envelope renders set_ch_fader.
//
//	{"node":"advanced_audio_mixer","command":"set_ch_fader",
//	 "args":{"name":"cam22-1","gain":[-1.6,-1.6]}}
//
// THE ARGUMENT SHAPE IS UNVERIFIED and this doc comment says so rather than
// implying otherwise. What is known:
//
//   - gain is in dB and MuteFaderDB (-144) is mute. [E]
//   - the READ side reports it per channel: the live frame has
//     fader["cam22-1"].ch_fader.gain = [-1.574803113937378,-1.574803113937378].
//     The pair sent here mirrors the pair read back, which is the shape most
//     likely to round-trip.
//   - the object form (rather than the array form used by the two *_muted
//     commands) follows set_comp_limit, the other per-strip configuration
//     command, whose object shape IS measured.
//
// It could not be confirmed against the device: confirming it means moving a
// fader on a live event, and this work package was prohibited from writing.
//
// TWO REASONS A NIL ERROR FROM Send IS NOT CONFIRMATION HERE, both recorded:
//
//   - docs/architecture.md line 81: set_ch_fader and set_output_fader writes
//     were repeatedly "ACKed 200 but silently dropped". [E]
//   - the read side carries ch_fader.enabled per channel, and MEASURED, most
//     strips in the live frame report enabled [false,false] while cam22-1 —
//     the one strip somebody had actually touched — reports [true,true]. A
//     disabled fader plausibly ignores a gain, which would explain the dropped
//     writes above. The SetChFader contract type carries no way to set
//     enabled, so on a strip whose fader is out of circuit this command may be
//     inert by construction. HYPOTHESIS, untested, and reported to the
//     contract owner rather than worked around here.
//
// Read the fader back from the next switcher_status frame. Always.
func (c SetChFader) Envelope() (map[string]any, error) {
	if c.Strip == "" {
		return nil, errors.New("mixer: SetChFader: Strip is empty; want a strip name such as \"cam22-1\"")
	}
	return envelope("set_ch_fader", map[string]any{
		"name": c.Strip,
		"gain": []float64{c.Gain[0], c.Gain[1]},
	}), nil
}

// Envelope renders set_comp_limit.
//
// MEASURED shape:
//
//	{"node":"advanced_audio_mixer","command":"set_comp_limit",
//	 "args":{"name":"cam23-1","agc_mode":"limiter","pre_gain":0,
//	         "compressor_th":0,"limiter_th":-3}}
//
// THE TRAP, and the one rejection in this file that is not about malformed
// input: a LimiterTh is SILENTLY INERT unless AGCMode is AGCLimiter. The
// command is accepted, the value is stored, and switcher_status reads it back
// unchanged — so the read-back check that catches every other mistake in this
// package cannot catch this one. The state will show exactly what was sent and
// no limiting will happen.
//
// A SetCompLimit carrying a LimiterTh with any other AGCMode is therefore
// refused before transmission. Note the shape of that rule: it is keyed on
// LimiterTh being MEANT, not on the mode. Turning a limiter off is legitimate
// and stays legal — AGCOff with a NIL LimiterTh is the factory state of every
// strip in the live frame, and it renders exactly as measured, including the
// vendor bundle's own agc_mode:"off" example.
//
// WHY "MEANT" IS A NIL CHECK AND NOT A ZERO CHECK. It used to be
// `c.LimiterTh != 0`, which had a hole exactly where it hurts: 0 dBFS is a
// real threshold — a brickwall at digital full scale — and it is the natural
// thing to reach for on the very bus that is MEASURED to sum past full scale
// (BusMaster, two sources at -5 dBFS summing to +1 dBFS with a -27 dB
// distortion residual). A caller asking for that with AGCOff passed the
// guard, was accepted by the mixer, read the value back unchanged, and did no
// limiting whatsoever. The one command that mitigates the clipping was the one
// command that could silently not run. SetCompLimit.LimiterTh is now a
// pointer, so "no limiter" (nil) and "limiter at 0 dBFS" (LimiterAt(0)) are
// different values and this guard can tell them apart.
//
// The converse is refused too: AGCLimiter with a nil LimiterTh would arm the
// limiter and leave its threshold at whatever limiter_th 0 means, which is a
// brickwall nobody asked for. Arming a limiter without saying where it clamps
// is the same guess in the other direction.
func (c SetCompLimit) Envelope() (map[string]any, error) {
	if c.Strip == "" {
		return nil, errors.New("mixer: SetCompLimit: Strip is empty; want a strip name such as \"cam23-1\"")
	}
	switch c.AGCMode {
	case AGCOff, AGCLimiter:
	case "":
		return nil, fmt.Errorf("mixer: SetCompLimit: AGCMode is empty for strip %q; want %q or %q", c.Strip, string(AGCOff), string(AGCLimiter))
	default:
		return nil, fmt.Errorf("mixer: SetCompLimit: unknown AGCMode %q for strip %q; want %q or %q", string(c.AGCMode), c.Strip, string(AGCOff), string(AGCLimiter))
	}
	if c.LimiterTh != nil && c.AGCMode != AGCLimiter {
		return nil, fmt.Errorf("mixer: SetCompLimit: strip %q sets LimiterTh %g dBFS with AGCMode %q; the limiter threshold is SILENTLY INERT unless AGCMode is %q — the mixer stores the value and reads it back unchanged while doing no limiting, so a read-back check will not catch this. Set AGCMode to %q, or leave LimiterTh nil for no limiter",
			c.Strip, *c.LimiterTh, string(c.AGCMode), string(AGCLimiter), string(AGCLimiter))
	}
	if c.LimiterTh == nil && c.AGCMode == AGCLimiter {
		return nil, fmt.Errorf("mixer: SetCompLimit: strip %q selects AGCMode %q with no LimiterTh; that would arm the limiter and send limiter_th 0, a brickwall at digital full scale the caller did not ask for. Say the threshold with LimiterAt, e.g. LimiterAt(-3)",
			c.Strip, string(AGCLimiter))
	}
	// nil serialises as limiter_th 0, which is the factory state and the
	// measured shape of the vendor's own agc_mode:"off" example. The wire is
	// identical either way; the distinction lives entirely in the guards
	// above, because the mixer cannot make it.
	limiterTh := 0.0
	if c.LimiterTh != nil {
		limiterTh = *c.LimiterTh
	}
	return envelope("set_comp_limit", map[string]any{
		"name":          c.Strip,
		"agc_mode":      string(c.AGCMode),
		"pre_gain":      c.PreGain,
		"compressor_th": c.CompressorTh,
		"limiter_th":    limiterTh,
	}), nil
}

// commandName reports the wire command name of a Command for error messages,
// without rendering its arguments.
//
// Arguments are deliberately excluded: an error string may reach a log or an
// operator's screen, and the useful fact is which command failed, not a replay
// of a message that did not go out.
func commandName(cmd Command) string {
	switch cmd.(type) {
	case SetRouting:
		return "set_routing"
	case SetInputMuted:
		return "set_input_muted"
	case SetOutputMuted:
		return "set_output_muted"
	case SetChFader:
		return "set_ch_fader"
	case SetCompLimit:
		return "set_comp_limit"
	default:
		return "unknown command"
	}
}

// ============================================================================
// Errors and observability
// ============================================================================

// BatchError reports a Send that did not complete, and exactly how far it got.
//
// It exists because a half-applied batch is worse than a failed one. If a
// caller removes a strip from the clean feed with one command and re-arms its
// limiter with the next, a bare error tells them nothing about which of those
// two states the mixer is now in — and the routing of a live clean feed is not
// a thing to guess at. Written says precisely how many commands reached the
// socket, so the caller can report "commands 1..Written were sent; the rest
// were not" and re-read switcher_status to confirm.
//
// Send returns either nil or a *BatchError, never a bare error, so a caller may
// always type-assert. Use errors.Is against ErrClosed, context.Canceled and so
// on; Unwrap exposes the cause.
type BatchError struct {
	// Index is the 0-based position, within the cmds passed to Send, of the
	// command that failed.
	Index int

	// Total is len(cmds), carried so a message can say "3 of 5" without the
	// caller re-counting.
	Total int

	// Written is how many commands were handed to the socket before the
	// failure — that is, commands [0,Written) of the batch.
	//
	// It is NOT the number that took effect. A written command may still be
	// rejected or silently ignored by the mixer; see Controller.Send. When the
	// failure was a validation or serialisation failure this is 0, because
	// Send builds every envelope before writing any of them.
	Written int

	// Command is the wire command name of the failing command, e.g.
	// "set_routing". Arguments are not carried: see commandName.
	Command string

	// Err is the underlying cause.
	Err error
}

// Error renders the failure with the applied/not-applied split spelled out, so
// that a bare log line is still actionable.
func (e *BatchError) Error() string {
	return fmt.Sprintf("mixer: send failed at command %d of %d (%s): %v; %d of %d command(s) were written to the mixer and are NOT rolled back",
		e.Index+1, e.Total, e.Command, e.Err, e.Written, e.Total)
}

// Unwrap exposes the cause for errors.Is and errors.As.
func (e *BatchError) Unwrap() error { return e.Err }

// PeerMessage is one frame the mixer sent to us, retained rather than dropped.
//
// Nothing is expected on this socket: MEASURED, an idle connection to the live
// dev event received no frames in 45 seconds. So anything here is by
// definition unexpected and worth keeping.
//
// A PeerMessage is NOT attributed to a command. See the file comment for why
// no correlation scheme exists.
type PeerMessage struct {
	// At is the local time the frame was read.
	At time.Time

	// Payload is the raw frame, truncated to nothing and interpreted as
	// nothing. It is kept verbatim so that whatever the device says can be
	// reported to Sony as-is.
	Payload []byte

	// LooksLikeError reports whether the frame matched errorish.
	//
	// It is a HEURISTIC over a response shape that has never been observed —
	// see the file comment — and a false here means "did not match a pattern",
	// not "this is fine".
	LooksLikeError bool
}

// errorish reports whether a frame looks like an error response.
//
// The response envelope of this socket has never been observed (file comment),
// so this cannot be a parser for a known shape. It looks for the members that
// M2L-X's REST surface uses to report failure — "detail" is the field in
// {"detail":"Output source is invalid."} — plus any numeric status or code at
// or above 400, which is the convention the same API follows elsewhere.
//
// A frame that is not a JSON object is not an error by this test, but is still
// retained as a PeerMessage.
func errorish(payload []byte) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return false
	}
	for _, k := range []string{"error", "errors", "detail", "details", "message_id", "status_message_id"} {
		if _, ok := obj[k]; ok {
			return true
		}
	}
	for _, k := range []string{"status", "code", "status_code"} {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		var n float64
		if err := json.Unmarshal(raw, &n); err == nil && n >= 400 {
			return true
		}
	}
	return false
}

// ============================================================================
// Token redaction
// ============================================================================

// redactor scrubs a bearer token out of a string.
//
// The token is in the URL query, so it can reach an error message through any
// library that quotes the URL it failed on — net/url, net/http and x/net all
// do this in at least one path. Rather than audit every dependency for that,
// every error leaving this package is passed through here.
//
// Two rules, because either alone has a hole: the literal token is removed
// wherever it appears (covering a bare quote of the token), and any
// access_token=<value> is truncated (covering a value that was re-encoded, and
// so no longer matches the literal, on its way into the message).
type redactor struct {
	token string
}

// redactedToken is what replaces a token in any string this package emits.
const redactedToken = "REDACTED"

// scrub returns s with the token removed. It is safe with an empty token: an
// empty needle would otherwise match everywhere.
func (r redactor) scrub(s string) string {
	if r.token != "" {
		s = strings.ReplaceAll(s, r.token, redactedToken)
		s = strings.ReplaceAll(s, url.QueryEscape(r.token), redactedToken)
	}
	return scrubAccessTokenParam(s)
}

// err wraps an error so that its message can never carry the token.
//
// The wrapper is a plain error carrying a scrubbed string rather than the
// original error, deliberately: an unwrappable chain would let a caller print
// the cause and defeat the redaction. Sentinel identity that this package's own
// callers need — ErrClosed, context errors — is preserved by redactedError
// keeping the cause for errors.Is while refusing to render it.
func (r redactor) err(e error) error {
	if e == nil {
		return nil
	}
	return &redactedError{msg: r.scrub(e.Error()), cause: e}
}

// redactedError is an error whose message has been scrubbed of a token.
//
// It keeps the cause reachable through errors.Is/errors.As so that
// errors.Is(err, context.DeadlineExceeded) still works, but Error() renders
// only the scrubbed text. Unwrap is intentionally NOT implemented; Is and As
// are, so identity checks work while fmt cannot walk to an unscrubbed message.
type redactedError struct {
	msg   string
	cause error
}

// Error returns the scrubbed message.
func (e *redactedError) Error() string { return e.msg }

// Is reports whether the wrapped cause matches target, so that sentinel checks
// survive redaction.
func (e *redactedError) Is(target error) bool { return errors.Is(e.cause, target) }

// As delegates to the wrapped cause, so typed inspection survives redaction.
// Callers that do this take responsibility for what they then print.
func (e *redactedError) As(target any) bool { return errors.As(e.cause, target) }

// scrubAccessTokenParam truncates the value of any access_token query
// parameter appearing in s.
//
// It scans rather than using a regexp so that the terminator set is explicit:
// a token value ends at &, whitespace, a quote or the end of the string.
func scrubAccessTokenParam(s string) string {
	const key = "access_token="
	var b strings.Builder
	for {
		i := strings.Index(s, key)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i+len(key)])
		rest := s[i+len(key):]
		end := strings.IndexAny(rest, "&\"' \t\r\n")
		if end < 0 {
			end = len(rest)
		}
		if end > 0 {
			b.WriteString(redactedToken)
		}
		s = rest[end:]
	}
}

// ============================================================================
// Transport
// ============================================================================

// wsConn is the subset of *websocket.Conn this package uses.
//
// It exists so the lifecycle can be tested against a substitute connection as
// well as a real one; *websocket.Conn satisfies it directly. gorilla permits
// one concurrent reader and one concurrent writer, and this package respects
// that: the reader goroutine is the only caller of ReadMessage, and writeMu
// makes Send the only caller of WriteMessage.
type wsConn interface {
	WriteMessage(messageType int, data []byte) error
	ReadMessage() (messageType int, p []byte, err error)
	SetWriteDeadline(t time.Time) error
	Close() error
}

// dialFunc opens one connection. Overridden in tests.
type dialFunc func(ctx context.Context, urlStr string) (wsConn, error)

// reconnectBackoff is the ladder used after the socket drops or a redial
// fails.
//
// Not a measured constant, and deliberately the same ladder internal/m2lx uses
// for switcher_status: there is no published guidance and no re-accept window
// to clear on a WebSocket, so the requirement is only that a flapping peer is
// not hammered. This socket is not in the transmission path — no audio and no
// video crosses it — so a slow reconnect degrades the drawer's write path and
// nothing else.
var reconnectBackoff = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
}

// reconnectBackoffCap is the ceiling the ladder holds at once exhausted.
const reconnectBackoffCap = 10 * time.Second

// WSController is the concrete Controller returned by NewController.
//
// It is exported so that a caller may type-assert to reach PeerMessages, which
// is the only way to see what the mixer has said to us; the Controller
// interface deliberately carries only the two methods every consumer needs.
// Callers must not depend on it beyond that.
//
// It is safe for concurrent use. Concurrent Sends are serialised whole-batch
// against each other, so two batches never interleave on the wire — a batch
// that corrects a routing and then arms a limiter cannot be split by another
// caller's batch landing between the two.
type WSController struct {
	// urlStr carries the token. It is never logged, never embedded in an
	// error, and never returned. Everything user-visible goes through red.
	urlStr string
	red    redactor

	dial    dialFunc
	backoff []time.Duration

	// gate is the Go-side arm gate. EMBEDDED, not a collaborator held
	// alongside: there is no *WSController that does not have one, so there is
	// no assembly of this type that produces an ungated Send. Its zero value
	// is disarmed, so a freshly dialled controller cannot write.
	gate armGate

	// ctx is cancelled by Close; it bounds every dial and wakes every waiter.
	ctx    context.Context
	cancel context.CancelFunc

	// stopped is closed by the supervisor as it returns, so Close can wait for
	// the reader to be gone rather than leaving it running.
	stopped chan struct{}

	closeOnce sync.Once
	closeErr  error

	// writeMu serialises whole batches. Held across the acquire-and-write of
	// every command in one Send.
	writeMu sync.Mutex

	mu sync.Mutex
	// conn is the current connection, nil while disconnected.
	conn wsConn
	// ready is closed while a connection is available and open while one is
	// not, so a Send waiting for a reconnect can block on it without polling.
	ready chan struct{}
	// readyClosed tracks ready's state explicitly.
	//
	// It exists because the transitions are not symmetric: a connection can be
	// retired twice (once by the failing writer, once by the reader that
	// notices the same failure) and dialled once. Replacing ready on every
	// retirement would orphan a channel that a waiting Send is already parked
	// on, and that Send would then never wake — a write to a live mixer
	// hanging until its context expired, on a socket that had come back.
	readyClosed bool
	// closed short-circuits Send after Close.
	closed bool
	// peer is the bounded ring of frames the mixer has pushed to us.
	peer []PeerMessage
}

// var _ Controller = (*WSController)(nil) is a compile-time check that
// *WSController keeps satisfying Controller as both evolve.
var _ Controller = (*WSController)(nil)

// NewController dials the switcher_controller WebSocket and returns a
// Controller.
//
// The endpoint is
//
//	wss://<host>/api/v1/switcher_controller?access_token=<PERCENT-ENCODED TOKEN>
//
// host is the bare host, optionally with a port, and never a scheme or a path;
// a value containing either is rejected rather than silently reinterpreted.
// The token comes from POST /api/local_auth/signin, whose body field is
// "alias" — "username" returns HTTP 500 — and that sign-in belongs to
// internal/m2lx, not here.
//
// The FIRST dial is synchronous and its failure is returned, because it is the
// authoritative one: MEASURED, a bad token is refused with HTTP 401 at the
// upgrade, so an operator arming the drawer against stale credentials must be
// told immediately rather than left watching a controller retry forever. Every
// SUBSEQUENT failure is not fatal — the socket carries no audio and no video,
// so a drop is handled by reconnecting with backoff behind the caller's back,
// and only surfaces if a Send needs the socket while it is down.
//
// The returned error never contains the token, and neither does any error the
// returned Controller produces.
//
// Calling this opens a socket capable of changing a live clean feed, so it
// belongs on the operator arming the drawer, not on application start.
func NewController(host, token string) (Controller, error) {
	if host == "" {
		return nil, errors.New("mixer: NewController: host is empty")
	}
	if strings.Contains(host, "://") || strings.ContainsAny(host, "/?#") {
		return nil, fmt.Errorf("mixer: NewController: host %q must be a bare host[:port], with no scheme and no path", host)
	}
	if token == "" {
		return nil, errors.New("mixer: NewController: token is empty; sign in via POST /api/local_auth/signin (the body field is \"alias\") and pass access_token")
	}
	c := newController(controllerURL(host, token), token, dialWebSocket)
	if err := c.start(); err != nil {
		return nil, err
	}
	return c, nil
}

// controllerURL builds the control socket URL, percent-encoding the token.
//
// url.Values.Encode performs standard percent-encoding, which is the correct
// treatment whatever characters the token contains. The scheme is always wss:
// the token is a bearer credential in a query string, so there is no
// circumstance in which this application should put it on the wire in clear.
func controllerURL(host, token string) string {
	u := url.URL{
		Scheme:   "wss",
		Host:     host,
		Path:     controllerPath,
		RawQuery: url.Values{"access_token": {token}}.Encode(),
	}
	return u.String()
}

// newController builds an unstarted controller. Split from NewController so
// that tests can supply their own dial and backoff without a network.
func newController(urlStr, token string, dial dialFunc) *WSController {
	ctx, cancel := context.WithCancel(context.Background())
	return &WSController{
		urlStr:  urlStr,
		red:     redactor{token: token},
		dial:    dial,
		backoff: reconnectBackoff,
		ctx:     ctx,
		cancel:  cancel,
		stopped: make(chan struct{}),
		ready:   make(chan struct{}),
	}
}

// dialWebSocket is the production dial.
//
// The dial error is returned raw; every caller passes it through the redactor,
// because gorilla and the net stack beneath it may quote the URL — which
// carries the token — in a handshake failure.
func dialWebSocket(ctx context.Context, urlStr string) (wsConn, error) {
	d := websocket.Dialer{HandshakeTimeout: dialTimeout}
	conn, resp, err := d.DialContext(ctx, urlStr, nil)
	if err != nil {
		if resp != nil {
			// gorilla returns ErrBadHandshake with no status in the message.
			// MEASURED: a bad token is a 401 here, and that is the difference
			// between "wrong credentials" and "the box is unreachable".
			return nil, fmt.Errorf("%w (HTTP %s)", err, resp.Status)
		}
		return nil, err
	}
	return conn, nil
}

// start performs the opening dial and, on success, launches the supervisor.
//
// On failure nothing is left running: no goroutine, no connection, and the
// context is cancelled so the half-built controller cannot be used.
func (c *WSController) start() error {
	ctx, cancel := context.WithTimeout(c.ctx, dialTimeout)
	defer cancel()
	conn, err := c.dial(ctx, c.urlStr)
	if err != nil {
		c.cancel()
		close(c.stopped)
		return fmt.Errorf("mixer: dial switcher_controller: %w", c.red.err(err))
	}
	if !c.setConn(conn) {
		// Only reachable if Close ran against a controller that was never
		// returned to a caller. Nothing is left running.
		close(c.stopped)
		return fmt.Errorf("mixer: dial switcher_controller: %w", ErrClosed)
	}
	go c.supervise(conn)
	return nil
}

// setConn publishes a live connection and wakes anything waiting for one. It
// reports whether the connection was published.
//
// A false result means Close won the race and the connection has been closed
// here rather than published; the supervisor must then stop.
//
// That race is not theoretical and its consequence is a hung Close. Close
// takes this same mutex to set closed and to read the connection it must shut,
// so a connection published moments later would be one Close never saw — and
// the reader parked on it would never wake, on a socket MEASURED to push
// nothing for 45 seconds at a time. Close would then wait on a supervisor that
// never returns. Doing the check and the close under the one lock makes the
// two outcomes exhaustive: either Close sees the connection in c.conn and
// closes it, or setConn sees closed and closes it here.
func (c *WSController) setConn(conn wsConn) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		conn.Close()
		return false
	}
	defer c.mu.Unlock()
	c.conn = conn
	if !c.readyClosed {
		close(c.ready)
		c.readyClosed = true
	}
	return true
}

// clearConn retires the current connection and arms a fresh ready channel for
// the next one. It is idempotent: retiring an already-retired connection
// leaves the existing ready channel in place, so waiters are not orphaned.
func (c *WSController) clearConn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn = nil
	if c.readyClosed {
		c.ready = make(chan struct{})
		c.readyClosed = false
	}
}

// supervise owns the connection for the life of the controller: read the
// current one until it fails, then reconnect with backoff, forever, until
// Close.
//
// It takes the already-dialled first connection so that NewController can
// report the opening failure to its caller while the supervisor handles every
// failure after that silently. A reconnect failure is deliberately not
// reported anywhere: this socket is not in the transmission path, and a
// Send that needs it while it is down finds out then.
func (c *WSController) supervise(conn wsConn) {
	defer close(c.stopped)

	attempt := 0
	for {
		// Drain the connection until it fails or Close shuts it.
		c.readUntilError(conn)
		c.clearConn()

		if c.ctx.Err() != nil {
			return
		}

		attempt++
		if !c.wait(backoffDuration(c.backoff, attempt)) {
			return
		}

		dialCtx, cancel := context.WithTimeout(c.ctx, dialTimeout)
		next, err := c.dial(dialCtx, c.urlStr)
		cancel()
		if err != nil {
			conn = deadConn{}
			continue
		}
		attempt = 0
		conn = next
		if !c.setConn(conn) {
			return
		}
	}
}

// deadConn stands in for a connection that could not be dialled, so the
// supervisor loop has a uniform shape: reading it returns immediately and the
// loop falls through to the next backoff step.
type deadConn struct{}

// WriteMessage always fails: there is no connection.
func (deadConn) WriteMessage(int, []byte) error { return errors.New("mixer: not connected") }

// ReadMessage always fails: there is no connection.
func (deadConn) ReadMessage() (int, []byte, error) { return 0, nil, errors.New("mixer: not connected") }

// SetWriteDeadline is a no-op: there is no connection.
func (deadConn) SetWriteDeadline(time.Time) error { return nil }

// Close is a no-op: there is no connection.
func (deadConn) Close() error { return nil }

// readUntilError drains conn until a read fails.
//
// Reading continuously is not optional even though the peer is measured to
// push nothing. An unread socket stops draining its receive window, so a peer
// that did decide to say something would eventually block on us; and gorilla
// processes ping, pong and close control frames only from inside a read, so
// without this loop a close from the far end would never be noticed and the
// reconnect would never happen.
//
// Everything read is retained — see recordPeer — and nothing read is
// interpreted as a reply to any command.
func (c *WSController) readUntilError(conn wsConn) {
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		c.recordPeer(payload)
	}
}

// recordPeer appends a frame to the bounded ring.
func (c *WSController) recordPeer(payload []byte) {
	msg := PeerMessage{
		At:             time.Now(),
		Payload:        append([]byte(nil), payload...),
		LooksLikeError: errorish(payload),
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.peer = append(c.peer, msg)
	if len(c.peer) > maxPeerMessages {
		c.peer = c.peer[len(c.peer)-maxPeerMessages:]
	}
}

// PeerMessages returns the frames the mixer has pushed, oldest first, capped at
// maxPeerMessages.
//
// This is the whole of "surface what the peer says". It is not correlated with
// any Send and cannot be — see the file comment — so it is diagnostic evidence
// for an operator or a bug report, never a confirmation that a write applied.
//
// MEASURED: an idle live connection produced nothing here in 45 seconds, so a
// non-empty result is itself the finding.
func (c *WSController) PeerMessages() []PeerMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]PeerMessage(nil), c.peer...)
}

// wait sleeps for d, or returns false if the controller is closed first.
func (c *WSController) wait(d time.Duration) bool {
	if d <= 0 {
		return c.ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-c.ctx.Done():
		return false
	}
}

// backoffDuration returns the delay before reconnect attempt n (1-based),
// holding at reconnectBackoffCap once the ladder is exhausted.
func backoffDuration(ladder []time.Duration, n int) time.Duration {
	if n <= 0 {
		return 0
	}
	if n-1 >= len(ladder) {
		if len(ladder) == 0 {
			return reconnectBackoffCap
		}
		last := ladder[len(ladder)-1]
		if last > reconnectBackoffCap {
			return last
		}
		return reconnectBackoffCap
	}
	return ladder[n-1]
}

// acquire returns the live connection, waiting for a reconnect if the socket is
// currently down.
//
// The wait is bounded by ctx and by Close, never by an internal timeout: how
// long an operator's arm-gated write is prepared to wait is the caller's
// decision, and a Send with no deadline against a box that is coming back up
// should wait for it.
func (c *WSController) acquire(ctx context.Context) (wsConn, error) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, ErrClosed
		}
		if conn := c.conn; conn != nil {
			c.mu.Unlock()
			return conn, nil
		}
		ready := c.ready
		c.mu.Unlock()

		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.ctx.Done():
			return nil, ErrClosed
		}
	}
}

// dropConn retires a connection a write has just failed on.
//
// Two things have to happen, in this order. The connection is unpublished, so
// that the retry inside sendOne waits for the supervisor's replacement instead
// of writing down a socket already known to be broken; and it is closed, which
// is what wakes the supervisor's parked ReadMessage and starts the reconnect.
//
// The identity check guards the unpublish only. If the supervisor has already
// replaced the connection, clearing conn here would retire that healthy
// replacement. Closing is unconditional and safe either way: a second Close on
// an already-closed connection returns an error and does nothing else, whereas
// failing to close one nothing else has noticed yet leaks it for as long as
// the far end keeps the TCP connection open.
func (c *WSController) dropConn(conn wsConn) {
	c.mu.Lock()
	current := c.conn == conn
	if current {
		c.conn = nil
		if c.readyClosed {
			c.ready = make(chan struct{})
			c.readyClosed = false
		}
	}
	c.mu.Unlock()
	conn.Close()
}

// writeOne writes a single serialised envelope, applying a deadline.
func writeOne(ctx context.Context, conn wsConn, payload []byte) error {
	deadline := time.Now().Add(writeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

// Send transmits commands in order and returns when they have been written.
//
// THIS IS THE ARM-GATED WRITE PATH. It changes a live mixer feeding the PGM
// and CLN outputs.
//
// # What happens on a mid-batch failure, precisely
//
// EVERY envelope is built and serialised BEFORE ANY of them is written. So a
// batch that is malformed anywhere — an unknown bus in command 3, a
// SetCompLimit that would be silently inert — writes NOTHING AT ALL and fails
// with a *BatchError whose Written is 0. Validation can never half-apply a
// batch.
//
// A TRANSPORT failure can. Commands are written one at a time in the given
// order, and if command i fails to write, commands [0,i) have already gone to
// the mixer and are NOT rolled back — there is no transaction on this socket
// and no undo. The returned *BatchError carries Written = i and Index = i, and
// its message states the split in words. The caller must treat the mixer as
// being in an intermediate state and re-read switcher_status.
//
// A write that fails at the transport level is retried ONCE, on a fresh
// connection, before the batch is abandoned. That is safe only because every
// command in this vocabulary is an ABSOLUTE ASSIGNMENT — set routing to this
// set, set muted to this value, set gain to this number — so applying one
// twice is indistinguishable from applying it once. Do not extend this retry
// to any command that is a delta.
//
// Concurrent Sends do not interleave: a batch holds the write lock for its
// whole duration.
//
// # A nil return is not confirmation
//
// It means the bytes were written. It does not mean the mixer did anything.
// Two commands in this vocabulary are known to accept a write and ignore it:
// SetCompLimit with the wrong AGCMode (silently inert limiter threshold) and
// SetChFader (measured as "ACKed 200 but silently dropped"). Callers MUST
// confirm by reading the next switcher_status frame back through ParseSnapshot
// and comparing. Reporting a nil error to an operator as "routing fixed" is
// how a fix that never applied gets signed off.
//
// # The arm gate
//
// Send refuses with ErrDisarmed, having written nothing, unless Arm has opened
// a window that has not expired. The check is HERE, in Go, and not only in the
// drawer that calls it: the JavaScript gate is good but it is one layer, and
// once this is behind a Wails binding anything in the webview can reach the
// binding. Two independent gates means a bypass needs both.
//
// The check runs BEFORE any envelope is built, so a disarmed Send does not
// even serialise the commands, let alone open the socket.
//
// Sending zero commands is a no-op and not an error, and is permitted while
// disarmed: a caller that computed "nothing to correct" must be able to call
// Send unconditionally without first opening a live write window, and a batch
// that writes nothing cannot change a mixer.
//
// The context bounds the whole batch, including any wait for a reconnect; on
// cancellation, commands already written have already been applied and are not
// rolled back.
func (c *WSController) Send(ctx context.Context, cmds ...Command) error {
	if len(cmds) == 0 {
		return nil
	}

	// Phase 0: the gate. Evaluated against the clock now, not against
	// anything a caller read earlier, and before a single byte is built.
	//
	// Closed is reported ahead of disarmed because Close disarms, so after a
	// Close both are true and ErrClosed is the one that tells the caller
	// something they did not already know. acquire would reach the same
	// conclusion later; doing it here keeps a closed controller from
	// serialising a batch it can never write.
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return &BatchError{Index: 0, Total: len(cmds), Written: 0, Command: commandName(cmds[0]), Err: ErrClosed}
	}
	if ok, err := c.gate.armedAt(time.Now()); !ok {
		return &BatchError{Index: 0, Total: len(cmds), Written: 0, Command: commandName(cmds[0]), Err: err}
	}

	// Phase 1: build everything. Nothing is written in this loop, so a
	// validation failure cannot half-apply a batch.
	payloads := make([][]byte, len(cmds))
	for i, cmd := range cmds {
		if cmd == nil {
			return &BatchError{Index: i, Total: len(cmds), Written: 0, Command: "nil command", Err: errors.New("mixer: nil Command in batch")}
		}
		env, err := cmd.Envelope()
		if err != nil {
			return &BatchError{Index: i, Total: len(cmds), Written: 0, Command: commandName(cmd), Err: err}
		}
		payload, err := json.Marshal(env)
		if err != nil {
			return &BatchError{Index: i, Total: len(cmds), Written: 0, Command: commandName(cmd), Err: err}
		}
		payloads[i] = payload
	}

	// Phase 2: write in order.
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	for i, payload := range payloads {
		if err := c.sendOne(ctx, payload); err != nil {
			return &BatchError{Index: i, Total: len(cmds), Written: i, Command: commandName(cmds[i]), Err: c.red.err(err)}
		}
	}
	return nil
}

// Arm opens the write window for window and returns the instant it closes.
//
// Pass ArmWindow unless there is a reason not to; see that constant for how
// two minutes was chosen and why the expiry is a stored deadline rather than a
// timer. A window of zero or less disarms.
//
// Arming is the only thing that permits Send. Nothing else opens the gate:
// dialling does not, reconnecting does not, and a previous successful Send
// does not. Arming again extends the window rather than stacking, so a caller
// that arms on every operator gesture cannot accumulate one.
//
// Arming does not touch the mixer. It permits a later Send to.
func (c *WSController) Arm(window time.Duration) time.Time {
	return c.gate.arm(window)
}

// Disarm shuts the write window immediately. Idempotent, and safe on a closed
// controller.
func (c *WSController) Disarm() {
	c.gate.disarm()
}

// ArmedUntil reports when the window closes, or the zero Time when disarmed,
// expired or closed. See the Controller interface: it is a report, not a
// permission — Send re-checks at the moment of the write.
func (c *WSController) ArmedUntil() time.Time {
	return c.gate.armedUntil()
}

// sendOne writes one envelope, retrying once on a fresh connection.
//
// See Send for why one retry is safe: every command in this vocabulary is an
// absolute assignment, so a duplicate write is a no-op rather than a doubled
// change.
func (c *WSController) sendOne(ctx context.Context, payload []byte) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		conn, err := c.acquire(ctx)
		if err != nil {
			if lastErr != nil && errors.Is(err, context.DeadlineExceeded) {
				return lastErr
			}
			return err
		}
		if err := writeOne(ctx, conn, payload); err != nil {
			lastErr = err
			c.dropConn(conn)
			continue
		}
		return nil
	}
	return lastErr
}

// Close releases the WebSocket.
//
// It is idempotent: the second and later calls do the same nothing and return
// the same result. It cannot deadlock against the reader — it cancels the
// context and closes the connection FIRST, which is what unblocks a
// ReadMessage that is parked waiting for a frame that (measured) never comes,
// and only then waits for the supervisor to exit. No lock is held across that
// wait, and the reader takes no lock Close holds.
//
// After Close, Send fails wrapped in a *BatchError, having written nothing.
//
// Close DISARMS first, before anything else and outside closeOnce, so that a
// second Close, a Close racing a Send, and a Close that somehow fails to tear
// the socket down all leave the gate shut. A closed Controller is never armed:
// ArmedUntil reports the zero Time, and Arm on a closed controller opens a
// window that Send refuses anyway, with ErrClosed.
func (c *WSController) Close() error {
	c.gate.disarm()
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		conn := c.conn
		c.conn = nil
		c.mu.Unlock()

		c.cancel()
		if conn != nil {
			c.closeErr = c.red.err(conn.Close())
		}
		<-c.stopped
	})
	return c.closeErr
}
