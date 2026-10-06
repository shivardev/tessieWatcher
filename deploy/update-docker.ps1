[CmdletBinding()]
param([switch]$SkipPull)

# Backward-compatible name for anyone who used the first Docker-only helper.
& (Join-Path $PSScriptRoot 'update.ps1') -Target Docker -SkipPull:$SkipPull
exit $LASTEXITCODE
