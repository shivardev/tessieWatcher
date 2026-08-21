#!/usr/bin/env bash
# Builds teslalog for both architectures we currently support:
#   linux/amd64 - a regular PC/server/VM (handy for testing side-by-side
#                 with an existing TeslaMate instance before it ever
#                 touches the Pi)
#   linux/arm64 - the Raspberry Pi Zero 2 W (64-bit Raspberry Pi OS
#                 Lite/Bookworm), the actual deployment target
#
# Run this on your dev machine (or in CI), not on the Pi itself - the
# Pi Zero 2 W is far too slow/RAM-constrained to be a comfortable build
# host.
#
# No C compiler, cross-toolchain, or target-side SQLite library is
# needed for either target: storage uses ncruces/go-sqlite3 (SQLite
# compiled to WebAssembly, embedded in the Go module, run via the
# pure-Go wazero runtime), so CGO_ENABLED=0 and both are completely
# standard Go cross-compiles producing fully static binaries.
set -euo pipefail
cd "$(dirname "$0")/.."

build() {
  local goos="$1" goarch="$2" out="$3"
  echo "Building $out ..."
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags="-s -w" -o "$out" ./cmd/teslalog
  echo "  -> $out ($(du -h "$out" | cut -f1), statically linked, no glibc/cgo dependency)"
}

build linux   amd64 teslalog-linux-amd64
build linux   arm64 teslalog-linux-arm64
# Windows/amd64, for run-teslalog.bat/status-teslalog.bat - side-by-side
# testing directly on a Windows dev machine, no Linux box or Pi required.
build windows amd64 teslalog-windows-amd64.exe

# 32-bit ARM (armv7), for a Pi Zero 2 W running 32-bit Raspberry Pi OS
# instead of the 64-bit image this README otherwise assumes. The chip
# itself (Cortex-A53) is 64-bit capable either way - this is only needed
# if the *OS image* you flashed is 32-bit. Check with `uname -m` on the
# Pi: "aarch64" -> use teslalog-linux-arm64 above, "armv7l" -> use this.
echo "Building teslalog-linux-armv7 ..."
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
  go build -trimpath -ldflags="-s -w" -o teslalog-linux-armv7 ./cmd/teslalog
echo "  -> teslalog-linux-armv7 ($(du -h teslalog-linux-armv7 | cut -f1), statically linked, no glibc/cgo dependency)"

cat <<'EOF'

Done. All four binaries are fully static (no glibc/cgo dependency) and
can just be copied to the target machine and run directly:

  Testing on a PC/server (linux/amd64), side-by-side with TeslaMate:
    ./teslalog-linux-amd64 auth   -config config.toml   # one-time login
    ./teslalog-linux-amd64 run    -config config.toml   # foreground, watch logs
    ./teslalog-linux-amd64 status -config config.toml

  Deploying to the Pi - check `uname -m` on the Pi first: "aarch64" means
  a 64-bit OS (use teslalog-linux-arm64, the usual case for this README);
  "armv7l" means a 32-bit OS (use teslalog-linux-armv7 instead):
    scp teslalog-linux-arm64 pi@<pi-host>:~/
    ssh pi@<pi-host> 'sudo bash deploy/install.sh ~/teslalog-linux-arm64'

  Testing directly on Windows (windows/amd64): copy
  teslalog-windows-amd64.exe next to config.windows-test.toml,
  run-teslalog.bat and status-teslalog.bat (double-click, or from a
  terminal) at the repo root - no Linux box or Pi required.

No Docker, systemd, or install step is required just to try it out -
either Linux binary can be run directly in a terminal against a
config.toml in the current directory. deploy/install.sh (systemd) and
Docker are both optional, for when you're ready to leave it running
unattended.
EOF
