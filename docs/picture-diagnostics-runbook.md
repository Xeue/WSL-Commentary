# Picture-return tearing — diagnostics runbook

Everything needed to nail the software-HEVC tearing in one session. Built 2026-08-25; the rig
(section 0) added 2026-09-10 after the tear was seen on several different laptops. Read top to
bottom; sections 3–4 are what the rig automates and how to read what it brings back.

## 0. The field rig — run this first (2026-09-10)

One exe, nothing to type. `wslcomms-rig-v<version>.exe` is the portable launcher under a file name
containing "rig": the same build as `wslcomms-portable-v<version>.exe`, started in rig mode
(`WSLCOMMS_RIG=1`; see `rig.go`). On the laptop that tears, with M2L-X on:

1. **Close WSL Commentary** — the main window and the PGM monitor. The rig looks for them and waits.
2. **Double-click `wslcomms-rig-v1.6.2.exe`.** A console opens and narrates. About three minutes.
3. **A zip appears on the Desktop**, `WSLComms-rig-<computer>-<stamp>.zip`, and Explorer opens on it.
   Send it. It is ~120 MB, most of it the captured stream.

The target comes from the **matchg** preset on that laptop (host, return port 40504, latency, the
return's encryption and its stored passphrase — used, never written); `WSLCOMMS_RIG_PRESET` names
another. The four readings of section 4, in one run, over ONE set of bytes:

| # | reading | file | says |
|---|---|---|---|
| 1 | `internal/tsprobe` capture, 60 s, **no decoder**; bytes banked to `cap.ts` | `probe-live.txt` | REAL holes vs FLAGGED discontinuities on the video PID, DTS monotonicity, NAL census |
| 2 | live dissection, `avdec_h265`, real time | `dissect-live-avdec_h265.txt` | the app's own receive path under pressure; CORRUPTED frames counted at the decoder; tsdemux CONTINUITY; demux DISCONT |
| 3 | `cap.ts` replayed, `avdec_h265`, flat out | `dissect-file-avdec_h265.txt` | the same bytes with no clock: **decoded fps = this laptop's software decode ceiling** against the 50 it needs |
| 4 | `cap.ts` replayed, `d3d11h265dec` | `dissect-file-d3d11h265dec.txt` | whether the laptop has a usable hardware HEVC decoder, and whether it is clean |

Plus `machine.txt` (OS build, CPU, memory, every display adapter and driver, battery/mains, remote
session, power plan, any `WSLCOMMS_*` A/B variables), `target.txt`, the app's last week of logs
under `logs/`, `config/` (config.json and the preset), `rig.log`, and **`summary.txt`**, which states
what the numbers support (section 4's rules, applied) and nothing more — the cross-machine step,
replaying `cap.ts` on the dev box, is the next step and is why `cap.ts` is in the zip.

Overrides, all optional: `WSLCOMMS_RIG_SECS` (60), `WSLCOMMS_RIG_TARGET=host:port` (the bench: a
local listener; skips the "close the app" wait), `WSLCOMMS_RIG_OUT` (folder instead of the Desktop),
`WSLCOMMS_RIG_NOWAIT` (no Enter at the end). Verified end to end on the bench 2026-09-10 against a
local `gst-launch-1.0 ... srtsink mode=listener` replaying a real HEVC capture: all four readings,
the zip, Explorer.

## 0b. RESULT — root cause found and fixed (2026-09-10, from the first rig zip, COMM-01)

The four readings on COMM-01 (Core 5 120U, no hardware HEVC, 7.8 GB with 1.4 GB free):

1. Probe: 16.09 Mbit/s, video PID 0x0041 HEVC, **1** real hole in 59 s, 0 flagged, DTS monotonic.
2. Live, `avdec_h265`: 47.8 fps, **0 CORRUPTED**, but **32 tsdemux continuity mismatches** in 60 s.
3. File replay, `avdec_h265`: **361 fps flat out** — software decode has 7x headroom; 41 mismatches.
4. `d3d11h265dec`: not creatable (no hardware decoder, as known).

So the bytes arrive intact, the decoder is fast enough, and yet tsdemux reports a "hole" ~40 times a
minute in bytes two independent parsers call continuous — and the same replay on the dev box gave
the identical numbers (252 `Could not find ref`, 2820 of 2955 pictures). The fault travels with the
bytes and the pipeline, on every machine. Scanning the capture found it: **40 packets on the video
PID with `adaptation_field_control=0b11` and `adaptation_field_length=183` — a payload flag and
zero payload bytes** — one before each of 40 pictures' first packet, counted by the muxer, skipped by
tsdemux, so the next packet reads as a skip; tsdemux's reaction (`gst_ts_demux_handle_packet`) frees
the finished picture it was holding and ignores the packet starting the next one. Two pictures lost
per event, then `Could not find ref` until the next IDR (GOP 10 frames = 0.2 s): the grey/green
garbage that "cleans up at the next keyframe", forty times a minute.

**Fix (1.6.3, `internal/gst/tsrepair.go`):** on `srtsrc`'s src pad, rewrite that packet to length
182 plus one `0x00` payload byte (a legal trailing zero in Annex B). Proved on COMM-01's bytes through
the app's own dissector: repair on → 40 repaired, 2896 decoded, 0 reference errors, 0 video
mismatches; repair off → 2820 decoded, 252 reference errors, 41 mismatches. A/B in the field:
`WSLCOMMS_PIC_TSREPAIR=0`. The dissector (and so the rig) reports "zero-payload TS packets made
compliant before tsdemux: N"; the app's srt stats line carries the running count.

What the earlier theories got wrong, for the record: SRT loss (there was none — the stats were right),
frame threading, parser-state resets (the rare bursts are the JOIN before the first SPS/PPS, ~1.1 s,
and are benign), the 120U's decode (361 fps), Parsec, memory. The one instrument that could see it was
the continuity counter read by TWO parsers that disagreed about the same bytes.

## 0c. Since 1.6.5: the picture never falls behind, and the kick is remote

- **Catch-up** (`internal/gst/catchup.go`): a probe on `picq`'s src pad reads the queue's fill on
  every access unit; at 15 queued (0.3 s) it drops everything up to the next keyframe with the
  queue at 3 or fewer, flags that keyframe DISCONT, and decoding resumes there with no garbage.
  "We cannot fall behind at all, I would rather drop frames." Log lines: "the decoder is N access
  units behind; dropping to the next keyframe" / "caught up: skipped N". The srt stats line carries
  the totals. `WSLCOMMS_PIC_CATCHUP=0` disables; `WSLCOMMS_PIC_CATCHUP_AU=N` moves the mark.
  Proved on the real GStreamer behind a throttled consumer (`catchup_live_test.go`).
- **Remote kick**: `RefreshMonitorPicture` (an event to the PGM monitor page: its own Refresh) and
  `RestartMonitor` are reachable from every seat and audit-logged. Buttons: the application's PGM
  monitor card and rail group; a remote seat's Picture section ("Refresh desk picture", "Restart
  desk monitor").

## 1. What we know (evidence, not theory)

- **Symptom:** COMM-01 (Dell Pro 14, Intel Core 5 120U = 2 P + 8 E cores, no hardware HEVC — Dell fused it off; Parsec remote) tears the software-HEVC picture "on and off indefinitely", persistently, never fully losing picture. The SAME MatchG H.265 1080p50 stream decodes clean on the dev box and under every software-avdec stress test there.
- **SRT is clean:** 0–3 packets lost of 1.48M (recovered), 0 dropped. Not the wire.
- **h265parse "broken/invalid nal" drops are RARE, not the persistent tear:** one 0.72 s burst in a 24-minute run, then nothing. The dropped slices are COMPLETE/full-size — the parser is missing its SPS/PPS state at that instant (proven from gsth265parse.c: a slice drops ONLY at the `GOT_SPS|GOT_PPS` gate, and those bits are cleared ONLY by the element's `start()`). In a pipeline that never rebuilds, WHAT resets the parser mid-stream is the open question.
- **The persistent signal is tsdemux `CONTINUITY: Mismatch` on pid 0x0041 (the VIDEO PID), ~1/sec.** These correlate with the persistent tear; the rare h265parse bursts do not.
- **The stream:** HEVC (byte-stream) + one stereo AAC-LC (fakesinked), bitrate pinned 15000, `hev1 1920x1080 50/1 main L4.1 8-bit`. Video PID 0x0041. tsdemux runs at defaults (no PID pin).
- **Shipping app has `output-corrupt=false`** — a frame avdec KNOWS is damaged FREEZES (holds last good), it does not tear. So *tearing* (not freezing) means either the corruption is not flagged by avdec (silent wrong decode of a corrupt slice payload), or it is not a decode mechanism at all.

Ruled out: SRT loss; frame-threading on the hybrid CPU (single-thread still tears); DISCONT/flush clearing the parser (source proves it can't); codec_data injection (byte-stream caps ignore it); stream difference.

## 2. The tools (in `build\dist\`)

| tool | needs | what it measures |
|---|---|---|
| **probe.exe** | nothing (pure Go, standalone) | Dials the SRT output, dissects the RECEIVED transport stream: per-PID continuity split into **REAL** (genuine video holes) / **FLAGGED** (legal discontinuities, the noise behind "CONTINUITY: Mismatch") / duplicate; PMT codec map (which PID is video); HEVC NAL census; PES **DTS monotonicity** (a backward DTS = the pipeline-restart bug). `-save cap.ts` banks the raw stream; `-file cap.ts` re-analyses one offline. |
| **wslcomms-portable.exe** `WSLCOMMS_DIAGNOSE=` | it's the app | Pipeline dissector: builds `srtsrc\|filesrc ! tsdemux ! queue ! h265parse ! avdec_h265 ! fakesink`, taps srtsrc:src / queue:sink / h265parse:src / decoder:src for buffer + byte counts, flag histograms (**DISCONT, CORRUPTED, GAP**...), **events** (CAPS/FLUSH/SEGMENT/RECONFIGURE — the reset suspects), a NAL census into the parser, and a census of every bus message. Leaves `output-corrupt=TRUE` so damaged frames are COUNTED at the decoder. A `.ts` file target replays a capture through filesrc. |
| **gstprobe.exe** | run inside build\env.ps1 (dev only) | The same dissector on the bench, for replaying a captured `.ts` here. |

## 3. Morning procedure (COMM-01) — turn M2L-X on first

Target = the host:port your COMM-01 picture actually dials (MatchG: `m2lx-wslstudios-matchg.etapsiota.com:40504`, unencrypted). Only one SRT peer per output, so **stop the running app before probing**.

1. **Capture + TS layer** (this banks the bytes so the live stream is needed only once):
   ```
   probe.exe -save cap.ts -secs 60 m2lx-wslstudios-matchg.etapsiota.com:40504
   ```
   Read the verdict; keep `cap.ts` and `probe-report-*.txt`.

2. **Pipeline dissection** (PowerShell, the 1.5.5 portable):
   ```
   $env:WSLCOMMS_DIAGNOSE='m2lx-wslstudios-matchg.etapsiota.com:40504'
   $env:WSLCOMMS_DIAGNOSE_SECS='60'
   .\wslcomms-portable.exe
   ```
   It prints and also writes `diagnose-*.txt` to the logs dir. Then clear it:
   ```
   $env:WSLCOMMS_DIAGNOSE=''
   ```

3. **Test the 1.5.5 fix** — run the app normally (re-injection is ON by default). Does the tearing change vs 1.5.4? Then A/B it:
   ```
   [Environment]::SetEnvironmentVariable('WSLCOMMS_PIC_REINJECT_PARAMS','0','User')   # OFF, relaunch, compare
   [Environment]::SetEnvironmentVariable('WSLCOMMS_PIC_REINJECT_PARAMS',$null,'User') # back to ON
   ```

4. **Send back:** `cap.ts`, the `probe-report`, and the `diagnose-*.txt`. With `cap.ts` I can replay COMM-01's exact bytes through the pipeline HERE, offline, as many ways as needed.

## 4. Decision tree (what each result means)

- **probe: REAL cc errors > 0 on the video PID** → the received video has genuine holes. Replay `cap.ts` here: if it shows the same holes, they're in the SOURCE TS (M2L-X muxer); if not, the loss is on COMM-01's link/receiver despite SRT's "0 lost" (late-drop past the TSBPD deadline).
- **probe: only FLAGGED discontinuities (0 REAL)** → the received video is intact; the "CONTINUITY: Mismatch" warnings are benign; the tear is DOWNSTREAM in decode.
- **probe: DTS went BACKWARDS on the video PID** → sender-side pipeline restart jamming the decoder; a distinct, known fault.
- **harness: CORRUPTED count at the decoder with a clean parser (no broken/invalid)** → avdec itself is producing damaged frames on this stream/CPU → decoder swap / concealment / the 120U.
- **harness: broken/invalid on the bus + the NAL-in count at the parser not matching the AU-out count** → the parser is dropping slices → the SPS/PPS-state reset path (what 1.5.5 re-injection targets); look at the harness **events** line for a CAPS/FLUSH/SEGMENT at h265parse that would explain the reset.
- **gstprobe cap.ts tears here too** → the fault travels with the BYTES → reproducible offline, iterate on dev.
- **gstprobe cap.ts clean here** → field-CPU/timing-specific → fix space is decoder choice / concealment / the hybrid CPU.
- **1.5.5 removes the rare bursts but the persistent tear stays** → confirms two separate faults; focus entirely on the CONTINUITY/decode axis above.

## 5b. Verified offline, and the source baseline (2026-08-25)

Both tools were exercised end-to-end against **real M2L-X captures** (no live stream), so they are known-good before the field session:
- A prior HEVC PGM capture runs clean through the whole dissector: 530 frames in → 530 AUs → **530 decoded frames, 0 CORRUPTED, no broken/invalid, no CONTINUITY** — the clean baseline to diff a field capture against.
- Every prior M2L-X output capture (H.264 and HEVC, all on video PID 0x0100) shows **0 continuity errors and monotonic DTS**. So the M2L-X *source* produces clean transport streams. COMM-01's ~1/s CONTINUITY mismatches on 0x0041 are therefore **not** normal source behaviour — they are introduced on COMM-01's path, or specific to MatchG's live config. The morning `probe.exe` on the LIVE stream (REAL vs FLAGGED) localises them; comparing a fresh capture from a good machine to one from COMM-01 confirms path-vs-source.

## 5c. Env-var A/B matrix (no rebuild)

`WSLCOMMS_PIC_REINJECT_PARAMS=0` (disable re-injection) · `WSLCOMMS_PIC_THREAD_TYPE=slice|frame|auto` · `WSLCOMMS_PIC_MAX_THREADS=N` · `WSLCOMMS_DIAGNOSE=<host:port|file>` · `WSLCOMMS_DIAGNOSE_SECS=N` · `WSLCOMMS_DIAGNOSE_DECODER=avdec_h265|d3d11h265dec`.
