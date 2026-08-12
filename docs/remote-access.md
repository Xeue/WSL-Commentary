# Remote access to the commentary contribution app

**Status: implemented, ON by default, bound to ALL interfaces (`0.0.0.0`), and UNAUTHENTICATED — by
the owner's explicit, repeated, final decision. Read this before deploying it anywhere that is not a
dedicated private facility network.** This document is the honest answer to "can a second person
drive the commentary app from another machine". The short version: yes, for CONTROL, over an open
LAN bridge that anyone who can reach the machine can use — and NO for the programme PICTURE, which is
a physical limitation, not a missing feature. If someone must SEE the SRT picture remotely, the
answer is console mirroring (Sunshine/Moonlight), covered at the end.

> **The single most important sentence in this file:** there is **no authentication of any kind** on
> this listener — no login, no client accounts, no password, no cookie, no origin/CSRF check. Anyone
> who can open a TCP connection to the commentary PC on the listener's ports has FULL control of the
> app, including writing to the live mixer (`SendMixerCommands`, after arming), setting passphrases
> (`SetSecret`), and reading live AWS session credentials (`GetKVSCredentials`). **The network is the
> only access control.** This is the owner's decision, made for a secure remote facility on a
> dedicated private network. It is recorded here, in a developer-facing document, and deliberately
> NOT surfaced as a warning in the app UI (the owner asked that the Settings section show only the
> current bound-port status).

---

## Recommendation, in one paragraph

Use the built-in **LAN control bridge** for a second operator or a producer who needs to watch the
lamps, adjust monitoring, run presets, or touch the mixer. It is a purpose-built bridge inside
`wslcomms` itself: a browser on the facility LAN loads the SAME frontend the local window runs,
talking to Go over a WebSocket instead of Wails. It is **on by default**, binds **all interfaces**,
serves **both plain HTTP and HTTPS**, and dispatches through a **hand-written allowlist** — a method
is reachable remotely only if it was deliberately classified so. **Deploy it only on a network you
trust completely**, because there is no login between that network and full control of a machine that
is on air. For anyone who must see the programme PICTURE at quality, pair it with console mirroring;
the bridge cannot carry that picture and never will.

### Why not the obvious alternatives

- **Wails' own dev server** (`-tags dev`, `devserver=0.0.0.0:34115`) is zero code but has no TLS
  option at all and an unauthenticated `GET /wails/reload` that reloads the operator's live window.
  This bridge is also unauthenticated, but it adds TLS (so the headphone picker and `setSinkId` work,
  and credentials do not cross the LAN in clear), a hand-written allowlist (so only classified methods
  are reachable), host-only refusals for the native surface, and mixer arm-ownership. It is a
  purpose-built bridge, not a debugging server pointed at the LAN.
- **Plain remote desktop (RDP)** carries the picture but actively breaks the app: an RDP session
  substitutes its own WASAPI endpoints (exactly the device list the app enumerates) and replaces the
  graphics stack the d3d11 decode path needs. It also does not give a second person a second seat.

---

## The operating rule: on, all interfaces, unauthenticated — the network is the control

The listener's configuration lives in its **own** file, `%APPDATA%\WSLComms\remote\remote.json` —
deliberately NOT `config.json`, because the Settings screen rewrites the whole config document from a
page cache and would silently drop a field it does not restate. A network listener's bind address
must not be reachable by that mechanism.

`remote.json` defaults, and the values a MISSING file yields, are:

```json
{ "enabled": true, "bind": "0.0.0.0", "httpPort": 80, "httpsPort": 443 }
```

So **doing nothing leaves the machine listening on every interface, for anyone who can reach it.**
The only structural rules `Settings.Validate` enforces are:

- The bind must be a **literal IP** (`0.0.0.0` is allowed and is the default), never a hostname —
  what is exposed cannot be allowed to change under DNS.
- Each port is `0..65535` (`0` means "let the OS assign one", used only by tests).

There is deliberately **no** "refuse a non-loopback bind" rule and **no** client concept: the owner's
decision is that the private network is the access control, so a wildcard bind is the intended
default, not a condition to refuse.

You can narrow or disable it from the local Settings screen (the `GetRemoteState` / `SetRemoteListener`
methods). Those two methods are **host-only**: a remote connection can never reconfigure or disable
the listener that is carrying it.

### Ports and the Windows fallback

The listener prefers the privileged well-known ports (80/443) so an operator can type a bare host
with no `:port`. On Windows those are frequently held by `http.sys` (WinRM, the print spooler, IIS),
so each has an automatic fallback: **80 → 8080**, **443 → 8443**. `Start` binds the primary if it
can and drops to the fallback if the primary is busy, logging which it used. If even the fallback is
unavailable the listener stays off for the run and the failure is reported on the error banner; it
never blocks startup.

---

## Trusting the certificate

TLS is still served (on the HTTPS port) even though there is no authentication, because it is
**functionally load-bearing**: over plain HTTP a LAN page is not a secure context, so
`navigator.mediaDevices` is undefined (the headphone dropdown is permanently empty) and `setSinkId`
is missing (return audio cannot be routed to a chosen device); and `GetKVSCredentials` returns live
AWS session credentials that must not cross a facility LAN in clear. So a remote browser should use
the `https://` URL, not the `http://` one.

The certificate is self-signed (ECDSA P-256, generated on first run into `%APPDATA%\WSLComms\remote\`),
so the remote browser shows a warning the first time. Because the bind is `0.0.0.0` and clients reach
the box by whatever LAN IP it answers on, the certificate's SANs cover **every non-loopback interface
IP** of the machine, plus loopback, `localhost` and the hostname; it is regenerated when the set of
interface IPs grows. If the facility has an internal CA, importing a real certificate is the better
answer than clicking through the self-signed warning.

Note the app UI does **not** show the certificate fingerprint. By the owner's decision the Settings
screen shows only the bound-port status (whether the listener is on, and the HTTP/HTTPS ports it bound
on) — `GetRemoteState` returns the SHA-256 `certFingerprint` for a developer or a future tool, but it
is deliberately not surfaced as UI prose. On a trusted facility network, accepting the self-signed
warning once is the intended flow; where a fingerprint check matters, pin it out of band or deploy a
CA-issued certificate.

There is **no login step**: navigate to the URL, accept the certificate, and the frontend loads and
connects. That is the whole handshake.

---

## What a remote connection can and cannot do

Every connection gets the SAME set of methods — there are no tiers. What a connection can do is bounded
by exactly two things:

1. **The host-only set.** The six native picture/return methods
   (`SetPictureRect`/`SetPictureVisible`/`StartPicture`/`StopPicture`/`StartReturn`/`StopReturn`) and
   the two remote-admin methods (`GetRemoteState`/`SetRemoteListener`) are refused for every remote
   connection and omitted from the method list the remote page ever sees. The picture ones are
   host-only because the picture is physics (below); the admin ones because a remote connection must
   not be able to reconfigure or disable the listener carrying it.
2. **Mixer arm-ownership.** `SendMixerCommands` requires not just that the caller reach the method but
   that the caller is the seat that most recently armed. With two controllers, one operator's arm must
   not silently authorise the other's write to the live clean feed — so a write from any seat other
   than the arming one is refused with the same `ErrDisarmed` sentinel as a write with no arm at all.
   This is about WHICH seat holds the open window, not authentication, so it is unaffected by the
   listener being open.

Every mutating remote call (`Start`, `Stop`, `SaveConfig`, `SetSecret`, `ArmMixer`,
`SendMixerCommands`, `SetMixerGolden`, the preset writes) is written to the log with the source
address; `SetSecret` logs the KEY it wrote, never the value. With no login, the source IP is the only
identity in the audit log.

---

## What degrades, and what cannot work at all — the honest table

A remote browser is a **second controller, not a viewer**, and it is NOT the local operator's exact
experience. Some of that is a secure-context limitation fixed by using the HTTPS URL; some of it is
physics.

| Capability | Remote behaviour | Why |
|---|---|---|
| **The SRT programme picture** | **Impossible.** The remote page shows the WebRTC multiviewer mosaic and an honest message; the high-quality SRT picture is never sent. | It is a native child window painted by `d3d11videosink` on the HOST GPU, outside the DOM (`internal/gst/overlay_windows.go`). No transport that carries the DOM can carry it. And `SetPictureRect` takes the CALLING page's CSS rect and DPI, so a remote browser at another size would drag the operator's own picture around. The six picture/return geometry methods are host-only and refused. |
| **The commentary INPUT device list** | Correct and useful. | `ListInputDevices` enumerates the HOST's WASAPI endpoints; picking host hardware from a remote seat is the intended behaviour. |
| **The headphone (output) picker** | Works only over the HTTPS URL. | `navigator.mediaDevices` needs a secure context; over the plain-HTTP URL the dropdown is empty. Use `https://`. |
| **Return audio** | Plays on the REMOTE machine's speakers/headphones, via `setSinkId`. | Useful for a remote producer, but it is NOT what the commentator is hearing. The SRT audio return to the HOST's headphones is host-only and refused. |
| **Monitoring level / gain sliders** | Apply to the REMOTE browser's own Web Audio graph — BUT `returnChannel`/`returnMid`/`returnGainDb` are SAVED and propagate to the local page via the `config` event and are re-applied to the commentator's ears. | The saved fields are shared config; a remote producer adjusting "their" monitoring can therefore change the commentator's. |
| **Config edits** | Two controllers editing Settings at once will still lose one set of edits; the `config` event shrinks the clobber window to one round trip but does not eliminate it. | Config is written as a whole object from each page's cache. A single-writer lease would close this, at the cost of materially more UI. |
| **Everything else** (lamps, presets, arm/disarm, mixer read, mixer write while holding the arm) | Works, for every connection. | These are ordinary bound calls; the shim installs exactly the methods the server's hello frame lists. |

---

## The picture: console mirroring is the only answer

If the requirement is genuinely "a remote person must SEE the programme picture at full quality",
the LAN bridge does not meet it and nothing that carries the DOM can. Use **console mirroring**:

- **Sunshine** (host, on the commentary PC) + **Moonlight** (client, on the remote machine). This
  streams the host's actual GPU output — including the native SRT picture window — as a low-latency
  video stream, without substituting the audio endpoints or graphics stack the app depends on.
- **Do NOT use plain RDP** for this. An RDP session replaces the WASAPI endpoints the app is
  enumerating and the graphics stack the d3d11 decode path needs, which breaks the running app.

Console mirroring shows ONE seat (the operator's screen, mirrored) — it is not a second controller.
Use it ALONGSIDE the LAN bridge when a remote person needs both to see the picture and to drive
controls: the bridge for control, mirroring for the picture.

---

## Threat model — stated plainly, because it is unusually blunt

Enabling this listener puts a machine that is on air under the full control of anyone who can reach it
on the network, with no credential in between. The bridge controls `SendMixerCommands` (a write path
to a live broadcast desk), `SetSecret` (passphrases), and `GetKVSCredentials` (live AWS session
credentials). **This is accepted, deliberately, on the basis that it runs on a dedicated private
facility network and the network is the access control.** That is the owner's decision; this document
records it rather than second-guesses it.

Given that decision, here is what protection remains and what it does — and does not — cover:

- **The network boundary is the whole of the access control.** If an untrusted host can reach the
  listener's ports, it has full control. Everything below assumes the network keeps that from
  happening; none of it is a substitute for that assumption.
- **TLS on the HTTPS port** keeps credentials and control frames off the wire in clear and makes the
  headphone picker / `setSinkId` work. The self-signed cert's fingerprint lets an operator confirm
  the box they reached is the box they meant to. TLS here is confidentiality and secure-context
  enablement, NOT authentication — anyone can complete the TLS handshake.
- **Host-only refusals.** The native picture/return surface and the two remote-admin methods are
  refused for every connection and omitted from the method list the remote page sees. In particular a
  remote connection cannot reconfigure or disable the listener carrying it.
- **Hand-written allowlist, not reflection.** Wails binds every exported method of the app object; the
  remote dispatcher exposes only methods classified in an explicit table, drift-guarded by a test that
  fails if any bound method is left unclassified. A new binding cannot default into being remotely
  reachable — but note that "reachable" now means "reachable by anyone on the network", so the
  allowlist is what bounds the blast radius, not who is inside it.
- **Mixer arm-ownership.** The write path to the live desk needs the caller to be the seat that armed,
  so two controllers cannot cross-authorise a write — but either of them can arm.
- **An audit log** of every mutating remote call, with the source address. With no login the source IP
  is the only identity recorded.

What is explicitly NOT here, by decision: no login, no client accounts, no password, no session
cookie, no per-IP lockout, no origin/CSRF check. Do not add any of them back without the owner's
instruction; the posture is intentional.

These mitigations are only as good as their tests; treat the test suite as the deliverable, not the
code. And nothing here removes the standing advice in `CONTRACT.md` rule 4: this is a live broadcast
machine, so validate any change against `cmd/mockm2lx` and a bench event before a match, never by
adding a seat during one.

---

## Known limits, stated plainly

- The listener is unauthenticated (above); it is safe ONLY on a trusted private network. On any other
  network it is a full remote-control hole into a machine that is on air.
- The SRT picture cannot be remoted, ever (above).
- Two config editors can still lose one set of edits (above); the `config` event only narrows the
  window.
- Two KVS viewers means two `RTCPeerConnection`s on the same signalling channel and two copies of the
  mosaic and return audio over the network; bandwidth, AWS cost and the channel's viewer limit have
  not been measured. Test on the mock and a bench event first.
- No match-length soak of a local window and a remote browser coexisting has been run. Schedule that
  with the operator; there is no way to prove the coexistence without running the real app, which
  rule 4 forbids doing on the live machine mid-match.
