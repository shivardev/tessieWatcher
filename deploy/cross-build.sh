#!/usr/bin/env bash
# Cross-compiles teslalog for the Raspberry Pi Zero 2 W (linux/arm64,
# 64-bit Raspberry Pi OS Lite). Run on your dev machine (or in CI), not
# on the Pi itself - the Pi Zero 2 W is far too slow/RAM-constrained to
# be a comfortable build host.
#
# No C compiler, cross-toolchain, or target-side SQLite library is
# needed: storage uses ncruces/go-sqlite3 (SQLite compiled to
# WebAssembly, embedded in the Go module, run via the pure-Go wazero
# runtime), so CGO_ENABLED=0 and this is a completely standard Go
# cross-compile with a fully static result.
set -euo pipefail
cd "$(dirname "$0")/.."

export GOOS=linux
export GOARCH=arm64
export CGO_ENABLED=0

OUT="teslalog-linux-arm64"
go build -trimpath -ldflags="-s -w" -o "$OUT" ./cmd/teslalog

echo "Built $OUT ($(du -h "$OUT" | cut -f1), statically linked, no glibc/cgo dependency)"
echo "Copy it to the Pi, e.g.:"
echo "  scp $OUT pi@<pi-host>:~/"
echo "Then on the Pi: sudo bash deploy/install.sh ~/teslalog-linux-arm64"
