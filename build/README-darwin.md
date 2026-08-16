# Building the macOS release — the runbook

**Owner:** WP-6 (macOS port). Companion to `README.md`, which is the Windows
side and remains the authority for everything under `windows\`.

This is the whole path from a bare macOS machine to a signed, notarised,
stapled, Gatekeeper-clean `.dmg`. Follow it top to bottom.

> ## Read this before you trust anything below
>
> The opposite of `README.md`'s warning, and worth saying just as plainly.
> **Everything in this document has been run.** The numbers are measured on an
> Apple silicon MacBook running macOS 26.3 with Homebrew GStreamer 1.26.10, on
> 2026-08-14, and the release it describes was submitted to Apple and came back
> `Accepted`:
>
> ```
> id: 7fd25603-d754-4813-bcdd-01e0776f05d0   status: Accepted   (the .app)
> id: 99acf806-c1c4-40e5-b5dc-4f8ab481f2f0   status: Accepted   (the .dmg)
>
> spctl -a -vvv --type exec "WSL Commentary.app"
>   accepted
>   source=Notarized Developer ID
>   origin=Developer ID Application: Sygnal TV Ltd (5P76UVY5WF)
> ```
>
> Where something is still unproven this document says so in those words. There
> is one such place and it is in section 9.

---

## 0. What you are building

**One deliverable.** `build/dist/wslcomms-<version>-macos-arm64.dmg`, 18 MB.
The operator mounts it, drags `WSL Commentary.app` onto the `Applications`
symlink beside it, and double-clicks. That is the whole installation.

No installer, no admin password, no service, nothing running when the app is
closed, and — the requirement that shapes every decision below — **no
prerequisites of any kind on the target Mac.** No Homebrew. No GStreamer. No
Xcode. The `.app` contains its own GStreamer runtime, relinked so that it can
only ever load from inside itself.

**With exactly one exception, added 2026-08-16, and it is stated here rather
than in a footnote because that requirement above is the one everybody quotes.**
A commentary position that takes its programme feed off SDI needs **Blackmagic
Desktop Video** installed, and this product does not and cannot ship it. The
GStreamer `decklink` plugin *is* in the bundle; the DeckLink API it wraps lives
in `/Library/Frameworks/DeckLinkAPI.framework`, belongs to Blackmagic, and the
plugin opens it as a `CFBundle` at run time from that absolute path — outside
the `.app`, invisible to `otool`, and therefore invisible to the audit in
section 5 as well. Without it there is no API, so there are no devices and no
capture, and **the failure is silent**: no dialog, just an input list with no
card in it. It is the same class of dependency as the WebView2 runtime on
Windows, minus the bootstrapper. See `licenses/NOTICE.txt` section F3.

Everything else in that paragraph still holds. A position that does not use a
DeckLink needs nothing at all.

A `.pkg` was not chosen. Windows ships an installer because Windows expects
one; macOS does not, and a `.pkg` would mean an admin prompt and a second
certificate (Developer ID **Installer**) for a product that installs nothing.

```
WSL Commentary.app/Contents/
  Info.plist                            from build/darwin/Info.plist
  MacOS/
    wslcomms                     29 MB  Go + Wails + embedded frontend, cgo-linked
    gst-plugin-scanner                  spawned by libgstreamer to scan plugins
    gst-inspect-1.0                     field diagnostics — see section 8
    gst-launch-1.0                      field diagnostics
    slate.png                           symlink -> ../Resources/slate.png
  Frameworks/                    28 dylibs, the core GStreamer/GLib runtime
  Resources/
    gstreamer-1.0/               22 plugins
    gio-modules/                        deliberately empty — see section 6
    slate.png                           1920x1080 slate for filesrc ! pngdec ! imagefreeze
    licenses/                           LGPL text, written offer, third-party notice
    iconfile.icns
    GST-BUNDLE-MANIFEST.txt             what shipped, with SHA-256 for every file
    GST-ELEMENT-RESOLUTION.txt          which element came from which plugin dylib
```

Bundle total **42 MB**, disk image **18 MB** (the disk image figure is from the
0.1.0 build and predates the three capture-path plugins, which cost 0.5 MB
before compression).

---

## 1. Why this must be built on macOS, on Apple silicon

`CGO_ENABLED=1`. GStreamer is reached through cgo (`internal/gst`) and Wails
links WebKit through cgo as well, so there is no cross-compilation and no point
trying. The build host must be an arm64 Mac.

It must also be the *right* arm64 Mac, for a reason that is not obvious and is
covered properly in section 7: Homebrew builds its bottles for the build
machine's macOS major version, so **the oldest macOS your release can run on is
the macOS your build machine is running**. On this box that is 26.0.

That is a property of the *bottles*, not of this product. The product's own
floor is macOS 11.0 — the arm64 floor, which is also the app binary's measured
`minos` — and `build/darwin/Info.plist` says so. The 26.0 is measured per build
and `ship-darwin.sh` stage 5 raises the *shipped* copy of the key to it. Build
the closure on an older Mac and the shipped number falls, with no source edit.

The only part of the tree that builds without any of this is Gate A:

```sh
CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go vet ./... && CGO_ENABLED=0 go test ./... -count=1
```

That works because `internal/gst` has a pure-Go stub twin. It does not produce
a shippable application.

**Gate A does not test the root package, and on macOS you have to notice that.**
`app.go`, `app_return.go`, `app_picture.go` and every one of their tests are
behind `//go:build dev || production || bindings`, so with no tags the untagged
`main_nocgo.go` is all that is left of `wslcomms` and the run says exactly
`?   wslcomms   [no test files]`. On Windows that is a shrug — `go test -tags dev
.` picks them up with cgo still off. On macOS it is not, because the same tags
drag in Wails' Objective-C frontend, and Wails' frontend is where this port's
link-time breakage lives. Covering it needs Gate B:

```sh
CGO_LDFLAGS="-framework UniformTypeIdentifiers" CGO_ENABLED=1 go test -tags "dev gststub" . -count=1
```

Both additions earn their place. `gststub` keeps `internal/gst` on its pure-Go
twin even though cgo is now on for Wails' sake — without it the cgo half is
selected, the root package's tests reference stub-only symbols such as
`gst.StubPipeline`, and the package fails to COMPILE. The framework flag is
upstream Wails' omission: its darwin frontend references `UTType` and declares
no `#cgo LDFLAGS` for it, so the Wails CLI injects the flag from outside
(`third_party/wails-v2.13.0/pkg/commands/build/base.go:349`, and
`build/ship-darwin.sh:174` does the same); `go test` injects nothing, so the
LINK fails on `Undefined symbols for architecture arm64:
"_OBJC_CLASS_$_UTType"`. See `app.go`'s header, which says the same thing from
the other side.

`-tags "bindings gststub"` also compiles and is **not** the gate to use.
`bindings` is the Wails CLI's binding-generation mode: it leaves the desktop
frontend out of the link entirely, so it passes without ever exercising the
Objective-C that the `UTType` failure comes from. It would go green on precisely
the breakage this gate exists to catch.

---

## 2. Prerequisites

Perhaps twenty minutes, most of it Homebrew downloading.

| | |
|---|---|
| Xcode command line tools | `xcode-select --install` — provides `codesign`, `otool`, `install_name_tool`, `hdiutil`, `ditto`, `xcrun`, `spctl` |
| Go | 1.25 or later |
| Node | for the frontend; `npm` must be on `PATH` |
| Wails CLI v2.13.0 | `go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0` — must match the `go.mod` version, the packager and the runtime are one product |
| GStreamer | `brew install gstreamer` — **build host only.** The shipped `.app` contains its own and the target Mac needs none of this |
| Blackmagic **Desktop Video** | from Blackmagic's support site; admin install, wants a reboot. **This one is needed on the build host AND on any target Mac that uses a card** — unlike everything else in this table. Without it `gst-inspect` cannot find `decklinkvideosrc`, so `bundle-gst-darwin.sh` fails at step 1 with a message naming this row rather than shipping a bundle that cannot capture. Section 0 explains why it cannot be bundled |
| Developer ID **Application** certificate | in the login keychain, with its private key. `Developer ID Installer` is not used and not looked for |
| notarytool keychain profile | `xcrun notarytool store-credentials sygnal-notary --apple-id <id> --team-id 5P76UVY5WF --password <app-specific-password>` |

`build/ship-darwin.sh` checks every one of these before it builds anything.
That is the point of it — see section 4.

---

## 3. Build

```sh
build/ship-darwin.sh
```

That is the whole thing. It takes about eight minutes, nearly all of it waiting
for Apple's notary service twice.

Optionally pass the version — `build/ship-darwin.sh 0.2.0` — in which case it
must **match** `wails.json`'s `info.productVersion`. The version lives in
`wails.json` because that is what templates `build/darwin/Info.plist`, so there
is exactly one place to bump it and the bundle cannot disagree with the
artefact name. Passing a version that differs is treated as "you meant to bump
it and forgot", which is what it almost always is.

Two escape hatches, neither for a release:

| | |
|---|---|
| `SKIP_FRONTEND=1` | reuse `frontend/dist` as it stands. For iterating on the script |
| `SKIP_NOTARIZE=1` | build, sign and package but do not submit to Apple. The result is **not shippable** and the script says so at the end rather than quietly producing something that looks finished |

### 3.1 The stages, and why they are in that order

| | |
|---|---|
| 0 | **Pre-flight.** Everything checkable without building |
| 1 | **Build.** `npm`, then `wails build -tags production` |
| 2 | **Name.** `wslcomms.app` → `WSL Commentary.app` |
| 3 | **Payload.** `slate.png` and the licence texts |
| 4 | **Vendor.** `bundle-gst-darwin.sh` — the GStreamer closure |
| 5 | **Floor.** Raise the shipped `LSMinimumSystemVersion` to what the payload measures |
| 6 | **Sign,** inside out, hardened runtime |
| 7 | **Notarise the .app,** staple it |
| 8 | **Disk image,** sign, notarise, staple |
| 9 | **Assess** with `spctl` |

Two of those orderings are load-bearing and would be easy to get wrong.

**npm before wails.** `wails build` generates bindings *before* it builds the
frontend, and generating bindings compiles this module, and `main.go` carries
`//go:embed all:frontend/dist`. On a clean checkout that directory does not
exist yet, so a release from a fresh clone dies before the frontend it is about
to build has been built:

```
Generating bindings: ERROR
main.go:63:12: pattern all:frontend/dist: no matching files found
```

It does not reproduce in a working tree that has ever run a build, which is
exactly why only a real release on a clean machine would ever find it. So the
script runs `npm run build` itself and then tells `wails` to skip the frontend.

**Vendoring after building.** Stage 4 rewrites the app binary's load commands.
A `wails build` run afterwards replaces that binary with a fresh
Homebrew-linked one and undoes the entire exercise while every other stage
still looks fine. Stage 6's audit re-runs the Homebrew check on the *finished,
signed* artefact specifically to catch that.

---

## 4. Refusing early

Everything after pre-flight is expensive: a cgo build, an npm build, a 48-file
signing pass, and two round trips to Apple that take minutes each and cannot be
undone. A missing certificate found at the end of that is the difference
between a typo and an evening.

So stage 0 checks, before anything is built:

- every tool on `PATH`, with the command that installs each one in the error;
- every file it needs — the entitlements, `Info.plist`, the bundler,
  `forbidden-names.ps1`, `assets/slate.png`, `build/licenses/`;
- the version, present in `wails.json` and a plain dotted number;
- the signing identity, **matched in the keychain**, not assumed. The match is
  on the full string including the team in brackets, because two certificates
  differing only by team is precisely the case a loose match gets wrong. If it
  fails, the script prints the identities that *are* available;
- the notary credentials, with `xcrun notarytool history` — about a second,
  against minutes for a submission that fails on authentication.

---

## 5. What `bundle-gst-darwin.sh` does, and why it is not the Windows script

`build\bundle-gst.ps1` works from an **explicit file list**, and its header
explains why: the list is the licensing control. That approach cannot be
transplanted. Homebrew's dylib names are versioned per keg — `libjpeg.8`,
`libicuuc.78`, `libbrotlidec.1` — and they move under you at every `brew
upgrade`. A stale hand-written list does not fail at build time on your
machine; it fails as a missing dylib at *launch* on the customer's.

So the macOS bundler **computes** the list:

1. **36 element names**, taken from this repo's own Go code —
   `requiredElements` in `internal/gst/gst_cgo.go`, plus
   `returnRequiredElements` and `pictureRequiredElements` — with the darwin
   substitutions spelled out in the script where the Windows name has no twin.
   Resolved to plugin dylibs by `gst-inspect`, never guessed.
2. **The transitive `otool -L` closure** of those plugins, of
   `gst-plugin-scanner`, of the two diagnostic tools, and of the app's own
   cgo-linked binary. Only `/opt/homebrew`-rooted dependencies are followed;
   `/usr/lib` and `/System/Library` are on every Mac already.
3. **The licence gate.** The computed closure is matched against the same
   forbidden-name list `bundle-gst.ps1` uses — **parsed out of
   `forbidden-names.ps1`** rather than restated, because that file's own header
   says it exists so there is exactly one of the list. If the parse yields
   fewer than ten patterns the build fails, because a licensing control that
   silently degrades to "allow everything" is worse than none: it still prints
   a reassuring line.
4. **Copy and relink.** Every `LC_ID_DYLIB`, every Homebrew-rooted
   `LC_LOAD_DYLIB` and every Homebrew-rooted `LC_RPATH` rewritten relative to
   the loading file, and each rewritten Mach-O ad-hoc re-signed.
5. **Audit.** The build fails if one `/opt/homebrew` reference survives.
6. **Prove it.** See section 8.

### 5.1 The measured closure

| | 2026-08-14 | + capture path | + cough mute and seam |
|---|---|---|---|
| Wanted elements | 29 | 33 | **36** |
| Unique plugin dylibs | 17 | 20 | **22** |
| Transitive closure | 48 Mach-O files, 21.9 MB | 51 Mach-O files, 22.4 MB | **53 Mach-O files, 22.5 MB** |
| The whole Homebrew keg, for comparison | ~190 MB, 268 plugins | ~190 MB, 268 plugins | ~190 MB, 268 plugins |
| `/opt/homebrew` load commands in the finished bundle | 0 of 49 Mach-O files | 0 of 52 Mach-O files | **0 of 54 Mach-O files** |
| Licence patterns applied | 19, zero matches | 19, zero matches | 19, zero matches |

The second column is the SDI capture path — `decklinkvideosrc`,
`decklinkaudiosrc`, `videorate` and `deinterlace`. **The three new files are the
three plugins and nothing else**: `libgstdecklink.dylib` (246,240 bytes),
`libgstdeinterlace.dylib` (204,512) and `libgstvideorate.dylib` (114,880). Not
one new library, because `otool -L` on all three names only `libgstreamer`,
`libgstbase`, `libgstvideo`, `libgstaudio`, `liborc`, `libglib` and `libgobject`
— every one already in the closure. That is a diff of the two manifests, not an
estimate.

The third column is `volume` (the cough mute, `libgstvolume.dylib`, 78,448
bytes) and `proxysink`/`proxysrc` (the capture/send seam, `libgstproxy.dylib`,
75,344 bytes, one file for both factories). Same rule again, and again from a
manifest diff rather than an estimate: **one new file each and no new library**
— `libgstproxy.dylib` names only `libgstreamer-1.0.0`, `libglib-2.0.0`,
`libgobject-2.0.0` and `/usr/lib/libSystem.B.dylib`. The closure's printed size
does not move between the second and third columns because 75,344 bytes is
0.07 MB; the file count is the honest measure of that change, not the megabytes.

`volume` landed without this table being updated, which is why the second column
reads 33/20/51 rather than the 34/21/52 the bundle actually carried by the end
of that day. Recorded rather than silently folded in: these numbers are only
ever as good as the edit that accompanies the change.

Nothing Blackmagic appears in any column, and section 0 says why: the
DeckLink API is opened as a `CFBundle` from an absolute path, which no load
command names and no `otool` walk can see.

### 5.2 The ad-hoc signatures are not optional

`install_name_tool` rewrites a Mach-O in place, which **invalidates** the
signature it was built with, and on Apple silicon dyld refuses to load a Mach-O
whose signature does not match. An unsigned-after-rewrite dylib is a launch
failure, not a warning. Those ad-hoc signatures are scaffolding and are
replaced wholesale by the Developer ID pass in stage 6.

### 5.3 Stricter than the script it learned from

The technique here — resolve elements to plugins, walk `otool` transitively,
rewrite to `@loader_path`, ad-hoc re-sign, fail on a surviving Homebrew
reference — is adapted from a proven bundler in another of the same author's
products. None of that product's *shape* came across: this targets an `.app`
rather than a daemon payload, it names no path outside this tree, and it
carries none of that product's identifiers. WSL Commentary is a separate
product for a different client and there is deliberately no build-time
dependency between the two.

One place this is stricter: that bundler audits `LC_LOAD_DYLIB` only.
Homebrew's `libjpeg.8` also carries an `LC_RPATH` of
`/opt/homebrew/Cellar/jpeg-turbo/<v>/lib`, which that audit would have shipped.
Nothing in today's closure resolves an `@rpath`-relative load command so it is
inert — but it is a live search path the moment any future dependency does, and
a Homebrew path inside a signed, notarised binary either way. It is stripped
and audited for.

---

## 6. Bundle layout — read this before changing it

**Two rules, both discovered the hard way, both fatal rather than untidy.**

### `Contents/Frameworks/` may not contain a plain directory

The obvious home for the plugins is `Contents/Frameworks/gstreamer-1.0`, which
is where the bundler put them for its first hour of life. Measured:

```
$ codesign --force --sign - "WSL Commentary.app"
WSL Commentary.app: bundle format unrecognized, invalid, or unsuitable
In subcomponent: .../Contents/Frameworks/gstreamer-1.0
```

`codesign` treats every **directory** under `Contents/Frameworks` as nested
code and requires it to be a bundle — a `.framework` or an `.app`. A plain
directory of dylibs is not one, so the whole `.app` cannot be signed *at all*.
`Contents/PlugIns` behaves the same way.

`Contents/Resources` has no such rule. Its contents are sealed by hash into
`CodeResources`, and a subdirectory of dylibs there signs, verifies
`--deep --strict` and notarises. So the plugins live in
`Contents/Resources/gstreamer-1.0`.

Flat dylibs directly in `Contents/Frameworks` are fine and stay there. Pointing
`GST_PLUGIN_PATH` at `Frameworks` instead would make the plugin scanner attempt
to load all twenty-eight core dylibs as plugins and log a failure for each.

### `Contents/MacOS/` may not contain a non-Mach-O file

`app.go`'s `slatePath` resolves the configured (default: bare `slate.png`) path
against `appDir`, and `main.go`'s `appDir` is the directory holding the
executable — `Contents/MacOS`. Putting `slate.png` there directly:

```
codesign: code object is not signed at all
In subcomponent: .../Contents/MacOS/slate.png
codesign --verify: a sealed resource is missing or invalid
```

The fix is a **symlink**: the file lives at `Contents/Resources/slate.png`,
where macOS wants data, and `Contents/MacOS/slate.png` points at it. `codesign`
seals the symlink, `--verify --deep --strict` passes, and Go's `os.Open`
follows it. The alternative was a darwin branch in `app.go` to look in
`../Resources` — a change to another package's file to solve a packaging
problem. Worth doing if this ever grows a second data file; not worth it for
one.

### The empty `gio-modules` directory

`libgio` has `/opt/homebrew/lib/gio/modules` compiled into it as a **C string**,
not a load command, so `otool` cannot see it and `install_name_tool` cannot
rewrite it. On a customer's Mac that directory does not exist and nothing
happens. On a developer's Mac — or any operator who happens to have Homebrew —
it does, and a foreign glib's modules get `dlopen`ed into this process next to
our own vendored glib. `internal/gst` points `GIO_MODULE_DIR` at the empty
bundled directory instead. It contains a `README.txt` so that it survives being
copied and zipped, and so that whoever finds it knows why it is empty.

The bundler reports every remaining embedded Homebrew **string** as a warning
rather than a failure, precisely because the fix for those is at run time and
not here. Measured today: 11 files, most of them inert (locale directories,
D-Bus socket paths). Two are not — `libgio`'s module directory and `libglib`'s
`XDG_DATA_DIRS` default.

---

## 7. `Info.plist`, entitlements, and the deployment floor

`build/darwin/Info.plist` **replaces** the one Wails would generate. Wails
prefers a project copy and only falls back to its embedded template when the
file is absent — and the fallback *writes* the template into `build/darwin/`,
so an accidental deletion looks like it worked. Stage 1 therefore verifies the
built bundle afterwards: it checks `CFBundleIdentifier` really is
`tv.wslstudios.commentary` and that `NSMicrophoneUsageDescription` really is
present, and fails if not.

Three things the stock template gets wrong, in order of expense:

1. **No `NSMicrophoneUsageDescription`.** When a process first touches
   CoreAudio input and TCC finds no usage string, macOS does not deny the
   request and does not prompt — it **terminates the process**. To the operator
   that is the app vanishing the instant they press Start. One string covers
   both users of the microphone here, which are not the same code:
   `osxaudiosrc` in the pipeline, and `getUserMedia` inside the WKWebView
   (WKWebView capture is attributed to the host bundle).
2. **`CFBundleIdentifier com.wails.<name>`** — a namespace this project does
   not own, derived from `wails.json`'s `name`, so renaming the project would
   silently change the identity that the TCC grant, the keychain ACL and the
   notarisation ticket are keyed on. It is a **literal** in our copy.
3. **`LSMinimumSystemVersion 10.13.0`**, a number that is not reachable on this
   platform at all: arm64 macOS begins at 11.0, and the toolchain clamps
   `-mmacosx-version-min=10.13` up to 11.0 (measured, on both the compiler and
   the shipped `Contents/MacOS/wslcomms`). Our copy says **11.0** — the arm64
   floor, which is also the app binary's own `minos`. See below for why that is
   not the same number as the payload's.

### Two floors meet at stage 5

There are two deployment floors in this bundle and they are different kinds of
thing. Confusing them is what put a build-machine accident into a checked-in
file once already.

**The product's floor** lives in `build/darwin/Info.plist` and is **11.0**. It
is derived, not picked: arm64 macOS starts at Big Sur, the app binary carries
`minos 11.0`, and every macOS API this application calls is older than that.
It is stable across build machines, which is what makes it safe to check in.

**The payload's floor** is measured on every build. Homebrew builds its bottles
for the build machine's macOS major version:

```
$ otool -l Contents/Frameworks/libgstreamer-1.0.0.dylib | grep -A1 minos
   minos 26.0
```

dyld will not map that on anything older. The `.app` would install happily on
macOS 14 and fail to launch with a message no operator can act on. So the
bundler computes the highest `LC_BUILD_VERSION minos` across the whole staged
payload and writes it into the manifest as `MINOS-FLOOR`.

**Stage 5 reconciles them**, and ships the higher of the two: it raises the
built bundle's `LSMinimumSystemVersion` to `MINOS-FLOOR` when the payload needs
more than the product promises, and prints why. Today that means the `.app` an
operator receives still says 26.0. Stage 5 also fails outright if the built
`Info.plist` has no such key at all, which is the signature of Wails having
written its own template over `build/darwin/Info.plist`.

Stage 5 used to *refuse* instead, and the only way to satisfy the refusal was to
edit the number in `build/darwin/Info.plist` to match whichever Mac last ran the
build — which is exactly the edit not to make. Lowering the floor an operator
sees is a matter of building GStreamer against an older SDK, or running the
release build on an older Mac. It is still not a matter of editing the number.

### Entitlements: two keys

`build/darwin/wslcomms.entitlements` is applied to the outer bundle only. The
48 nested Mach-Os are signed with the hardened runtime and no entitlements.

The file carries the reasoning for every key taken and every key rejected —
read it, it is the more complete document. Two results are worth repeating
here because they are counter-intuitive:

**`com.apple.security.cs.disable-library-validation` IS needed**, for one load
that is not in the bundle at all. This section used to say the opposite, on a
measurement that was correct and incomplete, and the correction is worth
following because the same trap is waiting for the next externally-installed
dependency.

The measurement that was right: with the whole bundle signed by one Developer ID
team, the bundle's own hardened-runtime `gst-inspect` `dlopen`ed
`libgstosxaudio.dylib` out of `Contents/Resources/gstreamer-1.0` with no waiver
at all, and re-signing one plugin ad-hoc so it carried a *different* team
produced:

```
module_open failed: dlopen(.../libgstlevel.dylib): code signature not valid for
use in process: mapping process and mapped file (non-platform) have different
Team IDs
No such element or plugin 'level'
```

What it missed: `libgstdecklink.dylib` does not link Blackmagic's API. It holds
the string `/Library/Frameworks/DeckLinkAPI.framework` and loads it through
CFBundle at first use. That framework belongs to Desktop Video, is signed by
Blackmagic (`9ZGFBWLSYP`), and is not ours to sign. Under the hardened runtime
with no waiver, the load is refused with the same "different Team IDs" message
— and the decklink device provider then reports zero cards, silently. The
symptom at the desk was a Settings screen greying out the DeckLink input and
saying no card was fitted, about a fitted, working UltraStudio 4K Mini.

A/B on this machine, same binary, hardened runtime both times, `dlopen` of
`/Library/Frameworks/DeckLinkAPI.framework/Versions/A/DeckLinkAPI`:

| entitlements | result |
| --- | --- |
| none | `not valid for use in process: ... different Team IDs` |
| `disable-library-validation` | loads |

**The signing pass is still load-bearing, and is now the only thing enforcing
itself.** `ship-darwin.sh` signs every Mach-O in the bundle with our team.
Before this key, a file it missed failed to load and somebody noticed
immediately; now it loads. If the signing pass ever stops covering part of the
payload, the fix is still to sign it — the difference is that nothing will tell
you.

**Neither JIT entitlement helps.** `liborc` — GStreamer's SIMD code generator,
used by `videoconvert`, `videoscale`, `audioconvert` and `audioresample` —
does generate code at run time, and under the hardened runtime it prints

```
ORC: ERROR: orc_code_region_allocate_codemem(): Failed to create write and exec
mmap regions. This is probably because the Hardened Runtime is enabled without
the com.apple.security.cs.allow-jit entitlement.
```

The obvious reading is wrong twice. The same binary re-signed with `allow-jit`,
and again with `allow-jit` **and** `allow-unsigned-executable-memory`, printed
the identical error: on Apple silicon a simultaneously writable and executable
mapping cannot be obtained at all. And it costs nothing — 300 buffers of
`png ! imagefreeze ! videoconvert ! videoscale ! I420 1280x720`, user CPU
seconds: 1.64 / 1.62 with no entitlement, 1.65 / 1.62 with `allow-jit`,
1.62 / 1.65 with both, 1.63 with `ORC_CODE=backup`. Indistinguishable.

So neither key is taken, and `internal/gst` sets `ORC_CODE=backup` before
`gst_init` to suppress an ERROR-level line that would otherwise appear in every
log this product produces and send whoever reads it hunting for a fault that is
not there.

---

## 8. Verify what you built

Stages 6b and 9 do all of this automatically and fail the build. Run them by
hand when you are diagnosing something.

```sh
APP="build/bin/WSL Commentary.app"

# 1. The signature is valid and the seal is intact. --deep IS right here:
#    this is VERIFICATION, it is read-only, and walking the tree is the point.
#    Never --deep for signing.
codesign --verify --deep --strict "$APP"

# 2. It is ours, hardened, and carries the entitlement it should.
codesign -dv --entitlements - "$APP"
#   Identifier=tv.wslstudios.commentary
#   flags=0x10000(runtime)
#   TeamIdentifier=5P76UVY5WF
#   Authority=Developer ID Application: Sygnal TV Ltd (5P76UVY5WF)
#   [Key] com.apple.security.device.audio-input

# 3. Nothing GPL or patent-encumbered got in. Must return nothing at all.
grep -Ei 'x264|x265|libav|ffmpeg|faac|ugly' "$APP/Contents/Resources/GST-BUNDLE-MANIFEST.txt"

# 4. It is genuinely self-contained. Must return nothing at all.
find "$APP" -type f | while read -r f; do
  file "$f" | grep -q Mach-O || continue
  otool -L "$f" | grep -q /opt/homebrew && echo "$f"
done

# 5. Gatekeeper's own verdict, which is the one that matters.
spctl -a -vvv --type exec "$APP"
#   accepted / source=Notarized Developer ID
```

### 5.1 The hermetic proof

The static audit proves no Mach-O *names* a Homebrew path. It does not prove
the bundle is **complete**, and it cannot: a plugin that fails to load because
a dependency was missed loads silently-not-at-all and the element simply is not
there.

So the bundler looks every wanted element up again, through a `gst-inspect`
that is itself inside the bundle — Homebrew's own `gst-inspect` links
`libgstreamer` absolutely and would load the core runtime from Homebrew
whatever `GST_PLUGIN_PATH` said — under `env -i`, and requires the resolved
`Filename` to be inside `Contents/Resources/gstreamer-1.0`. Anything else is a
LEAK and fails the build.

**All 36 elements resolve from inside the bundle.** That is why the two
diagnostic tools ship: 0.2 MB, and without them the proof is impossible and a
plugin problem on a machine with no GStreamer is undiagnosable.

To run a real pipeline out of a shipped bundle by hand:

```sh
A="/Applications/WSL Commentary.app"; D=$(mktemp -d)
env -i HOME="$D" ORC_CODE=backup \
  GST_PLUGIN_SYSTEM_PATH_1_0="$A/Contents/Resources/gstreamer-1.0" \
  GST_PLUGIN_PATH_1_0="$A/Contents/Resources/gstreamer-1.0" \
  GST_PLUGIN_SCANNER_1_0="$A/Contents/MacOS/gst-plugin-scanner" \
  GIO_MODULE_DIR="$A/Contents/Resources/gio-modules" \
  GST_REGISTRY_1_0="$D/registry.bin" \
  "$A/Contents/MacOS/gst-launch-1.0" \
    filesrc location="$A/Contents/MacOS/slate.png" ! pngdec ! imagefreeze num-buffers=50 \
    ! videoconvert ! videoscale ! video/x-raw,width=1280,height=720 \
    ! vtenc_h264_hw ! h264parse ! mpegtsmux name=m ! fakesink \
    osxaudiosrc num-buffers=100 ! audioconvert ! audioresample ! level \
    ! atenc ! aacparse ! m.
```

**Measured, from the copy pulled out of the notarised disk image:** ran to EOS
in 1.16 s with both elementary streams muxed. That is the full contribution
pipeline — hardware H.264, AudioToolbox AAC, MPEG-TS mux — running with nothing
on `PATH` and nothing in the environment but the bundle.

---

## 9. Sizes to expect

Measured 2026-08-16, except the disk image, which is the 0.1.0 figure and
predates the capture path.

| | Measured | Note |
|---|---|---|
| `wslcomms` binary | 29 MB | Go + Wails + embedded frontend, cgo |
| GStreamer closure | **22.5 MB** | 53 Mach-O files: 28 core dylibs, 22 plugins, 3 tools |
| — of which the SDI capture path | 0.5 MB | `decklink` 246 KB, `deinterlace` 205 KB, `videorate` 115 KB, and no new library |
| — of which the cough mute and the seam | 0.15 MB | `volume` 78 KB, `proxy` 75 KB — one file for both `proxysink` and `proxysrc`, and no new library |
| Whole Homebrew keg, for comparison | ~190 MB | 268 plugins |
| `WSL Commentary.app` | **42 MB** | 41 MB before the capture path |
| `wslcomms-<v>-macos-arm64.dmg` | 18 MB | UDZO, 0.1.0 — re-measure at the next release |

The bundler warns if the finished bundle falls outside 25–90 MB, on the same
reasoning as the Windows script's band: under, something did not get staged;
over, something is being copied that should not be — read the manifest and find
out what.

---

## 10. The AAC encoder, and why the licence position does not change

`mfaacenc` on Windows becomes **`atenc`** on macOS, and that is decided.

`atenc` is the exact analogue of the Media Foundation encoder: a thin LGPL
GStreamer wrapper (in `libgstosxaudio.dylib`, `gst-plugins-good`) over an AAC
encoder that is **part of the operating system** — AudioToolbox. So
`build/licenses/NOTICE.txt` section G's sentence, *"no third-party AAC
implementation and no separate AAC patent licence is involved on our side"*,
stays literally true on macOS, and section F gains a line rather than the
product gaining a dependency. It also shares a dylib with `osxaudiosrc`, so the
bundle gains **zero** files for the encoder.

Measured indistinguishable from `fdkaacenc` — 131.2 vs 131.5 kbit/s against a
128k target on white noise, ~21 ms transit for both.

`fdkaacenc` is **ruled out on licence**: Homebrew's metadata says Apache-2.0
and `gst-inspect` says LGPL, and both are wrong about the source. `faac`
overshot badly (140.4 kbit/s) and is on the forbidden list anyway. Anything
libav-derived is forbidden by `return_cgo.go`'s own rule and by
`forbidden-names.ps1`. The licence gate in stage 4 enforces all of this over
the computed closure, so none of them can arrive by accident through a
dependency.

---

## 11. Release checklist

1. `git status` clean, on the release commit.
2. Bump `info.productVersion` in `wails.json`. Nothing else carries a version.
3. Gate A green: `CGO_ENABLED=0 go build ./... && go vet ./... && go test ./... -count=1`,
   **and then Gate B**, because Gate A prints `?   wslcomms   [no test files]`
   and shipping on that alone would release `app.go`, `app_return.go` and
   `app_picture.go` untested and, worse, unlinked against Wails' Objective-C
   frontend:
   `CGO_LDFLAGS="-framework UniformTypeIdentifiers" CGO_ENABLED=1 go test -tags "dev gststub" . -count=1`.
   Section 1 explains both flags and why `-tags "bindings gststub"` is not a
   substitute for them.
4. `build/ship-darwin.sh <version>` — the version argument is a deliberate
   double-check against step 2, not a second source of truth.
5. Read the output. Specifically: the licence gate line, `0 of N Mach-O files
   reference /opt/homebrew`, `all 36 elements resolved`, both `status:
   Accepted`, and both `spctl` verdicts.
6. Note the `sha256` the script prints. That is the artefact's identity.
7. Test on a Mac that has never had Homebrew. **This is the one step this
   document has not been able to perform** — every machine available to this
   work has Homebrew installed. The static audit, the `env -i` hermetic proof
   and the `DYLD_PRINT_LIBRARIES` check together make it very hard for a
   Homebrew dependency to survive, but they are not the same thing as a clean
   machine, and until someone runs it on one this remains **unverified**.

---

## 12. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `bundle format unrecognized, invalid, or unsuitable / In subcomponent: …/Contents/Frameworks/<dir>` | a plain directory under `Frameworks` | section 6 |
| `code object is not signed at all / In subcomponent: …/Contents/MacOS/<file>` | a non-Mach-O file in `MacOS` | section 6 — symlink it from `Resources` |
| `Failed to parse entitlements: AMFIUnserializeXML: syntax error near line N` | a double hyphen inside an XML comment in the entitlements file | describe codesign flags, never write them out. The file says so at the top |
| `A timestamp was expected but was not found` | Apple's timestamp authority flaked | the script retries three times. Never drop the timestamp: an untimestamped signature expires with the certificate and the notary service rejects it |
| `Stapler is incapable of working with ZIP archive files` | stapling the transport zip instead of the `.app` | submit the zip, staple the bundle |
| `pattern all:frontend/dist: no matching files found` | `wails build` on a clean checkout | section 3.1 — build the frontend first |
| `ORC: ERROR: … Failed to create write and exec mmap regions` | `liborc` on Apple silicon under the hardened runtime | harmless and unfixable by entitlement; `ORC_CODE=backup` silences it. Section 7 |
| App launches, then reports missing elements | plugins not found | run the section 8.1 command; if `gst-inspect` finds them and the app does not, the environment `internal/gst` sets before `gst_init` is wrong, not the bundle |
| `gst-inspect-1.0 cannot find these elements: decklinkvideosrc decklinkaudiosrc` from the bundler | **Desktop Video is not installed on the build host** | install it and re-run. This is not a GStreamer packaging problem and reinstalling GStreamer will not help: `libgstdecklink.dylib` is in a stock Homebrew install, and it is the Blackmagic framework it opens at run time that is missing. The bundler says so in its own error |
| App runs, DeckLink input list is empty, no error anywhere | **Desktop Video is not installed on the operator's Mac**, or the card is claimed by another application | section 0. `system_profiler SPExtensionsDataType \| grep -i blackmagic` should name `com.blackmagic-design.BlackmagicIO.DExt`, and `ls /Library/Frameworks/DeckLinkAPI.framework` should exist. Blackmagic's own Desktop Video Setup is the quickest confirmation that the card is seen at all |
| `spctl: rejected` on the customer's Mac | not notarised, or not stapled | re-run without `SKIP_NOTARIZE`; check `xcrun stapler validate` |

---

## 13. Files in this directory (macOS)

| File | What it is |
|---|---|
| `ship-darwin.sh` | The release. Clean tree to signed, notarised, stapled, Gatekeeper-clean `.dmg`, in one command that refuses early |
| `bundle-gst-darwin.sh` | Computes the GStreamer closure, relinks it to `@loader_path`, applies the licence gate, audits, and proves the result hermetically |
| `darwin/Info.plist` | The shipping bundle's `Info.plist`. Replaces Wails' template — microphone string, our bundle identifier, the measured OS floor |
| `darwin/Info.dev.plist` | The `wails dev` bundle's. Exists so Wails does not write its own template with no microphone string |
| `darwin/wslcomms.entitlements` | One key, and the measurements behind every key that was rejected |
| `forbidden-names.ps1` | Shared with the Windows side. The macOS bundler **parses** it rather than restating it, so there is exactly one list |
| `licenses/` | Shipped into `Contents/Resources/licenses/` |
| `dist/` | `.dmg` output. Not committed |
| `bin/` | `wails build` output. Not committed |
