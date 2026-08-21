# teslalog Grafana dashboards

Seven ready-made dashboards, built against teslalog's actual SQLite
schema (not TeslaMate's Postgres one — see the main
[README's Data model section](../README.md#data-model--teslamate-parity)
for the field-by-field mapping between the two).

| File | What it shows |
|---|---|
| `teslalog-drives.json` | Every closed drive: locations, distance, duration, battery, speed, elevation |
| `teslalog-charges.json` | Every closed charging session: location, energy added/used, cost, max power |
| `teslalog-drive-stats.json` | Aggregate stats (drive count, total/median distance, max speed) and top 10 destinations |
| `teslalog-mileage.json` | Cumulative odometer over time |
| `teslalog-states.json` | Full state-machine history (a superset of TeslaMate's own — see main README) |
| `teslalog-battery.json` | Battery level/range over time from idle-poll samples — phantom-drain analysis |
| `teslalog-efficiency.json` | Derived per-drive efficiency (battery %/km, rated-range km lost per km) |

## Setup

1. Install the community SQLite datasource plugin on your Grafana instance:
   ```
   grafana-cli plugins install frser-sqlite-datasource
   ```
   then restart Grafana. (Docker: set `GF_INSTALL_PLUGINS=frser-sqlite-datasource`
   and `GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS=frser-sqlite-datasource`
   as environment variables on the Grafana container instead.)
2. Add a data source of that type (**Connections → Add new connection →
   SQLite**), pointing its **Path** at your downloaded `tesla-YYYY-MM-DD.db`
   file (via the [portal](../README.md#portal-optional-web-page--database-download)'s
   download button) — or a path you periodically re-download to, since
   the plugin reads a static file, not a live connection to teslalog.
3. **Dashboards → Import**, upload one of these JSON files, and pick your
   SQLite data source when prompted for the `DS_SQLITE` variable.

## Notes

- Every query is plain SQL against teslalog's real tables — open any
  panel's query editor to see or modify it.
- `teslalog-efficiency.json`'s numbers are genuinely derived at query
  time, not a stored column — teslalog stores the same raw
  battery/range columns TeslaMate's own efficiency panel computes from,
  it just doesn't pre-compute this itself (see the main README).
- These were validated by running every query in this directory against
  both a hand-built schema and the real `internal/storage` schema before
  being committed — but Grafana version differences in panel
  `fieldConfig`/`options` shapes are always possible; if a panel looks
  odd after import, the query itself (visible in the panel editor) is
  the part that's guaranteed correct.
