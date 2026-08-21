# Re-downloads the latest tesla.db snapshot from teslalog's portal into
# .\data\tesla.db, where docker-compose.yml's Grafana container (and
# its already-provisioned SQLite datasource) reads it from. Grafana
# picks up the new file on its next query - no restart needed.
#
# Usage: .\refresh-data.ps1 -PortalUrl http://10.0.0.236:8083
#   That's an example, not a default that works for everyone - it's
#   YOUR teslalog Pi's own address (config.toml's [portal].addr, as
#   seen from whatever device is on the same network as it). Or set
#   $env:TESLALOG_PORTAL_URL once instead of passing it every time.
param(
    [string]$PortalUrl = $env:TESLALOG_PORTAL_URL
)

if (-not $PortalUrl) {
    Write-Host "Usage: .\refresh-data.ps1 -PortalUrl http://<your-pi-ip>:8083" -ForegroundColor Yellow
    Write-Host "(or: `$env:TESLALOG_PORTAL_URL = 'http://<your-pi-ip>:8083'`)" -ForegroundColor Yellow
    Write-Host "This is YOUR teslalog portal's address, not a default that works for everyone." -ForegroundColor Yellow
    exit 1
}

Set-Location -Path $PSScriptRoot
New-Item -ItemType Directory -Force -Path "data" | Out-Null

$tmp = "data\tesla.db.new"
Invoke-WebRequest -Uri "$($PortalUrl.TrimEnd('/'))/download" -OutFile $tmp
Move-Item -Force -Path $tmp -Destination "data\tesla.db"

Write-Host "Refreshed data\tesla.db from $PortalUrl ($(Get-Date))"
