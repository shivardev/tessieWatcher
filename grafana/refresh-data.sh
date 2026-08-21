#!/usr/bin/env bash
# Re-downloads the latest tesla.db snapshot from teslalog's portal into
# ./data/tesla.db, where docker-compose.yml's Grafana container (and
# its already-provisioned SQLite datasource) reads it from. Grafana
# picks up the new file on its next query - no restart needed.
#
# Usage: ./refresh-data.sh [portal-url]
#   defaults to http://10.0.0.236:8083 - override by passing your
#   teslalog portal's actual address, or export TESLALOG_PORTAL_URL.
set -euo pipefail
cd "$(dirname "$0")"

PORTAL_URL="${1:-${TESLALOG_PORTAL_URL:-http://10.0.0.236:8083}}"

mkdir -p data
curl -fsSL -o data/tesla.db.new "${PORTAL_URL%/}/download"
mv data/tesla.db.new data/tesla.db
echo "Refreshed data/tesla.db from ${PORTAL_URL} ($(date))"
