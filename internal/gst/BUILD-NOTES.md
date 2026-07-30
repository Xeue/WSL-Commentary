# `internal/gst` — Gate B build notes

**Written by WP-3a. Read this before you try to build `gst_cgo.go`.**

`gst_cgo.go` has never been compiled. It was written on a machine with Go and Node and nothing
else — no MinGW gcc, no pkg-config, no GStreamer, no audio hardware. Everything below that says
"unverified" means exactly that: it is a reading of go-gst v0.0.2's source in the module cache and
of GStreamer's own source, not a thing anyone has run.

What *has* been verified on this machine, at every save:

```
$ gofmt -l internal/gst
(no output)
$ CGO_ENABLED=0 go build ./internal/gst/...
(no output)
$ CGO_ENABLED=0 go vet ./internal/gst/...
(no output)
$ CGO_ENABLED=0 go test ./internal/gst/... -count=50
ok      wslcomms/internal/gst   1.215s
```

That proves the frozen stub twin is undisturbed, that `gst_cgo.go` is at least syntactically valid
Go and structurally satisfies the `Pipeline` interface (§6.1), and nothing else. It proves nothing
about whether the file you are about to build *works*, because `//go:build cgo` excludes it from
every one of those commands.

**Race detection is deferred to Gate B.** `-race` requires cgo, which is exactly what this machine
does not have, so `go test -race` cannot be run here at all. The compensation is `-count=50` above
plus hand-reading the concurrency against the threading model in the `gst_cgo.go` file comment. The
first thing to do once Gate B is open, before anything else in §1's smoke test, is:

```
go test ./internal/gst/... -race -count=50
go test ./internal/sender/... -race -count=50
```

`internal/sender` matters more than this package for that: it is the one that drives a `Pipeline`
from a goroutine while the UI reads its state.

---

## 1. Opening Gate B

Four installs, roughly 2 GB and half an hour, all unattended.

1. **MinGW-w64 gcc, x86_64, POSIX threads, SEH.** The one Go expects on `amd64` Windows.
   Easiest route is MSYS2:

   ```
   winget install -e --id MSYS2.MSYS2
   C:\msys64\usr\bin\pacman -S --noconfirm mingw-w64-x86_64-gcc
   ```

   and then put `C:\msys64\mingw64\bin` on `PATH`. Verify with `gcc --version`; it must say
   `x86_64-w64-mingw32`.

2. **pkgconfiglite.** `choco install pkgconfiglite`, or the MSYS2 `mingw-w64-x86_64-pkgconf`
   package. Verify with `pkg-config --version`.

3. **GStreamer 1.28.5, mingw x86_64, BOTH installers.** The runtime one is what ships; the
   **development** one is what `pkg-config` and the linker need, and it is the one people forget.

   ```
   gstreamer-1.0-mingw-x86_64-1.28.5.msi
   gstreamer-1.0-devel-mingw-x86_64-1.28.5.msi
   ```

   Install "Complete", not "Typical" — "Typical" omits headers.

4. **Wails CLI** (WP-8 needs it; not needed to build this package alone):

   ```
   go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0
   ```

### Environment

```
set CGO_ENABLED=1
set PKG_CONFIG_PATH=C:\gstreamer\1.0\mingw_x86_64\lib\pkgconfig
set PATH=C:\msys64\mingw64\bin;C:\gstreamer\1.0\mingw_x86_64\bin;%PATH%
```

PowerShell:

```powershell
$env:CGO_ENABLED       = '1'
$env:PKG_CONFIG_PATH   = 'C:\gstreamer\1.0\mingw_x86_64\lib\pkgconfig'
$env:PATH              = 'C:\msys64\mingw64\bin;C:\gstreamer\1.0\mingw_x86_64\bin;' + $env:PATH
```

Sanity-check pkg-config **before** invoking Go. This package needs exactly two modules:

```
pkg-config --cflags --libs gstreamer-1.0
pkg-config --cflags --libs gobject-2.0
```

If either fails, nothing else will work and the Go error message will be much less helpful than
this one. (`gstreamer-video-1.0` is deliberately *not* required — see §4.5.)

### Build

```
go build ./internal/gst/...
go vet  ./internal/gst/...
```

Then, and only then, the whole tree:

```
go build ./...
```

Expect the first `go build` of `github.com/go-gst/go-gst/pkg/gst` to take several minutes.
`gst.gen.go` is 51,209 lines of generated cgo and it is compiled once per toolchain change.

### Smoke test, in this order

1. `Init(<dir containing gst\lib\gstreamer-1.0>)` must return nil. If it returns
   `the bundled GStreamer ... is incomplete`, the DLL/plugin allowlist (WP-6) is short — the
   message names every missing factory and the plugin that provides it.
2. `ListInputDevices()` must return at least the Dante inputs, with `ID` looking like
   `{0.0.1.00000000}.{b3f8fa53-...}` and `Name` containing the double space in
   `DVS Receive  1-2`. **If `ID` comes back empty for every device, go straight to §3.1.**
3. `New()`, `Start(...)`, then watch the log line
   `gst: H.264 encoder resolved by rank: ... (rank N)`. That answers specification open
   question 3. Write the answer down.
4. `ReplaceSink(...)` against `cmd/mockm2lx`'s SRT listener before anything real.

---

## 2. If it does not compile — check these first, in this order

**2.1 `cgo: C compiler "gcc" not found`.** `PATH` does not have MinGW on it. This is the exact
error this machine produces today.

**2.2 `pkg-config: exit status 1` / `Package gstreamer-1.0 was not found`.** `PKG_CONFIG_PATH` is
unset or points at the *runtime* installer's tree instead of the *devel* one. There is no
`lib\pkgconfig` in the runtime install.

**2.3 Errors inside `go-gst/pkg/gst/*.go`, not inside our file.** That is SP-2 — go-gst v0.0.2
has no Windows CI and open issue #179. Timebox it to one day, per the specification. If it will
not build, the agreed fallback is a ~200-line hand-written cgo shim behind the same
`internal/gst` signatures. That is a change *inside `gst_cgo.go`*, not a contract change; `gst.go`
and `gst_stub.go` stay exactly as they are. The fifteen C entry points the shim would need are:
`gst_init`, `gst_parse_launch`, `gst_bin_get_by_name`, `gst_bin_add`, `gst_bin_remove`,
`gst_element_factory_make`, `gst_element_set_state`, `gst_element_get_state`,
`gst_element_sync_state_with_parent`, `gst_element_send_event`, `gst_element_get_static_pad`,
`gst_pad_add_probe`, `gst_pad_remove_probe`, `gst_pad_link` / `gst_pad_unlink`,
`gst_bus_set_sync_handler`, plus `gst_device_monitor_*` and `g_object_set` / `g_object_get`.

**2.4 Errors inside `gst_cgo.go`.** They will almost certainly be one of the API assumptions in
§4. Each one there says what to change if it is wrong.

**2.5 `missing go.sum entry` for `github.com/go-gst/go-glib`.** `gst_cgo.go` imports
`github.com/go-gst/go-glib/pkg/gobject/v2` directly (for `gobject.Type`, `gobject.TypeInvalid`
and the `gobject.Object` parameter type). go-glib v0.0.2 is already in `go.mod` and `go.sum` — it
is listed in the `// indirect` block, which is a comment and not a constraint, so the build
resolves it fine. **Do not run `go mod tidy` to "fix" the indirect marker**; `go.mod` is frozen by
CONTRACT.md rule 1. If the coordinator ever does re-tidy, the only change will be that line moving
into the direct require block.

---

## 3. If it compiles but misbehaves

### 3.1 `ListInputDevices` returns devices with an empty `ID`

The endpoint GUID is looked up in the device's `GstStructure` properties under, in order,
`device.id`, `device.strid`, `device.path` — **all three unverified**. If none is present the code
falls back to `gst_device_create_element("")` and reads the resulting `wasapi2src`'s `device`
property, which is authoritative by construction. If *that* also comes back empty, the log line

```
gst: ListInputDevices: skipping "...": no endpoint ID; device properties are [ ... ]
```

prints every property key the provider actually publishes. Add the right one to `endpointIDKeys`.

### 3.2 No H.264 encoder found

`selectH264Encoder` queries `gst_element_factory_list_get_elements` with
`GST_ELEMENT_FACTORY_TYPE_ENCODER | GST_ELEMENT_FACTORY_TYPE_MEDIA_VIDEO`. go-gst does not
generate those macros (they are C preprocessor macros, so they are not in the GIR), so they are
reproduced by hand as `1 << 1` and `1 << 49`. **`1 << 49` is the one to double-check** against
`gstelementfactory.h` on the installed version. If the query matches nothing, the code falls back
to trying `mfh264enc`, `qsvh264enc`, `nvh264enc`, `d3d11h264enc`, `amfh264enc`, `openh264enc` by
name and logs that it did — treat that log line as a bug report against the constants.

### 3.3 The pipeline reaches PLAYING but M2L-X sees nothing

Check, in order: (a) the gate — `ReplaceSink` must have returned nil, because nothing flows until
it does; (b) `srtq`'s last flow return, which the code logs when it re-arms the queue; (c) whether
a `gst: pipeline-fatal:` error appeared on `Errors()`, which means the muxer or capture chain
stopped and only Stop/New/Start will recover it.

### 3.4 Non-monotonic DTS downstream — the bug this whole file exists to prevent

If it happens, the questions are: did anything take an element other than `srtout-N` out of
PLAYING? Did `savedBase` get re-sampled (it logs `gst: sampled the process-lifetime base time`
exactly once per process — a second line is a bug)? Did someone set `start-time-selection=first`
on `mpegtsmux`? Never set that; it reproduces the measured fault precisely.

---

## 4. Every unverified assumption, and what to do if it is wrong

### 4.1 `gst_parse_launch` returns something that satisfies `gst.Pipeline`

`ParseLaunch` is declared as returning `gst.Element`. `Start` type-asserts it to `gst.Pipeline`
because go-gst's `RegisterObjectCasting` should give it a `*PipelineInstance` dynamic type. If the
assertion fails at runtime, the error is
`gst_parse_launch returned a *gst.ElementInstance, not a GstPipeline` — the fix is
`gogst.UnsafePipelineFromGlibNone(gobject.UnsafeObjectToGlibNone(element))`, or wrap the
description in an explicit `pipeline` created with `gst.NewPipeline`.

### 4.2 The bus sync handler runs on the posting thread, before the failure unwinds

`onBusMessage` closing the gate is only useful if GStreamer calls it synchronously from
`gst_element_post_message`, which is what `gst_bus_post` does when a sync handler is installed.
This is documented GStreamer behaviour and is relied on hard. If it turns out messages are
queued instead, `srtsink`'s `GST_FLOW_ERROR` will reach `mpegtsmux` through the queue on every
disconnect and the capture chain will wedge. Symptom: a `gst: pipeline-fatal:` error naming `mux`
on the first reconnect, every time. There is no cheap fix; escalate, because it changes the shape
of the reconnect design.

### 4.3 A pad probe returning `GST_PAD_PROBE_DROP` from a `_BLOCK` probe yields `GST_FLOW_OK`

`gst_pad_push_data`'s `probe_stopped:` label converts `GST_FLOW_CUSTOM_SUCCESS` to
`GST_FLOW_OK`, so a dropped buffer looks like a delivered one to the caller. This is what makes
the gate safe on an unlinked pad. If it is wrong, `Start` will immediately produce
`not-linked` errors from `srtq`.

### 4.4 The probe mask deliberately excludes `EVENT_DOWNSTREAM`

`gateProbeMask` is `BLOCK | BUFFER | BUFFER_LIST` (2 | 16 | 32 = 50), **not**
`BLOCK_DOWNSTREAM` (114). The reasoning is in the file comment: `push_sticky()` marks a sticky
event as received when the push returns `GST_FLOW_OK`, and a dropped probe returns
`GST_FLOW_OK` — so gating events would let `STREAM_START` / `CAPS` / `SEGMENT` be recorded as
delivered to a sink that never saw them. This is a deliberate deviation from the work-package
brief, which said `GST_PAD_PROBE_TYPE_BLOCK_DOWNSTREAM`. If you change it back, expect
"data flow before segment event" from `srtsink` on the second and subsequent connects.

### 4.5 `ForceKeyUnit` builds the event by hand rather than importing `pkg/gstvideo`

`newForceKeyUnitEvent` reimplements `gst_video_event_new_upstream_force_key_unit`: a
`GST_EVENT_CUSTOM_UPSTREAM` carrying a `GstStructure` named `GstForceKeyUnit` with `all-headers`
and `count`, and no `running-time` field (the parser substitutes `GST_CLOCK_TIME_NONE`, which
means "as soon as possible"). This keeps the package's pkg-config requirements down to
`gstreamer-1.0` and `gobject-2.0`.

If forced keyframes demonstrably do not happen, swap it for the real thing:

```go
import gstvideo "github.com/go-gst/go-gst/pkg/gstvideo"

event := gstvideo.VideoEventNewUpstreamForceKeyUnit(gogst.ClockTimeNone, true, 0)
```

and add `gstreamer-video-1.0` to the pkg-config check in §1. `libgstvideo-1.0-0.dll` is already
required by the `videoconvertscale` and `videoparsersbad` plugins, so the DLL allowlist does not
change.

### 4.6 `gobject.ObjectInstance.PropertyType` is reachable through a type assertion

`hasProperty` asserts to `interface{ PropertyType(string) gobject.Type }` because go-glib
implements the method on `*ObjectInstance` but does not list it in the `gobject.Object`
interface. Every gst element embeds `ObjectInstance`, so the assertion should hold. If it does
not, `hasProperty` returns false for everything, and the symptom is `Start` failing with
`element has no location property`. Replace the body with a check on
`obj.ObjectProperty(name) != gobject.InvalidValue`.

### 4.7 `gst_util_set_object_arg` is used for enum and integer properties

Enum properties belonging to plugins (`srtsink`'s `mode` and `pbkeylen`, the encoder's `rc-mode`)
have GTypes go-gst has no binding for, so they cannot be set with a typed `GValue`.
`gst_util_set_object_arg` deserialises a string into whatever the property's GType is, which
handles enum nicks (`"caller"`, `"cbr"`), `gint` and `guint` uniformly. It is silent when the
property does not exist, which is why every call is preceded by a `hasProperty` check.

Nicks assumed: `mode=caller`; `pbkeylen=16` / `pbkeylen=32` (the `GstSRTKeyLength` enum's values
*are* 16/24/32, so both nick and integer deserialisation give the same answer); `rc-mode=cbr`.

### 4.8 The passphrase is set with `g_object_set_property`, never in the URI

Non-negotiable. A URI is percent-encoded, is printed by GStreamer's own debug output, and appears
inside the element's error messages. Nothing in this file formats a `SinkOpts` as a whole; the two
helpers `endpointForLog` and `encryptionForLog` exist so that no future edit is tempted to.
`encryptionForLog` reports `aes-128` / `aes-256` / `none` and nothing else.

### 4.9 `wasapi2src` publishes `device.api = "wasapi2"`

`ListInputDevices` filters on this string because `GstDeviceMonitor` has no API for restricting
which providers it uses. If the filter is wrong the dropdown comes back empty; the fastest check
is to comment out the filter and log `structureFieldNames(props)` for every device.

### 4.10 The queue re-arm destroys every sticky event, and puts them back by hand

**This is the highest-risk unverified thing in the file. Verify it first at Gate B.**

`rearmQueueLocked` relies on `gst_queue_src_activate_mode` resetting `srcresult` to
`GST_FLOW_OK`, flushing the queued data and restarting the loop task — which is what gstqueue.c
does. It runs only when `gst_pad_get_last_flow_return` on `srtq:src` is not `GST_FLOW_OK`, so on
the healthy path it is a single read and a return.

What was missed originally, and is now handled: **`gst_pad_set_active(pad, FALSE)` destroys all of
the pad's sticky events.** `gst_pad_activate_mode` reaches `post_activate()` with `new_mode ==
GST_PAD_MODE_NONE`, and that branch calls `remove_events (pad)`, which empties the pad's entire
sticky event list — `STREAM_START`, `CAPS`, `SEGMENT`, the lot. That is strictly worse than the
`FLUSH_START`/`FLUSH_STOP` pair the original comment said it had rejected in order to avoid exactly
this: `FLUSH_STOP` only removes `SEGMENT`, `EOS` and `STREAM_GROUP_DONE`.

Nothing puts them back. `mpegtsmux` sends `STREAM_START`, `CAPS` and `SEGMENT` once, at the start of
the stream; the queue forwards them once; `gst_pad_link`'s
`events_foreach (srcpad, mark_event_not_received, NULL)` re-marks the src pad's sticky list so
`check_sticky()` will re-push it to the new peer — but the list is now empty, so it marks nothing.
The fresh `srtsink` then receives buffers with no segment and `gstbasesink` logs

```
Received buffer without a new-segment. Assuming timestamps start from 0
```

Timestamps restarting at zero downstream of this package is the measured fault the whole design
exists to prevent.

**It is not a rare path.** `rearmQueueLocked` runs whenever `srtq:src`'s last flow return is not
`GST_FLOW_OK`, and on a genuine mid-match disconnect the in-flight buffer is inside `srtsink`'s
`render()` when the gate closes, so the last flow return *is* an error. Every real reconnect takes
it.

There is no re-arm in GStreamer that leaves the sticky events alone — deactivation clears all of
them, a flush pair clears `SEGMENT`, and taking the queue element to `READY` does both and more. So
the events are snapshotted before the deactivation and stored back afterwards:

- `stickyEventsOf(pad)` reads `STREAM_START`, `CAPS`, `SEGMENT`, `STREAM_COLLECTION` and `TAG` with
  `gst_pad_get_sticky_event(type, idx)`. go-gst v0.0.2 does **not** bind
  `gst_pad_sticky_events_foreach` (it takes a C callback), so the read is by type and index. The
  snapshot is taken from `srtq:src`; anything absent there is taken from `srtq:sink`, whose own
  sticky list is untouched by the deactivation.
- `restoreStickyEvents(pad, saved)` stores them back with `gst_pad_store_sticky_event`, which
  inserts with `received = FALSE` and sets `GST_PAD_FLAG_PENDING_EVENTS`, so `check_sticky()` pushes
  them ahead of the first buffer the moment the gate opens. That is the ordinary delivery path, not
  a special case. `gst_pad_link` on the new sink also re-marks them, so the two mechanisms agree.

**Gate B reproduction.** Confirm the destruction and the repair separately, because they fail
differently. Run with `GST_DEBUG=GST_PADS:5,basesink:4,queue:5` and watch for `remove_events` on
`srtq:src` and for the `new-segment` warning from `srtsink`:

```
set GST_DEBUG=GST_PADS:5,basesink:4,queue:5
set GST_DEBUG_FILE=rearm.log
gst-launch-1.0 -v ^
  audiotestsrc is-live=true ! audioconvert ! avenc_aac ! aacparse ^
    ! mpegtsmux name=mux alignment=7 ^
    ! queue name=srtq leaky=downstream max-size-buffers=4000 ^
    ! srtsink name=srtout uri=srt://127.0.0.1:9001 mode=caller sync=false async=false auto-reconnect=false
```

with `cmd\mockm2lx` listening on 9001, then kill the listener mid-stream, restart it, and drive a
`ReplaceSink` from the application rather than from `gst-launch` (the re-arm lives in our code, not
in a launch line). The two things to grep the log for:

1. `srtq:src` `removing events` / `remove_events` right after the deactivation — proves the
   destruction is real on this GStreamer.
2. `Received buffer without a new-segment` from `srtout-N` after the reconnect — if this appears,
   the restore did **not** work and the fix is incomplete. Escalate; do not ship.

If `gst_pad_store_sticky_event` turns out not to set `PENDING_EVENTS` on this version, the fallback
is to push the saved events with `gst_pad_push_event` on `srtq:src` **while the gate is closed and
after the new sink is linked**, which forces them through `check_sticky` explicitly.

### 4.11 `os.Setenv(name, "")` — a claim that was tested and is FALSE

Recorded because it was asserted confidently in review and acting on it would have been a change
made for a wrong reason.

**The claim.** Go's `os.Setenv` calls `SetEnvironmentVariableW`; Windows cannot represent an
empty-valued variable so it deletes the name; `GetEnvironmentVariableW` then returns
`ERROR_ENVVAR_NOT_FOUND`; GLib's `g_getenv` sees it as *unset* rather than *empty*; and
`gstregistry.c` therefore falls through to `GST_PLUGIN_SYSTEM_PATH` and the compiled-in system
directories — silently loading a foreign GStreamer.

**The measurement**, on this machine (Windows 11 Pro 26200, Go 1.24.5), parent and child process:

```
GOOS = windows  Go = go1.24.5
PARENT LookupEnv -> value="" present=true
PARENT Environ entry -> "WSLCOMMS_EMPTY_ENV_PROBE="
CHILD  LookupEnv -> value="" present=true
CHILD  Environ entry -> "WSLCOMMS_EMPTY_ENV_PROBE="
PS     GetEnvironmentVariable -> $null
```

The variable survives with an empty value, is present in the environment block as the literal entry
`NAME=`, and is inherited by a child process. `GetEnvironmentVariableW` did not report
`ERROR_ENVVAR_NOT_FOUND` — if it had, Go's `LookupEnv` would have returned `present=false`.
`TestEmptyEnvVarSurvivesOnWindows` in `gst_stub_test.go` pins this at Gate A.

The folklore is real but belongs to the wrappers, not to Win32: `cmd.exe`'s `set FOO=` passes `NULL`
(which *does* delete), the CRT's `_putenv("FOO=")` deletes, and .NET's `GetEnvironmentVariable`
normalises an empty value to `null` — which is the `$null` on the last line above. Go calls the API
directly and none of them apply.

**`doInit` nevertheless sets a path rather than `""`.** Not because of the disproven claim, but
because of the one link that cannot be tested without GLib: whether `g_getenv` maps an empty value
to `""` or to `NULL`. Reading `gutils.c` it returns `""`, and `gstregistry.c` splits `""` into zero
directories and scans nothing, which is the wanted behaviour — but that is a reading, not a
measurement. A non-empty path is correct under **both** behaviours, costs one redundant scan of a
directory that is already `GST_PLUGIN_PATH_1_0`, and cannot be lost. `GST_PLUGIN_SYSTEM_PATH`
(unversioned) is set for the same reason: it is the fallback `gstregistry.c` consults when the
versioned name is absent.

### 4.12 The bus sync handler is never detached — `SetSyncHandler(nil)` can kill the process

`teardownLocked` sets `busSilenced` and leaves the handler attached. Do not "tidy" this back into
`p.bus.SetSyncHandler(nil)`.

`gst_bus_post` reads the handler and its `user_data` under `GST_OBJECT_LOCK`, drops the lock, and
only then calls it. `gst_bus_set_sync_handler(bus, NULL, ...)` runs the `GDestroyNotify` go-gst
installed alongside the closure, which is `destroyUserdata` →
`userdata.Delete`. A streaming thread that had already read the pointer then enters
`_gogst_gst1_BusSyncHandler` in `go-gst/pkg/gst/bus_manual_export.go`:

```go
v := userdata.Load(unsafe.Pointer(carg3))
if v == nil {
    panic(`callback not found`)
}
fn = v.(BusSyncHandler)
```

A Go panic cannot unwind through a C frame, so the process dies. It is worse than a panic, in fact:
go-glib's `userdata` allocator returns the freed C pointer to a free list (`returnPointer`) and
hands the same address out to the next `Register`, so a late call can reach somebody else's closure
— or fail the type assertion on the following line.

The window is narrow. It is also at `Stop`, which is the end of every match and every mid-match
capture-device change, and the failure is the application vanishing during a live broadcast.

The justification the original code gave for detaching — that leaving the handler attached would
deadlock `deliver` against `Stop` — was wrong. `Stop` does not hold `errMu` while `teardownLocked`
runs; it takes `errMu` only afterwards, to close the channels. A re-entrant `deliver` on the same
goroutine would take `errMu.RLock()` and return. `errsClosed` already makes a late delivery safe.

Cost of not detaching: one `userdata` registration per pipeline, released when the `GstBus` is
finalised. A match with several device changes leaks a handful of map entries.

### 4.13 The timeout constants do not bound a state change, and cannot

`pipelineStartTimeout`, `sinkStateChangeTimeout` and `elementShutdownTimeout` used to be documented
as bounding a hang. They do not. go-gst's `BlockSetState` (`element_manual.go`) is:

```go
ret := el.SetState(state)
if ret == StateChangeAsync {
    _, _, ret = el.GetState(timeout)
}
return ret
```

`gst_element_set_state` takes no timeout, holds the element's `GST_STATE_LOCK`, and runs every
`change_state` function downwards through the bin synchronously on the calling goroutine. If
`wasapi2src` blocks inside `IAudioClient::Initialize` on a wedged WASAPI endpoint, `SetState` never
returns, the timeout is never consulted, and `Start` hangs forever holding `p.mu`. Likewise
`srtsink` with `async=false` performs its SRT connect inside its `READY→PAUSED` transition, which
runs inside `SyncStateWithParent()` — a call that takes no timeout argument at all. The only thing
bounding a connect is libsrt's own `SRTO_CONNTIMEO`, 3 s by default and not overridden by srtsink.

**A watchdog was considered and rejected**, and the comments were corrected instead. There is
nothing to cancel: `gst_element_set_state` offers no abort, a goroutine inside C cannot be
preempted, and it holds the pipeline's state lock — so the obvious "recovery" (`teardownLocked`,
which sets the pipeline to `NULL`) would block on that same lock the instant it ran. A watchdog
would convert one wedged goroutine into a wedged goroutine, plus a leaked one, plus an error return
that lies.

What *is* implemented is `stateChangeWatchdog`, which logs every `watchdogInterval` (3 s) that a
state change has still not returned, and stops. It recovers nothing. It buys the difference between
"the application froze" and a log line naming the element and the transition. The real mitigation is
architectural and belongs to WP-8 — see section 7.

### 4.14 A pipeline with no sink element can reach PLAYING

`Start` deliberately installs no sink. Nothing in `gst_bin_change_state_func` requires a sink, and
the pipeline's clock is pinned explicitly rather than provided by one, so this should be fine —
and it is the single most important structural property of the design. If the pipeline refuses to
go to PLAYING with no sink, the fallback is to install a `fakesink sync=false` at `Start` and have
the first `ReplaceSink` swap it out. That is a change inside `gst_cgo.go`; do not change `gst.go`.

---

### 4.15 The audio branch has no capsfilter above `audioresample`

There is deliberately nothing between `wasapi2src` and `audioconvert`. A capsfilter pinning
`rate=48000,channels=2` used to sit there, upstream of the resampler whose entire job is to produce
exactly that — where it could not convert anything and could only refuse.

`wasapi2src` in shared mode can only ever produce its endpoint's mix format. Dante Virtual
Soundcard is commonly configured at 44.1 or 96 kHz, and a DVS endpoint that is not at 48 k would
fail caps negotiation inside `Start`, twenty minutes before kick-off, with an error naming neither
the sample rate nor the device. `audioconvert ! audioresample` handle whatever the endpoint gives
us; the capsfilter that matters is the one below them, pinning `S16LE,48000,2ch` into `mfaacenc`.

If a Gate B run ever shows the encoder receiving something unexpected, that lower capsfilter is the
one to look at — not the absence of the upper one.

### 4.16 `videoscale` in the slate branch

`assets/slate.png` is 1920x1080 and `CONTRACT.md` says so, but the artwork is replaced every season
by somebody who will not read this file, and without `videoscale` a 1920x1200 export fails caps
negotiation at `Start` with no diagnostic naming the size. `videoconvertscale` is already a required
plugin for `videoconvert`, so the element costs nothing and does nothing at all when the slate is
already correct.

`add-borders` is set to `true` from `Start` with a `hasProperty` guard rather than in the parse
string, because `gst_parse_launch` treats an unknown property as a hard error and a commentary
position that will not start because a scaler property was renamed is worse than a slate scaled at
the element's default. The value letterboxes artwork whose aspect ratio is not 16:9 rather than
stretching it. `videoscale`'s own default is already `true`; this pins it against the default
changing.

---

## 5. Deliberate deviations from the work-package brief

Both are argued in full in the file comment at the top of `gst_cgo.go`.

1. **The pad probe drops; it does not hold the pad blocked.** The brief said "block srtq's SRC
   pad with `GST_PAD_PROBE_TYPE_BLOCK_DOWNSTREAM`" and also said "you MUST NOT block indefinitely
   inside the pad probe callback ... do the slow work on the calling goroutine". Those two cannot
   both be satisfied by the textbook idiom, because a GStreamer blocking probe holds the pad
   blocked only for the duration of its callback, and the slow work here is a multi-second SRT
   handshake. The resolution is a *gate*: a probe that drops buffers while a flag is set. Dropping
   during an outage is what `leaky=downstream` already does (specification section 6.2), so no
   behaviour is lost.

2. **There are two probes, not one, and the mask is narrower.** A probe on `srtq`'s **sink** pad
   was added so that `mpegtsmux` never enters `gst_queue_chain` while the queue is unhealthy —
   without it, one `srtsink` write failure propagates through the queue into the muxer and wedges
   the capture chain that the whole design exists to protect. The mask excludes
   `EVENT_DOWNSTREAM`; see §4.4.

Also worth the coordinator's attention, in `REPORT` terms rather than as a deviation: the
residual race documented at the end of the file comment. The window is microseconds wide against
a ~4.6 ms buffer period, it is loud rather than silent when it fires, and closing it entirely
would need a pipeline-shape change. **WP-9's match-length soak should specifically watch for a
bus error whose source is `mux`.**

---

## 6. Tests

### 6.1 What runs at Gate A today

`gst_stub_test.go` (`//go:build !cgo`) holds three kinds of test. Know which you are reading.

1. **Behavioural tests of the stub twin.** Real tests of real code — `Start` installs no sink,
   `ReplaceSink` fails synchronously, `RemoveSink` detaches and is idempotent, `Stop` closes
   `Errors()`. These are what let WP-3b, WP-5b and WP-8 be built and demonstrated.

2. **One platform measurement**, `TestEmptyEnvVarSurvivesOnWindows`. See §4.11. It tests Windows,
   not us, and it exists because a comment in `doInit` cites it.

3. **Source-level guards on `gst_cgo.go`.** These parse `gst_cgo.go` with `go/parser` and assert
   structural properties of the *source*. They are **not** behavioural tests and they are not a
   substitute for one — `gst_cgo.go` carries `//go:build cgo` and not one line of it is linked into
   the Gate A test binary.

   They earn their place for two reasons. First, `TestCgoSourceParses` is the only check anywhere
   that `gst_cgo.go` is even valid Go; without it a missing brace sits in the repository until
   somebody opens Gate B. Second, and more usefully, several of the defects this file has already
   had were structural and invisible to a reviewer reading prose:

   | Guard | The defect it would have caught |
   |---|---|
   | `TestCgoPipelineImplementsEveryPipelineMethod` | `RemoveSink` added to the interface and to the stub, missing from `*cgoPipeline`. `var _ Pipeline = (*cgoPipeline)(nil)` cannot catch this until Gate B. |
   | `TestReplaceSinkClearsTheRouteBeforeOpeningTheGate` | The route cleared by `defer` instead of before the gate opened — a swallowed sink error and a false green. |
   | `TestReplaceSinkRechecksFatalBeforeSucceeding` | A `nil` return alongside an asynchronous pipeline-fatal error. |
   | `TestRearmQueueRestoresStickyEvents` | §4.10, the sticky events destroyed by the re-arm. |
   | `TestTeardownDoesNotDetachTheBusSyncHandler` | §4.12, the process-killing `SetSyncHandler(nil)`. |
   | `TestBusHandlerDoesNotLogOnTheStreamingThread` | `log.Printf` from a bus callback under a warning storm. |
   | `TestPipelineDescriptionHasNoCapsfilterAboveTheResampler` | A rate capsfilter upstream of `audioresample`, failing Start on a 44.1 or 96 kHz DVS endpoint. |
   | `TestPipelineDescriptionScalesTheSlate` | No `videoscale`, so next season's artwork must be exactly 1920x1080. |
   | `TestInitPointsEveryPluginPathAtTheBundle` | A plugin path variable that stops isolating the bundle. |

   Every one of them was verified to FAIL with the fix reverted, in a scratchpad copy of the
   package. A guard that passes either way is worse than no guard, so if you add one, prove it the
   same way.

   If a guard starts failing because the code was legitimately restructured, **read the reason in
   its doc comment before changing it** — each of them is a bug that actually happened.

### 6.2 What still needs Gate B

`internal/gst/gst_cgo_test.go` was NOT created. WP-3a's allocated paths are `gst_cgo.go`,
`gst_stub_test.go` and this file, and rule R1 says not to create anything outside them for any
reason. The test file below is ready to paste; the coordinator should drop it in if the allocation
is extended.

It only covers the pure-Go logic — string building, option validation and encoder-property
selection. Everything else in `gst_cgo.go` needs a GStreamer, which is Gate B, and a real WASAPI
endpoint and SRT listener, which is Gate C. It carries `//go:build cgo`, so it does not run at
Gate A and cannot affect `CGO_ENABLED=0 go test ./...`.

Note that the audio-branch assertions in it are stale in one respect: the description no longer
contains a capsfilter directly after `wasapi2src` (§4.11 of the review, finding 7), so a test
asserting `audio/x-raw,rate=48000` appears twice would now fail correctly.

```go
//go:build cgo

package gst

import (
	"strings"
	"testing"
)

// TestPipelineDescription checks the parse-launch string against the decisions
// in specification section 5 that are load-bearing, and against the two things
// that must NOT be in it: a sink, and any user-supplied string.
func TestPipelineDescription(t *testing.T) {
	desc := pipelineDescription("mfh264enc", 128000)

	mustContain := []struct{ what, why string }{
		{"alignment=7", "7 x 188 = 1316 bytes, exactly one SRT payload, so nothing fragments"},
		{"pcr-interval=3600", "specification section 5"},
		{"leaky=downstream", "outage output is dropped, not back-pressured into live capture"},
		{"max-size-buffers=4000", "specification section 5"},
		{"is-live=true", "without it the slate branch will not pace correctly"},
		{"config-interval=-1", "SPS/PPS before every IDR so M2L-X can re-lock mid-stream"},
		{"format=NV12", "specification section 5"},
		{"framerate=50/1", "1080p50"},
		{"mfaacenc bitrate=128000", "AAC-LC is pinned; M2L-X silently drops MP2 and AC-3"},
		{"stream-format=adts", "specification section 5"},
		{"name=" + nameSRTQueue, "ReplaceSink finds the queue by this name"},
		{"name=" + nameVideoEncod, "ForceKeyUnit finds the encoder by this name"},
		{"name=" + nameAudioSrc, "Start sets the device property by this name"},
		{"name=" + nameSlateSrc, "Start sets the location property by this name"},
	}
	for _, tc := range mustContain {
		if !strings.Contains(desc, tc.what) {
			t.Errorf("description is missing %q (%s)\n%s", tc.what, tc.why, desc)
		}
	}

	mustNotContain := []struct{ what, why string }{
		{"srtsink", "Start installs no sink; the first ReplaceSink installs the first one"},
		{"start-time-selection", "start-time-selection=first reproduces the measured backwards-DTS bug"},
		{"location=", "the slate path is set with g_object_set, not through the parser"},
		{"device=", "the endpoint GUID is set with g_object_set, not through the parser"},
		{"passphrase", "the passphrase never goes near a parse string"},
	}
	for _, tc := range mustNotContain {
		if strings.Contains(desc, tc.what) {
			t.Errorf("description must not contain %q (%s)\n%s", tc.what, tc.why, desc)
		}
	}

	if got := strings.Count(desc, "! "+nameMux+"."); got != 2 {
		t.Errorf("expected exactly two branches feeding %s, got %d", nameMux, got)
	}
}

// TestPipelineDescriptionEncoderIsSubstituted proves the encoder name really is
// resolved at runtime rather than hardcoded (specification open question 3).
func TestPipelineDescriptionEncoderIsSubstituted(t *testing.T) {
	for _, enc := range []string{"mfh264enc", "qsvh264enc", "d3d11h264enc"} {
		desc := pipelineDescription(enc, 128000)
		if !strings.Contains(desc, enc+" name="+nameVideoEncod) {
			t.Errorf("encoder %q was not substituted into the description", enc)
		}
	}
}

// TestSRTURI covers the endpoint formatting, including the IPv6 literal a
// facility engineer might type into the settings screen.
func TestSRTURI(t *testing.T) {
	tests := []struct {
		name string
		host string
		port int
		want string
	}{
		{"hostname", "m2lx.example.com", 9000, "srt://m2lx.example.com:9000"},
		{"ipv4", "10.1.2.3", 4201, "srt://10.1.2.3:4201"},
		{"ipv6", "2001:db8::1", 4201, "srt://[2001:db8::1]:4201"},
		{"ipv6 already bracketed", "[2001:db8::1]", 4201, "srt://[2001:db8::1]:4201"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := srtURI(tc.host, tc.port); got != tc.want {
				t.Errorf("srtURI(%q, %d) = %q, want %q", tc.host, tc.port, got, tc.want)
			}
		})
	}
}

// TestEncryptionForLog is a guard on the one thing in this package that must
// never regress: nothing derived from the passphrase may be loggable.
func TestEncryptionForLog(t *testing.T) {
	tests := []struct {
		name string
		opts SinkOpts
		want string
	}{
		{"none", SinkOpts{}, "none"},
		{"aes128", SinkOpts{Passphrase: "hunter2hunter2hu", PBKeyLen: 16}, "aes-128"},
		{"aes256", SinkOpts{Passphrase: "hunter2hunter2hu", PBKeyLen: 32}, "aes-256"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := encryptionForLog(tc.opts)
			if got != tc.want {
				t.Errorf("encryptionForLog = %q, want %q", got, tc.want)
			}
			if tc.opts.Passphrase != "" && strings.Contains(got, tc.opts.Passphrase) {
				t.Fatalf("encryptionForLog leaked the passphrase")
			}
		})
	}
}

// TestEndpointForLogNeverCarriesTheSecret is the same guard from the other side.
func TestEndpointForLogNeverCarriesTheSecret(t *testing.T) {
	opts := SinkOpts{Host: "m2lx.example.com", Port: 9000, Passphrase: "correcthorsebatt", PBKeyLen: 32}
	if got := endpointForLog(opts); strings.Contains(got, opts.Passphrase) {
		t.Fatalf("endpointForLog leaked the passphrase: %q", got)
	}
}

// TestIsSinkSourced covers the classification that decides whether a bus error
// is an ordinary reconnect (retry) or pipeline-fatal (rebuild).
func TestIsSinkSourced(t *testing.T) {
	tests := []struct {
		source string
		want   bool
	}{
		{"srtout-1", true},
		{"srtout-27", true},
		{srtSinkNamePrefix + "1", true},
		{nameSRTQueue, true},
		{nameMux, false},
		{nameVideoEncod, false},
		{nameAudioSrc, false},
		{"pipeline", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.source, func(t *testing.T) {
			if got := isSinkSourced(tc.source); got != tc.want {
				t.Errorf("isSinkSourced(%q) = %v, want %v", tc.source, got, tc.want)
			}
		})
	}
}

// TestPreferenceIndex covers the equal-rank tie-break.
func TestPreferenceIndex(t *testing.T) {
	if preferenceIndex("mfh264enc") != 0 {
		t.Errorf("mfh264enc should win an equal-rank tie-break")
	}
	if preferenceIndex("somethingelse") != len(h264EncoderPreference) {
		t.Errorf("an unknown encoder should sort last")
	}
	if preferenceIndex("mfh264enc") >= preferenceIndex("amfh264enc") {
		t.Errorf("preference order is not monotonic")
	}
}

// TestH264EncoderDenylist pins the licence constraint: x264enc is GPL and the
// deliverable is commercial.
func TestH264EncoderDenylist(t *testing.T) {
	if !h264EncoderDenylist["x264enc"] {
		t.Fatal("x264enc must never be selectable: it is GPL")
	}
	for _, name := range h264EncoderFallbacks {
		if h264EncoderDenylist[name] {
			t.Errorf("%q is both a fallback and denied", name)
		}
	}
}

// TestGateProbeMaskExcludesDownstreamEvents pins the reasoning in section 4.4
// of BUILD-NOTES.md, because someone will eventually "fix" this back to
// BLOCK_DOWNSTREAM.
func TestGateProbeMaskExcludesDownstreamEvents(t *testing.T) {
	if gateProbeMask&PadProbeTypeEventDownstreamForTest() != 0 {
		t.Fatal("gateProbeMask must not cover downstream events: dropping one marks a " +
			"sticky event as received by a sink that never saw it")
	}
	if gateProbeMask&PadProbeTypeBlockForTest() == 0 {
		t.Fatal("gateProbeMask must keep BLOCK: gst_pad_push_data runs blocking probes " +
			"before it re-pushes pending sticky events")
	}
}
```

The last test needs two one-line helpers so the test file does not import go-gst symbols under
different names; add them next to `gateProbeMask` in `gst_cgo.go` if the test file is adopted:

```go
func PadProbeTypeEventDownstreamForTest() gogst.PadProbeType { return gogst.PadProbeTypeEventDownstream }
func PadProbeTypeBlockForTest() gogst.PadProbeType           { return gogst.PadProbeTypeBlock }
```

(or, better, just write `gogst.PadProbeTypeEventDownstream` directly in the test file and import
`gogst "github.com/go-gst/go-gst/pkg/gst"` there — the helpers exist only to keep new exported
symbols out of the package, since `gst_stub.go` would not have them and the two builds must expose
the same API.)

---

## 7. What the CALLER must not do — this is a WP-8 constraint, not a suggestion

### 7.1 No `Pipeline` method may be called from the Wails message loop

**`Stop` can block for the full length of an SRT connect attempt.** `Stop` takes `p.mu`, and
`ReplaceSink` holds `p.mu` across the handshake — `SyncStateWithParent` plus `BlockSetState`, which
is where libsrt's `SRTO_CONNTIMEO` (3 s by default, not overridden) is spent, plus a possible
`BlockSetState(NULL)` on the abandoned sink. A user pressing Stop while a reconnect attempt is in
flight waits for that attempt to finish. Called from the goroutine that services the Windows message
loop, the whole window stops repainting and Windows marks it "Not Responding" — during a match.

The same applies to `Start` and `ReplaceSink`, and worse: per §4.13 neither has an enforceable upper
bound at all. A wedged WASAPI endpoint hangs `Start` for the life of the process.

So: **WP-8 must call `Start`, `ReplaceSink`, `RemoveSink` and `Stop` from a dedicated goroutine and
report progress back through events.** A bound Wails method must return promptly and never wait on
one of these. `internal/sender` already owns its own goroutine, which covers `ReplaceSink`,
`RemoveSink` and `ForceKeyUnit`; the two that need attention are the ones the UI drives directly,
`Start` and `Stop`.

### 7.2 `Stop` must always be called

`New` starts one goroutine (the warning logger, §4.12's sibling — see `logWarnings`). `Stop` ends
it. A `Pipeline` created and abandoned leaks it. `Stop` is idempotent, so calling it on every path
out costs nothing.

---

## 8. Gate B assertions to add once GStreamer is present

These cannot be written at Gate A because they need a live registry. Add them to the smoke test in
§1 and record the results.

### 8.1 Every loaded plugin comes from the bundle

The single check that proves §4.11's isolation actually worked. `missingElements()` does not cover
it: that detects factories which are ABSENT, and the failure mode here is a foreign install's copy
of the same factory being loaded *instead of* ours — same name, different binary, possibly a
different licence.

```go
// Gate B only. After Init(appDir) returns nil.
pluginDir := filepath.Join(appDir, "gst", "lib", "gstreamer-1.0")
for _, plugin := range gogst.RegistryGet().GetPluginList() {
    file := plugin.GetFilename()
    if file == "" {
        continue // statically registered, e.g. the core elements
    }
    abs, _ := filepath.Abs(file)
    if !strings.HasPrefix(strings.ToLower(abs), strings.ToLower(pluginDir)) {
        t.Errorf("plugin %s was loaded from %s, which is outside the bundle at %s",
            plugin.GetName(), abs, pluginDir)
    }
}
```

Run it on a machine that **also** has a system-wide GStreamer installed, or the test proves nothing.
Install the 1.28.5 runtime MSI system-wide, set `GST_PLUGIN_SYSTEM_PATH` machine-wide to its plugin
directory, and confirm the assertion still passes.

### 8.2 The sticky events survive a reconnect

§4.10, and it is the one to do first. The reproduction and the two log lines to grep for are there.

### 8.3 The H.264 encoder actually chosen

Record the `gst: H.264 encoder resolved by rank: ... (rank N)` line. That is the answer to
specification open question 3 and it belongs in `docs/test-results.md`, not in someone's memory.

---

## 9. WP-9 soak items

### 9.1 `savedBase` never resets — running time is process uptime, and MPEG-TS PTS wraps

**Documented deliberately rather than coded around. Read this before "fixing" it.**

`savedBase` is sampled once, the first time any pipeline in the process is started, and is then
never reset — not by `Stop`, not by a later `Start`, not by a capture-device change. That is the
whole point: it is the secondary defence against the measured backwards-DTS bug, and every pipeline
this process builds gets the same base time so running time continues rather than restarting at
zero.

The consequence is that **running time equals process uptime, not stream duration.** MPEG-TS PTS and
PCR are 33-bit values at 90 kHz, so they wrap at

```
2^33 / 90000 = 95443.7 s = 26 h 30 m 44 s
```

A commentary PC left powered on between fixtures — which is the normal state of a rack machine —
that does `Stop`, `New`, `Start` two days later begins its second match at a running time of roughly
48 hours. That is past the wrap, and the first minutes of the match sit right on a wrap boundary.

**What is not known:** whether M2L-X's relay tolerates a PTS wrap. It forwards our timestamps
verbatim, which is why the original bug was never absorbed downstream, and "non-monotonic DTS
downstream" is precisely the symptom class this file exists to prevent. A wrap is legal MPEG-TS and
a correct demuxer handles it; whether M2L-X's does is unmeasured.

**Do not fix this by resetting `savedBase`.** That reintroduces the measured fault, which is
certain, to avoid one that is speculative. If it does turn out to matter, the fix is to seed
`savedBase` from a value that is small at the start of each match while still never moving backwards
within a match — which is a design change, not a one-line edit, and it needs the measurement first.

**Soak test.** Leave a machine powered on and the application running for 30 hours with an active
SRT session, and watch for a discontinuity at the 26 h 30 m mark: `ffprobe -show_packets` on M2L-X's
output, or the relay's own logs. A second run should `Stop`/`Start` at hour 27 to exercise the
rebuild across the wrap.

**Operational rule until that measurement exists**, and it belongs in the operator documentation
rather than only here:

> **Restart the WSL Commentary application before every match.** Do not leave it running between
> fixtures. Closing and reopening the application is sufficient; the machine does not need to be
> rebooted.

That rule costs the operator ten seconds and removes the entire question, which is why it is the
answer rather than a code change.

### 9.2 A bus error whose source is `mux`

Already noted in §5: the residual race at the end of the file comment in `gst_cgo.go`. The window is
microseconds against a ~4.6 ms buffer period, it is loud rather than silent when it fires, and
closing it would need a pipeline-shape change. If the soak produces a `gst: pipeline-fatal:` error
naming `mux`, that is it, and it needs escalating rather than retrying.
