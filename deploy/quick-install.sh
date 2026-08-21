#!/usr/bin/env bash
# One-line installer: downloads the right prebuilt teslalog binary for
# this machine's arch (no cross-build, no git clone) plus its config
# template and systemd unit, then runs the same steps deploy/install.sh
# does. Meant to be run directly on the target Linux box (a Raspberry
# Pi, or any systemd machine) as root:
#
#   curl -fsSL https://raw.githubusercontent.com/shivardev/tessieWatcher/master/deploy/quick-install.sh | sudo bash
#
# This is a convenience wrapper, not a different install path - it
# fetches the exact same three things (binary, config.example.toml,
# systemd/teslalog.service) that deploy/cross-build.sh + a manual scp
# would give you, then does exactly what deploy/install.sh does with
# them. If you'd rather see every step before running it, read this
# script (it's short) or just follow the README's Deploying section by
# hand instead - nothing here is hidden.
set -euo pipefail

REPO="shivardev/tessieWatcher"
RAW_BASE="https://raw.githubusercontent.com/${REPO}/master"

if [[ $EUID -ne 0 ]]; then
  echo "Run this as root, e.g.:" >&2
  echo "  curl -fsSL https://raw.githubusercontent.com/${REPO}/master/deploy/quick-install.sh | sudo bash" >&2
  exit 1
fi

# --- pick the right release asset for this machine ---
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64)   asset="teslalog-linux-amd64" ;;
  aarch64|arm64)  asset="teslalog-linux-arm64" ;;
  armv7l)         asset="teslalog-linux-armv7" ;;  # Raspberry Pi Zero 2 W on a 32-bit OS
  *)
    echo "No prebuilt binary for uname -m == '$arch'." >&2
    echo "Build from source instead: see the README's Building section." >&2
    exit 1
    ;;
esac
echo "Detected $arch -> using release asset $asset"

need() { command -v "$1" >/dev/null 2>&1 || { echo "This installer needs '$1', which isn't on PATH." >&2; exit 1; }; }
need curl

# --- resolve the latest release's download URL for that asset ---
api_url="https://api.github.com/repos/${REPO}/releases/latest"
download_url="$(curl -fsSL "$api_url" | grep -o "\"browser_download_url\": *\"[^\"]*${asset}\"" | head -n1 | sed -E 's/.*"(https[^"]+)"/\1/')"
if [[ -z "$download_url" ]]; then
  echo "Couldn't find a '$asset' asset on the latest GitHub release ($api_url)." >&2
  echo "Check https://github.com/${REPO}/releases yourself, or build from source (see README)." >&2
  exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $asset ..."
curl -fsSL -o "$tmp/teslalog" "$download_url"
chmod 0755 "$tmp/teslalog"

echo "Downloading config.example.toml and the systemd unit ..."
curl -fsSL -o "$tmp/config.example.toml" "${RAW_BASE}/config.example.toml"
curl -fsSL -o "$tmp/teslalog.service" "${RAW_BASE}/systemd/teslalog.service"

# --- same steps as deploy/install.sh from here on ---
id -u teslalog &>/dev/null || useradd --system --home-dir /var/lib/teslalog --shell /usr/sbin/nologin teslalog

install -d -o teslalog -g teslalog -m 0750 /var/lib/teslalog
install -d -o teslalog -g teslalog -m 0750 /var/lib/teslalog/backups
install -d -m 0755 /etc/teslalog

install -m 0755 "$tmp/teslalog" /usr/local/bin/teslalog

if [[ ! -f /etc/teslalog/config.toml ]]; then
  install -m 0644 "$tmp/config.example.toml" /etc/teslalog/config.toml
  echo "Wrote default config to /etc/teslalog/config.toml - review it (VIN, intervals) before starting."
fi

install -m 0644 "$tmp/teslalog.service" /etc/systemd/system/teslalog.service
systemctl daemon-reload

echo
echo "Installed $("/usr/local/bin/teslalog" version 2>/dev/null || echo teslalog). Next steps:"
echo "  1. Authenticate (as the teslalog user, so token file perms are right):"
echo "     sudo -u teslalog teslalog auth -config /etc/teslalog/config.toml"
echo "  2. Enable and start the service:"
echo "     sudo systemctl enable --now teslalog"
echo "  3. Watch logs:"
echo "     journalctl -u teslalog -f"
