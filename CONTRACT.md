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

   **RELAXED BY THE OWNER 2026-08-15: "the contracts rule 1 is overzealous, we can relax it."**
   Adding a dependency is now allowed and these three files are no longer frozen. The rule is
   amended rather than deleted because its *reason* survives its prohibition: concurrent edits to
   dependency manifests still lose work, so an agent adding one should say so where the other
   agents will see it. **Prefer the stdlib, and say why in a comment when you add anything.**
   Nothing has been added under the relaxation so far — the conform work, the DeckLink device seam
   and the bus-error filter are all stdlib and existing dependencies.

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
| `app_remote.go` (and `app_remote_test.go`) | **WP-8 — added 2026-08-12, reworked to the fully-open posture 2026-08-12** | the App-side of the LAN bridge: the hand-written allowlist that implements `remote.Dispatcher` (method → host-only; no capability tiers, the listener is unauthenticated), the audit log, mixer arm-ownership routing, the two host-only remote-admin bound methods (`GetRemoteState`, `SetRemoteListener`), and the listener's startup/teardown wiring. The transport it drives is `internal/remote` (WP-REMOTE). |
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
| `internal/remote/` (incl. `shim.js`) | **WP-REMOTE — added 2026-08-12, reworked to the fully-open posture 2026-08-12** | the App-agnostic UNAUTHENTICATED LAN control bridge: dual HTTP+HTTPS transport bound to all interfaces, no login/cookie/origin guard (owner's decision — the private network is the access control), session fan-out, the `//go:embed` frontend shim. Pairs with `app_remote.go` (root, WP-8), which implements `remote.Dispatcher`. |
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

**And on macOS, which is now a shipping platform, all three must pass there too, plus the Windows
cross-check** — a change that builds on one and not the other is not done:

```
GOOS=windows CGO_ENABLED=0 go build ./...
GOOS=windows CGO_ENABLED=0 go vet ./...
GOOS=windows CGO_ENABLED=0 go vet -tags "bindings gststub" ./...
```

**`gofmt -l` must be empty over everything except `third_party/`.** That exclusion is not
laziness. `third_party/wails-v2.13.0/` is upstream Wails v2.13.0 verbatim apart from one patch to
two files, and the whole value of vendoring it that way is that `diff -r` against the module cache
reports exactly those two files and no others. Nine upstream files are not gofmt-clean; running
gofmt over them would destroy the audit property that is the entire reason the directory exists.
See `third_party/README.md`.

That property is what lets anyone pick this project up without a 2 GB install, and it is what
keeps the cgo surface small enough to reason about. It is only possible because `internal/gst` is
the **only** package that touches cgo, and each of its three pipelines has two implementations
selected by build tag:

| cgo half | stub twin | supplies |
|---|---|---|
| `gst_cgo.go` | `gst_stub.go` | the contribution pipeline, `ListInputDevices` |
| `picture_cgo.go` | `picture_stub.go` | `newPicturePipe` — one picture attempt |
| `return_cgo.go` | `return_stub.go` | `newReturnPipe`, `ListOutputDevices` |
| `overlay_windows.go` | `overlay_darwin.go` / `overlay_other.go` | the native child window |

Since the macOS port there is a **second** axis of build tags inside the cgo half, and it is a
different axis: not cgo-versus-stub but Windows-versus-macOS. Everything true on every platform
stays in the shared file, and each twin supplies only what genuinely differs:

| shared | Windows twin | macOS twin | supplies |
|---|---|---|---|
| `gst_cgo.go` | `elements_windows.go` | `elements_darwin.go` | the factory names (capture source, AAC encoder, H.264 preference) |
| `gst_cgo.go` | `deviceprovider_windows.go` | `deviceprovider_darwin.go` | which enumerated devices are offered, and how an id resolves |
| `gst_cgo.go` | `gstpaths_windows.go` | `gstpaths_darwin.go` | where the bundled plugins are, and what else `gst_init` needs in the environment |
| `return_cgo.go` | `return_cgo_windows.go` | `return_cgo_darwin.go` (+ `return_cgo_other.go`) | the return decoder, sink and its properties |

The same idiom is used outside `internal/gst` wherever a platform genuinely differs —
`secrets_windows.go`/`secrets_darwin.go`, `applog_windows.go`/`applog_darwin.go`,
`exit_windows.go`/`exit_darwin.go`. **Prefer a build-tagged twin to a `runtime.GOOS` switch.** A
switch compiles the other platform's answer into this platform's binary and invites a third branch
nobody has measured; a twin cannot.

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
  `mediaDeviceId` and the operating system's own device identity (a WASAPI IMMDevice GUID on
  Windows, a CoreAudio device UID on macOS) — and must never be merged. Using one where the other
  belongs fails silently in both directions. That the native half is platform-dependent does not
  soften the rule by one inch: the browser id is not *any* operating system's identifier for a
  device, it is a per-origin salted token minted by one browsing context, so there is no platform
  on which the two converge. A `config.json` carried between the two machines must fail **safe** —
  `gst.chooseOutputDevice` checks the saved id against what the machine is actually offering, falls
  back to the default playback device, and logs the id, the reason and what is on offer instead.
- **On macOS `headphoneEndpointId` is the CoreAudio UID and never the integer.** `osxaudiosink`'s
  own `device` property is a gint `AudioDeviceID` that `coreaudiod` allocates per enumeration and
  reuses, so it survives neither a reboot nor a replug. `internal/gst` resolves the integer from
  the stored UID every time it opens a pipeline; the integer must never reach `config.json`, never
  cross the Wails boundary, and never appear in a log except as a diagnostic.
- **`Save` must never write a secret.** `TestSave_NeverWritesSecretFields` enforces it.

**Four fields added 2026-08-15, rule 3** — `videoBitrateKbps`, `videoFormatOverride` (both
INSTANCE, so both travel in a preset) and `audioSourceKind`, `decklinkPersistentId` (both MACHINE).
Two of them change rules stated above and so are recorded here rather than only in the source:

- **`videoFormatOverride` is a STRING (`"1920x1080p50"`), parsed by exactly one function**
  (`ParseVideoFormat` in `videoformat.go`), and not a nested struct of width/height/rate. Two
  reasons. What the operator typed must stay **visible** — a struct turns a format this build
  cannot express into three plausible zeros, where a string can be quoted back — and this package's
  merge primitive would otherwise be a trap: `Load` and `presets.Apply` unmarshal onto an
  already-populated struct, so a nested object merges **field by field**, and a preset carrying
  `{"width":1280}` would leave height 1080 and conform to 1280x1080. A string cannot half-arrive.
  Frame rates are stored as exact fractions (the NTSC decimals map to n×1000/1001); interlace is
  refused **by name**, because the video leg can only produce progressive.
- **`Validate` refuses an unparseable `videoFormatOverride`. This is a deliberate, single exception
  to "nothing in `Validate` may be a reason a match does not go out"** — and the exception is
  narrow on purpose. A bad value here means the contribution feed **cannot be built either way**,
  so the match does not go out regardless; the only question is whether the operator is told which
  box to fix while they can still fix it, or gets `not-negotiated (-4)` naming nothing, twenty
  seconds after START. **Empty is never an error** — empty means derive, and is the default.
- **`audioDeviceId` is now required only when the capture is native.** Leaving it unconditional
  would make a DeckLink seat unstartable over a subsystem its commentary never touches. See the
  Start-gate note under `app.go` below: until the DeckLink capture leg exists, `Start` refuses a
  DeckLink seat outright, because that relaxation otherwise lets an empty `audioDeviceId` reach
  `osxaudiosrc` and open the **system default input** silently.
- **`frontend/src/ui/validate.js` mirrors `ParseVideoFormat`'s grammar in JavaScript.** It has to:
  `SaveConfig` deliberately does not validate (a half-filled first-run form must be savable), so
  Go's `Validate` runs only at START and the JS is what stops a bad value reaching `config.json` at
  all. The two are pinned together by a 37-case verdict corpus in `settings.test.js`, verified
  against Go's parser directly, plus source-text assertions on the bounds. A string they disagree
  about is a defect either way round.

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

**Added 2026-08-15, rule 3: `SwitcherConfiguration(ctx)`.** `GET /api/v1/switcher_configuration`,
bearer-authenticated like the KVS and events calls, in the same `/api/v1/switcher_*` family as the
`switcher_status` and `switcher_controller` sockets. Its first key is the instance's **configured**
video format — the setting itself, not an observation of a consequence of it:

```
{"format":{"video":{"bit_depth":8,"color_space":"YCbCr","frame_rate":"50",
                    "height":1080,"signal_type":"rec709","width":1920}}, ...}
```

Measured on `matchH` 2026-08-15: 12108 bytes, HTTP 200 in 34 ms, top-level keys
`[format nodes system_info]`. `frame_rate` is a STRING while `width`/`height` beside it are
NUMBERS — the same trap `format.go` already documents, and the same `parseFrameRate` reads it.
`nodes[]` and `system_info` are deliberately unmodelled. `signal_type:"rec709"` is GStreamer's
`bt709`, which is what the video leg already pins.

**This replaced a derivation, and the replacement is the point.** An earlier pass on the same day
inferred the format from any node that was *running*, on the reasoning that every source must match
the switcher so a streaming node reports it. That is true and useless: with a real 1280x720p50 feed
accepted and streaming on `cam4 "COMMS"`, `matchH` reported `frame_rate="0"`, stable across 45 s.
The derivation correctly refused to build a 0 fps target and fell back — so it would have shipped
and silently never fired. And at the moment of the live check, 24 of 24 router inputs were stopped
with null formats, so it had nothing to read at all. The setting is always there.
`internal/m2lx/configuration.go` carries that measurement verbatim, so nobody rebuilds it.

The earlier note that "seven plausible REST paths all answered 404" was correct about those seven
and wrong to conclude no endpoint existed. It was found the way `/api/events/overview` was found:
by reading the switcher's own Angular bundle.

`GET /api/input/router/list/{eventId}` **does** state the configured format per router input —
measured on `matchH`, input 4 `{"name":"COMMS","port":40004,"width":1920,"height":1080,"codec":
"h264","frame_rate":50}` — and it answers while every node's format is still null. It is not used
yet. Three traps for whoever wires it, all re-measured against `matchH` on 2026-08-15:

1. `frame_rate` is a **number** there (`50`) and the **string** `"50"` on the status socket.
2. **Every** input answers with a plausible raster whether or not it is ours — the list came back
   with **48** entries, all `1920x1080p50 progressive`, of which exactly one was `h264` and the
   other 47 `h265` — so the match must be exact on `port` against `srtPort`. Matching on the first
   plausible raster, or on the codec, reads somebody else's input and turns a green lamp into a lie.
3. **`{eventId}` is not the sign-in response's `id`.** That field is the *user* id
   (`da8c3058-…`), and using it returns `404 {"detail":"event_id not found"}`. The event id is
   `event_id` from `GET /api/events/overview` (`9or-l7xtm4y8-sy5x` on `matchH`) — the same value
   `internal/m2lx.ListEvents` already returns as `Event.ID`.

This is not the `docs/architecture.md` ban on REST format — that ban is on reading REST as
**detection**, and detection still comes from `switcher_status` alone.

### `internal/gst` — the contribution pipeline — WP-3a
`Device{ID, Name, Kind}`; `ID` is the IMMDevice endpoint GUID and is the only thing persisted or
passed to `wasapi2src`. `Pipeline` with `Start(PipelineOpts)`, `ReplaceSink(SinkOpts)`,
`ForceKeyUnit()`, `Errors()`, `Stop()`; package functions `Init(appDir)`, `ListInputDevices()`,
`New()`.

**`Device.Kind` added 2026-08-15, rule 3.** `DeviceKind` is `"native"` (the platform's own audio
stack) or `"decklink"` (a Blackmagic card from GStreamer's decklink provider), and it exists
because `ListInputDevices` now enumerates **both**. The two ID spaces are different kinds of
identifier in exactly the sense this document uses of `headphoneDeviceId` / `headphoneEndpointId`,
and neither element reports a stranger's id as a stranger's — `osxaudiosrc` handed a DeckLink
persistent-id falls back to the **system default input**, silently. It is a **separate field and
never a prefix inside `ID`**, because `ID` is opaque to everything above this package, and it is
**not persisted**: `config.json` keeps the id alone, so no migration and no new entry in
`internal/presets/fields.go`. The empty string means "written before this field existed" and reads
as native — use `NormaliseDeviceKind`, never a comparison against `""`.

The one measurement that decides the design: the UltraStudio 4K Mini publishes `persistent-id`
`gint64 2747401380` and **no** `unique-id`, while its CoreAudio twin publishes `unique-id`
`"90:a3c204a4:00000000:Audio"` — and `0xa3c204a4` **is** 2747401380. The same card, twice, under
names an operator cannot tell apart, and **the twin a clever de-duplication would discard is the
one carrying the microphone** (the native twin measures -96 dBFS on all sixteen channels with the
mic live). Hence the de-dup key is scoped by kind, and hence the dropdown labels rather than
merges.

**The bus error filter is no longer `isSinkSourced` alone** (`capturefault.go`,
`capturefault_cgo.go`, added 2026-08-15, rule 3). This **reverses a documented invariant that
`internal/sender`'s design rested on** — "everything that is not sink-sourced is pipeline-fatal" —
so it is recorded here rather than only in the source. Errors are now classified by the element
that posted them: sink-sourced is unchanged and tested first; a **video capture** failure is
**recoverable** (the gate is not closed, the pipeline stays PLAYING, it is delivered as a warning
and never reaches `Errors()`, because `internal/sender` treats any error arriving while CONNECTED
as the peer going away and would spend seven seconds off air on a fault that never touched the
feed); an **audio capture** failure is fatal but **named**, separating device-missing, device-busy
and no-signal, which are one generic stream error at this level and have three different fixes.
The classification must run **before** the gate close, not merely replace the `isSinkSourced` test:
closing the gate on a video fault would stop media reaching the sink and starve the SRT peer,
defeating the sparing.

**Element names in `pipelineDescription` are therefore load-bearing.** The audio capture element
is `asrc` whatever factory is behind it; every element of a video capture leg carries the `vcap`
prefix. An element that does not rejoins the fatal default **silently** — the failure direction is
safe and therefore invisible — which is why the names are constants in `capturefault.go`.

**The video leg's two capsfilters are rendered from `PipelineOpts.ConformTo`, not constants.**
`ConformTarget{Width, Height, FrameRateNum, FrameRateDen}`, whose **zero value means "nothing is
known"** and resolves to `FallbackConformTarget()` = 1920x1080p50 — the value that used to be
hardcoded, so an unread switcher behaves exactly as before. The rate is an exact fraction because
GStreamer's `framerate` caps field takes nothing else; `ConformTargetFromRate` converts the
switcher's decimal, recognising the 1000/1001 family. **There is deliberately no parser in this
package**: `internal/config` cannot import `internal/gst` (a config package needing GStreamer
installed to be tested stops being tested), so the one grammar lives in `config.ParseVideoFormat`
and `ConformTarget`'s field names match `config.VideoFormatSpec`'s so the bridge is a
transcription. A second parser here would be a second grammar that drifts.

Two contract points that are easy to get wrong and cost a day each:

- **`Start` installs no sink.** It plays the capture/encode/mux chain with the `srtq` src pad
  blocked. The first `ReplaceSink` installs the first sink. That is what lets the chain stay in
  PLAYING for the life of the process, which is the structural fix for the backwards-DTS bug.
- **`Errors()` is closed by `Stop()`**, and implementations drop rather than block when it is
  full. Synchronous failures come back from the method that caused them and never appear there.

The H.264 encoder is resolved at runtime **by preference, not by rank** — on Windows `mfh264enc`,
then `qsvh264enc`, `nvh264enc`, `d3d11h264enc`, `amfh264enc`; on macOS `vtenc_h264` then
`vtenc_h264_hw` — with `x264enc` denylisted for its licence on both. Rank only excludes factories
GStreamer has marked unusable and tie-breaks equals. Spec v3 §5.1 has the measured ranks and the
argument; the short version is that rank picks whichever GPU vendor's element is installed, and the
property set was written against `mfh264enc`. Also: **`mfh264enc` has no `bframes` property**, and
`gst_parse_launch` rejects an unknown property outright.

The other two platform-dependent factories are the **AAC encoder** (`mfaacenc` / `atenc`) and the
**capture source** (`wasapi2src` / `osxaudiosrc`). All three seams live in `elements_windows.go`
and `elements_darwin.go`, pinned by `TestPlatformElementContractIsPinned`; every other factory in
the send pipeline is identical on both platforms under the same name.

**`x264enc` is a live candidate on macOS, not a theoretical one.** It is present in a stock
Homebrew GStreamer and ranks primary (256) — the same rank as `vtenc_h264` — so the denylist in
`gst_cgo.go` is load-bearing there in a way it never was on Windows. That denylist is a run-time
control and not a licensing one; what keeps GPL code out of the shipped artefact is
`build/forbidden-names.ps1`, which is now applied at three points by
`build/bundle-gst-darwin.sh` (wanted list, computed closure, staged tree) exactly as
`build/bundle-gst.ps1` applies it on Windows.

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

- **ON and all-interfaces by default, UNAUTHENTICATED — the owner's explicit, repeated, final
  decision (reworked 2026-08-12).** `DefaultSettings()` is `enabled:true, bind:"0.0.0.0",
  httpPort:80, httpsPort:443`; a missing `remote.json` yields exactly that, so a fresh machine on the
  facility network is listening out of the box. There is NO login, NO client accounts, NO capability
  tiers, and NO origin/CSRF/cookie guard of any kind — the app runs on a dedicated private network
  and the network is the access control. `Start()` binds nothing when disabled; `Settings.Validate`
  only refuses a bind that is not a literal IP and a port outside `0..65535` (`0` = OS-assigned, for
  tests). The risk write-up is `docs/remote-access.md` (developer-facing) ONLY; the app UI shows the
  bound-port status and nothing else. An old authenticated-era `remote.json` is migrated on load
  (its `clients` dropped, its `port` → `httpsPort`).
- **Dual listeners with fallback.** `Start` binds BOTH plain HTTP (`httpPort`, falling back to
  `8080` when busy) and HTTPS (`httpsPort`, falling back to `8443`) on the same handler — on Windows
  80/443 are frequently held by `http.sys`. HTTPS still matters: over plain HTTP a LAN page is not a
  secure context (`navigator.mediaDevices`/`setSinkId` vanish) and `GetKVSCredentials` would cross
  the LAN in clear, so the browser should use `https://`. The self-signed ECDSA P-256 cert is
  generated into `%APPDATA%\WSLComms\remote\`; because the bind is `0.0.0.0` its SANs cover **every
  non-loopback interface IP** + loopback + `localhost` + hostname, regenerating when the interface-IP
  set grows. Its SHA-256 fingerprint is `(*Server).Fingerprint()`; addresses are `HTTPURL()`/
  `HTTPSURL()`/`HTTPAddr()`/`HTTPSAddr()`/`HTTPPort()`/`HTTPSPort()`.
- **`remote.json` is deliberately NOT in `config.json`.** `settings.js collectConfig()` rewrites the
  whole config document from a page cache and drops any field it does not restate; a listener bind
  address must not be reachable by that mechanism. There are no passwords on disk any more — the
  listener is unauthenticated.
- **The upgrade is unconditional.** `handleWS` upgrades ANY connection; the `Upgrader`'s
  `CheckOrigin` returns `true`. No cookie, no origin check, no login endpoint. `/__wslremote/login`
  is gone; the reserved routes are `/__wslremote/ws` and `/__wslremote/shim.js`.
- **The hello frame's `methods` list is authoritative** — the shim installs exactly those functions
  on `window.go.main.App`, so **host-only methods degrade by OMISSION**, not by a refusal the
  frontend must be taught. `SetPictureRect`/`SetPictureVisible`/`StartPicture`/`StopPicture`/
  `StartReturn`/`StopReturn` are host-only: absent from `Methods()` and refused by `Call` for every
  connection (enforced in `app_remote.go`; the package's fake dispatcher mirrors it).
- **The fan-out never blocks a producer.** Each session has a bounded, drop-oldest event queue with
  a per-connection monotonic `seq` (mirroring `app.go`'s `eventPump`); results ride a separate queue
  that cannot overflow under the in-flight cap. A client that will not drain is dropped by the write
  deadline, never allowed to stall `Broadcast`. Disconnect cancels every in-flight call's context.
- **The shim (`shim.js`, `//go:embed`) is the entire frontend side and needs zero `backend.js`
  changes.** It is a classic script injected before the deferred module bundle, installs
  `window.go`/`window.runtime` (queuing calls until the socket opens), prunes to `methods` on hello,
  and sets `window.__wslcommsRemote = {client}` (`client` is this connection's id, used to recognise
  the echo of its own `SaveConfig`; there is no `caps`). It connects straight to `/__wslremote/ws`
  with no login step and derives `ws://` vs `wss://` from `window.location.protocol` so it works over
  both HTTP and HTTPS. It is DOM-optional so a headless test can drive its transport; the shim's node
  test is left to WP-5b's `remotewiring.test.js`.
- **`Start()` must complete-before `Fingerprint()`/`Close()` (usage contract, noted 2026-08-12 by
  WP-8).** `Server.Start` writes `fp`/`httpSrv`/the listeners/addrs without a lock, on the assumption
  the caller does not read them concurrently. Because the design starts the listener on a goroutine (so
  startup never blocks on the ECDSA keygen), `app.go` establishes that ordering by publishing the
  `*Server` pointer to its atomic field ONLY AFTER `Start` returns: a reader that Loads a non-nil
  pointer has a happens-before edge to `Start`'s writes, and a reader that Loads nil correctly sees
  "not running yet". A future direct, concurrent caller of `Start`+`Fingerprint` would need the same
  discipline or an internal lock in the Server.

---

## The Wails surface — the frontend's entire view of Go

**Wails binds every exported method of `*App`, so this list IS the contract.** Declared across
`app.go`, `app_picture.go`, `app_return.go`, `app_mixer.go` and `app_remote.go`.

The **Remote** column is added 2026-08-12 (reworked 2026-08-12 to the fully-open posture): the LAN
bridge exposes a HAND-WRITTEN allowlist (`remoteAllowlist` in `app_remote.go`), never a reflective
dispatch, so a method's remote reachability is a decision recorded here and drift-guarded by
`TestRemoteAllowlistCoversEveryBoundMethod` (every exported `*App` method must be classified). By the
owner's explicit, repeated, final decision the listener is **UNAUTHENTICATED** — it runs on a
dedicated private facility network and the network is the access control (see `docs/remote-access.md`)
— so there are **no client accounts and no capability tiers**. The column now reads `open` /
`host-only`: an **open** method is reachable by every connection; a **host-only** method is refused
for every connection AND omitted from the hello frame, so it degrades by omission on the remote page.
`SendMixerCommands` is additionally gated on the caller being the seat that armed (arm-ownership —
about WHICH seat holds the open window, not authentication), shown as `open + arm-owner`.

| Bound method | Returns | Caller | Remote |
|---|---|---|---|
| `ListInputDevices()` | `[]gst.Device` | WP-5b | open |
| `ListOutputDevices()` | `[]gst.Device` | WP-5b | open |
| `GetConfig()` | `*config.Config` | WP-5b | open |
| `SaveConfig(c)` | `error` | WP-5b | open |
| `SetSecret(key, value)` | `error` | WP-5b | open |
| `Start()` / `Stop()` | `error` | WP-5b | open |
| `GetKVSCredentials()` | `kvs.Credentials` | WP-5a | open |
| `GetStatusKeyCandidates()` | `[]m2lx.StatusKeyCandidate` | WP-5b | open |
| `StartPicture()` / `StopPicture()` | `error` | WP-5b | **host-only** |
| `GetPictureState()` | `gst.PictureState` | WP-5b | open |
| `SetPictureRect(x,y,w,h,ratio)` | `error` | WP-5b | **host-only** |
| `SetPictureVisible(visible)` | `error` | WP-5b | **host-only** |
| `IsSRTReturnSelected()` | `bool` | WP-5b | open |
| `StartReturn()` / `StopReturn()` | `error` | WP-5b | **host-only** |
| `GetReturnState()` | `gst.ReturnState` | WP-5b | open |
| `GetMixerSnapshot()` | `mixer.Snapshot` | WP-M4 | open |
| `ArmMixer()` | `MixerArmState` | WP-M4 | open |
| `DisarmMixer()` | `error` | WP-M4 | open |
| `SendMixerCommands(cmds)` | `error` | WP-M4 | open + arm-owner |
| `GetMixerGolden()` | `*mixer.Snapshot` | WP-M4 | open |
| `SetMixerGolden(s)` | `error` | WP-M4 | open |
| `ListPresets()` | `[]presets.Summary` | WP-5b | open |
| `SavePreset(name)` | `presets.Summary` | WP-5b | open |
| `ApplyPreset(id)` | `*config.Config` | WP-5b | open |
| `RenamePreset(id, name)` | `error` | WP-5b | open |
| `DeletePreset(id, alsoDeleteCredentials)` | `error` | WP-5b | open |
| `GetActivePreset()` | `presets.ActiveRecord` | WP-5b | open |
| `GetPresetCredentialStatus()` | `PresetCredentialStatus` | WP-5b | open |
| `GetRemoteState()` | `RemoteState` | WP-5b | **host-only** |
| `SetRemoteListener(enabled, bind, httpPort, httpsPort)` | `error` | WP-5b | **host-only** |
| `GetConformTarget()` | `*ConformTargetView` (nil when unknown) | WP-5b | open |

`GetConformTarget` is added 2026-08-15 with the conform work. It returns
`{width, height, frameRate, source, raw}` or **null**, and null is the
normal answer for every way of not knowing — `lamps.js` then uses its own documented 1080p50
fallback, i.e. exactly the behaviour this application always had. `frameRate` is the
**operator-facing decimal** (29.97, never 29.970029970…) because the frontend compares it with
`===` against a rate the switcher reports as the string `"29.97"`.

**A running session is answered from the session**, read back rather than re-derived: the lamp's
question is whether what the switcher sees matches what we are **sending**, and what we are sending
was decided at Start from a switcher that may since have changed. With no session it reports the
`videoFormatOverride` only and does **not** dial — this is a UI path, and with nothing sending
there is no feed for the lamp to judge. It is `open` for remote because a remote seat draws the
same status row: refusing it would leave that seat's VIDEO lamp red on a correctly conforming
720p50 feed while the desk's reads green, and two seats disagreeing about a lamp is worse than
either answer alone.

**Two changes to `Start` itself, 2026-08-15, rule 3.**

- **`Start` now makes one extra READ-ONLY status dial before building the pipeline** —
  `Watcher.RawSnapshot`, bounded at 3 s — to derive the conform format. It is done **first**,
  before the `statusKey` discovery is armed, because both touch the status socket and only one of
  them may see our own feed arrive: the discovery's whole method is "which node changed state while
  we were starting". It can never fail a Start; every failure falls through to the next source with
  a log line. The precedence is **switcher, then `videoFormatOverride`, then
  `FallbackConformTarget()`** — the measurement beats the declaration, because the switcher is the
  thing being conformed *to* and a stale override typed for another venue cannot be more true than
  a node that is streaming right now. A disagreement between the two is logged naming both, rather
  than the override being ignored in silence.
- **`Start` refuses `audioSourceKind:"decklink"` outright, and this is the one line to delete when
  the DeckLink capture leg lands.** `config`, the Settings screen and `ListInputDevices` all
  understand the value, but `PipelineOpts` still carries `AudioDeviceID` alone and
  `pipelineDescription` still builds the platform's own source unconditionally. Combined with
  `audioDeviceId` no longer being required for a DeckLink seat, accepting it would mean an empty
  device on `osxaudiosrc`/`wasapi2src` — which is not an error but the **system default input** —
  and the match going out from the laptop's built-in microphone with every lamp green. It is in
  `Start` rather than in `config.Validate` deliberately: it is a statement about what
  `internal/gst` can currently **build**, not about whether the configuration is well formed, and
  the same value becomes valid the day the element exists with no change to `config` at all.

The two remote-access methods live in `app_remote.go` and are BOTH host-only: they change WHETHER
the listener runs and on WHAT address and ports, so a remote connection must never reach them — the
remote dispatcher refuses them from every connection. They are on the bound surface solely so the
LOCAL Settings screen can configure the listener. `GetRemoteState` returns
`{enabled, bind, httpPort, httpsPort, httpURL, httpsURL, certFingerprint}` — **no client list, no
secret** (the listener is unauthenticated, so there is nothing to narrow the "no secret crosses this
boundary outbound" rule for here). The per-client admin methods (`AddRemoteClient`,
`SetRemoteClientPassword`, `DeleteRemoteClient`) are **removed**: there are no client accounts.
`SendMixerCommands` still requires the caller to be the seat that armed (`mixArmedBy`); a write from
any other seat is refused with `mixer.ErrDisarmed`, so one operator's arm cannot authorise another's
write to the live desk — arm-ownership is about which seat holds the open window, not authentication,
so it is unaffected by the move to the unauthenticated listener.

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
publishes; and the two host-only remote-admin wrappers `getRemoteState`/`setRemoteListener`, each
with an in-memory fake for `npm run dev` and an availability probe `remoteAvailable()`
(all-or-nothing, like `presetsAvailable`/`pictureAvailable`). **Reworked 2026-08-12 to the fully-open
posture (Go and JS both done):** the per-client admin wrappers
(`addRemoteClient`/`setRemoteClientPassword`/`deleteRemoteClient`) and the capability concept are
removed — the listener is unauthenticated, so the Settings "Remote access" group is **status-only**:
it reports whether the listener is on and which HTTP/HTTPS ports it bound on, with no configuring
controls, no client management, and — by the owner's minimal-UI decision — no warning, secure-context
note or fingerprint prose (the risk write-up lives in `docs/remote-access.md`). `setRemoteListener`
now takes `(enabled, bind, httpPort, httpsPort)`; `getRemoteState` returns
`{enabled, bind, httpPort, httpsPort, httpURL, httpsURL, certFingerprint}`, and the readout derives
"running" from a non-empty `certFingerprint` (there is no `running` field). The group is still hidden
when `isRemoteClient()`; `app.js` subscribes to `onConfig` (ignoring the echo of its own save by
comparing `origin` against `local-webview2`/`window.__wslcommsRemote.client`), wires `onRemote` to a
home-screen seat indicator, and gates the destructive `beforeunload` `stopReturn()`/`stopPicture()`
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
