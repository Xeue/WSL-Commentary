# CONTRACT — read this before you write a line

**Written by WP-0. Amended 2026-08-07 to describe what was built.** Nine work packages ran in
parallel against the original; four more (WP-P, WP-R, WP-M0…M4) were added afterwards and are
folded in below. The types and paths here are still the seam — rule 3 still applies — but the
document you are reading is no longer a forecast.

Authority, in order: [`docs/windows-app-spec.md`](docs/windows-app-spec.md) **v3** decides what is
built. [`docs/project-plan.md`](docs/project-plan.md) §2 is the original work breakdown and is now
historical. [`docs/architecture.md`](docs/architecture.md) and
[`docs/test-results.md`](docs/test-results.md) are the 2026-07-29/30 measurement record — read the
sections relevant to your package before you assume a wire shape, but note that they predate the
SRT picture path, the mixer drawer and the discovery that `switcher_status` is snapshot-then-delta.
Where they and spec v3 disagree, v3 is later and was measured against the same instance.

**What changed since the frozen version of this file, in one place.** Spec v3 §0 is the full list;
these are the ones that change what a package may assume:

- `internal/mixer` exists, and so do the two SRT **inbound** paths — the picture and the audio
  return — inside `internal/gst`. None of the three were in the original ownership map.
- **`statusKey` is a `node` value inside an array, not a top-level property.** The wording in the
  `internal/m2lx` summary below said `<statusKey>.stream_state`, and a parser written to that
  description could never match with any key. Corrected in place; spec v3 §8.1 has the measured
  frame.
- **`switcher_status` is snapshot-then-delta.** A complete document exists exactly once per
  connection. Spec v3 §8.2.
- The honest line is **withdrawn from the GUI** at the operator's instruction. The function and its
  tests are kept. See the closing section.
- There are **three** Credential Manager targets, not two.

---

## The three rules

1. **`go.mod`, `go.sum` and `frontend/package.json` are frozen.**
   Do not run `go get`, `go mod tidy`, `npm install <pkg>` or `npm update`. Every dependency you
   need is already declared and already in `go.sum` / `package-lock.json`. Concurrent edits to
   those files are the classic way parallel work on a Go project falls apart, so the possibility
   has been removed rather than managed. If you genuinely need a dependency that is not there,
   **stop and report it** — do not add it.

2. **Each package owns its paths exclusively.** Do not create, edit or delete a file under
   another WP's paths, not even a typo fix, not even a test. `main.go`, `app.go` and
   `main_nocgo.go` belong to WP-8 alone.

3. **If an interface is wrong, report it — do not edit it.** A silently widened interface breaks
   every sibling that already compiled against it. This includes struct fields, JSON tags,
   constant values, channel-closing semantics and error contracts. Say what is wrong and what it
   should be; WP-0's output is amended once, centrally.

### And a fourth, added 2026-08-07, because it did not apply when the first three were written

4. **The application is built, it is in use, and it is on air.** Two consequences, both absolute:
   - **Do not launch the GUI.** The operator has it open. Opening a second `wslcomms.exe` takes
     the SRT input's one peer slot away from the position that is broadcasting — an SRT listener
     accepts exactly one peer and never displaces the incumbent. Building is fine; running is not.
   - **Do not write to the live mixer.** Reading `switcher_status`, reading the REST API and
     dialling an output are all fine and are how most of spec v3 was measured. `set_routing` and
     its siblings change a clean feed that is going to a client.

   Nothing in this contract may be traded against that. A change that regresses the picture, the
   feed or the return is worse than the problem it fixes.

---

## Ownership map

| Path | Owner | Responsible for |
|---|---|---|
| `go.mod`, `go.sum` | **WP-0 — FROZEN** | every Go dependency, pre-declared |
| `frontend/package.json`, `frontend/package-lock.json` | **WP-0 — FROZEN** | every npm dependency, pre-declared |
| `assets/slate.png` | **WP-0** | 1920x1080 slate fed to `filesrc ! pngdec ! imagefreeze` |
| `CONTRACT.md` | **WP-0** | this file |
| `main.go`, `main_nocgo.go`, `app.go`, `exit_windows.go` | **WP-8** | Wails bindings, wire-up, events, lifecycle, the hard-exit path |
| `app_remote.go` (and `app_remote_test.go`) | **WP-8 — added 2026-08-12** | the App-side of the LAN bridge: the hand-written allowlist that implements `remote.Dispatcher` (method → capability → host-only), the audit log, mixer arm-ownership routing, the five host-only remote-admin bound methods, and the listener's startup/teardown wiring. The transport it drives is `internal/remote` (WP-REMOTE). |
| `app_picture.go` | **WP-P** | the SRT picture's bound surface, the native overlay, the `picture` event |
| `app_return.go` | **WP-R** | the SRT audio return's bound surface and the `return` event |
| `app_mixer.go` | **WP-8** | the mixer drawer's bound surface: snapshot, arm/disarm, send, golden |
| `app_presets.go`, `internal/presets/`, `frontend/src/ui/presets.js` (and their tests) | **WP-PRESETS** | the M2L-X instance presets: whitelist, file store, credential-scope decorator, bound surface, picker model |
| `internal/config/` | **WP-1** | `%APPDATA%\WSLComms\config.json`, spec §9 |
| `internal/secrets/` | **WP-1** | Windows Credential Manager: `WSLComms/m2lx`, `WSLComms/srt`, `WSLComms/srtreturn` |
| `internal/m2lx/` | **WP-2** | sign-in, token refresh, status WebSocket, the snapshot/delta document, 4 s debounce, 15 s staleness |
| `internal/gst/gst*.go` | **WP-3a** | the contribution pipeline, device monitor, sink swap, **and the stub twin** |
| `internal/gst/picture*.go`, `overlay_*.go` | **WP-P** | the SRT picture pipeline and the native child window |
| `internal/gst/return*.go` | **WP-R** | the SRT audio return pipeline and `ListOutputDevices` |
| `internal/sender/` | **WP-3b** | spec §6 in full: timestamp pinning, reconnect state machine, backoff ladder |
| `internal/kvs/` | **WP-4** | M2L-X → Cognito credential chain |
| `internal/mixer/` | **WP-M0…M3** | bus model, `switcher_status` parse, golden/compare, `switcher_controller` client |
| `internal/remote/` (incl. `shim.js`) | **WP-REMOTE — added 2026-08-12** | the App-agnostic authenticated LAN control bridge: TLS transport, PBKDF2 auth, session fan-out, the `//go:embed` frontend shim. Pairs with `app_remote.go` (root, WP-8), which implements `remote.Dispatcher`. |
| `frontend/src/monitor/` | **WP-5a** | KVS viewer, 8 transceivers, mosaic crop, bus + channel selection, Web Audio return, `setSinkId` |
| `frontend/src/ui/`, `frontend/src/styles/`, `frontend/index.html`, `frontend/src/main.js` | **WP-5b** | controls, lamps, Settings view, picture source |
| `frontend/src/ui/mixer/` | **WP-M4** | the drawer: `contract.js`, `model.js`, `drawer.js`, the demo harness |
| `build/` | **WP-6** | `env.ps1`, DLL allowlist script, LGPL notices and written offer, Inno Setup installer |
| `cmd/mockm2lx/` | **WP-7** | mock REST, status WS, SRT listener, **fault injection** |
| `docs/` | nobody — read-only | the spec and the measurement record |

`frontend/vite.config.js` does not exist. If WP-5b needs one, WP-5b creates it; nobody else.

**The three-way split inside `internal/gst` is not decorative.** The contribution pipeline, the
picture pipeline and the return pipeline share the process and nothing else: separate types,
separate bus handlers, separate reconnect machines, separate locks. There is deliberately no code
path from a monitor into the sender or back. A return monitor failing must never disturb the
contribution feed, and the way that is guaranteed is by there being nothing to share. The one thing
the picture path does borrow from the return path is `fakeSinkName`, and the comment at the top of
`picture.go` says so and says it is the only one.

---

## Gate A is still the definition of done, and the toolchain being present does not change it

The build host now has MinGW gcc, GStreamer 1.28.5 devel and the Wails CLI, and `wslcomms.exe` is
built and in use. **Keep Gate A working anyway.** On a machine with only Go and Node, all three of
these must pass, at every commit, from the repo root:

```
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test ./...
```

That property is what lets anyone pick this project up without a 2 GB install, and it is what
keeps the cgo surface small enough to reason about. It is only possible because `internal/gst` is
the **only** package that touches cgo, and each of its three pipelines has two implementations
selected by build tag:

| cgo half | stub twin | supplies |
|---|---|---|
| `gst_cgo.go` | `gst_stub.go` | the contribution pipeline, `ListInputDevices` |
| `picture_cgo.go` | `picture_stub.go` | `newPicturePipe` — one picture attempt |
| `return_cgo.go` | `return_stub.go` | `newReturnPipe`, `ListOutputDevices` |
| `overlay_windows.go` | `overlay_other.go` | the native child window |

The stubs are **real code and are meant to work** — plausible devices, and pipelines whose
transitions are driven programmatically. They are what the unit tests drive, and rightly so: no
unit test should be driving a live media pipeline. Everything testable without GStreamer lives in
the untagged file beside each pair — option validation, the backoff ladders, the whole reconnect
state machines, the overlay rectangle arithmetic.

**At Gate B the test tag is `dev gststub`, not `dev`.** `CGO_ENABLED=1` selects the cgo halves and
excludes the stubs, and without `gststub` the root package fails to build while every other package
reports `ok` — mostly green, with the lifecycle tests silently not run. `build\env.ps1` prints the
right command.

`main.go`, `app.go` and the four `app_*.go` files are `//go:build dev || production || bindings`.
`main_nocgo.go` supplies a `main()` for every other build that prints one line and exits, so a
stray `go build .` produces an inert binary rather than Wails' modal dialog.
`CGO_ENABLED=0 go build -tags production ./internal/gst` **fails to compile on purpose**, in
`gst_stub_guard.go`: without that guard it produced a plausible `wslcomms.exe` silently backed by
the stub, which lights the SENDING lamp green and sends nothing at all.

**Do not add a cgo import to any package other than `internal/gst`.** If you think you need
one, you have found a design problem, not a missing import.

---

## Package summaries

Read the doc comments in the source; they are the contract. What follows is the index.

### `internal/config` — WP-1
`Config` with the exact JSON field names of spec §9. `Defaults()` (a constant table, not logic),
`Path()`, `Load()`, `(*Config).Save()`, plus `Validate()` / `ValidateReturn()` and the
`Effective*` readers. Load on a missing file returns `Defaults()` and a nil error — first run is
not an error, and unmarshalling onto a `Defaults()`-populated struct means a key missing from an
older file keeps its documented default rather than becoming a Go zero.

The field list has grown well past spec v2's nine keys; spec v3 §9 is the table. Three points that
are contract rather than detail:

- **`Validate` gates `Start`; `ValidateReturn` gates the monitors.** Nothing in `Validate` may be a
  reason a match does not go out, so `statusKey`, `srtHost`, `returnChannel` and
  `pictureLatencyMs` are deliberately not in it. Requiring `statusKey` in particular made the app
  unstartable until the operator had guessed a value nothing in the API can tell them.
- **`headphoneDeviceId` and `headphoneEndpointId` are different kinds of identifier** — a browser
  `mediaDeviceId` and a WASAPI IMMDevice GUID — and must never be merged. Using one where the other
  belongs fails silently in both directions.
- **`Save` must never write a secret.** `TestSave_NeverWritesSecretFields` enforces it.

### `internal/secrets` — WP-1
`Store` with `Get(key)`/`Set(key, value)`. **Three** keys, not two: `KeyM2LX` (`"m2lx"`), `KeySRT`
(`"srt"`) and `KeySRTReturn` (`"srtreturn"`), mapping to Credential Manager targets
`WSLComms/m2lx`, `WSLComms/srt` and `WSLComms/srtreturn`. The third exists because **M2L-X sets SRT
encryption per output** — Output 1 measured `encrypted=false`, Outputs 2 and 3 `true` — so the
endpoint the feed goes to and the endpoint the picture comes from routinely need different keys.
Sharing one meant that entering the key which made the return work changed the key the feed went
out with.

`ErrNotFound` is a normal first-run condition, not a failure. Secrets never enter `config.json`, a
log line, a GStreamer URI, or the Wails boundary in the outbound direction — there is deliberately
no getter.

**Amended for the M2L-X instance presets (approved under rules 2 and 3):** the three logical keys
can be **scoped** to a preset — `ScopedKey("wembley", KeyM2LX)` → key `"wembley/m2lx"` → target
`WSLComms/wembley/m2lx`. The **empty scope resolves to the three legacy targets byte-for-byte**
(`TestTargetNames` is unchanged), the `Store` interface is untouched (the key carries the scope),
and the scope charset is exactly what `internal/presets.DeriveID` can produce, so a scope can never
collide with a legacy target (`TestScopedTargetsDoNotCollide` pins the adversarial `"srt"/m2lx`
pair). Secrets are never copied between scopes. The one recorded narrowing of "no getter":
`App.GetPresetCredentialStatus` reports whether a credential **exists** for the active scope —
three booleans, never a value — because after applying a preset the operator must know whether to
type the passwords, and the frontend's session-only badge cannot answer for a scope never written
to in this run. The reasoning lives beside the type in `app_presets.go`.

### `internal/presets` — WP-PRESETS
One M2L-X deployment's coordinates as a file under `%APPDATA%\WSLComms\presets\<id>.json`, applied
onto the live config as a **merge**. The load-bearing mechanism is a **whitelist**
(`InstanceFields`, 14 tags) plus apply-by-`json.Unmarshal`-onto-the-live-struct — the same
primitive `config.Load` uses — so a preset is *physically incapable* of writing a field it does not
carry, and `Extract`/`Filter` make sure it never carries a MACHINE field (`audioDeviceId`,
`headphoneDeviceId`, `headphoneEndpointId`, `slatePath`) or a UI field (`returnSource`,
`returnChannel`). A reflection test fails by name on any unclassified `config.Config` field.
`DeriveID` is the security-sensitive function — the id is both a filename and a Credential Manager
scope segment — and its rejection table (traversal, separators, colons, Windows reserved device
names, length) is not optional. `active.json` (which preset this PC points at, and its credential
scope) is MACHINE state: never inside a preset body, never a `config.Config` field. Gate A: the
package imports `internal/config` and nothing from `internal/gst`.

### `internal/m2lx` — WP-2
`Client` (`SignIn`/`Refresh`/`Token`/`KVSInfo`/`KVSToken`) and `Watcher` (`Watch`, `RawSnapshot`).
The sign-in body field is **`alias`**, not `username`; `username` returns HTTP 500.
`Status.Audio` is a **slice** because an empty slice is the MP2/AC-3 silent-drop signature and
every caller must be able to see it. `Status.Stale` carries the 15 s staleness verdict, which
`WP-5b` renders as `STATUS UNAVAILABLE` with the three WebSocket lamps greyed. `DebounceWindow`
is 4 s, `StaleAfter` 15 s.

**Two corrections to what this file used to say about the wire, both measured.**

1. **`statusKey` is not a top-level property.** A frame is
   `{"status":[{"node":…,"path":…,"state":{…}}, …],"timestamp":…}`; `statusKey` is the value of an
   entry's `node` and the lamp fields are nested under that entry's `state`. The parser written to
   the old description looked `statusKey` up at the top level, so it could never match with **any**
   key — which is why the three WebSocket lamps had never worked.
2. **The socket is snapshot-then-delta.** The first frame after a connection opens is the whole
   document (36 nodes, every entry at `path:"/"`, 84 KB); every frame after it is a subtree delta at
   ~21 fps, and **`path` says which subtree**. A parser that ignores `path` reads a
   `/statistics` delta as a whole node, finds no `stream_state`, and concludes the one input that
   is working is not a router input. Deltas are **merged** by JSON pointer onto the opening
   snapshot, not skipped — skipping froze the lamps at their connect values for the whole session.

`RawSnapshot` opens its **own** connection and takes the opening frame, because a complete document
exists exactly once per connection. Every call costs one dial, and the caller is expected to know
that.

`Status` is the app's **normalised** type, not the wire type: WP-2 unmarshals M2L-X's payload
into its own private structs and produces this. The measured wire values of
`streams.video.format` and `streams.audio[].format` are single strings — `"h264 1920x1080 50 P"`
and `"aac 48000 2ch"` — so `VideoFormat`/`AudioFormat` carry a `Raw` field alongside the parsed
fields. A format the parser does not understand must surface as `Raw` rather than read as zero.

Token handling: `TokenLifetime` is the measured 86399 s and `RefreshFraction` is 0.5, via
`/api/local_auth/refresh_token`. The refresh token's own TTL is unmeasured, so a failed
`Refresh` must fall back to a full `SignIn`.

### `internal/gst` — the contribution pipeline — WP-3a
`Device{ID, Name}`; `ID` is the IMMDevice endpoint GUID and is the only thing persisted or
passed to `wasapi2src`. `Pipeline` with `Start(PipelineOpts)`, `ReplaceSink(SinkOpts)`,
`ForceKeyUnit()`, `Errors()`, `Stop()`; package functions `Init(appDir)`, `ListInputDevices()`,
`New()`.

Two contract points that are easy to get wrong and cost a day each:

- **`Start` installs no sink.** It plays the capture/encode/mux chain with the `srtq` src pad
  blocked. The first `ReplaceSink` installs the first sink. That is what lets the chain stay in
  PLAYING for the life of the process, which is the structural fix for the backwards-DTS bug.
- **`Errors()` is closed by `Stop()`**, and implementations drop rather than block when it is
  full. Synchronous failures come back from the method that caused them and never appear there.

The H.264 encoder is resolved at runtime **by preference, not by rank** — `mfh264enc`, then
`qsvh264enc`, `nvh264enc`, `d3d11h264enc`, `amfh264enc`, with `x264enc` denylisted for its licence.
Rank only excludes factories GStreamer has marked unusable and tie-breaks equals. Spec v3 §5.1 has
the measured ranks and the argument; the short version is that rank picks whichever GPU vendor's
element is installed, and the property set was written against `mfh264enc`. Also: **`mfh264enc` has
no `bframes` property**, and `gst_parse_launch` rejects an unknown property outright.

### `internal/gst` — the SRT picture — WP-P
`PictureMonitor` with `Start(PictureOpts)` / `Stop()` / `States()`, `NewPictureMonitor()`,
`PictureState` (`stopped` / `connecting` / `showing` / `backoff` — **lowercase**, and those strings
reach the page), `PictureRect` and `ScaleRect`, `PictureBackoffLadder` / `PictureBackoffCap`, and
the `PictureOverlay` native child window.

- **A monitor is single-use.** After `Stop` its state channel is closed; build another.
- **`Play` waits for a decoded frame, not for `PLAYING`.** `srtsrc` connects lazily on a streaming
  thread after the state change has already returned success, so a `Play` that returned at PLAYING
  would report a working picture on a socket that does not exist.
- **The overlay takes CSS pixels *and* the page's `devicePixelRatio` in one call.** Reading the DPI
  on the Go side is a different number measured at a different moment.
- **`Stop` does not destroy the window.** The window outlives the monitor.
- `ErrAbandonedThread` is a sentinel, not a sentence: `App.teardown` tests for it with `errors.Is`
  and ends the process with `TerminateProcess` rather than running DLL detach over a killed thread.

### `internal/gst` — the SRT audio return — WP-R
`ReturnMonitor` with the same shape, `ReturnState` (`STOPPED` / `CONNECTING` / `PLAYING` /
`BACKOFF` — **uppercase**, unlike the picture's, and both are contract), `ReturnOpts` with the
stereo/left/right `MixMatrix`, and `ListOutputDevices()`.

**The picture and the return are mutually exclusive** and `app_picture.go` refuses rather than
racing: both dial the same M2L-X output, an SRT listener accepts one peer and never displaces the
incumbent, so two callers from one process means one sits in its backoff ladder for the whole match
and which one wins is a race.

### `internal/mixer` — WP-M0…M3
`Bus` and `AllBuses`, `BusLabel`, `Snapshot` / `ParseSnapshot`, `Compare` / `Diff`, golden-state
load and save, and `Controller` / `NewController` / `Arm` / `Send` / `Close`.

- **`aux1` IS the clean feed.** Sony's mixer surface calls that bus "AUX" and Sony's output list
  calls the same bus "cln", and nothing in their UI says they are the same. Everything rendering a
  bus name to a human goes through `BusLabel`; never print the raw `"aux1"`. The JS table in
  `frontend/src/ui/mixer/contract.js` duplicates it deliberately, so the warning renders on the
  first frame even with the backend unreachable. **The Go table is authoritative.**
- **Every strip defaults to `["master","aux1","aux2"]`**, so anything unmuted is in the clean feed
  unless corrected. That is the whole reason this package exists.
- **`set_routing` is an absolute REPLACE.** `nil` outputs is refused outright rather than read as
  "leave unchanged"; an explicit empty slice serialises as `[]`, never `null`.
- **`Send` refuses with `ErrDisarmed` outside an `Arm` window** (`ArmWindow` is two minutes). This
  is the second of two independent gates; the first is `createWriteGate` in the frontend model, and
  the whole value of two gates is that a bypass needs both. **Do not add a second write path.**
- **A resolved `Send` means SENT, not applied.** Responses are not correlated to commands and
  cannot honestly be — nothing arrives unsolicited, and two commands in this vocabulary are known
  to be ACKed and silently ignored. Confirmation is the next `switcher_status` frame.
- `aux2` is routable, its meter moves, and **no M2L-X output accepts it as a source.** It is
  carried in the model and labelled undeliverable precisely because it looks functional.

### `internal/sender` — WP-3b
`State` with `CONNECTING`/`CONNECTED`/`DRAINING`/`BACKOFF`/`STOPPED` — these strings reach the
frontend on the `sender` event. `Sender` with `Start(Opts)`/`Stop()`/`States()`.
`New(p gst.Pipeline) Sender`: the pipeline is **injected**, so WP-3b writes its own fake
`gst.Pipeline` in its test files and never needs cgo. `BackoffLadder` is 7, 7, 10, 15, 20 s then
`BackoffCap` 30 s forever; the first delay must stay ≥ 6 s to clear M2L-X's re-accept refusal
window. `Opts` composes `gst.PipelineOpts` and `gst.SinkOpts` rather than restating their fields.

### `internal/kvs` — WP-4
`Credentials` and `Fetch(ctx, m2lx.Client, eventID)`. The JSON tags on `Credentials` are what
WP-5a reads in JavaScript.

**SP-1 is answered.** These shapes have since been used against the live instance at Gate C and the
KVS return has been on air. The measured responses:

```
GET /api/live_operation/kvs/webrtc_info/{event}
  → {"region":"eu-west-1","signaling_channel":{"pgm":["webrtc-wslstudios-matcht"]}}
GET /api/live_operation/kvs/webrtc_token/{event}
  → {identity_id, token}
```

Note what `webrtc_info` does **not** return: a channel ARN. M2L-X gives a channel **name**.
`Credentials.ChannelName` is therefore the authoritative identifier and `Credentials.ChannelARN`
will normally be empty. WP-5a resolves the ARN in JavaScript with `DescribeSignalingChannel`
before `GetSignalingChannelEndpoint` — which is why `go.mod` has the Cognito client but no
`kinesisvideo` client, and `package.json` has both KVS clients. Do not move that boundary.

It is still one instance. If another disagrees, that is a change to `m2lx.KVSInfo`/`m2lx.KVSToken`
and must be reported under rule 3.

### `cmd/mockm2lx` — WP-7
`github.com/datarhei/gosrt` is a **mock-only** dependency. It is a pure-Go SRT implementation so
that the mock can run an SRT listener at Gate A with no libsrt. It must never be imported by
anything under `internal/`; the production path is GStreamer's `srtsink` over libsrt.
Most of WP-7's value is fault injection: drop the session, hold the listener socket open after a
disconnect, stall the status WebSocket, empty the audio array, and lie about `stream_state`.

### `internal/remote` — WP-REMOTE (added 2026-08-12, rule 3)

The authenticated LAN control bridge. **App-agnostic on purpose:** it imports nothing from the root
package or `internal/gst`, so the whole package builds and unit-tests at Gate A with
`CGO_ENABLED=0` against a fake dispatcher. The one seam is:

```
type Dispatcher interface {
    Call(ctx, ClientInfo, method string, args []json.RawMessage) (any, error)
    Methods(ClientInfo) []string
}
```

`app_remote.go` (root, WP-8) implements it as a **hand-written allowlist switch** — never a
reflective loop, so a method newly bound to `*App` does NOT become remotely callable by default.
`ClientInfo{ID, Name, Caps, RemoteAddr}` is the only thing about a caller the dispatcher sees.

Public surface consumed by the root package: `Server`/`NewServer(Options)`,
`(*Server).Start() (addr, error)` / `.Broadcast(name, data)` / `.Clients()` / `.Close(ctx)` /
`.Fingerprint()`; `Options{Enabled, Bind, Port, Dispatcher, Auth, Assets fs.FS, CertDir, Events,
Logf}`; `NewAuthenticator([]Client)`; `Settings` + `LoadSettings`/`DefaultSettings`/`SettingsPath`/
`RemoteDir` and its `Save`/`Validate`/`Add`/`SetPassword`/`SetCaps`/`DeleteClient` methods;
`Capability` (`CapView`/`CapOperate`/`CapMixer`) with `Allows(granted, required)` implementing tier
inclusion (mixer⊇operate⊇view); `EnsureCertificate(dir, bindIP) (cert, fp, err)`.

Contract points that are load-bearing rather than detail:

- **Off and loopback by default.** `DefaultSettings()` is `enabled:false, bind:"127.0.0.1",
  port:8443`; a missing `remote.json` yields exactly that, so doing nothing is the safe posture.
  `Start()` binds nothing when disabled; `Settings.Validate` **refuses a non-loopback bind with no
  clients** and refuses a bind that is not a literal IP.
- **TLS is mandatory, not optional.** Over plain HTTP a LAN page is not a secure context
  (`navigator.mediaDevices`/`setSinkId` vanish) and `GetKVSCredentials` would cross the LAN in clear.
  A self-signed ECDSA P-256 cert is generated into `%APPDATA%\WSLComms\remote\`, SANs cover the bind
  IP + loopback + `localhost` + hostname, and its SHA-256 fingerprint is `(*Server).Fingerprint()`.
- **`remote.json` is deliberately NOT in `config.json`.** `settings.js collectConfig()` rewrites the
  whole config document from a page cache and drops any field it does not restate; a listener bind
  address must not be reachable by that mechanism. Passwords are PBKDF2-HMAC-SHA256 (stdlib
  `crypto/pbkdf2`, confirmed present under `go 1.25.0`) + `crypto/rand` salt, never readable back.
- **Auth closes the exact hole Wails' devserver leaves open.** The upgrade requires an
  `HttpOnly/Secure/SameSite=Strict` session cookie AND a **strict same-origin** check (Origin
  host:port must equal Host; a missing Origin is refused). Per-source-IP lockout, constant-time
  compare, and a fixed minimum login delay. Sessions are in-memory and all revoked on `Close`.
- **The hello frame's `methods` list is authoritative** — the shim installs exactly those functions
  on `window.go.main.App`, so **host-only methods degrade by OMISSION**, not by a refusal the
  frontend must be taught. `SetPictureRect`/`SetPictureVisible`/`StartPicture`/`StopPicture`/
  `StartReturn`/`StopReturn` are host-only: absent from `Methods()` and refused by `Call` at every
  capability (enforced in `app_remote.go`; the package's fake dispatcher mirrors it).
- **The fan-out never blocks a producer.** Each session has a bounded, drop-oldest event queue with
  a per-connection monotonic `seq` (mirroring `app.go`'s `eventPump`); results ride a separate queue
  that cannot overflow under the in-flight cap. A client that will not drain is dropped by the write
  deadline, never allowed to stall `Broadcast`. Disconnect cancels every in-flight call's context.
- **The shim (`shim.js`, `//go:embed`) is the entire frontend side and needs zero `backend.js`
  changes.** It is a classic script injected before the deferred module bundle, installs
  `window.go`/`window.runtime` (queuing calls until the socket opens), prunes to `methods` on hello,
  and sets `window.__wslcommsRemote = {client, caps}`. It is DOM-optional so a headless test can
  drive its transport; the shim's node test is left to WP-5b's `remotewiring.test.js`.
- **`Start()` must complete-before `Fingerprint()`/`Close()` (usage contract, noted 2026-08-12 by
  WP-8).** `Server.Start` writes `fp`/`httpSrv`/`ln`/`addr` without a lock, on the assumption the
  caller does not read them concurrently. Because the design starts the listener on a goroutine (so
  startup never blocks on the ECDSA keygen), `app.go` establishes that ordering by publishing the
  `*Server` pointer to its atomic field ONLY AFTER `Start` returns: a reader that Loads a non-nil
  pointer has a happens-before edge to `Start`'s writes, and a reader that Loads nil correctly sees
  "not running yet". A future direct, concurrent caller of `Start`+`Fingerprint` would need the same
  discipline or an internal lock in the Server.

---

## The Wails surface — the frontend's entire view of Go

**Wails binds every exported method of `*App`, so this list IS the contract.** Declared across
`app.go`, `app_picture.go`, `app_return.go`, `app_mixer.go` and `app_remote.go`.

The **Remote** column is added 2026-08-12: the LAN bridge exposes a HAND-WRITTEN allowlist
(`remoteAllowlist` in `app_remote.go`), never a reflective dispatch, so a method's remote
reachability is a decision recorded here and drift-guarded by `TestRemoteAllowlistCoversEveryBoundMethod`
(every exported `*App` method must be classified). The column reads *capability* / `host-only`:
**view** ⊂ **operate** ⊂ **mixer**; a **host-only** method is refused for every remote client at
every capability AND omitted from the hello frame, so it degrades by omission on the remote page.

| Bound method | Returns | Caller | Remote |
|---|---|---|---|
| `ListInputDevices()` | `[]gst.Device` | WP-5b | view |
| `ListOutputDevices()` | `[]gst.Device` | WP-5b | view |
| `GetConfig()` | `*config.Config` | WP-5b | view |
| `SaveConfig(c)` | `error` | WP-5b | operate |
| `SetSecret(key, value)` | `error` | WP-5b | operate |
| `Start()` / `Stop()` | `error` | WP-5b | operate |
| `GetKVSCredentials()` | `kvs.Credentials` | WP-5a | view |
| `GetStatusKeyCandidates()` | `[]m2lx.StatusKeyCandidate` | WP-5b | view |
| `StartPicture()` / `StopPicture()` | `error` | WP-5b | **host-only** |
| `GetPictureState()` | `gst.PictureState` | WP-5b | view |
| `SetPictureRect(x,y,w,h,ratio)` | `error` | WP-5b | **host-only** |
| `SetPictureVisible(visible)` | `error` | WP-5b | **host-only** |
| `IsSRTReturnSelected()` | `bool` | WP-5b | view |
| `StartReturn()` / `StopReturn()` | `error` | WP-5b | **host-only** |
| `GetReturnState()` | `gst.ReturnState` | WP-5b | view |
| `GetMixerSnapshot()` | `mixer.Snapshot` | WP-M4 | view |
| `ArmMixer()` | `MixerArmState` | WP-M4 | operate |
| `DisarmMixer()` | `error` | WP-M4 | view |
| `SendMixerCommands(cmds)` | `error` | WP-M4 | mixer + arm-owner |
| `GetMixerGolden()` | `*mixer.Snapshot` | WP-M4 | view |
| `SetMixerGolden(s)` | `error` | WP-M4 | mixer |
| `ListPresets()` | `[]presets.Summary` | WP-5b | view |
| `SavePreset(name)` | `presets.Summary` | WP-5b | operate |
| `ApplyPreset(id)` | `*config.Config` | WP-5b | operate |
| `RenamePreset(id, name)` | `error` | WP-5b | operate |
| `DeletePreset(id, alsoDeleteCredentials)` | `error` | WP-5b | operate |
| `GetActivePreset()` | `presets.ActiveRecord` | WP-5b | view |
| `GetPresetCredentialStatus()` | `PresetCredentialStatus` | WP-5b | view |
| `GetRemoteState()` | `RemoteState` | WP-5b | **host-only** |
| `SetRemoteListener(enabled, bind, port)` | `error` | WP-5b | **host-only** |
| `AddRemoteClient(name, caps)` | `error` | WP-5b | **host-only** |
| `SetRemoteClientPassword(name, password)` | `error` | WP-5b | **host-only** |
| `DeleteRemoteClient(name)` | `error` | WP-5b | **host-only** |

The five remote-access methods live in `app_remote.go` and are ALL host-only: they change WHO may
connect and on WHAT address, so a remote client must never reach them — the remote dispatcher
refuses every one at every capability. They are on the bound surface solely so the LOCAL Settings
screen can configure the listener. `GetRemoteState` reports a *has-password* boolean per client,
never a hash — the second and last recorded narrowing of "no secret crosses this boundary
outbound", after `GetPresetCredentialStatus`. `SendMixerCommands` additionally requires the caller
to be the seat that armed (`mixArmedBy`); a write from any other seat is refused with
`mixer.ErrDisarmed`, so one operator's arm cannot authorise another's write to the live desk.

The seven preset methods live in `app_presets.go`. `ApplyPreset` **returns the merged config and
the page assigns it over its whole cache** — that return type is a correctness contract (the next
dropdown change re-writes the whole `currentConfig`, so an apply that returned only an error would
be clobbered by the stale cache). It **refuses while a session is SENDING**, in Go, and that
refusal is load-bearing: it is what makes the unconditional monitor/picture rebuild on apply
affordable. `GetPresetCredentialStatus` is the recorded exception to "no getter" — existence
booleans, never a value; see the `internal/secrets` amendment above.

Events emitted Go → JS: **`status`** (an `m2lx.Status`), **`sender`** (a `sender.State`),
**`return`** (a `gst.ReturnState`), **`picture`** (a `gst.PictureState`),
**`statusKeyCandidates`** (a `[]m2lx.StatusKeyCandidate`), **`error`** (a string),
**`levels`** (the input meters: `{peak, rms []float64}` in dBFS, ≤20/s while a
session runs, one all-`-100` zero-frame on stop), and — added 2026-08-12 with the LAN bridge —
**`config`** (`{config: *config.Config, origin: string}`, emitted from `SaveConfig` after the write
so a SECOND controller can refresh; `origin` is the id of the seat that saved, so a page ignores the
echo of its own save) and **`remote`** (`[]{name, addr}`, the currently-connected remote seats, for
the home-screen indicator that lets the operator at the desk see that someone else has a seat).

**Every event ALSO reaches the LAN bridge.** `app.go`'s event pump tees each event to
`remote.Server.Broadcast` at the single `wailsruntime.EventsEmit` tap point, so a remote seat sees
exactly the stream the local page sees. The bridge is App-agnostic; it learns the event names from
`Options.Events` (`remoteEventNames()`), not from any hard-coded list.

**There is no `mixer` event, and that is deliberate.** It is not known whether a routing or mute
change is pushed as a whole-node state at `"/"` or as a subtree delta, so a pushed "latest known"
frame would be precisely the stale frame the drawer must not be given —`set_routing` is an absolute
replace, so a write planned on a stale matrix is one intended change plus a rollback of every other
bus on that strip. `GetMixerSnapshot` therefore **caches nothing** and opens a fresh connection per
call, and the drawer polls it. If the open question is answered and changes do arrive at `"/"`, a
delta-fed subscription can replace the polling.

Headphone enumeration and selection for the **WebRTC** return are JavaScript-side only
(`enumerateDevices` + `setSinkId`). `ListOutputDevices` is the **WASAPI** list, for the SRT return
only. The two identifier spaces are not interchangeable — see `internal/config`.

**`backend.js`'s JS-side mirror of the LAN bridge (added 2026-08-12, WP-5b).** The remote transport
stays entirely behind the shim — `backend.js` contains no `fetch(` and no `WebSocket`
(`remotewiring.test.js` guards both) — but it gained the ordinary Go-facing wrappers the shell needs:
the event names `EVENT_CONFIG`/`EVENT_REMOTE` with `onConfig`/`onRemote` subscribers (mirroring
`onStatus`/`onLevels`); `isRemoteClient()`, which reads the `window.__wslcommsRemote` the shim
publishes; and the five host-only remote-admin wrappers `getRemoteState`/`setRemoteListener`/
`addRemoteClient`/`setRemoteClientPassword`/`deleteRemoteClient`, each with an in-memory fake for
`npm run dev` and an availability probe `remoteAvailable()` (all-or-nothing, like
`presetsAvailable`/`pictureAvailable`). The Settings "Remote access" group drives those five and is
hidden when `isRemoteClient()`; `app.js` subscribes to `onConfig` (ignoring the echo of its own save
by comparing `origin` against `local-webview2`/`window.__wslcommsRemote.client`), wires `onRemote` to
a home-screen seat indicator, and gates the destructive `beforeunload` `stopReturn()`/`stopPicture()`
on `!isRemoteClient()`. The `local-webview2` id is a string contract mirrored from `app_remote.go`'s
`localClientID`.

---

## Frozen dependencies

Go — every one of these is already in `go.mod` and `go.sum`:

| Module | Version | Used by |
|---|---|---|
| `github.com/wailsapp/wails/v2` | v2.13.0 | `main.go` (WP-8) |
| `github.com/go-gst/go-gst` | v0.0.2 | `internal/gst` cgo build only (WP-3a) |
| `github.com/danieljoos/wincred` | v1.2.3 | `internal/secrets` (WP-1) |
| `github.com/go-gst/go-glib` | v0.0.2 | `internal/gst` cgo build only |
| `github.com/gorilla/websocket` | v1.5.3 | `internal/m2lx`, `internal/mixer`, `cmd/mockm2lx` |
| `github.com/aws/aws-sdk-go-v2` | v1.43.2 | `internal/kvs` (WP-4) |
| `github.com/aws/aws-sdk-go-v2/config` | v1.32.33 | `internal/kvs` (WP-4) |
| `github.com/aws/aws-sdk-go-v2/service/cognitoidentity` | v1.36.2 | `internal/kvs` (WP-4) |
| `github.com/datarhei/gosrt` | v0.11.0 | `cmd/mockm2lx` **only** (WP-7) |

npm — already in `frontend/package.json` and `frontend/package-lock.json`:

| Package | Version | Used by |
|---|---|---|
| `amazon-kinesis-video-streams-webrtc` | 2.8.1 | WP-5a |
| `@aws-sdk/client-kinesis-video` | 3.1099.0 | WP-5a |
| `@aws-sdk/client-kinesis-video-signaling` | 3.1099.0 | WP-5a |
| `vite` (dev) | 7.3.6 | build |

Several packages carry a line of the form `var _ = pkg.Symbol` with a comment saying it keeps
`go mod tidy` from pruning a frozen dependency. **Delete that line when you write the real
implementation** — it is scaffolding, not contract.

---

## The original five spikes, and what they turned out to be

- **SP-1 — KVS endpoint shapes.** Answered. Used at Gate C and on air. `internal/kvs` still codes
  defensively and every error it returns names the exact field that was missing or wrong.
- **SP-2 — does go-gst v0.0.2 build under MinGW?** Yes. The ~200-line cgo shim fallback was not
  needed. `internal/gst/BUILD-NOTES.md` is the long-form record.
- **SP-3 — is the top-ranked H.264 encoder `mfh264enc`?** **No**, and the instruction has been
  reversed on measurement. On the target machine `nvh264enc` ranks 257 and `mfh264enc` 128, and
  resolving by rank makes the encode depend on the graphics card while silently skipping the
  property set. The encoder is now chosen by **preference**, with rank as an exclusion and a
  tie-break. Spec v3 §5.1.
- **SP-4 — the production `statusKey`.** Config, not code, and deliberately **not required to
  start**: it cannot be derived from any REST endpoint, so requiring it would make the application
  unstartable until the operator had guessed a value nothing in the API can tell them.
  `GetStatusKeyCandidates` exists to help them find it.
- **SP-5 — the SRT passphrases and key lengths.** Config, not code, and there are now **three**
  credentials because encryption is per output.

## Known unknowns you must not paper over

The full list, with the test that would settle each, is spec v3 §14. The four that constrain what
code in this tree may assume:

- **Nobody knows whether a `stream_state` change is ever pushed as a delta**, or whether it only
  appears in the connect snapshot. Over 3180 observed frames no input changed state.
  `internal/m2lx`'s `resyncInterval` is an explicit backstop against that one assumption, and the
  same question is why there is no `mixer` event. Do not delete the backstop and do not add the
  event until somebody has watched a real transition and recorded its `(node, path)`.
- **The negotiated SRT latency has never been read**, on any of the three paths. What exists is
  end-to-end time-to-first-frame at three settings, which is evidence and not the number.
- **Absolute glass-to-glass picture latency is unmeasured** and needs a reference clock at the
  source. The 1.2 ms figure is transit from `srtsrc`'s src pad to the sink — the part this
  application controls, not the whole path.
- **No match-length soak has been run.** Everything in spec §6 is defended by construction and by
  unit tests, not by having survived a match.

And the one that governs the whole UI: **there is no reliable in-app proof that commentary is on
air.** The permanent line that used to say so — *"Your feed is reaching the switcher. This does not
confirm you are audible on the broadcast output."* — has been **withdrawn from the GUI at the
operator's instruction**, and that is recorded as a deliberate change rather than a tidy-up.
`deriveHonestLine` and its per-state wording are kept and tested in `frontend/src/ui/lamps.js` with
no caller, so putting the line back is a change to `home.js` alone. The fact underneath it has not
changed. Do not build a lamp, a badge or a message that claims otherwise.
