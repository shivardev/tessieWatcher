<#
.SYNOPSIS
  Registers (or removes) a per-user Windows handler for the tesla:// URL
  scheme, pointed at a teslalog binary, so `teslalog auth` can capture
  Tesla's login redirect automatically instead of relying on a browser to
  show it to you (most browsers just hang on "Loading..." with nothing to
  copy - see README's Authentication section for why).

.DESCRIPTION
  Since Tesla's SSO login now redirects to tesla://auth/callback?code=...
  (the same URL the official Tesla mobile app registers itself to open),
  Windows needs SOME registered handler for that scheme or the redirect
  goes nowhere. This script registers teslalog itself as that handler,
  under HKEY_CURRENT_USER (no admin rights needed, and it only affects
  your own Windows user account).

  Registers: tesla:// -> "<TeslalogPath>" auth-callback "%1"

  When your browser hits the tesla:// redirect after login, Windows
  launches that command with the full URL as its one argument;
  `teslalog auth-callback` (see cmd/teslalog/main.go) relays it to
  whichever `teslalog auth` is currently running and waiting for it.

.PARAMETER TeslalogPath
  Path to teslalog-windows-amd64.exe (or whatever you've named it).
  Required unless -Unregister is passed.

.PARAMETER Unregister
  Removes the registration instead of adding it.

.EXAMPLE
  .\deploy\register-tesla-protocol.ps1 -TeslalogPath .\teslalog-windows-amd64.exe

.EXAMPLE
  .\deploy\register-tesla-protocol.ps1 -Unregister
#>
param(
    [string]$TeslalogPath,
    [switch]$Unregister
)

$key = "HKCU:\Software\Classes\tesla"

if ($Unregister) {
    if (Test-Path $key) {
        Remove-Item -Path $key -Recurse -Force
        Write-Host "Removed the tesla:// protocol handler."
    } else {
        Write-Host "No tesla:// protocol handler was registered - nothing to do."
    }
    exit 0
}

if (-not $TeslalogPath) {
    Write-Error "Pass -TeslalogPath <path to teslalog-windows-amd64.exe>, or -Unregister to remove an existing registration."
    exit 1
}
if (-not (Test-Path $TeslalogPath)) {
    Write-Error "No file found at '$TeslalogPath'."
    exit 1
}
$resolvedPath = (Resolve-Path $TeslalogPath).Path

New-Item -Path $key -Force | Out-Null
Set-ItemProperty -Path $key -Name "(default)" -Value "URL:Tesla Auth Callback"
New-ItemProperty -Path $key -Name "URL Protocol" -Value "" -PropertyType String -Force | Out-Null

$cmdKey = "$key\shell\open\command"
New-Item -Path $cmdKey -Force | Out-Null
Set-ItemProperty -Path $cmdKey -Name "(default)" -Value "`"$resolvedPath`" auth-callback `"%1`""

Write-Host "Registered tesla:// to launch: `"$resolvedPath`" auth-callback `"%1`""
Write-Host "Run 'teslalog auth' again (or continue an already-running one) - the login"
Write-Host "redirect should now be captured automatically instead of hanging in the browser."
Write-Host ""
Write-Host "To remove this later: .\deploy\register-tesla-protocol.ps1 -Unregister"
