<#
  probe-hevc.ps1 - why is WSLComms decoding the picture in SOFTWARE?

  Run in a normal PowerShell window on the affected machine (no admin needed):

      powershell -ExecutionPolicy Bypass -File probe-hevc.ps1

  It (1) reports the GPU and its driver, (2) clears the stale GStreamer registry
  so the app re-scans the hardware on the next launch, and (3) turns on d3d11
  logging so the next run records whether the driver actually exposes an HEVC
  decoder. Close WSLComms before running it, then relaunch and try the picture.
#>

$ErrorActionPreference = 'Continue'
$W = Join-Path $env:LOCALAPPDATA 'WSLComms'

Write-Host ''
Write-Host '======================================================================='
Write-Host ' WSLComms HEVC hardware-decode probe'
Write-Host '======================================================================='

# --- 1. GPU + driver ---------------------------------------------------------
Write-Host ''
Write-Host '--- 1. Graphics adapter(s) + driver -----------------------------------'
Get-CimInstance Win32_VideoController | ForEach-Object {
    $date = ''
    if ($_.DriverDate) {
        try { $date = ([Management.ManagementDateTimeConverter]::ToDateTime($_.DriverDate)).ToString('yyyy-MM-dd') } catch {}
    }
    [pscustomobject]@{
        Name          = $_.Name
        DriverVersion = $_.DriverVersion
        DriverDate    = $date
        VideoProcessor = $_.VideoProcessor
    }
} | Format-List
Write-Host '  A recent DriverDate and a real "Intel(R) ... Graphics" name mean the'
Write-Host '  driver updated. An old date, or "Microsoft Basic Display Adapter", means'
Write-Host '  it did NOT take - install the Intel Graphics driver first, then re-run.'

# --- 2. Clear the stale registry --------------------------------------------
Write-Host ''
Write-Host '--- 2. Clear the stale GStreamer registry (forces a hardware re-probe) -'
$reg = Join-Path $W 'registry.bin'
if (Test-Path -LiteralPath $reg) {
    try {
        Remove-Item -LiteralPath $reg -Force -ErrorAction Stop
        Write-Host "  DELETED $reg"
        Write-Host '  It cached the decoder list from BEFORE the driver update, which is why'
        Write-Host '  the app still reports no hardware HEVC decoder. The app rebuilds it and'
        Write-Host '  re-scans the GPU on the next launch.'
    } catch {
        Write-Host "  COULD NOT delete it - is WSLComms still running? Close it and re-run."
        Write-Host "  ($($_.Exception.Message))"
    }
} else {
    Write-Host "  No registry.bin at $reg (already clear, or the app has not run for this user)."
}

# --- 3. Arm d3d11 logging for the next launch --------------------------------
Write-Host ''
Write-Host '--- 3. Arm GStreamer logging for the next launch ----------------------'
[Environment]::SetEnvironmentVariable('GST_DEBUG', '2,d3d11*:5,srt*:4', 'User')
Write-Host '  GST_DEBUG = 2,d3d11*:5,srt*:4  (User scope; applies to NEW processes)'
Write-Host '  Records the d3d11 device/decoder probe so the log shows whether the driver'
Write-Host '  advertises an HEVC decode profile.'

# --- next --------------------------------------------------------------------
Write-Host ''
Write-Host '--- NEXT --------------------------------------------------------------'
Write-Host '  1. Fully close WSLComms.'
Write-Host '  2. Launch it again and try the picture.'
Write-Host ''
Write-Host '  3a. Picture now CLEAN (no breakup):  hardware HEVC is back - the registry'
Write-Host '      was just stale. Done. Clear the debug var afterwards with:'
Write-Host "        [Environment]::SetEnvironmentVariable('GST_DEBUG',`$null,'User')"
Write-Host ''
Write-Host '  3b. Still corrupted:  send me the newest *-gst.log, and the d3d11 lines:'
Write-Host "        Select-String -Path `"$W\logs\*-gst.log`" -Pattern 'd3d11|h265|decoder|HEVC|CheckVideo|VideoDevice'"
Write-Host ''
