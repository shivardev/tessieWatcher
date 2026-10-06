[CmdletBinding()]
param(
    [ValidateSet('Auto', 'Docker', 'Native')]
    [string]$Target = 'Auto',
    [switch]$SkipPull
)

$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$composeFile = Join-Path $repoRoot 'docker-compose.postgres.yml'

function Invoke-Checked([string]$Name, [scriptblock]$Command) {
    Write-Host $Name
    & $Command
    if ($LASTEXITCODE -ne 0) { throw "$Name failed." }
}

function Test-DockerEngine {
    if (-not (Get-Command docker -ErrorAction SilentlyContinue)) { return $false }
    $previousPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'SilentlyContinue'
        docker info *> $null
        return $LASTEXITCODE -eq 0
    }
    finally {
        $ErrorActionPreference = $previousPreference
    }
}

function Test-DockerServer {
    if (-not (Test-DockerEngine)) { return $false }
    $container = docker compose -f $composeFile ps -q server 2>$null
    return -not [string]::IsNullOrWhiteSpace(($container -join ''))
}

Push-Location $repoRoot
try {
    if (-not $SkipPull) {
        Invoke-Checked 'Pulling the latest teslalog changes...' { git pull --ff-only }
    }

    $selectedTarget = $Target
    if ($selectedTarget -eq 'Auto') {
        $selectedTarget = if (Test-DockerServer) { 'Docker' } else { 'Native' }
        Write-Host "Detected update target: $selectedTarget"
    }

    if ($selectedTarget -eq 'Docker') {
        if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
            throw 'Docker is not installed or is not on PATH.'
        }
        if (-not (Test-DockerEngine)) {
            throw 'Docker Desktop is installed but its engine is not running. Start Docker Desktop, wait until it says Engine running, then run this command again.'
        }

        Invoke-Checked 'Building the Docker data server and embedded frontend...' {
            docker compose -f $composeFile up -d --build server
        }

        Write-Host 'Waiting for the updated server to become healthy...'
        $healthy = $false
        for ($attempt = 1; $attempt -le 30; $attempt++) {
            $status = docker compose -f $composeFile ps --format json server 2>$null | ConvertFrom-Json
            if ($status.Health -eq 'healthy') { $healthy = $true; break }
            Start-Sleep -Seconds 2
        }
        if (-not $healthy) {
            docker compose -f $composeFile ps server
            throw 'The server did not become healthy within 60 seconds. Run: docker compose -f docker-compose.postgres.yml logs server'
        }
        Write-Host 'teslalog is updated and healthy: http://localhost:8085/app/' -ForegroundColor Green
        return
    }

    if (-not (Get-Command npm -ErrorAction SilentlyContinue)) { throw 'Native source builds require npm.' }
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'Native source builds require Go.' }
    Invoke-Checked 'Building the frontend...' { npm --prefix frontend run build }

    $webuiRoot = (Resolve-Path (Join-Path $repoRoot 'internal/webui')).Path
    $embeddedDist = Join-Path $webuiRoot 'dist'
    $builtDist = (Resolve-Path (Join-Path $repoRoot 'frontend/dist')).Path
    if (-not $embeddedDist.StartsWith($webuiRoot, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Refusing to replace an embedded frontend outside internal/webui.'
    }
    if (Test-Path -LiteralPath $embeddedDist) { Remove-Item -LiteralPath $embeddedDist -Recurse -Force }
    Copy-Item -LiteralPath $builtDist -Destination $embeddedDist -Recurse

    $runningOnWindows = [System.Environment]::OSVersion.Platform -eq [System.PlatformID]::Win32NT
    $output = if ($runningOnWindows) { 'teslalog-local.exe' } else { 'teslalog-local' }
    Invoke-Checked 'Building the native Go application...' {
        go build -trimpath -o $output ./cmd/teslalog
    }
    Write-Host "Native build updated: $repoRoot\$output" -ForegroundColor Green
}
finally {
    Pop-Location
}
