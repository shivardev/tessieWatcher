[CmdletBinding()]
param(
    [string]$Pi = 'konda@raspberrypi',
    [switch]$SkipFrontend
)

$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path

function Invoke-Checked([string]$Name, [scriptblock]$Command) {
    Write-Host $Name
    & $Command
    if ($LASTEXITCODE -ne 0) { throw "$Name failed." }
}

foreach ($command in @('go', 'ssh', 'scp')) {
    if (-not (Get-Command $command -ErrorAction SilentlyContinue)) {
        throw "$command is required but was not found on PATH."
    }
}

Push-Location $repoRoot
try {
    $machine = (ssh $Pi 'uname -m').Trim()
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($machine)) {
        throw "Could not detect the architecture of $Pi."
    }

    switch ($machine) {
        'aarch64' { $goArch = 'arm64'; $goArm = $null; $artifact = 'teslalog-linux-arm64' }
        'arm64'   { $goArch = 'arm64'; $goArm = $null; $artifact = 'teslalog-linux-arm64' }
        'armv7l'  { $goArch = 'arm'; $goArm = '7'; $artifact = 'teslalog-linux-armv7' }
        default   { throw "Unsupported Pi architecture: $machine" }
    }
    Write-Host "Detected $Pi as $machine; building $artifact."

    if (-not $SkipFrontend) {
        if (-not (Get-Command npm -ErrorAction SilentlyContinue)) { throw 'npm is required to build the frontend.' }
        Invoke-Checked 'Building the embedded frontend...' { npm --prefix frontend run build }

        $webuiRoot = (Resolve-Path (Join-Path $repoRoot 'internal/webui')).Path
        $embeddedDist = Join-Path $webuiRoot 'dist'
        $builtDist = (Resolve-Path (Join-Path $repoRoot 'frontend/dist')).Path
        if (-not $embeddedDist.StartsWith($webuiRoot, [StringComparison]::OrdinalIgnoreCase)) {
            throw 'Refusing to replace an embedded frontend outside internal/webui.'
        }
        if (Test-Path -LiteralPath $embeddedDist) { Remove-Item -LiteralPath $embeddedDist -Recurse -Force }
        Copy-Item -LiteralPath $builtDist -Destination $embeddedDist -Recurse
    }

    $oldCgo, $oldGoos, $oldGoarch, $oldGoarm = $env:CGO_ENABLED, $env:GOOS, $env:GOARCH, $env:GOARM
    try {
        $env:CGO_ENABLED = '0'
        $env:GOOS = 'linux'
        $env:GOARCH = $goArch
        $env:GOARM = $goArm
        Invoke-Checked "Building $artifact..." { go build -trimpath -ldflags='-s -w' -o $artifact ./cmd/teslalog }
    }
    finally {
        $env:CGO_ENABLED, $env:GOOS, $env:GOARCH, $env:GOARM = $oldCgo, $oldGoos, $oldGoarch, $oldGoarm
    }

    Invoke-Checked "Copying $artifact to $Pi..." { scp $artifact "${Pi}:/tmp/teslalog.new" }
    Write-Host 'Installing and restarting teslalog on the Pi (sudo may ask for your password)...'
    ssh -t $Pi 'sudo systemctl stop teslalog && sudo install -m 0755 /tmp/teslalog.new /usr/local/bin/teslalog && sudo systemctl start teslalog && /usr/local/bin/teslalog version && sudo systemctl --no-pager --lines=5 status teslalog'
    if ($LASTEXITCODE -ne 0) { throw 'Pi installation or service restart failed.' }

    Write-Host 'Pi update completed successfully.' -ForegroundColor Green
}
finally {
    Pop-Location
}
