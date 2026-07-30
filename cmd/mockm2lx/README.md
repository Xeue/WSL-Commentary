# mockm2lx

A stand-in for the M2L-X instance, so the control plane, the reconnect state
machine and the whole `wslcomms` application can be exercised with no cloud
instance and no libsrt. Owner: WP-7 (see `CONTRACT.md`).

It serves the four things `wslcomms.exe` talks to:

1. `POST /api/local_auth/signin` / `POST /api/local_auth/refresh_token`
2. `GET /api/live_operation/kvs/webrtc_info/{event}` /
   `webrtc_token/{event}`
3. `GET /api/v1/switcher_status?access_token=...` — the push-only status
   WebSocket
4. an SRT listener with the real listener's one-peer, re-accept-refusal
   semantics

...and one thing the real instance doesn't: a `/control` HTTP API for
driving faults mid-test. **That's the point of this program.** A mock that
only works when everything works is not worth having; see "Fault injection"
below.

## Build and run

```
CGO_ENABLED=0 go build -o mockm2lx.exe ./cmd/mockm2lx
.\mockm2lx.exe
```

or straight from source:

```
go run ./cmd/mockm2lx
```

Default addresses: `:8080` for HTTP/WS, `:4001` for SRT. Default sign-in
credentials are `alias=wsl-comms-ro`, `password=changeme` — override with
`-alias`/`-password`. Run `mockm2lx -h` for the full flag list; every fault
flag documented below has a startup flag AND a `/control` endpoint, and the
flag only sets where the fault starts.

Point `wslcomms`'s `config.json` (`%APPDATA%\WSLComms\config.json`, spec
section 9) at it:

```json
{
  "m2lxHost": "127.0.0.1:8080",
  "alias": "wsl-comms-ro",
  "srtHost": "127.0.0.1",
  "srtPort": 4001
}
```

(The M2L-X password goes in Windows Credential Manager, not the config
file — set it to `changeme`, or whatever `-password` you started the mock
with, under `WSLComms/m2lx`.)

## Endpoints

| Method | Path | Notes |
|---|---|---|
| POST | `/api/local_auth/signin` | `{"alias","password"}` → `{access_token, refresh_token, expires_in, id, roleIds}`. `"username"` instead of `"alias"` → HTTP 500, matching the real instance. |
| POST | `/api/local_auth/refresh_token` | `{"refresh_token"}` → same shape, rotated. |
| GET | `/api/live_operation/kvs/webrtc_info/{event}` | Bearer auth. Returns the one measured shape (`docs/test-results.md` line 141). |
| GET | `/api/live_operation/kvs/webrtc_token/{event}` | Bearer auth. Fabricated but correctly-shaped `identity_id`/`token`. |
| GET | `/api/v1/switcher_status?access_token=...` | WebSocket upgrade. Missing/invalid token → HTTP 401 `Token rejected`. Push-only; never reads client data frames. |
| SRT listener | `srt://<host>:<srt-port>` | Caller mode, one peer, spec section 5's shapes expected on the wire. |

## Fault injection

Every fault is process-wide (there is one M2L-X instance in real life, so
there is one fault state here) and settable two ways: a startup flag, and a
`/control` call for changing it mid-run.

| Fault | Startup flag | Control call |
|---|---|---|
| Drop the SRT session now | — (it's an action, not a state) | `POST /control/drop-srt` |
| One-peer-only (refuse a 2nd caller, never displace the 1st) | `-one-peer-only` (default `true`) | `POST /control/one-peer-only {"enabled":bool}` |
| Re-accept refusal window after a disconnect | `-refusal-window` (default `6s`) | `POST /control/refusal-window {"seconds":number}` |
| Stall the status WebSocket (stop pushing, keep sockets open) | `-stall-status` | `POST /control/stall-status {"enabled":bool}` |
| Lie about `stream_state` | `-lie-stream-state` (`streaming`/`starting`/`stopped`) | `POST /control/lie {"streamState":"..."}` (`""`/`"auto"` clears it) |
| Drop the audio array (MP2/AC-3 silent-drop signature) | `-drop-audio` | `POST /control/drop-audio {"enabled":bool}` |
| Make the token expire early | `-token-ttl` (applies to every future sign-in) | `POST /control/expire-token {"in":"2s"}` (empty/omitted `in` = immediately) |
| Read everything above at once | — | `GET /control/state` |
| Return every fault to its startup value | — | `POST /control/reset` |

`refusal-window` defaults to **6 s**, deliberately a little above the
measured ~5 s window (`docs/test-results.md` line 149,
`docs/architecture.md` line 377): `internal/sender`'s first backoff rung is
7 s specifically to clear this with margin, so the mock's default should be
the harder case that rung has to survive, not the easiest one.

## Worked example: reproducing the re-accept refusal window

This is the specific behaviour that makes `srtsink`'s own `auto-reconnect`
fail (spec section 6.2) and that `internal/sender`'s backoff ladder exists to
survive. Here's how to see it happen, and how to see a naive reconnect fail
against it.

**Terminal 1 — start the mock**, with a short window so the demo doesn't
take all day:

```
go run ./cmd/mockm2lx -refusal-window=5s -status-interval=2s
```

**Terminal 2 — connect a caller.** Anything that dials
`srt://127.0.0.1:4001?mode=caller` works: `wslcomms.exe` pointed at the mock
(see above), or `gst-launch-1.0` standing in for it:

```
gst-launch-1.0 videotestsrc ! video/x-raw,width=1920,height=1080,framerate=50/1 ^
  ! x264enc ! mpegtsmux ! srtsink uri="srt://127.0.0.1:4001?mode=caller" auto-reconnect=false
```

Terminal 1 logs:

```
[srt] ACCEPT 127.0.0.1:xxxxx (stream id "") — caller connected
```

**Terminal 3 — drop the session:**

```
curl -X POST http://127.0.0.1:8080/control/drop-srt
```

Terminal 1 logs the disconnect and the start of the refusal window:

```
[srt] drop-srt: forcing disconnect of 127.0.0.1:xxxxx
[srt] DISCONNECT 127.0.0.1:xxxxx — refusing re-accept for 5s
```

**Now try to reconnect immediately** — either let `gst-launch-1.0`'s (or
`wslcomms`'s) own reconnect fire right away, or dial again by hand in
Terminal 2. Inside the window, the mock refuses it and logs why:

```
[srt] REFUSE 127.0.0.1:xxxxx — re-accept refusal window active, 4.2s remaining
```

This is exactly the shape of the real failure: `srtsink`'s built-in
`auto-reconnect` closes the socket and reopens it *immediately* with no
backoff (spec section 6.2, reading `gstsrtobject.c`), which lands squarely
inside this window and hard-fails the pipeline — while the property name
makes it look like reconnection is handled. It isn't. That's why
`auto-reconnect=false` is mandatory and `internal/sender` owns the loop
itself.

**Wait out the window** (5 s in this example) and reconnect again — the mock
accepts it:

```
[srt] ACCEPT 127.0.0.1:xxxxx (stream id "") — caller connected
```

If you're testing `internal/sender` specifically: its first backoff rung is
7 s (`BackoffLadder[0]`), comfortably clearing this window's default 6 s (or
this demo's 5 s), so a correct implementation never logs a `REFUSE` line at
all — it simply reconnects on the first attempt after backoff. If you *do*
see a `REFUSE` line while testing `internal/sender`, either its backoff has
regressed below the window, or something reconnected without going through
it.

## Other fault demos, briefly

```
# Reproduce the MP2/AC-3 silent-drop signature: video stays "streaming",
# audio array goes empty.
curl -X POST http://127.0.0.1:8080/control/drop-audio -d '{"enabled":true}'

# Lie: claim "streaming" with nobody connected, or "stopped" with a live
# peer — the whole point of the honest line under the lamps (spec section
# 8) is that stream_state alone is not proof.
curl -X POST http://127.0.0.1:8080/control/lie -d '{"streamState":"streaming"}'
curl -X POST http://127.0.0.1:8080/control/lie -d '{"streamState":""}'   # back to the truth

# Stall the status socket: connections stay open, nothing is pushed. Watch
# the app's three WebSocket-derived lamps grey out and "STATUS UNAVAILABLE"
# appear after m2lx.StaleAfter (15s).
curl -X POST http://127.0.0.1:8080/control/stall-status -d '{"enabled":true}'
curl -X POST http://127.0.0.1:8080/control/stall-status -d '{"enabled":false}'

# Force a token refresh cycle without waiting ~12 hours for RefreshFraction:
curl -X POST http://127.0.0.1:8080/control/expire-token -d '{"in":"1s"}'
```

## SRT passphrase / pbkeylen

`-srt-passphrase` and `-srt-pbkeylen` reproduce spec open question 6: connect
with and without a passphrase and watch the mock distinguish
`ERROR:UNSECURE` (a passphrase was expected on one side and not offered on
the other) from `ERROR:BADSECRET` (offered, but wrong), exactly as libsrt
does. `-srt-pbkeylen` is validated but not enforced against the wire — see
the comment beside the passphrase check in `srt.go` for why (gosrt's
`ConnRequest` doesn't expose the caller's negotiated key length).

## Tests

```
CGO_ENABLED=0 go test ./cmd/mockm2lx/... -v
```

The SRT tests dial real loopback SRT connections with
`github.com/datarhei/gosrt`'s own `Dial`, in-process — no external tool, no
subprocess, no libsrt. The MPEG-TS/PES tests build synthetic streams with a
small hand-rolled generator (`mpegts_test.go`) to exercise the DTS-regression
detector — the automated check for the measured bug that broke a
match-length run while every indicator read green (spec section 6.1).
