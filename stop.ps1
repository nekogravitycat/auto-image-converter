# Stops the running Auto Image Converter background process.
# The program runs with no console window or tray icon, so this script is the
# convenient way to shut it down.
#
# It first asks for a graceful shutdown by setting a named event (the same one
# the program waits on), giving in-flight conversions time to finish so no
# ".converting.tmp" files are left behind. Only if the process does not exit
# within the grace period does it fall back to a forced stop.
#
# Usage: pwsh -File stop.ps1
$ErrorActionPreference = "Stop"

$processName = "auto-image-converter"
# Must match control.StopEventName in the Go source.
$eventName = "auto-image-converter-stop"
# A little longer than the program's in-flight drain deadline (30s) so a healthy
# shutdown completes before we consider forcing it.
$graceTimeoutSec = 35

$procs = Get-Process -Name $processName -ErrorAction SilentlyContinue
if (-not $procs) {
    Write-Host "Auto Image Converter is not running."
    exit 0
}

# 1) Ask for a graceful shutdown via the named stop event.
$signaled = $false
try {
    $evt = [System.Threading.EventWaitHandle]::OpenExisting($eventName)
    [void]$evt.Set()
    $evt.Dispose()
    $signaled = $true
    Write-Host "Requested graceful shutdown; waiting up to $graceTimeoutSec s for in-flight conversions..."
}
catch [System.Threading.WaitHandleCannotBeOpenedException] {
    Write-Warning "Graceful stop channel not found (older build or already exiting); forcing stop."
}
catch {
    Write-Warning "Could not signal graceful stop ($($_.Exception.Message)); forcing stop."
}

# 2) If signaled, wait for the process(es) to exit on their own.
if ($signaled) {
    foreach ($p in $procs) {
        try {
            if ($p.WaitForExit($graceTimeoutSec * 1000)) {
                Write-Host "Auto Image Converter (PID $($p.Id)) exited gracefully."
            }
            else {
                Write-Warning "PID $($p.Id) did not exit within $graceTimeoutSec s; will force stop."
            }
        }
        catch {
            Write-Warning "Could not wait on PID $($p.Id): $($_.Exception.Message)"
        }
    }
}

# 3) Force-stop anything still alive.
$remaining = Get-Process -Name $processName -ErrorAction SilentlyContinue
foreach ($p in $remaining) {
    try {
        Stop-Process -Id $p.Id -Force -ErrorAction Stop
        Write-Host "Force-stopped Auto Image Converter (PID $($p.Id))."
    }
    catch {
        Write-Warning "Could not stop PID $($p.Id): $($_.Exception.Message)"
        Write-Warning "Try running this script from an elevated (Administrator) PowerShell."
        exit 1
    }
}
