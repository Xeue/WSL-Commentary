#!/bin/bash
#
# One command from a clean tree to a signed, notarised, stapled,
# Gatekeeper-clean macOS artefact. The macOS equivalent of what
# build/installer.iss + bundle-gst.ps1 + pack-portable.ps1 do between them on
# Windows, except that here it is one script because there is one deliverable.
#
# Owner: WP-6 (macOS port). Specification section 11, read for darwin.
#
#   build/ship-darwin.sh [VERSION]
#
# Produces build/dist/wslcomms-<version>-macos-arm64.dmg — drag WSL
# Commentary.app to /Applications, double-click, and it runs on a Mac that has
# never had Homebrew, GStreamer, Xcode or anything else installed. That last
# clause is the whole requirement and everything below serves it.
#
# THE STAGES, and why they are in this order
#
#   0. PRE-FLIGHT. Everything that can be checked without building anything is
#      checked before anything is built. See the long comment on it: this is
#      the part that stops a release costing forty minutes to discover a
#      missing certificate.
#   1. BUILD. `wails build -tags production` with CGO_ENABLED=1 against the
#      build host's Homebrew GStreamer. Production, never dev — a dev binary
#      serves the frontend from a CWD-relative path and a double-clicked dev
#      .app exits 1, so a dev bundle can never be used to judge any of this.
#   2. NAME. wslcomms.app -> "WSL Commentary.app". Wails names the bundle from
#      wails.json's "name", which is the module name; the Finder shows the
#      bundle's filename, and the operator should see the product.
#   3. PAYLOAD. slate.png and the licence texts into the bundle.
#   4. VENDOR. build/bundle-gst-darwin.sh computes and stages the GStreamer
#      closure and relinks the app's own binary. AFTER the build, because it
#      rewrites that binary's load commands and a later `wails build` would
#      silently undo it; step 8's audit is the backstop for exactly that.
#   5. FLOOR. Reconcile the two deployment floors that meet here — the
#      product's, from build/darwin/Info.plist, and this particular payload's,
#      measured by stage 4 — and raise the SHIPPED key to the higher of them,
#      loudly. Nothing after this stage re-derives Info.plist.
#   6. SIGN, inside out.
#   7. NOTARISE the .app, staple it.
#   8. DMG, sign it, notarise it, staple it, and assess the finished artefact
#      with Gatekeeper.
#
# Env:
#   WSLCOMMS_SIGN_IDENTITY   default "Developer ID Application: Sygnal TV Ltd
#                            (5P76UVY5WF)". Must be an APPLICATION identity —
#                            there is no .pkg here, so the paired Installer
#                            certificate is not used and not looked for.
#   WSLCOMMS_NOTARY_PROFILE  default "sygnal-notary" — an `xcrun notarytool
#                            store-credentials` keychain profile.
#   SKIP_NOTARIZE=1          build, sign and package, but do not submit to
#                            Apple. The result is NOT shippable and the script
#                            says so at the end rather than quietly producing
#                            something that looks finished.
#   SKIP_FRONTEND=1          do not re-run npm install / npm run build. For
#                            iterating on this script, never for a release.
#
set -euo pipefail

[ "$(uname -s)" = "Darwin" ] || { echo "ship-darwin.sh only runs on macOS." >&2; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(dirname "$SCRIPT_DIR")"
cd "$REPO"

IDENTITY="${WSLCOMMS_SIGN_IDENTITY:-Developer ID Application: Sygnal TV Ltd (5P76UVY5WF)}"
PROFILE="${WSLCOMMS_NOTARY_PROFILE:-sygnal-notary}"

APP_NAME="WSL Commentary"
BIN_DIR="$REPO/build/bin"
APP="$BIN_DIR/$APP_NAME.app"
DIST="$REPO/build/dist"
ENTITLEMENTS="$SCRIPT_DIR/darwin/wslcomms.entitlements"
PLIST_SRC="$SCRIPT_DIR/darwin/Info.plist"

fail() { echo "" >&2; echo "ship-darwin.sh: $*" >&2; exit 1; }
step() { echo ""; echo "════ $*"; }

# ── Stage 0: pre-flight ─────────────────────────────────────────────────────
#
# THE POINT OF THIS SECTION is that a release must fail in the first five
# seconds or not at all. Everything after it is expensive: a cgo build, an npm
# build, a 49-file signing pass and two round trips to Apple's notary service
# that take minutes each and cannot be undone. A missing certificate or an
# unbumped version discovered at the end of that is the difference between a
# typo and an evening.
#
# The other half of the rule is that it must fail CLEARLY. Every check below
# says what to do about it, because the person running this at 16:00 on the day
# of an event is not necessarily the person who wrote it.
step "0. pre-flight"

for t in go npm wails gst-inspect-1.0 otool install_name_tool codesign xcrun hdiutil ditto shasum; do
    command -v "$t" >/dev/null 2>&1 || fail "$t is not on PATH.
       go/npm:            the normal toolchain, Go 1.25+.
       wails:             go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0
       gst-inspect-1.0:   brew install gstreamer  (BUILD host only — the shipped
                          .app contains its own copy and needs none of this)
       the rest:          Xcode command line tools, xcode-select --install"
done
[ -x /usr/libexec/PlistBuddy ] || fail "/usr/libexec/PlistBuddy is missing."

for f in "$ENTITLEMENTS" "$PLIST_SRC" "$SCRIPT_DIR/bundle-gst-darwin.sh" "$SCRIPT_DIR/forbidden-names.ps1" "$REPO/assets/slate.png" "$REPO/wails.json"; do
    [ -f "$f" ] || fail "missing $f — the build tree is incomplete."
done
[ -d "$REPO/build/licenses" ] || fail "missing build/licenses/ — the LGPL texts and the written offer must ship."

# The version. It lives in wails.json because that is what templates
# build/darwin/Info.plist, so there is one place to bump it and the plist
# cannot disagree with the artefact name. An argument may be given, and if it
# is, it must MATCH — that turns "I meant to bump the version" from a thing you
# discover after notarisation into a thing you discover now.
VERSION_JSON="$(/usr/bin/sed -n 's/.*"productVersion"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$REPO/wails.json" | head -1)"
[ -n "$VERSION_JSON" ] || fail "wails.json has no info.productVersion.
       That string is the version of the release, the CFBundleVersion and the
       CFBundleShortVersionString. It cannot be defaulted."
VERSION="${1:-$VERSION_JSON}"
if [ "$VERSION" != "$VERSION_JSON" ]; then
    fail "version mismatch: you asked for '$VERSION' but wails.json says '$VERSION_JSON'.
       Bump wails.json's info.productVersion — do not pass a version this
       script would then have to disagree with the bundle about."
fi
case "$VERSION" in
    *[!0-9.]*|""|.*|*.) fail "version '$VERSION' is not a plain dotted number. CFBundleVersion must be one." ;;
esac

# The signing identity, checked against the keychain rather than assumed. The
# grep is on the full quoted string including the team in brackets: two
# certificates whose names differ only by team is exactly the case where a
# loose match signs a release with the wrong one.
if ! security find-identity -v -p codesigning 2>/dev/null | grep -qF "$IDENTITY"; then
    echo "Available codesigning identities on this machine:" >&2
    security find-identity -v -p codesigning >&2 || true
    fail "no signing identity matching:
         $IDENTITY
       Import the Developer ID Application certificate and its private key into
       the login keychain, or set WSLCOMMS_SIGN_IDENTITY to one of the above.
       An 'Apple Development' certificate is NOT a substitute: it cannot be
       notarised and Gatekeeper will reject anything signed with it."
fi

# The notary credentials, checked with the cheapest call that proves them —
# about a second, against minutes for a submission that fails on auth.
if [ "${SKIP_NOTARIZE:-}" != "1" ]; then
    if ! xcrun notarytool history --keychain-profile "$PROFILE" >/dev/null 2>&1; then
        fail "notary keychain profile '$PROFILE' does not work.
       Create it once with:
         xcrun notarytool store-credentials $PROFILE \\
           --apple-id <apple-id> --team-id 5P76UVY5WF --password <app-specific-password>
       Or set SKIP_NOTARIZE=1 to produce a signed but UNSHIPPABLE bundle."
    fi
fi

echo "  version           $VERSION"
echo "  identity          $IDENTITY"
echo "  notary profile    $PROFILE$([ "${SKIP_NOTARIZE:-}" = "1" ] && echo "   (SKIPPED)")"
echo "  entitlements      ${ENTITLEMENTS#"$REPO"/}"
echo "  output            build/dist/wslcomms-$VERSION-macos-arm64.dmg"

# ── Stage 1: build ──────────────────────────────────────────────────────────
step "1. wails build (production, cgo, arm64)"
rm -rf "$BIN_DIR"
mkdir -p "$DIST"

# PKG_CONFIG_PATH is how cgo finds Homebrew's gstreamer-1.0.pc on a machine
# where /opt/homebrew/lib/pkgconfig is not already on the default path.
#
# CGO_LDFLAGS names UniformTypeIdentifiers explicitly. Wails' darwin frontend
# uses UTType symbols in its open-panel code but does not link the framework
# itself, so without this the final link fails on undefined symbols — measured,
# and it is the one flag in this script that looks arbitrary and is not.
#
# -tags production, never dev: see the header, and build/darwin/Info.dev.plist.
export CGO_ENABLED=1
export PKG_CONFIG_PATH="${PKG_CONFIG_PATH:-/opt/homebrew/lib/pkgconfig}"
export CGO_LDFLAGS="${CGO_LDFLAGS:--framework UniformTypeIdentifiers}"

# THE FRONTEND IS BUILT HERE, BY HAND, AND wails IS THEN TOLD TO SKIP IT.
#
# That looks like duplicated work and it is not. `wails build` does its stages
# in the order: generate bindings, build frontend, compile. Generating bindings
# COMPILES this module, main.go carries `//go:embed all:frontend/dist`, and on
# a clean checkout that directory does not exist yet — so a release from a
# fresh clone dies before the frontend it is about to build has been built:
#
#   Generating bindings: ERROR
#   main.go:63:12: pattern all:frontend/dist: no matching files found
#
# MEASURED on a pristine `git archive` of this repo. It does not reproduce in a
# working tree that has ever run a build, which is exactly why it would only
# ever be found by the person doing a real release on a clean machine.
#
# So: npm first, then `wails build -s`. The dependency is real and this is the
# order that respects it, rather than the order that happens to work.
if [ "${SKIP_FRONTEND:-}" = "1" ]; then
    echo "  (SKIP_FRONTEND=1 — reusing frontend/dist as it stands)"
    [ -f "$REPO/frontend/dist/index.html" ] || fail "SKIP_FRONTEND=1 but frontend/dist/index.html does not exist."
else
    ( cd "$REPO/frontend" && npm install && npm run build )
    [ -f "$REPO/frontend/dist/index.html" ] || fail "npm run build produced no frontend/dist/index.html."
fi

wails build -platform darwin/arm64 -tags production -clean -s

BUILT="$BIN_DIR/wslcomms.app"
[ -d "$BUILT" ] || fail "wails build produced no bundle at $BUILT."

# The Info.plist Wails just wrote came from build/darwin/Info.plist. Confirm
# that, rather than trusting it: if that file is ever deleted, Wails silently
# falls back to its own template AND rewrites the deleted file with it, which
# produces a bundle with no microphone string and the com.wails.* identifier
# and looks entirely successful.
GOT_ID="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$BUILT/Contents/Info.plist")"
[ "$GOT_ID" = "tv.wslstudios.commentary" ] || fail "the built bundle's CFBundleIdentifier is '$GOT_ID', not tv.wslstudios.commentary.
       That means Wails did not use build/darwin/Info.plist. Check that the
       file still exists and still parses as a template."
/usr/libexec/PlistBuddy -c 'Print :NSMicrophoneUsageDescription' "$BUILT/Contents/Info.plist" >/dev/null 2>&1 \
    || fail "the built bundle has no NSMicrophoneUsageDescription.
       macOS TERMINATES a process that touches the microphone without one. This
       is not a warning; do not ship past it."

# ── Stage 2: name ───────────────────────────────────────────────────────────
step "2. naming the bundle"
rm -rf "$APP"
mv "$BUILT" "$APP"
echo "  $APP_NAME.app"

# ── Stage 3: payload ────────────────────────────────────────────────────────
#
# slate.png goes in Contents/Resources, where macOS wants data, and is
# SYMLINKED into Contents/MacOS, where the Go code looks for it. Both halves
# are forced, and neither is a preference:
#
#   * app.go's slatePath resolves the configured (default: bare "slate.png")
#     path against appDir, and main.go's appDir is the directory holding the
#     executable — Contents/MacOS in a bundle.
#   * a non-Mach-O file in Contents/MacOS makes the bundle unsignable.
#     MEASURED: `codesign --force --sign …` on a bundle with slate.png sitting
#     next to the executable fails with "code object is not signed at all / In
#     subcomponent: …/Contents/MacOS/slate.png", and --verify then reports "a
#     sealed resource is missing or invalid".
#
# A symlink satisfies both: codesign seals it, --verify --deep --strict passes,
# and Go's os.Open follows it. The alternative was a darwin-specific branch in
# app.go to look in ../Resources, which is a change to somebody else's file to
# solve a packaging problem — worth doing if this ever grows a second data
# file, not worth doing for one.
step "3. staging the non-code payload"
cp "$REPO/assets/slate.png" "$APP/Contents/Resources/slate.png"
ln -sf "../Resources/slate.png" "$APP/Contents/MacOS/slate.png"
mkdir -p "$APP/Contents/Resources/licenses"
cp "$REPO"/build/licenses/*.txt "$APP/Contents/Resources/licenses/"
echo "  slate.png (Resources, symlinked into MacOS) + $(ls -1 "$APP/Contents/Resources/licenses" | wc -l | tr -d ' ') licence texts"

# ── Stage 4: vendor GStreamer ───────────────────────────────────────────────
step "4. vendoring GStreamer"
"$SCRIPT_DIR/bundle-gst-darwin.sh" "$APP"

# ── Stage 5: the deployment floor ───────────────────────────────────────────
#
# TWO NUMBERS MEET HERE, and they are different kinds of thing. Getting that
# wrong is what this stage now exists to prevent, in both directions.
#
#   PLIST_MIN, from build/darwin/Info.plist, is the PRODUCT's floor. It is
#   11.0: the architecture's own floor (arm64 macOS begins at Big Sur), which
#   is also the app binary's measured minos and is above every macOS API this
#   application calls. It is stable, it is derived, and it is checked in. See
#   note 3 in that file for the derivation.
#
#   FLOOR, from the bundle manifest stage 4 just wrote, is THIS PAYLOAD's
#   floor: the highest LC_BUILD_VERSION minos across the vendored GStreamer
#   closure. Homebrew builds its bottles for the build machine's macOS major
#   version, so it is a property of the machine this ran on and it moves when
#   that machine is upgraded. Today it is 26.0.
#
# LSMinimumSystemVersion is a PROMISE to the installer and to the Finder, and
# the artefact has to keep it, so the shipped key must be the HIGHER of the
# two. Promise lower than the payload can run on and the .app installs on a Mac
# it cannot launch on, dying in dyld before any of this project's code executes
# with a message no operator can act on.
#
# This used to be a refusal instead of a reconciliation, and the refusal is what
# put a build-machine accident into a checked-in file: the only way to satisfy it
# was to hand-edit Info.plist to 26.0, which is not true of the product and would
# have to be edited again on the next OS upgrade. So the shipped copy is raised
# here, where the measurement is, and said out loud — never silently, because a
# floor that moves without anyone noticing is how a release stops installing on
# the facility's Macs.
#
# ORDERING, checked rather than assumed: this runs after stage 4 (which creates
# the manifest) and before stage 6 (which seals the bundle), and nothing after
# it re-reads LSMinimumSystemVersion or re-derives Contents/Info.plist from
# build/darwin/Info.plist — the only later PlistBuddy call reads
# CFBundleExecutable. The manifest hashes Contents/Frameworks, Contents/MacOS
# and Contents/Resources/gstreamer-1.0 only, so mutating Contents/Info.plist
# does not invalidate it.
step "5. deployment floor"
MANIFEST="$APP/Contents/Resources/GST-BUNDLE-MANIFEST.txt"
FLOOR="$(sed -n 's/^MINOS-FLOOR=//p' "$MANIFEST" | head -1)"
# `|| true` and the discarded stderr are load-bearing under `set -e`: PlistBuddy
# exits 1 with "Entry … Does Not Exist" on a missing key (measured), and an
# unguarded command substitution would abort the script with that message
# instead of the one below, which says what to do about it.
PLIST_MIN="$(/usr/libexec/PlistBuddy -c 'Print :LSMinimumSystemVersion' \
    "$APP/Contents/Info.plist" 2>/dev/null || true)"
[ -n "$FLOOR" ] || fail "the bundle manifest has no MINOS-FLOOR line."
[ -n "$PLIST_MIN" ] || fail "the built bundle's Info.plist has no LSMinimumSystemVersion.
       build/darwin/Info.plist sets it; if it is missing, Wails wrote its own
       template over that file. See the header comment in build/darwin/Info.plist."
LOWEST="$(printf '%s\n%s\n' "$FLOOR" "$PLIST_MIN" | sort -V | head -1)"
if [ "$LOWEST" != "$FLOOR" ]; then
    /usr/libexec/PlistBuddy -c "Set :LSMinimumSystemVersion $FLOOR" "$APP/Contents/Info.plist"
    echo "  the product's floor is macOS $PLIST_MIN, but the VENDORED GStreamer in this"
    echo "  build requires macOS $FLOOR, so the shipped Info.plist now says $FLOOR."
    echo ""
    echo "  That is a property of the BUILD HOST, not of this product: Homebrew builds"
    echo "  its bottles for the build machine's macOS major version. To ship a lower"
    echo "  floor, build the GStreamer closure on a machine running the oldest macOS"
    echo "  you intend to support. Do NOT edit LSMinimumSystemVersion in"
    echo "  build/darwin/Info.plist to chase this number — that field is the product's"
    echo "  floor and this stage is what reconciles it with a particular build."
else
    echo "  payload requires macOS $FLOOR, Info.plist promises $PLIST_MIN — consistent"
fi

# ── Stage 6: signing, inside out ────────────────────────────────────────────
#
# ORDER IS EVERYTHING. Signing a bundle SEALS everything under it, so anything
# nested must be signed first; sign the outer bundle and then touch a dylib
# inside it and the outer signature no longer matches what is on disk.
#
# NEVER --deep for signing. It applies one entitlement set to everything it
# finds, and it hides which files it did and did not reach. It is a
# VERIFICATION flag and it is used as one, below.
#
# WALKED, NOT ENUMERATED. `find` over the whole bundle rather than a list of
# directories. A named list is correct only until somebody adds a directory,
# and the failure mode is not a build error — it is an unsigned dylib that
# sails through signing, gets rejected by the notary service after a round
# trip, or worse, gets accepted and then refuses to load on the customer's Mac
# because library validation sees a signature that is not ours.
#
# The nested-bundle exclusions look pointless because this .app contains no
# .framework and no nested .app today. They are there so that the day one
# arrives it is a decision someone makes rather than a set of Mach-Os that
# quietly get signed as loose files inside a bundle that then will not verify.
#
# `find` into a temp file, not a process substitution, and the reason is real:
# process substitution runs find CONCURRENTLY with the loop, and `codesign
# --force` re-signs in place via a `<name>.cstemp` rename — a find still
# scanning a directory an earlier iteration is signing can hand that transient
# .cstemp to codesign, which then fails when the rename completes underneath
# it. A plain temp file is a completed snapshot.
#
# RETRIES, and they are not defensive programming for its own sake. A secure
# timestamp is a live network call to Apple's timestamp authority, once per
# Mach-O — 48 of them in this bundle — and it flakes. Observed on the first
# real run of this script:
#
#   .../Contents/Frameworks/libXau.6.dylib: A timestamp was expected but was
#   not found.
#
# A signature without a timestamp is not merely untidy: it stops being valid
# the day the signing certificate expires, and the notary service rejects it.
# So the flag is never dropped and the call is retried instead. Failing after
# three attempts is a real network problem and should stop the release.
sign_macho() {
    local target="$1"; shift
    local attempt
    for attempt in 1 2 3; do
        if codesign --force --options runtime --timestamp "$@" --sign "$IDENTITY" "$target" 2>&1; then
            return 0
        fi
        echo "  timestamp/sign attempt $attempt failed for $(basename "$target") — retrying in 5s" >&2
        sleep 5
    done
    fail "could not sign $target after three attempts.
       If the message mentioned a timestamp, Apple's timestamp authority is
       unreachable or refusing. Do not work around it by dropping the
       timestamp: an untimestamped signature expires with the certificate and
       the notary service will not accept it."
}

step "6. signing (inside out, hardened runtime)"
LIST="$(mktemp)"
trap 'rm -f "$LIST"' EXIT
find "$APP" -type f ! -path "*.app/Contents/*.app/*" ! -path "*.framework/*" -print0 > "$LIST"

SIGNED=0
while IFS= read -r -d '' f; do
    [ "$f" = "$APP/Contents/MacOS/$(/usr/libexec/PlistBuddy -c 'Print :CFBundleExecutable' "$APP/Contents/Info.plist")" ] && continue
    file "$f" 2>/dev/null | grep -q 'Mach-O' || continue
    sign_macho "$f" >/dev/null
    SIGNED=$((SIGNED + 1))
done < "$LIST"
echo "  $SIGNED nested Mach-O files signed (no entitlements — they need none)"

# The outer bundle last. This is also what signs the main executable, and it is
# the only thing that carries entitlements: see build/darwin/wslcomms.entitlements
# for why the set is as small as it is, and in particular for the measurement
# that says disable-library-validation is NOT needed here.
sign_macho "$APP" --entitlements "$ENTITLEMENTS"
echo "  outer bundle signed with $(basename "$ENTITLEMENTS")"

step "6b. verifying the signature and the isolation"
# --deep IS right here and only here: this is verification, it is read-only,
# and walking the whole tree is the entire point. Quiet, because a hundred
# prepared/validated lines is not a result — the result is the exit status and
# the four facts printed below it.
codesign --verify --deep --strict "$APP"
echo "  signature valid, seal intact"
codesign -dv --entitlements - "$APP" 2>&1 | grep -E 'Identifier=|TeamIdentifier=|Authority=Developer ID|flags=|audio-input' | sed 's/^/  /' || true

# The audit again, on the FINISHED, SIGNED artefact rather than on the staged
# one. The bundler already ran it, and this is not paranoia about the bundler:
# it is the check that catches a `wails build` run after stage 4, which would
# replace the relinked executable with a fresh Homebrew-linked one and undo the
# entire point of the exercise while leaving every other stage looking fine.
find "$APP" -type f -print0 > "$LIST"
LEAKS=""
while IFS= read -r -d '' f; do
    file "$f" 2>/dev/null | grep -q 'Mach-O' || continue
    h="$(otool -L "$f" 2>/dev/null | tail -n +2 | awk '{print $1}' | grep -E '^/opt/homebrew' || true)"
    r="$(otool -l "$f" 2>/dev/null | awk '/LC_RPATH/{x=1} x&&/ path /{print $2; x=0}' | grep -E '^/opt/homebrew' || true)"
    [ -n "$h$r" ] && LEAKS="$LEAKS
  ${f#"$APP"/}: $h$r"
done < "$LIST"
[ -z "$LEAKS" ] || fail "the signed bundle still references Homebrew:$LEAKS
       Almost certainly a build was run after stage 4. Re-run this script."
echo "  self-contained: zero /opt/homebrew load commands in the signed bundle"

# ── Stage 7: notarise the .app ──────────────────────────────────────────────
#
# The .app is notarised and stapled SEPARATELY from the .dmg, which costs a
# second round trip and is worth it. Stapling the disk image alone puts the
# ticket on the image; the moment the operator drags the app out of it, the
# copy on their disk has no ticket of its own and Gatekeeper must reach Apple
# to assess it. At a venue on a bonded uplink, or on a machine with no route
# out at all, that is a first launch that hangs and then refuses. A stapled app
# inside a stapled image is assessable entirely offline.
# WHAT IS SUBMITTED AND WHAT IS STAPLED ARE NOT THE SAME PATH, and getting
# that wrong is the one mistake this function exists to make impossible.
# notarytool takes an archive; the ticket is stapled to the ARTEFACT. For the
# disk image those are the same file. For the .app they are not — an .app has
# to be zipped to be uploaded, and stapling the zip fails outright:
#
#   Processing: .../wslcomms-0.1.0-macos-arm64.zip
#   Stapler is incapable of working with ZIP archive files.
#
# (measured, on the first run that got this far — the submission itself was
# already Accepted at that point, so this cost a five-minute round trip to find
# out). So the caller passes both, and the zip is a transport wrapper that is
# deleted afterwards.
notarize() {
    local what="$1" submit="$2" staple="$3" out id status
    echo "  submitting $(basename "$submit") — this takes minutes, and waiting is the point"
    out="$(xcrun notarytool submit "$submit" --keychain-profile "$PROFILE" --wait 2>&1)" || {
        echo "$out" >&2
        fail "notarytool submit failed for $what."
    }
    echo "$out" | sed 's/^/    /'
    id="$(echo "$out" | sed -n 's/^ *id: \([0-9a-f-]*\)$/\1/p' | head -1)"
    status="$(echo "$out" | sed -n 's/^ *status: \(.*\)$/\1/p' | tail -1)"
    if [ "$status" != "Accepted" ]; then
        # The verdict alone never says WHY. The log does, and fetching it here
        # means the failure is diagnosable from this script's own output rather
        # than from a submission id somebody has to go and look up.
        echo "  fetching the notary log for $id" >&2
        xcrun notarytool log "$id" --keychain-profile "$PROFILE" >&2 || true
        fail "$what was not accepted (status: $status)."
    fi
    xcrun stapler staple "$staple"
    xcrun stapler validate "$staple"
}

ZIP="$DIST/wslcomms-$VERSION-macos-arm64.zip"
if [ "${SKIP_NOTARIZE:-}" = "1" ]; then
    step "7. notarising the .app — SKIPPED (SKIP_NOTARIZE=1)"
else
    step "7. notarising the .app"
    # ditto, not zip(1). The Info-ZIP `zip` on this machine does not preserve
    # the symlink at Contents/MacOS/slate.png or the extended attributes the
    # signature lives in, and the notary service rejects the result. ditto's
    # --keepParent is what puts the .app itself inside the archive rather than
    # its contents.
    rm -f "$ZIP"
    ditto -c -k --keepParent "$APP" "$ZIP"
    notarize "the .app" "$ZIP" "$APP"
    rm -f "$ZIP"
    echo "  ticket stapled to $APP_NAME.app"
fi

# ── Stage 8: the disk image ─────────────────────────────────────────────────
#
# A .dmg rather than a .pkg, and rather than a bare zip. A .pkg would mean an
# installer, an admin prompt and a second certificate for a product that
# installs nothing, registers no service and runs nothing in the background —
# the Windows side ships an installer because Windows expects one, and macOS
# does not. The /Applications symlink beside the app is the drag-and-drop
# convention every Mac user already knows.
step "8. disk image"
DMG="$DIST/wslcomms-$VERSION-macos-arm64.dmg"
STAGE="$(mktemp -d)"
cp -R "$APP" "$STAGE/"
ln -s /Applications "$STAGE/Applications"
rm -f "$DMG"
# THE VOLUME NAME MUST NOT EQUAL THE BUNDLE NAME. This used to be -volname
# "$APP_NAME", i.e. "WSL Commentary", while the bundle inside it is
# "WSL Commentary.app" — and hdiutil then fails the whole build with
#
#	could not access /Volumes/WSL Commentary/WSL Commentary.app - Operation not permitted
#	hdiutil: create failed - Operation not permitted
#
# It is a name collision, not a permissions problem, and it was worth pinning
# down because "Operation not permitted" on macOS 26 reads like TCC and sends
# you looking for a Full Disk Access grant that changes nothing. Measured, three
# ways: the same bundle into a volume called "WSL Commentary 0.1.0" succeeds;
# a bundle renamed plain.app into a volume called "WSL Commentary" succeeds;
# the two together fail every time. Appending the version also gives the mounted
# volume a name that says which build is being installed, which is worth having
# on a machine that has mounted three of them this week.
hdiutil create -volname "$APP_NAME $VERSION" -srcfolder "$STAGE" -fs HFS+ -format UDZO -ov "$DMG" >/dev/null
rm -rf "$STAGE"
# The disk image is signed too, but WITHOUT the hardened runtime and without
# entitlements: it is a container, not code, and codesign's runtime option on a
# non-code artefact is meaningless. What the signature buys is that Gatekeeper
# can assess the image itself before anything is copied out of it.
for attempt in 1 2 3; do
    codesign --force --timestamp --sign "$IDENTITY" "$DMG" && break
    [ "$attempt" = 3 ] && fail "could not sign $DMG after three attempts."
    echo "  dmg sign attempt $attempt failed — retrying in 5s" >&2
    sleep 5
done
echo "  $(basename "$DMG")  $(du -h "$DMG" | awk '{print $1}')"

if [ "${SKIP_NOTARIZE:-}" != "1" ]; then
    notarize "the .dmg" "$DMG" "$DMG"
fi

# ── Stage 9: assess what was actually produced ──────────────────────────────
#
# spctl, not our own belief about what the previous stages did. This is the
# same question Gatekeeper asks on the customer's Mac, asked here.
step "9. Gatekeeper assessment"
echo "  --- the app ---"
spctl -a -vvv --type exec "$APP" 2>&1 | sed 's/^/  /'
echo "  --- the disk image ---"
spctl -a -vvv --type open --context context:primary-signature "$DMG" 2>&1 | sed 's/^/  /'

echo ""
echo "════ done"
echo "  $DMG"
echo "  sha256 $(shasum -a 256 "$DMG" | awk '{print $1}')"
if [ "${SKIP_NOTARIZE:-}" = "1" ]; then
    echo ""
    echo "  NOT NOTARISED (SKIP_NOTARIZE=1). This artefact is signed but it is NOT"
    echo "  shippable: Gatekeeper will refuse it on any Mac but this one. Re-run"
    echo "  without SKIP_NOTARIZE to produce a release."
fi
