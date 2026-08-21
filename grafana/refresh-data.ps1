# Re-downloads the latest tesla.db snapshot from teslalog's portal into
# .\data\tesla.db, where docker-compose.yml's Grafana container (and
# its already-provisioned SQLite datasource) reads it from. Grafana
# picks up the new file on its next query - no restart needed.
#
# Usage: .\refresh-data.ps1 [-PortalUrl http://10.0.0.236:8083]
param(
    [string]$PortalUrl = "http://10.0.0.236:8083"
)

Set-Location -Path $PSScriptRoot
New-Item -ItemType Directory -Force -Path "data" | Out-Null

$tmp = "data\tesla.db.new"
Invoke-WebRequest -Uri "$($PortalUrl.TrimEnd('/'))/download" -OutFile $tmp
Move-Item -Force -Path $tmp -Destination "data\tesla.db"

Write-Host "Refreshed data\tesla.db from $PortalUrl ($(Get-Date))"
