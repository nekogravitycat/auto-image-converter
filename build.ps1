# Builds the release executable with the console window hidden.
#
# The GUI uses the tailscale/walk toolkit, which needs Common Controls 6. That is
# provided by the external application manifest (auto-image-converter.exe.manifest)
# shipped next to the executable, so no resource-embedding step is required and
# the build stays pure Go (CGO is not needed).
#
# Usage: pwsh -File build.ps1
$ErrorActionPreference = "Stop"

$env:CGO_ENABLED = "0"
go build -ldflags="-H=windowsgui" -o auto-image-converter.exe .

if (-not (Test-Path "auto-image-converter.exe.manifest")) {
    Write-Warning "auto-image-converter.exe.manifest is missing; native controls will not be themed. Keep it next to the .exe."
}

Write-Host "Built auto-image-converter.exe"
