@echo off
setlocal
cd /d "%~dp0"
.\teslalog-windows-amd64.exe status -config config.windows-test.toml
echo.
echo Exporting drives.csv and charges.csv next to this file...
.\teslalog-windows-amd64.exe export drives  -out drives.csv  -config config.windows-test.toml
.\teslalog-windows-amd64.exe export charges -out charges.csv -config config.windows-test.toml
echo Done.
pause
