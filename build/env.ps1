# Gate B build environment for wslcomms.
#
# Dot-source it, do not run it — it sets variables in your shell:
#
#     . .\build\env.ps1
#
# Verified working on 2026-07-30: `wails build -webview2 embed` produced
# build\bin\wslcomms.exe (19.8 MB) in 40 s, and `go test -race ./...` links.
#
# Every value here is load-bearing. See build\README.md sections 2.3 to 2.6.

$ErrorActionPreference = 'Stop'

# Toolchain. mingw64 is MSVCRT, matching how GStreamer's MinGW build is
# packaged. ucrt64 also works; if you switch, change BOTH this and CGO_LDFLAGS.
$Toolchain = 'C:\msys64\mingw64'
$GStreamer = 'C:\gstreamer\1.0\mingw_x86_64'

foreach ($p in @($Toolchain, $GStreamer)) {
    if (-not (Test-Path $p)) { throw "missing: $p - see build\README.md section 2" }
}

$env:PATH = "$Toolchain\bin;$GStreamer\bin;" + $env:PATH
$env:PKG_CONFIG_PATH = "$GStreamer\lib\pkgconfig"
$env:CGO_ENABLED = '1'

# MANDATORY. GStreamer's lib\ ships its own libmingw32.a, libmingwex.a and
# libgcc.a, built against an older mingw-w64. pkg-config puts that directory on
# the link line ahead of the toolchain's own, so -lmingw32 resolves to the stale
# copy, which lacks __mingw_SEH_error_handler and fails the link of anything
# that imports GStreamer. Putting the real runtime first fixes it.
# Full diagnosis, and the six things this is NOT, in build\README.md 2.5a.
$env:CGO_LDFLAGS = "-L$($Toolchain -replace '\\','/')/x86_64-w64-mingw32/lib -L$($Toolchain -replace '\\','/')/lib"

Write-Host "wslcomms build environment ready" -ForegroundColor Green
Write-Host ("  gcc         " + (Get-Command gcc).Source)
Write-Host ("  pkg-config  " + (& pkg-config --modversion gstreamer-1.0) + " (gstreamer-1.0)")
Write-Host ""
Write-Host "  wails build -webview2 embed              release build"
Write-Host "  go test -race -tags 'dev gststub' ./...  THE test command - see below"
Write-Host ""
Write-Host "The 'gststub' tag is not optional here." -ForegroundColor Yellow
Write-Host "This environment sets CGO_ENABLED=1, which selects internal/gst's REAL"
Write-Host "implementation and excludes the pure-Go stub. But app_test.go and"
Write-Host "internal/gst's own tests are written against that stub - correctly, since"
Write-Host "no unit test should be driving live GStreamer. Without the tag those"
Write-Host "packages do not compile, and the root package's tests are skipped in"
Write-Host "silence. 'dev' alone is a Gate A command; at Gate B you need both."
