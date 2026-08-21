#!/usr/bin/env bash
# Re-downloads the latest tesla.db snapshot from teslalog's portal into
# ./data/tesla.db, where docker-compose.yml's Grafana container (and
# its already-provisioned SQLite datasource) reads it from. Grafana
# picks up the new file on its next query - no restart needed.
#
# Usage: ./refresh-data.sh <portal-url>
#   e.g. ./refresh-data.sh http://10.0.0.236:8083 - your teslalog Pi's
#   own address (config.toml's [portal].addr, from whatever device is
#   on the same network as it), NOT a value copied from someone else's
#   setup. Or export TESLALOG_PORTAL_URL once instead of passing it
#   every time.
set -euo pipefail
cd "$(dirname "$0")"

PORTAL_URL="${1:-${TESLALOG_PORTAL_URL:-}}"
if [ -z "$PORTAL_URL" ]; then
  echo "Usage: $0 <portal-url>   e.g. $0 http://10.0.0.236:8083" >&2
  echo "(or: export TESLALOG_PORTAL_URL=http://<your-pi-ip>:8083)" >&2
  echo "This is YOUR teslalog portal's address, not a default that works for everyone." >&2
  exit 1
fi

mkdir -p data
curl -fsSL -o data/tesla.db.new "${PORTAL_URL%/}/download"
mv data/tesla.db.new data/tesla.db
echo "Refreshed data/tesla.db from ${PORTAL_URL} ($(date))"
