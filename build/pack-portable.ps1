<#
.SYNOPSIS
    Packs a staged build into wslcomms-portable.exe: one file, no installer.

.DESCRIPTION
    wslcomms.exe cannot travel alone. Its import table names four GStreamer
    DLLs, and Windows resolves those at process creation - before any of our
    code runs - so a copied .exe produces a loader error box rather than a
    program. This script solves that by putting the whole staged tree inside a
    launcher that unpacks it and starts it. See cmd\portable\unpack.go.

    What goes in is an explicit list, not a directory sweep. That is the same
    rule build\bundle-gst.ps1 follows and for the same reason: a sweep ships
    whatever happens to be lying in the staging folder, and this project has
    already found a stale 26 MB zip sitting there. Both scripts apply the same
    licensing control from build\forbidden-names.ps1.

    Run build\bundle-gst.ps1 first, then stage wslcomms.exe, slate.png and
    build\licenses\ into the same folder. This script verifies all of that
    before it packs anything.

.PARAMETER DistDir
    The staged folder to pack. Defaults to build\bin.

.PARAMETER OutFile
    Where to write the portable executable. Defaults to
    build\dist\wslcomms-portable.exe.

.PARAMETER KeepPayload
    Leave cmd\portable\payload.zip in place after building. It is around 20 MB
    and is deleted by default so it cannot go stale or be committed by accident.

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File build\pack-portable.ps1
#>
[CmdletBinding()]
param(
    [string]$DistDir,
    [string]$OutFile,
    [switch]$KeepPayload,
    # Matches installer.iss, which takes its version the same way:
    #   ISCC.exe /DAppVersion=1.0.0 build\installer.iss
    [string]$Version = '0.0.0'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Add-Type -AssemblyName System.IO.Compression | Out-Null
Add-Type -AssemblyName System.IO.Compression.FileSystem | Out-Null

$repoRoot = Split-Path -Parent $PSScriptRoot

# Absolute paths before anything derives from them: PowerShell resolves relative
# paths against its own location and .NET resolves them against the process
# working directory, and the two are not the same. Getting this wrong in
# bundle-gst.ps1 made its audit report every file in the bundle as a stray.
if (-not $DistDir) { $DistDir = Join-Path $repoRoot 'build\bin' }
if (-not [System.IO.Path]::IsPathRooted($DistDir)) {
    $DistDir = Join-Path (Get-Location).ProviderPath $DistDir
}
$DistDir = [System.IO.Path]::GetFullPath($DistDir)

if (-not $OutFile) { $OutFile = Join-Path $repoRoot 'build\dist\wslcomms-portable.exe' }
if (-not [System.IO.Path]::IsPathRooted($OutFile)) {
    $OutFile = Join-Path (Get-Location).ProviderPath $OutFile
}
$OutFile = [System.IO.Path]::GetFullPath($OutFile)

$payloadZip = Join-Path $repoRoot 'cmd\portable\payload.zip'

. (Join-Path $PSScriptRoot 'forbidden-names.ps1')
if (-not $ForbiddenPatterns -or $ForbiddenPatterns.Count -eq 0) {
    throw 'build\forbidden-names.ps1 did not define $ForbiddenPatterns. Refusing to pack with the licensing control disabled.'
}

Write-Host ''
Write-Host '=== pack-portable.ps1 ================================================='
Write-Host "  staged folder : $DistDir"
Write-Host "  output        : $OutFile"
Write-Host '======================================================================='
Write-Host ''

# --- 1. is the staged folder complete and verified? --------------------------
Write-Host 'Checking the staged folder...'

$required = @(
    @{ Path = 'wslcomms.exe'; Why = 'the application itself' }
    @{ Path = 'slate.png'; Why = 'the holding slate the sender falls back to' }
    @{ Path = 'gst\BUNDLE-MANIFEST.txt'; Why = 'proof bundle-gst.ps1 produced this tree and its licensing check passed' }
    @{ Path = 'licenses\NOTICE.txt'; Why = 'the licence notice that must ship with the LGPL binaries' }
)
foreach ($r in $required) {
    $full = Join-Path $DistDir $r.Path
    if (-not (Test-Path -LiteralPath $full -PathType Leaf)) {
        throw "'$($r.Path)' is missing from $DistDir - $($r.Why). Run build\bundle-gst.ps1 and stage the application first; see build\README.md."
    }
}

# The manifest records whether the dependency closure was actually verified. A
# bundle whose closure was never checked is one that fails at runtime on a
# machine that is not this one - which is precisely the machine a portable build
# is for.
$manifest = Get-Content -LiteralPath (Join-Path $DistDir 'gst\BUNDLE-MANIFEST.txt') -Raw
if ($manifest -notmatch 'Dependency check\s*:\s*PASSED') {
    throw "BUNDLE-MANIFEST.txt does not record a passing dependency check. Re-run build\bundle-gst.ps1 with objdump available; do not ship a bundle whose closure is unverified."
}

# The expected plugin count is not duplicated here: BUNDLE-MANIFEST.txt records
# what the bundler staged, and bundle-gst.ps1 itself asserts the set equals its
# closed allowlist. Packing checks the staged tree AGAINST the manifest, so a
# bundler allowlist change (15 became 16 when the level plugin arrived for the
# input meters) does not need a second edit here - a mismatch now means the
# tree was modified after staging, which is the actual failure this check
# exists to catch.
$pluginDir = Join-Path $DistDir 'gst\lib\gstreamer-1.0'
$plugins = @(Get-ChildItem -LiteralPath $pluginDir -Filter '*.dll' -File -ErrorAction SilentlyContinue)
$manifestPlugins = ([regex]::Matches($manifest, '(?m)^Plugin\b')).Count
if ($manifestPlugins -lt 1) {
    throw "BUNDLE-MANIFEST.txt lists no plugins at all; the staged bundle is not one bundle-gst.ps1 produced."
}
if ($plugins.Count -ne $manifestPlugins) {
    throw "The staged tree has $($plugins.Count) GStreamer plugins but BUNDLE-MANIFEST.txt records $manifestPlugins. Something changed the tree after staging - run build\bundle-gst.ps1 again with the application closed."
}
Write-Host ("  staged folder is complete: {0} plugins, closure verified by bundle-gst.ps1." -f $plugins.Count)
Write-Host ''

# --- 2. build the explicit file list -----------------------------------------
# Named sets, not Get-ChildItem over the folder. Anything not listed here does
# not ship, which is what stops staging debris travelling to a customer machine.
Write-Host 'Selecting files...'

$files = New-Object System.Collections.Generic.List[object]
function Add-PayloadFile {
    param([Parameter(Mandatory)][System.IO.FileInfo]$File)
    $rel = $File.FullName.Substring($DistDir.Length).TrimStart('\', '/')
    $script:files.Add([pscustomobject]@{ Full = $File.FullName; Rel = $rel; Bytes = $File.Length })
}

foreach ($name in @('wslcomms.exe', 'slate.png')) {
    Add-PayloadFile -File (Get-Item -LiteralPath (Join-Path $DistDir $name))
}
# Top-level runtime DLLs (the AppDir layout puts them beside the executable).
foreach ($f in (Get-ChildItem -LiteralPath $DistDir -Filter '*.dll' -File | Sort-Object Name)) {
    Add-PayloadFile -File $f
}
# The plugin tree and its manifest.
foreach ($f in (Get-ChildItem -LiteralPath (Join-Path $DistDir 'gst') -Recurse -File | Sort-Object FullName)) {
    Add-PayloadFile -File $f
}
# The licence texts.
foreach ($f in (Get-ChildItem -LiteralPath (Join-Path $DistDir 'licenses') -Recurse -File | Sort-Object FullName)) {
    Add-PayloadFile -File $f
}

# --- 3. licensing control ----------------------------------------------------
foreach ($f in $files) {
    foreach ($pattern in $ForbiddenPatterns) {
        if ($f.Rel -like $pattern) {
            throw "FORBIDDEN FILE: '$($f.Rel)' matches '$pattern'. This must not be redistributed. Delete '$DistDir' entirely and re-run build\bundle-gst.ps1; do not ship this."
        }
    }
}

$totalRaw = 0L
foreach ($f in $files) { $totalRaw += $f.Bytes }
Write-Host ("  {0} file(s), {1:N1} MB uncompressed; none matches the forbidden list." -f $files.Count, ($totalRaw / 1MB))
Write-Host ''

# --- 4. write the payload ----------------------------------------------------
# Entries are created one at a time with explicit forward-slash names rather
# than via ZipFile::CreateFromDirectory, which on .NET Framework writes
# backslash separators. cmd\portable\unpack.go rejects a backslash in an entry
# name - correctly, since it is not a legal zip separator and Windows would
# treat it as a path component - so a CreateFromDirectory archive would be
# refused by the launcher at run time.
Write-Host 'Writing the payload...'
$outDir = Split-Path -Parent $OutFile
if (-not (Test-Path -LiteralPath $outDir)) { New-Item -ItemType Directory -Path $outDir -Force | Out-Null }
if (Test-Path -LiteralPath $payloadZip) { Remove-Item -LiteralPath $payloadZip -Force }

$zip = [System.IO.Compression.ZipFile]::Open($payloadZip, [System.IO.Compression.ZipArchiveMode]::Create)
try {
    foreach ($f in $files) {
        $entryName = $f.Rel -replace '\\', '/'
        $entry = $zip.CreateEntry($entryName, [System.IO.Compression.CompressionLevel]::Optimal)
        $es = $entry.Open()
        try {
            $fs = [System.IO.File]::OpenRead($f.Full)
            try { $fs.CopyTo($es) } finally { $fs.Dispose() }
        }
        finally { $es.Dispose() }
    }
}
finally { $zip.Dispose() }

$payloadBytes = (Get-Item -LiteralPath $payloadZip).Length
Write-Host ("  payload: {0:N1} MB compressed from {1:N1} MB ({2:N0}%)." -f `
    ($payloadBytes / 1MB), ($totalRaw / 1MB), ($payloadBytes * 100 / $totalRaw))
Write-Host ''

# --- 5. icon and version resource --------------------------------------------
# Without this the launcher gets Go's default blank executable icon and no
# version information at all. That matters more here than it would for a file
# behind an installer: this is the file a user is asked to double-click, often
# one they were sent, and an unidentified blank-icon executable is exactly what
# people are trained not to trust - and what heuristic antivirus scores badly.
# The resource gives it the application's own icon and names its publisher.
#
# windres compiles it to a .syso, which the Go linker picks up automatically
# from the package directory. This works with CGO_ENABLED=0: a .syso is linked
# by the Go linker itself, not by gcc.
Write-Host 'Compiling the icon and version resource...'
$iconPath = Join-Path $repoRoot 'build\windows\icon.ico'
$sysoPath = Join-Path $repoRoot 'cmd\portable\rsrc_windows_amd64.syso'
$windres = Get-Command windres -ErrorAction SilentlyContinue

if (-not (Test-Path -LiteralPath $iconPath)) {
    Write-Warning "  $iconPath is missing; the launcher will have no icon."
}
elseif ($null -eq $windres) {
    Write-Warning '  windres is not on PATH; the launcher will have no icon. It ships with the'
    Write-Warning '  MinGW binutils installed for Gate B - run build\env.ps1 first.'
}
else {
    # VERSIONINFO wants a four-part numeric version; the -Version parameter is
    # the three-part form used everywhere else, so pad it.
    $vparts = @($Version -split '\.') + @('0', '0', '0', '0')
    $vcomma = ($vparts[0..3]) -join ','

    $rcPath = Join-Path $env:TEMP ("wslcomms-portable-{0}.rc" -f [System.Guid]::NewGuid().ToString('N'))
    # Forward slashes: windres treats a backslash in a quoted path as an escape.
    $iconForRc = $iconPath -replace '\\', '/'
    $rc = @"
1 ICON "$iconForRc"

1 VERSIONINFO
FILEVERSION $vcomma
PRODUCTVERSION $vcomma
FILEOS 0x40004L
FILETYPE 0x1L
{
  BLOCK "StringFileInfo"
  {
    BLOCK "080904B0"
    {
      VALUE "CompanyName",      "WSL Studios"
      VALUE "FileDescription",  "WSL Commentary (portable)"
      VALUE "FileVersion",      "$Version"
      VALUE "InternalName",     "wslcomms-portable"
      VALUE "OriginalFilename", "wslcomms-portable.exe"
      VALUE "ProductName",      "WSL Commentary"
      VALUE "ProductVersion",   "$Version"
    }
  }
  BLOCK "VarFileInfo"
  {
    VALUE "Translation", 0x0809, 1200
  }
}
"@
    Set-Content -LiteralPath $rcPath -Value $rc -Encoding ASCII
    try {
        & $windres.Source -i $rcPath -O coff -o $sysoPath
        if ($LASTEXITCODE -ne 0) { throw "windres failed (exit $LASTEXITCODE)." }
        Write-Host ("  resource compiled: {0:N0} bytes, version $Version." -f (Get-Item -LiteralPath $sysoPath).Length)
    }
    finally {
        Remove-Item -LiteralPath $rcPath -Force -ErrorAction SilentlyContinue
    }
}
Write-Host ''

# --- 6. build the launcher ---------------------------------------------------
# -tags portable selects cmd\portable\payload_embed.go over the stub.
# -H windowsgui: no console window flashes up when the user double-clicks.
# -s -w: no symbol table or DWARF in the launcher either.
Write-Host 'Building the launcher...'
Push-Location $repoRoot
try {
    $env:CGO_ENABLED = '0'   # the launcher touches no native code, by design
    & go build -tags portable -trimpath -ldflags '-H windowsgui -s -w' -o $OutFile ./cmd/portable
    if ($LASTEXITCODE -ne 0) { throw "go build failed (exit $LASTEXITCODE)." }
}
finally {
    Pop-Location
    if (-not $KeepPayload -and (Test-Path -LiteralPath $payloadZip)) {
        Remove-Item -LiteralPath $payloadZip -Force
    }
}

$outBytes = (Get-Item -LiteralPath $OutFile).Length
Write-Host ''
Write-Host '=== SUMMARY ==========================================================='
Write-Host ("  files packed  : {0}" -f $files.Count)
Write-Host ("  uncompressed  : {0:N1} MB" -f ($totalRaw / 1MB))
Write-Host ("  OUTPUT        : {0}" -f $OutFile)
Write-Host ("  SIZE          : {0:N1} MB ({1:N0} bytes)" -f ($outBytes / 1MB), $outBytes)
Write-Host '======================================================================='
Write-Host ''
Write-Host 'This one file is the whole application. Copy it anywhere and run it; it'
Write-Host 'needs no installer, no admin rights and nothing preinstalled except the'
Write-Host 'WebView2 runtime, which Windows 11 ships with. On first run it unpacks to'
Write-Host '%LOCALAPPDATA%\WSLComms\runtime\<payload digest>\ and starts from there;'
Write-Host 'later runs reuse it. Settings live in %APPDATA%\WSLComms and are kept'
Write-Host 'across upgrades.'
