# third_party — vendored, patched Wails

Owner: WP-9 (macOS port). This directory exists for two reasons, both of them
holes in the same protocol implementation upstream, and it should be deleted the
moment upstream makes it unnecessary.

## What is here

    third_party/wails-v2.13.0/   github.com/wailsapp/wails/v2 v2.13.0, verbatim,
                                 plus two patches — both of them fill in methods
                                 of the SAME protocol, WKUIDelegate, in the same
                                 two files
    third_party/patches/         those patches, as `patch -p1` files, numbered
                                 and ORDERED: 0002 lands on top of 0001

`go.mod` points the module at this copy:

    replace github.com/wailsapp/wails/v2 => ./third_party/wails-v2.13.0

The version in the directory name is load-bearing. It is the upstream release
this copy is a patch *against*, and it must match the `require` line for
`github.com/wailsapp/wails/v2` in `go.mod`. Nothing enforces that automatically,
so if you bump one, bump the other.

Both bugs below have the same shape, and it is worth stating once: **Wails v2
declares `WailsContext` a `WKUIDelegate` and then implements one single method of
that protocol.** Everything else WebKit routes to the delegate — permission
decisions, `alert`, `confirm`, `prompt` — falls on the floor. WebKit's failure
mode for a missing delegate method is never an error; it is silence, and silence
that the page reads as a legitimate answer.

## Why — bug one: the microphone

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

## Why — bug two: every preset dialog on the Settings page did nothing

Found by the operator running the macOS build. On the Settings page, **Apply**,
**Save current as...**, **Rename** and **Delete** (and Delete's second question,
about the stored passwords) all appeared to work and silently did nothing. No
dialog, no error, no console output, no log line. The preset dropdown on the
**home** screen applied instantly and correctly the whole time.

That contrast is the entire diagnosis. The five broken operations are exactly the
five that sit behind a JavaScript dialog:

| `frontend/src/ui/settings.js` | call |
|---|---|
| :347 | `if (!window.confirm(text)) return;` — Apply |
| :366 | `window.prompt(...)` — Save current as |
| :382 | `window.prompt(...)` — Rename |
| :396 | `window.confirm(...)` — Delete |
| :404 | `window.confirm(...)` — Delete stored credentials too |

The home dropdown is the one preset control that asks the operator nothing.

WKWebView has no built-in UI for `alert`/`confirm`/`prompt`. It routes all three
to its `WKUIDelegate`, and **if the delegate does not implement the matching
method, WebKit shows nothing and hands the page the cancelled answer**:
`confirm()` returns `false`, `prompt()` returns `null`. So every one of those
lines took its early return, immediately, every time. Upstream v2.13.0 implements
none of the three.

Windows is Chromium (WebView2), which draws these dialogs itself, which is why
this is macOS-only and why the on-air build never saw it.

Measured, in a purpose-built WKWebView harness that loads a page calling all
three and drives the resulting sheets programmatically (there is nobody to click
them). The **same delegate source**, extracted verbatim from `WailsContext.m`,
against a second delegate shaped like upstream — `runOpenPanelWithParameters`
and nothing else:

| what the page called, and what the harness then did to the sheet | upstream-shaped delegate | patched |
|---|---|---|
| `confirm(...)` — OK clicked | `false`, no sheet to click | **`true`** |
| `confirm(...)` — Cancel clicked | `false`, no sheet to click | **`false`** |
| `prompt(msg, "Studio 3")` — retyped, OK | `null`, no sheet to click | **`"Studio 3 (renamed by harness)"`** |
| `prompt(...)` — Cancel clicked | `null` | **`null`** |
| `alert(...)` | returned, nothing shown | returned, sheet shown |

The unpatched column is the operator's bug reproduced exactly: four panels
answered "cancelled" without being asked and the fifth drew nothing, the whole
page finishing in under 300 ms with no dialog ever on screen.

Two further things the harness settled, both of which are in the code comments
because they are not obvious:

- **The delegate is called on the main thread.** All five callbacks logged
  `isMainThread=1`. So the AppKit work is done directly, with no dispatch — a
  `dispatch_sync` onto the main queue from the main thread would deadlock
  instantly, and an async hop would only add a frame of latency.
- **`setInitialFirstResponder:` is not enough to put the caret in the prompt's
  text field.** With only that line the sheet opened with `firstResponder =
  _NSAlertPanel` and the field unfocused, so the first thing typed would have gone
  nowhere. Adding `makeFirstResponder:` *after* the sheet has been begun gives
  `firstResponder = NSTextView`, that text view being the field's own editor, with
  `selectedRange = 0+8` over the seeded `"Studio 3"` — i.e. type to replace, which
  is what Chromium does on Windows.

The window-is-gone path was tested too, by detaching the WebView from its window
and asking the page again: `confirm()` returned `false` and `prompt()` returned
`null`, both in about 3 ms, with no exception raised. That path matters more than
it looks — see the completion-handler note below.

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

## The patches

They are a **series**, in number order, and both touch the same two files in the
same class. 0002's context includes text that 0001 added, so 0002 does not apply
to pristine upstream on its own and 0001 does not reverse out from under 0002.
Apply 0001 then 0002; reverse 0002 then 0001. Nothing enforces that but this
paragraph and the numbers on the filenames.

### 0001 — `0001-wkuidelegate-media-capture.patch`

Two files:

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

### 0002 — `0002-wkuidelegate-js-panels.patch`

The same two files, and the same shape: the header declares, the `.m` implements.
Three methods —
`webView:runJavaScriptAlertPanelWithMessage:initiatedByFrame:completionHandler:`,
its `...ConfirmPanel...` sibling, and
`webView:runJavaScriptTextInputPanelWithPrompt:defaultText:initiatedByFrame:completionHandler:`
— plus one private helper that builds the shared `NSAlert`. No SDK guard on this
one: all three have existed since WKWebView shipped in macOS 10.10.

The decisions in it, and what breaks if they go the other way:

**Sheet-modal, never app-modal.** `beginSheetModalForWindow:completionHandler:`
returns immediately and leaves the run loop turning. `-[NSAlert runModal]` does
not: it spins a modal run loop in
`NSModalPanelRunLoopMode`, and the main *dispatch* queue is serviced from the
common modes, so `dispatch_async`-ed main-queue work can sit unserviced for the
entire life of the dialog. `internal/gst/overlay_darwin.go` — the picture surface,
an `NSView` that is a **sibling of this WKWebView inside the same contentView** —
does all its AppKit work exactly that way, and has one `dispatch_sync` at
construction. An app-modal alert therefore stalls the picture's geometry and can
block a Go caller outright, behind a dialog that is not visibly attached to
anything. A sheet also lands on the window the operator is already looking at,
which matters in an application that has more than one.

**The completion handler is called exactly once on every path, and that is
enforced rather than hoped for.** WKWebView raises an Objective-C exception if
one of these handlers is never called, or is called twice — an uncaught exception
inside the AppKit event loop, i.e. a crash of a live commentary position, not a
warning. Never-called is the worse of the two, because it also parks the page's
JavaScript thread for good. Each method funnels every exit through a single
`respond` block that latches a flag, and the easily-forgotten path —
`webView.window` is `nil`, because the window went away between the page asking
and us answering — answers with the cancelled value rather than returning
silently. `NO` and `nil` are also the *safe* answers there: every `confirm()` in
this application gates something destructive or disruptive.

**No `dispatch_async` to the main queue.** WebKit already delivers these
callbacks on the main thread — measured, `isMainThread=1` on all five panels the
harness raised, and upstream's own `runOpenPanelWithParameters` next door already
depends on it by touching `NSOpenPanel` directly. If that ever changes, the fix is
an async hop guarded by `[NSThread isMainThread]`; it is never `dispatch_sync`,
which deadlocks the instant the queue you are waiting on is the one you are
running on.

**The message is split at its first line break** — first line into `messageText`,
the rest into `informativeText`. JavaScript hands over one string and `NSAlert`
has two slots. The Apply confirmation is 300-odd characters: a question, a blank
line, then a field-by-field diff. All of it in `messageText` renders the diff in
bold heading type; all of it in `informativeText` leaves the alert with no
heading. The split matches how these messages are already written in
`settings.js`, and single-line messages simply get no body.

**No security origin in the text.** Chromium prefixes "example.com says" only for
content that is not the app's own; for a same-origin page it shows the bare
message. This application is entirely same-origin — its own `wails://` asset
handler, no iframes, no remote content — so `frame.securityOrigin` is
deliberately unused rather than overlooked.

**`nil`, not `@""`, on cancel.** WebKit maps a `nil` result to JavaScript `null`,
which is what `window.prompt` is specified to return and what Chromium hands the
Windows build. An empty string would *also* be falsy at every call site in
`settings.js`, so the bug would not show — until some later caller distinguishes
"cancelled" from "cleared the box", which is exactly the kind of divergence
between the two platforms this port is trying not to accumulate.

**Memory is manual.** The cgo build has no `-fobjc-arc` (see any
`#cgo CFLAGS: -x objective-c` in the package), so the alert and the accessory text
field are `autorelease`d at construction, per the Cocoa naming rules; AppKit keeps
the alert alive for as long as its sheet is attached. Verified rather than
assumed: with the harness told to raise a sheet and then never click it, the sheet
was still standing 20 s later and the completion handler had *not* fired.

Windows is untouched, for the same reason as 0001: this file is compiled only on
`darwin`, and WebView2 draws these three dialogs itself.

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
else. (The `-x templates` is the omission above.) To check the patch files still
match the tree, reverse the series **in reverse order** — 0002 first, because its
context sits on top of 0001's additions:

    cd third_party/wails-v2.13.0
    git apply --check -R -p1 ../patches/0002-wkuidelegate-js-panels.patch

`git apply --check -R` on 0001 alone is expected to FAIL against the patched
tree, and that is not a stale patch: 0002 inserts text immediately after 0001's
in both files, so 0001's trailing context no longer matches until 0002 is out of
the way. The check that means something is the round trip, and it should end in
silence:

    D=internal/frontend/desktop/darwin
    T=$(mktemp -d); mkdir -p "$T/$D"
    cp third_party/wails-v2.13.0/$D/WailsContext.[hm] "$T/$D/"
    for p in $(ls -r third_party/patches/000*.patch); do patch -R -p1 -d "$T" < "$p"; done
    for f in WailsContext.h WailsContext.m; do
      diff "$T/$D/$f" "$(go env GOMODCACHE)/github.com/wailsapp/wails/v2@v2.13.0/$D/$f"
    done

The recorded result of doing exactly that, 2026-08-14: both files came back
byte-identical to `v2.13.0` in the module cache, and applying 0001 then 0002 to a
pristine pair reproduced the vendored files byte-identically.

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

    # 2. Re-apply the patches, IN NUMBER ORDER — 0002's context includes lines
    #    0001 adds, so the order is not cosmetic. If either fails, fix it by hand
    #    against the new WailsContext.m and REGENERATE that .patch file — do not
    #    leave a stale one. Regenerate 0002 by diffing the finished tree against
    #    a pristine copy that has had ONLY 0001 applied, or you will fold the two
    #    patches into one and lose the ability to drop either independently.
    for p in third_party/patches/000*.patch; do
      (cd third_party/wails-$NEW && patch -p1 < "../../$p") || break
    done

    # 3. Point go.mod at it: bump BOTH the require line and the replace line.
    # 4. git rm -r third_party/wails-v2.13.0
    # 5. Build, and then actually check the dropdown AND the preset dialogs —
    #    see below. Both patches have a symptom you can see in ten seconds.

**Before doing any of that, check whether it is still needed** — patch by patch,
because they are independent bugs and upstream may close one and not the other.
If upstream has implemented a delegate method itself, or added an option for it,
drop that patch; if it has done both, drop this directory and the `replace` line
entirely, because a vendored dependency you no longer need is a liability.
Upstream tracking, both as of v2.13.0:
`internal/frontend/desktop/darwin/WailsContext.m` implements exactly one
`WKUIDelegate` method, `runOpenPanelWithParameters` — no media-capture callback
(0001) and no `alert`/`confirm`/`prompt` panels (0002).

## How to know it actually works

A build that compiles proves nothing here — the broken version compiled too.
There is one check per patch, and each takes seconds.

### 0002, the JavaScript panels

On the **Settings** page, select a preset and press **Rename**. A sheet must drop
out of the title bar with a text field in it, the current name selected, and the
caret already in the field so typing replaces it. Nothing appearing at all — the
button looking like it did nothing — is this bug, unfixed. Then check the four
others: **Apply** and **Delete** must each ask first, and **Save current as...**
must ask for a name.

From the DevTools console of the running app, without touching any preset:

    window.confirm('does this app draw dialogs?')

Fixed: a sheet appears and the console shows the answer you clicked. Broken: the
console prints `false` immediately with nothing on screen. `window.prompt('x')`
returning `null` instantly is the same failure.

Two behaviours are correct and can look wrong: **Cancel** on a prompt gives
`null` (not `""`), and OK on an emptied field gives `""`, which every current
caller treats as "no name given" and ignores. Both match Chromium, i.e. the
Windows build.

### 0001, the microphone

Run the real application and confirm the **Headphones** dropdown on the home
screen lists real device names. The one-line version, from the DevTools console of the
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
