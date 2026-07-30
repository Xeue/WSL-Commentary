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
$ CGO_ENABLED=0 go test ./internal/gst/...
ok      wslcomms/internal/gst
```

That proves only that the frozen stub twin is undisturbed. It proves nothing about the file you
are about to build, because `//go:build cgo` excludes it entirely.

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

### 4.10 `gst_pad_set_active(FALSE)` then `(TRUE)` re-arms a stalled `queue`

`rearmQueueLocked` relies on `gst_queue_src_activate_mode` resetting `srcresult` to
`GST_FLOW_OK`, flushing the queued data and restarting the loop task — which is what gstqueue.c
does. It runs only when `gst_pad_get_last_flow_return` on `srtq:src` is not `GST_FLOW_OK`, so on
the healthy path it is a single read and a return. The alternative (a `FLUSH_START`/`FLUSH_STOP`
pair) was rejected because `FLUSH_STOP` removes the sticky `SEGMENT` event from the pad it is sent
to and nothing re-pushes it.

### 4.11 A pipeline with no sink element can reach PLAYING

`Start` deliberately installs no sink. Nothing in `gst_bin_change_state_func` requires a sink, and
the pipeline's clock is pinned explicitly rather than provided by one, so this should be fine —
and it is the single most important structural property of the design. If the pipeline refuses to
go to PLAYING with no sink, the fallback is to install a `fakesink sync=false` at `Start` and have
the first `ReplaceSink` swap it out. That is a change inside `gst_cgo.go`; do not change `gst.go`.

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

`internal/gst/gst_cgo_test.go` was NOT created. WP-3a's allocated paths are `gst_cgo.go` and this
file, and rule R1 says not to create anything outside them for any reason. The test file below is
ready to paste; the coordinator should drop it in if the allocation is extended.

It only covers the pure-Go logic — string building, option validation and encoder-property
selection. Everything else in `gst_cgo.go` needs a GStreamer, which is Gate B, and a real WASAPI
endpoint and SRT listener, which is Gate C. It carries `//go:build cgo`, so it does not run at
Gate A and cannot affect `CGO_ENABLED=0 go test ./...`.

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
