# third_party — vendored, patched Wails

Owner: WP-9 (macOS port). This directory exists for exactly one reason, and it
should be deleted the moment upstream makes it unnecessary.

## What is here

    third_party/wails-v2.13.0/   github.com/wailsapp/wails/v2 v2.13.0, verbatim,
                                 plus one patch (two files, ~55 lines)
    third_party/patches/         that patch, as a `patch -p1` file

`go.mod` points the module at this copy:

    replace github.com/wailsapp/wails/v2 => ./third_party/wails-v2.13.0

The version in the directory name is load-bearing. It is the upstream release
this copy is a patch *against*, and it must match the `require` line for
`github.com/wailsapp/wails/v2` in `go.mod`. Nothing enforces that automatically,
so if you bump one, bump the other.

## Why — the bug

macOS, WKWebView. `navigator.mediaDevices.getUserMedia({audio: true})` **never
settles**. It does not reject; it hangs, for the lifetime of the process.

`WailsContext.h:34` declares `WailsContext` as a `WKUIDelegate`, and
`WailsContext.m` implements exactly one of that protocol's methods —
`runOpenPanelWithParameters`. There is no
`webView:requestMediaCapturePermissionForOrigin:initiatedByFrame:type:decisionHandler:`
anywhere in the file. So when the page asks for the microphone, WebKit asks the
delegate for a decision, and nobody ever calls the decision handler.

That is a much worse failure than a rejection, because it defeats the frontend's
own error handling. `frontend/src/ui/app.js`'s `loadHeadphoneDevices` does the
usual and entirely correct thing:

    try {
      const probe = await navigator.mediaDevices.getUserMedia({ audio: true });
      probe.getTracks().forEach((t) => t.stop());
    } catch { /* no input device, or permission denied */ }
    const all = await navigator.mediaDevices.enumerateDevices();

The `await` never returns, so the `catch` never runs, so `enumerateDevices` is
never reached, so the headphone dropdown is empty — with nothing in the console,
no error banner, and no lamp changing colour. The commentator simply cannot
choose an output device, forever.

Proved with two purpose-built WKWebView harnesses identical but for this one
method: without it, the promise was still pending at the end of the run; with
it, `getUserMedia` resolved in 75 ms and `enumerateDevices` returned three real
`audiooutput` devices *with labels* (labels being the other thing the capture
grant buys — an ungranted `enumerateDevices` returns blanks).

### What the real application actually measured, which is not quite that

The harness result above is the reason this patch was written. When the same
A/B was repeated in the *real* application on macOS 26 (Darwin 25.3.0) — two
production-tag `wslcomms` builds differing only in the `replace` line — the
result was sharper in one direction and softer in another, and the difference
matters enough to write down:

| | patched | unpatched |
|---|---|---|
| `getUserMedia({video:true})` | **NotAllowedError in 19–25 ms** | **granted in 4954 ms** |
| `getUserMedia({audio:true})` | resolved in 146–163 ms, track `"MacBook Pro Microphone"` | resolved in 313 ms |
| `enumerateDevices()` audiooutput | 3, with labels | 3, with labels |

Two things follow.

**The delegate is unambiguously being called, and its decision is honoured.**
A 19 ms camera *rejection* cannot come from anywhere but the code below; the
unpatched build hands the same request a camera.

**On this OS build, the missing delegate did not by itself hang anything.**
WebKit's behaviour with no implementation was to GRANT, not to stall — so the
empty-dropdown symptom did not reproduce from that cause here. That does not
make the patch optional, for two reasons, and the first is the serious one:
*without it WebKit gives the page the camera.* This application has no camera
feature; a WebView that will open one on request is a privacy defect on a
machine that sits in front of a commentator all match, and on a bundle with no
`NSCameraUsageDescription` the resulting TCC request is a hard kill rather than
a denial. The second is that "the undocumented default happens to suit us on
the version we tested" is not a thing to ship on an OS that updates itself.

A caution for whoever repeats this, because it cost an hour: **the measurement
is only meaningful once TCC is already satisfied.** WebKit consults TCC *before*
it consults this delegate. Launched with `open(1)`, the app gets its own TCC
identity, macOS raises microphone and camera prompts, and until somebody clicks
them `getUserMedia` is pending — on the patched build exactly as much as on the
unpatched one, which looks precisely like the bug and is not it. Run the binary
straight from a terminal that already holds the grants (TCC attributes to the
responsible parent process) and the two builds separate immediately.

## Why a vendored copy rather than something lighter

Wails v2 exposes no hook for the `WKUIDelegate`. There is no option in
`options.App`, no `mac.Options` field, and no way to reach `WailsContext` from
Go: the delegate is assigned to itself in `WailsContext.m:312`
(`self.webview.UIDelegate = self`) inside the C the cgo package compiles. Things
that were considered and are not good enough:

- **Editing the module cache.** It is read-only, it is shared between every
  checkout on the machine, and `go clean -modcache` or a CI runner wipes it. A
  fix that only exists there is not a fix.
- **`//go:linkname` / an Objective-C category from our own cgo file.** A category
  could in principle add the method to `WailsContext` at load time, but it would
  be silently overridden by any future upstream implementation, it would put cgo
  outside `internal/gst` (CONTRACT.md forbids that), and it depends on the class
  name being stable — which is a much thinner contract than the source we would
  be pinning anyway.
- **A fork on GitHub.** Same patch, but the source of truth is then somewhere
  else and a clean clone of this repo does not build without network access to
  it. A directory in the tree is auditable in a code review and survives GitHub.

## The patch

`third_party/patches/0001-wkuidelegate-media-capture.patch`, two files:

- `internal/frontend/desktop/darwin/WailsContext.h` — declares the method on
  `@interface WailsContext`, so the header shows this class has been changed.
- `internal/frontend/desktop/darwin/WailsContext.m` — implements it.

The decision is **microphone: grant, everything else: deny**, and that is
deliberate rather than lazy. This application captures a commentator's
microphone and has no camera feature at all, so a blanket grant would hand a
camera permission to the WebView for a capability we do not ship — and a bundle
that answers `WKPermissionDecisionGrant` to a camera request also needs
`NSCameraUsageDescription` in its `Info.plist` or TCC kills the process outright.
Deny rejects promptly, which is a diagnosable failure; the hang above is not.

`WKPermissionDecisionGrant` rather than `...Prompt` because this grant is only
WebKit's own gate. The real consent is still macOS's: TCC shows the system
microphone prompt the first time capture actually starts, which is why the
bundle needs `NSMicrophoneUsageDescription` in `build/darwin/Info.plist`
(without it TCC *terminates* the process rather than denying it). A second,
WebKit-drawn dialog stacked on top of the OS one buys nothing and costs a modal
in the middle of a match.

Both the declaration and the definition are wrapped in

    #if defined(MAC_OS_VERSION_12_0) && MAC_OS_X_VERSION_MAX_ALLOWED >= MAC_OS_VERSION_12_0

and annotated `API_AVAILABLE(macos(12.0))`. `WKMediaCaptureType` and
`WKPermissionDecision` arrived in the macOS 12 SDK; we build with
`-mmacosx-version-min=10.13`, so the guard is on the SDK being used, not on the
deployment target. On an older SDK the file still compiles and the method is
simply absent — which is upstream's behaviour, i.e. the bug, which is the honest
outcome for a toolchain that cannot express the fix.

Windows is untouched. This is a `darwin`-only source file; the WebView2 path
never compiles it and its behaviour is bit-for-bit what it was.

## What was left out of the copy

`pkg/templates/` (415 files, 4.6 MB) — the project scaffolding `wails init`
writes out. Nothing in this application's import graph references it, on either
GOOS. Everything else is present, including
`internal/webview2runtime/MicrosoftEdgeWebview2Setup.exe`, which **must** stay:
it is `go:embed`-ed by a package the Windows build imports.

## Verifying this copy is what it claims to be

    diff -r -x 'templates' \
      "$(go env GOMODCACHE)/github.com/wailsapp/wails/v2@v2.13.0" \
      third_party/wails-v2.13.0

should report differences in `WailsContext.h` and `WailsContext.m` and nothing
else. (The `-x templates` is the omission above.) To check the patch file still
matches the tree:

    cd third_party/wails-v2.13.0
    git apply --check -R -p1 ../patches/0001-wkuidelegate-media-capture.patch

One consequence to know about before it surprises somebody: `gofmt -l .` from
the repository root now lists nine files, all of them upstream Wails and none of
them touched by us (upstream ships them unformatted). They are deliberately left
exactly as upstream wrote them, because reformatting them would destroy the
`diff -r` property above — the whole point of which is that this copy differs
from the release in two files and no others. Any repository-wide gofmt or lint
check should exclude `third_party/`.

## Re-applying on upgrade

    # 1. Pull the new upstream into the module cache and copy it in.
    NEW=v2.14.0
    go mod download github.com/wailsapp/wails/v2@$NEW
    rsync -a --delete --exclude 'pkg/templates/' \
      "$(go env GOMODCACHE)/github.com/wailsapp/wails/v2@$NEW/" \
      third_party/wails-$NEW/
    chmod -R u+w third_party/wails-$NEW

    # 2. Re-apply the patch. If it fails, fix it by hand against the new
    #    WailsContext.m and REGENERATE the .patch file — do not leave a stale one.
    (cd third_party/wails-$NEW && patch -p1 < ../patches/0001-wkuidelegate-media-capture.patch)

    # 3. Point go.mod at it: bump BOTH the require line and the replace line.
    # 4. git rm -r third_party/wails-v2.13.0
    # 5. Build, and then actually check the dropdown — see below.

**Before doing any of that, check whether it is still needed.** If upstream has
added the delegate method itself, or added an option for it, drop this directory
and the `replace` line entirely; a vendored dependency you no longer need is a
liability. Upstream tracking issue: Wails v2 has no media-capture callback in
`internal/frontend/desktop/darwin/WailsContext.m` as of v2.13.0.

## How to know it actually works

A build that compiles proves nothing here — the broken version compiled too. Run
the real application and confirm the **Headphones** dropdown on the home screen
lists real device names. The one-line version, from the DevTools console of the
running app:

    (await navigator.mediaDevices.enumerateDevices())
      .filter(d => d.kind === 'audiooutput').map(d => [d.deviceId, d.label])

Fixed: three or more rows with real labels. Two failure modes look identical
from the outside and are not:

- Nothing at all, and the `getUserMedia` earlier in `loadHeadphoneDevices` is
  still pending — an unanswered TCC prompt, or the delegate not answering.
- Rows with **empty labels** — capture was refused, but enumeration still ran.

The sharper check, because it isolates this patch from TCC entirely, is to ask
for the camera:

    await navigator.mediaDevices.getUserMedia({video: true})
      .then(() => 'GRANTED — the patch is NOT in this build')
      .catch(e => e.name)   // NotAllowedError, in ~20 ms, is the patch working

Note that TCC will show the microphone prompt on the first run of a given
bundle; accept it. A build with no `NSMicrophoneUsageDescription` in its
`Info.plist` will be killed at that moment rather than prompting, which looks
like a crash and is not one.
