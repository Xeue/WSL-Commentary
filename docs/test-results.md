# WSL Studios / Sony M2L-X — Commentary Contribution: Test Battery Results Package

**Instance:** `m2lx-wslstudios-matcht.etapsiota.com` (34.242.91.248, eu-west-1) · **Event:** `dl9-5p5ah0bd-empd`
**Test window:** 2026-07-29 · **Audio mixer mode throughout: `internal`** (`GET /api/audio/mixer/mode/dl9-5p5ah0bd-empd` → `{"audio_mixer_mode":"internal"}`). `POST /api/audio/mixer/mode/update` was **never called**.

Evidence tags used below: **[EXP]** verified by experiment on this instance · **[CODE]** verified from the shipped `main.js` bundle · **[DOC]** verified from AWS documentation · **[INF]** inference · **[UNTESTED]** not run.

---

## 1. Results summary

### 1.1 Findings that change the design — read these first

**A. The three internal-mode audio paths are NOT time-aligned. The engine bus path arrives 310.7 ms later than a byte-transparent direct route of the same input. [EXP]**
N = 16 frequency-coded burst pairs, captured simultaneously by one process against a common wall-clock reference: mean **+310.5 ms**, median **+310.7 ms**, **sd 0.6 ms**. That is 15.5 frames at 1080p50. Track 3 (commentary via direct route) *leads* tracks 1 and 2 (mix and FX-only via engine buses) by that amount. Uncorrected this is a gross, audible echo for any consumer who mixes tracks, and it mis-aligns the deliverable if AWS aligns tracks by arrival.

**B. Consequence of A: a remux/alignment stage IS required in internal mode, notwithstanding MediaLive's multi-PID capability.** Two independent reasons, both hard:
1. MediaLive has **no per-PID or per-AudioDescription delay control** for live inputs — `AudioPidSelection.PremixSettings` provides channel remix, gain and loudness only, not offset. [DOC] The 311 ms must therefore be removed **upstream** of MediaLive.
2. A MediaLive channel consumes **one active input at a time**; multiple input attachments are failover/switching, not simultaneous combination. [DOC] In internal mode the three audio sources leave M2L-X as **three separate SRT streams** (PGM bus, CLN bus, direct route). MediaLive cannot sum audio across separate inputs; `AudioPidSelection.Pids[]` only sums PIDs **within one transport stream**.

The earlier "no remux required" conclusion is correct *conditionally* — it holds only once the contributing PIDs are already in one TS. Getting them into one TS is exactly what the remux does. **The multi-PID finding is still valuable: it moves the mixing out of ffmpeg and into MediaLive, so the remux shrinks to "delay + mux", which can be almost entirely stream-copy.**

**C. You do not need the PGM bus at all, and dropping it halves the egress. [EXP+INF]**
If the commentary strip is routed to **no bus** (or to `master` only when the venue also wants it), then:
- PGM bus audio = FX only (track 2), and PGM bus video carries the programme with graphics/DSK,
- the commentary direct route = track 3,
- track 1 (mix) is built downstream in AWS with controlled gain.

That is **two SRT pulls (~17 Mbps)** instead of three (~32 Mbps), it removes the M2L-X master-bus unity-sum clipping trap entirely (measured: two −5 dBFS contributors summed to **+1 dBFS with a −27 dB distortion residual** [EXP]), and it gives the client AWS-side control of commentary level in the mix without touching the venue.

**D. Commentator return audio is free, and it is already a mix-minus. [EXP]**
The WebRTC programme monitor carries **7 discrete stereo Opus tracks**, one per mixer bus. Monitor track index 1 (transceiver **mid 2**, msid `myAudioTrack1`) is the **aux1 / CLN** bus. With FX → `["master","aux1"]` and commentary → `["master"]`, aux1 is simultaneously (a) the AWS FX-only deliverable and (b) an inherent N−1 for the commentator. One bus, two consumers, **no extra SRT path, no extra port, no listener contention**. This deletes a whole transport from the app design.

**E. Hard fan-out ceiling: FOUR output destinations per router input — THREE if that input is assigned to a switcher input. [EXP]**
`HTTP 400 / MESSAGE.9013 "The output destinations for a single router input are limited to four."` Router input 21 (has strip `cam21-1`) accepted 3 direct outputs and rejected the 4th; router input 11 (no switcher strip) accepted 4 and rejected the 5th. Every production camera input is switcher-assigned, so budget **3**. Fan-out beyond that must happen in AWS (MediaConnect), not in M2L-X.

**F. M2L-X can PUSH as well as be pulled. [EXP]**
For outputs, `reverse=false` = SRT-Caller: M2L-X dials out to `address:port`, preserving all audio PIDs and language tags byte-for-byte. Proven end-to-end by pointing output 13 at the instance's own router input 20 (input went online, output reported `status=online` with an **empty** `status_message_id`, both audio PIDs arrived bit-identically). `MESSAGE.9017` on an output means "outbound connection could not be established", not a fatal error — proven differentially by changing only the `address` field.

**G. External mode's multi-track shape is now precisely known from code, and it would solve A and B at once — if NDI is reachable. [CODE]**
`GET /api/audio/mixer/output_audio_track/{event}` returns a two-element array, index 0 = **PGM**, index 1 = **CLN**, each with an `assign` list of `{track_id, external_track_id}` supporting **track_id 1–8**. `external_track_id` is an enum: `999 = Not Assigned`, `1 = "EXT Mixer T1(CH1/2)"` … `8 = "EXT Mixer T8(CH15/16)"`. So in external mode PGM and CLN can each carry up to **8 stereo tracks** sourced from a 16-channel NDI return from an external mixer. All three deliverable tracks would then come off **one output, through one engine path — no skew, no remux**. Whether those tracks are packaged as separate MPEG-TS audio PIDs or as one multi-channel stream is the decisive unknown; §5 answers it in one pass.

### 1.2 Pass / fail register

| # | Test | Result | Key measurement |
|---|---|---|---|
| 1 | Multi-PID transparency through a direct route | **PASS** | 2/3/4/**8** stereo AAC PIDs relayed intact (0x101–0x108), langs eng/fra/deu/spa/ita/nld/por/swe preserved; levels within **0.3 dB** of source; cross-PID separation **≥95 dB**; no limit reached at 8. Mux rate 6.17 → 7.16 Mbps for 2 → 8 PIDs. |
| 1b | Per-PID addressability downstream | **PASS** | `-map 0:a:N -c copy` produced standalone single-PID TS files retaining language tags. |
| 2 | Output caller mode (`reverse=false`) | **PASS** | Loopback to own router input 20: input went online, output `status=online` + empty message, both PIDs bit-identical. Unreachable destination → `MESSAGE.9017`, zero bytes. |
| 3 | Fan-out from one router input | **PASS with hard limit** | Ceiling **4** (3 if switcher-assigned) via `MESSAGE.9013`. Three concurrent consumers: outputs 12 and 13 byte-identical at **9,699,328 B / 51,592 TS packets**, identical PID tallies (0x100:48456, 0x101:1512, 0x102:1365), **zero SRT drops**, aggregate ~**18.6 Mbps**, no degradation. |
| 4a | Three-track separation | **PASS** | MIX carries both tones; **CLN rejects commentary by 133.3 dB**; **direct route rejects FX by 143.2 dB**. |
| 4b | Three-track remux to one TS | **PASS** | H.265 0x100 + AAC-LC 0x101/0x102/0x103 (eng/qaa/qab), **15.62 Mbps** total = ~14.85 Mbps video + **705 kbps** audio (255,993 + 255,995 + 192,737). |
| 4c | Time alignment between paths | **FAIL (finding)** | Engine bus **+310.7 ms** vs direct route, sd 0.6 ms, N=16. Design-changing. |
| 4d | Engine gain linearity | **PASS** | Exact unity: 440 Hz at −16.48 dBFS in → −16.48 dBFS out, peak −7.78 both, identical to 0.01 dB. |
| 5a | `set_comp_limit` limiter | **PASS with caveat** | `limiter_th=−3` moved PGM fundamental −0.81 → **−3.04 dB** (pinned to threshold); direct route untouched (−0.42 → −0.43). **But distortion residual worsened −10.04 → −7.42 dB.** |
| 5b | Two-contributor gain-sum characterisation | **NOT RUN** | Blocked: the second router input was held by a concurrent agent (SRT listeners serve exactly one peer). |
| 6 | WebRTC monitor 7-track layout | **PASS, with the briefing's track identities corrected** | See §2. |
| 7 | AWS ingest direction / quotas / region | **PASS (documentary)** | SRT_CALLER matches `reverse=true`. Pull inputs 100 vs push inputs 5 per region. Media host in **eu-west-1** (34.242.91.248 ∈ 34.240.0.0/13, `ip-ranges.json` createDate 2026-07-29). RTT min 20.1 / med 21.7 / mean 23.8 / max 45.2 ms (n=12, TCP:443; ICMP blocked). |
| 8 | Naive remux fail-open over SRT | **FAIL (finding)** | Killing the comms sender froze the pipeline **~4.6 s** of wall clock (media time stuck at 00:00:15.38 across elapsed 10.29 → 14.93 s) and **permanently removed the comms audio PID** (724 packets vs 2814). |
| 9 | Hardened remux fail-open | **PASS** | Zero stall — media time tracked wall clock monotonically through the kill. All three PIDs at exactly **2814 packets / ~59.9 s**; comms track became digital silence at **−91.0 dBFS** and the PID stayed alive. |
| 10 | MediaLive multi-PID summing | **PASS (documentary), UNVERIFIED live** | `AudioPidSelection.Pids[]` + per-PID `PremixSettings` interleave before `RemixSettings`. Not yet exercised against a real channel. |

### 1.3 Still unknown

1. **Whether CLN (aux1) has the same 310.7 ms delay as PGM (master).** Both are engine buses so they are expected to match, but this was measured PGM-vs-direct only. **Measure before relying on tracks 1 and 2 being mutually aligned.** [UNTESTED]
2. **Whether the 310.7 ms constant is stable across instance restarts / event stop-start / format changes.** sd was 0.6 ms *within* one capture. [UNTESTED]
3. **The two-contributor summed-programme figure** (FX at −20 dBFS + commentary at −18 dBFS into master: peak and residual). Blocked. Unity gain and near-0 dBFS clipping are characterised; the specific operating-level sum is not.
4. **Cause of the ~18 dB offset** between SRT ingest peak level and internal bus/monitor level. Measured consistently at two injection levels; no configuration field found that sets it; not tested against the client's production input 1. Determines make-up gain for commentator monitoring.
5. **Whether the KVS signalling channel serves more than one VIEWER concurrently.** If the operator's browser and our app fight over it, the monitor design changes. **Critical for deployment, not tested.**
6. **Cognito token lifetime and refresh behaviour** from `/api/live_operation/kvs/webrtc_token`. [UNTESTED]
7. **Whether M2L-X's SRT supports AES passphrase/stream-id**, and what latency the outbound caller negotiates. [UNTESTED]
8. **Everything about external mode below the API-shape level** — §5 is the plan to close it.
9. **Whether a MediaConnect flow preserves all audio PIDs of a multi-PID TS.** High confidence from architecture (transport-only service, no transcode surface in the API), but not doc-quoted and not tested. One 10-minute flow test settles it.
10. **TEST 4's absolute levels are not attributable** — the tones came from a concurrent agent's feed. The routing/separation *result* is sound (the routing under test was ours, verified by read-back immediately before capture); the absolute level relationships should be re-measured with a controlled source.

> **Process note, reported honestly:** a second agent was working the same instance and the same scratchpad throughout the streaming battery. Both were issuing `taskkill /IM ffmpeg.exe` and competing for the same single-peer SRT listeners. This contaminated TEST 4's source tones and blocked TEST 5b. Future runs need exclusive use, or at minimum non-overlapping port ranges and scratchpad prefixes.

---

## 2. The monitor audio track result

### 2.1 Is the 7-track layout real? Yes — but the briefing's track identities were wrong.

Opening `/live-operation/dl9-5p5ah0bd-empd` creates **exactly one `RTCPeerConnection` with 8 recvonly transceivers**: 1 video (mid 0) + 7 audio (mids 1–7), all in KVS stream `myKvsVideoStream`. Reproduced identically in 4 separate browser sessions, instrumented via Playwright `addInitScript`. [EXP] Confirmed in code: `addTransceiver("video",{direction:"recvonly"})` then `let o = this.internalMixing ? kb.Internal : kb.External; for(let a=0;a<o;a++) addTransceiver("audio",{direction:"recvonly"})`. [CODE]

**Measured track map** (tone injected on one bus at a time, read back, then swapped as a control):

| idx | mid | msid | **ACTUAL bus** | Sony's enum name | Evidence |
|---|---|---|---|---|---|
| 0 | 1 | `myAudioTrack` | **master** | PGM | 1234.02 Hz at −40.8 dB vs 777 Hz at −136.3 dB → **95.5 dB** isolation; swapped correctly in config B |
| 1 | 2 | `myAudioTrack1` | **aux1** | CLN | 777.02 Hz at −39.8 dB vs 1234 Hz at −133.9 dB → **94.1 dB**; swapped correctly |
| 2 | 3 | `myAudioTrack2` | **aux2** | "MON" | Sweep E: tone routed only to `aux2` appeared on mid 3 at −39.8 dB; silent (−240 dBFS) whenever the tone went only to master/aux1 |
| 3 | 4 | `myAudioTrack3` | **mon1** | "MIC1" | Sweep C: `cam21-1 → ["mon1"]` → 1234 Hz on mid 4 at −40.8 dB |
| 4 | 5 | `myAudioTrack4` | **mon2** | "MIC2" | Sweep C: `cam22-1 → ["mon2"]` → 777 Hz on mid 5 at −40.8 dB |
| 5 | 6 | `myAudioTrack5` | **mon3** | "MIC3" | Sweep D: `cam21-1 → ["mon3"]` → 1234 Hz on mid 6 |
| 6 | 7 | `myAudioTrack6` | **mon4 (PFL)** | PFL | Sweep F: reachable **only** via `{"matrix":"pfl",...}` — routing to `["mon4"]` on matrix `output` did nothing (meter stayed −100, mid 7 stayed −240 dBFS) |

Config B swapped the routing and the tones swapped tracks exactly, proving **the mapping follows the BUS, not the strip.** [EXP]

**The correction that matters:** tracks 3/4/5 are **not** pre-baked N−1 mixes and **not** mic input signals. They are ordinary `mon1/mon2/mon3` buses that merely have MIC 1/2/3 routed to them by factory default. **There is no server-side N−1.** Mix-minus is assembled client-side by summing tracks: [CODE]

```
case N_1_MIC1: enableAudioTrackIndices=[MON,MIC2,MIC3]   // = aux2 + mon2 + mon3
case N_1_MIC2: [MON,MIC1,MIC3]
case N_1_MIC3: [MON,MIC1,MIC2]
case N_0:      [MON,MIC1,MIC2,MIC3]
case N_3:      [CLN]                                      // = aux1
case PFL:      [PFL]
default:       [PGM]                                      // = master
```
`updateAudioTrackState()` then instantiates one `<audio>` element per selected index and plays them simultaneously.

**Second correction with real value:** `aux2` — previously recorded as having *no egress* (SRT output `source:"aux2"` → HTTP 400 "Output source is invalid.") — **does have egress, via the WebRTC monitor as track index 2.** It is a third independently routable, independently audible bus. So are mon1–mon3.

### 2.2 Track characteristics

- **Codec:** Opus PT 111, 48 kHz, `a=fmtp:111 minptime=10;stereo=1;useinbandfec=1`, 20 ms ptime, `channels=2` in `getStats`. [EXP]
- **Genuinely discrete stereo, no downmix:** a left-only 1234 Hz tone (ingest L −24.04, R −100 on the M2L-X meter) arrived on mid 1 as **L RMS −27.33 dBFS / R RMS −240 dBFS** (digital silence); L@1234 = −38.1 dB, R@1234 = −300 dB. [EXP]
- **Bandwidth:** **256.3 kbps** per *active* track, **1.2 kbps** per *silent* track. All 7 run at 50.0 pkt/s. `packetsLost = 0`, jitter 20 ms, average jitter buffer 60.7–61.8 ms. Subscribing to all 7 is nearly free when idle; budget **~1.8 Mbps** worst case if all were live. [EXP]
- **Tap point:** **post output fader.** aux1 and aux2 have `output_fader gain = 1 dB` while master has `0 dB`; measured mid 2 (aux1) at −29.09 dBFS vs mid 1 (master) at −30.09 dBFS for identical source tones — exactly the 1 dB fader difference. [EXP]
- **Level:** the monitor track level equals the M2L-X bus meter (sine peak basis) to within **0.1 dB**, and both sit **~18 dB below the SRT-ingested peak level**. Repeatable at two injection levels. Cause not established. [EXP / cause UNTESTED]
- **Latency (control → audible):** 501 / 474 / 492 ms, **mean 489 ms**, at 60 ms sampling granularity. This includes control-plane WSS RTT, mixer apply, Opus encode, KVS transit and the ~61 ms jitter buffer — it is an **upper bound** on the media-only path. A clean media-only figure would compare the same audio event on the monitor vs a direct-route SRT egress. [EXP / media-only UNTESTED]

### 2.3 Bonus: the video track is the whole multiviewer

All 31 `<video>` elements report intrinsic **2240×1440** and share exactly **one** video track id, wrapped in 31 distinct `MediaStream` objects and CSS-cropped per tile. [EXP] Computed source rects:

- **PVW** `(0, 0, 640, 360)` · **PGM** `(0, 360, 640, 360)`
- source thumbnails on a **320×180** grid at x = 640/960/1280/1600/1920, y = 0/181/362/543/723/904

One WebRTC session therefore yields **the entire multiviewer plus 7 buses**. Do not hard-code tile coordinates without confirming they are fixed across input counts. [UNTESTED]

### 2.4 What this means for the app

1. **Commentator return = monitor track at transceiver mid 2 (aux1 / CLN).** With FX → `["master","aux1"]` and commentary → `["master"]` (or nowhere), aux1 is inherently mix-minus. No MIC N−1 machinery is needed — our commentary arrives as a **router SRT input**, not a MIC input, so the entire MIC/N−1 subsystem is irrelevant to us.
2. **Select tracks by transceiver MID or by SDP msid — never by track-event arrival order.** Sony's own `updateProxyStream()` sorts `tmpRTCTrackEvents` by `o.timeStamp - a.timeStamp` and then indexes `getAudioTracks()[i]`. [CODE] Arrival order matched mid order in all 4 sessions, but nothing enforces it. **Key off mid: mid 2 = CLN return.**
3. **Apply ~+18 dB make-up gain**, or a volume control with plenty of range. The monitor arrives ~18 dB below programme level — far too quiet for headphones as delivered.
4. **Expose the CLN-fader coupling.** Because the tap is post output fader, an operator riding the CLN output fader changes **both** the AWS FX-only deliverable **and** the commentator's return level. Lock the fader or surface the coupling in the operator UI.
5. **Spare buses:** aux2, mon1, mon2, mon3 are four independently routable buses with monitor egress and no SRT egress. Use them for a producer/talkback mix or an alternate commentator bed without touching the AWS deliverables.
6. **Implementation path.** WebView2 is the pragmatic v1: host Sony's page, inject an init script wrapping `RTCPeerConnection` (`AddScriptToExecuteOnDocumentCreatedAsync`) exactly as this investigation did, pull mid 2, route to WASAPI. Proven — it is precisely what was done here. For native, the signalling is 4 REST calls + a SigV4-signed WSS, all with .NET SDK coverage (`AWSSDK.CognitoIdentity`, `AWSSDK.KinesisVideo`, `AWSSDK.KinesisVideoSignalingChannels`):
   `GET /api/live_operation/kvs/webrtc_info/{event}` → `{"region":"eu-west-1","signaling_channel":{"pgm":["webrtc-wslstudios-matcht"]}}` → `GET /api/live_operation/kvs/webrtc_token/{event}` → `{identity_id, token}` → Cognito `GetCredentialsForIdentity` → `DescribeSignalingChannel` → `GetSignalingChannelEndpoint` → `GetIceServerConfig` → SigV4 WSS to `v-*.kinesisvideo.eu-west-1.amazonaws.com` as VIEWER. AWS ships **no** .NET KVS-WebRTC SDK, so only the media stack must be filled (SIPSorcery, or a libwebrtc/Pion sidecar). [EXP]
7. **Test the two-viewer case before committing.** If the KVS channel serves only one VIEWER, our app and the operator's browser will fight. That would force proxying or an SRT return path.
8. **External mode:** the code requests **8** audio transceivers (`kb.External`) and the monitor component selects by `trackId` initialised to 1. [CODE] The layout is almost certainly the 8 EXT mixer tracks — §5 Step 12 verifies it.

---

## 3. Delivery options table — every way audio leaves M2L-X in internal mode

All existing outputs are SRT with `reverse=true` (M2L-X **listens**, consumer pulls as SRT caller). Each SRT listener serves **exactly one peer**; a second caller is rejected and the incumbent is never displaced; after an abrupt disconnect a listener may refuse re-accept for ~5 s. `reverse=false` inverts this (M2L-X dials out). [EXP]

| # | Output configuration | Stream format out | Measured bitrate | Audio content | Time base | AWS ingest | MediaLive alone? |
|---|---|---|---|---|---|---|---|
| **1** | `source:"pgm"`, `reverse:true` | H.265 Main + **1 × stereo AAC-LC 48 kHz**, 1 audio PID | **15,000 kbps** video (pinned) + **~256 kbps** audio | master bus. = FX+commentary if comms routed to `master`; = FX only if not | **T + 311 ms** | SRT_CALLER pull | Yields track 1 **or** track 2 — one only |
| **2** | `source:"cln"`, `reverse:true` | identical to #1 | 15,000 + ~256 kbps | aux1 bus = FX only (comms rejected by **133.3 dB**) | **T + 311 ms** | SRT_CALLER pull | Yields track 2 only |
| **3** | `source:"pvw"`, `reverse:true` | identical to #1 | 15,000 + ~256 kbps | **audio is the master bus — identical to PGM**; only video differs | T + 311 ms | — | Useless for audio; do not use |
| **4** | `source:"<router input id>"`, `through_mode:true` (direct route) | **byte-transparent relay** of whatever was ingested | test feed **~2 Mbps**; 8-PID case **7.16 Mbps** (2-PID case 6.17 Mbps) | exactly what the app sends: verified **8 stereo AAC PIDs** with lang tags, **8-ch SMPTE 302M** at ~77 dB separation, and 16 ch across 2 PIDs | **T + 0 ms (reference)** | SRT_CALLER pull | Yields track 3 only; **and can carry several tracks at once** |
| **5** | `source:"<input>"`, `through_mode:false` | **BROKEN** — first PID only; 302M yields zero decodable frames | — | — | — | — | Do not use |
| **6** | any of the above with `reverse:false` + `address`+`port` | identical payloads; M2L-X **dials out** | same | same | same | MediaLive SRT_LISTENER / MediaConnect srt-listener | Same as its pull equivalent |
| **7** | `source:"aux2"` / `"aux1"` / `"master"` / `"mon1"` | **HTTP 400 "Output source is invalid."** | — | — | — | — | **No SRT egress** |
| **8** | WebRTC programme monitor (KVS) | Opus 48 kHz stereo × 7 + one 2240×1440 mosaic | **256.3 kbps** active / **1.2 kbps** silent per track | all 7 buses incl. aux2 & mon1–4 | monitor path (~489 ms upper bound, control-inclusive) | Not an AWS-ingest path — browser/app only | **No** — monitor only; use for commentator return |

**Constraints that bound any combination:**
- Bus outputs are **hard-pinned**: POSTing `codec` / `bitrate` / `audio_codec` changes returns **200 and silently reverts**. You cannot reduce the 15 Mbps H.265, and you cannot add a second audio PID to a bus output. [EXP]
- `POST /api/output/update/.../{id}` must include `status` and `status_message_id` or it 422s; it is refused while the output is started ("Since it is online, settings cannot be applyed."); `name` max 16 chars. [EXP]
- **Always `GET /api/output/list/{event}` immediately before any start/stop/update.** `HTTP 400 / MESSAGE.9301 ("Please press the [F5] key")` is cleared by exactly this. Eight consecutive stop calls failed with 9301; interleaving a list call made every one succeed on attempt 0. [EXP]
- Fan-out ceiling **4 per router input, 3 if switcher-assigned** (`MESSAGE.9013`). [EXP]
- Ingest → egress ≈ **1 s**. Input goes stopped ~1.3–1.5 s after its source dies and recovers automatically ~2.4 s after return. [EXP]

### 3.1 Measured time alignment between paths

| Path pair | Offset | Method |
|---|---|---|
| Engine bus (PGM) vs direct route of the same router input | **+310.7 ms** (engine later); mean 310.5, sd 0.6, N=16 | Frequency-coded 40 ms bursts (3000 + 200·n Hz) every 5.000 s into router input 20; simultaneous timestamped capture of output 8 (direct, :40508) and output 13 (pgm, :40513) by one Node process against a common capture-stop wall clock. Burst onsets exactly 5.000 s apart within each stream (direct τ = 1.109/6.109/11.109…, pgm τ = 1.835/6.835/11.835…). [EXP] |
| CLN bus vs PGM bus | **assumed 0, NOT MEASURED** | Both are engine buses; expected to match. **Measure before shipping.** [UNTESTED] |

### 3.2 Viable three-track combinations

| Shape | Outputs pulled | M2L-X egress | Deliverable video | Alignment work | Verdict |
|---|---|---|---|---|---|
| **S1 — RECOMMENDED.** Comms strip routed **nowhere**; pull PGM (video + FX audio) + comms direct route | 2 | **~17 Mbps** | PGM, with DSK/graphics | delay comms **+311 ms**; build mix downstream | Cheapest, no clipping risk, AWS-side mix control. **M2L-X PGM bus is unchanged for other consumers.** |
| **S2.** Comms → `["master"]`, FX → `["master","aux1"]`; pull PGM (mix) + CLN (FX) + comms direct | 3 | **~32 Mbps** | PGM, with graphics | delay comms +311 ms; verify CLN≡PGM timing | Use when the venue/other consumers must hear commentary on the M2L-X PGM bus. Inherits the master-bus clipping trap. |
| **S3.** Pull CLN (video + FX audio) + comms direct | 2 | ~17 Mbps | **CLN — clean, no graphics** | delay comms +311 ms | Only if the deliverable is the clean feed. |
| **S4.** Everything from buses (no direct route) | — | — | — | zero skew | **IMPOSSIBLE in internal mode** — aux2/mon1–4 have no SRT egress. This is what forces the direct route and hence the skew. |

---

## 4. The AWS configuration

Recommended target: **S1**, delivered as **one MPEG-TS with two audio PIDs** into MediaLive, which builds the three deliverable tracks natively.

```
M2L-X (eu-west-1)
 ├─ output A  src=pgm         reverse=true  :40511   H.265 15 Mbps + AAC stereo (FX)   ─┐
 └─ output B  src=<comms in>  reverse=true  :40510   byte-transparent relay (commentary)┤
                                                                                        │
                             ┌──────────────────────────────────────────────────────────┘
                             ▼
             align+mux (ffmpeg, eu-west-1)   delay comms +310.7 ms, mux to 1 TS, 2 audio PIDs
                             │
                             ▼
             MediaConnect flow (srt-caller in / N outputs+entitlements)
                             │
                             ▼
             MediaLive SRT_CALLER input → 3 AudioDescriptions → 3-track TS out
```

### 4.1 MediaLive input (SRT_CALLER — matches `reverse=true`)

```json
{
  "Name": "m2lx-wsl-matcht-contribution",
  "Type": "SRT_CALLER",
  "SrtSettings": {
    "SrtCallerSources": [
      {
        "SrtListenerAddress": "34.242.91.248",
        "SrtListenerPort": "40512",
        "StreamId": "",
        "MinimumLatency": 120,
        "Decryption": {
          "Algorithm": "AES256",
          "PassphraseSecretArn": "arn:aws:secretsmanager:eu-west-1:111122223333:secret:m2lx/srt/passphrase-AbCdEf"
        }
      }
    ]
  },
  "Tags": { "Project": "WSL-Studios", "Env": "dev" }
}
```

- `SrtListenerPort` is a **string**; `MinimumLatency` is an **int in ms**. [DOC]
- **Omit the entire `Decryption` block** unless M2L-X's SRT is confirmed to support a passphrase — untested. [UNTESTED]
- Consumes 1 of the **100 Pull Inputs**, not the 5 Push Inputs. [DOC]
- **A STANDARD (2-pipeline) channel needs two `SrtCallerSources`, i.e. two separate M2L-X listeners**, because each M2L-X listener serves exactly one peer. With the fan-out ceiling of 3 per switcher-assigned input, a standard channel plus one spare consumer already exhausts the budget. **Prefer feeding MediaConnect once and letting MediaConnect fan out to both MediaLive pipelines.** [DOC + EXP]

### 4.2 MediaLive audio: two discrete PIDs → three tracks, no downstream mixing

Three selectors; the third spans **both** PIDs. This is the configuration that removes the standalone mixer box.

```json
"AudioSelectors": [
  { "Name": "sel-fx",    "SelectorSettings": { "AudioPidSelection": { "Pid": 481 } } },
  { "Name": "sel-comms", "SelectorSettings": { "AudioPidSelection": { "Pid": 482 } } },
  { "Name": "sel-mix",
    "SelectorSettings": {
      "AudioPidSelection": {
        "Pids": [
          { "Pid": 481, "PremixSettings": { "Channels": 2, "GainDb": -6 } },
          { "Pid": 482, "PremixSettings": { "Channels": 2, "GainDb": -6 } }
        ]
      }
    }
  }
],
"AudioDescriptions": [
  {
    "Name": "track1-mix", "AudioSelectorName": "sel-mix",
    "LanguageCode": "eng", "LanguageCodeControl": "USE_CONFIGURED",
    "CodecSettings": { "AacSettings": { "Bitrate": 192000, "CodingMode": "CODING_MODE_2_0", "SampleRate": 48000, "Profile": "LC", "RateControlMode": "CBR" } },
    "RemixSettings": {
      "ChannelsIn": 4, "ChannelsOut": 2,
      "ChannelMappings": [
        { "OutputChannel": 0, "InputChannelLevels": [ {"InputChannel":0,"Gain":0}, {"InputChannel":2,"Gain":0} ] },
        { "OutputChannel": 1, "InputChannelLevels": [ {"InputChannel":1,"Gain":0}, {"InputChannel":3,"Gain":0} ] }
      ]
    }
  },
  { "Name": "track2-fx",    "AudioSelectorName": "sel-fx",    "LanguageCode": "nar", "LanguageCodeControl": "USE_CONFIGURED",
    "CodecSettings": { "AacSettings": { "Bitrate": 192000, "CodingMode": "CODING_MODE_2_0", "SampleRate": 48000, "Profile": "LC", "RateControlMode": "CBR" } } },
  { "Name": "track3-comms", "AudioSelectorName": "sel-comms", "LanguageCode": "qad", "LanguageCodeControl": "USE_CONFIGURED",
    "CodecSettings": { "AacSettings": { "Bitrate": 128000, "CodingMode": "CODING_MODE_2_0", "SampleRate": 48000, "Profile": "LC", "RateControlMode": "CBR" } } }
]
```

**How it works.** The PIDs in `sel-mix` are interleaved into one 4-channel stream (PID 481 → input channels 0,1; PID 482 → channels 2,3) **in PID-array order**, with `PremixSettings` applied per PID *before* interleaving; `RemixSettings` then folds 4 → 2. CloudFormation documents `Pids` as "Selects one or more unique packet identifiers (PIDs) from within a source… you can specify per-PID audio pre-mixer settings", and `PremixSettings` as applying "channel remixing, gain adjustment, and loudness normalization to this PID **before interleaving**". [DOC] `AudioTrackSelection.Tracks[]` is the MP4/QuickTime analogue and is the **wrong** tool for MPEG-TS. MediaLive allows up to 20 audio selectors per channel. [DOC]

> **VERIFY the channel ordering on first deployment** by muting one source and observing which output channels go quiet. Do not assume. [UNTESTED]

**Gain staging.** `GainDb: -6` on both premix entries provides headroom for the unity sum that follows. Unity summing of two near-0 dBFS sources is exactly the trap already measured on the M2L-X master bus (two −5 dBFS → +1 dBFS, −27 dB residual). Confirm the permitted `InputChannelLevels.Gain` range against the API reference before applying (commonly −60…+6 dB). [UNTESTED]

**If the aligned feed arrives instead as one 4-channel PID** (FX ch1-2, comms ch3-4), use a single `{"AudioPidSelection":{"Pid":481}}` selector and three AudioDescriptions differing only in `RemixSettings` (4→2 sum for track 1; 4→2 taking channels 0,1 for track 2; channels 2,3 for track 3). Simpler and portable to any SDK version.

**PRE-DEPLOYMENT GATE — do this first.** `Pids` + `PremixSettings` is a recent feature ("Adding premixer settings to pid and track audio inputs in MediaLive… including support for AudioPidSelectors made up of multiple audio PIDs"). An out-of-date CLI/SDK/CFN provider will reject `Pids` as an unknown field. Run:

```
aws medialive create-channel --generate-cli-skeleton | grep -A6 AudioPidSelection
```

If it shows only `Pid`, upgrade the CLI/SDK — or fall back to the 4-channel shape or to a remux that builds all three tracks itself. **This is the single biggest deployment risk on this path.** [DOC, exact version UNTESTED]

### 4.3 Output group — all three tracks in one MPEG-TS

```json
"OutputGroups": [{
  "Name": "aws-delivery-ts",
  "OutputGroupSettings": { "UdpGroupSettings": { "InputLossAction": "EMIT_PROGRAM", "TimedMetadataId3Frame": "NONE" } },
  "Outputs": [{
    "OutputName": "three-track",
    "VideoDescriptionName": "video-1080p50",
    "AudioDescriptionNames": ["track1-mix", "track2-fx", "track3-comms"],
    "OutputSettings": {
      "UdpOutputSettings": {
        "Destination": { "DestinationRefId": "dest-1" },
        "ContainerSettings": { "M2tsSettings": {
          "AudioPids": "482-498",
          "VideoPid": "481",
          "PcrControl": "PCR_EVERY_PES_PACKET",
          "AudioBufferModel": "ATSC",
          "Bitrate": 15000000,
          "RateMode": "CBR",
          "NielsenId3Behavior": "NO_PASSTHROUGH"
        }},
        "FecOutputSettings": { "IncludeFec": "COLUMN_AND_ROW", "ColumnDepth": 5, "RowLength": 5 }
      }
    }
  }]
}]
```

`AudioPids: "482-498"` assigns the three audio PIDs sequentially from 482. `InputLossAction: EMIT_PROGRAM` keeps the TS structurally alive if the contribution drops — the AWS-side analogue of the fail-open behaviour validated locally.

### 4.4 MediaConnect fan-out

```json
{
  "Name": "wsl-matcht-contribution",
  "AvailabilityZone": "eu-west-1a",
  "Source": {
    "Name": "m2lx-matcht",
    "Protocol": "srt-caller",
    "SourceListenerAddress": "34.242.91.248",
    "SourceListenerPort": 40512,
    "MinLatency": 120,
    "StreamId": "",
    "Description": "aligned contribution TS, FX + comms audio PIDs"
  }
}
```

Quotas: **50 outputs** and **50 entitlements** per transport-stream flow (non-adjustable), 2 sources per flow, 20 flows per region (adjustable), recommended aggregate output bandwidth ≤ 400 Mb/s. [DOC] Entitlements give cross-account/cross-region distribution over the AWS backbone with the subscriber paying their own egress. MediaConnect does not transcode, so a multi-PID TS passes through untouched. [INF — architecture, not doc-quoted; confirm with one 10-minute flow test]

**Why MediaConnect rather than M2L-X-native fan-out:** M2L-X-native costs nothing in AWS charges but consumes the 33 output slots, is capped at **3 per switcher-assigned router input**, sends N copies over the contended stadium uplink, and each listener still serves exactly one peer. Use M2L-X-native only for 2 consumers on the same site.

**Cost shape:** MediaConnect = hourly per running flow + per-GB transfer; internet-bound outputs carry an extra hourly rate; first 100 GB/month to internet destinations free; reserved outbound bandwidth cuts per-GB by 70%+. The eu-west-1 transport-stream flow hourly rate was not surfaced in the pricing page text (only CDI/JPEG XS figures). [DOC, partial]

### 4.5 Latency and region

- **Region: eu-west-1.** The media host resolves to 34.242.91.248, which falls in **34.240.0.0/13**, tagged `eu-west-1 / AMAZON+EC2` in the authoritative `ip-ranges.json` (createDate 2026-07-29-15-27-05). This matches the measured ~21 ms RTT. **Disregard the `ap-northeast-1` value in event metadata** — that is control-plane/tenant metadata, not the media path. Both MediaLive and MediaConnect are available in eu-west-1. [EXP + DOC]
- **SRT latency: 120 ms** (≈4× median RTT, ~2.7× the worst observed 45.2 ms). Set it on **both** ends — MediaConnect resolves latency as `max(sender, receiver)`, so a one-sided setting is silently overridden upward, and ineffective if the peer asks for less. Fields: MediaLive `SrtCallerSource.MinimumLatency = 120`; MediaConnect `Source.MinLatency = 120`; ffmpeg `latency=120000` — **microseconds in ffmpeg, milliseconds in AWS**. Do not use `MaxLatency`; it applies only to RIST and Zixi. [DOC]
- **End-to-end budget:** ~1 s M2L-X ingest→egress [EXP] + 120 ms SRT + align/mux stage + MediaLive pipeline.

### 4.6 The align + mux stage (required — see §1.1 B)

This is **not** the mixing box that the multi-PID finding deleted. It does two things only: apply the measured **+310.7 ms** delay to the commentary, and mux two sources into one TS. MediaLive still builds all three tracks.

**Production version — hardened fail-open, video and FX stream-copied, only commentary decoded:**

```bash
ffmpeg -hide_banner -loglevel warning \
  -thread_queue_size 1024 -i "srt://34.242.91.248:40511?mode=caller&latency=120000&transtype=live" \
  -thread_queue_size 1024 -i "srt://34.242.91.248:40510?mode=caller&latency=120000&transtype=live" \
  -re -thread_queue_size 1024 -f lavfi -i "anullsrc=r=48000:cl=stereo" \
  -filter_complex "[1:a]aformat=sample_fmts=fltp:sample_rates=48000:channel_layouts=stereo,adelay=14914S|14914S:all=1[cmd];[cmd][2:a]amix=inputs=2:duration=longest:dropout_transition=0:normalize=0[comms]" \
  -map 0:v -c:v copy \
  -map 0:a:0 -c:a:0 copy \
  -map "[comms]" -c:a:1 aac -b:a:1 128k -ac 2 -ar 48000 \
  -metadata:s:a:0 language=nar -metadata:s:a:1 language=qad \
  -muxdelay 0 -muxpreload 0 \
  -f mpegts "srt://<mediaconnect-or-medialive-listener>:5000?mode=caller&latency=200000&transtype=live"
```

- **Input 0** = PGM bus (programme video + FX audio). **Input 1** = commentary direct route. **Input 2** = infinite real-time silence.
- `adelay=14914S|14914S` = 310.708 ms at 48 kHz, sample-accurate. **Re-verify the constant** if the instance is restarted or the system format changes.
- **Fail-open, measured:** the commentary branch is `amix`ed against a real-time infinite `anullsrc` with `duration=longest`, so that branch **never reaches EOF** even when the commentary SRT peer dies. Process lifetime is governed by input 0. Validated over real SRT with the sender SIGKILLed at T+20 s: **zero stall** (media time tracked wall clock continuously: 00:00:15.38/0:00:14.94 → 00:00:17.47/0:00:17.00 → 00:00:19.52/0:00:19.07 → 00:00:21.58/0:00:21.13, no repeated `time=` anywhere), all PIDs at **2814 packets / ~59.9 s**, commentary track became digital silence at **−91.0 dBFS with the PID alive**. [EXP]
- **The naive version is dangerous and must not be used.** Same test without the silence branch: **~4.6 s wall-clock freeze** (media time stuck at 00:00:15.38 from elapsed 10.29 s to 14.93 s), then `[in#1/mpegts] Error during demuxing: I/O error`, and the commentary PID **disappeared permanently** (724 packets / 15.445 s vs 2814 for the others). A PID-pinned downstream selector loses its source forever. [EXP]
- `-re` on `anullsrc` is **essential**; without it the silence branch free-runs and races the muxer.
- **Supervision:** the process still exits when input 0 dies. Run under systemd `Restart=always RestartSec=5` or as an ECS service. **`RestartSec` must be ≥ 5 s** because an M2L-X SRT listener may refuse re-accept for ~5 s after an abrupt disconnect. [EXP]

**Lower-CPU variant (no re-encode at all):** replace the filter chain with `-itsoffset 0.310708 -i <comms>` and `-c copy` throughout. This makes the stage a pure mux. **Trade-off:** you lose the amix-against-silence fail-open, because a copied stream cannot be back-filled — protection then rests entirely on supervision plus MediaLive's `InputLossAction: EMIT_PROGRAM`. Verify `-itsoffset` + `-c copy` against a real decoder before adopting. [UNTESTED]

**Full-fallback variant (remux builds all three tracks itself):** if the target account's CLI/SDK predates `AudioPidSelection.Pids`, use the previously validated three-PID command, with `adelay=14914S|14914S:all=1` inserted on the commentary branch before the `asplit`. That produced a validated single TS: video 0x100 + AAC-LC 0x101/0x102/0x103, langs eng/nar/qad, **muxing overhead 5.60%**, per-track bandpass measurements mix 440 Hz −33.1 / 1000 Hz −33.1, FX 440 −33.1 / 1000 −68.8 (**35.7 dB rejection**), comms 440 −61.7 / 1000 −33.1 (**28.6 dB rejection**). [EXP]

---

## 5. THE EXTERNAL-MODE SWITCH PACKAGE

**Objective:** in one pass, prove or disprove that external mode gives multi-track PGM/CLN on a single SRT output, and determine whether NDI is usable at all from a cloud instance.

**Everything below is derived from the shipped bundle and from live probes of the *internal*-mode instance. The endpoint paths, payload shapes and enum values are [CODE]-verified; nothing in external mode has been executed.**

### 5.0 API surface (all [CODE], paths confirmed against `AUDIO_MIXER_URL = /api/audio/mixer`)

| Operation | Method + path | Body |
|---|---|---|
| Read mode | `GET /api/audio/mixer/mode/{event}` | — |
| **Set mode (RESTARTS INSTANCE)** | `POST /api/audio/mixer/mode/update/{event}` | `{"audio_mixer_mode":"external"}` |
| Read track assignment | `GET /api/audio/mixer/output_audio_track/{event}` | — |
| Write track assignment | `POST /api/audio/mixer/output_audio_track/update/{event}` | `[{"channel":"PGM","assign":[…]},{"channel":"CLN","assign":[…]}]` |
| Read NDI input (M2L-X → ext mixer) | `GET /api/audio/mixer/input/ndi/{event}` | — |
| Write NDI input | `POST /api/audio/mixer/input/ndi/update/{event}` | `{"display_name":"…","ndi_source_name":"…"}` |
| Read NDI output (ext mixer → M2L-X) | `GET /api/audio/mixer/output/ndi/{event}` | — |
| Write NDI output | `POST /api/audio/mixer/output/ndi/update/{event}` | `{"display_name":"…","ndi_source_name":"…"}` |

**Enum `external_track_id`:** `999` = Not Assigned · `1` = "EXT Mixer T1(CH1/2)" · `2` = T2(CH3/4) · `3` = T3(CH5/6) · `4` = T4(CH7/8) · `5` = T5(CH9/10) · `6` = T6(CH11/12) · `7` = T7(CH13/14) · `8` = T8(CH15/16). So the external mixer returns **16 channels = 8 stereo tracks**.

**NDI info object shape:** `{id, status, display_name, ndi_source_name}`.

**Two operational rules from the code:**
- `updateAudioMixerMode()` and `updateAudioTrackAssign()` both call `eventsDialogs.openEventStopDialogIfRun(eventId)` first. **The EVENT must be STOPPED before either the mode change or a track-assignment change.** Expect a rejection or an implicit stop otherwise.
- Track assignment is validated client-side to be **contiguous**: `for(i=1;i<e.length;i++) if(e[i].track_id !== e[i-1].track_id+1) return false`. Entries with `external_track_id === 999` are **stripped** before POST. So send `track_id` 1..N with no gaps.

### 5.1 Internal-mode baseline — ALREADY CAPTURED (2026-07-29 ~22:58 UTC) [EXP]

These are the "before" values; every one must change after the flip.

```
GET /api/config/aws-use                                  -> 200 {"is_aws":true}
GET /api/audio/mixer/mode/dl9-5p5ah0bd-empd              -> 200 {"audio_mixer_mode":"internal"}
GET /api/audio/mixer/output_audio_track/dl9-5p5ah0bd-empd-> 400 {"detail":"This API can only be used in external audio mixer mode"}
GET /api/audio/mixer/input/ndi/dl9-5p5ah0bd-empd         -> 400 {"detail":"This API can only be used in external audio mixer mode"}
GET /api/audio/mixer/output/ndi/dl9-5p5ah0bd-empd        -> 400 {"detail":"This API can only be used in external audio mixer mode"}
```

**This already answers half of one of the brief's questions:** in internal mode all three external endpoints return **HTTP 400 with an explicit mode-gate message**, not 404 and not empty data. After the flip they must return 200 with data; if they still 400, the mode change did not take.

---

### PHASE 0 — Pre-flight (internal mode, no restart). ~15 min. **No NDI mixer needed.**

| Step | Action | Pass criterion |
|---|---|---|
| **0.1** | Snapshot everything: `GET /api/output/list/{ev}` → `EXT_BASE_outputs.json`; `GET /api/input/router/list/{ev}` → `EXT_BASE_inputs.json`; full `advanced_audio_mixer` state from the `switcher_status` WS → `EXT_BASE_aam.json`; the three baseline responses in §5.1. | 3 files written, non-empty |
| **0.2** | Record the current `internal` routing of every strip you will need to restore (factory is `["master","aux1","aux2"]`). | captured |
| **0.3** | Verify test objects exist and are stopped: router inputs 20/21/22, outputs 8/9/10/11/12. | all `status:"none"` |
| **0.4** | **Baseline the direct-route regression reference:** push a known 2-PID AAC stream into router input 21, start output 9, pull it, `ffprobe -show_streams -show_programs`. Save the PMT dump and per-channel tone levels as `REG_internal.json`. Stop the output, kill ffmpeg. | 2 audio PIDs, lang tags preserved, levels within 0.3 dB of source |
| **0.5** | **Baseline the skew** (this also closes open question §1.3-1): with the burst generator on router input 20, pull output 8 (direct) and output 11 (pgm) *and* a CLN-sourced output simultaneously; measure PGM−direct and CLN−direct. | PGM−direct ≈ 310.7 ms; record CLN−direct |

---

### PHASE 1 — The flip (client-run, ~1 restart). **No NDI mixer needed.**

| Step | Action | Pass criterion |
|---|---|---|
| **1.1** | Stop the event (M2L-X UI, or the API the UI's `openEventStopDialogIfRun` invokes). | event status ≠ Running |
| **1.2** | `POST /api/audio/mixer/mode/update/dl9-5p5ah0bd-empd` with `{"audio_mixer_mode":"external"}` | 2xx (endpoint is `postRequestForVoid`, so expect an empty body) |
| **1.3** | Wait for the instance to restart. Poll `GET /api/audio/mixer/mode/{ev}` (re-authenticating — the access token may not survive). | returns `{"audio_mixer_mode":"external"}` |
| **1.4** | Restart the event. | event status Running |

> If step 1.3 never returns `external`, **stop the whole exercise** and report — nothing further is meaningful.

---

### PHASE 2 — Cheapest decisive API-shape tests. ~5 min. **No NDI mixer needed.**

| Step | Action | Pass criterion / what to record |
|---|---|---|
| **2.1** | `GET /api/audio/mixer/output_audio_track/{ev}` | **PASS:** HTTP 200 with a **2-element array**, `[0]` = PGM, `[1]` = CLN, each `{channel, assign:[{track_id, external_track_id}, …]}`. **Record the exact JSON verbatim** — including whether `channel` really is the field name and whether `assign` is pre-populated or empty. **FAIL:** still 400 → mode did not take. **PARTIAL:** 200 but a different shape → the bundle's model is stale; record and re-derive. |
| **2.2** | `GET /api/audio/mixer/input/ndi/{ev}` | **PASS:** 200 with `{id, status, display_name, ndi_source_name}`. This is the **M2L-X → external mixer** direction (the send). **Record `status` and `ndi_source_name` exactly.** |
| **2.3** | `GET /api/audio/mixer/output/ndi/{ev}` | **PASS:** 200 with the same shape. This is the **external mixer → M2L-X** direction (the receive). Record `status`. |
| **2.4** | Poll 2.2/2.3 every 5 s for 60 s (the UI polls at `POLLING_INTERVAL_MS_FOR_GET_NDI_SOURCE_INFO = 5000`). | Record whether `status` ever changes on its own. A `status` that reports "no source" is itself informative. |
| **2.5** | Check whether MIC inputs disappeared: `GET /api/input/mic/list/{ev}` (the bundle sets `isEnableMicInput = (mode === Internal)`). | Expected: MIC inputs unavailable. **Client has said this does not matter — record only.** |
| **2.6** | `GET /api/output/list/{ev}` and diff against `EXT_BASE_outputs.json`. | Record any field the restart changed. |

**Gate:** if 2.1 does not return data, everything after this is moot — go to Phase 7.

---

### PHASE 3 — NDI reachability. **THE DEPLOYMENT BLOCKER. Do it here, before spending time on anything else.** ~20 min. **Steps 3.1–3.4 need no NDI mixer.**

The question is not "does M2L-X support NDI" — it clearly does. The question is **can an NDI sender/receiver outside the AWS instance see it and connect to it.** NDI discovery is mDNS (UDP 5353, link-local multicast, **does not traverse routers or VPCs**) and media is TCP in the 5960+ range. An AWS-hosted instance behind an ALB/security group is the worst possible case.

| Step | Action | Pass criterion |
|---|---|---|
| **3.1** | Set a recognisable send name: `POST /api/audio/mixer/input/ndi/update/{ev}` with `{"display_name":"<value read back in 2.2>","ndi_source_name":"WSLTEST-SEND"}`. Re-`GET` to confirm. | read-back shows `ndi_source_name":"WSLTEST-SEND"` |
| **3.2** | **Port reachability from this workstation** (cheap, definitive-negative): TCP-connect scan `34.242.91.248` on 5353/UDP, 5959–5990/TCP, 6960–6990/TCP, 7960–7990/TCP. | **FAIL-FAST:** all filtered/closed ⇒ **NDI is not reachable from outside the instance and external mode is undeployable over the internet.** Record and go to Phase 4 (the packaging question is still worth answering) then Phase 7. |
| **3.3** | Look for an **NDI Discovery Server** configuration field anywhere in the API: re-check `GET /api/audio/mixer/input/ndi/{ev}`, `/output/ndi/{ev}`, and any system-settings endpoint, for a host/IP field. Grep the bundle: `grep -oE '.{200}(discovery\|DISCOVERY\|NDI_DISC).{300}' main.js`. | Presence of a discovery-server field is the **only** realistic route to WAN NDI. Absence ⇒ NDI is LAN/VPC-only. |
| **3.4** | Determine whether the send is bound to a **private VPC address**. If any NDI-related field exposes an IP, record it. If it is RFC1918, external NDI is dead without a peer inside the same VPC. | recorded |
| **3.5** *(needs an NDI tool, still no mixer)* | Install **NDI Tools** (free) and run `NDI Studio Monitor` / the `ndi-directory-service` finder on this workstation. Also try NDI Tools' "Access Manager" → add `34.242.91.248` as a **remote source** (NDI 5+ supports explicitly-named remote sources without mDNS). | **PASS:** `WSLTEST-SEND` appears and plays. **FAIL:** not discoverable — consistent with 3.2. |

**Honest expectation:** [INF] mDNS will not traverse from a UK workstation to an eu-west-1 EC2. Unless 3.3 finds a discovery-server field, the only viable deployment is an **NDI mixer running inside the same VPC/subnet as the M2L-X instance** — i.e. a new always-on cloud component. That is a strictly worse architecture than the ~60-line ffmpeg align/mux stage §4.6, and it should be the deciding factor in §6.

---

### PHASE 4 — **THE DECISIVE PACKAGING TEST.** ~25 min. **Can be attempted WITHOUT an NDI mixer — see the caveat.**

Does a multi-track assignment produce **separate audio PIDs** or **one multi-channel stream** on an SRT output?

**Caveat, stated plainly:** with no NDI return, the assigned tracks will carry silence. **The PMT is still emitted and is still readable** — PID count, stream types, channel counts and language descriptors are all observable on a silent stream. So the *packaging* question is answerable without a mixer. What is **not** answerable is content correctness (which track carries what) and whether the encoder suppresses tracks that have no source. If Step 4.4 shows only one audio PID, you must distinguish "packaged as multichannel" from "suppressed for lack of source" — Step 4.6 does that.

| Step | Action | Pass criterion |
|---|---|---|
| **4.1** | Stop the event (track assignment requires it — `openEventStopDialogIfRun`). | event stopped |
| **4.2** | `POST /api/audio/mixer/output_audio_track/update/{ev}` with: <br>`[{"channel":"PGM","assign":[{"track_id":1,"external_track_id":1},{"track_id":2,"external_track_id":2},{"track_id":3,"external_track_id":3}]},{"channel":"CLN","assign":[{"track_id":1,"external_track_id":4},{"track_id":2,"external_track_id":5}]}]` | 2xx |
| **4.3** | `GET /api/audio/mixer/output_audio_track/{ev}` and confirm read-back. Restart the event. | assignment persisted exactly |
| **4.4** | Configure output 11 (`source:"pgm"`, `reverse:true`, `port:40511`, `through_mode:true`). **`GET /api/output/list/{ev}` first** (clears MESSAGE.9301), then `POST /api/output/start/{ev}/11`. Pull and dump: <br>`ffmpeg -i "srt://34.242.91.248:40511?mode=caller&latency=120000&transtype=live" -map 0 -c copy -t 20 -f mpegts pgm_ext.ts` <br>then `ffprobe -v error -show_programs -show_streams pgm_ext.ts` | **RECORD:** number of audio elementary streams in the PMT; each stream's PID, `codec_name`, `profile`, `channels`, `channel_layout`, `sample_rate`, and any `language` descriptor. |
| **4.5** | Repeat 4.4 for a CLN-sourced output (2 assigned tracks) on port 40513. | same record |
| **4.6** | **Disambiguation.** Re-run 4.2 with **one** track assigned to PGM, restart, re-capture. Compare PMTs. | If 3-track PGM gave 3 audio PIDs and 1-track PGM gives 1 ⇒ **separate PIDs, N = assigned count. BEST CASE — MediaLive builds all three tracks natively from one output, with no skew and no remux.** <br>If 3-track gave **one** PID with `channels: 6` ⇒ **one multi-channel stream** — still excellent: a single `AudioPidSelection.Pid` selector with three `RemixSettings` (6→2) yields the three deliverables. <br>If both give exactly 1 stereo PID ⇒ **assignment has no effect on SRT packaging** (tracks may be NDI/monitor-only) — external mode gives us nothing for delivery. |
| **4.7** | Also check whether the bus output codec/bitrate pinning changed: is it still H.265 @ 15000 kbps? Is the audio still AAC-LC, or has it become multi-channel AAC / SMPTE 302M? | recorded; measure actual mux bitrate with `ffprobe -show_format` |
| **4.8** | Stop both outputs (`GET /api/output/list` first, then `POST /api/output/stop/...`). | `status:"none"` |

**This step decides the entire architecture.** Record the raw `ffprobe` JSON verbatim.

---

### PHASE 5 — Direct-route regression. ~10 min. **No NDI mixer needed.**

| Step | Action | Pass criterion |
|---|---|---|
| **5.1** | Repeat Phase 0.4 **exactly**: push the same 2-PID AAC stream into router input 21, start output 9, pull, `ffprobe`. | PMT and per-channel levels **identical to `REG_internal.json`** — 2 audio PIDs, lang tags preserved, levels within 0.3 dB |
| **5.2** | Repeat with the 8-PID stream. | 8 PIDs 0x101–0x108, lang tags eng/fra/deu/spa/ita/nld/por/swe, ≥95 dB cross-PID separation |
| **5.3** | Repeat with 8-channel SMPTE 302M. | 8 channels, ~77 dB separation |
| **5.4** | Confirm the fan-out ceiling is unchanged: attempt a 4th destination on a switcher-assigned router input. | `MESSAGE.9013` still at 4 (3 usable) |
| **5.5** | Stop everything, kill ffmpeg. | clean |

**FAIL here would be serious** — it would mean external mode also changes the byte-transparent relay path, which is the only path we currently rely on for track 3.

---

### PHASE 6 — Skew re-measurement. ~10 min. **No NDI mixer needed.**

| Step | Action | Pass criterion |
|---|---|---|
| **6.1** | Repeat Phase 0.5 in external mode: burst generator into router input 20; simultaneous capture of the direct route and a PGM-sourced output; measure the offset. | Record. **If Phase 4 showed separate PIDs on one output, this step also confirms whether all assigned tracks share one time base — the whole point of external mode.** |
| **6.2** | If PGM now carries multiple tracks, cross-correlate track 1 against track 2 and track 3 within the *same* TS. | Expect 0 ms. Any non-zero value must be recorded — it would be a new, unexpected intra-stream skew. |

---

### PHASE 7 — Programme monitor in external mode. ~15 min. **No NDI mixer needed for the structural answer.**

| Step | Action | Pass criterion |
|---|---|---|
| **7.1** | Open `/live-operation/{ev}` with the Playwright `RTCPeerConnection` instrumentation used previously. | **Expect 9 transceivers: 1 video (mid 0) + 8 audio (mids 1–8)**, per `kb.External = 8`. Record actual count, mids, msids. |
| **7.2** | Dump the answer SDP per audio m-line. | Expect `opus/48000/2`, `stereo=1`. Record any deviation. |
| **7.3** | Record which track the app's single unmuted `<audio>` binds to (code initialises `trackId = 1` when `internalMixing` is false). | recorded |
| **7.4** *(needs a mixer to be conclusive)* | Identify each track. **Without an NDI return all 8 will be silent (−240 dBFS)** and identification is impossible. With any NDI source injecting a distinct tone per stereo pair, sweep and map mid → EXT track. | If mapping is achievable: table of mid → EXT Mixer T1..T8 |
| **7.5** | Measure per-track bitrate (`getStats`) for silent tracks. | Expect ~1.2 kbps each, as in internal mode |

**Honest limit:** without a mixer, 7.1–7.3 establish the *structure* (8 tracks, codec, msids) but **not the identities**. Structure alone is enough to size the app's monitor implementation.

---

### PHASE 8 — Return to internal mode safely. ~1 restart.

| Step | Action | Pass criterion |
|---|---|---|
| **8.1** | Clear the track assignment: `POST /api/audio/mixer/output_audio_track/update/{ev}` with `[{"channel":"PGM","assign":[]},{"channel":"CLN","assign":[]}]`. If an empty array is rejected, assign a single track to each. Read back. | 2xx + read-back |
| **8.2** | Restore the NDI source names captured in 2.2/2.3 via `POST /api/audio/mixer/input/ndi/update/{ev}` and `/output/ndi/update/{ev}`. | read-back matches baseline |
| **8.3** | Stop the event. | stopped |
| **8.4** | `POST /api/audio/mixer/mode/update/{ev}` with `{"audio_mixer_mode":"internal"}`. | 2xx |
| **8.5** | Wait for restart; re-auth; `GET /api/audio/mixer/mode/{ev}`. | `{"audio_mixer_mode":"internal"}` |
| **8.6** | Confirm the three external endpoints are gated again. | all three return `400 {"detail":"This API can only be used in external audio mixer mode"}` — **identical to the §5.1 baseline** |
| **8.7** | Restore mixer routing: every touched strip back to `["master","aux1","aux2"]`, muted, `comp_limit` off, faders as baselined. **Read back after every command** — `set_ch_fader`/`set_output_fader` are sometimes ACKed 200 and silently dropped. | diff vs `EXT_BASE_aam.json` empty (ignoring live meter leaves) |
| **8.8** | Diff `GET /api/output/list/{ev}` and `GET /api/input/router/list/{ev}` against the Phase 0 snapshots across all fields. | zero differences |
| **8.9** | Restart the event; confirm client input 1 recovers; confirm zero ffmpeg processes. | input 1 online, `tasklist` clean |

**Rollback if the mode change wedges the instance:** the only lever is `POST /api/audio/mixer/mode/update` with `internal`, which requires the API to be reachable. **Confirm before Phase 1 that Sony support can restore the instance out-of-band.** Do not run this exercise inside a match window.

---

### 5.9 What CANNOT be learned without a real NDI mixer

1. **Whether audio actually flows** either direction over NDI — the send may exist but be unreachable, and there is no return path to test.
2. **Track identity** on multi-track outputs and on the 8 monitor tracks — everything will be silent.
3. **Whether the encoder emits assigned-but-sourceless tracks.** If Phase 4.4 shows fewer PIDs than assigned, you cannot tell "packaged as multichannel" from "suppressed for lack of source" with certainty (4.6 narrows it, but does not eliminate the possibility).
4. **PGM/CLN audio behaviour with no external mixer at all** — plausibly silence on every output. Assume the instance produces **no programme audio** for the duration of the external-mode window.
5. **Latency of the NDI round trip**, and whether it re-introduces or removes the 311 ms skew.
6. **Channel mapping** of EXT Mixer T1..T8 → NDI channels 1..16 (the enum labels assert CH1/2..CH15/16 but that is a UI label, not a measurement).

### 5.10 Smallest thing that could serve as an NDI mixer

In increasing order of effort. **All of these must run where the M2L-X instance can reach them — realistically an EC2 instance in the same VPC/subnet, which is itself the finding.**

1. **NDI Tools "Test Pattern" (free, Windows).** Generates an NDI source with a 1 kHz tone. Proves M2L-X can *receive* an NDI source and lights up one track. Cheapest possible positive signal. Does not exercise 16 channels.
2. **OBS Studio + DistroAV (obs-ndi) plugin (free).** Receives M2L-X's NDI send *and* publishes an NDI output. Gives a genuine loop. OBS is limited to stereo on its main output, but multiple audio tracks can be configured — enough to prove 2–3 tracks.
3. **NDI Tools "Virtual Input" + a DAW / VB-Cable rig.** Lets you generate distinct tones per channel pair for identification sweeps.
4. **VizRT / Sienna / Kiloview NDI-capable audio mixers, or a licensed Calrec/Lawo NDI-enabled unit** — the real thing, 16 channels, needed for any production decision.
5. **ffmpeg is NOT an option** — libndi support was removed from FFmpeg (4.x) over licensing, so there is no `-f libndi_newtek` in the local build.

**Note:** items 1–3 all rely on mDNS discovery unless NDI Access Manager remote-source entries or a discovery server are usable — which Phase 3 determines. **Run Phase 3 before procuring anything.**

---

## 6. Recommendation

### Stay in **internal** mode for the build. Run the external-mode probe as a scheduled, boxed experiment, and treat NDI reachability (Phase 3) as its go/no-go gate.

**Why internal wins, on the measurements:**

1. **Internal works today, end to end, with margin.** Separation is **133.3 dB** (CLN rejecting commentary) and **143.2 dB** (direct route rejecting FX) — one to two orders of magnitude beyond any broadcast requirement. The engine passes at **exact unity** (−16.48 dBFS in, −16.48 dBFS out). Multi-PID relay is transparent to at least **8 stereo PIDs** with language tags and ≥95 dB cross-PID separation. Nothing here needs fixing.

2. **The one real defect — the 311 ms skew — costs a single ffmpeg process.** It is a **fixed, precisely measured constant** (310.7 ms, sd 0.6 ms over 16 pairs), corrected by `adelay=14914S|14914S`. The align/mux stage is ~15 lines, stream-copies the video and the FX audio, decodes only the commentary, and has **measured, validated fail-open behaviour** (zero stall, all PIDs preserved, silence at −91.0 dBFS with the PID alive). That is a small, well-understood, testable component.

3. **External mode's prize is real but it is bought with a much worse component.** External mode would deliver up to 8 tracks per output through one engine path — no skew, no mux stage. But it requires an **external NDI audio mixer that the cloud instance can reach**. NDI discovery is mDNS and will not traverse from a UK stadium to eu-west-1 [INF, to be confirmed by Phase 3], so in practice you would run an NDI mixer **on an EC2 instance inside the VPC** — an always-on cloud component with its own failover story, its own licence, its own operator surface, and its own latency. **You would be deleting a 15-line ffmpeg process and adding a cloud audio mixer.** That trade is clearly negative.

4. **External mode also relocates all mixing out of M2L-X**, which invalidates the app's entire routing model (`set_routing` to `master`/`aux1`, `set_input_muted`, `set_ch_fader`), the operator UI design, and — critically — the **free commentator return**. The mix-minus we get for nothing today (monitor mid 2 = aux1 = CLN = FX-only) would have to be reconstructed inside the external mixer and routed back. That is a fundamental redesign, not a tuning change.

5. **The MIC-input loss is genuinely irrelevant here** (the client has said so, and our commentary arrives as a router SRT input, not a MIC input), and **licensing is not a constraint** — so neither of the usual objections to external mode applies. External mode is being rejected purely on **transport feasibility and architectural cost**, which is the honest reason.

6. **Internal mode also gives a cheaper egress shape.** Recommended **S1**: leave the commentary strip routed nowhere, pull **PGM (programme video + FX audio) + commentary direct route** = **2 pulls, ~17 Mbps**, and build the mix in AWS. This halves the uplink versus the three-output shape, sidesteps the M2L-X master-bus clipping trap entirely (measured: two −5 dBFS → **+1 dBFS with a −27 dB residual**), gives the client AWS-side control of the commentary level in the mix, and — importantly — **leaves the M2L-X PGM bus exactly as the venue already has it.** If the venue must hear commentary on the M2L-X PGM bus, switch to S2 at a cost of one more 15 Mbps pull.

7. **Do not build a mixing box, and do not build a permanent remux box either.** MediaLive's `AudioPidSelection.Pids[]` builds the mixed track natively from two PIDs. The align/mux stage exists only to apply a fixed delay and put two elementary streams in one container. If the pre-deployment CLI gate fails, promote the stage to the validated three-track remux — that is the contingency, not the plan.

**Nevertheless, run the external-mode probe once.** It is one restart and about two hours, most of it cheap. Phase 3 alone (≈20 minutes) either kills external mode permanently — which is worth knowing before anyone designs around it — or opens a genuinely better architecture. Phase 4 answers the packaging question that has been open since the start. Phases 5–7 come almost free once the instance is already in external mode. **Order matters: 0 → 1 → 2 → 3 → 4 → 5 → 6 → 7 → 8**, and abandon early if Phase 2 or Phase 3 fails.

**Do not schedule it near a match.** The event must be stopped twice, the instance restarts twice, and there will very likely be no programme audio at all while in external mode.

**Three things to close before the build freezes, in priority order:**
1. **Measure CLN-vs-PGM alignment** (open question §1.3-1). If CLN and PGM differ, the delay constant is per-path, not global.
2. **Test the two-VIEWER KVS case** (open question §1.3-5). If the channel serves one viewer, our app fights the operator's browser and the monitor design changes.
3. **Run the MediaLive CLI `Pids` gate** (§4.2). One command; determines whether MediaLive or ffmpeg builds the mix.

---

## 7. State of the dev instance

### 7.1 Verified clean at the end of every run

| Check | Result |
|---|---|
| Mixer config diff vs pre-experiment golden (`GOLDEN_aam.json`, `T_BASE_aam.json`) | **EMPTY** on the monitor run; on the streaming run, only leaves `cam1-1[0]` and `cam1-1[1]` differed, proven to be **live meters** (they changed again between two consecutive no-command snapshots: `[-22.64,-23.22] → [-21.58,-23.29]`) |
| All 33 outputs vs `BASE_outputs.json`, 22 fields each | **ZERO DIFFERENCES** |
| Touched strips `cam20-1`, `cam21-1`, `cam22-1`, `cam21-2`, `cam22-2` | routing restored to factory `["master","aux1","aux2"]`, `muted:true`, `ch_fader` disabled, `comp_limit` off/0/0/0 |
| Bus meters | master and aux1 at **−100.0 dBFS** |
| Outputs started | **none** — all 33 `status:"none"` |
| ffmpeg processes | **zero** (`tasklist` empty) |
| `POST /api/audio/mixer/mode/update` | **NEVER CALLED** |
| Client production objects (router inputs 1–19, outputs 1–7) | **unmodified**; input 1 confirmed still online at the end of the streaming battery |
| Only pre-existing unmuted strip | `cam10-1` — untouched throughout |

### 7.2 Objects created or reconfigured, and their current state

Confirmed by a read-only `GET /api/output/list` and `GET /api/input/router/list` at **2026-07-29 ~22:58 UTC**:

**Router inputs (created earlier, left configured and stopped):**
| id | name | reverse | port | status |
|---|---|---|---|---|
| 20 | `CLAUDE-TEST-SRT` | false (M2L-X listens) | 40020 | none |
| 21 | `CLAUDE-FX` | false | 40021 | none |
| 22 | `CLAUDE-COMMS` | false | 40022 | none |

**Outputs (created earlier, left configured and stopped):**
| id | name | source | reverse | port | through_mode | status |
|---|---|---|---|---|---|---|
| 8 | `CLAUDE-RTN20` | 20 | true | 40508 | true | none |
| 9 | `CLAUDE-RTN21` | 21 | true | 40509 | true | none |
| 10 | `CLAUDE-RTN22` | 22 | true | 40510 | true | none |
| 11 | `CLAUDE-PGM` | pgm | true | 40511 | true | none |
| 12 | `CLAUDE-FX-B` | 21 | true | 40512 | true | none |

**Outputs 13–17** were used transiently for the caller-mode and fan-out tests and have been **fully restored to factory defaults** — `name:"Output 13".."Output 17"`, `source:""`, `reverse:false`, `port:0`. Nothing of ours remains on them.

**Mixer state:** internal mode, unmodified, all test strips muted at factory routing.

### 7.3 Read-only probes performed while writing this package (2026-07-29, ~22:58 UTC)

Authenticated as `matcht`. **GET requests only — no state was changed.**
`/api/config/aws-use`, `/api/audio/mixer/mode/{ev}`, `/api/audio/mixer/output_audio_track/{ev}`, `/api/audio/mixer/input/ndi/{ev}`, `/api/audio/mixer/output/ndi/{ev}`, `/api/output/list/{ev}`, `/api/input/router/list/{ev}`. Responses are quoted verbatim in §5.1.

### 7.4 Two observations to flag to the client

1. **All 48 router inputs reported `status:"none"` at 22:58 UTC, including client input 1** (config intact: `reverse:true`, `port:31013`). Earlier in the battery input 1 was confirmed **online**. Nothing in this work touched it — every call made to it was a read. The venue FX contribution appears simply not to have been connected at that moment.

2. **The instance became unreachable at approximately 23:00–23:05 UTC on 2026-07-29 and remained so.** `m2lx-wslstudios-matcht.etapsiota.com` still resolves to `34.242.91.248`, but TCP:443 times out on repeated attempts. A control test at the same moment showed general internet connectivity healthy (`1.1.1.1:443` OK in 31 ms, `ip-ranges.amazonaws.com:443` OK in 43 ms), so this is **not** a local network fault. The last successful API call was a few minutes earlier. Most likely the event or instance was stopped, or an ingress/security-group change was made. **Nothing in this work could have caused it — the only calls in that window were authenticated GETs.** As a consequence, the deeper end-state detail probe (full per-record field dumps, `event/status`, `input/mic/list`, `system/settings`) **could not be completed**; the state above is as of the last successful read.

### 7.5 Files written by this reporting pass (scratchpad only, no project files touched)

- `C:\Users\samsw\AppData\Local\Temp\claude\c--Users-samsw-GitProjects-M2LX-Commentary\2609fb7f-8b67-46ba-a5de-35b19c362845\scratchpad\probe_ext.js` — read-only endpoint probe (produced §5.1)
- `C:\Users\samsw\AppData\Local\Temp\claude\c--Users-samsw-GitProjects-M2LX-Commentary\2609fb7f-8b67-46ba-a5de-35b19c362845\scratchpad\rp_probe2.js` — deeper state probe, **did not complete** (instance unreachable)

No ffmpeg processes were spawned during this pass. No outputs were started. No mixer commands were issued.