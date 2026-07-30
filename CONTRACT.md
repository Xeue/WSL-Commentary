# CONTRACT — read this before you write a line

**Written by WP-0. Everything here is frozen.** Nine work packages run in parallel against it.

Authority, in order: [`docs/windows-app-spec.md`](docs/windows-app-spec.md) v2 and
[`docs/project-plan.md`](docs/project-plan.md) §2 decide what is built.
[`docs/architecture.md`](docs/architecture.md) and [`docs/test-results.md`](docs/test-results.md)
are the measurement record behind them — read the sections relevant to your package before you
assume a wire shape, because several are already measured and several documented "unknowns" are
things you must not paper over.

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

---

## Ownership map

| Path | Owner | Responsible for |
|---|---|---|
| `go.mod`, `go.sum` | **WP-0 — FROZEN** | every Go dependency, pre-declared |
| `frontend/package.json`, `frontend/package-lock.json` | **WP-0 — FROZEN** | every npm dependency, pre-declared |
| `assets/slate.png` | **WP-0** | 1920x1080 slate fed to `filesrc ! pngdec ! imagefreeze` |
| `CONTRACT.md` | **WP-0** | this file |
| `main.go`, `main_nocgo.go`, `app.go` | **WP-8** | Wails bindings, wire-up, events, end-to-end on the mock |
| `internal/config/` | **WP-1** | `%APPDATA%\WSLComms\config.json`, spec §9 |
| `internal/secrets/` | **WP-1** | Windows Credential Manager, `WSLComms/m2lx` and `WSLComms/srt` |
| `internal/m2lx/` | **WP-2** | sign-in, token refresh, status WebSocket, 4 s debounce, 15 s staleness |
| `internal/gst/` | **WP-3a** | the only cgo surface: pipeline, device monitor, sink swap, **and the stub twin** |
| `internal/sender/` | **WP-3b** | spec §6 in full: timestamp pinning, reconnect state machine, backoff ladder |
| `internal/kvs/` | **WP-4** | M2L-X → Cognito credential chain |
| `frontend/src/monitor/` | **WP-5a** | KVS viewer, 8 transceivers, mosaic crop, Web Audio return, `setSinkId` |
| `frontend/src/ui/`, `frontend/src/styles/`, `frontend/index.html`, `frontend/src/main.js` | **WP-5b** | controls, lamps, Settings view, the honest line |
| `build/` | **WP-6** | DLL allowlist script, LGPL notices and written offer, installer |
| `cmd/mockm2lx/` | **WP-7** | mock REST, status WS, SRT listener, **fault injection** |
| `docs/` | nobody — read-only | the spec and the plan |

`frontend/vite.config.js` does not exist yet. If WP-5b needs one, WP-5b creates it; nobody else.

---

## Gate A is the definition of done for every Wave 2 package

This machine has Go and Node and nothing else — no MinGW gcc, no GStreamer, no Wails CLI, no
M2L-X instance. All three of these must pass, at every commit, from the repo root:

```
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test ./...
```

That is only possible because `internal/gst` is the **only** package that touches cgo, and it
has two implementations selected by build tag:

- `internal/gst/gst_cgo.go`  `//go:build cgo`  — the real go-gst pipeline. Stubs today; WP-3a
  fills it in. Cannot be compiled until Gate B.
- `internal/gst/gst_stub.go` `//go:build !cgo` — a working pure-Go fake: three plausible
  devices and a pipeline whose transitions are driven programmatically. **This one is real code
  and is meant to work.** It is what makes WP-3b, WP-5b and WP-8 testable today.

`main.go` and `app.go` are `//go:build cgo` because Wails links WebView2 through cgo.
`main_nocgo.go` supplies a `main()` for `CGO_ENABLED=0` that prints one line and exits.

**Do not add a cgo import to any package other than `internal/gst`.** If you think you need
one, you have found a design problem, not a missing import.

---

## Package summaries

Read the doc comments in the source; they are the contract. What follows is the index.

### `internal/config` — WP-1
`Config` with the exact JSON field names of spec §9. `Defaults()` (already written: it is a
constant table, not logic), `Path()`, `Load()`, `(*Config).Save()`. Documented defaults:
`srtLatencyMs` 120, `returnMid` 2, `monitorTile` `{0,360,640,360}`, `slatePath` `slate.png`.
Load on a missing file returns `Defaults()` and a nil error — first run is not an error.

### `internal/secrets` — WP-1
`Store` with `Get(key)`/`Set(key, value)`. Keys are `KeyM2LX` (`"m2lx"`) and `KeySRT` (`"srt"`),
mapping to Credential Manager targets `WSLComms/m2lx` and `WSLComms/srt`. `ErrNotFound` is a
normal first-run condition, not a failure. Secrets never enter `config.json`, a log line, a
GStreamer URI, or the Wails boundary in the outbound direction.

### `internal/m2lx` — WP-2
`Client` (`SignIn`/`Refresh`/`Token`/`KVSInfo`/`KVSToken`) and `Watcher` (`Watch`).
The sign-in body field is **`alias`**, not `username`; `username` returns HTTP 500.
`Status.Audio` is a **slice** because an empty slice is the MP2/AC-3 silent-drop signature and
every caller must be able to see it. `Status.Stale` carries the 15 s staleness verdict, which
`WP-5b` renders as `STATUS UNAVAILABLE` with the three WebSocket lamps greyed. `DebounceWindow`
is 4 s, `StaleAfter` 15 s.

`Status` is the app's **normalised** type, not the wire type: WP-2 unmarshals M2L-X's payload
into its own private structs and produces this. The measured wire values of
`streams.video.format` and `streams.audio[].format` are single strings — `"h264 1920x1080 50 P"`
and `"aac 48000 2ch"` — so `VideoFormat`/`AudioFormat` carry a `Raw` field alongside the parsed
fields. A format the parser does not understand must surface as `Raw` rather than read as zero.

Token handling: `TokenLifetime` is the measured 86399 s and `RefreshFraction` is 0.5, via
`/api/local_auth/refresh_token`. The refresh token's own TTL is unmeasured, so a failed
`Refresh` must fall back to a full `SignIn`.

### `internal/gst` — WP-3a
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

**SP-1 is partly answered already** — `docs/test-results.md` §2.4 item 6 records one measured
response of each endpoint:

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

It is still one sample from one instance. If a live instance disagrees, that is a change to
`m2lx.KVSInfo`/`m2lx.KVSToken` and must be reported under rule 3.

### `cmd/mockm2lx` — WP-7
`github.com/datarhei/gosrt` is a **mock-only** dependency. It is a pure-Go SRT implementation so
that the mock can run an SRT listener at Gate A with no libsrt. It must never be imported by
anything under `internal/`; the production path is GStreamer's `srtsink` over libsrt.
Most of WP-7's value is fault injection: drop the session, hold the listener socket open after a
disconnect, stall the status WebSocket, and lie about `stream_state`.

---

## The Wails surface — the frontend's entire view of Go

Declared in `app.go`. WP-5a and WP-5b code against exactly this.

| Bound method | Returns | Caller |
|---|---|---|
| `ListInputDevices()` | `[]gst.Device` | WP-5b |
| `GetConfig()` | `*config.Config` | WP-5b |
| `SaveConfig(c)` | `error` | WP-5b |
| `SetSecret(key, value)` | `error` | WP-5b |
| `Start()` / `Stop()` | `error` | WP-5b |
| `GetKVSCredentials()` | `kvs.Credentials` | WP-5a |

Events emitted Go → JS: **`status`** (an `m2lx.Status`), **`sender`** (a `sender.State`),
**`error`** (a string).

Headphone enumeration and selection are **JavaScript-side only** (`enumerateDevices` +
`setSinkId`). No Go package owns output devices.

---

## Frozen dependencies

Go — every one of these is already in `go.mod` and `go.sum`:

| Module | Version | Used by |
|---|---|---|
| `github.com/wailsapp/wails/v2` | v2.13.0 | `main.go` (WP-8) |
| `github.com/go-gst/go-gst` | v0.0.2 | `internal/gst` cgo build only (WP-3a) |
| `github.com/danieljoos/wincred` | v1.2.3 | `internal/secrets` (WP-1) |
| `github.com/gorilla/websocket` | v1.5.3 | `internal/m2lx`, `cmd/mockm2lx` (WP-2, WP-7) |
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

## Known unknowns you must not paper over

- **SP-1** — one sample of each KVS endpoint exists (see `internal/kvs`). One sample from one
  instance is not a confirmed contract; WP-4 and WP-5a should code defensively and say so.
- **SP-2** — nobody has built go-gst v0.0.2 under MinGW. If it fights, the agreed fallback is a
  ~200-line hand-written cgo shim behind the same `internal/gst` signatures. That is a change
  inside `gst_cgo.go`, not a contract change.
- **SP-3** — resolve the H.264 encoder **by rank at runtime**. Do not hardcode `mfh264enc`.
- **SP-4 / SP-5** — `statusKey` and the SRT passphrase/`pbkeylen` are config, not code.

And the one that governs the whole UI: there is no reliable in-app proof that commentary is on
air. The line under the lamps — *"Your feed is reaching the switcher. This does not confirm you
are audible on the broadcast output."* — is permanent and is not a placeholder.
