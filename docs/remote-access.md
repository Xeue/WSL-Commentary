# Remote access to the commentary contribution app

**Status: implemented, OFF by default, bound to loopback by default. Read this before enabling it
at a venue.** This document is the honest answer to "can a second person drive the commentary app
from another machine". The short version: yes, for CONTROL, over an authenticated TLS bridge that is
off until you turn it on — and NO for the programme PICTURE, which is a physical limitation, not a
missing feature. If someone must SEE the SRT picture remotely, the answer is console mirroring
(Sunshine/Moonlight), covered at the end.

---

## Recommendation, in one paragraph

Use the built-in **LAN control bridge** for a second operator or a producer who needs to watch the
lamps, adjust monitoring, run presets, or (with the right capability) touch the mixer. It is a
purpose-built, authenticated bridge inside `wslcomms` itself: a browser on the facility LAN loads
the SAME frontend the local window runs, talking to Go over an authenticated WebSocket instead of
Wails. It is **off by default**, binds **127.0.0.1** by default, is **TLS-only**, and dispatches
through a **hand-written allowlist** — a method is reachable remotely only if it was deliberately
classified so. For anyone who must see the programme PICTURE at quality, pair it with console
mirroring; the bridge cannot carry that picture and never will.

### Why not the obvious alternatives

- **Wails' own dev server** (`-tags dev`, `devserver=0.0.0.0:34115`) is zero code but has
  `CheckOrigin` hardwired to `return true`, no auth, no TLS, and an unauthenticated
  `GET /wails/reload` that reloads the operator's live window. Anything on the LAN that reaches the
  port gets `SendMixerCommands` against a live desk, `SetSecret`, and `GetKVSCredentials` (live AWS
  session credentials). Unacceptable on a machine that is on air.
- **Plain remote desktop (RDP)** carries the picture but actively breaks the app: an RDP session
  substitutes its own WASAPI endpoints (exactly the device list the app enumerates) and replaces the
  graphics stack the d3d11 decode path needs. It also does not give a second person a second seat.

---

## The operating rule: off, and loopback, by default

The listener's configuration lives in its **own** file, `%APPDATA%\WSLComms\remote\remote.json` —
deliberately NOT `config.json`, because the Settings screen rewrites the whole config document from a
page cache and would silently drop a field it does not restate. A network listener's bind address
must not be reachable by that mechanism.

`remote.json` defaults, and the values a MISSING file yields, are:

```json
{ "enabled": false, "bind": "127.0.0.1", "port": 8443, "clients": [] }
```

So **doing nothing leaves the machine not listening.** When you do enable it, it still binds only
loopback unless you deliberately widen the bind address, and:

- The bind must be a **literal IP**, never a hostname — what is exposed cannot be allowed to change
  under DNS between the moment you read it in Settings and the moment the socket binds.
- A **non-loopback bind with zero clients is refused.** A listener reachable from the LAN that nobody
  can authenticate to is not a convenience waiting for its first client; it is unauthenticated attack
  surface. Add at least one client first.

You configure all of this from the local Settings screen (the `GetRemoteState` / `SetRemoteListener`
/ `AddRemoteClient` / `SetRemoteClientPassword` / `DeleteRemoteClient` methods). Those methods are
**host-only**: a remote client can never reconfigure the listener that is carrying it.

---

## Enabling it, and trusting the certificate

1. Open **Settings → Remote access** on the local (commentary PC) window.
2. Add a client: a name, a password, and its capabilities (see tiers below). The password is hashed
   with PBKDF2-HMAC-SHA256 and never stored in the clear; there is no way to read it back.
3. Set **enabled**, and — only if a second machine genuinely needs to reach it — widen the bind from
   `127.0.0.1` to the LAN IP of the commentary PC. Leave the port at 8443 unless it clashes.
4. Settings shows the **URL** (`https://<bind>:<port>`) and the certificate's **SHA-256
   fingerprint**. The certificate is self-signed (ECDSA P-256, generated on first enable into
   `%APPDATA%\WSLComms\remote\`), so the remote browser will show a warning the first time.
5. In the remote browser, navigate to the URL, compare the certificate fingerprint the browser shows
   against the one in Settings, and only then trust it. Log in with the client name and password.

> A self-signed certificate trains an operator to click through a browser warning, which is a habit
> worth being uneasy about. **The fingerprint comparison is the compensating control — do it.** If
> the facility has an internal CA, importing a real certificate is the better answer before this goes
> anywhere near a shared network.

TLS is **mandatory, not decorative**. Over plain HTTP a LAN page is not a secure context, so
`navigator.mediaDevices` is undefined (the headphone dropdown is permanently empty) and `setSinkId`
is missing (return audio cannot be routed to a chosen device). Separately, `GetKVSCredentials`
returns live AWS session credentials that must not cross a facility LAN in clear.

---

## Capability tiers

Each client is granted one tier; the tiers are inclusive (**mixer ⊇ operate ⊇ view**).

| Tier | What it can do |
|---|---|
| **view** | Read-only: watch the lamps, the config, the mixer snapshot, the presets; fetch the KVS mosaic credentials; and `DisarmMixer` (shutting a gate is always safe). |
| **operate** | Everything in view, plus configuration and session control: `Start`/`Stop`, `SaveConfig`, `SetSecret`, the preset writes, and `ArmMixer` (which changes nothing on the desk — it only opens the write window). |
| **mixer** | Everything in operate, plus the arm-gated write path to the live desk: `SendMixerCommands` and `SetMixerGolden`. **Off by default**, because it is the one tier that can change what goes to air. |

**Arm-ownership.** `SendMixerCommands` requires not just the mixer capability but that the caller is
the seat that most recently armed. With two controllers, one operator's arm must not silently
authorise the other's write to the live clean feed — so a write from any seat other than the arming
one is refused with the same `ErrDisarmed` sentinel as a write with no arm at all. Every mutating
remote call (`Start`, `Stop`, `SaveConfig`, `SetSecret`, `ArmMixer`, `SendMixerCommands`,
`SetMixerGolden`, the preset writes) is written to the log with the client name and source address;
`SetSecret` logs the KEY it wrote, never the value.

---

## What degrades, and what cannot work at all — the honest table

A remote browser is a **second controller, not a viewer**, and it is NOT the local operator's exact
experience. Some of that is a secure-context limitation fixed by TLS; some of it is physics.

| Capability | Remote behaviour | Why |
|---|---|---|
| **The SRT programme picture** | **Impossible.** The remote page shows the WebRTC multiviewer mosaic and an honest message; the high-quality SRT picture is never sent. | It is a native child window painted by `d3d11videosink` on the HOST GPU, outside the DOM (`internal/gst/overlay_windows.go`). No transport that carries the DOM can carry it. And `SetPictureRect` takes the CALLING page's CSS rect and DPI, so a remote browser at another size would drag the operator's own picture around. The six picture/return geometry methods are host-only and refused. |
| **The commentary INPUT device list** | Correct and useful. | `ListInputDevices` enumerates the HOST's WASAPI endpoints; picking host hardware from a remote seat is the intended behaviour. |
| **The headphone (output) picker** | Works only under TLS. | `navigator.mediaDevices` needs a secure context; over plain HTTP the dropdown is empty. TLS is mandatory, so in normal operation it works. |
| **Return audio** | Plays on the REMOTE machine's speakers/headphones, via `setSinkId`. | Useful for a remote producer, but it is NOT what the commentator is hearing. The SRT audio return to the HOST's headphones is host-only and refused. |
| **Monitoring level / gain sliders** | Apply to the REMOTE browser's own Web Audio graph — BUT `returnChannel`/`returnMid`/`returnGainDb` are SAVED and propagate to the local page via the `config` event and are re-applied to the commentator's ears. | The saved fields are shared config; a remote producer adjusting "their" monitoring can therefore change the commentator's. Say so on the remote UI. |
| **Config edits** | Two controllers editing Settings at once will still lose one set of edits; the `config` event shrinks the clobber window to one round trip but does not eliminate it. | Config is written as a whole object from each page's cache. A single-writer lease would close this, at the cost of materially more UI. |
| **Everything else** (lamps, presets, arm/disarm, mixer read, mixer write with the capability) | Works. | These are ordinary bound calls; the shim installs exactly the methods the server's hello frame lists for the client's tier. |

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

## Threat model

Enabling the listener is a genuine attack-surface change on a machine that is on air. The bridge
controls `SendMixerCommands` (a write path to a live broadcast desk), `SetSecret` (passphrases), and
`GetKVSCredentials` (live AWS session credentials). The mitigations, and what each closes:

- **Off by default, loopback by default.** The safe posture is the one you get by doing nothing; a
  non-loopback bind with no clients is refused outright.
- **TLS-only, self-signed with a shown fingerprint.** No credential and no control frame crosses the
  LAN in clear; the fingerprint lets an operator verify the endpoint before trusting it.
- **PBKDF2-HMAC-SHA256 password hashes** (600k iterations, per-password random salt), constant-time
  comparison, and a fixed minimum login delay that both rate-limits guessing and flattens the timing
  difference between "no such user" and "wrong password".
- **Per-source-IP lockout** after repeated failures, turning an online guessing attack into a crawl.
- **HttpOnly / Secure / SameSite=Strict session cookie** AND a **strict same-origin check** on the
  WebSocket upgrade (Origin host:port must equal Host; a missing Origin is refused). This closes the
  exact hole Wails' dev server leaves open and the class of cross-site request that would ride the
  operator's cookie.
- **Hand-written allowlist, not reflection.** Wails binds every exported method of the app object;
  the remote dispatcher exposes only methods classified in an explicit table, drift-guarded by a test
  that fails if any bound method is left unclassified. A new binding cannot default into being
  remotely callable.
- **Host-only refusals.** The native picture/return surface and the remote-admin methods are refused
  for every remote client at every capability, and omitted from the method list the remote page ever
  sees.
- **Capability tiers + arm-ownership.** The write path to the live desk needs both the mixer
  capability and being the seat that armed.
- **An audit log** of every mutating remote call, with the client name and source address.

These mitigations are only as good as their tests; treat the test suite as the deliverable, not the
code. And nothing here removes the standing advice in `CONTRACT.md` rule 4: this is a live broadcast
machine, so validate any change against `cmd/mockm2lx` and a bench event before a match, never by
adding a seat during one.

---

## Known limits, stated plainly

- The SRT picture cannot be remoted, ever (above).
- Two config editors can still lose one set of edits (above); the `config` event only narrows the
  window.
- Two KVS viewers means two `RTCPeerConnection`s on the same signalling channel and two copies of the
  mosaic and return audio over the network; bandwidth, AWS cost and the channel's viewer limit have
  not been measured. Test on the mock and a bench event first.
- No match-length soak of a local window and a remote browser coexisting has been run. Schedule that
  with the operator; there is no way to prove the coexistence without running the real app, which
  rule 4 forbids doing on the live machine mid-match.
