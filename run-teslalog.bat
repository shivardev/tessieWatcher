@echo off
setlocal
cd /d "%~dp0"

if not exist tokens.json (
  echo ==============================================================
  echo First run: logging in to your Tesla account.
  echo A login URL will appear below - open it in any browser, log in
  echo (2FA/CAPTCHA if Tesla asks), then copy the FULL address-bar URL
  echo from the blank/broken "void/callback" page it lands on and
  echo paste it back into this window.
  echo ==============================================================
  echo.
  teslalog-windows-amd64.exe auth -config config.windows-test.toml
  if errorlevel 1 (
    echo.
    echo Login did not complete - see the error above.
    echo Just double-click this file again to retry.
    pause
    exit /b 1
  )
  echo.
  echo Login succeeded. Starting the logger...
  echo.
)

echo ==============================================================
echo Running. Leave this window open while you drive/charge.
echo Press Ctrl+C to stop.
echo ==============================================================
teslalog-windows-amd64.exe run -config config.windows-test.toml
pause
