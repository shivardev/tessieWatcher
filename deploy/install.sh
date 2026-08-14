#!/usr/bin/env bash
# Installs teslalog on a Raspberry Pi (or any systemd Linux box).
# Run as root on the Pi itself, AFTER copying the cross-compiled
# teslalog-linux-arm64 binary alongside this script (see
# deploy/cross-build.sh, run on your dev machine, not the Pi).
set -euo pipefail

BIN_SRC="${1:-./teslalog-linux-arm64}"

if [[ $EUID -ne 0 ]]; then
  echo "Run this as root (sudo bash deploy/install.sh)" >&2
  exit 1
fi
if [[ ! -f "$BIN_SRC" ]]; then
  echo "Binary not found: $BIN_SRC" >&2
  echo "Build it first with deploy/cross-build.sh on your dev machine, then copy it to the Pi." >&2
  exit 1
fi

id -u teslalog &>/dev/null || useradd --system --home-dir /var/lib/teslalog --shell /usr/sbin/nologin teslalog

install -d -o teslalog -g teslalog -m 0750 /var/lib/teslalog
install -d -o teslalog -g teslalog -m 0750 /var/lib/teslalog/backups
install -d -m 0755 /etc/teslalog

install -m 0755 "$BIN_SRC" /usr/local/bin/teslalog

if [[ ! -f /etc/teslalog/config.toml ]]; then
  install -m 0644 config.example.toml /etc/teslalog/config.toml
  echo "Wrote default config to /etc/teslalog/config.toml - review it (VIN, intervals) before starting."
fi

install -m 0644 systemd/teslalog.service /etc/systemd/system/teslalog.service
systemctl daemon-reload

echo
echo "Installed. Next steps:"
echo "  1. Authenticate (as the teslalog user, so token file perms are right):"
echo "     sudo -u teslalog teslalog auth -config /etc/teslalog/config.toml"
echo "  2. Enable and start the service:"
echo "     sudo systemctl enable --now teslalog"
echo "  3. Watch logs:"
echo "     journalctl -u teslalog -f"
