# Builds the release executable with the console window hidden.
# Usage: pwsh -File build.ps1
$ErrorActionPreference = "Stop"
go build -ldflags="-H=windowsgui" -o auto-image-converter.exe .
Write-Host "Built auto-image-converter.exe"
