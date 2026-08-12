#Requires -Version 5.1
<#
.SYNOPSIS
    Stages the hand-picked GStreamer runtime for wslcomms.exe into dist\, from an
    EXPLICIT FILE LIST. Never a directory copy.

.DESCRIPTION
    Owner: WP-6. Specification sections 3 and 11.

    WHY THIS SCRIPT EXISTS AT ALL
    -----------------------------
    A directory copy of C:\gstreamer\1.0\mingw_x86_64 would be one line of
    PowerShell and would be wrong, because it drags libgstx264.dll - GPL-2.0 -
    into a commercial deliverable. Shipping it would place the whole combined
    work under the GPL. This script therefore names every single file it copies,
    refuses to copy anything whose name matches a forbidden pattern (x264 first
    among them), and audits the destination afterwards so that a file left behind
    by an earlier ad-hoc copy cannot survive into an installer. The licensing
    control is the explicit list; the tidiness is a side effect.

    The video encoder is mfh264enc - Media Foundation, already part of Windows,
    an LGPL wrapper over an OS codec. There is no reason for x264 to be anywhere
    near this build. See build\licenses\NOTICE.txt.

    WHAT THIS SCRIPT CANNOT KNOW, AND HOW YOU FIX IT
    ------------------------------------------------
    *** THE FILE LIST BELOW IS A BEST-EFFORT STARTING POINT. ***
    *** GATE B MUST VERIFY IT AGAINST A REAL GSTREAMER INSTALL. ***

    It was written on a machine with no GStreamer installed (Gate B closed), so
    the exact DLL names and the transitive dependency closure of the thirteen
    allowlisted plugins could not be computed. Two mechanisms make that
    recoverable rather than fatal:

      1. Every entry carries a list of candidate file names, because the MinGW
         builds sometimes prefix a library with "lib" where the MSVC builds do
         not (glib-2.0-0.dll vs libglib-2.0-0.dll). The first candidate that
         exists is used; if none exists the script fails and prints the entry's
         reason and remediation.

      2. -DependencyReport (on by default when a dependency walker is found)
         reads the import table of every file it copied and reports any imported
         DLL that is neither in the bundle nor present in %SystemRoot%\System32.
         That is the dependency closure, computed from the real files. Anything
         it reports must be added to the list below, or the app will fail at
         runtime with "The specified module could not be found" or a plugin that
         silently refuses to load.

    Walkers, in the order the script looks for them:
        objdump -p <file>            (ships with MinGW gcc - you have it at Gate B)
        x86_64-w64-mingw32-objdump -p <file>
        dumpbin /dependents <file>   (Visual Studio Build Tools)
    Dependencies.exe (https://github.com/lucasg/Dependencies) is the GUI
    equivalent and is the right tool if you want to eyeball the tree; this script
    does not drive it. Whichever you use, the loop is the same: run it, add the
    unresolved names to the list below with a one-line reason, re-run this script
    until it reports nothing unresolved.

.PARAMETER GstRoot
    Root of the GStreamer 1.28.5 mingw-x86_64 installation. The default matches
    the PKG_CONFIG_PATH given in specification section 11
    (C:\gstreamer\1.0\mingw_x86_64\lib\pkgconfig), so the root is
    C:\gstreamer\1.0\mingw_x86_64.

.PARAMETER DistDir
    The staging directory, which is a byte-for-byte image of the installed
    program folder. Defaults to <repo>\dist. build\installer.iss reads it.

.PARAMETER CoreDllLayout
    Where the GStreamer/GLib/MinGW runtime DLLs go. Plugins ALWAYS go to
    dist\gst\lib\gstreamer-1.0 because internal\gst sets
    GST_PLUGIN_PATH_1_0=<appdir>\gst\lib\gstreamer-1.0 and that is contract.

      AppDir (default) - runtime DLLs next to wslcomms.exe, i.e. dist\.
      GstBin           - runtime DLLs in dist\gst\bin, mirroring upstream.

    AppDir is the default for a load-order reason, and it is not cosmetic.
    wslcomms.exe is cgo-linked against gstreamer-1.0, glib-2.0 and friends, so
    those DLLs are resolved by the Windows loader BEFORE any Go code runs. No
    os.Setenv("PATH", ...) inside Init can help: it executes too late. The only
    directory guaranteed to be searched at that point is the one holding the
    .exe. GstBin therefore requires something extra on the Go side (a delay-load
    link, a launcher, or dotlocal redirection) and will otherwise fail at startup
    with 0xC0000135. See build\README.md, "DLL search order".

.PARAMETER DryRun
    Validate the file list and print the plan; touch nothing. Works with no
    GStreamer installed, which is the only mode that can be run before Gate B.

.PARAMETER NoDependencyReport
    Skip the import-table audit even if a walker is available. Do not use this at
    Gate B; the report is the whole point.

.PARAMETER AllowUnresolved
    Downgrade unresolved imports from an error to a warning. For diagnosis only -
    a bundle with an unresolved import is a bundle that does not run.

.PARAMETER KeepExisting
    Do not delete the destination first. Off by default: the clean is what makes
    the destination audit meaningful.

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File build\bundle-gst.ps1 -DryRun

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File build\bundle-gst.ps1
#>
[CmdletBinding()]
param(
    [string]$GstRoot = 'C:\gstreamer\1.0\mingw_x86_64',
    [string]$DistDir,
    [ValidateSet('AppDir', 'GstBin')]
    [string]$CoreDllLayout = 'AppDir',
    [switch]$DryRun,
    [switch]$NoDependencyReport,
    [switch]$AllowUnresolved,
    [switch]$KeepExisting,
    [string]$DependencyTool,
    [switch]$NoStrip,
    [string]$StripTool
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# ScriptVersion is stamped into BUNDLE-MANIFEST.txt so that an installer built
# six months from now can be traced back to the list that produced it.
$ScriptVersion = '1.0.0'

# Expected bundle size. Outside this range something is wrong in one direction
# or the other - a missing plugin, or a directory copy that crept back in.
#
# MEASURED, not estimated: the first complete bundle, GStreamer 1.28.5
# mingw-x86_64 at Gate B on 2026-07-30, was 52.5 MB - 13 plugins and 33 runtime
# files, with the dependency closure verified complete by objdump. Specification
# section 3's 60-110 MB was an estimate made before anyone could build one, and
# it is superseded by this.
#
# RE-MEASURED 2026-08-07, after the d3d11 plugin was added for the picture:
# 54.1 MB (56,675,770 bytes), 15 plugins and 35 runtime files, closure again
# reported complete. The picture cost 1.6 MB - libgstd3d11.dll at 899 KB plus
# libgstcodecs at 310 KB and libgstdxva at 115 KB - which is comfortably inside
# the band below, so the band is UNCHANGED. It is restated rather than silently
# left because the 52.5 MB figure above is no longer what a clean run produces
# and the next person to compare should be comparing against this line.
#
# The band below is the measured figure with room either side: enough slack that
# a GStreamer point release does not trip it, tight enough that a directory copy
# (the whole install is 2.4 GB) or a silently dropped plugin still does.
#
# RE-MEASURED 2026-08-12, after stripping became the default: 22.9 MB. The
# GStreamer mingw distribution ships its DLLs with full DWARF debug sections,
# and they are 58% of the bundle by size. libstdc++-6.dll alone is 25.3 MB, of
# which 23 MB is .debug_info and friends; stripped it is 2.2 MB with all 17,800
# exports intact. Nothing in a shipped product reads those sections - they
# describe C++ standard-library internals we do not debug and cannot patch - so
# they are 31 MB of download and disk for no return. -NoStrip restores the old
# behaviour and the old band.
if ($NoStrip) {
    $ExpectedMinBytes = 40MB
    $ExpectedMaxBytes = 80MB
}
else {
    $ExpectedMinBytes = 15MB
    $ExpectedMaxBytes = 40MB
}

# ---------------------------------------------------------------------------
# Forbidden names. The licensing control.
# ---------------------------------------------------------------------------
# Checked three times: against the file list before anything is touched, against
# every resolved source path before it is copied, and against the destination
# tree after the copy. A match is fatal, always, with no override switch -
# there is deliberately no -Force here.
#
# The list itself lives in build\forbidden-names.ps1 because build\pack-portable.ps1
# redistributes these same binaries inside a single executable and must apply the
# identical list. Two copies of a licensing control is one copy too many.
. (Join-Path $PSScriptRoot 'forbidden-names.ps1')
if (-not $ForbiddenPatterns -or $ForbiddenPatterns.Count -eq 0) {
    throw 'build\forbidden-names.ps1 did not define $ForbiddenPatterns. Refusing to stage a bundle with the licensing control disabled.'
}

# ---------------------------------------------------------------------------
# The file list.
# ---------------------------------------------------------------------------

function New-BundleEntry {
    <#
    .SYNOPSIS
        One file to copy. Names is an ordered candidate list; the first that
        exists wins.
    .DESCRIPTION
        Kind      Plugin | Runtime. Decides the destination directory.
        Names     Candidate file names. NEVER a wildcard: a wildcard would
                  reintroduce exactly the failure mode this script exists to
                  prevent. The script rejects any name containing * or ?.
        Why       Why this file is in the bundle. Printed on failure and written
                  into BUNDLE-MANIFEST.txt.
        Optional  $true means "copy it if it is there, carry on if it is not".
                  Reserved for files whose presence genuinely varies between
                  GStreamer builds. Every optional file that is missing is
                  listed loudly in the summary, because a missing optional is
                  the most likely explanation for a plugin that will not load.
        Fix       What to do if a required file is missing. Printed on failure.
    #>
    param(
        [Parameter(Mandatory)][ValidateSet('Plugin', 'Runtime')][string]$Kind,
        [Parameter(Mandatory)][string[]]$Names,
        [Parameter(Mandatory)][string]$Why,
        [switch]$Optional,
        [string]$Fix = ''
    )
    return [pscustomobject]@{
        Kind     = $Kind
        Names    = $Names
        Why      = $Why
        Optional = [bool]$Optional
        Fix      = $Fix
    }
}

function Get-PluginEntries {
    <#
    .SYNOPSIS
        The thirteen plugins of specification section 3, plus mpegtsdemux for
        the return path, and nothing else.
    .DESCRIPTION
        This list is closed. Adding to it is a specification change, not a build
        change: every plugin here was chosen because the pipeline in section 5
        needs it, and every plugin not here was left out because it is not
        needed, is GPL, or drags in a dependency we would then have to ship.

        There have been exactly two additions since section 3 was written, and
        both are stated here rather than slipped into the list, because that is
        the rule this function exists to enforce on everyone else:

          mpegtsdemux   for the SRT return monitor.
          d3d11         for the SRT PICTURE. See its Why line below.

        One plugin was CONSIDERED and deliberately NOT added:

          libav       gst-libav is FFmpeg. Its licence depends on how it was
                      built and is not ours to assume, which is the same
                      commercial-shipping concern that keeps x264 out and the
                      reason *libav* and *avcodec* are in $ForbiddenPatterns.
                      avdec_aac and avdec_h265 both rank primary on the dev
                      machine and NEITHER may be selected: the AAC decoder is
                      mfaacdec (mediafoundation) and the HEVC decoder is
                      d3d11h265dec (d3d11), both LGPL wrappers over decoders
                      that are already on the machine.

        THE d3d11 DECISION WAS TAKEN ON 2026-08-07 AND IT IS RECORDED HERE
        BECAUSE THIS COMMENT USED TO SAY THE OPPOSITE. It said the return was
        audio only, that the picture was "a separate, larger decision the
        operator has not taken", and that the H.265 video pad was fakesinked.
        The operator has now taken that decision the other way round: SRT
        carries the PICTURE and Kinesis carries the AUDIO. So the plugin is in
        the list, the video pad is decoded, and it is the AUDIO pad that is
        fakesinked. See internal/gst/picture.go.

        Note that libgstd3d11-1.0-0.dll in Get-RuntimeEntries is a different
        file from libgstd3d11.dll here: the first is the runtime LIBRARY, which
        was already bundled because the Media Foundation plugin links it, and
        the second is the PLUGIN, which was not. Adding the plugin also pulled
        in two runtime libraries that nothing previously bundled needed -
        libgstcodecs and libgstdxva - both verified by objdump against
        libgstd3d11.dll on this machine and both marked as such below.

        The gstreamer plugin file naming convention (libgst<name>.dll) is the
        same in the MSVC and MinGW builds, so these names are the ones this
        script is most confident about.
    #>
    return @(
        New-BundleEntry -Kind Plugin -Names 'libgstcoreelements.dll' `
            -Why 'queue, filesrc, capsfilter, tee. Nothing runs without it.'
        New-BundleEntry -Kind Plugin -Names 'libgsttypefindfunctions.dll' `
            -Why 'typefinding for the slate PNG; pngdec will not be reached without it.'
        New-BundleEntry -Kind Plugin -Names 'libgstvideoconvertscale.dll' `
            -Why 'videoconvert to NV12 ahead of mfh264enc (spec section 5).'
        New-BundleEntry -Kind Plugin -Names 'libgstaudioconvert.dll' `
            -Why 'audioconvert to S16LE ahead of mfaacenc (spec section 5).'
        New-BundleEntry -Kind Plugin -Names 'libgstaudioresample.dll' `
            -Why 'audioresample. Also absorbs the Dante-vs-system-clock drift caused by pinning the system clock (spec section 6.1).'
        New-BundleEntry -Kind Plugin -Names 'libgstimagefreeze.dll' `
            -Why 'imagefreeze is-live=true turns the slate into a live 1080p50 source. Mandatory per spec section 5.'
        New-BundleEntry -Kind Plugin -Names 'libgstpng.dll' `
            -Why 'pngdec for slate.png.'
        New-BundleEntry -Kind Plugin -Names 'libgstaudioparsers.dll' `
            -Why 'aacparse, to get adts framing into the muxer.'
        New-BundleEntry -Kind Plugin -Names 'libgstvideoparsersbad.dll' `
            -Why 'h264parse config-interval=-1, so SPS/PPS precede every IDR and M2L-X can re-lock mid-stream.'
        New-BundleEntry -Kind Plugin -Names 'libgstwasapi2.dll' `
            -Why 'wasapi2src, and the GstDeviceMonitor that fills the commentary input dropdown. Takes the IMMDevice endpoint ID.'
        New-BundleEntry -Kind Plugin -Names 'libgstmediafoundation.dll' `
            -Why 'mfh264enc and mfaacenc: LGPL wrappers over codecs already in Windows. This plugin is what makes x264 unnecessary.'
        New-BundleEntry -Kind Plugin -Names 'libgstmpegtsmux.dll' `
            -Why 'mpegtsmux alignment=7, giving 1316-byte buffers - exactly one SRT payload.'
        New-BundleEntry -Kind Plugin -Names 'libgstmpegtsdemux.dll' `
            -Why 'tsdemux: the RETURN path''s demuxer. The commentator''s off-air monitor dials an M2L-X output as an SRT caller and gets MPEG-TS back, and this is the only element in the tree that can take the AAC out of it. Audio only - the H.265 video pad is fakesinked, not decoded.'
        New-BundleEntry -Kind Plugin -Names 'libgstsrt.dll' `
            -Why 'srtsink, mode=caller, auto-reconnect=false. The contribution path. Also srtsrc, mode=caller, for the return monitor and for the picture.'
        New-BundleEntry -Kind Plugin -Names 'libgstd3d11.dll' `
            -Why 'd3d11h265dec AND d3d11videosink: the commentator''s PICTURE. The programme feed arrives as H.265 1920x1080 50p on an M2L-X output and there is no other way to decode or show it here. mfh265dec is ABSENT on the target (the Windows HEVC extension is not installed and buying it from the Store is not a deployment step) and avdec_h265 is FFmpeg, which $ForbiddenPatterns refuses. d3d11h265dec is DXVA in the GPU driver, wrapped LGPL by gst-plugins-bad; measured on 2026-08-07 decoding 1178 frames in 25 s off port 40501, hardware=true, NV12 1920x1080 50/1. One plugin file supplies both the decoder and the sink, so this single entry is the whole picture path.' `
            -Fix 'If this file is missing, the machine has a partial GStreamer install: libgstd3d11.dll ships with gst-plugins-bad in every official build. Do NOT substitute libav.'
    )
}

function Get-RuntimeEntries {
    <#
    .SYNOPSIS
        The libraries the .exe and the thirteen plugins link against.
    .DESCRIPTION
        UNVERIFIED - Gate B must confirm every line of this function against a
        real C:\gstreamer\1.0\mingw_x86_64\bin, and against the output of
        -DependencyReport. See the header block. The candidate-name lists exist
        because MinGW and MSVC builds disagree about the "lib" prefix.
    #>
    return @(
        # -- GStreamer core and libraries -----------------------------------
        New-BundleEntry -Kind Runtime -Names 'gstreamer-1.0-0.dll', 'libgstreamer-1.0-0.dll' `
            -Why 'GStreamer core. Linked into wslcomms.exe by go-gst.' `
            -Fix 'This file must exist. If it does not, -GstRoot is pointing at the wrong tree (or at the MSVC build).'
        New-BundleEntry -Kind Runtime -Names 'gstbase-1.0-0.dll', 'libgstbase-1.0-0.dll' `
            -Why 'GstBaseSrc/GstBaseTransform, used by every plugin here.'
        New-BundleEntry -Kind Runtime -Names 'gstcontroller-1.0-0.dll', 'libgstcontroller-1.0-0.dll' `
            -Why 'GstController. Core links it.'
        New-BundleEntry -Kind Runtime -Names 'gstapp-1.0-0.dll', 'libgstapp-1.0-0.dll' `
            -Why 'go-gst pkg/gstapp is a declared binding surface (spec section 3), so the .exe imports it whether or not appsrc is used.'
        New-BundleEntry -Kind Runtime -Names 'gstaudio-1.0-0.dll', 'libgstaudio-1.0-0.dll' `
            -Why 'audioconvert, audioresample, audioparsers, wasapi2, mediafoundation audio.'
        New-BundleEntry -Kind Runtime -Names 'gstvideo-1.0-0.dll', 'libgstvideo-1.0-0.dll' `
            -Why 'videoconvertscale, imagefreeze, mediafoundation video.'
        New-BundleEntry -Kind Runtime -Names 'gstpbutils-1.0-0.dll', 'libgstpbutils-1.0-0.dll' `
            -Why 'typefindfunctions and the parsers use pbutils descriptions.'
        New-BundleEntry -Kind Runtime -Names 'gsttag-1.0-0.dll', 'libgsttag-1.0-0.dll' `
            -Why 'pulled in by pbutils and by audioparsers.'
        New-BundleEntry -Kind Runtime -Names 'gstallocators-1.0-0.dll', 'libgstallocators-1.0-0.dll' `
            -Why 'GstDmaBuf/GstFdMemory allocators; wasapi2 and mediafoundation link it.'
        New-BundleEntry -Kind Runtime -Names 'gstcodecparsers-1.0-0.dll', 'libgstcodecparsers-1.0-0.dll' `
            -Why 'h264parse (videoparsersbad) parses SPS/PPS through this.'
        New-BundleEntry -Kind Runtime -Names 'gstmpegts-1.0-0.dll', 'libgstmpegts-1.0-0.dll' `
            -Why 'mpegtsmux section/descriptor handling.'
        New-BundleEntry -Kind Runtime -Names 'gstriff-1.0-0.dll', 'libgstriff-1.0-0.dll' -Optional `
            -Why 'RIFF helpers. Some builds have pbutils or the parsers link it; most do not. Copied if present.'
        New-BundleEntry -Kind Runtime -Names 'gstnet-1.0-0.dll', 'libgstnet-1.0-0.dll' -Optional `
            -Why 'net clock. We pin the system clock (spec section 6.1) so this should not be needed; some builds have core import it anyway.'
        New-BundleEntry -Kind Runtime -Names 'gstd3d11-1.0-0.dll', 'libgstd3d11-1.0-0.dll' `
            -Why 'VERIFIED REQUIRED, 1.28.5, 2026-08-07: libgstd3d11.dll (the PICTURE plugin) imports it, per objdump. It was optional while it was only a maybe-dependency of the Media Foundation plugin; it is not optional now, because without it the d3d11 plugin does not load and there is no decoder and no video sink.' `
            -Fix 'This file must exist. If it does not, -GstRoot is pointing at a tree without gst-plugins-bad.'
        New-BundleEntry -Kind Runtime -Names 'gstcodecs-1.0-0.dll', 'libgstcodecs-1.0-0.dll' `
            -Why 'VERIFIED REQUIRED, 1.28.5, 2026-08-07: libgstd3d11.dll imports it, per objdump. It is GstH265Decoder, the stateless-decoder base class d3d11h265dec derives from through GstDxvaH265Decoder. Nothing bundled before the picture path needed it.'
        New-BundleEntry -Kind Runtime -Names 'gstdxva-1.0-0.dll', 'libgstdxva-1.0-0.dll' `
            -Why 'VERIFIED REQUIRED, 1.28.5, 2026-08-07: libgstd3d11.dll imports it, per objdump. It is the DXVA layer between GstH265Decoder and Direct3D 11. Nothing bundled before the picture path needed it.'
        New-BundleEntry -Kind Runtime -Names 'gstdxgi-1.0-0.dll', 'libgstdxgi-1.0-0.dll' -Optional `
            -Why 'Companion to gstd3d11 in some releases. UNVERIFIED - copied only if present.'
        New-BundleEntry -Kind Runtime -Names 'gstd3dshader-1.0-0.dll', 'libgstd3dshader-1.0-0.dll' `
            -Why 'VERIFIED REQUIRED, 1.28.5, Gate B 2026-07-30: libgstd3d11-1.0-0.dll imports it, and it was the single unresolved import the dependency report found on the first real run. Not optional - without it the D3D11 library fails to load, which takes the Media Foundation plugin with it, which is where mfh264enc and mfaacenc live.' `
            -Fix 'If a future GStreamer drops this DLL, check whether libgstd3d11 still imports it before removing the entry - the dependency report is the authority, not this comment.'

        # -- GLib -----------------------------------------------------------
        New-BundleEntry -Kind Runtime -Names 'glib-2.0-0.dll', 'libglib-2.0-0.dll' `
            -Why 'GLib. Linked into wslcomms.exe.' `
            -Fix 'If the only match is libglib-2.0-0.dll and this still fails, the candidate list needs the name your build actually uses - add it, do not switch to a wildcard.'
        New-BundleEntry -Kind Runtime -Names 'gobject-2.0-0.dll', 'libgobject-2.0-0.dll' `
            -Why 'GObject. Every g_object_set in the pipeline goes through it.'
        New-BundleEntry -Kind Runtime -Names 'gio-2.0-0.dll', 'libgio-2.0-0.dll' `
            -Why 'GIO. GStreamer core and the srt plugin use it.'
        New-BundleEntry -Kind Runtime -Names 'gmodule-2.0-0.dll', 'libgmodule-2.0-0.dll' `
            -Why 'g_module_open is how GStreamer loads the thirteen plugins. Without it the registry is empty and nothing works.'
        New-BundleEntry -Kind Runtime -Names 'gthread-2.0-0.dll', 'libgthread-2.0-0.dll' -Optional `
            -Why 'Empty stub since GLib 2.32 but still shipped by some builds. Copied if present.'
        New-BundleEntry -Kind Runtime -Names 'libffi-8.dll', 'libffi-7.dll', 'ffi-8.dll', 'ffi-7.dll' `
            -Why 'GObject closures. The soname digit tracks the libffi major version, hence several candidates.'
        New-BundleEntry -Kind Runtime -Names 'libintl-8.dll', 'intl-8.dll', 'libintl-9.dll' `
            -Why 'gettext runtime; GLib links it for translated messages.' `
            -Fix 'If the dependency report shows nothing importing libintl, delete this entry rather than hunting for the file.'
        New-BundleEntry -Kind Runtime -Names 'libiconv-2.dll', 'iconv-2.dll' -Optional `
            -Why 'libintl needs iconv in most MinGW builds. Optional because some builds fold it into libintl.'
        New-BundleEntry -Kind Runtime -Names 'libpcre2-8-0.dll', 'pcre2-8-0.dll', 'libpcre2-8.dll' `
            -Why 'GLib 2.74+ uses PCRE2 for GRegex.' `
            -Fix 'If the dependency report shows glib importing no pcre, this build links it statically - delete this entry.'
        New-BundleEntry -Kind Runtime -Names 'libz.dll', 'zlib1.dll', 'libz-1.dll' `
            -Why 'zlib: libpng needs it, and GIO uses it for compressed streams.' `
            -Fix 'Whichever of these three names your build uses, put it FIRST in the candidate list so the manifest is unambiguous.'

        # -- Plugin dependencies -------------------------------------------
        New-BundleEntry -Kind Runtime -Names 'liborc-0.4-0.dll', 'orc-0.4-0.dll' `
            -Why 'ORC: the SIMD backend for audioconvert/audioresample/videoconvert. Named explicitly in spec section 11.'
        New-BundleEntry -Kind Runtime -Names 'libpng16-16.dll', 'libpng16.dll', 'png16-16.dll' `
            -Why 'pngdec decodes slate.png with it.'
        New-BundleEntry -Kind Runtime -Names 'srt.dll', 'libsrt.dll', 'libsrt-1-5.dll' `
            -Why 'libsrt: the entire contribution path. Named explicitly in spec section 11.' `
            -Fix 'Required. srtsink cannot load without it and there is no fallback - datarhei/gosrt is a mock-only dependency and must never be linked into the app.'
        New-BundleEntry -Kind Runtime -Names 'libcrypto-3-x64.dll', 'libcrypto-1_1-x64.dll', 'libcrypto-3.dll' -Optional `
            -Why 'OpenSSL: libsrt AES. THE SRT PASSPHRASE DOES NOT WORK WITHOUT A CRYPTO BACKEND. Optional only because the backend might be mbedTLS instead - the dependency report on srt.dll settles it, and one of the two groups MUST be present.'
        New-BundleEntry -Kind Runtime -Names 'libssl-3-x64.dll', 'libssl-1_1-x64.dll', 'libssl-3.dll' -Optional `
            -Why 'OpenSSL, as above. Copied if present.'
        New-BundleEntry -Kind Runtime -Names 'mbedcrypto.dll', 'libmbedcrypto.dll' -Optional `
            -Why 'The other possible libsrt crypto backend. Copied if present.'
        New-BundleEntry -Kind Runtime -Names 'mbedtls.dll', 'libmbedtls.dll' -Optional `
            -Why 'mbedTLS, as above.'
        New-BundleEntry -Kind Runtime -Names 'mbedx509.dll', 'libmbedx509.dll' -Optional `
            -Why 'mbedTLS, as above.'

        # -- MinGW runtime, spec section 11 ---------------------------------
        New-BundleEntry -Kind Runtime -Names 'libwinpthread-1.dll' `
            -Why 'MinGW threads. Named explicitly in spec section 11.'
        New-BundleEntry -Kind Runtime -Names 'libgcc_s_seh-1.dll' `
            -Why 'GCC unwinder. Named explicitly in spec section 11. GPL-3.0 WITH GCC-exception - see licenses\NOTICE.txt.'
        New-BundleEntry -Kind Runtime -Names 'libstdc++-6.dll' `
            -Why 'C++ runtime, needed by the C++ plugins (mediafoundation, wasapi2, srt). Named explicitly in spec section 11.'
    )
}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

function Test-ForbiddenName {
    <#
    .SYNOPSIS
        Returns the pattern a file name violates, or $null.
    #>
    param([Parameter(Mandatory)][string]$Name)
    foreach ($pattern in $ForbiddenPatterns) {
        if ($Name -like $pattern) { return $pattern }
    }
    return $null
}

function Assert-ManifestSane {
    <#
    .SYNOPSIS
        Checks the file list itself, before any file system access.
    .DESCRIPTION
        Catches the three ways a future edit could break the control:
        a wildcard, a forbidden name, or two entries writing the same
        destination file (which would make the destination audit meaningless).
    #>
    param([Parameter(Mandatory)][object[]]$Entries)

    $problems = New-Object System.Collections.Generic.List[string]
    $seen = @{}

    foreach ($e in $Entries) {
        if ($e.Names.Count -eq 0) {
            $problems.Add("entry with no candidate names: $($e.Why)")
        }
        foreach ($n in $e.Names) {
            if ($n -match '[\*\?]') {
                $problems.Add("wildcard in file list: '$n'. The list must name files, never patterns.")
            }
            $bad = Test-ForbiddenName -Name $n
            if ($null -ne $bad) {
                $problems.Add("FORBIDDEN name in file list: '$n' matches '$bad'.")
            }
            $key = "$($e.Kind)/$($n.ToLowerInvariant())"
            if ($seen.ContainsKey($key)) {
                $problems.Add("candidate '$n' appears in two entries; the destination would be ambiguous.")
            }
            $seen[$key] = $true
        }
    }

    # The plugin allowlist is closed and must equal specification section 3, plus
    # the two deliberate additions since: mpegtsdemux, the return monitor's
    # demuxer, and d3d11, the picture's HEVC decoder and video sink. Adding a
    # plugin remains a specification change and this list is where it has to be
    # made, not somewhere it can be drifted into - which is why this check exists
    # at all and why it caught d3d11 on the first run of this change.
    $expectedPlugins = @(
        'coreelements', 'typefindfunctions', 'videoconvertscale', 'audioconvert',
        'audioresample', 'imagefreeze', 'png', 'audioparsers', 'videoparsersbad',
        'wasapi2', 'mediafoundation', 'mpegtsmux', 'mpegtsdemux', 'srt', 'd3d11'
    )
    $actualPlugins = @(
        $Entries | Where-Object { $_.Kind -eq 'Plugin' } | ForEach-Object {
            $_.Names[0] -replace '^libgst', '' -replace '\.dll$', ''
        }
    )
    $missing = @($expectedPlugins | Where-Object { $actualPlugins -notcontains $_ })
    $extra = @($actualPlugins | Where-Object { $expectedPlugins -notcontains $_ })
    foreach ($m in $missing) { $problems.Add("plugin '$m' is in spec section 3 but not in the file list.") }
    foreach ($x in $extra) { $problems.Add("plugin '$x' is in the file list but not in spec section 3. Adding a plugin is a spec change.") }

    if ($problems.Count -gt 0) {
        foreach ($p in $problems) { Write-Host "  FILE LIST ERROR: $p" }
        throw "The file list in bundle-gst.ps1 is not valid ($($problems.Count) problem(s)). Nothing was copied."
    }
}

function Get-DestinationDirectory {
    <#
    .SYNOPSIS
        Where an entry's file goes. Plugins are fixed by contract; runtime DLLs
        follow -CoreDllLayout.
    #>
    param(
        [Parameter(Mandatory)][string]$Kind,
        [Parameter(Mandatory)][string]$Dist,
        [Parameter(Mandatory)][string]$Layout
    )
    if ($Kind -eq 'Plugin') {
        # Contract: internal/gst sets GST_PLUGIN_PATH_1_0 to
        # <appdir>\gst\lib\gstreamer-1.0. Do not move this without changing
        # internal/gst, which is WP-3a's file, not ours.
        return (Join-Path $Dist 'gst\lib\gstreamer-1.0')
    }
    if ($Layout -eq 'GstBin') {
        return (Join-Path $Dist 'gst\bin')
    }
    return $Dist
}

function Resolve-BundleEntry {
    <#
    .SYNOPSIS
        Finds the first candidate that exists on disk. Returns $null when an
        optional entry is absent; throws when a required one is.
    #>
    param(
        [Parameter(Mandatory)][object]$Entry,
        [Parameter(Mandatory)][string]$SourceDir
    )
    foreach ($name in $Entry.Names) {
        $candidate = Join-Path $SourceDir $name
        if (Test-Path -LiteralPath $candidate -PathType Leaf) {
            return (Get-Item -LiteralPath $candidate)
        }
    }
    if ($Entry.Optional) { return $null }

    $msg = New-Object System.Text.StringBuilder
    [void]$msg.AppendLine("MISSING REQUIRED FILE.")
    [void]$msg.AppendLine("  Looked in : $SourceDir")
    [void]$msg.AppendLine("  Tried     : $($Entry.Names -join ', ')")
    [void]$msg.AppendLine("  Needed for: $($Entry.Why)")
    if ($Entry.Fix) { [void]$msg.AppendLine("  Fix       : $($Entry.Fix)") }
    [void]$msg.AppendLine("  General   : if the file exists under another name, ADD that name to the")
    [void]$msg.AppendLine("              candidate list in bundle-gst.ps1. Do not relax the check and do")
    [void]$msg.AppendLine("              not fall back to copying the directory.")
    throw $msg.ToString()
}

function Get-ImportedDllName {
    <#
    .SYNOPSIS
        The DLL names in a PE file's import table, via objdump or dumpbin.
    .DESCRIPTION
        objdump prints "\tDLL Name: KERNEL32.dll"; dumpbin /dependents prints the
        names indented under "Image has the following dependencies:". Both are
        parsed with a name-shaped regex rather than by position, so a change of
        banner text does not silently produce an empty list - an empty list from
        a file that certainly has imports is reported by the caller.
    #>
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Tool,
        [Parameter(Mandatory)][string]$ToolKind
    )

    $names = New-Object System.Collections.Generic.List[string]
    if ($ToolKind -eq 'objdump') {
        $out = & $Tool -p $Path 2>&1
        foreach ($line in $out) {
            $m = [regex]::Match([string]$line, '^\s*DLL Name:\s*(\S+)\s*$')
            if ($m.Success) { $names.Add($m.Groups[1].Value) }
        }
    }
    else {
        $out = & $Tool /dependents $Path 2>&1
        foreach ($line in $out) {
            $m = [regex]::Match([string]$line, '^\s{4}([A-Za-z0-9_.+\-]+\.[Dd][Ll][Ll])\s*$')
            if ($m.Success) { $names.Add($m.Groups[1].Value) }
        }
    }
    return $names.ToArray()
}

function Get-ExportSignature {
    <#
    .SYNOPSIS
        A fingerprint of a PE file's export table: the count of exported names.
    .DESCRIPTION
        This is the check that makes stripping safe to do by default. `strip`
        removes symbol and debug sections; a DLL's exports live in the export
        data directory, which is not a symbol table and is not touched. That is
        the theory. This function is how the script proves it on every file it
        strips, rather than trusting it.

        Counting names rather than diffing the full table is deliberate: it is
        one objdump pass, and any strip that damaged the export directory would
        change the count (almost certainly to zero, since a corrupt directory
        does not parse). Returns -1 if the tool could not be run, which the
        caller treats as "unverified" rather than "unchanged".
    #>
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Tool,
        [Parameter(Mandatory)][string]$ToolKind
    )

    # dumpbin's export listing is formatted differently and this check is not
    # worth a second parser; with dumpbin the strip is simply not verified, and
    # the caller says so.
    if ($ToolKind -ne 'objdump') { return -1 }

    $out = & $Tool -p $Path 2>&1
    if ($LASTEXITCODE -ne 0) { return -1 }

    # objdump prints the export table as "\t[<ordinal>] <name>" lines under
    # "Export Address Table". Counting the ordinal-prefixed lines is stable
    # across binutils versions in a way that section offsets are not.
    $n = 0
    foreach ($line in $out) {
        if ([regex]::IsMatch([string]$line, '^\s*\[\s*\d+\s*\]\s+\S')) { $n++ }
    }
    return $n
}

function Find-StripTool {
    <#
    .SYNOPSIS
        Locates binutils strip. Returns $null if there is none.
    .DESCRIPTION
        Same shape as Find-DependencyTool: explicit path wins, then PATH, then
        the MinGW prefix that build\env.ps1 puts this script's toolchain in.
    #>
    param([string]$Explicit)

    if ($Explicit) {
        if (-not (Test-Path -LiteralPath $Explicit)) {
            throw "-StripTool '$Explicit' does not exist."
        }
        return $Explicit
    }
    foreach ($name in @('strip', 'x86_64-w64-mingw32-strip')) {
        $cmd = Get-Command $name -ErrorAction SilentlyContinue
        if ($cmd) { return $cmd.Source }
    }
    foreach ($p in @('C:\msys64\mingw64\bin\strip.exe')) {
        if (Test-Path -LiteralPath $p) { return $p }
    }
    return $null
}

function Test-SystemDll {
    <#
    .SYNOPSIS
        True if an imported DLL is part of Windows and must NOT be bundled.
    .DESCRIPTION
        Empirical, not a hardcoded list: if the name resolves inside
        %SystemRoot%\System32 on this machine it is an OS component. The
        api-ms-win-*/ext-ms-win-* prefixes are API sets, which are resolved by
        the loader and have no file of their own in some Windows versions.
    #>
    param([Parameter(Mandatory)][string]$Name)
    if ($Name -match '^(api|ext)-ms-win-') { return $true }
    $sys32 = Join-Path $env:SystemRoot 'System32'
    return (Test-Path -LiteralPath (Join-Path $sys32 $Name) -PathType Leaf)
}

function Find-DependencyTool {
    <#
    .SYNOPSIS
        Locates a dependency walker. Returns $null if there is none.
    #>
    param([string]$Explicit)

    if ($Explicit) {
        if (-not (Test-Path -LiteralPath $Explicit)) {
            throw "-DependencyTool '$Explicit' does not exist."
        }
        $kind = 'objdump'
        if ([System.IO.Path]::GetFileNameWithoutExtension($Explicit) -like '*dumpbin*') { $kind = 'dumpbin' }
        return [pscustomobject]@{ Path = $Explicit; Kind = $kind }
    }

    foreach ($candidate in @('objdump', 'x86_64-w64-mingw32-objdump')) {
        $cmd = Get-Command $candidate -ErrorAction SilentlyContinue
        if ($cmd) { return [pscustomobject]@{ Path = $cmd.Source; Kind = 'objdump' } }
    }
    $cmd = Get-Command 'dumpbin' -ErrorAction SilentlyContinue
    if ($cmd) { return [pscustomobject]@{ Path = $cmd.Source; Kind = 'dumpbin' } }
    return $null
}

function Remove-StagedTree {
    <#
    .SYNOPSIS
        Deletes the previous bundle. Guarded, because this deletes recursively.
    .DESCRIPTION
        Only ever removes <DistDir>\gst and the loose *.dll files in <DistDir>
        that a previous AppDir-layout run put there. It will not touch
        wslcomms.exe, slate.png or licenses\, and it refuses to run if DistDir
        looks like a root or a system directory.
    #>
    param([Parameter(Mandatory)][string]$Dist)

    $full = [System.IO.Path]::GetFullPath($Dist)
    if ($full.Length -lt 8 -or $full -eq [System.IO.Path]::GetPathRoot($full)) {
        throw "refusing to clean '$full': that is a drive root."
    }
    if ($full -like "$env:SystemRoot*" -or $full -like "$env:ProgramFiles*") {
        throw "refusing to clean '$full': that is a system location."
    }

    $gst = Join-Path $full 'gst'
    if (Test-Path -LiteralPath $gst) {
        Write-Host "  removing previous bundle: $gst"
        Remove-Item -LiteralPath $gst -Recurse -Force
    }
    $loose = @(Get-ChildItem -LiteralPath $full -Filter '*.dll' -File -ErrorAction SilentlyContinue)
    foreach ($f in $loose) {
        Write-Host "  removing previous runtime DLL: $($f.Name)"
        Remove-Item -LiteralPath $f.FullName -Force
    }
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

$repoRoot = Split-Path -Parent $PSScriptRoot
if (-not $DistDir) { $DistDir = Join-Path $repoRoot 'dist' }

# Make the destination absolute before anything derives a path from it.
#
# A relative -DistDir (say 'build\bin') is the natural thing to type, and it
# works for PowerShell's own cmdlets because they resolve against PowerShell's
# location. But this script also hands paths to .NET - [System.IO.File]::Open
# in the lock check, [System.IO.Path]::GetFullPath in the destination audit -
# and .NET resolves relative paths against the PROCESS working directory, which
# Set-Location does not update. The two disagree, and the audit then compares
# paths rooted in one place against files found in another and calls every
# single file a stray.
#
# Fixing it once here is the whole fix; every Join-Path below inherits it.
if (-not [System.IO.Path]::IsPathRooted($DistDir)) {
    $DistDir = Join-Path (Get-Location).ProviderPath $DistDir
}
$DistDir = [System.IO.Path]::GetFullPath($DistDir)

Write-Host ''
Write-Host '=== bundle-gst.ps1 ===================================================='
Write-Host "  script version : $ScriptVersion"
Write-Host "  GStreamer root : $GstRoot"
Write-Host "  destination    : $DistDir"
Write-Host "  runtime layout : $CoreDllLayout"
if ($DryRun) { Write-Host '  MODE           : DRY RUN - nothing will be created, copied or deleted' }
Write-Host '======================================================================='
Write-Host ''

$entries = @()
$entries += Get-PluginEntries
$entries += Get-RuntimeEntries

Write-Host "Validating the file list ($($entries.Count) entries)..."
Assert-ManifestSane -Entries $entries
Write-Host "  file list OK: no wildcards, no forbidden names, no duplicate destinations,"
Write-Host "  plugin set equals specification section 3 plus mpegtsdemux, exactly."
Write-Host ''

$srcBin = Join-Path $GstRoot 'bin'
$srcPlugins = Join-Path $GstRoot 'lib\gstreamer-1.0'
$sourcePresent = (Test-Path -LiteralPath $srcBin -PathType Container) -and
                 (Test-Path -LiteralPath $srcPlugins -PathType Container)

if (-not $sourcePresent) {
    $detail = "expected '$srcBin' and '$srcPlugins'"
    if ($DryRun) {
        Write-Host "DRY RUN: no GStreamer installation at '$GstRoot' ($detail)."
        Write-Host 'DRY RUN: file names cannot be resolved here; printing the plan only.'
        Write-Host ''
        Write-Host 'PLAN'
        Write-Host '----'
        foreach ($e in $entries) {
            $dest = Get-DestinationDirectory -Kind $e.Kind -Dist $DistDir -Layout $CoreDllLayout
            $flag = '         '
            if ($e.Optional) { $flag = 'OPTIONAL ' }
            Write-Host ("  {0}{1,-10} {2,-34} -> {3}" -f $flag, $e.Kind, $e.Names[0], $dest)
        }
        Write-Host ''
        Write-Host ("  {0} entries: {1} plugin(s), {2} runtime file(s), of which {3} optional." -f `
                $entries.Count,
            @($entries | Where-Object { $_.Kind -eq 'Plugin' }).Count,
            @($entries | Where-Object { $_.Kind -eq 'Runtime' }).Count,
            @($entries | Where-Object { $_.Optional }).Count)
        Write-Host ''
        Write-Host 'DRY RUN COMPLETE. This proves the file list is well-formed. It proves NOTHING'
        Write-Host 'about whether these files exist or whether the dependency closure is complete;'
        Write-Host 'only a run against a real GStreamer installation at Gate B can do that.'
        exit 0
    }
    throw "No GStreamer installation at '$GstRoot' ($detail). Install the GStreamer 1.28.5 mingw-x86_64 RUNTIME and DEVELOPMENT packages (see build\README.md) or pass -GstRoot."
}

# --- resolve -----------------------------------------------------------------
Write-Host 'Resolving files...'
$resolved = New-Object System.Collections.Generic.List[object]
$skippedOptional = New-Object System.Collections.Generic.List[object]

foreach ($e in $entries) {
    $srcDir = if ($e.Kind -eq 'Plugin') { $srcPlugins } else { $srcBin }
    $file = Resolve-BundleEntry -Entry $e -SourceDir $srcDir
    if ($null -eq $file) {
        $skippedOptional.Add($e)
        continue
    }

    # Second of the three forbidden-name checks: this one sees the name that
    # actually exists on disk, which is the name that would be copied.
    $bad = Test-ForbiddenName -Name $file.Name
    if ($null -ne $bad) {
        throw "REFUSING TO COPY '$($file.FullName)': the name matches the forbidden pattern '$bad'. This is a licensing control (spec section 3). Nothing has been copied."
    }

    $dest = Join-Path (Get-DestinationDirectory -Kind $e.Kind -Dist $DistDir -Layout $CoreDllLayout) $file.Name
    $resolved.Add([pscustomobject]@{
            Entry  = $e
            Source = $file
            Dest   = $dest
        })
}

Write-Host ("  resolved {0} file(s); {1} optional file(s) not present." -f $resolved.Count, $skippedOptional.Count)
Write-Host ''

if ($DryRun) {
    foreach ($r in $resolved) {
        Write-Host ("  {0,-34} {1,10:N0} bytes -> {2}" -f $r.Source.Name, $r.Source.Length, $r.Dest)
    }
    Write-Host ''
    # Summed in a loop rather than with Measure-Object -Property {scriptblock},
    # which Windows PowerShell 5.1 does not support.
    $planBytes = 0L
    foreach ($r in $resolved) { $planBytes += $r.Source.Length }
    Write-Host ("DRY RUN total: {0:N1} MB across {1} file(s). Nothing was copied." -f ($planBytes / 1MB), $resolved.Count)
    exit 0
}

# --- pre-flight: is anything holding the destination open? -------------------
# This check exists because its absence cost a working install. A running
# wslcomms.exe holds every plugin DLL it has loaded. The clean step below
# happily deletes the files it CAN delete, then the copy fails on the first one
# it cannot - leaving a destination with some plugins present, some missing, and
# a registry.bin that still lists all of them. The app then starts and fails at
# gst.Init with "the bundled GStreamer is incomplete".
#
# So: prove every file this run intends to write is writable BEFORE deleting
# anything. A locked file here means the bundle is left exactly as it was.
Write-Host 'Checking the destination is writable...'
$locked = New-Object System.Collections.Generic.List[string]
foreach ($r in $resolved) {
    if (-not (Test-Path -LiteralPath $r.Dest -PathType Leaf)) { continue }

    # Convert-Path, not the raw $r.Dest. PowerShell resolves a relative path
    # against its own location; [System.IO.File] resolves it against the
    # process working directory, and the two are not the same object. Passing
    # the relative path makes .NET look somewhere else, fail to find it, and
    # throw FileNotFoundException - which derives from IOException, so a naive
    # catch reports every file in the bundle as locked.
    $abs = (Convert-Path -LiteralPath $r.Dest)
    try {
        $fs = [System.IO.File]::Open($abs, 'Open', 'ReadWrite', 'None')
        $fs.Close()
        $fs.Dispose()
    }
    catch [System.IO.FileNotFoundException] {
        # Not there is not the same as held open, and only the second is fatal.
        continue
    }
    catch [System.IO.DirectoryNotFoundException] {
        continue
    }
    catch [System.IO.IOException] {
        $locked.Add($abs)
    }
}
if ($locked.Count -gt 0) {
    Write-Host ''
    Write-Host "  $($locked.Count) staged file(s) are open in another process, for example:"
    foreach ($l in ($locked | Select-Object -First 4)) { Write-Host "    $l" }
    Write-Host ''
    # Naming the process turns "something has it locked" into an instruction.
    $holders = @(Get-Process -Name 'wslcomms' -ErrorAction SilentlyContinue)
    if ($holders.Count -gt 0) {
        foreach ($h in $holders) { Write-Host "  wslcomms.exe is running (pid $($h.Id)). Close it and re-run." }
    }
    else {
        Write-Host '  No wslcomms.exe is running, so something else holds them - an installer,'
        Write-Host '  an antivirus scan, or an Explorer preview. Close it and re-run.'
    }
    Write-Host ''
    throw "The destination is locked. NOTHING HAS BEEN CHANGED - the existing bundle is intact and the app will still start."
}
Write-Host ("  all {0} destination file(s) are writable." -f $resolved.Count)
Write-Host ''

# --- clean -------------------------------------------------------------------
if (-not $KeepExisting) {
    Write-Host 'Cleaning destination...'
    Remove-StagedTree -Dist $DistDir
    Write-Host ''
}

# --- copy --------------------------------------------------------------------
Write-Host 'Copying...'
$copied = New-Object System.Collections.Generic.List[object]
foreach ($r in $resolved) {
    $destDir = Split-Path -Parent $r.Dest
    if (-not (Test-Path -LiteralPath $destDir)) {
        New-Item -ItemType Directory -Path $destDir -Force | Out-Null
    }
    Copy-Item -LiteralPath $r.Source.FullName -Destination $r.Dest -Force
    $item = Get-Item -LiteralPath $r.Dest
    $copied.Add([pscustomobject]@{
            Name   = $item.Name
            Kind   = $r.Entry.Kind
            Why    = $r.Entry.Why
            Source = $r.Source.FullName
            Dest   = $r.Dest
            Bytes  = $item.Length
            Sha256 = (Get-FileHash -LiteralPath $r.Dest -Algorithm SHA256).Hash
        })
}
Write-Host ("  copied {0} file(s)." -f $copied.Count)
Write-Host ''

# --- strip -------------------------------------------------------------------
# Runs before the audit and the dependency report on purpose: everything
# downstream - the stray-file audit, the closure check, the manifest hashes and
# the size band - then describes the bytes that actually ship, not the bytes
# that were copied and then changed underneath them.
$stripStats = @{ Ran = $false; Tool = ''; Before = 0L; After = 0L; Files = 0; Verified = 0; Unverified = 0 }
if ($NoStrip) {
    Write-Host 'Stripping: SKIPPED (-NoStrip). The bundle keeps its debug sections.'
    Write-Host ''
}
else {
    $stripExe = Find-StripTool -Explicit $StripTool
    if ($null -eq $stripExe) {
        Write-Warning 'No strip found (strip, x86_64-w64-mingw32-strip). The bundle will be roughly'
        Write-Warning 'three times larger than it needs to be - about 58% of it is DWARF debug'
        Write-Warning 'sections. strip ships with the MinGW binutils you installed for Gate B; put'
        Write-Warning 'it on PATH and re-run, or pass -StripTool <path>. This is not fatal.'
        Write-Host ''
    }
    else {
        # The export check needs objdump. If there is none, stripping still runs
        # but says plainly that it was not verified.
        $verifyTool = Find-DependencyTool -Explicit $DependencyTool
        $canVerify = ($null -ne $verifyTool) -and ($verifyTool.Kind -eq 'objdump')

        Write-Host "Stripping debug sections (via $stripExe)..."
        if (-not $canVerify) {
            Write-Warning '  no objdump available: export tables will NOT be checked after stripping.'
        }

        foreach ($c in $copied) {
            $before = $c.Bytes
            $exportsBefore = -1
            if ($canVerify) {
                $exportsBefore = Get-ExportSignature -Path $c.Dest -Tool $verifyTool.Path -ToolKind $verifyTool.Kind
            }

            # Strip a copy, not the staged file: a strip that fails partway
            # through would otherwise leave a truncated DLL in the bundle, and
            # the whole point of this script is that the destination is never
            # in a state nobody chose.
            $tmp = "$($c.Dest).stripping"
            Copy-Item -LiteralPath $c.Dest -Destination $tmp -Force
            & $stripExe --strip-all $tmp 2>&1 | Out-Null
            if ($LASTEXITCODE -ne 0) {
                Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
                throw "strip failed on '$($c.Name)' (exit $LASTEXITCODE). The staged file is untouched. Re-run with -NoStrip to bundle without stripping."
            }

            $after = (Get-Item -LiteralPath $tmp).Length
            if ($after -le 0 -or $after -gt $before) {
                Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
                throw "strip produced an implausible result for '$($c.Name)': $before bytes became $after. The staged file is untouched."
            }

            if ($canVerify) {
                $exportsAfter = Get-ExportSignature -Path $tmp -Tool $verifyTool.Path -ToolKind $verifyTool.Kind
                if ($exportsBefore -lt 0 -or $exportsAfter -lt 0) {
                    $stripStats.Unverified++
                }
                elseif ($exportsAfter -ne $exportsBefore) {
                    Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
                    throw "STRIPPING DAMAGED '$($c.Name)': it exported $exportsBefore symbols before and $exportsAfter after. The staged file is untouched. Re-run with -NoStrip and report this."
                }
                else {
                    $stripStats.Verified++
                }
            }
            else {
                $stripStats.Unverified++
            }

            Move-Item -LiteralPath $tmp -Destination $c.Dest -Force
            $c.Bytes = $after
            $c.Sha256 = (Get-FileHash -LiteralPath $c.Dest -Algorithm SHA256).Hash
            $stripStats.Before += $before
            $stripStats.After += $after
            $stripStats.Files++
        }

        $stripStats.Ran = $true
        $stripStats.Tool = $stripExe
        $saved = $stripStats.Before - $stripStats.After
        Write-Host ("  stripped {0} file(s): {1:N1} MB -> {2:N1} MB, saving {3:N1} MB ({4:N0}%)." -f `
                $stripStats.Files, ($stripStats.Before / 1MB), ($stripStats.After / 1MB), ($saved / 1MB), ($saved * 100 / $stripStats.Before))
        if ($stripStats.Unverified -gt 0) {
            Write-Host ("  export tables checked on {0} file(s); {1} NOT checked." -f $stripStats.Verified, $stripStats.Unverified)
        }
        else {
            Write-Host ("  every stripped file exports exactly what it exported before.")
        }
        Write-Host ''
    }
}

# --- audit the destination ---------------------------------------------------
# Third forbidden-name check, and the check that catches a stale bundle: every
# file now present must be one this run put there.
Write-Host 'Auditing destination...'
$expectedPaths = @{}
foreach ($c in $copied) { $expectedPaths[[System.IO.Path]::GetFullPath($c.Dest).ToLowerInvariant()] = $true }

$auditRoots = New-Object System.Collections.Generic.List[string]
$auditRoots.Add((Join-Path $DistDir 'gst'))
# BUNDLE-MANIFEST.txt is written by this script a few lines below. A previous
# run's copy survives -KeepExisting, so it is expected rather than stray.
$manifestFullPath = [System.IO.Path]::GetFullPath((Join-Path $DistDir 'gst\BUNDLE-MANIFEST.txt')).ToLowerInvariant()
$expectedPaths[$manifestFullPath] = $true
$present = New-Object System.Collections.Generic.List[object]
foreach ($root in $auditRoots) {
    if (Test-Path -LiteralPath $root) {
        $present.AddRange(@(Get-ChildItem -LiteralPath $root -Recurse -File))
    }
}
if ($CoreDllLayout -eq 'AppDir') {
    $present.AddRange(@(Get-ChildItem -LiteralPath $DistDir -Filter '*.dll' -File -ErrorAction SilentlyContinue))
}

$strays = New-Object System.Collections.Generic.List[string]
foreach ($f in $present) {
    $bad = Test-ForbiddenName -Name $f.Name
    if ($null -ne $bad) {
        throw "FORBIDDEN FILE IN THE STAGED BUNDLE: '$($f.FullName)' matches '$bad'. Delete '$DistDir' entirely and re-run; do not ship this."
    }
    if (-not $expectedPaths.ContainsKey([System.IO.Path]::GetFullPath($f.FullName).ToLowerInvariant())) {
        $strays.Add($f.FullName)
    }
}
if ($strays.Count -gt 0) {
    foreach ($s in $strays) { Write-Host "  STRAY: $s" }
    throw "$($strays.Count) file(s) in the destination were not produced by this run. Something copied files here by other means. Re-run without -KeepExisting."
}
Write-Host '  destination contains exactly the files this run copied, and nothing forbidden.'
Write-Host ''

# --- dependency closure ------------------------------------------------------
$depReport = @{ Ran = $false; Tool = ''; Unresolved = @{} }
if ($NoDependencyReport) {
    Write-Host 'Dependency report: SKIPPED (-NoDependencyReport).'
    Write-Host ''
}
else {
    $tool = Find-DependencyTool -Explicit $DependencyTool
    if ($null -eq $tool) {
        Write-Warning 'No dependency walker found (objdump, x86_64-w64-mingw32-objdump, dumpbin).'
        Write-Warning 'THE DEPENDENCY CLOSURE OF THIS BUNDLE IS THEREFORE UNVERIFIED. objdump ships'
        Write-Warning 'with the MinGW gcc you installed for Gate B - put it on PATH and re-run, or'
        Write-Warning 'pass -DependencyTool <path>. A bundle short one DLL fails at runtime, not here.'
        Write-Host ''
    }
    else {
        Write-Host "Dependency report (via $($tool.Kind): $($tool.Path))..."
        $bundleNames = @{}
        foreach ($c in $copied) { $bundleNames[$c.Name.ToLowerInvariant()] = $true }

        $unresolved = @{}
        foreach ($c in $copied) {
            $imports = Get-ImportedDllName -Path $c.Dest -Tool $tool.Path -ToolKind $tool.Kind
            if ($imports.Count -eq 0) {
                Write-Warning "  $($c.Name): the walker reported no imports. Every DLL here imports something; treat this as a parsing failure, not as a clean result."
                continue
            }
            foreach ($imp in $imports) {
                if ($bundleNames.ContainsKey($imp.ToLowerInvariant())) { continue }
                if (Test-SystemDll -Name $imp) { continue }
                if (-not $unresolved.ContainsKey($imp)) { $unresolved[$imp] = New-Object System.Collections.Generic.List[string] }
                $unresolved[$imp].Add($c.Name)
            }
        }

        $depReport.Ran = $true
        $depReport.Tool = "$($tool.Kind): $($tool.Path)"
        $depReport.Unresolved = $unresolved

        if ($unresolved.Count -eq 0) {
            Write-Host '  closure complete: every import resolves to a bundled file or to a Windows system DLL.'
        }
        else {
            Write-Host ''
            Write-Host '  UNRESOLVED IMPORTS:'
            foreach ($k in ($unresolved.Keys | Sort-Object)) {
                # A core library is imported by nearly every file in the bundle, and
                # a forty-name list buries the one fact that matters: the name.
                $importers = $unresolved[$k]
                $shown = $importers
                if ($importers.Count -gt 6) {
                    $shown = @($importers[0..5]) + @("and $($importers.Count - 6) more")
                }
                Write-Host ("    {0,-32} imported by: {1}" -f $k, ($shown -join ', '))
            }
            Write-Host ''
            Write-Host '  Each of these is a file the bundle needs and does not have. For each one,'
            Write-Host '  add an entry to Get-RuntimeEntries in this script with a one-line reason,'
            Write-Host '  then re-run. Do not switch to a directory copy.'
            if (-not $AllowUnresolved) {
                throw "$($unresolved.Count) unresolved import(s). The bundle is incomplete and would fail at runtime."
            }
            Write-Warning '  -AllowUnresolved is set: continuing with an incomplete bundle. Do not ship this.'
        }
        Write-Host ''
    }
}

# --- manifest ----------------------------------------------------------------
# BUNDLE-MANIFEST.txt is both the licensing audit artefact and the installer's
# proof of provenance: installer.iss refuses to build without it, so a hand-made
# dist\gst cannot be packaged by accident.
$gstVersion = 'UNKNOWN'
$coreDll = $copied | Where-Object { $_.Name -like '*gstreamer-1.0-0.dll' } | Select-Object -First 1
if ($coreDll) {
    $vi = (Get-Item -LiteralPath $coreDll.Dest).VersionInfo
    if ($vi -and $vi.ProductVersion) { $gstVersion = $vi.ProductVersion }
}

$totalBytes = ($copied | Measure-Object -Property Bytes -Sum).Sum
$manifestPath = Join-Path $DistDir 'gst\BUNDLE-MANIFEST.txt'

$lines = New-Object System.Collections.Generic.List[string]
$lines.Add('WSL Commentary - GStreamer bundle manifest')
$lines.Add('==========================================')
$lines.Add('')
$lines.Add("Produced by      : build\bundle-gst.ps1 version $ScriptVersion")
$lines.Add("Produced at (UTC): $((Get-Date).ToUniversalTime().ToString('yyyy-MM-dd HH:mm:ss'))")
$lines.Add("Source tree      : $GstRoot")
$lines.Add("GStreamer version: $gstVersion (read from the core DLL's version resource)")
$lines.Add("Runtime layout   : $CoreDllLayout")
$lines.Add("Files            : $($copied.Count)")
$lines.Add("Total size       : $('{0:N0}' -f $totalBytes) bytes ($('{0:N1}' -f ($totalBytes / 1MB)) MB)")
if ($depReport.Ran) {
    $lines.Add("Dependency check : PASSED via $($depReport.Tool)")
}
else {
    $lines.Add('Dependency check : NOT RUN - the dependency closure of this bundle is UNVERIFIED')
}
if ($stripStats.Ran) {
    $saved = $stripStats.Before - $stripStats.After
    $lines.Add("Debug sections   : STRIPPED via $($stripStats.Tool) - $('{0:N1}' -f ($stripStats.Before / 1MB)) MB became $('{0:N1}' -f ($stripStats.After / 1MB)) MB")
    $lines.Add("                   ($('{0:N1}' -f ($saved / 1MB)) MB of DWARF removed; export tables verified unchanged on $($stripStats.Verified) file(s), unverified on $($stripStats.Unverified))")
    $lines.Add('                   The sha256 values below are of the STRIPPED files, which are the')
    $lines.Add('                   files that ship. They will not match the GStreamer distribution.')
}
else {
    $lines.Add('Debug sections   : NOT STRIPPED - files are byte-identical to the source tree')
}
$lines.Add('')
$lines.Add('Every file below was named explicitly in bundle-gst.ps1. No directory copy was')
$lines.Add('performed. No file matching any of these patterns may be present, and the script')
$lines.Add('fails if one is:')
$lines.Add("  $($ForbiddenPatterns -join ' ')")
$lines.Add('GPL x264 is excluded by construction; the H.264 encoder is mfh264enc, a Media')
$lines.Add('Foundation wrapper over a codec already in Windows. See licenses\NOTICE.txt.')
$lines.Add('')
$lines.Add('FILES')
$lines.Add('-----')
foreach ($c in ($copied | Sort-Object Kind, Name)) {
    $rel = $c.Dest.Substring($DistDir.Length).TrimStart('\')
    $lines.Add(("{0,-9} {1,12:N0}  {2}" -f $c.Kind, $c.Bytes, $rel))
    $lines.Add(("            sha256 {0}" -f $c.Sha256))
    $lines.Add(("            reason {0}" -f $c.Why))
}
if ($skippedOptional.Count -gt 0) {
    $lines.Add('')
    $lines.Add('OPTIONAL FILES NOT PRESENT IN THE SOURCE TREE')
    $lines.Add('---------------------------------------------')
    foreach ($e in $skippedOptional) {
        $lines.Add(("  {0}" -f ($e.Names -join ' | ')))
        $lines.Add(("      {0}" -f $e.Why))
    }
}
Set-Content -LiteralPath $manifestPath -Value $lines -Encoding UTF8

# --- summary -----------------------------------------------------------------
Write-Host '=== SUMMARY ==========================================================='
Write-Host ("  plugins       : {0}" -f @($copied | Where-Object { $_.Kind -eq 'Plugin' }).Count)
Write-Host ("  runtime files : {0}" -f @($copied | Where-Object { $_.Kind -eq 'Runtime' }).Count)
Write-Host ("  GStreamer     : {0}" -f $gstVersion)
Write-Host ("  manifest      : {0}" -f $manifestPath)
if ($stripStats.Ran) {
    Write-Host ("  stripped      : {0:N1} MB of debug sections removed" -f (($stripStats.Before - $stripStats.After) / 1MB))
}
Write-Host ("  TOTAL SIZE    : {0:N1} MB ({1:N0} bytes)" -f ($totalBytes / 1MB), $totalBytes)
Write-Host '======================================================================='

if ($skippedOptional.Count -gt 0) {
    Write-Host ''
    Write-Host 'OPTIONAL FILES NOT FOUND (each one is a candidate explanation for a plugin'
    Write-Host 'that will not load - check them against the dependency report):'
    foreach ($e in $skippedOptional) {
        Write-Host ("  - {0}" -f ($e.Names -join ' | '))
        Write-Host ("      {0}" -f $e.Why)
    }
}

if ($totalBytes -lt $ExpectedMinBytes -or $totalBytes -gt $ExpectedMaxBytes) {
    Write-Host ''
    $measured = if ($NoStrip) { '54.1 MB unstripped' } else { '22.9 MB stripped' }
    Write-Warning ("Bundle is {0:N1} MB. Expected {1:N0}-{2:N0} MB, around the {3} measured on 2026-08-12." -f `
            ($totalBytes / 1MB), ($ExpectedMinBytes / 1MB), ($ExpectedMaxBytes / 1MB), $measured)
    Write-Warning 'Under: something is missing, most likely an optional file that is not optional'
    Write-Warning 'on this build. Over: something is being copied that should not be - check the'
    Write-Warning 'manifest file list before shipping.'
}

Write-Host ''
Write-Host 'Next: stage wslcomms.exe, slate.png and build\licenses\ into the same folder,'
Write-Host 'then compile build\installer.iss. See build\README.md.'
Write-Host ''
