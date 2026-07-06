# Stops the running Auto Image Converter background process.
# The program runs with no console window or tray icon, so this script is the
# convenient way to shut it down.
# Usage: pwsh -File stop.ps1
$ErrorActionPreference = "Stop"

$processName = "auto-image-converter"

$procs = Get-Process -Name $processName -ErrorAction SilentlyContinue
if (-not $procs) {
    Write-Host "Auto Image Converter is not running."
    exit 0
}

foreach ($p in $procs) {
    try {
        # CloseMainWindow() is a no-op for a windowless process, so stop directly.
        Stop-Process -Id $p.Id -Force -ErrorAction Stop
        Write-Host "Stopped Auto Image Converter (PID $($p.Id))."
    }
    catch {
        Write-Warning "Could not stop PID $($p.Id): $($_.Exception.Message)"
        Write-Warning "Try running this script from an elevated (Administrator) PowerShell."
        exit 1
    }
}
