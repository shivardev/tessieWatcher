param(
    [string]$Exe = "$PSScriptRoot\..\teslalog-windows-amd64.exe",
    [string]$DataDirectory = "$env:ProgramData\teslalog",
    [string]$ListenAddress = ":8084"
)

$ErrorActionPreference = "Stop"
if ([string]::IsNullOrWhiteSpace($env:TESLALOG_SERVER_TOKEN)) {
    throw "Set TESLALOG_SERVER_TOKEN to the same long random token used by the primary installation."
}

$resolvedData = [System.IO.Path]::GetFullPath($DataDirectory)
New-Item -ItemType Directory -Force -Path $resolvedData | Out-Null
$database = Join-Path $resolvedData "teslalog-replica.db"
$backups = Join-Path $resolvedData "backups"

& $Exe replica `
    -database $database `
    -id teslalog `
    -addr $ListenAddress `
    -backup-dir $backups `
    -backup-interval 24h `
    -backup-retention-days 30
