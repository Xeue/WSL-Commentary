#!/bin/bash
#
# Stages the hand-derived GStreamer runtime INTO an already-built .app bundle,
# relinked so that nothing it loads comes from Homebrew. The macOS twin of
# build/bundle-gst.ps1.
#
# Owner: WP-6 (macOS port). Specification sections 3 and 11, read for darwin.
#
# WHY THIS SCRIPT EXISTS AT ALL
# -----------------------------
# The owner's requirement is absolute: the shipped .app contains everything it
# needs. Nobody installs Homebrew. That is not a preference — a commentary
# operator gets a .dmg the afternoon before an event and drags it to
# /Applications, and "brew install gstreamer" is not a step that can exist.
#
# The Windows twin solves this with an EXPLICIT FILE LIST, because on Windows
# the DLL names are stable and knowable and the licensing control is the list
# itself. That approach cannot be transplanted here: Homebrew's dylib set is
# versioned per keg (libjpeg.8, libicuuc.78, libbrotlidec.1 …), the names move
# under you at every `brew upgrade`, and a stale hand-written list fails as a
# missing dylib at LAUNCH on the customer's Mac rather than at build time on
# ours. So this script COMPUTES the file list instead:
#
#   1. every element name this repo's Go code actually asks for, resolved to
#      its owning plugin .dylib by gst-inspect (not guessed);
#   2. the transitive `otool -L` closure of those plugins, of the plugin
#      scanner, AND of the app's own cgo-linked binary;
#   3. copied in, with every LC_ID_DYLIB, every Homebrew-rooted LC_LOAD_DYLIB
#      and every Homebrew-rooted LC_RPATH rewritten to be relative to the
#      loading file, and each rewritten Mach-O ad-hoc re-signed;
#   4. audited — the build FAILS if one /opt/homebrew reference survives;
#   5. proven — every wanted element is looked up again through a copy of
#      gst-inspect that is itself inside the bundle, with `env -i`, so a
#      Homebrew fallback cannot be what made the lookup succeed.
#
# The licensing control the Windows list gave away for free is bought back in
# step 2.5: the computed closure is matched against the SAME forbidden-name
# list build/bundle-gst.ps1 uses — parsed out of build/forbidden-names.ps1
# rather than restated here, because a security-relevant list kept in two
# places is a list that eventually differs in two places.
#
# WHAT WAS TAKEN FROM WHERE
# -------------------------
# The TECHNIQUE — resolve elements to plugins, walk otool transitively, rewrite
# to @loader_path, ad-hoc re-sign, fail on a surviving Homebrew reference — is
# adapted from a proven bundler in another product of the same author's
# (SYGNALview's deploy/scripts/bundle-gstreamer-darwin.sh). NONE of that
# product's shape is: this is a self-contained script in this repo, it targets
# an .app rather than a lib/ + libexec/ payload for a launchd daemon, it names
# no path outside this tree, and it carries none of that product's identifiers.
# WSL Commentary is a separate product for a different client and there is
# deliberately no build-time dependency between the two.
#
# One place this is STRICTER than the script it learned from: that one audits
# LC_LOAD_DYLIB only. Homebrew's libjpeg.8 also carries an LC_RPATH of
# /opt/homebrew/Cellar/jpeg-turbo/<v>/lib, which that audit would have shipped.
# Nothing in today's closure resolves an @rpath-relative load command, so it is
# inert — but it is a live search path the moment any future dependency does,
# and it is a Homebrew path inside a signed, shipped, notarised Mach-O either
# way. It is stripped and audited for.
#
# WHAT THIS SCRIPT DELIBERATELY DOES NOT DO
# -----------------------------------------
# It does not sign anything with a real identity, it does not build the .app,
# and it does not make a .dmg. build/ship-darwin.sh does all three and calls
# this script in the middle. The ad-hoc signatures applied here exist only
# because install_name_tool INVALIDATES a signature and arm64 refuses to load
# an invalidly-signed Mach-O at all — they are scaffolding, replaced wholesale
# by the Developer ID pass in ship-darwin.sh.
#
# Usage:
#   build/bundle-gst-darwin.sh <path-to-App.app>
#
# Env:
#   GST_KEG    override the Homebrew gstreamer keg root. The default is derived
#              from gst-inspect's OWN reported plugin Filename — the Cellar
#              directory GStreamer itself resolved — not `brew --prefix`, which
#              is only the /opt/homebrew/opt symlink.
#   KEEP_TOOLS=0
#              do not ship gst-inspect-1.0 / gst-launch-1.0 in Contents/MacOS.
#              They cost 0.2 MB and they are the only way to diagnose a plugin
#              problem on a machine that has no GStreamer; the hermetic proof
#              in step 5 is also impossible without them. Off is for size
#              experiments, not for shipping.
#
set -euo pipefail

[ "$(uname -s)" = "Darwin" ] || { echo "bundle-gst-darwin.sh only runs on macOS." >&2; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

APP="${1:-}"
[ -n "$APP" ] || { echo "Usage: bundle-gst-darwin.sh <path-to-App.app>" >&2; exit 1; }
[ -d "$APP" ] || { echo "Error: no such bundle: $APP" >&2; exit 1; }

# THE LAYOUT, AND THE ONE THING THAT DICTATES IT
# ----------------------------------------------
#   Contents/MacOS/<exe>                     the app
#   Contents/MacOS/gst-plugin-scanner        spawned by libgstreamer
#   Contents/MacOS/gst-{inspect,launch}-1.0  field diagnostics, 0.2 MB
#   Contents/Frameworks/*.dylib              the core runtime, FLAT
#   Contents/Resources/gstreamer-1.0/*.dylib the plugins
#   Contents/Resources/gio-modules/          deliberately empty, see step 3.6
#
# The plugins do NOT live in Contents/Frameworks/gstreamer-1.0, which is where
# anyone would put them and where this script put them for its first hour of
# life. MEASURED, and it is fatal rather than untidy:
#
#   $ codesign --force --sign - build/bin/wslcomms.app
#   build/bin/wslcomms.app: bundle format unrecognized, invalid, or unsuitable
#   In subcomponent: .../Contents/Frameworks/gstreamer-1.0
#
# codesign treats every DIRECTORY under Contents/Frameworks as nested code and
# requires it to be a bundle — a .framework or a .app. A plain directory of
# dylibs there is not one, so the whole .app cannot be signed at all. The same
# is true of Contents/PlugIns. Contents/Resources has no such rule: its
# contents are sealed by hash into CodeResources, and a subdirectory of dylibs
# in it signs, verifies `--deep --strict` and notarises.
#
# Flat dylibs directly in Contents/Frameworks are fine and stay there, because
# that is the one location Apple's own layout expects them and because pointing
# GST_PLUGIN_PATH at Frameworks instead would make the plugin scanner try to
# load all twenty-eight core dylibs as plugins and log a failure for each.
MACOS_DIR="$APP/Contents/MacOS"
FW_DIR="$APP/Contents/Frameworks"
RES_DIR="$APP/Contents/Resources"
PLUGIN_DIR="$RES_DIR/gstreamer-1.0"
GIO_DIR="$RES_DIR/gio-modules"
MANIFEST="$RES_DIR/GST-BUNDLE-MANIFEST.txt"

[ -d "$MACOS_DIR" ] || { echo "Error: $APP has no Contents/MacOS — is it a bundle?" >&2; exit 1; }

# The app's own binary. It is cgo-linked against Homebrew's libgstreamer,
# libglib, libgobject, libgstbase, libgstvideo and libintl, and those are
# LC_LOAD_DYLIBs resolved by dyld BEFORE main() runs — no amount of environment
# setting inside Init can affect them. So the binary is both a SEED of the
# closure (it may want something no plugin wants) and a TARGET of the rewrite.
# Finding it by CFBundleExecutable rather than by assuming the name keeps this
# working if wails.json's outputfilename ever changes.
APP_EXE_NAME="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleExecutable' "$APP/Contents/Info.plist" 2>/dev/null || true)"
APP_EXE=""
if [ -n "$APP_EXE_NAME" ] && [ -f "$MACOS_DIR/$APP_EXE_NAME" ]; then
    APP_EXE="$MACOS_DIR/$APP_EXE_NAME"
fi

for t in gst-inspect-1.0 otool install_name_tool codesign shasum; do
    command -v "$t" >/dev/null 2>&1 || { echo "Error: $t not on PATH." >&2; exit 1; }
done

_CORE="$(gst-inspect-1.0 filesrc 2>/dev/null | grep -E '^[[:space:]]*Filename[[:space:]]+/' | sed 's#.*Filename[[:space:]]*##' || true)"
[ -n "$_CORE" ] || { echo "Error: gst-inspect-1.0 filesrc found nothing — is GStreamer installed? (brew install gstreamer)" >&2; exit 1; }
GST_KEG="${GST_KEG:-$(cd "$(dirname "$_CORE")/../.." && pwd -P)}"
SCANNER_SRC="$GST_KEG/libexec/gstreamer-1.0/gst-plugin-scanner"
[ -f "$SCANNER_SRC" ] || { echo "Error: gst-plugin-scanner not found at $SCANNER_SRC" >&2; exit 1; }

echo "==> bundling GStreamer into $APP"
echo "    keg:        $GST_KEG"
echo "    app binary: ${APP_EXE:-(none in place yet)}"

# A re-run must not leave a dylib behind that the new closure no longer wants:
# a stale plugin in the tree is a plugin that gets signed, notarised and
# shipped. Only the directories this script owns are cleared.
rm -rf "$FW_DIR" "$PLUGIN_DIR" "$GIO_DIR"
mkdir -p "$FW_DIR" "$PLUGIN_DIR" "$GIO_DIR" "$RES_DIR"

# ── Step 0: the licensing control, parsed before anything is resolved ───────
#
# build/forbidden-names.ps1 is the project's single list of names that must
# never appear in anything it redistributes — x264 first among them, because
# shipping it would place the whole combined work under the GPL. That file is
# PowerShell and this script is not, so the choice was between restating the
# list here and parsing it. Restating it loses the property the file's own
# header states as its reason for existing ("it lives here so there is exactly
# one of it"), so this parses it: the single-quoted glob literals out of the
# $ForbiddenPatterns array, stars stripped, joined into one grep alternation.
#
# If the parse yields nothing the build FAILS. A licensing control that
# silently degrades to "allow everything" when its input moves is worse than no
# control at all, because it still prints a reassuring line.
#
# It is parsed HERE, before step 1, rather than where it was first applied,
# because build/bundle-gst.ps1 applies the same list at THREE points and this
# script has to as well:
#
#   1. refuse to run if a WANTED name matches            (step 1)
#   2. refuse if the computed closure contains a match   (step 2.5)
#   3. fail after staging if a match is in the output    (step 3.7)
#
# Two of those were missing here, and the gap was not theoretical on this
# platform the way it is on Windows: x264enc is PRESENT in a stock Homebrew
# GStreamer and ranks primary (256), which is the same rank as vtenc_h264. It is
# excluded by gst_cgo.go's encoder denylist at run time, but a denylist in Go is
# not a licensing control over what gets signed and notarised. Only the file
# gate is.
FORBIDDEN_PS1="$SCRIPT_DIR/forbidden-names.ps1"
[ -f "$FORBIDDEN_PS1" ] || { echo "Error: $FORBIDDEN_PS1 not found — the shared licensing control is missing." >&2; exit 1; }
FORBIDDEN="$(sed -n "s/^[[:space:]]*'\(.*\)'.*$/\1/p" "$FORBIDDEN_PS1" | tr -d '*' | grep -v '^$' | paste -sd'|' -)"
FORBIDDEN_N=$(printf '%s' "$FORBIDDEN" | tr '|' '\n' | grep -c . || true)
if [ -z "$FORBIDDEN" ] || [ "$FORBIDDEN_N" -lt 10 ]; then
    echo "Error: parsed only $FORBIDDEN_N patterns out of $FORBIDDEN_PS1 (expected the full list)." >&2
    echo "       Do not 'fix' this by inlining the patterns here. Fix the parse." >&2
    exit 1
fi
echo "==> licence gate: $FORBIDDEN_N patterns parsed from forbidden-names.ps1"

# ── Step 1: WANTED_ELEMENTS ─────────────────────────────────────────────────
#
# Every name here is named by this repo's Go code. Where the Windows name has
# no darwin twin the substitution is spelled out, with the reason, because the
# next person to read this list will be looking for wasapi2src and mfaacenc.
#
# From internal/gst/gst_cgo.go `requiredElements` — the CONTRIBUTION path, the
# list whose absence makes Init fail and the app refuse to start:
#
#   filesrc queue capsfilter pngdec imagefreeze videoconvert videoscale
#   audioconvert audioresample h264parse aacparse level mpegtsmux srtsink
#     — identical factory names on both platforms, all 14 of them.
#   wasapi2src  -> osxaudiosrc   the CoreAudio capture source. Its "device"
#                                property is an INTEGER AudioDeviceID, not a
#                                string; that is internal/gst's problem, not
#                                this script's, but it is why the substitution
#                                is not a rename.
#   mfaacenc    -> atenc         AudioToolbox AAC, the exact analogue of the
#                                Media Foundation encoder: a thin LGPL wrapper
#                                over an encoder that is PART OF THE OS. That
#                                keeps NOTICE.txt section G's "no third-party
#                                AAC implementation and no separate AAC patent
#                                licence is involved on our side" literally
#                                true on macOS, and it costs the bundle zero
#                                extra files because atenc lives in the same
#                                libgstosxaudio.dylib as osxaudiosrc.
#                                fdkaacenc is RULED OUT on licence (Homebrew's
#                                metadata says Apache-2.0, gst-inspect says
#                                LGPL, and both are wrong about the source).
#                                faac and anything libav-derived are forbidden
#                                outright — see step 2.5.
#
# The H.264 encoder is deliberately NOT in gst_cgo.go's list because it is
# resolved by rank at run time (specification open question 3). It still has to
# be IN THE BUNDLE, so both VideoToolbox spellings are wanted here: vtenc_h264
# is the software/any path and vtenc_h264_hw is the hardware-only one. They
# share libgstapplemedia.dylib with the decoder below, so this is one file.
#
# From internal/gst/return_cgo.go `returnRequiredElements` — the RETURN
# monitor, checked at pipeline-open rather than at Init:
#
#   srtsrc tsdemux queue fakesink aacparse audioconvert audioresample
#     — identical on both platforms.
#   mfaacdec    -> atdec         AudioToolbox again, same reasoning as atenc.
#                                It needs a capsfilter between aacparse and
#                                itself (stream-format=raw) or it decodes to
#                                digital silence; capsfilter is already wanted
#                                by the send path.
#   wasapi2sink -> osxaudiosink  CoreAudio playback.
#
# From internal/gst/picture_cgo.go `pictureRequiredElements` — the native
# picture overlay:
#
#   srtsrc tsdemux queue fakesink h265parse
#     — identical on both platforms.
#   d3d11h265dec  -> vtdec_hw    VideoToolbox hardware decode, with vtdec as
#                   and vtdec    the fallback the darwin candidate list in
#                                picture_cgo.go actually names. Both live in
#                                libgstapplemedia.dylib alongside the encoders,
#                                so naming the fallback costs no extra file and
#                                buys the hermetic proof covering the path the
#                                code will really take if the hardware decoder
#                                is unavailable.
#   d3d11videosink-> glimagesink VideoToolbox covers both codecs on darwin, so
#                   and          unlike d3d11 the decoder and the sink come
#                   osxvideosink from different plugins here. glimagesink is
#                                the sink PROVEN to work in a real Wails
#                                NSWindow (it implements GstVideoOverlay and
#                                accepted an NSView* through
#                                gst_video_overlay_set_window_handle, reached
#                                PLAYING and passed 400 buffers).
#                                caopengllayersink failed to link in the same
#                                harness and is not wanted here. osxvideosink
#                                is the second candidate in picture_cgo.go's
#                                darwin list and it is NOT in the same plugin —
#                                libgstosxvideo.dylib is a file this bundle
#                                would not otherwise contain, and a fallback
#                                that is not in the bundle is not a fallback.
#
# THE DECKLINK CAPTURE PATH — three plugins, added 2026-08-16
# -----------------------------------------------------------
# A commentary position in a sports facility takes its programme feed off SDI,
# not off the machine's own audio stack, and that means a Blackmagic card. The
# elements below were in NEITHER bundler until this change: `grep -c decklink`
# returned 0 for this script and for build/bundle-gst.ps1 alike, so however good
# the Go side got, the shipped app could not use a DeckLink at all on either
# platform.
#
#   decklinkvideosrc  the SDI video capture source, and
#   decklinkaudiosrc  the embedded-audio source. ONE FILE, libgstdecklink.dylib,
#                     so naming both costs nothing and makes the hermetic proof
#                     at step 5 cover the audio leg too. The audio leg is not
#                     wired up in the Go code yet (app.go says so, and
#                     CONTRACT.md records the single line that refuses it), but
#                     a fallback — or a next leg — that is not in the bundle is
#                     not one, which is the same reasoning that puts vtdec and
#                     osxvideosink in this list.
#
#   videorate         MANDATORY, and not a nicety. MEASURED, 3/3 runs:
#                     decklinkvideosrc emits the 720x486 NTSC PLACEHOLDER as its
#                     first buffer on EVERY start, and the real caps arrive
#                     ~170 ms later. A fixed capsfilter downstream of it and
#                     nothing else therefore sees a frame it cannot accept
#                     before it ever sees a real one, and the pipeline dies in
#                     0.088 s with not-negotiated (-4). videorate is what
#                     absorbs that first buffer and the rate change behind it.
#                     Leaving it out does not degrade the capture; it prevents
#                     it, in under a tenth of a second, with an error message
#                     about caps that names no plugin.
#
#   deinterlace       the only element in this bundle that can handle a 1080i50
#                     camera, which is a likely feed in a UK sports facility and
#                     is handled NOWHERE today. Everything downstream of the
#                     capture in this product's pipeline is progressive.
#
# All three are LGPL — verified with gst-inspect against 1.26.10, not assumed:
# decklink is gst-plugins-bad, videorate gst-plugins-base, deinterlace
# gst-plugins-good, and all three report License LGPL. Note in passing that
# deinterlace's own description says "Deinterlace Methods ported from
# DScaler/TvTime"; the plugin as GStreamer ships it is LGPL and that is what the
# licence gate and NOTICE.txt section A record.
#
# THE PART THAT IS NOT A BUILD PROBLEM AND WILL BE FORGOTTEN ANYWAY.
# libgstdecklink.dylib has NO load command naming anything Blackmagic. MEASURED:
# it carries the string /Library/Frameworks/DeckLinkAPI.framework together with
# the symbols InitDeckLinkAPI, IsDeckLinkAPIPresent and gDeckLinkAPIBundleRef,
# which is to say it opens that framework as a CFBundle AT RUN TIME, from an
# absolute path outside the .app. Three consequences, none of them obvious:
#
#   1. it costs this closure nothing. otool cannot see a CFBundle path, so
#      step 2 does not follow it, step 4 does not audit it and the bundle does
#      not grow by a byte on its account;
#   2. it cannot be bundled even if we wanted to. That framework belongs to
#      Blackmagic's Desktop Video installer and is not ours to redistribute. It
#      is a USER-INSTALLED PREREQUISITE, exactly the class WebView2 is in on
#      Windows — see build/licenses/NOTICE.txt section F3 and README-darwin.md
#      section 2;
#   3. THIS BUILD HOST needs it too. Step 1 resolves every wanted element
#      through gst-inspect and step 5 resolves them again from inside the
#      bundle, so on a Mac without Desktop Video this script now FAILS —
#      "gst-inspect-1.0 cannot find these elements: decklinkvideosrc
#      decklinkaudiosrc" — rather than quietly shipping a bundle that cannot
#      capture. Measured against Desktop Video 16.0 with a card present.
#
# What the failure looks like on a TARGET Mac without Desktop Video was NOT
# measured, because this host has it installed and there is no way to hide a
# framework at an absolute path from one process. It is quiet either way: with
# no DeckLinkAPI.framework there is no iterator, so there are no devices, and
# whether the elements are absent from the registry or merely find nothing is a
# distinction the operator cannot make and does not care about.
#
# typefindfunctions is wanted because it is not optional: GStreamer's own
# typefinding is a plugin, and a registry without it produces caps negotiation
# failures that read like anything but a missing plugin.
WANTED_ELEMENTS="
filesrc queue capsfilter fakesink typefindfunctions
pngdec imagefreeze
videoconvert videoscale videorate deinterlace
audioconvert audioresample
h264parse h265parse aacparse
level
mpegtsmux tsdemux
srtsink srtsrc
osxaudiosrc osxaudiosink
decklinkvideosrc decklinkaudiosrc
atenc atdec
vtenc_h264 vtenc_h264_hw vtdec_hw vtdec
glimagesink osxvideosink
"

# The FIRST application of the licence gate: refuse to run at all if a wanted
# name is one of the forbidden ones.
#
# This is the cheapest of the three and the only one that catches the mistake at
# the moment it is made — somebody adding "x264enc" to the list above because
# gst-inspect says it is there and ranks primary. The closure check at step 2.5
# would catch the resulting libgstx264.dylib too, but it would catch it after a
# minute of resolution work and with a message about dependencies rather than
# about the line that was just edited.
#
# Element names, not file names, so it matches on the pattern STEMS: "x264enc"
# contains "x264", "avdec_aac" contains "avdec"... it does not, in fact, and that
# is worth being honest about. This check catches x264, x265, faac, faad, fdkaac,
# lame and ugly by name; the gst-libav elements are called avenc_*/avdec_* and are
# caught by the closure check instead, on the file they resolve to
# (libgstlibav.dylib). Two overlapping controls, neither of which is complete
# alone, which is why both are here.
BAD_WANTED="$(printf '%s\n' $WANTED_ELEMENTS | grep -Ei "$FORBIDDEN" || true)"
if [ -n "$BAD_WANTED" ]; then
    echo "FAIL: WANTED_ELEMENTS names licence-forbidden elements:" >&2
    echo "$BAD_WANTED" | sed 's/^/       /' >&2
    echo "       See build/forbidden-names.ps1 and build/licenses/NOTICE.txt section G." >&2
    echo "       There is no override switch. If the pipeline genuinely needs one of these," >&2
    echo "       that is a licensing decision and not a build one." >&2
    exit 1
fi

RESOLVE_LOG="$RES_DIR/GST-ELEMENT-RESOLUTION.txt"
PLUGIN_LIST_FILE="$(mktemp)"
SEEN_FILE="$(mktemp)"
QUEUE_FILE="$(mktemp)"
NEXT_FILE="$(mktemp)"
WALK_FILE="$(mktemp)"
trap 'rm -f "$PLUGIN_LIST_FILE" "$SEEN_FILE" "$QUEUE_FILE" "$NEXT_FILE" "$WALK_FILE"' EXIT

{
    echo "Element -> plugin dylib, as resolved by gst-inspect-1.0 against"
    echo "$GST_KEG"
    echo "on $(date -u '+%Y-%m-%dT%H:%M:%SZ'). Written by build/bundle-gst-darwin.sh."
    echo ""
} > "$RESOLVE_LOG"

MISSING=""
for e in $WANTED_ELEMENTS; do
    f=$(gst-inspect-1.0 "$e" 2>/dev/null | grep -E '^[[:space:]]*Filename[[:space:]]+/' | sed 's#.*Filename[[:space:]]*##' || true)
    if [ -z "$f" ]; then
        MISSING="$MISSING $e"
        printf '  %-20s MISSING\n' "$e" >> "$RESOLVE_LOG"
        continue
    fi
    printf '  %-20s %s\n' "$e" "$f" >> "$RESOLVE_LOG"
    echo "$f" >> "$PLUGIN_LIST_FILE"
done
if [ -n "$MISSING" ]; then
    echo "Error: gst-inspect-1.0 cannot find these elements:$MISSING" >&2
    echo "       The build host's GStreamer is incomplete. On a stock Homebrew" >&2
    echo "       install all of them are present (measured against 1.26.10);" >&2
    echo "       'brew install gstreamer gst-plugins-base gst-plugins-good" >&2
    echo "       gst-plugins-bad' is the full set this needs." >&2
    case "$MISSING" in
        *decklink*)
            # A different remedy from every other name in that list, so it is
            # said rather than left to be discovered: the plugin file IS in a
            # stock Homebrew GStreamer, and reinstalling GStreamer will not
            # bring these two elements back.
            echo "" >&2
            echo "       decklink* is NOT a GStreamer packaging problem. libgstdecklink.dylib" >&2
            echo "       opens /Library/Frameworks/DeckLinkAPI.framework as a CFBundle at run" >&2
            echo "       time, and that framework comes from Blackmagic's DESKTOP VIDEO" >&2
            echo "       installer. Install Desktop Video on this build host and re-run." >&2
            echo "       See build/README-darwin.md section 2." >&2
            ;;
    esac
    exit 1
fi

sort -u "$PLUGIN_LIST_FILE" -o "$PLUGIN_LIST_FILE"
PLUGIN_COUNT=$(grep -c . "$PLUGIN_LIST_FILE" || true)
WANTED_COUNT=$(echo $WANTED_ELEMENTS | wc -w | tr -d ' ')
echo "==> $WANTED_COUNT wanted elements -> $PLUGIN_COUNT unique plugin dylibs"

# ── Step 2: transitive otool -L closure ─────────────────────────────────────
#
# Seeded with the plugins, the out-of-process plugin scanner, and the app's own
# binary. The scanner is easy to forget and its absence is not a hard failure —
# GStreamer falls back to scanning plugins IN PROCESS, prints a warning, and
# carries on — which is precisely why it would ship broken unnoticed.
#
# Only /opt/homebrew-rooted dependencies are followed. /usr/lib and
# /System/Library are dyld-shared-cache paths present on every Mac; copying
# them would be wrong as well as pointless.
: > "$SEEN_FILE"
cat "$PLUGIN_LIST_FILE" > "$QUEUE_FILE"
echo "$SCANNER_SRC" >> "$QUEUE_FILE"
[ -z "$APP_EXE" ] || echo "$APP_EXE" >> "$QUEUE_FILE"
if [ "${KEEP_TOOLS:-1}" != "0" ]; then
    for tool in gst-inspect-1.0 gst-launch-1.0; do
        [ -f "$GST_KEG/bin/$tool" ] && echo "$GST_KEG/bin/$tool" >> "$QUEUE_FILE"
    done
fi

while [ -s "$QUEUE_FILE" ]; do
    : > "$NEXT_FILE"
    while IFS= read -r item; do
        [ -n "$item" ] || continue
        real="$(cd "$(dirname "$item")" && pwd -P)/$(basename "$item")"
        grep -qxF "$real" "$SEEN_FILE" 2>/dev/null && continue
        echo "$real" >> "$SEEN_FILE"
        otool -L "$real" 2>/dev/null | tail -n +2 | awk '{print $1}' | grep -E '^/opt/homebrew' >> "$NEXT_FILE" || true
    done < "$QUEUE_FILE"
    cp "$NEXT_FILE" "$QUEUE_FILE"
done

# The app binary is already inside the .app. It is a closure SEED, never a
# closure MEMBER — copying it into Frameworks would ship a second 29 MB copy of
# the executable.
if [ -n "$APP_EXE" ]; then
    grep -vxF "$(cd "$MACOS_DIR" && pwd -P)/$APP_EXE_NAME" "$SEEN_FILE" > "$SEEN_FILE.tmp" || true
    mv "$SEEN_FILE.tmp" "$SEEN_FILE"
fi
sort -u "$SEEN_FILE" -o "$SEEN_FILE"

CLOSURE_COUNT=$(grep -c . "$SEEN_FILE" || true)
CLOSURE_BYTES=$(xargs stat -f%z < "$SEEN_FILE" 2>/dev/null | awk '{s+=$1} END {print s+0}')
CLOSURE_MB=$(awk -v b="$CLOSURE_BYTES" 'BEGIN{printf "%.1f", b/1048576}')
FULL_KEG_MB=$(du -sk "$GST_KEG" 2>/dev/null | awk '{print int($1/1024)}')
FULL_PLUGIN_COUNT=$(ls -1 "$GST_KEG/lib/gstreamer-1.0" 2>/dev/null | grep -c '\.dylib$' || true)
echo "==> closure: $CLOSURE_COUNT Mach-O files, ${CLOSURE_MB} MB (the whole keg is ~${FULL_KEG_MB} MB / $FULL_PLUGIN_COUNT plugins)"

# ── Step 2.5: the licensing control, applied to the CLOSURE ─────────────────
#
# The second of the three applications; the list itself is parsed at step 0.
# This is the one that matters most, because the closure is computed rather than
# listed: a plugin can be dragged in as a DEPENDENCY of something perfectly
# innocent, and nothing in the wanted list would ever mention it.
BAD="$(awk -F/ '{print $NF}' "$SEEN_FILE" | grep -Ei "$FORBIDDEN" || true)"
if [ -n "$BAD" ]; then
    echo "FAIL: the computed closure contains licence-forbidden files:" >&2
    echo "$BAD" | sed 's/^/       /' >&2
    echo "       See build/forbidden-names.ps1 and build/licenses/NOTICE.txt section G." >&2
    echo "       Something in WANTED_ELEMENTS is pulling in a plugin that must not ship." >&2
    exit 1
fi
echo "==> licence gate: $FORBIDDEN_N patterns from forbidden-names.ps1, zero matches in the closure"

# ── Step 3: copy, relink, ad-hoc re-sign ────────────────────────────────────
#
# The layout is fixed, so @loader_path alone is enough and no LC_RPATH
# indirection is needed:
#
#   Contents/MacOS/<exe>                     -> @executable_path/../Frameworks/<leaf>
#   Contents/MacOS/gst-plugin-scanner        -> @executable_path/../Frameworks/<leaf>
#   Contents/Frameworks/<lib>.dylib          -> @loader_path/<leaf>
#   Contents/Resources/gstreamer-1.0/<p>     -> @loader_path/../../Frameworks/<leaf>
#
# @executable_path is used for the things in Contents/MacOS rather than
# @loader_path only because it reads as what it means there; both resolve to
# the same directory for a main executable. The plugins must use @loader_path
# and not @executable_path, because gst-plugin-scanner loads them too and its
# executable path is a different file from the app's.
#
# Every file gets its LC_ID_DYLIB rewritten too, not just its load commands.
# A dylib whose own install name is still /opt/homebrew/... will be recorded
# under that name by anything that links against it later, and it is a Homebrew
# path inside a shipped binary either way.
#
# The ad-hoc re-sign at the end of each file is not optional and not tidiness.
# install_name_tool rewrites the Mach-O in place, which invalidates the
# signature it was built with; on Apple silicon dyld REFUSES to load a Mach-O
# whose signature does not match, so an unsigned-after-rewrite dylib is a
# launch failure, not a warning. These signatures are thrown away again by
# ship-darwin.sh's Developer ID pass.
relink_one() {
    local src="$1" dest="$2" kind="$3"   # kind: fw | plugin | macos
    local leaf dep_leaf new old rp
    leaf="$(basename "$src")"

    cp -p "$src" "$dest"
    chmod u+w "$dest"

    case "$kind" in
        macos) install_name_tool -id "@executable_path/$leaf" "$dest" 2>/dev/null || true ;;
        *)     install_name_tool -id "@loader_path/$leaf" "$dest" 2>/dev/null || true ;;
    esac

    otool -L "$src" 2>/dev/null | tail -n +2 | awk '{print $1}' | grep -E '^/opt/homebrew' | while IFS= read -r old; do
        dep_leaf="$(basename "$old")"
        case "$kind" in
            fw)     new="@loader_path/$dep_leaf" ;;
            plugin) new="@loader_path/../../Frameworks/$dep_leaf" ;;
            macos)  new="@executable_path/../Frameworks/$dep_leaf" ;;
        esac
        install_name_tool -change "$old" "$new" "$dest" 2>/dev/null || true
    done || true

    otool -l "$src" 2>/dev/null | awk '/LC_RPATH/{f=1} f&&/ path /{print $2; f=0}' | grep -E '^/opt/homebrew' | while IFS= read -r rp; do
        install_name_tool -delete_rpath "$rp" "$dest" 2>/dev/null || true
    done || true

    codesign --remove-signature "$dest" >/dev/null 2>&1 || true
    codesign --force --sign - "$dest" >/dev/null 2>&1
}

STAGED=0
while IFS= read -r f; do
    [ -n "$f" ] || continue
    leaf="$(basename "$f")"
    if [ "$f" = "$SCANNER_SRC" ] || [ "$(dirname "$f")" = "$GST_KEG/bin" ]; then
        relink_one "$f" "$MACOS_DIR/$leaf" macos
    elif grep -qxF "$f" "$PLUGIN_LIST_FILE"; then
        relink_one "$f" "$PLUGIN_DIR/$leaf" plugin
    else
        relink_one "$f" "$FW_DIR/$leaf" fw
    fi
    STAGED=$((STAGED + 1))
done < "$SEEN_FILE"
echo "==> staged $STAGED Mach-O files"

# ── Step 3.5: the app's own binary ──────────────────────────────────────────
#
# Rewritten in place, never copied. It is the one Mach-O in the bundle whose
# Homebrew references were put there by OUR compiler rather than Homebrew's,
# and it is the one whose failure to load is a Finder bounce and nothing else:
# these are LC_LOAD_DYLIBs, resolved by dyld before main() runs, so no Go code
# can rescue them. On Windows the same problem is sidestepped by the loader
# always searching the directory holding the .exe; a .app has no such rule
# because Contents/Frameworks is not Contents/MacOS.
if [ -n "$APP_EXE" ]; then
    echo "==> relinking Contents/MacOS/$APP_EXE_NAME"
    HITS=0
    otool -L "$APP_EXE" | tail -n +2 | awk '{print $1}' | grep -E '^/opt/homebrew' > "$WALK_FILE" || true
    while IFS= read -r old; do
        [ -n "$old" ] || continue
        leaf="$(basename "$old")"
        if [ ! -f "$FW_DIR/$leaf" ]; then
            echo "FAIL: $APP_EXE_NAME links $old but $leaf is not in the closure." >&2
            echo "      The closure walk missed a direct dependency of the app binary." >&2
            exit 1
        fi
        install_name_tool -change "$old" "@executable_path/../Frameworks/$leaf" "$APP_EXE" 2>/dev/null || true
        HITS=$((HITS + 1))
    done < "$WALK_FILE"
    otool -l "$APP_EXE" 2>/dev/null | awk '/LC_RPATH/{f=1} f&&/ path /{print $2; f=0}' | grep -E '^/opt/homebrew' > "$WALK_FILE" || true
    while IFS= read -r rp; do
        [ -n "$rp" ] || continue
        install_name_tool -delete_rpath "$rp" "$APP_EXE" 2>/dev/null || true
    done < "$WALK_FILE"
    echo "    $HITS load commands repointed at ../Frameworks"

    # The bundle, not the file. Handing codesign the MAIN EXECUTABLE of a
    # bundle makes it sign the whole bundle instead — documented behaviour,
    # and the reason the obvious `codesign --force --sign - "$APP_EXE"` here
    # produced "bundle format unrecognized" until the plugin directory moved
    # out of Contents/Frameworks. Saying so explicitly costs nothing and means
    # the next reader is not surprised by the same thing twice.
    codesign --remove-signature "$APP" >/dev/null 2>&1 || true
    codesign --force --sign - "$APP" >/dev/null 2>&1
fi

# ── Step 3.6: somewhere for libgio to look that is not Homebrew ─────────────
#
# libgio has /opt/homebrew/lib/gio/modules compiled into it as a C STRING, not
# as a load command, so step 4 cannot see it and step 3 cannot rewrite it. On a
# customer's Mac that directory does not exist and nothing happens. On a
# DEVELOPER's Mac — or any operator who happens to have Homebrew — it does, and
# a foreign glib's modules get dlopened into this process alongside our own
# vendored glib. GIO_MODULE_DIR (set by internal/gst before gst_init) points at
# this directory instead. It has a file in it so that it survives being copied,
# zipped and stapled, and so that whoever finds it knows why it is empty.
cat > "$GIO_DIR/README.txt" <<'EOF'
Deliberately empty of modules.

libgio.dylib has Homebrew's /opt/homebrew/lib/gio/modules compiled into it as
a string constant, so it cannot be rewritten by install_name_tool the way a
load command can. internal/gst points GIO_MODULE_DIR at THIS directory before
gst_init, so that on a Mac which does have Homebrew installed, a foreign glib's
GIO modules cannot be loaded into this process next to our own vendored glib.

Nothing should ever be put in here. See build/bundle-gst-darwin.sh step 3.6.
EOF

# ── Step 3.7: the licensing control, applied to what is ON DISK ─────────────
#
# The third and last application, and the only one that inspects the artefact
# rather than a list or a computation. Steps 1 and 2.5 both reason about what
# SHOULD be copied; this one asks what IS there, by walking the tree the same
# way step 4 does rather than by enumerating the directories this script knows
# about.
#
# It is not redundant with the other two, and the reason is specific: this
# script is not the only thing that writes into the bundle. `wails build` has
# already run, ship-darwin.sh runs after, and a re-run of this script over a
# tree somebody has hand-copied a dylib into would sign and notarise it. Walking
# catches that; a check against $SEEN_FILE could not, because the file would not
# be in it.
echo "==> licence gate: walking the staged bundle"
STAGED_BAD="$(find "$APP" -type f | awk -F/ '{print $NF}' | grep -Ei "$FORBIDDEN" || true)"
if [ -n "$STAGED_BAD" ]; then
    echo "FAIL: licence-forbidden files are present in the staged bundle:" >&2
    echo "$STAGED_BAD" | sed 's/^/       /' >&2
    echo "       These are IN $APP right now. Nothing above copied them, so something" >&2
    echo "       else did — check what else writes into this bundle before it is signed." >&2
    exit 1
fi
echo "    zero matches in $(find "$APP" -type f | wc -l | tr -d ' ') staged files"

# ── Step 4: the audit that fails the build ──────────────────────────────────
#
# Every Mach-O anywhere under the bundle, walked rather than enumerated. A
# named list of directories is correct only until somebody adds one, which is
# how three unsigned dylibs once shipped in the product this technique came
# from. Load commands AND LC_RPATH — see the header for why the rpath half
# matters.
#
# `find` into a temp file rather than into a process substitution, and it is
# worth knowing why even though nothing here signs concurrently: process
# substitution starts find as a background co-process, so a later `codesign
# --force` in the loop can be renaming its `<name>.cstemp` into place while
# find is still reading that directory, and find then hands the vanished
# .cstemp to the loop. The temp file is a completed snapshot before the loop
# starts. This script keeps the same discipline throughout so the two halves
# cannot drift.
echo "==> audit: /opt/homebrew references anywhere in the bundle"
find "$APP" -type f > "$WALK_FILE"
AUDIT_HITS=""
MACHO_COUNT=0
while IFS= read -r f; do
    file "$f" 2>/dev/null | grep -q 'Mach-O' || continue
    MACHO_COUNT=$((MACHO_COUNT + 1))
    hit=$(otool -L "$f" 2>/dev/null | tail -n +2 | awk '{print $1}' | grep -E '^/opt/homebrew' || true)
    rp=$(otool -l "$f" 2>/dev/null | awk '/LC_RPATH/{f=1} f&&/ path /{print $2; f=0}' | grep -E '^/opt/homebrew' || true)
    if [ -n "$hit$rp" ]; then
        AUDIT_HITS="$AUDIT_HITS
  ${f#"$APP"/}
$(printf '%s\n' "$hit" "$rp" | grep -v '^$' | sed 's/^/    /')"
    fi
done < "$WALK_FILE"
if [ -n "$AUDIT_HITS" ]; then
    echo "FAIL: /opt/homebrew references remain:$AUDIT_HITS" >&2
    exit 1
fi
echo "    OK — 0 of $MACHO_COUNT Mach-O files reference /opt/homebrew"

# ── Step 4b: the soft audit the hard one cannot do ──────────────────────────
#
# A Homebrew path compiled in as a C string is invisible to otool. Most are
# inert (locale directories, dbus socket paths), two are not: libgio's module
# directory and libglib's XDG_DATA_DIRS default, both live search paths on a
# machine that has Homebrew. They are neutralised at RUN TIME by internal/gst
# (GIO_MODULE_DIR, GST_PLUGIN_SCANNER_1_0), not here, because there is nothing
# to rewrite — the strings are the same length as nothing else that would fit.
# This is a WARNING and not a failure precisely because the fix is elsewhere;
# it exists so the count is visible and a sudden change in it gets noticed.
STR_HITS=$(while IFS= read -r f; do
    file "$f" 2>/dev/null | grep -q 'Mach-O' || continue
    n=$(strings - "$f" 2>/dev/null | grep -c '/opt/homebrew' || true)
    [ "${n:-0}" -gt 0 ] && printf '%-40s %s\n' "$(basename "$f")" "$n"
done < "$WALK_FILE" || true)
if [ -n "$STR_HITS" ]; then
    echo "==> note: embedded (non-load-command) /opt/homebrew strings remain, by file:"
    echo "$STR_HITS" | sed 's/^/    /'
    echo "    These are neutralised at run time by internal/gst (GIO_MODULE_DIR,"
    echo "    GST_PLUGIN_SCANNER_1_0), not by rewriting. See step 4b."
fi

# ── Step 4c: the deployment floor the bundle actually has ───────────────────
#
# MEASURED, and it is not what anyone assumes. Homebrew builds its bottles for
# the build machine's macOS major version, so on a Mac running macOS 26 the
# vendored libgstreamer and libglib carry LC_BUILD_VERSION minos 26.0 — the
# .app cannot run on anything older no matter what LSMinimumSystemVersion
# claims, and dyld's refusal at launch is not a message anybody can act on.
# Wails' stock Info.plist says 10.13.0, a number arm64 cannot reach at all.
#
# So the floor is computed here and written into the manifest, and
# ship-darwin.sh stage 5 RAISES the shipped bundle's LSMinimumSystemVersion to
# it before signing. It is deliberately not compared against a number checked
# into the tree: this one tracks the build machine, build/darwin/Info.plist's
# tracks the product, and a stage that refused on the difference would only ever
# be satisfied by hand-editing the product's floor to a build-host accident.
FLOOR="0"
while IFS= read -r f; do
    file "$f" 2>/dev/null | grep -q 'Mach-O' || continue
    v=$(otool -l "$f" 2>/dev/null | awk '/LC_BUILD_VERSION/{b=1} b&&/^ *minos /{print $2; b=0} /LC_VERSION_MIN_MACOSX/{m=1} m&&/^ *version /{print $2; m=0}' | sort -V | tail -1)
    [ -n "$v" ] || continue
    FLOOR=$(printf '%s\n%s\n' "$FLOOR" "$v" | sort -V | tail -1)
done < "$WALK_FILE"
echo "==> deployment floor of the staged payload: macOS $FLOOR (highest LC_BUILD_VERSION minos in the bundle)"

# ── Step 5: hermetic proof ──────────────────────────────────────────────────
#
# The static audit proves no Mach-O NAMES a Homebrew path. It does not prove
# the bundle is complete, and it cannot: a plugin that fails to load because a
# dependency was missed loads silently-not-at-all and the element simply is not
# there. So every wanted element is looked up again, through a gst-inspect that
# is itself inside the bundle (brew's own gst-inspect links libgstreamer
# absolutely and would load the CORE runtime from Homebrew whatever GST_PLUGIN_
# PATH said), under `env -i` so that nothing inherited from this shell can be
# what made it work, and the resolved Filename is required to be inside
# Contents/Resources/gstreamer-1.0. Anything else is a LEAK and fails the build.
#
# The environment here is deliberately the same set internal/gst sets before
# gst_init, ORC_CODE included, so that the proof exercises the configuration
# the app actually runs in rather than a friendlier one. ORC_CODE=backup is
# there because liborc cannot obtain a writable-executable mapping on Apple
# silicon under the hardened runtime and says so at ERROR level; see
# build/darwin/wslcomms.entitlements for the measurement behind that.
if [ "${KEEP_TOOLS:-1}" != "0" ] && [ -x "$MACOS_DIR/gst-inspect-1.0" ]; then
    echo "==> hermetic proof: resolving all $WANTED_COUNT elements from inside the bundle"
    PROOF_DIR="$(mktemp -d)"
    FAILS=0
    for e in $WANTED_ELEMENTS; do
        f=$(env -i HOME="$PROOF_DIR" \
            GST_PLUGIN_SYSTEM_PATH_1_0="$PLUGIN_DIR" \
            GST_PLUGIN_SYSTEM_PATH="$PLUGIN_DIR" \
            GST_PLUGIN_PATH_1_0="$PLUGIN_DIR" \
            GST_PLUGIN_SCANNER_1_0="$MACOS_DIR/gst-plugin-scanner" \
            GST_PLUGIN_SCANNER="$MACOS_DIR/gst-plugin-scanner" \
            GIO_MODULE_DIR="$GIO_DIR" \
            GST_REGISTRY_1_0="$PROOF_DIR/registry.bin" \
            ORC_CODE=backup \
            "$MACOS_DIR/gst-inspect-1.0" "$e" 2>/dev/null |
            grep -E '^[[:space:]]*Filename' | sed 's#.*Filename[[:space:]]*##' || true)
        case "$f" in
            */Contents/Resources/gstreamer-1.0/*) ;;
            "") echo "    FAIL $e — not found inside the bundle" >&2; FAILS=$((FAILS + 1)) ;;
            *)  echo "    LEAK $e — resolved to $f" >&2; FAILS=$((FAILS + 1)) ;;
        esac
    done
    rm -rf "$PROOF_DIR"
    [ "$FAILS" -eq 0 ] || { echo "FAIL: $FAILS element(s) did not resolve from the bundle." >&2; exit 1; }
    echo "    all $WANTED_COUNT elements resolved to Contents/Resources/gstreamer-1.0"
else
    echo "==> hermetic proof SKIPPED (KEEP_TOOLS=0) — the bundle is unproven"
fi

# ── Step 6: the manifest ────────────────────────────────────────────────────
#
# The .app's licensing audit artefact and its provenance record, in the same
# role as gst\BUNDLE-MANIFEST.txt on Windows: what was shipped, from which keg,
# with a SHA-256 for every file so that a bundle in the field can be compared
# against the one that was built. Written last so that a failed audit or a
# failed proof leaves no manifest behind claiming success.
{
    echo "WSL Commentary — bundled GStreamer runtime for macOS"
    echo "Written by build/bundle-gst-darwin.sh on $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    echo ""
    echo "Source keg:          $GST_KEG"
    echo "Wanted elements:     $WANTED_COUNT   (see GST-ELEMENT-RESOLUTION.txt)"
    echo "Plugin dylibs:       $PLUGIN_COUNT"
    echo "Closure Mach-O:      $CLOSURE_COUNT files, ${CLOSURE_MB} MB"
    echo "Whole keg, for size: ${FULL_KEG_MB} MB, $FULL_PLUGIN_COUNT plugins"
    echo "Licence patterns:    $FORBIDDEN_N, from build/forbidden-names.ps1, zero matches"
    echo "Deployment floor:    macOS $FLOOR"
    echo "MINOS-FLOOR=$FLOOR"
    echo ""
    echo "Every Mach-O below has been rewritten to load only from inside this"
    echo "bundle and re-signed. The sha256 values are of the REWRITTEN files,"
    echo "which are the ones that ship, not of the Homebrew originals."
    echo ""
    ( cd "$APP" && find Contents/Frameworks Contents/MacOS Contents/Resources/gstreamer-1.0 -type f | sort | while IFS= read -r rel; do
        printf '  %-56s %s\n' "$rel" "$(shasum -a 256 "$rel" | awk '{print $1}')"
    done )
} > "$MANIFEST"

FW_N=$(ls -1 "$FW_DIR" | grep -c '\.dylib$' || true)
PL_N=$(ls -1 "$PLUGIN_DIR" | wc -l | tr -d ' ')
MC_N=$(ls -1 "$MACOS_DIR" | wc -l | tr -d ' ')
TOTAL_MB=$(du -sm "$APP" | awk '{print $1}')

echo ""
echo "  Contents/Frameworks/                  $FW_N dylibs"
echo "  Contents/Resources/gstreamer-1.0/     $PL_N plugins"
echo "  Contents/MacOS/                       $MC_N files"
echo "  bundle total                          ${TOTAL_MB} MB"
echo ""

# The Windows twin warns when the staged bundle leaves the band it was measured
# in, on the same reasoning: under, something is missing; over, something is
# being copied that should not be. The band here is the .app as a whole, and it
# is set from a measured 45-file / 21.5 MB closure inside a ~29 MB binary.
if [ "$TOTAL_MB" -lt 25 ] || [ "$TOTAL_MB" -gt 90 ]; then
    echo "WARNING: the finished bundle is ${TOTAL_MB} MB, outside the 25–90 MB band this"
    echo "         was measured in. Under: something did not get staged. Over: something"
    echo "         is being copied that should not be — read $MANIFEST."
fi
echo "==> done: $APP"
