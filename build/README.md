# Building the installer — the Gate B runbook

**Owner:** WP-6. **Implements:** specification section 11.

This is the whole path from a bare Windows 11 x64 machine to `wslcomms-setup.exe`.
Follow it top to bottom. It assumes nothing is installed except Windows.

> ## Read this before you trust anything below
>
> Everything in `build/` was written on a machine with **no MinGW gcc, no
> GStreamer, no Wails CLI and no Inno Setup** — Gate B was closed. That means:
>
> - `bundle-gst.ps1` has been run and its logic exercised against a synthetic
>   file tree, but **never against a real GStreamer installation**. Its file
>   list is a best-effort starting point, not a verified one.
> - `installer.iss` has been structurally checked but **never compiled**.
> - Every DLL name, installer path and download URL below that is not quoted
>   from the specification is marked **UNVERIFIED — Gate B must confirm**.
>
> None of this is guesswork dressed up as fact. Where a value came from a
> measurement or from the spec, the file says so. Where it did not, it says
> that too. Fix the file, not just your local machine, when you find something
> wrong — the next person to build this is the reason these files exist.

---

## 0. What you are building

One installer that lays down one program folder:

```
C:\Program Files\WSL Studios\WSL Commentary\
  wslcomms.exe                     ~15 MB   Go + Wails + embedded frontend + embedded WebView2 bootstrapper
  slate.png                                 1920x1080 slate, fed to filesrc ! pngdec ! imagefreeze
  *.dll                            60-110 MB GStreamer / GLib / MinGW runtime (default AppDir layout — see section 6)
  gst\lib\gstreamer-1.0\*.dll               the thirteen allowlisted plugins
  gst\BUNDLE-MANIFEST.txt                   what was shipped, with SHA-256 for every file
  licenses\                                 LGPL-2.1 text, written offer, third-party notice
```

Installed footprint **70–120 MB** (specification section 11). Nothing else is
installed. No service is registered. Nothing runs when the app is closed.

---

## 1. Why this must be built on Windows

`CGO_ENABLED=1`. GStreamer is reached through cgo (`internal/gst`), and Wails
links WebView2 through cgo as well. **cgo means no cross-compilation** — you
cannot build this on Linux or macOS for Windows, and there is no point trying.
The build host must be Windows x64 with a working MinGW gcc.

The only part of the tree that builds without any of this is Gate A:

```powershell
$env:CGO_ENABLED=0; go build ./...; go vet ./...; go test ./...
```

That works today because `internal/gst` has a pure-Go stub twin. It does not
produce a shippable executable.

---

## 2. Prerequisites

Roughly 2 GB of downloads and half an hour, mostly unattended.

### 2.1 Go 1.25

`go.mod` declares `go 1.25.0`, and Wails v2.13.0, go-gst v0.0.2 and gosrt all
require it (specification section 3). Verify:

```powershell
go version    # expect: go version go1.25.x windows/amd64
```

### 2.2 Node 24.x and npm

Needed only to build the frontend into `frontend/dist`. End users need nothing.

```powershell
node --version   # expect v24.x
npm --version
```

### 2.3 MinGW-w64 gcc

`go env CC` must find a working `gcc`. Any of these works:

- **MSYS2** — install from https://www.msys2.org, then in the **MSYS2 MinGW
  64-bit** shell: `pacman -S mingw-w64-x86_64-gcc`, and put
  `C:\msys64\mingw64\bin` on `PATH`.
- **winlibs** — https://winlibs.com, unzip, put its `bin` on `PATH`.
- `choco install mingw` — convenient, but see the warning below.

> **UNVERIFIED — Gate B must confirm: the C runtime must match.**
> MinGW-w64 comes in two flavours, one linking the old `msvcrt.dll` and one
> linking the UCRT (`ucrt64` in MSYS2, `winlibs UCRT` in winlibs). The official
> GStreamer `mingw-x86_64` binaries are built by cerbero against one of them,
> and mixing the two can produce link errors against `__mingw_*` symbols, or
> worse, a binary that links and then misbehaves around file handles and
> `printf`-family calls.
>
> Try the **MSVCRT** flavour first (`C:\msys64\mingw64`, not
> `C:\msys64\ucrt64`). If the cgo link stage fails with undefined references
> that look like CRT internals, switch flavour before you debug anything else.
> Record the answer here once you know it.

Verify:

```powershell
gcc --version
go env CC CGO_ENABLED
```

### 2.4 pkg-config

Specification section 11 names `pkgconfiglite`:

```powershell
choco install pkgconfiglite -y
pkg-config --version
```

(If you do not have Chocolatey: https://chocolatey.org/install. `pkgconfiglite`
is a standalone `pkg-config.exe`; any pkg-config on `PATH` will do.)

### 2.5 GStreamer 1.28.5, mingw-x86_64 — one installer, Complete install type

**VERIFIED 2026-07-30 against the download site.** There is exactly one file:

```
https://gstreamer.freedesktop.org/data/pkg/windows/1.28.5/mingw/gstreamer-1.0-mingw-x86_64-1.28.5.exe
```

916 MB, `.exe`. Note two things that older instructions get wrong. It is **not**
an `.msi`, and there is **no separate devel package** — up to 1.26 the runtime
and development builds were shipped as two installers, and from 1.28 they are
one unified installer whose contents you choose at install time.

**Choose the Complete install type, not Typical.** Complete is what installs the
headers, the `.pc` files and the import libraries. Without them there is no
`lib\pkgconfig\gstreamer-1.0.pc`, so `pkg-config` finds nothing, so cgo has no
include or link flags, and the build fails at the first `#include <gst/gst.h>`.
That is the single most common way to lose an hour at Gate B, and picking
Typical is now the way you get there.

Install to the default location. Specification section 11 gives the path this
project expects:

```
C:\gstreamer\1.0\mingw_x86_64\
```

**On the version.** 1.28.5 is the current stable release and the pin in
specification section 3 is correct. The tree at
`gstreamer.freedesktop.org/data/pkg/windows/` also carries 1.29.x — do not take
it. GStreamer uses odd minor numbers for its development series, so 1.29 is
pre-release and 1.28 is the stable line it leads to. If the stable line moves
past 1.28.5, that is a specification change rather than a build decision.

### 2.6 `PKG_CONFIG_PATH`

Specification section 11, verbatim:

```powershell
setx PKG_CONFIG_PATH "C:\gstreamer\1.0\mingw_x86_64\lib\pkgconfig"
```

`setx` affects **new** shells only. In the current one:

```powershell
$env:PKG_CONFIG_PATH = "C:\gstreamer\1.0\mingw_x86_64\lib\pkgconfig"
pkg-config --modversion gstreamer-1.0    # expect 1.28.5
pkg-config --cflags gstreamer-1.0        # expect -I...\include\gstreamer-1.0 ...
```

If `--modversion` prints nothing, stop here. Nothing downstream will work.

### 2.7 The Wails CLI, pinned

```powershell
go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0
wails doctor
```

`wails doctor` is the fastest environment check you have: it reports gcc,
WebView2, npm and the Go toolchain in one screen. Fix anything it flags before
building.

### 2.8 Inno Setup 6.3 or newer

https://jrsoftware.org/isdl.php — the standard installer puts `ISCC.exe` in
`C:\Program Files (x86)\Inno Setup 6\`.

`installer.iss` uses `ArchitecturesAllowed=x64compatible`, which requires
**6.3+**. On an older 6.x, change that directive and
`ArchitecturesInstallIn64BitMode` to `x64` — do not simply delete them, or the
installer will run in 32-bit mode and `{autopf}` will resolve to
`C:\Program Files (x86)`.

### 2.9 WebView2

Nothing to do. Evergreen is part of Windows 11 (specification section 3), and
`wails build -webview2 embed` puts Microsoft's ~150 KB bootstrapper in the
executable for the cases where it is not.

---

## 3. Build

All commands from the repository root.

### 3.1 Frontend

```powershell
cd frontend
npm ci
cd ..
```

`npm ci`, never `npm install`: `package.json` and `package-lock.json` are frozen
by the contract. Wails runs the frontend build itself during `wails build`.

### 3.2 The executable

```powershell
$env:CGO_ENABLED = "1"
wails build -webview2 embed -clean
```

Output: `build\bin\wslcomms.exe`.

Two things to expect the first time:

- **`wails.json` must exist at the repository root.** It is Wails' project file
  and it names the output binary. It is not WP-6's file — it belongs with
  `main.go` and `app.go` (WP-8). If `wails build` complains that it cannot find
  the project file, that is what is missing.
- **Wails may create files in `build\`** — `appicon.png`,
  `windows\icon.ico`, `windows\info.json`, `windows\wails.exe.manifest`. That is
  normal; Wails regenerates its build assets when they are absent. Commit them
  once they exist so that every build produces the same icon and version
  resource.

### 3.3 Stage `dist\`

`dist\` is a byte-for-byte image of the installed program folder.

```powershell
New-Item -ItemType Directory -Force -Path dist\licenses | Out-Null
Copy-Item build\bin\wslcomms.exe dist\ -Force
Copy-Item assets\slate.png       dist\ -Force
Copy-Item build\licenses\*       dist\licenses\ -Force
```

### 3.4 The GStreamer bundle

```powershell
powershell -ExecutionPolicy Bypass -File build\bundle-gst.ps1
```

This copies the runtime **from an explicit file list, never a directory copy**,
because a directory copy would drag GPL `x264enc` into a commercial deliverable.
It fails loudly if a listed file is missing, refuses to copy anything matching
`*x264*`, audits its own output, walks the import table of everything it copied,
and prints the total size (expect **60–110 MB**).

Useful switches:

```powershell
build\bundle-gst.ps1 -DryRun                 # validate the file list, touch nothing (works with no GStreamer)
build\bundle-gst.ps1 -GstRoot D:\gstreamer\1.0\mingw_x86_64
build\bundle-gst.ps1 -CoreDllLayout GstBin   # runtime DLLs in gst\bin instead of beside the exe — read section 6 first
build\bundle-gst.ps1 -NoDependencyReport     # do not do this at Gate B
```

**The first run will probably fail**, and that is the script working correctly.
See section 5.

### 3.5 The installer

```powershell
& "C:\Program Files (x86)\Inno Setup 6\ISCC.exe" /DAppVersion=1.0.0 build\installer.iss
```

Output: `dist\installer\wslcomms-setup.exe`.

`installer.iss` refuses to compile unless `dist\gst\BUNDLE-MANIFEST.txt`,
`dist\wslcomms.exe`, `dist\slate.png` and `dist\licenses\LGPL-2.1.txt` are all
present. The manifest check is a provenance gate: only `bundle-gst.ps1` writes
that file, so a hand-assembled `gst\` folder cannot be packaged by accident.

---

## 4. Verify what you built

Before anyone takes this to a match:

```powershell
# 1. The bundle contains what it says it does.
Get-Content dist\gst\BUNDLE-MANIFEST.txt | Select-Object -First 20

# 2. Nothing GPL got in. Must return nothing at all.
Get-ChildItem dist -Recurse -Filter *x264* 

# 3. The installer installs and the app starts.
dist\installer\wslcomms-setup.exe

# 4. The app is using the bundled GStreamer, not one installed elsewhere.
#    This file only appears after a successful gst_init.
Test-Path "$env:LOCALAPPDATA\WSLComms\registry.bin"
```

If the app starts but no capture devices appear in the dropdown, or Start fails
immediately, a plugin is not loading. Run it with GStreamer's own diagnostics:

```powershell
$env:GST_DEBUG = "GST_PLUGIN_LOADING:5"
& "C:\Program Files\WSL Studios\WSL Commentary\wslcomms.exe"
```

`GST_PLUGIN_LOADING:5` prints every plugin file it tried to open and the exact
reason each one failed. A failure there is almost always a missing dependency
DLL — go to section 5.

Also delete `%LOCALAPPDATA%\WSLComms\registry.bin` between experiments. It is a
cache; a stale one will happily report a plugin that is no longer there.

---

## 5. Extending the DLL list — the loop you will actually run

The thirteen plugins are fixed by specification section 3. What could not be
computed without the files present is their **transitive dependency closure**:
which other DLLs each plugin imports. `bundle-gst.ps1` computes it for you at
Gate B, from the real binaries.

The loop:

1. Run `build\bundle-gst.ps1`.
2. **If it says `MISSING REQUIRED FILE`** — it prints what it looked for, why
   that file is needed and what to do. Usually the file exists under a slightly
   different name (`libglib-2.0-0.dll` vs `glib-2.0-0.dll`). Add the real name
   to that entry's candidate list in `Get-RuntimeEntries`, first in the list.
   Never replace a name with a wildcard, and never fall back to copying the
   directory: that is the whole control.
3. **If it says `UNRESOLVED IMPORTS`** — those are DLLs the bundle needs and
   does not have. For each, add a `New-BundleEntry -Kind Runtime` line with a
   one-line reason. Re-run.
4. Repeat until it reports `closure complete`.
5. Check the `OPTIONAL FILES NOT FOUND` list at the end. Each one is a
   candidate explanation for a plugin that will not load. In particular, if
   **neither** the OpenSSL nor the mbedTLS group was found, the SRT passphrase
   will not work — libsrt needs a crypto backend, and specification section 5
   sets `passphrase` and `pbkeylen=16`.

The walker the script uses, in order of preference: `objdump -p` (ships with
your MinGW gcc), `x86_64-w64-mingw32-objdump -p`, `dumpbin /dependents` (Visual
Studio Build Tools). If you would rather look at the tree by eye, use
[Dependencies.exe](https://github.com/lucasg/Dependencies) — the modern
replacement for Dependency Walker — and open
`dist\gst\lib\gstreamer-1.0\libgstmediafoundation.dll` first, since that is the
plugin most likely to pull in something unexpected.

Run it by hand if you want the raw output:

```powershell
objdump -p dist\gst\lib\gstreamer-1.0\libgstsrt.dll | Select-String "DLL Name"
dumpbin /dependents dist\gst\lib\gstreamer-1.0\libgstsrt.dll
```

Anything that resolves inside `C:\Windows\System32` is part of Windows and must
**not** be bundled.

---

## 6. DLL search order — read this before changing the layout

**The problem.** `wslcomms.exe` is cgo-linked against `gstreamer-1.0`,
`glib-2.0`, `gobject-2.0` and friends. Those imports are resolved by the Windows
loader **before a single line of Go runs**. Nothing `internal/gst`'s `Init` does
— not `os.Setenv("PATH", …)`, not `SetDllDirectory` — can affect them, because
by the time `Init` executes the process has either loaded them or failed to
start with `0xC0000135` ("The specified module could not be found").

The one directory guaranteed to be searched at that moment is the directory
holding the `.exe`.

**Therefore:** `bundle-gst.ps1` defaults to `-CoreDllLayout AppDir`, putting the
runtime DLLs beside `wslcomms.exe`. Plugins always go to
`gst\lib\gstreamer-1.0`, because `internal/gst` sets
`GST_PLUGIN_PATH_1_0=<appdir>\gst\lib\gstreamer-1.0` and that is contract.

`-CoreDllLayout GstBin` mirrors the upstream layout (`gst\bin`) and is tidier,
but it only works if something puts `gst\bin` on the loader's path before the
process starts. That means one of: delay-loading the GStreamer imports at link
time, a launcher stub, or `.local` DLL redirection. **UNVERIFIED — Gate B must
confirm which, if any, is wanted.** Until then, use the default.

One related point for whoever owns `internal/gst`: even in the `AppDir` layout,
plugins are opened with `g_module_open`, and their *own* dependencies are
resolved relative to the plugin's directory first. If a plugin fails to load
with a missing-dependency error while the DLL it wants is plainly sitting beside
the `.exe`, prepending the application directory to `PATH` **before** `gst_init`
is the fix. That is a change in `internal/gst`, not here.

---

## 7. Sizes to expect

| | Expected | Source |
|---|---|---|
| `wslcomms.exe` | ~15 MB | spec section 11 |
| GStreamer bundle | 60–110 MB | spec sections 3 and 11 |
| Installed footprint | 70–120 MB | spec section 11 |
| `wslcomms-setup.exe` | smaller than the above, LZMA2 solid — measure it | — |

`bundle-gst.ps1` warns if the bundle falls outside 60–110 MB. Under: something
is missing. Over: something is being copied that should not be — open
`BUNDLE-MANIFEST.txt` and find out what.

---

## 8. What the uninstaller does, and what it must never do

It removes the program folder. That is all.

It deliberately leaves alone:

- `%APPDATA%\WSLComms\config.json` — the M2L-X host, alias, event id, SRT
  endpoint, `statusKey`, device selections, monitor tile and return mid. Some of
  those values can only be obtained from someone with M2L-X access, so
  destroying them can leave a commentary position unusable until that person is
  free. An upgrade must never be able to do that.
- Credential Manager `WSLComms/m2lx` and `WSLComms/srt` — the M2L-X password and
  the SRT passphrase.
- `%LOCALAPPDATA%\WSLComms\registry.bin` — a rebuildable GStreamer cache, and
  under an admin uninstall `{localappdata}` is the *administrator's* profile
  anyway, so removing it would target the wrong user.

If you ever add an `[UninstallDelete]` section to `installer.iss`, re-read this.

---

## 9. Release checklist

Building the installer is not the same as being allowed to ship it.

- [ ] `build\licenses\WRITTEN-OFFER.txt` — every `<<<…>>>` field completed. The
      offer is void while any remains: it does not say who is offering, or how
      to take it up.
- [ ] `build\licenses\NOTICE.txt` — every line marked UNVERIFIED resolved:
      the GLib version, the ORC/libpng/zlib/MinGW copyright notices, and which
      crypto backend was actually bundled (read `BUNDLE-MANIFEST.txt`).
- [ ] The five licence texts diffed against their canonical URLs (listed at the
      end of `NOTICE.txt`).
- [ ] **Corresponding source archived** for every LGPL/MPL/GPL component at the
      exact versions shipped, keyed to the installer version, retained for three
      years. The written offer is a three-year commitment; upstream deletes old
      tarballs long before that.
- [ ] `dist\gst\BUNDLE-MANIFEST.txt` kept with the release artefacts. It is the
      SHA-256 record of exactly what was shipped.
- [ ] `Get-ChildItem dist -Recurse -Filter *x264*` returns nothing.
- [ ] Installer and executable Authenticode-signed. UNVERIFIED — no certificate
      is known to this project, and a facility will usually want one.
- [ ] The handover note says: **commentary must not be routed to aux1**
      (specification section 7). The app cannot verify or enforce it.

---

## 10. Everything Gate B must confirm

Collected in one place, because these are the things this directory could not
check for itself.

| # | Thing | Where |
|---|---|---|
| 1 | Every DLL name in `Get-RuntimeEntries`, and the full dependency closure | `bundle-gst.ps1` |
| 2 | Whether `libgstmediafoundation.dll` needs `gstd3d11-1.0-0.dll` | `bundle-gst.ps1`, optional entries |
| 3 | Which crypto backend libsrt uses (OpenSSL / mbedTLS) — the SRT passphrase depends on it | `bundle-gst.ps1`, optional entries |
| 4 | That `installer.iss` compiles under ISCC 6.3+ | section 3.5 |
| 5 | That the `AppDir` runtime layout actually starts, and whether `GstBin` can be made to | section 6 |
| 6 | Whether the GStreamer MinGW binaries want an MSVCRT or UCRT gcc | section 2.3 |
| 7 | The GStreamer download file names, and that 1.28.5 is still current | section 2.5 |
| 8 | The exact GLib version, for `NOTICE.txt` | section 9 |
| 9 | The real product version, replacing the `0.0.0` placeholder | `installer.iss` |
| 10 | SP-2: that go-gst v0.0.2 builds under MinGW at all | project plan section 5 |

---

## 11. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `pkg-config: exit status 1` during `wails build` | GStreamer installed with the **Typical** install type instead of Complete, so there are no `.pc` files — or `PKG_CONFIG_PATH` not set in this shell | section 2.5, 2.6 |
| `gcc: executable file not found` | no MinGW on `PATH`, or `CGO_ENABLED=0` | section 2.3 |
| `cannot find -lgstreamer-1.0` | same cause: Typical install, no import libraries | section 2.5 |
| Undefined references to `__mingw_*` or CRT internals | MSVCRT vs UCRT toolchain mismatch | section 2.3 |
| App exits instantly, `0xC0000135` | runtime DLLs not beside the `.exe` | section 6 |
| App starts, device dropdown empty | `wasapi2` plugin not loading — missing dependency | section 4, then 5 |
| Start fails with a passphrase error | libsrt has no crypto backend bundled | section 5, step 5 |
| `bundle-gst.ps1` says `MISSING REQUIRED FILE` | a DLL has a different name in this build | section 5, step 2 |
| `bundle-gst.ps1` says `STRAY` | something copied files into `dist\gst` by other means | re-run without `-KeepExisting` |
| ISCC: `dist\gst\BUNDLE-MANIFEST.txt is missing` | the bundle was not produced by `bundle-gst.ps1` | section 3.4 |
| ISCC: `Unknown directive: ArchitecturesAllowed` value | Inno older than 6.3 | section 2.8 |
| Script blocked by execution policy | default Windows policy | run with `powershell -ExecutionPolicy Bypass -File …` |

---

## 12. Files in this directory

| File | What it is |
|---|---|
| `bundle-gst.ps1` | The DLL allowlist and the staging script. The licensing control. |
| `installer.iss` | Inno Setup script: per-machine, one feature, no options. |
| `licenses\LGPL-2.1.txt` | GNU LGPL 2.1, verbatim. |
| `licenses\GPL-3.0.txt`, `licenses\GCC-RUNTIME-LIBRARY-EXCEPTION-3.1.txt` | For `libgcc_s_seh-1.dll` and `libstdc++-6.dll`. This product is not GPL; the exception is why. |
| `licenses\MPL-2.0.txt` | For libsrt. |
| `licenses\Apache-2.0.txt` | For the AWS SDKs and OpenSSL 3. |
| `licenses\WRITTEN-OFFER.txt` | The three-year source offer. **Has fields that must be completed.** |
| `licenses\NOTICE.txt` | Every bundled third-party component and its licence, and why x264 is excluded. |
| `bin\` | `wails build` output. Not committed. |

`wails build` also generates `appicon.png` and `windows\` here. Those are Wails'
files, not WP-6's.
