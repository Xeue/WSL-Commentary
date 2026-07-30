# RUNNING — how to work on this today

**You are at Gate A.** This machine has Go 1.25.0 and Node 24.6.0 and nothing
else: no MinGW gcc, no GStreamer, no Wails CLI, and the M2L-X dev instance is
powered off. That is not a broken checkout — the project was deliberately built
so that almost all of it works anyway.

Read [`CONTRACT.md`](CONTRACT.md) before editing anything. Authority for *what*
is built is [`docs/windows-app-spec.md`](docs/windows-app-spec.md) v2.

---

## 1. The one command that tells you the tree is healthy

From the repository root, in PowerShell:

```powershell
$env:CGO_ENABLED='0'
go build ./...
go vet ./...
go test ./... -count=1
go test -tags dev . -count=1
gofmt -l .
```

All five must be silent (bar `go test`'s `ok` lines). `gofmt -l .` printing a
filename means that file needs formatting; it should print nothing.

**The `-tags dev` line is not optional and is easy to leave out.** `main.go` and
`app.go` are behind `//go:build dev || production || bindings`, so the plain
`go test ./...` reports `? wslcomms [no test files]` and compiles neither them
nor `app_test.go`. `-tags dev` is what brings the root package — the whole
lifecycle, the event pump, `Start`/`Stop`, the shutdown ordering, and the 52
tests over them — into the run. Nothing about that tag builds or runs a GUI:
see §4.

Last run, 2026-07-30, whole tree:

```
?       wslcomms                        [no test files]
ok      wslcomms/cmd/mockm2lx           1.606s
ok      wslcomms/internal/config        0.589s
ok      wslcomms/internal/gst           0.448s
ok      wslcomms/internal/kvs           1.109s
ok      wslcomms/internal/m2lx          2.101s
ok      wslcomms/internal/secrets       0.494s
ok      wslcomms/internal/sender        1.008s
```

and the root package, which that run skipped — 52 tests, measured separately on
the same day:

```
ok      wslcomms                        0.215s
```

If a package is red when you pull, check whether the failure is in a file you
own before you spend an afternoon on it: this tree is worked on by several
packages at once, and a compile error in someone else's test file is theirs.

The frontend has its own tests, which use Node's built-in runner because
`package.json` is frozen and has no test framework in it:

```powershell
cd frontend
node --test "src/monitor/*.test.js"     # 260 tests
```

Use the quoted glob. `node --test src/monitor/` — the directory form — fails on
this Node version.

### `CGO_ENABLED=0` is not optional

Set it for every command. At `CGO_ENABLED=1` the cgo half of `internal/gst`
enters the build, there is no gcc to compile it, and you learn nothing you did
not already know. It is also how you summon the modal dialog described in §4.

`go test -race` **does not work here at all.** Go's race detector on Windows
requires cgo and a C compiler. Race verification is deferred to Gate B. Until
then the substitute is repetition and deliberate contention:

```powershell
go test ./... -count=20
go test -tags dev . -count=20
```

---

## 2. What works today

| Thing | State |
|---|---|
| `internal/config`, `internal/secrets` | Working and tested. The Credential Manager tests hit the **real** vault, backing up and restoring anything already stored. |
| `internal/m2lx` | Working and tested against its own fakes, **and now against `cmd/mockm2lx` over a real socket** — see §5.1. |
| `internal/sender` | Working and tested in full against a fake pipeline. This is where §6 of the spec lives. |
| `internal/gst` | The **stub twin** works and is what everything else runs against. The real cgo implementation cannot be compiled until Gate B. |
| `internal/kvs` | Written; unverifiable until Gate C, because it needs the live endpoints. |
| `cmd/mockm2lx` | Working. REST, status WebSocket, a real SRT listener, and fault injection. |
| Frontend | Runs in a browser against an in-memory fake backend. |
| `main.go` / `app.go` | Working, and covered by 52 tests that run at Gate A with `go test -tags dev .` — see §5.2. |
| `wslcomms.exe` | **Cannot be built.** Needs the Wails CLI, MinGW gcc and GStreamer. Gate B. |

---

## 3. Running the pieces

### 3.1 The mock M2L-X

```powershell
go run ./cmd/mockm2lx
```

Defaults: `:8080` for HTTP and the status WebSocket, `:4001` for SRT, sign-in
`wsl-comms-ro` / `changeme`, status key `cam7`. `go run ./cmd/mockm2lx -h` lists
every flag.

Check it is alive:

```powershell
curl.exe -s -X POST http://127.0.0.1:8080/api/local_auth/signin `
  -H "Content-Type: application/json" `
  -d '{\"alias\":\"wsl-comms-ro\",\"password\":\"changeme\"}'
```

The point of the mock is **fault injection** — it reproduces the four failure
modes the measurement work actually found. Full menu in
[`cmd/mockm2lx/README.md`](cmd/mockm2lx/README.md); the one worth doing first is
the re-accept refusal window, which is what makes `srtsink`'s own
`auto-reconnect` fail and why `internal/sender` owns the reconnect loop:

```powershell
curl.exe -X POST http://127.0.0.1:8080/control/drop-srt
```

### 3.2 The UI, in an ordinary browser

```powershell
cd frontend
npm run dev
```

Open the printed localhost URL. `window.go` does not exist in a plain browser
tab, so `frontend/src/ui/backend.js` serves every call from an in-memory fake:
three plausible input devices, a config seeded from `config.Defaults()`, and a
fake session that walks the SENDING lamp through CONNECTING → CONNECTED and the
switcher lamps through `starting` → `streaming`.

Drive it by hand from the devtools console:

```js
window.__wslcommsFake.emitSender('BACKOFF');
window.__wslcommsFake.emitError('something the operator should see');
window.__wslcommsFake.setDevices([]);          // the empty-dropdown case
```

This is the fastest loop for anything to do with lamps, layout or Settings. It
does not touch Go at all.

To rebuild the embedded bundle that `main.go` serves:

```powershell
cd frontend
npm ci
npm run build      # writes frontend/dist, which main.go go:embeds
```

`frontend/dist` is committed and current as of 2026-07-30, so you only need this
if you change the frontend.

### 3.3 The whole application

Not as a window: that needs the Wails CLI, MinGW gcc and GStreamer, and is Gate B
(§4, §6).

What you *can* run today is everything behind the window. `go test -tags dev .`
exercises the real `App` — the bound surface, the session lifecycle and the
shutdown ordering — against the `internal/gst` stub, and §5.1 drives the real
control plane against the real `cmd/mockm2lx` over a real socket, including its
fault injection. Between those two and the browser-hosted frontend of §3.2, the
only untested seam left at Gate A is GStreamer itself.

---

## 4. Do not run `go build .` or `go run .` — and why you cannot anyway

Wails guards against being built without its own build tags by popping a **modal
Windows dialog** that blocks until a human dismisses it, and whose OK button
opens a web browser. An earlier agent run produced a stream of them.

The tree is arranged so that this is structurally impossible:

```go
main.go, app.go    //go:build dev || production || bindings
main_nocgo.go      //go:build !(dev || production || bindings)
```

`dev`, `production` and `bindings` are the tags the Wails CLI sets. **Any other
build gets `main_nocgo.go`**, whose `main` prints one line to stderr and exits 1.
So a stray `go build .` produces an inert binary rather than a dialog.

`go test -tags dev .` is safe and is part of Gate A (§1). The tag decides which
`main` is compiled; it does not run one. Nothing in `app_test.go` calls
`wails.Run`, and nothing in it reaches a `wailsruntime` function that needs a
live runtime context — the only such caller is `eventPump.start`, and every test
that would reach it neutralises its `sync.Once` first.

**Do not "simplify" these constraints.** They used to read
`cgo && (dev || production || bindings)`, which cost roughly a thousand lines of
coverage; the `cgo &&` half was removed deliberately, and the hazard it used to
prevent as a side effect is now prevented on purpose:

```
CGO_ENABLED=0 go build -tags production .
```

used to produce a plausible-looking `wslcomms.exe` silently backed by the
`internal/gst` stub — one that installs, opens, enumerates invented Dante
devices, lights the SENDING lamp green and sends nothing at all. That
combination now fails at compile time in
[`internal/gst/gst_stub_guard.go`](internal/gst/gst_stub_guard.go), with an
error naming the problem. Verified on 2026-07-30 — and note the command, which
compiles the package rather than linking an executable, so nothing runnable is
produced even in the case where it succeeds:

```powershell
$env:CGO_ENABLED='0'; go build -tags production ./internal/gst
```

```
# wslcomms/internal/gst
internal\gst\gst_stub_guard.go:42:2: undefined: aProductionBuildMustNotUseTheGStreamerStub_setCGO_ENABLED_1
```

If you think the constraints are still wrong, raise it — they are the
coordinator's.

---

## 5. The two things that used to block integration, and how they were resolved

Both of these were open in the previous revision of this note. Both are closed.
The end-to-end walkthrough in §5.1 is the thing they were blocking, and it now
runs on this machine, at Gate A, with no cloud instance.

### 5.1 `wslcomms` can now talk to `cmd/mockm2lx`

`internal/m2lx` used to hardcode TLS — `url.URL{Scheme: "https"}` in `client.go`
and `Scheme: "wss"` in `watcher.go` — while `cmd/mockm2lx` serves plain HTTP with
no TLS anywhere in the package. The two could not meet, so no Gate A end-to-end
run of the real control plane was possible.

WP-2 has since added `internal/m2lx/host.go`. A host may now carry an explicit
scheme:

- a **bare** host (`m2lx.example.com`, `127.0.0.1:8080`) resolves to `https`/`wss`,
  exactly as before — this is the production path and the safe default;
- an explicit **`http://`** prefix opts into `http`/`ws`, and logs a loud one-line
  warning saying credentials will travel in clear;
- an explicit **`https://`** prefix behaves like a bare host;
- **anything else** — a typo, `ftp://` — resolves to `https`/`wss` with a warning,
  because guessing `http` for an unrecognised scheme would risk a plaintext
  password.

So `http://` is a deliberate, visible, dev-only downgrade whose one legitimate
use is the mock. **Never put it in a production `config.json`.**

#### The end-to-end walkthrough

This drives the real control plane — `App.startControlPlane`, the sign-in loop,
the `m2lx` client, the status `Watcher` and the event pump — against the real
mock, over a real socket. Everything below was executed on this machine on
2026-07-30; the output is verbatim.

**Terminal 1 — start the mock.** Non-default ports, so it cannot collide with
anything already on `:8080`:

```powershell
$env:CGO_ENABLED='0'
go run ./cmd/mockm2lx -addr 127.0.0.1:18081 -srt-addr 127.0.0.1:14001 -verbose
```

**Terminal 2 — point the app's control plane at it and run it.** The address
must carry the `http://` scheme; without it `internal/m2lx` correctly resolves
`https`/`wss` and the mock answers a TLS handshake with plain HTTP.

```powershell
$env:CGO_ENABLED='0'
$env:WSLCOMMS_MOCK_ADDR='http://127.0.0.1:18081'
go test -tags dev -run 'MockM2LX|Stale' -v -count=1 .
```

```
=== RUN   TestControlPlaneAgainstMockM2LX
m2lx: REST host "http://127.0.0.1:18081" selected via an explicit "http://" scheme — REST and the status socket will use http/ws, NOT https/wss. Credentials and the bearer token will travel in clear. This must never point at a production M2L-X instance; it exists only to reach cmd/mockm2lx.
m2lx: status socket host "http://127.0.0.1:18081" selected via an explicit "http://" scheme — ...
wslcomms: signed in to M2L-X as "wsl-comms-ro"
    app_test.go:1666: signed in to http://127.0.0.1:18081 as "wsl-comms-ro" and received {StreamState:stopped Video:{Raw: Codec: Width:0 Height:0 FrameRate:0} Audio:[] At:2026-07-30 22:03:51 +0100 BST Stale:false}
--- PASS: TestControlPlaneAgainstMockM2LX (1.01s)
=== RUN   TestStatusGoesStaleWhenTheMockStallsTheSocket
    app_test.go:1714: the stalled socket produced Stale=true after m2lx.StaleAfter (15s)
--- PASS: TestStatusGoesStaleWhenTheMockStallsTheSocket (18.84s)
PASS
ok      wslcomms        19.970s
```

Terminal 1 shows the same run from the other side, and one line of it is worth
knowing about before you go looking for a bug:

```
22:03:03 [statusws] upgrade refused from 127.0.0.1:50130 — invalid or missing access_token
22:03:03 [auth] signin: alias "wsl-comms-ro" ok, token expires in 23h59m59s
22:03:04 [statusws] client connected from 127.0.0.1:50133 (1 now connected)
```

`App.startControlPlane` launches the sign-in loop and the status `Watcher`
concurrently, so the Watcher's first upgrade attempt carries an empty token and
is refused. That is by design rather than a race to fix: `m2lx.Watcher` owns its
own reconnection — it must, because a token `Refresh` puts a new token in the
socket URL and forces a reopen mid-match — so the first attempt is simply the
first rung of that ladder, and it succeeds a second later. Expect one refused
upgrade in the log at every start.

Both tests `t.Skip` when `WSLCOMMS_MOCK_ADDR` is unset, so the Gate A suite never
depends on a subprocess it did not start. `WSLCOMMS_MOCK_ALIAS`,
`WSLCOMMS_MOCK_PASSWORD` and `WSLCOMMS_MOCK_STATUS_KEY` override the mock's
defaults (`wsl-comms-ro` / `changeme` / `cam7`).

What the second test does is the interesting half: it drives one of the mock's
fault injections all the way through to the event the frontend renders. Stalling
the status socket is the failure that looks like nothing — the WebSocket stays
open, so an implementation without a staleness check holds its last known values
green forever. `m2lx.StaleAfter` (15 s) catches it, `Status.Stale` carries the
verdict, and specification section 8 says the three WebSocket-derived lamps grey
out under STATUS UNAVAILABLE. The test asserts that verdict reaches the pump, and
that clearing the fault clears it.

**Terminal 3 — drive the faults by hand.** The test above does `stall-status`
programmatically; the rest are one `curl` each, and the app-side effect is
listed beside them. `curl.exe`, not PowerShell's `curl` alias:

```powershell
# What the mock is currently pretending: read every fault at once.
curl.exe -s http://127.0.0.1:18081/control/state

# The MP2/AC-3 silent-drop signature: video stays streaming, audio array empties.
# App side: Status.Audio is an empty slice and the AUDIO OK lamp must go red
# rather than panic on Audio[0].
curl.exe -s -X POST http://127.0.0.1:18081/control/drop-audio -d '{\"enabled\":true}'

# Lie about stream_state: claim streaming with nobody connected. This is the
# whole reason for the honest line under the lamps — stream_state is not proof.
curl.exe -s -X POST http://127.0.0.1:18081/control/lie -d '{\"streamState\":\"streaming\"}'
curl.exe -s -X POST http://127.0.0.1:18081/control/lie -d '{\"streamState\":\"\"}'

# Force a token refresh cycle without waiting RefreshFraction * 86399 s. The
# client refreshes and the Watcher must reopen its socket, because the token is
# in the URL.
curl.exe -s -X POST http://127.0.0.1:18081/control/expire-token -d '{\"in\":\"1s\"}'

# Put everything back.
curl.exe -s -X POST http://127.0.0.1:18081/control/reset
```

Each one answers with the state it just set, so you can see it took:

```
{"onePeerOnly":true,"refusalWindow":"6s","stallStatus":false,"dropAudio":false,"srt":{"connected":false},"wsClients":0,"sessions":3}
{"dropAudio":true}
{"lieStreamState":"streaming"}
{"tokensExpired":3}
{"status":"reset"}
```

**Not executed here, and honestly labelled:** the SRT half. `/control/drop-srt`
and the re-accept refusal window exercise the path from `internal/sender` through
`srtsink` to the mock's listener, and there is no `srtsink` at Gate A — the
`internal/gst` stub only pretends to connect and dials nothing. `internal/sender`
is fully tested against that stub, and the mock's listener is fully tested by its
own in-process `gosrt` dials, but **the two have never met over a real SRT
socket.** That meeting is Gate B, and it is the first thing to do once Gate B
opens. See `cmd/mockm2lx/README.md` for the worked example to run then.

### 5.2 `main.go` and `app.go` are inside the Gate A build

They used to be behind `cgo && (dev || production || bindings)`, and since
`CGO_ENABLED=0` never sets the `cgo` tag, `go build ./...` and `go test ./...`
skipped them entirely — roughly a thousand lines of wire-up outside the safety
net that covered every other package. The `cgo &&` clause has been removed (§4),
and `app_test.go` now lives at the repository root under the same constraint as
`app.go`.

It is **52 tests** (plus 35 subtests) covering the bound surface, the SRT
passphrase policy, the event pump under concurrent producers, the session
lifecycle, the shutdown ordering and races, the connection-failure reporting, and
the control plane against the mock. Two of them skip unless `WSLCOMMS_MOCK_ADDR`
is set (§5.1). Run them:

```powershell
$env:CGO_ENABLED='0'
go test -tags dev . -count=1
```

```
ok      wslcomms        0.215s
```

If you want proof the package compiles without producing anything anyone could
run, `go vet -tags dev .` type-checks it and links no executable. There is no
longer any need for the shadow-tree recipe that used to live here.

---

## 6. Opening Gate B

Gate B is what lets you compile cgo, build the real pipeline and produce an
actual `wslcomms.exe`. It is about 2 GB and half an hour, all unattended, all on
this machine.

1. **MinGW-w64 gcc** on `PATH`.
2. **GStreamer 1.28.5 mingw-x86_64 — the *development* installer**, not just the
   runtime.
3. **pkgconfiglite**, then
   `PKG_CONFIG_PATH=C:\gstreamer\1.0\mingw_x86_64\lib\pkgconfig`.
4. **The Wails CLI**, version-matched:
   ```powershell
   go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0
   ```

Then, and only then:

```powershell
cd frontend; npm ci; npm run build; cd ..
wails build -webview2 embed
```

`wails build` sets `CGO_ENABLED=1` and the `production` tag itself, which is what
brings `main.go`, `app.go` and `internal/gst/gst_cgo.go` into the build.

Two spikes are waiting at Gate B and should be run before anything else:

- **SP-2** — does `go-gst` v0.0.2 actually build under MinGW? One timeboxed day.
  If it fights, the agreed fallback is a ~200-line hand-written cgo shim behind
  the same `internal/gst` signatures. Do not spend a week on someone else's CI
  gap. See [`internal/gst/BUILD-NOTES.md`](internal/gst/BUILD-NOTES.md).
- **SP-3** — is the top-ranked H.264 encoder called `mfh264enc` on this machine,
  and does it emit a conformant IDR every 100 frames? Resolve the element **by
  rank at runtime**; do not hardcode the name.

Once Gate B is open, `go test -race` becomes available. Run it against
`internal/sender` and the root package first — those are the two with the most
concurrency and the least verification.

---

## 7. First-run configuration

`%APPDATA%\WSLComms\config.json`, created by the Settings screen. A missing file
is not an error: `config.Load` returns `config.Defaults()`. To point the app at
the mock:

```json
{
  "m2lxHost": "http://127.0.0.1:18081",
  "alias": "wsl-comms-ro",
  "eventId": "matcht",
  "srtHost": "127.0.0.1",
  "srtPort": 14001,
  "statusKey": "cam7",
  "audioDeviceId": "<IMMDevice endpoint GUID from the dropdown>"
}
```

The `http://` on `m2lxHost` is load-bearing and is the only thing that lets the
app reach the mock (§5.1). It is also a downgrade to cleartext that
`internal/m2lx` logs loudly every time it is used: it belongs in a development
`config.json` and nowhere else. A production instance is a bare host, which
resolves to `https`/`wss`.

The ports above match the mock invocation in §5.1. The SRT half of this cannot be
exercised until Gate B — `srtsink` dials the mock's listener directly, and there
is no `srtsink` without GStreamer.

The **M2L-X password** and the **SRT passphrase** never go in that file. They go
in Windows Credential Manager under `WSLComms/m2lx` and `WSLComms/srt`. Normally
the Settings screen writes them through `SetSecret`. There is deliberately no
getter — a secret goes in and does not come back out across the Wails boundary,
so the Settings screen can only ever show "set this session", never the value.

To seed them by hand before the GUI exists:

```powershell
cmdkey /generic:WSLComms/m2lx /user:wslcomms /pass:changeme
```

`internal/secrets` stores the value as a UTF-16LE `CredentialBlob`, which is what
Windows writes for a generic credential, so this should interoperate — but that
specific path is untested. `internal/secrets`'s own round-trip test, which does
hit the real vault, is the authority.

---

## 8. Where things are

| Path | Owner | What |
|---|---|---|
| `main.go`, `app.go`, `main_nocgo.go` | WP-8 | Wails bindings, wire-up, events, lifecycle |
| `internal/config`, `internal/secrets` | WP-1 | `config.json`, Credential Manager |
| `internal/m2lx` | WP-2 | sign-in, token refresh, status WebSocket |
| `internal/gst` | WP-3a | the only cgo surface, plus the stub twin |
| `internal/sender` | WP-3b | timestamp pinning, reconnect state machine, backoff |
| `internal/kvs` | WP-4 | M2L-X → Cognito credential chain |
| `frontend/src/monitor` | WP-5a | KVS viewer, mosaic crop, return audio |
| `frontend/src/ui`, `frontend/src/styles` | WP-5b | controls, lamps, Settings |
| `build/` | WP-6 | DLL allowlist, LGPL notices, installer |
| `cmd/mockm2lx` | WP-7 | mock M2L-X and fault injection |

Each package's doc comment is its real contract; the table above is only an
index. `docs/architecture.md` and `docs/test-results.md` are the measurement
record behind the numbers — when a constant looks arbitrary, it is usually
measured, and the comment beside it says which measurement.

---

## 9. Things that are known-unfinished, so you do not rediscover them

- **The SRT socket has never carried real media.** `internal/sender` is tested
  against the `internal/gst` stub, which dials nothing, and `cmd/mockm2lx`'s
  listener is tested by its own in-process `gosrt` dials. The two have never met
  (§5.1). Gate B, first job.
- **The GUI has never been built or opened.** Every command in this note stops at
  the Wails boundary on purpose (§4).
- **SP-1** — the two KVS endpoint shapes rest on a *single* captured sample from
  an instance that is now powered off. `internal/kvs` codes defensively and every
  error it returns names the exact field that was missing or the wrong shape.
  Expect to be surprised at Gate C.
- **SP-4 / SP-5** — the production `statusKey`, and whether the commentary input
  has an SRT passphrase set and at what `pbkeylen`, are both unmeasured. They are
  config, not code.
- **No race detector until Gate B.** The concurrency in `internal/sender` and in
  `app.go` has been read by hand and stressed with `go test -tags dev . -count=20`
  and `go test ./... -count=20`, which is not the same thing. `app.go` has four
  locks, an `atomic.Pointer` for the Wails context and five concurrent
  subsystems; run `-race` over the root package the hour Gate B opens.
- **The honest line is permanent.** *"Your feed is reaching the switcher. This
  does not confirm you are audible on the broadcast output."* There is no
  reliable in-app proof that commentary is on air. It is not a placeholder and it
  does not get removed when something better is found, because nothing better
  exists.
</content>
