# RUNNING — how to work on this today

**Gates A, B and C are all open, and the application is in use.** This machine
has Go 1.25.0, Node 24.6.0, MinGW gcc, GStreamer 1.28.5 (mingw-x86_64, devel)
and the Wails CLI. `wslcomms.exe` is built, in `build\bin\`, and has been run
on air against the live M2L-X instance
`m2lx-wslstudios-matcht.etapsiota.com`, event `dl9-5p5ah0bd-empd`.

That is a change from the previous revision of this note, which opened by
saying the toolchain was absent and the instance was powered off. Both were
true when it was written. Neither is true now.

Two things follow, and they are the two most important lines in this file:

- **Do not launch the GUI.** The operator uses it. Nothing here needs it —
  see §4 for why every command below stops at the Wails boundary on purpose.
- **Do not write to the live mixer.** Reading `switcher_status`, reading the
  REST API and dialling outputs are all fine. `set_routing` and its siblings
  change a clean feed that is on air.

Read [`CONTRACT.md`](CONTRACT.md) before editing anything. Authority for *what*
is built is [`docs/windows-app-spec.md`](docs/windows-app-spec.md) **v3**.
v2 is archived beside it and is materially wrong in several places; v3's §0
lists exactly where.

---

## 1. The one command that tells you the tree is healthy

From the repository root, in PowerShell, **after dot-sourcing the build
environment**:

```powershell
. .\build\env.ps1
go build -tags 'dev gststub' ./...
go vet -tags 'dev gststub' ./...
go test -race -tags 'dev gststub' ./... -count=1
gofmt -l .
```

All four must be silent (bar `go test`'s `ok` lines). `gofmt -l .` printing a
filename means that file needs formatting; it should print nothing.

**Both tags are load-bearing and both are easy to leave out.**

`dev` — `main.go`, `app.go`, `app_mixer.go`, `app_picture.go` and
`app_return.go` are behind `//go:build dev || production || bindings`, so a
plain `go test ./...` reports `? wslcomms [no test files]` and compiles neither
them nor their tests. `dev` is what brings the root package — the whole
lifecycle, the event pump, `Start`/`Stop`, the picture and return paths, the
mixer bindings and the shutdown ordering — into the run. Nothing about that tag
builds or runs a GUI: see §4.

`gststub` — `build\env.ps1` sets `CGO_ENABLED=1`, which selects
`internal/gst`'s real GStreamer implementation and excludes the pure-Go stub.
But the root package's tests and `internal/gst`'s own tests are written against
that stub, and rightly so: no unit test should be driving a live media
pipeline. Without the tag you get

```
.\app_test.go:919:30: undefined: gst.StubPipeline
```

and — this is the part that matters — **it does not fail loudly for the whole
run.** The root package fails to build while every other package reports `ok`,
so a hurried reader sees mostly green and moves on, and the tests over the
lifecycle, the picture path and the shutdown ordering are never executed.
`build\env.ps1` prints the correct command when you dot-source it.

`go test -race` works now that cgo is available, and should be used: the root
package has four locks, an `atomic.Pointer` for the Wails context and five
concurrent subsystems, and `internal/sender` and the two `internal/gst`
monitors are all reconnect state machines.

Last run, 2026-08-07, whole tree, `go test -tags 'dev gststub' ./... -count=1`:

```
ok      wslcomms                        4.554s
ok      wslcomms/cmd/mockm2lx           1.461s
ok      wslcomms/internal/config        0.472s
ok      wslcomms/internal/gst           2.525s
ok      wslcomms/internal/kvs           0.935s
ok      wslcomms/internal/m2lx          4.786s
ok      wslcomms/internal/mixer         1.461s
ok      wslcomms/internal/secrets       0.465s
ok      wslcomms/internal/sender        1.090s
```

If a package is red when you pull, check whether the failure is in a file you
own before you spend an afternoon on it: this tree is worked on by several
packages at once, and a compile error in someone else's test file is theirs.

**Gate A still works and is still worth keeping.** On a machine with Go and
Node and nothing else, everything except `internal/gst`'s cgo half still builds
and tests:

```powershell
$env:CGO_ENABLED='0'
go build ./...; go vet ./...; go test ./... -count=1; go test -tags dev . -count=1
```

That property is not a historical artefact — it is what lets anyone pick this
up without a 2 GB toolchain install, and it is enforced by the guard in §4. Do
not break it.

The frontend has its own tests, which use Node's built-in runner because
`package.json` is frozen and has no test framework in it:

```powershell
cd frontend
node --test "src/monitor/*.test.js" "src/ui/*.test.js" "src/ui/mixer/*.test.js"
```

**693 tests, all passing, 2026-08-12.** The three globs are the KVS monitor
(WP-5a), the shell (WP-5b) and the mixer drawer (WP-M4); running them
separately is fine and is what the per-package sections below assume.

`src/ui/` covers the pure modules the shell cannot get subtly wrong without
somebody noticing on air: `tile.js`, which rescales `config.monitorTile` from
the mosaic it was measured against onto the one that actually arrived;
`liveurl.js`, which parses the pasted live-operation URL into a host and an
event ID; `lamps.js`, including `deriveHonestLine`, which has no caller in this
build and is tested anyway; and `picturesource.js`, whose tests read the source
file's own text to assert that the picture control says nothing about audio —
because the last time a control on that screen touched two things at once,
selecting it silenced the operator. `liveurl.js`'s `bareHost` is a mirror of
`internal/config`'s `hostOnly` and is tested against the same cases, because if
they disagree the Settings screen's "same as M2L-X" placeholder is telling the
operator something untrue.

Use the quoted glob. `node --test src/monitor/` — the directory form — fails on
this Node version.

### Repetition still finds things `-race` does not

`-race` works now. It is not a substitute for running the concurrent packages
many times, because a state machine whose transitions are all correctly locked
can still deadlock or leak a goroutine:

```powershell
go test -race -tags 'dev gststub' ./... -count=5
```

---

## 2. What works today

| Thing | State |
|---|---|
| `internal/config`, `internal/secrets` | Working and tested. The Credential Manager tests hit the **real** vault, backing up and restoring anything already stored. |
| `internal/m2lx` | Working. Tested against its own fakes, against `cmd/mockm2lx` over a real socket (§5.1), and against captured live frames in `testdata/`. The socket is snapshot-then-delta; see spec v3 §8.2 before touching the parser. |
| `internal/sender` | Working and tested in full against a fake pipeline. This is where spec §6 lives. |
| `internal/gst` | **Working for real.** Three paths: the contribution pipeline (`gst.go`), the SRT picture (`picture.go`), the SRT audio return (`return.go`), plus the native overlay window. Each has a cgo half and a stub twin; the stub is what the unit tests drive. |
| `internal/kvs` | Working, and verified against the live endpoints. |
| `internal/mixer` | Working. Read path parses a live `switcher_status` frame; write path is arm-gated. **Do not point its write path at the live instance.** |
| `cmd/mockm2lx` | Working. REST, status WebSocket, a real SRT listener, and fault injection. |
| Frontend | 693 tests. Also runs in a browser against an in-memory fake backend (§3.2). |
| `main.go`, `app.go`, `app_picture.go`, `app_return.go`, `app_mixer.go` | Working and covered, and exercised on air. |
| `wslcomms.exe` | **Built**, in `build\bin\`, and in use by the operator. Do not launch it (§4). |

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

`frontend/dist` is committed, so you only need this if you change the frontend.

The mixer drawer has its own harness that does not need the app or the backend:
open `frontend/src/ui/mixer/demo.html`, which drives `drawer.js` from
`demo-fixture.js` — a captured live `switcher_status` frame. `frontend/src/monitor/harness.html`
does the same for the KVS monitor.

### 3.3 The whole application

**Not as a window. The operator is using it.** Building it is
`wails build -webview2 embed` (§6) and that is a build, not a launch; opening
`build\bin\wslcomms.exe` takes the SRT input's one peer slot away from the
position that is on air.

What you *can* run is everything behind the window.
`go test -tags 'dev gststub' .` exercises the real `App` — the bound surface,
the session lifecycle, the picture and return paths, the mixer bindings and the
shutdown ordering — against the `internal/gst` stubs, and §5.1 drives the real
control plane against the real `cmd/mockm2lx` over a real socket, including its
fault injection.

**Reading the live instance is fine, and is how most of spec v3 §5.2 and §8.2
were measured.** Sign in with `POST /api/local_auth/signin`, body
`{"alias":"…","password":"…"}` — the field is `alias`; `username` returns 500 —
then read the REST API, watch `switcher_status`, or dial an output with
`gst-launch-1.0` (`build\env.ps1` puts it on `PATH`). Dialling M2L-X **Output
1** on 40501 while the operator's app is running will fail: an SRT listener
accepts one peer and never displaces the incumbent. Use a different output, or
do it between matches.

---

## 4. Do not run `go build .` or `go run .` — and why you cannot anyway

Wails guards against being built without its own build tags by popping a **modal
Windows dialog** that blocks until a human dismisses it, and whose OK button
opens a web browser. An earlier agent run produced a stream of them.

The tree is arranged so that this is structurally impossible:

```go
main.go, app.go, app_mixer.go,      //go:build dev || production || bindings
app_picture.go, app_return.go
main_nocgo.go                       //go:build !(dev || production || bindings)
```

`dev`, `production` and `bindings` are the tags the Wails CLI sets. **Any other
build gets `main_nocgo.go`**, whose `main` prints one line to stderr and exits 1.
So a stray `go build .` produces an inert binary rather than a dialog.

`go test -tags 'dev gststub' .` is safe and is the command in §1. The tag
decides which `main` is compiled; it does not run one. Nothing in `app_test.go`
calls `wails.Run`, and nothing in it reaches a `wailsruntime` function that
needs a live runtime context — the only such caller is `eventPump.start`, and
every test that would reach it neutralises its `sync.Once` first.

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

Both of these were open two revisions ago. Both are closed. The end-to-end
walkthrough in §5.1 is the thing they were blocking, and it runs on this
machine with no cloud instance at all — which is why it is still here now that
there is one. It is the only way to exercise the fault injection.

The commands in this section are the Gate A ones (`CGO_ENABLED=0`,
`-tags dev`), because the mock needs neither cgo nor GStreamer. If you have
dot-sourced `build\env.ps1` in the same shell, add `gststub` to the tag list or
open a fresh shell.

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

# Lie about stream_state: claim streaming with nobody connected. This is why
# stream_state is not proof of anything, and was the whole reason for the
# honest line the operator has since had withdrawn (spec v3 §10). The fault is
# still real; only the sentence about it is gone.
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

**The SRT half of the mock has still never met a real `srtsink`.**
`/control/drop-srt` and the re-accept refusal window exercise the path from
`internal/sender` through `srtsink` to the mock's listener. `internal/sender` is
fully tested against the `internal/gst` stub, which dials nothing, and the
mock's listener is fully tested by its own in-process `gosrt` dials — but the
two have never been joined, even now that `srtsink` exists. The real send path
has instead been proven against the **live instance**, which is a better
test of the same thing but a worse one for fault injection: you cannot ask
M2L-X to hold its listener socket open for six seconds on demand. See
`cmd/mockm2lx/README.md` for the worked example, which is still worth running.

### 5.2 `main.go` and `app.go` are inside the ordinary build

They used to be behind `cgo && (dev || production || bindings)`, and since
`CGO_ENABLED=0` never sets the `cgo` tag, `go build ./...` and `go test ./...`
skipped them entirely — roughly a thousand lines of wire-up outside the safety
net that covered every other package. The `cgo &&` clause has been removed (§4),
and the root tests live at the repository root under the same constraint as
`app.go`.

They cover the bound surface, the SRT passphrase policy, the event pump under
concurrent producers, the session lifecycle, the shutdown ordering and races,
the connection-failure reporting, the picture overlay and its lock order, the
return path, the mixer bindings, and the control plane against the mock. Two
skip unless `WSLCOMMS_MOCK_ADDR` is set (§5.1). Run them:

```powershell
. .\build\env.ps1
go test -race -tags 'dev gststub' . -count=1
```

`go vet -tags 'dev gststub' .` type-checks the package and links no executable,
if you want proof it compiles without producing anything anyone could run.

---

## 6. The toolchain, and rebuilding the executable

All of this is installed on this machine already. It is written down because it
is what a fresh machine needs, and because `build\env.ps1` assumes the paths.

1. **MinGW-w64 gcc** — `C:\msys64\mingw64\bin`.
2. **GStreamer 1.28.5 mingw-x86_64 — the *development* installer**, not just the
   runtime — `C:\gstreamer\1.0\mingw_x86_64`.
3. **pkgconfiglite**, then
   `PKG_CONFIG_PATH=C:\gstreamer\1.0\mingw_x86_64\lib\pkgconfig`.
4. **The Wails CLI**, version-matched:
   ```powershell
   go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0
   ```

`build\env.ps1` sets `PATH`, `PKG_CONFIG_PATH` and `CGO_ENABLED=1`, and puts
the real MinGW runtime ahead of GStreamer's `lib\`, which ships its own
`libmingw32.a`/`libmingwex.a` and otherwise breaks the link. Dot-source it; do
not copy its contents into your shell profile.

Then, and only then:

```powershell
cd frontend; npm ci; npm run build; cd ..
wails build -webview2 embed
```

`wails build` sets `CGO_ENABLED=1` and the `production` tag itself, which is
what brings `main.go`, `app.go`, the four `app_*.go` files and
`internal/gst`'s cgo halves into the build. `build\bundle-gst.ps1` then copies
the DLL allowlist into `dist\gst\` from an explicit file list and verifies the
result against the expected set, so a plugin silently gained or lost is a build
failure rather than a runtime one on the installed machine.

**The two spikes that used to be listed here are both resolved**, and the
answers are in the spec rather than here:

- **SP-2** — `go-gst` v0.0.2 **does** build under MinGW. The ~200-line cgo shim
  fallback was not needed. See
  [`internal/gst/BUILD-NOTES.md`](internal/gst/BUILD-NOTES.md), which is the
  long-form record of that work.
- **SP-3** — the top-ranked H.264 encoder is **not** `mfh264enc`; on this
  machine it is `nvh264enc` at rank 257. The encoder is now chosen **by
  preference, not by rank**, and the whole argument is spec v3 §5.1. Rank is
  used only to exclude unusable factories and to tie-break.

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

The ports above match the mock invocation in §5.1.

A production `config.json` also carries the picture and return fields — the full
list with defaults is spec v3 §9. The two worth knowing here are
`returnSource`, which must be `"webrtc"` for the picture to start (they dial the
same M2L-X output and it accepts one peer), and `pictureSource`, which chooses
between the native SRT picture and the KVS mosaic fallback.

The **M2L-X password** and the **two SRT passphrases** never go in that file.
They go in Windows Credential Manager:

| Target | What it is |
|---|---|
| `WSLComms/m2lx` | the M2L-X sign-in password |
| `WSLComms/srt` | the SRT passphrase for the **send** path — the commentary input this app dials |
| `WSLComms/srtreturn` | the SRT passphrase for the **inbound** path — the M2L-X output the picture, and the SRT audio return, dial |

The two SRT passphrases are separate credentials because **M2L-X sets encryption
per output**, measured on the live instance:

```
Output 1  src=pgm  port 40501  encrypted=false
Output 2  src=pvw  port 40502  encrypted=true
Output 3  src=cln  port 40503  encrypted=true
```

so the endpoint the feed goes to and the endpoint the monitor comes from
routinely need different keys. Sharing one meant that entering the key which
made the return work changed the key the feed went out with. The key *lengths*
are separate too, and they are not secrets, so they live in `config.json` as
`pbkeylen` (send) and `srtReturnPBKeyLen` (return); both are `0`, `16` or `32`,
with `0` meaning no encryption is negotiated.

One consequence of "per output" that is easy to get wrong: the **picture** reads
`WSLComms/srtreturn` and `srtReturnPBKeyLen`, not the send path's. That is
correct — it is the same M2L-X *output* as the SRT audio return — but the field
names now describe two different features, and it is reported as a gap in spec
v3 §9.1 rather than quietly lived with.

Normally the Settings screen writes all three through `SetSecret`. There is
deliberately no getter — a secret goes in and does not come back out across the
Wails boundary, so the Settings screen can only ever show "set this session",
never the value.

To seed them by hand, or to set them without opening the GUI:

```powershell
cmdkey /generic:WSLComms/m2lx      /user:wslcomms /pass:changeme
cmdkey /generic:WSLComms/srt       /user:wslcomms /pass:send-path-key
cmdkey /generic:WSLComms/srtreturn /user:wslcomms /pass:return-path-key
```

`internal/secrets` stores the value as a UTF-16LE `CredentialBlob`, which is what
Windows writes for a generic credential, so this should interoperate — but that
specific path is untested. `internal/secrets`'s own round-trip test, which does
hit the real vault, is the authority.

---

## 8. Where things are

| Path | Owner | What |
|---|---|---|
| `main.go`, `app.go`, `main_nocgo.go`, `exit_windows.go` | WP-8 | Wails bindings, wire-up, events, lifecycle, the hard-exit path |
| `app_picture.go` | WP-P | the SRT picture's bound surface, the native overlay, the `picture` event |
| `app_return.go` | WP-R | the SRT audio return's bound surface and the `return` event |
| `app_mixer.go` | WP-8 | the mixer drawer's bound surface: snapshot, arm, send, golden |
| `internal/config`, `internal/secrets` | WP-1 | `config.json`, Credential Manager |
| `internal/m2lx` | WP-2 | sign-in, token refresh, status WebSocket, the snapshot/delta document |
| `internal/gst` | WP-3a / WP-P / WP-R | the only cgo surface: send pipeline, picture pipeline, return pipeline, overlay window — each with a stub twin |
| `internal/sender` | WP-3b | timestamp pinning, reconnect state machine, backoff |
| `internal/kvs` | WP-4 | M2L-X → Cognito credential chain |
| `internal/mixer` | WP-M0…M3 | bus model, `switcher_status` parse, golden/compare, `switcher_controller` client |
| `frontend/src/monitor` | WP-5a | KVS viewer, mosaic crop, bus and channel selection, return audio |
| `frontend/src/ui`, `frontend/src/styles` | WP-5b | controls, lamps, Settings, picture source |
| `frontend/src/ui/mixer` | WP-M4 | the drawer: contract, model, DOM, demo harness |
| `build/` | WP-6 | `env.ps1`, DLL allowlist, LGPL notices, Inno Setup installer |
| `cmd/mockm2lx` | WP-7 | mock M2L-X and fault injection |

Each package's doc comment is its real contract; the table above is only an
index, and the doc comments in this tree are unusually long on purpose — they
carry the measurement that justifies each constant. When a number looks
arbitrary it is usually measured, and the comment beside it says which
measurement.

`docs/architecture.md` and `docs/test-results.md` are the **2026-07-29/30
measurement record**. They are historical and were written before the picture
path, the mixer drawer and the snapshot/delta discovery existed; where they
disagree with `docs/windows-app-spec.md` v3, v3 is later and was measured
against the same instance. They are still the authority for the things only they
recorded — the tone-injection bus map, the KVS bitrates, the bus summing
measurement.

---

## 9. Things that are known-unfinished, so you do not rediscover them

Ordered by how much it would cost to be wrong about them. The full list with
the tests that would settle each is spec v3 §14; this is the working summary.

- **No match-length soak has been run.** The application has been used on air;
  it has not been left running for the length of a match, unattended, with the
  log kept. The two bugs that will actually hurt — the backwards-DTS jump and
  the reconnect window — only appear over hours, and everything defending
  against them is defended by construction and by unit tests rather than by
  having survived one.
- **Nobody knows whether a `stream_state` CHANGE is ever pushed as a delta**, or
  whether it only ever appears in the connect snapshot. Over 150 s and 3180
  observed frames no input changed state, so the question was never put, and
  putting it means changing an input's state on a live switcher.
  `internal/m2lx`'s `resyncInterval` is an explicit backstop against exactly
  this assumption. The same answer decides whether the mixer drawer can stop
  polling one dial per refresh.
- **The negotiated SRT latency has never been read**, on any of the three paths.
  `srtsrc`'s `stats` property is not reachable from `gst-launch` and nothing at
  `GST_DEBUG=srtobject:7` prints it, so what spec v3 §5.2 has is end-to-end
  time-to-first-frame at three settings, which is evidence and not the number.
- **Absolute glass-to-glass picture latency is unmeasured and currently
  unmeasurable** — it needs a reference clock at the source. What is measured is
  1.2 ms of transit from `srtsrc`'s src pad to the sink, which is the part this
  application controls and is not the whole path. The operator's verdict that
  the picture is now good is a real data point and is not a measurement.
- **A wrong or missing INBOUND passphrase cannot be named precisely.** libsrt
  distinguishes `ERROR:BADSECRET` from `ERROR:UNSECURE` and `internal/gst` logs
  whichever it got, but neither `gst.ReturnOpts` nor `gst.PictureOpts` has an
  `OnConnectError` callback the way `sender.Opts` does, so the reason reaches
  the log and stops there. After three consecutive failures the app emits one
  message naming the endpoint and the encryption it offered, and points at the
  log. It is reported, not worked around.
- **The mixer write path has never been exercised against the live instance from
  a test.** It has been used once, by the operator, through the GUI, to take
  commentary out of the clean feed. Responses are not correlated to commands and
  cannot honestly be — see spec v3 §11 — so confirmation is always "read the
  next snapshot back".
- **The mock's SRT listener and a real `srtsink` have still never met** (§5.1).
- **`internal/secrets` writing a UTF-16LE `CredentialBlob` interoperates with
  `cmdkey` in theory only.** The round-trip test through the package's own API
  hits the real vault and is the authority; the `cmdkey` path is untested.
- **The honest line is withdrawn from the GUI, at the operator's instruction.**
  `deriveHonestLine` and its wording are kept and tested in
  `frontend/src/ui/lamps.js` with no caller, exactly as `internal/mixer`'s
  golden/compare machinery was kept when the drift panel was withdrawn. Putting
  it back is a change to `home.js` alone. The fact underneath it has not
  changed and is not going to: **there is no reliable in-app proof that
  commentary is on air.** Do not build a lamp that claims otherwise.
