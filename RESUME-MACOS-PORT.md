# macOS port — where we got to, and what to do next

Paused 2026-08-14 mid-integration. Not committed to `main`; all work is on the
branch `macos-port`.

## State

Two commits on `macos-port`, both in the MAIN repo's object store (so this
worktree living in `/tmp` is disposable — if `/tmp` is purged, run
`git worktree prune` in the main repo and `git checkout macos-port`):

    c17ae3a  Make the repo build, vet and test clean on macOS
    02d84a0  Port the application to macOS: devices, both SRT paths, picture, packaging

Verified green at 02d84a0:

    CGO_ENABLED=0 go build ./...        darwin OK
    CGO_ENABLED=0 go vet ./...          darwin OK
    CGO_ENABLED=0 go test ./... -count=1  12/12 packages ok
    GOOS=windows CGO_ENABLED=0 go build ./...   OK
    GOOS=windows CGO_ENABLED=0 go vet ./...     OK
    gofmt -l (excluding third_party/)   clean

A real notarised artifact exists and is preserved OUTSIDE this worktree at
`<main repo>/build/dist/wslcomms-0.1.0-macos-arm64.dmg` (gitignored, 17 MB).
Apple accepted it; `spctl` reports `source=Notarized Developer ID`. Regenerating
it costs one `build/ship-darwin.sh` run plus a notary round trip of a few
minutes, so it is convenient rather than precious.

## THE IMPORTANT CAVEAT

The six implementation agents each finished and self-reported green, but the
**integration pass and the adversarial verification pass did not run** — the run
was stopped before them. So:

- Nobody has reviewed the seams BETWEEN the six agents' work.
- Nobody has read `git diff` hunting for Windows behaviour changes smuggled into
  what should have been pure refactoring. That matters more than anything else
  here, because the Windows build is ON AIR.
- Nobody has checked whether the ~20 AST tests in `internal/gst/gst_stub_test.go`
  still assert something meaningful, or were quietly weakened to pass.

Gate A passing is necessary, not sufficient. Treat the whole diff as unreviewed.

## Next steps, in order

1. **Run the verification pass.** The workflow script that does it is at
   `~/.claude/projects/-Users-sam-Documents-WSL-Commentary/<session>/workflows/scripts/
   macos-port-build-wf_8c37416e-875.js` — its Integrate and Verify phases are
   written and were never reached. Re-running with `resumeFromRunId` replays the
   six implementers from cache and runs only those two phases.
2. **Read the Windows diff by hand.** `git diff 0155797..macos-port -- '*.go'`
   filtered to Windows-relevant code. The refactors claim to have moved the
   wasapi2 logic verbatim into `deviceprovider_windows.go` and the return tail
   into `return_cgo_windows.go`; `TestTheWindowsTailIsUnchangedByThePort` claims
   to pin the latter. Verify both claims rather than trusting them.
3. **Build and run on Windows.** Nothing here has been compiled by a real
   Windows toolchain, only cross-vetted. `CGO_ENABLED=1` with MinGW and the
   bundled GStreamer is the real test, and Gate B's smoke test is the standard.
4. ~~**Prune the bundle.**~~ **Do not prune the X11 libraries.** An earlier
   version of this list called `libX11.6.dylib`, `libX11-xcb.1.dylib`, `libXau`
   and `libXdmcp` "almost certainly dead weight". They are not. Measured with
   `otool -L` on the staged bundle:

       libgstopengl.dylib  ->  libX11.6, libgraphene-1.0.0, libjpeg.8
       libgstgl-1.0.0      ->  libX11.6, libX11-xcb.1, libxcb.1
       libxcb.1            ->  libXau.6, libXdmcp.6

   Homebrew builds gst-plugins-base's GL support with the X11/GLX window system
   compiled in alongside the Cocoa one, so these are hard `LC_LOAD_DYLIB`
   entries, not a `dlopen` the plugin could decline. `glimagesink` lives in
   `libgstopengl.dylib` and is the picture sink that was actually proven in a
   Wails `NSWindow`; delete its dependencies and the plugin fails to load and
   the sink is simply absent. The only way to drop them is to build GStreamer
   from source with `-Dgl_winsys=cocoa`, which means abandoning the Homebrew
   bottles the whole bundling approach rests on. All seven together are 1.85 MB
   of a 49 MB bundle — leave them. They are now enumerated, with licences, in
   `build/licenses/NOTICE.txt` section C2, which is where they should have been
   from the start: they were being redistributed and were not being attributed.
5. **Exercise the real hardware paths.** Everything to do with M2L-X was proven
   against loopback SRT and `cmd/mockm2lx`, never against a live instance. The
   send path, the return path and the picture all need a real run.

## Decisions already made, so they are not re-litigated

- **Keychain, via `zalando/go-keyring`.** Not a settings file. The password
  prompts seen during development are a cdhash artifact of UNSIGNED builds; a
  Developer ID-signed binary records a designated requirement instead and does
  not prompt.
- **`atenc`, not `fdkaacenc`.** Licence grounds — Homebrew says Apache-2.0 and
  `gst-inspect` says LGPL, and both are wrong. Measured indistinguishable anyway.
- **Bundle identifier `tv.wslstudios.commentary`**, signed by
  `Developer ID Application: Sygnal TV Ltd (5P76UVY5WF)`. The owner has
  explicitly accepted that a WSL Studios product carries the Sygnal signature.
- **CoreAudio device identity is the string UID**, resolved to the integer
  AudioDeviceID at pipeline-open time. The integer is a `coreaudiod` handle, not
  an identity, and must never be persisted.
- **`exit_darwin.go` is needed.** Measured: on darwin `os.Exit` and
  `syscall.Exit` both run atexit handlers and C++ static destructors; only
  SIGKILL to self skips them.

## Housekeeping

Today's test runs, before the isolation bug was fixed in c17ae3a, wrote a real
config tree into the developer's profile:

    ~/Library/Application Support/WSLComms/   config.json, mixer-golden.json,
                                              presets/ (11 files), remote/ (cert.pem, key.pem)
    ~/Library/Caches/WSLComms/logs/           12 log files

All machine-generated today. The cause is fixed; the files were left in place
pending the owner's decision.
