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
    [string]$DependencyTool
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# ScriptVersion is stamped into BUNDLE-MANIFEST.txt so that an installer built
# six months from now can be traced back to the list that produced it.
$ScriptVersion = '1.0.0'

# Expected bundle size, specification section 3: the GStreamer folder is
# 60-110 MB. Outside that range something is wrong in one direction or the
# other - a missing plugin, or a directory copy that crept back in.
$ExpectedMinBytes = 60MB
$ExpectedMaxBytes = 110MB

# ---------------------------------------------------------------------------
# Forbidden names. The licensing control.
# ---------------------------------------------------------------------------
# Checked three times: against the file list before anything is touched, against
# every resolved source path before it is copied, and against the destination
# tree after the copy. A match is fatal, always, with no override switch -
# there is deliberately no -Force here.
#
# x264 is the one the specification names (GPL-2.0-or-later; mfh264enc replaces
# it). The rest are in the same family: GPL, or patent-encumbered, or both, and
# none of them has any business in this pipeline.
$ForbiddenPatterns = @(
    '*x264*'        # GPL-2.0-or-later. THE reason this script exists.
    '*x265*'        # GPL-2.0-or-later.
    '*libav*'       # gst-libav / FFmpeg: licence depends on how it was built. Not ours to assume.
    '*ffmpeg*'      # as above.
    '*avcodec*'     # FFmpeg component.
    '*avformat*'    # FFmpeg component.
    '*avfilter*'    # FFmpeg component.
    '*postproc*'    # FFmpeg component, GPL.
    '*swscale*'     # FFmpeg component.
    '*swresample*'  # FFmpeg component.
    '*ugly*'        # gst-plugins-ugly: the set exists precisely because of licensing.
    '*faac*'        # patent-encumbered AAC encoder. We use mfaacenc (the OS).
    '*lame*'        # LGPL but patent-encumbered MP3; nothing here needs MP3.
    '*mpeg2enc*'    # GPL.
    '*a52dec*'      # GPL.
    '*dvdread*'     # GPL.
    '*libmad*'      # GPL.
)

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
        The thirteen plugins of specification section 3, and nothing else.
    .DESCRIPTION
        This list is closed. Adding to it is a specification change, not a build
        change: every plugin here was chosen because the pipeline in section 5
        needs it, and every plugin not here was left out because it is not
        needed, is GPL, or drags in a dependency we would then have to ship.

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
        New-BundleEntry -Kind Plugin -Names 'libgstsrt.dll' `
            -Why 'srtsink, mode=caller, auto-reconnect=false. The contribution path.'
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
        New-BundleEntry -Kind Runtime -Names 'gstd3d11-1.0-0.dll', 'libgstd3d11-1.0-0.dll' -Optional `
            -Why 'The Media Foundation plugin uses D3D11 for GPU-backed MFTs in recent releases. UNVERIFIED for 1.28.5 - if the dependency report shows libgstmediafoundation.dll importing it, this stops being optional.'
        New-BundleEntry -Kind Runtime -Names 'gstdxgi-1.0-0.dll', 'libgstdxgi-1.0-0.dll' -Optional `
            -Why 'Companion to gstd3d11 in some releases. UNVERIFIED - copied only if present.'

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

    # The plugin allowlist is closed and must equal specification section 3.
    $expectedPlugins = @(
        'coreelements', 'typefindfunctions', 'videoconvertscale', 'audioconvert',
        'audioresample', 'imagefreeze', 'png', 'audioparsers', 'videoparsersbad',
        'wasapi2', 'mediafoundation', 'mpegtsmux', 'srt'
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
Write-Host "  plugin set equals specification section 3 exactly."
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
    Write-Warning ("Bundle is {0:N1} MB. Specification section 3 expects 60-110 MB." -f ($totalBytes / 1MB))
    Write-Warning 'Under: something is missing, most likely an optional file that is not optional'
    Write-Warning 'on this build. Over: something is being copied that should not be - check the'
    Write-Warning 'manifest file list before shipping.'
}

Write-Host ''
Write-Host 'Next: stage wslcomms.exe, slate.png and build\licenses\ into the same folder,'
Write-Host 'then compile build\installer.iss. See build\README.md.'
Write-Host ''
